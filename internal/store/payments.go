package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
)

// ErrOrderNotFound means no payment order carries the code, numeric code
// or id that was asked for.
var ErrOrderNotFound = errors.New("payment order not found")

// ErrOrderNotPayable means the order can no longer take money: it is
// already paid, expired, canceled or refunded. Settlement treats it as
// an expected answer and records the transfer for an operator instead of
// granting anything.
var ErrOrderNotPayable = errors.New("payment order is not payable")

// ErrOrderNotCancelable means the order is not pending, so the customer
// cannot cancel it: it was already paid, expired, canceled or refunded.
// It is separate from ErrOrderNotPayable because settlement owns that
// answer and the cancel path's caller needs to tell the two apart.
var ErrOrderNotCancelable = errors.New("payment order is not cancelable")

// ErrOrderInvalidInput means CreatePaymentOrder was given something the
// order cannot mean: a zero or past expiry, a negative amount, a quantity
// below one, a missing identifier, or a negative tax rate. It is returned
// before any database write.
var ErrOrderInvalidInput = errors.New("invalid payment order input")

// ErrAttemptStatusInvalid means UpdatePaymentAttemptStatus was given a
// status outside the payment_attempts CHECK vocabulary.
var ErrAttemptStatusInvalid = errors.New("invalid payment attempt status")

// OrderDiscount is one coupon applied to a new order, frozen at the
// amount it took off. CouponID is empty when the coupon was not a stored
// row (or has since been deleted); the code and amount are kept either
// way.
type OrderDiscount struct {
	CouponID    string
	CouponCode  string
	Type        string
	Value       int64
	AmountMinor int64
	Priority    int
}

// CreateOrderInput is everything CreatePaymentOrder writes: the priced
// amount, what it buys, and the coupons already applied to it. The
// numeric_code is left to the database identity column, and the status
// starts at pending.
type CreateOrderInput struct {
	ProductID, PlanID, PriceID, Email, Kind string
	Quantity                                int
	SubtotalMinor, DiscountMinor            int64
	TaxMinor, TotalMinor                    int64
	Currency                                string
	CurrencyExponent                        int16
	TaxRateBP                               int
	Provider                                string
	ExpiresAt                               time.Time
	Buyer, Metadata                         []byte
	CreatedByKeyID                          string
	// LicenseID and PeriodEnd place a renewal order against the licence it
	// extends. payment_orders_renewal_once allows one pending renewal per
	// (license_id, period_end), so a caller retrying a renewal must see
	// that collision as its own answer, not as a code-allocation retry.
	LicenseID string
	PeriodEnd *time.Time
	Discounts []OrderDiscount
}

// orderCode generates a code; a package var so a test can force a
// collision and pin the retry path.
var orderCode = domain.NewOrderCode

// orderCodeAttempts bounds the retry loop CreatePaymentOrder runs when
// the database rejects a generated code as a duplicate. Two collisions
// in a row are already vanishingly unlikely; five is only a stop so a
// generator that always repeats cannot loop forever.
const orderCodeAttempts = 5

// errOrderCodeCollision marks the one failure CreatePaymentOrder retries:
// the order insert rejecting its freshly generated code as a duplicate.
// A unique violation from elsewhere in the transaction — two coupon lines
// with one code, or the renewal_once index, say — is the caller's bug and
// is returned at once, not mistaken for a collision and retried with a
// new code.
var errOrderCodeCollision = errors.New("order code collision")

// orderCodeConstraint is the unique constraint the retry is for. The
// migration declares code as TEXT NOT NULL UNIQUE, whose backing
// constraint Postgres names payment_orders_code_key (verified against
// pg_constraint on the migrated test database).
const orderCodeConstraint = "payment_orders_code_key"

