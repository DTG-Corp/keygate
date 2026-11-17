// Package checkout turns a sellable plan, a buyer and a provider into a
// payment order plus the one provider instruction the customer pays
// with. It owns pricing, VAT and provider selection; the adapters own
// everything provider-specific.
package checkout

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/payment/provider"
	"github.com/dtg-corp/dsoft/internal/payment/providercfg"
	"github.com/dtg-corp/dsoft/internal/service"
	"github.com/dtg-corp/dsoft/internal/store"
)

var (
	// ErrPlanNotSellable covers every reason a plan cannot be sold right
	// now: unknown or inactive plan, no product, bad buyer email, or no
	// price for the requested provider and currency.
	ErrPlanNotSellable = errors.New("plan is not sellable")
	// ErrProviderUnavailable means the provider row could not be
	// resolved into an adapter: unknown, disabled, incomplete, or a
	// missing factory.
	ErrProviderUnavailable = errors.New("payment provider is not available")
	// ErrProviderError means a usable adapter could not create the
	// payment instruction: the provider API failed or refused the
	// request. It is upstream's fault, not the caller's, so the API maps
	// it to 502 rather than 500.
	ErrProviderError = errors.New("payment provider failed to create the payment instruction")
	// ErrQuantityInvalid means the requested licence volume cannot be
	// priced: it is outside 1..MaxQuantity, or multiplying it by the
	// unit price would overflow the order's integer money.
	ErrQuantityInvalid = errors.New("quantity is invalid")
	// ErrPriceNotConfigured is re-exported so callers of checkout can
	// errors.Is against one sentinel instead of importing store too.
	ErrPriceNotConfigured = store.ErrPriceNotConfigured
)

const (
	defaultOrderTTL = 24 * time.Hour
	// settlementCurrency is the only currency phase 1a sells. A plan
	// priced in anything else is not sellable rather than accidentally
	// charged in the wrong unit.
	settlementCurrency = "VND"

	// MaxQuantity caps one order. The spec sets no licence-volume
	// policy; 100 seats is a sane ceiling that also keeps a hostile
	// quantity far away from int64 money overflow.
	MaxQuantity = 100

	// maxOrderTotalMinor bounds one order so the VAT arithmetic cannot
	// overflow: domain.ComputeTotals multiplies the subtotal by 10000,
	// so the subtotal must stay below MaxInt64/20000 (about 4.6e14
	// minor units — far beyond any licence sale).
	maxOrderTotalMinor int64 = math.MaxInt64 / 20000

	// defaultBaseURL is only used when the server was built without a
	// public origin (tests, tools). The PayOS link needs *some* absolute
	// URL and a local one keeps a dev instance usable.
	defaultBaseURL = "http://localhost:9000"
)

// providerPriority is the fallback order when the request names no
// provider: SePay first (the phase-1a primary rail), then PayOS.
var providerPriority = []string{"sepay", "payos"}

// ProviderFactory resolves one product's provider row into a ready
// adapter. It is the only place that knows how a ResolvedConfig maps
// onto a concrete adapter; checkout and the reconciler both call it.
type ProviderFactory func(ctx context.Context, productID, providerName string) (provider.Provider, *providercfg.ResolvedConfig, error)

// Service creates payment orders. Store, Providers and BaseURL are
// required; Webhooks and Log are optional.
type Service struct {
	Store     *store.Store
	Config    *providercfg.Service
	Providers ProviderFactory
	Webhooks  *service.WebhookService
	Log       *slog.Logger
	// BaseURL is the public origin (config.BaseURL); PayOS requires
	// absolute https return/cancel URLs and phase 1a has no server-side
	// success/cancel routes, so both point at the SPA checkout pages.
	BaseURL string
}

// CreateOrderInput is one checkout request. Quantity must be between 1
// and MaxQuantity; Provider empty falls back to the configured default
// and then to the first provider that can actually sell the plan.
type CreateOrderInput struct {
	PlanID         string
	Email          string
	Provider       string
	Quantity       int
	Buyer          provider.Buyer
	Metadata       map[string]any
	CreatedByKeyID string
}

// OrderResult is the order and the instruction the buyer pays with. A
// retried create whose previous instruction is still payable returns the
// original pair rather than minting a second QR.
type OrderResult struct {
	Order   *model.PaymentOrder
	Attempt *model.PaymentAttempt
}

