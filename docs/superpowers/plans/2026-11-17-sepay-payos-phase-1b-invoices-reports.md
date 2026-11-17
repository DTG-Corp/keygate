# SePay + PayOS Phase 1b — Module C (invoices, PDF, reports) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.
> Module C of `2026-11-17-sepay-payos-phase-1b-master.md`. Contract:
> `2026-11-17-sepay-payos-phase-1b-CONTRACT.md` (binding). **Backend only** — all phase-1b web
> UI (invoice list/detail, report pages) is a later module.

**Goal:** Every paid order gets an immutable invoice record with a concurrency-safe
`KG-<YYYY>-<000001>` number, a Vietnamese-capable PDF, a manual e-invoice issue/void queue and
the `invoice.issued` / `invoice.voided` events; admins get invoice list/detail/PDF routes and
revenue / reconciliation / VAT reports (JSON + CSV); merchant API keys get
`GET /api/v1/payments/invoices/{id}` scoped to their product.

**Architecture:** `internal/store/invoices.go` owns the SQL (number allocation inside the insert
transaction, idempotency by `invoices.order_id`, state transitions as guarded UPDATEs) and
`internal/store/reports.go` owns the report aggregates (raw SQL, `Asia/Ho_Chi_Minh` buckets).
`internal/payment/invoice` is the service seam: it snapshots buyer/items/money from the paid
order, decides `issued` vs `pending_issue` from the `einvoice_provider` setting, emits events
through a small `EventEmitter` interface implemented by `*service.WebhookService`, and renders
the PDF in pure Go with an embedded OFL font. Handlers stay thin: three `payments_*.go` files
register the three route families and map service sentinels onto the pinned error vocabulary.
`settlement.PaidHook` wiring is done by the Lead in `cmd/server/main.go`; this module only
publishes the constructor and registration functions in its report.

**Tech Stack:** Go 1.27, Gin 1.12, Bun + PostgreSQL, `github.com/go-pdf/fpdf v0.9.0` (MIT,
pure Go, no cgo/browser) + Noto Sans Regular (OFL-1.1) embedded via `//go:embed`. Tests:
`go test` with `TEST_DATABASE_URL` (`internal/testsupport/testdb`).

**Spec:** `docs/superpowers/specs/2026-11-12-sepay-payos-integration-design.md` §3.8 (invoices,
PDF, e-invoice seam), §4.8 (`invoices`, `invoice_counters`), §6.2 (consumer invoice route), §6.3
(admin invoices + reports + CSV), §6.5 (`INVOICE_*`, `EINVOICE_PROVIDER_ERROR`), §8
(`invoice.issued`/`invoice.voided` payloads), §3.10/§3.11 (events/settings), §10 (testing),
§11 phase-1b items 2 and 5.

## Global Constraints

- **Files this module may not edit:** `cmd/server/main.go`, `internal/payment/settlement/service.go`,
  `db/migrations/**`, Lead-owned model foundation (`internal/model/invoice.go`, the phase-1b
  additions in `internal/model/model.go`), `web/**`. Wiring is delivered as a snippet in the
  final report; the Lead applies it.
- **Handler files must be named `payments_*.go`** and added to `paymentSourceFiles` in
  `internal/handler/payments_contract_test.go`; the same test refuses a `payments_*.go` file the
  list does not name.
- **One error code ⇔ one HTTP status.** `INVOICE_NOT_FOUND` 404, `INVOICE_ALREADY_ISSUED` 409,
  `EINVOICE_PROVIDER_ERROR` 502 — the three `emitted: false` entries flip to `true` in the commit
  that adds the handlers, and `TestPaymentErrorVocabulary` + `TestErrorCodeMapsToExactlyOneStatus`
  must pass.
- **Money** = integer minor units + `currency_exponent`; VND 0, USD 2. Never floats. Invoice
  amounts are **copied from the paid order exactly** — the invoice never recomputes VAT.
- **Time** stored UTC; report buckets and the invoice-number year use `Asia/Ho_Chi_Minh`
  (`time.FixedZone("ICT", 7*3600)`), matching the 1a provider adapters.
- **Attribution (AGPL v3 §7(b)):** the PDF footer uses `branding.Tagline` + `branding.URL`
  (overridable by the `attribution_url` setting only when the installer set it explicitly); the
  literal "Powered by Keygate" is never written in this module and is never removable.
