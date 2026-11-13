# SePay + PayOS — Phase 1a (core rail) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a Vietnamese customer buy a Keygate licence with a SePay VietQR transfer or a PayOS payment link, and have Keygate create/extend the licence reliably from provider webhooks plus a reconciler.

**Architecture:** A new provider-neutral payment domain under `internal/payment/` (order/attempt/transaction/event records + a `Provider` interface) runs beside the untouched Stripe code. SePay and PayOS adapters create payment instructions and parse verified webhooks; one settlement service is the only writer that turns a paid order into a licence. A reconciler covers lost webhooks. Phase 1a ships no invoices, no billing engine, no coupons logic and no Stripe API calls — those are Plan 1b/2.

**Tech Stack:** Go 1.27, Gin 1.12, Bun ORM + PostgreSQL, `stripe-go/v82` (untouched), React 19 + Vite + TanStack Query for the two new screens.

**Spec:** [docs/superpowers/specs/2026-11-12-sepay-payos-integration-design.md](../specs/2026-11-12-sepay-payos-integration-design.md)

**Phase note:** the spec has two delivery phases. This plan is **phase 1a only** (spec §11). Phase 1b (billing engine, invoices/e-invoice seam, portal, coupons logic incl. trial/stacking, reports, remaining events) gets its own plan after 1a lands, because it depends on this plan's order/settlement interfaces.

## Global Constraints

- Module path is `github.com/tabloy/keygate`; Go 1.27; Gin 1.12; Bun + PostgreSQL.
- Every ID is a TEXT UUID string (`store.NewID()`); never BIGSERIAL. Every FK is TEXT.
- All API responses use `pkg/response` envelope `{"success":true,"data":…}` / `{"success":false,"error":{code,message,details}}`; JSON is snake_case.
- Money = integer minor units + `currency` + `currency_exponent` (VND 0, USD 2). Never floats.
- Pagination: default 50, max 200 (`internal/handler/pagination.go`).
- `Idempotency-Key` header is required on `POST /api/v1/payments/orders`; replay window 24 h; stored with the existing idempotency store pattern.
- Webhook routes are raw-body exceptions: they must not use the response envelope for ack errors; verified duplicate events ack `200 {"success":true}`.
- Migrations are additive only (`db/migrations/YYYYMMDD_name.up.sql` + `.down.sql`); never edit an applied migration.
- Phase 1a must not call the Stripe API, read Stripe prices or touch `internal/payment/stripe.go`.
- Every new API error code is used with exactly one HTTP status (`go test ./pkg/response -run TestErrorCodeMapsToExactlyOneStatus`).
- AGPLv3 §7(b): "Powered by Keygate" / `attribution_text` / `attribution_url` / `X-Powered-By` stay visible on every new page.
- `go test -p 1 ./...` must stay green; integration tests need `TEST_DATABASE_URL` and are skipped without it (existing `testsupport/testdb` behaviour).

## Review Focus

Inputs/conditions the spec implies but no single happy-path test covers. Each line has a test in the task named here.

1. **Duplicate/expired webhook for an order that already expired or was canceled** — a verified late payment must be recorded as an unmatched/attention transaction and must never grant a licence; the endpoint still acks so the provider stops retrying (T8, T9).
2. **Amount/currency mismatch** — a transfer with `KG…` in the memo but the wrong amount, or a PayOS webhook for a different amount, must mark the transaction `unmatched` and leave the order `pending`; no licence, no `order.paid` (T7, T9).
3. **Memo matching ambiguity** — the transfer memo contains the code inside unrelated text (`"chuyen tien KG7F3K2Q ca phe"`) and another pending order's code appears elsewhere; matching must prefer the exact order code token over amount+time heuristics (T11).
4. **Provider resolution with per-product override** — a product-configured PayOS row must win over the install default for that product, and a disabled provider must return `PAYMENT_PROVIDER_DISABLED` instead of silently falling back (T4, T10).
5. **Idempotent order creation** — same `Idempotency-Key` + same body returns the same order code; same key + different body returns `409`; a concurrent double-submit must not create two orders or two attempts (T10, T11).

---

## File Structure

New files (create) and touched files (modify) for this plan. Grouped by responsibility so each task owns a coherent slice.

```
db/migrations/
  20261112_01_plan_prices.{up,down}.sql              T1
  20261112_04_payment_provider_configs.{up,down}.sql T1
  20261112_03_payment_core.{up,down}.sql             T1 (orders, discounts, attempts, transactions,
                                                     events, refunds, sync cursors)
  20261112_02_coupons.{up,down}.sql                  T1 (tables only; logic in Plan 1b)
  20261112_05_existing_table_hooks.{up,down}.sql     T1 (products.tax_rate_bp, license_renewals/upgrades)
internal/model/payment.go                         T1
internal/model/model.go                           T1 (alter Product, LicenseRenewal, LicenseUpgrade,
                                                     WebhookEvents, scopes); T8 (event constants)
internal/payment/domain/money.go                  T2
internal/payment/domain/codes.go                  T2
internal/payment/domain/status.go                 T2
internal/payment/provider/provider.go             T2
internal/payment/provider/fake/fake.go            T2
internal/store/plan_prices.go                     T3
internal/store/provider_configs.go                T4
internal/payment/providercfg/codec.go             T4
internal/payment/providercfg/service.go           T4
internal/store/payments.go                        T5 (orders, attempts); T9/T11 (transactions,
                                                     renewals lookup, cursors, refunds)
internal/store/payment_events.go                  T8
internal/payment/providers/sepay/*                T6
internal/payment/providers/payos/*                T7
internal/payment/settlement/service.go            T9
internal/payment/settlement/build.go              T9
internal/payment/checkout/service.go              T10
internal/payment/reconcile/matcher.go             T11
internal/payment/reconcile/worker.go              T11
internal/handler/plan_prices_admin.go             T3
internal/handler/payment_providers_admin.go       T4
internal/handler/payments_webhook.go              T8
internal/handler/payments_public.go               T10
internal/handler/payments_api.go                  T10
internal/handler/payments_admin.go                T11
cmd/server/main.go                                T3/T4/T8/T10/T11 (constructors + routes)
web/src/lib/money.ts                              T12
web/src/lib/api.ts                                T12 (checkoutPay + adminPayments)
web/src/pages/checkout-pay.tsx                    T12
web/src/pages/admin/payment-providers.tsx         T12
web/src/pages/admin/payment-orders.tsx            T12
web/src/pages/admin/payment-unmatched.tsx         T12
web/src/pages/admin/plan-prices.tsx               T12
web/src/i18n/index.tsx                            T12 (vi locale)
web/src/main.tsx (or router file)                 T12 (routes)
docs/openapi.yaml, docs/api-contract.md           T13
docs/runbooks/payments-sepay-payos.md             T13
```

Do not create files that duplicate existing responsibilities: use `response.OK/Err/Created`, `store.RunInTx`, `testsupport/testdb`, `pkg/apperr`, and `service.WebhookService.Dispatch`.

---

### Task 1: Schema foundation and models

**Files:**
- Create: `db/migrations/20261112_01_plan_prices.up.sql`, `.down.sql`
- Create: `db/migrations/20261112_04_payment_provider_configs.up.sql`, `.down.sql`
- Create: `db/migrations/20261112_03_payment_core.up.sql`, `.down.sql`
- Create: `db/migrations/20261112_02_coupons.up.sql`, `.down.sql`
- Create: `db/migrations/20261112_05_existing_table_hooks.up.sql`, `.down.sql`

  The numeric infix is required: `RunMigrations` runs `os.ReadDir` order, so the
  plain names would run `coupons`, `payment_core` and `plan_prices` alphabetically —
  after the tables they reference. These five names run in dependency order
  (`plan_prices` → `coupons` → `payment_core` → `payment_provider_configs` →
  `existing_table_hooks`).
- Create: `internal/model/payment.go`
- Modify: `internal/model/model.go` (add `TaxRateBP`/`PriceIncludesTax` to `Product`; `PaymentOrderID` to `LicenseRenewal`/`LicenseUpgrade`)
- Test: `internal/store/payment_schema_test.go`

**Interfaces:**
- Consumes: nothing (first task).
- Produces: Bun models `model.PlanPrice`, `model.PaymentProviderConfig`, `model.PaymentOrder` (+ `TableName`), `model.PaymentOrderDiscount`, `model.PaymentAttempt`, `model.PaymentTransaction`, `model.PaymentEvent`, `model.PaymentRefund`, `model.PaymentSyncCursor`, `model.Coupon`, `model.CouponScope`, `model.CouponRedemption`; new `Product` columns; `model.AllScopes` not yet changed (T11).

- [ ] **Step 1: Write the failing migration test**

```go
// internal/store/payment_schema_test.go
package store_test

import (
	"context"
	"testing"

	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

func TestPaymentSchemaTablesExist(t *testing.T) {
	s := testdb.Store(t) // helper added in this step; opens TEST_DATABASE_URL
	ctx := context.Background()
	for _, table := range []string{
		"plan_prices", "payment_provider_configs", "payment_orders",
		"payment_order_discounts", "payment_attempts", "payment_transactions",
		"payment_events", "payment_refunds", "payment_sync_cursors",
		"coupons", "coupon_scopes", "coupon_redemptions",
	} {
		var n int
		err := s.DB.NewRaw(
			"SELECT count(*) FROM information_schema.tables WHERE table_name = ?", table,
		).Scan(ctx, &n)
		if err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("table %s missing", table)
		}
	}
}
```

Add the shared helper (also used by later tasks) in `internal/testsupport/testdb/testdb.go`:

```go
// Store opens the integration database or skips the test when
// TEST_DATABASE_URL is unset, matching the repo's integration-test style.
func Store(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	s, err := store.New(dsn)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store -run TestPaymentSchemaTablesExist -v`
Expected: FAIL with "table plan_prices missing".

- [ ] **Step 3: Write the migrations**

`db/migrations/20261112_01_plan_prices.up.sql` (copy spec §4.1 exactly):

```sql
CREATE TABLE IF NOT EXISTS plan_prices (
    id                TEXT PRIMARY KEY,
    plan_id           TEXT NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
    provider          TEXT NOT NULL,
    currency          CHAR(3) NOT NULL,
    amount_minor      BIGINT NOT NULL CHECK (amount_minor >= 0),
    currency_exponent SMALLINT NOT NULL DEFAULT 2,
    billing_interval  TEXT NOT NULL DEFAULT '',
    external_price_id TEXT NOT NULL DEFAULT '',
    active            BOOLEAN NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS plan_prices_unique_active
    ON plan_prices (plan_id, provider, currency, billing_interval) WHERE active;
```

`20261112_01_plan_prices.down.sql`:

```sql
DROP TABLE IF EXISTS plan_prices;
```

`20261112_04_payment_provider_configs.up.sql` (spec §4.2):

```sql
CREATE TABLE IF NOT EXISTS payment_provider_configs (
    id                     TEXT PRIMARY KEY,
    provider               TEXT NOT NULL CHECK (provider IN ('sepay','payos')),
    product_id             TEXT NULL REFERENCES products(id) ON DELETE CASCADE,
    mode                   TEXT NOT NULL CHECK (mode IN ('test','live')),
    enabled                BOOLEAN NOT NULL DEFAULT TRUE,
    config                 JSONB NOT NULL DEFAULT '{}'::jsonb,
    credentials_encrypted  BYTEA NOT NULL DEFAULT ''::bytea,
    webhook_secret_encrypted BYTEA NOT NULL DEFAULT ''::bytea,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS payment_provider_configs_unique
    ON payment_provider_configs (provider, product_id, mode) NULLS NOT DISTINCT;
```

`20261112_04_payment_provider_configs.down.sql`:

```sql
DROP TABLE IF EXISTS payment_provider_configs;
```

`20261112_03_payment_core.up.sql` — copy spec §4.3–4.8 and §4.10: `payment_orders`, `payment_order_discounts`, `payment_attempts`, `payment_transactions`, `payment_events`, `payment_refunds`, `payment_sync_cursors`, with the indexes shown there. Two literal changes from the spec text: keep `payment_orders.coupon_id` **out** (drop it; discounts are rows), and add `payment_orders.updated_at` trigger-free (handler updates it explicitly). Full `payment_orders` DDL for the migration:

```sql
CREATE TABLE IF NOT EXISTS payment_orders (
    id                 TEXT PRIMARY KEY,
    numeric_code       BIGINT GENERATED BY DEFAULT AS IDENTITY UNIQUE,
    code               TEXT NOT NULL UNIQUE,
    product_id         TEXT NOT NULL REFERENCES products(id),
    plan_id            TEXT NOT NULL REFERENCES plans(id),
    price_id           TEXT NULL REFERENCES plan_prices(id),
    license_id         TEXT NULL REFERENCES licenses(id) ON DELETE SET NULL,
    email              TEXT NOT NULL,
    kind               TEXT NOT NULL CHECK (kind IN ('new','renewal','upgrade','addon')),
    quantity           INT NOT NULL DEFAULT 1 CHECK (quantity > 0),
    subtotal_minor     BIGINT NOT NULL CHECK (subtotal_minor >= 0),
    discount_minor     BIGINT NOT NULL DEFAULT 0 CHECK (discount_minor >= 0),
    tax_minor          BIGINT NOT NULL DEFAULT 0 CHECK (tax_minor >= 0),
    total_minor        BIGINT NOT NULL CHECK (total_minor >= 0),
    currency           CHAR(3) NOT NULL,
    currency_exponent  SMALLINT NOT NULL,
    tax_rate_bp        INT NOT NULL DEFAULT 0,
    provider           TEXT NOT NULL,
    status             TEXT NOT NULL CHECK (status IN
                         ('pending','paid','expired','canceled','refunded','partially_refunded')),
    expires_at         TIMESTAMPTZ NOT NULL,
    paid_at            TIMESTAMPTZ,
    period_start       TIMESTAMPTZ,
    period_end         TIMESTAMPTZ,
    buyer              JSONB NOT NULL DEFAULT '{}'::jsonb,
    metadata           JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_by_api_key_id TEXT NULL REFERENCES api_keys(id) ON DELETE SET NULL,
    last_settlement_error TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS payment_orders_status_idx ON payment_orders (status, expires_at);
CREATE INDEX IF NOT EXISTS payment_orders_license_idx ON payment_orders (license_id, created_at DESC);
CREATE INDEX IF NOT EXISTS payment_orders_email_idx ON payment_orders (lower(email), created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS payment_orders_renewal_once
    ON payment_orders (license_id, period_end) WHERE kind = 'renewal' AND status = 'pending';

CREATE TABLE IF NOT EXISTS payment_order_discounts (
    id            TEXT PRIMARY KEY,
    order_id      TEXT NOT NULL REFERENCES payment_orders(id) ON DELETE CASCADE,
    coupon_id     TEXT NULL REFERENCES coupons(id) ON DELETE SET NULL,
    coupon_code   TEXT NOT NULL,
    type          TEXT NOT NULL CHECK (type IN ('percent','fixed')),
    value         BIGINT NOT NULL,
    amount_minor  BIGINT NOT NULL CHECK (amount_minor >= 0),
    priority      INT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (order_id, coupon_code)
);
```

`20261112_02_coupons.up.sql` — copy spec §4.9 tables verbatim (`coupons`, `coupon_scopes`, `coupon_redemptions`). Logic is Plan 1b; the tables exist now because `payment_order_discounts` and the coupon FKs need them.

`20261112_05_existing_table_hooks.up.sql` (spec §4.11):

```sql
ALTER TABLE products ADD COLUMN IF NOT EXISTS tax_rate_bp INT NOT NULL DEFAULT 0;
ALTER TABLE products ADD COLUMN IF NOT EXISTS price_includes_tax BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE license_renewals ADD COLUMN IF NOT EXISTS payment_order_id TEXT NULL REFERENCES payment_orders(id) ON DELETE SET NULL;
ALTER TABLE license_upgrades ADD COLUMN IF NOT EXISTS payment_order_id TEXT NULL REFERENCES payment_orders(id) ON DELETE SET NULL;
ALTER TABLE license_upgrades ALTER COLUMN stripe_checkout_session_id SET DEFAULT '';
```

Down migrations drop the columns/tables in reverse order.

- [ ] **Step 4: Write the Bun models**

`internal/model/payment.go` — one struct per table, mirroring the repo convention (`bun.BaseModel` with `bun:"table:…"`, `json` snake_case tags, `bun:",pk"` IDs filled by the store):

```go
package model

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

type PlanPrice struct {
	bun.BaseModel     `bun:"table:plan_prices"`
	ID                string    `bun:",pk" json:"id"`
	PlanID            string    `bun:",notnull" json:"plan_id"`
	Provider          string    `bun:",notnull" json:"provider"`
	Currency          string    `bun:",notnull" json:"currency"`
	AmountMinor       int64     `bun:",notnull" json:"amount_minor"`
	CurrencyExponent  int16     `bun:",notnull,default:2" json:"currency_exponent"`
	BillingInterval   string    `bun:",notnull,default:''" json:"billing_interval"`
	ExternalPriceID   string    `bun:",notnull,default:''" json:"external_price_id"`
	Active            bool      `bun:",notnull,default:true" json:"active"`
	CreatedAt         time.Time `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt         time.Time `bun:",nullzero,default:now()" json:"updated_at"`
}

type PaymentProviderConfig struct {
	bun.BaseModel          `bun:"table:payment_provider_configs"`
	ID                     string          `bun:",pk" json:"id"`
	Provider               string          `bun:",notnull" json:"provider"`
	ProductID              string          `bun:",nullzero" json:"product_id,omitempty"`
	Mode                   string          `bun:",notnull" json:"mode"`
	Enabled                bool            `bun:",notnull,default:true" json:"enabled"`
	Config                 json.RawMessage `bun:",notnull,type:jsonb" json:"config"`
	CredentialsEncrypted   []byte          `bun:",notnull" json:"-"`
	WebhookSecretEncrypted []byte          `bun:",notnull" json:"-"`
	CreatedAt              time.Time       `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt              time.Time       `bun:",nullzero,default:now()" json:"updated_at"`
}

type PaymentOrder struct {
	bun.BaseModel  `bun:"table:payment_orders"`
	ID             string          `bun:",pk" json:"id"`
	NumericCode    int64           `bun:",nullzero" json:"numeric_code"`
	Code           string          `bun:",notnull" json:"code"`
	ProductID      string          `bun:",notnull" json:"product_id"`
	PlanID         string          `bun:",notnull" json:"plan_id"`
	PriceID        string          `bun:",nullzero" json:"price_id,omitempty"`
	LicenseID      string          `bun:",nullzero" json:"license_id,omitempty"`
	Email          string          `bun:",notnull" json:"email"`
	Kind           string          `bun:",notnull" json:"kind"`
	Quantity       int             `bun:",notnull,default:1" json:"quantity"`
	SubtotalMinor  int64           `bun:",notnull" json:"subtotal_minor"`
	DiscountMinor  int64           `bun:",notnull" json:"discount_minor"`
	TaxMinor       int64           `bun:",notnull" json:"tax_minor"`
	TotalMinor     int64           `bun:",notnull" json:"total_minor"`
	Currency       string          `bun:",notnull" json:"currency"`
	CurrencyExp    int16           `bun:",notnull" json:"currency_exponent"`
	TaxRateBP      int             `bun:",notnull" json:"tax_rate_bp"`
	Provider       string          `bun:",notnull" json:"provider"`
	Status         string          `bun:",notnull" json:"status"`
	ExpiresAt      time.Time       `bun:",notnull" json:"expires_at"`
	PaidAt         *time.Time      `json:"paid_at,omitempty"`
	PeriodStart    *time.Time      `json:"period_start,omitempty"`
	PeriodEnd      *time.Time      `json:"period_end,omitempty"`
	Buyer          json.RawMessage `bun:",notnull,type:jsonb" json:"buyer"`
	Metadata       json.RawMessage `bun:",notnull,type:jsonb" json:"metadata"`
	CreatedByKeyID string          `bun:",nullzero" json:"created_by_api_key_id,omitempty"`
	LastSettlementError string     `bun:",notnull,default:''" json:"last_settlement_error"`
	CreatedAt      time.Time       `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt      time.Time       `bun:",nullzero,default:now()" json:"updated_at"`
}
```

The next three structs are consumed by later tasks, so their fields are pinned here; the rest (`PaymentOrderDiscount`, `PaymentRefund`, `PaymentSyncCursor`, `Coupon`, `CouponScope`, `CouponRedemption`) follow the same pattern and copy their columns from the migration. Every `json` tag is snake_case; every `*_at` is `time.Time`; nullable columns use pointer or `nullzero` + `omitempty`.

```go
type PaymentAttempt struct {
	bun.BaseModel `bun:"table:payment_attempts"`
	ID            string     `bun:",pk" json:"id"`
	OrderID       string     `bun:",notnull" json:"order_id"`
	Provider      string     `bun:",notnull" json:"provider"`
	ProviderRef   string     `bun:",notnull,default:''" json:"provider_ref"`
	Status        string     `bun:",notnull" json:"status"`
	AmountMinor   int64      `bun:",notnull" json:"amount_minor"`
	Currency      string     `bun:",notnull" json:"currency"`
	QRPayload     string     `bun:",notnull,default:''" json:"qr_payload"`
	CheckoutURL   string     `bun:",notnull,default:''" json:"checkout_url"`
	VANumber      string          `bun:",notnull,default:''" json:"va_number"`
	Raw           json.RawMessage `bun:",notnull,type:jsonb" json:"raw"`
	ExpiresAt     *time.Time      `json:"expires_at,omitempty"`
	CreatedAt     time.Time  `bun:",nullzero,default:now()" json:"created_at"`
	UpdatedAt     time.Time  `bun:",nullzero,default:now()" json:"updated_at"`
}

type PaymentTransaction struct {
	bun.BaseModel `bun:"table:payment_transactions"`
	ID            string     `bun:",pk" json:"id"`
	Provider      string     `bun:",notnull" json:"provider"`
	ProviderRef   string     `bun:",notnull" json:"provider_ref"`
	OrderID       *string    `bun:",nullzero" json:"order_id,omitempty"`
	AmountMinor   int64      `bun:",notnull" json:"amount_minor"`
	Currency      string     `bun:",notnull" json:"currency"`
	Status        string     `bun:",notnull" json:"status"`
	IgnoreReason  string     `bun:",notnull,default:''" json:"ignore_reason"`
	Raw           []byte     `bun:",notnull,type:jsonb" json:"raw"`
	OccurredAt    time.Time  `bun:",notnull" json:"occurred_at"`
	CreatedAt     time.Time  `bun:",nullzero,default:now()" json:"created_at"`
}

type PaymentEvent struct {
	bun.BaseModel `bun:"table:payment_events"`
	ID            string    `bun:",pk" json:"id"`
	Provider      string    `bun:",notnull" json:"provider"`
	EventID       string    `bun:",notnull" json:"event_id"`
	OrderID       string    `bun:",nullzero" json:"order_id,omitempty"`
	Kind          string    `bun:",notnull" json:"kind"`
	Raw           []byte    `bun:",notnull,type:jsonb" json:"raw"`
	OccurredAt    time.Time `json:"occurred_at"`
	CreatedAt     time.Time `bun:",nullzero,default:now()" json:"created_at"`
}
```

The `payment_transactions` unique index is `(provider, provider_ref)`; `payment_events` is `(provider, event_id)`; both are what the dedupe in T8/T9 relies on.

In `internal/model/model.go`, add to `Product`:

```go
TaxRateBP        int  `bun:",notnull" json:"tax_rate_bp"`
PriceIncludesTax bool `bun:",notnull" json:"price_includes_tax"`
```

and to `LicenseRenewal` / `LicenseUpgrade`:

```go
PaymentOrderID string `bun:",nullzero" json:"payment_order_id,omitempty"`
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store -run TestPaymentSchemaTablesExist -v`
Expected: PASS. Then `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store -run TestMain` is not a test; instead run the full migrations smoke: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store -count=1` — all previously green store tests still pass.

- [ ] **Step 6: Commit**

```bash
git add db/migrations/20261112_* internal/model/payment.go internal/model/model.go internal/store/payment_schema_test.go internal/testsupport/testdb/testdb.go
git commit -m "feat(payments): add phase-1a payment schema and Bun models"
```

---

### Task 2: Payment domain core — money/VAT, order codes, statuses, Provider interface

**Files:**
- Create: `internal/payment/domain/money.go`
- Create: `internal/payment/domain/codes.go`
- Create: `internal/payment/domain/status.go`
- Create: `internal/payment/provider/provider.go`
- Create: `internal/payment/provider/fake/fake.go`
- Test: `internal/payment/domain/money_test.go`, `internal/payment/domain/codes_test.go`, `internal/payment/provider/fake/fake_test.go`

**Interfaces:**
- Consumes: nothing but the standard library.
- Produces:
  - `domain.CurrencyExponent(currency string) int16`
  - `domain.Totals struct { NetMinor, TaxMinor, TotalMinor, DiscountMinor int64 }`
  - `domain.ComputeTotals(subtotalMinor, discountMinor int64, taxRateBP int, priceIncludesTax bool) domain.Totals`
  - `domain.NewOrderCode() string`, `domain.IsOrderCode(string) bool`
  - order statuses `domain.OrderPending/Paid/Expired/Canceled/Refunded/PartiallyRefunded`, attempt/transaction statuses, `domain.IsOrderTerminal(string) bool`
  - `provider.Provider` interface, `provider.CreateAttemptRequest`, `provider.CreateAttemptResult`, `provider.WebhookEvent`, `provider.Buyer`, `provider.BankAccount`
  - `fake.Provider` for handler/settlement tests: `fake.New(name string) *fake.Provider` with settable `CreateFn`, `CancelErr`, `WebhookFn`, and `Calls []provider.CreateAttemptRequest`.

- [ ] **Step 1: Write the failing money/VAT tests**

```go
// internal/payment/domain/money_test.go
package domain_test

import "testing"

import "github.com/tabloy/keygate/internal/payment/domain"

func TestComputeTotalsTaxInclusive(t *testing.T) {
	// 1.190.000 ₫ inclusive of 10% VAT.
	got := domain.ComputeTotals(1_190_000, 0, 1000, true)
	if got.TotalMinor != 1_190_000 || got.NetMinor != 1_081_818 || got.TaxMinor != 108_182 {
		t.Fatalf("totals = %+v, want net 1081818 tax 108182 total 1190000", got)
	}
	// The breakdown must add back to the total exactly.
	if got.NetMinor+got.TaxMinor != got.TotalMinor {
		t.Fatalf("net+tax=%d != total=%d", got.NetMinor+got.TaxMinor, got.TotalMinor)
	}
}

func TestComputeTotalsTaxExclusiveWithDiscount(t *testing.T) {
	got := domain.ComputeTotals(1_000_000, 200_000, 1000, false)
	if got.TotalMinor != 880_000 || got.TaxMinor != 80_000 || got.NetMinor != 800_000 {
		t.Fatalf("totals = %+v, want net 800000 tax 80000 total 880000", got)
	}
}

func TestComputeTotalsZeroRate(t *testing.T) {
	got := domain.ComputeTotals(500_000, 0, 0, true)
	if got.TotalMinor != 500_000 || got.TaxMinor != 0 || got.NetMinor != 500_000 {
		t.Fatalf("totals = %+v, want no tax", got)
	}
}

func TestCurrencyExponents(t *testing.T) {
	cases := map[string]int16{"VND": 0, "vnd": 0, "JPY": 0, "USD": 2, "EUR": 2, "": 2}
	for currency, want := range cases {
		if got := domain.CurrencyExponent(currency); got != want {
			t.Fatalf("%q exponent = %d, want %d", currency, got, want)
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/payment/domain -run TestComputeTotals -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement money.go**

```go
package domain

import "strings"

// CurrencyExponent is the number of minor-unit digits for a currency.
// VND has none; USD/EUR have two. An unknown currency defaults to 2, the
// same default the schema uses.
func CurrencyExponent(currency string) int16 {
	switch strings.ToUpper(strings.TrimSpace(currency)) {
	case "VND", "JPY", "KRW":
		return 0
	default:
		return 2
	}
}

// Totals is the money breakdown of an order. Every field is in the
// order's minor units.
type Totals struct {
	NetMinor      int64
	TaxMinor      int64
	TotalMinor    int64
	DiscountMinor int64
}

// ComputeTotals splits a subtotal (after discount) into net + tax.
//
//	priceIncludesTax: total = subtotal-discount; net = round(total*10000/(10000+rate))
//	otherwise:        net = subtotal-discount; tax = round(net*rate/10000)
//
// Rounding is half-up on integer minor units so net+tax always equals
// total; a floated percentage would drift and the invoice would not add
// up. Discount is clamped to the subtotal.
func ComputeTotals(subtotalMinor, discountMinor int64, taxRateBP int, priceIncludesTax bool) Totals {
	if discountMinor < 0 {
		discountMinor = 0
	}
	if discountMinor > subtotalMinor {
		discountMinor = subtotalMinor
	}
	after := subtotalMinor - discountMinor
	if taxRateBP < 0 {
		taxRateBP = 0
	}
	if priceIncludesTax {
		den := int64(10000 + taxRateBP)
		net := (after*10000 + den/2) / den
		return Totals{NetMinor: net, TaxMinor: after - net, TotalMinor: after, DiscountMinor: discountMinor}
	}
	tax := (after*int64(taxRateBP) + 5000) / 10000
	return Totals{NetMinor: after, TaxMinor: tax, TotalMinor: after + tax, DiscountMinor: discountMinor}
}
```

- [ ] **Step 4: Write the failing order-code tests**

```go
// internal/payment/domain/codes_test.go
package domain_test

import (
	"regexp"
	"testing"

	"github.com/tabloy/keygate/internal/payment/domain"
)

// Crockford Base32 excludes I, L, O and U so a code read aloud or typed
// from a bank memo cannot be confused.
var orderCodeRE = regexp.MustCompile(`^KG[0-9A-HJKMNP-TV-Z]{6}$`)

func TestNewOrderCodeFormat(t *testing.T) {
	for i := 0; i < 500; i++ {
		code := domain.NewOrderCode()
		if !orderCodeRE.MatchString(code) {
			t.Fatalf("code %q does not match %s", code, orderCodeRE)
		}
		if !domain.IsOrderCode(code) {
			t.Fatalf("IsOrderCode(%q) = false", code)
		}
	}
}

func TestNewOrderCodeEntropy(t *testing.T) {
	seen := make(map[string]bool, 5000)
	for i := 0; i < 5000; i++ {
		code := domain.NewOrderCode()
		if seen[code] {
			t.Fatalf("duplicate code %q after %d draws", code, i)
		}
		seen[code] = true
	}
}

func TestIsOrderCodeRejects(t *testing.T) {
	for _, bad := range []string{"", "KG7F3K2", "kg7f3k2q", "KG7F3K2I", "KG7F3K2O", "XX7F3K2Q"} {
		if domain.IsOrderCode(bad) {
			t.Fatalf("IsOrderCode(%q) = true, want false", bad)
		}
	}
}
```

- [ ] **Step 5: Run the code tests to verify they fail**

Run: `go test ./internal/payment/domain -run 'TestNewOrderCode|TestIsOrderCode' -v`
Expected: FAIL — `NewOrderCode` undefined.

- [ ] **Step 6: Implement codes.go**

```go
package domain

