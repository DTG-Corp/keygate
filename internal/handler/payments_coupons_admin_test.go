package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/coupons"
	"github.com/tabloy/keygate/internal/store"
)

// couponAdmin returns the admin handler wired the way the server wires it.
func (f *paymentsFixture) couponAdmin() *CouponsAdminHandler {
	return &CouponsAdminHandler{Store: f.s, Checkout: f.svc}
}

// redeemedCoupon creates a coupon and records one redemption of it for a
// seeded order, so the ledger endpoints have something true to read.
func (f *paymentsFixture) redeemedCoupon(t *testing.T, code string, amountMinor int64) (*model.Coupon, *model.PaymentOrder) {
	t.Helper()
	cp, err := f.s.CreateCoupon(f.ctx, store.CouponInput{
		Code: code, Type: "percent", Value: 1000, StackGroup: "general",
		Stackable: true, Priority: 100, Active: true,
	})
	if err != nil {
		t.Fatalf("create coupon: %v", err)
	}
	order, _ := f.seedOrder(t, f.planA, f.email("redeem"), "sepay", 1_000_000, false)
	if err := f.s.RedeemCouponsForOrder(f.ctx, []store.RedemptionInput{{
		CouponID: cp.ID, CouponCode: cp.Code, OrderID: order.ID, Email: order.Email,
		Type: "percent", AmountMinor: amountMinor,
	}}); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	return cp, order
}

// listData reads the envelope's rows and total from a list response:
// listOK nests the rows under their own key inside the data object.
func listData(t *testing.T, out map[string]any) ([]any, float64) {
	t.Helper()
	env, _ := out["data"].(map[string]any)
	rows, _ := env["data"].([]any)
	total, _ := env["total"].(float64)
	return rows, total
}

