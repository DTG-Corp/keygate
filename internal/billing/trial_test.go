package billing_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/checkout"
	"github.com/dtg-corp/dsoft/internal/store"
)

// The trial conversion is a kind new order (spec §3.4) created inside the
// lead window before trial_end, dated so it stays payable through the
// grace that follows. It names the trial licence in its metadata because
// settlement's new branch has no licence column to put it in.
func TestTrialConversionOrderCreated(t *testing.T) {
	r := newRig(t)
	trialEnd := r.now.Add(3 * 24 * time.Hour)
	f := r.seed(seedOpts{
		End: r.now.Add(30 * 24 * time.Hour), TrialEnd: &trialEnd,
		LicenceStatus: model.StatusTrialing, SubStatus: model.StatusTrialing,
		Price: 250_000, GraceDays: 7,
	})
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	orders := r.orders(f.Licence.ID)
	if len(orders) != 0 {
		t.Fatalf("orders = %d by license_id; a new order must not name the licence", len(orders))
	}
	// Find it the way the engine does: by the metadata it wrote.
	var all []*model.PaymentOrder
	if err := r.s.DB.NewSelect().Model(&all).Where("plan_id = ?", f.Plan.ID).Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("plan orders = %d, want 1", len(all))
	}
	o := all[0]
	if o.Kind != checkout.KindNew || o.Email != f.Licence.Email || o.Quantity != 1 {
		t.Fatalf("order = %+v", o)
	}
	if want := trialEnd.AddDate(0, 0, 7); !o.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %v, want trial end + grace %v", o.ExpiresAt, want)
	}
	var meta struct {
		Billing struct {
			Source         string `json:"source"`
			TrialLicenseID string `json:"trial_license_id"`
			TrialEnd       string `json:"trial_end"`
		} `json:"billing"`
	}
	if err := json.Unmarshal(o.Metadata, &meta); err != nil {
		t.Fatalf("metadata = %s: %v", o.Metadata, err)
	}
	if meta.Billing.Source != "trial_conversion" || meta.Billing.TrialLicenseID != f.Licence.ID {
		t.Fatalf("metadata = %s", o.Metadata)
	}
	if meta.Billing.TrialEnd != trialEnd.Format(time.RFC3339) {
		t.Fatalf("trial_end metadata = %q", meta.Billing.TrialEnd)
	}
	events := r.events.byEvent(model.EventLicenseRenewalDue)
	if len(events) != 1 {
		t.Fatalf("renewal-due events = %d, want 1", len(events))
	}
	if lic, _ := events[0].Data["license"].(map[string]any); lic["id"] != f.Licence.ID {
		t.Fatalf("event licence = %v", events[0].Data["license"])
	}
	// The trial itself is untouched: settlement owns the grant, and until
	// then the fallback sweep owns the ended trial.
	if got := r.licence(f.Licence.ID).Status; got != model.StatusTrialing {
		t.Fatalf("licence status = %q, want trialing", got)
	}
	if r.mail.sent() != 0 {
		t.Fatalf("a trial conversion sent %d reminder mails", r.mail.sent())
	}
}

// Repeat passes resume the same order and emit nothing new.
func TestTrialConversionIsIdempotent(t *testing.T) {
	r := newRig(t)
	trialEnd := r.now.Add(3 * 24 * time.Hour)
	f := r.seed(seedOpts{
		End: r.now.Add(30 * 24 * time.Hour), TrialEnd: &trialEnd,
		LicenceStatus: model.StatusTrialing, SubStatus: model.StatusTrialing, Price: 250_000,
	})
	if err := r.run(); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if err := r.run(); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	var all []*model.PaymentOrder
	if err := r.s.DB.NewSelect().Model(&all).Where("plan_id = ?", f.Plan.ID).Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("orders = %d, want 1", len(all))
	}
	if got := len(r.events.byEvent(model.EventLicenseRenewalDue)); got != 1 {
		t.Fatalf("renewal-due events = %d, want 1", got)
	}
}

func TestTrialConversionOutsideWindow(t *testing.T) {
	r := newRig(t)
	trialEnd := r.now.Add(10 * 24 * time.Hour)
	f := r.seed(seedOpts{
		End: r.now.Add(30 * 24 * time.Hour), TrialEnd: &trialEnd,
		LicenceStatus: model.StatusTrialing, SubStatus: model.StatusTrialing, Price: 250_000,
	})
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := len(r.orders(f.Licence.ID)); got != 0 {
		t.Fatalf("orders = %d, want none before the window", got)
	}
	if got := len(r.events.calls); got != 0 {
		t.Fatalf("events = %d, want none", got)
	}
}

