const ROOT = import.meta.env.VITE_API_URL || ""
const BASE = `${ROOT}/api/v1`

// ServiceUnavailableError means the server could not be reached or could
// not answer right now: a network failure, a 429 or a 5xx — including a
// session renewal that failed for one of those reasons. The user may
// well still be signed in, so it must never be read as a sign-out; the
// auth layer retries instead (see AuthProvider).
export class ServiceUnavailableError extends Error {}

// errorCode reads the server's error code from a failed request, if any.
export function errorCode(err: unknown): string | undefined {
  return (err as { code?: string } | null)?.code
}

// errorDetails reads the details the server attached to an error, if any.
export function errorDetails(err: unknown): Record<string, unknown> | undefined {
  return (err as { details?: Record<string, unknown> } | null)?.details
}

type RefreshOutcome = "ok" | "denied" | "unavailable"

// The session cookie lives 24 hours; the refresh cookie 30 days. When a
// call comes back 401, renew the session once and retry it. Only one
// refresh runs per page at a time: the server rotates the refresh token
// on every use, so parallel calls would present the same token twice.
// (The server also tolerates a just-rotated token for a few seconds,
// which covers several tabs renewing at the same moment.)
let refreshing: Promise<RefreshOutcome> | null = null

function refreshSession(): Promise<RefreshOutcome> {
  refreshing ??= fetch(`${BASE}/auth/refresh`, { method: "POST", credentials: "include" })
    // Only 401/403 mean the refresh cookie is no good. A 429 (shared
    // rate limit) or a 5xx is a hiccup: the user is still signed in.
    .then((r): RefreshOutcome => (r.ok ? "ok" : r.status === 401 || r.status === 403 ? "denied" : "unavailable"))
    .catch((): RefreshOutcome => "unavailable")
    .finally(() => {
      refreshing = null
    })
  return refreshing
}

async function request<T>(path: string, opts?: RequestInit, retried = false): Promise<T> {
  let res: Response
  try {
    res = await fetch(BASE + path, {
      credentials: "include",
      headers: { "Content-Type": "application/json", ...opts?.headers },
      ...opts,
    })
  } catch (e) {
    // Network-level failure (server down, DNS, CORS). fetch() throws
    // a TypeError here; we surface a friendly message instead of
    // "Failed to fetch" which is meaningless to end users.
    const reason = e instanceof Error ? e.message : String(e)
    throw new ServiceUnavailableError(`Network error: ${reason}. Is the server reachable?`)
  }

  // Session expired: renew it and retry the call once. Auth endpoints
  // answer 401 for their own reasons (a wrong code, no refresh cookie),
  // so they are not retried — that would mask the error or loop. Logout
  // is the exception: it needs a live session to revoke the refresh
  // token, and if it fails the login page would silently renew the
  // session and sign the user straight back in.
  // The sign-up form answers 401 for a wrong code, not for a session: it
  // keeps its own error and is never renewed or sent to the login page.
  const signup = path.startsWith("/signup/")
  const renewable = (!path.startsWith("/auth/") && !signup) || path === "/auth/logout"
  if (res.status === 401 && !retried && renewable) {
    const outcome = await refreshSession()
    if (outcome === "ok") return request<T>(path, opts, true)
    if (outcome === "unavailable") {
      // Not a sign-out: the session may well still be renewable.
      throw new ServiceUnavailableError("Could not renew your session right now. Check your connection and try again.")
    }
  }

  // Could not renew: redirect to login
  if (res.status === 401 && !signup) {
    // Don't redirect if already on login page or fetching auth state
    if (!window.location.pathname.startsWith("/login") && path !== "/portal/me") {
      window.location.href = "/login"
    }
    throw new Error("Session expired")
  }

  if (res.status === 204) return undefined as T

  // Read the body as text first so an empty or non-JSON response (502
  // from a proxy, server crash mid-response, HTML error page, etc.)
  // doesn't produce the cryptic "Unexpected end of JSON input" — that
  // error confuses users who clicked a normal button.
  const raw = await res.text()
  let json: any = null
  if (raw) {
    try {
      json = JSON.parse(raw)
    } catch {
      // body wasn't JSON — keep `json` null, fall through to text path
    }
  }

  if (!res.ok) {
    const msg =
      json?.error?.message ||
      (typeof json?.error === "string" ? json.error : null) ||
      (raw && raw.length < 200 ? raw : null) ||
      `Request failed (${res.status}${res.statusText ? ` ${res.statusText}` : ""})`
    // A 429 or 5xx says nothing about the session: the server is busy or
    // failing. Callers that decide "signed in or not" must tell it apart.
    // The server's error code rides along for callers that translate it.
    const err = res.status === 429 || res.status >= 500 ? new ServiceUnavailableError(msg) : new Error(msg)
    throw Object.assign(err, {
      code: typeof json?.error?.code === "string" ? json.error.code : undefined,
      details: json?.error?.details && typeof json.error.details === "object" ? json.error.details : undefined,
    })
  }
  return (json?.data !== undefined ? json.data : json) as T
}

function get<T>(path: string) {
  return request<T>(path)
}
function post<T>(path: string, body?: unknown) {
  return request<T>(path, { method: "POST", body: body ? JSON.stringify(body) : undefined })
}
function put<T>(path: string, body?: unknown) {
  return request<T>(path, { method: "PUT", body: body ? JSON.stringify(body) : undefined })
}
// eslint-disable-next-line @typescript-eslint/no-unused-vars
function patch<T>(path: string, body?: unknown) {
  return request<T>(path, { method: "PATCH", body: body ? JSON.stringify(body) : undefined })
}
function del<T>(path: string) {
  return request<T>(path, { method: "DELETE" })
}

// publicGet talks to the routes served outside /api/v1 — today only the
// pay page's status endpoint. The order code is the capability, so no
// cookie is sent and a 401/403 from an unrelated API can never bounce a
// payer to the login page. The envelope is read the same way as
// request()'s.
async function publicGet<T>(path: string): Promise<T> {
  let res: Response
  try {
    res = await fetch(ROOT + path, { credentials: "omit", headers: { Accept: "application/json" } })
  } catch (e) {
    const reason = e instanceof Error ? e.message : String(e)
    throw new ServiceUnavailableError(`Network error: ${reason}. Is the server reachable?`)
  }

  const raw = await res.text()
  let json: any = null
  if (raw) {
    try {
      json = JSON.parse(raw)
    } catch {
      // Non-JSON body (a proxy error page, say): the status is still
      // the useful part below.
    }
  }

  if (!res.ok) {
    const msg =
      json?.error?.message ||
      (typeof json?.error === "string" ? json.error : null) ||
      (raw && raw.length < 200 ? raw : null) ||
      `Request failed (${res.status}${res.statusText ? ` ${res.statusText}` : ""})`
    const err = res.status === 429 || res.status >= 500 ? new ServiceUnavailableError(msg) : new Error(msg)
    throw Object.assign(err, {
      code: typeof json?.error?.code === "string" ? json.error.code : undefined,
      details: json?.error?.details && typeof json.error.details === "object" ? json.error.details : undefined,
    })
  }
  return (json?.data !== undefined ? json.data : json) as T
}

// ─── Site Config (public, no auth) ───
export const site = {
  config: () => get<Record<string, string>>("/config"),
}

// ─── Auth ───
// First run setup. Public: the server refuses initialize once an owner, an
// admin or a product exists.
export const setup = {
  status: () => get<{ needed: boolean; step: string }>("/setup/status"),
  initialize: (body: {
    admin_name: string
    admin_email: string
    site_name: string
    product_name: string
    product_slug: string
    product_type: "desktop" | "saas" | "hybrid"
  }) => post<{ user: unknown; product: unknown; plan: unknown }>("/setup/initialize", body),
}

export const auth = {
  me: () =>
    get<{ id: string; email: string; name: string; avatar_url: string; is_admin: boolean; role: string }>("/portal/me"),
  providers: () => get<{ dev_login: boolean; otp: boolean }>("/auth/providers"),
  logout: () => post<void>("/auth/logout"),
  devLogin: (email: string, name: string) => post<{ status: string }>("/auth/dev-login", { email, name }),
  otpSend: (email: string) => post<{ status: string }>("/auth/otp/send", { email }),
  otpVerify: (email: string, code: string) =>
    post<{ status: string; email: string; name: string; is_admin: boolean; role: string }>("/auth/otp/verify", {
      email,
      code,
    }),
}

// ─── Self sign-up (public) ───
// A free plan's /pay link leads to /signup/:checkoutId when the plan
// takes self sign-ups. verify signs the visitor in and emails the key.
export interface SignupInfo {
  product_name: string
  plan_name: string
  license_type: string
  trial_days: number
  consent_text: string
}

export const signup = {
  info: (checkoutId: string) => get<SignupInfo>(`/signup/${encodeURIComponent(checkoutId)}`),
  send: (checkoutId: string, email: string) =>
    post<{ status: string }>(`/signup/${encodeURIComponent(checkoutId)}/send`, { email }),
  // consentText is the wording the form showed; the server records a
  // yes only against the wording it holds now.
  verify: (checkoutId: string, email: string, code: string, marketingConsent: boolean, consentText: string) =>
    post<{ status: "created" | "existing"; license_id: string; license_status?: string; license_usable?: boolean }>(
      `/signup/${encodeURIComponent(checkoutId)}/verify`,
      {
        email,
        code,
        marketing_consent: marketingConsent,
        consent_text: consentText,
      },
    ),
}

// ─── Checkout ───
export const checkout = {
  verify: (sessionId: string) =>
    get<{ status: string; email?: string; kind?: string }>(`/checkout/verify?session_id=${sessionId}`),
}

// ─── Checkout payments (public, order code only) ───
// The status route is mounted at the root, not under /api/v1: using the
// prefixing helpers here would 404.
export interface PayStatus {
  code: string
  status: "pending" | "paid" | "expired" | "canceled" | "refunded" | "partially_refunded"
  amount_minor: number
  currency: string
  currency_exponent: number
  expires_at: string
  qr_image_url?: string
  checkout_url?: string
  bank?: { bank_name: string; bank_bin?: string; account_no: string; account_name: string }
  description?: string
  // The public payload is flat and carries no buyer identity. The
  // breakdown below is optional: it is only rendered when the server
  // includes it.
  subtotal_minor?: number
  discount_minor?: number
  tax_minor?: number
  tax_rate_bp?: number
  // Coupons attached to the order, when the public payload includes the
  // breakdown. Read-only: the pay page never re-prices an order (spec
  // §3.13).
  discounts?: CouponDiscount[]
  // Some handlers return only the applied codes; the pay page shows them
  // without amounts rather than hiding that a coupon was used.
  coupon_codes?: string[]
}

export const checkoutPay = {
  // Public: the order code is the capability, exactly like the Stripe
  // success page's session id. No cookie is sent.
  status: (code: string) => publicGet<PayStatus>(`/pay/${encodeURIComponent(code)}/status`),
}

