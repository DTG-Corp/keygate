package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/billing"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/checkout"
)

// One mail per rung: the customer gets a week, then three days, then the
// last warning, and nothing in between.
func TestReminderRungFireOnceEach(t *testing.T) {
	r := newRig(t)
	end := r.now.Add(7 * 24 * time.Hour)
	f := r.seed(seedOpts{End: end, Price: 250_000})

	if err := r.run(); err != nil {
		t.Fatalf("day -7: %v", err)
	}
	if len(r.mail.notices) != 1 || r.mail.notices[0].Days != 7 {
		t.Fatalf("day -7 notices = %+v", r.mail.notices)
	}
	if got := r.mail.notices[0].PayURL; got != "https://keygate.test/portal" {
		t.Fatalf("pay URL = %q", got)
	}
	if got := r.mail.notices[0].PeriodEnd; !got.Equal(end) {
		t.Fatalf("period end = %v", got)
	}
	queued := r.queued(f.Licence.Email)
	if len(queued) != 1 || queued[0].Subject != "renewal reminder" {
		t.Fatalf("queue = %+v", queued)
	}
	tags := r.notificationTags(f.Licence.ID)
	if len(tags) != 1 || tags[0] != "renewal_reminder_7:"+end.Format(time.RFC3339) {
		t.Fatalf("tags = %v", tags)
	}

	// Same instant again: the rung is already claimed.
	if err := r.run(); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if got := len(r.queued(f.Licence.Email)); got != 1 {
		t.Fatalf("repeat queued %d mails", got)
	}

	// Three days out: the next rung, not the first one again.
	r.now = end.Add(-3 * 24 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("day -3: %v", err)
	}
	if len(r.mail.notices) != 2 || r.mail.notices[1].Days != 3 {
		t.Fatalf("day -3 notices = %+v", r.mail.notices)
	}
	// One day out: the last one.
	r.now = end.Add(-24 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("day -1: %v", err)
	}
	if len(r.mail.notices) != 3 || r.mail.notices[2].Days != 1 {
		t.Fatalf("day -1 notices = %+v", r.mail.notices)
	}
	if got := len(r.queued(f.Licence.Email)); got != 3 {
		t.Fatalf("queued = %d mails, want 3", got)
	}
	// And a repeat of the last day adds nothing.
	if err := r.run(); err != nil {
		t.Fatalf("repeat day -1: %v", err)
	}
	if got := len(r.queued(f.Licence.Email)); got != 3 {
		t.Fatalf("queued = %d mails after a repeat, want 3", got)
	}
}

// The window edge and the rung edges: eight days is silent, six hours is
// the last warning.
func TestReminderEdges(t *testing.T) {
	r := newRig(t)
	outside := r.seed(seedOpts{End: r.now.Add(7*24*time.Hour + time.Second), Price: 250_000})
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := len(r.queued(outside.Licence.Email)); got != 0 {
		t.Fatalf("a licence just outside the window got %d mails", got)
	}

	r.now = outside.Sub.CurrentPeriodEnd.Add(-3 * 24 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	queued := r.queued(outside.Licence.Email)
	if len(queued) != 1 {
		t.Fatalf("queued = %d, want the _3 rung", len(queued))
	}
	if tags := r.notificationTags(outside.Licence.ID); len(tags) != 1 ||
		tags[0] != "renewal_reminder_3:"+outside.Sub.CurrentPeriodEnd.Format(time.RFC3339) {
		t.Fatalf("tags = %v", tags)
	}

	r.now = outside.Sub.CurrentPeriodEnd.Add(-6 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(r.queued(outside.Licence.Email)) != 2 {
		t.Fatalf("the _1 rung did not fire inside the last day")
	}
}

// Every period end gets its own ladder: a fixed notification tag would
// remind a customer once in their lifetime.
func TestReminderNewPeriodRestartsTheLadder(t *testing.T) {
	r := newRig(t)
	f := r.seed(seedOpts{End: r.now.Add(3 * 24 * time.Hour), Price: 250_000})
	if err := r.run(); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if got := len(r.queued(f.Licence.Email)); got != 1 {
		t.Fatalf("first cycle queued %d, want 1", got)
	}
	// A payment extends the licence by a period.
	next := r.now.Add(30 * 24 * time.Hour)
	if _, err := r.s.DB.NewRaw(`UPDATE subscriptions SET current_period_start = ?, current_period_end = ? WHERE license_id = ?`,
		f.Sub.CurrentPeriodEnd, next, f.Licence.ID).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.now = next.Add(-3 * 24 * time.Hour)
	if err := r.run(); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if got := len(r.queued(f.Licence.Email)); got != 2 {
		t.Fatalf("second cycle queued %d mails in total, want 2", got)
	}
}

// The scan may be an hour old; a payment that landed in between must not
// be answered with "your subscription renews soon".
func TestReminderSkipsRenewedSinceScan(t *testing.T) {
	r := newRig(t)
	f := r.seed(seedOpts{End: r.now.Add(3 * 24 * time.Hour), Price: 250_000})
	next := r.now.Add(33 * 24 * time.Hour)
	r.useCheckout(&fakeOrders{fn: func(in checkout.CreateOrderInput) (*checkout.OrderResult, error) {
		if _, err := r.s.DB.NewRaw(`UPDATE subscriptions SET current_period_start = ?, current_period_end = ? WHERE license_id = ?`,
			in.PeriodEnd, next, in.LicenseID).Exec(context.Background()); err != nil {
			t.Errorf("move period: %v", err)
		}
		if _, err := r.s.DB.NewRaw(`UPDATE licenses SET valid_until = ? WHERE id = ?`,
			next, in.LicenseID).Exec(context.Background()); err != nil {
			t.Errorf("extend licence: %v", err)
		}
		return &checkout.OrderResult{Order: &model.PaymentOrder{
			ID: "ord_" + f.Licence.ID, Code: "KGRENEW1", Kind: in.Kind,
			LicenseID: in.LicenseID, PlanID: in.PlanID, TotalMinor: 250_000,
			PeriodStart: in.PeriodStart, PeriodEnd: in.PeriodEnd,
		}, Created: true}, nil
	}})

	if err := r.run(); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := len(r.mail.notices); got != 0 {
		t.Fatalf("a renewed subscription was reminded %d times", got)
	}
	if got := len(r.queued(f.Licence.Email)); got != 0 {
		t.Fatalf("a renewed subscription was queued %d mails", got)
	}
	if tags := r.notificationTags(f.Licence.ID); len(tags) != 0 {
		t.Fatalf("reminder tags = %v", tags)
	}
}

// An unconfigured mailer means no ladder, not a panic.
func TestRemindersWithoutMailerAreSkipped(t *testing.T) {
	r := newRig(t)
	f := r.seed(seedOpts{End: r.now.Add(2 * 24 * time.Hour), Price: 250_000})
	r.svc = billing.New(billing.Deps{
		Store: r.s, Checkout: r.checkout, Logger: discardLogger(),
		Now: func() time.Time { return r.now },
	})
	if err := r.svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := len(r.orders(f.Licence.ID)); got != 1 {
		t.Fatalf("orders = %d, want 1 even with no mailer", got)
	}
}
