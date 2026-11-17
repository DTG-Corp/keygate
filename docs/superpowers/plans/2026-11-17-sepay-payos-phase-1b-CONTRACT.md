# Phase 1b — shared contract (SePay + PayOS)

Status: decided by Lead on 2026-11-17. All phase-1b plans MUST follow this document.
Branch/worktree: `feat/sepay-payos-phase-1b` at `/tmp/kg-1b` (based on `chore/dtg-branding`
@ 37e04a1, i.e. phase 1a + DTG branding). Spec: `docs/superpowers/specs/2026-11-12-sepay-payos-integration-design.md`
§11 phase 1b. 1a plan style reference: `docs/superpowers/plans/2026-11-12-sepay-payos-phase-1a-core-rail.md`.

## Non-negotiable global rules

- Go + Bun. Tests first (TDD), `go build ./...`, `go vet ./...`, `go test ./...` green before any commit.
  Frontend: `cd web && bun run typecheck && bun run build` (or `bunx tsc -b --noEmit` + `bunx vite build`).
- AGPL v3 §7(b): never remove/hide "Powered by Keygate" (`attribution_text`/`attribution_url`,
  `X-Powered-By`, footer sync). New pages keep the footer/attribution.
- New payment paths never call Stripe. `plans.stripe_price_id` stays read-only legacy.
- Money: integer minor units + `currency_exponent`; VND exponent 0. Never float.
- Time: store UTC; VN display `Asia/Ho_Chi_Minh` (existing helpers).
- Every admin write route is audited via `store.Audit` like existing admin handlers.
- No secrets in logs/API responses.

## Migrations (Lead writes these; plans must reference, not edit)

Naming continues the 1a series:
- `db/migrations/20261116_01_invoices.up.sql` / `.down.sql`
  - `invoice_counters(year INT PRIMARY KEY, last_seq BIGINT NOT NULL DEFAULT 0)`
  - `invoices` exactly as spec §4.8 (`id TEXT PK`, `number TEXT UNIQUE`, `order_id TEXT NOT NULL UNIQUE REFERENCES payment_orders(id) ON DELETE RESTRICT`, `license_id TEXT NULL REFERENCES licenses(id) ON DELETE SET NULL`, `buyer JSONB`, `items JSONB`, `subtotal_minor BIGINT`, `discount_minor BIGINT`, `tax_minor BIGINT`, `total_minor BIGINT`, `currency CHAR(3)`, `currency_exponent SMALLINT`, `status TEXT CHECK IN ('draft','issued','pending_issue','void')`, `pdf_path`, `e_invoice_provider`, `e_invoice_ref`, `e_invoice_status`, `issued_at`, timestamps). Index `invoices_created_idx (created_at DESC)` and `invoices_status_idx (status)`.
- `db/migrations/20261116_02_billing.up.sql` / `.down.sql`
  - `ALTER TABLE plans ADD COLUMN proration_mode TEXT NOT NULL DEFAULT 'immediate' CHECK (proration_mode IN ('immediate','next_cycle'))`
  - `CREATE INDEX subscriptions_billing_scan ON subscriptions (current_period_end) WHERE status IN ('active','trialing','past_due')`
- `db/migrations/20261116_03_portal_billing_profile.up.sql` / `.down.sql`
  - `customer_billing_profiles(user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE, buyer JSONB NOT NULL DEFAULT '{}'::jsonb, created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`

## Shared code owned by Lead (implementers consume, do not redefine)

### Models (`internal/model`)
- New file `internal/model/invoice.go`: `Invoice` and `InvoiceCounter` bun models matching the
  migration; JSON tags snake_case; `Items []InvoiceItem`; `Buyer provider.Buyer` as raw
  `map[string]any`/`json.RawMessage` is fine — use `json.RawMessage` for `buyer`/`items` and add
  typed helpers where needed.
- `Plan.ProrationMode string \`json:"proration_mode"\`` added to `internal/model/model.go`.
- New file `internal/model/portal.go`: `CustomerBillingProfile` (`user_id` PK, `buyer JSONB`).
- `internal/model/model.go` webhook events: add `EventOrderExpired = "order.expired"`,
  `EventOrderCanceled = "order.canceled"`, `EventPaymentRefunded = "payment.refunded"`,
  `EventInvoiceIssued = "invoice.issued"`, `EventInvoiceVoided = "invoice.voided"`,
  `EventLicenseRenewalDue = "license.renewal_due"`; extend `WebhookEvents`.
- `model.AllScopes` already has `payments:write`/`payments:read` from 1a.

