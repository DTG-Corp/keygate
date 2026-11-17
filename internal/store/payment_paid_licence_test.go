package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/internal/testsupport/testdb"
)

// seedPaidOrder grants a product + plan and a pending new order for it,
// removing everything the test hung on them when it ends.
func seedPaidOrder(t *testing.T, s *store.Store, licenseType, interval string, trialDays int) (*model.Plan, *model.PaymentOrder) {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:13]
	prod := &model.Product{Name: "Paid Co", Slug: "paid-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Paid", Slug: "paid-plan-" + suffix,
		LicenseType: licenseType, BillingInterval: interval, TrialDays: trialDays,
		LicenseModel: "standard", MaxActivations: 3, Active: true,
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: prod.ID, PlanID: plan.ID, Email: "paid-" + suffix + "@example.com",
		Kind: "new", Quantity: 1, SubtotalMinor: 1190000, TotalMinor: 1190000,
		Currency: "VND", Provider: "sepay", ExpiresAt: time.Now().Add(time.Hour),
		Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.DB.NewRaw("DELETE FROM payment_transactions WHERE order_id IN (SELECT id FROM payment_orders WHERE product_id = ?)", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM subscriptions WHERE license_id IN (SELECT id FROM licenses WHERE product_id = ?)", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM payment_orders WHERE product_id = ?", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM licenses WHERE product_id = ?", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM plans WHERE id = ?", plan.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", prod.ID).Exec(ctx)
	})
	return plan, order
}

func paidOrderLicence(t *testing.T, s *store.Store, plan *model.Plan, order *model.PaymentOrder) *model.License {
	t.Helper()
	key, err := s.NewLicenseKey(context.Background(), order.ProductID)
	if err != nil {
		t.Fatal(err)
	}
	return &model.License{
		ProductID: order.ProductID, PlanID: plan.ID, Email: order.Email,
		LicenseKey: key, Status: model.StatusActive, Quantity: 1,
		PaymentProvider: order.Provider,
	}
}

// The licence and its subscription row are written on the caller's
// transaction, the licence id is the order id (the derivation settlement
// relies on), and the subscription carries the period, provider and
// external reference a renewal and the billing engine need.
func TestCreatePaidOrderLicenceWritesSubscriptionPeriod(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	plan, order := seedPaidOrder(t, s, "subscription", "month", 0)
	start := time.Now().UTC().Truncate(time.Second)
	end := start.Add(30 * 24 * time.Hour)
	order.PeriodStart, order.PeriodEnd = &start, &end
	lic := paidOrderLicence(t, s, plan, order)

	var gotLic *model.License
	var gotSub *model.Subscription
	if err := s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		var err error
		gotLic, gotSub, err = s.CreatePaidOrderLicence(ctx, tx, store.PaidOrderLicenceInput{
			Order: order, Plan: plan, License: lic,
		})
		return err
	}); err != nil {
		t.Fatalf("create paid order licence: %v", err)
	}
	if gotLic == nil || gotLic.ID != order.ID {
		t.Fatalf("licence id = %+v, want the order id %s derived", gotLic, order.ID)
	}
	if gotLic.LicenseKey != lic.LicenseKey || gotLic.KeyHash == "" {
		t.Fatalf("licence key = %q, hash = %q", gotLic.LicenseKey, gotLic.KeyHash)
	}
	if gotSub == nil {
		t.Fatal("no subscription row written for a subscription plan")
	}
	if gotSub.LicenseID != order.ID || gotSub.PlanID != plan.ID || gotSub.Status != model.StatusActive {
		t.Fatalf("subscription = %+v", gotSub)
	}
	if gotSub.PaymentProvider != "sepay" || gotSub.ExternalID != order.Code {
		t.Fatalf("subscription provider/external = %q/%q, want sepay/%q",
			gotSub.PaymentProvider, gotSub.ExternalID, order.Code)
	}
	if gotSub.CurrentPeriodStart == nil || !gotSub.CurrentPeriodStart.Equal(start) {
		t.Fatalf("period start = %v, want %v", gotSub.CurrentPeriodStart, start)
	}
	if gotSub.CurrentPeriodEnd == nil || !gotSub.CurrentPeriodEnd.Equal(end) {
		t.Fatalf("period end = %v, want %v", gotSub.CurrentPeriodEnd, end)
	}

	// One row each, read back from the database.
	var licences, subs int
	if err := s.DB.NewRaw("SELECT count(*) FROM licenses WHERE id = ?", order.ID).Scan(ctx, &licences); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.NewRaw("SELECT count(*) FROM subscriptions WHERE license_id = ?", order.ID).Scan(ctx, &subs); err != nil {
		t.Fatal(err)
	}
	if licences != 1 || subs != 1 {
		t.Fatalf("licences = %d, subscriptions = %d, want 1/1", licences, subs)
	}
}

