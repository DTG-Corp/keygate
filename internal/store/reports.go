package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Reports aggregate the payment ledger. They are read-only: nothing here
// writes, and every amount is a BIGINT sum of the minor units already
// stored, so a report can never disagree with an order by rounding.
//
// All three bucket by Ho Chi Minh City calendar days/months. A VN day
// starts at 17:00 UTC the day before, which is exactly the mistake a
// report written on UTC would make and no order-level test would catch.
const (
	ReportGroupPeriod   = "period"
	ReportGroupProvider = "provider"
	ReportGroupPlan     = "plan"
	ReportGroupProduct  = "product"

	ReportPeriodDay   = "day"
	ReportPeriodMonth = "month"
)

// reportPaidStatuses is the set of orders that count as collected. A
// refunded order is included: the money arrived, and the refund is its
// own ledger row (the revenue report is about collections, not net
// revenue after refunds — that netting is a later module's concern).
const reportPaidStatuses = `('paid', 'partially_refunded', 'refunded')`

// ReportFilter is the window and grouping shared by the three reports.
// From/To are half-open: From is included, To is not, so adjacent
// windows cannot count an order twice.
type ReportFilter struct {
	From, To time.Time
	// Period is day or month; it only matters when GroupBy is period.
	Period string
	// GroupBy is period, provider, plan or product.
	GroupBy string
	// Optional narrowing.
	Provider, ProductID, PlanID string
}

// normalized fills the defaults and refuses an unusable window. The
// handler validates first so the client gets a 400; the store validates
// again so a direct caller cannot run a report over an unbounded range.
func (f ReportFilter) normalized() (ReportFilter, error) {
	if f.From.IsZero() || f.To.IsZero() || !f.To.After(f.From) {
		return f, errors.New("report window must be a non-empty range")
	}
	switch f.Period {
	case "":
		f.Period = ReportPeriodDay
	case ReportPeriodDay, ReportPeriodMonth:
	default:
		return f, fmt.Errorf("unknown report period %q", f.Period)
	}
	switch f.GroupBy {
	case "":
		f.GroupBy = ReportGroupPeriod
	case ReportGroupPeriod, ReportGroupProvider, ReportGroupPlan, ReportGroupProduct:
	default:
		return f, fmt.Errorf("unknown report grouping %q", f.GroupBy)
	}
	return f, nil
}

// reportPeriodExpr renders a timestamp column as a calendar bucket in
// Ho Chi Minh City time. period is validated before this is called, so
// it is one of two literals and never client input.
func reportPeriodExpr(column, period string) string {
	format := "'YYYY-MM-DD'"
	if period == ReportPeriodMonth {
		format = "'YYYY-MM'"
	}
	return fmt.Sprintf("to_char(date_trunc('%s', %s AT TIME ZONE 'Asia/Ho_Chi_Minh'), %s)",
		period, column, format)
}

// RevenueRow is one revenue bucket. Only the fields of the chosen
// grouping are populated (plus currency, which is always a dimension:
// summing VND and USD would be meaningless).
type RevenueRow struct {
	Period           string `bun:"period" json:"period,omitempty"`
	Provider         string `bun:"provider" json:"provider,omitempty"`
	PlanID           string `bun:"plan_id" json:"plan_id,omitempty"`
	PlanName         string `bun:"plan_name" json:"plan_name,omitempty"`
	ProductID        string `bun:"product_id" json:"product_id,omitempty"`
	ProductName      string `bun:"product_name" json:"product_name,omitempty"`
	Currency         string `bun:"currency" json:"currency"`
	CurrencyExponent int16  `bun:"currency_exponent" json:"currency_exponent"`
	Orders           int    `bun:"orders" json:"orders"`
	SubtotalMinor    int64  `bun:"subtotal_minor" json:"subtotal_minor"`
	DiscountMinor    int64  `bun:"discount_minor" json:"discount_minor"`
	TaxMinor         int64  `bun:"tax_minor" json:"tax_minor"`
	TotalMinor       int64  `bun:"total_minor" json:"total_minor"`
}

