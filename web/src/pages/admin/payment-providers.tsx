import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { CheckCircle2, CreditCard, Pencil, Plus, RefreshCw, ShieldAlert } from "lucide-react"
import { useState } from "react"
import { ProductSelect } from "@/components/product-select"
import { showToast } from "@/components/toast"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  DataTable,
  DataTableBody,
  DataTableCell,
  DataTableEmpty,
  DataTableHead,
  DataTableHeader,
  DataTableRow,
} from "@/components/ui/data-table"
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { type TranslationKeys, useI18n } from "@/i18n"
import { admin, adminPayments, type PaymentHealthRow, type ProviderRow } from "@/lib/api"
import { formatDate } from "@/lib/utils"

type ProviderName = "sepay" | "payos"
type Mode = "test" | "live"

interface ProviderForm {
  id?: string
  provider: ProviderName
  product_id: string
  mode: Mode
  enabled: boolean
  product_name: string
  credentials_set: boolean
  secret_set: boolean
  // SePay config
  bank_name: string
  bank_bin: string
  bank_code: string
  account_no: string
  account_name: string
  webhook_auth_mode: string
  // PayOS config
  client_id: string
  // Credentials: write-only. Blank means "keep what is stored".
  api_token: string
  ipn_secret: string
  api_key: string
  checksum_key: string
  webhook_secret: string
}

function emptyForm(): ProviderForm {
  return {
    provider: "sepay",
    product_id: "",
    mode: "test",
    enabled: false,
    product_name: "",
    credentials_set: false,
    secret_set: false,
    bank_name: "",
    bank_bin: "",
    bank_code: "",
    account_no: "",
    account_name: "",
    webhook_auth_mode: "hmac",
    client_id: "",
    api_token: "",
    ipn_secret: "",
    api_key: "",
    checksum_key: "",
    webhook_secret: "",
  }
}

function text(value: unknown): string {
  return typeof value === "string" ? value : ""
}

function formFromRow(row: ProviderRow): ProviderForm {
  const config = row.config ?? {}
  return {
    id: row.id,
    provider: row.provider === "payos" ? "payos" : "sepay",
    product_id: row.product_id ?? "",
    mode: row.mode === "live" ? "live" : "test",
    enabled: row.enabled,
    product_name: "",
    credentials_set: row.credentials_set,
    secret_set: row.secret_set,
    bank_name: text(config.bank_name),
    bank_bin: text(config.bank_bin),
    bank_code: text(config.bank_code),
    account_no: text(config.account_no),
    account_name: text(config.account_name),
    webhook_auth_mode: text(config.webhook_auth_mode) || "hmac",
    client_id: text(config.client_id),
    api_token: "",
    ipn_secret: "",
    api_key: "",
    checksum_key: "",
    webhook_secret: "",
  }
}

function providerLabel(name: string): string {
  if (name === "sepay") return "SePay"
  if (name === "payos") return "PayOS"
  return name
}

