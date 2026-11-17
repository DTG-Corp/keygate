package sepay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/payment/provider"
)

// ErrWebhookUnauthorized means the webhook failed authentication: a
// missing, wrong or stale signature/key, or an unusable auth mode.
var ErrWebhookUnauthorized = errors.New("sepay: webhook signature invalid")

// BankWebhookConfig is the per-webhook authentication configuration as
// resolved by providercfg. Exactly one mode is in force at a time.
type BankWebhookConfig struct {
	// Mode is the admin-configured auth mode: "hmac", "apikey", "none".
	Mode string
	// Secret is the HMAC secret (mode=hmac). An empty secret is refused:
	// an HMAC keyed with nothing is forgeable by anyone.
	Secret string
	// APIKey is checked against Authorization: Apikey <key> (mode=apikey).
	APIKey string
	// AllowedCIDRs is the optional IP allowlist. It cannot be enforced
	// here because this function only receives headers; the HTTP route
	// must check the peer address before calling in. An empty list means
	// the dashboard did not enable the allowlist.
	AllowedCIDRs []string
	// Now is a test seam; zero means time.Now().
	Now time.Time
}

type bankWebhookPayload struct {
	ID              int64   `json:"id"`
	Gateway         string  `json:"gateway"`
	TransactionDate string  `json:"transactionDate"`
	AccountNumber   string  `json:"accountNumber"`
	SubAccount      string  `json:"subAccount"`
	Code            *string `json:"code"`
	Content         string  `json:"content"`
	TransferType    string  `json:"transferType"`
	Description     string  `json:"description"`
	TransferAmount  int64   `json:"transferAmount"`
	Accumulated     int64   `json:"accumulated"`
	ReferenceCode   string  `json:"referenceCode"`
}

// ParseBankWebhook verifies and normalises a SePay bank webhook. All
// three auth modes are supported because SePay's dashboard configures
// one of them per webhook; "none" is only acceptable for a test-mode
// config and is the caller's job to refuse in live mode.
func ParseBankWebhook(h http.Header, raw []byte, cfg BankWebhookConfig) (provider.WebhookEvent, error) {
	switch cfg.Mode {
	case "hmac":
		if cfg.Secret == "" {
			return provider.WebhookEvent{}, ErrWebhookUnauthorized
		}
		ts := h.Get("X-SePay-Timestamp")
		sig := strings.TrimPrefix(h.Get("X-SePay-Signature"), "sha256=")
		tsUnix, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			return provider.WebhookEvent{}, ErrWebhookUnauthorized
		}
		now := cfg.Now
		if now.IsZero() {
			now = time.Now()
		}
		skew := now.Sub(time.Unix(tsUnix, 0))
		if skew > 5*time.Minute || skew < -5*time.Minute {
			return provider.WebhookEvent{}, ErrWebhookUnauthorized
		}
		m := hmac.New(sha256.New, []byte(cfg.Secret))
		_, _ = m.Write([]byte(ts + "." + string(raw)))
		want := hex.EncodeToString(m.Sum(nil))
		if !hmac.Equal([]byte(strings.ToLower(sig)), []byte(want)) {
			return provider.WebhookEvent{}, ErrWebhookUnauthorized
		}
	case "apikey":
		got := strings.TrimPrefix(h.Get("Authorization"), "Apikey ")
		if cfg.APIKey == "" || !hmac.Equal([]byte(got), []byte(cfg.APIKey)) {
			return provider.WebhookEvent{}, ErrWebhookUnauthorized
		}
	case "none":
		// Test mode only; the caller refuses this when config.mode=live.
	default:
		return provider.WebhookEvent{}, ErrWebhookUnauthorized
	}

	var p bankWebhookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return provider.WebhookEvent{}, err
	}
	ev := provider.WebhookEvent{
		Provider: "sepay", ProviderEventID: strconv.FormatInt(p.ID, 10),
		ProviderRef: strconv.FormatInt(p.ID, 10), TransferType: p.TransferType,
		AmountMinor: p.TransferAmount, Currency: "VND", Raw: raw,
	}
	// transactionDate is VN local time with no offset.
	if ts, err := time.ParseInLocation("2006-01-02 15:04:05", p.TransactionDate, vnLocation); err == nil {
		ev.OccurredAt = ts
	}
	switch {
	case p.TransferType == "in":
		ev.Status = domain.OrderPaid
	case p.TransferType == "out":
		ev.Status = "out"
	}
	code := ""
	if p.Code != nil {
		code = strings.TrimSpace(*p.Code)
	}
	if !domain.IsOrderCode(code) {
		code = extractOrderCode(p.Content)
	}
	if !domain.IsOrderCode(code) {
		code = extractOrderCode(p.Description)
	}
	ev.OrderCode = code
	return ev, nil
}

var vnLocation = time.FixedZone("ICT", 7*3600)

// extractOrderCode finds the first KG + 6 Crockford chars as a token, so
// "KG7F3K2Q ca phe" matches but "XKG7F3K2Q" does not.
func extractOrderCode(s string) string {
	for i := 0; i+8 <= len(s); i++ {
		if i > 0 {
			prev := s[i-1]
			if prev >= 'A' && prev <= 'Z' || prev >= '0' && prev <= '9' {
				continue
			}
		}
		if code := s[i : i+8]; domain.IsOrderCode(code) {
			if i+8 == len(s) || !isCodeChar(s[i+8]) {
				return code
			}
		}
	}
	return ""
}

func isCodeChar(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func parseSePayAmount(s string) int64 {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
