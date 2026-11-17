package model

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// PlanPrice is one plan's price with one payment provider. Phase 1 writes
// provider 'sepay' or 'payos'; external_price_id is reserved for a future
// Stripe adapter.
type PlanPrice struct {
	bun.BaseModel `bun:"table:plan_prices"`

	ID               string    `bun:",pk" json:"id"`
	PlanID           string    `bun:",notnull" json:"plan_id"`
	Provider         string    `bun:",notnull" json:"provider"`
	Currency         string    `bun:",notnull" json:"currency"`
	AmountMinor      int64     `bun:",notnull" json:"amount_minor"`
	CurrencyExponent int16     `bun:",notnull" json:"currency_exponent"`
	BillingInterval  string    `bun:",notnull,default:''" json:"billing_interval"`
	ExternalPriceID  string    `bun:",notnull,default:''" json:"external_price_id"`
	Active           bool      `bun:",notnull,default:true" json:"active"`
	CreatedAt        time.Time `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt        time.Time `bun:",nullzero,default:now()" json:"updated_at"`
}

// PaymentProviderConfig is one provider's credentials and options for a
// mode ('test' | 'live'), either install-wide (ProductID empty) or
// overriding one product. Secrets are sealed before they reach
// CredentialsEncrypted or WebhookSecretEncrypted and never leave the
// server.
type PaymentProviderConfig struct {
	bun.BaseModel `bun:"table:payment_provider_configs"`

	ID                     string          `bun:",pk" json:"id"`
	Provider               string          `bun:",notnull" json:"provider"`
	ProductID              string          `bun:",nullzero" json:"product_id,omitempty"`
	Mode                   string          `bun:",notnull" json:"mode"`
	Enabled                bool            `bun:",notnull" json:"enabled"`
	Config                 json.RawMessage `bun:",notnull,type:jsonb" json:"config"`
	CredentialsEncrypted   []byte          `bun:",notnull,nullzero" json:"-"`
	WebhookSecretEncrypted []byte          `bun:",notnull,nullzero" json:"-"`
	CreatedAt              time.Time       `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt              time.Time       `bun:",nullzero,default:now()" json:"updated_at"`
}

// PaymentOrder is one checkout: the priced amount, the provider that
// collects it, and the licence (or period) it buys. Numbering, state and
// expiry live here; provider instructions live in PaymentAttempt.
type PaymentOrder struct {
	bun.BaseModel `bun:"table:payment_orders"`

	ID            string `bun:",pk" json:"id"`
	NumericCode   int64  `bun:",nullzero" json:"numeric_code"`
	Code          string `bun:",notnull" json:"code"`
	ProductID     string `bun:",notnull" json:"product_id"`
	PlanID        string `bun:",notnull" json:"plan_id"`
	PriceID       string `bun:",nullzero" json:"price_id,omitempty"`
	LicenseID     string `bun:",nullzero" json:"license_id,omitempty"`
	Email         string `bun:",notnull" json:"email"`
	Kind          string `bun:",notnull" json:"kind"`
	Quantity      int    `bun:",notnull,default:1" json:"quantity"`
	SubtotalMinor int64  `bun:",notnull" json:"subtotal_minor"`
	DiscountMinor int64  `bun:",notnull" json:"discount_minor"`
	TaxMinor      int64  `bun:",notnull" json:"tax_minor"`
	TotalMinor    int64  `bun:",notnull" json:"total_minor"`
	// CreditMinor is the unused-time credit an immediate plan upgrade
	// applied to this order (0 for every other order). It is frozen here
	// so settlement, the invoice and an operator all read the number the
	// buyer was charged against.
	CreditMinor         int64           `bun:",notnull,default:0" json:"credit_minor"`
	Currency            string          `bun:",notnull" json:"currency"`
	CurrencyExp         int16           `bun:"currency_exponent,notnull" json:"currency_exponent"`
	TaxRateBP           int             `bun:",notnull" json:"tax_rate_bp"`
	Provider            string          `bun:",notnull" json:"provider"`
	Status              string          `bun:",notnull" json:"status"`
	ExpiresAt           time.Time       `bun:",notnull" json:"expires_at"`
	PaidAt              *time.Time      `json:"paid_at,omitempty"`
	PeriodStart         *time.Time      `json:"period_start,omitempty"`
	PeriodEnd           *time.Time      `json:"period_end,omitempty"`
	Buyer               json.RawMessage `bun:",notnull,type:jsonb" json:"buyer"`
	Metadata            json.RawMessage `bun:",notnull,type:jsonb" json:"metadata"`
	CreatedByKeyID      string          `bun:"created_by_api_key_id,nullzero" json:"created_by_api_key_id,omitempty"`
	LastSettlementError string          `bun:",notnull,default:''" json:"last_settlement_error"`
	CreatedAt           time.Time       `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt           time.Time       `bun:",nullzero,default:now()" json:"updated_at"`
}

// PaymentOrderDiscount is one coupon applied to an order, frozen at the
// amount it took off. CouponID is null when the coupon was deleted; the
// code and amount stay for the receipt and the audit trail.
type PaymentOrderDiscount struct {
	bun.BaseModel `bun:"table:payment_order_discounts"`

	ID          string    `bun:",pk" json:"id"`
	OrderID     string    `bun:",notnull" json:"order_id"`
	CouponID    string    `bun:",nullzero" json:"coupon_id,omitempty"`
	CouponCode  string    `bun:",notnull" json:"coupon_code"`
	Type        string    `bun:",notnull" json:"type"`
	Value       int64     `bun:",notnull" json:"value"`
	AmountMinor int64     `bun:",notnull" json:"amount_minor"`
	Priority    int       `bun:",notnull" json:"priority"`
	CreatedAt   time.Time `bun:",nullzero,default:now()" json:"created_at"`
}

// PaymentAttempt is one provider-side instruction (QR, hosted checkout,
// virtual account) for an order. Raw keeps the adapter's response so the
// pay page and the reconciler can render/read it without another call.
type PaymentAttempt struct {
	bun.BaseModel `bun:"table:payment_attempts"`

	ID          string          `bun:",pk" json:"id"`
	OrderID     string          `bun:",notnull" json:"order_id"`
	Provider    string          `bun:",notnull" json:"provider"`
	ProviderRef string          `bun:",notnull,default:''" json:"provider_ref"`
	Status      string          `bun:",notnull" json:"status"`
	AmountMinor int64           `bun:",notnull" json:"amount_minor"`
	Currency    string          `bun:",notnull" json:"currency"`
	QRPayload   string          `bun:",notnull,default:''" json:"qr_payload"`
	CheckoutURL string          `bun:",notnull,default:''" json:"checkout_url"`
	VANumber    string          `bun:",notnull,default:''" json:"va_number"`
	Raw         json.RawMessage `bun:",notnull,type:jsonb" json:"raw"`
	ExpiresAt   *time.Time      `json:"expires_at,omitempty"`
	CreatedAt   time.Time       `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt   time.Time       `bun:",nullzero,default:now()" json:"updated_at"`
}