func (s *Service) CreateOrder(ctx context.Context, in CreateOrderInput) (*OrderResult, error) {
	if in.Quantity < 1 || in.Quantity > MaxQuantity {
		return nil, fmt.Errorf("%w: quantity must be between 1 and %d", ErrQuantityInvalid, MaxQuantity)
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" || !strings.Contains(email, "@") {
		return nil, fmt.Errorf("%w: a buyer email is required for the licence", ErrPlanNotSellable)
	}
	plan, err := s.Store.FindPlanByID(ctx, in.PlanID)
	if err != nil {
		return nil, fmt.Errorf("%w: plan not found", ErrPlanNotSellable)
	}
	if !plan.Active {
		return nil, fmt.Errorf("%w: plan is inactive", ErrPlanNotSellable)
	}
	product, err := s.Store.FindProductByID(ctx, plan.ProductID)
	if err != nil {
		return nil, fmt.Errorf("%w: product not found", ErrPlanNotSellable)
	}
	providerName, prov, price, err := s.resolveProvider(ctx, plan, product, in.Provider)
	if err != nil {
		return nil, err
	}

	// Reuse this buyer's still-open order for the same purchase. A
	// retried create must not ask PayOS for a second link for one
	// orderCode, nor mint a second QR for one bank transfer. An attempt
	// that is still payable is handed back untouched; a failed or
	// expired one is replaced on the same order.
	order, latest, err := s.resumableOrder(ctx, plan, email, providerName, in.Quantity, price.ID)
	if err != nil {
		return nil, err
	}
	created := false
	if order != nil && latest != nil && (payableAttempt(latest, time.Now()) || latest.Status == domain.AttemptSucceeded) {
		return &OrderResult{Order: order, Attempt: latest}, nil
	}
	if order == nil {
		order, err = s.createOrder(ctx, plan, product, price, email, providerName, in)
		if err != nil {
			return nil, err
		}
		created = true
	}

	// The attempt never outlives its order: a reused order is up to a
	// day old, and a fresh 24h link on a soon-to-expire order would
	// invite a transfer the order can no longer accept.
	expiresAt := order.ExpiresAt
	attempt := &model.PaymentAttempt{
		ID: store.NewID(), OrderID: order.ID, Provider: providerName,
		Status: domain.AttemptPending, AmountMinor: order.TotalMinor, Currency: order.Currency,
		ExpiresAt: &expiresAt, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	result, err := prov.CreateAttempt(ctx, provider.CreateAttemptRequest{
		OrderCode: order.Code, NumericCode: order.NumericCode, AmountMinor: order.TotalMinor,
		Currency: order.Currency, CurrencyExp: order.CurrencyExp, Description: order.Code,
		Buyer: in.Buyer, ExpiresAt: expiresAt,
		// PayOS signs and requires both; SePay ignores them.
		ReturnURL: s.baseURL() + "/checkout/success?code=" + order.Code,
		CancelURL: s.baseURL() + "/checkout/success?code=" + order.Code + "&canceled=1",
	})
	if err != nil {
		// The order exists but has no payment instruction. Record the
		// failed attempt so the retry has an audit trail, leave the
		// order pending (the expiry sweeper closes it), and let the
		// caller retry.
		attempt.Status = domain.AttemptFailed
		attempt.QRPayload = ""
		attempt.CheckoutURL = ""
		attempt.Raw = json.RawMessage(`{}`)
		if cerr := s.Store.CreatePaymentAttempt(ctx, attempt); cerr != nil {
			s.logger().Error("checkout: could not record failed attempt",
				"order", order.Code, "provider", providerName, "error", cerr)
		}
		s.logger().Error("checkout: provider create attempt failed",
			"order", order.Code, "provider", providerName, "error", err)
		return nil, fmt.Errorf("%w: create payment instruction: %w", ErrProviderError, err)
	}
	attempt.ProviderRef = result.ProviderRef
	attempt.QRPayload = result.QRPayload
	attempt.CheckoutURL = result.CheckoutURL
	attempt.VANumber = result.VANumber
	if !result.ExpiresAt.IsZero() {
		attempt.ExpiresAt = &result.ExpiresAt
	}
	// attempts.raw is the spec's JSONB escape hatch: it keeps the bank
	// account the adapter chose, plus anything provider-specific, so the
	// public status page can render transfer details without asking the
	// provider again.
	attempt.Raw = attemptRaw(&result)
	if err := s.Store.CreatePaymentAttempt(ctx, attempt); err != nil {
		return nil, err
	}
	if created {
		s.Webhooks.Dispatch(ctx, product.ID, model.EventOrderCreated, map[string]any{"order": order})
	}
	return &OrderResult{Order: order, Attempt: attempt}, nil
}

// createOrder prices one quantity of a plan and writes the order.
func (s *Service) createOrder(
	ctx context.Context, plan *model.Plan, product *model.Product,
	price *model.PlanPrice, email, providerName string, in CreateOrderInput,
) (*model.PaymentOrder, error) {
	subtotal, err := orderSubtotal(price.AmountMinor, in.Quantity)
	if err != nil {
		return nil, err
	}
	taxRate := product.TaxRateBP
	priceIncludesTax := true
	if product.PriceIncludesTax != nil {
		priceIncludesTax = *product.PriceIncludesTax
	}
	totals := domain.ComputeTotals(subtotal, 0, taxRate, priceIncludesTax)
	metadata := make(map[string]any, len(in.Metadata))
	for k, v := range in.Metadata {
		metadata[k] = v
	}
	metaJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("%w: metadata is not encodable: %w", ErrPlanNotSellable, err)
	}
	buyerJSON, err := json.Marshal(in.Buyer)
	if err != nil {
		return nil, fmt.Errorf("%w: buyer is not encodable: %w", ErrPlanNotSellable, err)
	}
	return s.Store.CreatePaymentOrder(ctx, store.CreateOrderInput{
		ProductID: product.ID, PlanID: plan.ID, PriceID: price.ID, Email: email, Kind: "new",
		Quantity: in.Quantity, SubtotalMinor: subtotal, DiscountMinor: totals.DiscountMinor,
		TaxMinor: totals.TaxMinor, TotalMinor: totals.TotalMinor,
		Currency: settlementCurrency, CurrencyExponent: domain.CurrencyExponent(settlementCurrency),
		TaxRateBP: taxRate, Provider: providerName, ExpiresAt: time.Now().Add(s.orderTTL(ctx)),
		Buyer: buyerJSON, Metadata: metaJSON, CreatedByKeyID: in.CreatedByKeyID,
	})
}

