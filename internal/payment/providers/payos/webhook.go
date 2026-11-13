package payos

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
)

// ErrWebhookUnauthorized reports a webhook whose checksum is missing,
// malformed or computed under a different key. The route answers 401 and
// PayOS never learns whether the order exists.
var ErrWebhookUnauthorized = errors.New("payos: webhook signature invalid")

// webhookPayload is PayOS's envelope. Only data is signed; code/desc
// describe the delivery, and data.code describes the payment.
type webhookPayload struct {
	Code      string         `json:"code"`
	Desc      string         `json:"desc"`
	Success   bool           `json:"success"`
	Data      map[string]any `json:"data"`
	Signature string         `json:"signature"`
}

// ParseWebhook verifies the checksum and maps PayOS's status onto the
// normalised event. Only code=00 + success + data.code=00 is a payment;
// UNDERPAID stays pending and is recorded for an operator.
//
// PayOS signs the body, not the headers, so h is accepted for signature
// symmetry with the other adapters and deliberately unused.
func ParseWebhook(_ http.Header, raw []byte, checksumKey string) (provider.WebhookEvent, error) {
	// An HMAC keyed with nothing is forgeable by anyone, so a missing
	// checksum key fails closed instead of "verifying" the payload.
	if checksumKey == "" {
		return provider.WebhookEvent{}, ErrWebhookUnauthorized
	}
	var p webhookPayload
	// Numbers must stay exact: the checksum is over the JSON values, and
	// float64 would rewrite a large orderCode before canonicalisation.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		return provider.WebhookEvent{}, err
	}
	if p.Signature == "" || !VerifyDataSignature(checksumKey, p.Data, p.Signature) {
		return provider.WebhookEvent{}, ErrWebhookUnauthorized
	}

	ev := provider.WebhookEvent{
		Provider:        "payos",
		ProviderEventID: webhookEventID(p.Data, raw),
		TransferType:    "in",
		Raw:             append(json.RawMessage(nil), raw...),
	}
	if n, ok := intField(p.Data, "orderCode"); ok {
		ev.NumericCode = n
	}
	ev.OrderCode = stringField(p.Data, "description")
	ev.ProviderRef = stringField(p.Data, "paymentLinkId")
	ev.Currency = stringField(p.Data, "currency")
	if n, ok := intField(p.Data, "amount"); ok {
		ev.AmountMinor = n
	}
	if ts := stringField(p.Data, "transactionDateTime"); ts != "" {
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", ts, vnZone); err == nil {
			ev.OccurredAt = t
		}
	}

	dataCode := stringField(p.Data, "code")
	switch {
	case p.Code == "00" && p.Success && dataCode == "00":
		ev.Status = domain.OrderPaid
	case dataCode == "UNDERPAID":
		// Money arrived but less than the order total: the order stays
		// pending and settlement records the transaction for an operator
		// to top up or refund. There is no "partial" order status.
		ev.Status = "partial"
	case dataCode == "EXPIRED":
		ev.Status = domain.OrderExpired
	case dataCode == "CANCELLED":
		ev.Status = domain.OrderCanceled
	default:
		ev.Status = domain.OrderPending
	}
	return ev, nil
}

// vnZone is PayOS's transactionDateTime timezone: Vietnam local time,
// UTC+7, no DST and no offset in the string.
var vnZone = time.FixedZone("ICT", 7*3600)

// webhookEventID is the idempotency key settlement records under
// payment_events (unique on provider+event_id). PayOS's bank reference
// identifies one transfer, so a retry of the same delivery dedupes; a
// status notification for an unpaid link has no reference, and the
// (paymentLinkId, data.code) pair keeps those stable and distinct.
// Settlement refuses an empty id, so the raw-body digest is the last
// resort for a signed but otherwise empty payload.
func webhookEventID(data map[string]any, raw []byte) string {
	if ref := stringField(data, "reference"); ref != "" {
		return ref
	}
	code := stringField(data, "code")
	if link := stringField(data, "paymentLinkId"); link != "" {
		return eventKey("link", link, code)
	}
	if n, ok := intField(data, "orderCode"); ok {
		return eventKey("order", strconv.FormatInt(n, 10), code)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// eventKey joins the non-empty parts with ':', so a missing status does
// not leave a trailing separator that another event could collide with.
func eventKey(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(':')
		}
		b.WriteString(p)
	}
	return b.String()
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// intField reads a JSON integer without losing precision: payloads
// decoded with UseNumber carry json.Number, direct callers may pass an
// int or float64.
func intField(m map[string]any, key string) (int64, bool) {
	switch v := m[key].(type) {
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return n, true
	case float64:
		return int64(v), true
	case int:
		return int64(v), true
	case int64:
		return v, true
	}
	return 0, false
}
