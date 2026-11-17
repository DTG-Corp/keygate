package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
)

// Invoices are audited documents, so unlike orders they are never
// edited: a row is inserted once with its number and money snapshot and
// afterwards only its status, issue date and e-invoice/PDF references
// move. The guards below are the state machine; a transition that does
// not apply answers a sentinel instead of silently writing nothing.
var (
	// ErrInvoiceNotFound means no invoices row carries the id asked for.
	ErrInvoiceNotFound = errors.New("invoice not found")

	// ErrInvoiceAlreadyIssued means an issue was asked for an invoice
	// that is already issued or void: only a pending invoice can be
	// issued, and nothing re-opens an issued one.
	ErrInvoiceAlreadyIssued = errors.New("invoice is already issued")

	// ErrInvoiceNotVoidable means a void was asked for an invoice that
	// is already void.
	ErrInvoiceNotVoidable = errors.New("invoice cannot be voided")
)

// invoiceZone is the calendar the invoice number's year and the report
// buckets use. It is a fixed offset, like the provider adapters, so the
// server needs no tzdata and a VN new year never depends on the host's
// locale.
var invoiceZone = time.FixedZone("ICT", 7*3600)

// EInvoiceRef is one e-invoice adapter's handle for an invoice. The
// store keeps it as three strings; the invoice package owns the vendor
// interface, so the store never imports an adapter.
type EInvoiceRef struct {
	Provider string
	Ref      string
	Status   string
}

// InvoiceFilter is the invoice list query. Empty fields do not filter;
// From/To bound created_at inclusively. ProductID narrows through the
// order, because an invoice stores no product of its own.
type InvoiceFilter struct {
	Status, LicenseID, OrderID, Number, ProductID string
	From, To                                      *time.Time
	Page                                          Page
}

// CreateInvoiceInput is the snapshot CreateInvoice writes. The caller
// (the invoice service) has already copied the money fields from the
// paid order and decided the opening status; the store owns only the
// number, the id and the timestamps.
type CreateInvoiceInput struct {
	OrderID, LicenseID, EInvoiceProvider string
	Buyer                                []byte
	Items                                []model.InvoiceItem
	SubtotalMinor, DiscountMinor         int64
	TaxMinor, TotalMinor                 int64
	Currency                             string
	CurrencyExponent                     int16
	Status                               string
	IssuedAt                             *time.Time
	// NumberYear is the calendar year of the invoice number. The service
	// passes the Asia/Ho_Chi_Minh year; zero means "this year in that
	// zone", which only direct callers and tests rely on.
	NumberYear int
}

// validateCreateInvoiceInput rejects a row the schema would not accept,
// before a number is allocated for it.
func validateCreateInvoiceInput(in CreateInvoiceInput) error {
	switch {
	case in.OrderID == "":
		return errors.New("invoice order id is required")
	case in.Status != model.InvoiceStatusIssued && in.Status != model.InvoiceStatusPendingIssue:
		return fmt.Errorf("invoice cannot be created with status %q", in.Status)
	case in.SubtotalMinor < 0 || in.DiscountMinor < 0 || in.TaxMinor < 0 || in.TotalMinor < 0:
		return errors.New("invoice amounts cannot be negative")
	case len(in.Currency) != 3:
		return fmt.Errorf("invoice currency %q must be three letters", in.Currency)
	}
	return nil
}

