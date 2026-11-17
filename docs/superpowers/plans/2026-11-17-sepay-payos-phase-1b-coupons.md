# SePay + PayOS Phase 1b — Module B: Coupons — implementation plan

> **For agentic workers:** This plan is executed task by task, in order, with TDD. Every
> task ends with the exact verification commands and a commit. Steps use `- [ ]` checkboxes.
> Code blocks are the intended implementation, not sketches: copy them, then run the tests.
> Read `2026-11-17-sepay-payos-phase-1b-CONTRACT.md` first — it is binding and overrides any
> wording here. If the two disagree, stop and report to the Lead; do not "fix" the contract.

**Goal:** Deliver the coupon module of phase 1b: a `coupons` package that validates and prices
percent/fixed/trial codes (scopes, stacking, limits, email binding) with VAT consistent with
checkout; checkout integration that snapshots `payment_order_discounts` and settles zero-total
trial orders at creation; redemption recorded atomically at settlement through the Lead's
`PaidHook`; consumer API request/response fields; admin CRUD, redemption history and CSV; portal
validate; and the admin web surface (coupons list, editor with live quote preview, redemption
history) with en+vi strings.

**Architecture:** One pricing rule set (`coupons.Quote`) serves checkout, the admin preview and
the portal preview, so the price a buyer sees is the price the order is written with. Checkout
resolves plan/product/price (and therefore VAT settings), passes the resolved subtotal to
`Quote`, persists the returned totals plus one `payment_order_discounts` row per amount coupon,
and snapshots the applied codes in the order's server-owned `metadata.coupons` block. A
zero-total order (trial coupon) is settled immediately through the existing
`settlement.Service.Settle` entry point with a synthetic zero-amount paid event, which grants the
licence, marks the order paid and dispatches the normal events; the Lead's composite `PaidHook`
then calls `coupons.Service.RedeemForOrder`. Redemption is idempotent through
`UNIQUE(order_id, coupon_code)` and increments `coupons.redeemed_count` under a row lock.

**Tech Stack:** Go 1.27, Gin 1.12, Bun ORM, PostgreSQL 16; `go test` with `TEST_DATABASE_URL`
(`internal/testsupport/testdb`); React 19 + Vite + Tailwind 4 + TanStack Query + Radix, Bun.

**Spec:** `docs/superpowers/specs/2026-11-12-sepay-payos-integration-design.md` §3.3 (money/VAT),
§3.4–§3.5 (lifecycle, zero-total), §3.9 (coupons), §4.9 (DDL), §6.1–§6.5 (API + codes), §10
(required tests), §11 item 4.

**Contract:** `docs/superpowers/plans/2026-11-17-sepay-payos-phase-1b-CONTRACT.md` (module B
surface, owned files, wiring protocol). **Master:** `...-phase-1b-master.md` module 2.

---

## Background: what phase 1a already provides (do not rebuild)

Read these before Task 1; every one is used by this plan:

| Piece | Where | What it gives you |
|---|---|---|
| Money/VAT | `internal/payment/domain/money.go` | `ComputeTotals(subtotal, discount, taxRateBP, priceIncludesTax)` → `Totals{NetMinor, TaxMinor, TotalMinor, DiscountMinor}`, half-up rounding, inclusive or exclusive VAT, clamps the discount. Reuse it; never re-derive VAT. |
| Currency exponent | `domain.CurrencyExponent` | VND/JPY/KRW 0, everything else 2. |
| Order + discount persistence | `internal/store/payments.go` `CreatePaymentOrder` | Already inserts `store.CreateOrderInput.Discounts []store.OrderDiscount` **inside the order transaction**. Checkout only has to pass them. |
| Order state | `store.MarkPaymentOrderPaid`, `domain.OrderPaid/Pending`, `domain.IsOrderTerminal` | Paid is terminal for redemption purposes. |
| Settlement | `internal/payment/settlement/service.go` | `Settle(ctx, orderID, providerName, ev)` is idempotent, grants the licence, marks paid, records the transaction, dispatches `order.paid`/`payment.succeeded`, then runs `PaidHook`. `orderMismatch` accepts a zero amount when the order total is zero. |
| Licence grant | `internal/payment/settlement/build.go`, `internal/store/payment_settlement.go` | Uses `order.PeriodEnd` for a trial/subscription first period; a `license_type='trial'` plan becomes `trialing`, a subscription stays `active` with `valid_until = period_end`. |
| Store plumbing | `internal/store/store.go` | `RunInTx`, `GetSetting`, `Audit` (async), `NewID`. |
| Handlers | `internal/handler/payments_api.go`, `pagination.go`, `payments_admin.go` | Envelope, `listPage`/`listOK`/`listSort`, `adminID`, CSV (`csvSafeRow` in `admin.go`). |
| Contract test | `internal/handler/payments_contract_test.go` | Scans a fixed file list for literal `response.Err(c, http.StatusX, "CODE", …)` sites and pins one code ⇔ one status. |
| Coupon tables | `db/migrations/20261112_02_coupons.up.sql`, `…_03_payment_core.up.sql` | `coupons`, `coupon_scopes`, `payment_order_discounts` (type CHECK is **percent/fixed only**), `coupon_redemptions` (type percent/fixed/trial, `UNIQUE(order_id,coupon_code)`). **No migration is needed or allowed.** |

**1a has no zero-total path.** `settlement.Settle` is the only settlement entry point and it
accepts a zero amount when the order total is zero, so the minimal correct path is to call it
with a synthetic paid event at creation (Task 5). Do not add a second settlement implementation
and do not edit `internal/payment/settlement/**` (Lead-owned).

---

## Global Constraints

- **Do not edit:** `cmd/server/main.go`, `internal/payment/settlement/**`, `internal/model/**`,
  `db/migrations/**`, `internal/storage/**`. They are Lead/shared files. If a change there looks
  necessary, write the exact diff in the task's report and continue with what is possible.
- **Do edit (owned):** `internal/payment/coupons/**` (new), `internal/store/coupons.go` (new),
  `internal/payment/checkout/service.go`, `internal/handler/payments_api.go`,
  `internal/handler/coupons_admin.go` (new), `internal/handler/coupons_portal.go` (new),
  `internal/handler/payments_contract_test.go` (emitter flips `emitted`), web files listed in
  Task 10–13.
- Money is integer minor units + exponent. No floats, no `float64`, no `strconv.FormatFloat` on
  money. Percentages are basis points (`1000` = 10%).
- MySQL-is-not-a-target: write Postgres SQL (partial indexes, `ON CONFLICT`, `FOR UPDATE`).
- One error code ⇔ one HTTP status. Codes are written as **literal strings** at the
  `response.Err` call site (the contract test parses the AST and refuses variables). The
  coupons package exports the same values as constants; a test keeps them in step.
- Every admin write calls `h.Store.Audit` with `Entity`, `EntityID`, `Action`, `ActorID`,
  `ActorType:"admin"`, `IPAddress`, `Changes`.
- Route registration functions live in the handler package and are called by the Lead:
  `RegisterCouponsAdminRoutes(g *gin.RouterGroup, h *CouponsAdminHandler)` and
  `RegisterCouponsPortalRoutes(g *gin.RouterGroup, h *CouponsPortalHandler)`.
- No secrets in logs. Log order **codes**, never buyer payloads or provider secrets.
- Attribution (`X-Powered-By`, footer) is untouched; no Stripe calls.
- Frontend strings in **both** `web/src/i18n/locales/en.ts` and `vi.ts`; vi is typed as a subset
  of en (`Partial<Translations>`), so a missing vi key silently falls back to English — the
  Task 13 checklist verifies each new key exists in both files.

---

## Review Focus (the scenarios a reviewer must be able to trace)

1. **Two general coupons are rejected, general+campaign stacks, order is priority then code**
   — `admit` (Task 1), tested in `TestAdmitStackingRules`, and end-to-end through checkout in
   `TestCheckoutStacksGeneralAndCampaign` (Task 5).
2. **A trial coupon never mixes with an amount coupon and never lands on a perpetual or
   trial-less plan** — `validate` + `amountRule` (Tasks 1 and 3), tested in
   `TestTrialCannotMix` and `TestTrialNeedsRecurringPlanWithTrialDays`.
3. **A trial order is settled at creation: paid, one licence, zero provider attempts, one trial
   redemption** — `TestCheckoutTrialOrderSettlesAtCreation` (Task 5) drives the real
   `settlement.Service`.
4. **A buyer cannot escape `assigned_email`, even with `ignore_invalid_coupons`; the code is
   never applied, only skipped** — `TestAssignedEmailIsHardBlocked` (Task 3) and
   `TestCheckoutIgnoreInvalidStillRefusesAssigned` (Task 5).
5. **VAT after stacking equals VAT on the discounted total, with no double tax** — `price` uses
   `domain.ComputeTotals` once (Task 1), and `TestQuoteMatchesCheckoutTotals` compares the order
   row to the quote for inclusive and exclusive products (Task 5).
6. **Redemption is written only at settlement, once per (order, coupon), and the counter never
   drifts under concurrency** — `TestRedeemIsIdempotent`, `TestRedeemConcurrentSameOrder`,
   `TestRedeemConcurrentDifferentOrders` (Task 4); abandoned orders leave `redeemed_count` at 0.
7. **Deleting a coupon with redemptions archives instead; deleting one referenced only by a
   pending order leaves the settlement ledger intact** — `TestDeleteArchivesWhenRedeemed`,
   `TestRedeemSkipsDeletedCoupon` (Tasks 2 and 4).
8. **A retried checkout with different coupon codes does not silently reuse an order priced with
   the old codes** — `TestResumableOrderMatchesCouponSignature` (Task 5).
9. **Tax code is required exactly when the buyer asks for an invoice** — `TestCreateOrderRequiresTaxCode`
   (Task 6); the field is persisted in `orders.buyer` for module C.
10. **Scope replacement is atomic and audited, and every coupon route is registered through the
    module's one function** — `TestUpdateCouponReplacesScopes`,
    `TestCouponWritesAreAudited`, `TestRegisterCouponsAdminRoutes` (Tasks 7–8).

---

## File map

**New Go files**

| File | Responsibility |
|---|---|
| `internal/payment/coupons/coupon.go` | Rejection codes, `Error`, `ErrorCode`. |
| `internal/payment/coupons/quote.go` | Pure rule engine: `QuoteInput`, `Quote`, `Discount`, `admit`, `price`, `defaultMaxStack`. |
| `internal/payment/coupons/service.go` | `Service`, `New`, `Quote`, `PortalValidate`, validation, `maxStack`, metadata block helpers, `CodesSignature`. |
| `internal/payment/coupons/redeem.go` | `RedeemForOrder`, `AfterPaid` (satisfies `settlement.PaidHook`). |
| `internal/store/coupons.go` | Coupon CRUD + scopes + lookups + per-customer counts + discount read + redemption ledger + redemption report query. |
| `internal/handler/coupons_admin.go` | Admin CRUD, preview, per-coupon + global redemption history, CSV, `RegisterCouponsAdminRoutes`. |
| `internal/handler/coupons_portal.go` | `POST /portal/coupons/validate`, `RegisterCouponsPortalRoutes`. |

**New test files**

| File | Covers |
|---|---|
| `internal/payment/coupons/quote_test.go` | Pure engine: stacking, percent, fixed, rounding, cap, trial, ordering, ignore. |
| `internal/payment/coupons/service_test.go` | DB-backed `Quote`/`PortalValidate`: active/validity/scope/exhausted/per-customer/assigned. |
| `internal/payment/coupons/redeem_test.go` | Ledger, idempotency, concurrency, deleted coupon, trial row, `PaidHook` assertion. |
| `internal/store/coupons_test.go` | CRUD, scope replacement, code uniqueness/normalisation, delete vs archive, filters, report filters. |
| `internal/payment/checkout/coupons_test.go` | Checkout quote application, discount rows, VAT match, trial zero-total, reuse signature, preview. |
| `internal/handler/coupons_admin_test.go` | Admin CRUD/validation/audit/preview/redemptions/CSV/routes. |
| `internal/handler/coupons_portal_test.go` | Portal validate auth, one-code rule, discount preview. |

