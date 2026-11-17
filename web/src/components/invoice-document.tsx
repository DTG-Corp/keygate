import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { useI18n } from "@/i18n"
import type { InvoiceBuyer, PaymentInvoice } from "@/lib/api"
import { formatMoney } from "@/lib/money"
import { formatDate } from "@/lib/utils"

// InvoiceDocument is the printable part of an invoice: seller and buyer
// blocks, frozen line items and the money breakdown. Both the admin
// detail and the customer portal render it, so the document a buyer sees
// and the document an operator audits cannot drift apart.
export function InvoiceDocument({
  invoice,
  hideMissingSeller = false,
}: {
  invoice: PaymentInvoice
  /** The customer portal has no Settings link to offer, so it omits the
   * seller card when the payload does not include one. */
  hideMissingSeller?: boolean
}) {
  const { t } = useI18n()
  const money = (minor: number) => formatMoney(minor, invoice.currency, invoice.currency_exponent)

  return (
    <div className="space-y-4">
      <div className={`grid gap-4 ${invoice.seller || !hideMissingSeller ? "md:grid-cols-2" : ""}`}>
        {(invoice.seller || !hideMissingSeller) && (
          <Card>
            <CardHeader className="pb-3">
              <CardTitle className="text-sm font-semibold uppercase tracking-wide text-muted-foreground">
                {t("invoices.seller")}
              </CardTitle>
            </CardHeader>
            <CardContent className="space-y-1 text-sm">
              {invoice.seller ? (
                <BuyerBlock buyer={invoice.seller} />
              ) : (
                <p className="text-muted-foreground">{t("invoices.seller_unavailable")}</p>
              )}
            </CardContent>
          </Card>
        )}
        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="text-sm font-semibold uppercase tracking-wide text-muted-foreground">
              {t("invoices.buyer")}
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-1 text-sm">
            <BuyerBlock buyer={invoice.buyer} />
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardContent className="pt-6">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("invoices.description")}</TableHead>
                <TableHead className="text-right">{t("invoices.quantity")}</TableHead>
                <TableHead className="text-right">{t("invoices.unit_price")}</TableHead>
                <TableHead className="text-right">{t("invoices.tax_rate")}</TableHead>
                <TableHead className="text-right">{t("invoices.amount")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {invoice.items.length === 0 && (
                <TableRow>
                  <TableCell colSpan={5} className="text-center text-muted-foreground">
                    {t("common.noData")}
                  </TableCell>
                </TableRow>
              )}
              {invoice.items.map((item) => (
                <TableRow
                  key={`${item.description}|${item.plan_id ?? ""}|${item.quantity}|${item.amount_minor}|${item.tax_rate_bp}`}
                >
                  <TableCell className="max-w-[24rem] whitespace-normal">{item.description || "—"}</TableCell>
                  <TableCell className="text-right tabular-nums">{item.quantity}</TableCell>
                  <TableCell className="text-right tabular-nums">{money(item.unit_amount_minor)}</TableCell>
                  <TableCell className="text-right tabular-nums">
                    {item.tax_rate_bp > 0 ? `${(item.tax_rate_bp / 100).toLocaleString("vi-VN")}%` : "—"}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">{money(item.amount_minor)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>

          <dl className="ml-auto mt-4 max-w-sm space-y-1.5 text-sm">
            <div className="flex items-baseline justify-between gap-4">
              <dt className="text-muted-foreground">{t("pay.subtotal")}</dt>
              <dd className="tabular-nums">{money(invoice.subtotal_minor)}</dd>
            </div>
            {invoice.discount_minor > 0 && (
              <div className="flex items-baseline justify-between gap-4">
                <dt className="text-muted-foreground">{t("pay.discount")}</dt>
                <dd className="tabular-nums text-emerald-700">−{money(invoice.discount_minor)}</dd>
              </div>
            )}
            <div className="flex items-baseline justify-between gap-4">
              <dt className="text-muted-foreground">{t("pay.vat")}</dt>
              <dd className="tabular-nums">{money(invoice.tax_minor)}</dd>
            </div>
            <div className="flex items-baseline justify-between gap-4 border-t pt-2 text-base font-semibold">
              <dt>{t("pay.total")}</dt>
              <dd className="tabular-nums">{money(invoice.total_minor)}</dd>
            </div>
          </dl>
        </CardContent>
      </Card>

      <dl className="grid grid-cols-2 gap-x-4 gap-y-2 text-sm md:grid-cols-4">
        <Field label={t("invoices.number")} value={invoice.number || "—"} mono />
        <Field label={t("invoices.order")} value={invoice.order_id || "—"} mono />
        <Field label={t("common.status")} value={invoice.status} />
        <Field label={t("invoices.issued_at")} value={invoice.issued_at ? formatDate(invoice.issued_at) : "—"} />
        {invoice.e_invoice_provider && (
          <Field
            label={t("invoices.einvoice")}
            value={[invoice.e_invoice_provider, invoice.e_invoice_status, invoice.e_invoice_ref]
              .filter(Boolean)
              .join(" · ")}
          />
        )}
      </dl>
    </div>
  )
}

function BuyerBlock({ buyer }: { buyer: InvoiceBuyer }) {
  const { t } = useI18n()
  const hasAny = buyer.name || buyer.company_name || buyer.tax_code || buyer.address || buyer.email || buyer.phone
  if (!hasAny) return <p className="text-muted-foreground">—</p>
  return (
    <>
      {buyer.name && <p className="font-medium">{buyer.name}</p>}
      {buyer.company_name && <p>{buyer.company_name}</p>}
      {buyer.tax_code && (
        <p>
          <span className="text-muted-foreground">{t("invoices.tax_code")}: </span>
          <span className="font-mono">{buyer.tax_code}</span>
        </p>
      )}
      {buyer.address && <p className="text-muted-foreground">{buyer.address}</p>}
      {buyer.email && <p className="text-muted-foreground">{buyer.email}</p>}
      {buyer.phone && <p className="text-muted-foreground">{buyer.phone}</p>}
    </>
  )
}

function Field({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div>
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className={mono ? "break-all font-mono text-xs" : "break-words"}>{value}</dd>
    </div>
  )
}
