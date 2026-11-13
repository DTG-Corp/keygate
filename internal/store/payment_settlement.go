package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
)

// ErrRenewalNoLicense means a paid renewal order does not name the
// licence it extends. Guessing one from the email would extend the wrong
// customer, so the settlement is refused and the order is left for an
// operator.
var ErrRenewalNoLicense = errors.New("renewal order names no licence")

// PaidOrderLicenceInput is one paid new-order as the settlement built
// it. License carries the product, plan, email, key and status the
// caller decided; the helper fills its id, hash and, for a recurring
// plan, its first period and subscription row.
type PaidOrderLicenceInput struct {
	Order   *model.PaymentOrder
	Plan    *model.Plan
	License *model.License
}

// CreatePaidOrderLicence writes the licence a paid new-order bought and,
// for a subscription/trial plan, its subscription row, on the caller's
// transaction. It is additive: CreateLicenseWithSubscription keeps its
// own transaction for every other caller.
//
// The licence id is derived from the order id, the same derivation the
// settlement relies on: a retry after a crash lands on the row the first
// attempt committed instead of minting a second licence. The
// subscription carries the first period the order reserved (or the
// plan's interval), the provider that took the money and the order code
// as its external reference, which is what the billing engine and a
// later renewal read.
func (s *Store) CreatePaidOrderLicence(ctx context.Context, tx bun.IDB, in PaidOrderLicenceInput) (*model.License, *model.Subscription, error) {
	if in.Order == nil || in.Plan == nil || in.License == nil {
		return nil, nil, errors.New("create paid order licence: order, plan and licence are required")
	}
	db := bun.IDB(s.DB)
	if tx != nil {
		db = tx
	}
	lic := in.License
	// Derived, never random: see the doc comment.
	lic.ID = in.Order.ID
	if lic.PaymentProvider == "" {
		lic.PaymentProvider = in.Order.Provider
	}

	// The rows this insert references are locked first and the gating
	// state read after, in the same order CreateLicenseWithSubscription
	// uses, so the two writers cannot deadlock against each other.
	if err := LockReferencedRowsIn(ctx, db, in.Plan.ID, in.Order.ProductID); err != nil {
		return nil, nil, err
	}
	// The plan the caller read may have been retyped since the order was
	// priced. None of that can be repaired here, so the licence is not
	// written at all; the caller reads the plan again and rebuilds.
	var planType, billingInterval string
	var planTrialDays, planUpdatesDays int
	if err := db.NewRaw(
		"SELECT license_type, trial_days, updates_days, billing_interval FROM plans WHERE id = ?", in.Plan.ID).
		Scan(ctx, &planType, &planTrialDays, &planUpdatesDays, &billingInterval); err != nil {
		return nil, nil, fmt.Errorf("read plan %s: %w", in.Plan.ID, err)
	}
	if planType != in.Plan.LicenseType {
		return nil, nil, fmt.Errorf("%w: %s is now %s, not %s", ErrPlanChanged, in.Plan.ID, planType, in.Plan.LicenseType)
	}
	if planUpdatesDays != in.Plan.UpdatesDays {
		return nil, nil, fmt.Errorf("%w: %s now grants %d update days, not %d",
			ErrPlanChanged, in.Plan.ID, planUpdatesDays, in.Plan.UpdatesDays)
	}
	if planType == "trial" && planTrialDays != in.Plan.TrialDays {
		return nil, nil, fmt.Errorf("%w: %s now grants %d trial days, not %d",
			ErrPlanChanged, in.Plan.ID, planTrialDays, in.Plan.TrialDays)
	}
	// A trial licence is trialing, not active: the status is what the
	// expiry sweep and the trial-fallback read.
	if planType == "trial" {
		lic.Status = model.StatusTrialing
	} else if lic.Status == "" {
		lic.Status = model.StatusActive
	}

	// The first period belongs to the order when it reserved one, else
	// to the plan's interval. The subscription and the licence must
	// agree on where it ends, or the customer's access and the billing
	// engine's clock drift apart.
	var sub *model.Subscription
	if planType == "subscription" || planType == "trial" {
		start := time.Now()
		if in.Order.PeriodStart != nil {
			start = *in.Order.PeriodStart
		}
		end := time.Time{}
		switch {
		case in.Order.PeriodEnd != nil:
			end = *in.Order.PeriodEnd
		case lic.ValidUntil != nil:
			end = *lic.ValidUntil
		default:
			end = start.Add(paidOrderInterval(planType, planTrialDays, billingInterval))
		}
		if lic.ValidUntil == nil {
			v := end
			lic.ValidUntil = &v
		}
		sub = &model.Subscription{
			ID: newID(), LicenseID: lic.ID, PlanID: in.Plan.ID, Status: lic.Status,
			PaymentProvider:    in.Order.Provider,
			ExternalID:         in.Order.Code,
			CurrentPeriodStart: &start, CurrentPeriodEnd: &end,
		}
		if planType == "trial" {
			sub.TrialStart, sub.TrialEnd = &start, &end
		}
	}

	// A finite period is only written while the product's feeds can
	// enforce it, with the same checks CreateLicenseWithSubscription
	// makes; a settlement must not be the write that bypasses them.
	if lic.UpdatesUntil != nil {
		on, err := MaintenanceFeaturesEnabledForWriteIn(ctx, db)
		if err != nil {
			return nil, nil, err
		}
		if !on {
			return nil, nil, fmt.Errorf("%w: the maintenance features are switched off", ErrUpdatePeriodNotEnforceable)
		}
		if err := FeedGatingLockIn(ctx, db, lic.ProductID); err != nil {
			return nil, nil, err
		}
		ok, why, err := FeedsCanEnforcePeriodsIn(ctx, db, lic.ProductID, s.feedURLTTL())
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, fmt.Errorf("%w: %s", ErrUpdatePeriodNotEnforceable, why)
		}
	}

	// The id is set before encryption: the AAD is the licence id.
	if err := s.prepareLicenseForInsert(lic); err != nil {
		return nil, nil, err
	}
	if _, err := db.NewInsert().Model(lic).Exec(ctx); err != nil {
		return nil, nil, err
	}
	if sub != nil {
		if _, err := db.NewInsert().Model(sub).Exec(ctx); err != nil {
			return nil, nil, err
		}
	}
	return lic, sub, nil
}

