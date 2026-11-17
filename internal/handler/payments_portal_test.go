package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/checkout"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/store"
)

// ─── portal fixture ───

type fakePortalPayer struct {
	calls   []string
	attempt *model.PaymentAttempt
	err     error
}

func (p *fakePortalPayer) ReissueAttempt(_ context.Context, order *model.PaymentOrder) (*model.PaymentAttempt, error) {
	p.calls = append(p.calls, order.ID)
	return p.attempt, p.err
}

type fakePortalInvoices struct {
	invoices   map[string]*model.Invoice
	pdf        []byte
	err        error
	lastFilter PortalInvoiceFilter
}

func (f *fakePortalInvoices) Get(_ context.Context, id string) (*model.Invoice, error) {
	if f.err != nil {
		return nil, f.err
	}
	inv, ok := f.invoices[id]
	if !ok {
		return nil, ErrPortalInvoiceNotFound
	}
	return inv, nil
}

func (f *fakePortalInvoices) PDF(_ context.Context, id string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	if _, ok := f.invoices[id]; !ok {
		return nil, ErrPortalInvoiceNotFound
	}
	return f.pdf, nil
}

func (f *fakePortalInvoices) List(_ context.Context, filter PortalInvoiceFilter) ([]*model.Invoice, int, error) {
	f.lastFilter = filter
	if f.err != nil {
		return nil, 0, f.err
	}
	var all []*model.Invoice
	for _, inv := range f.invoices {
		if strings.EqualFold(portalBuyerEmail(inv.Buyer), filter.BuyerEmail) {
			all = append(all, inv)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ID > all[j].ID
	})
	total := len(all)
	if filter.Offset >= total {
		return nil, total, nil
	}
	all = all[filter.Offset:]
	if filter.Limit > 0 && len(all) > filter.Limit {
		all = all[:filter.Limit]
	}
	return all, total, nil
}

type portalFixture struct {
	*apiFixture
	h        *PortalPaymentsHandler
	payer    *fakePortalPayer
	invoices *fakePortalInvoices
	alice    *model.User
	bob      *model.User
}

func newPortalFixture(t *testing.T) *portalFixture {
	t.Helper()
	f := newAPIFixture(t)
	alice := &model.User{Email: "alice-" + f.suffix + "@example.com", Name: "Alice"}
	bob := &model.User{Email: "bob-" + f.suffix + "@example.com", Name: "Bob"}
	for _, u := range []*model.User{alice, bob} {
		if err := f.s.UpsertUser(f.ctx, u); err != nil {
			t.Fatalf("create user: %v", err)
		}
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, u := range []*model.User{alice, bob} {
			_, _ = f.s.DB.NewRaw("DELETE FROM customer_billing_profiles WHERE user_id = ?", u.ID).Exec(ctx)
			_, _ = f.s.DB.NewRaw("DELETE FROM users WHERE id = ?", u.ID).Exec(ctx)
		}
	})
	payer := new(fakePortalPayer)
	invoices := &fakePortalInvoices{invoices: map[string]*model.Invoice{}}
	return &portalFixture{
		apiFixture: f,
		h:          NewPortalPaymentsHandler(f.s, payer, invoices),
		payer:      payer,
		invoices:   invoices,
		alice:      alice,
		bob:        bob,
	}
}

// portalOrder writes an order for one of the fixture's customers.
func (f *portalFixture) portalOrder(t *testing.T, email, buyer string) *model.PaymentOrder {
	t.Helper()
	o, err := f.s.CreatePaymentOrder(f.ctx, store.CreateOrderInput{
		ProductID: f.prodA.ID, PlanID: f.planA.ID, Email: email, Kind: "new", Quantity: 1,
		SubtotalMinor: 1_190_000, TotalMinor: 1_190_000, Currency: "VND", CurrencyExponent: 0,
		Provider: "sepay", ExpiresAt: time.Now().Add(time.Hour),
		Buyer: []byte(buyer), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.s.DB.NewRaw("DELETE FROM payment_orders WHERE id = ?", o.ID).Exec(context.Background())
	})
	return o
}

// callPortal runs a handler with the session values SessionAuth would set.
// A blank email/ID leaves them unset, which is the no-session case.
func (f *portalFixture) callPortal(fn gin.HandlerFunc, method, target, body, userID, email string,
	params ...gin.Param) (int, map[string]any) {
	f.t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = params
	if userID != "" {
		c.Set("user_id", userID)
	}
	if email != "" {
		c.Set("email", email)
	}
	fn(c)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return c.Writer.Status(), out
}

