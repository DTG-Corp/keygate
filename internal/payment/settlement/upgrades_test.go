package settlement_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/settlement"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

// seedUpgradeLicence creates a product, the licence's current plan, a
// target plan and a subscription-backed licence, and removes everything
// the test hangs on when it ends (upgrade rows first: they reference the
// licence).
func seedUpgradeLicence(t *testing.T, s *store.Store, licenceStatus, suspendedBy string) (*model.Product, *model.Plan, *model.Plan, *model.License, time.Time) {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:13]
	prod := &model.Product{Name: "Upgrade Co " + suffix, Slug: "upgrade-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	from := &model.Plan{ProductID: prod.ID, Name: "Monthly", Slug: "monthly-" + suffix,
		LicenseType: "subscription", BillingInterval: "month", LicenseModel: "standard", Active: true}
	if err := s.CreatePlan(ctx, from); err != nil {
		t.Fatal(err)
	}
	to := &model.Plan{ProductID: prod.ID, Name: "Yearly", Slug: "yearly-" + suffix,
		LicenseType: "subscription", BillingInterval: "month", LicenseModel: "standard", Active: true}
	if err := s.CreatePlan(ctx, to); err != nil {
		t.Fatal(err)
	}
	// The end is far enough away that the billing engine's own tests,
	// which scan every subscription with a pinned clock, cannot mistake
	// these settlements' licences for due ones and suspend them while the
	// two packages run in parallel.
	end := time.Now().UTC().Truncate(time.Second).AddDate(10, 0, 0)
	start := end.Add(-30 * 24 * time.Hour)
	lic := &model.License{ProductID: prod.ID, PlanID: from.ID, Email: "upgrade-" + suffix + "@example.com",
		LicenseKey: "KEY-UPGRADE-" + suffix, Status: model.StatusActive, Quantity: 1,
		ValidUntil: &end, PaymentProvider: "sepay"}
	if err := s.CreateLicenseWithSubscription(ctx, lic, from); err != nil {
		t.Fatal(err)
	}
	if licenceStatus == "" {
		licenceStatus = model.StatusActive
	}
	if _, err := s.DB.NewRaw(`UPDATE subscriptions
		SET payment_provider = 'sepay', status = ?, current_period_start = ?, current_period_end = ?
		WHERE license_id = ?`, licenceStatus, start, end, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw(`UPDATE licenses
		SET status = ?, suspended_by = ?, past_due_at = CASE WHEN ? = 'past_due' THEN now() ELSE NULL END
		WHERE id = ?`, licenceStatus, suspendedBy, licenceStatus, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.DB.NewRaw("DELETE FROM license_upgrades WHERE license_id = ?", lic.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM payment_events WHERE order_id IN (SELECT id FROM payment_orders WHERE product_id = ?)", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM payment_transactions WHERE order_id IN (SELECT id FROM payment_orders WHERE product_id = ?)", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM payment_orders WHERE product_id = ?", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM subscriptions WHERE license_id = ?", lic.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM licenses WHERE id = ?", lic.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM plans WHERE id IN (?, ?)", from.ID, to.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", prod.ID).Exec(ctx)
	})
	return prod, from, to, lic, end
}

// upgradeOrder places the pending upgrade order.
func upgradeOrder(t *testing.T, s *store.Store, prod *model.Product, target *model.Plan, lic *model.License, end time.Time) *model.PaymentOrder {
	t.Helper()
	start := time.Now().UTC().Truncate(time.Second)
	order, err := s.CreatePaymentOrder(context.Background(), store.CreateOrderInput{
		ProductID: prod.ID, PlanID: target.ID, LicenseID: lic.ID, Email: lic.Email,
		Kind: "upgrade", Quantity: 1, SubtotalMinor: 2_000_000, DiscountMinor: 800_000,
		CreditMinor: 800_000, TotalMinor: 1_200_000, Currency: "VND", Provider: "sepay",
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
		PeriodStart: &start, PeriodEnd: &end,
	})
	if err != nil {
		t.Fatal(err)
	}
	return order
}

func paidEvent(order *model.PaymentOrder) provider.WebhookEvent {
	return provider.WebhookEvent{
		Provider: "sepay", ProviderEventID: uuid.NewString(), ProviderRef: uuid.NewString(),
		OrderCode: order.Code, AmountMinor: order.TotalMinor, Currency: "VND",
		Status: domain.OrderPaid, TransferType: "in", OccurredAt: time.Now(),
		Raw: []byte(`{"id":"paid"}`),
	}
}

