package handler

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// The three settings maps encode a policy, and the failure mode when
// they disagree is quiet: a key that is writable but hidden from
// GetSettings gives the dashboard a control it can save but never read
// back, and a server-owned key that slips into settingsWritable lets an
// operator overwrite the Stripe signing secret through the API.
func TestSettingsMapsAreConsistent(t *testing.T) {
	for key := range settingsServerOwned {
		if settingsWritable[key] {
			t.Errorf("%q is server-owned but also writable — the API must not accept it", key)
		}
		if settingsSecret[key] {
			t.Errorf("%q is server-owned; it is never exposed, so marking it secret is redundant", key)
		}
	}
	for key := range settingsSecret {
		if !settingsWritable[key] {
			t.Errorf("%q is write-only but not writable — nothing could ever set it", key)
		}
	}
}

// Regression guard for the save loop that broke once Stripe was
// configured: the dashboard seeds its form from GetSettings and PUTs
// the result back, so every key GetSettings returns must be one
// UpdateSettings will accept.
func TestGetSettingsOutputIsWritableBack(t *testing.T) {
	stored := map[string]string{
		"site_name":                  "Acme",
		"stripe_webhook_secret":      "whsec_live",
		"stripe_webhook_endpoint_id": "we_1",
		// Payment settings; the API must let an operator write them back.
		"payment_default_provider":  "sepay",
		"payment_order_ttl_minutes": "1440",
		"payment_return_base_url":   "https://shop.example.test",
		"payment_webhook_base_url":  "https://shop.example.test",
		// Written by setup; must be neither returned nor writable.
		"setup_complete": "true",
	}

	for key, val := range stored {
		if settingsServerOwned[key] || settingsSecret[key] {
			continue
		}
		if !settingsWritable[key] {
			t.Errorf("GetSettings would return %q=%q but UpdateSettings rejects it", key, val)
		}
	}

	for _, key := range []string{"stripe_webhook_secret", "stripe_webhook_endpoint_id", "setup_complete"} {
		if !settingsSecret[key] && !settingsServerOwned[key] {
			t.Errorf("%q must never be returned by GetSettings", key)
		}
	}
}

// A fixed-choice setting has to be writable (otherwise the choice can
// never be made) and has to list the exact string its reader compares
// against. signupAllowed tests `mode != "licensed_only"`, so if that
// literal ever drifts out of the enum the API would reject the only
// value that actually turns the restriction on.
func TestSettingsEnumsAreUsable(t *testing.T) {
	for key, values := range settingsEnum {
		if !settingsWritable[key] {
			t.Errorf("%q has an allowed-value list but is not writable — the value could never be set", key)
		}
		if len(values) == 0 {
			t.Errorf("%q has an empty allowed-value list — every save would be rejected", key)
		}
	}

	if !slices.Contains(settingsEnum["signup_mode"], "licensed_only") {
		t.Error(`signup_mode must accept "licensed_only" — that is the value signupAllowed gates on`)
	}
	if !slices.Contains(settingsEnum["signup_mode"], "open") {
		t.Error(`signup_mode must accept "open" — the dashboard sends it to turn the restriction off`)
	}
}

// Payment settings are read by checkout and the webhook URL builder, so
// they must be savable through the admin API. The default provider is a
// fixed choice: an unknown value would be ignored by providerCandidates
// and checkout would silently fall back to sepay.
func TestPaymentSettingsAreWritable(t *testing.T) {
	for _, key := range []string{
		"payment_default_provider",
		"payment_order_ttl_minutes",
		"payment_return_base_url",
		"payment_webhook_base_url",
	} {
		if !settingsWritable[key] {
			t.Errorf("%q is read by the payment code but UpdateSettings rejects it", key)
		}
	}
	for _, provider := range []string{"sepay", "payos"} {
		if !slices.Contains(settingsEnum["payment_default_provider"], provider) {
			t.Errorf("payment_default_provider must accept %q", provider)
		}
	}

	h := &AdminHandler{}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings",
		strings.NewReader(`{"settings":{"payment_default_provider":"stripe"}}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UpdateSettings(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown provider status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, `"code":"BAD_REQUEST"`) ||
		!strings.Contains(body, "invalid value for payment_default_provider") {
		t.Fatalf("unknown provider body = %q, want the invalid-value BAD_REQUEST", body)
	}
}