// resumableOrder returns the buyer's newest pending order for exactly
// this purchase and its latest attempt (nil when the order has none).
//
// There is no store helper for this lookup: every store/payments.go
// reader is keyed by an order identifier, and the reusable order is the
// one the caller has not been told about yet. The predicate is narrow on
// purpose — same plan, price, quantity, provider and buyer — so two
// genuinely different purchases never collapse into one.
func (s *Service) resumableOrder(
	ctx context.Context, plan *model.Plan, email, providerName string, quantity int, priceID string,
) (*model.PaymentOrder, *model.PaymentAttempt, error) {
	order := new(model.PaymentOrder)
	err := s.Store.DB.NewSelect().Model(order).
		Where("product_id = ?", plan.ProductID).
		Where("plan_id = ?", plan.ID).
		Where("price_id = ?", priceID).
		Where("email = ?", email).
		Where("provider = ?", providerName).
		Where("quantity = ?", quantity).
		Where("kind = ?", "new").
		Where("status = ?", domain.OrderPending).
		Where("expires_at > ?", time.Now()).
		Order("created_at DESC", "id DESC").
		Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	latest, err := s.Store.GetLatestPaymentAttempt(ctx, order.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return order, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return order, latest, nil
}

// payableAttempt reports whether an attempt still offers the buyer a way
// to pay: pending, with an instruction, and not expired.
func payableAttempt(a *model.PaymentAttempt, now time.Time) bool {
	return a != nil && a.Status == domain.AttemptPending &&
		(a.QRPayload != "" || a.CheckoutURL != "") &&
		a.ExpiresAt != nil && a.ExpiresAt.After(now)
}

// resolveProvider picks the provider, adapter and price for one order.
//
// An explicit request never falls back: asking for a provider that
// cannot serve the plan is an error. With no explicit provider the
// configured default is tried first, then sepay and payos in order. A
// candidate is usable only when the plan has an active price for it and
// the factory can resolve its config, which is what lets a PayOS-only
// install sell without naming payos in every request (spec §3.3).
func (s *Service) resolveProvider(ctx context.Context, plan *model.Plan, product *model.Product, requested string) (string, provider.Provider, *model.PlanPrice, error) {
	if s.Providers == nil {
		return "", nil, nil, fmt.Errorf("%w: no provider factory is configured", ErrProviderUnavailable)
	}
	if explicit := normaliseProvider(requested); explicit != "" {
		prov, err := s.adapter(ctx, product.ID, explicit)
		if err != nil {
			return "", nil, nil, fmt.Errorf("%w: %w", ErrProviderUnavailable, err)
		}
		price, err := s.Store.ResolvePlanPrice(ctx, plan.ID, explicit, settlementCurrency, plan.BillingInterval)
		if err != nil {
			return "", nil, nil, fmt.Errorf("%w: %w", ErrPlanNotSellable, err)
		}
		return explicit, prov, price, nil
	}

	var lastErr error
	priced := false
	for _, name := range s.providerCandidates(ctx) {
		price, err := s.Store.ResolvePlanPrice(ctx, plan.ID, name, settlementCurrency, plan.BillingInterval)
		if errors.Is(err, store.ErrPriceNotConfigured) {
			continue
		}
		if err != nil {
			// A database failure is not a sellability answer.
			return "", nil, nil, err
		}
		priced = true
		prov, err := s.adapter(ctx, product.ID, name)
		if err != nil {
			// Priced but unusable (disabled, missing credentials): the
			// next candidate may serve, so keep looking.
			lastErr = err
			continue
		}
		return name, prov, price, nil
	}
	if priced && lastErr != nil {
		return "", nil, nil, fmt.Errorf("%w: %w", ErrProviderUnavailable, lastErr)
	}
	return "", nil, nil, fmt.Errorf("%w: %w", ErrPlanNotSellable, store.ErrPriceNotConfigured)
}

// adapter resolves one provider name through the factory, rejecting a
// nil adapter as unavailable rather than dereferencing it later.
func (s *Service) adapter(ctx context.Context, productID, name string) (provider.Provider, error) {
	prov, _, err := s.Providers(ctx, productID, name)
	if err != nil {
		return nil, err
	}
	if prov == nil {
		return nil, fmt.Errorf("provider %q resolved to no adapter", name)
	}
	return prov, nil
}

// providerCandidates returns the configured default (when set) followed
// by the supported providers in priority order, without duplicates.
func (s *Service) providerCandidates(ctx context.Context) []string {
	candidates := make([]string, 0, len(providerPriority)+1)
	seen := make(map[string]bool, len(providerPriority)+1)
	add := func(name string) {
		name = normaliseProvider(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		candidates = append(candidates, name)
	}
	if def, err := s.Store.GetSetting(ctx, "payment_default_provider"); err == nil {
		add(def)
	}
	for _, name := range providerPriority {
		add(name)
	}
	return candidates
}

func normaliseProvider(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// orderSubtotal multiplies a unit price by a validated quantity without
// wrapping. A negative or overflowing product is ErrQuantityInvalid: the
// order would otherwise snapshot a total the buyer never sees.
func orderSubtotal(unitMinor int64, quantity int) (int64, error) {
	if unitMinor < 0 {
		return 0, fmt.Errorf("%w: the plan price is negative", ErrPlanNotSellable)
	}
	q := int64(quantity)
	if q < 1 {
		return 0, fmt.Errorf("%w: quantity must be positive", ErrQuantityInvalid)
	}
	if unitMinor != 0 && unitMinor > math.MaxInt64/q {
		return 0, fmt.Errorf("%w: quantity %d overflows the order total", ErrQuantityInvalid, quantity)
	}
	subtotal := unitMinor * q
	if unitMinor != 0 && subtotal/unitMinor != q {
		return 0, fmt.Errorf("%w: quantity %d overflows the order total", ErrQuantityInvalid, quantity)
	}
	if subtotal > maxOrderTotalMinor {
		return 0, fmt.Errorf("%w: order total %d exceeds the supported maximum", ErrQuantityInvalid, subtotal)
	}
	return subtotal, nil
}

func (s *Service) orderTTL(ctx context.Context) time.Duration {
	if raw, err := s.Store.GetSetting(ctx, "payment_order_ttl_minutes"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return defaultOrderTTL
}

func (s *Service) baseURL() string {
	if base := strings.TrimRight(s.BaseURL, "/"); base != "" {
		return base
	}
	return defaultBaseURL
}

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// attemptRaw serialises the non-secret parts of a CreateAttemptResult.
func attemptRaw(res *provider.CreateAttemptResult) json.RawMessage {
	b, err := json.Marshal(map[string]any{
		"bank": res.Bank, "provider_raw": res.Raw, "description": res.Description,
		// SePay's QRPayload is already an image URL; PayOS's is the
		// EMVCo string, and its renderable image would otherwise have no
		// column to live in (payment_attempts has no qr_image column).
		"qr_image_url": res.QRImageURL,
	})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
