package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dtg-corp/dsoft/internal/middleware"
	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment"
	"github.com/dtg-corp/dsoft/internal/payment/checkout"
	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/payment/provider"
	"github.com/dtg-corp/dsoft/internal/payment/provider/fake"
	"github.com/dtg-corp/dsoft/internal/payment/providercfg"
	"github.com/dtg-corp/dsoft/internal/store"
)

// paymentsFixture is the shared handler fixture plus the checkout
// service both payment handlers need.
type paymentsFixture struct {
	*apiFixture
	fp  *fake.Provider
	svc *checkout.Service
	api *PaymentsAPIHandler
	pub *PaymentsPublicHandler
}

// paymentProviderFactory lets a test fail provider resolution without a
// real config row.
type paymentProviderFactory struct {
	prov provider.Provider
	err  error
}

func (p *paymentProviderFactory) fn(context.Context, string, string) (provider.Provider, *providercfg.ResolvedConfig, error) {
	return p.prov, &providercfg.ResolvedConfig{Provider: "sepay"}, p.err
}

func newPaymentsFixture(t *testing.T) *paymentsFixture {
	t.Helper()
	f := newAPIFixture(t)
	fp := fake.New("sepay")
	svc := &checkout.Service{
		Store: f.s, BaseURL: "https://keygate.example.com",
		Providers: (&paymentProviderFactory{prov: fp}).fn,
	}
	pf := &paymentsFixture{
		apiFixture: f, fp: fp, svc: svc,
		api: &PaymentsAPIHandler{Checkout: svc, Store: f.s},
		pub: &PaymentsPublicHandler{Checkout: svc, Store: f.s, Stripe: &payment.StripeHandler{Store: f.s}},
	}
	return pf
}

func (f *paymentsFixture) email(prefix string) string {
	return prefix + "-" + f.suffix + "@example.com"
}

// key inserts a real API key and returns it for the handler context.
// payment_orders.created_by_api_key_id is a foreign key, so a made-up id
// would fail the insert (and the pinned test's literal "key-1" does).
func (f *paymentsFixture) key(t *testing.T, scopes ...string) *model.APIKey {
	return f.boundKey(t, "", scopes...)
}

func (f *paymentsFixture) boundKey(t *testing.T, productID string, scopes ...string) *model.APIKey {
	t.Helper()
	ak := &model.APIKey{
		Name: "payments-test-" + strings.Join(scopes, "-"), Prefix: "kg_live_test",
		Scopes: scopes, ProductID: productID,
	}
	if err := f.s.CreateAPIKey(f.ctx, ak, "kg_live_"+f.suffix+"-"+ak.Name+"-"+productID); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return ak
}

func (f *paymentsFixture) price(t *testing.T, plan *model.Plan, providerName string, amountMinor int64) {
	t.Helper()
	if _, err := f.s.UpsertPlanPrice(f.ctx, plan.ID, store.PlanPriceInput{
		Provider: providerName, Currency: "VND", AmountMinor: amountMinor,
	}); err != nil {
		t.Fatalf("upsert price: %v", err)
	}
}

