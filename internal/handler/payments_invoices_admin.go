package handler

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/invoice"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// vnZone is the calendar invoice dates and report buckets use. Vietnam
// has no DST, so a fixed offset is exact and needs no tzdata on the
// server; it must stay in step with the zone the store aggregates in.
var vnZone = time.FixedZone("ICT", 7*3600)

// InvoicesAdminHandler serves the operator's invoice ledger and the
// manual e-invoice queue. Reads are admin-only like the order ledger:
// an invoice carries the buyer's name, tax code and address.
type InvoicesAdminHandler struct {
	Invoices *invoice.Service
}

// RegisterInvoicesAdminRoutes mounts the invoice routes on g, which the
// server passes as the /api/v1/admin group. They live in a function so a
// router test mounts exactly what main mounts.
func RegisterInvoicesAdminRoutes(g *gin.RouterGroup, h *InvoicesAdminHandler) {
	g.GET("/payments/invoices", h.List)
	g.GET("/payments/invoices/:id", h.Get)
	g.POST("/payments/invoices/:id/issue", h.Issue)
	g.POST("/payments/invoices/:id/void", h.Void)
	g.GET("/payments/invoices/:id/pdf", h.PDF)
}

// adminInvoiceView is the invoice projection the console reads. The
// buyer block is passed through as the frozen JSON the invoice stored,
// so an operator sees exactly what the document says; pdf_url is
// derived, not persisted.
type adminInvoiceView struct {
	ID               string              `json:"id"`
	Number           string              `json:"number"`
	OrderID          string              `json:"order_id"`
	LicenseID        string              `json:"license_id,omitempty"`
	Status           string              `json:"status"`
	Buyer            any                 `json:"buyer"`
	Items            []model.InvoiceItem `json:"items"`
	SubtotalMinor    int64               `json:"subtotal_minor"`
	DiscountMinor    int64               `json:"discount_minor"`
	TaxMinor         int64               `json:"tax_minor"`
	TotalMinor       int64               `json:"total_minor"`
	Currency         string              `json:"currency"`
	CurrencyExponent int16               `json:"currency_exponent"`
	EInvoiceProvider string              `json:"e_invoice_provider,omitempty"`
	EInvoiceRef      string              `json:"e_invoice_ref,omitempty"`
	EInvoiceStatus   string              `json:"e_invoice_status,omitempty"`
	PDFURL           string              `json:"pdf_url"`
	IssuedAt         *time.Time          `json:"issued_at"`
	CreatedAt        time.Time           `json:"created_at"`
	UpdatedAt        time.Time           `json:"updated_at"`
}

func adminInvoiceViewOf(inv *model.Invoice) adminInvoiceView {
	items := inv.Items
	if items == nil {
		items = []model.InvoiceItem{}
	}
	return adminInvoiceView{
		ID: inv.ID, Number: inv.Number, OrderID: inv.OrderID, LicenseID: inv.LicenseID,
		Status: inv.Status, Buyer: inv.Buyer, Items: items,
		SubtotalMinor: inv.SubtotalMinor, DiscountMinor: inv.DiscountMinor,
		TaxMinor: inv.TaxMinor, TotalMinor: inv.TotalMinor,
		Currency: inv.Currency, CurrencyExponent: inv.CurrencyExp,
		EInvoiceProvider: inv.EInvoiceProvider, EInvoiceRef: inv.EInvoiceRef,
		EInvoiceStatus: inv.EInvoiceStatus,
		PDFURL:         "/api/v1/admin/payments/invoices/" + inv.ID + "/pdf",
		IssuedAt:       inv.IssuedAt, CreatedAt: inv.CreatedAt, UpdatedAt: inv.UpdatedAt,
	}
}

// invoiceStatuses is the status allowlist of the list filter. A typo is
// refused rather than silently ignored, so a console that asked for
// "pending" does not quietly show every invoice.
var invoiceStatuses = map[string]bool{
	model.InvoiceStatusDraft: true, model.InvoiceStatusIssued: true,
	model.InvoiceStatusPendingIssue: true, model.InvoiceStatusVoid: true,
}

