# SePay + PayOS Phase 1b — portal, remaining events and docs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking. Work only in the module's isolated worktree.
> Backend + docs only: `web/` is out of scope for this module.

**Goal:** Give the signed-in customer a portal for their Vietnamese payment orders, invoices and
buyer/tax profile; emit the three phase-1b events this module owns (`order.expired`,
`order.canceled`, `payment.refunded`) exactly once per real transition; and document every phase-1b
surface (consumer, admin, portal, events, settings, operations) in the API contract, OpenAPI and
operator runbook.

**Architecture:** The phase-1a rail (`internal/payment/checkout` creates an order + provider
attempt, `settlement.Service` grants the licence, `service.WebhookService` dispatches outbound
events) is reused, never duplicated. Two new server files carry the portal:
`internal/store/portal_payments.go` owns the customer-scoped queries (`lower(email)` or
`buyer->>'email'`, never an unscoped id lookup), and `internal/handler/payments_portal.go` exposes
the spec §6.4 routes under the existing `/api/v1/portal` session group. A new
`checkout.Service.ReissueAttempt` refreshes an existing order's provider instruction through the
same adapter call `CreateOrder` uses, so the portal never hand-rolls a QR or re-prices an order.
Invoices are consumed through a narrow local interface so the Lead can adapt module C's service
without this worktree depending on it. The three events get one small emitter
(`internal/service/webhook_events_1b.go`) wired at the transition points: the expiry sweep
(reconcile worker), the two cancel handlers, and the admin refund handler. Docs are updated last,
from the code that landed plus the shared contract, with sibling-module surfaces marked for
verify-after-merge rather than invented.

**Tech Stack:** Go 1.27, Gin 1.12, Bun ORM + PostgreSQL; tests use `TEST_DATABASE_URL`
(`internal/testsupport/testdb`); docs are Markdown + OpenAPI 3.1 YAML.

**Spec:** `docs/superpowers/specs/2026-11-12-sepay-payos-integration-design.md` §3.10, §3.13,
§6.4, §6.5, §8, §10, §11 phase 1b items 3 and 6.
**Contract:** `docs/superpowers/plans/2026-11-17-sepay-payos-phase-1b-CONTRACT.md`.
**Master:** `docs/superpowers/plans/2026-11-17-sepay-payos-phase-1b-master.md` (module 4).

## Global Constraints

- Go + Bun. Tests first (TDD); `go build ./... && go vet ./...` and the relevant package tests
  green before every commit.
- `cmd/server/main.go`, `web/`, `db/migrations/**`, `internal/model/**`, settlement code and
  sibling-module packages are **not edited**. The Lead wires routes/workers from the snippets in
  the final report.
- Edits to files owned by other modules are limited to event emission: `internal/store/payments.go`
  (new expiry method + delegation), `internal/payment/reconcile/worker.go` (emit after sweep),
  `internal/handler/payments_admin.go` (cancel/refund emission),
  `internal/handler/payments_api.go` (cancel emission). All are listed in the report.
- Portal routes take the existing session middleware (`SessionAuth`) and are registered relative
  to the `/api/v1/portal` group through **one** function:
  `RegisterPortalPaymentRoutes(g *gin.RouterGroup, h *PortalPaymentsHandler)`. A type alias
  `PortalPaymentHandler = PortalPaymentsHandler` keeps the contract's spelling compiling.
- Ownership is enforced in SQL: another customer's order or invoice answers **404**, never 403, so
  ids cannot be probed. The session email is the identity; a portal user has no API-key product
  binding.
- Money stays integer minor units + `currency_exponent`; no floats. Timestamps stay UTC.
- AGPL v3 §7(b): attribution fields/headers/footers are untouched. Portal config responses stay
  as they are; new handlers never strip attribution.
- Every new error code is pinned in `paymentVocabulary` (`emitted: true`) with exactly one
  status, and added to `docs/api-contract.md`. `TestPaymentErrorVocabulary` must stay green.
- Phase-1b vocabulary entries this module emits: `PAYMENT_TAX_CODE_REQUIRED` (422) and
  `INVOICE_NOT_FOUND` (404). The contract test's file scan must see the new handler:
  `payments_portal.go` is added to `paymentSourceFiles`.
