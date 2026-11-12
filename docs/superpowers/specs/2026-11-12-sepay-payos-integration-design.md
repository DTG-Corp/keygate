# SePay + PayOS integration — design spec

- **Status:** Draft — awaiting review
- **Date:** 2026-11-12
- **Scope:** Vietnamese payment rails (SePay, PayOS) plus a provider-neutral payment/licensing
  core that other internal services consume.
- **Related:** [api-contract.md](../../api-contract.md), [openapi.yaml](../../openapi.yaml),
  [AGENTS.md](../../../AGENTS.md)

---

## 0. Decisions locked with the product owner

| # | Decision | Choice |
|---|---|---|
| D1 | Revenue model for VN customers | **Subscriptions + one-time/perpetual**, with a Keygate-owned billing engine (neither SePay nor PayOS has recurring billing) |
| D2 | How other services consume payments | **Hybrid**: hosted payment page by default + headless API so a service can render its own QR/VA UI |
| D3 | Merchant / revenue routing | **Default merchant for the install + optional per-product override** (`product_id` nullable from day one) |
| D4 | Phase-1 must-haves | Core order/checkout/settlement/reconcile + billing engine, **VAT invoices/e-invoice**, **customer portal**, **coupons + tax/reconciliation reports**. Cards, per-order VA, payout, full refund workflow are phase 2 |
| D5 | Architecture | **Unified payment domain** (`Provider` interface + order/settlement/billing core). Stripe keeps its current code path in phase 1; `plan_prices` already maps Stripe prices so Stripe can move onto the shared path later |

Non-negotiable constraints carried from the existing project:

- AGPLv3 §7(b) attribution ("Powered by Keygate", `attribution_text`/`attribution_url`,
  `X-Powered-By`) must not be removed or altered by any new UI or API surface.
- Existing Stripe behaviour must not regress in phase 1.
- All migrations are additive; no destructive changes to existing tables.
- API responses use the `{"success":…,"data":…}` / `{"success":false,"error":{…}}` envelope
  except for explicitly documented raw-body routes.

---

## 1. Context

Keygate is a self-hosted license platform. Today the only payment integration is Stripe:

- `plans` has **no money columns**; the Stripe Price referenced by `plans.stripe_price_id` is
  the source of truth (`internal/handler/plans_public.go`).
- Checkout is a single handler bound to Stripe (`GET /pay/:checkout_id` →
  `StripeHandler.CheckoutByPlan` in `internal/payment/stripe.go`).
- Fulfilment, renewals, upgrades, dunning, refunds, invoices and subscription state all live in
  `internal/payment/stripe.go` (~3.8k lines) and are driven by Stripe events.
- The generic seams that already exist: `licenses.payment_provider`,
  `subscriptions.(payment_provider, external_id)`, `processed_events.provider` (claim/done
  idempotency), per-product outbound webhooks with HMAC + retry + replay, encrypted DB settings,
  and API-key scopes.

SePay and PayOS are Vietnamese **payment rails**, not billing engines:

- Both are VND-only, one-off, bank-transfer/QR oriented.
- Neither has subscriptions, saved cards, customer objects, dunning, proration or trials.
- PayOS has **no sandbox**, no refund API, no card acquiring, and caps non-linked-bank
  `description` at **9 characters**.
- SePay has a good sandbox, hosted checkout (VietQR, NAPAS, international cards), per-order
  virtual accounts, bank-transaction webhooks, query APIs, and refunds only for VietinBank
  enterprise accounts.
- SePay webhooks are retried for ~33 minutes and then dropped (>5 h never retried); PayOS
  retry policy is undocumented. **Reconciliation is mandatory, not optional.**

Therefore Keygate must own: prices, order lifecycle, billing cycles, dunning, invoices,
reconciliation and refunds — and treat providers as interchangeable settlement rails.

---

## 2. Goals and non-goals

### 2.1 Goals

1. Sell subscription, perpetual and trial plans to Vietnamese customers paying by bank transfer
   (VietQR), NAPAS QR, and later cards.
2. Make Keygate the single place that owns money and licences: orders, payments, refunds,
   invoices, reconciliation, and the licence state they produce.
3. Expose a stable, documented API + outbound events so other internal services can create
   payments, read licences/entitlements, and react to payment outcomes without knowing which
   provider was used.
4. Keep working with Stripe for international customers, without touching Stripe's code path in
   phase 1.
5. Give operators the tooling they need: unmatched-transfer queue, manual settlement, refund
   recording, provider health, reports/exports.
6. Support Vietnamese business requirements: VAT invoices/e-invoice, VND formatting, `vi`
   locale, Vietnam timezone, and marketing discounts (coupons with product/plan scopes,
   stacking, free-trial codes) that marketing can operate without engineering.

### 2.2 Non-goals (phase 1)

- Card acquiring (SePay gateway cards) — phase 2; the adapter interface is designed for it.
- Per-order virtual accounts (VA) — phase 2; the data model has `va_number` on attempts.
- Payouts / chi hộ (PayOS payouts, affiliate payouts) — phase 2.
- Full automated refund workflow (approval, provider execution) — phase 1 only records manual
  refunds; the `payment_refunds` table and admin flow are built so execution can be added.
- Migrating existing Stripe subscriptions/checkouts onto the unified path — phase 2+.
- Multi-currency for VN rails (VND only). Stripe keeps multi-currency.
- Multi-tenant organisations; "tenancy" remains one install with products, plus per-product
  provider overrides.
- Coupon stacking, gift cards, proration for downgrades.

---

## 3. Target architecture

### 3.1 Package layout

New code lives under `internal/payment/` next to the existing Stripe files. Stripe files are not
moved or refactored in phase 1.

```
internal/payment/
  domain/            # Money, Order, Attempt, Transaction, statuses, events (no I/O)
  provider/          # Provider interface, Capabilities, registry
  providers/
    sepay/           # SePay adapter (bank webhook, static QR, gateway, query, refund)
    payos/           # PayOS adapter (payment link, webhook, query, cancel)
    stripe/          # phase 2: adapter over the existing Stripe code
  checkout/          # order creation: price resolution, coupon, VAT, attempt dispatch
  settlement/        # idempotent settle: verify → paid → licence changes → invoice → events
  billing/           # renewal cycles, reminders, grace, dunning, cancel-at-period-end
  reconcile/         # cursors, provider polling, matching, unmatched queue
  invoicing/         # invoice numbering, PDF, e-invoice adapter
internal/store/
  payments.go        # orders, attempts, transactions, events, refunds, cursors
  payment_configs.go # provider configs (encrypted credentials)
  invoices.go        # invoices + counters
  coupons.go         # coupons + redemptions
internal/handler/
  payments_public.go # /pay/:checkout_id dispatcher (Stripe vs VN order) + /status
  payments_api.go    # consumer API (/api/v1/payments/*)
  payments_admin.go  # admin orders/transactions/unmatched/providers/coupons/invoices/reports
  payments_webhook.go# /webhook/sepay, /webhook/sepay-ipn, /webhook/payos
  payments_portal.go # portal orders/invoices/coupons
```

Rationale: `domain` and `provider` have no dependency on `store`/`handler`, so adapters can be
unit-tested and a fake provider can drive settlement/reconcile tests.

### 3.2 Provider interface

```go
// internal/payment/provider/provider.go
type Capabilities struct {
    HostedCheckout  bool // can redirect the customer to a provider-hosted page
    EmbeddedForm    bool // can be embedded in a service UI
    StaticQR        bool // QR can be rendered by Keygate without a provider API call
    VirtualAccount  bool // per-order VA supported
    Cards           bool
    Refund          bool
    PartialRefund   bool
    Payout          bool
    WebhookConfirm  bool // provider exposes a webhook-confirmation API (PayOS)
}

type Provider interface {
    Name() string // "sepay" | "payos" | "stripe"
    Capabilities() Capabilities

    // CreateCheckout starts a payment attempt. It either returns a redirect URL, a
    // QR payload, a virtual account, or a combination. Implementations must be
    // idempotent for the same order code.
    CreateCheckout(ctx context.Context, in CheckoutInput) (CheckoutResult, error)

    // ParseWebhook verifies the provider signature and normalises the payload.
    // It must return ErrInvalidSignature for bad signatures and must not mutate state.
    ParseWebhook(ctx context.Context, h http.Header, body []byte) (ProviderEvent, error)

    // QueryCheckout asks the provider for the current state of an attempt.
    QueryCheckout(ctx context.Context, providerRef string) (RemoteStatus, error)

    // CancelCheckout cancels an unpaid attempt when the provider supports it.
    CancelCheckout(ctx context.Context, providerRef string) error
}

// Optional interfaces, discovered by type assertion.
type Refunder interface {
    Refund(ctx context.Context, in RefundInput) (RefundResult, error)
    QueryRefund(ctx context.Context, providerRef string) (RefundStatus, error)
}
type WebhookConfirmer interface {
    ConfirmWebhook(ctx context.Context, url string) error
}
```

`CheckoutInput` carries: order code, amount (minor units + currency + exponent), description,
locale, expiry, buyer info, line items, success/cancel/return URLs, and the provider config
(credentials + non-secret config).

`CheckoutResult` carries: `ProviderRef`, `RedirectURL`, `FormHTML` (SePay gateway needs an
auto-submitting form), `QRPayload`, `VANumber`, `BankAccount`, `ExpiresAt`, `Raw`.

`ProviderEvent` carries: provider name, provider event id, normalised type
(`payment.succeeded`, `payment.failed`, `payment.voided`, `payment.pending`), order code,
provider ref, amount, currency, occurred-at, raw JSON.

### 3.3 Money, prices and VAT

- **Money is never a float.** `amount_minor BIGINT` + `currency CHAR(3)` + `currency_exponent
  SMALLINT` (VND = 0, USD/EUR = 2). Formatting is a presentation concern.
