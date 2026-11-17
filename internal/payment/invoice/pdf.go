package invoice

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"

	"github.com/tabloy/keygate/internal/branding"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/storage"
	"github.com/tabloy/keygate/internal/store"
)

// notoSansTTF is Noto Sans Regular (OFL-1.1), embedded so a PDF renders
// Vietnamese diacritics without a font directory, a headless browser or
// an external service. Provenance and the licence text are in
// assets/README.md and assets/OFL.txt.
//
//go:embed assets/NotoSans-Regular.ttf
var notoSansTTF []byte

const fontFamily = "NotoSans"

// PDF renders (or re-serves) one invoice as a PDF and returns it with the
// invoice row it belongs to.
//
// The first call renders the document, stores it under
// invoices/<year>/<number>.pdf when object storage is configured, and
// records the path on the invoice. Later calls stream the stored object;
// when storage is disabled or the object has gone missing the PDF is
// rendered again on demand, because an invoice must always be
// downloadable (spec §3.8).
func (s *Service) PDF(ctx context.Context, id string) ([]byte, *model.Invoice, error) {
	inv, order, err := s.Store.GetInvoiceWithOrder(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if inv.PDFPath != "" && s.storageEnabled() {
		body, err := s.readStored(ctx, inv.PDFPath)
		if err == nil {
			return body, inv, nil
		}
		s.Logger.Warn("invoice: stored pdf unreadable, rendering again",
			"invoice", inv.ID, "path", inv.PDFPath, "error", err)
	}

	doc := s.document(ctx, inv, order)
	body, err := renderInvoicePDF(doc)
	if err != nil {
		return nil, inv, err
	}
	if s.storageEnabled() {
		key := "invoices/" + numberYear(inv.Number, inv.CreatedAt) + "/" + inv.Number + ".pdf"
		if err := s.Storage.Put(ctx, key, body, "application/pdf"); err != nil {
			// The buyer still gets the document; only persistence failed.
			s.Logger.Warn("invoice: storing pdf failed", "invoice", inv.ID, "key", key, "error", err)
		} else if err := s.Store.SetInvoicePDFPath(ctx, inv.ID, key); err != nil {
			s.Logger.Warn("invoice: recording pdf path failed", "invoice", inv.ID, "key", key, "error", err)
		} else {
			inv.PDFPath = key
		}
	}
	return body, inv, nil
}

// storageEnabled reports whether a real object store is configured.
// storage.Disabled is the documented no-op implementation.
func (s *Service) storageEnabled() bool {
	switch s.Storage.(type) {
	case nil, storage.Disabled, *storage.Disabled:
		return false
	}
	return true
}

func (s *Service) readStored(ctx context.Context, key string) ([]byte, error) {
	body, err := s.Storage.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(body)
}

// numberYear is the year segment of an invoice number, used for the
// storage prefix. A malformed number falls back to the creation year.
func numberYear(number string, createdAt time.Time) string {
	parts := strings.Split(number, "-")
	if len(parts) >= 2 && len(parts[1]) == 4 {
		if _, err := strconv.Atoi(parts[1]); err == nil {
			return parts[1]
		}
	}
	return createdAt.In(zone).Format("2006")
}

// Seller is the invoice seller block, read from the install settings.
// It is intentionally not a legal-entity table: a self-hosted install
// configures four strings.
type Seller struct {
	Name    string
	TaxCode string
	Address string
	Email   string
}

// pdfDocument is everything one rendered invoice shows. Building it
// apart from rendering keeps the layout testable with fixed values.
type pdfDocument struct {
	Invoice         *model.Invoice
	Order           *model.PaymentOrder
	Seller          Seller
	Note            string
	Vi              bool
	AttributionText string
	AttributionURL  string
	GeneratedAt     time.Time
}

// document assembles the render input from the invoice, the order and
// the install settings.
func (s *Service) document(ctx context.Context, inv *model.Invoice, order *model.PaymentOrder) pdfDocument {
	attribution := s.setting(ctx, store.SettingAttributionURL, "")
	if attribution == "" {
		attribution = branding.URL
	}
	return pdfDocument{
		Invoice: inv, Order: order,
		Seller: Seller{
			Name:    s.setting(ctx, store.SettingInvoiceSellerName, branding.Project),
			TaxCode: s.setting(ctx, store.SettingInvoiceSellerTaxCode, ""),
			Address: s.setting(ctx, store.SettingInvoiceSellerAddress, ""),
			Email:   s.setting(ctx, store.SettingInvoiceSellerEmail, ""),
		},
		Note:            s.setting(ctx, store.SettingInvoiceNote, ""),
		Vi:              strings.EqualFold(s.setting(ctx, store.SettingLanguage, "en"), "vi"),
		AttributionText: branding.Tagline,
		AttributionURL:  attribution,
		GeneratedAt:     s.Now().UTC(),
	}
}

// invoiceLabels is the document's vocabulary. Vietnamese is the default
// for a Vietnamese install; English is the fallback so an untranslated
// install still prints a complete document.
type invoiceLabels struct {
	Title, Number, IssuedOn        string
	Seller, Buyer                  string
	TaxCode, Address, Email, Phone string
	Description, Qty, UnitPrice    string
	Amount, Subtotal, Discount     string
	VAT, Total                     string
	OrderCode, Payment, Note       string
	VATRate                        string
}

func labelsFor(vi bool) invoiceLabels {
	if vi {
		return invoiceLabels{
			Title: "HÓA ĐƠN GIÁ TRỊ GIA TĂNG", Number: "Số hóa đơn", IssuedOn: "Ngày phát hành",
			Seller: "NGƯỜI BÁN", Buyer: "NGƯỜI MUA",
			TaxCode: "Mã số thuế", Address: "Địa chỉ", Email: "Email", Phone: "Điện thoại",
			Description: "Mô tả", Qty: "SL", UnitPrice: "Đơn giá",
			Amount: "Thành tiền", Subtotal: "Tạm tính", Discount: "Giảm giá",
			VAT: "Thuế GTGT", Total: "Tổng cộng",
			OrderCode: "Mã đơn hàng", Payment: "Thanh toán", Note: "Ghi chú", VATRate: "Thuế suất",
		}
	}
	return invoiceLabels{
		Title: "VAT INVOICE", Number: "Invoice number", IssuedOn: "Issued on",
		Seller: "SELLER", Buyer: "BUYER",
		TaxCode: "Tax code", Address: "Address", Email: "Email", Phone: "Phone",
		Description: "Description", Qty: "Qty", UnitPrice: "Unit price",
		Amount: "Amount", Subtotal: "Subtotal", Discount: "Discount",
		VAT: "VAT", Total: "Total",
		OrderCode: "Order code", Payment: "Payment", Note: "Note", VATRate: "VAT rate",
	}
}

// renderInvoicePDF lays the document out on A4 and returns its bytes.
func renderInvoicePDF(d pdfDocument) ([]byte, error) {
	l := labelsFor(d.Vi)
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(15, 15, 15)
	pdf.SetAutoPageBreak(true, 24)
	pdf.AddUTF8FontFromBytes(fontFamily, "", notoSansTTF)
	if err := pdf.Error(); err != nil {
		return nil, fmt.Errorf("invoice pdf: embedding font: %w", err)
	}
	pdf.SetTitle("Invoice "+d.Invoice.Number, true)
	pdf.SetAuthor(d.Seller.Name, true)
	pdf.SetCreator(branding.Project, true)

	drawFooter := func() {
		pdf.SetY(-20)
		pdf.SetDrawColor(226, 232, 240)
		pdf.Line(15, pdf.GetY(), 195, pdf.GetY())
		pdf.SetY(-17)
		pdf.SetFont(fontFamily, "", 8)
		pdf.SetTextColor(120, 127, 140)
		pdf.CellFormat(0, 4, d.AttributionText, "", 1, "C", false, 0, d.AttributionURL)
		pdf.SetFont(fontFamily, "", 7)
		pdf.CellFormat(0, 4, d.AttributionURL, "", 0, "C", false, 0, d.AttributionURL)
		pdf.SetTextColor(0, 0, 0)
	}
	pdf.SetFooterFunc(drawFooter)
	pdf.AddPage()

	contentWidth := 180.0
	drawHeader(pdf, d, l)
	drawBuyer(pdf, d, l)
	drawItems(pdf, d, l)
	drawTotals(pdf, d, l, contentWidth)
	drawFooterNotes(pdf, d, l)

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("invoice pdf: %w", err)
	}
	return buf.Bytes(), nil
}

