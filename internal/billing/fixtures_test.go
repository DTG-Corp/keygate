package billing_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dtg-corp/dsoft/internal/billing"
	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/checkout"
	"github.com/dtg-corp/dsoft/internal/payment/provider"
	"github.com/dtg-corp/dsoft/internal/payment/provider/fake"
	"github.com/dtg-corp/dsoft/internal/payment/providercfg"
	"github.com/dtg-corp/dsoft/internal/service"
	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/internal/testsupport/testdb"
)

// rig is one billing service wired to the real store and the real
// checkout service, with only the clock, the mailer and the webhook sink
// replaced. Using the real checkout is deliberate: order creation,
// pricing, dedupe and the provider instruction are what the engine is
// judged on, and a fake checkout would test the fake.
type rig struct {
	t        *testing.T
	s        *store.Store
	now      time.Time
	svc      *billing.Service
	checkout billing.OrderCreator
	prod     *fake.Provider
	mail     *fakeMail
	events   *fakeEvents
	webhook  billing.EventEmitter
}

// fixedNow is the instant every pass runs at unless a test moves it.
func fixedNow() time.Time { return time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC) }

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, s: testdb.Store(t), now: fixedNow(), mail: &fakeMail{}, events: &fakeEvents{}}
	r.useRealCheckout()
	return r
}

// useRealCheckout wires the real pricing/order path with a scripted
// provider, the same shape phase 1a's checkout tests use.
func (r *rig) useRealCheckout() {
	r.prod = fake.New("sepay")
	// The default fake ref is derived from the order code, so a resumed
	// order would reuse it and trip the attempts' unique index. A real
	// adapter mints a fresh reference per instruction.
	var refs atomic.Int64
	r.prod.CreateFn = func(ctx context.Context, req provider.CreateAttemptRequest) (provider.CreateAttemptResult, error) {
		return provider.CreateAttemptResult{
			ProviderRef: fmt.Sprintf("fake-ref-%s-%d", req.OrderCode, refs.Add(1)),
			QRPayload:   "fake-qr",
			ExpiresAt:   req.ExpiresAt,
		}, nil
	}
	webhooks := service.NewWebhookService(r.s, discardLogger(), time.Second, 0, true)
	svc := &checkout.Service{
		Store: r.s, BaseURL: "https://dsoft.test", Webhooks: webhooks,
		Providers: func(ctx context.Context, productID, name string) (provider.Provider, *providercfg.ResolvedConfig, error) {
			if name != r.prod.Name() {
				return nil, nil, checkout.ErrProviderUnavailable
			}
			return r.prod, &providercfg.ResolvedConfig{}, nil
		},
	}
	r.checkout = svc
	r.rebuild()
}

// useCheckout swaps the order creator, for tests that script failures.
func (r *rig) useCheckout(c billing.OrderCreator) {
	r.checkout = c
	r.rebuild()
}

func (r *rig) rebuild() {
	if r.webhook == nil {
		r.webhook = r.events
	}
	r.svc = billing.New(billing.Deps{
		Store: r.s, Checkout: r.checkout, Email: r.mail, Webhook: r.webhook,
		Logger: discardLogger(), BaseURL: "https://dsoft.test",
		Now: func() time.Time { return r.now },
	})
}