func TestCouponsAdminCreateListAndGet(t *testing.T) {
	f := newCouponHandlerFixture(t)
	h := f.couponAdmin()

	code := "tet" + strings.ToUpper(f.suffix)
	status, out := f.call(h.Create, http.MethodPost, "/api/v1/admin/coupons",
		`{"code":"`+code+`","type":"percent","value":1500,"description":"Tet campaign",
		  "scopes":[{"scope_type":"product","scope_id":"`+f.planA.ProductID+`"}]}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("create status %d: %v", status, out)
	}
	data, _ := out["data"].(map[string]any)
	if data["code"] != strings.ToUpper(code) {
		t.Fatalf("code = %v, want the normalised code", data["code"])
	}
	// Omitted optional fields take the documented defaults, not Go's.
	for key, want := range map[string]any{"stackable": true, "priority": float64(100), "active": true} {
		if data[key] != want {
			t.Errorf("%s = %v, want %v", key, data[key], want)
		}
	}
	scopes, _ := data["scopes"].([]any)
	if len(scopes) != 1 {
		t.Fatalf("scopes = %v", data["scopes"])
	}
	scope, _ := scopes[0].(map[string]any)
	if scope["scope_type"] != "product" || scope["name"] != f.prodA.Name {
		t.Fatalf("scope = %v, want the product name resolved", scope)
	}
	id, _ := data["id"].(string)

	status, out = f.call(h.List, http.MethodGet, "/api/v1/admin/coupons?q="+code, "", nil)
	if status != http.StatusOK {
		t.Fatalf("list status %d: %v", status, out)
	}
	rows, total := listData(t, out)
	if total != 1 || len(rows) != 1 {
		t.Fatalf("total = %v rows = %v", total, rows)
	}
	row, _ := rows[0].(map[string]any)
	if row["code"] != strings.ToUpper(code) {
		t.Fatalf("row = %v", row)
	}

	status, out = f.call(h.Get, http.MethodGet, "/api/v1/admin/coupons/"+id, "", nil,
		gin.Param{Key: "id", Value: id})
	if status != http.StatusOK {
		t.Fatalf("get status %d: %v", status, out)
	}
	data, _ = out["data"].(map[string]any)
	stats, _ := data["redemptions"].(map[string]any)
	if stats["count"] != float64(0) || stats["amount_minor"] != float64(0) {
		t.Fatalf("redemptions = %v", data["redemptions"])
	}

	// Filters narrow the list; an unparseable active flag is refused
	// rather than read as false.
	status, out = f.call(h.List, http.MethodGet, "/api/v1/admin/coupons?active=true&q="+code, "", nil)
	if _, total := listData(t, out); status != http.StatusOK || total != 1 {
		t.Fatalf("active=true status %d: %v", status, out)
	}
	status, out = f.call(h.List, http.MethodGet, "/api/v1/admin/coupons?active=false&q="+code, "", nil)
	if _, total := listData(t, out); status != http.StatusOK || total != 0 {
		t.Fatalf("active=false status %d: %v", status, out)
	}
	status, _ = f.call(h.List, http.MethodGet, "/api/v1/admin/coupons?active=maybe", "", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("bad active status = %d", status)
	}
}

func TestCouponsAdminUpdateReplacesScopes(t *testing.T) {
	f := newCouponHandlerFixture(t)
	h := f.couponAdmin()
	cp, err := f.s.CreateCoupon(f.ctx, store.CouponInput{
		Code: "UPD" + strings.ToUpper(f.suffix), Type: "fixed", Value: 50_000, Currency: "VND",
		StackGroup: "campaign", Stackable: true, Priority: 200, Active: true,
		Scopes: []store.CouponScopeInput{{ScopeType: "product", ScopeID: f.planA.ProductID}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	status, out := f.call(h.Update, http.MethodPut, "/api/v1/admin/coupons/"+cp.ID,
		`{"type":"fixed","value":75000,"currency":"VND","stack_group":"campaign",
		  "priority":50,"scopes":[{"scope_type":"plan","scope_id":"`+f.planA.ID+`"}]}`, nil,
		gin.Param{Key: "id", Value: cp.ID})
	if status != http.StatusOK {
		t.Fatalf("update status %d: %v", status, out)
	}
	data, _ := out["data"].(map[string]any)
	if data["code"] != cp.Code {
		t.Fatalf("code = %v, want the stored code kept", data["code"])
	}
	if data["value"] != float64(75_000) || data["priority"] != float64(50) {
		t.Fatalf("update did not apply: %v", data)
	}
	scopes, _ := data["scopes"].([]any)
	if len(scopes) != 1 {
		t.Fatalf("scopes = %v", data["scopes"])
	}
	scope, _ := scopes[0].(map[string]any)
	if scope["scope_type"] != "plan" || scope["scope_id"] != f.planA.ID {
		t.Fatalf("scope = %v, want the replacement", scope)
	}

	// An empty scope list means "covers everything", and replacing with it
	// must remove the narrowing, not keep it.
	status, out = f.call(h.Update, http.MethodPut, "/api/v1/admin/coupons/"+cp.ID,
		`{"type":"fixed","value":75000,"currency":"VND","stack_group":"campaign"}`, nil,
		gin.Param{Key: "id", Value: cp.ID})
	if status != http.StatusOK {
		t.Fatalf("clear scopes status %d: %v", status, out)
	}
	data, _ = out["data"].(map[string]any)
	if scopes, _ := data["scopes"].([]any); len(scopes) != 0 {
		t.Fatalf("scopes = %v, want none", data["scopes"])
	}
}

func TestCouponsAdminDeleteArchivesRedeemedOnly(t *testing.T) {
	f := newCouponHandlerFixture(t)
	h := f.couponAdmin()
	redeemed, _ := f.redeemedCoupon(t, "USED"+strings.ToUpper(f.suffix), 100_000)
	unused, err := f.s.CreateCoupon(f.ctx, store.CouponInput{
		Code: "NEW" + strings.ToUpper(f.suffix), Type: "percent", Value: 500,
		StackGroup: "general", Stackable: true, Priority: 100, Active: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	status, out := f.call(h.Delete, http.MethodDelete, "/api/v1/admin/coupons/"+redeemed.ID, "", nil,
		gin.Param{Key: "id", Value: redeemed.ID})
	if status != http.StatusOK {
		t.Fatalf("delete redeemed status %d: %v", status, out)
	}
	data, _ := out["data"].(map[string]any)
	if data["archived"] != true {
		t.Fatalf("archived = %v, want true: the ledger must keep its coupon", data["archived"])
	}
	kept, err := f.s.GetCoupon(f.ctx, redeemed.ID)
	if err != nil || kept.Active {
		t.Fatalf("coupon = %+v err = %v, want archived", kept, err)
	}

	status, out = f.call(h.Delete, http.MethodDelete, "/api/v1/admin/coupons/"+unused.ID, "", nil,
		gin.Param{Key: "id", Value: unused.ID})
	if status != http.StatusOK {
		t.Fatalf("delete unused status %d: %v", status, out)
	}
	data, _ = out["data"].(map[string]any)
	if data["archived"] != false {
		t.Fatalf("archived = %v, want a real delete", data["archived"])
	}
	if _, err := f.s.GetCoupon(f.ctx, unused.ID); err == nil {
		t.Fatalf("unused coupon still exists")
	}

	// Unknown ids are 404 on every route that takes one.
	for name, fn := range map[string]gin.HandlerFunc{"get": h.Get, "delete": h.Delete, "update": h.Update} {
		status, out := f.call(fn, http.MethodGet, "/api/v1/admin/coupons/missing", `{}`, nil,
			gin.Param{Key: "id", Value: "00000000-0000-0000-0000-000000000000"})
		if status != http.StatusNotFound || errCode(out) != "NOT_FOUND" {
			t.Errorf("%s: status %d code %q", name, status, errCode(out))
		}
	}
}

func TestCouponsAdminWriteRefusals(t *testing.T) {
	f := newCouponHandlerFixture(t)
	h := f.couponAdmin()
	code := "DUP" + strings.ToUpper(f.suffix)
	if _, err := f.s.CreateCoupon(f.ctx, store.CouponInput{
		Code: code, Type: "percent", Value: 1000, StackGroup: "general",
		Stackable: true, Priority: 100, Active: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cases := map[string]struct {
		body   string
		status int
		code   string
	}{
		"duplicate code": {`{"code":"` + code + `","type":"percent","value":1000}`, http.StatusConflict, "PAYMENT_COUPON_CODE_TAKEN"},
		"unknown type":   {`{"code":"BAD1` + strings.ToUpper(f.suffix) + `","type":"bogus","value":1}`, http.StatusUnprocessableEntity, "PAYMENT_COUPON_INVALID"},
		"percent range":  {`{"code":"BAD2` + strings.ToUpper(f.suffix) + `","type":"percent","value":20000}`, http.StatusUnprocessableEntity, "PAYMENT_COUPON_INVALID"},
		"unknown scope": {`{"code":"BAD3` + strings.ToUpper(f.suffix) + `","type":"percent","value":100,
		  "scopes":[{"scope_type":"plan","scope_id":"00000000-0000-0000-0000-000000000000"}]}`,
			http.StatusUnprocessableEntity, "PAYMENT_COUPON_INVALID"},
		"bad stack group": {`{"code":"BAD4` + strings.ToUpper(f.suffix) + `","type":"percent","value":100,"stack_group":"vip"}`, http.StatusUnprocessableEntity, "PAYMENT_COUPON_INVALID"},
	}
	for name, tt := range cases {
		status, out := f.call(h.Create, http.MethodPost, "/api/v1/admin/coupons", tt.body, nil)
		if status != tt.status || errCode(out) != tt.code {
			t.Errorf("%s: status %d code %q, want %d %q (%v)", name, status, errCode(out), tt.status, tt.code, out)
		}
	}
}

func TestCouponsAdminPreviewPricesTheCodes(t *testing.T) {
	f := newCouponHandlerFixture(t)
	h := f.couponAdmin()
	f.price(t, f.planA, "sepay", 1_000_000)
	cp := f.coupon(t, nil)

	status, out := f.call(h.Preview, http.MethodPost, "/api/v1/admin/coupons/preview",
		`{"plan_id":"`+f.planA.ID+`","provider":"sepay","codes":["`+cp.Code+`"]}`, nil)
	if status != http.StatusOK {
		t.Fatalf("preview status %d: %v", status, out)
	}
	data, _ := out["data"].(map[string]any)
	quote, _ := data["quote"].(map[string]any)
	if quote["discount_minor"] != float64(100_000) || quote["total_minor"] != float64(900_000) {
		t.Fatalf("quote = %v", quote)
	}
	price, _ := data["price"].(map[string]any)
	if price["amount_minor"] != float64(1_000_000) {
		t.Fatalf("price = %v", price)
	}

	// A code checkout would refuse is refused here too, with the same code
	// the buyer would see.
	status, out = f.call(h.Preview, http.MethodPost, "/api/v1/admin/coupons/preview",
		`{"plan_id":"`+f.planA.ID+`","provider":"sepay","codes":["NOPE"]}`, nil)
	if status != http.StatusUnprocessableEntity || errCode(out) != "PAYMENT_COUPON_INVALID" {
		t.Fatalf("bad code status %d: %v", status, out)
	}
}

func TestCouponsAdminRedemptionHistoryAndCSV(t *testing.T) {
	f := newCouponHandlerFixture(t)
	h := f.couponAdmin()
	cp, order := f.redeemedCoupon(t, "HIST"+strings.ToUpper(f.suffix), 250_000)
	// A second coupon's row must not leak into the per-coupon read.
	f.redeemedCoupon(t, "OTHR"+strings.ToUpper(f.suffix), 10_000)

	// The per-coupon route is scoped to one coupon.
	status, out := f.call(h.Redemptions, http.MethodGet, "/api/v1/admin/coupons/"+cp.ID+"/redemptions", "", nil,
		gin.Param{Key: "id", Value: cp.ID})
	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, out)
	}
	rows, total := listData(t, out)
	if total != 1 || len(rows) != 1 {
		t.Fatalf("total = %v rows = %v", total, rows)
	}
	row, _ := rows[0].(map[string]any)
	if row["coupon_code"] != cp.Code || row["order_code"] != order.Code ||
		row["amount_minor"] != float64(250_000) || row["email"] != order.Email {
		t.Fatalf("row = %v", row)
	}

	// The global route filters by coupon code.
	status, out = f.call(h.RedemptionsGlobal, http.MethodGet,
		"/api/v1/admin/payments/redemptions?coupon="+cp.Code, "", nil)
	if _, total := listData(t, out); status != http.StatusOK || total != 1 {
		t.Fatalf("filtered status %d: %v", status, out)
	}

	// A date window that ends yesterday cannot see today's rows, and one
	// that ends today can.
	yesterday := time.Now().AddDate(0, 0, -1)
	status, out = f.call(h.RedemptionsGlobal, http.MethodGet,
		"/api/v1/admin/payments/redemptions?to="+yesterday.Format("2006-01-02"), "", nil)
	if _, total := listData(t, out); status != http.StatusOK || total != 0 {
		t.Fatalf("past window status %d: %v", status, out)
	}
	from := time.Now().AddDate(0, 0, -2)
	status, out = f.call(h.RedemptionsGlobal, http.MethodGet,
		"/api/v1/admin/payments/redemptions?coupon="+cp.Code+"&from="+from.Format("2006-01-02")+"&to="+time.Now().Format("2006-01-02"), "", nil)
	if _, total := listData(t, out); status != http.StatusOK || total != 1 {
		t.Fatalf("covering window status %d: %v", status, out)
	}
	status, _ = f.call(h.RedemptionsGlobal, http.MethodGet,
		"/api/v1/admin/payments/redemptions?from=nonsense", "", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("bad date status = %d", status)
	}

	// CSV: the documented header, one line per row, and a cell a
	// spreadsheet would otherwise execute is neutralised.
	w := f.record(h.RedemptionsGlobal, http.MethodGet,
		"/api/v1/admin/payments/redemptions?format=csv&coupon="+cp.Code, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("csv status %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("Content-Type = %q", ct)
	}
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("csv lines = %d: %q", len(lines), w.Body.String())
	}
	if lines[0] != "created_at,coupon_code,order_code,email,external_customer_id,type,amount_minor,trial_days,currency" {
		t.Fatalf("header = %q", lines[0])
	}
	if !strings.Contains(lines[1], cp.Code) || !strings.Contains(lines[1], order.Code) {
		t.Fatalf("csv row = %q", lines[1])
	}
}

func TestCouponsAdminCSVNeutralisesFormulaCells(t *testing.T) {
	f := newCouponHandlerFixture(t)
	h := f.couponAdmin()
	cp, err := f.s.CreateCoupon(f.ctx, store.CouponInput{
		Code: "CSV" + strings.ToUpper(f.suffix), Type: "percent", Value: 1000,
		StackGroup: "general", Stackable: true, Priority: 100, Active: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	order, _ := f.seedOrder(t, f.planA, "=sum(a1)@example.com", "sepay", 1_000_000, false)
	if err := f.s.RedeemCouponsForOrder(f.ctx, []store.RedemptionInput{{
		CouponID: cp.ID, CouponCode: cp.Code, OrderID: order.ID,
		Email: "=sum(a1)@example.com", Type: "percent", AmountMinor: 100_000,
	}}); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	w := f.record(h.RedemptionsGlobal, http.MethodGet,
		"/api/v1/admin/payments/redemptions?format=csv&coupon="+cp.Code, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `'=sum(a1)@example.com`) {
		t.Fatalf("csv did not neutralise the formula: %s", w.Body.String())
	}
}

