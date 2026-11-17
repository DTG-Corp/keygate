// Package hooks composes the post-settlement work phase 1b hangs off a
// paid order: invoice creation and coupon redemption. settlement.Service
// exposes exactly one PaidHook field, and both writers are required, so
// the Lead wires them as one chain in cmd/server/main.go.
package hooks

import (
	"context"
	"log/slog"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/settlement"
)

// Chain runs every hook in order. It satisfies settlement.PaidHook.
//
// One hook failing must not stop the others: an invoice that could not
// be written must not also swallow the coupon redemption, and neither
// may turn a settled payment into a provider retry. Panics are caught
// per hook for the same reason.
type Chain []settlement.PaidHook

// AfterPaid implements settlement.PaidHook.
func (c Chain) AfterPaid(ctx context.Context, order *model.PaymentOrder, lic *model.License, providerName string) {
	for i, hook := range c {
		if hook == nil {
			continue
		}
		runOne(ctx, i, hook, order, lic, providerName)
	}
}

// runOne isolates one hook's panic so the chain continues and the log
// names which writer failed.
func runOne(ctx context.Context, i int, hook settlement.PaidHook, order *model.PaymentOrder, lic *model.License, providerName string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("paid hook panicked",
				"index", i, "order", orderCode(order), "panic", r)
		}
	}()
	hook.AfterPaid(ctx, order, lic, providerName)
}

// orderCode is log-only context; the chain never branches on it.
func orderCode(order *model.PaymentOrder) string {
	if order == nil {
		return ""
	}
	return order.Code
}