**Modified Go files:** `internal/payment/checkout/service.go`, `internal/handler/payments_api.go`,
`internal/handler/payments_api_test.go`, `internal/handler/payments_contract_test.go`.

**Web files:** `web/src/lib/api.ts`, `web/src/i18n/locales/en.ts`, `web/src/i18n/locales/vi.ts`,
`web/src/main.tsx`, `web/src/components/layout.tsx`, `web/src/pages/admin/coupons.tsx` (new),
`web/src/pages/admin/coupon-editor.tsx` (new), `web/src/pages/admin/coupon-redemptions.tsx` (new).

---

## Task overview

| # | Task | Delivers |
|---|---|---|
| 1 | Coupon vocabulary + pure discount engine | `coupons` codes/errors and the stacking/pricing rules, unit-tested without a database. |
| 2 | Coupon store | CRUD, atomic scope replacement, lookups, per-customer counts, delete/archive, redemption ledger + report query. |
| 3 | Coupons service: `Quote`, `PortalValidate`, metadata | DB-backed validation and the one pricing entry point. |
| 4 | Redemption | `RedeemForOrder`, `AfterPaid`, idempotent/concurrent ledger writes. |
| 5 | Checkout integration | `coupon_codes` on `CreateOrderInput`, discount snapshots, zero-total settlement, reuse signature, `PreviewQuote`. |
| 6 | Consumer API | Request fields, `tax_code` validation, `discounts[]`/alias totals, error mapping, vocabulary. |
| 7 | Admin coupons API | CRUD + scopes + audit + preview + route registration. |
| 8 | Redemption history API | Per-coupon and global lists, filters, CSV, route registration. |
| 9 | Portal validate API | Session-scoped preview endpoint. |
| 10 | Web plumbing | API client types/functions, i18n, routes, nav. |
| 11 | Web coupons list | Filters, table, delete/archive, links, empty/error states. |
| 12 | Web coupon editor | All fields, scope multi-select, live quote preview, create/update. |
| 13 | Web redemption history | Filters, table, CSV download. |
| 14 | Phase acceptance | Full suite, contract vocabulary, wiring snippet, hand-off notes. |

---

### Task 1: Coupon vocabulary and the pure discount engine

**Why first:** every later task branches on these codes and prices with this function. Keeping the
rule engine free of `store` lets the matrix (percent/fixed/trial × stacking × rounding × cap) be
tested in milliseconds without a database, which is what makes the §10 matrix practical.

**Files:**
- Create `internal/payment/coupons/coupon.go`
- Create `internal/payment/coupons/quote.go`
- Create `internal/payment/coupons/quote_test.go`

- [ ] **Step 1: Write the failing tests first.**

`internal/payment/coupons/quote_test.go`:

```go
package coupons

import (
	"testing"

	"github.com/tabloy/keygate/internal/model"
)

// coupon builds an active, stackable coupon with the fields the engine
// reads. Currency is irrelevant to the pure engine; Quote's validation
// (service_test.go) covers it.
func coupon(code, typ string, value int64, group string, priority int) *model.Coupon {
	return &model.Coupon{
		ID: "cpn_" + code, Code: code, Type: typ, Value: value,
		StackGroup: group, Stackable: true, Priority: priority, Active: true,
	}
}

func trialCoupon(code string, days int) *model.Coupon {
	c := coupon(code, "trial", 0, "general", 100)
	c.TrialDays = days
	return c
}

func boolPtr(b bool) *bool { return &b }

// inclusiveInput is one VND subtotal with 10% VAT included (the common
// Vietnamese configuration).
func inclusiveInput(subtotal int64) QuoteInput {
	return QuoteInput{
		SubtotalMinor: subtotal, Currency: "VND", CurrencyExponent: 0, Kind: "new",
		Plan:    &model.Plan{ID: "plan_1", ProductID: "prod_1", LicenseType: "trial", TrialDays: 14},
		Product: &model.Product{TaxRateBP: 1000, PriceIncludesTax: boolPtr(true)},
	}
}

func TestAdmitOrderAndStacking(t *testing.T) {
	general := coupon("GEN10", "percent", 1000, "general", 100)
	campaign := coupon("CAMP50", "fixed", 50_000, "campaign", 200)
	private := coupon("VIP", "fixed", 20_000, "private", 50)
	secondGeneral := coupon("GEN20", "percent", 2000, "general", 300)
	fixedOnly := coupon("ONCE", "fixed", 10_000, "general", 100)
	fixedOnly.Stackable = false
	otherFixed := coupon("EXTRA", "fixed", 5_000, "campaign", 200)

	in := inclusiveInput(1_000_000)

	t.Run("order is priority then code", func(t *testing.T) {
		applied, days, err := admit([]*model.Coupon{general, campaign, private}, in, 3)
		if err != nil {
			t.Fatalf("admit: %v", err)
		}
		if days != 0 {
			t.Fatalf("trial days = %d", days)
		}
		want := []string{"VIP", "GEN10", "CAMP50"}
		if len(applied) != len(want) {
			t.Fatalf("applied = %d coupons, want %d", len(applied), len(want))
		}
		for i, code := range want {
			if applied[i].Code != code {
				t.Fatalf("applied[%d] = %s, want %s", i, applied[i].Code, code)
			}
		}
	})

	t.Run("two in the same group conflict", func(t *testing.T) {
		_, _, err := admit([]*model.Coupon{general, secondGeneral}, in, 2)
		if got := ErrorCode(err); got != CodeGroupConflict {
			t.Fatalf("code = %q, err = %v", got, err)
		}
	})

	t.Run("stack limit counts amount coupons only", func(t *testing.T) {
		_, _, err := admit([]*model.Coupon{general, campaign, private}, in, 2)
		if got := ErrorCode(err); got != CodeStackLimit {
			t.Fatalf("code = %q, err = %v", got, err)
		}
	})

	t.Run("non-stackable rejects a second coupon but works alone", func(t *testing.T) {
		if _, _, err := admit([]*model.Coupon{fixedOnly}, in, 2); err != nil {
			t.Fatalf("single non-stackable coupon must pass: %v", err)
		}
		_, _, err := admit([]*model.Coupon{fixedOnly, otherFixed}, in, 2)
		if got := ErrorCode(err); got != CodeNotStackable {
			t.Fatalf("code = %q, err = %v", got, err)
		}
	})

	t.Run("ignore drops the later conflicting coupon", func(t *testing.T) {
		withIgnore := in
		withIgnore.IgnoreInvalid = true
		applied, _, err := admit([]*model.Coupon{general, secondGeneral}, withIgnore, 2)
		if err != nil {
			t.Fatalf("admit: %v", err)
		}
		if len(applied) != 1 || applied[0].Code != "GEN10" {
			t.Fatalf("applied = %+v", applied)
		}
	})

	t.Run("trial and amount cannot mix; ignore keeps the earlier one", func(t *testing.T) {
		tr := trialCoupon("TRIAL30", 30)
		_, _, err := admit([]*model.Coupon{general, tr}, in, 2)
		if got := ErrorCode(err); got != CodeTrialMixed {
			t.Fatalf("code = %q, err = %v", got, err)
		}
		withIgnore := in
		withIgnore.IgnoreInvalid = true
		applied, days, err := admit([]*model.Coupon{general, tr}, withIgnore, 2)
		if err != nil || len(applied) != 1 || days != 0 {
			t.Fatalf("applied = %+v days = %d err = %v", applied, days, err)
		}
	})

	t.Run("two trial coupons are rejected", func(t *testing.T) {
		_, _, err := admit([]*model.Coupon{trialCoupon("T1", 7), trialCoupon("T2", 30)}, in, 2)
		if got := ErrorCode(err); got != CodeInvalid {
			t.Fatalf("code = %q, err = %v", got, err)
		}
	})
}

func TestPriceStacksPercentThenFixed(t *testing.T) {
	in := inclusiveInput(1_000_000)
	general := coupon("GEN10", "percent", 1000, "general", 100)
	campaign := coupon("CAMP200", "fixed", 200_000, "campaign", 200)
	q := price(in, []*model.Coupon{general, campaign}, 0)

	if q.DiscountMinor != 300_000 || q.TotalMinor != 700_000 {
		t.Fatalf("discount = %d total = %d", q.DiscountMinor, q.TotalMinor)
	}
	// 10% of 1,000,000 = 100,000; the fixed 200,000 comes off the remaining 900,000.
	if q.Discounts[0].AmountMinor != 100_000 || q.Discounts[1].AmountMinor != 200_000 {
		t.Fatalf("discounts = %+v", q.Discounts)
	}
	// 10% VAT included in 700,000: net = round(700000*10000/11000) = 636,364,
	// tax = 63,636.
	if q.TaxMinor != 63_636 {
		t.Fatalf("tax = %d, want 63636", q.TaxMinor)
	}
	if q.SubtotalMinor != 1_000_000 {
		t.Fatalf("subtotal = %d", q.SubtotalMinor)
	}
}

func TestPricePercentUsesRemainingSubtotal(t *testing.T) {
	in := inclusiveInput(1_000_000)
	ten := coupon("TEN", "percent", 1000, "general", 100)
	twenty := coupon("TWENTY", "percent", 2000, "campaign", 200)
	q := price(in, []*model.Coupon{ten, twenty}, 0)

	// 100,000 then 20% of 900,000 = 180,000.
	if q.DiscountMinor != 280_000 {
		t.Fatalf("discount = %d", q.DiscountMinor)
	}
	if q.Discounts[1].AmountMinor != 180_000 {
		t.Fatalf("second discount = %d", q.Discounts[1].AmountMinor)
	}
}

func TestPriceRoundsHalfUp(t *testing.T) {
	cases := []struct {
		name     string
		subtotal int64
		bp       int64
		want     int64
	}{
		{"99.9 rounds to 100", 999, 1000, 100},
		{"50.25 rounds to 50", 1_005, 500, 50},
		{"50.5 rounds to 51", 1_010, 500, 51},
		{"1 basis point of one unit rounds to 0", 1, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := inclusiveInput(tc.subtotal)
			q := price(in, []*model.Coupon{coupon("P", "percent", tc.bp, "general", 100)}, 0)
			if q.Discounts[0].AmountMinor != tc.want {
				t.Fatalf("discount = %d, want %d", q.Discounts[0].AmountMinor, tc.want)
			}
		})
	}
}

func TestPriceCapsDiscountAtSubtotal(t *testing.T) {
	in := inclusiveInput(1_000_000)
	huge := coupon("HUGE", "fixed", 2_000_000, "general", 100)
	q := price(in, []*model.Coupon{huge}, 0)

	if q.DiscountMinor != 1_000_000 || q.TotalMinor != 0 || q.TaxMinor != 0 {
		t.Fatalf("quote = %+v", q)
	}
	if q.Discounts[0].AmountMinor != 1_000_000 {
		t.Fatalf("discount row = %+v", q.Discounts[0])
	}
}

func TestPriceTrialTakesWholeSubtotal(t *testing.T) {
	in := inclusiveInput(2_000_000)
	q := price(in, []*model.Coupon{trialCoupon("TRIAL30", 30)}, 30)

	if q.DiscountMinor != 2_000_000 || q.TotalMinor != 0 || q.TaxMinor != 0 {
		t.Fatalf("quote = %+v", q)
	}
	if q.TrialDays != 30 {
		t.Fatalf("trial days = %d", q.TrialDays)
	}
	if len(q.Discounts) != 1 || q.Discounts[0].Type != "trial" || q.Discounts[0].TrialDays != 30 {
		t.Fatalf("discounts = %+v", q.Discounts)
	}
}

func TestAdmitTrialDaysUseLongerOfPlanAndCoupon(t *testing.T) {
	planFourteen := inclusiveInput(1_000_000)
	_, days, err := admit([]*model.Coupon{trialCoupon("SHORT", 7)}, planFourteen, 2)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if days != 14 {
		t.Fatalf("days = %d, want the plan's 14", days)
	}

	planLong := inclusiveInput(1_000_000)
	planLong.Plan.TrialDays = 30
	_, days, err = admit([]*model.Coupon{trialCoupon("LONG", 45)}, planLong, 2)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if days != 45 {
		t.Fatalf("days = %d, want 45", days)
	}
}

func TestPriceKeepsVATConsistent(t *testing.T) {
	// An exclusive-VAT product adds tax on top of the discounted subtotal;
	// the exclusive and inclusive cases must not both be applied.
	in := inclusiveInput(1_000_000)
	exclusive := false
	in.Product.PriceIncludesTax = &exclusive
	ten := coupon("TEN", "percent", 1000, "general", 100)
	q := price(in, []*model.Coupon{ten}, 0)

	if q.TotalMinor != 990_000 {
		t.Fatalf("total = %d", q.TotalMinor)
	}
	if q.TaxMinor != 90_000 { // 10% of 900,000, added once
		t.Fatalf("tax = %d", q.TaxMinor)
	}
}

func TestCodesSignatureIsOrderInsensitive(t *testing.T) {
	if got := CodesSignature([]string{" b ", "a"}); got != `["A","B"]` {
		t.Fatalf("signature = %q", got)
	}
	if got := CodesSignature(nil); got != "[]" {
		t.Fatalf("empty signature = %q", got)
	}
}
```

