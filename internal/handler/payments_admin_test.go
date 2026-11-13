package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/tabloy/keygate/internal/middleware"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/provider/fake"
	"github.com/tabloy/keygate/internal/payment/providercfg"
	"github.com/tabloy/keygate/internal/payment/settlement"
	"github.com/tabloy/keygate/internal/store"
)

// ─── fixture helpers ───

// adminOrder writes one order in the fixture's product A and registers
// the rows a test may add against it for cleanup. Every test owns its
// product, so a filter on product_id cannot see another test's orders.
func (f *apiFixture) adminOrder(t *testing.T, providerName string, amount int64) *model.PaymentOrder {
	t.Helper()
	o, err := f.s.CreatePaymentOrder(f.ctx, store.CreateOrderInput{
		ProductID: f.prodA.ID, PlanID: f.planA.ID, Email: "admin-" + uuid.NewString() + "@example.com",
		Kind: "new", Quantity: 1, SubtotalMinor: amount, TotalMinor: amount, Currency: "VND",
		Provider: providerName, ExpiresAt: time.Now().Add(time.Hour),
		Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, table := range []string{"payment_refunds", "payment_transactions", "payment_events", "payment_attempts"} {
			_, _ = f.s.DB.NewRaw("DELETE FROM "+table+" WHERE order_id = ?", o.ID).Exec(ctx)
		}
		_, _ = f.s.DB.NewRaw("DELETE FROM payment_orders WHERE id = ?", o.ID).Exec(ctx)
		// Settlement grants a licence whose id is the order id.
		_, _ = f.s.DB.NewRaw("DELETE FROM licenses WHERE id = ?", o.ID).Exec(ctx)
	})
	return o
}

// adminPayments builds a handler with no provider config: the admin
// operations below do not need a live rail unless the test says so.
func adminPayments(f *apiFixture) *PaymentsAdminHandler {
	return &PaymentsAdminHandler{
		Store:      f.s,
		Settlement: &settlement.Service{Store: f.s},
	}
}

func refundRow(t *testing.T, f *apiFixture, orderID string) *model.PaymentRefund {
	t.Helper()
	row := new(model.PaymentRefund)
	if err := f.s.DB.NewSelect().Model(row).Where("order_id = ?", orderID).Scan(f.ctx); err != nil {
		t.Fatalf("read refund: %v", err)
	}
	return row
}

// ─── mark-paid ───

