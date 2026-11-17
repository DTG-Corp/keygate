package handler

import (
	"encoding/csv"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/dtg-corp/dsoft/internal/store"
	"github.com/dtg-corp/dsoft/pkg/response"
)

// ReportsAdminHandler serves the three payment reports of spec §6.3:
// revenue, daily reconciliation and VAT. They are admin-only: a report
// sums every product's money, so a product-bound merchant key has no
// business reading one.
type ReportsAdminHandler struct {
	Store *store.Store
}

// RegisterReportsAdminRoutes mounts the report routes on g, which the
// server passes as the /api/v1/admin group.
func RegisterReportsAdminRoutes(g *gin.RouterGroup, h *ReportsAdminHandler) {
	g.GET("/payments/reports/revenue", h.Revenue)
	g.GET("/payments/reports/reconciliation", h.Reconciliation)
	g.GET("/payments/reports/vat", h.VAT)
}

// reportRequest is the parsed query shared by the three endpoints.
type reportRequest struct {
	From, To  time.Time
	Period    string
	GroupBy   string
	Provider  string
	ProductID string
	PlanID    string
	CSV       bool
}

// window reads from/to/format. The window is half-open, defaults to the
// last 30 days, and refuses a backwards or empty range before any SQL
// runs. A bare date on `to` includes that whole VN day.
func reportWindow(c *gin.Context) (reportRequest, bool) {
	req := reportRequest{}
	to, err := parseTimeParam(c.Query("to"), true)
	if err != nil {
		response.BadRequest(c, "to must be RFC3339 or YYYY-MM-DD")
		return req, false
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	from, err := parseTimeParam(c.Query("from"), false)
	if err != nil {
		response.BadRequest(c, "from must be RFC3339 or YYYY-MM-DD")
		return req, false
	}
	if from.IsZero() {
		from = to.AddDate(0, 0, -30)
	}
	if !to.After(from) {
		response.BadRequest(c, "to must be after from")
		return req, false
	}
	switch strings.ToLower(strings.TrimSpace(c.Query("format"))) {
	case "", "json":
	case "csv":
		req.CSV = true
	default:
		response.BadRequest(c, "format must be json or csv")
		return req, false
	}
	req.From, req.To = from, to
	req.Provider = strings.TrimSpace(c.Query("provider"))
	req.ProductID = strings.TrimSpace(c.Query("product_id"))
	req.PlanID = strings.TrimSpace(c.Query("plan_id"))
	return req, true
}

// reportPeriod validates ?period=, defaulting to day.
func reportPeriod(c *gin.Context) (string, bool) {
	switch p := strings.ToLower(strings.TrimSpace(c.Query("period"))); p {
	case "":
		return store.ReportPeriodDay, true
	case store.ReportPeriodDay, store.ReportPeriodMonth:
		return p, true
	default:
		response.BadRequest(c, "period must be day or month")
		return "", false
	}
}

func reportMetadata(req reportRequest) gin.H {
	return gin.H{
		"from": req.From.UTC().Format(time.RFC3339), "to": req.To.UTC().Format(time.RFC3339),
		"timezone": "Asia/Ho_Chi_Minh",
	}
}

// ─── revenue ───

func (h *ReportsAdminHandler) Revenue(c *gin.Context) {
	req, ok := reportWindow(c)
	if !ok {
		return
	}
	groupBy := strings.ToLower(strings.TrimSpace(c.Query("group_by")))
	if groupBy == "" {
		groupBy = store.ReportGroupPeriod
	}
	switch groupBy {
	case store.ReportGroupPeriod, store.ReportGroupProvider, store.ReportGroupPlan, store.ReportGroupProduct:
	default:
		response.BadRequest(c, "group_by must be period, provider, plan or product")
		return
	}
	period, ok := reportPeriod(c)
	if !ok {
		return
	}
	rows, err := h.Store.RevenueReport(c, store.ReportFilter{
		From: req.From, To: req.To, Period: period, GroupBy: groupBy,
		Provider: req.Provider, ProductID: req.ProductID, PlanID: req.PlanID,
	})
	if err != nil {
		response.Internal(c, err)
		return
	}
	if req.CSV {
		writeRevenueCSV(c, rows)
		return
	}
	out := reportMetadata(req)
	out["group_by"] = groupBy
	out["period"] = period
	out["rows"] = rows
	out["totals"] = revenueTotals(rows)
	response.OK(c, out)
}

// revenueTotals sums a report per currency. Cross-currency totals are
// deliberately not offered: adding VND and USD minor units would be a
// number that means nothing.
func revenueTotals(rows []store.RevenueRow) []gin.H {
	type bucket struct {
		exp                            int16
		orders                         int
		subtotal, discount, tax, total int64
	}
	byCurrency := map[string]*bucket{}
	for _, r := range rows {
		b := byCurrency[r.Currency]
		if b == nil {
			b = &bucket{exp: r.CurrencyExponent}
			byCurrency[r.Currency] = b
		}
		b.orders += r.Orders
		b.subtotal += r.SubtotalMinor
		b.discount += r.DiscountMinor
		b.tax += r.TaxMinor
		b.total += r.TotalMinor
	}
	out := make([]gin.H, 0, len(byCurrency))
	for currency, b := range byCurrency {
		out = append(out, gin.H{
			"currency": currency, "currency_exponent": b.exp, "orders": b.orders,
			"subtotal_minor": b.subtotal, "discount_minor": b.discount,
			"tax_minor": b.tax, "total_minor": b.total,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["currency"].(string) < out[j]["currency"].(string) })
	return out
}

func writeRevenueCSV(c *gin.Context, rows []store.RevenueRow) {
	setCSVHeaders(c, "revenue")
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{
		"period", "provider", "plan_id", "plan_name", "product_id", "product_name",
		"currency", "currency_exponent", "orders", "subtotal_minor", "discount_minor",
		"tax_minor", "total_minor",
	})
	for _, r := range rows {
		_ = w.Write(csvSafeRow([]string{
			r.Period, r.Provider, r.PlanID, r.PlanName, r.ProductID, r.ProductName,
			r.Currency, strconv.Itoa(int(r.CurrencyExponent)), strconv.Itoa(r.Orders),
			strconv.FormatInt(r.SubtotalMinor, 10), strconv.FormatInt(r.DiscountMinor, 10),
			strconv.FormatInt(r.TaxMinor, 10), strconv.FormatInt(r.TotalMinor, 10),
		}))
	}
	w.Flush()
}

// ─── reconciliation ───

func (h *ReportsAdminHandler) Reconciliation(c *gin.Context) {
	req, ok := reportWindow(c)
	if !ok {
		return
	}
	// Transfers have no product or plan; those filters would silently
	// skew the drift, so they are refused instead.
	if req.ProductID != "" || req.PlanID != "" {
		response.BadRequest(c, "reconciliation filters by provider only")
		return
	}
	rows, err := h.Store.ReconciliationReport(c, store.ReportFilter{
		From: req.From, To: req.To, Provider: req.Provider,
	})
	if err != nil {
		response.Internal(c, err)
		return
	}
	if req.CSV {
		writeReconciliationCSV(c, rows)
		return
	}
	out := reportMetadata(req)
	out["rows"] = rows
	out["totals"] = reconciliationTotals(rows)
	out["drift_minor"] = totalDrift(rows)
	response.OK(c, out)
}

func reconciliationTotals(rows []store.ReconRow) []gin.H {
	type bucket struct {
		orders, transactions                  int
		ordersMinor, transactionsMinor, drift int64
	}
	byCurrency := map[string]*bucket{}
	for _, r := range rows {
		b := byCurrency[r.Currency]
		if b == nil {
			b = &bucket{}
			byCurrency[r.Currency] = b
		}
		b.orders += r.OrdersCount
		b.transactions += r.TransactionsCount
		b.ordersMinor += r.OrdersMinor
		b.transactionsMinor += r.TransactionsMinor
		b.drift += r.DriftMinor
	}
	out := make([]gin.H, 0, len(byCurrency))
	for currency, b := range byCurrency {
		out = append(out, gin.H{
			"currency": currency, "orders_count": b.orders,
			"transactions_count": b.transactions, "orders_minor": b.ordersMinor,
			"transactions_minor": b.transactionsMinor, "drift_minor": b.drift,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["currency"].(string) < out[j]["currency"].(string) })
	return out
}

// totalDrift is the whole window's drift, for the one number an
// operator checks first. Without a provider filter it mixes providers,
// so the per-currency totals stay the authoritative shape.
func totalDrift(rows []store.ReconRow) int64 {
	var sum int64
	for _, r := range rows {
		sum += r.DriftMinor
	}
	return sum
}

func writeReconciliationCSV(c *gin.Context, rows []store.ReconRow) {
	setCSVHeaders(c, "reconciliation")
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{
		"day", "provider", "currency", "orders_count", "transactions_count",
		"orders_minor", "transactions_minor", "drift_minor",
	})
	for _, r := range rows {
		_ = w.Write(csvSafeRow([]string{
			r.Day, r.Provider, r.Currency, strconv.Itoa(r.OrdersCount),
			strconv.Itoa(r.TransactionsCount), strconv.FormatInt(r.OrdersMinor, 10),
			strconv.FormatInt(r.TransactionsMinor, 10), strconv.FormatInt(r.DriftMinor, 10),
		}))
	}
	w.Flush()
}

// ─── VAT ───

func (h *ReportsAdminHandler) VAT(c *gin.Context) {
	req, ok := reportWindow(c)
	if !ok {
		return
	}
	period, ok := reportPeriod(c)
	if !ok {
		return
	}
	rows, err := h.Store.VATReport(c, store.ReportFilter{
		From: req.From, To: req.To, Period: period,
		Provider: req.Provider, ProductID: req.ProductID, PlanID: req.PlanID,
	})
	if err != nil {
		response.Internal(c, err)
		return
	}
	if req.CSV {
		writeVATCSV(c, rows)
		return
	}
	out := reportMetadata(req)
	out["period"] = period
	out["rows"] = rows
	out["totals"] = vatTotals(rows)
	response.OK(c, out)
}

func vatTotals(rows []store.VATRow) []gin.H {
	type bucket struct {
		exp              int16
		orders           int
		base, tax, total int64
	}
	byCurrency := map[string]*bucket{}
	for _, r := range rows {
		b := byCurrency[r.Currency]
		if b == nil {
			b = &bucket{exp: r.CurrencyExponent}
			byCurrency[r.Currency] = b
		}
		b.orders += r.Orders
		b.base += r.TaxableBaseMinor
		b.tax += r.TaxMinor
		b.total += r.TotalMinor
	}
	out := make([]gin.H, 0, len(byCurrency))
	for currency, b := range byCurrency {
		out = append(out, gin.H{
			"currency": currency, "currency_exponent": b.exp, "orders": b.orders,
			"taxable_base_minor": b.base, "tax_minor": b.tax, "total_minor": b.total,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["currency"].(string) < out[j]["currency"].(string) })
	return out
}

func writeVATCSV(c *gin.Context, rows []store.VATRow) {
	setCSVHeaders(c, "vat")
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{
		"period", "currency", "currency_exponent", "tax_rate_bp", "orders",
		"taxable_base_minor", "tax_minor", "total_minor",
	})
	for _, r := range rows {
		_ = w.Write(csvSafeRow([]string{
			r.Period, r.Currency, strconv.Itoa(int(r.CurrencyExponent)),
			strconv.Itoa(r.TaxRateBP), strconv.Itoa(r.Orders),
			strconv.FormatInt(r.TaxableBaseMinor, 10), strconv.FormatInt(r.TaxMinor, 10),
			strconv.FormatInt(r.TotalMinor, 10),
		}))
	}
	w.Flush()
}

// setCSVHeaders marks the body as a download. The filename carries the
// VN date so two exports of the same report do not overwrite each other.
func setCSVHeaders(c *gin.Context, name string) {
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition",
		`attachment; filename="`+name+"-"+time.Now().In(vnZone).Format("2006-01-02")+`.csv"`)
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusOK)
}
