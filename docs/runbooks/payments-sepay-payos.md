# Payments: SePay and PayOS (phase 1a)

Operator runbook for the Vietnamese payment rails. It covers the state
this release actually ships: SePay **static VietQR** plus the SePay bank
webhook, an optional SePay hosted-gateway **IPN** receiver, and **PayOS**
hosted links. The API contract is in
[`docs/api-contract.md`](../api-contract.md); the design is
[the integration design](../superpowers/specs/2026-11-12-sepay-payos-integration-design.md).

> The code is the source of truth. This release deliberately does not
> implement coupons, invoices, a payment portal, a SePay CIDR allowlist,
> hosted-gateway checkout creation, or a PayOS sandbox. See **Known gaps**
> before enabling a rail.

The dashboard exposes the same operations under **Thanh toán**
(Providers, Orders, Unmatched; plan prices under **Plans**). The curl
shapes below are the canonical field names and are useful for automation
and for checking exactly what the SPA sent.

## 0. Before you sell anything

1. **Encryption key.** `RELEASE_KEY_ENCRYPTION_KEY` must be set (32-byte
   hex, `openssl rand -hex 32`). Provider credentials and webhook secrets
   are sealed with it. Without it the provider admin routes and the
   webhook routes are not registered, and checkout sees every provider as
   disabled. Verify the startup log says
   `payment provider config encryption: enabled`.
2. **Public URL.** `BASE_URL` must be the public HTTPS origin customers
   and providers reach. PayOS probes the webhook URL immediately; plain
   HTTP will not work. `payment_webhook_base_url` is writable through the
   admin settings API and overrides `BASE_URL` there when set; leave it
   empty to use `BASE_URL`.
3. **A plan and a price.** A plan with no active `plan_prices` row cannot
   be sold: checkout answers `422 PAYMENT_PRICE_NOT_CONFIGURED`. Enter
   the price **before** publishing the plan (section 4).
4. **An API key for the merchant backend.** The key needs
   `payments:write` (create/cancel) and `payments:read` (poll). Bind it
   to the product when the backend serves one product. An `admin` key
   does **not** reach the consumer `/payments/*` routes, and a
   `payments:*` key does not reach `/admin/*`.
5. **`PAYMENT_MODE`.** Optional, default `live`; it is only the fallback
   for an admin write or a resolve that omits a mode. A row carries the
   mode it was written under.

Health check once configured:

```sh
curl -sS -H "Authorization: Bearer $ADMIN_KEY" \
  "$BASE_URL/api/v1/admin/payments/health" | jq
```

Both `sepay` and `payos` always appear. `enabled` means a usable row,
not merely that one exists.

## 1. SePay static VietQR (MB Bank)

The account used in this deployment:

| Field | Value |
|---|---|
| Bank | MB Bank |
| Account number | `0978482700` |
| BIN | `970422` |
| VietQR short code | `MB` |
| Account name | the name registered on the MB account |

The API does not hold a merchant id or a gateway secret key for SePay.
The configuration fields are only those below; the static QR is built
from the bank details and the order's amount/code.

### 1.1 Save the provider row

```sh
curl -sS -X PUT "$BASE_URL/api/v1/admin/payments/providers" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "provider": "sepay",
    "product_id": "",
    "mode": "live",
    "enabled": true,
    "config": {
      "bank_name": "MB Bank",
      "bank_bin": "970422",
      "bank_code": "MB",
      "account_no": "0978482700",
      "account_name": "CONG TY ...",
      "webhook_auth_mode": "hmac"
    },
    "credentials": {
      "api_token": "SEPAY_API_TOKEN",
      "ipn_secret": "SEPAY_IPN_SECRET"
    },
    "webhook_secret": "SEPAY_WEBHOOK_HMAC_SECRET"
  }' | jq
```

- `product_id: ""` is the install-wide row; a product override has the
  product's id and otherwise the same shape.
- `api_token` is **required for an enabled row** even if you only use the
  static QR: the reconciler uses it to list bank transactions.
- `webhook_secret` is required to enable a SePay row. In `hmac` mode it
  must equal the secret configured on the SePay webhook. In `apikey` mode
  the adapter verifies `Authorization: Apikey <api_token>` instead, but
  the row still requires a non-empty `webhook_secret`; store a random
  value.