func (r *rig) run() error {
	r.t.Helper()
	return r.svc.RunOnce(context.Background())
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func ptr[T any](v T) *T { return &v }

// ─── seeds ───

type seeded struct {
	Product *model.Product
	Plan    *model.Plan
	Licence *model.License
	Sub     *model.Subscription
}

type seedOpts struct {
	Interval      string // default month
	LicenceType   string // default subscription
	GraceDays     int    // default 7
	RenewalDays   int
	TrialDays     int
	PriceProvider string // default sepay; the plan's price row
	Provider      string // default sepay; the subscription's provider
	LicenceStatus string // default active
	SubStatus     string // default active
	Start         time.Time
	End           time.Time
	Cancel        bool
	Price         int64 // 0 ⇒ no price row for the provider
	Quantity      int
	TrialEnd      *time.Time
}

// seed creates one product, plan and licence with a pinned subscription,
// the state the hourly scan reads.
func (r *rig) seed(opts seedOpts) *seeded {
	r.t.Helper()
	ctx := context.Background()
	if opts.Interval == "" {
		opts.Interval = "month"
	}
	if opts.LicenceType == "" {
		opts.LicenceType = "subscription"
	}
	if opts.Provider == "" {
		opts.Provider = "sepay"
	}
	if opts.LicenceStatus == "" {
		opts.LicenceStatus = model.StatusActive
	}
	if opts.SubStatus == "" {
		opts.SubStatus = model.StatusActive
	}
	if opts.Quantity == 0 {
		opts.Quantity = 1
	}
	if opts.End.IsZero() {
		opts.End = r.now.Add(3 * 24 * time.Hour)
	}
	if opts.Start.IsZero() {
		opts.Start = opts.End.AddDate(0, -1, 0)
	}
	suffix := uuid.NewString()[:13]
	prod := &model.Product{Name: "Acme " + suffix, Slug: "acme-" + suffix, Type: "desktop"}
	if err := r.s.CreateProduct(ctx, prod); err != nil {
		r.t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Pro", Slug: "pro-" + suffix,
		LicenseType: opts.LicenceType, BillingInterval: opts.Interval, LicenseModel: "standard",
		Active: true, GraceDays: opts.GraceDays, RenewalDays: opts.RenewalDays,
		TrialDays: opts.TrialDays, MaxActivations: 5,
	}
	if err := r.s.CreatePlan(ctx, plan); err != nil {
		r.t.Fatalf("create plan: %v", err)
	}
	if opts.Price > 0 {
		priceProvider := opts.PriceProvider
		if priceProvider == "" {
			priceProvider = "sepay"
		}
		if _, err := r.s.UpsertPlanPrice(ctx, plan.ID, store.PlanPriceInput{
			Provider: priceProvider, Currency: "VND", AmountMinor: opts.Price,
		}); err != nil {
			r.t.Fatalf("price plan: %v", err)
		}
	}
	lic := &model.License{
		ProductID: prod.ID, PlanID: plan.ID, Email: "billing-" + suffix + "@example.com",
		LicenseKey: "KEY-BILL-" + suffix, Status: opts.LicenceStatus, Quantity: opts.Quantity,
		ValidUntil: &opts.End,
	}
	if err := r.s.CreateLicenseWithSubscription(ctx, lic, plan); err != nil {
		r.t.Fatalf("create licence: %v", err)
	}
	if _, err := r.s.DB.NewRaw(`UPDATE subscriptions
		SET payment_provider = ?, external_id = ?, status = ?,
		    current_period_start = ?, current_period_end = ?, cancel_at_period_end = ?,
		    trial_start = ?, trial_end = ?
		WHERE license_id = ?`,
		opts.Provider, "SUB-"+suffix, opts.SubStatus, opts.Start, opts.End, opts.Cancel,
		opts.TrialEnd, opts.TrialEnd, lic.ID).Exec(ctx); err != nil {
		r.t.Fatalf("pin subscription: %v", err)
	}
	r.t.Cleanup(func() {
		ctx := context.Background()
		_, _ = r.s.DB.NewRaw("DELETE FROM email_queue WHERE to_addr = ?", lic.Email).Exec(ctx)
		_, _ = r.s.DB.NewRaw("DELETE FROM notifications WHERE license_id = ?", lic.ID).Exec(ctx)
		_, _ = r.s.DB.NewRaw("DELETE FROM payment_attempts WHERE order_id IN (SELECT id FROM payment_orders WHERE license_id = ?)", lic.ID).Exec(ctx)
		_, _ = r.s.DB.NewRaw("DELETE FROM payment_orders WHERE license_id = ?", lic.ID).Exec(ctx)
		_, _ = r.s.DB.NewRaw("DELETE FROM audit_logs WHERE entity_id = ?", lic.ID).Exec(ctx)
		_, _ = r.s.DB.NewRaw("DELETE FROM subscriptions WHERE license_id = ?", lic.ID).Exec(ctx)
		_, _ = r.s.DB.NewRaw("DELETE FROM licenses WHERE id = ?", lic.ID).Exec(ctx)
		_, _ = r.s.DB.NewRaw("DELETE FROM plan_prices WHERE plan_id = ?", plan.ID).Exec(ctx)
		_, _ = r.s.DB.NewRaw("DELETE FROM plans WHERE id = ?", plan.ID).Exec(ctx)
		_, _ = r.s.DB.NewRaw("DELETE FROM products WHERE id = ?", prod.ID).Exec(ctx)
	})
	sub, err := r.s.GetBillingSubscription(ctx, lic.ID)
	if err != nil {
		r.t.Fatalf("load subscription: %v", err)
	}
	if sub.License != nil {
		lic = sub.License
	}
	return &seeded{Product: prod, Plan: plan, Licence: lic, Sub: sub}
}

// ─── fakes ───

type fakeMail struct {
	mu          sync.Mutex
	notices     []mailCall
	overdue     []mailCall
	graces      []mailCall
	suspensions []mailCall
}

type mailCall struct {
	To, Product, Plan, PayURL string
	Days                      int
	PeriodEnd                 time.Time
}

func (f *fakeMail) RenderRenewalNotice(product, plan string, days int, payURL string, periodEnd time.Time) (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notices = append(f.notices, mailCall{"", product, plan, payURL, days, periodEnd})
	return "renewal reminder", "<html><body>reminder</body></html>"
}

func (f *fakeMail) RenderRenewalOverdue(product, plan, payURL string, periodEnd time.Time) (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.overdue = append(f.overdue, mailCall{"", product, plan, payURL, 0, periodEnd})
	return "payment due", "<html><body>due</body></html>"
}

func (f *fakeMail) RenderRenewalGrace(product, plan string, days int, payURL string, periodEnd time.Time) (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.graces = append(f.graces, mailCall{"", product, plan, payURL, days, periodEnd})
	return "grace", "<html><body>grace</body></html>"
}

func (f *fakeMail) RenderRenewalSuspension(product, plan, payURL string, periodEnd time.Time) (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspensions = append(f.suspensions, mailCall{"", product, plan, payURL, 0, periodEnd})
	return "suspended", "<html><body>suspended</body></html>"
}

func (f *fakeMail) sent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.notices) + len(f.overdue) + len(f.graces) + len(f.suspensions)
}

