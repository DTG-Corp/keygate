import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { EyeOff, Link2 } from "lucide-react"
import { useState } from "react"
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
import { useI18n } from "@/i18n"
import { adminPayments, type PaymentTransactionRow } from "@/lib/api"
import { currencyExponent, formatMoney } from "@/lib/money"
import { formatDate } from "@/lib/utils"

export default function PaymentUnmatchedPage() {
  const { t } = useI18n()
  const qc = useQueryClient()
  const { data, isLoading, error } = useQuery({
    queryKey: ["admin", "payments", "unmatched"],
    queryFn: () => adminPayments.unmatched(50),
  })
  const rows = data?.transactions ?? []

  const [matching, setMatching] = useState<PaymentTransactionRow | null>(null)
  const [orderCode, setOrderCode] = useState("")
  const [ignoring, setIgnoring] = useState<PaymentTransactionRow | null>(null)
  const [ignoreReason, setIgnoreReason] = useState("")
  const [actionError, setActionError] = useState("")

  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["admin", "payments"] })
  }

  const matchMut = useMutation({
    mutationFn: ({ id, code }: { id: string; code: string }) => adminPayments.match(id, code),
    onSuccess: () => {
      refresh()
      setMatching(null)
      setOrderCode("")
      setActionError("")
      showToast(t("admin.matched_ok"), "success")
    },
    meta: { ownErrors: true },
    onError: (e: Error) => setActionError(e.message),
  })
  const ignoreMut = useMutation({
    mutationFn: ({ id, reason }: { id: string; reason: string }) => adminPayments.ignore(id, reason),
    onSuccess: () => {
      refresh()
      setIgnoring(null)
      setIgnoreReason("")
      setActionError("")
      showToast(t("admin.ignored_ok"), "success")
    },
    meta: { ownErrors: true },
    onError: (e: Error) => setActionError(e.message),
  })

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold tracking-tight sr-only md:not-sr-only">{t("admin.unmatched")}</h1>
        <p className="text-muted-foreground">{t("admin.unmatched_subtitle")}</p>
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
            <DataTable>
              <DataTableHeader>
                <DataTableRow>
                  <DataTableHead>{t("admin.provider")}</DataTableHead>
                  <DataTableHead>{t("admin.provider_ref")}</DataTableHead>
                  <DataTableHead>{t("pay.amount")}</DataTableHead>
                  <DataTableHead>{t("admin.content")}</DataTableHead>
                  <DataTableHead>{t("admin.occurred_at")}</DataTableHead>
                  <DataTableHead className="w-40 text-right">{t("common.actions")}</DataTableHead>
                </DataTableRow>
              </DataTableHeader>
              <DataTableBody>
                {rows.length === 0 && <DataTableEmpty colSpan={6} message={t("admin.unmatched_empty")} />}
                {rows.map((row) => (
                  <DataTableRow key={row.id}>
                    <DataTableCell className="text-sm">{providerName(row.provider)}</DataTableCell>
                    <DataTableCell className="max-w-[10rem] truncate font-mono text-xs">
                      {row.provider_ref}
                    </DataTableCell>
                    <DataTableCell className="whitespace-nowrap tabular-nums">
                      {formatMoney(row.amount_minor, row.currency, currencyExponent(row.currency))}
                    </DataTableCell>
                    <DataTableCell className="max-w-[18rem] whitespace-normal break-words text-xs">
                      {row.content || "—"}
                    </DataTableCell>
                    <DataTableCell className="whitespace-nowrap text-xs text-muted-foreground">
                      {formatDate(row.occurred_at)}
                    </DataTableCell>
                    <DataTableCell>
                      <div className="flex justify-end gap-1">
                        <Button
                          variant="outline"
                          size="sm"
                          onClick={() => {
                            setActionError("")
                            setMatching(row)
                          }}
                        >
                          <Link2 className="mr-1.5 h-3.5 w-3.5" /> {t("admin.match_order")}
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => {
                            setActionError("")
                            setIgnoreReason("")
                            setIgnoring(row)
                          }}
                        >
                          <EyeOff className="mr-1.5 h-3.5 w-3.5" /> {t("admin.ignore")}
                        </Button>
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
        open={!!matching}
        onOpenChange={(open) => {
          if (!open) {
            setMatching(null)
            setActionError("")
          }
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("admin.match_order")}</DialogTitle>
            <DialogDescription>{t("admin.match_hint")}</DialogDescription>
          </DialogHeader>
          <DialogBody className="space-y-3">
            {actionError && <p className="text-sm text-destructive">{actionError}</p>}
            <p className="text-xs text-muted-foreground">
              {providerName(matching?.provider ?? "")} · {matching?.provider_ref} ·{" "}
              {matching
                ? formatMoney(matching.amount_minor, matching.currency, currencyExponent(matching.currency))
                : ""}
            </p>
            <div className="space-y-2">
              <Label htmlFor="match-order-code">{t("admin.order_code")}</Label>
              <Input
                id="match-order-code"
                value={orderCode}
                onChange={(e) => setOrderCode(e.target.value.toUpperCase())}
                placeholder="KG7F3K2Q"
                autoComplete="off"
              />
            </div>
          </DialogBody>
          <DialogFooter>
            <Button variant="outline" onClick={() => setMatching(null)}>
              {t("common.cancel")}
            </Button>
            <Button
              onClick={() => matching && matchMut.mutate({ id: matching.id, code: orderCode.trim().toUpperCase() })}
              disabled={matchMut.isPending || !orderCode.trim()}
            >
              {t("admin.match_order")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog
        open={!!ignoring}
        onOpenChange={(open) => {
          if (!open) {
            setIgnoring(null)
            setActionError("")
          }
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("admin.ignore")}</DialogTitle>
            <DialogDescription>{t("admin.ignore_reason")}</DialogDescription>
          </DialogHeader>
          <DialogBody className="space-y-3">
            {actionError && <p className="text-sm text-destructive">{actionError}</p>}
            <div className="space-y-2">
              <Label htmlFor="ignore-reason">{t("admin.ignore_reason")}</Label>
              <Input id="ignore-reason" value={ignoreReason} onChange={(e) => setIgnoreReason(e.target.value)} />
            </div>
          </DialogBody>
          <DialogFooter>
            <Button variant="outline" onClick={() => setIgnoring(null)}>
              {t("common.cancel")}
            </Button>
            <Button
              variant="destructive"
              onClick={() => ignoring && ignoreMut.mutate({ id: ignoring.id, reason: ignoreReason.trim() })}
              disabled={ignoreMut.isPending || !ignoreReason.trim()}
            >
              {t("admin.ignore")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

function providerName(provider: string): string {
  if (provider === "sepay") return "SePay"
  if (provider === "payos") return "PayOS"
  return provider
}
