package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/service"
)

type recordedEvent struct {
	event string
	data  map[string]any
}

type eventRecorder struct {
	events []recordedEvent
}

func (r *eventRecorder) Dispatch(_ context.Context, _ string, event string, data map[string]any) {
	r.events = append(r.events, recordedEvent{event: event, data: data})
}

func (r *eventRecorder) emitter() *service.OrderEventEmitter {
	return service.NewOrderEventEmitter(r, nil)
}

func (r *eventRecorder) count(event string) int {
	n := 0
	for _, e := range r.events {
		if e.event == event {
			n++
		}
	}
	return n
}

// The admin cancel is a transition: one event on the first successful
// cancel, nothing when the order already ended.
func TestAdminCancelEmitsOrderCanceledOnce(t *testing.T) {
	f := newAPIFixture(t)
	order := f.adminOrder(t, "sepay", 100)
	rec := new(eventRecorder)
	h := adminPayments(f)
	h.Events = rec.emitter()

	code, out := f.call(h.CancelOrder, http.MethodPost,
		"/api/v1/admin/payments/orders/"+order.Code+"/cancel", "", nil,
		gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusOK {
		t.Fatalf("cancel = %d %v", code, out)
	}
	if rec.count(model.EventOrderCanceled) != 1 {
		t.Fatalf("events = %+v, want one %s", rec.events, model.EventOrderCanceled)
	}
	if got, _ := rec.events[0].data["order"].(*model.PaymentOrder); got == nil || got.ID != order.ID {
		t.Fatalf("event order = %#v, want the canceled order", rec.events[0].data["order"])
	}

	code, out = f.call(h.CancelOrder, http.MethodPost,
		"/api/v1/admin/payments/orders/"+order.Code+"/cancel", "", nil,
		gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusConflict || errCode(out) != "PAYMENT_ORDER_NOT_PAYABLE" {
		t.Fatalf("second cancel = %d %v, want 409", code, out)
	}
	if rec.count(model.EventOrderCanceled) != 1 {
		t.Fatalf("events = %+v, want no second %s", rec.events, model.EventOrderCanceled)
	}
}

// The consumer API's cancel is the same transition and the same rule.
func TestConsumerCancelEmitsOrderCanceledOnce(t *testing.T) {
	f := newPaymentsFixture(t)
	order, _ := f.seedOrder(t, f.planA, f.email("consumer-cancel"), "sepay", 100, false)
	key := f.key(t, model.ScopePaymentsWrite)
	rec := new(eventRecorder)
	f.api.Events = rec.emitter()

	code, out := f.call(f.api.Cancel, http.MethodPost, "/api/v1/payments/orders/"+order.Code+"/cancel",
		"", key, gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusOK {
		t.Fatalf("cancel = %d %v", code, out)
	}
	if rec.count(model.EventOrderCanceled) != 1 {
		t.Fatalf("events = %+v, want one %s", rec.events, model.EventOrderCanceled)
	}

	code, _ = f.call(f.api.Cancel, http.MethodPost, "/api/v1/payments/orders/"+order.Code+"/cancel",
		"", key, gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusConflict {
		t.Fatalf("second cancel = %d, want 409", code)
	}
	if rec.count(model.EventOrderCanceled) != 1 {
		t.Fatalf("events = %+v, want no second %s", rec.events, model.EventOrderCanceled)
	}
}

// A refund that the store accepted is one event. A refund it refused is
// none: the event describes money that moved, not a request.
func TestAdminRefundEmitsPaymentRefundedOncePerRecordedRefund(t *testing.T) {
	f := newAPIFixture(t)
	order := f.adminOrder(t, "sepay", 10_000)
	if _, err := f.s.DB.NewRaw("UPDATE payment_orders SET status = 'paid', paid_at = now() WHERE id = ?", order.ID).
		Exec(f.ctx); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	rec := new(eventRecorder)
	h := adminPayments(f)
	h.Events = rec.emitter()

	code, out := f.call(h.RefundOrder, http.MethodPost,
		"/api/v1/admin/payments/orders/"+order.Code+"/refund",
		`{"amount_minor":4000,"reason":"customer asked","method":"manual"}`, nil,
		gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusOK {
		t.Fatalf("refund = %d %v", code, out)
	}
	if rec.count(model.EventPaymentRefunded) != 1 {
		t.Fatalf("events = %+v, want one %s", rec.events, model.EventPaymentRefunded)
	}
	event := rec.events[0]
	if got, _ := event.data["order"].(*model.PaymentOrder); got == nil || got.ID != order.ID || got.Status != "partially_refunded" {
		t.Fatalf("event order = %#v, want the partially refunded order", event.data["order"])
	}
	refund, _ := event.data["refund"].(map[string]any)
	if refund["amount"] != int64(4000) || refund["reason"] != "customer asked" || refund["method"] != "manual" {
		t.Fatalf("refund block = %#v", event.data["refund"])
	}

	// Over-refund is refused after the full amount: no new event.
	if code, _ := f.call(h.RefundOrder, http.MethodPost,
		"/api/v1/admin/payments/orders/"+order.Code+"/refund", `{"amount_minor":6000}`, nil,
		gin.Param{Key: "code", Value: order.Code}); code != http.StatusOK {
		t.Fatalf("final refund = %d, want 200", code)
	}
	code, out = f.call(h.RefundOrder, http.MethodPost,
		"/api/v1/admin/payments/orders/"+order.Code+"/refund", `{"amount_minor":1}`, nil,
		gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusUnprocessableEntity || errCode(out) != "PAYMENT_REFUND_EXCEEDS_ORDER" {
		t.Fatalf("over-refund = %d %v", code, out)
	}
	if rec.count(model.EventPaymentRefunded) != 2 {
		t.Fatalf("events = %+v, want exactly two successful refunds", rec.events)
	}
}
