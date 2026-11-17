package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/checkout"
	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/payment/provider"
	"github.com/dtg-corp/dsoft/internal/payment/providercfg"
	"github.com/dtg-corp/dsoft/internal/payment/settlement"
	"github.com/dtg-corp/dsoft/internal/service"
	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/pkg/response"
)

// PaymentsAdminHandler is the operator console for the payment rail:
// the order ledger, manual settlement, refunds, the unmatched queue and
// the reconciliation health page. The routes are registered under the
// install-wide admin scope; nothing here is reachable by a merchant API
// key.
//
// Config and Providers are optional: a server built without a master key
// has no provider rows and registers the provider endpoints as
// unavailable instead of guessing at credentials.
type PaymentsAdminHandler struct {
	Store      *store.Store
	Settlement *settlement.Service
	Config     *providercfg.Service
	Providers  checkout.ProviderFactory
	// PayOSConfirmer registers this install's webhook URL with PayOS.
	// main injects the client so the credentials stay out of the handler.
	// productID is "" for the install-wide row, or the product whose
	// PayOS override supplies the credentials.
	PayOSConfirmer func(ctx context.Context, url, productID string) error
	// PublicBaseURL is the install's public origin, used to build the
	// PayOS webhook URL when the payment_webhook_base_url setting is not
	// set.
	PublicBaseURL string
	// Events announces order/payment transitions. Optional; a nil emitter
	// is a no-op so a server built without webhooks still works.
	Events *service.OrderEventEmitter
}

// ─── views ───