- Docs must not invent response fields. Shapes come from the spec/contract and this module's
  code; sibling-module surfaces that cannot be executed in this worktree are marked
  **verify after merge** in the docs and in the final report.

## Review Focus

Conditions the spec implies but a happy-path test would miss; each gets a test in the task named.

1. **Ownership isolation** — customer B's order code and invoice id are 404 for customer A, for
   list, detail, pay, cancel and PDF. An invoice whose buyer email differs from the order email is
   still readable by the order's buyer (invoice is tied to the order), and never by anyone else
   (T2, T3).
2. **Pay only while pending and unexpired** — paid/expired/canceled/refunded and expiry-passed
   pending orders answer 409 `PAYMENT_ORDER_NOT_PAYABLE` and never call the provider; a payable
   order reuses the still-valid attempt instead of minting a second QR (T2, T4).
3. **Cancel is a real transition** — the first cancel emits one `order.canceled`; a second cancel
   (or an already paid order) is 409 and emits nothing (T5).
4. **Expiry sweep emits only rows it moved** — expired-pending rows emit once each; a second sweep
   emits nothing; paid/canceled/future rows are untouched (T5).
5. **Refund emits once per recorded refund** — a refused refund (`PAYMENT_REFUND_*`) emits nothing;
   a successful partial and final refund each emit one `payment.refunded` with
   `refund{amount, reason, method}` (T5).
6. **Billing profile validation** — `name` required (400), `tax_code` required when
   `request_invoice` is true (422 `PAYMENT_TAX_CODE_REQUIRED`); PUT upserts one row per user and is
   audited; GET for a user with no row returns `{"buyer":{}}`, not 404 (T3).
7. **PDF is a raw-body route** — `?format=pdf` streams `application/pdf` with a safe filename and
   `Cache-Control: private, no-store`; a foreign/missing invoice is 404 even for the PDF spelling
   (T3, T7).
8. **Docs match code** — every new route, code, event, setting and raw-body exception in
   `api-contract.md`/`openapi.yaml` exists; every sibling-module route is explicitly marked
   verify-after-merge (T7).

## File Structure

| Path | Action | Why |
|---|---|---|
| `internal/store/portal_payments.go` | create | customer-scoped order list/detail, order-by-id, billing profile get/upsert |
| `internal/store/portal_payments_test.go` | create | ownership + profile store tests (integration DB) |
| `internal/store/payments.go` | edit | add `ExpirePaymentOrdersWithRows`; old count method delegates |
| `internal/store/payments_test.go` | edit | pin rows returned by the new sweep method |
| `internal/handler/payments_portal.go` | create | §6.4 portal order/invoice/billing-profile handlers + registration |
| `internal/handler/payments_portal_test.go` | create | ownership, pay/cancel rules, invoices, profile, auth |
| `internal/handler/payments_contract_test.go` | edit | add file to scan list; flip emitted codes |
| `internal/payment/checkout/reissue.go` | create | `ReissueAttempt` reusing the adapter attempt path |
| `internal/payment/checkout/reissue_test.go` | create | same order, same total, reuse-valid-attempt, terminal refusal |
| `internal/service/webhook_events_1b.go` | create | `OrderEventEmitter` for the three events |
| `internal/service/webhook_events_1b_test.go` | create | payload shape + one dispatch per call |
| `internal/payment/reconcile/worker.go` | edit | emit `order.expired` for rows the sweep moved |
| `internal/payment/reconcile/worker_test.go` | edit | sweep emits once, no-op emits none |
| `internal/handler/payments_admin.go` | edit | emit `order.canceled` / `payment.refunded` |
| `internal/handler/payments_api.go` | edit | emit `order.canceled` |
| `internal/handler/payment_events_1b_test.go` | create | exactly-one-emission tests at the handler boundaries |
| `docs/api-contract.md` | edit | all phase-1b routes/codes/events/settings |
| `docs/openapi.yaml` | edit | schemas + paths for phase-1b surfaces, event enum |
| `docs/runbooks/payments-sepay-payos.md` | edit | phase-1b operations + acceptance checklist |

## Task 1 — Customer-scoped store queries

**Files:** create `internal/store/portal_payments.go`, `internal/store/portal_payments_test.go`.

**Interfaces:**
- Consumes: `store.Store` (`DB`, `NewID`, `scanPage`), `model.PaymentOrder`,
  `model.CustomerBillingProfile`, `store.Page`.
