package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/checkout"
	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/store"
)

var (
	// ErrLicenseNotEligible: this licence cannot change to that plan —
	// wrong product, plan not for sale, a perpetual target, a plan it is
	// already on, or a period that already ended.
	ErrLicenseNotEligible = errors.New("licence is not eligible for this plan change")
	// ErrUpgradeInProgress: an earlier change is still waiting for its
	// payment (or for its renewal).
	ErrUpgradeInProgress = errors.New("a plan change is already open for this licence")
	// ErrUpgradeNotChargeable: the unused time covers the whole change.
	// Phase 1b issues no credit or refund, so that is refused rather than
	// turned into a zero-amount payment order.
	ErrUpgradeNotChargeable = errors.New("the unused time covers the whole change")
)

// quoteCurrency is the only currency the Vietnamese rails sell.
const quoteCurrency = "VND"

// UpgradeQuote is what a plan change costs today, before anything is
// written. PeriodStart/PeriodEnd are the period the new plan is bought
// for; CreditMinor is the unused part of the current period, computed
// from the current plan's price and the subscription's current period.
type UpgradeQuote struct {
	LicenseID        string
	ProductID        string
	FromPlanID       string
	ToPlanID         string
	Provider         string
	Currency         string
	CurrencyExponent int16
	// FromAmountMinor is what the current period cost at the current
	// plan's price (price x quantity); CreditMinor is its unused part.
	FromAmountMinor int64
	CreditMinor     int64
	AmountMinor     int64
	PeriodStart     time.Time
	PeriodEnd       time.Time
	// Immediate is false for a next_cycle plan: the change is scheduled
	// and charged by the next renewal instead of today.
	Immediate bool
}

// CreateUpgradeOrderInput is one plan change asked for by the portal or
// an admin.
type CreateUpgradeOrderInput struct {
	LicenseID string
	ToPlanID  string
	// Provider empty uses the subscription's own rail: a customer does not
	// silently change rails mid-subscription.
	Provider string
}

// ProrateMinor is the unused fraction of amountMinor over
// periodStart..periodEnd at now, in integer minor units, floored. It is
// the whole amount at or before the start, and zero at or after the end.
// The arithmetic is exact integer math: a 32-bit float would lose dong at
// VND's scale.
func ProrateMinor(amountMinor int64, periodStart, periodEnd, now time.Time) int64 {
	if amountMinor <= 0 || !periodEnd.After(periodStart) {
		return 0
	}
	if !now.After(periodStart) {
		return amountMinor
	}
	if !now.Before(periodEnd) {
		return 0
	}
	total := periodEnd.Sub(periodStart)
	remaining := periodEnd.Sub(now)
	// amountMinor * remaining can overflow int64 (minor units times
	// nanoseconds), so the product is taken in big integers and floored
	// by integer division.
	num := new(big.Int).Mul(big.NewInt(amountMinor), big.NewInt(int64(remaining)))
	num.Quo(num, big.NewInt(int64(total)))
	return num.Int64()
}

