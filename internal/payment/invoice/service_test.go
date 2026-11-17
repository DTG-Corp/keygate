package invoice

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/storage"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

// ─── fixture ───

// emitted is one captured webhook dispatch.
type emitted struct {
	productID, event string
	data             map[string]any
}

type captureEmitter struct{ calls []emitted }

func (c *captureEmitter) Dispatch(_ context.Context, productID, event string, data map[string]any) {
	c.calls = append(c.calls, emitted{productID: productID, event: event, data: data})
}

type invoiceFixture struct {
	t       *testing.T
	s       *store.Store
	ctx     context.Context
	prod    *model.Product
	plan    *model.Plan
	svc     *Service
	emitter *captureEmitter
}

// newInvoiceFixture creates an isolated product and plan and a service
// whose clock is pinned to 2025-12-31T17:30Z — 00:30 on 1 January 2026 in
// Ho Chi Minh City, so the number's year and the UTC date disagree and a
// wrong timezone is visible in the number.
func newInvoiceFixture(t *testing.T) *invoiceFixture {
	t.Helper()
	s := testdb.Store(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:13]
	prod := &model.Product{Name: "Invoice Co", Slug: "inv-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Pro", Slug: "inv-pro-" + suffix,
		LicenseType: "subscription", LicenseModel: "standard", MaxActivations: 3, Active: true,
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.DB.NewRaw("DELETE FROM invoices WHERE order_id IN (SELECT id FROM payment_orders WHERE product_id = ?)", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM payment_orders WHERE product_id = ?", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM plans WHERE id = ?", plan.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", prod.ID).Exec(ctx)
	})
	emitter := &captureEmitter{}
	svc := NewWithDeps(Deps{
		Store: s, Storage: storage.Disabled{}, Logger: nil, Events: emitter,
		Now: func() time.Time { return time.Date(2025, 12, 31, 17, 30, 0, 0, time.UTC) },
	})
	return &invoiceFixture{t: t, s: s, ctx: ctx, prod: prod, plan: plan, svc: svc, emitter: emitter}
}

type orderOptions struct {
	quantity  int
	subtotal  int64
	discount  int64
	tax       int64
	total     int64
	currency  string
	exponent  int16
	taxRateBP int
	buyer     string
	paid      bool
	licenseID string
}

// order creates the paid order an invoice is built from. Amounts default
// to the spec's VN example (1_190_000 VND with an 8 % VAT inside it).
func (f *invoiceFixture) order(o orderOptions) *model.PaymentOrder {
	f.t.Helper()
	if o.quantity == 0 {
		o.quantity = 1
	}
	if o.currency == "" {
		o.currency = "VND"
	}
	if o.buyer == "" {
		o.buyer = `{"name":"Nguyễn Văn Ánh","company_name":"ACME JSC","tax_code":"0101234567",` +
			`"address":"1 Lê Lợi, Quận 1, TP. Hồ Chí Minh","email":"buyer@example.com",` +
			`"phone":"+84901234567","request_invoice":true}`
	}
	if o.subtotal == 0 && o.total == 0 {
		o.subtotal, o.total, o.tax, o.taxRateBP = 1_190_000, 1_190_000, 88_148, 800
	}
	order, err := f.s.CreatePaymentOrder(f.ctx, store.CreateOrderInput{
		ProductID: f.prod.ID, PlanID: f.plan.ID, LicenseID: o.licenseID,
		Email: "buyer@example.com", Kind: "new", Quantity: o.quantity,
		SubtotalMinor: o.subtotal, DiscountMinor: o.discount, TaxMinor: o.tax, TotalMinor: o.total,
		Currency: o.currency, CurrencyExponent: o.exponent, TaxRateBP: o.taxRateBP,
		Provider: "sepay", ExpiresAt: time.Now().Add(time.Hour),
		Buyer: []byte(o.buyer), Metadata: []byte(`{}`),
	})
	if err != nil {
		f.t.Fatalf("create order: %v", err)
	}
	if o.paid {
		if _, err := f.s.DB.NewUpdate().Model((*model.PaymentOrder)(nil)).
			Set("status = 'paid'").Set("paid_at = ?", time.Now().UTC()).
			Where("id = ?", order.ID).Exec(f.ctx); err != nil {
			f.t.Fatalf("mark order paid: %v", err)
		}
	}
	return order
}

func (f *invoiceFixture) invoiceCount(orderID string) int {
	f.t.Helper()
	var n int
	if err := f.s.DB.NewRaw("SELECT count(*) FROM invoices WHERE order_id = ?", orderID).Scan(f.ctx, &n); err != nil {
		f.t.Fatalf("count invoices: %v", err)
	}
	return n
}

// ─── creation ───

