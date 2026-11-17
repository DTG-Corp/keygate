import { useQuery } from "@tanstack/react-query"
import { ArrowLeft, Download, Loader2 } from "lucide-react"
import { useState } from "react"
import { Link, useParams } from "react-router-dom"
import { InvoiceDocument } from "@/components/invoice-document"
import { showToast } from "@/components/toast"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { useI18n } from "@/i18n"
import { portalInvoices } from "@/lib/api"
import { saveBlob } from "@/lib/download"
import { invoiceStatusClass, invoiceStatusLabel } from "@/lib/invoice-status"

export default function PortalInvoiceDetailPage() {
  const { t } = useI18n()
  const { id } = useParams<{ id: string }>()
  const [downloading, setDownloading] = useState(false)

  const detail = useQuery({
    queryKey: ["portal", "invoices", "detail", id],
    queryFn: () => portalInvoices.get(id ?? ""),
    enabled: !!id,
  })
  const invoice = detail.data

  const download = async () => {
    if (!invoice) return
    setDownloading(true)
    try {
      saveBlob(await portalInvoices.pdf(invoice.id), `${invoice.number || "invoice"}.pdf`)
    } catch (err) {
      showToast((err as Error).message, "error")
    } finally {
      setDownloading(false)
    }
  }

  if (detail.isLoading) return <div className="h-48 animate-pulse rounded-lg bg-muted" />
  if (detail.error) {
    return (
      <div className="space-y-3">
        <p className="text-sm text-destructive">{(detail.error as Error).message}</p>
        <Button variant="outline" asChild>
          <Link to="/portal/invoices">
            <ArrowLeft className="h-4 w-4" /> {t("portal.invoiceBack")}
          </Link>
        </Button>
      </div>
    )
  }
  if (!invoice) return <p className="text-sm text-muted-foreground">{t("portal.invoice_not_found")}</p>

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="space-y-1">
          <Button variant="ghost" size="sm" className="-ml-2" asChild>
            <Link to="/portal/invoices">
              <ArrowLeft className="h-4 w-4" /> {t("portal.invoiceBack")}
            </Link>
          </Button>
          <h1 className="flex flex-wrap items-center gap-2 font-mono text-2xl font-bold tracking-tight">
            {invoice.number || invoice.id}
            <Badge className={invoiceStatusClass(invoice.status)}>{invoiceStatusLabel(invoice.status, t)}</Badge>
          </h1>
        </div>
        <Button variant="outline" onClick={download} disabled={downloading}>
          {downloading ? <Loader2 className="h-4 w-4 animate-spin" /> : <Download className="h-4 w-4" />}
          {t("portal.downloadPdf")}
        </Button>
      </div>

      <InvoiceDocument invoice={invoice} hideMissingSeller />
    </div>
  )
}
