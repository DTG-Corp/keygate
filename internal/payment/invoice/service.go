// Package invoice turns a paid payment order into the immutable invoice
// record the spec requires (design §3.8, §4.8) and renders it as a PDF.
//
// The service is deliberately the only writer of invoices: settlement
// calls CreateForOrder exactly once per paid order (the PaidHook), the
// admin console calls Issue/Void for the manual e-invoice seam, and the
// consumer/admin APIs only read. Money is never recomputed here — the
// invoice copies the order's amounts so an invoice can never disagree
// with what the buyer was charged.
package invoice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/storage"
	"github.com/tabloy/keygate/internal/store"
)

// EventEmitter is the outbound webhook seam. *service.WebhookService
// satisfies it, and a test can capture what was emitted without a
// database full of webhook subscriptions.
type EventEmitter interface {
	Dispatch(ctx context.Context, productID, event string, data map[string]any)
}

// EInvoiceProvider is the vendor adapter seam of spec §3.8. Phase 1b
// ships the interface and the manual queue only; no vendor is wired, so
// Service.EInvoice stays nil and an operator issues pending invoices by
// hand. A future adapter implements this without touching the handlers.
type EInvoiceProvider interface {
	Issue(ctx context.Context, inv *model.Invoice) (EInvoiceRef, error)
	Void(ctx context.Context, ref EInvoiceRef) error
	Status(ctx context.Context, ref EInvoiceRef) (string, error)
}

// EInvoiceRef is one adapter's handle for an issued invoice.
type EInvoiceRef struct {
	Provider string
	Ref      string
	Status   string
}

// Deps is everything the service needs. Only Store is required.
type Deps struct {
	Store   *store.Store
	Storage storage.Storage
	Logger  *slog.Logger
	// Now is the clock; nil means time.Now. Tests use it to pin the
	// invoice-number year and issued_at.
	Now func() time.Time
	// Events is optional: a server without a webhook service still
	// invoices, it just cannot tell subscribers.
	Events EventEmitter
	// EInvoice is optional: nil is the manual seam (vendor deferred).
	EInvoice EInvoiceProvider
}

// Service is the invoice seam: creation from a paid order, reads for the
// admin and consumer APIs, the manual issue/void queue and PDF output.
type Service struct {
	Store    *store.Store
	Storage  storage.Storage
	Logger   *slog.Logger
	Now      func() time.Time
	Events   EventEmitter
	EInvoice EInvoiceProvider
}

// New builds the service with the contract's constructor shape. It is
// what cmd/server wires; NewWithDeps exists for tests and future knobs.
func New(st *store.Store, stor storage.Storage, logger *slog.Logger) *Service {
	return NewWithDeps(Deps{Store: st, Storage: stor, Logger: logger})
}

// NewWithDeps builds a service, filling the optional seams with the
// no-op implementations a self-hosted install runs with.
func NewWithDeps(d Deps) *Service {
	s := &Service{
		Store: d.Store, Storage: d.Storage, Logger: d.Logger,
		Now: d.Now, Events: d.Events, EInvoice: d.EInvoice,
	}
	if s.Storage == nil {
		s.Storage = storage.Disabled{}
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	return s
}

// zone is the calendar the invoice-number year uses (Asia/Ho_Chi_Minh),
// matching the provider adapters' fixed offset.
var zone = time.FixedZone("ICT", 7*3600)

// Buyer is the invoice's frozen buyer block. It mirrors the checkout
// buyer object of spec §6.2 while tolerating the phase-1a field names
// (tax_id for tax_code, company for company_name), because an order
// written before the field rename must still produce a correct invoice.
type Buyer struct {
	Name           string `json:"name,omitempty"`
	CompanyName    string `json:"company_name,omitempty"`
	TaxCode        string `json:"tax_code,omitempty"`
	Address        string `json:"address,omitempty"`
	Email          string `json:"email,omitempty"`
	Phone          string `json:"phone,omitempty"`
	RequestInvoice bool   `json:"request_invoice,omitempty"`
}

// buyerInput decodes both naming generations of the order's buyer blob.
type buyerInput struct {
	Name           string `json:"name"`
	CompanyName    string `json:"company_name"`
	Company        string `json:"company"`
	TaxCode        string `json:"tax_code"`
	TaxID          string `json:"tax_id"`
	Address        string `json:"address"`
	Email          string `json:"email"`
	Phone          string `json:"phone"`
	RequestInvoice bool   `json:"request_invoice"`
}

// BuyerOf decodes an invoice buyer snapshot (or the order's buyer blob)
// into the typed block. A malformed blob yields a zero Buyer rather than
// an error: the invoice is a financial document and must not fail to be
// read because a legacy order stored something odd.
func BuyerOf(raw json.RawMessage) Buyer {
	var in buyerInput
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &in)
	}
	company := in.CompanyName
	if company == "" {
		company = in.Company
	}
	taxCode := in.TaxCode
	if taxCode == "" {
		taxCode = in.TaxID
	}
	return Buyer{
		Name: in.Name, CompanyName: company, TaxCode: taxCode, Address: in.Address,
		Email: in.Email, Phone: in.Phone, RequestInvoice: in.RequestInvoice,
	}
}

