package reconcile_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/reconcile"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m, "../../../db/migrations")) }

// matchFixture is a product with one plan under it and every row a
// matcher test can leave behind removed when the test ends. The test
// database is shared by every package, so a fixture that only adds rows
// makes the next run match the previous one.
type matchFixture struct {
	t      *testing.T
	s      *store.Store
	ctx    context.Context
	prod   *model.Product
	plan   *model.Plan
	amount int64
}

func newMatchFixture(t *testing.T) *matchFixture {
	t.Helper()
	s := testdb.Store(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:13]
	prod := &model.Product{Name: "Matcher Co", Slug: "matcher-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Pro", Slug: "matcher-plan-" + suffix,
		LicenseType: "perpetual", LicenseModel: "standard", MaxActivations: 3, Active: true,
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	f := &matchFixture{t: t, s: s, ctx: ctx, prod: prod, plan: plan}
	// The amount is unique per test: another test's pending orders in
	// this shared database must not join the amount+window search and
	// turn a single match into an "ambiguous" answer.
	f.amount = time.Now().UnixNano()%1_500_000_000 + 1_000_000
	t.Cleanup(func() {
		ctx := context.Background()
		for _, table := range []string{"payment_transactions", "payment_events"} {
			_, _ = s.DB.NewRaw("DELETE FROM "+table+" WHERE order_id IN (SELECT id FROM payment_orders WHERE product_id = ?)", prod.ID).Exec(ctx)
		}
		_, _ = s.DB.NewRaw("DELETE FROM payment_orders WHERE product_id = ?", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM plans WHERE id = ?", plan.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", prod.ID).Exec(ctx)
	})
	return f
}

// order places one pending order for the fixture's plan. amount overrides
// the fixture's default so a test can seed two orders of different sizes.
func (f *matchFixture) order(email string, amount int64, providerName string) *model.PaymentOrder {
	f.t.Helper()
	o, err := f.s.CreatePaymentOrder(f.ctx, store.CreateOrderInput{
		ProductID: f.prod.ID, PlanID: f.plan.ID, Email: email, Kind: "new", Quantity: 1,
		SubtotalMinor: amount, TotalMinor: amount, Currency: "VND", Provider: providerName,
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return o
}

// match runs the matcher for a SePay transfer.
func (f *matchFixture) match(ev provider.WebhookEvent) (*model.PaymentOrder, bool) {
	f.t.Helper()
	order, ambiguous, err := (&reconcile.Matcher{Store: f.s}).Match(f.ctx, ev.Provider, ev)
	if err != nil {
		f.t.Fatalf("match: %v", err)
	}
	return order, ambiguous
}

// The order code is the only positive answer: two pending orders of the
// same amount must not make a coded transfer ambiguous.
func TestMatcherExactCodeWins(t *testing.T) {
	f := newMatchFixture(t)
	first := f.order("first-"+uuid.NewString()+"@example.com", f.amount, "sepay")
	second := f.order("second-"+uuid.NewString()+"@example.com", f.amount, "sepay")

	order, ambiguous := f.match(provider.WebhookEvent{
		Provider: "sepay", OrderCode: second.Code, AmountMinor: f.amount,
		OccurredAt: time.Now(),
	})
	if ambiguous {
		t.Fatal("a coded transfer reported ambiguity")
	}
	if order == nil || order.ID != second.ID {
		t.Fatalf("match = %+v, want order %s (not %s)", order, second.Code, first.Code)
	}
}

// A code that names an order which is no longer payable is a late
// duplicate, not a match: the money stays in the operator queue.
func TestMatcherCodeOfClosedOrderIsNotAMatch(t *testing.T) {
	f := newMatchFixture(t)
	order := f.order("closed-"+uuid.NewString()+"@example.com", f.amount, "sepay")
	if _, err := f.s.CancelPaymentOrder(f.ctx, order.Code); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	got, ambiguous := f.match(provider.WebhookEvent{
		Provider: "sepay", OrderCode: order.Code, AmountMinor: f.amount,
		OccurredAt: time.Now(),
	})
	if got != nil || ambiguous {
		t.Fatalf("match = %+v, ambiguous=%v; want no match", got, ambiguous)
	}
}

// A transfer that names another rail's order is not a match even when
// the code itself exists.
func TestMatcherCodeOfAnotherProviderIsNotAMatch(t *testing.T) {
	f := newMatchFixture(t)
	order := f.order("other-rail-"+uuid.NewString()+"@example.com", f.amount, "payos")

	got, ambiguous := f.match(provider.WebhookEvent{
		Provider: "sepay", OrderCode: order.Code, AmountMinor: f.amount,
		OccurredAt: time.Now(),
	})
	if got != nil || ambiguous {
		t.Fatalf("match = %+v, ambiguous=%v; want no match", got, ambiguous)
	}
}

// A memo that states an order code which does not exist must not fall
// back to amount matching: the transfer names an order, and attaching it
// to a different one on a coincidence of digits is exactly the mistake
// the queue exists to catch.
func TestMatcherUnknownCodeDoesNotFallBackToAmount(t *testing.T) {
	f := newMatchFixture(t)
	order := f.order("unknown-code-"+uuid.NewString()+"@example.com", f.amount, "sepay")
	_ = order

	got, ambiguous := f.match(provider.WebhookEvent{
		Provider: "sepay", OrderCode: "KGZZZZZZ", AmountMinor: f.amount,
		OccurredAt: time.Now(),
	})
	if got != nil || ambiguous {
		t.Fatalf("match = %+v, ambiguous=%v; want no match", got, ambiguous)
	}
}

// Without a code, an amount that fits exactly one recent pending order is
// a suggestion the operator can confirm.
func TestMatcherAmountAndWindowSingleCandidate(t *testing.T) {
	f := newMatchFixture(t)
	wanted := f.order("window-"+uuid.NewString()+"@example.com", f.amount, "sepay")
	f.order("window-other-"+uuid.NewString()+"@example.com", f.amount+7, "sepay")

	order, ambiguous := f.match(provider.WebhookEvent{
		Provider: "sepay", AmountMinor: f.amount,
		OccurredAt: wanted.CreatedAt.Add(10 * time.Minute),
	})
	if ambiguous {
		t.Fatal("a single amount match reported ambiguity")
	}
	if order == nil || order.ID != wanted.ID {
		t.Fatalf("match = %+v, want order %s", order, wanted.Code)
	}
}

// The amount must land inside the window around the transfer: an order
// created two hours earlier is history, not a suggestion.
func TestMatcherAmountOutsideWindowIsNotAMatch(t *testing.T) {
	f := newMatchFixture(t)
	order := f.order("outside-"+uuid.NewString()+"@example.com", f.amount, "sepay")

	got, ambiguous := f.match(provider.WebhookEvent{
		Provider: "sepay", AmountMinor: f.amount,
		OccurredAt: order.CreatedAt.Add(-2 * time.Hour),
	})
	if got != nil || ambiguous {
		t.Fatalf("match = %+v, ambiguous=%v; want no match", got, ambiguous)
	}
}

// Two pending orders of the same amount inside the window is the
// ambiguous answer: the matcher must not pick one.
func TestMatcherAmbiguousAmountMatch(t *testing.T) {
	f := newMatchFixture(t)
	f.order("amb-a-"+uuid.NewString()+"@example.com", f.amount, "sepay")
	f.order("amb-b-"+uuid.NewString()+"@example.com", f.amount, "sepay")

	got, ambiguous := f.match(provider.WebhookEvent{
		Provider: "sepay", AmountMinor: f.amount, OccurredAt: time.Now(),
	})
	if got != nil || !ambiguous {
		t.Fatalf("match = %+v, ambiguous=%v; want ambiguity", got, ambiguous)
	}
}

// A zero-amount transfer can never be a positive match: free orders would
// otherwise let an empty memo attach money to the wrong row.
func TestMatcherZeroAmountNeverMatches(t *testing.T) {
	f := newMatchFixture(t)
	f.order("zero-a-"+uuid.NewString()+"@example.com", 0, "sepay")
	f.order("zero-b-"+uuid.NewString()+"@example.com", 0, "sepay")

	got, ambiguous := f.match(provider.WebhookEvent{
		Provider: "sepay", AmountMinor: 0, OccurredAt: time.Now(),
	})
	if got != nil || ambiguous {
		t.Fatalf("zero-amount match = %+v, ambiguous=%v; want no match", got, ambiguous)
	}
}