// drawHeader prints the seller, the title and the invoice identity.
func drawHeader(pdf *fpdf.Fpdf, d pdfDocument, l invoiceLabels) {
	pdf.SetTextColor(15, 23, 42)
	pdf.SetFont(fontFamily, "", 14)
	pdf.CellFormat(105, 7, clipText(pdf, d.Seller.Name, 105), "", 0, "L", false, 0, "")
	pdf.SetFont(fontFamily, "", 17)
	pdf.SetTextColor(30, 41, 59)
	pdf.CellFormat(75, 7, l.Title, "", 1, "R", false, 0, "")

	// Seller facts on the left, invoice identity on the right, two
	// columns kept in step line by line.
	pdf.SetFont(fontFamily, "", 8.5)
	left := []string{}
	if d.Seller.Address != "" {
		left = append(left, l.Address+": "+d.Seller.Address)
	}
	if d.Seller.TaxCode != "" {
		left = append(left, l.TaxCode+": "+d.Seller.TaxCode)
	}
	if d.Seller.Email != "" {
		left = append(left, l.Email+": "+d.Seller.Email)
	}
	right := []string{
		l.Number + ": " + d.Invoice.Number,
		l.IssuedOn + ": " + formatDateTime(issueTime(d), d.Vi),
	}
	for i := 0; i < len(left) || i < len(right); i++ {
		if i < len(left) {
			pdf.SetTextColor(71, 85, 105)
			pdf.CellFormat(105, 4.6, clipText(pdf, left[i], 105), "", 0, "L", false, 0, "")
		} else {
			pdf.CellFormat(105, 4.6, "", "", 0, "L", false, 0, "")
		}
		if i < len(right) {
			pdf.SetTextColor(30, 41, 59)
			pdf.CellFormat(75, 4.6, clipText(pdf, right[i], 75), "", 1, "R", false, 0, "")
		} else {
			pdf.CellFormat(75, 4.6, "", "", 1, "R", false, 0, "")
		}
	}
	pdf.SetDrawColor(30, 41, 59)
	pdf.SetLineWidth(0.6)
	pdf.Line(15, pdf.GetY()+2, 195, pdf.GetY()+2)
	pdf.Ln(6)
	pdf.SetLineWidth(0.2)
}