- **No new infra:** no headless browser, no external PDF service, no vendor e-invoice API. The
  only new dependency is the PDF library plus the vendored OFL font.
- Admin writes are audited through `store.Audit` (`ActorType: "admin"`, actor from the route).
- `go build ./...`, `go vet ./...` and
  `go test ./internal/payment/... ./internal/handler/... ./internal/store/...` green before each
  commit (`TEST_DATABASE_URL` exported).

## Review Focus

Conditions the spec implies but a happy path does not cover. Each is pinned by a named test.

1. **Two settlements of two orders at the same instant must not mint the same number** — the
   counter is `INSERT … ON CONFLICT (year) DO UPDATE SET last_seq = last_seq + 1 RETURNING`, not a
   read-then-write (T1/T2: `TestInvoiceNumberingConcurrent`).
2. **Duplicate settlement of one order must return the same invoice, not a 500** — the second
   `CreateInvoice` hits `invoices_order_id_key`; it must return the existing row and must not burn
   a number (T1/T2: `TestCreateForOrderIsIdempotent`).
3. **A pending-issue invoice must not be "issued" twice or voided back to life** — guarded UPDATEs
   with explicit sentinels, one status per code (T3: `TestIssueAndVoidStateMachine`).
4. **A void must not erase the document** — `status='void'` keeps number/amounts/PDF path and
   emits `invoice.voided` once (T3: `TestVoidKeepsDocumentAndEmitsEvent`).
5. **VAT with `price_includes_tax` both ways and exponent ≠ 0 (USD)** — invoice money is copied
   from the order, so the test asserts the copy is exact and the PDF formats 2-decimal amounts
   (T2/T4: `TestCreateForOrderCopiesMoneyVerbatim`, `TestPDFUSDAndZeroTotal`).
6. **A merchant key bound to product A must not read product B's invoice** — 404, not 403, and no
   buyer data in the body (T6: `TestConsumerInvoiceScoping`).
7. **Report drift must be visible** — a matched transaction whose amount differs from the paid
   order produces non-zero `drift_minor`; a day with no rows on one side still appears (T7:
   `TestReconciliationReportDrift`).
8. **CSV is an attachment, formula-safe and header-stable** — `Content-Disposition: attachment`,
   `csvSafeRow` on every user-controlled cell, and one header per column (T7:
   `TestReportsCSVShape`).

## File Structure

```
internal/store/invoices.go                     T1  (schema-backed invoice CRUD, numbering, transitions)
internal/store/invoices_test.go                T1
internal/store/reports.go                      T7  (revenue / reconciliation / VAT aggregates)
internal/store/reports_test.go                 T7
internal/payment/invoice/service.go            T2/T3 (Deps, New, CreateForOrder, Get, List, Issue, Void)
internal/payment/invoice/service_test.go       T2/T3
internal/payment/invoice/pdf.go                T4  (PDF data + fpdf renderer + storage upload)
internal/payment/invoice/pdf_test.go           T4
internal/payment/invoice/assets/NotoSans-Regular.ttf   T4 (OFL-1.1, vendored)
internal/payment/invoice/assets/OFL.txt        T4  (licence text)
internal/payment/invoice/assets/README.md      T4  (origin + sha256)
internal/payment/invoice/main_test.go          T2  (testdb.TestMain)
internal/handler/payments_invoices_admin.go    T5
internal/handler/payments_invoices_admin_test.go  T5
internal/handler/payments_api_invoices.go      T6
internal/handler/payments_api_invoices_test.go T6
internal/handler/payments_reports_admin.go     T7
internal/handler/payments_reports_admin_test.go T7
internal/handler/payments_contract_test.go     T5  (paymentSourceFiles + 3 vocabulary flips)
go.mod / go.sum                                T4  (github.com/go-pdf/fpdf)
```

---

## T1 — Store: invoice rows, per-year numbering, guarded transitions

**Files:** create `internal/store/invoices.go`, `internal/store/invoices_test.go`.

**Interfaces (package `store`):**

