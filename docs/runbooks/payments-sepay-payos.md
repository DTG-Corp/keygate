# Payments: SePay and PayOS (phase 1a + 1b)

Operator runbook for the Vietnamese payment rails. It covers the state
this release actually ships: SePay **static VietQR** plus the SePay bank
webhook, an optional SePay hosted-gateway **IPN** receiver, **PayOS**
hosted links, and the phase-1b layer on top — coupons and redemptions,
invoices with PDF storage and an e-invoice manual queue, the billing
worker and dunning ladder, revenue/reconciliation/VAT reports, and the
signed-in customer's payment portal. The API contract is in
[`docs/api-contract.md`](../api-contract.md); the design is
[the integration design](../superpowers/specs/2026-11-12-sepay-payos-integration-design.md).

> The code is the source of truth. Still not implemented: a SePay CIDR
> allowlist, SePay hosted-gateway checkout creation, a PayOS sandbox, or
> an outbox for outbound events. See **Known gaps** before enabling a
> rail. Phase-1b modules were built in parallel worktrees; operator-facing
> details marked **verify after merge** are reconciled in the phase-1b
> merge, not frozen by this book.

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

If a SePay **hosted gateway** order exists outside DSoft, configure its
IPN URL as `{BASE_URL}/api/v1/webhook/sepay-ipn` and set the same secret
in `credentials.ipn_secret`; the route checks `X-Secret-Key` in constant
time and acks with `200` and an empty body.

Phase 1a does **not** create hosted-gateway checkouts; that is deferred
to phase **1b**. The static QR is the only SePay instruction the API
creates; DSoft does not redirect buyers to SePay's gateway, and the
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

## 4A. Coupons

Coupons are created in **Thanh toán → Coupons** or through the admin
API. The code is the customer-facing string; the rest is policy.

```sh
curl -sS -X POST "$BASE_URL/api/v1/admin/coupons" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "code": "TET2026",
    "type": "percent",
    "value": 1000,
    "active": true,
    "description": "Tet 2026",
    "valid_from": "2026-01-15T00:00:00Z",
    "valid_to": "2026-02-15T23:59:59Z",
    "max_redemptions": 500,
    "per_customer_limit": 1,
    "stack_group": "campaign",
    "stackable": true,
    "priority": 100,
    "scopes": [{"scope_type": "product", "scope_id": "'"$PRODUCT_ID"'"}]
  }' | jq
```

- `type` is `percent` (value in basis points: 1000 = 10%), `fixed`
  (minor units, VND đồng) or `trial` (adds trial days; it cannot be
  combined with amount coupons). `value` is never a decimal.
- `max_redemptions: 0` means unlimited; `per_customer_limit: 0` means
  unlimited per email. Redemption is recorded **at settlement**, not at
  order creation, so an abandoned order does not consume a code.
- `valid_from`/`valid_to` are UTC instants; empty means open-ended.
  `trial_days` is required for `trial` and forbidden/ignored otherwise.
- `stack_group` (`general|campaign|private`) and `stackable` enforce §3.9: two coupons from the same
  group conflict, a non-stackable coupon cannot be combined, and the
  number of amount coupons is capped by the `coupon_max_stack` setting
  (default 2). A violation is a `PAYMENT_COUPON_*` refusal at quote or
  order create, never a silent discount.
- `assigned_email` binds a code to one address; another email gets
  `PAYMENT_COUPON_ASSIGNED`.
- `scopes[]` replaces the stored set atomically on `PUT`. With no
  scopes the code applies to the whole product catalogue the admin key
  can see.
- `DELETE` only removes a code with no redemptions. Once redeemed, set
  `active: false` (archive) — history and reports keep pointing at it.

Preview a price without creating an order:

```sh
curl -sS -X POST "$BASE_URL/api/v1/payments/quote" \
  -H 'Content-Type: application/json' \
  -d '{"plan_id":"'"$PLAN_ID"'","quantity":1,
       "email":"buyer@example.com","coupon_codes":["TET2026"]}' | jq
```

