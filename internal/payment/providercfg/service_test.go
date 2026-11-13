package providercfg_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/providercfg"
	"github.com/tabloy/keygate/internal/store"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m, "../../../db/migrations")) }

func TestResolveProductOverrideWins(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	master := bytes.Repeat([]byte{0x17}, 32)
	codec, err := providercfg.NewCodec(master)
	if err != nil {
		t.Fatal(err)
	}
	svc := providercfg.NewService(s, codec, "live")

	// Install default for sepay.
	if _, err := svc.Upsert(ctx, providercfg.UpsertInput{
		Provider: "sepay", Mode: "live", Enabled: true,
		Config:        providercfg.SepayConfig{BankName: "MB Bank", BankCode: "MB", BankBIN: "970422", AccountNo: "0978482700", AccountName: "NGUYEN VAN A"},
		Credentials:   providercfg.SepayCredentials{APIToken: "default-token"},
		WebhookSecret: "default-secret",
	}); err != nil {
		t.Fatalf("upsert default: %v", err)
	}
	got, err := svc.Resolve(ctx, "", "sepay", "live")
	if err != nil || got.Sepay.AccountNo != "0978482700" || got.APIToken != "default-token" {
		t.Fatalf("resolve default = %+v, %v", got, err)
	}
	if got.WebhookSecret != "default-secret" {
		t.Fatalf("webhook secret not decrypted: %q", got.WebhookSecret)
	}

	// Product override for payos. payment_provider_configs.product_id is
	// a real FK, so the override needs a real product row.
	prodX := &model.Product{Name: "Override", Slug: "override-" + time.Now().Format("150405.000000"), Type: "desktop"}
	if err := s.CreateProduct(ctx, prodX); err != nil {
		t.Fatal(err)
	}
	prodY := &model.Product{Name: "Disabled", Slug: "disabled-" + time.Now().Format("150405.000000"), Type: "desktop"}
	if err := s.CreateProduct(ctx, prodY); err != nil {
		t.Fatal(err)
	}
	code, err := svc.Upsert(ctx, providercfg.UpsertInput{
		Provider: "payos", Mode: "live", Enabled: true, ProductID: prodX.ID,
		Config:        providercfg.PayOSConfig{ClientID: "client-1"},
		Credentials:   providercfg.PayOSCredentials{APIKey: "api-key", ChecksumKey: "checksum-key"},
		WebhookSecret: "payos-secret",
	})
	if err != nil || len(code.WebhookSecretEncrypted) == 0 {
		t.Fatalf("upsert override: %+v, %v", code, err)
	}
	got, err = svc.Resolve(ctx, prodX.ID, "payos", "live")
	if err != nil || got.ClientID != "client-1" {
		t.Fatalf("resolve override = %+v, %v", got, err)
	}

	// A different product must not inherit the override.
	if _, err := svc.Resolve(ctx, prodY.ID, "payos", "live"); !errors.Is(err, providercfg.ErrProviderNotConfigured) {
		t.Fatalf("resolve other = %v, want ErrProviderNotConfigured", err)
	}

	// Disabled is explicit, never a fallback to the default.
	if _, err := svc.Upsert(ctx, providercfg.UpsertInput{Provider: "payos", Mode: "live", Enabled: false, ProductID: prodY.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resolve(ctx, prodY.ID, "payos", "live"); !errors.Is(err, providercfg.ErrProviderDisabled) {
		t.Fatalf("disabled resolve = %v, want ErrProviderDisabled", err)
	}
}

// A blank secret on an update means "keep the stored one": the admin
// form cannot display it, so a save that omits it must not wipe the
// merchant's webhook verification. The row id must also survive, since
// the ciphertext is bound to it.
func TestUpsertBlankSecretKeepsStored(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x33}, 32))
	if err != nil {
		t.Fatal(err)
	}
	svc := providercfg.NewService(s, codec, "live")
	prod := &model.Product{Name: "Keep", Slug: "keep-" + time.Now().Format("150405.000000"), Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}

	first, err := svc.Upsert(ctx, providercfg.UpsertInput{
		Provider: "sepay", Mode: "test", Enabled: true, ProductID: prod.ID,
		Config:        providercfg.SepayConfig{BankName: "MB Bank", BankCode: "MB", AccountNo: "111"},
		Credentials:   providercfg.SepayCredentials{APIToken: "keep-me"},
		WebhookSecret: "keep-secret",
	})
	if err != nil {
		t.Fatalf("upsert first: %v", err)
	}
	second, err := svc.Upsert(ctx, providercfg.UpsertInput{
		Provider: "sepay", Mode: "test", Enabled: true, ProductID: prod.ID,
		Config: providercfg.SepayConfig{BankName: "MB Bank", BankCode: "MB", AccountNo: "222"},
	})
	if err != nil {
		t.Fatalf("upsert without secrets: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("row id changed on update: %s -> %s", first.ID, second.ID)
	}
	got, err := svc.Resolve(ctx, prod.ID, "sepay", "test")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.APIToken != "keep-me" || got.WebhookSecret != "keep-secret" {
		t.Fatalf("blank update dropped a secret: token=%q secret=%q", got.APIToken, got.WebhookSecret)
	}
	if got.AccountNo != "222" {
		t.Fatalf("config did not update: %q", got.AccountNo)
	}

	// Omitting config entirely (an enable/disable save) must not wipe
	// the stored bank details either.
	if _, err := svc.Upsert(ctx, providercfg.UpsertInput{
		Provider: "sepay", Mode: "test", Enabled: true, ProductID: prod.ID,
	}); err != nil {
		t.Fatalf("upsert without config: %v", err)
	}
	got, err = svc.Resolve(ctx, prod.ID, "sepay", "test")
	if err != nil {
		t.Fatalf("resolve after config-less update: %v", err)
	}
	if got.AccountNo != "222" || got.APIToken != "keep-me" || got.WebhookSecret != "keep-secret" {
		t.Fatalf("config-less update lost data: %+v", got)
	}
}