// seedOrder writes an order and, when withAttempt is set, a pending
// attempt carrying a SePay QR and bank details.
func (f *paymentsFixture) seedOrder(t *testing.T, plan *model.Plan, email, providerName string, amountMinor int64, withAttempt bool) (*model.PaymentOrder, *model.PaymentAttempt) {
	t.Helper()
	order, err := f.s.CreatePaymentOrder(f.ctx, store.CreateOrderInput{
		ProductID: plan.ProductID, PlanID: plan.ID, Email: strings.ToLower(email), Kind: "new", Quantity: 1,
		SubtotalMinor: amountMinor, TotalMinor: amountMinor, Currency: "VND", Provider: providerName,
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if !withAttempt {
		return order, nil
	}
	attempt := &model.PaymentAttempt{
		ID: store.NewID(), OrderID: order.ID, Provider: providerName, ProviderRef: "ref-" + order.Code,
		Status: domain.AttemptPending, AmountMinor: amountMinor, Currency: "VND",
		QRPayload:   "https://qr.example.com/" + order.Code,
		CheckoutURL: "https://pay.example.com/" + order.Code,
		Raw: json.RawMessage(`{"bank":{"bank_name":"MB","bank_bin":"970422","account_no":"0978482700",` +
			`"account_name":"CONG TY TNHH KEYGATE"},"description":"` + order.Code + `"}`),
		ExpiresAt: ptrTime(time.Now().Add(time.Hour)),
	}
	if err := f.s.CreatePaymentAttempt(f.ctx, attempt); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	return order, attempt
}

func ptrTime(t time.Time) *time.Time { return &t }

// record runs a handler against a fresh recorder so a test can read
// headers and the raw body, not just the decoded envelope.
func (f *apiFixture) record(fn gin.HandlerFunc, method, target, body string, key *model.APIKey, params ...gin.Param) *httptest.ResponseRecorder {
	f.t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = params
	if key != nil {
		c.Set("api_key", key)
	}
	fn(c)
	return w
}

// dataOf reads the envelope's data object.
func dataOf(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return env.Data
}

func TestPaymentsPublicStatus(t *testing.T) {
	f := newPaymentsFixture(t)
	order, _ := f.seedOrder(t, f.planA, "status@example.com", "sepay", 1_190_000, true)

	w := f.record(f.pub.Status, http.MethodGet, "/pay/"+order.Code+"/status", "", nil,
		gin.Param{Key: "checkout_id", Value: order.Code})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	data := dataOf(t, w)
	for key, want := range map[string]any{
		"code": order.Code, "status": "pending", "amount_minor": float64(1_190_000),
		"currency": "VND", "currency_exponent": float64(0), "description": order.Code,
		"qr_image_url": "https://qr.example.com/" + order.Code,
		"checkout_url": "https://pay.example.com/" + order.Code,
	} {
		if data[key] != want {
			t.Errorf("%s = %v, want %v", key, data[key], want)
		}
	}
	bank, ok := data["bank"].(map[string]any)
	if !ok || bank["account_no"] != "0978482700" || bank["bank_name"] != "MB" {
		t.Fatalf("bank = %v", data["bank"])
	}
	if _, ok := data["expires_at"].(string); !ok {
		t.Fatalf("expires_at = %v", data["expires_at"])
	}
	// The order code is the only capability the holder has: no buyer
	// identity, metadata or internal ids ride along.
	for _, leaked := range []string{"email", "metadata", "buyer", "product_id", "plan_id", "id", "created_by_api_key_id"} {
		if _, ok := data[leaked]; ok {
			t.Errorf("public status leaked %q: %v", leaked, data)
		}
	}
}

// PayOS's qr_payload is an EMVCo string, not an image: the pay page has
// to render the provider's image URL that checkout stored beside it.
func TestPaymentsPublicStatusPrefersStoredQRImage(t *testing.T) {
	f := newPaymentsFixture(t)
	order, _ := f.seedOrder(t, f.planA, f.email("qr-image"), "payos", 100_000, false)
	attempt := &model.PaymentAttempt{
		ID: store.NewID(), OrderID: order.ID, Provider: "payos", Status: domain.AttemptPending,
		AmountMinor: 100_000, Currency: "VND", QRPayload: "000201010212...",
		CheckoutURL: "https://pay.payos.vn/web/" + order.Code,
		Raw:         json.RawMessage(`{"qr_image_url":"https://img.example.com/payos.png","description":"PAYOS-DESC"}`),
		ExpiresAt:   ptrTime(time.Now().Add(time.Hour)),
	}
	if err := f.s.CreatePaymentAttempt(f.ctx, attempt); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	w := f.record(f.pub.Status, http.MethodGet, "/pay/"+order.Code+"/status", "", nil,
		gin.Param{Key: "checkout_id", Value: order.Code})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d %s", w.Code, w.Body.String())
	}
	data := dataOf(t, w)
	if data["qr_image_url"] != "https://img.example.com/payos.png" {
		t.Fatalf("qr_image_url = %v", data["qr_image_url"])
	}
	// A provider with no bank transfer (PayOS) still gets its transfer
	// description through; the bank block is simply absent.
	if data["description"] != "PAYOS-DESC" {
		t.Fatalf("description = %v", data["description"])
	}
	if _, ok := data["bank"]; ok {
		t.Fatalf("bank = %v, want absent without bank details", data["bank"])
	}
}

func TestPaymentsPublicStatusWithoutAttempt(t *testing.T) {
	f := newPaymentsFixture(t)
	order, _ := f.seedOrder(t, f.planA, "noattempt@example.com", "sepay", 50_000, false)
	w := f.record(f.pub.Status, http.MethodGet, "/pay/"+order.Code+"/status", "", nil,
		gin.Param{Key: "checkout_id", Value: order.Code})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d %s", w.Code, w.Body.String())
	}
	data := dataOf(t, w)
	if _, ok := data["qr_image_url"]; ok {
		t.Fatalf("qr_image_url present without an attempt: %v", data)
	}
	if data["description"] != order.Code {
		t.Fatalf("description = %v, want the order code", data["description"])
	}
}