// ─── Invites (public, token-only) ───
// The plain invite token IS the email-ownership proof — no session
// required. Backend collapses bad/expired/already-accepted into one
// generic error so an attacker can't probe token validity; only a valid
// invitation with no seat free says so (SEAT_LIMIT).
export const invites = {
  accept: (token: string) =>
    post<{ user_id: string; email: string; license_id: string; product_name?: string; role: string }>(
      "/invites/accept",
      { token },
    ),
}

// ─── Portal ───
export const portal = {
  licenses: () => get<{ licenses: PortalLicense[]; renewals_enabled?: boolean }>("/portal/licenses"),
  listPlans: (productId: string) => get<{ plans: Plan[] }>(`/portal/plans?product_id=${productId}`),
  updateProfile: (data: { name: string }) =>
    put<{ id: string; email: string; name: string; avatar_url: string; role: string }>("/portal/profile", data),
  recordUsage: (data: { license_key: string; feature: string; quantity?: number }) =>
    post<{ status?: string }>("/portal/usage", data),
  quotaStatus: (data: { license_key: string; feature: string }) =>
    post<{ used?: number; limit?: number; remaining?: number }>("/portal/usage/status", data),
  // Customer-facing team management for multi-seat plans. Session-
  // authed (cookie); the body's license_key only names the target
  // license — the cookie is the actual authentication.
  // Self-service device/activation management. Backend authorises the
  // license owner (every device) OR an accepted member (their own) — a
  // teammate who lost a laptop can free their own slot without a support ticket
  // (internal/handler/portal_activations.go). license_key is in the
  // path; the session cookie is the actual auth.
  listActivations: (licenseKey: string) =>
    get<{ activations: Activation[]; max: number }>(`/portal/licenses/${encodeURIComponent(licenseKey)}/activations`),
  removeActivation: (licenseKey: string, activationId: string) =>
    del<{ status: string }>(
      `/portal/licenses/${encodeURIComponent(licenseKey)}/activations/${encodeURIComponent(activationId)}`,
    ),
  listSeats: (licenseKey: string) => post<{ seats: Seat[] }>("/portal/seats", { license_key: licenseKey }),
  addSeat: (data: { license_key: string; email: string }) => post<Seat>("/portal/seats/add", data),
  removeSeat: (data: { license_key: string; seat_id: string }) =>
    post<{ status: string }>("/portal/seats/remove", data),
  // A change to the subscription's plan or seats: previewed, then made
  // from the preview's signed token (its target, the terms it started
  // from and its proration date).
  previewChange: (data: { license_id: string; price_id?: string; quantity?: number }) =>
    post<SubscriptionChangePreview>("/portal/subscription/preview", data),
  changeSubscription: (data: { license_id: string; preview_token: string }) =>
    post<SubscriptionChangeResult>("/portal/subscription/change", data),
  pendingChange: (data: { license_id: string }) =>
    post<{
      status: "none" | "pending_payment"
      hosted_invoice_url?: string
      expires_at?: string
      quantity?: number
      plan_name?: string
    }>("/portal/subscription/pending", data),
  cancelSubscription: (data: { license_id: string; immediate?: boolean }) =>
    post<{ status: string; immediate: boolean }>("/portal/subscription/cancel", data),
  getBillingPortal: (data: { license_id: string }) =>
    post<{ url: string }>("/portal/subscription/billing-portal", data),
  getInvoices: (licenseId: string) =>
    get<{ invoices: Invoice[] }>(`/portal/subscription/invoices?license_id=${licenseId}`),
  renewUpdates: (data: { license_id: string }) => post<{ url: string }>("/portal/updates/renew", data),
  // Moves a licence Stripe doesn't bill (a free plan's) to a paid plan
  // through checkout; the licence keeps its key.
  upgrade: (data: { license_id: string; plan_id: string }) => post<{ url: string }>("/portal/licenses/upgrade", data),
}

export interface SubscriptionTerms {
  plan_id: string
  plan_name: string
  price_id: string
  per_seat: boolean
  quantity: number
  seat_limit: number
  activation_limit: number
}

export interface SubscriptionChangePreview {
  current: SubscriptionTerms
  target: SubscriptionTerms
  // The change itself in the smallest currency unit: charged now when
  // positive, credited to the customer's balance when negative.
  total: number
  credit_balance: number
  due_now: number
  currency: string
  proration_date: number
  seats_in_use: number
  devices_in_use: number
  replaces_pending: boolean
  // A downgrade (it lowers a limit) applies now and is not prorated:
  // nothing is charged or credited today, and the next invoice, due at
  // next_invoice_at (unix seconds), bills next_invoice_total.
  downgrade: boolean
  next_invoice_total?: number
  next_invoice_at?: number
  preview_token: string
}

export interface SubscriptionChangeResult {
  status: "applied" | "pending_payment" | "processing"
  plan_id: string
  plan_name: string
  quantity: number
  hosted_invoice_url?: string
  expires_at?: string
  downgrade?: boolean
  keep?: boolean
}

// ─── Admin ───
// Every list endpoint answers with its rows under their own name and
// the same three numbers beside them. `limit` is what the server
// applied, not what was asked for — it clamps.
// Builds a list request's query string. Empty and undefined values
// are left out so a filter that is not set does not become one that
// matches the empty string.
function listQuery(params?: Record<string, string | number | undefined>) {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(params || {})) {
    if (v !== undefined && v !== "" && v !== null) q.set(k, String(v))
  }
  return q.toString()
}

export type Paged<T> = T & { total: number; limit: number; offset: number }

