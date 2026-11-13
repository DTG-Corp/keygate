package store_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

func cleanupPaymentTransactions(t *testing.T, s *store.Store, refs ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, ref := range refs {
			_, _ = s.DB.NewRaw("DELETE FROM payment_transactions WHERE provider_ref = ?", ref).Exec(ctx)
		}
	})
}

// One transfer is one row: (provider, provider_ref) is the dedupe key, so
// a redelivered webhook leaves the first row untouched instead of
// recording the money twice. A caller with no payload still stores an
// object, and a row is marked matched in place.
func TestPaymentTransactionDedupesAndMatches(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)
	order, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatal(err)
	}
	ref := "tx-" + uuid.NewString()[:13]
	cleanupPaymentTransactions(t, s, ref)
	occurred := time.Now().UTC().Truncate(time.Second)

	row, err := s.InsertPaymentTransaction(ctx, nil, store.PaymentTransactionInput{
		Provider: "sepay", ProviderRef: ref, AmountMinor: order.TotalMinor, Currency: order.Currency,
		Status: domain.TxUnmatched, OccurredAt: occurred,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if row == nil || row.ID == "" || row.OrderID != nil || row.Status != domain.TxUnmatched {
		t.Fatalf("row = %+v", row)
	}
	var raw string
	if err := s.DB.NewRaw("SELECT raw::text FROM payment_transactions WHERE id = ?", row.ID).Scan(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	if raw != "{}" {
		t.Fatalf("raw = %s, want the object default", raw)
	}

	// The same provider reference again: no error, no second row, and the
	// first row's amount is not overwritten.
	again, err := s.InsertPaymentTransaction(ctx, nil, store.PaymentTransactionInput{
		Provider: "sepay", ProviderRef: ref, AmountMinor: 1, Currency: order.Currency,
		Status: domain.TxUnmatched, OccurredAt: occurred,
	})
	if err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	if again == nil {
		t.Fatal("duplicate returned no row")
	}
	var n int
	if err := s.DB.NewRaw("SELECT count(*) FROM payment_transactions WHERE provider = ? AND provider_ref = ?",
		"sepay", ref).Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	var amount int64
	if err := s.DB.NewRaw("SELECT amount_minor FROM payment_transactions WHERE provider = ? AND provider_ref = ?",
		"sepay", ref).Scan(ctx, &amount); err != nil {
		t.Fatal(err)
	}
	if amount != order.TotalMinor {
		t.Fatalf("amount = %d, want the first row's %d", amount, order.TotalMinor)
	}

	if err := s.MarkTransactionMatched(ctx, row.ID, order.ID); err != nil {
		t.Fatalf("mark matched: %v", err)
	}
	var status string
	var orderID sql.NullString
	if err := s.DB.NewRaw("SELECT status, order_id FROM payment_transactions WHERE id = ?", row.ID).
		Scan(ctx, &status, &orderID); err != nil {
		t.Fatal(err)
	}
	if status != domain.TxMatched || !orderID.Valid || orderID.String != order.ID {
		t.Fatalf("status = %q, order_id = %v; want matched/%s", status, orderID, order.ID)
	}
}

// A matched row can name its order at insert time, and the write honours
// the caller's transaction: a settlement that rolls back leaves no
// half-recorded money.
func TestPaymentTransactionOnCallerTransaction(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)
	order, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatal(err)
	}
	ref := "tx-tx-" + uuid.NewString()[:13]
	cleanupPaymentTransactions(t, s, ref)

	if err := s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		row, err := s.InsertPaymentTransaction(ctx, tx, store.PaymentTransactionInput{
			Provider: "payos", ProviderRef: ref, OrderID: &order.ID, AmountMinor: order.TotalMinor,
			Currency: order.Currency, Status: domain.TxMatched, OccurredAt: time.Now(),
		})
		if err != nil {
			return err
		}
		if row.OrderID == nil || *row.OrderID != order.ID {
			t.Fatalf("order id = %v", row.OrderID)
		}
		return errTestRollback
	}); err != errTestRollback {
		t.Fatalf("tx = %v", err)
	}
	var n int
	if err := s.DB.NewRaw("SELECT count(*) FROM payment_transactions WHERE provider_ref = ?", ref).Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rolled-back rows = %d, want 0", n)
	}
}