```go
var (
    ErrInvoiceNotFound      = errors.New("invoice not found")
    ErrInvoiceAlreadyIssued = errors.New("invoice is already issued")
    ErrInvoiceNotVoidable   = errors.New("invoice cannot be voided")
)

type InvoiceFilter struct {
    Status, LicenseID, OrderID, Number, ProductID string
    From, To                                      *time.Time
    Page                                          Page
}

type CreateInvoiceInput struct {
    OrderID, LicenseID string
    Buyer              []byte
    Items              []model.InvoiceItem
    SubtotalMinor, DiscountMinor, TaxMinor, TotalMinor int64
    Currency           string
    CurrencyExponent   int16
    Status             string    // issued | pending_issue
    EInvoiceProvider   string
    IssuedAt           *time.Time
    NumberYear         int       // Asia/Ho_Chi_Minh year
}

// CreateInvoice allocates the next number for NumberYear and inserts the row in one
// transaction. A duplicate order_id returns the existing row (created=false), not an error.
func (s *Store) CreateInvoice(ctx context.Context, in CreateInvoiceInput) (*model.Invoice, bool, error)
func (s *Store) GetInvoice(ctx context.Context, id string) (*model.Invoice, error)
func (s *Store) GetInvoiceWithOrder(ctx context.Context, id string) (*model.Invoice, *model.PaymentOrder, error)
func (s *Store) ListInvoices(ctx context.Context, f InvoiceFilter) ([]*model.Invoice, int, error)
func (s *Store) MarkInvoiceIssued(ctx context.Context, id string, at time.Time, ref EInvoiceRef) (*model.Invoice, error)
func (s *Store) MarkInvoiceVoid(ctx context.Context, id string) (*model.Invoice, error)
func (s *Store) SetInvoicePDFPath(ctx context.Context, id, path string) error
func (s *Store) SetInvoiceEInvoice(ctx context.Context, id, provider, ref, status string) error
```

`EInvoiceRef` is a tiny store-local struct (`Provider, Ref, Status string`) so the store does not
import the invoice package.

- [ ] **Step 1 — failing test `TestCreateInvoiceNumbersPerYear`.** Open the test DB
      (`testdb.Store`), insert the order the invoice references (`s.CreatePaymentOrder`), call
      `CreateInvoice` twice with `NumberYear: 2026`, assert `KG-2026-000001` / `KG-2026-000002`,
      `created == true`, `status` copied, and that a 2027 call starts at `000001` again. Clean up
      `invoices`, `invoice_counters` (both years) and the order in `t.Cleanup`.
      Command: `go test ./internal/store -run TestCreateInvoiceNumbersPerYear -count=1` → FAIL
      (`CreateInvoice` undefined).
- [ ] **Step 2 — implement the allocator.**

```go
func nextInvoiceNumber(ctx context.Context, tx bun.Tx, year int) (string, error) {
    var seq int64
    err := tx.NewRaw(`INSERT INTO invoice_counters (year, last_seq) VALUES (?, 1)
        ON CONFLICT (year) DO UPDATE SET last_seq = invoice_counters.last_seq + 1
        RETURNING last_seq`, year).Scan(ctx, &seq)
    if err != nil { return "", err }
    return fmt.Sprintf("KG-%04d-%06d", year, seq), nil
}
```

  Insert (id `NewID()`, buyer/items marshalled to JSON when nil) inside `s.RunInTx`. On
  `isUniqueViolationOn(err, "invoices_order_id_key")` re-read by `order_id` and return it with
  `created=false` (the transaction rolls back, so the burned number is not committed — assert the
  next number is still sequential in the test).
- [ ] **Step 3 — failing test `TestInvoiceNumberingConcurrent`.** Two goroutines (WaitGroup +
      start channel) create invoices for two different orders in the same year; assert two
      distinct numbers, contiguous, and exactly two rows. Run with `-race`.
      Command: `go test ./internal/store -run TestInvoiceNumberingConcurrent -count=1 -race`.
- [ ] **Step 4 — failing test `TestInvoiceTransitionsAndFilters`.** `MarkInvoiceIssued` on
      `pending_issue` returns `issued` with `IssuedAt`; a second call returns
      `ErrInvoiceAlreadyIssued`; `MarkInvoiceVoid` from `issued` returns `void`; from `void`
      returns `ErrInvoiceNotVoidable`; `GetInvoice("missing")` returns `ErrInvoiceNotFound`;
      `ListInvoices` filters by status/license/order/date and pages. Implement the two guarded
      UPDATEs (`WHERE status = 'pending_issue'`, `WHERE status IN ('issued','pending_issue')`,
      `RowsAffected() == 0` → re-read to decide the sentinel) and the list query.
