package billing_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dtg-corp/dsoft/internal/billing"
	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/checkout"
)

// Two replicas wake on the same hour and scan the same due subscription.
// The partial unique index on (license_id, period_end) makes one of them
// lose; the loser must treat the winner's order as the answer, not as an
// error and not as a reason to mint a second order.
func TestConcurrentRunsCreateOneRenewalOrder(t *testing.T) {
	r := newRig(t)
	f := r.seed(seedOpts{End: r.now.Add(3 * 24 * time.Hour), Price: 250_000})

	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = r.svc.RunOnce(context.Background())
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if got := len(r.pendingOrders(f.Licence.ID)); got != 1 {
		t.Fatalf("pending orders = %d, want 1", got)
	}
	// The loser may have raced past the open-order lookup before the
	// winner committed and resumed the same order, which legitimately
	// mints a second instruction for it. What must not happen is a second
	// order.
	if got := len(r.prod.Calls); got < 1 || got > 2 {
		t.Fatalf("provider calls = %d, want one order's instructions", got)
	}
	if got := len(r.events.byEvent(model.EventLicenseRenewalDue)); got != 1 {
		t.Fatalf("renewal-due events = %d, want 1", got)
	}
	if got := len(r.queued(f.Licence.Email)); got > 1 {
		t.Fatalf("queued = %d mails, want at most one rung", got)
	}
}

// One licence failing must not stop the pass for the rest: the failure is
// reported and the next hour retries it.
func TestOneFailingLicenceDoesNotStopThePass(t *testing.T) {
	r := newRig(t)
	broken := r.seed(seedOpts{End: r.now.Add(2 * 24 * time.Hour), Price: 250_000})
	healthy := r.seed(seedOpts{End: r.now.Add(2 * 24 * time.Hour), Price: 250_000})
	r.useCheckout(&failFor{inner: r.checkout, licenseID: broken.Licence.ID})

	err := r.run()
	if err == nil {
		t.Fatal("the failing licence was not reported")
	}
	if got := len(r.pendingOrders(broken.Licence.ID)); got != 0 {
		t.Fatalf("broken licence orders = %d", got)
	}
	if got := len(r.pendingOrders(healthy.Licence.ID)); got != 1 {
		t.Fatalf("healthy licence orders = %d, want 1", got)
	}
}

// failFor fails the order creation of one licence and delegates the rest.
type failFor struct {
	inner     billing.OrderCreator
	licenseID string
}

func (f *failFor) CreateOrder(ctx context.Context, in checkout.CreateOrderInput) (*checkout.OrderResult, error) {
	if in.LicenseID == f.licenseID {
		return nil, errors.New("provider is down")
	}
	return f.inner.CreateOrder(ctx, in)
}
