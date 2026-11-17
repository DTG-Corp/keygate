package checkout_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/checkout"
	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/payment/provider/fake"
	"github.com/dtg-corp/dsoft/internal/store"
)

// subscriptionPlan turns the fixture's plan into a monthly subscription
// plan the renewal flow can bill. Price the plan *after* calling this:
// UpsertPlanPrice copies the plan's billing interval onto the price row,
// and a renewal order resolves its price by that interval.
func (f *fixture) subscriptionPlan() *model.Plan {
	f.t.Helper()
	plan, err := f.s.FindPlanByID(f.ctx, f.plan.ID)
	if err != nil {
		f.t.Fatalf("reload plan: %v", err)
	}
	plan.LicenseType = "subscription"
	plan.BillingInterval = "month"
	if err := f.s.UpdatePlan(f.ctx, plan, "license_type", "billing_interval"); err != nil {
		f.t.Fatalf("update plan: %v", err)
	}
	f.plan = plan
	return plan
}

// seedLicence creates a subscription licence for the fixture plan with a
// pinned billing period, the way settlement would leave it after a VN
// payment: provider sepay, the order code as external id, a live period.
func (f *fixture) seedLicence(email string, quantity int, periodStart, periodEnd time.Time) *model.License {
	f.t.Helper()
	lic := &model.License{
		ProductID: f.product.ID, PlanID: f.plan.ID, Email: email,
		LicenseKey: "KEY-" + uuid.NewString()[:13], Status: model.StatusActive, Quantity: quantity,
	}
	if err := f.s.CreateLicenseWithSubscription(f.ctx, lic, f.plan); err != nil {
		f.t.Fatalf("create licence: %v", err)
	}
	if _, err := f.s.DB.NewRaw(`UPDATE subscriptions
		SET payment_provider = 'sepay', external_id = ?, status = 'active',
		    current_period_start = ?, current_period_end = ?, cancel_at_period_end = false
		WHERE license_id = ?`, "SUB-"+uuid.NewString()[:13], periodStart, periodEnd, lic.ID).Exec(f.ctx); err != nil {
		f.t.Fatalf("pin period: %v", err)
	}
	if _, err := f.s.DB.NewRaw(`UPDATE licenses SET valid_until = ?, status = 'active' WHERE id = ?`,
		periodEnd, lic.ID).Exec(f.ctx); err != nil {
		f.t.Fatalf("pin licence: %v", err)
	}
	f.t.Cleanup(func() {
		ctx := context.Background()
		_, _ = f.s.DB.NewRaw("DELETE FROM email_queue WHERE to_addr = ?", email).Exec(ctx)
		_, _ = f.s.DB.NewRaw("DELETE FROM subscriptions WHERE license_id = ?", lic.ID).Exec(ctx)
		_, _ = f.s.DB.NewRaw("DELETE FROM licenses WHERE id = ?", lic.ID).Exec(ctx)
	})
	return lic
}