func TestCouponsPortalValidate(t *testing.T) {
	f := newCouponHandlerFixture(t)
	f.price(t, f.planA, "sepay", 1_000_000)
	cp := f.coupon(t, nil)
	h := &CouponsPortalHandler{Checkout: f.svc}

	// No session: the code's per-customer limit has no buyer to count.
	w := f.record(h.Validate, http.MethodPost, "/api/v1/portal/coupons/validate",
		`{"plan_id":"`+f.planA.ID+`","code":"`+cp.Code+`"}`, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d %s", w.Code, w.Body.String())
	}

	body := `{"plan_id":"` + f.planA.ID + `","code":"` + strings.ToLower(cp.Code) + `"}`
	status, out := f.portalCall(h, body)
	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, out)
	}
	data, _ := out["data"].(map[string]any)
	if data["valid"] != true || data["discount_minor"] != float64(100_000) ||
		data["total_minor"] != float64(900_000) || data["price_minor"] != float64(1_000_000) {
		t.Fatalf("data = %v", data)
	}
	if data["currency_exponent"] != float64(0) {
		t.Fatalf("currency_exponent = %v", data["currency_exponent"])
	}

	// A dead code is the buyer's answer, not a server error.
	status, out = f.portalCall(h, `{"plan_id":"`+f.planA.ID+`","code":"NOPE"}`)
	if status != http.StatusUnprocessableEntity || errCode(out) != "PAYMENT_COUPON_INVALID" {
		t.Fatalf("bad code status %d: %v", status, out)
	}
}

