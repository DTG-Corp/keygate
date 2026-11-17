import { useQuery } from "@tanstack/react-query"
import { Download, Eye, Loader2, Search } from "lucide-react"
import { useState } from "react"
import { Link } from "react-router-dom"
import { showToast } from "@/components/toast"
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
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { useI18n } from "@/i18n"
import { adminInvoices } from "@/lib/api"
import { saveBlob } from "@/lib/download"
import { invoiceStatusClass, invoiceStatusLabel } from "@/lib/invoice-status"
import { formatMoney } from "@/lib/money"
import { useDebounced } from "@/lib/use-debounced"
import { formatDateVN } from "@/lib/utils"

const STATUSES = ["draft", "pending_issue", "issued", "void"] as const

export default function PaymentInvoicesPage() {
  const { t } = useI18n()
  const [status, setStatus] = useState("all")
  const [number, setNumber] = useState("")
  const [order, setOrder] = useState("")
  const [from, setFrom] = useState("")
  const [to, setTo] = useState("")
  const numberQuery = useDebounced(number, 300)
  const orderQuery = useDebounced(order, 300)
  const pg = useServerPagination(20, [status, numberQuery, orderQuery, from, to])

  const filter = {
    status: status === "all" ? undefined : status,
    number: numberQuery || undefined,
    order_id: orderQuery || undefined,
    // Bare dates are whole Vietnam days on the server; a browser-local
    // instant would move the window for operators in other zones.
    from: from || undefined,
    to: to || undefined,
  }

  const { data, isLoading, error } = useQuery({
    queryKey: ["admin", "invoices", filter, pg.page, pg.pageSize],
    queryFn: () => adminInvoices.list({ ...filter, ...pg.params }),
  })
  const { items, total, totalPages } = pg.from(data, data?.invoices)

  const [downloading, setDownloading] = useState<string | null>(null)
  const download = async (id: string, numberLabel: string) => {
    setDownloading(id)
    try {
      saveBlob(await adminInvoices.pdf(id), `${numberLabel || "invoice"}.pdf`)
    } catch (err) {
      showToast((err as Error).message, "error")
    } finally {
      setDownloading(null)
    }
  }

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-2xl font-bold tracking-tight">{t("invoices.title")}</h1>
        <p className="text-sm text-muted-foreground">{t("invoices.subtitle")}</p>
      </div>

      <Card>
        <CardContent className="space-y-4 pt-6">
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
            <div className="space-y-1.5">
              <Label>{t("invoices.filter_status")}</Label>
              <Select value={status} onValueChange={setStatus}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="all">{t("common.all")}</SelectItem>
                  {STATUSES.map((value) => (
                    <SelectItem key={value} value={value}>
                      {t(`invoices.status_${value}`)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="invoice-number">{t("invoices.filter_number")}</Label>
              <div className="relative">
                <Search className="pointer-events-none absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
                <Input
                  id="invoice-number"
                  className="pl-8 font-mono"
                  value={number}
                  onChange={(e) => setNumber(e.target.value)}
                />
              </div>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="invoice-order">{t("invoices.filter_order")}</Label>
              <Input
                id="invoice-order"
                className="font-mono"
                value={order}
                onChange={(e) => setOrder(e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="invoice-from">{t("common.from")}</Label>
              <Input id="invoice-from" type="date" value={from} onChange={(e) => setFrom(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="invoice-to">{t("common.to")}</Label>
              <Input id="invoice-to" type="date" value={to} onChange={(e) => setTo(e.target.value)} />
            </div>
          </div>

          {isLoading && <div className="h-40 animate-pulse rounded-lg bg-muted" />}
          {error && <p className="text-sm text-destructive">{(error as Error).message}</p>}

          {!isLoading && !error && (
            <>
              <DataTable>
                <DataTableHeader>
                  <DataTableRow>
                    <DataTableHead>{t("invoices.number")}</DataTableHead>
                    <DataTableHead>{t("invoices.order")}</DataTableHead>
                    <DataTableHead>{t("invoices.buyer")}</DataTableHead>
                    <DataTableHead>{t("invoices.total")}</DataTableHead>
                    <DataTableHead>{t("invoices.status")}</DataTableHead>
                    <DataTableHead>{t("invoices.issued_at")}</DataTableHead>
                    <DataTableHead className="text-right">{t("common.actions")}</DataTableHead>
                  </DataTableRow>
                </DataTableHeader>
                <DataTableBody>
                  {items.length === 0 && <DataTableEmpty colSpan={7} message={t("common.noData")} />}
                  {items.map((invoice) => (
                    <DataTableRow key={invoice.id}>
                      <DataTableCell>
                        <Link
                          to={`/admin/payments/invoices/${invoice.id}`}
                          className="font-mono text-sm font-medium text-primary hover:underline"
                        >
                          {invoice.number || invoice.id}
                        </Link>
                      </DataTableCell>
                      <DataTableCell className="font-mono text-xs">{invoice.order_id || "—"}</DataTableCell>
                      <DataTableCell className="max-w-[16rem] truncate">
                        <div className="text-sm">{invoice.buyer.company_name || invoice.buyer.name || "—"}</div>
                        {invoice.buyer.tax_code && (
                          <div className="font-mono text-xs text-muted-foreground">{invoice.buyer.tax_code}</div>
                        )}
                      </DataTableCell>
                      <DataTableCell className="whitespace-nowrap tabular-nums">
                        {formatMoney(invoice.total_minor, invoice.currency, invoice.currency_exponent)}
                      </DataTableCell>
                      <DataTableCell>
                        <Badge className={invoiceStatusClass(invoice.status)}>
                          {invoiceStatusLabel(invoice.status, t)}
                        </Badge>
                      </DataTableCell>
                      <DataTableCell className="whitespace-nowrap text-sm text-muted-foreground">
                        {invoice.issued_at ? formatDateVN(invoice.issued_at) : "—"}
                      </DataTableCell>
                      <DataTableCell>
                        <div className="flex justify-end gap-1">
                          <Button variant="ghost" size="icon" title={t("common.view")} asChild>
                            <Link to={`/admin/payments/invoices/${invoice.id}`}>
                              <Eye className="h-4 w-4" />
                            </Link>
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon"
                            title={t("invoices.download_pdf")}
                            disabled={downloading === invoice.id}
                            onClick={() => download(invoice.id, invoice.number)}
                          >
                            {downloading === invoice.id ? (
                              <Loader2 className="h-4 w-4 animate-spin" />
                            ) : (
                              <Download className="h-4 w-4" />
                            )}
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
    </div>
  )
}
