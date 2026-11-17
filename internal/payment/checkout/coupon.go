package checkout

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/payment/coupons"
	"github.com/dtg-corp/dsoft/internal/payment/domain"
	"github.com/dtg-corp/dsoft/internal/payment/provider"
	"github.com/dtg-corp/dsoft/internal/store"
)

// Settler turns a zero-total order into a paid one. A trial coupon makes
// the order free, and a free order has no payment instruction to wait
// for, so it must be settled at creation through the same settlement path
// every other payment takes (licence, events, paid hooks). The Lead wires
// the settlement service here.
type Settler interface {
	Settle(ctx context.Context, orderID, providerName string, ev provider.WebhookEvent) error
}

// ErrCouponSupportMissing means the service was built without the coupon
// or settlement service a coupon-bearing request needs. It is an
// installation error, never something a buyer can fix.
var ErrCouponSupportMissing = errors.New("coupon support is not configured")

// PreviewInput is one coupon preview: the codes a buyer typed, priced
// against a plan and a provider, before any order exists.
type PreviewInput struct {
	PlanID             string
	Provider           string
	Quantity           int
	Codes              []string
	IgnoreInvalid      bool
	Email              string
	ExternalCustomerID string
	// Portal marks a portal preview: exactly one code, always a new
	// purchase, so a trial cannot be stretched across a renewal.
	Portal bool
}

// PreviewResult is what the checkout page shows: the resolved plan, the
// price it would charge, and the coupon quote.
type PreviewResult struct {
	Plan    *model.Plan
	Product *model.Product
	Price   *model.PlanPrice
	Quote   *coupons.Quote
}

// PreviewQuote prices coupon codes without creating an order. It resolves
// the same price a real order would use, so the page never quotes a
// discount against a number checkout would not charge.
func (s *Service) PreviewQuote(ctx context.Context, in PreviewInput) (*PreviewResult, error) {
	quantity := in.Quantity
	if quantity == 0 {
		quantity = 1
	}
	if quantity < 1 || quantity > MaxQuantity {
		return nil, fmt.Errorf("%w: quantity must be between 1 and %d", ErrQuantityInvalid, MaxQuantity)
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
	price, err := s.previewPrice(ctx, plan, in.Provider)
	if err != nil {
		return nil, err
	}
	subtotal, err := orderSubtotal(price.AmountMinor, quantity)
	if err != nil {
		return nil, err
	}
	qin := coupons.QuoteInput{
		Codes: in.Codes, IgnoreInvalid: in.IgnoreInvalid,
		Plan: plan, Product: product, Quantity: quantity, SubtotalMinor: subtotal,
		Currency: settlementCurrency, CurrencyExponent: domain.CurrencyExponent(settlementCurrency),
		Kind: "new", Email: in.Email, ExternalCustomerID: in.ExternalCustomerID,
	}
	q, err := s.priceCodes(ctx, qin, in.Portal)
	if err != nil {
		return nil, err
	}
	return &PreviewResult{Plan: plan, Product: product, Price: price, Quote: q}, nil
}

// previewPrice resolves the price a preview prices against. Unlike
// resolveProvider it does not need a live provider adapter: a preview
// shows numbers, it does not mint a payment instruction, so an install
// whose credentials are not yet configured can still quote.
func (s *Service) previewPrice(ctx context.Context, plan *model.Plan, requested string) (*model.PlanPrice, error) {
	if explicit := normaliseProvider(requested); explicit != "" {
		price, err := s.Store.ResolvePlanPrice(ctx, plan.ID, explicit, settlementCurrency, plan.BillingInterval)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrPlanNotSellable, err)
		}
		return price, nil
	}
	for _, name := range s.providerCandidates(ctx) {
		price, err := s.Store.ResolvePlanPrice(ctx, plan.ID, name, settlementCurrency, plan.BillingInterval)
		if errors.Is(err, store.ErrPriceNotConfigured) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return price, nil
	}
	return nil, fmt.Errorf("%w: %w", ErrPlanNotSellable, store.ErrPriceNotConfigured)
}

// priceCodes runs the coupon quote (or the portal's single-code preview)
// and falls back to full price when coupon support is not wired and the
// request carried no codes. A request that carries codes without a coupon
// service fails closed: charging full price for a code the buyer typed
// would be a silent overcharge.
func (s *Service) priceCodes(ctx context.Context, in coupons.QuoteInput, portal bool) (*coupons.Quote, error) {
	if s.Coupons == nil {
		if len(in.Codes) > 0 {
			return nil, fmt.Errorf("%w: the request carried coupon codes", ErrCouponSupportMissing)
		}
		return coupons.FullPrice(in), nil
	}
	if portal {
		return s.Coupons.PortalValidate(ctx, in)
	}
	return s.Coupons.Quote(ctx, in)
}

// quoteFor prices the codes on a checkout request against the price that
// was just resolved for it.
func (s *Service) quoteFor(ctx context.Context, plan *model.Plan, product *model.Product, price *model.PlanPrice, email string, in CreateOrderInput) (*coupons.Quote, error) {
	subtotal, err := orderSubtotal(price.AmountMinor, in.Quantity)
	if err != nil {
		return nil, err
	}
	return s.priceCodes(ctx, coupons.QuoteInput{
		Codes: in.CouponCodes, IgnoreInvalid: in.IgnoreInvalidCoupons,
		Plan: plan, Product: product, Quantity: in.Quantity, SubtotalMinor: subtotal,
		Currency: settlementCurrency, CurrencyExponent: domain.CurrencyExponent(settlementCurrency),
		Kind: "new", Email: email, ExternalCustomerID: in.ExternalCustomerID,
	}, false)
}

