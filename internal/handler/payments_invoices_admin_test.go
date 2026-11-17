package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dtg-corp/dsoft/internal/middleware"
	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/invoice"
	"github.com/dtg-corp/dsoft/internal/store"
)

func invoiceAdminFixture(t *testing.T) (*paymentsFixture, *InvoicesAdminHandler, *invoice.Service) {
	t.Helper()
	f := newPaymentsFixture(t)
	svc := invoice.New(f.s, nil, nil)
	return f, &InvoicesAdminHandler{Invoices: svc}, svc
}

// issuedInvoice seeds a paid-shaped order and the invoice it produces.
func issuedInvoice(t *testing.T, f *paymentsFixture, svc *invoice.Service) *model.Invoice {
	t.Helper()
	order, _ := f.seedOrder(t, f.planA, f.email("invoice"), "sepay", 1_190_000, false)
	inv, err := svc.CreateForOrder(f.ctx, order)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	return inv
}

// pendingInvoice puts the manual queue in force (an e-invoice provider
// is configured) and seeds an invoice that waits in it.
func pendingInvoice(t *testing.T, f *paymentsFixture, svc *invoice.Service) *model.Invoice {
	t.Helper()
	if err := f.s.SetSetting(f.ctx, store.SettingEInvoiceProvider, "sepay-einvoice"); err != nil {
		t.Fatalf("set e-invoice provider: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.s.DB.NewRaw("DELETE FROM settings WHERE key = ?", store.SettingEInvoiceProvider).Exec(f.ctx)
	})
	order, _ := f.seedOrder(t, f.planA, f.email("pending"), "sepay", 500_000, false)
	inv, err := svc.CreateForOrder(f.ctx, order)
	if err != nil {
		t.Fatalf("create pending invoice: %v", err)
	}
	if inv.Status != model.InvoiceStatusPendingIssue {
		t.Fatalf("seed invoice status = %q, want pending_issue", inv.Status)
	}
	return inv
}

func invoiceParam(id string) gin.Param { return gin.Param{Key: "id", Value: id} }