type fakeEvents struct {
	mu    sync.Mutex
	calls []eventCall
}

type eventCall struct {
	ProductID string
	Event     string
	Data      map[string]any
}

func (f *fakeEvents) Dispatch(ctx context.Context, productID, event string, data map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, eventCall{ProductID: productID, Event: event, Data: data})
}

func (f *fakeEvents) byEvent(event string) []eventCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []eventCall
	for _, c := range f.calls {
		if c.Event == event {
			out = append(out, c)
		}
	}
	return out
}

// fakeOrders is for scripted failures: it records the inputs and answers
// with the test's function.
type fakeOrders struct {
	inputs []checkout.CreateOrderInput
	fn     func(in checkout.CreateOrderInput) (*checkout.OrderResult, error)
}

func (f *fakeOrders) CreateOrder(ctx context.Context, in checkout.CreateOrderInput) (*checkout.OrderResult, error) {
	f.inputs = append(f.inputs, in)
	if f.fn != nil {
		return f.fn(in)
	}
	return &checkout.OrderResult{Order: &model.PaymentOrder{
		ID: store.NewID(), Code: "KGTEST", Kind: in.Kind, LicenseID: in.LicenseID,
		PlanID: in.PlanID, Email: in.Email, Quantity: in.Quantity, TotalMinor: 1000,
		PeriodStart: in.PeriodStart, PeriodEnd: in.PeriodEnd,
	}, Created: true}, nil
}