- [ ] **Step 5 — run and commit.** `go test ./internal/store -run 'TestInvoice' -count=1 && go build ./...`
      → commit `feat(invoices): invoice store with per-year numbering and transitions`.

## T2 — Invoice service: idempotent creation from a paid order

**Files:** create `internal/payment/invoice/service.go`, `internal/payment/invoice/main_test.go`,
`internal/payment/invoice/service_test.go`.

**Interfaces:**

```go
package invoice

// EventEmitter is the outbound webhook seam; *service.WebhookService satisfies it.
type EventEmitter interface {
    Dispatch(ctx context.Context, productID, event string, data map[string]any)
}

type Deps struct {
    Store   *store.Store
    Storage storage.Storage
    Logger  *slog.Logger
    Now     func() time.Time
    Events  EventEmitter
    EInvoice EInvoiceProvider // nil = manual seam only
}

func New(st *store.Store, stor storage.Storage, logger *slog.Logger) *Service
func NewWithDeps(d Deps) *Service
func (s *Service) CreateForOrder(ctx context.Context, order *model.PaymentOrder) (*model.Invoice, error)
func (s *Service) Get(ctx context.Context, id string) (*model.Invoice, error)
type ListFilter struct { Status, LicenseID, OrderID, Number string; From, To *time.Time; Page store.Page }
func (s *Service) List(ctx context.Context, f ListFilter) ([]*model.Invoice, int, error)
```

Creation rules: buyer snapshot from `order.Buyer` (tolerant of `tax_code`/`tax_id`,
`company_name`/`company`; `email` falls back to `order.Email`); one item line
(`plan name` from `FindPlanByID`, `quantity = max(order.Quantity, 1)`,
`unit = (subtotal + q/2) / q`, `amount = subtotal`, `tax_rate_bp = order.TaxRateBP`); money fields
copied verbatim; status `pending_issue` when the `einvoice_provider` setting is non-empty, else
`issued` with `issued_at = Now()`; number year from `Now().In(vnZone)`. Emit `invoice.issued`
after a successful `issued` creation exactly once (the duplicate path returns the existing row and
emits nothing).

- [ ] **Step 1 — `main_test.go`:** `func TestMain(m *testing.M) { os.Exit(testdb.Main(m, "../../../db/migrations")) }`.
- [ ] **Step 2 — failing test `TestCreateForOrderSnapshotsOrder`.** Seed product/plan/order with
      `tax_minor > 0`, buyer JSON `{"name":"Nguyễn Văn Ánh","company_name":"ACME JSC","tax_code":"0101234567","address":"1 Lê Lợi","email":"buyer@example.com"}`, quantity 2,
      subtotal 2_000_000, discount 200_000, tax 132_000, total 1_800_000, VND. Assert: status
      `issued`, `issued_at != nil`, number `KG-<current VN year>-…`, buyer round-trips, one item
      with unit 1_000_000 and amount 2_000_000, all money equal to the order, event captured once
      with `invoice.issued`.
      Command: `go test ./internal/payment/invoice -run TestCreateForOrderSnapshotsOrder -count=1`
      → FAIL.
- [ ] **Step 3 — implement** `CreateForOrder`, `Get`, `List`, the buyer/items snapshot, the
      settings read (`einvoice_provider`, default `""`) and event dispatch. Test doubles: fake
      `EventEmitter` recording `(event, productID, data)`, `storage.Disabled{}`.
- [ ] **Step 4 — failing tests** `TestCreateForOrderIsIdempotent` (call twice → same id/number,
      one row, one event), `TestCreateForOrderPendingIssueWhenProviderConfigured`
      (`store.SetSetting(ctx,"einvoice_provider","sepay-einvoice")` → `pending_issue`, no issued
      event), `TestCreateForOrderCopiesMoneyVerbatim` (USD exponent 2, zero-total invoice: all
      amounts copied, `tax_minor == 0`), `TestCreateForOrderMissingPlanStillInvoices` (plan
      deleted protection: description falls back to the plan id without failing).
