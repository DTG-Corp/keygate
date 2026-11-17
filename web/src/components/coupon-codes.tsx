import { Plus, Tag, X } from "lucide-react"
import { useState } from "react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { useI18n } from "@/i18n"
import type { CouponDiscount, CouponQuote } from "@/lib/api"
import { formatMoney } from "@/lib/money"

// Coupon codes belong to the order, not to the browser: the server
// validates, stacks and prices them (spec §3.9). This component only
// collects the codes as chips and shows the warning the server returned.
// It is shared by the admin preview and any coupon-aware checkout entry;
// the public pay page renders CouponChips read-only instead.
export function CouponCodesInput({
  codes,
  onChange,
  error,
  disabled,
  max,
  id = "coupon-code",
}: {
  codes: string[]
  onChange: (codes: string[]) => void
  /** Server rejection (PAYMENT_COUPON_*), already translated. */
  error?: string
  disabled?: boolean
  max?: number
  id?: string
}) {
  const { t } = useI18n()
  const [draft, setDraft] = useState("")

  const add = () => {
    const code = draft.trim().toUpperCase()
    if (!code) return
    if (codes.some((existing) => existing.toUpperCase() === code)) {
      setDraft("")
      return
    }
    if (max !== undefined && codes.length >= max) return
    onChange([...codes, code])
    setDraft("")
  }

  return (
    <div className="space-y-2">
      <div className="flex gap-2">
        <Input
          id={id}
          value={draft}
          disabled={disabled}
          placeholder={t("coupon.code_placeholder")}
          autoComplete="off"
          spellCheck={false}
          className="font-mono uppercase"
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault()
              add()
            }
          }}
        />
        <Button type="button" variant="outline" onClick={add} disabled={disabled || !draft.trim()}>
          <Plus className="h-4 w-4" />
          {t("coupon.apply")}
        </Button>
      </div>
      {codes.length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {codes.map((code) => (
            <span
              key={code}
              className="inline-flex items-center gap-1 rounded-full border border-primary/20 bg-primary/5 py-0.5 pl-2.5 pr-1 text-xs font-medium text-primary"
            >
              <Tag className="h-3 w-3" />
              <span className="font-mono">{code}</span>
              {!disabled && (
                <button
                  type="button"
                  className="rounded-full p-0.5 hover:bg-primary/10"
                  aria-label={t("coupon.remove", { code })}
                  onClick={() => onChange(codes.filter((item) => item !== code))}
                >
                  <X className="h-3 w-3" />
                </button>
              )}
            </span>
          ))}
        </div>
      )}
      {error && <p className="rounded-md border border-amber-200 bg-amber-50 p-2 text-xs text-amber-800">{error}</p>}
    </div>
  )
}

// CouponChips is the read-only surface of spec §3.13: the hosted pay page
// displays the codes attached to the order and notes that a new order is
// needed to change them. It never re-prices.
export function CouponChips({
  discounts,
  currency,
  exponent,
}: {
  discounts?: CouponDiscount[]
  currency: string
  exponent: number
}) {
  const { t } = useI18n()
  if (!discounts || discounts.length === 0) return null
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap gap-1.5">
        {discounts.map((discount) => (
          <span
            key={discount.code}
            className="inline-flex items-center gap-1 rounded-full border border-emerald-200 bg-emerald-50 px-2.5 py-0.5 text-xs font-medium text-emerald-800"
          >
            <Tag className="h-3 w-3" />
            <span className="font-mono">{discount.code}</span>
            {discount.amount_minor > 0 && (
              <span className="tabular-nums">−{formatMoney(discount.amount_minor, currency, exponent)}</span>
            )}
          </span>
        ))}
      </div>
      <p className="text-xs text-muted-foreground">{t("coupon.read_only_note")}</p>
    </div>
  )
}

// QuoteTotals renders whatever the quote actually returned. A missing
// field is omitted rather than rendered as zero, so a backend that has
// not adopted the richer shape degrades to the total alone.
export function QuoteTotals({
  quote,
  currency,
  exponent,
}: {
  quote: CouponQuote
  currency?: string
  exponent?: number
}) {
  const { t } = useI18n()
  const code = quote.currency || currency || "VND"
  const exp = quote.currency_exponent ?? exponent ?? 0
  const hasBreakdown =
    quote.subtotal_minor !== undefined || quote.discount_minor !== undefined || quote.tax_minor !== undefined
  return (
    <dl className="space-y-1.5 text-sm">
      {quote.subtotal_minor !== undefined && (
        <div className="flex items-baseline justify-between gap-4">
          <dt className="text-muted-foreground">{t("pay.subtotal")}</dt>
          <dd className="tabular-nums">{formatMoney(quote.subtotal_minor, code, exp)}</dd>
        </div>
      )}
      {(quote.discount_minor ?? 0) > 0 && (
        <div className="flex items-baseline justify-between gap-4">
          <dt className="text-muted-foreground">{t("pay.discount")}</dt>
          <dd className="tabular-nums text-emerald-700">−{formatMoney(quote.discount_minor ?? 0, code, exp)}</dd>
        </div>
      )}
      {quote.tax_minor !== undefined && (
        <div className="flex items-baseline justify-between gap-4">
          <dt className="text-muted-foreground">{t("pay.vat")}</dt>
          <dd className="tabular-nums">{formatMoney(quote.tax_minor, code, exp)}</dd>
        </div>
      )}
      {quote.total_minor !== undefined && (
        <div className="flex items-baseline justify-between gap-4 border-t pt-2 font-medium">
          <dt>{t("pay.total")}</dt>
          <dd className="tabular-nums">{formatMoney(quote.total_minor, code, exp)}</dd>
        </div>
      )}
      {!hasBreakdown && quote.total_minor === undefined && (
        <p className="text-xs text-muted-foreground">{t("coupon.quote_unavailable")}</p>
      )}
    </dl>
  )
}
