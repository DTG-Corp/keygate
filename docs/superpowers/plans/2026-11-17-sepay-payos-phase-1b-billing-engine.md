# SePay + PayOS — Phase 1b (billing engine, Module A) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give every licence the Vietnamese rails bill a self-running renewal cycle: an hourly worker creates the renewal order and payment instruction 7 days before the period ends, walks a reminder ladder down to suspension, moves the licence `active → past_due → suspended → expired` when nobody pays, reactivates it the moment somebody does, converts trials into paid orders, and prorates plan upgrades — all without calling the Stripe API.

**Architecture:** A new `internal/billing` package owns *decisions*: which licences are due, which rung of the reminder ladder fires, when a licence becomes past due, suspended, expired or reactivated. It writes nothing directly; every state change goes through a narrow, conditional store function in `internal/store/billing.go` (`UPDATE … WHERE status = … AND valid_until = …`, so a stale hourly scan can never overwrite a payment that landed in between), and every order goes through the existing 1a checkout service, extended with `kind`, `license_id`, `period_start`, `period_end` and an upgrade credit. Settlement stays the only writer that turns a paid order into a licence; the plan specifies the one extra `case "upgrade"` Lead has to add there.

**Tech Stack:** Go 1.27, Gin 1.12, Bun ORM + PostgreSQL, `html/template` for email, React 19 (portal, Module D — not this plan).

**Spec:** [docs/superpowers/specs/2026-11-12-sepay-payos-integration-design.md](../specs/2026-11-12-sepay-payos-integration-design.md) §3.6, §3.7, §3.10, §4.11, §8, §10, §11.

**Contract:** [docs/superpowers/plans/2026-11-17-sepay-payos-phase-1b-CONTRACT.md](./2026-11-17-sepay-payos-phase-1b-CONTRACT.md). Module A owns `internal/billing/**`, `internal/store/billing.go`, `internal/service/email_renewal.go` and the renewal/upgrade branches of `internal/payment/checkout/**`. `cmd/server/main.go`, `internal/payment/settlement/service.go`, the `internal/model` foundation and `db/migrations/**` are Lead-owned: this plan **describes** the edits they need and never makes them.

**Phase note:** Phase 1b ships in modules. Module A (this plan) is the billing engine only. Invoices/e-invoice, the customer portal, coupons logic (trial/stacking), reports and the remaining events are Modules B–E and get their own plans.

## State of the worktree at hand-off

This plan was written while the branch moved. Five commits landed in `feat/sepay-payos-phase-1b` before the plan was finished, and an implementer was writing `internal/billing/service.go` when this file was handed over. Read this section before Task 1.

| Commit | What it lands | Bearing on this plan |
|---|---|---|
| `5684d6f` | `checkout`: renewal/upgrade/trial-conversion order kinds, `CreateOrderInput` fields, `store.CreateOrderInput.PeriodStart` | **Task 3 is already done.** Verify it against Task 3's tests, then skip to Task 4. |
| `38f59e0` | `store/billing.go`: `ListBillingSubscriptions`/`GetBillingSubscription` returning `[]*model.Subscription` with `Relation("Plan")`, `Relation("License")`, `Relation("License.Product")`; `MarkBillingPastDue`, `SuspendBillingLicense`, `ExpireBillingLicense`, `CancelSubscriptionAtPeriodEnd` (all taking `(ctx, lic, sub, now)`); `FindOpenRenewalOrderForPeriod`, `IsRenewalOrderConflict`, `ReinstateLicenseForRenewalIn`, `BillingSubscriptionOrNil`; 510 lines of tests | **A first cut of Tasks 1 and 2, in a different shape.** See "Reconciling the landed store" below. |
| `1f50496` | `20261117_04_billing_constraints.{up,down}.sql` | **P0 is already applied.** Tasks 8 and 9 are unblocked. |
| `d4a459c` | `notBillingRailSQL` in `store.go` + `expiry.go` | **P2 applied, with one bug:** it is used for all four sweeps, so `FindLicensesForGraceExpiry` skips cancelled VN licences and they never expire. One-line fix in P2; Task 7's cancel test pins it. |
| `f106a87` | `internal/service/email_renewal.go` + tests | **Task 5 is already done.** Verify against Task 5's tests, including the Vietnamese and custom-template cases. |
| `a5178a1` | `internal/billing/{service,orders,reminders,dunning,cancel,trials}.go`, `fixtures_test.go`, `main_test.go`, `renewal_test.go` (382 + 388 + 281 lines) plus five empty-ish stubs for the later tasks | **A first cut of Task 4**, written against the landed store API and split across six files instead of this plan's `service.go`/`policy.go`/`reminders.go`/`lifecycle.go`/`upgrade.go`/`trial.go`. See below. |

The engine was being written by another agent while this plan was finished, and more commits may land after the table above. `git log --oneline -- internal/billing` is the truth; the invariant is that **Tasks 6–11 are the remaining work**, and their tests are the contract, not the file layout.

Two shapes now exist for Task 4 and they must not be merged by accident. The landing splits the package by *lifecycle stage* (`orders.go`, `reminders.go`, `dunning.go`, `cancel.go`, `trials.go`, one stub each) and reads `*model.Subscription`; this plan splits it by *layer* (policy / reminders / lifecycle / upgrade / trial) and reads `store.BillingSubscription`. Pick one and port the other: the file split is the cheaper of the two to keep, the row type is the one that decides how Tasks 6–11 are written. Whatever is chosen, Task 4's tests — the `renewal_once` race, the lead window, the zero-price skip, the install language, `RunOnceAt` boundaries — must pass unchanged, because Task 6 builds its ladder on top of them.

If the landed row type wins, this is the whole port — every `row.` in Tasks 6–11 has exactly one replacement:

| This plan | Landed shape |
|---|---|
| `row.LicenseID` | `sub.LicenseID` |
| `row.ProductID` | `sub.License.ProductID` |
| `row.PlanID` | `sub.PlanID` |
| `row.PlanName` | `sub.Plan.Name` |
| `row.Email` | `sub.License.Email` |
| `row.LicenseKey` | `s.store.DecryptLicenseKey(sub.License)` |
| `row.PlanLicenseType` | `sub.Plan.LicenseType` |
| `row.BillingInterval` | `sub.Plan.BillingInterval` |
| `row.GraceDays` | `sub.Plan.GraceDays` |
| `row.RenewalDays` | `sub.Plan.RenewalDays` |
| `row.LicenseStatus` | `sub.License.Status` |
| `row.ValidUntil` | `sub.License.ValidUntil` |
| `row.PastDueAt`, `row.SuspendedAt` | `sub.License.PastDueAt`, `sub.License.SuspendedAt` |
| `row.CurrentPeriodStart`, `row.CurrentPeriodEnd` | `sub.CurrentPeriodStart`, `sub.CurrentPeriodEnd` |
| `row.CancelAtPeriodEnd` | `sub.CancelAtPeriodEnd` |
| `row.TrialEnd` | `sub.TrialEnd` |
| `row.PaymentProvider` | `sub.PaymentProvider` |
| `row.PendingRenewalCode/Total/Currency/Exp` | `o, err := s.store.FindOpenRenewalOrderForPeriod(ctx, sub.LicenseID, *sub.CurrentPeriodEnd)` → `o.Code`, `o.TotalMinor`, `o.Currency`, `o.ExpiresAt` |
| `row.PendingUpgradeCode` | `u, err := s.store.FindPendingUpgradeForLicense(ctx, sub.LicenseID)` → `u.Code` |

Two rules survive the port: read the pending order **once**, before the ladder, so the mail and the order decision cannot disagree about what is open; and never let a nil `sub.License` or `sub.Plan` panic the sweep — a subscription whose relation did not load is a logged skip, not a crash in the hourly loop.

### Reconciling the landed store

The landed scan returns `*model.Subscription` with eager relations, and the landed writes take `(*model.License, *model.Subscription)`. This plan's Tasks 1–2 instead define one joined read (`store.BillingSubscription`) and id-based conditional writes. Tasks 4 and 6–11 are written against the plan's shape: the ladder needs the pending order's code, total and expiry in the *same* row it decided on (the landed scan leaves that to a second query per licence, which is the N+1 the plan avoids), and the conditional writes take ids so a stale `*model.Subscription` can never be written back.

Resolve it in favour of this plan, and keep everything the landing got right:

- **Keep** `BillingSuspendedBy`, `IsRenewalOrderConflict` (+ `renewalOnceIndex`), `billingNewestPerLicence`, `FindOpenRenewalOrderForPeriod` and `ReinstateLicenseForRenewalIn` — Tasks 1, 4 and 7 call all of them. `ReinstateLicenseForRenewalIn` is settlement's, so its signature does not change.
- **Replace** `ListBillingSubscriptions`, `GetBillingSubscription` and the four lifecycle writes with Task 1's and Task 2's versions, and move the landed tests for those six functions to the new signatures (the landed assertions are good: keep the cases, change the calls).
- **Do not** keep two scans. One `SELECT … DISTINCT ON (s.license_id)` is the reason the ladder can decide, mail and order from a single snapshot.

If the implementer's in-flight `service.go` is further along than Task 4, keep its structure and port Task 4's decisions into it; the tests in Tasks 4–11 are the contract, not the file layout.

## Global Constraints

