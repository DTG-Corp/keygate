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
// raw payload that is not JSON. The column is JSONB, so the database
// would answer an opaque SQLSTATE; the caller gets a plain error instead.
var ErrPaymentTransactionInvalid = errors.New("invalid payment transaction")

// PaymentTransactionInput is one transfer as the settlement service saw
// it. OrderID is nil when the transfer names no order (the unmatched
// queue), which is what makes the column NULL rather than ”.
type PaymentTransactionInput struct {
	Provider    string
	ProviderRef string
	OrderID     *string
	AmountMinor int64
	Currency    string
	Status      string
	Raw         []byte
	OccurredAt  time.Time
}

// InsertPaymentTransaction records one provider transfer. The unique
// (provider, provider_ref) index is the dedupe: a redelivered webhook
// inserts nothing and does not overwrite the row the first delivery
// wrote. A caller with no payload still stores a JSON object, and a
// missing occurrence time is stamped now rather than written as the zero
// year.
//
// tx is the caller's transaction when there is one, so a settlement that
// fails after recording the transfer can take the row back.
func (s *Store) InsertPaymentTransaction(ctx context.Context, tx bun.IDB, in PaymentTransactionInput) (*model.PaymentTransaction, error) {
	raw := json.RawMessage(in.Raw)
	if len(raw) == 0 {
		// raw is JSONB NOT NULL and readers expect an object; Bun would
		// write a nil RawMessage as the JSON literal null.
		raw = json.RawMessage("{}")
	} else if !json.Valid(raw) {
		return nil, fmt.Errorf("%w: raw is not valid JSON", ErrPaymentTransactionInvalid)
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
		AmountMinor: in.AmountMinor, Currency: in.Currency, Status: in.Status,
		Raw: raw, OccurredAt: occurred, CreatedAt: time.Now(),
	}
	if _, err := db.NewInsert().Model(row).On("CONFLICT (provider, provider_ref) DO NOTHING").Exec(ctx); err != nil {
		return nil, err
	}
	return row, nil
}

// MarkTransactionMatched attaches an unmatched transfer to the order it
// paid. The status moves to matched so the operator queue no longer
// lists it.
func (s *Store) MarkTransactionMatched(ctx context.Context, id, orderID string) error {
	_, err := s.DB.NewUpdate().Model((*model.PaymentTransaction)(nil)).
		Set("status = ?", domain.TxMatched).
		Set("order_id = ?", orderID).
		Where("id = ?", id).Exec(ctx)
	return err
}
