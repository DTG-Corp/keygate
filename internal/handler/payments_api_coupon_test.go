package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/coupons"
	"github.com/tabloy/keygate/internal/store"
)

// couponFixture is the payments handler fixture with the coupon service
// wired the way the server wires it.
func newCouponHandlerFixture(t *testing.T) *paymentsFixture {
	t.Helper()
	f := newPaymentsFixture(t)
	f.svc.Coupons = coupons.New(f.s)
	return f
}

func (f *paymentsFixture) coupon(t *testing.T, mutate func(*store.CouponInput)) *model.Coupon {
	t.Helper()
	in := store.CouponInput{
		Code: "H" + strings.ToUpper(f.suffix), Type: "percent", Value: 1000,
		StackGroup: "general", Stackable: true, Priority: 100, Active: true,
	}
	if mutate != nil {
		mutate(&in)
	}
	c, err := f.s.CreateCoupon(f.ctx, in)
	if err != nil {
		t.Fatalf("create coupon: %v", err)
	}
	return c
}

func TestCreateOrderReturnsCouponBreakdown(t *testing.T) {
	f := newCouponHandlerFixture(t)
	f.price(t, f.planA, "sepay", 1_000_000)
	c := f.coupon(t, nil)
	key := f.key(t, model.ScopePaymentsWrite)

	code, out := f.call(f.api.Create, http.MethodPost, "/api/v1/payments/orders",
		`{"plan_id":"`+f.planA.ID+`","email":"`+f.email("coupon-api")+`","provider":"sepay","quantity":1,
		  "coupon_codes":["`+strings.ToLower(c.Code)+`"]}`, key)
	if code != http.StatusCreated {
		t.Fatalf("status %d: %v", code, out)
	}
	data, _ := out["data"].(map[string]any)
	for key, want := range map[string]float64{
		"subtotal_minor": 1_000_000, "discount_minor": 100_000, "total_minor": 900_000,
		"subtotal": 1_000_000, "discount": 100_000, "total": 900_000, "amount": 900_000,
	} {
		if got, ok := data[key].(float64); !ok || got != want {
			t.Errorf("%s = %v, want %v", key, data[key], want)
		}
	}
	if got, ok := data["tax"].(float64); !ok || got != data["tax_minor"].(float64) {
		t.Errorf("tax alias = %v, tax_minor = %v", data["tax"], data["tax_minor"])
	}
	discounts, ok := data["discounts"].([]any)
	if !ok || len(discounts) != 1 {
		t.Fatalf("discounts = %v", data["discounts"])
	}
	row, _ := discounts[0].(map[string]any)
	if row["code"] != c.Code || row["type"] != "percent" || row["amount"] != float64(100_000) {
		t.Fatalf("discount row = %v", row)
	}

	// The same breakdown comes back from the status read.
	orderCode, _ := data["code"].(string)
	code, out = f.call(f.api.Get, http.MethodGet, "/api/v1/payments/orders/"+orderCode, "", key,
		gin.Param{Key: "code", Value: orderCode})
	if code != http.StatusOK {
		t.Fatalf("get status %d: %v", code, out)
	}
	got, _ := out["data"].(map[string]any)
	if discounts, ok := got["discounts"].([]any); !ok || len(discounts) != 1 {
		t.Fatalf("get discounts = %v", got["discounts"])
	}
}

