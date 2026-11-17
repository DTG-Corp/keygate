import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Pencil, Plus, Search, Trash2 } from "lucide-react"
import { useState } from "react"
import { Link } from "react-router-dom"
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
import { Input } from "@/components/ui/input"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { useI18n } from "@/i18n"
import { admin, adminCoupons, type Coupon } from "@/lib/api"
import { currencyExponent, formatMoney } from "@/lib/money"
import { useDebounced } from "@/lib/use-debounced"
import { boolColor, formatDate } from "@/lib/utils"

const STACK_GROUPS = ["general", "campaign", "private"] as const

export default function PaymentCouponsPage() {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [q, setQ] = useState("")
  const [active, setActive] = useState("all")
  const [stackGroup, setStackGroup] = useState("all")
  const qQuery = useDebounced(q, 300)
  const pg = useServerPagination(20, [qQuery, active, stackGroup])

  const { data, isLoading, error } = useQuery({
    queryKey: ["admin", "coupons", qQuery, active, stackGroup, pg.page, pg.pageSize],
    queryFn: () =>
      adminCoupons.list({
        q: qQuery,
        active: active === "all" ? undefined : active === "true",
        stack_group: stackGroup === "all" ? undefined : stackGroup,
        ...pg.params,
      }),
  })
  const { items, total, totalPages } = pg.from(data, data?.coupons)

  // Scope targets are shown by name when the list payload carries them.
  const { data: products } = useQuery({
    queryKey: ["admin", "products", "coupon-scope-labels"],
    queryFn: () => admin.listProducts({ limit: 200 }),
  })
  const { data: plans } = useQuery({
    queryKey: ["admin", "plans", "coupon-scope-labels"],
    queryFn: () => admin.listPlans({ limit: 200 }),
  })
  const productNames = new Map((products?.products ?? []).map((p) => [p.id, p.name]))
  const planNames = new Map((plans?.plans ?? []).map((p) => [p.id, p.name]))

  const [deleting, setDeleting] = useState<Coupon | null>(null)
  const remove = useMutation({
    meta: { ownErrors: true },
    mutationFn: (coupon: Coupon) => adminCoupons.remove(coupon.id),
    onSuccess: (result) => {
      qc.invalidateQueries({ queryKey: ["admin", "coupons"] })
      setDeleting(null)
      showToast(result?.archived ? t("coupons.archived") : t("coupons.deleted"), "success")
    },
  })

  const scopeLabel = (coupon: Coupon): string => {
    if (!coupon.scopes) return t("common.na")
    if (coupon.scopes.length === 0) return t("coupons.scope_all")
    const productIDs = coupon.scopes.filter((s) => s.scope_type === "product").map((s) => s.scope_id)
    const planIDs = coupon.scopes.filter((s) => s.scope_type === "plan").map((s) => s.scope_id)
    const names = [
      ...productIDs.map((id) => productNames.get(id)).filter(Boolean),
      ...planIDs.map((id) => planNames.get(id)).filter(Boolean),
    ]
    if (names.length > 0) return names.join(", ")
    return t("coupons.scope_count", { count: coupon.scopes.length })
  }

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-2xl font-bold tracking-tight">{t("coupons.title")}</h1>
          <p className="text-sm text-muted-foreground">{t("coupons.subtitle")}</p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" asChild>
            <Link to="/admin/payments/redemptions">{t("coupons.view_redemptions")}</Link>
          </Button>
          <Button asChild>
            <Link to="/admin/payments/coupons/new">
              <Plus className="h-4 w-4" /> {t("coupons.create")}
            </Link>
          </Button>
        </div>
      </div>

      <Card>
        <CardContent className="space-y-4 pt-6">
          <div className="flex flex-wrap items-center gap-2">
            <div className="relative min-w-[14rem] flex-1">
              <Search className="pointer-events-none absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
              <Input
                className="pl-8"
                value={q}
                placeholder={t("coupons.search")}
                onChange={(e) => setQ(e.target.value)}
              />
            </div>
            <Select value={active} onValueChange={setActive}>
              <SelectTrigger className="w-36">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">{t("common.all")}</SelectItem>
                <SelectItem value="true">{t("common.active")}</SelectItem>
                <SelectItem value="false">{t("common.inactive")}</SelectItem>
              </SelectContent>
            </Select>
            <Select value={stackGroup} onValueChange={setStackGroup}>
              <SelectTrigger className="w-40">
                <SelectValue placeholder={t("coupons.filter_stack_group")} />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">{t("common.all")}</SelectItem>
                {STACK_GROUPS.map((value) => (
                  <SelectItem key={value} value={value}>
                    {t(`coupons.stack_group_${value}`)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          {isLoading && <div className="h-40 animate-pulse rounded-lg bg-muted" />}
          {error && <p className="text-sm text-destructive">{(error as Error).message}</p>}

          {!isLoading && !error && (
            <>
              <DataTable>
                <DataTableHeader>
                  <DataTableRow>
                    <DataTableHead>{t("coupons.code")}</DataTableHead>
                    <DataTableHead>{t("coupons.type")}</DataTableHead>
                    <DataTableHead>{t("coupons.scope")}</DataTableHead>
                    <DataTableHead>{t("coupons.stack_group")}</DataTableHead>
                    <DataTableHead>{t("coupons.usage")}</DataTableHead>
                    <DataTableHead>{t("common.status")}</DataTableHead>
                    <DataTableHead>{t("coupons.validity")}</DataTableHead>
                    <DataTableHead className="text-right">{t("common.actions")}</DataTableHead>
                  </DataTableRow>
                </DataTableHeader>
                <DataTableBody>
                  {items.length === 0 && <DataTableEmpty colSpan={8} message={t("common.noData")} />}
                  {items.map((coupon) => (
                    <DataTableRow key={coupon.id}>
                      <DataTableCell>
                        <Link
                          to={`/admin/payments/coupons/${coupon.id}`}
                          className="font-mono text-sm font-medium text-primary hover:underline"
                        >
                          {coupon.code}
                        </Link>
                        {coupon.description && (
                          <p className="max-w-[16rem] truncate text-xs text-muted-foreground">{coupon.description}</p>
                        )}
                      </DataTableCell>
                      <DataTableCell className="whitespace-nowrap">{couponValue(coupon, t)}</DataTableCell>
                      <DataTableCell className="max-w-[18rem] truncate text-sm text-muted-foreground">
                        {scopeLabel(coupon)}
                      </DataTableCell>
                      <DataTableCell>
                        <Badge variant="outline">{stackGroupLabel(coupon.stack_group, t)}</Badge>
                      </DataTableCell>
                      <DataTableCell className="whitespace-nowrap text-sm tabular-nums">
                        {coupon.max_redemptions > 0
                          ? t("coupons.redeemed_of", { count: coupon.redeemed_count, max: coupon.max_redemptions })
                          : t("coupons.redeemed", { count: coupon.redeemed_count })}
                      </DataTableCell>
                      <DataTableCell>
                        <Badge className={boolColor(coupon.active)}>
                          {coupon.active ? t("common.active") : t("common.inactive")}
                        </Badge>
                      </DataTableCell>
                      <DataTableCell className="whitespace-nowrap text-xs text-muted-foreground">
                        {coupon.valid_from || coupon.valid_to
                          ? t("coupons.validity_window", {
                              from: coupon.valid_from ? formatDate(coupon.valid_from) : "—",
                              to: coupon.valid_to ? formatDate(coupon.valid_to) : "—",
                            })
                          : t("coupons.validity_always")}
                      </DataTableCell>
                      <DataTableCell>
                        <div className="flex justify-end gap-1">
                          <Button variant="ghost" size="icon" title={t("coupons.edit_title")} asChild>
                            <Link to={`/admin/payments/coupons/${coupon.id}`}>
                              <Pencil className="h-4 w-4" />
                            </Link>
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            title={t("coupons.delete_title")}
                            onClick={() => setDeleting(coupon)}
                          >
                            <Trash2 className="h-4 w-4 text-destructive" />
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

      <AlertDialog open={!!deleting} onOpenChange={(open) => (open ? undefined : setDeleting(null))}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("coupons.delete_title")}</AlertDialogTitle>
            <AlertDialogDescription>{t("coupons.delete_desc")}</AlertDialogDescription>
          </AlertDialogHeader>
          {remove.error && <p className="text-sm text-destructive">{(remove.error as Error).message}</p>}
          <div className="flex justify-end gap-2">
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-white hover:bg-destructive/90"
              onClick={(e) => {
                e.preventDefault()
                if (deleting) remove.mutate(deleting)
              }}
              disabled={remove.isPending}
            >
              {t("common.delete")}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

function couponValue(coupon: Coupon, t: ReturnType<typeof useI18n>["t"]): string {
  if (coupon.type === "percent") return `${(coupon.value / 100).toLocaleString("vi-VN")}%`
  if (coupon.type === "trial") return `${t("coupons.trial_days")}: ${coupon.trial_days}`
  const currency = coupon.currency || "VND"
  return formatMoney(coupon.value, currency, currencyExponent(currency))
}

function stackGroupLabel(value: string, t: ReturnType<typeof useI18n>["t"]): string {
  if (value === "campaign") return t("coupons.stack_group_campaign")
  if (value === "private") return t("coupons.stack_group_private")
  return t("coupons.stack_group_general")
}