func (f *portalFixture) asAlice(fn gin.HandlerFunc, method, target, body string, params ...gin.Param) (int, map[string]any) {
	return f.callPortal(fn, method, target, body, f.alice.ID, f.alice.Email, params...)
}

func (f *portalFixture) asBob(fn gin.HandlerFunc, method, target, body string, params ...gin.Param) (int, map[string]any) {
	return f.callPortal(fn, method, target, body, f.bob.ID, f.bob.Email, params...)
}

func portalDataList(t *testing.T, out map[string]any, key string) []any {
	t.Helper()
	data, _ := out["data"].(map[string]any)
	rows, _ := data[key].([]any)
	return rows
}

// ─── auth and registration ───

func TestPortalPaymentRoutesRequireSession(t *testing.T) {
	f := newPortalFixture(t)
	for _, tc := range []struct {
		name string
		fn   gin.HandlerFunc
		path string
	}{
		{"orders", f.h.ListOrders, "/api/v1/portal/payments/orders"},
		{"profile", f.h.GetBillingProfile, "/api/v1/portal/billing-profile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := f.callPortal(tc.fn, http.MethodGet, tc.path, "", "", "")
			if code != http.StatusUnauthorized || errCode(out) != "UNAUTHORIZED" {
				t.Fatalf("= %d %v, want 401 UNAUTHORIZED", code, out)
			}
		})
	}
}

// The registration function is the contract main.go calls: prove the
// paths exist under /api/v1/portal and that the session values flow in.
func TestRegisterPortalPaymentRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f := newPortalFixture(t)
	order := f.portalOrder(t, f.alice.Email, `{}`)

	r := gin.New()
	g := r.Group("/api/v1/portal")
	g.Use(func(c *gin.Context) {
		c.Set("user_id", f.alice.ID)
		c.Set("email", f.alice.Email)
	})
	RegisterPortalPaymentRoutes(g, f.h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/portal/payments/orders", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/portal/payments/orders/"+order.Code, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("detail = %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/portal/billing-profile", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("profile = %d %s", w.Code, w.Body.String())
	}
}

// ─── orders ───

func TestPortalOrderListIsCustomerScoped(t *testing.T) {
	f := newPortalFixture(t)
	alice := f.portalOrder(t, f.alice.Email, `{}`)
	// The buyer snapshot is the second identity an order carries.
	aliceBuyer := f.portalOrder(t, "licence-owner@example.com", `{"email":"`+f.alice.Email+`"}`)
	bob := f.portalOrder(t, f.bob.Email, `{}`)

	code, out := f.asAlice(f.h.ListOrders, http.MethodGet, "/api/v1/portal/payments/orders", "")
	if code != http.StatusOK {
		t.Fatalf("list = %d %v", code, out)
	}
	rows := portalDataList(t, out, "orders")
	data := out["data"].(map[string]any)
	if len(rows) != 2 || data["total"] != float64(2) {
		t.Fatalf("rows = %d total %v, want 2", len(rows), data["total"])
	}
	seen := map[string]bool{}
	for _, row := range rows {
		m := row.(map[string]any)
		seen[m["code"].(string)] = true
		if m["code"] == bob.Code {
			t.Fatalf("bob's order leaked: %v", m)
		}
	}
	if !seen[alice.Code] || !seen[aliceBuyer.Code] {
		t.Fatalf("alice's orders missing: %v", seen)
	}

	// A page reports the filtered total, not the page size.
	code, out = f.asAlice(f.h.ListOrders, http.MethodGet,
		"/api/v1/portal/payments/orders?limit=1&offset=1", "")
	if code != http.StatusOK {
		t.Fatalf("paged = %d %v", code, out)
	}
	rows = portalDataList(t, out, "orders")
	if len(rows) != 1 || out["data"].(map[string]any)["total"] != float64(2) {
		t.Fatalf("page = %v", out)
	}
}

