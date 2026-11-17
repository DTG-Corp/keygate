package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/internal/testsupport/testdb"
)

// portalOrder writes one order in the fixture product/plan. Email and
// buyer are separate so the ownership predicate can be checked on both.
func portalOrder(t *testing.T, s *store.Store, productID, planID, email, buyer string) *model.PaymentOrder {
	t.Helper()
	o, err := s.CreatePaymentOrder(context.Background(), store.CreateOrderInput{
		ProductID: productID, PlanID: planID, Email: email, Kind: "new", Quantity: 1,
		SubtotalMinor: 1_190_000, TotalMinor: 1_190_000, Currency: "VND",
		Provider: "sepay", ExpiresAt: time.Now().Add(time.Hour),
		Buyer: []byte(buyer), Metadata: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create order %s: %v", email, err)
	}
	return o
}

// The portal list is the customer's own orders: the order email or the
// buyer snapshot email, never somebody else's. The 1a schema has no
// external_customer_id on payment_orders, so email is the whole identity.
func TestPortalPaymentOrdersAreCustomerScoped(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	productID, planID := seedProductPlan(t, s)

	alice := portalOrder(t, s, productID, planID, "Alice@Example.com", `{"email":"alice@example.com"}`)
	// The buyer snapshot is a second identity the order carries: an
	// order created for a different licence email but with alice as the
	// buyer is still hers.
	aliceBuyer := portalOrder(t, s, productID, planID, "licence-owner@example.com", `{"email":"ALICE@example.com"}`)
	bob := portalOrder(t, s, productID, planID, "bob@example.com", `{"email":"bob@example.com"}`)

	rows, total, err := s.ListPortalPaymentOrders(ctx, "alice@example.com", store.Page{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("alice's orders = %d rows, total %d; want 2", len(rows), total)
	}
	// Newest first.
	if rows[0].ID != aliceBuyer.ID || rows[1].ID != alice.ID {
		t.Fatalf("order = [%s %s], want [%s %s]", rows[0].Code, rows[1].Code, aliceBuyer.Code, alice.Code)
	}
	for _, o := range rows {
		if o.ID == bob.ID {
			t.Fatalf("bob's order %s leaked into alice's list", o.Code)
		}
	}

	// Pagination reports the filtered total, not the page size.
	page, total, err := s.ListPortalPaymentOrders(ctx, "alice@example.com", store.Page{Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("paged list: %v", err)
	}
	if total != 2 || len(page) != 1 || page[0].ID != alice.ID {
		t.Fatalf("second page = %+v total %d, want the older order and total 2", page, total)
	}

	// Detail uses the same predicate: foreign looks exactly like unknown.
	if _, err := s.GetPortalPaymentOrder(ctx, bob.Code, "alice@example.com"); !errors.Is(err, store.ErrOrderNotFound) {
		t.Fatalf("foreign detail err = %v, want ErrOrderNotFound", err)
	}
	if got, err := s.GetPortalPaymentOrder(ctx, alice.Code, "alice@example.com"); err != nil || got.ID != alice.ID {
		t.Fatalf("own detail = %+v, %v", got, err)
	}
	if got, err := s.GetPortalPaymentOrder(ctx, aliceBuyer.Code, "alice@example.com"); err != nil || got.ID != aliceBuyer.ID {
		t.Fatalf("buyer-email detail = %+v, %v", got, err)
	}
	if _, err := s.GetPortalPaymentOrder(ctx, "KGZZZZZZ", "alice@example.com"); !errors.Is(err, store.ErrOrderNotFound) {
		t.Fatalf("unknown detail err = %v, want ErrOrderNotFound", err)
	}

	// The invoice ownership check loads the order by id; a missing one is
	// the same not-found answer.
	byID, err := s.GetPaymentOrderByID(ctx, alice.ID)
	if err != nil || byID.Code != alice.Code {
		t.Fatalf("by id = %+v, %v", byID, err)
	}
	if _, err := s.GetPaymentOrderByID(ctx, store.NewID()); !errors.Is(err, store.ErrOrderNotFound) {
		t.Fatalf("missing by id err = %v, want ErrOrderNotFound", err)
	}
}

// The billing profile is one row per user, created on first PUT and
// replaced on the next; a user with no row reads as no rows (the handler
// turns that into an empty buyer, not a 404).
func TestBillingProfileUpsert(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	u := &model.User{Email: "profile-" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "") + "@example.com", Name: "Profile"}
	if err := s.UpsertUser(ctx, u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DB.NewRaw("DELETE FROM customer_billing_profiles WHERE user_id = ?", u.ID).Exec(context.Background())
		_, _ = s.DB.NewRaw("DELETE FROM users WHERE id = ?", u.ID).Exec(context.Background())
	})

	if _, err := s.GetBillingProfile(ctx, u.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing profile err = %v, want sql.ErrNoRows", err)
	}

	first := []byte(`{"name":"Nguyen Van A","tax_code":"0101234567"}`)
	got, err := s.UpsertBillingProfile(ctx, u.ID, first)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if got.UserID != u.ID || !jsonEqual(got.Buyer, first) {
		t.Fatalf("first profile = %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps = %+v", got)
	}

	second := []byte(`{"name":"Nguyen Van B","request_invoice":true}`)
	got, err = s.UpsertBillingProfile(ctx, u.ID, second)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if !jsonEqual(got.Buyer, second) {
		t.Fatalf("updated buyer = %s, want %s", got.Buyer, second)
	}

	var rows int
	if err := s.DB.NewRaw("SELECT count(*) FROM customer_billing_profiles WHERE user_id = ?", u.ID).Scan(ctx, &rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("profile rows = %d, want 1", rows)
	}
	read, err := s.GetBillingProfile(ctx, u.ID)
	if err != nil || !jsonEqual(read.Buyer, second) {
		t.Fatalf("read back = %+v, %v", read, err)
	}
}

func jsonEqual(raw []byte, want []byte) bool {
	var a, b any
	if json.Unmarshal(raw, &a) != nil || json.Unmarshal(want, &b) != nil {
		return false
	}
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}
