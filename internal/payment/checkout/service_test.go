package checkout_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/checkout"
	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/payment/provider"
	"github.com/dtg-corp/dsoft/internal/payment/provider/fake"
	"github.com/dtg-corp/dsoft/internal/payment/providercfg"
	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/internal/testsupport/testdb"
)

// fixture is a priced product/plan pair per test. The email suffix keeps
// the reuse lookup from finding another test's order.
type fixture struct {
	t       *testing.T
	s       *store.Store
	ctx     context.Context
	suffix  string
	product *model.Product
	plan    *model.Plan
}

func newFixture(t *testing.T, priceIncludesTax bool) *fixture {
	t.Helper()
	s := testdb.Store(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	inclusive := priceIncludesTax
	p := &model.Product{
		Name: "Checkout", Slug: "checkout-" + suffix, Type: "desktop",
		TaxRateBP: 1000, PriceIncludesTax: &inclusive,
	}
	if err := s.CreateProduct(ctx, p); err != nil {
		t.Fatalf("create product: %v", err)
	}
	pl := &model.Plan{
		ProductID: p.ID, Name: "Pro", Slug: "checkout-pro-" + suffix,
		LicenseType: "perpetual", LicenseModel: "standard",
		MaxActivations: 3, Active: true,
	}
	if err := s.CreatePlan(ctx, pl); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return &fixture{t: t, s: s, ctx: ctx, suffix: suffix, product: p, plan: pl}
}

func (f *fixture) email(prefix string) string { return prefix + "-" + f.suffix + "@example.com" }

func (f *fixture) price(providerName string, amountMinor int64) {
	f.t.Helper()
	currency := "VND"
	if _, err := f.s.UpsertPlanPrice(f.ctx, f.plan.ID, store.PlanPriceInput{
		Provider: providerName, Currency: currency, AmountMinor: amountMinor,
	}); err != nil {
		f.t.Fatalf("upsert price: %v", err)
	}
}

// factory records the (product, provider) pairs checkout resolves.
type factory struct {
	prov  *fake.Provider
	err   error
	calls []string
}

func newFactory(prov *fake.Provider) *factory {
	return &factory{prov: prov}
}

func (f *factory) providerFactory() checkout.ProviderFactory {
	return func(_ context.Context, productID, providerName string) (provider.Provider, *providercfg.ResolvedConfig, error) {
		f.calls = append(f.calls, productID+"/"+providerName)
		return f.prov, &providercfg.ResolvedConfig{Provider: providerName}, f.err
	}
}

func TestCreateOrderResolvesPriceAndTotals(t *testing.T) {
	f := newFixture(t, true)
	f.price("sepay", 1_190_000)
	fp := fake.New("sepay")
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fp).providerFactory()}

	res, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: "c@" + f.suffix + ".example", Provider: "sepay", Quantity: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// 1,190,000 VND at 10% VAT included: tax = round(1190000*1000/11000) = 108,182.
	if res.Order.TotalMinor != 1_190_000 || res.Order.TaxMinor != 108_182 || res.Order.SubtotalMinor != 1_190_000 {
		t.Fatalf("order totals = %+v", res.Order)
	}
	if res.Order.Currency != "VND" || res.Order.CurrencyExp != 0 {
		t.Fatalf("currency = %+v", res.Order)
	}
	if res.Order.Status != domain.OrderPending || res.Order.Kind != "new" || res.Order.Quantity != 1 {
		t.Fatalf("order = %+v", res.Order)
	}
	if res.Attempt.Status != domain.AttemptPending || res.Attempt.AmountMinor != 1_190_000 {
		t.Fatalf("attempt = %+v", res.Attempt)
	}
	if len(fp.Calls) != 1 || fp.Calls[0].AmountMinor != 1_190_000 || fp.Calls[0].Currency != "VND" {
		t.Fatalf("provider calls = %+v", fp.Calls)
	}
	if res.Attempt.ProviderRef == "" || res.Attempt.QRPayload == "" {
		t.Fatalf("attempt = %+v", res.Attempt)
	}
	if !domain.IsOrderCode(res.Order.Code) {
		t.Fatalf("order code = %q", res.Order.Code)
	}
	// The default order lifetime is 24h.
	if d := time.Until(res.Order.ExpiresAt); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("expires_at = %v (in %v)", res.Order.ExpiresAt, d)
	}
}