// validateCreateOrderInput rejects an order that cannot mean anything
// before it reaches the database. The schema has CHECKs for most of these
// too; catching them here gives the caller ErrOrderInvalidInput instead of
// a SQLSTATE it has to interpret.
func validateCreateOrderInput(in CreateOrderInput) error {
	switch {
	case !in.ExpiresAt.After(time.Now()):
		return fmt.Errorf("%w: expires_at must be in the future", ErrOrderInvalidInput)
	case in.Quantity < 1:
		return fmt.Errorf("%w: quantity must be at least 1", ErrOrderInvalidInput)
	case in.ProductID == "":
		return fmt.Errorf("%w: product_id is required", ErrOrderInvalidInput)
	case in.PlanID == "":
		return fmt.Errorf("%w: plan_id is required", ErrOrderInvalidInput)
	case in.Email == "":
		return fmt.Errorf("%w: email is required", ErrOrderInvalidInput)
	case in.Currency == "":
		return fmt.Errorf("%w: currency is required", ErrOrderInvalidInput)
	case in.SubtotalMinor < 0, in.DiscountMinor < 0, in.TaxMinor < 0, in.TotalMinor < 0:
		return fmt.Errorf("%w: amounts must not be negative", ErrOrderInvalidInput)
	case in.TaxRateBP < 0:
		return fmt.Errorf("%w: tax_rate_bp must not be negative", ErrOrderInvalidInput)
	}
	return nil
}

// CreatePaymentOrder writes an order and its discount rows in one
// transaction and returns it with the code the database assigned. A
// duplicate code retries with a fresh one; any other error is returned
// as it is.
func (s *Store) CreatePaymentOrder(ctx context.Context, in CreateOrderInput) (*model.PaymentOrder, error) {
	if err := validateCreateOrderInput(in); err != nil {
		return nil, err
	}
	// buyer and metadata are NOT NULL and a JSON object is what readers
	// (receipts, admin views) expect. Bun writes a nil or empty RawMessage
	// as the JSON literal null (nil) or invalid JSON (empty), never as the
	// column's '{}' default, so default them here.
	buyer, metadata := in.Buyer, in.Metadata
	if len(buyer) == 0 {
		buyer = []byte("{}")
	}
	if len(metadata) == 0 {
		metadata = []byte("{}")
	}
	var lastErr error
	for i := 0; i < orderCodeAttempts; i++ {
		order := &model.PaymentOrder{
			ID: NewID(), Code: orderCode(), ProductID: in.ProductID, PlanID: in.PlanID,
			PriceID: in.PriceID, Email: in.Email, Kind: in.Kind, Quantity: in.Quantity,
			SubtotalMinor: in.SubtotalMinor, DiscountMinor: in.DiscountMinor,
			TaxMinor: in.TaxMinor, TotalMinor: in.TotalMinor,
			Currency: in.Currency, CurrencyExp: in.CurrencyExponent, TaxRateBP: in.TaxRateBP,
			Provider: in.Provider, Status: domain.OrderPending, ExpiresAt: in.ExpiresAt,
			Buyer: buyer, Metadata: metadata, CreatedByKeyID: in.CreatedByKeyID,
			LicenseID: in.LicenseID, PeriodEnd: in.PeriodEnd,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		err := RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.NewInsert().Model(order).Exec(ctx); err != nil {
				if isUniqueViolationOn(err, orderCodeConstraint) {
					return fmt.Errorf("%w: %w", errOrderCodeCollision, err)
				}
				return err
			}
			// numeric_code is a database identity column, so the insert
			// omits it and Bun does not read it back. Re-select the row
			// inside the same transaction to populate it — the PayOS
			// orderCode is this value, so a zero here breaks every link.
			if err := tx.NewSelect().Model(order).Where("id = ?", order.ID).Scan(ctx); err != nil {
				return err
			}
			for _, d := range in.Discounts {
				row := &model.PaymentOrderDiscount{
					ID: NewID(), OrderID: order.ID, CouponID: d.CouponID, CouponCode: d.CouponCode,
					Type: d.Type, Value: d.Value, AmountMinor: d.AmountMinor, Priority: d.Priority,
					CreatedAt: time.Now(),
				}
				if _, err := tx.NewInsert().Model(row).Exec(ctx); err != nil {
					return err
				}
			}
			return nil
		})
		if err == nil {
			return order, nil
		}
		// Only the order insert's own duplicate code is a collision;
		// anything else is a real failure.
		if errors.Is(err, errOrderCodeCollision) {
			lastErr = err
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("could not allocate an order code after %d tries: %w", orderCodeAttempts, lastErr)
}

// GetPaymentOrderByCode reads the one order carrying a customer-facing
// code.
func (s *Store) GetPaymentOrderByCode(ctx context.Context, code string) (*model.PaymentOrder, error) {
	order := new(model.PaymentOrder)
	err := s.DB.NewSelect().Model(order).Where("code = ?", code).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOrderNotFound
	}
	if err != nil {
		return nil, err
	}
	return order, nil
}

