package handler

import (
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/uptrace/bun"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/checkout"
	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/pkg/response"
)

// CouponsAdminHandler owns the coupon catalogue: definitions, their
// scopes, and the redemption ledger marketing reads.
//
// Checkout is used only by the preview route, which prices codes the way a
// real order would, so an operator cannot promise a discount the checkout
// page would refuse.
type CouponsAdminHandler struct {
	Store    *store.Store
	Checkout *checkout.Service
}

// RegisterCouponsAdminRoutes mounts the coupon admin routes on the admin
// group. The Lead calls this from the server wiring: the group already
// carries the admin scope and API-key middleware, so nothing here repeats
// them.
func RegisterCouponsAdminRoutes(g *gin.RouterGroup, h *CouponsAdminHandler) {
	g.GET("/coupons", h.List)
	g.POST("/coupons", h.Create)
	// Registered before the :id routes for readability; Gin keeps the
	// method trees separate, so a static POST and a param GET coexist.
	g.POST("/coupons/preview", h.Preview)
	g.GET("/coupons/:id", h.Get)
	g.PUT("/coupons/:id", h.Update)
	g.DELETE("/coupons/:id", h.Delete)
	g.GET("/coupons/:id/redemptions", h.Redemptions)
	g.GET("/payments/redemptions", h.RedemptionsGlobal)
}

// couponScopeRequest is one inline scope on a coupon write.
type couponScopeRequest struct {
	ScopeType string `json:"scope_type"`
	ScopeID   string `json:"scope_id"`
}

// couponRequest is a coupon definition as the admin form sends it. The
// booleans and counters are pointers so an omitted field takes the
// documented default (stackable, priority 100, active) instead of Go's
// zero value.
type couponRequest struct {
	Code             string               `json:"code"`
	Description      string               `json:"description"`
	Type             string               `json:"type"`
	Value            int64                `json:"value"`
	TrialDays        int                  `json:"trial_days"`
	Currency         string               `json:"currency"`
	StackGroup       string               `json:"stack_group"`
	Stackable        *bool                `json:"stackable"`
	Priority         *int                 `json:"priority"`
	AssignedEmail    string               `json:"assigned_email"`
	MaxRedemptions   *int                 `json:"max_redemptions"`
	PerCustomerLimit *int                 `json:"per_customer_limit"`
	ValidFrom        *time.Time           `json:"valid_from"`
	ValidTo          *time.Time           `json:"valid_to"`
	Active           *bool                `json:"active"`
	Scopes           []couponScopeRequest `json:"scopes"`
}

// toInput maps the request onto the store's definition, keeping code when
// the form left it out (an update may only change the limits).
func (r couponRequest) toInput(code string) store.CouponInput {
	if strings.TrimSpace(r.Code) != "" {
		code = r.Code
	}
	in := store.CouponInput{
		Code: code, Description: r.Description,
		Type: strings.ToLower(strings.TrimSpace(r.Type)), Value: r.Value,
		TrialDays: r.TrialDays, Currency: r.Currency,
		StackGroup: strings.ToLower(strings.TrimSpace(r.StackGroup)),
		Stackable:  true, Priority: 100, Active: true,
		AssignedEmail: r.AssignedEmail, ValidFrom: r.ValidFrom, ValidTo: r.ValidTo,
	}
	if r.Stackable != nil {
		in.Stackable = *r.Stackable
	}
	if r.Priority != nil {
		in.Priority = *r.Priority
	}
	if r.Active != nil {
		in.Active = *r.Active
	}
	if r.MaxRedemptions != nil {
		in.MaxRedemptions = *r.MaxRedemptions
	}
	if r.PerCustomerLimit != nil {
		in.PerCustomerLimit = *r.PerCustomerLimit
	}
	for _, sc := range r.Scopes {
		in.Scopes = append(in.Scopes, store.CouponScopeInput{ScopeType: sc.ScopeType, ScopeID: sc.ScopeID})
	}
	return in
}

