// Package billing runs the renewal cycle for subscriptions sold through
// the Vietnamese rails: it creates the renewal order inside a lead
// window, mails the reminder ladder, walks an unpaid licence from
// past_due to suspended to expired, honours cancel-at-period-end and
// converts a trial into its first paid order (spec §3.6).
//
// It owns decisions, not writes. Every licence or subscription change
// goes through a conditional write in internal/store/billing.go, so a
// pass that went stale while a payment landed cannot overwrite it, and
// every order goes through the existing checkout service. Settlement
// stays the only writer that turns a paid order into a licence.
package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/checkout"
	"github.com/tabloy/keygate/internal/store"
)

const (
	// DefaultLeadDays is the reminder window before a period ends. It is
	// the floor as well: the 7-day rung needs a window to fire in.
	DefaultLeadDays = 7
	// MaxLeadDays keeps an operator's typo from opening the window a
	// year early.
	MaxLeadDays = 90
	// leadDaysSetting is the install setting the window comes from.
	// plan.renewal_days is the perpetual maintenance term, not a
	// subscription lead time, so it is not used here.
	leadDaysSetting = "renewal_lead_days"

	// passInterval is how often Run repeats. The spec asks for hourly.
	passInterval = time.Hour

	// orderTTL is what checkout uses for an order the engine does not
	// date itself; a late renewal gets at least this long to be paid.
	orderTTL = 24 * time.Hour

	// graceReminderRung1/3/7 are the grace days the notices fire on.
	graceRungOne = 1
	graceRungMid = 3
	graceRungMax = 7

	// reminderRung1/3/7 are the days before a period end the notices
	// fire on.
	reminderRungOne = 1
	reminderRungMid = 3
	reminderRungMax = 7
)

// OrderCreator is the slice of checkout the engine uses. Keeping it an
// interface lets a test script a provider outage without an HTTP server.
type OrderCreator interface {
	CreateOrder(ctx context.Context, in checkout.CreateOrderInput) (*checkout.OrderResult, error)
}

// ReminderSender renders the two ladders. The interface is the email
// service's public surface, so a test can record what would be mailed
// instead of parsing HTML.
type ReminderSender interface {
	RenderRenewalNotice(productName, planName string, daysLeft int, payURL string, periodEnd time.Time) (subject, body string)
	RenderRenewalOverdue(productName, planName, payURL string, periodEnd time.Time) (subject, body string)
	RenderRenewalGrace(productName, planName string, daysLeft int, payURL string, periodEnd time.Time) (subject, body string)
	RenderRenewalSuspension(productName, planName, payURL string, periodEnd time.Time) (subject, body string)
}

// EventEmitter is the webhook dispatcher (service.WebhookService).
type EventEmitter interface {
	Dispatch(ctx context.Context, productID, event string, data map[string]any)
}

// Deps is everything the engine talks to.
type Deps struct {
	Store    *store.Store
	Checkout OrderCreator
	Email    ReminderSender
	Webhook  EventEmitter
	Logger   *slog.Logger
	BaseURL  string
	// Now is the clock every pass reads once. Tests pin it; nil is
	// time.Now().UTC().
	Now func() time.Time
}

// Service is the hourly billing engine.
type Service struct {
	deps   Deps
	logger *slog.Logger
}

// New builds the engine. Store and Checkout are required; a nil Logger
// discards, a nil Email or Webhook is tolerated and the corresponding
// step is skipped.
func New(d Deps) *Service {
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{deps: d, logger: logger}
}

// Run performs a pass immediately, then one an hour until ctx is done.
// It never returns an error: a failed pass is logged and the next hour
// retries, the same contract the expiry loop keeps.
func (s *Service) Run(ctx context.Context) {
	s.runLogged(ctx)
	ticker := time.NewTicker(passInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runLogged(ctx)
		}
	}
}

func (s *Service) runLogged(ctx context.Context) {
	if err := s.RunOnce(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.Error("billing pass failed", "error", err)
		return
	}
	s.logger.Debug("billing pass finished")
}

