import { useQuery } from "@tanstack/react-query"
import { ArrowRight } from "lucide-react"
import { Link } from "react-router-dom"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { useI18n } from "@/i18n"
import { portalPayments } from "@/lib/api"
import { formatMoney } from "@/lib/money"
import { paymentStatusClass, paymentStatusLabel } from "@/lib/payment-status"
import { formatDate } from "@/lib/utils"

export default function PortalPaymentsPage() {
  const { t } = useI18n()
  const { data, isLoading, error } = useQuery({
    queryKey: ["portal", "payments", "orders"],
    queryFn: () => portalPayments.orders({ limit: 100 }),
  })
  const orders = data?.orders ?? []

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold tracking-tight">{t("portal.myPayments")}</h1>
        <p className="text-muted-foreground">{t("portal.myPaymentsDesc")}</p>
      </div>

      {isLoading && <div className="h-40 animate-pulse rounded-lg bg-muted" />}
      {error && <p className="text-sm text-destructive">{(error as Error).message}</p>}

      {!isLoading && !error && orders.length === 0 && (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground">{t("portal.no_orders")}</CardContent>
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
                    {formatDate(order.created_at)}
                    {order.expires_at && order.status === "pending"
                      ? ` · ${t("portal.orderExpires")} ${formatDate(order.expires_at)}`
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