- `webhook_auth_mode` is `hmac`, `apikey` or `none`. `none` is test-only:
  a `live` row is refused with `400 PAYMENT_PROVIDER_CONFIG_INVALID`.
- `ipn_secret` is only for the hosted-gateway IPN (section 1.3). It may
  be empty if you do not receive IPNs.

The response never returns secrets, only `credentials_set` and
`secret_set`. A blank field on a later `PUT` keeps the stored value; use
`DELETE .../secrets` to clear one.

### 1.2 Configure the bank webhook in SePay

In the SePay dashboard, open the bank/webhook settings for account
`0978482700` and add:

- URL: `{BASE_URL}/api/v1/webhook/sepay`
- Method: `POST`
- Authentication: the same mode as `webhook_auth_mode`
  - HMAC: the secret you stored in `webhook_secret`. SePay sends
    `X-SePay-Timestamp` and `X-SePay-Signature: sha256=<hex>`; the
    signature is HMAC-SHA256(secret, `<timestamp>.<raw body>`), accepted
    within 300 seconds of server time.
  - API key: `Authorization: Apikey <api_token>`.
- Events: incoming bank transfers (the adapter also receives `out` and
  records it without settling).

Remove other delivery URLs unless they are intentional; SePay retries a
delivery it does not see `200 {"success": true}` for roughly 30 minutes.
A `200` with any other body counts as a failed delivery.

### 1.3 Hosted-gateway IPN (optional)

If a SePay **hosted gateway** order exists outside Keygate, configure its
IPN URL as `{BASE_URL}/api/v1/webhook/sepay-ipn` and set the same secret
in `credentials.ipn_secret`; the route checks `X-Secret-Key` in constant
time and acks with `200` and an empty body.

Phase 1a does **not** create hosted-gateway checkouts; that is deferred
to phase **1b**. The static QR is the only SePay instruction the API
creates; Keygate does not redirect buyers to SePay's gateway, and the
config/settings fields are accepted but unused for create. The IPN
route is live so an externally created gateway order can still settle.

### 1.4 Verify and rehearse

```sh
curl -sS -X POST "$BASE_URL/api/v1/admin/payments/providers/sepay/test" \
  -H "Authorization: Bearer $ADMIN_KEY" | jq
```

`ok: true` means the token reached `userapi.sepay.vn`. A rejected token
is still `200` with `ok: false` and a bounded `error`; a missing or
disabled row is `404`/`503`.

To rehearse without money, write a **test**-mode row and no live row for
the rail, create an order, then use `mark-paid`. `mark-paid` refuses
(`409 PAYMENT_MANUAL_SETTLEMENT_FORBIDDEN`) as soon as any enabled live
row covers the order's product and rail, so do this before the live row
exists. It also fails closed with `422
PAYMENT_MANUAL_SETTLEMENT_UNVERIFIABLE` when no usable test row resolves,
including a server without the provider config service (no
`RELEASE_KEY_ENCRYPTION_KEY`). In live mode the only settlement paths are
a verified webhook or an operator matching a real incoming transfer.

## 2. PayOS hosted links

PayOS is a hosted checkout: the order response carries `checkout_url`
and the buyer pays on PayOS's page. There is **no sandbox** in this
client; it always talks to `https://api-merchant.payos.vn`. Test with a
small real payment, or in test mode via `mark-paid` before configuring
the live row.

### 2.1 Save the provider row

From the PayOS merchant channel, collect `client_id`, `api_key` and
`checksum_key`, then:

```sh
curl -sS -X PUT "$BASE_URL/api/v1/admin/payments/providers" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "provider": "payos",
    "product_id": "",
    "mode": "live",
    "enabled": true,
    "config": { "client_id": "PAYOS_CLIENT_ID" },
    "credentials": {
      "api_key": "PAYOS_API_KEY",
      "checksum_key": "PAYOS_CHECKSUM_KEY"
    }
  }' | jq
```

PayOS verifies deliveries with `checksum_key`, not `webhook_secret`. The
`webhook_secret` field is optional and unused for PayOS; omit it (or
leave it blank on a later save to keep whatever is stored). `client_id`,
`api_key` and `checksum_key` are all required for an enabled row.