- Produces:
  - `func (s *Store) ListPortalPaymentOrders(ctx context.Context, email string, p Page) ([]*model.PaymentOrder, int, error)`
  - `func (s *Store) GetPortalPaymentOrder(ctx context.Context, code, email string) (*model.PaymentOrder, error)`
  - `func (s *Store) GetPaymentOrderByID(ctx context.Context, id string) (*model.PaymentOrder, error)`
  - `func (s *Store) GetBillingProfile(ctx context.Context, userID string) (*model.CustomerBillingProfile, error)`
  - `func (s *Store) UpsertBillingProfile(ctx context.Context, userID string, buyer []byte) (*model.CustomerBillingProfile, error)`
  - sentinel reuse: `ErrOrderNotFound` for a missing/foreign order; `sql.ErrNoRows` for a missing
    billing profile.

**Ownership predicate:** `lower(order.email) = lower(?) OR lower(order.buyer->>'email') = lower(?)`.
The 1a schema has no `external_customer_id` column on `payment_orders` (it lives on
`licenses`/`coupon_redemptions`), so email + buyer-email is the complete customer identity the
schema stores. Detail uses the same predicate, so a code that exists but is foreign is
indistinguishable from an unknown code.

- [ ] **Step 1: Write failing store tests.** Seed two customers' orders (one matching by
  `email`, one only by `buyer.email`), list as customer A and assert only A's two rows, newest
  first, total/limit/offset correct; `GetPortalPaymentOrder` of B's code as A returns
  `ErrOrderNotFound`; profile upsert creates then updates one row and GET returns the stored
  bytes.
- [ ] **Step 2: Run them and see them fail** (`go test ./internal/store/ -run TestPortal...`).
- [ ] **Step 3: Implement the queries** with bun; `scanPage` for the list; `RETURNING *` upsert
  for the profile with `updated_at = now()`.
- [ ] **Step 4: Run store tests** plus `go test ./internal/store/ -run 'TestExpire|TestPortal'`.

## Task 2 — Portal payment handlers + registration

**Files:** create `internal/handler/payments_portal.go`, `internal/handler/payments_portal_test.go`.

**Interfaces:**
- Consumes: Task 1 store methods; `checkout.Service.ReissueAttempt` (Task 4) through
  `PortalOrderPayer`:
  `ReissueAttempt(ctx context.Context, order *model.PaymentOrder) (*model.PaymentAttempt, error)`;
  `service.OrderEventEmitter` (Task 5) for cancel.
- Produces:
  ```go
  type PortalPaymentsHandler struct {
      Store    *store.Store
      Payer    PortalOrderPayer
      Invoices PortalInvoiceService
      Events   *service.OrderEventEmitter
  }
  type PortalPaymentHandler = PortalPaymentsHandler // contract spelling
  func NewPortalPaymentsHandler(st *store.Store, payer PortalOrderPayer, invoices PortalInvoiceService) *PortalPaymentsHandler
  func RegisterPortalPaymentRoutes(g *gin.RouterGroup, h *PortalPaymentsHandler)
  ```
  Routes (relative to the portal group): `GET /payments/orders`,
  `GET /payments/orders/:code`, `POST /payments/orders/:code/pay`,
  `POST /payments/orders/:code/cancel`.
- Views: order list rows are compact; detail adds the latest `payment` block (provider,
  provider_ref, qr_payload, qr_image_url, checkout_url, va_number, expires_at, bank_account) using
  the existing `attemptRawOf`/`attemptQRImage` helpers. No `id`, `numeric_code`, `buyer`,
  `metadata`, `created_by_api_key_id`, `last_settlement_error`.
- Errors: no session → 401 `UNAUTHORIZED`; foreign/unknown order → 404
  `PAYMENT_ORDER_NOT_FOUND`; pay/cancel on a terminal or expiry-passed order → 409
  `PAYMENT_ORDER_NOT_PAYABLE`; provider unavailable → 503 `PAYMENT_PROVIDER_DISABLED`; provider
  failure → 502 `PAYMENT_PROVIDER_ERROR`; malformed body → 400 `BAD_REQUEST`.

