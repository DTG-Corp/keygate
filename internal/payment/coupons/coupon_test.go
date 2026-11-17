package coupons

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestCouponErrorCodeConstants pins the values handlers write as literals:
// the AST contract test refuses variables at the response.Err call site, so
// these strings and the handler's literals have to agree by value. Adding a
// code here without the handler (or the reverse) would emit an
// undocumented answer, which is what the handler-side
// TestCouponCodesMatchConstants checks from the other side.
func TestCouponErrorCodeConstants(t *testing.T) {
	all := []string{
		CodeInvalid, CodeExhausted, CodeStackLimit, CodeGroupConflict,
		CodeNotStackable, CodeTrialMixed, CodeAssigned, CodeCodeTaken,
	}
	seen := make(map[string]bool, len(all))
	for _, code := range all {
		if !strings.HasPrefix(code, "PAYMENT_COUPON_") {
			t.Errorf("%q is not a PAYMENT_COUPON_ code", code)
		}
		if seen[code] {
			t.Errorf("%q is declared twice", code)
		}
		seen[code] = true
		if got := ErrorCode(reject(code, "rejected")); got != code {
			t.Errorf("ErrorCode(reject(%q)) = %q", code, got)
		}
	}

	// A wrapped rejection keeps its code: callers wrap with context.
	wrapped := fmt.Errorf("apply %s: %w", "GEN10", reject(CodeExhausted, "used up"))
	if got := ErrorCode(wrapped); got != CodeExhausted {
		t.Errorf("wrapped code = %q", got)
	}
	if got := ErrorCode(errors.New("plain")); got != "" {
		t.Errorf("non-coupon error code = %q, want empty", got)
	}
	if got := ErrorCode(nil); got != "" {
		t.Errorf("nil code = %q", got)
	}
	// Error() is the operator-facing message, never a bare code.
	if msg := reject(CodeInvalid, "%s is unknown", "NOPE").Error(); msg != "NOPE is unknown" {
		t.Errorf("message = %q", msg)
	}
}
