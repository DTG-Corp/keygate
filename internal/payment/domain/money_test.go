package domain_test

import (
	"testing"

	"github.com/tabloy/keygate/internal/payment/domain"
)

func TestComputeTotalsTaxInclusive(t *testing.T) {
	// 1.190.000 ₫ inclusive of 10% VAT.
	got := domain.ComputeTotals(1_190_000, 0, 1000, true)
	if got.TotalMinor != 1_190_000 || got.NetMinor != 1_081_818 || got.TaxMinor != 108_182 {
		t.Fatalf("totals = %+v, want net 1081818 tax 108182 total 1190000", got)
	}
	// The breakdown must add back to the total exactly.
	if got.NetMinor+got.TaxMinor != got.TotalMinor {
		t.Fatalf("net+tax=%d != total=%d", got.NetMinor+got.TaxMinor, got.TotalMinor)
	}
}

func TestComputeTotalsTaxExclusiveWithDiscount(t *testing.T) {
	got := domain.ComputeTotals(1_000_000, 200_000, 1000, false)
	if got.TotalMinor != 880_000 || got.TaxMinor != 80_000 || got.NetMinor != 800_000 {
		t.Fatalf("totals = %+v, want net 800000 tax 80000 total 880000", got)
	}
}

func TestComputeTotalsZeroRate(t *testing.T) {
	got := domain.ComputeTotals(500_000, 0, 0, true)
	if got.TotalMinor != 500_000 || got.TaxMinor != 0 || got.NetMinor != 500_000 {
		t.Fatalf("totals = %+v, want no tax", got)
	}
}

func TestCurrencyExponents(t *testing.T) {
	cases := map[string]int16{"VND": 0, "vnd": 0, "JPY": 0, "USD": 2, "EUR": 2, "": 2}
	for currency, want := range cases {
		if got := domain.CurrencyExponent(currency); got != want {
			t.Fatalf("%q exponent = %d, want %d", currency, got, want)
		}
	}
}

func TestComputeTotalsTaxExclusiveHalfUpRound(t *testing.T) {
	// 1 minor unit at 50% exclusive is the smallest half-up case: 0.5
	// rounds up to 1, never to 0.
	got := domain.ComputeTotals(1, 0, 5000, false)
	if got.NetMinor != 1 || got.TaxMinor != 1 || got.TotalMinor != 2 || got.DiscountMinor != 0 {
		t.Fatalf("totals = %+v, want net 1 tax 1 total 2 discount 0", got)
	}
}

func TestComputeTotalsDiscountClamped(t *testing.T) {
	// A discount larger than the subtotal cannot make the total negative.
	got := domain.ComputeTotals(100, 500, 1000, false)
	if got.NetMinor != 0 || got.TaxMinor != 0 || got.TotalMinor != 0 || got.DiscountMinor != 100 {
		t.Fatalf("totals = %+v, want total 0 discount clamped to 100", got)
	}
	// A negative discount is treated as none.
	got = domain.ComputeTotals(100, -50, 0, true)
	if got.NetMinor != 100 || got.TaxMinor != 0 || got.TotalMinor != 100 || got.DiscountMinor != 0 {
		t.Fatalf("totals = %+v, want total 100 no discount", got)
	}
}

func TestComputeTotalsNegativeTaxRateClamped(t *testing.T) {
	got := domain.ComputeTotals(1000, 0, -500, false)
	if got.NetMinor != 1000 || got.TaxMinor != 0 || got.TotalMinor != 1000 || got.DiscountMinor != 0 {
		t.Fatalf("totals = %+v, want no tax", got)
	}
}

func TestComputeTotalsEchoesDiscount(t *testing.T) {
	got := domain.ComputeTotals(1_000_000, 200_000, 1000, false)
	if got.DiscountMinor != 200_000 {
		t.Fatalf("DiscountMinor = %d, want 200000", got.DiscountMinor)
	}
}