// paidOrderInterval is the first period a recurring plan sells when the
// order reserved none: the trial length for a trial plan, otherwise the
// billing interval as the payment rails spell it.
func paidOrderInterval(planType string, trialDays int, billingInterval string) time.Duration {
	if planType == "trial" && trialDays > 0 {
		return time.Duration(trialDays) * 24 * time.Hour
	}
	if billingInterval == "year" {
		return 365 * 24 * time.Hour
	}
	return 30 * 24 * time.Hour
}

// ExtendLicenseFromOrderIn applies one paid renewal order to a licence.
// It covers both shapes the platform sells: a perpetual licence gets its
// maintenance window extended (and its ledger row written), and a
// subscription/trial licence gets its valid_until pushed by the paid
// period, with the subscription row kept in step. The license_renewals
// row carries payment_order_id, and the partial unique index on that
// column makes a replay a no-op instead of a double extension.
//
// The extension length comes from the order's reserved period when it
// has one; otherwise from the plan (its billing interval for a
// subscription, renewal_days for a perpetual one). The ledger's Days is
// that same length, so a refund or a replay recomputes the dates the
// settlement actually applied.
//
// A past_due licence is eligible: paying is exactly how a customer
// leaves dunning, and the extension write sets the licence active and
// clears past_due_at in the same statement.
//
// The licence row is locked before the ledger receipt is read: every
// settlement of this order serialises on that lock, so a concurrent
// retry either sees the committed renewal row (and returns) or waits and
// then sees it. Checking the ledger before taking the lock would let two
// transactions both read zero and both insert.
func ExtendLicenseFromOrderIn(ctx context.Context, tx bun.IDB, order *model.PaymentOrder, plan *model.Plan) error {
	if order == nil || plan == nil {
		return errors.New("extend licence: order and plan are required")
	}
	if order.LicenseID == "" {
		return fmt.Errorf("%w: order %s", ErrRenewalNoLicense, order.Code)
	}
	lic := new(model.License)
	if err := tx.NewSelect().Model(lic).Where("id = ?", order.LicenseID).For("UPDATE").Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: order %s names %s", ErrRenewalNoLicense, order.Code, order.LicenseID)
		}
		return err
	}
	// The ledger row keyed by the order is the receipt that this order
	// was already applied.
	var applied int
	if err := tx.NewRaw("SELECT count(*) FROM license_renewals WHERE payment_order_id = ?", order.ID).
		Scan(ctx, &applied); err != nil {
		return err
	}
	if applied > 0 {
		return nil
	}
	// A suspended, revoked or expired licence has nothing worth
	// extending; recording the renewal as fulfilled would take the money
	// and give nothing back. Past due is different: it is unpaid, and
	// this payment is the cure.
	if lic.Status != model.StatusActive && lic.Status != model.StatusPastDue {
		return ErrRenewalIneligible
	}
	// Read the plan's type and terms on this transaction: the plan the
	// caller read before the transaction may have been retyped since,
	// and the type decides whether there is a maintenance window to
	// extend or a valid_until to push.
	var planType string
	var renewalDays int
	var billingInterval string
	if err := tx.NewRaw(
		"SELECT license_type, renewal_days, billing_interval FROM plans WHERE id = ?", plan.ID).
		Scan(ctx, &planType, &renewalDays, &billingInterval); err != nil {
		return err
	}
	days, extension := renewalPeriod(order, planType, renewalDays, billingInterval)
	if planType == "perpetual" {
		return extendPerpetualFromOrder(ctx, tx, order, lic, days)
	}

	// Subscription/trial: the paid period continues from the current
	// end when it is still running, and from now when it has lapsed.
	now := time.Now()
	base := now
	if lic.ValidUntil != nil && lic.ValidUntil.After(now) {
		base = *lic.ValidUntil
	}
	end := base.Add(extension)
	// Paying revives a past_due licence in the same write that extends
	// it: a customer who has paid must not be left in dunning.
	if _, err := tx.NewUpdate().Model((*model.License)(nil)).
		Set("valid_until = ?", end).
		Set("status = ?", model.StatusActive).
		Set("past_due_at = NULL").
		Set("updated_at = now()").
		Where("id = ?", lic.ID).Exec(ctx); err != nil {
		return err
	}
	ledger := &model.LicenseRenewal{
		ID: newID(), LicenseID: lic.ID, PaymentOrderID: order.ID, Days: days, CreatedAt: now,
	}
	if _, err := tx.NewInsert().Model(ledger).Exec(ctx); err != nil {
		return err
	}
	// A subscription row exists for every subscription/trial licence
	// (CreateLicenseWithSubscription writes it). A licence moved to a
	// perpetual plan has none, and the UPDATE simply matches nothing.
	_, err := tx.NewUpdate().Model((*model.Subscription)(nil)).
		Set("current_period_start = ?", base).Set("current_period_end = ?", end).
		Set("status = ?", model.StatusActive).
		Set("updated_at = now()").
		Where("license_id = ?", lic.ID).Exec(ctx)
	return err
}