func (h *InvoicesAdminHandler) List(c *gin.Context) {
	status := strings.ToLower(strings.TrimSpace(c.Query("status")))
	if status != "" && !invoiceStatuses[status] {
		response.Err(c, http.StatusBadRequest, "INVALID_STATUS",
			"unknown invoice status, expected one of: draft, issued, pending_issue, void")
		return
	}
	from, err := parseTimeParam(c.Query("from"), false)
	if err != nil {
		response.BadRequest(c, "from must be RFC3339 or YYYY-MM-DD")
		return
	}
	to, err := parseTimeParam(c.Query("to"), true)
	if err != nil {
		response.BadRequest(c, "to must be RFC3339 or YYYY-MM-DD")
		return
	}
	if !from.IsZero() && !to.IsZero() && to.Before(from) {
		response.BadRequest(c, "to must not be before from")
		return
	}
	page := listPage(c)
	filter := invoice.ListFilter{
		Status: status, LicenseID: c.Query("license_id"), OrderID: c.Query("order_id"),
		Number: c.Query("number"), ProductID: c.Query("product_id"), Page: page,
	}
	if !from.IsZero() {
		filter.From = &from
	}
	if !to.IsZero() {
		filter.To = &to
	}
	rows, total, err := h.Invoices.List(c, filter)
	if err != nil {
		response.Internal(c, err)
		return
	}
	views := make([]adminInvoiceView, 0, len(rows))
	for _, inv := range rows {
		views = append(views, adminInvoiceViewOf(inv))
	}
	listOK(c, "invoices", views, total, page)
}

func (h *InvoicesAdminHandler) Get(c *gin.Context) {
	inv, err := h.Invoices.Get(c, c.Param("id"))
	if err != nil {
		writeInvoiceError(c, err)
		return
	}
	response.OK(c, gin.H{"invoice": adminInvoiceViewOf(inv)})
}

// Issue moves one pending invoice to issued — the manual e-invoice seam
// of spec §3.8. The actor is the authenticated admin, never a body
// field, so an operator cannot issue as somebody else.
func (h *InvoicesAdminHandler) Issue(c *gin.Context) {
	inv, err := h.Invoices.Issue(c, c.Param("id"), adminID(c))
	if err != nil {
		writeInvoiceError(c, err)
		return
	}
	response.OK(c, gin.H{"invoice": adminInvoiceViewOf(inv)})
}

// Void voids an invoice, optionally recording why. The document stays
// readable; only its status moves.
func (h *InvoicesAdminHandler) Void(c *gin.Context) {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		response.BadRequest(c, "invalid JSON body")
		return
	}
	inv, err := h.Invoices.VoidWithReason(c, c.Param("id"), adminID(c), strings.TrimSpace(req.Reason))
	if err != nil {
		writeInvoiceError(c, err)
		return
	}
	response.OK(c, gin.H{"invoice": adminInvoiceViewOf(inv)})
}

// PDF streams the invoice as an attachment-safe PDF. It is rendered or
// served from object storage by the service; the handler only sets the
// headers a browser needs to display it inline.
func (h *InvoicesAdminHandler) PDF(c *gin.Context) {
	body, inv, err := h.Invoices.PDF(c, c.Param("id"))
	if err != nil {
		writeInvoiceError(c, err)
		return
	}
	c.Header("Content-Disposition", `inline; filename="`+inv.Number+`.pdf"`)
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/pdf", body)
}

// writeInvoiceError maps the invoice sentinels onto the error vocabulary
// of spec §6.5. Each sentinel has exactly one status (a test in
// payments_contract_test.go enforces that), so a client can branch on
// the code.
func writeInvoiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrInvoiceNotFound):
		response.Err(c, http.StatusNotFound, "INVOICE_NOT_FOUND", "invoice not found")
	case errors.Is(err, store.ErrInvoiceAlreadyIssued):
		response.Err(c, http.StatusConflict, "INVOICE_ALREADY_ISSUED", "invoice has already been issued")
	case errors.Is(err, store.ErrInvoiceNotVoidable):
		response.Err(c, http.StatusConflict, "INVOICE_ALREADY_ISSUED", "invoice has already been voided")
	case errors.Is(err, invoice.ErrEInvoiceProvider):
		// The vendor's reason is for the operator's log, not the client.
		response.LogInternal(c, err)
		response.Err(c, http.StatusBadGateway, "EINVOICE_PROVIDER_ERROR",
			"the e-invoice provider rejected the request")
	default:
		response.Internal(c, err)
	}
}

// parseTimeParam reads a window bound. RFC3339 is taken as an instant;
// a bare YYYY-MM-DD is a Ho Chi Minh City calendar day, answered as its
// first instant, or its last when endOfDay is set, so
// from=2026-11-01&to=2026-11-30 covers the whole of the 30th. An empty
// value means "no bound" and returns the zero time.
func parseTimeParam(v string, endOfDay bool) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	day, err := time.ParseInLocation("2006-01-02", v, vnZone)
	if err != nil {
		return time.Time{}, err
	}
	if endOfDay {
		return day.Add(24*time.Hour - time.Nanosecond).UTC(), nil
	}
	return day.UTC(), nil
}
