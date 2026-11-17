package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/dtg-corp/dsoft/internal/model"
)

// The portal reads the payment ledger as one customer. A customer is the
// signed-in user's email: payment_orders stores it on the order and in
// the buyer snapshot, and phase 1a has no external_customer_id column on
// the order (that identity lives on licences and coupon redemptions).
// Every query below uses that predicate instead of accepting an order id
// from the client, so a foreign code and an unknown code are one answer.

// ListPortalPaymentOrders returns one customer's orders, newest first.
// The total is the number of rows the predicate matched, not the page
// size, so the portal can page without re-asking.
func (s *Store) ListPortalPaymentOrders(ctx context.Context, email string, p Page) ([]*model.PaymentOrder, int, error) {
	email = normalizeEmail(email)
	var out []*model.PaymentOrder
	q := s.DB.NewSelect().Model(&out).
		Where(`(lower(email) = ? OR lower(buyer->>'email') = ?)`, email, email).
		OrderExpr("created_at DESC, id DESC")
	total, err := scanPage(ctx, q, p)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// GetPortalPaymentOrder reads one customer's order by its customer-facing
// code. A code that belongs to somebody else is ErrOrderNotFound: the
// portal must not reveal that it exists.
func (s *Store) GetPortalPaymentOrder(ctx context.Context, code, email string) (*model.PaymentOrder, error) {
	email = normalizeEmail(email)
	order := new(model.PaymentOrder)
	err := s.DB.NewSelect().Model(order).
		Where("code = ?", code).
		Where(`(lower(email) = ? OR lower(buyer->>'email') = ?)`, email, email).
		Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOrderNotFound
	}
	if err != nil {
		return nil, err
	}
	return order, nil
}

// GetPaymentOrderByID reads an order by id without an ownership predicate.
// It is for the invoice ownership check, which starts from an invoice the
// customer already reached through their own invoice list: the invoice
// names the order, and the order's buyer is a second way the invoice can
// belong to the caller. It is never used to answer a portal order lookup.
func (s *Store) GetPaymentOrderByID(ctx context.Context, id string) (*model.PaymentOrder, error) {
	order := new(model.PaymentOrder)
	err := s.DB.NewSelect().Model(order).Where("id = ?", id).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOrderNotFound
	}
	if err != nil {
		return nil, err
	}
	return order, nil
}

// GetBillingProfile reads one customer's saved buyer/tax block. A user
// with no row answers sql.ErrNoRows; the portal turns that into an empty
// buyer rather than a 404, because "nothing saved yet" is not an error.
func (s *Store) GetBillingProfile(ctx context.Context, userID string) (*model.CustomerBillingProfile, error) {
	profile := new(model.CustomerBillingProfile)
	err := s.DB.NewSelect().Model(profile).Where("user_id = ?", userID).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return profile, nil
}

// UpsertBillingProfile writes the buyer block for one user and returns
// the stored row. The conflict target is the primary key, so the update
// path cannot create a second row for the same user; updated_at moves
// while created_at keeps the first save.
func (s *Store) UpsertBillingProfile(ctx context.Context, userID string, buyer []byte) (*model.CustomerBillingProfile, error) {
	if len(buyer) == 0 {
		buyer = []byte("{}")
	}
	now := time.Now()
	row := &model.CustomerBillingProfile{UserID: userID, Buyer: buyer, CreatedAt: now, UpdatedAt: now}
	err := s.DB.NewInsert().Model(row).
		On("CONFLICT (user_id) DO UPDATE").
		Set("buyer = EXCLUDED.buyer").
		Set("updated_at = now()").
		Returning("*").Scan(ctx)
	if err != nil {
		return nil, err
	}
	return row, nil
}
