package payos_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/payment/provider"
	"github.com/dtg-corp/dsoft/internal/payment/providers/payos"
)

const (
	testClientID = "client-1"
	testAPIKey   = "api-key"
)

// newClient points the client at a stub PayOS host. srv.Client() is used
// so the test also works if the stub is ever switched to TLS.
func newClient(t *testing.T, h http.HandlerFunc) *payos.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &payos.Client{
		BaseURL: srv.URL, ClientID: testClientID, APIKey: testAPIKey,
		ChecksumKey: checksumKey, HTTP: srv.Client(),
	}
}

func readJSON(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read request body: %v", err)
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Errorf("decode request body %q: %v", raw, err)
		return nil
	}
	return m
}

// writeEnvelope answers the way PayOS does, signing the data object so
// the client's response-integrity check is exercised on the happy path.
func writeEnvelope(w http.ResponseWriter, code, desc string, data map[string]any) {
	body := map[string]any{"code": code, "desc": desc}
	if data != nil {
		body["data"] = data
		body["signature"] = payos.SignWebhook(checksumKey, data)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func linkData() map[string]any {
	return map[string]any{
		"checkoutUrl":   "https://pay.payos.vn/web/pl-1",
		"qrCode":        "0002010102123857",
		"paymentLinkId": "pl-1",
		"status":        "PENDING",
		"amount":        1200000,
		"expiredAt":     1793000000,
	}
}

func TestCreatePaymentLinkSendsSignedRequest(t *testing.T) {
	req := payos.CreateRequest{
		OrderCode: 123, Amount: 1200000, Description: "KG7F3K2Q",
		BuyerName: "Nguyen Van A", BuyerEmail: "buyer@example.com",
		BuyerPhone: "+84900000000", BuyerAddress: "1 Le Loi", BuyerTaxID: "0312345678",
		Items:     []payos.Item{{Name: "Pro", Quantity: 1, Price: 1200000, TaxPercentage: 5}},
		CancelURL: "https://dsoft.example.com/pay/KG7F3K2Q",
		ReturnURL: "https://dsoft.example.com/pay/KG7F3K2Q",
		ExpiredAt: 1793000000,
	}
	var called atomic.Bool
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		if r.Method != http.MethodPost || r.URL.Path != "/v2/payment-requests" {
			t.Errorf("request = %s %s, want POST /v2/payment-requests", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("x-client-id"); got != testClientID {
			t.Errorf("x-client-id = %q", got)
		}
		if got := r.Header.Get("x-api-key"); got != testAPIKey {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		body := readJSON(t, r)
		if got := body["signature"]; got != payos.SignCreateRequest(checksumKey, req) {
			t.Errorf("signature = %v, want %s", got, payos.SignCreateRequest(checksumKey, req))
		}
		for field, want := range map[string]float64{
			"orderCode": 123, "amount": 1200000, "expiredAt": 1793000000,
		} {
			if got := body[field]; got != want {
				t.Errorf("%s = %v, want %v", field, got, want)
			}
		}
		for field, want := range map[string]string{
			"description": "KG7F3K2Q", "cancelUrl": req.CancelURL, "returnUrl": req.ReturnURL,
			"buyerName": req.BuyerName, "buyerEmail": req.BuyerEmail, "buyerPhone": req.BuyerPhone,
			"buyerAddress": req.BuyerAddress, "buyerTaxCode": req.BuyerTaxID,
		} {
			if got := body[field]; got != want {
				t.Errorf("%s = %v, want %q", field, got, want)
			}
		}
		items, ok := body["items"].([]any)
		if !ok || len(items) != 1 {
			t.Errorf("items = %v", body["items"])
		} else if item, ok := items[0].(map[string]any); !ok || item["name"] != "Pro" || item["quantity"] != float64(1) {
			t.Errorf("items[0] = %v", items[0])
		}
		writeEnvelope(w, "00", "success", linkData())
	})

	link, err := client.CreatePaymentLink(context.Background(), req)
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	if !called.Load() {
		t.Fatal("no request reached the stub")
	}
	if link.CheckoutURL != "https://pay.payos.vn/web/pl-1" || link.PaymentLinkID != "pl-1" ||
		link.QRCode != "0002010102123857" || link.Status != "PENDING" ||
		link.Amount != 1200000 || link.ExpiredAt != 1793000000 {
		t.Fatalf("link = %+v", link)
	}
}

func TestCreatePaymentLinkOmitsEmptyOptionalFields(t *testing.T) {
	req := payos.CreateRequest{OrderCode: 7, Amount: 5000, Description: "KG000007"}
	var seen map[string]any
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = readJSON(t, r)
		writeEnvelope(w, "00", "success", linkData())
	})
	if _, err := client.CreatePaymentLink(context.Background(), req); err != nil {
		t.Fatalf("create link: %v", err)
	}
	for _, field := range []string{"expiredAt", "items", "buyerName", "buyerEmail", "buyerPhone", "buyerAddress", "buyerTaxCode"} {
		if _, ok := seen[field]; ok {
			t.Errorf("empty %s was sent", field)
		}
	}
	if _, ok := seen["expiredAt"]; ok && req.ExpiredAt == 0 {
		t.Error("zero expiredAt was sent")
	}
}

// PayOS reports business errors with a 2xx status and a non-00 code, so
// the client must surface them as a typed error, not a successful link.
func TestCreatePaymentLinkReturnsAPIError(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "231", "desc": "Đơn thanh toán đã tồn tại",
		})
	})
	_, err := client.CreatePaymentLink(context.Background(), payos.CreateRequest{OrderCode: 123})
	var apiErr *payos.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "231" {
		t.Fatalf("error = %v, want *APIError code 231", err)
	}
}

