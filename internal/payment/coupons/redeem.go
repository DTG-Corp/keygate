package coupons

import (
	"context"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/payment/settlement"
	"github.com/dtg-corp/dsoft/internal/store"
)

// RedeemForOrder records every coupon a paid order used: one
// coupon_redemptions row per code, and one increment of each coupon's
// redeemed_count.
//
// It is called at settlement, never at order creation, so an abandoned
// checkout consumes no inventory. The write is idempotent — the ledger is
// unique on (order_id, coupon_code) — so a redelivered settlement or a
// crash retry cannot double-count. An order that is not paid is left
// alone: redemptions are evidence of money taken, not of a code typed.
func (s *Service) RedeemForOrder(ctx context.Context, order *model.PaymentOrder) error {
	if order == nil || order.Status != domain.OrderPaid {
		return nil
	}
	rows, err := s.Store.ListOrderDiscounts(ctx, order.ID)
	if err != nil {
		return err
	}
	block := FromOrderMetadata(order)
	in := make([]store.RedemptionInput, 0, len(rows)+1)
	for _, row := range rows {
		in = append(in, store.RedemptionInput{
			CouponID: row.CouponID, CouponCode: row.CouponCode, OrderID: order.ID,
			Email: order.Email, ExternalCustomerID: block.ExternalCustomerID,
			Type: row.Type, AmountMinor: row.AmountMinor,
		})
	}
	if trial := block.Trial; trial != nil {
		// A trial is never a payment_order_discounts row: that table's
		// type CHECK only admits money coupons, and a trial takes the
		// whole subtotal. The metadata block is its snapshot.
		in = append(in, store.RedemptionInput{
			CouponID: trial.CouponID, CouponCode: trial.Code, OrderID: order.ID,
			Email: order.Email, ExternalCustomerID: block.ExternalCustomerID,
			Type: "trial", AmountMinor: order.DiscountMinor, TrialDays: trial.TrialDays,
		})
	}
	if len(in) == 0 {
		return nil
	}
	return s.Store.RedeemCouponsForOrder(ctx, in)
}

// AfterPaid is the settlement.PaidHook implementation: the licence has
// been granted and the order marked paid, so the coupons are now spent.
// A failure is logged, never returned: the PaidHook contract forbids
// turning a settled payment into a provider retry storm, and the
// reconciliation report shows the missing redemption so an operator can
// re-run it.
func (s *Service) AfterPaid(ctx context.Context, order *model.PaymentOrder, _ *model.License, _ string) {
	if order == nil {
		return
	}
	if err := s.RedeemForOrder(ctx, order); err != nil {
		s.logger().Error("coupons: redemption after settlement failed",
			"order", order.Code, "order_id", order.ID, "error", err)
	}
}

// Service implements settlement.PaidHook; the Lead composes it with the
// invoice hook when wiring the settlement service.
var _ settlement.PaidHook = (*Service)(nil)