// drawBuyer prints the buyer block on a light panel.
func drawBuyer(pdf *fpdf.Fpdf, d pdfDocument, l invoiceLabels) {
	buyer := BuyerOf(d.Invoice.Buyer)
	lines := []string{}
	if buyer.CompanyName != "" && buyer.CompanyName != buyer.Name {
		lines = append(lines, buyer.CompanyName)
	}
	if buyer.Address != "" {
		lines = append(lines, buyer.Address)
	}
	facts := []string{}
	if buyer.TaxCode != "" {
		facts = append(facts, l.TaxCode+": "+buyer.TaxCode)
	}
	if buyer.Email != "" {
		facts = append(facts, l.Email+": "+buyer.Email)
	}
	if buyer.Phone != "" {
		facts = append(facts, l.Phone+": "+buyer.Phone)
	}
	if len(facts) > 0 {
		lines = append(lines, strings.Join(facts, "   ·   "))
	}

	height := 14.0 + 5.2*float64(len(lines)+1)
	pdf.SetFillColor(246, 248, 251)
	pdf.Rect(15, pdf.GetY(), 180, height, "F")
	pdf.SetY(pdf.GetY() + 2)
	pdf.SetX(18)
	pdf.SetFont(fontFamily, "", 7.5)
	pdf.SetTextColor(100, 116, 139)
	pdf.CellFormat(0, 4, l.Buyer, "", 1, "L", false, 0, "")
	pdf.SetX(18)
	pdf.SetFont(fontFamily, "", 11)
	pdf.SetTextColor(15, 23, 42)
	name := buyer.Name
	if name == "" {
		name = d.Order.Email
	}
	pdf.CellFormat(0, 6, clipText(pdf, name, 174), "", 1, "L", false, 0, "")
	pdf.SetFont(fontFamily, "", 8.5)
	pdf.SetTextColor(71, 85, 105)
	for _, line := range lines {
		pdf.SetX(18)
		pdf.CellFormat(0, 5.2, clipText(pdf, line, 174), "", 1, "L", false, 0, "")
	}
	pdf.Ln(5)
}