- [ ] **Step 1: Write failing handler tests** (recording fake payer): auth required; list
  isolation; detail isolation; pay on pending calls the payer once and returns the instruction;
  pay on paid/canceled/expiry-passed is 409 with zero payer calls; cancel flips the row and emits;
  second cancel is 409. Mount `RegisterPortalPaymentRoutes` on a gin group in one test to prove
  the route table.
- [ ] **Step 2: Implement helpers and handlers.** Session identity comes from `user_id`/`email` in
  the gin context; a missing/blank identity is 401 before any query.
- [ ] **Step 3: Run `go test ./internal/handler/ -run 'TestPortalPayment'`.**

## Task 3 — Portal invoices and billing profile

**Files:** create/extend `internal/handler/payments_portal.go` +
`internal/handler/payments_portal_test.go`.

**Interfaces:**
- Consumes: module C's invoice service through a local interface (adapter written by the Lead):
  ```go
  type PortalInvoiceFilter struct { BuyerEmail string; Limit, Offset int }
  type PortalInvoiceService interface {
      Get(ctx context.Context, id string) (*model.Invoice, error)
      PDF(ctx context.Context, id string) ([]byte, error)
      List(ctx context.Context, f PortalInvoiceFilter) ([]*model.Invoice, int, error)
  }
  type PortalInvoiceFuncs struct{ GetFn func(...); PDFFn func(...); ListFn func(...) }
  var ErrPortalInvoiceNotFound = errors.New("invoice not found")
  ```
  `GetInvoice` also accepts the linked order's email as ownership (`Store.GetPaymentOrderByID`),
  so an invoice whose buyer email differs from the order still belongs to the buyer.
- Produces routes: `GET /invoices`, `GET /invoices/:id` (`?format=pdf`), `GET /billing-profile`,
  `PUT /billing-profile`.
- Buyer profile shape: `name` (required), `company_name`, `tax_code`, `address`, `email`, `phone`,
  `request_invoice`. `tax_code` required when `request_invoice` is true → 422
  `PAYMENT_TAX_CODE_REQUIRED`; missing name or over-long field → 400 `BAD_REQUEST`. PUT upserts,
  writes an audit line (`entity=customer_billing_profile`, action `updated`, actor type
  `portal`), and answers the saved object. GET with no row answers `{"buyer":{}}`.
- Invoice list rows: `id`, `number`, `order_id`, `status`, `subtotal_minor`, `discount_minor`,
  `tax_minor`, `total_minor`, `currency`, `currency_exponent`, `issued_at`, `created_at`.
  Detail adds `buyer` and `items`. PDF: `application/pdf`,
  `Content-Disposition: attachment; filename="<number>.pdf"`, `Cache-Control: private, no-store`,
  raw bytes (documented as a raw-body route).

- [ ] **Step 1: Write failing tests** with a fake invoice service: list isolation; detail owner /
  foreign 404 / not-found 404; PDF headers+bytes; foreign PDF 404; profile GET default, PUT+GET
  round-trip, audit row, validation errors, upsert (PUT twice keeps one row).
- [ ] **Step 2: Implement** list/detail/PDF and profile handlers.
- [ ] **Step 3: Run `go test ./internal/handler/ -run 'TestPortalInvoice|TestPortalBilling'`.**

## Task 4 — Checkout re-issue for an existing order

**Files:** create `internal/payment/checkout/reissue.go`, `internal/payment/checkout/reissue_test.go`.

**Interfaces:**
- Produces:
  `func (s *Service) ReissueAttempt(ctx context.Context, order *model.PaymentOrder) (*model.PaymentAttempt, error)`.
  It re-reads the order, refuses terminal/expired orders (`store.ErrOrderNotPayable`), returns the
  latest attempt untouched when it is still payable (no second QR), otherwise resolves the order's
  provider through `s.adapter` and calls `prov.CreateAttempt` exactly as `CreateOrder` does,
  persisting the attempt (and a failed attempt on provider error, as `CreateOrder` does).
  It never creates an order and never changes the order's amount/price.
- Errors: `store.ErrOrderNotFound`, `store.ErrOrderNotPayable`, `checkout.ErrProviderUnavailable`,
  `checkout.ErrProviderError`.

- [ ] **Step 1: Write failing tests** with the existing `fixture` + fake provider: reissue keeps
  the same order id/code/total and adds one attempt; a still-payable attempt is returned without a
  provider call; a paid/expired order returns `ErrOrderNotPayable` and makes no provider call; a
  provider failure records a failed attempt and returns `ErrProviderError`.
