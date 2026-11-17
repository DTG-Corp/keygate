package handler

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/dtg-corp/dsoft/internal/model"
)

func TestConsumerInvoiceJSONAndPDF(t *testing.T) {
	f, _, svc := invoiceAdminFixture(t)
	inv := issuedInvoice(t, f, svc)
	h := &ConsumerInvoiceHandler{Invoices: svc, Store: f.s}
	key := f.boundKey(t, f.prodA.ID, model.ScopePaymentsRead)

	w := f.record(h.Get, http.MethodGet, "/api/v1/payments/invoices/"+inv.ID, "", key, invoiceParam(inv.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("json = %d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	data := dataOf(t, w)
	row, _ := data["invoice"].(map[string]any)
	if row["id"] != inv.ID || row["number"] != inv.Number || row["status"] != model.InvoiceStatusIssued {
		t.Fatalf("invoice = %v", row)
	}
	if row["total_minor"] != float64(inv.TotalMinor) || row["currency"] != inv.Currency {
		t.Errorf("money = %v", row)
	}
	if items, ok := row["items"].([]any); !ok || len(items) != 1 {
		t.Errorf("items = %v", row["items"])
	}
	if buyer, ok := row["buyer"].(map[string]any); !ok || buyer["email"] != "invoice-"+f.suffix+"@example.com" {
		t.Errorf("buyer = %v", row["buyer"])
	}
	// Operator-only fields stay out of the merchant response.
	for _, leaked := range []string{"pdf_path", "e_invoice_ref", "e_invoice_status"} {
		if _, ok := row[leaked]; ok {
			t.Errorf("consumer response carries %q: %v", leaked, row)
		}
	}
	if url, _ := row["pdf_url"].(string); !strings.HasSuffix(url, "?format=pdf") {
		t.Errorf("pdf_url = %q", url)
	}

	w = f.record(h.Get, http.MethodGet, "/api/v1/payments/invoices/"+inv.ID+"?format=pdf", "", key, invoiceParam(inv.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("pdf = %d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/pdf" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := w.Header().Get("Content-Disposition"); !strings.Contains(got, inv.Number) {
		t.Errorf("Content-Disposition = %q", got)
	}
	if !bytes.HasPrefix(w.Body.Bytes(), []byte("%PDF-")) {
		t.Error("body is not a PDF")
	}
}

func TestConsumerInvoiceScoping(t *testing.T) {
	f, _, svc := invoiceAdminFixture(t)
	inv := issuedInvoice(t, f, svc)
	h := &ConsumerInvoiceHandler{Invoices: svc, Store: f.s}

	// A key bound to another product sees a 404, and nothing about the
	// invoice — not even that it exists.
	other := f.boundKey(t, f.prodB.ID, model.ScopePaymentsRead)
	code, out := f.call(h.Get, http.MethodGet, "/x", "", other, invoiceParam(inv.ID))
	if code != http.StatusNotFound || errCode(out) != "INVOICE_NOT_FOUND" {
		t.Fatalf("other product = %d %v, want 404 INVOICE_NOT_FOUND", code, out)
	}
	body, _ := out["error"].(map[string]any)
	if msg, _ := body["message"].(string); strings.Contains(msg, "example.com") {
		t.Errorf("scoping error leaks buyer data: %v", out)
	}

	// An install-wide key reads any invoice.
	unbound := f.key(t, model.ScopePaymentsRead)
	if code, out := f.call(h.Get, http.MethodGet, "/x", "", unbound, invoiceParam(inv.ID)); code != http.StatusOK {
		t.Fatalf("unbound key = %d %v", code, out)
	}

	// Unknown id and bad format.
	if code, out := f.call(h.Get, http.MethodGet, "/x", "", unbound, invoiceParam("missing")); code != http.StatusNotFound || errCode(out) != "INVOICE_NOT_FOUND" {
		t.Fatalf("missing = %d %v", code, out)
	}
	if code, out := f.call(h.Get, http.MethodGet, "/x?format=xml", "", unbound, invoiceParam(inv.ID)); code != http.StatusBadRequest || errCode(out) != "BAD_REQUEST" {
		t.Fatalf("bad format = %d %v", code, out)
	}
}

// The registration function mounts the route under the group main hands
// it, so the documented path is what the server serves.
func TestRegisterConsumerInvoiceRoutesPath(t *testing.T) {
	f, _, svc := invoiceAdminFixture(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterConsumerInvoiceRoutes(r.Group("/api/v1/payments"),
		&ConsumerInvoiceHandler{Invoices: svc, Store: f.s})
	found := false
	for _, route := range r.Routes() {
		if route.Method == http.MethodGet && route.Path == "/api/v1/payments/invoices/:id" {
			found = true
		}
	}
	if !found {
		t.Fatalf("route not registered; routes = %+v", r.Routes())
	}
}
