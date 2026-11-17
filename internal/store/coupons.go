package store

import "strings"

// NormaliseCouponCode is the one spelling a coupon code is stored and
// looked up by. Operators type codes with stray spaces and buyers type
// them in any case; both must reach the same row.
func NormaliseCouponCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}