- [ ] **Step 2: Run it; it must fail to compile.**

```bash
cd /tmp/kg-1b
go test ./internal/payment/coupons/
```

Expected: `undefined: admit`, `undefined: price`, `undefined: CodeGroupConflict`, ….

- [ ] **Step 3: Implement the vocabulary.**

`internal/payment/coupons/coupon.go`:

```go
// Package coupons applies discount and trial codes to an order price.
//
// One rule set serves every caller: checkout, the admin quote preview and
// the portal preview all call Quote, so what a buyer is shown is what the
// order is written with (spec §3.9). The pure rules live in quote.go and
// depend on nothing but the model; service.go adds the database checks.
package coupons

import (
	"errors"
	"fmt"
)

// Rejection codes. One code ⇔ one HTTP status (spec §6.5), pinned in
// internal/handler/payments_contract_test.go. Handlers must write the
// literal string at the response.Err call site because that test parses
// the AST; TestCouponErrorCodeConstants keeps these values in step.
const (
	CodeInvalid       = "PAYMENT_COUPON_INVALID"        // 422
	CodeExhausted     = "PAYMENT_COUPON_EXHAUSTED"      // 409
	CodeStackLimit    = "PAYMENT_COUPON_STACK_LIMIT"    // 422
	CodeGroupConflict = "PAYMENT_COUPON_GROUP_CONFLICT" // 422
	CodeNotStackable  = "PAYMENT_COUPON_NOT_STACKABLE"  // 422
	CodeTrialMixed    = "PAYMENT_COUPON_TRIAL_MIXED"    // 422
	CodeAssigned      = "PAYMENT_COUPON_ASSIGNED"       // 422
	// CodeCodeTaken is an admin-write code, not a buyer-flow code: a
	// create/update whose code already exists. It is emitted only by
	// coupons_admin.go.
	CodeCodeTaken = "PAYMENT_COUPON_CODE_TAKEN" // 409
)

// Error is a rejected coupon or coupon combination. Code is what a
// client branches on; Message names the code so an operator can act.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func reject(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrorCode returns the coupon rejection code carried by err, or "" when
// err is not a coupon rejection.
func ErrorCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
```

- [ ] **Step 4: Add the code normaliser to the store.**

`internal/store/coupons.go` (Task 2 fills in the rest of this file):

```go
package store

import "strings"

// NormaliseCouponCode is the one spelling a coupon code is stored and
// looked up by. Operators type codes with stray spaces and buyers type
// them in any case; both must reach the same row.
func NormaliseCouponCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}
```

- [ ] **Step 5: Implement the rule engine.**

`internal/payment/coupons/quote.go`:

```go
package coupons

import (
	"encoding/json"
	"math"
	"sort"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/store"
)

const (
	// defaultMaxStack is the coupon_max_stack fallback (spec §3.9). A
	// non-positive or unreadable setting means this, never "no stack".
	defaultMaxStack = 2
	// maxStackCeiling bounds a typo in the setting; the rule exists to
	// keep a stack legible, not to allow twenty discounts.
	maxStackCeiling = 10
	// percentScale is 100% in basis points.
	percentScale int64 = 10000
	// maxSubtotal keeps subtotal*value inside int64. Checkout bounds an
	// order the same way (maxOrderTotalMinor).
	maxSubtotal int64 = math.MaxInt64 / 20000
	// maxCodes bounds one request; a coupon stack is a handful of codes.
	maxCodes = 20
)

// Discount is one applied coupon in a quote's breakdown. Value is the
// coupon's own unit (basis points for percent, minor units for fixed);
// AmountMinor is what it took off this order.
type Discount struct {
	CouponID    string `json:"coupon_id,omitempty"`
	Code        string `json:"code"`
	Type        string `json:"type"`
	Value       int64  `json:"value"`
	AmountMinor int64  `json:"amount_minor"`
	Priority    int    `json:"priority"`
	TrialDays   int    `json:"trial_days,omitempty"`
}

// Quote is the priced result of applying coupon codes to a subtotal.
type Quote struct {
	SubtotalMinor int64      `json:"subtotal_minor"`
	DiscountMinor int64      `json:"discount_minor"`
	TaxMinor      int64      `json:"tax_minor"`
	TotalMinor    int64      `json:"total_minor"`
	TrialDays     int        `json:"trial_days,omitempty"`
	Discounts     []Discount `json:"discounts"`
}

// QuoteInput is one pricing request. The caller supplies the money it has
// already resolved (plan, product, quantity, subtotal) because only
// checkout knows how to resolve a provider price; coupons decides what
// the codes are worth against it. Kind is the order kind ("new" when
// empty); Email and ExternalCustomerID are the buyer identity used by the
// per-customer limit and the email binding.
type QuoteInput struct {
	Codes              []string
	IgnoreInvalid      bool
	Plan               *model.Plan
	Product            *model.Product
	Quantity           int
	SubtotalMinor      int64
	Currency           string
	CurrencyExponent   int16
	Kind               string
	Email              string
	ExternalCustomerID string
}

// admit applies the stacking rules to already-validated coupons and
// returns them in application order (priority ascending, then code), plus
// the effective trial length. Sorting before admitting makes the outcome
// deterministic: priority decides which code wins a conflict, not the
// order the buyer typed.
func admit(cands []*model.Coupon, in QuoteInput, maxStack int) ([]*model.Coupon, int, error) {
	if maxStack < 1 {
		maxStack = defaultMaxStack
	}
	ordered := make([]*model.Coupon, len(cands))
	copy(ordered, cands)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Priority != ordered[j].Priority {
			return ordered[i].Priority < ordered[j].Priority
		}
		return ordered[i].Code < ordered[j].Code
	})

	applied := make([]*model.Coupon, 0, len(ordered))
	groups := make(map[string]string, len(ordered))
	amount := 0
	var trialDays int
	for _, c := range ordered {
		var err error
		if c.Type == "trial" {
			err = trialRule(c, applied)
		} else {
			err = amountRule(c, applied, groups, amount, maxStack)
		}
		if err != nil {
			if in.IgnoreInvalid {
				continue
			}
			return nil, 0, err
		}
		applied = append(applied, c)
		if c.Type == "trial" {
			trialDays = c.TrialDays
			if in.Plan != nil && in.Plan.TrialDays > trialDays {
				trialDays = in.Plan.TrialDays
			}
			continue
		}
		groups[c.StackGroup] = c.Code
		amount++
	}
	return applied, trialDays, nil
}

// trialRule checks a trial coupon against what is already applied. The
// applied set is homogeneous (a trial never coexists with amount coupons),
// so one look at the first entry answers both questions.
func trialRule(c *model.Coupon, applied []*model.Coupon) error {
	if len(applied) == 0 {
		return nil
	}
	if applied[0].Type == "trial" {
		return reject(CodeInvalid, "coupon %q: only one trial coupon can be applied", c.Code)
	}
	return reject(CodeTrialMixed, "coupon %q cannot be combined with amount coupon %q", c.Code, applied[0].Code)
}

// amountRule checks one amount coupon against the applied set: trial
// mixing, the stack size, the stack group, and the stackable flag. The
// stackable flag is symmetric — a non-stackable coupon blocks both being
// combined into and combined onto.
func amountRule(c *model.Coupon, applied []*model.Coupon, groups map[string]string, amount, maxStack int) error {
	if len(applied) > 0 && applied[0].Type == "trial" {
		return reject(CodeTrialMixed, "coupon %q cannot be combined with trial coupon %q", c.Code, applied[0].Code)
	}
	if amount >= maxStack {
		return reject(CodeStackLimit, "coupon %q cannot be added: at most %d coupons can be stacked", c.Code, maxStack)
	}
	if other, ok := groups[c.StackGroup]; ok {
		return reject(CodeGroupConflict, "coupon %q cannot be combined with %q: both are in stack group %q",
			c.Code, other, c.StackGroup)
	}
	for _, a := range applied {
		if !a.Stackable {
			return reject(CodeNotStackable, "coupon %q cannot be combined with %q", c.Code, a.Code)
		}
	}
	if !c.Stackable {
		return reject(CodeNotStackable, "coupon %q cannot be combined with other coupons", c.Code)
	}
	return nil
}

// price computes the money for the admitted coupons. Percent coupons take
// their share of what is still left, so a 10% then 20% stack is 100,000
// followed by 180,000 on a 1,000,000 subtotal, not 280,000 of the
// original. A trial takes the whole subtotal: the buyer owes zero now.
func price(in QuoteInput, applied []*model.Coupon, trialDays int) *Quote {
	q := &Quote{SubtotalMinor: in.SubtotalMinor, Discounts: make([]Discount, 0, len(applied))}
	remaining := in.SubtotalMinor
	if trialDays > 0 {
		q.TrialDays = trialDays
		for _, c := range applied {
			if c.Type != "trial" {
				continue
			}
			q.Discounts = append(q.Discounts, Discount{
				CouponID: c.ID, Code: c.Code, Type: c.Type, Value: c.Value,
				AmountMinor: remaining, Priority: c.Priority, TrialDays: trialDays,
			})
		}
		q.DiscountMinor = remaining
		remaining = 0
	} else {
		for _, c := range applied {
			amount := c.Value
			if c.Type == "percent" {
				amount = (remaining*c.Value + percentScale/2) / percentScale
			}
			if amount < 0 {
				amount = 0
			}
			if amount > remaining {
				amount = remaining
			}
			q.Discounts = append(q.Discounts, Discount{
				CouponID: c.ID, Code: c.Code, Type: c.Type, Value: c.Value,
				AmountMinor: amount, Priority: c.Priority,
			})
			q.DiscountMinor += amount
			remaining -= amount
		}
	}
	rate, inclusive := taxSettings(in)
	totals := domain.ComputeTotals(in.SubtotalMinor, q.DiscountMinor, rate, inclusive)
	q.DiscountMinor = totals.DiscountMinor
	q.TaxMinor = totals.TaxMinor
	q.TotalMinor = totals.TotalMinor
	return q
}

// taxSettings reads the product's VAT configuration. A quote without a
// product (a preview against a plan whose product was not loaded) is
// VAT-free rather than guessed.
func taxSettings(in QuoteInput) (rate int, inclusive bool) {
	if in.Product == nil {
		return 0, true
	}
	inclusive = true
	if in.Product.PriceIncludesTax != nil {
		inclusive = *in.Product.PriceIncludesTax
	}
	return in.Product.TaxRateBP, inclusive
}

// CodesSignature is the canonical JSON form of a set of codes. Checkout
// stores it on the order and compares it when deciding whether a retried
// create may reuse an order: same codes, same price.
func CodesSignature(codes []string) string {
	clean := normaliseCodes(codes)
	sort.Strings(clean)
	if len(clean) == 0 {
		return "[]"
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// normaliseCodes trims, upper-cases and de-duplicates submitted codes,
// preserving the first occurrence. Quote owns this so every caller (and
// the signature) agrees on what one code is.
func normaliseCodes(codes []string) []string {
	seen := make(map[string]bool, len(codes))
	out := make([]string, 0, len(codes))
	for _, raw := range codes {
		code := store.NormaliseCouponCode(raw)
		if code == "" || seen[code] {
			continue
		}
		seen[code] = true
		out = append(out, code)
	}
	return out
}
```

