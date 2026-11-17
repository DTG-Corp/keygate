package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
)

// BillingSuspendedBy marks a suspension the billing engine made. Payment
// lifts only these, the engine's expiry tail expires only these, and an
// operator's suspension is never touched by either: suspending and
// reinstating a licence by hand stays a decision of the operator.
const BillingSuspendedBy = "billing"

// renewalOnceIndex is the partial unique index that allows one pending
// renewal order per (license_id, period_end); the migration declares it
// on payment_orders.
const renewalOnceIndex = "payment_orders_renewal_once"

// IsRenewalOrderConflict reports whether err is the database refusing a
// second pending renewal for the same licence and period. Two workers
// racing on one due subscription end here, and the loser's answer is the
// winner's order, not a new one.
func IsRenewalOrderConflict(err error) bool {
	return isUniqueViolationOn(err, renewalOnceIndex)
}

// billingNewestPerLicence keeps one row per licence. subscriptions has no
// unique key on license_id (a licence may keep its history), so a scan
// that read every row would process one licence several times and could
// act on a superseded period.
const billingNewestPerLicence = `subscription.id = (
	SELECT s2.id FROM subscriptions s2
	 WHERE s2.license_id = subscription.license_id
	 ORDER BY s2.created_at DESC, s2.id DESC
	 LIMIT 1)`

// ListBillingSubscriptions returns every licence the billing engine
// manages whose current period ends within lead, plus the licences this
// engine suspended, whose period-end is long past but whose expiry has no
// other writer (the generic grace sweep only looks at active and
// past_due rows). A trial is scanned on its trial end, not on the period
// end: the paid period it converts into ends a month later, long after the
// conversion window.
//
// Only VN-billed subscriptions are returned: a Stripe subscription is
// Stripe's to renew, and phase 1b must never touch it.
func (s *Store) ListBillingSubscriptions(ctx context.Context, now time.Time, lead time.Duration) ([]*model.Subscription, error) {
	subs := []*model.Subscription{}
	err := s.DB.NewSelect().Model(&subs).
		Relation("Plan").
		Relation("License").
		Relation("License.Product").
		Where("subscription.status IN (?, ?, ?)",
			model.StatusActive, model.StatusTrialing, model.StatusPastDue).
		Where("subscription.payment_provider IN (?, ?)", "sepay", "payos").
		Where("subscription.current_period_end IS NOT NULL").
		Where(billingNewestPerLicence).
		Where(`subscription.license_id IN (
			SELECT l.id FROM licenses l
			 WHERE l.status IN (?, ?, ?)
			    OR (l.status = ? AND l.suspended_by = ?))`,
			model.StatusActive, model.StatusTrialing, model.StatusPastDue,
			model.StatusSuspended, BillingSuspendedBy).
		Where(`subscription.current_period_end <= ?
			OR (subscription.status = ? AND subscription.trial_end <= ?)
			OR EXISTS (SELECT 1 FROM licenses l
			            WHERE l.id = subscription.license_id
			              AND l.status = ? AND l.suspended_by = ?)`,
			now.Add(lead), model.StatusTrialing, now.Add(lead),
			model.StatusSuspended, BillingSuspendedBy).
		OrderExpr("subscription.current_period_end ASC, subscription.id ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("list billing subscriptions: %w", err)
	}
	return subs, nil
}

// GetBillingSubscription reads one licence's current subscription with
// the relations a billing pass needs, newest row first. A caller that has
// just created an order reads through this again before mailing, so a
// payment that landed during the pass is seen.
func (s *Store) GetBillingSubscription(ctx context.Context, licenseID string) (*model.Subscription, error) {
	sub := new(model.Subscription)
	err := s.DB.NewSelect().Model(sub).
		Relation("Plan").
		Relation("License").
		Relation("License.Product").
		Where("subscription.license_id = ?", licenseID).
		OrderExpr("subscription.created_at DESC, subscription.id DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("get billing subscription: %w", err)
	}
	return sub, nil
}

