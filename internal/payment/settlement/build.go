package settlement

import (
	"context"
	"fmt"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// buildNewLicense assembles the licence row for a paid new-order. It
// reads the plan as it is now; CreatePaidOrderLicence re-reads it on the
// settlement's transaction and refuses a plan that no longer sells the
// same kind of licence the order was priced from (ErrPlanChanged), which
// is the same protection the Stripe path has.
func buildNewLicense(ctx context.Context, s *store.Store, order *model.PaymentOrder) (*model.License, *model.Plan, error) {
	plan, err := s.FindPlanByID(ctx, order.PlanID)
	if err != nil {
		return nil, nil, fmt.Errorf("read plan: %w", err)
	}
	if !plan.Active {
		return nil, nil, fmt.Errorf("plan %s is not active", plan.ID)
	}
	// The order stores the money and the kind; it does not store the
	// plan's licence_type. The row below follows the plan's type as it is
	// now, and CreateLicenseWithSubscription refuses the write if that
	// type moved since this read.
	key, err := s.NewLicenseKey(ctx, order.ProductID)
	if err != nil {
		return nil, nil, err
	}
	// The licence row does not carry type or limits: those live on the
	// plan and limits are derived from it. Only the period is stored.
	now := time.Now()
	lic := &model.License{
		ProductID: order.ProductID, PlanID: order.PlanID, Email: order.Email,
		LicenseKey: key, Status: model.StatusActive, Quantity: order.Quantity,
		PaymentProvider: order.Provider,
	}
	switch plan.LicenseType {
	case "perpetual":
		// Perpetual: valid forever, updates for the purchased window
		// (nil when the plan says lifetime).
		lic.UpdatesUntil = plan.InitialUpdatesUntil(now)
		lic.UpdatesTermsSet = true
	case "trial":
		// A paid trial buys a trial, not a paid subscription: the
		// status is what the expiry sweep and the trial fallback read.
		lic.Status = model.StatusTrialing
		end := order.PeriodEnd
		if end == nil {
			e := now.Add(firstPeriod(plan))
			end = &e
		}
		lic.ValidUntil = end
	default:
		// A subscription plan's first period ends at the order's
		// reserved period_end; with none reserved, one billing interval
		// from now.
		end := order.PeriodEnd
		if end == nil {
			e := now.Add(intervalDuration(plan.BillingInterval))
			end = &e
		}
		lic.ValidUntil = end
	}
	return lic, plan, nil
}

// firstPeriod is the first period a recurring plan sells when the order
// reserved none: the trial length for a trial plan, otherwise one
// billing interval.
func firstPeriod(plan *model.Plan) time.Duration {
	if plan.LicenseType == "trial" {
		days := plan.TrialDays
		if days <= 0 {
			days = 30
		}
		return time.Duration(days) * 24 * time.Hour
	}
	return intervalDuration(plan.BillingInterval)
}

// intervalDuration is the length of one billing interval as the plan
// spells it. The two strings are the only ones the payment rails sell;
// anything else is a monthly term.
func intervalDuration(interval string) time.Duration {
	if interval == "year" {
		return 365 * 24 * time.Hour
	}
	return 30 * 24 * time.Hour
}