// RunOnce performs one pass. A single licence's failure is logged and
// collected; the rest of the pass still runs, and any error is returned
// joined so a caller can see all of them.
func (s *Service) RunOnce(ctx context.Context) error {
	if s.deps.Store == nil {
		return errors.New("billing: store is required")
	}
	now := s.deps.Now().UTC()
	lead := time.Duration(s.LeadDays(ctx)) * 24 * time.Hour
	subs, err := s.deps.Store.ListBillingSubscriptions(ctx, now, lead)
	if err != nil {
		return err
	}
	var errs []error
	for _, sub := range subs {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		if err := s.process(ctx, sub, now); err != nil {
			s.logger.Error("billing item failed", "license_id", sub.LicenseID, "error", err)
			errs = append(errs, fmt.Errorf("licence %s: %w", sub.LicenseID, err))
		}
	}
	return errors.Join(errs...)
}

// process routes one scanned subscription to the half of the cycle it is
// in. The order of the checks matters: a cancellation outranks
// everything (nothing is charged), a live trial has its own conversion,
// a period that has not ended gets the pre-expiry half, and an ended
// period gets dunning.
func (s *Service) process(ctx context.Context, sub *model.Subscription, now time.Time) error {
	if sub == nil || sub.License == nil || sub.CurrentPeriodEnd == nil {
		return nil
	}
	lic := sub.License
	end := sub.CurrentPeriodEnd.UTC()
	switch {
	case sub.CancelAtPeriodEnd:
		return s.endCanceled(ctx, sub, lic, now)
	case sub.Status == model.StatusTrialing && sub.TrialEnd != nil && now.Before(sub.TrialEnd.UTC()):
		return s.convertTrial(ctx, sub, lic, now)
	case now.Before(end):
		if err := s.ensureRenewalOrder(ctx, sub, lic, now); err != nil {
			return err
		}
		return s.sendReminders(ctx, sub, lic, now)
	default:
		return s.dun(ctx, sub, lic, now)
	}
}

// LeadDays is the reminder window in days: the install setting, floored
// at DefaultLeadDays and capped at MaxLeadDays.
func (s *Service) LeadDays(ctx context.Context) int {
	days := DefaultLeadDays
	if s.deps.Store != nil {
		if raw, err := s.deps.Store.GetSetting(ctx, leadDaysSetting); err == nil {
			if n, convErr := strconv.Atoi(strings.TrimSpace(raw)); convErr == nil && n > 0 {
				days = n
			}
		}
	}
	if days < DefaultLeadDays {
		days = DefaultLeadDays
	}
	if days > MaxLeadDays {
		days = MaxLeadDays
	}
	return days
}

// ensureRenewalOrder creates the renewal order for a due period unless
// one already covers it. It is idempotent: the store's open-order lookup
// catches the passes we see, and the renewal_once unique index catches
// the replicas we do not.
func (s *Service) ensureRenewalOrder(ctx context.Context, sub *model.Subscription, lic *model.License, now time.Time) error {
	if s.deps.Checkout == nil || sub.Plan == nil || sub.CurrentPeriodEnd == nil {
		return nil
	}
	// A licence an operator holds, or one that already ended, gets no
	// new order. A suspended licence still does not, until settlement
	// can apply a payment to it (see the dunning task).
	if !renewableLicence(lic) {
		return nil
	}
	end := sub.CurrentPeriodEnd.UTC()
	if _, err := s.deps.Store.FindOpenRenewalOrderForPeriod(ctx, lic.ID, end); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	email := strings.TrimSpace(lic.Email)
	if email == "" {
		s.logger.Warn("renewal order skipped: licence has no email", "license_id", lic.ID)
		return nil
	}
	start := end.Add(-intervalDuration(sub.Plan.BillingInterval))
	if sub.CurrentPeriodStart != nil {
		start = sub.CurrentPeriodStart.UTC()
	}
	quantity := lic.Quantity
	if quantity < 1 {
		quantity = 1
	}
	// The order has to outlive the grace window it is meant to be paid
	// in, otherwise the QR expires days before the customer is late.
	expires := end.AddDate(0, 0, graceDays(sub.Plan))
	if !expires.After(now) {
		expires = now.Add(orderTTL)
	}
	res, err := s.deps.Checkout.CreateOrder(ctx, checkout.CreateOrderInput{
		Kind: checkout.KindRenewal, LicenseID: lic.ID, PlanID: sub.PlanID,
		Provider: sub.PaymentProvider, Email: email, Quantity: quantity,
		PeriodStart: &start, PeriodEnd: &end, ExpiresAt: &expires,
		Metadata: map[string]any{"billing": map[string]any{
			"source":       "renewal_worker",
			"period_start": start.Format(time.RFC3339),
			"period_end":   end.Format(time.RFC3339),
		}},
	})
	switch {
	case err == nil:
	case store.IsRenewalOrderConflict(err):
		// Another replica won the race; its order is the answer and it
		// emits the event.
		s.logger.Info("renewal order already created for this period", "license_id", lic.ID, "period_end", end)
		return nil
	case errors.Is(err, checkout.ErrPlanNotSellable),
		errors.Is(err, checkout.ErrPriceNotConfigured),
		errors.Is(err, checkout.ErrQuantityInvalid):
		s.logger.Warn("renewal order skipped: plan is not sellable",
			"license_id", lic.ID, "plan_id", sub.PlanID, "error", err)
		return nil
	default:
		return fmt.Errorf("create renewal order: %w", err)
	}
	if res == nil || res.Order == nil || !res.Created {
		return nil
	}
	s.emitRenewalDue(ctx, lic, res.Order)
	return nil
}

