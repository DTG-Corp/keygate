package coupons

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/internal/testsupport/testdb"
)

// quoteFixture is a durable product+plan plus an open 10%-included VAT
// configuration, matching checkout's fixture.
type quoteFixture struct {
	t       *testing.T
	s       *store.Store
	ctx     context.Context
	suffix  string
	seq     int
	product *model.Product
	plan    *model.Plan
}

func newQuoteFixture(t *testing.T) *quoteFixture {
	t.Helper()
	s := testdb.Store(t)
	ctx := context.Background()
	suffix := strings.ToUpper(strings.ReplaceAll(uuid.NewString()[:13], "-", ""))
	inclusive := true
	p := &model.Product{
		Name: "Coupons", Slug: "coupons-" + strings.ToLower(suffix), Type: "desktop",
		TaxRateBP: 1000, PriceIncludesTax: &inclusive,
	}
	if err := s.CreateProduct(ctx, p); err != nil {
		t.Fatalf("create product: %v", err)
	}
	pl := &model.Plan{
		ProductID: p.ID, Name: "Coupon plan", Slug: "coupon-plan-" + strings.ToLower(suffix),
		LicenseType: "trial", LicenseModel: "standard", MaxActivations: 3,
		TrialDays: 14, Active: true,
	}
	if err := s.CreatePlan(ctx, pl); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return &quoteFixture{t: t, s: s, ctx: ctx, suffix: suffix, product: p, plan: pl}
}

func (f *quoteFixture) service() *Service { return New(f.s) }

func (f *quoteFixture) input(subtotal int64) QuoteInput {
	return QuoteInput{
		Plan: f.plan, Product: f.product, Quantity: 1, SubtotalMinor: subtotal,
		Currency: "VND", CurrencyExponent: 0, Kind: "new", Email: "buyer@example.com",
	}
}

// create writes one coupon. The sequence number keeps codes unique per
// test even when several are created inside the same microsecond.
func (f *quoteFixture) create(mutate func(*store.CouponInput)) *model.Coupon {
	f.t.Helper()
	f.seq++
	in := store.CouponInput{
		Code: fmt.Sprintf("C%s%d", f.suffix, f.seq), Type: "percent", Value: 1000,
		StackGroup: "general", Stackable: true, Priority: 100, Active: true,
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

func errFrom(_ *Quote, err error) error { return err }

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
		in.ValidFrom, in.ValidTo = &lastWeek, &yesterday
	})
	q = f.input(1_000_000)
	q.Codes = []string{expired.Code}
	if got := ErrorCode(errFrom(f.service().Quote(f.ctx, q))); got != CodeInvalid {
		t.Fatalf("expired code = %q", got)
	}
}

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
	if _, err := f.s.DB.NewRaw("UPDATE coupons SET redeemed_count = 1 WHERE id = ?", c.ID).Exec(f.ctx); err != nil {
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
	f.redeemAs(c.Code, c.ID, "buyer@example.com", "")
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
	general := f.create(func(in *store.CouponInput) { in.Priority = 100 })
	campaign := f.create(func(in *store.CouponInput) {
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

	secondGeneral := f.create(func(in *store.CouponInput) { in.Priority = 300 })
	q = f.input(1_000_000)
	q.Codes = []string{general.Code, secondGeneral.Code}
	if got := ErrorCode(errFrom(f.service().Quote(f.ctx, q))); got != CodeGroupConflict {
		t.Fatalf("code = %q", got)
	}
}

func TestQuoteReturnsDiscountBreakdownInOrder(t *testing.T) {
	f := newQuoteFixture(t)
	first := f.create(func(in *store.CouponInput) { in.Priority = 10 })
	second := f.create(func(in *store.CouponInput) {
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
	if err := f.s.SetSetting(f.ctx, SettingMaxStack, "1"); err != nil {
		t.Fatalf("setting: %v", err)
	}
	t.Cleanup(func() { _ = f.s.SetSetting(context.Background(), SettingMaxStack, "") })
	general := f.create(nil)
	campaign := f.create(func(in *store.CouponInput) {
		in.Type, in.Value, in.StackGroup, in.Currency = "fixed", 10_000, "campaign", "VND"
	})
	q := f.input(1_000_000)
	q.Codes = []string{general.Code, campaign.Code}
	if got := ErrorCode(errFrom(f.service().Quote(f.ctx, q))); got != CodeStackLimit {
		t.Fatalf("code = %q", got)
	}
}

func TestPortalValidateRequiresOneCode(t *testing.T) {
	f := newQuoteFixture(t)
	c := f.create(nil)
	if _, err := f.service().PortalValidate(f.ctx, f.input(1_000_000)); ErrorCode(err) != CodeInvalid {
		t.Fatalf("no code err = %v", err)
	}
	q := f.input(1_000_000)
	q.Codes = []string{c.Code}
	got, err := f.service().PortalValidate(f.ctx, q)
	if err != nil {
		t.Fatalf("one code: %v", err)
	}
	if got.DiscountMinor != 100_000 {
		t.Fatalf("quote = %+v", got)
	}
	// The portal preview is always a new purchase, whatever the caller put
	// on the input.
	q.Kind = "renewal"
	if _, err := f.service().PortalValidate(f.ctx, q); err != nil {
		t.Fatalf("portal forces kind new: %v", err)
	}
}

func TestOrderMetadataRoundTrips(t *testing.T) {
	q := &Quote{
		Discounts: []Discount{
			{CouponID: "cpn_1", Code: "TEN", Type: "percent", Value: 1000, AmountMinor: 100},
			{CouponID: "cpn_2", Code: "FIX", Type: "fixed", Value: 50, AmountMinor: 50},
		},
	}
	block := OrderMetadata(q, "ws_1")
	if block == nil {
		t.Fatalf("block is nil")
	}
	order := &model.PaymentOrder{Metadata: mustJSON(t, map[string]any{MetadataKey: block})}
	got := FromOrderMetadata(order)
	if len(got.Codes) != 2 || got.Codes[0] != "TEN" || got.Codes[1] != "FIX" {
		t.Fatalf("codes = %+v", got.Codes)
	}
	if got.ExternalCustomerID != "ws_1" {
		t.Fatalf("external id = %q", got.ExternalCustomerID)
	}
	if got.Signature != `["FIX","TEN"]` {
		t.Fatalf("signature = %q", got.Signature)
	}
	if OrderSignature(order) != `["FIX","TEN"]` {
		t.Fatalf("order signature = %q", OrderSignature(order))
	}
	if sig := OrderSignature(&model.PaymentOrder{Metadata: []byte(`{}`)}); sig != "[]" {
		t.Fatalf("no block signature = %q", sig)
	}
	if block := OrderMetadata(&Quote{}, ""); block != nil {
		t.Fatalf("empty quote block = %+v", block)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