// An upgrade order moves the licence and its subscription to the target
// plan without moving the period the customer already paid for, once. A
// duplicated webhook grants and emits nothing twice.
func TestSettleUpgradeAppliesOnce(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, from, to, lic, end := seedUpgradeLicence(t, s, model.StatusActive, "")
	order := upgradeOrder(t, s, prod, to, lic, end)
	up := &model.LicenseUpgrade{LicenseID: lic.ID, PaymentOrderID: order.ID,
		ToPlanID: to.ID, FromPlanID: from.ID, FromQuantity: 1}
	if err := s.CreateUpgradeForOrderIn(ctx, s.DB, up); err != nil {
		t.Fatalf("create upgrade row: %v", err)
	}
	webhook, wh := webhookFixture(t, s, prod.ID, model.EventOrderPaid, model.EventPlanChanged, model.EventPaymentSucceeded)
	svc := &settlement.Service{Store: s, Webhooks: webhook, Log: testLogger()}
	ev := paidEvent(order)
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("settle upgrade: %v", err)
	}
	got, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanID != to.ID || got.Status != model.StatusActive {
		t.Fatalf("licence = plan %s status %s", got.PlanID, got.Status)
	}
	if got.ValidUntil == nil || !got.ValidUntil.Equal(end) {
		t.Fatalf("valid_until = %v, want the paid period end %v", got.ValidUntil, end)
	}
	sub, err := s.GetBillingSubscription(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.PlanID != to.ID {
		t.Fatalf("subscription plan = %s, want %s", sub.PlanID, to.ID)
	}
	stored, err := s.GetPaymentOrderForUpdate(ctx, nil, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.OrderPaid || stored.LicenseID != lic.ID {
		t.Fatalf("order = status %s licence %s", stored.Status, stored.LicenseID)
	}
	applied, err := s.FindUpgradeByPaymentOrderIn(ctx, s.DB, order.ID)
	if err != nil {
		t.Fatalf("upgrade row: %v", err)
	}
	if applied.State != model.UpgradeApplied || applied.AppliedAt == nil || applied.FromPlanID != from.ID {
		t.Fatalf("upgrade = %+v", applied)
	}
	events := deliveriesByEvent(t, s, wh.ID)
	if n := len(events[model.EventPlanChanged]); n != 1 {
		t.Fatalf("plan.changed deliveries = %d, want 1", n)
	}
	if reason := events[model.EventPlanChanged][0]["reason"]; reason != "paid_change" {
		t.Fatalf("plan.changed reason = %v, want paid_change", reason)
	}

	// The provider redelivers: success, but no second grant and no second
	// event.
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("duplicate settle: %v", err)
	}
	events = deliveriesByEvent(t, s, wh.ID)
	if n := len(events[model.EventPlanChanged]); n != 1 {
		t.Fatalf("after duplicate: plan.changed = %d, want 1", n)
	}
	if n := len(events[model.EventOrderPaid]); n != 1 {
		t.Fatalf("after duplicate: order.paid = %d, want 1", n)
	}
}

