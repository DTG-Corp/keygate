import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Loader2, Save } from "lucide-react"
import { useEffect, useState } from "react"
import { showToast } from "@/components/toast"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { useI18n } from "@/i18n"
import { type BillingProfileInput, errorCode, portalBilling } from "@/lib/api"

const EMPTY: BillingProfileInput = {
  name: "",
  company_name: "",
  tax_code: "",
  address: "",
  email: "",
  phone: "",
  request_invoice: false,
}

export default function PortalBillingProfilePage() {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [form, setForm] = useState<BillingProfileInput>(EMPTY)
  const [error, setError] = useState("")

  const profile = useQuery({
    queryKey: ["portal", "billing-profile"],
    queryFn: portalBilling.get,
  })

  useEffect(() => {
    if (!profile.data) return
    setForm({
      name: profile.data.name,
      company_name: profile.data.company_name,
      tax_code: profile.data.tax_code,
      address: profile.data.address,
      email: profile.data.email,
      phone: profile.data.phone,
      request_invoice: profile.data.request_invoice ?? false,
    })
  }, [profile.data])

  const save = useMutation({
    meta: { ownErrors: true },
    mutationFn: (input: BillingProfileInput) => portalBilling.update(input),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["portal", "billing-profile"] })
      setError("")
      showToast(t("portal.billingSaved"), "success")
    },
    onError: (err: Error) => {
      // The server owns the validation rule; surface its code rather than
      // replacing it with a generic failure.
      if (errorCode(err) === "PAYMENT_TAX_CODE_REQUIRED") setError(t("portal.billingTaxRequired"))
      else setError(err.message)
    },
  })

  const set = <K extends keyof BillingProfileInput>(key: K, value: BillingProfileInput[K]) =>
    setForm((current) => ({ ...current, [key]: value }))

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    setError("")
    if (!form.name.trim()) {
      setError(t("portal.billingNameRequired"))
      return
    }
    if (form.email && !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(form.email)) {
      setError(t("portal.billingEmailInvalid"))
      return
    }
    if (form.request_invoice && !form.tax_code.trim()) {
      setError(t("portal.billingTaxRequired"))
      return
    }
    save.mutate({
      ...form,
      name: form.name.trim(),
      company_name: form.company_name.trim(),
      tax_code: form.tax_code.trim(),
      address: form.address.trim(),
      email: form.email.trim(),
      phone: form.phone.trim(),
    })
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-bold tracking-tight">{t("portal.billingProfile")}</h1>
        <p className="text-muted-foreground">{t("portal.billingProfileDesc")}</p>
      </div>

      {profile.isLoading && <div className="h-64 animate-pulse rounded-lg bg-muted" />}
      {profile.error && <p className="text-sm text-destructive">{t("portal.billingLoadFailed")}</p>}

      {!profile.isLoading && !profile.error && (
        <form onSubmit={submit}>
          <Card>
            <CardHeader>
              <CardTitle className="text-base">{t("portal.billingProfile")}</CardTitle>
              <p className="text-sm text-muted-foreground">{t("portal.billingHint")}</p>
            </CardHeader>
            <CardContent className="space-y-4">
              <div className="grid gap-4 sm:grid-cols-2">
                <div className="space-y-1.5">
                  <Label htmlFor="billing-name">{t("portal.billingName")} *</Label>
                  <Input id="billing-name" value={form.name} onChange={(e) => set("name", e.target.value)} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="billing-company">{t("portal.billingCompany")}</Label>
                  <Input
                    id="billing-company"
                    value={form.company_name}
                    onChange={(e) => set("company_name", e.target.value)}
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="billing-tax">
                    {t("portal.billingTaxCode")}
                    {form.request_invoice ? " *" : ""}
                  </Label>
                  <Input
                    id="billing-tax"
                    className="font-mono"
                    value={form.tax_code}
                    onChange={(e) => set("tax_code", e.target.value)}
                  />
                  {form.request_invoice && !form.tax_code.trim() && (
                    <p className="text-xs text-amber-700">{t("portal.billingTaxRequired")}</p>
                  )}
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="billing-email">{t("portal.billingEmail")}</Label>
                  <Input
                    id="billing-email"
                    type="email"
                    value={form.email}
                    onChange={(e) => set("email", e.target.value)}
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="billing-phone">{t("portal.billingPhone")}</Label>
                  <Input id="billing-phone" value={form.phone} onChange={(e) => set("phone", e.target.value)} />
                </div>
                <div className="space-y-1.5 sm:col-span-2">
                  <Label htmlFor="billing-address">{t("portal.billingAddress")}</Label>
                  <Input id="billing-address" value={form.address} onChange={(e) => set("address", e.target.value)} />
                </div>
              </div>

              <label className="flex items-start gap-2 rounded-md border p-3 text-sm">
                <input
                  type="checkbox"
                  className="mt-0.5 h-4 w-4 rounded border-input"
                  checked={form.request_invoice}
                  onChange={(e) => set("request_invoice", e.target.checked)}
                />
                <span>
                  <span className="font-medium">{t("portal.billingRequestInvoice")}</span>
                  <span className="block text-xs text-muted-foreground">{t("portal.billingTaxRequired")}</span>
                </span>
              </label>

              {error && (
                <p className="rounded-md border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive">
                  {error}
                </p>
              )}

              <div className="flex justify-end">
                <Button type="submit" disabled={save.isPending}>
                  {save.isPending ? <Loader2 className="h-4 w-4 animate-spin" /> : <Save className="h-4 w-4" />}
                  {t("common.save")}
                </Button>
              </div>
            </CardContent>
          </Card>
        </form>
      )}
    </div>
  )
}
