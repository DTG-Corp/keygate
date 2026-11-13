package providercfg_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/providercfg"
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
		Config:        providercfg.SepayConfig{BankName: "MB Bank", BankBIN: "970422", AccountNo: "0978482700", AccountName: "NGUYEN VAN A"},
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
		Config:        providercfg.SepayConfig{AccountNo: "111"},
		Credentials:   providercfg.SepayCredentials{APIToken: "keep-me"},
		WebhookSecret: "keep-secret",
	})
	if err != nil {
		t.Fatalf("upsert first: %v", err)
	}
	second, err := svc.Upsert(ctx, providercfg.UpsertInput{
		Provider: "sepay", Mode: "test", Enabled: true, ProductID: prod.ID,
		Config: providercfg.SepayConfig{AccountNo: "222"},
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