// RevenueReport sums collected orders over the window, grouped by the
// filter's dimension.
func (s *Store) RevenueReport(ctx context.Context, f ReportFilter) ([]RevenueRow, error) {
	f, err := f.normalized()
	if err != nil {
		return nil, err
	}
	var dims, group, order string
	switch f.GroupBy {
	case ReportGroupProvider:
		dims = `'' AS period, o.provider AS provider, '' AS plan_id, '' AS plan_name, '' AS product_id, '' AS product_name`
		group = `o.provider, o.currency, o.currency_exponent`
		order = `o.provider, o.currency`
	case ReportGroupPlan:
		dims = `'' AS period, '' AS provider, o.plan_id AS plan_id, COALESCE(p.name, '') AS plan_name,
		        o.product_id AS product_id, COALESCE(pr.name, '') AS product_name`
		group = `o.plan_id, p.name, o.product_id, pr.name, o.currency, o.currency_exponent`
		order = `pr.name, p.name, o.currency`
	case ReportGroupProduct:
		dims = `'' AS period, '' AS provider, '' AS plan_id, '' AS plan_name,
		        o.product_id AS product_id, COALESCE(pr.name, '') AS product_name`
		group = `o.product_id, pr.name, o.currency, o.currency_exponent`
		order = `pr.name, o.currency`
	default:
		dims = reportPeriodExpr("o.paid_at", f.Period) + ` AS period,
		        '' AS provider, '' AS plan_id, '' AS plan_name, '' AS product_id, '' AS product_name`
		group = `1, o.currency, o.currency_exponent`
		order = `1, o.currency`
	}
	where := `o.status IN ` + reportPaidStatuses + ` AND o.paid_at >= ? AND o.paid_at < ?`
	args := []any{f.From.UTC(), f.To.UTC()}
	where, args = appendReportNarrowing(where, args, "o", f)
	sql := `SELECT ` + dims + `,
	        o.currency AS currency, o.currency_exponent AS currency_exponent,
	        count(*) AS orders,
	        COALESCE(sum(o.subtotal_minor), 0)::bigint AS subtotal_minor,
	        COALESCE(sum(o.discount_minor), 0)::bigint AS discount_minor,
	        COALESCE(sum(o.tax_minor), 0)::bigint AS tax_minor,
	        COALESCE(sum(o.total_minor), 0)::bigint AS total_minor
	 FROM payment_orders o
	 LEFT JOIN plans p ON p.id = o.plan_id
	 LEFT JOIN products pr ON pr.id = o.product_id
	 WHERE ` + where + `
	 GROUP BY ` + group + `
	 ORDER BY ` + order
	rows := make([]RevenueRow, 0)
	if err := s.DB.NewRaw(sql, args...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("revenue report: %w", err)
	}
	return rows, nil
}

// ReconRow is one (day, provider, currency) reconciliation line. A day
// present on only one side still appears: the missing side is zero, and
// the drift shows the gap.
type ReconRow struct {
	Day               string `bun:"day" json:"day"`
	Provider          string `bun:"provider" json:"provider"`
	Currency          string `bun:"currency" json:"currency"`
	OrdersCount       int    `bun:"orders_count" json:"orders_count"`
	TransactionsCount int    `bun:"transactions_count" json:"transactions_count"`
	OrdersMinor       int64  `bun:"orders_minor" json:"orders_minor"`
	TransactionsMinor int64  `bun:"transactions_minor" json:"transactions_minor"`
	DriftMinor        int64  `bun:"drift_minor" json:"drift_minor"`
}

// ReconciliationReport compares what the orders say was collected with
// what matched incoming transactions say arrived, per VN day, provider
// and currency. drift_minor is orders − transactions: zero is healthy,
// a positive number is money the ledger claims but no transaction
// evidences (or a settlement written without one), and a negative
// number is money that arrived without an order to hang it on.
func (s *Store) ReconciliationReport(ctx context.Context, f ReportFilter) ([]ReconRow, error) {
	f, err := f.normalized()
	if err != nil {
		return nil, err
	}
	// A transfer carries neither a product nor a plan. Narrowing only
	// the order side would count a legitimate transfer as drift, so this
	// report is provider/day/currency only; the handler refuses the
	// other filters rather than ignoring them.
	f.ProductID, f.PlanID = "", ""
	ordersWhere := `status IN ` + reportPaidStatuses + ` AND paid_at >= ? AND paid_at < ?`
	ordersArgs := []any{f.From.UTC(), f.To.UTC()}
	ordersWhere, ordersArgs = appendReportNarrowing(ordersWhere, ordersArgs, "", f)

	txWhere := `direction = 'in' AND status = 'matched' AND occurred_at >= ? AND occurred_at < ?`
	txArgs := []any{f.From.UTC(), f.To.UTC()}
	txWhere, txArgs = appendReportNarrowing(txWhere, txArgs, "", f)

	sql := `WITH o AS (
	    SELECT ` + reportPeriodExpr("paid_at", ReportPeriodDay) + ` AS day,
	           provider, currency,
	           count(*) AS n, COALESCE(sum(total_minor), 0)::bigint AS amount
	    FROM payment_orders WHERE ` + ordersWhere + `
	    GROUP BY 1, provider, currency
	), t AS (
	    SELECT ` + reportPeriodExpr("occurred_at", ReportPeriodDay) + ` AS day,
	           provider, currency,
	           count(*) AS n, COALESCE(sum(amount_minor), 0)::bigint AS amount
	    FROM payment_transactions WHERE ` + txWhere + `
	    GROUP BY 1, provider, currency
	)
	SELECT COALESCE(o.day, t.day) AS day,
	       COALESCE(o.provider, t.provider) AS provider,
	       COALESCE(o.currency, t.currency) AS currency,
	       COALESCE(o.n, 0) AS orders_count,
	       COALESCE(t.n, 0) AS transactions_count,
	       COALESCE(o.amount, 0)::bigint AS orders_minor,
	       COALESCE(t.amount, 0)::bigint AS transactions_minor,
	       (COALESCE(o.amount, 0) - COALESCE(t.amount, 0))::bigint AS drift_minor
	FROM o FULL OUTER JOIN t USING (day, provider, currency)
	ORDER BY day, provider, currency`
	args := append(append([]any{}, ordersArgs...), txArgs...)
	rows := make([]ReconRow, 0)
	if err := s.DB.NewRaw(sql, args...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("reconciliation report: %w", err)
	}
	return rows, nil
}

