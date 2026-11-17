package invoice

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/storage"
	"github.com/tabloy/keygate/internal/store"
)

// ─── storage double ───

type fakeStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
	gets    int
	putErr  error
}

func newFakeStorage() *fakeStorage { return &fakeStorage{objects: map[string][]byte{}} }

func (f *fakeStorage) Put(_ context.Context, key string, body []byte, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.objects[key] = append([]byte(nil), body...)
	f.puts++
	return nil
}

func (f *fakeStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	body, ok := f.objects[key]
	if !ok {
		return nil, storage.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func (f *fakeStorage) object(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[key]
}

func (f *fakeStorage) PresignedPut(_ context.Context, _, _ string, _ int64, _ time.Duration) (string, error) {
	return "", storage.ErrStorageDisabled
}

func (f *fakeStorage) PresignedGet(_ context.Context, _, _ string, _ time.Duration) (string, error) {
	return "", storage.ErrStorageDisabled
}

func (f *fakeStorage) Head(_ context.Context, key string) (*storage.ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.objects[key]
	if !ok {
		return nil, storage.ErrObjectNotFound
	}
	return &storage.ObjectInfo{Size: int64(len(body)), ContentType: "application/pdf"}, nil
}

func (f *fakeStorage) Exists(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok, nil
}

func (f *fakeStorage) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

func (f *fakeStorage) Copy(_ context.Context, src, dst string, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.objects[src]
	if !ok {
		return storage.ErrObjectNotFound
	}
	f.objects[dst] = append([]byte(nil), body...)
	return nil
}

// ─── renderer ───

// sampleDocument is a fully populated Vietnamese invoice with the money
// shape the spec's example uses.
func sampleDocument() pdfDocument {
	issued := time.Date(2026, 11, 17, 2, 30, 0, 0, time.UTC) // 09:30 in HCMC
	return pdfDocument{
		Invoice: &model.Invoice{
			ID: "inv-1", Number: "KG-2026-000001", OrderID: "order-1",
			Buyer: []byte(`{"name":"Nguyễn Văn Ánh","company_name":"Công ty ACME","tax_code":"0101234567",` +
				`"address":"1 Lê Lợi, Quận 1, TP. Hồ Chí Minh","email":"buyer@example.com","phone":"+84901234567"}`),
			Items: []model.InvoiceItem{{
				Description: "Gói Pro (1 năm)", PlanID: "plan-1", Quantity: 1,
				UnitAmountMinor: 1_190_000, AmountMinor: 1_190_000, TaxRateBP: 800,
			}},
			SubtotalMinor: 1_190_000, DiscountMinor: 0, TaxMinor: 88_148, TotalMinor: 1_190_000,
			Currency: "VND", CurrencyExp: 0, Status: model.InvoiceStatusIssued,
			IssuedAt: &issued, CreatedAt: issued,
		},
		Order: &model.PaymentOrder{Code: "KG-7F3K2Q", Provider: "sepay"},
		Seller: Seller{
			Name: "Công ty TNHH Keygate", TaxCode: "0312345678",
			Address: "12 Nguyễn Huệ, Quận 1, TP. Hồ Chí Minh", Email: "billing@keygate.app",
		},
		Note:            "Cảm ơn quý khách đã thanh toán.",
		Vi:              true,
		AttributionText: "Powered by Keygate",
		AttributionURL:  "https://keygate.app",
		GeneratedAt:     issued,
	}
}

func TestRenderInvoicePDFVietnamese(t *testing.T) {
	body, err := renderInvoicePDF(sampleDocument())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !bytes.HasPrefix(body, []byte("%PDF-")) {
		t.Fatalf("pdf header = %q", body[:min(8, len(body))])
	}
	if len(body) < 2000 {
		t.Errorf("pdf is %d bytes, want an embedded font and content", len(body))
	}
	// An embedded TrueType font shows up as a FontFile2 stream; without it
	// the Vietnamese diacritics would be missing glyphs.
	if !bytes.Contains(body, []byte("/FontFile2")) {
		t.Error("pdf does not embed a TrueType font")
	}
}

func TestRenderInvoicePDFEnglishUSD(t *testing.T) {
	d := sampleDocument()
	d.Vi = false
	d.Invoice.Currency, d.Invoice.CurrencyExp = "USD", 2
	d.Invoice.SubtotalMinor, d.Invoice.DiscountMinor = 123_450, 10_000
	d.Invoice.TaxMinor, d.Invoice.TotalMinor = 14_181, 127_631
	d.Invoice.Items = []model.InvoiceItem{{
		Description: "Pro plan (annual)", Quantity: 1, UnitAmountMinor: 123_450,
		AmountMinor: 123_450, TaxRateBP: 1250,
	}}
	body, err := renderInvoicePDF(d)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !bytes.HasPrefix(body, []byte("%PDF-")) {
		t.Fatal("not a pdf")
	}
	// A zero-total invoice must render too: free plans are invoiced.
	d.Invoice.TotalMinor = 0
	d.Invoice.SubtotalMinor, d.Invoice.DiscountMinor, d.Invoice.TaxMinor = 0, 0, 0
	if _, err := renderInvoicePDF(d); err != nil {
		t.Fatalf("render zero total: %v", err)
	}
}

func TestLabelsForVietnamese(t *testing.T) {
	l := labelsFor(true)
	if l.Total != "Tổng cộng" || l.Title != "HÓA ĐƠN GIÁ TRỊ GIA TĂNG" || l.Buyer != "NGƯỜI MUA" {
		t.Errorf("vi labels = %+v", l)
	}
	if en := labelsFor(false); en.Total != "Total" || en.Title != "VAT INVOICE" {
		t.Errorf("en labels = %+v", en)
	}
}

func TestFormatMinor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		amount int64
		exp    int16
		vi     bool
		want   string
	}{
		{"VND vi", 1_190_000, 0, true, "1.190.000"},
		{"VND en", 1_190_000, 0, false, "1,190,000"},
		{"USD vi", 123_450, 2, true, "1.234,50"},
		{"USD en", 123_450, 2, false, "1,234.50"},
		{"USD whole", 100_000, 2, false, "1,000.00"},
		{"USD cents", 5, 2, false, "0.05"},
		{"zero", 0, 0, true, "0"},
		{"negative", -1_000, 0, true, "-1.000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatMinor(tc.amount, tc.exp, tc.vi); got != tc.want {
				t.Errorf("formatMinor(%d, %d, %v) = %q, want %q", tc.amount, tc.exp, tc.vi, got, tc.want)
			}
		})
	}
	if got := formatMoney(1_190_000, 0, "VND", true); got != "1.190.000 VND" {
		t.Errorf("formatMoney = %q", got)
	}
}

