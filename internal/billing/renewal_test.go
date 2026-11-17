package billing_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/checkout"
	"github.com/dtg-corp/dsoft/internal/payment/provider"
	"github.com/dtg-corp/dsoft/internal/testsupport"
)

// The renewal order is the whole point of the engine: it has to name the
// licence, its plan, its quantity, the exact period it pays for, and
// stay payable through the grace window so the QR the customer was sent
// on day -7 still works on day +1.
func TestRenewalOrderCreatedInsideLeadWindow(t *testing.T) {
	r := newRig(t)
	end := r.now.Add(3 * 24 * time.Hour)
	f := r.seed(seedOpts{End: end, Price: 250_000, Quantity: 2, GraceDays: 7})
	start := *f.Sub.CurrentPeriodStart

	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	orders := r.orders(f.Licence.ID)
	if len(orders) != 1 {
		t.Fatalf("orders = %d, want 1", len(orders))
	}
	o := orders[0]
	if o.Kind != checkout.KindRenewal || o.LicenseID != f.Licence.ID || o.PlanID != f.Plan.ID {
		t.Fatalf("order = %+v", o)
	}
	if o.Email != f.Licence.Email || o.Quantity != 2 {
		t.Fatalf("email/quantity = %q/%d", o.Email, o.Quantity)
	}
	if o.TotalMinor != 500_000 {
		t.Fatalf("total = %d, want 2 x 250000", o.TotalMinor)
	}
	if o.PeriodStart == nil || !o.PeriodStart.Equal(start) {
		t.Fatalf("period start = %v, want %v", o.PeriodStart, start)
	}
	if o.PeriodEnd == nil || !o.PeriodEnd.Equal(end) {
		t.Fatalf("period end = %v, want %v", o.PeriodEnd, end)
	}
	if want := end.Add(7 * 24 * time.Hour); !o.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %v, want period end + grace %v", o.ExpiresAt, want)
	}
	var meta struct {
		Billing struct {
			Source string `json:"source"`
		} `json:"billing"`
	}
	if err := json.Unmarshal(o.Metadata, &meta); err != nil {
		t.Fatalf("metadata = %s: %v", o.Metadata, err)
	}
	if meta.Billing.Source != "renewal_worker" {
		t.Fatalf("metadata = %s", o.Metadata)
	}
	if o.Provider != "sepay" {
		t.Fatalf("provider = %q, want the licence's sepay", o.Provider)
	}
	if len(r.prod.Calls) != 1 || r.prod.Calls[0].OrderCode != o.Code {
		t.Fatalf("provider calls = %+v", r.prod.Calls)
	}
	// The order is the period record: a pending order must exist in the
	// table the unique index guards.
	if got := len(r.pendingOrders(f.Licence.ID)); got != 1 {
		t.Fatalf("pending orders = %d, want 1", got)
	}
}

func TestRenewalNotCreatedBeforeWindow(t *testing.T) {
	r := newRig(t)
	f := r.seed(seedOpts{End: r.now.Add(8 * 24 * time.Hour), Price: 250_000})
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := len(r.orders(f.Licence.ID)); got != 0 {
		t.Fatalf("orders = %d, want none before the lead window", got)
	}
	if r.mail.sent() != 0 {
		t.Fatalf("mails = %d, want none", r.mail.sent())
	}
}

// Exactly seven days out is inside the window; one second further is
// not. The edge is the bug every "before" comparison hides.
func TestRenewalWindowEdgeIsInclusive(t *testing.T) {
	r := newRig(t)
	inside := r.seed(seedOpts{End: r.now.Add(7 * 24 * time.Hour), Price: 250_000})
	outside := r.seed(seedOpts{End: r.now.Add(7*24*time.Hour + time.Second), Price: 250_000})
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := len(r.orders(inside.Licence.ID)); got != 1 {
		t.Fatalf("inside window orders = %d, want 1", got)
	}
	if got := len(r.orders(outside.Licence.ID)); got != 0 {
		t.Fatalf("outside window orders = %d, want 0", got)
	}
}

// Two passes at the same instant are the same hour twice: one order, one
// event, one instruction.
func TestRenewalRepeatPassCreatesOneOrder(t *testing.T) {
	r := newRig(t)
	f := r.seed(seedOpts{End: r.now.Add(2 * 24 * time.Hour), Price: 250_000})
	if err := r.run(); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if err := r.run(); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := len(r.pendingOrders(f.Licence.ID)); got != 1 {
		t.Fatalf("pending orders = %d, want 1", got)
	}
	if got := len(r.events.byEvent(model.EventLicenseRenewalDue)); got != 1 {
		t.Fatalf("renewal-due events = %d, want 1", got)
	}
	if len(r.prod.Calls) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(r.prod.Calls))
	}
}

// A cancellation announced for the period end stops the order and the
// ladder right there; nothing is charged, nothing is mailed.
func TestRenewalSkipsCancelAtPeriodEnd(t *testing.T) {
	r := newRig(t)
	f := r.seed(seedOpts{End: r.now.Add(2 * 24 * time.Hour), Price: 250_000, Cancel: true})
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := len(r.orders(f.Licence.ID)); got != 0 {
		t.Fatalf("orders = %d, want none", got)
	}
	if r.mail.sent() != 0 || len(r.queued(f.Licence.Email)) != 0 {
		t.Fatalf("a cancelled subscription was mailed")
	}
}

