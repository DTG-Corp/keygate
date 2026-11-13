// Money arrives from the API as integer minor units plus the currency's
// exponent. Every amount is stored, transmitted and compared as that
// integer; this module is the only place that turns one into text.
//
// Never divide by 100: VND has exponent 0, so a hardcoded two-digit
// division would turn 1190000 ₫ into 11900 ₫. The exponent is data, not
// an assumption.

const CURRENCY_EXPONENTS: Record<string, number> = {
  VND: 0,
  JPY: 0,
  KRW: 0,
  KHR: 0,
  LAK: 0,
  MMK: 0,
  IDR: 0,
  CLP: 0,
  ISK: 0,
  HUF: 0,
  TWD: 0,
  USD: 2,
  EUR: 2,
  GBP: 2,
  AUD: 2,
  CAD: 2,
  SGD: 2,
  THB: 2,
  MYR: 2,
  PHP: 2,
  CNY: 2,
  INR: 2,
  BHD: 3,
  JOD: 3,
  KWD: 3,
  OMR: 3,
  TND: 3,
}

// currencyExponent is the number of minor-unit digits a currency has.
// It is only a fallback for payloads that do not carry the server's
// value (payment transactions, for one).
export function currencyExponent(currency: string): number {
  return CURRENCY_EXPONENTS[(currency || "").toUpperCase()] ?? 2
}

function normaliseExponent(exponent: number, currency: string): number {
  if (Number.isInteger(exponent) && exponent >= 0) return exponent
  return currencyExponent(currency)
}

// minorToDecimal splits an integer by its exponent without arithmetic on
// fractions, so the digits Intl sees are exactly the digits the server
// stored. The final decimal string is what gets parsed; no amount of
// 0.1-style binary drift can change a digit before Intl rounds it.
function minorToDecimal(minor: number, exponent: number): string {
  const negative = minor < 0
  const digits = String(Math.abs(minor)).padStart(exponent + 1, "0")
  const whole = exponent > 0 ? digits.slice(0, -exponent) : digits
  const fraction = exponent > 0 ? digits.slice(-exponent) : ""
  return `${negative ? "-" : ""}${whole}${fraction ? `.${fraction}` : ""}`
}

// formatMoney renders integer minor units as localised currency.
export function formatMoney(minor: number, currency: string, exponent: number): string {
  const code = (currency || "VND").toUpperCase()
  const exp = normaliseExponent(exponent, code)
  const decimal = minorToDecimal(minor, exp)
  try {
    return new Intl.NumberFormat("vi-VN", {
      style: "currency",
      currency: code,
      maximumFractionDigits: exp,
      minimumFractionDigits: exp,
    }).format(Number(decimal))
  } catch {
    // A currency code Intl does not know still has to render.
    const grouped = Number(decimal).toLocaleString("vi-VN", {
      maximumFractionDigits: exp,
      minimumFractionDigits: exp,
    })
    return `${grouped} ${code}`
  }
}

export function formatVND(minor: number): string {
  return formatMoney(minor, "VND", 0)
}

// formatAmountInput is the editable form of an amount: grouping
// separators, no currency symbol, comma as the Vietnamese decimal mark.
export function formatAmountInput(minor: number, exponent: number): string {
  const exp = normaliseExponent(exponent, "VND")
  const [whole, fraction] = minorToDecimal(minor, exp).split(".")
  const grouped = new Intl.NumberFormat("vi-VN", { useGrouping: true, maximumFractionDigits: 0 }).format(Number(whole))
  return fraction ? `${grouped},${fraction}` : grouped
}

// parseAmountToMinor reads what a person typed and returns integer minor
// units, or null when the text is not an amount.
//
// Both grouping conventions are accepted: 1.190.000 (Vietnamese) and
// 1,190,000 (English) mean the same VND amount. For a currency with
// minor digits the last separator is the decimal mark when it is
// followed by at most `exponent` digits; otherwise every separator is
// grouping. A lone separator followed by exactly `exponent` digits is
// decimal (1,23 in a 2-digit currency); three digits after a lone
// separator can only be grouping (1.234 = 1234).
export function parseAmountToMinor(raw: string, exponent: number): number | null {
  const exp = Math.max(0, Math.trunc(normaliseExponent(exponent, "VND")))
  let text = raw.trim().replace(/\s/g, "")
  if (!text) return null

  let negative = false
  if (text.startsWith("-")) {
    negative = true
    text = text.slice(1)
  }
  text = text.replace(/[^\d.,]/g, "")
  if (!text || !/\d/.test(text)) return null

  let whole = text
  let fraction = ""

  if (exp > 0) {
    const lastDot = text.lastIndexOf(".")
    const lastComma = text.lastIndexOf(",")
    const lastSep = Math.max(lastDot, lastComma)
    if (lastSep >= 0) {
      const head = text.slice(0, lastSep)
      const tail = text.slice(lastSep + 1)
      const dots = (text.match(/\./g) || []).length
      const commas = (text.match(/,/g) || []).length
      const bothKinds = dots > 0 && commas > 0
      const repeated = dots + commas > 1
      const decimal = tail.length > 0 && tail.length <= exp && (bothKinds || !repeated || tail.length === exp)
      if (decimal) {
        whole = head
        fraction = tail.padEnd(exp, "0").slice(0, exp)
      }
    }
  }

  const wholeDigits = whole.replace(/[.,]/g, "")
  if (wholeDigits === "" && fraction === "") return null

  const wholeValue = wholeDigits === "" ? 0 : Number(wholeDigits)
  const fractionValue = fraction === "" ? 0 : Number(fraction)
  if (!Number.isSafeInteger(wholeValue) || !Number.isSafeInteger(fractionValue)) return null

  const minor = wholeValue * 10 ** exp + fractionValue
  if (!Number.isSafeInteger(minor)) return null
  return negative ? -minor : minor
}
