package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/domain"
)

// Refund sentinels. The admin API branches on them; anything else is a
// database fault and must not be reported as a refused refund.
var (
	ErrPaymentRefundInvalid      = errors.New("payment refund is invalid")
	ErrPaymentRefundExceedsOrder = errors.New("payment refund exceeds the order total")
	ErrOrderNotRefundable        = errors.New("payment order is not refundable")
)

// validRefundMethod mirrors the payment_refunds.method CHECK. Phase 1a
// only moves money by hand: an operator makes the bank transfer and
// records it. The other methods exist in the schema for the provider
// refund APIs a later phase adds, and the HTTP layer refuses them until
// those calls exist.
func validRefundMethod(method string) bool {
	switch method {
	case "manual", "sepay", "payos":
		return true
	}
	return false
}

// RecordPaymentRefund records money returned against a paid order and
// moves the order to refunded (or partially_refunded) in the same
// transaction.
//
// The order row is locked first, so two operators recording a refund at
// the same moment cannot each see the full remaining balance and both
// take it. The sum counts every refund that is not failed: a requested
// or pending refund has already promised that money back, and letting a
// second operator refund it again is how an order gets refunded twice.
//
// The caller fills AmountMinor, Currency, Method and Reason; the id,
// status and times are the store's to set. ProviderRef, when set, is the
// bank reference of the return transfer and is kept for reconciliation.
func (s *Store) RecordPaymentRefund(ctx context.Context, r *model.PaymentRefund) error {
	if r == nil || r.OrderID == "" || r.AmountMinor <= 0 || strings.TrimSpace(r.Currency) == "" || !validRefundMethod(r.Method) {
		return ErrPaymentRefundInvalid
	}
	return s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		order, err := s.GetPaymentOrderForUpdate(ctx, tx, r.OrderID)
		if err != nil {
			return err
		}
		// A refunded order reaches the balance check with nothing left,
		// which is the honest answer ("exceeds the order") rather than
		// "not refundable" for an order that was refundable a moment ago.
		// Pending and closed orders never get that far.
		if order.Status != domain.OrderPaid && order.Status != domain.OrderPartiallyRefunded &&
			order.Status != domain.OrderRefunded {
			return ErrOrderNotRefundable
		}
		if !strings.EqualFold(strings.TrimSpace(order.Currency), strings.TrimSpace(r.Currency)) {
			return ErrPaymentRefundInvalid
		}
		var refunded int64
		if err := tx.NewSelect().Model((*model.PaymentRefund)(nil)).
			ColumnExpr("COALESCE(SUM(amount_minor), 0)").
			Where("order_id = ?", order.ID).
			Where("status IN ('requested','pending','succeeded')").
			Scan(ctx, &refunded); err != nil {
			return err
		}
		if refunded+r.AmountMinor > order.TotalMinor {
			return ErrPaymentRefundExceedsOrder
		}

		if r.ID == "" {
			r.ID = NewID()
		}
		if r.Status == "" {
			r.Status = "succeeded"
		}
		now := time.Now()
		r.CreatedAt, r.UpdatedAt = now, now
		if _, err := tx.NewInsert().Model(r).Exec(ctx); err != nil {
			return err
		}

		status := domain.OrderPartiallyRefunded
		if refunded+r.AmountMinor == order.TotalMinor {
			status = domain.OrderRefunded
		}
		_, err = tx.NewUpdate().Model((*model.PaymentOrder)(nil)).
			Set("status = ?", status).Set("updated_at = ?", now).
			Where("id = ?", order.ID).Exec(ctx)
		return err
	})
}