// PaymentTransaction is a transfer seen at a provider, matched to an
// order or left for the unmatched queue. (Provider, ProviderRef) is the
// dedupe key, so redelivery of the same bank transfer is a no-op.
// Direction separates money in from refunds and payouts ('out'), which
// the reconciler stores but the order matcher does not consume; the
// unmatched queue reads Reference and Content, and a manual match
// records who matched it and when. AttemptID is a pointer for the same
// reason as OrderID: NULL means "no attempt/order", which no empty
// string can express. Raw is json.RawMessage, not []byte, so the payload
// is stored as JSON rather than base64.
type PaymentTransaction struct {
	bun.BaseModel `bun:"table:payment_transactions"`

	ID              string          `bun:",pk" json:"id"`
	Provider        string          `bun:",notnull" json:"provider"`
	ProviderRef     string          `bun:",notnull" json:"provider_ref"`
	OrderID         *string         `bun:",nullzero" json:"order_id,omitempty"`
	AttemptID       *string         `bun:",nullzero" json:"attempt_id,omitempty"`
	AmountMinor     int64           `bun:",notnull" json:"amount_minor"`
	Currency        string          `bun:",notnull" json:"currency"`
	Direction       string          `bun:",notnull,default:'in'" json:"direction"`
	ProviderAccount string          `bun:",notnull,default:''" json:"provider_account"`
	Reference       string          `bun:",notnull,default:''" json:"reference"`
	Content         string          `bun:",notnull,default:''" json:"content"`
	Status          string          `bun:",notnull" json:"status"`
	IgnoreReason    string          `bun:",notnull,default:''" json:"ignore_reason"`
	MatchedBy       *string         `bun:",nullzero" json:"matched_by,omitempty"`
	MatchedAt       *time.Time      `json:"matched_at,omitempty"`
	Raw             json.RawMessage `bun:",notnull,type:jsonb" json:"raw"`
	OccurredAt      time.Time       `bun:",notnull" json:"occurred_at"`
	CreatedAt       time.Time       `bun:",nullzero,default:now()" json:"created_at"`
}

// PaymentEvent is the raw provider payload behind a state change, kept
// once per (Provider, EventID). processed_events remains the claim/done
// idempotency mechanism; this is the audit trail. Raw is json.RawMessage
// so the payload is stored as JSON rather than base64.
type PaymentEvent struct {
	bun.BaseModel `bun:"table:payment_events"`

	ID         string          `bun:",pk" json:"id"`
	Provider   string          `bun:",notnull" json:"provider"`
	EventID    string          `bun:",notnull" json:"event_id"`
	OrderID    string          `bun:",nullzero" json:"order_id,omitempty"`
	Kind       string          `bun:",notnull" json:"kind"`
	Raw        json.RawMessage `bun:",notnull,type:jsonb" json:"raw"`
	OccurredAt time.Time       `json:"occurred_at"`
	CreatedAt  time.Time       `bun:",nullzero,default:now()" json:"created_at"`
}

