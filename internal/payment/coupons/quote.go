package coupons

import (
	"encoding/json"
	"math"
	"sort"

	"github.com/tabloy/keygate/internal/model"
	"github.com/tabloy/keygate/internal/payment/domain"
	"github.com/tabloy/keygate/internal/store"
)

const (
	// defaultMaxStack is the coupon_max_stack fallback (spec §3.9). A
	// non-positive or unreadable setting means this, never "no stack".
	defaultMaxStack = 2
	// maxStackCeiling bounds a typo in the setting; the rule exists to
	// keep a stack legible, not to allow twenty discounts.
	maxStackCeiling = 10
	// percentScale is 100% in basis points.
	percentScale int64 = 10000
	// maxSubtotal keeps subtotal*value inside int64. Checkout bounds an
	// order the same way (maxOrderTotalMinor).
	maxSubtotal int64 = math.MaxInt64 / 20000
	// maxCodes bounds one request; a coupon stack is a handful of codes.
	maxCodes = 20
)

// Discount is one applied coupon in a quote's breakdown. Value is the
// coupon's own unit (basis points for percent, minor units for fixed);
// AmountMinor is what it took off this order.
type Discount struct {
	CouponID    string `json:"coupon_id,omitempty"`
	Code        string `json:"code"`
	Type        string `json:"type"`
	Value       int64  `json:"value"`
	AmountMinor int64  `json:"amount_minor"`
	Priority    int    `json:"priority"`
	TrialDays   int    `json:"trial_days,omitempty"`
}

// Quote is the priced result of applying coupon codes to a subtotal.
type Quote struct {
	SubtotalMinor int64      `json:"subtotal_minor"`
	DiscountMinor int64      `json:"discount_minor"`
	TaxMinor      int64      `json:"tax_minor"`
	TotalMinor    int64      `json:"total_minor"`
	TrialDays     int        `json:"trial_days,omitempty"`
	Discounts     []Discount `json:"discounts"`
}

// Codes returns the applied codes in the order the discounts were
// applied. It is what the order's signature is built from, so it must
// list only codes that were actually applied — with ignore_invalid_coupons
// a rejected code is not part of the purchase.
func (q *Quote) Codes() []string {
	if q == nil || len(q.Discounts) == 0 {
		return nil
	}
	codes := make([]string, 0, len(q.Discounts))
	for _, d := range q.Discounts {
		codes = append(codes, d.Code)
	}
	return codes
}

// FullPrice is the quote for a purchase with no coupon applied. Checkout
// uses it when coupon support is not wired into a service, so every order
// is priced through the same arithmetic.
func FullPrice(in QuoteInput) *Quote { return price(in, nil, 0) }

// QuoteInput is one pricing request. The caller supplies the money it has
// already resolved (plan, product, quantity, subtotal) because only
// checkout knows how to resolve a provider price; coupons decides what
// the codes are worth against it. Kind is the order kind ("new" when
// empty); Email and ExternalCustomerID are the buyer identity used by the
// per-customer limit and the email binding.
type QuoteInput struct {
	Codes              []string
	IgnoreInvalid      bool
	Plan               *model.Plan
	Product            *model.Product
	Quantity           int
	SubtotalMinor      int64
	Currency           string
	CurrencyExponent   int16
	Kind               string
	Email              string
	ExternalCustomerID string
}

// admit applies the stacking rules to already-validated coupons and
// returns them in application order (priority ascending, then code), plus
// the effective trial length. Sorting before admitting makes the outcome
// deterministic: priority decides which code wins a conflict, not the
// order the buyer typed.
func admit(cands []*model.Coupon, in QuoteInput, maxStack int) ([]*model.Coupon, int, error) {
	if maxStack < 1 {
		maxStack = defaultMaxStack
	}
	ordered := make([]*model.Coupon, len(cands))
	copy(ordered, cands)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Priority != ordered[j].Priority {
			return ordered[i].Priority < ordered[j].Priority
		}
		return ordered[i].Code < ordered[j].Code
	})

	applied := make([]*model.Coupon, 0, len(ordered))
	groups := make(map[string]string, len(ordered))
	amount := 0
	var trialDays int
	for _, c := range ordered {
		var err error
		if c.Type == "trial" {
			err = trialRule(c, applied)
		} else {
			err = amountRule(c, applied, groups, amount, maxStack)
		}
		if err != nil {
			if in.IgnoreInvalid {
				continue
			}
			return nil, 0, err
		}
		applied = append(applied, c)
		if c.Type == "trial" {
			trialDays = c.TrialDays
			if in.Plan != nil && in.Plan.TrialDays > trialDays {
				trialDays = in.Plan.TrialDays
			}
			continue
		}
		groups[c.StackGroup] = c.Code
		amount++
	}
	return applied, trialDays, nil
}

