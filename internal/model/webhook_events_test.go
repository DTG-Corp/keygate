package model_test

import (
	"testing"

	"github.com/tabloy/keygate/internal/model"
)

// The phase-1a payment rails emit exactly these four events (spec §11).
// Delivery matches WebhookEvents by name, so a constant that is not
// registered would never reach a subscriber.
func TestPaymentWebhookEventsRegistered(t *testing.T) {
	for _, e := range []string{
		model.EventOrderCreated, model.EventOrderPaid,
		model.EventPaymentSucceeded, model.EventPaymentFailed,
	} {
		if !model.IsValidWebhookEvent(e) {
			t.Fatalf("%q is not in WebhookEvents", e)
		}
	}
	// The event names are the wire contract; pin them.
	want := map[string]string{
		model.EventOrderCreated:     "order.created",
		model.EventOrderPaid:        "order.paid",
		model.EventPaymentSucceeded: "payment.succeeded",
		model.EventPaymentFailed:    "payment.failed",
	}
	for got, name := range want {
		if got != name {
			t.Fatalf("event %q, want %q", got, name)
		}
	}
}
