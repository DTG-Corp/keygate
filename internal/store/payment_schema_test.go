package store_test

import (
	"context"
	"testing"

	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

func TestPaymentSchemaTablesExist(t *testing.T) {
	s := testdb.Store(t) // helper added in this step; opens TEST_DATABASE_URL
	ctx := context.Background()
	for _, table := range []string{
		"plan_prices", "payment_provider_configs", "payment_orders",
		"payment_order_discounts", "payment_attempts", "payment_transactions",
		"payment_events", "payment_refunds", "payment_sync_cursors",
		"coupons", "coupon_scopes", "coupon_redemptions",
	} {
		var n int
		err := s.DB.NewRaw(
			"SELECT count(*) FROM information_schema.tables WHERE table_name = ?", table,
		).Scan(ctx, &n)
		if err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("table %s missing", table)
		}
	}
}
