package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// The cure for dunning is money. A payment that lands inside the grace
// window extends the period, clears past_due and reactivates the licence
// in one write (settlement's ExtendLicenseFromOrderIn); the next pass must
// find nothing left to dun.
func TestPaymentDuringGraceStopsDunning(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	end := r.now
	f := r.seed(seedOpts{End: end, Price: 250_000, GraceDays: 7})
	if err := r.run(); err != nil {
		t.Fatalf("due pass: %v", err)
	}
	if got := r.licence(f.Licence.ID).Status; got != model.StatusPastDue {
		t.Fatalf("status = %q, want past_due", got)
	}
	orders := r.orders(f.Licence.ID)
	if len(orders) != 1 {
		t.Fatalf("orders = %d", len(orders))
	}

	// The customer pays the renewal order; settlement applies it.
	if err := r.s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		return store.ExtendLicenseFromOrderIn(ctx, tx, orders[0], f.Plan)
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	lic := r.licence(f.Licence.ID)
	if lic.Status != model.StatusActive || lic.PastDueAt != nil {
		t.Fatalf("licence = %q past_due_at %v after payment", lic.Status, lic.PastDueAt)
	}

	r.now = end.Add(3 * 24 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("post-payment pass: %v", err)
	}
	if got := r.licence(f.Licence.ID).Status; got != model.StatusActive {
		t.Fatalf("status = %q after the pass", got)
	}
	if len(r.mail.graces) != 0 || len(r.mail.suspensions) != 0 {
		t.Fatalf("a paid licence was dunned: %d grace / %d suspension", len(r.mail.graces), len(r.mail.suspensions))
	}
	if got := len(r.queued(f.Licence.Email)); got != 1 {
		t.Fatalf("queued = %d mails, want only the pre-payment due notice", got)
	}
}

// A payment after suspension has to lift the engine's suspension, or the
// customer pays and stays locked out. settlement's own extension refuses
// a suspended licence, so the reinstate comes first, in the same
// transaction.
func TestReinstateLiftsBillingSuspension(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	end := r.now.Add(-10 * 24 * time.Hour)
	f := r.seed(seedOpts{End: end, Price: 250_000, GraceDays: 7, LicenceStatus: model.StatusSuspended})
	if _, err := r.s.DB.NewRaw(`UPDATE licenses SET suspended_at = ?, suspended_by = ?, past_due_at = ? WHERE id = ?`,
		end.Add(7*24*time.Hour), store.BillingSuspendedBy, end, f.Licence.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	order, err := r.s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: f.Product.ID, PlanID: f.Plan.ID, Email: f.Licence.Email,
		Kind: "renewal", Quantity: 1, SubtotalMinor: 250_000, TotalMinor: 250_000,
		Currency: "VND", CurrencyExponent: 0, Provider: "sepay",
		ExpiresAt: r.now.Add(24 * time.Hour),
		LicenseID: f.Licence.ID, PeriodStart: f.Sub.CurrentPeriodStart, PeriodEnd: &end,
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if err := r.s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := r.s.ReinstateLicenseForRenewalIn(ctx, tx, f.Licence.ID); err != nil {
			return err
		}
		return store.ExtendLicenseFromOrderIn(ctx, tx, order, f.Plan)
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	lic := r.licence(f.Licence.ID)
	if lic.Status != model.StatusActive || lic.SuspendedBy != "" || lic.SuspendedAt != nil || lic.PastDueAt != nil {
		t.Fatalf("licence after payment = %q/%q suspended_at %v past_due_at %v",
			lic.Status, lic.SuspendedBy, lic.SuspendedAt, lic.PastDueAt)
	}
	if err := r.run(); err != nil {
		t.Fatalf("post-payment pass: %v", err)
	}
	if got := r.licence(f.Licence.ID).Status; got != model.StatusActive {
		t.Fatalf("status = %q", got)
	}
	if len(r.mail.suspensions) != 0 || len(r.events.byEvent(model.EventLicenseExpired)) != 0 {
		t.Fatalf("a reinstated licence was dunned: %d mails / %d expiries",
			len(r.mail.suspensions), len(r.events.byEvent(model.EventLicenseExpired)))
	}
}

// A restart mid-ladder must not repeat a rung: the claim lives in the
// database, not in the process.
func TestRestartDoesNotRepeatARung(t *testing.T) {
	r := newRig(t)
	f := r.seed(seedOpts{End: r.now.Add(3 * 24 * time.Hour), Price: 250_000})
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	// A fresh service over the same store is a restart.
	r.rebuild()
	if err := r.run(); err != nil {
		t.Fatalf("restarted pass: %v", err)
	}
	if got := len(r.queued(f.Licence.Email)); got != 1 {
		t.Fatalf("queued = %d mails after a restart, want 1", got)
	}
}
