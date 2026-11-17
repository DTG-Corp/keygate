// Package domain holds provider-neutral payment value types: money and
// VAT maths, order codes, and the status vocabulary shared by every
// rail. It depends only on the standard library.
package domain

import "strings"

// CurrencyExponent is the number of minor-unit digits for a currency.
// VND has none; USD/EUR have two. An unknown currency defaults to 2, the
// same default the schema uses.
func CurrencyExponent(currency string) int16 {
	switch strings.ToUpper(strings.TrimSpace(currency)) {
	case "VND", "JPY", "KRW":
		return 0
	default:
		return 2
	}
}

// Totals is the money breakdown of an order. Every field is in the
// order's minor units.
type Totals struct {
	NetMinor      int64
	TaxMinor      int64
	TotalMinor    int64
	DiscountMinor int64
}

// ComputeTotals splits a subtotal (after discount) into net + tax.
//
//	priceIncludesTax: total = subtotal-discount; net = round(total*10000/(10000+rate))
//	otherwise:        net = subtotal-discount; tax = round(net*rate/10000)
//
// Rounding is half-up on integer minor units so net+tax always equals
// total; a floated percentage would drift and the invoice would not add
// up. Discount is clamped to the subtotal.
func ComputeTotals(subtotalMinor, discountMinor int64, taxRateBP int, priceIncludesTax bool) Totals {
	if discountMinor < 0 {
		discountMinor = 0
	}
	if discountMinor > subtotalMinor {
		discountMinor = subtotalMinor
	}
	after := subtotalMinor - discountMinor
	if taxRateBP < 0 {
		taxRateBP = 0
	}
	if priceIncludesTax {
		den := int64(10000 + taxRateBP)
		net := (after*10000 + den/2) / den
		return Totals{NetMinor: net, TaxMinor: after - net, TotalMinor: after, DiscountMinor: discountMinor}
	}
	tax := (after*int64(taxRateBP) + 5000) / 10000
	return Totals{NetMinor: after, TaxMinor: tax, TotalMinor: after + tax, DiscountMinor: discountMinor}
}