- Module path `github.com/tabloy/keygate`; Go 1.27; Gin 1.12; Bun + PostgreSQL. Run `go build ./... && go vet ./...` before every commit.
- Every ID is a TEXT UUID (`store.NewID()`); every FK is TEXT; never BIGSERIAL.
- Money is integer minor units + `currency` + `currency_exponent` (VND 0, USD 2). **Never floats** — not even for a percentage or a proration factor. All proration is integer arithmetic with explicit rounding.
- Time is stored and compared in **UTC**; every duration is a `time.Duration` (24 h days, 30-day months, 365-day years — the same `intervalDuration`/`paidOrderInterval` convention 1a already uses). No `time.Date` month arithmetic, no local-zone comparison, so no DST effect. Vietnamese display uses a fixed `+07:00` zone (`time.FixedZone("ICT", 7*3600)`, no tzdata dependency); decisions never use it.
- The hourly worker must honour `ctx`: no work item starts after cancellation, every ticker loop returns on `<-ctx.Done()`, and no goroutine outlives `Run`.
- Phase 1b never calls the Stripe API, never reads `plans.stripe_price_id` and never writes a Stripe column. `stripe_checkout_session_id` stays `''` on VN rows.
- Migrations are additive only and Lead-owned. This plan **requires one new migration** (§"Lead-owned patches", P0) and Tasks 8 and 9 must not start before it is applied: `license_upgrades` has a non-partial unique index on `stripe_checkout_session_id`, so the second VN row collides.
- Notification rows are unique per `(license_id, tag)`, so ladder tags are **period-scoped**: `<rung>:<period_end RFC3339Z>` (e.g. `renewal_reminder_7:2026-12-01T00:00:00Z`). The rung is `split_part(tag, ':', 1)`, the same shape the dunning code uses. A bare `renewal_reminder_7` would fire once per licence *ever*.
- No hidden clock. `billing.Service` uses `time.Now().UTC()`; every store function and pure policy function takes `now` as a parameter so tests can pin an exact instant. This matches the repo (grep: there is no fake-clock helper anywhere) and keeps `payment_orders.expires_at > now` honest.
- Per-row failures are logged and the run continues (the expiry loop's behaviour); `RunOnce` returns an error only when the scan itself fails or `ctx` is cancelled. A plan with no price is a configuration problem, not a crash.
- `go test -p 1 ./...` stays green. Integration tests need `TEST_DATABASE_URL` and skip without it (`internal/testsupport/testdb`).
- AGPLv3 §7(b): the "Powered by Keygate" footer stays on every renewal email (it is added by `withAttribution` at send time — do not bypass it).

### Shared files this plan edits (declared, additive)

The contract allows Module A to extend checkout and email. Four edits land outside `internal/billing/**` and each is **additive only**, existing callers untouched:

| File | Edit | Why it cannot wait |
|---|---|---|
| `internal/payment/checkout/service.go` | new optional `CreateOrderInput` fields + `kind`/period/credit handling | renewal and upgrade orders have to come from the one service that prices, taxes and dates an order |
| `internal/payment/checkout/proration.go` | new file | the single integer proration implementation, used by billing quotes and by order pricing |
| `internal/store/payments.go` | `CreateOrderInput.PeriodStart *time.Time` (one field, assigned to the column that already exists) | `payment_orders.period_start` is in the schema and the order row is the period record |
| `internal/service/email.go` | 8 new keys in `DefaultTemplates()`, 5 new entries in `templateSample` | without them an admin cannot save a customised renewal template (`ValidateTemplate` rejects unknown variables) |

Everything else this plan needs from a shared file is a **request to Lead** and is listed verbatim in "Lead-owned patches" below.

## Review Focus

Inputs/conditions the spec implies but no single happy-path test covers. Each line is pinned by a test in the task named.

1. **A scan that is already stale when it writes.** The hourly scan reads `current_period_end`, then the customer pays; the run must not mark the licence past due, must not suspend it, must not expire the fresh order, and must not mail "your period ended" for the old period. Conditional `UPDATE … WHERE status = … AND valid_until …` writes plus a fresh re-read before mailing (Task 2, Task 6: `TestPastDueSkipsRenewedSinceScan`, `TestSamePeriod`).
2. **An operator (or a Stripe pause) holds the licence mid-renewal.** A licence that is `suspended`, `revoked` or `canceled` when the run reaches it gets no order, no ladder mail and — crucially — a payment against its old QR must not reactivate it: settlement's existing eligibility guard must stay the only word on that (Task 1, Task 7: `TestScanExcludesHeldLicences`, `TestPaymentDuringGraceReactivates`, `TestPaymentAfterSuspensionIsRecordedUnmatched`).
3. **Plan terms that are missing or zero.** `plans.renewal_days = 0` must not stop a subscription renewing (it is a *perpetual maintenance* term, the interval is the subscription period), a plan with no active `plan_prices` row must produce no order and no mail, and a zero-price plan must not produce a 0 VND QR for a provider to reject (Task 4, Task 6: `TestRenewalIgnoresRenewalDays`, `TestRenewalWithoutPriceIsSkipped`, `TestRenewalSkipsZeroPrice`, `TestReminderNeedsAPayableOrder`).
4. **Two subscription rows for one licence, and two workers racing.** `subscriptions` has no unique licence key, so the scan must take the newest row only; and two replicas running the same hour must end with one order and one mail, not two (Task 1, Task 4: `TestScanTakesNewestSubscription`, `TestConcurrentRunsCreateOneOrder`).
5. **Period boundaries and their display.** `period_end` exactly at run time, the 7/3/1 and grace 1/3/7 edges, and the fact that a VN customer's "due 01/12" is that date in `+07:00` while every decision is UTC (Task 3, Task 6: `TestReminderRungBoundaries`, `TestDisplayDateIsVietnamCalendar`).

Also pinned, but not in the five: a `cancel_at_period_end` flip after an order exists (Task 7), a trial with no conversion plan configured (Task 9), an upgrade whose credit covers the whole price (Task 8), and a `next_cycle` upgrade that must not move money (Task 8).

---

## File Structure

New files (create) and touched files (modify) for this plan. Numbers are tasks.

```
internal/store/
  billing.go                                      T1 (scan + BillingSubscription), T2 (lifecycle writes),
                                                  T8 (upgrade rows, paid-order lookup, ApplyUpgradeIn),
                                                  T10 (ApplyUpgradeFromOrderIn)
  billing_test.go                                 T1, T2, T8, T10
internal/payment/checkout/
  proration.go                                    T3
  proration_test.go                               T3
  service.go                                      T3 (MODIFY: CreateOrderInput, kind, period, credit,
                                                  SettlementCurrency), T9 (MODIFY: ExpiresAt)
  billing_test.go                                 T3 (renewal + upgrade order shapes)
internal/store/payments.go                        T3 (MODIFY: CreateOrderInput.PeriodStart)
internal/billing/
  service.go                                      T4 (Deps, New, Run, RunOnce, renewal order creation),
                                                  T6 (RunOnceAt), T7, T9 (processRow steps)
  policy.go                                       T6 (ladder rung, lead window, suspend window, display date)
  reminders.go                                    T6 (claim + queue the mail)
  lifecycle.go                                    T7 (past due, suspend, expire, cancel_at_period_end)
  trial.go                                        T9 (trial conversion order)
  upgrade.go                                      T8 (quote, order, next_cycle)
  main_test.go                                    T4 (TestMain, testdb.Main)
  fixtures_test.go                                T4 (seeded licence/product/plan/price, fake email + webhook),
                                                  T6 (settings helpers), T7 (pay + reload), T9 (trial/paid plans)
  service_test.go                                 T4
  policy_test.go                                  T6
  reminders_test.go                               T6
  lifecycle_test.go                               T7
  trial_test.go                                   T9
  upgrade_test.go                                 T8
  e2e_test.go                                     T11 (renew → pay → extend, upgrade → pay → plan change)
internal/service/
  email_renewal.go                                T5 (RenewalMail, RenderRenewalMail, en + vi templates)
  email_renewal_test.go                           T5
  email.go                                        T5 (MODIFY: DefaultTemplates, templateSample)
LEAD-OWNED (described here, applied by Lead)
  db/migrations/20261117_04_billing_constraints.{up,down}.sql   P0
  internal/payment/settlement/service.go                        P1 (case "upgrade")
  internal/store/store.go                                       P2 (four sweep predicates)
  internal/service/expiry.go                                    P2 (dunning sweep predicate)
  cmd/server/main.go                                            P3 (wiring)
```

**Layout conventions** (the 1a plan's): one responsibility per file; the service struct in `service.go`; pure decision functions in `policy.go` so a table test can hit every boundary without a database; store SQL in one file so the conditional writes are read together; fixtures in `fixtures_test.go` only.

Do not create files that duplicate existing responsibilities: use `store.RunInTx`, `store.CreatePaymentOrder` (via checkout), `store.ClaimNotification` / `EnqueueEmailAndCloseNotification`, `service.WebhookService.Dispatch`, `testsupport/testdb`.

---

### Task 1: The hourly scan — one row per billable licence

**Files:**
- Create/replace: `internal/store/billing.go` (the file exists from commit `38f59e0`; this task replaces its two read functions and keeps the rest — see "Reconciling the landed store")
- Test: `internal/store/billing_test.go` (the landed scan tests move to the new return type; their cases stay)

**Interfaces:**
- Consumes: `subscriptions`, `licenses`, `plans`, `payment_orders` (all existing); `store.NewID` not needed here.
- Keeps from `38f59e0`, unchanged, in the same file: `BillingSuspendedBy`, `IsRenewalOrderConflict`, `renewalOnceIndex`, `billingNewestPerLicence`, `FindOpenRenewalOrderForPeriod`, `ReinstateLicenseForRenewalIn`.
- Produces: `store.BillingSubscription` (the row every later task reads) and
  `func (s *Store) ListBillingSubscriptions(ctx context.Context, now time.Time, lead time.Duration) ([]*BillingSubscription, error)`,
  `func (s *Store) GetBillingSubscription(ctx context.Context, licenseID string, now time.Time) (*BillingSubscription, error)`.

The scan is the whole contract between SQL and Go, so it is built and tested first. Two properties matter more than the rest: exactly one row per licence when `subscriptions` has duplicates (it has no unique licence key), and the *pending renewal order for this period* carried on the row, because the unique index `payment_orders (license_id, period_end) WHERE kind='renewal' AND status='pending'` is what makes renewal creation idempotent.

- [ ] **Step 1: Write the failing test**

```go
// internal/store/billing_test.go
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

// seedBillingLicence builds a VN-billed subscription licence whose period
// ends at periodEnd, and returns it with its product, plan and subscription.
// Every row is removed when the test ends.
func seedBillingLicence(t *testing.T, s *store.Store, opts billingSeed) (*model.Product, *model.Plan, *model.License) {
	t.Helper()
	ctx := context.Background()
	suffix := newSuffix(t)
	prod := &model.Product{Name: "Billing Co", Slug: "billing-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Monthly", Slug: "billing-monthly-" + suffix,
		LicenseType: opts.licenseType, BillingInterval: "month", LicenseModel: "standard",
		MaxActivations: 3, Active: true, GraceDays: opts.graceDays, RenewalDays: opts.renewalDays,
	}
	if opts.licenseType == "" {
		plan.LicenseType = "subscription"
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{
		ProductID: prod.ID, PlanID: plan.ID, Email: "billing-" + suffix + "@example.com",
		LicenseKey: "KEY-BILL-" + suffix, Status: model.StatusActive, Quantity: 1,
	}
	if err := s.CreateLicenseWithSubscription(ctx, lic, plan); err != nil {
		t.Fatal(err)
	}
	provider := opts.provider
	if provider == "" {
		provider = "sepay"
	}
	// The insert path derives the period from "now"; the test pins it.
	if _, err := s.DB.NewRaw(`UPDATE licenses SET valid_until = ?, status = ? WHERE id = ?`,
		opts.periodEnd, lic.Status, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw(`UPDATE subscriptions
		SET payment_provider = ?, external_id = ?, status = 'active',
		    current_period_start = ?, current_period_end = ?, cancel_at_period_end = ?
		WHERE license_id = ?`,
		provider, "sub-"+suffix, opts.periodEnd.AddDate(0, 0, -30), opts.periodEnd,
		opts.cancelAtPeriodEnd, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.DB.NewRaw("DELETE FROM payment_orders WHERE product_id = ?", prod.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM subscriptions WHERE license_id = ?", lic.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM licenses WHERE id = ?", lic.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM plan_prices WHERE plan_id = ?", plan.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM plans WHERE id = ?", plan.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", prod.ID).Exec(ctx)
	})
	return prod, plan, lic
}

type billingSeed struct {
	licenseType       string
	provider          string
	graceDays         int
	renewalDays       int
	periodEnd         time.Time
	cancelAtPeriodEnd bool
}

// A licence whose period ends inside the lead window is returned with the
// state the engine decides on.
func TestListBillingSubscriptionsSelectsDueLicences(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)
	_, plan, lic := seedBillingLicence(t, s, billingSeed{
		periodEnd: now.Add(5 * 24 * time.Hour), graceDays: 7,
	})

	rows, err := s.ListBillingSubscriptions(ctx, now, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var got *store.BillingSubscription
	for _, row := range rows {
		if row.LicenseID == lic.ID {
			got = row
		}
	}
	if got == nil {
		t.Fatalf("licence %s missing from the scan (%d rows)", lic.ID, len(rows))
	}
	if got.ProductID != lic.ProductID || got.PlanID != plan.ID || got.PlanLicenseType != "subscription" {
		t.Fatalf("row identity = %+v", got)
	}
	if got.LicenseStatus != model.StatusActive || got.GraceDays != 7 || got.PaymentProvider != "sepay" {
		t.Fatalf("row state = %+v", got)
	}
	if got.CurrentPeriodEnd == nil || !got.CurrentPeriodEnd.Equal(now.Add(5*24*time.Hour)) {
		t.Fatalf("current_period_end = %v", got.CurrentPeriodEnd)
	}
	if got.PendingRenewalCode != "" {
		t.Fatalf("unexpected pending renewal %q", got.PendingRenewalCode)
	}
}

// A licence whose period ends beyond the lead window is not yet due; one
// on the Stripe rail is never this engine's business.
func TestListBillingSubscriptionsWindowAndProvider(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)
	_, _, far := seedBillingLicence(t, s, billingSeed{periodEnd: now.Add(30 * 24 * time.Hour)})
	_, _, stripe := seedBillingLicence(t, s, billingSeed{periodEnd: now.Add(2 * 24 * time.Hour), provider: "stripe"})

	rows, err := s.ListBillingSubscriptions(ctx, now, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.LicenseID == far.ID {
			t.Fatalf("licence beyond the lead window was selected: %+v", row)
		}
		if row.LicenseID == stripe.ID {
			t.Fatalf("stripe-billed licence was selected: %+v", row)
		}
	}
}

// subscriptions has no unique licence key: a licence re-subscribed twice
// must appear once, as its newest row, or the engine mints two orders.
func TestScanTakesNewestSubscription(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)
	_, _, lic := seedBillingLicence(t, s, billingSeed{periodEnd: now.Add(2 * 24 * time.Hour)})
	// A second, newer subscription row pointing at a later period: the
	// licence was re-subscribed after the first row was written.
	if _, err := s.DB.NewRaw(`INSERT INTO subscriptions
		(id, license_id, plan_id, status, payment_provider, external_id,
		 current_period_start, current_period_end, created_at, updated_at)
		VALUES (?, ?, (SELECT plan_id FROM licenses WHERE id = ?), 'active', 'sepay', ?,
		        ?, ?, now() + interval '1 minute', now())`,
		store.NewID(), lic.ID, lic.ID, "sub-newer",
		now.Add(28*24*time.Hour), now.Add(58*24*time.Hour)).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListBillingSubscriptions(ctx, now, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, row := range rows {
		if row.LicenseID != lic.ID {
			continue
		}
		n++
		if row.CurrentPeriodEnd == nil || !row.CurrentPeriodEnd.Equal(now.Add(58*24*time.Hour)) {
			t.Fatalf("newest subscription not chosen: %v", row.CurrentPeriodEnd)
		}
	}
	if n != 1 {
		t.Fatalf("licence appeared %d times in the scan", n)
	}
}

// A licence the operator holds gets no row: billing must not mail, order
// or reactivate it. The suspended case is included because its expiry tail
// still needs a row — see TestScanIncludesBillingSuspensions.
func TestScanExcludesHeldLicences(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)
	_, _, revoked := seedBillingLicence(t, s, billingSeed{periodEnd: now.Add(24 * time.Hour)})
	if _, err := s.DB.NewRaw(`UPDATE licenses SET status = 'revoked' WHERE id = ?`, revoked.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	rows, err := s.ListBillingSubscriptions(ctx, now, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.LicenseID == revoked.ID {
			t.Fatalf("revoked licence selected: %+v", row)
		}
	}
}

// GetBillingSubscription is the fresh read the reminder pass does before
// mailing, so a period renewed since the scan is not described as due.
func TestGetBillingSubscription(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)
	_, _, lic := seedBillingLicence(t, s, billingSeed{periodEnd: now.Add(48 * time.Hour)})

	row, err := s.GetBillingSubscription(ctx, lic.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if row.LicenseID != lic.ID || row.CurrentPeriodEnd == nil {
		t.Fatalf("row = %+v", row)
	}
}

func newSuffix(t *testing.T) string {
	t.Helper()
	return strings.ToLower(uuid.NewString()[:13])
}
```

Add the `strings` and `github.com/google/uuid` imports. (`uuid` is already a direct dependency; `internal/store/payment_settlement_test.go` uses it the same way.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store -run 'TestListBillingSubscriptions|TestScanTakesNewestSubscription|TestScanExcludesHeldLicences|TestGetBillingSubscription' -v`

Expected: FAIL — `s.ListBillingSubscriptions undefined (type *store.Store has no field or method ListBillingSubscriptions)`.

- [ ] **Step 3: Write the scan**

```go
// internal/store/billing.go
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
)

// BillingSubscription is one row of the billing engine's hourly scan: the
// newest subscription of a licence the Vietnamese rails bill, with the plan
// and licence state the engine decides on, plus any order already open for
// this period. billing.New is the only consumer.
//
// Every field is read-only; the engine never writes these structs back.
type BillingSubscription struct {
	LicenseID           string
	ProductID           string
	PlanID              string
	PlanName            string
	Email               string
	LicenseKey          string
	LicenseKeyEncrypted []byte

	PlanLicenseType string
	BillingInterval string
	GraceDays       int
	RenewalDays     int

	LicenseStatus string
	ValidUntil    *time.Time
	PastDueAt     *time.Time
	SuspendedAt   *time.Time

	SubscriptionStatus string
	CurrentPeriodStart *time.Time
	CurrentPeriodEnd   *time.Time
	CancelAtPeriodEnd  bool
	TrialEnd           *time.Time
	PaymentProvider    string

	// The pending renewal order for CurrentPeriodEnd, if one is open. The
	// partial unique index on (license_id, period_end) makes this the
	// engine's idempotency key: a row with a code already has its order.
	PendingRenewalID       string
	PendingRenewalCode     string
	PendingRenewalTotal    int64
	PendingRenewalCurrency string
	PendingRenewalExp       int16
	PendingRenewalExpiry   *time.Time

	// The newest pending upgrade order for the licence, if any.
	PendingUpgradeID   string
	PendingUpgradeCode string
	PendingUpgradeTotal int64
}

// billingSelect is shared by the list scan and the single-licence read so
// the two can never drift. It aliases every column to the snake_case name
// bun maps the struct field to.
const billingSelect = `
SELECT DISTINCT ON (s.license_id)
       s.license_id,
       l.product_id,
       s.plan_id,
       p.name                                AS plan_name,
       l.email,
       l.license_key,
       l.license_key_encrypted,
       p.license_type                        AS plan_license_type,
       p.billing_interval,
       p.grace_days,
       p.renewal_days,
       l.status                              AS license_status,
       l.valid_until,
       l.past_due_at,
       l.suspended_at,
       s.status                              AS subscription_status,
       s.current_period_start,
       s.current_period_end,
       s.cancel_at_period_end,
       s.trial_end,
       s.payment_provider,
       ro.id                                 AS pending_renewal_id,
       ro.code                               AS pending_renewal_code,
       ro.total_minor                        AS pending_renewal_total,
       ro.currency                           AS pending_renewal_currency,
       ro.currency_exponent                  AS pending_renewal_exp,
       ro.expires_at                         AS pending_renewal_expiry,
       uo.id                                 AS pending_upgrade_id,
       uo.code                               AS pending_upgrade_code,
       uo.total_minor                        AS pending_upgrade_total
  FROM subscriptions s
  JOIN licenses l ON l.id = s.license_id
  JOIN plans p ON p.id = s.plan_id
  LEFT JOIN payment_orders ro
         ON ro.kind = 'renewal'
        AND ro.license_id = s.license_id
        AND ro.period_end = s.current_period_end
        AND ro.status = 'pending'
  LEFT JOIN LATERAL (
        SELECT o.id, o.code, o.total_minor
          FROM payment_orders o
         WHERE o.kind = 'upgrade'
           AND o.license_id = s.license_id
           AND o.status = 'pending'
         ORDER BY o.created_at DESC
         LIMIT 1
  ) uo ON TRUE
 WHERE s.current_period_end IS NOT NULL
   AND s.status IN ('active', 'trialing', 'past_due')
   AND l.status IN ('active', 'trialing', 'past_due', 'suspended')
   AND s.payment_provider IN ('sepay', 'payos')
`

// ListBillingSubscriptions returns every VN-billed licence whose current
// period ends within lead, at most once per licence (its newest
// subscription row), plus the licences suspended by the engine whose tail
// still needs an expiry write. It is ordered so two runs over the same
// data process licences in the same order.
func (s *Store) ListBillingSubscriptions(ctx context.Context, now time.Time, lead time.Duration) ([]*BillingSubscription, error) {
	rows := []*BillingSubscription{}
	// The suspension branch has no upper window: a licence suspended
	// before this deploy still needs its expiry, and the expire write is
	// conditional and idempotent, so re-selecting it is free.
	err := s.DB.NewRaw(billingSelect+`
   AND (s.current_period_end <= ?
        OR (l.status = 'suspended' AND l.suspended_by = ?))
 ORDER BY 2, 1`, now.Add(lead), BillingSuspendedBy).
		Scan(ctx, &rows)
	if err != nil {
		return nil, fmt.Errorf("list billing subscriptions: %w", err)
	}
	return rows, nil
}

// GetBillingSubscription reads one licence's row with the same query the
// scan uses. The reminder pass calls it immediately before mailing: the
// scan may be an hour old and the customer may have paid in between.
func (s *Store) GetBillingSubscription(ctx context.Context, licenseID string, now time.Time) (*BillingSubscription, error) {
	row := new(BillingSubscription)
	err := s.DB.NewRaw(billingSelect+" AND s.license_id = ?", licenseID).Scan(ctx, row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrBillingSubscriptionNotFound
		}
		return nil, fmt.Errorf("get billing subscription: %w", err)
	}
	return row, nil
}

// BillingSuspendedBy marks a suspension this engine made. Only these are
// lifted by payment and expired by the tail write; an operator's
// suspension is the operator's to lift.
const BillingSuspendedBy = "billing"

var ErrBillingSubscriptionNotFound = errors.New("billing subscription not found")
```

`ORDER BY 2, 1` orders by `current_period_end` then `license_id`; with `DISTINCT ON` inside the same `SELECT`, Postgres requires the `DISTINCT ON` expression to lead the `ORDER BY`, so wrap the query once:

```go
const billingSelect = `SELECT * FROM (` + billingInner + `) b`
```

with `billingInner` the `SELECT DISTINCT ON (s.license_id) … FROM … WHERE …` text above, and the outer `ORDER BY b.current_period_end, b.license_id`. `GetBillingSubscription` appends `AND s.license_id = ?` to the **inner** query, so build the two constants as `billingInner` and a helper:

```go
func billingQuery(extra string) string { return "SELECT * FROM (" + billingInner + extra + ") b" }
```

`ListBillingSubscriptions` uses `billingQuery(" AND (s.current_period_end <= ? OR (l.status = 'suspended' AND l.suspended_by = ?)) ORDER BY …")` — the inner query's `ORDER BY` must stay last, so put the outer ordering in the wrapper: `"SELECT * FROM (" + billingInner + extra + ") b ORDER BY b.current_period_end, b.license_id"`.

- [ ] **Step 4: Run the test to verify it passes**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store -run 'TestListBillingSubscriptions|TestScan' -v`

Expected: PASS (4 tests, some skipped when `TEST_DATABASE_URL` is unset).

- [ ] **Step 5: `go vet` and commit**

```bash
go build ./... && go vet ./internal/store
git add internal/store/billing.go internal/store/billing_test.go
git commit -m "store: billing scan row and query (phase 1b module A)"
```

---

### Task 2: Conditional lifecycle writes

**Files:**
- Modify: `internal/store/billing.go`
- Test: `internal/store/billing_test.go` (append; the landed tests for these four functions move to the id-based signatures rather than being deleted)

**Interfaces:**
- Consumes: `BillingSuspendedBy` (Task 1), `RunInTx`, `model.StatusPastDue/Suspended/Expired`.
- Replaces `38f59e0`'s `MarkBillingPastDue`, `SuspendBillingLicense`, `ExpireBillingLicense` and `CancelSubscriptionAtPeriodEnd`, which take `(*model.License, *model.Subscription)`. The id-based versions here re-check the row inside the `UPDATE`, so nothing read outside the transaction can be written back; `ExpireBillingLicense` is renamed `ExpireBillingSuspendedLicense` because it refuses operator suspensions.
- Produces:
  - `func (s *Store) MarkBillingPastDue(ctx context.Context, licenseID string, now time.Time) (bool, error)`
  - `func (s *Store) SuspendBillingLicense(ctx context.Context, licenseID string, cutoff, now time.Time) (bool, error)`
  - `func (s *Store) ExpireBillingSuspendedLicense(ctx context.Context, licenseID string, cutoff, now time.Time) (bool, error)`
  - `func (s *Store) CancelBillingSubscriptionAtPeriodEnd(ctx context.Context, licenseID string, now time.Time) (bool, error)`

Every write repeats the condition it read, so a stale scan is harmless, and returns `(changed bool)`: `false` means "somebody else already did it", which is not an error and must not emit an event or a mail. All four use the database clock only through the `now`/`cutoff` arguments, so a test pins the instant.

- [ ] **Step 1: Write the failing tests**

```go
// internal/store/billing_test.go (append)
func TestMarkBillingPastDue(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)
	_, _, lic := seedBillingLicence(t, s, billingSeed{periodEnd: now.Add(-time.Hour), graceDays: 7})

	changed, err := s.MarkBillingPastDue(ctx, lic.ID, now)
	if err != nil || !changed {
		t.Fatalf("first call changed=%v err=%v", changed, err)
	}
	got, err := s.FindLicenseByID(ctx, lic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusPastDue || got.PastDueAt == nil {
		t.Fatalf("licence = %s past_due_at=%v", got.Status, got.PastDueAt)
	}
	var sub string
	if err := s.DB.NewRaw("SELECT status FROM subscriptions WHERE license_id = ?", lic.ID).Scan(ctx, &sub); err != nil {
		t.Fatal(err)
	}
	if sub != model.StatusPastDue {
		t.Fatalf("subscription = %s", sub)
	}
	// Idempotent: a second run changes nothing and reports it.
	changed, err = s.MarkBillingPastDue(ctx, lic.ID, now.Add(time.Minute))
	if err != nil || changed {
		t.Fatalf("second call changed=%v err=%v", changed, err)
	}
}

// A payment that lands between the scan and the write must win: the write
// only matches the period it read.
func TestPastDueSkipsRenewedSinceScan(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)
	_, _, lic := seedBillingLicence(t, s, billingSeed{periodEnd: now.Add(-time.Hour)})
	// The customer paid: the licence now runs to the next period.
	if _, err := s.DB.NewRaw("UPDATE licenses SET valid_until = ? WHERE id = ?",
		now.Add(30*24*time.Hour), lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	changed, err := s.MarkBillingPastDue(ctx, lic.ID, now)
	if err != nil || changed {
		t.Fatalf("past due applied to a renewed licence: changed=%v err=%v", changed, err)
	}
	got, _ := s.FindLicenseByID(ctx, lic.ID)
	if got.Status != model.StatusActive {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestSuspendBillingLicenseExpiresOpenRenewalOrder(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)
	periodEnd := now.Add(-8 * 24 * time.Hour)
	prod, plan, lic := seedBillingLicence(t, s, billingSeed{periodEnd: periodEnd, graceDays: 7})
	if _, err := s.DB.NewRaw("UPDATE licenses SET status = 'past_due', past_due_at = ? WHERE id = ?",
		periodEnd, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	order := renewalOrderFor(t, s, prod, plan, lic, periodEnd)

	changed, err := s.SuspendBillingLicense(ctx, lic.ID, now.Add(-7*24*time.Hour), now)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	got, _ := s.FindLicenseByID(ctx, lic.ID)
	if got.Status != model.StatusSuspended || got.SuspendedBy != store.BillingSuspendedBy {
		t.Fatalf("licence = %s by %q", got.Status, got.SuspendedBy)
	}
	// The QR must not be payable after the licence it buys is suspended.
	o, err := s.FindPaymentOrder(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != "expired" {
		t.Fatalf("order status = %s", o.Status)
	}
}

// Grace is a boundary, not a range: one second before it the licence is
// still past due with a payable order.
func TestSuspendBillingLicenseRespectsGraceBoundary(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)
	periodEnd := now.Add(-7*24*time.Hour + time.Second)
	_, _, lic := seedBillingLicence(t, s, billingSeed{periodEnd: periodEnd, graceDays: 7})
	if _, err := s.DB.NewRaw("UPDATE licenses SET status = 'past_due', past_due_at = ? WHERE id = ?",
		periodEnd, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	changed, err := s.SuspendBillingLicense(ctx, lic.ID, now.Add(-7*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("suspended one second before the grace ended")
	}
}

func TestExpireBillingSuspendedLicense(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)
	_, _, lic := seedBillingLicence(t, s, billingSeed{periodEnd: now.Add(-40 * 24 * time.Hour)})
	if _, err := s.DB.NewRaw(`UPDATE licenses SET status = 'suspended', suspended_by = ?,
		suspended_at = ? WHERE id = ?`, store.BillingSuspendedBy, now.Add(-31*24*time.Hour), lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	changed, err := s.ExpireBillingSuspendedLicense(ctx, lic.ID, now.Add(-30*24*time.Hour), now)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	got, _ := s.FindLicenseByID(ctx, lic.ID)
	if got.Status != model.StatusExpired {
		t.Fatalf("status = %s", got.Status)
	}
	// An operator's suspension is not billing's to expire.
	if _, _, op := seedBillingLicence(t, s, billingSeed{periodEnd: now.Add(-90 * 24 * time.Hour)}); true {
		if _, err := s.DB.NewRaw(`UPDATE licenses SET status = 'suspended', suspended_by = 'admin',
			suspended_at = ? WHERE id = ?`, now.Add(-90*24*time.Hour), op.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		changed, err := s.ExpireBillingSuspendedLicense(ctx, op.ID, now.Add(-30*24*time.Hour), now)
		if err != nil || changed {
			t.Fatalf("admin suspension expired: changed=%v err=%v", changed, err)
		}
	}
}

func TestCancelBillingSubscriptionAtPeriodEnd(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	now := time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)
	_, _, lic := seedBillingLicence(t, s, billingSeed{
		periodEnd: now.Add(-time.Hour), cancelAtPeriodEnd: true,
	})

	changed, err := s.CancelBillingSubscriptionAtPeriodEnd(ctx, lic.ID, now)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var status string
	var canceledAt *time.Time
	if err := s.DB.NewRaw("SELECT status, canceled_at FROM subscriptions WHERE license_id = ?",
		lic.ID).Scan(ctx, &status, &canceledAt); err != nil {
		t.Fatal(err)
	}
	if status != model.StatusCanceled || canceledAt == nil {
		t.Fatalf("subscription = %s canceled_at=%v", status, canceledAt)
	}
	// The licence is left to the expiry sweep, exactly as a licence that
	// was never on a subscription would be.
	got, _ := s.FindLicenseByID(ctx, lic.ID)
	if got.Status != model.StatusActive {
		t.Fatalf("billing touched the licence: %s", got.Status)
	}
}

// renewalOrderFor is a pending renewal order for one period.
func renewalOrderFor(t *testing.T, s *store.Store, prod *model.Product, plan *model.Plan, lic *model.License, periodEnd time.Time) *model.PaymentOrder {
	t.Helper()
	order, err := s.CreatePaymentOrder(context.Background(), store.CreateOrderInput{
		ProductID: prod.ID, PlanID: plan.ID, LicenseID: lic.ID, Email: lic.Email, Kind: "renewal",
		Quantity: 1, SubtotalMinor: 1190000, TotalMinor: 1190000, Currency: "VND",
		Provider: "sepay", PeriodEnd: &periodEnd, ExpiresAt: periodEnd.Add(7 * 24 * time.Hour),
		Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return order
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store -run 'TestMarkBillingPastDue|TestPastDueSkips|TestSuspendBilling|TestExpireBilling|TestCancelBilling' -v`

Expected: FAIL — methods undefined.

- [ ] **Step 3: Write the four writes**

```go
// MarkBillingPastDue moves a licence whose paid period has ended into
// past_due and brings its subscription with it, in one transaction. The
// UPDATE repeats the conditions the scan read, so a licence renewed or
// paid since the scan is left alone and reported as unchanged.
func (s *Store) MarkBillingPastDue(ctx context.Context, licenseID string, now time.Time) (bool, error) {
	var changed bool
	err := s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewRaw(`UPDATE licenses
			SET status = 'past_due', past_due_at = COALESCE(past_due_at, ?), updated_at = ?
			WHERE id = ? AND status IN ('active', 'trialing')
			  AND valid_until IS NOT NULL AND valid_until <= ?`,
			now, now, licenseID, now).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		changed = true
		_, err = tx.NewRaw(`UPDATE subscriptions
			SET status = 'past_due', updated_at = ?
			WHERE license_id = ? AND status IN ('active', 'trialing')`, now, licenseID).Exec(ctx)
		return err
	})
	return changed, err
}

// SuspendBillingLicense ends dunning for a licence still unpaid after its
// grace days. The open renewal order is expired in the same transaction:
// a QR that would no longer be honoured must not stay payable. cutoff is
// now minus the plan's grace days, so the caller decides the boundary and
// the write applies it to the row as it is now.
func (s *Store) SuspendBillingLicense(ctx context.Context, licenseID string, cutoff, now time.Time) (bool, error) {
	var changed bool
	err := s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewRaw(`UPDATE licenses
			SET status = 'suspended', suspended_at = ?, suspended_by = ?, updated_at = ?
			WHERE id = ? AND status = 'past_due'
			  AND valid_until IS NOT NULL AND valid_until <= ?`,
			now, BillingSuspendedBy, now, licenseID, cutoff).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		changed = true
		if _, err := tx.NewRaw(`UPDATE payment_orders
			SET status = 'expired', updated_at = ?
			WHERE license_id = ? AND kind = 'renewal' AND status = 'pending'`,
			now, licenseID).Exec(ctx); err != nil {
			return err
		}
		_, err = tx.NewRaw(`UPDATE subscriptions SET status = 'past_due', updated_at = ?
			WHERE license_id = ? AND status IN ('active', 'past_due')`, now, licenseID).Exec(ctx)
		return err
	})
	return changed, err
}

// ExpireBillingSuspendedLicense ends a licence the engine suspended once
// its dunning tail has run out. Only BillingSuspendedBy rows match: an
// operator's suspension stays until the operator lifts it.
func (s *Store) ExpireBillingSuspendedLicense(ctx context.Context, licenseID string, cutoff, now time.Time) (bool, error) {
	res, err := s.DB.NewRaw(`UPDATE licenses
		SET status = 'expired', updated_at = ?
		WHERE id = ? AND status = 'suspended' AND suspended_by = ?
		  AND suspended_at IS NOT NULL AND suspended_at <= ?`,
		now, licenseID, BillingSuspendedBy, cutoff).Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("expire suspended licence: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CancelBillingSubscriptionAtPeriodEnd closes a subscription whose owner
// turned renewal off, once the period they paid for has ended. The licence
// is deliberately untouched: a licence that stops paying ends through the
// existing expiry rules, and its grace period is the customer's, not the
// engine's.
func (s *Store) CancelBillingSubscriptionAtPeriodEnd(ctx context.Context, licenseID string, now time.Time) (bool, error) {
	res, err := s.DB.NewRaw(`UPDATE subscriptions
		SET status = 'canceled', canceled_at = COALESCE(current_period_end, ?), updated_at = ?
		WHERE license_id = ? AND cancel_at_period_end
		  AND status IN ('active', 'past_due')
		  AND current_period_end IS NOT NULL AND current_period_end <= ?`,
		now, now, licenseID, now).Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("cancel subscription at period end: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store -run 'Billing|PastDue|Suspend|ExpireBilling' -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go build ./... && go vet ./internal/store
git add internal/store/billing.go internal/store/billing_test.go
git commit -m "store: conditional billing lifecycle writes (phase 1b module A)"
```

<!-- APPEND -->
> **Status: landed in `5684d6f`.** Run this task as a verification checklist: steps 1–8 are the tests that must pass and the API the later tasks assume (`checkout.SettlementCurrency`, `ErrPlanNotSellable`, `ErrPriceNotConfigured`, the new `CreateOrderInput` fields, `store.CreateOrderInput.PeriodStart`). Where the landing differs from the code below, the tests decide; port the difference into the landing rather than reverting it.

### Task 3: Proration arithmetic and the renewal/upgrade order shape

**Files:**
- Create: `internal/payment/checkout/proration.go`
- Create: `internal/payment/checkout/proration_test.go`
- Modify: `internal/payment/checkout/service.go`
- Modify: `internal/store/payments.go` (one field)
- Test: `internal/payment/checkout/billing_test.go`

**Interfaces:**
- Consumes: `domain.ComputeTotals`, `store.CreatePaymentOrder`, `model.Plan.GraceDays`.
- Produces:
  - `func checkout.ProrateMinor(priceMinor int64, periodStart, periodEnd, at time.Time) int64`
  - `func checkout.UpgradeAmount(proratedNewMinor, creditMinor int64) int64`
  - `checkout.CreateOrderInput` with `Kind`, `LicenseID`, `PeriodStart`, `PeriodEnd`, `SubtotalMinor`, `CreditMinor`
  - `store.CreateOrderInput.PeriodStart`

One implementation of proration, in the package that already owns pricing and VAT. Billing quotes with it; checkout prices an upgrade order with it. Half-up integer rounding, no floats, no overflow: the division happens before the multiplication.

- [ ] **Step 1: Write the failing proration tests**

```go
// internal/payment/checkout/proration_test.go
package checkout_test

import (
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/payment/checkout"
)

// A 30-day period at 1,190,000 VND. Every case is exact integer maths,
// so the expectations are the arithmetic, not a tolerance.
func TestProrateMinor(t *testing.T) {
	start := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(30 * 24 * time.Hour)
	cases := []struct {
		name string
		at   time.Time
		want int64
	}{
		{"before the period starts", start.Add(-time.Hour), 0},
		{"at the start", start, 0},
		{"one day in", start.Add(24 * time.Hour), 1_190_000 - 1_110_667},
		{"half way", start.Add(15 * 24 * time.Hour), 595_000},
		{"one day left", end.Add(-24 * time.Hour), 39_667},
		{"at the end", end, 1_190_000},
		{"after the end", end.Add(24 * time.Hour), 1_190_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkout.ProrateMinor(1_190_000, start, end, tc.at); got != tc.want {
				t.Fatalf("ProrateMinor = %d, want %d", got, tc.want)
			}
		})
	}
}

// Properties that must hold for every price and instant: the credit never
// exceeds the price, never goes backwards as time advances, and a year of
// VND at the largest sane price does not overflow.
func TestProrateMinorProperties(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, period := range []time.Duration{30 * 24 * time.Hour, 365 * 24 * time.Hour} {
		end := start.Add(period)
		for _, price := range []int64{1, 9_999, 1_190_000, 500_000_000, 4_000_000_000_000} {
			prev := int64(-1)
			for h := 0; h <= int(period/time.Hour); h += 7 {
				at := start.Add(time.Duration(h) * time.Hour)
				got := checkout.ProrateMinor(price, start, end, at)
				if got < 0 || got > price {
					t.Fatalf("price %d at +%dh = %d: out of range", price, h, got)
				}
				if prev >= 0 && got < prev {
					t.Fatalf("price %d at +%dh = %d went backwards from %d", price, h, got, prev)
				}
				prev = got
			}
			if full := checkout.ProrateMinor(price, start, end, end); full != price {
				t.Fatalf("price %d at the end = %d", price, full)
			}
			if zero := checkout.ProrateMinor(price, start, end, start); zero != 0 {
				t.Fatalf("price %d at the start = %d", price, zero)
			}
		}
	}
}

func TestUpgradeAmount(t *testing.T) {
	cases := []struct{ proratedNew, credit, want int64 }{
		{1_190_000, 300_000, 890_000},
		{1_190_000, 1_190_000, 0},
		{1_190_000, 2_000_000, 0},
		{0, 0, 0},
	}
	for _, tc := range cases {
		if got := checkout.UpgradeAmount(tc.proratedNew, tc.credit); got != tc.want {
			t.Fatalf("UpgradeAmount(%d, %d) = %d, want %d", tc.proratedNew, tc.credit, got, tc.want)
		}
	}
}
```

`1_190_000 / 30 = 39,666.67`; one day of 30 is 39,667 (half-up), so "one day left" is 39,667 and "one day in" is 1,190,000 − 39,667 = 1,150,333. Fix the first expectation to `1_150_333`.

- [ ] **Step 2: Run them to fail**

Run: `go test ./internal/payment/checkout -run 'TestProrate|TestUpgradeAmount' -v`
Expected: FAIL — `undefined: checkout.ProrateMinor`.

- [ ] **Step 3: Write `proration.go`**

```go
// internal/payment/checkout/proration.go
package checkout

import "time"

// ProrateMinor prices the elapsed part of a billing period in integer
// minor units:
//
//	round(price * elapsed / period)
//
// elapsed is clamped to [0, period]: a period that has not begun is worth
// nothing and one that has ended is worth the whole price. Rounding is
// half-up, the rule domain.ComputeTotals uses, so an invoice that adds a
// credit and a charge never drifts by a unit.
//
// The division happens before the multiplication (price/period, then the
// remainder), so no intermediate product can overflow int64 even for a
// yearly price of four billion đồng.
func ProrateMinor(priceMinor int64, periodStart, periodEnd, at time.Time) int64 {
	if priceMinor <= 0 || !periodEnd.After(periodStart) {
		return 0
	}
	period := int64(periodEnd.Sub(periodStart) / time.Second)
	if period <= 0 {
		return 0
	}
	elapsed := int64(at.Sub(periodStart) / time.Second)
	switch {
	case elapsed <= 0:
		return 0
	case elapsed >= period:
		return priceMinor
	}
	whole, rem := priceMinor/period, priceMinor%period
	return whole*elapsed + (rem*elapsed+period/2)/period
}

// UpgradeAmount is what a plan change costs today: the prorated price of
// the new plan for the rest of the period, less the unused-time credit of
// the plan being left. It never goes below zero — a change that the credit
// fully covers is a downgrade, and phase 1b refunds nothing.
func UpgradeAmount(proratedNewMinor, creditMinor int64) int64 {
	if creditMinor < 0 {
		creditMinor = 0
	}
	if amount := proratedNewMinor - creditMinor; amount > 0 {
		return amount
	}
	return 0
}
```

- [ ] **Step 4: Run them to pass**

Run: `go test ./internal/payment/checkout -run 'TestProrate|TestUpgradeAmount' -v`
Expected: PASS.

- [ ] **Step 5: Write the failing order-shape tests**

```go
// internal/payment/checkout/billing_test.go
package checkout_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/payment/checkout"
	"github.com/tabloy/keygate/internal/payment/provider/fake"
)

// A renewal order is dated for the period it buys, not for the click, so
// the extension settlement applies is exactly one interval and the order
// stays payable through the grace period.
func TestCreateRenewalOrder(t *testing.T) {
	f := newFixture(t, true)
	f.price("sepay", 1_190_000)
	// 7 grace days: the last instant a payment can cure the period.
	if _, err := f.s.DB.NewRaw(`UPDATE plans SET license_type = 'subscription', grace_days = 7,
		billing_interval = 'month' WHERE id = ?`, f.plan.ID).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	lic := seedSubscriptionLicence(t, f, "sepay")
	fp := fake.New("sepay")
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fp).providerFactory()}
	start := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(30 * 24 * time.Hour)

	res, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: lic.Email, Provider: "sepay", Quantity: 1,
		Kind: "renewal", LicenseID: lic.ID, PeriodStart: &start, PeriodEnd: &end,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Order.Kind != "renewal" || res.Order.LicenseID != lic.ID {
		t.Fatalf("order = %+v", res.Order)
	}
	if res.Order.PeriodStart == nil || !res.Order.PeriodStart.Equal(start) ||
		res.Order.PeriodEnd == nil || !res.Order.PeriodEnd.Equal(end) {
		t.Fatalf("order period = %v → %v", res.Order.PeriodStart, res.Order.PeriodEnd)
	}
	if res.Order.TotalMinor != 1_190_000 || res.Order.DiscountMinor != 0 {
		t.Fatalf("totals = %+v", res.Order)
	}
	// period_end + 7 grace days.
	if want := end.Add(7 * 24 * time.Hour); !res.Order.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %v, want %v", res.Order.ExpiresAt, want)
	}
	if res.Attempt == nil || res.Attempt.AmountMinor != 1_190_000 {
		t.Fatalf("attempt = %+v", res.Attempt)
	}
}

// An upgrade order charges the prorated new price less the credit, and
// records the credit as the order's discount so the receipt adds up.
func TestCreateUpgradeOrderChargesProratedDifference(t *testing.T) {
	f := newFixture(t, true)
	f.price("sepay", 2_000_000)
	fp := fake.New("sepay")
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fp).providerFactory()}
	lic := seedSubscriptionLicence(t, f, "sepay")
	start := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(30 * 24 * time.Hour)

	res, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: lic.Email, Provider: "sepay", Quantity: 1,
		Kind: "upgrade", LicenseID: lic.ID, PeriodStart: &start, PeriodEnd: &end,
		SubtotalMinor: 1_000_000, CreditMinor: 400_000,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Order.Kind != "upgrade" {
		t.Fatalf("kind = %s", res.Order.Kind)
	}
	if res.Order.SubtotalMinor != 1_000_000 || res.Order.DiscountMinor != 400_000 ||
		res.Order.TotalMinor != 600_000 {
		t.Fatalf("totals = %+v", res.Order)
	}
	if fp.Calls[0].AmountMinor != 600_000 {
		t.Fatalf("provider amount = %d", fp.Calls[0].AmountMinor)
	}
}

// A period that ended before the order is written cannot be bought: the
// instruction would be born dead.
func TestCreateBillingOrderRefusesEndedPeriod(t *testing.T) {
	f := newFixture(t, true)
	f.price("sepay", 1_190_000)
	fp := fake.New("sepay")
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fp).providerFactory()}
	lic := seedSubscriptionLicence(t, f, "sepay")
	start := time.Now().Add(-60 * 24 * time.Hour)
	end := time.Now().Add(-30 * 24 * time.Hour)

	_, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: lic.Email, Provider: "sepay", Quantity: 1,
		Kind: "renewal", LicenseID: lic.ID, PeriodStart: &start, PeriodEnd: &end,
	})
	if !errors.Is(err, checkout.ErrPlanNotSellable) {
		t.Fatalf("err = %v, want ErrPlanNotSellable", err)
	}
	if len(fp.Calls) != 0 {
		t.Fatalf("provider was asked for an instruction: %+v", fp.Calls)
	}
}

// A renewal never resumes an unrelated open order: its idempotency is the
// partial unique index on (license_id, period_end), not the buyer's cart.
func TestCreateRenewalTwiceHitsTheUniqueIndex(t *testing.T) {
	f := newFixture(t, true)
	f.price("sepay", 1_190_000)
	fp := fake.New("sepay")
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fp).providerFactory()}
	lic := seedSubscriptionLicence(t, f, "sepay")
	start := time.Now().Add(24 * time.Hour)
	end := start.Add(30 * 24 * time.Hour)
	in := checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: lic.Email, Provider: "sepay", Quantity: 1,
		Kind: "renewal", LicenseID: lic.ID, PeriodStart: &start, PeriodEnd: &end,
	}
	if _, err := svc.CreateOrder(f.ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateOrder(f.ctx, in); err == nil {
		t.Fatal("second renewal order for the same period was created")
	}
}

// seedSubscriptionLicence makes a licence on the fixture's plan with a
// subscription on the given rail.
func seedSubscriptionLicence(t *testing.T, f *fixture, providerName string) *model.License {
	t.Helper()
	ctx := context.Background()
	lic := &model.License{
		ProductID: f.product.ID, PlanID: f.plan.ID, Email: f.email("sub"),
		LicenseKey: "KEY-SUB-" + f.suffix, Status: model.StatusActive, Quantity: 1,
	}
	if err := f.s.CreateLicenseWithSubscription(ctx, lic, f.plan); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.NewRaw(`UPDATE subscriptions SET payment_provider = ?, external_id = ?
		WHERE license_id = ?`, providerName, "sub-"+f.suffix, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	return lic
}
```

Add `"github.com/tabloy/keygate/internal/model"` to the imports.

- [ ] **Step 6: Run them to fail**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/payment/checkout -run 'TestCreateRenewal|TestCreateUpgrade|TestCreateBilling' -v`
Expected: FAIL — the order is written with `kind = "new"` and no period.

- [ ] **Step 7: Extend the checkout service**

`internal/store/payments.go` — add the field to `CreateOrderInput` and assign it:

```go
	// PeriodStart and PeriodEnd are the billing period a renewal or
	// upgrade order buys. Nil for kind "new".
	PeriodStart      *time.Time
	PeriodEnd        *time.Time
```
and inside `CreatePaymentOrder`'s insert, next to `PeriodEnd: in.PeriodEnd`:
```go
		PeriodStart: in.PeriodStart,
```
(If the insert builds the model literally, add `PeriodStart: in.PeriodStart` to that literal.)

`internal/payment/checkout/service.go` — the currency becomes public, because billing has to resolve prices in the same currency this service charges in:

```go
// SettlementCurrency is the currency the Vietnamese rails settle in. It is
// a constant, not a setting: the providers price in VND only, and a plan
// price in another currency is a configuration error, not a currency
// conversion.
const SettlementCurrency = "VND"
```

Rename every use of the private `settlementCurrency` in that file (four) to `SettlementCurrency`; delete the old constant.

Then the input:

```go
type CreateOrderInput struct {
	PlanID         string
	Email          string
	Provider       string
	Quantity       int
	Buyer          provider.Buyer
	Metadata       map[string]any
	CreatedByKeyID string

	// Kind is what the order buys: "" and "new" sell a licence,
	// "renewal" extends one and "upgrade" changes one's plan. Only the
	// billing engine passes a kind other than "".
	Kind string
	// LicenseID is the licence a renewal or upgrade belongs to.
	LicenseID string
	// PeriodStart and PeriodEnd are the billing period the order buys;
	// both are required for renewal and upgrade orders.
	PeriodStart *time.Time
	PeriodEnd   *time.Time
	// SubtotalMinor replaces the plan price as the order's subtotal. It is
	// only used for upgrades, where it is the prorated price of the new
	// plan for the rest of the period.
	SubtotalMinor int64
	// CreditMinor is the unused-time credit on an upgrade, recorded as the
	// order's discount.
	CreditMinor int64
}
```

`CreateOrder` — after the quantity and email checks, add the kind gate:

```go
	kind := in.Kind
	if kind == "" {
		kind = "new"
	}
	switch kind {
	case "new":
	case "renewal", "upgrade":
		if in.LicenseID == "" || in.PeriodStart == nil || in.PeriodEnd == nil {
			return nil, fmt.Errorf("%w: a %s order needs a licence and a period", ErrPlanNotSellable, kind)
		}
		if !in.PeriodEnd.After(*in.PeriodStart) {
			return nil, fmt.Errorf("%w: the period ends before it starts", ErrPlanNotSellable)
		}
		if in.Quantity != 1 {
			return nil, fmt.Errorf("%w: a %s order is for one licence", ErrPlanNotSellable, kind)
		}
	default:
		return nil, fmt.Errorf("%w: unknown order kind %q", ErrPlanNotSellable, kind)
	}
```

then guard the resume path:

```go
	order, latest, err := (*model.PaymentOrder)(nil), (*model.PaymentAttempt)(nil), error(nil)
	if kind == "new" {
		order, latest, err = s.resumableOrder(ctx, plan, email, providerName, in.Quantity, price.ID)
		if err != nil {
			return nil, err
		}
	}
```
(`if order != nil && latest != nil && …` stays as it is.)

pass the kind into `createOrder` (`s.createOrder(ctx, plan, product, price, email, providerName, kind, in)`), and in `createOrder`:

```go
func (s *Service) createOrder(
	ctx context.Context, plan *model.Plan, product *model.Product,
	price *model.PlanPrice, email, providerName, kind string, in CreateOrderInput,
) (*model.PaymentOrder, error) {
	subtotal, err := orderSubtotal(price.AmountMinor, in.Quantity)
	if err != nil {
		return nil, err
	}
	discount := int64(0)
	if kind == "upgrade" {
		if in.SubtotalMinor <= 0 {
			return nil, fmt.Errorf("%w: an upgrade needs a positive prorated price", ErrPlanNotSellable)
		}
		subtotal, discount = in.SubtotalMinor, in.CreditMinor
	}
	taxRate := product.TaxRateBP
	priceIncludesTax := true
	if product.PriceIncludesTax != nil {
		priceIncludesTax = *product.PriceIncludesTax
	}
	totals := domain.ComputeTotals(subtotal, discount, taxRate, priceIncludesTax)
	...
	expiresAt := time.Now().Add(s.orderTTL(ctx))
	if kind != "new" {
		// The order lives exactly as long as paying it can still cure
		// the period: through the plan's grace days.
		expiresAt = in.PeriodEnd.Add(time.Duration(plan.GraceDays) * 24 * time.Hour)
		if !expiresAt.After(time.Now()) {
			return nil, fmt.Errorf("%w: the billing period ended before the order could be paid", ErrPlanNotSellable)
		}
	}
	return s.Store.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: product.ID, PlanID: plan.ID, PriceID: price.ID, Email: email, Kind: kind,
		LicenseID: in.LicenseID, PeriodStart: in.PeriodStart, PeriodEnd: in.PeriodEnd,
		Quantity: in.Quantity, SubtotalMinor: subtotal, DiscountMinor: totals.DiscountMinor,
		TaxMinor: totals.TaxMinor, TotalMinor: totals.TotalMinor,
		Currency: settlementCurrency, CurrencyExponent: domain.CurrencyExponent(settlementCurrency),
		TaxRateBP: taxRate, Provider: providerName, ExpiresAt: expiresAt,
		Buyer: buyerJSON, Metadata: metaJSON, CreatedByKeyID: in.CreatedByKeyID,
	})
}
```

`total == 0` is *not* refused here: an upgrade whose credit covers the price is a downgrade and billing refuses it before calling (Task 8), and a zero-priced plan is refused by billing's price check (Task 4). Checkout stays a pure writer.

- [ ] **Step 8: Run the whole checkout package to pass**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/payment/checkout -v`
Expected: PASS, including the 1a tests (they pass no kind and must be untouched).

