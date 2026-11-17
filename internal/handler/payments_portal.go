package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/checkout"
	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/service"
	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/pkg/response"
)

// PortalOrderPayer is the checkout surface the portal's Pay action uses.
// *checkout.Service implements it. It is an interface here so the
// handler tests can answer without a provider.
type PortalOrderPayer interface {
	ReissueAttempt(ctx context.Context, order *model.PaymentOrder) (*model.PaymentAttempt, error)
}

// PortalInvoiceFilter is the slice of the invoice ledger a portal list
// asks for. BuyerEmail is the session email; the invoice service must
// scope its query by it, not filter after the fact.
type PortalInvoiceFilter struct {
	BuyerEmail string
	Limit      int
	Offset     int
}

// PortalInvoiceService is the invoice service surface the portal reads.
// The production adapter in main.go maps module C's service onto it: its
// PDF returns the invoice too, which the portal does not need.
type PortalInvoiceService interface {
	Get(ctx context.Context, id string) (*model.Invoice, error)
	PDF(ctx context.Context, id string) ([]byte, error)
	List(ctx context.Context, f PortalInvoiceFilter) ([]*model.Invoice, int, error)
}

// ErrPortalInvoiceNotFound is what an adapter returns for an unknown
// invoice. sql.ErrNoRows is accepted too, so a thin wrapper need not
// translate.
var ErrPortalInvoiceNotFound = errors.New("portal invoice not found")

// PortalInvoiceFuncs adapts function values to PortalInvoiceService, so
// main.go can wire module C without declaring a named adapter type.
type PortalInvoiceFuncs struct {
	GetFn  func(ctx context.Context, id string) (*model.Invoice, error)
	PDFFn  func(ctx context.Context, id string) ([]byte, error)
	ListFn func(ctx context.Context, f PortalInvoiceFilter) ([]*model.Invoice, int, error)
}

func (f PortalInvoiceFuncs) Get(ctx context.Context, id string) (*model.Invoice, error) {
	if f.GetFn == nil {
		return nil, errPortalInvoicesUnavailable
	}
	return f.GetFn(ctx, id)
}

func (f PortalInvoiceFuncs) PDF(ctx context.Context, id string) ([]byte, error) {
	if f.PDFFn == nil {
		return nil, errPortalInvoicesUnavailable
	}
	return f.PDFFn(ctx, id)
}

func (f PortalInvoiceFuncs) List(ctx context.Context, filter PortalInvoiceFilter) ([]*model.Invoice, int, error) {
	if f.ListFn == nil {
		return nil, 0, errPortalInvoicesUnavailable
	}
	return f.ListFn(ctx, filter)
}

var errPortalInvoicesUnavailable = errors.New("portal: invoice service is not configured")

// PortalPaymentsHandler is the signed-in customer's view of the payment
// ledger: their orders, their invoices and the buyer/tax block saved for
// them. Every query is scoped by the session email; a code or id that
// belongs to another customer is answered exactly like an unknown one.
type PortalPaymentsHandler struct {
	Store    *store.Store
	Payer    PortalOrderPayer
	Invoices PortalInvoiceService
	Events   *service.OrderEventEmitter
}

// PortalPaymentHandler is the contract's spelling of the handler type.
type PortalPaymentHandler = PortalPaymentsHandler

// NewPortalPaymentsHandler builds the handler. Payer and Invoices are
// required by the actions that use them; a nil one answers 503/500
// rather than panicking.
func NewPortalPaymentsHandler(st *store.Store, payer PortalOrderPayer, invoices PortalInvoiceService) *PortalPaymentsHandler {
	return &PortalPaymentsHandler{Store: st, Payer: payer, Invoices: invoices}
}

