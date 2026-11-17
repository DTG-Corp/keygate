package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
// It takes the caller's transaction so settlement's payment path can call
// it inside the write that extends the licence.
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

// ─── Phase 1b plan changes and trial conversion ───

// NextCycleUpgradeSessionPrefix marks the license_upgrades row of a
// next_cycle plan change. Such a row has no payment order — it is applied
// by the next renewal settlement — so the prefix (not the Stripe column)
// is what tells it apart from an immediate change whose checkout is open.
const NextCycleUpgradeSessionPrefix = "billing-next-cycle-"

var (
	// ErrUpgradeInvalid: the upgrade row or the order it names cannot be
	// applied (no such upgrade, wrong kind, wrong plan, a replay of a
	// closed change).
	ErrUpgradeInvalid = errors.New("upgrade is not applicable")
	// ErrUpgradeIneligible: the licence cannot change to this plan (wrong
	// product, inactive plan, an operator hold, an ended period).
	ErrUpgradeIneligible = errors.New("licence cannot change to this plan")
	// ErrUpgradeInProgress: the licence already has an open plan change.
	ErrUpgradeInProgress = errors.New("a plan change is already open for this licence")
	// ErrTrialConversionInvalid: the order is not a trial conversion, or
	// the trial licence it names cannot be converted.
	ErrTrialConversionInvalid = errors.New("trial conversion is not applicable")
)

// FindPendingUpgradeForLicense returns the licence's open plan change, or
// nil when there is none. The row is not locked: the caller only needs it
// to decide whether a retried request is the same change.
func (s *Store) FindPendingUpgradeForLicense(ctx context.Context, licenseID string) (*model.LicenseUpgrade, error) {
	up := new(model.LicenseUpgrade)
	err := s.DB.NewSelect().Model(up).
		Where("license_id = ?", licenseID).
		Where("state = ?", model.UpgradePending).
		Order("created_at DESC", "id DESC").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find pending upgrade: %w", err)
	}
	return up, nil
}

// FindPendingNextCycleUpgradeIn returns the next_cycle plan change a
// renewal settlement has to apply, or nil. It locks the row: the renewal
// that applies it and any concurrent write serialise on this read.
func (s *Store) FindPendingNextCycleUpgradeIn(ctx context.Context, db bun.IDB, licenseID string) (*model.LicenseUpgrade, error) {
	up := new(model.LicenseUpgrade)
	err := db.NewSelect().Model(up).
		Where("license_id = ?", licenseID).
		Where("state = ?", model.UpgradePending).
		Where("payment_order_id IS NULL").
		Where("stripe_checkout_session_id LIKE ?", NextCycleUpgradeSessionPrefix+"%").
		Order("created_at DESC", "id DESC").Limit(1).For("UPDATE").Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find pending next-cycle upgrade: %w", err)
	}
	return up, nil
}

// CreateUpgradeForOrderIn writes the pending plan change an immediate
// upgrade order pays for, linked by payment_order_id. The caller has
// already created the order; a unique violation means another change
// already owns it.
func (s *Store) CreateUpgradeForOrderIn(ctx context.Context, db bun.IDB, u *model.LicenseUpgrade) error {
	if u == nil || u.PaymentOrderID == "" {
		return fmt.Errorf("%w: an upgrade row for an order needs payment_order_id", ErrUpgradeInvalid)
	}
	if u.ID == "" {
		u.ID = newID()
	}
	u.State = model.UpgradePending
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	if _, err := db.NewInsert().Model(u).Exec(ctx); err != nil {
		if IsUniqueViolation(err) {
			return fmt.Errorf("%w: order %s already pays for a plan change", ErrUpgradeInvalid, u.PaymentOrderID)
		}
		return fmt.Errorf("create upgrade for order: %w", err)
	}
	return nil
}

// LinkUpgradeOrderIn points a pending upgrade at the order that now pays
// for it. Its previous order was left behind (expired checkout), so no
// settlement can apply that one.
func (s *Store) LinkUpgradeOrderIn(ctx context.Context, db bun.IDB, upgradeID, orderID string) error {
	res, err := db.NewRaw(`UPDATE license_upgrades SET payment_order_id = ?
		WHERE id = ? AND state = 'pending'`, orderID, upgradeID).Exec(ctx)
	if err != nil {
		if IsUniqueViolation(err) {
			return fmt.Errorf("%w: order %s already pays for a plan change", ErrUpgradeInvalid, orderID)
		}
		return fmt.Errorf("link upgrade order: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: upgrade %s is not pending", ErrUpgradeInvalid, upgradeID)
	}
	return nil
}

