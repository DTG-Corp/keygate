package coupons

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/settlement"
	"github.com/tabloy/keygate/internal/store"
)

// couponOrder writes an order for the fixture's product/plan carrying the
// given discount snapshot and coupon metadata block, and marks it paid
// unless pending is set. It is the redemption tests' one way of producing
// a settled order.
func (f *quoteFixture) couponOrder(discounts []store.OrderDiscount, block map[string]any, subtotal, discount, total int64, pending bool) *model.PaymentOrder {
	f.t.Helper()
	metadata := []byte("{}")
	if block != nil {
		metadata = mustJSON(f.t, map[string]any{MetadataKey: block})
	}
	order, err := f.s.CreatePaymentOrder(f.ctx, store.CreateOrderInput{
		ProductID: f.product.ID, PlanID: f.plan.ID, Email: "buyer@example.com", Kind: "new",
		Quantity: 1, SubtotalMinor: subtotal, DiscountMinor: discount, TotalMinor: total,
		Currency: "VND", Provider: "sepay", ExpiresAt: time.Now().Add(time.Hour),
		Buyer: []byte("{}"), Metadata: metadata, Discounts: discounts,
	})
	if err != nil {
		f.t.Fatalf("create order: %v", err)
	}
	if pending {
		return order
	}
	paid, err := f.s.MarkPaymentOrderPaid(f.ctx, nil, order.ID, time.Now())
	if err != nil {
		f.t.Fatalf("mark paid: %v", err)
	}
	f.t.Cleanup(func() {
		_, _ = f.s.DB.NewRaw("DELETE FROM coupon_redemptions WHERE order_id = ?", order.ID).Exec(context.Background())
		_, _ = f.s.DB.NewRaw("DELETE FROM payment_orders WHERE id = ?", order.ID).Exec(context.Background())
	})
	return paid
}

func TestRedeemForOrderWritesRowsAndBumpsCounters(t *testing.T) {
	f := newQuoteFixture(t)
	percent := f.create(nil)
	fixed := f.create(func(in *store.CouponInput) {
		in.Type, in.Value, in.StackGroup, in.Currency = "fixed", 50_000, "campaign", "VND"
	})
	order := f.couponOrder([]store.OrderDiscount{
		{CouponID: percent.ID, CouponCode: percent.Code, Type: "percent", Value: 1000, AmountMinor: 100_000, Priority: 100},
		{CouponID: fixed.ID, CouponCode: fixed.Code, Type: "fixed", Value: 50_000, AmountMinor: 50_000, Priority: 200},
	}, map[string]any{
		"codes": []string{percent.Code, fixed.Code}, "external_customer_id": "ws_9",
	}, 1_000_000, 150_000, 850_000, false)

	if err := f.service().RedeemForOrder(f.ctx, order); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	rows, total, err := f.s.ListRedemptions(f.ctx, store.RedemptionFilter{Page: store.Page{Limit: 10}})
	if err != nil || total == 0 {
		t.Fatalf("rows = %+v total = %d err = %v", rows, total, err)
	}
	found := 0
	for _, row := range rows {
		if row.OrderID != order.ID {
			continue
		}
		found++
		if row.ExternalCustomerID != "ws_9" {
			t.Fatalf("external id = %q", row.ExternalCustomerID)
		}
	}
	if found != 2 {
		t.Fatalf("redemptions for order = %d, want 2", found)
	}
	for _, c := range []*model.Coupon{percent, fixed} {
		got, err := f.s.GetCoupon(f.ctx, c.ID)
		if err != nil || got.RedeemedCount != 1 {
			t.Fatalf("coupon %s redeemed_count = %+v err = %v", c.Code, got, err)
		}
	}
	stats, err := f.s.CouponRedemptionStats(f.ctx, percent.ID)
	if err != nil || stats.Count != 1 || stats.AmountMinor != 100_000 {
		t.Fatalf("stats = %+v err = %v", stats, err)
	}
}

func TestRedeemForOrderIsIdempotentUnderConcurrency(t *testing.T) {
	f := newQuoteFixture(t)
	coupons := []*model.Coupon{
		f.create(nil),
		f.create(func(in *store.CouponInput) {
			in.Type, in.Value, in.StackGroup, in.Currency = "fixed", 10_000, "campaign", "VND"
		}),
	}
	discounts := make([]store.OrderDiscount, 0, len(coupons))
	for i, c := range coupons {
		discounts = append(discounts, store.OrderDiscount{
			CouponID: c.ID, CouponCode: c.Code, Type: c.Type, Value: c.Value,
			AmountMinor: 10_000, Priority: (i + 1) * 100,
		})
	}
	order := f.couponOrder(discounts, nil, 1_000_000, 20_000, 980_000, false)

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers*2)
	for i := 0; i < workers*2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.service().RedeemForOrder(f.ctx, order); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent redeem: %v", err)
	}

	for _, c := range coupons {
		rows, _, err := f.s.ListRedemptions(f.ctx, store.RedemptionFilter{CouponID: c.ID, Page: store.Page{Limit: 100}})
		if err != nil {
			t.Fatalf("rows: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("coupon %s has %d redemptions, want 1", c.Code, len(rows))
		}
		got, err := f.s.GetCoupon(f.ctx, c.ID)
		if err != nil || got.RedeemedCount != 1 {
			t.Fatalf("coupon %s redeemed_count = %+v err = %v", c.Code, got, err)
		}
	}
}

