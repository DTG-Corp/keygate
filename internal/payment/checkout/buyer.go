package checkout

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/dtg-corp/dsoft/internal/payment/provider"
)

// ErrTaxCodeRequired means the buyer asked for a VAT invoice without a
// usable tax code (spec §6.2). The request is refused before anything is
// written: an invoice that cannot be issued is worse than a rejected
// checkout.
var ErrTaxCodeRequired = errors.New("a tax code is required to request an invoice")

// validateBuyer enforces the buyer rules checkout owns. Only one rule
// exists so far: requesting an invoice requires a Vietnamese tax code.
func validateBuyer(b provider.Buyer) error {
	if !b.RequestInvoice {
		return nil
	}
	if !validTaxCode(normaliseTaxCode(b.TaxCode)) {
		return fmt.Errorf("%w: %q is not a tax code of 10 to 13 digits", ErrTaxCodeRequired, b.TaxCode)
	}
	return nil
}

// normaliseTaxCode strips the separators a buyer types — spaces, dots,
// dashes, slashes — so the stored value is the digits an invoice is issued
// against.
func normaliseTaxCode(code string) string {
	var b strings.Builder
	b.Grow(len(code))
	for _, r := range code {
		switch {
		case unicode.IsDigit(r):
			b.WriteRune(r)
		case r == ' ' || r == '.' || r == '-' || r == '/':
			// Typed for readability; not part of the code.
		default:
			// Any other character makes the code invalid; keep it so the
			// validator can say so rather than silently accepting it.
			b.WriteRune(r)
		}
	}
	return b.String()
}

// validTaxCode reports whether a normalised code is 10 to 13 digits.
func validTaxCode(code string) bool {
	if len(code) < 10 || len(code) > 13 {
		return false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
