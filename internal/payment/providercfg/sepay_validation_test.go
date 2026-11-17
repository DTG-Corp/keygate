package providercfg_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/providercfg"
	"github.com/tabloy/keygate/internal/testsupport/testdb"
)

// newSepayValidator returns a service plus a fresh product, so every slot
// (provider, product, mode) is unique: the integration database persists
// between tests and an install-wide row would be sealed with another
// test's codec.
func newSepayValidator(t *testing.T) (*providercfg.Service, *model.Product) {
	t.Helper()
	s := testdb.Store(t)
	codec, err := providercfg.NewCodec(bytes.Repeat([]byte{0x51}, 32))
	if err != nil {
		t.Fatal(err)
	}
	prod := &model.Product{Name: "Sepay validation", Slug: "sepay-validation-" + time.Now().Format("150405.000000"), Type: "desktop"}
	if err := s.CreateProduct(context.Background(), prod); err != nil {
		t.Fatal(err)
	}
	return providercfg.NewService(s, codec, "live"), prod
}

// The static QR is built from the short bank code and the account number:
// an enabled row without bank_code renders "?bank=&amount=…", a QR no
// banking app can pay. Refuse it at write time in both modes.
func TestUpsertRequiresSepayBankCode(t *testing.T) {
	svc, prod := newSepayValidator(t)
	ctx := context.Background()

	for _, mode := range []string{"live", "test"} {
		_, err := svc.Upsert(ctx, providercfg.UpsertInput{
			Provider: "sepay", ProductID: prod.ID, Mode: mode, Enabled: true,
			Config:        providercfg.SepayConfig{BankName: "MB Bank", AccountNo: "0978482700"},
			Credentials:   providercfg.SepayCredentials{APIToken: "token"},
			WebhookSecret: "secret",
		})
		if !errors.Is(err, providercfg.ErrProviderConfigInvalid) {
			t.Fatalf("%s row without bank_code: err = %v, want ErrProviderConfigInvalid", mode, err)
		}
	}

	// A disabled row may stay incomplete: that is how an operator parks a
	// provider without erasing the bank details.
	if _, err := svc.Upsert(ctx, providercfg.UpsertInput{
		Provider: "sepay", ProductID: prod.ID, Mode: "live", Enabled: false,
	}); err != nil {
		t.Fatalf("disabled row: %v", err)
	}
}

// webhook_auth_mode=none means unsigned deliveries. It is legitimate for a
// test-mode row (the dashboard test webhook) and fail-open in live mode, so
// it is refused at write time as well as by the runtime guard.
func TestUpsertRejectsNoneWebhookAuthInLiveMode(t *testing.T) {
	svc, prod := newSepayValidator(t)
	ctx := context.Background()
	cfg := func(mode string) providercfg.SepayConfig {
		return providercfg.SepayConfig{
			BankName: "MB Bank", BankCode: "MB", AccountNo: "0978482700",
			WebhookAuthMode: mode,
		}
	}
	creds := providercfg.SepayCredentials{APIToken: "token"}

	if _, err := svc.Upsert(ctx, providercfg.UpsertInput{
		Provider: "sepay", ProductID: prod.ID, Mode: "live", Enabled: true,
		Config: cfg("none"), Credentials: creds, WebhookSecret: "secret",
	}); !errors.Is(err, providercfg.ErrProviderConfigInvalid) {
		t.Fatalf("live none: err = %v, want ErrProviderConfigInvalid", err)
	}

	// Test-mode "none" remains allowed.
	if _, err := svc.Upsert(ctx, providercfg.UpsertInput{
		Provider: "sepay", ProductID: prod.ID, Mode: "test", Enabled: true,
		Config: cfg("none"), Credentials: creds, WebhookSecret: "secret",
	}); err != nil {
		t.Fatalf("test-mode none rejected: %v", err)
	}

	// A signed mode is still accepted in live mode.
	if _, err := svc.Upsert(ctx, providercfg.UpsertInput{
		Provider: "sepay", ProductID: prod.ID, Mode: "live", Enabled: true,
		Config: cfg("hmac"), Credentials: creds, WebhookSecret: "secret",
	}); err != nil {
		t.Fatalf("live hmac rejected: %v", err)
	}
}
