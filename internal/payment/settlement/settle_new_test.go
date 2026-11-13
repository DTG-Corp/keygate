package settlement_test

import (
	"context"
	"io"
	"log/slog"
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

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedSellable creates a product and a perpetual plan that sells it, and
// removes them — and everything the test hung on them — when it ends.
// Integration tests share one database, so every slug and email carries
// a UUID rather than a timestamp.
func seedSellable(t *testing.T, s *store.Store) (productID, planID string) {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:13]
	p := &model.Product{Name: "SettleCo", Slug: "settle-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, p); err != nil {
		t.Fatal(err)
	}
	pl := &model.Plan{ProductID: p.ID, Name: "Pro", Slug: "settle-pro-" + suffix,
		LicenseType: "perpetual", LicenseModel: "standard", MaxActivations: 3, Active: true}
	if err := s.CreatePlan(ctx, pl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.DB.NewRaw("DELETE FROM payment_transactions WHERE order_id IN (SELECT id FROM payment_orders WHERE product_id = ?)", p.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM payment_events WHERE order_id IN (SELECT id FROM payment_orders WHERE product_id = ?)", p.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM payment_orders WHERE product_id = ?", p.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM licenses WHERE product_id = ?", p.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM plans WHERE id = ?", pl.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", p.ID).Exec(ctx)
	})
	return p.ID, pl.ID
}

func TestSettleNewOrderGrantsLicenseOnce(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedSellable(t, s)
	email := "new-" + uuid.NewString()[:8] + "@example.com"
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: productID, PlanID: planID, Email: email, Kind: "new", Quantity: 1,
		SubtotalMinor: 1190000, TotalMinor: 1190000, Currency: "VND", Provider: "sepay",
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := &settlement.Service{Store: s, Log: testLogger()}
	ev := provider.WebhookEvent{Provider: "sepay", ProviderEventID: "92704", ProviderRef: "92704",
		OrderCode: order.Code, AmountMinor: 1190000, Currency: "VND", Status: domain.OrderPaid,
		TransferType: "in", OccurredAt: time.Now(), Raw: []byte(`{"id":92704}`)}
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OrderPaid || got.PaidAt == nil {
		t.Fatalf("order = %+v", got)
	}
	var lic model.License
	if err := s.DB.NewSelect().Model(&lic).Where("email = ?", email).Scan(ctx); err != nil {
		t.Fatalf("license: %v", err)
	}
	if lic.Status != model.StatusActive || lic.PlanID != planID || lic.LicenseKey == "" {
		t.Fatalf("license = %+v", lic)
	}
	if got.LicenseID != lic.ID {
		t.Fatalf("order.license_id = %q, want %q", got.LicenseID, lic.ID)
	}
	// The money is on the ledger as matched, and the settlement error an
	// earlier attempt may have left is gone.
	var matched int
	if err := s.DB.NewRaw("SELECT count(*) FROM payment_transactions WHERE order_id = ? AND status = ?",
		order.ID, domain.TxMatched).Scan(ctx, &matched); err != nil {
		t.Fatal(err)
	}
	if matched != 1 {
		t.Fatalf("matched transactions = %d, want 1", matched)
	}
	if got.LastSettlementError != "" {
		t.Fatalf("last_settlement_error = %q", got.LastSettlementError)
	}

	// A second delivery (retry) is a no-op: still one licence.
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("second settle: %v", err)
	}
	n, err := s.DB.NewSelect().Table("licenses").Where("email = ?", email).Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("licences = %d", n)
	}
}

// A new subscription order writes the first period the order reserved,
// with the provider and the external reference recorded on the
// subscription row — the fields a later renewal and the billing engine
// read. The licence's id is the order's: the derivation that makes a
// retry land on the row a crash already wrote.
func TestSettleNewOrderSubscriptionPeriod(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, plan := seedPricedPlan(t, s, "subscription", "month", 0)
	order := newOrder(t, s, prod, plan, "new", "sub-"+uuid.NewString()[:8]+"@example.com")
	start := time.Now().UTC().Truncate(time.Second)
	end := start.AddDate(0, 1, 0)
	if _, err := s.DB.NewRaw("UPDATE payment_orders SET period_start = ?, period_end = ? WHERE id = ?",
		start, end, order.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	svc := &settlement.Service{Store: s, Log: testLogger()}
	ev := provider.WebhookEvent{Provider: "sepay", ProviderEventID: "sub-1", ProviderRef: "sub-1",
		OrderCode: order.Code, AmountMinor: 1190000, Currency: "VND", Status: domain.OrderPaid,
		TransferType: "in", OccurredAt: time.Now(), Raw: []byte(`{"id":"sub-1"}`)}
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("settle: %v", err)
	}
	lic, err := s.FindLicenseByID(ctx, order.ID)
	if err != nil {
		t.Fatalf("licence: %v", err)
	}
	if lic.ValidUntil == nil || !lic.ValidUntil.Equal(end) {
		t.Fatalf("valid_until = %v, want the reserved %v", lic.ValidUntil, end)
	}
	if lic.PaymentProvider != "sepay" {
		t.Fatalf("payment_provider = %q, want sepay", lic.PaymentProvider)
	}
	var sub model.Subscription
	if err := s.DB.NewSelect().Model(&sub).Where("license_id = ?", lic.ID).Scan(ctx); err != nil {
		t.Fatalf("subscription: %v", err)
	}
	if sub.PaymentProvider != "sepay" || sub.ExternalID != order.Code {
		t.Fatalf("subscription provider/external = %q/%q, want sepay/%q",
			sub.PaymentProvider, sub.ExternalID, order.Code)
	}
	if sub.CurrentPeriodStart == nil || !sub.CurrentPeriodStart.Equal(start) {
		t.Fatalf("period start = %v, want %v", sub.CurrentPeriodStart, start)
	}
	if sub.CurrentPeriodEnd == nil || !sub.CurrentPeriodEnd.Equal(end) {
		t.Fatalf("period end = %v, want %v", sub.CurrentPeriodEnd, end)
	}
}

