package invoice

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/storage"
	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/internal/testsupport/testdb"
)

// ─── e-invoice adapter double ───

type fakeEInvoice struct {
	ref      EInvoiceRef
	issueErr error
	voidErr  error
	issued   []string
	voided   []EInvoiceRef
}

func (f *fakeEInvoice) Issue(_ context.Context, inv *model.Invoice) (EInvoiceRef, error) {
	f.issued = append(f.issued, inv.ID)
	if f.issueErr != nil {
		return EInvoiceRef{}, f.issueErr
	}
	return f.ref, nil
}

func (f *fakeEInvoice) Void(_ context.Context, ref EInvoiceRef) error {
	f.voided = append(f.voided, ref)
	return f.voidErr
}

func (f *fakeEInvoice) Status(_ context.Context, _ EInvoiceRef) (string, error) {
	return f.ref.Status, nil
}

// withEInvoiceProvider puts the manual queue in force: invoices open as
// pending_issue instead of issued.
func (f *invoiceFixture) withEInvoiceProvider(name string) {
	f.t.Helper()
	if err := f.s.SetSetting(f.ctx, store.SettingEInvoiceProvider, name); err != nil {
		f.t.Fatalf("set e-invoice provider: %v", err)
	}
	f.t.Cleanup(func() {
		_, _ = f.s.DB.NewRaw("DELETE FROM settings WHERE key = ?", store.SettingEInvoiceProvider).Exec(context.Background())
	})
}