// The create response carries a signature over data; a tampered
// checkoutUrl must be rejected before the customer is redirected.
func TestCreatePaymentLinkVerifiesResponseSignature(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		data := linkData()
		body := map[string]any{"code": "00", "desc": "success", "data": data, "signature": "deadbeef"}
		_ = json.NewEncoder(w).Encode(body)
	})
	if _, err := client.CreatePaymentLink(context.Background(), payos.CreateRequest{OrderCode: 1}); !errors.Is(err, payos.ErrResponseSignature) {
		t.Fatalf("error = %v, want ErrResponseSignature", err)
	}

	// A response without a signature is tolerated: PayOS has changed the
	// field's presence over time and TLS still authenticates the peer,
	// but a present-and-wrong signature is never accepted.
	client = newClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "00", "desc": "success", "data": linkData()})
	})
	if _, err := client.CreatePaymentLink(context.Background(), payos.CreateRequest{OrderCode: 1}); err != nil {
		t.Fatalf("unsigned response rejected: %v", err)
	}
}

func TestGetPaymentLink(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/payment-requests/123" {
			t.Errorf("request = %s %s, want GET /v2/payment-requests/123", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("x-client-id"); got != testClientID {
			t.Errorf("x-client-id = %q", got)
		}
		writeEnvelope(w, "00", "success", linkData())
	})
	link, err := client.GetPaymentLink(context.Background(), 123)
	if err != nil {
		t.Fatalf("get link: %v", err)
	}
	if link.PaymentLinkID != "pl-1" || link.Amount != 1200000 {
		t.Fatalf("link = %+v", link)
	}
}

func TestGetPaymentLinkReturnsAPIError(t *testing.T) {
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "01", "desc": "Không tìm thấy"})
	})
	_, err := client.GetPaymentLink(context.Background(), 123)
	var apiErr *payos.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "01" || apiErr.Desc != "Không tìm thấy" {
		t.Fatalf("error = %v, want *APIError code 01", err)
	}
	if apiErr.Error() == "" {
		t.Fatal("APIError has an empty message")
	}
}

// TestConnection must distinguish "keys work but the order does not
// exist" (codes 01 and 101) from "keys are wrong" (401).
func TestConnection(t *testing.T) {
	for _, code := range []string{"01", "101"} {
		notFound := newClient(t, func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "desc": "not found"})
		})
		if err := notFound.TestConnection(context.Background()); err != nil {
			t.Fatalf("not-found order (code %s) failed the connection test: %v", code, err)
		}
	}

	unauthorized := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "401", "desc": "Unauthorized"})
	})
	if err := unauthorized.TestConnection(context.Background()); err == nil {
		t.Fatal("bad credentials passed the connection test")
	}
}

func TestCancelPaymentLink(t *testing.T) {
	var body map[string]any
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/payment-requests/123/cancel" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		body = readJSON(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "00", "desc": "success", "data": linkData()})
	})
	if err := client.CancelPaymentLink(context.Background(), 123, "Cancelled by merchant"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if body["cancellationReason"] != "Cancelled by merchant" {
		t.Fatalf("cancellationReason = %v", body["cancellationReason"])
	}

	failing := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "221", "desc": "link already paid"})
	})
	err := failing.CancelPaymentLink(context.Background(), 123, "no")
	var apiErr *payos.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "221" {
		t.Fatalf("cancel error = %v, want *APIError code 221", err)
	}
}