// An upgrade order naming a plan from another product must not move the
// licence; the money is recorded unmatched and the error is the store's
// sentinel so the caller can tell it from a database failure.
func TestSettleUpgradeWrongProductFails(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, _, _, lic, end := seedUpgradeLicence(t, s, model.StatusActive, "")
	suffix := uuid.NewString()[:13]
	otherProd := &model.Product{Name: "Other Co", Slug: "other-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, otherProd); err != nil {
		t.Fatal(err)
	}
	foreign := &model.Plan{ProductID: otherProd.ID, Name: "Foreign", Slug: "foreign-" + suffix,
		LicenseType: "subscription", BillingInterval: "month", LicenseModel: "standard", Active: true}
	if err := s.CreatePlan(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.DB.NewRaw("DELETE FROM plans WHERE id = ?", foreign.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", otherProd.ID).Exec(ctx)
	})
	order := upgradeOrder(t, s, prod, foreign, lic, end)
	up := &model.LicenseUpgrade{LicenseID: lic.ID, PaymentOrderID: order.ID,
		ToPlanID: foreign.ID, FromQuantity: 1}
	if err := s.CreateUpgradeForOrderIn(ctx, s.DB, up); err != nil {
		t.Fatal(err)
	}
	svc := &settlement.Service{Store: s, Log: testLogger()}
	err := svc.Settle(ctx, order.ID, "sepay", paidEvent(order))
	if !errors.Is(err, store.ErrUpgradeIneligible) {
		t.Fatalf("err = %v, want ErrUpgradeIneligible", err)
	}
	got, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanID == foreign.ID {
		t.Fatalf("licence moved to the foreign plan: %s", got.PlanID)
	}
	stored, err := s.GetPaymentOrderForUpdate(ctx, nil, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status == domain.OrderPaid {
		t.Fatal("order paid despite the failed grant")
	}
	if stored.LastSettlementError == "" {
		t.Fatal("no settlement error recorded for the operator")
	}
	var unmatched int
	if err := s.DB.NewRaw("SELECT count(*) FROM payment_transactions WHERE order_id = ? AND status = ?",
		order.ID, domain.TxUnmatched).Scan(ctx, &unmatched); err != nil {
		t.Fatal(err)
	}
	if unmatched != 1 {
		t.Fatalf("unmatched transactions = %d, want 1", unmatched)
	}
}

// A paid trial-conversion order converts the trial licence in place: no
// second licence, the subscription becomes the paid one, and the first
// paid period starts at trial_end.
func TestSettleTrialConversionConvertsInPlace(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:13]
	prod := &model.Product{Name: "Trial Co", Slug: "trial-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	trialPlan := &model.Plan{ProductID: prod.ID, Name: "Trial", Slug: "trial-" + suffix,
		LicenseType: "trial", BillingInterval: "month", TrialDays: 14, LicenseModel: "standard", Active: true}
	if err := s.CreatePlan(ctx, trialPlan); err != nil {
		t.Fatal(err)
	}
	paidPlan := &model.Plan{ProductID: prod.ID, Name: "Monthly", Slug: "monthly-" + suffix,
		LicenseType: "subscription", BillingInterval: "month", LicenseModel: "standard", Active: true}
	if err := s.CreatePlan(ctx, paidPlan); err != nil {
		t.Fatal(err)
	}
	trialEnd := time.Now().UTC().Truncate(time.Second).Add(5 * 24 * time.Hour)
	email := "trial-" + suffix + "@example.com"
	lic := &model.License{ProductID: prod.ID, PlanID: trialPlan.ID, Email: email,
		LicenseKey: "KEY-TRIAL-" + suffix, Status: model.StatusTrialing, Quantity: 1,
		ValidUntil: &trialEnd, PaymentProvider: "sepay"}
	if err := s.CreateLicenseWithSubscription(ctx, lic, trialPlan); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw(`UPDATE subscriptions
		SET payment_provider = 'sepay', status = 'trialing', current_period_start = ?,
		    current_period_end = ?, trial_start = ?, trial_end = ?
		WHERE license_id = ?`, trialEnd.Add(-14*24*time.Hour), trialEnd,
		trialEnd.Add(-14*24*time.Hour), trialEnd, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.DB.NewRaw("DELETE FROM payment_events WHERE order_id IN (SELECT id FROM payment_orders WHERE product_id = ?)", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM payment_transactions WHERE order_id IN (SELECT id FROM payment_orders WHERE product_id = ?)", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM payment_orders WHERE product_id = ?", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM subscriptions WHERE license_id = ?", lic.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM licenses WHERE product_id = ?", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM plans WHERE id IN (?, ?)", trialPlan.ID, paidPlan.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", prod.ID).Exec(ctx)
	})
	metadata := []byte(`{"billing":{"source":"trial_conversion","trial_license_id":"` + lic.ID + `","trial_end":"` + trialEnd.Format(time.RFC3339) + `"}}`)
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: prod.ID, PlanID: paidPlan.ID, Email: email, Kind: "new", Quantity: 1,
		SubtotalMinor: 990_000, TotalMinor: 990_000, Currency: "VND", Provider: "sepay",
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	webhook, wh := webhookFixture(t, s, prod.ID, model.EventOrderPaid, model.EventPlanChanged)
	svc := &settlement.Service{Store: s, Webhooks: webhook, Log: testLogger()}
	ev := paidEvent(order)
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("settle trial conversion: %v", err)
	}
	var count int
	if err := s.DB.NewRaw("SELECT count(*) FROM licenses WHERE email = ?", email).Scan(ctx, &count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("licences = %d, want the one trial licence converted", count)
	}
	got, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantEnd := trialEnd.Add(30 * 24 * time.Hour)
	if got.PlanID != paidPlan.ID || got.Status != model.StatusActive || got.Quantity != 1 {
		t.Fatalf("licence = plan %s status %s quantity %d", got.PlanID, got.Status, got.Quantity)
	}
	if got.ValidUntil == nil || !got.ValidUntil.Equal(wantEnd) {
		t.Fatalf("valid_until = %v, want %v", got.ValidUntil, wantEnd)
	}
	sub, err := s.GetBillingSubscription(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.PlanID != paidPlan.ID || sub.Status != model.StatusActive ||
		sub.CurrentPeriodStart == nil || !sub.CurrentPeriodStart.Equal(trialEnd) ||
		sub.CurrentPeriodEnd == nil || !sub.CurrentPeriodEnd.Equal(wantEnd) ||
		sub.TrialEnd != nil || sub.PaymentProvider != "sepay" {
		t.Fatalf("subscription = %+v", sub)
	}
	stored, err := s.GetPaymentOrderForUpdate(ctx, nil, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.OrderPaid || stored.LicenseID != lic.ID {
		t.Fatalf("order = status %s licence %s", stored.Status, stored.LicenseID)
	}
	events := deliveriesByEvent(t, s, wh.ID)
	if n := len(events[model.EventPlanChanged]); n != 1 {
		t.Fatalf("plan.changed = %d, want 1", n)
	}
	if reason := events[model.EventPlanChanged][0]["reason"]; reason != "trial_conversion" {
		t.Fatalf("plan.changed reason = %v", reason)
	}
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("duplicate settle: %v", err)
	}
	if n := len(deliveriesByEvent(t, s, wh.ID)[model.EventPlanChanged]); n != 1 {
		t.Fatalf("after duplicate: plan.changed = %d", n)
	}
}

