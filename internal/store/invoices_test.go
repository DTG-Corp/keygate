package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

// The invoice tests mint numbers in years no production row can use, so
// a parallel package sharing this database cannot advance the counter a
// test asserts on, and each test starts from a known empty year.
const (
	numberingYear    = 2199
	concurrentYear   = 2198
	transitionsYear  = 2197
	idempotencyYear  = 2196
	invoiceTestBuyer = `{"name":"Nguyễn Văn Ánh","company_name":"ACME JSC","tax_code":"0101234567",` +
		`"address":"1 Lê Lợi, Quận 1, TP. Hồ Chí Minh","email":"buyer@example.com"}`
)

// resetInvoiceCounter empties one year's numbering row, before the test
// and again after it, so the assertions do not depend on run order.
func resetInvoiceCounter(t *testing.T, s *store.Store, year int) {
	t.Helper()
	ctx := context.Background()
	del := func() {
		if _, err := s.DB.NewRaw("DELETE FROM invoice_counters WHERE year = ?", year).Exec(ctx); err != nil {
			t.Errorf("reset invoice counter %d: %v", year, err)
		}
	}
	del()
	t.Cleanup(del)
}

// seedInvoiceOrder creates a product, plan and pending order for an
// invoice to reference, and removes all four when the test ends. The
// invoice goes first: invoices.order_id is ON DELETE RESTRICT.
func seedInvoiceOrder(t *testing.T, s *store.Store, amount int64) *model.PaymentOrder {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:13]
	prod := &model.Product{Name: "Invoice Co", Slug: "invoice-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Pro", Slug: "invoice-pro-" + suffix,
		LicenseType: "subscription", LicenseModel: "standard", MaxActivations: 3, Active: true,
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: prod.ID, PlanID: plan.ID, Email: "buyer@example.com", Kind: "new", Quantity: 2,
		SubtotalMinor: amount, DiscountMinor: 0, TaxMinor: 0, TotalMinor: amount,
		Currency: "VND", CurrencyExponent: 0, Provider: "sepay",
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(invoiceTestBuyer), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.DB.NewRaw("DELETE FROM invoices WHERE order_id = ?", order.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM payment_orders WHERE id = ?", order.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM plans WHERE id = ?", plan.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", prod.ID).Exec(ctx)
	})
	return order
}

func invoiceInput(order *model.PaymentOrder, year int) store.CreateInvoiceInput {
	return store.CreateInvoiceInput{
		OrderID: order.ID, Buyer: order.Buyer,
		Items: []model.InvoiceItem{{
			Description: "Pro", PlanID: order.PlanID, Quantity: order.Quantity,
			UnitAmountMinor: order.SubtotalMinor / int64(order.Quantity),
			AmountMinor:     order.SubtotalMinor, TaxRateBP: 800,
		}},
		SubtotalMinor: order.SubtotalMinor, DiscountMinor: order.DiscountMinor,
		TaxMinor: order.TaxMinor, TotalMinor: order.TotalMinor,
		Currency: order.Currency, CurrencyExponent: order.CurrencyExp,
		Status: model.InvoiceStatusIssued, NumberYear: year,
	}
}

func TestCreateInvoiceNumbersPerYear(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	resetInvoiceCounter(t, s, numberingYear)
	orderA := seedInvoiceOrder(t, s, 1_190_000)
	orderB := seedInvoiceOrder(t, s, 500_000)

	issuedAt := time.Now().UTC().Truncate(time.Second)
	in := invoiceInput(orderA, numberingYear)
	in.IssuedAt = &issuedAt
	first, created, err := s.CreateInvoice(ctx, in)
	if err != nil {
		t.Fatalf("first invoice: %v", err)
	}
	if !created {
		t.Fatal("first invoice reported created=false")
	}
	if want := fmt.Sprintf("KG-%d-000001", numberingYear); first.Number != want {
		t.Errorf("first number = %q, want %q", first.Number, want)
	}
	// The invoice is a snapshot: every money field and the buyer blob
	// must survive the round trip unchanged.
	if first.SubtotalMinor != orderA.SubtotalMinor || first.TotalMinor != orderA.TotalMinor ||
		first.Currency != orderA.Currency || first.CurrencyExp != orderA.CurrencyExp {
		t.Errorf("invoice money = %d/%d %s/%d, want %d/%d %s/%d",
			first.SubtotalMinor, first.TotalMinor, first.Currency, first.CurrencyExp,
			orderA.SubtotalMinor, orderA.TotalMinor, orderA.Currency, orderA.CurrencyExp)
	}
	if string(first.Buyer) == "" || !strings.Contains(string(first.Buyer), "0101234567") {
		t.Errorf("buyer snapshot = %s, want the order's buyer JSON", first.Buyer)
	}
	if len(first.Items) != 1 || first.Items[0].AmountMinor != orderA.SubtotalMinor {
		t.Errorf("items = %+v, want the one line built from the order", first.Items)
	}
	if first.Status != model.InvoiceStatusIssued || first.IssuedAt == nil {
		t.Errorf("status/issued_at = %q/%v, want issued and set", first.Status, first.IssuedAt)
	}

	second, created, err := s.CreateInvoice(ctx, invoiceInput(orderB, numberingYear))
	if err != nil || !created {
		t.Fatalf("second invoice: created=%v err=%v", created, err)
	}
	if want := fmt.Sprintf("KG-%d-000002", numberingYear); second.Number != want {
		t.Errorf("second number = %q, want %q", second.Number, want)
	}

	// A new year opens its own counter.
	if _, err := s.DB.NewRaw("DELETE FROM invoice_counters WHERE year = ?", numberingYear+1).Exec(ctx); err != nil {
		t.Fatalf("reset next year: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DB.NewRaw("DELETE FROM invoice_counters WHERE year = ?", numberingYear+1).Exec(context.Background())
	})
	orderC := seedInvoiceOrder(t, s, 100_000)
	third, _, err := s.CreateInvoice(ctx, invoiceInput(orderC, numberingYear+1))
	if err != nil {
		t.Fatalf("next-year invoice: %v", err)
	}
	if want := fmt.Sprintf("KG-%d-000001", numberingYear+1); third.Number != want {
		t.Errorf("next-year number = %q, want %q", third.Number, want)
	}
}