- [ ] **Step 6: Run the engine tests until green.**

```bash
cd /tmp/kg-1b
go test ./internal/payment/coupons/ -run 'TestAdmit|TestPrice|TestCodes' -v
```

Expected: all pass. Then check the package builds into the wider tree:

```bash
go build ./...
go vet ./internal/payment/coupons/ ./internal/store/
```

- [ ] **Step 7: Commit.**

```bash
git add internal/payment/coupons internal/store/coupons.go
git commit -m "feat(coupons): pure discount engine with stacking, percent, fixed and trial rules"
```

---

### Task 2: Coupon store — definitions, scopes, lookups, ledger

**Why:** the admin API, quote validation and redemption all need one place that enforces the
definition invariants the database CHECKs cannot express (scope targets exist, fixed coupons have
a currency, trial coupons do not carry a value), and one place that owns the redemption ledger's
locking and idempotency.

**Files:**
- Modify (replace, keeping `NormaliseCouponCode`) `internal/store/coupons.go`
- Create `internal/store/coupons_test.go`

- [ ] **Step 1: Write the failing tests.**

`internal/store/coupons_test.go` (package `store`, so it can call unexported helpers; it cannot
import `testsupport/testdb` because that package imports `store`, hence the local open helper):

```go
package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func couponTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("skipping integration test: TEST_DATABASE_URL not set")
	}
	s, err := New(dsn)
	if err != nil {
		t.Skipf("skipping integration test: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func couponSuffix() string {
	return strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
}

func baseCouponInput(code string) CouponInput {
	return CouponInput{
		Code: code, Description: "test", Type: "percent", Value: 1000,
		StackGroup: "general", Stackable: true, Priority: 100, Active: true,
	}
}

func TestCreateCouponNormalisesAndLists(t *testing.T) {
	s := couponTestStore(t)
	ctx := context.Background()
	suffix := couponSuffix()

	in := baseCouponInput("  xmas-" + suffix + "  ")
	in.Type = "fixed"
	in.Value = 50_000
	in.Currency = "vnd"
	c, err := s.CreateCoupon(ctx, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.Code != strings.ToUpper("xmas-"+suffix) {
		t.Fatalf("code = %q", c.Code)
	}
	if c.Currency != "VND" {
		t.Fatalf("currency = %q", c.Currency)
	}

	got, err := s.FindCouponByCode(ctx, " XMAS-"+strings.ToUpper(suffix)+" ")
	if err != nil || got.ID != c.ID {
		t.Fatalf("find by code: %v %v", got, err)
	}

	list, total, err := s.ListCoupons(ctx, CouponFilter{Q: "xmas-" + suffix, Page: Page{Limit: 10}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(list) != 1 || list[0].ID != c.ID {
		t.Fatalf("list = %+v total = %d", list, total)
	}

	byCodes, err := s.CouponsByCodes(ctx, []string{"xmas-" + suffix})
	if err != nil || len(byCodes) != 1 {
		t.Fatalf("by codes: %v %v", byCodes, err)
	}
}

func TestCreateCouponRejectsBadDefinitions(t *testing.T) {
	s := couponTestStore(t)
	ctx := context.Background()
	suffix := couponSuffix()

	cases := []struct {
		name   string
		mutate func(*CouponInput)
	}{
		{"percent over 100", func(in *CouponInput) { in.Value = 10001 }},
		{"percent zero", func(in *CouponInput) { in.Value = 0 }},
		{"fixed no currency", func(in *CouponInput) {
			in.Type, in.Value, in.Currency = "fixed", 1000, ""
		}},
		{"trial without days", func(in *CouponInput) {
			in.Type, in.Value, in.TrialDays = "trial", 0, 0
		}},
		{"trial with value", func(in *CouponInput) {
			in.Type, in.Value, in.TrialDays = "trial", 1000, 30
		}},
		{"unknown group", func(in *CouponInput) { in.StackGroup = "friends" }},
		{"bad code", func(in *CouponInput) { in.Code = "has spaces" }},
		{"negative limit", func(in *CouponInput) { in.MaxRedemptions = -1 }},
		{"valid_to before valid_from", func(in *CouponInput) {
			now := time.Now()
			in.ValidFrom = &now
			earlier := now.Add(-time.Hour)
			in.ValidTo = &earlier
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseCouponInput(fmt.Sprintf("bad%d-%s", i, suffix[len(suffix)-6:]))
			tc.mutate(&in)
			_, err := s.CreateCoupon(ctx, in)
			if !errors.Is(err, ErrCouponInvalid) {
				t.Fatalf("err = %v, want ErrCouponInvalid", err)
			}
		})
	}
}

func TestCreateCouponRejectsUnknownScopeTarget(t *testing.T) {
	s := couponTestStore(t)
	ctx := context.Background()
	in := baseCouponInput("scope-" + couponSuffix())
	in.Scopes = []CouponScopeInput{{ScopeType: "plan", ScopeID: "does-not-exist"}}
	if _, err := s.CreateCoupon(ctx, in); !errors.Is(err, ErrCouponInvalid) {
		t.Fatalf("err = %v, want ErrCouponInvalid", err)
	}
}

func TestCouponCodeIsUniqueAndTaken(t *testing.T) {
	s := couponTestStore(t)
	ctx := context.Background()
	code := "dup-" + couponSuffix()
	if _, err := s.CreateCoupon(ctx, baseCouponInput(code)); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := s.CreateCoupon(ctx, baseCouponInput(strings.ToUpper(code)))
	if !errors.Is(err, ErrCouponCodeTaken) {
		t.Fatalf("err = %v, want ErrCouponCodeTaken", err)
	}
}

func TestUpdateCouponReplacesScopesAtomically(t *testing.T) {
	s := couponTestStore(t)
	ctx := context.Background()
	suffix := couponSuffix()
	first := modelProduct(t, s, suffix, "first")
	second := modelProduct(t, s, suffix, "second")

	in := baseCouponInput("scope-" + suffix)
	in.Scopes = []CouponScopeInput{{ScopeType: "product", ScopeID: first.ID}}
	c, err := s.CreateCoupon(ctx, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	in.Scopes = []CouponScopeInput{
		{ScopeType: "product", ScopeID: second.ID},
		{ScopeType: "product", ScopeID: second.ID}, // duplicate must collapse
	}
	updated, err := s.UpdateCoupon(ctx, c.ID, in)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Code != c.Code || updated.ID != c.ID {
		t.Fatalf("updated = %+v", updated)
	}
	scopes, err := s.CouponScopesFor(ctx, []string{c.ID})
	if err != nil {
		t.Fatalf("scopes: %v", err)
	}
	if len(scopes[c.ID]) != 1 || scopes[c.ID][0].ScopeID != second.ID {
		t.Fatalf("scopes = %+v", scopes[c.ID])
	}

	// A rejected update must leave both the definition and its scopes
	// untouched, because the write and the scope replacement share one
	// transaction.
	other := baseCouponInput("other-" + suffix)
	if _, err := s.CreateCoupon(ctx, other); err != nil {
		t.Fatalf("other: %v", err)
	}
	conflict := in
	conflict.Code = other.Code
	if _, err := s.UpdateCoupon(ctx, c.ID, conflict); !errors.Is(err, ErrCouponCodeTaken) {
		t.Fatalf("err = %v, want ErrCouponCodeTaken", err)
	}
	scopes, err = s.CouponScopesFor(ctx, []string{c.ID})
	if err != nil || len(scopes[c.ID]) != 1 || scopes[c.ID][0].ScopeID != second.ID {
		t.Fatalf("scopes after rejected update = %+v err = %v", scopes[c.ID], err)
	}
}

func TestDeleteCouponRemovesOrArchives(t *testing.T) {
	s := couponTestStore(t)
	ctx := context.Background()
	suffix := couponSuffix()

	unused, err := s.CreateCoupon(ctx, baseCouponInput("del-"+suffix))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	archived, err := s.DeleteCoupon(ctx, unused.ID)
	if err != nil || archived {
		t.Fatalf("archived = %v err = %v", archived, err)
	}
	if _, err := s.GetCoupon(ctx, unused.ID); !errors.Is(err, ErrCouponNotFound) {
		t.Fatalf("err = %v, want ErrCouponNotFound", err)
	}
	if _, err := s.DeleteCoupon(ctx, unused.ID); !errors.Is(err, ErrCouponNotFound) {
		t.Fatalf("second delete err = %v, want ErrCouponNotFound", err)
	}

	used, err := s.CreateCoupon(ctx, baseCouponInput("used-"+suffix))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.insertRedemptionFixture(ctx, used.ID, used.Code, "buyer@example.com", "", "del-"+suffix); err != nil {
		t.Fatalf("redemption fixture: %v", err)
	}
	archived, err = s.DeleteCoupon(ctx, used.ID)
	if err != nil || !archived {
		t.Fatalf("archived = %v err = %v, want archived", archived, err)
	}
	kept, err := s.GetCoupon(ctx, used.ID)
	if err != nil {
		t.Fatalf("coupon must survive: %v", err)
	}
	if kept.Active {
		t.Fatalf("archived coupon is still active")
	}
}

func TestCountCouponRedemptionsForUsesIdentity(t *testing.T) {
	s := couponTestStore(t)
	ctx := context.Background()
	c, err := s.CreateCoupon(ctx, baseCouponInput("id-"+couponSuffix()))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A redemption row requires an order; the helper below creates one row
	// directly so this test does not depend on checkout.
	if err := s.insertRedemptionFixture(ctx, c.ID, c.Code, "buyer@example.com", "", "order-1"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	n, err := s.CountCouponRedemptionsFor(ctx, c.ID, "BUYER@example.com", "")
	if err != nil || n != 1 {
		t.Fatalf("n = %d err = %v", n, err)
	}
	n, err = s.CountCouponRedemptionsFor(ctx, c.ID, "buyer@example.com", "ws_1")
	if err != nil || n != 0 {
		t.Fatalf("external id must not match the email bucket: n = %d err = %v", n, err)
	}
}
```

Two helpers are referenced but not written yet (`modelProduct`, `insertRedemptionFixture`) —
write them in this step too:

```go
// modelProduct creates a product the scope tests can point at.
func modelProduct(t *testing.T, s *Store, suffix, name string) *model.Product {
	t.Helper()
	p := &model.Product{Name: "Coupons " + name, Slug: "coupons-" + name + "-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(context.Background(), p); err != nil {
		t.Fatalf("create product: %v", err)
	}
	return p
}

// insertRedemptionFixture writes the minimal order + redemption rows this
// test needs. It bypasses checkout on purpose: the store's counting rule
// is what is under test. The product and plan are created here so the test
// does not depend on another test having seeded the database.
func (s *Store) insertRedemptionFixture(ctx context.Context, couponID, code, email, externalID, orderID string) error {
	product := &model.Product{
		Name: "Redemption fixture", Slug: "redemption-" + orderID, Type: "desktop",
	}
	if err := s.CreateProduct(ctx, product); err != nil {
		return err
	}
	plan := &model.Plan{
		ProductID: product.ID, Name: "Redemption fixture", Slug: "redemption-plan-" + orderID,
		LicenseType: "perpetual", LicenseModel: "standard", MaxActivations: 1, Active: true,
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		return err
	}
	order := &model.PaymentOrder{
		ID: NewID(), Code: "KGFIX" + orderID, ProductID: product.ID, PlanID: plan.ID,
		Email: email, Kind: "new", Quantity: 1, SubtotalMinor: 100, TotalMinor: 100,
		Currency: "VND", CurrencyExp: 0, Provider: "sepay", Status: domain.OrderPending,
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte("{}"), Metadata: []byte("{}"),
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if _, err := s.DB.NewInsert().Model(order).Exec(ctx); err != nil {
		return err
	}
	row := &model.CouponRedemption{
		ID: NewID(), CouponID: couponID, CouponCode: code, OrderID: order.ID,
		Email: strings.ToLower(email), ExternalCustomerID: externalID, Type: "percent",
		AmountMinor: 10, CreatedAt: time.Now(),
	}
	_, err := s.DB.NewInsert().Model(row).Exec(ctx)
	return err
}

- [ ] **Step 2: Run; it must fail.**

```bash
cd /tmp/kg-1b
go test ./internal/store/ -run 'TestCreateCoupon|TestCouponCode|TestUpdateCoupon|TestDeleteCoupon|TestCountCoupon'
```

Expected: compile errors for the missing types and methods.

- [ ] **Step 3: Implement the store.**

Replace `internal/store/coupons.go` with the following (it keeps `NormaliseCouponCode`):

```go
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
)

