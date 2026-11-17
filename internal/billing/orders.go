package billing

import (
	"context"

	"github.com/tabloy/keygate/internal/model"
)

// keyPrefixLength is how much of a licence key an event may carry: enough
// for support to match a customer, not enough to use the key.
const keyPrefixLength = 8

// keyPrefix is the leading slice of a licence key, for events only.
func keyPrefix(key string) string {
	if len(key) <= keyPrefixLength {
		return key
	}
	return key[:keyPrefixLength]
}

// emitRenewalDue tells the product's webhook endpoint that a renewal
// order exists (spec §8): the licence it belongs to and the order the
// customer pays. It fires once per created order, which is why the
// caller checks OrderResult.Created first.
func (s *Service) emitRenewalDue(ctx context.Context, lic *model.License, order *model.PaymentOrder) {
	s.logger.Info("renewal order created",
		"license_id", lic.ID, "order_code", order.Code, "period_end", order.PeriodEnd)
	if s.deps.Webhook == nil {
		return
	}
	s.deps.Webhook.Dispatch(ctx, lic.ProductID, model.EventLicenseRenewalDue, map[string]any{
		"license": map[string]any{
			"id":          lic.ID,
			"key_prefix":  keyPrefix(lic.LicenseKey),
			"valid_until": lic.ValidUntil,
		},
		"order": map[string]any{
			"code":       order.Code,
			"total":      order.TotalMinor,
			"expires_at": order.ExpiresAt,
		},
	})
}
