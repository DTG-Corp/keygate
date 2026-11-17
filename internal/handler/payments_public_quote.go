package handler

import (
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dtg-corp/dsoft/internal/middleware"
	"github.com/dtg-corp/dsoft/internal/payment/checkout"
	"github.com/dtg-corp/dsoft/pkg/response"
)

// PublicQuoteRoute is the unauthenticated dry-run price (spec §6.1). It is
// the only payment route that both reads prices and creates nothing, so it
// shares the public bucket's budget with the pay-page status poll rather
// than opening a second one.
const PublicQuoteRoute = "/api/v1/payments/quote"

var errQuoteNotWired = errors.New("public quote is not wired")

// RegisterPublicQuoteRoutes mounts the public quote on r.
func RegisterPublicQuoteRoutes(r gin.IRoutes, h *PaymentsPublicHandler, rateLimit int) {
	r.POST(PublicQuoteRoute,
		middleware.RateLimitByIPScoped(publicStatusRateLimitScope, rateLimit, time.Minute), h.Quote)
}

// publicQuoteRequest is the documented body. The buyer's email is optional
// here (a pricing widget has not collected one yet); coupon per-customer
// limits simply cannot apply without it, which is why the checkout call
// that creates the order always carries one.
type publicQuoteRequest struct {
	PlanID               string   `json:"plan_id"`
	Quantity             int      `json:"quantity"`
	Email                string   `json:"email"`
	CouponCodes          []string `json:"coupon_codes"`
	IgnoreInvalidCoupons bool     `json:"ignore_invalid_coupons"`
}

// Quote prices a plan with its coupons and tax without creating an order,
// an attempt or a redemption.
func (h *PaymentsPublicHandler) Quote(c *gin.Context) {
	if h.Checkout == nil {
		response.Internal(c, errQuoteNotWired)
		return
	}
	var req publicQuoteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.PlanID) == "" {
		response.BadRequest(c, "plan_id is required")
		return
	}
	res, err := h.Checkout.PreviewQuote(c.Request.Context(), checkout.PreviewInput{
		PlanID: req.PlanID, Quantity: req.Quantity, Email: strings.TrimSpace(req.Email),
		Codes: req.CouponCodes, IgnoreInvalid: req.IgnoreInvalidCoupons,
	})
	if err != nil {
		writeCheckoutError(c, err)
		return
	}
	// Deliberately narrow: a public caller sees the price it asked for,
	// not the internal plan ids, product metadata or coupon internals.
	response.OK(c, gin.H{
		"subtotal_minor":    res.Quote.SubtotalMinor,
		"discount_minor":    res.Quote.DiscountMinor,
		"tax_minor":         res.Quote.TaxMinor,
		"total_minor":       res.Quote.TotalMinor,
		"currency":          res.Price.Currency,
		"currency_exponent": res.Price.CurrencyExponent,
		"billing_interval":  res.Price.BillingInterval,
		"trial_days":        res.Quote.TrialDays,
		"discounts":         res.Quote.Discounts,
	})
}
