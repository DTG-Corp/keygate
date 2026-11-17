package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/dtg-corp/dsoft/internal/model"
)

// ErrPaymentEventInvalid means RecordPaymentEvent was given an event it
// cannot record meaningfully: no provider event id (which dedupes
// nothing) or a raw payload that is not JSON.
var ErrPaymentEventInvalid = errors.New("invalid payment event")

// PaymentEventInput is one verified provider delivery as the webhook
// handler records it before settlement. OrderID is empty for an event
// that names no order (the column is then NULL).
type PaymentEventInput struct {
	Provider   string
	EventID    string
	OrderID    string
	Kind       string
	Raw        []byte
	OccurredAt time.Time
}

// RecordPaymentEvent inserts the provider event id once. A duplicate —
// the provider's retry, two replicas receiving one delivery, or a
// replay after a partial settlement — answers inserted=false with no
// error. The unique key is (provider, event_id); an empty event id would
// dedupe every anonymous delivery into one row, so it is refused.
//
// The claim is kept when settlement fails: the webhook handler treats a
// duplicate whose order is still pending as a retry, so the next
// delivery settles without needing the claim removed.
//
// tx is the caller's transaction when there is one. bun.IDB is the
// parameter type rather than bun.Tx because bun.Tx is a struct here and
// cannot be nil.
func (s *Store) RecordPaymentEvent(ctx context.Context, tx bun.IDB, in PaymentEventInput) (bool, error) {
	if in.EventID == "" {
		return false, fmt.Errorf("%w: provider event id is required", ErrPaymentEventInvalid)
	}
	raw := json.RawMessage(in.Raw)
	if len(raw) == 0 {
		// raw is JSONB NOT NULL and readers expect an object; Bun would
		// write a nil RawMessage as the JSON literal null.
		raw = json.RawMessage("{}")
	} else if !json.Valid(raw) {
		return false, fmt.Errorf("%w: raw is not valid JSON", ErrPaymentEventInvalid)
	}
	occurred := in.OccurredAt
	if occurred.IsZero() {
		// occurred_at is NOT NULL; a caller that only knows the event id
		// (a provider that sends no timestamp) gets the arrival time
		// instead of the zero year.
		occurred = time.Now()
	}
	db := bun.IDB(s.DB)
	if tx != nil {
		db = tx
	}
	row := &model.PaymentEvent{
		ID: NewID(), Provider: in.Provider, EventID: in.EventID, OrderID: in.OrderID,
		Kind: in.Kind, Raw: raw, OccurredAt: occurred, CreatedAt: time.Now(),
	}
	res, err := db.NewInsert().Model(row).On("CONFLICT (provider, event_id) DO NOTHING").Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}
