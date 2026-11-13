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
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	p := &model.Product{Name: "PriceCo", Slug: "price-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, p); err != nil {
		t.Fatalf("create product: %v", err)
	}
	pl := &model.Plan{ProductID: p.ID, Name: "Pro", Slug: "price-pro-" + suffix, LicenseType: "subscription", LicenseModel: "standard", MaxActivations: 3, Active: true}
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
	// The input omitted an interval, so the row lives in the "" slot.
	got, err := s.ResolvePlanPrice(ctx, pl.ID, "sepay", "VND", "")
	if err != nil || got.ID != created.ID {
		t.Fatalf("resolve = %+v, %v", got, err)
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
