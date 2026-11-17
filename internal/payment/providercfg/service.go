package providercfg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/store"
)

var (
	// ErrProviderNotConfigured means no row exists for the product and
	// no install-wide default either: checkout must hide the provider.
	ErrProviderNotConfigured = errors.New("payment provider not configured")
	// ErrProviderDisabled means a row exists but was switched off. It
	// is explicit and never falls back to the install default.
	ErrProviderDisabled = errors.New("payment provider disabled")
	// ErrProviderUnknown means the name is not one of the providers this
	// install supports. The admin API maps it to PAYMENT_PROVIDER_UNKNOWN.
	ErrProviderUnknown = errors.New("unknown payment provider")
	// ErrProviderConfigInvalid means the row cannot be used: its shapes
	// are wrong, an enabled row is missing required fields, or its stored
	// ciphertext cannot be decrypted. It is a 400 on the admin API and
	// unusable at checkout.
	ErrProviderConfigInvalid = errors.New("invalid payment provider configuration")
	// ErrProviderNotFound means the row a caller named does not exist.
	ErrProviderNotFound = errors.New("payment provider config not found")
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
// decrypted. It must never be serialised: every credential field is
// excluded from JSON and MarshalJSON fails closed, so an accidental
// response.OK(c, cfg) or log line is an error, not a leak.
type ResolvedConfig struct {
	Provider  string `json:"-"`
	Mode      string `json:"-"`
	ProductID string `json:"-"`
	Enabled   bool   `json:"-"`

	Sepay *SepayConfig `json:"-"`
	PayOS *PayOSConfig `json:"-"`

	// Decrypted credentials.
	APIToken      string `json:"-"` // SePay REST token
	IPNSecret     string `json:"-"` // SePay gateway IPN (X-Secret-Key)
	ClientID      string `json:"-"` // PayOS
	APIKey        string `json:"-"` // PayOS; SePay webhook apikey mode
	ChecksumKey   string `json:"-"` // PayOS
	WebhookSecret string `json:"-"` // SePay bank webhook HMAC secret

	// Static-QR bank details, copied out of SepayConfig for the adapter.
	BankName        string `json:"-"`
	BankCode        string `json:"-"`
	AccountNo       string `json:"-"`
	AccountName     string `json:"-"`
	WebhookAuthMode string `json:"-"`
}

// MarshalJSON refuses to encode a resolved config. Returning an error
// rather than an empty object is deliberate: an empty value would let a
// caller believe the response was scrubbed, while the caller here is
// almost always a bug that must be visible.
func (ResolvedConfig) MarshalJSON() ([]byte, error) {
	return nil, errors.New("providercfg.ResolvedConfig carries decrypted secrets and must not be serialised")
}

type UpsertInput struct {
	Provider  string
	ProductID string
	// Mode empty uses the service's configured default (PAYMENT_MODE).
	Mode    string
	Enabled bool
	// Config and Credentials may be nil, meaning "keep the stored value".
	Config      any
	Credentials any // SepayCredentials or PayOSCredentials
	// WebhookSecret empty keeps the stored secret.
	WebhookSecret string
}

type Service struct {
	store *store.Store
	codec *Codec
	mode  string
}

// NewService builds the resolver. mode is the default for a Resolve or
// Upsert call that omits one (PAYMENT_MODE at the server); an explicit
// mode always wins, so a self-hosted install can keep both a test and a
// live row. Resolve(ctx, productID, provider, "") uses this default.
func NewService(s *store.Store, c *Codec, mode string) *Service {
	return &Service{store: s, codec: c, mode: mode}
}

// Stored returns the exact row an upsert of this slot would update, or
// nil when the slot is empty. The admin handler uses it to keep an
// omitted `enabled`; unlike find it never falls back to the install
// default, because writing an override must not copy the default's
// enabled state into a new product row.
func (s *Service) Stored(ctx context.Context, providerName, productID, mode string) (*model.PaymentProviderConfig, error) {
	provider, err := normalizeProvider(providerName)
	if err != nil {
		return nil, err
	}
	mode, err = s.modeOrDefault(mode)
	if err != nil {
		return nil, err
	}
	row, err := s.store.FindPaymentProviderConfig(ctx, provider, productID, mode)
	if errors.Is(err, store.ErrProviderConfigNotFound) {
		return nil, nil
	}
	return row, err
}

func (s *Service) Upsert(ctx context.Context, in UpsertInput) (*model.PaymentProviderConfig, error) {
	provider, err := normalizeProvider(in.Provider)
	if err != nil {
		return nil, err
	}
	mode, err := s.modeOrDefault(in.Mode)
	if err != nil {
		return nil, err
	}

	existing, err := s.store.FindPaymentProviderConfig(ctx, provider, in.ProductID, mode)
	if err != nil && !errors.Is(err, store.ErrProviderConfigNotFound) {
		return nil, err
	}

	// A nil config or credentials means "keep what is stored" for the
	// same reason a blank secret does: an API caller that writes only
	// the enabled flag must not erase the merchant's bank details or
	// credentials. A provided value is validated here, before any write:
	// the admin API must not be able to store a row it could not resolve.
	var cfgJSON []byte
	if in.Config != nil {
		cfgJSON, err = marshalProviderObject(in.Config, "config")
		if err != nil {
			return nil, err
		}
		if err := validateConfig(provider, cfgJSON, in.Enabled, mode); err != nil {
			return nil, err
		}
	} else if in.Enabled {
		// An enable-only save must still be complete: with no config
		// supplied the effective config is the stored one, or the empty
		// object a new row would get. Neither is usable for a provider
		// that needs bank details or a client id.
		effective := []byte(`{}`)
		if existing != nil {
			effective = existing.Config
		}
		if err := validateConfig(provider, effective, true, mode); err != nil {
			return nil, err
		}
	}

	var credsJSON []byte
	switch {
	case in.Credentials != nil:
		credsJSON, err = marshalProviderObject(in.Credentials, "credentials")
		if err != nil {
			return nil, err
		}
		if err := validateCredentials(provider, credsJSON, in.Enabled); err != nil {
			return nil, err
		}
	case existing != nil && len(existing.CredentialsEncrypted) > 0:
		// Omitted credentials are decrypted only to prove the enabled
		// row is still usable; corrupt stored bytes must not survive an
		// enable save as a time bomb.
		plain, err := s.codec.Open(existing.ID, fieldCredentials, existing.CredentialsEncrypted)
		if err != nil {
			return nil, fmt.Errorf("%w: stored credentials cannot be decrypted", ErrProviderConfigInvalid)
		}
		if in.Enabled {
			if err := validateCredentials(provider, plain, true); err != nil {
				return nil, err
			}
		}
	}

	hasSecret := in.WebhookSecret != "" || (existing != nil && len(existing.WebhookSecretEncrypted) > 0)
	if in.Enabled {
		// PayOS authenticates its webhook with checksum_key, not the
		// webhook secret, so requiring one would force operators to
		// store a meaningless value. SePay still needs its secret for
		// HMAC mode.
		if requiresWebhookSecret(provider) && !hasSecret {
			return nil, fmt.Errorf("%w: webhook secret is required for an enabled provider", ErrProviderConfigInvalid)
		}
		if credsJSON == nil && (existing == nil || len(existing.CredentialsEncrypted) == 0) {
			return nil, fmt.Errorf("%w: credentials are required for an enabled provider", ErrProviderConfigInvalid)
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
		input.ID = rowID
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
	// Admin rows are stored normalised; a checkout caller that repeats
	// the name with stray case or spaces must still find them.
	provider = strings.ToLower(strings.TrimSpace(provider))
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
	// An enabled row without usable secret material is a broken
	// configuration. It must not surface as ErrProviderDisabled (which
	// means "the operator turned this off") and must not leak the raw
	// AES-GCM error ("ciphertext too short") either.
	if len(row.CredentialsEncrypted) == 0 {
		return nil, fmt.Errorf("%w: enabled provider has no stored credentials", ErrProviderConfigInvalid)
	}
	if requiresWebhookSecret(row.Provider) && len(row.WebhookSecretEncrypted) == 0 {
		return nil, fmt.Errorf("%w: enabled provider has no stored webhook secret", ErrProviderConfigInvalid)
	}
	creds, err := s.codec.Open(row.ID, fieldCredentials, row.CredentialsEncrypted)
	if err != nil {
		return nil, fmt.Errorf("%w: stored credentials cannot be decrypted", ErrProviderConfigInvalid)
	}
	var secret []byte
	if len(row.WebhookSecretEncrypted) > 0 {
		secret, err = s.codec.Open(row.ID, fieldWebhookSecret, row.WebhookSecretEncrypted)
		if err != nil {
			return nil, fmt.Errorf("%w: stored webhook secret cannot be decrypted", ErrProviderConfigInvalid)
		}
	}
	if err := validateConfig(row.Provider, row.Config, true, row.Mode); err != nil {
		return nil, err
	}
	if err := validateCredentials(row.Provider, creds, true); err != nil {
		return nil, err
	}
	out := &ResolvedConfig{Provider: row.Provider, Mode: row.Mode, ProductID: row.ProductID, Enabled: true}
	out.WebhookSecret = string(secret)
	switch row.Provider {
	case "sepay":
		var c SepayConfig
		if err := json.Unmarshal(row.Config, &c); err != nil {
			return nil, fmt.Errorf("%w: sepay config is not a JSON object", ErrProviderConfigInvalid)
		}
		var cr SepayCredentials
		if err := json.Unmarshal(creds, &cr); err != nil {
			return nil, fmt.Errorf("%w: sepay credentials are not a JSON object", ErrProviderConfigInvalid)
		}
		out.Sepay = &c
		out.APIToken, out.IPNSecret = cr.APIToken, cr.IPNSecret
		// SePay's "apikey" webhook mode authenticates with the same
		// token the REST client uses.
		out.APIKey = cr.APIToken
		out.BankName, out.BankCode = c.BankName, c.BankCode
		out.AccountNo, out.AccountName = c.AccountNo, c.AccountName
		out.WebhookAuthMode = c.WebhookAuthMode
	case "payos":
		var c PayOSConfig
		if err := json.Unmarshal(row.Config, &c); err != nil {
			return nil, fmt.Errorf("%w: payos config is not a JSON object", ErrProviderConfigInvalid)
		}
		var cr PayOSCredentials
		if err := json.Unmarshal(creds, &cr); err != nil {
			return nil, fmt.Errorf("%w: payos credentials are not a JSON object", ErrProviderConfigInvalid)
		}
		out.PayOS, out.ClientID, out.APIKey, out.ChecksumKey = &c, c.ClientID, cr.APIKey, cr.ChecksumKey
	default:
		return nil, fmt.Errorf("%w: %q", ErrProviderUnknown, row.Provider)
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
// deletes the row nor touches the other field. A missing row is
// ErrProviderNotFound so the admin API can answer 404 instead of
// pretending it cleared something.
func (s *Service) DeleteSecrets(ctx context.Context, id string, fields []string) error {
	var creds, secret bool
	for _, f := range fields {
		switch f {
		case fieldCredentials:
			creds = true
		case fieldWebhookSecret:
			secret = true
		default:
			return fmt.Errorf("%w: unknown secret field %q", ErrProviderConfigInvalid, f)
		}
	}
	if !creds && !secret {
		return fmt.Errorf("%w: no secret fields named", ErrProviderConfigInvalid)
	}
	n, err := s.store.ClearProviderSecrets(ctx, id, creds, secret)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrProviderNotFound
	}
	return nil
}

func (s *Service) modeOrDefault(mode string) (string, error) {
	if mode == "" {
		mode = s.mode
	}
	switch mode {
	case "test", "live":
		return mode, nil
	default:
		return "", fmt.Errorf("%w: mode must be test or live", ErrProviderConfigInvalid)
	}
}

func normalizeProvider(raw string) (string, error) {
	switch provider := strings.ToLower(strings.TrimSpace(raw)); provider {
	case "sepay", "payos":
		return provider, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrProviderUnknown, provider)
	}
}

// requiresWebhookSecret reports whether a provider's webhook is verified
// with the row's webhook_secret. SePay's HMAC mode uses it; PayOS signs
// deliveries with checksum_key and never reads this field, so an enabled
// PayOS row may legitimately have none.
func requiresWebhookSecret(provider string) bool {
	return provider == "sepay"
}

// marshalProviderObject serialises one admin-supplied blob and
// canonicalises it (compact JSON, object root) before it is validated or
// sealed. A string or array is not a provider config.
func marshalProviderObject(v any, what string) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not valid JSON", ErrProviderConfigInvalid, what)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, fmt.Errorf("%w: %s is not valid JSON", ErrProviderConfigInvalid, what)
	}
	if trimmed := bytes.TrimSpace(buf.Bytes()); len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("%w: %s must be a JSON object", ErrProviderConfigInvalid, what)
	}
	return buf.Bytes(), nil
}

