package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
)

var (
	ErrPriceNotConfigured = errors.New("plan price not configured")
	ErrPriceInvalid       = errors.New("plan price invalid")
	// ErrPriceSlotConflict means another writer put an active row in the
	// same (plan, provider, currency, interval) slot between this
	// upsert's deactivate and its insert, so the partial unique index
	// rejected the insert. The caller can read and retry; it is not an
	// internal failure.
	ErrPriceSlotConflict = errors.New("plan price slot conflict")
)

type PlanPriceInput struct {
	Provider         string
	Currency         string
	AmountMinor      int64
	CurrencyExponent int16
	BillingInterval  string
	ExternalPriceID  string
}

func (in *PlanPriceInput) normalize() error {
	in.Provider = strings.ToLower(strings.TrimSpace(in.Provider))
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	in.BillingInterval = strings.ToLower(strings.TrimSpace(in.BillingInterval))
	switch in.Provider {
	case "sepay", "payos":
	default:
		return fmt.Errorf("%w: provider must be sepay or payos", ErrPriceInvalid)
	}
	if len(in.Currency) != 3 {
		return fmt.Errorf("%w: currency must be a 3-letter ISO code", ErrPriceInvalid)
	}
	if in.AmountMinor < 0 {
		return fmt.Errorf("%w: amount_minor must be >= 0", ErrPriceInvalid)
	}
	// The exponent is checked exactly, not treated as optional when zero:
	// a caller that omits it for USD would otherwise store 0 and turn
	// $1.00 into $0.01. VND/JPY/KRW legitimately have exponent 0, so
	// their zero is both the omitted and the correct value.
	want := domain.CurrencyExponent(in.Currency)
	if in.CurrencyExponent != want {
		return fmt.Errorf("%w: currency_exponent %d does not match %s (%d)", ErrPriceInvalid, in.CurrencyExponent, in.Currency, want)
	}
	switch in.BillingInterval {
	case "", "month", "year":
	default:
		return fmt.Errorf("%w: billing_interval must be '', 'month' or 'year'", ErrPriceInvalid)
	}
	return nil
}

// UpsertPlanPrice makes in the single active price for its
// (plan, provider, currency, interval) slot. The old row is deactivated,
// not deleted: payment_orders references it, and an order's price
// snapshot must stay readable after a price change.
//
// A plan sells one billing cycle, so in.BillingInterval is optional: an
// empty interval means the plan's own, and a different one is refused.
// Checkout resolves the plan's interval, so a row in any other slot
// would be stored and never sellable.
func (s *Store) UpsertPlanPrice(ctx context.Context, planID string, in PlanPriceInput) (*model.PlanPrice, error) {
	if err := in.normalize(); err != nil {
		return nil, err
	}
	plan, err := s.FindPlanByID(ctx, planID)
	if err != nil {
		return nil, err
	}
	planInterval := strings.ToLower(strings.TrimSpace(plan.BillingInterval))
	switch {
	case in.BillingInterval == "":
		in.BillingInterval = planInterval
	case in.BillingInterval != planInterval:
		return nil, fmt.Errorf("%w: billing_interval %q does not match the plan's %q", ErrPriceInvalid, in.BillingInterval, planInterval)
	}
	row := &model.PlanPrice{
		ID: NewID(), PlanID: planID, Provider: in.Provider, Currency: in.Currency,
		AmountMinor: in.AmountMinor, CurrencyExponent: in.CurrencyExponent,
		BillingInterval: in.BillingInterval, ExternalPriceID: in.ExternalPriceID,
		Active: true, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	err = RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.NewUpdate().Model((*model.PlanPrice)(nil)).
			Set("active = ?", false).Set("updated_at = ?", time.Now()).
			Where("plan_id = ?", planID).
			Where("provider = ?", in.Provider).
			Where("currency = ?", in.Currency).
			Where("billing_interval = ?", in.BillingInterval).
			Where("active").
			Exec(ctx)
		if err != nil {
			return err
		}
		_, err = tx.NewInsert().Model(row).Exec(ctx)
		return err
	})
	if err != nil {
		// The deactivate above and this insert are one transaction, but
		// two concurrent upserts can both pass the deactivate before
		// either inserts; the partial unique index then rejects one.
		if IsUniqueViolation(err) {
			return nil, ErrPriceSlotConflict
		}
		return nil, err
	}
	return row, nil
}

func (s *Store) ListPlanPrices(ctx context.Context, planID string) ([]*model.PlanPrice, error) {
	var out []*model.PlanPrice
	err := s.DB.NewSelect().Model(&out).
		Where("plan_id = ?", planID).Order("provider ASC", "currency ASC", "billing_interval ASC", "created_at DESC").
		Scan(ctx)
	return out, err
}

func (s *Store) ListActivePlanPrices(ctx context.Context, planIDs []string) (map[string][]*model.PlanPrice, error) {
	out := make(map[string][]*model.PlanPrice, len(planIDs))
	if len(planIDs) == 0 {
		return out, nil
	}
	var rows []*model.PlanPrice
	if err := s.DB.NewSelect().Model(&rows).
		Where("plan_id IN (?)", bun.In(planIDs)).
		Where("active").
		// Deterministic: the public handler's legacy flat price uses the
		// first row of a plan, so the order must not depend on physical
		// storage or the planner.
		Order("plan_id ASC", "billing_interval ASC", "provider ASC", "currency ASC", "created_at DESC").
		Scan(ctx); err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.PlanID] = append(out[r.PlanID], r)
	}
	return out, nil
}

func (s *Store) DeactivatePlanPrice(ctx context.Context, planID, priceID string) error {
	return deletedOne(s.DB.NewUpdate().Model((*model.PlanPrice)(nil)).
		Set("active = ?", false).Set("updated_at = ?", time.Now()).
		Where("id = ?", priceID).Where("plan_id = ?", planID).Exec(ctx))
}

func (s *Store) ResolvePlanPrice(ctx context.Context, planID, provider, currency, billingInterval string) (*model.PlanPrice, error) {
	row := new(model.PlanPrice)
	err := s.DB.NewSelect().Model(row).
		Where("plan_id = ?", planID).
		Where("provider = ?", strings.ToLower(provider)).
		Where("currency = ?", strings.ToUpper(currency)).
		Where("billing_interval = ?", strings.ToLower(billingInterval)).
		Where("active").
		Limit(1).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPriceNotConfigured
		}
		return nil, err
	}
	return row, nil
}
