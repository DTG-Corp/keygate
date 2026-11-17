package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/dtg-corp/dsoft/internal/model"
)

// The admin detail page reads an order and everything the rails did
// with it. The lists are small (an order has one or two attempts) and
// each is ordered oldest first, because that is the order the money
// moved in.

func (s *Store) ListPaymentAttemptsByOrder(ctx context.Context, orderID string) ([]*model.PaymentAttempt, error) {
	var out []*model.PaymentAttempt
	err := s.DB.NewSelect().Model(&out).Where("order_id = ?", orderID).
		Order("created_at ASC", "id ASC").Scan(ctx)
	return out, err
}

func (s *Store) ListPaymentTransactionsByOrder(ctx context.Context, orderID string) ([]*model.PaymentTransaction, error) {
	var out []*model.PaymentTransaction
	err := s.DB.NewSelect().Model(&out).Where("order_id = ?", orderID).
		Order("occurred_at ASC", "id ASC").Scan(ctx)
	return out, err
}

func (s *Store) ListPaymentEventsByOrder(ctx context.Context, orderID string) ([]*model.PaymentEvent, error) {
	var out []*model.PaymentEvent
	err := s.DB.NewSelect().Model(&out).Where("order_id = ?", orderID).
		Order("occurred_at ASC", "id ASC").Scan(ctx)
	return out, err
}

func (s *Store) ListPaymentRefundsByOrder(ctx context.Context, orderID string) ([]*model.PaymentRefund, error) {
	var out []*model.PaymentRefund
	err := s.DB.NewSelect().Model(&out).Where("order_id = ?", orderID).
		Order("created_at ASC", "id ASC").Scan(ctx)
	return out, err
}

// PaymentProviderHealth is one provider's row on the admin health page:
// how much money is waiting, what the last failure was and how far the
// cursor has got. Every field is read-only and none of it is a secret.
type PaymentProviderHealth struct {
	Provider      string
	LastEventAt   *time.Time
	UnmatchedOpen int
	PendingOrders int
	LastError     string
	LastSyncedAt  *time.Time
}

// PaymentProviderHealth computes one provider's health in four small
// queries. The counts are install-wide: an operator watching a rail
// wants the whole queue, not one product's slice.
func (s *Store) PaymentProviderHealth(ctx context.Context, providerName string) (*PaymentProviderHealth, error) {
	out := &PaymentProviderHealth{Provider: providerName}

	var lastEvent sql.NullTime
	if err := s.DB.NewRaw("SELECT MAX(occurred_at) FROM payment_events WHERE provider = ?", providerName).
		Scan(ctx, &lastEvent); err != nil {
		return nil, err
	}
	if lastEvent.Valid {
		at := lastEvent.Time
		out.LastEventAt = &at
	}

	n, err := s.DB.NewSelect().Model((*model.PaymentTransaction)(nil)).
		Where("provider = ?", providerName).Where("status = ?", "unmatched").Count(ctx)
	if err != nil {
		return nil, err
	}
	out.UnmatchedOpen = n

	n, err = s.DB.NewSelect().Model((*model.PaymentOrder)(nil)).
		Where("provider = ?", providerName).Where("status = ?", "pending").Count(ctx)
	if err != nil {
		return nil, err
	}
	out.PendingOrders = n

	var lastError sql.NullString
	if err := s.DB.NewRaw(`SELECT last_settlement_error FROM payment_orders
		WHERE provider = ? AND last_settlement_error <> ''
		ORDER BY updated_at DESC LIMIT 1`, providerName).Scan(ctx, &lastError); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if lastError.Valid {
		out.LastError = lastError.String
	}

	cursor, err := s.GetPaymentSyncCursor(ctx, providerName, DefaultSyncScope)
	switch {
	case err == nil:
		out.LastSyncedAt = cursor.LastSyncedAt
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	return out, nil
}
