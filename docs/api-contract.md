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
**PayOS** (a hosted payment link). Amounts are integers in the
currency's minor unit: `amount_minor`, `subtotal_minor`, `tax_minor`,
`total_minor`. VND has exponent 0, so the integer is đồng. Phase 1a
sells VND only.

Provider configuration and the webhook routes need
`RELEASE_KEY_ENCRYPTION_KEY`. Without it no provider config can be
decrypted, the webhook routes are **not registered** (they answer the
ordinary `404 NOT_FOUND`), and every provider looks disabled to
checkout.

### Consumer API

Every route needs an API key. `payments:write` reaches create and
cancel; `payments:read` (or `payments:write`) reaches the two GETs.
All four consumer routes share the `payments_api` rate-limit scope —
`RATE_LIMIT_API` per minute per IP; a throttled call is `429`. A key
bound to one product sees only that product's orders.

| Method | Path | Scope |
|---|---|---|
| `POST` | `/api/v1/payments/orders` | `payments:write` |
| `GET` | `/api/v1/payments/orders` | `payments:read` or `payments:write` |
| `GET` | `/api/v1/payments/orders/:code` | `payments:read` or `payments:write` |
| `POST` | `/api/v1/payments/orders/:code/cancel` | `payments:write` |

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
- `buyer` accepts exactly `name`, `email`, `phone`, `tax_id` and
  `address`. PayOS receives the ones it supports; there is no
  `company_name` or `request_invoice` in phase 1a.
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
      "account_name": "CONG TY TNHH KEYGATE"
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
`partially_refunded`. Attempt statuses are separate: `created`,
`pending`, `succeeded`, `failed`, `expired`, `canceled` (`paid` is an
order status and never an attempt status).

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
            "account_no": "0978482700", "account_name": "CONG TY TNHH KEYGATE" }
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

### Phase-1a drift as shipped

The design spec describes a larger phase 1
([`2026-11-12-sepay-payos-integration-design.md`](superpowers/specs/2026-11-12-sepay-payos-integration-design.md)).
These are the places the shipped rail is intentionally narrower; code
and this contract are the source of truth.

- **Four webhook events, not ten.** Only `order.created`, `order.paid`,
  `payment.succeeded` and `payment.failed` are emitted. There is no
  `order.expired`, `order.canceled`, `payment.refunded`,
  `invoice.*` or `license.renewal_due` yet.
- **`payment.succeeded.amount` is an integer in minor units.** VND has
  exponent 0, so it is đồng — not a decimal string and not a float.
  `payment.failure_reason` is derived server-side from the normalised
  status (`failed`/`partial`/`voided`), not copied from the provider's
  free-text `desc`.
- **No outbox.** Events are dispatched synchronously at settlement
  through the existing webhook service; a consumer that missed one must
  reconcile against `GET /api/v1/payments/orders/:code` or the status
  endpoint. There is no replay log for payment events beyond
  `payment_events` (raw provider deliveries, admin-visible).
- **`POST /api/v1/payments/quote` is deferred.** It is not registered
  at all and answers the ordinary `404 NOT_FOUND`; phase 1a has no
  coupon pricing to preview.
- **No coupons, invoices or portal payments.** `coupon_codes`,
  `discounts[]`, invoice JSON/PDF and portal order/invoice pages are
  absent; `discount_minor` is always 0. The coupon tables exist only for
  a later phase.
- **SePay CIDR allowlist deferred.** There is no source-IP check in
  phase 1a; HMAC/apikey (and PayOS's checksum) is the gate. The
  `payment_default_provider`, `payment_order_ttl_minutes`,
  `payment_return_base_url` and `payment_webhook_base_url` settings are
  writable through the admin settings API. `payment_return_base_url` is
  accepted but not yet read by a create path (PayOS return/cancel URLs
  come from `BASE_URL`).
- **PayOS confirm-webhook is explicit, and PayOS has no sandbox.** The
  admin action registers the URL only after the install is public
  HTTPS; PayOS probes immediately. Test with a real small payment, or
  mark a `test`-mode order paid manually. The optional spec §5.2 buyer
  fields `company_name` and `request_invoice` are not accepted; only
  `name`, `email`, `phone`, `tax_id`, `address` are.
- **SePay hosted-gateway checkout creation is deferred to phase 1b.**
  Phase 1a creates only the static VietQR: `CreateAttempt` has no
  `RedirectURL`/`FormHTML` path, and Keygate never redirects a buyer to
  SePay's gateway. The IPN route, `X-Secret-Key` verification and
  settlement are live, so an existing external gateway integration can
  still notify `/api/v1/webhook/sepay-ipn`. The `ipn_secret` credential
  and the `payment_return_base_url` setting are accepted but are not
  used to create a gateway checkout.
- **Admin actor attribution is NULL for API-key admins.**
  `payment_refunds.created_by` and `payment_transactions.matched_by`
  reference `users(id)`, so an `apikey:<id>` principal is stored as
  SQL NULL; the audit row keeps the full principal. A dashboard session
  stores the real user id.
- **Refunds are manual-only and do not revoke licences.** The operator
  returns the money outside the system and records the row; `method`
  other than `manual` is refused. The order becomes
  `refunded`/`partially_refunded`, the licence is untouched.
- **Public status field names are flat.** `amount_minor`, not
  `amount` or `total`; `bank` holds the account object; there is no
  `paid_at`, `qr_payload` or `va_number`. Consumer order responses use
  `subtotal_minor`/`tax_minor`/`total_minor` and nest the instruction
  under `payment`.
- **Quantity is required and capped at 100.** There is no implicit
  quantity of 1; `0` is `400 INVALID_QUANTITY`.
- **Idempotency mismatch is 422, not 409.** `IDEMPOTENCY_KEY_CONFLICT`
  is the existing middleware answer; the plan's "409" is stale. The
  header itself is optional on create.

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