export default function PaymentProvidersPage() {
  const { t } = useI18n()
  const qc = useQueryClient()
  const [editor, setEditor] = useState<ProviderForm | null>(null)
  const [formError, setFormError] = useState("")
  const { data, isLoading, error } = useQuery({
    queryKey: ["admin", "payments", "providers"],
    queryFn: adminPayments.providers,
  })
  const { data: products } = useQuery({
    queryKey: ["admin", "products", "payment-provider-labels"],
    queryFn: () => admin.listProducts({ limit: 200 }),
  })
  // Health is optional: a deployment without the reconciliation worker
  // still has a working provider editor.
  const { data: health } = useQuery({
    queryKey: ["admin", "payments", "health"],
    queryFn: adminPayments.health,
    retry: false,
  })

  const providers = data?.providers ?? []
  const productNames = new Map((products?.products ?? []).map((p) => [p.id, p.name]))
  const healthRows = health?.providers ?? health?.health ?? []
  const hasPayOS = providers.some((p) => p.provider === "payos")

  const saveMut = useMutation({
    mutationFn: (form: ProviderForm) => adminPayments.upsertProvider(buildBody(form)),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["admin", "payments", "providers"] })
      qc.invalidateQueries({ queryKey: ["admin", "payments", "health"] })
      setEditor(null)
      setFormError("")
      showToast(t("admin.provider_saved"), "success")
    },
    meta: { ownErrors: true },
    onError: (e: Error) => setFormError(e.message),
  })
  const testMut = useMutation({
    mutationFn: (row: ProviderRow) => adminPayments.testProvider(row.provider, row.product_id),
    onSuccess: (result) => {
      if (result.ok) {
        showToast(t("admin.test_ok"), "success")
      } else {
        showToast(result.error ? `${t("admin.test_failed")}: ${result.error}` : t("admin.test_failed"), "error")
      }
    },
    onError: (e: Error) => showToast(e.message, "error"),
  })
  const confirmMut = useMutation({
    mutationFn: adminPayments.confirmPayOSWebhook,
    onSuccess: () => showToast(t("admin.confirm_webhook_done"), "success"),
    onError: (e: Error) => showToast(e.message, "error"),
  })

  const openNew = () => {
    setFormError("")
    setEditor(emptyForm())
  }
  const openEdit = (row: ProviderRow) => {
    setFormError("")
    setEditor({ ...formFromRow(row), product_name: productNames.get(row.product_id ?? "") ?? "" })
  }

  const save = () => {
    if (!editor) return
    const problem = validate(editor, t)
    if (problem) {
      setFormError(problem)
      return
    }
    setFormError("")
    saveMut.mutate(editor)
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="min-w-0">
          <h1 className="text-2xl font-bold tracking-tight sr-only md:not-sr-only">{t("admin.providers")}</h1>
          <p className="text-muted-foreground">{t("admin.providers_subtitle")}</p>
        </div>
        <div className="flex flex-wrap gap-2">
          {hasPayOS && (
            <Button variant="outline" onClick={() => confirmMut.mutate()} disabled={confirmMut.isPending}>
              <CheckCircle2 className="mr-2 h-4 w-4" /> {t("admin.confirm_webhook")}
            </Button>
          )}
          <Button onClick={openNew}>
            <Plus className="mr-2 h-4 w-4" /> {t("admin.add_provider")}
          </Button>
        </div>
      </div>

      <div className="flex items-start gap-2 rounded-lg border border-amber-200 bg-amber-50 p-4 text-sm text-amber-800">
        <ShieldAlert className="mt-0.5 h-4 w-4 shrink-0" />
        <p>{t("admin.providers_warning")}</p>
      </div>

      {error && (
        <p className="rounded-md border border-red-200 bg-red-50 p-3 text-sm text-red-700">
          {(error as Error).message}
        </p>
      )}

      <Card>
        <CardContent className="pt-6">
          {isLoading ? (
            <div className="h-32 animate-pulse rounded-lg bg-muted" />
          ) : (
            <DataTable>
              <DataTableHeader>
                <DataTableRow>
                  <DataTableHead>{t("admin.provider")}</DataTableHead>
                  <DataTableHead>{t("admin.mode")}</DataTableHead>
                  <DataTableHead>{t("common.product")}</DataTableHead>
                  <DataTableHead>{t("common.status")}</DataTableHead>
                  <DataTableHead>{t("admin.secrets")}</DataTableHead>
                  <DataTableHead className="w-32 text-right">{t("common.actions")}</DataTableHead>
                </DataTableRow>
              </DataTableHeader>
              <DataTableBody>
                {providers.length === 0 && <DataTableEmpty colSpan={6} message={t("admin.no_providers")} />}
                {providers.map((row) => (
                  <DataTableRow key={row.id}>
                    <DataTableCell className="font-medium">
                      <span className="inline-flex items-center gap-2">
                        <CreditCard className="h-4 w-4 text-muted-foreground" />
                        {providerLabel(row.provider)}
                      </span>
                    </DataTableCell>
                    <DataTableCell>
                      <Badge variant="outline">{t(row.mode === "live" ? "admin.mode_live" : "admin.mode_test")}</Badge>
                    </DataTableCell>
                    <DataTableCell className="text-sm text-muted-foreground">
                      {row.product_id
                        ? (productNames.get(row.product_id) ?? row.product_id)
                        : t("admin.install_default")}
                    </DataTableCell>
                    <DataTableCell>
                      <Badge variant={row.enabled ? "default" : "secondary"}>
                        {row.enabled ? t("admin.enabled") : t("common.inactive")}
                      </Badge>
                    </DataTableCell>
                    <DataTableCell>
                      <div className="flex flex-wrap gap-1">
                        <Badge variant="outline">{row.credentials_set ? t("admin.credentials_set") : "—"}</Badge>
                        <Badge variant="outline">{row.secret_set ? t("admin.secret_set") : "—"}</Badge>
                      </div>
                    </DataTableCell>
                    <DataTableCell>
                      <div className="flex justify-end gap-1">
                        <Button
                          variant="ghost"
                          size="icon"
                          title={t("admin.test_connection")}
                          disabled={!row.enabled || testMut.isPending}
                          onClick={() => testMut.mutate(row)}
                        >
                          <RefreshCw className={`h-4 w-4 ${testMut.isPending ? "animate-spin" : ""}`} />
                        </Button>
                        <Button variant="ghost" size="icon" title={t("common.edit")} onClick={() => openEdit(row)}>
                          <Pencil className="h-4 w-4" />
                        </Button>
                      </div>
                    </DataTableCell>
                  </DataTableRow>
                ))}
              </DataTableBody>
            </DataTable>
          )}
        </CardContent>
      </Card>

      {healthRows.length > 0 && <HealthCard rows={healthRows} />}

      <Dialog open={!!editor} onOpenChange={(open) => (open ? undefined : setEditor(null))}>
        <DialogContent className="h-[min(760px,90vh)]">
          <DialogHeader>
            <DialogTitle>{editor?.id ? t("admin.edit_provider") : t("admin.add_provider")}</DialogTitle>
            <DialogDescription>{t("admin.credentials_hint")}</DialogDescription>
          </DialogHeader>
          {editor && (
            <>
              <DialogBody className="space-y-5">
                {formError && (
                  <p className="rounded-md border border-red-200 bg-red-50 p-3 text-sm text-red-700">{formError}</p>
                )}

                <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
                  <div className="space-y-2">
                    <Label>{t("admin.provider")}</Label>
                    <Select
                      value={editor.provider}
                      disabled={!!editor.id}
                      onValueChange={(v) => setEditor({ ...editor, provider: v as ProviderName, enabled: false })}
                    >
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="sepay">SePay</SelectItem>
                        <SelectItem value="payos">PayOS</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="space-y-2">
                    <Label>{t("admin.mode")}</Label>
                    <Select
                      value={editor.mode}
                      disabled={!!editor.id}
                      onValueChange={(v) => setEditor({ ...editor, mode: v as Mode })}
                    >
                      <SelectTrigger>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="test">{t("admin.mode_test")}</SelectItem>
                        <SelectItem value="live">{t("admin.mode_live")}</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="space-y-2">
                    <Label>{t("common.product")}</Label>
                    <ProductSelect
                      value={editor.product_id}
                      onChange={(product_id) =>
                        setEditor({
                          ...editor,
                          product_id,
                          product_name: productNames.get(product_id) ?? "",
                          enabled: false,
                        })
                      }
                      noneLabel={t("admin.install_default")}
                      current={
                        editor.product_id
                          ? { id: editor.product_id, name: editor.product_name || editor.product_id }
                          : null
                      }
                      disabled={!!editor.id}
                      className="w-full"
                      placeholder={t("admin.install_default")}
                    />
                  </div>
                </div>

                <div className="flex items-center gap-3">
                  <input
                    id="payment-provider-enabled"
                    type="checkbox"
                    checked={editor.enabled}
                    onChange={(e) => setEditor({ ...editor, enabled: e.target.checked })}
                    className="h-4 w-4 rounded border-input accent-primary"
                  />
                  <Label htmlFor="payment-provider-enabled">{t("admin.enabled")}</Label>
                </div>

                <div className="space-y-4 rounded-lg border p-4">
                  <p className="text-sm font-medium">{t("admin.config")}</p>
                  {editor.provider === "sepay" ? (
                    <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                      <TextField
                        label={t("admin.bank_name")}
                        value={editor.bank_name}
                        onChange={(bank_name) => setEditor({ ...editor, bank_name })}
                      />
                      <TextField
                        label={t("admin.bank_bin")}
                        value={editor.bank_bin}
                        onChange={(bank_bin) => setEditor({ ...editor, bank_bin })}
                      />
                      <TextField
                        label={t("admin.bank_code")}
                        value={editor.bank_code}
                        onChange={(bank_code) => setEditor({ ...editor, bank_code })}
                      />
                      <TextField
                        label={t("admin.account_no")}
                        value={editor.account_no}
                        onChange={(account_no) => setEditor({ ...editor, account_no })}
                      />
                      <TextField
                        label={t("admin.account_name")}
                        value={editor.account_name}
                        onChange={(account_name) => setEditor({ ...editor, account_name })}
                      />
                      <div className="space-y-2">
                        <Label>{t("admin.webhook_auth_mode")}</Label>
                        <Select
                          value={editor.webhook_auth_mode}
                          onValueChange={(webhook_auth_mode) => setEditor({ ...editor, webhook_auth_mode })}
                        >
                          <SelectTrigger>
                            <SelectValue />
                          </SelectTrigger>
                          <SelectContent>
                            <SelectItem value="hmac">{t("admin.auth_hmac")}</SelectItem>
                            <SelectItem value="apikey">{t("admin.auth_apikey")}</SelectItem>
                            <SelectItem value="none">{t("admin.auth_none")}</SelectItem>
                          </SelectContent>
                        </Select>
                      </div>
                    </div>
                  ) : (
                    <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                      <TextField
                        label={t("admin.client_id")}
                        value={editor.client_id}
                        onChange={(client_id) => setEditor({ ...editor, client_id })}
                      />
                    </div>
                  )}
                </div>

                <div className="space-y-4 rounded-lg border p-4">
                  <div>
                    <p className="text-sm font-medium">{t("admin.credentials")}</p>
                    <p className="text-xs text-muted-foreground">{t("admin.leave_blank_keep")}</p>
                  </div>
                  {editor.provider === "sepay" ? (
                    <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                      <SecretField
                        label={t("admin.api_token")}
                        value={editor.api_token}
                        onChange={(api_token) => setEditor({ ...editor, api_token })}
                      />
                      <SecretField
                        label={t("admin.ipn_secret")}
                        value={editor.ipn_secret}
                        onChange={(ipn_secret) => setEditor({ ...editor, ipn_secret })}
                      />
                    </div>
                  ) : (
                    <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                      <SecretField
                        label={t("admin.api_key")}
                        value={editor.api_key}
                        onChange={(api_key) => setEditor({ ...editor, api_key })}
                      />
                      <SecretField
                        label={t("admin.checksum_key")}
                        value={editor.checksum_key}
                        onChange={(checksum_key) => setEditor({ ...editor, checksum_key })}
                      />
                    </div>
                  )}
                  <SecretField
                    label={t("admin.webhook_secret")}
                    hint={t("admin.webhook_secret_hint")}
                    value={editor.webhook_secret}
                    onChange={(webhook_secret) => setEditor({ ...editor, webhook_secret })}
                  />
                </div>
              </DialogBody>
              <DialogFooter>
                <Button variant="outline" onClick={() => setEditor(null)} disabled={saveMut.isPending}>
                  {t("common.cancel")}
                </Button>
                <Button onClick={save} disabled={saveMut.isPending}>
                  {saveMut.isPending ? "..." : t("admin.save_provider")}
                </Button>
              </DialogFooter>
            </>
          )}
        </DialogContent>
      </Dialog>
    </div>
  )
}