export const admin = {
  stats: () => get<Stats>("/admin/stats"),

  listProducts: (params?: { search?: string; type?: string; limit?: number; offset?: number }) =>
    get<Paged<{ products: Product[] }>>(`/admin/products?${listQuery(params)}`),
  getProduct: (id: string) => get<Product>(`/admin/products/${id}`),
  createProduct: (data: {
    name: string
    slug: string
    type: string
    license_prefix?: string
    feed_license_required?: boolean
    download_url?: string
  }) => post<Product>("/admin/products", data),
  updateProduct: (id: string, data: Partial<Product>) => put<Product>(`/admin/products/${id}`, data),
  deleteProduct: (id: string) => del(`/admin/products/${id}`),

  listPlans: (params?: { product_id?: string; search?: string; limit?: number; offset?: number }) =>
    get<Paged<{ plans: Plan[] }>>(`/admin/plans?${listQuery(params)}`),
  getPlan: (id: string) => get<Plan>(`/admin/plans/${id}`),
  createPlan: (data: Partial<Plan>) => post<Plan>("/admin/plans", data),
  updatePlan: (id: string, data: Partial<Plan>) => put<Plan>(`/admin/plans/${id}`, data),
  // How many of the plan's licenses would hold more people than maxSeats.
  planSeatImpact: (id: string, maxSeats: number) =>
    get<{ max_seats: number; licenses_over: number }>(`/admin/plans/${id}/seat-impact?max_seats=${maxSeats}`),
  deletePlan: (id: string) => del(`/admin/plans/${id}`),

  createEntitlement: (data: { plan_id: string; feature: string; value_type: string; value: string }) =>
    post<Entitlement>("/admin/entitlements", data),
  updateEntitlement: (id: string, data: Partial<Entitlement>) => put<Entitlement>(`/admin/entitlements/${id}`, data),
  deleteEntitlement: (id: string) => del(`/admin/entitlements/${id}`),

  listLicenses: (params?: {
    product_id?: string
    status?: string
    search?: string
    external_customer_id?: string
    external_workspace_id?: string
    // Server-side ordering. The API refuses a column it does not
    // know rather than quietly serving a different order, so these
    // have to match its allowlist: created_at, valid_until, email,
    // status, product, plan.
    sort?: string
    order?: "asc" | "desc"
    offset?: number
    limit?: number
  }) => {
    const q = new URLSearchParams()
    if (params?.product_id) q.set("product_id", params.product_id)
    if (params?.status) q.set("status", params.status)
    if (params?.search) q.set("search", params.search)
    if (params?.external_customer_id) q.set("external_customer_id", params.external_customer_id)
    if (params?.external_workspace_id) q.set("external_workspace_id", params.external_workspace_id)
    if (params?.sort) q.set("sort", params.sort)
    if (params?.order) q.set("order", params.order)
    if (params?.offset) q.set("offset", String(params.offset))
    if (params?.limit) q.set("limit", String(params.limit))
    return get<{
      licenses: License[]
      total: number
      limit: number
      offset: number
      license_key_hints: Record<string, string>
      license_key_prefixes: Record<string, string>
    }>(`/admin/licenses?${q}`)
  },
  // The licence keeps its own shape; only the key is gone, replaced by
  // a last-four hint and the key's prefix.
  getLicense: (id: string) =>
    get<
      License & {
        license_key_hint: string
        license_key_prefix: string
        key_usable: boolean
        key_unusable_reason?: string
      }
    >(`/admin/licenses/${id}`),
  // The key is never in a list or detail payload — one explicit
  // request per key, audited server-side.
  revealLicenseKey: (id: string) => get<{ license_key: string }>(`/admin/licenses/${id}/key`),
  rotateLicenseKey: (id: string) =>
    post<{ license_key: string; devices_removed: number }>(`/admin/licenses/${id}/rotate-key`, {}),
  // Re-queues the "here is your key" mail. The address is the one on
  // the licence; the server does not accept one from here.
  resendLicenseEmail: (id: string) =>
    post<{ queued: boolean; email: string }>(`/admin/licenses/${id}/resend-email`, {}),
  createLicense: (data: {
    product_id: string
    plan_id: string
    email: string
    notes?: string
    external_customer_id?: string
    external_workspace_id?: string
    valid_until?: string
  }) => post<License>("/admin/licenses", data),
  // Empty valid_until clears the expiry (perpetual license).
  setLicenseValidUntil: (id: string, validUntil: string) =>
    post<License>(`/admin/licenses/${id}/valid-until`, { valid_until: validUntil }),
  // Empty updates_until means updates for life.
  setLicenseUpdatesUntil: (id: string, updatesUntil: string) =>
    post<License>(`/admin/licenses/${id}/updates-until`, { updates_until: updatesUntil }),
  revokeLicense: (id: string) => post(`/admin/licenses/${id}/revoke`),
  suspendLicense: (id: string) => post(`/admin/licenses/${id}/suspend`),
  reinstateLicense: (id: string) =>
    post<{ status: string; usable?: boolean; notice?: string }>(`/admin/licenses/${id}/reinstate`),
  refundLicense: (id: string) => post(`/admin/licenses/${id}/refund`),

  deleteActivation: (id: string) => del(`/admin/activations/${id}`),

  listAPIKeys: (params?: { product_id?: string; search?: string; limit?: number; offset?: number }) =>
    get<Paged<{ api_keys: APIKey[] }>>(`/admin/api-keys?${listQuery(params)}`),
  createAPIKey: (data: { product_id?: string; name: string; scopes?: string[] }) =>
    post<APIKey & { key: string }>("/admin/api-keys", data),
  rotateAPIKey: (id: string) => post<APIKey & { key: string }>(`/admin/api-keys/${id}/rotate`, {}),
  deleteAPIKey: (id: string) => del(`/admin/api-keys/${id}`),

  listWebhooks: (params?: { product_id?: string; search?: string; limit?: number; offset?: number }) =>
    get<Paged<{ webhooks: WebhookConfig[] }>>(`/admin/webhooks?${listQuery(params)}`),
  createWebhook: (data: { product_id: string; url: string; events: string[] }) =>
    post<WebhookConfig & { secret: string }>("/admin/webhooks", data),
  updateWebhook: (id: string, data: Partial<WebhookConfig>) => put<WebhookConfig>(`/admin/webhooks/${id}`, data),
  deleteWebhook: (id: string) => del(`/admin/webhooks/${id}`),
  listWebhookDeliveries: (
    id: string,
    params?: { offset?: number; limit?: number; status?: string; event?: string },
  ) => {
    const q = new URLSearchParams()
    if (params?.offset) q.set("offset", String(params.offset))
    if (params?.limit) q.set("limit", String(params.limit))
    if (params?.status) q.set("status", params.status)
    if (params?.event) q.set("event", params.event)
    return get<{ deliveries: WebhookDeliveryLog[]; total: number }>(`/admin/webhooks/${id}/deliveries?${q}`)
  },
  resendWebhookDelivery: (webhookId: string, deliveryId: string) =>
    post<WebhookDeliveryLog>(`/admin/webhooks/${webhookId}/deliveries/${deliveryId}/resend`),
  testWebhook: (id: string) =>
    post<{ status: string; delivery_id: string; response_code: number; response_body: string }>(
      `/admin/webhooks/${id}/test`,
    ),

  getLicenseUsage: (id: string, params?: { feature?: string; offset?: number; limit?: number }) => {
    const q = new URLSearchParams()
    if (params?.feature) q.set("feature", params.feature)
    if (params?.offset) q.set("offset", String(params.offset))
    if (params?.limit) q.set("limit", String(params.limit))
    return get<{ events: UsageEvent[]; counters: UsageCounter[]; total: number }>(`/admin/licenses/${id}/usage?${q}`)
  },
  resetUsageCounter: (id: string, data: { feature: string; period?: string; period_key?: string }) =>
    post(`/admin/licenses/${id}/usage/reset`, data),

  getLicenseSeats: (id: string, params?: { limit?: number; offset?: number }) =>
    get<Paged<{ seats: AdminSeat[]; active_count: number }>>(`/admin/licenses/${id}/seats?${listQuery(params)}`),
  // A member's own key: revealed one at a time and audited, reissued, or
  // the seat removed (an invitation cancelled), as the owner would.
  revealMemberKey: (licenseId: string, seatId: string) =>
    get<{ license_key: string }>(`/admin/licenses/${licenseId}/seats/${seatId}/key`),
  rotateMemberKey: (licenseId: string, seatId: string) =>
    post<{ license_key: string; devices_removed: number }>(
      `/admin/licenses/${licenseId}/seats/${seatId}/rotate-key`,
      {},
    ),
  removeLicenseSeat: (licenseId: string, seatId: string) =>
    del<{ status: string }>(`/admin/licenses/${licenseId}/seats/${seatId}`),

  getAnalytics: (params?: { product_id?: string; from?: string; to?: string; granularity?: string }) => {
    const q = new URLSearchParams()
    if (params?.product_id) q.set("product_id", params.product_id)
    if (params?.from) q.set("from", params.from)
    if (params?.to) q.set("to", params.to)
    if (params?.granularity) q.set("granularity", params.granularity)
    return get<{ snapshots: AnalyticsSnapshot[] | AggregatedSnapshot[] }>(`/admin/analytics?${q}`)
  },

  getAnalyticsSummary: (params?: {
    product_id?: string
    plan_id?: string
    license_type?: string
    status?: string
    from?: string
    to?: string
  }) => {
    const q = new URLSearchParams()
    if (params?.product_id) q.set("product_id", params.product_id)
    if (params?.plan_id) q.set("plan_id", params.plan_id)
    if (params?.license_type) q.set("license_type", params.license_type)
    if (params?.status) q.set("status", params.status)
    if (params?.from) q.set("from", params.from)
    if (params?.to) q.set("to", params.to)
    return get<AnalyticsSummary>(`/admin/analytics/summary?${q}`)
  },

  getAnalyticsBreakdown: (params: {
    product_id?: string
    plan_id?: string
    license_type?: string
    status?: string
    from?: string
    to?: string
    dimension: string
  }) => {
    const q = new URLSearchParams()
    if (params.product_id) q.set("product_id", params.product_id)
    if (params.plan_id) q.set("plan_id", params.plan_id)
    if (params.license_type) q.set("license_type", params.license_type)
    if (params.status) q.set("status", params.status)
    if (params.from) q.set("from", params.from)
    if (params.to) q.set("to", params.to)
    q.set("dimension", params.dimension)
    return get<{ items: BreakdownItem[] }>(`/admin/analytics/breakdown?${q}`)
  },

  getAnalyticsUsageTop: (params?: { product_id?: string; from?: string; to?: string; limit?: number }) => {
    const q = new URLSearchParams()
    if (params?.product_id) q.set("product_id", params.product_id)
    if (params?.from) q.set("from", params.from)
    if (params?.to) q.set("to", params.to)
    if (params?.limit) q.set("limit", String(params.limit))
    return get<{ features: FeatureUsageItem[] }>(`/admin/analytics/usage-top?${q}`)
  },

  getAnalyticsActivationTrend: (params?: { product_id?: string; from?: string; to?: string }) => {
    const q = new URLSearchParams()
    if (params?.product_id) q.set("product_id", params.product_id)
    if (params?.from) q.set("from", params.from)
    if (params?.to) q.set("to", params.to)
    return get<{ trend: TrendPoint[] }>(`/admin/analytics/activation-trend?${q}`)
  },

  // Cuts a licence loose from a Stripe subscription that is over, so
  // it can be managed locally again. Refused (409) while Stripe still
  // has the subscription.
  unlinkStripeSubscription: (id: string) => post<{ status: string }>(`/admin/licenses/${id}/stripe/unlink`, {}),
  changeLicensePlan: (id: string, data: { plan_id: string }) => post(`/admin/licenses/${id}/change-plan`, data),
  setLicenseQuantity: (id: string, quantity: number) => post<License>(`/admin/licenses/${id}/quantity`, { quantity }),

  // Addons
  listAddons: (params?: { product_id?: string; search?: string; limit?: number; offset?: number }) =>
    get<Paged<{ addons: Addon[] }>>(`/admin/addons?${listQuery(params)}`),
  createAddon: (data: Partial<Addon>) => post<Addon>("/admin/addons", data),
  updateAddon: (id: string, data: Partial<Addon>) => put<Addon>(`/admin/addons/${id}`, data),
  deleteAddon: (id: string) => del(`/admin/addons/${id}`),
  getLicenseAddons: (id: string, params?: { limit?: number; offset?: number }) =>
    get<Paged<{ addons: LicenseAddon[] }>>(`/admin/licenses/${id}/addons?${listQuery(params)}`),
  addLicenseAddon: (id: string, addonId: string) =>
    post<LicenseAddon>(`/admin/licenses/${id}/addons`, { addon_id: addonId }),
  removeLicenseAddon: (id: string, addonId: string) => del(`/admin/licenses/${id}/addons/${addonId}`),
  getFloatingSessions: (id: string, params?: { limit?: number; offset?: number }) =>
    get<Paged<{ sessions: FloatingSession[]; active: number }>>(`/admin/licenses/${id}/floating?${listQuery(params)}`),

  listAuditLogs: (params?: {
    entity?: string
    entity_id?: string
    product_id?: string
    offset?: number
    limit?: number
  }) => {
    const q = new URLSearchParams()
    if (params?.entity) q.set("entity", params.entity)
    if (params?.entity_id) q.set("entity_id", params.entity_id)
    if (params?.product_id) q.set("product_id", params.product_id)
    if (params?.offset) q.set("offset", String(params.offset))
    if (params?.limit) q.set("limit", String(params.limit))
    return get<{ audit_logs: AuditLog[]; total: number }>(`/admin/audit-logs?${q}`)
  },

  listUsers: (params?: { search?: string; offset?: number; limit?: number }) => {
    const q = new URLSearchParams()
    if (params?.search) q.set("search", params.search)
    if (params?.offset) q.set("offset", String(params.offset))
    if (params?.limit) q.set("limit", String(params.limit))
    return get<{ users: User[]; total: number }>(`/admin/users?${q}`)
  },

  // Team (admin management)
  listTeam: (params?: { limit?: number; offset?: number }) =>
    get<Paged<{ members: User[] }>>(`/admin/team?${listQuery(params)}`),
  inviteTeamMember: (data: { email: string; role?: string }) => post<User>("/admin/team", data),
  removeTeamMember: (id: string) => del(`/admin/team/${id}`),

  getAnalyticsInsights: (params?: { product_id?: string }) => {
    const q = new URLSearchParams()
    if (params?.product_id) q.set("product_id", params.product_id)
    return get<AnalyticsInsights>(`/admin/analytics/insights?${q}`)
  },

  getUserDetail: (id: string) =>
    get<
      UserDetail & {
        license_key_hints: Record<string, string>
        license_key_prefixes: Record<string, string>
        // By seat ID: a membership's own key.
        member_key_hints: Record<string, string>
        member_key_prefixes: Record<string, string>
      }
    >(`/admin/users/${id}`),

  // Settings
  getSettings: () =>
    get<{
      settings: Record<string, string>
      secrets_set?: Record<string, boolean>
      email?: { configured: boolean; provider: string; source: string; host: string; from: string }
    }>("/admin/settings"),
  updateSettings: (settings: Record<string, string>) => put<{ status: string }>("/admin/settings", { settings }),
  sendTestEmail: (to?: string) => post<{ status: string }>("/admin/settings/test-email", to ? { to } : {}),
  clearSecretSetting: (key: string) => del<{ status: string; key: string }>(`/admin/settings/secrets/${key}`),

  // Email Templates
  getEmailTemplates: () =>
    get<{ templates: Record<string, { custom: string; default: string }> }>("/admin/email-templates"),

  // System
  getVersion: () => get<{ version: string; commit: string; build_date: string }>("/version"),
  checkUpdate: () =>
    get<{
      available: boolean
      latest_version: string
      current_version: string
      release_url?: string
      release_date?: string
      changelog?: string
      update_command?: string
      checked_at?: string
    }>("/admin/system/update-check"),

  // ─── Releases (industry-standard bundle model) ───
  listReleases: (params?: {
    product_id?: string
    channel?: string
    status?: string
    limit?: number
    offset?: number
  }) => {
    const q = new URLSearchParams()
    if (params?.product_id) q.set("product_id", params.product_id)
    if (params?.channel) q.set("channel", params.channel)
    if (params?.status) q.set("status", params.status)
    if (params?.limit) q.set("limit", String(params.limit))
    if (params?.offset) q.set("offset", String(params.offset))
    return get<{
      releases: Release[]
      total: number
      limit: number
      offset: number
      // Highest published version per product and channel, whatever the
      // filter and page.
      latest_published?: { product_id: string; channel: string; version: string }[]
    }>(`/admin/releases?${q}`)
  },
  getRelease: (id: string) => get<Release>(`/admin/releases/${id}`),
  createRelease: (data: {
    product_id: string
    version: string
    channel?: string
    name?: string
    release_notes?: string
  }) => post<Release>("/admin/releases", data),
  addArtifact: (
    releaseId: string,
    data: {
      platform: string
      content_type?: string
      expected_size?: number
      filename?: string
    },
  ) =>
    post<{ artifact: ReleaseArtifact; upload_url: string; expires_at: string }>(
      `/admin/releases/${releaseId}/artifacts`,
      data,
    ),
  finalizeArtifact: (releaseId: string, artifactId: string, data: { expected_sha256?: string }) =>
    post<ReleaseArtifact>(`/admin/releases/${releaseId}/artifacts/${artifactId}/finalize`, data),
  deleteArtifact: (releaseId: string, artifactId: string) =>
    del(`/admin/releases/${releaseId}/artifacts/${artifactId}`),
  publishRelease: (id: string) => post<Release>(`/admin/releases/${id}/actions/publish`),
  yankRelease: (id: string, reason: string) => post<Release>(`/admin/releases/${id}/actions/yank`, { reason }),
  unyankRelease: (id: string) => post<Release>(`/admin/releases/${id}/actions/unyank`),
  updateReleaseNotes: (id: string, data: { name?: string; release_notes?: string }) =>
    patch<Release>(`/admin/releases/${id}`, data),
  deleteRelease: (id: string) => del(`/admin/releases/${id}`),

  // ─── Release signing keys (per product) ───
  listSigningKeys: (productId: string) =>
    get<{ keys: ReleaseSigningKey[] }>(`/admin/products/${productId}/signing-keys`),
  generateSigningKey: (productId: string) => post<ReleaseSigningKey>(`/admin/products/${productId}/signing-key`),
  rotateSigningKey: (productId: string, note: string) =>
    post<ReleaseSigningKey>(`/admin/products/${productId}/signing-key/rotate`, { note }),
  deactivateSigningKey: (productId: string, note: string) =>
    request<{ status: string }>(`/admin/products/${productId}/signing-key`, {
      method: "DELETE",
      body: JSON.stringify({ note }),
    }),
  publicKeyURL: (productId: string) => `${BASE}/admin/products/${productId}/signing-key/public.pem`,
  tauriPublicKey: (productId: string) =>
    get<{ pubkey: string }>(`/admin/products/${productId}/signing-key/tauri-pubkey`),
}

