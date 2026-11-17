package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/checkout"
)

// convertTrial creates the first paid order for a trial that is about to
// end (spec §3.4: a trial conversion is a kind new order).
//
// The order cannot name the licence: checkout refuses a new order with a
// LicenseID, and settlement's new branch derives the licence from the
// order. So the trial licence travels in the metadata, and the paid order
// is the record that the trial has converted — which is also what stops a
// second conversion after payment.
func (s *Service) convertTrial(ctx context.Context, sub *model.Subscription, lic *model.License, now time.Time) error {
	if s.deps.Checkout == nil || sub == nil || sub.Plan == nil || sub.TrialEnd == nil || lic == nil {
		return nil
	}
	if lic.Status != model.StatusTrialing {
		return nil
	}
	trialEnd := sub.TrialEnd.UTC()
	if !trialEnd.After(now) {
		return nil
	}
	lead := time.Duration(s.LeadDays(ctx)) * 24 * time.Hour
	if trialEnd.After(now.Add(lead)) {
		return nil
	}
	paid, err := s.deps.Store.HasPaidTrialConversion(ctx, lic.ID)
	if err != nil {
		return err
	}
	if paid {
		return nil
	}
	email := strings.TrimSpace(lic.Email)
	if email == "" {
		return nil
	}
	quantity := lic.Quantity
	if quantity < 1 {
		quantity = 1
	}
	// The customer may pay a little late: the order stays open through the
	// grace the plan gives a lapsed subscription.
	expires := trialEnd.AddDate(0, 0, graceDays(sub.Plan))
	if !expires.After(now) {
		expires = now.Add(orderTTL)
	}
	res, err := s.deps.Checkout.CreateOrder(ctx, checkout.CreateOrderInput{
		Kind: checkout.KindNew, PlanID: sub.PlanID, Provider: sub.PaymentProvider,
		Email: email, Quantity: quantity, ExpiresAt: &expires,
		Metadata: map[string]any{"billing": map[string]any{
			"source":           "trial_conversion",
			"trial_license_id": lic.ID,
			"trial_end":        trialEnd.Format(time.RFC3339),
		}},
	})
	switch {
	case err == nil:
	case errors.Is(err, checkout.ErrPlanNotSellable),
		errors.Is(err, checkout.ErrPriceNotConfigured),
		errors.Is(err, checkout.ErrQuantityInvalid):
		s.logger.Warn("trial conversion skipped: plan is not sellable",
			"license_id", lic.ID, "plan_id", sub.PlanID, "error", err)
		return nil
	default:
		return fmt.Errorf("create trial conversion order: %w", err)
	}
	if res == nil || res.Order == nil || !res.Created {
		return nil
	}
	s.logger.Info("trial conversion order created",
		"license_id", lic.ID, "order_code", res.Order.Code, "trial_end", trialEnd)
	// The conversion order is a renewal of the customer's access, so the
	// integrator hears about it through the event that means "a payment is
	// needed to keep this licence".
	s.emitRenewalDue(ctx, lic, res.Order)
	return nil
}