// QuoteUpgrade prices a plan change without writing anything.
//
// The credit is the unused part of the current period at the current
// plan's price; the charge is the target plan's price for the same period
// less that credit, floored at zero. The new plan starts now and runs to
// the end of the period the customer already bought, so its next renewal
// prices the new plan in full.
func (s *Service) QuoteUpgrade(ctx context.Context, licenseID, toPlanID string, now time.Time) (*UpgradeQuote, error) {
	if s.deps.Store == nil {
		return nil, errors.New("billing: store is required")
	}
	now = now.UTC()
	lic, err := s.deps.Store.FindLicenseByID(ctx, licenseID)
	if err != nil {
		return nil, fmt.Errorf("%w: licence %s not found", ErrLicenseNotEligible, licenseID)
	}
	target, err := s.deps.Store.FindPlanByID(ctx, toPlanID)
	if err != nil {
		return nil, fmt.Errorf("%w: plan %s not found", ErrLicenseNotEligible, toPlanID)
	}
	if target.ID == lic.PlanID {
		return nil, fmt.Errorf("%w: the licence is already on plan %s", ErrLicenseNotEligible, target.ID)
	}
	if !target.Active || target.ProductID != lic.ProductID {
		return nil, fmt.Errorf("%w: plan %s is not for sale on this product", ErrLicenseNotEligible, target.ID)
	}
	// Only a time-based plan can be entered mid-period: a perpetual
	// plan's price buys a year of updates, not the rest of this period.
	if target.LicenseType != "subscription" {
		return nil, fmt.Errorf("%w: only subscription plans can be prorated", ErrLicenseNotEligible)
	}
	switch lic.Status {
	case model.StatusActive, model.StatusTrialing, model.StatusPastDue:
	default:
		return nil, fmt.Errorf("%w: licence is %s", ErrLicenseNotEligible, lic.Status)
	}
	sub, err := s.deps.Store.FindSubscriptionByLicense(ctx, licenseID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: licence has no subscription", ErrLicenseNotEligible)
	}
	if err != nil {
		return nil, fmt.Errorf("read subscription: %w", err)
	}
	provider := sub.PaymentProvider
	if provider == "" {
		provider = lic.PaymentProvider
	}
	if provider != "sepay" && provider != "payos" {
		return nil, fmt.Errorf("%w: licence is not billed by sepay or payos", ErrLicenseNotEligible)
	}
	// PeriodEnd: what the customer already paid for. The licence end is
	// authoritative for what the customer can use; the subscription's
	// period end is the fallback for a subscription-only row.
	periodEnd := time.Time{}
	switch {
	case lic.ValidUntil != nil:
		periodEnd = lic.ValidUntil.UTC()
	case sub.CurrentPeriodEnd != nil:
		periodEnd = sub.CurrentPeriodEnd.UTC()
	default:
		return nil, fmt.Errorf("%w: licence has no paid period", ErrLicenseNotEligible)
	}
	if !periodEnd.After(now) {
		return nil, fmt.Errorf("%w: the current period has ended", ErrLicenseNotEligible)
	}
	// The proration window's start: the subscription's period start, or,
	// when the subscription predates the column, one current interval
	// before the period end. The quote's PeriodStart stays "now" because
	// that is when the new plan begins.
	periodStart := periodEnd.Add(-intervalDuration(subPlanInterval(sub)))
	if sub.CurrentPeriodStart != nil && sub.CurrentPeriodStart.Before(periodEnd) {
		periodStart = sub.CurrentPeriodStart.UTC()
	}
	quantity := lic.Quantity
	if quantity < 1 {
		quantity = 1
	}
	currentPlan := sub.Plan
	if currentPlan == nil {
		currentPlan, err = s.deps.Store.FindPlanByID(ctx, lic.PlanID)
		if err != nil {
			return nil, fmt.Errorf("%w: current plan %s not found", ErrLicenseNotEligible, lic.PlanID)
		}
	}
	currentPrice, err := s.deps.Store.ResolvePlanPrice(ctx, lic.PlanID, provider, quoteCurrency, currentPlan.BillingInterval)
	if err != nil {
		return nil, fmt.Errorf("%w: the current plan has no %s price for %s", ErrLicenseNotEligible, quoteCurrency, provider)
	}
	targetPrice, err := s.deps.Store.ResolvePlanPrice(ctx, target.ID, provider, quoteCurrency, target.BillingInterval)
	if err != nil {
		return nil, fmt.Errorf("%w: plan %s has no %s price for %s", ErrLicenseNotEligible, target.ID, quoteCurrency, provider)
	}
	fromAmount := currentPrice.AmountMinor * int64(quantity)
	targetAmount := targetPrice.AmountMinor * int64(quantity)
	credit := ProrateMinor(fromAmount, periodStart, periodEnd, now)
	amount := targetAmount - credit
	if amount < 0 {
		amount = 0
	}
	return &UpgradeQuote{
		LicenseID: lic.ID, ProductID: lic.ProductID,
		FromPlanID: lic.PlanID, ToPlanID: target.ID, Provider: provider,
		Currency: quoteCurrency, CurrencyExponent: domain.CurrencyExponent(quoteCurrency),
		FromAmountMinor: fromAmount, CreditMinor: credit, AmountMinor: amount,
		PeriodStart: now, PeriodEnd: periodEnd,
		Immediate: target.ProrationMode != "next_cycle",
	}, nil
}