// ─── Admin: payments ───
// The endpoints behind these helpers are the admin payment operations:
// reconciliation, manual settlement and the provider/price editors.

export interface PaymentOrderRow {
  id: string
  numeric_code: number
  code: string
  product_id: string
  plan_id: string
  price_id?: string
  license_id?: string
  email: string
  kind: string
  quantity: number
  subtotal_minor: number
  discount_minor: number
  tax_minor: number
  total_minor: number
  currency: string
  currency_exponent: number
  tax_rate_bp?: number
  provider: string
  status: string
  expires_at: string
  paid_at?: string
  period_start?: string
  period_end?: string
  created_by_api_key_id?: string
  last_settlement_error?: string
  created_at: string
  updated_at: string
}

export interface PaymentAttemptRow {
  id: string
  order_id: string
  provider: string
  provider_ref: string
  status: string
  amount_minor: number
  currency: string
  qr_payload: string
  checkout_url: string
  va_number: string
  raw?: unknown
  expires_at?: string
  created_at: string
  updated_at: string
}

export interface PaymentRefundRow {
  id: string
  order_id: string
  amount_minor: number
  currency: string
  reason: string
  method: string
  provider_ref: string
  status: string
  created_at: string
}

export interface PaymentTransactionRow {
  id: string
  provider: string
  provider_ref: string
  order_id?: string
  attempt_id?: string
  amount_minor: number
  currency: string
  direction: string
  status: string
  ignore_reason: string
  provider_account: string
  reference: string
  content: string
  matched_by?: string
  matched_at?: string
  raw?: unknown
  occurred_at: string
  created_at: string
}

export interface PlanPriceRow {
  id: string
  plan_id: string
  provider: string
  currency: string
  amount_minor: number
  currency_exponent: number
  billing_interval: string
  external_price_id: string
  active: boolean
  created_at: string
  updated_at: string
}

// ProviderRow is the admin projection: it says whether a credential or
// webhook secret is stored, never what it is. The fields below are the
// only ones the API returns.
export interface ProviderRow {
  id: string
  provider: string
  product_id?: string
  mode: string
  enabled: boolean
  config: Record<string, unknown> | null
  credentials_set: boolean
  secret_set: boolean
}

export interface PaymentHealthRow {
  provider: string
  enabled: boolean
  last_event_at?: string
  unmatched_open: number
  pending_orders: number
  last_error?: string
}

export const adminPayments = {
  orders: (params: { status?: string; provider?: string; email?: string; limit?: number; offset?: number }) =>
    get<Paged<{ orders: PaymentOrderRow[] }>>(`/admin/payments/orders?${listQuery(params)}`),
  order: (code: string) =>
    get<{
      order: PaymentOrderRow
      attempts: PaymentAttemptRow[]
      transactions: PaymentTransactionRow[]
      refunds?: PaymentRefundRow[]
    }>(`/admin/payments/orders/${encodeURIComponent(code)}`),
  markPaid: (code: string, reason: string) =>
    post<{ order: PaymentOrderRow }>(`/admin/payments/orders/${encodeURIComponent(code)}/mark-paid`, { reason }),
  cancel: (code: string) =>
    post<{ order: PaymentOrderRow }>(`/admin/payments/orders/${encodeURIComponent(code)}/cancel`),
  refund: (code: string, body: { amount_minor: number; reason: string; method: string }) =>
    post<{ refund: unknown; order: PaymentOrderRow }>(
      `/admin/payments/orders/${encodeURIComponent(code)}/refund`,
      body,
    ),
  unmatched: (limit = 50) =>
    get<{ transactions: PaymentTransactionRow[]; count: number; limit: number }>(
      `/admin/payments/unmatched?limit=${limit}`,
    ),
  match: (id: string, orderCode: string) =>
    post<{ transaction: PaymentTransactionRow; order: PaymentOrderRow }>(
      `/admin/payments/transactions/${encodeURIComponent(id)}/match`,
      { order_code: orderCode },
    ),
  ignore: (id: string, reason: string) =>
    post<{ transaction: PaymentTransactionRow }>(`/admin/payments/transactions/${encodeURIComponent(id)}/ignore`, {
      reason,
    }),
  priceList: (planId: string) => get<{ prices: PlanPriceRow[] }>(`/admin/plans/${encodeURIComponent(planId)}/prices`),
  // A successful upsert answers with the saved price row, not just its
  // id: the editor can update its table without a second request.
  upsertPrice: (
    planId: string,
    body: {
      provider: string
      currency: string
      amount_minor: number
      currency_exponent?: number
      billing_interval?: string
    },
  ) => put<PlanPriceRow>(`/admin/plans/${encodeURIComponent(planId)}/prices`, body),
  // DELETE deactivates the row and answers 204.
  deletePrice: (planId: string, priceId: string) =>
    del<void>(`/admin/plans/${encodeURIComponent(planId)}/prices/${encodeURIComponent(priceId)}`),
  providers: () => get<{ providers: ProviderRow[] }>("/admin/payments/providers"),
  // The saved row is returned so the editor does not have to refetch
  // the list to learn its new id and credential flags.
  upsertProvider: (body: unknown) => put<ProviderRow>("/admin/payments/providers", body),
  // The path segment is the provider name ("sepay" | "payos"), not the
  // config row id: one credential check serves every row of that rail.
  testProvider: (provider: string, productId?: string) =>
    post<{ provider: string; mode: string; ok: boolean; error?: string }>(
      `/admin/payments/providers/${encodeURIComponent(provider)}/test${
        productId ? `?product_id=${encodeURIComponent(productId)}` : ""
      }`,
    ),
  confirmPayOSWebhook: () =>
    post<{ provider: string; webhook_url: string; confirmed: boolean }>(
      "/admin/payments/providers/payos/confirm-webhook",
    ),
  // Per-provider health, as described by the reconciliation task. Both
  // key names are accepted so a small response-shape change does not
  // blank the panel.
  health: () => get<{ providers?: PaymentHealthRow[]; health?: PaymentHealthRow[] }>("/admin/payments/health"),
}

// ─── Phase 1b: coupons ───
// The backend module for coupons was written in parallel with this client,
// so every payload is read through a normaliser: a field that arrives as
// `amount` instead of `amount_minor`, a list under `items` instead of
// `coupons`, or a detail wrapped in `{coupon}` is accepted rather than
// blanking the page. The canonical shape is the one declared here.

type RawRecord = Record<string, any>

function rawRecord(value: unknown): RawRecord {
  if (typeof value === "string") {
    try {
      const parsed = JSON.parse(value)
      return parsed && typeof parsed === "object" ? (parsed as RawRecord) : {}
    } catch {
      return {}
    }
  }
  return value && typeof value === "object" ? (value as RawRecord) : {}
}

function rawString(value: unknown): string {
  if (typeof value === "string") return value
  if (value === null || value === undefined) return ""
  return String(value)
}

function rawNumber(value: unknown): number | undefined {
  if (typeof value === "number" && Number.isFinite(value)) return value
  if (typeof value === "string" && value.trim() !== "") {
    const parsed = Number(value)
    if (Number.isFinite(parsed)) return parsed
  }
  return undefined
}

function rawBool(value: unknown, fallback: boolean): boolean {
  return typeof value === "boolean" ? value : fallback
}

// pagedRows reads a list envelope whose array name may differ between
// handlers (`data`, `coupons`, `invoices`, `items`, `rows`). Coupon and
// redemption lists call the envelope key `data` while the other payment
// lists name their rows, so callers pass the names they expect. The three
// pagination numbers are passed through so useServerPagination sees the
// limit the server applied.
//
// Tolerant on purpose: an unknown key is not an error, because a list
// response may still be a bare array. A response whose shape matches
// none of the names yields an empty list, which surfaces as an empty
// state rather than a crash.
function pagedRows<T>(raw: unknown, keys: string[], normalise: (row: unknown) => T) {
  const rec = rawRecord(raw)
  let rows: unknown[] = []
  for (const key of keys) {
    if (Array.isArray(rec[key])) {
      rows = rec[key]
      break
    }
  }
  if (!rows.length && Array.isArray(raw)) rows = raw
  return {
    rows: rows.map(normalise),
    total: rawNumber(rec.total) ?? rows.length,
    limit: rawNumber(rec.limit) ?? rows.length,
    offset: rawNumber(rec.offset) ?? 0,
  }
}

export interface CouponScope {
  scope_type: "product" | "plan" | string
  scope_id: string
  // The admin handler resolves target names for display. Absent on a
  // scope the editor is about to save; the UI falls back to the id then.
  name?: string
}

export interface CouponStats {
  count: number
  amount_minor: number
}