func TestCreateOrderExclusiveTaxAddsVAT(t *testing.T) {
	f := newFixture(t, false)
	f.price("sepay", 1_000_000)
	fp := fake.New("sepay")
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fp).providerFactory()}

	res, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: f.email("exclusive"), Provider: "sepay", Quantity: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Order.SubtotalMinor != 1_000_000 || res.Order.TaxMinor != 100_000 || res.Order.TotalMinor != 1_100_000 {
		t.Fatalf("totals = %+v", res.Order)
	}
	if fp.Calls[0].AmountMinor != 1_100_000 {
		t.Fatalf("provider amount = %d, want the tax-inclusive 1,100,000", fp.Calls[0].AmountMinor)
	}
}

func TestCreateOrderQuantityMultipliesPrice(t *testing.T) {
	f := newFixture(t, true)
	f.price("sepay", 500_000)
	fp := fake.New("sepay")
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fp).providerFactory()}

	res, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: f.email("qty"), Provider: "sepay", Quantity: 3,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Order.Quantity != 3 || res.Order.SubtotalMinor != 1_500_000 || res.Order.TotalMinor != 1_500_000 {
		t.Fatalf("order = %+v", res.Order)
	}
	if fp.Calls[0].AmountMinor != 1_500_000 {
		t.Fatalf("provider amount = %d", fp.Calls[0].AmountMinor)
	}
}

// A licence volume is bounded: zero, negative and runaway quantities are
// refused before any order or provider call exists. 2^60 would otherwise
// wrap the subtotal to a handful of dong and hand the buyer a licence
// for nothing.
func TestCreateOrderRejectsInvalidQuantity(t *testing.T) {
	f := newFixture(t, true)
	f.price("sepay", 1_190_000)
	fp := fake.New("sepay")
	fac := newFactory(fp)
	svc := &checkout.Service{Store: f.s, Providers: fac.providerFactory()}
	email := f.email("qty-invalid")

	for _, quantity := range []int{0, -1, checkout.MaxQuantity + 1, 1 << 60} {
		_, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
			PlanID: f.plan.ID, Email: email, Provider: "sepay", Quantity: quantity,
		})
		if !errors.Is(err, checkout.ErrQuantityInvalid) {
			t.Errorf("quantity %d: err = %v, want ErrQuantityInvalid", quantity, err)
		}
	}
	if len(fac.calls) != 0 || len(fp.Calls) != 0 {
		t.Fatalf("provider called for an invalid quantity: factory %v, provider %+v", fac.calls, fp.Calls)
	}
	if n := f.orderCount(strings.ToLower(email)); n != 0 {
		t.Fatalf("orders = %d, want 0", n)
	}

	// The documented cap itself is sellable.
	res, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: f.email("qty-max"), Provider: "sepay", Quantity: checkout.MaxQuantity,
	})
	if err != nil {
		t.Fatalf("quantity %d: %v", checkout.MaxQuantity, err)
	}
	if res.Order.Quantity != checkout.MaxQuantity {
		t.Fatalf("quantity = %d, want %d", res.Order.Quantity, checkout.MaxQuantity)
	}
}

// A price that cannot be multiplied safely is refused, not wrapped into
// a tiny total the customer could pay.
func TestCreateOrderRejectsOverflowingTotal(t *testing.T) {
	f := newFixture(t, true)
	if _, err := f.s.UpsertPlanPrice(f.ctx, f.plan.ID, store.PlanPriceInput{
		Provider: "sepay", Currency: "VND", AmountMinor: math.MaxInt64/2 + 1,
	}); err != nil {
		t.Fatalf("upsert huge price: %v", err)
	}
	fp := fake.New("sepay")
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fp).providerFactory()}

	_, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: f.email("overflow"), Provider: "sepay", Quantity: 3,
	})
	if !errors.Is(err, checkout.ErrQuantityInvalid) {
		t.Fatalf("err = %v, want ErrQuantityInvalid", err)
	}
	if len(fp.Calls) != 0 {
		t.Fatalf("provider called for an overflowing total: %+v", fp.Calls)
	}
	if n := f.orderCount(f.email("overflow")); n != 0 {
		t.Fatalf("orders = %d, want 0", n)
	}
}