// CreateInvoice inserts one invoice, allocating the next number of its
// year inside the same transaction. It is idempotent by order: when the
// order already has an invoice, that row is returned with created=false
// and no number is spent.
func (s *Store) CreateInvoice(ctx context.Context, in CreateInvoiceInput) (*model.Invoice, bool, error) {
	if err := validateCreateInvoiceInput(in); err != nil {
		return nil, false, err
	}
	buyer := in.Buyer
	if len(buyer) == 0 {
		buyer = []byte(`{}`)
	}
	items := in.Items
	if items == nil {
		items = []model.InvoiceItem{}
	}
	year := in.NumberYear
	if year == 0 {
		year = time.Now().In(invoiceZone).Year()
	}
	inv := &model.Invoice{
		ID: NewID(), OrderID: in.OrderID, LicenseID: in.LicenseID,
		Buyer: buyer, Items: items,
		SubtotalMinor: in.SubtotalMinor, DiscountMinor: in.DiscountMinor,
		TaxMinor: in.TaxMinor, TotalMinor: in.TotalMinor,
		Currency: in.Currency, CurrencyExp: in.CurrencyExponent,
		Status: in.Status, EInvoiceProvider: in.EInvoiceProvider,
		IssuedAt: in.IssuedAt,
	}
	created := false
	err := s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		number, err := nextInvoiceNumber(ctx, tx, year)
		if err != nil {
			return err
		}
		inv.Number = number
		now := time.Now().UTC()
		inv.CreatedAt, inv.UpdatedAt = now, now
		if _, err := tx.NewInsert().Model(inv).Exec(ctx); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		// The order already has an invoice: the winner committed before
		// the unique index could reject this insert, and the number this
		// transaction allocated rolled back with it.
		if isUniqueViolationOn(err, "invoices_order_id_key") {
			existing, getErr := s.GetInvoiceByOrderID(ctx, in.OrderID)
			if getErr != nil {
				return nil, false, getErr
			}
			return existing, false, nil
		}
		return nil, false, err
	}
	return inv, created, nil
}

// nextInvoiceNumber advances the per-year counter row and returns the
// number it produced. ON CONFLICT DO UPDATE takes the row lock, so two
// concurrent invoices serialize on it and never see the same sequence;
// a plain SELECT-then-UPDATE would hand both the same number.
func nextInvoiceNumber(ctx context.Context, tx bun.Tx, year int) (string, error) {
	var seq int64
	err := tx.NewRaw(
		`INSERT INTO invoice_counters (year, last_seq) VALUES (?, 1)
		 ON CONFLICT (year) DO UPDATE SET last_seq = invoice_counters.last_seq + 1
		 RETURNING last_seq`, year).Scan(ctx, &seq)
	if err != nil {
		return "", fmt.Errorf("allocate invoice number for %d: %w", year, err)
	}
	return fmt.Sprintf("KG-%04d-%06d", year, seq), nil
}

// GetInvoice reads one invoice by id.
func (s *Store) GetInvoice(ctx context.Context, id string) (*model.Invoice, error) {
	inv := new(model.Invoice)
	err := s.DB.NewSelect().Model(inv).Where("id = ?", id).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvoiceNotFound
	}
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// GetInvoiceByOrderID reads the invoice a paid order produced. The
// unique constraint on order_id is what makes creation idempotent, so
// this is the lookup its duplicate path and the PaidHook use.
func (s *Store) GetInvoiceByOrderID(ctx context.Context, orderID string) (*model.Invoice, error) {
	inv := new(model.Invoice)
	err := s.DB.NewSelect().Model(inv).Where("order_id = ?", orderID).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvoiceNotFound
	}
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// GetInvoiceWithOrder returns an invoice and the order it bills. The
// consumer API needs the order's product to scope a merchant key, and
// the PDF needs the order code printed as the payment reference.
func (s *Store) GetInvoiceWithOrder(ctx context.Context, id string) (*model.Invoice, *model.PaymentOrder, error) {
	inv, err := s.GetInvoice(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	order := new(model.PaymentOrder)
	err = s.DB.NewSelect().Model(order).Where("id = ?", inv.OrderID).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("invoice %s: order %s is missing", inv.ID, inv.OrderID)
	}
	if err != nil {
		return nil, nil, err
	}
	return inv, order, nil
}

