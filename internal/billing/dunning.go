package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// dunningReason is the audit and webhook reason for everything the
// ladder decides. It distinguishes the engine's expiry from an operator's
// and from the generic grace sweep's.
const dunningReason = "billing_dunning"

// dun runs the post-expiry ladder for one licence: past_due at the
// period end, grace notices on days 1, 3 and 7, suspension at the end of
// the plan's grace window, expiry after a hold on the suspended licence.
//
// The order of the checks is the ladder: expiry outranks suspension,
// which outranks the grace notices. Each step is a conditional write, so
// a payment that landed while the pass ran wins over the pass.
func (s *Service) dun(ctx context.Context, sub *model.Subscription, lic *model.License, now time.Time) error {
	if sub == nil || sub.CurrentPeriodEnd == nil || !liveLicence(lic) {
		return nil
	}
	end := sub.CurrentPeriodEnd.UTC()
	graceEnd := end.AddDate(0, 0, graceDays(sub.Plan))
	expiry := graceEnd.Add(holdDuration(sub.Plan))

	if !now.Before(expiry) {
		_, err := s.expireIfDue(ctx, sub, lic, now)
		return err
	}
	if !now.Before(graceEnd) {
		return s.suspendIfDue(ctx, sub, lic, now, end)
	}
	// Inside the grace window the customer must have something to pay, so
	// the order is (re)created before the mail that links to the portal.
	if err := s.ensureRenewalOrder(ctx, sub, lic, now); err != nil {
		return err
	}
	return s.markPastDueAndMail(ctx, sub, lic, now, end, graceEnd)
}

// markPastDueAndMail moves the licence into dunning once and mails the
// due notice or the most urgent grace rung reached. The due notice owns
// the first day after the period end; the rungs start on day 1.
func (s *Service) markPastDueAndMail(ctx context.Context, sub *model.Subscription, lic *model.License, now, end, graceEnd time.Time) error {
	marked, err := s.deps.Store.MarkBillingPastDue(ctx, lic, sub, now)
	if err != nil {
		return err
	}
	if marked {
		s.audit(ctx, lic, "past_due", map[string]any{"reason": dunningReason, "period_end": end})
	}
	// Re-read before mailing. The write was conditional, and a payment
	// may have landed between the scan and here: the customer must not
	// be told they are late for a period they already renewed.
	fresh, err := s.deps.Store.BillingSubscriptionOrNil(ctx, lic.ID)
	if err != nil {
		return err
	}
	if fresh == nil || fresh.License == nil || fresh.CurrentPeriodEnd == nil ||
		!fresh.CurrentPeriodEnd.Equal(end) || fresh.License.Status != model.StatusPastDue {
		return nil
	}
	if s.deps.Email == nil {
		return nil
	}
	daysLate := ceilClock(end, now)
	if daysLate <= 1 {
		tag := "renewal_due:" + end.Format(time.RFC3339)
		return s.queueMail(ctx, fresh.License, tag, func() (string, string) {
			return s.deps.Email.RenderRenewalOverdue(
				productName(fresh.License), planName(sub.Plan), s.portalURL(), end)
		})
	}
	rung := graceRung(daysLate)
	if rung == 0 {
		return nil
	}
	daysLeft := ceilClock(now, graceEnd)
	tag := fmt.Sprintf("renewal_grace_%d:%s", rung, end.Format(time.RFC3339))
	return s.queueMail(ctx, fresh.License, tag, func() (string, string) {
		return s.deps.Email.RenderRenewalGrace(
			productName(fresh.License), planName(sub.Plan), daysLeft, s.portalURL(), end)
	})
}

// graceRung is the most urgent grace notice a pass that is daysLate days
// late has reached. The first day belongs to the due notice, so the
// rungs start at one day late.
func graceRung(daysLate int) int {
	switch {
	case daysLate <= graceRungOne:
		return 0
	case daysLate < graceRungMid:
		return 1
	case daysLate < graceRungMax:
		return 3
	default:
		return 7
	}
}