// HasPaidTrialConversion reports whether a paid order already converted
// this trial. A trial-conversion order is kind new (spec §3.4), so it
// cannot name the trial licence in license_id; the metadata does instead,
// and it is the only record that the trial has been paid for.
func (s *Store) HasPaidTrialConversion(ctx context.Context, licenseID string) (bool, error) {
	var paid bool
	err := s.DB.NewRaw(`SELECT EXISTS (
		SELECT 1 FROM payment_orders
		 WHERE kind = ? AND status = ?
		   AND metadata -> 'billing' ->> 'trial_license_id' = ?
	)`, "new", domain.OrderPaid, licenseID).Scan(ctx, &paid)
	if err != nil {
		return false, fmt.Errorf("has paid trial conversion: %w", err)
	}
	return paid, nil
}

// FindOpenRenewalOrderForPeriod returns the renewal order that already
// covers a period, paid or still waiting for money. Either means the
// engine must not create another one.
func (s *Store) FindOpenRenewalOrderForPeriod(ctx context.Context, licenseID string, periodEnd time.Time) (*model.PaymentOrder, error) {
	order := new(model.PaymentOrder)
	err := s.DB.NewSelect().Model(order).
		Where("kind = ?", "renewal").
		Where("license_id = ?", licenseID).
		Where("period_end = ?", periodEnd).
		Where("status IN (?, ?)", domain.OrderPending, domain.OrderPaid).
		OrderExpr("created_at DESC, id DESC").
		Limit(1).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("find open renewal order: %w", err)
	}
	return order, nil
}

// MarkBillingPastDue moves a licence into dunning and stamps past_due_at
// once. The stamp anchors every later rung, so a second pass must not move
// it; the period guard makes a payment that landed after the scan win over
// the scan.
func (s *Store) MarkBillingPastDue(ctx context.Context, lic *model.License, sub *model.Subscription, now time.Time) (ok bool, err error) {
	if lic == nil || sub == nil || sub.CurrentPeriodEnd == nil {
		return false, nil
	}
	err = RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewUpdate().Model((*model.License)(nil)).
			Set("status = ?", model.StatusPastDue).
			Set("past_due_at = ?", now).
			Set("updated_at = ?", now).
			Where("id = ?", lic.ID).
			Where("past_due_at IS NULL").
			Where("status IN (?, ?)", model.StatusActive, model.StatusTrialing).
			Where("valid_until IS NULL OR valid_until <= ?", *sub.CurrentPeriodEnd).
			Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		ok = true
		_, err = tx.NewUpdate().Model((*model.Subscription)(nil)).
			Set("status = ?", model.StatusPastDue).
			Set("updated_at = ?", now).
			Where("id = ?", sub.ID).
			Exec(ctx)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("mark billing past due: %w", err)
	}
	return ok, nil
}

// SuspendBillingLicense suspends a licence this engine manages, after its
// grace window ran out. Only a live licence is suspended, and only while
// nothing has extended it; an operator's suspension is left as it is.
func (s *Store) SuspendBillingLicense(ctx context.Context, lic *model.License, sub *model.Subscription, now time.Time) (ok bool, err error) {
	if lic == nil || sub == nil || sub.CurrentPeriodEnd == nil {
		return false, nil
	}
	err = RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewUpdate().Model((*model.License)(nil)).
			Set("status = ?", model.StatusSuspended).
			Set("suspended_at = ?", now).
			Set("suspended_by = ?", BillingSuspendedBy).
			Set("past_due_at = COALESCE(past_due_at, ?)", now).
			Set("updated_at = ?", now).
			Where("id = ?", lic.ID).
			Where("status IN (?, ?)", model.StatusActive, model.StatusPastDue).
			Where("valid_until IS NULL OR valid_until <= ?", *sub.CurrentPeriodEnd).
			Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		ok = true
		_, err = tx.NewUpdate().Model((*model.Subscription)(nil)).
			Set("status = ?", model.StatusPastDue).
			Set("updated_at = ?", now).
			Where("license_id = ?", lic.ID).
			Where("status IN (?, ?)", model.StatusActive, model.StatusTrialing).
			Exec(ctx)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("suspend billing licence: %w", err)
	}
	return ok, nil
}

