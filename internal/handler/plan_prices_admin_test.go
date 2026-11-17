package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dtg-corp/dsoft/internal/store"
)

func TestPlanPriceAdminUpsertAndList(t *testing.T) {
	f := newAPIFixture(t)
	h := &PlanPriceAdminHandler{Store: f.s}
	planPath := "/api/v1/admin/plans/" + f.planA.ID + "/prices"
	planParam := gin.Param{Key: "id", Value: f.planA.ID}

	code, out := f.call(h.Upsert, http.MethodPut, planPath,
		`{"provider":"sepay","currency":"VND","amount_minor":1190000}`, nil, planParam)
	if code != http.StatusOK {
		t.Fatalf("upsert status %d: %v", code, out)
	}
	row, _ := out["data"].(map[string]any)
	priceID, _ := row["id"].(string)
	// The admin UI omits the exponent; VND derives 0.
	if row["currency_exponent"] != float64(0) {
		t.Fatalf("derived exponent = %v, want 0", row["currency_exponent"])
	}

	code, out = f.call(h.List, http.MethodGet, planPath, "", nil, planParam)
	if code != http.StatusOK {
		t.Fatalf("list status %d: %v", code, out)
	}
	data, _ := out["data"].(map[string]any)
	rows, _ := data["prices"].([]any)
	if len(rows) != 1 {
		t.Fatalf("prices = %v", data["prices"])
	}

	// Missing plan is a 404, not a silent create.
	code, out = f.call(h.Upsert, http.MethodPut, "/api/v1/admin/plans/does-not-exist/prices",
		`{"provider":"sepay","currency":"VND","amount_minor":1}`, nil, gin.Param{Key: "id", Value: "does-not-exist"})
	if code != http.StatusNotFound || errCode(out) != "PLAN_NOT_FOUND" {
		t.Fatalf("missing plan upsert: %d %v", code, out)
	}
	// Listing an unknown plan is the same 404, not an empty page that
	// reads like "this plan has no prices".
	code, out = f.call(h.List, http.MethodGet, "/api/v1/admin/plans/does-not-exist/prices", "", nil,
		gin.Param{Key: "id", Value: "does-not-exist"})
	if code != http.StatusNotFound || errCode(out) != "PLAN_NOT_FOUND" {
		t.Fatalf("missing plan list: %d %v", code, out)
	}

	// An unknown price is a 404, not a no-op 204.
	code, out = f.call(h.Delete, http.MethodDelete, planPath+"/does-not-exist", "", nil,
		planParam, gin.Param{Key: "price_id", Value: "does-not-exist"})
	if code != http.StatusNotFound || errCode(out) != "PRICE_NOT_FOUND" {
		t.Fatalf("missing price delete: %d %v", code, out)
	}

	// Deleting the row retires it; the history stays readable.
	code, out = f.call(h.Delete, http.MethodDelete, planPath+"/"+priceID, "", nil,
		planParam, gin.Param{Key: "price_id", Value: priceID})
	if code != http.StatusNoContent {
		t.Fatalf("delete status %d: %v", code, out)
	}
	code, out = f.call(h.List, http.MethodGet, planPath, "", nil, planParam)
	data, _ = out["data"].(map[string]any)
	if rows, _ := data["prices"].([]any); len(rows) != 1 {
		t.Fatalf("retired row missing from history: %v", data["prices"])
	}

	// The plan's own cycle decides the slot: a year row on a plan that
	// sells no cycle can never be resolved by checkout.
	code, out = f.call(h.Upsert, http.MethodPut, planPath,
		`{"provider":"sepay","currency":"VND","amount_minor":1,"billing_interval":"year"}`, nil, planParam)
	if code != http.StatusBadRequest {
		t.Fatalf("mismatched interval status %d: %v", code, out)
	}
}

