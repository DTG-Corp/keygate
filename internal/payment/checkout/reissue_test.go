package checkout_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/checkout"
	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/payment/provider"
	"github.com/dtg-corp/dsoft/internal/payment/provider/fake"
	"github.com/dtg-corp/dsoft/internal/store"
)

// reissueFixture is a checkout service with one priced plan and one
// provider, plus the order its buyer created.
type reissueFixture struct {
	*fixture
	svc *checkout.Service
	fp  *fake.Provider
}

func newReissueFixture(t *testing.T) *reissueFixture {
	t.Helper()
	f := newFixture(t, true)
	f.price("sepay", 1_190_000)
	fp := fake.New("sepay")
	// Each instruction needs its own provider reference: the table keeps
	// one row per reference, and a real provider never repeats one.
	fp.CreateFn = func(_ context.Context, req provider.CreateAttemptRequest) (provider.CreateAttemptResult, error) {
		return provider.CreateAttemptResult{
			ProviderRef: fmt.Sprintf("fake-%s-%d", req.OrderCode, len(fp.Calls)),
			QRPayload:   "fake-qr-" + req.OrderCode, ExpiresAt: req.ExpiresAt,
		}, nil
	}
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fp).providerFactory()}
	return &reissueFixture{fixture: f, svc: svc, fp: fp}
}

func (f *reissueFixture) create(t *testing.T, email string) (*model.PaymentOrder, *model.PaymentAttempt) {
	t.Helper()
	res, err := f.svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: email, Provider: "sepay", Quantity: 1,
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	return res.Order, res.Attempt
}

func (f *reissueFixture) latest(t *testing.T, orderID string) *model.PaymentAttempt {
	t.Helper()
	a, err := f.s.GetLatestPaymentAttempt(f.ctx, orderID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("latest attempt: %v", err)
	}
	return a
}

// A failed/expired instruction is replaced on the same order, at the
// same price: the portal's "Pay" button must never mint a second order.
func TestReissueAttemptReplacesAnUnpayableInstruction(t *testing.T) {
	f := newReissueFixture(t)
	order, first := f.create(t, "reissue-"+f.suffix+"@example.com")
	if err := f.s.UpdatePaymentAttemptStatus(f.ctx, first.ID, domain.AttemptExpired, ""); err != nil {
		t.Fatalf("expire attempt: %v", err)
	}

	second, err := f.svc.ReissueAttempt(f.ctx, order)
	if err != nil {
		t.Fatalf("reissue: %v", err)
	}
	if second.OrderID != order.ID || second.ID == first.ID {
		t.Fatalf("attempt = %+v, want a new attempt on order %s", second, order.ID)
	}
	if second.AmountMinor != order.TotalMinor || second.Currency != order.Currency ||
		second.Provider != order.Provider {
		t.Fatalf("attempt terms = %+v, want the order's %d %s %s",
			second, order.TotalMinor, order.Currency, order.Provider)
	}
	if len(f.fp.Calls) != 2 {
		t.Fatalf("provider calls = %d, want create + reissue", len(f.fp.Calls))
	}
	if got := f.fp.Calls[1]; got.OrderCode != order.Code || got.AmountMinor != order.TotalMinor {
		t.Fatalf("reissue call = %+v, want the existing order's code and total", got)
	}

	// Exactly one order still exists for this purchase.
	var orders int
	if err := f.s.DB.NewRaw("SELECT count(*) FROM payment_orders WHERE email = ?", order.Email).Scan(f.ctx, &orders); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if orders != 1 {
		t.Fatalf("orders for buyer = %d, want 1", orders)
	}
}

// Pressing Pay again while the instruction is still payable hands the
// same QR/link back: no provider call, no second QR.
func TestReissueAttemptReusesAPayableInstruction(t *testing.T) {
	f := newReissueFixture(t)
	order, first := f.create(t, "reuse-"+f.suffix+"@example.com")

	second, err := f.svc.ReissueAttempt(f.ctx, order)
	if err != nil {
		t.Fatalf("reissue: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("attempt = %s, want the still-payable %s", second.ID, first.ID)
	}
	if len(f.fp.Calls) != 1 {
		t.Fatalf("provider calls = %d, want no second call", len(f.fp.Calls))
	}
}

// Terminal orders never get a fresh instruction, whatever page the
// customer still has open in the browser.
func TestReissueAttemptRefusesTerminalAndExpiredOrders(t *testing.T) {
	f := newReissueFixture(t)
	email := "closed-" + f.suffix + "@example.com"

	cases := []struct {
		name  string
		setup func(t *testing.T, order *model.PaymentOrder)
	}{
		{"paid", func(t *testing.T, order *model.PaymentOrder) {
			if _, err := f.s.MarkPaymentOrderPaid(f.ctx, nil, order.ID, time.Now()); err != nil {
				t.Fatalf("mark paid: %v", err)
			}
		}},
		{"canceled", func(t *testing.T, order *model.PaymentOrder) {
			if _, err := f.s.CancelPaymentOrder(f.ctx, order.Code); err != nil {
				t.Fatalf("cancel: %v", err)
			}
		}},
		{"past expiry but still pending", func(t *testing.T, order *model.PaymentOrder) {
			if _, err := f.s.DB.NewRaw("UPDATE payment_orders SET expires_at = now() - interval '1 minute' WHERE id = ?", order.ID).Exec(f.ctx); err != nil {
				t.Fatalf("backdate: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			order, _ := f.create(t, email+"-"+tc.name)
			tc.setup(t, order)
			calls := len(f.fp.Calls)
			if _, err := f.svc.ReissueAttempt(f.ctx, order); !errors.Is(err, store.ErrOrderNotPayable) {
				t.Fatalf("reissue err = %v, want ErrOrderNotPayable", err)
			}
			if len(f.fp.Calls) != calls {
				t.Fatalf("provider calls = %d, want none after refusal", len(f.fp.Calls)-calls)
			}
		})
	}
}

// A provider failure is recorded as a failed attempt and reported as the
// provider's error, so the portal can render "try again" without losing
// the audit trail.
func TestReissueAttemptRecordsProviderFailure(t *testing.T) {
	f := newReissueFixture(t)
	order, first := f.create(t, "fail-"+f.suffix+"@example.com")
	if err := f.s.UpdatePaymentAttemptStatus(f.ctx, first.ID, domain.AttemptFailed, ""); err != nil {
		t.Fatalf("fail attempt: %v", err)
	}
	f.fp.CreateFn = func(context.Context, provider.CreateAttemptRequest) (provider.CreateAttemptResult, error) {
		return provider.CreateAttemptResult{}, errors.New("provider down")
	}

	if _, err := f.svc.ReissueAttempt(f.ctx, order); !errors.Is(err, checkout.ErrProviderError) {
		t.Fatalf("reissue err = %v, want ErrProviderError", err)
	}
	latest := f.latest(t, order.ID)
	if latest == nil || latest.Status != domain.AttemptFailed {
		t.Fatalf("latest attempt = %+v, want a failed attempt", latest)
	}
}
