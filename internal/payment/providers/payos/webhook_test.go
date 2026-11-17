package payos_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/payment/providers/payos"
)

const checksumKey = "checksum-key"

// payosBody wraps a data object the way PayOS does: the checksum is over
// data only, not over the envelope.
func payosBody(t *testing.T, code string, success bool, data map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"code": code, "desc": "success", "success": success, "data": data,
		"signature": payos.SignWebhook(checksumKey, data),
	})
	if err != nil {
		t.Fatalf("marshal webhook: %v", err)
	}
	return body
}

func paidData() map[string]any {
	return map[string]any{
		"orderCode": 123.0, "amount": 1200000.0, "description": "KG7F3K2Q",
		"currency": "VND", "paymentLinkId": "abc", "code": "00",
		"transactionDateTime": "2026-11-12 15:04:05",
	}
}

func TestParseWebhookPaid(t *testing.T) {
	data := paidData()
	ev, err := payos.ParseWebhook(http.Header{}, payosBody(t, "00", true, data), checksumKey)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Status != domain.OrderPaid || ev.OrderCode != "KG7F3K2Q" || ev.NumericCode != 123 || ev.AmountMinor != 1200000 {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Provider != "payos" || ev.TransferType != "in" || ev.Currency != "VND" || ev.ProviderRef != "abc" {
		t.Fatalf("event envelope = %+v", ev)
	}
	// Settlement records the event under ProviderEventID and refuses an
	// empty id (payment_events dedupes on provider+event_id), so every
	// signed webhook must carry one.
	if ev.ProviderEventID == "" {
		t.Fatal("webhook event has no provider event id")
	}
	// PayOS sends Vietnam local time; 15:04:05 ICT is 08:04:05 UTC.
	want := time.Date(2026, 11, 12, 15, 4, 5, 0, time.FixedZone("ICT", 7*3600))
	if !ev.OccurredAt.Equal(want) {
		t.Fatalf("occurred at = %s, want %s", ev.OccurredAt, want)
	}
	if string(ev.Raw) != string(payosBody(t, "00", true, data)) {
		t.Fatal("raw body was not preserved")
	}
}

func TestParseWebhookRejectsBadSignature(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"code": "00", "success": true,
		"data": map[string]any{"orderCode": 1.0}, "signature": "deadbeef"})
	if _, err := payos.ParseWebhook(http.Header{}, body, checksumKey); !errors.Is(err, payos.ErrWebhookUnauthorized) {
		t.Fatalf("bad signature error = %v, want ErrWebhookUnauthorized", err)
	}

	// No signature at all must not be treated as an unauthenticated event.
	unsigned, _ := json.Marshal(map[string]any{"code": "00", "success": true,
		"data": map[string]any{"orderCode": 1.0}})
	if _, err := payos.ParseWebhook(http.Header{}, unsigned, checksumKey); !errors.Is(err, payos.ErrWebhookUnauthorized) {
		t.Fatalf("missing signature error = %v, want ErrWebhookUnauthorized", err)
	}

	// A body edited after signing must fail even though the payload is
	// otherwise well-formed.
	body = payosBody(t, "00", true, paidData())
	tampered := []byte(string(body))
	tampered[len(tampered)/2] = 'X'
	if _, err := payos.ParseWebhook(http.Header{}, tampered, checksumKey); err == nil {
		t.Fatal("tampered body accepted")
	}
}

// An empty checksum key would verify any signature an attacker computes
// with the same empty key, so parsing must refuse rather than accept.
func TestParseWebhookRejectsEmptyChecksumKey(t *testing.T) {
	data := paidData()
	body, _ := json.Marshal(map[string]any{"code": "00", "success": true, "data": data,
		"signature": payos.SignWebhook("", data)})
	if _, err := payos.ParseWebhook(http.Header{}, body, ""); !errors.Is(err, payos.ErrWebhookUnauthorized) {
		t.Fatalf("empty checksum key error = %v, want ErrWebhookUnauthorized", err)
	}
}

func TestParseWebhookRejectsMalformedJSON(t *testing.T) {
	if _, err := payos.ParseWebhook(http.Header{}, []byte("{not json"), checksumKey); err == nil {
		t.Fatal("malformed body accepted")
	}
}

// Notifications about an unpaid link are normal deliveries too: they
// must map onto the settlement vocabulary instead of being dropped or
// mistaken for a payment.
func TestParseWebhookStatusMapping(t *testing.T) {
	cases := []struct {
		name     string
		topCode  string
		success  bool
		dataCode string
		want     string
	}{
		{"paid", "00", true, "00", domain.OrderPaid},
		{"underpaid", "00", true, "UNDERPAID", "partial"},
		{"expired", "00", true, "EXPIRED", domain.OrderExpired},
		{"cancelled", "00", true, "CANCELLED", domain.OrderCanceled},
		{"pending", "00", true, "PENDING", domain.OrderPending},
		{"failed envelope", "01", false, "00", domain.OrderPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := paidData()
			data["code"] = tc.dataCode
			ev, err := payos.ParseWebhook(http.Header{}, payosBody(t, tc.topCode, tc.success, data), checksumKey)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if ev.Status != tc.want {
				t.Fatalf("status = %q, want %q", ev.Status, tc.want)
			}
		})
	}
}

// A replayed body must dedupe to the same event id, while a different
// payment (or a different status for the same link) must not.
func TestParseWebhookEventIDStable(t *testing.T) {
	paid := paidData()
	paid["reference"] = "TF230204212323"
	first, err := payos.ParseWebhook(http.Header{}, payosBody(t, "00", true, paid), checksumKey)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	replay, err := payos.ParseWebhook(http.Header{}, payosBody(t, "00", true, paid), checksumKey)
	if err != nil {
		t.Fatalf("parse replay: %v", err)
	}
	if first.ProviderEventID != "TF230204212323" || replay.ProviderEventID != first.ProviderEventID {
		t.Fatalf("event ids = %q / %q, want the bank reference", first.ProviderEventID, replay.ProviderEventID)
	}

	// Status notifications carry no bank reference: they still need a
	// stable, distinct id, or settlement would drop them as duplicates.
	expired := paidData()
	delete(expired, "reference")
	expired["code"] = "EXPIRED"
	expiredEv, err := payos.ParseWebhook(http.Header{}, payosBody(t, "00", true, expired), checksumKey)
	if err != nil {
		t.Fatalf("parse expired: %v", err)
	}
	delete(paid, "reference")
	paidEv, err := payos.ParseWebhook(http.Header{}, payosBody(t, "00", true, paid), checksumKey)
	if err != nil {
		t.Fatalf("parse paid without reference: %v", err)
	}
	if expiredEv.ProviderEventID == "" || expiredEv.ProviderEventID == paidEv.ProviderEventID {
		t.Fatalf("event ids collide: expired %q, paid %q", expiredEv.ProviderEventID, paidEv.ProviderEventID)
	}

	// Even an empty-but-signed data object must produce an id, because
	// settlement rejects an empty one with a 500 the provider would retry.
	empty, err := payos.ParseWebhook(http.Header{}, payosBody(t, "00", false, map[string]any{}), checksumKey)
	if err != nil {
		t.Fatalf("parse empty data: %v", err)
	}
	if empty.ProviderEventID == "" {
		t.Fatal("empty data object produced no event id")
	}
}