func TestConfirmWebhook(t *testing.T) {
	var body map[string]any
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/confirm-webhook" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		body = readJSON(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "00", "desc": "success", "data": "ok"})
	})
	if err := client.ConfirmWebhook(context.Background(), "https://dsoft.example.com/api/v1/webhook/payos"); err != nil {
		t.Fatalf("confirm webhook: %v", err)
	}
	if body["webhookUrl"] != "https://dsoft.example.com/api/v1/webhook/payos" {
		t.Fatalf("webhookUrl = %v", body["webhookUrl"])
	}
}

func TestClientRateLimitedAndServerError(t *testing.T) {
	limited := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	if _, err := limited.GetPaymentLink(context.Background(), 1); !errors.Is(err, payos.ErrRateLimited) {
		t.Fatalf("429 error = %v, want ErrRateLimited", err)
	}

	broken := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := broken.GetPaymentLink(context.Background(), 1); err == nil {
		t.Fatal("500 response reported success")
	}
}

// --- Provider wiring -------------------------------------------------

func newProvider(t *testing.T, baseURL string) *payos.Provider {
	t.Helper()
	return payos.New(payos.Config{
		BaseURL: baseURL, ClientID: testClientID, APIKey: testAPIKey, ChecksumKey: checksumKey,
	})
}

func TestProviderName(t *testing.T) {
	if got := newProvider(t, "").Name(); got != "payos" {
		t.Fatalf("name = %q, want payos", got)
	}
}

func TestProviderCreateAttempt(t *testing.T) {
	expiresAt := time.Date(2026, 11, 13, 15, 4, 5, 0, time.UTC)
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = readJSON(t, r)
		writeEnvelope(w, "00", "success", linkData())
	}))
	t.Cleanup(srv.Close)

	p := newProvider(t, srv.URL)
	res, err := p.CreateAttempt(context.Background(), provider.CreateAttemptRequest{
		OrderCode: "KG7F3K2Q", NumericCode: 123, AmountMinor: 1200000, Currency: "VND",
		Description: "KG7F3K2Q",
		Buyer: provider.Buyer{
			Name: "Nguyen Van A", Email: "buyer@example.com",
			Phone: "+84900000000", TaxID: "0312345678", Address: "1 Le Loi",
		},
		ReturnURL: "https://dsoft.example.com/pay/KG7F3K2Q",
		CancelURL: "https://dsoft.example.com/pay/KG7F3K2Q",
		ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	if res.ProviderRef != "pl-1" || res.CheckoutURL != "https://pay.payos.vn/web/pl-1" ||
		res.QRPayload != "0002010102123857" || !res.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("result = %+v", res)
	}
	wantSig := payos.SignCreateRequest(checksumKey, payos.CreateRequest{
		OrderCode: 123, Amount: 1200000, Description: "KG7F3K2Q",
		CancelURL: "https://dsoft.example.com/pay/KG7F3K2Q",
		ReturnURL: "https://dsoft.example.com/pay/KG7F3K2Q",
	})
	if body["signature"] != wantSig {
		t.Fatalf("signature = %v, want %s", body["signature"], wantSig)
	}
	if body["orderCode"] != float64(123) || body["amount"] != float64(1200000) ||
		body["expiredAt"] != float64(expiresAt.Unix()) {
		t.Fatalf("body = %v", body)
	}
	if body["buyerTaxCode"] != "0312345678" || body["buyerName"] != "Nguyen Van A" {
		t.Fatalf("buyer fields = %v", body)
	}
}

