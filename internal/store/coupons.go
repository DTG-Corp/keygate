package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
)

var (
	// ErrCouponNotFound means no coupons row carries the id asked for.
	ErrCouponNotFound = errors.New("coupon not found")
	// ErrCouponInvalid means a coupon definition cannot mean what it says:
	// an unknown type or stack group, a percent value outside 1..10000
	// basis points, a fixed coupon with no currency, a trial with no days,
	// a malformed code, a negative limit, an inverted validity window, or
	// a scope pointing at a row that does not exist. It is returned before
	// or inside the write transaction, never after a partial write.
	ErrCouponInvalid = errors.New("invalid coupon definition")
	// ErrCouponCodeTaken means the normalised code already belongs to
	// another coupon. The code is unique in the schema, so this is also
	// what a concurrent create of the same code returns.
	ErrCouponCodeTaken = errors.New("coupon code already exists")
)

// NormaliseCouponCode is the one spelling a coupon code is stored and
// looked up by. Operators type codes with stray spaces and buyers type
// them in any case; both must reach the same row.
func NormaliseCouponCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// couponCodeConstraint is the coupons.code unique constraint, as Postgres
// names it.
const couponCodeConstraint = "coupons_code_key"

// couponStackGroups is the stack_group CHECK vocabulary.
var couponStackGroups = map[string]bool{"general": true, "campaign": true, "private": true}

// CouponScopeInput is one target a coupon is restricted to. No scopes
// means the coupon covers every product.
type CouponScopeInput struct {
	ScopeType string
	ScopeID   string
}

// CouponInput is a coupon definition as an operator writes it. Code is
// normalised by the store, so the caller may pass what was typed.
type CouponInput struct {
	Code             string
	Description      string
	Type             string
	Value            int64
	TrialDays        int
	Currency         string
	StackGroup       string
	Stackable        bool
	Priority         int
	AssignedEmail    string
	MaxRedemptions   int
	PerCustomerLimit int
	ValidFrom        *time.Time
	ValidTo          *time.Time
	Active           bool
	Scopes           []CouponScopeInput
}

func invalidCouponf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCouponInvalid, fmt.Sprintf(format, args...))
}

// validateCouponInput applies every rule the definition needs before a
// row is written. The schema repeats most of them as CHECKs; doing it here
// turns a constraint violation into an answer an operator can act on, and
// catches the rules SQL cannot express (the code alphabet, the validity
// window order).
func validateCouponInput(in CouponInput) error {
	code := NormaliseCouponCode(in.Code)
	if code == "" || len(code) > 64 {
		return invalidCouponf("code must be 1 to 64 characters")
	}
	for _, r := range code {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' && r != '.' {
			return invalidCouponf("code %q may only contain letters, digits, '-', '_' and '.'", code)
		}
	}
	currency := strings.ToUpper(strings.TrimSpace(in.Currency))
	switch in.Type {
	case "percent":
		if in.Value < 1 || in.Value > 10000 {
			return invalidCouponf("percent value must be between 1 and 10000 basis points")
		}
	case "fixed":
		if in.Value <= 0 {
			return invalidCouponf("fixed value must be greater than zero")
		}
		if currency == "" {
			return invalidCouponf("a fixed coupon needs a currency")
		}
	case "trial":
		if in.Value != 0 {
			return invalidCouponf("a trial coupon takes no money value")
		}
		if in.TrialDays < 1 || in.TrialDays > 3650 {
			return invalidCouponf("a trial coupon needs 1 to 3650 trial days")
		}
	default:
		return invalidCouponf("type must be percent, fixed or trial")
	}
	if currency != "" && len(currency) != 3 {
		return invalidCouponf("currency must be a three-letter ISO code")
	}
	if in.StackGroup != "" && !couponStackGroups[in.StackGroup] {
		return invalidCouponf("stack group must be general, campaign or private")
	}
	if in.MaxRedemptions < 0 {
		return invalidCouponf("max_redemptions cannot be negative")
	}
	if in.PerCustomerLimit < 0 {
		return invalidCouponf("per_customer_limit cannot be negative")
	}
	if in.Priority < 0 {
		return invalidCouponf("priority cannot be negative")
	}
	if in.ValidFrom != nil && in.ValidTo != nil && in.ValidTo.Before(*in.ValidFrom) {
		return invalidCouponf("valid_to must not be before valid_from")
	}
	for _, sc := range in.Scopes {
		if normaliseScopeType(sc.ScopeType) == "" {
			return invalidCouponf("scope_type must be product or plan")
		}
		if strings.TrimSpace(sc.ScopeID) == "" {
			return invalidCouponf("scope_id is required")
		}
	}
	return nil
}

