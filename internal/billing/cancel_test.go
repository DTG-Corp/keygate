package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// A cancellation announced for the period end does nothing before it:
// no order, no reminder, and the subscription stays live.
func TestCancelAtPeriodEndWaitsForThePeriod(t *testing.T) {
	r := newRig(t)
	end := r.now.Add(2 * 24 * time.Hour)
	f := r.seed(seedOpts{End: end, Price: 250_000, Cancel: true})
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	sub := r.subscription(f.Licence.ID)
	if sub.Status != model.StatusActive || !sub.CancelAtPeriodEnd {
		t.Fatalf("subscription = %q cancel=%v", sub.Status, sub.CancelAtPeriodEnd)
	}
	if len(r.orders(f.Licence.ID)) != 0 || r.mail.sent() != 0 {
		t.Fatalf("a cancelled subscription got %d orders / %d mails", len(r.orders(f.Licence.ID)), r.mail.sent())
	}
}

// At the period end the flag becomes the cancellation: the subscription
// ends, and the licence is left exactly as the customer paid for it.
func TestCancelAtPeriodEndEndsTheSubscription(t *testing.T) {
	r := newRig(t)
	end := r.now
	f := r.seed(seedOpts{End: end, Price: 250_000, Cancel: true})
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	sub := r.subscription(f.Licence.ID)
	if sub.Status != model.StatusCanceled {
		t.Fatalf("subscription status = %q, want canceled", sub.Status)
	}
	if sub.CanceledAt == nil || !sub.CanceledAt.Equal(end) {
		t.Fatalf("canceled_at = %v, want the period end %v", sub.CanceledAt, end)
	}
	lic := r.licence(f.Licence.ID)
	if lic.Status != model.StatusActive {
		t.Fatalf("licence status = %q; the cancellation is not an expiry", lic.Status)
	}
	if lic.ValidUntil == nil || !lic.ValidUntil.Equal(end) {
		t.Fatalf("valid_until = %v, want %v", lic.ValidUntil, end)
	}
	if len(r.orders(f.Licence.ID)) != 0 || r.mail.sent() != 0 || len(r.events.calls) != 0 {
		t.Fatalf("cancellation produced %d orders / %d mails / %d events",
			len(r.orders(f.Licence.ID)), r.mail.sent(), len(r.events.calls))
	}

	// A second pass finds nothing to do, and the audit trail has one row.
	if err := r.run(); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if audits := r.waitForAudits(t, f.Licence.ID, "subscription_canceled", 1); audits != 1 {
		t.Fatalf("cancellation audits = %d, want 1", audits)
	}
}

// The licence keeps its paid period and the platform grace sweep decides
// what happens after it; the engine has nothing left to say.
func TestCancelAtPeriodEndLeavesTheLicenceToTheSweep(t *testing.T) {
	r := newRig(t)
	end := r.now
	f := r.seed(seedOpts{End: end, Price: 250_000, Cancel: true})
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	r.now = end.Add(48 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("later pass: %v", err)
	}
	if got := r.licence(f.Licence.ID).Status; got != model.StatusActive {
		t.Fatalf("licence status = %q, the engine must not expire a cancellation", got)
	}
	if got := len(r.queued(f.Licence.Email)); got != 0 {
		t.Fatalf("queued = %d mails", got)
	}
}

// Cancelling while past due stops the ladder at the period end: the
// subscription closes instead of walking on to suspension.
func TestCancelStopsDunning(t *testing.T) {
	r := newRig(t)
	end := r.now
	f := r.seed(seedOpts{End: end, Price: 250_000, GraceDays: 7})
	if err := r.run(); err != nil {
		t.Fatalf("due pass: %v", err)
	}
	if got := r.licence(f.Licence.ID).Status; got != model.StatusPastDue {
		t.Fatalf("licence status = %q, want past_due", got)
	}
	// The customer cancels from the portal while past due.
	if _, err := r.s.DB.NewRaw(`UPDATE subscriptions SET cancel_at_period_end = true WHERE license_id = ?`,
		f.Licence.ID).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.run(); err != nil {
		t.Fatalf("cancel pass: %v", err)
	}
	if sub := r.subscription(f.Licence.ID); sub.Status != model.StatusCanceled {
		t.Fatalf("subscription status = %q", sub.Status)
	}
	r.now = end.Add(7 * 24 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("grace-end pass: %v", err)
	}
	if got := r.licence(f.Licence.ID).Status; got == model.StatusSuspended {
		t.Fatal("a cancelled subscription was suspended by the engine")
	}
	// The due notice from before the cancellation stands; nothing after it
	// does.
	if len(r.mail.graces) != 0 || len(r.mail.suspensions) != 0 {
		t.Fatalf("a cancelled subscription walked on: %d grace / %d suspension mails",
			len(r.mail.graces), len(r.mail.suspensions))
	}
	if got := len(r.queued(f.Licence.Email)); got != 1 {
		t.Fatalf("queued = %d mails, want only the due notice", got)
	}
}