// ListFilter is the service-level invoice list query; it mirrors
// store.InvoiceFilter without leaking the store to the handlers.
type ListFilter struct {
	Status, LicenseID, OrderID, Number, ProductID string
	From, To                                      *time.Time
	Page                                          store.Page
}

// CreateForOrder writes the invoice for one paid order. It is
// idempotent by order: settlement may call it on a redelivery or after a
// crash retry, and the unique index on invoices.order_id makes a second
// row impossible. The returned invoice is the existing one in that case.
//
// The invoice opens as 'issued' with issued_at=now unless an e-invoice
// provider is configured, in which case it waits in 'pending_issue' for
// the operator queue (spec §3.8: vendor integration deferred).
func (s *Service) CreateForOrder(ctx context.Context, order *model.PaymentOrder) (*model.Invoice, error) {
	if order == nil {
		return nil, errors.New("invoice: nil order")
	}
	if existing, err := s.Store.GetInvoiceByOrderID(ctx, order.ID); err == nil {
		return existing, nil
	} else if !errors.Is(err, store.ErrInvoiceNotFound) {
		return nil, err
	}

	provider := s.setting(ctx, store.SettingEInvoiceProvider, "")
	now := s.Now().UTC()
	status := model.InvoiceStatusIssued
	var issuedAt *time.Time
	if provider != "" {
		status = model.InvoiceStatusPendingIssue
	} else {
		issuedAt = &now
	}
	inv, created, err := s.Store.CreateInvoice(ctx, store.CreateInvoiceInput{
		OrderID: order.ID, LicenseID: order.LicenseID,
		Buyer: s.buyerSnapshot(order),
		Items: s.itemsSnapshot(ctx, order),
		// Copied verbatim: the invoice is a snapshot of what was charged,
		// never a re-derivation (a later plan price or VAT change must not
		// rewrite history).
		SubtotalMinor: order.SubtotalMinor, DiscountMinor: order.DiscountMinor,
		TaxMinor: order.TaxMinor, TotalMinor: order.TotalMinor,
		Currency: order.Currency, CurrencyExponent: order.CurrencyExp,
		Status: status, EInvoiceProvider: provider, IssuedAt: issuedAt,
		NumberYear: now.In(zone).Year(),
	})
	if err != nil {
		return nil, err
	}
	if created && inv.Status == model.InvoiceStatusIssued {
		s.emitIssued(ctx, order.ProductID, inv)
	}
	return inv, nil
}

// AfterPaid creates the invoice for a settled order. It is the seam the
// settlement service calls through settlement.PaidHook, so main wires
// one line — settleSvc.PaidHook = invoiceSvc — and every paid order gets
// its document without the settlement package knowing invoices exist.
//
// PaidHook has no error return: a failure here must not undo a payment.
// The error is logged and the order stays invoiceable — CreateForOrder
// is idempotent, so a retry (or the admin's manual create, once the
// module that offers one lands) closes the gap.
func (s *Service) AfterPaid(ctx context.Context, order *model.PaymentOrder, _ *model.License, _ string) {
	if s == nil || order == nil {
		return
	}
	if _, err := s.CreateForOrder(ctx, order); err != nil {
		logger := s.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Error("invoice: create for paid order", "order_id", order.ID, "error", err)
	}
}

