package checkout

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/payment/provider"
	"github.com/tabloy/keygate/internal/store"
)

// ReissueAttempt returns a payment instruction for an existing pending
// order: the still-payable one when there is one, or a new attempt from
// the order's provider when the last instruction failed or expired.
//
// It is the portal's "Pay" action. The order, its price and its code are
// already fixed, so this never re-prices and never creates a second
// order; a paid, canceled, refunded or expiry-passed order is refused
// before the provider is called. The caller owns the order already (the
// portal looks it up under the customer predicate first); the id is
// re-read here so a stale page cannot mint an instruction for an order
// that settled in the meantime.
func (s *Service) ReissueAttempt(ctx context.Context, order *model.PaymentOrder) (*model.PaymentAttempt, error) {
	if order == nil {
		return nil, store.ErrOrderNotFound
	}
	fresh, err := s.Store.GetPaymentOrderByID(ctx, order.ID)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if fresh.Status != domain.OrderPending || !fresh.ExpiresAt.After(now) {
		return nil, fmt.Errorf("%w: order is not pending and unexpired", store.ErrOrderNotPayable)
	}

	latest, err := s.Store.GetLatestPaymentAttempt(ctx, fresh.ID)
	switch {
	case err == nil && payableAttempt(latest, now):
		return latest, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}

	prov, err := s.adapter(ctx, fresh.ProductID, fresh.Provider)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrProviderUnavailable, err)
	}
	buyer := provider.Buyer{}
	if len(fresh.Buyer) > 0 {
		// The order's buyer snapshot was written by createOrder from the
		// same shape, so a failure here means a hand-edited row; the
		// instruction can still be issued without the display fields.
		_ = json.Unmarshal(fresh.Buyer, &buyer)
	}

	// The instruction never outlives its order (same rule as CreateOrder).
	expiresAt := fresh.ExpiresAt
	attempt := &model.PaymentAttempt{
		ID: store.NewID(), OrderID: fresh.ID, Provider: fresh.Provider,
		Status: domain.AttemptPending, AmountMinor: fresh.TotalMinor, Currency: fresh.Currency,
		ExpiresAt: &expiresAt, CreatedAt: now, UpdatedAt: now,
	}
	result, err := prov.CreateAttempt(ctx, provider.CreateAttemptRequest{
		OrderCode: fresh.Code, NumericCode: fresh.NumericCode, AmountMinor: fresh.TotalMinor,
		Currency: fresh.Currency, CurrencyExp: fresh.CurrencyExp, Description: fresh.Code,
		Buyer: buyer, ExpiresAt: expiresAt,
		ReturnURL: s.baseURL() + "/checkout/success?code=" + fresh.Code,
		CancelURL: s.baseURL() + "/checkout/success?code=" + fresh.Code + "&canceled=1",
	})
	if err != nil {
		attempt.Status = domain.AttemptFailed
		attempt.QRPayload = ""
		attempt.CheckoutURL = ""
		attempt.Raw = json.RawMessage(`{}`)
		if cerr := s.Store.CreatePaymentAttempt(ctx, attempt); cerr != nil {
			s.logger().Error("checkout: could not record failed reissue attempt",
				"order", fresh.Code, "provider", fresh.Provider, "error", cerr)
		}
		s.logger().Error("checkout: provider reissue attempt failed",
			"order", fresh.Code, "provider", fresh.Provider, "error", err)
		return nil, fmt.Errorf("%w: create payment instruction: %w", ErrProviderError, err)
	}
	attempt.ProviderRef = result.ProviderRef
	attempt.QRPayload = result.QRPayload
	attempt.CheckoutURL = result.CheckoutURL
	attempt.VANumber = result.VANumber
	if !result.ExpiresAt.IsZero() {
		attempt.ExpiresAt = &result.ExpiresAt
	}
	attempt.Raw = attemptRaw(&result)
	if err := s.Store.CreatePaymentAttempt(ctx, attempt); err != nil {
		return nil, err
	}
	return attempt, nil
}