func TestCreateOrderRequiresTaxCode(t *testing.T) {
	f := newPaymentsFixture(t)
	f.price(t, f.planA, "sepay", 1_000_000)
	key := f.key(t, model.ScopePaymentsWrite)
	email := f.email("invoice")

	code, out := f.call(f.api.Create, http.MethodPost, "/api/v1/payments/orders",
		`{"plan_id":"`+f.planA.ID+`","email":"`+email+`","provider":"sepay","quantity":1,
		  "buyer":{"name":"ACME","request_invoice":true}}`, key)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d: %v", code, out)
	}
	if got := errCode(out); got != "PAYMENT_TAX_CODE_REQUIRED" {
		t.Fatalf("code = %q", got)
	}
	var orders int
	if err := f.s.DB.NewRaw("SELECT count(*) FROM payment_orders WHERE email = ?", email).
		Scan(f.ctx, &orders); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if orders != 0 {
		t.Fatalf("a refused invoice request wrote an order")
	}

	// A 13-digit code typed with separators is accepted and stored as the
	// digits an invoice is issued against.
	code, out = f.call(f.api.Create, http.MethodPost, "/api/v1/payments/orders",
		`{"plan_id":"`+f.planA.ID+`","email":"`+email+`","provider":"sepay","quantity":1,
		  "buyer":{"name":"ACME","request_invoice":true,"tax_code":"010 123-4567.001"}}`, key)
	if code != http.StatusCreated {
		t.Fatalf("with tax code status %d: %v", code, out)
	}
	data, _ := out["data"].(map[string]any)
	order, err := f.s.GetPaymentOrderByCode(f.ctx, data["code"].(string))
	if err != nil {
		t.Fatalf("re-read order: %v", err)
	}
	var buyer struct {
		RequestInvoice bool   `json:"request_invoice"`
		TaxCode        string `json:"tax_code"`
	}
	if err := json.Unmarshal(order.Buyer, &buyer); err != nil {
		t.Fatalf("decode buyer: %v", err)
	}
	if !buyer.RequestInvoice || buyer.TaxCode != "0101234567001" {
		t.Fatalf("stored buyer = %+v", buyer)
	}

	// A malformed code is refused the same way.
	code, out = f.call(f.api.Create, http.MethodPost, "/api/v1/payments/orders",
		`{"plan_id":"`+f.planA.ID+`","email":"`+f.email("invoice2")+`","provider":"sepay","quantity":1,
		  "buyer":{"request_invoice":true,"tax_code":"12345"}}`, key)
	if code != http.StatusUnprocessableEntity || errCode(out) != "PAYMENT_TAX_CODE_REQUIRED" {
		t.Fatalf("malformed status %d: %v", code, out)
	}
}

func TestCreateOrderCouponErrorsMapToDocumentedCodes(t *testing.T) {
	f := newCouponHandlerFixture(t)
	f.price(t, f.planA, "sepay", 1_000_000)
	key := f.key(t, model.ScopePaymentsWrite)
	email := f.email("coupon-errors")
	exhausted := f.coupon(t, func(in *store.CouponInput) {
		in.Code = "EX" + strings.ToUpper(f.suffix)
		in.MaxRedemptions = 1
	})
	if _, err := f.s.DB.NewRaw("UPDATE coupons SET redeemed_count = 1 WHERE id = ?", exhausted.ID).
		Exec(f.ctx); err != nil {
		t.Fatalf("bump: %v", err)
	}
	general := f.coupon(t, func(in *store.CouponInput) { in.Code = "G1" + strings.ToUpper(f.suffix) })
	general2 := f.coupon(t, func(in *store.CouponInput) { in.Code = "G2" + strings.ToUpper(f.suffix) })

	cases := map[string]struct {
		codes  string
		status int
		code   string
	}{
		"unknown":        {`["NOPE"]`, http.StatusUnprocessableEntity, "PAYMENT_COUPON_INVALID"},
		"exhausted":      {`["` + exhausted.Code + `"]`, http.StatusConflict, "PAYMENT_COUPON_EXHAUSTED"},
		"group conflict": {`["` + general.Code + `","` + general2.Code + `"]`, http.StatusUnprocessableEntity, "PAYMENT_COUPON_GROUP_CONFLICT"},
	}
	for name, tt := range cases {
		status, out := f.call(f.api.Create, http.MethodPost, "/api/v1/payments/orders",
			`{"plan_id":"`+f.planA.ID+`","email":"`+email+`","provider":"sepay","quantity":1,"coupon_codes":`+tt.codes+`}`, key)
		if status != tt.status || errCode(out) != tt.code {
			t.Errorf("%s: status %d code %q, want %d %q (%v)", name, status, errCode(out), tt.status, tt.code, out)
		}
	}

	// Best effort: the dead codes are skipped and the order is full price.
	status, out := f.call(f.api.Create, http.MethodPost, "/api/v1/payments/orders",
		`{"plan_id":"`+f.planA.ID+`","email":"`+email+`","provider":"sepay","quantity":1,
		  "coupon_codes":["NOPE"],"ignore_invalid_coupons":true}`, key)
	if status != http.StatusCreated {
		t.Fatalf("ignore status %d: %v", status, out)
	}
	data, _ := out["data"].(map[string]any)
	if data["discount_minor"] != float64(0) || data["total_minor"] != float64(1_000_000) {
		t.Fatalf("best-effort order = %v", data)
	}
	if _, ok := data["discounts"]; ok {
		t.Fatalf("best-effort order reports discounts: %v", data["discounts"])
	}
}
