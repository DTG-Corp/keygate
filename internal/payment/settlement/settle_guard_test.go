package settlement_test

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strings"
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

// A provider event that reports no amount cannot settle a priced order:
// zero is not "the provider did not say" for a real transfer, it is a
// mismatch, and it must never buy a licence for free.
func TestSettleZeroAmountDoesNotGrant(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, plan := seedPricedPlan(t, s, "perpetual", "", 0)
	email := "zero-" + uuid.NewString()[:8] + "@example.com"
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: prod.ID, PlanID: plan.ID, Email: email, Kind: "new", Quantity: 1,
		SubtotalMinor: 1190000, TotalMinor: 1190000, Currency: "VND", Provider: "payos",
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	webhooks, wh := webhookFixture(t, s, prod.ID, "order.paid", "payment.succeeded")
	svc := &settlement.Service{Store: s, Webhooks: webhooks, Log: testLogger()}
	ev := provider.WebhookEvent{Provider: "payos", ProviderEventID: "zero-1", ProviderRef: "zero-" + uuid.NewString()[:8],
		OrderCode: order.Code, AmountMinor: 0, Currency: "VND",
		Status: domain.OrderPaid, TransferType: "in", Raw: []byte(`{}`), OccurredAt: time.Now()}
	if err := svc.Settle(ctx, order.ID, "payos", ev); err != nil {
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
	if n := countUnmatchedForOrder(t, s, order.ID); n != 1 {
		t.Fatalf("unmatched transactions = %d, want 1", n)
	}
	if deliveries := deliveriesByEvent(t, s, wh.ID); len(deliveries) != 0 {
		t.Fatalf("deliveries = %v, want none", deliveries)
	}
}

// The amount matching is not enough when the currency is not the
// order's: 1,190,000 of something else is not this order's money.
func TestSettleWrongCurrencyDoesNotGrant(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, plan := seedPricedPlan(t, s, "perpetual", "", 0)
	email := "currency-" + uuid.NewString()[:8] + "@example.com"
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: prod.ID, PlanID: plan.ID, Email: email, Kind: "new", Quantity: 1,
		SubtotalMinor: 1190000, TotalMinor: 1190000, Currency: "VND", Provider: "payos",
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := &settlement.Service{Store: s, Log: testLogger()}
	ev := provider.WebhookEvent{Provider: "payos", ProviderEventID: "cur-1", ProviderRef: "cur-" + uuid.NewString()[:8],
		OrderCode: order.Code, AmountMinor: 1190000, Currency: "USD",
		Status: domain.OrderPaid, TransferType: "in", Raw: []byte(`{}`), OccurredAt: time.Now()}
	if err := svc.Settle(ctx, order.ID, "payos", ev); err != nil {
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
	if n := countUnmatchedForOrder(t, s, order.ID); n != 1 {
		t.Fatalf("unmatched transactions = %d, want 1", n)
	}
}

// Late money against a terminal order is acked for the operator (the
// unmatched queue) but grants nothing, emits nothing, and is logged.
func TestSettleTerminalOrderRecordsUnmatchedWithoutEvents(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, plan := seedPricedPlan(t, s, "perpetual", "", 0)
	email := "terminal-" + uuid.NewString()[:8] + "@example.com"
	order := newOrder(t, s, prod, plan, "new", email)
	if _, err := s.DB.NewRaw("UPDATE payment_orders SET status = ? WHERE id = ?", domain.OrderExpired, order.ID).
		Exec(ctx); err != nil {
		t.Fatal(err)
	}
	webhooks, wh := webhookFixture(t, s, prod.ID, "order.paid", "payment.succeeded", "payment.failed")
	var logged bytes.Buffer
	svc := &settlement.Service{Store: s, Webhooks: webhooks,
		Log: slog.New(slog.NewTextHandler(&logged, nil))}
	ev := provider.WebhookEvent{Provider: "sepay", ProviderEventID: "term-1", ProviderRef: "term-" + uuid.NewString()[:8],
		OrderCode: order.Code, AmountMinor: 1190000, Currency: "VND",
		Status: domain.OrderPaid, TransferType: "in", Raw: []byte(`{}`), OccurredAt: time.Now()}
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OrderExpired || got.PaidAt != nil {
		t.Fatalf("order = %+v, want expired and unpaid", got)
	}
	if got.LicenseID != "" {
		t.Fatalf("license_id = %q on an expired order", got.LicenseID)
	}
	if n := countLicences(t, s, email); n != 0 {
		t.Fatalf("licences = %d, want 0", n)
	}
	if n := countUnmatchedForOrder(t, s, order.ID); n != 1 {
		t.Fatalf("unmatched transactions = %d, want 1", n)
	}
	if deliveries := deliveriesByEvent(t, s, wh.ID); len(deliveries) != 0 {
		t.Fatalf("deliveries = %v, want none", deliveries)
	}
	if !strings.Contains(logged.String(), "terminal") {
		t.Fatalf("log does not mention the terminal order: %s", logged.String())
	}
}
