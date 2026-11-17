package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

func seedPlan(t *testing.T, s *store.Store) *model.Plan {
	t.Helper()
	return seedPlanOn(t, s, "")
}

// seedPlanOn is seedPlan with a billing interval, for the cases where the
// plan's cycle decides which price slot is legal.
func seedPlanOn(t *testing.T, s *store.Store, billingInterval string) *model.Plan {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	p := &model.Product{Name: "PriceCo", Slug: "price-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, p); err != nil {
		t.Fatalf("create product: %v", err)
	}
	pl := &model.Plan{ProductID: p.ID, Name: "Pro", Slug: "price-pro-" + suffix, LicenseType: "subscription",
		LicenseModel: "standard", MaxActivations: 3, BillingInterval: billingInterval, Active: true}
	if err := s.CreatePlan(ctx, pl); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return pl
}

func TestUpsertPlanPriceAndResolve(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	pl := seedPlan(t, s)

	created, err := s.UpsertPlanPrice(ctx, pl.ID, store.PlanPriceInput{Provider: "sepay", Currency: "VND", AmountMinor: 1_190_000})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if created.CurrencyExponent != 0 || created.AmountMinor != 1_190_000 {
		t.Fatalf("row = %+v", created)
	}
	// The input omitted an interval, so the row lives in the plan's own
	// (here empty, perpetual) interval slot.
	got, err := s.ResolvePlanPrice(ctx, pl.ID, "sepay", "VND", "")
	if err != nil || got.ID != created.ID {
		t.Fatalf("resolve = %+v, %v", got, err)
	}
	// Read back from the database, not only the in-memory row: the
	// column defaults to 2, so a write that dropped the exponent would
	// turn 1,190,000 VND into 11,900.00 of nothing.
	if got.CurrencyExponent != 0 {
		t.Fatalf("resolved currency_exponent = %d, want 0 read back from the row", got.CurrencyExponent)
	}

	// Same (plan, provider, currency, interval) is replaced: one active
	// price per slot, the old row stays for order history.
	updated, err := s.UpsertPlanPrice(ctx, pl.ID, store.PlanPriceInput{Provider: "sepay", Currency: "VND", AmountMinor: 1_290_000})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if updated.ID == created.ID || updated.AmountMinor != 1_290_000 {
		t.Fatalf("upsert did not replace active row: %+v", updated)
	}
	if _, err := s.ResolvePlanPrice(ctx, pl.ID, "payos", "VND", ""); !errors.Is(err, store.ErrPriceNotConfigured) {
		t.Fatalf("resolve missing = %v, want ErrPriceNotConfigured", err)
	}
}

func TestUpsertPlanPriceValidation(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	pl := seedPlan(t, s)
	for _, in := range []store.PlanPriceInput{
		{Provider: "stripe", Currency: "VND", AmountMinor: 100},                     // phase 1 providers only
		{Provider: "sepay", Currency: "", AmountMinor: 100},                         // currency required
		{Provider: "sepay", Currency: "VND", AmountMinor: -1},                       // no negative price
		{Provider: "sepay", Currency: "USD", AmountMinor: 100, CurrencyExponent: 0}, // wrong exponent
	} {
		if _, err := s.UpsertPlanPrice(ctx, pl.ID, in); err == nil {
			t.Fatalf("input %+v accepted", in)
		}
	}
}

// One plan sells one billing cycle. A row saved without an interval takes
// the plan's, so a monthly plan's price lands in the slot checkout
// resolves; a row saved on another cycle is refused, because checkout
// resolves the plan's own interval and would never find it.
func TestUpsertPlanPriceUsesThePlanInterval(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	pl := seedPlanOn(t, s, "month")

	created, err := s.UpsertPlanPrice(ctx, pl.ID, store.PlanPriceInput{Provider: "sepay", Currency: "VND", AmountMinor: 199_000})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if created.BillingInterval != "month" {
		t.Fatalf("billing_interval = %q, want month from the plan", created.BillingInterval)
	}
	got, err := s.ResolvePlanPrice(ctx, pl.ID, "sepay", "VND", "month")
	if err != nil || got.ID != created.ID {
		t.Fatalf("resolve by plan interval = %+v, %v", got, err)
	}

	if _, err := s.UpsertPlanPrice(ctx, pl.ID, store.PlanPriceInput{
		Provider: "sepay", Currency: "VND", AmountMinor: 1, BillingInterval: "year",
	}); !errors.Is(err, store.ErrPriceInvalid) {
		t.Fatalf("year on a monthly plan = %v, want ErrPriceInvalid", err)
	}
}

