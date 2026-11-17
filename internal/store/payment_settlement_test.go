package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

// seedRenewalFixture creates the product, plan and licence a renewal is
// applied to, and removes them when the test ends.
func seedRenewalFixture(t *testing.T, s *store.Store, licenseType string) (*model.Product, *model.Plan, *model.License) {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:13]
	prod := &model.Product{Name: "Renew Store", Slug: "renew-store-" + suffix, Type: "desktop"}
	if licenseType == "perpetual" {
		// A finite maintenance window needs the operator's confirmation
		// and a product whose feeds can enforce it; both are set up here
		// so the write goes through the real path.
		testsupport.SetSetting(t, s.DB, store.SettingMaintenanceFeatures, "true")
		drained := time.Now().Add(-30 * 24 * time.Hour)
		prod.FeedLicenseRequired = true
		prod.FeedGatedAt = &drained
	}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Renew", Slug: "renew-plan-" + suffix,
		LicenseType: licenseType, LicenseModel: "standard", Active: true, RenewalDays: 30,
	}
	if licenseType == "perpetual" {
		plan.UpdatesDays = 365
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{
		ProductID: prod.ID, PlanID: plan.ID, Email: "renew-store-" + suffix + "@example.com",
		LicenseKey: "KEY-RENEW-STORE-" + suffix, Status: model.StatusActive, Quantity: 1,
	}
	if licenseType == "perpetual" {
		lic.UpdatesUntil = plan.InitialUpdatesUntil(time.Now())
		lic.UpdatesTermsSet = true
	}
	if err := s.CreateLicenseWithSubscription(ctx, lic, plan); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.DB.NewRaw("DELETE FROM payment_orders WHERE product_id = ?", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM licenses WHERE product_id = ?", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM plans WHERE id = ?", plan.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", prod.ID).Exec(ctx)
	})
	return prod, plan, lic
}

func renewalOrder(t *testing.T, s *store.Store, prod *model.Product, plan *model.Plan, lic *model.License) *model.PaymentOrder {
	t.Helper()
	order, err := s.CreatePaymentOrder(context.Background(), store.CreateOrderInput{
		ProductID: prod.ID, PlanID: plan.ID, LicenseID: lic.ID, Email: lic.Email, Kind: "renewal",
		Quantity: 1, SubtotalMinor: 1190000, TotalMinor: 1190000, Currency: "VND",
		Provider: "sepay", ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return order
}

// A perpetual renewal extends the maintenance window through the
// existing ledger logic and records the payment order that bought it, so
// a replay is a no-op instead of a second extension.
func TestExtendLicenseFromOrderPerpetual(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, plan, lic := seedRenewalFixture(t, s, "perpetual")
	order := renewalOrder(t, s, prod, plan, lic)
	// Re-read: the database keeps microseconds, the in-memory value the
	// nanoseconds it was built with.
	stored, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := *stored.UpdatesUntil

	apply := func() error {
		return s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
			return store.ExtendLicenseFromOrderIn(ctx, tx, order, plan)
		})
	}
	if err := apply(); err != nil {
		t.Fatalf("extend: %v", err)
	}
	got, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := before.AddDate(0, 0, plan.RenewalDays)
	if got.UpdatesUntil == nil || !got.UpdatesUntil.Equal(want) {
		t.Fatalf("updates_until = %v, want %v", got.UpdatesUntil, want)
	}
	ledger, err := s.FindLicenseRenewalByPaymentOrder(ctx, order.ID)
	if err != nil {
		t.Fatalf("renewal ledger: %v", err)
	}
	if ledger.LicenseID != lic.ID || ledger.Days != plan.RenewalDays {
		t.Fatalf("ledger = %+v", ledger)
	}

	// The same order applied again: the ledger row is the receipt, so
	// nothing moves and no second row is written.
	if err := apply(); err != nil {
		t.Fatalf("replay: %v", err)
	}
	got, err = s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatesUntil == nil || !got.UpdatesUntil.Equal(want) {
		t.Fatalf("replay moved updates_until to %v, want %v", got.UpdatesUntil, want)
	}
	var n int
	if err := s.DB.NewRaw("SELECT count(*) FROM license_renewals WHERE payment_order_id = ?", order.ID).
		Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("ledger rows = %d, want 1", n)
	}
}

// A subscription renewal pushes valid_until one interval past the current
// end and keeps the subscription row in step; the ledger carries the
// order id there too.
func TestExtendLicenseFromOrderSubscription(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, plan, lic := seedRenewalFixture(t, s, "subscription")
	oldEnd := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	if _, err := s.DB.NewRaw("UPDATE licenses SET valid_until = ? WHERE id = ?", oldEnd, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	order := renewalOrder(t, s, prod, plan, lic)

	if err := s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		return store.ExtendLicenseFromOrderIn(ctx, tx, order, plan)
	}); err != nil {
		t.Fatalf("extend: %v", err)
	}
	got, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantEnd := oldEnd.Add(30 * 24 * time.Hour)
	if got.ValidUntil == nil || !got.ValidUntil.Equal(wantEnd) {
		t.Fatalf("valid_until = %v, want %v", got.ValidUntil, wantEnd)
	}
	var periodStart, periodEnd time.Time
	if err := s.DB.NewRaw("SELECT current_period_start, current_period_end FROM subscriptions WHERE license_id = ?", lic.ID).
		Scan(ctx, &periodStart, &periodEnd); err != nil {
		t.Fatal(err)
	}
	if !periodEnd.Equal(wantEnd) {
		t.Fatalf("subscription period end = %v, want %v", periodEnd, wantEnd)
	}
	ledger, err := s.FindLicenseRenewalByPaymentOrder(ctx, order.ID)
	if err != nil {
		t.Fatalf("renewal ledger: %v", err)
	}
	if ledger.LicenseID != lic.ID || ledger.Days != 30 {
		t.Fatalf("ledger = %+v", ledger)
	}

	// A replay changes nothing.
	if err := s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		return store.ExtendLicenseFromOrderIn(ctx, tx, order, plan)
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	got, err = s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ValidUntil == nil || !got.ValidUntil.Equal(wantEnd) {
		t.Fatalf("replay moved valid_until to %v", got.ValidUntil)
	}
}

// A licence that is no longer active has nothing worth extending; the
// renewal is refused so the order stays visible to an operator instead
// of being recorded as fulfilled for no benefit.
func TestExtendLicenseFromOrderRefusesInactiveLicense(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, plan, lic := seedRenewalFixture(t, s, "perpetual")
	if _, err := s.DB.NewRaw("UPDATE licenses SET status = ? WHERE id = ?", model.StatusSuspended, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	order := renewalOrder(t, s, prod, plan, lic)

	err := s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		return store.ExtendLicenseFromOrderIn(ctx, tx, order, plan)
	})
	if !errors.Is(err, store.ErrRenewalIneligible) {
		t.Fatalf("extend suspended = %v, want ErrRenewalIneligible", err)
	}
}

// A renewal order that names no licence cannot be applied: guessing one
// from the email would extend the wrong customer.
func TestExtendLicenseFromOrderRequiresLicense(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, plan, lic := seedRenewalFixture(t, s, "subscription")
	order := renewalOrder(t, s, prod, plan, lic)
	order.LicenseID = ""

	err := s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		return store.ExtendLicenseFromOrderIn(ctx, tx, order, plan)
	})
	if err == nil {
		t.Fatal("renewal with no licence was applied")
	}
}