// adminOrderView is the order projection the console reads. Buyer and
// Metadata are only carried on the detail page: a list page does not
// need the checkout payload and should not ship it to every row.
type adminOrderView struct {
	ID                  string          `json:"id"`
	Code                string          `json:"code"`
	NumericCode         int64           `json:"numeric_code"`
	Status              string          `json:"status"`
	Kind                string          `json:"kind"`
	Provider            string          `json:"provider"`
	ProductID           string          `json:"product_id"`
	PlanID              string          `json:"plan_id"`
	PriceID             string          `json:"price_id,omitempty"`
	LicenseID           string          `json:"license_id,omitempty"`
	Email               string          `json:"email"`
	Quantity            int             `json:"quantity"`
	SubtotalMinor       int64           `json:"subtotal_minor"`
	DiscountMinor       int64           `json:"discount_minor"`
	TaxMinor            int64           `json:"tax_minor"`
	TotalMinor          int64           `json:"total_minor"`
	Currency            string          `json:"currency"`
	CurrencyExp         int16           `json:"currency_exponent"`
	ExpiresAt           time.Time       `json:"expires_at"`
	PaidAt              *time.Time      `json:"paid_at,omitempty"`
	PeriodStart         *time.Time      `json:"period_start,omitempty"`
	PeriodEnd           *time.Time      `json:"period_end,omitempty"`
	LastSettlementError string          `json:"last_settlement_error,omitempty"`
	Buyer               json.RawMessage `json:"buyer,omitempty"`
	Metadata            json.RawMessage `json:"metadata,omitempty"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

func adminOrderViewOf(o *model.PaymentOrder, detail bool) adminOrderView {
	v := adminOrderView{
		ID: o.ID, Code: o.Code, NumericCode: o.NumericCode, Status: o.Status, Kind: o.Kind,
		Provider: o.Provider, ProductID: o.ProductID, PlanID: o.PlanID, PriceID: o.PriceID,
		LicenseID: o.LicenseID, Email: o.Email, Quantity: o.Quantity,
		SubtotalMinor: o.SubtotalMinor, DiscountMinor: o.DiscountMinor, TaxMinor: o.TaxMinor,
		TotalMinor: o.TotalMinor, Currency: o.Currency, CurrencyExp: o.CurrencyExp,
		ExpiresAt: o.ExpiresAt, PaidAt: o.PaidAt, PeriodStart: o.PeriodStart, PeriodEnd: o.PeriodEnd,
		LastSettlementError: o.LastSettlementError, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
	}
	if detail {
		v.Buyer, v.Metadata = o.Buyer, o.Metadata
	}
	return v
}

// adminTransactionView is the transfer projection. Raw is only included
// when the caller asked for one row: a page of provider payloads is both
// large and noise for a list.
type adminTransactionView struct {
	ID              string          `json:"id"`
	Provider        string          `json:"provider"`
	ProviderRef     string          `json:"provider_ref"`
	OrderID         *string         `json:"order_id,omitempty"`
	AttemptID       *string         `json:"attempt_id,omitempty"`
	AmountMinor     int64           `json:"amount_minor"`
	Currency        string          `json:"currency"`
	Direction       string          `json:"direction"`
	ProviderAccount string          `json:"provider_account,omitempty"`
	Reference       string          `json:"reference,omitempty"`
	Content         string          `json:"content,omitempty"`
	Status          string          `json:"status"`
	IgnoreReason    string          `json:"ignore_reason,omitempty"`
	MatchedBy       *string         `json:"matched_by,omitempty"`
	MatchedAt       *time.Time      `json:"matched_at,omitempty"`
	Raw             json.RawMessage `json:"raw,omitempty"`
	OccurredAt      time.Time       `json:"occurred_at"`
	CreatedAt       time.Time       `json:"created_at"`
}

func adminTransactionViewOf(tx *model.PaymentTransaction, withRaw bool) adminTransactionView {
	v := adminTransactionView{
		ID: tx.ID, Provider: tx.Provider, ProviderRef: tx.ProviderRef, OrderID: tx.OrderID,
		AttemptID: tx.AttemptID, AmountMinor: tx.AmountMinor, Currency: strings.TrimSpace(tx.Currency),
		Direction: tx.Direction, ProviderAccount: tx.ProviderAccount, Reference: tx.Reference,
		Content: tx.Content, Status: tx.Status, IgnoreReason: tx.IgnoreReason,
		MatchedBy: tx.MatchedBy, MatchedAt: tx.MatchedAt, OccurredAt: tx.OccurredAt,
		CreatedAt: tx.CreatedAt,
	}
	if withRaw {
		v.Raw = tx.Raw
	}
	return v
}

// ─── orders ───

var orderStatuses = map[string]bool{
	domain.OrderPending: true, domain.OrderPaid: true, domain.OrderExpired: true,
	domain.OrderCanceled: true, domain.OrderRefunded: true, domain.OrderPartiallyRefunded: true,
}

var transactionStatuses = map[string]bool{
	domain.TxMatched: true, domain.TxUnmatched: true, domain.TxIgnored: true,
}

func (h *PaymentsAdminHandler) ListOrders(c *gin.Context) {
	page := listPage(c)
	status := strings.ToLower(strings.TrimSpace(c.Query("status")))
	if status != "" && !orderStatuses[status] {
		response.Err(c, http.StatusBadRequest, "INVALID_STATUS",
			"unknown order status, expected one of: pending, paid, expired, canceled, refunded, partially_refunded")
		return
	}
	orders, total, err := h.Store.ListPaymentOrders(c, store.OrderFilter{
		Status: status, Kind: c.Query("kind"), ProductID: c.Query("product_id"),
		Email: c.Query("email"), Provider: c.Query("provider"),
		Limit: page.Limit, Offset: page.Offset,
	})
	if err != nil {
		response.Internal(c, err)
		return
	}
	rows := make([]adminOrderView, 0, len(orders))
	for _, o := range orders {
		rows = append(rows, adminOrderViewOf(o, false))
	}
	listOK(c, "orders", rows, total, page)
}

func (h *PaymentsAdminHandler) GetOrder(c *gin.Context) {
	order, ok := h.order(c)
	if !ok {
		return
	}
	attempts, err := h.Store.ListPaymentAttemptsByOrder(c, order.ID)
	if err != nil {
		response.Internal(c, err)
		return
	}
	transactions, err := h.Store.ListPaymentTransactionsByOrder(c, order.ID)
	if err != nil {
		response.Internal(c, err)
		return
	}
	events, err := h.Store.ListPaymentEventsByOrder(c, order.ID)
	if err != nil {
		response.Internal(c, err)
		return
	}
	refunds, err := h.Store.ListPaymentRefundsByOrder(c, order.ID)
	if err != nil {
		response.Internal(c, err)
		return
	}
	txRows := make([]adminTransactionView, 0, len(transactions))
	for _, tx := range transactions {
		txRows = append(txRows, adminTransactionViewOf(tx, true))
	}
	refundRows := refunds
	if refundRows == nil {
		refundRows = []*model.PaymentRefund{}
	}
	response.OK(c, gin.H{
		"order":        adminOrderViewOf(order, true),
		"attempts":     attempts,
		"transactions": txRows,
		"events":       events,
		"refunds":      refundRows,
	})
}

// MarkPaid settles an order by hand — the bank transfer arrived and the
// webhook did not. It is refused on a live rail: a manual settlement
// grants a licence without money moving through a provider, and that is
// a rehearsal facility, not a production power.
func (h *PaymentsAdminHandler) MarkPaid(c *gin.Context) {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		response.BadRequest(c, "invalid JSON body")
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" {
		response.BadRequest(c, "reason is required for a manual settlement")
		return
	}
	order, ok := h.order(c)
	if !ok {
		return
	}
	// Settlement would silently treat a paid or closed order as a late
	// payment and answer success; the operator asked to settle, so say
	// that the order is not in a state where that is possible.
	if order.Status != domain.OrderPending {
		response.Err(c, http.StatusConflict, "PAYMENT_ORDER_NOT_PAYABLE", "order is already paid or closed")
		return
	}
	if !h.manualSettlementIsTestOnly(c, order) {
		return
	}
	raw, _ := json.Marshal(map[string]any{
		"source": "manual", "manual": true, "reason": req.Reason, "admin_id": adminID(c),
	})
	ev := provider.WebhookEvent{
		Provider: order.Provider, ProviderEventID: "manual-" + order.Code,
		ProviderRef: "manual-" + order.Code, OrderCode: order.Code,
		AmountMinor: order.TotalMinor, Currency: strings.TrimSpace(order.Currency),
		Status: domain.OrderPaid, TransferType: "in", OccurredAt: time.Now(), Raw: raw,
	}
	if err := h.Settlement.Settle(c, order.ID, order.Provider, ev); err != nil {
		writeAdminSettlementError(c, err)
		return
	}
	reloaded, err := h.Store.GetPaymentOrderByCode(c, order.Code)
	if err != nil {
		response.Internal(c, err)
		return
	}
	h.audit(c, order, "marked_paid", map[string]any{"reason": req.Reason, "provider": order.Provider})
	response.OK(c, gin.H{"order": adminOrderViewOf(reloaded, false)})
}

// CancelOrder cancels a pending order. A paid order is refused: the
// money is in and the licence must not be stranded by an operator click.
func (h *PaymentsAdminHandler) CancelOrder(c *gin.Context) {
	order, err := h.Store.CancelPaymentOrder(c, c.Param("code"))
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
	h.audit(c, order, "canceled", map[string]any{"status": order.Status})
	h.Events.OrderCanceled(c, order)
	response.OK(c, gin.H{"order": adminOrderViewOf(order, false)})
}

// RefundOrder records money returned against a paid order. Phase 1a
// refunds are manual: the operator transfers the money back and records
// the row, so a provider refund method is refused rather than recorded
// as if it had happened.
func (h *PaymentsAdminHandler) RefundOrder(c *gin.Context) {
	var req struct {
		AmountMinor int64  `json:"amount_minor"`
		Reason      string `json:"reason"`
		Method      string `json:"method"`
		ProviderRef string `json:"provider_ref"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid JSON body")
		return
	}
	method := strings.ToLower(strings.TrimSpace(req.Method))
	if method == "" {
		method = "manual"
	}
	if method != "manual" {
		response.Err(c, http.StatusUnprocessableEntity, "PAYMENT_REFUND_UNSUPPORTED",
			"only manual refunds are recorded in this release")
		return
	}
	order, ok := h.order(c)
	if !ok {
		return
	}
	if req.AmountMinor <= 0 {
		response.Err(c, http.StatusUnprocessableEntity, "PAYMENT_REFUND_INVALID",
			"amount_minor must be greater than zero")
		return
	}
	refund := &model.PaymentRefund{
		OrderID: order.ID, AmountMinor: req.AmountMinor, Currency: strings.TrimSpace(order.Currency),
		Reason: strings.TrimSpace(req.Reason), Method: method, ProviderRef: strings.TrimSpace(req.ProviderRef),
		// The FK-bearing column accepts a user id or NULL, not an
		// "apikey:<id>" principal. The audit line below keeps that id.
		CreatedBy: actorUserID(c),
	}
	if err := h.Store.RecordPaymentRefund(c, refund); err != nil {
		switch {
		case errors.Is(err, store.ErrOrderNotRefundable):
			response.Err(c, http.StatusConflict, "PAYMENT_ORDER_NOT_REFUNDABLE", "only a paid order can be refunded")
		case errors.Is(err, store.ErrPaymentRefundExceedsOrder):
			response.Err(c, http.StatusUnprocessableEntity, "PAYMENT_REFUND_EXCEEDS_ORDER",
				"the refund is larger than the order's remaining balance")
		case errors.Is(err, store.ErrPaymentRefundInvalid):
			response.Err(c, http.StatusUnprocessableEntity, "PAYMENT_REFUND_INVALID", err.Error())
		case errors.Is(err, store.ErrOrderNotFound):
			response.Err(c, http.StatusNotFound, "PAYMENT_ORDER_NOT_FOUND", "order not found")
		default:
			response.Internal(c, err)
		}
		return
	}
	reloaded, err := h.Store.GetPaymentOrderByCode(c, order.Code)
	if err != nil {
		response.Internal(c, err)
		return
	}
	h.audit(c, order, "refunded", map[string]any{
		"refund_id": refund.ID, "amount_minor": refund.AmountMinor,
		"method": refund.Method, "status": reloaded.Status, "actor": adminID(c),
	})
	h.Events.PaymentRefunded(c, reloaded, refund)
	response.OK(c, gin.H{"refund": refund, "order": adminOrderViewOf(reloaded, false)})
}

