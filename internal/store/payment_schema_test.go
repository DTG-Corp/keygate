package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/internal/testsupport/testdb"
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
			"SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = ?", table,
		).Scan(ctx, &n)
		if err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("table %s missing", table)
		}
	}
}

// paymentSchemaProductAndPlan creates the product and plan the probes
// below hang their rows on. The cleanup deletes the plan (its
// plan_prices rows cascade) and then the product (its provider configs
// and its plans cascade).
func paymentSchemaProductAndPlan(t *testing.T, s *store.Store) (*model.Product, *model.Plan) {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]

	product := &model.Product{Name: "Schema Probe", Slug: "schema-probe-" + suffix, Type: "saas"}
	if err := s.CreateProduct(ctx, product); err != nil {
		t.Fatalf("create product: %v", err)
	}
	plan := &model.Plan{
		ProductID:    product.ID,
		Name:         "Schema Probe",
		Slug:         "schema-probe-plan-" + suffix,
		LicenseType:  "subscription",
		LicenseModel: "standard",
		GraceDays:    7,
	}
	if err := s.CreatePlan(ctx, plan); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = s.DB.NewRaw("DELETE FROM plans WHERE id = ?", plan.ID).Exec(ctx)
		_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", product.ID).Exec(ctx)
	})
	return product, plan
}

// A whole PaymentOrder round-trips through Bun: the database assigns the
// identity numeric_code, a VND order stays at currency_exponent 0, the
// JSONB buyer/metadata read back as objects, and the status is pending.
// This is the shape T8/T9 write and T11 reads.
func TestPaymentOrderModelRoundTrip(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	product, plan := paymentSchemaProductAndPlan(t, s)

	order := &model.PaymentOrder{
		ID:            uuid.NewString(),
		Code:          "KG-RT-" + uuid.NewString()[:8],
		ProductID:     product.ID,
		PlanID:        plan.ID,
		Email:         "buyer@example.com",
		Kind:          "new",
		Quantity:      1,
		SubtotalMinor: 199000,
		DiscountMinor: 0,
		TaxMinor:      0,
		TotalMinor:    199000,
		Currency:      "VND",
		CurrencyExp:   0,
		TaxRateBP:     0,
		Provider:      "payos",
		Status:        "pending",
		ExpiresAt:     time.Now().UTC().Add(15 * time.Minute),
		Buyer:         json.RawMessage(`{"name":"A"}`),
		Metadata:      json.RawMessage(`{"k":"v"}`),
	}
	if _, err := s.DB.NewInsert().Model(order).Exec(ctx); err != nil {
		t.Fatalf("insert payment order: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DB.NewRaw("DELETE FROM payment_orders WHERE id = ?", order.ID).Exec(context.Background())
	})

	var got model.PaymentOrder
	if err := s.DB.NewSelect().Model(&got).Where("id = ?", order.ID).Scan(ctx); err != nil {
		t.Fatalf("select payment order: %v", err)
	}
	if got.NumericCode == 0 {
		t.Fatal("numeric_code = 0, want the identity value the database assigns")
	}
	if got.CurrencyExp != 0 {
		t.Fatalf("currency_exponent = %d, want 0 for VND (Bun fell back to a column default)", got.CurrencyExp)
	}
	if got.Status != "pending" {
		t.Fatalf("status = %q, want pending", got.Status)
	}
	for name, raw := range map[string]json.RawMessage{"buyer": got.Buyer, "metadata": got.Metadata} {
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("%s = %q, want a JSON object: %v", name, raw, err)
		}
		if len(obj) == 0 {
			t.Fatalf("%s = %q, want a non-empty JSON object", name, raw)
		}
	}
}

// A VND price has currency exponent 0. Bun treats a zero field tagged
// with default: as "use the column default", so an exponent of 0 would
// be stored as 2 — the tag must not carry default:2.
func TestPlanPriceStoresZeroCurrencyExponent(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	_, plan := paymentSchemaProductAndPlan(t, s)

	price := &model.PlanPrice{
		ID:               uuid.NewString(),
		PlanID:           plan.ID,
		Provider:         "payos",
		Currency:         "VND",
		AmountMinor:      199000,
		CurrencyExponent: 0,
		BillingInterval:  "monthly",
		Active:           true,
	}
	if _, err := s.DB.NewInsert().Model(price).Exec(ctx); err != nil {
		t.Fatalf("insert plan price: %v", err)
	}

	var stored int16
	if err := s.DB.NewRaw(
		"SELECT currency_exponent FROM plan_prices WHERE id = ?", price.ID,
	).Scan(ctx, &stored); err != nil {
		t.Fatalf("read currency exponent: %v", err)
	}
	if stored != 0 {
		t.Fatalf("VND price exponent = %d, want 0 (Bun fell back to the column default 2)", stored)
	}
}

