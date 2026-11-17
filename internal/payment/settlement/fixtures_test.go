package settlement_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/service"
	"github.com/dtg-corp/dsoft/internal/store"
)

// seedPricedPlan creates a product and a plan of the given shape, with
// every row a settlement can leave behind removed when the test ends.
func seedPricedPlan(t *testing.T, s *store.Store, licenseType, interval string, trialDays int) (*model.Product, *model.Plan) {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:13]
	prod := &model.Product{Name: "Settle Co", Slug: "settle-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Plan", Slug: "settle-plan-" + suffix,
		LicenseType: licenseType, BillingInterval: interval, TrialDays: trialDays,
		LicenseModel: "standard", MaxActivations: 3, Active: true,
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.DB.NewRaw("DELETE FROM webhook_deliveries WHERE webhook_id IN (SELECT id FROM webhooks WHERE product_id = ?)", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM webhooks WHERE product_id = ?", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM payment_events WHERE order_id IN (SELECT id FROM payment_orders WHERE product_id = ?)", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM payment_transactions WHERE order_id IN (SELECT id FROM payment_orders WHERE product_id = ?)", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM license_renewals WHERE license_id IN (SELECT id FROM licenses WHERE product_id = ?)", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM subscriptions WHERE license_id IN (SELECT id FROM licenses WHERE product_id = ?)", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM payment_orders WHERE product_id = ?", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM licenses WHERE product_id = ?", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM plans WHERE id = ?", plan.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", prod.ID).Exec(ctx)
	})
	return prod, plan
}

// newOrder places a pending order for the plan.
func newOrder(t *testing.T, s *store.Store, prod *model.Product, plan *model.Plan, kind, email string) *model.PaymentOrder {
	t.Helper()
	order, err := s.CreatePaymentOrder(context.Background(), store.CreateOrderInput{
		ProductID: prod.ID, PlanID: plan.ID, Email: email, Kind: kind, Quantity: 1,
		SubtotalMinor: 1190000, TotalMinor: 1190000, Currency: "VND", Provider: "sepay",
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return order
}

// webhookFixture subscribes a real WebhookService to the product's
// events and returns it with the webhook row. Dispatch writes its
// delivery rows synchronously, so a test can read them when Settle
// returns; the goroutine's POST goes nowhere and does not affect the
// assertions.
func webhookFixture(t *testing.T, s *store.Store, productID string, events ...string) (*service.WebhookService, *model.Webhook) {
	t.Helper()
	ctx := context.Background()
	wh := &model.Webhook{
		ProductID: productID, URL: "http://127.0.0.1:1/dsoft-hook",
		Secret: "test-secret", Events: events, Active: true,
	}
	if err := s.CreateWebhook(ctx, wh); err != nil {
		t.Fatal(err)
	}
	svc := service.NewWebhookService(s, slog.New(slog.NewTextHandler(discardWriter{}, nil)), 200*time.Millisecond, 0, true)
	return svc, wh
}

// deliveriesByEvent reads the deliveries the webhook received, keyed by
// event name, with the "data" object of each payload decoded.
func deliveriesByEvent(t *testing.T, s *store.Store, webhookID string) map[string][]map[string]any {
	t.Helper()
	rows, total, err := s.ListWebhookDeliveries(context.Background(), store.WebhookDeliveryFilter{WebhookID: webhookID})
	if err != nil {
		t.Fatal(err)
	}
	if total != len(rows) {
		t.Fatalf("delivery total = %d, read %d", total, len(rows))
	}
	out := make(map[string][]map[string]any, len(rows))
	for _, row := range rows {
		data, ok := row.Payload["data"].(map[string]any)
		if !ok {
			// The map may have gone through JSONB and come back as
			// map[string]any already, which is the expected shape.
			raw, err := json.Marshal(row.Payload["data"])
			if err != nil {
				t.Fatalf("payload data: %v", err)
			}
			if err := json.Unmarshal(raw, &data); err != nil {
				t.Fatalf("decode payload data: %v", err)
			}
		}
		out[row.Event] = append(out[row.Event], data)
	}
	return out
}

// discarded is an io.Writer that drops the webhook sender's log noise.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