- [ ] **Step 5 — commit** `feat(invoices): invoice service snapshots paid orders idempotently`.

## T3 — Issue / Void state machine, audit, events

**Interfaces:**

```go
// ErrEInvoiceProvider marks a configured e-invoice adapter's failure.
var ErrEInvoiceProvider = errors.New("e-invoice provider error")

type EInvoiceRef struct{ Provider, Ref, Status string }
type EInvoiceProvider interface {
    Issue(ctx context.Context, inv *model.Invoice) (EInvoiceRef, error)
    Void(ctx context.Context, ref EInvoiceRef) error
    Status(ctx context.Context, ref EInvoiceRef) (string, error)
}

func (s *Service) Issue(ctx context.Context, id, actorID string) (*model.Invoice, error)
func (s *Service) Void(ctx context.Context, id, actorID string) (*model.Invoice, error)
// VoidWithReason is Void plus the operator's note, recorded on the audit entry.
func (s *Service) VoidWithReason(ctx context.Context, id, actorID, reason string) (*model.Invoice, error)
```

`Issue`: pending_issue → issued (sets `issued_at`, optional provider call when `s.EInvoice != nil`,
persists ref/status, audit `invoice_issued`, emits `invoice.issued`). Issued/void →
`store.ErrInvoiceAlreadyIssued`; missing → `store.ErrInvoiceNotFound`. `Void`: issued or
pending_issue → void (audit `invoice_voided` with reason, emits `invoice.voided`); void →
`store.ErrInvoiceNotVoidable`. Adapter failure wraps `ErrEInvoiceProvider`.

- [ ] **Step 1 — failing test `TestIssueAndVoidStateMachine`.** pending → `Issue` → issued, audit
      row exists (`invoice_issued`), event emitted; second `Issue` → `ErrInvoiceAlreadyIssued`;
      `Issue("missing")` → `ErrInvoiceNotFound`; `Void` on issued → void + `invoice.voided`
      event + audit; `Void` again → `ErrInvoiceNotVoidable`.
- [ ] **Step 2 — implement** with `store.MarkInvoiceIssued` / `MarkInvoiceVoid`, mapping the
      store sentinels 1:1 (no new codes).
- [ ] **Step 3 — failing test `TestIssueUsesEInvoiceProvider`.** Fake provider returns a ref →
      `e_invoice_ref/status` persisted and → issued. Fake returns error → `ErrEInvoiceProvider`
      and the row stays `pending_issue`.
- [ ] **Step 4 — failing test `TestVoidKeepsDocumentAndEmitsEvent`.** Void keeps number, totals,
      `pdf_path`; the webhook payload has `invoice{id,number}` only.
- [ ] **Step 5 — commit** `feat(invoices): issue and void with audit and events`.

## T4 — PDF: pure-Go renderer, embedded OFL font, storage upload

**Files:** `go.mod`/`go.sum`, `internal/payment/invoice/pdf.go`, `pdf_test.go`, `assets/…`.

Feasibility was verified before writing this plan: `go-pdf/fpdf v0.9.0` renders
`HÓA ĐƠN GIÁ TRỊ GIA TĂNG — Nguyễn Văn Ánh — 1.190.000 ₫` correctly with Noto Sans Regular, no
cgo, no browser (rendered sample inspected).

**Interfaces:**

```go
// assets: NotoSans-Regular.ttf (sha256 b85c38ec…), OFL.txt
func (s *Service) PDF(ctx context.Context, id string) ([]byte, *model.Invoice, error)

type pdfDocument struct {
    Invoice *model.Invoice
    Order   *model.PaymentOrder
    Seller  sellerBlock // invoice_seller_name/_tax_code/_address/_email
    Note    string      // invoice_note
    Vi      bool        // language == "vi"
    AttributionText, AttributionURL string
}
func renderInvoicePDF(d pdfDocument) ([]byte, error)
```

Layout: seller block, title (HÓA ĐƠN / INVOICE), number + issue date (VN time), buyer block,
items table (description, qty, unit, amount), totals (subtotal, discount, VAT, total with
exponent-aware formatting), payment reference/order code, note, "Powered by Keygate" footer
linked to the attribution URL. Labels switch to Vietnamese when the `language` setting is `vi`.

