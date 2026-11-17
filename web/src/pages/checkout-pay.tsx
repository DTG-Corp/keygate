import { useQuery } from "@tanstack/react-query"
import { CheckCircle2, Clock3, ExternalLink, Loader2, XCircle } from "lucide-react"
import { useEffect, useState } from "react"
import { useParams } from "react-router-dom"
import { CopyButton } from "@/components/copy-button"
import { CouponChips } from "@/components/coupon-codes"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { useSiteConfig } from "@/hooks/use-site-config"
import { useI18n } from "@/i18n"
import { checkoutPay, errorCode, type PayStatus } from "@/lib/api"
import { formatMoney } from "@/lib/money"
import { safeCheckoutURL, safeImageURL } from "@/lib/safe-url"

// The pay page is public: the order code in the URL is the capability,
// and the API it polls deliberately returns no buyer identity, license
// key or payment secret. It must never ask for or transmit more than
// that code.
export default function CheckoutPayPage() {
  const { t } = useI18n()
  const { site_name, logo_url, attribution_text, attribution_url } = useSiteConfig()
  const { code = "" } = useParams<{ code: string }>()
  const { data, error, isLoading, refetch, isFetching } = useQuery({
    queryKey: ["pay-status", code],
    queryFn: () => checkoutPay.status(code),
    enabled: !!code,
    // Poll while the order can still be paid, then stop. A paid page
    // that keeps polling hammers the API from every open tab.
    refetchInterval: (query) => (query.state.data?.status === "pending" ? 3000 : false),
  })

  return (
    <div className="flex min-h-screen flex-col bg-muted/30">
      <header className="border-b bg-card">
        <div className="mx-auto flex h-14 w-full max-w-lg items-center gap-2 px-4">
          <img src={logo_url || "/logo.svg"} alt="" className="h-7 w-7" />
          <span className="truncate font-bold tracking-tight">{site_name}</span>
        </div>
      </header>

      <main className="mx-auto w-full max-w-lg flex-1 space-y-4 px-4 py-8">
        {isLoading && (
          <div className="flex flex-col items-center gap-3 py-16 text-muted-foreground">
            <Loader2 className="h-8 w-8 animate-spin" />
            <p className="text-sm">{t("pay.loading")}</p>
          </div>
        )}

        {error && !data && (
          <Card>
            <CardContent className="space-y-4 py-10 text-center">
              <XCircle className="mx-auto h-10 w-10 text-destructive" />
              <p className="text-sm text-destructive">
                {errorCode(error) === "NOT_FOUND" ? t("pay.not_found") : (error as Error).message}
              </p>
              <Button variant="outline" onClick={() => refetch()} disabled={isFetching}>
                {t("common.retry")}
              </Button>
            </CardContent>
          </Card>
        )}

        {/* A failed poll must not blank an order the buyer is already
            looking at: keep the last status on screen and warn instead. */}
        {data && error && (
          <p className="rounded-md border border-amber-200 bg-amber-50 p-3 text-sm text-amber-800">
            {t("pay.stale_warning")}
          </p>
        )}

        {data && <PayOrder data={data} />}
      </main>

      {/* Attribution required by AGPL v3 Section 7(b) — see NOTICE */}
      <footer className="border-t bg-card px-4 py-3 text-center">
        <a
          href={attribution_url}
          target="_blank"
          rel="noopener noreferrer"
          className="text-xs text-muted-foreground/60 transition-colors hover:text-muted-foreground"
        >
          {attribution_text}
        </a>
      </footer>
    </div>
  )
}