func TestCreateInvoiceIsIdempotentByOrder(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	resetInvoiceCounter(t, s, idempotencyYear)
	order := seedInvoiceOrder(t, s, 300_000)

	first, created, err := s.CreateInvoice(ctx, invoiceInput(order, idempotencyYear))
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}
	second, created, err := s.CreateInvoice(ctx, invoiceInput(order, idempotencyYear))
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created {
		t.Fatal("second create reported a new row for the same order")
	}
	if second.ID != first.ID || second.Number != first.Number {
		t.Errorf("second = %s/%s, want the first %s/%s", second.ID, second.Number, first.ID, first.Number)
	}
	var rows int
	if err := s.DB.NewRaw("SELECT count(*) FROM invoices WHERE order_id = ?", order.ID).Scan(ctx, &rows); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	if rows != 1 {
		t.Fatalf("invoices for the order = %d, want 1", rows)
	}
	// The duplicate path rolled back its number allocation: the next
	// invoice is the immediate successor, with no gap.
	orderB := seedInvoiceOrder(t, s, 300_000)
	next, _, err := s.CreateInvoice(ctx, invoiceInput(orderB, idempotencyYear))
	if err != nil {
		t.Fatalf("next create: %v", err)
	}
	if want := fmt.Sprintf("KG-%d-000002", idempotencyYear); next.Number != want {
		t.Errorf("next number = %q, want %q (duplicate burned a number)", next.Number, want)
	}
}

func TestInvoiceNumberingConcurrent(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	resetInvoiceCounter(t, s, concurrentYear)
	orderA := seedInvoiceOrder(t, s, 100_000)
	orderB := seedInvoiceOrder(t, s, 200_000)

	start := make(chan struct{})
	var wg sync.WaitGroup
	numbers := make([]string, 2)
	errs := make([]error, 2)
	for i, order := range []*model.PaymentOrder{orderA, orderB} {
		wg.Add(1)
		go func(i int, order *model.PaymentOrder) {
			defer wg.Done()
			<-start
			inv, _, err := s.CreateInvoice(ctx, invoiceInput(order, concurrentYear))
			if err != nil {
				errs[i] = err
				return
			}
			numbers[i] = inv.Number
		}(i, order)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent create %d: %v", i, err)
		}
	}
	want := map[string]bool{
		fmt.Sprintf("KG-%d-000001", concurrentYear): true,
		fmt.Sprintf("KG-%d-000002", concurrentYear): true,
	}
	if numbers[0] == numbers[1] {
		t.Fatalf("two invoices share number %q", numbers[0])
	}
	for _, n := range numbers {
		if !want[n] {
			t.Errorf("number %q is not the expected sequential pair %v", n, want)
		}
	}
}