Quote is public and rate-limited. It does not reserve or consume the
code. Check `data.discount_minor` and `data.total_minor`; the order
create applies the same calculation.

Redemption history is `GET /api/v1/admin/payments/redemptions`
(filters `coupon`, `email`, `from`, `to`) or the per-coupon route. Add
`?format=csv` for accounting. To stop a campaign immediately, archive
the coupon (`PUT` with `active:false`) rather than deleting it.

## 4B. Invoices and PDFs

A paid order creates one invoice in the same transaction as the licence.
The number is minted from a per-year counter under a row lock, so it is
gap-free per year and never reused after a void. Statuses:

| Status | Meaning | Operator action |
|---|---|---|
| `draft` | written but not issued | `POST .../issue` |
| `issued` | PDF available, number final | download / send |
| `pending_issue` | e-invoice provider configured but not wired | issue by hand, then mark issued |
| `void` | cancelled; number stays spent | none |

Seller block (printed on every invoice) comes from settings, not from
the plan or the buyer:

```sh
curl -sS -X PUT "$BASE_URL/api/v1/admin/settings" \
  -H "Authorization: Bearer $ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"settings":{
    "invoice_seller_name":"CONG TY TNHH DSOFT",
    "invoice_seller_tax_code":"0312345678",
    "invoice_seller_address":"1 Le Loi, District 1, HCMC",
    "invoice_seller_email":"billing@dsoft.app",
    "invoice_note":"Cam on quy khach.",
    "einvoice_provider":""
  }}' | jq
```

`invoice_note` is a footer, not a legal term. `einvoice_provider` empty
is the normal PDF-only mode; a configured-but-unwired provider is what
puts new invoices in `pending_issue` for the manual queue. **Verify
after merge:** module C adds these keys to the settings allow-list; if a
save returns `400 unknown setting`, the module has not been merged yet.

List and inspect:

```sh
curl -sS -H "Authorization: Bearer $ADMIN_KEY" \
  "$BASE_URL/api/v1/admin/payments/invoices?status=pending_issue&limit=50" | jq
curl -sS -H "Authorization: Bearer $ADMIN_KEY" \
  "$BASE_URL/api/v1/admin/payments/invoices/$INVOICE_ID" | jq
curl -sS -H "Authorization: Bearer $ADMIN_KEY" \
  "$BASE_URL/api/v1/admin/payments/invoices/$INVOICE_ID/pdf" -o invoice.pdf
```

`issue` on a draft (or retry on `pending_issue`) moves it to `issued`
once the provider accepts. An already issued or void invoice is `409
INVOICE_ALREADY_ISSUED`; a provider failure is `502
EINVOICE_PROVIDER_ERROR` and leaves the row retryable. `void` keeps the
number and changes no order or licence. Do **not** delete PDF files by
hand: the invoice row and the stored object are one record; a missing
PDF is a `404 INVOICE_NOT_FOUND` on the read route.

The customer sees the same invoice in the portal, and the merchant API
sees it at `GET /api/v1/payments/invoices/{id}` (product-scoped). The
buyer/tax block is frozen at issue time; fixing a customer's address
later does not rewrite an issued invoice — void and reissue if the
document is wrong.

## 4C. Billing worker and dunning ladder

The billing worker owns renewals, reminders, grace and the terminal
state. Its cadence is set by the server configuration, not a settings
key. The ladder is:

1. **Renewal due.** At `valid_until`, a renewal order is created (or the
   subscription renews) and `license.renewal_due` is dispatched. The
   customer can pay from the portal.
2. **Reminders.** `renewal_reminder_7`, `_3`, `_1` are dispatched before
   the due date by the configured email templates.
3. **Grace.** After `valid_until`, the licence enters `past_due` for the
   plan's `grace_days`; `renewal_grace_1/3/7` reminders fire inside it.
   Payment at any point in grace settles the renewal and clears
   `past_due`.