// subPlanInterval is the billing interval the subscription's own plan
// sells, used only when the subscription has no current_period_start.
func subPlanInterval(sub *model.Subscription) string {
	if sub == nil || sub.Plan == nil {
		return ""
	}
	return sub.Plan.BillingInterval
}

// CreateUpgradeOrder opens a plan change.
//
// immediate: one pending upgrade order for the prorated difference; the
// licence changes only when that order is paid. next_cycle: one pending
// upgrade row, no order and no money, and the renewal settlement applies
// the change when the current period renews. A retried call returns the
// same open row/order instead of opening a second change.
func (s *Service) CreateUpgradeOrder(ctx context.Context, in CreateUpgradeOrderInput) (*checkout.OrderResult, error) {
	if s.deps.Store == nil {
		return nil, errors.New("billing: store is required")
	}
	if s.deps.Checkout == nil {
		return nil, errors.New("billing: checkout is required")
	}
	now := s.deps.Now().UTC()
	quote, err := s.QuoteUpgrade(ctx, in.LicenseID, in.ToPlanID, now)
	if err != nil {
		return nil, err
	}
	if in.Provider != "" && in.Provider != quote.Provider {
		return nil, fmt.Errorf("%w: the licence is billed on %s, not %s",
			ErrLicenseNotEligible, quote.Provider, in.Provider)
	}
	lic, err := s.deps.Store.FindLicenseByID(ctx, quote.LicenseID)
	if err != nil {
		return nil, fmt.Errorf("%w: licence %s not found", ErrLicenseNotEligible, quote.LicenseID)
	}
	existing, err := s.deps.Store.FindPendingUpgradeForLicense(ctx, quote.LicenseID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.ToPlanID != quote.ToPlanID {
			return nil, fmt.Errorf("%w: a change to plan %s is already open", ErrUpgradeInProgress, existing.ToPlanID)
		}
		if !quote.Immediate {
			// The same scheduled change; nothing to charge.
			return nil, nil
		}
		if existing.PaymentOrderID == "" {
			// A crash between the row insert and the checkout. Close the
			// half-open row so a fresh order can own the change.
			if err := store.EndLicenseUpgradeIn(ctx, s.deps.Store.DB, existing, model.UpgradeVoid, "checkout was never created"); err != nil {
				return nil, err
			}
			return s.openImmediateUpgrade(ctx, quote, lic)
		}
		return s.resumeUpgradeCheckout(ctx, existing, quote)
	}
	if !quote.Immediate {
		return s.scheduleNextCycleUpgrade(ctx, quote, lic)
	}
	return s.openImmediateUpgrade(ctx, quote, lic)
}

// openImmediateUpgrade creates the order and then the pending upgrade row
// that pays for it. The row is written before the caller can observe the
// order: a paid upgrade without its row is money settlement must refuse.
func (s *Service) openImmediateUpgrade(ctx context.Context, quote *UpgradeQuote, lic *model.License) (*checkout.OrderResult, error) {
	if quote.AmountMinor <= 0 {
		return nil, fmt.Errorf("%w: a downgrade is not refunded in phase 1b", ErrUpgradeNotChargeable)
	}
	upgradeID := store.NewLicenseUpgradeID()
	quantity := lic.Quantity
	if quantity < 1 {
		quantity = 1
	}
	res, err := s.deps.Checkout.CreateOrder(ctx, checkout.CreateOrderInput{
		Kind: checkout.KindUpgrade, LicenseID: quote.LicenseID, PlanID: quote.ToPlanID,
		Provider: quote.Provider, Quantity: quantity, CreditMinor: quote.CreditMinor,
		PeriodStart: &quote.PeriodStart, PeriodEnd: &quote.PeriodEnd,
		Metadata: map[string]any{"billing": map[string]any{
			"source":       "upgrade",
			"upgrade_id":   upgradeID,
			"credit_minor": quote.CreditMinor,
		}},
	})
	if err != nil {
		return nil, err
	}
	if res == nil || res.Order == nil {
		return nil, errors.New("billing: checkout returned no order for a plan change")
	}
	up := &model.LicenseUpgrade{
		ID: upgradeID, LicenseID: quote.LicenseID, PaymentOrderID: res.Order.ID,
		ToPlanID: quote.ToPlanID, FromPlanID: quote.FromPlanID,
		FromStatus: lic.Status, FromValidUntil: lic.ValidUntil,
		FromUpdatesUntil: lic.UpdatesUntil, FromUpdatesTermsSet: lic.UpdatesTermsSet,
		FromPaymentProvider: lic.PaymentProvider, FromCanceledAt: lic.CanceledAt,
		FromQuantity: lic.Quantity,
	}
	if err := s.deps.Store.CreateUpgradeForOrderIn(ctx, s.deps.Store.DB, up); err != nil {
		return nil, err
	}
	return res, nil
}

