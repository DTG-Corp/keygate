package settlement_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/payment/provider"
	"github.com/dtg-corp/dsoft/internal/payment/settlement"
	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/internal/testsupport/testdb"
)

// A paid renewal extends the licence it names, writes the ledger row that
// makes the order's replay a no-op, and moves the subscription period
// with it. It never mints a second licence.
func TestSettleRenewalExtendsLicense(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:13]
	prod := &model.Product{Name: "RenewCo", Slug: "renew-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Monthly", Slug: "monthly-" + suffix,
		LicenseType: "subscription", BillingInterval: "month", LicenseModel: "standard", Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	oldEnd := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	email := "renew-" + suffix + "@example.com"
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: email,
		LicenseKey: "KEY-RENEW-" + suffix, Status: model.StatusActive, Quantity: 1,
		ValidUntil: &oldEnd}
	if err := s.CreateLicenseWithSubscription(ctx, lic, plan); err != nil {
		t.Fatal(err)
	}
	// CreatePaymentOrder places the renewal against the licence it
	// extends (LicenseID) and reserves no period: the extension starts
	// from the licence's current end.
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: prod.ID, PlanID: plan.ID, LicenseID: lic.ID, Email: email, Kind: "renewal",
		Quantity: 1, SubtotalMinor: 1190000, TotalMinor: 1190000, Currency: "VND",
		Provider: "sepay", ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.DB.NewRaw("DELETE FROM payment_transactions WHERE order_id IN (SELECT id FROM payment_orders WHERE product_id = ?)", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM payment_orders WHERE product_id = ?", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM licenses WHERE product_id = ?", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM plans WHERE id = ?", plan.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", prod.ID).Exec(ctx)
	})

	svc := &settlement.Service{Store: s, Log: testLogger()}
	ev := provider.WebhookEvent{Provider: "sepay", ProviderEventID: "r1", ProviderRef: "r1",
		OrderCode: order.Code, AmountMinor: 1190000, Currency: "VND", Status: domain.OrderPaid,
		TransferType: "in", OccurredAt: time.Now(), Raw: []byte(`{"id":"r1"}`)}
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("settle renewal: %v", err)
	}
	got, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ValidUntil == nil || !got.ValidUntil.After(oldEnd) {
		t.Fatalf("valid_until = %v, want after %v", got.ValidUntil, oldEnd)
	}
	// The renewal ledger carries the order id.
	ledger, err := s.FindLicenseRenewalByPaymentOrder(ctx, order.ID)
	if err != nil {
		t.Fatalf("renewal ledger: %v", err)
	}
	if ledger.LicenseID != lic.ID || ledger.Days != 30 {
		t.Fatalf("ledger = %+v", ledger)
	}
	// The subscription row follows the licence.
	var periodEnd time.Time
	if err := s.DB.NewRaw("SELECT current_period_end FROM subscriptions WHERE license_id = ?", lic.ID).
		Scan(ctx, &periodEnd); err != nil {
		t.Fatal(err)
	}
	if !periodEnd.Equal(*got.ValidUntil) {
		t.Fatalf("subscription period end = %v, want %v", periodEnd, got.ValidUntil)
	}
	// No second licence was minted for the renewal order.
	licences, err := s.DB.NewSelect().Table("licenses").Where("email = ?", email).Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if licences != 1 {
		t.Fatalf("licences = %d, want 1", licences)
	}

	// A redelivery of the same paid event changes nothing: the order is
	// already paid and the ledger already holds the order.
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("second settle: %v", err)
	}
	again, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.ValidUntil == nil || !again.ValidUntil.Equal(*got.ValidUntil) {
		t.Fatalf("second settle moved valid_until to %v, want %v", again.ValidUntil, got.ValidUntil)
	}
}

