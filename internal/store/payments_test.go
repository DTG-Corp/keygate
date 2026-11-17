package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/internal/testsupport/testdb"
)

// seedProductPlan creates a product and plan for the order tests and
// removes them, and every order hung on them, when the test ends. The
// slug suffix is a UUID rather than a timestamp: integration tests share
// one database, and two runs that start in the same microsecond must not
// collide.
func seedProductPlan(t *testing.T, s *store.Store) (productID, planID string) {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:13]
	p := &model.Product{Name: "PayCo", Slug: "pay-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, p); err != nil {
		t.Fatal(err)
	}
	// LicenseModel is written explicitly: Bun sends the zero value for a
	// notnull field with no `default:`, and the schema's CHECK rejects ''.
	pl := &model.Plan{ProductID: p.ID, Name: "Pro", Slug: "pay-pro-" + suffix, LicenseType: "subscription", LicenseModel: "standard", MaxActivations: 3, Active: true}
	if err := s.CreatePlan(ctx, pl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.DB.NewRaw("DELETE FROM payment_orders WHERE product_id = ?", p.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM plans WHERE id = ?", pl.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", p.ID).Exec(ctx)
	})
	return p.ID, pl.ID
}

func newOrderInput(productID, planID string) store.CreateOrderInput {
	return store.CreateOrderInput{
		ProductID: productID, PlanID: planID, Email: "buyer@example.com", Kind: "new", Quantity: 1,
		SubtotalMinor: 1_190_000, TotalMinor: 1_190_000, Currency: "VND", CurrencyExponent: 0,
		Provider: "sepay", ExpiresAt: time.Now().Add(time.Hour),
		Buyer: []byte(`{"name":"ACME"}`), Metadata: []byte(`{}`),
	}
}

func TestPaymentOrderCreateAndFetch(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)

	in := newOrderInput(productID, planID)
	in.Discounts = []store.OrderDiscount{{
		CouponCode: "LAUNCH10", Type: "percent", Value: 10, AmountMinor: 119_000, Priority: 100,
	}}
	order, err := s.CreatePaymentOrder(ctx, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !domain.IsOrderCode(order.Code) {
		t.Fatalf("code %q not a KG code", order.Code)
	}
	if order.NumericCode == 0 || order.Status != domain.OrderPending {
		t.Fatalf("order = %+v", order)
	}
	got, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil || got.ID != order.ID {
		t.Fatalf("get by code = %+v, %v", got, err)
	}
	got, err = s.GetPaymentOrderByNumericCode(ctx, order.NumericCode)
	if err != nil || got.ID != order.ID {
		t.Fatalf("get by numeric = %+v, %v", got, err)
	}
	if _, err := s.GetPaymentOrderByCode(ctx, "KGZZZZZZ"); !errors.Is(err, store.ErrOrderNotFound) {
		t.Fatalf("missing order = %v, want ErrOrderNotFound", err)
	}

	var discountMinor int64
	if err := s.DB.NewRaw("SELECT amount_minor FROM payment_order_discounts WHERE order_id = ? AND coupon_code = ?",
		order.ID, "LAUNCH10").Scan(ctx, &discountMinor); err != nil {
		t.Fatalf("read discount: %v", err)
	}
	if discountMinor != 119_000 {
		t.Fatalf("discount amount = %d, want 119000", discountMinor)
	}
}

func TestOrderLifecycleGuards(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)
	order, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatal(err)
	}

	// Truncated to the second: Postgres stores microseconds, so a
	// nanosecond-precision value would not compare equal after the round
	// trip, and this assertion is about the first write surviving.
	paidAt := time.Now().Truncate(time.Second)
	paid, err := s.MarkPaymentOrderPaid(ctx, nil, order.ID, paidAt)
	if err != nil || paid.Status != domain.OrderPaid || paid.PaidAt == nil {
		t.Fatalf("mark paid = %+v, %v", paid, err)
	}
	// Paying twice must not move the paid_at or re-grant.
	if _, err := s.MarkPaymentOrderPaid(ctx, nil, order.ID, time.Now().Add(time.Minute)); !errors.Is(err, store.ErrOrderNotPayable) {
		t.Fatalf("double pay = %v, want ErrOrderNotPayable", err)
	}
	stored, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PaidAt == nil || !stored.PaidAt.Equal(paidAt) {
		t.Fatalf("paid_at = %v, want the first payment's %v", stored.PaidAt, paidAt)
	}

	canceled, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.CancelPaymentOrder(ctx, canceled.Code)
	if err != nil || got.Status != domain.OrderCanceled {
		t.Fatalf("cancel = %+v, %v", got, err)
	}
	if _, err := s.MarkPaymentOrderPaid(ctx, nil, canceled.ID, time.Now()); !errors.Is(err, store.ErrOrderNotPayable) {
		t.Fatalf("pay canceled = %v, want ErrOrderNotPayable", err)
	}
}

