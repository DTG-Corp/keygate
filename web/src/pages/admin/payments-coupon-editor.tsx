import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { ArrowLeft, Loader2, Save, Sparkles } from "lucide-react"
import { useEffect, useMemo, useState } from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { CouponCodesInput, QuoteTotals } from "@/components/coupon-codes"
import { showToast } from "@/components/toast"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { useI18n } from "@/i18n"
import {
  admin,
  adminCoupons,
  type Coupon,
  type CouponInput,
  type CouponPreviewInput,
  CouponPreviewUnavailableError,
  type CouponScope,
} from "@/lib/api"
import { currencyExponent, formatAmountInput, formatMoney, parseAmountToMinor } from "@/lib/money"
import { useDebounced } from "@/lib/use-debounced"
import { formatDateVN } from "@/lib/utils"

type CouponType = "percent" | "fixed" | "trial"
type StackGroup = "general" | "campaign" | "private"

interface FormState {
  code: string
  description: string
  type: CouponType
  percent: string
  fixed: string
  trial_days: string
  currency: string
  stack_group: StackGroup
  stackable: boolean
  priority: string
  assigned_email: string
  max_redemptions: string
  per_customer_limit: string
  valid_from: string
  valid_to: string
  active: boolean
}

const EMPTY: FormState = {
  code: "",
  description: "",
  type: "percent",
  percent: "10",
  fixed: "",
  trial_days: "14",
  currency: "VND",
  stack_group: "general",
  stackable: true,
  priority: "100",
  assigned_email: "",
  max_redemptions: "0",
  per_customer_limit: "0",
  valid_from: "",
  valid_to: "",
  active: true,
}

