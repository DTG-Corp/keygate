package settlement_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/payment/settlement"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedSellable creates a product and a perpetual plan that sells it, and
// removes them — and everything the test hung on them — when it ends.
// Integration tests share one database, so every slug and email carries
// a UUID rather than a timestamp.
func seedSellable(t *testing.T, s *store.Store) (productID, planID string) {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:13]
	p := &model.Product{Name: "SettleCo", Slug: "settle-co-" + suffix, Type: "desktop"}
	if err := s.CreateProduct(ctx, p); err != nil {
		t.Fatal(err)
	}
	pl := &model.Plan{ProductID: p.ID, Name: "Pro", Slug: "settle-pro-" + suffix,
		LicenseType: "perpetual", LicenseModel: "standard", MaxActivations: 3, Active: true}
	if err := s.CreatePlan(ctx, pl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.DB.NewRaw("DELETE FROM payment_transactions WHERE order_id IN (SELECT id FROM payment_orders WHERE product_id = ?)", p.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM payment_events WHERE order_id IN (SELECT id FROM payment_orders WHERE product_id = ?)", p.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM payment_orders WHERE product_id = ?", p.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM licenses WHERE product_id = ?", p.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM plans WHERE id = ?", pl.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", p.ID).Exec(ctx)
	})
	return p.ID, pl.ID
}

func TestSettleNewOrderGrantsLicenseOnce(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedSellable(t, s)
	email := "new-" + uuid.NewString()[:8] + "@example.com"
	order, err := s.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: productID, PlanID: planID, Email: email, Kind: "new", Quantity: 1,
		SubtotalMinor: 1190000, TotalMinor: 1190000, Currency: "VND", Provider: "sepay",
		ExpiresAt: time.Now().Add(time.Hour), Buyer: []byte(`{}`), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := &settlement.Service{Store: s, Log: testLogger()}
	ev := provider.WebhookEvent{Provider: "sepay", ProviderEventID: "92704", ProviderRef: "92704",
		OrderCode: order.Code, AmountMinor: 1190000, Currency: "VND", Status: domain.OrderPaid,
		TransferType: "in", OccurredAt: time.Now(), Raw: []byte(`{"id":92704}`)}
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("settle: %v", err)
	}
	got, err := s.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.OrderPaid || got.PaidAt == nil {
		t.Fatalf("order = %+v", got)
	}
	var lic model.License
	if err := s.DB.NewSelect().Model(&lic).Where("email = ?", email).Scan(ctx); err != nil {
		t.Fatalf("license: %v", err)
	}
	if lic.Status != model.StatusActive || lic.PlanID != planID || lic.LicenseKey == "" {
		t.Fatalf("license = %+v", lic)
	}
	if got.LicenseID != lic.ID {
		t.Fatalf("order.license_id = %q, want %q", got.LicenseID, lic.ID)
	}
	// The money is on the ledger as matched, and the settlement error an
	// earlier attempt may have left is gone.
	var matched int
	if err := s.DB.NewRaw("SELECT count(*) FROM payment_transactions WHERE order_id = ? AND status = ?",
		order.ID, domain.TxMatched).Scan(ctx, &matched); err != nil {
		t.Fatal(err)
	}
	if matched != 1 {
		t.Fatalf("matched transactions = %d, want 1", matched)
	}
	if got.LastSettlementError != "" {
		t.Fatalf("last_settlement_error = %q", got.LastSettlementError)
	}

	// A second delivery (retry) is a no-op: still one licence.
	if err := svc.Settle(ctx, order.ID, "sepay", ev); err != nil {
		t.Fatalf("second settle: %v", err)
	}
	n, err := s.DB.NewSelect().Table("licenses").Where("email = ?", email).Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("licences = %d", n)
	}
}