- [ ] **Step 9: Commit**

```bash
go build ./... && go vet ./internal/payment/... ./internal/store
git add internal/payment/checkout internal/store/payments.go
git commit -m "checkout: renewal and upgrade orders with integer proration (phase 1b module A)"
```

---

### Task 4: The engine — dependency seam, hourly loop, renewal order

**Files:**
- Create: `internal/billing/service.go`
- Create: `internal/billing/main_test.go`
- Create: `internal/billing/fixtures_test.go`
- Test: `internal/billing/service_test.go`

**Interfaces:**
- Consumes: `store.ListBillingSubscriptions`, `store.GetBillingSubscription` (T1), `checkout.CreateOrderInput` (T3), `service.WebhookService.Dispatch`, `store.ResolvePlanPrice`, `internal/license.KeyPrefix`.
- Produces:
  - `billing.Deps`, `billing.New(Deps) *Service`, `(*Service).Run(ctx)`, `(*Service).RunOnce(ctx) error`
  - `billing.OrderCreator`, `billing.EventEmitter`, `billing.ReminderSender` (the interfaces later tasks and `main.go` wire)
  - `billing.IntervalDuration(interval string) time.Duration` and `billing.DefaultInterval`
  - `s.createRenewalOrder`, `s.emitRenewalDue`
  - `Service.Interval time.Duration` (test/wiring hook, `DefaultInterval` when zero)

- [ ] **Step 1: Write the failing tests**

```go
// internal/billing/main_test.go
package billing_test

import (
	"os"
	"testing"

	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m, "../../db/migrations")) }
```

```go
// internal/billing/fixtures_test.go
package billing_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tabloy/keygate/internal/billing"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/checkout"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/provider/fake"
	"github.com/tabloy/keygate/internal/payment/providercfg"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

// harness is one engine wired to a real database, a real checkout service
// with a fake provider, and recorders for mail and events.
type harness struct {
	t       *testing.T
	s       *store.Store
	ctx     context.Context
	svc     *billing.Service
	prov    *fake.Provider
	mail    *fakeMail
	events  *fakeEvents
	product *model.Product
	plan    *model.Plan
	suffix  string
}

type sentMail struct {
	Subject string
	Body    string
	In      service.RenewalMail
}

type fakeMail struct {
	enabled bool
	notify  map[string]bool
	sent    []sentMail
}

func (m *fakeMail) Configured() (bool, error) { return m.enabled, nil }

func (m *fakeMail) NotifyEnabled(kind string) bool {
	if m.notify == nil {
		return true
	}
	v, ok := m.notify[kind]
	return !ok || v
}

func (m *fakeMail) RenderRenewalMail(in service.RenewalMail) (string, string) {
	m.sent = append(m.sent, sentMail{Subject: "renewal " + in.Kind, Body: in.Kind + "|" + in.DueDate, In: in})
	return "renewal " + in.Kind, in.Kind
}

type dispatched struct {
	Event string
	Data  map[string]any
}

type fakeEvents struct{ got []dispatched }

func (f *fakeEvents) Dispatch(_ context.Context, _ string, event string, data map[string]any) {
	f.got = append(f.got, dispatched{Event: event, Data: data})
}

// newHarness seeds a product whose plan is a monthly subscription priced
// at 1,190,000 VND for sepay, then wires the engine.
func newHarness(t *testing.T, opts seedOpts) *harness {
	t.Helper()
	s := testdb.Store(t)
	ctx := context.Background()
	suffix := strings.ToLower(uuid.NewString()[:13])
	inclusive := true
	prod := &model.Product{
		Name: "Bill Co", Slug: "bill-co-" + suffix, Type: "desktop",
		TaxRateBP: 1000, PriceIncludesTax: &inclusive,
	}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{
		ProductID: prod.ID, Name: "Monthly", Slug: "bill-monthly-" + suffix,
		LicenseType: opts.licenseType, BillingInterval: "month", LicenseModel: "standard",
		MaxActivations: 3, Active: true, GraceDays: opts.graceDays, RenewalDays: opts.renewalDays,
	}
	if plan.LicenseType == "" {
		plan.LicenseType = "subscription"
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if !opts.noPrice {
		if _, err := s.UpsertPlanPrice(ctx, plan.ID, store.PlanPriceInput{
			Provider: opts.provider, Currency: "VND", AmountMinor: opts.priceMinor,
		}); err != nil {
			t.Fatal(err)
		}
	}
	h := &harness{
		t: t, s: s, ctx: ctx, product: prod, plan: plan, suffix: suffix,
		prov: fake.New(opts.provider), mail: &fakeMail{enabled: true}, events: &fakeEvents{},
	}
	h.svc = billing.New(billing.Deps{
		Store: s,
		Checkout: &checkout.Service{Store: s, Providers: func(_ context.Context, _, _ string) (provider.Provider, *providercfg.ResolvedConfig, error) {
			return h.prov, &providercfg.ResolvedConfig{Provider: opts.provider}, nil
		}},
		Email:   h.mail,
		Webhook: h.events,
		Logger:  slog.New(slog.NewTextHandler(discard{}, nil)),
		BaseURL: "https://licenses.test",
	})
	t.Cleanup(func() {
		ctx := context.Background()
		// Order matters: every child row before the row it points at.
		// Every plan and price of the product goes, not just the seeded
		// one, because later tasks add plans to the same product.
		for _, q := range []string{
			"DELETE FROM email_queue WHERE notification_id IN (SELECT id FROM notifications WHERE license_id IN (SELECT id FROM licenses WHERE product_id = ?))",
			"DELETE FROM license_upgrades WHERE license_id IN (SELECT id FROM licenses WHERE product_id = ?)",
			"DELETE FROM license_renewals WHERE license_id IN (SELECT id FROM licenses WHERE product_id = ?)",
			"DELETE FROM payment_transactions WHERE order_id IN (SELECT id FROM payment_orders WHERE product_id = ?)",
			"DELETE FROM payment_attempts WHERE order_id IN (SELECT id FROM payment_orders WHERE product_id = ?)",
			"DELETE FROM payment_orders WHERE product_id = ?",
			"DELETE FROM notifications WHERE license_id IN (SELECT id FROM licenses WHERE product_id = ?)",
			"DELETE FROM subscriptions WHERE license_id IN (SELECT id FROM licenses WHERE product_id = ?)",
			"DELETE FROM licenses WHERE product_id = ?",
			"DELETE FROM plan_prices WHERE plan_id IN (SELECT id FROM plans WHERE product_id = ?)",
			"DELETE FROM plans WHERE product_id = ?",
			"DELETE FROM products WHERE id = ?",
		} {
			_, _ = s.DB.NewRaw(q, prod.ID).Exec(ctx)
		}
	})
	return h
}

type seedOpts struct {
	licenseType string
	provider    string
	priceMinor  int64
	graceDays   int
	renewalDays int
	noPrice     bool
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// licence creates a licence on the harness plan whose paid period ends at
// periodEnd, with a subscription on the harness provider.
func (h *harness) licence(periodEnd time.Time) *model.License {
	h.t.Helper()
	ctx := context.Background()
	lic := &model.License{
		ProductID: h.product.ID, PlanID: h.plan.ID, Email: "cust-" + uuid.NewString()[:8] + "@example.com",
		LicenseKey: "KEY-" + strings.ToUpper(uuid.NewString()[:13]), Status: model.StatusActive, Quantity: 1,
	}
	if err := h.s.CreateLicenseWithSubscription(ctx, lic, h.plan); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.s.DB.NewRaw(`UPDATE licenses SET valid_until = ? WHERE id = ?`, periodEnd, lic.ID).Exec(ctx); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.s.DB.NewRaw(`UPDATE subscriptions SET payment_provider = ?, external_id = ?,
		status = 'active', current_period_start = ?, current_period_end = ? WHERE license_id = ?`,
		h.prov.Name(), "sub-"+lic.ID[:8], periodEnd.Add(-30*24*time.Hour), periodEnd, lic.ID).Exec(ctx); err != nil {
		h.t.Fatal(err)
	}
	return lic
}

// orders returns the licence's orders, newest first.
func (h *harness) orders(licenseID string) []*model.PaymentOrder {
	h.t.Helper()
	var rows []*model.PaymentOrder
	if err := h.s.DB.NewSelect().Model(&rows).Where("license_id = ?", licenseID).
		Order("created_at DESC").Scan(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	return rows
}

// metadata decodes an order's metadata JSONB.
func metadata(t *testing.T, o *model.PaymentOrder) map[string]any {
	t.Helper()
	out := map[string]any{}
	if len(o.Metadata) > 0 {
		if err := json.Unmarshal(o.Metadata, &out); err != nil {
			t.Fatal(err)
		}
	}
	return out
}
```

```go
// internal/billing/service_test.go
package billing_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// A licence inside the lead window gets one renewal order for the next
// period and one license.renewal_due event carrying the spec's payload.
func TestRunOnceCreatesRenewalOrder(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	periodEnd := time.Now().UTC().Add(5 * 24 * time.Hour).Truncate(time.Second)
	lic := h.licence(periodEnd)

	if err := h.svc.RunOnce(h.ctx); err != nil {
		t.Fatal(err)
	}
	orders := h.orders(lic.ID)
	if len(orders) != 1 {
		t.Fatalf("%d orders, want 1", len(orders))
	}
	o := orders[0]
	if o.Kind != "renewal" || o.Status != "pending" || o.TotalMinor != 1_190_000 {
		t.Fatalf("order = %+v", o)
	}
	if o.PeriodStart == nil || !o.PeriodStart.Equal(periodEnd) ||
		o.PeriodEnd == nil || !o.PeriodEnd.Equal(periodEnd.Add(30*24*time.Hour)) {
		t.Fatalf("period = %v → %v", o.PeriodStart, o.PeriodEnd)
	}
	if want := periodEnd.Add(30*24*time.Hour + 7*24*time.Hour); !o.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %v, want %v", o.ExpiresAt, want)
	}
	if o.Metadata == nil {
		t.Fatal("metadata is empty")
	}
	if got := metadata(t, o)["billing_period_end"]; got != o.PeriodEnd.Format(time.RFC3339) {
		t.Fatalf("metadata billing_period_end = %v", got)
	}
	// Exactly one attempt, payable, for the order's total.
	if h.prov.Calls == nil || len(h.prov.Calls) != 1 || h.prov.Calls[0].AmountMinor != 1_190_000 {
		t.Fatalf("provider calls = %+v", h.prov.Calls)
	}
	// The event carries license{id,key_prefix,valid_until} + order{...}.
	if len(h.events.got) != 1 || h.events.got[0].Event != model.EventLicenseRenewalDue {
		t.Fatalf("events = %+v", h.events.got)
	}
	data := h.events.got[0].Data
	licData, _ := data["license"].(map[string]any)
	if licData["id"] != lic.ID || licData["key_prefix"] != "KEY" {
		t.Fatalf("license payload = %+v", licData)
	}
	orderData, _ := data["order"].(map[string]any)
	if orderData["code"] != o.Code || orderData["total"] != int64(1_190_000) {
		t.Fatalf("order payload = %+v", orderData)
	}
}

// The second run of the hour must find the order and add nothing: this is
// the idempotency the partial unique index exists for.
func TestRunOnceIsIdempotent(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	lic := h.licence(time.Now().UTC().Add(3 * 24 * time.Hour))
	for i := 0; i < 3; i++ {
		if err := h.svc.RunOnce(h.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(h.orders(lic.ID)); n != 1 {
		t.Fatalf("%d orders after three runs", n)
	}
	if len(h.events.got) != 1 {
		t.Fatalf("%d events after three runs", len(h.events.got))
	}
}

// Two replicas running the same hour: one order, one event. The loser's
// unique violation is success, not an error.
func TestConcurrentRunsCreateOneOrder(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	lic := h.licence(time.Now().UTC().Add(2 * 24 * time.Hour))
	errc := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { errc <- h.svc.RunOnce(h.ctx) }()
	}
	for i := 0; i < 2; i++ {
		if err := <-errc; err != nil {
			t.Fatalf("concurrent run: %v", err)
		}
	}
	if n := len(h.orders(lic.ID)); n != 1 {
		t.Fatalf("%d orders from two concurrent runs", n)
	}
}

// A licence outside the lead window is left alone.
func TestRunOnceLeavesDistantRenewalsAlone(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	lic := h.licence(time.Now().UTC().Add(20 * 24 * time.Hour))
	if err := h.svc.RunOnce(h.ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(h.orders(lic.ID)); n != 0 {
		t.Fatalf("%d orders created 20 days out", n)
	}
}

// renewal_days is a perpetual maintenance term, not the subscription
// period: a subscription plan with renewal_days = 0 still renews monthly.
func TestRenewalIgnoresRenewalDays(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 500_000, renewalDays: 0})
	periodEnd := time.Now().UTC().Add(4 * 24 * time.Hour).Truncate(time.Second)
	lic := h.licence(periodEnd)
	if err := h.svc.RunOnce(h.ctx); err != nil {
		t.Fatal(err)
	}
	orders := h.orders(lic.ID)
	if len(orders) != 1 {
		t.Fatalf("%d orders", len(orders))
	}
	if got := orders[0].PeriodEnd.Sub(*orders[0].PeriodStart); got != 30*24*time.Hour {
		t.Fatalf("renewal period = %v", got)
	}
}

// A plan with no price cannot be sold: no order, no event, no error.
func TestRenewalWithoutPriceIsSkipped(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", noPrice: true})
	lic := h.licence(time.Now().UTC().Add(3 * 24 * time.Hour))
	if err := h.svc.RunOnce(h.ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := len(h.orders(lic.ID)); n != 0 {
		t.Fatalf("%d orders for an unpriced plan", n)
	}
	if len(h.events.got) != 0 {
		t.Fatalf("events = %+v", h.events.got)
	}
}

// A zero price is not a 0 VND QR for a provider to reject: the renewal is
// skipped and logged, and the licence follows the unpaid lifecycle.
func TestRenewalSkipsZeroPrice(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 0})
	lic := h.licence(time.Now().UTC().Add(3 * 24 * time.Hour))
	if err := h.svc.RunOnce(h.ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(h.orders(lic.ID)); n != 0 {
		t.Fatalf("%d orders for a zero-priced plan", n)
	}
	if len(h.prov.Calls) != 0 {
		t.Fatalf("provider called: %+v", h.prov.Calls)
	}
}

// The provider named by the licence wins even when the plan is also priced
// for the other rail: a customer does not silently change rails.
func TestRenewalKeepsTheLicenceProvider(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000})
	if _, err := h.s.UpsertPlanPrice(h.ctx, h.plan.ID, storePlanPrice("payos")); err != nil {
		t.Fatal(err)
	}
	lic := h.licence(time.Now().UTC().Add(2 * 24 * time.Hour))
	if err := h.svc.RunOnce(h.ctx); err != nil {
		t.Fatal(err)
	}
	orders := h.orders(lic.ID)
	if len(orders) != 1 || orders[0].Provider != "sepay" {
		t.Fatalf("orders = %+v", orders)
	}
}

// Run returns when the context is cancelled, so a shutdown is not held up
// by the hourly ticker.
func TestRunStopsOnContextCancel(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000})
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() { h.svc.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	_ = errors.Is
}
```

Add `storePlanPrice` as a tiny helper in the fixtures file:

```go
func storePlanPrice(providerName string) store.PlanPriceInput {
	return store.PlanPriceInput{Provider: providerName, Currency: "VND", AmountMinor: 1_190_000}
}
```

and `"github.com/tabloy/keygate/internal/store"`, `"context"` to `service_test.go`'s imports; drop the `_ = errors.Is` line and the `errors` import.

- [ ] **Step 2: Run them to fail**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/billing -run 'TestRunOnce|TestRunStops|TestRenewal|TestConcurrentRuns' -v`
Expected: FAIL — `no required module provides package internal/billing` / `undefined: billing.New`.

- [ ] **Step 3: Write the service**

```go
// Package billing runs the renewal cycle for the licences the Vietnamese
// rails bill: it opens a renewal order before the period ends, walks a
// reminder ladder, ends a licence that stops paying, and reopens it the
// moment a payment lands. It decides; the store writes; checkout prices.
package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/tabloy/keygate/internal/license"
	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/checkout"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
)

// DefaultInterval is how often Run sweeps. Hourly, like the expiry loop:
// every rung of the ladder is a day wide and renewal_due is the finest
// decision, so an hour is plenty and the scan is cheap.
const DefaultInterval = time.Hour

// RenewalLeadDaysSetting is how many days before a period ends the licence
// gets its renewal order. The default matches the first rung of the ladder;
// the order's instruction lives 24 h, so a lead shorter than a day would
// create orders nobody sees.
const (
	RenewalLeadDaysSetting  = "billing_renewal_lead_days"
	DefaultRenewalLeadDays  = 7
	minRenewalLeadDays      = 1
	maxRenewalLeadDays      = 90
	languageSetting         = "language"
)

// Deps is what the engine needs. Store, Checkout and Email are required in
// production; Webhook and Logger are optional.
type Deps struct {
	Store    *store.Store
	Checkout OrderCreator
	Email    ReminderSender
	Webhook  EventEmitter
	Logger   *slog.Logger
	// BaseURL is the public origin, used for the pay and portal links in
	// the reminder mail.
	BaseURL string
}

// OrderCreator is the part of checkout.Service the engine uses, so a test
// can count and shape orders without a provider.
type OrderCreator interface {
	CreateOrder(ctx context.Context, in checkout.CreateOrderInput) (*checkout.OrderResult, error)
}

// EventEmitter is service.WebhookService.Dispatch.
type EventEmitter interface {
	Dispatch(ctx context.Context, productID, event string, data map[string]any)
}

// ReminderSender is the part of service.EmailService the engine uses.
type ReminderSender interface {
	RenderRenewalMail(in service.RenewalMail) (subject, body string)
	NotifyEnabled(kind string) bool
	Configured() (bool, error)
}

// Service is the billing engine. It is safe for concurrent use: it holds no
// mutable state between runs.
type Service struct {
	store    *store.Store
	checkout OrderCreator
	email    ReminderSender
	webhook  EventEmitter
	log      *slog.Logger
	baseURL  string
	// Interval overrides DefaultInterval. Tests set it; main.go leaves it.
	Interval time.Duration
}

// New builds the engine. A nil Logger falls back to the process logger, so
// a misconfigured caller still logs rather than panics.
func New(d Deps) *Service {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store: d.Store, checkout: d.Checkout, email: d.Email,
		webhook: d.Webhook, log: log, baseURL: strings.TrimSuffix(d.BaseURL, "/"),
	}
}

// Run sweeps immediately, then every Interval until ctx is done. The first
// sweep is deliberate: a restart after downtime must not wait an hour for
// the orders that came due while it was down.
func (s *Service) Run(ctx context.Context) {
	if err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
		s.log.Error("billing: sweep failed", "error", err)
	}
	ticker := time.NewTicker(s.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.RunOnce(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				s.log.Error("billing: sweep failed", "error", err)
			}
		}
	}
}

func (s *Service) interval() time.Duration {
	if s.Interval > 0 {
		return s.Interval
	}
	return DefaultInterval
}

// RunOnce is one sweep. A licence that fails is logged and skipped so one
// bad row cannot stop the other thousand; the error return is for the scan
// itself and for cancellation.
func (s *Service) RunOnce(ctx context.Context) error {
	now := time.Now().UTC()
	lead := s.renewalLead(ctx)
	rows, err := s.store.ListBillingSubscriptions(ctx, now, lead)
	if err != nil {
		return err
	}
	locale := s.locale(ctx)
	for _, row := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.processRow(ctx, row, now, lead, locale); err != nil {
			s.log.Error("billing: licence failed",
				"license", row.LicenseID, "plan", row.PlanID, "error", err)
		}
	}
	return nil
}

// processRow is the engine's decision for one licence: the state it is in
// decides what it needs. This task implements the renewal order; Task 6
// adds the ladder, Task 7 the past-due lifecycle and Task 9 trial
// conversion, each by extending this function.
func (s *Service) processRow(ctx context.Context, row *store.BillingSubscription, now time.Time, lead time.Duration, locale string) error {
	if row.PlanLicenseType != "subscription" {
		// A trial is not renewed, it is converted or it ends: Task 9.
		return nil
	}
	if row.CancelAtPeriodEnd {
		// The customer turned renewal off. Task 7 closes the
		// subscription at period end; nothing is ordered or mailed.
		return nil
	}
	_, err := s.createRenewalOrder(ctx, row, now, lead)
	return err
}

// createRenewalOrder opens the renewal order and its instruction for the
// next period. A plan the operator has not priced (or priced at zero) is
// not an error: there is nothing to sell, so the run logs and moves on.
func (s *Service) createRenewalOrder(ctx context.Context, row *store.BillingSubscription, now time.Time, lead time.Duration) (*model.PaymentOrder, error) {
	// Too early, or an order is already open for this period: nothing to
	// do. Keeping both checks here means every caller — today's sweep and
	// any admin "run billing now" — gets the same answer.
	if row.CurrentPeriodEnd == nil || now.Add(lead).Before(*row.CurrentPeriodEnd) {
		return nil, nil
	}
	if row.PendingRenewalCode != "" {
		return nil, nil
	}
	price, err := s.store.ResolvePlanPrice(ctx, row.PlanID, row.PaymentProvider, checkout.SettlementCurrency, row.BillingInterval)
	if err != nil || price.AmountMinor <= 0 {
		s.log.Warn("billing: plan has no sellable price, renewal skipped",
			"license", row.LicenseID, "plan", row.PlanID, "provider", row.PaymentProvider, "error", err)
		return nil, nil
	}
	start := *row.CurrentPeriodEnd
	end := start.Add(IntervalDuration(row.BillingInterval))
	res, err := s.checkout.CreateOrder(ctx, checkout.CreateOrderInput{
		PlanID: row.PlanID, Email: row.Email, Provider: row.PaymentProvider, Quantity: 1,
		Kind: "renewal", LicenseID: row.LicenseID, PeriodStart: &start, PeriodEnd: &end,
		Metadata: map[string]any{
			"billing_period_start": start.Format(time.RFC3339),
			"billing_period_end":   end.Format(time.RFC3339),
			"billing_engine":       "phase-1b",
		},
	})
	if err != nil {
		// Another replica won the race for this period. The unique index
		// refused the second order, and the first one is the answer: read
		// it back rather than reporting a failure the customer cannot see.
		if store.IsRenewalOrderConflict(err) {
			if open, gerr := s.store.FindOpenRenewalOrderForPeriod(ctx, row.LicenseID, end); gerr == nil {
				return open, nil
			}
		}
		if errors.Is(err, checkout.ErrPlanNotSellable) || errors.Is(err, checkout.ErrPriceNotConfigured) {
			s.log.Warn("billing: plan is not sellable, renewal skipped",
				"license", row.LicenseID, "plan", row.PlanID, "error", err)
			return nil, nil
		}
		return nil, err
	}
	s.emitRenewalDue(ctx, row, res.Order)
	return res.Order, nil
}

// emitRenewalDue publishes license.renewal_due with the spec's payload. The
// event is emitted once per order because only the run that created the
// order reaches here.
func (s *Service) emitRenewalDue(ctx context.Context, row *store.BillingSubscription, order *model.PaymentOrder) {
	if s.webhook == nil || order == nil {
		return
	}
	prefix := ""
	if lic, err := s.store.FindLicenseByID(ctx, row.LicenseID); err == nil {
		prefix = license.KeyPrefix(s.store.DecryptLicenseKey(lic))
	}
	s.webhook.Dispatch(ctx, row.ProductID, model.EventLicenseRenewalDue, map[string]any{
		"license": map[string]any{
			"id":          row.LicenseID,
			"key_prefix":  prefix,
			"valid_until": row.ValidUntil,
		},
		"order": map[string]any{
			"code":       order.Code,
			"total":      order.TotalMinor,
			"expires_at": order.ExpiresAt,
		},
	})
}