4. **Suspension.** After grace, the licence is `suspended`
   (`renewal_suspended`); the product's licence checks fail until the
   renewal is paid or the licence is reinstated.
5. **Expiry.** After the plan's renewal window, the licence expires. A
   late payment still settles through the normal idempotent path if the
   order is pending; expired orders do not.

Operators can suspend, reinstate, extend or change a licence from
**Licences** without waiting for the ladder. Cancel-at-period-end is a
licence flag, not an immediate cancel. Trials convert on the same
PaidHook as a paid order; a `trial` coupon and amount coupons cannot be
combined.

If the worker is not running, nothing below the first step happens:
renewals stay `past_due` and no reminder is sent. Check the server log
for the billing worker's start line and its tick errors before editing
licence dates by hand.

## 4D. Reports and CSV

All three reports are admin-only, accept `from`/`to`, and stream CSV
with `?format=csv`:

| Report | Use it for |
|---|---|
| `/admin/payments/reports/revenue` | revenue by provider/plan/product/period, net of recorded refunds |
| `/admin/payments/reports/reconciliation` | daily settled vs provider-reported totals and drift; pair it with the unmatched queue |
| `/admin/payments/reports/vat` | output VAT contained in paid totals per period |

Money is integer minor units (`amount_minor` style), VND exponent 0.
Reconciliation is the month-end control: a non-zero drift with an empty
unmatched queue means a provider transfer was never ingested (check
`cursor_lag_seconds` and the provider dashboard), not that the ledger
should be edited. Report column names are module C's — **verify after
merge** before handing a CSV to accounting.

## 4E. Customer portal

The portal is the signed-in surface at `/portal` (browser session, no
API key). It shows only the customer's own orders and invoices.

- **Orders.** The list and detail show pending/paid/expired/canceled
  orders. **Pay** re-issues the instruction for a pending, unexpired
  order without changing its price or creating a second order. **Cancel**
  works only while pending. A paid or expired order answers
  `PAYMENT_ORDER_NOT_PAYABLE`; support should read the status rather
  than retry.
- **Invoices.** JSON list/detail and `?format=pdf`. A customer who
  cannot see an invoice is usually signed in with a different email
  than the order's; ask them to check the account email, do not send
  another customer's PDF.
- **Billing profile.** Name is required; asking for a VAT invoice
  requires a tax code (`PAYMENT_TAX_CODE_REQUIRED`). Saving updates the
  checkout default and is audited; it does not rewrite invoices already
  issued.

Ownership is by email in phase 1: the order email or the buyer JSON's
`email`; an invoice's buyer email or its order's email. A foreign
order/invoice is a plain `404`, never `403` — do not treat that as a
missing record until you have checked which account is signed in.

## 5. Reconciliation worker

The worker starts only when `RELEASE_KEY_ENCRYPTION_KEY` is configured.
It ticks **immediately on boot**, then every **5 minutes**
(`reconcile.DefaultInterval`). Each pass:

1. Expires lapsed pending orders and dispatches `order.expired` once
   per moved row.
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

The operator returns the money outside DSoft. The order becomes
`refunded` (full) or `partially_refunded`; the licence is **not**
revoked. Revoke or shorten the licence separately if the business
decision is to do so.

### A coupon is refused

Check `GET /admin/coupons?q=<code>` first: archived, expired (compare
`valid_to` with UTC now), outside its product/plan scope, already at
`max_redemptions`, over `per_customer_limit`, or assigned to another
email. `PAYMENT_COUPON_EXHAUSTED` is a 409 and means the global cap;
the other `PAYMENT_COUPON_*` codes are 422 pricing refusals. Do not
"fix" it by editing the order: re-quote with the corrected code.
`ignore_invalid_coupons` is for marketing best-effort flows only; a
checkout that asks for a discount should fail loudly.

