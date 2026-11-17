import { useQuery } from "@tanstack/react-query"
import { Download, Eye, Loader2 } from "lucide-react"
import { useState } from "react"
import { Link } from "react-router-dom"
import { showToast } from "@/components/toast"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { useI18n } from "@/i18n"
import { portalInvoices } from "@/lib/api"
import { saveBlob } from "@/lib/download"
import { invoiceStatusClass, invoiceStatusLabel } from "@/lib/invoice-status"
import { formatMoney } from "@/lib/money"
import { formatDateVN } from "@/lib/utils"

export default function PortalInvoicesPage() {
  const { t } = useI18n()
  const { data, isLoading, error } = useQuery({
    queryKey: ["portal", "invoices"],
    queryFn: () => portalInvoices.list({ limit: 100 }),
  })
  const invoices = data?.invoices ?? []
  const [downloading, setDownloading] = useState<string | null>(null)

  const download = async (id: string, number: string) => {
    setDownloading(id)
    try {
      saveBlob(await portalInvoices.pdf(id), `${number || "invoice"}.pdf`)
    } catch (err) {
      showToast((err as Error).message, "error")
    } finally {
      setDownloading(null)
    }
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold tracking-tight">{t("portal.invoices")}</h1>
        <p className="text-muted-foreground">{t("portal.invoicesDesc")}</p>
      </div>

      {isLoading && <div className="h-40 animate-pulse rounded-lg bg-muted" />}
      {error && <p className="text-sm text-destructive">{(error as Error).message}</p>}

      {!isLoading && !error && invoices.length === 0 && (
        <Card>
          <CardContent className="py-12 text-center text-muted-foreground">{t("portal.no_invoices")}</CardContent>
        </Card>
      )}

      {invoices.length > 0 && (
        <div className="overflow-x-auto rounded-xl border bg-card">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b bg-muted/40 text-left text-xs text-muted-foreground">
                <th className="p-3">{t("portal.invoiceNumber")}</th>
                <th className="p-3">{t("portal.invoiceOrder")}</th>
                <th className="p-3">{t("portal.invoiceIssued")}</th>
                <th className="p-3">{t("invoices.status")}</th>
                <th className="p-3 text-right">{t("portal.invoiceTotal")}</th>
                <th className="p-3 text-right">{t("common.actions")}</th>
              </tr>
            </thead>
            <tbody>
              {invoices.map((invoice) => (
                <tr key={invoice.id} className="border-b last:border-0">
                  <td className="p-3 font-mono">{invoice.number || invoice.id}</td>
                  <td className="p-3 font-mono text-xs">{invoice.order_id || "—"}</td>
                  <td className="p-3 text-muted-foreground">
                    {invoice.issued_at ? formatDateVN(invoice.issued_at) : "—"}
                  </td>
                  <td className="p-3">
                    <Badge className={invoiceStatusClass(invoice.status)}>
                      {invoiceStatusLabel(invoice.status, t)}
                    </Badge>
                  </td>
                  <td className="p-3 text-right tabular-nums">
                    {formatMoney(invoice.total_minor, invoice.currency, invoice.currency_exponent)}
                  </td>
                  <td className="p-3">
                    <div className="flex justify-end gap-1">
                      <Button variant="ghost" size="icon" title={t("portal.viewInvoice")} asChild>
                        <Link to={`/portal/invoices/${invoice.id}`}>
                          <Eye className="h-4 w-4" />
                        </Link>
                      </Button>
                      <Button
                        variant="ghost"
                        size="icon"
                        title={t("portal.downloadPdf")}
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
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
