package settlement

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/service"
	"github.com/tabloy/keygate/internal/store"
)

// ErrOrderAlreadySettled is the answer callers may expect when an order
// was settled before. Settle deliberately does not return it: a
// redelivery of a paid order is a success (nil), because the provider
// must be acked and no second grant may happen. It exists so a caller
// that wants to tell "this delivery granted something" from "it was a
// duplicate" can do so without matching a string.
var ErrOrderAlreadySettled = errors.New("payment order is already settled")

// Service turns verified provider events into licences. It is the only
// writer that grants one.
type Service struct {
	Store    *store.Store
	Webhooks *service.WebhookService
	Log      *slog.Logger
}

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Settle applies one verified provider event to one order. It is safe to
// call twice: the order is read under a row lock, a paid order returns
// without a grant, and the transaction table dedupes on (provider,
// provider_ref). A new order's licence id is derived from the order id,
// so a retry after a crash between the licence insert and the order
// update finds that same row instead of minting a second licence.
//
// An event that does not describe the order — another provider, another
// currency, another amount — is recorded as an unmatched transaction and
// grants nothing. Money arriving against an expired or canceled order is
// recorded the same way, without events: the operator has to see it, and
// the customer's licence must not appear from a lapsed checkout.
func (s *Service) Settle(ctx context.Context, orderID, providerName string, ev provider.WebhookEvent) error {
	order, err := s.Store.GetPaymentOrderForUpdate(ctx, nil, orderID)
	if err != nil {
		return err
	}
	if reason := orderMismatch(order, providerName, ev); reason != "" {
		if _, err := s.recordTransaction(ctx, nil, providerName, order, ev, domain.TxUnmatched); err != nil {
			return err
		}
		s.logger().Warn("settlement: event does not match the order, transaction left unmatched",
			"order", order.Code, "provider", providerName, "reason", reason)
		return nil
	}
	if ev.Status != domain.OrderPaid {
		return s.applyNonPaid(ctx, order, providerName, ev)
	}
	if order.Status == domain.OrderPaid {
		// Duplicate delivery after a successful settlement. Repair the
		// one link a crash can leave behind: a new order whose licence
		// row was written but never stamped with its id. The licence id
		// is the order id by construction, so no lookup table is needed.
		s.repairLicenseLink(ctx, order)
		return nil
	}
	if domain.IsOrderTerminal(order.Status) {
		// Late money against an expired, canceled or refunded order:
		// keep it visible for an operator, grant nothing, emit nothing.
		_, err := s.recordTransaction(ctx, nil, providerName, order, ev, domain.TxUnmatched)
		return err
	}

	switch order.Kind {
	case "new":
		return s.settleNew(ctx, order, providerName, ev)
	case "renewal":
		return s.settleRenewal(ctx, order, providerName, ev)
	default:
		return fmt.Errorf("settlement: order %s has unsupported kind %q", order.Code, order.Kind)
	}
}

// orderMismatch explains why an event cannot be this order's payment, or
// returns "" when it can. An amount of zero means the provider reported
// no amount (a zero-total order); every real transfer carries one.
func orderMismatch(order *model.PaymentOrder, providerName string, ev provider.WebhookEvent) string {
	if providerName != "" && order.Provider != "" && !strings.EqualFold(order.Provider, providerName) {
		return fmt.Sprintf("event provider %q, order was created for %q", providerName, order.Provider)
	}
	if ev.Currency != "" && order.Currency != "" && !strings.EqualFold(ev.Currency, order.Currency) {
		return fmt.Sprintf("event currency %q, order is %q", ev.Currency, order.Currency)
	}
	if ev.AmountMinor != 0 && ev.AmountMinor != order.TotalMinor {
		return fmt.Sprintf("event amount %d, order is %d", ev.AmountMinor, order.TotalMinor)
	}
	return ""
}

