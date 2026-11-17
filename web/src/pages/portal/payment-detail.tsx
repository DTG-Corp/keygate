import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { ArrowLeft, Ban, ExternalLink, Loader2, RefreshCw } from "lucide-react"
import { useState } from "react"
import { Link, useParams } from "react-router-dom"
import { PaymentInstructionsBlock } from "@/components/payment-instructions"
import { showToast } from "@/components/toast"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { useI18n } from "@/i18n"
import { portalPayments } from "@/lib/api"
import { formatMoney } from "@/lib/money"
import { paymentStatusClass, paymentStatusLabel } from "@/lib/payment-status"
import { formatDate } from "@/lib/utils"

export default function PortalPaymentDetailPage() {
  const { t } = useI18n()
  const { code } = useParams<{ code: string }>()
  const qc = useQueryClient()
  const [cancelOpen, setCancelOpen] = useState(false)

  const detail = useQuery({
    queryKey: ["portal", "payments", "order", code],
    queryFn: () => portalPayments.order(code ?? ""),
    enabled: !!code,
    // A pending transfer clears on the provider webhook, not in this tab:
    // poll while it is pending so the page flips to Paid without a manual
    // refresh, and stop once it reaches a terminal state.
    refetchInterval: (query) => (query.state.data?.order.status === "pending" ? 5000 : false),
  })
  const order = detail.data?.order

  // A cancel may answer 204 with no order, and a re-issued instruction may
  // carry only the payment block. Keep the order already on screen and
  // merge what did arrive instead of replacing it with empty defaults.
  const refresh = (payload: Awaited<ReturnType<typeof portalPayments.pay>>, status?: string) => {
    if (payload.order.code) {
      qc.setQueryData(["portal", "payments", "order", code], payload)
    } else {
      qc.setQueryData<typeof payload>(["portal", "payments", "order", code], (current) =>
        current
          ? {
              order: { ...current.order, status: status ?? current.order.status },
              payment: Object.values(payload.payment).some((value) => value !== undefined)
                ? payload.payment
                : current.payment,
            }
          : current,
      )
      void detail.refetch()
    }
    qc.invalidateQueries({ queryKey: ["portal", "payments", "orders"] })
  }

  const pay = useMutation({
    meta: { ownErrors: true },
    mutationFn: () => portalPayments.pay(code ?? ""),
    onSuccess: (payload) => {
      refresh(payload)
      showToast(t("portal.payment_updated"), "success")
    },
    onError: (err: Error) => showToast(err.message, "error"),
  })

  const cancel = useMutation({
    meta: { ownErrors: true },
    mutationFn: () => portalPayments.cancel(code ?? ""),
    onSuccess: (payload) => {
      refresh(payload, "canceled")
      setCancelOpen(false)
      showToast(t("portal.order_canceled"), "success")
    },
    onError: (err: Error) => showToast(err.message, "error"),
  })

  if (detail.isLoading) return <div className="h-48 animate-pulse rounded-lg bg-muted" />
  if (detail.error) {
    return (
      <div className="space-y-3">
        <p className="text-sm text-destructive">{(detail.error as Error).message}</p>
        <Button variant="outline" asChild>
          <Link to="/portal/payments">
            <ArrowLeft className="h-4 w-4" /> {t("portal.myPayments")}
          </Link>
        </Button>
      </div>
    )
  }
  if (!order) return <p className="text-sm text-muted-foreground">{t("portal.not_found")}</p>

  const isPending = order.status === "pending"
  const canPay = isPending
  return (
    <div className="space-y-4">
      <Button variant="ghost" size="sm" className="-ml-2" asChild>
        <Link to="/portal/payments">
          <ArrowLeft className="h-4 w-4" /> {t("portal.myPayments")}
        </Link>
      </Button>

      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="space-y-1">
          <h1 className="flex flex-wrap items-center gap-2 font-mono text-2xl font-bold tracking-tight">
            {order.code}
            <Badge className={paymentStatusClass(order.status)}>{paymentStatusLabel(order.status, t)}</Badge>
          </h1>
          <p className="text-sm text-muted-foreground">
            {order.product_name || order.plan_name || t("portal.kind")}
            {order.plan_name && order.product_name ? ` · ${order.plan_name}` : ""}
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          {canPay && (
            <Button onClick={() => pay.mutate()} disabled={pay.isPending}>
              {pay.isPending ? <Loader2 className="h-4 w-4 animate-spin" /> : <ExternalLink className="h-4 w-4" />}
              {order.kind === "renewal" ? t("portal.renew") : t("portal.pay")}
            </Button>
          )}
          {isPending && (
            <Button variant="outline" className="text-destructive" onClick={() => setCancelOpen(true)}>
              <Ban className="h-4 w-4" /> {t("portal.cancel_order")}
            </Button>
          )}
          {order.invoice_id && (
            <Button variant="outline" asChild>
              <Link to={`/portal/invoices/${order.invoice_id}`}>{t("portal.invoice_link")}</Link>
            </Button>
          )}
        </div>
      </div>

      <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr),22rem]">
        <Card>
          <CardHeader className="flex-row items-center justify-between space-y-0">
            <CardTitle className="text-base">{t("portal.payment_instructions")}</CardTitle>
            {isPending && (
              <Button
                variant="ghost"
                size="sm"
                onClick={() => pay.mutate()}
                disabled={pay.isPending}
                title={t("portal.payment_updated")}
              >
                <RefreshCw className={`h-4 w-4 ${pay.isPending ? "animate-spin" : ""}`} />
              </Button>
            )}
          </CardHeader>
          <CardContent>
            <PaymentInstructionsBlock
              instructions={order.payment ?? {}}
              orderCode={order.code}
              amountMinor={order.total_minor}
              currency={order.currency}
              currencyExponent={order.currency_exponent}
              showAmount={false}
            />
            {order.expires_at && isPending && (
              <p className="mt-4 text-xs text-muted-foreground">
                {t("portal.orderExpires")}: {formatDate(order.expires_at)}
              </p>
            )}
            {(order.status === "expired" || order.status === "canceled") && (
              <p className="mt-4 rounded-md border bg-muted p-3 text-sm text-muted-foreground">
                {order.status === "expired" ? t("pay.expired_hint") : t("pay.canceled_hint")}
              </p>
            )}
          </CardContent>
        </Card>

        <Card className="h-fit">
          <CardHeader>
            <CardTitle className="text-base">{t("portal.payment_order")}</CardTitle>
          </CardHeader>
          <CardContent>
            <dl className="space-y-2 text-sm">
              <Row label={t("portal.kind")} value={kindLabel(order.kind, t)} />
              <Row label={t("portal.orderCreated")} value={formatDate(order.created_at)} />
              {order.expires_at && <Row label={t("portal.orderExpires")} value={formatDate(order.expires_at)} />}
              {order.email && <Row label={t("portal.billingEmail")} value={order.email} />}
              <div className="flex items-baseline justify-between gap-3 border-t pt-2">
                <dt className="text-muted-foreground">{t("portal.orderAmount")}</dt>
                <dd className="text-lg font-semibold tabular-nums">
                  {formatMoney(order.total_minor, order.currency, order.currency_exponent)}
                </dd>
              </div>
              {order.discount_minor !== undefined && order.discount_minor > 0 && (
                <p className="text-right text-xs text-emerald-700">
                  {t("pay.discount")}: −{formatMoney(order.discount_minor, order.currency, order.currency_exponent)}
                </p>
              )}
            </dl>
          </CardContent>
        </Card>
      </div>

      <AlertDialog open={cancelOpen} onOpenChange={setCancelOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("portal.cancel_order_title")}</AlertDialogTitle>
            <AlertDialogDescription>{t("portal.cancel_order_desc")}</AlertDialogDescription>
          </AlertDialogHeader>
          <div className="flex justify-end gap-2">
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-white hover:bg-destructive/90"
              onClick={(e) => {
                e.preventDefault()
                cancel.mutate()
              }}
              disabled={cancel.isPending}
            >
              {cancel.isPending && <Loader2 className="h-4 w-4 animate-spin" />}
              {t("portal.cancel_order")}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

function Row({ label, value }: { label: string; value?: string }) {
  return (
    <div className="flex items-baseline justify-between gap-3">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="text-right">{value || "—"}</dd>
    </div>
  )
}

function kindLabel(kind: string, t: ReturnType<typeof useI18n>["t"]): string {
  if (kind === "renewal") return t("portal.kind_renewal")
  if (kind === "upgrade") return t("portal.kind_upgrade")
  return t("portal.kind_new")
}