func TestCreateOrderRejectsMissingPrice(t *testing.T) {
	f := newFixture(t, true)
	fp := fake.New("sepay")
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fp).providerFactory()}

	_, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: f.email("noprice"), Provider: "sepay", Quantity: 1,
	})
	if !errors.Is(err, checkout.ErrPlanNotSellable) || !errors.Is(err, store.ErrPriceNotConfigured) {
		t.Fatalf("err = %v, want ErrPlanNotSellable wrapping ErrPriceNotConfigured", err)
	}
	if len(fp.Calls) != 0 {
		t.Fatalf("provider called without a price: %+v", fp.Calls)
	}
	if n := f.orderCount(f.email("noprice")); n != 0 {
		t.Fatalf("orders = %d, want 0", n)
	}
}

// Phase 1a sells VND only. A plan priced in USD is not sellable rather
// than silently charged in the wrong currency.
func TestCreateOrderRejectsUnsupportedCurrency(t *testing.T) {
	f := newFixture(t, true)
	if _, err := f.s.UpsertPlanPrice(f.ctx, f.plan.ID, store.PlanPriceInput{
		Provider: "sepay", Currency: "USD", AmountMinor: 4900, CurrencyExponent: 2,
	}); err != nil {
		t.Fatalf("upsert usd price: %v", err)
	}
	fp := fake.New("sepay")
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fp).providerFactory()}

	_, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: f.email("usd"), Provider: "sepay", Quantity: 1,
	})
	if !errors.Is(err, checkout.ErrPlanNotSellable) || !errors.Is(err, store.ErrPriceNotConfigured) {
		t.Fatalf("err = %v, want ErrPlanNotSellable wrapping ErrPriceNotConfigured", err)
	}
	if len(fp.Calls) != 0 {
		t.Fatalf("provider called: %+v", fp.Calls)
	}
}

func TestCreateOrderRejectsPlanNotSellable(t *testing.T) {
	f := newFixture(t, true)
	f.price("sepay", 1_190_000)

	inactive := &model.Plan{
		ProductID: f.product.ID, Name: "Old", Slug: "checkout-old-" + f.suffix,
		LicenseType: "perpetual", LicenseModel: "standard", Active: false,
	}
	if err := f.s.CreatePlan(f.ctx, inactive); err != nil {
		t.Fatalf("create inactive plan: %v", err)
	}

	svc := &checkout.Service{Store: f.s, Providers: newFactory(fake.New("sepay")).providerFactory()}
	for name, in := range map[string]checkout.CreateOrderInput{
		"unknown plan":    {PlanID: "does-not-exist", Email: f.email("unknown"), Provider: "sepay", Quantity: 1},
		"inactive plan":   {PlanID: inactive.ID, Email: f.email("inactive"), Provider: "sepay", Quantity: 1},
		"missing email":   {PlanID: f.plan.ID, Email: "", Provider: "sepay", Quantity: 1},
		"malformed email": {PlanID: f.plan.ID, Email: "not-an-email", Provider: "sepay", Quantity: 1},
	} {
		if _, err := svc.CreateOrder(f.ctx, in); !errors.Is(err, checkout.ErrPlanNotSellable) {
			t.Errorf("%s: err = %v, want ErrPlanNotSellable", name, err)
		}
	}
}

func TestCreateOrderSelectsProvider(t *testing.T) {
	f := newFixture(t, true)
	f.price("payos", 1_190_000)
	fp := fake.New("payos")
	fac := newFactory(fp)
	svc := &checkout.Service{Store: f.s, Providers: fac.providerFactory()}

	res, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: f.email("payos"), Provider: "  PayOS ", Quantity: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(fac.calls) != 1 || fac.calls[0] != f.product.ID+"/payos" {
		t.Fatalf("factory calls = %v, want %q", fac.calls, f.product.ID+"/payos")
	}
	if res.Order.Provider != "payos" || res.Attempt.Provider != "payos" {
		t.Fatalf("provider = %q/%q", res.Order.Provider, res.Attempt.Provider)
	}
	if len(fp.Calls) != 1 || fp.Calls[0].Currency != "VND" {
		t.Fatalf("provider calls = %+v", fp.Calls)
	}
}

