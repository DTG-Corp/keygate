package handler

import (
	"net/http"
	"sort"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/store"
)

// The public pricing page reads plan_prices only. A priced active plan
// is listed, a free self-signup plan stays listed with no price, and an
// unpriced plan that cannot be self-signed-up for or an inactive plan is
// not listed at all.
func TestPublicPlansReadsLocalPrices(t *testing.T) {
	f := newAPIFixture(t)
	h := NewPublicPlansHandler(f.s, nil)

	if _, err := f.s.UpsertPlanPrice(f.ctx, f.planA.ID, store.PlanPriceInput{
		Provider: "sepay", Currency: "VND", AmountMinor: 1_190_000,
	}); err != nil {
		t.Fatalf("price plan A: %v", err)
	}
	// An active plan in this same product with no price and self_signup
	// off is not purchasable (no checkout price, no self-service signup),
	// so it must stay hidden from the catalogue.
	unpriced := &model.Plan{ProductID: f.prodA.ID, Name: "No price", Slug: "api-unpriced-" + f.suffix,
		LicenseType: "perpetual", LicenseModel: "standard", MaxActivations: 1, Active: true}
	if err := f.s.CreatePlan(f.ctx, unpriced); err != nil {
		t.Fatalf("create unpriced plan: %v", err)
	}

	inactive := &model.Plan{ProductID: f.prodA.ID, Name: "Retired", Slug: "api-retired-" + f.suffix,
		LicenseType: "perpetual", LicenseModel: "standard", MaxActivations: 1, Active: false}
	if err := f.s.CreatePlan(f.ctx, inactive); err != nil {
		t.Fatalf("create inactive plan: %v", err)
	}
	// plans.active carries a DB default of true, which bun applies when
	// the Go value is false, so retirement is an explicit write.
	if _, err := f.s.DB.NewRaw("UPDATE plans SET active = false WHERE id = ?", inactive.ID).Exec(f.ctx); err != nil {
		t.Fatalf("retire plan: %v", err)
	}
	if _, err := f.s.UpsertPlanPrice(f.ctx, inactive.ID, store.PlanPriceInput{
		Provider: "sepay", Currency: "VND", AmountMinor: 99_000,
	}); err != nil {
		t.Fatalf("price inactive plan: %v", err)
	}

	free := &model.Plan{ProductID: f.prodA.ID, Name: "Free", Slug: "api-free-" + f.suffix,
		LicenseType: "perpetual", LicenseModel: "standard", MaxActivations: 1, Active: true, SelfSignup: true}
	if err := f.s.CreatePlan(f.ctx, free); err != nil {
		t.Fatalf("create self-signup plan: %v", err)
	}

	code, out := f.call(h.List, http.MethodGet, "/api/v1/products/"+f.prodA.Slug+"/plans", "", nil,
		gin.Param{Key: "product_slug", Value: f.prodA.Slug})
	if code != http.StatusOK || out["success"] != true {
		t.Fatalf("list = %d %v", code, out)
	}
	data, _ := out["data"].(map[string]any)
	list, _ := data["plans"].([]any)

	byID := make(map[string]map[string]any, len(list))
	for _, raw := range list {
		row, _ := raw.(map[string]any)
		id, _ := row["id"].(string)
		byID[id] = row
	}
	if len(list) != 2 {
		t.Fatalf("plans = %v, want the priced plan and the free self-signup plan", list)
	}
	if byID[f.planA.ID] == nil || byID[free.ID] == nil {
		t.Fatalf("plans = %v, want %s and %s", list, f.planA.ID, free.ID)
	}
	if _, hidden := byID[unpriced.ID]; hidden {
		t.Errorf("an unpriced plan without self-signup was listed")
	}
	if _, hidden := byID[inactive.ID]; hidden {
		t.Errorf("an inactive plan was listed")
	}

	priced := byID[f.planA.ID]
	if priced["price"] != float64(1_190_000) || priced["currency"] != "VND" {
		t.Errorf("flat price = %v %v, want 1190000 VND", priced["price"], priced["currency"])
	}
	prices, _ := priced["prices"].([]any)
	if len(prices) != 1 {
		t.Fatalf("plan A prices = %v, want one", priced["prices"])
	}
	price, _ := prices[0].(map[string]any)
	assertPriceKeys(t, price, "provider", "amount", "currency", "currency_exponent", "billing_interval")
	for _, leaked := range []string{"id", "plan_id", "external_price_id"} {
		if _, ok := price[leaked]; ok {
			t.Errorf("public price leaks %s: %v", leaked, price)
		}
	}
	if price["provider"] != "sepay" || price["amount"] != float64(1_190_000) ||
		price["currency"] != "VND" || price["currency_exponent"] != float64(0) {
		t.Errorf("public price = %v", price)
	}

	freeRow := byID[free.ID]
	if freeRow["price"] != nil || freeRow["currency"] != nil {
		t.Errorf("free plan flat price = %v %v, want null null", freeRow["price"], freeRow["currency"])
	}
	if freeRow["self_signup"] != true {
		t.Errorf("free plan self_signup = %v, want true", freeRow["self_signup"])
	}
	if freePrices, ok := freeRow["prices"].([]any); !ok || len(freePrices) != 0 {
		t.Errorf("free plan prices = %v, want []", freeRow["prices"])
	}

	// A product with no plans answers an empty array, not null: a typed
	// client decodes one shape either way.
	empty := &model.Product{Name: "Empty", Slug: "api-empty-" + f.suffix, Type: "desktop"}
	if err := f.s.CreateProduct(f.ctx, empty); err != nil {
		t.Fatalf("create empty product: %v", err)
	}
	code, out = f.call(h.List, http.MethodGet, "/api/v1/products/"+empty.Slug+"/plans", "", nil,
		gin.Param{Key: "product_slug", Value: empty.Slug})
	if code != http.StatusOK {
		t.Fatalf("empty product = %d %v", code, out)
	}
	data, _ = out["data"].(map[string]any)
	if plans, ok := data["plans"].([]any); !ok || len(plans) != 0 {
		t.Fatalf("empty product plans = %v, want []", data["plans"])
	}
}

// assertPriceKeys pins the public projection exactly: adding a database
// column must not silently widen the public contract.
func assertPriceKeys(t *testing.T, row map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(row))
	for k := range row {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("public price keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("public price keys = %v, want %v", got, want)
		}
	}
}