func TestAdminMarkPaidRecordsManualSettlement(t *testing.T) {
	f := newAPIFixture(t)
	order := f.adminOrder(t, "sepay", 100)
	h := adminPayments(f)

	code, out := f.call(h.MarkPaid, http.MethodPost, "/api/v1/admin/payments/orders/"+order.Code+"/mark-paid",
		`{"reason":"bank transfer confirmed in app"}`, nil, gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusOK {
		t.Fatalf("mark paid: %d %v", code, out)
	}
	got, err := f.s.GetPaymentOrderByCode(f.ctx, order.Code)
	if err != nil || got.Status != domain.OrderPaid {
		t.Fatalf("order = %+v, %v; want paid", got, err)
	}
	if got.PaidAt == nil {
		t.Fatal("paid_at is not set")
	}
	// Manual settlement is auditable: a refund-able row with method=manual exists.
	n, _ := f.s.DB.NewSelect().Table("payment_transactions").Where("order_id = ?", order.ID).Count(f.ctx)
	if n < 1 {
		t.Fatalf("manual transaction rows = %d", n)
	}
	var tx model.PaymentTransaction
	if err := f.s.DB.NewSelect().Model(&tx).Where("order_id = ?", order.ID).Scan(f.ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tx.ProviderRef, "manual-") {
		t.Fatalf("provider ref = %q, want the manual marker", tx.ProviderRef)
	}
	var raw map[string]any
	if err := json.Unmarshal(tx.Raw, &raw); err != nil || raw["manual"] != true {
		t.Fatalf("raw = %s, want the manual audit payload", tx.Raw)
	}
	if n, _ := f.s.DB.NewSelect().Table("licenses").Where("id = ?", order.ID).Count(f.ctx); n != 1 {
		t.Fatalf("licences = %d, want 1", n)
	}

	// A second click is refused and grants nothing more.
	code, out = f.call(h.MarkPaid, http.MethodPost, "/api/v1/admin/payments/orders/"+order.Code+"/mark-paid",
		`{"reason":"clicked again"}`, nil, gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusConflict || errCode(out) != "PAYMENT_ORDER_NOT_PAYABLE" {
		t.Fatalf("mark paid twice = %d %v, want 409", code, out)
	}
	if n, _ := f.s.DB.NewSelect().Table("licenses").Where("id = ?", order.ID).Count(f.ctx); n != 1 {
		t.Fatalf("licences after a second click = %d, want 1", n)
	}
}

func TestAdminMarkPaidRequiresAReason(t *testing.T) {
	f := newAPIFixture(t)
	order := f.adminOrder(t, "sepay", 100)
	h := adminPayments(f)

	code, out := f.call(h.MarkPaid, http.MethodPost, "/api/v1/admin/payments/orders/"+order.Code+"/mark-paid",
		`{"reason":"  "}`, nil, gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusBadRequest {
		t.Fatalf("mark paid without reason = %d %v, want 400", code, out)
	}
	if got, _ := f.s.GetPaymentOrderByCode(f.ctx, order.Code); got.Status != domain.OrderPending {
		t.Fatalf("order = %q, want pending", got.Status)
	}
}

// Manual settlement grants a licence without money changing hands, so it
// must be impossible on a live rail; the same call is allowed on a test
// row so an operator can rehearse it.
func TestAdminMarkPaidRefusedInLiveMode(t *testing.T) {
	f := newAPIFixture(t)
	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x33}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.cleanupProviderConfigSlots(t, providerConfigSlot{Provider: "sepay", Mode: "live"})
	svc := providercfg.NewService(f.s, codec, "live")
	if _, err := svc.Upsert(f.ctx, providercfg.UpsertInput{
		Provider: "sepay", Mode: "live", Enabled: true,
		Config:        providercfg.SepayConfig{BankName: "MB Bank", BankCode: "MB", AccountNo: "0978482700", AccountName: "NGUYEN VAN A"},
		Credentials:   providercfg.SepayCredentials{APIToken: "tok-live"},
		WebhookSecret: "whsec-live",
	}); err != nil {
		t.Fatalf("upsert live config: %v", err)
	}
	order := f.adminOrder(t, "sepay", 12345)
	h := adminPayments(f)
	h.Config = svc

	code, out := f.call(h.MarkPaid, http.MethodPost, "/api/v1/admin/payments/orders/"+order.Code+"/mark-paid",
		`{"reason":"operator says so"}`, nil, gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusConflict || errCode(out) != "PAYMENT_MANUAL_SETTLEMENT_FORBIDDEN" {
		t.Fatalf("live mark paid = %d %v, want 409 PAYMENT_MANUAL_SETTLEMENT_FORBIDDEN", code, out)
	}
	if got, _ := f.s.GetPaymentOrderByCode(f.ctx, order.Code); got.Status != domain.OrderPending {
		t.Fatalf("order = %q, want pending", got.Status)
	}
}

// ─── order list and detail ───

func TestAdminOrderListFilters(t *testing.T) {
	f := newAPIFixture(t)
	h := adminPayments(f)
	sepayA := f.adminOrder(t, "sepay", 1000)
	sepayB := f.adminOrder(t, "sepay", 2000)
	payosC := f.adminOrder(t, "payos", 3000)
	if _, err := f.s.CancelPaymentOrder(f.ctx, sepayB.Code); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	_ = payosC

	call := func(query string) map[string]any {
		t.Helper()
		return f.callList(h.ListOrders, "/api/v1/admin/payments/orders?"+query)
	}
	all := call("product_id=" + f.prodA.ID)
	if int(all["total"].(float64)) != 3 {
		t.Fatalf("all total = %v, want 3 (%v)", all["total"], all)
	}
	pending := call("product_id=" + f.prodA.ID + "&status=pending")
	if int(pending["total"].(float64)) != 2 {
		t.Fatalf("pending total = %v, want 2", pending["total"])
	}
	payos := call("product_id=" + f.prodA.ID + "&provider=payos")
	if int(payos["total"].(float64)) != 1 {
		t.Fatalf("payos total = %v, want 1", payos["total"])
	}
	byEmail := call("product_id=" + f.prodA.ID + "&email=" + sepayA.Email)
	if int(byEmail["total"].(float64)) != 1 {
		t.Fatalf("email total = %v, want 1", byEmail["total"])
	}
	page := call("product_id=" + f.prodA.ID + "&limit=1&offset=1")
	rows := page["orders"].([]any)
	if len(rows) != 1 || int(page["total"].(float64)) != 3 {
		t.Fatalf("page = %v rows, total %v; want 1 row of 3", len(rows), page["total"])
	}
	if got := int(page["limit"].(float64)); got != 1 {
		t.Fatalf("limit echo = %d", got)
	}

	code, out := f.call(h.ListOrders, http.MethodGet, "/api/v1/admin/payments/orders?status=refunded_pending", "", nil)
	if code != http.StatusBadRequest || errCode(out) != "INVALID_STATUS" {
		t.Fatalf("bad status = %d %v, want 400 INVALID_STATUS", code, out)
	}
}

func TestAdminOrderDetailIncludesProviderRows(t *testing.T) {
	f := newAPIFixture(t)
	h := adminPayments(f)
	order := f.adminOrder(t, "sepay", 5000)
	if err := f.s.CreatePaymentAttempt(f.ctx, &model.PaymentAttempt{
		OrderID: order.ID, Provider: "sepay", ProviderRef: order.Code, Status: domain.AttemptPending,
		AmountMinor: 5000, Currency: "VND", Raw: []byte(`{}`),
	}); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if _, _, err := f.s.InsertPaymentTransaction(f.ctx, nil, store.PaymentTransactionInput{
		Provider: "sepay", ProviderRef: "detail-" + uuid.NewString(), OrderID: &order.ID,
		AmountMinor: 5000, Currency: "VND", Status: domain.TxMatched, OccurredAt: time.Now(),
	}); err != nil {
		t.Fatalf("transaction: %v", err)
	}
	if _, err := f.s.RecordPaymentEvent(f.ctx, nil, store.PaymentEventInput{
		Provider: "sepay", EventID: "detail-" + uuid.NewString(), OrderID: order.ID,
		Kind: "paid", Raw: []byte(`{"ok":true}`), OccurredAt: time.Now(),
	}); err != nil {
		t.Fatalf("event: %v", err)
	}

	var detail struct {
		Order        map[string]any   `json:"order"`
		Attempts     []map[string]any `json:"attempts"`
		Transactions []map[string]any `json:"transactions"`
		Events       []map[string]any `json:"events"`
		Refunds      []map[string]any `json:"refunds"`
	}
	if err := f.decodeData(t, h.GetOrder, &detail, gin.Param{Key: "code", Value: order.Code}); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.Order["code"] != order.Code {
		t.Fatalf("order code = %v", detail.Order["code"])
	}
	if len(detail.Attempts) != 1 || len(detail.Transactions) != 1 || len(detail.Events) != 1 {
		t.Fatalf("rows = %d attempts, %d transactions, %d events", len(detail.Attempts), len(detail.Transactions), len(detail.Events))
	}
	if _, ok := detail.Order["buyer"]; !ok {
		t.Fatal("detail must carry the buyer snapshot")
	}
	if len(detail.Refunds) != 0 {
		t.Fatalf("refunds = %d, want 0", len(detail.Refunds))
	}

	code, out := f.call(h.GetOrder, http.MethodGet, "/api/v1/admin/payments/orders/KGZZZZZZ", "", nil,
		gin.Param{Key: "code", Value: "KGZZZZZZ"})
	if code != http.StatusNotFound || errCode(out) != "PAYMENT_ORDER_NOT_FOUND" {
		t.Fatalf("unknown order = %d %v, want 404", code, out)
	}
}

// ─── cancel ───

func TestAdminCancelOrder(t *testing.T) {
	f := newAPIFixture(t)
	h := adminPayments(f)
	order := f.adminOrder(t, "sepay", 700)
	paid := f.adminOrder(t, "payos", 800)
	if code, out := f.call(h.MarkPaid, "POST", "/api/v1/admin/payments/orders/"+paid.Code+"/mark-paid",
		`{"reason":"already paid"}`, nil, gin.Param{Key: "code", Value: paid.Code}); code != http.StatusOK {
		t.Fatalf("mark paid: %d %v", code, out)
	}

	code, out := f.call(h.CancelOrder, http.MethodPost, "/api/v1/admin/payments/orders/"+order.Code+"/cancel",
		"", nil, gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusOK {
		t.Fatalf("cancel = %d %v", code, out)
	}
	if got, _ := f.s.GetPaymentOrderByCode(f.ctx, order.Code); got.Status != domain.OrderCanceled {
		t.Fatalf("status = %q, want canceled", got.Status)
	}

	code, out = f.call(h.CancelOrder, http.MethodPost, "/api/v1/admin/payments/orders/"+paid.Code+"/cancel",
		"", nil, gin.Param{Key: "code", Value: paid.Code})
	if code != http.StatusConflict || errCode(out) != "PAYMENT_ORDER_NOT_PAYABLE" {
		t.Fatalf("cancel paid = %d %v, want 409", code, out)
	}
	code, out = f.call(h.CancelOrder, http.MethodPost, "/api/v1/admin/payments/orders/KGZZZZZZ/cancel",
		"", nil, gin.Param{Key: "code", Value: "KGZZZZZZ"})
	if code != http.StatusNotFound {
		t.Fatalf("cancel unknown = %d %v, want 404", code, out)
	}
}

// ─── refunds ───

func TestAdminRefundRecordsAndFlipsTheOrder(t *testing.T) {
	f := newAPIFixture(t)
	h := adminPayments(f)
	order := f.adminOrder(t, "sepay", 10_000)
	if code, out := f.call(h.MarkPaid, "POST", "/api/v1/admin/payments/orders/"+order.Code+"/mark-paid",
		`{"reason":"bank transfer"}`, nil, gin.Param{Key: "code", Value: order.Code}); code != http.StatusOK {
		t.Fatalf("mark paid: %d %v", code, out)
	}

	code, out := f.call(h.RefundOrder, http.MethodPost, "/api/v1/admin/payments/orders/"+order.Code+"/refund",
		`{"amount_minor":4000,"reason":"customer asked","method":"manual","provider_ref":"bank-ref-1"}`, nil,
		gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusOK {
		t.Fatalf("partial refund = %d %v", code, out)
	}
	if got, _ := f.s.GetPaymentOrderByCode(f.ctx, order.Code); got.Status != domain.OrderPartiallyRefunded {
		t.Fatalf("status = %q, want partially_refunded", got.Status)
	}
	row := refundRow(t, f, order.ID)
	if row.AmountMinor != 4000 || row.Method != "manual" || row.ProviderRef != "bank-ref-1" || row.Status != "succeeded" {
		t.Fatalf("refund row = %+v", row)
	}

	code, out = f.call(h.RefundOrder, http.MethodPost, "/api/v1/admin/payments/orders/"+order.Code+"/refund",
		`{"amount_minor":6000}`, nil, gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusOK {
		t.Fatalf("final refund = %d %v", code, out)
	}
	if got, _ := f.s.GetPaymentOrderByCode(f.ctx, order.Code); got.Status != domain.OrderRefunded {
		t.Fatalf("status = %q, want refunded", got.Status)
	}
	// The remaining balance is gone: one more unit is refused.
	code, out = f.call(h.RefundOrder, http.MethodPost, "/api/v1/admin/payments/orders/"+order.Code+"/refund",
		`{"amount_minor":1}`, nil, gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusUnprocessableEntity || errCode(out) != "PAYMENT_REFUND_EXCEEDS_ORDER" {
		t.Fatalf("over-refund = %d %v, want 422 PAYMENT_REFUND_EXCEEDS_ORDER", code, out)
	}
}

func TestAdminRefundRefusals(t *testing.T) {
	f := newAPIFixture(t)
	h := adminPayments(f)
	pending := f.adminOrder(t, "sepay", 1000)
	paid := f.adminOrder(t, "sepay", 1000)
	if code, _ := f.call(h.MarkPaid, "POST", "/api/v1/admin/payments/orders/"+paid.Code+"/mark-paid",
		`{"reason":"bank"}`, nil, gin.Param{Key: "code", Value: paid.Code}); code != http.StatusOK {
		t.Fatal("mark paid failed")
	}

	cases := []struct {
		name  string
		order *model.PaymentOrder
		body  string
		code  int
		err   string
	}{
		{"unpaid order", pending, `{"amount_minor":100}`, http.StatusConflict, "PAYMENT_ORDER_NOT_REFUNDABLE"},
		{"zero amount", paid, `{"amount_minor":0}`, http.StatusUnprocessableEntity, "PAYMENT_REFUND_INVALID"},
		{"provider method", paid, `{"amount_minor":100,"method":"payos"}`, http.StatusUnprocessableEntity, "PAYMENT_REFUND_UNSUPPORTED"},
		{"too much", paid, `{"amount_minor":1001}`, http.StatusUnprocessableEntity, "PAYMENT_REFUND_EXCEEDS_ORDER"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := f.call(h.RefundOrder, http.MethodPost, "/api/v1/admin/payments/orders/"+tc.order.Code+"/refund",
				tc.body, nil, gin.Param{Key: "code", Value: tc.order.Code})
			if code != tc.code || errCode(out) != tc.err {
				t.Fatalf("= %d %v, want %d %s", code, out, tc.code, tc.err)
			}
		})
	}
	// A malformed body and an unknown order are different refusals.
	if code, _ := f.call(h.RefundOrder, http.MethodPost, "/api/v1/admin/payments/orders/"+paid.Code+"/refund",
		`{`, nil, gin.Param{Key: "code", Value: paid.Code}); code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d, want 400", code)
	}
	if code, _ := f.call(h.RefundOrder, http.MethodPost, "/api/v1/admin/payments/orders/KGZZZZZZ/refund",
		`{"amount_minor":1}`, nil, gin.Param{Key: "code", Value: "KGZZZZZZ"}); code != http.StatusNotFound {
		t.Fatalf("unknown order = %d, want 404", code)
	}
}

// ─── transactions, unmatched queue and matching ───

func (f *apiFixture) unmatchedTransfer(t *testing.T, providerName string, amount int64, ref string) *model.PaymentTransaction {
	t.Helper()
	tx, _, err := f.s.InsertPaymentTransaction(f.ctx, nil, store.PaymentTransactionInput{
		Provider: providerName, ProviderRef: ref, AmountMinor: amount, Currency: "VND",
		Direction: "in", Reference: "bank-ref-" + uuid.NewString()[:8], Content: "chuyen tien",
		Status: domain.TxUnmatched, OccurredAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("insert transaction: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.s.DB.NewRaw("DELETE FROM payment_transactions WHERE id = ?", tx.ID).Exec(context.Background())
	})
	return tx
}

func TestAdminTransactionListFilters(t *testing.T) {
	f := newAPIFixture(t)
	h := adminPayments(f)
	order := f.adminOrder(t, "sepay", 3000)
	matched, _, err := f.s.InsertPaymentTransaction(f.ctx, nil, store.PaymentTransactionInput{
		Provider: "sepay", ProviderRef: "list-" + uuid.NewString(), OrderID: &order.ID,
		AmountMinor: 3000, Currency: "VND", Status: domain.TxMatched, OccurredAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.s.DB.NewRaw("DELETE FROM payment_transactions WHERE id = ?", matched.ID).Exec(context.Background())
	})
	unmatched := f.unmatchedTransfer(t, "sepay", 4444, "list-unmatched-"+uuid.NewString())

	all := f.callList(h.ListTransactions, "/api/v1/admin/payments/transactions?product_id="+f.prodA.ID)
	if int(all["total"].(float64)) != 1 {
		t.Fatalf("product total = %v, want 1 (%v)", all["total"], all)
	}
	rows := all["transactions"].([]any)
	row := rows[0].(map[string]any)
	if row["id"] != matched.ID || row["status"] != "matched" {
		t.Fatalf("row = %v", row)
	}
	if _, ok := row["raw"]; ok {
		t.Fatal("the list must not carry the raw payload")
	}
	byStatus := f.callList(h.ListTransactions, "/api/v1/admin/payments/transactions?status=unmatched&provider=sepay")
	found := false
	for _, r := range byStatus["transactions"].([]any) {
		if r.(map[string]any)["id"] == unmatched.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("unmatched transfer %s missing from the filtered list", unmatched.ID)
	}
	if code, out := f.call(h.ListTransactions, http.MethodGet, "/api/v1/admin/payments/transactions?status=gone", "", nil); code != http.StatusBadRequest || errCode(out) != "INVALID_STATUS" {
		t.Fatalf("bad transaction status = %d %v", code, out)
	}
}

func TestAdminUnmatchedQueueAndIgnore(t *testing.T) {
	f := newAPIFixture(t)
	h := adminPayments(f)
	tx := f.unmatchedTransfer(t, "sepay", 7777, "queue-"+uuid.NewString())

	var queue struct {
		Transactions []map[string]any `json:"transactions"`
		Count        int              `json:"count"`
	}
	if err := f.decodeData(t, h.ListUnmatched, &queue); err != nil {
		t.Fatalf("decode queue: %v", err)
	}
	found := false
	for _, row := range queue.Transactions {
		if row["id"] == tx.ID {
			found = true
			if row["content"] != "chuyen tien" || row["reference"] == "" {
				t.Fatalf("queue row = %v, want content and reference", row)
			}
		}
	}
	if !found {
		t.Fatalf("transfer %s missing from the queue", tx.ID)
	}

	// Ignoring takes it out of the queue and keeps the reason.
	code, out := f.call(h.IgnoreTransaction, http.MethodPost, "/api/v1/admin/payments/transactions/"+tx.ID+"/ignore",
		`{"reason":"bank fee, not a sale"}`, nil, gin.Param{Key: "id", Value: tx.ID})
	if code != http.StatusOK {
		t.Fatalf("ignore = %d %v", code, out)
	}
	reloaded, err := f.s.GetPaymentTransaction(f.ctx, tx.ID)
	if err != nil || reloaded.Status != domain.TxIgnored || reloaded.IgnoreReason != "bank fee, not a sale" {
		t.Fatalf("transaction = %+v, %v", reloaded, err)
	}

	code, out = f.call(h.IgnoreTransaction, http.MethodPost, "/api/v1/admin/payments/transactions/"+tx.ID+"/ignore",
		`{"reason":"again"}`, nil, gin.Param{Key: "id", Value: tx.ID})
	if code != http.StatusConflict || errCode(out) != "PAYMENT_TRANSACTION_NOT_UNMATCHED" {
		t.Fatalf("ignore twice = %d %v, want 409", code, out)
	}
	code, out = f.call(h.IgnoreTransaction, http.MethodPost, "/api/v1/admin/payments/transactions/not-a-row/ignore",
		`{"reason":"x"}`, nil, gin.Param{Key: "id", Value: "not-a-row"})
	if code != http.StatusNotFound || errCode(out) != "PAYMENT_TRANSACTION_NOT_FOUND" {
		t.Fatalf("ignore unknown = %d %v, want 404", code, out)
	}
	code, out = f.call(h.IgnoreTransaction, http.MethodPost, "/api/v1/admin/payments/transactions/"+tx.ID+"/ignore",
		`{"reason":"  "}`, nil, gin.Param{Key: "id", Value: tx.ID})
	if code != http.StatusBadRequest {
		t.Fatalf("ignore without reason = %d %v, want 400", code, out)
	}
}

func TestAdminMatchTransactionSettlesTheOrder(t *testing.T) {
	f := newAPIFixture(t)
	h := adminPayments(f)
	order := f.adminOrder(t, "sepay", 12_345)
	tx := f.unmatchedTransfer(t, "sepay", 12_345, "match-"+uuid.NewString())
	// matched_by references users(id), so the operator is a real row.
	admin := &model.User{Email: "admin-" + uuid.NewString() + "@example.com", Name: "Admin", Role: model.RoleAdmin}
	if err := f.s.UpsertUser(f.ctx, admin); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.s.DB.NewRaw("DELETE FROM users WHERE id = ?", admin.ID).Exec(context.Background())
	})

	code, out := f.callAs(admin.ID, h.MatchTransaction, http.MethodPost,
		"/api/v1/admin/payments/transactions/"+tx.ID+"/match",
		`{"order_code":"`+order.Code+`"}`, gin.Param{Key: "id", Value: tx.ID})
	if code != http.StatusOK {
		t.Fatalf("match = %d %v", code, out)
	}
	if got, _ := f.s.GetPaymentOrderByCode(f.ctx, order.Code); got.Status != domain.OrderPaid {
		t.Fatalf("order = %q, want paid", got.Status)
	}
	reloaded, err := f.s.GetPaymentTransaction(f.ctx, tx.ID)
	if err != nil || reloaded.Status != domain.TxMatched || reloaded.OrderID == nil || *reloaded.OrderID != order.ID {
		t.Fatalf("transaction = %+v, %v", reloaded, err)
	}
	if reloaded.MatchedBy == nil || *reloaded.MatchedBy != admin.ID {
		t.Fatalf("matched_by = %v, want %s", reloaded.MatchedBy, admin.ID)
	}
	if n, _ := f.s.DB.NewSelect().Table("licenses").Where("id = ?", order.ID).Count(f.ctx); n != 1 {
		t.Fatalf("licences = %d, want 1", n)
	}

	code, out = f.call(h.MatchTransaction, http.MethodPost, "/api/v1/admin/payments/transactions/"+tx.ID+"/match",
		`{"order_code":"`+order.Code+`"}`, nil, gin.Param{Key: "id", Value: tx.ID})
	if code != http.StatusConflict || errCode(out) != "PAYMENT_TRANSACTION_NOT_UNMATCHED" {
		t.Fatalf("match twice = %d %v, want 409", code, out)
	}
}

func TestAdminMatchTransactionRefusals(t *testing.T) {
	f := newAPIFixture(t)
	h := adminPayments(f)
	order := f.adminOrder(t, "sepay", 8000)
	other := f.adminOrder(t, "payos", 8000)

	// Amount mismatch: the order stays pending and the transfer stays in
	// the queue.
	wrongAmount := f.unmatchedTransfer(t, "sepay", 7999, "match-amount-"+uuid.NewString())
	code, out := f.call(h.MatchTransaction, http.MethodPost, "/api/v1/admin/payments/transactions/"+wrongAmount.ID+"/match",
		`{"order_code":"`+order.Code+`"}`, nil, gin.Param{Key: "id", Value: wrongAmount.ID})
	if code != http.StatusUnprocessableEntity || errCode(out) != "PAYMENT_TRANSACTION_MISMATCH" {
		t.Fatalf("amount mismatch = %d %v, want 422 PAYMENT_TRANSACTION_MISMATCH", code, out)
	}
	if got, _ := f.s.GetPaymentOrderByCode(f.ctx, order.Code); got.Status != domain.OrderPending {
		t.Fatalf("order = %q, want pending", got.Status)
	}

	// Provider mismatch.
	wrongRail := f.unmatchedTransfer(t, "payos", 8000, "match-rail-"+uuid.NewString())
	code, out = f.call(h.MatchTransaction, http.MethodPost, "/api/v1/admin/payments/transactions/"+wrongRail.ID+"/match",
		`{"order_code":"`+order.Code+`"}`, nil, gin.Param{Key: "id", Value: wrongRail.ID})
	if code != http.StatusConflict || errCode(out) != "PAYMENT_TRANSACTION_PROVIDER_MISMATCH" {
		t.Fatalf("provider mismatch = %d %v, want 409", code, out)
	}

	// Unknown order code.
	code, out = f.call(h.MatchTransaction, http.MethodPost, "/api/v1/admin/payments/transactions/"+wrongAmount.ID+"/match",
		`{"order_code":"KGZZZZZZ"}`, nil, gin.Param{Key: "id", Value: wrongAmount.ID})
	if code != http.StatusNotFound || errCode(out) != "PAYMENT_ORDER_NOT_FOUND" {
		t.Fatalf("unknown order = %d %v, want 404", code, out)
	}

	// Missing code and unknown transaction.
	code, out = f.call(h.MatchTransaction, http.MethodPost, "/api/v1/admin/payments/transactions/"+wrongAmount.ID+"/match",
		`{}`, nil, gin.Param{Key: "id", Value: wrongAmount.ID})
	if code != http.StatusBadRequest {
		t.Fatalf("missing code = %d %v, want 400", code, out)
	}
	code, out = f.call(h.MatchTransaction, http.MethodPost, "/api/v1/admin/payments/transactions/nope/match",
		`{"order_code":"`+other.Code+`"}`, nil, gin.Param{Key: "id", Value: "nope"})
	if code != http.StatusNotFound || errCode(out) != "PAYMENT_TRANSACTION_NOT_FOUND" {
		t.Fatalf("unknown transaction = %d %v, want 404", code, out)
	}
}

// ─── health, provider test and confirm-webhook ───

func TestAdminHealthListsProviders(t *testing.T) {
	f := newAPIFixture(t)
	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x44}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.cleanupProviderConfigSlots(t,
		providerConfigSlot{Provider: "sepay", ProductID: f.prodA.ID, Mode: "test"},
		providerConfigSlot{Provider: "payos", ProductID: f.prodA.ID, Mode: "test"},
	)
	svc := providercfg.NewService(f.s, codec, "test")
	for _, name := range []string{"sepay", "payos"} {
		var cfg, creds any = providercfg.SepayConfig{BankName: "MB Bank", BankCode: "MB", AccountNo: "0978482700", AccountName: "NGUYEN VAN A"},
			providercfg.SepayCredentials{APIToken: "tok"}
		if name == "payos" {
			cfg = providercfg.PayOSConfig{ClientID: "client"}
			creds = providercfg.PayOSCredentials{APIKey: "key", ChecksumKey: "sum"}
		}
		if _, err := svc.Upsert(f.ctx, providercfg.UpsertInput{
			Provider: name, ProductID: f.prodA.ID, Mode: "test", Enabled: true,
			Config: cfg, Credentials: creds, WebhookSecret: "whsec-" + name,
		}); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
	}
	order := f.adminOrder(t, "sepay", 900)
	f.unmatchedTransfer(t, "sepay", 900, "health-"+uuid.NewString())
	if _, err := f.s.RecordPaymentEvent(f.ctx, nil, store.PaymentEventInput{
		Provider: "sepay", EventID: "health-" + uuid.NewString(), OrderID: order.ID,
		Kind: "paid", Raw: []byte(`{}`), OccurredAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	h := adminPayments(f)
	h.Config = svc

	var out struct {
		Providers []map[string]any `json:"providers"`
	}
	if err := f.decodeData(t, h.Health, &out); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	got := map[string]map[string]any{}
	for _, p := range out.Providers {
		got[p["provider"].(string)] = p
	}
	sepay, ok := got["sepay"]
	if !ok {
		t.Fatalf("sepay missing from health: %v", out.Providers)
	}
	if sepay["enabled"] != true {
		t.Fatalf("sepay enabled = %v", sepay["enabled"])
	}
	if sepay["pending_orders"].(float64) < 1 {
		t.Fatalf("sepay pending = %v", sepay["pending_orders"])
	}
	if sepay["unmatched_open"].(float64) < 1 {
		t.Fatalf("sepay unmatched = %v", sepay["unmatched_open"])
	}
	if sepay["last_event_at"] == nil {
		t.Fatalf("sepay last_event_at = nil: %v", sepay)
	}
	if payos, ok := got["payos"]; !ok || payos["enabled"] != true {
		t.Fatalf("payos health = %v", got["payos"])
	}
}

func TestAdminProviderTestConnection(t *testing.T) {
	f := newAPIFixture(t)
	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x55}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.cleanupProviderConfigSlots(t, providerConfigSlot{Provider: "sepay", Mode: "test"})
	svc := providercfg.NewService(f.s, codec, "test")
	if _, err := svc.Upsert(f.ctx, providercfg.UpsertInput{
		Provider: "sepay", Mode: "test", Enabled: true,
		Config:        providercfg.SepayConfig{BankName: "MB Bank", BankCode: "MB", AccountNo: "0978482700", AccountName: "NGUYEN VAN A"},
		Credentials:   providercfg.SepayCredentials{APIToken: "tok"},
		WebhookSecret: "whsec",
	}); err != nil {
		t.Fatal(err)
	}
	h := adminPayments(f)
	h.Config = svc
	adapter := fake.New("sepay")
	h.Providers = func(context.Context, string, string) (provider.Provider, *providercfg.ResolvedConfig, error) {
		return adapter, &providercfg.ResolvedConfig{Provider: "sepay", Mode: "test"}, nil
	}

	var result struct {
		Provider string `json:"provider"`
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
	}
	if err := f.decodeData(t, h.TestProvider, &result, gin.Param{Key: "id", Value: "sepay"}); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.OK || result.Provider != "sepay" {
		t.Fatalf("result = %+v", result)
	}

	adapter.TestErr = errors.New("401 invalid credentials")
	result = struct {
		Provider string `json:"provider"`
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
	}{}
	if err := f.decodeData(t, h.TestProvider, &result, gin.Param{Key: "id", Value: "sepay"}); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.OK || !strings.Contains(result.Error, "401") {
		t.Fatalf("failing result = %+v", result)
	}

	code, out := f.call(h.TestProvider, http.MethodPost, "/api/v1/admin/payments/providers/unknown/test",
		"", nil, gin.Param{Key: "id", Value: "unknown"})
	if code != http.StatusBadRequest || errCode(out) != "PAYMENT_PROVIDER_UNKNOWN" {
		t.Fatalf("unknown provider = %d %v, want 400", code, out)
	}
	code, out = f.call(h.TestProvider, http.MethodPost, "/api/v1/admin/payments/providers/payos/test",
		"", nil, gin.Param{Key: "id", Value: "payos"})
	if code != http.StatusNotFound || errCode(out) != "PAYMENT_PROVIDER_NOT_FOUND" {
		t.Fatalf("unconfigured payos = %d %v, want 404", code, out)
	}
}

func TestAdminConfirmPayOSWebhook(t *testing.T) {
	f := newAPIFixture(t)
	h := adminPayments(f)

	code, out := f.call(h.ConfirmPayOSWebhook, http.MethodPost, "/api/v1/admin/payments/providers/payos/confirm-webhook", "", nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("no confirmer = %d %v, want 503", code, out)
	}

	var called string
	h.PayOSConfirmer = func(_ context.Context, url string) error {
		called = url
		return nil
	}
	code, out = f.call(h.ConfirmPayOSWebhook, http.MethodPost, "/api/v1/admin/payments/providers/payos/confirm-webhook", "", nil)
	if code != http.StatusBadRequest || errCode(out) != "PAYMENT_WEBHOOK_URL_UNSET" {
		t.Fatalf("no public url = %d %v, want 400 PAYMENT_WEBHOOK_URL_UNSET", code, out)
	}

	if err := f.s.SetSetting(f.ctx, "payment_webhook_base_url", "https://shop.example.test/"); err != nil {
		t.Fatalf("store webhook base url: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.s.DB.NewRaw("DELETE FROM settings WHERE key = ?", "payment_webhook_base_url").Exec(context.Background())
	})
	code, out = f.call(h.ConfirmPayOSWebhook, http.MethodPost, "/api/v1/admin/payments/providers/payos/confirm-webhook", "", nil)
	if code != http.StatusOK {
		t.Fatalf("confirm = %d %v", code, out)
	}
	if called != "https://shop.example.test/api/v1/webhook/payos" {
		t.Fatalf("confirmed %q", called)
	}

	h.PayOSConfirmer = func(context.Context, string) error { return errors.New("payos rejected it") }
	code, out = f.call(h.ConfirmPayOSWebhook, http.MethodPost, "/api/v1/admin/payments/providers/payos/confirm-webhook", "", nil)
	if code != http.StatusBadGateway || errCode(out) != "PAYMENT_PROVIDER_ERROR" {
		t.Fatalf("failed confirm = %d %v, want 502", code, out)
	}
}

// ─── auth and scope ───

// The routes are only as safe as the middleware they are registered
// with: an admin payment operation must not be reachable by a
// payments:read key, and no key at all is a 401.
func TestAdminPaymentsScopeEnforcement(t *testing.T) {
	f := newAPIFixture(t)
	h := adminPayments(f)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	admin := r.Group("/api/v1/admin",
		middleware.APIKeyAuth(f.s), middleware.RequireScope(model.ScopeAdmin))
	admin.GET("/payments/orders", h.ListOrders)
	admin.POST("/payments/orders/:code/cancel", h.CancelOrder)
	admin.GET("/payments/health", h.Health)

	send := func(rawKey, method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil)
		if rawKey != "" {
			req.Header.Set("Authorization", "Bearer "+rawKey)
		}
		r.ServeHTTP(w, req)
		return w
	}
	if w := send("", http.MethodGet, "/api/v1/admin/payments/orders"); w.Code != http.StatusUnauthorized {
		t.Fatalf("no key = %d %s, want 401", w.Code, w.Body.String())
	}
	readRaw := f.scopedAPIKey(t, model.ScopePaymentsRead)
	if w := send(readRaw, http.MethodGet, "/api/v1/admin/payments/orders"); w.Code != http.StatusForbidden {
		t.Fatalf("payments:read key = %d %s, want 403", w.Code, w.Body.String())
	}
	order := f.adminOrder(t, "sepay", 10)
	if w := send(readRaw, http.MethodPost, "/api/v1/admin/payments/orders/"+order.Code+"/cancel"); w.Code != http.StatusForbidden {
		t.Fatalf("payments:read cancel = %d %s, want 403", w.Code, w.Body.String())
	}
	adminRaw := f.scopedAPIKey(t, model.ScopeAdmin)
	if w := send(adminRaw, http.MethodGet, "/api/v1/admin/payments/health"); w.Code != http.StatusOK {
		t.Fatalf("admin health = %d %s, want 200", w.Code, w.Body.String())
	}
}

// callAs is call with a signed-in admin in the context, which is what
// the middleware sets in production and what the audit columns read.
func (f *apiFixture) callAs(userID string, fn gin.HandlerFunc, method, target, body string, params ...gin.Param) (int, map[string]any) {
	f.t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = params
	c.Set("user_id", userID)
	fn(c)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return c.Writer.Status(), out
}

// scopedAPIKey writes an API key with exactly the given scopes and
// returns its raw value.
func (f *apiFixture) scopedAPIKey(t *testing.T, scopes ...string) string {
	t.Helper()
	raw := "kg_live_admin_" + uuid.NewString()
	if err := f.s.CreateAPIKey(f.ctx, &model.APIKey{
		Name: "admin-scope-" + uuid.NewString()[:8], Prefix: "kg_live_test", Scopes: scopes,
	}, raw); err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return raw
}

// callList runs a list handler directly and returns its data object.
func (f *apiFixture) callList(fn gin.HandlerFunc, target string) map[string]any {
	f.t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, target, nil)
	fn(c)
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		f.t.Fatalf("decode %s: %v (%s)", target, err, w.Body.String())
	}
	return env.Data
}

// ─── fix round 1: fail-closed mark-paid ───

// sepayTestConfig writes one provider row for the order's rail and
// returns the resolver wired to the server's default mode. The caller
// names the row's own mode: those two deliberately differ in the
// live-row-under-a-test-server cases below.
func sepayTestConfig(t *testing.T, f *apiFixture, mode string, enabled bool, productID string) *providercfg.Service {
	t.Helper()
	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x66}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.cleanupProviderConfigSlots(t, providerConfigSlot{Provider: "sepay", ProductID: productID, Mode: mode})
	svc := providercfg.NewService(f.s, codec, "test")
	if _, err := svc.Upsert(f.ctx, providercfg.UpsertInput{
		Provider: "sepay", ProductID: productID, Mode: mode, Enabled: enabled,
		Config:        providercfg.SepayConfig{BankName: "MB Bank", BankCode: "MB", AccountNo: "0978482700", AccountName: "NGUYEN VAN A"},
		Credentials:   providercfg.SepayCredentials{APIToken: "tok-" + uuid.NewString()[:8]},
		WebhookSecret: "whsec-" + uuid.NewString()[:8],
	}); err != nil {
		t.Fatalf("upsert sepay config: %v", err)
	}
	return svc
}