// pendingOrdersFor counts the orders still waiting for money on a licence.
func (f *fixture) pendingOrdersFor(licenseID string) []*model.PaymentOrder {
	f.t.Helper()
	var out []*model.PaymentOrder
	if err := f.s.DB.NewSelect().Model(&out).
		Where("license_id = ?", licenseID).Where("status = ?", domain.OrderPending).
		Scan(f.ctx); err != nil {
		f.t.Fatalf("list orders: %v", err)
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func TestRenewalOrderUsesLicencePlanEmailAndPeriod(t *testing.T) {
	f := newFixture(t, true)
	f.subscriptionPlan()
	f.price("sepay", 500_000)
	periodEnd := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)
	periodStart := periodEnd.AddDate(0, -1, 0)
	lic := f.seedLicence("renew-"+f.suffix+"@example.com", 2, periodStart, periodEnd)
	fp := fake.New("sepay")
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fp).providerFactory()}

	res, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		Kind: checkout.KindRenewal, LicenseID: lic.ID,
		PeriodStart: &periodStart, PeriodEnd: &periodEnd,
	})
	if err != nil {
		t.Fatalf("create renewal: %v", err)
	}
	if !res.Created {
		t.Fatal("renewal order was not reported as created")
	}
	o := res.Order
	if o.Kind != checkout.KindRenewal {
		t.Fatalf("kind = %q, want renewal", o.Kind)
	}
	if o.LicenseID != lic.ID || o.PlanID != f.plan.ID || o.ProductID != f.product.ID {
		t.Fatalf("order = %+v", o)
	}
	if o.Email != lic.Email {
		t.Fatalf("email = %q, want the licence's %q", o.Email, lic.Email)
	}
	if o.Quantity != 2 {
		t.Fatalf("quantity = %d, want the licence's 2", o.Quantity)
	}
	if o.PeriodStart == nil || !o.PeriodStart.Equal(periodStart) ||
		o.PeriodEnd == nil || !o.PeriodEnd.Equal(periodEnd) {
		t.Fatalf("period = %v..%v, want %v..%v", o.PeriodStart, o.PeriodEnd, periodStart, periodEnd)
	}
	if o.TotalMinor != 1_000_000 {
		t.Fatalf("total = %d, want 2 x 500000", o.TotalMinor)
	}
	if res.Attempt == nil || res.Attempt.OrderID != o.ID {
		t.Fatalf("attempt = %+v", res.Attempt)
	}
	if got := len(f.pendingOrdersFor(lic.ID)); got != 1 {
		t.Fatalf("pending orders = %d, want 1", got)
	}
}

func TestRenewalOrderIsResumedNotDuplicated(t *testing.T) {
	f := newFixture(t, true)
	f.subscriptionPlan()
	f.price("sepay", 300_000)
	periodEnd := time.Now().Add(5 * 24 * time.Hour).UTC()
	periodStart := periodEnd.AddDate(0, -1, 0)
	lic := f.seedLicence("resume-"+f.suffix+"@example.com", 1, periodStart, periodEnd)
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fake.New("sepay")).providerFactory()}

	in := checkout.CreateOrderInput{
		Kind: checkout.KindRenewal, LicenseID: lic.ID,
		PeriodStart: &periodStart, PeriodEnd: &periodEnd,
	}
	first, err := svc.CreateOrder(f.ctx, in)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := svc.CreateOrder(f.ctx, in)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if second.Created {
		t.Fatal("second create minted a second order")
	}
	if second.Order.ID != first.Order.ID {
		t.Fatalf("order ids differ: %s vs %s", second.Order.ID, first.Order.ID)
	}
	if got := len(f.pendingOrdersFor(lic.ID)); got != 1 {
		t.Fatalf("pending orders = %d, want 1", got)
	}
}

func TestRenewalRequiresLicenceOfSameProduct(t *testing.T) {
	f := newFixture(t, true)
	f.subscriptionPlan()
	f.price("sepay", 300_000)

	// A licence on another product's plan.
	other := newFixture(t, true)
	other.subscriptionPlan()
	other.price("sepay", 300_000)
	foreign := other.seedLicence("foreign-"+other.suffix+"@example.com", 1,
		time.Now(), time.Now().AddDate(0, 1, 0))

	svc := &checkout.Service{Store: f.s, Providers: newFactory(fake.New("sepay")).providerFactory()}
	_, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		Kind: checkout.KindRenewal, LicenseID: foreign.ID, PlanID: f.plan.ID,
	})
	if !errors.Is(err, checkout.ErrPlanNotSellable) {
		t.Fatalf("err = %v, want ErrPlanNotSellable", err)
	}
}

func TestRenewalRequiresExistingLicence(t *testing.T) {
	f := newFixture(t, true)
	f.subscriptionPlan()
	f.price("sepay", 300_000)
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fake.New("sepay")).providerFactory()}
	_, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		Kind: checkout.KindRenewal, LicenseID: "no-such-licence",
	})
	if !errors.Is(err, checkout.ErrPlanNotSellable) {
		t.Fatalf("err = %v, want ErrPlanNotSellable", err)
	}
}