// List answers GET /admin/coupons with filters q, active, stack_group,
// scope_type and scope_id.
func (h *CouponsAdminHandler) List(c *gin.Context) {
	filter := store.CouponFilter{
		Q:          strings.TrimSpace(c.Query("q")),
		StackGroup: strings.ToLower(strings.TrimSpace(c.Query("stack_group"))),
		ScopeType:  strings.ToLower(strings.TrimSpace(c.Query("scope_type"))),
		ScopeID:    strings.TrimSpace(c.Query("scope_id")),
		Page:       listPage(c),
	}
	if raw := strings.TrimSpace(c.Query("active")); raw != "" {
		active, err := strconv.ParseBool(raw)
		if err != nil {
			response.BadRequest(c, "active must be a boolean")
			return
		}
		filter.Active = &active
	}
	if filter.StackGroup != "" && filter.StackGroup != "general" &&
		filter.StackGroup != "campaign" && filter.StackGroup != "private" {
		response.BadRequest(c, "stack_group must be general, campaign or private")
		return
	}
	couponsPage, total, err := h.Store.ListCoupons(c, filter)
	if err != nil {
		response.Internal(c, err)
		return
	}
	scopes, err := h.Store.CouponScopesFor(c, couponIDs(couponsPage))
	if err != nil {
		response.Internal(c, err)
		return
	}
	rows := make([]gin.H, 0, len(couponsPage))
	for _, cp := range couponsPage {
		rows = append(rows, h.couponView(c, cp, scopes[cp.ID]))
	}
	listOK(c, "data", rows, total, filter.Page)
}

// Get answers GET /admin/coupons/:id including the redemption counters.
func (h *CouponsAdminHandler) Get(c *gin.Context) {
	cp, ok := h.coupon(c)
	if !ok {
		return
	}
	scopes, err := h.Store.CouponScopesFor(c, []string{cp.ID})
	if err != nil {
		response.Internal(c, err)
		return
	}
	body := h.couponView(c, cp, scopes[cp.ID])
	stats, err := h.Store.CouponRedemptionStats(c, cp.ID)
	if err != nil {
		response.Internal(c, err)
		return
	}
	body["redemptions"] = gin.H{"count": stats.Count, "amount_minor": stats.AmountMinor}
	response.OK(c, body)
}

// Create answers POST /admin/coupons.
func (h *CouponsAdminHandler) Create(c *gin.Context) {
	var req couponRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid JSON body")
		return
	}
	cp, err := h.Store.CreateCoupon(c, req.toInput(""))
	if err != nil {
		couponWriteError(c, err)
		return
	}
	h.audit(c, cp, "create", req)
	h.respondWithCoupon(c, http.StatusCreated, cp)
}

// Update answers PUT /admin/coupons/:id. Scopes are replaced atomically by
// the store, so a failed write leaves both the row and its scopes as they
// were.
func (h *CouponsAdminHandler) Update(c *gin.Context) {
	existing, ok := h.coupon(c)
	if !ok {
		return
	}
	var req couponRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid JSON body")
		return
	}
	cp, err := h.Store.UpdateCoupon(c, existing.ID, req.toInput(existing.Code))
	if err != nil {
		couponWriteError(c, err)
		return
	}
	h.audit(c, cp, "update", req)
	h.respondWithCoupon(c, http.StatusOK, cp)
}

// Delete answers DELETE /admin/coupons/:id. A coupon that was ever
// redeemed cannot be deleted without orphaning the ledger, so it is
// archived instead; the response says which happened.
func (h *CouponsAdminHandler) Delete(c *gin.Context) {
	existing, ok := h.coupon(c)
	if !ok {
		return
	}
	archived, err := h.Store.DeleteCoupon(c, existing.ID)
	if err != nil {
		couponWriteError(c, err)
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "coupon", EntityID: existing.ID, Action: "delete",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{"code": existing.Code, "archived": archived},
	})
	response.OK(c, gin.H{"id": existing.ID, "code": existing.Code, "deleted": true, "archived": archived})
}