// VATRow is one VAT bucket: taxable base (total minus VAT), VAT, gross,
// and the order count behind them.
type VATRow struct {
	Period           string `bun:"period" json:"period"`
	Currency         string `bun:"currency" json:"currency"`
	CurrencyExponent int16  `bun:"currency_exponent" json:"currency_exponent"`
	TaxRateBP        int    `bun:"tax_rate_bp" json:"tax_rate_bp"`
	Orders           int    `bun:"orders" json:"orders"`
	TaxableBaseMinor int64  `bun:"taxable_base_minor" json:"taxable_base_minor"`
	TaxMinor         int64  `bun:"tax_minor" json:"tax_minor"`
	TotalMinor       int64  `bun:"total_minor" json:"total_minor"`
}

// VATReport sums VAT per period, rate and currency. The taxable base is
// total − tax, which is correct whether the price included tax or not:
// orders store the VAT amount that was applied either way, and the total
// always includes it.
func (s *Store) VATReport(ctx context.Context, f ReportFilter) ([]VATRow, error) {
	f, err := f.normalized()
	if err != nil {
		return nil, err
	}
	where := `o.status IN ` + reportPaidStatuses + ` AND o.paid_at >= ? AND o.paid_at < ?`
	args := []any{f.From.UTC(), f.To.UTC()}
	where, args = appendReportNarrowing(where, args, "o", f)
	sql := `SELECT ` + reportPeriodExpr("o.paid_at", f.Period) + ` AS period,
	        o.currency AS currency, o.currency_exponent AS currency_exponent,
	        o.tax_rate_bp AS tax_rate_bp,
	        count(*) AS orders,
	        COALESCE(sum(o.total_minor - o.tax_minor), 0)::bigint AS taxable_base_minor,
	        COALESCE(sum(o.tax_minor), 0)::bigint AS tax_minor,
	        COALESCE(sum(o.total_minor), 0)::bigint AS total_minor
	 FROM payment_orders o
	 WHERE ` + where + `
	 GROUP BY 1, o.currency, o.currency_exponent, o.tax_rate_bp
	 ORDER BY 1, o.currency, o.tax_rate_bp`
	rows := make([]VATRow, 0)
	if err := s.DB.NewRaw(sql, args...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("vat report: %w", err)
	}
	return rows, nil
}

// appendReportNarrowing adds the optional provider/product/plan filters
// to a report WHERE clause. The prefix is "" for a subquery whose table
// is unaliased and "o" for the revenue/VAT queries; the column names are
// the report's own, not the caller's, so nothing the client typed is
// spliced into SQL.
func appendReportNarrowing(where string, args []any, prefix string, f ReportFilter) (string, []any) {
	col := func(name string) string {
		if prefix == "" {
			return name
		}
		return prefix + "." + name
	}
	if f.Provider != "" {
		where += ` AND ` + col("provider") + ` = ?`
		args = append(args, f.Provider)
	}
	if f.ProductID != "" {
		where += ` AND ` + col("product_id") + ` = ?`
		args = append(args, f.ProductID)
	}
	if f.PlanID != "" {
		where += ` AND ` + col("plan_id") + ` = ?`
		args = append(args, f.PlanID)
	}
	return where, args
}
