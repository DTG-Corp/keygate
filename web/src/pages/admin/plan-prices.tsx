import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { AlertTriangle, ArrowLeft, Pencil, Plus, Trash2 } from "lucide-react"
import { useState } from "react"
import { Link, useParams } from "react-router-dom"
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
  DataTableRow,
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
import { type TranslationKeys, useI18n } from "@/i18n"
import { admin, adminPayments, type PlanPriceRow } from "@/lib/api"
import { currencyExponent, formatAmountInput, formatMoney, parseAmountToMinor } from "@/lib/money"
import { formatDate } from "@/lib/utils"

const CURRENCIES = ["VND", "USD", "EUR"] as const

interface PriceForm {
  provider: "sepay" | "payos"
  currency: string
  amount: string
  interval: string
}

function emptyForm(): PriceForm {
  return { provider: "sepay", currency: "VND", amount: "", interval: "" }
}

export default function PlanPricesPage() {
  const { t } = useI18n()
  const qc = useQueryClient()
  const { id = "" } = useParams<{ id: string }>()
  const [editor, setEditor] = useState<PriceForm | null>(null)
  const [formError, setFormError] = useState("")
  const [deleting, setDeleting] = useState<PlanPriceRow | null>(null)

  const planQuery = useQuery({
    queryKey: ["admin", "plans", "detail", id],
    queryFn: () => admin.getPlan(id),
    enabled: !!id,
  })
  const pricesQuery = useQuery({
    queryKey: ["admin", "plans", id, "prices"],
    queryFn: () => adminPayments.priceList(id),
    enabled: !!id,
  })
  const rows = pricesQuery.data?.prices ?? []
  const active = rows.filter((row) => row.active)

  const saveMut = useMutation({
    mutationFn: (form: PriceForm) => {
      const exponent = currencyExponent(form.currency)
      const amount = parseAmountToMinor(form.amount, exponent)
      if (amount === null) throw new Error(t("admin.amount_invalid"))
      return adminPayments.upsertPrice(id, {
        provider: form.provider,
        currency: form.currency,
        amount_minor: amount,
        currency_exponent: exponent,
        billing_interval: form.interval,
      })
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "plans", id, "prices"] })
      setEditor(null)
      setFormError("")
      showToast(t("admin.price_saved"), "success")
    },
    meta: { ownErrors: true },
    onError: (e: Error) => setFormError(e.message),
  })
  const deleteMut = useMutation({
    mutationFn: (row: PlanPriceRow) => adminPayments.deletePrice(id, row.id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "plans", id, "prices"] })
      setDeleting(null)
      showToast(t("admin.price_deleted"), "success")
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })

  const openEdit = (row: PlanPriceRow) => {
    setFormError("")
    setEditor({
      provider: row.provider === "payos" ? "payos" : "sepay",
      currency: row.currency,
      amount: formatAmountInput(row.amount_minor, row.currency_exponent),
      interval: row.billing_interval,
    })
  }

  const save = () => {
    if (!editor) return
    setFormError("")
    saveMut.mutate(editor)
  }

  if (planQuery.isLoading) {
    return <div className="h-32 animate-pulse rounded-lg bg-muted" />
  }
  if (planQuery.error) {
    return (
      <Card>
        <CardContent className="space-y-4 py-10 text-center">
          <p className="text-sm text-destructive">{t("admin.plan_not_found")}</p>
          <Button asChild variant="outline">
            <Link to="/admin/plans">
              <ArrowLeft className="mr-2 h-4 w-4" /> {t("common.back")}
            </Link>
          </Button>
        </CardContent>
      </Card>
    )
  }

  const plan = planQuery.data

  return (
    <div className="space-y-6">
      <div>
        <Button asChild variant="ghost" size="sm" className="-ml-2 mb-1">
          <Link to="/admin/plans">
            <ArrowLeft className="mr-2 h-4 w-4" /> {t("common.back")}
          </Link>
        </Button>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <h1 className="text-2xl font-bold tracking-tight">{t("admin.prices_title")}</h1>
            <p className="text-muted-foreground">
              {plan?.name ? `${plan.name} — ` : ""}
              {t("admin.prices_subtitle")}
            </p>
          </div>
          <Button
            onClick={() => {
              setFormError("")
              setEditor(emptyForm())
            }}
          >
            <Plus className="mr-2 h-4 w-4" /> {t("admin.add_price")}
          </Button>
        </div>
      </div>

      {pricesQuery.error && (
        <p className="rounded-md border border-red-200 bg-red-50 p-3 text-sm text-red-700">
          {(pricesQuery.error as Error).message}
        </p>
      )}

      {!pricesQuery.isLoading && !pricesQuery.error && active.length === 0 && (
        <div className="flex items-start gap-2 rounded-lg border border-amber-200 bg-amber-50 p-4 text-sm text-amber-800">
          <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
          <p>{t("admin.price_missing")}</p>
        </div>
      )}

      <Card>
        <CardContent className="pt-6">
          {pricesQuery.isLoading ? (
            <div className="h-32 animate-pulse rounded-lg bg-muted" />
          ) : (
            <DataTable>
              <DataTableHeader>
                <DataTableRow>
                  <DataTableHead>{t("admin.price_provider")}</DataTableHead>
                  <DataTableHead>{t("admin.price_currency")}</DataTableHead>
                  <DataTableHead>{t("admin.price_amount")}</DataTableHead>
                  <DataTableHead>{t("admin.price_interval")}</DataTableHead>
                  <DataTableHead>{t("common.status")}</DataTableHead>
                  <DataTableHead>{t("common.created")}</DataTableHead>
                  <DataTableHead className="w-24 text-right">{t("common.actions")}</DataTableHead>
                </DataTableRow>
              </DataTableHeader>
              <DataTableBody>
                {rows.length === 0 && <DataTableEmpty colSpan={7} message={t("admin.no_prices")} />}
                {rows.map((row) => (
                  <DataTableRow key={row.id}>
                    <DataTableCell className="font-medium">
                      {row.provider === "sepay" ? "SePay" : row.provider === "payos" ? "PayOS" : row.provider}
                    </DataTableCell>
                    <DataTableCell>{row.currency}</DataTableCell>
                    <DataTableCell className="whitespace-nowrap tabular-nums">
                      {formatMoney(row.amount_minor, row.currency, row.currency_exponent)}
                    </DataTableCell>
                    <DataTableCell>{intervalLabel(row.billing_interval, t)}</DataTableCell>
                    <DataTableCell>
                      <Badge variant={row.active ? "default" : "secondary"}>
                        {row.active ? t("admin.price_active") : t("admin.price_inactive")}
                      </Badge>
                    </DataTableCell>
                    <DataTableCell className="whitespace-nowrap text-xs text-muted-foreground">
                      {formatDate(row.created_at)}
                    </DataTableCell>
                    <DataTableCell>
                      <div className="flex justify-end gap-1">
                        {row.active && (
                          <>
                            <Button variant="ghost" size="icon" title={t("common.edit")} onClick={() => openEdit(row)}>
                              <Pencil className="h-4 w-4" />
                            </Button>
                            <Button
                              variant="ghost"
                              size="icon"
                              title={t("admin.delete_price")}
                              onClick={() => setDeleting(row)}
                            >
                              <Trash2 className="h-4 w-4" />
                            </Button>
                          </>
                        )}
                      </div>
                    </DataTableCell>
                  </DataTableRow>
                ))}
              </DataTableBody>
            </DataTable>
          )}
        </CardContent>
      </Card>

      <Dialog
        open={!!editor}
        onOpenChange={(open) => {
          if (!open) {
            setEditor(null)
            setFormError("")
          }
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("admin.add_price")}</DialogTitle>
            <DialogDescription>{t("admin.price_exponent_hint")}</DialogDescription>
          </DialogHeader>
          {editor && (
            <>
              <DialogBody className="space-y-4">
                {formError && <p className="text-sm text-destructive">{formError}</p>}
                <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                  <div className="space-y-2">
                    <Label>{t("admin.price_provider")}</Label>
                    <Select
                      value={editor.provider}
                      onValueChange={(v) => setEditor({ ...editor, provider: v as "sepay" | "payos" })}
                    >
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="sepay">SePay</SelectItem>
                        <SelectItem value="payos">PayOS</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="space-y-2">
                    <Label>{t("admin.price_currency")}</Label>
                    <Select value={editor.currency} onValueChange={(currency) => setEditor({ ...editor, currency })}>
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        {CURRENCIES.map((currency) => (
                          <SelectItem key={currency} value={currency}>
                            {currency}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="space-y-2">
                    <Label htmlFor="price-amount">{t("admin.price_amount")}</Label>
                    <Input
                      id="price-amount"
                      value={editor.amount}
                      onChange={(e) => setEditor({ ...editor, amount: e.target.value })}
                      inputMode="decimal"
                      placeholder="1.190.000"
                    />
                    <p className="text-xs text-muted-foreground">{t("admin.price_exponent_hint")}</p>
                  </div>
                  <div className="space-y-2">
                    <Label>{t("admin.price_interval")}</Label>
                    <Select
                      value={editor.interval || "once"}
                      onValueChange={(v) => setEditor({ ...editor, interval: v === "once" ? "" : v })}
                    >
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="once">{t("admin.price_interval_once")}</SelectItem>
                        <SelectItem value="month">{t("admin.price_interval_month")}</SelectItem>
                        <SelectItem value="year">{t("admin.price_interval_year")}</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                </div>
              </DialogBody>
              <DialogFooter>
                <Button variant="outline" onClick={() => setEditor(null)} disabled={saveMut.isPending}>
                  {t("common.cancel")}
                </Button>
                <Button onClick={save} disabled={saveMut.isPending}>
                  {t("common.save")}
                </Button>
              </DialogFooter>
            </>
          )}
        </DialogContent>
      </Dialog>

      <AlertDialog open={!!deleting} onOpenChange={(open) => (open ? undefined : setDeleting(null))}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("admin.delete_price")}</AlertDialogTitle>
            <AlertDialogDescription>{t("admin.delete_price_confirm")}</AlertDialogDescription>
          </AlertDialogHeader>
          <div className="flex justify-end gap-2">
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-white hover:bg-destructive/90"
              onClick={() => deleting && deleteMut.mutate(deleting)}
              disabled={deleteMut.isPending}
            >
              {t("admin.delete_price")}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

function intervalLabel(value: string, t: (key: TranslationKeys) => string): string {
  if (value === "month") return t("admin.price_interval_month")
  if (value === "year") return t("admin.price_interval_year")
  return t("admin.price_interval_once")
}