func TestFormatRateBPAndNumberYear(t *testing.T) {
	for bp, want := range map[int]string{800: "8%", 1250: "12.5%", 1000: "10%", 825: "8.25%", 0: "0%"} {
		if got := formatRateBP(bp); got != want {
			t.Errorf("formatRateBP(%d) = %q, want %q", bp, got, want)
		}
	}
	created := time.Date(2025, 12, 31, 17, 30, 0, 0, time.UTC) // 2026 in HCMC
	if got := numberYear("KG-2026-000001", created); got != "2026" {
		t.Errorf("numberYear = %q", got)
	}
	if got := numberYear("garbage", created); got != "2026" {
		t.Errorf("numberYear fallback = %q, want the creation year in HCMC", got)
	}
}

// ─── service PDF path ───

func TestPDFMissingInvoice(t *testing.T) {
	f := newInvoiceFixture(t)
	if _, _, err := f.svc.PDF(f.ctx, "missing"); !errors.Is(err, store.ErrInvoiceNotFound) {
		t.Fatalf("pdf of a missing invoice = %v, want ErrInvoiceNotFound", err)
	}
}

func TestPDFDisabledStorageReturnsBytesWithoutPath(t *testing.T) {
	f := newInvoiceFixture(t)
	inv, err := f.svc.CreateForOrder(f.ctx, f.order(orderOptions{}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	body, got, err := f.svc.PDF(f.ctx, inv.ID)
	if err != nil {
		t.Fatalf("pdf: %v", err)
	}
	if !bytes.HasPrefix(body, []byte("%PDF-")) {
		t.Fatal("not a pdf")
	}
	if got.ID != inv.ID {
		t.Errorf("returned invoice = %s, want %s", got.ID, inv.ID)
	}
	if got.PDFPath != "" {
		t.Errorf("pdf_path = %q, want empty with storage disabled", got.PDFPath)
	}
	row, err := f.s.GetInvoice(f.ctx, inv.ID)
	if err != nil || row.PDFPath != "" {
		t.Fatalf("stored row pdf_path = %q, %v", row.PDFPath, err)
	}
}

func TestPDFStoresToStorageAndReusesIt(t *testing.T) {
	f := newInvoiceFixture(t)
	stor := newFakeStorage()
	f.svc.Storage = stor
	inv, err := f.svc.CreateForOrder(f.ctx, f.order(orderOptions{}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	first, got, err := f.svc.PDF(f.ctx, inv.ID)
	if err != nil {
		t.Fatalf("pdf: %v", err)
	}
	key := "invoices/2026/" + inv.Number + ".pdf"
	if got.PDFPath != key {
		t.Errorf("pdf_path = %q, want %q", got.PDFPath, key)
	}
	if puts := stor.puts; puts != 1 {
		t.Errorf("puts = %d, want 1", puts)
	}
	if stored := stor.object(key); !bytes.Equal(stored, first) {
		t.Error("stored object differs from the returned pdf")
	}
	row, err := f.s.GetInvoice(f.ctx, inv.ID)
	if err != nil || row.PDFPath != key {
		t.Fatalf("persisted pdf_path = %q, %v", row.PDFPath, err)
	}

	// The second call streams the object instead of rendering again.
	second, _, err := f.svc.PDF(f.ctx, inv.ID)
	if err != nil {
		t.Fatalf("second pdf: %v", err)
	}
	if stor.gets != 1 {
		t.Errorf("gets = %d, want 1 (served from storage)", stor.gets)
	}
	if !bytes.Equal(second, first) {
		t.Error("re-served pdf differs from the stored one")
	}

	// A missing object is regenerated rather than failing the download.
	if err := stor.Delete(f.ctx, key); err != nil {
		t.Fatalf("delete object: %v", err)
	}
	third, _, err := f.svc.PDF(f.ctx, inv.ID)
	if err != nil {
		t.Fatalf("third pdf: %v", err)
	}
	if !bytes.HasPrefix(third, []byte("%PDF-")) || stor.puts != 2 {
		t.Errorf("regenerate: prefix ok=%v puts=%d", bytes.HasPrefix(third, []byte("%PDF-")), stor.puts)
	}
}

func TestPDFStoragePutFailureStillReturnsBytes(t *testing.T) {
	f := newInvoiceFixture(t)
	stor := newFakeStorage()
	stor.putErr = errors.New("bucket unavailable")
	f.svc.Storage = stor
	inv, err := f.svc.CreateForOrder(f.ctx, f.order(orderOptions{}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	body, got, err := f.svc.PDF(f.ctx, inv.ID)
	if err != nil {
		t.Fatalf("pdf with failing storage: %v", err)
	}
	if !bytes.HasPrefix(body, []byte("%PDF-")) {
		t.Fatal("not a pdf")
	}
	if got.PDFPath != "" {
		t.Errorf("pdf_path = %q, want empty after a failed put", got.PDFPath)
	}
}

// The footer's attribution text comes from branding, never from a
// literal in this package (AGPL §7(b) — see AGENTS.md).
func TestPDFAttributionComesFromBranding(t *testing.T) {
	if strings.Contains(string(mustReadSource(t)), `"Powered by Keygate"`) {
		t.Fatal("pdf.go hardcodes the attribution text instead of using branding.Tagline")
	}
}

func mustReadSource(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile("pdf.go")
	if err != nil {
		t.Fatalf("read pdf.go: %v", err)
	}
	return body
}
