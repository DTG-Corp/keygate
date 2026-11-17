package handler

import (
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/dtg-corp/dsoft/internal/middleware"
	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/store"
)

// Reports are read over a fixed window in 2027 so no other fixture's
// orders can fall inside it.
var (
	reportFrom = "2027-05-01T00:00:00Z"
	reportTo   = "2027-06-01T00:00:00Z"
)

// reportOrder writes an order and moves it to paid at the given instant.
func reportOrder(t *testing.T, f *paymentsFixture, plan *model.Plan,
	provider string, total, tax int64, taxRateBP int, currency string, exp int16, paidAt time.Time) *model.PaymentOrder {
	t.Helper()
	order, err := f.s.CreatePaymentOrder(f.ctx, store.CreateOrderInput{
		ProductID: plan.ProductID, PlanID: plan.ID, Email: "reports-" + uuid.NewString()[:8] + "@example.com",
		Kind: "new", Quantity: 1, SubtotalMinor: total - tax, TaxMinor: tax, TotalMinor: total,
		Currency: currency, CurrencyExponent: exp, TaxRateBP: taxRateBP,
		Provider: provider, ExpiresAt: time.Now().Add(time.Hour),
		Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := f.s.DB.NewUpdate().Model((*model.PaymentOrder)(nil)).
		Set("status = 'paid'").Set("paid_at = ?", paidAt).Where("id = ?", order.ID).Exec(f.ctx); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	// Unscoped reports would otherwise see the previous test's orders:
	// the package shares one database and each test leaves its product.
	t.Cleanup(func() {
		_, _ = f.s.DB.NewRaw("DELETE FROM payment_orders WHERE id = ?", order.ID).Exec(context.Background())
	})
	return order
}

func TestAdminReportsRevenue(t *testing.T) {
	f, _, _ := invoiceAdminFixture(t)
	h := &ReportsAdminHandler{Store: f.s}
	reportOrder(t, f, f.planA, "sepay", 1_190_000, 88_148, 800, "VND", 0,
		time.Date(2027, 5, 10, 3, 0, 0, 0, time.UTC))
	reportOrder(t, f, f.planB, "payos", 500_000, 0, 0, "VND", 0,
		time.Date(2027, 5, 11, 3, 0, 0, 0, time.UTC))

	w := f.record(h.Revenue, http.MethodGet,
		"/api/v1/admin/payments/reports/revenue?from="+reportFrom+"&to="+reportTo+"&group_by=provider", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("revenue = %d %s", w.Code, w.Body.String())
	}
	data := dataOf(t, w)
	if data["timezone"] != "Asia/Ho_Chi_Minh" || data["group_by"] != "provider" {
		t.Errorf("metadata = %v", data)
	}
	rows, _ := data["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows = %v", data["rows"])
	}
	totals, _ := data["totals"].([]any)
	if len(totals) != 1 || totals[0].(map[string]any)["currency"] != "VND" ||
		totals[0].(map[string]any)["total_minor"] != float64(1_690_000) {
		t.Fatalf("totals = %v", data["totals"])
	}

	// CSV export is an attachment with one header row and one row per bucket.
	w = f.record(h.Revenue, http.MethodGet,
		"/api/v1/admin/payments/reports/revenue?from="+reportFrom+"&to="+reportTo+"&group_by=provider&format=csv", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("csv = %d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/csv") {
		t.Errorf("Content-Type = %q", got)
	}
	if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") || !strings.HasSuffix(got, `.csv"`) {
		t.Errorf("Content-Disposition = %q", got)
	}
	records, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("csv rows = %v", records)
	}
	if records[0][0] != "period" || records[0][8] != "orders" || records[0][12] != "total_minor" {
		t.Errorf("csv header = %v", records[0])
	}
	seen := map[string]string{}
	for _, r := range records[1:] {
		seen[r[1]+"/"+r[8]] = r[12]
	}
	if seen["sepay/1"] != "1190000" || seen["payos/1"] != "500000" {
		t.Errorf("csv rows = %v", records[1:])
	}

	// Query validation.
	for _, tc := range []struct{ name, query string }{
		{"bad group", "?from=" + reportFrom + "&to=" + reportTo + "&group_by=email"},
		{"bad format", "?from=" + reportFrom + "&to=" + reportTo + "&format=xlsx"},
		{"backwards", "?from=" + reportTo + "&to=" + reportFrom},
		{"bad from", "?from=yesterday&to=" + reportTo},
		{"bad period", "?from=" + reportFrom + "&to=" + reportTo + "&period=week"},
	} {
		code, out := f.call(h.Revenue, http.MethodGet, "/api/v1/admin/payments/reports/revenue"+tc.query, "", nil)
		if code != http.StatusBadRequest || errCode(out) != "BAD_REQUEST" {
			t.Errorf("%s = %d %v, want 400 BAD_REQUEST", tc.name, code, out)
		}
	}
}

func TestAdminReportsReconciliation(t *testing.T) {
	f, _, _ := invoiceAdminFixture(t)
	h := &ReportsAdminHandler{Store: f.s}
	paidAt := time.Date(2027, 5, 10, 3, 0, 0, 0, time.UTC)
	reportOrder(t, f, f.planA, "sepay", 500_000, 0, 0, "VND", 0, paidAt)
	ref := "handler-recon-" + uuid.NewString()[:8]
	if _, _, err := f.s.InsertPaymentTransaction(f.ctx, nil, store.PaymentTransactionInput{
		Provider: "sepay", ProviderRef: ref, AmountMinor: 400_000, Currency: "VND",
		Status: "matched", Direction: "in", OccurredAt: paidAt.Add(10 * time.Minute), Raw: []byte(`{}`),
	}); err != nil {
		t.Fatalf("insert transaction: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.s.DB.NewRaw("DELETE FROM payment_transactions WHERE provider = ? AND provider_ref = ?", "sepay", ref).
			Exec(f.ctx)
	})

	w := f.record(h.Reconciliation, http.MethodGet,
		"/api/v1/admin/payments/reports/reconciliation?from="+reportFrom+"&to="+reportTo, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("reconciliation = %d %s", w.Code, w.Body.String())
	}
	data := dataOf(t, w)
	rows, _ := data["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v", data["rows"])
	}
	row, _ := rows[0].(map[string]any)
	if row["day"] != "2027-05-10" || row["orders_minor"] != float64(500_000) ||
		row["transactions_minor"] != float64(400_000) || row["drift_minor"] != float64(100_000) {
		t.Errorf("row = %v", row)
	}
	if data["drift_minor"] != float64(100_000) {
		t.Errorf("drift_minor = %v", data["drift_minor"])
	}

	w = f.record(h.Reconciliation, http.MethodGet,
		"/api/v1/admin/payments/reports/reconciliation?from="+reportFrom+"&to="+reportTo+"&format=csv", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("reconciliation csv = %d %s", w.Code, w.Body.String())
	}
	records, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil || len(records) != 2 {
		t.Fatalf("csv = %v (%v)", records, err)
	}
	if records[0][0] != "day" || records[0][7] != "drift_minor" || records[1][7] != "100000" {
		t.Errorf("csv = %v", records)
	}

	// A transfer has no product, so that filter is refused rather than
	// applied to one side of the comparison.
	code, out := f.call(h.Reconciliation, http.MethodGet,
		"/api/v1/admin/payments/reports/reconciliation?from="+reportFrom+"&to="+reportTo+"&product_id="+f.prodA.ID, "", nil)
	if code != http.StatusBadRequest || errCode(out) != "BAD_REQUEST" {
		t.Fatalf("product filter = %d %v, want 400", code, out)
	}
}

func TestAdminReportsVAT(t *testing.T) {
	f, _, _ := invoiceAdminFixture(t)
	h := &ReportsAdminHandler{Store: f.s}
	at := time.Date(2027, 5, 10, 3, 0, 0, 0, time.UTC)
	reportOrder(t, f, f.planA, "sepay", 1_190_000, 88_148, 800, "VND", 0, at)
	reportOrder(t, f, f.planA, "sepay", 1_100_000, 100_000, 1000, "VND", 0, at.Add(time.Hour))
	reportOrder(t, f, f.planB, "payos", 112_500, 12_500, 1250, "USD", 2, at.Add(2*time.Hour))

	w := f.record(h.VAT, http.MethodGet,
		"/api/v1/admin/payments/reports/vat?from="+reportFrom+"&to="+reportTo+"&period=month", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("vat = %d %s", w.Code, w.Body.String())
	}
	data := dataOf(t, w)
	if data["period"] != "month" {
		t.Errorf("period = %v", data["period"])
	}
	rows, _ := data["rows"].([]any)
	if len(rows) != 3 {
		t.Fatalf("rows = %v", data["rows"])
	}
	byRate := map[float64]map[string]any{}
	for _, r := range rows {
		m := r.(map[string]any)
		byRate[m["tax_rate_bp"].(float64)] = m
	}
	if r := byRate[800]; r["period"] != "2027-05" || r["taxable_base_minor"] != float64(1_101_852) || r["tax_minor"] != float64(88_148) {
		t.Errorf("8%% = %v", r)
	}
	if r := byRate[1000]; r["taxable_base_minor"] != float64(1_000_000) || r["tax_minor"] != float64(100_000) {
		t.Errorf("10%% = %v", r)
	}
	if r := byRate[1250]; r["currency"] != "USD" || r["currency_exponent"] != float64(2) || r["tax_minor"] != float64(12_500) {
		t.Errorf("12.5%% = %v", r)
	}
	totals, _ := data["totals"].([]any)
	if len(totals) != 2 {
		t.Fatalf("totals = %v", data["totals"])
	}

	w = f.record(h.VAT, http.MethodGet,
		"/api/v1/admin/payments/reports/vat?from="+reportFrom+"&to="+reportTo+"&period=month&format=csv", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("vat csv = %d %s", w.Code, w.Body.String())
	}
	records, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil || len(records) != 4 {
		t.Fatalf("csv = %v (%v)", records, err)
	}
	if records[0][0] != "period" || records[0][6] != "tax_minor" {
		t.Errorf("csv header = %v", records[0])
	}
}

// The report routes are mounted on the admin group; a payments:read key
// must not reach them.
func TestAdminReportsRequireAdmin(t *testing.T) {
	f, _, _ := invoiceAdminFixture(t)
	h := &ReportsAdminHandler{Store: f.s}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterReportsAdminRoutes(r.Group("/api/v1/admin",
		middleware.APIKeyAuth(f.s), middleware.RequireScope(model.ScopeAdmin)), h)

	send := func(rawKey, path string) (int, string) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if rawKey != "" {
			req.Header.Set("Authorization", "Bearer "+rawKey)
		}
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	readRaw := f.scopedAPIKey(t, model.ScopePaymentsRead)
	for _, path := range []string{
		"/api/v1/admin/payments/reports/revenue",
		"/api/v1/admin/payments/reports/reconciliation",
		"/api/v1/admin/payments/reports/vat?format=csv",
	} {
		if code, body := send(readRaw, path); code != http.StatusForbidden {
			t.Errorf("payments:read %s = %d %s, want 403", path, code, body)
		}
		if code, _ := send("", path); code != http.StatusUnauthorized {
			t.Errorf("no key %s = %d, want 401", path, code)
		}
	}
	if code, body := send(f.scopedAPIKey(t, model.ScopeAdmin),
		"/api/v1/admin/payments/reports/revenue?from="+reportFrom+"&to="+reportTo); code != http.StatusOK {
		t.Fatalf("admin report = %d %s", code, body)
	}
}