// ─── transactions and the unmatched queue ───

func (h *PaymentsAdminHandler) ListTransactions(c *gin.Context) {
	page := listPage(c)
	status := strings.ToLower(strings.TrimSpace(c.Query("status")))
	if status != "" && !transactionStatuses[status] {
		response.Err(c, http.StatusBadRequest, "INVALID_STATUS",
			"unknown transaction status, expected one of: matched, unmatched, ignored")
		return
	}
	rows, total, err := h.Store.ListPaymentTransactions(c, store.TransactionFilter{
		Provider: c.Query("provider"), Direction: c.Query("direction"), Status: status,
		ProductID: c.Query("product_id"), OrderID: c.Query("order_id"),
		Limit: page.Limit, Offset: page.Offset,
	})
	if err != nil {
		response.Internal(c, err)
		return
	}
	out := make([]adminTransactionView, 0, len(rows))
	for _, tx := range rows {
		out = append(out, adminTransactionViewOf(tx, false))
	}
	listOK(c, "transactions", out, total, page)
}

// ListUnmatched is the operator queue. It is not offset-paged: the queue
// is worked from the top, and a limit is all the console needs to bound
// the page.
func (h *PaymentsAdminHandler) ListUnmatched(c *gin.Context) {
	limit := queryInt(c, "limit", defaultListLimit)
	if limit < 1 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	rows, err := h.Store.ListUnmatchedTransactions(c, limit)
	if err != nil {
		response.Internal(c, err)
		return
	}
	out := make([]adminTransactionView, 0, len(rows))
	for _, tx := range rows {
		out = append(out, adminTransactionViewOf(tx, true))
	}
	response.OK(c, gin.H{"transactions": out, "count": len(out), "limit": limit})
}

