package coupons

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/dtg-corp/dsoft/internal/model"
	"github.com/dtg-corp/dsoft/internal/store"
)

// MetadataKey is the order metadata block checkout writes the applied
// codes into. It is server-owned: buyer-supplied metadata is copied under
// it only by checkout, never merged from the request.
const MetadataKey = "coupons"

// SettingMaxStack is the installation setting that caps how many amount
// coupons one order may carry (spec §3.9).
const SettingMaxStack = "coupon_max_stack"

// Service prices codes and records their redemptions.
type Service struct {
	Store *store.Store
	Log   *slog.Logger
}

// New binds the service to a store. A service with no Log uses the
// process default logger.
func New(st *store.Store) *Service { return &Service{Store: st} }

func (s *Service) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Quote validates the submitted codes against the plan, the buyer and the
// clock, applies the stacking rules, and prices what survives. The caller
// supplies the subtotal it has already resolved; this function never
// resolves a price itself.
func (s *Service) Quote(ctx context.Context, in QuoteInput) (*Quote, error) {
	if in.SubtotalMinor < 0 || in.SubtotalMinor > maxSubtotal {
		return nil, fmt.Errorf("coupons: subtotal %d is outside the quotable range", in.SubtotalMinor)
	}
	codes := normaliseCodes(in.Codes)
	if len(codes) == 0 {
		return price(in, nil, 0), nil
	}
	if len(codes) > maxCodes {
		if in.IgnoreInvalid {
			codes = codes[:maxCodes]
		} else {
			return nil, reject(CodeInvalid, "at most %d coupon codes can be applied", maxCodes)
		}
	}
	if in.Plan == nil {
		return nil, errors.New("coupons: a plan is required to price coupon codes")
	}
	rows, err := s.Store.CouponsByCodes(ctx, codes)
	if err != nil {
		return nil, err
	}
	byCode := make(map[string]*model.Coupon, len(rows))
	ids := make([]string, 0, len(rows))
	for _, c := range rows {
		byCode[c.Code] = c
		ids = append(ids, c.ID)
	}
	scopes, err := s.Store.CouponScopesFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	maxStack := s.maxStack(ctx)
	candidates := make([]*model.Coupon, 0, len(codes))
	for _, code := range codes {
		c, ok := byCode[code]
		if !ok {
			if in.IgnoreInvalid {
				continue
			}
			return nil, reject(CodeInvalid, "coupon %q does not exist", code)
		}
		if err := s.validate(ctx, c, scopes[c.ID], in); err != nil {
			if in.IgnoreInvalid {
				continue
			}
			return nil, err
		}
		candidates = append(candidates, c)
	}
	applied, trialDays, err := admit(candidates, in, maxStack)
	if err != nil {
		return nil, err
	}
	return price(in, applied, trialDays), nil
}

// PortalValidate is the portal's preview: exactly one code, always for a
// new purchase. The portal never previews a renewal, so a trial cannot be
// stretched across one.
func (s *Service) PortalValidate(ctx context.Context, in QuoteInput) (*Quote, error) {
	codes := normaliseCodes(in.Codes)
	if len(codes) != 1 {
		return nil, reject(CodeInvalid, "exactly one coupon code is required")
	}
	in.Codes = codes
	in.Kind = "new"
	return s.Quote(ctx, in)
}

// validate checks one coupon against the current time, the plan, the
// buyer identity and the redemption counters. The pure rules (stacking and
// pricing) run later on the survivors.
func (s *Service) validate(ctx context.Context, c *model.Coupon, scopes []*model.CouponScope, in QuoteInput) error {
	now := time.Now()
	switch {
	case !c.Active:
		return reject(CodeInvalid, "coupon %q is not active", c.Code)
	case c.ValidFrom != nil && now.Before(*c.ValidFrom):
		return reject(CodeInvalid, "coupon %q is not valid yet", c.Code)
	case c.ValidTo != nil && now.After(*c.ValidTo):
		return reject(CodeInvalid, "coupon %q has expired", c.Code)
	}
	if !couponCovers(c, scopes, in.Plan) {
		return reject(CodeInvalid, "coupon %q does not apply to this plan", c.Code)
	}
	if c.Currency != "" && !strings.EqualFold(c.Currency, in.Currency) {
		return reject(CodeInvalid, "coupon %q is priced in %s", c.Code, strings.ToUpper(c.Currency))
	}
	if c.AssignedEmail != "" && !strings.EqualFold(c.AssignedEmail, strings.TrimSpace(in.Email)) {
		// A hard block: this code is never applied to another email,
		// with or without ignore_invalid_coupons. With the flag set it is
		// merely skipped, which applies nothing.
		return reject(CodeAssigned, "coupon %q is assigned to another customer", c.Code)
	}
	if c.MaxRedemptions > 0 && c.RedeemedCount >= c.MaxRedemptions {
		return reject(CodeExhausted, "coupon %q has reached its redemption limit", c.Code)
	}
	if c.PerCustomerLimit > 0 {
		n, err := s.Store.CountCouponRedemptionsFor(ctx, c.ID, in.Email, in.ExternalCustomerID)
		if err != nil {
			return err
		}
		if n >= c.PerCustomerLimit {
			return reject(CodeExhausted,
				"coupon %q has already been used the maximum number of times for this customer", c.Code)
		}
	}
	if c.Type == "trial" {
		kind := in.Kind
		if kind == "" {
			kind = "new"
		}
		if kind != "new" {
			return reject(CodeInvalid, "coupon %q is a trial and only applies to new purchases", c.Code)
		}
		// A trial needs a recurring plan. A perpetual plan cannot hold a
		// period, so settling it would grant a free perpetual licence.
		if in.Plan.LicenseType != "trial" && in.Plan.LicenseType != "subscription" {
			return reject(CodeInvalid, "coupon %q needs a recurring plan", c.Code)
		}
		if in.Plan.TrialDays <= 0 {
			return reject(CodeInvalid, "coupon %q needs a plan that offers a trial", c.Code)
		}
	}
	return nil
}

