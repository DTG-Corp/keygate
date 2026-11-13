package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
)

// ErrPaymentTransactionInvalid means InsertPaymentTransaction was given a
// row the schema cannot accept — a raw payload that is not JSON, or a
// direction other than in/out. The caller gets a plain error instead of
// an opaque SQLSTATE.
var ErrPaymentTransactionInvalid = errors.New("invalid payment transaction")

// PaymentTransactionInput is one transfer as the settlement service saw
// it. OrderID is nil when the transfer names no order (the unmatched
// queue), which is what makes the column NULL rather than ”.
type PaymentTransactionInput struct {
	Provider    string
	ProviderRef string
	OrderID     *string
	AttemptID   *string
	AmountMinor int64
	Currency    string
	// Direction is "in" (customer paid) or "out" (refund/payout). Empty
	// means "in", the schema default.
	Direction string
	// ProviderAccount, Reference and Content are the account that
	// received the transfer, the bank reference and the memo: the three
	// fields the unmatched queue shows an operator.
	ProviderAccount string
	Reference       string
	Content         string
	Status          string
	// IgnoreReason explains a row that was not applied: the settlement
	// failure that recorded it, or the operator's reason for ignoring
	// it. Persisted in payment_transactions.ignore_reason.
	IgnoreReason string
	Raw          []byte
	OccurredAt   time.Time
}

// InsertPaymentTransaction records one provider transfer and reports
// whether it was written. The unique (provider, provider_ref) index is
// the dedupe: a redelivered webhook inserts nothing, and the caller is
// handed the row that is actually stored — not a struct that was never
// written, which a later MarkTransactionMatched would target by an id
// no row carries. A caller with no payload still stores a JSON object,
// and a missing occurrence time is stamped now rather than written as
// the zero year.
//
// tx is the caller's transaction when there is one, so a settlement that
// fails after recording the transfer can take the row back.
func (s *Store) InsertPaymentTransaction(ctx context.Context, tx bun.IDB, in PaymentTransactionInput) (*model.PaymentTransaction, bool, error) {
	raw := json.RawMessage(in.Raw)
	if len(raw) == 0 {
		// raw is JSONB NOT NULL and readers expect an object; Bun would
		// write a nil RawMessage as the JSON literal null.
		raw = json.RawMessage("{}")
	} else if !json.Valid(raw) {
		return nil, false, fmt.Errorf("%w: raw is not valid JSON", ErrPaymentTransactionInvalid)
	}
	direction := in.Direction
	if direction == "" {
		direction = "in"
	}
	if direction != "in" && direction != "out" {
		return nil, false, fmt.Errorf("%w: direction %q is not in/out", ErrPaymentTransactionInvalid, in.Direction)
	}
	attemptID := in.AttemptID
	if attemptID != nil && *attemptID == "" {
		attemptID = nil
	}
	occurred := in.OccurredAt
	if occurred.IsZero() {
		occurred = time.Now()
	}
	db := bun.IDB(s.DB)
	if tx != nil {
		db = tx
	}
	row := &model.PaymentTransaction{
		ID: NewID(), Provider: in.Provider, ProviderRef: in.ProviderRef, OrderID: in.OrderID,
		AttemptID: attemptID, AmountMinor: in.AmountMinor, Currency: in.Currency,
		Direction: direction, ProviderAccount: in.ProviderAccount,
		Reference: in.Reference, Content: in.Content,
		Status: in.Status, IgnoreReason: in.IgnoreReason,
		Raw: raw, OccurredAt: occurred, CreatedAt: time.Now(),
	}
	res, err := db.NewInsert().Model(row).On("CONFLICT (provider, provider_ref) DO NOTHING").Exec(ctx)
	if err != nil {
		return nil, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return row, true, nil
	}
	// The transfer was recorded before (a redelivery, or a settlement
	// retried after a failed grant). Return that row: its id is the one
	// the database holds, and its amount/raw are the first delivery's.
	stored := new(model.PaymentTransaction)
	if err := db.NewSelect().Model(stored).
		Where("provider = ?", in.Provider).Where("provider_ref = ?", in.ProviderRef).
		Scan(ctx); err != nil {
		return nil, false, fmt.Errorf("read stored payment transaction: %w", err)
	}
	return stored, false, nil
}

// MarkTransactionMatched attaches an unmatched transfer to the order it
// paid. The status moves to matched so the operator queue no longer
// lists it. id must be a row's id — InsertPaymentTransaction returns the
// stored row, so a conflict cannot hand the caller a phantom one.
func (s *Store) MarkTransactionMatched(ctx context.Context, id, orderID string) error {
	return s.MarkTransactionMatchedIn(ctx, nil, id, orderID)
}

// MarkTransactionMatchedIn is MarkTransactionMatched on the caller's
// transaction, so a settlement can attach the transfer it just re-found
// in the same write that stamps the order paid.
func (s *Store) MarkTransactionMatchedIn(ctx context.Context, tx bun.IDB, id, orderID string) error {
	db := bun.IDB(s.DB)
	if tx != nil {
		db = tx
	}
	_, err := db.NewUpdate().Model((*model.PaymentTransaction)(nil)).
		Set("status = ?", domain.TxMatched).
		Set("order_id = ?", orderID).
		Set("ignore_reason = ''").
		Where("id = ?", id).Exec(ctx)
	return err
}
