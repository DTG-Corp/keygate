import { ExternalLink, QrCode } from "lucide-react"
import { CopyButton } from "@/components/copy-button"
import { Button } from "@/components/ui/button"
import { useI18n } from "@/i18n"
import type { PaymentInstructions } from "@/lib/api"
import { formatMoney } from "@/lib/money"
import { safeCheckoutURL, safeImageURL } from "@/lib/safe-url"

// PaymentInstructions renders one provider attempt. Every block is
// optional: SePay answers with a VietQR image/bank block, PayOS with a
// checkout URL and an EMVCo payload, and a re-issued attempt may carry
// only some of them. Missing fields are omitted, never rendered empty.
export function PaymentInstructionsBlock({
  instructions,
  orderCode,
  amountMinor,
  currency,
  currencyExponent,
  showAmount = true,
}: {
  instructions: PaymentInstructions
  orderCode: string
  amountMinor?: number
  currency: string
  currencyExponent: number
  showAmount?: boolean
}) {
  const { t } = useI18n()
  const qrImage = safeImageURL(instructions.qr_image_url)
  const checkoutURL = safeCheckoutURL(instructions.checkout_url)
  const bank = instructions.bank_account
  const hasAny = qrImage || checkoutURL || instructions.qr_payload || instructions.va_number || bank?.account_no

  if (!hasAny) {
    return (
      <p className="rounded-md border bg-muted/40 p-3 text-sm text-muted-foreground">{t("portal.no_instructions")}</p>
    )
  }

  return (
    <div className="space-y-4">
      {showAmount && amountMinor !== undefined && (
        <div className="flex items-baseline justify-between gap-4 text-sm">
          <span className="text-muted-foreground">{t("pay.amount")}</span>
          <span className="text-lg font-semibold tabular-nums">
            {formatMoney(amountMinor, currency, currencyExponent)}
          </span>
        </div>
      )}

      {qrImage && (
        <div className="space-y-3">
          <p className="text-sm text-muted-foreground">{t("pay.qr_hint")}</p>
          <img src={qrImage} alt="VietQR" className="mx-auto h-56 w-56 rounded-md border bg-white object-contain p-2" />
        </div>
      )}

      {checkoutURL && (
        <div className="space-y-2">
          <p className="text-sm text-muted-foreground">{t("pay.checkout_hint")}</p>
          <Button asChild className="w-full">
            <a href={checkoutURL} target="_blank" rel="noopener noreferrer">
              <ExternalLink className="h-4 w-4" /> {t("pay.pay_now")}
            </a>
          </Button>
        </div>
      )}

      {!qrImage && !checkoutURL && instructions.qr_payload && (
        <div className="space-y-1">
          <p className="flex items-center gap-1.5 text-sm text-muted-foreground">
            <QrCode className="h-4 w-4" /> {t("portal.qr_payload")}
          </p>
          <div className="flex flex-wrap items-center gap-2 rounded-md border bg-muted/40 p-2">
            <code className="max-w-full break-all text-xs">{instructions.qr_payload}</code>
            <CopyButton value={instructions.qr_payload} />
          </div>
        </div>
      )}

      {(bank?.account_no || instructions.va_number) && (
        <dl className="grid grid-cols-[auto,1fr] gap-x-4 gap-y-2 text-sm">
          {bank?.bank_name && (
            <>
              <dt className="text-muted-foreground">{t("pay.bank")}</dt>
              <dd>{bank.bank_name}</dd>
            </>
          )}
          {bank?.account_no && (
            <>
              <dt className="text-muted-foreground">{t("pay.account")}</dt>
              <dd className="flex flex-wrap items-center gap-1 font-mono">
                {bank.account_no}
                <CopyButton value={bank.account_no} />
              </dd>
            </>
          )}
          {bank?.account_name && (
            <>
              <dt className="text-muted-foreground">{t("pay.account_name")}</dt>
              <dd>{bank.account_name}</dd>
            </>
          )}
          {instructions.va_number && (
            <>
              <dt className="text-muted-foreground">{t("portal.va_number")}</dt>
              <dd className="flex flex-wrap items-center gap-1 font-mono">
                {instructions.va_number}
                <CopyButton value={instructions.va_number} />
              </dd>
            </>
          )}
          <dt className="text-muted-foreground">{t("pay.content")}</dt>
          <dd className="flex flex-wrap items-center gap-1 font-mono font-medium">
            {instructions.description ?? orderCode}
            <CopyButton value={instructions.description ?? orderCode} />
          </dd>
        </dl>
      )}

      {instructions.provider_ref && (
        <p className="text-xs text-muted-foreground">
          {t("admin.provider_ref")}: <span className="font-mono">{instructions.provider_ref}</span>
        </p>
      )}

      {(bank?.account_no || instructions.va_number) && (
        <p className="rounded-md bg-amber-50 p-3 text-xs text-amber-800">{t("pay.transfer_warning")}</p>
      )}
    </div>
  )
}
