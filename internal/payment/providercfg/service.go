package providercfg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/store"
)

var (
	// ErrProviderNotConfigured means no row exists for the product and
	// no install-wide default either: checkout must hide the provider.
	ErrProviderNotConfigured = errors.New("payment provider not configured")
	// ErrProviderDisabled means a row exists but was switched off. It
	// is explicit and never falls back to the install default.
	ErrProviderDisabled = errors.New("payment provider disabled")
)

// AAD field names, also the names the admin delete endpoint accepts.
const (
	fieldCredentials   = "credentials"
	fieldWebhookSecret = "webhook_secret"
)

type SepayConfig struct {
	BankName string `json:"bank_name"`
	BankBIN  string `json:"bank_bin"`
	// BankCode is the short code the VietQR image host expects (MB, VCB,
	// …); BankBIN is the full 6-digit code the bank API uses.
	BankCode    string `json:"bank_code"`
	AccountNo   string `json:"account_no"`
	AccountName string `json:"account_name"`
	// WebhookAuthMode is "hmac", "apikey" or "none" — the mode chosen in
	// the SePay dashboard for this webhook.
	WebhookAuthMode string `json:"webhook_auth_mode"`
}

type SepayCredentials struct {
	APIToken  string `json:"api_token"`
	IPNSecret string `json:"ipn_secret"` // hosted-gateway IPN, X-Secret-Key
}

type PayOSConfig struct {
	ClientID string `json:"client_id"`
}

type PayOSCredentials struct {
	APIKey      string `json:"api_key"`
	ChecksumKey string `json:"checksum_key"`
}

// ResolvedConfig is everything an adapter needs, with secrets already
// decrypted. It must never be serialised.
type ResolvedConfig struct {
	Provider  string
	Mode      string
	ProductID string
	Enabled   bool

	Sepay *SepayConfig
	PayOS *PayOSConfig

	// Decrypted credentials.
	APIToken      string // SePay REST token
	IPNSecret     string // SePay gateway IPN (X-Secret-Key)
	ClientID      string // PayOS
	APIKey        string // PayOS
	ChecksumKey   string // PayOS
	WebhookSecret string // SePay bank webhook HMAC secret

	// Static-QR bank details, copied out of SepayConfig for the adapter.
	BankName        string
	BankCode        string
	AccountNo       string
	AccountName     string
	WebhookAuthMode string
}

type UpsertInput struct {
	Provider      string
	ProductID     string
	Mode          string
	Enabled       bool
	Config        any
	Credentials   any // SepayCredentials or PayOSCredentials
	WebhookSecret string
}

type Service struct {
	store *store.Store
	codec *Codec
	mode  string
}

// NewService builds the resolver. mode is only the fallback for a call
// that omits one: the server environment never forces a mode, so a
// self-hosted install can keep both a test and a live row.
func NewService(s *store.Store, c *Codec, mode string) *Service {
	return &Service{store: s, codec: c, mode: mode}
}

func (s *Service) Upsert(ctx context.Context, in UpsertInput) (*model.PaymentProviderConfig, error) {
	provider := strings.ToLower(strings.TrimSpace(in.Provider))
	if provider != "sepay" && provider != "payos" {
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}
	mode := in.Mode
	if mode == "" {
		mode = s.mode
	}
	if mode != "test" && mode != "live" {
		return nil, fmt.Errorf("mode must be test or live")
	}

	existing, err := s.store.FindPaymentProviderConfig(ctx, provider, in.ProductID, mode)
	if err != nil && !errors.Is(err, store.ErrProviderConfigNotFound) {
		return nil, err
	}
	hasSecret := existing != nil && len(existing.WebhookSecretEncrypted) > 0
	if in.Enabled && in.WebhookSecret == "" && !hasSecret {
		return nil, fmt.Errorf("webhook secret is required for an enabled provider")
	}

	// A nil config or credentials means "keep what is stored" for the
	// same reason a blank secret does: an API caller that writes only
	// the enabled flag must not erase the merchant's bank details or
	// credentials. The store defaults a new row's config to {}.
	var cfgJSON []byte
	if in.Config != nil {
		cfgJSON, err = json.Marshal(in.Config)
		if err != nil {
			return nil, err
		}
	}
	var credsJSON []byte
	if in.Credentials != nil {
		credsJSON, err = json.Marshal(in.Credentials)
		if err != nil {
			return nil, err
		}
	}

	// A row may already exist and keep its id (the AAD binds to it), so
	// find it before sealing. A row that appears between that lookup and
	// the insert keeps its own id; the extra attempt below re-seals
	// against it rather than leaving ciphertext bound to a discarded id.
	rowID := store.NewID()
	if existing != nil {
		rowID = existing.ID
	}
	input := store.ProviderConfigInput{
		Provider: provider, ProductID: in.ProductID, Mode: mode, Enabled: in.Enabled, Config: cfgJSON,
	}
	for attempt := 0; attempt < 3; attempt++ {
		if credsJSON != nil {
			input.CredentialsEncrypted, err = s.codec.Seal(rowID, fieldCredentials, credsJSON)
			if err != nil {
				return nil, err
			}
		}
		if in.WebhookSecret != "" {
			input.WebhookSecretEncrypted, err = s.codec.Seal(rowID, fieldWebhookSecret, []byte(in.WebhookSecret))
			if err != nil {
				return nil, err
			}
		}
		saved, err := s.store.UpsertPaymentProviderConfig(ctx, input)
		if err != nil {
			return nil, err
		}
		if saved.ID == rowID {
			return saved, nil
		}
		rowID = saved.ID
	}
	return nil, fmt.Errorf("payment provider config %s/%s/%s was rewritten concurrently", provider, in.ProductID, mode)
}

