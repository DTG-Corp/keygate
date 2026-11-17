package settlement

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/dtg-corp/dsoft/internal/license"
	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/payment/provider"
	"github.com/dtg-corp/dsoft/internal/service"
	"github.com/dtg-corp/dsoft/internal/store"
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
// call twice: the order is re-read under a row lock inside the grant
// transaction, a paid order returns without a grant, and the transaction
// table dedupes on (provider, provider_ref).
//
// A new order is granted in one transaction — licence, subscription,
// paid stamp and matched transaction — so a crash can leave neither half
// of the pair. A retry that finds a licence already committed for the
// order reuses it instead of minting a second one.
//
// An event that does not describe the order — another provider, another
// currency, or an amount that is not exactly the order's — is recorded as
// an unmatched transaction and grants nothing. Money arriving against an
// expired or canceled order is recorded the same way, without events: the
// operator has to see it, and the customer's licence must not appear from
// a lapsed checkout.
func (s *Service) Settle(ctx context.Context, orderID, providerName string, ev provider.WebhookEvent) error {
	order, err := s.Store.GetPaymentOrderForUpdate(ctx, nil, orderID)
	if err != nil {
		return err
	}
	if reason := orderMismatch(order, providerName, ev); reason != "" {
		if _, _, err := s.recordTransaction(ctx, nil, providerName, order, ev, domain.TxUnmatched, reason); err != nil {
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
		return s.recordLatePayment(ctx, order, providerName, ev)
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
// returns "" when it can. The amount is exact for a paid event: zero is a
// mismatch, not "the provider did not say", because a real transfer
// always carries one and accepting zero would grant a priced order for
// nothing. Currency is compared when both sides name one, and the
// provider check covers every status (a failure notice from another rail
// is not this order's failure).
func orderMismatch(order *model.PaymentOrder, providerName string, ev provider.WebhookEvent) string {
	if providerName != "" && order.Provider != "" && !strings.EqualFold(order.Provider, providerName) {
		return fmt.Sprintf("event provider %q, order was created for %q", providerName, order.Provider)
	}
	if ev.Status != domain.OrderPaid {
		return ""
	}
	if ev.AmountMinor != order.TotalMinor {
		return fmt.Sprintf("event amount %d, order is %d", ev.AmountMinor, order.TotalMinor)
	}
	if ev.Currency != "" && order.Currency != "" && !strings.EqualFold(ev.Currency, order.Currency) {
		return fmt.Sprintf("event currency %q, order is %q", ev.Currency, order.Currency)
	}
	return ""
}

// settleNew grants the licence a paid new-order bought, marks the order
// paid, records the transfer and stamps the licence link in one
// transaction, then emits the paid events. A grant failure rolls back all
// of it, records why for an operator, and returns the error so the
// provider retries — it is not a payment failure and emits no event.
func (s *Service) settleNew(ctx context.Context, order *model.PaymentOrder, providerName string, ev provider.WebhookEvent) error {
	lic, plan, err := buildNewLicense(ctx, s.Store, order)
	if err != nil {
		return s.failGrant(ctx, order, providerName, ev, err)
	}
	var paid, alreadySettled *model.PaymentOrder
	var granted *model.License
	err = s.Store.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		// The row lock is the grant's serialisation point: the paid
		// check, the licence and the order write are one decision.
		locked, err := s.Store.GetPaymentOrderForUpdate(ctx, tx, order.ID)
		if err != nil {
			return err
		}
		if locked.Status == domain.OrderPaid {
			alreadySettled = locked
			return nil
		}
		if domain.IsOrderTerminal(locked.Status) {
			return store.ErrOrderNotPayable
		}
		// A crash between the committed licence insert and the order
		// update of the old two-transaction flow left this state. The
		// licence id is derived from the order, so it is found here and
		// reused rather than failing the insert on the primary key.
		existing, err := s.Store.FindLicenseByIDIn(ctx, tx, order.ID)
		switch {
		case err == nil:
			granted = existing
		case errors.Is(err, sql.ErrNoRows):
			created, _, err := s.Store.CreatePaidOrderLicence(ctx, tx, store.PaidOrderLicenceInput{
				Order: locked, Plan: plan, License: lic,
			})
			if err != nil {
				return err
			}
			granted = created
		default:
			return err
		}
		paid, err = s.Store.MarkPaymentOrderPaid(ctx, tx, order.ID, time.Now())
		if err != nil {
			return err
		}
		if _, err := tx.NewUpdate().Model((*model.PaymentOrder)(nil)).
			Set("license_id = ?", granted.ID).Where("id = ?", paid.ID).Exec(ctx); err != nil {
			return err
		}
		paid.LicenseID = granted.ID
		return s.matchTransaction(ctx, tx, providerName, paid, ev)
	})
	if err != nil {
		if errors.Is(err, store.ErrOrderNotPayable) {
			return s.settleNotPayable(ctx, order, providerName, ev, err)
		}
		return s.failGrant(ctx, order, providerName, ev, err)
	}
	if alreadySettled != nil {
		// A concurrent settlement applied this order first. It emitted
		// the events; this delivery is only an ack.
		return nil
	}
	s.dispatchPaid(ctx, paid, granted, providerName, ev)
	return nil
}

// settleRenewal extends the licence a paid renewal order bought, marks
// the order paid and records the matched transfer in one transaction, so
// the money and the extension either all land or none do. A licence that
// is already active or past due is eligible; past due becomes active in
// the same write.
func (s *Service) settleRenewal(ctx context.Context, order *model.PaymentOrder, providerName string, ev provider.WebhookEvent) error {
	plan, err := s.Store.FindPlanByID(ctx, order.PlanID)
	if err != nil {
		return s.failGrant(ctx, order, providerName, ev, fmt.Errorf("read plan: %w", err))
	}
	var paid, alreadySettled *model.PaymentOrder
	var granted *model.License
	err = s.Store.RunInTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		locked, err := s.Store.GetPaymentOrderForUpdate(ctx, tx, order.ID)
		if err != nil {
			return err
		}
		if locked.Status == domain.OrderPaid {
			alreadySettled = locked
			return nil
		}
		if domain.IsOrderTerminal(locked.Status) {
			return store.ErrOrderNotPayable
		}
		if err := store.ExtendLicenseFromOrderIn(ctx, tx, locked, plan); err != nil {
			return err
		}
		granted, err = s.Store.FindLicenseByIDIn(ctx, tx, locked.LicenseID)
		if err != nil {
			return err
		}
		paid, err = s.Store.MarkPaymentOrderPaid(ctx, tx, order.ID, time.Now())
		if err != nil {
			return err
		}
		return s.matchTransaction(ctx, tx, providerName, paid, ev)
	})
	if err != nil {
		if errors.Is(err, store.ErrOrderNotPayable) {
			return s.settleNotPayable(ctx, order, providerName, ev, err)
		}
		return s.failGrant(ctx, order, providerName, ev, err)
	}
	if alreadySettled != nil {
		return nil
	}
	s.dispatchPaid(ctx, paid, granted, providerName, ev)
	return nil
}