func TestExpirePaymentOrders(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)
	// Create with a valid future expiry (CreatePaymentOrder rejects an
	// expiry that has already passed), then move it into the past: that is
	// the state a running checkout reaches as time passes.
	in := newOrderInput(productID, planID)
	order, err := s.CreatePaymentOrder(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	backdate := `UPDATE payment_orders SET expires_at = now() - interval '1 minute' WHERE id = ?`
	if _, err := s.DB.NewRaw(backdate, order.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	n, err := s.ExpirePaymentOrders(ctx, time.Now())
	if err != nil || n < 1 {
		t.Fatalf("expire = %d, %v", n, err)
	}
	got, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OrderExpired {
		t.Fatalf("status = %q, want expired", got.Status)
	}
	// A paid order must not be expired afterwards.
	paidIn := newOrderInput(productID, planID)
	paidOrder, err := s.CreatePaymentOrder(ctx, paidIn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw(backdate, paidOrder.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkPaymentOrderPaid(ctx, nil, paidOrder.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExpirePaymentOrders(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	paidGot, err := s.GetPaymentOrderByCode(ctx, paidOrder.Code)
	if err != nil {
		t.Fatal(err)
	}
	if paidGot.Status != domain.OrderPaid {
		t.Fatalf("paid order status = %q, want paid after expiry sweep", paidGot.Status)
	}
}

// TestPaymentOrderRetriesCodeCollision pins the retry path: a duplicate
// code is a unique violation the store swallows and retries, while the
// second call gets a fresh code.
func TestPaymentOrderRetriesCodeCollision(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)

	// The test owns these two codes. Clear any left on the shared test
	// database by an interrupted earlier run before it starts, and again
	// when it ends, so a rerun starts from a known state.
	const clear = `DELETE FROM payment_orders WHERE code IN ('KGAAAAAA', 'KGBBBBBB')`
	t.Cleanup(func() { _, _ = s.DB.NewRaw(clear).Exec(context.Background()) })
	if _, err := s.DB.NewRaw(clear).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	codes := []string{"KGAAAAAA", "KGAAAAAA", "KGBBBBBB"}
	i := 0
	orig := store.OrderCodeGeneratorForTest()
	t.Cleanup(func() { store.SetOrderCodeGeneratorForTest(orig) })
	store.SetOrderCodeGeneratorForTest(func() string {
		if i >= len(codes) {
			t.Fatalf("the order-code generator was called %d times, past the %d scripted codes", i+1, len(codes))
		}
		c := codes[i]
		i++
		return c
	})

	first, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.Code != "KGAAAAAA" || second.Code != "KGBBBBBB" {
		t.Fatalf("first=%q second=%q", first.Code, second.Code)
	}
}

// TestPaymentAttemptLifecycle pins the attempt store: creation fills the
// id, the newest attempt is the one "latest" means, a status update is
// visible, and an order with no attempts answers sql.ErrNoRows.
func TestPaymentAttemptLifecycle(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)
	order, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatal(err)
	}

	base := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	expires := base.Add(15 * time.Minute)
	first := &model.PaymentAttempt{
		OrderID: order.ID, Provider: "sepay", Status: domain.AttemptPending,
		AmountMinor: order.TotalMinor, Currency: order.Currency,
		QRPayload: "000201010212", CheckoutURL: "https://pay.example/checkout/1",
		VANumber:  "9704000000000001",
		Raw:       json.RawMessage(`{"code":"00"}`),
		ExpiresAt: &expires, CreatedAt: base,
	}
	if err := s.CreatePaymentAttempt(ctx, first); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	if first.ID == "" {
		t.Fatal("CreatePaymentAttempt left the id empty")
	}

	got, err := s.GetLatestPaymentAttempt(ctx, order.ID)
	if err != nil {
		t.Fatalf("latest attempt: %v", err)
	}
	if got.ID != first.ID || got.QRPayload != first.QRPayload || got.CheckoutURL != first.CheckoutURL ||
		got.VANumber != first.VANumber || !json.Valid(got.Raw) {
		t.Fatalf("attempt = %+v", got)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Fatalf("expires_at = %v, want %v", got.ExpiresAt, expires)
	}

	if err := s.UpdatePaymentAttemptStatus(ctx, first.ID, domain.AttemptSucceeded, "sepay-ref-1"); err != nil {
		t.Fatalf("update attempt: %v", err)
	}
	got, err = s.GetLatestPaymentAttempt(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.AttemptSucceeded || got.ProviderRef != "sepay-ref-1" {
		t.Fatalf("attempt after update = %+v", got)
	}

	second := &model.PaymentAttempt{
		OrderID: order.ID, Provider: "sepay", ProviderRef: "sepay-ref-2", Status: domain.AttemptCreated,
		AmountMinor: order.TotalMinor, Currency: order.Currency, Raw: json.RawMessage(`{}`),
		CreatedAt: base.Add(time.Minute),
	}
	if err := s.CreatePaymentAttempt(ctx, second); err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	latest, err := s.GetLatestPaymentAttempt(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID != second.ID {
		t.Fatalf("latest = %s, want the newer attempt %s", latest.ID, second.ID)
	}

	if _, err := s.GetLatestPaymentAttempt(ctx, uuid.NewString()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("attempts of an unknown order = %v, want sql.ErrNoRows", err)
	}
}

// TestPaymentOrderDuplicateDiscountIsNotACodeRetry pins what the retry
// loop is for: only a generated code the database rejected. A unique
// violation from another row in the same transaction — here two coupon
// lines with one code — is the caller's bug and must fail on the first
// attempt, not be mistaken for a collision and retried.
func TestPaymentOrderDuplicateDiscountIsNotACodeRetry(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)

	calls := 0
	orig := store.OrderCodeGeneratorForTest()
	t.Cleanup(func() { store.SetOrderCodeGeneratorForTest(orig) })
	store.SetOrderCodeGeneratorForTest(func() string {
		calls++
		return domain.NewOrderCode()
	})

	in := newOrderInput(productID, planID)
	in.Discounts = []store.OrderDiscount{
		{CouponCode: "DUP", Type: "fixed", Value: 100, AmountMinor: 100, Priority: 1},
		{CouponCode: "DUP", Type: "fixed", Value: 100, AmountMinor: 100, Priority: 2},
	}
	if _, err := s.CreatePaymentOrder(ctx, in); err == nil {
		t.Fatal("duplicate coupon lines were accepted")
	}
	if calls != 1 {
		t.Fatalf("the order-code generator ran %d times: a duplicate coupon line is not a code collision", calls)
	}
	var n int
	if err := s.DB.NewRaw("SELECT count(*) FROM payment_orders WHERE product_id = ?", productID).Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d order rows were left by a create that failed", n)
	}
}

// TestPaymentOrderConcurrentPayGrantsOnce is the race the terminal guard
// exists for: settlement calls MarkPaymentOrderPaid with no transaction
// of its own, so the row lock is released with the read. Eight settlers
// racing one order must produce exactly one success; every other attempt
// must answer ErrOrderNotPayable, and the first paid_at must stand.
func TestPaymentOrderConcurrentPayGrantsOnce(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)
	order, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatal(err)
	}

	const settlers = 8
	start := make(chan struct{})
	results := make(chan error, settlers)
	var wg sync.WaitGroup
	for i := 0; i < settlers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.MarkPaymentOrderPaid(ctx, nil, order.ID, time.Now())
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	paidCount := 0
	for err := range results {
		switch {
		case err == nil:
			paidCount++
		case errors.Is(err, store.ErrOrderNotPayable):
		default:
			t.Fatalf("mark paid: %v", err)
		}
	}
	if paidCount != 1 {
		t.Fatalf("%d of %d settlements paid the order, want exactly 1", paidCount, settlers)
	}

	stored, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.OrderPaid || stored.PaidAt == nil {
		t.Fatalf("order after the race = %+v", stored)
	}
}