// PaymentRefund records money returned against an order. Method says how
// it was returned ('manual' for a bank transfer done by an operator).
type PaymentRefund struct {
	bun.BaseModel `bun:"table:payment_refunds"`

	ID          string    `bun:",pk" json:"id"`
	OrderID     string    `bun:",notnull" json:"order_id"`
	AmountMinor int64     `bun:",notnull" json:"amount_minor"`
	Currency    string    `bun:",notnull" json:"currency"`
	Reason      string    `bun:",notnull,default:''" json:"reason"`
	Method      string    `bun:",notnull" json:"method"`
	ProviderRef string    `bun:",notnull,default:''" json:"provider_ref"`
	Status      string    `bun:",notnull" json:"status"`
	CreatedBy   string    `bun:",nullzero" json:"created_by,omitempty"`
	CreatedAt   time.Time `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt   time.Time `bun:",nullzero,default:now()" json:"updated_at"`
}

// PaymentSyncCursor is the reconciliation high-water mark per provider
// and scope.
type PaymentSyncCursor struct {
	bun.BaseModel `bun:"table:payment_sync_cursors"`

	ID                string     `bun:",pk" json:"id"`
	Provider          string     `bun:",notnull" json:"provider"`
	Scope             string     `bun:",notnull,default:'default'" json:"scope"`
	LastTransactionID string     `bun:",notnull,default:''" json:"last_transaction_id"`
	LastSyncedAt      *time.Time `json:"last_synced_at,omitempty"`
	UpdatedAt         time.Time  `bun:",nullzero,default:now()" json:"updated_at"`
}

// Coupon is a discount or trial definition. Redemption logic is Plan 1b;
// the table exists in phase 1a so order discounts can reference it.
type Coupon struct {
	bun.BaseModel `bun:"table:coupons"`

	ID               string     `bun:",pk" json:"id"`
	Code             string     `bun:",notnull,unique" json:"code"`
	Description      string     `bun:",notnull,default:''" json:"description"`
	Type             string     `bun:",notnull" json:"type"`
	Value            int64      `bun:",notnull,default:0" json:"value"`
	TrialDays        int        `bun:",notnull,default:0" json:"trial_days"`
	Currency         string     `bun:",nullzero" json:"currency,omitempty"`
	StackGroup       string     `bun:",notnull,default:'general'" json:"stack_group"`
	Stackable        bool       `bun:",notnull,default:true" json:"stackable"`
	Priority         int        `bun:",notnull,default:100" json:"priority"`
	AssignedEmail    string     `bun:",notnull,default:''" json:"assigned_email"`
	MaxRedemptions   int        `bun:",notnull,default:0" json:"max_redemptions"`
	RedeemedCount    int        `bun:",notnull,default:0" json:"redeemed_count"`
	PerCustomerLimit int        `bun:",notnull,default:0" json:"per_customer_limit"`
	ValidFrom        *time.Time `json:"valid_from,omitempty"`
	ValidTo          *time.Time `json:"valid_to,omitempty"`
	Active           bool       `bun:",notnull,default:true" json:"active"`
	CreatedAt        time.Time  `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt        time.Time  `bun:",nullzero,default:now()" json:"updated_at"`
}

// CouponScope narrows a coupon to one product or plan. No rows means the
// coupon covers everything.
type CouponScope struct {
	bun.BaseModel `bun:"table:coupon_scopes"`

	CouponID  string `bun:",pk" json:"coupon_id"`
	ScopeType string `bun:",pk" json:"scope_type"`
	ScopeID   string `bun:",pk" json:"scope_id"`
}

// CouponRedemption is one order that used a coupon. The FK is ON DELETE
// CASCADE: a redemption row is removed with its order, so per-customer
// counts only cover orders that still exist.
type CouponRedemption struct {
	bun.BaseModel `bun:"table:coupon_redemptions"`

	ID                 string    `bun:",pk" json:"id"`
	CouponID           string    `bun:",notnull" json:"coupon_id"`
	CouponCode         string    `bun:",notnull" json:"coupon_code"`
	OrderID            string    `bun:",notnull" json:"order_id"`
	Email              string    `bun:",notnull,default:''" json:"email"`
	ExternalCustomerID string    `bun:",notnull,default:''" json:"external_customer_id"`
	Type               string    `bun:",notnull" json:"type"`
	AmountMinor        int64     `bun:",notnull,default:0" json:"amount_minor"`
	TrialDays          int       `bun:",notnull,default:0" json:"trial_days"`
	CreatedAt          time.Time `bun:",nullzero,default:now()" json:"created_at"`
}
