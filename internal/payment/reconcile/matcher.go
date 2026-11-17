// Package reconcile is the payment rail's safety net: a background pass
// that closes lapsed orders and finds money the webhooks missed. It
// never grants anything settlement would not grant; when it cannot say
// which order a transfer paid, the transfer is recorded for an operator
// instead of guessed at.
package reconcile

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/store"
)

// matchWindow is how far on either side of a transfer an order may have
// been created for the amount to be a suggestion. Orders are short-lived
// and a bank transfer lands within minutes of checkout; a day of slack
// would turn every equal-priced order into a candidate.
const matchWindow = 15 * time.Minute

// Matcher answers one question: which order, if any, did this transfer
// pay? It is deterministic and read-only. A positive answer is only ever
// an order that is still payable and belongs to the same rail; the
// amount + window rule is a suggestion for the operator queue and is
// never applied to money by itself (the worker records it as unmatched).
type Matcher struct {
	Store *store.Store
}

// MatchKind is the evidence behind a Match answer. Only MatchCode is
// authoritative; MatchSuggestion is the amount + window rule and must
// be confirmed by an operator before any money is settled.
type MatchKind int

const (
	// MatchNone means no payable order fits the transfer.
	MatchNone MatchKind = iota
	// MatchCode means the transfer named the order's code. It is safe
	// for the reconciler to settle.
	MatchCode
	// MatchSuggestion means exactly one pending order has the amount
	// inside the window but the transfer carried no usable code. The
	// order is returned so the operator queue can show the candidate;
	// nobody may settle from it without confirmation.
	MatchSuggestion
	// MatchAmbiguous means several pending orders fit the amount and
	// window. No order is returned.
	MatchAmbiguous
)

func (k MatchKind) String() string {
	switch k {
	case MatchCode:
		return "code"
	case MatchSuggestion:
		return "suggestion"
	case MatchAmbiguous:
		return "ambiguous"
	default:
		return "none"
	}
}

// Match resolves a verified or polled transfer against the orders table.
//
//   - An event that names an order code answers that order with
//     MatchCode, and only when it is still pending and belongs to the
//     same provider. A code that names a paid, expired or canceled order
//     is a late duplicate: no match, and not an ambiguity — the caller
//     records it as unmatched.
//   - An event with no code falls back to amount and a ±15 minute window.
//     One candidate is MatchSuggestion: the order is returned so the
//     operator knows what to confirm, but the worker must not settle it.
//     Two or more is MatchAmbiguous (nil order), which the caller must
//     record rather than pick one.
//
// An error is a read fault, never "not found": a database problem must
// not turn into an unmatched row that hides a transfer.
func (m *Matcher) Match(ctx context.Context, providerName string, ev provider.WebhookEvent) (*model.PaymentOrder, MatchKind, error) {
	if m == nil || m.Store == nil {
		return nil, MatchNone, errors.New("reconcile: matcher has no store")
	}
	if code := strings.ToUpper(strings.TrimSpace(ev.OrderCode)); domain.IsOrderCode(code) {
		order, err := m.Store.GetPaymentOrderByCode(ctx, code)
		switch {
		case err == nil:
			if order.Status == domain.OrderPending && order.Provider == providerName {
				return order, MatchCode, nil
			}
			return nil, MatchNone, nil
		case errors.Is(err, store.ErrOrderNotFound):
			// The memo stated an order code, and no order carries it.
			// Falling back to the amount here would attach the money to
			// an order the payer did not name.
			return nil, MatchNone, nil
		default:
			return nil, MatchNone, err
		}
	}
	if ev.AmountMinor <= 0 || ev.OccurredAt.IsZero() {
		// A zero amount cannot pay anything, and without an occurrence
		// time there is no window to widen. Both are the unmatched
		// queue's problem, not a guess.
		return nil, MatchNone, nil
	}
	orders, err := m.Store.FindPendingPaymentOrdersForMatch(ctx, providerName, ev.AmountMinor,
		ev.OccurredAt.Add(-matchWindow), ev.OccurredAt.Add(matchWindow))
	if err != nil {
		return nil, MatchNone, err
	}
	switch len(orders) {
	case 0:
		return nil, MatchNone, nil
	case 1:
		return orders[0], MatchSuggestion, nil
	default:
		return nil, MatchAmbiguous, nil
	}
}
