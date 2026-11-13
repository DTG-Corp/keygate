package reconcile

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/providercfg"
	"github.com/tabloy/keygate/internal/payment/providers/payos"
	"github.com/tabloy/keygate/internal/payment/providers/sepay"
	"github.com/tabloy/keygate/internal/payment/settlement"
	"github.com/tabloy/keygate/internal/store"
)

// DefaultInterval is how often a full pass runs. Spec §3.7 asks for five
// minutes: a bank webhook that was dropped must not stay invisible for
// an operator any longer than that.
const DefaultInterval = 5 * time.Minute

const (
	// sepayCursorScope is the (provider, scope) key the bank cursor is
	// stored under. Spec §4.10 expects the bank account id here; phase 1a
	// reads the install's one account and uses the default scope.
	sepayCursorScope = store.DefaultSyncScope
	// sepayBackfill is how far back the first pass reads when no cursor
	// exists yet: a day of bank history is enough to cover orders left
	// pending by the release that introduced the worker.
	sepayBackfill = 24 * time.Hour
	// sepayOverlap re-reads a little history before the cursor so a
	// transaction that was committed late on SePay's side is still seen.
	// The (provider, provider_ref) unique index makes the overlap free.
	sepayOverlap = 5 * time.Minute
	// payosPendingBatch bounds one PayOS pass. Rows are ordered oldest
	// first, so the next pass continues where this one stopped.
	payosPendingBatch = 100
)

// Worker is the reconciliation pass. Tick does three jobs in one pass:
// close lapsed orders, ask PayOS about pending orders whose webhook never
// arrived, and pull SePay bank transactions since the cursor. It never
// grants anything except through Settlement, so a transfer it cannot
// place is recorded for an operator instead of guessed at.
//
// Run calls Tick on an interval; a caller that wants to drive the pass
// itself (tests, a one-off admin trigger) calls Tick directly.
type Worker struct {
	Store      *store.Store
	Config     *providercfg.Service
	Matcher    *Matcher
	Settlement *settlement.Service
	Log        *slog.Logger
	Interval   time.Duration

	// QueryPaymentLink and ListTransactions are the two provider calls a
	// pass makes. They are fields so a test can drive Tick without a
	// network; nil means the real client, built from the resolved config
	// row (which is what keeps this package free of stored secrets).
	QueryPaymentLink func(ctx context.Context, cfg *providercfg.ResolvedConfig, numericCode int64) (*payos.PaymentLink, error)
	ListTransactions func(ctx context.Context, cfg *providercfg.ResolvedConfig, from, to time.Time, sinceID int64) ([]sepay.BankTransaction, error)
}

func (w *Worker) logger() *slog.Logger {
	if w != nil && w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

// Run ticks until ctx is cancelled. It ticks once immediately so a
// restart catches up on the orders that lapsed while the process was
// down instead of waiting a whole interval.
func (w *Worker) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	tick := func() {
		if err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			w.logger().Error("reconcile: pass failed", "error", err)
		}
	}
	tick()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

// Tick runs one pass. Expiry always runs; a provider pass whose config is
// missing is skipped rather than guessed at, and a failing provider never
// stops the other one: a PayOS outage must not stop bank reconciliation.
// The per-provider failures are logged here and do not surface as an
// error, so Run's own log does not report the same failure twice.
func (w *Worker) Tick(ctx context.Context) error {
	if w == nil || w.Store == nil {
		return errors.New("reconcile: worker has no store")
	}
	if _, err := w.Store.ExpirePaymentOrders(ctx, time.Now()); err != nil {
		return err
	}
	if w.Config == nil || w.Settlement == nil {
		// A developer install with no encryption key has no provider
		// config rows; there is nothing to query. Expiry above is still
		// the worker's job.
		return nil
	}
	if err := w.reconcilePayOS(ctx); err != nil {
		w.logger().Warn("reconcile: payos pass failed", "error", err)
	}
	if w.Matcher == nil {
		w.logger().Warn("reconcile: no matcher, skipping the bank transaction pass")
		return nil
	}
	if err := w.reconcileSePay(ctx); err != nil {
		w.logger().Warn("reconcile: sepay pass failed", "error", err)
	}
	return nil
}

