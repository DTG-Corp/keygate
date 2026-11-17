package store_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/internal/testsupport/testdb"
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

	row, inserted, err := s.InsertPaymentTransaction(ctx, nil, store.PaymentTransactionInput{
		Provider: "sepay", ProviderRef: ref, AmountMinor: order.TotalMinor, Currency: order.Currency,
		Status: domain.TxUnmatched, OccurredAt: occurred,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if !inserted {
		t.Fatal("first insert reported inserted=false")
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

	// The same provider reference again: no error, no second row, the
	// first row's amount is not overwritten, and the caller is handed the
	// row that is actually stored — not a fresh struct that was never
	// written (a phantom id a later MarkTransactionMatched could target).
	again, againInserted, err := s.InsertPaymentTransaction(ctx, nil, store.PaymentTransactionInput{
		Provider: "sepay", ProviderRef: ref, AmountMinor: 1, Currency: order.Currency,
		Status: domain.TxUnmatched, OccurredAt: occurred,
	})
	if err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	if again == nil {
		t.Fatal("duplicate returned no row")
	}
	if againInserted {
		t.Fatal("duplicate reported inserted=true")
	}
	if again.ID != row.ID {
		t.Fatalf("duplicate returned id %q, want the stored %q", again.ID, row.ID)
	}
	if again.AmountMinor != order.TotalMinor {
		t.Fatalf("duplicate amount = %d, want the first row's %d", again.AmountMinor, order.TotalMinor)
	}
	var n int
	if err := s.DB.NewRaw("SELECT count(*) FROM payment_transactions WHERE provider = ? AND provider_ref = ?",
		"sepay", ref).Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}

	// Marking the returned (stored) row matched reaches the row that
	// exists.
	if err := s.MarkTransactionMatched(ctx, again.ID, order.ID); err != nil {
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
		row, inserted, err := s.InsertPaymentTransaction(ctx, tx, store.PaymentTransactionInput{
			Provider: "payos", ProviderRef: ref, OrderID: &order.ID, AmountMinor: order.TotalMinor,
			Currency: order.Currency, Status: domain.TxMatched, OccurredAt: time.Now(),
		})
		if err != nil {
			return err
		}
		if !inserted {
			t.Fatal("insert inside the transaction reported inserted=false")
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

// The transfer's direction and the bank reference/content the unmatched
// queue shows are stored as given, and the attempt link survives; the
// zero value still lands on the 'in' default the schema asks for.
func TestPaymentTransactionPersistsSpecColumns(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)
	order, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatal(err)
	}
	attempt := &model.PaymentAttempt{
		OrderID: order.ID, Provider: "sepay", Status: domain.AttemptSucceeded,
		AmountMinor: order.TotalMinor, Currency: order.Currency,
	}
	if err := s.CreatePaymentAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	ref := "tx-cols-" + uuid.NewString()[:13]
	cleanupPaymentTransactions(t, s, ref)

	row, inserted, err := s.InsertPaymentTransaction(ctx, nil, store.PaymentTransactionInput{
		Provider: "sepay", ProviderRef: ref, OrderID: &order.ID, AttemptID: &attempt.ID,
		AmountMinor: order.TotalMinor, Currency: order.Currency, Direction: "out",
		Reference: "FT24012345678", Content: "KG7F3K2Q thanh toan",
		Status: domain.TxUnmatched, OccurredAt: time.Now(),
	})
	if err != nil || !inserted {
		t.Fatalf("insert = %v, inserted %v", err, inserted)
	}
	var direction, reference, content string
	var attemptID sql.NullString
	if err := s.DB.NewRaw(`SELECT direction, reference, content, attempt_id
		FROM payment_transactions WHERE id = ?`, row.ID).
		Scan(ctx, &direction, &reference, &content, &attemptID); err != nil {
		t.Fatal(err)
	}
	if direction != "out" || reference != "FT24012345678" || content != "KG7F3K2Q thanh toan" {
		t.Fatalf("row = %q, %q, %q", direction, reference, content)
	}
	if !attemptID.Valid || attemptID.String != attempt.ID {
		t.Fatalf("attempt_id = %v, want %s", attemptID, attempt.ID)
	}

	// An empty direction is the schema's 'in', never ''.
	plainRef := "tx-cols-in-" + uuid.NewString()[:13]
	cleanupPaymentTransactions(t, s, plainRef)
	plain, _, err := s.InsertPaymentTransaction(ctx, nil, store.PaymentTransactionInput{
		Provider: "sepay", ProviderRef: plainRef, AmountMinor: 1, Currency: "VND",
		Status: domain.TxUnmatched, OccurredAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB.NewRaw("SELECT direction FROM payment_transactions WHERE id = ?", plain.ID).
		Scan(ctx, &direction); err != nil {
		t.Fatal(err)
	}
	if direction != "in" {
		t.Fatalf("direction = %q, want the 'in' default", direction)
	}
}
