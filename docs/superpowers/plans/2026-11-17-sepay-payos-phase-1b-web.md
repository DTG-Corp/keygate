# SePay + PayOS Phase 1b — Web (admin, portal, checkout) implementation plan

> **For agentic workers:** execute task by task, in order. Each task ends with typecheck/lint and a
> conventional commit. Backend handlers for phase 1b are being written in parallel and are not in
> this worktree; every uncertain shape is normalised in `web/src/lib/api.ts` and listed in the final
> alignment report.

**Goal:** Ship the phase-1b web surface: admin coupon CRUD (all fields, scopes, quote preview),
redemption history + CSV, invoices (list/detail/issue/void/PDF), revenue/reconciliation/VAT
reports + CSV, portal payment orders with instructions/pay/cancel and invoice links, portal
invoices + PDF, portal billing profile, coupon display on checkout (read-only per spec §3.13), and
complete en/vi strings — without touching Go, migrations or docs.

**Architecture:** React 19 + Vite + Tailwind 4 + Radix/shadcn, TanStack Query, react-router-dom 7.
Pages are thin, data comes only through typed helpers in `web/src/lib/api.ts`; money is formatted
only through `lib/money.ts` (integer minor units + exponent); dates through `lib/utils.ts`. New
pages reuse `DataTable`, `Card`, `Select`, `Dialog`/`Sheet` and the existing navy DTG palette.
Blob routes (invoice PDF, report/redemption CSV) go through `api.ts` fetch helpers, never raw
`fetch` in a page. The AGPL "Powered by DSoft" footer/sidebar attribution is untouched.

**Spec:** `docs/superpowers/specs/2026-11-12-sepay-payos-integration-design.md` §3.9, §3.13, §4.8,
§4.9, §6.2–§6.5, §8. **Contract:** `2026-11-17-sepay-payos-phase-1b-CONTRACT.md`. Module plans:
`2026-11-17-sepay-payos-phase-1b-coupons.md` (web tasks 10–13), `-invoices-reports.md` (web UI
deferred to this module), `-portal-events-docs.md` (portal view shapes).

## Constraints

- Frontend only: `web/src/**` TypeScript/TSX/CSS + this plan file. No Go, migration or doc edit.
- No new `package.json` dependency (recharts already present; tables chosen over charts).
- No secrets, no raw `fetch` outside `api.ts`; all routes centralised there.
- Money never divides by 100; use `currency_exponent` from the payload.
- English + Vietnamese strings for every new key; zh falls back (locale is `Partial<Translations>`).
- Attribution untouched on admin, portal and public checkout surfaces.
- `bun run typecheck`, `bun run build` green; `bun run lint` clean on touched files.

## File Structure

```
web/src/lib/api.ts                         types + helpers for coupons/redemptions/invoices/reports/portal/quote/PDF/CSV
web/src/lib/download.ts                    saveBlob (anchor download) helper
web/src/components/coupon-codes.tsx        editable chips input + read-only chips + totals
web/src/components/payment-instructions.tsx QR/checkout/VA/bank block + copy buttons (pay + portal)
web/src/components/invoice-document.tsx    shared invoice detail blocks (admin + portal)
web/src/pages/admin/payments-coupons.tsx   coupon list + filters
web/src/pages/admin/payments-coupon-editor.tsx  create/edit all fields + scopes + preview + usage
web/src/pages/admin/payments-redemptions.tsx    redemption history + CSV
web/src/pages/admin/payments-invoices.tsx       invoice list + filters
web/src/pages/admin/payments-invoice-detail.tsx invoice detail + issue/void/PDF
web/src/pages/admin/payments-reports.tsx        revenue / reconciliation / VAT tabs + CSV
web/src/pages/portal/payments.tsx               customer order list
web/src/pages/portal/payment-detail.tsx         order + instructions + pay/cancel + invoice link
web/src/pages/portal/invoices.tsx               customer invoice list
web/src/pages/portal/invoice-detail.tsx         customer invoice detail + PDF
web/src/pages/portal/billing-profile.tsx        saved buyer/tax form
web/src/pages/checkout-pay.tsx                  read-only coupon chips + totals from status
web/src/main.tsx                                routes
web/src/components/layout.tsx                   admin + portal nav entries
web/src/i18n/locales/en.ts, vi.ts               strings
```