// suspendIfDue suspends the licence when the grace window is over and
// mails the suspension once. A licence an operator holds is left alone
// by the conditional write, and then no mail goes out either.
func (s *Service) suspendIfDue(ctx context.Context, sub *model.Subscription, lic *model.License, now, end time.Time) error {
	suspended, err := s.deps.Store.SuspendBillingLicense(ctx, lic, sub, now)
	if err != nil {
		return err
	}
	if suspended {
		s.audit(ctx, lic, "suspended", map[string]any{"reason": dunningReason, "period_end": end})
		s.logger.Info("billing licence suspended", "license_id", lic.ID, "period_end", end)
	}
	fresh, err := s.deps.Store.BillingSubscriptionOrNil(ctx, lic.ID)
	if err != nil {
		return err
	}
	if fresh == nil || fresh.License == nil || fresh.CurrentPeriodEnd == nil ||
		!fresh.CurrentPeriodEnd.Equal(end) {
		return nil
	}
	if fresh.License.Status != model.StatusSuspended || fresh.License.SuspendedBy != store.BillingSuspendedBy {
		return nil
	}
	if s.deps.Email == nil {
		return nil
	}
	tag := "renewal_suspended:" + end.Format(time.RFC3339)
	return s.queueMail(ctx, fresh.License, tag, func() (string, string) {
		return s.deps.Email.RenderRenewalSuspension(
			productName(fresh.License), planName(sub.Plan), s.portalURL(), end)
	})
}

// expireIfDue ends the licence once the hold after the grace window is
// over. The conditional write refuses an operator's suspension and a
// licence a payment extended; both report false and nothing happens.
func (s *Service) expireIfDue(ctx context.Context, sub *model.Subscription, lic *model.License, now time.Time) (bool, error) {
	if sub == nil || sub.CurrentPeriodEnd == nil {
		return false, nil
	}
	// The deadline check lives here, not only in dun: the generic grace
	// sweep calls this seam at valid_until + grace, which is earlier than
	// the end of the dunning hold.
	end := sub.CurrentPeriodEnd.UTC()
	if now.Before(end.AddDate(0, 0, graceDays(sub.Plan)).Add(holdDuration(sub.Plan))) {
		return false, nil
	}
	expired, err := s.deps.Store.ExpireBillingLicense(ctx, lic, sub, now)
	if err != nil || !expired {
		return false, err
	}
	s.audit(ctx, lic, "expired", map[string]any{"reason": dunningReason, "period_end": sub.CurrentPeriodEnd})
	if s.deps.Webhook != nil {
		s.deps.Webhook.Dispatch(ctx, lic.ProductID, model.EventLicenseExpired, map[string]any{
			"license_id": lic.ID,
			"email":      lic.Email,
			"reason":     dunningReason,
		})
	}
	s.logger.Info("billing licence expired", "license_id", lic.ID, "period_end", sub.CurrentPeriodEnd)
	return true, nil
}

// EndGraceExpiredLicense is the billing engine's say in the generic
// grace sweep (service.ExpiryChecker.EndGraceExpiredUpgrade). A licence
// with a live SePay or PayOS subscription belongs to this engine: the
// sweep must not expire it on the licence's valid_until alone, because the
// dunning hold runs past that. The engine's own hourly pass does the
// expiry, with the webhook that goes with it. A closed subscription is
// left to the sweep.
func (s *Service) EndGraceExpiredLicense(ctx context.Context, lic *model.License, cutoff time.Time) (bool, error) {
	if lic == nil || s.deps.Store == nil {
		return false, nil
	}
	sub, err := s.deps.Store.BillingSubscriptionOrNil(ctx, lic.ID)
	if err != nil {
		return false, err
	}
	if !billingManaged(sub) {
		return false, nil
	}
	_, err = s.expireIfDue(ctx, sub, lic, s.deps.Now().UTC())
	return true, err
}

// billingManaged reports whether the subscription is one this engine is
// still driving. Stripe subscriptions keep their own lifecycle and are
// never read or written here. A subscription the engine has closed (a
// cancel-at-period-end that took effect, a lapsed trial) is no longer in
// the scan, so its licence belongs to the platform's grace sweep again:
// claiming it here would park the licence outside every sweep.
func billingManaged(sub *model.Subscription) bool {
	if sub == nil {
		return false
	}
	switch sub.PaymentProvider {
	case "sepay", "payos":
	default:
		return false
	}
	switch sub.Status {
	case model.StatusActive, model.StatusTrialing, model.StatusPastDue:
		return true
	}
	return false
}

// audit records a lifecycle decision the engine made, with the system as
// the actor, the same trail an operator's change leaves.
func (s *Service) audit(ctx context.Context, lic *model.License, action string, changes map[string]any) {
	if lic == nil {
		return
	}
	s.deps.Store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: lic.ID, Action: action,
		ActorType: "system", Changes: changes,
	})
}