func TestInvoiceTransitionsAndFilters(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	resetInvoiceCounter(t, s, transitionsYear)
	order := seedInvoiceOrder(t, s, 800_000)

	in := invoiceInput(order, transitionsYear)
	in.Status = model.InvoiceStatusPendingIssue
	pending, _, err := s.CreateInvoice(ctx, in)
	if err != nil {
		t.Fatalf("create pending invoice: %v", err)
	}
	if pending.Status != model.InvoiceStatusPendingIssue || pending.IssuedAt != nil {
		t.Fatalf("pending invoice = %q issued_at=%v", pending.Status, pending.IssuedAt)
	}

	issuedAt := time.Now().UTC().Truncate(time.Second)
	issued, err := s.MarkInvoiceIssued(ctx, pending.ID, issuedAt, store.EInvoiceRef{})
	if err != nil {
		t.Fatalf("mark issued: %v", err)
	}
	if issued.Status != model.InvoiceStatusIssued || issued.IssuedAt == nil {
		t.Fatalf("issued = %q issued_at=%v", issued.Status, issued.IssuedAt)
	}
	if _, err := s.MarkInvoiceIssued(ctx, pending.ID, issuedAt, store.EInvoiceRef{}); !errors.Is(err, store.ErrInvoiceAlreadyIssued) {
		t.Fatalf("second issue = %v, want ErrInvoiceAlreadyIssued", err)
	}
	if _, err := s.MarkInvoiceIssued(ctx, "missing", issuedAt, store.EInvoiceRef{}); !errors.Is(err, store.ErrInvoiceNotFound) {
		t.Fatalf("issue missing = %v, want ErrInvoiceNotFound", err)
	}

	// An e-invoice reference is persisted with the transition.
	orderB := seedInvoiceOrder(t, s, 100_000)
	inB := invoiceInput(orderB, transitionsYear)
	inB.Status = model.InvoiceStatusPendingIssue
	pendB, _, err := s.CreateInvoice(ctx, inB)
	if err != nil {
		t.Fatalf("create second pending invoice: %v", err)
	}
	ref := store.EInvoiceRef{Provider: "sepay-einvoice", Ref: "HD-1", Status: "issued"}
	withRef, err := s.MarkInvoiceIssued(ctx, pendB.ID, issuedAt, ref)
	if err != nil {
		t.Fatalf("mark issued with ref: %v", err)
	}
	if withRef.EInvoiceProvider != ref.Provider || withRef.EInvoiceRef != ref.Ref || withRef.EInvoiceStatus != ref.Status {
		t.Errorf("e-invoice fields = %q/%q/%q, want %+v",
			withRef.EInvoiceProvider, withRef.EInvoiceRef, withRef.EInvoiceStatus, ref)
	}

	voided, err := s.MarkInvoiceVoid(ctx, issued.ID)
	if err != nil {
		t.Fatalf("void: %v", err)
	}
	if voided.Status != model.InvoiceStatusVoid {
		t.Errorf("voided status = %q", voided.Status)
	}
	if voided.Number != issued.Number || voided.TotalMinor != issued.TotalMinor {
		t.Errorf("void lost the document: %q/%d", voided.Number, voided.TotalMinor)
	}
	if _, err := s.MarkInvoiceVoid(ctx, issued.ID); !errors.Is(err, store.ErrInvoiceNotVoidable) {
		t.Fatalf("second void = %v, want ErrInvoiceNotVoidable", err)
	}
	if _, err := s.MarkInvoiceVoid(ctx, "missing"); !errors.Is(err, store.ErrInvoiceNotFound) {
		t.Fatalf("void missing = %v, want ErrInvoiceNotFound", err)
	}

	if err := s.SetInvoicePDFPath(ctx, issued.ID, "invoices/2197/KG-2197-000001.pdf"); err != nil {
		t.Fatalf("set pdf path: %v", err)
	}
	got, err := s.GetInvoice(ctx, issued.ID)
	if err != nil {
		t.Fatalf("get invoice: %v", err)
	}
	if got.PDFPath != "invoices/2197/KG-2197-000001.pdf" {
		t.Errorf("pdf path = %q", got.PDFPath)
	}
	inv, order2, err := s.GetInvoiceWithOrder(ctx, issued.ID)
	if err != nil {
		t.Fatalf("get invoice with order: %v", err)
	}
	if order2.ID != order.ID || inv.ID != issued.ID {
		t.Errorf("with-order = %s/%s, want %s/%s", inv.ID, order2.ID, issued.ID, order.ID)
	}

	from := time.Now().Add(-time.Hour)
	to := time.Now().Add(time.Hour)
	rows, total, err := s.ListInvoices(ctx, store.InvoiceFilter{
		Status: model.InvoiceStatusVoid, OrderID: order.ID, From: &from, To: &to, Page: store.Page{Limit: 10},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].ID != issued.ID {
		t.Fatalf("filtered list = %d rows / total %d, want the voided invoice", len(rows), total)
	}
	if _, total, err := s.ListInvoices(ctx, store.InvoiceFilter{LicenseID: "no-such-license"}); err != nil || total != 0 {
		t.Fatalf("license filter = total %d err %v, want 0", total, err)
	}
	if _, _, err := s.ListInvoices(ctx, store.InvoiceFilter{ProductID: order.ProductID}); err != nil {
		t.Fatalf("product filter: %v", err)
	}
	if _, err := s.GetInvoice(ctx, "missing"); !errors.Is(err, store.ErrInvoiceNotFound) {
		t.Fatalf("get missing = %v, want ErrInvoiceNotFound", err)
	}
}