// normaliseScopeType maps a written scope type onto the column vocabulary,
// or "" when it is neither.
func normaliseScopeType(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "product":
		return "product"
	case "plan":
		return "plan"
	default:
		return ""
	}
}

// dedupeCouponScopes normalises and de-duplicates submitted scopes,
// preserving the first occurrence. The scope table's primary key is
// (coupon_id, scope_type, scope_id), so a duplicate would abort the insert.
func dedupeCouponScopes(in []CouponScopeInput) []CouponScopeInput {
	out := make([]CouponScopeInput, 0, len(in))
	seen := make(map[CouponScopeInput]bool, len(in))
	for _, sc := range in {
		sc = CouponScopeInput{ScopeType: normaliseScopeType(sc.ScopeType), ScopeID: strings.TrimSpace(sc.ScopeID)}
		if sc.ScopeType == "" || sc.ScopeID == "" || seen[sc] {
			continue
		}
		seen[sc] = true
		out = append(out, sc)
	}
	return out
}

// couponFromInput maps a validated definition onto the row it will be
// written as. Currency only means something for a fixed coupon, so the
// other types store NULL.
func couponFromInput(id string, in CouponInput) *model.Coupon {
	next := &model.Coupon{
		ID: id, Code: NormaliseCouponCode(in.Code), Description: strings.TrimSpace(in.Description),
		Type: in.Type, Value: in.Value, TrialDays: in.TrialDays,
		StackGroup: in.StackGroup, Stackable: in.Stackable, Priority: in.Priority,
		AssignedEmail: strings.TrimSpace(in.AssignedEmail), MaxRedemptions: in.MaxRedemptions,
		PerCustomerLimit: in.PerCustomerLimit,
		ValidFrom:        in.ValidFrom, ValidTo: in.ValidTo, Active: in.Active,
	}
	if next.Type == "fixed" {
		next.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	}
	if next.StackGroup == "" {
		next.StackGroup = "general"
	}
	return next
}

// checkCouponScopes proves every scope target exists before a coupon is
// written. Nothing in the schema can enforce it: coupon_scopes.scope_id is
// a plain text column because it points at two different tables.
func checkCouponScopes(ctx context.Context, db bun.IDB, scopes []CouponScopeInput) error {
	scopes = dedupeCouponScopes(scopes)
	ids := map[string][]string{"plan": nil, "product": nil}
	for _, sc := range scopes {
		ids[sc.ScopeType] = append(ids[sc.ScopeType], sc.ScopeID)
	}
	if len(ids["plan"]) > 0 {
		var found []string
		if err := db.NewSelect().Model((*model.Plan)(nil)).Column("id").
			Where("id IN (?)", bun.In(ids["plan"])).Scan(ctx, &found); err != nil {
			return err
		}
		if len(found) != len(ids["plan"]) {
			return invalidCouponf("a scope names a plan that does not exist")
		}
	}
	if len(ids["product"]) > 0 {
		var found []string
		if err := db.NewSelect().Model((*model.Product)(nil)).Column("id").
			Where("id IN (?)", bun.In(ids["product"])).Scan(ctx, &found); err != nil {
			return err
		}
		if len(found) != len(ids["product"]) {
			return invalidCouponf("a scope names a product that does not exist")
		}
	}
	return nil
}

