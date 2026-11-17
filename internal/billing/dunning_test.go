package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// Day zero: the period is over, the licence goes past due once, and the
// customer is told to pay.
func TestDunningDueAtPeriodEnd(t *testing.T) {
	r := newRig(t)
	end := r.now
	f := r.seed(seedOpts{End: end, Price: 250_000, GraceDays: 7})
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	lic := r.licence(f.Licence.ID)
	if lic.Status != model.StatusPastDue {
		t.Fatalf("licence status = %q, want past_due", lic.Status)
	}
	if lic.PastDueAt == nil || !lic.PastDueAt.Equal(r.now) {
		t.Fatalf("past_due_at = %v, want the pass time", lic.PastDueAt)
	}
	if sub := r.subscription(f.Licence.ID); sub.Status != model.StatusPastDue {
		t.Fatalf("subscription status = %q", sub.Status)
	}
	queued := r.queued(f.Licence.Email)
	if len(queued) != 1 || queued[0].Subject != "payment due" {
		t.Fatalf("queue = %+v", queued)
	}
	if tags := r.notificationTags(f.Licence.ID); len(tags) != 1 ||
		tags[0] != "renewal_due:"+end.Format(time.RFC3339) {
		t.Fatalf("tags = %v", tags)
	}
	// The customer can still pay: a renewal order exists and stays open
	// through the grace window.
	orders := r.orders(f.Licence.ID)
	if len(orders) != 1 || !orders[0].ExpiresAt.Equal(end.AddDate(0, 0, 7)) {
		t.Fatalf("orders = %+v", orders)
	}
}