// renewalLead is the configured lead window, clamped to something an
// operator cannot turn into a mail storm (1) or a dead order (90).
func (s *Service) renewalLead(ctx context.Context) time.Duration {
	days := DefaultRenewalLeadDays
	if raw, err := s.store.GetSetting(ctx, RenewalLeadDaysSetting); err == nil && strings.TrimSpace(raw) != "" {
		if n, convErr := strconv.Atoi(strings.TrimSpace(raw)); convErr == nil {
			days = n
		} else {
			s.log.Warn("billing: lead setting is not a number, using the default",
				"setting", RenewalLeadDaysSetting, "value", raw)
		}
	}
	if days < minRenewalLeadDays {
		days = minRenewalLeadDays
	}
	if days > maxRenewalLeadDays {
		days = maxRenewalLeadDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// locale is the install's customer language, used to pick the mail
// template. Anything but "vi" renders English, the platform default.
func (s *Service) locale(ctx context.Context) string {
	if raw, err := s.store.GetSetting(ctx, languageSetting); err == nil && strings.EqualFold(strings.TrimSpace(raw), "vi") {
		return "vi"
	}
	return ""
}

// IntervalDuration is the length of one billing interval. It is the same
// 30/365-day convention the settlement path uses, so the period of the
// order and the extension it buys agree to the second.
func IntervalDuration(interval string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(interval)) {
	case "year", "yearly", "annual", "annually":
		return 365 * 24 * time.Hour
	default:
		return 30 * 24 * time.Hour
	}
}
```

Both `checkout.SettlementCurrency` and `store.CreateOrderInput.PeriodStart` come from Task 3.

- [ ] **Step 4: Run the tests to pass**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/billing -v`
Expected: PASS. (`TestRunStopsOnContextCancel` returns in milliseconds.)

- [ ] **Step 5: Commit**

```bash
go build ./... && go vet ./internal/billing
git add internal/billing
git commit -m "billing: engine seam, hourly loop and renewal orders (phase 1b module A)"
```

<!-- APPEND -->
> **Status: landed in `f106a87`.** Verify with this task's tests (Vietnamese, custom override, pay link, `ValidateTemplate`). If the landed templates and `templateSample` do not cover `Amount`, `DueDate`, `DaysPastDue`, `SuspendDate` and `PayURL`, an admin's customised renewal template is rejected on save — that is the one thing worth checking first.

### Task 5: Renewal mail — one renderer, four rungs, English and Vietnamese

**Files:**
- Create: `internal/service/email_renewal.go`
- Modify: `internal/service/email.go` (`DefaultTemplates()`, `templateSample`)
- Test: `internal/service/email_renewal_test.go`

**Interfaces:**
- Consumes: `(*EmailService).renderEmail`, `(*EmailService).templateData`, `(*EmailService).getTemplate`, `renderTemplate`.
- Produces:
  - `service.RenewalMail` (the struct billing fills), `(*EmailService).RenderRenewalMail(in RenewalMail) (subject, body string)`
  - kind constants `service.RenewalMailReminder`, `RenewalMailDue`, `RenewalMailGrace`, `RenewalMailSuspended`
  - eight setting-editable templates: `<kind>` and `<kind>_vi`

The customer gets four mails, not seven: the 7/3/1 rungs share one "renews on X" template with the days interpolated, and the grace 1/3/7 rungs share one "overdue, works until X" template. Vietnamese is a separate set of defaults because a machine-translated HTML mail reads worse than a written one, and the install's `language` setting selects it.

- [ ] **Step 1: Write the failing test**

```go
// internal/service/email_renewal_test.go
package service

import (
	"strings"
	"testing"
)

func renewalSample(kind, locale string) RenewalMail {
	return RenewalMail{
		Kind: kind, Locale: locale,
		Product: "Acme App", Plan: "Pro", LicenseKey: "KEY-AAAA-BBBB-CCCC-DDDD-EEEE",
		Amount: "1.190.000 VND", DueDate: "2026-12-01", DaysLeft: 3,
		DaysPastDue: 2, SuspendDate: "2026-12-08",
		PayURL: "https://licenses.example.com/checkout/pay/KG7F3K2Q",
	}
}

// Every rung has a subject and a body, the amount and the date are in the
// body, and the pay link is a real anchor.
func TestRenderRenewalMail(t *testing.T) {
	s := newTemplateTestService()
	for _, kind := range []string{RenewalMailReminder, RenewalMailDue, RenewalMailGrace, RenewalMailSuspended} {
		subject, body := s.RenderRenewalMail(renewalSample(kind, ""))
		if subject == "" || !strings.Contains(subject, "Acme App") {
			t.Fatalf("%s subject = %q", kind, subject)
		}
		if !strings.Contains(body, "1.190.000 VND") {
			t.Fatalf("%s body lacks the amount:\n%s", kind, body)
		}
		if !strings.Contains(body, "Acme App") {
			t.Fatalf("%s body lacks the product:\n%s", kind, body)
		}
	}
	// The three reminder rungs differ in the days they quote.
	_, three := s.RenderRenewalMail(renewalSample(RenewalMailReminder, ""))
	if !strings.Contains(three, "3 day") {
		t.Fatalf("reminder body lacks the days left:\n%s", three)
	}
}

// The reminder carries a pay link; the suspension mail does not, because
// the order it would point at has expired.
func TestRenderRenewalMailPayLink(t *testing.T) {
	s := newTemplateTestService()
	_, reminder := s.RenderRenewalMail(renewalSample(RenewalMailReminder, ""))
	if !strings.Contains(reminder, `href="https://licenses.example.com/checkout/pay/KG7F3K2Q"`) {
		t.Fatalf("pay link missing:\n%s", reminder)
	}
	suspended := renewalSample(RenewalMailSuspended, "")
	suspended.PayURL = ""
	_, body := s.RenderRenewalMail(suspended)
	if strings.Contains(body, "Pay 1.190.000 VND") {
		t.Fatalf("suspension mail offers a payment:\n%s", body)
	}
}

// Vietnamese is chosen by the install's language, not by the customer's
// browser: the mail is rendered on the server.
func TestRenderRenewalMailVietnamese(t *testing.T) {
	s := newTemplateTestService()
	subject, body := s.RenderRenewalMail(renewalSample(RenewalMailReminder, "vi"))
	if !strings.Contains(body, "ngày") || !strings.Contains(body, "1.190.000 VND") {
		t.Fatalf("not Vietnamese:\n%s", body)
	}
	if subject == s.mustEnglishSubject(RenewalMailReminder) {
		t.Fatalf("subject was not translated: %q", subject)
	}
	_, english := s.RenderRenewalMail(renewalSample(RenewalMailReminder, ""))
	if body == english {
		t.Fatal("Vietnamese body is the English body")
	}
}

func (s *EmailService) mustEnglishSubject(kind string) string {
	subject, _ := s.RenderRenewalMail(renewalSample(kind, ""))
	return subject
}

// An admin can replace the Vietnamese reminder with their own wording,
// and the setting that carries it is the template's setting suffix.
func TestRenderRenewalMailCustomTemplate(t *testing.T) {
	st := testStore(t)
	setSetting(t, st, "email_template_renewal_reminder_vi", `<p>Nhắc gia hạn: {{.Product}} {{.Amount}}</p>`)
	s := NewEmailService("", "", "", "", "", testLogger(), st)
	s.SetBaseURL("https://licenses.example.com/")

	_, body := s.RenderRenewalMail(renewalSample(RenewalMailReminder, "vi"))
	if body != `<p>Nhắc gia hạn: Acme App 1.190.000 VND</p>` {
		t.Fatalf("custom template not used: %q", body)
	}
}

// The new variables are in templateSample, so an admin can save a custom
// template that uses them (ValidateTemplate rejects unknown variables).
func TestRenewalTemplateVariablesAreValidatable(t *testing.T) {
	src := `<p>{{.Product}} {{.Plan}} {{.LicenseKey}} {{.Amount}} {{.DueDate}} ` +
		`{{.DaysLeft}} {{.DaysPastDue}} {{.SuspendDate}} <a href="{{.PayURL}}">pay</a></p>`
	if err := ValidateTemplate(src); err != nil {
		t.Fatalf("a renewal template is not validatable: %v", err)
	}
}
```

Add three tiny helpers used by this package's newer tests, in the same file (the older `email_test.go` fixtures do not expose them):

```go
func testStore(t *testing.T) *store.Store {
	t.Helper()
	return testdb.Store(t)
}

func setSetting(t *testing.T, s *store.Store, key, value string) {
	t.Helper()
	testsupport.SetSetting(t, s.DB, key, value)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
```

with `store`, `testdb`, `testsupport`, `slog`, `io` imported. `internal/service` already has `main_test.go` running `testdb.Main`, so these tests run against the integration database and skip without `TEST_DATABASE_URL`; the two rendering tests that need no database use `newTemplateTestService()` and always run.

- [ ] **Step 2: Run it to fail**

Run: `go test ./internal/service -run 'TestRenderRenewalMail|TestRenewalTemplateVariables' -v`
Expected: FAIL — `undefined: RenewalMail`, `undefined: RenewalMailReminder`.

- [ ] **Step 3: Write `email_renewal.go`**

```go
package service

import "strings"

// The renewal ladder's mail kinds. The billing engine picks one per
// reminder; the template key is the kind, so an admin can edit each.
const (
	RenewalMailReminder  = "renewal_reminder"  // 7, 3 and 1 day before the period ends
	RenewalMailDue       = "renewal_due"       // the period ended today
	RenewalMailGrace     = "renewal_grace"     // 1, 3 and 7 days overdue, still working
	RenewalMailSuspended = "renewal_suspended" // the licence was just suspended
)

// RenewalMail is everything a renewal template may use. Amount and the
// dates arrive preformatted: the engine owns the money formatter and the
// Vietnam calendar, and a template should not do arithmetic.
type RenewalMail struct {
	Kind       string
	Locale     string // "vi" for Vietnamese; anything else renders English
	Product    string
	Plan       string
	LicenseKey string
	Amount     string
	DueDate    string
	DaysLeft   int
	DaysPastDue int
	SuspendDate string
	PayURL     string
}

// RenderRenewalMail renders one rung of the ladder in the install's
// language. SiteName and PortalURL are added by templateData, so every
// template can link the portal and sign with the site's name.
func (s *EmailService) RenderRenewalMail(in RenewalMail) (subject, body string) {
	key, tmpl, subject := renewalTemplate(in)
	data := s.templateData(map[string]any{
		"Product": in.Product, "Plan": in.Plan, "LicenseKey": in.LicenseKey,
		"Amount": in.Amount, "DueDate": in.DueDate,
		"DaysLeft": in.DaysLeft, "DaysPastDue": in.DaysPastDue,
		"SuspendDate": in.SuspendDate, "PayURL": in.PayURL,
	})
	return subject, s.renderEmail(key, tmpl, data)
}

// renewalTemplate picks the template, its subject and the setting suffix
// the admin edits for one kind and language.
func renewalTemplate(in RenewalMail) (key, tmpl, subject string) {
	vi := strings.EqualFold(strings.TrimSpace(in.Locale), "vi")
	key = in.Kind
	if vi {
		key += "_vi"
	}
	switch in.Kind {
	case RenewalMailDue:
		if vi {
			return key, tmplRenewalDueVI, "[" + in.Product + "] đến hạn gia hạn"
		}
		return key, tmplRenewalDue, "[" + in.Product + "] payment due"
	case RenewalMailGrace:
		if vi {
			return key, tmplRenewalGraceVI, "[" + in.Product + "] thanh toán quá hạn"
		}
		return key, tmplRenewalGrace, "[" + in.Product + "] payment overdue"
	case RenewalMailSuspended:
		if vi {
			return key, tmplRenewalSuspendedVI, "[" + in.Product + "] giấy phép đã bị tạm ngưng"
		}
		return key, tmplRenewalSuspended, "[" + in.Product + "] licence suspended"
	default:
		if vi {
			return RenewalMailReminder + "_vi", tmplRenewalReminderVI,
				"[" + in.Product + "] sắp đến hạn gia hạn"
		}
		return RenewalMailReminder, tmplRenewalReminder,
			"[" + in.Product + "] renewal coming up"
	}
}
```

The templates (all eight; `{{if .PayURL}}` guards every payment button so the suspension mail, which has no order, renders without one):

```go
const tmplRenewalReminder = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #111;">Your licence renews soon</h2>
<p>Your <strong>{{.Product}}</strong> licence on the <strong>{{.Plan}}</strong> plan is due for renewal on <strong>{{.DueDate}}</strong>, in {{.DaysLeft}} day(s).</p>
<p>Amount due: <strong>{{.Amount}}</strong></p>
<p>Licence key: <code>{{.LicenseKey}}</code></p>
{{if .PayURL}}<p style="margin: 24px 0;">
  <a href="{{.PayURL}}" style="display: inline-block; padding: 10px 20px; background: #2563eb; color: white; text-decoration: none; border-radius: 6px;">Pay {{.Amount}}</a>
</p>
<p style="font-size: 12px; color: #666;">Or paste this link into your browser: <a href="{{.PayURL}}">{{.PayURL}}</a></p>{{end}}
<p style="color: #666; font-size: 14px;">The licence keeps working until {{.DueDate}} and for a short grace period after it. Paying before then keeps it active without interruption.</p>
</body></html>`

const tmplRenewalDue = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #d97706;">Renewal due today</h2>
<p>Your <strong>{{.Product}}</strong> licence on the <strong>{{.Plan}}</strong> plan reached its renewal date on <strong>{{.DueDate}}</strong>.</p>
<p>Amount due: <strong>{{.Amount}}</strong></p>
<p>Licence key: <code>{{.LicenseKey}}</code></p>
{{if .PayURL}}<p style="margin: 24px 0;">
  <a href="{{.PayURL}}" style="display: inline-block; padding: 10px 20px; background: #2563eb; color: white; text-decoration: none; border-radius: 6px;">Pay {{.Amount}}</a>
</p>
<p style="font-size: 12px; color: #666;">Or paste this link into your browser: <a href="{{.PayURL}}">{{.PayURL}}</a></p>{{end}}
<p style="color: #666; font-size: 14px;">Your licence is still active. If the payment does not arrive within the grace period it will be suspended, and it will be restored automatically as soon as the payment is matched.</p>
</body></html>`

const tmplRenewalGrace = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #d97706;">Payment overdue</h2>
<p>Your <strong>{{.Product}}</strong> licence renewal has been unpaid for <strong>{{.DaysPastDue}} day(s)</strong>.</p>
<p>Amount due: <strong>{{.Amount}}</strong></p>
<p>Licence key: <code>{{.LicenseKey}}</code></p>
{{if .PayURL}}<p style="margin: 24px 0;">
  <a href="{{.PayURL}}" style="display: inline-block; padding: 10px 20px; background: #2563eb; color: white; text-decoration: none; border-radius: 6px;">Pay {{.Amount}}</a>
</p>
<p style="font-size: 12px; color: #666;">Or paste this link into your browser: <a href="{{.PayURL}}">{{.PayURL}}</a></p>{{end}}
<p style="color: #dc2626; font-size: 14px;">The licence keeps working until <strong>{{.SuspendDate}}</strong>. After that it is suspended and stops working until a payment is matched.</p>
</body></html>`

const tmplRenewalSuspended = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #dc2626;">Licence suspended</h2>
<p>Your <strong>{{.Product}}</strong> licence on the <strong>{{.Plan}}</strong> plan has been suspended because its renewal on <strong>{{.DueDate}}</strong> was not paid.</p>
<p>Licence key: <code>{{.LicenseKey}}</code></p>
{{if .PortalURL}}<p style="margin: 24px 0;">
  <a href="{{.PortalURL}}" style="display: inline-block; padding: 10px 20px; background: #2563eb; color: white; text-decoration: none; border-radius: 6px;">Open your account</a>
</p>{{end}}
<p style="color: #666; font-size: 14px;">The licence and its data are kept. Contact us to settle the outstanding {{.Amount}} and restore it.</p>
</body></html>`

const tmplRenewalReminderVI = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #111;">Giấy phép sắp đến hạn gia hạn</h2>
<p>Giấy phép <strong>{{.Product}}</strong> của bạn (gói <strong>{{.Plan}}</strong>) đến hạn gia hạn vào <strong>{{.DueDate}}</strong>, còn {{.DaysLeft}} ngày.</p>
<p>Số tiền cần thanh toán: <strong>{{.Amount}}</strong></p>
<p>Mã giấy phép: <code>{{.LicenseKey}}</code></p>
{{if .PayURL}}<p style="margin: 24px 0;">
  <a href="{{.PayURL}}" style="display: inline-block; padding: 10px 20px; background: #2563eb; color: white; text-decoration: none; border-radius: 6px;">Thanh toán {{.Amount}}</a>
</p>
<p style="font-size: 12px; color: #666;">Hoặc dán liên kết này vào trình duyệt: <a href="{{.PayURL}}">{{.PayURL}}</a></p>{{end}}
<p style="color: #666; font-size: 14px;">Giấy phép vẫn hoạt động đến hết {{.DueDate}} và thêm một khoảng ân hạn ngắn. Thanh toán trước thời điểm đó để không bị gián đoạn.</p>
</body></html>`

const tmplRenewalDueVI = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #d97706;">Đến hạn gia hạn hôm nay</h2>
<p>Giấy phép <strong>{{.Product}}</strong> của bạn (gói <strong>{{.Plan}}</strong>) đã đến hạn gia hạn vào <strong>{{.DueDate}}</strong>.</p>
<p>Số tiền cần thanh toán: <strong>{{.Amount}}</strong></p>
<p>Mã giấy phép: <code>{{.LicenseKey}}</code></p>
{{if .PayURL}}<p style="margin: 24px 0;">
  <a href="{{.PayURL}}" style="display: inline-block; padding: 10px 20px; background: #2563eb; color: white; text-decoration: none; border-radius: 6px;">Thanh toán {{.Amount}}</a>
</p>
<p style="font-size: 12px; color: #666;">Hoặc dán liên kết này vào trình duyệt: <a href="{{.PayURL}}">{{.PayURL}}</a></p>{{end}}
<p style="color: #666; font-size: 14px;">Giấy phép vẫn đang hoạt động. Nếu chưa nhận được thanh toán trong thời gian ân hạn, giấy phép sẽ bị tạm ngưng và tự động mở lại ngay khi thanh toán được ghi nhận.</p>
</body></html>`

const tmplRenewalGraceVI = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #d97706;">Thanh toán quá hạn</h2>
<p>Khoản gia hạn giấy phép <strong>{{.Product}}</strong> đã quá hạn <strong>{{.DaysPastDue}} ngày</strong>.</p>
<p>Số tiền cần thanh toán: <strong>{{.Amount}}</strong></p>
<p>Mã giấy phép: <code>{{.LicenseKey}}</code></p>
{{if .PayURL}}<p style="margin: 24px 0;">
  <a href="{{.PayURL}}" style="display: inline-block; padding: 10px 20px; background: #2563eb; color: white; text-decoration: none; border-radius: 6px;">Thanh toán {{.Amount}}</a>
</p>
<p style="font-size: 12px; color: #666;">Hoặc dán liên kết này vào trình duyệt: <a href="{{.PayURL}}">{{.PayURL}}</a></p>{{end}}
<p style="color: #dc2626; font-size: 14px;">Giấy phép vẫn hoạt động đến hết <strong>{{.SuspendDate}}</strong>. Sau đó giấy phép sẽ bị tạm ngưng cho đến khi thanh toán được ghi nhận.</p>
</body></html>`

const tmplRenewalSuspendedVI = `<!DOCTYPE html>
<html><body style="font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;">
<h2 style="color: #dc2626;">Giấy phép đã bị tạm ngưng</h2>
<p>Giấy phép <strong>{{.Product}}</strong> của bạn (gói <strong>{{.Plan}}</strong>) đã bị tạm ngưng vì khoản gia hạn ngày <strong>{{.DueDate}}</strong> chưa được thanh toán.</p>
<p>Mã giấy phép: <code>{{.LicenseKey}}</code></p>
{{if .PortalURL}}<p style="margin: 24px 0;">
  <a href="{{.PortalURL}}" style="display: inline-block; padding: 10px 20px; background: #2563eb; color: white; text-decoration: none; border-radius: 6px;">Mở trang tài khoản</a>
</p>{{end}}
<p style="color: #666; font-size: 14px;">Giấy phép và dữ liệu vẫn được giữ. Vui lòng liên hệ để thanh toán khoản {{.Amount}} còn thiếu và mở lại giấy phép.</p>
</body></html>`
```

- [ ] **Step 4: Register the templates and their variables**

In `internal/service/email.go`, `DefaultTemplates()` gains eight entries:

```go
		"renewal_reminder":             tmplRenewalReminder,
		"renewal_due":                  tmplRenewalDue,
		"renewal_grace":                tmplRenewalGrace,
		"renewal_suspended":            tmplRenewalSuspended,
		"renewal_reminder_vi":          tmplRenewalReminderVI,
		"renewal_due_vi":               tmplRenewalDueVI,
		"renewal_grace_vi":             tmplRenewalGraceVI,
		"renewal_suspended_vi":         tmplRenewalSuspendedVI,
```

and `templateSample` gains five entries:

```go
	"Amount": "1.190.000 VND", "DueDate": "2026-01-01", "DaysPastDue": 2,
	"SuspendDate": "2026-01-08", "PayURL": "https://example.com/pay",
```

`DaysLeft` is already in the sample. Without these, `ValidateTemplate` refuses any admin template that uses the new variables — the failure would only show up when a customer mail is sent.

- [ ] **Step 5: Run the package to pass**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/service -run 'TestRenderRenewal|TestRenewalTemplate|TestTemplate|TestValidate' -v`
Expected: PASS. `TestEmailNotificationSwitches`-style existing tests still pass (the new keys are additive).

- [ ] **Step 6: Commit**

```bash
go build ./... && go vet ./internal/service
git add internal/service/email_renewal.go internal/service/email_renewal_test.go internal/service/email.go
git commit -m "email: renewal ladder templates in English and Vietnamese (phase 1b module A)"
```

<!-- APPEND -->
### Task 6: The reminder ladder

**Files:**
- Create: `internal/billing/policy.go`
- Create: `internal/billing/reminders.go`
- Modify: `internal/billing/service.go` (`RunOnceAt`, call `remind` from `processRow`)
- Test: `internal/billing/policy_test.go`, `internal/billing/reminders_test.go`

**Interfaces:**
- Consumes: `store.ClaimNotification`, `store.EnqueueEmailAndCloseNotification`, `store.ReleaseNotification`, `store.ErrBillingSubscriptionNotFound`, `service.RenderRenewalMail`, `service.RenewalMail*` kinds.
- Produces:
  - `billing.ReminderRung(now, periodEnd time.Time, graceDays int) string` (tag without the period suffix)
  - `billing.SamePeriod(row *store.BillingSubscription, periodEnd time.Time) bool`
  - `billing.FormatAmount(minor int64, currency string, exp int16, locale string) string`
  - `billing.DisplayDate(t time.Time) string`
  - `billing.SuspendWindow(row *store.BillingSubscription) time.Duration`
  - `(*Service).RunOnceAt(ctx context.Context, now time.Time) error`
  - `s.remind`, `s.queueReminder`

Rung selection is the one decision a whole year of customer mail depends on, so it is pure and table-tested at every edge. Delivery uses the existing claim/queue pair, and the tag carries the period: `renewal_reminder_7:2026-12-01T00:00:00Z`. A bare tag would fire once per licence *ever*, because `notifications` is unique on `(license_id, tag)`.

- [ ] **Step 1: Write the failing policy tests**

```go
// internal/billing/policy_test.go
package billing_test

import (
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/billing"
	"github.com/tabloy/keygate/internal/store"
)

// Every edge of the ladder. The tag is the rung only; the period suffix is
// added where the notification is claimed.
func TestReminderRungBoundaries(t *testing.T) {
	end := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		now    time.Time
		grace  int
		want   string
	}{
		{"8 days left", end.Add(-8 * 24 * time.Hour), 7, ""},
		{"7 days exactly", end.Add(-7 * 24 * time.Hour), 7, "renewal_reminder_7"},
		{"7 days minus a second", end.Add(-7*24*time.Hour + time.Second), 7, "renewal_reminder_7"},
		{"4 days left", end.Add(-4 * 24 * time.Hour), 7, "renewal_reminder_7"},
		{"3 days exactly", end.Add(-3 * 24 * time.Hour), 7, "renewal_reminder_3"},
		{"2 days left", end.Add(-2 * 24 * time.Hour), 7, "renewal_reminder_3"},
		{"1 day exactly", end.Add(-24 * time.Hour), 7, "renewal_reminder_1"},
		{"1 hour left", end.Add(-time.Hour), 7, "renewal_reminder_1"},
		{"at the end", end, 7, "renewal_due"},
		{"23 hours overdue", end.Add(23 * time.Hour), 7, "renewal_due"},
		{"1 day overdue", end.Add(24 * time.Hour), 7, "renewal_grace_1"},
		{"3 days exactly overdue", end.Add(3 * 24 * time.Hour), 7, "renewal_grace_3"},
		{"7 days exactly overdue", end.Add(7 * 24 * time.Hour), 7, "renewal_grace_7"},
		{"grace of 3 skips rung 7", end.Add(7 * 24 * time.Hour), 3, "renewal_grace_3"},
		{"no grace still has rung 1", end.Add(2 * 24 * time.Hour), 0, "renewal_grace_1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := billing.ReminderRung(tc.now, end, tc.grace); got != tc.want {
				t.Fatalf("ReminderRung = %q, want %q", got, tc.want)
			}
		})
	}
}

// The money in the mail is grouped in the customer's convention and never
// printed as a float or as bare minor units.
func TestFormatAmount(t *testing.T) {
	cases := []struct {
		minor    int64
		currency string
		exp      int16
		locale   string
		want     string
	}{
		{1_190_000, "VND", 0, "vi", "1.190.000 VND"},
		{1_190_000, "VND", 0, "", "1,190,000 VND"},
		{500, "VND", 0, "vi", "500 VND"},
		{0, "VND", 0, "vi", "0 VND"},
		{119_000_50, "USD", 2, "", "119,000.50 USD"},
		{100_00, "USD", 2, "vi", "100.00 USD"},
		{5, "USD", 2, "", "0.05 USD"},
	}
	for _, tc := range cases {
		if got := billing.FormatAmount(tc.minor, tc.currency, tc.exp, tc.locale); got != tc.want {
			t.Fatalf("FormatAmount(%d %s) = %q, want %q", tc.minor, tc.currency, got, tc.want)
		}
	}
}

// The date a customer reads is the Vietnamese calendar, while every
// decision stays UTC: 31 Dec 20:00Z is 1 Jan in Ho Chi Minh City.
func TestDisplayDateIsVietnamCalendar(t *testing.T) {
	utcLate := time.Date(2026, 12, 31, 20, 0, 0, 0, time.UTC)
	if got := billing.DisplayDate(utcLate); got != "2027-01-01" {
		t.Fatalf("DisplayDate = %q", got)
	}
	// ... and the rung that same instant selects is decided in UTC.
	end := time.Date(2027, 1, 1, 0, 30, 0, 0, time.UTC)
	if got := billing.ReminderRung(utcLate, end, 7); got != "renewal_due" {
		t.Fatalf("rung = %q", got)
	}
}

// A row still describes the period the caller decided on; a renewal moves
// it, and the stale reminder must not go out.
func TestSamePeriod(t *testing.T) {
	end := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	row := &store.BillingSubscription{CurrentPeriodEnd: &end}
	if !billing.SamePeriod(row, end) {
		t.Fatal("same period reported different")
	}
	later := end.Add(30 * 24 * time.Hour)
	row.CurrentPeriodEnd = &later
	if billing.SamePeriod(row, end) {
		t.Fatal("a renewed period reported the same")
	}
	row.CurrentPeriodEnd = nil
	if billing.SamePeriod(row, end) {
		t.Fatal("a row without a period reported the same")
	}
}

// The suspension tail: a plan that names one uses it, otherwise the tail
// is one billing interval.
func TestSuspendWindow(t *testing.T) {
	end := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		row  *store.BillingSubscription
		want time.Duration
	}{
		{&store.BillingSubscription{CurrentPeriodEnd: &end, RenewalDays: 15}, 15 * 24 * time.Hour},
		{&store.BillingSubscription{CurrentPeriodEnd: &end, BillingInterval: "month"}, 30 * 24 * time.Hour},
		{&store.BillingSubscription{CurrentPeriodEnd: &end, BillingInterval: "year"}, 365 * 24 * time.Hour},
	}
	for _, tc := range cases {
		if got := billing.SuspendWindow(tc.row); got != tc.want {
			t.Fatalf("SuspendWindow(%q, %d) = %v, want %v", tc.row.BillingInterval, tc.row.RenewalDays, got, tc.want)
		}
	}
}
```

- [ ] **Step 2: Run them to fail**

Run: `go test ./internal/billing -run 'TestReminderRung|TestFormatAmount|TestDisplayDate|TestSamePeriod|TestSuspendWindow' -v`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Write `policy.go`**

```go
// internal/billing/policy.go
package billing

import (
	"strconv"
	"strings"
	"time"

	"github.com/tabloy/keygate/internal/store"
)

// The ladder, from furthest out to the end. The tag is `rung`; the
// notification tag adds the period, so each period gets its own rungs.
const (
	RungReminder7   = "renewal_reminder_7"
	RungReminder3   = "renewal_reminder_3"
	RungReminder1   = "renewal_reminder_1"
	RungDue         = "renewal_due"
	RungGrace1      = "renewal_grace_1"
	RungGrace3      = "renewal_grace_3"
	RungGrace7      = "renewal_grace_7"
	RungSuspended   = "renewal_suspended"
)

// ReminderRung picks the one rung due at now: the narrowest window the
// licence has already entered. A rung is sent when it is first reached
// and never again for the same period.
//
// The grace rungs are capped by the plan's grace days, so a plan that
// suspends after 3 days never sends "7 days overdue".
func ReminderRung(now, periodEnd time.Time, graceDays int) string {
	left := periodEnd.Sub(now)
	if left > 0 {
		switch {
		case left <= 24*time.Hour:
			return RungReminder1
		case left <= 3*24*time.Hour:
			return RungReminder3
		case left <= 7*24*time.Hour:
			return RungReminder7
		}
		return ""
	}
	overdue := -left
	switch {
	case overdue < 24*time.Hour:
		return RungDue
	case overdue >= 7*24*time.Hour && graceDays >= 7:
		return RungGrace7
	case overdue >= 3*24*time.Hour && graceDays >= 3:
		return RungGrace3
	default:
		return RungGrace1
	}
}

// SamePeriod reports whether row still describes the period the caller
// decided on. A renewal between the scan and the mail moves
// current_period_end, and the mail must not describe the old period.
func SamePeriod(row *store.BillingSubscription, periodEnd time.Time) bool {
	return row != nil && row.CurrentPeriodEnd != nil && row.CurrentPeriodEnd.Equal(periodEnd)
}

// SuspendWindow is how long a suspended licence is kept before it expires:
// the plan's renewal days when it names them (a perpetual maintenance
// term), otherwise one billing interval.
func SuspendWindow(row *store.BillingSubscription) time.Duration {
	if row.RenewalDays > 0 {
		return time.Duration(row.RenewalDays) * 24 * time.Hour
	}
	return IntervalDuration(row.BillingInterval)
}

// vnZone is Vietnam's offset. The country has no daylight saving, so a
// fixed offset is exact and needs no tzdata in the container.
var vnZone = time.FixedZone("ICT", 7*60*60)

// DisplayDate is a date as a Vietnamese customer reads it. It is used for
// display only — every comparison and stored instant stays UTC.
func DisplayDate(t time.Time) string {
	return t.In(vnZone).Format("2006-01-02")
}

// FormatAmount renders integer minor units for a customer: grouped digits,
// the currency's own number of decimals, and the grouping separator the
// language uses ('.' in Vietnamese, ',' otherwise). The currency code is
// kept visible because a VND amount with no symbol is unambiguous and a
// symbol is not.
func FormatAmount(minor int64, currency string, exp int16, locale string) string {
	negative := minor < 0
	if negative {
		minor = -minor
	}
	digits := strconv.FormatInt(minor, 10)
	fraction := ""
	if exp > 0 {
		for len(digits) <= int(exp) {
			digits = "0" + digits
		}
		fraction = digits[len(digits)-int(exp):]
		digits = digits[:len(digits)-int(exp)]
	}
	sep := ","
	if strings.EqualFold(strings.TrimSpace(locale), "vi") {
		sep = "."
	}
	out := groupDigits(digits, sep)
	if fraction != "" {
		out += "." + fraction
	}
	if currency = strings.ToUpper(strings.TrimSpace(currency)); currency != "" {
		out += " " + currency
	}
	if negative {
		out = "-" + out
	}
	return out
}

// groupDigits inserts sep every three digits, right to left.
func groupDigits(digits, sep string) string {
	if len(digits) <= 3 {
		return digits
	}
	var b strings.Builder
	first := len(digits) % 3
	if first == 0 {
		first = 3
	}
	b.WriteString(digits[:first])
	for i := first; i < len(digits); i += 3 {
		b.WriteString(sep)
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}

// DaysLeft is the days a customer is told are left: rounded up, so 71
// hours reads as 3 days rather than 2.
func DaysLeft(periodEnd, now time.Time) int {
	left := periodEnd.Sub(now)
	if left <= 0 {
		return 0
	}
	days := int(left / (24 * time.Hour))
	if left%(24*time.Hour) != 0 {
		days++
	}
	return days
}

// DaysPastDue is whole days overdue, rounded down: 25 hours is one day.
func DaysPastDue(periodEnd, now time.Time) int {
	overdue := now.Sub(periodEnd)
	if overdue <= 0 {
		return 0
	}
	return int(overdue / (24 * time.Hour))
}
```

- [ ] **Step 4: Run the policy tests to pass**

Run: `go test ./internal/billing -run 'TestReminderRung|TestFormatAmount|TestDisplayDate|TestSamePeriod|TestSuspendWindow' -v`
Expected: PASS.

- [ ] **Step 5: Write the failing ladder tests**

```go
// internal/billing/reminders_test.go
package billing_test

import (
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/service"
)

// Pinning the sweep's now makes the ladder a sequence of exact instants:
// one mail per rung, in order, and none twice.
func TestReminderLadderWalksDown(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 10})
	base := time.Now().UTC().Truncate(time.Second)
	periodEnd := base.Add(9 * 24 * time.Hour)
	lic := h.licence(periodEnd)

	steps := []struct {
		at   time.Time
		kind string
	}{
		{periodEnd.Add(-8 * 24 * time.Hour), ""},
		{periodEnd.Add(-7 * 24 * time.Hour), service.RenewalMailReminder},
		{periodEnd.Add(-3 * 24 * time.Hour), service.RenewalMailReminder},
		{periodEnd.Add(-24 * time.Hour), service.RenewalMailReminder},
		{periodEnd, service.RenewalMailDue},
		{periodEnd.Add(24 * time.Hour), service.RenewalMailGrace},
		{periodEnd.Add(3 * 24 * time.Hour), service.RenewalMailGrace},
		{periodEnd.Add(7 * 24 * time.Hour), service.RenewalMailGrace},
		{periodEnd.Add(9 * 24 * time.Hour), ""}, // the same rung twice
	}
	for i, step := range steps {
		if err := h.svc.RunOnceAt(h.ctx, step.at); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if step.kind == "" {
			continue
		}
		if len(h.mail.sent) != 1 {
			t.Fatalf("step %d: %d mails, want 1", i, len(h.mail.sent))
		}
		if got := h.mail.sent[0].In.Kind; got != step.kind {
			t.Fatalf("step %d: kind %q, want %q", i, got, step.kind)
		}
		h.mail.sent = nil
	}
	// And the licence got exactly one renewal order across all of it.
	if n := len(h.orders(lic.ID)); n != 1 {
		t.Fatalf("%d orders", n)
	}
}

// The mail carries the amount, the pay link, the dates in the Vietnamese
// calendar and the days the rung implies.
func TestReminderMailContent(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(3 * 24 * time.Hour)
	lic := h.licence(periodEnd)
	if err := h.svc.RunOnceAt(h.ctx, periodEnd.Add(-3*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(h.mail.sent) != 1 {
		t.Fatalf("%d mails", len(h.mail.sent))
	}
	in := h.mail.sent[0].In
	// The install has no language set, so the mail is English: ',' groups
	// thousands. The Vietnamese grouping is asserted in its own test.
	if in.Amount != "1,190,000 VND" {
		t.Fatalf("amount = %q", in.Amount)
	}
	if in.DueDate != billing.DisplayDate(periodEnd) {
		t.Fatalf("due date = %q", in.DueDate)
	}
	if in.DaysLeft != 3 {
		t.Fatalf("days left = %d", in.DaysLeft)
	}
	orders := h.orders(lic.ID)
	if len(orders) != 1 || in.PayURL != "https://licenses.test/checkout/pay/"+orders[0].Code {
		t.Fatalf("pay url = %q", in.PayURL)
	}
	if in.Product != h.product.Name || in.Plan != h.plan.Name || in.LicenseKey == "" {
		t.Fatalf("identity fields = %+v", in)
	}
}

// A mail relay that is not configured must not burn the rung: nothing is
// claimed, and the next run sends it once the relay works.
func TestReminderWaitsForEmailConfiguration(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	h.mail.enabled = false
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(2 * 24 * time.Hour)
	lic := h.licence(periodEnd)
	if err := h.svc.RunOnceAt(h.ctx, periodEnd.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(h.mail.sent) != 0 {
		t.Fatalf("%d mails with the relay off", len(h.mail.sent))
	}
	h.mail.enabled = true
	if err := h.svc.RunOnceAt(h.ctx, periodEnd.Add(-47*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(h.mail.sent) != 1 {
		t.Fatalf("%d mails after the relay came back", len(h.mail.sent))
	}
	_ = lic
}

// The existing "renewal_reminder" switch covers the whole ladder.
func TestReminderHonoursNotificationSwitch(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	h.mail.notify = map[string]bool{"renewal_reminder": false}
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(24 * time.Hour)
	h.licence(periodEnd)
	if err := h.svc.RunOnceAt(h.ctx, periodEnd.Add(-23*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(h.mail.sent) != 0 {
		t.Fatalf("%d mails while the switch is off", len(h.mail.sent))
	}
}

// No payable order, no mail: a reminder that cannot say how or where to pay
// is noise.
func TestReminderNeedsAPayableOrder(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", noPrice: true})
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(2 * 24 * time.Hour)
	h.licence(periodEnd)
	if err := h.svc.RunOnceAt(h.ctx, periodEnd.Add(-2*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(h.mail.sent) != 0 {
		t.Fatalf("%d mails without an order", len(h.mail.sent))
	}
}

// The install's language picks the template family and the digit grouping:
// a Vietnamese shop's customer reads "1.190.000 VND", not "1,190,000 VND".
func TestReminderUsesTheInstallLanguage(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	h.setSetting(t, "language", "vi")
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(2 * 24 * time.Hour)
	h.licence(periodEnd)
	if err := h.svc.RunOnceAt(h.ctx, periodEnd.Add(-2*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(h.mail.sent) != 1 {
		t.Fatalf("%d mails", len(h.mail.sent))
	}
	in := h.mail.sent[0].In
	if in.Locale != "vi" || in.Amount != "1.190.000 VND" {
		t.Fatalf("mail = locale %q amount %q", in.Locale, in.Amount)
	}
}
```

Two settings helpers, added to `fixtures_test.go` (later tasks reuse them; `testsupport.SetSetting` restores every row it writes):

```go
// setSetting writes one settings row for the test.
func (h *harness) setSetting(t *testing.T, key, value string) {
	t.Helper()
	testsupport.SetSetting(t, h.s.DB, key, value)
}

// setProductSetting writes a per-product settings row:
// <key>:<product_id>, which is how a product opts into trial conversion.
func (h *harness) setProductSetting(t *testing.T, key, value string) {
	t.Helper()
	h.setSetting(t, key+":"+h.product.ID, value)
}
```

with `"github.com/tabloy/keygate/internal/testsupport"` added to that file's imports.

`h.licence` must use the harness's own email so the queued mail's recipient can be asserted; add to `TestReminderMailContent` a check by reading `email_queue`:

```go
	var queued int
	if err := h.s.DB.NewRaw("SELECT count(*) FROM email_queue WHERE to_addr = ?", lic.Email).Scan(h.ctx, &queued); err != nil {
		t.Fatal(err)
	}
	if queued != 1 {
		t.Fatalf("%d queued mails for %s", queued, lic.Email)
	}
```

and delete `email_queue` rows in the harness cleanup (`DELETE FROM email_queue WHERE notification_id IN (SELECT id FROM notifications WHERE license_id IN (...))` before the notifications delete, or simply `WHERE to_addr LIKE 'cust-%'`). Use the notification join.

- [ ] **Step 6: Run them to fail**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/billing -run 'TestReminder' -v`
Expected: FAIL — `h.svc.RunOnceAt undefined`.

- [ ] **Step 7: Add `RunOnceAt` and write `reminders.go`**

In `service.go`, split the entry point so every boundary is testable without a hidden clock. `lead` leaves `processRow`'s signature — the order path checks its own window — and the scan still resolves it:

```go
// RunOnce is one sweep at the current instant.
func (s *Service) RunOnce(ctx context.Context) error {
	return s.RunOnceAt(ctx, time.Now().UTC())
}

// RunOnceAt is one sweep as if it were now. Run hands it time.Now; the
// tests hand it exact instants, which is how the lead window, every rung
// and the grace cut-off are pinned without a global clock.
func (s *Service) RunOnceAt(ctx context.Context, now time.Time) error {
	now = now.UTC()
	lead := s.renewalLead(ctx)
	rows, err := s.store.ListBillingSubscriptions(ctx, now, lead)
	if err != nil {
		return err
	}
	locale := s.locale(ctx)
	for _, row := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.processRow(ctx, row, now, lead, locale); err != nil {
			s.log.Error("billing: licence failed",
				"license", row.LicenseID, "plan", row.PlanID, "error", err)
		}
	}
	return nil
}
```

and `processRow` becomes the order plus the ladder. The ladder runs whether
or not an order was just created — the 7-day mail goes out the same hour the
order does — and `createRenewalOrder` decides for itself whether an order is
wanted, so there is nothing left to branch on:

```go
func (s *Service) processRow(ctx context.Context, row *store.BillingSubscription, now time.Time, lead time.Duration, locale string) error {
	if row.PlanLicenseType != "subscription" {
		return nil // Task 9 converts opted-in trials here
	}
	if row.CancelAtPeriodEnd {
		return nil // Task 7 closes the subscription at period end
	}
	if _, err := s.createRenewalOrder(ctx, row, now, lead); err != nil {
		return err
	}
	return s.remind(ctx, row, now, locale)
}
```

```go
// internal/billing/reminders.go
package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
)