// drawItems prints the line-item table. A phase-1b invoice has one line,
// but the loop keeps the layout correct if a later module snapshots more.
func drawItems(pdf *fpdf.Fpdf, d pdfDocument, l invoiceLabels) {
	const (
		colDesc   = 92.0
		colQty    = 14.0
		colUnit   = 37.0
		colAmount = 37.0
	)
	pdf.SetFont(fontFamily, "", 8)
	pdf.SetTextColor(71, 85, 105)
	pdf.SetFillColor(238, 241, 246)
	pdf.SetDrawColor(226, 232, 240)
	pdf.CellFormat(colDesc, 7.5, " "+l.Description, "B", 0, "L", true, 0, "")
	pdf.CellFormat(colQty, 7.5, l.Qty, "B", 0, "C", true, 0, "")
	pdf.CellFormat(colUnit, 7.5, l.UnitPrice, "B", 0, "R", true, 0, "")
	pdf.CellFormat(colAmount, 7.5, l.Amount+" ", "B", 1, "R", true, 0, "")

	pdf.SetFont(fontFamily, "", 9)
	pdf.SetTextColor(15, 23, 42)
	for _, item := range d.Invoice.Items {
		desc := item.Description
		if item.TaxRateBP > 0 {
			desc += "  ·  " + l.VATRate + " " + formatRateBP(item.TaxRateBP)
		}
		pdf.CellFormat(colDesc, 7.5, " "+clipText(pdf, desc, colDesc-2), "B", 0, "L", false, 0, "")
		pdf.CellFormat(colQty, 7.5, strconv.Itoa(item.Quantity), "B", 0, "C", false, 0, "")
		pdf.CellFormat(colUnit, 7.5, formatMoney(item.UnitAmountMinor, d.Invoice.CurrencyExp, d.Invoice.Currency, d.Vi), "B", 0, "R", false, 0, "")
		pdf.CellFormat(colAmount, 7.5, formatMoney(item.AmountMinor, d.Invoice.CurrencyExp, d.Invoice.Currency, d.Vi)+" ", "B", 1, "R", false, 0, "")
	}
	pdf.Ln(4)
}

// drawTotals prints the amount block, right-aligned above the payment
// reference.
func drawTotals(pdf *fpdf.Fpdf, d pdfDocument, l invoiceLabels, contentWidth float64) {
	inv := d.Invoice
	type totalRow struct {
		label  string
		amount int64
	}
	rows := []totalRow{{l.Subtotal, inv.SubtotalMinor}}
	if inv.DiscountMinor > 0 {
		rows = append(rows, totalRow{l.Discount, -inv.DiscountMinor})
	}
	if inv.TaxMinor > 0 {
		rows = append(rows, totalRow{l.VAT, inv.TaxMinor})
	}

	// The block sits at the right edge of the content area; its width is
	// fixed so every line starts at the same x and nothing overflows the
	// page (an overflowing cell would be pushed onto the next line).
	const (
		labelWidth  = 50.0
		amountWidth = 45.0
	)
	startX := 15 + contentWidth - labelWidth - amountWidth
	for _, row := range rows {
		pdf.SetX(startX)
		pdf.SetFont(fontFamily, "", 9)
		pdf.SetTextColor(71, 85, 105)
		pdf.CellFormat(labelWidth, 6, row.label, "", 0, "L", false, 0, "")
		pdf.SetTextColor(15, 23, 42)
		pdf.CellFormat(amountWidth, 6, formatMoney(row.amount, inv.CurrencyExp, inv.Currency, d.Vi)+" ", "", 1, "R", false, 0, "")
	}
	pdf.SetX(startX)
	pdf.SetFont(fontFamily, "", 11)
	pdf.SetTextColor(255, 255, 255)
	pdf.SetFillColor(30, 41, 59)
	pdf.CellFormat(labelWidth, 9, " "+l.Total, "", 0, "L", true, 0, "")
	pdf.CellFormat(amountWidth, 9, formatMoney(inv.TotalMinor, inv.CurrencyExp, inv.Currency, d.Vi)+" ", "", 1, "R", true, 0, "")
	pdf.SetTextColor(0, 0, 0)
	pdf.Ln(6)
}