// The admin UI sends the currency and omits currency_exponent, so the API
// derives it; an explicit value that disagrees with the currency is a
// client bug and is refused rather than silently rewritten.
func TestPlanPriceAdminDerivesCurrencyExponent(t *testing.T) {
	f := newAPIFixture(t)
	h := &PlanPriceAdminHandler{Store: f.s}
	planPath := "/api/v1/admin/plans/" + f.planA.ID + "/prices"
	planParam := gin.Param{Key: "id", Value: f.planA.ID}

	code, out := f.call(h.Upsert, http.MethodPut, planPath,
		`{"provider":"sepay","currency":"USD","amount_minor":500}`, nil, planParam)
	if code != http.StatusOK {
		t.Fatalf("derived USD exponent status %d: %v", code, out)
	}
	row, _ := out["data"].(map[string]any)
	if row["currency_exponent"] != float64(2) || row["amount_minor"] != float64(500) {
		t.Fatalf("USD row = %v, want exponent 2 and amount 500 minor units", row)
	}

	code, out = f.call(h.Upsert, http.MethodPut, planPath,
		`{"provider":"sepay","currency":"USD","amount_minor":500,"currency_exponent":0}`, nil, planParam)
	if code != http.StatusBadRequest || errCode(out) != "INVALID_CURRENCY_EXPONENT" {
		t.Fatalf("mismatched exponent = %d %v, want 400 INVALID_CURRENCY_EXPONENT", code, out)
	}
}

// amount_minor is required: an omitted amount must not become a free
// price by accident, while a deliberate 0 stays legal for free plans.
func TestPlanPriceAdminRequiresAmount(t *testing.T) {
	f := newAPIFixture(t)
	h := &PlanPriceAdminHandler{Store: f.s}
	planPath := "/api/v1/admin/plans/" + f.planA.ID + "/prices"
	planParam := gin.Param{Key: "id", Value: f.planA.ID}

	code, out := f.call(h.Upsert, http.MethodPut, planPath,
		`{"provider":"sepay","currency":"VND"}`, nil, planParam)
	if code != http.StatusBadRequest || errCode(out) != "INVALID_AMOUNT" {
		t.Fatalf("missing amount = %d %v, want 400 INVALID_AMOUNT", code, out)
	}

	code, out = f.call(h.Upsert, http.MethodPut, planPath,
		`{"provider":"sepay","currency":"VND","amount_minor":0}`, nil, planParam)
	if code != http.StatusOK {
		t.Fatalf("zero amount = %d %v, want it accepted for a free plan", code, out)
	}
}

// A concurrent save in the same slot loses the partial unique index. The
// API answers 409 so the editor can retry instead of showing a 500.
func TestPlanPriceAdminUpsertConflict(t *testing.T) {
	f := newAPIFixture(t)
	h := &PlanPriceAdminHandler{Store: f.s}

	// A competing writer has the slot's row uncommitted: the upsert's
	// deactivate cannot see it, so its insert waits on the unique index
	// until the competitor commits.
	hold, err := f.s.DB.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.ExecContext(f.ctx, `INSERT INTO plan_prices
		(id, plan_id, provider, currency, amount_minor, currency_exponent, billing_interval, active)
		VALUES (?, ?, 'sepay', 'VND', 100, 0, '', TRUE)`, store.NewID(), f.planA.ID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		code int
		out  map[string]any
	}
	done := make(chan result, 1)
	go func() {
		code, out := f.call(h.Upsert, http.MethodPut, "/api/v1/admin/plans/"+f.planA.ID+"/prices",
			`{"provider":"sepay","currency":"VND","amount_minor":200}`, nil,
			gin.Param{Key: "id", Value: f.planA.ID})
		done <- result{code, out}
	}()
	waitForPlanPriceWaiter(t, f)
	if err := hold.Commit(); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.code != http.StatusConflict || errCode(got.out) != "PRICE_SLOT_CONFLICT" {
		t.Fatalf("concurrent upsert = %d %v, want 409 PRICE_SLOT_CONFLICT", got.code, got.out)
	}
}

// waitForPlanPriceWaiter waits until the upsert above is blocked on the
// price slot's index, so the test commits the competing row only then.
func waitForPlanPriceWaiter(t *testing.T, f *apiFixture) {
	t.Helper()
	for range 200 {
		var waiting int
		if err := f.s.DB.NewRaw(`SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			  AND query LIKE '%plan_prices%'`).Scan(f.ctx, &waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 1 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the upsert never waited on the price slot")
}