export interface Coupon {
  id: string
  code: string
  description: string
  type: string
  value: number
  trial_days: number
  currency?: string
  stack_group: string
  stackable: boolean
  priority: number
  assigned_email: string
  max_redemptions: number
  redeemed_count: number
  per_customer_limit: number
  valid_from?: string
  valid_to?: string
  active: boolean
  created_at: string
  updated_at: string
  // Included by the list/detail handler when the backend has the scope
  // rows at hand. Absent means "not returned", not "no scopes".
  scopes?: CouponScope[]
  stats?: CouponStats
}

export interface CouponInput {
  code: string
  description: string
  type: "percent" | "fixed" | "trial"
  value: number
  trial_days: number
  currency: string
  stack_group: "general" | "campaign" | "private"
  stackable: boolean
  priority: number
  assigned_email: string
  max_redemptions: number
  per_customer_limit: number
  valid_from: string | null
  valid_to: string | null
  active: boolean
  scopes: CouponScope[]
}

export interface CouponDiscount {
  coupon_id?: string
  code: string
  type: string
  value?: number
  amount_minor: number
  priority?: number
  trial_days?: number
}

export interface CouponQuote {
  subtotal_minor?: number
  discount_minor?: number
  tax_minor?: number
  total_minor?: number
  currency?: string
  currency_exponent?: number
  discounts: CouponDiscount[]
  trial_days?: number
}

export interface CouponRedemptionRow {
  id: string
  coupon_id: string
  coupon_code: string
  order_id: string
  order_code: string
  email: string
  external_customer_id: string
  type: string
  amount_minor: number
  trial_days: number
  currency: string
  currency_exponent: number
  created_at: string
}

function normaliseScopes(value: unknown): CouponScope[] {
  if (!Array.isArray(value)) return []
  return value
    .map((scope) => {
      const rec = rawRecord(scope)
      return {
        scope_type: rawString(rec.scope_type),
        scope_id: rawString(rec.scope_id),
        name: rawString(rec.name) || undefined,
      }
    })
    .filter((scope) => scope.scope_type !== "" && scope.scope_id !== "")
}

function normaliseCouponStats(value: unknown): CouponStats | undefined {
  const rec = rawRecord(value)
  if (Object.keys(rec).length === 0) return undefined
  return {
    count: rawNumber(rec.count) ?? rawNumber(rec.redeemed_count) ?? 0,
    amount_minor: rawNumber(rec.amount_minor) ?? rawNumber(rec.total_minor) ?? 0,
  }
}

function normaliseCoupon(value: unknown): Coupon {
  const rec = rawRecord(value)
  // The detail handler calls its aggregate `redemptions`; older shapes
  // used `stats`/`usage`. Prefer whichever the payload carries.
  const stats = normaliseCouponStats(rec.stats ?? rec.usage ?? rec.redemptions)
  return {
    id: rawString(rec.id),
    code: rawString(rec.code),
    description: rawString(rec.description),
    type: rawString(rec.type) || "percent",
    value: rawNumber(rec.value) ?? 0,
    trial_days: rawNumber(rec.trial_days) ?? 0,
    currency: rawString(rec.currency) || undefined,
    stack_group: rawString(rec.stack_group) || "general",
    stackable: rawBool(rec.stackable, true),
    priority: rawNumber(rec.priority) ?? 100,
    assigned_email: rawString(rec.assigned_email),
    max_redemptions: rawNumber(rec.max_redemptions) ?? 0,
    redeemed_count: rawNumber(rec.redeemed_count) ?? stats?.count ?? 0,
    per_customer_limit: rawNumber(rec.per_customer_limit) ?? 0,
    valid_from: rawString(rec.valid_from) || undefined,
    valid_to: rawString(rec.valid_to) || undefined,
    active: rawBool(rec.active, true),
    created_at: rawString(rec.created_at),
    updated_at: rawString(rec.updated_at),
    scopes: Array.isArray(rec.scopes) ? normaliseScopes(rec.scopes) : undefined,
    stats,
  }
}

function normaliseCouponDetail(value: unknown): Coupon {
  const rec = rawRecord(value)
  const base = rawRecord(rec.coupon ?? value)
  const coupon = normaliseCoupon(base)
  const scopes = rec.scopes ?? base.scopes
  if (Array.isArray(scopes)) coupon.scopes = normaliseScopes(scopes)
  coupon.stats =
    normaliseCouponStats(rec.stats ?? base.stats ?? rec.usage ?? rec.redemptions ?? base.redemptions) ?? coupon.stats
  return coupon
}

function quoteMoney(rec: RawRecord, name: string): number | undefined {
  return rawNumber(rec[`${name}_minor`]) ?? rawNumber(rec[name]) ?? rawNumber(rec[`${name}_amount`])
}

// A discount row is frozen on the order as an amount plus the code that
// produced it. `amount` is accepted as an alias because the checkout
// model has carried both names across the phase-1b merge.
export function normaliseCouponDiscounts(value: unknown): CouponDiscount[] {
  return (Array.isArray(value) ? value : [])
    .map((entry) => {
      const row = rawRecord(entry)
      return {
        coupon_id: rawString(row.coupon_id) || undefined,
        code: rawString(row.code ?? row.coupon_code),
        type: rawString(row.type),
        value: rawNumber(row.value),
        amount_minor: rawNumber(row.amount_minor) ?? rawNumber(row.amount) ?? 0,
        priority: rawNumber(row.priority),
        trial_days: rawNumber(row.trial_days),
      }
    })
    .filter((discount) => discount.code !== "")
}

function normaliseCouponQuote(value: unknown): CouponQuote {
  const rec = rawRecord(rawRecord(value).quote ?? value)
  const discountRows = rec.discounts ?? rec.applied ?? rec.coupon_discounts
  const discounts = normaliseCouponDiscounts(discountRows)
  return {
    subtotal_minor: quoteMoney(rec, "subtotal"),
    discount_minor: quoteMoney(rec, "discount"),
    tax_minor: quoteMoney(rec, "tax"),
    total_minor: quoteMoney(rec, "total") ?? quoteMoney(rec, "amount"),
    currency: rawString(rec.currency) || undefined,
    currency_exponent: rawNumber(rec.currency_exponent) ?? rawNumber(rec.currency_exp),
    discounts,
    trial_days: rawNumber(rec.trial_days),
  }
}

// The admin preview prices stored codes only. Its body is exactly the
// fields the handler reads: plan, provider, quantity, codes, invalid-code
// policy, buyer identity. There is no subtotal override or inline coupon
// definition — the quote always prices the plan's configured price.
export interface CouponPreviewInput {
  plan_id: string
  provider?: string
  quantity?: number
  codes?: string[]
  ignore_invalid?: boolean
  email?: string
  external_customer_id?: string
}

export interface CouponPreviewTarget {
  id: string
  name: string
  slug?: string
  license_type?: string
}

export interface CouponPreviewPrice {
  provider: string
  currency: string
  amount_minor: number
  billing_interval: string
}

export interface CouponPreview {
  plan: CouponPreviewTarget
  product: CouponPreviewTarget
  price: CouponPreviewPrice
  quote: CouponQuote
}

export class CouponPreviewUnavailableError extends Error {}

// A 404/405 on the preview path means the deployment does not mount the
// route, not that the coupon is invalid. Anything else (409, 422, ...)
// carries a payment error code and is surfaced to the editor.
function isMissingRoute(err: unknown): boolean {
  const code = errorCode(err)
  const message = err instanceof Error ? err.message : ""
  return (!code || code === "NOT_FOUND") && /(404|405|not found|method not allowed)/i.test(message)
}

function normaliseCouponPreview(value: unknown): CouponPreview {
  const rec = rawRecord(value)
  const plan = rawRecord(rec.plan)
  const product = rawRecord(rec.product)
  const price = rawRecord(rec.price)
  const quote = normaliseCouponQuote(rec.quote ?? value)
  const currency = rawString(price.currency)
  if (!quote.currency) quote.currency = currency || undefined
  return {
    plan: {
      id: rawString(plan.id),
      name: rawString(plan.name),
      slug: rawString(plan.slug) || undefined,
      license_type: rawString(plan.license_type) || undefined,
    },
    product: {
      id: rawString(product.id),
      name: rawString(product.name),
      slug: rawString(product.slug) || undefined,
    },
    price: {
      provider: rawString(price.provider),
      currency,
      amount_minor: rawNumber(price.amount_minor) ?? 0,
      billing_interval: rawString(price.billing_interval),
    },
    quote,
  }
}

export const adminCoupons = {
  list: async (params?: {
    q?: string
    active?: boolean
    stack_group?: string
    scope_type?: string
    scope_id?: string
    limit?: number
    offset?: number
  }): Promise<Paged<{ coupons: Coupon[] }>> => {
    const query = listQuery({
      q: params?.q,
      active: params?.active === undefined ? undefined : String(params.active),
      stack_group: params?.stack_group,
      scope_type: params?.scope_type,
      scope_id: params?.scope_id,
      limit: params?.limit,
      offset: params?.offset,
    })
    const raw = await get<unknown>(`/admin/coupons?${query}`)
    // The coupon handler calls the array `data` inside the envelope.
    const page = pagedRows(raw, ["data", "coupons", "items", "rows"], normaliseCoupon)
    return {
      coupons: page.rows,
      total: page.total,
      limit: page.limit,
      offset: page.offset,
    }
  },
  get: async (id: string) => normaliseCouponDetail(await get<unknown>(`/admin/coupons/${encodeURIComponent(id)}`)),
  create: async (input: CouponInput) => normaliseCouponDetail(await post<unknown>("/admin/coupons", input)),
  update: async (id: string, input: CouponInput) =>
    normaliseCouponDetail(await put<unknown>(`/admin/coupons/${encodeURIComponent(id)}`, input)),
  // The route deletes an unused coupon and archives a redeemed one; the
  // response says which happened so the list can word the toast.
  remove: async (id: string) =>
    (await del<unknown>(`/admin/coupons/${encodeURIComponent(id)}`)) as
      | {
          archived?: boolean
          deleted?: boolean
        }
      | undefined,
  redemptions: async (
    id: string,
    params?: { limit?: number; offset?: number },
  ): Promise<Paged<{ redemptions: CouponRedemptionRow[] }>> => {
    const raw = await get<unknown>(`/admin/coupons/${encodeURIComponent(id)}/redemptions?${listQuery(params)}`)
    const page = pagedRows(raw, ["data", "redemptions", "items", "rows"], normaliseRedemption)
    return { redemptions: page.rows, total: page.total, limit: page.limit, offset: page.offset }
  },
  // Live quote preview for the editor. It prices the saved coupon
  // definition, so the editor only calls it for a coupon that exists.
  // Throws CouponPreviewUnavailableError when the deployment does not
  // mount the route, which the editor reports instead of inventing a
  // subtotal or a tax rate.
  preview: async (input: CouponPreviewInput): Promise<CouponPreview> => {
    const body: CouponPreviewInput = { ...input, quantity: input.quantity ?? 1 }
    try {
      return normaliseCouponPreview(await post<unknown>("/admin/coupons/preview", body))
    } catch (err) {
      if (isMissingRoute(err)) {
        throw new CouponPreviewUnavailableError("coupon preview is not available on this server")
      }
      throw err
    }
  },
}