// reconcilePayOS asks PayOS about pending orders the webhook never
// settled. A paid link is applied through Settlement with the same
// event shape a webhook carries, so the grant is idempotent and the
// transaction row lands on the order. Errors are collected: one broken
// order (or one transient call) must not hide the rest, and the caller
// logs them as one provider failure.
func (w *Worker) reconcilePayOS(ctx context.Context) error {
	orders, err := w.Store.ListPendingPaymentOrders(ctx, "payos", payosPendingBatch)
	if err != nil {
		return err
	}
	query := w.QueryPaymentLink
	if query == nil {
		query = w.queryPayOSPaymentLink
	}
	var errs []error
	for _, order := range orders {
		cfg, err := w.Config.Resolve(ctx, order.ProductID, "payos", "")
		if err != nil {
			// This product has no usable PayOS row (or it is disabled):
			// the order is not this pass's to settle.
			continue
		}
		link, err := query(ctx, cfg, order.NumericCode)
		if err != nil {
			errs = append(errs, fmt.Errorf("order %s: %w", order.Code, err))
			continue
		}
		if link == nil || !strings.EqualFold(strings.TrimSpace(link.Status), "paid") {
			continue
		}
		raw, _ := json.Marshal(map[string]any{
			"source": "reconcile", "payment_link_id": link.PaymentLinkID,
			"status": link.Status, "amount": link.Amount, "checkout_url": link.CheckoutURL,
		})
		currency := order.Currency
		if currency == "" {
			currency = "VND"
		}
		ev := provider.WebhookEvent{
			Provider: "payos", ProviderEventID: "query-" + order.Code, ProviderRef: link.PaymentLinkID,
			OrderCode: order.Code, NumericCode: order.NumericCode, AmountMinor: link.Amount,
			Currency: currency, Status: domain.OrderPaid, OccurredAt: time.Now(), Raw: raw,
		}
		if err := w.Settlement.Settle(ctx, order.ID, "payos", ev); err != nil {
			errs = append(errs, fmt.Errorf("settle order %s: %w", order.Code, err))
		}
	}
	return errors.Join(errs...)
}

// reconcileSePay pulls the bank account's transfers since the cursor.
// The cursor advances only after a transfer is processed, so a failed
// settlement is retried on the next pass instead of being skipped; the
// overlap window above and the provider_ref dedupe make the retry
// harmless.
func (w *Worker) reconcileSePay(ctx context.Context) error {
	cfg, err := w.Config.Resolve(ctx, "", "sepay", "")
	if err != nil {
		// No install-wide row (or it is disabled). A static-QR install
		// with no API token is a supported configuration: there is
		// nothing to pull from the bank API.
		return nil
	}
	if strings.TrimSpace(cfg.APIToken) == "" {
		return nil
	}

	cursor, err := w.Store.GetPaymentSyncCursor(ctx, "sepay", sepayCursorScope)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	from := time.Now().Add(-sepayBackfill)
	var sinceID int64
	if cursor != nil {
		if cursor.LastSyncedAt != nil {
			from = cursor.LastSyncedAt.Add(-sepayOverlap)
		}
		if cursor.LastTransactionID != "" {
			id, err := strconv.ParseInt(cursor.LastTransactionID, 10, 64)
			if err != nil {
				return fmt.Errorf("reconcile: sepay cursor holds %q, not a transaction id", cursor.LastTransactionID)
			}
			sinceID = id
		}
	}

	list := w.ListTransactions
	if list == nil {
		list = w.listSePayTransactions
	}
	transactions, err := list(ctx, cfg, from, time.Now(), sinceID)
	if err != nil {
		return err
	}
	for _, tx := range transactions {
		if tx.ID <= sinceID {
			// The provider includes the cursor row (or repeated it): it
			// was already ingested, and reprocessing is not free even
			// when the dedupe would swallow the write.
			continue
		}
		if err := w.ingestSePayTransaction(ctx, tx); err != nil {
			return err
		}
		if err := w.Store.SetPaymentSyncCursor(ctx, "sepay", sepayCursorScope,
			strconv.FormatInt(tx.ID, 10), tx.TransactionDate); err != nil {
			return err
		}
		sinceID = tx.ID
	}
	return nil
}

