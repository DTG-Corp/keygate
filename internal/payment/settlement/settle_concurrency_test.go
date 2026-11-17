package settlement_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/payment/provider"
	"github.com/dtg-corp/dsoft/internal/payment/settlement"
	"github.com/dtg-corp/dsoft/internal/testsupport/testdb"
)

// Spec §10 requires two concurrent webhooks for one order to be safe: both
// deliveries read a pending order, the order's row lock serialises the
// grant, and the loser re-reads it paid and only acks. The order is paid
// once, exactly one licence and one subscription exist, the ledger holds
// one row for the order, and each paid event is delivered once.
//
// The two calls start together from a closed channel; the scenario runs
// five times inside the test (and the file is meant to be run with
// -count too) so a lock that is only sometimes taken shows up. DB-gated
// like the rest of the package: without TEST_DATABASE_URL it skips.
func TestSettleTwoConcurrentWebhooksGrantOnce(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		prod, plan := seedPricedPlan(t, s, "subscription", "month", 0)
		email := fmt.Sprintf("concurrent-%d-%s@example.com", i, uuid.NewString()[:8])
		order := newOrder(t, s, prod, plan, "new", email)
		webhooks, wh := webhookFixture(t, s, prod.ID, model.EventOrderPaid, model.EventPaymentSucceeded)
		svc := &settlement.Service{Store: s, Webhooks: webhooks, Log: testLogger()}
		ev := provider.WebhookEvent{
			Provider: "sepay", ProviderEventID: "concurrent-" + order.ID,
			ProviderRef: "concurrent-ref-" + order.ID, OrderCode: order.Code,
			AmountMinor: 1190000, Currency: "VND", Status: domain.OrderPaid,
			TransferType: "in", OccurredAt: time.Now().UTC().Truncate(time.Second),
			Raw: []byte(`{"id":"concurrent"}`),
		}

		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- svc.Settle(ctx, order.ID, "sepay", ev)
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("iteration %d: a concurrent delivery failed: %v", i, err)
			}
		}

		paid, err := s.GetPaymentOrderByCode(ctx, order.Code)
		if err != nil {
			t.Fatal(err)
		}
		if paid.Status != domain.OrderPaid || paid.PaidAt == nil {
			t.Fatalf("iteration %d: order = %+v, want paid once", i, paid)
		}
		lic, err := s.FindLicenseByID(ctx, order.ID)
		if err != nil {
			t.Fatalf("iteration %d: licence: %v", i, err)
		}
		if paid.LicenseID != lic.ID {
			t.Fatalf("iteration %d: order.license_id = %q, want %q", i, paid.LicenseID, lic.ID)
		}
		if n := countLicences(t, s, email); n != 1 {
			t.Fatalf("iteration %d: licences = %d, want 1", i, n)
		}
		var subs int
		if err := s.DB.NewRaw("SELECT count(*) FROM subscriptions WHERE license_id = ?", lic.ID).
			Scan(ctx, &subs); err != nil {
			t.Fatal(err)
		}
		if subs != 1 {
			t.Fatalf("iteration %d: subscriptions = %d, want 1", i, subs)
		}
		var ledger int
		if err := s.DB.NewRaw("SELECT count(*) FROM payment_transactions WHERE order_id = ?", order.ID).
			Scan(ctx, &ledger); err != nil {
			t.Fatal(err)
		}
		if ledger != 1 {
			t.Fatalf("iteration %d: payment_transactions for the order = %d, want 1", i, ledger)
		}
		deliveries := deliveriesByEvent(t, s, wh.ID)
		if got := len(deliveries[model.EventOrderPaid]); got != 1 {
			t.Fatalf("iteration %d: order.paid deliveries = %d, want 1", i, got)
		}
		if got := len(deliveries[model.EventPaymentSucceeded]); got != 1 {
			t.Fatalf("iteration %d: payment.succeeded deliveries = %d, want 1", i, got)
		}
	}
}
