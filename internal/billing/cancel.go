package billing

import (
	"context"
	"time"

	"github.com/dtg-corp/dsoft/internal/model"
)

// endCanceled honours cancel-at-period-end: the customer keeps the period
// they paid for and the subscription ends when it is over. No order, no
// reminder and no dunning is produced on the way, and the licence is left
// alone — its valid_until is the end of the paid period, and the platform
// grace sweep owns what happens after it.
func (s *Service) endCanceled(ctx context.Context, sub *model.Subscription, lic *model.License, now time.Time) error {
	if sub == nil || sub.CurrentPeriodEnd == nil || now.Before(sub.CurrentPeriodEnd.UTC()) {
		return nil
	}
	canceled, err := s.deps.Store.CancelSubscriptionAtPeriodEnd(ctx, sub, now)
	if err != nil {
		return err
	}
	if !canceled {
		// The flag was cleared, or the period moved, between the scan and
		// the write. The next pass sees the new state.
		return nil
	}
	s.audit(ctx, lic, "subscription_canceled", map[string]any{
		"reason":     "cancel_at_period_end",
		"period_end": sub.CurrentPeriodEnd,
	})
	s.logger.Info("subscription canceled at period end",
		"license_id", lic.ID, "period_end", sub.CurrentPeriodEnd)
	return nil
}