// insertCouponScopes writes the scope rows of a coupon inside the caller's
// transaction.
func insertCouponScopes(ctx context.Context, tx bun.Tx, couponID string, scopes []CouponScopeInput) error {
	for _, sc := range dedupeCouponScopes(scopes) {
		row := &model.CouponScope{
			CouponID: couponID, ScopeType: sc.ScopeType, ScopeID: sc.ScopeID,
		}
		if _, err := tx.NewInsert().Model(row).On("CONFLICT DO NOTHING").Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

// CreateCoupon writes one definition and its scopes.
//
// The insert is explicit SQL because bun turns a zero Go value on a
// `default:` column into SQL DEFAULT: "not stackable", "priority 0" and
// "inactive" are meaningful values an operator can choose, and passing
// them through the ORM would silently store the column defaults instead.
func (s *Store) CreateCoupon(ctx context.Context, in CouponInput) (*model.Coupon, error) {
	if err := validateCouponInput(in); err != nil {
		return nil, err
	}
	id := NewID()
	next := couponFromInput(id, in)
	var currency any
	if next.Currency != "" {
		currency = next.Currency
	}
	err := RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		if err := checkCouponScopes(ctx, tx, in.Scopes); err != nil {
			return err
		}
		_, err := tx.NewRaw(`INSERT INTO coupons
			(id, code, description, type, value, trial_days, currency, stack_group,
			 stackable, priority, assigned_email, max_redemptions, redeemed_count,
			 per_customer_limit, valid_from, valid_to, active, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, now(), now())`,
			next.ID, next.Code, next.Description, next.Type, next.Value, next.TrialDays,
			currency, next.StackGroup, next.Stackable, next.Priority, next.AssignedEmail,
			next.MaxRedemptions, next.PerCustomerLimit, next.ValidFrom, next.ValidTo,
			next.Active).Exec(ctx)
		if err != nil {
			if isUniqueViolationOn(err, couponCodeConstraint) {
				return fmt.Errorf("%w: %s", ErrCouponCodeTaken, next.Code)
			}
			return err
		}
		return insertCouponScopes(ctx, tx, id, in.Scopes)
	})
	if err != nil {
		return nil, err
	}
	return s.GetCoupon(ctx, id)
}

// UpdateCoupon rewrites a definition and replaces its scopes atomically:
// a failure leaves both the old row and the old scopes in place.
func (s *Store) UpdateCoupon(ctx context.Context, id string, in CouponInput) (*model.Coupon, error) {
	if err := validateCouponInput(in); err != nil {
		return nil, err
	}
	next := couponFromInput(id, in)
	var currency any
	if next.Currency != "" {
		currency = next.Currency
	}
	err := RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		if _, err := getCouponIn(ctx, tx, id); err != nil {
			return err
		}
		if err := checkCouponScopes(ctx, tx, in.Scopes); err != nil {
			return err
		}
		_, err := tx.NewUpdate().Model((*model.Coupon)(nil)).
			Set(`code = ?, description = ?, type = ?, value = ?, trial_days = ?,
				currency = ?, stack_group = ?, stackable = ?, priority = ?,
				assigned_email = ?, max_redemptions = ?, per_customer_limit = ?,
				valid_from = ?, valid_to = ?, active = ?, updated_at = now()`,
				next.Code, next.Description, next.Type, next.Value, next.TrialDays,
				currency, next.StackGroup, next.Stackable, next.Priority,
				next.AssignedEmail, next.MaxRedemptions, next.PerCustomerLimit,
				next.ValidFrom, next.ValidTo, next.Active).
			Where("id = ?", id).Exec(ctx)
		if err != nil {
			if isUniqueViolationOn(err, couponCodeConstraint) {
				return fmt.Errorf("%w: %s", ErrCouponCodeTaken, next.Code)
			}
			return err
		}
		if _, err := tx.NewDelete().Model((*model.CouponScope)(nil)).Where("coupon_id = ?", id).Exec(ctx); err != nil {
			return err
		}
		return insertCouponScopes(ctx, tx, id, in.Scopes)
	})
	if err != nil {
		return nil, err
	}
	return s.GetCoupon(ctx, id)
}