func validateConfig(provider string, raw []byte, requireComplete bool, mode string) error {
	switch provider {
	case "sepay":
		var c SepayConfig
		if err := json.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("%w: sepay config must be a JSON object", ErrProviderConfigInvalid)
		}
		if !requireComplete {
			return nil
		}
		if strings.TrimSpace(c.AccountNo) == "" {
			return fmt.Errorf("%w: sepay config.account_no is required for an enabled provider", ErrProviderConfigInvalid)
		}
		// The adapter builds the static QR from the short bank code; a row
		// without it would render "?bank=&amount=…" and no banking app
		// could pay the transfer.
		if strings.TrimSpace(c.BankCode) == "" {
			return fmt.Errorf("%w: sepay config.bank_code is required for an enabled provider", ErrProviderConfigInvalid)
		}
		if c.BankName == "" && c.BankBIN == "" {
			return fmt.Errorf("%w: sepay config needs a bank name or BIN for an enabled provider", ErrProviderConfigInvalid)
		}

		// mode=none accepts unsigned deliveries. That is a legitimate
		// dashboard test setting, but fail-open in live mode, so refuse the
		// combination at write time as well as at webhook time (T8).
		if mode == "live" && strings.EqualFold(strings.TrimSpace(c.WebhookAuthMode), "none") {
			return fmt.Errorf("%w: sepay webhook_auth_mode must not be none in live mode", ErrProviderConfigInvalid)
		}
	case "payos":
		var c PayOSConfig
		if err := json.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("%w: payos config must be a JSON object", ErrProviderConfigInvalid)
		}
		if requireComplete && strings.TrimSpace(c.ClientID) == "" {
			return fmt.Errorf("%w: payos config.client_id is required for an enabled provider", ErrProviderConfigInvalid)
		}
	default:
		return fmt.Errorf("%w: %q", ErrProviderUnknown, provider)
	}
	return nil
}

func validateCredentials(provider string, raw []byte, requireComplete bool) error {
	switch provider {
	case "sepay":
		var c SepayCredentials
		if err := json.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("%w: sepay credentials must be a JSON object", ErrProviderConfigInvalid)
		}
		if requireComplete && strings.TrimSpace(c.APIToken) == "" {
			return fmt.Errorf("%w: sepay credentials.api_token is required for an enabled provider", ErrProviderConfigInvalid)
		}
	case "payos":
		var c PayOSCredentials
		if err := json.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("%w: payos credentials must be a JSON object", ErrProviderConfigInvalid)
		}
		if requireComplete && (strings.TrimSpace(c.APIKey) == "" || strings.TrimSpace(c.ChecksumKey) == "") {
			return fmt.Errorf("%w: payos credentials.api_key and checksum_key are required for an enabled provider", ErrProviderConfigInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown provider %q", ErrProviderUnknown, provider)
	}
	return nil
}