// FindUpgradeByPaymentOrderIn returns the plan change an order pays for,
// locking the row for the settlement's transaction. A paid upgrade order
// without a row is ErrUpgradeInvalid: the money must not be granted to
// whatever plan the order names.
func (s *Store) FindUpgradeByPaymentOrderIn(ctx context.Context, db bun.IDB, orderID string) (*model.LicenseUpgrade, error) {
	up := new(model.LicenseUpgrade)
	err := db.NewSelect().Model(up).
		Where("payment_order_id = ?", orderID).
		Limit(1).For("UPDATE").Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: order %s pays for no plan change", ErrUpgradeInvalid, orderID)
	}
	if err != nil {
		return nil, fmt.Errorf("find upgrade by payment order: %w", err)
	}
	return up, nil
}

// ApplyUpgradeFromOrderIn applies the immediate plan change an upgrade
// order paid for, on the settlement's transaction: the licence moves to
// the target plan, the dunning state clears and the subscription follows,
// while valid_until stays at the period the order paid for.
//
// It is idempotent-safe: an already-applied change is returned untouched,
// which is what a duplicated provider webhook needs.
func (s *Store) ApplyUpgradeFromOrderIn(ctx context.Context, db bun.IDB, order *model.PaymentOrder, plan *model.Plan) (*model.LicenseUpgrade, error) {
	if order == nil || plan == nil {
		return nil, fmt.Errorf("%w: order and plan are required", ErrUpgradeInvalid)
	}
	if order.Kind != "upgrade" {
		return nil, fmt.Errorf("%w: order %s is a %s order", ErrUpgradeInvalid, order.Code, order.Kind)
	}
	if order.LicenseID == "" {
		return nil, fmt.Errorf("%w: order %s names no licence", ErrUpgradeInvalid, order.Code)
	}
	if plan.ID != order.PlanID {
		return nil, fmt.Errorf("%w: order %s is for plan %s, not %s", ErrUpgradeInvalid, order.Code, order.PlanID, plan.ID)
	}
	lic := new(model.License)
	if err := db.NewSelect().Model(lic).Where("id = ?", order.LicenseID).For("UPDATE").Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: licence %s not found", ErrUpgradeIneligible, order.LicenseID)
		}
		return nil, err
	}
	if plan.ProductID != lic.ProductID || !plan.Active {
		return nil, fmt.Errorf("%w: plan %s is not for sale on licence %s", ErrUpgradeIneligible, plan.ID, lic.ID)
	}
	up, err := s.FindUpgradeByPaymentOrderIn(ctx, db, order.ID)
	if err != nil {
		return nil, err
	}
	if up.State == model.UpgradeApplied {
		return up, nil
	}
	if up.State != model.UpgradePending {
		return nil, fmt.Errorf("%w: upgrade %s is %s", ErrUpgradeInvalid, up.ID, up.State)
	}
	if up.LicenseID != lic.ID || up.ToPlanID != plan.ID {
		return nil, fmt.Errorf("%w: upgrade %s does not name licence %s on plan %s", ErrUpgradeInvalid, up.ID, lic.ID, plan.ID)
	}
	// Snapshot the licence as the change found it before the UPDATE.
	if up.FromPlanID == "" {
		up.FromPlanID = lic.PlanID
	}
	if up.FromStatus == "" {
		up.FromStatus = lic.Status
	}
	if up.FromValidUntil == nil {
		up.FromValidUntil = lic.ValidUntil
	}
	if up.FromUpdatesUntil == nil {
		up.FromUpdatesUntil = lic.UpdatesUntil
	}
	if up.FromPaymentProvider == "" {
		up.FromPaymentProvider = lic.PaymentProvider
	}
	if up.FromCanceledAt == nil {
		up.FromCanceledAt = lic.CanceledAt
	}
	if up.FromQuantity < 1 {
		up.FromQuantity = lic.Quantity
	}
	now := time.Now()
	if _, err := db.NewUpdate().Model((*model.License)(nil)).
		Set("plan_id = ?", plan.ID).
		Set("status = ?", model.StatusActive).
		Set("past_due_at = NULL").
		Set("suspended_at = NULL").
		Set("suspended_by = NULL").
		Set("updated_at = ?", now).
		Where("id = ?", lic.ID).Exec(ctx); err != nil {
		return nil, fmt.Errorf("apply upgrade to licence: %w", err)
	}
	// The period is untouched: the customer already paid for it and the
	// upgrade only changes which plan it buys. The next renewal prices the
	// new plan.
	if _, err := db.NewRaw(`UPDATE subscriptions
		SET plan_id = ?, status = ?, updated_at = ?
		WHERE license_id = ?`, plan.ID, model.StatusActive, now, lic.ID).Exec(ctx); err != nil {
		return nil, fmt.Errorf("apply upgrade to subscription: %w", err)
	}
	up.ToPlanID = plan.ID
	up.State = model.UpgradeApplied
	up.AppliedAt = &now
	up.PaymentOrderID = order.ID
	if _, err := db.NewUpdate().Model(up).Column(
		"state", "applied_at", "to_plan_id", "from_plan_id", "from_status", "from_valid_until",
		"from_updates_until", "from_updates_terms_set", "from_payment_provider", "from_canceled_at",
		"from_quantity", "payment_order_id",
	).WherePK().Exec(ctx); err != nil {
		return nil, fmt.Errorf("mark upgrade applied: %w", err)
	}
	return up, nil
}