- `plan_prices` is the new source of truth for price:
  `(plan_id, provider, currency, billing_interval, amount_minor, currency_exponent,
  external_price_id, active)`.
  - Stripe rows keep `external_price_id = plans.stripe_price_id` after backfill.
  - VND rows have `external_price_id = ''`.
- Read path (`GET /api/v1/products/:product_slug/plans`) becomes **local-first** with a Stripe
  fallback only while the backfill has not run for a plan. The Stripe fallback is removed in
  phase 2.
- The existing public plans fields keep working (the same `price`/`currency`/interval fields);
  only the source of the number changes. Stripe checkouts continue to use Stripe's own
  invoices/receipts — the invoice module in §3.8 applies to VN orders in phase 1.
- Public plans response: keep the existing top-level `price` (minor units) and `currency` for
  backward compatibility, and add a `prices` array
  (`{provider, amount, currency, currency_exponent, billing_interval}`); the legacy top-level
  fields reflect the default provider.
- API money fields are integers in **minor units** (the repo convention — Stripe invoice
  `amount_due`/`amount_paid` already work this way). Every payment response includes
  `currency` + `currency_exponent` so a client can format zero-decimal VND correctly.
- Provider resolution when creating an order: explicit `provider` in the request → the
  product's configured default provider → the first enabled provider that has an active
  `plan_prices` row for the plan. The same order is payable through one provider at a time;
  switching provider creates a new attempt.
- Default order lifetime: 24 h (`payment_order_ttl_minutes` setting, admin-tunable; PayOS
  `expiredAt` and SePay QR instructions are clamped to the same value). Expired orders are
  never re-opened; a new order is created instead.
- Order creation **snapshots** subtotal, discount, tax and total on the order; later price edits
  never mutate historical orders.
- VAT:
  - `products.tax_rate_bp` and `products.price_includes_tax` (new columns, default 0 / true).
  - Order stores `tax_rate_bp`, `subtotal_minor`, `discount_minor`, `tax_minor`, `total_minor`.
  - `price_includes_tax = true` (VN default): `total_minor = subtotal_minor - discount_minor`;
    `net = round(total_minor * 10000 / (10000 + tax_rate_bp))`, `tax_minor = total_minor - net`
    — the VAT portion already contained in the total.
  - `price_includes_tax = false`: `tax_minor = round((subtotal_minor - discount_minor) *
    tax_rate_bp / 10000)` and `total_minor = subtotal_minor - discount_minor + tax_minor`.
  - Displayed price is tax-inclusive by default for VN; the invoice always shows the VAT
    breakdown separately.
- Rounding: all arithmetic in integer minor units with half-up rounding; totals are recomputed
  and asserted at settle time and must equal the snapshot stored on the order.

### 3.4 Order lifecycle

```
                 ┌──────────── canceled (admin/user, unpaid)
                 │
created ──► pending ──► paid ──► refunded / partially_refunded (manual in phase 1)
                 │
                 └──────────── expired (past expires_at, no payment)
```

- One `payment_order` can have **multiple attempts** (one per provider/retry). At most one
  attempt may reach `succeeded`; the order is paid by the first successful settlement.
- `kind`:
  - `new` — creates a licence (subscription, perpetual or trial conversion).
  - `renewal` — extends an existing subscription/perpetual maintenance licence.
  - `upgrade` — plan change with locally computed proration credit.
  - `addon` — seats/add-on purchase (phase 1: seats via quantity on `new`/`renewal`).
- Order code format: `KG` + 6 Crockford-Base32 characters (8 chars total).
  - Fits PayOS `description` (≤ 9 for non-payOS-linked banks).
  - Fits SePay payment-code config (prefix `KG`, suffix length 6).
  - Fits SePay VA `order_code` (6–50 alnum; VCB/Shinhan ≤ 15).
  - Generation is random + unique index; retry on collision.
- Zero-total orders (a `trial` coupon, or a 100 % comp code) skip the payment rail: the order is
  created as `paid` in the same transaction that creates the licence and records redemptions,
  with no `payment_attempts` row and `paid_at = now()`. An invoice is still recorded (total 0)
  but no e-invoice is issued unless the buyer asked for one.
- PayOS requires a numeric `orderCode`: every order gets a `numeric_code BIGINT` identity
  column (int64, unique). Use that as the PayOS `orderCode`, store it in
  `payment_attempts.provider_ref`, and put `order.code` in `description`. PayOS
  duplicate-create (`Đơn thanh toán đã tồn tại`) is handled by fetching the existing link by
  `orderCode`.

### 3.5 Settlement

Settlement is provider-neutral and idempotent. Triggered by a verified provider event or by the
reconciler. Algorithm (single DB transaction per step, row locks):

1. Resolve attempt/order:
   - by `(provider, provider_ref)`, then by `order.code` from the event, then by provider
     description/memo containing the code.
2. `SELECT … FOR UPDATE` the order. If already `paid`, record the transaction idempotently and
   return success.
3. Verify: provider matches, currency matches, `amount == order.total_minor` (exact for VND),
   order not expired/canceled. Mismatch → record transaction as `unmatched`, emit
   `payment.amount_mismatch` log/alert, do **not** grant.
4. Mark attempt `succeeded`, order `paid`, `paid_at = event.occurred_at` (bounded to now).
5. Apply the order:
   - `new`: create licence + subscription row in one transaction, reusing the locking/plan-
     revalidation shape of `store.CreateLicenseWithSubscription` but through a new
     `store.CreateLicenseFromOrder` that also stamps `licenses.payment_provider` and the
     subscription's `payment_provider`, `external_id` (= order code) and
     `current_period_start/end`.
   - `renewal`: `store.ExtendLicenseFromOrder` extends `valid_until` / `updates_until`, inserts
     the `license_renewals` ledger row, updates `subscriptions.current_period_start/end` and
     clears `past_due_at`.
   - `upgrade`: apply plan change + quantity, insert `license_upgrades` row.
   - `addon`: attach addon/seats.
6. Create the invoice record and enqueue PDF/e-invoice issuance.
7. Dispatch outbound events (`order.paid`, `payment.succeeded`) and enqueue emails.
8. All steps are idempotent under duplicate webhooks: unique keys on
   `payment_transactions(provider, provider_transaction_id)`, `payment_events(provider,
   provider_event_id)`, `invoices(order_id)` and `license_renewals` ledger uniqueness.

Zero-total orders (§3.4) bypass steps 1–6: settlement runs synchronously at creation inside the
same transaction as licence creation and coupon redemption; events and emails are dispatched
after commit like any other paid order.

Failure handling: transient errors roll back the transaction and let the webhook retry
(return non-2xx only for retryable errors; always ack verified-but-duplicate events). A
per-order `settlement_attempts` counter and `last_settlement_error` on the order support
operator visibility.

### 3.6 Billing engine (VN subscriptions)

Because providers cannot charge automatically, Keygate generates a new order each cycle.

- Hourly worker (`billing.Run`) scans active subscription licences whose
  `subscriptions.current_period_end` falls inside the plan's renewal lead window and that have
  no `pending` renewal order for that `period_end` (enforced by the partial unique index on
  `payment_orders (license_id, period_end) WHERE kind = 'renewal' AND status = 'pending'`; an
  expired pending renewal frees the slot for a new one).
- It creates a `renewal` order + attempt (static QR / payment link), then sends a reminder email.
- Reminder ladder (tags in `notifications`, unique per licence+tag to prevent duplicates):
  `renewal_reminder_7`, `renewal_reminder_3`, `renewal_reminder_1`, `renewal_due`,
  `renewal_grace_1`, `renewal_grace_3`, `renewal_grace_7`, `renewal_suspended`.
- On `period_end` without payment: licence `past_due`, `past_due_at = now`.
- After `plan.grace_days`: licence `suspended` (existing status), subscription `past_due`.
- Payment at any point before/inside grace → settle renewal, clear `past_due`, reactivate.
- After grace + `renewal_days` (or plan policy): licence `expired`.
- `cancel_at_period_end = true` → no renewal order is generated; at `period_end` the
  subscription becomes `canceled` and the licence follows the existing expiry rules.
- Trial plans: at `trial_end`, billing creates the first paid order (or expires the trial if
  no payment method is required by policy).
- Proration for `upgrade` is computed at order creation:
  `credit = round(unused_days / period_days * current_period_price)` (only for time-based
  plans), `amount = prorated_new_price - credit`, clamped to ≥ 0; policy flag
  `proration_mode = immediate | next_cycle` on the plan (default `immediate`).

### 3.7 Reconciliation

Reconciliation is a first-class subsystem, not a fallback.

- Cursors: `payment_sync_cursors(provider, scope, last_transaction_id, last_synced_at)` where
  `scope` is the bank account id (SePay) or `default` (PayOS).
- Every 5 minutes:
  - **SePay**: `GET /v2/transactions` from `last_synced_at - 1h`, `transaction_date_sort=asc`,
    `per_page=100`, page until exhausted, respect 3 req/s with backoff. Upsert every
    transaction into `payment_transactions` keyed by `(provider, id)`.
  - **PayOS**: for pending orders with an attempt older than 2 minutes,
    `GET /v2/payment-requests/{orderCode}`; settle if `PAID`, expire if `EXPIRED/CANCELLED`.
  - Expire pending orders whose `expires_at` has passed; call provider cancel when supported.
  - Match unmatched transactions (see below) and retry failed settlements.
- Matching rules (in order):
  1. `payment_transactions` already linked to an attempt/order (idempotent replay).
  2. Payment code parsed by SePay (`code`) equals an order code.
  3. Order code appears in `content`/`description` (case-insensitive, diacritics stripped).
  4. `provider_ref` matches an attempt (VA number, PayOS orderCode, gateway invoice number).
  5. Amount + currency + time window (±72 h) + same provider account — **only** produces a
     "suggested" match that an operator must confirm.
- Anything not auto-matched becomes `unmatched` and appears in the admin queue. Operators can:
  link to an order (settle), create a licence manually, mark as other income, or ignore; every
  action is audited.
