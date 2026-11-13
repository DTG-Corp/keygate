package settlement_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/settlement"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

// A settled new order emits order.paid and payment.succeeded exactly
// once, carrying the shape spec §8 promises: the licence's id and prefix
// (never the key), and the provider payment. A redelivery emits nothing.
func TestSettleDispatchesSpecEventsOnce(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, plan := seedPricedPlan(t, s, "subscription", "month", 0)
	email := "events-" + uuid.NewString()[:8] + "@example.com"
	order := newOrder(t, s, prod, plan, "new", email)
	webhooks, wh := webhookFixture(t, s, prod.ID, model.EventOrderPaid, model.EventPaymentSucceeded)
	svc := &settlement.Service{Store: s, Webhooks: webhooks, Log: testLogger()}
	occurred := time.Now().UTC().Truncate(time.Second)
	ev := provider.WebhookEvent{Provider: "sepay", ProviderEventID: "evt-1", ProviderRef: "evt-ref-1",
		OrderCode: order.Code, AmountMinor: 1190000, Currency: "VND", Status: domain.OrderPaid,
		TransferType: "in", OccurredAt: occurred, Raw: []byte(`{"id":"evt-ref-1"}`)}
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("settle: %v", err)
	}

	deliveries := deliveriesByEvent(t, s, wh.ID)
	if len(deliveries[model.EventOrderPaid]) != 1 {
		t.Fatalf("order.paid deliveries = %d, want 1", len(deliveries[model.EventOrderPaid]))
	}
	if len(deliveries[model.EventPaymentSucceeded]) != 1 {
		t.Fatalf("payment.succeeded deliveries = %d, want 1", len(deliveries[model.EventPaymentSucceeded]))
	}
	lic, err := s.FindLicenseByID(ctx, order.ID)
	if err != nil {
		t.Fatalf("licence: %v", err)
	}
	paid, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}

	paidData := deliveries[model.EventOrderPaid][0]
	if got := nestedString(paidData, "order", "code"); got != order.Code {
		t.Fatalf("order.paid order.code = %q, want %q", got, order.Code)
	}
	if got := nestedString(paidData, "order", "status"); got != domain.OrderPaid {
		t.Fatalf("order.paid order.status = %q", got)
	}
	if got := nestedString(paidData, "license", "id"); got != lic.ID {
		t.Fatalf("order.paid license.id = %q, want %q", got, lic.ID)
	}
	if got := nestedString(paidData, "license", "key_prefix"); got != "KG" {
		t.Fatalf("order.paid license.key_prefix = %q, want KG", got)
	}
	if got := nestedString(paidData, "license", "plan_id"); got != plan.ID {
		t.Fatalf("order.paid license.plan_id = %q, want %q", got, plan.ID)
	}
	licenseObj, _ := paidData["license"].(map[string]any)
	if licenseObj == nil || licenseObj["valid_until"] == nil {
		t.Fatalf("order.paid license.valid_until missing: %v", paidData["license"])
	}
	if got := nestedString(paidData, "payment", "provider"); got != "sepay" {
		t.Fatalf("order.paid payment.provider = %q", got)
	}
	if got := nestedString(paidData, "payment", "provider_ref"); got != "evt-ref-1" {
		t.Fatalf("order.paid payment.provider_ref = %q", got)
	}
	if payment, _ := paidData["payment"].(map[string]any); payment == nil || payment["paid_at"] == nil {
		t.Fatalf("order.paid payment.paid_at missing: %v", paidData["payment"])
	}
	if paid.PaidAt == nil {
		t.Fatal("the stored order has no paid_at")
	}
	// The key itself is a secret: only its prefix may leave the process.
	if raw, err := json.Marshal(paidData); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(raw), lic.LicenseKey) {
		t.Fatalf("order.paid payload leaks the licence key: %s", raw)
	}

	succeeded := deliveries[model.EventPaymentSucceeded][0]
	if got := nestedString(succeeded, "order", "code"); got != order.Code {
		t.Fatalf("payment.succeeded order.code = %q", got)
	}
	if got := nestedString(succeeded, "provider"); got != "sepay" {
		t.Fatalf("payment.succeeded provider = %q", got)
	}
	if got := nestedString(succeeded, "payment", "provider"); got != "sepay" {
		t.Fatalf("payment.succeeded payment.provider = %q", got)
	}
	if got := nestedString(succeeded, "payment", "provider_ref"); got != "evt-ref-1" {
		t.Fatalf("payment.succeeded payment.provider_ref = %q", got)
	}
	if got := nestedString(succeeded, "payment", "reference"); got != "evt-ref-1" {
		t.Fatalf("payment.succeeded payment.reference = %q", got)
	}
	if payment, _ := succeeded["payment"].(map[string]any); payment == nil || payment["amount_minor"] != float64(1190000) {
		t.Fatalf("payment.succeeded amount = %v, want 1190000", payment["amount_minor"])
	}
	if got := nestedString(succeeded, "payment", "currency"); got != "VND" {
		t.Fatalf("payment.succeeded currency = %q", got)
	}
	if payment, _ := succeeded["payment"].(map[string]any); payment == nil || payment["occurred_at"] == nil {
		t.Fatalf("payment.succeeded occurred_at missing: %v", succeeded["payment"])
	}

	// A replay of the same delivery is acked but grants and emits nothing.
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("replay: %v", err)
	}
	again := deliveriesByEvent(t, s, wh.ID)
	if len(again[model.EventOrderPaid]) != 1 || len(again[model.EventPaymentSucceeded]) != 1 {
		t.Fatalf("replay emitted again: order.paid=%d payment.succeeded=%d",
			len(again[model.EventOrderPaid]), len(again[model.EventPaymentSucceeded]))
	}
}