// ApplyNextCycleUpgradeIn applies a next_cycle plan change during a
// renewal settlement: the licence and its subscription move to the target
// plan, the new price applies from this renewal on, and the change is
// marked applied. It charges nothing (there is no order).
func (s *Store) ApplyNextCycleUpgradeIn(ctx context.Context, db bun.IDB, u *model.LicenseUpgrade) error {
	if u == nil {
		return fmt.Errorf("%w: no upgrade", ErrUpgradeInvalid)
	}
	if u.State == model.UpgradeApplied {
		return nil
	}
	if u.State != model.UpgradePending || u.PaymentOrderID != "" {
		return fmt.Errorf("%w: upgrade %s is not an open next-cycle change", ErrUpgradeInvalid, u.ID)
	}
	lic := new(model.License)
	if err := db.NewSelect().Model(lic).Where("id = ?", u.LicenseID).For("UPDATE").Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: licence %s not found", ErrUpgradeIneligible, u.LicenseID)
		}
		return err
	}
	if u.FromPlanID == "" {
		u.FromPlanID = lic.PlanID
	}
	if u.FromStatus == "" {
		u.FromStatus = lic.Status
	}
	if u.FromValidUntil == nil {
		u.FromValidUntil = lic.ValidUntil
	}
	if u.FromQuantity < 1 {
		u.FromQuantity = lic.Quantity
	}
	now := time.Now()
	if _, err := db.NewUpdate().Model((*model.License)(nil)).
		Set("plan_id = ?", u.ToPlanID).
		Set("updated_at = ?", now).
		Where("id = ?", lic.ID).Exec(ctx); err != nil {
		return fmt.Errorf("apply next-cycle upgrade to licence: %w", err)
	}
	if _, err := db.NewRaw(`UPDATE subscriptions SET plan_id = ?, status = ?, updated_at = ?
		WHERE license_id = ?`, u.ToPlanID, model.StatusActive, now, lic.ID).Exec(ctx); err != nil {
		return fmt.Errorf("apply next-cycle upgrade to subscription: %w", err)
	}
	u.State = model.UpgradeApplied
	u.AppliedAt = &now
	if _, err := db.NewUpdate().Model(u).Column(
		"state", "applied_at", "to_plan_id", "from_plan_id", "from_status", "from_valid_until",
		"from_quantity",
	).WherePK().Exec(ctx); err != nil {
		return fmt.Errorf("mark next-cycle upgrade applied: %w", err)
	}
	return nil
}