function PayOrder({ data }: { data: PayStatus }) {
  const { t } = useI18n()
  const amount = formatMoney(data.amount_minor, data.currency, data.currency_exponent)
  const subtotal =
    data.subtotal_minor ??
    (data.tax_minor !== undefined ? data.amount_minor + (data.discount_minor ?? 0) - data.tax_minor : undefined)
  const qrImage = safeImageURL(data.qr_image_url)
  // checkout_url comes from the provider's API response, not from us:
  // validate it before it reaches an href. Only an https URL on a host
  // the checkout rail actually returns is offered; anything else is
  // treated as absent, so the button disappears instead of redirecting
  // the buyer to an untrusted origin.
  const checkoutURL = safeCheckoutURL(data.checkout_url)
  const vatLabel =
    data.tax_rate_bp && data.tax_rate_bp > 0
      ? `${t("pay.vat")} (${(data.tax_rate_bp / 100).toLocaleString("vi-VN")}%)`
      : t("pay.vat")

  return (
    <>
      <Card>
        <CardHeader className="space-y-2">
          <div className="flex items-start justify-between gap-3">
            <CardTitle className="text-xl">{t("pay.title")}</CardTitle>
            <StatusBadge status={data.status} />
          </div>
          <p className="text-sm text-muted-foreground">
            {t("pay.order")} <span className="font-mono font-medium text-foreground">{data.code}</span>
          </p>
        </CardHeader>
        <CardContent className="space-y-4">
          <dl className="space-y-2 text-sm">
            {subtotal !== undefined && (
              <div className="flex items-baseline justify-between gap-4">
                <dt className="text-muted-foreground">{t("pay.subtotal")}</dt>
                <dd className="tabular-nums">{formatMoney(subtotal, data.currency, data.currency_exponent)}</dd>
              </div>
            )}
            {data.discount_minor !== undefined && data.discount_minor > 0 && (
              <div className="flex items-baseline justify-between gap-4">
                <dt className="text-muted-foreground">{t("pay.discount")}</dt>
                <dd className="tabular-nums text-emerald-700">
                  -{formatMoney(data.discount_minor, data.currency, data.currency_exponent)}
                </dd>
              </div>
            )}
            {data.tax_minor !== undefined && (
              <div className="flex items-baseline justify-between gap-4">
                <dt className="text-muted-foreground">{vatLabel}</dt>
                <dd className="tabular-nums">{formatMoney(data.tax_minor, data.currency, data.currency_exponent)}</dd>
              </div>
            )}
            <div className="flex items-baseline justify-between gap-4 border-t pt-3">
              <dt className="font-medium">{t("pay.total")}</dt>
              <dd className="text-lg font-semibold tabular-nums">{amount}</dd>
            </div>
          </dl>

          {/* Read-only by contract: this page displays codes already
              attached to the order and never re-prices it. */}
          <CouponChips discounts={data.discounts} currency={data.currency} exponent={data.currency_exponent} />

          {data.status === "pending" && data.expires_at && (
            <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <Clock3 className="h-3.5 w-3.5" />
              <Countdown key={data.expires_at} expiresAt={data.expires_at} />
            </p>
          )}
        </CardContent>
      </Card>

      {data.status === "pending" && (
        <Card>
          <CardContent className="space-y-4 pt-6">
            <p className="text-sm text-muted-foreground">{qrImage ? t("pay.qr_hint") : t("pay.checkout_hint")}</p>
            <p className="text-xs font-medium text-muted-foreground">
              {checkoutURL ? t("pay.gateway") : t("pay.bank_transfer")}
            </p>

            {qrImage && (
              <img
                src={qrImage}
                alt="VietQR"
                className="mx-auto h-56 w-56 rounded-md border bg-white object-contain p-2"
              />
            )}

            {data.bank && (
              <dl className="grid grid-cols-[auto,1fr] gap-x-4 gap-y-2 text-sm">
                <dt className="text-muted-foreground">{t("pay.bank")}</dt>
                <dd>{data.bank.bank_name}</dd>
                <dt className="text-muted-foreground">{t("pay.account")}</dt>
                <dd className="flex flex-wrap items-center gap-1 font-mono">
                  {data.bank.account_no}
                  <CopyButton value={data.bank.account_no} />
                </dd>
                <dt className="text-muted-foreground">{t("pay.account_name")}</dt>
                <dd>{data.bank.account_name}</dd>
                <dt className="text-muted-foreground">{t("pay.content")}</dt>
                <dd className="flex flex-wrap items-center gap-1 font-mono font-medium">
                  {data.description ?? data.code}
                  <CopyButton value={data.description ?? data.code} />
                </dd>
              </dl>
            )}

            {data.bank && (
              <p className="rounded-md bg-amber-50 p-3 text-xs text-amber-800">{t("pay.transfer_warning")}</p>
            )}

            {checkoutURL && (
              <Button asChild className="w-full">
                <a href={checkoutURL} target="_blank" rel="noopener noreferrer">
                  <ExternalLink className="h-4 w-4" /> {t("pay.pay_now")}
                </a>
              </Button>
            )}
          </CardContent>
        </Card>
      )}

      {data.status === "paid" && (
        <section className="rounded-lg border border-emerald-200 bg-emerald-50 p-4">
          <p className="flex items-center gap-2 font-medium text-emerald-800">
            <CheckCircle2 className="h-5 w-5" /> {t("pay.paid")}
          </p>
          <p className="mt-1 text-sm text-emerald-700">{t("pay.paid_hint")}</p>
          <Button asChild variant="outline" className="mt-3 border-emerald-300 bg-white text-emerald-800">
            <a href="/portal">{t("pay.open_license")}</a>
          </Button>
        </section>
      )}

      {(data.status === "refunded" || data.status === "partially_refunded") && (
        <p className="rounded-lg border bg-muted p-4 text-sm">
          <span className="font-medium">{t("pay.refunded")}</span>: {t("pay.refunded_hint")}
        </p>
      )}

      {data.status === "expired" && <p className="rounded-lg border bg-muted p-4 text-sm">{t("pay.expired_hint")}</p>}

      {data.status === "canceled" && <p className="rounded-lg border bg-muted p-4 text-sm">{t("pay.canceled_hint")}</p>}

      <p className="text-center text-xs text-muted-foreground">{t("pay.contact")}</p>
    </>
  )
}