// extendPerpetualFromOrder writes the maintenance extension and the
// ledger row a paid perpetual renewal buys, and revives a past_due
// licence in the same licence UPDATE.
//
// It mirrors ApplyLicenseRenewalIn's write rather than calling it: that
// helper refuses a past_due licence and owns its own UPDATE, so a
// past_due renewal could not clear past_due_at in the statement that
// extends the window.
func extendPerpetualFromOrder(ctx context.Context, tx bun.IDB,
	order *model.PaymentOrder, lic *model.License, days int) error {
	// A licence with updates for life has no window a renewal could
	// extend; taking the money for nothing must not look like success.
	if lic.UpdatesUntil == nil {
		return ErrRenewalIneligible
	}
	now, err := ledgerStamp(ctx, tx)
	if err != nil {
		return err
	}
	end := model.RenewedUpdatesUntil(lic.UpdatesUntil, now, days)
	ledger := &model.LicenseRenewal{
		ID: newID(), LicenseID: lic.ID, PaymentOrderID: order.ID, Days: days,
		PreviousUpdatesUntil: lic.UpdatesUntil, UpdatesUntil: &end, CreatedAt: now,
	}
	if _, err := tx.NewInsert().Model(ledger).Exec(ctx); err != nil {
		return err
	}
	_, err = tx.NewUpdate().Model((*model.License)(nil)).
		Set("updates_until = ?", end).
		Set("status = ?", model.StatusActive).
		Set("past_due_at = NULL").
		Set("updated_at = now()").
		Where("id = ?", lic.ID).Exec(ctx)
	return err
}

// renewalPeriod is the number of days one renewal order buys and the
// exact length of the extension. The order's reserved period is
// authoritative when it has one; otherwise the plan's terms — its
// billing interval for a subscription, renewal_days for a perpetual —
// with a month as the last resort the rails assume.
func renewalPeriod(order *model.PaymentOrder, planType string, renewalDays int, billingInterval string) (int, time.Duration) {
	if order.PeriodStart != nil && order.PeriodEnd != nil {
		if d := order.PeriodEnd.Sub(*order.PeriodStart); d > 0 {
			return int(d / (24 * time.Hour)), d
		}
	}
	days := 30
	if planType == "perpetual" {
		if renewalDays > 0 {
			days = renewalDays
		}
	} else if billingInterval == "year" {
		days = 365
	}
	return days, time.Duration(days) * 24 * time.Hour
}

// FindLicenseRenewalByPaymentOrder reads the ledger row a paid renewal
// order produced, or sql.ErrNoRows when the order has not been applied.
func (s *Store) FindLicenseRenewalByPaymentOrder(ctx context.Context, orderID string) (*model.LicenseRenewal, error) {
	r := new(model.LicenseRenewal)
	err := s.DB.NewSelect().Model(r).Where("payment_order_id = ?", orderID).Limit(1).Scan(ctx)
	return r, err
}

// FindLicenseByIDIn reads a licence on the caller's transaction. The
// settlement uses it to find the row a crashed attempt may have
// committed before the order was stamped, without opening a second
// connection and deadlocking against its own lock.
func (s *Store) FindLicenseByIDIn(ctx context.Context, db bun.IDB, id string) (*model.License, error) {
	l := new(model.License)
	err := db.NewSelect().Model(l).Where("id = ?", id).Scan(ctx)
	return l, err
}