// scheduleNextCycleUpgrade writes the pending row a renewal will apply.
// The licence is deliberately untouched: the customer keeps the plan they
// paid for until the period they paid for ends.
func (s *Service) scheduleNextCycleUpgrade(ctx context.Context, quote *UpgradeQuote, lic *model.License) (*checkout.OrderResult, error) {
	up := &model.LicenseUpgrade{
		ID: store.NewLicenseUpgradeID(), LicenseID: quote.LicenseID,
		ToPlanID: quote.ToPlanID, FromPlanID: quote.FromPlanID,
		FromStatus: lic.Status, FromValidUntil: lic.ValidUntil,
		FromUpdatesUntil: lic.UpdatesUntil, FromUpdatesTermsSet: lic.UpdatesTermsSet,
		FromPaymentProvider: lic.PaymentProvider, FromCanceledAt: lic.CanceledAt,
		FromQuantity: lic.Quantity,
	}
	up.StripeCheckoutSessionID = store.NextCycleUpgradeSessionPrefix + up.ID
	if err := store.CreateLicenseUpgradeIn(ctx, s.deps.Store.DB, up); err != nil {
		return nil, err
	}
	// No order, no money, no licence change: the next renewal carries the
	// new plan's price (spec §3.6).
	return nil, nil
}

// resumeUpgradeCheckout hands back the open change's order. Checkout
// resumes the same pending order (or a fresh attempt on it) for the same
// licence, plan and credit; a replacement order is re-linked before it is
// returned.
func (s *Service) resumeUpgradeCheckout(ctx context.Context, up *model.LicenseUpgrade, quote *UpgradeQuote) (*checkout.OrderResult, error) {
	order, err := s.deps.Store.GetPaymentOrderForUpdate(ctx, nil, up.PaymentOrderID)
	if err != nil {
		return nil, fmt.Errorf("read open upgrade order: %w", err)
	}
	if order.Kind != checkout.KindUpgrade || order.LicenseID != quote.LicenseID || order.PlanID != quote.ToPlanID {
		return nil, fmt.Errorf("%w: open upgrade %s does not match this change", ErrUpgradeInProgress, up.ID)
	}
	lic, err := s.deps.Store.FindLicenseByID(ctx, quote.LicenseID)
	if err != nil {
		return nil, fmt.Errorf("%w: licence %s not found", ErrLicenseNotEligible, quote.LicenseID)
	}
	quantity := lic.Quantity
	if quantity < 1 {
		quantity = 1
	}
	res, err := s.deps.Checkout.CreateOrder(ctx, checkout.CreateOrderInput{
		Kind: checkout.KindUpgrade, LicenseID: quote.LicenseID, PlanID: quote.ToPlanID,
		Provider: quote.Provider, Quantity: quantity, CreditMinor: quote.CreditMinor,
		PeriodStart: &quote.PeriodStart, PeriodEnd: &quote.PeriodEnd,
		Metadata: map[string]any{"billing": map[string]any{
			"source":       "upgrade",
			"upgrade_id":   up.ID,
			"credit_minor": quote.CreditMinor,
		}},
	})
	if err != nil {
		return nil, err
	}
	if res == nil || res.Order == nil {
		return nil, errors.New("billing: checkout returned no order for a plan change")
	}
	if res.Order.ID != up.PaymentOrderID {
		// The old order expired: the replacement now pays for the change.
		if err := s.deps.Store.LinkUpgradeOrderIn(ctx, s.deps.Store.DB, up.ID, res.Order.ID); err != nil {
			return nil, err
		}
	}
	return res, nil
}
