package settlement_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/settlement"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

func countLicences(t *testing.T, s *store.Store, email string) int {
	t.Helper()
	n, err := s.DB.NewSelect().Table("licenses").Where("email = ?", email).Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func countUnmatchedForOrder(t *testing.T, s *store.Store, orderID string) int {
	t.Helper()
	var n int
	if err := s.DB.NewRaw("SELECT count(*) FROM payment_transactions WHERE order_id = ? AND status = ?",
		orderID, domain.TxUnmatched).Scan(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A transfer that is not the amount the order asked for is a different
// payment: it is recorded for an operator and nothing is granted.
func TestSettleAmountMismatchGrantsNothing(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedSellable(t, s)
	email := "short-" + uuid.NewString()[:8] + "@example.com"
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: productID, PlanID: planID, Email: email, Kind: "new", Quantity: 1,
		SubtotalMinor: 1190000, TotalMinor: 1190000, Currency: "VND", Provider: "sepay",
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := &settlement.Service{Store: s, Log: testLogger()}
	err = svc.Settle(ctx, order.ID, "sepay", provider.WebhookEvent{Provider: "sepay",
		ProviderEventID: "mismatch-1", ProviderRef: "mismatch-" + uuid.NewString()[:8],
		OrderCode: order.Code, AmountMinor: 1, Currency: "VND",
		Status: domain.OrderPaid, TransferType: "in"})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OrderPending {
		t.Fatalf("order = %q, want pending", got.Status)
	}
	if n := countLicences(t, s, email); n != 0 {
		t.Fatalf("licences = %d, want 0", n)
	}
	if n := countUnmatchedForOrder(t, s, order.ID); n < 1 {
		t.Fatalf("unmatched transactions = %d", n)
	}
}

// Money arriving against an order that already expired is recorded and
// left for an operator; it does not reopen the order or buy a licence.
func TestSettleLatePaymentOnExpiredOrderGrantsNothing(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedSellable(t, s)
	email := "late-" + uuid.NewString()[:8] + "@example.com"
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: productID, PlanID: planID, Email: email, Kind: "new", Quantity: 1,
		SubtotalMinor: 1190000, TotalMinor: 1190000, Currency: "VND", Provider: "sepay",
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE payment_orders SET status = ? WHERE id = ?", domain.OrderExpired, order.ID).
		Exec(ctx); err != nil {
		t.Fatal(err)
	}
	svc := &settlement.Service{Store: s, Log: testLogger()}
	err = svc.Settle(ctx, order.ID, "sepay", provider.WebhookEvent{Provider: "sepay",
		ProviderEventID: "late-1", ProviderRef: "late-" + uuid.NewString()[:8],
		OrderCode: order.Code, AmountMinor: 1190000, Currency: "VND",
		Status: domain.OrderPaid, TransferType: "in"})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OrderExpired {
		t.Fatalf("order = %q, want expired", got.Status)
	}
	if got.PaidAt != nil {
		t.Fatalf("paid_at = %v, want nil", got.PaidAt)
	}
	if n := countLicences(t, s, email); n != 0 {
		t.Fatalf("licences = %d, want 0", n)
	}
	if n := countUnmatchedForOrder(t, s, order.ID); n < 1 {
		t.Fatalf("unmatched transactions = %d", n)
	}
}

// An event that names a different provider than the order was created
// for is not that order's payment.
func TestSettleForeignProviderGrantsNothing(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedSellable(t, s)
	email := "foreign-" + uuid.NewString()[:8] + "@example.com"
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: productID, PlanID: planID, Email: email, Kind: "new", Quantity: 1,
		SubtotalMinor: 1190000, TotalMinor: 1190000, Currency: "VND", Provider: "sepay",
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := &settlement.Service{Store: s, Log: testLogger()}
	err = svc.Settle(ctx, order.ID, "payos", provider.WebhookEvent{Provider: "payos",
		ProviderEventID: "foreign-1", ProviderRef: "foreign-" + uuid.NewString()[:8],
		OrderCode: order.Code, AmountMinor: 1190000, Currency: "VND",
		Status: domain.OrderPaid, TransferType: "in"})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OrderPending {
		t.Fatalf("order = %q, want pending", got.Status)
	}
	if n := countLicences(t, s, email); n != 0 {
		t.Fatalf("licences = %d, want 0", n)
	}
	if n := countUnmatchedForOrder(t, s, order.ID); n < 1 {
		t.Fatalf("unmatched transactions = %d", n)
	}
}

// A transfer no order names is still recorded: the operator needs to see
// the money, and the row has no order to point at.
func TestRecordUnmatchedRecordsTransactionWithoutOrder(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	ref := "unmatched-" + uuid.NewString()[:13]
	t.Cleanup(func() {
		_, _ = s.DB.NewRaw("DELETE FROM payment_transactions WHERE provider_ref = ?", ref).Exec(context.Background())
	})
	svc := &settlement.Service{Store: s, Log: testLogger()}
	err := svc.RecordUnmatched(ctx, "sepay", provider.WebhookEvent{Provider: "sepay",
		ProviderEventID: ref, ProviderRef: ref, AmountMinor: 5000, Currency: "VND",
		Status: domain.OrderPaid, TransferType: "in", Raw: []byte(`{"id":1}`), OccurredAt: time.Now()}, nil)
	if err != nil {
		t.Fatalf("record unmatched: %v", err)
	}
	var status string
	var orderID sql.NullString
	var amount int64
	if err := s.DB.NewRaw("SELECT status, order_id, amount_minor FROM payment_transactions WHERE provider = ? AND provider_ref = ?",
		"sepay", ref).Scan(ctx, &status, &orderID, &amount); err != nil {
		t.Fatal(err)
	}
	if status != domain.TxUnmatched || orderID.Valid || amount != 5000 {
		t.Fatalf("row = %q, %v, %d", status, orderID, amount)
	}
}
