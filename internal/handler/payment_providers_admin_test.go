package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/payment/providercfg"
)

func TestProviderAdminNeverReturnsSecrets(t *testing.T) {
	f := newAPIFixture(t)
	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatal(err)
	}
	svc := providercfg.NewService(f.s, codec, "live")
	h := &PaymentProviderAdminHandler{Service: svc}

	code, out := f.call(h.Upsert, "PUT", "/api/v1/admin/payments/providers",
		`{"provider":"sepay","mode":"live","enabled":true,"config":{"bank_name":"MB Bank","bank_bin":"970422","bank_code":"MB","account_no":"0978482700","account_name":"NGUYEN VAN A"},"credentials":{"api_token":"tok_123"},"webhook_secret":"whsec_123"}`, nil)
	if code != 200 {
		t.Fatalf("upsert: %d %v", code, out)
	}
	// The write response must not echo a secret either; the admin form
	// cannot display it and a stolen session must not recover it from
	// the save reply.
	raw := callRaw(f, h.Upsert, "PUT", "/api/v1/admin/payments/providers",
		`{"provider":"sepay","mode":"live","enabled":true,"config":{"bank_name":"MB Bank","bank_bin":"970422","bank_code":"MB","account_no":"0978482700","account_name":"NGUYEN VAN A"},"credentials":{"api_token":"tok_123"},"webhook_secret":"whsec_123"}`)
	if bytes.Contains(raw, []byte("tok_123")) || bytes.Contains(raw, []byte("whsec_123")) {
		t.Fatalf("upsert response leaked a secret: %s", raw)
	}
	created, _ := out["data"].(map[string]any)
	rowID, _ := created["id"].(string)
	if rowID == "" {
		t.Fatalf("upsert returned no id: %v", out)
	}

	// The table is shared with other tests, so assert the secret values
	// never appear anywhere in the response and then inspect the row
	// this test wrote.
	body := string(f.bodyJSON(t, h.List))
	if bytes.Contains([]byte(body), []byte("tok_123")) {
		t.Fatal("list returned the api token")
	}
	if bytes.Contains([]byte(body), []byte("whsec_123")) {
		t.Fatal("list returned the webhook secret")
	}
	var env struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := f.decodeData(t, h.List, &env); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var row map[string]any
	for _, r := range env.Providers {
		if r["id"] == rowID {
			row = r
			break
		}
	}
	if row == nil {
		t.Fatalf("written row %s missing from list: %v", rowID, env.Providers)
	}
	if row["secret_set"] != true || row["credentials_set"] != true {
		t.Fatalf("flags = %v", row)
	}
	if _, ok := row["credentials"]; ok {
		t.Fatal("credentials field present in admin response")
	}

	// Clearing the webhook secret is an explicit delete, and the row
	// reports it as cleared afterwards.
	code, out = f.call(h.DeleteSecrets, "DELETE", "/api/v1/admin/payments/providers/"+rowID+"/secrets",
		`{"fields":["webhook_secret"]}`, nil, gin.Param{Key: "id", Value: rowID})
	if code != 204 {
		t.Fatalf("delete secrets: %d %v", code, out)
	}
	env.Providers = nil
	if err := f.decodeData(t, h.List, &env); err != nil {
		t.Fatalf("decode list after delete: %v", err)
	}
	for _, r := range env.Providers {
		if r["id"] == rowID {
			if r["secret_set"] != false || r["credentials_set"] != true {
				t.Fatalf("flags after delete = %v", r)
			}
			return
		}
	}
	t.Fatalf("row %s disappeared from list", rowID)
}

func callRaw(f *apiFixture, fn gin.HandlerFunc, method, target, body string, params ...gin.Param) []byte {
	f.t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = params
	fn(c)
	return w.Body.Bytes()
}

func providerHandler(t *testing.T) (*apiFixture, *PaymentProviderAdminHandler) {
	t.Helper()
	f := newAPIFixture(t)
	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x5b}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return f, &PaymentProviderAdminHandler{Service: providercfg.NewService(f.s, codec, "live")}
}

