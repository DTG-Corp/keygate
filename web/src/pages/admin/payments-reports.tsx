import { useQuery } from "@tanstack/react-query"
import { AlertTriangle, Download, Loader2 } from "lucide-react"
import { useMemo, useState } from "react"
import { useSearchParams } from "react-router-dom"
import { showToast } from "@/components/toast"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useI18n } from "@/i18n"
import {
  admin,
  adminReports,
  type ReconciliationReport,
  type ReportFilter,
  type ReportName,
  type RevenueReport,
  type VatReport,
} from "@/lib/api"
import { saveBlob } from "@/lib/download"
import { currencyExponent, formatMoney } from "@/lib/money"
import { formatDateVN } from "@/lib/utils"

const TABS: ReportName[] = ["revenue", "reconciliation", "vat"]

// The report window is a Vietnam calendar day. en-CA formats as
// YYYY-MM-DD, the shape the handler reads as a VN day; using UTC here
// would show the previous day before 07:00 in Vietnam.
function isoDate(daysAgo: number): string {
  const date = new Date(Date.now() - daysAgo * 86_400_000)
  return new Intl.DateTimeFormat("en-CA", { timeZone: "Asia/Ho_Chi_Minh" }).format(date)
}

export default function PaymentReportsPage() {
  const { t } = useI18n()
  const [params, setParams] = useSearchParams()
  const requested = params.get("tab")
  const tab: ReportName = requested === "reconciliation" || requested === "vat" ? requested : "revenue"

  const [from, setFrom] = useState(params.get("from") ?? isoDate(29))
  const [to, setTo] = useState(params.get("to") ?? isoDate(0))
  const [provider, setProvider] = useState(params.get("provider") ?? "all")
  const [product, setProduct] = useState(params.get("product") ?? "all")
  const [plan, setPlan] = useState(params.get("plan") ?? "all")
  const [groupBy, setGroupBy] = useState(params.get("group_by") ?? "period")
  const [period, setPeriod] = useState(params.get("period") ?? "day")

  // Reconciliation refuses product and plan filters (the ledger has no
  // such columns), so they are not sent for that report.
  const filter: ReportFilter = {
    // Bare dates keep the window in Vietnam days on both the JSON and
    // CSV routes; the server expands `to` to the end of that day.
    from: from || undefined,
    to: to || undefined,
    provider: provider === "all" ? undefined : provider,
    product_id: tab === "reconciliation" ? undefined : product === "all" ? undefined : product,
    plan_id: tab === "reconciliation" ? undefined : plan === "all" ? undefined : plan,
    group_by: tab === "revenue" ? groupBy : undefined,
    period: tab === "revenue" || tab === "vat" ? period : undefined,
  }

  const { data: products } = useQuery({
    queryKey: ["admin", "products", "report-labels"],
    queryFn: () => admin.listProducts({ limit: 200 }),
  })
  const { data: plans } = useQuery({
    queryKey: ["admin", "plans", "report-labels"],
    queryFn: () => admin.listPlans({ limit: 200 }),
  })
  const productNames = useMemo(() => new Map((products?.products ?? []).map((p) => [p.id, p.name])), [products])
  const planNames = useMemo(() => new Map((plans?.plans ?? []).map((p) => [p.id, p.name])), [plans])

  const revenue = useQuery({
    queryKey: ["admin", "reports", "revenue", filter],
    queryFn: () => adminReports.revenue(filter),
    enabled: tab === "revenue",
  })
  const reconciliation = useQuery({
    queryKey: ["admin", "reports", "reconciliation", filter],
    queryFn: () => adminReports.reconciliation(filter),
    enabled: tab === "reconciliation",
  })
  const vat = useQuery({
    queryKey: ["admin", "reports", "vat", filter],
    queryFn: () => adminReports.vat(filter),
    enabled: tab === "vat",
  })

  const active = tab === "revenue" ? revenue : tab === "reconciliation" ? reconciliation : vat

  const [downloading, setDownloading] = useState(false)
  const download = async () => {
    setDownloading(true)
    try {
      saveBlob(await adminReports.csv(tab, filter), `${tab}-${from}-${to}.csv`)
    } catch (err) {
      showToast((err as Error).message, "error")
    } finally {
      setDownloading(false)
    }
  }

  const setTab = (next: string) => {
    const search = new URLSearchParams(params)
    search.set("tab", next)
    setParams(search, { replace: true })
  }

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{t("reports.title")}</h1>
          <p className="text-sm text-muted-foreground">{t("reports.subtitle")}</p>
        </div>
        <Button variant="outline" onClick={download} disabled={downloading}>
          {downloading ? <Loader2 className="h-4 w-4 animate-spin" /> : <Download className="h-4 w-4" />}
          {t("reports.download")}
        </Button>
      </div>

      <Tabs value={tab} onValueChange={setTab}>
        <TabsList>
          {TABS.map((name) => (
            <TabsTrigger key={name} value={name}>
              {reportLabel(name, t)}
            </TabsTrigger>
          ))}
        </TabsList>
      </Tabs>

      <Card>
        <CardContent className="grid gap-3 pt-6 sm:grid-cols-2 lg:grid-cols-4">
          <div className="space-y-1.5">
            <Label htmlFor="report-from">{t("common.from")}</Label>
            <Input id="report-from" type="date" value={from} onChange={(e) => setFrom(e.target.value)} />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="report-to">{t("common.to")}</Label>
            <Input id="report-to" type="date" value={to} onChange={(e) => setTo(e.target.value)} />
          </div>
          <div className="space-y-1.5">
            <Label>{t("reports.provider")}</Label>
            <Select value={provider} onValueChange={setProvider}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">{t("common.all")}</SelectItem>
                <SelectItem value="sepay">SePay</SelectItem>
                <SelectItem value="payos">PayOS</SelectItem>
              </SelectContent>
            </Select>
          </div>
          {tab !== "reconciliation" && (
            <>
              <div className="space-y-1.5">
                <Label>{t("reports.product")}</Label>
                <Select value={product} onValueChange={setProduct}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">{t("common.all")}</SelectItem>
                    {(products?.products ?? []).map((row) => (
                      <SelectItem key={row.id} value={row.id}>
                        {row.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label>{t("reports.plan")}</Label>
                <Select value={plan} onValueChange={setPlan}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all">{t("common.all")}</SelectItem>
                    {(plans?.plans ?? []).map((row) => (
                      <SelectItem key={row.id} value={row.id}>
                        {row.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            </>
          )}
          {tab === "revenue" && (
            <div className="space-y-1.5">
              <Label>{t("reports.group_by")}</Label>
              <Select value={groupBy} onValueChange={setGroupBy}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="period">{t("reports.group_period")}</SelectItem>
                  <SelectItem value="provider">{t("reports.group_provider")}</SelectItem>
                  <SelectItem value="plan">{t("reports.group_plan")}</SelectItem>
                  <SelectItem value="product">{t("reports.group_product")}</SelectItem>
                </SelectContent>
              </Select>
            </div>
          )}
          {(tab === "revenue" || tab === "vat") && (
            <div className="space-y-1.5">
              <Label>{t("reports.period")}</Label>
              <Select value={period} onValueChange={setPeriod}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="day">{t("reports.period_day")}</SelectItem>
                  <SelectItem value="month">{t("reports.period_month")}</SelectItem>
                </SelectContent>
              </Select>
            </div>
          )}
          {tab === "reconciliation" && (
            <p className="self-end pb-2 text-xs text-muted-foreground sm:col-span-2">
              {t("reports.reconciliation_provider_only")}
            </p>
          )}
        </CardContent>
      </Card>

      {active.isLoading && <div className="h-48 animate-pulse rounded-lg bg-muted" />}
      {active.error && (
        <p className="rounded-md border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive">
          {t("reports.load_failed")}: {(active.error as Error).message}
        </p>
      )}

      {!active.isLoading && !active.error && (
        <>
          {active.data && (
            <p className="text-xs text-muted-foreground">
              {t("reports.window", {
                // The handler returns UTC instants; show them in the same
                // Vietnam zone it reports in, not sliced as UTC dates.
                from: formatDateVN(active.data.from),
                to: formatDateVN(active.data.to),
                timezone: active.data.timezone,
              })}
            </p>
          )}
          {tab === "revenue" && revenue.data && (
            <RevenueTable report={revenue.data} groupBy={groupBy} productNames={productNames} planNames={planNames} />
          )}
          {tab === "reconciliation" && reconciliation.data && <ReconciliationTable report={reconciliation.data} />}
          {tab === "vat" && vat.data && <VatTable report={vat.data} />}
        </>
      )}
    </div>
  )
}

function RevenueTable({
  report,
  groupBy,
  productNames,
  planNames,
}: {
  report: RevenueReport
  groupBy: string
  productNames: Map<string, string>
  planNames: Map<string, string>
}) {
  const { t } = useI18n()
  const showPeriod = groupBy === "period" || report.rows.some((row) => row.period)
  const showProvider = groupBy === "provider" || report.rows.some((row) => row.provider)

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">{t("reports.summary")}</CardTitle>
        <div className="flex flex-wrap gap-3 pt-2">
          {report.totals.map((total) => (
            <div key={total.currency} className="rounded-lg border px-3 py-2 text-sm">
              <span className="font-medium">{total.currency}</span>
              <span className="ml-2 tabular-nums">
                {formatMoney(total.total_minor, total.currency, total.currency_exponent)}
              </span>
              <span className="ml-2 text-xs text-muted-foreground">
                {t("reports.orders")}: {total.orders}
              </span>
            </div>
          ))}
          {report.totals.length === 0 && <p className="text-sm text-muted-foreground">{t("reports.empty")}</p>}
        </div>
      </CardHeader>
      <CardContent>
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-xs text-muted-foreground">
                {showPeriod && <th className="p-2">{t("reports.group_period")}</th>}
                {showProvider && <th className="p-2">{t("reports.provider")}</th>}
                {groupBy === "plan" && <th className="p-2">{t("reports.plan")}</th>}
                {groupBy === "product" && <th className="p-2">{t("reports.product")}</th>}
                <th className="p-2 text-right">{t("reports.orders")}</th>
                <th className="p-2 text-right">{t("reports.subtotal")}</th>
                <th className="p-2 text-right">{t("reports.discount")}</th>
                <th className="p-2 text-right">{t("reports.tax")}</th>
                <th className="p-2 text-right">{t("reports.total")}</th>
              </tr>
            </thead>
            <tbody>
              {report.rows.length === 0 && (
                <tr>
                  <td colSpan={9} className="p-4 text-center text-muted-foreground">
                    {t("reports.empty")}
                  </td>
                </tr>
              )}
              {report.rows.map((row) => (
                <tr
                  key={`${row.period}|${row.provider}|${row.plan_id}|${row.product_id}|${row.currency}`}
                  className="border-b"
                >
                  {showPeriod && <td className="p-2 tabular-nums">{row.period ?? "—"}</td>}
                  {showProvider && <td className="p-2">{row.provider ?? "—"}</td>}
                  {groupBy === "plan" && (
                    <td className="p-2">{row.plan_name || planNames.get(row.plan_id ?? "") || row.plan_id || "—"}</td>
                  )}
                  {groupBy === "product" && (
                    <td className="p-2">
                      {row.product_name || productNames.get(row.product_id ?? "") || row.product_id || "—"}
                    </td>
                  )}
                  <td className="p-2 text-right tabular-nums">{row.orders}</td>
                  <td className="p-2 text-right tabular-nums">
                    {formatMoney(row.subtotal_minor, row.currency, row.currency_exponent)}
                  </td>
                  <td className="p-2 text-right tabular-nums">
                    {formatMoney(row.discount_minor, row.currency, row.currency_exponent)}
                  </td>
                  <td className="p-2 text-right tabular-nums">
                    {formatMoney(row.tax_minor, row.currency, row.currency_exponent)}
                  </td>
                  <td className="p-2 text-right font-medium tabular-nums">
                    {formatMoney(row.total_minor, row.currency, row.currency_exponent)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </CardContent>
    </Card>
  )
}

function ReconciliationTable({ report }: { report: ReconciliationReport }) {
  const { t } = useI18n()
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">{t("reports.summary")}</CardTitle>
        <div className="flex flex-wrap items-center gap-3 pt-2">
          {report.totals.map((total) => (
            <div key={total.currency} className="rounded-lg border px-3 py-2 text-sm">
              <span className="font-medium">{total.currency}</span>
              <span className="ml-2 text-xs text-muted-foreground">
                {t("reports.orders_minor")}:{" "}
                {formatMoney(total.orders_minor, total.currency, currencyExponent(total.currency))}
              </span>
              <span className="ml-2 text-xs text-muted-foreground">
                {t("reports.transactions_minor")}:{" "}
                {formatMoney(total.transactions_minor, total.currency, currencyExponent(total.currency))}
              </span>
            </div>
          ))}
          {/* Drift is only meaningful per currency: the handler's scalar
              total mixes VND and USD minor units. */}
          {report.totals.map((total) => (
            <Badge
              key={`drift-${total.currency}`}
              variant={total.drift_minor === 0 ? "secondary" : "destructive"}
              className="gap-1"
            >
              {total.drift_minor !== 0 && <AlertTriangle className="h-3 w-3" />}
              {total.drift_minor === 0
                ? t("reports.healthy")
                : t("reports.drift_alert", {
                    amount: formatMoney(total.drift_minor, total.currency, currencyExponent(total.currency)),
                  })}
            </Badge>
          ))}
        </div>
      </CardHeader>
      <CardContent>
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-xs text-muted-foreground">
                <th className="p-2">{t("reports.period_day")}</th>
                <th className="p-2">{t("reports.provider")}</th>
                <th className="p-2">{t("reports.currency")}</th>
                <th className="p-2 text-right">{t("reports.orders_count")}</th>
                <th className="p-2 text-right">{t("reports.transactions_count")}</th>
                <th className="p-2 text-right">{t("reports.orders_minor")}</th>
                <th className="p-2 text-right">{t("reports.transactions_minor")}</th>
                <th className="p-2 text-right">{t("reports.drift")}</th>
              </tr>
            </thead>
            <tbody>
              {report.rows.length === 0 && (
                <tr>
                  <td colSpan={8} className="p-4 text-center text-muted-foreground">
                    {t("reports.empty")}
                  </td>
                </tr>
              )}
              {report.rows.map((row) => (
                <tr key={`${row.day}|${row.provider}|${row.currency}`} className="border-b">
                  <td className="p-2 tabular-nums">{row.day}</td>
                  <td className="p-2">{row.provider}</td>
                  <td className="p-2">{row.currency}</td>
                  <td className="p-2 text-right tabular-nums">{row.orders_count}</td>
                  <td className="p-2 text-right tabular-nums">{row.transactions_count}</td>
                  <td className="p-2 text-right tabular-nums">
                    {formatMoney(row.orders_minor, row.currency, currencyExponent(row.currency))}
                  </td>
                  <td className="p-2 text-right tabular-nums">
                    {formatMoney(row.transactions_minor, row.currency, currencyExponent(row.currency))}
                  </td>
                  <td
                    className={`p-2 text-right font-medium tabular-nums ${row.drift_minor !== 0 ? "text-destructive" : ""}`}
                  >
                    {formatMoney(row.drift_minor, row.currency, currencyExponent(row.currency))}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </CardContent>
    </Card>
  )
}

function VatTable({ report }: { report: VatReport }) {
  const { t } = useI18n()
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">{t("reports.summary")}</CardTitle>
        <div className="flex flex-wrap gap-3 pt-2">
          {report.totals.map((total) => (
            <div key={total.currency} className="rounded-lg border px-3 py-2 text-sm">
              <span className="font-medium">{total.currency}</span>
              <span className="ml-2 tabular-nums">
                {t("reports.taxable_base")}:{" "}
                {formatMoney(total.taxable_base_minor, total.currency, currencyExponent(total.currency))}
              </span>
              <span className="ml-2 tabular-nums">
                {t("reports.tax")}: {formatMoney(total.tax_minor, total.currency, currencyExponent(total.currency))}
              </span>
            </div>
          ))}
          {report.totals.length === 0 && <p className="text-sm text-muted-foreground">{t("reports.empty")}</p>}
        </div>
      </CardHeader>
      <CardContent>
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-xs text-muted-foreground">
                <th className="p-2">{t("reports.group_period")}</th>
                <th className="p-2">{t("reports.currency")}</th>
                <th className="p-2 text-right">{t("reports.tax_rate")}</th>
                <th className="p-2 text-right">{t("reports.orders")}</th>
                <th className="p-2 text-right">{t("reports.taxable_base")}</th>
                <th className="p-2 text-right">{t("reports.tax")}</th>
                <th className="p-2 text-right">{t("reports.total")}</th>
              </tr>
            </thead>
            <tbody>
              {report.rows.length === 0 && (
                <tr>
                  <td colSpan={7} className="p-4 text-center text-muted-foreground">
                    {t("reports.empty")}
                  </td>
                </tr>
              )}
              {report.rows.map((row) => (
                <tr key={`${row.period}|${row.currency}|${row.tax_rate_bp}`} className="border-b">
                  <td className="p-2 tabular-nums">{row.period}</td>
                  <td className="p-2">{row.currency}</td>
                  <td className="p-2 text-right tabular-nums">{(row.tax_rate_bp / 100).toLocaleString("vi-VN")}%</td>
                  <td className="p-2 text-right tabular-nums">{row.orders}</td>
                  <td className="p-2 text-right tabular-nums">
                    {formatMoney(row.taxable_base_minor, row.currency, row.currency_exponent)}
                  </td>
                  <td className="p-2 text-right tabular-nums">
                    {formatMoney(row.tax_minor, row.currency, row.currency_exponent)}
                  </td>
                  <td className="p-2 text-right font-medium tabular-nums">
                    {formatMoney(row.total_minor, row.currency, row.currency_exponent)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </CardContent>
    </Card>
  )
}

function reportLabel(name: ReportName, t: ReturnType<typeof useI18n>["t"]): string {
  if (name === "reconciliation") return t("reports.reconciliation")
  if (name === "vat") return t("reports.vat")
  return t("reports.revenue")
}