// TestPaymentOrderNilJSONDefaults pins that a nil (and an empty) buyer or
// metadata is written as the schema's '{}' rather than SQL NULL. The
// columns are NOT NULL, so passing the input through untouched fails the
// insert with an opaque 23502.
func TestPaymentOrderNilJSONDefaults(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)

	for _, tc := range []struct {
		name        string
		buyer, meta []byte
	}{
		{"nil", nil, nil},
		{"empty", []byte{}, []byte{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := newOrderInput(productID, planID)
			in.Buyer, in.Metadata = tc.buyer, tc.meta
			order, err := s.CreatePaymentOrder(ctx, in)
			if err != nil {
				t.Fatalf("create with %s buyer/metadata: %v", tc.name, err)
			}
			got, err := s.GetPaymentOrderByCode(ctx, order.Code)
			if err != nil {
				t.Fatal(err)
			}
			if string(got.Buyer) != "{}" || string(got.Metadata) != "{}" {
				t.Fatalf("buyer = %s, metadata = %s, want both {}", got.Buyer, got.Metadata)
			}
		})
	}
}

// TestPaymentAttemptNilRawDefault pins the same default on an attempt's
// raw provider response: nil would be written as NULL, which the column
// forbids.
func TestPaymentAttemptNilRawDefault(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)
	order, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatal(err)
	}
	a := &model.PaymentAttempt{
		OrderID: order.ID, Provider: "sepay", Status: domain.AttemptCreated,
		AmountMinor: order.TotalMinor, Currency: order.Currency,
	}
	if err := s.CreatePaymentAttempt(ctx, a); err != nil {
		t.Fatalf("create attempt with nil raw: %v", err)
	}
	got, err := s.GetLatestPaymentAttempt(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Raw) != "{}" {
		t.Fatalf("raw = %s, want {}", got.Raw)
	}
}