func TestScanExcludesHeldAndStripeLicences(t *testing.T) {
	r := newRig(t)
	revoked := r.seed(seedOpts{LicenceStatus: model.StatusRevoked, End: r.now.Add(2 * 24 * time.Hour), Price: 250_000})
	stripe := r.seed(seedOpts{Provider: "stripe", Price: 250_000, End: r.now.Add(2 * 24 * time.Hour)})
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	for _, f := range []*seeded{revoked, stripe} {
		if got := len(r.orders(f.Licence.ID)); got != 0 {
			t.Fatalf("licence %s (%s/%s) got %d orders", f.Licence.ID, f.Licence.Status, f.Sub.PaymentProvider, got)
		}
	}
	if len(r.events.calls) != 0 || r.mail.sent() != 0 {
		t.Fatalf("excluded licences produced %d events / %d mails", len(r.events.calls), r.mail.sent())
	}
}

// A plan with no price is an operator's missing row, not a broken pass:
// the run succeeds, nothing is ordered and nothing is mailed.
func TestRenewalUnpricedPlanIsLoggedNotFatal(t *testing.T) {
	r := newRig(t)
	f := r.seed(seedOpts{End: r.now.Add(2 * 24 * time.Hour)})
	if err := r.run(); err != nil {
		t.Fatalf("pass returned an error for a missing price: %v", err)
	}
	if got := len(r.orders(f.Licence.ID)); got != 0 {
		t.Fatalf("orders = %d, want none", got)
	}
	if r.mail.sent() != 0 {
		t.Fatalf("a licence with no payable order was mailed")
	}
}

// A provider failure is a transient upstream problem: the pass reports
// it and the next hour retries.
func TestRenewalProviderFailureIsReported(t *testing.T) {
	r := newRig(t)
	f := r.seed(seedOpts{End: r.now.Add(2 * 24 * time.Hour), Price: 250_000})
	r.prod.CreateFn = func(context.Context, provider.CreateAttemptRequest) (provider.CreateAttemptResult, error) {
		return provider.CreateAttemptResult{}, errors.New("provider down")
	}
	err := r.run()
	if err == nil {
		t.Fatal("a provider failure was swallowed")
	}
	// The order exists but carries no instruction; the next pass retries
	// the attempt on the same order rather than minting a second one.
	orders := r.orders(f.Licence.ID)
	if len(orders) != 1 {
		t.Fatalf("orders = %d, want 1", len(orders))
	}
	if got := len(r.events.byEvent(model.EventLicenseRenewalDue)); got != 0 {
		t.Fatalf("renewal-due events = %d, want none without an instruction", got)
	}
}

// The event carries exactly what §8 promises: the licence identity and
// the order the customer pays, through the real delivery table.
func TestRenewalDueEventPayload(t *testing.T) {
	r := newRig(t)
	r.useRealWebhooks()
	f := r.seed(seedOpts{End: r.now.Add(3 * 24 * time.Hour), Price: 250_000})
	wh := r.createWebhook(f.Product.ID, model.EventLicenseRenewalDue)

	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	orders := r.orders(f.Licence.ID)
	if len(orders) != 1 {
		t.Fatalf("orders = %d", len(orders))
	}
	deliveries := r.deliveries(wh.ID, model.EventLicenseRenewalDue)
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(deliveries))
	}
	payload, _ := deliveries[0].Payload["data"].(map[string]any)
	lic, _ := payload["license"].(map[string]any)
	if lic["id"] != f.Licence.ID {
		t.Fatalf("license payload = %v", payload["license"])
	}
	if prefix, _ := lic["key_prefix"].(string); prefix == "" || prefix != f.Licence.LicenseKey[:8] {
		t.Fatalf("key_prefix = %v, want %q", lic["key_prefix"], f.Licence.LicenseKey[:8])
	}
	if lic["valid_until"] == nil {
		t.Fatal("valid_until missing from the payload")
	}
	ord, _ := payload["order"].(map[string]any)
	if ord["code"] != orders[0].Code {
		t.Fatalf("order payload = %v", payload["order"])
	}
	if ord["total"] == nil || ord["expires_at"] == nil {
		t.Fatalf("order payload = %v", payload["order"])
	}
}

// The lead window is an install setting, floored at seven days and
// capped so a typo cannot open it a year early.
func TestLeadDaysSetting(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = r.s.DB.NewRaw("DELETE FROM settings WHERE key = ?", "renewal_lead_days").Exec(context.Background())
	})

	for _, tc := range []struct {
		setting string
		want    int
	}{
		{"", 7}, {"3", 7}, {"14", 14}, {"90", 90}, {"365", 90}, {"abc", 7}, {"-4", 7},
	} {
		if tc.setting == "" {
			_, _ = r.s.DB.NewRaw("DELETE FROM settings WHERE key = ?", "renewal_lead_days").Exec(ctx)
		} else {
			testsupport.SetSetting(t, r.s.DB, "renewal_lead_days", tc.setting)
		}
		if got := r.svc.LeadDays(ctx); got != tc.want {
			t.Errorf("setting %q: lead days = %d, want %d", tc.setting, got, tc.want)
		}
	}

	// A wider window really creates the order earlier: 14 days out is
	// outside the default window and inside a 14-day one.
	f := r.seed(seedOpts{End: r.now.Add(14 * 24 * time.Hour), Price: 250_000})
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := len(r.orders(f.Licence.ID)); got != 0 {
		t.Fatalf("orders = %d, want none at the default window", got)
	}
	testsupport.SetSetting(t, r.s.DB, "renewal_lead_days", "14")
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := len(r.orders(f.Licence.ID)); got != 1 {
		t.Fatalf("orders = %d, want 1 inside a 14-day window", got)
	}
}