export default function PaymentCouponEditorPage() {
  const { t } = useI18n()
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()
  const qc = useQueryClient()
  const editing = !!id

  const detail = useQuery({
    queryKey: ["admin", "coupons", "detail", id],
    queryFn: () => adminCoupons.get(id ?? ""),
    enabled: editing,
  })
  const coupon = detail.data

  const [form, setForm] = useState<FormState>(EMPTY)
  const [scopes, setScopes] = useState<CouponScope[]>([])
  const [formError, setFormError] = useState("")
  const [extraCodes, setExtraCodes] = useState<string[]>([])
  const [previewPlan, setPreviewPlan] = useState("")
  const [previewEmail, setPreviewEmail] = useState("")

  // Load the saved definition once per coupon id. A refetch after a save
  // must not overwrite what the operator is currently typing.
  useEffect(() => {
    if (!coupon) return
    setForm({
      code: coupon.code,
      description: coupon.description,
      type: (coupon.type as CouponType) || "percent",
      percent: String(coupon.value / 100),
      fixed: coupon.currency ? formatAmountInput(coupon.value, currencyExponent(coupon.currency)) : "",
      trial_days: String(coupon.trial_days || 14),
      currency: coupon.currency || "VND",
      stack_group: (coupon.stack_group as StackGroup) || "general",
      stackable: coupon.stackable,
      priority: String(coupon.priority ?? 100),
      assigned_email: coupon.assigned_email,
      max_redemptions: String(coupon.max_redemptions ?? 0),
      per_customer_limit: String(coupon.per_customer_limit ?? 0),
      valid_from: toLocalInput(coupon.valid_from),
      valid_to: toLocalInput(coupon.valid_to),
      active: coupon.active,
    })
    setScopes(coupon.scopes ?? [])
  }, [coupon])

  const { data: products } = useQuery({
    queryKey: ["admin", "products", "coupon-scope-picker"],
    queryFn: () => admin.listProducts({ limit: 200 }),
  })
  const { data: plans } = useQuery({
    queryKey: ["admin", "plans", "coupon-scope-picker"],
    queryFn: () => admin.listPlans({ limit: 200 }),
  })
  const [productSearch, setProductSearch] = useState("")
  const [planSearch, setPlanSearch] = useState("")
  const productRows = (products?.products ?? []).filter((p) =>
    p.name.toLowerCase().includes(productSearch.toLowerCase()),
  )
  const productNames = new Map((products?.products ?? []).map((p) => [p.id, p.name]))
  const planRows = (plans?.plans ?? []).filter((p) => {
    const text = `${p.name} ${productNames.get(p.product_id) ?? ""}`.toLowerCase()
    return text.includes(planSearch.toLowerCase())
  })

  const exponent = currencyExponent(form.currency)
  const fixedMinor = parseAmountToMinor(form.fixed, exponent) ?? 0
  const valueMinor =
    form.type === "percent" ? Math.round(Number(form.percent) * 100) : form.type === "fixed" ? fixedMinor : 0

  const draft: CouponInput = useMemo(
    () => ({
      code: form.code.trim().toUpperCase(),
      description: form.description.trim(),
      type: form.type,
      value: Number.isFinite(valueMinor) ? valueMinor : 0,
      trial_days: form.type === "trial" ? Number(form.trial_days) || 0 : 0,
      currency: form.type === "fixed" ? form.currency : "",
      stack_group: form.stack_group,
      stackable: form.stackable,
      priority: Number(form.priority) || 0,
      assigned_email: form.assigned_email.trim(),
      max_redemptions: Number(form.max_redemptions) || 0,
      per_customer_limit: Number(form.per_customer_limit) || 0,
      valid_from: fromLocalInput(form.valid_from),
      valid_to: fromLocalInput(form.valid_to),
      active: form.active,
      scopes,
    }),
    [form, valueMinor, scopes],
  )

  const validationError = validate(draft, t)
  const maxRedemptions = Number(form.max_redemptions) || 0

  const save = useMutation({
    meta: { ownErrors: true },
    mutationFn: (input: CouponInput) => (editing && id ? adminCoupons.update(id, input) : adminCoupons.create(input)),
    onSuccess: (saved: Coupon) => {
      qc.invalidateQueries({ queryKey: ["admin", "coupons"] })
      qc.invalidateQueries({ queryKey: ["admin", "coupons", "preview"] })
      showToast(t("coupons.saved"), "success")
      if (!editing) navigate(`/admin/payments/coupons/${saved.id}`, { replace: true })
      else qc.setQueryData(["admin", "coupons", "detail", id], saved)
    },
    onError: (err: Error) => setFormError(err.message),
  })

  const submit = () => {
    setFormError("")
    if (validationError) {
      setFormError(validationError)
      return
    }
    save.mutate(draft)
  }

  // Live quote. The preview prices stored coupons only, so it is offered
  // for a saved coupon and never for the unsaved draft. The plan's own
  // price is the subtotal: the handler ignores a caller-supplied one, and
  // sending one would only pretend the input changed the quote.
  // Price the stored code, not the code currently in the form: an
  // unsaved rename must not turn into a not-found preview.
  const previewSignature = useDebounced(JSON.stringify({ code: coupon?.code ?? "", codes: extraCodes }), 500)
  const previewEnabled = editing && !!id && !!coupon?.code && !!previewPlan && !validationError
  const preview = useQuery({
    queryKey: ["admin", "coupons", "preview", id ?? "new", previewPlan, previewSignature, previewEmail],
    queryFn: () => {
      const parsed = JSON.parse(previewSignature) as { code: string; codes: string[] }
      const input: CouponPreviewInput = {
        plan_id: previewPlan,
        quantity: 1,
        codes: [...new Set([parsed.code, ...parsed.codes])].filter(Boolean),
        email: previewEmail || undefined,
      }
      return adminCoupons.preview(input)
    },
    enabled: previewEnabled,
    retry: false,
    staleTime: 15_000,
  })

  const previewMissing = preview.error instanceof CouponPreviewUnavailableError
  const quote = preview.data?.quote
  const quoteCurrency = preview.data?.price.currency || quote?.currency || form.currency || "VND"
  const quoteExponent = currencyExponent(quoteCurrency)

  const set = <K extends keyof FormState>(key: K, value: FormState[K]) =>
    setForm((current) => ({ ...current, [key]: value }))

  const toggleScope = (scopeType: "product" | "plan", scopeID: string) => {
    setScopes((current) => {
      const exists = current.some((s) => s.scope_type === scopeType && s.scope_id === scopeID)
      return exists
        ? current.filter((s) => !(s.scope_type === scopeType && s.scope_id === scopeID))
        : [...current, { scope_type: scopeType, scope_id: scopeID }]
    })
  }
  const hasScope = (scopeType: string, scopeID: string) =>
    scopes.some((s) => s.scope_type === scopeType && s.scope_id === scopeID)

  if (editing && detail.isLoading) {
    return <div className="h-48 animate-pulse rounded-lg bg-muted" />
  }
  if (editing && detail.error) {
    return (
      <div className="space-y-3">
        <p className="text-sm text-destructive">{(detail.error as Error).message}</p>
        <Button variant="outline" asChild>
          <Link to="/admin/payments/coupons">
            <ArrowLeft className="h-4 w-4" /> {t("coupons.back")}
          </Link>
        </Button>
      </div>
    )
  }

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="space-y-1">
          <Button variant="ghost" size="sm" className="-ml-2" asChild>
            <Link to="/admin/payments/coupons">
              <ArrowLeft className="h-4 w-4" /> {t("coupons.back")}
            </Link>
          </Button>
          <h1 className="text-2xl font-bold tracking-tight">
            {editing ? t("coupons.edit_title") : t("coupons.create_title")}
          </h1>
          {coupon && <p className="font-mono text-xs text-muted-foreground">{coupon.code}</p>}
        </div>
        <div className="flex items-center gap-2">
          <Badge className={form.active ? "bg-emerald-100 text-emerald-800" : "bg-gray-100 text-gray-800"}>
            {form.active ? t("common.active") : t("common.inactive")}
          </Badge>
          <Button onClick={submit} disabled={save.isPending}>
            {save.isPending ? <Loader2 className="h-4 w-4 animate-spin" /> : <Save className="h-4 w-4" />}
            {editing ? t("common.save") : t("coupons.create")}
          </Button>
        </div>
      </div>

      {(formError || save.error) && (
        <p className="rounded-md border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive">
          {formError || (save.error as Error)?.message}
        </p>
      )}

      <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr),22rem]">
        <div className="space-y-4">
          <Card>
            <CardHeader>
              <CardTitle>{t("coupons.code")}</CardTitle>
            </CardHeader>
            <CardContent className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label htmlFor="coupon-code">{t("coupons.code")}</Label>
                <div className="flex gap-2">
                  <Input
                    id="coupon-code"
                    value={form.code}
                    disabled={editing}
                    className="font-mono uppercase"
                    onChange={(e) => set("code", e.target.value.toUpperCase())}
                  />
                  {!editing && (
                    <Button
                      type="button"
                      variant="outline"
                      title={t("coupons.generate_hint")}
                      onClick={() => set("code", generateCode())}
                    >
                      <Sparkles className="h-4 w-4" /> {t("coupons.generate")}
                    </Button>
                  )}
                </div>
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="coupon-description">{t("coupons.description")}</Label>
                <Input
                  id="coupon-description"
                  value={form.description}
                  onChange={(e) => set("description", e.target.value)}
                />
              </div>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>{t("coupons.type")}</CardTitle>
            </CardHeader>
            <CardContent className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label>{t("coupons.type")}</Label>
                <Select
                  value={form.type}
                  onValueChange={(value) => {
                    set("type", value as CouponType)
                    setFormError("")
                  }}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="percent">{t("coupons.type_percent")}</SelectItem>
                    <SelectItem value="fixed">{t("coupons.type_fixed")}</SelectItem>
                    <SelectItem value="trial">{t("coupons.type_trial")}</SelectItem>
                  </SelectContent>
                </Select>
              </div>

              {form.type === "percent" && (
                <div className="space-y-1.5">
                  <Label htmlFor="coupon-percent">{t("coupons.value")} (%)</Label>
                  <Input
                    id="coupon-percent"
                    inputMode="decimal"
                    value={form.percent}
                    onChange={(e) => set("percent", e.target.value)}
                  />
                  <p className="text-xs text-muted-foreground">{t("coupons.percent_hint")}</p>
                </div>
              )}

              {form.type === "fixed" && (
                <>
                  <div className="space-y-1.5">
                    <Label htmlFor="coupon-fixed">{t("coupons.value")}</Label>
                    <Input
                      id="coupon-fixed"
                      inputMode="decimal"
                      value={form.fixed}
                      placeholder={formatAmountInput(0, exponent)}
                      onChange={(e) => set("fixed", e.target.value)}
                    />
                    <p className="text-xs text-muted-foreground">{t("coupons.fixed_hint")}</p>
                  </div>
                  <div className="space-y-1.5">
                    <Label>{t("coupons.currency")}</Label>
                    <Select
                      value={form.currency}
                      onValueChange={(value) => {
                        // Re-format the amount for the new exponent so a
                        // currency switch cannot silently rescale it.
                        const minor = parseAmountToMinor(form.fixed, exponent)
                        set("currency", value)
                        if (minor !== null) set("fixed", formatAmountInput(minor, currencyExponent(value)))
                      }}
                    >
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="VND">VND</SelectItem>
                        <SelectItem value="USD">USD</SelectItem>
                      </SelectContent>
                    </Select>
                    <p className="text-xs text-muted-foreground">{t("coupons.currency_required")}</p>
                  </div>
                </>
              )}

              {form.type === "trial" && (
                <div className="space-y-1.5">
                  <Label htmlFor="coupon-trial">{t("coupons.trial_days")}</Label>
                  <Input
                    id="coupon-trial"
                    inputMode="numeric"
                    value={form.trial_days}
                    onChange={(e) => set("trial_days", e.target.value)}
                  />
                  <p className="text-xs text-muted-foreground">{t("coupons.trial_hint")}</p>
                </div>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>{t("coupons.stack_group")}</CardTitle>
            </CardHeader>
            <CardContent className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label>{t("coupons.stack_group")}</Label>
                <Select value={form.stack_group} onValueChange={(value) => set("stack_group", value as StackGroup)}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="general">{t("coupons.stack_group_general")}</SelectItem>
                    <SelectItem value="campaign">{t("coupons.stack_group_campaign")}</SelectItem>
                    <SelectItem value="private">{t("coupons.stack_group_private")}</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="coupon-priority">{t("coupons.priority")}</Label>
                <Input
                  id="coupon-priority"
                  inputMode="numeric"
                  value={form.priority}
                  onChange={(e) => set("priority", e.target.value)}
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="coupon-assigned">{t("coupons.assigned_email")}</Label>
                <Input
                  id="coupon-assigned"
                  type="email"
                  value={form.assigned_email}
                  onChange={(e) => set("assigned_email", e.target.value)}
                />
              </div>
              <div className="flex flex-col gap-3 pt-6">
                <label className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    className="h-4 w-4 rounded border-input"
                    checked={form.stackable}
                    onChange={(e) => set("stackable", e.target.checked)}
                  />
                  {t("coupons.stackable")}
                </label>
                <label className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    className="h-4 w-4 rounded border-input"
                    checked={form.active}
                    onChange={(e) => set("active", e.target.checked)}
                  />
                  {t("coupons.active")}
                </label>
              </div>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>{t("coupons.usage")}</CardTitle>
            </CardHeader>
            <CardContent className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label htmlFor="coupon-max">{t("coupons.max_redemptions")}</Label>
                <Input
                  id="coupon-max"
                  inputMode="numeric"
                  value={form.max_redemptions}
                  onChange={(e) => set("max_redemptions", e.target.value)}
                />
                <p className="text-xs text-muted-foreground">{t("coupons.limit_hint")}</p>
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="coupon-per-customer">{t("coupons.per_customer_limit")}</Label>
                <Input
                  id="coupon-per-customer"
                  inputMode="numeric"
                  value={form.per_customer_limit}
                  onChange={(e) => set("per_customer_limit", e.target.value)}
                />
                <p className="text-xs text-muted-foreground">{t("coupons.limit_hint")}</p>
              </div>
              {coupon && (
                <div className="sm:col-span-2 flex flex-wrap gap-4 text-sm">
                  <span>
                    {t("coupons.stats_count")}: <strong className="tabular-nums">{coupon.redeemed_count}</strong>
                    {maxRedemptions > 0 ? ` / ${maxRedemptions}` : ""}
                  </span>
                  {coupon.stats && (
                    <span>
                      {t("coupons.stats_amount")}:{" "}
                      <strong className="tabular-nums">
                        {formatMoney(
                          coupon.stats.amount_minor,
                          coupon.currency || form.currency || "VND",
                          currencyExponent(coupon.currency || form.currency || "VND"),
                        )}
                      </strong>
                    </span>
                  )}
                  <Link
                    to={`/admin/payments/redemptions?coupon=${encodeURIComponent(coupon.code)}`}
                    className="text-primary hover:underline"
                  >
                    {t("coupons.view_redemptions")}
                  </Link>
                </div>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>{t("coupons.validity")}</CardTitle>
            </CardHeader>
            <CardContent className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label htmlFor="coupon-from">{t("coupons.valid_from")}</Label>
                <Input
                  id="coupon-from"
                  type="datetime-local"
                  value={form.valid_from}
                  onChange={(e) => set("valid_from", e.target.value)}
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="coupon-to">{t("coupons.valid_to")}</Label>
                <Input
                  id="coupon-to"
                  type="datetime-local"
                  value={form.valid_to}
                  onChange={(e) => set("valid_to", e.target.value)}
                />
              </div>
              {form.valid_from && (
                <p className="text-xs text-muted-foreground sm:col-span-2">
                  {formatDateVN(fromLocalInput(form.valid_from) ?? undefined)} →{" "}
                  {form.valid_to
                    ? formatDateVN(fromLocalInput(form.valid_to) ?? undefined)
                    : t("coupons.validity_always")}
                </p>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader className="flex-row items-start justify-between space-y-0">
              <div>
                <CardTitle>{t("coupons.scope")}</CardTitle>
                <p className="mt-1 text-sm text-muted-foreground">{t("coupons.scope_hint")}</p>
              </div>
              {scopes.length === 0 ? (
                <Badge variant="secondary">{t("coupons.scope_all")}</Badge>
              ) : (
                <Badge variant="secondary">{t("coupons.scope_count", { count: scopes.length })}</Badge>
              )}
            </CardHeader>
            <CardContent className="grid gap-4 md:grid-cols-2">
              <div className="space-y-2">
                <Label>{t("coupons.select_products")}</Label>
                <Input
                  value={productSearch}
                  placeholder={t("coupons.search_products")}
                  onChange={(e) => setProductSearch(e.target.value)}
                />
                <div className="max-h-64 space-y-1 overflow-y-auto rounded-md border p-2">
                  {productRows.length === 0 && (
                    <p className="p-2 text-sm text-muted-foreground">{t("coupons.no_products")}</p>
                  )}
                  {productRows.map((product) => (
                    <label
                      key={product.id}
                      className="flex items-center gap-2 rounded px-1 py-1 text-sm hover:bg-muted"
                    >
                      <input
                        type="checkbox"
                        className="h-4 w-4 rounded border-input"
                        checked={hasScope("product", product.id)}
                        onChange={() => toggleScope("product", product.id)}
                      />
                      <span className="truncate">{product.name}</span>
                    </label>
                  ))}
                </div>
              </div>
              <div className="space-y-2">
                <Label>{t("coupons.select_plans")}</Label>
                <Input
                  value={planSearch}
                  placeholder={t("coupons.search_plans")}
                  onChange={(e) => setPlanSearch(e.target.value)}
                />
                <div className="max-h-64 space-y-1 overflow-y-auto rounded-md border p-2">
                  {planRows.length === 0 && (
                    <p className="p-2 text-sm text-muted-foreground">{t("coupons.no_plans")}</p>
                  )}
                  {planRows.map((plan) => (
                    <label key={plan.id} className="flex items-center gap-2 rounded px-1 py-1 text-sm hover:bg-muted">
                      <input
                        type="checkbox"
                        className="h-4 w-4 rounded border-input"
                        checked={hasScope("plan", plan.id)}
                        onChange={() => toggleScope("plan", plan.id)}
                      />
                      <span className="truncate">
                        {plan.name}
                        <span className="text-muted-foreground"> · {productNames.get(plan.product_id) ?? ""}</span>
                      </span>
                    </label>
                  ))}
                </div>
              </div>
              {scopes.length === 0 && (
                <p className="text-xs text-muted-foreground md:col-span-2">{t("coupons.no_scopes")}</p>
              )}
            </CardContent>
          </Card>
        </div>

        <div className="space-y-4">
          <Card className="lg:sticky lg:top-4">
            <CardHeader>
              <CardTitle>{t("coupons.preview")}</CardTitle>
              <p className="text-sm text-muted-foreground">{t("coupons.preview_hint")}</p>
            </CardHeader>
            <CardContent className="space-y-3">
              {!editing ? (
                <p className="rounded-md border bg-muted/40 p-3 text-xs text-muted-foreground">
                  {t("coupons.preview_unsaved")}
                </p>
              ) : (
                <>
                  <div className="space-y-1.5">
                    <Label>{t("coupons.preview_plan")}</Label>
                    <Select value={previewPlan} onValueChange={setPreviewPlan}>
                      <SelectTrigger>
                        <SelectValue placeholder={t("coupons.preview_plan")} />
                      </SelectTrigger>
                      <SelectContent>
                        {(plans?.plans ?? []).slice(0, 50).map((plan) => (
                          <SelectItem key={plan.id} value={plan.id}>
                            {plan.name}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="preview-email">{t("coupons.preview_email")}</Label>
                    <Input id="preview-email" value={previewEmail} onChange={(e) => setPreviewEmail(e.target.value)} />
                  </div>
                  <div className="space-y-1.5">
                    <Label>{t("coupons.preview_codes")}</Label>
                    <CouponCodesInput codes={extraCodes} onChange={setExtraCodes} />
                  </div>

                  {!previewPlan && <p className="text-xs text-muted-foreground">{t("coupons.preview_hint")}</p>}
                  {previewPlan && preview.isLoading && (
                    <div className="flex items-center gap-2 text-sm text-muted-foreground">
                      <Loader2 className="h-4 w-4 animate-spin" /> {t("common.loading")}
                    </div>
                  )}
                  {previewPlan && quote && !preview.error && (
                    <QuoteTotals quote={quote} currency={quoteCurrency} exponent={quoteExponent} />
                  )}
                  {previewPlan && preview.error && !previewMissing && (
                    <p className="rounded-md border border-amber-200 bg-amber-50 p-2 text-xs text-amber-800">
                      {t("coupons.preview_invalid", { message: (preview.error as Error).message })}
                    </p>
                  )}
                  {previewMissing && (
                    <p className="rounded-md border border-amber-200 bg-amber-50 p-2 text-xs text-amber-800">
                      {t("coupons.preview_unavailable")}
                    </p>
                  )}
                  {draft.type === "trial" && (
                    <p className="text-xs text-muted-foreground">
                      {t("coupons.trial_days")}: {draft.trial_days}
                    </p>
                  )}
                </>
              )}
            </CardContent>
          </Card>
        </div>
      </div>
    </div>
  )
}

function validate(input: CouponInput, t: ReturnType<typeof useI18n>["t"]): string {
  if (!input.code) return t("coupons.code_required")
  // The store accepts letters (including non-Latin), digits, dot, dash
  // and underscore, 1–64 characters. Checking here turns the server's
  // PAYMENT_COUPON_INVALID into an inline message before the round trip.
  if (input.code.length > 64 || !/^[\p{L}\p{N}._-]+$/u.test(input.code)) return t("coupons.code_invalid")
  if (input.type === "percent" && (input.value < 1 || input.value > 10000)) return t("coupons.percent_range")
  if (input.type === "fixed" && input.value <= 0) return t("coupons.value_required")
  if (input.type === "fixed" && !input.currency) return t("coupons.currency_required")
  if (input.type === "trial" && (input.trial_days < 1 || input.trial_days > 3650)) {
    return t("coupons.trial_required")
  }
  if (input.valid_from && input.valid_to && new Date(input.valid_to) <= new Date(input.valid_from)) {
    return t("coupons.valid_range")
  }
  return ""
}

function generateCode(): string {
  const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
  const bytes = new Uint8Array(8)
  crypto.getRandomValues(bytes)
  return Array.from(bytes, (byte) => alphabet[byte % alphabet.length]).join("")
}

// datetime-local inputs work in the browser's local zone; the API stores
// UTC. Both helpers convert through Date so the operator's wall clock is
// what they see and the instant is what the server stores.
function toLocalInput(iso?: string): string {
  if (!iso) return ""
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return ""
  const pad = (value: number) => String(value).padStart(2, "0")
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
}

function fromLocalInput(value: string): string | null {
  if (!value) return null
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? null : date.toISOString()
}
