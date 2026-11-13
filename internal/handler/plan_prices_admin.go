package handler

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// PlanPriceAdminHandler is the admin editor for a plan's local prices.
// Phase 1a reads prices from plan_prices only, so a plan with no row
// here is not sellable.
type PlanPriceAdminHandler struct {
	Store *store.Store
}

// List returns every price row for a plan, active and retired, so the
// editor can show history while upserts only touch the active slot.
func (h *PlanPriceAdminHandler) List(c *gin.Context) {
	rows, err := h.Store.ListPlanPrices(c, c.Param("id"))
	if err != nil {
		response.Internal(c, err)
		return
	}
	response.OK(c, gin.H{"prices": response.Array(rows)})
}

type planPriceUpsertRequest struct {
	Provider         string `json:"provider"`
	Currency         string `json:"currency"`
	AmountMinor      int64  `json:"amount_minor"`
	CurrencyExponent int16  `json:"currency_exponent"`
	BillingInterval  string `json:"billing_interval"`
	ExternalPriceID  string `json:"external_price_id"`
}

// Upsert replaces the active price in its (provider, currency,
// interval) slot; the previous row is deactivated, not deleted.
func (h *PlanPriceAdminHandler) Upsert(c *gin.Context) {
	var req planPriceUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid JSON body")
		return
	}
	row, err := h.Store.UpsertPlanPrice(c, c.Param("id"), store.PlanPriceInput{
		Provider: req.Provider, Currency: req.Currency, AmountMinor: req.AmountMinor,
		CurrencyExponent: req.CurrencyExponent, BillingInterval: req.BillingInterval,
		ExternalPriceID: req.ExternalPriceID,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrPriceInvalid):
			response.BadRequest(c, err.Error())
		case errors.Is(err, sql.ErrNoRows):
			response.Err(c, http.StatusNotFound, "PLAN_NOT_FOUND", "plan not found")
		default:
			response.Internal(c, err)
		}
		return
	}
	response.OK(c, row)
}

// Delete deactivates one price row. The row stays for the order
// history that references it.
func (h *PlanPriceAdminHandler) Delete(c *gin.Context) {
	if err := h.Store.DeactivatePlanPrice(c, c.Param("id"), c.Param("price_id")); err != nil {
		response.Internal(c, err)
		return
	}
	response.NoContent(c)
}
