package sepay_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/providers/sepay"
)

func TestGatewayCanonicalDropsEmptyAndZero(t *testing.T) {
	got := sepay.GatewayCanonical(map[string]string{
		"order_amount": "50000", "merchant": "KGTEST", "currency": "VND",
		"operation": "PURCHASE", "order_description": "Goi Pro", "order_invoice_number": "KG7F3K2Q",
		"customer_id": "", "payment_method": "", "success_url": "https://x/s", "error_url": "", "cancel_url": "",
	})
	want := "order_amount=50000,merchant=KGTEST,currency=VND,operation=PURCHASE,order_description=Goi Pro,order_invoice_number=KG7F3K2Q,success_url=https://x/s"
	if got != want {
		t.Fatalf("canonical = %q, want %q", got, want)
	}
	if sig := sepay.SignGateway("secret", got); sig == "" || sig == got {
		t.Fatalf("signature = %q", sig)
	}
}

const sepayIPNBody = `{"notification_type":"ORDER_PAID","order":{"order_invoice_number":"KG7F3K2Q","order_status":"CAPTURED","order_amount":"1190000.00","order_currency":"VND"},"transaction":{"id":"tx_1","transaction_status":"APPROVED","transaction_amount":"1190000.00"}}`

func TestParseIPN(t *testing.T) {
	h := http.Header{}
	h.Set("X-Secret-Key", "ipn_secret")

	ev, err := sepay.ParseIPN(h, []byte(sepayIPNBody), "ipn_secret")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.OrderCode != "KG7F3K2Q" || ev.AmountMinor != 1190000 || ev.Currency != "VND" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Status != domain.OrderPaid || ev.ProviderRef != "tx_1" || ev.TransferType != "in" {
		t.Fatalf("event = %+v", ev)
	}
	// Settlement dedupes on (provider, event_id) and rejects an empty
	// id, so the notification type must be part of the id: a later
	// TRANSACTION_VOID for the same transaction is a distinct event.
	if ev.ProviderEventID != "ORDER_PAID:tx_1" {
		t.Fatalf("event id = %q", ev.ProviderEventID)
	}

	// Missing or wrong X-Secret-Key is refused; an empty configured
	// secret must not accept an empty header either.
	if _, err := sepay.ParseIPN(http.Header{}, []byte(sepayIPNBody), "ipn_secret"); !errors.Is(err, sepay.ErrWebhookUnauthorized) {
		t.Fatalf("missing key err = %v", err)
	}
	bad := http.Header{}
	bad.Set("X-Secret-Key", "wrong")
	if _, err := sepay.ParseIPN(bad, []byte(sepayIPNBody), "ipn_secret"); !errors.Is(err, sepay.ErrWebhookUnauthorized) {
		t.Fatalf("wrong key err = %v", err)
	}
	if _, err := sepay.ParseIPN(http.Header{}, []byte(sepayIPNBody), ""); !errors.Is(err, sepay.ErrWebhookUnauthorized) {
		t.Fatalf("empty configured secret err = %v", err)
	}
}

func TestParseIPNNonPaidStatuses(t *testing.T) {
	h := http.Header{}
	h.Set("X-Secret-Key", "ipn_secret")

	// Not captured/approved yet: recorded as pending, falling back to
	// the order amount when the transaction amount is still zero.
	pending := `{"notification_type":"ORDER_PENDING","order":{"order_invoice_number":"KG7F3K2Q","order_status":"PENDING","order_amount":"50000.00","order_currency":"VND"},"transaction":{"id":"tx_2","transaction_status":"PENDING","transaction_amount":"0.00"}}`
	ev, err := sepay.ParseIPN(h, []byte(pending), "ipn_secret")
	if err != nil {
		t.Fatalf("parse pending: %v", err)
	}
	if ev.Status != "pending" || ev.AmountMinor != 50000 {
		t.Fatalf("pending event = %+v", ev)
	}

	voided := `{"notification_type":"TRANSACTION_VOID","order":{"order_invoice_number":"KG7F3K2Q","order_status":"CAPTURED","order_amount":"50000.00","order_currency":"VND"},"transaction":{"id":"tx_3","transaction_status":"APPROVED","transaction_amount":"50000.00"}}`
	ev, err = sepay.ParseIPN(h, []byte(voided), "ipn_secret")
	if err != nil || ev.Status != "voided" {
		t.Fatalf("void event = %+v, %v", ev, err)
	}
	if ev.ProviderEventID != "TRANSACTION_VOID:tx_3" {
		t.Fatalf("void event id = %q", ev.ProviderEventID)
	}

	if _, err := sepay.ParseIPN(h, []byte("{"), "ipn_secret"); err == nil {
		t.Fatal("malformed JSON accepted")
	}
}