func TestUnknownKindIsRejected(t *testing.T) {
	f := newFixture(t, true)
	f.subscriptionPlan()
	f.price("sepay", 300_000)
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fake.New("sepay")).providerFactory()}
	_, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		Kind: "gift", PlanID: f.plan.ID, Email: "buyer@example.com", Quantity: 1,
	})
	if !errors.Is(err, checkout.ErrKindInvalid) {
		t.Fatalf("err = %v, want ErrKindInvalid", err)
	}
}

func TestUpgradeRequiresDifferentPlan(t *testing.T) {
	f := newFixture(t, true)
	base := f.subscriptionPlan()
	f.price("sepay", 300_000)
	lic := f.seedLicence("upgrade-"+f.suffix+"@example.com", 1,
		time.Now(), time.Now().AddDate(0, 1, 0))
	fp := fake.New("sepay")
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fp).providerFactory()}

	// Same plan as the licence: nothing to upgrade to.
	if _, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		Kind: checkout.KindUpgrade, LicenseID: lic.ID, PlanID: base.ID,
	}); !errors.Is(err, checkout.ErrPlanNotSellable) {
		t.Fatalf("same-plan upgrade err = %v, want ErrPlanNotSellable", err)
	}

	// A pricier plan on the same product is the upgrade target.
	target := &model.Plan{
		ProductID: f.product.ID, Name: "Max", Slug: "checkout-max-" + f.suffix,
		LicenseType: "subscription", BillingInterval: "month", LicenseModel: "standard",
		MaxActivations: 5, Active: true,
	}
	if err := f.s.CreatePlan(f.ctx, target); err != nil {
		t.Fatalf("create target plan: %v", err)
	}
	if _, err := f.s.UpsertPlanPrice(f.ctx, target.ID, store.PlanPriceInput{
		Provider: "sepay", Currency: "VND", AmountMinor: 900_000,
	}); err != nil {
		t.Fatalf("price target: %v", err)
	}

	res, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		Kind: checkout.KindUpgrade, LicenseID: lic.ID, PlanID: target.ID,
	})
	if err != nil {
		t.Fatalf("create upgrade: %v", err)
	}
	if res.Order.Kind != checkout.KindUpgrade || res.Order.PlanID != target.ID || res.Order.LicenseID != lic.ID {
		t.Fatalf("upgrade order = %+v", res.Order)
	}
	if res.Order.Email != lic.Email || res.Order.Quantity != lic.Quantity {
		t.Fatalf("upgrade order = %+v", res.Order)
	}
}

func TestExpiresAtOverrideIsHonoured(t *testing.T) {
	f := newFixture(t, true)
	f.subscriptionPlan()
	f.price("sepay", 300_000)
	periodEnd := time.Now().Add(7 * 24 * time.Hour).UTC()
	lic := f.seedLicence("expiry-"+f.suffix+"@example.com", 1,
		periodEnd.AddDate(0, -1, 0), periodEnd)
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fake.New("sepay")).providerFactory()}

	past := time.Now().Add(-time.Hour)
	if _, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		Kind: checkout.KindRenewal, LicenseID: lic.ID, ExpiresAt: &past,
	}); !errors.Is(err, checkout.ErrPlanNotSellable) {
		t.Fatalf("past expiry err = %v, want ErrPlanNotSellable", err)
	}

	want := periodEnd.Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)
	res, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		Kind: checkout.KindRenewal, LicenseID: lic.ID, PeriodEnd: &periodEnd,
		ExpiresAt: &want,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !res.Order.ExpiresAt.UTC().Truncate(time.Second).Equal(want) {
		t.Fatalf("expires_at = %v, want %v", res.Order.ExpiresAt, want)
	}
}