- Daily 00:10 Asia/Ho_Chi_Minh: full previous-day reconciliation + report (revenue by provider,
  fees if configured, unmatched, drift vs. provider totals).
- Alerting: unmatched count, webhook failure rate, orders paid at provider but not settled,
  cursor lag.

### 3.8 Invoices and e-invoice

- Every paid order gets an invoice record. Numbering: `KG-<YYYY>-<000001>`, per-year counter
  (`invoice_counters` row locked with `UPDATE … RETURNING`).
- Invoice content: seller block (from branding settings), buyer block (from order.buyer),
  line items (plan name, quantity, unit price, VAT rate), subtotal, discount, VAT, total, and a
  note "Powered by Keygate" footer where the template shows it (attribution must remain).
- PDF generation: server-side from an HTML template, stored in object storage
  (`invoices/<year>/<number>.pdf`) when storage is configured, otherwise generated on demand
  and streamed. The concrete PDF library is chosen in the implementation plan; it must not add
  a headless browser dependency.
- E-invoice adapter:

```go
type EInvoiceProvider interface {
    Issue(ctx context.Context, inv Invoice) (ExternalRef, error)
    Void(ctx context.Context, ref ExternalRef) error
    Status(ctx context.Context, ref ExternalRef) (string, error)
}
```

  - Configuration: provider name + credentials in `payment_provider_configs`
    (`provider = 'einvoice:<vendor>'`), admin selectable.
  - Default phase-1 behaviour: if an e-invoice provider is configured, issue on payment; if
    not, invoice status is `pending_issue` and appears in an admin queue for manual issuance.
  - Buyer tax information is collected at checkout when the buyer requests a VAT invoice
    (`buyer.tax_code` required in that case).

### 3.9 Coupons

Coupons are marketing discounts owned by Keygate. Two families exist: **shared codes**
(`stack_group = general` or `campaign`) and **private codes** (`stack_group = private`, optionally
bound to one buyer email) — so a general campaign code and a hand-out code for a single customer
can be combined on the same order.

- Types:
  - `percent` — `value` in basis points (1000 = 10 %).
  - `fixed` — `value` in minor units of `currency`.
  - `trial` — `trial_days`; no money changes hands: the order is created with `total_minor = 0`
    and settles at creation (§3.4, §3.5), granting a trial licence for
    `max(coupon.trial_days, plan.trial_days)`. Only for `kind = new` on **trial-capable plans**
    (`plans.trial_days > 0`); a plan without a trial rejects the code with
    `PAYMENT_COUPON_INVALID`.
- Scope lives in `coupon_scopes` (0..n rows): an empty set means **all products**; otherwise the
  coupon applies when the order's product/plan is one of the listed targets. A coupon can target
  several products and/or plans in the same list; a plan target covers exactly that plan, a
  product target covers every plan of that product.
- Stacking:
  - An order may carry up to `coupon_max_stack` **amount** coupons (installation setting,
    default 2; set to 1 to disable stacking).
  - At most one coupon per `stack_group`: `general` + `campaign` + `private` can stack with each
    other, but two `general` codes cannot.
  - Each coupon in the stack must be `stackable = true`; a non-stackable coupon is valid only
    alone.
  - A `trial` coupon cannot be combined with any amount coupon.
  - Application order is `priority` ascending, then `code`; `percent` is computed on the
    remaining subtotal after previous discounts, `fixed` subtracts from it; the sum of discounts
    is capped at the subtotal and the order total never goes below 0.
  - `payment_order_discounts` snapshots one row per applied coupon (`coupon_code`, `type`,
    `value`, `amount_minor`, `priority`) so invoices and reports stay correct after a coupon is
    edited or deleted.
- Validation happens at quote/order creation (and again inside order creation); the hosted
  payment page only displays the codes already attached to the order. **Redemption is written at
  settlement**, one `coupon_redemptions` row per coupon per order
  (`UNIQUE(order_id, coupon_code)`), so abandoned orders never consume inventory and a stacked
  order records each code separately.
- Per-customer identity: `external_customer_id` when provided, else lowercased email. A coupon
  with `assigned_email` is **hard-blocked** to that buyer: no admin override, no fallback to a
  different email, and no "apply anyway" path in checkout or API. Admins who need to move a
  private code reassign `assigned_email` on the coupon (audited) instead.
- Admin keeps the full redemption history (`GET /api/v1/admin/payments/redemptions`) with the
  discount amount per code, plus per-coupon counters — needed for marketing attribution and VAT
  reporting.

### 3.10 Consumer API and outbound events

Other services must not see provider specifics. They use:

- `POST /api/v1/payments/orders` (idempotent) — create an order and get back either a
  `checkout_url` (hosted) or headless data (`qr_payload`, `va_number`, `bank_account`,
  `expires_at`) plus a `status` URL.
- `GET /api/v1/payments/orders/{code}` — status + attempts + licence reference.
- `POST /api/v1/payments/orders/{code}/cancel`.
- `GET /api/v1/payments/orders` — filter by `external_customer_id`, `status`, `from`, `to`.
- `GET /api/v1/payments/prices` — plan prices by provider/currency.
- `GET /api/v1/payments/invoices/{id}` — JSON; `?format=pdf` for the PDF.

New scopes (enforced at the route layer; product binding enforced in middleware):
`payments:write`, `payments:read`. Existing `licenses:write` keeps covering licence reads/CRUD
in phase 1; adding read-only `licenses:read` / `entitlements:read` scopes is a phase-2 change
because it would relax the current admin/`licenses:write` checks on many routes.

Outbound webhook events (added to `model.WebhookEvents` + `IsValidWebhookEvent`):

- `order.created`, `order.paid`, `order.expired`, `order.canceled`
- `payment.succeeded`, `payment.failed`, `payment.refunded`
- `invoice.issued`, `invoice.voided`
- `license.renewal_due`

Payload shape follows the existing convention `{event, timestamp, data}` with `data` containing
the order, licence and amount fields. Existing `license.*` events are unchanged.

### 3.11 Configuration, secrets and modes

- `payment_provider_configs` holds per-provider settings:
  - `provider` (`sepay`|`payos`), `product_id` (null = install default), `mode` (`test`|`live`),
    `enabled`, `config JSONB` (non-secret: merchant id, bank account, display name, gateway
    host, IP allowlist toggle), `credentials_encrypted`, `webhook_secret_encrypted`.
  - Unique `(provider, product_id, mode)` with `NULLS NOT DISTINCT`.
- Secrets are encrypted with AES-256-GCM using a new HKDF subkey derived from the existing
  master key (`RELEASE_KEY_ENCRYPTION_KEY`, purpose `payment-config`), AAD
  `payment_provider_config:<id>:<field>`. Missing master key → writes fail with the existing
  `ErrSecretEncryptionUnavailable` semantics.
- Admin `GET /admin/payments/providers` returns `secrets_set` booleans only; `PUT` accepts blank
  to keep the current secret; `DELETE …/secrets/{field}` clears it.
- Mode isolation: `mode` is part of the config row; a live order never uses test credentials and
  vice versa. The server environment does not force a mode (self-hosted installs may run both),
  but the admin UI shows a persistent warning for `test` mode and test orders are tagged
  `metadata.test = true`.
- Feature flags: a provider is usable only if a config row exists, is `enabled`, and has valid
  credentials; otherwise its prices are hidden from checkout and its routes return
  `PAYMENT_PROVIDER_DISABLED`.
- New install settings (stored with the existing settings store): `payment_order_ttl_minutes`
  (default 1440), `coupon_max_stack` (default 2), `payment_quote_rate_limit` (default 30/min/IP),
  plus the VAT seller block and e-invoice credentials referenced in §3.8.

### 3.12 Localization

- Add `vi` to `web/src/i18n/index.tsx` (`Locale = "en" | "zh" | "vi"`), `locales/vi.ts`,
  browser detection, and a `language` setting enum.
- VND formatting via `Intl.NumberFormat("vi-VN", { style: "currency", currency: "VND" })`;
  amounts are stored as integers.
- Timestamps stored UTC; displayed in `Asia/Ho_Chi_Minh` by default for VN surfaces.
- Email templates: payment confirmation, renewal reminders, dunning ladder, invoice issued —
  all available in `vi`.

### 3.13 Web UI

The frontend is React 19 + Vite + react-router-dom 7 + TanStack Query + Tailwind 4
(Radix/shadcn). New surfaces:

- `/checkout/pay/:code` — customer payment page: amount, order summary, provider tabs
  (SePay QR / PayOS link), QR image or bank-transfer block, copyable memo, countdown to
  expiry, status polling (`GET /pay/:checkout_id/status` every 3 s with backoff), success and
  expired states. Coupons are shown read-only (chips with code + discount, plus a note that a
  new order is needed to change them); the page never re-prices an order. No auth required; the
  order code is the capability. It must never grant a licence by itself — it only reads status.
- Admin → **Payments**: orders list/detail, unmatched transfers, transactions, provider
  configuration (secrets show as "configured"), invoices (issue/void/PDF), reports.
- Admin → **Payments → Coupons**: list + editor (type percent/fixed/trial, currency, value or
  trial days, scope picker for products and plans, stack group, stackable, priority, assigned
  email, redemption limits, validity), live quote preview, per-coupon usage stats, and a global
  **redemption history** page (filter by coupon/email/date, CSV export).
- Admin → **Plans**: price rows editor per provider/currency/interval (replaces the raw
  `stripe_price_id` input as the primary editor; the Stripe price id moves to an
  "advanced/legacy" field that writes both `plans.stripe_price_id` and the Stripe
  `plan_prices` row).
- Admin → **Settings**: payment section pointing at the provider configs, VAT defaults,
  invoice seller block.
- Portal → orders (pay/renew), invoices (download), billing profile (buyer/tax details).
- Pricing displays read `plan_prices` (local-first), with VND formatting and VAT note.

