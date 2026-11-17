package handler

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/checkout"
	"github.com/dtg-corp/dsoft/internal/payment/provider"
	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/pkg/response"
)

// PaymentsAPIHandler is the consumer-facing payments API: merchant
// backends create orders, poll them and cancel them with an API key
// carrying payments:write / payments:read. It is not the admin API, and
// it never returns another product's orders.
type PaymentsAPIHandler struct {
	Checkout *checkout.Service
	Store    *store.Store
}

type createOrderRequest struct {
	PlanID   string         `json:"plan_id"`
	Email    string         `json:"email"`
	Provider string         `json:"provider,omitempty"`
	Quantity int            `json:"quantity,omitempty"`
	Buyer    provider.Buyer `json:"buyer,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

func (h *PaymentsAPIHandler) Create(c *gin.Context) {
	var req createOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.PlanID) == "" {
		response.BadRequest(c, "plan_id is required")
		return
	}
	key := apiKeyFrom(c)
	if key != nil && key.ProductID != "" {
		// Check the binding before checkout writes anything: a
		// product-bound key must not be able to create an order for
		// another product's plan.
		plan, err := h.Store.FindPlanByID(c, req.PlanID)
		if errors.Is(err, sql.ErrNoRows) {
			writeCheckoutError(c, checkout.ErrPlanNotSellable)
			return
		}
		if err != nil {
			response.Internal(c, err)
			return
		}
		if plan.ProductID != key.ProductID {
			response.Err(c, http.StatusForbidden, "PRODUCT_SCOPE_MISMATCH",
				"the plan belongs to another product")
			return
		}
	}
	res, err := h.Checkout.CreateOrder(c, checkout.CreateOrderInput{
		PlanID: req.PlanID, Email: req.Email, Provider: req.Provider, Quantity: req.Quantity,
		Buyer: req.Buyer, Metadata: req.Metadata, CreatedByKeyID: apiKeyID(c),
	})
	if err != nil {
		writeCheckoutError(c, err)
		return
	}
	response.Created(c, orderView(res))
}

func (h *PaymentsAPIHandler) Get(c *gin.Context) {
	order, ok := h.orderInScope(c)
	if !ok {
		return
	}
	res := &checkout.OrderResult{Order: order}
	attempt, err := h.Store.GetLatestPaymentAttempt(c, order.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		response.Internal(c, err)
		return
	}
	res.Attempt = attempt
	response.OK(c, orderView(res))
}

func (h *PaymentsAPIHandler) List(c *gin.Context) {
	page := listPage(c)
	q := h.Store.DB.NewSelect().Model((*model.PaymentOrder)(nil))

	if status := strings.ToLower(strings.TrimSpace(c.Query("status"))); status != "" {
		q = q.Where("status = ?", status)
	}
	if providerName := strings.ToLower(strings.TrimSpace(c.Query("provider"))); providerName != "" {
		q = q.Where("provider = ?", providerName)
	}
	if email := strings.ToLower(strings.TrimSpace(c.Query("email"))); email != "" {
		// payment_orders_email_idx is on lower(email), which is also the
		// column the order rows store.
		q = q.Where("lower(email) = ?", email)
	}
	if from := c.Query("created_from"); from != "" {
		ts, err := time.Parse(time.RFC3339, from)
		if err != nil {
			response.BadRequest(c, "created_from must be an RFC 3339 timestamp")
			return
		}
		q = q.Where("created_at >= ?", ts)
	}
	if to := c.Query("created_to"); to != "" {
		ts, err := time.Parse(time.RFC3339, to)
		if err != nil {
			response.BadRequest(c, "created_to must be an RFC 3339 timestamp")
			return
		}
		q = q.Where("created_at <= ?", ts)
	}

	productID := strings.TrimSpace(c.Query("product_id"))
	if key := apiKeyFrom(c); key != nil && key.ProductID != "" {
		// The key's product is the filter, not a caller choice: asking
		// for another product is refused rather than silently narrowed.
		if productID != "" && productID != key.ProductID {
			response.Err(c, http.StatusForbidden, "PRODUCT_SCOPE_MISMATCH",
				"the key is bound to another product")
			return
		}
		productID = key.ProductID
	}
	if productID != "" {
		q = q.Where("product_id = ?", productID)
	}

	total, err := q.Count(c)
	if err != nil {
		response.Internal(c, err)
		return
	}
	orders := make([]*model.PaymentOrder, 0)
	q = q.Model(&orders).Order("created_at DESC", "id DESC").
		Limit(page.Limit).Offset(page.Offset)
	if err := q.Scan(c); err != nil {
		response.Internal(c, err)
		return
	}
	rows := make([]gin.H, 0, len(orders))
	for _, o := range orders {
		rows = append(rows, orderRowView(o))
	}
	listOK(c, "data", rows, total, page)
}

func (h *PaymentsAPIHandler) Cancel(c *gin.Context) {
	if _, ok := h.orderInScope(c); !ok {
		return
	}
	order, err := h.Store.CancelPaymentOrder(c, c.Param("code"))
	if errors.Is(err, store.ErrOrderNotFound) {
		response.Err(c, http.StatusNotFound, "PAYMENT_ORDER_NOT_FOUND", "order not found")
		return
	}
	// CancelPaymentOrder answers ErrOrderNotCancelable; ErrOrderNotPayable
	// is accepted too because both mean the same thing to a client: the
	// order already ended and nothing was changed.
	if errors.Is(err, store.ErrOrderNotCancelable) || errors.Is(err, store.ErrOrderNotPayable) {
		response.Err(c, http.StatusConflict, "PAYMENT_ORDER_NOT_PAYABLE", "order is already paid or closed")
		return
	}
	if err != nil {
		response.Internal(c, err)
		return
	}
	response.OK(c, orderView(&checkout.OrderResult{Order: order}))
}

// orderInScope reads the order named by :code and refuses it when the
// caller's key is bound to another product. An out-of-scope order is a
// 404, not a 403: the key must not learn that the code exists.
func (h *PaymentsAPIHandler) orderInScope(c *gin.Context) (*model.PaymentOrder, bool) {
	order, err := h.Store.GetPaymentOrderByCode(c, c.Param("code"))
	if errors.Is(err, store.ErrOrderNotFound) {
		response.Err(c, http.StatusNotFound, "PAYMENT_ORDER_NOT_FOUND", "order not found")
		return nil, false
	}
	if err != nil {
		response.Internal(c, err)
		return nil, false
	}
	if key := apiKeyFrom(c); key != nil && key.ProductID != "" && order.ProductID != key.ProductID {
		response.Err(c, http.StatusNotFound, "PAYMENT_ORDER_NOT_FOUND", "order not found")
		return nil, false
	}
	return order, true
}

func writeCheckoutError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, checkout.ErrQuantityInvalid):
		response.Err(c, http.StatusBadRequest, "INVALID_QUANTITY", err.Error())
	case errors.Is(err, checkout.ErrProviderError):
		// The request was valid; the provider API failed while creating
		// the instruction. The order stays pending, so a retry is safe.
		response.Err(c, http.StatusBadGateway, "PAYMENT_PROVIDER_ERROR",
			"the payment provider could not create the payment instruction")
	case errors.Is(err, checkout.ErrProviderUnavailable):
		response.Err(c, http.StatusServiceUnavailable, "PAYMENT_PROVIDER_DISABLED", err.Error())
	case errors.Is(err, store.ErrPriceNotConfigured), errors.Is(err, checkout.ErrPlanNotSellable):
		response.Err(c, http.StatusUnprocessableEntity, "PAYMENT_PRICE_NOT_CONFIGURED", err.Error())
	default:
		response.Internal(c, err)
	}
}

// apiKeyFrom returns the key the auth middleware attached, if any. The
// handlers are also called directly in tests and by the admin routes,
// where no key is present and the caller is already trusted.
func apiKeyFrom(c *gin.Context) *model.APIKey {
	v, ok := c.Get("api_key")
	if !ok {
		return nil
	}
	ak, _ := v.(*model.APIKey)
	return ak
}

func apiKeyID(c *gin.Context) string {
	if ak := apiKeyFrom(c); ak != nil {
		return ak.ID
	}
	return ""
}

func orderView(res *checkout.OrderResult) gin.H {
	out := gin.H{
		"code": res.Order.Code, "status": res.Order.Status, "kind": res.Order.Kind,
		"provider":   res.Order.Provider,
		"product_id": res.Order.ProductID, "plan_id": res.Order.PlanID,
		"email": res.Order.Email, "quantity": res.Order.Quantity,
		"subtotal_minor": res.Order.SubtotalMinor, "discount_minor": res.Order.DiscountMinor,
		"tax_minor": res.Order.TaxMinor, "total_minor": res.Order.TotalMinor,
		"currency": res.Order.Currency, "currency_exponent": res.Order.CurrencyExp,
		"expires_at": res.Order.ExpiresAt,
	}
	if res.Order.LicenseID != "" {
		out["license_id"] = res.Order.LicenseID
	}
	if res.Order.PaidAt != nil {
		out["paid_at"] = res.Order.PaidAt
	}
	if res.Attempt != nil {
		payment := gin.H{
			"provider": res.Attempt.Provider, "provider_ref": res.Attempt.ProviderRef,
			"qr_payload": res.Attempt.QRPayload, "qr_image_url": attemptQRImage(res.Attempt),
			"checkout_url": res.Attempt.CheckoutURL,
			"va_number":    res.Attempt.VANumber, "expires_at": res.Attempt.ExpiresAt,
		}
		if raw := attemptRawOf(res.Attempt); raw.Bank.AccountNo != "" {
			payment["bank_account"] = raw.Bank
		}
		out["payment"] = payment
	}
	return out
}

// orderRowView is the compact list row: no buyer blob, metadata or the
// creating key id in every element of a page.
func orderRowView(o *model.PaymentOrder) gin.H {
	row := gin.H{
		"code": o.Code, "status": o.Status, "kind": o.Kind, "provider": o.Provider,
		"product_id": o.ProductID, "plan_id": o.PlanID, "email": o.Email,
		"quantity": o.Quantity, "total_minor": o.TotalMinor,
		"currency": o.Currency, "currency_exponent": o.CurrencyExp,
		"expires_at": o.ExpiresAt, "created_at": o.CreatedAt,
	}
	if o.LicenseID != "" {
		row["license_id"] = o.LicenseID
	}
	if o.PaidAt != nil {
		row["paid_at"] = o.PaidAt
	}
	return row
}