function normaliseRedemption(value: unknown): CouponRedemptionRow {
  const rec = rawRecord(value)
  return {
    id: rawString(rec.id),
    coupon_id: rawString(rec.coupon_id),
    coupon_code: rawString(rec.coupon_code),
    order_id: rawString(rec.order_id),
    order_code: rawString(rec.order_code),
    email: rawString(rec.email),
    external_customer_id: rawString(rec.external_customer_id),
    type: rawString(rec.type),
    amount_minor: rawNumber(rec.amount_minor) ?? 0,
    trial_days: rawNumber(rec.trial_days) ?? 0,
    currency: rawString(rec.currency) || "VND",
    currency_exponent: rawNumber(rec.currency_exponent) ?? rawNumber(rec.currency_exp) ?? 0,
    created_at: rawString(rec.created_at),
  }
}

export interface RedemptionFilter {
  coupon?: string
  email?: string
  from?: string
  to?: string
  limit?: number
  offset?: number
}

export const adminRedemptions = {
  list: async (params?: RedemptionFilter): Promise<Paged<{ redemptions: CouponRedemptionRow[] }>> => {
    const raw = await get<unknown>(
      `/admin/payments/redemptions?${listQuery({
        coupon: params?.coupon,
        email: params?.email,
        from: params?.from,
        to: params?.to,
        limit: params?.limit,
        offset: params?.offset,
      })}`,
    )
    const page = pagedRows(raw, ["data", "redemptions", "items", "rows"], normaliseRedemption)
    return { redemptions: page.rows, total: page.total, limit: page.limit, offset: page.offset }
  },
  csv: (params?: RedemptionFilter) =>
    requestFile(`/admin/payments/redemptions?${listQuery({ ...params, format: "csv" })}`),
}

// ─── Phase 1b: invoices ───

export interface InvoiceBuyer {
  name: string
  company_name: string
  tax_code: string
  address: string
  email: string
  phone: string
  request_invoice?: boolean
}

export interface InvoiceLineItem {
  description: string
  plan_id?: string
  quantity: number
  unit_amount_minor: number
  amount_minor: number
  tax_rate_bp: number
}

export interface PaymentInvoice {
  id: string
  number: string
  order_id: string
  license_id?: string
  status: string
  buyer: InvoiceBuyer
  items: InvoiceLineItem[]
  subtotal_minor: number
  discount_minor: number
  tax_minor: number
  total_minor: number
  currency: string
  currency_exponent: number
  e_invoice_provider?: string
  e_invoice_ref?: string
  e_invoice_status?: string
  pdf_url?: string
  issued_at?: string
  created_at: string
  updated_at: string
  // The invoice row freezes the buyer, not the seller; the admin detail
  // renders a seller block only if the handler includes the current
  // settings snapshot.
  seller?: InvoiceBuyer
}

function normaliseBuyer(value: unknown): InvoiceBuyer {
  const rec = rawRecord(value)
  return {
    name: rawString(rec.name ?? rec.buyer_name),
    company_name: rawString(rec.company_name ?? rec.company),
    tax_code: rawString(rec.tax_code ?? rec.tax_id),
    address: rawString(rec.address),
    email: rawString(rec.email),
    phone: rawString(rec.phone),
    request_invoice: rawBool(rec.request_invoice, false),
  }
}

function normaliseInvoiceItem(value: unknown): InvoiceLineItem {
  const rec = rawRecord(value)
  return {
    description: rawString(rec.description),
    plan_id: rawString(rec.plan_id) || undefined,
    quantity: rawNumber(rec.quantity) ?? 1,
    unit_amount_minor: rawNumber(rec.unit_amount_minor) ?? 0,
    amount_minor: rawNumber(rec.amount_minor) ?? 0,
    tax_rate_bp: rawNumber(rec.tax_rate_bp) ?? 0,
  }
}

function normaliseInvoice(value: unknown): PaymentInvoice {
  const rec = rawRecord(rawRecord(value).invoice ?? value)
  return {
    id: rawString(rec.id),
    number: rawString(rec.number),
    order_id: rawString(rec.order_id),
    license_id: rawString(rec.license_id) || undefined,
    status: rawString(rec.status),
    buyer: normaliseBuyer(rec.buyer),
    items: Array.isArray(rec.items) ? rec.items.map(normaliseInvoiceItem) : [],
    subtotal_minor: rawNumber(rec.subtotal_minor) ?? 0,
    discount_minor: rawNumber(rec.discount_minor) ?? 0,
    tax_minor: rawNumber(rec.tax_minor) ?? 0,
    total_minor: rawNumber(rec.total_minor) ?? 0,
    currency: rawString(rec.currency) || "VND",
    currency_exponent: rawNumber(rec.currency_exponent) ?? rawNumber(rec.currency_exp) ?? 0,
    e_invoice_provider: rawString(rec.e_invoice_provider) || undefined,
    e_invoice_ref: rawString(rec.e_invoice_ref) || undefined,
    e_invoice_status: rawString(rec.e_invoice_status) || undefined,
    pdf_url: rawString(rec.pdf_url) || undefined,
    issued_at: rawString(rec.issued_at) || undefined,
    created_at: rawString(rec.created_at),
    updated_at: rawString(rec.updated_at),
    seller: rec.seller ? normaliseBuyer(rec.seller) : undefined,
  }
}

export interface InvoiceFilter {
  status?: string
  license_id?: string
  order_id?: string
  number?: string
  product_id?: string
  from?: string
  to?: string
  limit?: number
  offset?: number
}

export const adminInvoices = {
  list: async (params?: InvoiceFilter): Promise<Paged<{ invoices: PaymentInvoice[] }>> => {
    const raw = await get<unknown>(
      `/admin/payments/invoices?${listQuery({
        status: params?.status,
        license_id: params?.license_id,
        order_id: params?.order_id,
        number: params?.number,
        product_id: params?.product_id,
        from: params?.from,
        to: params?.to,
        limit: params?.limit,
        offset: params?.offset,
      })}`,
    )
    const page = pagedRows(raw, ["invoices", "items", "rows"], normaliseInvoice)
    return { invoices: page.rows, total: page.total, limit: page.limit, offset: page.offset }
  },
  get: async (id: string) => normaliseInvoice(await get<unknown>(`/admin/payments/invoices/${encodeURIComponent(id)}`)),
  issue: async (id: string) =>
    normaliseInvoice(await post<unknown>(`/admin/payments/invoices/${encodeURIComponent(id)}/issue`)),
  void: async (id: string, reason: string) =>
    normaliseInvoice(await post<unknown>(`/admin/payments/invoices/${encodeURIComponent(id)}/void`, { reason })),
  pdf: (id: string) => requestFile(`/admin/payments/invoices/${encodeURIComponent(id)}/pdf`),
}

// ─── Phase 1b: reports ───

export interface RevenueReportRow {
  period?: string
  provider?: string
  plan_id?: string
  plan_name?: string
  product_id?: string
  product_name?: string
  currency: string
  currency_exponent: number
  orders: number
  subtotal_minor: number
  discount_minor: number
  tax_minor: number
  total_minor: number
}

export interface RevenueReportTotals {
  currency: string
  currency_exponent: number
  orders: number
  subtotal_minor: number
  discount_minor: number
  tax_minor: number
  total_minor: number
}

export interface ReconciliationReportRow {
  day: string
  provider: string
  currency: string
  orders_count: number
  transactions_count: number
  orders_minor: number
  transactions_minor: number
  drift_minor: number
}

export interface ReconciliationReportTotals {
  currency: string
  orders_count: number
  transactions_count: number
  orders_minor: number
  transactions_minor: number
  drift_minor: number
}

export interface VatReportRow {
  period: string
  currency: string
  currency_exponent: number
  tax_rate_bp: number
  orders: number
  taxable_base_minor: number
  tax_minor: number
  total_minor: number
}

export interface VatReportTotals {
  currency: string
  orders: number
  taxable_base_minor: number
  tax_minor: number
  total_minor: number
}

export interface ReportWindow {
  from: string
  to: string
  timezone: string
}

export interface RevenueReport extends ReportWindow {
  group_by: string
  period: string
  rows: RevenueReportRow[]
  totals: RevenueReportTotals[]
}

export interface ReconciliationReport extends ReportWindow {
  rows: ReconciliationReportRow[]
  totals: ReconciliationReportTotals[]
  drift_minor: number
}

export interface VatReport extends ReportWindow {
  rows: VatReportRow[]
  totals: VatReportTotals[]
}

export interface ReportFilter {
  from?: string
  to?: string
  group_by?: string
  period?: string
  provider?: string
  product_id?: string
  plan_id?: string
}

export type ReportName = "revenue" | "reconciliation" | "vat"

function reportQuery(params?: ReportFilter) {
  return listQuery({
    from: params?.from,
    to: params?.to,
    group_by: params?.group_by,
    period: params?.period,
    provider: params?.provider,
    product_id: params?.product_id,
    plan_id: params?.plan_id,
  })
}

export const adminReports = {
  revenue: async (params?: ReportFilter): Promise<RevenueReport> => {
    const raw = rawRecord(await get<unknown>(`/admin/payments/reports/revenue?${reportQuery(params)}`))
    return {
      from: rawString(raw.from),
      to: rawString(raw.to),
      timezone: rawString(raw.timezone) || "Asia/Ho_Chi_Minh",
      group_by: rawString(raw.group_by) || "period",
      period: rawString(raw.period) || "day",
      rows: Array.isArray(raw.rows) ? (raw.rows as RevenueReportRow[]) : [],
      totals: Array.isArray(raw.totals) ? (raw.totals as RevenueReportTotals[]) : [],
    }
  },
  reconciliation: async (params?: ReportFilter): Promise<ReconciliationReport> => {
    const raw = rawRecord(await get<unknown>(`/admin/payments/reports/reconciliation?${reportQuery(params)}`))
    return {
      from: rawString(raw.from),
      to: rawString(raw.to),
      timezone: rawString(raw.timezone) || "Asia/Ho_Chi_Minh",
      rows: Array.isArray(raw.rows) ? (raw.rows as ReconciliationReportRow[]) : [],
      totals: Array.isArray(raw.totals) ? (raw.totals as ReconciliationReportTotals[]) : [],
      drift_minor: rawNumber(raw.drift_minor) ?? 0,
    }
  },
  vat: async (params?: ReportFilter): Promise<VatReport> => {
    const raw = rawRecord(await get<unknown>(`/admin/payments/reports/vat?${reportQuery(params)}`))
    return {
      from: rawString(raw.from),
      to: rawString(raw.to),
      timezone: rawString(raw.timezone) || "Asia/Ho_Chi_Minh",
      rows: Array.isArray(raw.rows) ? (raw.rows as VatReportRow[]) : [],
      totals: Array.isArray(raw.totals) ? (raw.totals as VatReportTotals[]) : [],
    }
  },
  csv: (report: ReportName, params?: ReportFilter) =>
    requestFile(`/admin/payments/reports/${report}?${listQuery({ ...params, format: "csv" })}`),
}

// ─── Phase 1b: portal orders and invoices ───