Attribution surfaces (footer, "Powered by Keygate", `X-Powered-By`) remain untouched on every
new page.

---

## 4. Data model

All tables are new and additive. Types are PostgreSQL; Bun models follow existing conventions
(snake_case JSON, `*_at` RFC3339, no nullable slices).

Primary keys are UUID strings (`TEXT PRIMARY KEY`, generated with `store.NewID()`), matching
every existing table, and every foreign key is therefore `TEXT` too. IDs are never exposed as
numeric IDs; PayOS's required numeric `orderCode` uses the dedicated `numeric_code` identity
column on `payment_orders` instead.

### 4.1 `plan_prices`

```sql
CREATE TABLE plan_prices (
    id                TEXT PRIMARY KEY,
    plan_id           TEXT NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
    provider          TEXT NOT NULL,                -- 'stripe' | 'sepay' | 'payos'
    currency          CHAR(3) NOT NULL,             -- 'VND', 'USD'
    amount_minor      BIGINT NOT NULL CHECK (amount_minor >= 0),
    currency_exponent SMALLINT NOT NULL DEFAULT 2,
    billing_interval  TEXT NOT NULL DEFAULT '',     -- '', 'month', 'year'
    external_price_id TEXT NOT NULL DEFAULT '',     -- Stripe price id when provider='stripe'
    active            BOOLEAN NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX plan_prices_unique_active
    ON plan_prices (plan_id, provider, currency, billing_interval) WHERE active;
```

### 4.2 `payment_provider_configs`

```sql
CREATE TABLE payment_provider_configs (
    id                      TEXT PRIMARY KEY,
    provider                TEXT NOT NULL,
    product_id              TEXT NULL REFERENCES products(id) ON DELETE CASCADE,
    mode                    TEXT NOT NULL CHECK (mode IN ('test','live')),
    enabled                 BOOLEAN NOT NULL DEFAULT FALSE,
    config                  JSONB NOT NULL DEFAULT '{}'::jsonb,
    credentials_encrypted   TEXT NOT NULL DEFAULT '',
    webhook_secret_encrypted TEXT NOT NULL DEFAULT '',
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX payment_provider_configs_unique
    ON payment_provider_configs (provider, product_id, mode) NULLS NOT DISTINCT;
```

### 4.3 `payment_orders`

```sql
CREATE TABLE payment_orders (
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
CREATE INDEX payment_orders_status_idx ON payment_orders (status, expires_at);
CREATE INDEX payment_orders_license_idx ON payment_orders (license_id, created_at DESC);
CREATE INDEX payment_orders_email_idx ON payment_orders (lower(email), created_at DESC);
CREATE UNIQUE INDEX payment_orders_renewal_once
    ON payment_orders (license_id, period_end) WHERE kind = 'renewal' AND status = 'pending';
```

### 4.4 `payment_attempts`

```sql
CREATE TABLE payment_attempts (
    id            TEXT PRIMARY KEY,
    order_id      TEXT NOT NULL REFERENCES payment_orders(id) ON DELETE CASCADE,
    provider      TEXT NOT NULL,
    provider_ref  TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL CHECK (status IN
                    ('created','pending','succeeded','failed','expired','canceled')),
    checkout_url  TEXT NOT NULL DEFAULT '',
    qr_payload    TEXT NOT NULL DEFAULT '',
    va_number     TEXT NOT NULL DEFAULT '',
    expires_at    TIMESTAMPTZ,
    raw           JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX payment_attempts_provider_ref
    ON payment_attempts (provider, provider_ref) WHERE provider_ref <> '';
CREATE INDEX payment_attempts_order_idx ON payment_attempts (order_id);
```

### 4.5 `payment_transactions`

```sql
CREATE TABLE payment_transactions (
    id                      TEXT PRIMARY KEY,
    provider                TEXT NOT NULL,
    provider_transaction_id TEXT NOT NULL,
    order_id                TEXT NULL REFERENCES payment_orders(id) ON DELETE SET NULL,
    attempt_id              TEXT NULL REFERENCES payment_attempts(id) ON DELETE SET NULL,
    amount_minor            BIGINT NOT NULL,
    currency                CHAR(3) NOT NULL,
    direction               TEXT NOT NULL CHECK (direction IN ('in','out')),
    occurred_at             TIMESTAMPTZ NOT NULL,
    provider_account        TEXT NOT NULL DEFAULT '',
    reference               TEXT NOT NULL DEFAULT '',
    content                 TEXT NOT NULL DEFAULT '',
    match_status            TEXT NOT NULL CHECK (match_status IN ('matched','unmatched','ignored')),
    matched_by              TEXT NULL REFERENCES users(id) ON DELETE SET NULL,
    matched_at              TIMESTAMPTZ,
    raw                     JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX payment_transactions_provider_id
    ON payment_transactions (provider, provider_transaction_id);
CREATE INDEX payment_transactions_unmatched_idx
    ON payment_transactions (match_status, occurred_at DESC);
CREATE INDEX payment_transactions_order_idx ON payment_transactions (order_id);
```

### 4.6 `payment_events`

```sql
CREATE TABLE payment_events (
    id                TEXT PRIMARY KEY,
    provider          TEXT NOT NULL,
    provider_event_id TEXT NOT NULL,
    event_type        TEXT NOT NULL,
    order_id          TEXT NULL REFERENCES payment_orders(id) ON DELETE SET NULL,
    raw               JSONB NOT NULL,
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at      TIMESTAMPTZ,
    status            TEXT NOT NULL DEFAULT 'received'
);
CREATE UNIQUE INDEX payment_events_unique ON payment_events (provider, provider_event_id);
```

`processed_events` remains the claim/done idempotency mechanism; `payment_events` is the audit
trail of raw payloads.

### 4.7 `payment_refunds`

```sql
CREATE TABLE payment_refunds (
    id           TEXT PRIMARY KEY,
    order_id     TEXT NOT NULL REFERENCES payment_orders(id) ON DELETE RESTRICT,
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    currency     CHAR(3) NOT NULL,
    reason       TEXT NOT NULL DEFAULT '',
    method       TEXT NOT NULL CHECK (method IN ('manual','sepay','payos','stripe')),
    provider_ref TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL CHECK (status IN ('requested','pending','succeeded','failed')),
    created_by   TEXT NULL REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX payment_refunds_order_idx ON payment_refunds (order_id);
```

### 4.8 `invoices` and `invoice_counters`

```sql
CREATE TABLE invoice_counters (
    year     INT PRIMARY KEY,
    last_seq BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE invoices (
    id                TEXT PRIMARY KEY,
    number            TEXT NOT NULL UNIQUE,
    order_id          TEXT NOT NULL UNIQUE REFERENCES payment_orders(id) ON DELETE RESTRICT,
    license_id        TEXT NULL REFERENCES licenses(id) ON DELETE SET NULL,
    buyer             JSONB NOT NULL DEFAULT '{}'::jsonb,
    items             JSONB NOT NULL DEFAULT '[]'::jsonb,
    subtotal_minor    BIGINT NOT NULL,
    discount_minor    BIGINT NOT NULL DEFAULT 0,
    tax_minor         BIGINT NOT NULL DEFAULT 0,
    total_minor       BIGINT NOT NULL,
    currency          CHAR(3) NOT NULL,
    currency_exponent SMALLINT NOT NULL,
    status            TEXT NOT NULL CHECK (status IN ('draft','issued','pending_issue','void')),
    pdf_path          TEXT NOT NULL DEFAULT '',
    e_invoice_provider TEXT NOT NULL DEFAULT '',
    e_invoice_ref     TEXT NOT NULL DEFAULT '',
    e_invoice_status  TEXT NOT NULL DEFAULT '',
    issued_at         TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### 4.9 `coupons`, `coupon_scopes`, `payment_order_discounts`, `coupon_redemptions`

```sql
CREATE TABLE coupons (
    id                 TEXT PRIMARY KEY,
    code               TEXT NOT NULL UNIQUE,
    description        TEXT NOT NULL DEFAULT '',
    type               TEXT NOT NULL CHECK (type IN ('percent','fixed','trial')),
    value              BIGINT NOT NULL DEFAULT 0 CHECK (value >= 0),
    trial_days         INT NOT NULL DEFAULT 0 CHECK (trial_days >= 0),
    currency           CHAR(3) NULL,
    stack_group        TEXT NOT NULL DEFAULT 'general'
                         CHECK (stack_group IN ('general','campaign','private')),
    stackable          BOOLEAN NOT NULL DEFAULT TRUE,
    priority           INT NOT NULL DEFAULT 100,
    assigned_email     TEXT NOT NULL DEFAULT '',
    max_redemptions    INT NOT NULL DEFAULT 0,   -- 0 = unlimited
    redeemed_count     INT NOT NULL DEFAULT 0,
    per_customer_limit INT NOT NULL DEFAULT 0,   -- 0 = unlimited
    valid_from         TIMESTAMPTZ,
    valid_to           TIMESTAMPTZ,
    active             BOOLEAN NOT NULL DEFAULT TRUE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (type <> 'trial' OR trial_days > 0),
    CHECK (type = 'trial' OR value > 0)
);

-- Empty scope set = all products. Otherwise every listed target is covered.
CREATE TABLE coupon_scopes (
    coupon_id   TEXT NOT NULL REFERENCES coupons(id) ON DELETE CASCADE,
    scope_type  TEXT NOT NULL CHECK (scope_type IN ('product','plan')),
    scope_id    TEXT NOT NULL,
    PRIMARY KEY (coupon_id, scope_type, scope_id)
);
CREATE INDEX coupon_scopes_target_idx ON coupon_scopes (scope_type, scope_id);

