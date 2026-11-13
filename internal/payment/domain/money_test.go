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