function HealthCard({ rows }: { rows: PaymentHealthRow[] }) {
  const { t } = useI18n()
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">{t("admin.health")}</CardTitle>
      </CardHeader>
      <CardContent>
        <DataTable>
          <DataTableHeader>
            <DataTableRow>
              <DataTableHead>{t("admin.provider")}</DataTableHead>
              <DataTableHead>{t("admin.last_event")}</DataTableHead>
              <DataTableHead>{t("admin.pending_orders")}</DataTableHead>
              <DataTableHead>{t("admin.unmatched_open")}</DataTableHead>
              <DataTableHead>{t("admin.last_error")}</DataTableHead>
            </DataTableRow>
          </DataTableHeader>
          <DataTableBody>
            {rows.map((row) => (
              <DataTableRow key={row.provider}>
                <DataTableCell className="font-medium">{providerLabel(row.provider)}</DataTableCell>
                <DataTableCell className="text-xs text-muted-foreground">
                  {row.last_event_at ? formatDate(row.last_event_at) : "—"}
                </DataTableCell>
                <DataTableCell>{row.pending_orders ?? 0}</DataTableCell>
                <DataTableCell>{row.unmatched_open ?? 0}</DataTableCell>
                <DataTableCell className="max-w-[24rem] truncate text-xs text-red-700" title={row.last_error}>
                  {row.last_error || "—"}
                </DataTableCell>
              </DataTableRow>
            ))}
          </DataTableBody>
        </DataTable>
      </CardContent>
    </Card>
  )
}

