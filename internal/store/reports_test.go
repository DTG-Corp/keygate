package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

// The report tests write in a week no other fixture uses, so a parallel
// package's collections cannot land inside the asserted window.
var (
	reportDayFrom   = time.Date(2027, 3, 10, 17, 0, 0, 0, time.UTC) // 11 Mar 00:00 ICT
	reportDayTo     = time.Date(2027, 3, 11, 17, 0, 0, 0, time.UTC) // 12 Mar 00:00 ICT
	reportMonthFrom = time.Date(2027, 2, 28, 17, 0, 0, 0, time.UTC) // 1 Mar 00:00 ICT
	reportMonthTo   = time.Date(2027, 3, 31, 17, 0, 0, 0, time.UTC) // 1 Apr 00:00 ICT
)

type reportFixture struct {
	t     *testing.T
	s     *store.Store
	ctx   context.Context
	prodA *model.Product
	prodB *model.Product
	planA *model.Plan
	planB *model.Plan
}

func newReportFixture(t *testing.T) *reportFixture {
	t.Helper()
	s := testdb.Store(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:13]
	mk := func(name string) (*model.Product, *model.Plan) {
		p := &model.Product{Name: "Report " + name, Slug: "report-" + name + "-" + suffix, Type: "desktop"}
		if err := s.CreateProduct(ctx, p); err != nil {
			t.Fatalf("create product: %v", err)
		}
		pl := &model.Plan{ProductID: p.ID, Name: name + " Pro", Slug: "report-pro-" + name + "-" + suffix,
			LicenseType: "subscription", LicenseModel: "standard", Active: true}
		if err := s.CreatePlan(ctx, pl); err != nil {
			t.Fatalf("create plan: %v", err)
		}
		return p, pl
	}
	f := &reportFixture{t: t, s: s, ctx: ctx}
	f.prodA, f.planA = mk("a")
	f.prodB, f.planB = mk("b")
	t.Cleanup(func() {
		ctx := context.Background()
		for _, p := range []*model.Product{f.prodA, f.prodB} {
			_, _ = s.DB.NewRaw("DELETE FROM invoices WHERE order_id IN (SELECT id FROM payment_orders WHERE product_id = ?)", p.ID).Exec(ctx)
			_, _ = s.DB.NewRaw("DELETE FROM payment_orders WHERE product_id = ?", p.ID).Exec(ctx)
			_, _ = s.DB.NewRaw("DELETE FROM plans WHERE product_id = ?", p.ID).Exec(ctx)
			_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", p.ID).Exec(ctx)
		}
	})
	return f
}

type reportOrder struct {
	product   *model.Product
	plan      *model.Plan
	provider  string
	currency  string
	exponent  int16
	taxRateBP int
	subtotal  int64
	discount  int64
	tax       int64
	total     int64
	paidAt    time.Time
	status    string
}