// waitForAudit polls for the background audit write (store.Audit runs it
// in a goroutine) and returns its changes map.
func (f *invoiceFixture) waitForAudit(id, action string) map[string]any {
	f.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var raw []byte
		err := f.s.DB.NewRaw(
			`SELECT changes FROM audit_logs WHERE entity = 'invoice' AND entity_id = ? AND action = ?
			 ORDER BY created_at DESC LIMIT 1`, id, action).Scan(f.ctx, &raw)
		if err == nil && len(raw) > 0 {
			out := map[string]any{}
			_ = json.Unmarshal(raw, &out)
			return out
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("audit %q for invoice %s never appeared (err=%v)", action, id, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (f *invoiceFixture) pendingInvoice() *model.Invoice {
	f.t.Helper()
	f.withEInvoiceProvider("sepay-einvoice")
	inv, err := f.svc.CreateForOrder(f.ctx, f.order(orderOptions{}))
	if err != nil {
		f.t.Fatalf("create pending invoice: %v", err)
	}
	if inv.Status != model.InvoiceStatusPendingIssue {
		f.t.Fatalf("invoice status = %q, want pending_issue", inv.Status)
	}
	return inv
}

// ─── tests ───

func TestIssueAndVoidStateMachine(t *testing.T) {
	f := newInvoiceFixture(t)
	inv := f.pendingInvoice()
	f.emitter.calls = nil // the pending creation emitted nothing; keep it that way.

	issued, err := f.svc.Issue(f.ctx, inv.ID, "admin-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if issued.Status != model.InvoiceStatusIssued || issued.IssuedAt == nil {
		t.Fatalf("issued = %q issued_at=%v", issued.Status, issued.IssuedAt)
	}
	if issued.Number != inv.Number || issued.TotalMinor != inv.TotalMinor {
		t.Errorf("issue changed the document: %s/%d", issued.Number, issued.TotalMinor)
	}
	// A manual issue keeps the configured provider visible on the row.
	if issued.EInvoiceProvider != "sepay-einvoice" {
		t.Errorf("e_invoice_provider = %q, want the configured provider", issued.EInvoiceProvider)
	}
	changes := f.waitForAudit(inv.ID, "invoice_issued")
	if changes["number"] != inv.Number {
		t.Errorf("audit changes = %v, want the number", changes)
	}
	if len(f.emitter.calls) != 1 || f.emitter.calls[0].event != model.EventInvoiceIssued {
		t.Fatalf("events = %+v, want one invoice.issued", f.emitter.calls)
	}

	if _, err := f.svc.Issue(f.ctx, inv.ID, "admin-1"); !errors.Is(err, store.ErrInvoiceAlreadyIssued) {
		t.Fatalf("second issue = %v, want ErrInvoiceAlreadyIssued", err)
	}
	if _, err := f.svc.Issue(f.ctx, "missing", "admin-1"); !errors.Is(err, store.ErrInvoiceNotFound) {
		t.Fatalf("issue missing = %v, want ErrInvoiceNotFound", err)
	}

	// A stored PDF path survives the void: a voided document is kept.
	if err := f.s.SetInvoicePDFPath(f.ctx, inv.ID, "invoices/2026/"+inv.Number+".pdf"); err != nil {
		t.Fatalf("set pdf path: %v", err)
	}
	voided, err := f.svc.VoidWithReason(f.ctx, inv.ID, "admin-2", "issued to the wrong buyer")
	if err != nil {
		t.Fatalf("void: %v", err)
	}
	if voided.Status != model.InvoiceStatusVoid {
		t.Errorf("voided status = %q", voided.Status)
	}
	if voided.Number != inv.Number || voided.TotalMinor != inv.TotalMinor ||
		voided.PDFPath != "invoices/2026/"+inv.Number+".pdf" {
		t.Errorf("void lost the document: %+v", voided)
	}
	if changes := f.waitForAudit(inv.ID, "invoice_voided"); changes["reason"] != "issued to the wrong buyer" {
		t.Errorf("void audit = %v, want the reason", changes)
	}
	if len(f.emitter.calls) != 2 || f.emitter.calls[1].event != model.EventInvoiceVoided {
		t.Fatalf("events = %+v, want invoice.issued then invoice.voided", f.emitter.calls)
	}
	payload, _ := json.Marshal(f.emitter.calls[1].data)
	if strings.Contains(string(payload), "total") {
		t.Errorf("invoice.voided payload = %s, want identity only", payload)
	}
	if !strings.Contains(string(payload), inv.Number) {
		t.Errorf("invoice.voided payload = %s, want the number", payload)
	}

	if _, err := f.svc.Void(f.ctx, inv.ID, "admin-2"); !errors.Is(err, store.ErrInvoiceNotVoidable) {
		t.Fatalf("second void = %v, want ErrInvoiceNotVoidable", err)
	}
	if _, err := f.svc.Void(f.ctx, "missing", "admin-2"); !errors.Is(err, store.ErrInvoiceNotFound) {
		t.Fatalf("void missing = %v, want ErrInvoiceNotFound", err)
	}
}

func TestIssueUsesEInvoiceProvider(t *testing.T) {
	f := newInvoiceFixture(t)
	inv := f.pendingInvoice()
	adapter := &fakeEInvoice{ref: EInvoiceRef{Provider: "sepay-einvoice", Ref: "HD-2026-1", Status: "issued"}}
	f.svc.EInvoice = adapter

	issued, err := f.svc.Issue(f.ctx, inv.ID, "admin-1")
	if err != nil {
		t.Fatalf("issue with adapter: %v", err)
	}
	if issued.EInvoiceRef != "HD-2026-1" || issued.EInvoiceStatus != "issued" || issued.Status != model.InvoiceStatusIssued {
		t.Errorf("issued = %q/%q/%q", issued.EInvoiceRef, issued.EInvoiceStatus, issued.Status)
	}
	if len(adapter.issued) != 1 || adapter.issued[0] != inv.ID {
		t.Errorf("adapter issued = %v, want the invoice id", adapter.issued)
	}

	// A failed adapter leaves the invoice pending and surfaces the
	// sentinel the handler maps to EINVOICE_PROVIDER_ERROR.
	other := f.pendingInvoice()
	f.svc.EInvoice = &fakeEInvoice{issueErr: errors.New("vendor down")}
	if _, err := f.svc.Issue(f.ctx, other.ID, "admin-1"); !errors.Is(err, ErrEInvoiceProvider) {
		t.Fatalf("failing adapter = %v, want ErrEInvoiceProvider", err)
	}
	row, err := f.s.GetInvoice(f.ctx, other.ID)
	if err != nil || row.Status != model.InvoiceStatusPendingIssue {
		t.Fatalf("row after failed adapter = %+v, %v; want pending_issue", row, err)
	}
}

func TestVoidCallsAdapterOnlyForExternalRefs(t *testing.T) {
	f := newInvoiceFixture(t)
	inv := f.pendingInvoice()
	adapter := &fakeEInvoice{ref: EInvoiceRef{Provider: "sepay-einvoice", Ref: "HD-9", Status: "issued"}}
	f.svc.EInvoice = adapter
	if _, err := f.svc.Issue(f.ctx, inv.ID, "admin-1"); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := f.svc.Void(f.ctx, inv.ID, "admin-1"); err != nil {
		t.Fatalf("void: %v", err)
	}
	if len(adapter.voided) != 1 || adapter.voided[0].Ref != "HD-9" {
		t.Fatalf("adapter voided = %+v, want the external ref", adapter.voided)
	}

	// A manually issued invoice has no external ref; the adapter must not
	// be called for it.
	manual := f.pendingInvoice()
	adapter.voided = nil
	// Issued by hand, with no adapter running: the row has no external
	// ref even though an adapter is wired again by the time it is voided.
	f.svc.EInvoice = nil
	if _, err := f.svc.Issue(f.ctx, manual.ID, "admin-1"); err != nil {
		t.Fatalf("manual issue: %v", err)
	}
	f.svc.EInvoice = adapter
	if _, err := f.svc.Void(f.ctx, manual.ID, "admin-1"); err != nil {
		t.Fatalf("manual void: %v", err)
	}
	if len(adapter.voided) != 0 {
		t.Errorf("adapter voided = %+v, want no call without a ref", adapter.voided)
	}
}

func TestServiceGetAndList(t *testing.T) {
	f := newInvoiceFixture(t)
	order := f.order(orderOptions{})
	inv, err := f.svc.CreateForOrder(f.ctx, order)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := f.svc.Get(f.ctx, inv.ID)
	if err != nil || got.ID != inv.ID {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := f.svc.Get(f.ctx, "missing"); !errors.Is(err, store.ErrInvoiceNotFound) {
		t.Fatalf("get missing = %v", err)
	}
	rows, total, err := f.svc.List(f.ctx, ListFilter{OrderID: order.ID, Page: store.Page{Limit: 10}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].ID != inv.ID {
		t.Fatalf("list = %d rows / total %d, want the invoice", len(rows), total)
	}
	if _, _, err := f.svc.List(f.ctx, ListFilter{Status: model.InvoiceStatusVoid}); err != nil {
		t.Fatalf("list by status: %v", err)
	}
}

// The service keeps working with no webhook service: the invoice is the
// product, the event is a courtesy.
func TestCreateForOrderWithoutEmitter(t *testing.T) {
	f := newInvoiceFixture(t)
	f.svc.Events = nil
	if _, err := f.svc.CreateForOrder(f.ctx, f.order(orderOptions{})); err != nil {
		t.Fatalf("create without emitter: %v", err)
	}
}

// A nil storage seam still lets the PDF path run (covered in pdf_test);
// this pins the constructor default.
func TestNewWithDepsDefaultsStorage(t *testing.T) {
	svc := NewWithDeps(Deps{Store: testdb.Store(t)})
	if _, ok := svc.Storage.(storage.Disabled); !ok {
		t.Fatalf("storage = %T, want disabled", svc.Storage)
	}
}