## Tasks

1. **API client + i18n scaffolding.** Add tolerant interfaces and every function below in
   `api.ts`: coupons CRUD/list/preview/redemptions(+CSV), invoices list/detail/issue/void/PDF,
   reports JSON/CSV, portal orders/detail/pay/cancel, portal invoices/detail/PDF, billing profile,
   coupon quote and payments quote. Normalise envelopes (`{coupon}`, `{invoice}`, `{buyer}`,
   direct objects, `listOK` keys) in one place so a backend shape difference cannot blank a page.
   Add en + vi keys for nav/pages/actions/errors.
2. **Admin coupons list** `/admin/payments/coupons`: search/active/type/stack-group filters,
   table (code, type/value, scope summary, stack group, redeemed/max, active, validity, usage),
   create/edit/redemptions links, archive/delete confirm with server error surface.
3. **Admin coupon editor** `/admin/payments/coupons/new` and `/:id`: all fields (code + generate,
   description, percent/fixed/trial, value or trial days, currency, assigned email, validity,
   max redemptions, per-customer limit, stack group, stackable, priority, product/plan scope
   multi-select, active), validation mirroring §3.9, live quote preview via the preview endpoint
   when present and a clearly marked local estimate otherwise, usage stats for saved coupons.
4. **Admin redemption history** `/admin/payments/redemptions`: coupon/email/date filters, table
   (coupon, order, customer, amount/currency, created), CSV download with the same filters.
5. **Admin invoices** `/admin/payments/invoices` + `/:id`: filters (number/order/status/date),
   list (number, order, buyer name/tax code, total, status, issued), detail with seller/buyer/tax
   blocks, items, subtotal/discount/VAT/total, issue/void confirm + audit reason, PDF blob
   download. No backfill action (no route in the phase-1b contract).
6. **Admin reports** `/admin/payments/reports`: Revenue / Reconciliation / VAT tabs, date range,
   granularity (day/month), revenue grouping, provider/product/plan filters where supported,
   summary cards from `totals`, tables, per-report CSV download.
7. **Portal orders** `/portal/payments` + `/:code`: list with status filter; detail with amount,
   status, expiry, payment instructions (QR payload/image, checkout URL, VA number, bank account,
   memo), copy buttons, Pay/Renew for pending orders, Cancel with confirm, invoice link; graceful
   rendering when individual instruction fields are absent.
8. **Portal invoices + billing profile** `/portal/invoices`, `/:id`, `/portal/billing-profile`:
   list/detail/PDF download; saved buyer form with `request_invoice` requiring `tax_code`, save
   success/error states, prefill from profile and account email.
9. **Checkout coupon display + navigation.** Add read-only coupon chips and discount/VAT/total
   breakdown to `/checkout/pay/:code` when the status payload carries them (spec §3.13; never
   re-prices). Add reusable `CouponCodes` editable component for future quote surfaces. Register
   admin/portal routes and nav entries; keep attribution untouched.
10. **Verify + report.** `bun run fmt`, `bun run lint`, `bun run typecheck`, `bun run build`;
    commit per task; final report with alignment items (all shapes centralised in `api.ts`).

## Out of scope / deferred

- Consumer order creation from the SPA: `POST /api/v1/payments/orders` is API-key scoped
  (`payments:write`), and §3.13 forbids re-pricing on the pay page. Editable coupon apply/remove
  needs a public/session quote+order-start route; the component is built and the pay page shows
  read-only chips meanwhile.
- Invoice backfill, refund netting in reports, recharts visualisations (tables chosen).
- Settings UI for `coupon_max_stack`/invoice seller block (existing settings page owner).