func TestDunningRepeatIsIdempotent(t *testing.T) {
	r := newRig(t)
	end := r.now
	f := r.seed(seedOpts{End: end, Price: 250_000, GraceDays: 7})
	if err := r.run(); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	first := *r.licence(f.Licence.ID).PastDueAt
	r.now = end.Add(2 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := *r.licence(f.Licence.ID).PastDueAt; !got.Equal(first) {
		t.Fatalf("past_due_at moved from %v to %v", first, got)
	}
	if got := len(r.queued(f.Licence.Email)); got != 1 {
		t.Fatalf("queued = %d mails, want 1", got)
	}
	if got := len(r.orders(f.Licence.ID)); got != 1 {
		t.Fatalf("orders = %d, want 1", got)
	}
}

// Grace rungs: day 1, day 3, day 7 of a fourteen-day window. Each fires
// once, in order, and the last one only when the window outlives day 7.
func TestDunningGraceRungs(t *testing.T) {
	r := newRig(t)
	end := r.now
	f := r.seed(seedOpts{End: end, Price: 250_000, GraceDays: 14})

	step := func(at time.Time, wantGraces int) {
		t.Helper()
		r.now = at
		if err := r.run(); err != nil {
			t.Fatalf("pass at %v: %v", at, err)
		}
		if got := len(r.mail.graces); got != wantGraces {
			t.Fatalf("at %v grace mails = %d, want %d", at, got, wantGraces)
		}
	}

	step(end, 0)                               // the due notice
	step(end.Add(1*24*time.Hour+time.Hour), 1) // day 1 of grace
	// 12 days and 23 hours left, rounded up: a customer told "12 days"
	// on the morning of day 13 would feel cheated.
	if got := r.mail.graces[0].Days; got != 13 {
		t.Fatalf("grace day 1 mail says %d days left, want 13", got)
	}
	step(end.Add(3*24*time.Hour+time.Hour), 2) // day 3
	step(end.Add(7*24*time.Hour+time.Hour), 3) // day 7
	step(end.Add(9*24*time.Hour), 3)           // nothing new
	if lic := r.licence(f.Licence.ID); lic.Status != model.StatusPastDue {
		t.Fatalf("licence status = %q before the grace window ends", lic.Status)
	}
	if got := len(r.queued(f.Licence.Email)); got != 4 {
		t.Fatalf("queued = %d, want due + 3 grace rungs", got)
	}
	tags := r.notificationTags(f.Licence.ID)
	want := []string{
		"renewal_due:" + end.Format(time.RFC3339),
		"renewal_grace_1:" + end.Format(time.RFC3339),
		"renewal_grace_3:" + end.Format(time.RFC3339),
		"renewal_grace_7:" + end.Format(time.RFC3339),
	}
	for i := range want {
		if tags[i] != want[i] {
			t.Fatalf("tags = %v, want %v", tags, want)
		}
	}

	// The window ends: suspension, with its own mail.
	r.now = end.Add(14 * 24 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("pass at grace end: %v", err)
	}
	lic := r.licence(f.Licence.ID)
	if lic.Status != model.StatusSuspended || lic.SuspendedBy != store.BillingSuspendedBy {
		t.Fatalf("licence = %q/%q, want suspended/billing", lic.Status, lic.SuspendedBy)
	}
	if len(r.mail.suspensions) != 1 {
		t.Fatalf("suspension mails = %d, want 1", len(r.mail.suspensions))
	}
	if got := len(r.queued(f.Licence.Email)); got != 5 {
		t.Fatalf("queued = %d, want 5", got)
	}
}

// The default seven-day grace makes day 7 the suspension day, so the
// seventh rung is the suspension mail, not a fourth grace mail.
func TestDunningSuspendsAtGraceEnd(t *testing.T) {
	r := newRig(t)
	end := r.now
	f := r.seed(seedOpts{End: end, Price: 250_000, GraceDays: 7})
	r.now = end.Add(7 * 24 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	lic := r.licence(f.Licence.ID)
	if lic.Status != model.StatusSuspended || lic.SuspendedBy != store.BillingSuspendedBy {
		t.Fatalf("licence = %q/%q", lic.Status, lic.SuspendedBy)
	}
	if lic.SuspendedAt == nil || !lic.SuspendedAt.Equal(r.now) {
		t.Fatalf("suspended_at = %v", lic.SuspendedAt)
	}
	if len(r.mail.suspensions) != 1 || len(r.mail.graces) != 0 {
		t.Fatalf("mails = %d grace / %d suspension", len(r.mail.graces), len(r.mail.suspensions))
	}
	if sub := r.subscription(f.Licence.ID); sub.Status != model.StatusPastDue {
		t.Fatalf("subscription status = %q", sub.Status)
	}
	// The suspension is mailed once, however often the pass runs.
	r.now = end.Add(8 * 24 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if got := len(r.queued(f.Licence.Email)); got != 1 {
		t.Fatalf("queued = %d mails after a repeat", got)
	}
}

// A suspended licence ends after the hold: renewal_days when the plan
// sets one, and it does not end a day early.
func TestDunningExpiresAfterHold(t *testing.T) {
	r := newRig(t)
	end := r.now
	f := r.seed(seedOpts{End: end, Price: 250_000, GraceDays: 7, RenewalDays: 3})
	if err := r.run(); err != nil {
		t.Fatalf("due pass: %v", err)
	}
	r.now = end.Add(7 * 24 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("suspension pass: %v", err)
	}
	if lic := r.licence(f.Licence.ID); lic.Status != model.StatusSuspended {
		t.Fatalf("status = %q, want suspended", lic.Status)
	}

	r.now = end.Add(9 * 24 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("hold pass: %v", err)
	}
	if lic := r.licence(f.Licence.ID); lic.Status != model.StatusSuspended {
		t.Fatalf("status = %q one day before the hold ends", lic.Status)
	}
	if got := len(r.events.byEvent(model.EventLicenseExpired)); got != 0 {
		t.Fatalf("expired events = %d before the hold ends", got)
	}

	r.now = end.Add(10 * 24 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("expiry pass: %v", err)
	}
	if lic := r.licence(f.Licence.ID); lic.Status != model.StatusExpired {
		t.Fatalf("status = %q, want expired", lic.Status)
	}
	events := r.events.byEvent(model.EventLicenseExpired)
	if len(events) != 1 || events[0].Data["reason"] != "billing_dunning" {
		t.Fatalf("expired events = %+v", events)
	}
	if sub := r.subscription(f.Licence.ID); sub.Status != model.StatusExpired {
		t.Fatalf("subscription status = %q", sub.Status)
	}
	// Expiry is a one-way door: another pass changes nothing.
	if err := r.run(); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if got := len(r.events.byEvent(model.EventLicenseExpired)); got != 1 {
		t.Fatalf("expired events = %d after a repeat", got)
	}
}

// No renewal_days means the hold is one billing interval: a month for a
// monthly plan, a year for a yearly one. Zero would make suspension
// unobservable, which is why it is not the fallback.
func TestDunningHoldFallsBackToTheBillingInterval(t *testing.T) {
	r := newRig(t)
	end := r.now
	f := r.seed(seedOpts{End: end, Price: 250_000, GraceDays: 7})
	r.now = end.Add(7 * 24 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("suspension pass: %v", err)
	}
	r.now = end.Add(36 * 24 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if lic := r.licence(f.Licence.ID); lic.Status != model.StatusSuspended {
		t.Fatalf("status = %q before one month of hold", lic.Status)
	}
	r.now = end.Add(37 * 24 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if lic := r.licence(f.Licence.ID); lic.Status != model.StatusExpired {
		t.Fatalf("status = %q after one month of hold", lic.Status)
	}
}

// An operator's suspension is not the engine's to lift or to deepen: no
// mail, no expiry, no webhook.
func TestDunningSkipsOperatorSuspension(t *testing.T) {
	r := newRig(t)
	end := r.now.Add(-20 * 24 * time.Hour)
	f := r.seed(seedOpts{End: end, Price: 250_000, GraceDays: 7, LicenceStatus: model.StatusSuspended})
	if _, err := r.s.DB.NewRaw(`UPDATE licenses SET suspended_at = ?, suspended_by = ?, past_due_at = ? WHERE id = ?`,
		r.now.Add(-19*24*time.Hour), model.SuspendedByAdmin, r.now.Add(-20*24*time.Hour), f.Licence.ID).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	lic := r.licence(f.Licence.ID)
	if lic.Status != model.StatusSuspended || lic.SuspendedBy != model.SuspendedByAdmin {
		t.Fatalf("licence = %q/%q", lic.Status, lic.SuspendedBy)
	}
	if got := len(r.queued(f.Licence.Email)); got != 0 {
		t.Fatalf("queued = %d mails for an operator's suspension", got)
	}

	// Well past the hold: still the operator's decision.
	r.now = end.Add(60 * 24 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("late pass: %v", err)
	}
	if lic := r.licence(f.Licence.ID); lic.Status != model.StatusSuspended {
		t.Fatalf("status = %q, want the operator's suspension kept", lic.Status)
	}
	if got := len(r.events.byEvent(model.EventLicenseExpired)); got != 0 {
		t.Fatalf("expired events = %d for an operator's suspension", got)
	}
}

// The scan keeps a suspended licence in view however old its period is:
// this engine, not the generic grace sweep, is what ends it.
func TestSuspendedTailIsExpiredByTheScan(t *testing.T) {
	r := newRig(t)
	end := r.now.Add(-40 * 24 * time.Hour)
	f := r.seed(seedOpts{End: end, Price: 250_000, GraceDays: 7, LicenceStatus: model.StatusSuspended})
	if _, err := r.s.DB.NewRaw(`UPDATE licenses SET suspended_at = ?, suspended_by = ?, past_due_at = ? WHERE id = ?`,
		end.Add(7*24*time.Hour), store.BillingSuspendedBy, end, f.Licence.ID).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if lic := r.licence(f.Licence.ID); lic.Status != model.StatusExpired {
		t.Fatalf("status = %q, want expired", lic.Status)
	}
	if got := len(r.events.byEvent(model.EventLicenseExpired)); got != 1 {
		t.Fatalf("expired events = %d, want 1", got)
	}
}

// EndGraceExpiredLicense is the seam the generic grace sweep asks before
// it expires anything: billing licences are claimed (and expired on the
// engine's own schedule), everything else is left to the sweep.
func TestEndGraceExpiredLicenseClaimsBillingSubscriptions(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	end := r.now.Add(-8 * 24 * time.Hour)
	f := r.seed(seedOpts{End: end, Price: 250_000, GraceDays: 7, RenewalDays: 3})
	lic := r.licence(f.Licence.ID)

	handled, err := r.svc.EndGraceExpiredLicense(ctx, lic, end.Add(7*24*time.Hour))
	if err != nil || !handled {
		t.Fatalf("handled = %v, err = %v, want claimed", handled, err)
	}
	if got := r.licence(f.Licence.ID).Status; got != model.StatusPastDue && got != model.StatusActive {
		t.Fatalf("status = %q, claimed licences must not be expired early", got)
	}

	r.now = end.Add(10 * 24 * time.Hour)
	handled, err = r.svc.EndGraceExpiredLicense(ctx, r.licence(f.Licence.ID), end.Add(7*24*time.Hour))
	if err != nil || !handled {
		t.Fatalf("handled = %v, err = %v", handled, err)
	}
	if got := r.licence(f.Licence.ID).Status; got != model.StatusExpired {
		t.Fatalf("status = %q, want expired once the hold is over", got)
	}

	stripe := r.seed(seedOpts{Provider: "stripe", Price: 250_000, End: end})
	handled, err = r.svc.EndGraceExpiredLicense(ctx, r.licence(stripe.Licence.ID), end)
	if err != nil || handled {
		t.Fatalf("stripe licence handled = %v, err = %v, want left to the sweep", handled, err)
	}
	plain := r.seed(seedOpts{End: end, Price: 250_000})
	if _, err := r.s.DB.NewRaw(`DELETE FROM subscriptions WHERE license_id = ?`, plain.Licence.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	handled, err = r.svc.EndGraceExpiredLicense(ctx, r.licence(plain.Licence.ID), end)
	if err != nil || handled {
		t.Fatalf("subscription-less licence handled = %v, err = %v", handled, err)
	}
}

// A revoked licence is a decision, not a dunning state.
func TestDunningLeavesRevokedAlone(t *testing.T) {
	r := newRig(t)
	end := r.now.Add(-20 * 24 * time.Hour)
	f := r.seed(seedOpts{End: end, Price: 250_000, GraceDays: 7, LicenceStatus: model.StatusRevoked})
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if lic := r.licence(f.Licence.ID); lic.Status != model.StatusRevoked {
		t.Fatalf("status = %q", lic.Status)
	}
	if r.mail.sent() != 0 || len(r.events.calls) != 0 {
		t.Fatalf("a revoked licence produced %d mails / %d events", r.mail.sent(), len(r.events.calls))
	}
}
