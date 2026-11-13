package sepay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
)

// gatewayFieldOrder is the canonical order SePay's hosted-checkout
// signature requires (docs: "chữ ký" — fields in this exact order,
// empty and zero values dropped).
var gatewayFieldOrder = []string{
	"order_amount", "merchant", "currency", "operation", "order_description",
	"order_invoice_number", "customer_id", "payment_method", "success_url",
	"error_url", "cancel_url",
}

// GatewayCanonical joins the non-empty, non-zero signature fields in
// SePay's required order. The hosted form signs this exact string.
func GatewayCanonical(fields map[string]string) string {
	parts := make([]string, 0, len(gatewayFieldOrder))
	for _, key := range gatewayFieldOrder {
		v := fields[key]
		if v == "" || v == "0" {
			continue
		}
		parts = append(parts, key+"="+v)
	}
	return strings.Join(parts, ",")
}

// SignGateway returns base64(HMAC-SHA256(secret_key, canonical)) —
// base64, not hex, because SePay's hosted form expects this alphabet.
func SignGateway(secretKey, canonical string) string {
	m := hmac.New(sha256.New, []byte(secretKey))
	_, _ = m.Write([]byte(canonical))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// ParseIPN verifies X-Secret-Key and normalises the gateway
// notification. Only ORDER_PAID + CAPTURED + APPROVED counts as paid.
func ParseIPN(header http.Header, raw []byte, ipnSecret string) (provider.WebhookEvent, error) {
	if ipnSecret == "" || !hmac.Equal([]byte(header.Get("X-Secret-Key")), []byte(ipnSecret)) {
		return provider.WebhookEvent{}, ErrWebhookUnauthorized
	}
	var p struct {
		NotificationType string `json:"notification_type"`
		Order            struct {
			OrderInvoiceNumber string `json:"order_invoice_number"`
			OrderStatus        string `json:"order_status"`
			OrderAmount        string `json:"order_amount"`
			OrderCurrency      string `json:"order_currency"`
		} `json:"order"`
		Transaction struct {
			ID                string `json:"id"`
			TransactionStatus string `json:"transaction_status"`
			TransactionAmount string `json:"transaction_amount"`
		} `json:"transaction"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return provider.WebhookEvent{}, err
	}
	// Settlement dedupes on (provider, event_id) and refuses an empty
	// id. The notification type is part of the id because SePay sends
	// several notifications (pending, paid, void) for one transaction;
	// a retry of the same notification still dedupes.
	eventID := strings.TrimSpace(p.Transaction.ID)
	if eventID == "" {
		eventID = strings.TrimSpace(p.Order.OrderInvoiceNumber)
	}
	if nt := strings.TrimSpace(p.NotificationType); nt != "" {
		if eventID != "" {
			eventID = nt + ":" + eventID
		} else {
			eventID = nt
		}
	}
	ev := provider.WebhookEvent{Provider: "sepay", ProviderEventID: eventID, ProviderRef: p.Transaction.ID,
		OrderCode: strings.TrimSpace(p.Order.OrderInvoiceNumber), TransferType: "in",
		Currency: p.Order.OrderCurrency, Raw: raw}
	if p.NotificationType == "ORDER_PAID" && p.Order.OrderStatus == "CAPTURED" && p.Transaction.TransactionStatus == "APPROVED" {
		ev.Status = domain.OrderPaid
	} else if p.NotificationType == "TRANSACTION_VOID" {
		ev.Status = "voided"
	} else {
		ev.Status = "pending"
	}
	// SePay sends money as "50000.00" strings; VND has no minor digits.
	ev.AmountMinor = parseSePayAmount(p.Transaction.TransactionAmount)
	if ev.AmountMinor == 0 {
		ev.AmountMinor = parseSePayAmount(p.Order.OrderAmount)
	}
	return ev, nil
}