func TestProviderParseIPN(t *testing.T) {
	p := sepay.New(sepay.Config{IPNSecret: "ipn_secret"})
	r := httptest.NewRequest(http.MethodPost, "/webhook/sepay-ipn", strings.NewReader(sepayIPNBody))
	r.Header.Set("X-Secret-Key", "ipn_secret")
	ev, err := p.ParseIPN(r, []byte(sepayIPNBody))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.OrderCode != "KG7F3K2Q" || ev.Status != domain.OrderPaid {
		t.Fatalf("event = %+v", ev)
	}
	missing := httptest.NewRequest(http.MethodPost, "/webhook/sepay-ipn", strings.NewReader(sepayIPNBody))
	if _, err := p.ParseIPN(missing, []byte(sepayIPNBody)); !errors.Is(err, sepay.ErrWebhookUnauthorized) {
		t.Fatalf("missing key err = %v", err)
	}
}

func TestClientTestConnection(t *testing.T) {
	var auth, uri string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, uri = r.Header.Get("Authorization"), r.URL.RequestURI()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &sepay.Client{BaseURL: srv.URL, APIToken: "token", HTTP: srv.Client()}
	if err := c.TestConnection(context.Background()); err != nil {
		t.Fatalf("test connection: %v", err)
	}
	if auth != "Bearer token" || uri != "/transactions?per_page=1" {
		t.Fatalf("auth/uri = %q/%q", auth, uri)
	}
}

func TestClientTestConnectionRejectsBadStatusAndMissingToken(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer bad.Close()
	c := &sepay.Client{BaseURL: bad.URL, APIToken: "revoked", HTTP: bad.Client()}
	if err := c.TestConnection(context.Background()); err == nil {
		t.Fatal("401 accepted")
	}

	// A missing token is reported without making a request at all.
	hit := false
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer empty.Close()
	c = &sepay.Client{BaseURL: empty.URL, HTTP: empty.Client()}
	if err := c.TestConnection(context.Background()); !errors.Is(err, sepay.ErrMissingAPIKey) {
		t.Fatalf("missing token err = %v", err)
	}
	if hit {
		t.Fatal("request sent without a token")
	}

	p := sepay.New(sepay.Config{})
	if err := p.TestConnection(context.Background()); !errors.Is(err, sepay.ErrMissingAPIKey) {
		t.Fatalf("provider test connection err = %v", err)
	}
}

const sepayTxBody = `{"transactions":[{"id":92704,"transaction_date":"2026-11-12T15:04:05+07:00","account_number":"0978482700","code":"KG7F3K2Q","content":"KG7F3K2Q chuyen tien","transfer_type":"in","transfer_amount":1190000,"reference_code":"FT24012345678"}]}`

func TestClientListTransactions(t *testing.T) {
	var q url.Values
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, auth = r.URL.Query(), r.Header.Get("Authorization")
		_, _ = io.WriteString(w, sepayTxBody)
	}))
	defer srv.Close()

	c := &sepay.Client{BaseURL: srv.URL, APIToken: "token", HTTP: srv.Client()}
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	txs, err := c.ListTransactions(context.Background(), from, to, 92703)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if auth != "Bearer token" {
		t.Fatalf("auth = %q", auth)
	}
	wantQ := map[string]string{
		"transaction_date_from": "2026-11-01 00:00:00",
		"transaction_date_to":   "2026-11-02 00:00:00",
		"transaction_date_sort": "asc",
		"timestamp_format":      "iso8601",
		"per_page":              "100",
		"since_id":              "92703",
	}
	for key, want := range wantQ {
		if got := q.Get(key); got != want {
			t.Fatalf("%s = %q, want %q (query %v)", key, got, want, q)
		}
	}
	if len(txs) != 1 {
		t.Fatalf("transactions = %+v", txs)
	}
	tx := txs[0]
	if tx.ID != 92704 || tx.AccountNumber != "0978482700" || tx.Code != "KG7F3K2Q" ||
		tx.Content != "KG7F3K2Q chuyen tien" || tx.TransferType != "in" ||
		tx.TransferAmount != 1190000 || tx.ReferenceCode != "FT24012345678" {
		t.Fatalf("transaction = %+v", tx)
	}
	wantTime := time.Date(2026, 11, 12, 15, 4, 5, 0, time.FixedZone("", 7*3600))
	if !tx.TransactionDate.Equal(wantTime) {
		t.Fatalf("transaction date = %v, want %v", tx.TransactionDate, wantTime)
	}

	// No since_id when the cursor is unset: SePay would otherwise
	// return nothing on the first reconciliation pass.
	if _, err := c.ListTransactions(context.Background(), from, to, 0); err != nil {
		t.Fatalf("list without cursor: %v", err)
	}
	if q.Has("since_id") {
		t.Fatalf("since_id = %q", q.Get("since_id"))
	}
}

func TestClientListTransactionsErrors(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusTooManyRequests, sepay.ErrRateLimited},
		{http.StatusInternalServerError, nil},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		c := &sepay.Client{BaseURL: srv.URL, APIToken: "token", HTTP: srv.Client()}
		_, err := c.ListTransactions(context.Background(), time.Now(), time.Now(), 0)
		srv.Close()
		if err == nil {
			t.Fatalf("status %d accepted", tc.status)
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Fatalf("status %d err = %v, want %v", tc.status, err, tc.want)
		}
	}
}