// renewableLicence reports whether the engine may write a new order for
// a licence: an operator's hold, an expiry or a revocation is a decision
// the engine respects.
func renewableLicence(lic *model.License) bool {
	if lic == nil {
		return false
	}
	switch lic.Status {
	case model.StatusActive, model.StatusPastDue:
		return true
	}
	return false
}

// liveLicence reports whether the dunning ladder still applies: an
// operator's suspension and a revocation are not the engine's to touch.
func liveLicence(lic *model.License) bool {
	if lic == nil {
		return false
	}
	switch lic.Status {
	case model.StatusActive, model.StatusPastDue, model.StatusSuspended, model.StatusTrialing:
		return true
	}
	return false
}

// graceDays is the plan's grace window, never negative.
func graceDays(plan *model.Plan) int {
	if plan == nil || plan.GraceDays < 0 {
		return 0
	}
	return plan.GraceDays
}

// intervalDuration mirrors settlement's convention: a year is 365 days,
// everything else is a 30-day month. No calendar arithmetic, so no month
// length or DST surprises.
func intervalDuration(interval string) time.Duration {
	if strings.EqualFold(interval, "year") {
		return 365 * 24 * time.Hour
	}
	return 30 * 24 * time.Hour
}

// holdDuration is how long a licence stays suspended before it expires:
// plan.renewal_days when the operator set one, else one billing interval.
// Zero is not a valid hold; it would make suspension unobservable.
func holdDuration(plan *model.Plan) time.Duration {
	if plan != nil && plan.RenewalDays > 0 {
		return time.Duration(plan.RenewalDays) * 24 * time.Hour
	}
	if plan == nil {
		return 30 * 24 * time.Hour
	}
	return intervalDuration(plan.BillingInterval)
}

// ceilDays is the whole number of days until t, rounded up: 23h30m is one
// day left, and exactly 0 is zero days left.
func ceilClock(now, t time.Time) int {
	if !t.After(now) {
		return 0
	}
	d := t.Sub(now)
	days := int(d / (24 * time.Hour))
	if d%(24*time.Hour) != 0 {
		days++
	}
	return days
}

// portalURL is the one link the mails carry: phase 1b has no server-side
// payment page, so the customer pays from the portal.
func (s *Service) portalURL() string {
	base := strings.TrimRight(strings.TrimSpace(s.deps.BaseURL), "/")
	if base == "" {
		return ""
	}
	return base + "/portal"
}

// planName and productName are safe accessors: a relation the scan did
// not load must not panic a pass.
func planName(plan *model.Plan) string {
	if plan == nil {
		return ""
	}
	return plan.Name
}

func productName(lic *model.License) string {
	if lic == nil || lic.Product == nil {
		return ""
	}
	return lic.Product.Name
}
