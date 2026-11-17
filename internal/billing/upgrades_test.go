package billing_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/billing"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/checkout"
	"github.com/tabloy/keygate/internal/store"
)

// ProrateMinor is the unused fraction of the period, floored, and it has
// exact answers at both boundaries.
func TestProrateMinor(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(30 * 24 * time.Hour)
	cases := []struct {
		name string
		now  time.Time
		want int64
	}{
		{"before the period starts", start.Add(-time.Hour), 1_200_000},
		{"exactly at the start", start, 1_200_000},
		{"ten days in", start.Add(10 * 24 * time.Hour), 800_000},
		{"half way", start.Add(15 * 24 * time.Hour), 600_000},
		{"exactly at the end", end, 0},
		{"after the end", end.Add(time.Hour), 0},
	}
	for _, tc := range cases {
		if got := billing.ProrateMinor(1_200_000, start, end, tc.now); got != tc.want {
			t.Errorf("%s: ProrateMinor = %d, want %d", tc.name, got, tc.want)
		}
	}
	// Floor, not round: the last third of 100 is 33.
	if got := billing.ProrateMinor(100, start, start.Add(3*time.Hour), start.Add(2*time.Hour)); got != 33 {
		t.Errorf("floor: got %d, want 33", got)
	}
	// A zero or inverted period has no unused time.
	if got := billing.ProrateMinor(100, end, start, start); got != 0 {
		t.Errorf("inverted: got %d, want 0", got)
	}
	if got := billing.ProrateMinor(0, start, end, start.Add(time.Hour)); got != 0 {
		t.Errorf("zero amount: got %d, want 0", got)
	}
}

// targetPlan adds a subscription plan to the seeded product and prices it
// for sepay/VND, with the requested proration mode.
func (r *rig) targetPlan(prod *model.Product, name string, price int64, mode string) *model.Plan {
	r.t.Helper()
	ctx := context.Background()
	plan := &model.Plan{
		ProductID: prod.ID, Name: name, Slug: "target-" + name + "-" + prod.Slug,
		LicenseType: "subscription", BillingInterval: "month", LicenseModel: "standard",
		MaxActivations: 5, Active: true, ProrationMode: mode,
	}
	if err := r.s.CreatePlan(ctx, plan); err != nil {
		r.t.Fatalf("create target plan: %v", err)
	}
	if price > 0 {
		if _, err := r.s.UpsertPlanPrice(ctx, plan.ID, store.PlanPriceInput{
			Provider: "sepay", Currency: "VND", AmountMinor: price,
		}); err != nil {
			r.t.Fatalf("price target plan: %v", err)
		}
	}
	r.t.Cleanup(func() {
		ctx := context.Background()
		_, _ = r.s.DB.NewRaw("DELETE FROM plan_prices WHERE plan_id = ?", plan.ID).Exec(ctx)
		_, _ = r.s.DB.NewRaw("DELETE FROM plans WHERE id = ?", plan.ID).Exec(ctx)
	})
	return plan
}

// The quote credits the unused part of the current period at the current
// plan's price and charges the target price for the same remaining time.
func TestQuoteUpgrade(t *testing.T) {
	r := newRig(t)
	end := r.now.Add(20 * 24 * time.Hour)
	seed := r.seed(seedOpts{Price: 1_200_000, Start: end.Add(-30 * 24 * time.Hour), End: end})
	target := r.targetPlan(seed.Product, "Yearly", 2_000_000, "immediate")

	q, err := r.svc.QuoteUpgrade(context.Background(), seed.Licence.ID, target.ID, r.now)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if q.LicenseID != seed.Licence.ID || q.ProductID != seed.Product.ID ||
		q.FromPlanID != seed.Plan.ID || q.ToPlanID != target.ID {
		t.Fatalf("quote ids = %+v", q)
	}
	if q.Provider != "sepay" || q.Currency != "VND" || q.CurrencyExponent != 0 {
		t.Fatalf("quote money identity = %q %q %d", q.Provider, q.Currency, q.CurrencyExponent)
	}
	// 10 of 30 days unused at 1,200,000 = 400,000 credit.
	if q.FromAmountMinor != 1_200_000 || q.CreditMinor != 800_000 {
		t.Fatalf("amounts = from %d credit %d, want 1200000/800000", q.FromAmountMinor, q.CreditMinor)
	}
	if q.AmountMinor != 1_200_000 {
		t.Fatalf("amount = %d, want 1200000", q.AmountMinor)
	}
	if !q.Immediate {
		t.Fatal("immediate plan quoted as next_cycle")
	}
	if !q.PeriodStart.Equal(r.now) {
		t.Fatalf("period start = %v, want now %v", q.PeriodStart, r.now)
	}
	if !q.PeriodEnd.Equal(end) {
		t.Fatalf("period end = %v, want %v", q.PeriodEnd, end)
	}
}