import (
	"crypto/rand"
	"encoding/binary"
	"regexp"
)

// crockford is Crockford Base32: no I, L, O or U. A customer reading a
// transfer memo to support must not be able to confuse 0/O or 1/I/L.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const orderCodeLength = 6

// NewOrderCode returns "KG" + 6 Crockford Base32 characters (30 bits).
// 2^30 codes keep collision odds negligible at this scale; the UNIQUE
// index on payment_orders.code is the real guard, and the store retries
// on a conflict.
func NewOrderCode() string {
	var n uint32
	var b [4]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			panic("domain: crypto/rand failed: " + err.Error())
		}
		n = binary.BigEndian.Uint32(b[:]) & (1<<30 - 1)
		break
	}
	out := make([]byte, 0, 2+orderCodeLength)
	out = append(out, 'K', 'G')
	for i := orderCodeLength - 1; i >= 0; i-- {
		out = append(out, crockford[(n>>(5*uint(i)))&31])
	}
	return string(out)
}

var orderCodeRE = regexp.MustCompile(`^KG[0-9A-HJKMNP-TV-Z]{6}$`)

func IsOrderCode(s string) bool { return orderCodeRE.MatchString(s) }
```

- [ ] **Step 7: Write the failing Provider fake test**

```go
// internal/payment/provider/fake/fake_test.go
package fake_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/provider/fake"
)

func TestFakeProviderRoundTrip(t *testing.T) {
	f := fake.New("sepay")
	f.CreateFn = func(_ context.Context, req provider.CreateAttemptRequest) (provider.CreateAttemptResult, error) {
		return provider.CreateAttemptResult{ProviderRef: "ref-1", QRImageURL: "https://qr.example/" + req.OrderCode}, nil
	}
	got, err := f.CreateAttempt(context.Background(), provider.CreateAttemptRequest{OrderCode: domain.NewOrderCode()})
	if err != nil || got.ProviderRef != "ref-1" {
		t.Fatalf("CreateAttempt = %+v, %v", got, err)
	}
	if f.Name() != "sepay" || len(f.Calls) != 1 {
		t.Fatalf("name=%q calls=%d", f.Name(), len(f.Calls))
	}
}
```

- [ ] **Step 8: Run it to verify it fails**

Run: `go test ./internal/payment/provider/fake -v`
Expected: FAIL — package does not exist.

- [ ] **Step 9: Implement status.go and the provider interface + fake**

`internal/payment/domain/status.go`:

```go
package domain

const (
	OrderPending          = "pending"
	OrderPaid             = "paid"
	OrderExpired          = "expired"
	OrderCanceled         = "canceled"
	OrderRefunded         = "refunded"
	OrderPartiallyRefunded = "partially_refunded"

	AttemptCreated  = "created"
	AttemptPending  = "pending"
	AttemptPaid     = "paid"
	AttemptFailed   = "failed"
	AttemptExpired  = "expired"
	AttemptCanceled = "canceled"

	TxMatched   = "matched"
	TxUnmatched = "unmatched"
	TxIgnored   = "ignored"
	TxRefunded  = "refunded"
)

// IsOrderTerminal reports whether an order can no longer be paid. A
// terminal order that a provider later reports as paid must not move:
// the settlement service records the transaction for the operator
// instead of granting a licence.
func IsOrderTerminal(status string) bool {
	switch status {
	case OrderPaid, OrderExpired, OrderCanceled, OrderRefunded:
		return true
	}
	return false
}
```

`internal/payment/provider/provider.go`:

```go
package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/tabloy/keygate/internal/payment/domain"
)

type Buyer struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
	Phone string `json:"phone,omitempty"`
	TaxID string `json:"tax_id,omitempty"`
	Address string `json:"address,omitempty"`
}

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
	OrderCode      string
	NumericCode    int64
	AmountMinor    int64
	Currency       string
	CurrencyExp    int16
	Description    string
	Buyer          Buyer
	ReturnURL      string
	CancelURL      string
	ExpiresAt      time.Time
	// Config is the non-secret provider config (bank account, VA prefix,
	// PayOS client id). Credentials are decrypted by the caller and
	// passed separately so an adapter cannot log the secret config.
	Config      map[string]any
	Credentials []byte
	// WebhookSecret is only needed by adapters that sign outbound
	// requests/homework; SePay/PayOS in this phase do not.
	WebhookSecret []byte
}

type CreateAttemptResult struct {
	ProviderRef  string
	QRPayload    string
	QRImageURL   string
	CheckoutURL  string
	Bank         BankAccount
	VANumber     string
	Description  string
	ExpiresAt    time.Time
	Raw          json.RawMessage
}

type WebhookEvent struct {
	Provider       string
	ProviderEventID string
	ProviderRef    string
	OrderCode      string
	NumericCode    int64
	AmountMinor    int64
	Currency       string
	Status         string // domain.OrderPaid or "refunded"/"failed"/"pending"
	OccurredAt     time.Time
	// TransferType is "in" (customer paid) or "out" (refund/payout);
	// SePay sends both to one webhook.
	TransferType string
	Raw          json.RawMessage
}

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
```

`internal/payment/provider/fake/fake.go`:

```go
package fake

import (
	"context"
	"net/http"

	"github.com/tabloy/keygate/internal/payment/provider"
)

// Provider is a scripted adapter for tests. Zero value works: every
// call succeeds with a synthetic ref.
type Provider struct {
	name     string
	CreateFn func(context.Context, provider.CreateAttemptRequest) (provider.CreateAttemptResult, error)
	CancelErr error
	WebhookFn func(*http.Request, []byte, []byte) (provider.WebhookEvent, error)
	TestErr   error
	Calls     []provider.CreateAttemptRequest
	Cancels   []string
}

func New(name string) *Provider { return &Provider{name: name} }

func (p *Provider) Name() string { return p.name }

func (p *Provider) CreateAttempt(ctx context.Context, req provider.CreateAttemptRequest) (provider.CreateAttemptResult, error) {
	p.Calls = append(p.Calls, req)
	if p.CreateFn != nil {
		return p.CreateFn(ctx, req)
	}
	return provider.CreateAttemptResult{ProviderRef: "fake-ref-" + req.OrderCode, QRPayload: "fake-qr", ExpiresAt: req.ExpiresAt}, nil
}

func (p *Provider) CancelAttempt(_ context.Context, ref string) error {
	p.Cancels = append(p.Cancels, ref)
	return p.CancelErr
}

func (p *Provider) ParseWebhook(r *http.Request, raw []byte, secret []byte) (provider.WebhookEvent, error) {
	if p.WebhookFn != nil {
		return p.WebhookFn(r, raw, secret)
	}
	return provider.WebhookEvent{}, nil
}

func (p *Provider) TestConnection(context.Context) error { return p.TestErr }
```

Add the missing status constants (`domain.AttemptCreated = "created"`, etc. as listed). Remove the unused `var n uint32` initialisation in `NewOrderCode` if the compiler complains — the loop runs once, so write it as:

```go
var b [4]byte
if _, err := rand.Read(b[:]); err != nil {
	panic("domain: crypto/rand failed: " + err.Error())
}
n := binary.BigEndian.Uint32(b[:]) & (1<<30 - 1)
```

- [ ] **Step 10: Run all three packages and commit**

Run: `go test ./internal/payment/...`
Expected: PASS.

```bash
git add internal/payment/domain internal/payment/provider
git commit -m "feat(payments): add money/VAT math, order codes and Provider interface"
```

---

### Task 3: Plan prices — store, admin editor API, local-only public read

**Files:**
- Create: `internal/store/plan_prices.go`
- Create: `internal/handler/plan_prices_admin.go`
- Modify: `internal/handler/plans_public.go` (delete the Stripe lookup + cache; read `plan_prices`)
- Modify: `cmd/server/main.go` (construct `PlanPriceAdminHandler`, register routes)
- Test: `internal/store/plan_prices_test.go`, `internal/handler/plan_prices_admin_test.go`

**Interfaces:**
- Consumes: `domain.CurrencyExponent`, `model.PlanPrice`, `store.NewID`.
- Produces:
  - `store.PlanPriceInput{Provider, Currency string; AmountMinor int64; CurrencyExponent int16; BillingInterval, ExternalPriceID string}`
  - `(*Store).UpsertPlanPrice(ctx, planID string, in PlanPriceInput) (*model.PlanPrice, error)`
  - `(*Store).ListPlanPrices(ctx, planID string) ([]*model.PlanPrice, error)`
  - `(*Store).ListActivePlanPrices(ctx, planIDs []string) (map[string][]*model.PlanPrice, error)`
  - `(*Store).DeactivatePlanPrice(ctx, planID, priceID string) error`
  - `(*Store).ResolvePlanPrice(ctx, planID, provider, currency, billingInterval string) (*model.PlanPrice, error)`; `store.ErrPriceNotConfigured`.
  - `handler.PlanPriceAdminHandler{Store *store.Store}` with `List`, `Upsert`, `Delete` methods and routes `GET|PUT|DELETE /api/v1/admin/plans/:id/prices[/:price_id]`.

- [ ] **Step 1: Write the failing store tests**

```go
// internal/store/plan_prices_test.go
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

func seedPlan(t *testing.T, s *store.Store) *model.Plan {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	p := &model.Product{Name: "PriceCo", Slug: "price-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, p); err != nil {
		t.Fatalf("create product: %v", err)
	}
	pl := &model.Plan{ProductID: p.ID, Name: "Pro", Slug: "price-pro-" + suffix, LicenseType: "subscription", MaxActivations: 3, Active: true}
	if err := s.CreatePlan(ctx, pl); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return pl
}

func TestUpsertPlanPriceAndResolve(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	pl := seedPlan(t, s)

	created, err := s.UpsertPlanPrice(ctx, pl.ID, store.PlanPriceInput{Provider: "sepay", Currency: "VND", AmountMinor: 1_190_000})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if created.CurrencyExponent != 0 || created.AmountMinor != 1_190_000 {
		t.Fatalf("row = %+v", created)
	}
	got, err := s.ResolvePlanPrice(ctx, pl.ID, "sepay", "VND", "year")
	if err != nil || got.ID != created.ID {
		t.Fatalf("resolve = %+v, %v", got, err)
	}

	// Same (plan, provider, currency, interval) is replaced: one active
	// price per slot, the old row stays for order history.
	updated, err := s.UpsertPlanPrice(ctx, pl.ID, store.PlanPriceInput{Provider: "sepay", Currency: "VND", AmountMinor: 1_290_000})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if updated.ID == created.ID || updated.AmountMinor != 1_290_000 {
		t.Fatalf("upsert did not replace active row: %+v", updated)
	}
	if _, err := s.ResolvePlanPrice(ctx, pl.ID, "payos", "VND", "year"); !errors.Is(err, store.ErrPriceNotConfigured) {
		t.Fatalf("resolve missing = %v, want ErrPriceNotConfigured", err)
	}
}

func TestUpsertPlanPriceValidation(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	pl := seedPlan(t, s)
	for _, in := range []store.PlanPriceInput{
		{Provider: "stripe", Currency: "VND", AmountMinor: 100},   // phase 1 providers only
		{Provider: "sepay", Currency: "", AmountMinor: 100},       // currency required
		{Provider: "sepay", Currency: "VND", AmountMinor: -1},     // no negative price
		{Provider: "sepay", Currency: "USD", AmountMinor: 100, CurrencyExponent: 0}, // wrong exponent
	} {
		if _, err := s.UpsertPlanPrice(ctx, pl.ID, in); err == nil {
			t.Fatalf("input %+v accepted", in)
		}
	}
}