// A disabled provider row must refuse manual settlement rather than
// skip the guard: an unresolvable config is exactly when a licence must
// not be granted by hand.
func TestAdminMarkPaidRefusesDisabledProviderConfig(t *testing.T) {
	f := newAPIFixture(t)
	svc := sepayTestConfig(t, f, "test", false, "")
	order := f.adminOrder(t, "sepay", 1234)
	h := adminPayments(f)
	h.Config = svc

	code, out := f.call(h.MarkPaid, http.MethodPost, "/api/v1/admin/payments/orders/"+order.Code+"/mark-paid",
		`{"reason":"operator says so"}`, nil, gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusUnprocessableEntity || errCode(out) != "PAYMENT_MANUAL_SETTLEMENT_UNVERIFIABLE" {
		t.Fatalf("disabled config mark paid = %d %v, want 422 PAYMENT_MANUAL_SETTLEMENT_UNVERIFIABLE", code, out)
	}
	if got, _ := f.s.GetPaymentOrderByCode(f.ctx, order.Code); got.Status != domain.OrderPending {
		t.Fatalf("order = %q, want pending", got.Status)
	}
	if n, _ := f.s.DB.NewSelect().Table("licenses").Where("id = ?", order.ID).Count(f.ctx); n != 0 {
		t.Fatalf("licences = %d, want 0", n)
	}
}

// The server may be running with PAYMENT_MODE=test while a live row
// exists for the order's product. The order does not record the mode it
// was created in, so a live row anywhere on that rail is proof enough
// that a manual settlement is not provably a rehearsal: refuse it.
func TestAdminMarkPaidRefusesLiveRowOnTestServer(t *testing.T) {
	f := newAPIFixture(t)
	svc := sepayTestConfig(t, f, "live", true, f.prodA.ID)
	order := f.adminOrder(t, "sepay", 4321)
	h := adminPayments(f)
	h.Config = svc

	code, out := f.call(h.MarkPaid, http.MethodPost, "/api/v1/admin/payments/orders/"+order.Code+"/mark-paid",
		`{"reason":"operator says so"}`, nil, gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusConflict || errCode(out) != "PAYMENT_MANUAL_SETTLEMENT_FORBIDDEN" {
		t.Fatalf("live row on test server = %d %v, want 409 PAYMENT_MANUAL_SETTLEMENT_FORBIDDEN", code, out)
	}
	if got, _ := f.s.GetPaymentOrderByCode(f.ctx, order.Code); got.Status != domain.OrderPending {
		t.Fatalf("order = %q, want pending", got.Status)
	}
	if n, _ := f.s.DB.NewSelect().Table("licenses").Where("id = ?", order.ID).Count(f.ctx); n != 0 {
		t.Fatalf("licences = %d, want 0", n)
	}
}

// A resolvable test row is the rehearsal case the operation exists for.
func TestAdminMarkPaidAllowsTestProviderConfig(t *testing.T) {
	f := newAPIFixture(t)
	svc := sepayTestConfig(t, f, "test", true, "")
	order := f.adminOrder(t, "sepay", 5678)
	h := adminPayments(f)
	h.Config = svc

	code, out := f.call(h.MarkPaid, http.MethodPost, "/api/v1/admin/payments/orders/"+order.Code+"/mark-paid",
		`{"reason":"rehearsal"}`, nil, gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusOK {
		t.Fatalf("test config mark paid = %d %v, want 200", code, out)
	}
	if got, _ := f.s.GetPaymentOrderByCode(f.ctx, order.Code); got.Status != domain.OrderPaid {
		t.Fatalf("order = %q, want paid", got.Status)
	}
}

// ─── fix round 1: match direction and API-key actors ───

// A payout recorded with direction=out is never money for an order; the
// manual match must refuse it rather than settling on the amount.
func TestAdminMatchTransactionRefusesOutgoingTransfer(t *testing.T) {
	f := newAPIFixture(t)
	h := adminPayments(f)
	order := f.adminOrder(t, "sepay", 8500)
	tx, _, err := f.s.InsertPaymentTransaction(f.ctx, nil, store.PaymentTransactionInput{
		Provider: "sepay", ProviderRef: "out-" + uuid.NewString(), AmountMinor: 8500,
		Currency: "VND", Direction: "out", Status: domain.TxUnmatched, OccurredAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.s.DB.NewRaw("DELETE FROM payment_transactions WHERE id = ?", tx.ID).Exec(context.Background())
	})

	code, out := f.call(h.MatchTransaction, http.MethodPost, "/api/v1/admin/payments/transactions/"+tx.ID+"/match",
		`{"order_code":"`+order.Code+`"}`, nil, gin.Param{Key: "id", Value: tx.ID})
	if code != http.StatusUnprocessableEntity || errCode(out) != "PAYMENT_TRANSACTION_NOT_INCOMING" {
		t.Fatalf("outgoing match = %d %v, want 422 PAYMENT_TRANSACTION_NOT_INCOMING", code, out)
	}
	if got, _ := f.s.GetPaymentOrderByCode(f.ctx, order.Code); got.Status != domain.OrderPending {
		t.Fatalf("order = %q, want pending", got.Status)
	}
}

// waitAudit reads the audit line an async writer eventually stores.
func waitAudit(t *testing.T, f *apiFixture, entityID, action string) *model.AuditLog {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		row := new(model.AuditLog)
		err := f.s.DB.NewSelect().Model(row).
			Where("entity = ?", "payment_order").Where("entity_id = ?", entityID).
			Where("action = ?", action).Order("created_at DESC").Limit(1).Scan(f.ctx)
		if err == nil {
			return row
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("audit line %s/%s was never written", entityID, action)
	return nil
}

// An admin API key is a string principal, not a users(id) value. The
// refund must still be recorded; the FK-bearing actor column stays NULL
// and the key id moves into the audit payload.
func TestAdminRefundWithAPIKeyPrincipal(t *testing.T) {
	f := newAPIFixture(t)
	h := adminPayments(f)
	order := f.adminOrder(t, "sepay", 10_000)
	if code, out := f.call(h.MarkPaid, http.MethodPost, "/api/v1/admin/payments/orders/"+order.Code+"/mark-paid",
		`{"reason":"bank transfer"}`, nil, gin.Param{Key: "code", Value: order.Code}); code != http.StatusOK {
		t.Fatalf("mark paid: %d %v", code, out)
	}
	principal := "apikey:" + uuid.NewString()
	code, out := f.callAs(principal, h.RefundOrder, http.MethodPost,
		"/api/v1/admin/payments/orders/"+order.Code+"/refund",
		`{"amount_minor":2500,"reason":"customer asked"}`, gin.Param{Key: "code", Value: order.Code})
	if code != http.StatusOK {
		t.Fatalf("api-key refund = %d %v, want 200", code, out)
	}
	row := refundRow(t, f, order.ID)
	if row.CreatedBy != "" {
		t.Fatalf("created_by = %q, want NULL for a non-user principal", row.CreatedBy)
	}
	audit := waitAudit(t, f, order.ID, "refunded")
	if audit.Changes["actor"] != principal {
		t.Fatalf("audit changes = %v, want actor %q", audit.Changes, principal)
	}
}

// The same rule applies to matched_by: an API-key match settles the
// order and stamps no user id, while the audit line keeps the key id.
func TestAdminMatchWithAPIKeyPrincipal(t *testing.T) {
	f := newAPIFixture(t)
	h := adminPayments(f)
	order := f.adminOrder(t, "sepay", 12_000)
	tx := f.unmatchedTransfer(t, "sepay", 12_000, "match-apikey-"+uuid.NewString())
	principal := "apikey:" + uuid.NewString()

	code, out := f.callAs(principal, h.MatchTransaction, http.MethodPost,
		"/api/v1/admin/payments/transactions/"+tx.ID+"/match",
		`{"order_code":"`+order.Code+`"}`, gin.Param{Key: "id", Value: tx.ID})
	if code != http.StatusOK {
		t.Fatalf("api-key match = %d %v, want 200", code, out)
	}
	if got, _ := f.s.GetPaymentOrderByCode(f.ctx, order.Code); got.Status != domain.OrderPaid {
		t.Fatalf("order = %q, want paid", got.Status)
	}
	reloaded, err := f.s.GetPaymentTransaction(f.ctx, tx.ID)
	if err != nil || reloaded.Status != domain.TxMatched {
		t.Fatalf("transaction = %+v, %v", reloaded, err)
	}
	if reloaded.MatchedBy != nil {
		t.Fatalf("matched_by = %v, want NULL for a non-user principal", *reloaded.MatchedBy)
	}
	audit := waitAudit(t, f, order.ID, "transaction_matched")
	if audit.Changes["actor"] != principal {
		t.Fatalf("audit changes = %v, want actor %q", audit.Changes, principal)
	}
}
