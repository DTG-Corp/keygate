package payos

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/dtg-corp/dsoft/internal/payment/provider"
)

// maxLinkExpiry is PayOS's ceiling for expiredAt, a signed 32-bit epoch
// (2038-01-19T00:00:00Z is below the int32 limit of 2038-01-19T03:14:07Z).
var maxLinkExpiry = time.Date(2038, 1, 19, 0, 0, 0, 0, time.UTC)

// Config is one resolved PayOS config row with its credentials already
// decrypted. PayOS signs webhooks with the checksum key, so
// WebhookSecret is not used by this adapter; it stays in the struct so
// the admin config shape is the same for every rail.
type Config struct {
	ClientID      string
	APIKey        string
	ChecksumKey   string
	WebhookSecret string
	BaseURL       string // test seam; empty means production
}

// Provider is the PayOS adapter. It holds one client built from the
// resolved config; there is no per-call credential path, matching the
// provider.Provider contract.
type Provider struct{ client *Client }

func New(cfg Config) *Provider {
	return &Provider{client: &Client{
		BaseURL: cfg.BaseURL, ClientID: cfg.ClientID,
		APIKey: cfg.APIKey, ChecksumKey: cfg.ChecksumKey,
	}}
}

func (p *Provider) Name() string { return "payos" }

// CreateAttempt creates a hosted payment link for the order. PayOS needs
// the numeric orderCode (the KG code travels in the 8-character
// description), and its expiry must fit in 32 bits, so a longer-lived
// order is clamped rather than rejected by the API.
func (p *Provider) CreateAttempt(ctx context.Context, req provider.CreateAttemptRequest) (provider.CreateAttemptResult, error) {
	if err := p.configured(); err != nil {
		return provider.CreateAttemptResult{}, err
	}
	expiresAt := req.ExpiresAt
	if expiresAt.After(maxLinkExpiry) {
		expiresAt = maxLinkExpiry
	}
	var expiredAt int64
	if !expiresAt.IsZero() {
		expiredAt = expiresAt.Unix()
	}
	link, err := p.client.CreatePaymentLink(ctx, CreateRequest{
		OrderCode: req.NumericCode, Amount: req.AmountMinor, Description: req.Description,
		BuyerName: req.Buyer.Name, BuyerEmail: req.Buyer.Email, BuyerPhone: req.Buyer.Phone,
		BuyerAddress: req.Buyer.Address, BuyerTaxID: req.Buyer.TaxID,
		CancelURL: req.CancelURL, ReturnURL: req.ReturnURL, ExpiredAt: expiredAt,
	})
	if err != nil {
		return provider.CreateAttemptResult{}, err
	}
	return provider.CreateAttemptResult{
		ProviderRef: link.PaymentLinkID, CheckoutURL: link.CheckoutURL,
		QRPayload: link.QRCode, ExpiresAt: expiresAt,
	}, nil
}

// CancelAttempt is a no-op in phase 1a: the link expires with the order
// (expiredAt), and cancelling by paymentLinkId is not part of PayOS's
// API — the call needs the numeric orderCode, which the order row owns.
func (p *Provider) CancelAttempt(context.Context, string) error { return nil }

// ParseWebhook verifies PayOS's checksum over the raw body. The caller's
// secret is ignored: PayOS signs with the checksum key, not a separately
// registered webhook secret, and the key comes from the resolved config.
func (p *Provider) ParseWebhook(r *http.Request, body []byte, _ []byte) (provider.WebhookEvent, error) {
	if err := p.configured(); err != nil {
		return provider.WebhookEvent{}, err
	}
	var header http.Header
	if r != nil {
		header = r.Header
	}
	return ParseWebhook(header, body, p.client.ChecksumKey)
}

func (p *Provider) TestConnection(ctx context.Context) error {
	if err := p.configured(); err != nil {
		return err
	}
	return p.client.TestConnection(ctx)
}

// ConfirmWebhook registers this install's webhook URL with PayOS. It is
// an explicit admin action: PayOS probes the URL immediately.
func (p *Provider) ConfirmWebhook(ctx context.Context, url string) error {
	if err := p.configured(); err != nil {
		return err
	}
	return p.client.ConfirmWebhook(ctx, url)
}

// configured keeps a missing credential from surfacing as an opaque 401
// or, worse, as a signature computed with an empty key.
func (p *Provider) configured() error {
	if p.client.ClientID == "" || p.client.APIKey == "" || p.client.ChecksumKey == "" {
		return ErrNotConfigured
	}
	return nil
}

var _ provider.Provider = (*Provider)(nil)

// ErrNotConfigured reports a Provider built without the merchant
// channel's client id, API key or checksum key.
var ErrNotConfigured = errors.New("payos: credentials are not configured")
