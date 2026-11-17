# SePay + PayOS Phase 1b — implementation plan (master)

> **For agentic workers:** This is the master plan for phase 1b. Each module has its own
> module plan written by its implementer before code:
> `2026-11-17-sepay-payos-phase-1b-billing-engine.md`, `-coupons.md`,
> `-invoices-reports.md`, `-portal-events-docs.md`. Shared decisions live in
> `2026-11-17-sepay-payos-phase-1b-CONTRACT.md`. Execute modules in the order below.

**Goal:** Hoàn thiện phase 1b của tích hợp SePay/PayOS: billing engine (tự gia hạn, nhắc hạn,
grace/dunning, suspend/reinstate, cancel-at-period-end, trial conversion, upgrade proration),
hoá đơn VAT + PDF + e-invoice seam thủ công, portal khách hàng (orders/invoices/billing profile),
coupons đầy đủ (admin CRUD, scope, stacking, giới hạn, redemption + CSV), reports
(revenue/reconciliation/VAT/CSV), các event còn lại — và deploy staging.

**Architecture:** Phase 1a (`internal/payment`: checkout → provider attempt → verified webhook →
`settlement.Service` grant → events) stays the rail. Phase 1b adds: `internal/billing` (order
lifecycle worker), `internal/payment/coupons` (quote + redemption, wired into checkout and the
settlement PaidHook), `internal/payment/invoice` (invoice record, PDF, manual e-invoice seam,
called from the PaidHook), `internal/payment/reports`-style store queries + admin handlers, and
portal endpoints that read orders/invoices. All money stays integer minor units with currency
exponent; VN placement is `Asia/Ho_Chi_Minh`; new paths never call Stripe.

**Tech Stack:** Go 1.27 (Gin, Bun, PostgreSQL), React 19 + Vite + Tailwind 4, Bun. Tests:
`go test` with `TEST_DATABASE_URL` integration DB (`internal/testsupport/testdb`).

**Spec:** `docs/superpowers/specs/2026-11-12-sepay-payos-integration-design.md` (§3.6, §3.8,
§3.9, §3.10, §4.8, §4.9, §6.2–§6.5, §8, §10, §11 phase 1b). Contract:
`docs/superpowers/plans/2026-11-17-sepay-payos-phase-1b-CONTRACT.md`.

## Global Constraints

- AGPL v3 §7(b): "Powered by Keygate" / `X-Powered-By` attribution remains on every surface.
- Money = integer minor units + exponent; VND exponent 0; no floats.
- One error code ⇔ one HTTP status (`payments_contract_test.go` + `docs/api-contract.md`).
- Admin writes are audited; every new public/admin/portal route has an auth + ownership test.
- Coupon redemption is written only at settlement; abandoned orders consume nothing.
- Invoice numbers `KG-<YYYY>-<000001>`, per-year concurrency-safe counter.
- PDF generation must not add a headless browser.
- Every phase-1b webhook event is emitted exactly once per transition (at-least-once delivery is
  fine; duplicate *emission* is not).
- Existing 1a behaviour keeps working: `go test ./...` green, Stripe paths untouched.

## Foundation (Lead — committed `336d29b`)

- Migrations `20261116_01_invoices`, `_02_billing`, `_03_portal_billing_profile`.
- Models: `Invoice`, `InvoiceItem`, `InvoiceCounter`, `CustomerBillingProfile`,
  `Plan.ProrationMode`; phase-1b webhook event constants.
- `storage.Storage.Put`; `settlement.PaidHook`; phase-1b error vocabulary pinned
  (documented-ahead entries flip to `emitted: true` in the module that writes them).

## Module order and acceptance