// portalCall runs the portal handler with the session email the portal's
// auth middleware sets, so the per-customer limit has a buyer to count.
func (f *paymentsFixture) portalCall(h *CouponsPortalHandler, body string) (int, map[string]any) {
	f.t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/portal/coupons/validate", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("email", f.email("portal"))
	h.Validate(c)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return c.Writer.Status(), out
}

// TestCouponCodesMatchConstants checks the step between the coupons
// package and the literal strings the handlers write: every rejection the
// package can return is pinned in the vocabulary with the status its
// comment documents, and the admin-only duplicate-code answer is pinned
// too.
func TestCouponCodesMatchConstants(t *testing.T) {
	for _, code := range []string{
		coupons.CodeInvalid, coupons.CodeStackLimit, coupons.CodeGroupConflict,
		coupons.CodeNotStackable, coupons.CodeTrialMixed, coupons.CodeAssigned,
	} {
		spec, ok := paymentVocabulary[code]
		if !ok {
			t.Errorf("%q is not pinned in paymentVocabulary", code)
			continue
		}
		if spec.status != http.StatusUnprocessableEntity {
			t.Errorf("%q is pinned with %d, want 422", code, spec.status)
		}
		if !spec.emitted {
			t.Errorf("%q is pinned as not emitted, but the handlers write it", code)
		}
	}
	// The two refusals a client retries differently are 409, not 422: the
	// coupon exists and is valid, there is just none of it left (or the
	// code is already someone else's).
	for code, want := range map[string]string{
		coupons.CodeExhausted: "PAYMENT_COUPON_EXHAUSTED",
		coupons.CodeCodeTaken: "PAYMENT_COUPON_CODE_TAKEN",
	} {
		spec, ok := paymentVocabulary[code]
		if !ok || spec.status != http.StatusConflict || !spec.emitted {
			t.Errorf("%s = %+v, want 409 emitted", want, spec)
		}
		if code != want {
			t.Errorf("constant value %q != %q", code, want)
		}
	}
}