// The AAD binds the ciphertext to the row id, so the store has to insert
// with the id the service sealed against. If it generated its own id, the
// returned row would hold ciphertext that can never be opened again.
func TestUpsertSealsAgainstInsertedID(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x41}, 32))
	if err != nil {
		t.Fatal(err)
	}
	svc := providercfg.NewService(s, codec, "live")
	prod := &model.Product{Name: "Seal", Slug: "seal-" + time.Now().Format("150405.000000"), Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}

	created, err := svc.Upsert(ctx, providercfg.UpsertInput{
		Provider: "sepay", Mode: "test", Enabled: true, ProductID: prod.ID,
		Config:        providercfg.SepayConfig{BankName: "MB Bank", BankCode: "MB", AccountNo: "111"},
		Credentials:   providercfg.SepayCredentials{APIToken: "first-token"},
		WebhookSecret: "first-secret",
	})
	if err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created row has no id")
	}
	stored, err := s.FindPaymentProviderConfig(ctx, "sepay", prod.ID, "test")
	if err != nil {
		t.Fatalf("find created row: %v", err)
	}
	if stored.ID != created.ID {
		t.Fatalf("stored row id = %s, service sealed against %s", stored.ID, created.ID)
	}
	creds, err := codec.Open(stored.ID, "credentials", stored.CredentialsEncrypted)
	if err != nil {
		t.Fatalf("create sealed credentials against a discarded id: %v", err)
	}
	var sc providercfg.SepayCredentials
	if err := json.Unmarshal(creds, &sc); err != nil || sc.APIToken != "first-token" {
		t.Fatalf("stored credentials = %q, %v", creds, err)
	}
	secret, err := codec.Open(stored.ID, "webhook_secret", stored.WebhookSecretEncrypted)
	if err != nil || string(secret) != "first-secret" {
		t.Fatalf("stored webhook secret = %q, %v", secret, err)
	}

	updated, err := svc.Upsert(ctx, providercfg.UpsertInput{
		Provider: "sepay", Mode: "test", Enabled: true, ProductID: prod.ID,
		Config:        providercfg.SepayConfig{BankName: "MB Bank", BankCode: "MB", AccountNo: "222"},
		Credentials:   providercfg.SepayCredentials{APIToken: "second-token"},
		WebhookSecret: "second-secret",
	})
	if err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	if updated.ID != created.ID {
		t.Fatalf("update changed the row id: %s -> %s", created.ID, updated.ID)
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("update replaced the row: created_at %s -> %s", created.CreatedAt, updated.CreatedAt)
	}
	var rows int
	if err := s.DB.NewRaw(
		"SELECT count(*) FROM payment_provider_configs WHERE provider = 'sepay' AND product_id = ? AND mode = 'test'",
		prod.ID,
	).Scan(ctx, &rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("one slot has %d rows, want 1", rows)
	}
	creds, err = codec.Open(updated.ID, "credentials", updated.CredentialsEncrypted)
	if err != nil {
		t.Fatalf("update sealed credentials against a different id: %v", err)
	}
	sc = providercfg.SepayCredentials{}
	if err := json.Unmarshal(creds, &sc); err != nil || sc.APIToken != "second-token" {
		t.Fatalf("updated credentials = %q, %v", creds, err)
	}
}

