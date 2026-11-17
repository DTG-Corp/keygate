package handler

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/pkg/response"
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
	planID := c.Param("id")
	if _, err := h.Store.FindPlanByID(c, planID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			response.Err(c, http.StatusNotFound, "PLAN_NOT_FOUND", "plan not found")
			return
		}
		response.Internal(c, err)
		return
	}
	rows, err := h.Store.ListPlanPrices(c, planID)
	if err != nil {
		response.Internal(c, err)
		return
	}
	response.OK(c, gin.H{"prices": response.Array(rows)})
}

// planPriceUpsertRequest is the admin editor's body. AmountMinor is a
// pointer because the field is required: an omitted amount must not be
// read as a free price. CurrencyExponent is a pointer because the editor
// omits it for the currency's own exponent; an explicit value is checked
// against the currency.
type planPriceUpsertRequest struct {
	Provider         string `json:"provider"`
	Currency         string `json:"currency"`
	AmountMinor      *int64 `json:"amount_minor"`
	CurrencyExponent *int16 `json:"currency_exponent"`
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
	if req.AmountMinor == nil {
		response.Err(c, http.StatusBadRequest, "INVALID_AMOUNT", "amount_minor is required")
		return
	}
	// Derive the exponent when the editor omits it, so a USD/EUR price
	// gets its two minor-unit digits without the client repeating them;
	// an explicit value that disagrees is refused rather than rewritten.
	exponent := domain.CurrencyExponent(req.Currency)
	if req.CurrencyExponent != nil && *req.CurrencyExponent != exponent {
		response.Err(c, http.StatusBadRequest, "INVALID_CURRENCY_EXPONENT",
			"currency_exponent does not match currency")
		return
	}
	row, err := h.Store.UpsertPlanPrice(c, c.Param("id"), store.PlanPriceInput{
		Provider: req.Provider, Currency: req.Currency, AmountMinor: *req.AmountMinor,
		CurrencyExponent: exponent, BillingInterval: req.BillingInterval,
		ExternalPriceID: req.ExternalPriceID,
	})
	if err != nil {
		writePlanPriceError(c, err)
		return
	}
	response.OK(c, row)
}

// Delete deactivates one price row. The row stays for the order
// history that references it.
func (h *PlanPriceAdminHandler) Delete(c *gin.Context) {
	err := h.Store.DeactivatePlanPrice(c, c.Param("id"), c.Param("price_id"))
	switch {
	case err == nil:
		response.NoContent(c)
	case errors.Is(err, sql.ErrNoRows):
		response.Err(c, http.StatusNotFound, "PRICE_NOT_FOUND", "price not found")
	default:
		response.Internal(c, err)
	}
}

// writePlanPriceError maps the store's price errors onto the admin API:
// invalid input is the caller's to fix, a missing plan is a 404, and a
// lost slot race is a 409 it can retry.
func writePlanPriceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrPriceInvalid):
		response.BadRequest(c, err.Error())
	case errors.Is(err, sql.ErrNoRows):
		response.Err(c, http.StatusNotFound, "PLAN_NOT_FOUND", "plan not found")
	case errors.Is(err, store.ErrPriceSlotConflict):
		response.Err(c, http.StatusConflict, "PRICE_SLOT_CONFLICT",
			"another price in this slot was saved concurrently; retry")
	default:
		response.Internal(c, err)
	}
}
