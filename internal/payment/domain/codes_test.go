package domain_test

import (
	"regexp"
	"testing"

	"github.com/tabloy/keygate/internal/payment/domain"
)

// Crockford Base32 excludes I, L, O and U so a code read aloud or typed
// from a bank memo cannot be confused.
var orderCodeRE = regexp.MustCompile(`^KG[0-9A-HJKMNP-TV-Z]{6}$`)

func TestNewOrderCodeFormat(t *testing.T) {
	for i := 0; i < 500; i++ {
		code := domain.NewOrderCode()
		if !orderCodeRE.MatchString(code) {
			t.Fatalf("code %q does not match %s", code, orderCodeRE)
		}
		if !domain.IsOrderCode(code) {
			t.Fatalf("IsOrderCode(%q) = false", code)
		}
	}
}

func TestNewOrderCodeEntropy(t *testing.T) {
	seen := make(map[string]bool, 5000)
	for i := 0; i < 5000; i++ {
		code := domain.NewOrderCode()
		if seen[code] {
			t.Fatalf("duplicate code %q after %d draws", code, i)
		}
		seen[code] = true
	}
}

func TestIsOrderCodeRejects(t *testing.T) {
	for _, bad := range []string{"", "KG7F3K2", "kg7f3k2q", "KG7F3K2I", "KG7F3K2O", "XX7F3K2Q"} {
		if domain.IsOrderCode(bad) {
			t.Fatalf("IsOrderCode(%q) = true, want false", bad)
		}
	}
}