var (
	// ErrCouponNotFound is a coupon id or code that does not exist.
	ErrCouponNotFound = errors.New("coupon not found")
	// ErrCouponCodeTaken is a create/update whose code is already used.
	ErrCouponCodeTaken = errors.New("coupon code already exists")
	// ErrCouponInvalid is a definition the database CHECKs cannot express:
	// an unknown scope target, a code with unsupported characters, a
	// trial coupon carrying a value, a fixed coupon without a currency.
	ErrCouponInvalid = errors.New("invalid coupon")
)

// couponCodeConstraint is the unique constraint on coupons.code.
const couponCodeConstraint = "coupons_code_key"

// CouponScopeInput is one target of a coupon definition.
type CouponScopeInput struct {
	ScopeType string
	ScopeID   string
}

// CouponInput is a coupon definition as the admin API writes it.
// ValidFrom/ValidTo nil mean open-ended.
type CouponInput struct {
	Code             string
	Description      string
	Type             string
	Value            int64
	TrialDays        int
	Currency         string
	StackGroup       string
	Stackable        bool
	Priority         int
	AssignedEmail    string
	MaxRedemptions   int
	PerCustomerLimit int
	ValidFrom        *time.Time
	ValidTo          *time.Time
	Active           bool
	Scopes           []CouponScopeInput
}