// previewRequest is one coupon preview: the codes an operator wants to
// check against a plan before publishing them.
type previewRequest struct {
	PlanID             string   `json:"plan_id"`
	Provider           string   `json:"provider,omitempty"`
	Quantity           int      `json:"quantity,omitempty"`
	Codes              []string `json:"codes,omitempty"`
	IgnoreInvalid      bool     `json:"ignore_invalid,omitempty"`
	Email              string   `json:"email,omitempty"`
	ExternalCustomerID string   `json:"external_customer_id,omitempty"`
}

// Preview answers POST /admin/coupons/preview with exactly what checkout
// would charge for these codes: the same resolver, the same quote.
func (h *CouponsAdminHandler) Preview(c *gin.Context) {
	if h.Checkout == nil {
		response.Internal(c, errors.New("coupon preview is not wired"))
		return
	}
	var req previewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.PlanID) == "" {
		response.BadRequest(c, "plan_id is required")
		return
	}
	res, err := h.Checkout.PreviewQuote(c, checkout.PreviewInput{
		PlanID: req.PlanID, Provider: req.Provider, Quantity: req.Quantity,
		Codes: req.Codes, IgnoreInvalid: req.IgnoreInvalid,
		Email: req.Email, ExternalCustomerID: req.ExternalCustomerID,
	})
	if err != nil {
		writeCheckoutError(c, err)
		return
	}
	response.OK(c, gin.H{
		"plan":    gin.H{"id": res.Plan.ID, "name": res.Plan.Name, "slug": res.Plan.Slug, "license_type": res.Plan.LicenseType},
		"product": gin.H{"id": res.Product.ID, "name": res.Product.Name, "slug": res.Product.Slug},
		"price": gin.H{
			"provider": res.Price.Provider, "currency": res.Price.Currency,
			"amount_minor": res.Price.AmountMinor, "billing_interval": res.Price.BillingInterval,
		},
		"quote": gin.H{
			"subtotal_minor": res.Quote.SubtotalMinor, "discount_minor": res.Quote.DiscountMinor,
			"tax_minor": res.Quote.TaxMinor, "total_minor": res.Quote.TotalMinor,
			"trial_days": res.Quote.TrialDays, "discounts": res.Quote.Discounts,
		},
	})
}

// Redemptions answers GET /admin/coupons/:id/redemptions.
func (h *CouponsAdminHandler) Redemptions(c *gin.Context) {
	cp, ok := h.coupon(c)
	if !ok {
		return
	}
	page := listPage(c)
	rows, total, err := h.Store.RedemptionsForCoupon(c, cp.ID, page)
	if err != nil {
		response.Internal(c, err)
		return
	}
	listOK(c, "data", redemptionViews(rows), total, page)
}

// RedemptionsGlobal answers GET /admin/payments/redemptions with filters
// coupon, email, from and to, and a CSV export when ?format=csv is asked
// for.
func (h *CouponsAdminHandler) RedemptionsGlobal(c *gin.Context) {
	filter, ok := h.redemptionFilter(c)
	if !ok {
		return
	}
	if strings.EqualFold(strings.TrimSpace(c.Query("format")), "csv") {
		h.exportRedemptions(c, filter)
		return
	}
	page := listPage(c)
	filter.Page = page
	rows, total, err := h.Store.ListRedemptions(c, filter)
	if err != nil {
		response.Internal(c, err)
		return
	}
	listOK(c, "data", redemptionViews(rows), total, page)
}

// redemptionFilter reads the shared filter set. Dates accept an RFC 3339
// timestamp or a plain date; a plain `to` covers that whole day.
func (h *CouponsAdminHandler) redemptionFilter(c *gin.Context) (store.RedemptionFilter, bool) {
	f := store.RedemptionFilter{
		CouponID:   strings.TrimSpace(c.Query("coupon_id")),
		CouponCode: strings.TrimSpace(c.Query("coupon")),
		Email:      strings.TrimSpace(c.Query("email")),
	}
	from, err := parseRedemptionBound(c.Query("from"), false)
	if err != nil {
		response.BadRequest(c, "from must be an RFC 3339 timestamp or a date (YYYY-MM-DD)")
		return f, false
	}
	to, err := parseRedemptionBound(c.Query("to"), true)
	if err != nil {
		response.BadRequest(c, "to must be an RFC 3339 timestamp or a date (YYYY-MM-DD)")
		return f, false
	}
	f.From, f.To = from, to
	return f, true
}

