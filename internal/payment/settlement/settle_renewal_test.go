package settlement_test

import (
	"context"
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