func TestPaymentsPublicStatusUnknownAndMalformed(t *testing.T) {
	f := newPaymentsFixture(t)
	for name, code := range map[string]string{
		"unknown":   "KGBBBBBB",
		"malformed": "not-a-code",
		"empty":     "",
	} {
		w := f.record(f.pub.Status, http.MethodGet, "/pay/"+code+"/status", "", nil,
			gin.Param{Key: "checkout_id", Value: code})
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d %s, want 404", name, w.Code, w.Body.String())
		}
	}
}

func TestPayPageDispatchesOrderCode(t *testing.T) {
	f := newPaymentsFixture(t)
	order, _ := f.seedOrder(t, f.planA, "paypage@example.com", "sepay", 50_000, true)
	w := f.record(f.pub.PayPage, http.MethodGet, "/pay/"+order.Code, "", nil,
		gin.Param{Key: "checkout_id", Value: order.Code})
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/checkout/pay/"+order.Code {
		t.Fatalf("Location = %q", loc)
	}
}

// The legacy route's ids are 8 hex characters today (older links used
// cs_…): neither is a KG code, so both keep reaching the Stripe handler
// unchanged.
func TestPayPageKeepsLegacyStripeBehaviour(t *testing.T) {
	f := newPaymentsFixture(t)
	tests := map[string]struct {
		id     string
		status int
		body   string
	}{
		"eight char checkout id": {id: "a1b2c3d4", status: http.StatusNotFound, body: "plan not found"},
		"legacy cs_ prefix":      {id: "cs_abcde", status: http.StatusNotFound, body: "plan not found"},
		"wrong length":           {id: "abc", status: http.StatusBadRequest, body: "invalid checkout id"},
	}
	for name, tt := range tests {
		w := f.record(f.pub.PayPage, http.MethodGet, "/pay/"+tt.id, "", nil,
			gin.Param{Key: "checkout_id", Value: tt.id})
		if w.Code != tt.status {
			t.Errorf("%s: status = %d %s, want %d", name, w.Code, w.Body.String(), tt.status)
		}
		if !strings.Contains(w.Body.String(), tt.body) {
			t.Errorf("%s: body = %q, want %q", name, w.Body.String(), tt.body)
		}
	}
}