// A paid trial plan grants a trialing licence, not an active one: the
// status decides whether the expiry sweep treats it as a paying
// customer. Its first period is the trial.
func TestSettleNewOrderTrialPlanIsTrialing(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, plan := seedPricedPlan(t, s, "trial", "", 14)
	order := newOrder(t, s, prod, plan, "new", "trial-"+uuid.NewString()[:8]+"@example.com")
	before := time.Now()

	svc := &settlement.Service{Store: s, Log: testLogger()}
	ev := provider.WebhookEvent{Provider: "sepay", ProviderEventID: "trial-1", ProviderRef: "trial-1",
		OrderCode: order.Code, AmountMinor: 1190000, Currency: "VND", Status: domain.OrderPaid,
		TransferType: "in", OccurredAt: time.Now(), Raw: []byte(`{"id":"trial-1"}`)}
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("settle: %v", err)
	}
	lic, err := s.FindLicenseByID(ctx, order.ID)
	if err != nil {
		t.Fatalf("licence: %v", err)
	}
	if lic.Status != model.StatusTrialing {
		t.Fatalf("licence status = %q, want trialing", lic.Status)
	}
	wantEnd := before.Add(14 * 24 * time.Hour)
	if lic.ValidUntil == nil || lic.ValidUntil.Before(wantEnd.Add(-time.Minute)) || lic.ValidUntil.After(wantEnd.Add(time.Minute)) {
		t.Fatalf("valid_until = %v, want about %v", lic.ValidUntil, wantEnd)
	}
	var subStatus string
	if err := s.DB.NewRaw("SELECT status FROM subscriptions WHERE license_id = ?", lic.ID).Scan(ctx, &subStatus); err != nil {
		t.Fatal(err)
	}
	if subStatus != model.StatusTrialing {
		t.Fatalf("subscription status = %q, want trialing", subStatus)
	}
}

// A licence already committed for this order (the crash window the old
// non-atomic write left behind) is reused, not duplicated: the retry
// finishes the order and the money without minting a second licence.
func TestSettleNewOrderReusesCommittedLicence(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, plan := seedPricedPlan(t, s, "subscription", "month", 0)
	email := "orphan-" + uuid.NewString()[:8] + "@example.com"
	order := newOrder(t, s, prod, plan, "new", email)

	key, err := s.NewLicenseKey(ctx, prod.ID)
	if err != nil {
		t.Fatal(err)
	}
	lic := &model.License{
		ID: order.ID, ProductID: prod.ID, PlanID: plan.ID, Email: email,
		LicenseKey: key, Status: model.StatusActive, Quantity: 1,
		PaymentProvider: "sepay",
	}
	if err := s.CreateLicenseWithSubscription(ctx, lic, plan); err != nil {
		t.Fatal(err)
	}

	svc := &settlement.Service{Store: s, Log: testLogger()}
	ev := provider.WebhookEvent{Provider: "sepay", ProviderEventID: "orphan-1", ProviderRef: "orphan-1",
		OrderCode: order.Code, AmountMinor: 1190000, Currency: "VND", Status: domain.OrderPaid,
		TransferType: "in", OccurredAt: time.Now(), Raw: []byte(`{"id":"orphan-1"}`)}
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if again := svc.Settle(ctx, order.ID, "sepay", ev); again != nil {
		t.Fatalf("second settle: %v", again)
	}
	paid, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if paid.Status != domain.OrderPaid || paid.LicenseID != order.ID {
		t.Fatalf("order = %+v, want paid with licence %s", paid, order.ID)
	}
	var licences, subs int
	if err := s.DB.NewRaw("SELECT count(*) FROM licenses WHERE email = ?", email).Scan(ctx, &licences); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.NewRaw("SELECT count(*) FROM subscriptions WHERE license_id = ?", order.ID).Scan(ctx, &subs); err != nil {
		t.Fatal(err)
	}
	if licences != 1 || subs != 1 {
		t.Fatalf("licences = %d, subscriptions = %d, want 1/1", licences, subs)
	}
	var matched int
	if err := s.DB.NewRaw("SELECT count(*) FROM payment_transactions WHERE order_id = ? AND status = ?",
		order.ID, domain.TxMatched).Scan(ctx, &matched); err != nil {
		t.Fatal(err)
	}
	if matched != 1 {
		t.Fatalf("matched transactions = %d, want 1", matched)
	}
}
