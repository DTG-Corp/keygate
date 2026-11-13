package domain

// Order statuses mirror the payment_orders.status CHECK.
const (
	OrderPending           = "pending"
	OrderPaid              = "paid"
	OrderExpired           = "expired"
	OrderCanceled          = "canceled"
	OrderRefunded          = "refunded"
	OrderPartiallyRefunded = "partially_refunded"
)

// Attempt statuses mirror the payment_attempts.status CHECK. A payment
// that went through is "succeeded", not "paid": "paid" is an order
// status and the attempt CHECK does not allow it.
const (
	AttemptCreated   = "created"
	AttemptPending   = "pending"
	AttemptSucceeded = "succeeded"
	AttemptFailed    = "failed"
	AttemptExpired   = "expired"
	AttemptCanceled  = "canceled"
)

// Transaction statuses mirror the payment_transactions.status CHECK.
// Refunds are rows in payment_refunds, not a transaction status.
const (
	TxMatched   = "matched"
	TxUnmatched = "unmatched"
	TxIgnored   = "ignored"
)

// IsOrderTerminal reports whether an order can no longer be paid. Paid,
// expired, canceled, refunded and partially_refunded are all terminal: a
// partially refunded order was paid first, so settlement must not treat
// it as payable again. When a provider later reports a payment for a
// terminal order, settlement records the transaction for the operator
// instead of granting a licence.
func IsOrderTerminal(status string) bool {
	switch status {
	case OrderPaid, OrderExpired, OrderCanceled, OrderRefunded, OrderPartiallyRefunded:
		return true
	}
	return false
}
