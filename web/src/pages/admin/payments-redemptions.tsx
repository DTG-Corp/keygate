import { useQuery } from "@tanstack/react-query"
import { Download, Loader2, Search } from "lucide-react"
import { useState } from "react"
import { Link, useSearchParams } from "react-router-dom"
import { CopyButton } from "@/components/copy-button"
import { showToast } from "@/components/toast"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import {
  DataTable,
  DataTableBody,
  DataTableCell,
  DataTableEmpty,
  DataTableHead,
  DataTableHeader,
  DataTablePagination,
  DataTableRow,
  useServerPagination,
} from "@/components/ui/data-table"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { useI18n } from "@/i18n"
import { adminRedemptions } from "@/lib/api"
import { saveBlob } from "@/lib/download"
import { formatMoney } from "@/lib/money"
import { useDebounced } from "@/lib/use-debounced"
import { formatDateVN } from "@/lib/utils"

export default function PaymentRedemptionsPage() {
  const { t } = useI18n()
  const [params, setParams] = useSearchParams()
  const [coupon, setCoupon] = useState(params.get("coupon") ?? "")
  const [email, setEmail] = useState(params.get("email") ?? "")
  const [from, setFrom] = useState(params.get("from") ?? "")
  const [to, setTo] = useState(params.get("to") ?? "")
  const couponQuery = useDebounced(coupon, 300)
  const emailQuery = useDebounced(email, 300)
  const pg = useServerPagination(25, [couponQuery, emailQuery, from, to])

  const filter = {
    coupon: couponQuery || undefined,
    email: emailQuery || undefined,
    // Bare dates: the handler reads them as whole days in Vietnam time.
    // Sending a browser-local ISO instant would shift the window for an
    // operator outside Vietnam.
    from: from || undefined,
    to: to || undefined,
  }

  const { data, isLoading, error } = useQuery({
    queryKey: ["admin", "redemptions", filter, pg.page, pg.pageSize],
    queryFn: () => adminRedemptions.list({ ...filter, ...pg.params }),
  })
  const { items, total, totalPages } = pg.from(data, data?.redemptions)

  const [downloading, setDownloading] = useState(false)
  const download = async () => {
    setDownloading(true)
    try {
      const file = await adminRedemptions.csv(filter)
      saveBlob(file, `coupon-redemptions-${new Date().toISOString().slice(0, 10)}.csv`)
    } catch (err) {
      showToast((err as Error).message, "error")
    } finally {
      setDownloading(false)
    }
  }

  const applyParams = (next: { coupon?: string; email?: string; from?: string; to?: string }) => {
    const merged = { coupon, email, from, to, ...next }
    const search = new URLSearchParams()
    if (merged.coupon) search.set("coupon", merged.coupon)
    if (merged.email) search.set("email", merged.email)
    if (merged.from) search.set("from", merged.from)
    if (merged.to) search.set("to", merged.to)
    setParams(search, { replace: true })
  }

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{t("redemptions.title")}</h1>
          <p className="text-sm text-muted-foreground">{t("redemptions.subtitle")}</p>
        </div>
        <Button variant="outline" onClick={download} disabled={downloading}>
          {downloading ? <Loader2 className="h-4 w-4 animate-spin" /> : <Download className="h-4 w-4" />}
          {t("redemptions.download")}
        </Button>
      </div>

      <Card>
        <CardContent className="space-y-4 pt-6">
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <div className="space-y-1.5">
              <Label htmlFor="filter-coupon">{t("redemptions.filter_coupon")}</Label>
              <div className="relative">
                <Search className="pointer-events-none absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
                <Input
                  id="filter-coupon"
                  className="pl-8 font-mono uppercase"
                  value={coupon}
                  onChange={(e) => {
                    setCoupon(e.target.value)
                    applyParams({ coupon: e.target.value })
                  }}
                />
              </div>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="filter-email">{t("redemptions.filter_email")}</Label>
              <Input
                id="filter-email"
                type="email"
                value={email}
                onChange={(e) => {
                  setEmail(e.target.value)
                  applyParams({ email: e.target.value })
                }}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="filter-from">{t("redemptions.from")}</Label>
              <Input
                id="filter-from"
                type="date"
                value={from}
                onChange={(e) => {
                  setFrom(e.target.value)
                  applyParams({ from: e.target.value })
                }}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="filter-to">{t("redemptions.to")}</Label>
              <Input
                id="filter-to"
                type="date"
                value={to}
                onChange={(e) => {
                  setTo(e.target.value)
                  applyParams({ to: e.target.value })
                }}
              />
            </div>
          </div>

          {isLoading && <div className="h-40 animate-pulse rounded-lg bg-muted" />}
          {error && <p className="text-sm text-destructive">{(error as Error).message}</p>}

          {!isLoading && !error && (
            <>
              <DataTable>
                <DataTableHeader>
                  <DataTableRow>
                    <DataTableHead>{t("redemptions.coupon")}</DataTableHead>
                    <DataTableHead>{t("redemptions.order")}</DataTableHead>
                    <DataTableHead>{t("redemptions.customer")}</DataTableHead>
                    <DataTableHead>{t("redemptions.amount")}</DataTableHead>
                    <DataTableHead>{t("redemptions.created")}</DataTableHead>
                  </DataTableRow>
                </DataTableHeader>
                <DataTableBody>
                  {items.length === 0 && <DataTableEmpty colSpan={5} message={t("redemptions.empty")} />}
                  {items.map((row) => (
                    <DataTableRow key={row.id}>
                      <DataTableCell>
                        <Link
                          to={`/admin/payments/coupons/${row.coupon_id}`}
                          className="font-mono text-sm text-primary hover:underline"
                        >
                          {row.coupon_code}
                        </Link>
                      </DataTableCell>
                      <DataTableCell>
                        <div className="flex items-center gap-1 font-mono text-xs">
                          {row.order_code}
                          {row.order_code && <CopyButton value={row.order_code} />}
                        </div>
                      </DataTableCell>
                      <DataTableCell>
                        <div className="text-sm">{row.email || "—"}</div>
                        {row.external_customer_id && (
                          <div className="text-xs text-muted-foreground">{row.external_customer_id}</div>
                        )}
                      </DataTableCell>
                      <DataTableCell className="whitespace-nowrap tabular-nums">
                        {row.type === "trial"
                          ? `${t("coupons.trial_days")}: ${row.trial_days}`
                          : formatMoney(row.amount_minor, row.currency || "VND", row.currency_exponent)}
                      </DataTableCell>
                      <DataTableCell className="whitespace-nowrap text-sm text-muted-foreground">
                        {formatDateVN(row.created_at)}
                      </DataTableCell>
                    </DataTableRow>
                  ))}
                </DataTableBody>
              </DataTable>
              <DataTablePagination
                page={pg.page}
                totalPages={totalPages}
                total={total}
                pageSize={pg.pageSize}
                onPageChange={pg.setPage}
                onPageSizeChange={pg.setPageSize}
              />
            </>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