// getCouponIn reads one coupon by id on the given connection.
func getCouponIn(ctx context.Context, db bun.IDB, id string) (*model.Coupon, error) {
	c := new(model.Coupon)
	err := db.NewSelect().Model(c).Where("id = ?", id).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCouponNotFound
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// GetCoupon reads one definition by id.
func (s *Store) GetCoupon(ctx context.Context, id string) (*model.Coupon, error) {
	return getCouponIn(ctx, s.DB, id)
}

// FindCouponByCode reads one definition by its normalised code.
func (s *Store) FindCouponByCode(ctx context.Context, code string) (*model.Coupon, error) {
	c := new(model.Coupon)
	err := s.DB.NewSelect().Model(c).Where("code = ?", NormaliseCouponCode(code)).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrCouponNotFound
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// CouponsByCodes reads every definition matching the given codes. Codes
// are normalised by the caller (Quote does) and again here, so a direct
// store call cannot miss a row.
func (s *Store) CouponsByCodes(ctx context.Context, codes []string) ([]*model.Coupon, error) {
	clean := make([]string, 0, len(codes))
	for _, code := range codes {
		if c := NormaliseCouponCode(code); c != "" {
			clean = append(clean, c)
		}
	}
	out := make([]*model.Coupon, 0, len(clean))
	if len(clean) == 0 {
		return out, nil
	}
	if err := s.DB.NewSelect().Model(&out).Where("code IN (?)", bun.In(clean)).Scan(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// CouponScopesFor reads the scope rows of several coupons at once,
// keyed by coupon id. No entry means the coupon covers everything.
func (s *Store) CouponScopesFor(ctx context.Context, couponIDs []string) (map[string][]*model.CouponScope, error) {
	out := make(map[string][]*model.CouponScope, len(couponIDs))
	if len(couponIDs) == 0 {
		return out, nil
	}
	var rows []*model.CouponScope
	if err := s.DB.NewSelect().Model(&rows).
		Where("coupon_id IN (?)", bun.In(couponIDs)).
		Order("coupon_id ASC", "scope_type ASC", "scope_id ASC").
		Scan(ctx); err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.CouponID] = append(out[row.CouponID], row)
	}
	return out, nil
}

// DeleteCoupon removes an untouched coupon and archives a used one.
//
// The redemption ledger references coupons with ON DELETE RESTRICT: a code
// that was ever redeemed must stay readable so marketing attribution and
// the per-customer limit keep working. Such a coupon is deactivated and
// kept, and the caller is told which happened.
func (s *Store) DeleteCoupon(ctx context.Context, id string) (archived bool, err error) {
	err = RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		if _, err := getCouponIn(ctx, tx, id); err != nil {
			return err
		}
		var used bool
		if err := tx.NewRaw("SELECT EXISTS (SELECT 1 FROM coupon_redemptions WHERE coupon_id = ?)", id).Scan(ctx, &used); err != nil {
			return err
		}
		if used {
			archived = true
			_, err := tx.NewUpdate().Model((*model.Coupon)(nil)).
				Set("active = FALSE, updated_at = now()").Where("id = ?", id).Exec(ctx)
			return err
		}
		_, err := tx.NewDelete().Model((*model.Coupon)(nil)).Where("id = ?", id).Exec(ctx)
		return err
	})
	if err != nil {
		return false, err
	}
	return archived, nil
}

// CouponFilter narrows the admin list. Q matches code or description;
// ScopeType (with optional ScopeID) selects coupons that cover a target.
type CouponFilter struct {
	Q          string
	Active     *bool
	StackGroup string
	ScopeType  string
	ScopeID    string
	Page       Page
}

func (f CouponFilter) apply(q *bun.SelectQuery) *bun.SelectQuery {
	if f.Q != "" {
		like := "%" + f.Q + "%"
		q = q.Where("(code ILIKE ? OR description ILIKE ?)", like, like)
	}
	if f.Active != nil {
		q = q.Where("active = ?", *f.Active)
	}
	if f.StackGroup != "" {
		q = q.Where("stack_group = ?", f.StackGroup)
	}
	if f.ScopeType != "" {
		q = q.Where(`EXISTS (SELECT 1 FROM coupon_scopes cs WHERE cs.coupon_id = coupons.id
			AND cs.scope_type = ? AND (cs.scope_id = ? OR ? = ''))`,
			f.ScopeType, f.ScopeID, f.ScopeID)
	}
	return q
}