// reminderToggle is the existing settings switch that turns the whole
// ladder off (service.ToggleableEmails).
const reminderToggle = "renewal_reminder"

// remind sends the rung that is due for this licence, at most once per
// (rung, period). It re-reads the row first: the scan may be an hour old
// and the customer may have paid in between.
func (s *Service) remind(ctx context.Context, row *store.BillingSubscription, now time.Time, locale string) error {
	if row.CurrentPeriodEnd == nil || row.LicenseStatus == model.StatusSuspended {
		return nil
	}
	periodEnd := *row.CurrentPeriodEnd
	rung := ReminderRung(now, periodEnd, row.GraceDays)
	if rung == "" {
		return nil
	}
	fresh, err := s.store.GetBillingSubscription(ctx, row.LicenseID, now)
	if err != nil {
		if errors.Is(err, store.ErrBillingSubscriptionNotFound) {
			return nil
		}
		return err
	}
	if !SamePeriod(fresh, periodEnd) {
		return nil
	}
	if fresh.PendingRenewalCode == "" {
		// Nothing to pay: the plan is unpriced or the order expired.
		return nil
	}
	amount := amountOf(fresh.PendingRenewalTotal, fresh.PendingRenewalCurrency, fresh.PendingRenewalExp)
	return s.queueReminder(ctx, fresh, rung, amount, periodEnd, now, locale)
}

// queueReminder claims the rung for this period and queues one mail. The
// claim is the dedupe: two replicas reaching the same rung inside the
// notification lease queue one mail between them.
func (s *Service) queueReminder(ctx context.Context, row *store.BillingSubscription, rung string,
	amount reminderAmount, periodEnd, now time.Time, locale string) error {
	if s.email == nil {
		return nil
	}
	if !s.email.NotifyEnabled(reminderToggle) {
		return nil
	}
	// No relay configured is normal on a fresh install, and it would warn
	// once per licence per hour: debug, not warn. The rung is not claimed,
	// so the mail still goes out the hour the relay starts working.
	if configured, err := s.email.Configured(); err != nil || !configured {
		s.log.Debug("billing: email is not configured, reminder skipped", "license", row.LicenseID)
		return nil
	}
	// The suspension mail has no order left to pay: the QR expired with
	// the licence, so the template must not offer it.
	payURL := s.payURL(row.PendingRenewalCode)
	if rung == RungSuspended {
		payURL = ""
	}
	tag := rung + ":" + periodEnd.UTC().Format(time.RFC3339)
	token, err := s.store.ClaimNotification(ctx, row.LicenseID, tag)
	if err != nil {
		return fmt.Errorf("claim %s: %w", tag, err)
	}
	if token == "" {
		return nil // already sent, or another run holds it
	}
	mail := service.RenewalMail{
		Kind: mailKind(rung), Locale: locale,
		Product: row.ProductName, Plan: row.PlanName, LicenseKey: s.licenseKey(ctx, row),
		Amount: amount.format(locale), DueDate: DisplayDate(periodEnd),
		DaysLeft: DaysLeft(periodEnd, now), DaysPastDue: DaysPastDue(periodEnd, now),
		SuspendDate: DisplayDate(periodEnd.Add(time.Duration(row.GraceDays) * 24 * time.Hour)),
		PayURL:      payURL,
	}
	subject, body := s.email.RenderRenewalMail(mail)
	if err := s.store.EnqueueEmailAndCloseNotification(ctx, row.Email, subject, body, token); err != nil {
		if errors.Is(err, store.ErrNotificationClaimLost) {
			return nil
		}
		if releaseErr := s.store.ReleaseNotification(ctx, token); releaseErr != nil {
			s.log.Warn("billing: could not release a reminder claim", "license", row.LicenseID, "error", releaseErr)
		}
		return err
	}
	return nil
}

// payURL is the public checkout page for an order code.
func (s *Service) payURL(code string) string {
	if s.baseURL == "" || code == "" {
		return ""
	}
	return s.baseURL + "/checkout/pay/" + code
}

// licenseKey is the decrypted key, or "" when it cannot be read: the mail
// is still worth sending without it.
func (s *Service) licenseKey(ctx context.Context, row *store.BillingSubscription) string {
	lic, err := s.store.FindLicenseByID(ctx, row.LicenseID)
	if err != nil {
		s.log.Warn("billing: licence key unreadable for the reminder", "license", row.LicenseID, "error", err)
		return ""
	}
	return s.store.DecryptLicenseKey(lic)
}

// mailKind maps a rung to the template family it uses.
func mailKind(rung string) string {
	switch {
	case strings.HasPrefix(rung, "renewal_reminder"):
		return service.RenewalMailReminder
	case strings.HasPrefix(rung, "renewal_grace"):
		return service.RenewalMailGrace
	case rung == RungSuspended:
		return service.RenewalMailSuspended
	default:
		return service.RenewalMailDue
	}
}

// reminderAmount is the money a mail quotes, kept in minor units until it
// is formatted for one language.
type reminderAmount struct {
	Minor    int64
	Currency string
	Exp      int16
}

func amountOf(minor int64, currency string, exp int16) reminderAmount {
	return reminderAmount{Minor: minor, Currency: currency, Exp: exp}
}

func (a reminderAmount) format(locale string) string {
	return FormatAmount(a.Minor, a.Currency, a.Exp, locale)
}
```

- [ ] **Step 8: Run the ladder tests to pass**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/billing -v`
Expected: PASS (the Task 4 tests still pass). One Task 4 assertion changes: `RunOnce` now also reminds, so a licence 5 days out gets a `renewal_reminder_7` mail. Add to `TestRunOnceCreatesRenewalOrder`:

```go
	if len(h.mail.sent) != 1 || h.mail.sent[0].In.Kind != service.RenewalMailReminder {
		t.Fatalf("mails = %+v, want one renewal reminder", h.mail.sent)
	}
```

- [ ] **Step 9: Commit**

```bash
go build ./... && go vet ./internal/billing
git add internal/billing
git commit -m "billing: reminder ladder with period-scoped dedupe (phase 1b module A)"
```

<!-- APPEND -->
### Task 7: Lifecycle — past due, suspend, expire, and the customer who turns renewal off

**Files:**
- Create: `internal/billing/lifecycle.go`
- Modify: `internal/billing/service.go` (`processRow` steps, `ReminderSender` gains `SendLicenseExpired`)
- Modify: `internal/billing/fixtures_test.go` (fake records the expiry mail; `h.pay` helper)
- Test: `internal/billing/lifecycle_test.go`

**Interfaces:**
- Consumes: `store.MarkBillingPastDue`, `store.SuspendBillingLicense`, `store.ExpireBillingSuspendedLicense`, `store.CancelBillingSubscriptionAtPeriodEnd` (T2), `billing.SuspendWindow`, `s.queueReminder` (T6), `store.Audit`, `model.EventLicensePaymentFailed/Suspended/Expired/Canceled`.
- Produces: `s.markPastDue`, `s.suspend`, `s.expireSuspended`, `s.closeCanceledSubscription`; `processRow`'s final step order.

The licence walks `active → past_due` when its period ends unpaid, `past_due → suspended` when the grace days run out (and the QR dies in the same transaction), `suspended → expired` when the dunning tail runs out. Payment puts it back — but only while it is still eligible, which settlement already decides.

- [ ] **Step 1: Extend the harness for the tests**

In `fixtures_test.go`:

```go
func (m *fakeMail) SendLicenseExpired(to, productName string) {
	m.expired = append(m.expired, to+"|"+productName)
}
```
(`fakeMail` gains `expired []string`.)

```go
// pay settles one order the way a verified provider webhook would.
func (h *harness) pay(t *testing.T, order *model.PaymentOrder) error {
	t.Helper()
	svc := &settlement.Service{Store: h.s, Log: slog.New(slog.NewTextHandler(discard{}, nil))}
	return svc.Settle(h.ctx, order.ID, order.Provider, provider.WebhookEvent{
		Provider: order.Provider, ProviderEventID: uuid.NewString(), ProviderRef: uuid.NewString(),
		OrderCode: order.Code, AmountMinor: order.TotalMinor, Currency: order.Currency,
		Status: domain.OrderPaid, OccurredAt: time.Now().UTC(), TransferType: "in",
		Raw: []byte(`{}`),
	})
}

// reload reads the licence and its subscription back from the database.
func (h *harness) reload(t *testing.T, licenseID string) (*model.License, *model.Subscription) {
	t.Helper()
	lic, err := h.s.FindLicenseByID(h.ctx, licenseID)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := h.s.FindSubscriptionByLicense(h.ctx, licenseID)
	if err != nil {
		t.Fatal(err)
	}
	return lic, sub
}
```

with `settlement`, `provider`, `domain` and `uuid` imported.

And in `service.go`, the interface grows one method:

```go
type ReminderSender interface {
	RenderRenewalMail(in service.RenewalMail) (subject, body string)
	NotifyEnabled(kind string) bool
	Configured() (bool, error)
	// SendLicenseExpired is the platform's existing expiry notice, sent
	// when the engine gives up on a suspended licence.
	SendLicenseExpired(to, productName string)
}
```

- [ ] **Step 2: Write the failing lifecycle tests**

```go
// internal/billing/lifecycle_test.go
package billing_test

import (
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
)

// The end of the paid period, unpaid: past due, with the subscription, and
// one payment_failed event. The customer keeps working.
func TestPastDueAtPeriodEnd(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(2 * 24 * time.Hour)
	lic := h.licence(periodEnd)
	// A second before the end nothing happens.
	if err := h.svc.RunOnceAt(h.ctx, periodEnd.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.reload(t, lic.ID); got.Status != model.StatusActive {
		t.Fatalf("status before the period ended = %s", got.Status)
	}
	if err := h.svc.RunOnceAt(h.ctx, periodEnd); err != nil {
		t.Fatal(err)
	}
	got, sub := h.reload(t, lic.ID)
	if got.Status != model.StatusPastDue || got.PastDueAt == nil {
		t.Fatalf("licence = %s past_due_at=%v", got.Status, got.PastDueAt)
	}
	if sub.Status != model.StatusPastDue {
		t.Fatalf("subscription = %s", sub.Status)
	}
	if !h.sawEvent(model.EventLicensePaymentFailed) {
		t.Fatalf("events = %+v", h.events.got)
	}
	// The reminder for the due rung went out too.
	if len(h.mail.sent) != 1 || h.mail.sent[0].In.Kind != service.RenewalMailDue {
		t.Fatalf("mails = %+v", h.mail.sent)
	}
}

// Grace is the boundary between past due and suspended, and the QR dies
// with the licence.
func TestSuspendAtGraceEnd(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(2 * 24 * time.Hour)
	lic := h.licence(periodEnd)
	if err := h.svc.RunOnceAt(h.ctx, periodEnd); err != nil {
		t.Fatal(err)
	}
	orders := h.orders(lic.ID)
	if len(orders) != 1 {
		t.Fatalf("%d orders", len(orders))
	}
	// One second before the grace ends: still past due, order still open.
	if err := h.svc.RunOnceAt(h.ctx, periodEnd.Add(7*24*time.Hour-time.Second)); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.reload(t, lic.ID); got.Status != model.StatusPastDue {
		t.Fatalf("status a second before grace ended = %s", got.Status)
	}
	// At the boundary: suspended, order expired, one suspension mail.
	h.mail.sent = nil
	if err := h.svc.RunOnceAt(h.ctx, periodEnd.Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ := h.reload(t, lic.ID)
	if got.Status != model.StatusSuspended || got.SuspendedBy != "billing" {
		t.Fatalf("licence = %s by %q", got.Status, got.SuspendedBy)
	}
	if o := h.orders(lic.ID)[0]; o.Status != "expired" {
		t.Fatalf("order = %s, want expired", o.Status)
	}
	if !h.sawEvent(model.EventLicenseSuspended) {
		t.Fatalf("events = %+v", h.events.got)
	}
	if len(h.mail.sent) != 1 || h.mail.sent[0].In.Kind != service.RenewalMailSuspended {
		t.Fatalf("mails = %+v", h.mail.sent)
	}
	if h.mail.sent[0].In.PayURL != "" {
		t.Fatalf("suspension mail offers a dead link: %q", h.mail.sent[0].In.PayURL)
	}
	// A second run at the same instant changes nothing.
	if err := h.svc.RunOnceAt(h.ctx, periodEnd.Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(h.mail.sent) != 1 {
		t.Fatalf("suspension mailed twice: %+v", h.mail.sent)
	}
}

// The dunning tail: a suspended licence is expired after its window, and
// the customer gets the platform's expiry notice.
func TestExpireSuspendedTail(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7, renewalDays: 15})
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(-30 * 24 * time.Hour)
	lic := h.licence(periodEnd)
	suspendedAt := time.Now().UTC().Truncate(time.Second).Add(-15*24*time.Hour + time.Hour)
	if _, err := h.s.DB.NewRaw(`UPDATE licenses SET status = 'suspended', suspended_by = 'billing',
		suspended_at = ? WHERE id = ?`, suspendedAt, lic.ID).Exec(h.ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RunOnceAt(h.ctx, suspendedAt.Add(14*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.reload(t, lic.ID); got.Status != model.StatusSuspended {
		t.Fatalf("expired before the tail ended: %s", got.Status)
	}
	if err := h.svc.RunOnceAt(h.ctx, suspendedAt.Add(15*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.reload(t, lic.ID); got.Status != model.StatusExpired {
		t.Fatalf("status = %s", got.Status)
	}
	if !h.sawEvent(model.EventLicenseExpired) {
		t.Fatalf("events = %+v", h.events.got)
	}
	if len(h.mail.expired) != 1 || h.mail.expired[0] != lic.Email+"|"+h.product.Name {
		t.Fatalf("expiry mails = %+v", h.mail.expired)
	}
}

// cancel_at_period_end closes the subscription and nothing else: no order,
// no reminder, and the licence is left to the expiry rules.
func TestCancelAtPeriodEndClosesSubscription(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(2 * 24 * time.Hour)
	lic := h.licence(periodEnd)
	if _, err := h.s.DB.NewRaw(`UPDATE subscriptions SET cancel_at_period_end = true
		WHERE license_id = ?`, lic.ID).Exec(h.ctx); err != nil {
		t.Fatal(err)
	}
	// Before the end: no order and no mail.
	if err := h.svc.RunOnceAt(h.ctx, periodEnd.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := len(h.orders(lic.ID)); n != 0 {
		t.Fatalf("%d orders for a cancelling subscription", n)
	}
	if len(h.mail.sent) != 0 {
		t.Fatalf("mails = %+v", h.mail.sent)
	}
	// At the end: the subscription closes, the licence still works.
	if err := h.svc.RunOnceAt(h.ctx, periodEnd); err != nil {
		t.Fatal(err)
	}
	got, sub := h.reload(t, lic.ID)
	if sub.Status != model.StatusCanceled || sub.CanceledAt == nil {
		t.Fatalf("subscription = %s canceled_at=%v", sub.Status, sub.CanceledAt)
	}
	if got.Status != model.StatusActive {
		t.Fatalf("licence = %s", got.Status)
	}
	if !h.sawEvent(model.EventLicenseCanceled) {
		t.Fatalf("events = %+v", h.events.got)
	}
	// And it is not picked up again: a closed subscription is not billed.
	if err := h.svc.RunOnceAt(h.ctx, periodEnd.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := len(h.orders(lic.ID)); n != 0 {
		t.Fatalf("%d orders after the subscription closed", n)
	}
	// The handoff: once the engine stops scanning a cancelled licence, the
	// platform's own grace-expiry sweep has to see it, or it works forever.
	due, err := h.s.FindLicensesForGraceExpiry(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range due {
		if l.ID == lic.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("a cancelled VN licence is invisible to the platform expiry sweep")
	}
}

// The customer pays one day into grace: the licence is active again, the
// period extended, and past_due_at cleared — settlement's existing write.
func TestPaymentDuringGraceReactivates(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(-24 * time.Hour)
	lic := h.licence(periodEnd)
	if err := h.svc.RunOnceAt(h.ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.reload(t, lic.ID); got.Status != model.StatusPastDue {
		t.Fatalf("status = %s", got.Status)
	}
	order := h.orders(lic.ID)[0]
	if err := h.pay(t, order); err != nil {
		t.Fatalf("pay: %v", err)
	}
	got, sub := h.reload(t, lic.ID)
	if got.Status != model.StatusActive || got.PastDueAt != nil {
		t.Fatalf("licence = %s past_due_at=%v", got.Status, got.PastDueAt)
	}
	if got.ValidUntil == nil || !got.ValidUntil.Equal(periodEnd.Add(30*24*time.Hour)) {
		t.Fatalf("valid_until = %v", got.ValidUntil)
	}
	if sub.Status != model.StatusActive {
		t.Fatalf("subscription = %s", sub.Status)
	}
	// The next sweep sees the new period and does not suspend or re-order.
	if err := h.svc.RunOnceAt(h.ctx, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.reload(t, lic.ID); got.Status != model.StatusActive {
		t.Fatalf("status after the next sweep = %s", got.Status)
	}
	if n := len(h.orders(lic.ID)); n != 1 {
		t.Fatalf("%d orders", n)
	}
}

// After the grace ran out the licence is suspended and its order expired:
// a late transfer is recorded for an operator, and the licence stays down.
func TestPaymentAfterSuspensionIsRecordedUnmatched(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(2 * 24 * time.Hour)
	lic := h.licence(periodEnd)
	if err := h.svc.RunOnceAt(h.ctx, periodEnd); err != nil {
		t.Fatal(err)
	}
	order := h.orders(lic.ID)[0]
	if err := h.svc.RunOnceAt(h.ctx, periodEnd.Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.reload(t, lic.ID); got.Status != model.StatusSuspended {
		t.Fatalf("status = %s", got.Status)
	}
	if err := h.pay(t, order); err != nil {
		t.Fatalf("late payment must be recorded, not retried: %v", err)
	}
	got, _ := h.reload(t, lic.ID)
	if got.Status != model.StatusSuspended {
		t.Fatalf("a late transfer reactivated the licence: %s", got.Status)
	}
	var unmatched int
	if err := h.s.DB.NewRaw(`SELECT count(*) FROM payment_transactions
		WHERE order_id = ? AND match_status = 'unmatched'`, order.ID).Scan(h.ctx, &unmatched); err != nil {
		t.Fatal(err)
	}
	if unmatched != 1 {
		t.Fatalf("%d unmatched transactions for the late transfer", unmatched)
	}
}

// A licence an operator revoked gets no order and no mail, even inside the
// lead window.
func TestRevokedLicenceGetsNoOrder(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	lic := h.licence(time.Now().UTC().Add(2 * 24 * time.Hour))
	if _, err := h.s.DB.NewRaw(`UPDATE licenses SET status = 'revoked' WHERE id = ?`, lic.ID).Exec(h.ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RunOnce(h.ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(h.orders(lic.ID)); n != 0 {
		t.Fatalf("%d orders for a revoked licence", n)
	}
	if len(h.mail.sent) != 0 {
		t.Fatalf("mails = %+v", h.mail.sent)
	}
}
```

Plus `sawEvent` in the fixtures file:

```go
func (h *harness) sawEvent(name string) bool {
	for _, ev := range h.events.got {
		if ev.Event == name {
			return true
		}
	}
	return false
}
```

- [ ] **Step 3: Run them to fail**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/billing -run 'TestPastDueAtPeriodEnd|TestSuspendAtGrace|TestExpireSuspended|TestCancelAtPeriodEnd|TestPaymentDuringGrace|TestPaymentAfterSuspension|TestRevoked' -v`
Expected: FAIL — nothing moves the licence off `active` yet.

- [ ] **Step 4: Write `lifecycle.go`**

```go
// internal/billing/lifecycle.go
package billing

import (
	"context"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

// markPastDue moves the licence into past_due when the period it paid for
// has ended. The store repeats the condition, so a payment that landed
// since the scan wins.
func (s *Service) markPastDue(ctx context.Context, row *store.BillingSubscription, now time.Time) (bool, error) {
	if row.CurrentPeriodEnd == nil || now.Before(*row.CurrentPeriodEnd) {
		return false, nil
	}
	if row.LicenseStatus != model.StatusActive && row.LicenseStatus != model.StatusTrialing {
		return false, nil
	}
	changed, err := s.store.MarkBillingPastDue(ctx, row.LicenseID, now)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	s.log.Info("billing: licence is past due", "license", row.LicenseID, "period_end", row.CurrentPeriodEnd)
	s.store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: row.LicenseID, Action: "past_due",
		ActorType: "system",
		Changes:   map[string]any{"reason": "renewal_unpaid", "period_end": row.CurrentPeriodEnd},
	})
	s.dispatch(ctx, row, model.EventLicensePaymentFailed, map[string]any{
		"license_id": row.LicenseID, "email": row.Email, "reason": "renewal_unpaid",
	})
	return true, nil
}

// suspend ends dunning once the grace days have run out: the licence is
// suspended, its open order expired, and the customer told, in that order.
func (s *Service) suspend(ctx context.Context, row *store.BillingSubscription, now time.Time, locale string) (bool, error) {
	grace := time.Duration(row.GraceDays) * 24 * time.Hour
	if now.Before(row.CurrentPeriodEnd.Add(grace)) {
		return false, nil
	}
	changed, err := s.store.SuspendBillingLicense(ctx, row.LicenseID, now.Add(-grace), now)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	s.log.Info("billing: licence suspended, renewal unpaid",
		"license", row.LicenseID, "period_end", row.CurrentPeriodEnd, "grace_days", row.GraceDays)
	s.store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: row.LicenseID, Action: "suspended",
		ActorType: "system",
		Changes: map[string]any{
			"reason": "renewal_unpaid", "period_end": row.CurrentPeriodEnd, "grace_days": row.GraceDays,
		},
	})
	s.dispatch(ctx, row, model.EventLicenseSuspended, map[string]any{
		"license_id": row.LicenseID, "email": row.Email, "reason": "renewal_unpaid",
	})
	amount := amountOf(row.PendingRenewalTotal, row.PendingRenewalCurrency, row.PendingRenewalExp)
	if err := s.queueReminder(ctx, row, RungSuspended, amount, *row.CurrentPeriodEnd, now, locale); err != nil {
		s.log.Warn("billing: suspension mail failed", "license", row.LicenseID, "error", err)
	}
	return true, nil
}

// expireSuspended ends a licence whose dunning tail has run out. The
// customer gets the platform's existing expiry notice.
func (s *Service) expireSuspended(ctx context.Context, row *store.BillingSubscription, now time.Time) error {
	if row.SuspendedAt == nil {
		return nil
	}
	window := SuspendWindow(row)
	if now.Before(row.SuspendedAt.Add(window)) {
		return nil
	}
	changed, err := s.store.ExpireBillingSuspendedLicense(ctx, row.LicenseID, now.Add(-window), now)
	if err != nil || !changed {
		return err
	}
	s.log.Info("billing: suspended licence expired",
		"license", row.LicenseID, "suspended_at", row.SuspendedAt, "window", window)
	s.store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: row.LicenseID, Action: "expired",
		ActorType: "system",
		Changes:   map[string]any{"reason": "dunning_tail_ended", "suspended_at": row.SuspendedAt},
	})
	s.dispatch(ctx, row, model.EventLicenseExpired, map[string]any{
		"license_id": row.LicenseID, "email": row.Email, "reason": "dunning_tail_ended",
	})
	if s.email != nil {
		s.email.SendLicenseExpired(row.Email, row.ProductName)
	}
	return nil
}