// Paying a renewal for a licence the billing engine suspended reinstates
// it and extends it in the same settlement.
func TestSettleRenewalReinstatesSuspendedLicence(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, from, _, lic, end := seedUpgradeLicence(t, s, model.StatusSuspended, store.BillingSuspendedBy)
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: prod.ID, PlanID: from.ID, LicenseID: lic.ID, Email: lic.Email,
		Kind: "renewal", Quantity: 1, SubtotalMinor: 1_000_000, TotalMinor: 1_000_000,
		Currency: "VND", Provider: "sepay", ExpiresAt: time.Now().Add(time.Hour),
		Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := &settlement.Service{Store: s, Log: testLogger()}
	if err := svc.Settle(ctx, order.ID, "sepay", paidEvent(order)); err != nil {
		t.Fatalf("settle suspended renewal: %v", err)
	}
	got, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantEnd := end.Add(30 * 24 * time.Hour)
	if got.Status != model.StatusActive {
		t.Fatalf("status = %s, want active", got.Status)
	}
	if got.PastDueAt != nil || got.SuspendedAt != nil || got.SuspendedBy != "" {
		t.Fatalf("dunning not cleared: past_due=%v suspended=%v/%q", got.PastDueAt, got.SuspendedAt, got.SuspendedBy)
	}
	if got.ValidUntil == nil || !got.ValidUntil.Equal(wantEnd) {
		t.Fatalf("valid_until = %v, want %v", got.ValidUntil, wantEnd)
	}
}

// A next_cycle plan change waits for the renewal and is applied there, in
// the same transaction as the extension, with plan.changed emitted once.
func TestSettleRenewalAppliesNextCycleUpgrade(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, from, to, lic, end := seedUpgradeLicence(t, s, model.StatusActive, "")
	up := &model.LicenseUpgrade{ID: store.NewLicenseUpgradeID(), LicenseID: lic.ID,
		ToPlanID: to.ID, FromPlanID: from.ID, FromQuantity: 1}
	up.StripeCheckoutSessionID = store.NextCycleUpgradeSessionPrefix + up.ID
	if err := store.CreateLicenseUpgradeIn(ctx, s.DB, up); err != nil {
		t.Fatal(err)
	}
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: prod.ID, PlanID: from.ID, LicenseID: lic.ID, Email: lic.Email,
		Kind: "renewal", Quantity: 1, SubtotalMinor: 1_000_000, TotalMinor: 1_000_000,
		Currency: "VND", Provider: "sepay", ExpiresAt: time.Now().Add(time.Hour),
		Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	webhook, wh := webhookFixture(t, s, prod.ID, model.EventOrderPaid, model.EventPlanChanged)
	svc := &settlement.Service{Store: s, Webhooks: webhook, Log: testLogger()}
	if err := svc.Settle(ctx, order.ID, "sepay", paidEvent(order)); err != nil {
		t.Fatalf("settle next-cycle renewal: %v", err)
	}
	got, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantEnd := end.Add(30 * 24 * time.Hour)
	if got.PlanID != to.ID {
		t.Fatalf("licence plan = %s, want the scheduled %s", got.PlanID, to.ID)
	}
	if got.ValidUntil == nil || !got.ValidUntil.Equal(wantEnd) {
		t.Fatalf("valid_until = %v, want %v", got.ValidUntil, wantEnd)
	}
	sub, err := s.GetBillingSubscription(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.PlanID != to.ID || sub.Status != model.StatusActive {
		t.Fatalf("subscription = plan %s status %s", sub.PlanID, sub.Status)
	}
	applied, err := s.FindLicenseUpgradeByID(ctx, up.ID)
	if err != nil {
		t.Fatal(err)
	}
	if applied.State != model.UpgradeApplied || applied.AppliedAt == nil {
		t.Fatalf("next-cycle upgrade = %+v", applied)
	}
	events := deliveriesByEvent(t, s, wh.ID)
	if n := len(events[model.EventPlanChanged]); n != 1 {
		t.Fatalf("plan.changed = %d, want 1", n)
	}
	if reason := events[model.EventPlanChanged][0]["reason"]; reason != "next_cycle_upgrade" {
		t.Fatalf("reason = %v, want next_cycle_upgrade", reason)
	}
}