export interface PaymentBankAccount {
  bank_name: string
  bank_bin?: string
  account_no: string
  account_name: string
}

export interface PaymentInstructions {
  provider?: string
  provider_ref?: string
  qr_payload?: string
  qr_image_url?: string
  checkout_url?: string
  va_number?: string
  description?: string
  expires_at?: string
  bank_account?: PaymentBankAccount
}

export interface PortalPaymentOrder {
  id?: string
  code: string
  kind: string
  status: string
  provider: string
  product_id?: string
  plan_id?: string
  plan_name?: string
  product_name?: string
  quantity?: number
  subtotal_minor?: number
  discount_minor?: number
  tax_minor?: number
  total_minor: number
  currency: string
  currency_exponent: number
  email?: string
  expires_at?: string
  paid_at?: string
  created_at?: string
  license_id?: string
  // Not returned by the shipped portal order view; the detail page links
  // to the invoice list instead. Kept for deployments that add the field.
  invoice_id?: string
  // The order view only carries a breakdown when the handler attached
  // the frozen discount rows. The shipped handler does not, so this is
  // usually undefined and the page shows the order total alone.
  discounts?: CouponDiscount[]
  payment?: PaymentInstructions
}

function normaliseBankAccount(value: unknown): PaymentBankAccount | undefined {
  const rec = rawRecord(value)
  if (Object.keys(rec).length === 0) return undefined
  const accountNo = rawString(rec.account_no ?? rec.account_number)
  const bankName = rawString(rec.bank_name ?? rec.bank)
  if (!accountNo && !bankName) return undefined
  return {
    bank_name: bankName,
    bank_bin: rawString(rec.bank_bin) || undefined,
    account_no: accountNo,
    account_name: rawString(rec.account_name),
  }
}

function normalisePayment(value: unknown): PaymentInstructions {
  const rec = rawRecord(value)
  return {
    provider: rawString(rec.provider) || undefined,
    provider_ref: rawString(rec.provider_ref) || undefined,
    qr_payload: rawString(rec.qr_payload) || undefined,
    qr_image_url: rawString(rec.qr_image_url) || undefined,
    checkout_url: rawString(rec.checkout_url) || undefined,
    va_number: rawString(rec.va_number) || undefined,
    description: rawString(rec.description) || undefined,
    expires_at: rawString(rec.expires_at) || undefined,
    bank_account: normaliseBankAccount(rec.bank_account ?? rec.bank),
  }
}

function normalisePortalOrder(value: unknown): PortalPaymentOrder {
  const rec = rawRecord(value)
  return {
    id: rawString(rec.id) || undefined,
    code: rawString(rec.code),
    kind: rawString(rec.kind) || "new",
    status: rawString(rec.status),
    provider: rawString(rec.provider),
    product_id: rawString(rec.product_id) || undefined,
    plan_id: rawString(rec.plan_id) || undefined,
    plan_name: rawString(rec.plan_name) || undefined,
    product_name: rawString(rec.product_name) || undefined,
    quantity: rawNumber(rec.quantity),
    subtotal_minor: rawNumber(rec.subtotal_minor),
    discount_minor: rawNumber(rec.discount_minor),
    tax_minor: rawNumber(rec.tax_minor),
    total_minor: rawNumber(rec.total_minor) ?? 0,
    currency: rawString(rec.currency) || "VND",
    currency_exponent: rawNumber(rec.currency_exponent) ?? rawNumber(rec.currency_exp) ?? 0,
    email: rawString(rec.email) || undefined,
    expires_at: rawString(rec.expires_at) || undefined,
    paid_at: rawString(rec.paid_at) || undefined,
    created_at: rawString(rec.created_at) || undefined,
    license_id: rawString(rec.license_id) || undefined,
    invoice_id: rawString(rec.invoice_id) || undefined,
    discounts: (() => {
      const rows = normaliseCouponDiscounts(rec.discounts ?? rec.coupon_discounts)
      return rows.length > 0 ? rows : undefined
    })(),
    payment: undefined,
  }
}

function normalisePortalOrderDetail(value: unknown): {
  order: PortalPaymentOrder
  payment: PaymentInstructions
} {
  const rec = rawRecord(value)
  const orderRaw = rawRecord(rec.order ?? value)
  const order = normalisePortalOrder(orderRaw)
  const payment = normalisePayment(rec.payment ?? orderRaw.payment ?? rec.instructions)
  // Some handlers flatten the attempt into the order row. Fill only the
  // fields the dedicated payment block did not answer.
  const fallback = normalisePayment(orderRaw)
  const merged: PaymentInstructions = {
    provider: payment.provider ?? fallback.provider,
    provider_ref: payment.provider_ref ?? fallback.provider_ref,
    qr_payload: payment.qr_payload ?? fallback.qr_payload,
    qr_image_url: payment.qr_image_url ?? fallback.qr_image_url,
    checkout_url: payment.checkout_url ?? fallback.checkout_url,
    va_number: payment.va_number ?? fallback.va_number,
    description: payment.description ?? fallback.description,
    expires_at: payment.expires_at ?? fallback.expires_at ?? order.expires_at,
    bank_account: payment.bank_account ?? fallback.bank_account,
  }
  order.payment = merged
  return { order, payment: merged }
}

export const portalPayments = {
  // The shipped portal list reads only limit/offset; it has no status
  // filter. The list page filters the rows it has loaded client-side and
  // labels that as a loaded-page filter rather than implying server
  // filtering.
  orders: async (params?: { limit?: number; offset?: number }): Promise<Paged<{ orders: PortalPaymentOrder[] }>> => {
    const raw = await get<unknown>(`/portal/payments/orders?${listQuery(params)}`)
    const page = pagedRows(raw, ["orders", "items", "rows"], normalisePortalOrder)
    return { orders: page.rows, total: page.total, limit: page.limit, offset: page.offset }
  },
  order: async (code: string) =>
    normalisePortalOrderDetail(await get<unknown>(`/portal/payments/orders/${encodeURIComponent(code)}`)),
  // Re-issues/refreshes the payment instruction. The backend refuses a
  // terminal or expiry-passed order with PAYMENT_ORDER_NOT_PAYABLE.
  pay: async (code: string) =>
    normalisePortalOrderDetail(await post<unknown>(`/portal/payments/orders/${encodeURIComponent(code)}/pay`)),
  cancel: async (code: string) =>
    normalisePortalOrderDetail(await post<unknown>(`/portal/payments/orders/${encodeURIComponent(code)}/cancel`)),
}

export const portalInvoices = {
  list: async (params?: { limit?: number; offset?: number }): Promise<Paged<{ invoices: PaymentInvoice[] }>> => {
    const raw = await get<unknown>(`/portal/invoices?${listQuery(params)}`)
    const page = pagedRows(raw, ["invoices", "items", "rows"], normaliseInvoice)
    return { invoices: page.rows, total: page.total, limit: page.limit, offset: page.offset }
  },
  // The full document is on the detail route. If a deployment mounts
  // only the list route, fall back to the row the list already returns so
  // the detail page still opens (without line items).
  get: async (id: string) => {
    try {
      return normaliseInvoice(await get<unknown>(`/portal/invoices/${encodeURIComponent(id)}`))
    } catch (err) {
      if (errorCode(err) !== "NOT_FOUND") throw err
      const list = await portalInvoices.list({ limit: 200 })
      const row = list.invoices.find((invoice) => invoice.id === id)
      if (!row) throw err
      return row
    }
  },
  pdf: (id: string) => requestFile(`/portal/invoices/${encodeURIComponent(id)}?format=pdf`),
}

export interface BillingProfileInput {
  name: string
  company_name: string
  tax_code: string
  address: string
  email: string
  phone: string
  request_invoice: boolean
}

function normaliseBillingProfile(value: unknown): InvoiceBuyer {
  const rec = rawRecord(value)
  return normaliseBuyer(rec.buyer ?? rawRecord(rec.profile).buyer ?? rec.profile ?? rec)
}

export const portalBilling = {
  get: async () => normaliseBillingProfile(await get<unknown>("/portal/billing-profile")),
  update: async (buyer: BillingProfileInput) =>
    normaliseBillingProfile(await put<unknown>("/portal/billing-profile", buyer)),
}

// ─── File downloads (invoice PDF, report/redemption CSV) ───
//
// There is deliberately no client for the public `POST /payments/quote`
// route: the shipped server does not register a handler for it. The pay
// page only reads an existing order's status and never re-prices one, so
// there is nothing here to call. If the route is mounted later, add the
// client alongside its handler rather than guessing its shape.

export interface DownloadedFile {
  blob: Blob
  filename?: string
}

function filenameFromDisposition(value: string | null): string | undefined {
  if (!value) return undefined
  const encoded = /filename\*=UTF-8''([^;]+)/i.exec(value)
  if (encoded) {
    try {
      return decodeURIComponent(encoded[1])
    } catch {
      return encoded[1]
    }
  }
  const plain = /filename="?([^";]+)"?/i.exec(value)
  return plain?.[1]
}

// requestFile is the blob counterpart of request: it keeps the session
// renewal behaviour and turns a JSON error body into the same Error the
// pages already know how to translate.
async function requestFile(path: string, opts?: RequestInit, retried = false): Promise<DownloadedFile> {
  let res: Response
  try {
    res = await fetch(BASE + path, { credentials: "include", ...opts })
  } catch (e) {
    const reason = e instanceof Error ? e.message : String(e)
    throw new ServiceUnavailableError(`Network error: ${reason}. Is the server reachable?`)
  }

  if (res.status === 401 && !retried) {
    const outcome = await refreshSession()
    if (outcome === "ok") return requestFile(path, opts, true)
    if (outcome === "unavailable") {
      throw new ServiceUnavailableError("Could not renew your session right now. Check your connection and try again.")
    }
  }
  if (res.status === 401) {
    if (!window.location.pathname.startsWith("/login")) window.location.href = "/login"
    throw new Error("Session expired")
  }
  if (!res.ok) {
    const raw = await res.text()
    let json: any = null
    try {
      json = raw ? JSON.parse(raw) : null
    } catch {
      json = null
    }
    const msg =
      json?.error?.message ||
      (raw && raw.length < 200 ? raw : null) ||
      `Request failed (${res.status}${res.statusText ? ` ${res.statusText}` : ""})`
    const err = res.status === 429 || res.status >= 500 ? new ServiceUnavailableError(msg) : new Error(msg)
    throw Object.assign(err, {
      code: typeof json?.error?.code === "string" ? json.error.code : undefined,
      details: json?.error?.details && typeof json.error.details === "object" ? json.error.details : undefined,
    })
  }
  return {
    blob: await res.blob(),
    filename: filenameFromDisposition(res.headers.get("Content-Disposition")),
  }
}

// ─── Types ───

export interface User {
  id: string
  email: string
  name: string
  avatar_url?: string
  role: string // "owner" | "admin" | "user"
  created_at: string
  updated_at: string
}

