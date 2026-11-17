package service

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/dtg-corp/dsoft/internal/model"
)

type recordedDispatch struct {
	productID string
	event     string
	data      map[string]any
}

type recordingDispatcher struct {
	dispatches []recordedDispatch
}

func (r *recordingDispatcher) Dispatch(_ context.Context, productID, event string, data map[string]any) {
	r.dispatches = append(r.dispatches, recordedDispatch{productID: productID, event: event, data: data})
}

func testEmitter(r WebhookDispatcher) *OrderEventEmitter {
	return NewOrderEventEmitter(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// The order events carry the order under "order", the same key and shape
// order.created/order.paid already use. A receiver can branch on the
// event name and read one object without a second lookup.
func TestOrderEventEmitterOrderPayloads(t *testing.T) {
	rec := new(recordingDispatcher)
	e := testEmitter(rec)
	order := &model.PaymentOrder{ID: "ord_1", Code: "KGABC123", ProductID: "prod_1", Status: "expired"}

	e.OrderExpired(context.Background(), order)
	e.OrderCanceled(context.Background(), order)

	if len(rec.dispatches) != 2 {
		t.Fatalf("dispatches = %d, want 2", len(rec.dispatches))
	}
	want := []string{model.EventOrderExpired, model.EventOrderCanceled}
	for i, d := range rec.dispatches {
		if d.event != want[i] {
			t.Fatalf("dispatch %d event = %q, want %q", i, d.event, want[i])
		}
		if d.productID != order.ProductID {
			t.Fatalf("dispatch %d product = %q, want %q", i, d.productID, order.ProductID)
		}
		got, ok := d.data["order"].(*model.PaymentOrder)
		if !ok || got != order {
			t.Fatalf("dispatch %d order = %#v, want the order as-is", i, d.data["order"])
		}
	}
}

// The refund event adds only the facts the order does not carry, and the
// amount is integer minor units like every other money field.
func TestOrderEventEmitterRefundPayload(t *testing.T) {
	rec := new(recordingDispatcher)
	e := testEmitter(rec)
	order := &model.PaymentOrder{ID: "ord_1", Code: "KGABC123", ProductID: "prod_1", Status: "refunded"}
	refund := &model.PaymentRefund{OrderID: order.ID, AmountMinor: 4000, Reason: "customer asked", Method: "manual"}

	e.PaymentRefunded(context.Background(), order, refund)

	if len(rec.dispatches) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(rec.dispatches))
	}
	d := rec.dispatches[0]
	if d.event != model.EventPaymentRefunded {
		t.Fatalf("event = %q, want %q", d.event, model.EventPaymentRefunded)
	}
	if got, _ := d.data["order"].(*model.PaymentOrder); got != order {
		t.Fatalf("order = %#v", d.data["order"])
	}
	payload, err := json.Marshal(d.data["refund"])
	if err != nil {
		t.Fatalf("refund marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("refund unmarshal: %v", err)
	}
	if got["amount"] != float64(4000) || got["reason"] != "customer asked" || got["method"] != "manual" {
		t.Fatalf("refund payload = %v, want amount/reason/method", got)
	}
	if len(got) != 3 {
		t.Fatalf("refund payload = %v, want exactly amount, reason and method", got)
	}
}

// A server built without a dispatcher, or a nil emitter left in a
// handler, must not panic on a transition: the state change is the job,
// the event is best-effort.
func TestOrderEventEmitterIsNilSafe(t *testing.T) {
	order := &model.PaymentOrder{ID: "ord_1", ProductID: "prod_1"}
	var nilEmitter *OrderEventEmitter
	nilEmitter.OrderExpired(context.Background(), order)
	nilEmitter.OrderCanceled(context.Background(), order)
	nilEmitter.PaymentRefunded(context.Background(), order, &model.PaymentRefund{})

	rec := new(recordingDispatcher)
	testEmitter(rec).PaymentRefunded(context.Background(), order, nil)
	if len(rec.dispatches) != 0 {
		t.Fatalf("nil refund dispatched %d events, want none", len(rec.dispatches))
	}
}