### Invoice issue is refused or stuck

- `409 INVOICE_ALREADY_ISSUED`: the row is already `issued` or `void`.
  Read it before retrying; `void` is terminal and keeps the number.
- `502 EINVOICE_PROVIDER_ERROR`: the provider call failed; the row is
  retryable (`pending_issue` or still `draft`). Check the provider
  credentials and the configured `einvoice_provider`, then issue again.
- `pending_issue` that never moves: this is the manual queue. The
  provider is configured but not wired; issue the document by hand,
  then mark the row issued. Do not set `einvoice_provider` to a random
  string to "unstick" it.
- PDF missing but the row is `issued`: the stored object is gone or the
  PDF service is unconfigured. Restore the object from backup; do not
  mint a second invoice number by deleting the row.

### Portal pay or cancel is refused

`409 PAYMENT_ORDER_NOT_PAYABLE` is the correct answer for a paid,
expired, canceled or refunded order; the portal is not allowed to
resurrect it. Re-issue is only for a pending, unexpired order. `503
PAYMENT_PROVIDER_DISABLED` means no usable provider config for that
rail, not a customer error. A `404` on the customer's own order means
the signed-in email does not match the order email or buyer email —
check the account, not the order id.

### Reports do not match the ledger

Start with `/admin/payments/reports/reconciliation?from=…&to=…` and the
unmatched queue. Revenue is net of refunds; VAT is the contained tax,
not a separate charge. If the ledger and provider totals differ, look
for an un-ingested transfer (cursor lag), an amount-mismatch
transaction, or a manually settled test row. Never edit `payment_orders`
to make a report tie; settle or match the transaction instead.

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
| No payment outbox | Events are dispatched at the transition; a missed consumer event must be reconciled from the order/invoice/status API |
| Provider-reported expiry does not emit `order.expired` | Settlement records `pending -> expired` without the event; only the reconciliation sweep emits it. Verify/close after the merge |
| No SePay CIDR allowlist | A valid HMAC/apikey is the only gate; `allowed_cidrs` is not in the config |
| No SePay hosted-gateway checkout creation | Static QR only; no redirect to SePay's gateway. The IPN route can still settle an external gateway order |
| PayOS has no sandbox | Test with a small live payment, or mark a test-mode order paid before enabling the live row |
| Refunds are manual and do not revoke | Recording a refund is not a licence revocation; `payment.refunded` is emitted once per row |
| Portal ownership is by email | `payment_orders` has no `external_customer_id`; a customer signed in with another email cannot see the order |
| Coupon/report/invoice response shapes | Module B/C were built in parallel; the contract marks them **verify after merge** |
| `payment_return_base_url` is writable but unused | Return/cancel URLs still come from `BASE_URL` |
| Billing-worker cadence is not a setting | It is server configuration; there is no dashboard knob in phase 1b |

## 8. Acceptance checklist (phase 1a + 1b)

Tick every line against the merged install, not this worktree. The
parenthesised Go tests are the automated evidence; manual steps cover
what a test cannot see.

**Rails and 1a**

- [ ] `RELEASE_KEY_ENCRYPTION_KEY` set; startup log confirms provider
      encryption enabled.
- [ ] `BASE_URL` is public HTTPS and matches the host providers call.
- [ ] SePay row enabled; bank details are MB `0978482700`, BIN `970422`,
      code `MB`; `api_token` set; webhook mode/secret match the SePay
      dashboard; `.../test` returns `ok: true`.
- [ ] PayOS row enabled; `client_id`, `api_key`, `checksum_key` set;
      `confirm-webhook` returns `confirmed: true` after PayOS probes.
- [ ] A price exists for every plan/provider you sell; checkout create
      returns `201` and a payable instruction.
- [ ] A small live transfer or PayOS payment settles exactly once and
      appears in the order's `transactions` and `events`.
- [ ] `GET /admin/payments/health` has no `last_error`, no growing
      `cursor_lag_seconds`, and the unmatched queue is empty or
      explained.

