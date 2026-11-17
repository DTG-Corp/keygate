package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
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

// TestCreateCouponPersistsExplicitZeroAndFalse pins the insert against bun's
// default-placeholder behaviour: a zero Go value on a `default:` column is
// written as SQL DEFAULT, which would silently turn "not stackable",
// "priority 0" and "inactive" into the column defaults.
func TestCreateCouponPersistsExplicitZeroAndFalse(t *testing.T) {
	s := couponTestStore(t)
	ctx := context.Background()
	in := baseCouponInput("zero-" + couponSuffix())
	in.Priority = 0
	in.Stackable = false
	in.Active = false
	c, err := s.CreateCoupon(ctx, in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetCoupon(ctx, c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Active {
		t.Fatalf("active = true, want false")
	}
	if got.Stackable {
		t.Fatalf("stackable = true, want false")
	}
	if got.Priority != 0 {
		t.Fatalf("priority = %d, want 0", got.Priority)
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
			if _, err := s.CreateCoupon(ctx, in); !errors.Is(err, ErrCouponInvalid) {
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
	if _, err := s.CreateCoupon(ctx, baseCouponInput(strings.ToUpper(code))); !errors.Is(err, ErrCouponCodeTaken) {
		t.Fatalf("err = %v, want ErrCouponCodeTaken", err)
	}
}

func TestUpdateCouponReplacesScopes(t *testing.T) {
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

func TestDeleteArchivesWhenRedeemed(t *testing.T) {
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

func TestRedeemCouponsForOrderIsIdempotentAndCounts(t *testing.T) {
	s := couponTestStore(t)
	ctx := context.Background()
	c, err := s.CreateCoupon(ctx, baseCouponInput("ledger-"+couponSuffix()))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	order := createOrderFixture(t, s, "ledger-"+c.ID)
	in := []RedemptionInput{{
		CouponID: c.ID, CouponCode: c.Code, OrderID: order.ID,
		Email: "Buyer@Example.com", Type: "percent", AmountMinor: 100,
	}}
	if err := s.RedeemCouponsForOrder(ctx, in); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if err := s.RedeemCouponsForOrder(ctx, in); err != nil {
		t.Fatalf("redeem twice: %v", err)
	}
	got, err := s.GetCoupon(ctx, c.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.RedeemedCount != 1 {
		t.Fatalf("redeemed_count = %d, want 1", got.RedeemedCount)
	}
	stats, err := s.CouponRedemptionStats(ctx, c.ID)
	if err != nil || stats.Count != 1 || stats.AmountMinor != 100 {
		t.Fatalf("stats = %+v err = %v", stats, err)
	}
}

func TestListOrderDiscountsReturnsTheSnapshot(t *testing.T) {
	s := couponTestStore(t)
	ctx := context.Background()
	c, err := s.CreateCoupon(ctx, baseCouponInput("snap-"+couponSuffix()))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	game := createOrderFixture(t, s, "snap-"+c.ID)
	order, err := s.CreatePaymentOrder(ctx, CreateOrderInput{
		ProductID: game.ProductID, PlanID: game.PlanID, Email: game.Email, Kind: "new", Quantity: 1,
		SubtotalMinor: 1000, DiscountMinor: 100, TaxMinor: 90, TotalMinor: 900,
		Currency: "VND", Provider: "sepay", ExpiresAt: time.Now().Add(time.Hour),
		Buyer: []byte(`{}`), Metadata: []byte(`{}`),
		Discounts: []OrderDiscount{{
			CouponID: c.ID, CouponCode: c.Code, Type: "percent", Value: 1000,
			AmountMinor: 100, Priority: 100,
		}},
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DB.NewRaw("DELETE FROM payment_order_discounts WHERE order_id = ?", order.ID).Exec(context.Background())
		_, _ = s.DB.NewRaw("DELETE FROM payment_orders WHERE id = ?", order.ID).Exec(context.Background())
	})
	rows, err := s.ListOrderDiscounts(ctx, order.ID)
	if err != nil {
		t.Fatalf("discounts: %v", err)
	}
	if len(rows) != 1 || rows[0].CouponCode != c.Code || rows[0].AmountMinor != 100 || rows[0].CouponID != c.ID {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestListRedemptionsFilters(t *testing.T) {
	s := couponTestStore(t)
	ctx := context.Background()
	c, err := s.CreateCoupon(ctx, baseCouponInput("report-"+couponSuffix()))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	orderA := createOrderFixture(t, s, "report-a-"+c.ID)
	orderB := createOrderFixture(t, s, "report-b-"+c.ID)
	if err := s.RedeemCouponsForOrder(ctx, []RedemptionInput{
		{CouponID: c.ID, CouponCode: c.Code, OrderID: orderA.ID, Email: "a@example.com", Type: "percent", AmountMinor: 10},
		{CouponID: c.ID, CouponCode: c.Code, OrderID: orderB.ID, Email: "b@example.com", Type: "percent", AmountMinor: 20},
	}); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	rows, total, err := s.ListRedemptions(ctx, RedemptionFilter{CouponID: c.ID, Page: Page{Limit: 10}})
	if err != nil || total != 2 || len(rows) != 2 {
		t.Fatalf("rows = %+v total = %d err = %v", rows, total, err)
	}
	if rows[0].OrderCode == "" || rows[0].Currency != "VND" {
		t.Fatalf("row lacks the order join: %+v", rows[0])
	}
	rows, total, err = s.ListRedemptions(ctx, RedemptionFilter{CouponCode: c.Code, Email: "A@example.com", Page: Page{Limit: 10}})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].Email != "a@example.com" {
		t.Fatalf("email filter rows = %+v total = %d err = %v", rows, total, err)
	}
	future := time.Now().Add(time.Hour)
	rows, total, err = s.ListRedemptions(ctx, RedemptionFilter{CouponID: c.ID, From: &future, Page: Page{Limit: 10}})
	if err != nil || total != 0 || len(rows) != 0 {
		t.Fatalf("from filter rows = %+v total = %d err = %v", rows, total, err)
	}
}

// ─── fixtures ───

// modelProduct creates a product the scope tests can point at.
func modelProduct(t *testing.T, s *Store, suffix, name string) *model.Product {
	t.Helper()
	p := &model.Product{Name: "Coupons " + name, Slug: "coupons-" + name + "-" + suffix + "-" + uuid.NewString()[:6], Type: "desktop"}
	if err := s.CreateProduct(context.Background(), p); err != nil {
		t.Fatalf("create product: %v", err)
	}
	return p
}

// createOrderFixture writes the minimal order a redemption row needs and
// returns it. Every identifier carries a UUID: the integration database is
// shared by every package and outlives one run.
func createOrderFixture(t *testing.T, s *Store, tag string) *model.PaymentOrder {
	t.Helper()
	ctx := context.Background()
	unique := uuid.NewString()[:13]
	product := &model.Product{
		Name: "Redemption fixture", Slug: "redemption-" + unique, Type: "desktop",
	}
	if err := s.CreateProduct(ctx, product); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{
		ProductID: product.ID, Name: "Redemption fixture", Slug: "redemption-plan-" + unique,
		LicenseType: "perpetual", LicenseModel: "standard", MaxActivations: 1, Active: true,
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	order, err := s.CreatePaymentOrder(ctx, CreateOrderInput{
		ProductID: product.ID, PlanID: plan.ID, Email: "fixture-" + unique + "@example.com",
		Kind: "new", Quantity: 1, SubtotalMinor: 100, TotalMinor: 100, Currency: "VND",
		Provider: "sepay", ExpiresAt: time.Now().Add(time.Hour),
		Buyer: []byte("{}"), Metadata: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DB.NewRaw("DELETE FROM coupon_redemptions WHERE order_id = ?", order.ID).Exec(context.Background())
		_, _ = s.DB.NewRaw("DELETE FROM payment_orders WHERE id = ?", order.ID).Exec(context.Background())
		_, _ = s.DB.NewRaw("DELETE FROM plans WHERE id = ?", plan.ID).Exec(context.Background())
		_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", product.ID).Exec(context.Background())
	})
	return order
}

// insertRedemptionFixture writes the minimal order + redemption rows this
// test needs. It bypasses checkout on purpose: the store's counting rule
// is what is under test.
func (s *Store) insertRedemptionFixture(ctx context.Context, couponID, code, email, externalID, tag string) error {
	unique := uuid.NewString()[:13]
	product := &model.Product{
		Name: "Redemption fixture", Slug: "redemption-fixture-" + unique, Type: "desktop",
	}
	if err := s.CreateProduct(ctx, product); err != nil {
		return err
	}
	plan := &model.Plan{
		ProductID: product.ID, Name: "Redemption fixture", Slug: "redemption-fixture-plan-" + unique,
		LicenseType: "perpetual", LicenseModel: "standard", MaxActivations: 1, Active: true,
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		return err
	}
	order := &model.PaymentOrder{
		ID: NewID(), Code: "KGFIX" + strings.ToUpper(unique), ProductID: product.ID, PlanID: plan.ID,
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
	if _, err := s.DB.NewInsert().Model(row).Exec(ctx); err != nil {
		return err
	}
	return nil
}