func TestCreateForOrderSnapshotsOrder(t *testing.T) {
	f := newInvoiceFixture(t)
	order := f.order(orderOptions{quantity: 2, subtotal: 2_000_000, discount: 200_000,
		tax: 132_000, total: 1_800_000, taxRateBP: 800})

	inv, err := f.svc.CreateForOrder(f.ctx, order)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	if inv.Status != model.InvoiceStatusIssued {
		t.Errorf("status = %q, want issued", inv.Status)
	}
	if inv.IssuedAt == nil || !inv.IssuedAt.Equal(time.Date(2025, 12, 31, 17, 30, 0, 0, time.UTC)) {
		t.Errorf("issued_at = %v, want the service clock", inv.IssuedAt)
	}
	// The clock is 31 Dec 2025 UTC but 1 Jan 2026 in Ho Chi Minh City:
	// the number must use the VN year.
	if !strings.HasPrefix(inv.Number, "KG-2026-") {
		t.Errorf("number = %q, want the Asia/Ho_Chi_Minh year 2026", inv.Number)
	}
	if !invoiceNumberPattern.MatchString(inv.Number) {
		t.Errorf("number = %q, want KG-YYYY-NNNNNN", inv.Number)
	}
	if inv.OrderID != order.ID || inv.LicenseID != order.LicenseID {
		t.Errorf("order/license = %q/%q", inv.OrderID, inv.LicenseID)
	}
	for name, pair := range map[string][2]int64{
		"subtotal": {inv.SubtotalMinor, order.SubtotalMinor},
		"discount": {inv.DiscountMinor, order.DiscountMinor},
		"tax":      {inv.TaxMinor, order.TaxMinor},
		"total":    {inv.TotalMinor, order.TotalMinor},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %d, want the order's %d", name, pair[0], pair[1])
		}
	}
	if inv.Currency != order.Currency || inv.CurrencyExp != order.CurrencyExp {
		t.Errorf("currency = %s/%d, want %s/%d", inv.Currency, inv.CurrencyExp, order.Currency, order.CurrencyExp)
	}
	buyer := BuyerOf(inv.Buyer)
	if buyer.Name != "Nguyễn Văn Ánh" || buyer.CompanyName != "ACME JSC" ||
		buyer.TaxCode != "0101234567" || buyer.Email != "buyer@example.com" ||
		buyer.Address != "1 Lê Lợi, Quận 1, TP. Hồ Chí Minh" || !buyer.RequestInvoice {
		t.Errorf("buyer snapshot = %+v", buyer)
	}
	if len(inv.Items) != 1 {
		t.Fatalf("items = %+v, want one line", inv.Items)
	}
	item := inv.Items[0]
	if item.Description != "Pro" || item.PlanID != f.plan.ID || item.Quantity != 2 ||
		item.UnitAmountMinor != 1_000_000 || item.AmountMinor != 2_000_000 || item.TaxRateBP != 800 {
		t.Errorf("item = %+v", item)
	}
	if len(f.emitter.calls) != 1 {
		t.Fatalf("events = %+v, want one invoice.issued", f.emitter.calls)
	}
	ev := f.emitter.calls[0]
	if ev.event != model.EventInvoiceIssued || ev.productID != f.prod.ID {
		t.Errorf("event = %s on %s, want invoice.issued on the order's product", ev.event, ev.productID)
	}
	payload, _ := json.Marshal(ev.data)
	for _, want := range []string{inv.ID, inv.Number, `"total":1800000`, `"currency":"VND"`} {
		if !strings.Contains(string(payload), want) {
			t.Errorf("event payload %s does not carry %s", payload, want)
		}
	}
}

func TestCreateForOrderIsIdempotent(t *testing.T) {
	f := newInvoiceFixture(t)
	order := f.order(orderOptions{})

	first, err := f.svc.CreateForOrder(f.ctx, order)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := f.svc.CreateForOrder(f.ctx, order)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if second.ID != first.ID || second.Number != first.Number {
		t.Errorf("second = %s/%s, want %s/%s", second.ID, second.Number, first.ID, first.Number)
	}
	if n := f.invoiceCount(order.ID); n != 1 {
		t.Errorf("invoices for the order = %d, want 1", n)
	}
	if len(f.emitter.calls) != 1 {
		t.Errorf("events = %d, want exactly one for two creates", len(f.emitter.calls))
	}
}

func TestCreateForOrderPendingIssueWhenProviderConfigured(t *testing.T) {
	f := newInvoiceFixture(t)
	if err := f.s.SetSetting(f.ctx, store.SettingEInvoiceProvider, "sepay-einvoice"); err != nil {
		t.Fatalf("set provider: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.s.DB.NewRaw("DELETE FROM settings WHERE key = ?", store.SettingEInvoiceProvider).Exec(context.Background())
	})
	order := f.order(orderOptions{})

	inv, err := f.svc.CreateForOrder(f.ctx, order)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if inv.Status != model.InvoiceStatusPendingIssue || inv.IssuedAt != nil {
		t.Errorf("status/issued_at = %q/%v, want pending_issue with no issue date", inv.Status, inv.IssuedAt)
	}
	if inv.EInvoiceProvider != "sepay-einvoice" {
		t.Errorf("e_invoice_provider = %q, want the configured provider", inv.EInvoiceProvider)
	}
	if len(f.emitter.calls) != 0 {
		t.Errorf("events = %+v, want none for a pending invoice", f.emitter.calls)
	}
}