// settleNew grants the licence a paid new-order bought, then marks the
// order paid and emits the paid events. The licence write and the order
// write cannot share one transaction — CreateLicenseWithSubscription owns
// its own — so the licence id derived from the order is what makes a
// retry land on the same row.
func (s *Service) settleNew(ctx context.Context, order *model.PaymentOrder, providerName string, ev provider.WebhookEvent) error {
	lic, plan, err := buildNewLicense(ctx, s.Store, order)
	if err != nil {
		return s.failOrder(ctx, order, err)
	}
	if _, err := s.recordTransaction(ctx, nil, providerName, order, ev, domain.TxMatched); err != nil {
		return err
	}
	// The licence id is derived from the order, not random: a retry
	// after a crash between the insert and the order update inserts the
	// same row id and is caught below, instead of minting a second
	// licence for one payment.
	lic.ID = order.ID
	if err := s.Store.CreateLicenseWithSubscription(ctx, lic, plan); err != nil {
		if !store.IsUniqueViolation(err) {
			return s.failOrder(ctx, order, err)
		}
		lic, err = s.Store.FindLicenseByID(ctx, order.ID)
		if err != nil {
			return s.failOrder(ctx, order, err)
		}
	}
	paid, err := s.Store.MarkPaymentOrderPaid(ctx, nil, order.ID, time.Now())
	if err != nil {
		if errors.Is(err, store.ErrOrderNotPayable) {
			// A concurrent settlement won the row and paid it; the
			// licence this order names is the row it wrote.
			return nil
		}
		return err
	}
	if _, err := s.Store.DB.NewUpdate().Model((*model.PaymentOrder)(nil)).
		Set("license_id = ?", lic.ID).Where("id = ?", paid.ID).Exec(ctx); err != nil {
		// The licence exists and the order is paid; a redelivery repairs
		// this link (see the paid branch in Settle).
		s.logger().Error("settlement: could not stamp the order's licence",
			"order", paid.Code, "licence", lic.ID, "error", err)
	}
	s.dispatchPaid(ctx, paid, providerName)
	return nil
}

// repairLicenseLink fills a new order's licence_id when an earlier
// settlement wrote the licence but failed to stamp it. It is a no-op for
// a renewal (which already names its licence) and for an order whose
// licence is already stamped.
func (s *Service) repairLicenseLink(ctx context.Context, order *model.PaymentOrder) {
	if order.Kind != "new" || order.LicenseID != "" {
		return
	}
	lic, err := s.Store.FindLicenseByID(ctx, order.ID)
	if err != nil {
		return
	}
	if _, err := s.Store.DB.NewUpdate().Model((*model.PaymentOrder)(nil)).
		Set("license_id = ?", lic.ID).Where("id = ?", order.ID).Exec(ctx); err != nil {
		s.logger().Warn("settlement: could not repair the order's licence link",
			"order", order.Code, "error", err)
	}
}

// settleRenewal extends the licence a paid renewal order names and marks
// the order paid in one transaction: the ledger row, the extension and
// the status either all land or none do. A concurrent settlement that
// already paid the order makes this one a no-op.
func (s *Service) settleRenewal(ctx context.Context, order *model.PaymentOrder, providerName string, ev provider.WebhookEvent) error {
	plan, err := s.Store.FindPlanByID(ctx, order.PlanID)
	if err != nil {
		return s.failOrder(ctx, order, fmt.Errorf("read plan: %w", err))
	}
	var paid *model.PaymentOrder
	err = s.Store.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if _, err := s.recordTransaction(ctx, tx, providerName, order, ev, domain.TxMatched); err != nil {
			return err
		}
		if err := store.ExtendLicenseFromOrderIn(ctx, tx, order, plan); err != nil {
			return err
		}
		var err error
		paid, err = s.Store.MarkPaymentOrderPaid(ctx, tx, order.ID, time.Now())
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrOrderNotPayable) {
			// A concurrent settlement paid the order and applied the
			// extension; this transaction rolled back cleanly.
			return nil
		}
		return s.failOrder(ctx, order, err)
	}
	s.dispatchPaid(ctx, paid, providerName)
	return nil
}