`PDF` logic: load invoice (404 sentinel) + order; if `pdf_path != ""` and storage enabled, `Get`
the stored bytes (regenerate on storage error); otherwise render, then `Put` to
`invoices/<year>/<number>.pdf` and persist `pdf_path` (an upload failure is logged, never fails the
request). Year from the number, not `now`.

- [ ] **Step 1 — vendor + pin:** `go get github.com/go-pdf/fpdf@v0.9.0`; copy the font and
      `OFL.txt` into `assets/`; write `assets/README.md` with origin URL, sha256 and licence.
- [ ] **Step 2 — failing test `TestPDFHasHeaderAndVietnameseText`.** Create an invoice with a
      Vietnamese buyer and item, `PDF` → `bytes.HasPrefix(out, []byte("%PDF-"))`, len > 2 KB,
      no error; the page count / font objects prove an embedded font (`/FontFile2` in the raw
      bytes). Also `TestPDFMissingInvoice` → `ErrInvoiceNotFound` (as `store.ErrInvoiceNotFound`).
- [ ] **Step 3 — implement** `renderInvoicePDF` with `fpdf.New`, `AddUTF8FontFromBytes`, a
      `fish`-free helper `formatMinor(amount, exp, vi)` (vi: `.` thousands / `,` decimals; en:
      `,` / `.`) and a footer using `branding.Tagline` + `branding.URL` (or the `attribution_url`
      setting when set).
- [ ] **Step 4 — failing test `TestPDFUSDAndZeroTotal`.** USD invoice exponent 2 →
      `1,234.50` formatting; zero-total invoice renders and totals read `0`. Vietnamese language
      setting → the bytes contain the ToUnicode-mapped label? Assert instead on the renderer's
      label table (`invoiceLabels(true)["total"] == "Tổng cộng"`) plus a successful render.
- [ ] **Step 5 — failing test `TestPDFStoresToStorageAndReusesIt`.** fake storage records the
      `Put` key `invoices/2026/KG-2026-000001.pdf`, `pdf_path` persisted, second `PDF` call
      returns the stored bytes (fake counts `Get`). `storage.Disabled{}` → no path, bytes still
      returned.
- [ ] **Step 6 — commit** `feat(invoices): pure-Go PDF with embedded Vietnamese font and storage`.

## T5 — Admin invoice routes + error vocabulary

**Files:** create `internal/handler/payments_invoices_admin.go`,
`internal/handler/payments_invoices_admin_test.go`; edit `internal/handler/payments_contract_test.go`.

**Interfaces:**

```go
type InvoicesAdminHandler struct{ Invoices *invoice.Service }
func RegisterInvoicesAdminRoutes(g *gin.RouterGroup, h *InvoicesAdminHandler) // g = /api/v1/admin
// GET  /payments/invoices            filters: status, license_id, order_id, number, from, to; pagination
// GET  /payments/invoices/:id
// POST /payments/invoices/:id/issue  {"actor_id"?} — actor is adminID(c)
// POST /payments/invoices/:id/void   {"reason"}       — audited, INVOICE_ALREADY_ISSUED when void
// GET  /payments/invoices/:id/pdf    inline application/pdf
```

Mapping: `store.ErrInvoiceNotFound` → 404 `INVOICE_NOT_FOUND`; `store.ErrInvoiceAlreadyIssued` /
`store.ErrInvoiceNotVoidable` → 409 `INVOICE_ALREADY_ISSUED`; `invoice.ErrEInvoiceProvider` → 502
`EINVOICE_PROVIDER_ERROR`.

- [ ] **Step 1 — failing test `TestAdminInvoiceRoutes`.** Build a gin engine, register the
      routes on `/api/v1/admin`, exercise: list filter by status/order, detail, issue (200,
      status issued), issue again (409 `INVOICE_ALREADY_ISSUED`), void with reason (200), void
      again (409), missing (404 `INVOICE_NOT_FOUND`), PDF (`Content-Type: application/pdf`,
      `%PDF-`).
      Command: `go test ./internal/handler -run TestAdminInvoiceRoutes -count=1` → FAIL.