CREATE TABLE payment_order_discounts (
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
CREATE INDEX payment_order_discounts_coupon_idx ON payment_order_discounts (coupon_id);

CREATE TABLE coupon_redemptions (
    id                    TEXT PRIMARY KEY,
    coupon_id             TEXT NOT NULL REFERENCES coupons(id) ON DELETE RESTRICT,
    coupon_code           TEXT NOT NULL,
    order_id              TEXT NOT NULL REFERENCES payment_orders(id) ON DELETE CASCADE,
    email                 TEXT NOT NULL DEFAULT '',
    external_customer_id  TEXT NOT NULL DEFAULT '',
    type                  TEXT NOT NULL CHECK (type IN ('percent','fixed','trial')),
    amount_minor          BIGINT NOT NULL DEFAULT 0,
    trial_days            INT NOT NULL DEFAULT 0,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (order_id, coupon_code)
);
CREATE INDEX coupon_redemptions_customer_idx
    ON coupon_redemptions (coupon_id, email, external_customer_id);
CREATE INDEX coupon_redemptions_coupon_idx
    ON coupon_redemptions (coupon_id, created_at DESC);
```

Migration order: `coupons` → `coupon_scopes` → `payment_orders` → `payment_order_discounts` →
`coupon_redemptions` (the redemption table references both orders and coupons).

### 4.10 `payment_sync_cursors`

```sql
CREATE TABLE payment_sync_cursors (
    id                  TEXT PRIMARY KEY,
    provider            TEXT NOT NULL,
    scope               TEXT NOT NULL DEFAULT 'default',
    last_transaction_id TEXT NOT NULL DEFAULT '',
    last_synced_at      TIMESTAMPTZ,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX payment_sync_cursors_unique ON payment_sync_cursors (provider, scope);
```

### 4.11 Changes to existing tables

- `products`: add `tax_rate_bp INT NOT NULL DEFAULT 0`, `price_includes_tax BOOLEAN NOT NULL
  DEFAULT TRUE`.
- `plans`: no changes. `billing_interval` remains; `plan_prices` rows must match it.
- `licenses`: no new columns. `payment_provider` gains values `sepay`/`payos`. VN licences keep
  provider refs in `subscriptions.external_id` (set to the first order code) and in
  `payment_orders.license_id`.
- `subscriptions`: no schema change; VN rows use `payment_provider = sepay|payos`,
  `external_id = first order code`, and `current_period_start/end` advanced by the billing
  engine.
- `license_renewals`: add nullable `payment_order_id TEXT REFERENCES payment_orders(id)` plus a
  partial unique index (`WHERE payment_order_id IS NOT NULL`) so a VN renewal is idempotent
  without abusing the Stripe columns (they keep `''` defaults).
- `license_upgrades`: same `payment_order_id` addition, and relax
  `stripe_checkout_session_id` from `NOT NULL` (no default) to `NOT NULL DEFAULT ''`, because
  a VN upgrade has no Stripe checkout session and the existing unique index is a partial index
  on non-empty values.
- `model.WebhookEvents` / `IsValidWebhookEvent`: add the payment events listed in §3.10.
- `model.AllScopes`: add `payments:write`, `payments:read` (read-only licence scopes are a
  phase-2 item, see §3.10).
- `docs/api-contract.md`: add the three raw-body webhook routes and the new API families. The
  existing row `POST /webhook/stripe` is stale — the registered route is
  `POST /api/v1/webhook/stripe` (`stripe_webhook_setup.go` builds the same URL); fix it while
  adding the new webhook rows.
- `docs/openapi.yaml`: new schemas/paths.

---

## 5. Provider adapters

### 5.1 SePay

Credentials (per config row, encrypted): `merchant_id`, `secret_key`, `api_token`,
`ipn_secret`, `webhook_secret`. Non-secret config: `bank_account` (bank code, account number,
account name), `gateway_enabled`, `static_qr_enabled`, `webhook_auth_mode`
(`hmac`|`apikey`|`none`), `ip_allowlist_enabled`.

Hosts: sandbox `pay-sandbox.sepay.vn` (checkout) and `userapi-sandbox.sepay.vn/v2` (API v2;
the sandbox REST host for the gateway order endpoints is taken from SePay's sandbox page and
must be re-verified at implementation time — the production host is `pgapi.sepay.vn`);
production `pay.sepay.vn` / `pgapi.sepay.vn` / `userapi.sepay.vn/v2`.

**A. Static QR (default phase-1 rail, no provider API call).**

- Build QR image URL:
  `https://vietqr.app/img?acc=<account>&bank=<bank>&amount=<int>&des=<order code>&template=compact`
  (no auth). Store as `payment_attempts.qr_payload`, `provider_ref = order.code`.
- Settlement source: SePay bank webhook (`transferType=in`, `code`/`content` matches order code).
- Cancel: local expiry only (a bank transfer cannot be cancelled).

**B. Hosted gateway (VietQR/NAPAS in phase 1; cards phase 2).**

- Create: form POST (auto-submit HTML page served by Keygate) to
  `<checkout host>/v1/checkout/init` with fields:
  `merchant`, `currency=VND`, `order_amount` (integer VND), `operation=PURCHASE`,
  `order_description`, `order_invoice_number=<order code>` (must be unique),
  `payment_method` (optional; omit for chooser), `customer_id`, `success_url`, `error_url`,
  `cancel_url`, `lang=vi|en`, `signature`.
- Signature: canonical string is the ordered list
  `order_amount, merchant, currency, operation, order_description, order_invoice_number,
  customer_id, payment_method, success_url, error_url, cancel_url`, dropping fields whose value
  is empty/`0`/`"0"`, joined as `field=value` with commas; then
  `base64(HMAC-SHA256(secret_key, canonical))` (Base64, not hex).
- Settlement source: IPN (`X-Secret-Key` header, `notification_type=ORDER_PAID`,
  `order.order_status=CAPTURED`, `transaction.transaction_status=APPROVED`). `TRANSACTION_VOID`
  → `payment.voided`.
- Query: `GET <pgapi>/v1/order/detail/{order_invoice_number}` with Basic auth.
- Cancel: `POST <pgapi>/v1/order/cancel` for unpaid bank-transfer orders; card void is phase 2.
- Refund: phase 2 (API v2, VietinBank only, `X-Idempotency-Key` required).

**C. Bank webhook ingestion (always on when the provider is enabled).**

- Route `POST /api/v1/webhook/sepay`.
- Verification modes: HMAC (`X-SePay-Signature: sha256=<hex>` over `{X-SePay-Timestamp}.{raw
  body}`, ±300 s), API key (`Authorization: Apikey <key>`), or none (test only).
- Optional IP allowlist (7 IPv4 + 2 IPv6 from SePay docs).
- Ack exactly `{"success": true}` with HTTP 200 (or 201) within 30 s; any other body/status is
  treated as failure by SePay.
- Payload fields: `id` (stable idempotency key), `gateway`, `transactionDate` (VN local,
  no offset), `accountNumber`, `subAccount`, `code` (may be null), `content`,
  `transferType` (`in`/`out`), `description`, `transferAmount` (integer VND), `accumulated`,
  `referenceCode`.
- Dedupe on `id`. Store every transaction (in and out) in `payment_transactions`.

**D. Reconciliation API.**

- `GET /v2/transactions` with `transaction_date_from/to`, `since_id`,
  `transaction_date_sort=asc`, `per_page` ≤ 100, `timestamp_format=iso8601`; Bearer token.
- Rate limit 3 req/s per IP: global limiter per config row + `Retry-After` handling.
- `since_id` does not reorder results; always page fully with ascending sort to avoid gaps.

### 5.2 PayOS

Credentials (encrypted): `client_id`, `api_key`, `checksum_key`. Non-secret config:
`channel_name`, `payout_enabled` (phase 2).

Host: `https://api-merchant.payos.vn`. Headers: `x-client-id`, `x-api-key`,
`Content-Type: application/json`.

- **Create:** `POST /v2/payment-requests`
  body: `orderCode` (int64 = `payment_orders.numeric_code`), `amount` (integer VND), `description` =
  order code (8 chars), `buyerName/buyerCompanyName/buyerTaxCode/buyerEmail/buyerPhone/buyerAddress` (from order.buyer),
  `items` (one line item per plan quantity, each with `taxPercentage` matching the order
  tax rate), `cancelUrl`, `returnUrl`, `expiredAt`
  (int32, must be ≤ 2038-01-19), `signature`.
  Request signature: `HMAC_SHA256(checksum_key, "amount=<a>&cancelUrl=<c>&description=<d>&
  orderCode=<o>&returnUrl=<r>")`, hex.
  Response: `data.{checkoutUrl, qrCode (EMVCo string), paymentLinkId, status, expiredAt}` plus a
  top-level `signature` over `data`; verify it.
- **Query:** `GET /v2/payment-requests/{orderCode}` → `status`, `amountPaid`,
  `amountRemaining`, `transactions[]`.
- **Cancel:** `POST /v2/payment-requests/{orderCode}/cancel` with `cancellationReason`
  (only unpaid links).
- **Webhook:** `POST /api/v1/webhook/payos`; payload
  `{code, desc, success, data:{orderCode, amount, description, accountNumber, reference,
  transactionDateTime (VN local), currency, paymentLinkId, code, desc, counterAccount*,
  virtualAccount*}, signature}`.
  Verification: `HMAC_SHA256(checksum_key, canonical(data))` where canonical is
  alphabetically sorted `key=value` pairs, arrays JSON-stringified, null/`"null"`/`"undefined"`
  → `""`, hex; constant-time compare. Ack any 2xx (empty body is fine).
- **Register webhook:** `POST /confirm-webhook` with `{"webhookUrl": "…"}`. PayOS immediately
  probes the URL with a sample transaction; the URL must be public HTTPS and answer 2xx. This is
  an explicit admin action ("Confirm PayOS webhook"), not automatic on boot.
- **Idempotency:** `orderCode` is the only key; a duplicate create returns
  `Đơn thanh toán đã tồn tại` → adapter fetches the existing link instead of failing.
- **No sandbox:** test with a real small amount in production; test orders are tagged
  `metadata.test = true` and can be marked paid manually in a dedicated test config row.
- **No refunds:** refunds are recorded manually (`method=manual`); PayOS payouts are phase 2.

### 5.3 Status mapping

| Provider signal | Normalised | Order/attempt effect |
|---|---|---|
| SePay webhook `transferType=in`, code matches | `payment.succeeded` | settle |
| SePay webhook `transferType=out` | `payment.out` | record only (not an order payment) |
| SePay IPN `ORDER_PAID` + `CAPTURED` + `APPROVED` | `payment.succeeded` | settle |
| SePay IPN `TRANSACTION_VOID` | `payment.voided` | attempt `failed`; order stays pending/expired |
| PayOS webhook `code="00"`, `success=true`, `data.code="00"` | `payment.succeeded` | settle |
| PayOS `UNDERPAID` | `payment.partial` | record transaction; order stays pending; operator top-up/review |
| PayOS `EXPIRED`/`CANCELLED` | `payment.expired`/`payment.canceled` | attempt closed; order expired/canceled |
| Query API `PAID` | `payment.succeeded` | settle (reconciliation path) |

---

## 6. HTTP API

All new JSON routes use the standard envelope and pagination helpers. Auth column:
`public` = no auth, `key` = API key with scope, `session` = admin/portal JWT.

### 6.1 Public

| Method | Path | Auth | Notes |
|---|---|---|---|
| GET | `/pay/:checkout_id` | public | **Existing route, extended into a dispatcher** — `cs_…` goes to the current Stripe handler; `KG…` answers `302` to the SPA pay page `/checkout/pay/<code>`. Gin allows only one wildcard name at a path position, so the existing `/pay/:checkout_id` route and param name are reused; a second `/pay/:code` route would panic on startup |
| POST | `/api/v1/payments/quote` | public | Dry-run total for `{plan_id, quantity, email, coupon_codes[]}`; returns discounts/tax/total with **no order created**; rate-limited `payments_public` |
| GET | `/pay/:checkout_id/status` | public | JSON status for polling (`status`, `amount`, `currency`, `expires_at`, `paid_at`, `qr_payload`, `va_number`, `bank_account`) |
| POST | `/api/v1/webhook/sepay` | signature | Bank webhook; raw body; ack `{"success": true}` |
| POST | `/api/v1/webhook/sepay-ipn` | `X-Secret-Key` | Gateway IPN; raw body; ack 200 |
| POST | `/api/v1/webhook/payos` | signature | PayOS webhook; raw body; ack 2xx |

There are **no new server-side success/cancel routes**: the SPA already owns `/checkout/success`,
and VN flows return to `/pay/:checkout_id` (or the service's own `success_url`), which polls the
status endpoint. `POST /api/v1/checkout/verify` and the Stripe checkout page stay unchanged.

Rate-limit scopes: `sepay_webhook`, `sepay_ipn`, `payos_webhook`, `payments_public`.

### 6.2 Consumer API

`POST /api/v1/payments/orders` (scope `payments:write`, `Idempotency-Key` required)

```json
{
  "kind": "new",
  "plan_id": "3f2a9c10-6b7e-4d21-9c55-0a1b2c3d4e5f",
  "email": "buyer@example.com",
  "provider": "sepay",
  "quantity": 1,
  "external_customer_id": "ws_123",
  "coupon_codes": ["TET2026", "ACME10"],
  "buyer": {
    "name": "Nguyen Van A",
    "company_name": "ACME JSC",
    "tax_code": "0101234567",
    "address": "1 Le Loi, District 1, HCMC",
    "email": "buyer@example.com",
    "phone": "+84901234567",
    "request_invoice": true
  },
  "success_url": "https://service.example.com/billing/return",
  "cancel_url": "https://service.example.com/billing/cancel",
  "metadata": { "workspace_id": "ws_123" }
}
```

Response `201`:

```json
{
  "success": true,
  "data": {
    "code": "KG7F3K2Q",
    "status": "pending",
    "kind": "new",
    "provider": "sepay",
    "amount": 971000,
    "currency": "VND",
    "currency_exponent": 0,
    "subtotal": 1190000,
    "discount": 219000,
    "tax": 88273,
    "total": 971000,
    "discounts": [
      {"code": "TET2026", "type": "percent", "amount": 119000},
      {"code": "ACME10", "type": "fixed", "amount": 100000}
    ],
    "expires_at": "2026-11-12T17:00:00+07:00",
    "checkout_url": "https://keygate.example.com/pay/KG7F3K2Q",
    "status_url": "https://keygate.example.com/api/v1/payments/orders/KG7F3K2Q",
    "qr_payload": "000201010212…",
    "va_number": "",
    "bank_account": { "bank": "VCB", "account_number": "0071000123456", "account_name": "ACME JSC" }
  }
}
```

- `kind` defaults to `new`. `renewal` and `upgrade` require `license_id`; for `upgrade`,
  `plan_id` is the target plan and the server computes the prorated amount. For `renewal`,
  `plan_id` is optional (the licence's current plan is used).
- `provider` optional; defaults to the product default provider or the first enabled provider
  with a price for the plan (§3.3).
- `success_url`/`cancel_url` must be `https` and must not contain credentials; they are only
  used for browser redirects (no server-side fetch).
- The same endpoint is used for headless integrations: the service renders `qr_payload` /
  `va_number` / `bank_account` itself and polls `status_url` or listens to webhooks.
- `coupon_codes` is optional; coupons are validated and stacked per §3.9, and the discount
  breakdown is returned in `discounts[]`. A rejected code fails the request with
  `PAYMENT_COUPON_*` (no silent drop) unless the caller passes `ignore_invalid_coupons: true`
  for best-effort marketing flows.
- `tax` is the VAT portion contained in `total` when `price_includes_tax` is on; it is never added
  twice.
- Order creation does not consume coupon inventory; redemption is recorded at settlement.

Other consumer routes:

| Method | Path | Scope | Notes |
|---|---|---|---|
| GET | `/api/v1/payments/orders/{code}` | `payments:read` | Order + attempts + licence ref |
| POST | `/api/v1/payments/orders/{code}/cancel` | `payments:write` | Cancels unpaid order and provider attempt |
| GET | `/api/v1/payments/orders` | `payments:read` | Filters: `status`, `external_customer_id`, `email`, `from`, `to`; pagination |
| GET | `/api/v1/payments/prices` | `payments:read` | `?product_id=&provider=&currency=` |
| GET | `/api/v1/payments/invoices/{id}` | `payments:read` | JSON; `?format=pdf` streams PDF |

### 6.3 Admin

| Method | Path | Notes |
|---|---|---|
| GET | `/api/v1/admin/payments/providers` | Configs + `secrets_set` booleans |
| PUT | `/api/v1/admin/payments/providers/{provider}` | Create/update config; blank secret keeps current |
| DELETE | `/api/v1/admin/payments/providers/{provider}/secrets/{field}` | Clear a secret |
| POST | `/api/v1/admin/payments/providers/{provider}/test` | Test connection |
| POST | `/api/v1/admin/payments/providers/payos/confirm-webhook` | Calls PayOS `confirm-webhook` |
| GET | `/api/v1/admin/payments/health` | Per-provider health: last webhook, cursor lag, pending, unmatched |
| GET | `/api/v1/admin/payments/orders` | Filters: status, provider, kind, email, code, date range |
| GET | `/api/v1/admin/payments/orders/{code}` | Detail + attempts + transactions + events |
| POST | `/api/v1/admin/payments/orders/{code}/mark-paid` | Manual settlement; reason required; audited |
| POST | `/api/v1/admin/payments/orders/{code}/cancel` | Cancel unpaid order |
| POST | `/api/v1/admin/payments/orders/{code}/refund` | Record a refund (manual in phase 1) |
| GET | `/api/v1/admin/payments/transactions` | Filters: provider, direction, match_status, date range |
| GET | `/api/v1/admin/payments/unmatched` | Unmatched queue |
| POST | `/api/v1/admin/payments/transactions/{id}/match` | `{order_code}` or `{action: "ignore"|"other_income"}` |
| GET | `/api/v1/admin/coupons` | List; filters `q`, `active`, `stack_group`, `scope_type`; pagination |
| POST | `/api/v1/admin/coupons` | Create with inline `scopes[]` (`{scope_type, scope_id}`) |
| GET/PUT | `/api/v1/admin/coupons/{id}` | Detail/update (scopes replaced atomically) |
| DELETE | `/api/v1/admin/coupons/{id}` | Delete only when it has no redemptions; otherwise archive via `active=false` |
| GET | `/api/v1/admin/coupons/{id}/redemptions` | Per-coupon redemption history |
| GET | `/api/v1/admin/payments/redemptions` | Global redemption history; filters `coupon`, `email`, `from`, `to`; CSV |
| GET | `/api/v1/admin/payments/invoices` | Invoice list; filters |
| POST | `/api/v1/admin/payments/invoices/{id}/issue` | Issue/retry e-invoice |
| POST | `/api/v1/admin/payments/invoices/{id}/void` | Void invoice |
| GET | `/api/v1/admin/payments/reports/revenue` | Group by provider/plan/product/period |
| GET | `/api/v1/admin/payments/reports/reconciliation` | Daily reconciliation + drift |
| GET | `/api/v1/admin/payments/reports/vat` | VAT report by period |
| GET | `/api/v1/admin/payments/reports/*?format=csv` | CSV export |

All admin write routes require the `admin` scope (existing behaviour) and are audited via
`store.Audit`.

### 6.4 Portal

| Method | Path | Notes |
|---|---|---|
| GET | `/api/v1/portal/payments/orders` | Customer's orders |
| GET | `/api/v1/portal/payments/orders/{code}` | Order + payment instructions |
| POST | `/api/v1/portal/payments/orders/{code}/pay` | Re-issue/refresh payment attempt |
| POST | `/api/v1/portal/payments/orders/{code}/cancel` | Cancel unpaid order |
| GET | `/api/v1/portal/invoices` | Invoice list |
| GET | `/api/v1/portal/invoices/{id}` | JSON or `?format=pdf` |
| POST | `/api/v1/portal/coupons/validate` | `{code, plan_id, quantity}` → discount preview |
| GET/PUT | `/api/v1/portal/billing-profile` | Buyer/invoice details |

### 6.5 Error codes

One code ⇔ one HTTP status (enforced by the existing error-code test). Proposed additions:

| Code | Status | When |
|---|---|---|
| `PAYMENT_PROVIDER_UNKNOWN` | 400 | Unknown provider name |
| `PAYMENT_PROVIDER_DISABLED` | 503 | Provider not configured/enabled (mirrors `STRIPE_NOT_CONFIGURED`) |
| `PAYMENT_ORDER_NOT_FOUND` | 404 | Unknown order code |
| `PAYMENT_ORDER_NOT_PAYABLE` | 409 | Order not pending (paid/expired/canceled) |
| `PAYMENT_PRICE_NOT_CONFIGURED` | 409 | No active `plan_prices` row for provider/currency |
| `PAYMENT_AMOUNT_MISMATCH` | 422 | Settle amount differs from order total |
| `PAYMENT_SIGNATURE_INVALID` | 400 | Webhook signature invalid |
| `PAYMENT_CURRENCY_UNSUPPORTED` | 422 | Provider does not support the currency |
| `PAYMENT_TAX_CODE_REQUIRED` | 422 | `buyer.request_invoice=true` without a valid `tax_code` |
| `PAYMENT_COUPON_INVALID` | 422 | Coupon unknown/expired/not applicable |
| `PAYMENT_COUPON_EXHAUSTED` | 409 | Redemption limit reached |
| `PAYMENT_COUPON_STACK_LIMIT` | 422 | More amount coupons than `coupon_max_stack` |
| `PAYMENT_COUPON_GROUP_CONFLICT` | 422 | Two coupons from the same `stack_group` |
| `PAYMENT_COUPON_NOT_STACKABLE` | 422 | A non-stackable coupon was combined with another |
| `PAYMENT_COUPON_TRIAL_MIXED` | 422 | A `trial` coupon was combined with amount coupons |
| `PAYMENT_COUPON_ASSIGNED` | 422 | Coupon is bound to another email (`assigned_email`) |
| `PAYMENT_PROVIDER_ERROR` | 502 | Provider API failure after retries |
| `PAYMENT_REFUND_UNSUPPORTED` | 422 | Provider/rail cannot execute refunds |
| `INVOICE_NOT_FOUND` | 404 | Unknown invoice |
| `INVOICE_ALREADY_ISSUED` | 409 | Issue/void on a non-draft invoice |
| `EINVOICE_PROVIDER_ERROR` | 502 | E-invoice provider failure |

Existing codes (`PRODUCT_SCOPE_MISMATCH`, `INSUFFICIENT_SCOPE`, etc.) are reused.

---

## 7. Webhook ingestion

All three routes are raw-body exceptions (added to `docs/api-contract.md`):

1. Read body with a size cap (256 KiB).
2. Verify signature before any DB work.
3. Insert `payment_events` (unique `provider, provider_event_id`) — duplicate → ack success.
4. Reserve `processed_events(provider, event_id)` with the existing claim/done protocol; release
   on transient error.
5. Store `payment_transactions` (idempotent upsert on provider transaction id).
6. Settle synchronously when possible; if settlement is slow, ack and let the reconciler finish
   (the webhook ack budget is 30 s for SePay; PayOS is more forgiving but keep it short).
7. Always ack in the provider's expected format:
   - SePay webhook: HTTP 200, body exactly `{"success": true}`.
   - SePay IPN: HTTP 200.
   - PayOS: any 2xx (an empty body is fine).

Provider-specific notes:

- SePay HMAC replay window: reject `|now - X-SePay-Timestamp| > 300 s`.
- SePay IPN `X-Secret-Key` compare in constant time.
- PayOS webhook has no documented retry policy → the reconciler must treat every pending order
  as potentially un-notified.

---

## 8. Outbound webhook events

Added to the existing per-product webhook subscriptions (HMAC + retry + claim-lease + replay):

| Event | Data |
|---|---|
| `order.created` | `order{code, kind, provider, status, total, currency, expires_at, product_id, plan_id}` |
| `order.paid` | order + `license{id, key_prefix, plan_id, valid_until}` + `payment{provider, provider_ref, paid_at}` |
| `order.expired` | order |
| `order.canceled` | order |
| `payment.succeeded` | `order` + `payment{provider, amount, currency, reference, occurred_at}` |
| `payment.failed` | `order` + `payment{failure_reason}` |
| `payment.refunded` | `order` + `refund{amount, reason, method}` |
| `invoice.issued` | `invoice{id, number, total, currency, pdf_url?}` |
| `invoice.voided` | `invoice{id, number}` |
| `license.renewal_due` | `license{id, key_prefix, valid_until}` + `order{code, total, expires_at}` |

Existing `license.*` events keep their current payloads and semantics.

---

## 9. Security

- Webhook signatures verified before parsing/DB work; constant-time comparisons.
- Replay protection: SePay timestamp window; PayOS/SePay event-id dedupe; transaction id dedupe.
- Amount/currency/merchant verified at settlement; redirects (`success_url`, PayOS `returnUrl`)
  never grant a licence.
- Provider credentials encrypted at rest (AES-GCM, HKDF subkey `payment-config`), never returned
  by the API, never logged. `config` JSONB holds only non-secret values.
- SSRF: provider base URLs are fixed constants per mode (no user-supplied URLs); webhook
  outbound uses the existing SSRF-hardened client.
- Open redirect: `success_url`/`cancel_url` validated as absolute `https` URLs without
  credentials; admin-configurable allowlist per API key is a phase-2 hardening item.
- No card data is stored; card fields from SePay IPN are masked and only raw payloads are kept
  for audit with PAN-like fields redacted before persistence (store masked values only).
- Rate limits: per route family scope; webhook routes also support optional IP allowlist
  (SePay egress IPs). The public quote route (`POST /api/v1/payments/quote`) is limited by
  `payment_quote_rate_limit` and answers a generic `PAYMENT_COUPON_INVALID` for
  unknown/expired/not-applicable codes, so it cannot be used to enumerate valid coupon codes.
- Audit: every admin action (mark paid, refund, match, provider config change, coupon
  create/update/delete, redemption status change, invoice issue/void, CSV export of redemptions)
  writes `audit_logs`.
- AGPL attribution surfaces untouched.

### 9.1 Observability

- Structured logs (existing logger) with `payment.order_code`, `provider`, `provider_ref`,
  `event_id`, `amount_minor`, `settlement_result`; secrets and full PANs never logged.
- Metrics/counters: orders created/paid/expired, settle latency, webhook verification failures,
  duplicate events, settle failures by reason, unmatched transfers count, cursor lag seconds,
  billing run outcome, invoice issuance failures, coupon rejections by error code, stacked
  orders count, discount amount by coupon code.
- Admin health: `GET /api/v1/admin/payments/health` returns per-provider status
  (last webhook received, last successful reconcile, cursor lag, pending orders, unmatched
  count) for the dashboard.
- Alerts (email/webhook to operators): unmatched count above threshold, orders paid at the
  provider but not settled for >15 min, webhook failure rate, reconcile worker not running,
  provider credential missing while enabled.

---

## 10. Testing

- **Unit**
  - Signature vectors: SePay HMAC (`{timestamp}.{body}`), SePay gateway Base64 signature,
    PayOS create-link signature, PayOS webhook canonical string, PayOS payout signature.
  - Money/VAT/coupon arithmetic, order-code generation, status mapping, proration.
  - Provider payload parsers with literal JSON fixtures from the docs.
- **Integration (Postgres via `TEST_DATABASE_URL`, `go test -p 1`)**
  - Settlement idempotency: same webhook twice, two concurrent webhooks, duplicate transaction.
  - Renewal order uniqueness under concurrent billing runs.
  - Coupon stacking: `general` + `private` stack to the expected amounts; two `general` codes
    rejected; `trial` + amount rejected; `coupon_max_stack` honoured; total discount capped at
    subtotal; `assigned_email` hard-blocked (different buyer email → `PAYMENT_COUPON_ASSIGNED`,
    no override path); `trial` coupon rejected on a plan with `trial_days = 0`; redemptions
    written only at settlement and unique per `(order, coupon)`.
  - VAT with `price_includes_tax`: net/VAT split and total stay consistent after stacking.
  - Coupon redemption race.
  - Reconciliation matching rules and unmatched transitions.
  - Invoice numbering concurrency.
- **Provider fakes**
  - In-memory `Provider` implementation for checkout/settlement/reconcile tests.
  - HTTP stubs for SePay/PayOS clients (injectable `BaseURL` + `HTTPClient`, same pattern as
    `stubStripe`).
- **Sandbox E2E**
  - SePay test mode: simulate transaction → webhook → settle; gateway sandbox redirect + IPN.
  - PayOS: manual checklist with a small real amount (create link, pay, webhook, cancel,
    expire), documented in the runbook; test config row tagged `mode=test`.
- **Regression**
  - Existing Stripe test suite must stay green (`go test -p 1 ./...`).
  - `TestErrorCodeMapsToExactlyOneStatus` covers new error codes.

---

## 11. Rollout and phases

### Phase 1a — Core rail (first shippable slice)

1. Migrations, in dependency order: `plan_prices` + Stripe backfill; `payment_provider_configs`;
   `coupons` + `coupon_scopes` (tables only — CRUD lands in 1b, created now so migrations stay
   additive and the editor needs no second schema change); `payment_orders`;
   `payment_order_discounts`; `payment_attempts`; `payment_transactions`; `payment_events`;
   `payment_refunds`; `coupon_redemptions`; `payment_sync_cursors`; `products.tax_rate_bp` /
   `products.price_includes_tax`; `license_renewals.payment_order_id` and
   `license_upgrades.payment_order_id` (+ column relaxation from §4.11).
2. `domain` + `provider` + `sepay` (static QR + bank webhook + reconcile query) + `payos`
   (payment link + webhook + query/cancel).
3. Checkout: `POST /payments/orders`, `/pay/:checkout_id` dispatcher (Stripe vs VN), SPA page
   `/checkout/pay/:code`, headless payload, `GET /pay/:checkout_id/status`.
4. Settlement for `new` (licence creation) and `renewal` (extension).
5. Reconcile worker + unmatched queue + admin orders/transactions/providers + manual
   mark-paid and refund-record actions.
6. Admin plan-price editor (multi-provider/currency rows) + provider config UI; public pricing
   reads `plan_prices`.
7. Consumer API + scopes + events `order.created/paid`, `payment.succeeded/failed`.
8. VI locale + VND formatting.

**Exit criteria:** a VN customer can buy a subscription via PayOS link or SePay QR; licence is
created; webhook loss is recovered by the reconciler; a service can create an order through the
API and receive `order.paid`.

### Phase 1b — Billing, invoices, portal, coupons

1. Billing engine: renewal orders, reminders, grace, dunning, suspend/reinstate,
   cancel-at-period-end, trial conversion, upgrade proration.
2. Invoices: numbering, PDF, buyer info, VAT; e-invoice adapter + admin issue queue.
3. Portal: orders, payment page, invoices, billing profile, coupon validation.
4. Coupons: percent/fixed/trial, scopes (products/plans), stacking groups + limits, admin
   editor, per-coupon and global redemption history with CSV export.
5. Reports: revenue, reconciliation, VAT, CSV exports.
6. Remaining events (`order.expired/canceled`, `payment.refunded`, `invoice.*`,
   `license.renewal_due`).

**Exit criteria:** a monthly subscription renews itself through the billing engine and settles
via QR/link; VAT invoices are produced; portal self-service works; reports reconcile with
provider totals.

### Phase 2 — Full capability

- SePay gateway cards + NAPAS hosted checkout (dossier-dependent), card void before settlement.
- Per-order VA (BIDV/Sacombank/VCB/VietinBank/Shinhan) with bank-specific parameters.
- Refund execution: SePay API v2 (VietinBank, idempotency key, async status) + admin approval.
- PayOS payouts (chi hộ) for refunds/affiliate, approval workflow.
- E-invoice provider integrations beyond the default adapter.
- Migrate Stripe checkout/settlement onto the unified path; remove dual price read.
- Optional: multi-merchant per product UI, payout reports, tax reports.

### Backfill and compatibility

- Backfill script (`cmd/backfill-plan-prices` or admin action) iterates plans with
  `stripe_price_id`, fetches the Stripe Price, inserts `plan_prices` rows with the correct
  currency exponent (VND 0, USD/EUR 2), idempotent on `(plan_id, provider, currency,
  billing_interval)`.
- Until backfill completes, the public plans endpoint falls back to live Stripe price lookup
  (current behaviour).
- `plans.stripe_renewal_price_id` is deliberately not backfilled: the untouched Stripe path
  keeps reading it directly in phase 1. If phase 2 moves Stripe renewals onto the unified
  path, `plan_prices` gains a `price_role` (`initial`/`renewal`) column or a separate mapping
  instead of overloading one row.
- Rollback: disable provider configs (feature flag) and stop the workers; existing Stripe paths
  and data are untouched.

---

## 12. Risks

| Risk | Impact | Mitigation |
|---|---|---|
| PayOS has no sandbox | Cannot test end-to-end safely | Small real amounts, `mode=test` config row, manual settlement, reconciler; test checklist in runbook |
| PayOS `description` ≤ 9 chars | Payment matching failures | 8-char order code + PayOS `orderCode` numeric mapping; matching uses both |
| PayOS retry policy undocumented | Missed webhooks | Reconciler polls pending orders every 5 min; daily full reconcile |
| SePay webhook retries stop after ~33 min / 5 h | Missed payments | Reconciler + daily full reconcile; alert on cursor lag |
| SePay webhook registration is UI-only | Manual onboarding per merchant | Documented runbook; config row cannot be auto-provisioned |
| SePay 3 req/s | Reconcile throttling | Global limiter, backoff, page size 100, ascending sort |
| Bank memo matching is lossy | Unmatched transfers | Order code + VA as primary keys; amount+time only suggests; operator queue |
| E-invoice vendor dependency | VAT invoices not issued automatically | Adapter + `pending_issue` queue; manual issuance path |
| Touching `stripe.go` | Regression in revenue path | Phase 1 does not modify Stripe files; unified path only for new providers |
| Two price sources during migration | Price mismatch between Stripe and local | Backfill + local-first read + fallback; parity check in reports |
| Refund not automated in phase 1 | Manual ops burden | Record-only refunds with audit; phase 2 execution |
| VAT inclusive/exclusive ambiguity | Wrong totals/invoices | `products.price_includes_tax` explicit; invoice always shows breakdown |
| Coupon abuse | Revenue loss | Per-customer limits, max redemptions, one coupon per order, redemption at settlement |

---

## 13. Open questions

1. **E-invoice vendor**: SePay eInvoice (same vendor, separate onboarding) vs. MISA/VNPT/Viettel
   vs. manual issuance in phase 1. The adapter design makes this swappable; the default should
   be confirmed before phase 1b.
2. **PayOS merchant readiness**: has the PayOS channel been created and verified (KYC + bank
   link)? No sandbox means production credentials are required for any end-to-end test.
3. **SePay bank account**: which bank/account will receive transfers? VA-per-order support
   (phase 2) depends on the bank (BIDV DN, Sacombank, VCB, VietinBank DN, Shinhan).
4. **Invoice PDF library**: choose a pure-Go PDF library in the implementation plan (no headless
   browser); confirm invoice template requirements with accounting.
5. **Tax rate defaults**: confirm VAT rate(s) per product and whether listed prices include VAT.
6. **Consumer return URLs**: do services need per-key allowlists for `success_url`/`cancel_url`
   in phase 1, or is `https`-only validation sufficient?
7. **Stripe parity**: should `plan_prices` be backfilled for all existing plans in phase 1a, or
   only for plans that will be sold in VND?

---

## Appendix A — Provider field reference (verified)

### SePay bank webhook payload

```json
{
  "id": 92704,
  "gateway": "Vietcombank",
  "transactionDate": "2024-07-02 11:08:33",
  "accountNumber": "1017588888",
  "subAccount": "",
  "code": "SEVN63DC8E5C",
  "content": "SEVN63DC8E5C chuyen tien",
  "transferType": "in",
  "description": "NGUYEN VAN A chuyen tien",
  "transferAmount": 5000000,
  "accumulated": 105000000,
  "referenceCode": "FT24012345678"
}
```

### SePay gateway IPN payload (abridged)

```json
{
  "timestamp": 1700000000,
  "notification_type": "ORDER_PAID",
  "order": {
    "id": "…", "order_id": "SEPAY-…", "order_status": "CAPTURED",
    "order_currency": "VND", "order_amount": "50000.00",
    "order_invoice_number": "KG7F3K2Q", "custom_data": [],
    "order_description": "…"
  },
  "transaction": {
    "id": "…", "payment_method": "BANK_TRANSFER", "transaction_id": "…",
    "transaction_type": "PAYMENT", "transaction_status": "APPROVED",
    "transaction_amount": "50000.00", "card_number": "…", "card_brand": "…"
  },
  "customer": { "id": "…", "customer_id": "…" }
}
```

### PayOS create payment link response (abridged)

```json
{
  "code": "00",
  "desc": "success",
  "data": {
    "bin": "970422",
    "accountNumber": "113366668888",
    "accountName": "ACME JSC",
    "amount": 1200000,
    "description": "KG7F3K2Q",
    "orderCode": 123,
    "currency": "VND",
    "paymentLinkId": "124c33293c934a85be5b7f8761a27a07",
    "status": "PENDING",
    "checkoutUrl": "https://pay.payos.vn/web/124c33293c934a85be5b7f8761a27a07",
    "qrCode": "0002010102123857…6304BE36",
    "expiredAt": 1762900000
  },
  "signature": "…"
}
```

### PayOS webhook payload (abridged)

```json
{
  "code": "00",
  "desc": "success",
  "success": true,
  "data": {
    "orderCode": 123,
    "amount": 1200000,
    "description": "KG7F3K2Q",
    "accountNumber": "113366668888",
    "reference": "TF230204212323",
    "transactionDateTime": "2026-11-12 15:04:05",
    "currency": "VND",
    "paymentLinkId": "…",
    "code": "00",
    "desc": "Thành công",
    "counterAccountBankId": "",
    "counterAccountBankName": "",
    "counterAccountName": "",
    "counterAccountNumber": "",
    "virtualAccountName": "",
    "virtualAccountNumber": ""
  },
  "signature": "…"
}
```

## Appendix B — Sources

- SePay: `developer.sepay.vn/vi/sepay-webhooks/*`, `…/sepay-api/v2/*`, `…/cong-thanh-toan/*`
  (checkout form, IPN, order APIs, sandbox, payment flows), `…/tien-ich-khac/test-mode/*`,
  `…/tien-ich-khac/tao-qr-code`, `…/dia-chi-ip`, `sepay.vn/bang-gia.html`.
- PayOS: `payos.vn/docs/*` (checkout, webhook signature, return URL, test environment,
  payout API), `github.com/payOSHQ/payos-lib-golang` (types + crypto reference),
  `@payos/node` 2.0.5 types.
- Keygate: `internal/model/model.go`, `internal/payment/stripe.go`,
  `internal/store/store.go`, `internal/handler/plans_public.go`, `internal/handler/admin.go`,
  `internal/middleware/*`, `db/migrations/*`, `docs/api-contract.md`, `docs/openapi.yaml`.
