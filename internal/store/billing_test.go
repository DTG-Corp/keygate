package store_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

// renewalKind is checkout.KindRenewal; spelled out so this test package
// stays free of the checkout dependency.
const renewalKind = "renewal"

// billingFixture is one subscription licence pinned to an exact period,
// the state the billing engine scans.
type billingFixture struct {
	Product *model.Product
	Plan    *model.Plan
	Licence *model.License
	Sub     *model.Subscription
}

type billingOpts struct {
	Provider string // default sepay
	Licence  string // default active
	Sub      string // default active
	Start    time.Time
	End      time.Time
	Cancel   bool
}

func seedBillingFixture(t *testing.T, s *store.Store, opts billingOpts) *billingFixture {
	t.Helper()
	ctx := context.Background()
	if opts.Provider == "" {
		opts.Provider = "sepay"
	}
	if opts.Licence == "" {
		opts.Licence = model.StatusActive
	}
	if opts.Sub == "" {
		opts.Sub = model.StatusActive
	}
	if opts.End.IsZero() {
		opts.End = time.Now().Add(3 * 24 * time.Hour).UTC()
	}
	if opts.Start.IsZero() {
		opts.Start = opts.End.AddDate(0, -1, 0)
	}
	suffix := uuid.NewString()[:13]
	prod := &model.Product{Name: "Billing Store", Slug: "billing-store-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Monthly", Slug: "billing-plan-" + suffix,
		LicenseType: "subscription", BillingInterval: "month", LicenseModel: "standard",
		Active: true, GraceDays: 7, MaxActivations: 3,
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	lic := &model.License{
		ProductID: prod.ID, PlanID: plan.ID, Email: "billing-" + suffix + "@example.com",
		LicenseKey: "KEY-BILLING-" + suffix, Status: opts.Licence, Quantity: 1,
		ValidUntil: &opts.End,
	}
	if err := s.CreateLicenseWithSubscription(ctx, lic, plan); err != nil {
		t.Fatalf("create licence: %v", err)
	}
	sub := new(model.Subscription)
	if err := s.DB.NewSelect().Model(sub).Where("license_id = ?", lic.ID).Scan(ctx); err != nil {
		t.Fatalf("load subscription: %v", err)
	}
	if _, err := s.DB.NewRaw(`UPDATE subscriptions
		SET payment_provider = ?, external_id = ?, status = ?,
		    current_period_start = ?, current_period_end = ?, cancel_at_period_end = ?
		WHERE id = ?`,
		opts.Provider, "SUB-"+suffix, opts.Sub, opts.Start, opts.End, opts.Cancel, sub.ID).Exec(ctx); err != nil {
		t.Fatalf("pin subscription: %v", err)
	}
	if _, err := s.DB.NewRaw(`UPDATE licenses SET status = ?, valid_until = ? WHERE id = ?`,
		opts.Licence, opts.End, lic.ID).Exec(ctx); err != nil {
		t.Fatalf("pin licence: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.DB.NewRaw("DELETE FROM notifications WHERE license_id = ?", lic.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM license_renewals WHERE license_id = ?", lic.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM payment_orders WHERE license_id = ?", lic.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM subscriptions WHERE license_id = ?", lic.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM licenses WHERE id = ?", lic.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM plans WHERE id = ?", plan.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", prod.ID).Exec(ctx)
	})
	// Reload with the relations a billing pass expects.
	full, err := s.GetBillingSubscription(ctx, lic.ID)
	if err != nil {
		t.Fatalf("reload subscription: %v", err)
	}
	if full.License != nil {
		lic = full.License
	}
	return &billingFixture{Product: prod, Plan: plan, Licence: lic, Sub: full}
}

func TestListBillingSubscriptionsFilters(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Now().UTC()

	live := seedBillingFixture(t, s, billingOpts{End: now.Add(3 * 24 * time.Hour)})
	seedBillingFixture(t, s, billingOpts{Provider: "stripe", End: now.Add(3 * 24 * time.Hour)})
	seedBillingFixture(t, s, billingOpts{Licence: model.StatusRevoked, End: now.Add(3 * 24 * time.Hour)})
	seedBillingFixture(t, s, billingOpts{End: now.Add(30 * 24 * time.Hour)})

	rows, err := s.ListBillingSubscriptions(ctx, now, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found *model.Subscription
	for _, row := range rows {
		if row.LicenseID == live.Licence.ID {
			found = row
		}
		if row.PaymentProvider == "stripe" {
			t.Fatalf("stripe subscription %s was selected", row.ID)
		}
		if row.License != nil && row.License.Status == model.StatusRevoked {
			t.Fatalf("revoked licence %s was selected", row.LicenseID)
		}
	}
	if found == nil {
		t.Fatalf("live subscription %s missing from %d rows", live.Sub.ID, len(rows))
	}
	if found.Plan == nil || found.Plan.ID != live.Plan.ID {
		t.Fatalf("plan relation not loaded: %+v", found.Plan)
	}
	if found.License == nil || found.License.Product == nil {
		t.Fatalf("licence/product relation not loaded: %+v", found.License)
	}
	if !found.CurrentPeriodEnd.Equal(*live.Sub.CurrentPeriodEnd) {
		t.Fatalf("period end = %v", found.CurrentPeriodEnd)
	}
}

func TestListBillingIncludesSuspendedTail(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Now().UTC()
	f := seedBillingFixture(t, s, billingOpts{Licence: model.StatusSuspended, Sub: model.StatusPastDue,
		End: now.Add(-60 * 24 * time.Hour)})
	if _, err := s.DB.NewRaw(`UPDATE licenses SET suspended_by = ?, suspended_at = ? WHERE id = ?`,
		store.BillingSuspendedBy, now.Add(-30*24*time.Hour), f.Licence.ID).Exec(ctx); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	rows, err := s.ListBillingSubscriptions(ctx, now, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, row := range rows {
		if row.LicenseID == f.Licence.ID {
			return
		}
	}
	t.Fatalf("billing suspension %s was not selected for expiry", f.Licence.ID)
}

func TestListBillingTakesNewestSubscription(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Now().UTC()
	f := seedBillingFixture(t, s, billingOpts{End: now.Add(2 * 24 * time.Hour)})
	// An older duplicate row for the same licence must not win the scan.
	older := now.Add(-90 * 24 * time.Hour)
	old := &model.Subscription{
		ID: store.NewID(), LicenseID: f.Licence.ID, PlanID: f.Plan.ID, Status: model.StatusCanceled,
		PaymentProvider: "sepay", CurrentPeriodEnd: &older, CreatedAt: older, UpdatedAt: older,
	}
	if _, err := s.DB.NewInsert().Model(old).Exec(ctx); err != nil {
		t.Fatalf("insert older subscription: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DB.NewRaw("DELETE FROM subscriptions WHERE id = ?", old.ID).Exec(context.Background())
	})

	rows, err := s.ListBillingSubscriptions(ctx, now, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := 0
	for _, row := range rows {
		if row.LicenseID != f.Licence.ID {
			continue
		}
		seen++
		if row.ID != f.Sub.ID {
			t.Fatalf("selected subscription %s, want the newest %s", row.ID, f.Sub.ID)
		}
	}
	if seen != 1 {
		t.Fatalf("licence selected %d times, want 1", seen)
	}
}

func TestMarkBillingPastDueStampsOnce(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Now().UTC()
	f := seedBillingFixture(t, s, billingOpts{End: now.Add(-time.Hour)})

	ok, err := s.MarkBillingPastDue(ctx, f.Licence, f.Sub, now)
	if err != nil || !ok {
		t.Fatalf("first mark = %v, %v", ok, err)
	}
	lic, err := s.FindLicenseByID(ctx, f.Licence.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lic.Status != model.StatusPastDue || lic.PastDueAt == nil {
		t.Fatalf("licence = %s past_due_at %v", lic.Status, lic.PastDueAt)
	}
	stamp := *lic.PastDueAt
	sub, err := s.GetBillingSubscription(ctx, f.Licence.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != model.StatusPastDue {
		t.Fatalf("subscription status = %s", sub.Status)
	}

	ok, err = s.MarkBillingPastDue(ctx, lic, sub, now.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("second mark: %v", err)
	}
	if ok {
		t.Fatal("second mark reported a change")
	}
	again, err := s.FindLicenseByID(ctx, f.Licence.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.PastDueAt == nil || !again.PastDueAt.Equal(stamp) {
		t.Fatalf("past_due_at moved: %v -> %v", stamp, again.PastDueAt)
	}
}

func TestMarkBillingPastDueSkipsExtendedLicence(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Now().UTC()
	f := seedBillingFixture(t, s, billingOpts{End: now.Add(-time.Hour)})
	// A payment landed after the scan read the period.
	if _, err := s.DB.NewRaw(`UPDATE licenses SET valid_until = ? WHERE id = ?`,
		now.AddDate(0, 1, 0), f.Licence.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	ok, err := s.MarkBillingPastDue(ctx, f.Licence, f.Sub, now)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if ok {
		t.Fatal("an extended licence was marked past due")
	}
}

func TestSuspendBillingOnlyFromLiveStates(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Now().UTC()

	due := seedBillingFixture(t, s, billingOpts{Licence: model.StatusPastDue, Sub: model.StatusPastDue,
		End: now.Add(-8 * 24 * time.Hour)})
	if _, err := s.DB.NewRaw(`UPDATE licenses SET past_due_at = ? WHERE id = ?`,
		now.Add(-8*24*time.Hour), due.Licence.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	ok, err := s.SuspendBillingLicense(ctx, due.Licence, due.Sub, now)
	if err != nil || !ok {
		t.Fatalf("suspend = %v, %v", ok, err)
	}
	lic, err := s.FindLicenseByID(ctx, due.Licence.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lic.Status != model.StatusSuspended || lic.SuspendedAt == nil || lic.SuspendedBy != store.BillingSuspendedBy {
		t.Fatalf("licence = %s by %q at %v", lic.Status, lic.SuspendedBy, lic.SuspendedAt)
	}

	// An operator's suspension is not ours to re-suspend, and an already
	// suspended licence is not suspended twice.
	held := seedBillingFixture(t, s, billingOpts{Licence: model.StatusSuspended, Sub: model.StatusPastDue,
		End: now.Add(-8 * 24 * time.Hour)})
	if _, err := s.DB.NewRaw(`UPDATE licenses SET suspended_by = ?, suspended_at = ? WHERE id = ?`,
		model.SuspendedByAdmin, now.Add(-time.Hour), held.Licence.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	ok, err = s.SuspendBillingLicense(ctx, held.Licence, held.Sub, now)
	if err != nil {
		t.Fatalf("admin suspension: %v", err)
	}
	if ok {
		t.Fatal("an admin suspension was rewritten")
	}
}

func TestExpireBillingFromPastDueAndBillingSuspension(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Now().UTC()

	due := seedBillingFixture(t, s, billingOpts{Licence: model.StatusPastDue, Sub: model.StatusPastDue,
		End: now.Add(-40 * 24 * time.Hour)})
	ok, err := s.ExpireBillingLicense(ctx, due.Licence, due.Sub, now)
	if err != nil || !ok {
		t.Fatalf("expire past due = %v, %v", ok, err)
	}
	if lic, err := s.FindLicenseByID(ctx, due.Licence.ID); err != nil || lic.Status != model.StatusExpired {
		t.Fatalf("licence = %v (%v)", lic, err)
	}
	sub, err := s.GetBillingSubscription(ctx, due.Licence.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != model.StatusExpired {
		t.Fatalf("subscription = %s, want expired", sub.Status)
	}

	suspended := seedBillingFixture(t, s, billingOpts{Licence: model.StatusSuspended, Sub: model.StatusPastDue,
		End: now.Add(-7 * 24 * time.Hour)})
	if _, err := s.DB.NewRaw(`UPDATE licenses SET suspended_by = ?, suspended_at = ? WHERE id = ?`,
		store.BillingSuspendedBy, now.Add(-3*24*time.Hour), suspended.Licence.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	ok, err = s.ExpireBillingLicense(ctx, suspended.Licence, suspended.Sub, now)
	if err != nil || !ok {
		t.Fatalf("expire suspension = %v, %v", ok, err)
	}

	held := seedBillingFixture(t, s, billingOpts{Licence: model.StatusSuspended, Sub: model.StatusPastDue,
		End: now.Add(-7 * 24 * time.Hour)})
	if _, err := s.DB.NewRaw(`UPDATE licenses SET suspended_by = ?, suspended_at = ? WHERE id = ?`,
		model.SuspendedByAdmin, now.Add(-3*24*time.Hour), held.Licence.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	ok, err = s.ExpireBillingLicense(ctx, held.Licence, held.Sub, now)
	if err != nil {
		t.Fatalf("expire admin suspension: %v", err)
	}
	if ok {
		t.Fatal("an admin suspension was expired by the billing engine")
	}
}

func TestCancelSubscriptionAtPeriodEndOnce(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Now().UTC()
	end := now.Add(-time.Hour)
	f := seedBillingFixture(t, s, billingOpts{End: end, Cancel: true})

	ok, err := s.CancelSubscriptionAtPeriodEnd(ctx, f.Sub, now)
	if err != nil || !ok {
		t.Fatalf("cancel = %v, %v", ok, err)
	}
	sub, err := s.GetBillingSubscription(ctx, f.Licence.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != model.StatusCanceled || sub.CanceledAt == nil {
		t.Fatalf("subscription = %s canceled_at %v", sub.Status, sub.CanceledAt)
	}
	stamp := *sub.CanceledAt
	ok, err = s.CancelSubscriptionAtPeriodEnd(ctx, sub, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if ok {
		t.Fatal("second cancel reported a change")
	}
	again, err := s.GetBillingSubscription(ctx, f.Licence.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.CanceledAt == nil || !again.CanceledAt.Equal(stamp) {
		t.Fatalf("canceled_at moved: %v -> %v", stamp, again.CanceledAt)
	}
	// The licence keeps its access until the period really ends.
	lic, err := s.FindLicenseByID(ctx, f.Licence.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lic.Status != model.StatusActive || lic.ValidUntil == nil || !lic.ValidUntil.Equal(end) {
		t.Fatalf("licence = %s valid_until %v", lic.Status, lic.ValidUntil)
	}
}

func TestFindOpenRenewalOrderIgnoresExpired(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Now().UTC()
	f := seedBillingFixture(t, s, billingOpts{End: now.Add(-time.Hour)})
	periodEnd := *f.Sub.CurrentPeriodEnd

	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: f.Product.ID, PlanID: f.Plan.ID, Email: f.Licence.Email,
		Kind: renewalKind, Quantity: 1, SubtotalMinor: 100_000, TotalMinor: 100_000,
		Currency: "VND", CurrencyExponent: 0, Provider: "sepay", ExpiresAt: now.Add(time.Hour),
		LicenseID: f.Licence.ID, PeriodStart: f.Sub.CurrentPeriodStart, PeriodEnd: &periodEnd,
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	found, err := s.FindOpenRenewalOrderForPeriod(ctx, f.Licence.ID, periodEnd)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found.ID != order.ID {
		t.Fatalf("found %s, want %s", found.ID, order.ID)
	}
	if _, err := s.DB.NewRaw(`UPDATE payment_orders SET status = ? WHERE id = ?`,
		domain.OrderExpired, order.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FindOpenRenewalOrderForPeriod(ctx, f.Licence.ID, periodEnd); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired order err = %v, want sql.ErrNoRows", err)
	}
}

func TestIsRenewalOrderConflict(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Now().UTC()
	f := seedBillingFixture(t, s, billingOpts{End: now.Add(time.Hour)})
	periodEnd := *f.Sub.CurrentPeriodEnd
	in := store.CreateOrderInput{
		ProductID: f.Product.ID, PlanID: f.Plan.ID, Email: f.Licence.Email,
		Kind: renewalKind, Quantity: 1, SubtotalMinor: 100_000, TotalMinor: 100_000,
		Currency: "VND", CurrencyExponent: 0, Provider: "sepay", ExpiresAt: now.Add(time.Hour),
		LicenseID: f.Licence.ID, PeriodStart: f.Sub.CurrentPeriodStart, PeriodEnd: &periodEnd,
	}
	if _, err := s.CreatePaymentOrder(ctx, in); err != nil {
		t.Fatalf("first order: %v", err)
	}
	_, err := s.CreatePaymentOrder(ctx, in)
	if err == nil {
		t.Fatal("second pending renewal for one period was accepted")
	}
	if store.IsRenewalOrderConflict(err) != true {
		t.Fatalf("err = %v, want a renewal-order conflict", err)
	}
	// Any other duplicate is not a renewal conflict.
	if store.IsRenewalOrderConflict(errors.New("payment_orders_code_key")) {
		t.Fatal("a plain error was classified as a renewal conflict")
	}
}

func TestReinstateLicenseForRenewalInLiftsOnlyBillingSuspension(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Now().UTC()

	ours := seedBillingFixture(t, s, billingOpts{Licence: model.StatusSuspended, Sub: model.StatusPastDue,
		End: now.Add(-8 * 24 * time.Hour)})
	if _, err := s.DB.NewRaw(`UPDATE licenses SET suspended_by = ?, suspended_at = ?, past_due_at = ? WHERE id = ?`,
		store.BillingSuspendedBy, now.Add(-time.Hour), now.Add(-8*24*time.Hour), ours.Licence.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	ok, err := s.ReinstateLicenseForRenewalIn(ctx, s.DB, ours.Licence.ID)
	if err != nil || !ok {
		t.Fatalf("reinstate = %v, %v", ok, err)
	}
	lic, err := s.FindLicenseByID(ctx, ours.Licence.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lic.Status != model.StatusActive || lic.SuspendedAt != nil || lic.SuspendedBy != "" || lic.PastDueAt != nil {
		t.Fatalf("licence = %s by %q at %v, past_due_at %v", lic.Status, lic.SuspendedBy, lic.SuspendedAt, lic.PastDueAt)
	}

	held := seedBillingFixture(t, s, billingOpts{Licence: model.StatusSuspended, Sub: model.StatusPastDue,
		End: now.Add(-8 * 24 * time.Hour)})
	if _, err := s.DB.NewRaw(`UPDATE licenses SET suspended_by = ?, suspended_at = ? WHERE id = ?`,
		model.SuspendedByAdmin, now.Add(-time.Hour), held.Licence.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	ok, err = s.ReinstateLicenseForRenewalIn(ctx, s.DB, held.Licence.ID)
	if err != nil {
		t.Fatalf("admin reinstate: %v", err)
	}
	if ok {
		t.Fatal("an operator's suspension was lifted by a renewal")
	}
	again, err := s.FindLicenseByID(ctx, held.Licence.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != model.StatusSuspended || again.SuspendedBy != model.SuspendedByAdmin {
		t.Fatalf("licence = %s by %q", again.Status, again.SuspendedBy)
	}
}
