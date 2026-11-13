package domain

const (
	OrderPending           = "pending"
	OrderPaid              = "paid"
	OrderExpired           = "expired"
	OrderCanceled          = "canceled"
	OrderRefunded          = "refunded"
	OrderPartiallyRefunded = "partially_refunded"

	AttemptCreated  = "created"
	AttemptPending  = "pending"
	AttemptPaid     = "paid"
	AttemptFailed   = "failed"
	AttemptExpired  = "expired"
	AttemptCanceled = "canceled"

	TxMatched   = "matched"
	TxUnmatched = "unmatched"
	TxIgnored   = "ignored"
	TxRefunded  = "refunded"
)

// IsOrderTerminal reports whether an order can no longer be paid. A
// terminal order that a provider later reports as paid must not move:
// the settlement service records the transaction for the operator
// instead of granting a licence.
func IsOrderTerminal(status string) bool {
	switch status {
	case OrderPaid, OrderExpired, OrderCanceled, OrderRefunded:
		return true
	}
	return false
}