// PayOS stores expiredAt as a 32-bit epoch, so a longer-lived order must
// be clamped before the request is signed, or PayOS rejects the link.
func TestProviderCreateAttemptClampsExpiry(t *testing.T) {
	const maxExpiredAt = int64(2147472000) // 2038-01-19T00:00:00Z, below the int32 limit
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = readJSON(t, r)
		writeEnvelope(w, "00", "success", linkData())
	}))
	t.Cleanup(srv.Close)

	p := newProvider(t, srv.URL)
	res, err := p.CreateAttempt(context.Background(), provider.CreateAttemptRequest{
		NumericCode: 123, AmountMinor: 1000, Description: "KG7F3K2Q",
		ExpiresAt: time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	if body["expiredAt"] != float64(maxExpiredAt) {
		t.Fatalf("expiredAt = %v, want %d", body["expiredAt"], maxExpiredAt)
	}
	if want := time.Date(2038, 1, 19, 0, 0, 0, 0, time.UTC); !res.ExpiresAt.Equal(want) {
		t.Fatalf("result expiry = %s, want %s", res.ExpiresAt, want)
	}
}

// A zero claim time must be omitted rather than sent as 1970 (or a
// negative epoch), which PayOS would treat as an already-expired link.
func TestProviderCreateAttemptWithoutExpiry(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body = readJSON(t, r)
		writeEnvelope(w, "00", "success", linkData())
	}))
	t.Cleanup(srv.Close)

	p := newProvider(t, srv.URL)
	res, err := p.CreateAttempt(context.Background(), provider.CreateAttemptRequest{
		NumericCode: 123, AmountMinor: 1000, Description: "KG7F3K2Q",
	})
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	if _, ok := body["expiredAt"]; ok {
		t.Fatalf("expiredAt was sent for a zero expiry: %v", body["expiredAt"])
	}
	if !res.ExpiresAt.IsZero() {
		t.Fatalf("result expiry = %s, want zero", res.ExpiresAt)
	}
}

func TestProviderCreateAttemptRequiresCredentials(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
	}))
	t.Cleanup(srv.Close)

	p := payos.New(payos.Config{BaseURL: srv.URL, ChecksumKey: checksumKey})
	if _, err := p.CreateAttempt(context.Background(), provider.CreateAttemptRequest{NumericCode: 1}); !errors.Is(err, payos.ErrNotConfigured) {
		t.Fatalf("error = %v, want ErrNotConfigured", err)
	}
	if called.Load() {
		t.Fatal("unconfigured provider called the API")
	}
}

// Phase 1a leaves unpaid PayOS links to expire with the order: the
// cancel API needs the numeric orderCode, which the attempt row does not
// carry, so Provider.CancelAttempt must be a safe no-op.
func TestProviderCancelAttemptIsNoop(t *testing.T) {
	p := payos.New(payos.Config{})
	if err := p.CancelAttempt(context.Background(), "pl-1"); err != nil {
		t.Fatalf("cancel = %v, want nil", err)
	}
}

func TestProviderParseWebhook(t *testing.T) {
	p := newProvider(t, "")
	body := payosBody(t, "00", true, paidData())
	r := httptest.NewRequest(http.MethodPost, "/api/v1/webhook/payos", bytes.NewReader(body))
	ev, err := p.ParseWebhook(r, body, []byte("ignored: the checksum key signs PayOS webhooks"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Status != domain.OrderPaid || ev.NumericCode != 123 {
		t.Fatalf("event = %+v", ev)
	}

	if _, err := p.ParseWebhook(r, []byte(`{"code":"00","success":true,"data":{},"signature":"deadbeef"}`), nil); !errors.Is(err, payos.ErrWebhookUnauthorized) {
		t.Fatalf("bad signature error = %v, want ErrWebhookUnauthorized", err)
	}

	// Without a checksum key, verification is impossible: fail closed.
	empty := payos.New(payos.Config{})
	if _, err := empty.ParseWebhook(r, body, nil); !errors.Is(err, payos.ErrNotConfigured) {
		t.Fatalf("unconfigured parse error = %v, want ErrNotConfigured", err)
	}
}

func TestProviderTestConnection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "01", "desc": "not found"})
	}))
	t.Cleanup(srv.Close)
	if err := newProvider(t, srv.URL).TestConnection(context.Background()); err != nil {
		t.Fatalf("test connection: %v", err)
	}

	if err := payos.New(payos.Config{}).TestConnection(context.Background()); !errors.Is(err, payos.ErrNotConfigured) {
		t.Fatalf("unconfigured test connection = %v, want ErrNotConfigured", err)
	}
}

func TestProviderConfirmWebhook(t *testing.T) {
	var path string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		body = readJSON(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "00", "desc": "success", "data": "ok"})
	}))
	t.Cleanup(srv.Close)

	if err := newProvider(t, srv.URL).ConfirmWebhook(context.Background(), "https://dsoft.example.com/api/v1/webhook/payos"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if path != "/confirm-webhook" || body["webhookUrl"] != "https://dsoft.example.com/api/v1/webhook/payos" {
		t.Fatalf("request = %s %v", path, body)
	}

	if err := payos.New(payos.Config{}).ConfirmWebhook(context.Background(), "https://example.com"); !errors.Is(err, payos.ErrNotConfigured) {
		t.Fatalf("unconfigured confirm = %v, want ErrNotConfigured", err)
	}
}
