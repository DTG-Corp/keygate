package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/providercfg"
	"github.com/tabloy/keygate/internal/payment/providers/payos"
	"github.com/tabloy/keygate/internal/payment/providers/sepay"
	"github.com/tabloy/keygate/internal/payment/settlement"
	"github.com/tabloy/keygate/internal/store"
)

// maxWebhookBody bounds one provider delivery at the spec's 256 KiB.
// SePay and PayOS payloads are a few KB; anything larger is a mistake or
// an attack, and it is refused before it is buffered whole or handed to
// a parser.
const maxWebhookBody = 256 << 10

// PaymentsWebhookHandler receives the SePay and PayOS webhook routes.
//
// Every handler reads the raw request body and passes those exact bytes
// to the adapter: a signature covers the provider's bytes, not Go's
// re-marshalling of the decoded struct. Before verification a small,
// untrusted peek names the order whose product selects the config row
// (and so the secret) that verifies the delivery; the verified event is
// what names the order the handler audits and settles, and the row that
// verified it must belong to that order's product (bindConfig).
type PaymentsWebhookHandler struct {
	Store      *store.Store
	Config     *providercfg.Service
	Settlement *settlement.Service
	Log        *slog.Logger
}

func (h *PaymentsWebhookHandler) logger() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func readRawBody(c *gin.Context) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxWebhookBody+1))
	if err != nil {
		c.Status(http.StatusBadRequest)
		return nil, false
	}
	if len(body) > maxWebhookBody {
		c.Status(http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return body, true
}

// ackJSON writes exactly the body SePay checks; any other body reads as
// a failed delivery and is retried for roughly half an hour. PayOS only
// requires a 2xx and ignores the body, so the same object answers it.
func ackJSON(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"success": true}) }

// ackEmpty is the hosted-gateway IPN answer: 200 and no body.
func ackEmpty(c *gin.Context) { c.Status(http.StatusOK) }

// peekReference reads an order reference out of a payload that has not
// been verified yet. It is used only to choose which product's config
// row verifies the signature, never to decide that money arrived: the
// verified event is what names the order the handler settles.
func peekReference(body []byte) (code string, numeric int64) {
	var v struct {
		Code        *string `json:"code"`
		Content     string  `json:"content"`
		Description string  `json:"description"`
		Data        struct {
			Description string `json:"description"`
			OrderCode   int64  `json:"orderCode"`
		} `json:"data"`
		Order struct {
			OrderInvoiceNumber string `json:"order_invoice_number"`
		} `json:"order"`
	}
	if json.Unmarshal(body, &v) != nil {
		return "", 0
	}
	if v.Code != nil {
		if code := strings.TrimSpace(*v.Code); domain.IsOrderCode(code) {
			return code, 0
		}
	}
	if code := strings.TrimSpace(v.Data.Description); domain.IsOrderCode(code) {
		return code, v.Data.OrderCode
	}
	if v.Data.OrderCode != 0 {
		return "", v.Data.OrderCode
	}
	if code := strings.TrimSpace(v.Order.OrderInvoiceNumber); domain.IsOrderCode(code) {
		return code, 0
	}
	if code := extractCodeFromText(v.Content); code != "" {
		return code, 0
	}
	return extractCodeFromText(v.Description), 0
}

// extractCodeFromText finds the first KG + 6 Crockford characters that
// stands as its own token, so "KG7F3K2Q ca phe" matches but
// "XKG7F3K2Q" does not.
func extractCodeFromText(s string) string {
	for i := 0; i+8 <= len(s); i++ {
		if i > 0 && isCodeTokenChar(s[i-1]) {
			continue
		}
		if code := s[i : i+8]; domain.IsOrderCode(code) {
			if i+8 == len(s) || !isCodeTokenChar(s[i+8]) {
				return code
			}
		}
	}
	return ""
}

