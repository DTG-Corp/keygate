package domain_test

import (
	"testing"

	"github.com/tabloy/keygate/internal/payment/domain"
)

// The CHECK vocabularies from db/migrations/20261112_03_payment_core.up.sql,
// kept as literal sets so a constant that the database would reject fails
// here instead of at INSERT time.
var (
	orderStatusCheck = map[string]bool{
		"pending":            true,
		"paid":               true,
		"expired":            true,
		"canceled":           true,
		"refunded":           true,
		"partially_refunded": true,
	}
	attemptStatusCheck = map[string]bool{
		"created":   true,
		"pending":   true,
		"succeeded": true,
		"failed":    true,
		"expired":   true,
		"canceled":  true,
	}
	txStatusCheck = map[string]bool{
		"matched":   true,
		"unmatched": true,
		"ignored":   true,
	}
)

func TestIsOrderTerminal(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   bool
	}{
		{"pending", domain.OrderPending, false},
		{"paid", domain.OrderPaid, true},
		{"expired", domain.OrderExpired, true},
		{"canceled", domain.OrderCanceled, true},
		{"refunded", domain.OrderRefunded, true},
		{"partially_refunded", domain.OrderPartiallyRefunded, true},
		{"empty", "", false},
		{"unknown", "unknown", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := domain.IsOrderTerminal(tc.status); got != tc.want {
				t.Errorf("IsOrderTerminal(%q) = %t, want %t", tc.status, got, tc.want)
			}
		})
	}
}

func TestOrderStatusConstantsMatchSchema(t *testing.T) {
	constants := map[string]string{
		"OrderPending":           domain.OrderPending,
		"OrderPaid":              domain.OrderPaid,
		"OrderExpired":           domain.OrderExpired,
		"OrderCanceled":          domain.OrderCanceled,
		"OrderRefunded":          domain.OrderRefunded,
		"OrderPartiallyRefunded": domain.OrderPartiallyRefunded,
	}
	for name, value := range constants {
		if !orderStatusCheck[value] {
			t.Errorf("%s = %q is outside payment_orders.status CHECK", name, value)
		}
	}
}

func TestAttemptStatusConstantsMatchSchema(t *testing.T) {
	// "paid" is an order status; the payment_attempts.status CHECK only
	// allows "succeeded" for a successful payment.
	if got, want := domain.AttemptSucceeded, "succeeded"; got != want {
		t.Fatalf("AttemptSucceeded = %q, want %q", got, want)
	}
	constants := map[string]string{
		"AttemptCreated":   domain.AttemptCreated,
		"AttemptPending":   domain.AttemptPending,
		"AttemptSucceeded": domain.AttemptSucceeded,
		"AttemptFailed":    domain.AttemptFailed,
		"AttemptExpired":   domain.AttemptExpired,
		"AttemptCanceled":  domain.AttemptCanceled,
	}
	for name, value := range constants {
		if !attemptStatusCheck[value] {
			t.Errorf("%s = %q is outside payment_attempts.status CHECK", name, value)
		}
	}
}

func TestTxStatusConstantsMatchSchema(t *testing.T) {
	// Refunds are rows in payment_refunds; payment_transactions.status has
	// no "refunded" value.
	constants := map[string]string{
		"TxMatched":   domain.TxMatched,
		"TxUnmatched": domain.TxUnmatched,
		"TxIgnored":   domain.TxIgnored,
	}
	for name, value := range constants {
		if !txStatusCheck[value] {
			t.Errorf("%s = %q is outside payment_transactions.status CHECK", name, value)
		}
	}
}
