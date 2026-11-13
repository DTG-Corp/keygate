package handler

import (
	"bytes"
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
		`{"provider":"sepay","mode":"live","enabled":true,"config":{"bank_name":"MB Bank","bank_bin":"970422","account_no":"0978482700","account_name":"NGUYEN VAN A"},"credentials":{"api_token":"tok_123"},"webhook_secret":"whsec_123"}`, nil)
	if code != 200 {
		t.Fatalf("upsert: %d %v", code, out)
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