function StatusBadge({ status }: { status: PayStatus["status"] }) {
  const { t } = useI18n()
  const label =
    status === "paid" || status === "partially_refunded" || status === "refunded"
      ? t("pay.paid")
      : status === "expired"
        ? t("pay.expired")
        : status === "canceled"
          ? t("pay.canceled")
          : t("pay.pending")
  const tone =
    status === "paid"
      ? "border-emerald-200 bg-emerald-50 text-emerald-800"
      : status === "pending"
        ? "border-amber-200 bg-amber-50 text-amber-800"
        : "border-border bg-muted text-muted-foreground"
  return (
    <span className={`shrink-0 rounded-full border px-2.5 py-0.5 text-xs font-medium ${tone}`}>
      {status === "refunded" || status === "partially_refunded" ? t("pay.refunded") : label}
    </span>
  )
}

function Countdown({ expiresAt }: { expiresAt: string }) {
  const { t } = useI18n()
  const target = new Date(expiresAt).getTime()
  const [now, setNow] = useState(() => Date.now())

  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [])

  const left = target - now
  if (!Number.isFinite(target) || left <= 0) {
    return <span>{t("pay.countdown_expired")}</span>
  }
  const hours = Math.floor(left / 3_600_000)
  const minutes = Math.floor((left % 3_600_000) / 60_000)
  const seconds = Math.floor((left % 60_000) / 1000)
  const clock =
    hours > 0
      ? `${hours}:${String(minutes).padStart(2, "0")}:${String(seconds).padStart(2, "0")}`
      : `${minutes}:${String(seconds).padStart(2, "0")}`
  return (
    <span>
      {t("pay.expires")} {clock}
    </span>
  )
}