func TestCreateOrderUsesDefaultProviderSetting(t *testing.T) {
	f := newFixture(t, true)
	f.price("payos", 1_190_000)
	if err := f.s.SetSetting(f.ctx, "payment_default_provider", "payos"); err != nil {
		t.Fatalf("set setting: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.s.DB.NewDelete().Model((*store.Setting)(nil)).
			Where("key = ?", "payment_default_provider").Exec(context.Background())
	})
	fac := newFactory(fake.New("payos"))
	svc := &checkout.Service{Store: f.s, Providers: fac.providerFactory()}

	res, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: f.email("default"), Quantity: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Order.Provider != "payos" {
		t.Fatalf("provider = %q, want the configured default", res.Order.Provider)
	}
	if len(fac.calls) != 1 || fac.calls[0] != f.product.ID+"/payos" {
		t.Fatalf("factory calls = %v", fac.calls)
	}
}

func TestCreateOrderProviderUnavailable(t *testing.T) {
	f := newFixture(t, true)
	f.price("sepay", 1_190_000)
	fac := newFactory(fake.New("sepay"))
	fac.err = providercfg.ErrProviderUnknown
	fp := fac.prov
	svc := &checkout.Service{Store: f.s, Providers: fac.providerFactory()}

	_, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: f.email("unavailable"), Provider: "stripe", Quantity: 1,
	})
	if !errors.Is(err, checkout.ErrProviderUnavailable) {
		t.Fatalf("err = %v, want ErrProviderUnavailable", err)
	}
	if len(fp.Calls) != 0 {
		t.Fatalf("provider called: %+v", fp.Calls)
	}
	if n := f.orderCount(f.email("unavailable")); n != 0 {
		t.Fatalf("orders = %d, want 0", n)
	}
}

// An install that only sells through PayOS has no install-wide default
// and no SePay price: checkout must discover the one provider that can
// actually serve the plan instead of hardcoding sepay.
func TestCreateOrderFallsBackToPricedProvider(t *testing.T) {
	f := newFixture(t, true)
	f.clearSetting("payment_default_provider")
	f.price("payos", 1_190_000)
	fac := newFactory(fake.New("payos"))
	svc := &checkout.Service{Store: f.s, Providers: fac.providerFactory()}

	res, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: f.email("payos-only"), Quantity: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Order.Provider != "payos" || res.Attempt.Provider != "payos" {
		t.Fatalf("provider = %q/%q, want payos", res.Order.Provider, res.Attempt.Provider)
	}
	if res.Order.TotalMinor != 1_190_000 {
		t.Fatalf("total = %d, want the payos price", res.Order.TotalMinor)
	}
	if len(fac.calls) != 1 || fac.calls[0] != f.product.ID+"/payos" {
		t.Fatalf("factory calls = %v, want only the priced provider", fac.calls)
	}
}

// The configured default wins when both providers are priced and
// configured, even though sepay comes first in the fallback order.
func TestCreateOrderDefaultProviderWinsWhenBothPriced(t *testing.T) {
	f := newFixture(t, true)
	f.price("sepay", 100_000)
	f.price("payos", 200_000)
	f.setSetting("payment_default_provider", "payos")
	fac := newFactory(fake.New("payos"))
	svc := &checkout.Service{Store: f.s, Providers: fac.providerFactory()}

	res, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: f.email("default-both"), Quantity: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Order.Provider != "payos" || res.Order.TotalMinor != 200_000 {
		t.Fatalf("order = %q/%d, want the configured payos price", res.Order.Provider, res.Order.TotalMinor)
	}
	if len(fac.calls) != 1 || fac.calls[0] != f.product.ID+"/payos" {
		t.Fatalf("factory calls = %v, want only payos", fac.calls)
	}
}