// GetPaymentOrderByNumericCode reads the one order carrying a provider's
// numeric order code (PayOS's orderCode).
func (s *Store) GetPaymentOrderByNumericCode(ctx context.Context, numeric int64) (*model.PaymentOrder, error) {
	order := new(model.PaymentOrder)
	err := s.DB.NewSelect().Model(order).Where("numeric_code = ?", numeric).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOrderNotFound
	}
	if err != nil {
		return nil, err
	}
	return order, nil
}

// GetPaymentOrderForUpdate reads an order under FOR UPDATE so a caller
// can decide on it and write while no other transaction changes it. With
// a nil tx the row is still read, but the lock ends with the statement:
// that is the shape tests and read-mostly callers use, and a caller that
// must keep the row still passes the transaction its write runs in.
//
// The plan wrote this as tx bun.Tx, but bun.Tx is a struct: nil cannot be
// passed for it and "tx != nil" does not compile. The callers hold no
// transaction when they only read (and settlement calls Mark with none),
// so the parameter is the interface a *bun.DB and a bun.Tx both satisfy.
func (s *Store) GetPaymentOrderForUpdate(ctx context.Context, tx bun.IDB, orderID string) (*model.PaymentOrder, error) {
	order := new(model.PaymentOrder)
	db := bun.IDB(s.DB)
	if tx != nil {
		db = tx
	}
	err := db.NewSelect().Model(order).Where("id = ?", orderID).For("UPDATE").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOrderNotFound
	}
	if err != nil {
		return nil, err
	}
	return order, nil
}

// MarkPaymentOrderPaid moves a pending order to paid and stamps the
// moment, under a row lock and behind the terminal guard: an order that
// already ended — paid, expired, canceled or refunded — answers
// ErrOrderNotPayable, so a second settlement cannot grant twice. The
// caller's transaction, when given, is the one the read and the write
// run on, so a settlement can commit its licence with the status.
func (s *Store) MarkPaymentOrderPaid(ctx context.Context, tx bun.IDB, orderID string, paidAt time.Time) (*model.PaymentOrder, error) {
	db := bun.IDB(s.DB)
	if tx != nil {
		db = tx
	}
	order, err := s.GetPaymentOrderForUpdate(ctx, tx, orderID)
	if err != nil {
		return nil, err
	}
	if domain.IsOrderTerminal(order.Status) {
		return nil, ErrOrderNotPayable
	}
	now := time.Now()
	res, err := db.NewUpdate().Model((*model.PaymentOrder)(nil)).
		Set("status = ?", domain.OrderPaid).Set("paid_at = ?", paidAt).Set("updated_at = ?", now).
		// The order is settled: whatever error an earlier settlement left
		// must not be read as the current state.
		Set("last_settlement_error = ''").
		Where("id = ?", orderID).
		// The guard is repeated in the write. A caller with no transaction
		// holds no row lock between the read above and this UPDATE, so two
		// settlers can both find the order pending; the loser then matches
		// no row here and answers ErrOrderNotPayable, leaving paid_at on the
		// first payment.
		Where("status = ?", domain.OrderPending).Exec(ctx)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrOrderNotPayable
	}
	// Return the row as stored: paid_at is the database's value and the
	// settlement error and updated_at were changed by this statement.
	if err := db.NewSelect().Model(order).Where("id = ?", orderID).Scan(ctx); err != nil {
		return nil, err
	}
	return order, nil
}