- [ ] **Step 2 — implement** the handler with the shared envelope (`response.OK`,
      `listOK`, `response.Err`), `adminID(c)` for the actor, and a JSON body for the optional
      void reason.
- [ ] **Step 3 — failing test `TestAdminInvoiceRoutesRequireAdmin`.** Register under a
      `RequireScope(model.ScopeAdmin, model.ScopeLicensesWrite)` group (the shape of main's
      `adminMW`); a `licenses:write`-only API key gets 403 on list and issue. (The handler never
      runs.)
- [ ] **Step 4 — vocabulary flip.** Add `"payments_invoices_admin.go"` to `paymentSourceFiles`
      and flip `INVOICE_NOT_FOUND`, `INVOICE_ALREADY_ISSUED`, `EINVOICE_PROVIDER_ERROR` to
      `emitted: true`. Run `go test ./internal/handler -run 'TestPaymentErrorVocabulary' -count=1`
      and `go test ./pkg/response -run TestErrorCodeMapsToExactlyOneStatus -count=1`.
- [ ] **Step 5 — commit** `feat(invoices): admin invoice routes with issue/void/pdf`.

## T6 — Consumer invoice route (JSON + PDF, product-scoped)

**Files:** create `internal/handler/payments_api_invoices.go`,
`internal/handler/payments_api_invoices_test.go`; edit `payments_contract_test.go`
(`paymentSourceFiles`).

```go
type ConsumerInvoiceHandler struct{ Invoices *invoice.Service }
func RegisterConsumerInvoiceRoutes(g *gin.RouterGroup, h *ConsumerInvoiceHandler) // g = payRead
// GET /invoices/:id          JSON
// GET /invoices/:id?format=pdf
```

- [ ] **Step 1 — failing test `TestConsumerInvoiceJSONAndPDF`.** Call the handler with a key
      (`payments:read`) whose `ProductID` is the order's product: JSON 200 with `id`, `number`,
      `items`, totals; `format=pdf` → `application/pdf` + `%PDF-`.
- [ ] **Step 2 — failing test `TestConsumerInvoiceScoping`.** Bound key for another product →
      404 `INVOICE_NOT_FOUND` and no buyer email in the body; unbound key → 200; unknown id →
      404. Bad `format` → 400 `BAD_REQUEST`.
- [ ] **Step 3 — implement** using `Invoices.Get` + `store.GetInvoiceWithOrder` for the product
      check (invoice service exposes `OrderFor(ctx, inv)`? no — the handler holds `Store` too:
      `ConsumerInvoiceHandler{Invoices, Store}`), `Cache-Control: no-store`.
- [ ] **Step 4 — commit** `feat(invoices): consumer invoice route with product scoping`.

## T7 — Reports: revenue, reconciliation, VAT + CSV

**Files:** create `internal/store/reports.go`, `internal/store/reports_test.go`,
`internal/handler/payments_reports_admin.go`, `internal/handler/payments_reports_admin_test.go`;
edit `paymentSourceFiles` + `paymentVocabulary` if a new code is written (prefer reusing
`BAD_REQUEST`/`INTERNAL_ERROR` only).

**Store interfaces:**

```go
type ReportWindow struct{ From, To time.Time } // [From, To)
type RevenueQuery struct {
    Window ReportWindow
    GroupBy string // period | provider | plan | product
    Period  string // day | month (GroupBy == period)
    Provider, ProductID, PlanID string
}
type RevenueRow struct {
    Period, Provider, PlanID, PlanName, ProductID, ProductName string
    Currency string; CurrencyExponent int16
    Orders int
    SubtotalMinor, DiscountMinor, TaxMinor, TotalMinor int64
}
func (s *Store) RevenueReport(ctx context.Context, q RevenueQuery) ([]RevenueRow, error)

type ReconRow struct {
    Day, Provider, Currency string
    OrdersCount, TransactionsCount int
    OrdersMinor, TransactionsMinor, DriftMinor int64
}
func (s *Store) ReconciliationReport(ctx context.Context, q ReportWindow, provider string) ([]ReconRow, error)

type VATRow struct {
    Period, Currency string; CurrencyExponent int16; TaxRateBP int
    Orders int; TaxableBaseMinor, TaxMinor, TotalMinor int64
}
func (s *Store) VATReport(ctx context.Context, q ReportWindow, period string) ([]VATRow, error)
```