// With no setting, the first provider in priority order that has a
// price and a usable config wins: sepay before payos.
func TestCreateOrderFirstPricedProviderWithoutDefault(t *testing.T) {
	f := newFixture(t, true)
	f.clearSetting("payment_default_provider")
	f.price("sepay", 100_000)
	f.price("payos", 200_000)
	fac := newFactory(fake.New("sepay"))
	svc := &checkout.Service{Store: f.s, Providers: fac.providerFactory()}

	res, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: f.email("first-priced"), Quantity: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Order.Provider != "sepay" || res.Order.TotalMinor != 100_000 {
		t.Fatalf("order = %q/%d, want the first priced sepay", res.Order.Provider, res.Order.TotalMinor)
	}
	if len(fac.calls) != 1 || fac.calls[0] != f.product.ID+"/sepay" {
		t.Fatalf("factory calls = %v, want sepay first", fac.calls)
	}
}

// A default provider with no price for this plan is not sellable; the
// fallback still finds the one that is.
func TestCreateOrderUnpricedDefaultFallsBack(t *testing.T) {
	f := newFixture(t, true)
	f.setSetting("payment_default_provider", "payos")
	f.price("sepay", 100_000)
	fac := newFactory(fake.New("sepay"))
	svc := &checkout.Service{Store: f.s, Providers: fac.providerFactory()}

	res, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: f.email("unpriced-default"), Quantity: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Order.Provider != "sepay" {
		t.Fatalf("provider = %q, want the priced sepay", res.Order.Provider)
	}
}

// A retried create must hand back the same order and the same QR: PayOS
// would reject a second link for one orderCode, and a second QR for one
// bank transfer confuses the payer.
func TestCreateOrderReusesPayableAttempt(t *testing.T) {
	f := newFixture(t, true)
	f.price("sepay", 1_190_000)
	fp := fake.New("sepay")
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fp).providerFactory()}
	in := checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: f.email("retry"), Provider: "sepay", Quantity: 1,
	}

	first, err := svc.CreateOrder(f.ctx, in)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := svc.CreateOrder(f.ctx, in)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if second.Order.ID != first.Order.ID || second.Order.Code != first.Order.Code {
		t.Fatalf("second order = %s/%s, want %s/%s",
			second.Order.ID, second.Order.Code, first.Order.ID, first.Order.Code)
	}
	if second.Attempt == nil || second.Attempt.ID != first.Attempt.ID {
		t.Fatalf("second attempt = %+v, want the first %s", second.Attempt, first.Attempt.ID)
	}
	if len(fp.Calls) != 1 {
		t.Fatalf("provider calls = %d, want 1 (the retry must not mint a second QR)", len(fp.Calls))
	}
	if n := f.orderCount(strings.ToLower(in.Email)); n != 1 {
		t.Fatalf("orders = %d, want 1", n)
	}
}

// A provider failure still leaves the order pending, and the attempt is
// recorded as failed so the retry has an audit trail. The retry reuses
// the order and creates a fresh attempt.
func TestCreateOrderProviderFailureMarksAttemptFailed(t *testing.T) {
	f := newFixture(t, true)
	f.price("sepay", 1_190_000)
	fp := fake.New("sepay")
	fp.CreateFn = func(context.Context, provider.CreateAttemptRequest) (provider.CreateAttemptResult, error) {
		return provider.CreateAttemptResult{}, errors.New("provider is down")
	}
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fp).providerFactory()}
	in := checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: f.email("fail"), Provider: "sepay", Quantity: 1,
	}

	if _, err := svc.CreateOrder(f.ctx, in); err == nil {
		t.Fatal("create succeeded while the provider was down")
	} else if !errors.Is(err, checkout.ErrProviderError) {
		t.Fatalf("err = %v, want ErrProviderError", err)
	}

	order := f.onlyOrder(strings.ToLower(in.Email))
	if order.Status != domain.OrderPending {
		t.Fatalf("order status = %q, want pending", order.Status)
	}
	failed, err := f.s.GetLatestPaymentAttempt(f.ctx, order.ID)
	if err != nil {
		t.Fatalf("latest attempt: %v", err)
	}
	if failed.Status != domain.AttemptFailed || failed.QRPayload != "" {
		t.Fatalf("attempt = %+v, want a failed attempt with no QR", failed)
	}

	// The provider recovers: the retry reuses the same order and writes
	// a new attempt instead of a second order the buyer never sees.
	fp.CreateFn = nil
	second, err := svc.CreateOrder(f.ctx, in)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if second.Order.ID != order.ID {
		t.Fatalf("retry order = %s, want the original %s", second.Order.ID, order.ID)
	}
	if second.Attempt.Status != domain.AttemptPending || second.Attempt.ID == failed.ID {
		t.Fatalf("retry attempt = %+v, want a new pending attempt", second.Attempt)
	}
	if n := f.orderCount(strings.ToLower(in.Email)); n != 1 {
		t.Fatalf("orders = %d, want 1", n)
	}
	if len(fp.Calls) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(fp.Calls))
	}
}