// A trial plan's licence is trialing, both on the licence and on its
// subscription, and its first period ends where the trial ends — the
// same date the licence uses.
func TestCreatePaidOrderLicenceTrialIsTrialing(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	plan, order := seedPaidOrder(t, s, "trial", "", 14)
	lic := paidOrderLicence(t, s, plan, order)
	before := time.Now()

	var gotLic *model.License
	var gotSub *model.Subscription
	if err := s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		var err error
		gotLic, gotSub, err = s.CreatePaidOrderLicence(ctx, tx, store.PaidOrderLicenceInput{
			Order: order, Plan: plan, License: lic,
		})
		return err
	}); err != nil {
		t.Fatalf("create paid order licence: %v", err)
	}
	if gotLic.Status != model.StatusTrialing {
		t.Fatalf("licence status = %q, want trialing", gotLic.Status)
	}
	if gotLic.ValidUntil == nil {
		t.Fatal("trial licence has no valid_until")
	}
	wantEnd := before.Add(14 * 24 * time.Hour)
	if delta := gotLic.ValidUntil.Sub(wantEnd); delta < -time.Minute || delta > time.Minute {
		t.Fatalf("valid_until = %v, want about %v", gotLic.ValidUntil, wantEnd)
	}
	if gotSub == nil || gotSub.Status != model.StatusTrialing {
		t.Fatalf("subscription = %+v, want trialing", gotSub)
	}
	if gotSub.CurrentPeriodEnd == nil || !gotSub.CurrentPeriodEnd.Equal(*gotLic.ValidUntil) {
		t.Fatalf("period end = %v, want the licence's %v", gotSub.CurrentPeriodEnd, gotLic.ValidUntil)
	}
	if gotSub.TrialStart == nil || gotSub.TrialEnd == nil || !gotSub.TrialEnd.Equal(*gotLic.ValidUntil) {
		t.Fatalf("trial window = %v..%v, want ending %v", gotSub.TrialStart, gotSub.TrialEnd, gotLic.ValidUntil)
	}
}

// A renewal of a past-due licence revives it: the extension write clears
// past_due_at and sets the licence active in the same statement, so a
// customer who paid is never left past due.
func TestExtendLicenseFromOrderReactivatesPastDue(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, plan, lic := seedRenewalFixture(t, s, "perpetual")
	order := renewalOrder(t, s, prod, plan, lic)
	var updatesUntil time.Time
	if err := s.DB.NewRaw("SELECT updates_until FROM licenses WHERE id = ?", lic.ID).Scan(ctx, &updatesUntil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw(`UPDATE licenses SET status = ?, past_due_at = now() WHERE id = ?`,
		model.StatusPastDue, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	if err := s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		return store.ExtendLicenseFromOrderIn(ctx, tx, order, plan)
	}); err != nil {
		t.Fatalf("extend past-due perpetual: %v", err)
	}
	var status string
	var pastDue *time.Time
	var gotUntil time.Time
	if err := s.DB.NewRaw("SELECT status, past_due_at, updates_until FROM licenses WHERE id = ?", lic.ID).
		Scan(ctx, &status, &pastDue, &gotUntil); err != nil {
		t.Fatal(err)
	}
	if status != model.StatusActive || pastDue != nil {
		t.Fatalf("status = %q, past_due_at = %v, want active/nil", status, pastDue)
	}
	want := updatesUntil.AddDate(0, 0, plan.RenewalDays)
	if !gotUntil.Equal(want) {
		t.Fatalf("updates_until = %v, want %v", gotUntil, want)
	}
	ledger, err := s.FindLicenseRenewalByPaymentOrder(ctx, order.ID)
	if err != nil {
		t.Fatalf("renewal ledger: %v", err)
	}
	if ledger.Days != plan.RenewalDays {
		t.Fatalf("ledger days = %d, want %d", ledger.Days, plan.RenewalDays)
	}
}