// TestPaymentAttemptStatusUpdateKeepsProviderRef pins that a status-only
// update (providerRef "") does not clear the reference the attempt was
// created with, which would also free its unique slot for another
// attempt.
func TestPaymentAttemptStatusUpdateKeepsProviderRef(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)
	order, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatal(err)
	}
	a := &model.PaymentAttempt{
		OrderID: order.ID, Provider: "sepay", ProviderRef: "sepay-ref-keep",
		Status: domain.AttemptPending, AmountMinor: order.TotalMinor, Currency: order.Currency,
		Raw: json.RawMessage(`{}`),
	}
	if err := s.CreatePaymentAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}

	if err := s.UpdatePaymentAttemptStatus(ctx, a.ID, domain.AttemptSucceeded, ""); err != nil {
		t.Fatalf("status-only update: %v", err)
	}
	got, err := s.GetLatestPaymentAttempt(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProviderRef != "sepay-ref-keep" {
		t.Fatalf("provider_ref = %q after a status-only update, want the original sepay-ref-keep", got.ProviderRef)
	}
	if got.Status != domain.AttemptSucceeded {
		t.Fatalf("status = %q, want succeeded", got.Status)
	}

	// A non-empty ref still replaces the old one.
	if err := s.UpdatePaymentAttemptStatus(ctx, a.ID, domain.AttemptFailed, "sepay-ref-new"); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetLatestPaymentAttempt(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProviderRef != "sepay-ref-new" || got.Status != domain.AttemptFailed {
		t.Fatalf("attempt after ref replacement = %+v", got)
	}
}

// TestPaymentAttemptLatestDeterministicOnSameTimestamp pins the tie
// breaker: two attempts written at the same created_at must always answer
// the same "latest", the one with the greater id. The smaller id is
// inserted first, so insertion/physical order answers the smaller one and
// cannot pass for the contract's answer.
func TestPaymentAttemptLatestDeterministicOnSameTimestamp(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)
	order, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatal(err)
	}

	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	const highID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	const lowID = "00000000-0000-4000-8000-000000000000"
	for _, id := range []string{lowID, highID} {
		a := &model.PaymentAttempt{
			ID: id, OrderID: order.ID, Provider: "sepay", Status: domain.AttemptCreated,
			AmountMinor: order.TotalMinor, Currency: order.Currency,
			Raw: json.RawMessage(`{}`), CreatedAt: at,
		}
		if err := s.CreatePaymentAttempt(ctx, a); err != nil {
			t.Fatalf("create attempt %s: %v", id, err)
		}
	}

	first, err := s.GetLatestPaymentAttempt(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.GetLatestPaymentAttempt(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != highID || second.ID != highID {
		t.Fatalf("latest on a created_at tie = %q then %q, want the greater id %q both times", first.ID, second.ID, highID)
	}
}

// TestPaymentOrderMarkPaidClearsSettlementError pins the returned
// contract of a successful mark: the stored row no longer carries the
// error a failed settlement left, updated_at moves, and the struct the
// caller gets back is the row that was written.
func TestPaymentOrderMarkPaidClearsSettlementError(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)
	order, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE payment_orders SET last_settlement_error = 'bank timeout' WHERE id = ?", order.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}

	paid, err := s.MarkPaymentOrderPaid(ctx, nil, order.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if paid.LastSettlementError != "" {
		t.Fatalf("returned order still carries settlement error %q", paid.LastSettlementError)
	}
	if paid.Status != domain.OrderPaid || paid.PaidAt == nil {
		t.Fatalf("returned order = %+v", paid)
	}
	if !paid.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("returned updated_at = %v, want later than the pre-mark %v", paid.UpdatedAt, before.UpdatedAt)
	}
	stored, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LastSettlementError != "" {
		t.Fatalf("stored last_settlement_error = %q, want cleared", stored.LastSettlementError)
	}
	if !paid.UpdatedAt.Equal(stored.UpdatedAt) || !paid.PaidAt.Equal(*stored.PaidAt) {
		t.Fatalf("returned order %+v is not the stored row %+v", paid, stored)
	}
}

