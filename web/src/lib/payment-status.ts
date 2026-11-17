import type { useI18n } from "@/i18n"

// Payment statuses are shared by the portal list, detail and any admin
// surface that shows a customer-facing state. The label reuses the pay
// page's wording so one order never has two names for the same status.
export function paymentStatusLabel(status: string, t: ReturnType<typeof useI18n>["t"]): string {
  switch (status) {
    case "paid":
      return t("pay.paid")
    case "expired":
      return t("pay.expired")
    case "canceled":
      return t("pay.canceled")
    case "refunded":
    case "partially_refunded":
      return t("pay.refunded")
    default:
      return t("pay.pending")
  }
}

export function paymentStatusClass(status: string): string {
  switch (status) {
    case "paid":
      return "bg-emerald-100 text-emerald-800"
    case "pending":
      return "bg-blue-100 text-blue-800"
    case "expired":
      return "bg-red-100 text-red-700"
    case "canceled":
      return "bg-gray-100 text-gray-800"
    case "refunded":
    case "partially_refunded":
      return "bg-amber-100 text-amber-800"
    default:
      return "bg-gray-100 text-gray-800"
  }
}