// NormaliseCouponCode is the one spelling a coupon code is stored and
// looked up by. Operators type codes with stray spaces and buyers type
// them in any case; both must reach the same row.
func NormaliseCouponCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// validCouponCode bounds a code to what a URL, a CSV cell and a human can
// carry without quoting.
func validCouponCode(code string) bool {
	if len(code) < 3 || len(code) > 40 {
		return false
	}
	for _, r := range code {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func validCurrency(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for _, r := range currency {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// validateCouponInput enforces the definition invariants once. The
// handler maps ErrCouponInvalid to 400; the database CHECKs remain the
// last line of defence.
func validateCouponInput(in CouponInput) error {
	if !validCouponCode(NormaliseCouponCode(in.Code)) {
		return fmt.Errorf("%w: code must be 3-40 characters of A-Z, 0-9, '-' or '_'", ErrCouponInvalid)
	}
	switch in.Type {
	case "percent":
		if in.Value < 1 || in.Value > 10000 {
			return fmt.Errorf("%w: a percent coupon needs value between 1 and 10000 basis points", ErrCouponInvalid)
		}
		if in.TrialDays != 0 {
			return fmt.Errorf("%w: a percent coupon cannot set trial_days", ErrCouponInvalid)
		}
		if in.Currency != "" {
			return fmt.Errorf("%w: a percent coupon has no currency", ErrCouponInvalid)
		}
	case "fixed":
		if in.Value < 1 {
			return fmt.Errorf("%w: a fixed coupon needs a positive value in minor units", ErrCouponInvalid)
		}
		if !validCurrency(strings.ToUpper(strings.TrimSpace(in.Currency))) {
			return fmt.Errorf("%w: a fixed coupon needs a three-letter currency", ErrCouponInvalid)
		}
		if in.TrialDays != 0 {
			return fmt.Errorf("%w: a fixed coupon cannot set trial_days", ErrCouponInvalid)
		}
	case "trial":
		if in.TrialDays < 1 {
			return fmt.Errorf("%w: a trial coupon needs trial_days greater than zero", ErrCouponInvalid)
		}
		if in.Value != 0 {
			return fmt.Errorf("%w: a trial coupon has no value", ErrCouponInvalid)
		}
		if in.Currency != "" {
			return fmt.Errorf("%w: a trial coupon has no currency", ErrCouponInvalid)
		}
	default:
		return fmt.Errorf("%w: type must be percent, fixed or trial", ErrCouponInvalid)
	}
	switch in.StackGroup {
	case "general", "campaign", "private":
	default:
		return fmt.Errorf("%w: stack_group must be general, campaign or private", ErrCouponInvalid)
	}
	if in.Priority < 0 {
		return fmt.Errorf("%w: priority must not be negative", ErrCouponInvalid)
	}
	if in.MaxRedemptions < 0 || in.PerCustomerLimit < 0 {
		return fmt.Errorf("%w: redemption limits must not be negative", ErrCouponInvalid)
	}
	if in.ValidFrom != nil && in.ValidTo != nil && !in.ValidTo.After(*in.ValidFrom) {
		return fmt.Errorf("%w: valid_to must be after valid_from", ErrCouponInvalid)
	}
	for _, sc := range in.Scopes {
		if sc.ScopeType != "product" && sc.ScopeType != "plan" {
			return fmt.Errorf("%w: scope_type must be product or plan", ErrCouponInvalid)
		}
		if strings.TrimSpace(sc.ScopeID) == "" {
			return fmt.Errorf("%w: a scope needs a scope_id", ErrCouponInvalid)
		}
	}
	return nil
}

func couponFromInput(id string, in CouponInput) *model.Coupon {
	now := time.Now()
	return &model.Coupon{
		ID: id, Code: NormaliseCouponCode(in.Code), Description: strings.TrimSpace(in.Description),
		Type: in.Type, Value: in.Value, TrialDays: in.TrialDays,
		Currency: strings.ToUpper(strings.TrimSpace(in.Currency)),
		StackGroup: in.StackGroup, Stackable: in.Stackable, Priority: in.Priority,
		AssignedEmail: strings.ToLower(strings.TrimSpace(in.AssignedEmail)),
		MaxRedemptions: in.MaxRedemptions, PerCustomerLimit: in.PerCustomerLimit,
		ValidFrom: in.ValidFrom, ValidTo: in.ValidTo, Active: in.Active,
		CreatedAt: now, UpdatedAt: now,
	}
}

// dedupeCouponScopes removes duplicate targets so a request with the same
// scope twice does not fail on the primary key.
func dedupeCouponScopes(scopes []CouponScopeInput) []CouponScopeInput {
	seen := make(map[string]bool, len(scopes))
	out := make([]CouponScopeInput, 0, len(scopes))
	for _, sc := range scopes {
		key := sc.ScopeType + ":" + sc.ScopeID
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, sc)
	}
	return out
}

// checkCouponScopes refuses a target that does not exist: a typo would
// otherwise silently narrow the coupon to nothing.
func checkCouponScopes(ctx context.Context, db bun.IDB, scopes []CouponScopeInput) error {
	for _, sc := range dedupeCouponScopes(scopes) {
		var (
			exists bool
			err    error
		)
		switch sc.ScopeType {
		case "product":
			exists, err = db.NewSelect().Model((*model.Product)(nil)).Where("id = ?", sc.ScopeID).Exists(ctx)
		case "plan":
			exists, err = db.NewSelect().Model((*model.Plan)(nil)).Where("id = ?", sc.ScopeID).Exists(ctx)
		default:
			return fmt.Errorf("%w: scope_type must be product or plan", ErrCouponInvalid)
		}
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: %s %s does not exist", ErrCouponInvalid, sc.ScopeType, sc.ScopeID)
		}
	}
	return nil
}

func insertCouponScopes(ctx context.Context, db bun.IDB, couponID string, scopes []CouponScopeInput) error {
	for _, sc := range dedupeCouponScopes(scopes) {
		row := &model.CouponScope{CouponID: couponID, ScopeType: sc.ScopeType, ScopeID: sc.ScopeID}
		if _, err := db.NewInsert().Model(row).On("CONFLICT DO NOTHING").Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

func getCouponIn(ctx context.Context, db bun.IDB, id string) (*model.Coupon, error) {
	c := new(model.Coupon)
	err := db.NewSelect().Model(c).Where("id = ?", id).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCouponNotFound
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// CreateCoupon writes a definition and its scopes in one transaction.
func (s *Store) CreateCoupon(ctx context.Context, in CouponInput) (*model.Coupon, error) {
	if err := validateCouponInput(in); err != nil {
		return nil, err
	}
	c := couponFromInput(NewID(), in)
	err := RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		if err := checkCouponScopes(ctx, tx, in.Scopes); err != nil {
			return err
		}
		if _, err := tx.NewInsert().Model(c).Exec(ctx); err != nil {
			if isUniqueViolationOn(err, couponCodeConstraint) {
				return fmt.Errorf("%w: %s", ErrCouponCodeTaken, c.Code)
			}
			return err
		}
		return insertCouponScopes(ctx, tx, c.ID, in.Scopes)
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// UpdateCoupon rewrites a definition and replaces its scopes atomically:
// a failure leaves both the old row and the old scopes in place.
func (s *Store) UpdateCoupon(ctx context.Context, id string, in CouponInput) (*model.Coupon, error) {
	if err := validateCouponInput(in); err != nil {
		return nil, err
	}
	var out *model.Coupon
	err := RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		if _, err := getCouponIn(ctx, tx, id); err != nil {
			return err
		}
		if err := checkCouponScopes(ctx, tx, in.Scopes); err != nil {
			return err
		}
		next := couponFromInput(id, in)
		_, err := tx.NewUpdate().Model((*model.Coupon)(nil)).
			Set(`code = ?, description = ?, type = ?, value = ?, trial_days = ?,
				currency = ?, stack_group = ?, stackable = ?, priority = ?,
				assigned_email = ?, max_redemptions = ?, per_customer_limit = ?,
				valid_from = ?, valid_to = ?, active = ?, updated_at = now()`,
				next.Code, next.Description, next.Type, next.Value, next.TrialDays,
				nullIfEmpty(next.Currency), next.StackGroup, next.Stackable, next.Priority,
				next.AssignedEmail, next.MaxRedemptions, next.PerCustomerLimit,
				next.ValidFrom, next.ValidTo, next.Active).
			Where("id = ?", id).Exec(ctx)
		if err != nil {
			if isUniqueViolationOn(err, couponCodeConstraint) {
				return fmt.Errorf("%w: %s", ErrCouponCodeTaken, next.Code)
			}
			return err
		}
		if _, err := tx.NewDelete().Model((*model.CouponScope)(nil)).Where("coupon_id = ?", id).Exec(ctx); err != nil {
			return err
		}
		if err := insertCouponScopes(ctx, tx, id, in.Scopes); err != nil {
			return err
		}
		out, err = getCouponIn(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// nullIfEmpty maps the empty string to SQL NULL for the nullable CHAR(3)
// currency column; ''::char(3) would be three spaces, not "no currency".
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// GetCoupon reads one definition by id.
func (s *Store) GetCoupon(ctx context.Context, id string) (*model.Coupon, error) {
	return getCouponIn(ctx, s.DB, id)
}

// FindCouponByCode reads one definition by its normalised code.
func (s *Store) FindCouponByCode(ctx context.Context, code string) (*model.Coupon, error) {
	c := new(model.Coupon)
	err := s.DB.NewSelect().Model(c).Where("code = ?", NormaliseCouponCode(code)).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCouponNotFound
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// CouponsByCodes reads every definition matching the given codes. Codes
// are normalised by the caller (Quote does) and again here, so a direct
// store call cannot miss a row.
func (s *Store) CouponsByCodes(ctx context.Context, codes []string) ([]*model.Coupon, error) {
	clean := make([]string, 0, len(codes))
	for _, code := range codes {
		if c := NormaliseCouponCode(code); c != "" {
			clean = append(clean, c)
		}
	}
	out := make([]*model.Coupon, 0, len(clean))
	if len(clean) == 0 {
		return out, nil
	}
	if err := s.DB.NewSelect().Model(&out).Where("code IN (?)", bun.In(clean)).Scan(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// CouponScopesFor reads the scope rows of several coupons at once,
// keyed by coupon id. No entry means the coupon covers everything.
func (s *Store) CouponScopesFor(ctx context.Context, couponIDs []string) (map[string][]*model.CouponScope, error) {
	out := make(map[string][]*model.CouponScope, len(couponIDs))
	if len(couponIDs) == 0 {
		return out, nil
	}
	var rows []*model.CouponScope
	if err := s.DB.NewSelect().Model(&rows).
		Where("coupon_id IN (?)", bun.In(couponIDs)).
		Order("coupon_id ASC", "scope_type ASC", "scope_id ASC").
		Scan(ctx); err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.CouponID] = append(out[row.CouponID], row)
	}
	return out, nil
}

// CouponFilter narrows the admin list. ScopeType (with optional ScopeID)
// selects coupons that cover a target.
type CouponFilter struct {
	Q          string
	Active     *bool
	StackGroup string
	ScopeType  string
	ScopeID    string
	Page       Page
}

func (f CouponFilter) apply(q *bun.SelectQuery) *bun.SelectQuery {
	if f.Q != "" {
		like := "%" + strings.TrimSpace(f.Q) + "%"
		q = q.Where("(code ILIKE ? OR description ILIKE ?)", like, like)
	}
	if f.Active != nil {
		q = q.Where("active = ?", *f.Active)
	}
	if f.StackGroup != "" {
		q = q.Where("stack_group = ?", f.StackGroup)
	}
	if f.ScopeType != "" {
		q = q.Where("id IN (SELECT coupon_id FROM coupon_scopes WHERE scope_type = ?)", f.ScopeType)
		if f.ScopeID != "" {
			q = q.Where("id IN (SELECT coupon_id FROM coupon_scopes WHERE scope_type = ? AND scope_id = ?)",
				f.ScopeType, f.ScopeID)
		}
	}
	return q
}

// ListCoupons reads one filtered page plus the total the filter matches.
func (s *Store) ListCoupons(ctx context.Context, f CouponFilter) ([]*model.Coupon, int, error) {
	q := f.apply(s.DB.NewSelect().Model((*model.Coupon)(nil)))
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	q = q.Order("created_at DESC", "id DESC")
	if f.Page.Limit > 0 {
		q = q.Limit(f.Page.Limit)
	}
	if f.Page.Offset > 0 {
		q = q.Offset(f.Page.Offset)
	}
	var out []*model.Coupon
	if err := q.Scan(ctx, &out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// DeleteCoupon removes a coupon that was never redeemed and archives one
// that was: a redemption is a financial record, so the coupon row it
// points at must survive. Returns archived=true when the row was kept.
func (s *Store) DeleteCoupon(ctx context.Context, id string) (bool, error) {
	archived := false
	err := RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		// The row lock serialises this against a concurrent redemption,
		// which takes the same lock before it increments the counter.
		var locked string
		err := tx.NewRaw("SELECT id FROM coupons WHERE id = ? FOR UPDATE", id).Scan(ctx, &locked)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCouponNotFound
		}
		if err != nil {
			return err
		}
		var count int
		if err := tx.NewRaw("SELECT count(*) FROM coupon_redemptions WHERE coupon_id = ?", id).Scan(ctx, &count); err != nil {
			return err
		}
		if count > 0 {
			if _, err := tx.NewUpdate().Model((*model.Coupon)(nil)).
				Set("active = false, updated_at = now()").Where("id = ?", id).Exec(ctx); err != nil {
				return err
			}
			archived = true
			return nil
		}
		res, err := tx.NewDelete().Model((*model.Coupon)(nil)).Where("id = ?", id).Exec(ctx)
		if err != nil {
			return err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return ErrCouponNotFound
		}
		return nil
	})
	return archived, err
}

// CountCouponRedemptionsFor counts a customer's redemptions for one
// coupon. Identity is the external customer id when the caller has one,
// else the lower-cased email — the same rule Quote uses, so the limit
// cannot be dodged by supplying or omitting the external id.
func (s *Store) CountCouponRedemptionsFor(ctx context.Context, couponID, email, externalCustomerID string) (int, error) {
	q := s.DB.NewSelect().Model((*model.CouponRedemption)(nil)).Where("coupon_id = ?", couponID)
	if external := strings.TrimSpace(externalCustomerID); external != "" {
		q = q.Where("external_customer_id = ?", external)
	} else {
		q = q.Where("external_customer_id = ''").
			Where("lower(email) = ?", strings.ToLower(strings.TrimSpace(email)))
	}
	return q.Count(ctx)
}

// ListOrderDiscounts reads an order's snapshotted discount rows in
// application order.
func (s *Store) ListOrderDiscounts(ctx context.Context, orderID string) ([]*model.PaymentOrderDiscount, error) {
	out := make([]*model.PaymentOrderDiscount, 0, 2)
	if err := s.DB.NewSelect().Model(&out).
		Where("order_id = ?", orderID).
		Order("priority ASC", "coupon_code ASC").
		Scan(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// RedemptionInput is one coupon redemption to record for a paid order.
type RedemptionInput struct {
	CouponID           string
	CouponCode         string
	OrderID            string
	Email              string
	ExternalCustomerID string
	Type               string
	AmountMinor        int64
	TrialDays          int
}

// RedeemCouponsForOrder records every coupon a paid order used, in one
// transaction. It is idempotent: UNIQUE(order_id, coupon_code) makes a
// redelivered settlement a no-op, and the counter moves only when a row
// was actually inserted. A coupon deleted between quote and settlement
// has no row to count against; the order's discount snapshot remains the
// evidence, so the redemption is skipped rather than failing a paid order.
func (s *Store) RedeemCouponsForOrder(ctx context.Context, in []RedemptionInput) error {
	if len(in) == 0 {
		return nil
	}
	return RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		for _, r := range in {
			if err := redeemCouponIn(ctx, tx, r); err != nil {
				return err
			}
		}
		return nil
	})
}

func redeemCouponIn(ctx context.Context, tx bun.Tx, in RedemptionInput) error {
	if strings.TrimSpace(in.CouponID) == "" {
		return nil
	}
	var locked string
	err := tx.NewRaw("SELECT id FROM coupons WHERE id = ? FOR UPDATE", in.CouponID).Scan(ctx, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var inserted string
	err = tx.NewRaw(`INSERT INTO coupon_redemptions
			(id, coupon_id, coupon_code, order_id, email, external_customer_id, type, amount_minor, trial_days, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, now())
		ON CONFLICT (order_id, coupon_code) DO NOTHING
		RETURNING id`,
		NewID(), in.CouponID, NormaliseCouponCode(in.CouponCode), in.OrderID,
		strings.ToLower(strings.TrimSpace(in.Email)), strings.TrimSpace(in.ExternalCustomerID),
		in.Type, in.AmountMinor, in.TrialDays).Scan(ctx, &inserted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // already redeemed by an earlier delivery
	}
	if err != nil {
		return err
	}
	_, err = tx.NewRaw("UPDATE coupons SET redeemed_count = redeemed_count + 1, updated_at = now() WHERE id = ?", in.CouponID).Exec(ctx)
	return err
}

// RedemptionRow is one redemption with the order fields a report needs.
type RedemptionRow struct {
	ID                 string    `bun:"id" json:"id"`
	CouponID           string    `bun:"coupon_id" json:"coupon_id"`
	CouponCode         string    `bun:"coupon_code" json:"coupon_code"`
	OrderID            string    `bun:"order_id" json:"order_id"`
	OrderCode          string    `bun:"order_code" json:"order_code"`
	Email              string    `bun:"email" json:"email"`
	ExternalCustomerID string    `bun:"external_customer_id" json:"external_customer_id"`
	Type               string    `bun:"type" json:"type"`
	AmountMinor        int64     `bun:"amount_minor" json:"amount_minor"`
	TrialDays          int       `bun:"trial_days" json:"trial_days"`
	Currency           string    `bun:"currency" json:"currency"`
	CreatedAt          time.Time `bun:"created_at" json:"created_at"`
}

// RedemptionFilter narrows a redemption list. From is inclusive, To is
// exclusive, so consecutive days do not overlap.
type RedemptionFilter struct {
	CouponID   string
	CouponCode string
	Email      string
	From, To   *time.Time
	Page       Page
}

func (f RedemptionFilter) cond() (string, []any) {
	where := []string{"1 = 1"}
	args := make([]any, 0, 5)
	if f.CouponID != "" {
		where = append(where, "r.coupon_id = ?")
		args = append(args, f.CouponID)
	}
	if f.CouponCode != "" {
		where = append(where, "r.coupon_code = ?")
		args = append(args, NormaliseCouponCode(f.CouponCode))
	}
	if f.Email != "" {
		where = append(where, "lower(r.email) = ?")
		args = append(args, strings.ToLower(strings.TrimSpace(f.Email)))
	}
	if f.From != nil {
		where = append(where, "r.created_at >= ?")
		args = append(args, *f.From)
	}
	if f.To != nil {
		where = append(where, "r.created_at < ?")
		args = append(args, *f.To)
	}
	return strings.Join(where, " AND "), args
}

const redemptionSelect = `SELECT r.id, r.coupon_id, r.coupon_code, r.order_id,
	COALESCE(o.code, '') AS order_code, r.email, r.external_customer_id, r.type,
	r.amount_minor, r.trial_days, COALESCE(o.currency, '') AS currency, r.created_at
	FROM coupon_redemptions r
	LEFT JOIN payment_orders o ON o.id = r.order_id`

// ListRedemptions reads one filtered page of the redemption report plus
// the total the filter matches. The order join is LEFT so a redemption
// whose order was deleted still appears in the ledger the counter uses.
func (s *Store) ListRedemptions(ctx context.Context, f RedemptionFilter) ([]*RedemptionRow, int, error) {
	cond, args := f.cond()
	var total int
	if err := s.DB.NewRaw("SELECT count(*) FROM coupon_redemptions r WHERE "+cond, args...).Scan(ctx, &total); err != nil {
		return nil, 0, err
	}
	query := redemptionSelect + " WHERE " + cond + " ORDER BY r.created_at DESC, r.id DESC"
	pageArgs := slices.Clone(args)
	if f.Page.Limit > 0 {
		query += " LIMIT ?"
		pageArgs = append(pageArgs, f.Page.Limit)
	}
	if f.Page.Offset > 0 {
		query += " OFFSET ?"
		pageArgs = append(pageArgs, f.Page.Offset)
	}
	out := make([]*RedemptionRow, 0, 20)
	if err := s.DB.NewRaw(query, pageArgs...).Scan(ctx, &out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// CouponStats is one coupon's usage: how many redemptions and how much
// they took off orders. AmountMinor sums the snapshot, so it survives a
// later repricing of the coupon.
type CouponStats struct {
	Count       int   `bun:"count" json:"count"`
	AmountMinor int64 `bun:"amount_minor" json:"amount_minor"`
}

// CouponRedemptionStats reads one coupon's usage totals.
func (s *Store) CouponRedemptionStats(ctx context.Context, couponID string) (CouponStats, error) {
	var st CouponStats
	err := s.DB.NewRaw(`SELECT count(*) AS count, COALESCE(sum(amount_minor), 0) AS amount_minor
		FROM coupon_redemptions WHERE coupon_id = ?`, couponID).Scan(ctx, &st)
	return st, err
}
```

- [ ] **Step 4: Run the store tests.**

```bash
cd /tmp/kg-1b
TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store/ -run 'TestCreateCoupon|TestCouponCode|TestUpdateCoupon|TestDeleteCoupon|TestCountCoupon' -v
```

If `TEST_DATABASE_URL` is unset the tests skip; they must be run against the integration database
before the task is considered done.

- [ ] **Step 5: Build and vet, then commit.**

```bash
go build ./... && go vet ./internal/store/
git add internal/store/coupons.go internal/store/coupons_test.go
git commit -m "feat(coupons): coupon store with atomic scopes, limits and redemption ledger"
```

---

### Task 3: Coupons service — `Quote`, `PortalValidate`, order metadata

**Why:** the pure engine does not know about the clock, the database or plan kinds. This task
adds the checks that make a code genuinely usable *now*, and exposes the one pricing entry point
checkout and the previews call.

**Files:**
- Create `internal/payment/coupons/service.go`
- Create `internal/payment/coupons/service_test.go`

- [ ] **Step 1: Write the failing service tests.**

`internal/payment/coupons/service_test.go` (package `coupons`, so it can use `price` directly and
`testdb.Store`; `testdb` imports `store`, and `coupons` imports `store`, so there is no cycle):

```go
package coupons

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

// quoteFixture is a durable product+plan plus an open 10%-included VAT
// configuration, matching checkout's fixture.
type quoteFixture struct {
	t       *testing.T
	s       *store.Store
	ctx     context.Context
	product *model.Product
	plan    *model.Plan
}

func newQuoteFixture(t *testing.T) *quoteFixture {
	t.Helper()
	s := testdb.Store(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	inclusive := true
	p := &model.Product{
		Name: "Coupons", Slug: "coupons-" + suffix, Type: "desktop",
		TaxRateBP: 1000, PriceIncludesTax: &inclusive,
	}
	if err := s.CreateProduct(ctx, p); err != nil {
		t.Fatalf("create product: %v", err)
	}
	pl := &model.Plan{
		ProductID: p.ID, Name: "Coupon plan", Slug: "coupon-plan-" + suffix,
		LicenseType: "trial", LicenseModel: "standard", MaxActivations: 3,
		TrialDays: 14, Active: true,
	}
	if err := s.CreatePlan(ctx, pl); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return &quoteFixture{t: t, s: s, ctx: ctx, product: p, plan: pl}
}

func (f *quoteFixture) service() *Service { return New(f.s) }

func (f *quoteFixture) input(subtotal int64) QuoteInput {
	return QuoteInput{
		Plan: f.plan, Product: f.product, Quantity: 1, SubtotalMinor: subtotal,
		Currency: "VND", CurrencyExponent: 0, Kind: "new", Email: "buyer@example.com",
	}
}

func (f *quoteFixture) create(mutate func(*store.CouponInput)) *model.Coupon {
	f.t.Helper()
	in := store.CouponInput{
		Code: "C" + strings.ToUpper(strings.ReplaceAll(time.Now().Format("150405000000"), ".", "")),
		Type: "percent", Value: 1000, StackGroup: "general", Stackable: true,
		Priority: 100, Active: true,
	}
	if mutate != nil {
		mutate(&in)
	}
	c, err := f.s.CreateCoupon(f.ctx, in)
	if err != nil {
		f.t.Fatalf("create coupon: %v", err)
	}
	return c
}

func TestQuoteNoCodesReturnsFullPrice(t *testing.T) {
	f := newQuoteFixture(t)
	q, err := f.service().Quote(f.ctx, f.input(1_000_000))
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if q.DiscountMinor != 0 || q.TotalMinor != 1_000_000 || q.TaxMinor != 90_909 {
		t.Fatalf("quote = %+v", q)
	}
	if q.Discounts == nil || len(q.Discounts) != 0 {
		t.Fatalf("discounts = %+v", q.Discounts)
	}
}

func TestQuoteRejectsInactiveAndExpired(t *testing.T) {
	f := newQuoteFixture(t)
	inactive := f.create(func(in *store.CouponInput) { in.Active = false })
	if _, err := f.service().Quote(f.ctx, f.input(1_000_000)); err != nil {
		t.Fatalf("no codes: %v", err)
	}
	q := f.input(1_000_000)
	q.Codes = []string{inactive.Code}
	if got := ErrorCode(errFrom(f.service().Quote(f.ctx, q))); got != CodeInvalid {
		t.Fatalf("inactive code = %q", got)
	}

	yesterday := time.Now().Add(-24 * time.Hour)
	lastWeek := time.Now().Add(-7 * 24 * time.Hour)
	expired := f.create(func(in *store.CouponInput) {
		in.Code += "X"
		in.ValidFrom, in.ValidTo = &lastWeek, &yesterday
	})
	q = f.input(1_000_000)
	q.Codes = []string{expired.Code}
	if got := ErrorCode(errFrom(f.service().Quote(f.ctx, q))); got != CodeInvalid {
		t.Fatalf("expired code = %q", got)
	}
}

func errFrom(_ *Quote, err error) error { return err }

func TestQuoteRejectsOutOfScope(t *testing.T) {
	f := newQuoteFixture(t)
	otherProduct := &model.Product{Name: "Other", Slug: "other-" + f.plan.Slug, Type: "desktop"}
	if err := f.s.CreateProduct(f.ctx, otherProduct); err != nil {
		t.Fatalf("product: %v", err)
	}
	scoped := f.create(func(in *store.CouponInput) {
		in.Scopes = []store.CouponScopeInput{{ScopeType: "product", ScopeID: otherProduct.ID}}
	})
	q := f.input(1_000_000)
	q.Codes = []string{scoped.Code}
	if got := ErrorCode(errFrom(f.service().Quote(f.ctx, q))); got != CodeInvalid {
		t.Fatalf("code = %q", got)
	}

	// Same coupon, matching plan scope: applies.
	planScoped := f.create(func(in *store.CouponInput) {
		in.Scopes = []store.CouponScopeInput{{ScopeType: "plan", ScopeID: f.plan.ID}}
	})
	q = f.input(1_000_000)
	q.Codes = []string{planScoped.Code}
	got, err := f.service().Quote(f.ctx, q)
	if err != nil {
		t.Fatalf("plan scope: %v", err)
	}
	if got.DiscountMinor != 100_000 {
		t.Fatalf("discount = %d", got.DiscountMinor)
	}
}

func TestQuoteEnforcesMaxRedemptions(t *testing.T) {
	f := newQuoteFixture(t)
	c := f.create(func(in *store.CouponInput) { in.MaxRedemptions = 1 })
	if err := f.s.DB.NewRaw("UPDATE coupons SET redeemed_count = 1 WHERE id = ?", c.ID).Exec(f.ctx); err != nil {
		t.Fatalf("bump: %v", err)
	}
	q := f.input(1_000_000)
	q.Codes = []string{c.Code}
	if got := ErrorCode(errFrom(f.service().Quote(f.ctx, q))); got != CodeExhausted {
		t.Fatalf("code = %q", got)
	}
}

func TestQuoteEnforcesPerCustomerLimit(t *testing.T) {
	f := newQuoteFixture(t)
	c := f.create(func(in *store.CouponInput) { in.PerCustomerLimit = 1 })
	if err := f.s.insertRedemptionFixture(f.ctx, c.ID, c.Code, "buyer@example.com", "", "pc-"+c.ID); err != nil {
		t.Fatalf("redemption fixture: %v", err)
	}
	q := f.input(1_000_000)
	q.Codes = []string{c.Code}
	if got := ErrorCode(errFrom(f.service().Quote(f.ctx, q))); got != CodeExhausted {
		t.Fatalf("same email code = %q", got)
	}
	q.Email = "OTHER@example.com"
	if _, err := f.service().Quote(f.ctx, q); err != nil {
		// Different email is a different customer; the fixture name has no
		// external id, so it matches only the first email.
		t.Fatalf("other email: %v", err)
	}
}
```

`insertRedemptionFixture` is defined in `internal/store/coupons_test.go`, so it is not available
to package `coupons`. Replace that call with a local helper in `service_test.go` that inserts a
`coupon_redemptions` row against a real order — or simpler, use `f.s.DB.NewRaw` with a real order
created by `store.CreatePaymentOrder`. Write the local helper:

```go
// redeemAs records one redemption for the per-customer limit test.
func (f *quoteFixture) redeemAs(couponCode, couponID, email, externalID string) {
	f.t.Helper()
	order, err := f.s.CreatePaymentOrder(f.ctx, store.CreateOrderInput{
		ProductID: f.product.ID, PlanID: f.plan.ID, Email: email, Kind: "new", Quantity: 1,
		SubtotalMinor: 1000, TaxMinor: 90, TotalMinor: 1000, Currency: "VND", CurrencyExponent: 0,
		Provider: "sepay", ExpiresAt: time.Now().Add(time.Hour),
		Buyer: []byte("{}"), Metadata: []byte("{}"),
	})
	if err != nil {
		f.t.Fatalf("order: %v", err)
	}
	row := &model.CouponRedemption{
		ID: store.NewID(), CouponID: couponID, CouponCode: couponCode, OrderID: order.ID,
		Email: strings.ToLower(email), ExternalCustomerID: externalID, Type: "percent",
		AmountMinor: 100, CreatedAt: time.Now(),
	}
	if _, err := f.s.DB.NewInsert().Model(row).Exec(f.ctx); err != nil {
		f.t.Fatalf("redemption: %v", err)
	}
}
```

`store.CreateOrderInput` requires `TaxMinor` etc. (validated by `validateCreateOrderInput`); the
values above are internally consistent (`subtotal - discount + tax` is not checked by the store,
but total >= 0 is).

- [ ] **Step 2: Add the trial, assigned-email and stacking integration tests.**

```go
func TestQuoteTrialNeedsRecurringPlanWithTrialDays(t *testing.T) {
	f := newQuoteFixture(t)
	trial := f.create(func(in *store.CouponInput) {
		in.Type, in.Value, in.TrialDays = "trial", 0, 30
	})
	q := f.input(1_000_000)
	q.Codes = []string{trial.Code}
	got, err := f.service().Quote(f.ctx, q)
	if err != nil {
		t.Fatalf("trial: %v", err)
	}
	if got.TotalMinor != 0 || got.TrialDays != 30 || got.DiscountMinor != 1_000_000 {
		t.Fatalf("quote = %+v", got)
	}

	// A perpetual plan cannot express a trial period; the coupon must not
	// turn it into a free perpetual licence.
	perpetual := &model.Plan{
		ProductID: f.product.ID, Name: "Lifetime", Slug: "lifetime-" + f.plan.Slug,
		LicenseType: "perpetual", LicenseModel: "standard", MaxActivations: 3,
		TrialDays: 30, Active: true,
	}
	if err := f.s.CreatePlan(f.ctx, perpetual); err != nil {
		t.Fatalf("plan: %v", err)
	}
	q = f.input(1_000_000)
	q.Plan = perpetual
	q.Codes = []string{trial.Code}
	if got := ErrorCode(errFrom(f.service().Quote(f.ctx, q))); got != CodeInvalid {
		t.Fatalf("perpetual code = %q", got)
	}

	// A renewal is not a trial.
	q = f.input(1_000_000)
	q.Codes = []string{trial.Code}
	q.Kind = "renewal"
	if got := ErrorCode(errFrom(f.service().Quote(f.ctx, q))); got != CodeInvalid {
		t.Fatalf("renewal code = %q", got)
	}
}

func TestAssignedEmailIsHardBlocked(t *testing.T) {
	f := newQuoteFixture(t)
	c := f.create(func(in *store.CouponInput) { in.AssignedEmail = "owner@example.com" })
	q := f.input(1_000_000)
	q.Codes = []string{c.Code}
	q.Email = "someone@example.com"
	if got := ErrorCode(errFrom(f.service().Quote(f.ctx, q))); got != CodeAssigned {
		t.Fatalf("code = %q", got)
	}
	q.IgnoreInvalid = true
	got, err := f.service().Quote(f.ctx, q)
	if err != nil {
		t.Fatalf("ignore: %v", err)
	}
	if got.DiscountMinor != 0 || len(got.Discounts) != 0 {
		t.Fatalf("an assigned coupon must never be applied: %+v", got)
	}

	q = f.input(1_000_000)
	q.Codes = []string{c.Code}
	q.Email = "OWNER@example.com"
	if _, err := f.service().Quote(f.ctx, q); err != nil {
		t.Fatalf("owner: %v", err)
	}
}

func TestQuoteStacksTwoGroupsAndRejectsTwoGeneral(t *testing.T) {
	f := newQuoteFixture(t)
	general := f.create(func(in *store.CouponInput) { in.Code += "G"; in.Priority = 100 })
	campaign := f.create(func(in *store.CouponInput) {
		in.Code += "C"
		in.Type, in.Value, in.StackGroup, in.Currency, in.Priority = "fixed", 50_000, "campaign", "VND", 200
	})
	q := f.input(1_000_000)
	q.Codes = []string{general.Code, campaign.Code}
	got, err := f.service().Quote(f.ctx, q)
	if err != nil {
		t.Fatalf("stack: %v", err)
	}
	if got.DiscountMinor != 150_000 {
		t.Fatalf("discount = %d (%+v)", got.DiscountMinor, got.Discounts)
	}

	secondGeneral := f.create(func(in *store.CouponInput) { in.Code += "G2"; in.Priority = 300 })
	q = f.input(1_000_000)
	q.Codes = []string{general.Code, secondGeneral.Code}
	if got := ErrorCode(errFrom(f.service().Quote(f.ctx, q))); got != CodeGroupConflict {
		t.Fatalf("code = %q", got)
	}
}

func TestQuoteReturnsDiscountBreakdownInOrder(t *testing.T) {
	f := newQuoteFixture(t)
	first := f.create(func(in *store.CouponInput) { in.Code += "A"; in.Priority = 10 })
	second := f.create(func(in *store.CouponInput) {
		in.Code += "B"
		in.Type, in.Value, in.StackGroup, in.Currency, in.Priority = "fixed", 25_000, "campaign", "VND", 20
	})
	q := f.input(1_000_000)
	q.Codes = []string{second.Code, first.Code} // request order must not matter
	got, err := f.service().Quote(f.ctx, q)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if len(got.Discounts) != 2 || got.Discounts[0].Code != first.Code || got.Discounts[1].Code != second.Code {
		t.Fatalf("discounts = %+v", got.Discounts)
	}
	if got.TotalMinor != 875_000 {
		t.Fatalf("total = %d", got.TotalMinor)
	}
}

func TestQuoteReadsMaxStackSetting(t *testing.T) {
	f := newQuoteFixture(t)
	if err := f.s.SetSetting(f.ctx, "coupon_max_stack", "1"); err != nil {
		t.Fatalf("setting: %v", err)
	}
	t.Cleanup(func() { _ = f.s.SetSetting(context.Background(), "coupon_max_stack", "") })
	general := f.create(func(in *store.CouponInput) { in.Code += "G" })
	campaign := f.create(func(in *store.CouponInput) {
		in.Code += "C"
		in.Type, in.Value, in.StackGroup, in.Currency = "fixed", 10_000, "campaign", "VND"
	})
	q := f.input(1_000_000)
	q.Codes = []string{general.Code, campaign.Code}
	if got := ErrorCode(errFrom(f.service().Quote(f.ctx, q))); got != CodeStackLimit {
		t.Fatalf("code = %q", got)
	}
}
```

`SetSetting(ctx, key, value)` exists (used to persist settings); verify its signature before
writing — if it is `SetSetting(ctx, key, value string) error`, the cleanup above is correct.

- [ ] **Step 3: Run; expect compile failures.**

```bash
cd /tmp/kg-1b
go test ./internal/payment/coupons/ -run 'TestQuote|TestAssigned|TestTrial'
```

- [ ] **Step 4: Implement the service.**

`internal/payment/coupons/service.go`:

```go
package coupons

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// MetadataKey is the order metadata block checkout writes the applied
// codes into. It is server-owned: buyer-supplied metadata is copied under
// it only by checkout, never merged from the request.
const MetadataKey = "coupons"

// Service prices codes and records their redemptions.
type Service struct {
	Store *store.Store
	Log   *slog.Logger
}

// New binds the service to a store. A service with no Log uses the
// process default logger.
func New(st *store.Store) *Service { return &Service{Store: st} }

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Quote validates the submitted codes against the plan, the buyer and the
// clock, applies the stacking rules, and prices what survives. The caller
// supplies the subtotal it has already resolved; this function never
// resolves a price itself.
func (s *Service) Quote(ctx context.Context, in QuoteInput) (*Quote, error) {
	if in.SubtotalMinor < 0 || in.SubtotalMinor > maxSubtotal {
		return nil, fmt.Errorf("coupons: subtotal %d is outside the quotable range", in.SubtotalMinor)
	}
	codes := normaliseCodes(in.Codes)
	if len(codes) == 0 {
		return price(in, nil, 0), nil
	}
	if len(codes) > maxCodes {
		if in.IgnoreInvalid {
			codes = codes[:maxCodes]
		} else {
			return nil, reject(CodeInvalid, "at most %d coupon codes can be applied", maxCodes)
		}
	}
	if in.Plan == nil {
		return nil, errors.New("coupons: a plan is required to price coupon codes")
	}
	rows, err := s.Store.CouponsByCodes(ctx, codes)
	if err != nil {
		return nil, err
	}
	byCode := make(map[string]*model.Coupon, len(rows))
	ids := make([]string, 0, len(rows))
	for _, c := range rows {
		byCode[c.Code] = c
		ids = append(ids, c.ID)
	}
	scopes, err := s.Store.CouponScopesFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	maxStack := s.maxStack(ctx)
	candidates := make([]*model.Coupon, 0, len(codes))
	for _, code := range codes {
		c, ok := byCode[code]
		if !ok {
			if in.IgnoreInvalid {
				continue
			}
			return nil, reject(CodeInvalid, "coupon %q does not exist", code)
		}
		if err := s.validate(ctx, c, scopes[c.ID], in); err != nil {
			if in.IgnoreInvalid {
				continue
			}
			return nil, err
		}
		candidates = append(candidates, c)
	}
	applied, trialDays, err := admit(candidates, in, maxStack)
	if err != nil {
		return nil, err
	}
	return price(in, applied, trialDays), nil
}

// PortalValidate is the portal's preview: exactly one code, always for a
// new purchase. The portal never previews a renewal, so a trial cannot be
// stretched across one.
func (s *Service) PortalValidate(ctx context.Context, in QuoteInput) (*Quote, error) {
	codes := normaliseCodes(in.Codes)
	if len(codes) != 1 {
		return nil, reject(CodeInvalid, "exactly one coupon code is required")
	}
	in.Codes = codes
	in.Kind = "new"
	return s.Quote(ctx, in)
}

// validate checks one coupon against the current time, the plan, the
// buyer identity and the redemption counters. The pure rules (stacking and
// pricing) run later on the survivors.
func (s *Service) validate(ctx context.Context, c *model.Coupon, scopes []*model.CouponScope, in QuoteInput) error {
	now := time.Now()
	switch {
	case !c.Active:
		return reject(CodeInvalid, "coupon %q is not active", c.Code)
	case c.ValidFrom != nil && now.Before(*c.ValidFrom):
		return reject(CodeInvalid, "coupon %q is not valid yet", c.Code)
	case c.ValidTo != nil && now.After(*c.ValidTo):
		return reject(CodeInvalid, "coupon %q has expired", c.Code)
	}
	if !couponCovers(c, scopes, in.Plan) {
		return reject(CodeInvalid, "coupon %q does not apply to this plan", c.Code)
	}
	if c.Currency != "" && !strings.EqualFold(c.Currency, in.Currency) {
		return reject(CodeInvalid, "coupon %q is priced in %s", c.Code, strings.ToUpper(c.Currency))
	}
	if c.AssignedEmail != "" && !strings.EqualFold(c.AssignedEmail, strings.TrimSpace(in.Email)) {
		// A hard block: this code is never applied to another email,
		// with or without ignore_invalid_coupons. With the flag set it is
		// merely skipped, which applies nothing.
		return reject(CodeAssigned, "coupon %q is assigned to another customer", c.Code)
	}
	if c.MaxRedemptions > 0 && c.RedeemedCount >= c.MaxRedemptions {
		return reject(CodeExhausted, "coupon %q has reached its redemption limit", c.Code)
	}
	if c.PerCustomerLimit > 0 {
		n, err := s.Store.CountCouponRedemptionsFor(ctx, c.ID, in.Email, in.ExternalCustomerID)
		if err != nil {
			return err
		}
		if n >= c.PerCustomerLimit {
			return reject(CodeExhausted,
				"coupon %q has already been used the maximum number of times for this customer", c.Code)
		}
	}
	if c.Type == "trial" {
		kind := in.Kind
		if kind == "" {
			kind = "new"
		}
		if kind != "new" {
			return reject(CodeInvalid, "coupon %q is a trial and only applies to new purchases", c.Code)
		}
		// A trial needs a recurring plan. A perpetual plan cannot hold a
		// period, so settling it would grant a free perpetual licence.
		if in.Plan.LicenseType != "trial" && in.Plan.LicenseType != "subscription" {
			return reject(CodeInvalid, "coupon %q needs a recurring plan", c.Code)
		}
		if in.Plan.TrialDays <= 0 {
			return reject(CodeInvalid, "coupon %q needs a plan that offers a trial", c.Code)
		}
	}
	return nil
}

// couponCovers reports whether a coupon's scope matches the plan. An empty
// scope set covers everything; a product scope matches the plan's product,
// a plan scope the plan itself.
func couponCovers(c *model.Coupon, scopes []*model.CouponScope, plan *model.Plan) bool {
	if len(scopes) == 0 {
		return true
	}
	for _, sc := range scopes {
		switch sc.ScopeType {
		case "product":
			if plan != nil && sc.ScopeID == plan.ProductID {
				return true
			}
		case "plan":
			if plan != nil && sc.ScopeID == plan.ID {
				return true
			}
		}
	}
	return false
}

// maxStack reads coupon_max_stack with the documented default. An
// unreadable or non-positive value means the default, never "no stack".
func (s *Service) maxStack(ctx context.Context) int {
	raw, err := s.Store.GetSetting(ctx, "coupon_max_stack")
	if err != nil {
		return defaultMaxStack
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 {
		return defaultMaxStack
	}
	if n > maxStackCeiling {
		return maxStackCeiling
	}
	return n
}

// OrderCoupons is the block checkout snapshots on an order: the applied
// codes, the trial a coupon granted, and the customer identity used for
// the per-customer limit. It is read back at settlement, when the coupon
// row may already have been edited.
type OrderCoupons struct {
	Codes              []string     `json:"codes,omitempty"`
	Trial              *TrialCoupon `json:"trial,omitempty"`
	ExternalCustomerID string       `json:"external_customer_id,omitempty"`
}

// TrialCoupon is the trial a coupon granted.
type TrialCoupon struct {
	CouponID  string `json:"coupon_id"`
	Code      string `json:"code"`
	TrialDays int    `json:"trial_days"`
}

// OrderMetadata renders the block checkout merges into the order metadata.
// It returns nil when no coupon was applied, so an order without coupons
// keeps the caller's metadata untouched. The external customer id lives
// here because payment_orders has no column for it.
func OrderMetadata(q *Quote, externalCustomerID string) map[string]any {
	if q == nil || (len(q.Discounts) == 0 && q.TrialDays == 0) {
		return nil
	}
	block := make(map[string]any, 3)
	codes := make([]string, 0, len(q.Discounts))
	for _, d := range q.Discounts {
		codes = append(codes, d.Code)
		if d.Type == "trial" {
			block["trial"] = map[string]any{
				"coupon_id": d.CouponID, "code": d.Code, "trial_days": d.TrialDays,
			}
		}
	}
	if len(codes) > 0 {
		block["codes"] = codes
	}
	if id := strings.TrimSpace(externalCustomerID); id != "" {
		block["external_customer_id"] = id
	}
	return block
}

// FromOrderMetadata reads the block back. A malformed or absent block is
// an empty one: orders written before coupons existed have none.
func FromOrderMetadata(order *model.PaymentOrder) OrderCoupons {
	var out OrderCoupons
	if order == nil || len(order.Metadata) == 0 {
		return out
	}
	var envelope struct {
		Coupons *OrderCoupons `json:"coupons"`
	}
	if err := json.Unmarshal(order.Metadata, &envelope); err != nil || envelope.Coupons == nil {
		return out
	}
	return *envelope.Coupons
}
```

- [ ] **Step 5: Run the service tests.**

```bash
cd /tmp/kg-1b
TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/payment/coupons/ -v
```

All Task 1 and Task 3 tests must pass. If `TestQuoteEnforcesPerCustomerLimit`'s fixture helper
name collides with anything, rename it `redeemAsCustomer`.

- [ ] **Step 6: Commit.**

```bash
git add internal/payment/coupons/service.go internal/payment/coupons/service_test.go
git commit -m "feat(coupons): quote service with validity, scopes, limits and trial rules"
```

---

<!-- PLAN-CONTINUES -->