const validSepayBody = `{"provider":"sepay","mode":"live","enabled":true,"config":{"bank_name":"MB Bank","bank_code":"MB","account_no":"0978482700"},"credentials":{"api_token":"tok_err"},"webhook_secret":"whsec_err"}`

// A rejected save must say why with a stable code: unknown provider and
// invalid configuration are 400s the admin form can act on, a database
// failure is a 500 the operator has to look at.
func TestProviderAdminErrorCodes(t *testing.T) {
	f, h := providerHandler(t)

	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{
			name:   "unknown provider",
			body:   `{"provider":"stripe","mode":"live","enabled":true,"config":{},"credentials":{},"webhook_secret":"x"}`,
			status: http.StatusBadRequest,
			code:   "PAYMENT_PROVIDER_UNKNOWN",
		},
		{
			name:   "invalid enabled config",
			body:   `{"provider":"sepay","mode":"live","enabled":true,"config":{"bank_name":"MB Bank"},"credentials":{"api_token":"tok"},"webhook_secret":"x"}`,
			status: http.StatusBadRequest,
			code:   "PAYMENT_PROVIDER_CONFIG_INVALID",
		},
		{
			// A real product id is a foreign key; a made-up one fails in
			// the store and must not be reported as a bad request.
			name:   "database failure",
			body:   strings.TrimSuffix(validSepayBody, "}") + `,"product_id":"00000000-0000-0000-0000-000000000000"}`,
			status: http.StatusInternalServerError,
			code:   "INTERNAL_ERROR",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			code, out := f.call(h.Upsert, "PUT", "/api/v1/admin/payments/providers", tt.body, nil)
			if code != tt.status {
				t.Fatalf("status = %d, want %d (%v)", code, tt.status, out)
			}
			if got := errCode(out); got != tt.code {
				t.Fatalf("code = %q, want %q (%v)", got, tt.code, out)
			}
		})
	}
}

func TestProviderAdminDeleteUnknownRow(t *testing.T) {
	f, h := providerHandler(t)
	code, out := f.call(h.DeleteSecrets, "DELETE", "/api/v1/admin/payments/providers/missing/secrets",
		`{"fields":["webhook_secret"]}`, nil, gin.Param{Key: "id", Value: "missing"})
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%v)", code, out)
	}
	if got := errCode(out); got != "PAYMENT_PROVIDER_NOT_FOUND" {
		t.Fatalf("code = %q, want PAYMENT_PROVIDER_NOT_FOUND (%v)", got, out)
	}
}

// Omitting enabled on an update keeps the stored value; a new row
// defaults to disabled rather than silently enabling a half-filled form.
func TestProviderAdminOmittedEnabledKeepsStored(t *testing.T) {
	f, h := providerHandler(t)
	code, out := f.call(h.Upsert, "PUT", "/api/v1/admin/payments/providers", validSepayBody, nil)
	if code != http.StatusOK {
		t.Fatalf("create: %d %v", code, out)
	}
	created, _ := out["data"].(map[string]any)
	if created["enabled"] != true {
		t.Fatalf("created row enabled = %v", created["enabled"])
	}

	code, out = f.call(h.Upsert, "PUT", "/api/v1/admin/payments/providers",
		`{"provider":"sepay","mode":"live","config":{"bank_name":"MB Bank","bank_code":"MB","account_no":"222"}}`, nil)
	if code != http.StatusOK {
		t.Fatalf("update without enabled: %d %v", code, out)
	}
	row, _ := out["data"].(map[string]any)
	if row["enabled"] != true {
		t.Fatalf("omitted enabled turned the row off: %v", row)
	}

	code, out = f.call(h.Upsert, "PUT", "/api/v1/admin/payments/providers",
		`{"provider":"sepay","mode":"live","enabled":false}`, nil)
	if code != http.StatusOK {
		t.Fatalf("disable: %d %v", code, out)
	}
	row, _ = out["data"].(map[string]any)
	if row["enabled"] != false {
		t.Fatalf("explicit false was ignored: %v", row)
	}
}