// ConvertTrialLicenseFromOrderIn converts the trial licence a paid
// trial-conversion order names, in place, on the settlement's
// transaction: the licence becomes active on the order's plan, its first
// paid period starts at trial_end, and the subscription loses its trial
// dates. It returns the converted licence — no second licence is minted.
//
// The order cannot name the licence in license_id (it is a kind new
// order), so metadata.billing.trial_license_id does. A missing or
// mismatched trial licence is ErrTrialConversionInvalid: the money must
// not create a second licence for a trial that is already paid for.
func (s *Store) ConvertTrialLicenseFromOrderIn(ctx context.Context, db bun.IDB, order *model.PaymentOrder, plan *model.Plan) (*model.License, error) {
	if order == nil || plan == nil {
		return nil, fmt.Errorf("%w: order and plan are required", ErrTrialConversionInvalid)
	}
	if order.Kind != "new" {
		return nil, fmt.Errorf("%w: order %s is a %s order", ErrTrialConversionInvalid, order.Code, order.Kind)
	}
	var meta struct {
		Billing struct {
			Source         string `json:"source"`
			TrialLicenseID string `json:"trial_license_id"`
			TrialEnd       string `json:"trial_end"`
		} `json:"billing"`
	}
	if err := json.Unmarshal(order.Metadata, &meta); err != nil {
		return nil, fmt.Errorf("%w: order %s metadata is not JSON", ErrTrialConversionInvalid, order.Code)
	}
	if meta.Billing.Source != "trial_conversion" || meta.Billing.TrialLicenseID == "" {
		return nil, fmt.Errorf("%w: order %s is not a trial conversion", ErrTrialConversionInvalid, order.Code)
	}
	if plan.ID != order.PlanID || plan.ProductID != order.ProductID || !plan.Active {
		return nil, fmt.Errorf("%w: plan %s is not the plan the order sells", ErrTrialConversionInvalid, plan.ID)
	}
	lic := new(model.License)
	if err := db.NewSelect().Model(lic).Where("id = ?", meta.Billing.TrialLicenseID).For("UPDATE").Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: trial licence %s not found", ErrTrialConversionInvalid, meta.Billing.TrialLicenseID)
		}
		return nil, err
	}
	if lic.ProductID != order.ProductID {
		return nil, fmt.Errorf("%w: trial licence %s belongs to another product", ErrTrialConversionInvalid, lic.ID)
	}
	var planType, billingInterval string
	var planTrialDays int
	if err := db.NewRaw("SELECT license_type, trial_days, billing_interval FROM plans WHERE id = ?", plan.ID).
		Scan(ctx, &planType, &planTrialDays, &billingInterval); err != nil {
		return nil, fmt.Errorf("read plan %s: %w", plan.ID, err)
	}
	trialEnd := trialEndFromConversion(ctx, db, lic, meta.Billing.TrialEnd)
	end := trialEnd.Add(paidOrderInterval(planType, planTrialDays, billingInterval))
	// A retry that finds the conversion already committed returns the
	// licence rather than failing the payment.
	if lic.Status != model.StatusTrialing {
		if lic.Status == model.StatusActive && lic.PlanID == plan.ID &&
			lic.ValidUntil != nil && lic.ValidUntil.Equal(end) {
			return lic, nil
		}
		return nil, fmt.Errorf("%w: licence %s is %s, not trialing", ErrTrialConversionInvalid, lic.ID, lic.Status)
	}
	quantity := order.Quantity
	if quantity < 1 {
		quantity = lic.Quantity
	}
	if quantity < 1 {
		quantity = 1
	}
	now := time.Now()
	if _, err := db.NewUpdate().Model((*model.License)(nil)).
		Set("plan_id = ?", plan.ID).
		Set("status = ?", model.StatusActive).
		Set("quantity = ?", quantity).
		Set("valid_until = ?", end).
		Set("payment_provider = ?", order.Provider).
		Set("past_due_at = NULL").
		Set("updated_at = ?", now).
		Where("id = ?", lic.ID).Exec(ctx); err != nil {
		return nil, fmt.Errorf("convert trial licence: %w", err)
	}
	// The subscription keeps its payment provider and external reference:
	// the rail did not change, only the plan and the trial dates.
	if _, err := db.NewRaw(`UPDATE subscriptions
		SET plan_id = ?, status = ?, current_period_start = ?, current_period_end = ?,
		    trial_start = NULL, trial_end = NULL, updated_at = ?
		WHERE license_id = ?`,
		plan.ID, model.StatusActive, trialEnd, end, now, lic.ID).Exec(ctx); err != nil {
		return nil, fmt.Errorf("convert trial subscription: %w", err)
	}
	lic.PlanID, lic.Status, lic.Quantity = plan.ID, model.StatusActive, quantity
	lic.ValidUntil, lic.PaymentProvider, lic.PastDueAt = &end, order.Provider, nil
	return lic, nil
}

// trialEndFromConversion is the instant the paid period starts: the
// order's reserved trial_end, else the subscription's, else the trial
// licence's own end, else now (a conversion with no recorded end still
// starts a full period rather than failing the payment).
func trialEndFromConversion(ctx context.Context, db bun.IDB, lic *model.License, reserved string) time.Time {
	if reserved != "" {
		if t, err := time.Parse(time.RFC3339, reserved); err == nil {
			return t.UTC()
		}
	}
	var subTrialEnd *time.Time
	err := db.NewRaw(`SELECT trial_end FROM subscriptions WHERE license_id = ?
		ORDER BY created_at DESC LIMIT 1`, lic.ID).Scan(ctx, &subTrialEnd)
	if err == nil && subTrialEnd != nil {
		return subTrialEnd.UTC()
	}
	if lic.ValidUntil != nil {
		return lic.ValidUntil.UTC()
	}
	return time.Now().UTC()
}