- [ ] **Step 2: Implement.**
- [ ] **Step 3: Run `go test ./internal/payment/checkout/ -run TestReissue`.**

## Task 5 — Emit `order.expired`, `order.canceled`, `payment.refunded`

**Files:** create `internal/service/webhook_events_1b.go`,
`internal/service/webhook_events_1b_test.go`, `internal/handler/payment_events_1b_test.go`;
edit `internal/store/payments.go`, `internal/store/payments_test.go`,
`internal/payment/reconcile/worker.go`, `internal/payment/reconcile/worker_test.go`,
`internal/handler/payments_admin.go`, `internal/handler/payments_api.go`.

**Interfaces:**
- Produces:
  ```go
  type WebhookDispatcher interface {
      Dispatch(ctx context.Context, productID, event string, data map[string]any)
  }
  type OrderEventEmitter struct { Webhooks WebhookDispatcher; Log *slog.Logger }
  func NewOrderEventEmitter(w WebhookDispatcher, log *slog.Logger) *OrderEventEmitter
  func (e *OrderEventEmitter) OrderExpired(ctx context.Context, order *model.PaymentOrder)
  func (e *OrderEventEmitter) OrderCanceled(ctx context.Context, order *model.PaymentOrder)
  func (e *OrderEventEmitter) PaymentRefunded(ctx context.Context, order *model.PaymentOrder, refund *model.PaymentRefund)
  ```
  Payloads follow the existing convention: `{"order": order}` for the two order events;
  `{"order": order, "refund": {"amount": refund.AmountMinor, "reason": refund.Reason,
  "method": refund.Method}}` for the refund (`amount` is integer minor units, like
  `payment.succeeded.amount`). Nil receiver/dispatcher is a no-op.
- Store: `func (s *Store) ExpirePaymentOrdersWithRows(ctx, now) ([]*model.PaymentOrder, error)`
  (`UPDATE … RETURNING *`); `ExpirePaymentOrders` keeps its signature and delegates, so existing
  callers/tests are unchanged.
- Reconcile `Worker` gains `Events` (interface with `OrderExpired`), called for each returned row.
- `PaymentsAdminHandler` and `PaymentsAPIHandler` gain `Events *service.OrderEventEmitter`;
  cancel emits after a successful state change, refund emits after `RecordPaymentRefund` succeeds.
  No emission on any error branch.

- [ ] **Step 1: Write failing tests.** Store: two expired pending + one paid + one future → only
  the two move and are returned; second call returns none. Worker: one pending expired order,
  subscribed webhook → one delivery; second `Tick` → still one; no pending expired → none.
  Handlers: admin cancel once → one `order.canceled`; second → 409 and no new delivery; consumer
  cancel same; admin refund success → one `payment.refunded` with the payload; refused refund →
  none. Emitter unit test for the refund payload and nil-safety.
- [ ] **Step 2: Implement** emitter, store method, worker loop and handler calls.
- [ ] **Step 3: Run `go test ./internal/service/ ./internal/store/ ./internal/payment/reconcile/ ./internal/handler/`**
  (full packages, not only the new tests).

## Task 6 — Contract-test pinning

**Files:** edit `internal/handler/payments_contract_test.go`.

- [ ] Add `"payments_portal.go"` to `paymentSourceFiles`.
- [ ] Flip `PAYMENT_TAX_CODE_REQUIRED` and `INVOICE_NOT_FOUND` to `emitted: true` (only after the
  handlers write them); leave the other sibling-module codes `emitted: false`.
- [ ] Run `go test ./internal/handler/ -run TestPaymentErrorVocabulary` and the full handler
  package.

## Task 7 — Docs

**Files:** `docs/api-contract.md`, `docs/openapi.yaml`,
`docs/runbooks/payments-sepay-payos.md`.