// couponCovers reports whether a coupon's scope matches the plan. An empty
// scope set covers everything; a product scope matches the plan's product,
// a plan scope the plan itself.
func couponCovers(c *model.Coupon, scopes []*model.CouponScope, plan *model.Plan) bool {
	if len(scopes) == 0 {
		return true
	}
	for _, sc := range scopes {
		switch sc.ScopeType {
		case "product":
			if plan != nil && sc.ScopeID == plan.ProductID {
				return true
			}
		case "plan":
			if plan != nil && sc.ScopeID == plan.ID {
				return true
			}
		}
	}
	return false
}

// maxStack reads coupon_max_stack with the documented default. An
// unreadable or non-positive value means the default, never "no stack".
func (s *Service) maxStack(ctx context.Context) int {
	raw, err := s.Store.GetSetting(ctx, SettingMaxStack)
	if err != nil {
		return defaultMaxStack
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 {
		return defaultMaxStack
	}
	if n > maxStackCeiling {
		return maxStackCeiling
	}
	return n
}

// OrderCoupons is the block checkout snapshots on an order: the applied
// codes, the trial a coupon granted, and the customer identity used for
// the per-customer limit. It is read back at settlement, when the coupon
// row may already have been edited.
type OrderCoupons struct {
	Codes              []string     `json:"codes,omitempty"`
	Signature          string       `json:"signature,omitempty"`
	Trial              *TrialCoupon `json:"trial,omitempty"`
	ExternalCustomerID string       `json:"external_customer_id,omitempty"`
}

// TrialCoupon is the trial a coupon granted.
type TrialCoupon struct {
	CouponID  string `json:"coupon_id"`
	Code      string `json:"code"`
	TrialDays int    `json:"trial_days"`
}

// OrderMetadata renders the block checkout merges into the order metadata.
// It returns nil when no coupon was applied, so an order without coupons
// keeps the caller's metadata untouched. The external customer id lives
// here because payment_orders has no column for it.
func OrderMetadata(q *Quote, externalCustomerID string) map[string]any {
	if q == nil || (len(q.Discounts) == 0 && q.TrialDays == 0) {
		return nil
	}
	block := make(map[string]any, 4)
	codes := make([]string, 0, len(q.Discounts))
	for _, d := range q.Discounts {
		codes = append(codes, d.Code)
		if d.Type == "trial" {
			block["trial"] = map[string]any{
				"coupon_id": d.CouponID, "code": d.Code, "trial_days": d.TrialDays,
			}
		}
	}
	if len(codes) > 0 {
		block["codes"] = codes
		// The signature is what a retried create compares against: an
		// order priced with these codes must not be handed to a request
		// that asked for different ones.
		block["signature"] = CodesSignature(codes)
	}
	if id := strings.TrimSpace(externalCustomerID); id != "" {
		block["external_customer_id"] = id
	}
	return block
}

// FromOrderMetadata reads the block back. A malformed or absent block is
// an empty one: orders written before coupons existed have none.
func FromOrderMetadata(order *model.PaymentOrder) OrderCoupons {
	var out OrderCoupons
	if order == nil || len(order.Metadata) == 0 {
		return out
	}
	var envelope struct {
		Coupons *OrderCoupons `json:"coupons"`
	}
	if err := json.Unmarshal(order.Metadata, &envelope); err != nil || envelope.Coupons == nil {
		return out
	}
	return *envelope.Coupons
}

// OrderSignature is the coupon signature an order was priced with, "[]"
// when it carried none. Checkout compares it before reusing an order for a
// retried create.
func OrderSignature(order *model.PaymentOrder) string {
	if sig := FromOrderMetadata(order).Signature; sig != "" {
		return sig
	}
	return "[]"
}