### 1. Billing engine — `internal/billing` (module plan: billing-engine)
Renewal orders + attempts inside the lead window with dedupe; reminder ladder
`renewal_reminder_7/3/1`, `renewal_due`, `renewal_grace_1/3/7`, `renewal_suspended`;
past_due → grace → suspended → expired; payment recovers; cancel-at-period-end; trial
conversion; upgrade proration (`immediate`/`next_cycle`, credit =
round(unused/period*price), total ≥ 0); `license.renewal_due` event; hourly `Run(ctx)` with
`RunOnce` for tests; en+vi reminder emails. **Acceptance:** all listed transitions covered by
fake-clock integration tests; two concurrent `RunOnce` calls create at most one pending renewal
per `(licence, period_end)`.

### 2. Coupons — `internal/payment/coupons` (module plan: coupons)
Validation + stacking per §3.9; `Quote` (VAT-consistent), checkout integration with
`coupon_codes`/`ignore_invalid_coupons` + `payment_order_discounts`; trial coupon zero-total
orders settle immediately; redemption at settlement via PaidHook (idempotent, atomic counter);
admin CRUD + scope replacement + redemptions list/CSV + audit; portal validate; admin UI
(Coupons list/editor + redemption history). **Acceptance:** every stacking/limit/email-bind
rule has a test; order totals match quote; abandoned orders consume no inventory.

### 3. Invoices + reports — `internal/payment/invoice` (module plan: invoices-reports)
Invoice created idempotently from PaidHook; numbering concurrency test; seller settings, buyer
snapshot, VAT line items; pure-Go PDF with embedded Vietnamese-capable OFL font; storage upload
optional, stream on demand otherwise; manual e-invoice seam (`pending_issue` → `issued`, void);
`invoice.issued`/`invoice.voided` events; admin list/detail/issue/void/PDF + consumer
`GET /payments/invoices/{id}`; reports revenue/reconciliation/VAT with CSV. **Acceptance:**
duplicate settlement cannot duplicate an invoice; numbering has no gaps/dupes under concurrency;
VAT math tested with `price_includes_tax` both ways and exponent ≠ 0; report totals reconcile
with seeded orders/transactions.

### 4. Portal + remaining events + docs (module plan: portal-events-docs)
Portal orders list/detail/pay/cancel, invoices JSON/PDF, billing profile GET/PUT backed by
`customer_billing_profiles`; ownership isolation (another customer's order/invoice ⇒ 404);
portal UI pages + nav/i18n. Events `order.expired`, `order.canceled`, `payment.refunded` emitted
at their single transition points. Docs: `api-contract.md`, `openapi.yaml`, runbook and the
phase-1b acceptance section. **Acceptance:** ownership tests pass; each event emitted exactly
once; docs list every new route/event/error code.

## Cross-cutting integration (Lead)

- `cmd/server/main.go`: construct billing/coupons/invoice/portal handlers + workers, call each
  module's registration function, add billing worker next to the expiry loop, build the composite
  `settlement.PaidHook` (invoice create + coupon redeem) with error logging.
- Flip phase-1b `paymentVocabulary` entries to `emitted: true` as handlers land; keep
  `TestPaymentErrorVocabulary` green.
- Frontend integration review: routes, nav, api client, attribution footer, vi/en strings.
- Final verification: `go test ./...` with `TEST_DATABASE_URL`, `go vet ./...`, frontend
  `bun run build`, phase-1b acceptance pass.
- Deploy staging (`stg-license.dtgsoft.vn`) with pre-deploy DB dump + image rollback, then verify
  health/version, migrations 56, public pages and payment flows; write handover note.
- Production deploy is a separate explicit step after staging verification (user approval).

## Risks

- Checkout service is touched by both billing (renewal/upgrade kinds) and coupons
  (`coupon_codes`): same file, sequential modules, tests pin both uses.
- Settlement is Lead-owned: modules propose an exact diff if recovery/upgrade semantics need it.
- PDF font/language: embedded OFL TTF with Vietnamese coverage must be committed with licence.
- Existing Stripe subscription rows must not be picked up by the VN billing scan: scope the scan
  to licences/subscriptions on sepay/payos payment provider (or non-Stripe) and test it.
