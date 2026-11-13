package sepay_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/providers/sepay"
)

const sepayBody = `{"id":92704,"gateway":"MB Bank","transactionDate":"2026-11-12 15:04:05","accountNumber":"0978482700","subAccount":"","code":"KG7F3K2Q","content":"KG7F3K2Q chuyen tien","transferType":"in","description":"NGUYEN VAN A chuyen tien","transferAmount":1190000,"accumulated":105000000,"referenceCode":"FT24012345678"}`

func hmacHeader(secret, timestamp, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	_, _ = m.Write([]byte(timestamp + "." + body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestParseBankWebhookHMAC(t *testing.T) {
	secret := "whsec_test"
	now := time.Unix(1700000000, 0).UTC()
	ts := strconv.FormatInt(now.Unix(), 10)
	h := http.Header{}
	h.Set("X-SePay-Timestamp", ts)
	h.Set("X-SePay-Signature", hmacHeader(secret, ts, sepayBody))
	h.Set("X-SePay-Webhook-Id", "evt_1")

	ev, err := sepay.ParseBankWebhook(h, []byte(sepayBody), sepay.BankWebhookConfig{Mode: "hmac", Secret: secret, Now: now})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.OrderCode != "KG7F3K2Q" || ev.AmountMinor != 1190000 || ev.Currency != "VND" || ev.Status != domain.OrderPaid {
		t.Fatalf("event = %+v", ev)
	}
	if ev.ProviderRef != "92704" || ev.TransferType != "in" {
		t.Fatalf("ref/type = %q/%q", ev.ProviderRef, ev.TransferType)
	}
}

func TestParseBankWebhookRejectsBadSignatureAndSkew(t *testing.T) {
	secret := "whsec_test"
	now := time.Unix(1700000000, 0).UTC()
	ts := strconv.FormatInt(now.Unix(), 10)
	h := http.Header{}
	h.Set("X-SePay-Timestamp", ts)
	h.Set("X-SePay-Signature", hmacHeader("wrong", ts, sepayBody))

	if _, err := sepay.ParseBankWebhook(h, []byte(sepayBody), sepay.BankWebhookConfig{Mode: "hmac", Secret: secret, Now: now}); err == nil {
		t.Fatal("bad signature accepted")
	}
	// A replayed body outside the ±300 s window is refused even with a
	// valid signature.
	old := strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10)
	h.Set("X-SePay-Timestamp", old)
	h.Set("X-SePay-Signature", hmacHeader(secret, old, sepayBody))
	if _, err := sepay.ParseBankWebhook(h, []byte(sepayBody), sepay.BankWebhookConfig{Mode: "hmac", Secret: secret, Now: now}); err == nil {
		t.Fatal("stale timestamp accepted")
	}
	// A missing configured secret must not accept an HMAC keyed with
	// nothing: that signature is forgeable by anyone.
	h.Set("X-SePay-Timestamp", ts)
	h.Set("X-SePay-Signature", hmacHeader("", ts, sepayBody))
	if _, err := sepay.ParseBankWebhook(h, []byte(sepayBody), sepay.BankWebhookConfig{Mode: "hmac", Now: now}); err == nil {
		t.Fatal("empty secret accepted")
	}
}

// The replay window is pinned to the Now seam: exactly ±300 s is accepted,
// one second more is refused, and unusable timestamps fail closed. No
// wall-clock reads are involved.
func TestParseBankWebhookWindowBoundary(t *testing.T) {
	secret := "whsec_test"
	now := time.Unix(1700000000, 0).UTC()
	cfg := sepay.BankWebhookConfig{Mode: "hmac", Secret: secret, Now: now}
	tsAt := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }
	signed := func(ts string) http.Header {
		h := http.Header{}
		h.Set("X-SePay-Timestamp", ts)
		h.Set("X-SePay-Signature", hmacHeader(secret, ts, sepayBody))
		return h
	}

	for _, d := range []time.Duration{-300 * time.Second, 0, 300 * time.Second} {
		if _, err := sepay.ParseBankWebhook(signed(tsAt(d)), []byte(sepayBody), cfg); err != nil {
			t.Fatalf("skew %s rejected: %v", d, err)
		}
	}
	for _, d := range []time.Duration{-301 * time.Second, 301 * time.Second, -10 * time.Minute, 10 * time.Minute} {
		if _, err := sepay.ParseBankWebhook(signed(tsAt(d)), []byte(sepayBody), cfg); !errors.Is(err, sepay.ErrWebhookUnauthorized) {
			t.Fatalf("skew %s err = %v, want ErrWebhookUnauthorized", d, err)
		}
	}

	missing := signed(tsAt(0))
	missing.Del("X-SePay-Timestamp")
	nonNumeric := signed(tsAt(0))
	nonNumeric.Set("X-SePay-Timestamp", "not-a-time")
	for name, h := range map[string]http.Header{"missing": missing, "non-numeric": nonNumeric} {
		if _, err := sepay.ParseBankWebhook(h, []byte(sepayBody), cfg); !errors.Is(err, sepay.ErrWebhookUnauthorized) {
			t.Fatalf("%s timestamp err = %v, want ErrWebhookUnauthorized", name, err)
		}
	}

	if _, err := sepay.ParseBankWebhook(signed(tsAt(0)), []byte(sepayBody), sepay.BankWebhookConfig{Mode: "hmac", Now: now}); !errors.Is(err, sepay.ErrWebhookUnauthorized) {
		t.Fatalf("empty secret err = %v, want ErrWebhookUnauthorized", err)
	}
}

func TestParseBankWebhookExtractsCodeFromContent(t *testing.T) {
	// `code` null (unlinked bank), the code only in the free-text memo.
	body := `{"id":1,"gateway":"MB","transactionDate":"2026-11-12 15:04:05","accountNumber":"0978482700","subAccount":"","code":null,"content":"ACME JSC chuyen tien KG7F3K2Q ca phe","transferType":"in","description":"x","transferAmount":50000,"accumulated":0,"referenceCode":"FT1"}`
	ev, err := sepay.ParseBankWebhook(http.Header{}, []byte(body), sepay.BankWebhookConfig{Mode: "none"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.OrderCode != "KG7F3K2Q" {
		t.Fatalf("order code = %q", ev.OrderCode)
	}
	// transferType=out is a refund/payout, never a payment.
	out := `{"id":2,"gateway":"MB","transactionDate":"2026-11-12 15:04:05","accountNumber":"0978482700","code":"KG7F3K2Q","content":"out","transferType":"out","description":"x","transferAmount":50000,"accumulated":0,"referenceCode":"FT2"}`
	ev, err = sepay.ParseBankWebhook(http.Header{}, []byte(out), sepay.BankWebhookConfig{Mode: "none"})
	if err != nil || ev.TransferType != "out" || ev.Status == domain.OrderPaid {
		t.Fatalf("out event = %+v, %v", ev, err)
	}
}

func TestProviderParseWebhookWiresResolvedAuth(t *testing.T) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	hmacProvider := sepay.New(sepay.Config{WebhookAuthMode: "hmac", WebhookSecret: "whsec_test"})
	r := httptest.NewRequest(http.MethodPost, "/webhook/sepay", strings.NewReader(sepayBody))
	r.Header.Set("X-SePay-Timestamp", ts)
	r.Header.Set("X-SePay-Signature", hmacHeader("whsec_test", ts, sepayBody))
	ev, err := hmacProvider.ParseWebhook(r, []byte(sepayBody), []byte("whsec_test"))
	if err != nil {
		t.Fatalf("hmac parse: %v", err)
	}
	if ev.OrderCode != "KG7F3K2Q" || ev.Status != domain.OrderPaid {
		t.Fatalf("event = %+v", ev)
	}
	if _, err := hmacProvider.ParseWebhook(r, []byte(sepayBody), []byte("wrong")); !errors.Is(err, sepay.ErrWebhookUnauthorized) {
		t.Fatalf("wrong secret err = %v", err)
	}

	// The resolved Config.WebhookSecret is the fallback when the route
	// passes no per-request secret; both empty fails closed.
	r = httptest.NewRequest(http.MethodPost, "/webhook/sepay", strings.NewReader(sepayBody))
	r.Header.Set("X-SePay-Timestamp", ts)
	r.Header.Set("X-SePay-Signature", hmacHeader("whsec_test", ts, sepayBody))
	if _, err := hmacProvider.ParseWebhook(r, []byte(sepayBody), nil); err != nil {
		t.Fatalf("config secret fallback: %v", err)
	}
	noSecret := sepay.New(sepay.Config{WebhookAuthMode: "hmac"})
	r = httptest.NewRequest(http.MethodPost, "/webhook/sepay", strings.NewReader(sepayBody))
	r.Header.Set("X-SePay-Timestamp", ts)
	r.Header.Set("X-SePay-Signature", hmacHeader("", ts, sepayBody))
	if _, err := noSecret.ParseWebhook(r, []byte(sepayBody), nil); !errors.Is(err, sepay.ErrWebhookUnauthorized) {
		t.Fatalf("no configured secret err = %v", err)
	}

	// apikey mode reads the key from the provider config, not the
	// per-request secret.
	keyProvider := sepay.New(sepay.Config{WebhookAuthMode: "apikey", APIKey: "apikey_test"})
	r = httptest.NewRequest(http.MethodPost, "/webhook/sepay", strings.NewReader(sepayBody))
	r.Header.Set("Authorization", "Apikey apikey_test")
	if _, err := keyProvider.ParseWebhook(r, []byte(sepayBody), nil); err != nil {
		t.Fatalf("apikey parse: %v", err)
	}
	if _, err := keyProvider.ParseWebhook(httptest.NewRequest(http.MethodPost, "/webhook/sepay", strings.NewReader(sepayBody)), []byte(sepayBody), nil); err == nil {
		t.Fatal("missing apikey accepted")
	}
}
