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

// ExtendLicenseFromOrderIn applies one paid renewal order to a licence.
// It covers both shapes the platform sells: a perpetual licence gets its
// maintenance window extended (through the existing ledger logic), and a
// subscription/trial licence gets its valid_until pushed one interval
// past the current end, with the subscription row kept in step. The
// license_renewals row carries payment_order_id, and the partial unique
// index on that column makes a replay a no-op instead of a double
// extension.
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
	// and give nothing back.
	if lic.Status != model.StatusActive {
		return ErrRenewalIneligible
	}
	days := renewalDays(order, plan)
	// Read the plan's type on this transaction: the plan the caller read
	// before the transaction may have been retyped since, and the type
	// decides whether there is a maintenance window to extend or a
	// valid_until to push.
	var planType string
	if err := tx.NewRaw("SELECT license_type FROM plans WHERE id = ?", plan.ID).Scan(ctx, &planType); err != nil {
		return err
	}
	if planType == "perpetual" {
		return ApplyLicenseRenewalIn(ctx, tx, &model.LicenseRenewal{
			LicenseID: lic.ID, PaymentOrderID: order.ID, Days: days,
		})
	}

	// Subscription/trial: the paid period continues from the current
	// end when it is still running, and from now when it has lapsed.
	now := time.Now()
	base := now
	if lic.ValidUntil != nil && lic.ValidUntil.After(now) {
		base = *lic.ValidUntil
	}
	end := base.Add(subscriptionInterval(plan.BillingInterval))
	if _, err := tx.NewUpdate().Model((*model.License)(nil)).
		Set("valid_until = ?", end).Set("updated_at = now()").
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
		Set("updated_at = now()").
		Where("license_id = ?", lic.ID).Exec(ctx)
	return err
}

// renewalDays is the number of days one renewal order buys. The order's
// reserved period is authoritative when it has one; otherwise the plan's
// renewal term, and 30 days as the last resort the monthly rails assume.
func renewalDays(order *model.PaymentOrder, plan *model.Plan) int {
	if order.PeriodStart != nil && order.PeriodEnd != nil {
		if days := int(order.PeriodEnd.Sub(*order.PeriodStart).Hours() / 24); days > 0 {
			return days
		}
	}
	if plan.RenewalDays > 0 {
		return plan.RenewalDays
	}
	return 30
}

// subscriptionInterval maps the plan's billing interval to the period a
// renewal buys. Kept local to the store because the checkout service has
// its own copy for pricing; both read the same two strings.
func subscriptionInterval(billingInterval string) time.Duration {
	if billingInterval == "year" {
		return 365 * 24 * time.Hour
	}
	return 30 * 24 * time.Hour
}

// FindLicenseRenewalByPaymentOrder reads the ledger row a paid renewal
// order produced, or sql.ErrNoRows when the order has not been applied.
func (s *Store) FindLicenseRenewalByPaymentOrder(ctx context.Context, orderID string) (*model.LicenseRenewal, error) {
	r := new(model.LicenseRenewal)
	err := s.DB.NewSelect().Model(r).Where("payment_order_id = ?", orderID).Limit(1).Scan(ctx)
	return r, err
}
