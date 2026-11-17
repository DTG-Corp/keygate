package handler

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/payment/checkout"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/pkg/response"
)

// CouponsPortalHandler answers the customer portal's "do I have a code?"
// question. It prices the code against a plan without creating an order,
// so a buyer sees the discounted number before committing to it.
type CouponsPortalHandler struct {
	Checkout *checkout.Service
}

// RegisterCouponsPortalRoutes mounts the portal coupon route on the
// portal group. The Lead supplies the session middleware on that group:
// this handler reads the buyer's email from the session it set and never
// trusts one in the body.
func RegisterCouponsPortalRoutes(g *gin.RouterGroup, h *CouponsPortalHandler) {
	g.POST("/coupons/validate", h.Validate)
}

// portalCouponRequest is one code validated against one plan. A portal
// buyer applies a single code; stacking is decided at checkout.
type portalCouponRequest struct {
	PlanID   string `json:"plan_id"`
	Quantity int    `json:"quantity,omitempty"`
	Code     string `json:"code"`
}

// Validate answers POST /portal/coupons/validate.
func (h *CouponsPortalHandler) Validate(c *gin.Context) {
	email, _ := c.Get("email")
	buyer, _ := email.(string)
	buyer = strings.ToLower(strings.TrimSpace(buyer))
	if buyer == "" {
		// Without a session there is no buyer to check the code's
		// per-customer limit against.
		response.Unauthorized(c, "a portal session is required")
		return
	}
	var req portalCouponRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.PlanID) == "" {
		response.BadRequest(c, "plan_id is required")
		return
	}
	res, err := h.Checkout.PreviewQuote(c, checkout.PreviewInput{
		PlanID: req.PlanID, Quantity: req.Quantity, Codes: []string{req.Code},
		Email: buyer, Portal: true,
	})
	if err != nil {
		writeCheckoutError(c, err)
		return
	}
	response.OK(c, gin.H{
		"valid":             true,
		"plan":              gin.H{"id": res.Plan.ID, "name": res.Plan.Name, "slug": res.Plan.Slug},
		"product":           gin.H{"id": res.Product.ID, "name": res.Product.Name},
		"price_minor":       res.Price.AmountMinor,
		"currency":          res.Price.Currency,
		"currency_exponent": domain.CurrencyExponent(res.Price.Currency),
		"subtotal_minor":    res.Quote.SubtotalMinor, "discount_minor": res.Quote.DiscountMinor,
		"tax_minor": res.Quote.TaxMinor, "total_minor": res.Quote.TotalMinor,
		"trial_days": res.Quote.TrialDays,
		"discounts":  res.Quote.Discounts,
	})
}