// paidZeroOrder answers a retried create for a free purchase. A trial (or
// a 100%-off) order is settled the moment it is created, so the retry must
// get that same order back instead of minting a second free licence.
//
// The window is one order TTL: inside it the retry is the same purchase,
// outside it a customer whose trial ended long ago may buy again and the
// ordinary flow applies.
func (s *Service) paidZeroOrder(ctx context.Context, plan *model.Plan, email, providerName string, quantity int, priceID string) (*model.PaymentOrder, error) {
	order := new(model.PaymentOrder)
	err := s.Store.DB.NewSelect().Model(order).
		Where("product_id = ?", plan.ProductID).
		Where("plan_id = ?", plan.ID).
		Where("price_id = ?", priceID).
		Where("email = ?", email).
		Where("provider = ?", providerName).
		Where("quantity = ?", quantity).
		Where("kind = ?", "new").
		Where("status = ?", domain.OrderPaid).
		Where("total_minor = 0").
		Where("created_at > ?", time.Now().Add(-s.orderTTL(ctx))).
		Order("created_at DESC", "id DESC").
		Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return order, nil
}

// settleZeroTotal settles an order that has nothing to pay. The event is
// synthetic and deterministic (one per order code), so settlement's own
// idempotency covers a retry as well; the order is then re-read so the
// caller sees the paid row, licence link and all.
func (s *Service) settleZeroTotal(ctx context.Context, order *model.PaymentOrder, providerName string, q *coupons.Quote) (*OrderResult, error) {
	if s.Settler == nil {
		return nil, fmt.Errorf("%w: zero-total order %s cannot be settled", ErrCouponSupportMissing, order.Code)
	}
	ev := provider.WebhookEvent{
		Provider: providerName, ProviderEventID: "zero-total-" + order.Code, ProviderRef: order.Code,
		OrderCode: order.Code, AmountMinor: 0, Currency: order.Currency,
		Status: domain.OrderPaid, TransferType: "in", OccurredAt: time.Now(),
		Raw: json.RawMessage(`{"zero_total":true}`),
	}
	if err := s.Settler.Settle(ctx, order.ID, providerName, ev); err != nil {
		return nil, fmt.Errorf("checkout: settle zero-total order %s: %w", order.Code, err)
	}
	paid, err := s.Store.GetPaymentOrderByCode(ctx, order.Code)
	if err != nil {
		return nil, err
	}
	return &OrderResult{Order: paid, Discounts: discountRows(q)}, nil
}

// discountRows maps a quote onto response rows for the amount coupons.
// A trial is not a money coupon and has no row in
// payment_order_discounts; the response rebuilds it from order metadata,
// so one rule covers a fresh quote and a stored order alike.
func discountRows(q *coupons.Quote) []*model.PaymentOrderDiscount {
	if q == nil || len(q.Discounts) == 0 {
		return nil
	}
	rows := make([]*model.PaymentOrderDiscount, 0, len(q.Discounts))
	for _, d := range q.Discounts {
		if d.Type == "trial" {
			continue
		}
		rows = append(rows, &model.PaymentOrderDiscount{
			CouponID: d.CouponID, CouponCode: d.Code, Type: d.Type,
			Value: d.Value, AmountMinor: d.AmountMinor, Priority: d.Priority,
		})
	}
	return rows
}

// zeroTotalNeedsSettlement reports whether an order was made free by a
// coupon. A plan priced at zero is a different thing — it keeps the old
// flow, because nothing in the coupon module asked to change it.
func zeroTotalNeedsSettlement(q *coupons.Quote) bool {
	return q != nil && q.TotalMinor == 0 && q.DiscountMinor > 0
}

// couponMetadata merges the coupon block into buyer-supplied metadata.
// The block is server-owned: a buyer cannot write it, and it is what
// settlement reads the applied codes back from.
func couponMetadata(in map[string]any, q *coupons.Quote, externalCustomerID string) map[string]any {
	metadata := make(map[string]any, len(in)+1)
	for k, v := range in {
		metadata[k] = v
	}
	delete(metadata, coupons.MetadataKey)
	if block := coupons.OrderMetadata(q, externalCustomerID); block != nil {
		metadata[coupons.MetadataKey] = block
	}
	return metadata
}

// orderDiscounts maps a quote onto the store's snapshot rows.
func orderDiscounts(q *coupons.Quote) []store.OrderDiscount {
	if q == nil || len(q.Discounts) == 0 {
		return nil
	}
	out := make([]store.OrderDiscount, 0, len(q.Discounts))
	for _, d := range q.Discounts {
		// A trial is not a money coupon: payment_order_discounts.type only
		// admits percent and fixed, and the trial's snapshot is the
		// metadata block.
		if d.Type == "trial" {
			continue
		}
		out = append(out, store.OrderDiscount{
			CouponID: d.CouponID, CouponCode: d.Code, Type: d.Type,
			Value: d.Value, AmountMinor: d.AmountMinor, Priority: d.Priority,
		})
	}
	return out
}
