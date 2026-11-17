package model

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// Invoice statuses (spec §4.8). A paid order gets a row immediately; the
// row is 'issued' once it can be handed to the buyer, 'pending_issue'
// while an e-invoice provider is configured but not yet wired and an
// operator must issue it by hand, and 'void' when it was cancelled.
const (
	InvoiceStatusDraft        = "draft"
	InvoiceStatusIssued       = "issued"
	InvoiceStatusPendingIssue = "pending_issue"
	InvoiceStatusVoid         = "void"
)

// InvoiceItem is one frozen line on an invoice. The order's plan is
// snapshotted at payment time so a later plan rename or re-price cannot
// rewrite history.
type InvoiceItem struct {
	Description     string `json:"description"`
	PlanID          string `json:"plan_id,omitempty"`
	Quantity        int    `json:"quantity"`
	UnitAmountMinor int64  `json:"unit_amount_minor"`
	AmountMinor     int64  `json:"amount_minor"`
	TaxRateBP       int    `json:"tax_rate_bp"`
}

// Invoice is the immutable record of what a paid order billed (spec
// §4.8). Buyer and items are JSON snapshots: invoices are audited
// documents, not a second live view of plans and orders.
type Invoice struct {
	bun.BaseModel `bun:"table:invoices"`

	ID                string          `bun:",pk" json:"id"`
	Number            string          `bun:",notnull" json:"number"`
	OrderID           string          `bun:",notnull" json:"order_id"`
	LicenseID         string          `bun:",nullzero" json:"license_id,omitempty"`
	Buyer             json.RawMessage `bun:",notnull,type:jsonb" json:"buyer"`
	Items             []InvoiceItem   `bun:",notnull,type:jsonb" json:"items"`
	SubtotalMinor     int64           `bun:",notnull" json:"subtotal_minor"`
	DiscountMinor     int64           `bun:",notnull,default:0" json:"discount_minor"`
	TaxMinor          int64           `bun:",notnull,default:0" json:"tax_minor"`
	TotalMinor        int64           `bun:",notnull" json:"total_minor"`
	Currency          string          `bun:",notnull" json:"currency"`
	CurrencyExp       int16           `bun:"currency_exponent,notnull" json:"currency_exponent"`
	Status            string          `bun:",notnull" json:"status"`
	PDFPath           string          `bun:",notnull,default:''" json:"pdf_path"`
	EInvoiceProvider  string          `bun:",notnull,default:''" json:"e_invoice_provider"`
	EInvoiceRef       string          `bun:",notnull,default:''" json:"e_invoice_ref"`
	EInvoiceStatus    string          `bun:",notnull,default:''" json:"e_invoice_status"`
	IssuedAt          *time.Time      `json:"issued_at,omitempty"`
	CreatedAt         time.Time       `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt         time.Time       `bun:",nullzero,default:now()" json:"updated_at"`
}

// InvoiceCounter is the per-year numbering row. The invoice writer locks
// the year row with UPDATE ... RETURNING so two concurrent payments can
// never mint the same number.
type InvoiceCounter struct {
	bun.BaseModel `bun:"table:invoice_counters"`

	Year    int   `bun:",pk" json:"year"`
	LastSeq int64 `bun:",notnull,default:0" json:"last_seq"`
}
