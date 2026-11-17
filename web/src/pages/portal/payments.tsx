import { useQuery } from "@tanstack/react-query"
import { ArrowRight } from "lucide-react"
import { useState } from "react"
import { Link } from "react-router-dom"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { useI18n } from "@/i18n"
import { portalPayments } from "@/lib/api"
import { formatMoney } from "@/lib/money"
import { paymentStatusClass, paymentStatusLabel } from "@/lib/payment-status"
import { formatDateVN } from "@/lib/utils"

// The shipped portal list has no status query parameter, so the filter
// is applied to the orders this page has loaded. The hint under the
// select says so instead of implying a server-side filter.
const STATUSES = ["pending", "paid", "expired", "canceled", "refunded"] as const

export default function PortalPaymentsPage() {
  const { t } = useI18n()
  const [status, setStatus] = useState("all")
  const { data, isLoading, error } = useQuery({
    queryKey: ["portal", "payments", "orders"],
    queryFn: () => portalPayments.orders({ limit: 100 }),
  })
  const loaded = data?.orders ?? []
  const orders = status === "all" ? loaded : loaded.filter((order) => order.status === status)

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold tracking-tight">{t("portal.myPayments")}</h1>
        <p className="text-muted-foreground">{t("portal.myPaymentsDesc")}</p>
      </div>

      <div className="flex flex-wrap items-end gap-2">
        <div className="space-y-1.5">
          <label htmlFor="portal-order-status" className="text-sm font-medium">
            {t("common.status")}
          </label>
          <Select value={status} onValueChange={setStatus}>
            <SelectTrigger id="portal-order-status" className="w-44">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">{t("common.all")}</SelectItem>
              {STATUSES.map((value) => (
                <SelectItem key={value} value={value}>
                  {paymentStatusLabel(value, t)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
        {status !== "all" && <p className="pb-2 text-xs text-muted-foreground">{t("portal.filter_loaded_only")}</p>}
      </div>

      {isLoading && <div className="h-40 animate-pulse rounded-lg bg-muted" />}
      {error && <p className="text-sm text-destructive">{(error as Error).message}</p>}

      {!isLoading && !error && orders.length === 0 && (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground">
            {loaded.length === 0 ? t("portal.no_orders") : t("portal.no_orders_match")}
          </CardContent>
        </Card>
      )}

      {orders.length > 0 && (
        <div className="space-y-3">
          {orders.map((order) => (
            <Card key={order.code}>
              <CardContent className="flex flex-wrap items-center justify-between gap-4 py-4">
                <div className="min-w-0 space-y-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="font-mono text-sm font-medium">{order.code}</span>
                    <Badge className={paymentStatusClass(order.status)}>{paymentStatusLabel(order.status, t)}</Badge>
                  </div>
                  <p className="text-sm text-muted-foreground">
                    {order.product_name || order.plan_name || order.kind}
                    {order.plan_name && order.product_name ? ` · ${order.plan_name}` : ""}
                  </p>
                  <p className="text-xs text-muted-foreground">
                    {formatDateVN(order.created_at)}
                    {order.expires_at && order.status === "pending"
                      ? ` · ${t("portal.orderExpires")} ${formatDateVN(order.expires_at)}`
                      : ""}
                  </p>
                </div>
                <div className="flex items-center gap-3">
                  <span className="font-semibold tabular-nums">
                    {formatMoney(order.total_minor, order.currency, order.currency_exponent)}
                  </span>
                  <Button variant="outline" size="sm" asChild>
                    <Link to={`/portal/payments/${order.code}`}>
                      {t("portal.viewOrder")} <ArrowRight className="h-3.5 w-3.5" />
                    </Link>
                  </Button>
                </div>
              </CardContent>
            </Card>
          ))}
        </div>
      )}
    </div>
  )
}