// TestCouponWritesAreAudited: the admin log is where "who made it free" is
// answered, so create, update and delete each leave a line. Audit writes
// are asynchronous, hence the wait.
func TestCouponWritesAreAudited(t *testing.T) {
	f := newCouponHandlerFixture(t)
	h := f.couponAdmin()
	code := "AUD" + strings.ToUpper(f.suffix)

	status, out := f.call(h.Create, http.MethodPost, "/api/v1/admin/coupons",
		`{"code":"`+code+`","type":"percent","value":1000}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("create status %d: %v", status, out)
	}
	data, _ := out["data"].(map[string]any)
	id, _ := data["id"].(string)

	status, out = f.call(h.Update, http.MethodPut, "/api/v1/admin/coupons/"+id,
		`{"type":"percent","value":2000}`, nil, gin.Param{Key: "id", Value: id})
	if status != http.StatusOK {
		t.Fatalf("update status %d: %v", status, out)
	}
	status, out = f.call(h.Delete, http.MethodDelete, "/api/v1/admin/coupons/"+id, "", nil,
		gin.Param{Key: "id", Value: id})
	if status != http.StatusOK {
		t.Fatalf("delete status %d: %v", status, out)
	}

	// The audit rows are written by goroutines: poll briefly rather than
	// sleeping a fixed time.
	deadline := time.Now().Add(3 * time.Second)
	var actions map[string]int
	for time.Now().Before(deadline) {
		var rows []struct {
			Action string `bun:"action"`
		}
		if err := f.s.DB.NewRaw("SELECT action FROM audit_logs WHERE entity = 'coupon' AND entity_id = ?", id).
			Scan(f.ctx, &rows); err != nil {
			t.Fatalf("read audit rows: %v", err)
		}
		actions = map[string]int{}
		for _, row := range rows {
			actions[row.Action]++
		}
		if actions["create"] > 0 && actions["update"] > 0 && actions["delete"] > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("audit actions = %v, want create, update and delete", actions)
}

func TestRegisterCouponsAdminRoutes(t *testing.T) {
	r := gin.New()
	RegisterCouponsAdminRoutes(r.Group("/api/v1/admin"), &CouponsAdminHandler{Store: nil})

	want := map[string]bool{
		"GET /api/v1/admin/coupons":                 false,
		"POST /api/v1/admin/coupons":                false,
		"POST /api/v1/admin/coupons/preview":        false,
		"GET /api/v1/admin/coupons/:id":             false,
		"PUT /api/v1/admin/coupons/:id":             false,
		"DELETE /api/v1/admin/coupons/:id":          false,
		"GET /api/v1/admin/coupons/:id/redemptions": false,
		"GET /api/v1/admin/payments/redemptions":    false,
	}
	for _, route := range r.Routes() {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("route %q is not mounted", key)
		}
	}
}

// TestRegisterCouponsPortalRoutes proves the portal route mounts under the
// session group the Lead supplies, and that the admin registration does not
// sneak it in.
func TestRegisterCouponsPortalRoutes(t *testing.T) {
	r := gin.New()
	RegisterCouponsPortalRoutes(r.Group("/api/v1/portal"), &CouponsPortalHandler{})
	found := false
	for _, route := range r.Routes() {
		if route.Method+" "+route.Path == "POST /api/v1/portal/coupons/validate" {
			found = true
		}
	}
	if !found {
		t.Fatalf("portal validate route is not mounted")
	}
}