// MatchTransaction links an unmatched bank transfer to the order it
// paid. It settles through the same service a webhook uses, so the
// grant, the transaction row and the order status are one decision; the
// amount and provider must agree exactly, which is the reason the money
// was in the queue in the first place.
func (h *PaymentsAdminHandler) MatchTransaction(c *gin.Context) {
	tx, ok := h.transaction(c)
	if !ok {
		return
	}
	if tx.Status != domain.TxUnmatched {
		response.Err(c, http.StatusConflict, "PAYMENT_TRANSACTION_NOT_UNMATCHED",
			"only an unmatched transaction can be linked to an order")
		return
	}
	if !strings.EqualFold(strings.TrimSpace(tx.Direction), "in") {
		// The worker stores payouts as direction=out. A payout is money
		// leaving the account; it can never pay for an order, whatever
		// its amount says.
		response.Err(c, http.StatusUnprocessableEntity, "PAYMENT_TRANSACTION_NOT_INCOMING",
			"only an incoming transfer can be matched to an order")
		return
	}
	var req struct {
		OrderCode string `json:"order_code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid JSON body")
		return
	}
	code := strings.ToUpper(strings.TrimSpace(req.OrderCode))
	if code == "" {
		response.BadRequest(c, "order_code is required")
		return
	}
	order, err := h.Store.GetPaymentOrderByCode(c, code)
	if errors.Is(err, store.ErrOrderNotFound) {
		response.Err(c, http.StatusNotFound, "PAYMENT_ORDER_NOT_FOUND", "order not found")
		return
	}
	if err != nil {
		response.Internal(c, err)
		return
	}
	if !strings.EqualFold(order.Provider, tx.Provider) {
		response.Err(c, http.StatusConflict, "PAYMENT_TRANSACTION_PROVIDER_MISMATCH",
			"the transfer and the order belong to different payment providers")
		return
	}
	if order.TotalMinor != tx.AmountMinor || !strings.EqualFold(strings.TrimSpace(order.Currency), strings.TrimSpace(tx.Currency)) {
		response.Err(c, http.StatusUnprocessableEntity, "PAYMENT_TRANSACTION_MISMATCH",
			"the transfer amount or currency does not match the order")
		return
	}
	if order.Status != domain.OrderPending {
		response.Err(c, http.StatusConflict, "PAYMENT_ORDER_NOT_PAYABLE", "order is already paid or closed")
		return
	}
	ev := provider.WebhookEvent{
		Provider: tx.Provider, ProviderEventID: "manual-match-" + tx.ID,
		ProviderRef: tx.ProviderRef, OrderCode: order.Code, AmountMinor: tx.AmountMinor,
		Currency: strings.TrimSpace(tx.Currency), Status: domain.OrderPaid,
		TransferType: "in", OccurredAt: tx.OccurredAt, Raw: tx.Raw,
	}
	if err := h.Settlement.Settle(c, order.ID, tx.Provider, ev); err != nil {
		writeAdminSettlementError(c, err)
		return
	}
	// Settlement already made the transfer row matched. Stamping who
	// asked for it is best effort: failing the request here would tell
	// the operator the match did not happen when the order is already
	// paid, so the attribution failure is logged instead.
	if err := h.Store.RecordTransactionMatchedBy(c, tx.ID, actorUserID(c)); err != nil {
		slog.Warn("manual match: settlement succeeded but the operator stamp failed",
			"transaction_id", tx.ID, "error", err)
	}
	reloadedOrder, err := h.Store.GetPaymentOrderByCode(c, order.Code)
	if err != nil {
		response.Internal(c, err)
		return
	}
	reloadedTx, err := h.Store.GetPaymentTransaction(c, tx.ID)
	if err != nil {
		response.Internal(c, err)
		return
	}
	h.audit(c, reloadedOrder, "transaction_matched", map[string]any{
		"transaction_id": tx.ID, "actor": adminID(c),
	})
	response.OK(c, gin.H{
		"transaction": adminTransactionViewOf(reloadedTx, true),
		"order":       adminOrderViewOf(reloadedOrder, false),
	})
}

func (h *PaymentsAdminHandler) IgnoreTransaction(c *gin.Context) {
	tx, ok := h.transaction(c)
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid JSON body")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		response.BadRequest(c, "reason is required to ignore a transaction")
		return
	}
	if err := h.Store.IgnoreTransaction(c, tx.ID, reason); err != nil {
		switch {
		case errors.Is(err, store.ErrTransactionNotFound):
			response.Err(c, http.StatusNotFound, "PAYMENT_TRANSACTION_NOT_FOUND", "transaction not found")
		case errors.Is(err, store.ErrTransactionNotUnmatched):
			response.Err(c, http.StatusConflict, "PAYMENT_TRANSACTION_NOT_UNMATCHED",
				"only an unmatched transaction can be ignored")
		default:
			response.Internal(c, err)
		}
		return
	}
	reloaded, err := h.Store.GetPaymentTransaction(c, tx.ID)
	if err != nil {
		response.Internal(c, err)
		return
	}
	h.audit(c, nil, "transaction_ignored", map[string]any{"transaction_id": tx.ID, "reason": reason})
	response.OK(c, gin.H{"transaction": adminTransactionViewOf(reloaded, true)})
}

// ─── health and provider operations ───

// adminProviderHealthView is one rail's row on the health page. Mode is
// filled only when every stored row agrees on one; a product override in
// test mode beside an install-wide live row has no single answer, and
// showing one of them would be a lie.
type adminProviderHealthView struct {
	Provider         string     `json:"provider"`
	Enabled          bool       `json:"enabled"`
	Mode             string     `json:"mode,omitempty"`
	LastEventAt      *time.Time `json:"last_event_at,omitempty"`
	UnmatchedOpen    int        `json:"unmatched_open"`
	PendingOrders    int        `json:"pending_orders"`
	LastError        string     `json:"last_error,omitempty"`
	LastReconciledAt *time.Time `json:"last_reconciled_at,omitempty"`
	CursorLagSeconds *int64     `json:"cursor_lag_seconds,omitempty"`
}

func (h *PaymentsAdminHandler) Health(c *gin.Context) {
	// The two live rails always appear, even before a row is saved: an
	// operator needs to see "not configured", not an empty page.
	views := map[string]*adminProviderHealthView{
		"sepay": {Provider: "sepay"},
		"payos": {Provider: "payos"},
	}
	if h.Config != nil {
		rows, err := h.Config.List(c)
		if err != nil {
			response.Internal(c, err)
			return
		}
		modes := map[string]map[string]bool{}
		for _, row := range rows {
			v, ok := views[row.Provider]
			if !ok {
				v = &adminProviderHealthView{Provider: row.Provider}
				views[row.Provider] = v
			}
			if row.Enabled {
				v.Enabled = true
			}
			if modes[row.Provider] == nil {
				modes[row.Provider] = map[string]bool{}
			}
			modes[row.Provider][row.Mode] = true
		}
		for name, set := range modes {
			if len(set) == 1 {
				for mode := range set {
					views[name].Mode = mode
				}
			}
		}
	}
	names := make([]string, 0, len(views))
	for name := range views {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*adminProviderHealthView, 0, len(names))
	for _, name := range names {
		v := views[name]
		health, err := h.Store.PaymentProviderHealth(c, name)
		if err != nil {
			response.Internal(c, err)
			return
		}
		v.LastEventAt = health.LastEventAt
		v.UnmatchedOpen = health.UnmatchedOpen
		v.PendingOrders = health.PendingOrders
		v.LastError = health.LastError
		v.LastReconciledAt = health.LastSyncedAt
		if health.LastSyncedAt != nil {
			lag := int64(time.Since(*health.LastSyncedAt).Seconds())
			if lag < 0 {
				lag = 0
			}
			v.CursorLagSeconds = &lag
		}
		out = append(out, v)
	}
	response.OK(c, gin.H{"providers": out})
}

// TestProvider asks the adapter to prove its credentials. A failing
// credential check is a result, not a transport error, so it answers 200
// with ok=false and the bounded error text; only a missing or unusable
// config row is a 4xx/5xx.
func (h *PaymentsAdminHandler) TestProvider(c *gin.Context) {
	name := strings.ToLower(strings.TrimSpace(c.Param("id")))
	if name != "sepay" && name != "payos" {
		response.Err(c, http.StatusBadRequest, "PAYMENT_PROVIDER_UNKNOWN", "unknown payment provider")
		return
	}
	if h.Config == nil || h.Providers == nil {
		response.Err(c, http.StatusServiceUnavailable, "PAYMENT_PROVIDER_DISABLED",
			"payment providers are not configured on this server")
		return
	}
	productID := strings.TrimSpace(c.Query("product_id"))
	cfg, err := h.Config.Resolve(c, productID, name, "")
	if err != nil {
		writeAdminProviderError(c, err)
		return
	}
	adapter, _, err := h.Providers(c, productID, name)
	if err != nil {
		writeAdminProviderError(c, err)
		return
	}
	testErr := adapter.TestConnection(c)
	view := gin.H{"provider": name, "mode": cfg.Mode, "ok": testErr == nil}
	if testErr != nil {
		view["error"] = clipForMessage(testErr.Error())
	}
	h.audit(c, nil, "provider_tested", map[string]any{"provider": name, "ok": testErr == nil})
	response.OK(c, view)
}

// ConfirmPayOSWebhook tells PayOS which URL to deliver to. The URL comes
// from the payment_webhook_base_url setting, or the install's public
// origin when the setting is empty. An optional product_id query scopes
// the confirmation to that product's PayOS row, for a product that has
// its own credentials and no install-wide row.
func (h *PaymentsAdminHandler) ConfirmPayOSWebhook(c *gin.Context) {
	if h.PayOSConfirmer == nil {
		response.Err(c, http.StatusServiceUnavailable, "PAYMENT_PROVIDER_DISABLED",
			"payment providers are not configured on this server")
		return
	}
	productID := strings.TrimSpace(c.Query("product_id"))
	if productID != "" {
		// Resolve falls back to the install-wide row, so a mistyped
		// product id would otherwise confirm against a product that does
		// not exist. Answer like the other admin product lookups.
		if _, err := h.Store.FindProductByID(c, productID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				response.NotFound(c, "product not found")
				return
			}
			response.Internal(c, err)
			return
		}
	}
	base := strings.TrimSpace(h.PublicBaseURL)
	if stored, err := h.Store.GetSetting(c, "payment_webhook_base_url"); err == nil && strings.TrimSpace(stored) != "" {
		base = strings.TrimSpace(stored)
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		response.Internal(c, err)
		return
	}
	if base == "" {
		response.Err(c, http.StatusBadRequest, "PAYMENT_WEBHOOK_URL_UNSET",
			"payment_webhook_base_url is not set and the install has no public base URL")
		return
	}
	webhookURL := strings.TrimRight(base, "/") + "/api/v1/webhook/payos"
	if err := h.PayOSConfirmer(c, webhookURL, productID); err != nil {
		response.Err(c, http.StatusBadGateway, "PAYMENT_PROVIDER_ERROR",
			"PayOS rejected the webhook confirmation: "+clipForMessage(err.Error()))
		return
	}
	changes := map[string]any{"url": webhookURL}
	if productID != "" {
		changes["product_id"] = productID
	}
	h.audit(c, nil, "payos_webhook_confirmed", changes)
	response.OK(c, gin.H{"provider": "payos", "webhook_url": webhookURL, "confirmed": true})
}

// ─── helpers ───

func (h *PaymentsAdminHandler) order(c *gin.Context) (*model.PaymentOrder, bool) {
	order, err := h.Store.GetPaymentOrderByCode(c, c.Param("code"))
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

func (h *PaymentsAdminHandler) transaction(c *gin.Context) (*model.PaymentTransaction, bool) {
	tx, err := h.Store.GetPaymentTransaction(c, c.Param("id"))
	if errors.Is(err, store.ErrTransactionNotFound) {
		response.Err(c, http.StatusNotFound, "PAYMENT_TRANSACTION_NOT_FOUND", "transaction not found")
		return nil, false
	}
	if err != nil {
		response.Internal(c, err)
		return nil, false
	}
	return tx, true
}

// manualSettlementIsTestOnly is mark-paid's fail-closed guard. The order
// does not store the provider mode it was created under, and resolving
// with the server's current default can miss a product override. So a
// live row for this order's rail (install-wide or its product's override)
// refuses the operation outright; a config that cannot be resolved
// refuses too. Only a resolvable test configuration allows a licence to
// be granted without money moving through a provider. A handler built
// without a config service has no rail to check, so it refuses as well:
// an unverifiable rail is not a test rail.
func (h *PaymentsAdminHandler) manualSettlementIsTestOnly(c *gin.Context, order *model.PaymentOrder) bool {
	if h.Config == nil {
		response.Err(c, http.StatusUnprocessableEntity, "PAYMENT_MANUAL_SETTLEMENT_UNVERIFIABLE",
			"manual settlement needs a provider configuration service")
		return false
	}
	rows, err := h.Config.List(c)
	if err != nil {
		response.Internal(c, err)
		return false
	}
	providerName := strings.ToLower(strings.TrimSpace(order.Provider))
	for _, row := range rows {
		if !row.Enabled || !strings.EqualFold(row.Provider, providerName) {
			continue
		}
		// An install-wide row covers every product; a product row only
		// covers its own.
		if row.ProductID != "" && row.ProductID != order.ProductID {
			continue
		}
		if row.Mode == "live" {
			response.Err(c, http.StatusConflict, "PAYMENT_MANUAL_SETTLEMENT_FORBIDDEN",
				"manual settlement is disabled: a live payment provider is configured for this order")
			return false
		}
	}
	cfg, err := h.Config.Resolve(c, order.ProductID, order.Provider, "")
	if err != nil {
		// Disabled, deleted, broken or missing: an unverifiable rail is
		// not a test rail, and manual settlement is refused rather than
		// granting a licence on a config the operator cannot see.
		response.Err(c, http.StatusUnprocessableEntity, "PAYMENT_MANUAL_SETTLEMENT_UNVERIFIABLE",
			"manual settlement needs a usable test payment provider configuration for this order")
		return false
	}
	if cfg.Mode != "test" {
		response.Err(c, http.StatusConflict, "PAYMENT_MANUAL_SETTLEMENT_FORBIDDEN",
			"manual settlement is disabled for a live payment provider")
		return false
	}
	return true
}

// actorUserID is the signed-in user behind an operator action, or "" for
// an API key. payment_refunds.created_by and
// payment_transactions.matched_by reference users(id); an API key's
// "apikey:<id>" principal is not a user id, and passing it would fail the
// write after the money decision was already made. The audit event keeps
// the full principal in its payload either way.
func actorUserID(c *gin.Context) string {
	id := adminID(c)
	if _, err := uuid.Parse(id); err != nil {
		return ""
	}
	return id
}

// writeAdminSettlementError maps the settlement sentinels the admin
// operations can hit. A stale order state is a conflict; anything else
// is a server fault and stays a 500.
func writeAdminSettlementError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrOrderNotFound):
		response.Err(c, http.StatusNotFound, "PAYMENT_ORDER_NOT_FOUND", "order not found")
	case errors.Is(err, store.ErrOrderNotPayable):
		response.Err(c, http.StatusConflict, "PAYMENT_ORDER_NOT_PAYABLE", "order is already paid or closed")
	default:
		response.Internal(c, err)
	}
}

func writeAdminProviderError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, providercfg.ErrProviderUnknown):
		response.Err(c, http.StatusBadRequest, "PAYMENT_PROVIDER_UNKNOWN", "unknown payment provider")
	case errors.Is(err, providercfg.ErrProviderConfigInvalid):
		response.Err(c, http.StatusBadRequest, "PAYMENT_PROVIDER_CONFIG_INVALID", "invalid payment provider configuration")
	case errors.Is(err, providercfg.ErrProviderNotFound), errors.Is(err, providercfg.ErrProviderNotConfigured):
		response.Err(c, http.StatusNotFound, "PAYMENT_PROVIDER_NOT_FOUND", "payment provider config not found")
	case errors.Is(err, providercfg.ErrProviderDisabled):
		response.Err(c, http.StatusServiceUnavailable, "PAYMENT_PROVIDER_DISABLED", "payment provider is disabled")
	default:
		response.Internal(c, err)
	}
}

// audit records an operator action; the handler is usable without a
// store audit sink (nil Store only happens in a broken wiring, and the
// store is required for everything above).
func (h *PaymentsAdminHandler) audit(c *gin.Context, order *model.PaymentOrder, action string, changes map[string]any) {
	if h.Store == nil {
		return
	}
	entry := &model.AuditLog{
		Entity: "payment_order", Action: action, ActorType: "admin",
		ActorID: adminID(c), IPAddress: c.ClientIP(), Changes: changes,
	}
	if order != nil {
		entry.EntityID, entry.ProductID = order.ID, order.ProductID
	}
	h.Store.Audit(c, entry)
}