// Once the conversion is paid there is nothing left to order, even though
// the trial licence stays trialing until settlement (or the fallback
// sweep) closes it.
func TestTrialConversionStopsAfterPayment(t *testing.T) {
	r := newRig(t)
	trialEnd := r.now.Add(3 * 24 * time.Hour)
	f := r.seed(seedOpts{
		End: r.now.Add(30 * 24 * time.Hour), TrialEnd: &trialEnd,
		LicenceStatus: model.StatusTrialing, SubStatus: model.StatusTrialing, Price: 250_000,
	})
	ctx := context.Background()
	paid, err := r.s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: f.Product.ID, PlanID: f.Plan.ID, Email: f.Licence.Email,
		Kind: "new", Quantity: 1, SubtotalMinor: 250_000, TotalMinor: 250_000,
		Currency: "VND", CurrencyExponent: 0, Provider: "sepay",
		ExpiresAt: r.now.Add(24 * time.Hour),
		Metadata:  []byte(fmt.Sprintf(`{"billing":{"source":"trial_conversion","trial_license_id":%q}}`, f.Licence.ID)),
	})
	if err != nil {
		t.Fatalf("seed paid order: %v", err)
	}
	if _, err := r.s.DB.NewRaw(`UPDATE payment_orders SET status = ? WHERE id = ?`, "paid", paid.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	var all []*model.PaymentOrder
	if err := r.s.DB.NewSelect().Model(&all).Where("plan_id = ?", f.Plan.ID).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("orders = %d, want only the paid one", len(all))
	}
	if got := len(r.events.calls); got != 0 {
		t.Fatalf("events = %d, want none for an already-paid conversion", got)
	}
}

// A trial plan without a price is an operator's missing row: no order, no
// event, and the pass still succeeds.
func TestTrialConversionUnpricedIsNotFatal(t *testing.T) {
	r := newRig(t)
	trialEnd := r.now.Add(3 * 24 * time.Hour)
	r.seed(seedOpts{
		End: r.now.Add(30 * 24 * time.Hour), TrialEnd: &trialEnd,
		LicenceStatus: model.StatusTrialing, SubStatus: model.StatusTrialing,
	})
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := len(r.events.calls); got != 0 {
		t.Fatalf("events = %d, want none", got)
	}
}

// An ended trial is the fallback sweep's decision: it moves the licence
// onto trial_fallback_plan_id, or expires it when there is none. Dunning
// it here would take that decision away.
func TestEndedTrialIsLeftToTheFallbackSweep(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	trialEnd := r.now.Add(-time.Hour)
	f := r.seed(seedOpts{
		End: trialEnd, TrialEnd: &trialEnd, TrialDays: 14, LicenceType: "trial",
		LicenceStatus: model.StatusTrialing, SubStatus: model.StatusTrialing, Price: 250_000, GraceDays: 7,
	})
	fallback := &model.Plan{
		ProductID: f.Product.ID, Name: "Free", Slug: "free-" + f.Plan.Slug,
		LicenseType: "subscription", BillingInterval: "month", LicenseModel: "standard",
		Active: true, MaxActivations: 1,
	}
	if err := r.s.CreatePlan(ctx, fallback); err != nil {
		t.Fatalf("create fallback: %v", err)
	}
	t.Cleanup(func() {
		_, _ = r.s.DB.NewRaw("DELETE FROM plans WHERE id = ?", fallback.ID).Exec(context.Background())
	})
	if _, err := r.s.DB.NewRaw(`UPDATE plans SET trial_fallback_plan_id = ? WHERE id = ?`,
		fallback.ID, f.Plan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	lic := r.licence(f.Licence.ID)
	if lic.Status != model.StatusTrialing {
		t.Fatalf("status = %q, want the trial left for the fallback sweep", lic.Status)
	}
	if lic.PastDueAt != nil {
		t.Fatalf("past_due_at = %v, the engine dunned an ended trial", lic.PastDueAt)
	}
	if len(r.orders(f.Licence.ID)) != 0 || r.mail.sent() != 0 || len(r.events.calls) != 0 {
		t.Fatalf("ended trial produced %d orders / %d mails / %d events",
			len(r.orders(f.Licence.ID)), r.mail.sent(), len(r.events.calls))
	}
}
