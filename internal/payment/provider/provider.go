// Package provider defines the contract each payment rail adapter
// implements, plus the request and result types the checkout and
// settlement services pass across it.
package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Buyer is the payer's details as the provider needs them on an invoice
// or receipt.
type Buyer struct {
	Name    string `json:"name,omitempty"`
	Email   string `json:"email,omitempty"`
	Phone   string `json:"phone,omitempty"`
	TaxID   string `json:"tax_id,omitempty"`
	Address string `json:"address,omitempty"`
}

// BankAccount is the destination account a customer transfers to.
type BankAccount struct {
	BankName    string `json:"bank_name"`
	BankBIN     string `json:"bank_bin,omitempty"`
	AccountNo   string `json:"account_no"`
	AccountName string `json:"account_name"`
}

// CreateAttemptRequest is everything an adapter needs to produce one
// payment instruction. Money is already resolved by the checkout
// service; the adapter never computes totals.
type CreateAttemptRequest struct {
	OrderCode   string
	NumericCode int64
	AmountMinor int64
	Currency    string
	CurrencyExp int16
	Description string
	Buyer       Buyer
	ReturnURL   string
	CancelURL   string
	ExpiresAt   time.Time
	// Config is the non-secret provider config (bank account, VA prefix,
	// PayOS client id). Credentials are decrypted by the caller and
	// passed separately so an adapter cannot log the secret config.
	Config      map[string]any
	Credentials []byte
	// WebhookSecret is only needed by adapters that sign outbound
	// requests; SePay/PayOS in this phase do not.
	WebhookSecret []byte
}

// CreateAttemptResult is the payment instruction returned to the
// customer, plus the raw provider payload for debugging.
type CreateAttemptResult struct {
	ProviderRef string
	QRPayload   string
	QRImageURL  string
	CheckoutURL string
	Bank        BankAccount
	VANumber    string
	Description string
	ExpiresAt   time.Time
	Raw         json.RawMessage
}

// WebhookEvent is a provider notification normalised into the fields
// the settlement service matches on.
type WebhookEvent struct {
	Provider        string
	ProviderEventID string
	ProviderRef     string
	OrderCode       string
	NumericCode     int64
	AmountMinor     int64
	Currency        string
	Status          string // domain.OrderPaid or "refunded"/"failed"/"pending"
	OccurredAt      time.Time
	// TransferType is "in" (customer paid) or "out" (refund/payout);
	// SePay sends both to one webhook.
	TransferType string
	Raw          json.RawMessage
}

// Provider is one payment rail. Implementations are stateless apart from
// configuration already resolved into the request.
type Provider interface {
	Name() string
	CreateAttempt(ctx context.Context, req CreateAttemptRequest) (CreateAttemptResult, error)
	CancelAttempt(ctx context.Context, ref string) error
	// ParseWebhook verifies the provider's signature over the raw body.
	// It must use only the raw bytes: Gin's JSON binding would change
	// them and break the HMAC.
	ParseWebhook(r *http.Request, rawBody []byte, secret []byte) (WebhookEvent, error)
	// TestConnection checks credentials against the provider without
	// creating money movement.
	TestConnection(ctx context.Context) error
}