// CancelPaymentOrder moves a pending order to canceled. A terminal order
// answers ErrOrderNotCancelable: cancelling a paid order must not strand a
// licence the customer bought. The UPDATE repeats the pending condition
// so a concurrent payment or expiry wins the row rather than being
// overwritten; the loser answers ErrOrderNotCancelable too.
func (s *Store) CancelPaymentOrder(ctx context.Context, code string) (*model.PaymentOrder, error) {
	order, err := s.GetPaymentOrderByCode(ctx, code)
	if err != nil {
		return nil, err
	}
	if order.Status != domain.OrderPending {
		return nil, ErrOrderNotCancelable
	}
	res, err := s.DB.NewUpdate().Model((*model.PaymentOrder)(nil)).
		Set("status = ?", domain.OrderCanceled).Set("updated_at = ?", time.Now()).
		// A canceled order is settled too: no stale settlement error may
		// outlive the state it described.
		Set("last_settlement_error = ''").
		Where("id = ?", order.ID).Where("status = ?", domain.OrderPending).Exec(ctx)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Another writer took the row between the read and this write.
		return nil, ErrOrderNotCancelable
	}
	if err := s.DB.NewSelect().Model(order).Where("id = ?", order.ID).Scan(ctx); err != nil {
		return nil, err
	}
	return order, nil
}

// ExpirePaymentOrders moves every pending order whose expires_at has
// passed to expired, and reports how many it moved. Only pending rows
// are touched: a paid order keeps its status even if its expiry is in the
// past, which is what lets a late bank transfer be recorded rather than
// treated as unpaid.
func (s *Store) ExpirePaymentOrders(ctx context.Context, now time.Time) (int, error) {
	res, err := s.DB.NewUpdate().Model((*model.PaymentOrder)(nil)).
		Set("status = ?", domain.OrderExpired).Set("updated_at = ?", now).
		Where("status = ?", domain.OrderPending).Where("expires_at < ?", now).Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CreatePaymentAttempt writes one provider instruction for an order,
// filling in its id and creation time when the caller left them zero.
func (s *Store) CreatePaymentAttempt(ctx context.Context, a *model.PaymentAttempt) error {
	if a.ID == "" {
		a.ID = NewID()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	if len(a.Raw) == 0 {
		// raw is NOT NULL and callers read it as a JSON object; a nil or
		// empty RawMessage is written as the JSON literal null or as
		// invalid JSON, not as the column default.
		a.Raw = json.RawMessage("{}")
	}
	_, err := s.DB.NewInsert().Model(a).Exec(ctx)
	return err
}

// GetLatestPaymentAttempt reads the newest attempt for an order, or
// sql.ErrNoRows when the order has none yet. The id breaks a created_at
// tie so two attempts written in the same instant still have one
// deterministic answer.
func (s *Store) GetLatestPaymentAttempt(ctx context.Context, orderID string) (*model.PaymentAttempt, error) {
	a := new(model.PaymentAttempt)
	err := s.DB.NewSelect().Model(a).
		Where("order_id = ?", orderID).Order("created_at DESC", "id DESC").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// UpdatePaymentAttemptStatus records where a provider instruction ended
// up, and the provider's reference for it when one is given. An empty
// providerRef is a status-only update: the stored reference is kept
// rather than cleared, so its unique slot stays with this attempt. An
// empty result answers sql.ErrNoRows, like GetLatestPaymentAttempt.
func (s *Store) UpdatePaymentAttemptStatus(ctx context.Context, id, status, providerRef string) error {
	if !validAttemptStatus(status) {
		return fmt.Errorf("%w: %q", ErrAttemptStatusInvalid, status)
	}
	res, err := s.DB.NewUpdate().Model((*model.PaymentAttempt)(nil)).
		Set("status = ?", status).
		Set("provider_ref = COALESCE(NULLIF(?, ''), provider_ref)", providerRef).
		Set("updated_at = ?", time.Now()).
		Where("id = ?", id).Exec(ctx)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// validAttemptStatus mirrors the payment_attempts.status CHECK. It lives
// here rather than in domain because the vocabulary is a store write
// contract: a caller passing "paid" (an order status) gets a sentinel
// instead of a database CHECK violation.
func validAttemptStatus(status string) bool {
	switch status {
	case domain.AttemptCreated, domain.AttemptPending, domain.AttemptSucceeded,
		domain.AttemptFailed, domain.AttemptExpired, domain.AttemptCanceled:
		return true
	}
	return false
}