### Storage (`internal/storage`)
- Add to `Storage` interface: `Put(ctx context.Context, key string, body []byte, contentType string) error`
  (server-side upload; idempotent overwrite). Implement in `s3.go` (`PutObject`), `Disabled`
  (`ErrStorageDisabled`), and every fake in tests (`storage.Storage` fakes implement it).
- `S3.PresignedGet` already exists for downloads; invoices may stream PDF bytes directly instead.

### Settlement hook (`internal/payment/settlement`)
- Lead adds to `Service` an optional hook interface:
  `type PaidHook interface { AfterPaid(ctx context.Context, order *model.PaymentOrder, lic *model.License, providerName string) }`
  called exactly once after a successful `settleNew` / `settleRenewal` (inside `dispatchPaid`).
  Hook must never block ack: implementers' hooks must be safe/idempotent and log errors.
- Lead wires a composite hook in `cmd/server/main.go` that calls:
  `invoice.Service.CreateForOrder` (module C) and `coupons.Service.RedeemForOrder` (module B).
  The invoice service emits `invoice.issued`/`invoice.voided` itself; the billing engine emits
  `license.renewal_due`; module D emits `order.expired`/`order.canceled`/`payment.refunded`.

### Routes wiring (`cmd/server/main.go`) — Lead edits
Each module exposes ONE registration function it owns, e.g.:
- `internal/handler/coupons_admin.go`: `func RegisterCouponsAdminRoutes(g *gin.RouterGroup, h *CouponsAdminHandler)`
- `internal/handler/coupons_portal.go`: `func RegisterCouponsPortalRoutes(g *gin.RouterGroup, h *CouponsPortalHandler)`
- `internal/handler/invoices_admin.go`: `func RegisterInvoicesAdminRoutes(g *gin.RouterGroup, h *InvoicesAdminHandler)`
- `internal/handler/reports_admin.go`: `func RegisterReportsAdminRoutes(g *gin.RouterGroup, h *ReportsAdminHandler)`
- `internal/handler/payments_api_invoices.go`: `func RegisterConsumerInvoiceRoutes(g *gin.RouterGroup, h *ConsumerInvoiceHandler)`
- `internal/handler/portal_payments.go`: `func RegisterPortalPaymentRoutes(g *gin.RouterGroup, h *PortalPaymentHandler)`
Lead will add the handler construction + one call per function at the right place (admin group,
consumer group, portal group). Implementers MUST NOT edit `cmd/server/main.go`; instead they write
a short "wiring snippet" in their final report (exact constructor args + call line).

### Error codes
1b codes from spec §6.5 are added to `internal/handler/payments_contract_test.go`
`paymentVocabulary` by whoever emits them (module owner), and to `docs/api-contract.md` by the
docs task. One code ⇔ one status. Reserved-but-unemitted codes stay `emitted: false`.

### Settings (existing store)
New keys, read via `store.GetSetting` with defaults:
- `coupon_max_stack` (default `2`)
- `invoice_seller_name`, `invoice_seller_tax_code`, `invoice_seller_address`, `invoice_seller_email`
- `invoice_note` (default `''`)
- `einvoice_provider` (default `''` = manual issue only)

## Module map (one implementer each, executed sequentially by Lead)

### Module A — Billing engine (`internal/billing`, plans/2026-11-17-...-phase-1b-billing.md)
Own new: `internal/billing/**`, `internal/store/billing.go`, `internal/service/email_renewal.go`,
changes to `internal/payment/checkout/service.go` needed to create renewal/upgrade orders
(add fields to `CreateOrderInput`, keep existing callers compiling), settlement upgrade path if
required (coordinate with Lead via report), its tests.
Contract surface:
```go
package billing
type Deps struct { Store *store.Store; Checkout OrderCreator; Email ReminderSender; Webhook EventEmitter; Logger *slog.Logger; BaseURL string }
type Service struct{...}
func New(d Deps) *Service
func (s *Service) Run(ctx context.Context)            // hourly loop, honours ctx
func (s *Service) RunOnce(ctx context.Context) error  // exported for tests/admin
```
Reminder ladder tags per spec §3.6, using existing `notifications` store methods
(unique per licence+tag) and new email templates in `internal/service/email_renewal.go`.
No new HTTP routes except optional admin "run billing once" (skip unless trivial).