// VAT cases and a non-zero exponent: the invoice copies money, it never
// recomputes it. USD is the exponent-2 case the spec calls out.
func TestCreateForOrderCopiesMoneyVerbatim(t *testing.T) {
	f := newInvoiceFixture(t)
	// tax included in the total (VND, exponent 0)
	included := f.order(orderOptions{subtotal: 1_190_000, tax: 88_148, total: 1_190_000, taxRateBP: 800})
	// tax added on top (USD, exponent 2)
	exclusive := f.order(orderOptions{
		subtotal: 100_000, discount: 10_000, tax: 12_500, total: 112_500,
		currency: "USD", exponent: 2, taxRateBP: 1250,
	})
	// nothing to pay
	zero := f.order(orderOptions{subtotal: 0, total: 0, tax: 0, taxRateBP: 0})

	for _, tc := range []struct {
		name  string
		order *model.PaymentOrder
	}{
		{"tax-included VND", included},
		{"tax-added USD", exclusive},
		{"zero total", zero},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv, err := f.svc.CreateForOrder(f.ctx, tc.order)
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if inv.SubtotalMinor != tc.order.SubtotalMinor || inv.DiscountMinor != tc.order.DiscountMinor ||
				inv.TaxMinor != tc.order.TaxMinor || inv.TotalMinor != tc.order.TotalMinor ||
				inv.Currency != tc.order.Currency || inv.CurrencyExp != tc.order.CurrencyExp {
				t.Errorf("invoice money = %d/%d/%d/%d %s/%d, want %d/%d/%d/%d %s/%d",
					inv.SubtotalMinor, inv.DiscountMinor, inv.TaxMinor, inv.TotalMinor,
					inv.Currency, inv.CurrencyExp,
					tc.order.SubtotalMinor, tc.order.DiscountMinor, tc.order.TaxMinor, tc.order.TotalMinor,
					tc.order.Currency, tc.order.CurrencyExp)
			}
		})
	}
}

// itemsFor is the arithmetic behind the snapshot; a quantity that does
// not divide the subtotal must still produce a line whose amount equals
// the subtotal.
func TestItemsForRoundingAndFallback(t *testing.T) {
	order := &model.PaymentOrder{PlanID: "plan-1", Quantity: 3, SubtotalMinor: 100, TaxRateBP: 1000}
	items := itemsFor(order, "")
	if len(items) != 1 || items[0].UnitAmountMinor != 33 || items[0].AmountMinor != 100 {
		t.Fatalf("items = %+v, want unit 33 and amount 100", items)
	}
	if items[0].Description != "plan-1" {
		t.Errorf("description = %q, want the plan id fallback", items[0].Description)
	}
	if got := itemsFor(&model.PaymentOrder{Quantity: 0, SubtotalMinor: 10}, "Pro"); got[0].Quantity != 1 {
		t.Errorf("quantity = %d, want 1 for an order without one", got[0].Quantity)
	}
}

func TestBuyerOfToleratesLegacyFields(t *testing.T) {
	b := BuyerOf(json.RawMessage(`{"name":"A","company":"Legacy Ltd","tax_id":"123","email":"a@x.vn"}`))
	if b.CompanyName != "Legacy Ltd" || b.TaxCode != "123" {
		t.Errorf("legacy buyer = %+v, want company/tax_id folded in", b)
	}
	if got := BuyerOf(nil); got != (Buyer{}) {
		t.Errorf("nil buyer = %+v, want the zero block", got)
	}
}

// A nil order is a programming error, not a document.
func TestCreateForOrderRejectsNil(t *testing.T) {
	f := newInvoiceFixture(t)
	if _, err := f.svc.CreateForOrder(f.ctx, nil); err == nil {
		t.Fatal("nil order produced an invoice")
	}
}

// The service must still invoice when storage is unconfigured.
func TestNewWithoutStorageUsesDisabled(t *testing.T) {
	s := testdb.Store(t)
	svc := New(s, nil, nil)
	if _, ok := svc.Storage.(storage.Disabled); !ok {
		t.Fatalf("storage = %T, want storage.Disabled", svc.Storage)
	}
	if svc.Now == nil || svc.Logger == nil {
		t.Fatal("optional seams were not filled")
	}
}

// invoiceNumberPattern is the number format the spec fixes:
// KG-<YYYY>-<000001>.
var invoiceNumberPattern = regexp.MustCompile(`^KG-\d{4}-\d{6}$`)