- [ ] **api-contract.md**
  - New `### Portal payments, invoices and billing profile` under Payments: route table, session
    auth, ownership-404 rule, order list/detail shapes, pay/cancel rules and statuses, invoice
    list/detail/PDF, profile shape + validation, audit.
  - Add the PDF route to the raw-body table.
  - Extend the consumer section with `POST /payments/quote` and
    `GET /payments/invoices/:id` (module B/C; verify after merge).
  - Extend admin with coupons CRUD, redemptions (+CSV), invoice list/detail/issue/void/PDF,
    reports (revenue/reconciliation/VAT, `?format=csv`) — statuses/codes from the spec, shapes
    marked verify-after-merge where module code is not in this worktree.
  - Replace the phase-1a "four events only" drift bullet with the phase-1b event list and payload
    table (`order.expired`, `order.canceled`, `payment.refunded`, `invoice.issued`,
    `invoice.voided`, `license.renewal_due`, plus the 1a events).
  - Document new settings keys: `coupon_max_stack`, `payment_quote_rate_limit`,
    `invoice_seller_name`, `invoice_seller_tax_code`, `invoice_seller_address`,
    `invoice_seller_email`, `invoice_note`, `einvoice_provider`, and the existing
    `payment_order_ttl_minutes`.
  - Mark the error vocabulary table rows for phase-1b codes as emitted by which module.
- [ ] **openapi.yaml**
  - `WebhookEventName` enum: add the four phase-1a events and the six phase-1b events.
  - Schemas: `Invoice`, `InvoiceItem`, `PortalOrder` (or reuse `PaymentOrder`), `BuyerProfile`,
    `Coupon`, `CouponScope`, `CouponRedemption`, report row wrappers.
  - Paths: consumer quote + consumer invoice (JSON/PDF), admin coupons/redemptions/invoices/
    reports, portal payments orders (list/detail/pay/cancel), portal invoices (list/detail/PDF),
    portal billing profile (GET/PUT), with 401/403/404/409/422 error descriptions matching the
    contract. Reuse `SessionRequired`, `AdminUnauthorized`, `AdminForbidden`, `RateLimited`.
- [ ] **runbook**
  - Title/scope becomes phase 1a+1b; remove "no invoices/coupons/portal" from known gaps.
  - Add sections: billing worker + dunning ladder (schedule, reminder tags, recovery, cancel at
    period end); coupon operations (create/scope/limits, assigned email, redemption CSV); invoice
    issue/void + PDF storage; e-invoice manual queue (`pending_issue`); reports/CSV; settings
    keys; troubleshooting entries (invoice stuck pending_issue, PDF unavailable, event not
    delivered, coupon refuses); phase-1b acceptance checklist mapped to tests/manual steps.
  - Note the deferred gap: a provider-reported expiry in settlement changes the order status but
    is not an emitter in this module (Lead wiring/verify after merge).
- [ ] **Step: verify docs mechanically** — every route path mentioned in api-contract exists in
  openapi or is explicitly marked verify-after-merge; every new code appears in the code scan;
  YAML parses (`python -c 'import yaml…'` or ruby); no secrets.

## Task 8 — Final verification and commits

- [ ] `go build ./... && go vet ./...`
- [ ] `TEST_DATABASE_URL=… go test ./internal/handler/... ./internal/service/... ./internal/store/... ./internal/payment/reconcile/... ./internal/payment/checkout/...`
- [ ] Confirm `TestPaymentErrorVocabulary`, `TestErrorCodeMapsToExactlyOneStatus` (pkg/response)
  and the phase-1a payment tests are green.
- [ ] Commit per task with conventional messages:
  `feat(portal): ...`, `feat(events): ...`, `docs(payments): ...`.
- [ ] Final report: commits, files, exact commands/pass counts, main.go wiring snippet + invoice
  adapter, docs changed, deferred/verify-after-merge items.

## Acceptance checklist

- [ ] Portal order list/detail/pay/cancel + invoice list/detail/PDF + billing profile GET/PUT are
  registered by `RegisterPortalPaymentRoutes` and session-auth only.
- [ ] Foreign order/invoice ⇒ 404 for every spelling (JSON, PDF, pay, cancel).
- [ ] Pay works only for pending, unexpired orders; re-issues through checkout's provider path;
  never re-prices and never creates a second order.
- [ ] `order.expired` fires once per expired row in the sweep; `order.canceled` once per real
  cancel; `payment.refunded` once per recorded refund; none on no-op/failure.
- [ ] `payments_portal.go` is in `paymentSourceFiles`; new codes pinned and
  `TestPaymentErrorVocabulary` green.
- [ ] `api-contract.md`, `openapi.yaml` and the runbook cover every phase-1b route, event, setting
  and error code owned by this module, with sibling-module items marked verify-after-merge.