// Two admins saving the same slot at once: one insert loses the partial
// unique index. That is a conflict the caller can retry, not a 500.
func TestUpsertPlanPriceConcurrentSlotConflict(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	pl := seedPlanOn(t, s, "month")

	// A competing writer holds the slot's row uncommitted, so the
	// upsert's deactivate cannot see it and its insert waits on the
	// unique index until the competitor commits.
	hold, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.ExecContext(ctx, `INSERT INTO plan_prices
		(id, plan_id, provider, currency, amount_minor, currency_exponent, billing_interval, active)
		VALUES (?, ?, 'sepay', 'VND', 100, 0, 'month', TRUE)`, store.NewID(), pl.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.UpsertPlanPrice(ctx, pl.ID, store.PlanPriceInput{Provider: "sepay", Currency: "VND", AmountMinor: 200})
		done <- err
	}()
	waitForPriceSlotWaiter(t, s, ctx)
	if err := hold.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, store.ErrPriceSlotConflict) {
		t.Fatalf("concurrent upsert = %v, want ErrPriceSlotConflict", err)
	}
}

// waitForPriceSlotWaiter waits until an upsert is blocked on the price
// slot's index, so the test commits the competing row only then. The
// query filter keeps a waiter for something else from counting.
func waitForPriceSlotWaiter(t *testing.T, s *store.Store, ctx context.Context) {
	t.Helper()
	for range 200 {
		var waiting int
		if err := s.DB.NewRaw(`SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			  AND query LIKE '%plan_prices%'`).Scan(ctx, &waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 1 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the upsert never waited on the price slot")
}

func TestListActivePlanPrices(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	pl := seedPlan(t, s)
	if _, err := s.UpsertPlanPrice(ctx, pl.ID, store.PlanPriceInput{Provider: "sepay", Currency: "VND", AmountMinor: 100_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertPlanPrice(ctx, pl.ID, store.PlanPriceInput{Provider: "payos", Currency: "VND", AmountMinor: 100_000}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListActivePlanPrices(ctx, []string{pl.ID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got[pl.ID]) != 2 {
		t.Fatalf("prices = %d, want 2", len(got[pl.ID]))
	}
	// Order is part of the contract: interval, then provider, so the
	// public handler's first-row fallback cannot flip between calls.
	if got[pl.ID][0].Provider != "payos" {
		t.Fatalf("first price provider = %s, want payos for a stable order", got[pl.ID][0].Provider)
	}
	if err := s.DeactivatePlanPrice(ctx, pl.ID, got[pl.ID][0].ID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	got, _ = s.ListActivePlanPrices(ctx, []string{pl.ID})
	if len(got[pl.ID]) != 1 {
		t.Fatalf("after deactivate = %d, want 1", len(got[pl.ID]))
	}
}

// The public pricing page must read only local rows: a plan with no
// active price is not sellable and carries no price in the response.
func TestPlanWithoutLocalPriceIsAbsent(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	pl := seedPlan(t, s)
	prices, err := s.ListActivePlanPrices(ctx, []string{pl.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(prices[pl.ID]) != 0 {
		t.Fatalf("prices = %d, want 0 before an admin enters one", len(prices[pl.ID]))
	}
	if _, err := s.ResolvePlanPrice(ctx, pl.ID, "sepay", "VND", "year"); !errors.Is(err, store.ErrPriceNotConfigured) {
		t.Fatalf("resolve = %v, want ErrPriceNotConfigured", err)
	}
}