// RegisterPortalPaymentRoutes mounts the phase-1b portal routes on the
// already-authenticated /api/v1/portal group. The group owns the session
// middleware; this function owns only the paths below it.
func RegisterPortalPaymentRoutes(g *gin.RouterGroup, h *PortalPaymentsHandler) {
	g.GET("/payments/orders", h.ListOrders)
	g.GET("/payments/orders/:code", h.GetOrder)
	g.POST("/payments/orders/:code/pay", h.PayOrder)
	g.POST("/payments/orders/:code/cancel", h.CancelOrder)

	g.GET("/invoices", h.ListInvoices)
	g.GET("/invoices/:id", h.GetInvoice)

	g.GET("/billing-profile", h.GetBillingProfile)
	g.PUT("/billing-profile", h.SaveBillingProfile)
}

// portalSession reads the identity SessionAuth put on the request. A
// user id without an email cannot be scoped to an order, so both are
// required.
func portalSession(c *gin.Context) (userID, email string, ok bool) {
	v, _ := c.Get("user_id")
	userID, _ = v.(string)
	userID = strings.TrimSpace(userID)
	email = strings.ToLower(strings.TrimSpace(emailFromContext(c)))
	if userID == "" || email == "" {
		response.Unauthorized(c, "portal session required")
		return "", "", false
	}
	return userID, email, true
}

// ─── orders ───

func (h *PortalPaymentsHandler) ListOrders(c *gin.Context) {
	_, email, ok := portalSession(c)
	if !ok {
		return
	}
	page := listPage(c)
	rows, total, err := h.Store.ListPortalPaymentOrders(c, email, page)
	if err != nil {
		response.Internal(c, err)
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, order := range rows {
		out = append(out, orderRowView(order))
	}
	listOK(c, "orders", out, total, page)
}

func (h *PortalPaymentsHandler) GetOrder(c *gin.Context) {
	_, email, ok := portalSession(c)
	if !ok {
		return
	}
	order, ok := h.portalOrder(c, email)
	if !ok {
		return
	}
	var attempt *model.PaymentAttempt
	if latest, err := h.Store.GetLatestPaymentAttempt(c, order.ID); err == nil {
		attempt = latest
	} else if !errors.Is(err, sql.ErrNoRows) {
		response.Internal(c, err)
		return
	}
	response.OK(c, orderView(&checkout.OrderResult{Order: order, Attempt: attempt}))
}

// PayOrder re-issues the instruction for a pending order. The order's
// price is already fixed: this never re-prices, never creates a second
// order, and refuses a terminal or expiry-passed order before touching
// the provider.
func (h *PortalPaymentsHandler) PayOrder(c *gin.Context) {
	_, email, ok := portalSession(c)
	if !ok {
		return
	}
	order, ok := h.portalOrder(c, email)
	if !ok {
		return
	}
	if order.Status != domain.OrderPending || !order.ExpiresAt.After(time.Now()) {
		response.Err(c, http.StatusConflict, "PAYMENT_ORDER_NOT_PAYABLE", "order is already paid or closed")
		return
	}
	if h.Payer == nil {
		response.Err(c, http.StatusServiceUnavailable, "PAYMENT_PROVIDER_DISABLED", "no payment provider is configured")
		return
	}
	attempt, err := h.Payer.ReissueAttempt(c, order)
	if err != nil {
		writePortalCheckoutError(c, err)
		return
	}
	response.OK(c, orderView(&checkout.OrderResult{Order: order, Attempt: attempt}))
}

func (h *PortalPaymentsHandler) CancelOrder(c *gin.Context) {
	_, email, ok := portalSession(c)
	if !ok {
		return
	}
	order, ok := h.portalOrder(c, email)
	if !ok {
		return
	}
	canceled, err := h.Store.CancelPaymentOrder(c, order.Code)
	if errors.Is(err, store.ErrOrderNotFound) {
		response.Err(c, http.StatusNotFound, "PAYMENT_ORDER_NOT_FOUND", "order not found")
		return
	}
	if errors.Is(err, store.ErrOrderNotCancelable) || errors.Is(err, store.ErrOrderNotPayable) {
		response.Err(c, http.StatusConflict, "PAYMENT_ORDER_NOT_PAYABLE", "order is already paid or closed")
		return
	}
	if err != nil {
		response.Internal(c, err)
		return
	}
	h.Events.OrderCanceled(c, canceled)
	response.OK(c, orderView(&checkout.OrderResult{Order: canceled}))
}

// portalOrder reads the :code order under the customer predicate. A
// foreign code is written as the same 404 as an unknown one.
func (h *PortalPaymentsHandler) portalOrder(c *gin.Context, email string) (*model.PaymentOrder, bool) {
	order, err := h.Store.GetPortalPaymentOrder(c, c.Param("code"), email)
	if errors.Is(err, store.ErrOrderNotFound) {
		response.Err(c, http.StatusNotFound, "PAYMENT_ORDER_NOT_FOUND", "order not found")
		return nil, false
	}
	if err != nil {
		response.Internal(c, err)
		return nil, false
	}
	return order, true
}

func writePortalCheckoutError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrOrderNotFound):
		response.Err(c, http.StatusNotFound, "PAYMENT_ORDER_NOT_FOUND", "order not found")
	case errors.Is(err, store.ErrOrderNotPayable):
		response.Err(c, http.StatusConflict, "PAYMENT_ORDER_NOT_PAYABLE", "order is already paid or closed")
	case errors.Is(err, checkout.ErrProviderUnavailable):
		response.Err(c, http.StatusServiceUnavailable, "PAYMENT_PROVIDER_DISABLED",
			"the payment provider is not configured")
	case errors.Is(err, checkout.ErrProviderError):
		response.Err(c, http.StatusBadGateway, "PAYMENT_PROVIDER_ERROR",
			"the payment provider could not create an instruction")
	default:
		response.Internal(c, err)
	}
}

