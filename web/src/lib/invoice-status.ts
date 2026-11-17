import type { useI18n } from "@/i18n"

export function invoiceStatusClass(status: string): string {
  switch (status) {
    case "issued":
      return "bg-emerald-100 text-emerald-800"
    case "pending_issue":
      return "bg-amber-100 text-amber-800"
    case "void":
      return "bg-red-100 text-red-700"
    default:
      return "bg-gray-100 text-gray-800"
  }
}

export function invoiceStatusLabel(status: string, t: ReturnType<typeof useI18n>["t"]): string {
  switch (status) {
    case "draft":
      return t("invoices.status_draft")
    case "issued":
      return t("invoices.status_issued")
    case "pending_issue":
      return t("invoices.status_pending_issue")
    case "void":
      return t("invoices.status_void")
    default:
      return status || "—"
  }
}