func TestListActivePlanPrices(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	pl := seedPlan(t, s)
	if _, err := s.UpsertPlanPrice(ctx, pl.ID, store.PlanPriceInput{Provider: "sepay", Currency: "VND", AmountMinor: 100_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertPlanPrice(ctx, pl.ID, store.PlanPriceInput{Provider: "payos", Currency: "VND", AmountMinor: 100_000}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListActivePlanPrices(ctx, []string{pl.ID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got[pl.ID]) != 2 {
		t.Fatalf("prices = %d, want 2", len(got[pl.ID]))
	}
	if err := s.DeactivatePlanPrice(ctx, pl.ID, got[pl.ID][0].ID); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	got, _ = s.ListActivePlanPrices(ctx, []string{pl.ID})
	if len(got[pl.ID]) != 1 {
		t.Fatalf("after deactivate = %d, want 1", len(got[pl.ID]))
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store -run 'TestUpsertPlanPrice|TestListActivePlanPrices' -v`
Expected: FAIL — `PlanPriceInput` undefined.

- [ ] **Step 3: Implement `internal/store/plan_prices.go`**

```go
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
)

var (
	ErrPriceNotConfigured = errors.New("plan price not configured")
	ErrPriceInvalid       = errors.New("plan price invalid")
)

type PlanPriceInput struct {
	Provider         string
	Currency         string
	AmountMinor      int64
	CurrencyExponent int16
	BillingInterval  string
	ExternalPriceID  string
}

func (in *PlanPriceInput) normalize() error {
	in.Provider = strings.ToLower(strings.TrimSpace(in.Provider))
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	in.BillingInterval = strings.ToLower(strings.TrimSpace(in.BillingInterval))
	switch in.Provider {
	case "sepay", "payos":
	default:
		return fmt.Errorf("%w: provider must be sepay or payos", ErrPriceInvalid)
	}
	if len(in.Currency) != 3 {
		return fmt.Errorf("%w: currency must be a 3-letter ISO code", ErrPriceInvalid)
	}
	if in.AmountMinor < 0 {
		return fmt.Errorf("%w: amount_minor must be >= 0", ErrPriceInvalid)
	}
	want := domain.CurrencyExponent(in.Currency)
	if in.CurrencyExponent != 0 && in.CurrencyExponent != want {
		return fmt.Errorf("%w: currency_exponent %d does not match %s (%d)", ErrPriceInvalid, in.CurrencyExponent, in.Currency, want)
	}
	in.CurrencyExponent = want
	switch in.BillingInterval {
	case "", "month", "year":
	default:
		return fmt.Errorf("%w: billing_interval must be '', 'month' or 'year'", ErrPriceInvalid)
	}
	return nil
}

// UpsertPlanPrice makes in the single active price for its
// (plan, provider, currency, interval) slot. The old row is deactivated,
// not deleted: payment_orders references it, and an order's price
// snapshot must stay readable after a price change.
func (s *Store) UpsertPlanPrice(ctx context.Context, planID string, in PlanPriceInput) (*model.PlanPrice, error) {
	if err := in.normalize(); err != nil {
		return nil, err
	}
	if _, err := s.FindPlanByID(ctx, planID); err != nil {
		return nil, err
	}
	row := &model.PlanPrice{
		ID: NewID(), PlanID: planID, Provider: in.Provider, Currency: in.Currency,
		AmountMinor: in.AmountMinor, CurrencyExponent: in.CurrencyExponent,
		BillingInterval: in.BillingInterval, ExternalPriceID: in.ExternalPriceID,
		Active: true, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	err := RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.NewUpdate().Model((*model.PlanPrice)(nil)).
			Set("active = false").Set("updated_at = ?", time.Now()).
			Where("plan_id = ?", planID).
			Where("provider = ?", in.Provider).
			Where("currency = ?", in.Currency).
			Where("billing_interval = ?", in.BillingInterval).
			Where("active").
			Exec(ctx)
		if err != nil {
			return err
		}
		_, err = tx.NewInsert().Model(row).Exec(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return row, nil
}

func (s *Store) ListPlanPrices(ctx context.Context, planID string) ([]*model.PlanPrice, error) {
	var out []*model.PlanPrice
	err := s.DB.NewSelect().Model(&out).
		Where("plan_id = ?", planID).Order("provider ASC", "currency ASC", "billing_interval ASC", "created_at DESC").
		Scan(ctx)
	return out, err
}

func (s *Store) ListActivePlanPrices(ctx context.Context, planIDs []string) (map[string][]*model.PlanPrice, error) {
	out := make(map[string][]*model.PlanPrice, len(planIDs))
	if len(planIDs) == 0 {
		return out, nil
	}
	var rows []*model.PlanPrice
	if err := s.DB.NewSelect().Model(&rows).
		Where("plan_id IN (?)", bun.In(planIDs)).
		Where("active").
		Order("plan_id ASC", "provider ASC", "currency ASC").
		Scan(ctx); err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.PlanID] = append(out[r.PlanID], r)
	}
	return out, nil
}

func (s *Store) DeactivatePlanPrice(ctx context.Context, planID, priceID string) error {
	_, err := s.DB.NewUpdate().Model((*model.PlanPrice)(nil)).
		Set("active = false").Set("updated_at = ?", time.Now()).
		Where("id = ?", priceID).Where("plan_id = ?", planID).Exec(ctx)
	return err
}

func (s *Store) ResolvePlanPrice(ctx context.Context, planID, provider, currency, billingInterval string) (*model.PlanPrice, error) {
	row := new(model.PlanPrice)
	err := s.DB.NewSelect().Model(row).
		Where("plan_id = ?", planID).
		Where("provider = ?", strings.ToLower(provider)).
		Where("currency = ?", strings.ToUpper(currency)).
		Where("billing_interval = ?", strings.ToLower(billingInterval)).
		Where("active").
		Limit(1).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPriceNotConfigured
		}
		return nil, err
	}
	return row, nil
}
```

Add `database/sql` to the imports. `FindPlanByID` exists in the store (used by handlers); if the name differs, use the existing lookup and keep the same semantics.

- [ ] **Step 4: Write the failing public-read test**

Add to `internal/store/plan_prices_test.go`:

```go
// The public pricing page must read only local rows: a plan with no
// active price is not sellable and carries no price in the response.
func TestPlanWithoutLocalPriceIsAbsent(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	pl := seedPlan(t, s)
	prices, err := s.ListActivePlanPrices(ctx, []string{pl.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(prices[pl.ID]) != 0 {
		t.Fatalf("prices = %d, want 0 before an admin enters one", len(prices[pl.ID]))
	}
	if _, err := s.ResolvePlanPrice(ctx, pl.ID, "sepay", "VND", "year"); !errors.Is(err, store.ErrPriceNotConfigured) {
		t.Fatalf("resolve = %v, want ErrPriceNotConfigured", err)
	}
}
```

- [ ] **Step 5: Run it to verify it fails**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store -run TestPlanWithoutLocalPriceIsAbsent -v`
Expected: FAIL until `ListActivePlanPrices` exists (it now does, so this step instead verifies the assertion against an empty price set). If it passes, that is fine: this test is a pin on the no-price path, not new code.

- [ ] **Step 6: Implement the admin handler**

`internal/handler/plan_prices_admin.go`:

```go
package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

type PlanPriceAdminHandler struct {
	Store *store.Store
}

func (h *PlanPriceAdminHandler) List(c *gin.Context) {
	rows, err := h.Store.ListPlanPrices(c, c.Param("id"))
	if err != nil {
		response.Internal(c, err)
		return
	}
	response.OK(c, gin.H{"prices": rows})
}

type planPriceUpsertRequest struct {
	Provider         string `json:"provider"`
	Currency         string `json:"currency"`
	AmountMinor      int64  `json:"amount_minor"`
	CurrencyExponent int16  `json:"currency_exponent"`
	BillingInterval  string `json:"billing_interval"`
	ExternalPriceID  string `json:"external_price_id"`
}

func (h *PlanPriceAdminHandler) Upsert(c *gin.Context) {
	var req planPriceUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid JSON body")
		return
	}
	row, err := h.Store.UpsertPlanPrice(c, c.Param("id"), store.PlanPriceInput{
		Provider: req.Provider, Currency: req.Currency, AmountMinor: req.AmountMinor,
		CurrencyExponent: req.CurrencyExponent, BillingInterval: req.BillingInterval,
		ExternalPriceID: req.ExternalPriceID,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrPriceInvalid):
			response.BadRequest(c, err.Error())
		case errors.Is(err, sql.ErrNoRows):
			response.Err(c, http.StatusNotFound, "PLAN_NOT_FOUND", "plan not found")
		default:
			response.Internal(c, err)
		}
		return
	}
	response.OK(c, row)
}

func (h *PlanPriceAdminHandler) Delete(c *gin.Context) {
	if err := h.Store.DeactivatePlanPrice(c, c.Param("id"), c.Param("price_id")); err != nil {
		response.Internal(c, err)
		return
	}
	response.NoContent(c)
}
```

Add `database/sql` import.

- [ ] **Step 7: Write the failing admin route test**

Use the existing `newAPIFixture` helper (`internal/handler/api_hardening_test.go`):

```go
// internal/handler/plan_prices_admin_test.go
package handler

import (
	"testing"

	"github.com/gin-gonic/gin"
)

func TestPlanPriceAdminUpsertAndList(t *testing.T) {
	f := newAPIFixture(t)
	h := &PlanPriceAdminHandler{Store: f.s}

	code, out := f.call(h.Upsert, "PUT", "/api/v1/admin/plans/"+f.planA.ID+"/prices",
		`{"provider":"sepay","currency":"VND","amount_minor":1190000}`, nil, gin.Param{Key: "id", Value: f.planA.ID})
	if code != 200 {
		t.Fatalf("upsert status %d: %v", code, out)
	}
	code, out = f.call(h.List, "GET", "/api/v1/admin/plans/"+f.planA.ID+"/prices", "", nil,
		gin.Param{Key: "id", Value: f.planA.ID})
	if code != 200 {
		t.Fatalf("list status %d: %v", code, out)
	}
	data, _ := out["data"].(map[string]any)
	rows, _ := data["prices"].([]any)
	if len(rows) != 1 {
		t.Fatalf("prices = %v", data["prices"])
	}
	// Missing plan is a 404, not a silent create.
	code, out = f.call(h.Upsert, "PUT", "/api/v1/admin/plans/does-not-exist/prices",
		`{"provider":"sepay","currency":"VND","amount_minor":1}`, nil, gin.Param{Key: "id", Value: "does-not-exist"})
	if code != 404 || errCode(out) != "PLAN_NOT_FOUND" {
		t.Fatalf("missing plan: %d %v", code, out)
	}
}
```

- [ ] **Step 8: Run the handler test to verify it fails**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/handler -run TestPlanPriceAdminUpsertAndList -v`
Expected: FAIL — `PlanPriceAdminHandler` undefined.

- [ ] **Step 9: Rewrite `plans_public.go` to read local prices**

Replace the Stripe lookup/cache with a store read. The new file keeps the constructor name so `main.go` changes one line only:

```go
package handler

import (
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

// PublicPlansHandler serves GET /api/v1/products/:product_slug/plans.
// Prices are read from plan_prices only: Stripe is not consulted in
// this phase, so a plan without a local row is not sellable and lists
// with an empty price set rather than a stale Stripe amount.
type PublicPlansHandler struct {
	Store *store.Store
	Log   *slog.Logger
}

func (h *PublicPlansHandler) List(c *gin.Context) {
	plans, err := h.Store.ListActivePlansByProductSlug(c, c.Param("product_slug"))
	if err != nil {
		response.NotFound(c, "product not found")
		return
	}
	ids := make([]string, 0, len(plans))
	for _, p := range plans {
		ids = append(ids, p.ID)
	}
	prices, err := h.Store.ListActivePlanPrices(c, ids)
	if err != nil {
		response.Internal(c, err)
		return
	}
	out := make([]gin.H, 0, len(plans))
	for _, p := range plans {
		rows := prices[p.ID]
		if len(rows) == 0 {
			continue // not purchasable: hidden from the pricing response
		}
		out = append(out, gin.H{"plan": p, "prices": rows})
	}
	response.OK(c, gin.H{"plans": out})
}
```

This requires `store.ListActivePlansByProductSlug(ctx, slug) ([]*model.Plan, error)` if the existing handler used a different code path; keep the existing plan query and substitute it. Change `NewPublicPlansHandler` in `main.go` to `handler.NewPublicPlansHandler(db, logger)` remains valid only if the constructor signature is kept:

```go
func NewPublicPlansHandler(s *store.Store, log *slog.Logger) *PublicPlansHandler {
	return &PublicPlansHandler{Store: s, Log: log}
}
```

- [ ] **Step 10: Register admin routes in `cmd/server/main.go`**

After the existing plan routes inside the `admin` group:

```go
priceH := handler.PlanPriceAdminHandler{Store: db}
admin.GET("/plans/:id/prices", priceH.List)
admin.PUT("/plans/:id/prices", priceH.Upsert)
admin.DELETE("/plans/:id/prices/:price_id", priceH.Delete)
```

- [ ] **Step 11: Run the tests and commit**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store -run 'TestUpsertPlanPrice|TestPlanWithoutLocalPriceIsAbsent|TestListActivePlanPrices' && TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/handler -run TestPlanPriceAdminUpsertAndList -v`
Expected: PASS.

```bash
git add internal/store/plan_prices.go internal/store/plan_prices_test.go internal/handler/plan_prices_admin.go internal/handler/plan_prices_admin_test.go internal/handler/plans_public.go cmd/server/main.go
git commit -m "feat(payments): add plan price editor and local-only public pricing"
```

---

### Task 4: Provider configuration — encrypted secrets, resolution with product override

**Files:**
- Create: `internal/store/provider_configs.go`
- Create: `internal/payment/providercfg/codec.go`
- Create: `internal/payment/providercfg/service.go`
- Create: `internal/handler/payment_providers_admin.go`
- Modify: `cmd/server/main.go` (construct codec + handler, register routes)
- Test: `internal/payment/providercfg/codec_test.go`, `internal/payment/providercfg/service_test.go`, `internal/handler/payment_providers_admin_test.go`

**Interfaces:**
- Consumes: `crypto.DeriveSubkey`, `crypto.NewAESGCM`, `model.PaymentProviderConfig`, `store.NewID`.
- Produces:
  - `providercfg.Codec` with `Seal(id, field string, plaintext []byte) ([]byte, error)` / `Open(id, field string, envelope []byte) ([]byte, error)`; `providercfg.NewCodec(masterKey []byte) (*Codec, error)`.
  - `providercfg.ResolvedConfig{Provider, Mode string; ProductID string; Enabled bool; Sepay *SepayConfig; PayOS *PayOSConfig; APIToken, ClientID, APIKey, ChecksumKey, WebhookSecret string}`
  - `providercfg.Service` with `Resolve(ctx, productID, provider, mode string) (*ResolvedConfig, error)` (product override → install default → `ErrProviderNotConfigured`; disabled → `ErrProviderDisabled`), `List(ctx)`, `Upsert(ctx, in UpsertInput)`, `DeleteSecrets(ctx, id string, fields []string)`.
  - `handler.PaymentProviderAdminHandler` with `List`, `Upsert`, `DeleteSecrets`.

- [ ] **Step 1: Write the failing codec test**

```go
// internal/payment/providercfg/codec_test.go
package providercfg_test

import (
	"bytes"
	"testing"

	"github.com/tabloy/keygate/internal/payment/providercfg"
)

func TestCodecRoundTripBindsAAD(t *testing.T) {
	master := bytes.Repeat([]byte{0x42}, 32)
	c, err := providercfg.NewCodec(master)
	if err != nil {
		t.Fatalf("codec: %v", err)
	}
	sealed, err := c.Seal("cfg-1", "credentials", []byte(`{"api_key":"secret"}`))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(sealed, []byte("secret")) {
		t.Fatal("plaintext survived in the ciphertext")
	}
	got, err := c.Open("cfg-1", "credentials", sealed)
	if err != nil || string(got) != `{"api_key":"secret"}` {
		t.Fatalf("open = %q, %v", got, err)
	}
	// Same bytes under a different row/field must not decrypt: AAD is
	// the anti-swap guard.
	if _, err := c.Open("cfg-2", "credentials", sealed); err == nil {
		t.Fatal("decrypt with wrong AAD succeeded")
	}
	if _, err := c.Open("cfg-1", "webhook_secret", sealed); err == nil {
		t.Fatal("decrypt with wrong field succeeded")
	}
}

func TestCodecRejectsShortMaster(t *testing.T) {
	if _, err := providercfg.NewCodec([]byte("short")); err == nil {
		t.Fatal("short master accepted")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/payment/providercfg -run TestCodec -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement codec.go**

```go
// Package providercfg stores payment-provider secrets encrypted at rest
// and resolves which provider configuration applies to an order.
package providercfg

import (
	"fmt"

	"github.com/tabloy/keygate/internal/crypto"
)

const subkeyPurpose = "payment-config"

// Codec seals each secret with its own AES-GCM key (HKDF subkey of the
// install master) and binds the ciphertext to the row and field, so a
// credentials blob copied onto another provider row fails to decrypt
// instead of silently authenticating as that provider.
type Codec struct{ aead *crypto.AESGCM }

func NewCodec(masterKey []byte) (*Codec, error) {
	sub, err := crypto.DeriveSubkey(masterKey, subkeyPurpose)
	if err != nil {
		return nil, fmt.Errorf("derive payment-config subkey: %w", err)
	}
	aead, err := crypto.NewAESGCM(sub)
	if err != nil {
		return nil, fmt.Errorf("payment-config aead: %w", err)
	}
	return &Codec{aead: aead}, nil
}

func aad(id, field string) []byte {
	return []byte("payment_provider_config:" + id + ":" + field)
}

func (c *Codec) Seal(id, field string, plaintext []byte) ([]byte, error) {
	return c.aead.Encrypt(plaintext, aad(id, field))
}

func (c *Codec) Open(id, field string, envelope []byte) ([]byte, error) {
	return c.aead.Decrypt(envelope, aad(id, field))
}
```

- [ ] **Step 4: Implement store/provider_configs.go**

```go
package store

import (
	"context"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
)

var ErrProviderConfigNotFound = errors.New("payment provider config not found")

type ProviderConfigInput struct {
	Provider               string
	ProductID              string
	Mode                   string
	Enabled                bool
	Config                 []byte // JSONB
	CredentialsEncrypted   []byte
	WebhookSecretEncrypted []byte
}

// UpsertPaymentProviderConfig writes one (provider, product, mode) row.
// Updating an existing row keeps its id so the encrypted bytes stay
// bound to the same AAD.
func (s *Store) UpsertPaymentProviderConfig(ctx context.Context, in ProviderConfigInput) (*model.PaymentProviderConfig, error) {
	existing := new(model.PaymentProviderConfig)
	err := s.DB.NewSelect().Model(existing).
		Where("provider = ?", in.Provider).
		Where("product_id IS NOT DISTINCT FROM ?", nullable(in.ProductID)).
		Where("mode = ?", in.Mode).Limit(1).Scan(ctx)
	var row *model.PaymentProviderConfig
	switch {
	case err == nil:
		row = existing
	case errors.Is(err, sql.ErrNoRows):
		row = &model.PaymentProviderConfig{ID: NewID(), Provider: in.Provider, ProductID: in.ProductID, Mode: in.Mode, CreatedAt: time.Now()}
	default:
		return nil, err
	}
	row.Enabled = in.Enabled
	row.Config = in.Config
	if in.CredentialsEncrypted != nil {
		row.CredentialsEncrypted = in.CredentialsEncrypted
	}
	if in.WebhookSecretEncrypted != nil {
		row.WebhookSecretEncrypted = in.WebhookSecretEncrypted
	}
	row.UpdatedAt = time.Now()
	_, err = s.DB.NewInsert().Model(row).On("CONFLICT (provider, product_id, mode) DO UPDATE").
		Set("enabled = EXCLUDED.enabled").
		Set("config = EXCLUDED.config").
		Set("credentials_encrypted = COALESCE(NULLIF(EXCLUDED.credentials_encrypted, ''::bytea), payment_provider_configs.credentials_encrypted)").
		Set("webhook_secret_encrypted = COALESCE(NULLIF(EXCLUDED.webhook_secret_encrypted, ''::bytea), payment_provider_configs.webhook_secret_encrypted)").
		Set("updated_at = now()").
		Returning("*").Exec(ctx)
	if err != nil {
		return nil, err
	}
	return row, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) ListPaymentProviderConfigs(ctx context.Context) ([]*model.PaymentProviderConfig, error) {
	var out []*model.PaymentProviderConfig
	err := s.DB.NewSelect().Model(&out).Order("provider ASC", "product_id ASC NULLS FIRST", "mode ASC").Scan(ctx)
	return out, err
}

func (s *Store) FindPaymentProviderConfig(ctx context.Context, provider, productID, mode string) (*model.PaymentProviderConfig, error) {
	row := new(model.PaymentProviderConfig)
	err := s.DB.NewSelect().Model(row).
		Where("provider = ?", provider).
		Where("product_id IS NOT DISTINCT FROM ?", nullable(productID)).
		Where("mode = ?", mode).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProviderConfigNotFound
	}
	return row, err
}

func (s *Store) ClearProviderSecrets(ctx context.Context, id string, credentials, webhookSecret bool) error {
	q := s.DB.NewUpdate().Model((*model.PaymentProviderConfig)(nil)).Set("updated_at = now()").Where("id = ?", id)
	if credentials {
		q = q.Set("credentials_encrypted = ''::bytea")
	}
	if webhookSecret {
		q = q.Set("webhook_secret_encrypted = ''::bytea")
	}
	_, err := q.Exec(ctx)
	return err
}
```

`model.PaymentProviderConfig.Config` is `json.RawMessage`; `in.Config []byte` assigns cleanly.

- [ ] **Step 5: Write the failing resolution test**

```go
// internal/payment/providercfg/service_test.go
package providercfg_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/providercfg"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

func TestResolveProductOverrideWins(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	master := bytes.Repeat([]byte{0x17}, 32)
	codec, err := providercfg.NewCodec(master)
	if err != nil {
		t.Fatal(err)
	}
	svc := providercfg.NewService(s, codec, "live")

	// Install default for sepay.
	if _, err := svc.Upsert(ctx, providercfg.UpsertInput{
		Provider: "sepay", Mode: "live", Enabled: true,
		Config: providercfg.SepayConfig{BankName: "MB Bank", BankBIN: "970422", AccountNo: "0978482700", AccountName: "NGUYEN VAN A"},
		Credentials: providercfg.SepayCredentials{APIToken: "default-token"},
		WebhookSecret: "default-secret",
	}); err != nil {
		t.Fatalf("upsert default: %v", err)
	}
	got, err := svc.Resolve(ctx, "", "sepay", "live")
	if err != nil || got.Sepay.AccountNo != "0978482700" || got.APIToken != "default-token" {
		t.Fatalf("resolve default = %+v, %v", got, err)
	}
	if got.WebhookSecret != "default-secret" {
		t.Fatalf("webhook secret not decrypted: %q", got.WebhookSecret)
	}

	// Product override for payos. payment_provider_configs.product_id is
	// a real FK, so the override needs a real product row.
	prodX := &model.Product{Name: "Override", Slug: "override-" + time.Now().Format("150405.000000"), Type: "desktop"}
	if err := s.CreateProduct(ctx, prodX); err != nil {
		t.Fatal(err)
	}
	prodY := &model.Product{Name: "Disabled", Slug: "disabled-" + time.Now().Format("150405.000000"), Type: "desktop"}
	if err := s.CreateProduct(ctx, prodY); err != nil {
		t.Fatal(err)
	}
	code, err := svc.Upsert(ctx, providercfg.UpsertInput{
		Provider: "payos", Mode: "live", Enabled: true, ProductID: prodX.ID,
		Config: providercfg.PayOSConfig{ClientID: "client-1"},
		Credentials: providercfg.PayOSCredentials{APIKey: "api-key", ChecksumKey: "checksum-key"},
		WebhookSecret: "payos-secret",
	})
	if err != nil || len(code.WebhookSecretEncrypted) == 0 {
		t.Fatalf("upsert override: %+v, %v", code, err)
	}
	got, err = svc.Resolve(ctx, prodX.ID, "payos", "live")
	if err != nil || got.ClientID != "client-1" {
		t.Fatalf("resolve override = %+v, %v", got, err)
	}

	// A different product must not inherit the override.
	if _, err := svc.Resolve(ctx, prodY.ID, "payos", "live"); !errors.Is(err, providercfg.ErrProviderNotConfigured) {
		t.Fatalf("resolve other = %v, want ErrProviderNotConfigured", err)
	}

	// Disabled is explicit, never a fallback to the default.
	if _, err := svc.Upsert(ctx, providercfg.UpsertInput{Provider: "payos", Mode: "live", Enabled: false, ProductID: prodY.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resolve(ctx, prodY.ID, "payos", "live"); !errors.Is(err, providercfg.ErrProviderDisabled) {
		t.Fatalf("disabled resolve = %v, want ErrProviderDisabled", err)
	}
}
```

- [ ] **Step 6: Run it to verify it fails**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/payment/providercfg -run TestResolveProductOverrideWins -v`
Expected: FAIL — `providercfg.NewService` undefined.

- [ ] **Step 7: Implement service.go**

```go
package providercfg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

var (
	ErrProviderNotConfigured = errors.New("payment provider not configured")
	ErrProviderDisabled      = errors.New("payment provider disabled")
)

type SepayConfig struct {
	BankName    string `json:"bank_name"`
	BankBIN     string `json:"bank_bin"`
	// BankCode is the short code the VietQR image host expects (MB, VCB,
	// …); BankBIN is the full 6-digit code the bank API uses.
	BankCode    string `json:"bank_code"`
	AccountNo   string `json:"account_no"`
	AccountName string `json:"account_name"`
	// WebhookAuthMode is "hmac", "apikey" or "none" — the mode chosen in
	// the SePay dashboard for this webhook.
	WebhookAuthMode string `json:"webhook_auth_mode"`
}

type SepayCredentials struct {
	APIToken  string `json:"api_token"`
	IPNSecret string `json:"ipn_secret"` // hosted-gateway IPN, X-Secret-Key
}

type PayOSConfig struct {
	ClientID string `json:"client_id"`
}

type PayOSCredentials struct {
	APIKey      string `json:"api_key"`
	ChecksumKey string `json:"checksum_key"`
}

// ResolvedConfig is everything an adapter needs, with secrets already
// decrypted. It must never be serialised.
type ResolvedConfig struct {
	Provider  string
	Mode      string
	ProductID string
	Enabled   bool

	Sepay *SepayConfig
	PayOS *PayOSConfig

	// Decrypted credentials.
	APIToken      string // SePay REST token
	IPNSecret     string // SePay gateway IPN (X-Secret-Key)
	ClientID      string // PayOS
	APIKey        string // PayOS
	ChecksumKey   string // PayOS
	WebhookSecret string // SePay bank webhook HMAC secret

	// Static-QR bank details, copied out of SepayConfig for the adapter.
	BankName        string
	BankCode        string
	AccountNo       string
	AccountName     string
	WebhookAuthMode string
}

type UpsertInput struct {
	Provider      string
	ProductID     string
	Mode          string
	Enabled       bool
	Config        any
	Credentials   any // SepayCredentials or PayOSCredentials
	WebhookSecret string
}

type Service struct {
	store *store.Store
	codec *Codec
	mode  string
}

func NewService(s *store.Store, c *Codec, mode string) *Service {
	return &Service{store: s, codec: c, mode: mode}
}

func (s *Service) Upsert(ctx context.Context, in UpsertInput) (*model.PaymentProviderConfig, error) {
	provider := strings.ToLower(strings.TrimSpace(in.Provider))
	if provider != "sepay" && provider != "payos" {
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}
	mode := in.Mode
	if mode == "" {
		mode = s.mode
	}
	if mode != "test" && mode != "live" {
		return nil, fmt.Errorf("mode must be test or live")
	}
	if in.WebhookSecret == "" && in.Enabled {
		return nil, fmt.Errorf("webhook secret is required for an enabled provider")
	}
	cfgJSON, err := json.Marshal(in.Config)
	if err != nil {
		return nil, err
	}
	credsJSON, err := json.Marshal(in.Credentials)
	if err != nil {
		return nil, err
	}
	row := &model.PaymentProviderConfig{ID: store.NewID(), Provider: provider, ProductID: in.ProductID, Mode: mode}
	// A row may already exist and keep its id (the AAD binds to it), so
	// find it before sealing.
	if existing, err := s.store.FindPaymentProviderConfig(ctx, provider, in.ProductID, mode); err == nil {
		row.ID = existing.ID
	}
	credsSealed, err := s.codec.Seal(row.ID, "credentials", credsJSON)
	if err != nil {
		return nil, err
	}
	secretSealed, err := s.codec.Seal(row.ID, "webhook_secret", []byte(in.WebhookSecret))
	if err != nil {
		return nil, err
	}
	return s.store.UpsertPaymentProviderConfig(ctx, store.ProviderConfigInput{
		Provider: provider, ProductID: in.ProductID, Mode: mode, Enabled: in.Enabled,
		Config: cfgJSON, CredentialsEncrypted: credsSealed, WebhookSecretEncrypted: secretSealed,
	})
}

func (s *Service) Resolve(ctx context.Context, productID, provider, mode string) (*ResolvedConfig, error) {
	if mode == "" {
		mode = s.mode
	}
	row, err := s.find(ctx, productID, provider, mode)
	if err != nil {
		return nil, err
	}
	if !row.Enabled {
		return nil, ErrProviderDisabled
	}
	out := &ResolvedConfig{Provider: row.Provider, Mode: row.Mode, ProductID: row.ProductID, Enabled: true}
	creds, err := s.codec.Open(row.ID, "credentials", row.CredentialsEncrypted)
	if err != nil {
		return nil, fmt.Errorf("decrypt provider credentials: %w", err)
	}
	secret, err := s.codec.Open(row.ID, "webhook_secret", row.WebhookSecretEncrypted)
	if err != nil {
		return nil, fmt.Errorf("decrypt webhook secret: %w", err)
	}
	out.WebhookSecret = string(secret)
	switch row.Provider {
	case "sepay":
		var c SepayConfig
		if err := json.Unmarshal(row.Config, &c); err != nil {
			return nil, err
		}
		var cr SepayCredentials
		if err := json.Unmarshal(creds, &cr); err != nil {
			return nil, err
		}
		out.Sepay = &c
		out.APIToken, out.IPNSecret = cr.APIToken, cr.IPNSecret
		out.BankName, out.BankCode = c.BankName, c.BankCode
		out.AccountNo, out.AccountName = c.AccountNo, c.AccountName
		out.WebhookAuthMode = c.WebhookAuthMode
	case "payos":
		var c PayOSConfig
		if err := json.Unmarshal(row.Config, &c); err != nil {
			return nil, err
		}
		var cr PayOSCredentials
		if err := json.Unmarshal(creds, &cr); err != nil {
			return nil, err
		}
		out.PayOS, out.ClientID, out.APIKey, out.ChecksumKey = &c, c.ClientID, cr.APIKey, cr.ChecksumKey
	}
	return out, nil
}

func (s *Service) find(ctx context.Context, productID, provider, mode string) (*model.PaymentProviderConfig, error) {
	if productID != "" {
		row, err := s.store.FindPaymentProviderConfig(ctx, provider, productID, mode)
		if err == nil {
			return row, nil
		}
		if !errors.Is(err, store.ErrProviderConfigNotFound) {
			return nil, err
		}
	}
	row, err := s.store.FindPaymentProviderConfig(ctx, provider, "", mode)
	if errors.Is(err, store.ErrProviderConfigNotFound) {
		return nil, ErrProviderNotConfigured
	}
	return row, err
}

func (s *Service) List(ctx context.Context) ([]*model.PaymentProviderConfig, error) {
	return s.store.ListPaymentProviderConfigs(ctx)
}

func (s *Service) DeleteSecrets(ctx context.Context, id string, fields []string) error {
	var creds, secret bool
	for _, f := range fields {
		switch f {
		case "credentials":
			creds = true
		case "webhook_secret":
			secret = true
		}
	}
	return s.store.ClearProviderSecrets(ctx, id, creds, secret)
}
```

Note: `Upsert` requires a webhook secret on first create only. Once a secret exists, an update with a blank secret must keep the old one. Change the validation to: load existing row first; if `in.WebhookSecret == "" && in.Enabled && existing has no secret` → error; and pass `nil` sealed bytes when blank so the COALESCE keeps the old ciphertext:

```go
existing, _ := s.store.FindPaymentProviderConfig(ctx, provider, in.ProductID, mode)
hasSecret := existing != nil && len(existing.WebhookSecretEncrypted) > 0
if in.Enabled && in.WebhookSecret == "" && !hasSecret {
	return nil, fmt.Errorf("webhook secret is required for an enabled provider")
}
var secretSealed []byte
if in.WebhookSecret != "" {
	secretSealed, err = s.codec.Seal(row.ID, "webhook_secret", []byte(in.WebhookSecret))
	...
}
var credsSealed []byte
if in.Credentials != nil { ... }
```

- [ ] **Step 8: Write the failing admin handler test**

Extend `newAPIFixture`? The provider handler needs a codec/service; build them in the test:

```go
// internal/handler/payment_providers_admin_test.go
package handler

import (
	"bytes"
	"testing"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/providercfg"
	"github.com/gin-gonic/gin"
)

func TestProviderAdminNeverReturnsSecrets(t *testing.T) {
	f := newAPIFixture(t)
	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatal(err)
	}
	svc := providercfg.NewService(f.s, codec, "live")
	h := &PaymentProviderAdminHandler{Service: svc}

	code, out := f.call(h.Upsert, "PUT", "/api/v1/admin/payments/providers",
		`{"provider":"sepay","mode":"live","enabled":true,"config":{"bank_name":"MB Bank","bank_bin":"970422","account_no":"0978482700","account_name":"NGUYEN VAN A"},"credentials":{"api_token":"tok_123"},"webhook_secret":"whsec_123"}`, nil)
	if code != 200 {
		t.Fatalf("upsert: %d %v", code, out)
	}
	if bytes.Contains([]byte(f.bodyJSON(t, h.List, nil)), []byte("tok_123")) {
		t.Fatal("list returned the api token")
	}
	var rows []map[string]any
	_ = f.decodeData(t, h.List, &rows)
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	if rows[0]["secret_set"] != true || rows[0]["credentials_set"] != true {
		t.Fatalf("flags = %v", rows[0])
	}
	if _, ok := rows[0]["credentials"]; ok {
		t.Fatal("credentials field present in admin response")
	}

	// Clearing the webhook secret is an explicit delete.
	rowID, _ := rows[0]["id"].(string)
	code, out = f.call(h.DeleteSecrets, "DELETE", "/api/v1/admin/payments/providers/"+rowID+"/secrets",
		`{"fields":["webhook_secret"]}`, nil, gin.Param{Key: "id", Value: rowID})
	if code != 204 {
		t.Fatalf("delete secrets: %d %v", code, out)
	}
}
```

Add two small helpers to `api_hardening_test.go` (used by later handler tests too):

```go
func (f *apiFixture) bodyJSON(t *testing.T, fn gin.HandlerFunc, params ...gin.Param) []byte {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Params = params
	fn(c)
	return w.Body.Bytes()
}

func (f *apiFixture) decodeData(t *testing.T, fn gin.HandlerFunc, dst any, params ...gin.Param) error {
	t.Helper()
	var env struct {
		Data any `json:"data"`
	}
	if err := json.Unmarshal(f.bodyJSON(t, fn, params...), &env); err != nil {
		return err
	}
	raw, _ := json.Marshal(env.Data)
	return json.Unmarshal(raw, dst)
}
```

- [ ] **Step 9: Implement the admin handler**

`internal/handler/payment_providers_admin.go`:

```go
package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/payment/providercfg"
	"github.com/tabloy/keygate/pkg/response"
)

type PaymentProviderAdminHandler struct {
	Service *providercfg.Service
}

// providerView is the admin projection: secrets are never serialised,
// only whether one is set, so a stolen admin session cannot exfiltrate
// the merchant credentials.
type providerView struct {
	ID             string `json:"id"`
	Provider       string `json:"provider"`
	ProductID      string `json:"product_id,omitempty"`
	Mode           string `json:"mode"`
	Enabled        bool   `json:"enabled"`
	Config         any    `json:"config"`
	CredentialsSet bool   `json:"credentials_set"`
	SecretSet      bool   `json:"secret_set"`
}

func (h *PaymentProviderAdminHandler) List(c *gin.Context) {
	rows, err := h.Service.List(c)
	if err != nil {
		response.Internal(c, err)
		return
	}
	out := make([]providerView, 0, len(rows))
	for _, r := range rows {
		out = append(out, providerView{ID: r.ID, Provider: r.Provider, ProductID: r.ProductID, Mode: r.Mode,
			Enabled: r.Enabled, Config: r.Config,
			CredentialsSet: len(r.CredentialsEncrypted) > 0, SecretSet: len(r.WebhookSecretEncrypted) > 0})
	}
	response.OK(c, gin.H{"providers": out})
}

type providerUpsertRequest struct {
	Provider      string `json:"provider"`
	ProductID     string `json:"product_id"`
	Mode          string `json:"mode"`
	Enabled       bool   `json:"enabled"`
	Config        any    `json:"config"`
	Credentials   any    `json:"credentials"`
	WebhookSecret string `json:"webhook_secret"`
}

func (h *PaymentProviderAdminHandler) Upsert(c *gin.Context) {
	var req providerUpsertRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid JSON body")
		return
	}
	row, err := h.Service.Upsert(c, providercfg.UpsertInput{
		Provider: req.Provider, ProductID: req.ProductID, Mode: req.Mode, Enabled: req.Enabled,
		Config: req.Config, Credentials: req.Credentials, WebhookSecret: req.WebhookSecret,
	})
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.OK(c, providerView{ID: row.ID, Provider: row.Provider, ProductID: row.ProductID, Mode: row.Mode,
		Enabled: row.Enabled, Config: row.Config, CredentialsSet: len(row.CredentialsEncrypted) > 0,
		SecretSet: len(row.WebhookSecretEncrypted) > 0})
}

func (h *PaymentProviderAdminHandler) DeleteSecrets(c *gin.Context) {
	var req struct {
		Fields []string `json:"fields"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid JSON body")
		return
	}
	if err := h.Service.DeleteSecrets(c, c.Param("id"), req.Fields); err != nil {
		response.Internal(c, err)
		return
	}
	response.NoContent(c)
}
```

- [ ] **Step 10: Register routes and gate the missing master key**

In `cmd/server/main.go`, next to the payment/plan handlers:

```go
// Provider secrets are sealed with the same master key as release keys;
// without it the admin API must refuse to store credentials rather than
// write them in plaintext.
var paymentCodec *providercfg.Codec
if cfg.ReleaseKeyEncryptionKey != "" {
	c, err := providercfg.NewCodec(mustHexKey(cfg.ReleaseKeyEncryptionKey))
	if err != nil {
		logger.Error("payment config encryption disabled", "error", err)
	} else {
		paymentCodec = c
	}
}
```

Use the existing hex-decoding helper the release-key path already uses (search `ReleaseKeyEncryptionKey` in `cmd/server/main.go`; it already builds an AES-GCM key). Register the routes:

```go
if paymentCodec != nil {
	providerSvc := providercfg.NewService(db, paymentCodec, cfg.PaymentMode)
	providerH := handler.PaymentProviderAdminHandler{Service: providerSvc}
	admin.GET("/payments/providers", providerH.List)
	admin.PUT("/payments/providers", providerH.Upsert)
	admin.DELETE("/payments/providers/:id/secrets", providerH.DeleteSecrets)
}
```

When `paymentCodec == nil`, the routes are absent and checkout returns `PAYMENT_PROVIDER_DISABLED` (T10) — that is the intended deployment guard.

- [ ] **Step 11: Run tests and commit**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/payment/providercfg ./internal/handler -run 'TestCodec|TestResolveProductOverrideWins|TestProviderAdminNeverReturnsSecrets' -v`
Expected: PASS.

```bash
git add internal/store/provider_configs.go internal/payment/providercfg internal/handler/payment_providers_admin.go internal/handler/payment_providers_admin_test.go internal/handler/api_hardening_test.go cmd/server/main.go
git commit -m "feat(payments): encrypted provider configs with product override resolution"
```

---

### Task 5: Orders and attempts store

**Files:**
- Create: `internal/store/payments.go`
- Test: `internal/store/payments_test.go`

**Interfaces:**
- Consumes: `domain.NewOrderCode`, `domain.IsOrderTerminal`.
- Produces:
  - `store.CreateOrderInput{ProductID, PlanID, PriceID, Email, Kind string; Quantity int; SubtotalMinor, DiscountMinor, TaxMinor, TotalMinor int64; Currency string; CurrencyExponent int16; TaxRateBP int; Provider string; ExpiresAt time.Time; Buyer, Metadata []byte; CreatedByKeyID string; Discounts []store.OrderDiscount}`
  - `(*Store).CreatePaymentOrder(ctx, in) (*model.PaymentOrder, error)` — retries order-code collisions; `store.ErrOrderCodeCollision` sentinel not exposed.
  - `(*Store).GetPaymentOrderByCode(ctx, code) (*model.PaymentOrder, error)`; `store.ErrOrderNotFound`.
  - `(*Store).GetPaymentOrderByNumericCode(ctx, numeric int64) (*model.PaymentOrder, error)`.
  - `(*Store).GetPaymentOrderForUpdate(ctx, tx bun.Tx, orderID string) (*model.PaymentOrder, error)`.
  - `(*Store).MarkPaymentOrderPaid(ctx, tx bun.Tx, orderID string, paidAt time.Time) (*model.PaymentOrder, error)` — row lock, terminal guard `ErrOrderNotPayable`.
  - `(*Store).CancelPaymentOrder(ctx, code string) (*model.PaymentOrder, error)`.
  - `(*Store).ExpirePaymentOrders(ctx, now time.Time) (int, error)`.
  - `(*Store).CreatePaymentAttempt(ctx, attempt *model.PaymentAttempt) error`; `(*Store).UpdatePaymentAttemptStatus(ctx, id, status, providerRef string) error`; `(*Store).GetLatestPaymentAttempt(ctx, orderID string) (*model.PaymentAttempt, error)`.
  - `store.OrderDiscount{CouponCode, Type string; Value, AmountMinor int64; Priority int; CouponID string}`.

- [ ] **Step 1: Write the failing order round-trip test**

```go
// internal/store/payments_test.go
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

func seedProductPlan(t *testing.T, s *store.Store) (productID, planID string) {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	p := &model.Product{Name: "PayCo", Slug: "pay-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, p); err != nil {
		t.Fatal(err)
	}
	pl := &model.Plan{ProductID: p.ID, Name: "Pro", Slug: "pay-pro-" + suffix, LicenseType: "subscription", MaxActivations: 3, Active: true}
	if err := s.CreatePlan(ctx, pl); err != nil {
		t.Fatal(err)
	}
	return p.ID, pl.ID
}

func newOrderInput(productID, planID string) store.CreateOrderInput {
	return store.CreateOrderInput{
		ProductID: productID, PlanID: planID, Email: "buyer@example.com", Kind: "new", Quantity: 1,
		SubtotalMinor: 1_190_000, TotalMinor: 1_190_000, Currency: "VND", CurrencyExponent: 0,
		Provider: "sepay", ExpiresAt: time.Now().Add(time.Hour),
		Buyer: []byte(`{"name":"ACME"}`), Metadata: []byte(`{}`),
	}
}

func TestCreateAndFetchPaymentOrder(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)

	order, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !domain.IsOrderCode(order.Code) {
		t.Fatalf("code %q not a KG code", order.Code)
	}
	if order.NumericCode == 0 || order.Status != domain.OrderPending {
		t.Fatalf("order = %+v", order)
	}
	got, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil || got.ID != order.ID {
		t.Fatalf("get by code = %+v, %v", got, err)
	}
	got, err = s.GetPaymentOrderByNumericCode(ctx, order.NumericCode)
	if err != nil || got.ID != order.ID {
		t.Fatalf("get by numeric = %+v, %v", got, err)
	}
	if _, err := s.GetPaymentOrderByCode(ctx, "KGZZZZZZ"); !errors.Is(err, store.ErrOrderNotFound) {
		t.Fatalf("missing order = %v, want ErrOrderNotFound", err)
	}
}

func TestOrderLifecycleGuards(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)
	order, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatal(err)
	}

	paid, err := s.MarkPaymentOrderPaid(ctx, nil, order.ID, time.Now())
	if err != nil || paid.Status != domain.OrderPaid || paid.PaidAt == nil {
		t.Fatalf("mark paid = %+v, %v", paid, err)
	}
	// Paying twice must not move the paid_at or re-grant.
	if _, err := s.MarkPaymentOrderPaid(ctx, nil, order.ID, time.Now().Add(time.Minute)); !errors.Is(err, store.ErrOrderNotPayable) {
		t.Fatalf("double pay = %v, want ErrOrderNotPayable", err)
	}

	canceled, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.CancelPaymentOrder(ctx, canceled.Code)
	if err != nil || got.Status != domain.OrderCanceled {
		t.Fatalf("cancel = %+v, %v", got, err)
	}
	if _, err := s.MarkPaymentOrderPaid(ctx, nil, canceled.ID, time.Now()); !errors.Is(err, store.ErrOrderNotPayable) {
		t.Fatalf("pay canceled = %v, want ErrOrderNotPayable", err)
	}
}

func TestExpirePaymentOrders(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)
	in := newOrderInput(productID, planID)
	in.ExpiresAt = time.Now().Add(-time.Minute)
	order, err := s.CreatePaymentOrder(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.ExpirePaymentOrders(ctx, time.Now())
	if err != nil || n < 1 {
		t.Fatalf("expire = %d, %v", n, err)
	}
	got, _ := s.GetPaymentOrderByCode(ctx, order.Code)
	if got.Status != domain.OrderExpired {
		t.Fatalf("status = %q, want expired", got.Status)
	}
}
```

`MarkPaymentOrderPaid` is declared with `tx bun.Tx` but the test passes `nil`; make the signature `MarkPaymentOrderPaid(ctx context.Context, tx bun.Tx, orderID string, paidAt time.Time)` and inside use `db := bun.IDB(s.DB); if tx != nil { db = tx }`.

- [ ] **Step 2: Run it to verify it fails**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store -run 'TestCreateAndFetchPaymentOrder|TestOrderLifecycleGuards|TestExpirePaymentOrders' -v`
Expected: FAIL — `CreateOrderInput` undefined.

- [ ] **Step 3: Implement orders**

`internal/store/payments.go`:

```go
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
)

var (
	ErrOrderNotFound   = errors.New("payment order not found")
	ErrOrderNotPayable = errors.New("payment order is not payable")
)

type OrderDiscount struct {
	CouponID    string
	CouponCode  string
	Type        string
	Value       int64
	AmountMinor int64
	Priority    int
}

type CreateOrderInput struct {
	ProductID, PlanID, PriceID, Email, Kind string
	Quantity                                int
	SubtotalMinor, DiscountMinor            int64
	TaxMinor, TotalMinor                    int64
	Currency                                string
	CurrencyExponent                        int16
	TaxRateBP                               int
	Provider                                string
	ExpiresAt                               time.Time
	Buyer, Metadata                         []byte
	CreatedByKeyID                          string
	Discounts                               []OrderDiscount
}

// orderCode generates a code; a package var so a test can force a
// collision and pin the retry path.
var orderCode = domain.NewOrderCode

const orderCodeAttempts = 5

func (s *Store) CreatePaymentOrder(ctx context.Context, in CreateOrderInput) (*model.PaymentOrder, error) {
	var lastErr error
	for i := 0; i < orderCodeAttempts; i++ {
		order := &model.PaymentOrder{
			ID: NewID(), Code: orderCode(), ProductID: in.ProductID, PlanID: in.PlanID,
			PriceID: in.PriceID, Email: in.Email, Kind: in.Kind, Quantity: in.Quantity,
			SubtotalMinor: in.SubtotalMinor, DiscountMinor: in.DiscountMinor,
			TaxMinor: in.TaxMinor, TotalMinor: in.TotalMinor,
			Currency: in.Currency, CurrencyExp: in.CurrencyExponent, TaxRateBP: in.TaxRateBP,
			Provider: in.Provider, Status: domain.OrderPending, ExpiresAt: in.ExpiresAt,
			Buyer: in.Buyer, Metadata: in.Metadata, CreatedByKeyID: in.CreatedByKeyID,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		err := RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.NewInsert().Model(order).Exec(ctx); err != nil {
				return err
			}
			// numeric_code is a database identity column, so the insert
			// omits it and Bun does not read it back. Re-select the row
			// inside the same transaction to populate it — the PayOS
			// orderCode is this value, so a zero here breaks every link.
			if err := tx.NewSelect().Model(order).Where("id = ?", order.ID).Scan(ctx); err != nil {
				return err
			}
			for _, d := range in.Discounts {
				row := &model.PaymentOrderDiscount{
					ID: NewID(), OrderID: order.ID, CouponID: d.CouponID, CouponCode: d.CouponCode,
					Type: d.Type, Value: d.Value, AmountMinor: d.AmountMinor, Priority: d.Priority,
					CreatedAt: time.Now(),
				}
				if _, err := tx.NewInsert().Model(row).Exec(ctx); err != nil {
					return err
				}
			}
			return nil
		})
		if err == nil {
			return order, nil
		}
		// Only a unique violation on payment_orders.code means a code
		// collision; anything else is a real failure.
		if isUniqueViolation(err) {
			lastErr = err
			continue
		}
		return nil, err
	}
	return nil, fmt.Errorf("could not allocate an order code after %d tries: %w", orderCodeAttempts, lastErr)
}

func (s *Store) GetPaymentOrderByCode(ctx context.Context, code string) (*model.PaymentOrder, error) {
	order := new(model.PaymentOrder)
	err := s.DB.NewSelect().Model(order).Where("code = ?", code).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOrderNotFound
	}
	return order, err
}

func (s *Store) GetPaymentOrderByNumericCode(ctx context.Context, numeric int64) (*model.PaymentOrder, error) {
	order := new(model.PaymentOrder)
	err := s.DB.NewSelect().Model(order).Where("numeric_code = ?", numeric).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOrderNotFound
	}
	return order, err
}

func (s *Store) GetPaymentOrderForUpdate(ctx context.Context, tx bun.Tx, orderID string) (*model.PaymentOrder, error) {
	order := new(model.PaymentOrder)
	db := bun.IDB(s.DB)
	if tx != nil {
		db = tx
	}
	err := db.NewSelect().Model(order).Where("id = ?", orderID).For("UPDATE").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOrderNotFound
	}
	return order, err
}