// parseRedemptionBound reads one date bound. A date-only upper bound means
// the whole day, so `from=2026-11-01&to=2026-11-01` is not empty.
func parseRedemptionBound(raw string, upper bool) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return &ts, nil
	}
	day, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return nil, err
	}
	if upper {
		day = day.Add(24 * time.Hour)
	}
	return &day, nil
}

// exportRedemptions streams the filtered ledger as CSV. The header and
// column order are part of the contract with the finance spreadsheet, and
// cells go through csvSafeRow because an email is user-controlled.
func (h *CouponsAdminHandler) exportRedemptions(c *gin.Context, filter store.RedemptionFilter) {
	filter.Page = store.All
	rows, _, err := h.Store.ListRedemptions(c, filter)
	if err != nil {
		response.Internal(c, err)
		return
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "coupon_redemption", EntityID: "export", Action: "export",
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		Changes: map[string]any{
			"coupon": filter.CouponCode, "coupon_id": filter.CouponID,
			"email": filter.Email, "count": len(rows),
		},
	})
	c.Header("Content-Type", "text/csv")
	c.Header("Content-Disposition",
		fmt.Sprintf("attachment; filename=redemptions-%s.csv", time.Now().Format("20060102")))
	w := csv.NewWriter(c.Writer)
	defer w.Flush()
	_ = w.Write([]string{
		"created_at", "coupon_code", "order_code", "email", "external_customer_id",
		"type", "amount_minor", "trial_days", "currency",
	})
	for _, row := range rows {
		_ = w.Write(csvSafeRow([]string{
			row.CreatedAt.Format(time.RFC3339), row.CouponCode, row.OrderCode, row.Email,
			row.ExternalCustomerID, row.Type, strconv.FormatInt(row.AmountMinor, 10),
			strconv.Itoa(row.TrialDays), row.Currency,
		}))
	}
}

// coupon reads the :id coupon or writes the 404.
func (h *CouponsAdminHandler) coupon(c *gin.Context) (*model.Coupon, bool) {
	cp, err := h.Store.GetCoupon(c, c.Param("id"))
	if errors.Is(err, store.ErrCouponNotFound) {
		response.NotFound(c, "coupon not found")
		return nil, false
	}
	if err != nil {
		response.Internal(c, err)
		return nil, false
	}
	return cp, true
}

// respondWithCoupon writes one coupon with its scopes.
func (h *CouponsAdminHandler) respondWithCoupon(c *gin.Context, status int, cp *model.Coupon) {
	scopes, err := h.Store.CouponScopesFor(c, []string{cp.ID})
	if err != nil {
		response.Internal(c, err)
		return
	}
	body := h.couponView(c, cp, scopes[cp.ID])
	if status == http.StatusCreated {
		response.Created(c, body)
		return
	}
	response.OK(c, body)
}

// couponView is the admin representation of a coupon. The redemption
// counter is the stored one; Get adds the ledger totals beside it.
func (h *CouponsAdminHandler) couponView(c *gin.Context, cp *model.Coupon, scopes []*model.CouponScope) gin.H {
	return gin.H{
		"id": cp.ID, "code": cp.Code, "description": cp.Description,
		"type": cp.Type, "value": cp.Value, "trial_days": cp.TrialDays,
		"currency": cp.Currency, "stack_group": cp.StackGroup,
		"stackable": cp.Stackable, "priority": cp.Priority,
		"assigned_email":  cp.AssignedEmail,
		"max_redemptions": cp.MaxRedemptions, "redeemed_count": cp.RedeemedCount,
		"per_customer_limit": cp.PerCustomerLimit,
		"valid_from":         cp.ValidFrom, "valid_to": cp.ValidTo,
		"active": cp.Active, "created_at": cp.CreatedAt, "updated_at": cp.UpdatedAt,
		"scopes": h.scopeViews(c, scopes),
	}
}