func TestCreateOrderViaAPI(t *testing.T) {
	f := newPaymentsFixture(t)
	f.price(t, f.planA, "sepay", 1_190_000)
	key := f.key(t, model.ScopePaymentsWrite)
	email := f.email("api")

	code, out := f.call(f.api.Create, http.MethodPost, "/api/v1/payments/orders",
		`{"plan_id":"`+f.planA.ID+`","email":"`+email+`","provider":"sepay","quantity":2,
		  "buyer":{"name":"Nguyen Van A","email":"`+email+`"},
		  "metadata":{"workspace_id":"ws_123"}}`, key)
	if code != http.StatusCreated {
		t.Fatalf("status %d: %v", code, out)
	}
	data, _ := out["data"].(map[string]any)
	if data["status"] != "pending" || data["kind"] != "new" || data["provider"] != "sepay" {
		t.Fatalf("data = %v", data)
	}
	if q, ok := data["quantity"].(float64); !ok || q != 2 {
		t.Fatalf("quantity: %T %v (%v)", data["quantity"], data["quantity"], data)
	}
	if s, ok := data["subtotal_minor"].(float64); !ok || s != 2_380_000 {
		t.Fatalf("subtotal: %T %v (%v)", data["subtotal_minor"], data["subtotal_minor"], data)
	}
	if tot, ok := data["total_minor"].(float64); !ok || tot != 2_380_000 {
		t.Fatalf("total: %T %v (%v)", data["total_minor"], data["total_minor"], data)
	}
	if data["currency"] != "VND" {
		t.Fatalf("currency = %v", data["currency"])
	}
	orderCode, _ := data["code"].(string)
	if !domain.IsOrderCode(orderCode) {
		t.Fatalf("code = %v, want a KG order code", data["code"])
	}
	payment, ok := data["payment"].(map[string]any)
	if !ok || payment["provider"] != "sepay" || payment["qr_payload"] != "fake-qr" ||
		payment["qr_image_url"] != "fake-qr" {
		t.Fatalf("payment = %v", data["payment"])
	}

	stored, err := f.s.GetPaymentOrderByCode(f.ctx, orderCode)
	if err != nil {
		t.Fatalf("re-read order: %v", err)
	}
	if stored.CreatedByKeyID != key.ID {
		t.Fatalf("created_by_api_key_id = %q, want %q", stored.CreatedByKeyID, key.ID)
	}
	if stored.Email != email {
		t.Fatalf("email = %q, want %q", stored.Email, email)
	}
	var meta map[string]any
	if err := json.Unmarshal(stored.Metadata, &meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if meta["workspace_id"] != "ws_123" {
		t.Fatalf("metadata = %v", meta)
	}
}

func TestCreateOrderIdempotent(t *testing.T) {
	f := newPaymentsFixture(t)
	f.price(t, f.planA, "sepay", 1_190_000)
	r := gin.New()
	r.POST("/api/v1/payments/orders", middleware.Idempotency(f.s), f.api.Create)
	email := f.email("idem")
	body := `{"plan_id":"` + f.planA.ID + `","email":"` + email + `","provider":"sepay","quantity":1}`
	send := func(payload string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/payments/orders", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "idem-order-"+f.suffix)
		r.ServeHTTP(w, req)
		return w
	}

	first := send(body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d %s", first.Code, first.Body.String())
	}
	second := send(body)
	if second.Code != first.Code || second.Body.String() != first.Body.String() {
		t.Fatalf("replay = %d %s, want the identical response", second.Code, second.Body.String())
	}
	if n := f.orderCount(email); n != 1 {
		t.Fatalf("orders = %d, want 1", n)
	}
	// A different body under the same key is a client bug, not a retry:
	// the middleware refuses it with 422 and never reaches the handler.
	third := send(`{"plan_id":"` + f.planB.ID + `","email":"` + email + `","provider":"sepay","quantity":1}`)
	if third.Code != http.StatusUnprocessableEntity {
		t.Fatalf("body mismatch = %d %s, want 422", third.Code, third.Body.String())
	}
	if n := f.orderCount(email); n != 1 {
		t.Fatalf("orders after mismatch = %d, want 1", n)
	}
}

func TestCreateOrderAPIValidationErrors(t *testing.T) {
	f := newPaymentsFixture(t)
	// planA is unpriced here: the price row is what this test is missing.
	key := f.key(t, model.ScopePaymentsWrite)
	email := f.email("validation")

	tests := map[string]struct {
		body   string
		status int
		code   string
	}{
		"invalid json": {
			body: `{"plan_id":`, status: http.StatusBadRequest, code: "BAD_REQUEST",
		},
		"missing plan": {
			body: `{"email":"` + email + `"}`, status: http.StatusBadRequest, code: "BAD_REQUEST",
		},
		"no price": {
			body: `{"plan_id":"` + f.planA.ID + `","email":"` + email + `","provider":"sepay","quantity":1}`, status: http.StatusUnprocessableEntity, code: "PAYMENT_PRICE_NOT_CONFIGURED",
		},
		"unknown plan": {
			body: `{"plan_id":"does-not-exist","email":"` + email + `","provider":"sepay","quantity":1}`, status: http.StatusUnprocessableEntity, code: "PAYMENT_PRICE_NOT_CONFIGURED",
		},
	}
	for name, tt := range tests {
		code, out := f.call(f.api.Create, http.MethodPost, "/api/v1/payments/orders", tt.body, key)
		if code != tt.status || errCode(out) != tt.code {
			t.Errorf("%s = %d %v, want %d %s", name, code, out, tt.status, tt.code)
		}
	}
}

func TestCreateOrderProviderUnavailable(t *testing.T) {
	f := newPaymentsFixture(t)
	f.price(t, f.planA, "sepay", 1_190_000)
	f.svc.Providers = (&paymentProviderFactory{err: providercfg.ErrProviderDisabled}).fn
	key := f.key(t, model.ScopePaymentsWrite)

	code, out := f.call(f.api.Create, http.MethodPost, "/api/v1/payments/orders",
		`{"plan_id":"`+f.planA.ID+`","email":"`+f.email("unavailable")+`","provider":"sepay","quantity":1}`, key)
	if code != http.StatusServiceUnavailable || errCode(out) != "PAYMENT_PROVIDER_DISABLED" {
		t.Fatalf("status = %d %v, want 503 PAYMENT_PROVIDER_DISABLED", code, out)
	}
}

// A provider that is configured but fails while creating the payment
// instruction is a bad gateway, not an internal error: the request was
// valid and the upstream is at fault.
func TestCreateOrderProviderFailure(t *testing.T) {
	f := newPaymentsFixture(t)
	f.price(t, f.planA, "sepay", 1_190_000)
	f.fp.CreateFn = func(context.Context, provider.CreateAttemptRequest) (provider.CreateAttemptResult, error) {
		return provider.CreateAttemptResult{}, errors.New("provider gateway exploded")
	}
	key := f.key(t, model.ScopePaymentsWrite)
	email := f.email("provider-down")

	code, out := f.call(f.api.Create, http.MethodPost, "/api/v1/payments/orders",
		`{"plan_id":"`+f.planA.ID+`","email":"`+email+`","provider":"sepay","quantity":1}`, key)
	if code != http.StatusBadGateway || errCode(out) != "PAYMENT_PROVIDER_ERROR" {
		t.Fatalf("status = %d %v, want 502 PAYMENT_PROVIDER_ERROR", code, out)
	}
	// The order stays pending so the caller can retry; no buyer-facing
	// error body leaks the provider's message.
	if e, _ := out["error"].(map[string]any); strings.Contains(e["message"].(string), "exploded") {
		t.Fatalf("error message leaked provider detail: %v", e)
	}
	if n := f.orderCount(email); n != 1 {
		t.Fatalf("orders = %d, want the pending order kept for retry", n)
	}
}

// Quantity is validated before anything is written: zero (including an
// omitted field), negatives and runaway volumes are 400s, not orders.
func TestCreateOrderAPIRejectsInvalidQuantity(t *testing.T) {
	f := newPaymentsFixture(t)
	f.price(t, f.planA, "sepay", 1_190_000)
	key := f.key(t, model.ScopePaymentsWrite)
	email := f.email("bad-qty")

	for name, body := range map[string]string{
		"zero":       `{"plan_id":"` + f.planA.ID + `","email":"` + email + `","provider":"sepay","quantity":0}`,
		"negative":   `{"plan_id":"` + f.planA.ID + `","email":"` + email + `","provider":"sepay","quantity":-1}`,
		"over limit": `{"plan_id":"` + f.planA.ID + `","email":"` + email + `","provider":"sepay","quantity":101}`,
	} {
		code, out := f.call(f.api.Create, http.MethodPost, "/api/v1/payments/orders", body, key)
		if code != http.StatusBadRequest || errCode(out) != "INVALID_QUANTITY" {
			t.Errorf("%s = %d %v, want 400 INVALID_QUANTITY", name, code, out)
		}
	}
	if n := f.orderCount(email); n != 0 {
		t.Fatalf("orders = %d, want 0", n)
	}
}

// A product-bound write key cannot create an order for another
// product's plan, and the refused call writes nothing.
func TestCreateOrderAPIProductScope(t *testing.T) {
	f := newPaymentsFixture(t)
	f.price(t, f.planB, "sepay", 100_000)
	key := f.boundKey(t, f.prodA.ID, model.ScopePaymentsWrite)
	email := f.email("other-product")

	code, out := f.call(f.api.Create, http.MethodPost, "/api/v1/payments/orders",
		`{"plan_id":"`+f.planB.ID+`","email":"`+email+`","provider":"sepay"}`, key)
	if code != http.StatusForbidden || errCode(out) != "PRODUCT_SCOPE_MISMATCH" {
		t.Fatalf("create = %d %v, want 403 PRODUCT_SCOPE_MISMATCH", code, out)
	}
	if n := f.orderCount(email); n != 0 {
		t.Fatalf("orders = %d, want 0", n)
	}

	// An install-wide key is not tied to a product and may sell any of
	// them.
	system := f.key(t, model.ScopePaymentsWrite)
	code, out = f.call(f.api.Create, http.MethodPost, "/api/v1/payments/orders",
		`{"plan_id":"`+f.planB.ID+`","email":"`+email+`","provider":"sepay","quantity":1}`, system)
	if code != http.StatusCreated {
		t.Fatalf("system key create = %d %v, want 201", code, out)
	}
}

func TestGetOrderViaAPI(t *testing.T) {
	f := newPaymentsFixture(t)
	order, _ := f.seedOrder(t, f.planA, f.email("get"), "sepay", 1_190_000, true)
	key := f.boundKey(t, f.prodA.ID, model.ScopePaymentsRead)

	code, out := f.call(f.api.Get, http.MethodGet, "/api/v1/payments/orders/"+order.Code, "", key,
		gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusOK {
		t.Fatalf("status = %d %v", code, out)
	}
	data, _ := out["data"].(map[string]any)
	if data["code"] != order.Code || data["status"] != "pending" || data["email"] != order.Email {
		t.Fatalf("data = %v", data)
	}
	payment, ok := data["payment"].(map[string]any)
	if !ok || payment["qr_image_url"] != "https://qr.example.com/"+order.Code {
		t.Fatalf("payment = %v", data["payment"])
	}

	// Another product's order, and an unknown code, are both 404 to a
	// product-bound key: existence itself is not disclosed.
	other, _ := f.seedOrder(t, f.planB, f.email("get-other"), "sepay", 50_000, false)
	for name, wantCode := range map[string]string{
		other.Code: "PAYMENT_ORDER_NOT_FOUND",
		"KGBBBBBB": "PAYMENT_ORDER_NOT_FOUND",
	} {
		code, out := f.call(f.api.Get, http.MethodGet, "/api/v1/payments/orders/"+name, "", key,
			gin.Param{Key: "code", Value: name})
		if code != http.StatusNotFound || errCode(out) != wantCode {
			t.Errorf("get %q = %d %v, want 404 %s", name, code, out, wantCode)
		}
	}
}

func TestListOrdersViaAPI(t *testing.T) {
	f := newPaymentsFixture(t)
	// The test database is shared across runs, so every assertion is
	// scoped to this fixture's own products rather than to the table.
	first, _ := f.seedOrder(t, f.planA, f.email("list-one"), "sepay", 100_000, false)
	second, _ := f.seedOrder(t, f.planA, f.email("list-two"), "sepay", 200_000, false)
	if _, err := f.s.CancelPaymentOrder(f.ctx, second.Code); err != nil {
		t.Fatalf("cancel second: %v", err)
	}
	f.seedOrder(t, f.planB, f.email("list-other"), "payos", 300_000, false)
	productA := "?product_id=" + f.prodA.ID

	unbound := f.key(t, model.ScopePaymentsRead)
	code, out := f.call(f.api.List, http.MethodGet, "/api/v1/payments/orders"+productA, "", unbound)
	if code != http.StatusOK {
		t.Fatalf("status = %d %v", code, out)
	}
	data, _ := out["data"].(map[string]any)
	if data["total"] != float64(2) {
		t.Fatalf("total = %v, want 2", data["total"])
	}
	if data["limit"] != float64(defaultListLimit) || data["offset"] != float64(0) {
		t.Fatalf("pagination = %v", data)
	}
	rows, _ := data["data"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows = %v", data["data"])
	}

	// Filters combine with the product filter.
	filters := map[string]struct {
		query string
		total float64
	}{
		"status":   {query: productA + "&status=canceled", total: 1},
		"email":    {query: productA + "&email=" + first.Email, total: 1},
		"provider": {query: productA + "&provider=payos", total: 0},
		"none":     {query: productA + "&provider=stripe", total: 0},
		"other":    {query: "?product_id=" + f.prodB.ID, total: 1},
	}
	for name, tt := range filters {
		code, out := f.call(f.api.List, http.MethodGet, "/api/v1/payments/orders"+tt.query, "", unbound)
		if code != http.StatusOK {
			t.Fatalf("%s: status = %d %v", name, code, out)
		}
		data, _ := out["data"].(map[string]any)
		if data["total"] != tt.total {
			t.Errorf("%s: total = %v, want %v", name, data["total"], tt.total)
		}
		if name == "none" {
			if rows, ok := data["data"].([]any); !ok || len(rows) != 0 {
				t.Errorf("empty list = %v, want []", data["data"])
			}
		}
	}

	// An invalid date is refused instead of silently ignored.
	code, out = f.call(f.api.List, http.MethodGet, "/api/v1/payments/orders?created_from=yesterday", "", unbound)
	if code != http.StatusBadRequest {
		t.Fatalf("bad date = %d %v, want 400", code, out)
	}

	// A product-bound key sees only its own product, whatever it asks
	// for; naming another product is refused.
	bound := f.boundKey(t, f.prodA.ID, model.ScopePaymentsRead)
	code, out = f.call(f.api.List, http.MethodGet, "/api/v1/payments/orders", "", bound)
	if code != http.StatusOK {
		t.Fatalf("bound list = %d %v", code, out)
	}
	data, _ = out["data"].(map[string]any)
	if data["total"] != float64(2) {
		t.Fatalf("bound total = %v, want 2", data["total"])
	}
	for _, row := range data["data"].([]any) {
		if product := row.(map[string]any)["product_id"]; product != f.prodA.ID {
			t.Fatalf("bound list leaked product %v", product)
		}
	}
	// The first row is the newest (created_at DESC): second was created
	// last, so the cancelled one leads.
	if rows := data["data"].([]any); len(rows) > 0 && rows[0].(map[string]any)["code"] != second.Code {
		t.Fatalf("first row = %v, want the newest %s", rows[0], second.Code)
	}
	code, out = f.call(f.api.List, http.MethodGet, "/api/v1/payments/orders?product_id="+f.prodB.ID, "", bound)
	if code != http.StatusForbidden || errCode(out) != "PRODUCT_SCOPE_MISMATCH" {
		t.Fatalf("bound cross-product list = %d %v, want 403", code, out)
	}
	if first.Code == second.Code {
		t.Fatal("fixture produced identical codes")
	}
}

func TestCancelOrderViaAPI(t *testing.T) {
	f := newPaymentsFixture(t)
	order, _ := f.seedOrder(t, f.planA, f.email("cancel"), "sepay", 100_000, true)
	key := f.boundKey(t, f.prodA.ID, model.ScopePaymentsWrite)

	code, out := f.call(f.api.Cancel, http.MethodPost, "/api/v1/payments/orders/"+order.Code+"/cancel", "", key,
		gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusOK {
		t.Fatalf("cancel = %d %v", code, out)
	}
	if data, _ := out["data"].(map[string]any); data["status"] != "canceled" {
		t.Fatalf("data = %v", data)
	}
	// Cancelling twice is a conflict, not an internal error.
	code, out = f.call(f.api.Cancel, http.MethodPost, "/api/v1/payments/orders/"+order.Code+"/cancel", "", key,
		gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusConflict || errCode(out) != "PAYMENT_ORDER_NOT_PAYABLE" {
		t.Fatalf("second cancel = %d %v, want 409 PAYMENT_ORDER_NOT_PAYABLE", code, out)
	}
	// Unknown code.
	code, out = f.call(f.api.Cancel, http.MethodPost, "/api/v1/payments/orders/KGBBBBBB/cancel", "", key,
		gin.Param{Key: "code", Value: "KGBBBBBB"})
	if code != http.StatusNotFound || errCode(out) != "PAYMENT_ORDER_NOT_FOUND" {
		t.Fatalf("unknown cancel = %d %v", code, out)
	}

	// A bound key cannot cancel another product's order, and the order
	// stays pending.
	other, _ := f.seedOrder(t, f.planB, f.email("cancel-other"), "sepay", 50_000, false)
	code, out = f.call(f.api.Cancel, http.MethodPost, "/api/v1/payments/orders/"+other.Code+"/cancel", "", key,
		gin.Param{Key: "code", Value: other.Code})
	if code != http.StatusNotFound {
		t.Fatalf("cross-product cancel = %d %v, want 404", code, out)
	}
	reloaded, err := f.s.GetPaymentOrderByCode(f.ctx, other.Code)
	if err != nil {
		t.Fatalf("re-read other: %v", err)
	}
	if reloaded.Status != domain.OrderPending {
		t.Fatalf("cross-product order status = %q, want pending", reloaded.Status)
	}
}

// The routes are only as safe as the middleware they are registered
// with: reads must not be able to create or cancel, and a write key
// implies read.
func TestPaymentsAPIScopeEnforcement(t *testing.T) {
	f := newPaymentsFixture(t)
	f.price(t, f.planA, "sepay", 1_190_000)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	write := r.Group("/api/v1/payments",
		middleware.APIKeyAuth(f.s),
		middleware.RequireScope(model.ScopePaymentsWrite))
	read := r.Group("/api/v1/payments",
		middleware.APIKeyAuth(f.s),
		middleware.RequireScope(model.ScopePaymentsRead, model.ScopePaymentsWrite))
	write.POST("/orders", f.api.Create)
	write.POST("/orders/:code/cancel", f.api.Cancel)
	read.GET("/orders/:code", f.api.Get)
	read.GET("/orders", f.api.List)

	body := `{"plan_id":"` + f.planA.ID + `","email":"` + f.email("scope") + `","provider":"sepay","quantity":1}`
	send := func(rawKey, method, path, payload string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		if rawKey != "" {
			req.Header.Set("Authorization", "Bearer "+rawKey)
		}
		r.ServeHTTP(w, req)
		return w
	}

	if w := send("", http.MethodPost, "/api/v1/payments/orders", body); w.Code != http.StatusUnauthorized {
		t.Errorf("no key = %d %s, want 401", w.Code, w.Body.String())
	}

	_, readRaw := f.apiKey(t, []string{model.ScopePaymentsRead}, f.prodA.ID)
	if w := send(readRaw, http.MethodPost, "/api/v1/payments/orders", body); w.Code != http.StatusForbidden {
		t.Errorf("read key create = %d %s, want 403", w.Code, w.Body.String())
	}
	if w := send(readRaw, http.MethodGet, "/api/v1/payments/orders", ""); w.Code != http.StatusOK {
		t.Errorf("read key list = %d %s, want 200", w.Code, w.Body.String())
	}

	writeRaw := f.writeAPIKey(t)
	w := send(writeRaw, http.MethodPost, "/api/v1/payments/orders", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("write key create = %d %s", w.Code, w.Body.String())
	}
	created := dataOf(t, w)
	orderCode := created["code"].(string)
	if w := send(writeRaw, http.MethodGet, "/api/v1/payments/orders/"+orderCode, ""); w.Code != http.StatusOK {
		t.Errorf("write key read = %d %s, want 200", w.Code, w.Body.String())
	}

	// An install-wide key (no product binding) passes the same scope
	// checks and is not tied to one product's catalogue.
	systemRaw := f.writeAPIKeyUnbound(t)
	if w := send(systemRaw, http.MethodPost, "/api/v1/payments/orders", body); w.Code != http.StatusCreated {
		t.Errorf("system key create = %d %s, want 201", w.Code, w.Body.String())
	}
}

// apiKey creates a bound key and returns it with the bearer token.
func (f *paymentsFixture) apiKey(t *testing.T, scopes []string, productID string) (*model.APIKey, string) {
	t.Helper()
	raw := "kg_live_" + f.suffix + "-" + strings.Join(scopes, "-") + "-" + productID
	ak := &model.APIKey{Name: "payments-test", Prefix: "kg_live_test", Scopes: scopes, ProductID: productID}
	if err := f.s.CreateAPIKey(f.ctx, ak, raw); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return ak, raw
}

func (f *paymentsFixture) writeAPIKey(t *testing.T) string {
	t.Helper()
	_, raw := f.apiKey(t, []string{model.ScopePaymentsWrite}, f.prodA.ID)
	return raw
}

func (f *paymentsFixture) writeAPIKeyUnbound(t *testing.T) string {
	t.Helper()
	raw := "kg_live_" + f.suffix + "-system"
	ak := &model.APIKey{Name: "payments-system", Prefix: "kg_live_test", Scopes: []string{model.ScopePaymentsWrite}}
	if err := f.s.CreateAPIKey(f.ctx, ak, raw); err != nil {
		t.Fatalf("create unbound api key: %v", err)
	}
	return raw
}

func (f *paymentsFixture) orderCount(email string) int {
	f.t.Helper()
	n, err := f.s.DB.NewSelect().Model((*model.PaymentOrder)(nil)).
		Where("email = ?", email).Count(f.ctx)
	if err != nil {
		f.t.Fatalf("count orders: %v", err)
	}
	return n
}
