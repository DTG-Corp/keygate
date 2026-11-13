package sepay

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tabloy/keygate/internal/payment/provider"
)

// Config is one resolved provider config row. Secrets arrive decrypted
// from providercfg.ResolvedConfig and are never serialised.
type Config struct {
	Sandbox         bool
	WebhookAuthMode string // hmac | apikey | none
	APIKey          string
	IPNSecret       string
	WebhookSecret   string
	APIToken        string
	Bank            provider.BankAccount
	BankCode        string // short bank code used by the QR image host (MB, VCB, …)
}

// Provider is the SePay adapter: static QR creation, bank-webhook
// verification, hosted-gateway IPN verification and the reconciliation
// query client.
type Provider struct {
	cfg    Config
	client *Client
	now    func() time.Time
}

// New builds a Provider from one resolved config row. It performs no
// network I/O; TestConnection is the explicit credential check.
func New(cfg Config) *Provider {
	base := "https://userapi.sepay.vn/v2"
	if cfg.Sandbox {
		base = "https://userapi-sandbox.sepay.vn/v2"
	}
	return &Provider{cfg: cfg, client: &Client{BaseURL: base, APIToken: cfg.APIToken}, now: time.Now}
}

func (p *Provider) Name() string { return "sepay" }

// CreateAttempt produces the static QR instruction. No provider call is
// made: the bank webhook is the settlement source.
func (p *Provider) CreateAttempt(_ context.Context, req provider.CreateAttemptRequest) (provider.CreateAttemptResult, error) {
	desc := req.OrderCode
	return provider.CreateAttemptResult{
		ProviderRef: req.OrderCode,
		QRPayload:   BuildQRImageURL(p.cfg.Bank.AccountNo, p.cfg.BankCode, req.AmountMinor, desc),
		QRImageURL:  BuildQRImageURL(p.cfg.Bank.AccountNo, p.cfg.BankCode, req.AmountMinor, desc),
		Bank:        p.cfg.Bank,
		Description: desc,
		ExpiresAt:   req.ExpiresAt,
	}, nil
}

// CancelAttempt is a no-op: a bank transfer cannot be cancelled, the
// order simply expires.
func (p *Provider) CancelAttempt(context.Context, string) error { return nil }

func (p *Provider) ParseWebhook(r *http.Request, body []byte, secret []byte) (provider.WebhookEvent, error) {
	if len(secret) == 0 && p.cfg.WebhookSecret != "" {
		secret = []byte(p.cfg.WebhookSecret)
	}
	return ParseBankWebhook(r.Header, body, BankWebhookConfig{
		Mode: p.cfg.WebhookAuthMode, Secret: string(secret), APIKey: p.cfg.APIKey, Now: p.now(),
	})
}

func (p *Provider) TestConnection(ctx context.Context) error { return p.client.TestConnection(ctx) }

// ParseIPN and ListTransactions are used by the webhook route and the
// reconciler directly; they are not part of the Provider interface.
func (p *Provider) ParseIPN(r *http.Request, body []byte) (provider.WebhookEvent, error) {
	return ParseIPN(r.Header, body, p.cfg.IPNSecret)
}

func (p *Provider) ListTransactions(ctx context.Context, from, to time.Time, sinceID int64) ([]BankTransaction, error) {
	return p.client.ListTransactions(ctx, from, to, sinceID)
}

var _ provider.Provider = (*Provider)(nil)

// ErrMissingAPIKey keeps a bad config from surfacing as an opaque 401.
var ErrMissingAPIKey = errors.New("sepay: api token is not configured")