// ListCoupons returns one page of definitions, newest first.
func (s *Store) ListCoupons(ctx context.Context, f CouponFilter) ([]*model.Coupon, int, error) {
	out := make([]*model.Coupon, 0)
	q := f.apply(s.DB.NewSelect().Model(&out))
	total, err := scanPage(ctx, q.Order("active DESC", "created_at DESC", "code ASC"), f.Page)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// RedemptionInput is one redemption to record: a coupon, the order that
// used it, and the identity the per-customer limit counts against.
type RedemptionInput struct {
	CouponID           string
	CouponCode         string
	OrderID            string
	Email              string
	ExternalCustomerID string
	Type               string
	AmountMinor        int64
	TrialDays          int
}

// RedeemCouponsForOrder records every coupon a settled order used.
//
// It is idempotent twice over: the redemption table is unique on
// (order_id, coupon_code), so a redelivered paid hook cannot write a
// second row, and the counter is only incremented for rows the insert
// actually created. Nothing is consumed at order creation, so an
// abandoned order leaves every counter untouched.
func (s *Store) RedeemCouponsForOrder(ctx context.Context, in []RedemptionInput) error {
	if len(in) == 0 {
		return nil
	}
	return RunInTx(ctx, s.DB, func(ctx context.Context, tx bun.Tx) error {
		for _, r := range in {
			if err := redeemCouponIn(ctx, tx, r); err != nil {
				return err
			}
		}
		return nil
	})
}

// redeemCouponIn records one redemption and bumps its counter. A coupon
// deleted between quote and settlement has no row to count against; the
// order's discount snapshot remains the evidence, so the redemption is
// skipped rather than failing a paid order.
func redeemCouponIn(ctx context.Context, tx bun.Tx, in RedemptionInput) error {
	if strings.TrimSpace(in.CouponID) == "" {
		return nil
	}
	// Lock the counter row first: two settlements of different orders
	// using the same coupon must not lose an increment, and a coupon that
	// is gone must not reach the insert (its FK would abort the whole
	// paid hook).
	var locked string
	err := tx.NewRaw("SELECT id FROM coupons WHERE id = ? FOR UPDATE", in.CouponID).Scan(ctx, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var inserted string
	err = tx.NewRaw(`INSERT INTO coupon_redemptions
			(id, coupon_id, coupon_code, order_id, email, external_customer_id,
			 type, amount_minor, trial_days, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, now())
		ON CONFLICT (order_id, coupon_code) DO NOTHING
		RETURNING id`,
		NewID(), in.CouponID, NormaliseCouponCode(in.CouponCode), in.OrderID,
		strings.ToLower(strings.TrimSpace(in.Email)), strings.TrimSpace(in.ExternalCustomerID),
		in.Type, in.AmountMinor, in.TrialDays).Scan(ctx, &inserted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // already recorded by an earlier delivery
	}
	if err != nil {
		return err
	}
	_, err = tx.NewRaw("UPDATE coupons SET redeemed_count = redeemed_count + 1, updated_at = now() WHERE id = ?", in.CouponID).Exec(ctx)
	return err
}

// CouponRedemptionStats is the per-coupon counter block.
type CouponRedemptionStats struct {
	Count       int   `json:"count"`
	AmountMinor int64 `json:"amount_minor"`
}

// CouponRedemptionStats reads a coupon's redemption count and total
// discount. It counts the ledger rather than trusting redeemed_count,
// which is the number marketing reports on.
func (s *Store) CouponRedemptionStats(ctx context.Context, couponID string) (*CouponRedemptionStats, error) {
	stats := new(CouponRedemptionStats)
	if err := s.DB.NewRaw(`SELECT count(*), coalesce(sum(amount_minor), 0)
		FROM coupon_redemptions WHERE coupon_id = ?`, couponID).
		Scan(ctx, &stats.Count, &stats.AmountMinor); err != nil {
		return nil, err
	}
	return stats, nil
}

// CountCouponRedemptionsFor counts redemptions of one coupon by one
// customer. The identity is external_customer_id when it is given,
// otherwise the lowercased email (spec §3.9), so a coupon cannot leak a
// second redemption to the same buyer under a different spelling.
func (s *Store) CountCouponRedemptionsFor(ctx context.Context, couponID, email, externalID string) (int, error) {
	q := s.DB.NewSelect().Model((*model.CouponRedemption)(nil)).Where("coupon_id = ?", couponID)
	if external := strings.TrimSpace(externalID); external != "" {
		q = q.Where("external_customer_id = ?", external)
	} else {
		q = q.Where("external_customer_id = ''").
			Where("lower(email) = ?", strings.ToLower(strings.TrimSpace(email)))
	}
	return q.Count(ctx)
}

// RedemptionRow is one row of the redemption history, joined with the
// order it belongs to so CSV and JSON readers get the order code and
// currency without a second query.
type RedemptionRow struct {
	ID                 string    `json:"id"`
	CouponID           string    `json:"coupon_id"`
	CouponCode         string    `json:"coupon_code"`
	OrderID            string    `json:"order_id"`
	OrderCode          string    `json:"order_code"`
	Email              string    `json:"email"`
	ExternalCustomerID string    `json:"external_customer_id"`
	Type               string    `json:"type"`
	AmountMinor        int64     `json:"amount_minor"`
	TrialDays          int       `json:"trial_days"`
	Currency           string    `json:"currency"`
	CurrencyExp        int16     `bun:"currency_exponent" json:"currency_exponent"`
	CreatedAt          time.Time `json:"created_at"`
}

// RedemptionFilter narrows the redemption history. CouponCode matches the
// code snapshot on the row, so a deleted coupon's history is still
// findable by the code that was used.
type RedemptionFilter struct {
	CouponID   string
	CouponCode string
	Email      string
	From, To   *time.Time
	Page       Page
}

func (f RedemptionFilter) apply(q *bun.SelectQuery) *bun.SelectQuery {
	if f.CouponID != "" {
		q = q.Where("r.coupon_id = ?", f.CouponID)
	}
	if f.CouponCode != "" {
		q = q.Where("r.coupon_code = ?", NormaliseCouponCode(f.CouponCode))
	}
	if f.Email != "" {
		q = q.Where("lower(r.email) = ?", strings.ToLower(strings.TrimSpace(f.Email)))
	}
	if f.From != nil {
		q = q.Where("r.created_at >= ?", *f.From)
	}
	if f.To != nil {
		// Exclusive upper bound, so consecutive days do not overlap.
		q = q.Where("r.created_at < ?", *f.To)
	}
	return q
}

// redemptionSelect is the join every redemption read starts from.
func (s *Store) redemptionSelect() *bun.SelectQuery {
	return s.DB.NewSelect().
		TableExpr("coupon_redemptions AS r").
		Join("LEFT JOIN payment_orders AS o ON o.id = r.order_id").
		ColumnExpr("r.id, r.coupon_id, r.coupon_code, r.order_id, r.email, " +
			"r.external_customer_id, r.type, r.amount_minor, r.trial_days, r.created_at").
		ColumnExpr("COALESCE(o.code, '') AS order_code, COALESCE(o.currency, '') AS currency, " +
			"COALESCE(o.currency_exponent, 0) AS currency_exponent")
}

// ListRedemptions returns one page of the redemption history, newest
// first.
func (s *Store) ListRedemptions(ctx context.Context, f RedemptionFilter) ([]RedemptionRow, int, error) {
	out := make([]RedemptionRow, 0)
	total, err := f.apply(s.DB.NewSelect().TableExpr("coupon_redemptions AS r")).Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	q := f.apply(s.redemptionSelect())
	if f.Page.Limit > 0 {
		q = q.Limit(f.Page.Limit)
	}
	if f.Page.Offset > 0 {
		q = q.Offset(f.Page.Offset)
	}
	if err := q.Order("r.created_at DESC", "r.id DESC").Scan(ctx, &out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// ListOrderDiscounts reads the coupon snapshot of one order, in the order
// the coupons were applied. The rows outlive the coupons they name.
func (s *Store) ListOrderDiscounts(ctx context.Context, orderID string) ([]*model.PaymentOrderDiscount, error) {
	out := make([]*model.PaymentOrderDiscount, 0)
	if err := s.DB.NewSelect().Model(&out).Where("order_id = ?", orderID).
		Order("priority ASC", "coupon_code ASC").Scan(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// RedemptionsForCoupon is the per-coupon history shortcut the admin route
// uses; it exists so the handler does not build a filter twice.
func (s *Store) RedemptionsForCoupon(ctx context.Context, couponID string, page Page) ([]RedemptionRow, int, error) {
	return s.ListRedemptions(ctx, RedemptionFilter{CouponID: couponID, Page: page})
}