// A provider config inserted with Enabled false must be stored disabled:
// a default:true tag turns the false into SQL DEFAULT (TRUE).
func TestProviderConfigStoresDisabled(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	product, _ := paymentSchemaProductAndPlan(t, s)

	cfg := &model.PaymentProviderConfig{
		ID:        uuid.NewString(),
		Provider:  "sepay",
		ProductID: product.ID,
		Mode:      "test",
		Enabled:   false,
		Config:    json.RawMessage(`{"bank":"VCB"}`),
	}
	if _, err := s.DB.NewInsert().Model(cfg).Exec(ctx); err != nil {
		t.Fatalf("insert disabled provider config: %v", err)
	}

	var enabled bool
	var credentialBytes int
	if err := s.DB.NewRaw(
		"SELECT enabled, octet_length(credentials_encrypted) FROM payment_provider_configs WHERE id = ?", cfg.ID,
	).Scan(ctx, &enabled, &credentialBytes); err != nil {
		t.Fatalf("read provider config: %v", err)
	}
	if enabled {
		t.Fatal("provider config inserted with Enabled:false stored enabled")
	}
	if credentialBytes != 0 {
		t.Fatalf("nil credentials stored %d bytes, want the ''::bytea default", credentialBytes)
	}

	var bank string
	if err := s.DB.NewRaw(
		"SELECT config->>'bank' FROM payment_provider_configs WHERE id = ?", cfg.ID,
	).Scan(ctx, &bank); err != nil {
		t.Fatalf("read provider config jsonb: %v", err)
	}
	if bank != "VCB" {
		t.Fatalf("config->>'bank' = %q, want VCB (jsonb stored as an object, not base64)", bank)
	}
}

// Raw is json.RawMessage so the provider payload is stored as JSON. A
// []byte field with type:jsonb is JSON-marshalled as a base64 string.
func TestPaymentRawStoredAsJSON(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()

	txn := &model.PaymentTransaction{
		ID:          uuid.NewString(),
		Provider:    "sepay",
		ProviderRef: uuid.NewString(),
		AmountMinor: 250000,
		Currency:    "VND",
		Status:      "matched",
		Raw:         json.RawMessage(`{"tx":1,"code":"KG-1"}`),
		OccurredAt:  time.Now().UTC(),
	}
	if _, err := s.DB.NewInsert().Model(txn).Exec(ctx); err != nil {
		t.Fatalf("insert transaction: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DB.NewRaw("DELETE FROM payment_transactions WHERE id = ?", txn.ID).Exec(context.Background())
	})

	event := &model.PaymentEvent{
		ID:         uuid.NewString(),
		Provider:   "sepay",
		EventID:    uuid.NewString(),
		Kind:       "payment.succeeded",
		Raw:        json.RawMessage(`{"event":"payment.succeeded","amount":250000}`),
		OccurredAt: time.Now().UTC(),
	}
	if _, err := s.DB.NewInsert().Model(event).Exec(ctx); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DB.NewRaw("DELETE FROM payment_events WHERE id = ?", event.ID).Exec(context.Background())
	})

	var txnRaw, eventRaw string
	if err := s.DB.NewRaw("SELECT raw::text FROM payment_transactions WHERE id = ?", txn.ID).Scan(ctx, &txnRaw); err != nil {
		t.Fatalf("read transaction raw: %v", err)
	}
	if err := s.DB.NewRaw("SELECT raw::text FROM payment_events WHERE id = ?", event.ID).Scan(ctx, &eventRaw); err != nil {
		t.Fatalf("read event raw: %v", err)
	}

	for name, raw := range map[string]string{"transaction": txnRaw, "event": eventRaw} {
		var obj map[string]any
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			t.Fatalf("%s raw = %q, want a JSON object (a base64 string is the []byte bug): %v", name, raw, err)
		}
		if len(obj) == 0 {
			t.Fatalf("%s raw = %q, want a non-empty JSON object", name, raw)
		}
	}

	// Direction defaults to 'in' for money received. A reconciler-stored
	// refund/payout is 'out' and keeps the bank reference, content and
	// account the unmatched queue displays.
	outTxn := &model.PaymentTransaction{
		ID:              uuid.NewString(),
		Provider:        "sepay",
		ProviderRef:     uuid.NewString(),
		AmountMinor:     50000,
		Currency:        "VND",
		Direction:       "out",
		ProviderAccount: "0123456789",
		Reference:       "REF-OUT-1",
		Content:         "REFUND KG-1",
		Status:          "unmatched",
		Raw:             json.RawMessage(`{"transferType":"out"}`),
		OccurredAt:      time.Now().UTC(),
	}
	if _, err := s.DB.NewInsert().Model(outTxn).Exec(ctx); err != nil {
		t.Fatalf("insert outgoing transaction: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DB.NewRaw("DELETE FROM payment_transactions WHERE id = ?", outTxn.ID).Exec(context.Background())
	})

	var inDirection string
	if err := s.DB.NewRaw(
		"SELECT direction FROM payment_transactions WHERE id = ?", txn.ID,
	).Scan(ctx, &inDirection); err != nil {
		t.Fatalf("read default direction: %v", err)
	}
	if inDirection != "in" {
		t.Fatalf("transaction inserted without direction stored %q, want the 'in' default", inDirection)
	}

	var stored model.PaymentTransaction
	if err := s.DB.NewSelect().Model(&stored).Where("id = ?", outTxn.ID).Scan(ctx); err != nil {
		t.Fatalf("select outgoing transaction: %v", err)
	}
	if stored.Direction != "out" {
		t.Fatalf("direction = %q, want out", stored.Direction)
	}
	if stored.ProviderAccount != outTxn.ProviderAccount {
		t.Fatalf("provider_account = %q, want %q", stored.ProviderAccount, outTxn.ProviderAccount)
	}
	if stored.Reference != outTxn.Reference {
		t.Fatalf("reference = %q, want %q", stored.Reference, outTxn.Reference)
	}
	if stored.Content != outTxn.Content {
		t.Fatalf("content = %q, want %q", stored.Content, outTxn.Content)
	}
	if stored.AttemptID != nil || stored.MatchedBy != nil || stored.MatchedAt != nil {
		t.Fatalf("attempt/matched columns = (%v, %v, %v), want NULL when omitted", stored.AttemptID, stored.MatchedBy, stored.MatchedAt)
	}
}