// Two admins saving the same new slot race on the unique index. The loser
// must re-seal against the winning row's id, not leave ciphertext bound to
// the id it generated before the insert.
func TestConcurrentUpsertKeepsCiphertextDecryptable(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	svc := providercfg.NewService(s, codec, "live")
	prod := &model.Product{Name: "Race", Slug: "race-" + time.Now().Format("150405.000000"), Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}

	const writers = 5
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := svc.Upsert(ctx, providercfg.UpsertInput{
				Provider: "sepay", Mode: "test", Enabled: true, ProductID: prod.ID,
				Config:        providercfg.SepayConfig{BankName: "MB Bank", BankCode: "MB", AccountNo: fmt.Sprintf("100%d", i)},
				Credentials:   providercfg.SepayCredentials{APIToken: fmt.Sprintf("token-%d", i)},
				WebhookSecret: fmt.Sprintf("secret-%d", i),
			})
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent upsert: %v", err)
		}
	}

	row, err := s.FindPaymentProviderConfig(ctx, "sepay", prod.ID, "test")
	if err != nil {
		t.Fatalf("find row: %v", err)
	}
	creds, err := codec.Open(row.ID, "credentials", row.CredentialsEncrypted)
	if err != nil {
		t.Fatalf("racing writers left undecryptable credentials: %v", err)
	}
	var sc providercfg.SepayCredentials
	if err := json.Unmarshal(creds, &sc); err != nil || sc.APIToken == "" {
		t.Fatalf("racing credentials = %q, %v", creds, err)
	}
	secret, err := codec.Open(row.ID, "webhook_secret", row.WebhookSecretEncrypted)
	if err != nil || len(secret) == 0 {
		t.Fatalf("racing webhook secret = %q, %v", secret, err)
	}
}

// A caller that already chose the row id must get that exact primary key:
// that is the contract the service relies on to seal before it inserts.
func TestStoreUpsertUsesCallerRowID(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	prod := &model.Product{Name: "Preseal", Slug: "preseal-" + time.Now().Format("150405.000000"), Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}

	id := store.NewID()
	sealed, err := codec.Seal(id, "credentials", []byte(`{"api_token":"known-token"}`))
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.UpsertPaymentProviderConfig(ctx, store.ProviderConfigInput{
		ID: id, Provider: "sepay", ProductID: prod.ID, Mode: "test", Enabled: false,
		Config: []byte(`{"account_no":"1"}`), CredentialsEncrypted: sealed,
	})
	if err != nil {
		t.Fatalf("upsert with caller id: %v", err)
	}
	if row.ID != id {
		t.Fatalf("stored id = %s, caller asked for %s", row.ID, id)
	}
	found, err := s.FindPaymentProviderConfig(ctx, "sepay", prod.ID, "test")
	if err != nil {
		t.Fatalf("find row: %v", err)
	}
	got, err := codec.Open(found.ID, "credentials", found.CredentialsEncrypted)
	if err != nil {
		t.Fatalf("caller-sealed credentials do not open with the stored id: %v", err)
	}
	if string(got) != `{"api_token":"known-token"}` {
		t.Fatalf("stored credentials = %q", got)
	}
}