// TestPaymentOrderCancelClearsSettlementError is the cancel half of the
// same contract.
func TestPaymentOrderCancelClearsSettlementError(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)
	order, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw("UPDATE payment_orders SET last_settlement_error = 'provider 500' WHERE id = ?", order.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}

	canceled, err := s.CancelPaymentOrder(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.LastSettlementError != "" {
		t.Fatalf("returned order still carries settlement error %q", canceled.LastSettlementError)
	}
	if canceled.Status != domain.OrderCanceled {
		t.Fatalf("status = %q, want canceled", canceled.Status)
	}
	if !canceled.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("returned updated_at = %v, want later than the pre-cancel %v", canceled.UpdatedAt, before.UpdatedAt)
	}
	stored, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LastSettlementError != "" || !canceled.UpdatedAt.Equal(stored.UpdatedAt) {
		t.Fatalf("returned order %+v is not the stored row %+v", canceled, stored)
	}
}

// TestPaymentOrderRejectsInvalidInput pins the validation CreatePaymentOrder
// performs before it touches the database: the schema would reject most of
// these too, but with a CHECK/FK error instead of an answer the caller can
// act on. Quantity 0 stays an error rather than silently becoming the
// column default 1.
func TestPaymentOrderRejectsInvalidInput(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)
	ok := newOrderInput(productID, planID)

	cases := []struct {
		name   string
		mutate func(*store.CreateOrderInput)
	}{
		{"zero expires_at", func(in *store.CreateOrderInput) { in.ExpiresAt = time.Time{} }},
		{"past expires_at", func(in *store.CreateOrderInput) { in.ExpiresAt = time.Now().Add(-time.Minute) }},
		{"negative subtotal", func(in *store.CreateOrderInput) { in.SubtotalMinor = -1 }},
		{"negative discount", func(in *store.CreateOrderInput) { in.DiscountMinor = -1 }},
		{"negative tax", func(in *store.CreateOrderInput) { in.TaxMinor = -1 }},
		{"negative total", func(in *store.CreateOrderInput) { in.TotalMinor = -1 }},
		{"zero quantity", func(in *store.CreateOrderInput) { in.Quantity = 0 }},
		{"negative quantity", func(in *store.CreateOrderInput) { in.Quantity = -1 }},
		{"empty product", func(in *store.CreateOrderInput) { in.ProductID = "" }},
		{"empty plan", func(in *store.CreateOrderInput) { in.PlanID = "" }},
		{"empty email", func(in *store.CreateOrderInput) { in.Email = "" }},
		{"empty currency", func(in *store.CreateOrderInput) { in.Currency = "" }},
		{"negative tax rate", func(in *store.CreateOrderInput) { in.TaxRateBP = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := ok
			tc.mutate(&in)
			_, err := s.CreatePaymentOrder(ctx, in)
			if !errors.Is(err, store.ErrOrderInvalidInput) {
				t.Fatalf("CreatePaymentOrder(%s) = %v, want ErrOrderInvalidInput", tc.name, err)
			}
		})
	}

	var n int
	if err := s.DB.NewRaw("SELECT count(*) FROM payment_orders WHERE product_id = ?", productID).Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d orders were written by rejected inputs", n)
	}
}