// A provider failure notification is surfaced as payment.failed with the
// reason under payment.failure_reason — never as a grant, and never with
// an "error" key.
func TestSettleFailedPaymentEmitsFailureReason(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, plan := seedPricedPlan(t, s, "perpetual", "", 0)
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: prod.ID, PlanID: plan.ID, Email: "failed-" + uuid.NewString()[:8] + "@example.com",
		Kind: "new", Quantity: 1, SubtotalMinor: 1190000, TotalMinor: 1190000,
		Currency: "VND", Provider: "payos", ExpiresAt: time.Now().Add(time.Hour),
		Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	webhooks, wh := webhookFixture(t, s, prod.ID, model.EventOrderPaid, model.EventPaymentSucceeded, model.EventPaymentFailed)
	svc := &settlement.Service{Store: s, Webhooks: webhooks, Log: testLogger()}
	ev := provider.WebhookEvent{Provider: "payos", ProviderEventID: "fail-1", ProviderRef: "fail-ref-1",
		OrderCode: order.Code, Status: "failed", TransferType: "in",
		OccurredAt: time.Now(), Raw: []byte(`{}`)}
	if err := svc.Settle(ctx, order.ID, "payos", ev); err != nil {
		t.Fatalf("settle: %v", err)
	}
	deliveries := deliveriesByEvent(t, s, wh.ID)
	if len(deliveries[model.EventPaymentFailed]) != 1 {
		t.Fatalf("payment.failed deliveries = %d, want 1", len(deliveries[model.EventPaymentFailed]))
	}
	if len(deliveries[model.EventOrderPaid]) != 0 || len(deliveries[model.EventPaymentSucceeded]) != 0 {
		t.Fatalf("a failed payment granted events: %v", deliveries)
	}
	failed := deliveries[model.EventPaymentFailed][0]
	if got := nestedString(failed, "order", "code"); got != order.Code {
		t.Fatalf("payment.failed order.code = %q", got)
	}
	payment, _ := failed["payment"].(map[string]any)
	if payment == nil {
		t.Fatalf("payment.failed has no payment object: %v", failed)
	}
	if reason, _ := payment["failure_reason"].(string); reason == "" {
		t.Fatalf("payment.failure_reason = %v, want a reason", payment["failure_reason"])
	}
	if _, hasError := failurePayloadError(failed); hasError {
		t.Fatalf("payment.failed carries an error key: %v", failed)
	}
	// The payment is on the ledger for the operator, and no licence exists.
	if n := countUnmatchedForOrder(t, s, order.ID); n != 1 {
		t.Fatalf("unmatched transactions = %d, want 1", n)
	}
	paid, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if paid.Status != domain.OrderPending || paid.PaidAt != nil {
		t.Fatalf("order = %+v, want pending and unpaid", paid)
	}
}

