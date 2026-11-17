# API contract

The rules an SDK can build on. Deliberately small: only what holds
across endpoints and is covered by a test. Anything specific to one
endpoint belongs in `openapi.yaml` or in the response itself, not here.

## The envelope

Every response under `/api/` is the same shape, success or failure,
apart from the raw-body routes listed below.

```json
{ "success": true,  "data": { ... } }
{ "success": false, "error": { "code": "LICENSE_NOT_FOUND", "message": "...", "details": {} } }
```

- `data` and `error` are mutually exclusive; exactly one is present.
- `error.code` is `SCREAMING_SNAKE_CASE`, and **a given code always
  arrives with the same HTTP status**, so a client can branch on the
  code and decide retries from the status. Enforced by
  `TestErrorCodeMapsToExactlyOneStatus`.
- `error.details` is optional and carried by only a few codes, for
  example `ACTIVATION_LIMIT`:
  `{"code":"ACTIVATION_LIMIT","message":"...","details":{"current":1,"max":1}}`.
  Treat it as absent unless a code documents it.
- `204 No Content` is the only success with no body.
- An unmatched path or a wrong method answers with the same envelope
  (`404 NOT_FOUND`), so a decoder needs one path, not a second one for
  "the URL was wrong".

### Raw-body routes

These are read by something that is not our SDK, so their bodies are
whatever that consumer requires. They are the only exceptions above.

| Route | Body |
|---|---|
| `GET /releases/:slug/feed.xml` | RSS/Sparkle XML |
| `GET /releases/:slug/feed.json` | a Velopack asset feed, `{"Assets": [...]}` |
| `GET /releases/:slug/velopack/[:platform/]:file` | the same feed at the URL Velopack builds, or a `302` to a package |
| `GET /releases/:slug/upgrade.json` | a bare Tauri manifest, or `204` when there is no update |
| `GET /admin/products/:id/signing-key/public.pem` | the PEM itself |
| `GET /admin/licenses/export` | a CSV file, or a bare JSON array with `format=json`, as an attachment |
| `POST /api/v1/webhook/stripe` | `{"received": …}`, for Stripe; **errors are not the envelope either** |
| `POST /api/v1/webhook/sepay` | SePay bank webhook; ack exactly `{"success": true}` |
| `POST /api/v1/webhook/sepay-ipn` | SePay hosted-gateway IPN; ack `200` with an empty body |
| `POST /api/v1/webhook/payos` | PayOS webhook; any `2xx`, empty body included |
| `GET /pay/:checkout_id` | a 302 to Stripe, plain text on error; outside `/api/` |

The read routes still answer *errors* with the envelope: ask
`feed.xml` for a product that does not exist and you get `404
NOT_FOUND` as JSON. So check the status first and decode the route's
own format only on success. The webhook write routes mostly do not:
their consumer is the provider, whose retry logic reads the status and
(for SePay) the ack body. A signature or key failure is the one
enveloped webhook error — `401 PAYMENT_WEBHOOK_UNAUTHORIZED`; every
other rejection is a bare `400`, `403`, `413`, `500` or `503` with no
JSON code. `GET /pay/:checkout_id` is outside `/api/` and is not the
envelope either.

## Naming and types

- `snake_case` throughout.
- A field ending in `_at` is an RFC3339 timestamp. It carries its UTC
  offset, which is not always `Z`: parse the offset rather than assume
  UTC.
- A chart bucket (`date`, `period`) is a plain `YYYY-MM-DD` date, not a
  timestamp. Do not hand both to the same parser.
- An empty collection is `[]`, never `null`.

## Pagination

```
GET /api/v1/admin/licenses?limit=50&offset=0

{ "success": true, "data": {
    "licenses": [ ... ],   // named after the resource, not a fixed "items"
    "total": 1234,         // rows matching the filter, not rows in this page
    "limit": 50,           // the limit actually applied
    "offset": 0
} }
```

**Lists are paginated, and the default is 50.** A client that reads the
collection and assumes it received everything will silently see only
the first page. This changed: these lists used to return every row.

- The cap is 200, and asking for more is **clamped, not refused**: ask
  for 1000 and you get 200, with `limit: 200` in the response. Compute
  page counts from the response, never from the request.
- `limit < 1`, a negative `offset`, or anything unparseable reads as
  "not asked for" and takes the default. `limit=0` does not mean
  "everything".
- Every paginated list orders by a unique tiebreaker (`, id`) and sorts
  `NULL` last, so tied rows cannot repeat or vanish across pages.

Paginated resource keys: `addons`, `api_keys`, `audit_logs`,
`deliveries`, `events`, `licenses`, `members`, `plans`, `products`,
`releases`, `seats`, `sessions`, `users`, `webhooks`.

Some endpoints answer with a bounded collection and no paging at all:
`/portal/licenses`, `/products/:slug/plans`, `/admin/system/migrations`
and the `/admin/analytics*` and `/admin/stats` aggregates. Do not wrap
those in a paging loop; it would spin on the same page.

### Sorting

Only `GET /admin/licenses` reads `?sort=` and `?order=`. Columns are an
allowlist: an unknown one is `400 INVALID_SORT` and the message lists
what is accepted, and `order` takes `asc` or `desc` else `400
INVALID_ORDER`.

Every other list has a fixed order and **ignores `sort` and `order`
silently**. Do not write one sorting helper and point it at every list.

## Authentication

- `/license/*` and the public feeds need no API key: the `license_key`
  in the body is the credential.
- `/admin/*` takes `Authorization: Bearer kg_live_…` or a session
  cookie. A key's scopes decide which routes it reaches: `admin` all
  of `/admin/*`, `licenses:write` the license routes, `releases:write`
  the release routes; a route outside them is `403
  INSUFFICIENT_SCOPE`. A `licenses:write` or `releases:write` key may be
  bound to one product: it gets `403 PRODUCT_SCOPE_MISMATCH` reaching
  for another product's resources, and its lists and the license export
  narrow to its product. An `admin` key cannot be bound.
- `/portal/*` is a customer session.
  `/portal/licenses/:license_key/activations` carries the key in the path;
  the server redacts that segment before logging and an SDK's own
  logging should too.

## Idempotency

Four endpoints accept an `Idempotency-Key` header:

```
POST /license/activate
POST /license/usage
POST /license/floating/checkout
POST /api/v1/payments/orders
```

A replay returns the first response and carries
`Idempotent-Replayed: true`. Keys live 24h. The middleware answers
before the handler runs:

| Code | Status |
|---|---|
| `INVALID_IDEMPOTENCY_KEY` | 400 |
| `BODY_TOO_LARGE` | 413 (over 256 KiB) |
| `IDEMPOTENCY_KEY_CONFLICT` | 422 (same key, different body) |
| `IDEMPOTENCY_IN_FLIGHT` | 409 (`Retry-After: 1`) |

