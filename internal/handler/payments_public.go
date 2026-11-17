package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dtg-corp/dsoft/internal/middleware"
	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment"
	"github.com/dtg-corp/dsoft/internal/payment/checkout"
	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/payment/provider"
	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/pkg/response"
)

// The public order-status poll is mounted at two spellings: the spec
// §6.1 root path handed out in links, and the /api/v1 path the SPA's api
// client builds (it prefixes every request). Both share one rate-limit
// scope so the alias cannot be used to double the poll budget.
const (
	PublicStatusRoute    = "/pay/:checkout_id/status"
	PublicStatusAPIRoute = "/api/v1/pay/:checkout_id/status"
	// publicStatusRateLimitScope is the bucket both spellings share.
	publicStatusRateLimitScope = "payments_public"
)

// RegisterPublicStatusRoutes mounts the public status poll on r.
//
// It lives here rather than inline in main() so a router test mounts the
// exact routes the server does: main() itself has no test, and a dropped
// alias would otherwise only surface as a 404 in the browser.
func RegisterPublicStatusRoutes(r gin.IRoutes, h *PaymentsPublicHandler, rateLimit int) {
	r.GET(PublicStatusRoute,
		middleware.RateLimitByIPScoped(publicStatusRateLimitScope, rateLimit, time.Minute), h.Status)
	r.GET(PublicStatusAPIRoute,
		middleware.RateLimitByIPScoped(publicStatusRateLimitScope, rateLimit, time.Minute), h.Status)
}

// PaymentsPublicHandler serves the two unauthenticated payment routes.
// The order code is the capability: anyone holding it may see the
// amount and the payment instruction, and nothing else about the buyer.
type PaymentsPublicHandler struct {
	Checkout *checkout.Service
	Store    *store.Store
	// Stripe keeps serving the legacy /pay/:checkout_id links; it is
	// never called for a KG order code.
	Stripe *payment.StripeHandler
}

// statusView is the public pay-page payload. It is deliberately flat and
// contains no buyer email, metadata or API-key id: the order code is the
// only capability the holder has.
func statusView(order *model.PaymentOrder, attempt *model.PaymentAttempt) gin.H {
	out := gin.H{
		"code": order.Code, "status": order.Status,
		"amount_minor": order.TotalMinor, "currency": order.Currency,
		"currency_exponent": order.CurrencyExp,
		"expires_at":        order.ExpiresAt, "description": order.Code,
	}
	if attempt == nil {
		return out
	}
	out["qr_image_url"] = attemptQRImage(attempt)
	out["checkout_url"] = attempt.CheckoutURL
	raw := attemptRawOf(attempt)
	if raw.Bank.AccountNo != "" {
		out["bank"] = raw.Bank
	}
	if raw.Description != "" {
		out["description"] = raw.Description
	}
	return out
}

// attemptRawView is the non-secret part of CreateAttemptResult that
// checkout persists in payment_attempts.raw.
type attemptRawView struct {
	Bank        provider.BankAccount `json:"bank"`
	Description string               `json:"description"`
	// QRImageURL is the renderable QR. SePay's QRPayload is already an
	// image URL; PayOS's is an EMVCo string, so its image is only here.
	QRImageURL string `json:"qr_image_url"`
}

func attemptRawOf(a *model.PaymentAttempt) attemptRawView {
	var raw attemptRawView
	if len(a.Raw) > 0 {
		_ = json.Unmarshal(a.Raw, &raw)
	}
	return raw
}

// attemptQRImage picks the value a page can put in an <img src>: the
// adapter's image when it stored one, otherwise the payload itself.
func attemptQRImage(a *model.PaymentAttempt) string {
	if raw := attemptRawOf(a); raw.QRImageURL != "" {
		return raw.QRImageURL
	}
	return a.QRPayload
}

func (h *PaymentsPublicHandler) Status(c *gin.Context) {
	code := c.Param("checkout_id")
	if !domain.IsOrderCode(code) {
		response.NotFound(c, "checkout not found")
		return
	}
	order, err := h.Store.GetPaymentOrderByCode(c, code)
	if errors.Is(err, store.ErrOrderNotFound) {
		response.NotFound(c, "checkout not found")
		return
	}
	if err != nil {
		response.Internal(c, err)
		return
	}
	attempt, err := h.Store.GetLatestPaymentAttempt(c, order.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		response.Internal(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.OK(c, statusView(order, attempt))
}

// PayPage keeps the single existing wildcard route alive: Stripe links
// (the 8-character checkout ids the plans table still hands out) keep
// their current handler, KG order codes answer with a 302 to the SPA
// page that polls the status endpoint.
func (h *PaymentsPublicHandler) PayPage(c *gin.Context) {
	id := c.Param("checkout_id")
	if domain.IsOrderCode(id) {
		c.Redirect(http.StatusFound, "/checkout/pay/"+id)
		return
	}
	// Everything that is not an order code is a legacy Stripe checkout
	// id and keeps the behaviour it has always had, including that
	// handler's own validation and error bodies.
	if h.Stripe != nil {
		h.Stripe.CheckoutByPlan(c)
		return
	}
	response.NotFound(c, "checkout not found")
}