// A next_cycle plan is quoted with the same numbers but is not charged
// today.
func TestQuoteUpgradeNextCycle(t *testing.T) {
	r := newRig(t)
	end := r.now.Add(20 * 24 * time.Hour)
	seed := r.seed(seedOpts{Price: 1_200_000, Start: end.Add(-30 * 24 * time.Hour), End: end})
	target := r.targetPlan(seed.Product, "Yearly", 2_000_000, "next_cycle")

	q, err := r.svc.QuoteUpgrade(context.Background(), seed.Licence.ID, target.ID, r.now)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if q.Immediate {
		t.Fatal("next_cycle plan quoted as immediate")
	}
	if q.CreditMinor != 800_000 || q.AmountMinor != 1_200_000 {
		t.Fatalf("amounts = credit %d amount %d", q.CreditMinor, q.AmountMinor)
	}
}

func TestQuoteUpgradeRefusals(t *testing.T) {
	r := newRig(t)
	seed := r.seed(seedOpts{Price: 1_200_000})
	ctx := context.Background()

	if _, err := r.svc.QuoteUpgrade(ctx, seed.Licence.ID, seed.Plan.ID, r.now); !errors.Is(err, billing.ErrLicenseNotEligible) {
		t.Fatalf("same plan: err = %v", err)
	}
	otherProd := &model.Product{Name: "Other", Slug: "other-" + seed.Product.Slug, Type: "desktop"}
	if err := r.s.CreateProduct(ctx, otherProd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = r.s.DB.NewRaw("DELETE FROM products WHERE id = ?", otherProd.ID).Exec(context.Background())
	})
	foreign := &model.Plan{ProductID: otherProd.ID, Name: "Foreign", Slug: "foreign-" + otherProd.Slug,
		LicenseType: "subscription", BillingInterval: "month", LicenseModel: "standard", Active: true}
	if err := r.s.CreatePlan(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = r.s.DB.NewRaw("DELETE FROM plans WHERE id = ?", foreign.ID).Exec(context.Background())
	})
	if _, err := r.svc.QuoteUpgrade(ctx, seed.Licence.ID, foreign.ID, r.now); !errors.Is(err, billing.ErrLicenseNotEligible) {
		t.Fatalf("foreign product: err = %v", err)
	}
	if _, err := r.svc.QuoteUpgrade(ctx, seed.Licence.ID, "no-such-plan", r.now); !errors.Is(err, billing.ErrLicenseNotEligible) {
		t.Fatalf("missing plan: err = %v", err)
	}
}