// ─── invoices ───

func (h *PortalPaymentsHandler) ListInvoices(c *gin.Context) {
	_, email, ok := portalSession(c)
	if !ok {
		return
	}
	if h.Invoices == nil {
		response.Internal(c, errPortalInvoicesUnavailable)
		return
	}
	page := listPage(c)
	rows, total, err := h.Invoices.List(c, PortalInvoiceFilter{
		BuyerEmail: email, Limit: page.Limit, Offset: page.Offset,
	})
	if err != nil {
		response.Internal(c, err)
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, inv := range rows {
		out = append(out, portalInvoiceRowView(inv))
	}
	listOK(c, "invoices", out, total, page)
}

// GetInvoice answers JSON, or the stored PDF when ?format=pdf is asked
// for. Ownership is checked before the PDF is fetched, so a foreign id
// cannot even learn whether a PDF exists.
func (h *PortalPaymentsHandler) GetInvoice(c *gin.Context) {
	_, email, ok := portalSession(c)
	if !ok {
		return
	}
	if h.Invoices == nil {
		response.Internal(c, errPortalInvoicesUnavailable)
		return
	}
	id := c.Param("id")
	inv, err := h.Invoices.Get(c, id)
	if errors.Is(err, ErrPortalInvoiceNotFound) || errors.Is(err, sql.ErrNoRows) {
		response.Err(c, http.StatusNotFound, "INVOICE_NOT_FOUND", "invoice not found")
		return
	}
	if err != nil {
		response.Internal(c, err)
		return
	}
	owned, err := h.invoiceOwned(c, inv, email)
	if err != nil {
		response.Internal(c, err)
		return
	}
	if !owned {
		response.Err(c, http.StatusNotFound, "INVOICE_NOT_FOUND", "invoice not found")
		return
	}

	switch strings.ToLower(strings.TrimSpace(c.Query("format"))) {
	case "":
		response.OK(c, portalInvoiceDetailView(inv))
	case "pdf":
		pdf, err := h.Invoices.PDF(c, id)
		if errors.Is(err, ErrPortalInvoiceNotFound) || errors.Is(err, sql.ErrNoRows) {
			response.Err(c, http.StatusNotFound, "INVOICE_NOT_FOUND", "invoice not found")
			return
		}
		if err != nil {
			response.Internal(c, err)
			return
		}
		c.Header("Content-Type", "application/pdf")
		c.Header("Content-Disposition", `attachment; filename="`+portalInvoiceFilename(inv)+`"`)
		c.Header("Cache-Control", "private, no-store")
		c.Data(http.StatusOK, "application/pdf", pdf)
	default:
		response.BadRequest(c, "format must be pdf")
	}
}

// invoiceOwned reports whether the invoice belongs to the caller: its
// buyer snapshot names them, or the order it was issued for does. The
// second path exists because the invoice buyer block is written from the
// order at issue time and may name a billing contact instead.
func (h *PortalPaymentsHandler) invoiceOwned(c *gin.Context, inv *model.Invoice, email string) (bool, error) {
	if strings.EqualFold(portalBuyerEmail(inv.Buyer), email) {
		return true, nil
	}
	if inv.OrderID == "" {
		return false, nil
	}
	order, err := h.Store.GetPaymentOrderByID(c, inv.OrderID)
	if errors.Is(err, store.ErrOrderNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.EqualFold(order.Email, email) ||
		strings.EqualFold(portalBuyerEmail(order.Buyer), email), nil
}

func portalBuyerEmail(raw json.RawMessage) string {
	var buyer struct {
		Email string `json:"email"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &buyer)
	}
	return strings.TrimSpace(buyer.Email)
}

func portalInvoiceFilename(inv *model.Invoice) string {
	name := strings.TrimSpace(inv.Number)
	if name == "" {
		name = "invoice"
	}
	return strings.NewReplacer(`"`, "_", `\`, "_", "\r", "_", "\n", "_").Replace(name) + ".pdf"
}

func portalInvoiceRowView(inv *model.Invoice) gin.H {
	row := gin.H{
		"id": inv.ID, "number": inv.Number, "order_id": inv.OrderID, "status": inv.Status,
		"subtotal_minor": inv.SubtotalMinor, "discount_minor": inv.DiscountMinor,
		"tax_minor": inv.TaxMinor, "total_minor": inv.TotalMinor,
		"currency": inv.Currency, "currency_exponent": inv.CurrencyExp,
		"created_at": inv.CreatedAt,
	}
	if inv.IssuedAt != nil {
		row["issued_at"] = inv.IssuedAt
	}
	if inv.LicenseID != "" {
		row["license_id"] = inv.LicenseID
	}
	return row
}

func portalInvoiceDetailView(inv *model.Invoice) gin.H {
	out := portalInvoiceRowView(inv)
	buyer := inv.Buyer
	if len(buyer) == 0 || string(buyer) == "null" {
		buyer = json.RawMessage(`{}`)
	}
	items := inv.Items
	if items == nil {
		items = []model.InvoiceItem{}
	}
	out["buyer"] = buyer
	out["items"] = items
	return out
}

// ─── billing profile ───

type portalBillingProfileInput struct {
	Name           string `json:"name"`
	CompanyName    string `json:"company_name"`
	TaxCode        string `json:"tax_code"`
	Address        string `json:"address"`
	Email          string `json:"email"`
	Phone          string `json:"phone"`
	RequestInvoice bool   `json:"request_invoice"`
}

type portalBillingProfileRequest struct {
	Buyer          *portalBillingProfileInput `json:"buyer"`
	Name           string                     `json:"name"`
	CompanyName    string                     `json:"company_name"`
	TaxCode        string                     `json:"tax_code"`
	Address        string                     `json:"address"`
	Email          string                     `json:"email"`
	Phone          string                     `json:"phone"`
	RequestInvoice bool                       `json:"request_invoice"`
}

func (r portalBillingProfileRequest) input() portalBillingProfileInput {
	if r.Buyer != nil {
		return *r.Buyer
	}
	return portalBillingProfileInput{
		Name: r.Name, CompanyName: r.CompanyName, TaxCode: r.TaxCode, Address: r.Address,
		Email: r.Email, Phone: r.Phone, RequestInvoice: r.RequestInvoice,
	}
}

var (
	errPortalBuyerName  = errors.New("buyer.name is required")
	errPortalBuyerField = errors.New("a buyer field is too long")
	errPortalTaxCode    = errors.New("buyer.tax_code is required when request_invoice is true")
)

// clean trims every field and enforces the portal's field bounds. The
// tax rule is spec §3.8: a VAT invoice cannot be requested without a
// buyer tax code.
func (in *portalBillingProfileInput) clean() error {
	in.Name = strings.TrimSpace(in.Name)
	in.CompanyName = strings.TrimSpace(in.CompanyName)
	in.TaxCode = strings.TrimSpace(in.TaxCode)
	in.Address = strings.TrimSpace(in.Address)
	in.Email = strings.TrimSpace(in.Email)
	in.Phone = strings.TrimSpace(in.Phone)
	switch {
	case in.Name == "":
		return errPortalBuyerName
	case utf8.RuneCountInString(in.Name) > 200,
		utf8.RuneCountInString(in.CompanyName) > 200,
		utf8.RuneCountInString(in.TaxCode) > 50,
		utf8.RuneCountInString(in.Address) > 500,
		utf8.RuneCountInString(in.Email) > 200,
		utf8.RuneCountInString(in.Phone) > 50:
		return errPortalBuyerField
	case in.RequestInvoice && in.TaxCode == "":
		return errPortalTaxCode
	}
	return nil
}

// portalBuyerJSON is the stored buyer block. Optional fields disappear
// when empty; request_invoice is always written so a later edit can turn
// it off explicitly rather than by absence.
type portalBuyerJSON struct {
	Name           string `json:"name"`
	CompanyName    string `json:"company_name,omitempty"`
	TaxCode        string `json:"tax_code,omitempty"`
	Address        string `json:"address,omitempty"`
	Email          string `json:"email,omitempty"`
	Phone          string `json:"phone,omitempty"`
	RequestInvoice bool   `json:"request_invoice"`
}

func (h *PortalPaymentsHandler) GetBillingProfile(c *gin.Context) {
	userID, _, ok := portalSession(c)
	if !ok {
		return
	}
	profile, err := h.Store.GetBillingProfile(c, userID)
	if errors.Is(err, sql.ErrNoRows) {
		response.OK(c, gin.H{"buyer": gin.H{}})
		return
	}
	if err != nil {
		response.Internal(c, err)
		return
	}
	buyer := profile.Buyer
	if len(buyer) == 0 || string(buyer) == "null" {
		buyer = json.RawMessage(`{}`)
	}
	response.OK(c, gin.H{"buyer": buyer})
}

// SaveBillingProfile validates and upserts the one buyer block this user
// has. A successful write is audited; a rejected one writes nothing.
func (h *PortalPaymentsHandler) SaveBillingProfile(c *gin.Context) {
	userID, _, ok := portalSession(c)
	if !ok {
		return
	}
	var req portalBillingProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid JSON body")
		return
	}
	in := req.input()
	if err := in.clean(); err != nil {
		switch {
		case errors.Is(err, errPortalTaxCode):
			response.Err(c, http.StatusUnprocessableEntity, "PAYMENT_TAX_CODE_REQUIRED",
				"buyer.tax_code is required when request_invoice is true")
		default:
			response.BadRequest(c, err.Error())
		}
		return
	}
	encoded, err := json.Marshal(portalBuyerJSON{
		Name: in.Name, CompanyName: in.CompanyName, TaxCode: in.TaxCode,
		Address: in.Address, Email: in.Email, Phone: in.Phone, RequestInvoice: in.RequestInvoice,
	})
	if err != nil {
		response.Internal(c, err)
		return
	}
	profile, err := h.Store.UpsertBillingProfile(c, userID, encoded)
	if err != nil {
		response.Internal(c, err)
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "customer_billing_profile", EntityID: userID, Action: "updated",
		ActorType: "portal", ActorID: userID, IPAddress: c.ClientIP(),
		Changes: map[string]any{"name": in.Name, "request_invoice": in.RequestInvoice},
	})
	response.OK(c, gin.H{"buyer": profile.Buyer})
}