// A renewal order that reserved a period is extended by exactly that
// period, and the ledger records the same number of days — not the
// plan's monthly interval. A 365-day reserved period on a monthly plan
// buys a year.
func TestSettleRenewalUsesReservedPeriod(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, plan := seedPricedPlan(t, s, "subscription", "month", 0)
	key, err := s.NewLicenseKey(ctx, prod.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldEnd := time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Second)
	email := "reserved-" + uuid.NewString()[:8] + "@example.com"
	lic := &model.License{
		ProductID: prod.ID, PlanID: plan.ID, Email: email, LicenseKey: key,
		Status: model.StatusActive, Quantity: 1, ValidUntil: &oldEnd,
	}
	if err := s.CreateLicenseWithSubscription(ctx, lic, plan); err != nil {
		t.Fatal(err)
	}
	periodStart := time.Now().UTC().Truncate(time.Second)
	periodEnd := periodStart.Add(365 * 24 * time.Hour)
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: prod.ID, PlanID: plan.ID, LicenseID: lic.ID, Email: email, Kind: "renewal",
		Quantity: 1, SubtotalMinor: 1190000, TotalMinor: 1190000, Currency: "VND", Provider: "sepay",
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
		PeriodEnd: &periodEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE payment_orders SET period_start = ? WHERE id = ?", periodStart, order.ID).
		Exec(ctx); err != nil {
		t.Fatal(err)
	}

	svc := &settlement.Service{Store: s, Log: testLogger()}
	ev := provider.WebhookEvent{Provider: "sepay", ProviderEventID: "reserved-1", ProviderRef: "reserved-1",
		OrderCode: order.Code, AmountMinor: 1190000, Currency: "VND", Status: domain.OrderPaid,
		TransferType: "in", OccurredAt: time.Now(), Raw: []byte(`{"id":"reserved-1"}`)}
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantEnd := oldEnd.Add(365 * 24 * time.Hour)
	if got.ValidUntil == nil || !got.ValidUntil.Equal(wantEnd) {
		t.Fatalf("valid_until = %v, want %v", got.ValidUntil, wantEnd)
	}
	ledger, err := s.FindLicenseRenewalByPaymentOrder(ctx, order.ID)
	if err != nil {
		t.Fatalf("renewal ledger: %v", err)
	}
	if ledger.Days != 365 {
		t.Fatalf("ledger days = %d, want 365", ledger.Days)
	}
	var periodStartOut, periodEndOut time.Time
	if err := s.DB.NewRaw("SELECT current_period_start, current_period_end FROM subscriptions WHERE license_id = ?", lic.ID).
		Scan(ctx, &periodStartOut, &periodEndOut); err != nil {
		t.Fatal(err)
	}
	if !periodEndOut.Equal(wantEnd) || !periodStartOut.Equal(oldEnd) {
		t.Fatalf("subscription period = %v..%v, want %v..%v",
			periodStartOut, periodEndOut, oldEnd, wantEnd)
	}
}

// A renewal paid while the subscription is past due revives it: the
// licence is active again with past_due_at cleared, and the subscription
// row follows, so dunning stops and access returns.
func TestSettlePastDueRenewalReactivates(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, plan := seedPricedPlan(t, s, "subscription", "month", 0)
	key, err := s.NewLicenseKey(ctx, prod.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldEnd := time.Now().Add(5 * 24 * time.Hour).UTC().Truncate(time.Second)
	email := "pastdue-" + uuid.NewString()[:8] + "@example.com"
	lic := &model.License{
		ProductID: prod.ID, PlanID: plan.ID, Email: email, LicenseKey: key,
		Status: model.StatusActive, Quantity: 1, ValidUntil: &oldEnd,
	}
	if err := s.CreateLicenseWithSubscription(ctx, lic, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE licenses SET status = ?, past_due_at = now() WHERE id = ?",
		model.StatusPastDue, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: prod.ID, PlanID: plan.ID, LicenseID: lic.ID, Email: email, Kind: "renewal",
		Quantity: 1, SubtotalMinor: 1190000, TotalMinor: 1190000, Currency: "VND", Provider: "sepay",
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	svc := &settlement.Service{Store: s, Log: testLogger()}
	ev := provider.WebhookEvent{Provider: "sepay", ProviderEventID: "pastdue-1", ProviderRef: "pastdue-1",
		OrderCode: order.Code, AmountMinor: 1190000, Currency: "VND", Status: domain.OrderPaid,
		TransferType: "in", OccurredAt: time.Now(), Raw: []byte(`{"id":"pastdue-1"}`)}
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusActive {
		t.Fatalf("licence status = %q, want active", got.Status)
	}
	if got.PastDueAt != nil {
		t.Fatalf("past_due_at = %v, want cleared", got.PastDueAt)
	}
	wantEnd := oldEnd.Add(30 * 24 * time.Hour)
	if got.ValidUntil == nil || !got.ValidUntil.Equal(wantEnd) {
		t.Fatalf("valid_until = %v, want %v", got.ValidUntil, wantEnd)
	}
	var subStatus string
	var periodEnd time.Time
	if err := s.DB.NewRaw("SELECT status, current_period_end FROM subscriptions WHERE license_id = ?", lic.ID).
		Scan(ctx, &subStatus, &periodEnd); err != nil {
		t.Fatal(err)
	}
	if subStatus != model.StatusActive || !periodEnd.Equal(wantEnd) {
		t.Fatalf("subscription = %q ending %v, want active ending %v", subStatus, periodEnd, wantEnd)
	}
	ledger, err := s.FindLicenseRenewalByPaymentOrder(ctx, order.ID)
	if err != nil {
		t.Fatalf("renewal ledger: %v", err)
	}
	if ledger.Days != 30 {
		t.Fatalf("ledger days = %d, want 30", ledger.Days)
	}
}