All three bucket with
`date_trunc($period, paid_at AT TIME ZONE 'Asia/Ho_Chi_Minh')` / `occurred_at …` and count orders
with `paid_at` in the window and `status IN ('paid','partially_refunded','refunded')`;
transactions are `direction='in' AND status='matched'`. Money is summed as `BIGINT`.

**Handler:**

```go
type ReportsAdminHandler struct{ Store *store.Store }
func RegisterReportsAdminRoutes(g *gin.RouterGroup, h *ReportsAdminHandler) // /api/v1/admin
// GET /payments/reports/revenue?from=&to=&group_by=&period=&provider=&product_id=&plan_id=&format=
// GET /payments/reports/reconciliation?from=&to=&provider=&format=
// GET /payments/reports/vat?from=&to=&period=day|month&format=
```

Window parsing: RFC3339 or `YYYY-MM-DD` (VN day, converted to UTC bounds); defaults last 30 days;
`to < from` → 400. JSON: `{from, to, timezone:"Asia/Ho_Chi_Minh", rows, totals}`. CSV:
`Content-Type: text/csv`, `Content-Disposition: attachment; filename="revenue-<date>.csv"`,
`csvSafeRow` on user-controlled cells.

- [ ] **Step 1 — failing test `TestRevenueReportBuckets`.** Seed two products/plans, three paid
      orders for provider `sepay` (two in one VN day across the UTC boundary: `2026-11-16 18:00Z`
      = `2026-11-17 01:00 +07`), one `payos` order; group by provider and by period/day. Assert
      counts, subtotal/discount/tax/total sums, and that the two orders land on the VN day.
- [ ] **Step 2 — implement** `RevenueReport`; command
      `go test ./internal/store -run TestRevenueReportBuckets -count=1`.
- [ ] **Step 3 — failing test `TestReconciliationReportDrift`.** One paid order 500_000 with a
      matched transaction 400_000 (drift 100_000), one day with only a transaction, one day with
      only an order; assert each day's counts/amounts/drift and `direction='out'` excluded.
- [ ] **Step 4 — failing test `TestVATReportSplit`.** Orders with `price_includes_tax` (tax
      contained) and exclusive (tax added) and USD exponent 2: per period/rate asserts
      `taxable_base = total - tax`, tax and gross sums; grouping by month vs day.
- [ ] **Step 5 — failing test `TestReportsCSVShape`.** Handler called directly for each report
      with `?format=csv`; assert `Content-Disposition: attachment`, header row equals the
      documented columns, row count, formula-safe cell (`=SUM(A1)` prefixed with `'`), and JSON
      shape for the non-CSV call. Auth: register under the admin group (as T5) and assert 403
      without admin scope.
- [ ] **Step 6 — commit** `feat(reports): revenue, reconciliation and VAT with CSV export`.

## T8 — Wiring snippet, docs-in-report and full verification

- [ ] **Step 1 — write the wiring snippet** (no file edit) for `cmd/server/main.go`:
      construct `invoice.New(db, invoiceStorage, logger)`, set `.Events = webhookSvc`, set
      `.EInvoice = nil` (vendor deferred), add the invoice service to the settlement
      `PaidHook` composite, construct the handlers and call the three registration functions in
      the admin/consumer groups.
- [ ] **Step 2 — full verification.**
      `go build ./... && go vet ./... && go test ./internal/payment/... ./internal/handler/... ./internal/store/... -count=1`
      with `TEST_DATABASE_URL`; paste pass counts into the report.
- [ ] **Step 3 — commit** `docs(invoices): module C report and wiring snippet` if any plan/report
      document is added (otherwise no commit).

## Deferred (explicitly out of scope)

- Vendor e-invoice adapter (`EInvoiceProvider` implementation for SePay eInvoice / MISA / VNPT /
  Viettel) — the interface and the 502 mapping exist, `s.EInvoice` stays nil.
- All web UI: admin invoice list/detail, report pages, portal invoice pages (later phase-1b web
  module).
- Refund netting inside the revenue report (refunds are recorded manually and reported by the
  refunds API; the revenue report shows collected amounts).
- Invoice email delivery (module D/portal owns notifications).
