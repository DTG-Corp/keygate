import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { ArrowLeft, CheckCircle2, Download, Loader2, XCircle } from "lucide-react"
import { useState } from "react"
import { Link, useParams } from "react-router-dom"
import { InvoiceDocument } from "@/components/invoice-document"
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
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { useI18n } from "@/i18n"
import { adminInvoices } from "@/lib/api"
import { saveBlob } from "@/lib/download"
import { invoiceStatusClass, invoiceStatusLabel } from "@/lib/invoice-status"
import { formatMoney } from "@/lib/money"

export default function PaymentInvoiceDetailPage() {
  const { t } = useI18n()
  const { id } = useParams<{ id: string }>()
  const qc = useQueryClient()
  const [issueOpen, setIssueOpen] = useState(false)
  const [voidOpen, setVoidOpen] = useState(false)
  const [reason, setReason] = useState("")
  const [downloading, setDownloading] = useState(false)
  const [actionError, setActionError] = useState("")

  const detail = useQuery({
    queryKey: ["admin", "invoices", "detail", id],
    queryFn: () => adminInvoices.get(id ?? ""),
    enabled: !!id,
  })
  const invoice = detail.data

  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["admin", "invoices"] })
  }

  const issue = useMutation({
    meta: { ownErrors: true },
    mutationFn: () => adminInvoices.issue(id ?? ""),
    onSuccess: () => {
      refresh()
      setIssueOpen(false)
      setActionError("")
      showToast(t("invoices.issued"), "success")
    },
    onError: (err: Error) => setActionError(err.message),
  })

  const voidInvoice = useMutation({
    meta: { ownErrors: true },
    mutationFn: () => adminInvoices.void(id ?? "", reason.trim()),
    onSuccess: () => {
      refresh()
      setVoidOpen(false)
      setReason("")
      setActionError("")
      showToast(t("invoices.voided"), "success")
    },
    onError: (err: Error) => setActionError(err.message),
  })

  const downloadPDF = async () => {
    if (!invoice) return
    setDownloading(true)
    try {
      saveBlob(await adminInvoices.pdf(invoice.id), `${invoice.number || "invoice"}.pdf`)
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
          <Link to="/admin/payments/invoices">
            <ArrowLeft className="h-4 w-4" /> {t("invoices.back")}
          </Link>
        </Button>
      </div>
    )
  }
  if (!invoice) return <p className="text-sm text-muted-foreground">{t("invoices.not_found")}</p>

  // The service issues only a pending_issue invoice; a draft is not
  // issuable yet (the API answers INVOICE_ALREADY_ISSUED). Voiding is
  // allowed for issued and pending_issue, never for an already-void one.
  const canIssue = invoice.status === "pending_issue"
  const canVoid = invoice.status === "issued" || invoice.status === "pending_issue"
  const error = actionError || (issue.error as Error)?.message || (voidInvoice.error as Error)?.message

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="space-y-1">
          <Button variant="ghost" size="sm" className="-ml-2" asChild>
            <Link to="/admin/payments/invoices">
              <ArrowLeft className="h-4 w-4" /> {t("invoices.back")}
            </Link>
          </Button>
          <h1 className="flex items-center gap-2 font-mono text-2xl font-bold tracking-tight">
            {invoice.number || invoice.id}
            <Badge className={invoiceStatusClass(invoice.status)}>{invoiceStatusLabel(invoice.status, t)}</Badge>
          </h1>
          <p className="text-sm text-muted-foreground">
            {t("invoices.order")}: <span className="font-mono">{invoice.order_id || "—"}</span>
            {" · "}
            {formatMoney(invoice.total_minor, invoice.currency, invoice.currency_exponent)}
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button variant="outline" onClick={downloadPDF} disabled={downloading}>
            {downloading ? <Loader2 className="h-4 w-4 animate-spin" /> : <Download className="h-4 w-4" />}
            {t("invoices.download_pdf")}
          </Button>
          {canIssue && (
            <Button
              onClick={() => {
                setActionError("")
                setIssueOpen(true)
              }}
            >
              <CheckCircle2 className="h-4 w-4" /> {t("invoices.issue")}
            </Button>
          )}
          {canVoid && (
            <Button
              variant="outline"
              className="text-destructive"
              onClick={() => {
                setActionError("")
                setVoidOpen(true)
              }}
            >
              <XCircle className="h-4 w-4" /> {t("invoices.void")}
            </Button>
          )}
        </div>
      </div>

      {error && (
        <p className="rounded-md border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive">{error}</p>
      )}

      {invoice.status === "void" && (
        <p className="rounded-md border border-red-200 bg-red-50 p-3 text-sm text-red-800">{t("invoices.void_desc")}</p>
      )}

      <Card>
        <CardContent className="pt-6">
          <InvoiceDocument invoice={invoice} />
        </CardContent>
      </Card>

      <AlertDialog open={issueOpen} onOpenChange={setIssueOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("invoices.issue_confirm")}</AlertDialogTitle>
            <AlertDialogDescription>{t("invoices.issue_desc")}</AlertDialogDescription>
          </AlertDialogHeader>
          <div className="flex justify-end gap-2">
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                e.preventDefault()
                issue.mutate()
              }}
              disabled={issue.isPending}
            >
              {issue.isPending && <Loader2 className="h-4 w-4 animate-spin" />}
              {t("invoices.issue")}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={voidOpen} onOpenChange={setVoidOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("invoices.void_title")}</AlertDialogTitle>
            <AlertDialogDescription>{t("invoices.void_desc")}</AlertDialogDescription>
          </AlertDialogHeader>
          <div className="space-y-1.5">
            <Label htmlFor="void-reason">{t("invoices.void_reason")}</Label>
            <Input id="void-reason" value={reason} onChange={(e) => setReason(e.target.value)} />
          </div>
          <div className="flex justify-end gap-2">
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-white hover:bg-destructive/90"
              onClick={(e) => {
                e.preventDefault()
                voidInvoice.mutate()
              }}
              disabled={voidInvoice.isPending}
            >
              {voidInvoice.isPending && <Loader2 className="h-4 w-4 animate-spin" />}
              {t("invoices.void")}
            </AlertDialogAction>
          </div>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}