func isCodeTokenChar(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func (h *PaymentsWebhookHandler) configFor(c *gin.Context, order *model.PaymentOrder, providerName string) (*providercfg.ResolvedConfig, error) {
	if h.Config == nil {
		return nil, providercfg.ErrProviderNotConfigured
	}
	productID := ""
	if order != nil {
		productID = order.ProductID
	}
	return h.Config.Resolve(c.Request.Context(), productID, providerName, "")
}

// findOrder looks an order up by the reference an unverified payload
// names. It answers 500 and returns ok=false only for a read fault: a
// lookup error must not be mistaken for "no such order", because that
// would send a known delivery to the unmatched queue instead of letting
// the provider retry a database problem.
func (h *PaymentsWebhookHandler) findOrder(c *gin.Context, code string, numeric int64) (*model.PaymentOrder, bool) {
	if code != "" {
		order, err := h.Store.GetPaymentOrderByCode(c.Request.Context(), code)
		switch {
		case err == nil:
			return order, true
		case !errors.Is(err, store.ErrOrderNotFound):
			h.logger().Error("webhook: read order by code", "code", code, "error", err)
			c.Status(http.StatusInternalServerError)
			return nil, false
		}
	}
	if numeric != 0 {
		order, err := h.Store.GetPaymentOrderByNumericCode(c.Request.Context(), numeric)
		switch {
		case err == nil:
			return order, true
		case !errors.Is(err, store.ErrOrderNotFound):
			h.logger().Error("webhook: read order by numeric code", "numeric_code", numeric, "error", err)
			c.Status(http.StatusInternalServerError)
			return nil, false
		}
	}
	return nil, true
}

// verifiedOrder finds the order the (now verified) event names. The
// pre-signature peek is only the fallback for an adapter that could not
// extract a reference the handler's looser peek did; the amount and
// currency check in settlement still guards the match. ok is false only
// when the lookup faulted and a 500 has been written.
func (h *PaymentsWebhookHandler) verifiedOrder(c *gin.Context, ev provider.WebhookEvent, peeked *model.PaymentOrder) (*model.PaymentOrder, bool) {
	if ev.OrderCode == "" && ev.NumericCode == 0 {
		return peeked, true
	}
	order, ok := h.findOrder(c, ev.OrderCode, ev.NumericCode)
	if !ok {
		return nil, false
	}
	if order != nil {
		return order, true
	}
	return peeked, true
}

// bindConfig proves the row that verified a delivery is the row the
// order's product uses. The unverified peek chooses the secret, so a
// signed body can name one product's order where the peek reads and
// another where the adapter reads. Without this check, product A's
// secret could settle product B's order via the install default, or a
// product row could settle another product's order outright.
func (h *PaymentsWebhookHandler) bindConfig(c *gin.Context, providerName string, order *model.PaymentOrder, cfg *providercfg.ResolvedConfig) bool {
	if order == nil || cfg == nil {
		return true
	}
	if !strings.EqualFold(cfg.Provider, providerName) {
		h.rejectBinding(c, providerName, order, cfg, "provider mismatch")
		return false
	}
	if cfg.ProductID == order.ProductID {
		return true
	}
	if cfg.ProductID != "" {
		h.rejectBinding(c, providerName, order, cfg, "another product's account verified the delivery")
		return false
	}
	// The install default verified it. That is right only while the
	// order's product has no row of its own; a product row must never be
	// bypassed by a default-secret delivery.
	if _, err := h.Config.Resolve(c.Request.Context(), order.ProductID, providerName, cfg.Mode); err == nil {
		h.rejectBinding(c, providerName, order, cfg, "the install default verified a product that has its own account")
		return false
	} else if !errors.Is(err, providercfg.ErrProviderNotConfigured) {
		// A disabled or unusable product row is not permission to fall
		// back to the default either.
		h.rejectBinding(c, providerName, order, cfg, "the order's product has an unusable account")
		return false
	}
	return true
}

func (h *PaymentsWebhookHandler) rejectBinding(c *gin.Context, providerName string, order *model.PaymentOrder, cfg *providercfg.ResolvedConfig, reason string) {
	h.logger().Error("webhook: verifying account does not match the order's product",
		"provider", providerName, "order", order.Code,
		"order_product", order.ProductID, "config_product", cfg.ProductID, "reason", reason)
	c.Status(http.StatusForbidden)
}

// scopedEventID is the id a verified delivery is recorded under. Two
// per-product accounts can see the same provider transfer id, so a
// product's own row scopes the id to that account; the install default
// keeps the provider's bare id. Scoping is applied to every delivery,
// including out transfers and unknown orders, and before the duplicate
// short-circuit.
func scopedEventID(cfg *providercfg.ResolvedConfig, ev provider.WebhookEvent) string {
	if cfg != nil && cfg.ProductID != "" {
		return cfg.ProductID + ":" + ev.ProviderEventID
	}
	return ev.ProviderEventID
}

// parseStatus maps an adapter error to the answer the provider gets. An
// authentication failure is 401; a body the adapter cannot decode is the
// sender's mistake (400), not a retryable fault.
func (h *PaymentsWebhookHandler) parseStatus(c *gin.Context, providerName string, err error) {
	h.logger().Warn("webhook: provider payload rejected", "provider", providerName, "error", err)
	if errors.Is(err, sepay.ErrWebhookUnauthorized) || errors.Is(err, payos.ErrWebhookUnauthorized) {
		c.Status(http.StatusUnauthorized)
		return
	}
	c.Status(http.StatusBadRequest)
}

// HandleSePay receives the SePay bank webhook: HMAC, apikey or (test
// mode only) unsigned deliveries of one bank transfer.
func (h *PaymentsWebhookHandler) HandleSePay(c *gin.Context) {
	body, ok := readRawBody(c)
	if !ok {
		return
	}
	code, numeric := peekReference(body)
	peeked, ok := h.findOrder(c, code, numeric)
	if !ok {
		return
	}
	cfg, err := h.configFor(c, peeked, "sepay")
	if err != nil {
		// That includes providercfg refusing a live row whose
		// webhook_auth_mode is "none": the handler relies on Resolve for
		// that rule and never parses an unsigned live body.
		h.logger().Error("sepay webhook: no usable provider config", "error", err)
		c.Status(http.StatusServiceUnavailable)
		return
	}
	ev, err := sepay.ParseBankWebhook(c.Request.Header, body, sepay.BankWebhookConfig{
		Mode: cfg.WebhookAuthMode, Secret: cfg.WebhookSecret, APIKey: cfg.APIToken,
	})
	if err != nil {
		h.parseStatus(c, "sepay", err)
		return
	}
	order, ok := h.verifiedOrder(c, ev, peeked)
	if !ok {
		return
	}
	if !h.bindConfig(c, "sepay", order, cfg) {
		return
	}
	// A refund or payout keeps its audit row and unmatched transaction,
	// but never settles an order from money leaving the account.
	h.ingest(c, "sepay", cfg, ev, order, ev.TransferType != "out", ackJSON)
}

// HandleSePayIPN receives the SePay hosted-gateway IPN. It shares the
// ingest path with the bank webhook but authenticates with X-Secret-Key
// and is acked with an empty 200.
func (h *PaymentsWebhookHandler) HandleSePayIPN(c *gin.Context) {
	body, ok := readRawBody(c)
	if !ok {
		return
	}
	code, numeric := peekReference(body)
	peeked, ok := h.findOrder(c, code, numeric)
	if !ok {
		return
	}
	cfg, err := h.configFor(c, peeked, "sepay")
	if err != nil {
		h.logger().Error("sepay ipn: no usable provider config", "error", err)
		c.Status(http.StatusServiceUnavailable)
		return
	}
	ev, err := sepay.ParseIPN(c.Request.Header, body, cfg.IPNSecret)
	if err != nil {
		h.parseStatus(c, "sepay-ipn", err)
		return
	}
	order, ok := h.verifiedOrder(c, ev, peeked)
	if !ok {
		return
	}
	if !h.bindConfig(c, "sepay", order, cfg) {
		return
	}
	h.ingest(c, "sepay", cfg, ev, order, true, ackEmpty)
}

// HandlePayOS receives a PayOS webhook. PayOS signs the data object, so
// the adapter reports a missing or empty checksum key as an
// authentication failure and the route fails closed.
func (h *PaymentsWebhookHandler) HandlePayOS(c *gin.Context) {
	body, ok := readRawBody(c)
	if !ok {
		return
	}
	code, numeric := peekReference(body)
	peeked, ok := h.findOrder(c, code, numeric)
	if !ok {
		return
	}
	cfg, err := h.configFor(c, peeked, "payos")
	if err != nil {
		h.logger().Error("payos webhook: no usable provider config", "error", err)
		c.Status(http.StatusServiceUnavailable)
		return
	}
	ev, err := payos.ParseWebhook(c.Request.Header, body, cfg.ChecksumKey)
	if err != nil {
		h.parseStatus(c, "payos", err)
		return
	}
	order, ok := h.verifiedOrder(c, ev, peeked)
	if !ok {
		return
	}
	if !h.bindConfig(c, "payos", order, cfg) {
		return
	}
	h.ingest(c, "payos", cfg, ev, order, true, ackJSON)
}

// ingest is the one audit and settlement entry point after
// verification. Every verified delivery is recorded in payment_events
// first, including the ones that will not settle (an unknown order, an
// outgoing transfer), so the audit trail covers everything the provider
// sent. The record is also the claim: a duplicate short-circuits to the
// retry rule below.
func (h *PaymentsWebhookHandler) ingest(
	c *gin.Context, providerName string, cfg *providercfg.ResolvedConfig,
	ev provider.WebhookEvent, order *model.PaymentOrder, settles bool, ack func(*gin.Context),
) {
	if h.Settlement == nil || h.Store == nil {
		h.logger().Error("webhook: settlement is not configured", "provider", providerName)
		c.Status(http.StatusServiceUnavailable)
		return
	}
	orderID := ""
	if order != nil {
		orderID = order.ID
	}
	inserted, err := h.Store.RecordPaymentEvent(c.Request.Context(), nil, store.PaymentEventInput{
		Provider: providerName, EventID: scopedEventID(cfg, ev), OrderID: orderID,
		Kind: ev.Status, Raw: ev.Raw, OccurredAt: ev.OccurredAt,
	})
	if err != nil {
		if errors.Is(err, store.ErrPaymentEventInvalid) {
			// An event with no id cannot dedupe and a retry would not
			// fix it: tell the provider the delivery is unusable.
			h.logger().Error("webhook: unusable payment event", "provider", providerName, "error", err)
			c.Status(http.StatusBadRequest)
			return
		}
		h.logger().Error("webhook: record payment event", "provider", providerName, "error", err)
		c.Status(http.StatusInternalServerError)
		return
	}
	if !inserted {
		if !settles {
			// A repeated out transfer or orderless delivery is already
			// on the unmatched ledger; ack it and change nothing.
			ack(c)
			return
		}
		h.retryPendingSettlement(c, providerName, ev, order, ack)
		return
	}
	// An unknown order is normal: a manual transfer with the code typed
	// wrong. Record it for the operator instead of 500-looping the
	// provider. An out transfer is recorded the same way and never
	// settles an order.
	if !settles || order == nil {
		if err := h.Settlement.RecordUnmatched(c.Request.Context(), providerName, ev, order); err != nil {
			h.logger().Error("webhook: record unmatched transaction", "provider", providerName, "error", err)
			c.Status(http.StatusInternalServerError)
			return
		}
		ack(c)
		return
	}
	if err := h.Settlement.Settle(c.Request.Context(), order.ID, providerName, ev); err != nil {
		// The claim is kept on purpose: the provider's retry reaches the
		// duplicate path, which settles the order again once the cause
		// is fixed.
		h.logger().Error("webhook: settle order", "order", order.Code, "error", err)
		c.Status(http.StatusInternalServerError)
		return
	}
	ack(c)
}

// retryPendingSettlement is the duplicate rule. A claim can exist while
// its order is still pending when the process died between the claim and
// settlement, or a request context was cancelled mid-settlement; the
// redelivery must settle rather than be acked as already handled.
// Settlement is idempotent, so an order that a concurrent delivery
// settled in the meantime is a no-op. A terminal order is only acked:
// its first delivery is the one that decided it.
func (h *PaymentsWebhookHandler) retryPendingSettlement(c *gin.Context, providerName string, ev provider.WebhookEvent, order *model.PaymentOrder, ack func(*gin.Context)) {
	if order == nil {
		ack(c)
		return
	}
	current, err := h.Store.GetPaymentOrderForUpdate(c.Request.Context(), nil, order.ID)
	if errors.Is(err, store.ErrOrderNotFound) {
		ack(c)
		return
	}
	if err != nil {
		h.logger().Error("webhook: reload order for a duplicate delivery", "order", order.Code, "error", err)
		c.Status(http.StatusInternalServerError)
		return
	}
	if current.Status != domain.OrderPending {
		ack(c)
		return
	}
	if err := h.Settlement.Settle(c.Request.Context(), current.ID, providerName, ev); err != nil {
		h.logger().Error("webhook: settle pending order on a duplicate delivery", "order", order.Code, "error", err)
		c.Status(http.StatusInternalServerError)
		return
	}
	ack(c)
}