func TestAdminInvoiceRoutes(t *testing.T) {
	f, h, svc := invoiceAdminFixture(t)
	inv := issuedInvoice(t, f, svc)

	// The list filter narrows to one order, so rows other tests left in
	// the shared database cannot change the count.
	w := f.record(h.List, http.MethodGet,
		"/api/v1/admin/payments/invoices?order_id="+inv.OrderID, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	data := dataOf(t, w)
	if data["total"] != float64(1) {
		t.Fatalf("total = %v, want 1 (%s)", data["total"], w.Body.String())
	}
	rows, _ := data["invoices"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want one", data["invoices"])
	}
	row, _ := rows[0].(map[string]any)
	if row["id"] != inv.ID || row["number"] != inv.Number || row["status"] != model.InvoiceStatusIssued {
		t.Errorf("row = %v", row)
	}
	for _, key := range []string{"subtotal_minor", "tax_minor", "total_minor", "currency", "pdf_url", "issued_at", "created_at"} {
		if _, ok := row[key]; !ok {
			t.Errorf("row is missing %q: %v", key, row)
		}
	}

	// Detail carries the frozen buyer block.
	w = f.record(h.Get, http.MethodGet, "/api/v1/admin/payments/invoices/"+inv.ID, "", nil, invoiceParam(inv.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("detail = %d %s", w.Code, w.Body.String())
	}
	detail, _ := dataOf(t, w)["invoice"].(map[string]any)
	if detail["id"] != inv.ID || detail["order_id"] != inv.OrderID {
		t.Fatalf("detail = %v", detail)
	}
	if buyer, ok := detail["buyer"].(map[string]any); !ok || buyer["email"] == "" {
		t.Errorf("buyer block = %v", detail["buyer"])
	}
	if url, _ := detail["pdf_url"].(string); !strings.HasSuffix(url, "/pdf") {
		t.Errorf("pdf_url = %q", url)
	}

	// The PDF is served inline with the invoice number in the filename.
	w = f.record(h.PDF, http.MethodGet, "/api/v1/admin/payments/invoices/"+inv.ID+"/pdf", "", nil, invoiceParam(inv.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("pdf = %d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/pdf" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := w.Header().Get("Content-Disposition"); !strings.Contains(got, inv.Number) || !strings.HasPrefix(got, "inline") {
		t.Errorf("Content-Disposition = %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	if !bytes.HasPrefix(w.Body.Bytes(), []byte("%PDF-")) {
		t.Error("body is not a PDF")
	}

	// A missing invoice is the pinned 404 on every id-taking route.
	for name, fn := range map[string]gin.HandlerFunc{
		"detail": h.Get, "issue": h.Issue, "void": h.Void, "pdf": h.PDF,
	} {
		code, out := f.call(fn, http.MethodPost, "/x", "", nil, invoiceParam("missing"))
		if code != http.StatusNotFound || errCode(out) != "INVOICE_NOT_FOUND" {
			t.Errorf("%s of a missing invoice = %d %v, want 404 INVOICE_NOT_FOUND", name, code, out)
		}
	}

	// Issuing an already-issued invoice is the pinned conflict.
	code, out := f.call(h.Issue, http.MethodPost, "/x", "", nil, invoiceParam(inv.ID))
	if code != http.StatusConflict || errCode(out) != "INVOICE_ALREADY_ISSUED" {
		t.Fatalf("issue issued invoice = %d %v, want 409 INVOICE_ALREADY_ISSUED", code, out)
	}
}

func TestAdminInvoiceIssueAndVoid(t *testing.T) {
	f, h, svc := invoiceAdminFixture(t)
	inv := pendingInvoice(t, f, svc)

	code, out := f.callAs("admin-user-1", h.Issue, http.MethodPost,
		"/api/v1/admin/payments/invoices/"+inv.ID+"/issue", "", invoiceParam(inv.ID))
	if code != http.StatusOK {
		t.Fatalf("issue = %d %v", code, out)
	}
	issued, _ := out["data"].(map[string]any)["invoice"].(map[string]any)
	if issued["status"] != model.InvoiceStatusIssued || issued["issued_at"] == nil {
		t.Fatalf("issued = %v", issued)
	}
	// A manual issue keeps the queued provider visible.
	if issued["e_invoice_provider"] != "sepay-einvoice" {
		t.Errorf("e_invoice_provider = %v", issued["e_invoice_provider"])
	}
	if code, out := f.callAs("admin-user-1", h.Issue, http.MethodPost, "/x", "", invoiceParam(inv.ID)); code != http.StatusConflict || errCode(out) != "INVOICE_ALREADY_ISSUED" {
		t.Fatalf("second issue = %d %v, want 409", code, out)
	}

	code, out = f.callAs("admin-user-2", h.Void, http.MethodPost, "/x",
		`{"reason":"issued to the wrong buyer"}`, invoiceParam(inv.ID))
	if code != http.StatusOK {
		t.Fatalf("void = %d %v", code, out)
	}
	voided, _ := out["data"].(map[string]any)["invoice"].(map[string]any)
	if voided["status"] != model.InvoiceStatusVoid {
		t.Fatalf("voided = %v", voided)
	}
	// The document is kept: number and amount survive the void.
	if voided["number"] != inv.Number || voided["total_minor"] != float64(inv.TotalMinor) {
		t.Errorf("void lost the document: %v", voided)
	}
	if code, out := f.callAs("admin-user-2", h.Void, http.MethodPost, "/x", "", invoiceParam(inv.ID)); code != http.StatusConflict || errCode(out) != "INVOICE_ALREADY_ISSUED" {
		t.Fatalf("second void = %d %v, want 409", code, out)
	}

	// A malformed body is refused before the service is reached.
	if code, out := f.callAs("admin-user-2", h.Void, http.MethodPost, "/x", "{not json", invoiceParam(inv.ID)); code != http.StatusBadRequest || errCode(out) != "BAD_REQUEST" {
		t.Fatalf("bad void body = %d %v, want 400 BAD_REQUEST", code, out)
	}
}

func TestAdminInvoiceListValidation(t *testing.T) {
	f, h, _ := invoiceAdminFixture(t)
	for _, tc := range []struct {
		name, query, code string
	}{
		{"unknown status", "?status=pending", "INVALID_STATUS"},
		{"bad from", "?from=yesterday", "BAD_REQUEST"},
		{"bad to", "?to=2026-13-01", "BAD_REQUEST"},
		{"backwards window", "?from=2026-11-02&to=2026-11-01", "BAD_REQUEST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := f.call(h.List, http.MethodGet, "/api/v1/admin/payments/invoices"+tc.query, "", nil)
			if code != http.StatusBadRequest || errCode(out) != tc.code {
				t.Fatalf("%s = %d %v, want 400 %s", tc.name, code, out, tc.code)
			}
		})
	}

	// A valid but empty filter is a 200 with an empty array, not null.
	code, out := f.call(h.List, http.MethodGet,
		"/api/v1/admin/payments/invoices?number=KG-1999-999999", "", nil)
	if code != http.StatusOK {
		t.Fatalf("empty list = %d %v", code, out)
	}
	data, _ := out["data"].(map[string]any)
	if rows, ok := data["invoices"].([]any); !ok || len(rows) != 0 {
		t.Fatalf("empty list rows = %v, want []", data["invoices"])
	}
}

// The invoice routes are only as safe as the middleware they are
// registered with: main mounts them on the admin group, so a
// payments:read key must not reach the list, the PDF or the queue.
func TestAdminInvoiceRoutesRequireAdmin(t *testing.T) {
	f, h, svc := invoiceAdminFixture(t)
	inv := issuedInvoice(t, f, svc)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterInvoicesAdminRoutes(r.Group("/api/v1/admin",
		middleware.APIKeyAuth(f.s), middleware.RequireScope(model.ScopeAdmin)), h)

	send := func(rawKey, method, path, body string) (int, string, []byte) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if rawKey != "" {
			req.Header.Set("Authorization", "Bearer "+rawKey)
		}
		r.ServeHTTP(w, req)
		return w.Code, w.Header().Get("Content-Type"), w.Body.Bytes()
	}

	if code, _, _ := send("", http.MethodGet, "/api/v1/admin/payments/invoices", ""); code != http.StatusUnauthorized {
		t.Fatalf("no key = %d, want 401", code)
	}
	readRaw := f.scopedAPIKey(t, model.ScopePaymentsRead)
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/payments/invoices"},
		{http.MethodGet, "/api/v1/admin/payments/invoices/" + inv.ID + "/pdf"},
		{http.MethodPost, "/api/v1/admin/payments/invoices/" + inv.ID + "/issue"},
	} {
		code, _, body := send(readRaw, probe.method, probe.path, "")
		if code != http.StatusForbidden {
			t.Fatalf("payments:read %s = %d %s, want 403", probe.path, code, body)
		}
		if bytes.Contains(body, []byte("buyer@")) {
			t.Fatalf("403 body leaks buyer data: %s", body)
		}
	}
	adminRaw := f.scopedAPIKey(t, model.ScopeAdmin)
	code, ctype, body := send(adminRaw, http.MethodGet, "/api/v1/admin/payments/invoices?order_id="+inv.OrderID, "")
	if code != http.StatusOK {
		t.Fatalf("admin list = %d %s", code, body)
	}
	if !strings.HasPrefix(ctype, "application/json") {
		t.Errorf("admin list Content-Type = %q", ctype)
	}
	var env struct {
		Data struct {
			Invoices []map[string]any `json:"invoices"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil || len(env.Data.Invoices) != 1 {
		t.Fatalf("admin list body = %s (%v)", body, err)
	}
}

func TestParseTimeParam(t *testing.T) {
	if got, err := parseTimeParam("2026-11-17T02:30:00Z", false); err != nil || !got.Equal(time.Date(2026, 11, 17, 2, 30, 0, 0, time.UTC)) {
		t.Errorf("RFC3339 = %v, %v", got, err)
	}
	// A bare date is a Ho Chi Minh City day, not a UTC one.
	if got, err := parseTimeParam("2026-11-01", false); err != nil || !got.Equal(time.Date(2026, 10, 31, 17, 0, 0, 0, time.UTC)) {
		t.Errorf("date start = %v, %v; want 2026-10-31T17:00Z", got, err)
	}
	if got, err := parseTimeParam("2026-11-01", true); err != nil || !got.Equal(time.Date(2026, 11, 1, 16, 59, 59, 999999999, time.UTC)) {
		t.Errorf("date end = %v, %v; want the last instant of the VN day", got, err)
	}
	if got, err := parseTimeParam("", false); err != nil || !got.IsZero() {
		t.Errorf("empty = %v, %v; want the zero time", got, err)
	}
	if _, err := parseTimeParam("17/11/2026", false); err == nil {
		t.Error("a non-ISO date was accepted")
	}
}