// ExpireBillingLicense ends a licence whose dunning window is over: a
// past_due licence, or one this engine suspended. An operator's
// suspension is not expired here, and a licence a payment extended is not
// expired either — the period guard does that.
func (s *Store) ExpireBillingLicense(ctx context.Context, lic *model.License, sub *model.Subscription, now time.Time) (ok bool, err error) {
	if lic == nil || sub == nil || sub.CurrentPeriodEnd == nil {
		return false, nil
	}
	err = RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewUpdate().Model((*model.License)(nil)).
			Set("status = ?", model.StatusExpired).
			Set("updated_at = ?", now).
			Where("id = ?", lic.ID).
			Where(`status IN (?, ?) OR (status = ? AND suspended_by = ?)`,
				model.StatusPastDue, model.StatusActive, model.StatusSuspended, BillingSuspendedBy).
			Where("valid_until IS NULL OR valid_until <= ?", *sub.CurrentPeriodEnd).
			Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		ok = true
		// The same tail an operator's expire writes: floating seats end
		// with the licence and the subscription mirrors the status.
		if err := endFloatingSessionsIfStoppedIn(ctx, tx, lic.ID, model.StatusExpired); err != nil {
			return err
		}
		return syncSubscriptionStatus(ctx, tx, lic.ID, model.StatusExpired)
	})
	if err != nil {
		return false, fmt.Errorf("expire billing licence: %w", err)
	}
	return ok, nil
}

// CancelSubscriptionAtPeriodEnd turns a cancel-at-period-end flag into the
// cancellation itself, once the paid period is over. The licence is left
// alone on purpose: its valid_until is the end of the period the customer
// paid for, and the generic grace sweep owns what happens after that.
func (s *Store) CancelSubscriptionAtPeriodEnd(ctx context.Context, sub *model.Subscription, now time.Time) (ok bool, err error) {
	if sub == nil {
		return false, nil
	}
	res, err := s.DB.NewUpdate().Model((*model.Subscription)(nil)).
		Set("status = ?", model.StatusCanceled).
		Set("canceled_at = COALESCE(current_period_end, ?)", now).
		Set("updated_at = ?", now).
		Where("id = ?", sub.ID).
		Where("cancel_at_period_end").
		Where("status IN (?, ?)", model.StatusActive, model.StatusPastDue).
		Where("current_period_end IS NOT NULL AND current_period_end <= ?", now).
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("cancel subscription at period end: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReinstateLicenseForRenewalIn clears the dunning state a successful
// renewal order leaves behind, in the caller's transaction: past due, and
// a suspension this engine made. An operator's suspension stays — money
// does not override an explicit hold.
//
// It is a package function so settlement's payment path can call it
// inside the transaction that extends the licence.
func (s *Store) ReinstateLicenseForRenewalIn(ctx context.Context, db bun.IDB, licenseID string) (bool, error) {
	res, err := db.NewUpdate().Model((*model.License)(nil)).
		Set("status = ?", model.StatusActive).
		Set("past_due_at = NULL").
		Set("suspended_at = NULL").
		Set("suspended_by = NULL").
		Set("updated_at = now()").
		Where("id = ?", licenseID).
		Where("suspended_by IS NULL OR suspended_by = ?", BillingSuspendedBy).
		Where("status IN (?, ?, ?)", model.StatusActive, model.StatusPastDue, model.StatusSuspended).
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("reinstate licence: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// BillingSubscriptionOrNil is GetBillingSubscription for callers that
// treat a missing row as "not managed by the billing engine".
func (s *Store) BillingSubscriptionOrNil(ctx context.Context, licenseID string) (*model.Subscription, error) {
	sub, err := s.GetBillingSubscription(ctx, licenseID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sub, err
}