func TestPortalOrderDetailOwnershipAndPayment(t *testing.T) {
	f := newPortalFixture(t)
	order := f.portalOrder(t, f.alice.Email, `{}`)
	attempt := &model.PaymentAttempt{
		ID: store.NewID(), OrderID: order.ID, Provider: "sepay", ProviderRef: "ref-" + order.Code,
		Status: domain.AttemptPending, AmountMinor: order.TotalMinor, Currency: order.Currency,
		QRPayload: "qr-" + order.Code, CheckoutURL: "https://pay.example.com/" + order.Code,
		Raw: json.RawMessage(`{"bank":{"bank_name":"MB","account_no":"0978482700"},"description":"` + order.Code + `"}`),
		ExpiresAt: ptrTime(time.Now().Add(time.Hour)),
	}
	if err := f.s.CreatePaymentAttempt(f.ctx, attempt); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	bob := f.portalOrder(t, f.bob.Email, `{}`)

	code, out := f.asAlice(f.h.GetOrder, http.MethodGet, "/api/v1/portal/payments/orders/"+order.Code, "",
		gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusOK {
		t.Fatalf("detail = %d %v", code, out)
	}
	data := out["data"].(map[string]any)
	if data["code"] != order.Code || data["status"] != domain.OrderPending {
		t.Fatalf("order = %v", data)
	}
	payment, _ := data["payment"].(map[string]any)
	if payment["qr_payload"] != "qr-"+order.Code || payment["provider_ref"] != "ref-"+order.Code {
		t.Fatalf("payment = %v", payment)
	}

	for _, tc := range []struct{ name, code string }{
		{"foreign", bob.Code},
		{"unknown", "KGZZZZZZ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := f.asAlice(f.h.GetOrder, http.MethodGet, "/api/v1/portal/payments/orders/"+tc.code, "",
				gin.Param{Key: "code", Value: tc.code})
			if code != http.StatusNotFound || errCode(out) != "PAYMENT_ORDER_NOT_FOUND" {
				t.Fatalf("= %d %v, want 404 PAYMENT_ORDER_NOT_FOUND", code, out)
			}
		})
	}
}