// PayOS signs returnUrl and cancelUrl into the payment link, so both
// must be absolute and carry the order code; SePay ignores them.
func TestCreateOrderBuildsReturnAndCancelURLs(t *testing.T) {
	f := newFixture(t, true)
	f.price("payos", 1_190_000)
	fp := fake.New("payos")
	svc := &checkout.Service{
		Store: f.s, Providers: newFactory(fp).providerFactory(),
		BaseURL: "https://keygate.example.com/",
	}

	res, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: f.email("urls"), Provider: "payos", Quantity: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(fp.Calls) != 1 {
		t.Fatalf("provider calls = %d", len(fp.Calls))
	}
	got := fp.Calls[0]
	wantReturn := "https://keygate.example.com/checkout/success?code=" + res.Order.Code
	wantCancel := wantReturn + "&canceled=1"
	if got.ReturnURL != wantReturn {
		t.Errorf("ReturnURL = %q, want %q", got.ReturnURL, wantReturn)
	}
	if got.CancelURL != wantCancel {
		t.Errorf("CancelURL = %q, want %q", got.CancelURL, wantCancel)
	}
	if got.OrderCode != res.Order.Code || got.NumericCode != res.Order.NumericCode {
		t.Errorf("create request codes = %q/%d, order %q/%d",
			got.OrderCode, got.NumericCode, res.Order.Code, res.Order.NumericCode)
	}
	if got.NumericCode == 0 {
		t.Error("NumericCode is zero; PayOS needs the database identity value")
	}
}

func TestCreateOrderPersistsMetadataAndBuyer(t *testing.T) {
	f := newFixture(t, true)
	f.price("sepay", 1_190_000)
	fp := fake.New("sepay")
	fp.CreateFn = func(_ context.Context, req provider.CreateAttemptRequest) (provider.CreateAttemptResult, error) {
		return provider.CreateAttemptResult{
			ProviderRef: "ref-" + req.OrderCode, QRPayload: "qr",
			QRImageURL:  "https://img.example.com/" + req.OrderCode + ".png",
			Description: req.Description, ExpiresAt: req.ExpiresAt,
		}, nil
	}
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fp).providerFactory()}
	buyer := provider.Buyer{
		Name: "Nguyen Van A", Email: "buyer@example.com", Phone: "+84901234567",
		TaxID: "0101234567", Address: "1 Le Loi, District 1, HCMC",
	}
	email := "  Buyer-" + f.suffix + "@Example.COM "
	res, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: email, Provider: "sepay", Quantity: 1,
		Buyer:    buyer,
		Metadata: map[string]any{"workspace_id": "ws_123", "test": true},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	wantEmail := strings.ToLower(strings.TrimSpace(email))
	if res.Order.Email != wantEmail {
		t.Fatalf("order email = %q, want %q", res.Order.Email, wantEmail)
	}
	if fp.Calls[0].Buyer != buyer {
		t.Fatalf("provider buyer = %+v", fp.Calls[0].Buyer)
	}

	stored, err := f.s.GetPaymentOrderByCode(f.ctx, res.Order.Code)
	if err != nil {
		t.Fatalf("re-read order: %v", err)
	}
	var gotBuyer provider.Buyer
	if err := json.Unmarshal(stored.Buyer, &gotBuyer); err != nil {
		t.Fatalf("decode buyer: %v", err)
	}
	if gotBuyer != buyer {
		t.Fatalf("stored buyer = %+v, want %+v", gotBuyer, buyer)
	}
	var gotMeta map[string]any
	if err := json.Unmarshal(stored.Metadata, &gotMeta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if gotMeta["workspace_id"] != "ws_123" || gotMeta["test"] != true {
		t.Fatalf("stored metadata = %v", gotMeta)
	}

	attempt, err := f.s.GetLatestPaymentAttempt(f.ctx, stored.ID)
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	var raw struct {
		Bank        provider.BankAccount `json:"bank"`
		Description string               `json:"description"`
		QRImageURL  string               `json:"qr_image_url"`
	}
	if err := json.Unmarshal(attempt.Raw, &raw); err != nil {
		t.Fatalf("decode attempt raw: %v", err)
	}
	if raw.Description != res.Order.Code {
		t.Fatalf("attempt raw description = %q, want the order code", raw.Description)
	}
	if raw.QRImageURL != "https://img.example.com/"+res.Order.Code+".png" {
		t.Fatalf("attempt raw qr image = %q", raw.QRImageURL)
	}
}

