package checkout_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/checkout"
	"github.com/tabloy/keygate/internal/payment/coupons"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider/fake"
	"github.com/tabloy/keygate/internal/payment/settlement"
	"github.com/tabloy/keygate/internal/store"
)

// couponFixture is the checkout fixture plus a coupon service and a
// settlement service wired the way the server wires them.
type couponFixture struct {
	*fixture
	svc       *checkout.Service
	coupons   *coupons.Service
	prov      *fake.Provider
	factory   *factory
	settler   *settlement.Service
	planForID *model.Plan
}

func newCouponFixture(t *testing.T, amountMinor int64) *couponFixture {
	t.Helper()
	f := newFixture(t, true)
	f.price("sepay", amountMinor)
	prov := fake.New("sepay")
	factory := newFactory(prov)
	cs := coupons.New(f.s)
	settler := &settlement.Service{Store: f.s, PaidHook: cs}
	svc := &checkout.Service{
		Store: f.s, Providers: factory.providerFactory(),
		Coupons: cs, Settler: settler, BaseURL: "https://example.test",
	}
	return &couponFixture{fixture: f, svc: svc, coupons: cs, prov: prov, factory: factory, settler: settler}
}

// trialPlan adds a second, trial-capable plan to the fixture's product and
// prices it.
func (f *couponFixture) trialPlan(licenseType string, trialDays int) *model.Plan {
	f.t.Helper()
	plan := &model.Plan{
		ProductID: f.product.ID, Name: "Trial " + licenseType, Slug: "trial-" + licenseType + "-" + f.suffix,
		LicenseType: licenseType, LicenseModel: "standard", MaxActivations: 3,
		TrialDays: trialDays, BillingInterval: "month", Active: true,
	}
	if err := f.s.CreatePlan(f.ctx, plan); err != nil {
		f.t.Fatalf("create plan: %v", err)
	}
	f.planForID = plan
	if _, err := f.s.UpsertPlanPrice(f.ctx, plan.ID, store.PlanPriceInput{
		Provider: "sepay", Currency: "VND", AmountMinor: 1_000_000,
	}); err != nil {
		f.t.Fatalf("upsert price: %v", err)
	}
	return plan
}