// scopeViews renders a coupon's scope rows with the names behind the ids,
// so an operator reads "DSoft Pro" rather than a UUID.
func (h *CouponsAdminHandler) scopeViews(c *gin.Context, scopes []*model.CouponScope) []gin.H {
	rows := make([]gin.H, 0, len(scopes))
	if len(scopes) == 0 {
		return rows
	}
	labels := h.scopeLabels(c, scopes)
	for _, sc := range scopes {
		row := gin.H{"scope_type": sc.ScopeType, "scope_id": sc.ScopeID}
		if label := labels[sc.ScopeType+":"+sc.ScopeID]; label != "" {
			row["name"] = label
		}
		rows = append(rows, row)
	}
	return rows
}

// scopeLabels resolves scope target names in two queries for a whole page.
// A name that cannot be read is simply absent: the label is decoration,
// the id is the fact.
func (h *CouponsAdminHandler) scopeLabels(c *gin.Context, scopes []*model.CouponScope) map[string]string {
	labels := make(map[string]string, len(scopes))
	ids := map[string][]string{}
	for _, sc := range scopes {
		ids[sc.ScopeType] = append(ids[sc.ScopeType], sc.ScopeID)
	}
	type named struct {
		ID   string `bun:"id"`
		Name string `bun:"name"`
	}
	for scopeType, table := range map[string]string{"product": "products", "plan": "plans"} {
		if len(ids[scopeType]) == 0 {
			continue
		}
		var rows []named
		if err := h.Store.DB.NewSelect().TableExpr(table).Column("id", "name").
			Where("id IN (?)", bun.In(ids[scopeType])).Scan(c, &rows); err != nil {
			continue
		}
		for _, r := range rows {
			labels[scopeType+":"+r.ID] = r.Name
		}
	}
	return labels
}

// audit records one coupon write. The definition itself is the change:
// the admin log is where "who made it free" is answered.
func (h *CouponsAdminHandler) audit(c *gin.Context, cp *model.Coupon, action string, req couponRequest) {
	changes := map[string]any{
		"code": cp.Code, "type": cp.Type, "value": cp.Value, "trial_days": cp.TrialDays,
		"currency": cp.Currency, "stack_group": cp.StackGroup, "stackable": cp.Stackable,
		"priority": cp.Priority, "active": cp.Active, "scopes": len(req.Scopes),
	}
	h.Store.Audit(c, &model.AuditLog{
		Entity: "coupon", EntityID: cp.ID, Action: action,
		ActorType: "admin", ActorID: adminID(c), IPAddress: c.ClientIP(),
		Changes: changes,
	})
}

// couponIDs is the id list a batch scope read needs.
func couponIDs(list []*model.Coupon) []string {
	ids := make([]string, 0, len(list))
	for _, cp := range list {
		ids = append(ids, cp.ID)
	}
	return ids
}

// redemptionViews is the response view of the redemption ledger.
func redemptionViews(rows []store.RedemptionRow) []gin.H {
	out := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		out = append(out, gin.H{
			"id": row.ID, "coupon_id": row.CouponID, "coupon_code": row.CouponCode,
			"order_id": row.OrderID, "order_code": row.OrderCode,
			"email": row.Email, "external_customer_id": row.ExternalCustomerID,
			"type": row.Type, "amount_minor": row.AmountMinor, "trial_days": row.TrialDays,
			"currency": row.Currency, "currency_exponent": row.CurrencyExp,
			"created_at": row.CreatedAt,
		})
	}
	return out
}

// couponWriteError maps a store refusal onto the documented admin answer.
func couponWriteError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrCouponNotFound):
		response.NotFound(c, "coupon not found")
	case errors.Is(err, store.ErrCouponCodeTaken):
		response.Err(c, http.StatusConflict, "PAYMENT_COUPON_CODE_TAKEN", err.Error())
	case errors.Is(err, store.ErrCouponInvalid):
		response.Err(c, http.StatusUnprocessableEntity, "PAYMENT_COUPON_INVALID", err.Error())
	default:
		response.Internal(c, err)
	}
}
