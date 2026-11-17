import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Ban, CheckCircle2, Eye, RotateCcw } from "lucide-react"
import { useState } from "react"
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
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Sheet, SheetContent } from "@/components/ui/sheet"
import { type TranslationKeys, useI18n } from "@/i18n"
import { admin, adminPayments, type PaymentTransactionRow } from "@/lib/api"
import { formatAmountInput, formatMoney, parseAmountToMinor } from "@/lib/money"
import { useDebounced } from "@/lib/use-debounced"
import { formatDate } from "@/lib/utils"

const STATUSES = ["pending", "paid", "expired", "canceled", "refunded", "partially_refunded"] as const
const PROVIDERS = ["sepay", "payos"] as const

export default function PaymentOrdersPage() {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [status, setStatus] = useState("")
  const [provider, setProvider] = useState("")
  const [email, setEmail] = useState("")
  const emailQuery = useDebounced(email, 300)
  const pg = useServerPagination(20, [status, provider, emailQuery])
  const { data, isLoading, error } = useQuery({
    queryKey: ["admin", "payments", "orders", status, provider, emailQuery, pg.page, pg.pageSize],
    queryFn: () =>
      adminPayments.orders({
        status,
        provider,
        email: emailQuery,
        ...pg.params,
      }),
  })
  const { items: orders, total, totalPages } = pg.from(data, data?.orders)

  const { data: products } = useQuery({
    queryKey: ["admin", "products", "payment-order-labels"],
    queryFn: () => admin.listProducts({ limit: 200 }),
  })
  const { data: plans } = useQuery({
    queryKey: ["admin", "plans", "payment-order-labels"],
    queryFn: () => admin.listPlans({ limit: 200 }),
  })
  const productNames = new Map((products?.products ?? []).map((p) => [p.id, p.name]))
  const planNames = new Map((plans?.plans ?? []).map((p) => [p.id, p.name]))

  const [selected, setSelected] = useState<string | null>(null)
  const detail = useQuery({
    queryKey: ["admin", "payments", "order", selected],
    queryFn: () => adminPayments.order(selected ?? ""),
    enabled: !!selected,
  })

  const [cancelOpen, setCancelOpen] = useState(false)
  const [markOpen, setMarkOpen] = useState(false)
  const [markReason, setMarkReason] = useState("")
  const [refundOpen, setRefundOpen] = useState(false)
  const [refundAmount, setRefundAmount] = useState("")
  const [refundReason, setRefundReason] = useState("")
  const [actionError, setActionError] = useState("")

  const order = detail.data?.order
  const refundedMinor = (detail.data?.refunds ?? []).reduce((sum, refund) => sum + refund.amount_minor, 0)

  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["admin", "payments"] })
  }

  const cancelMut = useMutation({
    mutationFn: (code: string) => adminPayments.cancel(code),
    onSuccess: () => {
      refresh()
      setCancelOpen(false)
      showToast(t("admin.order_canceled"), "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const markPaidMut = useMutation({
    mutationFn: ({ code, reason }: { code: string; reason: string }) => adminPayments.markPaid(code, reason),
    onSuccess: () => {
      refresh()
      setMarkOpen(false)
      setMarkReason("")
      setActionError("")
      showToast(t("admin.mark_paid_saved"), "success")
    },
    meta: { ownErrors: true },
    onError: (e: Error) => setActionError(e.message),
  })
  const refundMut = useMutation({
    // Phase 1a refunds are manual: the operator transfers the money back
    // and records it. RefundOrder refuses any other method, so the UI
    // does not offer one.
    mutationFn: (body: { code: string; amount_minor: number; reason: string }) =>
      adminPayments.refund(body.code, {
        amount_minor: body.amount_minor,
        reason: body.reason,
        method: "manual",
      }),
    onSuccess: () => {
      refresh()
      setRefundOpen(false)
      setActionError("")
      showToast(t("admin.refund_recorded"), "success")
    },
    meta: { ownErrors: true },
    onError: (e: Error) => setActionError(e.message),
  })

  const openMarkPaid = () => {
    setMarkReason("")
    setActionError("")
    setMarkOpen(true)
  }
  const openRefund = () => {
    if (!order) return
    setRefundAmount(formatAmountInput(Math.max(0, order.total_minor - refundedMinor), order.currency_exponent))
    setRefundReason("")
    setActionError("")
    setRefundOpen(true)
  }
  const submitRefund = () => {
    if (!order) return
    const parsed = parseAmountToMinor(refundAmount, order.currency_exponent)
    if (parsed === null || parsed <= 0) {
      setActionError(t("admin.amount_invalid"))
      return
    }
    if (parsed > order.total_minor - refundedMinor) {
      setActionError(t("admin.refund_too_large"))
      return
    }
    if (!refundReason.trim()) {
      setActionError(t("admin.reason"))
      return
    }
    setActionError("")
    refundMut.mutate({
      code: order.code,
      amount_minor: parsed,
      reason: refundReason.trim(),
    })
  }

  const statusLabel = (value: string) => {
    switch (value) {
      case "paid":
        return t("pay.paid")
      case "expired":
        return t("pay.expired")
      case "canceled":
        return t("pay.canceled")
      case "refunded":
      case "partially_refunded":
        return t("pay.refunded")
      default:
        return t("pay.pending")
    }
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold tracking-tight sr-only md:not-sr-only">{t("admin.orders")}</h1>
        <p className="text-muted-foreground">{t("admin.orders_subtitle")}</p>
      </div>

      <div className="flex flex-wrap gap-3">
        <Select value={status || "all"} onValueChange={(v) => setStatus(v === "all" ? "" : v)}>
          <SelectTrigger className="w-44">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">{t("common.all")}</SelectItem>
            {STATUSES.map((s) => (
              <SelectItem key={s} value={s}>
                {statusLabel(s)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select value={provider || "all"} onValueChange={(v) => setProvider(v === "all" ? "" : v)}>
          <SelectTrigger className="w-44">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">{t("common.all")}</SelectItem>
            {PROVIDERS.map((p) => (
              <SelectItem key={p} value={p}>
                {p === "sepay" ? "SePay" : "PayOS"}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Input
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          placeholder={t("common.email")}
          className="w-full sm:w-64"
        />
      </div>

      {error && (
        <p className="rounded-md border border-red-200 bg-red-50 p-3 text-sm text-red-700">
          {(error as Error).message}
        </p>
      )}

      <Card>
        <CardContent className="pt-6">
          {isLoading ? (
            <div className="h-32 animate-pulse rounded-lg bg-muted" />
          ) : (
            <>
              <DataTable>
                <DataTableHeader>
                  <DataTableRow>
                    <DataTableHead>{t("pay.order")}</DataTableHead>
                    <DataTableHead>{t("common.email")}</DataTableHead>
                    <DataTableHead>{t("pay.amount")}</DataTableHead>
                    <DataTableHead>{t("common.status")}</DataTableHead>
                    <DataTableHead>{t("admin.provider")}</DataTableHead>
                    <DataTableHead>{t("common.created")}</DataTableHead>
                    <DataTableHead className="w-16 text-right">{t("common.actions")}</DataTableHead>
                  </DataTableRow>
                </DataTableHeader>
                <DataTableBody>
                  {orders.length === 0 && <DataTableEmpty colSpan={7} message={t("admin.no_orders")} />}
                  {orders.map((row) => (
                    <DataTableRow key={row.id}>
                      <DataTableCell>
                        <button
                          type="button"
                          className="font-mono text-sm font-medium text-primary hover:underline"
                          onClick={() => setSelected(row.code)}
                        >
                          {row.code}
                        </button>
                      </DataTableCell>
                      <DataTableCell className="max-w-[16rem] truncate">{row.email}</DataTableCell>
                      <DataTableCell className="whitespace-nowrap tabular-nums">
                        {formatMoney(row.total_minor, row.currency, row.currency_exponent)}
                      </DataTableCell>
                      <DataTableCell>
                        <OrderStatusBadge status={row.status} />
                      </DataTableCell>
                      <DataTableCell className="text-sm text-muted-foreground">
                        {row.provider === "sepay" ? "SePay" : row.provider === "payos" ? "PayOS" : row.provider}
                      </DataTableCell>
                      <DataTableCell className="whitespace-nowrap text-xs text-muted-foreground">
                        {formatDate(row.created_at)}
                      </DataTableCell>
                      <DataTableCell>
                        <div className="flex justify-end">
                          <Button
                            variant="ghost"
                            size="icon"
                            title={t("admin.view")}
                            onClick={() => setSelected(row.code)}
                          >
                            <Eye className="h-4 w-4" />
                          </Button>
                        </div>
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

      <Sheet open={!!selected} onOpenChange={(open) => (open ? undefined : setSelected(null))}>
        {/* The sheet's title is its whole accessible name; a detail panel
            has no single sentence that describes it. Passing undefined is
            Radix's documented way to say so without a warning. */}
        <SheetContent
          side="right"
          label={t("admin.order_detail")}
          aria-describedby={undefined}
          className="w-[min(44rem,100vw)] max-w-[100vw]"
        >
          <div className="shrink-0 border-b p-4 pr-12">
            <h2 className="font-semibold">{t("admin.order_detail")}</h2>
            {order && <p className="font-mono text-xs text-muted-foreground">{order.code}</p>}
          </div>
          <div className="flex-1 space-y-5 overflow-y-auto p-4">
            {detail.isLoading && <div className="h-32 animate-pulse rounded-lg bg-muted" />}
            {detail.error && <p className="text-sm text-destructive">{(detail.error as Error).message}</p>}
            {order && (
              <>
                <div className="flex flex-wrap items-center gap-2">
                  <OrderStatusBadge status={order.status} />
                  <span className="text-sm text-muted-foreground">{order.email}</span>
                </div>

                <dl className="grid grid-cols-2 gap-x-4 gap-y-2 text-sm">
                  <Field label={t("common.product")} value={productNames.get(order.product_id) ?? order.product_id} />
                  <Field label={t("common.plan")} value={planNames.get(order.plan_id) ?? order.plan_id} />
                  <Field label={t("admin.provider")} value={providerName(order.provider)} />
                  <Field label={t("common.created")} value={formatDate(order.created_at)} />
                  <Field label={t("pay.expires")} value={formatDate(order.expires_at)} />
                  <Field label={t("pay.paid")} value={order.paid_at ? formatDate(order.paid_at) : "—"} />
                </dl>

                <div className="rounded-lg border p-4">
                  <p className="mb-2 text-sm font-medium">{t("admin.totals")}</p>
                  <dl className="space-y-1.5 text-sm">
                    <TotalRow
                      label={t("pay.subtotal")}
                      value={formatMoney(order.subtotal_minor, order.currency, order.currency_exponent)}
                    />
                    {order.discount_minor > 0 && (
                      <TotalRow
                        label={t("pay.discount")}
                        value={`-${formatMoney(order.discount_minor, order.currency, order.currency_exponent)}`}
                      />
                    )}
                    {order.tax_minor > 0 && (
                      <TotalRow
                        label={
                          (order.tax_rate_bp ?? 0) > 0
                            ? `${t("pay.vat")} (${((order.tax_rate_bp ?? 0) / 100).toLocaleString("vi-VN")}%)`
                            : t("pay.vat")
                        }
                        value={formatMoney(order.tax_minor, order.currency, order.currency_exponent)}
                      />
                    )}
                    <div className="flex items-baseline justify-between border-t pt-2 font-medium">
                      <dt>{t("pay.total")}</dt>
                      <dd className="tabular-nums">
                        {formatMoney(order.total_minor, order.currency, order.currency_exponent)}
                      </dd>
                    </div>
                  </dl>
                </div>

                {order.last_settlement_error && (
                  <p className="rounded-md border border-red-200 bg-red-50 p-3 text-xs text-red-700">
                    {t("admin.last_settlement_error")}: {order.last_settlement_error}
                  </p>
                )}

                <div className="flex flex-wrap gap-2">
                  {order.status === "pending" && (
                    <>
                      <Button size="sm" onClick={openMarkPaid}>
                        <CheckCircle2 className="mr-2 h-4 w-4" /> {t("admin.mark_paid")}
                      </Button>
                      <Button
                        size="sm"
                        variant="destructive"
                        onClick={() => {
                          setActionError("")
                          setCancelOpen(true)
                        }}
                      >
                        <Ban className="mr-2 h-4 w-4" /> {t("admin.cancel_order")}
                      </Button>
                    </>
                  )}
                  {(order.status === "paid" || order.status === "partially_refunded") && (
                    <Button size="sm" variant="outline" onClick={openRefund}>
                      <RotateCcw className="mr-2 h-4 w-4" /> {t("admin.refund")}
                    </Button>
                  )}
                </div>

                <DetailTable
                  title={t("admin.attempts")}
                  empty={t("common.noData")}
                  headers={[t("admin.provider"), t("common.status"), t("admin.provider_ref"), t("pay.amount")]}
                  rows={(detail.data?.attempts ?? []).map((a) => [
                    providerName(a.provider),
                    attemptStatusLabel(a.status, t),
                    a.provider_ref || "—",
                    formatMoney(a.amount_minor, a.currency, order.currency_exponent),
                  ])}
                />

                <TransactionsTable rows={detail.data?.transactions ?? []} fallbackExponent={order.currency_exponent} />
              </>
            )}
          </div>
        </SheetContent>
      </Sheet>

      <AlertDialog open={cancelOpen} onOpenChange={setCancelOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("admin.cancel_order")}</AlertDialogTitle>
            <AlertDialogDescription>{t("admin.cancel_order_confirm")}</AlertDialogDescription>
          </AlertDialogHeader>
          <div className="flex justify-end gap-2">
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => order && cancelMut.mutate(order.code)}
              disabled={cancelMut.isPending}
              className="bg-destructive text-white hover:bg-destructive/90"
            >
              {t("admin.cancel_order")}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>

      <Dialog
        open={markOpen}
        onOpenChange={(open) => {
          setMarkOpen(open)
          if (!open) setActionError("")
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("admin.mark_paid")}</DialogTitle>
            <DialogDescription>{t("admin.mark_paid_reason")}</DialogDescription>
          </DialogHeader>
          <DialogBody className="space-y-3">
            {actionError && <p className="text-sm text-destructive">{actionError}</p>}
            <Label htmlFor="mark-paid-reason">{t("admin.mark_paid_reason")}</Label>
            <Input
              id="mark-paid-reason"
              value={markReason}
              onChange={(e) => setMarkReason(e.target.value)}
              placeholder={t("admin.mark_paid_reason")}
            />
          </DialogBody>
          <DialogFooter>
            <Button variant="outline" onClick={() => setMarkOpen(false)}>
              {t("common.cancel")}
            </Button>
            <Button
              onClick={() => order && markPaidMut.mutate({ code: order.code, reason: markReason.trim() })}
              disabled={markPaidMut.isPending || !markReason.trim()}
            >
              {t("admin.mark_paid")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog
        open={refundOpen}
        onOpenChange={(open) => {
          setRefundOpen(open)
          if (!open) setActionError("")
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("admin.record_refund")}</DialogTitle>
            <DialogDescription>{t("admin.refund_hint")}</DialogDescription>
          </DialogHeader>
          <DialogBody className="space-y-3">
            {actionError && <p className="text-sm text-destructive">{actionError}</p>}
            <div className="space-y-2">
              <Label htmlFor="refund-amount">{t("admin.refund_amount")}</Label>
              <Input
                id="refund-amount"
                value={refundAmount}
                onChange={(e) => setRefundAmount(e.target.value)}
                inputMode="decimal"
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="refund-reason">{t("admin.refund_reason")}</Label>
              <Input id="refund-reason" value={refundReason} onChange={(e) => setRefundReason(e.target.value)} />
            </div>
          </DialogBody>
          <DialogFooter>
            <Button variant="outline" onClick={() => setRefundOpen(false)}>
              {t("common.cancel")}
            </Button>
            <Button onClick={submitRefund} disabled={refundMut.isPending}>
              {t("admin.record_refund")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

function OrderStatusBadge({ status }: { status: string }) {
  const { t } = useI18n()
  let label = t("pay.pending")
  let tone = "border-amber-200 bg-amber-50 text-amber-800"
  if (status === "paid") {
    label = t("pay.paid")
    tone = "border-emerald-200 bg-emerald-50 text-emerald-800"
  } else if (status === "refunded" || status === "partially_refunded") {
    label = t("pay.refunded")
    tone = "border-blue-200 bg-blue-50 text-blue-800"
  } else if (status === "expired" || status === "canceled") {
    label = status === "expired" ? t("pay.expired") : t("pay.canceled")
    tone = "border-border bg-muted text-muted-foreground"
  }
  return <span className={`rounded-full border px-2.5 py-0.5 text-xs font-medium ${tone}`}>{label}</span>
}

function Field({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="break-words">{value}</dd>
    </div>
  )
}

function TotalRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-baseline justify-between gap-4">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="tabular-nums">{value}</dd>
    </div>
  )
}

function DetailTable({
  title,
  empty,
  headers,
  rows,
}: {
  title: string
  empty: string
  headers: string[]
  rows: string[][]
}) {
  return (
    <section className="space-y-2">
      <h3 className="text-sm font-medium">{title}</h3>
      <DataTable>
        <DataTableHeader>
          <DataTableRow>
            {headers.map((header) => (
              <DataTableHead key={header}>{header}</DataTableHead>
            ))}
          </DataTableRow>
        </DataTableHeader>
        <DataTableBody>
          {rows.length === 0 && <DataTableEmpty colSpan={headers.length} message={empty} />}
          {rows.map((row) => (
            <DataTableRow key={row.join("|")}>
              {row.map((cell, index) => (
                <DataTableCell key={headers[index]} className="text-xs">
                  {cell}
                </DataTableCell>
              ))}
            </DataTableRow>
          ))}
        </DataTableBody>
      </DataTable>
    </section>
  )
}

function TransactionsTable({ rows, fallbackExponent }: { rows: PaymentTransactionRow[]; fallbackExponent: number }) {
  const { t } = useI18n()
  return (
    <section className="space-y-2">
      <h3 className="text-sm font-medium">{t("admin.transactions")}</h3>
      <DataTable>
        <DataTableHeader>
          <DataTableRow>
            <DataTableHead>{t("admin.provider")}</DataTableHead>
            <DataTableHead>{t("common.status")}</DataTableHead>
            <DataTableHead>{t("admin.provider_ref")}</DataTableHead>
            <DataTableHead>{t("pay.amount")}</DataTableHead>
            <DataTableHead>{t("admin.occurred_at")}</DataTableHead>
          </DataTableRow>
        </DataTableHeader>
        <DataTableBody>
          {rows.length === 0 && <DataTableEmpty colSpan={5} message={t("common.noData")} />}
          {rows.map((row) => (
            <DataTableRow key={row.id}>
              <DataTableCell className="text-xs">{providerName(row.provider)}</DataTableCell>
              <DataTableCell className="text-xs">
                <Badge variant="outline">{transactionStatusLabel(row.status, t)}</Badge>
              </DataTableCell>
              <DataTableCell className="max-w-[10rem] truncate font-mono text-xs">{row.provider_ref}</DataTableCell>
              <DataTableCell className="whitespace-nowrap text-xs tabular-nums">
                {formatMoney(row.amount_minor, row.currency, fallbackExponent)}
              </DataTableCell>
              <DataTableCell className="whitespace-nowrap text-xs text-muted-foreground">
                {formatDate(row.occurred_at)}
              </DataTableCell>
            </DataTableRow>
          ))}
        </DataTableBody>
      </DataTable>
    </section>
  )
}

function providerName(provider: string): string {
  if (provider === "sepay") return "SePay"
  if (provider === "payos") return "PayOS"
  return provider
}

function attemptStatusLabel(status: string, t: (key: TranslationKeys) => string): string {
  switch (status) {
    case "succeeded":
      return t("admin.attempt_succeeded")
    case "failed":
      return t("admin.attempt_failed")
    case "created":
      return t("admin.attempt_created")
    case "expired":
      return t("pay.expired")
    case "canceled":
      return t("pay.canceled")
    default:
      return t("pay.pending")
  }
}

function transactionStatusLabel(status: string, t: (key: TranslationKeys) => string): string {
  if (status === "matched") return t("admin.tx_matched")
  if (status === "ignored") return t("admin.tx_ignored")
  if (status === "unmatched") return t("admin.tx_unmatched")
  return status
}