// ListInvoices returns one page of invoices and how many the filter
// matched. Newest first, with the id as tiebreaker so pages line up.
func (s *Store) ListInvoices(ctx context.Context, f InvoiceFilter) ([]*model.Invoice, int, error) {
	q := f.apply(s.DB.NewSelect().Model((*model.Invoice)(nil)))
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	rows := make([]*model.Invoice, 0)
	q = q.Model(&rows).OrderExpr("created_at DESC").OrderExpr("id DESC")
	if f.Page.Limit > 0 {
		q = q.Limit(f.Page.Limit).Offset(f.Page.Offset)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

func (f InvoiceFilter) apply(q *bun.SelectQuery) *bun.SelectQuery {
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.LicenseID != "" {
		q = q.Where("license_id = ?", f.LicenseID)
	}
	if f.OrderID != "" {
		q = q.Where("order_id = ?", f.OrderID)
	}
	if f.Number != "" {
		q = q.Where("number = ?", f.Number)
	}
	if f.ProductID != "" {
		q = q.Where("order_id IN (SELECT id FROM payment_orders WHERE product_id = ?)", f.ProductID)
	}
	if f.From != nil {
		q = q.Where("created_at >= ?", f.From.UTC())
	}
	if f.To != nil {
		q = q.Where("created_at <= ?", f.To.UTC())
	}
	return q
}

// MarkInvoiceIssued moves a pending invoice to issued and records the
// e-invoice handle (empty for a manual issue). An invoice that is not
// pending answers ErrInvoiceAlreadyIssued; a missing one answers
// ErrInvoiceNotFound.
func (s *Store) MarkInvoiceIssued(ctx context.Context, id string, at time.Time, ref EInvoiceRef) (*model.Invoice, error) {
	res, err := s.DB.NewUpdate().Model((*model.Invoice)(nil)).
		Set("status = ?", model.InvoiceStatusIssued).
		Set("issued_at = ?", at.UTC()).
		Set("e_invoice_provider = ?", ref.Provider).
		Set("e_invoice_ref = ?", ref.Ref).
		Set("e_invoice_status = ?", ref.Status).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ? AND status = ?", id, model.InvoiceStatusPendingIssue).
		Exec(ctx)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, s.invoiceTransitionError(ctx, id, ErrInvoiceAlreadyIssued)
	}
	return s.GetInvoice(ctx, id)
}

// MarkInvoiceVoid voids an issued or pending invoice. The document
// itself stays: number, amounts and PDF path are kept so the voided
// invoice can still be audited. A void invoice is not voidable again.
func (s *Store) MarkInvoiceVoid(ctx context.Context, id string) (*model.Invoice, error) {
	res, err := s.DB.NewUpdate().Model((*model.Invoice)(nil)).
		Set("status = ?", model.InvoiceStatusVoid).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ? AND status IN (?)", id, bun.List([]string{
			model.InvoiceStatusIssued, model.InvoiceStatusPendingIssue,
		})).
		Exec(ctx)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, s.invoiceTransitionError(ctx, id, ErrInvoiceNotVoidable)
	}
	return s.GetInvoice(ctx, id)
}

// invoiceTransitionError tells "no such invoice" apart from "that
// transition does not apply": the guarded UPDATE matched no row either
// way, and only a re-read can say which answer the caller owes.
func (s *Store) invoiceTransitionError(ctx context.Context, id string, notApplicable error) error {
	if _, err := s.GetInvoice(ctx, id); err != nil {
		return err
	}
	return notApplicable
}

// SetInvoicePDFPath records where the rendered PDF was stored.
func (s *Store) SetInvoicePDFPath(ctx context.Context, id, path string) error {
	res, err := s.DB.NewUpdate().Model((*model.Invoice)(nil)).
		Set("pdf_path = ?", path).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInvoiceNotFound
	}
	return nil
}

// SetInvoiceEInvoice records an adapter's handle without changing the
// invoice's own status, for a provider whose own status is asynchronous.
func (s *Store) SetInvoiceEInvoice(ctx context.Context, id, provider, ref, status string) error {
	res, err := s.DB.NewUpdate().Model((*model.Invoice)(nil)).
		Set("e_invoice_provider = ?", provider).
		Set("e_invoice_ref = ?", ref).
		Set("e_invoice_status = ?", status).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInvoiceNotFound
	}
	return nil
}

// Invoice-related settings (spec §3.8, §3.11). They are read with the
// existing settings store; a missing row is the documented default, not
// an error. The keys live here because the invoice service and the PDF
// renderer both read them.
const (
	SettingInvoiceSellerName    = "invoice_seller_name"
	SettingInvoiceSellerTaxCode = "invoice_seller_tax_code"
	SettingInvoiceSellerAddress = "invoice_seller_address"
	SettingInvoiceSellerEmail   = "invoice_seller_email"
	SettingInvoiceNote          = "invoice_note"
	SettingEInvoiceProvider     = "einvoice_provider"
	// SettingAttributionURL overrides the branding URL the PDF footer and
	// web pages link to. The text itself stays branding.Tagline: it is a
	// licence obligation (AGPL v3 §7(b)), not a configurable string.
	SettingAttributionURL = "attribution_url"
	// SettingLanguage is the install language; "vi" switches the invoice
	// PDF labels to Vietnamese.
	SettingLanguage = "language"
)