// ResolvedConfig carries plaintext secrets. Serialising it by accident
// (response.OK(c, cfg), a log line) must fail closed, not emit them.
func TestResolvedConfigRefusesSerialization(t *testing.T) {
	cfg := &providercfg.ResolvedConfig{
		Provider: "sepay", APIToken: "tok_live_123", IPNSecret: "ipn_123",
		ClientID: "client_123", APIKey: "api_key_123", ChecksumKey: "checksum_123",
		WebhookSecret: "whsec_123",
	}
	if _, err := json.Marshal(cfg); err == nil {
		t.Fatal("json.Marshal(*ResolvedConfig) succeeded; secrets could leak")
	}
	if _, err := json.Marshal(*cfg); err == nil {
		t.Fatal("json.Marshal(ResolvedConfig) succeeded; secrets could leak")
	}
}

// An enabled row whose ciphertext is missing, truncated, or decrypts to
// credentials without the provider's required fields is a broken
// configuration, not a disabled provider and not a raw cipher error.
func TestResolveInvalidEnabledRowIsConfigInvalid(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x44}, 32))
	if err != nil {
		t.Fatal(err)
	}
	svc := providercfg.NewService(s, codec, "live")
	prod := &model.Product{Name: "Broken", Slug: "broken-" + time.Now().Format("150405.000000"), Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	row, err := svc.Upsert(ctx, providercfg.UpsertInput{
		Provider: "sepay", Mode: "test", Enabled: true, ProductID: prod.ID,
		Config:        providercfg.SepayConfig{BankName: "MB Bank", BankCode: "MB", AccountNo: "111"},
		Credentials:   providercfg.SepayCredentials{APIToken: "token"},
		WebhookSecret: "secret",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	assertInvalid := func(what string) {
		t.Helper()
		_, err := svc.Resolve(ctx, prod.ID, "sepay", "test")
		if !errors.Is(err, providercfg.ErrProviderConfigInvalid) {
			t.Fatalf("%s: resolve = %v, want ErrProviderConfigInvalid", what, err)
		}
		if errors.Is(err, providercfg.ErrProviderDisabled) {
			t.Fatalf("%s: resolve returned ErrProviderDisabled", what)
		}
	}

	if err := svc.DeleteSecrets(ctx, row.ID, []string{"credentials"}); err != nil {
		t.Fatalf("clear credentials: %v", err)
	}
	assertInvalid("empty credentials ciphertext")

	if _, err := s.DB.NewRaw(
		"UPDATE payment_provider_configs SET credentials_encrypted = ? WHERE id = ?", []byte{1, 2, 3}, row.ID,
	).Exec(ctx); err != nil {
		t.Fatalf("store short ciphertext: %v", err)
	}
	assertInvalid("short credentials ciphertext")

	sealed, err := codec.Seal(row.ID, "credentials", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw(
		"UPDATE payment_provider_configs SET credentials_encrypted = ? WHERE id = ?", sealed, row.ID,
	).Exec(ctx); err != nil {
		t.Fatalf("store empty credentials: %v", err)
	}
	assertInvalid("credentials missing api_token")

	sealed, err = codec.Seal(row.ID, "credentials", []byte(`{"api_token":"token"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.NewRaw(
		"UPDATE payment_provider_configs SET credentials_encrypted = ?, config = ? WHERE id = ?",
		sealed, `{}`, row.ID,
	).Exec(ctx); err != nil {
		t.Fatalf("store empty config: %v", err)
	}
	assertInvalid("config missing bank details")
}

// Validation happens before any write: the admin API cannot store an
// enabled row it could not later resolve.
func TestUpsertValidatesProviderConfigs(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x45}, 32))
	if err != nil {
		t.Fatal(err)
	}
	svc := providercfg.NewService(s, codec, "live")
	// A fresh slot per case: no pre-existing row can supply a secret or
	// credentials that the case under test omitted.
	prod := &model.Product{Name: "Validate", Slug: "validate-" + time.Now().Format("150405.000000"), Type: "desktop"}
	if err := s.CreateProduct(ctx, prod); err != nil {
		t.Fatal(err)
	}
	sepayCfg := providercfg.SepayConfig{BankName: "MB Bank", BankCode: "MB", AccountNo: "111"}
	sepayCreds := providercfg.SepayCredentials{APIToken: "token"}
	payosCfg := providercfg.PayOSConfig{ClientID: "client"}
	payosCreds := providercfg.PayOSCredentials{APIKey: "key", ChecksumKey: "checksum"}

	cases := []struct {
		name string
		in   providercfg.UpsertInput
		want error
	}{
		{"unknown provider", providercfg.UpsertInput{Provider: "stripe", ProductID: prod.ID, Mode: "live", Enabled: true,
			Config: sepayCfg, Credentials: sepayCreds, WebhookSecret: "secret"}, providercfg.ErrProviderUnknown},
		{"sepay missing account_no", providercfg.UpsertInput{Provider: "sepay", ProductID: prod.ID, Mode: "live", Enabled: true,
			Config: providercfg.SepayConfig{BankName: "MB Bank"}, Credentials: sepayCreds, WebhookSecret: "secret"}, providercfg.ErrProviderConfigInvalid},
		{"sepay missing bank", providercfg.UpsertInput{Provider: "sepay", ProductID: prod.ID, Mode: "live", Enabled: true,
			Config: providercfg.SepayConfig{AccountNo: "111"}, Credentials: sepayCreds, WebhookSecret: "secret"}, providercfg.ErrProviderConfigInvalid},
		{"sepay missing api_token", providercfg.UpsertInput{Provider: "sepay", ProductID: prod.ID, Mode: "live", Enabled: true,
			Config: sepayCfg, Credentials: providercfg.SepayCredentials{}, WebhookSecret: "secret"}, providercfg.ErrProviderConfigInvalid},
		{"sepay missing webhook secret", providercfg.UpsertInput{Provider: "sepay", ProductID: prod.ID, Mode: "live", Enabled: true,
			Config: sepayCfg, Credentials: sepayCreds}, providercfg.ErrProviderConfigInvalid},
		{"sepay enabled without config", providercfg.UpsertInput{Provider: "sepay", ProductID: prod.ID, Mode: "live", Enabled: true,
			Credentials: sepayCreds, WebhookSecret: "secret"}, providercfg.ErrProviderConfigInvalid},
		{"payos missing client_id", providercfg.UpsertInput{Provider: "payos", ProductID: prod.ID, Mode: "live", Enabled: true,
			Config: providercfg.PayOSConfig{}, Credentials: payosCreds, WebhookSecret: "secret"}, providercfg.ErrProviderConfigInvalid},
		{"payos missing checksum_key", providercfg.UpsertInput{Provider: "payos", ProductID: prod.ID, Mode: "live", Enabled: true,
			Config: payosCfg, Credentials: providercfg.PayOSCredentials{APIKey: "key"}, WebhookSecret: "secret"}, providercfg.ErrProviderConfigInvalid},
		{"config is not an object", providercfg.UpsertInput{Provider: "sepay", ProductID: prod.ID, Mode: "live", Enabled: true,
			Config: "not-an-object", Credentials: sepayCreds, WebhookSecret: "secret"}, providercfg.ErrProviderConfigInvalid},
		{"credentials are not an object", providercfg.UpsertInput{Provider: "sepay", ProductID: prod.ID, Mode: "live", Enabled: true,
			Config: sepayCfg, Credentials: "not-an-object", WebhookSecret: "secret"}, providercfg.ErrProviderConfigInvalid},
		{"invalid mode", providercfg.UpsertInput{Provider: "sepay", ProductID: prod.ID, Mode: "staging", Enabled: true,
			Config: sepayCfg, Credentials: sepayCreds, WebhookSecret: "secret"}, providercfg.ErrProviderConfigInvalid},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.Upsert(ctx, tt.in)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Upsert = %v, want %v", err, tt.want)
			}
		})
	}

	// Disabled rows may be saved without credentials or bank details:
	// that is how an operator takes a provider out of checkout.
	if _, err := svc.Upsert(ctx, providercfg.UpsertInput{
		Provider: "sepay", ProductID: prod.ID, Mode: "live", Enabled: false,
	}); err != nil {
		t.Fatalf("disabled row without credentials: %v", err)
	}
}

// Precedence: a product override beats the install default; a product
// with no row of its own falls back to the default; a disabled override
// is explicit and never falls back. Modes never bleed into each other.
func TestResolvePrecedenceAndModeIsolation(t *testing.T) {
	s := testdb.Store(t)
	ctx := context.Background()
	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x46}, 32))
	if err != nil {
		t.Fatal(err)
	}
	svc := providercfg.NewService(s, codec, "live")
	prodX := &model.Product{Name: "Precedence X", Slug: "prec-x-" + time.Now().Format("150405.000000"), Type: "desktop"}
	if err := s.CreateProduct(ctx, prodX); err != nil {
		t.Fatal(err)
	}
	prodY := &model.Product{Name: "Precedence Y", Slug: "prec-y-" + time.Now().Format("150405.000000"), Type: "desktop"}
	if err := s.CreateProduct(ctx, prodY); err != nil {
		t.Fatal(err)
	}
	upsert := func(in providercfg.UpsertInput) {
		t.Helper()
		if _, err := svc.Upsert(ctx, in); err != nil {
			t.Fatalf("upsert %s/%s/%s: %v", in.Provider, in.ProductID, in.Mode, err)
		}
	}

	upsert(providercfg.UpsertInput{Provider: "sepay", Mode: "live", Enabled: true,
		Config:        providercfg.SepayConfig{BankName: "Default Bank", BankCode: "MB", AccountNo: "000"},
		Credentials:   providercfg.SepayCredentials{APIToken: "live-default"},
		WebhookSecret: "live-default-secret"})
	upsert(providercfg.UpsertInput{Provider: "sepay", Mode: "live", Enabled: true, ProductID: prodX.ID,
		Config:        providercfg.SepayConfig{BankName: "Override Bank", BankCode: "MB", AccountNo: "111"},
		Credentials:   providercfg.SepayCredentials{APIToken: "live-x"},
		WebhookSecret: "live-x-secret"})

	got, err := svc.Resolve(ctx, prodX.ID, "sepay", "live")
	if err != nil {
		t.Fatalf("resolve product override: %v", err)
	}
	if got.AccountNo != "111" || got.APIToken != "live-x" {
		t.Fatalf("product override lost: %+v", got)
	}
	got, err = svc.Resolve(ctx, prodY.ID, "sepay", "live")
	if err != nil {
		t.Fatalf("resolve fallback: %v", err)
	}
	if got.AccountNo != "000" || got.APIToken != "live-default" {
		t.Fatalf("second product did not fall back to the install default: %+v", got)
	}

	// A disabled product row is a decision, not a gap: it must not let
	// prodY use the install default that is still enabled.
	upsert(providercfg.UpsertInput{Provider: "sepay", Mode: "live", Enabled: false, ProductID: prodY.ID})
	if _, err := svc.Resolve(ctx, prodY.ID, "sepay", "live"); !errors.Is(err, providercfg.ErrProviderDisabled) {
		t.Fatalf("disabled override = %v, want ErrProviderDisabled (no fallback)", err)
	}

	// Test and live rows are separate worlds. The service's configured
	// default mode is used only when the caller omits one.
	upsert(providercfg.UpsertInput{Provider: "sepay", Mode: "test", Enabled: true,
		Config:        providercfg.SepayConfig{BankName: "Test Bank", BankCode: "MB", AccountNo: "999"},
		Credentials:   providercfg.SepayCredentials{APIToken: "test-default"},
		WebhookSecret: "test-default-secret"})
	testSvc := providercfg.NewService(s, codec, "test")

	got, err = testSvc.Resolve(ctx, "", "sepay", "")
	if err != nil || got.Mode != "test" || got.APIToken != "test-default" {
		t.Fatalf(`resolve "" with test default = %+v, %v`, got, err)
	}
	got, err = svc.Resolve(ctx, "", "sepay", "")
	if err != nil || got.Mode != "live" || got.APIToken != "live-default" {
		t.Fatalf(`resolve "" with live default = %+v, %v`, got, err)
	}
	got, err = testSvc.Resolve(ctx, "", "sepay", "live")
	if err != nil || got.Mode != "live" || got.APIToken != "live-default" {
		t.Fatalf("explicit live on test-default service = %+v, %v", got, err)
	}
	got, err = svc.Resolve(ctx, "", "sepay", "test")
	if err != nil || got.Mode != "test" || got.APIToken != "test-default" {
		t.Fatalf("explicit test on live-default service = %+v, %v", got, err)
	}
}
