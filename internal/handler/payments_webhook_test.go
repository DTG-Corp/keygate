package handler

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/providercfg"
	"github.com/tabloy/keygate/internal/payment/providers/payos"
	"github.com/tabloy/keygate/internal/payment/settlement"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
)

const (
	sepayWebhookSecret = "whsec_test"
	sepayIPNSecret     = "ipn_test_secret"
	payosChecksumKey   = "payos-checksum-key"
)

// webhookFixture is an API fixture with one live SePay row (product A
// and install default), one live PayOS row for product A, a settlement
// service, and a webhook subscription for product A that records the
// two paid events as delivery rows.
type webhookFixture struct {
	*apiFixture
	handler   *PaymentsWebhookHandler
	config    *providercfg.Service
	secret    string
	webhookID string
}

func newSepayWebhookFixture(t *testing.T) *webhookFixture {
	t.Helper()
	f := newAPIFixture(t)
	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	cfgSvc := providercfg.NewService(f.s, codec, "live")

	sepayCfg := providercfg.SepayConfig{
		BankName: "MB Bank", BankCode: "MB", BankBIN: "970422",
		AccountNo: "0978482700", AccountName: "NGUYEN VAN A",
		WebhookAuthMode: "hmac",
	}
	sepayCreds := providercfg.SepayCredentials{APIToken: "token", IPNSecret: sepayIPNSecret}
	// The product row proves the peeked order code selects the override;
	// the install-wide row is what verifies a transfer naming no order.
	for _, productID := range []string{f.prodA.ID, ""} {
		if _, err := cfgSvc.Upsert(f.ctx, providercfg.UpsertInput{
			Provider: "sepay", ProductID: productID, Mode: "live", Enabled: true,
			Config: sepayCfg, Credentials: sepayCreds, WebhookSecret: sepayWebhookSecret,
		}); err != nil {
			t.Fatalf("upsert sepay %q: %v", productID, err)
		}
	}
	if _, err := cfgSvc.Upsert(f.ctx, providercfg.UpsertInput{
		Provider: "payos", ProductID: f.prodA.ID, Mode: "live", Enabled: true,
		Config:        providercfg.PayOSConfig{ClientID: "client"},
		Credentials:   providercfg.PayOSCredentials{APIKey: "api-key", ChecksumKey: payosChecksumKey},
		WebhookSecret: "payos-webhook-secret",
	}); err != nil {
		t.Fatalf("upsert payos: %v", err)
	}

	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(receiver.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	webhookSvc := service.NewWebhookService(f.s, log, time.Second, 1, true)
	h := &PaymentsWebhookHandler{
		Store:      f.s,
		Config:     cfgSvc,
		Settlement: &settlement.Service{Store: f.s, Webhooks: webhookSvc, Log: log},
		Log:        log,
	}
	hook := &model.Webhook{
		ProductID: f.prodA.ID, URL: receiver.URL, Secret: "whsec_hook", Active: true,
		Events: []string{model.EventOrderPaid, model.EventPaymentSucceeded},
	}
	if err := f.s.CreateWebhook(f.ctx, hook); err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	return &webhookFixture{apiFixture: f, handler: h, config: cfgSvc, secret: sepayWebhookSecret, webhookID: hook.ID}
}

func (fx *webhookFixture) newOrder(t *testing.T, kind, providerName string) *model.PaymentOrder {
	t.Helper()
	return fx.newOrderOn(t, fx.prodA.ID, fx.planA.ID, kind, providerName)
}

func (fx *webhookFixture) newOrderOn(t *testing.T, productID, planID, kind, providerName string) *model.PaymentOrder {
	t.Helper()
	order, err := fx.s.CreatePaymentOrder(fx.ctx, store.CreateOrderInput{
		ProductID: productID, PlanID: planID,
		Email: "buyer-" + uuid.NewString()[:8] + "@example.com",
		Kind:  kind, Quantity: 1,
		SubtotalMinor: 1190000, TotalMinor: 1190000, Currency: "VND",
		Provider:  providerName,
		ExpiresAt: time.Now().Add(time.Hour),
		Buyer:     []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	return order
}

func (fx *webhookFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := fx.s.DB.NewRaw(query, args...).Scan(fx.ctx, &n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func (fx *webhookFixture) deliveryCount(t *testing.T, event string) int {
	t.Helper()
	return fx.count(t, "SELECT count(*) FROM webhook_deliveries WHERE webhook_id = ? AND event = ?", fx.webhookID, event)
}

// newSePayEventID returns an id no earlier test run has recorded: both
// payment_events and payment_transactions dedupe on it, and the
// integration database outlives one run.
func newSePayEventID() int64 { return time.Now().UnixNano() }

func sepayBankBody(orderCode string, id int64, transferType string) []byte {
	return fmt.Appendf(nil, `{"id":%d,"gateway":"MB","transactionDate":"2026-11-12 15:04:05","accountNumber":"0978482700","subAccount":"","code":%q,"content":%q,"transferType":%q,"description":"x","transferAmount":1190000,"accumulated":0,"referenceCode":"FT%d"}`,
		id, orderCode, orderCode+" chuyen tien", transferType, id)
}

func (fx *webhookFixture) postSePayRaw(body []byte, timestamp, signature string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/webhook/sepay", bytes.NewReader(body))
	c.Request.Header.Set("X-SePay-Timestamp", timestamp)
	c.Request.Header.Set("X-SePay-Signature", signature)
	fx.handler.HandleSePay(c)
	// status-only writes are buffered by Gin's responseWriter until a
	// body is written; the handler is called directly, so read the
	// status Gin recorded rather than the recorder's default 200.
	w.Code = c.Writer.Status()
	return w
}

func (fx *webhookFixture) postSePay(t *testing.T, body []byte, secret string) *httptest.ResponseRecorder {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(ts + "." + string(body)))
	return fx.postSePayRaw(body, ts, "sha256="+hex.EncodeToString(mac.Sum(nil)))
}

// A SePay bank webhook is verified, recorded once, settled once and
// acked with exactly the body SePay requires. The provider's retry gets
// the same ack and changes nothing.
func TestSePayWebhookAcksAndSettlesOnce(t *testing.T) {
	fx := newSepayWebhookFixture(t)
	order := fx.newOrder(t, "new", "sepay")
	eventID := newSePayEventID()
	body := sepayBankBody(order.Code, eventID, "in")

	first := fx.postSePay(t, body, fx.secret)
	if first.Code != http.StatusOK {
		t.Fatalf("first ack = %d: %s", first.Code, first.Body.String())
	}
	// SePay retries a delivery whose body is anything but this object.
	if got := first.Body.String(); got != `{"success":true}` {
		t.Fatalf("ack body = %q, want exact success object", got)
	}

	second := fx.postSePay(t, body, fx.secret)
	if second.Code != http.StatusOK {
		t.Fatalf("retry ack = %d: %s", second.Code, second.Body.String())
	}
	if second.Body.String() != first.Body.String() {
		t.Fatalf("retry ack body = %q, want %q", second.Body.String(), first.Body.String())
	}

	got, err := fx.s.GetPaymentOrderByCode(fx.ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OrderPaid || got.PaidAt == nil {
		t.Fatalf("order = status %q paid_at %v, want paid", got.Status, got.PaidAt)
	}
	if n := fx.count(t, "SELECT count(*) FROM licenses WHERE email = ?", order.Email); n != 1 {
		t.Fatalf("licences = %d, want 1", n)
	}
	// The event claim survives exactly once and the transfer is on the
	// ledger as matched.
	if n := fx.count(t, "SELECT count(*) FROM payment_events WHERE provider = 'sepay' AND event_id = ?", strconv.FormatInt(eventID, 10)); n != 1 {
		t.Fatalf("payment_events rows = %d, want 1", n)
	}
	if n := fx.count(t, "SELECT count(*) FROM payment_transactions WHERE provider = 'sepay' AND provider_ref = ? AND status = ?",
		strconv.FormatInt(eventID, 10), domain.TxMatched); n != 1 {
		t.Fatalf("matched transactions = %d, want 1", n)
	}
	// One payment.succeeded and one order.paid, emitted once despite the
	// retry.
	if n := fx.deliveryCount(t, model.EventPaymentSucceeded); n != 1 {
		t.Fatalf("payment.succeeded deliveries = %d, want 1", n)
	}
	if n := fx.deliveryCount(t, model.EventOrderPaid); n != 1 {
		t.Fatalf("order.paid deliveries = %d, want 1", n)
	}
}

func TestSePayWebhookBadSignatureIs401(t *testing.T) {
	fx := newSepayWebhookFixture(t)
	order := fx.newOrder(t, "new", "sepay")
	body := sepayBankBody(order.Code, newSePayEventID(), "in")

	w := fx.postSePayRaw(body, strconv.FormatInt(time.Now().Unix(), 10), "sha256=deadbeef")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", w.Code, w.Body.String())
	}
	got, err := fx.s.GetPaymentOrderByCode(fx.ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OrderPending {
		t.Fatalf("order status = %q, want pending", got.Status)
	}
	if n := fx.count(t, "SELECT count(*) FROM payment_events WHERE provider = 'sepay' AND order_id = ?", order.ID); n != 0 {
		t.Fatalf("payment_events written for a rejected delivery: %d", n)
	}
}

// The HMAC is over the raw bytes. A body with formatting Go would
// normalise on a re-marshal only verifies when the original bytes are
// the ones handed to the adapter.
func TestSePayWebhookVerifiesRawBody(t *testing.T) {
	fx := newSepayWebhookFixture(t)
	order := fx.newOrder(t, "new", "sepay")
	eventID := newSePayEventID()
	body := []byte(fmt.Sprintf("{\n  \"transferType\" : \"in\",\n  \"id\": %d,\n  \"code\":%q,\n  \"content\": %q,\n  \"transferAmount\": 1190000,\n  \"gateway\": \"MB\"\n}",
		eventID, order.Code, order.Code+" chuyen tien"))

	w := fx.postSePay(t, body, fx.secret)
	if w.Code != http.StatusOK {
		t.Fatalf("raw-body delivery = %d: %s", w.Code, w.Body.String())
	}
	got, err := fx.s.GetPaymentOrderByCode(fx.ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OrderPaid {
		t.Fatalf("order status = %q, want paid", got.Status)
	}
}

// A signed body that is not JSON is the caller's mistake, not a retryable
// provider fault: 400 rather than 401 or 500.
func TestSePayWebhookMalformedBodyIs400(t *testing.T) {
	fx := newSepayWebhookFixture(t)
	w := fx.postSePay(t, []byte(`{"id":`), fx.secret)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// An oversized body is refused before it is parsed or buffered whole.
func TestSePayWebhookOversizedBodyIs413(t *testing.T) {
	fx := newSepayWebhookFixture(t)
	body := append([]byte(`{"id":1,"pad":"`), bytes.Repeat([]byte("a"), maxWebhookBody)...)
	body = append(body, []byte(`"}`)...)
	w := fx.postSePay(t, body, fx.secret)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
}

// A transfer whose memo names no order is normal (a manual transfer with
// the code typed wrong). It is acked and kept for the operator, not
// 500-looped back to SePay.
func TestSePayWebhookUnknownOrderAcksAndRecordsUnmatched(t *testing.T) {
	fx := newSepayWebhookFixture(t)
	code := domain.NewOrderCode()
	eventID := newSePayEventID()

	w := fx.postSePay(t, sepayBankBody(code, eventID, "in"), fx.secret)
	if w.Code != http.StatusOK || w.Body.String() != `{"success":true}` {
		t.Fatalf("ack = %d %q", w.Code, w.Body.String())
	}
	if n := fx.count(t, "SELECT count(*) FROM payment_transactions WHERE provider = 'sepay' AND provider_ref = ? AND status = ?",
		strconv.FormatInt(eventID, 10), domain.TxUnmatched); n != 1 {
		t.Fatalf("unmatched transactions = %d, want 1", n)
	}
	if n := fx.count(t, "SELECT count(*) FROM payment_transactions WHERE provider = 'sepay' AND provider_ref = ? AND order_id IS NULL",
		strconv.FormatInt(eventID, 10)); n != 1 {
		t.Fatalf("unmatched transaction should name no order")
	}
	if n := fx.count(t, "SELECT count(*) FROM payment_events WHERE provider = 'sepay' AND event_id = ?", strconv.FormatInt(eventID, 10)); n != 0 {
		t.Fatalf("payment_events rows = %d, want 0 for an orderless event", n)
	}
}

// SePay sends refunds/payouts to the same webhook. An `out` transfer is
// recorded but never settles an order.
func TestSePayWebhookOutTransferIsRecordedNotSettled(t *testing.T) {
	fx := newSepayWebhookFixture(t)
	order := fx.newOrder(t, "new", "sepay")
	eventID := newSePayEventID()

	w := fx.postSePay(t, sepayBankBody(order.Code, eventID, "out"), fx.secret)
	if w.Code != http.StatusOK {
		t.Fatalf("ack = %d: %s", w.Code, w.Body.String())
	}
	got, err := fx.s.GetPaymentOrderByCode(fx.ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OrderPending {
		t.Fatalf("order status = %q, want pending (an out transfer must not settle)", got.Status)
	}
	if n := fx.count(t, "SELECT count(*) FROM payment_transactions WHERE provider = 'sepay' AND provider_ref = ? AND status = ?",
		strconv.FormatInt(eventID, 10), domain.TxUnmatched); n != 1 {
		t.Fatalf("out transfer rows = %d, want 1 unmatched", n)
	}
	if n := fx.count(t, "SELECT count(*) FROM licenses WHERE email = ?", order.Email); n != 0 {
		t.Fatalf("licences = %d, want 0", n)
	}
	if n := fx.deliveryCount(t, model.EventPaymentSucceeded) + fx.deliveryCount(t, model.EventOrderPaid); n != 0 {
		t.Fatalf("events emitted for an out transfer: %d", n)
	}
}

// Money for an order that already expired or was canceled is recorded
// for an operator, grants nothing and emits nothing.
func TestSePayWebhookPaymentOnClosedOrderOnlyRecordsTransaction(t *testing.T) {
	for _, status := range []string{domain.OrderExpired, domain.OrderCanceled} {
		t.Run(status, func(t *testing.T) {
			fx := newSepayWebhookFixture(t)
			order := fx.newOrder(t, "new", "sepay")
			if _, err := fx.s.DB.NewRaw("UPDATE payment_orders SET status = ? WHERE id = ?", status, order.ID).Exec(fx.ctx); err != nil {
				t.Fatal(err)
			}
			eventID := newSePayEventID()

			w := fx.postSePay(t, sepayBankBody(order.Code, eventID, "in"), fx.secret)
			if w.Code != http.StatusOK {
				t.Fatalf("ack = %d: %s", w.Code, w.Body.String())
			}
			got, err := fx.s.GetPaymentOrderByCode(fx.ctx, order.Code)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != status {
				t.Fatalf("order status = %q, want %q", got.Status, status)
			}
			if n := fx.count(t, "SELECT count(*) FROM payment_transactions WHERE provider = 'sepay' AND provider_ref = ? AND status = ?",
				strconv.FormatInt(eventID, 10), domain.TxUnmatched); n != 1 {
				t.Fatalf("late payment rows = %d, want 1 unmatched", n)
			}
			if n := fx.count(t, "SELECT count(*) FROM licenses WHERE email = ?", order.Email); n != 0 {
				t.Fatalf("licences = %d, want 0", n)
			}
			if n := fx.deliveryCount(t, model.EventPaymentSucceeded) + fx.deliveryCount(t, model.EventOrderPaid); n != 0 {
				t.Fatalf("events emitted for a closed order: %d", n)
			}
		})
	}
}

// A settlement failure answers 500 so the provider retries. The event
// claim must be released, otherwise the retry would be mistaken for a
// duplicate and acked while the order stays unsettled.
func TestSePayWebhookFailedSettlementReleasesEventClaim(t *testing.T) {
	fx := newSepayWebhookFixture(t)
	// "upgrade" has no settlement path in phase 1a: Settle refuses it
	// without granting anything.
	order := fx.newOrder(t, "upgrade", "sepay")
	eventID := newSePayEventID()
	body := sepayBankBody(order.Code, eventID, "in")

	if w := fx.postSePay(t, body, fx.secret); w.Code != http.StatusInternalServerError {
		t.Fatalf("failed settle = %d, want 500: %s", w.Code, w.Body.String())
	}
	if n := fx.count(t, "SELECT count(*) FROM payment_events WHERE provider = 'sepay' AND event_id = ?", strconv.FormatInt(eventID, 10)); n != 0 {
		t.Fatalf("payment_events rows after a failed settle = %d, want 0", n)
	}
	// The retry runs settlement again instead of being acked as a
	// duplicate.
	if w := fx.postSePay(t, body, fx.secret); w.Code != http.StatusInternalServerError {
		t.Fatalf("retry after failed settle = %d, want 500", w.Code)
	}
	got, err := fx.s.GetPaymentOrderByCode(fx.ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OrderPending {
		t.Fatalf("order status = %q, want pending", got.Status)
	}
}

// A live config may never accept unsigned deliveries. The write path
// refuses the combination, so the row is inserted directly to prove the
// runtime guard still answers 503 (and parses nothing) when such a row
// exists from before the validation.
func TestSePayWebhookLiveNoneAuthIs503(t *testing.T) {
	fx := newSepayWebhookFixture(t)
	// Product B has no provider row yet, so this creates rather than
	// updates the slot (an update would keep the old id and the
	// ciphertext sealed against a discarded one).
	order := fx.newOrderOn(t, fx.prodB.ID, fx.planB.ID, "new", "sepay")

	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	id := store.NewID()
	creds, err := codec.Seal(id, "credentials", []byte(`{"api_token":"token"}`))
	if err != nil {
		t.Fatal(err)
	}
	secret, err := codec.Seal(id, "webhook_secret", []byte(sepayWebhookSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.s.UpsertPaymentProviderConfig(fx.ctx, store.ProviderConfigInput{
		ID: id, Provider: "sepay", ProductID: fx.prodB.ID, Mode: "live", Enabled: true,
		Config:                 []byte(`{"bank_name":"MB Bank","bank_code":"MB","account_no":"0978482700","webhook_auth_mode":"none"}`),
		CredentialsEncrypted:   creds,
		WebhookSecretEncrypted: secret,
	}); err != nil {
		t.Fatalf("seed live+none row: %v", err)
	}

	// No timestamp or signature: mode "none" would accept this.
	w := fx.postSePayRaw(sepayBankBody(order.Code, newSePayEventID(), "in"), "", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
	}
	got, err := fx.s.GetPaymentOrderByCode(fx.ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OrderPending {
		t.Fatalf("order status = %q, want pending", got.Status)
	}
	if n := fx.count(t, "SELECT count(*) FROM payment_transactions WHERE provider = 'sepay' AND order_id = ?", order.ID); n != 0 {
		t.Fatalf("transactions written for a refused config: %d", n)
	}
}

func TestLiveNoneWebhookGuard(t *testing.T) {
	cases := []struct {
		name string
		cfg  *providercfg.ResolvedConfig
		want bool
	}{
		{"live none", &providercfg.ResolvedConfig{Mode: "live", WebhookAuthMode: "none"}, true},
		{"live NONE", &providercfg.ResolvedConfig{Mode: "live", WebhookAuthMode: " NONE "}, true},
		{"live hmac", &providercfg.ResolvedConfig{Mode: "live", WebhookAuthMode: "hmac"}, false},
		{"live apikey", &providercfg.ResolvedConfig{Mode: "live", WebhookAuthMode: "apikey"}, false},
		{"test none", &providercfg.ResolvedConfig{Mode: "test", WebhookAuthMode: "none"}, false},
		{"nil config", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := liveUnsignedWebhook(tc.cfg); got != tc.want {
				t.Fatalf("liveUnsignedWebhook(%+v) = %t, want %t", tc.cfg, got, tc.want)
			}
		})
	}
}

// The SePay hosted-gateway IPN shares the ingest path but authenticates
// with X-Secret-Key and answers 200 with no body.
func TestSePayIPNWebhookVerifiesSecretAndSettles(t *testing.T) {
	fx := newSepayWebhookFixture(t)
	order := fx.newOrder(t, "new", "sepay")
	txnID := "txn-" + uuid.NewString()[:8]
	body := fmt.Appendf(nil, `{"timestamp":%d,"notification_type":"ORDER_PAID","order":{"order_invoice_number":%q,"order_status":"CAPTURED","order_amount":"1190000","order_currency":"VND"},"transaction":{"id":%q,"transaction_status":"APPROVED","transaction_amount":"1190000"}}`,
		time.Now().Unix(), order.Code, txnID)

	post := func(secret string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/webhook/sepay-ipn", bytes.NewReader(body))
		c.Request.Header.Set("X-Secret-Key", secret)
		fx.handler.HandleSePayIPN(c)
		w.Code = c.Writer.Status()
		return w
	}

	if w := post("wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad IPN key = %d, want 401", w.Code)
	}
	w := post(sepayIPNSecret)
	if w.Code != http.StatusOK {
		t.Fatalf("IPN ack = %d: %s", w.Code, w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Fatalf("IPN ack body = %q, want empty", w.Body.String())
	}
	got, err := fx.s.GetPaymentOrderByCode(fx.ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OrderPaid {
		t.Fatalf("order status = %q, want paid", got.Status)
	}
	if n := fx.count(t, "SELECT count(*) FROM licenses WHERE email = ?", order.Email); n != 1 {
		t.Fatalf("licences = %d, want 1", n)
	}
}

// PayOS signs the data object, not the envelope; the checksum is over the
// raw body's decoded data and a valid delivery settles the order. The
// provider gets a 2xx JSON ack.
func TestPayOSWebhookVerifiesChecksumAndSettles(t *testing.T) {
	fx := newSepayWebhookFixture(t)
	order := fx.newOrder(t, "new", "payos")
	if order.NumericCode == 0 {
		t.Fatal("order has no numeric code")
	}
	reference := "FT-" + uuid.NewString()[:8]
	data := map[string]any{
		"orderCode": order.NumericCode, "amount": int64(1190000),
		"description": order.Code, "currency": "VND",
		"paymentLinkId": "plink-" + reference, "code": "00",
		"transactionDateTime": "2026-11-12 15:04:05", "reference": reference,
	}
	body, err := json.Marshal(map[string]any{
		"code": "00", "desc": "success", "success": true, "data": data,
		"signature": payos.SignWebhook(payosChecksumKey, data),
	})
	if err != nil {
		t.Fatal(err)
	}

	post := func(payload []byte) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/webhook/payos", bytes.NewReader(payload))
		fx.handler.HandlePayOS(c)
		w.Code = c.Writer.Status()
		return w
	}

	first := post(body)
	if first.Code != http.StatusOK || first.Body.String() != `{"success":true}` {
		t.Fatalf("payos ack = %d %q", first.Code, first.Body.String())
	}
	if second := post(body); second.Code != http.StatusOK {
		t.Fatalf("payos retry ack = %d: %s", second.Code, second.Body.String())
	}
	got, err := fx.s.GetPaymentOrderByCode(fx.ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OrderPaid {
		t.Fatalf("order status = %q, want paid", got.Status)
	}
	if n := fx.count(t, "SELECT count(*) FROM licenses WHERE email = ?", order.Email); n != 1 {
		t.Fatalf("licences = %d, want 1", n)
	}
	if n := fx.count(t, "SELECT count(*) FROM payment_events WHERE provider = 'payos' AND event_id = ?", reference); n != 1 {
		t.Fatalf("payos payment_events rows = %d, want 1", n)
	}

	bad := bytes.Replace(body, []byte(`"signature":"`), []byte(`"signature":"dead`), 1)
	if w := post(bad); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad checksum = %d, want 401", w.Code)
	}
}