function TextField({ label, value, onChange }: { label: string; value: string; onChange: (value: string) => void }) {
  return (
    <div className="space-y-2">
      <Label>{label}</Label>
      <Input value={value} onChange={(e) => onChange(e.target.value)} autoComplete="off" />
    </div>
  )
}

function SecretField({
  label,
  hint,
  value,
  onChange,
}: {
  label: string
  hint?: string
  value: string
  onChange: (value: string) => void
}) {
  return (
    <div className="space-y-2">
      <Label>{label}</Label>
      <Input
        type="password"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        autoComplete="new-password"
        spellCheck={false}
      />
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  )
}

function validate(form: ProviderForm, t: (key: TranslationKeys) => string): string {
  if (form.provider === "sepay") {
    const entered = form.api_token.trim() !== "" || form.ipn_secret.trim() !== ""
    if (entered && form.api_token.trim() === "") return t("admin.credentials_required")
    if (form.enabled && !form.credentials_set && !entered) return t("admin.credentials_required")
  } else {
    const entered = form.api_key.trim() !== "" || form.checksum_key.trim() !== ""
    if (entered && (form.api_key.trim() === "" || form.checksum_key.trim() === "")) {
      return t("admin.credentials_required")
    }
    if (form.enabled && !form.credentials_set && !entered) return t("admin.credentials_required")
  }
  if (form.enabled && !form.secret_set && form.webhook_secret.trim() === "") {
    return t("admin.webhook_secret_required")
  }
  return ""
}