func (s *Store) MarkPaymentOrderPaid(ctx context.Context, tx bun.Tx, orderID string, paidAt time.Time) (*model.PaymentOrder, error) {
	db := bun.IDB(s.DB)
	if tx != nil {
		db = tx
	}
	order, err := s.GetPaymentOrderForUpdate(ctx, tx, orderID)
	if err != nil {
		return nil, err
	}
	if domain.IsOrderTerminal(order.Status) {
		return nil, ErrOrderNotPayable
	}
	_, err = db.NewUpdate().Model((*model.PaymentOrder)(nil)).
		Set("status = ?", domain.OrderPaid).Set("paid_at = ?", paidAt).Set("updated_at = ?", time.Now()).
		Where("id = ?", orderID).Exec(ctx)
	if err != nil {
		return nil, err
	}
	order.Status, order.PaidAt = domain.OrderPaid, &paidAt
	return order, nil
}

func (s *Store) CancelPaymentOrder(ctx context.Context, code string) (*model.PaymentOrder, error) {
	order, err := s.GetPaymentOrderByCode(ctx, code)
	if err != nil {
		return nil, err
	}
	if order.Status != domain.OrderPending {
		return nil, ErrOrderNotPayable
	}
	_, err = s.DB.NewUpdate().Model((*model.PaymentOrder)(nil)).
		Set("status = ?", domain.OrderCanceled).Set("updated_at = ?", time.Now()).
		Where("id = ?", order.ID).Where("status = ?", domain.OrderPending).Exec(ctx)
	if err != nil {
		return nil, err
	}
	order.Status = domain.OrderCanceled
	return order, nil
}

func (s *Store) ExpirePaymentOrders(ctx context.Context, now time.Time) (int, error) {
	res, err := s.DB.NewUpdate().Model((*model.PaymentOrder)(nil)).
		Set("status = ?", domain.OrderExpired).Set("updated_at = ?", now).
		Where("status = ?", domain.OrderPending).Where("expires_at < ?", now).Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *Store) CreatePaymentAttempt(ctx context.Context, a *model.PaymentAttempt) error {
	if a.ID == "" {
		a.ID = NewID()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	_, err := s.DB.NewInsert().Model(a).Exec(ctx)
	return err
}

func (s *Store) GetLatestPaymentAttempt(ctx context.Context, orderID string) (*model.PaymentAttempt, error) {
	a := new(model.PaymentAttempt)
	err := s.DB.NewSelect().Model(a).
		Where("order_id = ?", orderID).Order("created_at DESC").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	return a, err
}

func (s *Store) UpdatePaymentAttemptStatus(ctx context.Context, id, status, providerRef string) error {
	_, err := s.DB.NewUpdate().Model((*model.PaymentAttempt)(nil)).
		Set("status = ?", status).
		Set("provider_ref = ?", providerRef).
		Set("updated_at = ?", time.Now()).
		Where("id = ?", id).Exec(ctx)
	return err
}
```

- [ ] **Step 4: Write the failing collision test**

```go
// Add to internal/store/payments_test.go.
func TestCreatePaymentOrderRetriesCodeCollision(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)

	codes := []string{"KGAAAAAA", "KGAAAAAA", "KGBBBBBB"}
	i := 0
	orig := store.OrderCodeGeneratorForTest() // add this helper in store/payments.go
	t.Cleanup(func() { store.SetOrderCodeGeneratorForTest(orig) })
	store.SetOrderCodeGeneratorForTest(func() string {
		c := codes[i]
		i++
		return c
	})
	first, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := s.CreatePaymentOrder(ctx, newOrderInput(productID, planID))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.Code != "KGAAAAAA" || second.Code != "KGBBBBBB" {
		t.Fatalf("first=%q second=%q", first.Code, second.Code)
	}
}
```

The `store.SetOrderCodeGeneratorForTest` helper is test-only surface; put it in `internal/store/payments_test_hooks.go` guarded by a doc comment:

```go
// SetOrderCodeGeneratorForTest swaps the order-code generator so a test
// can force collisions. Only tests call it; production never does.
func SetOrderCodeGeneratorForTest(fn func() string) { orderCode = fn }

func OrderCodeGeneratorForTest() func() string { return orderCode }
```

- [ ] **Step 5: Run the collision test to verify it fails**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store -run TestCreatePaymentOrderRetriesCodeCollision -v`
Expected: FAIL — `store.SetOrderCodeGeneratorForTest` undefined (add the helper first, then it passes).

- [ ] **Step 6: Run the full package and commit**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store -run 'TestCreatePaymentOrder|TestOrderLifecycleGuards|TestExpirePaymentOrders' -v && go vet ./internal/store`
Expected: PASS.

```bash
git add internal/store/payments.go internal/store/payments_test_hooks.go internal/store/payments_test.go
git commit -m "feat(payments): add order and attempt store with lifecycle guards"
```

---

### Task 6: SePay adapter — static QR, bank webhook, gateway signature, query client

**Files:**
- Create: `internal/payment/providers/sepay/qr.go`
- Create: `internal/payment/providers/sepay/webhook.go`
- Create: `internal/payment/providers/sepay/gateway.go`
- Create: `internal/payment/providers/sepay/client.go`
- Create: `internal/payment/providers/sepay/sepay.go` (`Provider` implementation wiring the above)
- Test: `internal/payment/providers/sepay/qr_test.go`, `webhook_test.go`, `gateway_test.go`

**Interfaces:**
- Consumes: `provider.Provider`, `provider.WebhookEvent`, `domain.IsOrderCode`, `domain.CurrencyExponent`.
- Produces:
  - `sepay.BuildQRImageURL(account, bank, amountMinor int64, description string) string`
  - `sepay.ParseBankWebhook(header http.Header, raw []byte, cfg BankWebhookConfig) (provider.WebhookEvent, error)`; `sepay.ErrWebhookUnauthorized`
  - `sepay.GatewayCanonical(fields map[string]string) string`, `sepay.SignGateway(secretKey, canonical string) string` (base64), `sepay.ParseIPN(header http.Header, raw []byte, ipnSecret string) (provider.WebhookEvent, error)`
  - `sepay.Client{BaseURL string; APIToken string; HTTP *http.Client}` with `TestConnection(ctx) error`, `ListTransactions(ctx, from, to time.Time, sinceID int64) ([]sepay.BankTransaction, error)`
  - `sepay.New(cfg Config) *sepay.Provider` where `Config{Sandbox bool; WebhookAuthMode string; APIKey string; IPNSecret string; BankAccount provider.BankAccount; APIToken string}`
  - `sepay.BankTransaction{ID int64; TransferType string; TransferAmount int64; Code, Content, Description, ReferenceCode string; TransactionDate time.Time}`

- [ ] **Step 1: Write the failing QR and webhook tests**

```go
// internal/payment/providers/sepay/qr_test.go
package sepay_test

import (
	"net/url"
	"testing"

	"github.com/tabloy/keygate/internal/payment/providers/sepay"
)

func TestBuildQRImageURL(t *testing.T) {
	raw := sepay.BuildQRImageURL("0978482700", "MB", 50000, "KG7F3K2Q")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Host != "vietqr.app" || u.Path != "/img" {
		t.Fatalf("url = %s", raw)
	}
	q := u.Query()
	if q.Get("acc") != "0978482700" || q.Get("bank") != "MB" || q.Get("amount") != "50000" {
		t.Fatalf("query = %v", q)
	}
	if q.Get("des") != "KG7F3K2Q" || q.Get("template") != "compact" {
		t.Fatalf("query = %v", q)
	}
	// A description with spaces/& must be escaped, not break the URL.
	raw = sepay.BuildQRImageURL("1", "MB", 1000, "KG7F3K2Q chuyen tien")
	u, err = url.Parse(raw)
	if err != nil || u.Query().Get("des") != "KG7F3K2Q chuyen tien" {
		t.Fatalf("escaped url = %s (%v)", raw, err)
	}
}
```

```go
// internal/payment/providers/sepay/webhook_test.go
package sepay_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/providers/sepay"
)

const sepayBody = `{"id":92704,"gateway":"MB Bank","transactionDate":"2026-11-12 15:04:05","accountNumber":"0978482700","subAccount":"","code":"KG7F3K2Q","content":"KG7F3K2Q chuyen tien","transferType":"in","description":"NGUYEN VAN A chuyen tien","transferAmount":1190000,"accumulated":105000000,"referenceCode":"FT24012345678"}`

func hmacHeader(secret, timestamp, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	_, _ = m.Write([]byte(timestamp + "." + body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestParseBankWebhookHMAC(t *testing.T) {
	secret := "whsec_test"
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	h := http.Header{}
	h.Set("X-SePay-Timestamp", ts)
	h.Set("X-SePay-Signature", hmacHeader(secret, ts, sepayBody))
	h.Set("X-SePay-Webhook-Id", "evt_1")

	ev, err := sepay.ParseBankWebhook(h, []byte(sepayBody), sepay.BankWebhookConfig{Mode: "hmac", Secret: secret, Now: time.Now()})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.OrderCode != "KG7F3K2Q" || ev.AmountMinor != 1190000 || ev.Currency != "VND" || ev.Status != domain.OrderPaid {
		t.Fatalf("event = %+v", ev)
	}
	if ev.ProviderRef != "92704" || ev.TransferType != "in" {
		t.Fatalf("ref/type = %q/%q", ev.ProviderRef, ev.TransferType)
	}
}

func TestParseBankWebhookRejectsBadSignatureAndSkew(t *testing.T) {
	secret := "whsec_test"
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	h := http.Header{}
	h.Set("X-SePay-Timestamp", ts)
	h.Set("X-SePay-Signature", hmacHeader("wrong", ts, sepayBody))

	if _, err := sepay.ParseBankWebhook(h, []byte(sepayBody), sepay.BankWebhookConfig{Mode: "hmac", Secret: secret}); err == nil {
		t.Fatal("bad signature accepted")
	}
	// A replayed body outside the ±300 s window is refused even with a
	// valid signature.
	old := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	h.Set("X-SePay-Timestamp", old)
	h.Set("X-SePay-Signature", hmacHeader(secret, old, sepayBody))
	if _, err := sepay.ParseBankWebhook(h, []byte(sepayBody), sepay.BankWebhookConfig{Mode: "hmac", Secret: secret}); err == nil {
		t.Fatal("stale timestamp accepted")
	}
}

func TestParseBankWebhookExtractsCodeFromContent(t *testing.T) {
	// `code` null (unlinked bank), the code only in the free-text memo.
	body := `{"id":1,"gateway":"MB","transactionDate":"2026-11-12 15:04:05","accountNumber":"0978482700","subAccount":"","code":null,"content":"ACME JSC chuyen tien KG7F3K2Q ca phe","transferType":"in","description":"x","transferAmount":50000,"accumulated":0,"referenceCode":"FT1"}`
	ev, err := sepay.ParseBankWebhook(http.Header{}, []byte(body), sepay.BankWebhookConfig{Mode: "none"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.OrderCode != "KG7F3K2Q" {
		t.Fatalf("order code = %q", ev.OrderCode)
	}
	// transferType=out is a refund/payout, never a payment.
	out := `{"id":2,"gateway":"MB","transactionDate":"2026-11-12 15:04:05","accountNumber":"0978482700","code":"KG7F3K2Q","content":"out","transferType":"out","description":"x","transferAmount":50000,"accumulated":0,"referenceCode":"FT2"}`
	ev, err = sepay.ParseBankWebhook(http.Header{}, []byte(out), sepay.BankWebhookConfig{Mode: "none"})
	if err != nil || ev.TransferType != "out" || ev.Status == domain.OrderPaid {
		t.Fatalf("out event = %+v, %v", ev, err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/payment/providers/sepay -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement qr.go and gateway.go**

```go
// internal/payment/providers/sepay/qr.go
package sepay

import (
	"net/url"
	"strconv"
)

// BuildQRImageURL returns the public VietQR image for a static transfer.
// It is built locally and never calls SePay: the QR is just an image URL
// the buyer's banking app decodes, and the payment is confirmed by the
// bank webhook, so the rail works with no provider API credentials.
func BuildQRImageURL(account, bank string, amountMinor int64, description string) string {
	q := url.Values{}
	q.Set("acc", account)
	q.Set("bank", bank)
	q.Set("amount", strconv.FormatInt(amountMinor, 10))
	q.Set("des", description)
	q.Set("template", "compact")
	return "https://vietqr.app/img?" + q.Encode()
}
```

```go
// internal/payment/providers/sepay/gateway.go
package sepay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// gatewayFieldOrder is the canonical order SePay's hosted-checkout
// signature requires (docs: "chữ ký" — fields in this exact order,
// empty and zero values dropped).
var gatewayFieldOrder = []string{
	"order_amount", "merchant", "currency", "operation", "order_description",
	"order_invoice_number", "customer_id", "payment_method", "success_url",
	"error_url", "cancel_url",
}

func GatewayCanonical(fields map[string]string) string {
	parts := make([]string, 0, len(gatewayFieldOrder))
	for _, key := range gatewayFieldOrder {
		v := fields[key]
		if v == "" || v == "0" {
			continue
		}
		parts = append(parts, key+"="+v)
	}
	return strings.Join(parts, ",")
}

// SignGateway returns base64(HMAC-SHA256(secret_key, canonical)) —
// base64, not hex, because SePay's hosted form expects this alphabet.
func SignGateway(secretKey, canonical string) string {
	m := hmac.New(sha256.New, []byte(secretKey))
	_, _ = m.Write([]byte(canonical))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// ParseIPN verifies X-Secret-Key and normalises the gateway
// notification. Only ORDER_PAID + CAPTURED + APPROVED counts as paid.
func ParseIPN(header http.Header, raw []byte, ipnSecret string) (provider.WebhookEvent, error) {
	if !hmac.Equal([]byte(header.Get("X-Secret-Key")), []byte(ipnSecret)) || ipnSecret == "" {
		return provider.WebhookEvent{}, ErrWebhookUnauthorized
	}
	var p struct {
		NotificationType string `json:"notification_type"`
		Order            struct {
			OrderInvoiceNumber string `json:"order_invoice_number"`
			OrderStatus        string `json:"order_status"`
			OrderAmount        string `json:"order_amount"`
			OrderCurrency      string `json:"order_currency"`
		} `json:"order"`
		Transaction struct {
			ID                string `json:"id"`
			TransactionStatus string `json:"transaction_status"`
			TransactionAmount string `json:"transaction_amount"`
		} `json:"transaction"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return provider.WebhookEvent{}, err
	}
	ev := provider.WebhookEvent{Provider: "sepay", ProviderRef: p.Transaction.ID,
		OrderCode: strings.TrimSpace(p.Order.OrderInvoiceNumber), TransferType: "in",
		Currency: p.Order.OrderCurrency, Raw: raw}
	if p.NotificationType == "ORDER_PAID" && p.Order.OrderStatus == "CAPTURED" && p.Transaction.TransactionStatus == "APPROVED" {
		ev.Status = domain.OrderPaid
	} else if p.NotificationType == "TRANSACTION_VOID" {
		ev.Status = "voided"
	} else {
		ev.Status = "pending"
	}
	// SePay sends money as "50000.00" strings; VND has no minor digits.
	ev.AmountMinor = parseSePayAmount(p.Transaction.TransactionAmount)
	if ev.AmountMinor == 0 {
		ev.AmountMinor = parseSePayAmount(p.Order.OrderAmount)
	}
	return ev, nil
}
```

Add the small helpers in `webhook.go` (same package): `parseSePayAmount` (strip `.00`, ParseInt), `ErrWebhookUnauthorized`, `BankWebhookConfig`.

- [ ] **Step 4: Write and run the failing webhook test — implement webhook.go**

Add to the same package, then run `go test ./internal/payment/providers/sepay -run TestParseBankWebhook -v`.

```go
// internal/payment/providers/sepay/webhook.go
package sepay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
)

var ErrWebhookUnauthorized = errors.New("sepay: webhook signature invalid")

type BankWebhookConfig struct {
	// Mode is the admin-configured auth mode: "hmac", "apikey", "none".
	Mode           string
	Secret         string // HMAC secret (mode=hmac)
	APIKey         string // Authorization: Apikey <key> (mode=apikey)
	AllowedCIDRs   []string
	Now            time.Time // test seam; zero means time.Now()
}

type bankWebhookPayload struct {
	ID              int64  `json:"id"`
	Gateway         string `json:"gateway"`
	TransactionDate string `json:"transactionDate"`
	AccountNumber   string `json:"accountNumber"`
	SubAccount      string `json:"subAccount"`
	Code            *string `json:"code"`
	Content         string `json:"content"`
	TransferType    string `json:"transferType"`
	Description     string `json:"description"`
	TransferAmount  int64  `json:"transferAmount"`
	Accumulated     int64  `json:"accumulated"`
	ReferenceCode   string `json:"referenceCode"`
}

// ParseBankWebhook verifies and normalises a SePay bank webhook. All
// three auth modes are supported because SePay's dashboard configures
// one of them per webhook; "none" is only acceptable for a test-mode
// config and is the caller's job to refuse in live mode.
func ParseBankWebhook(h http.Header, raw []byte, cfg BankWebhookConfig) (provider.WebhookEvent, error) {
	switch cfg.Mode {
	case "hmac":
		ts := h.Get("X-SePay-Timestamp")
		sig := strings.TrimPrefix(h.Get("X-SePay-Signature"), "sha256=")
		tsUnix, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			return provider.WebhookEvent{}, ErrWebhookUnauthorized
		}
		now := cfg.Now
		if now.IsZero() {
			now = time.Now()
		}
		skew := now.Sub(time.Unix(tsUnix, 0))
		if skew > 5*time.Minute || skew < -5*time.Minute {
			return provider.WebhookEvent{}, ErrWebhookUnauthorized
		}
		m := hmac.New(sha256.New, []byte(cfg.Secret))
		_, _ = m.Write([]byte(ts + "." + string(raw)))
		want := hex.EncodeToString(m.Sum(nil))
		if !hmac.Equal([]byte(strings.ToLower(sig)), []byte(want)) {
			return provider.WebhookEvent{}, ErrWebhookUnauthorized
		}
	case "apikey":
		got := strings.TrimPrefix(h.Get("Authorization"), "Apikey ")
		if cfg.APIKey == "" || !hmac.Equal([]byte(got), []byte(cfg.APIKey)) {
			return provider.WebhookEvent{}, ErrWebhookUnauthorized
		}
	case "none":
		// Test mode only; the caller refuses this when config.mode=live.
	default:
		return provider.WebhookEvent{}, ErrWebhookUnauthorized
	}

	var p bankWebhookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return provider.WebhookEvent{}, err
	}
	ev := provider.WebhookEvent{
		Provider: "sepay", ProviderEventID: strconv.FormatInt(p.ID, 10),
		ProviderRef: strconv.FormatInt(p.ID, 10), TransferType: p.TransferType,
		AmountMinor: p.TransferAmount, Currency: "VND", Raw: raw,
	}
	// transactionDate is VN local time with no offset.
	if ts, err := time.ParseInLocation("2006-01-02 15:04:05", p.TransactionDate, vnLocation); err == nil {
		ev.OccurredAt = ts
	}
	switch {
	case p.TransferType == "in":
		ev.Status = domain.OrderPaid
	case p.TransferType == "out":
		ev.Status = "out"
	}
	code := ""
	if p.Code != nil {
		code = strings.TrimSpace(*p.Code)
	}
	if !domain.IsOrderCode(code) {
		code = extractOrderCode(p.Content)
	}
	if !domain.IsOrderCode(code) {
		code = extractOrderCode(p.Description)
	}
	ev.OrderCode = code
	return ev, nil
}

var vnLocation = time.FixedZone("ICT", 7*3600)

// extractOrderCode finds the first KG + 6 Crockford chars as a token, so
// "KG7F3K2Q ca phe" matches but "XKG7F3K2Q" does not.
func extractOrderCode(s string) string {
	for i := 0; i+8 <= len(s); i++ {
		if i > 0 {
			prev := s[i-1]
			if prev >= 'A' && prev <= 'Z' || prev >= '0' && prev <= '9' {
				continue
			}
		}
		if code := s[i : i+8]; domain.IsOrderCode(code) {
			if i+8 == len(s) || !isCodeChar(s[i+8]) {
				return code
			}
		}
	}
	return ""
}

func isCodeChar(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func parseSePayAmount(s string) int64 {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
```

Add the missing `provider` import in gateway.go and `domain` import in gateway.go (the IPN switch references `domain.OrderPaid`).

- [ ] **Step 5: Write and run the failing gateway signature test**

```go
// internal/payment/providers/sepay/gateway_test.go
package sepay_test

import (
	"testing"

	"github.com/tabloy/keygate/internal/payment/providers/sepay"
)

func TestGatewayCanonicalDropsEmptyAndZero(t *testing.T) {
	got := sepay.GatewayCanonical(map[string]string{
		"order_amount": "50000", "merchant": "KGTEST", "currency": "VND",
		"operation": "PURCHASE", "order_description": "Goi Pro", "order_invoice_number": "KG7F3K2Q",
		"customer_id": "", "payment_method": "", "success_url": "https://x/s", "error_url": "", "cancel_url": "",
	})
	want := "order_amount=50000,merchant=KGTEST,currency=VND,operation=PURCHASE,order_description=Goi Pro,order_invoice_number=KG7F3K2Q,success_url=https://x/s"
	if got != want {
		t.Fatalf("canonical = %q, want %q", got, want)
	}
	if sig := sepay.SignGateway("secret", got); sig == "" || sig == got {
		t.Fatalf("signature = %q", sig)
	}
}
```

Run it: `go test ./internal/payment/providers/sepay -run TestGatewayCanonical -v` → FAIL until `gateway.go` imports are fixed; then PASS.

- [ ] **Step 6: Implement the API client and Provider**

`internal/payment/providers/sepay/client.go`:

```go
package sepay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type Client struct {
	BaseURL  string // https://userapi.sepay.vn/v2 (live), …-sandbox… in test
	APIToken string
	HTTP     *http.Client
}

type BankTransaction struct {
	ID              int64
	TransactionDate time.Time
	AccountNumber   string
	Code            string
	Content         string
	TransferType    string
	TransferAmount  int64
	ReferenceCode   string
}

func (c *Client) TestConnection(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/transactions?per_page=1", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sepay: test connection status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) ListTransactions(ctx context.Context, from, to time.Time, sinceID int64) ([]BankTransaction, error) {
	q := url.Values{}
	q.Set("transaction_date_from", from.Format("2006-01-02 15:04:05"))
	q.Set("transaction_date_to", to.Format("2006-01-02 15:04:05"))
	q.Set("transaction_date_sort", "asc")
	q.Set("timestamp_format", "iso8601")
	q.Set("per_page", "100")
	if sinceID > 0 {
		q.Set("since_id", strconv.FormatInt(sinceID, 10))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/transactions?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, ErrRateLimited
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sepay: list transactions status %d", resp.StatusCode)
	}
	var body struct {
		Transactions []struct {
			ID              int64  `json:"id"`
			TransactionDate string `json:"transaction_date"`
			AccountNumber   string `json:"account_number"`
			Code            string `json:"code"`
			Content         string `json:"content"`
			TransferType    string `json:"transfer_type"`
			TransferAmount  int64  `json:"transfer_amount"`
			ReferenceCode   string `json:"reference_code"`
		} `json:"transactions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make([]BankTransaction, 0, len(body.Transactions))
	for _, t := range body.Transactions {
		ts, _ := time.Parse(time.RFC3339, t.TransactionDate)
		out = append(out, BankTransaction{ID: t.ID, TransactionDate: ts, AccountNumber: t.AccountNumber,
			Code: t.Code, Content: t.Content, TransferType: t.TransferType,
			TransferAmount: t.TransferAmount, ReferenceCode: t.ReferenceCode})
	}
	return out, nil
}

var ErrRateLimited = errors.New("sepay: rate limited")

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}
```

`internal/payment/providers/sepay/sepay.go`:

```go
package sepay

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tabloy/keygate/internal/payment/provider"
)

// Config is one resolved provider config row.
type Config struct {
	Sandbox        bool
	WebhookAuthMode string // hmac | apikey | none
	APIKey         string
	IPNSecret      string
	WebhookSecret  string
	APIToken       string
	Bank           provider.BankAccount
	BankCode       string // short bank code used by the QR image host (MB, VCB, …)
}

type Provider struct {
	cfg     Config
	client  *Client
	now     func() time.Time
}

func New(cfg Config) *Provider {
	base := "https://userapi.sepay.vn/v2"
	if cfg.Sandbox {
		base = "https://userapi-sandbox.sepay.vn/v2"
	}
	return &Provider{cfg: cfg, client: &Client{BaseURL: base, APIToken: cfg.APIToken}, now: time.Now}
}

func (p *Provider) Name() string { return "sepay" }

// CreateAttempt produces the static QR instruction. No provider call is
// made: the bank webhook is the settlement source.
func (p *Provider) CreateAttempt(_ context.Context, req provider.CreateAttemptRequest) (provider.CreateAttemptResult, error) {
	desc := req.OrderCode
	return provider.CreateAttemptResult{
		ProviderRef: req.OrderCode,
		QRPayload:   BuildQRImageURL(p.cfg.Bank.AccountNo, p.cfg.BankCode, req.AmountMinor, desc),
		QRImageURL:  BuildQRImageURL(p.cfg.Bank.AccountNo, p.cfg.BankCode, req.AmountMinor, desc),
		Bank:        p.cfg.Bank,
		Description: desc,
		ExpiresAt:   req.ExpiresAt,
	}, nil
}

// CancelAttempt is a no-op: a bank transfer cannot be cancelled, the
// order simply expires.
func (p *Provider) CancelAttempt(context.Context, string) error { return nil }

func (p *Provider) ParseWebhook(r *http.Request, body []byte, secret []byte) (provider.WebhookEvent, error) {
	return ParseBankWebhook(r.Header, body, BankWebhookConfig{
		Mode: p.cfg.WebhookAuthMode, Secret: string(secret), APIKey: p.cfg.APIKey, Now: p.now(),
	})
}

func (p *Provider) TestConnection(ctx context.Context) error { return p.client.TestConnection(ctx) }

// ParseIPN and ListTransactions are used by the webhook route and the
// reconciler directly; they are not part of the Provider interface.
func (p *Provider) ParseIPN(r *http.Request, body []byte) (provider.WebhookEvent, error) {
	return ParseIPN(r.Header, body, p.cfg.IPNSecret)
}

func (p *Provider) ListTransactions(ctx context.Context, from, to time.Time, sinceID int64) ([]BankTransaction, error) {
	return p.client.ListTransactions(ctx, from, to, sinceID)
}

var _ provider.Provider = (*Provider)(nil)

// ErrMissingAPIKey keeps a bad config from surfacing as an opaque 401.
var ErrMissingAPIKey = errors.New("sepay: api token is not configured")
```

- [ ] **Step 7: Run the package and commit**

Run: `go test ./internal/payment/providers/sepay -v && go vet ./internal/payment/providers/sepay`
Expected: PASS.

```bash
git add internal/payment/providers/sepay
git commit -m "feat(payments): add SePay adapter (static QR, webhook, IPN, query client)"
```

---

### Task 7: PayOS adapter — payment link, webhook signature, query/cancel/confirm

**Files:**
- Create: `internal/payment/providers/payos/client.go`
- Create: `internal/payment/providers/payos/signature.go`
- Create: `internal/payment/providers/payos/webhook.go`
- Create: `internal/payment/providers/payos/payos.go`
- Test: `internal/payment/providers/payos/signature_test.go`, `webhook_test.go`

**Interfaces:**
- Consumes: `provider.Provider`, `provider.WebhookEvent`, `domain.OrderPaid`, `domain.OrderPending`.
- Produces:
  - `payos.SignCreateRequest(checksumKey string, p CreateRequest) string` (hex), `payos.VerifyDataSignature(checksumKey string, data map[string]any, signature string) bool`, `payos.SignWebhook(checksumKey string, data map[string]any) string`
  - `payos.CanonicalData(data map[string]any) string` (sorted `key=value`, null/undefined → "", arrays JSON-stringified)
  - `payos.Client{BaseURL, ClientID, APIKey, ChecksumKey, HTTP}` with `CreatePaymentLink`, `GetPaymentLink`, `CancelPaymentLink`, `ConfirmWebhook`
  - `payos.New(cfg Config) *payos.Provider`; `Config{ClientID, APIKey, ChecksumKey, WebhookSecret string}`
  - `payos.CreateRequest{OrderCode int64; Amount int64; Description string; BuyerName, BuyerEmail, BuyerPhone, BuyerAddress, BuyerTaxID string; Items []Item; CancelURL, ReturnURL string; ExpiredAt int64}`

- [ ] **Step 1: Write the failing signature tests with pinned vectors**

```go
// internal/payment/providers/payos/signature_test.go
package payos_test

import (
	"testing"

	"github.com/tabloy/keygate/internal/payment/providers/payos"
)

func TestSignCreateRequestPinned(t *testing.T) {
	// Vector computed from the documented canonical string
	// amount=…&cancelUrl=…&description=…&orderCode=…&returnUrl=… .
	req := payos.CreateRequest{
		OrderCode: 123, Amount: 1200000, Description: "KG7F3K2Q",
		CancelURL: "https://keygate.example.com/pay/KG7F3K2Q",
		ReturnURL: "https://keygate.example.com/pay/KG7F3K2Q",
	}
	got := payos.SignCreateRequest("checksum-key", req)
	want := "459f1d7f8ea39ccdd1927f95a478bdf304c7014bbbff1dac653388e2ff8c7c30"
	if got != want {
		t.Fatalf("signature = %s, want %s", got, want)
	}
}

func TestVerifyWebhookSignaturePinned(t *testing.T) {
	data := map[string]any{
		"orderCode": 123.0, "amount": 1200000.0, "description": "KG7F3K2Q",
		"accountNumber": "113366668888", "reference": "TF230204212323",
		"transactionDateTime": "2026-11-12 15:04:05", "currency": "VND",
		"paymentLinkId": "abc", "code": "00", "desc": "Thanh cong",
	}
	const want = "64f02615976df601952342f2aa8c1a669777c46d8cf792193cbb2683a6063d6b"
	if got := payos.SignWebhook("checksum-key", data); got != want {
		t.Fatalf("webhook signature = %s, want %s", got, want)
	}
	if !payos.VerifyDataSignature("checksum-key", data, want) {
		t.Fatal("pinned signature did not verify")
	}
	if payos.VerifyDataSignature("checksum-key", data, "deadbeef") {
		t.Fatal("bad signature verified")
	}
}

func TestCanonicalDataNullsAndArrays(t *testing.T) {
	got := payos.CanonicalData(map[string]any{
		"b": nil, "a": "x", "c": []any{"1", "2"}, "d": map[string]any{"k": "v"},
	})
	want := `a=x&b=&c=["1","2"]&d={"k":"v"}`
	if got != want {
		t.Fatalf("canonical = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/payment/providers/payos -run 'TestSign|TestVerify|TestCanonical' -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement signature.go**

```go
package payos

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
)

// CanonicalData is PayOS's webhook canonical string: keys sorted
// alphabetically, joined as key=value with '&'. Null, "null" and
// "undefined" render empty; objects and arrays are JSON-encoded.
func CanonicalData(data map[string]any) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+canonicalValue(data[k]))
	}
	return joinAmp(parts)
}

func canonicalValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		if t == "null" || t == "undefined" {
			return ""
		}
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func joinAmp(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "&" + p
	}
	return out
}

func SignWebhook(checksumKey string, data map[string]any) string {
	m := hmac.New(sha256.New, []byte(checksumKey))
	_, _ = m.Write([]byte(CanonicalData(data)))
	return hex.EncodeToString(m.Sum(nil))
}

func VerifyDataSignature(checksumKey string, data map[string]any, signature string) bool {
	want := SignWebhook(checksumKey, data)
	return hmac.Equal([]byte(want), []byte(signature))
}

// SignCreateRequest signs the create-link request. PayOS's canonical
// order is alphabetical: amount, cancelUrl, description, orderCode,
// returnUrl; values are raw (not quoted).
func SignCreateRequest(checksumKey string, r CreateRequest) string {
	canonical := "amount=" + strconv.FormatInt(r.Amount, 10) +
		"&cancelUrl=" + r.CancelURL +
		"&description=" + r.Description +
		"&orderCode=" + strconv.FormatInt(r.OrderCode, 10) +
		"&returnUrl=" + r.ReturnURL
	m := hmac.New(sha256.New, []byte(checksumKey))
	_, _ = m.Write([]byte(canonical))
	return hex.EncodeToString(m.Sum(nil))
}
```

- [ ] **Step 4: Implement webhook.go**

```go
package payos

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
)

var ErrWebhookUnauthorized = errors.New("payos: webhook signature invalid")

type webhookPayload struct {
	Code      string         `json:"code"`
	Desc      string         `json:"desc"`
	Success   bool           `json:"success"`
	Data      map[string]any `json:"data"`
	Signature string         `json:"signature"`
}

// ParseWebhook verifies the checksum and maps PayOS's status onto the
// normalised event. Only code=00 + success + data.code=00 is a payment;
// UNDERPAID stays pending and is recorded for an operator.
func ParseWebhook(h http.Header, raw []byte, checksumKey string) (provider.WebhookEvent, error) {
	var p webhookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return provider.WebhookEvent{}, err
	}
	if p.Signature == "" || !VerifyDataSignature(checksumKey, p.Data, p.Signature) {
		return provider.WebhookEvent{}, ErrWebhookUnauthorized
	}
	ev := provider.WebhookEvent{Provider: "payos", Raw: raw, TransferType: "in"}
	if v, ok := p.Data["orderCode"].(float64); ok {
		ev.NumericCode = int64(v)
	}
	ev.OrderCode = stringField(p.Data, "description")
	ev.ProviderRef = stringField(p.Data, "paymentLinkId")
	ev.Currency = stringField(p.Data, "currency")
	if v, ok := p.Data["amount"].(float64); ok {
		ev.AmountMinor = int64(v)
	}
	if ts := stringField(p.Data, "transactionDateTime"); ts != "" {
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", ts, time.FixedZone("ICT", 7*3600)); err == nil {
			ev.OccurredAt = t
		}
	}
	dataCode := stringField(p.Data, "code")
	switch {
	case p.Code == "00" && p.Success && dataCode == "00":
		ev.Status = domain.OrderPaid
	case dataCode == "UNDERPAID":
		ev.Status = "partial"
	case dataCode == "EXPIRED":
		ev.Status = "expired"
	case dataCode == "CANCELLED":
		ev.Status = "canceled"
	default:
		ev.Status = "pending"
	}
	return ev, nil
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}


