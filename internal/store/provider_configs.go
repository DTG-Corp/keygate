package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// ErrProviderConfigNotFound is returned when no row matches a provider
// slot. It is distinct from a database failure so resolution can fall
// back to the install default and only then report "not configured".
var ErrProviderConfigNotFound = errors.New("payment provider config not found")

// ProviderConfigInput is one write to a (provider, product, mode) slot.
// The encrypted fields may be nil: the write then leaves whatever is
// already stored alone (a blank secret in the admin form means "keep
// the current one"), and a new row starts with the empty bytea default.
type ProviderConfigInput struct {
	// ID is the primary key the caller sealed the encrypted fields
	// against. The AAD binds each ciphertext to this id, so a create must
	// insert under it; an empty ID generates a fresh one.
	ID                     string
	Provider               string
	ProductID              string
	Mode                   string
	Enabled                bool
	Config                 []byte // JSONB
	CredentialsEncrypted   []byte
	WebhookSecretEncrypted []byte
}

// UpsertPaymentProviderConfig writes one (provider, product, mode) row.
// Updating an existing row keeps its id so the encrypted bytes stay
// bound to the same AAD.
func (s *Store) UpsertPaymentProviderConfig(ctx context.Context, in ProviderConfigInput) (*model.PaymentProviderConfig, error) {
	existing := new(model.PaymentProviderConfig)
	err := s.DB.NewSelect().Model(existing).
		Where("provider = ?", in.Provider).
		Where("product_id IS NOT DISTINCT FROM ?", nullable(in.ProductID)).
		Where("mode = ?", in.Mode).Limit(1).Scan(ctx)
	var row *model.PaymentProviderConfig
	switch {
	case err == nil:
		row = existing
	case errors.Is(err, sql.ErrNoRows):
		id := in.ID
		if id == "" {
			id = NewID()
		}
		row = &model.PaymentProviderConfig{ID: id, Provider: in.Provider, ProductID: in.ProductID, Mode: in.Mode, CreatedAt: time.Now()}
	default:
		return nil, err
	}
	row.Enabled = in.Enabled
	if len(in.Config) > 0 {
		row.Config = in.Config
	} else if len(row.Config) == 0 {
		// config is NOT NULL, and a nil json.RawMessage would be
		// written as an explicit NULL rather than the column default.
		row.Config = []byte("{}")
	}
	if in.CredentialsEncrypted != nil {
		row.CredentialsEncrypted = in.CredentialsEncrypted
	}
	if in.WebhookSecretEncrypted != nil {
		row.WebhookSecretEncrypted = in.WebhookSecretEncrypted
	}
	row.UpdatedAt = time.Now()
	// Return the stored row, not the struct we built: a concurrent
	// first write can win the slot, and then the id the caller sealed
	// against is not the one the row carries.
	err = s.DB.NewInsert().Model(row).On("CONFLICT (provider, product_id, mode) DO UPDATE").
		Set("enabled = EXCLUDED.enabled").
		Set("config = EXCLUDED.config").
		// Only write ciphertext the caller sealed for this row. When a
		// concurrent insert wins the slot between the lookup above and
		// this statement, EXCLUDED carries a different id and its
		// ciphertext is bound to that discarded id: keep the winner's
		// bytes and let the caller re-seal against the returned row.
		Set(`credentials_encrypted = CASE WHEN "payment_provider_config".id = EXCLUDED.id THEN EXCLUDED.credentials_encrypted ELSE "payment_provider_config".credentials_encrypted END`).
		Set(`webhook_secret_encrypted = CASE WHEN "payment_provider_config".id = EXCLUDED.id THEN EXCLUDED.webhook_secret_encrypted ELSE "payment_provider_config".webhook_secret_encrypted END`).
		Set("updated_at = now()").
		Returning("*").Scan(ctx)
	if err != nil {
		return nil, err
	}
	return row, nil
}

// nullable maps the zero string to SQL NULL: product_id NULL is the
// install-wide default row.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) ListPaymentProviderConfigs(ctx context.Context) ([]*model.PaymentProviderConfig, error) {
	var out []*model.PaymentProviderConfig
	err := s.DB.NewSelect().Model(&out).Order("provider ASC", "product_id ASC NULLS FIRST", "mode ASC").Scan(ctx)
	return out, err
}

func (s *Store) FindPaymentProviderConfig(ctx context.Context, provider, productID, mode string) (*model.PaymentProviderConfig, error) {
	row := new(model.PaymentProviderConfig)
	err := s.DB.NewSelect().Model(row).
		Where("provider = ?", provider).
		Where("product_id IS NOT DISTINCT FROM ?", nullable(productID)).
		Where("mode = ?", mode).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProviderConfigNotFound
	}
	return row, err
}

// ClearProviderSecrets blanks the named encrypted columns and reports
// how many rows were updated. The row and its id stay: the AAD of
// anything else stored on it is unaffected. A zero count means no row
// carried that id, which the API turns into a 404.
func (s *Store) ClearProviderSecrets(ctx context.Context, id string, credentials, webhookSecret bool) (int64, error) {
	q := s.DB.NewUpdate().Model((*model.PaymentProviderConfig)(nil)).Set("updated_at = now()").Where("id = ?", id)
	if credentials {
		q = q.Set("credentials_encrypted = ''::bytea")
	}
	if webhookSecret {
		q = q.Set("webhook_secret_encrypted = ''::bytea")
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
