package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// DefaultSyncScope is the cursor scope a provider that has no
// per-account subdivision uses. Spec §4.10 keys a cursor by
// (provider, scope) so a future SePay account id can share the table
// without a migration.
const DefaultSyncScope = "default"

// GetPaymentSyncCursor reads the reconciler's high-water mark for one
// provider and scope. A row that does not exist yet answers
// sql.ErrNoRows — the first pass has nothing to resume from and reads a
// backfill window instead.
func (s *Store) GetPaymentSyncCursor(ctx context.Context, providerName, scope string) (*model.PaymentSyncCursor, error) {
	if scope == "" {
		scope = DefaultSyncScope
	}
	row := new(model.PaymentSyncCursor)
	err := s.DB.NewSelect().Model(row).
		Where("provider = ?", providerName).
		Where("scope = ?", scope).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

// SetPaymentSyncCursor records how far a provider pass got. The
// provider's own transaction id is stored as TEXT so a rail with string
// ids fits the same table as one with integers; a zero at is the arrival
// time, which keeps the column meaningful for a provider that sent no
// timestamp.
func (s *Store) SetPaymentSyncCursor(ctx context.Context, providerName, scope, lastTransactionID string, at time.Time) error {
	if scope == "" {
		scope = DefaultSyncScope
	}
	if at.IsZero() {
		at = time.Now()
	}
	row := &model.PaymentSyncCursor{
		ID: NewID(), Provider: providerName, Scope: scope,
		LastTransactionID: lastTransactionID, LastSyncedAt: &at, UpdatedAt: time.Now(),
	}
	_, err := s.DB.NewInsert().Model(row).
		On("CONFLICT (provider, scope) DO UPDATE").
		Set("last_transaction_id = EXCLUDED.last_transaction_id").
		Set("last_synced_at = EXCLUDED.last_synced_at").
		Set("updated_at = now()").
		Exec(ctx)
	return err
}