Keys are stored per endpoint, not per caller: use random keys. 4xx
answers are replayed too, including a `429 LOCKED_OUT` on the request
that tripped the lockout; retry that with a new key once it has passed.

The other `POST /license/*` routes are idempotent by nature and ignore
the header. On the consumer payments route the header is optional at
the middleware level: without it every retry is a new order (the
checkout service has its own narrow reuse rule for an identical
pending purchase, but it is not a replay guarantee). With it, a retry
replays the first `201` and a *different body under the same key* is
`422 IDEMPOTENCY_KEY_CONFLICT` — not a 409, and not a second order.
Use one key per intended order, including across network retries.

## Payments

Phase 1a ships two Vietnamese rails behind one API: **SePay** (a static
VietQR by default; settlement comes from SePay's bank webhook) and
**PayOS** (a hosted payment link). Phase 1b adds coupons, invoices,
reports and the signed-in customer's payment portal on top of them. Amounts are integers in the
currency's minor unit: `amount_minor`, `subtotal_minor`, `tax_minor`,
`total_minor`. VND has exponent 0, so the integer is đồng. Phase 1a
sells VND only.

Provider configuration and the webhook routes need
`RELEASE_KEY_ENCRYPTION_KEY`. Without it no provider config can be
decrypted, the webhook routes are **not registered** (they answer the
ordinary `404 NOT_FOUND`), and every provider looks disabled to
checkout.

### Consumer API

Every route marked "key" needs an API key. `payments:write` reaches
create and cancel; `payments:read` (or `payments:write`) reaches the
GETs and invoice downloads. The quote is public but rate-limited. The
order routes share the `payments_api` rate-limit scope —
`RATE_LIMIT_API` per minute per IP; a throttled call is `429`. The quote
uses `payments_public`. A key bound to one product sees only that
product's orders and invoices.

| Method | Path | Auth | Scope |
|---|---|---|---|
| `POST` | `/api/v1/payments/orders` | key | `payments:write` |
| `GET` | `/api/v1/payments/orders` | key | `payments:read` or `payments:write` |
| `GET` | `/api/v1/payments/orders/:code` | key | `payments:read` or `payments:write` |
| `POST` | `/api/v1/payments/orders/:code/cancel` | key | `payments:write` |
| `POST` | `/api/v1/payments/quote` | none | — |
| `GET` | `/api/v1/payments/invoices/:id` | key | `payments:read` or `payments:write` |

`POST /api/v1/payments/orders` body (the shipped shape; fields outside
it are ignored, not rejected):

```json
{
  "plan_id": "3f2a9c10-6b7e-4d21-9c55-0a1b2c3d4e5f",
  "email": "buyer@example.com",
  "provider": "sepay",
  "quantity": 1,
  "buyer": {
    "name": "Nguyen Van A",
    "email": "buyer@example.com",
    "phone": "+84901234567",
    "tax_id": "0101234567",
    "address": "1 Le Loi, District 1, HCMC"
  },
  "metadata": { "workspace_id": "ws_123" }
}
```

- `plan_id`, `email` and `quantity` are required. `quantity` is an
  integer from 1 to 100; omitted, `0`, negative or over 100 is
  `400 INVALID_QUANTITY`.
- `email` is lower-cased and must contain `@`; anything else is not
  sellable (`422 PAYMENT_PRICE_NOT_CONFIGURED`, same as an unknown or
  inactive plan — the code means "this plan cannot be sold right now",
  not only "no price row").
- `provider` is optional. Omitted, the `payment_default_provider`
  setting is tried, then `sepay`, then `payos`; the first that has both
  an active price and a usable config wins. An explicit provider never
  falls back: an unknown, disabled or incomplete one is
  `503 PAYMENT_PROVIDER_DISABLED`, and a provider with no price for the
  plan is `422 PAYMENT_PRICE_NOT_CONFIGURED`.
- `buyer` in phase 1a accepts `name`, `email`, `phone`, `tax_id` and
  `address`. Phase 1b also accepts `company_name`, `tax_code` and
  `request_invoice`; `tax_code` is the new spelling of `tax_id` and
  both are accepted from old clients. `request_invoice=true` without a
  tax code is `422 PAYMENT_TAX_CODE_REQUIRED`. PayOS receives the
  fields it supports.
- Phase 1b order create adds `coupon_codes[]` and
  `ignore_invalid_coupons`. The caller cannot choose a `kind` in phase
  1b: the consumer API creates `new` orders, `renewal` orders are created
  by the billing engine, and prorated `upgrade` orders by
  `billing.Service.CreateUpgradeOrder` (no public HTTP surface yet; the
  portal upgrade route remains the Stripe-era one). The response adds
  `discounts[]` (`code`, `type`, `amount_minor`) and priced
  `subtotal_minor`/`discount_minor`/`tax_minor`/`total_minor`; an
  upgrade order also carries `credit_minor` (prorated unused time on the
  current plan).
- `metadata` is stored on the order and never sent to the provider.
- `Idempotency-Key` is honoured but optional (see **Idempotency**). A
  replay returns the first `201` with `Idempotent-Replayed: true`; the
  same key with a different body is `422 IDEMPOTENCY_KEY_CONFLICT`.

Response `201`:

```json
{ "success": true, "data": {
  "code": "KG7F3K2Q",
  "status": "pending",
  "kind": "new",
  "provider": "sepay",
  "product_id": "…",
  "plan_id": "…",
  "email": "buyer@example.com",
  "quantity": 1,
  "subtotal_minor": 1190000,
  "discount_minor": 0,
  "tax_minor": 108182,
  "total_minor": 1190000,
  "currency": "VND",
  "currency_exponent": 0,
  "expires_at": "2026-11-13T18:00:00+07:00",
  "payment": {
    "provider": "sepay",
    "provider_ref": "KG7F3K2Q",
    "qr_payload": "https://vietqr.app/img?acc=0978482700&bank=MB&amount=1190000&des=KG7F3K2Q&template=compact",
    "qr_image_url": "https://vietqr.app/img?acc=0978482700&bank=MB&amount=1190000&des=KG7F3K2Q&template=compact",
    "checkout_url": "",
    "va_number": "",
    "expires_at": "2026-11-13T18:00:00+07:00",
    "bank_account": {
      "bank_name": "MB Bank",
      "bank_bin": "970422",
      "account_no": "0978482700",
      "account_name": "CONG TY TNHH DSOFT"
    }
  }
}}
```

- For a SePay static QR, `qr_payload` is already the VietQR image URL,
  so `qr_image_url` repeats it. For PayOS, `checkout_url` is the hosted
  link and `qr_payload` is the EMVCo string; `qr_image_url` is a
  renderable URL when the adapter stored one, otherwise it repeats
  `qr_payload` (an EMVCo string, not an image URL).
- `bank_account` is absent unless the attempt stored one.
- There is no top-level `checkout_url`, `status_url`, `discounts[]` or
  `buyer` echo in phase 1a. Poll `GET /pay/:checkout_id/status` for the
  customer-facing status.
- `expires_at` comes from `payment_order_ttl_minutes` (default 1440), or
  PayOS's own link expiry when that is sooner.

`GET /api/v1/payments/orders/:code` returns the same object
(`200`, `data` = the order above). An unknown code is `404
PAYMENT_ORDER_NOT_FOUND`; so is another product's code for a
product-bound key. The order's latest attempt is included under
`payment` when one exists.

`GET /api/v1/payments/orders` is paginated (default 50, max 200) and
ordered newest first. The item key is literally `data`, inside `data`:

```json
{ "success": true, "data": {
  "data": [ { "code": "KG7F3K2Q", "status": "pending", "kind": "new",
              "provider": "sepay", "email": "buyer@example.com",
              "quantity": 1, "total_minor": 1190000, "currency": "VND",
              "currency_exponent": 0, "expires_at": "…", "created_at": "…" } ],
  "total": 1, "limit": 50, "offset": 0
}}
```

Filters: `status`, `provider`, `email` (exact, lower-cased),
`product_id`, `created_from`, `created_to` (RFC3339). A list row omits
`subtotal_minor`, `discount_minor`, `tax_minor` and `payment`; fetch
the order for those. A bad timestamp is `400 BAD_REQUEST`. A
product-bound key asking for another `product_id` is `403
PRODUCT_SCOPE_MISMATCH`.

`POST /api/v1/payments/orders/:code/cancel` cancels a still-pending
order and returns `200` with `data` = the order (no `payment` block).
`404 PAYMENT_ORDER_NOT_FOUND` for an unknown or out-of-scope code;
`409 PAYMENT_ORDER_NOT_PAYABLE` once the order is paid, expired,
canceled or refunded. Cancelling does not call the provider: SePay has
nothing to cancel and the PayOS link simply expires.

Order statuses: `pending`, `paid`, `expired`, `canceled`, `refunded`,
`partially_refunded`. Attempt statuses are separate: `pending`,
`succeeded`, `failed`, `expired` (`paid` is an order status and never
an attempt status).

#### Quote (phase 1b)

`POST /api/v1/payments/quote` is the public dry run: it prices a plan
with coupons and tax and creates **nothing** — no order, no attempt, no
coupon redemption. It shares the `payments_public` bucket.

```json
{
  "plan_id": "3f2a9c10-6b7e-4d21-9c55-0a1b2c3d4e5f",
  "quantity": 1,
  "email": "buyer@example.com",
  "coupon_codes": ["TET2026"],
  "ignore_invalid_coupons": false
}
```

`data` is the priced total: `subtotal_minor`, `discount_minor`,
`tax_minor`, `total_minor`, `currency`, `currency_exponent` and
`discounts[]` (`code`, `type`, `amount_minor`). A rejected coupon is
the matching `PAYMENT_COUPON_*` code; with `ignore_invalid_coupons`
the code is dropped instead. The response also carries `currency`,
`currency_exponent`, `billing_interval`, `trial_days` and `discounts[]`
(`code`, `type`, `amount_minor`). It is mounted with the shared
`payments_public` rate-limit bucket.

#### Consumer invoices (phase 1b)

`GET /api/v1/payments/invoices/:id` returns the invoice JSON; adding
`?format=pdf` streams the stored PDF as a raw body
(`application/pdf`, `Content-Disposition: attachment`). Ownership is
the API key's product: an invoice whose order belongs to another
product is `404 INVOICE_NOT_FOUND`, the same answer as an unknown id.
The JSON carries `id`, `number`, `order_id`, `license_id`, `status`
(`draft|issued|pending_issue|void`), the frozen `buyer` snapshot,
`items[]` (`description`, `plan_id`, `quantity`, `unit_amount_minor`,
`amount_minor`, `tax_rate_bp`), `subtotal_minor`, `discount_minor`,
`tax_minor`, `total_minor`, `currency`, `currency_exponent`,
`issued_at` and `created_at`. Issue/void rules are admin actions; a
consumer or API-key read does not change the invoice.

### Portal (session)

`/api/v1/portal/*` is the signed-in customer's surface. It is **not**
an API-key surface: `SessionAuth` authenticates the browser session
and puts `user_id` and `email` on the request. Every route answers
`401 UNAUTHORIZED` without that session. A customer only ever sees
their own rows: an order or invoice belonging to somebody else is
answered exactly like an unknown one (`404`), never `403`.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/portal/payments/orders` | the customer's orders, newest first |
| `GET` | `/api/v1/portal/payments/orders/:code` | one order and its current instruction |
| `POST` | `/api/v1/portal/payments/orders/:code/pay` | re-issue the instruction for a pending order |
| `POST` | `/api/v1/portal/payments/orders/:code/cancel` | cancel a pending order |
| `GET` | `/api/v1/portal/invoices` | the customer's invoices |
| `GET` | `/api/v1/portal/invoices/:id` | invoice JSON, or `?format=pdf` |
| `GET` | `/api/v1/portal/billing-profile` | saved buyer/tax block |
| `PUT` | `/api/v1/portal/billing-profile` | validate and upsert it |
| `POST` | `/api/v1/portal/coupons/validate` | phase 1b coupon preview for the signed-in buyer |

**Ownership.** An order belongs to the caller when the order email or
the buyer snapshot's `email` equals the session email
(case-insensitive). Phase 1a has no `external_customer_id` column on
`payment_orders`; the buyer JSON is the second identity. An invoice
belongs to the caller when its `buyer.email` matches, or when the
order it was issued for belongs to them. A foreign code/id is
`404 PAYMENT_ORDER_NOT_FOUND` / `404 INVOICE_NOT_FOUND`.

**Orders.** The list is paginated (default 50, max 200) and returns
`data.orders[]` — the same compact row the consumer list uses — plus
`total`, `limit` and `offset`. The detail returns the consumer order
projection (including `subtotal_minor`, `discount_minor`, `tax_minor`,
`payment`) and no buyer blob or metadata. `payment` is absent when no
attempt exists. `email` in a row is the order's email, which for a
buyer-matched order may be the purchasing contact.

`POST .../pay` refreshes the instruction for a **pending, unexpired**
order only. It never re-prices and never creates a second order: the
amount is the order's stored total. A paid, expired, canceled or
refunded order is `409 PAYMENT_ORDER_NOT_PAYABLE` before any provider
call. Provider failures reuse the vocabulary: `503
PAYMENT_PROVIDER_DISABLED` and `502 PAYMENT_PROVIDER_ERROR`.
`POST .../cancel` cancels a pending order (no provider call; SePay has
nothing to cancel and the PayOS link is left to expire) and answers
the canceled order. A second cancel or a paid order is `409
PAYMENT_ORDER_NOT_PAYABLE`. Cancelling has no licence effect.

**Invoices.** The list returns `data.invoices[]` with `id`, `number`,
`order_id`, `status`, the money fields, `currency`,
`currency_exponent`, `created_at` and `issued_at` when set — not the
internal `pdf_path` or e-invoice refs. The detail adds the frozen
`buyer` and `items[]`. `?format=pdf` is a raw PDF body with
`Cache-Control: private, no-store`; a foreign or unknown id is `404`
before the PDF is fetched, and any other `format` is `400
BAD_REQUEST`.

**Billing profile.** The row is one per user. `GET` answers
`{"buyer": {}}` when nothing is saved, otherwise the stored block.
`PUT` accepts either the flat object or `{"buyer": {...}}` with
`name` (required, ≤ 200 characters), `company_name`, `tax_code`,
`address`, `email`, `phone` and `request_invoice` (bool). Fields are
trimmed; a missing/blank name or an over-long field is `400
BAD_REQUEST`; `request_invoice=true` with an empty `tax_code` is `422
PAYMENT_TAX_CODE_REQUIRED`. A successful PUT upserts and writes an
audit row (`entity=customer_billing_profile`, actor `portal`); a
rejected PUT writes nothing. This profile is what "request a VAT
invoice" reads at checkout; it does not change an existing invoice.

### Public status and pay page


| Method | Path | Auth |
|---|---|---|
| `GET` | `/pay/:checkout_id/status` | none |
| `GET` | `/api/v1/pay/:checkout_id/status` | none — alias of the same handler |
| `GET` | `/pay/:checkout_id` | none |

Both status spellings share one rate-limit scope, `payments_public`
(`RATE_LIMIT_API` per minute); polling one does not buy a second
budget by switching spellings. The order code is the only capability:
there is no session, no API key and no buyer identity in the response.
The answer is `200` with `Cache-Control: no-store`. A malformed or
unknown code is `404 NOT_FOUND` (the generic code, not
`PAYMENT_ORDER_NOT_FOUND`).

```json
{ "success": true, "data": {
  "code": "KG7F3K2Q",
  "status": "pending",
  "amount_minor": 1190000,
  "currency": "VND",
  "currency_exponent": 0,
  "expires_at": "2026-11-13T18:00:00+07:00",
  "description": "KG7F3K2Q",
  "qr_image_url": "https://vietqr.app/img?…",
  "checkout_url": "",
  "bank": { "bank_name": "MB Bank", "bank_bin": "970422",
            "account_no": "0978482700", "account_name": "CONG TY TNHH DSOFT" }
}}
```

`description` is always present: the order code, or the memo the
attempt stored when the provider returned one. `qr_image_url` and
`checkout_url` are absent only when there is no attempt at all; when an
attempt exists they may be empty strings. `bank` is present only when
the attempt stored an account. This payload has **no** `paid_at`,
`qr_payload` or `va_number`, and no licence or buyer data.

`GET /pay/:checkout_id` is the unified checkout entry. A KG order code
answers `302` to the SPA page `/checkout/pay/<code>` (which polls the
status route); anything else is a legacy Stripe checkout id and keeps
that handler. It is outside `/api/` and is not the envelope.

### Webhooks (raw body)

The three webhook routes read the raw bytes themselves: a signature
covers the provider's bytes, not a re-marshalled struct. They never use
the consumer/admin auth middleware. A signature or key failure is
`401 PAYMENT_WEBHOOK_UNAUTHORIZED` in the standard envelope; every
other failure is a bare status. Each route has its own rate-limit
scope at 120 requests/minute per IP and caps the body at 256 KiB
(`413` when over).

| Route | Auth | Ack on success |
|---|---|---|
| `POST /api/v1/webhook/sepay` | config's `webhook_auth_mode` | `200` with exactly `{"success": true}` |
| `POST /api/v1/webhook/sepay-ipn` | `X-Secret-Key: <ipn_secret>` (constant-time) | `200`, empty body |
| `POST /api/v1/webhook/payos` | `signature` inside the JSON body (checksum key) | any `2xx`; this server sends `200 {"success": true}` |

SePay bank webhook, in the mode saved on the config row:

- `hmac` — `X-SePay-Timestamp` (Unix seconds) and
  `X-SePay-Signature: sha256=<hex>` where the HMAC-SHA256 key is the
  config's `webhook_secret` and the message is
  `<timestamp>.<raw body>`. A skew over 300 seconds is rejected.
- `apikey` — `Authorization: Apikey <api_token>` (the same token the
  REST client uses).
- `none` — unsigned; accepted only by a `test` row. A `live` row with
  `webhook_auth_mode=none` is refused when it is saved
  (`400 PAYMENT_PROVIDER_CONFIG_INVALID`) and never verifies a
  delivery.

SePay IPN is the hosted-gateway notification. It counts as paid only
when `notification_type=ORDER_PAID`, `order.order_status=CAPTURED` and
`transaction.transaction_status=APPROVED`; `TRANSACTION_VOID` produces
`payment.failed`; other statuses are recorded but change no licence.
Phase 1a does **not** create hosted-gateway checkouts (see **Phase-1a
drift as shipped**), but the IPN endpoint, `ipn_secret` and settlement
path exist for a gateway order created elsewhere.

PayOS signs `data` with the config's `checksum_key`; the route verifies
it before any database write. A missing or wrong signature is `401
PAYMENT_WEBHOOK_UNAUTHORIZED`.

Common answers (only the 401 is enveloped):

| Status | When |
|---|---|
| `401` | signature/key missing, wrong or stale — envelope code `PAYMENT_WEBHOOK_UNAUTHORIZED` |
| `400` | the body is not JSON, or an event has no usable id |
| `403` | the account that verified the delivery is not the order's product's |
| `413` | body over 256 KiB |
| `503` | no enabled, decryptable provider config for that order |
| `500` | settlement failed transiently; the provider should retry |
| `200` | duplicate, unknown order, out transfer, or settled — all acked, none grants twice |

Deduplication is `(provider, event id)` for `payment_events` and
`(provider, provider_ref)` for `payment_transactions`; a redelivery is
acked and never grants a second licence. An unknown order or a `out`
transfer is stored for the unmatched queue and acked. An amount or
currency that does not match the order records an unmatched
transaction and is also acked (the money is visible to an operator; the
licence is not granted).

Retries are the provider's business: SePay retries a delivery it does
not see acked with the exact body for roughly half an hour; PayOS's
retry policy is undocumented, so treat every pending order as possibly
un-notified. The reconciliation worker is the backstop (below). There
is **no source-IP allowlist** in phase 1a: the HMAC/apikey/checksum is
the gate. The spec's SePay CIDR allowlist is deferred, and
`allowed_cidrs` is not part of the config shape.

### Outbound events

Webhook subscriptions (`/admin/webhooks`) receive the install's existing
`license.*`, `quota.*`, `seat.*`, `plan.*` and `release.*` events plus
the payment and billing events below. Every delivery is
`{"event": "...", "timestamp": "...", "data": {...}}` and is signed,
retried, claimed and replayable by the existing webhook service; this
section only fixes the `data` shape. Money fields are integer minor
units.

| Event | `data` |
|---|---|
| `order.created` | `order` (code, kind, provider, status, total, currency, expires_at, product_id, plan_id) |
| `order.paid` | `order` + `license` (id, key_prefix, plan_id, valid_until) + `payment` (provider, provider_ref, paid_at) |
| `order.expired` | `order` |
| `order.canceled` | `order` |
| `payment.succeeded` | `order` + `payment` (provider, amount, currency, reference, occurred_at) |
| `payment.failed` | `order` + `payment` (failure_reason) |
| `payment.refunded` | `order` + `refund` (amount, reason, method) |
| `invoice.issued` | `invoice` (id, number, total, currency, pdf_url?) |
| `invoice.voided` | `invoice` (id, number) |
| `license.renewal_due` | `license` (id, key_prefix, valid_until) + `order` (code, total, expires_at) |

`order.expired` and `order.canceled` fire at the transition, once: a
sweep that finds no row, a cancel that loses the compare-and-set, or a
retry of an already-terminal order emits nothing. `payment.refunded`
fires once per refund row recorded, with the refund's amount in minor
units and its `method` (`manual` in phase 1). Settlement writes these
inside the same transition as the status change; there is still no
outbox, so a consumer that missed a delivery reconciles against the
order/invoice APIs, not a replay log.

**Known phase-1b gap.** The settlement path records a provider-reported
expiry (PayOS/SePay telling us the order lapsed) as `pending -> expired`
without emitting `order.expired`; only the reconciliation sweep emits
it. A consumer that missed an expiry reconciles against the order API.

### Admin: payments

Every route below is under the install-wide admin group:
`SessionOrAPIKey` + `RequireScope(admin)`. A customer session is not
enough; an admin dashboard session or an API key with the `admin` scope
is. Responses use the envelope and the standard pagination shape.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/admin/payments/orders` | order ledger |
| `GET` | `/api/v1/admin/payments/orders/:code` | order + attempts + transactions + events + refunds |
| `POST` | `/api/v1/admin/payments/orders/:code/mark-paid` | manual settlement, test rows only |
| `POST` | `/api/v1/admin/payments/orders/:code/cancel` | cancel a pending order |
| `POST` | `/api/v1/admin/payments/orders/:code/refund` | record a manual refund |
| `GET` | `/api/v1/admin/payments/transactions` | transaction ledger |
| `GET` | `/api/v1/admin/payments/unmatched` | the operator queue |
| `POST` | `/api/v1/admin/payments/transactions/:id/match` | link a transfer to an order |
| `POST` | `/api/v1/admin/payments/transactions/:id/ignore` | ignore a transfer |
| `GET` | `/api/v1/admin/payments/health` | per-rail health |
| `GET` | `/api/v1/admin/payments/providers` | provider config rows |
| `PUT` | `/api/v1/admin/payments/providers` | create/update one config row |
| `DELETE` | `/api/v1/admin/payments/providers/:id/secrets` | blank selected secrets |
| `POST` | `/api/v1/admin/payments/providers/:provider/test` | credential check |
| `POST` | `/api/v1/admin/payments/providers/payos/confirm-webhook` | register this install's PayOS URL |

`GET /admin/payments/orders` filters `status`, `kind`, `product_id`,
`email`, `provider` and is paginated. `status` outside
`pending|paid|expired|canceled|refunded|partially_refunded` is `400
INVALID_STATUS`. Rows are the ledger projection (`id`, `numeric_code`,
`code`, `status`, `kind`, `provider`, `product_id`, `plan_id`,
`price_id`, `license_id`, `email`, `quantity`, `subtotal_minor`,
`discount_minor`, `tax_minor`, `total_minor`, `currency`,
`currency_exponent`, `expires_at`, `paid_at`, `period_start`,
`period_end`, `last_settlement_error`, `created_at`, `updated_at`);
`buyer` and `metadata` appear only on the detail route.

`GET /admin/payments/orders/:code` answers `data` with keys `order`,
`attempts`, `transactions`, `events`, `refunds`. The detail order adds
`buyer` and `metadata`; transactions include their raw provider
payloads there (PII and provider noise — admin only).

`POST …/mark-paid` requires `{"reason": "..."}` and a *pending* order;
the answer is `data: {order}`. It is a rehearsal facility: it refuses
(`409 PAYMENT_MANUAL_SETTLEMENT_FORBIDDEN`) when an enabled live config
row covers the order's rail and product, and refuses (`422
PAYMENT_MANUAL_SETTLEMENT_UNVERIFIABLE`) when the row is missing,
disabled or unusable, or when the server has no config service at all
(no `RELEASE_KEY_ENCRYPTION_KEY`). Only a resolvable `test`
configuration lets it grant a licence without money moving. Any other
order state is `409 PAYMENT_ORDER_NOT_PAYABLE`. It settles through the
same service as a webhook, so the grant is idempotent.

`POST …/cancel` has no body and answers `data: {order}`; it is `404
PAYMENT_ORDER_NOT_FOUND` or `409 PAYMENT_ORDER_NOT_PAYABLE` (paid orders
are never cancelled).

`POST …/refund` body:
`{"amount_minor": 119000, "reason": "...", "method": "manual", "provider_ref": ""}`.
`method` defaults to `manual`; anything else is `422
PAYMENT_REFUND_UNSUPPORTED` because phase 1a performs no provider
refund. `amount_minor` must be positive (`422 PAYMENT_REFUND_INVALID`),
the order must be paid (`409 PAYMENT_ORDER_NOT_REFUNDABLE`) and the
sum must not exceed the order (`422 PAYMENT_REFUND_EXCEEDS_ORDER`).
The response is `data: {refund, order}`. **A refund records the money
and changes the order to `refunded`/`partially_refunded`; it does not
revoke or shorten the licence.** Revoking is a separate licence action.

`GET /admin/payments/transactions` filters `provider`, `direction`
(`in`/`out`), `status` (`matched|unmatched|ignored`, else `400
INVALID_STATUS`), `product_id`, `order_id`; paginated. Raw payloads
are omitted from list rows.

`GET /admin/payments/unmatched` is the queue: `limit` only (no
offset), default 50, max 200. It returns
`data: {transactions: [...], count, limit}` with raw payloads included.
An amount-only match from the reconciler is stored here with an
`ignore_reason` naming the suggested order — **a suggestion, not a
settlement**; only an operator match turns it into a licence.

`POST …/transactions/:id/match` body `{"order_code": "KG7F3K2Q"}`. It
requires an unmatched (`409 PAYMENT_TRANSACTION_NOT_UNMATCHED`),
incoming (`422 PAYMENT_TRANSACTION_NOT_INCOMING`) transfer, an order
of the same provider (`409 PAYMENT_TRANSACTION_PROVIDER_MISMATCH`), the
exact amount and currency (`422 PAYMENT_TRANSACTION_MISMATCH`) and a
pending order (`409 PAYMENT_ORDER_NOT_PAYABLE`); unknown order `404
PAYMENT_ORDER_NOT_FOUND`. Success is `200 {transaction, order}`.

`POST …/transactions/:id/ignore` requires `{"reason": "..."}`; only an
unmatched transfer can be ignored (`409
PAYMENT_TRANSACTION_NOT_UNMATCHED`); `404
PAYMENT_TRANSACTION_NOT_FOUND` otherwise. It returns the row.

`GET /admin/payments/health` returns
`data.providers[]` with `provider`, `enabled`, `mode` (only when every
row agrees), `last_event_at`, `unmatched_open`, `pending_orders`,
`last_error`, `last_reconciled_at`, `cursor_lag_seconds`. Both rails
appear even before a config row exists.

Provider configuration:

- `GET /admin/payments/providers` returns `data.providers[]` with `id`,
  `provider`, `product_id` (absent for the install default), `mode`,
  `enabled`, `config` and the booleans `credentials_set` and
  `secret_set`. A secret is never returned.
- `PUT /admin/payments/providers` takes
  `{"provider": "sepay"|"payos", "product_id": "", "mode": "test"|"live", "enabled": true, "config": {...}, "credentials": {...}, "webhook_secret": "..."}`.
  Omitted `enabled` keeps the stored value; omitted `config`,
  `credentials` or an empty `webhook_secret` keeps the stored value.
  Saving a row that is enabled but incomplete is `400
  PAYMENT_PROVIDER_CONFIG_INVALID`; so is a `live` SePay row with
  `webhook_auth_mode=none`.
- SePay `config`: `bank_name`, `bank_bin`, `bank_code`, `account_no`,
  `account_name`, `webhook_auth_mode` (`hmac|apikey|none`). SePay
  `credentials`: `api_token`, `ipn_secret`.
- PayOS `config`: `client_id`. PayOS `credentials`: `api_key`,
  `checksum_key`. PayOS signs webhooks with `checksum_key`; the
  `webhook_secret` field is accepted but may be empty and is not used
  by the PayOS adapter. SePay's `webhook_secret` is required to enable
  a row.
- `DELETE /admin/payments/providers/:id/secrets` body
  `{"fields": ["credentials"|"webhook_secret"]}`; `204` on success.
  `:id` is the config row id, not the provider name.
- `POST /admin/payments/providers/:provider/test` calls the adapter's
  credential check. A failed check is `200` with `ok: false` and a
  bounded `error`; only a missing/unusable row is `4xx`/`503`. The
  `product_id` query selects a product override.
- `POST /admin/payments/providers/payos/confirm-webhook` has no body.
  It builds `{payment_webhook_base_url or BASE_URL}/api/v1/webhook/payos`
  and asks PayOS to register it; PayOS probes the URL immediately, so
  it must be public HTTPS. Success is
  `{provider, webhook_url, confirmed: true}`. The optional `product_id`
  query scopes the credentials to that product's PayOS row (default: the
  install-wide row); a `product_id` that names no product is `404
  NOT_FOUND`. Missing base URL is `400 PAYMENT_WEBHOOK_URL_UNSET`; a
  PayOS refusal is `502 PAYMENT_PROVIDER_ERROR`.

Plan prices:

| Method | Path |
|---|---|
| `GET` | `/api/v1/admin/plans/:id/prices` |
| `PUT` | `/api/v1/admin/plans/:id/prices` |
| `DELETE` | `/api/v1/admin/plans/:id/prices/:price_id` |

`GET` returns `data.prices[]` (active and retired rows, newest first,
no pagination). `PUT` body:
`{"provider": "sepay", "currency": "VND", "amount_minor": 1190000, "currency_exponent": 0, "billing_interval": "month", "external_price_id": ""}`.
`amount_minor` is required (`400 INVALID_AMOUNT` when omitted; a
non-positive or otherwise invalid price is `400 BAD_REQUEST`); an
explicit `currency_exponent` that disagrees with `currency` is `400
INVALID_CURRENCY_EXPONENT`; a lost slot race is `409 PRICE_SLOT_CONFLICT`;
unknown plan `404 PLAN_NOT_FOUND`. The upsert replaces the active row
in its `(provider, currency, billing_interval)` slot and retires the
old one. `DELETE` deactivates one row and answers `204`; an unknown row
or one that does not belong to the named plan is `404 PRICE_NOT_FOUND`.

### Admin: coupons, invoices and reports (phase 1b)

These routes sit on the same install-wide admin group as
`/admin/payments` (`SessionOrAPIKey` + `RequireScope(admin)`); every
write is audited. The shapes below are the merged handlers' shapes.

**Coupons and redemptions**

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/admin/coupons` | list; filters `q`, `active`, `stack_group`, `scope_type`; paginated |
| `POST` | `/api/v1/admin/coupons` | create, with inline `scopes[]` |
| `POST` | `/api/v1/admin/coupons/preview` | dry-run price for a plan + codes; creates nothing |
| `GET` | `/api/v1/admin/coupons/:id` | detail |
| `PUT` | `/api/v1/admin/coupons/:id` | update; scopes replaced atomically |
| `DELETE` | `/api/v1/admin/coupons/:id` | delete only with no redemptions; otherwise archive (`active=false`) |
| `GET` | `/api/v1/admin/coupons/:id/redemptions` | one coupon's redemption history |
| `GET` | `/api/v1/admin/payments/redemptions` | global history; filters `coupon`, `email`, `from`, `to`; `?format=csv` |

A coupon carries `code`, `description`, `type` (`percent|fixed|trial`),
`value` (basis points for `percent`, minor units for `fixed`),
`trial_days`, `currency`, `active`, `valid_from`, `valid_to`,
`max_redemptions` (0 = unlimited), `redeemed_count`,
`per_customer_limit`, `stack_group` (`general|campaign|private`),
`stackable`, `priority`, `assigned_email` and `scopes[]`
(`{scope_type, scope_id}` with `scope_type` one of product/plan; an
empty set covers every product).
Delete with redemptions is `409`; the code is module-owned
(**verify**). Redeeming a coupon is recorded once at settlement; the
redemption row carries `coupon_id`, `order_id`, `email`, `amount_minor`
and `created_at`. `?format=csv` is a raw `text/csv` body with a header
row and the filtered rows; the filename is `redemptions-<date>.csv`.

**Invoices**

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/admin/payments/invoices` | list; filters `status`, `number`, `license_id`, `order_id`, `product_id`, `from`, `to` |
| `GET` | `/api/v1/admin/payments/invoices/:id` | one invoice with buyer/items |
| `GET` | `/api/v1/admin/payments/invoices/:id/pdf` | stream the stored PDF |
| `POST` | `/api/v1/admin/payments/invoices/:id/issue` | issue a draft / retry e-invoice |
| `POST` | `/api/v1/admin/payments/invoices/:id/void` | void an issued invoice |

Invoice statuses are `draft`, `issued`, `pending_issue` and `void`.
`issue` on an already issued or void invoice is `409
INVOICE_ALREADY_ISSUED`; `void` on a draft is the same conflict. When
an e-invoice provider is configured but not wired, issuing moves the
row to `pending_issue` and puts it on the operator's manual queue; a
provider failure is `502 EINVOICE_PROVIDER_ERROR`. Issuing and voiding
emit `invoice.issued` / `invoice.voided` once. A voided invoice keeps
its number; numbers are never reused. PDFs are stored by the invoice
service and served with `Cache-Control: private, no-store`; admin PDF
download is admin-authenticated. **Verify after merge:** module C may
name the detail/PDF routes differently; the list/issue/void paths above
are the spec's.

**Reports** (all `GET`, all accept `?format=csv`)

| Path | Groups by |
|---|---|
| `/api/v1/admin/payments/reports/revenue` | `provider`, `plan`, `product`, `period` |
| `/api/v1/admin/payments/reports/reconciliation` | day; settled vs provider-reported, with drift |
| `/api/v1/admin/payments/reports/vat` | period; output VAT contained in paid totals |

JSON rows use integer minor units and the same
`currency`/`currency_exponent` pair as the ledger. CSV responses are a
raw `text/csv` body (`Content-Disposition: attachment`), with a header
row and one row per group. Columns:

- revenue: `period, provider, plan_id, plan_name, product_id,
  product_name, currency, currency_exponent, orders, subtotal_minor,
  discount_minor, tax_minor, total_minor`
- reconciliation: `day, provider, currency, orders_count,
  transactions_count, orders_minor, transactions_minor, drift_minor`
- VAT: `period, currency, currency_exponent, tax_rate_bp, orders,
  taxable_base_minor, tax_minor, total_minor`

Revenue includes paid and partially refunded orders net of recorded
refunds; reconciliation reports unmatched/amount-mismatch counts and the
drift between the ledger and provider totals; VAT reports the tax
component, not a second tax charge.

**Settings used by phase 1b** (admin settings API; values are strings)

| Key | Meaning |
|---|---|
| `coupon_max_stack` | max amount coupons per order (default 2) |
| — | the public quote shares the install's API rate-limit bucket (`RATE_LIMIT_API`); the spec's separate `payment_quote_rate_limit` key is not read in phase 1b |
| `invoice_seller_name`, `invoice_seller_tax_code`, `invoice_seller_address`, `invoice_seller_email` | seller block printed on invoices |
| `invoice_note` | footer note printed on invoices |
| `einvoice_provider` | `""` (PDF only) or a configured e-invoice provider key |

Unset or blank invoice-seller values render as empty lines, not
`<nil>`; `einvoice_provider` blank is the normal PDF-only mode. The
renewal/dunning worker's cadence is not a settings key; its reminder
steps (`renewal_reminder_7/3/1`, `renewal_due`, `renewal_grace_1/3/7`,
`renewal_suspended`) are dispatched by the billing worker and use the
plan's `grace_days` and the licence's `valid_until`.

### Payment error vocabulary

Every code below arrives with exactly this status; the contract test
`TestPaymentErrorVocabulary` pins the handler call sites and
`TestErrorCodeMapsToExactlyOneStatus` pins global uniqueness. Shared
envelope codes (`BAD_REQUEST` 400, `NOT_FOUND` 404, `INTERNAL_ERROR`
500, `PRODUCT_SCOPE_MISMATCH` 403) keep their global statuses.

| Code | Status |
|---|---|
| `INVALID_QUANTITY` | 400 |
| `INVALID_AMOUNT` | 400 |
| `INVALID_CURRENCY_EXPONENT` | 400 |
| `INVALID_STATUS` | 400 |
| `PAYMENT_PROVIDER_UNKNOWN` | 400 |
| `PAYMENT_PROVIDER_CONFIG_INVALID` | 400 |
| `PAYMENT_WEBHOOK_URL_UNSET` | 400 |
| `PAYMENT_WEBHOOK_UNAUTHORIZED` | 401 — signature/key failure on a webhook route (enveloped) |
| `PAYMENT_ORDER_NOT_FOUND` | 404 |
| `PAYMENT_PROVIDER_NOT_FOUND` | 404 |
| `PAYMENT_TRANSACTION_NOT_FOUND` | 404 |
| `PLAN_NOT_FOUND` | 404 |
| `PRICE_NOT_FOUND` | 404 |
| `PAYMENT_ORDER_NOT_PAYABLE` | 409 |
| `PAYMENT_ORDER_NOT_REFUNDABLE` | 409 |
| `PAYMENT_TRANSACTION_NOT_UNMATCHED` | 409 |
| `PAYMENT_TRANSACTION_PROVIDER_MISMATCH` | 409 |
| `PAYMENT_MANUAL_SETTLEMENT_FORBIDDEN` | 409 |
| `PRICE_SLOT_CONFLICT` | 409 |
| `PAYMENT_PRICE_NOT_CONFIGURED` | 422 |
| `PAYMENT_TRANSACTION_MISMATCH` | 422 |
| `PAYMENT_TRANSACTION_NOT_INCOMING` | 422 |
| `PAYMENT_MANUAL_SETTLEMENT_UNVERIFIABLE` | 422 |
| `PAYMENT_REFUND_INVALID` | 422 |
| `PAYMENT_REFUND_UNSUPPORTED` | 422 |
| `PAYMENT_REFUND_EXCEEDS_ORDER` | 422 |
| `PAYMENT_PROVIDER_ERROR` | 502 |
| `PAYMENT_PROVIDER_DISABLED` | 503 |

Phase 1b adds the following. Some are documented ahead of their
handler and are not emitted until the owning module merges; the module
that writes one pins it in `paymentVocabulary`. The two this module
emits — `PAYMENT_TAX_CODE_REQUIRED` and `INVOICE_NOT_FOUND` — are
already pinned. Treat the table as the target vocabulary for the merge.

| Code | Status |
|---|---|
| `PAYMENT_SIGNATURE_INVALID` | 400 |
| `PAYMENT_TAX_CODE_REQUIRED` | 422 |
| `PAYMENT_AMOUNT_MISMATCH` | 422 |
| `PAYMENT_CURRENCY_UNSUPPORTED` | 422 |
| `PAYMENT_COUPON_INVALID` | 422 |
| `PAYMENT_COUPON_STACK_LIMIT` | 422 |
| `PAYMENT_COUPON_GROUP_CONFLICT` | 422 |
| `PAYMENT_COUPON_NOT_STACKABLE` | 422 |
| `PAYMENT_COUPON_TRIAL_MIXED` | 422 |
| `PAYMENT_COUPON_ASSIGNED` | 422 |
| `PAYMENT_COUPON_EXHAUSTED` | 409 |
| `INVOICE_NOT_FOUND` | 404 |
| `INVOICE_ALREADY_ISSUED` | 409 |
| `EINVOICE_PROVIDER_ERROR` | 502 |

`PAYMENT_PRICE_NOT_CONFIGURED` remains `422` even though the design
spec's table says `409`; the shipped code and this contract win.

### As shipped: phase 1a and 1b

The design spec describes a larger phase 1
([`2026-11-12-sepay-payos-integration-design.md`](superpowers/specs/2026-11-12-sepay-payos-integration-design.md)).
These are the places the shipped code is intentionally narrower or
different; code and this contract are the source of truth.

- **No outbox.** Payment and billing events are dispatched synchronously
  from the transition that writes them, through the existing webhook
  service. A consumer that missed one reconciles against the order,
  invoice or status APIs; `payment_events` records raw provider
  deliveries for operators, not a replay log.
- **`payment.succeeded.amount` is an integer in minor units.** VND has
  exponent 0, so it is đồng — not a decimal string and not a float.
  `payment.failure_reason` is derived server-side from the normalised
  status (`failed`/`partial`/`voided`), not copied from the provider's
  free-text `desc`.
- **Coupons, invoices, reports and the portal are phase 1b.** The
  consumer quote, consumer invoice read, portal orders/invoices/billing
  profile, admin coupon and redemption surfaces, admin invoice issue/void
  and the three reports are registered by the phase-1b wiring; see the
  sections above. `discount_minor` is 0 only when no coupon applied.
  The shapes above match the merged handlers: quote returns
  `subtotal_minor`/`discount_minor`/`tax_minor`/`total_minor`,
  `currency`/`currency_exponent`, `billing_interval`, `trial_days` and
  `discounts[]`; report CSV columns are listed in the reports section.
- **The billing worker and dunning ladder are phase 1b.** Renewal
  reminders and `license.renewal_due` are dispatched by that worker on
  the plan's `grace_days` and the licence's `valid_until`; the
  `invoice_*` and `einvoice_provider` settings keys are listed above.
  Its cadence is one pass per hour; reminder rungs are 7/3/1 days before
  the period end, dunning rungs 1/3/7 days after it, and a suspended
  licence gets one suspension notice.
- **SePay CIDR allowlist deferred.** There is no source-IP check in
  phase 1a/1b; HMAC/apikey (and PayOS's checksum) is the gate.
- **PayOS confirm-webhook is explicit, and PayOS has no sandbox.** The
  admin action registers the URL only after the install is public
  HTTPS; PayOS probes immediately. Test with a real small payment, or
  mark a `test`-mode order paid manually.
- **SePay hosted-gateway checkout creation is deferred.** Phase 1a
  creates only the static VietQR and phase 1b does not change that:
  `CreateAttempt` has no redirect/form path, and DSoft never redirects
  a buyer to SePay's gateway. The IPN route, `X-Secret-Key`
  verification and settlement are live, so an existing external gateway
  integration can still notify `/api/v1/webhook/sepay-ipn`.
- **Admin actor attribution is NULL for API-key admins.**
  `payment_refunds.created_by` and `payment_transactions.matched_by`
  reference `users(id)`, so an `apikey:<id>` principal is stored as
  SQL NULL; the audit row keeps the full principal. A dashboard session
  stores the real user id.
- **Refunds are manual-only and do not revoke licences.** The operator
  returns the money outside the system and records the row; `method`
  other than `manual` is refused. The order becomes
  `refunded`/`partially_refunded`, the licence is untouched, and one
  `payment.refunded` event is emitted per recorded refund.
- **Public status field names are flat.** `amount_minor`, not
  `amount` or `total`; `bank` holds the account object; there is no
  `paid_at`, `qr_payload` or `va_number`. Consumer and portal order
  responses use `subtotal_minor`/`tax_minor`/`total_minor` and nest the
  instruction under `payment`.
- **Quantity is required and capped at 100.** There is no implicit
  quantity of 1; `0` is `400 INVALID_QUANTITY`.
- **Idempotency mismatch is 422, not 409.** `IDEMPOTENCY_KEY_CONFLICT`
  is the existing middleware answer; the plan's "409" is stale. The
  header itself is optional on create, required by no consumer route.
- **Portal ownership uses email, not an external customer id.**
  `payment_orders` has no `external_customer_id` column in phase 1a;
  the session email must match the order email or the buyer JSON's
  `email`. An invoice is matched by its buyer email or its order's.
  The spec's `external_customer_id` filters are not implemented on the
  portal routes.
- **Provider-reported expiry does not emit `order.expired`.** Only the
  reconciliation sweep does; settlement is outside this module. This is
  the one event gap to close before relying on expiry webhooks.

**Verify after merge.** The phase-1b modules were built in parallel
worktrees. Reconcile these against the merged handlers before treating
them as frozen: quote response keys and rounding; `discounts[]`
element fields; admin coupon/report response columns; admin invoice
list filters and PDF route; billing-worker settings defaults; and the
portal coupon-validate route (it is module B's, not this module's).

## Retrying

Decide from the status, not the code:

- **429** back off. `QUOTA_EXCEEDED` is the exception: a spent usage
  quota does not refill by waiting.
- **502 / 503** back off, except `*_NOT_CONFIGURED` and `*_DISABLED`,
  which mean the feature is off for this install and will answer the
  same way forever.
- **500** `INTERNAL_ERROR`, always the same opaque message; the detail
  goes to the server log. Worth one retry.
- **4xx otherwise** the request is the problem; retrying it unchanged
  will not help.
