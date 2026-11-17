package reconcile_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/providercfg"
	"github.com/tabloy/keygate/internal/payment/providers/payos"
	"github.com/tabloy/keygate/internal/payment/providers/sepay"
	"github.com/tabloy/keygate/internal/payment/reconcile"
	"github.com/tabloy/keygate/internal/payment/settlement"
)

// workerFixture is a matcher fixture plus the settlement service and the
// provider config rows a worker pass resolves. The provider calls are
// seams on the Worker, so a Tick test never leaves the process.
type workerFixture struct {
	*matchFixture
	cfg    *providercfg.Service
	settle *settlement.Service
	log    *slog.Logger
}

func newWorkerFixture(t *testing.T) *workerFixture {
	t.Helper()
	f := newMatchFixture(t)
	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x4d}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return &workerFixture{
		matchFixture: f,
		cfg:          providercfg.NewService(f.s, codec, "test"),
		settle:       &settlement.Service{Store: f.s},
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// sepayConfig writes the install-wide SePay row the worker resolves and
// returns the API token it holds, so a fake list knows which config it
// was called with. The row is removed afterwards: an install-wide row is
// a shared slot every test package sees.
func (f *workerFixture) sepayConfig() string {
	f.t.Helper()
	token := "worker-token-" + uuid.NewString()[:13]
	// The cursor is one install-wide row that outlives a test run. A
	// leftover high-water mark would make a fixed transaction id look
	// already ingested, so every test starts from none.
	f.clearSePayCursor()
	f.t.Cleanup(f.clearSePayCursor)
	row, err := f.cfg.Upsert(f.ctx, providercfg.UpsertInput{
		Provider: "sepay", Mode: "test", Enabled: true,
		Config:        providercfg.SepayConfig{BankName: "MB Bank", BankCode: "MB", AccountNo: "0978482700", AccountName: "NGUYEN VAN A"},
		Credentials:   providercfg.SepayCredentials{APIToken: token},
		WebhookSecret: "worker-secret-" + uuid.NewString()[:13],
	})
	if err != nil {
		f.t.Fatalf("upsert sepay config: %v", err)
	}
	f.t.Cleanup(func() {
		_, _ = f.s.DB.NewRaw("DELETE FROM payment_provider_configs WHERE id = ?", row.ID).Exec(context.Background())
	})
	return token
}

// deleteTransactions removes synthetic bank rows a previous run of the
// same test may have left behind. The (provider, provider_ref) index
// would otherwise swallow the insert and the assertions would read the
// old row.
func (f *workerFixture) deleteTransactions(refs ...string) {
	f.t.Helper()
	for _, ref := range refs {
		if _, err := f.s.DB.NewRaw("DELETE FROM payment_transactions WHERE provider = ? AND provider_ref = ?", "sepay", ref).Exec(context.Background()); err != nil {
			f.t.Fatalf("delete transaction %s: %v", ref, err)
		}
	}
}

func (f *workerFixture) clearSePayCursor() {
	f.t.Helper()
	if _, err := f.s.DB.NewRaw("DELETE FROM payment_sync_cursors WHERE provider = ? AND scope = ?", "sepay", "default").Exec(context.Background()); err != nil {
		f.t.Fatalf("clear sepay cursor: %v", err)
	}
}

// payosConfig writes the product-scoped PayOS row the worker resolves.
func (f *workerFixture) payosConfig() {
	f.t.Helper()
	row, err := f.cfg.Upsert(f.ctx, providercfg.UpsertInput{
		Provider: "payos", Mode: "test", Enabled: true, ProductID: f.prod.ID,
		Config:        providercfg.PayOSConfig{ClientID: "client-" + uuid.NewString()[:8]},
		Credentials:   providercfg.PayOSCredentials{APIKey: "api-key", ChecksumKey: "checksum-key"},
		WebhookSecret: "payos-secret-" + uuid.NewString()[:13],
	})
	if err != nil {
		f.t.Fatalf("upsert payos config: %v", err)
	}
	f.t.Cleanup(func() {
		_, _ = f.s.DB.NewRaw("DELETE FROM payment_provider_configs WHERE id = ?", row.ID).Exec(context.Background())
	})
}

func (f *workerFixture) worker() *reconcile.Worker {
	return &reconcile.Worker{
		Store: f.s, Config: f.cfg, Matcher: &reconcile.Matcher{Store: f.s},
		Settlement: f.settle, Log: f.log,
	}
}

// lapsedOrder places a pending order and then moves its expiry into the
// past, which CreatePaymentOrder refuses to do on purpose.
func (f *workerFixture) lapsedOrder(email, providerName string, amount int64) *model.PaymentOrder {
	f.t.Helper()
	o := f.order(email, amount, providerName)
	if _, err := f.s.DB.NewRaw("UPDATE payment_orders SET expires_at = ? WHERE id = ?",
		time.Now().Add(-time.Minute), o.ID).Exec(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	return o
}

func (f *workerFixture) reload(code string) *model.PaymentOrder {
	f.t.Helper()
	o, err := f.s.GetPaymentOrderByCode(f.ctx, code)
	if err != nil {
		f.t.Fatalf("reload order: %v", err)
	}
	return o
}

func (f *workerFixture) countLicences() int {
	f.t.Helper()
	n, err := f.s.DB.NewSelect().Model((*model.License)(nil)).Where("product_id = ?", f.prod.ID).Count(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *workerFixture) countTransactions(ref string) int {
	f.t.Helper()
	n, err := f.s.DB.NewSelect().Model((*model.PaymentTransaction)(nil)).
		Where("provider = ?", "sepay").Where("provider_ref = ?", ref).Count(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

// transactionByRef reads the one row a bank transfer id was recorded as.
func (f *workerFixture) transactionByRef(ref string) *model.PaymentTransaction {
	f.t.Helper()
	row := new(model.PaymentTransaction)
	if err := f.s.DB.NewSelect().Model(row).
		Where("provider = ?", "sepay").Where("provider_ref = ?", ref).Scan(f.ctx); err != nil {
		f.t.Fatalf("read transaction %s: %v", ref, err)
	}
	return row
}

// fakePayOS answers the payment-link query from a map and records every
// numeric code it was asked about.
type fakePayOS struct {
	mu      sync.Mutex
	links   map[int64]*payos.PaymentLink
	err     error
	queried []int64
}

func (f *fakePayOS) query(_ context.Context, _ *providercfg.ResolvedConfig, numericCode int64) (*payos.PaymentLink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queried = append(f.queried, numericCode)
	if f.err != nil {
		return nil, f.err
	}
	if link, ok := f.links[numericCode]; ok {
		return link, nil
	}
	return &payos.PaymentLink{PaymentLinkID: "unknown", Status: "PENDING"}, nil
}

func (f *fakePayOS) queriesFor(numericCode int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, code := range f.queried {
		if code == numericCode {
			n++
		}
	}
	return n
}

// fakeSePay returns a fixed page and records the cursor it was resumed
// from, so a test can prove the worker advanced past what it ingested.
type fakeSePay struct {
	mu       sync.Mutex
	rows     []sepay.BankTransaction
	err      error
	sinceIDs []int64
	tokens   []string
}

func (f *fakeSePay) list(_ context.Context, cfg *providercfg.ResolvedConfig, _, _ time.Time, sinceID int64) ([]sepay.BankTransaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sinceIDs = append(f.sinceIDs, sinceID)
	f.tokens = append(f.tokens, cfg.APIToken)
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func (f *fakeSePay) calls() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.sinceIDs...)
}

func (f *fakeSePay) seenTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tokens...)
}

func TestWorkerTickExpiresLapsedOrders(t *testing.T) {
	f := newWorkerFixture(t)
	order := f.lapsedOrder("lapsed-"+uuid.NewString()+"@example.com", "sepay", f.amount)

	// No provider config is needed to close an order, and a worker that
	// was built without one must still do this much rather than panic.
	w := &reconcile.Worker{Store: f.s, Log: f.log}
	if err := w.Tick(f.ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := f.reload(order.Code).Status; got != domain.OrderExpired {
		t.Fatalf("status = %q, want expired", got)
	}
}

func TestWorkerTickPayOSSettlesAPaidLink(t *testing.T) {
	f := newWorkerFixture(t)
	f.payosConfig()

	order := f.order("payos-paid-"+uuid.NewString()+"@example.com", f.amount, "payos")
	other := f.order("payos-pending-"+uuid.NewString()+"@example.com", f.amount+11, "payos")

	fake := &fakePayOS{links: map[int64]*payos.PaymentLink{
		order.NumericCode: {PaymentLinkID: "plink-" + order.Code, Status: "PAID", Amount: order.TotalMinor},
	}}
	w := f.worker()
	w.QueryPaymentLink = fake.query
	if err := w.Tick(f.ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	paid := f.reload(order.Code)
	if paid.Status != domain.OrderPaid || paid.PaidAt == nil {
		t.Fatalf("order = %+v, want paid", paid)
	}
	if got := f.reload(other.Code).Status; got != domain.OrderPending {
		t.Fatalf("unpaid order = %q, want pending", got)
	}
	if n := f.countLicences(); n != 1 {
		t.Fatalf("licences = %d, want 1", n)
	}
	tx, err := f.s.DB.NewSelect().Model((*model.PaymentTransaction)(nil)).
		Where("provider = ?", "payos").Where("order_id = ?", order.ID).Count(f.ctx)
	if err != nil || tx != 1 {
		t.Fatalf("transactions = %d, %v; want 1", tx, err)
	}

	// A second pass must not grant again, and must not even query an
	// order that is no longer pending.
	if err := w.Tick(f.ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if n := f.countLicences(); n != 1 {
		t.Fatalf("licences after second tick = %d, want 1", n)
	}
	if n := fake.queriesFor(order.NumericCode); n != 1 {
		t.Fatalf("paid order was queried %d times, want 1", n)
	}
}

func TestWorkerTickSePayIngestsAndAdvancesTheCursor(t *testing.T) {
	f := newWorkerFixture(t)
	token := f.sepayConfig()

	f.deleteTransactions("900001", "900002")
	f.t.Cleanup(func() { f.deleteTransactions("900001", "900002") })
	order := f.order("sepay-pull-"+uuid.NewString()+"@example.com", f.amount, "sepay")
	occurred := time.Now().Add(-time.Minute).Truncate(time.Second)
	refIn := "worker-in-" + uuid.NewString()
	refOut := "worker-out-" + uuid.NewString()
	rows := []sepay.BankTransaction{
		{ID: 900001, TransactionDate: occurred, AccountNumber: "0978482700", Content: "chuyen tien " + order.Code,
			TransferType: "in", TransferAmount: f.amount, ReferenceCode: refIn},
		{ID: 900002, TransactionDate: occurred.Add(30 * time.Second), AccountNumber: "0978482700",
			Content: "phi ngan hang", TransferType: "out", TransferAmount: 11000, ReferenceCode: refOut},
	}
	fake := &fakeSePay{rows: rows}
	w := f.worker()
	w.ListTransactions = fake.list

	if err := w.Tick(f.ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := f.reload(order.Code).Status; got != domain.OrderPaid {
		t.Fatalf("order = %q, want paid from the bank transfer", got)
	}
	if n := f.countLicences(); n != 1 {
		t.Fatalf("licences = %d, want 1", n)
	}

	// The outgoing transfer is not money for an order: it is recorded,
	// with its memo and reference intact, for the operator queue.
	var outgoing model.PaymentTransaction
	if err := f.s.DB.NewSelect().Model(&outgoing).Where("provider = ?", "sepay").Where("provider_ref = ?", "900002").Scan(f.ctx); err != nil {
		t.Fatalf("read outgoing transfer: %v", err)
	}
	if outgoing.Status != domain.TxUnmatched || outgoing.Direction != "out" {
		t.Fatalf("outgoing transfer = %+v", outgoing)
	}
	if outgoing.Reference != refOut || outgoing.Content != "phi ngan hang" {
		t.Fatalf("outgoing transfer lost its reference/content: %+v", outgoing)
	}

	cursor, err := f.s.GetPaymentSyncCursor(f.ctx, "sepay", "default")
	if err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if cursor.LastTransactionID != "900002" {
		t.Fatalf("cursor id = %q, want 900002", cursor.LastTransactionID)
	}
	if cursor.LastSyncedAt == nil || !cursor.LastSyncedAt.Equal(occurred.Add(30*time.Second)) {
		t.Fatalf("cursor synced at = %v", cursor.LastSyncedAt)
	}

	// The provider ignores since_id and returns the same page again.
	// Nothing is written twice and the cursor does not move backwards.
	if err := w.Tick(f.ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if n := f.countTransactions("900001"); n != 1 {
		t.Fatalf("in transactions = %d, want 1", n)
	}
	if n := f.countTransactions("900002"); n != 1 {
		t.Fatalf("out transactions = %d, want 1", n)
	}
	if n := f.countLicences(); n != 1 {
		t.Fatalf("licences after second tick = %d, want 1", n)
	}
	calls := fake.calls()
	if len(calls) != 2 || calls[0] != 0 || calls[1] != 900002 {
		t.Fatalf("listTransaction since ids = %v, want [0 900002]", calls)
	}
	if tokens := fake.seenTokens(); len(tokens) != 2 || tokens[0] != token {
		t.Fatalf("listTransactions resolved token %v, want the configured %q", tokens, token)
	}
	cursor, err = f.s.GetPaymentSyncCursor(f.ctx, "sepay", "default")
	if err != nil || cursor.LastTransactionID != "900002" {
		t.Fatalf("cursor after second tick = %+v, %v", cursor, err)
	}
}

func TestWorkerTickRecordsAnAmbiguousTransferWithoutGranting(t *testing.T) {
	f := newWorkerFixture(t)
	f.sepayConfig()
	// Two pending orders of equal size and neither names itself in the
	// transfer: the amount+window rule can only suggest, so the worker
	// records the money and grants nothing.
	f.deleteTransactions("910001")
	f.t.Cleanup(func() { f.deleteTransactions("910001") })
	f.order("amb-a-"+uuid.NewString()+"@example.com", f.amount, "sepay")
	f.order("amb-b-"+uuid.NewString()+"@example.com", f.amount, "sepay")
	ref := "worker-amb-" + uuid.NewString()
	fake := &fakeSePay{rows: []sepay.BankTransaction{{
		ID: 910001, TransactionDate: time.Now().Add(-time.Minute), AccountNumber: "0978482700",
		Content: "chuyen tien", TransferType: "in", TransferAmount: f.amount, ReferenceCode: ref,
	}}}
	w := f.worker()
	w.ListTransactions = fake.list
	if err := w.Tick(f.ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n := f.countLicences(); n != 0 {
		t.Fatalf("licences = %d, want 0: an ambiguous transfer granted a licence", n)
	}
	unmatched, err := f.s.ListUnmatchedTransactions(f.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var found *model.PaymentTransaction
	for _, row := range unmatched {
		if row.ProviderRef == "910001" {
			found = row
		}
	}
	if found == nil {
		t.Fatalf("ambiguous transfer 910001 (ref %s) is not in the unmatched queue: %+v", ref, unmatched)
	}
	if found.Reference != ref {
		t.Fatalf("unmatched row reference = %q, want %q", found.Reference, ref)
	}
	if !strings.Contains(strings.ToLower(found.IgnoreReason), "ambig") {
		t.Fatalf("ignore reason = %q, want the ambiguity explained", found.IgnoreReason)
	}
}

func TestWorkerTickKeepsGoingWhenOneProviderFails(t *testing.T) {
	f := newWorkerFixture(t)
	f.sepayConfig()
	f.payosConfig()

	lapsed := f.lapsedOrder("isolation-lapsed-"+uuid.NewString()+"@example.com", "payos", f.amount+3)
	payosOrder := f.order("isolation-payos-"+uuid.NewString()+"@example.com", f.amount+5, "payos")
	sepayOrder := f.order("isolation-sepay-"+uuid.NewString()+"@example.com", f.amount+9, "sepay")

	payosFake := &fakePayOS{err: errors.New("payos is down")}
	// The bank transaction id is unique per run: (provider, provider_ref)
	// is the dedupe key, and a fixed id would make the second run settle
	// onto the first run's row.
	bankID := time.Now().UnixNano() / 1000
	sepayFake := &fakeSePay{rows: []sepay.BankTransaction{{
		ID: bankID, TransactionDate: time.Now().Add(-time.Minute), AccountNumber: "0978482700",
		Content: "tra tien " + sepayOrder.Code, TransferType: "in",
		TransferAmount: f.amount + 9, ReferenceCode: "worker-iso-" + uuid.NewString(),
	}}}
	w := f.worker()
	w.QueryPaymentLink = payosFake.query
	w.ListTransactions = sepayFake.list

	if err := w.Tick(f.ctx); err != nil {
		t.Fatalf("tick = %v, want the provider failures contained", err)
	}
	if got := f.reload(lapsed.Code).Status; got != domain.OrderExpired {
		t.Fatalf("lapsed order = %q, want expired", got)
	}
	if got := f.reload(payosOrder.Code).Status; got != domain.OrderPending {
		t.Fatalf("payos order = %q, want pending after a query failure", got)
	}
	if got := f.reload(sepayOrder.Code).Status; got != domain.OrderPaid {
		t.Fatalf("sepay order = %q, want paid: a payos failure stopped the bank pass", got)
	}
}

func TestWorkerTickWithoutConfigOnlyExpires(t *testing.T) {
	f := newWorkerFixture(t)
	// A developer install with no encryption key has no provider config
	// rows at all; the worker must still close orders and never panic on
	// a nil resolver.
	lapsed := f.lapsedOrder("noconfig-"+uuid.NewString()+"@example.com", "sepay", f.amount)
	pending := f.order("noconfig-pending-"+uuid.NewString()+"@example.com", f.amount+1, "sepay")
	w := &reconcile.Worker{
		Store: f.s, Matcher: &reconcile.Matcher{Store: f.s}, Settlement: f.settle, Log: f.log,
	}
	if err := w.Tick(f.ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := f.reload(lapsed.Code).Status; got != domain.OrderExpired {
		t.Fatalf("status = %q, want expired", got)
	}
	// No provider row means no credentials, so no pull and no grant.
	if got := f.reload(pending.Code).Status; got != domain.OrderPending {
		t.Fatalf("pending order = %q, want pending: a config-less worker settled something", got)
	}
}

// A single pending order of the transfer's amount is a suggestion, not a
// match. The worker must leave the order pending and queue the transfer
// with the candidate's code so an operator can confirm it.
func TestWorkerTickAmountOnlySuggestionIsQueuedNotSettled(t *testing.T) {
	f := newWorkerFixture(t)
	f.sepayConfig()
	f.deleteTransactions("920001")
	f.t.Cleanup(func() { f.deleteTransactions("920001") })
	order := f.order("suggest-"+uuid.NewString()+"@example.com", f.amount, "sepay")
	ref := "worker-suggest-" + uuid.NewString()
	fake := &fakeSePay{rows: []sepay.BankTransaction{{
		ID: 920001, TransactionDate: time.Now().Add(-time.Minute), AccountNumber: "0978482700",
		Content: "chuyen tien", TransferType: "in", TransferAmount: f.amount, ReferenceCode: ref,
	}}}
	w := f.worker()
	w.ListTransactions = fake.list
	if err := w.Tick(f.ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := f.reload(order.Code).Status; got != domain.OrderPending {
		t.Fatalf("order = %q, want pending: an amount-only suggestion settled the order", got)
	}
	if n := f.countLicences(); n != 0 {
		t.Fatalf("licences = %d, want 0: an amount-only suggestion granted", n)
	}
	tx := f.transactionByRef("920001")
	if tx.Status != domain.TxUnmatched {
		t.Fatalf("transaction status = %q, want unmatched", tx.Status)
	}
	if !strings.Contains(tx.IgnoreReason, "suggested="+order.Code) {
		t.Fatalf("ignore reason = %q, want suggested=%s", tx.IgnoreReason, order.Code)
	}
	if tx.Reference != ref {
		t.Fatalf("reference = %q, want %q", tx.Reference, ref)
	}
}

// SePay should only ever say in or out. A blank or unknown transfer type
// is money the worker cannot classify: it is never treated as incoming,
// even when the memo names an order, and the raw type is kept for the
// operator.
func TestWorkerTickUnknownTransferTypeIsQueuedNotSettled(t *testing.T) {
	for _, transferType := range []string{"", "other"} {
		name := transferType
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.sepayConfig()
			bankID := time.Now().UnixNano()/1000 + int64(len(transferType))
			ref := "worker-unknown-" + uuid.NewString()
			f.deleteTransactions(strconv.FormatInt(bankID, 10))
			f.t.Cleanup(func() { f.deleteTransactions(strconv.FormatInt(bankID, 10)) })
			order := f.order("unknown-type-"+uuid.NewString()+"@example.com", f.amount, "sepay")
			fake := &fakeSePay{rows: []sepay.BankTransaction{{
				ID: bankID, TransactionDate: time.Now().Add(-time.Minute), AccountNumber: "0978482700",
				Content: "chuyen tien " + order.Code, TransferType: transferType,
				TransferAmount: f.amount, ReferenceCode: ref,
			}}}
			w := f.worker()
			w.ListTransactions = fake.list
			if err := w.Tick(f.ctx); err != nil {
				t.Fatalf("tick: %v", err)
			}
			if got := f.reload(order.Code).Status; got != domain.OrderPending {
				t.Fatalf("order = %q, want pending: an unknown transfer type settled it", got)
			}
			if n := f.countLicences(); n != 0 {
				t.Fatalf("licences = %d, want 0", n)
			}
			tx := f.transactionByRef(strconv.FormatInt(bankID, 10))
			if tx.Status != domain.TxUnmatched {
				t.Fatalf("transaction status = %q, want unmatched", tx.Status)
			}
			if !strings.Contains(strings.ToLower(tx.IgnoreReason), "transfer_type") {
				t.Fatalf("ignore reason = %q, want the raw transfer type named", tx.IgnoreReason)
			}
			var raw map[string]any
			if err := json.Unmarshal(tx.Raw, &raw); err != nil || raw["transfer_type"] != transferType {
				t.Fatalf("raw = %s, want transfer_type %q", tx.Raw, transferType)
			}
		})
	}
}

// A bank memo is written by a human: the code may be lowercase. Matching
// normalises it, but the token boundary still stops a code glued to a
// longer identifier from matching.
func TestWorkerTickMemoCodeIsCaseInsensitiveAndTokenBounded(t *testing.T) {
	f := newWorkerFixture(t)
	f.sepayConfig()
	f.deleteTransactions("930001", "930002")
	f.t.Cleanup(func() { f.deleteTransactions("930001", "930002") })
	order := f.order("lower-"+uuid.NewString()+"@example.com", f.amount, "sepay")
	noise := f.order("noise-"+uuid.NewString()+"@example.com", f.amount+7, "sepay")
	occurred := time.Now().Add(-time.Minute)
	fake := &fakeSePay{rows: []sepay.BankTransaction{
		{ID: 930001, TransactionDate: occurred, AccountNumber: "0978482700",
			Content: "chuyen tien " + strings.ToLower(order.Code), TransferType: "in",
			TransferAmount: f.amount, ReferenceCode: "worker-lower-" + uuid.NewString()},
		{ID: 930002, TransactionDate: occurred.Add(time.Second), AccountNumber: "0978482700",
			Content: "tracking" + noise.Code, TransferType: "in",
			TransferAmount: f.amount + 7, ReferenceCode: "worker-noise-" + uuid.NewString()},
	}}
	w := f.worker()
	w.ListTransactions = fake.list
	if err := w.Tick(f.ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := f.reload(order.Code).Status; got != domain.OrderPaid {
		t.Fatalf("lowercase memo order = %q, want paid", got)
	}
	if got := f.reload(noise.Code).Status; got != domain.OrderPending {
		t.Fatalf("embedded code order = %q, want pending: a token boundary was lost", got)
	}
	if n := f.countLicences(); n != 1 {
		t.Fatalf("licences = %d, want 1", n)
	}
	if got := f.transactionByRef("930002").Status; got != domain.TxUnmatched {
		t.Fatalf("noise transaction = %q, want unmatched", got)
	}
}

// expiryRecorder captures the rows the sweep announced.
type expiryRecorder struct {
	orders []*model.PaymentOrder
}

func (r *expiryRecorder) OrderExpired(_ context.Context, order *model.PaymentOrder) {
	r.orders = append(r.orders, order)
}

// One event per order the sweep actually moved. A second pass has
// nothing left to move, so it must not repeat the announcement.
func TestWorkerTickEmitsOrderExpiredOnce(t *testing.T) {
	f := newWorkerFixture(t)
	order := f.lapsedOrder("expired-event-"+uuid.NewString()+"@example.com", "sepay", f.amount)
	rec := new(expiryRecorder)
	w := &reconcile.Worker{Store: f.s, Log: f.log, Events: rec}

	countOrder := func() int {
		n := 0
		for _, got := range rec.orders {
			if got.ID == order.ID {
				n++
				if got.Status != domain.OrderExpired {
					t.Fatalf("emitted status = %q, want expired", got.Status)
				}
			}
		}
		return n
	}

	if err := w.Tick(f.ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n := countOrder(); n != 1 {
		t.Fatalf("lapsed order emitted %d times, want once (all emitted: %+v)", n, rec.orders)
	}
	// The sweep is install-wide and earlier tests leave lapsed rows
	// behind, so the first tick may announce them too; what must not
	// happen is a second announcement of anything.
	first := len(rec.orders)
	if err := w.Tick(f.ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if len(rec.orders) != first || countOrder() != 1 {
		t.Fatalf("second tick re-emitted %d rows, want none", len(rec.orders)-first)
	}
}