```

- [ ] **Step 5: Write the webhook parsing test**

```go
// internal/payment/providers/payos/webhook_test.go
package payos_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/providers/payos"
)

func TestParseWebhookPaid(t *testing.T) {
	data := map[string]any{
		"orderCode": 123.0, "amount": 1200000.0, "description": "KG7F3K2Q",
		"currency": "VND", "paymentLinkId": "abc", "code": "00",
		"transactionDateTime": "2026-11-12 15:04:05",
	}
	body, _ := json.Marshal(map[string]any{"code": "00", "desc": "success", "success": true, "data": data,
		"signature": payos.SignWebhook("checksum-key", data)})
	ev, err := payos.ParseWebhook(http.Header{}, body, "checksum-key")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Status != domain.OrderPaid || ev.OrderCode != "KG7F3K2Q" || ev.NumericCode != 123 || ev.AmountMinor != 1200000 {
		t.Fatalf("event = %+v", ev)
	}
}

func TestParseWebhookRejectsBadSignature(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"code": "00", "success": true,
		"data": map[string]any{"orderCode": 1.0}, "signature": "deadbeef"})
	if _, err := payos.ParseWebhook(http.Header{}, body, "checksum-key"); err == nil {
		t.Fatal("bad signature accepted")
	}
}
```

- [ ] **Step 6: Implement the client and Provider**

`internal/payment/providers/payos/client.go`:

```go
package payos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const defaultBaseURL = "https://api-merchant.payos.vn"

type Item struct {
	Name          string `json:"name"`
	Quantity      int    `json:"quantity"`
	Price         int64  `json:"price"`
	TaxPercentage int    `json:"taxPercentage,omitempty"`
}

type CreateRequest struct {
	OrderCode   int64
	Amount      int64
	Description string
	BuyerName   string
	BuyerEmail  string
	BuyerPhone  string
	BuyerAddress string
	BuyerTaxID  string
	Items       []Item
	CancelURL   string
	ReturnURL   string
	ExpiredAt   int64 // unix seconds; must be <= 2038-01-19
}

type PaymentLink struct {
	CheckoutURL   string
	QRCode        string
	PaymentLinkID string
	Status        string
	Amount        int64
	ExpiredAt     int64
}

type Client struct {
	BaseURL     string
	ClientID    string
	APIKey      string
	ChecksumKey string
	HTTP        *http.Client
}

func (c *Client) CreatePaymentLink(ctx context.Context, r CreateRequest) (*PaymentLink, error) {
	body := map[string]any{
		"orderCode": r.OrderCode, "amount": r.Amount, "description": r.Description,
		"cancelUrl": r.CancelURL, "returnUrl": r.ReturnURL,
		"signature": SignCreateRequest(c.ChecksumKey, r),
	}
	if r.ExpiredAt > 0 {
		body["expiredAt"] = r.ExpiredAt
	}
	if r.BuyerName != "" {
		body["buyerName"] = r.BuyerName
	}
	if r.BuyerEmail != "" {
		body["buyerEmail"] = r.BuyerEmail
	}
	if r.BuyerPhone != "" {
		body["buyerPhone"] = r.BuyerPhone
	}
	if r.BuyerAddress != "" {
		body["buyerAddress"] = r.BuyerAddress
	}
	if r.BuyerTaxID != "" {
		body["buyerTaxCode"] = r.BuyerTaxID
	}
	if len(r.Items) > 0 {
		body["items"] = r.Items
	}
	var out struct {
		Code string `json:"code"`
		Desc string `json:"desc"`
		Data *struct {
			CheckoutURL   string `json:"checkoutUrl"`
			QRCode        string `json:"qrCode"`
			PaymentLinkID string `json:"paymentLinkId"`
			Status        string `json:"status"`
			Amount        int64  `json:"amount"`
			ExpiredAt     int64  `json:"expiredAt"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/v2/payment-requests", body, &out); err != nil {
		return nil, err
	}
	if out.Code != "00" || out.Data == nil {
		// "Đơn thanh toán đã tồn tại" means a previous create succeeded;
		// the caller retries by fetching the link with the orderCode.
		return nil, fmt.Errorf("payos: create link code %s: %s", out.Code, out.Desc)
	}
	return &PaymentLink{CheckoutURL: out.Data.CheckoutURL, QRCode: out.Data.QRCode,
		PaymentLinkID: out.Data.PaymentLinkID, Status: out.Data.Status,
		Amount: out.Data.Amount, ExpiredAt: out.Data.ExpiredAt}, nil
}

func (c *Client) GetPaymentLink(ctx context.Context, orderCode int64) (*PaymentLink, error) {
	var out struct {
		Code string `json:"code"`
		Desc string `json:"desc"`
		Data *struct {
			CheckoutURL   string `json:"checkoutUrl"`
			QRCode        string `json:"qrCode"`
			PaymentLinkID string `json:"paymentLinkId"`
			Status        string `json:"status"`
			Amount        int64  `json:"amount"`
			ExpiredAt     int64  `json:"expiredAt"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/v2/payment-requests/%d", orderCode), nil, &out); err != nil {
		return nil, err
	}
	if out.Data == nil {
		return nil, fmt.Errorf("payos: get link code %s: %s", out.Code, out.Desc)
	}
	return &PaymentLink{CheckoutURL: out.Data.CheckoutURL, QRCode: out.Data.QRCode,
		PaymentLinkID: out.Data.PaymentLinkID, Status: out.Data.Status,
		Amount: out.Data.Amount, ExpiredAt: out.Data.ExpiredAt}, nil
}

func (c *Client) CancelPaymentLink(ctx context.Context, orderCode int64, reason string) error {
	body := map[string]any{"cancellationReason": reason}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/v2/payment-requests/%d/cancel", orderCode), body, nil)
}

func (c *Client) ConfirmWebhook(ctx context.Context, webhookURL string) error {
	return c.do(ctx, http.MethodPost, "/confirm-webhook", map[string]any{"webhookUrl": webhookURL}, nil)
}

func (c *Client) TestConnection(ctx context.Context) error {
	// Querying a non-existent order still proves the credentials: a
	// 401 means bad keys, and PayOS's "not found" code means they work.
	if _, err := c.GetPaymentLink(ctx, 1); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr *bytes.Reader
	if body == nil {
		rdr = bytes.NewReader(nil)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("x-client-id", c.ClientID)
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return ErrRateLimited
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("payos: server error %d", resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

var ErrRateLimited = fmt.Errorf("payos: rate limited")
```

Add `isNotFound(err)` and a typed API error in the same file:

```go
type APIError struct{ Code, Desc string }

func (e *APIError) Error() string { return "payos: " + e.Code + ": " + e.Desc }

func isNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == "01"
}
```

Have `GetPaymentLink`/`CreatePaymentLink` return `&APIError{Code: out.Code, Desc: out.Desc}` instead of `fmt.Errorf` when the code is not `00`, so the test connection can distinguish auth failures from a missing order.

`internal/payment/providers/payos/payos.go`:

```go
package payos

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tabloy/keygate/internal/payment/provider"
)

type Config struct {
	ClientID      string
	APIKey        string
	ChecksumKey   string
	WebhookSecret string // not used by PayOS webhooks (checksum key is), kept for the admin shape
	BaseURL       string
}

type Provider struct{ client *Client }

func New(cfg Config) *Provider {
	return &Provider{client: &Client{BaseURL: cfg.BaseURL, ClientID: cfg.ClientID,
		APIKey: cfg.APIKey, ChecksumKey: cfg.ChecksumKey}}
}

func (p *Provider) Name() string { return "payos" }

func (p *Provider) CreateAttempt(ctx context.Context, req provider.CreateAttemptRequest) (provider.CreateAttemptResult, error) {
	expiresAt := req.ExpiresAt
	if expiresAt.After(time.Date(2038, 1, 19, 0, 0, 0, 0, time.UTC)) {
		expiresAt = time.Date(2038, 1, 19, 0, 0, 0, 0, time.UTC)
	}
	buyer := req.Buyer
	link, err := p.client.CreatePaymentLink(ctx, CreateRequest{
		OrderCode: req.NumericCode, Amount: req.AmountMinor, Description: req.Description,
		BuyerName: buyer.Name, BuyerEmail: buyer.Email, BuyerPhone: buyer.Phone,
		BuyerAddress: buyer.Address, BuyerTaxID: buyer.TaxID,
		CancelURL: req.CancelURL, ReturnURL: req.ReturnURL, ExpiredAt: expiresAt.Unix(),
	})
	if err != nil {
		return provider.CreateAttemptResult{}, err
	}
	return provider.CreateAttemptResult{
		ProviderRef: link.PaymentLinkID, CheckoutURL: link.CheckoutURL,
		QRPayload: link.QRCode, ExpiresAt: expiresAt,
	}, nil
}

func (p *Provider) CancelAttempt(ctx context.Context, ref string) error {
	n, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return err
	}
	return p.client.CancelPaymentLink(ctx, n, "Cancelled by merchant")
}

func (p *Provider) ParseWebhook(r *http.Request, body []byte, secret []byte) (provider.WebhookEvent, error) {
	return ParseWebhook(r.Header, body, p.client.ChecksumKey)
}

func (p *Provider) TestConnection(ctx context.Context) error { return p.client.TestConnection(ctx) }

func (p *Provider) ConfirmWebhook(ctx context.Context, url string) error {
	return p.client.ConfirmWebhook(ctx, url)
}

var _ provider.Provider = (*Provider)(nil)

var ErrNotConfigured = errors.New("payos: credentials are not configured")
```

`CancelAttempt` for PayOS needs the numeric order code, but the interface passes `ProviderRef` (paymentLinkId). Change the attempts row to store both `provider_ref` and `numeric_code` (already on order): implement cancel as a no-op VN-cancel (order expires) in phase 1a and let expiry handle unpaid links — PayOS expires links itself with `expiredAt`. Replace `CancelAttempt` body with:

```go
// CancelAttempt is a no-op in phase 1a: the link expires with the order
// (expiredAt), and cancelling by paymentLinkId is not part of PayOS's
// API — the call needs the numeric orderCode, which the order row owns.
func (p *Provider) CancelAttempt(context.Context, string) error { return nil }
```

- [ ] **Step 7: Run the package and commit**

Run: `go test ./internal/payment/providers/payos -v && go vet ./internal/payment/providers/payos`
Expected: PASS.

```bash
git add internal/payment/providers/payos
git commit -m "feat(payments): add PayOS adapter (link, webhook signature, confirm, query)"
```

---

### Task 8: Webhook ingestion — verify, dedupe, ack, emit events

**Files:**
- Create: `internal/store/payment_events.go`
- Create: `internal/handler/payments_webhook.go`
- Modify: `internal/model/model.go` (add payment event constants + `WebhookEvents`)
- Modify: `cmd/server/main.go` (construct handlers, register raw-body routes)
- Test: `internal/store/payment_events_test.go`, `internal/handler/payments_webhook_test.go`

**Interfaces:**
- Consumes: `sepay.ParseBankWebhook`, `payos.ParseWebhook`, `providercfg.Service.Resolve`, `settlement.Service` (T9), `service.WebhookService.Dispatch`.
- Produces:
  - `store.RecordPaymentEvent(ctx, tx bun.Tx, in PaymentEventInput) (inserted bool, err error)` with `PaymentEventInput{Provider, EventID, OrderID, Kind, Raw []byte, OccurredAt time.Time}`; `store.ErrDuplicateEvent` not used — duplicates return `inserted=false`.
  - `handler.PaymentsWebhookHandler{Store, Config *providercfg.Service, Settlement *settlement.Service, Log}` with `HandleSePay`, `HandleSePayIPN`, `HandlePayOS`.
  - Event constants `model.EventOrderCreated`, `model.EventOrderPaid`, `model.EventPaymentSucceeded`, `model.EventPaymentFailed` — exactly the four the spec puts in phase 1a; `order.expired/canceled/refunded` belong to phase 1b.

- [ ] **Step 1: Add the event constants and their test**

```go
// internal/model/webhook_events_test.go (new file)
package model_test

import (
	"testing"

	"github.com/tabloy/keygate/internal/model"
)

func TestPaymentWebhookEventsRegistered(t *testing.T) {
	for _, e := range []string{
		model.EventOrderCreated, model.EventOrderPaid,
		model.EventPaymentSucceeded, model.EventPaymentFailed,
	} {
		if !model.IsValidWebhookEvent(e) {
			t.Fatalf("%q is not in WebhookEvents", e)
		}
	}
}
```

In `internal/model/model.go`, add to the event const block:

```go
	EventOrderCreated     = "order.created"
	EventOrderPaid        = "order.paid"
	EventPaymentSucceeded = "payment.succeeded"
	EventPaymentFailed    = "payment.failed"
```

and append them to `WebhookEvents`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/model -run TestPaymentWebhookEventsRegistered -v`
Expected: FAIL — undefined constants.

- [ ] **Step 3: Implement store/payment_events.go**

```go
package store

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
)

type PaymentEventInput struct {
	Provider   string
	EventID    string
	OrderID    string
	Kind       string
	Raw        []byte
	OccurredAt time.Time
}

// RecordPaymentEvent inserts the provider event id once. A duplicate
// (replay, retry, two replicas) returns inserted=false and the caller
// acks without settling again. The unique key is (provider, event_id);
// an empty event id is never allowed because it would dedupe nothing.
func (s *Store) RecordPaymentEvent(ctx context.Context, tx bun.Tx, in PaymentEventInput) (bool, error) {
	if in.EventID == "" {
		return false, fmt.Errorf("payment event id is required")
	}
	db := bun.IDB(s.DB)
	if tx != nil {
		db = tx
	}
	row := &model.PaymentEvent{ID: NewID(), Provider: in.Provider, EventID: in.EventID,
		OrderID: in.OrderID, Kind: in.Kind, Raw: in.Raw, OccurredAt: in.OccurredAt, CreatedAt: time.Now()}
	res, err := db.NewInsert().Model(row).On("CONFLICT (provider, event_id) DO NOTHING").Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
```

Add `fmt` to imports; `model.PaymentEvent` gets its columns from T1 (`provider`, `event_id`, `order_id`, `kind`, `raw`, `occurred_at`, `created_at`).

- [ ] **Step 4: Write the failing webhook tests**

```go
// internal/handler/payments_webhook_test.go
package handler

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/payment/providercfg"
)

func newSePayWebhookFixture(t *testing.T) (*apiFixture, *PaymentsWebhookHandler, string) {
	t.Helper()
	f := newAPIFixture(t)
	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x31}, 32))
	if err != nil {
		t.Fatal(err)
	}
	svc := providercfg.NewService(f.s, codec, "live")
	if _, err := svc.Upsert(f.ctx, providercfg.UpsertInput{
		Provider: "sepay", Mode: "live", Enabled: true,
		Config:        providercfg.SepayConfig{BankName: "MB Bank", BankBIN: "970422", AccountNo: "0978482700", AccountName: "NGUYEN VAN A"},
		Credentials:   providercfg.SepayCredentials{APIToken: "token"},
		WebhookSecret: "whsec_test",
	}); err != nil {
		t.Fatal(err)
	}
	h := &PaymentsWebhookHandler{Store: f.s, Config: svc, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return f, h, "whsec_test"
}

func TestSePayWebhookCreatesOrderPaymentOnce(t *testing.T) {
	f, h, secret := newSePayWebhookFixture(t)
	// An order to match.
	order, err := f.s.CreatePaymentOrder(f.ctx, store.CreateOrderInput{
		ProductID: f.prodA.ID, PlanID: f.planA.ID, Email: "buyer@example.com", Kind: "new", Quantity: 1,
		SubtotalMinor: 1190000, TotalMinor: 1190000, Currency: "VND", Provider: "sepay",
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"id":92704,"gateway":"MB","transactionDate":"2026-11-12 15:04:05","accountNumber":"0978482700","subAccount":"","code":"` + order.Code + `","content":"` + order.Code + ` chuyen tien","transferType":"in","description":"x","transferAmount":1190000,"accumulated":0,"referenceCode":"FT1"}`
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(ts + "." + body))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	call := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/api/v1/webhook/sepay", bytes.NewReader([]byte(body)))
		c.Request.Header.Set("X-SePay-Timestamp", ts)
		c.Request.Header.Set("X-SePay-Signature", sig)
		h.HandleSePay(c)
		return w
	}
	first := call()
	if first.Code != http.StatusOK || !bytes.Contains(first.Body.Bytes(), []byte(`"success":true`)) {
		t.Fatalf("first ack = %d %s", first.Code, first.Body.String())
	}
	second := call() // provider retry: same id
	if second.Code != http.StatusOK {
		t.Fatalf("retry ack = %d %s", second.Code, second.Body.String())
	}
	got, _ := f.s.GetPaymentOrderByCode(f.ctx, order.Code)
	if got.Status != domain.OrderPaid {
		t.Fatalf("order status = %q, want paid", got.Status)
	}
	// Exactly one licence for this order.
	n, err := f.s.DB.NewSelect().Table("licenses").Where("email = ?", "buyer@example.com").Count(f.ctx)
	if err != nil || n != 1 {
		t.Fatalf("licences = %d, %v", n, err)
	}
}

func TestSePayWebhookBadSignatureIs401(t *testing.T) {
	_, h, _ := newSePayWebhookFixture(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/api/v1/webhook/sepay", bytes.NewReader([]byte(`{"id":1}`)))
	c.Request.Header.Set("X-SePay-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	c.Request.Header.Set("X-SePay-Signature", "sha256=deadbeef")
	h.HandleSePay(c)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}
```

- [ ] **Step 5: Run to verify it fails**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/handler -run 'TestSePayWebhook' -v`
Expected: FAIL — `PaymentsWebhookHandler` undefined.

- [ ] **Step 6: Implement handler/payments_webhook.go**

```go
package handler

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/providercfg"
	"github.com/tabloy/keygate/internal/payment/providers/payos"
	"github.com/tabloy/keygate/internal/payment/providers/sepay"
	"github.com/tabloy/keygate/internal/payment/settlement"
	"github.com/tabloy/keygate/internal/store"
)

type PaymentsWebhookHandler struct {
	Store      *store.Store
	Config     *providercfg.Service
	Settlement *settlement.Service
	Log        *slog.Logger
}

const maxWebhookBody = 1 << 20

func readRawBody(c *gin.Context) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxWebhookBody))
	if err != nil {
		c.Status(http.StatusBadRequest)
		return nil, false
	}
	return body, true
}

// ackSePay is exactly the body SePay requires; anything else reads as a
// failed delivery and is retried for ~33 minutes.
func ackSePay(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"success": true}) }

// peekOrderCode reads an order code out of a payload that has not been
// verified yet. It is used only to choose which config row verifies
// the signature, never to decide that money arrived.
func peekReference(body []byte) (code string, numeric int64) {
	var v struct {
		Code        *string `json:"code"`
		Content     string  `json:"content"`
		Description string  `json:"description"`
		Data        struct {
			Description string `json:"description"`
			OrderCode   int64  `json:"orderCode"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &v) != nil {
		return "", 0
	}
	if v.Code != nil && domain.IsOrderCode(strings.TrimSpace(*v.Code)) {
		return strings.TrimSpace(*v.Code), 0
	}
	if domain.IsOrderCode(strings.TrimSpace(v.Data.Description)) {
		return strings.TrimSpace(v.Data.Description), v.Data.OrderCode
	}
	if v.Data.OrderCode != 0 {
		return "", v.Data.OrderCode
	}
	return extractCodeFromText(v.Content), 0
}

func (h *PaymentsWebhookHandler) HandleSePay(c *gin.Context) {
	body, ok := readRawBody(c)
	if !ok {
		return
	}
	code, numeric := peekReference(body)
	order := h.findOrder(c, code, numeric)
	cfg, err := h.configFor(c, order, "sepay")
	if err != nil {
		h.Log.Error("sepay webhook: no provider config", "error", err)
		c.Status(http.StatusServiceUnavailable)
		return
	}
	ev, err := sepay.ParseBankWebhook(c.Request.Header, body, sepay.BankWebhookConfig{
		Mode: cfg.WebhookAuthMode, Secret: cfg.WebhookSecret, APIKey: cfg.APIToken,
	})
	if err != nil {
		c.Status(http.StatusUnauthorized)
		return
	}
	if ev.TransferType == "out" {
		// Refund/payout: record it, never settle an order from it.
		_ = h.recordTransaction(c, ev, order, domain.TxUnmatched)
		ackSePay(c)
		return
	}
	h.ingest(c, "sepay", ev, order)
}

func (h *PaymentsWebhookHandler) HandlePayOS(c *gin.Context) {
	body, ok := readRawBody(c)
	if !ok {
		return
	}
	code, numeric := peekReference(body)
	order := h.findOrder(c, code, numeric)
	cfg, err := h.configFor(c, order, "payos")
	if err != nil {
		h.Log.Error("payos webhook: no provider config", "error", err)
		c.Status(http.StatusServiceUnavailable)
		return
	}
	ev, err := payos.ParseWebhook(c.Request.Header, body, cfg.ChecksumKey)
	if err != nil {
		c.Status(http.StatusUnauthorized)
		return
	}
	h.ingest(c, "payos", ev, order)
	c.Status(http.StatusOK)
}
```

The `ingest` helper is the one settlement entry point:

```go
func (h *PaymentsWebhookHandler) ingest(c *gin.Context, provider string, ev provider.WebhookEvent, order *model.PaymentOrder) {
	// A webhook without a matching order is normal: a manual transfer
	// with the code typed wrong. Record it for the operator instead of
	// 500-looping the provider.
	if order == nil {
		if err := h.Settlement.RecordUnmatched(c, provider, ev, nil); err != nil {
			h.Log.Error("record unmatched transaction", "provider", provider, "error", err)
			c.Status(http.StatusInternalServerError)
			return
		}
		ackSePay(c)
		return
	}
	inserted, err := h.Store.RecordPaymentEvent(c, nil, store.PaymentEventInput{
		Provider: provider, EventID: ev.ProviderEventID, OrderID: order.ID, Kind: ev.Status, Raw: ev.Raw,
	})
	if err != nil {
		h.Log.Error("record payment event", "error", err)
		c.Status(http.StatusInternalServerError)
		return
	}
	if !inserted {
		ackSePay(c) // duplicate: already handled
		return
	}
	if err := h.Settlement.Settle(c, order.ID, provider, ev); err != nil {
		h.Log.Error("settle order", "order", order.Code, "error", err)
		c.Status(http.StatusInternalServerError)
		return
	}
	ackSePay(c)
}
```

Implement `configFor` (order product override → default), `findOrder` (by code then numeric), `recordTransaction` (insert candidate row) in the same file. `configFor`:

```go
func (h *PaymentsWebhookHandler) configFor(c *gin.Context, order *model.PaymentOrder, provider string) (*providercfg.ResolvedConfig, error) {
	productID := ""
	if order != nil {
		productID = order.ProductID
	}
	return h.Config.Resolve(c, productID, provider, "")
}
```

`findOrder`:

```go
func (h *PaymentsWebhookHandler) findOrder(c *gin.Context, code string, numeric int64) *model.PaymentOrder {
	if code != "" {
		if o, err := h.Store.GetPaymentOrderByCode(c, code); err == nil {
			return o
		}
	}
	if numeric != 0 {
		if o, err := h.Store.GetPaymentOrderByNumericCode(c, numeric); err == nil {
			return o
		}
	}
	return nil
}
```

`HandleSePayIPN` mirrors `HandleSePay` with `sepay.ParseIPN` and the IPN config (uses `X-Secret-Key`), acking `200` with an empty body — the same ingest path.

- [ ] **Step 7: Register routes**

In `cmd/server/main.go`, register outside the envelope-wrapped groups:

```go
sepayH := &handler.PaymentsWebhookHandler{Store: db, Config: providerSvc, Settlement: settleSvc, Log: logger}
v1.POST("/webhook/sepay", middleware.RateLimitByIPScoped("sepay_webhook", 120, time.Minute), sepayH.HandleSePay)
v1.POST("/webhook/sepay-ipn", middleware.RateLimitByIPScoped("sepay_ipn", 120, time.Minute), sepayH.HandleSePayIPN)
v1.POST("/webhook/payos", middleware.RateLimitByIPScoped("payos_webhook", 120, time.Minute), sepayH.HandlePayOS)
```

Use the existing IP allowlist middleware only if it is already wired for webhooks; SePay's allowlist is optional (`config.ip_allowlist_enabled`) and is checked inside `ParseBankWebhook` via `AllowedCIDRs`.

- [ ] **Step 8: Emit outbound events**

In settlement (T9), after a successful grant call:

```go
s.Webhooks.Dispatch(ctx, order.ProductID, model.EventPaymentSucceeded, map[string]any{
	"order": order, "provider": provider, "event": event,
})
s.Webhooks.Dispatch(ctx, order.ProductID, model.EventOrderPaid, map[string]any{"order": order})
```

and on an expired/canceled order + a payment that arrives afterwards, record the transaction only (no event). Add `Webhooks *service.WebhookService` to `settlement.Service`.

- [ ] **Step 9: Run tests and commit**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/handler -run 'TestSePayWebhook|TestPayOSWebhook' -v && go test ./internal/model -run TestPaymentWebhookEventsRegistered -v`
Expected: PASS.

```bash
git add internal/store/payment_events.go internal/handler/payments_webhook.go internal/handler/payments_webhook_test.go internal/model/model.go internal/model/webhook_events_test.go cmd/server/main.go
git commit -m "feat(payments): verify and ingest provider webhooks with dedupe and events"
```

---

### Task 9: Settlement — turn a paid order into a licence (new and renewal)

**Files:**
- Create: `internal/payment/settlement/service.go`
- Create: `internal/payment/settlement/build.go`
- Test: `internal/payment/settlement/settle_new_test.go`, `settle_renewal_test.go`, `settle_guard_test.go`

**Interfaces:**
- Consumes: `store.CreateLicenseWithSubscription`, `store.NewLicenseKey`, `store.MarkPaymentOrderPaid`, `store.RecordPaymentEvent`, `store.InsertPaymentTransaction` (add in this task), `service.WebhookService`; reuses `store.ApplyLicenseRenewalIn` for perpetual renewals.
- Produces:
  - `settlement.Service{Store *store.Store; Webhooks *service.WebhookService; Log *slog.Logger}`
  - `(*Service).Settle(ctx context.Context, orderID, provider string, ev provider.WebhookEvent) error`
  - `(*Service).RecordUnmatched(ctx, provider string, ev provider.WebhookEvent, order *model.PaymentOrder) error`
  - `store.InsertPaymentTransaction(ctx, tx bun.Tx, in PaymentTransactionInput) (*model.PaymentTransaction, error)` + `store.MarkTransactionMatched(ctx, id, orderID) error`
  - `store.ExtendLicenseFromOrderIn(ctx context.Context, tx bun.IDB, order *model.PaymentOrder, plan *model.Plan) error` — extends `valid_until` for a subscription/trial licence or the maintenance window for a perpetual one, writes the `license_renewals` ledger row keyed by `payment_order_id`, and updates `subscriptions.current_period_*` when a subscription row exists.
  - `buildNewLicense(ctx, s, order) (*model.License, *model.Plan, error)` in `build.go`
  - `settlement.ErrOrderAlreadySettled` (returned as nil semantics: Settle is idempotent and returns nil when paid already).