func TestCreateOrderUsesOrderTTLSetting(t *testing.T) {
	f := newFixture(t, true)
	f.price("sepay", 1_190_000)
	if err := f.s.SetSetting(f.ctx, "payment_order_ttl_minutes", "30"); err != nil {
		t.Fatalf("set setting: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.s.DB.NewDelete().Model((*store.Setting)(nil)).
			Where("key = ?", "payment_order_ttl_minutes").Exec(context.Background())
	})
	svc := &checkout.Service{Store: f.s, Providers: newFactory(fake.New("sepay")).providerFactory()}

	res, err := svc.CreateOrder(f.ctx, checkout.CreateOrderInput{
		PlanID: f.plan.ID, Email: f.email("ttl"), Provider: "sepay", Quantity: 1,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if d := time.Until(res.Order.ExpiresAt); d < 29*time.Minute || d > 31*time.Minute {
		t.Fatalf("expires_at is %v away, want about 30m", d)
	}
	if res.Attempt.ExpiresAt == nil || !res.Attempt.ExpiresAt.Equal(res.Order.ExpiresAt) {
		t.Fatalf("attempt expiry = %v, order expiry = %v", res.Attempt.ExpiresAt, res.Order.ExpiresAt)
	}
}

func (f *fixture) orderCount(email string) int {
	f.t.Helper()
	n, err := f.s.DB.NewSelect().Model((*model.PaymentOrder)(nil)).
		Where("email = ?", email).Count(f.ctx)
	if err != nil {
		f.t.Fatalf("count orders: %v", err)
	}
	return n
}

func (f *fixture) onlyOrder(email string) *model.PaymentOrder {
	f.t.Helper()
	order := new(model.PaymentOrder)
	if err := f.s.DB.NewSelect().Model(order).
		Where("email = ?", email).Order("created_at DESC").Limit(1).Scan(f.ctx); err != nil {
		f.t.Fatalf("read order: %v", err)
	}
	return order
}

// setSetting writes one install setting and removes it again when the
// test ends: the integration database outlives one run and a leftover
// default provider would steer every later test in this package.
func (f *fixture) setSetting(key, value string) {
	f.t.Helper()
	if err := f.s.SetSetting(f.ctx, key, value); err != nil {
		f.t.Fatalf("set setting %q: %v", key, err)
	}
	f.t.Cleanup(func() { f.clearSetting(key) })
}

func (f *fixture) clearSetting(key string) {
	f.t.Helper()
	if _, err := f.s.DB.NewDelete().Model((*store.Setting)(nil)).
		Where("key = ?", key).Exec(f.ctx); err != nil {
		f.t.Fatalf("clear setting %q: %v", key, err)
	}
}
