import { Copy } from "lucide-react"
import { useState } from "react"
import { useI18n } from "@/i18n"

// CopyButton copies a value the buyer has to reproduce elsewhere (a bank
// account, a transfer memo, an order code). Clipboard access can be
// denied: the failure is shown in place instead of silently doing
// nothing.
export function CopyButton({ value, label }: { value: string; label?: string }) {
  const { t } = useI18n()
  const [copied, setCopied] = useState(false)
  const [failed, setFailed] = useState(false)

  const copy = async () => {
    setFailed(false)
    try {
      if (!navigator.clipboard) throw new Error("clipboard unavailable")
      await navigator.clipboard.writeText(value)
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    } catch {
      setFailed(true)
      setTimeout(() => setFailed(false), 3000)
    }
  }

  return (
    <button
      type="button"
      className="inline-flex items-center gap-1 text-xs text-primary hover:underline"
      title={failed ? t("pay.copy_failed") : t("pay.copy")}
      onClick={copy}
    >
      <Copy className="h-3 w-3" />
      {copied ? t("pay.copied") : failed ? t("pay.copy_failed") : (label ?? t("pay.copy"))}
    </button>
  )
}