### Module B — Coupons (`internal/payment/coupons`, plan ...-coupons.md)
Own new: `internal/payment/coupons/**`, `internal/store/coupons.go`,
`internal/handler/coupons_admin.go`, `internal/handler/coupons_portal.go`,
checkout integration (`CreateOrderInput.CouponCodes`, `IgnoreInvalidCoupons`; quote+persist in
`createOrder`; response exposes discounts/totals), `internal/handler/payments_api.go` order-create
request/response fields, settlement redemption via hook (module wiring by Lead), web admin pages
Coupons/Redemptions + API client functions, its tests.
Contract surface:
```go
package coupons
type Service struct{...}
func New(st *store.Store) *Service
type QuoteInput struct { Codes []string; IgnoreInvalid bool; Plan *model.Plan; Product *model.Product; Quantity int; SubtotalMinor int64; Currency string; CurrencyExponent int16; Kind string; Email string; ExternalCustomerID string }
type Discount struct { CouponID, Code, Type string; Value int64; AmountMinor int64; Priority int; TrialDays int }
type Quote struct { SubtotalMinor, DiscountMinor, TaxMinor, TotalMinor int64; Discounts []Discount; TrialDays int }
func (s *Service) Quote(ctx context.Context, in QuoteInput) (*Quote, error) // wraps coupon errors with codes
func (s *Service) RedeemForOrder(ctx context.Context, order *model.PaymentOrder) error // idempotent
func (s *Service) PortalValidate(ctx context.Context, in QuoteInput) (*Quote, error)
func ErrorCode(err error) string // PAYMENT_COUPON_* or ""
```
Trial coupons create `total_minor = 0` orders that settle immediately (existing 1a settlement
must handle zero-total orders; add if missing).

### Module C — Invoices + reports (`internal/payment/invoice`, `internal/store/invoices.go`,
`internal/store/reports.go`, `internal/handler/invoices_admin.go`, `internal/handler/reports_admin.go`,
`internal/handler/payments_api_invoices.go`, web admin invoice list/detail + reports pages, PDF).
Contract surface:
```go
package invoice
type Service struct{...}
func New(st *store.Store, stor storage.Storage, logger *slog.Logger) *Service
func (s *Service) CreateForOrder(ctx context.Context, order *model.PaymentOrder) (*model.Invoice, error) // idempotent by order_id
func (s *Service) Get(ctx context.Context, id string) (*model.Invoice, error)
func (s *Service) PDF(ctx context.Context, id string) ([]byte, *model.Invoice, error)
func (s *Service) Issue(ctx context.Context, id, actorID string) (*model.Invoice, error)
func (s *Service) Void(ctx context.Context, id, actorID string) (*model.Invoice, error)
func (s *Service) List(ctx context.Context, f ListFilter) ([]*model.Invoice, int, error)
```
PDF: pure-Go, no headless browser; embed an OFL-licensed TTF with Vietnamese coverage
(DejaVu Sans or Noto Sans). Invoice numbering `KG-<YYYY>-<000001>` with `invoice_counters`.
Manual e-invoice seam only (status `pending_issue` → `issued` via admin action), events
`invoice.issued`/`invoice.voided`. Reports: revenue (by provider/plan/product/period),
reconciliation (daily, drift vs transactions), VAT; `?format=csv`.
Consumer `GET /api/v1/payments/invoices/{id}` (JSON + `?format=pdf`, `payments:read`).

### Module D — Portal + remaining events + docs
Own new: `internal/store/portal_payments.go`, `internal/handler/portal_payments.go`,
`internal/service/webhook_events_1b.go` (server-side emitters for `order.expired`/`order.canceled`/
`payment.refunded` only — reuse the existing webhook dispatch service; invoice events are emitted
by module C, `license.renewal_due` by module A), portal web pages
(orders, invoices, billing profile), docs: `docs/api-contract.md`, `docs/openapi.yaml`,
`docs/runbooks/payments-sepay-payos.md`, and final phase-1b acceptance test.
Portal routes per spec §6.4 with session auth; `customer_billing_profiles` table from Lead.
Dependencies: consumes invoice/coupon packages from B and C.

## Execution protocol (subagent implementers)

1. Read this contract, the spec sections named in your plan, and your plan file.
2. Execute the plan task by task with TDD. Commit per task with conventional commit messages.
3. Run `go build ./... && go test ./...` (and frontend checks when UI touched) before each commit.
4. Do not edit files outside your module scope except: shared test fixtures may be added in new
   files; report any required edit to a shared file (main.go, settlement/service.go, model
   foundation) instead of making it — Lead performs those edits.
5. Finish with a report: files created/modified, wiring snippet, test/verification evidence,
   deferred items, and any interface changes future modules must know.