func (f *couponFixture) createCoupon(mutate func(*store.CouponInput)) *model.Coupon {
	f.t.Helper()
	in := store.CouponInput{
		Code: "C" + strings.ToUpper(f.suffix), Type: "percent", Value: 1000,
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

func (f *couponFixture) input(email, planID string, codes ...string) checkout.CreateOrderInput {
	return checkout.CreateOrderInput{
		PlanID: planID, Email: email, Provider: "sepay", Quantity: 1, CouponCodes: codes,
	}
}

func TestCheckoutAppliesCouponAndSnapshotsIt(t *testing.T) {
	f := newCouponFixture(t, 1_000_000)
	c := f.createCoupon(nil)

	res, err := f.svc.CreateOrder(f.ctx, f.input(f.email("coupon"), f.plan.ID, strings.ToLower(c.Code)))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// 1,000,000 VND with 10% off, 10% VAT included: 900,000 total,
	// tax = round(900000*1000/11000) = 81,818.
	if res.Order.SubtotalMinor != 1_000_000 || res.Order.DiscountMinor != 100_000 {
		t.Fatalf("order money = %+v", res.Order)
	}
	if res.Order.TotalMinor != 900_000 || res.Order.TaxMinor != 81_818 {
		t.Fatalf("order totals = %+v", res.Order)
	}
	if res.Attempt.AmountMinor != 900_000 {
		t.Fatalf("attempt amount = %d", res.Attempt.AmountMinor)
	}
	if len(f.prov.Calls) != 1 || f.prov.Calls[0].AmountMinor != 900_000 {
		t.Fatalf("provider calls = %+v", f.prov.Calls)
	}

	rows, err := f.s.ListOrderDiscounts(f.ctx, res.Order.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("discount rows = %+v err = %v", rows, err)
	}
	if rows[0].CouponCode != c.Code || rows[0].Type != "percent" || rows[0].Value != 1000 || rows[0].AmountMinor != 100_000 {
		t.Fatalf("discount row = %+v", rows[0])
	}

	block := coupons.FromOrderMetadata(res.Order)
	if len(block.Codes) != 1 || block.Codes[0] != c.Code {
		t.Fatalf("metadata codes = %+v", block.Codes)
	}
	if block.Signature != `["`+c.Code+`"]` {
		t.Fatalf("metadata signature = %q", block.Signature)
	}

	// The coupons were quoted, not consumed: nothing is redeemed until
	// the order is paid.
	if got, err := f.s.GetCoupon(f.ctx, c.ID); err != nil || got.RedeemedCount != 0 {
		t.Fatalf("redeemed_count = %+v err = %v", got, err)
	}
}

func TestCheckoutRejectsInvalidCouponAndIgnoresWhenAsked(t *testing.T) {
	f := newCouponFixture(t, 1_000_000)
	_, err := f.svc.CreateOrder(f.ctx, f.input(f.email("invalid"), f.plan.ID, "NOPE"))
	if got := coupons.ErrorCode(err); got != coupons.CodeInvalid {
		t.Fatalf("code = %q (err = %v)", got, err)
	}

	in := f.input(f.email("invalid"), f.plan.ID, "NOPE")
	in.IgnoreInvalidCoupons = true
	res, err := f.svc.CreateOrder(f.ctx, in)
	if err != nil {
		t.Fatalf("ignore invalid: %v", err)
	}
	if res.Order.TotalMinor != 1_000_000 || res.Order.DiscountMinor != 0 {
		t.Fatalf("order = %+v", res.Order)
	}
	if rows, err := f.s.ListOrderDiscounts(f.ctx, res.Order.ID); err != nil || len(rows) != 0 {
		t.Fatalf("discount rows = %+v err = %v", rows, err)
	}
	if block := coupons.FromOrderMetadata(res.Order); len(block.Codes) != 0 || block.Signature != "" {
		t.Fatalf("metadata block = %+v", block)
	}
}

func TestCheckoutStacksGeneralAndCampaign(t *testing.T) {
	f := newCouponFixture(t, 1_000_000)
	general := f.createCoupon(nil)
	campaign := f.createCoupon(func(in *store.CouponInput) {
		in.Code = "CAMP" + strings.ToUpper(f.suffix)
		in.Type, in.Value, in.StackGroup, in.Currency, in.Priority = "fixed", 50_000, "campaign", "VND", 200
	})
	res, err := f.svc.CreateOrder(f.ctx, f.input(f.email("stack"), f.plan.ID, general.Code, campaign.Code))
	if err != nil {
		t.Fatalf("stack: %v", err)
	}
	if res.Order.DiscountMinor != 150_000 || res.Order.TotalMinor != 850_000 {
		t.Fatalf("order = %+v", res.Order)
	}
	rows, err := f.s.ListOrderDiscounts(f.ctx, res.Order.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v err = %v", rows, err)
	}
	if rows[0].CouponCode != general.Code || rows[1].CouponCode != campaign.Code {
		t.Fatalf("row order = %s, %s", rows[0].CouponCode, rows[1].CouponCode)
	}

	// Two codes in one stack group never apply together.
	second := f.createCoupon(func(in *store.CouponInput) {
		in.Code = "C2" + strings.ToUpper(f.suffix)
		in.Priority = 300
	})
	_, err = f.svc.CreateOrder(f.ctx, f.input(f.email("stack2"), f.plan.ID, general.Code, second.Code))
	if got := coupons.ErrorCode(err); got != coupons.CodeGroupConflict {
		t.Fatalf("code = %q (err = %v)", got, err)
	}
}

func TestCheckoutIgnoreInvalidStillRefusesAssigned(t *testing.T) {
	f := newCouponFixture(t, 1_000_000)
	c := f.createCoupon(func(in *store.CouponInput) { in.AssignedEmail = "owner@example.com" })

	_, err := f.svc.CreateOrder(f.ctx, f.input(f.email("other"), f.plan.ID, c.Code))
	if got := coupons.ErrorCode(err); got != coupons.CodeAssigned {
		t.Fatalf("code = %q (err = %v)", got, err)
	}

	in := f.input(f.email("other"), f.plan.ID, c.Code)
	in.IgnoreInvalidCoupons = true
	res, err := f.svc.CreateOrder(f.ctx, in)
	if err != nil {
		t.Fatalf("ignore: %v", err)
	}
	if res.Order.DiscountMinor != 0 || res.Order.TotalMinor != 1_000_000 {
		t.Fatalf("an assigned coupon must never be applied: %+v", res.Order)
	}
	if rows, err := f.s.ListOrderDiscounts(f.ctx, res.Order.ID); err != nil || len(rows) != 0 {
		t.Fatalf("rows = %+v err = %v", rows, err)
	}
}

func TestResumableOrderMatchesCouponSignature(t *testing.T) {
	f := newCouponFixture(t, 1_000_000)
	c := f.createCoupon(nil)
	email := f.email("retry")

	first, err := f.svc.CreateOrder(f.ctx, f.input(email, f.plan.ID, c.Code))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	same, err := f.svc.CreateOrder(f.ctx, f.input(email, f.plan.ID, c.Code))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if same.Order.ID != first.Order.ID || same.Attempt.ID != first.Attempt.ID {
		t.Fatalf("a retry with the same codes must reuse the order")
	}

	// The same buyer changing codes must not be handed the order priced
	// with the old ones.
	other, err := f.svc.CreateOrder(f.ctx, f.input(email, f.plan.ID))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if other.Order.ID == first.Order.ID {
		t.Fatalf("an order priced with a coupon was reused for a full-price request")
	}
	if other.Order.TotalMinor != 1_000_000 {
		t.Fatalf("full-price order = %+v", other.Order)
	}
	if rows, err := f.s.ListOrderDiscounts(f.ctx, other.Order.ID); err != nil || len(rows) != 0 {
		t.Fatalf("full-price order carries discounts: %+v err = %v", rows, err)
	}
}

func TestCheckoutTrialOrderSettlesAtCreation(t *testing.T) {
	f := newCouponFixture(t, 1_000_000)
	plan := f.trialPlan("trial", 14)
	trial := f.createCoupon(func(in *store.CouponInput) {
		in.Code = "TRIAL" + strings.ToUpper(f.suffix)
		in.Type, in.Value, in.TrialDays = "trial", 0, 30
	})
	email := f.email("trial")

	res, err := f.svc.CreateOrder(f.ctx, f.input(email, plan.ID, trial.Code))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Attempt != nil {
		t.Fatalf("a free order must not get a payment instruction: %+v", res.Attempt)
	}
	if len(f.prov.Calls) != 0 {
		t.Fatalf("provider was asked for an instruction: %+v", f.prov.Calls)
	}
	if res.Order.Status != domain.OrderPaid || res.Order.TotalMinor != 0 || res.Order.DiscountMinor != 1_000_000 {
		t.Fatalf("order = %+v", res.Order)
	}
	if res.Order.LicenseID == "" {
		t.Fatalf("order has no licence: %+v", res.Order)
	}
	lic, err := f.s.FindLicenseByID(f.ctx, res.Order.LicenseID)
	if err != nil {
		t.Fatalf("licence: %v", err)
	}
	if lic.Status != model.StatusTrialing || lic.ValidUntil == nil {
		t.Fatalf("licence = %+v", lic)
	}
	// The coupon's 30 days beat the plan's 14.
	days := time.Until(*lic.ValidUntil).Hours() / 24
	if days < 29 || days > 30.01 {
		t.Fatalf("licence period = %.2f days", days)
	}
	if got, err := f.s.GetCoupon(f.ctx, trial.ID); err != nil || got.RedeemedCount != 1 {
		t.Fatalf("redeemed_count = %+v err = %v", got, err)
	}
	rows, _, err := f.s.ListRedemptions(f.ctx, store.RedemptionFilter{CouponID: trial.ID, Page: store.Page{Limit: 10}})
	if err != nil || len(rows) != 1 || rows[0].Type != "trial" || rows[0].TrialDays != 30 {
		t.Fatalf("redemptions = %+v err = %v", rows, err)
	}

	// A retried create gets the settled order back rather than granting a
	// second free trial.
	again, err := f.svc.CreateOrder(f.ctx, f.input(email, plan.ID, trial.Code))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if again.Order.ID != res.Order.ID || again.Attempt != nil {
		t.Fatalf("retry created a second free order: %+v", again.Order)
	}
	var licences int
	if err := f.s.DB.NewRaw("SELECT count(*) FROM licenses WHERE email = ?", strings.ToLower(email)).Scan(f.ctx, &licences); err != nil {
		t.Fatalf("count licences: %v", err)
	}
	if licences != 1 {
		t.Fatalf("licences for the buyer = %d, want 1", licences)
	}
}

func TestCheckoutRefusesInvoiceWithoutTaxCode(t *testing.T) {
	f := newCouponFixture(t, 1_000_000)
	in := f.input(f.email("buyer-tax"), f.plan.ID)
	in.Buyer.RequestInvoice = true
	if _, err := f.svc.CreateOrder(f.ctx, in); !errors.Is(err, checkout.ErrTaxCodeRequired) {
		t.Fatalf("err = %v", err)
	}
	var orders int
	if err := f.s.DB.NewRaw("SELECT count(*) FROM payment_orders WHERE email = ?", strings.ToLower(f.email("buyer-tax"))).
		Scan(f.ctx, &orders); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if orders != 0 {
		t.Fatalf("orders = %d, want none", orders)
	}
}

func TestCheckoutRefusesCodesWithoutCouponService(t *testing.T) {
	f := newCouponFixture(t, 1_000_000)
	f.svc.Coupons = nil
	_, err := f.svc.CreateOrder(f.ctx, f.input(f.email("nocoupons"), f.plan.ID, "ANY"))
	if !errors.Is(err, checkout.ErrCouponSupportMissing) {
		t.Fatalf("err = %v", err)
	}
	var orders int
	if err := f.s.DB.NewRaw("SELECT count(*) FROM payment_orders WHERE email = ?", strings.ToLower(f.email("nocoupons"))).
		Scan(f.ctx, &orders); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if orders != 0 {
		t.Fatalf("orders = %d, want 0", orders)
	}
}

func TestPreviewQuotePricesCodes(t *testing.T) {
	f := newCouponFixture(t, 1_000_000)
	c := f.createCoupon(nil)
	// A preview needs no provider adapter: it shows numbers, not an
	// instruction.
	preview := &checkout.Service{Store: f.s, Coupons: f.coupons}
	res, err := preview.PreviewQuote(f.ctx, checkout.PreviewInput{
		PlanID: f.plan.ID, Provider: "sepay", Quantity: 1, Codes: []string{c.Code},
		Email: f.email("preview"),
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if res.Quote.SubtotalMinor != 1_000_000 || res.Quote.DiscountMinor != 100_000 || res.Quote.TotalMinor != 900_000 {
		t.Fatalf("quote = %+v", res.Quote)
	}
	if res.Plan.ID != f.plan.ID || res.Price.AmountMinor != 1_000_000 {
		t.Fatalf("preview result = %+v", res)
	}
	var orders int
	if err := f.s.DB.NewRaw("SELECT count(*) FROM payment_orders WHERE email = ?", strings.ToLower(f.email("preview"))).
		Scan(f.ctx, &orders); err != nil {
		t.Fatalf("count orders: %v", err)
	}
	if orders != 0 {
		t.Fatalf("a preview created an order")
	}

	// The portal preview takes exactly one code.
	_, err = preview.PreviewQuote(f.ctx, checkout.PreviewInput{
		PlanID: f.plan.ID, Provider: "sepay", Quantity: 1, Codes: []string{c.Code, "OTHER"}, Portal: true,
	})
	if got := coupons.ErrorCode(err); got != coupons.CodeInvalid {
		t.Fatalf("portal code = %q (err = %v)", got, err)
	}
}

func TestCheckoutQuoteMatchesOrderTotals(t *testing.T) {
	// Inclusive VAT: the discount comes off the gross price and the tax is
	// recomputed once on the discounted amount, so the order row and the
	// quote cannot disagree.
	for _, inclusive := range []bool{true, false} {
		f := newFixture(t, inclusive)
		f.price("sepay", 1_190_000)
		cs := coupons.New(f.s)
		svc := &checkout.Service{Store: f.s, Providers: newFactory(fake.New("sepay")).providerFactory(), Coupons: cs}
		c, err := f.s.CreateCoupon(f.ctx, store.CouponInput{
			Code: "TAX" + strings.ToUpper(strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")) + map[bool]string{true: "I", false: "E"}[inclusive],
			Type: "percent", Value: 2500, StackGroup: "general", Stackable: true, Priority: 100, Active: true,
		})
		if err != nil {
			t.Fatalf("coupon: %v", err)
		}
		email := "tax-" + f.suffix + "-" + map[bool]string{true: "i", false: "e"}[inclusive] + "@example.com"
		in := checkout.CreateOrderInput{PlanID: f.plan.ID, Email: email, Provider: "sepay", Quantity: 1, CouponCodes: []string{c.Code}}
		q, err := cs.Quote(f.ctx, coupons.QuoteInput{
			Codes: in.CouponCodes, Plan: f.plan, Product: f.product, Quantity: 1, SubtotalMinor: 1_190_000,
			Currency: "VND", Kind: "new", Email: email,
		})
		if err != nil {
			t.Fatalf("quote: %v", err)
		}
		res, err := svc.CreateOrder(f.ctx, in)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if res.Order.SubtotalMinor != q.SubtotalMinor || res.Order.DiscountMinor != q.DiscountMinor ||
			res.Order.TaxMinor != q.TaxMinor || res.Order.TotalMinor != q.TotalMinor {
			t.Fatalf("inclusive=%v order %+v != quote %+v", inclusive, res.Order, q)
		}
	}
}