### 2.2 Register the webhook

```sh
curl -sS -X POST \
  "$BASE_URL/api/v1/admin/payments/providers/payos/confirm-webhook" \
  -H "Authorization: Bearer $ADMIN_KEY" | jq
```

This calls PayOS's `confirm-webhook` with
`{BASE_URL}/api/v1/webhook/payos`; PayOS probes the URL immediately with
a sample delivery, so the endpoint must already be public HTTPS and
answer `2xx`. Success is
`{"provider":"payos","webhook_url":"…","confirmed":true}`. It resolves
the **install-wide** PayOS row, or the row of the product named by the
optional `?product_id=…` query (for a product override with no
install-wide row); an unknown product is `404`. It refuses with `400
PAYMENT_WEBHOOK_URL_UNSET` if no base URL exists.

Repeat the call after changing `BASE_URL`, or when PayOS reports the
webhook as unverified. A PayOS refusal is `502 PAYMENT_PROVIDER_ERROR`
with a clipped message.

### 2.3 Verify and rehearse

```sh
curl -sS -X POST \
  "$BASE_URL/api/v1/admin/payments/providers/payos/test" \
  -H "Authorization: Bearer $ADMIN_KEY" | jq
```

The check queries a non-existent order: `01 not found` proves the keys
work; `401` means bad keys. As with SePay, `ok` is in a `200` response.

## 3. Enter prices before selling

Prices are per plan, provider, currency and billing interval:

```sh
curl -sS -X PUT "$BASE_URL/api/v1/admin/plans/$PLAN_ID/prices" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "provider": "sepay",
    "currency": "VND",
    "amount_minor": 1190000,
    "billing_interval": "month"
  }' | jq
```

- `amount_minor` is an integer in minor units. VND has exponent 0, so
  `1190000` is 1,190,000 ₫.
- Enter a row for every provider you intend to sell. With `provider`
  omitted at checkout, the service tries `payment_default_provider`, then
  `sepay`, then `payos`, and picks the first rail that has both a price
  and a usable config. If a rail has a price but no usable config, it
  moves to the next; if at least one is priced but none is usable the
  answer is `503 PAYMENT_PROVIDER_DISABLED`; if none is priced it is
  `422 PAYMENT_PRICE_NOT_CONFIGURED`.
- `billing_interval` should match the plan's interval (`month`, `year`,
  or empty for one-time).
- `GET /admin/plans/{id}/prices` lists active and retired rows;
  `DELETE /admin/plans/{id}/prices/{price_id}` deactivates one (soft
  delete). A slot conflict is `409 PRICE_SLOT_CONFLICT` — retry.

## 4. One end-to-end sale

1. Create the order with the merchant key:

   ```sh
   curl -sS -X POST "$BASE_URL/api/v1/payments/orders" \
     -H "Authorization: Bearer $PAYMENTS_KEY" \
     -H 'Content-Type: application/json' \
     -H "Idempotency-Key: $(uuidgen)" \
     -d '{"plan_id":"'"$PLAN_ID"'","email":"buyer@example.com","quantity":1}' | jq
   ```

2. The response has `data.code` (`KG…`), `data.total_minor`, and
   `data.payment`. For SePay, `qr_image_url` is the VietQR image and
   `bank_account` has `0978482700`; the buyer must transfer the **exact**
   `total_minor` with the order code in the memo. For PayOS, send the
   buyer to `payment.checkout_url`.
3. Share `{BASE_URL}/pay/{code}` or the status page. The status route is
   public and capability-based:
   `GET /pay/{code}/status` (also `/api/v1/pay/{code}/status`), with
   `Cache-Control: no-store`. It returns `amount_minor` (not `amount`)
   and no buyer/licence data.
4. When the webhook arrives, the order becomes `paid`, the licence is
   granted once, and `order.paid` / `payment.succeeded` are dispatched.
   Check `GET /admin/payments/orders/{code}`: `events` should contain the
   provider delivery and `transactions` the matched transfer.
5. If the order is still `pending` after a couple of minutes, see
   section 6 before touching anything.

Order TTL is 24 hours by default (`payment_order_ttl_minutes` read from
settings, default 1440; writable through the admin settings API). PayOS
orders may expire sooner if PayOS's link expiry is earlier. Expiry is
swept by the reconciler and the expiry loop; an expired order cannot be
paid.