// closeCanceledSubscription closes a subscription whose owner turned
// renewal off, once the paid period ends. The licence itself is untouched:
// it ends through the existing expiry rules like any other licence.
func (s *Service) closeCanceledSubscription(ctx context.Context, row *store.BillingSubscription, now time.Time) error {
	changed, err := s.store.CancelBillingSubscriptionAtPeriodEnd(ctx, row.LicenseID, now)
	if err != nil || !changed {
		return err
	}
	s.log.Info("billing: subscription closed at period end", "license", row.LicenseID)
	s.store.Audit(ctx, &model.AuditLog{
		Entity: "license", EntityID: row.LicenseID, Action: "subscription_canceled",
		ActorType: "system",
		Changes:   map[string]any{"reason": "cancel_at_period_end", "period_end": row.CurrentPeriodEnd},
	})
	s.dispatch(ctx, row, model.EventLicenseCanceled, map[string]any{
		"license_id": row.LicenseID, "email": row.Email, "reason": "cancel_at_period_end",
	})
	return nil
}

// dispatch publishes one licence event for the product the row belongs to.
func (s *Service) dispatch(ctx context.Context, row *store.BillingSubscription, event string, data map[string]any) {
	if s.webhook == nil {
		return
	}
	s.webhook.Dispatch(ctx, row.ProductID, event, data)
}
```

- [ ] **Step 5: Wire the steps into `processRow`**

```go
func (s *Service) processRow(ctx context.Context, row *store.BillingSubscription, now time.Time, lead time.Duration, locale string) error {
	if row.CurrentPeriodEnd == nil {
		return nil
	}
	if row.PlanLicenseType != "subscription" {
		// A trial is converted or it ends; Task 9 replaces this line.
		return nil
	}

	// 1. The paid period ended unpaid.
	pastDue, err := s.markPastDue(ctx, row, now)
	if err != nil {
		return err
	}
	if pastDue {
		row.LicenseStatus = model.StatusPastDue
	}

	// 2. Renewal was turned off: close the subscription, order nothing.
	if row.CancelAtPeriodEnd {
		if row.CurrentPeriodEnd.After(now) {
			return nil
		}
		return s.closeCanceledSubscription(ctx, row, now)
	}

	// 3. Grace ran out.
	if row.LicenseStatus == model.StatusPastDue {
		suspended, err := s.suspend(ctx, row, now, locale)
		if err != nil || suspended {
			return err
		}
	}
	// 4. The dunning tail ran out.
	if row.LicenseStatus == model.StatusSuspended {
		return s.expireSuspended(ctx, row, now)
	}

	// 5. The order for the period that is ending.
	if _, err := s.createRenewalOrder(ctx, row, now, lead); err != nil {
		return err
	}
	// 6. What the customer is told.
	return s.remind(ctx, row, now, locale)
}
```

`processRow` keeps Task 6's signature; only its body changes.

- [ ] **Step 6: Run the package to pass**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/billing -v`
Expected: PASS. If `TestPaymentDuringGraceReactivates` fails with an unmatched transaction, the event's `AmountMinor` is wrong: it must equal the order's `TotalMinor`.

- [ ] **Step 7: Commit**

```bash
go build ./... && go vet ./internal/billing
git add internal/billing
git commit -m "billing: past-due, suspension, expiry and cancel-at-period-end (phase 1b module A)"
```

<!-- APPEND -->
### Task 8: Plan changes — quote, order, and the `next_cycle` mode

**Files:**
- Create: `internal/billing/upgrade.go`
- Modify: `internal/store/billing.go` (upgrade row writes + `FindPaidOrderForPeriod` + `ApplyUpgradeIn`)
- Test: `internal/billing/upgrade_test.go`, `internal/store/billing_test.go` (append)

**Interfaces:**
- Consumes: `checkout.ProrateMinor`, `checkout.UpgradeAmount`, `store.ResolvePlanPrice`, `store.FindLicenseByID`, `store.FindSubscriptionByLicense`, `store.FindPaidOrderForPeriod`, `store.CreatePendingUpgrade`, `store.LinkUpgradeOrder`, `store.FindPendingUpgradeForLicense`, `store.ApplyUpgradeIn`.
- Produces:
  - `billing.UpgradeRequest`, `billing.UpgradeQuote`, `billing.UpgradeResult`
  - `(*Service).QuoteUpgrade(ctx, req) (*UpgradeQuote, error)`
  - `(*Service).CreateUpgradeOrder(ctx, req) (*UpgradeResult, error)`
  - sentinels `ErrLicenseNotEligible`, `ErrUpgradeNotChargeable`, `ErrUpgradeInProgress`
  - store: `CreatePendingUpgradeIn`, `LinkUpgradeOrderIn`, `FindPendingUpgradeForLicense`, `FindPaidOrderForPeriod`, `ApplyUpgradeIn`, `ErrUpgradeIneligible`, `ErrUpgradeInvalid`

**Prerequisite:** Lead-owned patch **P0** must be applied first. `license_upgrades` today has a *non-partial* unique index on `stripe_checkout_session_id`, so a second VN upgrade row (`''`) collides with the first. P0 makes it partial and adds the `payment_order_id` unique indexes the contract requires. Do not start this task on the old index.

`plans.proration_mode` decides how the change is charged, and the **target** plan's mode governs (the charge is for the new plan):

- `immediate` (the column default): charge the prorated new price less the unused-time credit, now.
- `next_cycle`: change the plan now, charge nothing, and let the next renewal order carry the new price. No order row, no payment, no refund.

- [ ] **Step 1: Write the failing store tests**

```go
// internal/store/billing_test.go (append)
func TestFindPaidOrderForPeriod(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(20 * 24 * time.Hour)
	prod, plan, lic := seedBillingLicence(t, s, billingSeed{periodEnd: periodEnd})
	order := renewalOrderFor(t, s, prod, plan, lic, periodEnd)
	if _, err := s.DB.NewRaw("UPDATE payment_orders SET status = 'paid' WHERE id = ?", order.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := s.FindPaidOrderForPeriod(ctx, lic.ID, periodEnd)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != order.ID {
		t.Fatalf("order = %+v", got)
	}
	// A different period has no paid order; that is not an error.
	other, err := s.FindPaidOrderForPeriod(ctx, lic.ID, periodEnd.Add(-30*24*time.Hour))
	if err != nil || other != nil {
		t.Fatalf("other period = %+v err=%v", other, err)
	}
}

// The upgrade row is the receipt for the plan change: one pending row per
// licence, keyed by the order that pays for it.
func TestUpgradeRowLifecycle(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(20 * 24 * time.Hour)
	prod, plan, lic := seedBillingLicence(t, s, billingSeed{periodEnd: periodEnd})
	target := &model.Plan{
		ProductID: prod.ID, Name: "Yearly", Slug: "yearly-" + newSuffix(t),
		LicenseType: "subscription", BillingInterval: "year", LicenseModel: "standard",
		MaxActivations: 10, Active: true,
	}
	if err := s.CreatePlan(ctx, target); err != nil {
		t.Fatal(err)
	}
	up := &model.LicenseUpgrade{
		LicenseID: lic.ID, ToPlanID: target.ID, State: model.UpgradePending,
		FromPlanID: plan.ID, FromStatus: lic.Status, FromQuantity: 1,
	}
	if err := s.CreatePendingUpgrade(ctx, up); err != nil {
		t.Fatal(err)
	}
	// One pending row per licence.
	second := &model.LicenseUpgrade{LicenseID: lic.ID, ToPlanID: target.ID, State: model.UpgradePending, FromQuantity: 1}
	if err := s.CreatePendingUpgrade(ctx, second); !errors.Is(err, store.ErrUpgradeInProgress) {
		t.Fatalf("second pending upgrade err = %v", err)
	}
	found, err := s.FindPendingUpgradeForLicense(ctx, lic.ID)
	if err != nil || found.ID != up.ID {
		t.Fatalf("found = %+v err=%v", found, err)
	}
	order := renewalOrderFor(t, s, prod, plan, lic, periodEnd)
	if err := s.LinkUpgradeOrder(ctx, up.ID, order.ID); err != nil {
		t.Fatal(err)
	}
	byOrder, err := s.FindUpgradeByPaymentOrder(ctx, order)
	if err != nil || byOrder.ID != up.ID {
		t.Fatalf("by order = %+v err=%v", byOrder, err)
	}
	// One order pays for one change: the partial unique index on
	// payment_order_id (P0) refuses the second link. Close the first row
	// so the "one pending per licence" rule is not what fails here.
	if _, err := s.DB.NewRaw(`UPDATE license_upgrades SET state = 'applied', applied_at = now()
		WHERE id = ?`, up.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	third := &model.LicenseUpgrade{LicenseID: lic.ID, ToPlanID: target.ID, State: model.UpgradePending, FromQuantity: 1}
	if err := s.CreatePendingUpgrade(ctx, third); err != nil {
		t.Fatal(err)
	}
	if err := s.LinkUpgradeOrder(ctx, third.ID, order.ID); err == nil {
		t.Fatal("two upgrades linked to one order")
	}
}

// Applying an upgrade with no order changes the plan and nothing else:
// that is the next_cycle mode, which charges nothing.
func TestApplyUpgradeInChangesPlanOnly(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(20 * 24 * time.Hour)
	prod, plan, lic := seedBillingLicence(t, s, billingSeed{periodEnd: periodEnd})
	target := &model.Plan{
		ProductID: prod.ID, Name: "Yearly", Slug: "yearly-" + newSuffix(t),
		LicenseType: "subscription", BillingInterval: "year", LicenseModel: "standard",
		MaxActivations: 10, Active: true,
	}
	if err := s.CreatePlan(ctx, target); err != nil {
		t.Fatal(err)
	}
	up := &model.LicenseUpgrade{LicenseID: lic.ID, ToPlanID: target.ID, State: model.UpgradePending, FromPlanID: plan.ID, FromQuantity: 1}
	if err := s.CreatePendingUpgrade(ctx, up); err != nil {
		t.Fatal(err)
	}
	err := s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		return s.ApplyUpgradeIn(ctx, tx, up.ID, target, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.FindLicenseByID(ctx, lic.ID)
	if got.PlanID != target.ID || got.Status != model.StatusActive {
		t.Fatalf("licence = plan %s status %s", got.PlanID, got.Status)
	}
	if got.ValidUntil == nil || !got.ValidUntil.Equal(periodEnd) {
		t.Fatalf("next_cycle moved valid_until: %v", got.ValidUntil)
	}
	applied, err := s.FindPendingUpgradeForLicense(ctx, lic.ID)
	if err != nil || applied != nil {
		t.Fatalf("upgrade still pending: %+v err=%v", applied, err)
	}
}
```

Add `"errors"`, `"github.com/uptrace/bun"` to that file's imports if not already there.

- [ ] **Step 2: Run them to fail**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store -run 'TestFindPaidOrder|TestUpgradeRow|TestApplyUpgrade' -v`
Expected: FAIL — methods undefined.

- [ ] **Step 3: Write the store half (append to `store/billing.go`)**

```go
// ErrUpgradeInProgress: the licence already has a pending plan change.
var ErrUpgradeInProgress = errors.New("a plan change is already open for this licence")

// ErrUpgradeIneligible: the licence cannot change to this plan.
var ErrUpgradeIneligible = errors.New("licence cannot change to this plan")

// ErrUpgradeInvalid: the upgrade row or the order it names is not usable.
var ErrUpgradeInvalid = errors.New("upgrade is not applicable")

// FindPaidOrderForPeriod returns the order that paid for the period ending
// at periodEnd, or nil when the licence was granted without one. The
// upgrade credit is the price the customer actually paid, not today's
// price list, so a mid-period price change does not move their money.
func (s *Store) FindPaidOrderForPeriod(ctx context.Context, licenseID string, periodEnd time.Time) (*model.PaymentOrder, error) {
	order := new(model.PaymentOrder)
	err := s.DB.NewSelect().Model(order).
		Where("license_id = ?", licenseID).
		Where("status = ?", "paid").
		Where("period_end = ?", periodEnd).
		Where("kind IN ('new', 'renewal')").
		Order("created_at DESC", "id DESC").
		Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find paid order for period: %w", err)
	}
	return order, nil
}

// CreatePendingUpgrade records a plan change that is about to be paid for
// (or, in next_cycle mode, about to be applied). One pending row per
// licence: a second attempt while the first is open is refused rather than
// stacked, because two orders for one change is two charges.
func (s *Store) CreatePendingUpgrade(ctx context.Context, up *model.LicenseUpgrade) error {
	if up.ID == "" {
		up.ID = newID()
	}
	if up.State == "" {
		up.State = model.UpgradePending
	}
	// The Stripe column is not a place to park a VN identifier; the row
	// keeps the empty string the VN rails always wrote, and P0's partial
	// index ignores it.
	up.StripeCheckoutSessionID = ""
	err := s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		var pending int
		if err := tx.NewRaw(`SELECT count(*) FROM license_upgrades
			WHERE license_id = ? AND state = 'pending'`, up.LicenseID).Scan(ctx, &pending); err != nil {
			return err
		}
		if pending > 0 {
			return ErrUpgradeInProgress
		}
		_, err := tx.NewInsert().Model(up).Exec(ctx)
		return err
	})
	if IsUniqueViolation(err) {
		return ErrUpgradeInProgress
	}
	return err
}

// LinkUpgradeOrder records the order that pays for a plan change, which is
// how settlement finds the row it has to apply.
func (s *Store) LinkUpgradeOrder(ctx context.Context, upgradeID, orderID string) error {
	res, err := s.DB.NewRaw(`UPDATE license_upgrades SET payment_order_id = ?
		WHERE id = ? AND state = 'pending' AND payment_order_id IS NULL`, orderID, upgradeID).Exec(ctx)
	if err != nil {
		if IsUniqueViolation(err) {
			return fmt.Errorf("%w: order already pays for another upgrade", ErrUpgradeInvalid)
		}
		return fmt.Errorf("link upgrade order: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: upgrade %s is not pending or already linked", ErrUpgradeInvalid, upgradeID)
	}
	return nil
}

// FindPendingUpgradeForLicense returns the open plan change, or nil.
func (s *Store) FindPendingUpgradeForLicense(ctx context.Context, licenseID string) (*model.LicenseUpgrade, error) {
	up := new(model.LicenseUpgrade)
	err := s.DB.NewSelect().Model(up).
		Where("license_id = ?", licenseID).Where("state = ?", model.UpgradePending).
		Order("created_at DESC").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return up, nil
}

// FindUpgradeByPaymentOrder returns the plan change an order pays for,
// falling back to the upgrade_id in the order's metadata. The second path
// covers a crash between the order insert and the link write: the money
// must still find its upgrade.
func (s *Store) FindUpgradeByPaymentOrder(ctx context.Context, order *model.PaymentOrder) (*model.LicenseUpgrade, error) {
	up := new(model.LicenseUpgrade)
	err := s.DB.NewSelect().Model(up).Where("payment_order_id = ?", order.ID).Limit(1).Scan(ctx)
	if err == nil {
		return up, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var meta struct {
		UpgradeID string `json:"upgrade_id"`
	}
	if len(order.Metadata) > 0 {
		if jsonErr := json.Unmarshal(order.Metadata, &meta); jsonErr == nil && meta.UpgradeID != "" {
			err = s.DB.NewSelect().Model(up).Where("id = ?", meta.UpgradeID).Limit(1).Scan(ctx)
			if err == nil {
				return up, nil
			}
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return nil, err
}

// ApplyUpgradeIn applies a plan change inside the caller's transaction.
// With no order (next_cycle) only the plan moves; with an order the plan
// moves and the order's period becomes the licence's period, which is how
// a trial conversion starts its first paid term at trial_end.
//
// The licence lock is the serialisation point: a concurrent payment for
// the same licence waits, then sees the plan the caller just wrote.
func (s *Store) ApplyUpgradeIn(ctx context.Context, tx bun.IDB, upgradeID string, newPlan *model.Plan, order *model.PaymentOrder) error {
	if newPlan == nil {
		return fmt.Errorf("%w: no target plan", ErrUpgradeInvalid)
	}
	up := new(model.LicenseUpgrade)
	if err := tx.NewSelect().Model(up).Where("id = ?", upgradeID).For("UPDATE").Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: upgrade %s not found", ErrUpgradeInvalid, upgradeID)
		}
		return err
	}
	if up.State == model.UpgradeApplied {
		return nil
	}
	if up.State != model.UpgradePending {
		return fmt.Errorf("%w: upgrade %s is %s", ErrUpgradeInvalid, up.ID, up.State)
	}
	lic := new(model.License)
	if err := tx.NewSelect().Model(lic).Where("id = ?", up.LicenseID).For("UPDATE").Scan(ctx); err != nil {
		return err
	}
	if lic.Status != model.StatusActive && lic.Status != model.StatusTrialing && lic.Status != model.StatusPastDue {
		return fmt.Errorf("%w: licence is %s", ErrUpgradeIneligible, lic.Status)
	}
	if newPlan.ID != up.ToPlanID || newPlan.ProductID != lic.ProductID || !newPlan.Active {
		return fmt.Errorf("%w: plan %s does not belong to this licence", ErrUpgradeIneligible, newPlan.ID)
	}
	now := time.Now()
	licUpdate := tx.NewUpdate().Model((*model.License)(nil)).
		Set("plan_id = ?", newPlan.ID).
		Set("updated_at = ?", now).
		Where("id = ?", lic.ID)
	if order != nil {
		if order.PeriodStart == nil || order.PeriodEnd == nil {
			return fmt.Errorf("%w: order %s has no period", ErrUpgradeInvalid, order.ID)
		}
		licUpdate = licUpdate.
			Set("status = ?", model.StatusActive).
			Set("valid_until = ?", *order.PeriodEnd).
			Set("past_due_at = NULL").
			Set("suspended_at = NULL").
			Set("suspended_by = NULL")
	}
	if _, err := licUpdate.Exec(ctx); err != nil {
		return err
	}
	if order != nil {
		if _, err := tx.NewRaw(`UPDATE subscriptions
			SET plan_id = ?, status = 'active', current_period_start = ?, current_period_end = ?, updated_at = ?
			WHERE license_id = ?`, newPlan.ID, *order.PeriodStart, *order.PeriodEnd, now, lic.ID).Exec(ctx); err != nil {
			return err
		}
	} else if _, err := tx.NewRaw(`UPDATE subscriptions SET plan_id = ?, updated_at = ?
		WHERE license_id = ? AND status IN ('active', 'trialing', 'past_due')`, newPlan.ID, now, lic.ID).Exec(ctx); err != nil {
		return err
	}
	_, err := tx.NewRaw(`UPDATE license_upgrades
		SET state = 'applied', applied_at = ?, to_plan_id = ?,
		    from_plan_id = COALESCE(NULLIF(from_plan_id, ''), ?), from_status = COALESCE(NULLIF(from_status, ''), ?),
		    from_valid_until = COALESCE(from_valid_until, ?), from_quantity = ?
		WHERE id = ? AND state = 'pending'`,
		now, newPlan.ID, lic.PlanID, lic.Status, lic.ValidUntil, lic.Quantity, up.ID).Exec(ctx)
	return err
}
```

`newID()` is the package-internal id helper (used throughout `store`). Add imports `encoding/json` and `database/sql` if missing.

- [ ] **Step 4: Run the store tests to pass**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store -run 'TestFindPaidOrder|TestUpgradeRow|TestApplyUpgrade' -v`
Expected: PASS (if `TestUpgradeRowLifecycle` fails on the second `CreatePendingUpgrade` with a unique violation instead of `ErrUpgradeInProgress`, the count check and the insert need to be in the same transaction — they are; check `IsUniqueViolation(err)` mapping).

- [ ] **Step 5: Write the failing engine tests**

```go
// internal/billing/upgrade_test.go
package billing_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/billing"
	"github.com/tabloy/keygate/internal/model"
)

// upgradeHarness is a monthly licence with 20 days left and a yearly plan
// to change to, priced so the arithmetic is checkable by hand.
func upgradeHarness(t *testing.T, mode string) (*harness, *model.License, *model.Plan) {
	t.Helper()
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_200_000, graceDays: 7})
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(20 * 24 * time.Hour)
	lic := h.licence(periodEnd)
	// 10 of 30 days used, so two thirds of the period remains.
	if _, err := h.s.DB.NewRaw(`UPDATE subscriptions SET current_period_start = ?
		WHERE license_id = ?`, periodEnd.Add(-30*24*time.Hour), lic.ID).Exec(h.ctx); err != nil {
		t.Fatal(err)
	}
	target := &model.Plan{
		ProductID: h.product.ID, Name: "Yearly", Slug: "yearly-" + h.product.Slug,
		LicenseType: "subscription", BillingInterval: "year", LicenseModel: "standard",
		MaxActivations: 10, Active: true, ProrationMode: mode,
	}
	if err := h.s.CreatePlan(h.ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.UpsertPlanPrice(h.ctx, target.ID, planPrice("sepay", 3_600_000)); err != nil {
		t.Fatal(err)
	}
	return h, lic, target
}

func planPrice(providerName string, minor int64) store.PlanPriceInput {
	return store.PlanPriceInput{Provider: providerName, Currency: "VND", AmountMinor: minor}
}

// immediate: credit = 20/30 of 1,200,000 = 800,000; new = 20/30 of
// 3,600,000 = 2,400,000; the order is for 1,600,000.
func TestQuoteUpgradeImmediate(t *testing.T) {
	h, lic, target := upgradeHarness(t, "immediate")
	quote, err := h.svc.QuoteUpgrade(h.ctx, billing.UpgradeRequest{LicenseID: lic.ID, PlanID: target.ID})
	if err != nil {
		t.Fatal(err)
	}
	if quote.CreditMinor != 800_000 || quote.NewRemainingMinor != 2_400_000 || quote.AmountMinor != 1_600_000 {
		t.Fatalf("quote = %+v", quote)
	}
	if quote.Currency != "VND" || quote.Mode != "immediate" || quote.ToPlanID != target.ID {
		t.Fatalf("quote = %+v", quote)
	}
	if got := quote.PeriodEnd.Sub(quote.PeriodStart); got != 30*24*time.Hour {
		t.Fatalf("period = %v", got)
	}
}

// The credit follows what the customer paid for the period, not a price
// list that changed after they paid.
func TestQuoteUpgradeUsesThePaidOrderPrice(t *testing.T) {
	h, lic, target := upgradeHarness(t, "immediate")
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(20 * 24 * time.Hour)
	if _, err := h.s.UpsertPlanPrice(h.ctx, h.plan.ID, planPrice("sepay", 1_200_000)); err != nil {
		t.Fatal(err)
	}
	order, err := h.s.CreatePaymentOrder(h.ctx, store.CreateOrderInput{
		ProductID: h.product.ID, PlanID: h.plan.ID, LicenseID: lic.ID, Email: lic.Email, Kind: "new",
		Quantity: 1, SubtotalMinor: 600_000, TotalMinor: 600_000, Currency: "VND",
		Provider: "sepay", PeriodStart: timePtr(periodEnd.Add(-30 * 24 * time.Hour)), PeriodEnd: &periodEnd,
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.NewRaw("UPDATE payment_orders SET status = 'paid' WHERE id = ?", order.ID).Exec(h.ctx); err != nil {
		t.Fatal(err)
	}
	quote, err := h.svc.QuoteUpgrade(h.ctx, billing.UpgradeRequest{LicenseID: lic.ID, PlanID: target.ID})
	if err != nil {
		t.Fatal(err)
	}
	// 20/30 of the 600,000 actually paid.
	if quote.CreditMinor != 400_000 {
		t.Fatalf("credit = %d, want the paid 400,000", quote.CreditMinor)
	}
}

// immediate writes one order for the difference, one upgrade row linked to
// it, and no plan change yet: the plan moves when the money lands.
func TestCreateUpgradeOrderImmediate(t *testing.T) {
	h, lic, target := upgradeHarness(t, "immediate")
	res, err := h.svc.CreateUpgradeOrder(h.ctx, billing.UpgradeRequest{LicenseID: lic.ID, PlanID: target.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Order == nil || res.Order.Kind != "upgrade" || res.Order.TotalMinor != 1_600_000 {
		t.Fatalf("order = %+v", res.Order)
	}
	if res.Order.SubtotalMinor != 2_400_000 || res.Order.DiscountMinor != 800_000 {
		t.Fatalf("totals = %+v", res.Order)
	}
	if res.Upgrade == nil || res.Upgrade.PaymentOrderID != res.Order.ID || res.Upgrade.State != model.UpgradePending {
		t.Fatalf("upgrade = %+v", res.Upgrade)
	}
	if res.Upgrade.FromPlanID != h.plan.ID || res.Upgrade.FromQuantity != 1 {
		t.Fatalf("snapshot = %+v", res.Upgrade)
	}
	if got, _ := h.reload(t, lic.ID); got.PlanID != h.plan.ID {
		t.Fatalf("the plan moved before the payment: %s", got.PlanID)
	}
	if got := metadata(t, res.Order)["upgrade_id"]; got != res.Upgrade.ID {
		t.Fatalf("metadata upgrade_id = %v", got)
	}
}

// A retried request returns the open change rather than a second order.
func TestCreateUpgradeOrderIsIdempotent(t *testing.T) {
	h, lic, target := upgradeHarness(t, "immediate")
	first, err := h.svc.CreateUpgradeOrder(h.ctx, billing.UpgradeRequest{LicenseID: lic.ID, PlanID: target.ID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.svc.CreateUpgradeOrder(h.ctx, billing.UpgradeRequest{LicenseID: lic.ID, PlanID: target.ID})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if second.Order.ID != first.Order.ID {
		t.Fatalf("retry minted a second order: %s vs %s", second.Order.ID, first.Order.ID)
	}
	if n := len(h.orders(lic.ID)); n != 1 {
		t.Fatalf("%d orders", n)
	}
}

// next_cycle changes the plan at once, charges nothing, and leaves the
// period alone: the next renewal order carries the new price.
func TestCreateUpgradeOrderNextCycle(t *testing.T) {
	h, lic, target := upgradeHarness(t, "next_cycle")
	res, err := h.svc.CreateUpgradeOrder(h.ctx, billing.UpgradeRequest{LicenseID: lic.ID, PlanID: target.ID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Order != nil || res.Attempt != nil {
		t.Fatalf("next_cycle wrote an order: %+v", res.Order)
	}
	got, sub := h.reload(t, lic.ID)
	if got.PlanID != target.ID {
		t.Fatalf("plan = %s, want %s", got.PlanID, target.ID)
	}
	if got.ValidUntil == nil || !got.ValidUntil.Equal(*res.Quote.PeriodEnd) {
		t.Fatalf("next_cycle moved valid_until: %v", got.ValidUntil)
	}
	if sub.PlanID != target.ID {
		t.Fatalf("subscription plan = %s", sub.PlanID)
	}
	if !h.sawEvent(model.EventPlanChanged) {
		t.Fatalf("events = %+v", h.events.got)
	}
	if len(h.prov.Calls) != 0 {
		t.Fatalf("next_cycle asked for a payment instruction: %+v", h.prov.Calls)
	}
}

// A change the credit fully covers is a downgrade: phase 1b refunds
// nothing, so it is refused with a clear error instead of a 0 VND order.
func TestCreateUpgradeOrderRefusesFreeChange(t *testing.T) {
	h, lic, target := upgradeHarness(t, "immediate")
	// The new plan is cheaper than the credit for the same period.
	if _, err := h.s.UpsertPlanPrice(h.ctx, target.ID, planPrice("sepay", 600_000)); err != nil {
		t.Fatal(err)
	}
	_, err := h.svc.CreateUpgradeOrder(h.ctx, billing.UpgradeRequest{LicenseID: lic.ID, PlanID: target.ID})
	if !errors.Is(err, billing.ErrUpgradeNotChargeable) {
		t.Fatalf("err = %v", err)
	}
	if n := len(h.orders(lic.ID)); n != 0 {
		t.Fatalf("%d orders", n)
	}
}

// A perpetual licence is not a subscription and cannot be prorated.
func TestUpgradeRefusesNonSubscriptionPlan(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_200_000})
	lic := h.licence(time.Now().UTC().Add(20 * 24 * time.Hour))
	// A trial plan in the same product: time-based, but not for sale to a
	// subscription through proration.
	trial := &model.Plan{
		ProductID: h.product.ID, Name: "Trial", Slug: "trial-" + h.product.Slug,
		LicenseType: "trial", LicenseModel: "standard", MaxActivations: 3, Active: true,
	}
	if err := h.s.CreatePlan(h.ctx, trial); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.UpsertPlanPrice(h.ctx, trial.ID, planPrice("sepay", 100_000)); err != nil {
		t.Fatal(err)
	}
	_, err := h.svc.CreateUpgradeOrder(h.ctx, billing.UpgradeRequest{LicenseID: lic.ID, PlanID: trial.ID})
	if !errors.Is(err, billing.ErrLicenseNotEligible) {
		t.Fatalf("err = %v", err)
	}
}

// The same plan twice is not a change.
func TestUpgradeRefusesSamePlan(t *testing.T) {
	h, lic, _ := upgradeHarness(t, "immediate")
	_, err := h.svc.CreateUpgradeOrder(h.ctx, billing.UpgradeRequest{LicenseID: lic.ID, PlanID: h.plan.ID})
	if !errors.Is(err, billing.ErrLicenseNotEligible) {
		t.Fatalf("err = %v", err)
	}
}

func timePtr(t time.Time) *time.Time { return &t }
```

with `store` imported in `upgrade_test.go`.

- [ ] **Step 6: Run them to fail**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/billing -run 'TestQuoteUpgrade|TestCreateUpgrade|TestUpgradeRefuses' -v`
Expected: FAIL — `h.svc.QuoteUpgrade undefined`.

- [ ] **Step 7: Write `upgrade.go`**

```go
// internal/billing/upgrade.go
package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/checkout"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/store"
)

var (
	// ErrLicenseNotEligible: this licence cannot change to that plan —
	// wrong product, plan not for sale, a trial, the plan it is on, or a
	// period that already ended.
	ErrLicenseNotEligible = errors.New("licence is not eligible for this plan change")
	// ErrUpgradeNotChargeable: the credit covers the whole change. That is
	// a downgrade, and phase 1b issues no credit or refund.
	ErrUpgradeNotChargeable = errors.New("the unused time covers the whole change")
	// ErrUpgradeInProgress: an earlier change is still waiting for its
	// payment.
	ErrUpgradeInProgress = errors.New("a plan change is already open for this licence")
)

// UpgradeRequest is one plan change asked for by the portal or an admin.
type UpgradeRequest struct {
	LicenseID string
	PlanID    string
	// Provider empty uses the licence's own rail: a customer does not
	// silently change rails mid-subscription.
	Provider string
	// Email empty uses the licence's email.
	Email string
	Buyer provider.Buyer
	CreatedByKeyID string
}

// UpgradeQuote is what the change costs today, before anything is written.
type UpgradeQuote struct {
	LicenseID         string
	ProductID         string
	Email             string
	FromPlanID        string
	ToPlanID          string
	PeriodStart       time.Time
	PeriodEnd         time.Time
	CurrentPriceMinor int64
	NewPriceMinor     int64
	NewRemainingMinor int64
	CreditMinor       int64
	AmountMinor       int64
	Currency          string
	CurrencyExp       int16
	Mode              string
}

// UpgradeResult is the quote and what was written for it. Order is nil in
// next_cycle mode, where nothing is charged.
type UpgradeResult struct {
	Quote   *UpgradeQuote
	Upgrade *model.LicenseUpgrade
	Order   *model.PaymentOrder
	Attempt *model.PaymentAttempt
}