// PriceIncludesTax is a nullable pointer: omitting it lets the column's
// DEFAULT TRUE apply, while an explicit false pointer is still stored.
// A plain bool would write false on every insert.
func TestProductPriceIncludesTaxDefault(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	product, _ := paymentSchemaProductAndPlan(t, s)

	var omitted bool
	if err := s.DB.NewRaw(
		"SELECT price_includes_tax FROM products WHERE id = ?", product.ID,
	).Scan(ctx, &omitted); err != nil {
		t.Fatalf("read omitted price_includes_tax: %v", err)
	}
	if !omitted {
		t.Fatal("product created without price_includes_tax stored false, want the column default true")
	}

	no := false
	explicit := &model.Product{
		Name:             "Schema Probe",
		Slug:             "schema-probe-tax-" + uuid.NewString()[:8],
		Type:             "saas",
		PriceIncludesTax: &no,
	}
	if err := s.CreateProduct(ctx, explicit); err != nil {
		t.Fatalf("create product with explicit false: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.DB.NewRaw("DELETE FROM products WHERE id = ?", explicit.ID).Exec(context.Background())
	})

	var stored bool
	if err := s.DB.NewRaw(
		"SELECT price_includes_tax FROM products WHERE id = ?", explicit.ID,
	).Scan(ctx, &stored); err != nil {
		t.Fatalf("read explicit price_includes_tax: %v", err)
	}
	if stored {
		t.Fatal("product created with an explicit false pointer stored true")
	}
}

// Migration 06 fixes the schema the later tasks rely on: the enabled
// default is false, the transaction index carries the spec's name, and
// the VN uniqueness indexes are partial.
func TestPaymentSchemaSpecFixes(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()

	for _, tc := range []struct {
		table, column, want string
	}{
		{"payment_provider_configs", "enabled", "false"},
		// The model no longer carries default:2, but raw SQL still
		// gets the documented default.
		{"plan_prices", "currency_exponent", "2"},
	} {
		var got string
		if err := s.DB.NewRaw(
			`SELECT column_default FROM information_schema.columns
			 WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`,
			tc.table, tc.column,
		).Scan(ctx, &got); err != nil {
			t.Fatalf("read %s.%s default: %v", tc.table, tc.column, err)
		}
		if got != tc.want {
			t.Fatalf("%s.%s default = %q, want %q", tc.table, tc.column, got, tc.want)
		}
	}

	if def, ok := schemaIndexDef(t, s, "payment_transactions_provider_ref"); ok {
		t.Fatalf("pre-06 transaction index name still exists: %s", def)
	}
	def, ok := schemaIndexDef(t, s, "payment_transactions_provider_id")
	if !ok {
		t.Fatal("index payment_transactions_provider_id missing (spec §4.5 name)")
	}
	if !strings.Contains(def, "(provider, provider_ref)") {
		t.Fatalf("payment_transactions_provider_id = %s, want columns (provider, provider_ref)", def)
	}

	def, ok = schemaIndexDef(t, s, "idx_license_upgrades_session")
	if !ok {
		t.Fatal("index idx_license_upgrades_session missing")
	}
	if !strings.Contains(def, "WHERE") || !strings.Contains(def, "<> ''") {
		t.Fatalf("idx_license_upgrades_session = %s, want a partial index on non-empty sessions", def)
	}

	def, ok = schemaIndexDef(t, s, "license_renewals_payment_order_once")
	if !ok {
		t.Fatal("index license_renewals_payment_order_once missing (spec §4.11)")
	}
	if !strings.Contains(def, "IS NOT NULL") {
		t.Fatalf("license_renewals_payment_order_once = %s, want WHERE payment_order_id IS NOT NULL", def)
	}
}

// schemaIndexDef returns one index's definition in the current schema, or
// false when no such index exists.
func schemaIndexDef(t *testing.T, s *store.Store, name string) (string, bool) {
	t.Helper()
	var def string
	err := s.DB.NewRaw(
		"SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND indexname = ?", name,
	).Scan(context.Background(), &def)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false
	case err != nil:
		t.Fatalf("read index %s: %v", name, err)
	}
	return def, true
}