// buildBody sends secrets only when the form contains new ones: a blank
// field means "keep the stored value", and the API never returns a
// secret to compare against. The server rejects an incomplete set
// before writing, so a half-filled credential form cannot erase one.
function buildBody(form: ProviderForm) {
  const config =
    form.provider === "sepay"
      ? {
          bank_name: form.bank_name.trim(),
          bank_bin: form.bank_bin.trim(),
          bank_code: form.bank_code.trim(),
          account_no: form.account_no.trim(),
          account_name: form.account_name.trim(),
          webhook_auth_mode: form.webhook_auth_mode,
        }
      : { client_id: form.client_id.trim() }

  const body: Record<string, unknown> = {
    provider: form.provider,
    product_id: form.product_id,
    mode: form.mode,
    enabled: form.enabled,
    config,
  }

  if (form.provider === "sepay") {
    if (form.api_token.trim() || form.ipn_secret.trim()) {
      body.credentials = { api_token: form.api_token.trim(), ipn_secret: form.ipn_secret.trim() }
    }
  } else if (form.api_key.trim() || form.checksum_key.trim()) {
    body.credentials = { api_key: form.api_key.trim(), checksum_key: form.checksum_key.trim() }
  }

  if (form.webhook_secret.trim()) body.webhook_secret = form.webhook_secret.trim()
  return body
}