// QuoteUpgrade prices a plan change without writing anything. The portal
// shows it before asking for a payment.
//
// The formula is the contract's: both prices are prorated over the current
// period, so the charge is the remainder of the new plan less the unused
// part of what was already paid. A change of billing interval (monthly to
// yearly) therefore prices the rest of a *month* at the yearly rate; that
// is a product question for the portal (Module C), which should offer
// interval changes as a new period rather than an upgrade.
func (s *Service) QuoteUpgrade(ctx context.Context, req UpgradeRequest) (*UpgradeQuote, error) {
	now := time.Now().UTC()
	lic, err := s.store.FindLicenseByID(ctx, req.LicenseID)
	if err != nil {
		return nil, fmt.Errorf("%w: licence not found", ErrLicenseNotEligible)
	}
	target, err := s.store.FindPlanByID(ctx, req.PlanID)
	if err != nil {
		return nil, fmt.Errorf("%w: plan not found", ErrLicenseNotEligible)
	}
	if target.ID == lic.PlanID {
		return nil, fmt.Errorf("%w: the licence is already on that plan", ErrLicenseNotEligible)
	}
	if !target.Active || target.ProductID != lic.ProductID {
		return nil, fmt.Errorf("%w: plan is not for sale on this product", ErrLicenseNotEligible)
	}
	// Only a time-based plan can be entered mid-period: a perpetual
	// plan's price buys updates for a year, not the rest of this period.
	if target.LicenseType != "subscription" {
		return nil, fmt.Errorf("%w: only subscription plans can be prorated", ErrLicenseNotEligible)
	}
	if lic.Status != model.StatusActive && lic.Status != model.StatusTrialing && lic.Status != model.StatusPastDue {
		return nil, fmt.Errorf("%w: licence is %s", ErrLicenseNotEligible, lic.Status)
	}
	sub, err := s.store.FindSubscriptionByLicense(ctx, lic.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: licence has no subscription period", ErrLicenseNotEligible)
	}
	if sub.CurrentPeriodStart == nil || sub.CurrentPeriodEnd == nil || !now.Before(*sub.CurrentPeriodEnd) {
		return nil, fmt.Errorf("%w: the current period has ended", ErrLicenseNotEligible)
	}
	rail := req.Provider
	if rail == "" {
		rail = lic.PaymentProvider
	}
	if !isBillingRail(rail) {
		return nil, fmt.Errorf("%w: licence is not billed by a Vietnamese rail", ErrLicenseNotEligible)
	}
	mode := target.ProrationMode
	if mode != "next_cycle" {
		mode = "immediate"
	}
	currentPrice := s.currentPeriodPrice(ctx, lic, rail)
	newPrice, err := s.store.ResolvePlanPrice(ctx, target.ID, rail, checkout.SettlementCurrency, target.BillingInterval)
	if err != nil || newPrice.AmountMinor <= 0 {
		return nil, fmt.Errorf("%w: the new plan has no price for %s", ErrLicenseNotEligible, rail)
	}
	credit := checkout.ProrateMinor(currentPrice, *sub.CurrentPeriodStart, *sub.CurrentPeriodEnd, now)
	remaining := checkout.ProrateMinor(newPrice.AmountMinor, *sub.CurrentPeriodStart, *sub.CurrentPeriodEnd, now)
	return &UpgradeQuote{
		LicenseID: lic.ID, ProductID: lic.ProductID, Email: lic.Email,
		FromPlanID: lic.PlanID, ToPlanID: target.ID,
		PeriodStart: *sub.CurrentPeriodStart, PeriodEnd: *sub.CurrentPeriodEnd,
		CurrentPriceMinor: currentPrice, NewPriceMinor: newPrice.AmountMinor,
		NewRemainingMinor: remaining, CreditMinor: credit,
		AmountMinor: checkout.UpgradeAmount(remaining, credit),
		Currency:    newPrice.Currency, CurrencyExp: newPrice.CurrencyExponent, Mode: mode,
	}, nil
}

// CreateUpgradeOrder opens the plan change. immediate: one upgrade order
// for the prorated difference. next_cycle: the plan changes now, nothing
// is charged, and the next renewal carries the new price.
func (s *Service) CreateUpgradeOrder(ctx context.Context, req UpgradeRequest) (*UpgradeResult, error) {
	quote, err := s.QuoteUpgrade(ctx, req)
	if err != nil {
		return nil, err
	}
	// A retry of the same request finds the open change and hands it back:
	// two orders for one change is two charges.
	if existing, err := s.store.FindPendingUpgradeForLicense(ctx, quote.LicenseID); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.ToPlanID != quote.ToPlanID {
			return nil, fmt.Errorf("%w: a change to another plan is open", ErrUpgradeInProgress)
		}
		return s.resumeUpgrade(ctx, existing, quote)
	}
	lic, err := s.store.FindLicenseByID(ctx, quote.LicenseID)
	if err != nil {
		return nil, err
	}
	upgrade := &model.LicenseUpgrade{
		ID: store.NewID(), LicenseID: lic.ID, ToPlanID: quote.ToPlanID, State: model.UpgradePending,
		FromPlanID: lic.PlanID, FromStatus: lic.Status, FromValidUntil: lic.ValidUntil,
		FromUpdatesUntil: lic.UpdatesUntil, FromUpdatesTermsSet: lic.UpdatesTermsSet,
		FromPaymentProvider: lic.PaymentProvider, FromCanceledAt: lic.CanceledAt, FromQuantity: lic.Quantity,
	}
	if err := s.store.CreatePendingUpgrade(ctx, upgrade); err != nil {
		if errors.Is(err, store.ErrUpgradeInProgress) {
			return nil, ErrUpgradeInProgress
		}
		return nil, err
	}
	if quote.Mode == "next_cycle" {
		return s.applyNextCycle(ctx, upgrade, quote)
	}
	if quote.AmountMinor <= 0 {
		// Unwind the row: nothing will pay for it.
		_ = s.store.EndPendingUpgrade(ctx, upgrade.ID, "void", "the credit covers the whole change")
		return nil, ErrUpgradeNotChargeable
	}
	email := req.Email
	if email == "" {
		email = lic.Email
	}
	res, err := s.checkout.CreateOrder(ctx, checkout.CreateOrderInput{
		PlanID: quote.ToPlanID, Email: email, Provider: req.Provider, Quantity: 1,
		Kind: "upgrade", LicenseID: lic.ID, PeriodStart: &quote.PeriodStart, PeriodEnd: &quote.PeriodEnd,
		SubtotalMinor: quote.NewRemainingMinor, CreditMinor: quote.CreditMinor,
		Buyer: req.Buyer, CreatedByKeyID: req.CreatedByKeyID,
		Metadata: map[string]any{
			"upgrade_id":             upgrade.ID,
			"from_plan_id":           quote.FromPlanID,
			"proration_mode":         quote.Mode,
			"proration_credit_minor": quote.CreditMinor,
		},
	})
	if err != nil {
		_ = s.store.EndPendingUpgrade(ctx, upgrade.ID, "void", "the order could not be created")
		return nil, err
	}
	if err := s.store.LinkUpgradeOrder(ctx, upgrade.ID, res.Order.ID); err != nil {
		return nil, err
	}
	upgrade.PaymentOrderID = res.Order.ID
	return &UpgradeResult{Quote: quote, Upgrade: upgrade, Order: res.Order, Attempt: res.Attempt}, nil
}

// resumeUpgrade returns the order already open for a pending change, or
// completes a next_cycle change whose apply step was interrupted.
func (s *Service) resumeUpgrade(ctx context.Context, upgrade *model.LicenseUpgrade, quote *UpgradeQuote) (*UpgradeResult, error) {
	if upgrade.PaymentOrderID != "" {
		order, err := s.store.GetPaymentOrderForUpdate(ctx, nil, upgrade.PaymentOrderID)
		if err != nil {
			return nil, err
		}
		if order.Status != "pending" {
			return nil, fmt.Errorf("%w: the open change's order is %s", ErrUpgradeInProgress, order.Status)
		}
		latest, attemptErr := s.store.GetLatestPaymentAttempt(ctx, order.ID)
		if attemptErr != nil && !errors.Is(attemptErr, sql.ErrNoRows) {
			return nil, attemptErr
		}
		return &UpgradeResult{Quote: quote, Upgrade: upgrade, Order: order, Attempt: latest}, nil
	}
	return s.applyNextCycle(ctx, upgrade, quote)
}

// applyNextCycle changes the plan without money: the customer keeps the
// period they paid for, at the new plan's limits, and the next renewal
// order quotes the new price.
func (s *Service) applyNextCycle(ctx context.Context, upgrade *model.LicenseUpgrade, quote *UpgradeQuote) (*UpgradeResult, error) {
	target, err := s.store.FindPlanByID(ctx, quote.ToPlanID)
	if err != nil {
		return nil, err
	}
	err = s.store.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		return s.store.ApplyUpgradeIn(ctx, tx, upgrade.ID, target, nil)
	})
	if err != nil {
		if errors.Is(err, store.ErrUpgradeIneligible) || errors.Is(err, store.ErrUpgradeInvalid) {
			return nil, fmt.Errorf("%w: %v", ErrLicenseNotEligible, err)
		}
		return nil, err
	}
	upgrade.State = model.UpgradeApplied
	s.dispatch(ctx, &store.BillingSubscription{ProductID: quote.ProductID}, model.EventPlanChanged, map[string]any{
		"license_id": quote.LicenseID, "email": quote.Email,
		"from_plan_id": quote.FromPlanID, "to_plan_id": quote.ToPlanID,
		"reason": "next_cycle", "period_end": quote.PeriodEnd,
	})
	return &UpgradeResult{Quote: quote, Upgrade: upgrade}, nil
}

// currentPeriodPrice is what the customer paid for the period they are in:
// the paid order when there is one, otherwise today's list price. Using
// the paid amount keeps a mid-period price change out of the credit.
func (s *Service) currentPeriodPrice(ctx context.Context, lic *model.License, rail string) int64 {
	if sub, err := s.store.FindSubscriptionByLicense(ctx, lic.ID); err == nil && sub.CurrentPeriodEnd != nil {
		if order, err := s.store.FindPaidOrderForPeriod(ctx, lic.ID, *sub.CurrentPeriodEnd); err == nil && order != nil {
			return order.SubtotalMinor
		}
	}
	plan, err := s.store.FindPlanByID(ctx, lic.PlanID)
	if err != nil {
		return 0
	}
	price, err := s.store.ResolvePlanPrice(ctx, plan.ID, rail, checkout.SettlementCurrency, plan.BillingInterval)
	if err != nil {
		return 0
	}
	return price.AmountMinor
}

