// Package fake provides a scripted in-memory provider.Provider for
// handler and settlement tests.
package fake

import (
	"context"
	"net/http"

	"github.com/tabloy/keygate/internal/payment/provider"
)

// Provider is a scripted adapter for tests. Zero value works: every
// call succeeds with a synthetic ref.
type Provider struct {
	name      string
	CreateFn  func(context.Context, provider.CreateAttemptRequest) (provider.CreateAttemptResult, error)
	CancelErr error
	WebhookFn func(*http.Request, []byte, []byte) (provider.WebhookEvent, error)
	TestErr   error
	Calls     []provider.CreateAttemptRequest
	Cancels   []string
}

// New returns a fake adapter that reports the given provider name.
func New(name string) *Provider { return &Provider{name: name} }

func (p *Provider) Name() string { return p.name }

func (p *Provider) CreateAttempt(ctx context.Context, req provider.CreateAttemptRequest) (provider.CreateAttemptResult, error) {
	p.Calls = append(p.Calls, req)
	if p.CreateFn != nil {
		return p.CreateFn(ctx, req)
	}
	return provider.CreateAttemptResult{ProviderRef: "fake-ref-" + req.OrderCode, QRPayload: "fake-qr", ExpiresAt: req.ExpiresAt}, nil
}

func (p *Provider) CancelAttempt(_ context.Context, ref string) error {
	p.Cancels = append(p.Cancels, ref)
	return p.CancelErr
}

func (p *Provider) ParseWebhook(r *http.Request, raw []byte, secret []byte) (provider.WebhookEvent, error) {
	if p.WebhookFn != nil {
		return p.WebhookFn(r, raw, secret)
	}
	return provider.WebhookEvent{}, nil
}

func (p *Provider) TestConnection(context.Context) error { return p.TestErr }

var _ provider.Provider = (*Provider)(nil)
