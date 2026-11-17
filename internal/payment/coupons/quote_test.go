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