// order writes an order and then moves it into the collected state the
// reports read. The update is deliberate: CreatePaymentOrder always
// starts pending, and a report over a pending order must count nothing.
func (f *reportFixture) order(o reportOrder) *model.PaymentOrder {
	f.t.Helper()
	if o.status == "" {
		o.status = "paid"
	}
	if o.currency == "" {
		o.currency = "VND"
	}
	order, err := f.s.CreatePaymentOrder(f.ctx, store.CreateOrderInput{
		ProductID: o.product.ID, PlanID: o.plan.ID, Email: "report@example.com", Kind: "new", Quantity: 1,
		SubtotalMinor: o.subtotal, DiscountMinor: o.discount, TaxMinor: o.tax, TotalMinor: o.total,
		Currency: o.currency, CurrencyExponent: o.exponent, TaxRateBP: o.taxRateBP,
		Provider: o.provider, ExpiresAt: time.Now().Add(time.Hour),
		Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		f.t.Fatalf("create order: %v", err)
	}
	if _, err := f.s.DB.NewUpdate().Model((*model.PaymentOrder)(nil)).
		Set("status = ?", o.status).Set("paid_at = ?", o.paidAt).
		Where("id = ?", order.ID).Exec(f.ctx); err != nil {
		f.t.Fatalf("mark order %s: %v", order.ID, err)
	}
	return order
}

func (f *reportFixture) transaction(provider, ref, status, direction string, amount int64, currency string, at time.Time) {
	f.t.Helper()
	_, _, err := f.s.InsertPaymentTransaction(f.ctx, nil, store.PaymentTransactionInput{
		Provider: provider, ProviderRef: ref, AmountMinor: amount, Currency: currency,
		Status: status, Direction: direction, OccurredAt: at, Raw: []byte(`{}`),
	})
	if err != nil {
		f.t.Fatalf("insert transaction %s: %v", ref, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.s.DB.NewRaw("DELETE FROM payment_transactions WHERE provider = ? AND provider_ref = ?", provider, ref).
			Exec(context.Background())
	})
}

func TestRevenueReportBuckets(t *testing.T) {
	f := newReportFixture(t)
	// Two orders in the window on one VN calendar day (the first is still
	// 10 March in UTC), one on the same VN day from the other provider,
	// and one just past the window's end.
	f.order(reportOrder{product: f.prodA, plan: f.planA, provider: "sepay",
		subtotal: 1_000_000, discount: 100_000, tax: 72_000, total: 900_000, taxRateBP: 800,
		paidAt: time.Date(2027, 3, 10, 18, 0, 0, 0, time.UTC)})
	f.order(reportOrder{product: f.prodA, plan: f.planA, provider: "sepay",
		subtotal: 500_000, tax: 40_000, total: 500_000, taxRateBP: 800,
		paidAt: time.Date(2027, 3, 11, 2, 0, 0, 0, time.UTC)})
	f.order(reportOrder{product: f.prodB, plan: f.planB, provider: "payos",
		subtotal: 300_000, total: 300_000,
		paidAt: time.Date(2027, 3, 11, 16, 0, 0, 0, time.UTC)})
	f.order(reportOrder{product: f.prodB, plan: f.planB, provider: "payos",
		subtotal: 999_000, total: 999_000,
		paidAt: time.Date(2027, 3, 11, 17, 0, 0, 0, time.UTC)}) // 12 Mar 00:00 ICT
	// A pending order inside the window is not revenue.
	f.order(reportOrder{product: f.prodA, plan: f.planA, provider: "sepay",
		subtotal: 777_000, total: 777_000, status: "pending",
		paidAt: time.Date(2027, 3, 11, 3, 0, 0, 0, time.UTC)})

	window := store.ReportFilter{From: reportDayFrom, To: reportDayTo}

	rows, err := f.s.RevenueReport(f.ctx, store.ReportFilter{
		From: window.From, To: window.To, GroupBy: store.ReportGroupProvider,
	})
	if err != nil {
		t.Fatalf("revenue by provider: %v", err)
	}
	byProvider := map[string]store.RevenueRow{}
	for _, r := range rows {
		byProvider[r.Provider] = r
	}
	if len(rows) != 2 {
		t.Fatalf("provider rows = %+v, want sepay and payos", rows)
	}
	sepay := byProvider["sepay"]
	if sepay.Orders != 2 || sepay.SubtotalMinor != 1_500_000 || sepay.DiscountMinor != 100_000 ||
		sepay.TaxMinor != 112_000 || sepay.TotalMinor != 1_400_000 || sepay.Currency != "VND" {
		t.Errorf("sepay row = %+v", sepay)
	}
	payos := byProvider["payos"]
	if payos.Orders != 1 || payos.SubtotalMinor != 300_000 || payos.TotalMinor != 300_000 {
		t.Errorf("payos row = %+v", payos)
	}

	// Period grouping: the 18:00Z order belongs to 11 March in HCMC, and
	// all three windowed orders share that bucket.
	rows, err = f.s.RevenueReport(f.ctx, store.ReportFilter{
		From: window.From, To: window.To, GroupBy: store.ReportGroupPeriod, Period: store.ReportPeriodDay,
	})
	if err != nil {
		t.Fatalf("revenue by period: %v", err)
	}
	if len(rows) != 1 || rows[0].Period != "2027-03-11" || rows[0].Orders != 3 ||
		rows[0].TotalMinor != 1_700_000 {
		t.Fatalf("day rows = %+v, want one 2027-03-11 bucket of three orders", rows)
	}

	// Plan and product groupings.
	rows, err = f.s.RevenueReport(f.ctx, store.ReportFilter{From: window.From, To: window.To, GroupBy: store.ReportGroupPlan})
	if err != nil {
		t.Fatalf("revenue by plan: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("plan rows = %+v", rows)
	}
	for _, r := range rows {
		switch r.PlanID {
		case f.planA.ID:
			if r.Orders != 2 || r.PlanName != "a Pro" {
				t.Errorf("plan A row = %+v", r)
			}
		case f.planB.ID:
			if r.Orders != 1 || r.PlanName != "b Pro" {
				t.Errorf("plan B row = %+v", r)
			}
		default:
			t.Errorf("unexpected plan row %+v", r)
		}
	}
	rows, err = f.s.RevenueReport(f.ctx, store.ReportFilter{From: window.From, To: window.To, GroupBy: store.ReportGroupProduct})
	if err != nil {
		t.Fatalf("revenue by product: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("product rows = %+v", rows)
	}

	// Narrowing to one product keeps only its revenue.
	rows, err = f.s.RevenueReport(f.ctx, store.ReportFilter{
		From: window.From, To: window.To, GroupBy: store.ReportGroupProvider, ProductID: f.prodA.ID,
	})
	if err != nil {
		t.Fatalf("revenue filtered: %v", err)
	}
	if len(rows) != 1 || rows[0].Provider != "sepay" || rows[0].Orders != 2 {
		t.Fatalf("filtered rows = %+v", rows)
	}

	// An invalid grouping or window is refused, not guessed at.
	if _, err := f.s.RevenueReport(f.ctx, store.ReportFilter{From: window.From, To: window.To, GroupBy: "email"}); err == nil {
		t.Error("unknown grouping was accepted")
	}
	if _, err := f.s.RevenueReport(f.ctx, store.ReportFilter{From: window.To, To: window.From}); err == nil {
		t.Error("backwards window was accepted")
	}
	if _, err := f.s.RevenueReport(f.ctx, store.ReportFilter{To: window.To}); err == nil {
		t.Error("open window was accepted")
	}
}

func TestReconciliationReportDrift(t *testing.T) {
	f := newReportFixture(t)
	day := func(d int, hour int) time.Time {
		return time.Date(2027, 3, d, hour, 0, 0, 0, time.UTC)
	}
	// Day 17: the order says 500k, the matched transfer says 400k.
	f.order(reportOrder{product: f.prodA, plan: f.planA, provider: "sepay",
		subtotal: 500_000, total: 500_000, paidAt: day(17, 3)})
	f.transaction("sepay", "recon-in-17-"+f.suffix(), "matched", "in", 400_000, "VND", day(17, 3).Add(5*time.Minute))
	// Excluded rows: an outgoing refund and an unmatched transfer.
	f.transaction("sepay", "recon-out-17-"+f.suffix(), "matched", "out", 50_000, "VND", day(17, 4))
	f.transaction("sepay", "recon-unmatched-17-"+f.suffix(), "unmatched", "in", 60_000, "VND", day(17, 4))
	// Day 18: money arrived with no order.
	f.transaction("sepay", "recon-in-18-"+f.suffix(), "matched", "in", 200_000, "VND", day(18, 3))
	// Day 19: an order with no transfer.
	f.order(reportOrder{product: f.prodA, plan: f.planA, provider: "sepay",
		subtotal: 700_000, total: 700_000, paidAt: day(19, 3)})

	rows, err := f.s.ReconciliationReport(f.ctx, store.ReportFilter{
		From: day(17, 0), To: day(20, 0),
	})
	if err != nil {
		t.Fatalf("reconciliation: %v", err)
	}
	byDay := map[string]store.ReconRow{}
	for _, r := range rows {
		if r.Provider != "sepay" || r.Currency != "VND" {
			t.Errorf("unexpected row %+v", r)
		}
		byDay[r.Day] = r
	}
	if got := byDay["2027-03-17"]; got.OrdersCount != 1 || got.TransactionsCount != 1 ||
		got.OrdersMinor != 500_000 || got.TransactionsMinor != 400_000 || got.DriftMinor != 100_000 {
		t.Errorf("17th = %+v, want drift 100000 and only the incoming matched transfer counted", got)
	}
	if got := byDay["2027-03-18"]; got.OrdersCount != 0 || got.TransactionsCount != 1 ||
		got.TransactionsMinor != 200_000 || got.DriftMinor != -200_000 {
		t.Errorf("18th = %+v, want a transaction-only day", got)
	}
	if got := byDay["2027-03-19"]; got.OrdersCount != 1 || got.TransactionsCount != 0 ||
		got.OrdersMinor != 700_000 || got.DriftMinor != 700_000 {
		t.Errorf("19th = %+v, want an order-only day", got)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want exactly three days", rows)
	}
}

func TestVATReportSplit(t *testing.T) {
	f := newReportFixture(t)
	at := time.Date(2027, 3, 11, 3, 0, 0, 0, time.UTC)
	// VAT contained in the price (the VN B2C case of the spec).
	f.order(reportOrder{product: f.prodA, plan: f.planA, provider: "sepay", taxRateBP: 800,
		subtotal: 1_190_000, tax: 88_148, total: 1_190_000, paidAt: at})
	// VAT added on top.
	f.order(reportOrder{product: f.prodA, plan: f.planA, provider: "sepay", taxRateBP: 1000,
		subtotal: 1_000_000, tax: 100_000, total: 1_100_000, paidAt: at.Add(time.Hour)})
	// A USD order: exponent 2 must survive the report.
	f.order(reportOrder{product: f.prodB, plan: f.planB, provider: "payos", taxRateBP: 1250,
		subtotal: 100_000, tax: 12_500, total: 112_500,
		currency: "USD", exponent: 2, paidAt: at.Add(2 * time.Hour)})

	rows, err := f.s.VATReport(f.ctx, store.ReportFilter{
		From: reportMonthFrom, To: reportMonthTo, Period: store.ReportPeriodMonth,
	})
	if err != nil {
		t.Fatalf("vat: %v", err)
	}
	type key struct {
		currency string
		rate     int
	}
	got := map[key]store.VATRow{}
	for _, r := range rows {
		if r.Period != "2027-03" {
			t.Errorf("period = %q, want 2027-03", r.Period)
		}
		got[key{r.Currency, r.TaxRateBP}] = r
	}
	if len(got) != 3 {
		t.Fatalf("vat rows = %+v, want three (VND 8%%, VND 10%%, USD 12.5%%)", rows)
	}
	if r := got[key{"VND", 800}]; r.TaxableBaseMinor != 1_101_852 || r.TaxMinor != 88_148 || r.TotalMinor != 1_190_000 {
		t.Errorf("VND 8%% = %+v", r)
	}
	if r := got[key{"VND", 1000}]; r.TaxableBaseMinor != 1_000_000 || r.TaxMinor != 100_000 || r.TotalMinor != 1_100_000 {
		t.Errorf("VND 10%% = %+v", r)
	}
	if r := got[key{"USD", 1250}]; r.TaxableBaseMinor != 100_000 || r.TaxMinor != 12_500 ||
		r.TotalMinor != 112_500 || r.CurrencyExponent != 2 {
		t.Errorf("USD 12.5%% = %+v", r)
	}

	// A daily window on the same data buckets the same amounts per day.
	rows, err = f.s.VATReport(f.ctx, store.ReportFilter{
		From: reportMonthFrom, To: reportMonthTo, Period: store.ReportPeriodDay,
	})
	if err != nil {
		t.Fatalf("vat daily: %v", err)
	}
	for _, r := range rows {
		if r.Period != "2027-03-11" {
			t.Errorf("daily period = %q", r.Period)
		}
	}
}

// suffix makes the transaction references unique per run so the shared
// test database does not swallow an insert as a duplicate.
func (f *reportFixture) suffix() string { return uuid.NewString()[:8] }