// applyNonPaid records a provider event that is not a payment and, when
// the provider says the checkout expired, closes the order. No licence is
// touched. A failure status surfaces payment.failed with the spec's
// failure_reason; a pending/expired/canceled notice does not.
func (s *Service) applyNonPaid(ctx context.Context, order *model.PaymentOrder, providerName string, ev provider.WebhookEvent) error {
	if _, _, err := s.recordTransaction(ctx, nil, providerName, order, ev, domain.TxUnmatched,
		fmt.Sprintf("provider reported status %q, not a payment", ev.Status)); err != nil {
		return err
	}
	if reason, failed := failureStatus(ev.Status); failed {
		if s.Webhooks != nil {
			s.Webhooks.Dispatch(ctx, order.ProductID, model.EventPaymentFailed, map[string]any{
				"order": order,
				"payment": map[string]any{
					"failure_reason": reason,
				},
			})
		}
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

// failureStatus maps a provider status to the reason payment.failed
// carries. Only the statuses the rails use for a failed payment are
// failures; pending, expired and canceled are not.
func failureStatus(status string) (string, bool) {
	switch status {
	case "failed":
		return "the provider reported the payment as failed", true
	case "partial":
		return "the provider reported the payment as partial", true
	case "voided":
		return "the provider reported the payment as voided", true
	}
	return "", false
}

// settleNotPayable handles the race where an order became terminal
// between the grant's pre-check and its row lock. A paid order is an ack;
// any other terminal order keeps the money visible without granting.
func (s *Service) settleNotPayable(ctx context.Context, order *model.PaymentOrder, providerName string, ev provider.WebhookEvent, cause error) error {
	current, err := s.Store.GetPaymentOrderForUpdate(ctx, nil, order.ID)
	if err != nil {
		return err
	}
	if current.Status == domain.OrderPaid {
		return nil
	}
	if domain.IsOrderTerminal(current.Status) {
		if err := s.recordLatePayment(ctx, current, providerName, ev); err != nil {
			return err
		}
		return nil
	}
	// The order is still pending: this was not the terminal we thought
	// and the caller should retry.
	return cause
}

// recordLatePayment records money that arrived after its order closed.
// It is visible to an operator on the unmatched queue, grants nothing
// and emits nothing; returning nil acks the provider so it stops
// retrying a payment that will never be applied.
func (s *Service) recordLatePayment(ctx context.Context, order *model.PaymentOrder, providerName string, ev provider.WebhookEvent) error {
	if _, _, err := s.recordTransaction(ctx, nil, providerName, order, ev, domain.TxUnmatched,
		fmt.Sprintf("order is %s; the payment arrived after it closed", order.Status)); err != nil {
		return err
	}
	s.logger().Warn("settlement: paid event for a terminal order, transaction left unmatched",
		"order", order.Code, "status", order.Status, "provider", providerName)
	return nil
}

// failGrant records why a confirmed payment could not be applied, so an
// operator can see it on the order, and leaves the transfer on the
// unmatched queue with the reason. No payment.failed is emitted: the
// provider did take the money. The cause is returned so the webhook
// answers non-2xx and the provider retries once the cause is fixed.
func (s *Service) failGrant(ctx context.Context, order *model.PaymentOrder, providerName string, ev provider.WebhookEvent, cause error) error {
	res, err := s.Store.DB.NewUpdate().Model((*model.PaymentOrder)(nil)).
		Set("last_settlement_error = ?", cause.Error()).
		Set("updated_at = ?", time.Now()).
		Where("id = ?", order.ID).
		Where("status = ?", domain.OrderPending).Exec(ctx)
	if err != nil {
		s.logger().Error("settlement: could not record the failure",
			"order", order.Code, "error", err)
	} else if n, _ := res.RowsAffected(); n == 0 {
		// A concurrent settlement changed the order while this one was
		// failing; its outcome is the one that counts.
		s.logger().Warn("settlement: grant failed after the order changed state",
			"order", order.Code, "error", cause)
		return cause
	}
	if _, _, err := s.recordTransaction(ctx, nil, providerName, order, ev, domain.TxUnmatched, cause.Error()); err != nil {
		s.logger().Error("settlement: could not record the failed transfer",
			"order", order.Code, "error", err)
	}
	s.logger().Error("settlement: grant failed, the provider will retry",
		"order", order.Code, "error", cause)
	return cause
}

// RecordUnmatched stores a transfer no order matched, for the operator
// queue. order may be nil (no order was found); when it is given, the row
// points at it so the operator sees which order the transfer may belong
// to. No event is emitted.
func (s *Service) RecordUnmatched(ctx context.Context, providerName string, ev provider.WebhookEvent, order *model.PaymentOrder) error {
	_, _, err := s.recordTransaction(ctx, nil, providerName, order, ev, domain.TxUnmatched, "")
	return err
}

// dispatchPaid emits the two events a successful grant owes, with the
// payloads spec §8 describes. Only called after the order is paid and the
// licence exists: a subscriber must never learn about a payment that
// granted nothing.
func (s *Service) dispatchPaid(ctx context.Context, order *model.PaymentOrder, lic *model.License, providerName string, ev provider.WebhookEvent) {
	if s.Webhooks == nil || order == nil {
		return
	}
	provider := providerName
	if provider == "" {
		provider = order.Provider
	}
	ref := transactionRef(ev)
	var validUntil any
	licenseID := order.LicenseID
	keyPrefix := ""
	planID := order.PlanID
	if lic != nil {
		licenseID = lic.ID
		planID = lic.PlanID
		keyPrefix = license.KeyPrefix(lic.LicenseKey)
		validUntil = lic.ValidUntil
	}
	paidAt := order.PaidAt
	occurred := ev.OccurredAt
	if occurred.IsZero() {
		occurred = time.Now()
	}
	currency := ev.Currency
	if currency == "" {
		currency = order.Currency
	}

	s.Webhooks.Dispatch(ctx, order.ProductID, model.EventPaymentSucceeded, map[string]any{
		"order":    order,
		"provider": provider,
		"payment": map[string]any{
			"provider": provider, "provider_ref": ref,
			// Spec §8 names the money "amount"; it is integer minor
			// units, documented in T13.
			"amount": ev.AmountMinor, "currency": currency,
			"occurred_at": occurred, "reference": ref,
		},
	})
	s.Webhooks.Dispatch(ctx, order.ProductID, model.EventOrderPaid, map[string]any{
		"order": order,
		"license": map[string]any{
			"id": licenseID, "key_prefix": keyPrefix,
			"plan_id": planID, "valid_until": validUntil,
		},
		"payment": map[string]any{
			"provider": provider, "provider_ref": ref, "paid_at": paidAt,
		},
	})
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

// matchTransaction records one provider transfer in the grant's
// transaction and marks it matched to the order. On a retry the row
// already exists (the first attempt recorded it unmatched after a failed
// grant): InsertPaymentTransaction returns the stored row and it is
// attached here, so the operator queue empties exactly when the grant
// lands.
func (s *Service) matchTransaction(ctx context.Context, tx bun.IDB, providerName string,
	order *model.PaymentOrder, ev provider.WebhookEvent) error {
	row, inserted, err := s.recordTransaction(ctx, tx, providerName, order, ev, domain.TxMatched, "")
	if err != nil {
		return err
	}
	if !inserted {
		if err := s.Store.MarkTransactionMatchedIn(ctx, tx, row.ID, order.ID); err != nil {
			return fmt.Errorf("match the recorded transfer: %w", err)
		}
	}
	return nil
}

// recordTransaction writes one provider transfer. The provider reference
// is the dedupe key; an event that carries none falls back to its event
// id rather than sharing the empty slot with every other event.
func (s *Service) recordTransaction(ctx context.Context, tx bun.IDB, providerName string,
	order *model.PaymentOrder, ev provider.WebhookEvent, status, reason string) (*model.PaymentTransaction, bool, error) {
	in := store.PaymentTransactionInput{
		Provider: providerName, ProviderRef: transactionRef(ev),
		AmountMinor: ev.AmountMinor, Currency: ev.Currency, Status: status,
		Raw: ev.Raw, OccurredAt: ev.OccurredAt,
	}
	if ev.TransferType == "in" || ev.TransferType == "out" {
		in.Direction = ev.TransferType
	}
	if status == domain.TxUnmatched {
		in.IgnoreReason = reason
	}
	if order != nil {
		in.OrderID = &order.ID
	}
	return s.Store.InsertPaymentTransaction(ctx, tx, in)
}

// transactionRef is the provider reference an event is deduped by, with
// the event id as the fallback for a rail that sends no separate ref.
func transactionRef(ev provider.WebhookEvent) string {
	if ev.ProviderRef != "" {
		return ev.ProviderRef
	}
	return ev.ProviderEventID
}
