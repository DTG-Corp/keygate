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

// maxWebhookBody bounds one provider delivery. SePay and PayOS payloads
// are a few KB; anything larger is a mistake or an attack, and it is
// refused before it is buffered whole or handed to a parser.
const maxWebhookBody = 1 << 20

// PaymentsWebhookHandler receives the SePay and PayOS webhook routes.
//
// Every handler reads the raw request body and passes those exact bytes
// to the adapter: a signature covers the provider's bytes, not Go's
// re-marshalling of the decoded struct. Only after the adapter has
// verified and normalised the delivery does the handler look for the
// order and settle it.
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

// liveUnsignedWebhook reports a config that would accept unsigned
// deliveries in live mode. The admin write path already refuses the
// combination; this guard covers rows written before that rule, because
// a live webhook that accepts an unsigned body is fail-open: anyone who
// learns the URL could mark an order paid.
func liveUnsignedWebhook(cfg *providercfg.ResolvedConfig) bool {
	return cfg != nil && cfg.Mode == "live" &&
		strings.EqualFold(strings.TrimSpace(cfg.WebhookAuthMode), "none")
}

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

func (h *PaymentsWebhookHandler) findOrder(c *gin.Context, code string, numeric int64) *model.PaymentOrder {
	if code != "" {
		if o, err := h.Store.GetPaymentOrderByCode(c.Request.Context(), code); err == nil {
			return o
		}
	}
	if numeric != 0 {
		if o, err := h.Store.GetPaymentOrderByNumericCode(c.Request.Context(), numeric); err == nil {
			return o
		}
	}
	return nil
}

// verifiedOrder finds the order the (now verified) event names. The
// pre-signature peek is only the fallback for an adapter that could not
// extract a reference the handler's looser peek did; the amount and
// currency check in settlement still guards the match.
func (h *PaymentsWebhookHandler) verifiedOrder(c *gin.Context, ev provider.WebhookEvent, peeked *model.PaymentOrder) *model.PaymentOrder {
	if o := h.findOrder(c, ev.OrderCode, ev.NumericCode); o != nil {
		return o
	}
	return peeked
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
	peeked := h.findOrder(c, code, numeric)
	cfg, err := h.configFor(c, peeked, "sepay")
	if err != nil {
		h.logger().Error("sepay webhook: no usable provider config", "error", err)
		c.Status(http.StatusServiceUnavailable)
		return
	}
	if liveUnsignedWebhook(cfg) {
		h.logger().Error("sepay webhook: webhook_auth_mode none is not allowed for a live config",
			"product", cfg.ProductID)
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
	order := h.verifiedOrder(c, ev, peeked)
	if ev.TransferType == "out" {
		// A refund or payout: keep it on the unmatched ledger, never
		// settle an order from money leaving the account.
		if h.Settlement == nil {
			h.logger().Error("sepay webhook: settlement is not configured")
			c.Status(http.StatusServiceUnavailable)
			return
		}
		if err := h.Settlement.RecordUnmatched(c.Request.Context(), "sepay", ev, order); err != nil {
			h.logger().Error("sepay webhook: record out transfer", "error", err)
			c.Status(http.StatusInternalServerError)
			return
		}
		ackJSON(c)
		return
	}
	h.ingest(c, "sepay", ev, order, ackJSON)
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
	peeked := h.findOrder(c, code, numeric)
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
	h.ingest(c, "sepay", ev, h.verifiedOrder(c, ev, peeked), ackEmpty)
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
	peeked := h.findOrder(c, code, numeric)
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
	h.ingest(c, "payos", ev, h.verifiedOrder(c, ev, peeked), ackJSON)
}

// ingest is the one settlement entry point after verification.
func (h *PaymentsWebhookHandler) ingest(c *gin.Context, providerName string, ev provider.WebhookEvent, order *model.PaymentOrder, ack func(*gin.Context)) {
	if h.Settlement == nil || h.Store == nil {
		h.logger().Error("webhook: settlement is not configured", "provider", providerName)
		c.Status(http.StatusServiceUnavailable)
		return
	}
	// A webhook without a matching order is normal: a manual transfer
	// with the code typed wrong. Record it for the operator instead of
	// 500-looping the provider.
	if order == nil {
		if err := h.Settlement.RecordUnmatched(c.Request.Context(), providerName, ev, nil); err != nil {
			h.logger().Error("webhook: record unmatched transaction", "provider", providerName, "error", err)
			c.Status(http.StatusInternalServerError)
			return
		}
		ack(c)
		return
	}
	// Claim the provider event before settling. A duplicate delivery
	// (the provider's retry, two replicas, a replay) is acked without a
	// second settlement.
	inserted, err := h.Store.RecordPaymentEvent(c.Request.Context(), nil, store.PaymentEventInput{
		Provider: providerName, EventID: ev.ProviderEventID, OrderID: order.ID,
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
		ack(c)
		return
	}
	if err := h.Settlement.Settle(c.Request.Context(), order.ID, providerName, ev); err != nil {
		// The claim was written before settlement; take it back so the
		// provider's retry runs settlement again instead of being
		// mistaken for a duplicate and acked unsettled.
		if ferr := h.Store.ForgetPaymentEvent(c.Request.Context(), providerName, ev.ProviderEventID); ferr != nil {
			h.logger().Error("webhook: release the payment event claim",
				"provider", providerName, "event", ev.ProviderEventID, "error", ferr)
		}
		h.logger().Error("webhook: settle order", "order", order.Code, "error", err)
		c.Status(http.StatusInternalServerError)
		return
	}
	ack(c)
}