// A confirmed provider payment whose grant fails is not a payment
// failure: no payment.failed is emitted. The order keeps the error, the
// transfer is on the unmatched queue with the reason, and the error is
// returned so the provider retries; the retry then grants and emits the
// paid events exactly once.
func TestSettleGrantFailureRetriesWithoutPaymentFailed(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, plan := seedPricedPlan(t, s, "perpetual", "", 0)
	email := "grantfail-" + uuid.NewString()[:8] + "@example.com"
	order := newOrder(t, s, prod, plan, "new", email)
	webhooks, wh := webhookFixture(t, s, prod.ID, model.EventOrderPaid, model.EventPaymentSucceeded, model.EventPaymentFailed)
	if _, err := s.DB.NewRaw("UPDATE plans SET active = false WHERE id = ?", plan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	svc := &settlement.Service{Store: s, Webhooks: webhooks, Log: testLogger()}
	ev := provider.WebhookEvent{Provider: "sepay", ProviderEventID: "grant-1", ProviderRef: "grant-ref-1",
		OrderCode: order.Code, AmountMinor: 1190000, Currency: "VND", Status: domain.OrderPaid,
		TransferType: "in", OccurredAt: time.Now(), Raw: []byte(`{"id":"grant-ref-1"}`)}
	err := svc.Settle(ctx, order.ID, "sepay", ev)
	if err == nil {
		t.Fatal("settle on an inactive plan returned nil, want the grant error")
	}
	if !strings.Contains(err.Error(), "not active") {
		t.Fatalf("settle error = %v", err)
	}
	pending, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != domain.OrderPending {
		t.Fatalf("order = %q after a failed grant, want pending", pending.Status)
	}
	if pending.LastSettlementError == "" {
		t.Fatal("last_settlement_error is empty after a failed grant")
	}
	if n := countLicences(t, s, email); n != 0 {
		t.Fatalf("licences = %d after a failed grant, want 0", n)
	}
	if n := countUnmatchedForOrder(t, s, order.ID); n != 1 {
		t.Fatalf("unmatched transactions = %d, want 1", n)
	}
	var reason string
	if err := s.DB.NewRaw("SELECT ignore_reason FROM payment_transactions WHERE order_id = ? AND status = ?",
		order.ID, domain.TxUnmatched).Scan(ctx, &reason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "not active") {
		t.Fatalf("recorded failure reason = %q, want the grant error", reason)
	}
	if deliveries := deliveriesByEvent(t, s, wh.ID); len(deliveries) != 0 {
		t.Fatalf("a failed grant emitted events: %v", deliveries)
	}

	// The operator fixes the plan; the provider's retry completes the
	// settlement, turns the queued transfer into a matched one, and
	// emits the paid events once.
	if _, err := s.DB.NewRaw("UPDATE plans SET active = true WHERE id = ?", plan.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("retry settle: %v", err)
	}
	paid, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if paid.Status != domain.OrderPaid || paid.LicenseID == "" {
		t.Fatalf("order = %+v, want paid with a licence", paid)
	}
	if paid.LastSettlementError != "" {
		t.Fatalf("last_settlement_error = %q after the retry", paid.LastSettlementError)
	}
	var matched int
	if err := s.DB.NewRaw("SELECT count(*) FROM payment_transactions WHERE order_id = ? AND status = ?",
		order.ID, domain.TxMatched).Scan(ctx, &matched); err != nil {
		t.Fatal(err)
	}
	if matched != 1 {
		t.Fatalf("matched transactions = %d, want 1", matched)
	}
	deliveries := deliveriesByEvent(t, s, wh.ID)
	if len(deliveries[model.EventOrderPaid]) != 1 || len(deliveries[model.EventPaymentSucceeded]) != 1 {
		t.Fatalf("retry deliveries = order.paid %d, payment.succeeded %d; want one each",
			len(deliveries[model.EventOrderPaid]), len(deliveries[model.EventPaymentSucceeded]))
	}
	if len(deliveries[model.EventPaymentFailed]) != 0 {
		t.Fatalf("a confirmed payment emitted payment.failed: %v", deliveries[model.EventPaymentFailed])
	}
}

// nestedString reads s[keys...] as a string.
func nestedString(data map[string]any, keys ...string) string {
	var cur any = data
	for _, key := range keys {
		obj, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = obj[key]
	}
	str, _ := cur.(string)
	return str
}

// failurePayloadError reports whether the payload carries a top-level
// "error" or a payment.error key, the shape the spec does not use.
func failurePayloadError(data map[string]any) (any, bool) {
	if v, ok := data["error"]; ok {
		return v, true
	}
	if payment, ok := data["payment"].(map[string]any); ok {
		if v, ok := payment["error"]; ok {
			return v, true
		}
	}
	return nil, false
}

// A pending or expired notice is not a payment failure. Only the statuses
// the rails use for a failed payment emit payment.failed; the rest stay on
// the unmatched ledger silently.
func TestSettlePendingNoticeEmitsNoPaymentFailed(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	prod, plan := seedPricedPlan(t, s, "perpetual", "", 0)
	order := newOrder(t, s, prod, plan, "new", "pending-"+uuid.NewString()[:8]+"@example.com")
	webhooks, wh := webhookFixture(t, s, prod.ID, model.EventOrderPaid, model.EventPaymentSucceeded, model.EventPaymentFailed)
	svc := &settlement.Service{Store: s, Webhooks: webhooks, Log: testLogger()}
	ev := provider.WebhookEvent{Provider: "sepay", ProviderEventID: "pending-1", ProviderRef: "pending-ref-1",
		OrderCode: order.Code, Status: "pending", TransferType: "in",
		OccurredAt: time.Now(), Raw: []byte(`{}`)}
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if deliveries := deliveriesByEvent(t, s, wh.ID); len(deliveries) != 0 {
		t.Fatalf("a pending notice emitted events: %v", deliveries)
	}
	if n := countUnmatchedForOrder(t, s, order.ID); n != 1 {
		t.Fatalf("unmatched transactions = %d, want 1", n)
	}
}