// TestPaymentAttemptStatusValidation pins the attempt-status vocabulary:
// a status outside the schema CHECK is refused before the database sees
// it, and an update that matches no row answers sql.ErrNoRows.
func TestPaymentAttemptStatusValidation(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)
	order, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatal(err)
	}
	a := &model.PaymentAttempt{
		OrderID: order.ID, Provider: "sepay", Status: domain.AttemptPending,
		AmountMinor: order.TotalMinor, Currency: order.Currency, Raw: json.RawMessage(`{}`),
	}
	if err := s.CreatePaymentAttempt(ctx, a); err != nil {
		t.Fatal(err)
	}

	for _, status := range []string{"paid", "", "SUCCEEDED"} {
		if err := s.UpdatePaymentAttemptStatus(ctx, a.ID, status, ""); !errors.Is(err, store.ErrAttemptStatusInvalid) {
			t.Fatalf("status %q = %v, want ErrAttemptStatusInvalid", status, err)
		}
	}
	got, err := s.GetLatestPaymentAttempt(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.AttemptPending {
		t.Fatalf("status = %q after rejected updates, want pending", got.Status)
	}

	if err := s.UpdatePaymentAttemptStatus(ctx, uuid.NewString(), domain.AttemptFailed, ""); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("update of an unknown attempt = %v, want sql.ErrNoRows", err)
	}
}

// TestPaymentOrderCancelErrorNaming pins the cancel-specific sentinel: a
// paid order cannot be canceled, and that answer must not be
// ErrOrderNotPayable, which settlement owns. Canceling twice is the same
// answer.
func TestPaymentOrderCancelErrorNaming(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)

	paid, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkPaymentOrderPaid(ctx, nil, paid.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelPaymentOrder(ctx, paid.Code); !errors.Is(err, store.ErrOrderNotCancelable) {
		t.Fatalf("cancel paid = %v, want ErrOrderNotCancelable", err)
	} else if errors.Is(err, store.ErrOrderNotPayable) {
		t.Fatalf("cancel paid = %v, must not reuse ErrOrderNotPayable", err)
	}

	order, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelPaymentOrder(ctx, order.Code); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelPaymentOrder(ctx, order.Code); !errors.Is(err, store.ErrOrderNotCancelable) {
		t.Fatalf("second cancel = %v, want ErrOrderNotCancelable", err)
	}
}

// TestPaymentOrderRenewalCollisionIsNotACodeRetry pins the retry
// classifier: only a duplicate payment_orders.code is a collision worth a
// fresh code. The other unique index on the table,
// payment_orders_renewal_once, is a caller error and must fail on the
// first attempt, not be retried five times as "could not allocate a code".
func TestPaymentOrderRenewalCollisionIsNotACodeRetry(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)

	licenseID := store.NewID()
	if _, err := s.DB.NewRaw(
		`INSERT INTO licenses (id, product_id, plan_id, email, license_key) VALUES (?, ?, ?, ?, ?)`,
		licenseID, productID, planID, "renew@example.com", "KG-RENEW-"+licenseID,
	).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.DB.NewRaw("DELETE FROM licenses WHERE id = ?", licenseID).Exec(context.Background())
	})

	calls := 0
	orig := store.OrderCodeGeneratorForTest()
	t.Cleanup(func() { store.SetOrderCodeGeneratorForTest(orig) })
	store.SetOrderCodeGeneratorForTest(func() string {
		calls++
		return domain.NewOrderCode()
	})

	periodEnd := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	in := newOrderInput(productID, planID)
	in.Kind, in.LicenseID, in.PeriodEnd = "renewal", licenseID, &periodEnd
	if _, err := s.CreatePaymentOrder(ctx, in); err != nil {
		t.Fatalf("first renewal: %v", err)
	}
	if calls != 1 {
		t.Fatalf("the order-code generator ran %d times for the first order, want 1", calls)
	}

	if _, err := s.CreatePaymentOrder(ctx, in); err == nil {
		t.Fatal("a second pending renewal for the same license period was accepted")
	} else if strings.Contains(err.Error(), "could not allocate an order code") {
		t.Fatalf("renewal_once collision was retried as an order-code collision: %v", err)
	}
	if calls != 2 {
		t.Fatalf("the order-code generator ran %d times, want 2 (one per create): a renewal_once collision was retried", calls)
	}

	var n int
	if err := s.DB.NewRaw("SELECT count(*) FROM payment_orders WHERE product_id = ?", productID).Scan(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d order rows exist after the rejected renewal, want 1", n)
	}
}
