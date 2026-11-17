package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/internal/testsupport/testdb"
)

// errTestRollback is the failure a transaction test returns to prove the
// write on that transaction is discarded.
var errTestRollback = errors.New("test rollback")

// paymentEventIDs keeps the rows a test writes, so it can remove them
// when it ends: integration tests share one database.
func cleanupPaymentEvents(t *testing.T, s *store.Store, ids ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range ids {
			_, _ = s.DB.NewRaw("DELETE FROM payment_events WHERE event_id = ?", id).Exec(ctx)
		}
	})
}

// A provider event id is recorded once. A replay — the provider's retry,
// or two replicas receiving the same delivery — answers inserted=false
// without an error, so the caller acks without settling again.
func TestPaymentEventInsertOnce(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	eventID := "evt-once-" + suffix
	cleanupPaymentEvents(t, s, eventID)

	occurred := time.Now().UTC().Truncate(time.Second)
	in := store.PaymentEventInput{
		Provider: "sepay", EventID: eventID, Kind: "paid",
		Raw: []byte(`{"transferAmount":1190000}`), OccurredAt: occurred,
	}
	inserted, err := s.RecordPaymentEvent(ctx, nil, in)
	if err != nil || !inserted {
		t.Fatalf("first record = %v, %v; want true, nil", inserted, err)
	}
	inserted, err = s.RecordPaymentEvent(ctx, nil, in)
	if err != nil || inserted {
		t.Fatalf("duplicate = %v, %v; want false, nil", inserted, err)
	}
	var n int
	if err := s.DB.NewRaw("SELECT count(*) FROM payment_events WHERE provider = ? AND event_id = ?", "sepay", eventID).
		Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	var raw string
	if err := s.DB.NewRaw("SELECT raw::text FROM payment_events WHERE provider = ? AND event_id = ?", "sepay", eventID).
		Scan(ctx, &raw); err != nil {
		t.Fatal(err)
	}
	// jsonb's text form normalises whitespace and key order.
	if raw != `{"transferAmount": 1190000}` {
		t.Fatalf("raw = %s", raw)
	}

	// The same event id from a different provider is a different event:
	// the dedupe key is the pair.
	otherID := "evt-other-" + suffix
	cleanupPaymentEvents(t, s, otherID)
	inserted, err = s.RecordPaymentEvent(ctx, nil, store.PaymentEventInput{
		Provider: "payos", EventID: otherID, Kind: "paid", Raw: []byte(`{}`), OccurredAt: occurred,
	})
	if err != nil || !inserted {
		t.Fatalf("other provider = %v, %v; want true, nil", inserted, err)
	}
}

// An empty event id dedupes nothing: every anonymous delivery would
// collapse into one row and the second real event would be dropped.
func TestPaymentEventRejectsEmptyID(t *testing.T) {
	s := testdb.Store(t)
	inserted, err := s.RecordPaymentEvent(context.Background(), nil, store.PaymentEventInput{
		Provider: "sepay", Kind: "paid", Raw: []byte(`{}`),
	})
	if err == nil {
		t.Fatalf("empty event id inserted=%v, want an error", inserted)
	}
	if inserted {
		t.Fatal("an event that was refused must not report inserted")
	}
}

// raw is jsonb: a payload that is not JSON would be rejected by the
// database with an opaque SQLSTATE, so it is refused before the write.
func TestPaymentEventRejectsInvalidRaw(t *testing.T) {
	s := testdb.Store(t)
	eventID := "evt-bad-json-" + uuid.NewString()[:8]
	cleanupPaymentEvents(t, s, eventID)
	inserted, err := s.RecordPaymentEvent(context.Background(), nil, store.PaymentEventInput{
		Provider: "sepay", EventID: eventID, Kind: "paid", Raw: []byte(`{"truncated":`),
	})
	if err == nil {
		t.Fatalf("invalid raw inserted=%v, want an error", inserted)
	}
	var n int
	if err := s.DB.NewRaw("SELECT count(*) FROM payment_events WHERE event_id = ?", eventID).Scan(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rows = %d, want 0", n)
	}
}

// A caller with no payload still leaves an object: raw is NOT NULL and
// readers expect JSON. A missing occurrence time is stamped now rather
// than written as the zero year.
func TestPaymentEventDefaultsRawAndOccurredAt(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	eventID := "evt-defaults-" + uuid.NewString()[:8]
	cleanupPaymentEvents(t, s, eventID)
	inserted, err := s.RecordPaymentEvent(ctx, nil, store.PaymentEventInput{
		Provider: "payos", EventID: eventID, Kind: "paid",
	})
	if err != nil || !inserted {
		t.Fatalf("record = %v, %v", inserted, err)
	}
	var raw string
	var occurred time.Time
	if err := s.DB.NewRaw("SELECT raw::text, occurred_at FROM payment_events WHERE event_id = ?", eventID).
		Scan(ctx, &raw, &occurred); err != nil {
		t.Fatal(err)
	}
	if raw != "{}" {
		t.Fatalf("raw = %s, want {}", raw)
	}
	if occurred.IsZero() || time.Since(occurred) > time.Minute {
		t.Fatalf("occurred_at = %v, want roughly now", occurred)
	}
}

// The event row is written on the caller's transaction when one is
// given, so a settlement that fails after recording the event can take
// the record back and let the provider's retry start clean.
func TestPaymentEventUsesCallerTransaction(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	committedID := "evt-tx-ok-" + uuid.NewString()[:8]
	rolledID := "evt-tx-rollback-" + uuid.NewString()[:8]
	cleanupPaymentEvents(t, s, committedID, rolledID)

	if err := s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		inserted, err := s.RecordPaymentEvent(ctx, tx, store.PaymentEventInput{
			Provider: "sepay", EventID: committedID, Kind: "paid", Raw: []byte(`{}`),
		})
		if err != nil || !inserted {
			t.Fatalf("in tx = %v, %v", inserted, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	errRollback := errTestRollback
	if err := s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := s.RecordPaymentEvent(ctx, tx, store.PaymentEventInput{
			Provider: "sepay", EventID: rolledID, Kind: "paid", Raw: []byte(`{}`),
		}); err != nil {
			return err
		}
		return errRollback
	}); err != errRollback {
		t.Fatalf("rollback tx = %v", err)
	}

	var committed, rolled int
	if err := s.DB.NewRaw("SELECT count(*) FROM payment_events WHERE event_id = ?", committedID).Scan(ctx, &committed); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.NewRaw("SELECT count(*) FROM payment_events WHERE event_id = ?", rolledID).Scan(ctx, &rolled); err != nil {
		t.Fatal(err)
	}
	if committed != 1 || rolled != 0 {
		t.Fatalf("committed rows = %d, rolled back rows = %d; want 1, 0", committed, rolled)
	}
}