func TestPortalPayReissuesOnlyAPayableOwnOrder(t *testing.T) {
	f := newPortalFixture(t)
	f.payer.attempt = &model.PaymentAttempt{
		ID: store.NewID(), Provider: "sepay", Status: domain.AttemptPending,
		QRPayload: "fresh-qr", CheckoutURL: "https://pay.example.com/fresh",
		ExpiresAt: ptrTime(time.Now().Add(time.Hour)),
	}
	pending := f.portalOrder(t, f.alice.Email, `{}`)
	paid := f.portalOrder(t, f.alice.Email, `{}`)
	if _, err := f.s.DB.NewRaw("UPDATE payment_orders SET status = 'paid', paid_at = now() WHERE id = ?", paid.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	expired := f.portalOrder(t, f.alice.Email, `{}`)
	if _, err := f.s.DB.NewRaw("UPDATE payment_orders SET expires_at = now() - interval '1 minute' WHERE id = ?", expired.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	bob := f.portalOrder(t, f.bob.Email, `{}`)

	code, out := f.asAlice(f.h.PayOrder, http.MethodPost,
		"/api/v1/portal/payments/orders/"+pending.Code+"/pay", "",
		gin.Param{Key: "code", Value: pending.Code})
	if code != http.StatusOK {
		t.Fatalf("pay = %d %v", code, out)
	}
	if len(f.payer.calls) != 1 {
		t.Fatalf("payer calls = %d, want 1", len(f.payer.calls))
	}
	if got := out["data"].(map[string]any)["payment"].(map[string]any)["qr_payload"]; got != "fresh-qr" {
		t.Fatalf("fresh payment = %v", got)
	}

	for _, tc := range []struct {
		name  string
		order *model.PaymentOrder
		code  int
		err   string
	}{
		{"paid", paid, http.StatusConflict, "PAYMENT_ORDER_NOT_PAYABLE"},
		{"expired pending", expired, http.StatusConflict, "PAYMENT_ORDER_NOT_PAYABLE"},
		{"foreign", bob, http.StatusNotFound, "PAYMENT_ORDER_NOT_FOUND"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(f.payer.calls)
			code, out := f.asAlice(f.h.PayOrder, http.MethodPost,
				"/api/v1/portal/payments/orders/"+tc.order.Code+"/pay", "",
				gin.Param{Key: "code", Value: tc.order.Code})
			if code != tc.code || errCode(out) != tc.err {
				t.Fatalf("= %d %v, want %d %s", code, out, tc.code, tc.err)
			}
			if len(f.payer.calls) != before {
				t.Fatalf("payer called for a refused order")
			}
		})
	}
}

func TestPortalPayMapsProviderFailures(t *testing.T) {
	f := newPortalFixture(t)
	order := f.portalOrder(t, f.alice.Email, `{}`)
	for _, tc := range []struct {
		err  error
		code int
		want string
	}{
		{checkout.ErrProviderUnavailable, http.StatusServiceUnavailable, "PAYMENT_PROVIDER_DISABLED"},
		{checkout.ErrProviderError, http.StatusBadGateway, "PAYMENT_PROVIDER_ERROR"},
	} {
		f.payer.err = tc.err
		code, out := f.asAlice(f.h.PayOrder, http.MethodPost,
			"/api/v1/portal/payments/orders/"+order.Code+"/pay", "",
			gin.Param{Key: "code", Value: order.Code})
		if code != tc.code || errCode(out) != tc.want {
			t.Fatalf("%v = %d %v, want %d %s", tc.err, code, out, tc.code, tc.want)
		}
	}
}

func TestPortalCancelOnlyOwnPendingOrder(t *testing.T) {
	f := newPortalFixture(t)
	rec := new(eventRecorder)
	f.h.Events = rec.emitter()
	order := f.portalOrder(t, f.alice.Email, `{}`)
	bob := f.portalOrder(t, f.bob.Email, `{}`)

	code, out := f.asAlice(f.h.CancelOrder, http.MethodPost,
		"/api/v1/portal/payments/orders/"+order.Code+"/cancel", "",
		gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusOK {
		t.Fatalf("cancel = %d %v", code, out)
	}
	if got, _ := f.s.GetPaymentOrderByCode(f.ctx, order.Code); got.Status != domain.OrderCanceled {
		t.Fatalf("status = %q, want canceled", got.Status)
	}
	if rec.count(model.EventOrderCanceled) != 1 {
		t.Fatalf("events = %+v, want one cancel", rec.events)
	}

	// A second cancel, and another customer's order, change nothing.
	code, out = f.asAlice(f.h.CancelOrder, http.MethodPost,
		"/api/v1/portal/payments/orders/"+order.Code+"/cancel", "",
		gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusConflict || errCode(out) != "PAYMENT_ORDER_NOT_PAYABLE" {
		t.Fatalf("second cancel = %d %v, want 409", code, out)
	}
	code, out = f.asAlice(f.h.CancelOrder, http.MethodPost,
		"/api/v1/portal/payments/orders/"+bob.Code+"/cancel", "",
		gin.Param{Key: "code", Value: bob.Code})
	if code != http.StatusNotFound || errCode(out) != "PAYMENT_ORDER_NOT_FOUND" {
		t.Fatalf("foreign cancel = %d %v, want 404", code, out)
	}
	if got, _ := f.s.GetPaymentOrderByCode(f.ctx, bob.Code); got.Status != domain.OrderPending {
		t.Fatalf("bob's status = %q, want pending", got.Status)
	}
	if rec.count(model.EventOrderCanceled) != 1 {
		t.Fatalf("events = %+v, want no further cancel", rec.events)
	}
}

// ─── invoices ───

func (f *portalFixture) invoice(t *testing.T, id, buyerEmail, orderID string) *model.Invoice {
	t.Helper()
	inv := &model.Invoice{
		ID: id, Number: "KG-2026-0000" + id[len(id)-1:], OrderID: orderID,
		Buyer:     json.RawMessage(`{"name":"Buyer","email":"` + buyerEmail + `"}`),
		Items:     []model.InvoiceItem{{Description: "Pro", Quantity: 1, UnitAmountMinor: 1_190_000, AmountMinor: 1_190_000}},
		SubtotalMinor: 1_190_000, TotalMinor: 1_190_000, Currency: "VND", CurrencyExp: 0,
		Status: model.InvoiceStatusIssued, CreatedAt: time.Now(),
	}
	f.invoices.invoices[id] = inv
	return inv
}

func TestPortalInvoiceListIsCustomerScoped(t *testing.T) {
	f := newPortalFixture(t)
	own := f.invoice(t, "inv-a1", f.alice.Email, "")
	f.invoice(t, "inv-b1", f.bob.Email, "")

	code, out := f.asAlice(f.h.ListInvoices, http.MethodGet, "/api/v1/portal/invoices", "")
	if code != http.StatusOK {
		t.Fatalf("list = %d %v", code, out)
	}
	rows := portalDataList(t, out, "invoices")
	if len(rows) != 1 || out["data"].(map[string]any)["total"] != float64(1) {
		t.Fatalf("rows = %v", out)
	}
	if rows[0].(map[string]any)["id"] != own.ID {
		t.Fatalf("row = %v, want %s", rows[0], own.ID)
	}
	if f.invoices.lastFilter.BuyerEmail != f.alice.Email {
		t.Fatalf("filter email = %q, want the session email", f.invoices.lastFilter.BuyerEmail)
	}
}

func TestPortalInvoiceDetailOwnershipAndOrderFallback(t *testing.T) {
	f := newPortalFixture(t)
	own := f.invoice(t, "inv-a1", f.alice.Email, "")
	// An invoice whose buyer block names somebody else still belongs to
	// the buyer of the order it was issued for.
	order := f.portalOrder(t, f.alice.Email, `{}`)
	viaOrder := f.invoice(t, "inv-a2", "accounts@example.com", order.ID)
	foreign := f.invoice(t, "inv-b1", f.bob.Email, "")

	for _, tc := range []struct {
		name string
		id   string
		code int
		err  string
	}{
		{"own", own.ID, http.StatusOK, ""},
		{"via order", viaOrder.ID, http.StatusOK, ""},
		{"foreign", foreign.ID, http.StatusNotFound, "INVOICE_NOT_FOUND"},
		{"unknown", "inv-missing", http.StatusNotFound, "INVOICE_NOT_FOUND"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := f.asAlice(f.h.GetInvoice, http.MethodGet, "/api/v1/portal/invoices/"+tc.id, "",
				gin.Param{Key: "id", Value: tc.id})
			if code != tc.code || (tc.err != "" && errCode(out) != tc.err) {
				t.Fatalf("= %d %v, want %d %s", code, out, tc.code, tc.err)
			}
			if tc.code == http.StatusOK {
				data := out["data"].(map[string]any)
				if data["id"] != tc.id {
					t.Fatalf("invoice = %v", data)
				}
				if data["items"] == nil || data["buyer"] == nil {
					t.Fatalf("detail missing buyer/items: %v", data)
				}
			}
		})
	}
}

func TestPortalInvoicePDFIsRawAndOwned(t *testing.T) {
	f := newPortalFixture(t)
	f.invoices.pdf = []byte("%PDF-1.4 test")
	own := f.invoice(t, "inv-a1", f.alice.Email, "")
	foreign := f.invoice(t, "inv-b1", f.bob.Email, "")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/portal/invoices/"+own.ID+"?format=pdf", nil)
	c.Params = gin.Params{{Key: "id", Value: own.ID}}
	c.Set("user_id", f.alice.ID)
	c.Set("email", f.alice.Email)
	f.h.GetInvoice(c)

	if c.Writer.Status() != http.StatusOK {
		t.Fatalf("pdf = %d %s", c.Writer.Status(), w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/pdf" {
		t.Fatalf("content type = %q", got)
	}
	if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, `attachment; filename="`) {
		t.Fatalf("disposition = %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("cache control = %q", got)
	}
	if string(w.Body.Bytes()) != "%PDF-1.4 test" {
		t.Fatalf("body = %q", w.Body.String())
	}

	for _, tc := range []struct {
		id   string
		code int
		err  string
	}{
		{foreign.ID, http.StatusNotFound, "INVOICE_NOT_FOUND"},
		{"inv-missing", http.StatusNotFound, "INVOICE_NOT_FOUND"},
	} {
		code, out := f.asAlice(f.h.GetInvoice, http.MethodGet,
			"/api/v1/portal/invoices/"+tc.id+"?format=pdf", "",
			gin.Param{Key: "id", Value: tc.id})
		if code != tc.code || errCode(out) != tc.err {
			t.Fatalf("%s = %d %v, want %d %s", tc.id, code, out, tc.code, tc.err)
		}
	}

	code, out := f.asAlice(f.h.GetInvoice, http.MethodGet,
		"/api/v1/portal/invoices/"+own.ID+"?format=xml", "",
		gin.Param{Key: "id", Value: own.ID})
	if code != http.StatusBadRequest || errCode(out) != "BAD_REQUEST" {
		t.Fatalf("bad format = %d %v, want 400", code, out)
	}
}

// ─── billing profile ───

func TestPortalBillingProfileDefaultsToEmpty(t *testing.T) {
	f := newPortalFixture(t)
	code, out := f.asAlice(f.h.GetBillingProfile, http.MethodGet, "/api/v1/portal/billing-profile", "")
	if code != http.StatusOK {
		t.Fatalf("get = %d %v", code, out)
	}
	buyer := out["data"].(map[string]any)["buyer"].(map[string]any)
	if len(buyer) != 0 {
		t.Fatalf("buyer = %v, want {}", buyer)
	}
}

func TestPortalBillingProfileValidation(t *testing.T) {
	f := newPortalFixture(t)
	cases := []struct {
		name string
		body string
		code int
		err  string
	}{
		{"missing name", `{"tax_code":"0101"}`, http.StatusBadRequest, "BAD_REQUEST"},
		{"blank name", `{"name":"  "}`, http.StatusBadRequest, "BAD_REQUEST"},
		{"tax code required", `{"name":"Alice","request_invoice":true}`, http.StatusUnprocessableEntity, "PAYMENT_TAX_CODE_REQUIRED"},
		{"nested tax code required", `{"buyer":{"name":"Alice","request_invoice":true}}`, http.StatusUnprocessableEntity, "PAYMENT_TAX_CODE_REQUIRED"},
		{"name too long", `{"name":"` + strings.Repeat("a", 201) + `"}`, http.StatusBadRequest, "BAD_REQUEST"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := f.asAlice(f.h.SaveBillingProfile, http.MethodPut, "/api/v1/portal/billing-profile", tc.body)
			if code != tc.code || errCode(out) != tc.err {
				t.Fatalf("= %d %v, want %d %s", code, out, tc.code, tc.err)
			}
		})
	}
}

func TestPortalBillingProfileRoundTripAndAudit(t *testing.T) {
	f := newPortalFixture(t)
	body := `{"buyer":{"name":"Nguyen Van A","company_name":"ACME","tax_code":"0101234567",` +
		`"address":"1 Le Loi","email":"billing@example.com","phone":"0909","request_invoice":true}}`
	code, out := f.asAlice(f.h.SaveBillingProfile, http.MethodPut, "/api/v1/portal/billing-profile", body)
	if code != http.StatusOK {
		t.Fatalf("put = %d %v", code, out)
	}
	saved := out["data"].(map[string]any)["buyer"].(map[string]any)
	if saved["name"] != "Nguyen Van A" || saved["tax_code"] != "0101234567" || saved["request_invoice"] != true {
		t.Fatalf("saved = %v", saved)
	}

	code, out = f.asAlice(f.h.GetBillingProfile, http.MethodGet, "/api/v1/portal/billing-profile", "")
	if code != http.StatusOK {
		t.Fatalf("get = %d %v", code, out)
	}
	got := out["data"].(map[string]any)["buyer"].(map[string]any)
	if got["company_name"] != "ACME" {
		t.Fatalf("get = %v", got)
	}

	// The flat spelling is accepted too, and upserts the same row.
	code, out = f.asAlice(f.h.SaveBillingProfile, http.MethodPut, "/api/v1/portal/billing-profile",
		`{"name":"Nguyen Van B","request_invoice":false}`)
	if code != http.StatusOK {
		t.Fatalf("flat put = %d %v", code, out)
	}
	got = out["data"].(map[string]any)["buyer"].(map[string]any)
	if got["name"] != "Nguyen Van B" || got["company_name"] != nil || got["request_invoice"] != false {
		t.Fatalf("flat saved = %v", got)
	}
	var rows int
	if err := f.s.DB.NewRaw("SELECT count(*) FROM customer_billing_profiles WHERE user_id = ?", f.alice.ID).
		Scan(f.ctx, &rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("profile rows = %d, want 1", rows)
	}

	// The write is audited (the store writes the line in the background).
	deadline := time.Now().Add(2 * time.Second)
	for {
		var audits int
		if err := f.s.DB.NewRaw(
			`SELECT count(*) FROM audit_logs WHERE entity = 'customer_billing_profile' AND entity_id = ?`,
			f.alice.ID).Scan(f.ctx, &audits); err != nil {
			t.Fatal(err)
		}
		if audits >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit rows = %d, want the two updates", audits)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPortalBillingProfileIsPerUser(t *testing.T) {
	f := newPortalFixture(t)
	if code, out := f.asAlice(f.h.SaveBillingProfile, http.MethodPut, "/api/v1/portal/billing-profile",
		`{"name":"Alice","tax_code":"0101"}`); code != http.StatusOK {
		t.Fatalf("alice put = %d %v", code, out)
	}
	code, out := f.asBob(f.h.GetBillingProfile, http.MethodGet, "/api/v1/portal/billing-profile", "")
	if code != http.StatusOK {
		t.Fatalf("bob get = %d %v", code, out)
	}
	if buyer := out["data"].(map[string]any)["buyer"].(map[string]any); len(buyer) != 0 {
		t.Fatalf("bob's buyer = %v, want his own empty profile", buyer)
	}
}
