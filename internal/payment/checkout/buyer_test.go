package checkout

import (
	"errors"
	"testing"

	"github.com/dtg-corp/dsoft/internal/payment/provider"
)

// The tax code rule is Vietnamese: 10 digits for a company, 13 for a
// branch, and buyers type it with the separators the registration
// certificate prints.
func TestValidateBuyerTaxCode(t *testing.T) {
	cases := map[string]struct {
		buyer provider.Buyer
		ok    bool
	}{
		"no invoice requested": {provider.Buyer{}, true},
		"ten digits":           {provider.Buyer{RequestInvoice: true, TaxCode: "0101234567"}, true},
		"thirteen digits":      {provider.Buyer{RequestInvoice: true, TaxCode: "0101234567-001"}, true},
		"spaced and dotted":    {provider.Buyer{RequestInvoice: true, TaxCode: "0101 234 567.001"}, true},
		"too short":            {provider.Buyer{RequestInvoice: true, TaxCode: "01012345"}, false},
		"too long":             {provider.Buyer{RequestInvoice: true, TaxCode: "01012345678901"}, false},
		"letters":              {provider.Buyer{RequestInvoice: true, TaxCode: "010123456A"}, false},
		"missing":              {provider.Buyer{RequestInvoice: true}, false},
	}
	for name, tt := range cases {
		err := validateBuyer(tt.buyer)
		if tt.ok && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !tt.ok && !errors.Is(err, ErrTaxCodeRequired) {
			t.Errorf("%s: err = %v, want ErrTaxCodeRequired", name, err)
		}
	}
	if got := normaliseTaxCode(" 010 123-4567.001 "); got != "0101234567001" {
		t.Fatalf("normaliseTaxCode = %q", got)
	}
}