export interface Product {
  id: string
  name: string
  slug: string
  type: string
  license_prefix: string
  minimum_supported_version?: string
  minimum_supported_message?: string
  require_signing: boolean
  // Update feeds answer only with a license key (maintenance-period products).
  feed_license_required?: boolean
  feed_gated_at?: string
  /** Vendor's download page, used as {{.DownloadURL}} in emails. */
  download_url?: string
  created_at: string
}

export interface Plan {
  id: string
  product_id: string
  name: string
  slug: string
  checkout_id: string
  license_type: string
  billing_interval?: string
  max_activations: number
  license_model?: string
  floating_timeout?: number
  token_ttl_days?: number
  // Self sign-up (free plans only): visitors claim a licence from the
  // plan's /pay link by confirming their email.
  self_signup?: boolean
  signup_daily_cap?: number
  signup_consent_text?: string
  max_seats: number
  // Sold per seat (subscriptions only): the buyer picks a quantity in
  // [min_quantity, max_quantity]; max_activations is per seat.
  per_seat?: boolean
  min_quantity?: number
  max_quantity?: number
  trial_days: number
  // Trial plans only: the free plan a licence moves to when its trial
  // ends, instead of expiring. Absent = the trial expires.
  trial_fallback_plan_id?: string
  grace_days: number
  stripe_price_id?: string
  // Maintenance period (perpetual plans): days of updates a purchase
  // includes (0 = for life) and the one-time renewal sold in the portal.
  updates_days?: number
  renewal_days?: number
  stripe_renewal_price_id?: string
  active: boolean
  sort_order: number
  created_at: string
  product?: Product
  entitlements?: Entitlement[]
}

export interface Entitlement {
  id: string
  plan_id: string
  feature: string
  value_type: string
  value: string
  quota_period?: string
  quota_unit?: string
}

export interface License {
  id: string
  product_id: string
  plan_id: string
  user_id?: string
  email: string
  // Present only where the server deliberately attaches it: the
  // customer portal (their own key) and the create-license response.
  // Admin list/detail carry a last-four hint instead — see PortalLicense.
  license_key?: string
  payment_provider?: string
  // Set only for subscription-backed Stripe licenses. A one-time
  // purchase has no subscription, so nothing to change, cancel, or
  // update a payment method for.
  stripe_subscription_id?: string
  status: string
  valid_from: string
  valid_until?: string
  // End of a perpetual license's update period; absent = no separate limit.
  updates_until?: string
  canceled_at?: string
  suspended_at?: string
  org_name?: string
  notes?: string
  external_customer_id?: string
  external_workspace_id?: string
  // Set on a licence the customer claimed through self sign-up.
  signup_source?: string
  marketing_consent?: boolean
  marketing_consent_at?: string
  // Seats the licence is paid for; only a plan sold per seat turns it
  // into limits.
  quantity?: number
  // The limits the server enforces (people, devices or floating
  // sessions); 0 seats means no limit. Present when the plan is loaded.
  seat_limit?: number
  activation_limit?: number
  // A subscription change waits for payment until this moment.
  stripe_pending_update_expires_at?: string
  created_at: string
  updated_at: string
  product?: Product
  plan?: Plan
  // How many activations the licence has. Present on list rows, where
  // the activations themselves are not; the detail payload carries
  // both and they agree.
  activation_count?: number
  // Floating seats in use right now. A floating plan keeps occupancy
  // in its own table, so activation_count reads zero for one however
  // full it is; this is the number that answers "how full" there.
  active_session_count?: number
  activations?: Activation[]
  seats?: Seat[]
  addons?: LicenseAddon[]
}

// The portal always receives the key: it is the customer's own
// credential and also names the license in the activation and seat
// endpoints. Admin payloads never carry it.
export interface PortalLicense extends License {
  license_key: string
  // The owner can upgrade it to a paid plan now, keeping the key.
  upgradable?: boolean
  // For a member: their seat is paused (over the seat limit).
  key_paused?: boolean
  // Its subscription ends at the period end rather than renews (owner only).
  cancel_scheduled?: boolean
  // What the license grants: the plan's features with its addons applied.
  features?: Entitlement[]
}

export interface Activation {
  id: string
  license_id: string
  identifier: string
  identifier_type: string
  label?: string
  ip_address?: string
  last_verified: string
  created_at: string
  // The member whose own key activated the device; absent for the
  // owner's key.
  seat_id?: string
  member_email?: string
}

export interface APIKey {
  id: string
  product_id: string
  name: string
  prefix: string
  scopes: string[]
  last_used?: string
  last_used_ip?: string
  created_at: string
  product?: Product
}

export interface AuditLog {
  id: string
  entity: string
  entity_id: string
  action: string
  actor_id?: string
  actor_type?: string
  changes?: Record<string, unknown>
  ip_address?: string
  created_at: string
}

export interface Stats {
  total_licenses: number
  active_licenses: number
  total_activations: number
  total_products: number
  total_seats: number
  total_usage_events: number
  total_webhooks: number
  by_status: Record<string, number>
  recent_licenses: License[]
}

export interface Seat {
  id: string
  license_id: string
  email: string
  role: string
  user_id?: string
  invited_at: string
  invite_expires_at?: string
  // Over the licence's seat limit: joined after the places ran out, so
  // their own key may not be used until a place is free.
  paused?: boolean
  accepted_at?: string
  removed_at?: string
  created_at: string
}

// A seat as the admin's seat list gives it: the member's own key by its
// prefix and last four characters, absent until they have one.
export interface AdminSeat extends Seat {
  key_prefix?: string
  key_hint?: string
  device_count: number
}

export interface UsageEvent {
  id: string
  license_id: string
  feature: string
  quantity: number
  metadata?: Record<string, unknown>
  ip_address?: string
  recorded_at: string
}

export interface UsageCounter {
  id: string
  license_id: string
  feature: string
  period: string
  period_key: string
  used: number
  updated_at: string
}

export interface WebhookConfig {
  id: string
  product_id: string
  url: string
  events: string[]
  active: boolean
  created_at: string
  updated_at: string
  product?: Product
}

export interface WebhookDeliveryLog {
  id: string
  webhook_id: string
  event: string
  payload?: Record<string, unknown>
  response_code?: number
  response_body?: string
  status: string
  attempts: number
  created_at: string
  delivered_at?: string
}

export interface AnalyticsSnapshot {
  id: string
  date: string
  product_id: string
  total_licenses: number
  active_licenses: number
  new_licenses: number
  churned: number
  total_activations: number
  total_seats: number
  total_usage: number
}

export interface AnalyticsSummary {
  total_licenses: number
  active_licenses: number
  trialing_licenses: number
  expired_licenses: number
  canceled_licenses: number
  suspended_licenses: number
  revoked_licenses: number
  past_due_licenses: number
  total_activations: number
  total_seats: number
  avg_activations_per_license: number
}

export interface BreakdownItem {
  key: string
  label: string
  count: number
}

export interface FeatureUsageItem {
  feature: string
  total_usage: number
  unique_users: number
}

export interface TrendPoint {
  date: string
  count: number
}

export interface AggregatedSnapshot {
  period: string
  total_licenses: number
  active_licenses: number
  new_licenses: number
  churned: number
  total_activations: number
  total_seats: number
  total_usage: number
}

export interface FloatingSession {
  id: string
  license_id: string
  identifier: string
  label?: string
  ip_address?: string
  checked_out: string
  expires_at: string
  heartbeat: string
}

export interface Addon {
  id: string
  product_id: string
  name: string
  slug: string
  description?: string
  feature: string
  value_type: string
  value: string
  quota_period?: string
  quota_unit?: string
  active: boolean
  sort_order: number
  created_at: string
  product?: Product
}

export interface LicenseAddon {
  id: string
  license_id: string
  addon_id: string
  enabled: boolean
  created_at: string
  addon?: Addon
}

export interface MeteredBilling {
  id: string
  license_id: string
  feature: string
  quantity: number
  period_key: string
  synced: boolean
  created_at: string
}

export interface Subscription {
  id: string
  license_id: string
  user_id?: string
  plan_id: string
  status: string
  payment_provider?: string
  external_id?: string
  current_period_start?: string
  current_period_end?: string
  cancel_at_period_end: boolean
  canceled_at?: string
  trial_start?: string
  trial_end?: string
  metadata?: Record<string, unknown>
  created_at: string
  updated_at: string
  license?: License
  plan?: Plan
}

export interface GrowthMetrics {
  net_growth_rate: number
  trial_conversion: number
  avg_license_age_days: number
  median_license_age_days: number
  total_trials: number
  converted_trials: number
  new_last_30d: number
  churned_last_30d: number
}

export interface RetentionData {
  period: string
  start_count: number
  end_count: number
  retention_pct: number
  churn_pct: number
}

export interface LicenseAgeDistribution {
  bucket: string
  count: number
}

export interface RecentActivity {
  id: string
  entity: string
  entity_id: string
  action: string
  actor_type: string
  created_at: string
}

export interface TopUser {
  email: string
  user_id: string
  license_count: number
  active_count: number
  total_usage: number
  activation_count: number
}

export interface AnalyticsInsights {
  growth: GrowthMetrics
  age_distribution: LicenseAgeDistribution[]
  top_users: TopUser[]
  retention: RetentionData[]
  recent_activity: RecentActivity[]
}

export interface Invoice {
  id: string
  number: string
  status: string
  amount_due: number
  amount_paid: number
  // What the invoice billed, after discounts and tax. Not what was paid:
  // amount_paid is 0 for one settled outside Stripe or from the balance.
  total: number
  currency: string
  created: number
  period_start: number
  period_end: number
  invoice_pdf: string
  hosted_url: string
}

export interface UserDetail {
  user: User
  licenses: License[]
  // Seats the customer holds on other people's licences, each with it.
  memberships: (Seat & { license?: License })[]
  subscriptions: Subscription[]
  total_usage: number
  active_seats: number
  activations: number
  recent_audit_logs: AuditLog[]
}

// Release: a logical version event for a product. Contains many platform
// artifacts. Lifecycle status is on the release; yank affects all artifacts.
export interface Release {
  id: string
  product_id: string
  version: string
  channel: "stable" | "beta" | "alpha" | "dev"
  name: string
  release_notes: string
  status: "draft" | "published" | "yanked"
  yanked_reason?: string
  published_at?: string
  yanked_at?: string
  created_at: string
  updated_at: string
  product?: Product
  artifacts?: ReleaseArtifact[]
}

// ReleaseArtifact: per-platform binary inside a Release.
export interface ReleaseArtifact {
  id: string
  release_id: string
  platform: string
  /** Name the file had when uploaded; empty for older artifacts. */
  filename?: string
  file_key: string
  file_size: number
  sha256: string
  ed25519_sig: string
  content_type: string
  signing_key_id?: string
  created_at: string
  updated_at: string
}

export interface ReleaseSigningKey {
  id: string
  product_id: string
  public_key: string
  active: boolean
  note?: string
  created_at: string
  rotated_at?: string
}

export const RELEASE_PLATFORMS = [
  "darwin-arm64",
  "darwin-x64",
  "windows-arm64",
  "windows-x64",
  "linux-arm64",
  "linux-x64",
  "linux-armhf",
] as const

export const RELEASE_CHANNELS = ["stable", "beta", "alpha", "dev"] as const