// useRealWebhooks sends events through the real webhook service, so the
// persisted payload is the one an integrator receives.
func (r *rig) useRealWebhooks() {
	r.webhook = service.NewWebhookService(r.s, discardLogger(), 2*time.Second, 0, true)
	r.rebuild()
}

// createWebhook subscribes a product to events and removes the rows after.
func (r *rig) createWebhook(productID string, events ...string) *model.Webhook {
	r.t.Helper()
	wh := &model.Webhook{ProductID: productID, URL: "https://example.invalid/hook", Secret: "s3cret", Events: events, Active: true}
	if err := r.s.CreateWebhook(context.Background(), wh); err != nil {
		r.t.Fatalf("create webhook: %v", err)
	}
	r.t.Cleanup(func() {
		ctx := context.Background()
		_, _ = r.s.DB.NewRaw("DELETE FROM webhook_deliveries WHERE webhook_id = ?", wh.ID).Exec(ctx)
		_, _ = r.s.DB.NewRaw("DELETE FROM webhooks WHERE id = ?", wh.ID).Exec(ctx)
	})
	return wh
}

func (r *rig) deliveries(webhookID, event string) []*model.WebhookDelivery {
	r.t.Helper()
	var out []*model.WebhookDelivery
	if err := r.s.DB.NewSelect().Model(&out).
		Where("webhook_id = ?", webhookID).Where("event = ?", event).
		OrderExpr("created_at ASC").Scan(context.Background()); err != nil {
		r.t.Fatalf("list deliveries: %v", err)
	}
	return out
}

// ─── reads ───

func (r *rig) orders(licenseID string) []*model.PaymentOrder {
	r.t.Helper()
	var out []*model.PaymentOrder
	if err := r.s.DB.NewSelect().Model(&out).
		Where("license_id = ?", licenseID).OrderExpr("created_at ASC").Scan(context.Background()); err != nil {
		r.t.Fatalf("list orders: %v", err)
	}
	return out
}

func (r *rig) pendingOrders(licenseID string) []*model.PaymentOrder {
	var out []*model.PaymentOrder
	for _, o := range r.orders(licenseID) {
		if o.Status == "pending" {
			out = append(out, o)
		}
	}
	return out
}

func (r *rig) licence(id string) *model.License {
	r.t.Helper()
	l, err := r.s.FindLicenseByID(context.Background(), id)
	if err != nil {
		r.t.Fatalf("load licence: %v", err)
	}
	return l
}

func (r *rig) subscription(licenseID string) *model.Subscription {
	r.t.Helper()
	sub, err := r.s.GetBillingSubscription(context.Background(), licenseID)
	if err != nil {
		r.t.Fatalf("load subscription: %v", err)
	}
	return sub
}

type queuedMail struct {
	To      string `bun:"to_addr"`
	Subject string `bun:"subject"`
	Body    string `bun:"body"`
}

// waitForAudits polls for the audit rows a pass wrote. store.Audit inserts
// in a goroutine (audit must never block the request), so a test that read
// straight after the pass would race it.
func (r *rig) waitForAudits(t *testing.T, entityID, action string, want int) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		if err := r.s.DB.NewRaw("SELECT count(*) FROM audit_logs WHERE entity_id = ? AND action = ?",
			entityID, action).Scan(context.Background(), &n); err != nil {
			t.Fatalf("count audits: %v", err)
		}
		if n >= want || time.Now().After(deadline) {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *rig) queued(to string) []queuedMail {
	r.t.Helper()
	var out []queuedMail
	if err := r.s.DB.NewRaw("SELECT to_addr, subject, body FROM email_queue WHERE to_addr = ? ORDER BY created_at ASC", to).
		Scan(context.Background(), &out); err != nil {
		r.t.Fatalf("list email queue: %v", err)
	}
	return out
}

func (r *rig) notificationTags(licenseID string) []string {
	r.t.Helper()
	var tags []string
	if err := r.s.DB.NewRaw("SELECT tag FROM notifications WHERE license_id = ? ORDER BY tag", licenseID).
		Scan(context.Background(), &tags); err != nil {
		r.t.Fatalf("list notifications: %v", err)
	}
	return tags
}