## 5. Reconciliation worker

The worker starts only when `RELEASE_KEY_ENCRYPTION_KEY` is configured.
It ticks **immediately on boot**, then every **5 minutes**
(`reconcile.DefaultInterval`). Each pass:

1. Expires lapsed pending orders.
2. Asks PayOS about up to 100 pending PayOS orders (oldest first) whose
   webhook never arrived; a `paid` link settles through the same
   idempotent settlement service.
3. Pulls SePay bank transactions through the SePay API from the stored
   cursor, with a 24-hour backfill on the first pass and a 5-minute
   overlap on every pass. `(provider, provider_ref)` dedupe makes the
   overlap free.
4. Records transactions it cannot place in the **unmatched queue**. It
   never settles on a guess. An amount-only candidate is stored as a
   suggestion for an operator, not as a payment.

Cursor state is per `(provider, scope)` in `payment_sync_cursors`.
`cursor_lag_seconds` in `/admin/payments/health` is the first signal that
the SePay pull is stuck.

### Working the unmatched queue

```sh
curl -sS -H "Authorization: Bearer $ADMIN_KEY" \
  "$BASE_URL/api/v1/admin/payments/unmatched?limit=50" | jq
```

For each transfer, compare `amount_minor`, `currency`, `occurred_at`,
`reference`, `content` and the suggested order. Then either match:

```sh
curl -sS -X POST \
  "$BASE_URL/api/v1/admin/payments/transactions/$TX_ID/match" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"order_code":"KG7F3K2Q"}' | jq
```

or ignore it with a reason:

```sh
curl -sS -X POST \
  "$BASE_URL/api/v1/admin/payments/transactions/$TX_ID/ignore" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"reason":"duplicate of transfer FT…"}' | jq
```

Matching requires an unmatched, **incoming** transfer, the same provider,
a **pending** order and the exact amount and currency. A match settles
the order and grants the licence; an ignore does not. Outgoing transfers
are never matched.

## 6. Incidents

### A buyer paid but the order is still `pending`

1. Look at the order detail: `GET /admin/payments/orders/{code}`. Check
   `events` (did a delivery arrive?), `transactions` (did the money land
   but stay unmatched?), `last_settlement_error`, and `attempts`.
2. If the money is in `transactions` with `status=unmatched`, compare the
   amounts. A mismatch (buyer transferred the wrong amount) cannot be
   matched; refund it manually or ask the buyer to send the difference.
   If it matches, use `match`.
3. If there is no transaction, check the SePay dashboard's webhook
   deliveries. A `401` means the auth mode/secret differs (or the server
   clock is more than 300 s off in HMAC mode). A `500` means settlement
   failed transiently; SePay retries for roughly 30 minutes, and the
   5-minute reconcile pass is the backstop.
4. For PayOS, `GET /admin/payments/health` and the order's `provider_ref`
   show whether the link is paid. The reconciler queries pending PayOS
   orders each pass; do not create a second order for the same purchase.
5. Only in a test configuration may you use `mark-paid`
   (`POST …/mark-paid` with `{"reason":"…"}`). In live mode it is
   refused; settle by matching the real incoming transfer.

### Webhook returns `503`

No enabled, decryptable config for the order's product/rail, or the
server has no master key. Check the startup log for
`payment provider config encryption: enabled`, check
`GET /admin/payments/providers` for `enabled`, `credentials_set` and
`secret_set`, then restart after fixing `RELEASE_KEY_ENCRYPTION_KEY`.
Without the key the webhook routes are not registered at all (404).

### Webhook returns `401`

The response is `401` with the envelope code
`PAYMENT_WEBHOOK_UNAUTHORIZED`; the message is deliberately generic.
Check the matching verification material:

- HMAC: `webhook_secret` must equal the dashboard secret byte-for-byte;
  the timestamp must be within 300 s (fix NTP); the signature is over
  `<timestamp>.<raw body>`, so a proxy that rewrites the body breaks it.
- API-key mode: the dashboard must send `Authorization: Apikey <token>`
  (capital `A`, single space), and `api_token` must match.