// ingestSePayTransaction records one bank transfer. Money in is matched
// to an order and settled through Settlement; money out and money that
// matches nothing is written to the unmatched queue with the memo the
// operator needs to decide what it was.
func (w *Worker) ingestSePayTransaction(ctx context.Context, tx sepay.BankTransaction) error {
	occurred := tx.TransactionDate
	if occurred.IsZero() {
		occurred = time.Now()
	}
	direction := strings.ToLower(strings.TrimSpace(tx.TransferType))
	if direction != "in" && direction != "out" {
		direction = "in"
	}
	raw, _ := json.Marshal(map[string]any{
		"source": "reconcile", "id": tx.ID, "transaction_date": occurred,
		"account_number": tx.AccountNumber, "code": tx.Code, "content": tx.Content,
		"transfer_type": tx.TransferType, "transfer_amount": tx.TransferAmount,
		"reference_code": tx.ReferenceCode,
	})
	ev := provider.WebhookEvent{
		Provider: "sepay", ProviderEventID: "sepay-api-" + strconv.FormatInt(tx.ID, 10),
		ProviderRef: strconv.FormatInt(tx.ID, 10), AmountMinor: tx.TransferAmount,
		Currency: "VND", OccurredAt: occurred, TransferType: direction, Raw: raw,
	}

	reason := ""
	if direction == "out" {
		// A refund or a payout leaves the account. It is never money for
		// an order: the operator queue is where it belongs.
		reason = "bank transfer out of the account (refund or payout); no order is settled"
	} else {
		ev.OrderCode = sepayOrderCode(tx)
		order, ambiguous, err := w.Matcher.Match(ctx, "sepay", ev)
		if err != nil {
			return err
		}
		if order != nil {
			ev.Status = domain.OrderPaid
			return w.Settlement.Settle(ctx, order.ID, "sepay", ev)
		}
		if ambiguous {
			reason = "ambiguous: more than one pending order has this amount in the matching window"
		} else {
			reason = "no pending order matched this transfer"
		}
	}

	_, _, err := w.Store.InsertPaymentTransaction(ctx, nil, store.PaymentTransactionInput{
		Provider: "sepay", ProviderRef: ev.ProviderRef,
		AmountMinor: tx.TransferAmount, Currency: "VND", Direction: direction,
		ProviderAccount: tx.AccountNumber, Reference: tx.ReferenceCode, Content: tx.Content,
		Status: domain.TxUnmatched, IgnoreReason: reason, Raw: raw, OccurredAt: occurred,
	})
	return err
}

// sepayOrderCode is the order code a bank transfer names, if any. SePay
// parses a payment code from the memo into Code, so that field wins when
// it is code-shaped; otherwise the memo itself is searched, which also
// covers a customer who typed the code by hand.
func sepayOrderCode(tx sepay.BankTransaction) string {
	if code := strings.ToUpper(strings.TrimSpace(tx.Code)); domain.IsOrderCode(code) {
		return code
	}
	return extractOrderCodeFromText(tx.Content)
}

// extractOrderCodeFromText finds the first KG + 6 Crockford characters
// that stands as its own token, so "KG7F3K2Q ca phe" matches but
// "XKG7F3K2Q" does not. A code embedded in a longer run of letters or
// digits is some other identifier that happens to look like ours.
func extractOrderCodeFromText(s string) string {
	for i := 0; i+8 <= len(s); i++ {
		if i > 0 && isCodeTokenChar(s[i-1]) {
			continue
		}
		if code := s[i : i+8]; domain.IsOrderCode(code) {
			if i+8 == len(s) || !isCodeTokenChar(s[i+8]) {
				return code
			}
		}
	}
	return ""
}

func isCodeTokenChar(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// queryPayOSPaymentLink is the real PayOS query, built from the resolved
// row so the credentials never live anywhere else.
func (w *Worker) queryPayOSPaymentLink(ctx context.Context, cfg *providercfg.ResolvedConfig, numericCode int64) (*payos.PaymentLink, error) {
	client := &payos.Client{ClientID: cfg.ClientID, APIKey: cfg.APIKey, ChecksumKey: cfg.ChecksumKey}
	return client.GetPaymentLink(ctx, numericCode)
}

// listSePayTransactions is the real bank query, pointed at the sandbox
// host when the row was written in test mode.
func (w *Worker) listSePayTransactions(ctx context.Context, cfg *providercfg.ResolvedConfig, from, to time.Time, sinceID int64) ([]sepay.BankTransaction, error) {
	base := "https://userapi.sepay.vn/v2"
	if cfg.Mode == "test" {
		base = "https://userapi-sandbox.sepay.vn/v2"
	}
	client := &sepay.Client{BaseURL: base, APIToken: cfg.APIToken}
	return client.ListTransactions(ctx, from, to, sinceID)
}