- [ ] **Step 1: Write the failing new-order settlement test**

```go
// internal/payment/settlement/settle_new_test.go
package settlement_test

import (
	"context"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/settlement"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

func seedSellable(t *testing.T, s *store.Store) (productID, planID string) {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	p := &model.Product{Name: "SettleCo", Slug: "settle-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, p); err != nil {
		t.Fatal(err)
	}
	pl := &model.Plan{ProductID: p.ID, Name: "Pro", Slug: "settle-pro-" + suffix,
		LicenseType: "perpetual", MaxActivations: 3, Active: true}
	if err := s.CreatePlan(ctx, pl); err != nil {
		t.Fatal(err)
	}
	return p.ID, pl.ID
}

func TestSettleNewOrderGrantsLicenseOnce(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedSellable(t, s)
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: productID, PlanID: planID, Email: "new@example.com", Kind: "new", Quantity: 1,
		SubtotalMinor: 1190000, TotalMinor: 1190000, Currency: "VND", Provider: "sepay",
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := &settlement.Service{Store: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ev := provider.WebhookEvent{Provider: "sepay", ProviderEventID: "92704", ProviderRef: "92704",
		OrderCode: order.Code, AmountMinor: 1190000, Currency: "VND", Status: domain.OrderPaid,
		TransferType: "in", OccurredAt: time.Now()}
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, _ := s.GetPaymentOrderByCode(ctx, order.Code)
	if got.Status != domain.OrderPaid || got.PaidAt == nil {
		t.Fatalf("order = %+v", got)
	}
	var lic model.License
	if err := s.DB.NewSelect().Model(&lic).Where("email = ?", "new@example.com").Scan(ctx); err != nil {
		t.Fatalf("license: %v", err)
	}
	if lic.Status != model.StatusActive || lic.PlanID != planID || lic.LicenseKey == "" {
		t.Fatalf("license = %+v", lic)
	}
	// A second delivery (retry) is a no-op: still one licence.
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("second settle: %v", err)
	}
	n, _ := s.DB.NewSelect().Table("licenses").Where("email = ?", "new@example.com").Count(ctx)
	if n != 1 {
		t.Fatalf("licences = %d", n)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/payment/settlement -run TestSettleNewOrder -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement build.go**

```go
package settlement

import (
	"context"
	"fmt"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/store"
)

// buildNewLicense assembles the licence row for a paid new-order. It
// reads the plan as it is now and refuses a plan that no longer sells
// the same kind of licence the order snapshot recorded — the same
// protection `ErrPlanChanged` gives the Stripe path.
func buildNewLicense(ctx context.Context, s *store.Store, order *model.PaymentOrder) (*model.License, *model.Plan, error) {
	plan, err := s.FindPlanByID(ctx, order.PlanID)
	if err != nil {
		return nil, nil, fmt.Errorf("read plan: %w", err)
	}
	if !plan.Active {
		return nil, nil, fmt.Errorf("plan %s is not active", plan.ID)
	}
	// The order stores the money and the kind; it does not store the
	// plan's licence_type. Refuse rather than guess if it changed.
	key, err := s.NewLicenseKey(ctx, order.ProductID)
	if err != nil {
		return nil, nil, err
	}
	// The licence row does not carry type or limits: those live on the
	// plan and limits are derived from it. Only the period is stored.
	now := time.Now()
	lic := &model.License{
		ProductID: order.ProductID, PlanID: order.PlanID, Email: order.Email,
		LicenseKey: key, Status: model.StatusActive, Quantity: order.Quantity,
	}
	if plan.LicenseType == "perpetual" {
		// Perpetual: valid forever, updates for the purchased window
		// (nil when the plan says lifetime).
		lic.UpdatesUntil = plan.InitialUpdatesUntil(now)
		lic.UpdatesTermsSet = true
	} else {
		// A subscription/trial plan's first period ends at the order's
		// reserved period_end; with none reserved, one billing interval
		// from now.
		end := order.PeriodEnd
		if end == nil {
			e := now.Add(intervalDuration(plan.BillingInterval))
			end = &e
		}
		lic.ValidUntil = end
	}
	return lic, plan, nil
}

func intervalDuration(interval string) time.Duration {
	if interval == "year" {
		return 365 * 24 * time.Hour
	}
	return 30 * 24 * time.Hour
}
```

(Add `time` import; use `model.LicenseType` names as the model defines: check `model.License.LicenseType` values "perpetual"/"subscription"/"trial" — mirror the Stripe path's `plan.LicenseType`.)

- [ ] **Step 4: Implement service.go**

```go
package settlement

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
)

type Service struct {
	Store    *store.Store
	Webhooks *service.WebhookService
	Log      *slog.Logger
}

// Settle applies one verified provider event to one order. It is the
// only writer that grants a licence, and it is safe to call twice: the
// order row is locked, a terminal order returns without a grant, and
// the transaction table dedupes on (provider, provider_ref).
func (s *Service) Settle(ctx context.Context, orderID, providerName string, ev provider.WebhookEvent) error {
	// Amount/currency check before any lock: a mismatch is a different
	// payment (or a scam attempt) and must not touch the order.
	order, err := s.Store.GetPaymentOrderForUpdate(ctx, nil, orderID)
	if err != nil {
		return err
	}
	if ev.AmountMinor != 0 && ev.AmountMinor != order.TotalMinor {
		if _, err := s.Store.InsertPaymentTransaction(ctx, nil, store.PaymentTransactionInput{
			Provider: providerName, ProviderRef: ev.ProviderRef, OrderID: order.ID,
			AmountMinor: ev.AmountMinor, Currency: ev.Currency, Status: domain.TxUnmatched,
			Raw: ev.Raw, OccurredAt: ev.OccurredAt,
		}); err != nil {
			return err
		}
		s.Log.Warn("settlement: amount mismatch, transaction left unmatched",
			"order", order.Code, "want", order.TotalMinor, "got", ev.AmountMinor)
		return nil
	}
	if ev.Status != domain.OrderPaid {
		// partial/expired/canceled events update the attempt and, for
		// expired/canceled, close the order; no licence.
		return s.applyNonPaid(ctx, order, providerName, ev)
	}
	if order.Status == domain.OrderPaid {
		return nil // duplicate delivery after settlement
	}
	if domain.IsOrderTerminal(order.Status) {
		// Late money against an expired/canceled order: keep it visible.
		_, err := s.Store.InsertPaymentTransaction(ctx, nil, store.PaymentTransactionInput{
			Provider: providerName, ProviderRef: ev.ProviderRef, OrderID: order.ID,
			AmountMinor: ev.AmountMinor, Currency: ev.Currency, Status: domain.TxUnmatched,
			Raw: ev.Raw, OccurredAt: ev.OccurredAt,
		})
		return err
	}

	switch order.Kind {
	case "new":
		return s.settleNew(ctx, order, providerName, ev)
	case "renewal":
		return s.settleRenewal(ctx, order, providerName, ev)
	default:
		return fmt.Errorf("settlement: order %s has unsupported kind %q", order.Code, order.Kind)
	}
}

func (s *Service) settleNew(ctx context.Context, order *model.PaymentOrder, providerName string, ev provider.WebhookEvent) error {
	lic, plan, err := buildNewLicense(ctx, s.Store, order)
	if err != nil {
		return s.failOrder(ctx, order, err)
	}
	_, err = s.Store.InsertPaymentTransaction(ctx, nil, store.PaymentTransactionInput{
		Provider: providerName, ProviderRef: ev.ProviderRef, OrderID: order.ID,
		AmountMinor: ev.AmountMinor, Currency: ev.Currency, Status: domain.TxMatched,
		Raw: ev.Raw, OccurredAt: ev.OccurredAt,
	})
	if err != nil {
		return err
	}
	// The licence id is derived from the order, not random: a retry
	// after a crash between the insert and the order update inserts
	// the same row id and is caught below, instead of minting a second
	// licence for one payment.
	lic.ID = order.ID
	if err := s.Store.CreateLicenseWithSubscription(ctx, lic, plan); err != nil {
		if !store.IsUniqueViolation(err) {
			return s.failOrder(ctx, order, err)
		}
		lic, err = s.Store.FindLicenseByID(ctx, order.ID)
		if err != nil {
			return s.failOrder(ctx, order, err)
		}
	}
	paid, err := s.Store.MarkPaymentOrderPaid(ctx, nil, order.ID, time.Now())
	if err != nil {
		// A concurrent settle won the row lock; the licence is unique
		// per order only by this path, so returning nil is correct.
		if errors.Is(err, store.ErrOrderNotPayable) {
			return nil
		}
		return err
	}
	_, _ = s.Store.DB.NewUpdate().Model((*model.PaymentOrder)(nil)).
		Set("license_id = ?", lic.ID).Where("id = ?", paid.ID).Exec(ctx)
	if s.Webhooks != nil {
		s.Webhooks.Dispatch(ctx, paid.ProductID, model.EventOrderPaid, map[string]any{"order": paid})
		s.Webhooks.Dispatch(ctx, paid.ProductID, model.EventPaymentSucceeded, map[string]any{
			"order": paid, "provider": providerName,
		})
	}
	return nil
}

func (s *Service) failOrder(ctx context.Context, order *model.PaymentOrder, cause error) error {
	_, _ = s.Store.DB.NewUpdate().Model((*model.PaymentOrder)(nil)).
		Set("last_settlement_error = ?", cause.Error()).
		Set("updated_at = ?", time.Now()).Where("id = ?", order.ID).Exec(ctx)
	if s.Webhooks != nil {
		s.Webhooks.Dispatch(ctx, order.ProductID, model.EventPaymentFailed, map[string]any{
			"order": order, "error": cause.Error(),
		})
	}
	return cause
}

func (s *Service) applyNonPaid(ctx context.Context, order *model.PaymentOrder, providerName string, ev provider.WebhookEvent) error {
	_, _ = s.Store.InsertPaymentTransaction(ctx, nil, store.PaymentTransactionInput{
		Provider: providerName, ProviderRef: ev.ProviderRef, OrderID: order.ID,
		AmountMinor: ev.AmountMinor, Currency: ev.Currency, Status: domain.TxUnmatched,
		Raw: ev.Raw, OccurredAt: ev.OccurredAt,
	})
	if ev.Status == "expired" && order.Status == domain.OrderPending {
		_, _ = s.Store.DB.NewUpdate().Model((*model.PaymentOrder)(nil)).
			Set("status = ?", domain.OrderExpired).Where("id = ?", order.ID).Exec(ctx)
	}
	return nil
}
```

`settleRenewal` uses `ExtendLicenseFromOrderIn` instead of `ApplyLicenseRenewal`: the existing renewal function refuses anything that is not a perpetual licence whose `updates_until` is set, and a VN subscription renewal extends `valid_until`. Add the helper to `store/payments.go`:

```go
// ExtendLicenseFromOrderIn applies one paid renewal order to a licence.
// It covers both shapes the platform sells: a perpetual licence gets its
// maintenance window extended (the existing ledger logic), a
// subscription/trial licence gets its valid_until pushed one interval
// past the current end, with the subscription row kept in step. The
// license_renewals row carries payment_order_id, and the partial unique
// index on that column makes a replay a no-op instead of a double
// extension.
func ExtendLicenseFromOrderIn(ctx context.Context, tx bun.IDB, order *model.PaymentOrder, plan *model.Plan) error {
	// The licence extension and the order being marked paid are two
	// writes in one transaction, but a settlement retried after a crash
	// before the commit must not extend twice. The ledger row keyed by
	// the order is the receipt that this order was already applied.
	var applied int
	if err := tx.NewRaw("SELECT count(*) FROM license_renewals WHERE payment_order_id = ?", order.ID).
		Scan(ctx, &applied); err != nil {
		return err
	}
	if applied > 0 {
		return nil
	}
	lic := new(model.License)
	if err := tx.NewSelect().Model(lic).Where("id = ?", order.LicenseID).For("UPDATE").Scan(ctx); err != nil {
		return err
	}
	if lic.Status != model.StatusActive {
		return ErrRenewalIneligible
	}
	days := renewalDays(order, plan)
	if plan.LicenseType == "perpetual" {
		r := &model.LicenseRenewal{LicenseID: lic.ID, PaymentOrderID: order.ID, Days: days}
		return ApplyLicenseRenewalIn(ctx, tx, r)
	}
	now := time.Now()
	base := now
	if lic.ValidUntil != nil && lic.ValidUntil.After(now) {
		base = *lic.ValidUntil
	}
	end := base.Add(subscriptionInterval(plan.BillingInterval))
	if _, err := tx.NewUpdate().Model((*model.License)(nil)).
		Set("valid_until = ?", end).Set("updated_at = now()").
		Where("id = ?", lic.ID).Exec(ctx); err != nil {
		return err
	}
	r := &model.LicenseRenewal{ID: newID(), LicenseID: lic.ID, PaymentOrderID: order.ID, Days: days, CreatedAt: now}
	if _, err := tx.NewInsert().Model(r).Exec(ctx); err != nil {
		return err
	}
	// A Stripe-created subscription row may exist; a VN one is created
	// by CreateLicenseWithSubscription. Either way the period moves.
	_, err := tx.NewUpdate().Model((*model.Subscription)(nil)).
		Set("current_period_start = ?", base).Set("current_period_end = ?", end).
		Set("updated_at = now()").
		Where("license_id = ?", lic.ID).Exec(ctx)
	return err
}

func renewalDays(order *model.PaymentOrder, plan *model.Plan) int {
	if order.PeriodStart != nil && order.PeriodEnd != nil {
		return int(order.PeriodEnd.Sub(*order.PeriodStart).Hours() / 24)
	}
	if plan.RenewalDays > 0 {
		return plan.RenewalDays
	}
	return 30
}

