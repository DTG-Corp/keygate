package handler

import (
	"database/sql"
	"errors"
	"log/slog"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// PublicPlansHandler serves GET /api/v1/products/:product_slug/plans —
// the plan catalogue a pricing page needs before the visitor has an
// account. /portal/plans covers the same ground but sits behind a
// session, which is no use to an anonymous visitor deciding whether to
// buy.
//
// Prices are read from plan_prices only: Stripe is not consulted in this
// phase. A plan with no active local price is not sellable, so it is
// omitted from the response rather than listed with a null price.
type PublicPlansHandler struct {
	Store *store.Store
	Log   *slog.Logger
}

func NewPublicPlansHandler(s *store.Store, log *slog.Logger) *PublicPlansHandler {
	if log == nil {
		log = slog.Default()
	}
	return &PublicPlansHandler{Store: s, Log: log}
}

func (h *PublicPlansHandler) List(c *gin.Context) {
	slug := strings.ToLower(strings.TrimSpace(c.Param("product_slug")))
	if slug == "" {
		response.BadRequest(c, "product_slug is required")
		return
	}

	prod, err := h.Store.FindProductBySlug(c, slug)
	if err != nil {
		// "No such product" and "lookup blew up" answer the same, so
		// slug guesses learn nothing from the difference. The operator
		// still needs to tell them apart, and only one of the two is
		// worth a log line.
		if !errors.Is(err, sql.ErrNoRows) {
			h.Log.Error("public plans: product lookup failed", "slug", slug, "error", err)
		}
		response.NotFound(c, "product not found")
		return
	}

	plans, _, err := h.Store.ListPlans(c, prod.ID, "", store.All)
	if err != nil {
		response.Internal(c, err)
		return
	}

	ids := make([]string, 0, len(plans))
	for _, p := range plans {
		ids = append(ids, p.ID)
	}
	prices, err := h.Store.ListActivePlanPrices(c, ids)
	if err != nil {
		response.Internal(c, err)
		return
	}

	// No Stripe secrets and no internal limits like max_activations:
	// just what a pricing page renders, whether the plan is sold per
	// seat, and the checkout_id that starts a purchase.
	active := make([]gin.H, 0, len(plans))
	for _, p := range plans {
		if !p.Active {
			continue
		}
		rows := prices[p.ID]
		if len(rows) == 0 {
			// Not purchasable: hidden from the pricing response, with
			// no legacy stripe_price_id fallback.
			continue
		}
		out := gin.H{
			"id": p.ID, "name": p.Name, "slug": p.Slug,
			"license_type": p.LicenseType, "billing_interval": p.BillingInterval,
			"checkout_id": p.CheckoutID,
			"prices":      publicPlanPrices(rows),
			// True when /pay/<checkout_id> leads to the free sign-up
			// form rather than a checkout, so a pricing page can label
			// the button.
			"self_signup": p.SelfSignup && p.StripePriceID == "",
			// A plan sold per seat: price is per seat, and the buyer
			// picks between min_quantity and max_quantity at checkout
			// (/pay/<checkout_id>?quantity=N presets it).
			"per_seat": p.PerSeat,
		}
		if p.PerSeat {
			out["min_quantity"], out["max_quantity"] = p.MinQuantity, p.MaxQuantity
		}
		// The legacy top-level fields reflect the price for the plan's
		// own interval, so clients written against the old shape keep
		// working while they migrate to prices[].
		display := rows[0]
		for _, r := range rows {
			if r.BillingInterval == p.BillingInterval {
				display = r
				break
			}
		}
		out["price"], out["currency"] = display.AmountMinor, display.Currency
		active = append(active, out)
	}

	response.OK(c, gin.H{"plans": response.Array(active)})
}

// publicPlanPrices is the public projection of a plan's active rows:
// no row ids, plan ids, external price ids or timestamps.
func publicPlanPrices(rows []*model.PlanPrice) []gin.H {
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		out = append(out, gin.H{
			"provider":          r.Provider,
			"amount":            r.AmountMinor,
			"currency":          r.Currency,
			"currency_exponent": r.CurrencyExponent,
			"billing_interval":  r.BillingInterval,
		})
	}
	return out
}