**Coupons**

- [ ] Create a percent coupon scoped to the product; quote it and see
      `discount_minor`/`total_minor` change with no order created.
- [ ] Stack two amount coupons up to `coupon_max_stack`; a third is
      `PAYMENT_COUPON_STACK_LIMIT`; two in one `stack_group` are
      `PAYMENT_COUPON_GROUP_CONFLICT`.
- [ ] Settle one order with the coupon; the redemption history shows
      exactly one row, and the global CSV has the header plus that row.
- [ ] Archive the coupon; it no longer quotes, but the redemption row
      and the old invoice discount remain.
- [ ] (`TestCoupon*`, module B — verify the names after merge.)

**Invoices**

- [ ] A paid order created exactly one invoice; the number is unique
      and the seller block matches the settings.
- [ ] Issue a draft; `invoice.issued` appears once in the webhook
      deliveries, and the customer can download the PDF from the portal
      and the merchant API.
- [ ] Void an issued invoice; the number is not reused, `invoice.voided`
      appears once, and the order/licence are unchanged.
- [ ] With `einvoice_provider` set but unwired, a new invoice lands in
      `pending_issue`; issuing it by hand clears the queue.
- [ ] (`TestPortalInvoice*`, `TestInvoice*` in module C — verify after
      merge.)

**Portal and events**

- [ ] Two customers: each sees only their own orders/invoices; the
      other's code/id is `404` (`TestPortalOrderListIsCustomerScoped`,
      `TestPortalOrderDetailOwnershipAndPayment`,
      `TestPortalInvoiceListIsCustomerScoped`,
      `TestPortalInvoiceDetailOwnershipAndOrderFallback`,
      `TestPortalInvoicePDFIsRawAndOwned`).
- [ ] Pay a pending order from the portal: a fresh instruction replaces
      the old one; a paid order is `409`
      (`TestPortalPayReissuesOnlyAPayableOwnOrder`,
      `TestReissueAttempt*`).
- [ ] Cancel from the portal: status becomes `canceled`, one
      `order.canceled` delivery, a second cancel is `409`
      (`TestPortalCancelOnlyOwnPendingOrder`,
      `TestConsumerCancelEmitsOrderCanceledOnce`).
- [ ] Save a billing profile without a name → `400`; with
      `request_invoice` and no tax code → `422
      PAYMENT_TAX_CODE_REQUIRED`; the saved row is per-user and audited
      (`TestPortalBillingProfileValidation`,
      `TestPortalBillingProfileRoundTripAndAudit`,
      `TestPortalBillingProfileIsPerUser`).
- [ ] Let a test order lapse: the sweep emits `order.expired` exactly
      once, and a second tick emits nothing
      (`TestWorkerTickEmitsOrderExpiredOnce`,
      `TestExpirePaymentOrdersWithRowsReturnsMovedRows`).
- [ ] Refund a paid order: one `payment.refunded` delivery with the
      refund amount and method; a retry of the same refund emits nothing
      (`TestAdminRefundEmitsPaymentRefundedOncePerRecordedRefund`,
      `TestOrderEventEmitterRefundPayload`).
- [ ] The merchant API and portal invoice PDFs are byte-identical for
      the same invoice; every PDF response is
      `Cache-Control: private, no-store`.

**Reports and operations**

- [ ] Revenue CSV for a known day ties to the order ledger net of
      refunds; reconciliation shows no drift with the unmatched queue
      empty; VAT equals the contained tax on the same orders.
- [ ] The settings keys `coupon_max_stack`, `payment_quote_rate_limit`,
      `invoice_seller_*`, `invoice_note` and `einvoice_provider`
      round-trip through `PUT/GET /admin/settings`.
- [ ] The billing worker's start line is in the log; a renewal reaches
      `renewal_due` and the grace/suspend ladder in a test product
      without hand-editing a licence.