func TestRedemptionOnlyAtSettlement(t *testing.T) {
	f := newQuoteFixture(t)
	c := f.create(nil)
	discount := []store.OrderDiscount{{
		CouponID: c.ID, CouponCode: c.Code, Type: "percent", Value: 1000, AmountMinor: 100_000, Priority: 100,
	}}
	// A pending order that was never paid: the hook must not spend the
	// code, and an abandoned checkout leaves the counter at zero.
	pending := f.couponOrder(discount, nil, 1_000_000, 100_000, 900_000, true)
	f.service().AfterPaid(f.ctx, pending, nil, "sepay")
	rows, total, err := f.s.ListRedemptions(f.ctx, store.RedemptionFilter{CouponID: c.ID, Page: store.Page{Limit: 10}})
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	if total != 0 || len(rows) != 0 {
		t.Fatalf("pending order created redemptions: %+v", rows)
	}
	got, err := f.s.GetCoupon(f.ctx, c.ID)
	if err != nil || got.RedeemedCount != 0 {
		t.Fatalf("redeemed_count = %+v err = %v", got, err)
	}
}

func TestSettlementHookRedeemsOnce(t *testing.T) {
	f := newQuoteFixture(t)
	c := f.create(nil)
	order := f.couponOrder([]store.OrderDiscount{{
		CouponID: c.ID, CouponCode: c.Code, Type: "percent", Value: 1000, AmountMinor: 100_000, Priority: 100,
	}}, map[string]any{"codes": []string{c.Code}}, 1_000_000, 100_000, 900_000, true)

	settler := &settlement.Service{Store: f.s, PaidHook: f.service()}
	ev := provider.WebhookEvent{
		Provider: "sepay", ProviderEventID: "evt-" + order.Code, ProviderRef: "ref-" + order.Code,
		OrderCode: order.Code, AmountMinor: order.TotalMinor, Currency: order.Currency,
		Status: domain.OrderPaid, TransferType: "in", OccurredAt: time.Now(), Raw: []byte(`{}`),
	}
	if err := settler.Settle(f.ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, err := f.s.GetCoupon(f.ctx, c.ID)
	if err != nil || got.RedeemedCount != 1 {
		t.Fatalf("after settle redeemed_count = %+v err = %v", got, err)
	}
	// A redelivery is an ack, not a second grant and not a second
	// redemption.
	if err := settler.Settle(f.ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("settle again: %v", err)
	}
	got, err = f.s.GetCoupon(f.ctx, c.ID)
	if err != nil || got.RedeemedCount != 1 {
		t.Fatalf("after redelivery redeemed_count = %+v err = %v", got, err)
	}
	rows, _, err := f.s.ListRedemptions(f.ctx, store.RedemptionFilter{CouponID: c.ID, Page: store.Page{Limit: 10}})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v err = %v", rows, err)
	}
}

func TestTrialRedemptionComesFromMetadata(t *testing.T) {
	f := newQuoteFixture(t)
	trial := f.create(func(in *store.CouponInput) {
		in.Type, in.Value, in.TrialDays = "trial", 0, 30
	})
	order := f.couponOrder(nil, map[string]any{
		"codes": []string{trial.Code},
		"trial": map[string]any{
			"coupon_id": trial.ID, "code": trial.Code, "trial_days": 30,
		},
	}, 1_000_000, 1_000_000, 0, false)

	if err := f.service().RedeemForOrder(f.ctx, order); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	rows, _, err := f.s.ListRedemptions(f.ctx, store.RedemptionFilter{CouponID: trial.ID, Page: store.Page{Limit: 10}})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v err = %v", rows, err)
	}
	if rows[0].Type != "trial" || rows[0].TrialDays != 30 || rows[0].AmountMinor != 1_000_000 {
		t.Fatalf("trial row = %+v", rows[0])
	}
	got, err := f.s.GetCoupon(f.ctx, trial.ID)
	if err != nil || got.RedeemedCount != 1 {
		t.Fatalf("redeemed_count = %+v err = %v", got, err)
	}
}

func TestRedeemSkipsDeletedCoupon(t *testing.T) {
	f := newQuoteFixture(t)
	c := f.create(nil)
	order := f.couponOrder([]store.OrderDiscount{{
		CouponID: c.ID, CouponCode: c.Code, Type: "percent", Value: 1000, AmountMinor: 100_000, Priority: 100,
	}}, nil, 1_000_000, 100_000, 900_000, false)
	// Deleting a coupon that was never redeemed sets its discount
	// snapshots to NULL; the code on the order stays as evidence.
	if _, err := f.s.DB.NewRaw("DELETE FROM coupons WHERE id = ?", c.ID).Exec(f.ctx); err != nil {
		t.Fatalf("delete coupon: %v", err)
	}
	if err := f.service().RedeemForOrder(f.ctx, order); err != nil {
		t.Fatalf("a paid order must not fail on a deleted coupon: %v", err)
	}
}