func (s *Service) Resolve(ctx context.Context, productID, provider, mode string) (*ResolvedConfig, error) {
	if mode == "" {
		mode = s.mode
	}
	row, err := s.find(ctx, productID, provider, mode)
	if err != nil {
		return nil, err
	}
	if !row.Enabled {
		return nil, ErrProviderDisabled
	}
	out := &ResolvedConfig{Provider: row.Provider, Mode: row.Mode, ProductID: row.ProductID, Enabled: true}
	creds, err := s.codec.Open(row.ID, fieldCredentials, row.CredentialsEncrypted)
	if err != nil {
		return nil, fmt.Errorf("decrypt provider credentials: %w", err)
	}
	secret, err := s.codec.Open(row.ID, fieldWebhookSecret, row.WebhookSecretEncrypted)
	if err != nil {
		return nil, fmt.Errorf("decrypt webhook secret: %w", err)
	}
	out.WebhookSecret = string(secret)
	switch row.Provider {
	case "sepay":
		var c SepayConfig
		if err := json.Unmarshal(row.Config, &c); err != nil {
			return nil, err
		}
		var cr SepayCredentials
		if err := json.Unmarshal(creds, &cr); err != nil {
			return nil, err
		}
		out.Sepay = &c
		out.APIToken, out.IPNSecret = cr.APIToken, cr.IPNSecret
		out.BankName, out.BankCode = c.BankName, c.BankCode
		out.AccountNo, out.AccountName = c.AccountNo, c.AccountName
		out.WebhookAuthMode = c.WebhookAuthMode
	case "payos":
		var c PayOSConfig
		if err := json.Unmarshal(row.Config, &c); err != nil {
			return nil, err
		}
		var cr PayOSCredentials
		if err := json.Unmarshal(creds, &cr); err != nil {
			return nil, err
		}
		out.PayOS, out.ClientID, out.APIKey, out.ChecksumKey = &c, c.ClientID, cr.APIKey, cr.ChecksumKey
	default:
		return nil, fmt.Errorf("unsupported provider %q", row.Provider)
	}
	return out, nil
}

// find prefers the product's own row and falls back to the install-wide
// default (product_id NULL). A disabled product row is returned as-is so
// Resolve can report ErrProviderDisabled instead of silently using the
// install default.
func (s *Service) find(ctx context.Context, productID, provider, mode string) (*model.PaymentProviderConfig, error) {
	if productID != "" {
		row, err := s.store.FindPaymentProviderConfig(ctx, provider, productID, mode)
		if err == nil {
			return row, nil
		}
		if !errors.Is(err, store.ErrProviderConfigNotFound) {
			return nil, err
		}
	}
	row, err := s.store.FindPaymentProviderConfig(ctx, provider, "", mode)
	if errors.Is(err, store.ErrProviderConfigNotFound) {
		return nil, ErrProviderNotConfigured
	}
	return row, err
}

func (s *Service) List(ctx context.Context) ([]*model.PaymentProviderConfig, error) {
	return s.store.ListPaymentProviderConfigs(ctx)
}

// DeleteSecrets blanks the named encrypted fields on a row; it neither
// deletes the row nor touches the other field.
func (s *Service) DeleteSecrets(ctx context.Context, id string, fields []string) error {
	var creds, secret bool
	for _, f := range fields {
		switch f {
		case fieldCredentials:
			creds = true
		case fieldWebhookSecret:
			secret = true
		default:
			return fmt.Errorf("unknown secret field %q", f)
		}
	}
	if !creds && !secret {
		return fmt.Errorf("no secret fields named")
	}
	return s.store.ClearProviderSecrets(ctx, id, creds, secret)
}