// trialRule checks a trial coupon against what is already applied. The
// applied set is homogeneous (a trial never coexists with amount coupons),
// so one look at the first entry answers both questions.
func trialRule(c *model.Coupon, applied []*model.Coupon) error {
	if len(applied) == 0 {
		return nil
	}
	if applied[0].Type == "trial" {
		return reject(CodeInvalid, "coupon %q: only one trial coupon can be applied", c.Code)
	}
	return reject(CodeTrialMixed, "coupon %q cannot be combined with amount coupon %q", c.Code, applied[0].Code)
}

// amountRule checks one amount coupon against the applied set: trial
// mixing, the stack size, the stack group, and the stackable flag. The
// stackable flag is symmetric — a non-stackable coupon blocks both being
// combined into and combined onto.
func amountRule(c *model.Coupon, applied []*model.Coupon, groups map[string]string, amount, maxStack int) error {
	if len(applied) > 0 && applied[0].Type == "trial" {
		return reject(CodeTrialMixed, "coupon %q cannot be combined with trial coupon %q", c.Code, applied[0].Code)
	}
	if amount >= maxStack {
		return reject(CodeStackLimit, "coupon %q cannot be added: at most %d coupons can be stacked", c.Code, maxStack)
	}
	if other, ok := groups[c.StackGroup]; ok {
		return reject(CodeGroupConflict, "coupon %q cannot be combined with %q: both are in stack group %q",
			c.Code, other, c.StackGroup)
	}
	for _, a := range applied {
		if !a.Stackable {
			return reject(CodeNotStackable, "coupon %q cannot be combined with %q", c.Code, a.Code)
		}
	}
	if len(applied) > 0 && !c.Stackable {
		// A non-stackable coupon is valid on its own; it only refuses to
		// be combined with anything, whichever way round the pair is
		// submitted.
		return reject(CodeNotStackable, "coupon %q cannot be combined with other coupons", c.Code)
	}
	return nil
}

// price computes the money for the admitted coupons. Percent coupons take
// their share of what is still left, so a 10% then 20% stack is 100,000
// followed by 180,000 on a 1,000,000 subtotal, not 280,000 of the
// original. A trial takes the whole subtotal: the buyer owes zero now.
func price(in QuoteInput, applied []*model.Coupon, trialDays int) *Quote {
	q := &Quote{SubtotalMinor: in.SubtotalMinor, Discounts: make([]Discount, 0, len(applied))}
	remaining := in.SubtotalMinor
	if trialDays > 0 {
		q.TrialDays = trialDays
		for _, c := range applied {
			if c.Type != "trial" {
				continue
			}
			q.Discounts = append(q.Discounts, Discount{
				CouponID: c.ID, Code: c.Code, Type: c.Type, Value: c.Value,
				AmountMinor: remaining, Priority: c.Priority, TrialDays: trialDays,
			})
		}
		q.DiscountMinor = remaining
		remaining = 0
	} else {
		for _, c := range applied {
			amount := c.Value
			if c.Type == "percent" {
				amount = (remaining*c.Value + percentScale/2) / percentScale
			}
			if amount < 0 {
				amount = 0
			}
			if amount > remaining {
				amount = remaining
			}
			q.Discounts = append(q.Discounts, Discount{
				CouponID: c.ID, Code: c.Code, Type: c.Type, Value: c.Value,
				AmountMinor: amount, Priority: c.Priority,
			})
			q.DiscountMinor += amount
			remaining -= amount
		}
	}
	rate, inclusive := taxSettings(in)
	totals := domain.ComputeTotals(in.SubtotalMinor, q.DiscountMinor, rate, inclusive)
	q.DiscountMinor = totals.DiscountMinor
	q.TaxMinor = totals.TaxMinor
	q.TotalMinor = totals.TotalMinor
	return q
}

// taxSettings reads the product's VAT configuration. A quote without a
// product (a preview against a plan whose product was not loaded) is
// VAT-free rather than guessed.
func taxSettings(in QuoteInput) (rate int, inclusive bool) {
	if in.Product == nil {
		return 0, true
	}
	inclusive = true
	if in.Product.PriceIncludesTax != nil {
		inclusive = *in.Product.PriceIncludesTax
	}
	return in.Product.TaxRateBP, inclusive
}

// CodesSignature is the canonical JSON form of a set of codes. Checkout
// stores it on the order and compares it when deciding whether a retried
// create may reuse an order: same codes, same price.
func CodesSignature(codes []string) string {
	clean := normaliseCodes(codes)
	sort.Strings(clean)
	if len(clean) == 0 {
		return "[]"
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// normaliseCodes trims, upper-cases and de-duplicates submitted codes,
// preserving the first occurrence. Quote owns this so every caller (and
// the signature) agrees on what one code is.
func normaliseCodes(codes []string) []string {
	seen := make(map[string]bool, len(codes))
	out := make([]string, 0, len(codes))
	for _, raw := range codes {
		code := store.NormaliseCouponCode(raw)
		if code == "" || seen[code] {
			continue
		}
		seen[code] = true
		out = append(out, code)
	}
	return out
}