- PayOS: `checksum_key` must be the channel's checksum key, not the API
  key. PayOS signs `data`, so a proxy that reformats JSON breaks it too.

### Webhook returns `429`

Each webhook route is limited to 120 requests/minute per IP
(`sepay_webhook`, `sepay_ipn`, `payos_webhook`). Provider retries may
burst. Check for a retry storm or a shared egress IP; do not raise the
limit blindly.

### Duplicate licence / double grant

Settlement dedupes on the provider event id and on
`(provider, provider_ref)`, and the licence id is derived from the order,
so a redelivery must not grant twice. If two licences exist, preserve the
rows and the audit trail, find both orders/transactions, and escalate;
do not delete payment rows to "clean up".

### Refund requested

Phase 1a records **manual** refunds only:

```sh
curl -sS -X POST \
  "$BASE_URL/api/v1/admin/payments/orders/$CODE/refund" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"amount_minor":1190000,"reason":"customer request","method":"manual"}' | jq
```

The operator returns the money outside Keygate. The order becomes
`refunded` (full) or `partially_refunded`; the licence is **not**
revoked. Revoke or shorten the licence separately if the business
decision is to do so.

### Rotating or clearing secrets

- To replace a secret, `PUT` the provider row with the new value. A
  blank value keeps the old one.
- To remove one, `DELETE /admin/payments/providers/{row_id}/secrets`
  with `{"fields":["credentials"]}` or `["webhook_secret"]` (or both).
  `{row_id}` is the config row's `id`, not `sepay`/`payos`.
- After rotating, call `.../test` and, for PayOS, `confirm-webhook`
  again. Update the SePay dashboard in the same change window.

### Actor attribution

`payment_refunds.created_by` and `payment_transactions.matched_by` store
the signed-in admin's user id; an API-key admin is stored as `NULL`
because `apikey:<id>` is not a `users(id)`. The audit log keeps the full
principal (`apikey:<id>` or the user id). This is expected in phase 1a;
do not "fix" the NULL by writing a fake user id.

## 7. Known gaps (do not plan around them)

| Gap | Consequence |
|---|---|
| No coupons / invoice / customer portal payments | `discount_minor` is 0; no invoice PDF/JSON; no portal order pages |
| No payment outbox | Events are dispatched at settlement; a missed consumer event must be reconciled from the order/status API |
| Four events only | `order.created`, `order.paid`, `payment.succeeded`, `payment.failed`; no expiry/cancel/refund events |
| No SePay CIDR allowlist | A valid HMAC/apikey is the only gate; `allowed_cidrs` is not in the config |
| No SePay hosted-gateway checkout creation | Deferred to phase 1b: static QR only, no redirect to SePay's gateway; the IPN route can still settle an external gateway order |
| PayOS has no sandbox | Test with a small live payment, or mark a test-mode order paid before enabling the live row |
| Refunds are manual and do not revoke | Recording a refund is not a licence revocation |
| Consumer GETs share the `payments_api` rate limit | `RATE_LIMIT_API` per minute per IP across create, cancel and both GETs |
| `payment_return_base_url` is writable but unused | Return/cancel URLs still come from `BASE_URL` in phase 1a |

## 8. Go-live checklist

- [ ] `RELEASE_KEY_ENCRYPTION_KEY` set; startup log confirms provider
      encryption enabled.
- [ ] `BASE_URL` is public HTTPS and matches the host providers call.
- [ ] SePay row enabled; bank details are MB `0978482700`, BIN `970422`,
      code `MB`; `api_token` set; webhook mode/secret match the SePay
      dashboard; `.../test` returns `ok: true`.
- [ ] PayOS row enabled; `client_id`, `api_key`, `checksum_key` set;
      `confirm-webhook` returns `confirmed: true` after PayOS probes.
- [ ] A price exists for every plan/provider you sell; the checkout
      create returns `201` and a payable instruction.
- [ ] A small live transfer or PayOS payment settles exactly once and
      appears in the order's `transactions` and `events`.
- [ ] `GET /admin/payments/health` has no `last_error`, no growing
      `cursor_lag_seconds`, and the unmatched queue is empty or explained.
- [ ] Refund and manual-settlement permissions are understood: only a
      test configuration allows `mark-paid`; refunds are recorded, not
      revoked.
