package service

import (
	"context"
	"log/slog"

	"github.com/dtg-corp/dsoft/internal/model"
)

// WebhookDispatcher is the one method the phase-1b emitters use. The
// production implementation is *WebhookService; tests pass a recorder.
// Keeping the surface this narrow is what lets the reconcile worker and
// the payment handlers depend on an emitter without importing the
// delivery machinery.
type WebhookDispatcher interface {
	Dispatch(ctx context.Context, productID, event string, data map[string]any)
}

var _ WebhookDispatcher = (*WebhookService)(nil)

// OrderEventEmitter emits the three phase-1b order/payment events this
// module owns. Every method is safe on a nil emitter and a nil
// dispatcher: emitting is best-effort, the state transition is the job.
type OrderEventEmitter struct {
	Webhooks WebhookDispatcher
	Log      *slog.Logger
}

// NewOrderEventEmitter builds an emitter. Either argument may be nil.
func NewOrderEventEmitter(webhooks WebhookDispatcher, log *slog.Logger) *OrderEventEmitter {
	return &OrderEventEmitter{Webhooks: webhooks, Log: log}
}

// OrderExpired announces an order the expiry sweep moved out of pending.
// The caller emits only for rows the sweep actually changed.
func (e *OrderEventEmitter) OrderExpired(ctx context.Context, order *model.PaymentOrder) {
	e.emit(ctx, model.EventOrderExpired, order, nil)
}

// OrderCanceled announces an order an admin or its buyer canceled. It is
// called after the guarded status update succeeded, never before.
func (e *OrderEventEmitter) OrderCanceled(ctx context.Context, order *model.PaymentOrder) {
	e.emit(ctx, model.EventOrderCanceled, order, nil)
}

// PaymentRefunded announces one recorded refund. The order carries the
// new status; the refund block adds what the order cannot: the amount,
// the operator's reason and how the money went back.
func (e *OrderEventEmitter) PaymentRefunded(ctx context.Context, order *model.PaymentOrder, refund *model.PaymentRefund) {
	if refund == nil {
		return
	}
	e.emit(ctx, model.EventPaymentRefunded, order, map[string]any{
		"refund": map[string]any{
			"amount": refund.AmountMinor,
			"reason": refund.Reason,
			"method": refund.Method,
		},
	})
}

func (e *OrderEventEmitter) emit(ctx context.Context, event string, order *model.PaymentOrder, extra map[string]any) {
	if e == nil || e.Webhooks == nil || order == nil {
		return
	}
	data := map[string]any{"order": order}
	for k, v := range extra {
		data[k] = v
	}
	e.Webhooks.Dispatch(ctx, order.ProductID, event, data)
}
