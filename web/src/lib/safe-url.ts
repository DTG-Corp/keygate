// Redirects and QR images are values we did not build: the provider
// returned them. Allowlists keep a compromised or misconfigured provider
// payload from turning a public pay page into an open redirect or an
// image beacon to an arbitrary origin.
//
// Native checkout host only: the PayOS adapter forwards the checkoutUrl
// from PayOS's API (https://pay.payos.vn/web/...); SePay returns no
// checkout URL at all. Anything else is hidden.
const CHECKOUT_HOSTS = ["pay.payos.vn"]
// SePay builds its VietQR image on this host
// (internal/payment/providers/sepay/qr.go). PayOS returns an EMVCo
// payload, not an image URL, so it never reaches this list.
const IMAGE_HOSTS = ["vietqr.app"]

// allowedHTTPSURL accepts only an absolute https URL whose hostname is
// exactly one of `hosts`. A relative value, a protocol-relative value and
// anything carrying credentials or a lookalike host are rejected.
export function allowedHTTPSURL(value: string, hosts: readonly string[]): string | undefined {
  let url: URL
  try {
    url = new URL(value)
  } catch {
    return undefined
  }
  if (url.protocol !== "https:") return undefined
  const host = url.hostname.toLowerCase()
  return hosts.includes(host) ? url.toString() : undefined
}

// sameOriginPath accepts a single-slash absolute path on this site.
// "//host/path" is a protocol-relative URL, not a local path: it must be
// rejected before the URL parser resolves it, or the other host wins.
export function sameOriginPath(value: string): string | undefined {
  if (!value.startsWith("/") || value.startsWith("//")) return undefined
  try {
    const url = new URL(value, window.location.origin)
    if (url.origin !== window.location.origin) return undefined
    return `${url.pathname}${url.search}${url.hash}`
  } catch {
    return undefined
  }
}

export function safeCheckoutURL(value?: string): string | undefined {
  if (!value) return undefined
  return allowedHTTPSURL(value, CHECKOUT_HOSTS)
}

// The API's qr_image_url is renderable for SePay. PayOS stores its raw
// EMVCo payload in the same field, which is an <img> broken-image icon,
// not a QR; only a same-origin absolute path or an https URL on an
// allowed image host is put in src, and the page falls back to the
// checkout button otherwise.
export function safeImageURL(value?: string): string | undefined {
  if (!value) return undefined
  return sameOriginPath(value) ?? allowedHTTPSURL(value, IMAGE_HOSTS)
}