// isBillingRail reports whether a provider is one this engine bills.
func isBillingRail(name string) bool {
	return name == "sepay" || name == "payos"
}
```

`EndPendingUpgrade` must exist in `store/billing.go`:

```go
// EndPendingUpgrade closes a pending change that will never apply, with
// the reason an operator reads later.
func (s *Store) EndPendingUpgrade(ctx context.Context, upgradeID, state, reason string) error {
	_, err := s.DB.NewRaw(`UPDATE license_upgrades
		SET state = ?, end_reason = ?, ended_at = ?
		WHERE id = ? AND state = 'pending'`, state, reason, time.Now(), upgradeID).Exec(ctx)
	return err
}
```
`state` is one of `model.UpgradeExpired`, `model.UpgradeVoid` (both in the migration's CHECK).

- [ ] **Step 8: Run the package to pass**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/billing -v`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
go build ./... && go vet ./internal/billing ./internal/store
git add internal/billing internal/store/billing.go internal/store/billing_test.go
git commit -m "billing: prorated plan changes with next_cycle mode (phase 1b module A)"
```

<!-- APPEND -->
### Task 9: Trial conversion — opt in per product, charge the first period at `trial_end`

**Files:**
- Create: `internal/billing/trial.go`
- Modify: `internal/billing/upgrade.go` (extract `openChange`; `CreateUpgradeOrder` calls it)
- Modify: `internal/billing/service.go` (`processRow`'s trial branch)
- Modify: `internal/payment/checkout/service.go` (`CreateOrderInput.ExpiresAt`)
- Modify: `internal/billing/fixtures_test.go` (`h.trialLicence`, `setSetting`)
- Test: `internal/billing/trial_test.go`

**Interfaces:**
- Consumes: `store.GetSetting`, `store.FindPlanByID`, `store.ResolvePlanPrice`, `store.FindPendingUpgradeForLicense`, `s.renewalLead`, `s.openChange`.
- Produces:
  - `s.convertTrial(ctx, row, now) error`
  - `s.trialConversionPlan(ctx, productID) string` (reads `billing_trial_conversion_plan:<product_id>`)
  - `s.openChange(ctx, openChangeIn) (*UpgradeResult, error)`
  - `checkout.CreateOrderInput.ExpiresAt` (zero means "the default")

A trial is not renewed; it is either converted to a paid plan or it ends. Conversion is **opt-in per product** — the platform has always let a trial simply lapse, and turning that into an invoice for every install on upgrade day would be a surprise. The operator sets `billing_trial_conversion_plan:<product_id>` to a subscription plan; the engine then opens the same kind of `upgrade` order a plan change uses, with the new period starting exactly at `trial_end`, and settlement's upgrade path puts the licence on the paid plan.

- [ ] **Step 1: Add the harness pieces**

```go
// trialLicence makes a trial licence whose trial ends at trialEnd, with its
// own trial plan in the harness's product.
func (h *harness) trialLicence(t *testing.T, trialEnd time.Time) (*model.License, *model.Plan) {
	t.Helper()
	plan := &model.Plan{
		ProductID: h.product.ID, Name: "Trial", Slug: "trial-" + h.suffix,
		LicenseType: "trial", LicenseModel: "standard", MaxActivations: 3, Active: true,
	}
	if err := h.s.CreatePlan(h.ctx, plan); err != nil {
		t.Fatal(err)
	}
	lic := &model.License{
		ProductID: h.product.ID, PlanID: plan.ID, Email: h.email("trial"),
		LicenseKey: "KEY-TRIAL-" + h.suffix, Status: model.StatusTrialing, Quantity: 1,
	}
	if err := h.s.CreateLicenseWithSubscription(h.ctx, lic, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.NewRaw(`UPDATE licenses SET valid_until = ? WHERE id = ?`, trialEnd, lic.ID).Exec(h.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.DB.NewRaw(`UPDATE subscriptions SET status = 'trialing', trial_start = ?, trial_end = ?
		WHERE license_id = ?`, trialEnd.Add(-14*24*time.Hour), trialEnd, lic.ID).Exec(h.ctx); err != nil {
		t.Fatal(err)
	}
	return lic, plan
}

// paidPlan adds a subscription plan to the harness's product and prices it.
func (h *harness) paidPlan(t *testing.T, name string, minor int64) *model.Plan {
	t.Helper()
	plan := &model.Plan{
		ProductID: h.product.ID, Name: name, Slug: strings.ToLower(name) + "-" + h.suffix,
		LicenseType: "subscription", BillingInterval: "month", LicenseModel: "standard",
		MaxActivations: 10, Active: true,
	}
	if err := h.s.CreatePlan(h.ctx, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.UpsertPlanPrice(h.ctx, plan.ID, store.PlanPriceInput{
		Provider: "sepay", Currency: "VND", AmountMinor: minor,
	}); err != nil {
		t.Fatal(err)
	}
	return plan
}
```

with `strings` imported.

- [ ] **Step 2: Write the failing tests**

```go
// internal/billing/trial_test.go
package billing_test

import (
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// The conversion order starts the paid period exactly when the trial ends,
// charges one full period, and dies with the trial.
func TestTrialConversionCreatesUpgradeOrder(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	trialEnd := time.Now().UTC().Truncate(time.Second).Add(5 * 24 * time.Hour)
	lic, _ := h.trialLicence(t, trialEnd)
	paid := h.paidPlan(t, "Monthly", 990_000)
	h.setProductSetting(t, "billing_trial_conversion_plan", paid.ID)

	if err := h.svc.RunOnce(h.ctx); err != nil {
		t.Fatal(err)
	}
	orders := h.orders(lic.ID)
	if len(orders) != 1 {
		t.Fatalf("%d orders", len(orders))
	}
	o := orders[0]
	if o.Kind != "upgrade" || o.TotalMinor != 990_000 || o.SubtotalMinor != 990_000 || o.DiscountMinor != 0 {
		t.Fatalf("order = kind %s total %d subtotal %d discount %d", o.Kind, o.TotalMinor, o.SubtotalMinor, o.DiscountMinor)
	}
	if o.PeriodStart == nil || !o.PeriodStart.Equal(trialEnd) {
		t.Fatalf("period_start = %v, want the trial end %v", o.PeriodStart, trialEnd)
	}
	if o.PeriodEnd == nil || !o.PeriodEnd.Equal(trialEnd.Add(30*24*time.Hour)) {
		t.Fatalf("period_end = %v", o.PeriodEnd)
	}
	// A conversion cannot be paid after the trial is over.
	if !o.ExpiresAt.Equal(trialEnd) {
		t.Fatalf("expires_at = %v, want the trial end", o.ExpiresAt)
	}
	up, err := h.s.FindPendingUpgradeForLicense(h.ctx, lic.ID)
	if err != nil || up == nil || up.PaymentOrderID != o.ID {
		t.Fatalf("upgrade = %+v err=%v", up, err)
	}
	if up.FromPlanID != lic.PlanID {
		t.Fatalf("from plan = %s, want the trial plan %s", up.FromPlanID, lic.PlanID)
	}
	// The licence is still a trial until the money lands, and the ladder
	// does not fire for a trial.
	if got, _ := h.reload(t, lic.ID); got.Status != model.StatusTrialing {
		t.Fatalf("status = %s", got.Status)
	}
	if len(h.mail.sent) != 0 {
		t.Fatalf("trial conversion mailed the renewal ladder: %+v", h.mail.sent)
	}
}

// No configured conversion plan: the trial is left to the existing trial
// expiry rules, exactly as before phase 1b.
func TestTrialConversionIsOptIn(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	lic, _ := h.trialLicence(t, time.Now().UTC().Truncate(time.Second).Add(3*24*time.Hour))
	if err := h.svc.RunOnce(h.ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(h.orders(lic.ID)); n != 0 {
		t.Fatalf("%d orders without a conversion plan", n)
	}
	if len(h.mail.sent) != 0 {
		t.Fatalf("mails = %+v", h.mail.sent)
	}
}

// The order is opened inside the same lead window as a renewal, not before.
func TestTrialConversionWaitsForTheLeadWindow(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	trialEnd := time.Now().UTC().Truncate(time.Second).Add(9 * 24 * time.Hour)
	lic, _ := h.trialLicence(t, trialEnd)
	h.setProductSetting(t, "billing_trial_conversion_plan", h.paidPlan(t, "Monthly", 990_000).ID)

	if err := h.svc.RunOnceAt(h.ctx, trialEnd.Add(-7*24*time.Hour-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := len(h.orders(lic.ID)); n != 0 {
		t.Fatalf("%d orders more than 7 days out", n)
	}
	if err := h.svc.RunOnceAt(h.ctx, trialEnd.Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := len(h.orders(lic.ID)); n != 1 {
		t.Fatalf("%d orders at the lead window", n)
	}
	// A second run inside the window reuses the open order.
	if err := h.svc.RunOnceAt(h.ctx, trialEnd.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := len(h.orders(lic.ID)); n != 1 {
		t.Fatalf("%d orders after two runs", n)
	}
}

// A conversion plan with no price is a configuration error, not a crash and
// not a 0 VND order: the run logs it and the trial keeps its old ending.
func TestTrialConversionSkipsUnpricedPlan(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	lic, _ := h.trialLicence(t, time.Now().UTC().Truncate(time.Second).Add(3*24*time.Hour))
	unpriced := &model.Plan{
		ProductID: h.product.ID, Name: "Unpriced", Slug: "unpriced-" + h.suffix,
		LicenseType: "subscription", BillingInterval: "month", LicenseModel: "standard",
		MaxActivations: 5, Active: true,
	}
	if err := h.s.CreatePlan(h.ctx, unpriced); err != nil {
		t.Fatal(err)
	}
	h.setProductSetting(t, "billing_trial_conversion_plan", unpriced.ID)
	if err := h.svc.RunOnce(h.ctx); err != nil {
		t.Fatalf("an unpriced conversion plan failed the sweep: %v", err)
	}
	if n := len(h.orders(lic.ID)); n != 0 {
		t.Fatalf("%d orders for an unpriced plan", n)
	}
}
```

`h.setProductSetting` is the helper Task 6 added; the trial tests only need it.

- [ ] **Step 3: Run them to fail**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/billing -run 'TestTrialConversion' -v`
Expected: FAIL — no conversion order is ever written.

- [ ] **Step 4: Let the caller pin the order's expiry**

`internal/payment/checkout/service.go`:

```go
	// ExpiresAt overrides when the order can no longer be paid. Zero means
	// the default: 24 hours for a new licence, period_end + the plan's
	// grace days for a renewal or upgrade. A trial conversion passes the
	// trial's end, because paying after it must not resurrect the licence.
	ExpiresAt time.Time
```

and in `createOrder`, after the kind branch:

```go
	if !in.ExpiresAt.IsZero() {
		expiresAt = in.ExpiresAt
		if !expiresAt.After(time.Now()) {
			return nil, fmt.Errorf("%w: the order would expire before it could be paid", ErrPlanNotSellable)
		}
	}
```

- [ ] **Step 5: Extract `openChange` from `CreateUpgradeOrder`**

`internal/billing/upgrade.go`. `CreateUpgradeOrder` keeps its decisions and hands the writing to one helper, which the trial path then reuses unchanged:

```go
// openChangeIn is everything the two plan-change writers (an upgrade and a
// trial conversion) have already decided.
type openChangeIn struct {
	lic        *model.License
	target     *model.Plan
	periodFrom time.Time
	periodTo   time.Time
	subtotal   int64
	credit     int64
	expiresAt  time.Time // zero: period_to + grace
	metadata   map[string]any
	provider   string
	email      string
	buyer      provider.Buyer
	createdBy  string
	quote      *UpgradeQuote
}

// openChange writes the pending plan change, the order that pays for it,
// and the link between them. One helper for both callers so the row, the
// order and the link cannot drift apart.
func (s *Service) openChange(ctx context.Context, in openChangeIn) (*UpgradeResult, error) {
	upgrade := &model.LicenseUpgrade{
		ID: store.NewID(), LicenseID: in.lic.ID, ToPlanID: in.target.ID, State: model.UpgradePending,
		FromPlanID: in.lic.PlanID, FromStatus: in.lic.Status, FromValidUntil: in.lic.ValidUntil,
		FromUpdatesUntil: in.lic.UpdatesUntil, FromUpdatesTermsSet: in.lic.UpdatesTermsSet,
		FromPaymentProvider: in.lic.PaymentProvider, FromCanceledAt: in.lic.CanceledAt,
		FromQuantity: in.lic.Quantity,
	}
	if err := s.store.CreatePendingUpgrade(ctx, upgrade); err != nil {
		if errors.Is(err, store.ErrUpgradeInProgress) {
			return nil, ErrUpgradeInProgress
		}
		return nil, err
	}
	if in.quote != nil && in.quote.Mode == "next_cycle" {
		return s.applyNextCycle(ctx, upgrade, in.quote)
	}
	if in.subtotal <= 0 || in.subtotal-in.credit <= 0 {
		_ = s.store.EndPendingUpgrade(ctx, upgrade.ID, model.UpgradeVoid, "nothing to charge")
		return nil, ErrUpgradeNotChargeable
	}
	metadata := map[string]any{"upgrade_id": upgrade.ID, "from_plan_id": in.lic.PlanID}
	for k, v := range in.metadata {
		metadata[k] = v
	}
	res, err := s.checkout.CreateOrder(ctx, checkout.CreateOrderInput{
		PlanID: in.target.ID, Email: in.email, Provider: in.provider, Quantity: 1,
		Kind: "upgrade", LicenseID: in.lic.ID,
		PeriodStart: &in.periodFrom, PeriodEnd: &in.periodTo,
		SubtotalMinor: in.subtotal, CreditMinor: in.credit,
		ExpiresAt: in.expiresAt, Buyer: in.buyer, CreatedByKeyID: in.createdBy,
		Metadata: metadata,
	})
	if err != nil {
		_ = s.store.EndPendingUpgrade(ctx, upgrade.ID, model.UpgradeVoid, "the order could not be created")
		return nil, err
	}
	if err := s.store.LinkUpgradeOrder(ctx, upgrade.ID, res.Order.ID); err != nil {
		return nil, err
	}
	upgrade.PaymentOrderID = res.Order.ID
	return &UpgradeResult{Quote: in.quote, Upgrade: upgrade, Order: res.Order, Attempt: res.Attempt}, nil
}
```

and `CreateUpgradeOrder`'s tail becomes:

```go
	email := req.Email
	if email == "" {
		email = lic.Email
	}
	return s.openChange(ctx, openChangeIn{
		lic: lic, target: target, periodFrom: quote.PeriodStart, periodTo: quote.PeriodEnd,
		subtotal: quote.NewRemainingMinor, credit: quote.CreditMinor,
		provider: req.Provider, email: email, buyer: req.Buyer, createdBy: req.CreatedByKeyID,
		metadata: map[string]any{
			"proration_mode":         quote.Mode,
			"proration_credit_minor": quote.CreditMinor,
		},
		quote: quote,
	})
```

`CreateUpgradeOrder` must load the target plan before this call (`target, err := s.store.FindPlanByID(ctx, quote.ToPlanID)`); the `AmountMinor <= 0` guard it had is now in `openChange`, so delete it there and keep `quote.AmountMinor` only for the portal.

- [ ] **Step 6: Write `trial.go`**

```go
// internal/billing/trial.go
package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// trialConversionSetting names the plan a product's trials convert to, per
// product: billing_trial_conversion_plan:<product_id>. Unset means trials
// end, which is the platform's behaviour before phase 1b.
const trialConversionSetting = "billing_trial_conversion_plan"

// convertTrial opens the paid order that turns a trial into a subscription.
// It runs inside the lead window, before the trial ends, and does nothing
// at all unless the product names a conversion plan.
func (s *Service) convertTrial(ctx context.Context, row *store.BillingSubscription, now time.Time, locale string) error {
	if row.TrialEnd == nil {
		return nil
	}
	trialEnd := *row.TrialEnd
	if !now.Before(trialEnd) {
		return nil // over: the trial sweep owns the ending
	}
	if trialEnd.Sub(now) > s.renewalLead(ctx) {
		return nil
	}
	planID := s.trialConversionPlan(ctx, row.ProductID)
	if planID == "" {
		return nil
	}
	// One change at a time: a pending conversion (or an upgrade) means the
	// customer already has something to pay.
	if open, err := s.store.FindPendingUpgradeForLicense(ctx, row.LicenseID); err != nil {
		return err
	} else if open != nil {
		return nil
	}
	target, err := s.store.FindPlanByID(ctx, planID)
	if err != nil || !target.Active || target.ProductID != row.ProductID || target.LicenseType != "subscription" {
		s.log.Warn("billing: the trial conversion plan is not usable",
			"product", row.ProductID, "plan", planID, "error", err)
		return nil
	}
	price, err := s.store.ResolvePlanPrice(ctx, target.ID, row.PaymentProvider, checkout.SettlementCurrency, target.BillingInterval)
	if err != nil || price.AmountMinor <= 0 {
		s.log.Warn("billing: the trial conversion plan has no price",
			"product", row.ProductID, "plan", target.ID, "provider", row.PaymentProvider, "error", err)
		return nil
	}
	lic, err := s.store.FindLicenseByID(ctx, row.LicenseID)
	if err != nil {
		return err
	}
	interval := IntervalDuration(target.BillingInterval)
	res, err := s.openChange(ctx, openChangeIn{
		lic: lic, target: target,
		periodFrom: trialEnd, periodTo: trialEnd.Add(interval),
		subtotal: price.AmountMinor, credit: 0,
		// The order dies with the trial: a payment after it must not
		// resurrect a licence the trial sweep already expired.
		expiresAt: trialEnd,
		provider:  row.PaymentProvider,
		email:     row.Email,
		metadata:  map[string]any{"reason": "trial_conversion", "trial_end": trialEnd},
	})
	if err != nil {
		s.log.Warn("billing: trial conversion order not created", "license", row.LicenseID, "error", err)
		return nil
	}
	s.log.Info("billing: trial conversion order created",
		"license", row.LicenseID, "plan", target.ID, "order", res.Order.Code, "trial_end", trialEnd)
	return nil
}

// trialConversionPlan reads the product's configured conversion plan, or ""
// when the operator has not opted in.
func (s *Service) trialConversionPlan(ctx context.Context, productID string) string {
	value, err := s.store.GetSetting(ctx, trialConversionSetting+":"+productID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.log.Warn("billing: could not read the trial conversion plan", "product", productID, "error", err)
		}
		return ""
	}
	return strings.TrimSpace(value)
}
```

with `store`, `checkout` and `strings` imported and `locale` unused — drop the parameter and call `s.convertTrial(ctx, row, now)` from `processRow` (replace the early return):

```go
	if row.PlanLicenseType != "subscription" {
		return s.convertTrial(ctx, row, now)
	}
```

- [ ] **Step 7: Run the package to pass**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/billing -v`
Expected: PASS. `TestRunOnceCreatesRenewalOrder` and the ladder tests still pass; the trial tests leave the trial plan untouched.

- [ ] **Step 8: Commit**

```bash
go build ./... && go vet ./internal/billing ./internal/payment/checkout
git add internal/billing internal/payment/checkout
git commit -m "billing: opt-in trial conversion into a paid order (phase 1b module A)"
```

<!-- APPEND -->
### Task 10: The paid upgrade — applying it from settlement

**Files:**
- Modify: `internal/store/billing.go` (`ApplyUpgradeFromOrderIn`, `FindUpgradeByPaymentOrderIn`)
- Test: `internal/store/billing_test.go` (append), `internal/billing/upgrade_test.go` (append)
- **Lead-owned (P1):** `internal/payment/settlement/service.go` — `case "upgrade"` and `settleUpgrade`

**Interfaces:**
- Consumes: `ApplyUpgradeIn`, `FindUpgradeByPaymentOrder`, `GetPaymentOrderForUpdate`, `MatchTransaction`/`matchTransaction`, `MarkPaymentOrderPaid`, `dispatchPaid`, `afterPaid`.
- Produces: `store.ApplyUpgradeFromOrderIn(ctx, tx bun.IDB, order *model.PaymentOrder, plan *model.Plan) error`; `store.FindUpgradeByPaymentOrderIn(ctx, tx bun.IDB, order)`.

This is the one task that cannot finish inside Module A: `settlement.Settle` switches on `order.Kind` and rejects anything but `new` and `renewal`, so a paid upgrade order currently errors out and the provider retries forever. The store function below is Module A's; the `case "upgrade"` is a five-line edit in a Lead-owned file, written out here to be applied (not by this plan).

- [ ] **Step 1: Write the failing store tests**

```go
// internal/store/billing_test.go (append)
// An upgrade order applied with a period moves the plan, starts the paid
// period and clears dunning, in one transaction.
func TestApplyUpgradeFromOrderIn(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(10 * 24 * time.Hour)
	prod, plan, lic := seedBillingLicence(t, s, billingSeed{periodEnd: periodEnd, pastDue: true})
	target := &model.Plan{
		ProductID: prod.ID, Name: "Yearly", Slug: "yearly-" + newSuffix(t),
		LicenseType: "subscription", BillingInterval: "year", LicenseModel: "standard",
		MaxActivations: 10, Active: true,
	}
	if err := s.CreatePlan(ctx, target); err != nil {
		t.Fatal(err)
	}
	up := &model.LicenseUpgrade{LicenseID: lic.ID, ToPlanID: target.ID, State: model.UpgradePending, FromPlanID: plan.ID, FromQuantity: 1}
	if err := s.CreatePendingUpgrade(ctx, up); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Truncate(time.Second)
	end := start.Add(365 * 24 * time.Hour)
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: prod.ID, PlanID: target.ID, LicenseID: lic.ID, Email: lic.Email, Kind: "upgrade",
		Quantity: 1, SubtotalMinor: 3_000_000, TotalMinor: 3_000_000, Currency: "VND",
		Provider: "sepay", PeriodStart: &start, PeriodEnd: &end,
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LinkUpgradeOrder(ctx, up.ID, order.ID); err != nil {
		t.Fatal(err)
	}
	err = s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		return s.ApplyUpgradeFromOrderIn(ctx, tx, order, target)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, sub := mustLicence(t, s, lic.ID)
	if got.PlanID != target.ID || got.Status != model.StatusActive {
		t.Fatalf("licence = plan %s status %s", got.PlanID, got.Status)
	}
	if got.PastDueAt != nil || got.SuspendedAt != nil || got.SuspendedBy != "" {
		t.Fatalf("dunning not cleared: past_due=%v suspended=%v/%q", got.PastDueAt, got.SuspendedAt, got.SuspendedBy)
	}
	if got.ValidUntil == nil || !got.ValidUntil.Equal(end) {
		t.Fatalf("valid_until = %v, want %v", got.ValidUntil, end)
	}
	if sub.PlanID != target.ID || !sub.CurrentPeriodEnd.Equal(end) || !sub.CurrentPeriodStart.Equal(start) {
		t.Fatalf("subscription = plan %s %v → %v", sub.PlanID, sub.CurrentPeriodStart, sub.CurrentPeriodEnd)
	}
	applied, err := s.FindPendingUpgradeForLicense(ctx, lic.ID)
	if err != nil || applied != nil {
		t.Fatalf("upgrade still pending: %+v", applied)
	}
	snapshot := new(model.LicenseUpgrade)
	if err := s.DB.NewSelect().Model(snapshot).Where("id = ?", up.ID).Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if snapshot.State != model.UpgradeApplied || snapshot.AppliedAt == nil || snapshot.FromPlanID != plan.ID {
		t.Fatalf("upgrade row = state %s applied_at %v from %s", snapshot.State, snapshot.AppliedAt, snapshot.FromPlanID)
	}
	// Settlement retries: applying twice is a no-op, not a second period.
	err = s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		return s.ApplyUpgradeFromOrderIn(ctx, tx, order, target)
	})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if got, _ := mustLicence(t, s, lic.ID); !got.ValidUntil.Equal(end) {
		t.Fatalf("second apply moved valid_until to %v", got.ValidUntil)
	}
}

// A licence the operator revoked cannot be bought back by an upgrade order.
func TestApplyUpgradeFromOrderRefusesDeadLicence(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(10 * 24 * time.Hour)
	prod, plan, lic := seedBillingLicence(t, s, billingSeed{periodEnd: periodEnd})
	if _, err := s.DB.NewRaw(`UPDATE licenses SET status = 'revoked' WHERE id = ?`, lic.ID).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	target := &model.Plan{
		ProductID: prod.ID, Name: "Yearly", Slug: "yearly-" + newSuffix(t),
		LicenseType: "subscription", BillingInterval: "year", LicenseModel: "standard",
		MaxActivations: 10, Active: true,
	}
	if err := s.CreatePlan(ctx, target); err != nil {
		t.Fatal(err)
	}
	up := &model.LicenseUpgrade{LicenseID: lic.ID, ToPlanID: target.ID, State: model.UpgradePending, FromPlanID: plan.ID, FromQuantity: 1}
	if err := s.CreatePendingUpgrade(ctx, up); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC()
	end := start.Add(365 * 24 * time.Hour)
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: prod.ID, PlanID: target.ID, LicenseID: lic.ID, Email: lic.Email, Kind: "upgrade",
		Quantity: 1, SubtotalMinor: 3_000_000, TotalMinor: 3_000_000, Currency: "VND",
		Provider: "sepay", PeriodStart: &start, PeriodEnd: &end,
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LinkUpgradeOrder(ctx, up.ID, order.ID); err != nil {
		t.Fatal(err)
	}
	err = s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		return s.ApplyUpgradeFromOrderIn(ctx, tx, order, target)
	})
	if !errors.Is(err, store.ErrUpgradeIneligible) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := mustLicence(t, s, lic.ID); got.Status != model.StatusRevoked {
		t.Fatalf("revoked licence came back: %s", got.Status)
	}
}

// An order that pays for no plan change, or a renewal order, is refused
// rather than applied to whatever plan it names.
func TestApplyUpgradeFromOrderRefusesWrongOrder(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(10 * 24 * time.Hour)
	prod, plan, lic := seedBillingLicence(t, s, billingSeed{periodEnd: periodEnd})
	start := time.Now().UTC()
	end := start.Add(30 * 24 * time.Hour)
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: prod.ID, PlanID: plan.ID, LicenseID: lic.ID, Email: lic.Email, Kind: "renewal",
		Quantity: 1, SubtotalMinor: 1_000_000, TotalMinor: 1_000_000, Currency: "VND",
		Provider: "sepay", PeriodStart: &start, PeriodEnd: &end,
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		return s.ApplyUpgradeFromOrderIn(ctx, tx, order, plan)
	})
	if !errors.Is(err, store.ErrUpgradeInvalid) {
		t.Fatalf("err = %v", err)
	}
}
```

Two additions to that package's fixtures. First, a reader:

```go
// mustLicence reads a licence and its subscription back in one go.
func mustLicence(t *testing.T, s *store.Store, id string) (*model.License, *model.Subscription) {
	t.Helper()
	ctx := context.Background()
	lic, err := s.FindLicenseByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.FindSubscriptionByLicense(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return lic, sub
}
```

Second, add `pastDue bool` to Task 1's `billingSeed` and, at the end of `seedBillingLicence`, honour it — an upgrade test starts from a licence that is already in dunning:

```go
	if opts.pastDue {
		now := time.Now().UTC()
		if _, err := s.DB.NewRaw(`UPDATE licenses SET status = 'past_due', past_due_at = ?
			WHERE id = ?`, now, lic.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB.NewRaw(`UPDATE subscriptions SET status = 'past_due'
			WHERE license_id = ?`, lic.ID).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		lic.Status = model.StatusPastDue
	}
```

The struct stays Task 1's; the Task 1 tests pass neither field and keep working.

- [ ] **Step 2: Run them to fail**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store -run 'TestApplyUpgradeFromOrder' -v`
Expected: FAIL — `ApplyUpgradeFromOrderIn` undefined.

- [ ] **Step 3: Write the store function**

```go
// ApplyUpgradeFromOrderIn applies the plan change an upgrade order paid
// for, in the payment's own transaction: the plan, the paid period and the
// dunning state move together, so a crash cannot leave a paid customer on
// the old plan or an unpaid one on the new.
//
// Idempotent: an upgrade already applied returns nil, which is what a
// duplicated provider webhook needs.
func (s *Store) ApplyUpgradeFromOrderIn(ctx context.Context, tx bun.IDB, order *model.PaymentOrder, plan *model.Plan) error {
	if order.Kind != "upgrade" {
		return fmt.Errorf("%w: order %s is a %s order", ErrUpgradeInvalid, order.Code, order.Kind)
	}
	if order.PeriodStart == nil || order.PeriodEnd == nil {
		return fmt.Errorf("%w: order %s has no period", ErrUpgradeInvalid, order.Code)
	}
	upgrade, err := s.FindUpgradeByPaymentOrderIn(ctx, tx, order)
	if err != nil {
		return err
	}
	if upgrade == nil {
		return fmt.Errorf("%w: order %s pays for no plan change", ErrUpgradeInvalid, order.Code)
	}
	return s.ApplyUpgradeIn(ctx, tx, upgrade.ID, plan, order)
}

// FindUpgradeByPaymentOrderIn is FindUpgradeByPaymentOrder on a
// transaction, with the metadata fallback.
func (s *Store) FindUpgradeByPaymentOrderIn(ctx context.Context, db bun.IDB, order *model.PaymentOrder) (*model.LicenseUpgrade, error) {
	up := new(model.LicenseUpgrade)
	err := db.NewSelect().Model(up).Where("payment_order_id = ?", order.ID).Limit(1).Scan(ctx)
	if err == nil {
		return up, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var meta struct {
		UpgradeID string `json:"upgrade_id"`
	}
	if len(order.Metadata) > 0 {
		if jsonErr := json.Unmarshal(order.Metadata, &meta); jsonErr == nil && meta.UpgradeID != "" {
			err = db.NewSelect().Model(up).Where("id = ?", meta.UpgradeID).Limit(1).Scan(ctx)
			if err == nil {
				return up, nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
	}
	return nil, nil
}
```

Then rewrite Task 8's `FindUpgradeByPaymentOrder` as the wrapper, so there is one implementation:

```go
func (s *Store) FindUpgradeByPaymentOrder(ctx context.Context, order *model.PaymentOrder) (*model.LicenseUpgrade, error) {
	return s.FindUpgradeByPaymentOrderIn(ctx, s.DB, order)
}
```

And in `ApplyUpgradeIn`, the order branch of the subscription update also ends the trial — a converted trial has no trial any more:

```go
		if _, err := tx.NewRaw(`UPDATE subscriptions
			SET plan_id = ?, status = 'active', current_period_start = ?, current_period_end = ?,
			    trial_start = NULL, trial_end = NULL, updated_at = ?
			WHERE license_id = ?`, newPlan.ID, *order.PeriodStart, *order.PeriodEnd, now, lic.ID).Exec(ctx); err != nil {
			return err
		}
```

- [ ] **Step 4: Run the store tests to pass**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store -run 'TestApplyUpgradeFromOrder' -v`
Expected: PASS.

- [ ] **Step 5: Apply Lead-owned patch P1, then write the settlement test**

The patch is in "Lead-owned patches" below (`P1`). Apply it in the Lead's change, then this test compiles against it:

```go
// internal/billing/upgrade_test.go (append)
// Paying an upgrade order applies the change: same licence, new plan, the
// period the order bought, dunning cleared — and settlement's own events.
func TestSettlementAppliesUpgradeOrder(t *testing.T) {
	h, lic, target := upgradeHarness(t, "immediate")
	res, err := h.svc.CreateUpgradeOrder(h.ctx, billing.UpgradeRequest{LicenseID: lic.ID, PlanID: target.ID})
	if err != nil {
		t.Fatal(err)
	}
	err = h.pay(t, res.Order)
	if err != nil {
		if strings.Contains(err.Error(), "unsupported kind") {
			// Module A is complete; the routing edit lives in a Lead-owned
			// file. See "Lead-owned patches" P1.
			t.Skip("settlement has no upgrade branch yet: apply Lead-owned patch P1")
		}
		t.Fatalf("settle: %v", err)
	}
	got, sub := h.reload(t, lic.ID)
	if got.PlanID != target.ID || got.Status != model.StatusActive {
		t.Fatalf("licence = plan %s status %s", got.PlanID, got.Status)
	}
	if sub.PlanID != target.ID {
		t.Fatalf("subscription plan = %s", sub.PlanID)
	}
	if got.ValidUntil == nil || !got.ValidUntil.Equal(*res.Order.PeriodEnd) {
		t.Fatalf("valid_until = %v, want %v", got.ValidUntil, res.Order.PeriodEnd)
	}
	// The change is applied, not merely paid. The plan.changed event goes
	// out through the real webhook service, which the harness does not
	// wire: settlement's Dispatch is nil-safe and its payload is the
	// store's, so the row is what this test can and should assert.
	if up, err := h.s.FindPendingUpgradeForLicense(h.ctx, lic.ID); err != nil || up != nil {
		t.Fatalf("upgrade still pending after payment: %+v", up)
	}
	// The doubled webhook is a no-op.
	if err := h.pay(t, res.Order); err != nil {
		t.Fatalf("duplicate delivery: %v", err)
	}
	if got, _ := h.reload(t, lic.ID); !got.ValidUntil.Equal(*res.Order.PeriodEnd) {
		t.Fatalf("duplicate delivery extended to %v", got.ValidUntil)
	}
}
```

`h.pay` on an already-paid order: `Settle` sees `order.Status == paid` → `repairLicenseLink` → nil. Good.

- [ ] **Step 6: Run the settlement test**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/billing -run 'TestSettlementAppliesUpgradeOrder' -v`
Expected: **SKIP** while P1 is unapplied, **PASS** after. This skip is the plan's one intentional gap and is the first thing to re-check at merge; a green skip that nobody reads is the risk, so the verification step at the end greps for it.

- [ ] **Step 7: Commit**

```bash
go build ./... && go vet ./internal/store ./internal/billing
git add internal/store/billing.go internal/store/billing_test.go internal/billing/upgrade_test.go
git commit -m "billing: apply a paid upgrade inside settlement's transaction (phase 1b module A)"
```

<!-- APPEND -->
### Task 11: Two full cycles end to end, and the wiring that starts it

**Files:**
- Create: `internal/billing/e2e_test.go`
- **Lead-owned (P3):** `cmd/server/main.go` — construct the engine and start it

**Interfaces:**
- Consumes: everything above; `billing.New`, `billing.Deps`.
- Produces: proof that the hourly worker does the whole job unattended, and the exact wiring block `main.go` needs.

The unit tests prove each step. This task proves the sequence a real customer lives through, twice, on one licence — because the most common production failure in a billing loop is not a wrong day count, it is a cycle that works once and then stops.

- [ ] **Step 1: Write the failing end-to-end tests**

```go
// internal/billing/e2e_test.go
package billing_test

import (
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/service"
)

// Two renewals in a row: the first is paid on time, the second is paid
// during grace. The licence never dips, and the ladder restarts for the
// new period instead of remembering the old one.
func TestEndToEndTwoRenewalCycles(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	base := time.Now().UTC().Truncate(time.Second)
	firstEnd := base.Add(9 * 24 * time.Hour)
	lic := h.licence(firstEnd)

	// 9 days out: nothing. 7 days out: the first order and its mail.
	if err := h.svc.RunOnceAt(h.ctx, firstEnd.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(h.orders(lic.ID)) != 0 {
		t.Fatalf("order before the lead window")
	}
	if err := h.svc.RunOnceAt(h.ctx, firstEnd.Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := len(h.orders(lic.ID)); n != 1 {
		t.Fatalf("%d orders", n)
	}
	if len(h.mail.sent) != 1 {
		t.Fatalf("%d mails", len(h.mail.sent))
	}

	// Paid two days before the end. The licence extends one interval and
	// the next period is exactly one interval long.
	if err := h.pay(t, h.orders(lic.ID)[0]); err != nil {
		t.Fatal(err)
	}
	got, sub := h.reload(t, lic.ID)
	secondEnd := firstEnd.Add(30 * 24 * time.Hour)
	if got.ValidUntil == nil || !got.ValidUntil.Equal(secondEnd) {
		t.Fatalf("valid_until = %v, want %v", got.ValidUntil, secondEnd)
	}
	if !sub.CurrentPeriodEnd.Equal(secondEnd) {
		t.Fatalf("subscription period_end = %v", sub.CurrentPeriodEnd)
	}

	// The sweep for the new period starts from scratch: no rung is
	// considered already sent for it.
	h.mail.sent = nil
	if err := h.svc.RunOnceAt(h.ctx, firstEnd.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := len(h.orders(lic.ID)); n != 1 {
		t.Fatalf("%d orders right after the first payment", n)
	}
	if err := h.svc.RunOnceAt(h.ctx, secondEnd.Add(-3*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n := len(h.orders(lic.ID)); n != 2 {
		t.Fatalf("%d orders once the second period is inside its lead window", n)
	}
	if len(h.mail.sent) != 1 || h.mail.sent[0].In.Kind != service.RenewalMailReminder {
		t.Fatalf("second cycle mails = %+v", h.mail.sent)
	}

	// Pay the second one a day late: grace still cures it.
	second := h.orders(lic.ID)[1]
	if err := h.svc.RunOnceAt(h.ctx, secondEnd.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.reload(t, lic.ID); got.Status != model.StatusPastDue {
		t.Fatalf("status in grace = %s", got.Status)
	}
	if err := h.pay(t, second); err != nil {
		t.Fatal(err)
	}
	got, _ = h.reload(t, lic.ID)
	if got.Status != model.StatusActive || got.PastDueAt != nil {
		t.Fatalf("licence = %s past_due=%v", got.Status, got.PastDueAt)
	}
	if got.ValidUntil == nil || !got.ValidUntil.Equal(secondEnd.Add(30*24*time.Hour)) {
		t.Fatalf("valid_until = %v", got.ValidUntil)
	}
	if n := len(h.orders(lic.ID)); n != 2 {
		t.Fatalf("%d orders after two cycles", n)
	}
}

// A year of neglect, then a payment: past due, suspended, expired — and a
// late transfer that grants nothing but stays visible to an operator.
func TestEndToEndUnpaidRunOut(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7, renewalDays: 15})
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(2 * 24 * time.Hour)
	lic := h.licence(periodEnd)

	steps := []struct {
		at     time.Time
		status string
	}{
		{periodEnd, model.StatusPastDue},
		{periodEnd.Add(7 * 24 * time.Hour), model.StatusSuspended},
		{periodEnd.Add(22 * 24 * time.Hour), model.StatusExpired},
	}
	for _, step := range steps {
		if err := h.svc.RunOnceAt(h.ctx, step.at); err != nil {
			t.Fatalf("run at %v: %v", step.at, err)
		}
		if got, _ := h.reload(t, lic.ID); got.Status != step.status {
			t.Fatalf("at %v status = %s, want %s", step.at, got.Status, step.status)
		}
	}
	// Every run again at the same instant: nothing changes, nothing mails.
	mails := len(h.mail.sent) + len(h.mail.expired)
	for _, step := range steps {
		if err := h.svc.RunOnceAt(h.ctx, step.at); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.mail.sent)+len(h.mail.expired) != mails {
		t.Fatalf("re-running the same days mailed again: %d → %d", mails, len(h.mail.sent)+len(h.mail.expired))
	}
	// The expired licence is no longer scanned at all.
	if err := h.svc.RunOnceAt(h.ctx, periodEnd.Add(60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.reload(t, lic.ID); got.Status != model.StatusExpired {
		t.Fatalf("status = %s", got.Status)
	}
}

// A licence that pays exactly on its period end is never past due: the
// boundary is not a one-hour dunning window.
func TestEndToEndPaymentOnTheBoundary(t *testing.T) {
	h := newHarness(t, seedOpts{provider: "sepay", priceMinor: 1_190_000, graceDays: 7})
	periodEnd := time.Now().UTC().Truncate(time.Second).Add(2 * 24 * time.Hour)
	lic := h.licence(periodEnd)
	if err := h.svc.RunOnceAt(h.ctx, periodEnd.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	order := h.orders(lic.ID)[0]
	if err := h.pay(t, order); err != nil {
		t.Fatal(err)
	}
	// The run that would have marked it past due sees the new period.
	if err := h.svc.RunOnceAt(h.ctx, periodEnd.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got, _ := h.reload(t, lic.ID)
	if got.Status != model.StatusActive || got.PastDueAt != nil {
		t.Fatalf("licence = %s past_due=%v", got.Status, got.PastDueAt)
	}
	paid, err := h.s.GetPaymentOrderForUpdate(h.ctx, nil, order.ID)
	if err != nil || paid.Status != "paid" {
		t.Fatalf("order = %+v err=%v", paid, err)
	}
}
```

- [ ] **Step 2: Run them to fail, then fix the cause**

Run: `TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/billing -run 'TestEndToEnd' -v`
Expected: PASS for `TestEndToEndPaymentOnTheBoundary`; the other two are the acceptance gate for Tasks 4–7. If `TestEndToEndTwoRenewalCycles` fails at the second mail, the rung tag is missing its period suffix (`reminders.go`, Task 6, Step 7). If it fails at "orders right after the first payment", the scan is still returning the paid period — the store's `NOT EXISTS` on paid orders (Task 1) is the place to look, not the engine.

- [ ] **Step 3: Wire the engine (Lead-owned, P3)**

`cmd/server/main.go`, immediately after `go expiryChecker.StartExpiryLoop(ctx)`:

```go
	// Vietnamese billing engine: renewals, the reminder ladder and the
	// past-due lifecycle for subscriptions on SePay and PayOS. Hourly,
	// because the ladder's finest rung is a day. It never calls Stripe:
	// Stripe subscriptions stay on their own path.
	billingSvc := billing.New(billing.Deps{
		Store:    db,
		Checkout: checkoutSvc,
		Email:    emailSvc,
		Webhook:  webhookSvc,
		Logger:   logger,
		BaseURL:  cfg.BaseURL,
	})
	go billingSvc.Run(ctx)
```

with `"github.com/tabloy/keygate/internal/billing"` in the import block. `checkoutSvc` is created above this point (it is used by the payment handlers earlier in the same function), so no reordering is needed.

- [ ] **Step 4: Verify the whole plan**

```bash
go build ./...
go vet ./...
TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/billing/... -count=1
TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/store/... ./internal/payment/... ./internal/service/... -count=1
TEST_DATABASE_URL=$TEST_DATABASE_URL go test ./internal/billing/... -race -count=1
# No intentional gap may survive the merge:
go test ./internal/billing -run TestSettlementAppliesUpgradeOrder -v 2>&1 | grep -q SKIP && echo "P1 NOT APPLIED — upgrade settlement is still broken"
```
Expected: everything green, and the last command prints nothing (or you fix P1 before merging).

- [ ] **Step 5: Commit**

```bash
git add internal/billing/e2e_test.go
git commit -m "billing: end-to-end renewal cycles (phase 1b module A)"
```

---

## Lead-owned patches

Five edits live in files this plan must not touch. They are written out here so the Lead can apply them without re-deriving the design; each names the task whose tests depend on it.

> **Status: applied in `1f50496`.** Verify the two indexes exist before Tasks 8 and 9 (`\d license_upgrades` and `\d payment_orders` in `psql`), then this patch is done.

### P0 — `db/migrations/20261117_04_billing_constraints.{up,down}.sql` (blocks Tasks 8 and 9)

```sql
-- 20261117_04_billing_constraints.up.sql
-- The VN rails write '' into the Stripe session column, so the old
-- non-partial unique index allowed exactly one such row per table. Make it
-- partial, and give the VN rail its own unique key: the order that pays
-- for the change.
DROP INDEX IF EXISTS idx_license_upgrades_session;
CREATE UNIQUE INDEX IF NOT EXISTS idx_license_upgrades_session
    ON license_upgrades (stripe_checkout_session_id)
    WHERE stripe_checkout_session_id <> '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_license_upgrades_payment_order
    ON license_upgrades (payment_order_id)
    WHERE payment_order_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_license_renewals_payment_order
    ON license_renewals (payment_order_id)
    WHERE payment_order_id IS NOT NULL;
```

```sql
-- 20261117_04_billing_constraints.down.sql
DROP INDEX IF EXISTS idx_license_renewals_payment_order;
DROP INDEX IF EXISTS idx_license_upgrades_payment_order;
DROP INDEX IF EXISTS idx_license_upgrades_session;
CREATE UNIQUE INDEX IF NOT EXISTS idx_license_upgrades_session
    ON license_upgrades (stripe_checkout_session_id);
```

### P1 — `internal/payment/settlement/service.go`, two edits (blocks Task 10's last test)

In `Settle`'s switch, after `case "renewal"`:

```go
	case "upgrade":
		return s.settleUpgrade(ctx, order, providerName, ev)
```

and the method, next to `settleRenewal`:

```go
// settleUpgrade applies the plan change an upgrade order paid for, in the
// payment's transaction, then emits the paid events plus the change. A
// duplicated delivery is absorbed by the same two guards a renewal uses.
func (s *Service) settleUpgrade(ctx context.Context, order *model.PaymentOrder, providerName string, ev provider.WebhookEvent) error {
	plan, err := s.Store.FindPlanByID(ctx, order.PlanID)
	if err != nil {
		return s.failGrant(ctx, order, providerName, ev, fmt.Errorf("read plan: %w", err))
	}
	var paid, alreadySettled *model.PaymentOrder
	var granted *model.License
	var upgrade *model.LicenseUpgrade
	err = s.Store.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		locked, err := s.Store.GetPaymentOrderForUpdate(ctx, tx, order.ID)
		if err != nil {
			return err
		}
		if locked.Status == domain.OrderPaid {
			alreadySettled = locked
			return nil
		}
		if domain.IsOrderTerminal(locked.Status) {
			return store.ErrOrderNotPayable
		}
		upgrade, err = s.Store.FindUpgradeByPaymentOrderIn(ctx, tx, locked)
		if err != nil {
			return err
		}
		if upgrade == nil {
			return fmt.Errorf("%w: order %s pays for no plan change", store.ErrUpgradeInvalid, locked.Code)
		}
		if err := s.Store.ApplyUpgradeFromOrderIn(ctx, tx, locked, plan); err != nil {
			return err
		}
		granted, err = s.Store.FindLicenseByIDIn(ctx, tx, locked.LicenseID)
		if err != nil {
			return err
		}
		paid, err = s.Store.MarkPaymentOrderPaid(ctx, tx, order.ID, time.Now())
		if err != nil {
			return err
		}
		return s.matchTransaction(ctx, tx, providerName, paid, ev)
	})
	if err != nil {
		if errors.Is(err, store.ErrOrderNotPayable) {
			return s.settleNotPayable(ctx, order, providerName, ev, err)
		}
		return s.failGrant(ctx, order, providerName, ev, err)
	}
	if alreadySettled != nil {
		return nil
	}
	s.dispatchPaid(ctx, paid, granted, providerName, ev)
	if s.Webhooks != nil && upgrade != nil {
		s.Webhooks.Dispatch(ctx, granted.ProductID, model.EventPlanChanged, map[string]any{
			"license_id": granted.ID, "from_plan_id": upgrade.FromPlanID,
			"to_plan_id": plan.ID, "period_end": paid.PeriodEnd, "reason": "paid_change",
		})
	}
	s.afterPaid(ctx, paid, granted, providerName)
	return nil
}
```

> **Status: applied in `d4a459c`, with one bug to fix.** The commit adds `notBillingRailSQL` (`NOT EXISTS (… payment_provider IN ('sepay','payos'))`) and uses it for all four sweeps, including `FindLicensesForGraceExpiry`. That is the blanket exclusion this section warns about, and it strands licences today: `CancelSubscriptionAtPeriodEnd` (also landed) sets the subscription to `canceled`, the billing scan then stops returning it (`status IN ('active','trialing','past_due')`), and the grace sweep no longer sees it either — so a customer who turns renewal off keeps a working licence forever. Fix `FindLicensesForGraceExpiry` to the status-aware predicate below and keep `notBillingRailSQL` for the three mail sweeps. Task 7's `TestCancelAtPeriodEndClosesSubscription` asserts the handoff and fails until this is fixed.

### P2 — four predicates that must stop matching VN subscriptions (blocks nothing, but ships duplicates if missed)

Once the engine owns VN renewals, the older Stripe-era sweeps must skip them or a customer gets two mails and, worse, is expired instead of suspended.

Two different predicates, because the three sweeps are not doing the same job.

`internal/store/store.go`, `FindLicensesForGraceExpiry` — **expiry must still reach a cancelled VN licence**, so exclude only the subscriptions the engine is still billing. Written in the landed `notBillingRailSQL` shape (a `NOT EXISTS` over `license.id`, because the sweep's query has no subscription join):

```go
// notEngineBilledSQL keeps the grace sweep away from licences the billing
// engine is still responsible for. It is deliberately narrower than
// notBillingRailSQL: a VN subscription the customer cancelled is no longer
// the engine's, and dropping it from this sweep too would leave the
// licence working forever.
const notEngineBilledSQL = `NOT EXISTS (
	SELECT 1 FROM subscriptions bs
	WHERE bs.license_id = license.id
	  AND bs.payment_provider IN ('sepay', 'payos')
	  AND bs.status IN ('active', 'trialing', 'past_due')
)`
```

and `Where(notEngineBilledSQL)` replaces `Where(notBillingRailSQL)` in `FindLicensesForGraceExpiry` only.

The condition is on the *subscription* status, not on the provider alone, and the `canceled` case is why: when a customer turns renewal off, the engine cancels the subscription and then stops scanning that licence (the scan wants `active`, `trialing` or `past_due`). The licence still has to end when its paid period runs out, and the platform's existing grace-expiry sweep is what ends it — exactly what `CancelBillingSubscriptionAtPeriodEnd`'s comment promises. A blanket `NOT IN ('sepay','payos')` would strand every cancelled VN licence as working forever.

`internal/store/store.go`, `FindStalePastDueLicenses` and `FindLicensesForExpiryReminder`, and `internal/service/expiry.go`'s dunning query (`SendPaymentFailureReminders`) — **no legacy mail for a VN customer, ever**, so there the exclusion is by provider alone, which is exactly the landed `notBillingRailSQL`:

```go
const notBillingRailSQL = `NOT EXISTS (
	SELECT 1 FROM subscriptions bs
	WHERE bs.license_id = license.id
	  AND bs.payment_provider IN ('sepay', 'payos')
)`
```

Keep those three as they are.

Those three send mail (renewal-ending notices, card-failure dunning). Every VN customer gets the ladder instead, and two reminders for the same period is the kind of bug a customer reports before a test does.

`FindExpiredTrials` keeps matching VN trials — a trial that lapses is the trial sweep's job, and the engine only converts opted-in ones (Task 9).

### P3 — `cmd/server/main.go` (Task 11, Step 3)

The block in Task 11, Step 3.

---

## Definition of done

- `go build ./...`, `go vet ./...` clean.
- `TEST_DATABASE_URL=… go test ./internal/billing/... -count=1` green, including the three end-to-end tests.
- `TEST_DATABASE_URL=… go test ./internal/billing/... -race -count=1` green.
- The rest of the suite green: `./internal/store/... ./internal/payment/... ./internal/service/...`.
- `go test ./internal/billing -run TestSettlementAppliesUpgradeOrder -v` does **not** print `SKIP` (P1 applied).
- No file outside `internal/billing/`, `internal/store/billing.go`, `internal/payment/checkout/` and `internal/service/email*.go` is modified by this module.
- The "Powered by Keygate" footer is intact in every rendered mail: `service.withAttribution` is what appends it, and none of this plan's mail path bypasses the relay.