// drawFooterNotes prints the payment reference and the operator's note.
func drawFooterNotes(pdf *fpdf.Fpdf, d pdfDocument, l invoiceLabels) {
	pdf.SetFont(fontFamily, "", 8.5)
	pdf.SetTextColor(71, 85, 105)
	ref := d.Order.Code
	if d.Order.Provider != "" {
		ref += "  ·  " + l.Payment + ": " + d.Order.Provider
	}
	pdf.CellFormat(0, 5, l.OrderCode+": "+ref, "", 1, "L", false, 0, "")
	if d.Note != "" {
		pdf.SetFont(fontFamily, "", 8)
		pdf.MultiCell(0, 4.6, l.Note+": "+d.Note, "", "L", false)
	}
}

// issueTime is the date the invoice claims: its issue date when issued,
// otherwise its creation.
func issueTime(d pdfDocument) time.Time {
	if d.Invoice.IssuedAt != nil {
		return *d.Invoice.IssuedAt
	}
	return d.Invoice.CreatedAt
}

// formatDateTime renders a UTC timestamp in Ho Chi Minh City time, the
// calendar the document is issued in.
func formatDateTime(t time.Time, vi bool) string {
	local := t.In(zone)
	if vi {
		return local.Format("02/01/2006 15:04")
	}
	return local.Format("2006-01-02 15:04")
}

// formatRateBP renders basis points as a percentage: 800 → "8%",
// 1250 → "12.5%".
func formatRateBP(bp int) string {
	whole := bp / 100
	frac := bp % 100
	if frac == 0 {
		return strconv.Itoa(whole) + "%"
	}
	if frac%10 == 0 {
		return fmt.Sprintf("%d.%d%%", whole, frac/10)
	}
	return fmt.Sprintf("%d.%02d%%", whole, frac)
}

// formatMoney renders minor units with the currency's exponent and
// locale grouping, followed by the currency code.
func formatMoney(minor int64, exp int16, currency string, vi bool) string {
	text := formatMinor(minor, exp, vi)
	if currency != "" {
		text += " " + currency
	}
	return text
}

// formatMinor renders an amount in minor units. VND (exponent 0) has no
// decimal part; USD (exponent 2) always shows two digits. Grouping
// follows the document language: Vietnamese uses "." thousands and ","
// decimals, English the reverse.
func formatMinor(amount int64, exp int16, vi bool) string {
	sign := ""
	if amount < 0 {
		sign, amount = "-", -amount
	}
	if exp <= 0 {
		return sign + groupDigits(amount, vi)
	}
	scale := int64(1)
	for i := int16(0); i < exp; i++ {
		scale *= 10
	}
	whole, frac := amount/scale, amount%scale
	decimal := "."
	if vi {
		decimal = ","
	}
	return sign + groupDigits(whole, vi) + decimal + fmt.Sprintf("%0*d", exp, frac)
}

// groupDigits inserts the thousands separator every three digits.
func groupDigits(n int64, vi bool) string {
	sep := ","
	if vi {
		sep = "."
	}
	digits := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, c := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteString(sep)
		}
		b.WriteRune(c)
	}
	return b.String()
}

// clipText shortens text that would overrun its column, so a long plan
// name cannot push the layout apart. The ellipsis is a plain "...":
// the embedded font has no reason to carry more glyphs than the
// document needs.
func clipText(pdf *fpdf.Fpdf, text string, width float64) string {
	if pdf.GetStringWidth(text) <= width {
		return text
	}
	runes := []rune(text)
	for len(runes) > 0 {
		runes = runes[:len(runes)-1]
		candidate := string(runes) + "..."
		if pdf.GetStringWidth(candidate) <= width {
			return candidate
		}
	}
	return ""
}
