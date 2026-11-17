// Package coupons applies discount and trial codes to an order price.
//
// One rule set serves every caller: checkout, the admin quote preview and
// the portal preview all call Quote, so what a buyer is shown is what the
// order is written with (spec §3.9). The pure rules live in quote.go and
// depend on nothing but the model; service.go adds the database checks.
package coupons

import (
	"errors"
	"fmt"
)

// Rejection codes. One code ⇔ one HTTP status (spec §6.5), pinned in
// internal/handler/payments_contract_test.go. Handlers must write the
// literal string at the response.Err call site because that test parses
// the AST; TestCouponErrorCodeConstants keeps these values in step.
const (
	CodeInvalid       = "PAYMENT_COUPON_INVALID"        // 422
	CodeExhausted     = "PAYMENT_COUPON_EXHAUSTED"      // 409
	CodeStackLimit    = "PAYMENT_COUPON_STACK_LIMIT"    // 422
	CodeGroupConflict = "PAYMENT_COUPON_GROUP_CONFLICT" // 422
	CodeNotStackable  = "PAYMENT_COUPON_NOT_STACKABLE"  // 422
	CodeTrialMixed    = "PAYMENT_COUPON_TRIAL_MIXED"    // 422
	CodeAssigned      = "PAYMENT_COUPON_ASSIGNED"       // 422
	// CodeCodeTaken is an admin-write code, not a buyer-flow code: a
	// create/update whose code already exists. It is emitted only by
	// payments_coupons_admin.go.
	CodeCodeTaken = "PAYMENT_COUPON_CODE_TAKEN" // 409
)

// Error is a rejected coupon or coupon combination. Code is what a
// client branches on; Message names the code so an operator can act.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func reject(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrorCode returns the coupon rejection code carried by err, or "" when
// err is not a coupon rejection.
func ErrorCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
