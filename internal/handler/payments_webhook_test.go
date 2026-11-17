package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/tabloy/keygate/internal/middleware"
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

// webhookFixture is an API fixture with live SePay rows, a live PayOS
// row, a settlement service, and a webhook subscription for product A
// that records the two paid events as delivery rows. The two-account
// variant holds a row for product A and the install default; the
// default-only variant holds the install-wide rows alone.
type webhookFixture struct {
	*apiFixture
	handler   *PaymentsWebhookHandler
	config    *providercfg.Service
	secret    string
	webhookID string
}

func newSepayWebhookFixture(t *testing.T) *webhookFixture {
	t.Helper()
	return newWebhookFixture(t, true)
}

// newDefaultOnlyWebhookFixture is a single-account install: the
// install-wide (product_id NULL) SePay and PayOS rows exist and no
// product has a row of its own, so every order resolves to the default.
func newDefaultOnlyWebhookFixture(t *testing.T) *webhookFixture {
	t.Helper()
	return newWebhookFixture(t, false)
}

func newWebhookFixture(t *testing.T, perProduct bool) *webhookFixture {
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
	sepayProductIDs := []string{""}
	payosProductID := ""
	if perProduct {
		sepayProductIDs = append(sepayProductIDs, f.prodA.ID)
		payosProductID = f.prodA.ID
	}
	// Register the cleanup before the first write, so a setup failure
	// half-way through still leaves the database clean. The install-wide
	// slots are always listed: even when this variant does not write one,
	// deleting the slot removes a row an earlier run left there, and that
	// row would otherwise be sealed with this package's codec and break
	// another package resolving the same install default.
	slots := []providerConfigSlot{
		{Provider: "sepay", Mode: "live"},
		{Provider: "payos", Mode: "live"},
	}
	if perProduct {
		slots = append(slots,
			providerConfigSlot{Provider: "sepay", ProductID: f.prodA.ID, Mode: "live"},
			providerConfigSlot{Provider: "payos", ProductID: f.prodA.ID, Mode: "live"},
		)
	}
	f.cleanupProviderConfigSlots(t, slots...)
	for _, productID := range sepayProductIDs {
		if _, err := cfgSvc.Upsert(f.ctx, providercfg.UpsertInput{
			Provider: "sepay", ProductID: productID, Mode: "live", Enabled: true,
			Config: sepayCfg, Credentials: sepayCreds, WebhookSecret: sepayWebhookSecret,
		}); err != nil {
			t.Fatalf("upsert sepay %q: %v", productID, err)
		}
	}
	if _, err := cfgSvc.Upsert(f.ctx, providercfg.UpsertInput{
		Provider: "payos", ProductID: payosProductID, Mode: "live", Enabled: true,
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

// The integration database is shared by every package and outlives one
// run. An install-wide provider row this package sealed must therefore
// not survive the fixture that wrote it: providercfg resolves the same
// slot with a different master key and would report ErrProviderConfigInvalid
// instead of falling back to "not configured", which is how the default-only
// webhook fixture used to turn the full suite red.
func TestWebhookFixturesLeaveNoInstallWideProviderRows(t *testing.T) {
	f := newAPIFixture(t)
	assertGone := func(provider, productID, mode string) {
		t.Helper()
		row, err := f.s.FindPaymentProviderConfig(f.ctx, provider, productID, mode)
		if !errors.Is(err, store.ErrProviderConfigNotFound) {
			t.Fatalf("%s/%q/%s survived fixture cleanup: row=%+v err=%v", provider, productID, mode, row, err)
		}
	}

	t.Run("default-only fixture", func(t *testing.T) {
		newDefaultOnlyWebhookFixture(t)
	})
	assertGone("sepay", "", "live")
	assertGone("payos", "", "live")

	t.Run("per-product fixture", func(t *testing.T) {
		fx := newSepayWebhookFixture(t)
		fx.addSepayProduct(t, fx.prodB.ID, "whsec_cleanup_b")
	})
	assertGone("sepay", "", "live")
	assertGone("sepay", f.prodB.ID, "live")
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

// eventIDFor is the account-scoped event key the handler records: a
// product's own row scopes the provider event id, the install default
// keeps it bare.
func eventIDFor(productID string, id int64) string {
	if productID == "" {
		return strconv.FormatInt(id, 10)
	}
	return productID + ":" + strconv.FormatInt(id, 10)
}

// addSepayProduct adds a live SePay row for one product with its own
// webhook secret, so a test can hold two accounts in one install.
func (fx *webhookFixture) addSepayProduct(t *testing.T, productID, secret string) {
	t.Helper()
	fx.cleanupProviderConfigSlots(t, providerConfigSlot{Provider: "sepay", ProductID: productID, Mode: "live"})
	if _, err := fx.config.Upsert(fx.ctx, providercfg.UpsertInput{
		Provider: "sepay", ProductID: productID, Mode: "live", Enabled: true,
		Config: providercfg.SepayConfig{
			BankName: "MB Bank", BankCode: "MB", BankBIN: "970422",
			AccountNo: "0978482700", AccountName: "NGUYEN VAN A", WebhookAuthMode: "hmac",
		},
		Credentials:   providercfg.SepayCredentials{APIToken: "token-" + productID},
		WebhookSecret: secret,
	}); err != nil {
		t.Fatalf("upsert sepay row for product %s: %v", productID, err)
	}
}

// sepaySplitBody names one order where the handler's unverified peek
// reads a code (data.description) and another where the SePay adapter
// reads one (content). The full exploit is a body like this signed with
// the first product's secret: verification succeeds against that
// account, while settlement follows the second code.
func sepaySplitBody(peekCode, adapterCode string, id int64) []byte {
	return fmt.Appendf(nil, `{"id":%d,"gateway":"MB","transactionDate":"2026-11-12 15:04:05","accountNumber":"0978482700","subAccount":"","content":%q,"transferType":"in","description":"x","transferAmount":1190000,"accumulated":0,"referenceCode":"FT%d","data":{"description":%q}}`,
		id, adapterCode+" chuyen tien", id, peekCode)
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
	if n := fx.count(t, "SELECT count(*) FROM payment_events WHERE provider = 'sepay' AND event_id = ?", eventIDFor(fx.prodA.ID, eventID)); n != 1 {
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
	// The documented 401 carries PAYMENT_WEBHOOK_UNAUTHORIZED; monitoring
	// branches on the code, not only the status.
	if got := w.Body.String(); !strings.Contains(got, `"code":"PAYMENT_WEBHOOK_UNAUTHORIZED"`) {
		t.Fatalf("401 body = %q, want the PAYMENT_WEBHOOK_UNAUTHORIZED code", got)
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

// The cap is the spec's 256 KiB, enforced on the real route shape: the
// BodyLimit middleware refuses a declared oversized length, and a body
// that arrives without a length stops at the middleware's reader and the
// handler answers 400 instead of buffering it.
func TestSePayWebhookBodyLimitThroughRouter(t *testing.T) {
	if maxWebhookBody != 256<<10 {
		t.Fatalf("maxWebhookBody = %d, want 256 KiB (spec §7)", maxWebhookBody)
	}
	fx := newSepayWebhookFixture(t)
	router := gin.New()
	router.Use(middleware.BodyLimit(maxWebhookBody))
	router.POST("/api/v1/webhook/sepay", fx.handler.HandleSePay)

	oversized := append([]byte(`{"id":1,"pad":"`), bytes.Repeat([]byte("a"), maxWebhookBody)...)
	oversized = append(oversized, []byte(`"}`)...)

	t.Run("declared content length", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhook/sepay", bytes.NewReader(oversized))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413: %s", w.Code, w.Body.String())
		}
	})
	t.Run("streamed body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhook/sepay", bytes.NewReader(oversized))
		req.ContentLength = -1
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
	})
}

// A database fault during the order lookup answers 500, not "no such
// order": classifying a known delivery as unmatched would hide the fault
// and give up the provider's retry.
func TestSePayWebhookOrderLookupErrorIs500(t *testing.T) {
	fx := newSepayWebhookFixture(t)
	order := fx.newOrder(t, "new", "sepay")
	body := sepayBankBody(order.Code, newSePayEventID(), "in")
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(fx.secret))
	_, _ = mac.Write([]byte(ts + "." + string(body)))

	ctx, cancel := context.WithCancel(fx.ctx)
	cancel()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/webhook/sepay", bytes.NewReader(body)).WithContext(ctx)
	c.Request.Header.Set("X-SePay-Timestamp", ts)
	c.Request.Header.Set("X-SePay-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	fx.handler.HandleSePay(c)
	w.Code = c.Writer.Status()
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
	if n := fx.count(t, "SELECT count(*) FROM payment_transactions WHERE order_id = ?", order.ID); n != 0 {
		t.Fatalf("transactions written after a read fault: %d", n)
	}
}

// A transfer whose memo names no order is normal (a manual transfer with
// the code typed wrong). It is acked and kept for the operator, not
// 500-looped back to SePay. The verified delivery is still an audit row
// (§7.3), and its redelivery changes nothing.
func TestSePayWebhookUnknownOrderAcksAndRecordsUnmatched(t *testing.T) {
	fx := newSepayWebhookFixture(t)
	code := domain.NewOrderCode()
	eventID := newSePayEventID()
	body := sepayBankBody(code, eventID, "in")

	w := fx.postSePay(t, body, fx.secret)
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
	// The default config verifies an orderless transfer, so the audit
	// row keeps the bare provider id (no account scope).
	if n := fx.count(t, "SELECT count(*) FROM payment_events WHERE provider = 'sepay' AND event_id = ? AND order_id IS NULL",
		strconv.FormatInt(eventID, 10)); n != 1 {
		t.Fatalf("payment_events rows = %d, want 1 for an orderless event", n)
	}

	if retry := fx.postSePay(t, body, fx.secret); retry.Code != http.StatusOK {
		t.Fatalf("retry ack = %d: %s", retry.Code, retry.Body.String())
	}
	if n := fx.count(t, "SELECT count(*) FROM payment_events WHERE provider = 'sepay' AND event_id = ?", strconv.FormatInt(eventID, 10)); n != 1 {
		t.Fatalf("payment_events rows after retry = %d, want 1", n)
	}
	if n := fx.count(t, "SELECT count(*) FROM payment_transactions WHERE provider = 'sepay' AND provider_ref = ?", strconv.FormatInt(eventID, 10)); n != 1 {
		t.Fatalf("transactions after retry = %d, want 1", n)
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
	// The out transfer is a verified delivery too: it belongs in the
	// audit trail, scoped to the account that signed it.
	if n := fx.count(t, "SELECT count(*) FROM payment_events WHERE provider = 'sepay' AND event_id = ? AND order_id = ?",
		eventIDFor(fx.prodA.ID, eventID), order.ID); n != 1 {
		t.Fatalf("out transfer audit rows = %d, want 1", n)
	}

	retry := fx.postSePay(t, sepayBankBody(order.Code, eventID, "out"), fx.secret)
	if retry.Code != http.StatusOK {
		t.Fatalf("out retry ack = %d: %s", retry.Code, retry.Body.String())
	}
	if n := fx.count(t, "SELECT count(*) FROM payment_events WHERE provider = 'sepay' AND event_id = ?", eventIDFor(fx.prodA.ID, eventID)); n != 1 {
		t.Fatalf("out audit rows after retry = %d, want 1", n)
	}
	if n := fx.count(t, "SELECT count(*) FROM payment_transactions WHERE provider = 'sepay' AND provider_ref = ?", strconv.FormatInt(eventID, 10)); n != 1 {
		t.Fatalf("out transactions after retry = %d, want 1", n)
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

// A claim can exist while its order is still pending: the process died
// between the claim and settlement, or the request context was canceled
// mid-settlement. The redelivery must settle the pending order rather
// than ack it as already handled.
func TestSePayWebhookDuplicateClaimForPendingOrderSettles(t *testing.T) {
	fx := newSepayWebhookFixture(t)
	order := fx.newOrder(t, "new", "sepay")
	eventID := newSePayEventID()
	body := sepayBankBody(order.Code, eventID, "in")
	key := eventIDFor(fx.prodA.ID, eventID)
	// Exactly the row a crash after the claim and before settlement
	// leaves behind.
	if _, err := fx.s.RecordPaymentEvent(fx.ctx, nil, store.PaymentEventInput{
		Provider: "sepay", EventID: key, OrderID: order.ID,
		Kind: domain.OrderPaid, Raw: body, OccurredAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed event claim: %v", err)
	}

	w := fx.postSePay(t, body, fx.secret)
	if w.Code != http.StatusOK {
		t.Fatalf("duplicate claim delivery = %d: %s", w.Code, w.Body.String())
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
	if n := fx.count(t, "SELECT count(*) FROM payment_events WHERE provider = 'sepay' AND event_id = ?", key); n != 1 {
		t.Fatalf("payment_events rows = %d, want 1", n)
	}
	if n := fx.deliveryCount(t, model.EventPaymentSucceeded); n != 1 {
		t.Fatalf("payment.succeeded deliveries = %d, want 1", n)
	}
}

// A failed settlement answers 500 and keeps the claim. The provider's
// retry then sees a duplicate for a still-pending order, settles it, and
// answers 200: one licence, one event row, one emission.
func TestSePayWebhookFailedSettlementRetriedOnDuplicate(t *testing.T) {
	fx := newSepayWebhookFixture(t)
	order := fx.newOrder(t, "new", "sepay")
	eventID := newSePayEventID()
	body := sepayBankBody(order.Code, eventID, "in")
	key := eventIDFor(fx.prodA.ID, eventID)

	// An inactive plan makes the grant fail without breaking the
	// database, so the test can fix the cause and retry.
	if _, err := fx.s.DB.NewRaw("UPDATE plans SET active = false WHERE id = ?", order.PlanID).Exec(fx.ctx); err != nil {
		t.Fatal(err)
	}
	if w := fx.postSePay(t, body, fx.secret); w.Code != http.StatusInternalServerError {
		t.Fatalf("failed settle = %d, want 500: %s", w.Code, w.Body.String())
	}
	// The claim stays: the retry path, not a release, is what makes the
	// redelivery settle.
	if n := fx.count(t, "SELECT count(*) FROM payment_events WHERE provider = 'sepay' AND event_id = ?", key); n != 1 {
		t.Fatalf("payment_events rows after a failed settle = %d, want 1 (claim kept)", n)
	}
	if n := fx.count(t, "SELECT count(*) FROM licenses WHERE email = ?", order.Email); n != 0 {
		t.Fatalf("licences after a failed settle = %d, want 0", n)
	}

	if _, err := fx.s.DB.NewRaw("UPDATE plans SET active = true WHERE id = ?", order.PlanID).Exec(fx.ctx); err != nil {
		t.Fatal(err)
	}
	if w := fx.postSePay(t, body, fx.secret); w.Code != http.StatusOK {
		t.Fatalf("retry after fixed cause = %d, want 200: %s", w.Code, w.Body.String())
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
	if n := fx.count(t, "SELECT count(*) FROM payment_events WHERE provider = 'sepay' AND event_id = ?", key); n != 1 {
		t.Fatalf("payment_events rows after retry = %d, want 1 (no duplicate audit row)", n)
	}
	if n := fx.deliveryCount(t, model.EventPaymentSucceeded); n != 1 {
		t.Fatalf("payment.succeeded deliveries = %d, want 1", n)
	}
	if n := fx.deliveryCount(t, model.EventOrderPaid); n != 1 {
		t.Fatalf("order.paid deliveries = %d, want 1", n)
	}
}

// providercfg rejects a live row whose webhook_auth_mode is "none" at
// write time (there is no handler-side guard: the handler relies on
// Resolve refusing such a row). This seeds the row directly to prove the
// runtime still fails closed, parses nothing and never treats a live
// unsigned body as payment.
func TestSePayWebhookLiveNoneConfigIsRefused(t *testing.T) {
	fx := newSepayWebhookFixture(t)
	// Product B has no provider row yet, so this creates rather than
	// updates the slot (an update would keep the old id and the
	// ciphertext sealed against a discarded one). Raw inserts do not go
	// through the fixture, so register their slot explicitly.
	fx.cleanupProviderConfigSlots(t, providerConfigSlot{Provider: "sepay", ProductID: fx.prodB.ID, Mode: "live"})
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

// assertRejectedDeliveryLeavesOrderPending checks a refused delivery had
// no effect: the order is still pending, with no licence, no transfer
// and no audit row.
func assertRejectedDeliveryLeavesOrderPending(t *testing.T, fx *webhookFixture, order *model.PaymentOrder) {
	t.Helper()
	got, err := fx.s.GetPaymentOrderByCode(fx.ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OrderPending {
		t.Fatalf("order status = %q, want pending", got.Status)
	}
	if n := fx.count(t, "SELECT count(*) FROM licenses WHERE email = ?", order.Email); n != 0 {
		t.Fatalf("licences = %d, want 0", n)
	}
	if n := fx.count(t, "SELECT count(*) FROM payment_transactions WHERE provider = 'sepay' AND order_id = ?", order.ID); n != 0 {
		t.Fatalf("transactions = %d, want 0", n)
	}
	if n := fx.count(t, "SELECT count(*) FROM payment_events WHERE provider = 'sepay' AND order_id = ?", order.ID); n != 0 {
		t.Fatalf("audit rows = %d, want 0", n)
	}
}

// The unverified peek chooses which product's secret verifies a
// delivery, so the verified order must belong to that same account.
// Otherwise a body can name product A where the peek reads (spending
// A's secret) and product B where the SePay adapter reads (settling B's
// order).
func TestSePayWebhookVerifiedOrderMustMatchVerifyingAccount(t *testing.T) {
	const productBSecret = "whsec_product_b"

	t.Run("signed body names another product's order", func(t *testing.T) {
		fx := newSepayWebhookFixture(t)
		fx.addSepayProduct(t, fx.prodB.ID, productBSecret)
		orderA := fx.newOrderOn(t, fx.prodA.ID, fx.planA.ID, "new", "sepay")
		orderB := fx.newOrderOn(t, fx.prodB.ID, fx.planB.ID, "new", "sepay")
		body := sepaySplitBody(orderA.Code, orderB.Code, newSePayEventID())

		// A's secret verifies because the peek reads data.description.
		w := fx.postSePay(t, body, fx.secret)
		if w.Code != http.StatusForbidden {
			t.Fatalf("cross-product delivery = %d, want 403: %s", w.Code, w.Body.String())
		}
		assertRejectedDeliveryLeavesOrderPending(t, fx, orderB)
	})

	t.Run("default config used for a product that has its own", func(t *testing.T) {
		fx := newSepayWebhookFixture(t)
		fx.addSepayProduct(t, fx.prodB.ID, productBSecret)
		orderB := fx.newOrderOn(t, fx.prodB.ID, fx.planB.ID, "new", "sepay")
		// A valid code that names no order makes the peek fall back to
		// the install default; the adapter still reads B's code out of
		// content, and the default must not settle B's order.
		body := sepaySplitBody(domain.NewOrderCode(), orderB.Code, newSePayEventID())

		w := fx.postSePay(t, body, fx.secret) // the default row's secret
		if w.Code != http.StatusForbidden {
			t.Fatalf("default-config delivery = %d, want 403: %s", w.Code, w.Body.String())
		}
		assertRejectedDeliveryLeavesOrderPending(t, fx, orderB)
	})
}

// payosWebhookBody is one signed PayOS delivery naming the order, and
// postPayOS hands it to the handler the way the router would.
func payosWebhookBody(t *testing.T, order *model.PaymentOrder, reference string) []byte {
	t.Helper()
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
	return body
}

func postPayOS(fx *webhookFixture, body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/webhook/payos", bytes.NewReader(body))
	fx.handler.HandlePayOS(c)
	w.Code = c.Writer.Status()
	return w
}

// A default-only SePay install holds one install-wide row and no
// per-product rows. Resolve falls back to that row for every product, so
// its nil error does not mean the order's product has an account of its
// own; a genuine delivery must pass binding and settle.
func TestSePayWebhookDefaultOnlyInstallSettles(t *testing.T) {
	fx := newDefaultOnlyWebhookFixture(t)
	order := fx.newOrder(t, "new", "sepay")
	eventID := newSePayEventID()

	w := fx.postSePay(t, sepayBankBody(order.Code, eventID, "in"), fx.secret)
	if w.Code != http.StatusOK {
		t.Fatalf("default-only delivery = %d, want 200: %s", w.Code, w.Body.String())
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
	// The install default keeps the provider's bare event id.
	if n := fx.count(t, "SELECT count(*) FROM payment_events WHERE provider = 'sepay' AND event_id = ?", eventIDFor("", eventID)); n != 1 {
		t.Fatalf("payment_events rows = %d, want 1", n)
	}
}

// The same default-only install for PayOS: the checksum key of the
// install-wide row verifies a delivery for a product with no row.
func TestPayOSWebhookDefaultOnlyInstallSettles(t *testing.T) {
	fx := newDefaultOnlyWebhookFixture(t)
	order := fx.newOrder(t, "new", "payos")
	reference := "FT-" + uuid.NewString()[:8]

	w := postPayOS(fx, payosWebhookBody(t, order, reference))
	if w.Code != http.StatusOK {
		t.Fatalf("default-only delivery = %d, want 200: %s", w.Code, w.Body.String())
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
		t.Fatalf("payment_events rows = %d, want 1", n)
	}
}

// Two per-product SePay accounts can see the same bank transfer id. The
// event key is scoped to the account, so the second account's genuine
// delivery is not mistaken for a duplicate of the first and both orders
// settle.
func TestSePayWebhookSameEventIDTwoAccountsBothSettle(t *testing.T) {
	const productBSecret = "whsec_product_b"
	fx := newSepayWebhookFixture(t)
	fx.addSepayProduct(t, fx.prodB.ID, productBSecret)
	orderA := fx.newOrderOn(t, fx.prodA.ID, fx.planA.ID, "new", "sepay")
	orderB := fx.newOrderOn(t, fx.prodB.ID, fx.planB.ID, "new", "sepay")
	eventID := newSePayEventID()

	if w := fx.postSePay(t, sepayBankBody(orderA.Code, eventID, "in"), fx.secret); w.Code != http.StatusOK {
		t.Fatalf("account A delivery = %d: %s", w.Code, w.Body.String())
	}
	if w := fx.postSePay(t, sepayBankBody(orderB.Code, eventID, "in"), productBSecret); w.Code != http.StatusOK {
		t.Fatalf("account B delivery = %d: %s", w.Code, w.Body.String())
	}

	for _, tc := range []struct {
		name      string
		order     *model.PaymentOrder
		productID string
	}{
		{"product A", orderA, fx.prodA.ID},
		{"product B", orderB, fx.prodB.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fx.s.GetPaymentOrderByCode(fx.ctx, tc.order.Code)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != domain.OrderPaid {
				t.Fatalf("order status = %q, want paid", got.Status)
			}
			if n := fx.count(t, "SELECT count(*) FROM licenses WHERE email = ?", tc.order.Email); n != 1 {
				t.Fatalf("licences = %d, want 1", n)
			}
			if n := fx.count(t, "SELECT count(*) FROM payment_events WHERE provider = 'sepay' AND event_id = ?", eventIDFor(tc.productID, eventID)); n != 1 {
				t.Fatalf("audit rows for %s = %d, want 1", eventIDFor(tc.productID, eventID), n)
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
	body := payosWebhookBody(t, order, reference)

	first := postPayOS(fx, body)
	if first.Code != http.StatusOK || first.Body.String() != `{"success":true}` {
		t.Fatalf("payos ack = %d %q", first.Code, first.Body.String())
	}
	if second := postPayOS(fx, body); second.Code != http.StatusOK {
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
	if n := fx.count(t, "SELECT count(*) FROM payment_events WHERE provider = 'payos' AND event_id = ?", fx.prodA.ID+":"+reference); n != 1 {
		t.Fatalf("payos payment_events rows = %d, want 1", n)
	}

	bad := bytes.Replace(body, []byte(`"signature":"`), []byte(`"signature":"dead`), 1)
	if w := postPayOS(fx, bad); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad checksum = %d, want 401", w.Code)
	}
}