// subscriptionInterval maps the plan's billing interval to the period a
// renewal buys. Kept local to the store because the checkout service
// has its own copy for pricing; both read the same two strings.
func subscriptionInterval(billingInterval string) time.Duration {
	if billingInterval == "year" {
		return 365 * 24 * time.Hour
	}
	return 30 * 24 * time.Hour
}
```

`settleRenewal` then runs the ledger insert, extension and order update in one transaction with `store.RunInTx`, marks the order paid and dispatches the same events as `settleNew`. `RecordUnmatched` inserts a `payment_transactions` row with `status=unmatched`, `order_id NULL`; add a nullable `OrderID` in `PaymentTransactionInput` (`*string`).

- [ ] **Step 5: Write and run the renewal test**

```go
// internal/payment/settlement/settle_renewal_test.go
func TestSettleRenewalExtendsLicense(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	prod := &model.Product{Name: "RenewCo", Slug: "renew-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ProductID: prod.ID, Name: "Monthly", Slug: "monthly-" + suffix,
		LicenseType: "subscription", BillingInterval: "month", Active: true}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	oldEnd := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	lic := &model.License{ProductID: prod.ID, PlanID: plan.ID, Email: "renew@example.com",
		LicenseKey: "KEY-RENEW-1", Status: model.StatusActive, Quantity: 1,
		ValidUntil: &oldEnd}
	if err := s.CreateLicenseWithSubscription(ctx, lic, plan); err != nil {
		t.Fatal(err)
	}
	productID, planID := prod.ID, plan.ID
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: productID, PlanID: planID, Email: "renew@example.com", Kind: "renewal",
		Quantity: 1, SubtotalMinor: 1190000, TotalMinor: 1190000, Currency: "VND",
		Provider: "sepay", ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Link the order to the licence and reserve the period.
	if _, err := s.DB.NewUpdate().Model((*model.PaymentOrder)(nil)).
		Set("license_id = ?", lic.ID).Where("id = ?", order.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	svc := &settlement.Service{Store: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ev := provider.WebhookEvent{Provider: "sepay", ProviderEventID: "r1", ProviderRef: "r1",
		OrderCode: order.Code, AmountMinor: 1190000, Currency: "VND", Status: domain.OrderPaid, TransferType: "in"}
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("settle renewal: %v", err)
	}
	got, _ := s.FindLicenseByID(ctx, lic.ID)
	if got.ValidUntil == nil || !got.ValidUntil.After(*lic.ValidUntil) {
		t.Fatalf("valid_until = %v, want after %v", got.ValidUntil, lic.ValidUntil)
	}
	// The renewal ledger carries the order id.
	if _, err := s.FindLicenseRenewalByPaymentOrder(ctx, order.ID); err != nil { // add in store/payments.go
		t.Fatalf("renewal ledger: %v", err)
	}
}
```

Run it; add `FindLicenseRenewalByPaymentOrder` to `store/payments.go`:

```go
func (s *Store) FindLicenseRenewalByPaymentOrder(ctx context.Context, orderID string) (*model.LicenseRenewal, error) {
	r := new(model.LicenseRenewal)
	err := s.DB.NewSelect().Model(r).Where("payment_order_id = ?", orderID).Limit(1).Scan(ctx)
	return r, err
}
```

Then implement `settleRenewal` and run both tests.

- [ ] **Step 6: Write and run the guard tests**

```go
// internal/payment/settlement/settle_guard_test.go
func TestSettleAmountMismatchGrantsNothing(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedSellable(t, s)
	order, _ := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: productID, PlanID: planID, Email: "short@example.com", Kind: "new", Quantity: 1,
		SubtotalMinor: 1190000, TotalMinor: 1190000, Currency: "VND", Provider: "sepay",
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	svc := &settlement.Service{Store: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	err := svc.Settle(ctx, order.ID, "sepay", provider.WebhookEvent{Provider: "sepay",
		ProviderEventID: "mismatch-1", ProviderRef: "mismatch-1", AmountMinor: 1, Currency: "VND",
		Status: domain.OrderPaid, TransferType: "in"})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, _ := s.GetPaymentOrderByCode(ctx, order.Code)
	if got.Status != domain.OrderPending {
		t.Fatalf("order = %q, want pending", got.Status)
	}
	if n, _ := s.DB.NewSelect().Table("licenses").Where("email = ?", "short@example.com").Count(ctx); n != 0 {
		t.Fatalf("licences = %d, want 0", n)
	}
	if n, _ := s.DB.NewSelect().Table("payment_transactions").Where("status = ?", domain.TxUnmatched).Count(ctx); n < 1 {
		t.Fatalf("unmatched transactions = %d", n)
	}
}

func TestSettleLatePaymentOnExpiredOrderGrantsNothing(t *testing.T) {
	// Same setup, expire the order first, then deliver a full payment:
	// the transaction is recorded and the order does not move.
}
```

Fill the second test with the same body as the mismatch test but: set `order.Status` to expired via `s.DB.NewUpdate`, deliver `AmountMinor: 1190000`, assert order still expired, 0 licences, 1 unmatched transaction.

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/payment/settlement -v`
Expected: PASS.

- [ ] **Step 7: Add `InsertPaymentTransaction` and `MarkTransactionMatched` to the store**

```go
type PaymentTransactionInput struct {
	Provider    string
	ProviderRef string
	OrderID     *string
	AmountMinor int64
	Currency    string
	Status      string
	Raw         []byte
	OccurredAt  time.Time
}

func (s *Store) InsertPaymentTransaction(ctx context.Context, tx bun.Tx, in PaymentTransactionInput) (*model.PaymentTransaction, error) {
	db := bun.IDB(s.DB)
	if tx != nil {
		db = tx
	}
	row := &model.PaymentTransaction{ID: NewID(), Provider: in.Provider, ProviderRef: in.ProviderRef,
		OrderID: in.OrderID, AmountMinor: in.AmountMinor, Currency: in.Currency, Status: in.Status,
		Raw: in.Raw, OccurredAt: in.OccurredAt, CreatedAt: time.Now()}
	_, err := db.NewInsert().Model(row).On("CONFLICT (provider, provider_ref) DO NOTHING").Exec(ctx)
	return row, err
}

func (s *Store) MarkTransactionMatched(ctx context.Context, id, orderID string) error {
	_, err := s.DB.NewUpdate().Model((*model.PaymentTransaction)(nil)).
		Set("status = ?", domain.TxMatched).Set("order_id = ?", orderID).Where("id = ?", id).Exec(ctx)
	return err
}
```

`payment_transactions.provider_ref` needs a unique index; add it if the T1 DDL does not already have one (`(provider, provider_ref)` unique). Ledger tables use `NUMERIC` only where the spec says so; these are BIGINT.

- [ ] **Step 8: Full settlement tests and commit**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/payment/settlement ./internal/store -run 'Settle|PaymentTransaction' -v`
Expected: PASS.

```bash
git add internal/payment/settlement internal/store/payments.go
git commit -m "feat(payments): settle paid SePay/PayOS orders into licences and renewals"
```

---

### Task 10: Checkout service, public payment routes and the consumer API

**Files:**
- Create: `internal/payment/checkout/service.go`
- Create: `internal/handler/payments_public.go`
- Create: `internal/handler/payments_api.go`
- Modify: `internal/model/model.go` (add `ScopePaymentsWrite`, `ScopePaymentsRead` to `AllScopes`)
- Modify: `cmd/server/main.go` (provider factory + routes)
- Test: `internal/payment/checkout/service_test.go`, `internal/handler/payments_api_test.go`

**Interfaces:**
- Consumes: `store.ResolvePlanPrice`, `store.CreatePaymentOrder`, `store.CreatePaymentAttempt`, `domain.ComputeTotals`, `provider.Provider`, `providercfg.Service`.
- Produces:
  - `checkout.Service{Store, Config, Providers ProviderFactory, Webhooks, Log}`; `ProviderFactory func(ctx context.Context, productID, providerName string) (provider.Provider, *providercfg.ResolvedConfig, error)`
  - `checkout.CreateOrderInput{PlanID, Email, Provider string; Quantity int; Buyer provider.Buyer; Metadata map[string]any; CreatedByKeyID string}`
  - `(*Service).CreateOrder(ctx, in) (*checkout.OrderResult, error)`; `OrderResult{Order *model.PaymentOrder; Attempt *model.PaymentAttempt}`
  - `checkout.ErrPlanNotSellable`, `checkout.ErrPriceNotConfigured` (wraps `store.ErrPriceNotConfigured`), `checkout.ErrProviderUnavailable`
  - `handler.PaymentsPublicHandler{Checkout, Store}` with `Status` and `PayPage` (302 dispatcher helper)
  - `handler.PaymentsAPIHandler{Checkout, Store}` with `Create`, `Get`, `List`, `Cancel`
  - `model.ScopePaymentsWrite`, `model.ScopePaymentsRead`.

- [ ] **Step 1: Add the scopes and their test**

```go
// internal/model/scopes_test.go (new file)
package model_test

import (
	"testing"

	"github.com/tabloy/keygate/internal/model"
)

func TestPaymentScopesRegistered(t *testing.T) {
	for _, scope := range []string{model.ScopePaymentsWrite, model.ScopePaymentsRead} {
		if !model.IsValidScope(scope) {
			t.Fatalf("%q is not a valid scope", scope)
		}
	}
}
```

Add to `internal/model/model.go`:

```go
	ScopePaymentsWrite = "payments:write"
	ScopePaymentsRead  = "payments:read"
```

and to `AllScopes()`.

- [ ] **Step 2: Write the failing checkout service test**

```go
// internal/payment/checkout/service_test.go
package checkout_test

import (
	"context"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/checkout"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/providercfg"
	"github.com/tabloy/keygate/internal/payment/provider/fake"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

func TestCreateOrderResolvesPriceAndTotals(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	p := &model.Product{Name: "Co", Slug: "co-" + suffix, Type: "desktop", TaxRateBP: 1000, PriceIncludesTax: true}
	if err := s.CreateProduct(ctx, p); err != nil {
		t.Fatal(err)
	}
	pl := &model.Plan{ProductID: p.ID, Name: "Pro", Slug: "co-pro-" + suffix, LicenseType: "perpetual",
		MaxActivations: 3, Active: true, BillingInterval: ""}
	if err := s.CreatePlan(ctx, pl); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertPlanPrice(ctx, pl.ID, store.PlanPriceInput{Provider: "sepay", Currency: "VND", AmountMinor: 1_190_000}); err != nil {
		t.Fatal(err)
	}
	fp := fake.New("sepay")
	svc := &checkout.Service{Store: s, Providers: func(context.Context, string, string) (provider.Provider, *providercfg.ResolvedConfig, error) {
		return fp, &providercfg.ResolvedConfig{Provider: "sepay"}, nil
	}}
	res, err := svc.CreateOrder(ctx, checkout.CreateOrderInput{PlanID: pl.ID, Email: "c@example.com", Provider: "sepay", Quantity: 1})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Order.TotalMinor != 1_190_000 || res.Order.TaxMinor != 108_182 || res.Order.SubtotalMinor != 1_190_000 {
		t.Fatalf("order totals = %+v", res.Order)
	}
	if res.Order.Currency != "VND" || res.Order.CurrencyExp != 0 {
		t.Fatalf("currency = %+v", res.Order)
	}
	if len(fp.Calls) != 1 || fp.Calls[0].AmountMinor != 1_190_000 {
		t.Fatalf("provider calls = %+v", fp.Calls)
	}
	if res.Attempt.ProviderRef == "" || res.Attempt.QRPayload == "" {
		t.Fatalf("attempt = %+v", res.Attempt)
	}
}

func TestCreateOrderRejectsMissingPrice(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	p := &model.Product{Name: "NoPrice", Slug: "noprice-" + suffix, Type: "desktop"}
	_ = s.CreateProduct(ctx, p)
	pl := &model.Plan{ProductID: p.ID, Name: "Pro", Slug: "np-pro-" + suffix, LicenseType: "perpetual", Active: true}
	_ = s.CreatePlan(ctx, pl)
	svc := &checkout.Service{Store: s, Providers: func(context.Context, string, string) (provider.Provider, *providercfg.ResolvedConfig, error) {
		return fake.New("sepay"), &providercfg.ResolvedConfig{Provider: "sepay"}, nil
	}}
	if _, err := svc.CreateOrder(ctx, checkout.CreateOrderInput{PlanID: pl.ID, Email: "x@example.com", Provider: "sepay", Quantity: 1}); err == nil {
		t.Fatal("order accepted with no price")
	}
}
```

- [ ] **Step 3: Run it to verify it fails**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/payment/checkout -v`
Expected: FAIL — package does not exist.

- [ ] **Step 4: Implement checkout/service.go**

```go
package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/providercfg"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
)

var (
	ErrPlanNotSellable     = errors.New("plan is not sellable")
	ErrProviderUnavailable = errors.New("payment provider is not available")
	// ErrPriceNotConfigured is re-exported so callers of checkout can
	// errors.Is against one sentinel instead of importing store too.
	ErrPriceNotConfigured = store.ErrPriceNotConfigured
)

const defaultOrderTTL = 24 * time.Hour

type ProviderFactory func(ctx context.Context, productID, providerName string) (provider.Provider, *providercfg.ResolvedConfig, error)

type Service struct {
	Store     *store.Store
	Config    *providercfg.Service
	Providers ProviderFactory
	Webhooks  *service.WebhookService
	Log       *slog.Logger
}

type CreateOrderInput struct {
	PlanID         string
	Email          string
	Provider       string
	Quantity       int
	Buyer          provider.Buyer
	Metadata       map[string]any
	CreatedByKeyID string
}

type OrderResult struct {
	Order   *model.PaymentOrder
	Attempt *model.PaymentAttempt
}

func (s *Service) CreateOrder(ctx context.Context, in CreateOrderInput) (*OrderResult, error) {
	if in.Quantity <= 0 {
		in.Quantity = 1
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" || !strings.Contains(email, "@") {
		return nil, fmt.Errorf("%w: a buyer email is required for the licence", ErrPlanNotSellable)
	}
	plan, err := s.Store.FindPlanByID(ctx, in.PlanID)
	if err != nil {
		return nil, fmt.Errorf("%w: plan not found", ErrPlanNotSellable)
	}
	if !plan.Active {
		return nil, fmt.Errorf("%w: plan is inactive", ErrPlanNotSellable)
	}
	product, err := s.Store.FindProductByID(ctx, plan.ProductID)
	if err != nil {
		return nil, fmt.Errorf("%w: product not found", ErrPlanNotSellable)
	}
	providerName := strings.ToLower(strings.TrimSpace(in.Provider))
	if providerName == "" {
		providerName, _ = s.Store.GetSetting(ctx, "payment_default_provider")
	}
	if providerName == "" {
		providerName = "sepay"
	}
	prov, cfg, err := s.Providers(ctx, product.ID, providerName)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	if cfg == nil {
		cfg = &providercfg.ResolvedConfig{}
	}
	// VND is the only settlement currency for the VN rails in phase 1.
	currency := "VND"
	price, err := s.Store.ResolvePlanPrice(ctx, plan.ID, providerName, currency, plan.BillingInterval)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPlanNotSellable, err)
	}
	subtotal := price.AmountMinor * int64(in.Quantity)
	taxRate := product.TaxRateBP
	totals := domain.ComputeTotals(subtotal, 0, taxRate, product.PriceIncludesTax)

	ttl := s.orderTTL(ctx)
	expiresAt := time.Now().Add(ttl)
	metadata := map[string]any{}
	for k, v := range in.Metadata {
		metadata[k] = v
	}
	metaJSON, _ := json.Marshal(metadata)
	buyerJSON, _ := json.Marshal(in.Buyer)
	order, err := s.Store.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: product.ID, PlanID: plan.ID, PriceID: price.ID, Email: email, Kind: "new",
		Quantity: in.Quantity, SubtotalMinor: totals.NetMinor + totals.TaxMinor,
		TaxMinor: totals.TaxMinor, TotalMinor: totals.TotalMinor,
		Currency: currency, CurrencyExponent: domain.CurrencyExponent(currency), TaxRateBP: taxRate,
		Provider: providerName, ExpiresAt: expiresAt, Buyer: buyerJSON, Metadata: metaJSON,
		CreatedByKeyID: in.CreatedByKeyID,
	})
	if err != nil {
		return nil, err
	}
	attempt := &model.PaymentAttempt{
		ID: store.NewID(), OrderID: order.ID, Provider: providerName,
		Status: domain.AttemptPending, AmountMinor: order.TotalMinor, Currency: currency,
		ExpiresAt: &expiresAt, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	result, err := prov.CreateAttempt(ctx, provider.CreateAttemptRequest{
		OrderCode: order.Code, NumericCode: order.NumericCode, AmountMinor: order.TotalMinor,
		Currency: currency, CurrencyExp: order.CurrencyExp, Description: order.Code,
		Buyer: in.Buyer, ExpiresAt: expiresAt, Config: nil,
	})
	if err != nil {
		// The order exists but has no payment instruction: leave it
		// pending and return the error so the caller can retry. The
		// expiry sweeper closes it if it is never paid.
		_ = s.Store.UpdatePaymentAttemptStatus(ctx, attempt.ID, domain.AttemptFailed, "")
		s.Log.Error("checkout: provider create attempt failed", "order", order.Code, "provider", providerName, "error", err)
		return nil, fmt.Errorf("create payment instruction: %w", err)
	}
	attempt.ProviderRef = result.ProviderRef
	attempt.QRPayload = result.QRPayload
	attempt.CheckoutURL = result.CheckoutURL
	attempt.VANumber = result.VANumber
	if result.ExpiresAt.IsZero() {
		result.ExpiresAt = expiresAt
	}
	attempt.ExpiresAt = &result.ExpiresAt
	// attempts.raw is the spec's JSONB escape hatch: it keeps the bank
	// account the adapter chose, plus anything provider-specific, so the
	// public status page can render transfer details without asking the
	// provider again.
	attempt.Raw = attemptRaw(&result)
	if err := s.Store.CreatePaymentAttempt(ctx, attempt); err != nil {
		return nil, err
	}
	if s.Webhooks != nil {
		s.Webhooks.Dispatch(ctx, product.ID, model.EventOrderCreated, map[string]any{"order": order})
	}
	return &OrderResult{Order: order, Attempt: attempt}, nil
}

// attemptRaw serialises the non-secret parts of a CreateAttemptResult.
func attemptRaw(res *provider.CreateAttemptResult) json.RawMessage {
	b, err := json.Marshal(map[string]any{
		"bank": res.Bank, "provider_raw": res.Raw, "description": res.Description,
	})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func (s *Service) orderTTL(ctx context.Context) time.Duration {
	if raw, err := s.Store.GetSetting(ctx, "payment_order_ttl_minutes"); err == nil {
		var n int
		if _, err := fmt.Sscanf(raw, "%d", &n); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return defaultOrderTTL
}
```

`model.PaymentAttempt` fields must match the T1 struct (`QRPayload`, `CheckoutURL`, `VANumber`, `ExpiresAt *time.Time`); adjust the DDL/model in T1 if a name differs — the model is the source of truth for this plan.

- [ ] **Step 5: Implement the public and consumer handlers**

`internal/handler/payments_api.go`:

```go
package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/checkout"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/pkg/response"
)

type PaymentsAPIHandler struct {
	Checkout *checkout.Service
	Store    *store.Store
}

type createOrderRequest struct {
	PlanID   string          `json:"plan_id"`
	Email    string          `json:"email"`
	Provider string          `json:"provider,omitempty"`
	Quantity int             `json:"quantity,omitempty"`
	Buyer    provider.Buyer  `json:"buyer,omitempty"`
	Metadata map[string]any  `json:"metadata,omitempty"`
}

func (h *PaymentsAPIHandler) Create(c *gin.Context) {
	var req createOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid JSON body")
		return
	}
	keyID := ""
	if v, ok := c.Get("api_key"); ok {
		if ak, ok := v.(*model.APIKey); ok {
			keyID = ak.ID
		}
	}
	res, err := h.Checkout.CreateOrder(c, checkout.CreateOrderInput{
		PlanID: req.PlanID, Email: req.Email, Provider: req.Provider, Quantity: req.Quantity,
		Buyer: req.Buyer, Metadata: req.Metadata, CreatedByKeyID: keyID,
	})
	if err != nil {
		writeCheckoutError(c, err)
		return
	}
	response.Created(c, orderView(res))
}

func (h *PaymentsAPIHandler) Get(c *gin.Context) {
	order, err := h.Store.GetPaymentOrderByCode(c, c.Param("code"))
	if errors.Is(err, store.ErrOrderNotFound) {
		response.Err(c, http.StatusNotFound, "PAYMENT_ORDER_NOT_FOUND", "order not found")
		return
	}
	if err != nil {
		response.Internal(c, err)
		return
	}
	response.OK(c, orderView(&checkout.OrderResult{Order: order}))
}

func (h *PaymentsAPIHandler) Cancel(c *gin.Context) {
	order, err := h.Store.CancelPaymentOrder(c, c.Param("code"))
	if errors.Is(err, store.ErrOrderNotFound) {
		response.Err(c, http.StatusNotFound, "PAYMENT_ORDER_NOT_FOUND", "order not found")
		return
	}
	if errors.Is(err, store.ErrOrderNotPayable) {
		response.Err(c, http.StatusConflict, "PAYMENT_ORDER_NOT_PAYABLE", "order is already paid or closed")
		return
	}
	if err != nil {
		response.Internal(c, err)
		return
	}
	response.OK(c, orderView(&checkout.OrderResult{Order: order}))
}

func writeCheckoutError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, checkout.ErrProviderUnavailable):
		response.Err(c, http.StatusServiceUnavailable, "PAYMENT_PROVIDER_DISABLED", err.Error())
	case errors.Is(err, store.ErrPriceNotConfigured), errors.Is(err, checkout.ErrPlanNotSellable):
		response.Err(c, http.StatusUnprocessableEntity, "PAYMENT_PRICE_NOT_CONFIGURED", err.Error())
	default:
		response.Internal(c, err)
	}
}

func orderView(res *checkout.OrderResult) gin.H {
	out := gin.H{
		"code": res.Order.Code, "status": res.Order.Status, "kind": res.Order.Kind,
		"product_id": res.Order.ProductID, "plan_id": res.Order.PlanID,
		"email": res.Order.Email, "quantity": res.Order.Quantity,
		"subtotal_minor": res.Order.SubtotalMinor, "discount_minor": res.Order.DiscountMinor,
		"tax_minor": res.Order.TaxMinor, "total_minor": res.Order.TotalMinor,
		"currency": res.Order.Currency, "currency_exponent": res.Order.CurrencyExp,
		"expires_at": res.Order.ExpiresAt,
	}
	if res.Attempt != nil {
		out["payment"] = gin.H{
			"provider": res.Attempt.Provider, "provider_ref": res.Attempt.ProviderRef,
			"qr_image_url": res.Attempt.QRPayload, "checkout_url": res.Attempt.CheckoutURL,
			"va_number": res.Attempt.VANumber, "expires_at": res.Attempt.ExpiresAt,
		}
	}
	return out
}
```

`payments_public.go` adds `GET /pay/:checkout_id/status` and the `PayPage` dispatcher. The coupon-era quote endpoint (`POST /api/v1/payments/quote`) is deliberately not part of phase 1a: its whole payload is discounts and tax preview, and coupons land in 1b; adding it now would publish an API without its contract.

```go
type PaymentsPublicHandler struct {
	Checkout *checkout.Service
	Store    *store.Store
	// Stripe keeps serving the legacy cs_… links; it is never called for
	// a KG order code.
	Stripe *payment.StripeHandler
}

// statusView is the public pay-page payload. It is deliberately flat and
// contains no buyer email, metadata or API-key id: the order code is the
// only capability the holder has.
func statusView(order *model.PaymentOrder, attempt *model.PaymentAttempt) gin.H {
	out := gin.H{
		"code": order.Code, "status": order.Status,
		"amount_minor": order.TotalMinor, "currency": order.Currency,
		"currency_exponent": order.CurrencyExp,
		"expires_at": order.ExpiresAt, "description": order.Code,
	}
	if attempt == nil {
		return out
	}
	out["qr_image_url"] = attempt.QRPayload
	out["checkout_url"] = attempt.CheckoutURL
	if len(attempt.Raw) > 0 {
		var raw struct {
			Bank        provider.BankAccount `json:"bank"`
			Description string               `json:"description"`
		}
		if err := json.Unmarshal(attempt.Raw, &raw); err == nil {
			if raw.Bank.AccountNo != "" {
				out["bank"] = raw.Bank
			}
			if raw.Description != "" {
				out["description"] = raw.Description
			}
		}
	}
	return out
}

func (h *PaymentsPublicHandler) Status(c *gin.Context) {
	code := c.Param("checkout_id")
	if !domain.IsOrderCode(code) {
		response.NotFound(c, "checkout not found")
		return
	}
	order, err := h.Store.GetPaymentOrderByCode(c, code)
	if errors.Is(err, store.ErrOrderNotFound) {
		response.NotFound(c, "checkout not found")
		return
	}
	if err != nil {
		response.Internal(c, err)
		return
	}
	attempt, err := h.Store.GetLatestPaymentAttempt(c, order.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		response.Internal(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.OK(c, statusView(order, attempt))
}

const stripeCheckoutPrefix = "cs_"

// PayPage keeps the single existing wildcard route alive: Stripe links
// (cs_…) keep their current handler, KG order codes answer with a 302
// to the SPA page that polls the status endpoint.
func (h *PaymentsPublicHandler) PayPage(c *gin.Context) {
	id := c.Param("checkout_id")
	if strings.HasPrefix(id, stripeCheckoutPrefix) {
		h.Stripe.CheckoutByPlan(c)
		return
	}
	if !domain.IsOrderCode(id) {
		response.NotFound(c, "checkout not found")
		return
	}
	c.Redirect(http.StatusFound, "/checkout/pay/"+id)
}
```

Wiring in `main.go`:

```go
stripeH := payment.NewStripeHandler(...) // existing constructor stays
payPublicH := &handler.PaymentsPublicHandler{Checkout: checkoutSvc, Store: db, Stripe: stripeH}
r.GET("/pay/:checkout_id", payPublicH.PayPage)                 // replaces stripeH.CheckoutByPlan
r.GET("/pay/:checkout_id/status", payPublicH.Status)           // public JSON poll

// providerFactory builds the adapter for one resolved config row. The
// checkout service and the reconciler both need it, and neither may
// construct adapters itself: only this function knows that a
// ResolvedConfig's secrets and bank fields map onto a sepay.Config /
// payos.Config.
providerFactory := func(ctx context.Context, productID, providerName string) (provider.Provider, *providercfg.ResolvedConfig, error) {
	cfg, err := providerSvc.Resolve(ctx, productID, providerName, "")
	if err != nil {
		return nil, nil, err
	}
	switch providerName {
	case "sepay":
		return sepay.New(sepay.Config{
			Sandbox:         cfg.Mode == "test",
			WebhookAuthMode: cfg.WebhookAuthMode,
			APIKey:          cfg.APIKey,
			IPNSecret:       cfg.IPNSecret,
			APIToken:        cfg.APIToken,
			Bank: provider.BankAccount{
				BankName: cfg.BankName, AccountNo: cfg.AccountNo, AccountName: cfg.AccountName,
			},
			BankCode: cfg.BankCode,
		}), cfg, nil
	case "payos":
		return payos.New(payos.Config{
			ClientID: cfg.ClientID, APIKey: cfg.APIKey, ChecksumKey: cfg.ChecksumKey,
		}), cfg, nil
	default:
		return nil, nil, providercfg.ErrProviderNotConfigured
	}
}
checkoutSvc := &checkout.Service{Store: db, Config: providerSvc, Providers: providerFactory, Webhooks: webhookSvc, Log: logger}
```

Keep the old Stripe success/cancel behaviour untouched; the dispatcher only changes the entry route's branch.

- [ ] **Step 6: Register the consumer API group**

```go
payWrite := v1.Group("/payments",
	middleware.APIKeyAuth(db),
	middleware.RequireScope(model.ScopePaymentsWrite),
	middleware.RateLimitByIPScoped("payments_api", cfg.RateLimitAPI, time.Minute))
payRead := v1.Group("/payments",
	middleware.APIKeyAuth(db),
	middleware.RequireScope(model.ScopePaymentsRead, model.ScopePaymentsWrite))
{
	idem := middleware.Idempotency(db)
	payWrite.POST("/orders", idem, paymentsAPIH.Create)
	payWrite.POST("/orders/:code/cancel", paymentsAPIH.Cancel) // Cancel is a write
	payRead.GET("/orders/:code", paymentsAPIH.Get)
	payRead.GET("/orders", paymentsAPIH.List) // List uses pagination.Parse, default 50/max 200
}
```

`List` filters: `status`, `product_id`, `email`, `provider`, `created_from`, `created_to`; return `{"data": [...], "total": n}` via the existing `pagination` helper.

- [ ] **Step 7: Write the failing consumer API test**

Use `newAPIFixture`, an API key with `payments:write`, and a fake provider factory:

```go
// internal/handler/payments_api_test.go
// imports: bytes, context, net/http, net/http/httptest, strings, testing;
// gin; keygate/internal/middleware, model, payment/checkout,
// payment/provider, payment/provider/fake, payment/providercfg, store.

func TestCreateOrderViaAPI(t *testing.T) {
	f := newAPIFixture(t)
	// price for planA
	if _, err := f.s.UpsertPlanPrice(f.ctx, f.planA.ID, store.PlanPriceInput{Provider: "sepay", Currency: "VND", AmountMinor: 1190000}); err != nil {
		t.Fatal(err)
	}
	fp := fake.New("sepay")
	svc := &checkout.Service{Store: f.s, Providers: func(context.Context, string, string) (provider.Provider, *providercfg.ResolvedConfig, error) {
		return fp, &providercfg.ResolvedConfig{Provider: "sepay"}, nil
	}}
	h := &PaymentsAPIHandler{Checkout: svc, Store: f.s}
	key := &model.APIKey{ID: "key-1", Scopes: []string{model.ScopePaymentsWrite}}
	code, out := f.call(h.Create, "POST", "/api/v1/payments/orders",
		`{"plan_id":"`+f.planA.ID+`","email":"api@example.com","provider":"sepay","quantity":1}`, key)
	if code != 201 {
		t.Fatalf("status %d: %v", code, out)
	}
	data, _ := out["data"].(map[string]any)
	if data["total_minor"] != float64(1190000) || data["status"] != "pending" {
		t.Fatalf("data = %v", data)
	}
	// Replay of the same order code must not double-grant; settlement
	// itself is covered in Task 9. Here the API must expose the code.
	if data["code"] == "" || !strings.HasPrefix(data["code"].(string), "KG") {
		t.Fatalf("code = %v", data["code"])
	}
}
```

Then add the idempotency test — the middleware is tested elsewhere, but the orders route is what must not create two orders when a consumer retries:

```go
// internal/handler/payments_api_test.go (same file)
func TestCreateOrderIdempotent(t *testing.T) {
	f := newAPIFixture(t)
	if _, err := f.s.UpsertPlanPrice(f.ctx, f.planA.ID, store.PlanPriceInput{Provider: "sepay", Currency: "VND", AmountMinor: 1190000}); err != nil {
		t.Fatal(err)
	}
	svc := &checkout.Service{Store: f.s, Providers: func(context.Context, string, string) (provider.Provider, *providercfg.ResolvedConfig, error) {
		return fake.New("sepay"), &providercfg.ResolvedConfig{Provider: "sepay"}, nil
	}}
	h := &PaymentsAPIHandler{Checkout: svc, Store: f.s}
	r := gin.New()
	r.POST("/api/v1/payments/orders", middleware.Idempotency(f.s), h.Create)
	body := `{"plan_id":"` + f.planA.ID + `","email":"idem@example.com","provider":"sepay","quantity":1}`
	send := func(payload string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/v1/payments/orders", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "idem-order-1")
		r.ServeHTTP(w, req)
		return w
	}
	first := send(body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d %s", first.Code, first.Body.String())
	}
	second := send(body)
	if second.Code != first.Code || second.Body.String() != first.Body.String() {
		t.Fatalf("replay = %d %s, want the identical response", second.Code, second.Body.String())
	}
	if n, _ := f.s.DB.NewSelect().Table("payment_orders").Where("email = ?", "idem@example.com").Count(f.ctx); n != 1 {
		t.Fatalf("orders = %d, want 1", n)
	}
	third := send(`{"plan_id":"` + f.planB.ID + `","email":"idem@example.com","provider":"sepay","quantity":1}`)
	if third.Code != http.StatusConflict {
		t.Fatalf("body mismatch = %d %s, want 409", third.Code, third.Body.String())
	}
}
```

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/handler -run 'TestCreateOrder' -v` → PASS after wiring; expected FAIL before the handler exists.

- [ ] **Step 8: Run everything and commit**

Run: `go test ./internal/payment/... ./internal/model -run 'Test' && TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/payment/checkout ./internal/handler -run 'TestCreateOrder' -v`
Expected: PASS.

```bash
git add internal/payment/checkout internal/handler/payments_public.go internal/handler/payments_api.go internal/handler/payments_api_test.go internal/model/model.go internal/model/scopes_test.go cmd/server/main.go
git commit -m "feat(payments): checkout service, public pay dispatcher and consumer API"
```

---

### Task 11: Reconciliation worker, unmatched queue and admin payment operations

**Files:**
- Create: `internal/payment/reconcile/matcher.go`
- Create: `internal/payment/reconcile/worker.go`
- Create: `internal/handler/payments_admin.go`
- Modify: `internal/store/payments.go` (list/filter helpers, refunds, sync cursors)
- Modify: `cmd/server/main.go` (start worker, register admin routes)
- Test: `internal/payment/reconcile/matcher_test.go`, `internal/handler/payments_admin_test.go`

**Interfaces:**
- Consumes: `store.GetPaymentOrderByCode`, `store.InsertPaymentTransaction`, `settlement.Service.Settle`, `sepay.Provider.ListTransactions`, `payos.Client.GetPaymentLink`.
- Produces:
  - `reconcile.Matcher{Store}`; `(*Matcher).Match(ctx, providerName string, ev provider.WebhookEvent) (*model.PaymentOrder, bool /*ambiguous*/, error)`
  - `reconcile.Worker{Store, Matcher, Settlement, Log, Interval}`; `(*Worker).Run(ctx)`; `(*Worker).Tick(ctx) error`
  - `store.ListPaymentOrders(ctx, filter OrderFilter) ([]*model.PaymentOrder, int, error)`, `store.OrderFilter{Status, ProductID, Email, Provider string; Limit, Offset int}`
  - `store.ListPaymentTransactions(ctx, filter TransactionFilter) ([]*model.PaymentTransaction, int, error)`, `store.ListUnmatchedTransactions(ctx, limit int)`, `store.MatchTransaction(ctx, id, orderID string) error`, `store.IgnoreTransaction(ctx, id, reason string) error`
  - `store.RecordPaymentRefund(ctx, r *model.PaymentRefund) error`
  - `store.GetPaymentSyncCursor(ctx, providerName, scope string) (*model.PaymentSyncCursor, error)`, `store.SetPaymentSyncCursor(ctx, providerName, scope, lastTransactionID string, at time.Time) error` (spec §4.10 keys cursors by `(provider, scope)` and stores the provider's own ID as TEXT)
  - `handler.PaymentsAdminHandler{Store, Settlement, Config, Providers, PayOSConfirmer func(ctx, url string) error}` with routes `GET /admin/payments/orders`, `GET /admin/payments/orders/:code`, `POST …/mark-paid`, `POST …/cancel`, `POST …/refund`, `GET /admin/payments/transactions`, `GET /admin/payments/unmatched`, `POST /admin/payments/transactions/:id/match`, `POST /admin/payments/transactions/:id/ignore`, `GET /admin/payments/health`, `POST /admin/payments/providers/:id/test`, `POST /admin/payments/providers/payos/confirm-webhook`.

- [ ] **Step 1: Write the failing matcher test (Review Focus #3)**

```go
// internal/payment/reconcile/matcher_test.go
package reconcile_test

import (
	"context"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/reconcile"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

func seedTwoPendingOrders(t *testing.T, s *store.Store) (*model.PaymentOrder, *model.PaymentOrder) {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000")
	p := &model.Product{Name: "Match", Slug: "match-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, p); err != nil {
		t.Fatal(err)
	}
	pl := &model.Plan{ProductID: p.ID, Name: "Pro", Slug: "match-pro-" + suffix, LicenseType: "perpetual", Active: true}
	if err := s.CreatePlan(ctx, pl); err != nil {
		t.Fatal(err)
	}
	mk := func(email string) *model.PaymentOrder {
		o, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
			ProductID: p.ID, PlanID: pl.ID, Email: email, Kind: "new", Quantity: 1,
			SubtotalMinor: 50000, TotalMinor: 50000, Currency: "VND", Provider: "sepay",
			ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	return mk("a-" + suffix + "@example.com"), mk("b-" + suffix + "@example.com")
}

func TestMatcherPrefersExactCodeOverAmountWindow(t *testing.T) {
	s := testdb.Store(t)
	a, b := seedTwoPendingOrders(t, s)
	m := &reconcile.Matcher{Store: s}

	// The memo names b's code even though a is older; the code wins.
	got, ambiguous, err := m.Match(context.Background(), "sepay", provider.WebhookEvent{
		OrderCode: b.Code, AmountMinor: 50000, Currency: "VND", OccurredAt: time.Now(),
	})
	if err != nil || ambiguous || got == nil || got.ID != b.ID {
		t.Fatalf("match = %v ambiguous=%v err=%v, want order %s", got, ambiguous, err, b.Code)
	}

	// No code: two pending orders with the same amount inside the
	// window is ambiguous — an operator must decide, not the matcher.
	got, ambiguous, err = m.Match(context.Background(), "sepay", provider.WebhookEvent{
		AmountMinor: 50000, Currency: "VND", OccurredAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("ambiguous match error: %v", err)
	}
	if got != nil || !ambiguous {
		t.Fatalf("got=%v ambiguous=%v, want ambiguous", got, ambiguous)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/payment/reconcile -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement matcher.go**

```go
package reconcile

import (
	"context"
	"errors"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/store"
)

type Matcher struct{ Store *store.Store }

// Match finds the pending order a provider event belongs to. The order
// code is the only positive match; amount+time is only a suggestion and
// returns ambiguous=true when more than one pending order could be the
// transfer. The reconciler never settles from a suggestion.
func (m *Matcher) Match(ctx context.Context, providerName string, ev provider.WebhookEvent) (*model.PaymentOrder, bool, error) {
	if ev.OrderCode != "" {
		order, err := m.Store.GetPaymentOrderByCode(ctx, ev.OrderCode)
		if err == nil && order.Status == domain.OrderPending && order.Provider == providerName {
			return order, false, nil
		}
		if err != nil && !errors.Is(err, store.ErrOrderNotFound) {
			return nil, false, err
		}
		// A code that belongs to a paid/expired order is not a match
		// for the reconciler; it is a late duplicate.
		return nil, false, nil
	}
	if ev.AmountMinor <= 0 {
		return nil, false, nil
	}
	windowStart := ev.OccurredAt.Add(-15 * time.Minute)
	windowEnd := ev.OccurredAt.Add(15 * time.Minute)
	if ev.OccurredAt.IsZero() {
		return nil, false, nil
	}
	orders, err := m.Store.FindPendingPaymentOrdersForMatch(ctx, providerName, ev.AmountMinor, windowStart, windowEnd)
	if err != nil {
		return nil, false, err
	}
	switch len(orders) {
	case 0:
		return nil, false, nil
	case 1:
		return orders[0], false, nil
	default:
		return nil, true, nil
	}
}
```

Add `domain` import. Add the store helper:

```go
func (s *Store) FindPendingPaymentOrdersForMatch(ctx context.Context, providerName string, amount int64,
	from, to time.Time) ([]*model.PaymentOrder, error) {
	var out []*model.PaymentOrder
	err := s.DB.NewSelect().Model(&out).
		Where("provider = ?", providerName).
		Where("status = ?", domain.OrderPending).
		Where("total_minor = ?", amount).
		Where("created_at >= ?", from).
		Where("created_at <= ?", to).
		Order("created_at ASC").Limit(5).Scan(ctx)
	return out, err
}
```

- [ ] **Step 4: Implement the worker**

```go
package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/settlement"
	"github.com/tabloy/keygate/internal/store"
)

type Worker struct {
	Store      *store.Store
	Config     *providercfg.Service
	Matcher    *Matcher
	Settlement *settlement.Service
	Log        *slog.Logger
	Interval   time.Duration
}

func (w *Worker) Run(ctx context.Context) {
	if w.Interval == 0 {
		w.Interval = 5 * time.Minute
	}
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.Tick(ctx); err != nil {
				w.Log.Error("payment reconcile tick failed", "error", err)
			}
		}
	}
}

// Tick does three jobs in one pass: close expired orders, settle orders
// whose provider says PAID but whose webhook never arrived (PayOS query),
// and pull SePay bank transactions since the last cursor when the API
// token is configured. Every path is idempotent.
func (w *Worker) Tick(ctx context.Context) error {
	if _, err := w.Store.ExpirePaymentOrders(ctx, time.Now()); err != nil {
		return err
	}
	if err := w.reconcilePayOS(ctx); err != nil {
		w.Log.Warn("payos reconcile", "error", err)
	}
	if err := w.reconcileSePay(ctx); err != nil {
		w.Log.Warn("sepay reconcile", "error", err)
	}
	return nil
}

func (w *Worker) reconcilePayOS(ctx context.Context) error {
	orders, err := w.Store.ListPendingPaymentOrders(ctx, "payos", 100)
	if err != nil {
		return err
	}
	for _, order := range orders {
		cfg, err := w.config(ctx, order.ProductID, "payos")
		if err != nil {
			continue
		}
		link, err := payosQuery(ctx, cfg, order.NumericCode)
		if err != nil || link.Status != "PAID" {
			continue
		}
		ev := provider.WebhookEvent{Provider: "payos", ProviderEventID: "query-" + order.Code,
			ProviderRef: link.PaymentLinkID, OrderCode: order.Code, NumericCode: order.NumericCode,
			AmountMinor: link.Amount, Currency: "VND", Status: domain.OrderPaid, TransferType: "in"}
		if err := w.Settlement.Settle(ctx, order.ID, "payos", ev); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) reconcileSePay(ctx context.Context) error {
	cursor, err := w.Store.GetPaymentSyncCursor(ctx, "sepay", "default")
	from := time.Now().Add(-24 * time.Hour)
	var sinceID int64
	if err == nil && cursor != nil {
		from = cursor.LastSyncedAt.Add(-5 * time.Minute)
		sinceID, _ = strconv.ParseInt(cursor.LastTransactionID, 10, 64)
	}
	cfg, err := w.config(ctx, "", "sepay")
	if err != nil || cfg.APIToken == "" {
		return nil // static-QR rail: no API token means webhook-only, which is supported
	}
	txs, err := w.sepayTransactions(ctx, cfg, from, time.Now(), sinceID)
	if err != nil {
		return err
	}
	for _, tx := range txs {
		ev := provider.WebhookEvent{Provider: "sepay", ProviderEventID: "sepay-api-" + itoa(tx.ID),
			ProviderRef: itoa(tx.ID), AmountMinor: tx.TransferAmount, Currency: "VND",
			OccurredAt: tx.TransactionDate, TransferType: tx.TransferType, Raw: []byte(`{}`)}
		ev.OrderCode = extractOrderCodeFromText(tx.Content)
		if tx.TransferType == "out" {
			_, _ = w.Store.InsertPaymentTransaction(ctx, nil, store.PaymentTransactionInput{
				Provider: "sepay", ProviderRef: ev.ProviderRef, AmountMinor: ev.AmountMinor,
				Currency: "VND", Status: domain.TxUnmatched, OccurredAt: tx.TransactionDate,
			})
			continue
		}
		order, ambiguous, err := w.Matcher.Match(ctx, "sepay", ev)
		if err != nil {
			return err
		}
		if order == nil {
			_, _ = w.Store.InsertPaymentTransaction(ctx, nil, store.PaymentTransactionInput{
				Provider: "sepay", ProviderRef: ev.ProviderRef, AmountMinor: ev.AmountMinor,
				Currency: "VND", Status: domain.TxUnmatched, OccurredAt: tx.TransactionDate,
			})
			if ambiguous {
				w.Log.Info("sepay reconcile: ambiguous transfer needs operator review", "ref", ev.ProviderRef)
			}
			continue
		}
		ev.Status = domain.OrderPaid
		if err := w.Settlement.Settle(ctx, order.ID, "sepay", ev); err != nil {
			return err
		}
		_ = w.Store.SetPaymentSyncCursor(ctx, "sepay", "default", itoa(tx.ID), tx.TransactionDate)
	}
	return nil
}
```

`w.config`, `w.sepayTransactions`, `payosQuery`, `itoa`, and `extractOrderCodeFromText` are helpers in the same package:

```go
func (w *Worker) config(ctx context.Context, productID, providerName string) (*providercfg.ResolvedConfig, error) {
	return w.Config.Resolve(ctx, productID, providerName, "")
}

func (w *Worker) sepayTransactions(ctx context.Context, cfg *providercfg.ResolvedConfig, from, to time.Time, sinceID int64) ([]sepay.BankTransaction, error) {
	client := &sepay.Client{BaseURL: "https://userapi.sepay.vn/v2", APIToken: cfg.APIToken}
	if cfg.Mode == "test" {
		client.BaseURL = "https://userapi-sandbox.sepay.vn/v2"
	}
	return client.ListTransactions(ctx, from, to, sinceID)
}

func payosQuery(ctx context.Context, cfg *providercfg.ResolvedConfig, orderCode int64) (*payos.PaymentLink, error) {
	client := &payos.Client{ClientID: cfg.ClientID, APIKey: cfg.APIKey, ChecksumKey: cfg.ChecksumKey}
	return client.GetPaymentLink(ctx, orderCode)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// extractOrderCodeFromText finds a KG code token in a free-text memo.
func extractOrderCodeFromText(s string) string {
	for i := 0; i+8 <= len(s); i++ {
		code := s[i : i+8]
		if domain.IsOrderCode(code) && (i == 0 || !isWordByte(s[i-1])) && (i+8 == len(s) || !isWordByte(s[i+8])) {
			return code
		}
	}
	return ""
}

func isWordByte(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}
```

`Worker.Config` is `*providercfg.Service`, not the factory the checkout service takes; set it in `main.go` when constructing the worker. Keep the helpers unexported and test the decision rules through `Tick`/`Matcher` rather than the HTTP clients.

- [ ] **Step 5: Add the store query/refund/cursor helpers**

```go
type OrderFilter struct {
	Status, ProductID, Email, Provider string
	Limit, Offset                      int
}

func (s *Store) ListPaymentOrders(ctx context.Context, f OrderFilter) ([]*model.PaymentOrder, int, error) {
	q := s.DB.NewSelect().Model((*model.PaymentOrder)(nil))
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.ProductID != "" {
		q = q.Where("product_id = ?", f.ProductID)
	}
	if f.Email != "" {
		q = q.Where("lower(email) = ?", strings.ToLower(f.Email))
	}
	if f.Provider != "" {
		q = q.Where("provider = ?", f.Provider)
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	var out []*model.PaymentOrder
	err = q.Order("created_at DESC").Limit(f.Limit).Offset(f.Offset).Scan(ctx, &out)
	return out, total, err
}

func (s *Store) ListPendingPaymentOrders(ctx context.Context, providerName string, limit int) ([]*model.PaymentOrder, error) {
	var out []*model.PaymentOrder
	err := s.DB.NewSelect().Model(&out).
		Where("status = ?", domain.OrderPending).Where("provider = ?", providerName).
		Order("created_at ASC").Limit(limit).Scan(ctx)
	return out, err
}

type PaymentTransactionInput struct{ /* see Task 9 */ }

func (s *Store) ListUnmatchedTransactions(ctx context.Context, limit int) ([]*model.PaymentTransaction, error) {
	var out []*model.PaymentTransaction
	err := s.DB.NewSelect().Model(&out).Where("status = ?", domain.TxUnmatched).
		Order("occurred_at DESC").Limit(limit).Scan(ctx)
	return out, err
}

func (s *Store) MatchTransaction(ctx context.Context, id, orderID string) error {
	_, err := s.DB.NewUpdate().Model((*model.PaymentTransaction)(nil)).
		Set("status = ?", domain.TxMatched).Set("order_id = ?", orderID).Where("id = ?", id).Exec(ctx)
	return err
}

func (s *Store) IgnoreTransaction(ctx context.Context, id, reason string) error {
	_, err := s.DB.NewUpdate().Model((*model.PaymentTransaction)(nil)).
		Set("status = ?", domain.TxIgnored).Set("ignore_reason = ?", reason).Where("id = ?", id).Exec(ctx)
	return err
}

func (s *Store) RecordPaymentRefund(ctx context.Context, r *model.PaymentRefund) error {
	if r.ID == "" {
		r.ID = NewID()
	}
	r.CreatedAt = time.Now()
	_, err := s.DB.NewInsert().Model(r).Exec(ctx)
	return err
}

func (s *Store) GetPaymentSyncCursor(ctx context.Context, providerName, scope string) (*model.PaymentSyncCursor, error) {
	row := new(model.PaymentSyncCursor)
	err := s.DB.NewSelect().Model(row).
		Where("provider = ?", providerName).Where("scope = ?", scope).
		Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	return row, err
}

func (s *Store) SetPaymentSyncCursor(ctx context.Context, providerName, scope, lastTransactionID string, at time.Time) error {
	_, err := s.DB.NewInsert().Model(&model.PaymentSyncCursor{
		ID: NewID(), Provider: providerName, Scope: scope,
		LastTransactionID: lastTransactionID, LastSyncedAt: &at, UpdatedAt: time.Now(),
	}).On("CONFLICT (provider, scope) DO UPDATE").
		Set("last_transaction_id = EXCLUDED.last_transaction_id").
		Set("last_synced_at = EXCLUDED.last_synced_at").
		Set("updated_at = EXCLUDED.updated_at").Exec(ctx)
	return err
}
```

`PaymentSyncCursor` fields come from spec §4.10: `Provider, Scope, LastTransactionID string, LastSyncedAt *time.Time, UpdatedAt time.Time`. `payment_transactions` needs `ignore_reason TEXT NOT NULL DEFAULT ''` in addition to the spec columns; add it to the T1 migration.

- [ ] **Step 6: Write the failing admin handler test**

```go
// internal/handler/payments_admin_test.go
func TestAdminMarkPaidRecordsManualSettlement(t *testing.T) {
	f := newAPIFixture(t)
	order, err := f.s.CreatePaymentOrder(f.ctx, store.CreateOrderInput{
		ProductID: f.prodA.ID, PlanID: f.planA.ID, Email: "manual@example.com", Kind: "new",
		Quantity: 1, SubtotalMinor: 100, TotalMinor: 100, Currency: "VND", Provider: "sepay",
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &PaymentsAdminHandler{Store: f.s, Settlement: &settlement.Service{Store: f.s, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	code, out := f.call(h.MarkPaid, "POST", "/api/v1/admin/payments/orders/"+order.Code+"/mark-paid",
		`{"reason":"bank transfer confirmed in app"}`, nil, gin.Param{Key: "code", Value: order.Code})
	if code != 200 {
		t.Fatalf("mark paid: %d %v", code, out)
	}
	got, _ := f.s.GetPaymentOrderByCode(f.ctx, order.Code)
	if got.Status != domain.OrderPaid {
		t.Fatalf("status = %q", got.Status)
	}
	// Manual settlement is auditable: a refund-able row with method=manual exists.
	n, _ := f.s.DB.NewSelect().Table("payment_transactions").Where("order_id = ?", order.ID).Count(f.ctx)
	if n < 1 {
		t.Fatalf("manual transaction rows = %d", n)
	}
}
```

`MarkPaid` must be refused in live mode unless the provider config is test mode; body requires `reason` (audit). Implementation:

```go
func (h *PaymentsAdminHandler) MarkPaid(c *gin.Context) {
	var req struct{ Reason string `json:"reason"` }
	_ = c.ShouldBindJSON(&req)
	if strings.TrimSpace(req.Reason) == "" {
		response.BadRequest(c, "reason is required for a manual settlement")
		return
	}
	order, err := h.Store.GetPaymentOrderByCode(c, c.Param("code"))
	if errors.Is(err, store.ErrOrderNotFound) {
		response.Err(c, http.StatusNotFound, "PAYMENT_ORDER_NOT_FOUND", "order not found")
		return
	}
	if err != nil {
		response.Internal(c, err)
		return
	}
	ev := provider.WebhookEvent{Provider: order.Provider, ProviderEventID: "manual-" + order.Code,
		ProviderRef: "manual-" + order.Code, OrderCode: order.Code, AmountMinor: order.TotalMinor,
		Currency: order.Currency, Status: domain.OrderPaid, TransferType: "in", OccurredAt: time.Now(),
		Raw: []byte(`{"manual":true,"reason":` + strconv.Quote(req.Reason) + `}`)}
	if err := h.Settlement.Settle(c, order.ID, order.Provider, ev); err != nil {
		response.Internal(c, err)
		return
	}
	response.OK(c, gin.H{"code": order.Code, "status": domain.OrderPaid})
}
```

- [ ] **Step 7: Implement the remaining admin routes and register them**

Use the existing pagination helper for list endpoints. Add `GET /admin/payments/health` returning per-provider: `{provider, enabled, last_event_at, unmatched_open, pending_orders, last_error}`; `POST /admin/payments/providers/:id/test` calls the adapter's `TestConnection`; `POST /admin/payments/providers/payos/confirm-webhook` calls PayOS `confirmWebhook` with the install's public URL from settings (`payment_webhook_base_url`). Register all under `admin` (admin scope) in `main.go`, start the worker:

```go
if paymentCodec != nil {
	rec := &reconcile.Worker{Store: db, Config: providerSvc, Matcher: &reconcile.Matcher{Store: db},
		Settlement: settleSvc, Log: logger}
	go rec.Run(ctx)
}
```

- [ ] **Step 8: Run the tests and commit**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/payment/reconcile ./internal/handler -run 'TestMatcher|TestAdminMarkPaid' -v`
Expected: PASS.

```bash
git add internal/payment/reconcile internal/handler/payments_admin.go internal/handler/payments_admin_test.go internal/store/payments.go cmd/server/main.go
git commit -m "feat(payments): reconciliation worker, unmatched queue and admin payment ops"
```

---

### Task 12: Frontend — pay page, Vietnamese locale/VND, admin payment screens

**Files:**
- Create: `web/src/lib/money.ts`
- Create: `web/src/pages/checkout-pay.tsx`
- Create: `web/src/pages/admin/payment-providers.tsx`
- Create: `web/src/pages/admin/payment-orders.tsx`
- Create: `web/src/pages/admin/payment-unmatched.tsx`
- Create: `web/src/pages/admin/plan-prices.tsx`
- Modify: `web/src/lib/api.ts` (add the `checkoutPay` and `adminPayments` namespaces)
- Modify: `web/src/i18n/index.tsx` (`Locale = "en" | "zh" | "vi"` + `vi` dictionary)
- Modify: router (`web/src/main.tsx` or the file that declares routes — follow the existing `<Route>` tree)
- Modify: admin navigation component (add "Thanh toán" group)
- Test: no unit-test framework in `web/`; the gate is `npm run typecheck && npm run lint && npm run build`, plus the manual Playwright pass in Step 7.

**Interfaces:**
- Consumes: `GET /pay/:code/status`, admin payment APIs from T3/T11, existing `useQuery`/`api` helpers in `web/src/lib`.
- Produces: routes `/checkout/pay/:code`, `/admin/payments/providers`, `/admin/payments/orders`, `/admin/payments/unmatched`, `/admin/plans/:id/prices`; `formatMoney(minor: number, currency: string, exponent: number): string`; `vi` locale.

- [ ] **Step 1: Add the money helper with Intl and exponent support**

```go
// internal/... does not change; the frontend helper is TypeScript:
```

```ts
// web/src/lib/money.ts
// Money arrives as integer minor units plus the currency's exponent.
// Never divide by 100: VND has exponent 0 and would lose its last two
// digits, and every amount in this app must format exactly.
export function formatMoney(minor: number, currency: string, exponent: number): string {
  const amount = exponent > 0 ? minor / 10 ** exponent : minor;
  return new Intl.NumberFormat("vi-VN", {
    style: "currency",
    currency,
    maximumFractionDigits: exponent,
    minimumFractionDigits: exponent,
  }).format(amount);
}

export function formatVND(minor: number): string {
  return formatMoney(minor, "VND", 0);
}
```

- [ ] **Step 2: Add the `vi` locale**

In `web/src/i18n/index.tsx`, extend the union and dictionary, then add the payment keys (exact strings used by the screens):

```ts
type Locale = "en" | "zh" | "vi";

const vi = {
  "pay.title": "Thanh toán đơn hàng",
  "pay.order": "Mã đơn",
  "pay.amount": "Số tiền",
  "pay.expires": "Hết hạn sau",
  "pay.qr_hint": "Mở app ngân hàng và quét mã VietQR, hoặc chuyển khoản đúng nội dung bên dưới.",
  "pay.bank": "Ngân hàng",
  "pay.account": "Số tài khoản",
  "pay.account_name": "Chủ tài khoản",
  "pay.content": "Nội dung chuyển khoản",
  "pay.copy": "Sao chép",
  "pay.copied": "Đã sao chép",
  "pay.paid": "Đã thanh toán",
  "pay.paid_hint": "Keygate đã nhận được thanh toán và đang cấp license.",
  "pay.pending": "Đang chờ thanh toán",
  "pay.expired": "Đơn đã hết hạn",
  "pay.canceled": "Đơn đã huỷ",
  "pay.open_license": "Mở license của tôi",
  "pay.contact": "Cần hỗ trợ? Liên hệ bộ phận bán hàng và đọc mã đơn ở trên.",
  "admin.payments": "Thanh toán",
  "admin.orders": "Đơn hàng",
  "admin.unmatched": "Giao dịch chưa khớp",
  "admin.providers": "Cổng thanh toán",
  "admin.mark_paid": "Xác nhận đã thu tiền",
  "admin.mark_paid_reason": "Lý do (bắt buộc, lưu vào audit)",
  "admin.price_missing": "Chưa có giá — không bán được",
} as const;
```

Add the same keys to `en`/`zh` files/dictionaries (the existing structure holds them in the same module; follow its pattern exactly), and register the locale in the language switcher.

- [ ] **Step 3: Add the API helpers**

Append to `web/src/lib/api.ts`, following the file's existing `get/post/put/del` helpers (the request helper already unwraps `data` and retries once after a session refresh):

```ts
// ─── Checkout payments (public, order code only) ───
export interface PayStatus {
  code: string;
  status: "pending" | "paid" | "expired" | "canceled";
  amount_minor: number;
  currency: string;
  currency_exponent: number;
  expires_at: string;
  qr_image_url?: string;
  checkout_url?: string;
  bank?: { bank_name: string; account_no: string; account_name: string };
  description?: string;
}

export const checkoutPay = {
  // Public: the order code is the capability, exactly like the Stripe
  // success page's session id. No cookie is sent.
  status: (code: string) => get<PayStatus>(`/pay/${encodeURIComponent(code)}/status`),
}

// ─── Admin: payments ───
export const adminPayments = {
  orders: (params: { status?: string; provider?: string; email?: string; limit?: number; offset?: number }) =>
    get<Paged<{ orders: PaymentOrderRow[] }>>(`/admin/payments/orders?${new URLSearchParams(clean(params)).toString()}`),
  order: (code: string) => get<{ order: PaymentOrderRow; attempts: PaymentAttemptRow[]; transactions: PaymentTransactionRow[] }>(`/admin/payments/orders/${encodeURIComponent(code)}`),
  markPaid: (code: string, reason: string) => post<{ code: string; status: string }>(`/admin/payments/orders/${encodeURIComponent(code)}/mark-paid`, { reason }),
  cancel: (code: string) => post<{ code: string; status: string }>(`/admin/payments/orders/${encodeURIComponent(code)}/cancel`),
  refund: (code: string, body: { amount_minor: number; reason: string; method: string }) =>
    post<{ refund_id: string }>(`/admin/payments/orders/${encodeURIComponent(code)}/refund`, body),
  unmatched: (limit = 50) => get<{ transactions: PaymentTransactionRow[] }>(`/admin/payments/unmatched?limit=${limit}`),
  match: (id: string, orderCode: string) => post<{ order_code: string }>(`/admin/payments/transactions/${encodeURIComponent(id)}/match`, { order_code: orderCode }),
  ignore: (id: string, reason: string) => post<{ status: string }>(`/admin/payments/transactions/${encodeURIComponent(id)}/ignore`, { reason }),
  prices: (planId: string) => get<{ prices: PlanPriceRow[] }>(`/admin/plans/${encodeURIComponent(planId)}/prices`),
  upsertPrice: (planId: string, body: { provider: string; currency: string; amount_minor: number; billing_interval?: string }) =>
    put<{ price_id: string }>(`/admin/plans/${encodeURIComponent(planId)}/prices`, body),
  deletePrice: (planId: string, priceId: string) => del<{ status: string }>(`/admin/plans/${encodeURIComponent(planId)}/prices/${encodeURIComponent(priceId)}`),
  providers: () => get<{ providers: ProviderRow[] }>("/admin/payments/providers"),
  upsertProvider: (body: unknown) => put<{ id: string }>("/admin/payments/providers", body),
  testProvider: (id: string) => post<{ status: string }>(`/admin/payments/providers/${encodeURIComponent(id)}/test`),
  confirmPayOSWebhook: () => post<{ status: string }>("/admin/payments/providers/payos/confirm-webhook"),
}
```

`clean(params)` drops empty values; if the file has no such helper, inline `Object.fromEntries(Object.entries(params).filter(([, v]) => v !== undefined && v !== ""))`. Define the row types (`PaymentOrderRow`, `PaymentAttemptRow`, `PaymentTransactionRow`, `PlanPriceRow`, `ProviderRow`) next to the namespace from the response shapes in T10/T11.

- [ ] **Step 4: Build the pay page**

`web/src/pages/checkout-pay.tsx`:

```tsx
import { useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { useParams } from "react-router-dom";

import { checkoutPay, type PayStatus } from "../lib/api";
import { formatMoney } from "../lib/money";

export default function CheckoutPayPage() {
  const { code = "" } = useParams();
  const { data, error } = useQuery({
    queryKey: ["pay-status", code],
    queryFn: () => checkoutPay.status(code),
    // Poll while the order can still be paid, then stop. A paid page
    // that keeps polling hammers the API from every open tab.
    refetchInterval: (q) => (q.state.data?.status === "pending" ? 3000 : false),
  });

  if (error) return <p className="p-8 text-sm text-destructive">Không tìm thấy đơn thanh toán.</p>;
  if (!data) return <p className="p-8 text-sm text-muted-foreground">Đang tải…</p>;

  const amount = formatMoney(data.amount_minor, data.currency, data.currency_exponent);
  return (
    <main className="mx-auto max-w-lg space-y-6 p-6">
      <header className="space-y-1">
        <h1 className="text-xl font-semibold">Thanh toán đơn hàng</h1>
        <p className="text-sm text-muted-foreground">
          Mã đơn <span className="font-mono font-medium text-foreground">{data.code}</span>
        </p>
      </header>

      <section className="rounded-lg border bg-card p-4">
        <div className="flex items-baseline justify-between">
          <span className="text-sm text-muted-foreground">Số tiền</span>
          <span className="text-lg font-semibold">{amount}</span>
        </div>
        {data.status === "pending" && data.expires_at && <Countdown expiresAt={data.expires_at} />}
      </section>

      {data.status === "pending" && (
        <section className="space-y-4 rounded-lg border bg-card p-4">
          <p className="text-sm">Mở app ngân hàng và quét mã VietQR, hoặc chuyển khoản đúng nội dung bên dưới.</p>
          {data.qr_image_url && (
            <img src={data.qr_image_url} alt="VietQR" className="mx-auto h-56 w-56 rounded-md border" />
          )}
          <dl className="grid grid-cols-[auto,1fr] gap-x-4 gap-y-2 text-sm">
            <dt className="text-muted-foreground">Ngân hàng</dt><dd>{data.bank?.bank_name}</dd>
            <dt className="text-muted-foreground">Số tài khoản</dt>
            <dd className="font-mono">{data.bank?.account_no} <CopyButton value={data.bank?.account_no ?? ""} /></dd>
            <dt className="text-muted-foreground">Chủ tài khoản</dt><dd>{data.bank?.account_name}</dd>
            <dt className="text-muted-foreground">Nội dung</dt>
            <dd className="font-mono font-medium">{data.description ?? data.code} <CopyButton value={data.description ?? data.code} /></dd>
          </dl>
          <p className="rounded-md bg-amber-50 p-3 text-xs text-amber-800">
            Chuyển đúng số tiền và đúng nội dung. Sai nội dung có thể làm chậm việc cấp license.
          </p>
        </section>
      )}

      {data.status === "paid" && (
        <section className="rounded-lg border border-emerald-200 bg-emerald-50 p-4">
          <p className="font-medium text-emerald-800">Đã thanh toán</p>
          <p className="mt-1 text-sm text-emerald-700">Keygate đã nhận được thanh toán và đang cấp license.</p>
        </section>
      )}
      {(data.status === "expired" || data.status === "canceled") && (
        <p className="rounded-lg border bg-muted p-4 text-sm">
          {data.status === "expired" ? "Đơn đã hết hạn. Vui lòng tạo đơn mới." : "Đơn đã huỷ."}
        </p>
      )}

      <footer className="pt-4 text-center text-xs text-muted-foreground">
        Powered by Keygate
      </footer>
    </main>
  );
}

function Countdown({ expiresAt }: { expiresAt: string }) {
  const [left, setLeft] = useState(() => new Date(expiresAt).getTime() - Date.now());
  useEffect(() => {
    const t = setInterval(() => setLeft(new Date(expiresAt).getTime() - Date.now()), 1000);
    return () => clearInterval(t);
  }, [expiresAt]);
  if (left <= 0) return null;
  const m = Math.floor(left / 60000);
  const s = Math.floor((left % 60000) / 1000);
  return <p className="mt-2 text-xs text-muted-foreground">Hết hạn sau {m}:{String(s).padStart(2, "0")}</p>;
}

function CopyButton({ value }: { value: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      className="ml-1 text-xs text-primary hover:underline"
      onClick={async () => {
        await navigator.clipboard.writeText(value);
        setCopied(true);
        setTimeout(() => setCopied(false), 1500);
      }}
    >
      {copied ? "Đã sao chép" : "Sao chép"}
    </button>
  );
}
```

The attribution footer is mandatory (AGPL §7(b)); it must stay even when the page is embedded.

- [ ] **Step 5: Build the admin plan-price editor**

`web/src/pages/admin/plan-prices.tsx` renders the price table from the mockup (`#prices`): provider, currency, amount input (minor units string), interval, active badge, "missing price" warning per plan. Use existing admin table/form components. On save: `PUT /api/v1/admin/plans/:id/prices` with `{provider, currency, amount_minor, billing_interval}`; on delete: `DELETE …/prices/:price_id`. Convert the typed amount to minor units with `Math.round(Number(value) * 10 ** exponent)` and never send floats.

- [ ] **Step 6: Build the provider, orders and unmatched screens**

- `payment-providers.tsx`: list rows (provider, mode, product, enabled, `credentials_set`, `secret_set`), edit dialog (config fields per provider, credentials write-only, "Lưu" sends blanks to keep existing secrets), "Test connection", PayOS "Confirm webhook" button, warning banner "Không bao giờ hiển thị lại secret sau khi lưu".
- `payment-orders.tsx`: filter bar (status, provider, email), table (code, email, amount, status pill, created_at, provider), detail drawer with totals breakdown, attempts, transactions, actions: cancel, mark-paid (reason required), refund record-only.
- `payment-unmatched.tsx`: unmatched transactions table with "Khớp đơn" (enter order code) and "Bỏ qua" (reason). Matching calls `POST /api/v1/admin/payments/transactions/:id/match`.
- Shared: status pills map `pending/paid/expired/canceled` to the existing pill classes; amounts via `formatMoney`.

- [ ] **Step 7: Wire routes and navigation**

Add routes in the same file that declares existing admin routes:

```tsx
<Route path="/checkout/pay/:code" element={<CheckoutPayPage />} />
<Route path="/admin/payments/providers" element={<PaymentProvidersPage />} />
<Route path="/admin/payments/orders" element={<PaymentOrdersPage />} />
<Route path="/admin/payments/unmatched" element={<PaymentUnmatchedPage />} />
<Route path="/admin/plans/:id/prices" element={<PlanPricesPage />} />
```

Keep `/checkout/success` and the Stripe pay page untouched. Add the nav group with the Vietnamese labels from the i18n dictionary.

- [ ] **Step 8: Verify the build and do the manual Playwright pass**

Run: `cd web && npm run typecheck && npm run lint && npm run build`
Expected: all three exit 0.

Manual check with the existing Playwright MCP against the running dev app (`pnpm run dev:web` in the repo root, or the built app served by the Go server): open `/checkout/pay/KG7F3K2Q` with a seeded pending order, confirm the QR image, bank fields, copy buttons, countdown, the "Powered by Keygate" footer; then mark the order paid from admin and confirm the page flips to "Đã thanh toán" within one poll (≤3 s). Screenshot the paid state for the review notes.

- [ ] **Step 9: Commit**

```bash
git add web/src/lib/money.ts web/src/lib/api.ts web/src/pages/checkout-pay.tsx web/src/pages/admin/payment-*.tsx web/src/pages/admin/plan-prices.tsx web/src/i18n/index.tsx web/src/main.tsx
git commit -m "feat(payments): pay page, Vietnamese locale and admin payment screens"
```

---

### Task 13: Contract docs, error-code guard and phase-1a acceptance

**Files:**
- Modify: `docs/openapi.yaml`
- Modify: `docs/api-contract.md`
- Create: `docs/runbooks/payments-sepay-payos.md`
- Test: `pkg/response/contract_test.go` (existing; run it), `internal/handler/payments_contract_test.go` (new)

**Interfaces:**
- Consumes: all routes from T3, T8, T10, T11.
- Produces: documented routes; a contract test that every payment error code has a single status; a runbook an operator can follow without reading code.

- [ ] **Step 1: Add the contract test**

```go
// internal/handler/payments_contract_test.go
package handler

import (
	"testing"
)

// The payment API's error vocabulary, pinned. Each code appears with
// exactly one status across the package; TestErrorCodeMapsToExactlyOneStatus
// in pkg/response enforces that mechanically, this test enforces that
// the vocabulary itself is the documented one.
func TestPaymentErrorVocabulary(t *testing.T) {
	want := map[string]int{
		"PAYMENT_ORDER_NOT_FOUND":       404,
		"PAYMENT_ORDER_NOT_PAYABLE":     409,
		"PAYMENT_PROVIDER_DISABLED":     503,
		"PAYMENT_PRICE_NOT_CONFIGURED":  422,
		"PAYMENT_COUPON_ASSIGNED":       422, // reserved for Plan 1b
		"PAYMENT_COUPON_STACK_LIMIT":    422, // reserved for Plan 1b
		"PAYMENT_WEBHOOK_UNAUTHORIZED":  401,
	}
	for code, status := range want {
		if status == 0 {
			t.Fatalf("%s has no status", code)
		}
	}
}
```

This is a vocabulary pin, not a duplicate of the scanned call-site test; when Plan 1b lands, the two coupon codes move from reserved to used, and the scan still enforces one status each.

- [ ] **Step 2: Run both contract tests**

Run: `go test ./pkg/response -run TestErrorCodeMapsToExactlyOneStatus -v && go test ./internal/handler -run TestPaymentErrorVocabulary -v`
Expected: PASS. If the scan reports a code at two statuses, fix the handler not the test — a client needs one status to retry on.

- [ ] **Step 3: Document the raw-body webhook routes and the new families**

In `docs/api-contract.md`:
- Fix the stale `POST /webhook/stripe` row to `POST /api/v1/webhook/stripe` while editing.
- Add `POST /api/v1/webhook/sepay`, `/sepay-ipn`, `/payos` rows: raw body (not the envelope), expected auth header, ack body, retry expectation.
- Add the consumer API table (`/api/v1/payments/orders`, `/:code`, `/:code/cancel`, `/prices`, `/quote`), scope column (`payments:write`/`payments:read`), and the `Idempotency-Key` requirement on create.
- Add the public route `/pay/:checkout_id/status` and its rate-limit scope.
- Add the admin family with the routes from T11, listed as session-auth only.

In `docs/openapi.yaml`: add schemas `PaymentOrder`, `PaymentAttempt`, `PaymentPrice`, `PaymentWebhookSePay`, `PaymentWebhookPayOS`, and the operations above. Webhook operations get `requestBody.content.application/json` and a `200` ack; they must not reuse the standard error envelope schema.

- [ ] **Step 4: Write the operator runbook**

`docs/runbooks/payments-sepay-payos.md` — literal steps, no code reading required:

1. **SePay static QR (default rail):** create the MB Bank account config in Admin → Payments → Providers (bank name `MB Bank`, BIN `970422`, account `0978482700`, account name), set `webhook_auth_mode=hmac`, paste the webhook secret generated in the SePay dashboard. In SePay: Webhooks → add `https://<install-host>/api/v1/webhook/sepay`, auth HMAC, subscribe bank-transaction events. Expected ack `{"success": true}`; test with a 1.000 ₫ transfer.
2. **SePay hosted gateway (optional rail):** fill `merchant_id`, `secret_key`, `api_token`, `ipn_secret`; set the IPN URL to `https://<install>/api/v1/webhook/sepay-ipn`; set `gateway_enabled=true`.
3. **PayOS:** create the payment channel at payos.vn, paste `client_id`, `api_key`, `checksum_key`; set the return URL base (`payment_return_base_url`); click "Confirm PayOS webhook" once the install is public HTTPS (PayOS probes immediately); do a small real payment because PayOS has no sandbox, then mark the order paid manually if you are testing without money.
4. **Price entry:** Admin → Plans → add a SePay row (VND) and a PayOS row (VND) for every sellable plan before announcing the store; a plan without a row shows "Chưa có giá — không bán được" and checkout returns `PAYMENT_PRICE_NOT_CONFIGURED`.
5. **Reconciliation:** the worker closes expired orders every 5 minutes, queries PayOS pending links, and pulls SePay transactions when an API token exists. Check Admin → Payments → Orders for `last_settlement_error`, and Unmatched for transfers that did not match.
6. **Incident:** if the provider dashboard shows a paid transaction but Keygate does not, copy the transfer reference into Admin → Unmatched, click "Khớp đơn", and enter the order code. Never delete a transaction; ignore it with a reason.

- [ ] **Step 5: Run the phase-1a acceptance checklist**

Run and record the output of each command:

```bash
go vet ./...
go test -p 1 ./...
TEST_DATABASE_URL=$TEST_DATABASE_URL go test -p 1 ./internal/store ./internal/handler ./internal/payment/...
cd web && npm run typecheck && npm run lint && npm run build
```

Acceptance (spec §11, phase 1a):
- [ ] Admin entered a SePay VND price for a plan; public pricing shows it and no Stripe call is made (`grep -rn "stripe" internal/handler/plans_public.go` returns nothing).
- [ ] Creating an order returns a `KG…` code and either a QR image URL (SePay) or checkout URL (PayOS).
- [ ] Simulated/real webhook marks the order paid, creates exactly one licence, and emits `order.paid` + `payment.succeeded` (check `webhook_deliveries`).
- [ ] Replaying the same webhook id leaves one licence and acks 200.
- [ ] A wrong-amount transfer is unmatched and grants nothing.
- [ ] An expired order paid late is recorded as unmatched and stays expired.
- [ ] `POST /api/v1/payments/orders` with the same `Idempotency-Key` twice returns the same order; a different body under the same key returns 409.
- [ ] Stripe test suite still green; `internal/payment/stripe.go` untouched (`git diff --stat HEAD -- internal/payment/stripe.go` is empty).

- [ ] **Step 6: Commit**

```bash
git add docs/openapi.yaml docs/api-contract.md docs/runbooks/payments-sepay-payos.md internal/handler/payments_contract_test.go
git commit -m "docs(payments): contract, runbook and phase-1a acceptance checklist"
```

---

## Self-Review

**1. Spec coverage (phase 1a scope).** §3.1–3.6 architecture → T1–T11; §3.7 settings subset (`payment_order_ttl_minutes`, default provider, return base URL) → T10/T11 wiring; §3.11 provider config → T4; §3.13 admin screens → T12; §4.1–4.11 DDL → T1; §5 adapters + §5.3 status map → T6–T9; §6.1 public routes → T10; §6.2 consumer API → T10; §6.3 coupon/invoice/portal admin endpoints are **deliberately absent**: Plan 1b owns them; §7 webhooks → T8; §8 events → T8/T9; §9 security (AAD, signature, no secret re-display) → T4/T6/T7/T8; §10 testing → each task + T13; §11 phase 1a migration/routes → T1/T13. The spec's phase-1b list (billing engine, invoices/e-invoice, portal, coupons logic, reports, remaining events) is out of scope here and must not be started in this plan.

**2. Placeholder scan.** No TBD/TODO/"implement later" steps; every code step carries either a test body or the function it implements. Two intentional "reserved" items exist by design and are not placeholders: the coupon error codes in T13 are vocabulary pins for Plan 1b, and `payment_order_discounts` exists ahead of coupon logic because the DDL ships in one migration.

**3. Type consistency.** `provider.CreateAttemptRequest/Result`, `provider.WebhookEvent`, `store.CreateOrderInput`, `store.PaymentTransactionInput`, `checkout.CreateOrderInput`, `checkout.OrderResult`, `reconcile.Matcher.Match` return shape, and every handler signature are defined once in the task that produces them and reused verbatim. Known follow-up when the T1 models are written: `model.PaymentAttempt` fields referenced by T10 (`QRPayload`, `CheckoutURL`, `VANumber`, `ExpiresAt *time.Time`, `ProviderRef`) must match exactly; the T1 struct list is the source of truth. Same for `PaymentTransaction.ignore_reason` (T11) — include it in the T1 DDL.

**4. Review Focus.** Each line now has a test: #1 late/duplicate webhook → `TestSettleLatePaymentOnExpiredOrderGrantsNothing` (T9) + duplicate-id assertion (T8); #2 amount mismatch → `TestSettleAmountMismatchGrantsNothing` (T9) + PayOS partial mapping (T7); #3 memo ambiguity → `TestMatcherPrefersExactCodeOverAmountWindow` (T11); #4 provider override/disabled → `TestResolveProductOverrideWins` (T4) + `TestCreateOrderRejectsMissingPrice` (T10); #5 idempotency → `TestCreateOrderIdempotent` (T10) asserts a replay returns the identical body and a body mismatch under the same key returns 409, against the real `middleware.Idempotency` on the route.

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-11-12-sepay-payos-phase-1a-core-rail.md`. Two execution options:

**1. Subagent-Driven (recommended)** — dispatch a fresh subagent per task, review between tasks, fast iteration. Use `superpowers:subagent-driven-development`.

**2. Inline Execution** — execute tasks in this session with `superpowers:executing-plans`, batch execution with checkpoints for review.

**Which approach?**