// An immediate upgrade opens one order with the credit frozen on it, one
// pending upgrade row linked to it, and does not touch the licence until
// the order is paid. A retried call returns the same order.
func TestCreateUpgradeOrderImmediate(t *testing.T) {
	r := newRig(t)
	end := r.now.Add(20 * 24 * time.Hour)
	seed := r.seed(seedOpts{Price: 1_200_000, Start: end.Add(-30 * 24 * time.Hour), End: end})
	target := r.targetPlan(seed.Product, "Yearly", 2_000_000, "immediate")

	res, err := r.svc.CreateUpgradeOrder(context.Background(), billing.CreateUpgradeOrderInput{
		LicenseID: seed.Licence.ID, ToPlanID: target.ID,
	})
	if err != nil {
		t.Fatalf("create upgrade order: %v", err)
	}
	if res == nil || res.Order == nil {
		t.Fatal("no order")
	}
	order := res.Order
	if order.Kind != checkout.KindUpgrade || order.LicenseID != seed.Licence.ID || order.PlanID != target.ID {
		t.Fatalf("order = kind %s licence %s plan %s", order.Kind, order.LicenseID, order.PlanID)
	}
	if order.SubtotalMinor != 2_000_000 || order.CreditMinor != 800_000 ||
		order.DiscountMinor != 800_000 || order.TotalMinor != 1_200_000 {
		t.Fatalf("order money = subtotal %d credit %d discount %d total %d",
			order.SubtotalMinor, order.CreditMinor, order.DiscountMinor, order.TotalMinor)
	}
	if order.PeriodEnd == nil || !order.PeriodEnd.Equal(end) {
		t.Fatalf("order period end = %v, want %v", order.PeriodEnd, end)
	}
	// The licence is untouched until payment.
	if got := r.licence(seed.Licence.ID); got.PlanID != seed.Plan.ID {
		t.Fatalf("licence moved before payment: plan %s", got.PlanID)
	}
	up, err := r.s.FindPendingUpgradeForLicense(context.Background(), seed.Licence.ID)
	if err != nil {
		t.Fatal(err)
	}
	if up == nil || up.PaymentOrderID != order.ID || up.ToPlanID != target.ID || up.State != model.UpgradePending {
		t.Fatalf("upgrade row = %+v", up)
	}
	var meta struct {
		Billing struct {
			Source      string `json:"source"`
			UpgradeID   string `json:"upgrade_id"`
			CreditMinor int64  `json:"credit_minor"`
		} `json:"billing"`
	}
	if err := json.Unmarshal(order.Metadata, &meta); err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if meta.Billing.Source != "upgrade" || meta.Billing.UpgradeID != up.ID || meta.Billing.CreditMinor != 800_000 {
		t.Fatalf("metadata = %+v", meta.Billing)
	}
	// Retrying the same change returns the same order instead of a second
	// charge.
	again, err := r.svc.CreateUpgradeOrder(context.Background(), billing.CreateUpgradeOrderInput{
		LicenseID: seed.Licence.ID, ToPlanID: target.ID,
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if again == nil || again.Order == nil || again.Order.ID != order.ID {
		t.Fatalf("retry order = %+v, want %s", again, order.ID)
	}
	if orders := r.orders(seed.Licence.ID); len(orders) != 1 {
		t.Fatalf("orders = %d, want 1", len(orders))
	}
}

// A next_cycle change schedules one pending row, charges nothing and
// leaves the licence on the plan the customer paid for.
func TestCreateUpgradeOrderNextCycle(t *testing.T) {
	r := newRig(t)
	seed := r.seed(seedOpts{Price: 1_200_000})
	target := r.targetPlan(seed.Product, "Yearly", 2_000_000, "next_cycle")

	res, err := r.svc.CreateUpgradeOrder(context.Background(), billing.CreateUpgradeOrderInput{
		LicenseID: seed.Licence.ID, ToPlanID: target.ID,
	})
	if err != nil {
		t.Fatalf("create next-cycle upgrade: %v", err)
	}
	if res != nil {
		t.Fatalf("next-cycle change returned an order: %+v", res)
	}
	if orders := r.orders(seed.Licence.ID); len(orders) != 0 {
		t.Fatalf("orders = %d, want none", len(orders))
	}
	up, err := r.s.FindPendingUpgradeForLicense(context.Background(), seed.Licence.ID)
	if err != nil {
		t.Fatal(err)
	}
	if up == nil || up.PaymentOrderID != "" || up.ToPlanID != target.ID ||
		up.StripeCheckoutSessionID != store.NextCycleUpgradeSessionPrefix+up.ID {
		t.Fatalf("next-cycle row = %+v", up)
	}
	if got := r.licence(seed.Licence.ID); got.PlanID != seed.Plan.ID {
		t.Fatalf("next-cycle change moved the licence early: plan %s", got.PlanID)
	}
	// Scheduling the same change twice is still one row.
	if _, err := r.svc.CreateUpgradeOrder(context.Background(), billing.CreateUpgradeOrderInput{
		LicenseID: seed.Licence.ID, ToPlanID: target.ID,
	}); err != nil {
		t.Fatalf("reschedule: %v", err)
	}
	var n int
	if err := r.s.DB.NewRaw("SELECT count(*) FROM license_upgrades WHERE license_id = ?", seed.Licence.ID).
		Scan(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("upgrade rows = %d, want 1", n)
	}
}