// buyerSnapshot freezes the order's buyer block, filling the email from
// the order when the buyer object omitted it.
func (s *Service) buyerSnapshot(order *model.PaymentOrder) []byte {
	buyer := BuyerOf(order.Buyer)
	if buyer.Email == "" {
		buyer.Email = order.Email
	}
	out, err := json.Marshal(buyer)
	if err != nil {
		return []byte(`{}`)
	}
	return out
}

// itemsSnapshot freezes the invoice's line items. A phase-1b order
// carries one plan and a quantity, not a line-item array, so the
// snapshot is one line: plan name, quantity, unit amount and the VAT
// rate that was applied. The line's amount is the order subtotal, so the
// invoice always adds up even when the unit price cannot divide evenly.
func (s *Service) itemsSnapshot(ctx context.Context, order *model.PaymentOrder) []model.InvoiceItem {
	return itemsFor(order, s.planName(ctx, order.PlanID))
}

// planName resolves the plan name for the invoice line, falling back to
// the plan id. A read failure is logged and never blocks the invoice: a
// paid order must still produce a document.
func (s *Service) planName(ctx context.Context, planID string) string {
	plan, err := s.Store.FindPlanByID(ctx, planID)
	if err != nil || plan == nil || strings.TrimSpace(plan.Name) == "" {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.Logger.Warn("invoice: plan lookup failed", "plan_id", planID, "error", err)
		}
		return planID
	}
	return plan.Name
}

// itemsFor builds the one-line snapshot; exported to tests through the
// package's internal test file.
func itemsFor(order *model.PaymentOrder, description string) []model.InvoiceItem {
	if strings.TrimSpace(description) == "" {
		// A blank line would make the document unreadable; the plan id is
		// still a truthful identifier even when its name cannot be read.
		description = order.PlanID
	}
	quantity := order.Quantity
	if quantity < 1 {
		quantity = 1
	}
	unit := order.SubtotalMinor / int64(quantity)
	if order.SubtotalMinor%int64(quantity) != 0 {
		unit = (order.SubtotalMinor + int64(quantity)/2) / int64(quantity)
	}
	return []model.InvoiceItem{{
		Description: description, PlanID: order.PlanID, Quantity: quantity,
		UnitAmountMinor: unit, AmountMinor: order.SubtotalMinor, TaxRateBP: order.TaxRateBP,
	}}
}

// Get reads one invoice.
func (s *Service) Get(ctx context.Context, id string) (*model.Invoice, error) {
	return s.Store.GetInvoice(ctx, id)
}

// List returns one page of invoices and the total the filter matched.
func (s *Service) List(ctx context.Context, f ListFilter) ([]*model.Invoice, int, error) {
	return s.Store.ListInvoices(ctx, store.InvoiceFilter{
		Status: f.Status, LicenseID: f.LicenseID, OrderID: f.OrderID, Number: f.Number,
		ProductID: f.ProductID, From: f.From, To: f.To, Page: f.Page,
	})
}

// setting reads one install setting, answering the documented default
// when the row does not exist. A read failure is logged once and does
// not fail the invoice: a missing branding value must not stop a paid
// order from being documented.
func (s *Service) setting(ctx context.Context, key, fallback string) string {
	v, err := s.Store.GetSetting(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return fallback
	}
	if err != nil {
		s.Logger.Warn("invoice: setting read failed", "key", key, "error", err)
		return fallback
	}
	return strings.TrimSpace(v)
}

// emitIssued publishes the invoice.issued event of spec §8. It is
// best-effort: the invoice is already committed, and a webhook failure
// must not turn a paid order into a provider retry.
func (s *Service) emitIssued(ctx context.Context, productID string, inv *model.Invoice) {
	if s.Events == nil {
		return
	}
	s.Events.Dispatch(ctx, productID, model.EventInvoiceIssued, map[string]any{
		"invoice": map[string]any{
			"id": inv.ID, "number": inv.Number,
			"total": inv.TotalMinor, "currency": inv.Currency,
		},
	})
}

// emitVoided publishes invoice.voided: the document identity only, as
// spec §8 defines it.
func (s *Service) emitVoided(ctx context.Context, productID string, inv *model.Invoice) {
	if s.Events == nil {
		return
	}
	s.Events.Dispatch(ctx, productID, model.EventInvoiceVoided, map[string]any{
		"invoice": map[string]any{"id": inv.ID, "number": inv.Number},
	})
}