// applyNonPaid records a provider event that is not a payment and, when
// the provider says the checkout expired, closes the order. No licence is
// touched.
func (s *Service) applyNonPaid(ctx context.Context, order *model.PaymentOrder, providerName string, ev provider.WebhookEvent) error {
	if _, err := s.recordTransaction(ctx, nil, providerName, order, ev, domain.TxUnmatched); err != nil {
		return err
	}
	if ev.Status == domain.OrderExpired && order.Status == domain.OrderPending {
		if _, err := s.Store.DB.NewUpdate().Model((*model.PaymentOrder)(nil)).
			Set("status = ?", domain.OrderExpired).
			Set("updated_at = ?", time.Now()).
			Where("id = ?", order.ID).
			// The write repeats the pending condition so a concurrent
			// payment wins the row rather than being overwritten.
			Where("status = ?", domain.OrderPending).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

// failOrder records why a settlement could not finish, so an operator can
// see it on the order, and emits payment.failed. The cause is returned:
// the webhook answers non-2xx and the provider retries once the cause is
// fixed.
func (s *Service) failOrder(ctx context.Context, order *model.PaymentOrder, cause error) error {
	if _, err := s.Store.DB.NewUpdate().Model((*model.PaymentOrder)(nil)).
		Set("last_settlement_error = ?", cause.Error()).
		Set("updated_at = ?", time.Now()).
		Where("id = ?", order.ID).Exec(ctx); err != nil {
		s.logger().Error("settlement: could not record the failure",
			"order", order.Code, "error", err)
	}
	if s.Webhooks != nil {
		s.Webhooks.Dispatch(ctx, order.ProductID, model.EventPaymentFailed, map[string]any{
			"order": order, "error": cause.Error(),
		})
	}
	return cause
}

// RecordUnmatched stores a transfer no order matched, for the operator
// queue. order may be nil (no order was found); when it is given, the row
// points at it so the operator sees which order the transfer may belong
// to. No event is emitted.
func (s *Service) RecordUnmatched(ctx context.Context, providerName string, ev provider.WebhookEvent, order *model.PaymentOrder) error {
	_, err := s.recordTransaction(ctx, nil, providerName, order, ev, domain.TxUnmatched)
	return err
}

// dispatchPaid emits the two events a successful grant owes. Only called
// after the order is paid: a subscriber must never learn about a payment
// that granted nothing.
func (s *Service) dispatchPaid(ctx context.Context, order *model.PaymentOrder, providerName string) {
	if s.Webhooks == nil {
		return
	}
	s.Webhooks.Dispatch(ctx, order.ProductID, model.EventPaymentSucceeded, map[string]any{
		"order": order, "provider": providerName,
	})
	s.Webhooks.Dispatch(ctx, order.ProductID, model.EventOrderPaid, map[string]any{"order": order})
}

// recordTransaction writes one provider transfer. The provider reference
// is the dedupe key; an event that carries none falls back to its event
// id rather than sharing the empty slot with every other event.
func (s *Service) recordTransaction(ctx context.Context, tx bun.IDB, providerName string,
	order *model.PaymentOrder, ev provider.WebhookEvent, status string) (*model.PaymentTransaction, error) {
	ref := ev.ProviderRef
	if ref == "" {
		ref = ev.ProviderEventID
	}
	in := store.PaymentTransactionInput{
		Provider: providerName, ProviderRef: ref,
		AmountMinor: ev.AmountMinor, Currency: ev.Currency, Status: status,
		Raw: ev.Raw, OccurredAt: ev.OccurredAt,
	}
	if order != nil {
		in.OrderID = &order.ID
	}
	return s.Store.InsertPaymentTransaction(ctx, tx, in)
}
