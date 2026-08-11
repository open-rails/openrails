# OpenRails API Reference

The HTTP surface of the standalone OpenRails server. Everything is served on ONE
public port under the `/v1` prefix (plus unprefixed health probes and the `/auth`
control-plane mount). Embedded hosts mount a subset of the same route groups —
`GET /v1/capabilities` reports which groups a deployment actually serves.

All requests and responses are JSON unless noted. Non-2xx responses use the
Stripe-shaped error envelope from `pkg/api`:

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "invalid_parameter",
    "message": "Human readable description",
    "param": "optional_param_name"
  }
}
```

List endpoints use a Stripe-like list envelope:

```json
{ "object": "list", "data": [], "total": 0, "limit": 20, "offset": 0, "has_more": false }
```

## Authentication overview

| Caller class | Credential |
|---|---|
| Public (catalog, health, capabilities, solana pricing) | none |
| Self-service `/v1/me/*`, customer treasury `/v1/customers/*` | `Authorization: Bearer <delegated JWT>` — short-lived token minted by the merchant's registered issuer with `delegated_sub` (embedded mode: the host's user bearer adapted to the same principal) |
| Checkout `/v1/checkout` | any authenticated user bearer |
| Merchant `/v1/merchant/*`, `/v1/import/*` | `Authorization: Bearer <API key (openrails_st_…) | service JWT | delegated JWT | user access token>` — every route is gated on a `merchant:*` permission, not on credential type |
| Platform `/v1/platform/*` | human operator session checked against root-group grants (standalone only) |
| Webhooks | provider signature / source-IP verification, no bearer |

Merchant permissions: API keys carry the permissions they were minted with;
service JWTs (`token_use=service`, max 15-min lifetime, signed by a registered
issuer) carry a self-asserted `permissions` claim scoped to the issuer's
merchant; human sessions are checked against the user's merchant-group role.
The required permission is listed per route below.

### Idempotency-Key

Any mutating request (`POST`/`PUT`/`PATCH`/`DELETE`) may carry an optional
`Idempotency-Key` header. Keys are scoped per merchant and cached 24h. Same key
+ same method/path/body → the original response is replayed byte-for-byte with
`Idempotent-Replayed: true`. Same key + different body → `409
idempotency_key_reuse`. Key still in flight → `409 idempotency_in_progress`.
5xx responses are never cached. This is a replay cache layered on top of each
route's own dedup guards, never a replacement for them.

## 1. Public routes

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/` | none | JSON service banner `{"service":"billing","status":"ok",...}` |
| GET | `/health/live` (alias `/healthz`) | none | Unconditional liveness probe |
| GET | `/health/ready` (alias `/readyz`) | none | Readiness: Postgres, Redis, auth verifier. 200 or 503 `not_ready`; `?verbose=1` adds per-dependency detail |
| GET | `/v1/capabilities` | none | Static capability document: `route_groups` (which route sets are mounted) + `routes` (provider-specific toggles: `billing_portal`, `solana`, `solana_signing`, `webhooks`, `secret_write`). ETagged, `Cache-Control: public, max-age=300` |
| GET | `/v1/captcha/status` | none | Captcha challenge status for the browser tier |
| GET | `/v1/captcha/client.js` | none | Captcha client script |
| GET | `/v1/products` | optional | List products with embedded active prices. Query: `limit` (1-100, default 20), `offset` |
| GET | `/v1/prices` | optional | List prices. Query: `currency`, `product` (`prod_` ID or raw UUID), `type` (`recurring`/`one_time`), `limit`, `offset` |
| GET | `/v1/checkout-config` | none | Per-merchant checkout discovery: the merchant's **armed** PSPs as `{key, rail, display_name, flow, config}`, where `key` is checkout's `payment.rail` value, `flow` is `tokenize`/`redirect`/`wallet`, and `config` carries only public-by-nature values (NMI `tokenization_key` + `tokenization_url`; Basis Theory `public_api_key`). Merchant resolved from `Host`. ETagged, `Cache-Control: public, max-age=60`. Serves a fixed per-rail whitelist — no merchant secret can appear |
| GET | `/v1/solana/config` | none | Solana network/recipient config (mounted only when a Solana rail is configured) |
| GET | `/v1/solana/tokens` | none | Supported Solana tokens with live pricing: `{ tokens: [{symbol, name, mint, decimals, price}] }` |

There is no `/health` route — probes are `/health/live` and `/health/ready`.

## 2. Checkout + rail-specific public routes

Top-level checkout requires an authenticated user bearer. The same three
handlers are also mounted under `/v1/me/checkout/*` (delegated token) and
`/v1/customers/{customer_id}/checkout/*` (customer grant) — see section 3.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/v1/checkout` | bearer | Create a checkout session for a new subscription or one-off purchase |
| GET | `/v1/checkout/{id}` | bearer | Retrieve a session (403 if it belongs to another user) |
| POST | `/v1/checkout/{id}/confirm` | bearer | Confirm a Solana session: `{ payment: { rail: "solana", signature, wallet? } }` |
| GET | `/v1/checkout/{id}/solana-pay` | none (session-addressed) | Solana Pay transfer/transaction request for the session (buyer signs; mounted when a Solana rail is configured) |
| POST | `/v1/checkout/{id}/solana-pay` | none (session-addressed) | Solana Pay transaction-request callback |
| POST | `/v1/solana/recurring/enroll` | bearer (handler-enforced) | Confirm a Solana recurring enrollment after the wallet signs subscribe; OpenRails then charges the first cycle. Mounted only when OpenRails has a Solana signer |

`POST /v1/checkout` body:

- `price_id` (required)
- `mode` (optional) — `one_off` or `subscription`; resolved from the price if omitted
- `payment` (required):
  - `rail` (optional) — a configured PSP key (e.g. `mobius`) or reserved rail (`ccbill`, `solana`, `stripe`). Naming one pins it (never silently switched); omitting it hands the choice to the merchant's routing policy, which falls through unavailable PSPs and records the decision on the session's `routing_reason` (or#288)
  - `payment_method_id` or `payment_token` for NMI-backed rails / Stripe
  - `token_symbol` for `solana`; `flow` — `transfer_request` (default) or `transaction_request` (`wallet` required)
  - billing details for `ccbill`/`stripe`: `email`, `first_name`, `last_name`, `address1`, `city`, `state`, `zip`, `country`
- `metadata` (optional string map)

Response: checkout session with `payment` details, `next_action`
(redirect/solana), and `payment_id`/`subscription_id` once completed. Tier
changes are NOT supported here — if the user already has an active subscription
in the price's tier group the response is `{ "status": "blocked" }` pointing at
`POST /v1/me/subscriptions/{id}/change-tier`.

## 3. Self-service (`/v1/me/*`) and customer treasury (`/v1/customers/*`)

All `/v1/me/*` routes require a delegated customer principal; every operation is
scoped to the token's subject — no `:user_id` appears in any path.

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/me/balance` | Per-currency balance `{ currency, balance_amount }` (amounts in micros). Query: `currency` |
| GET | `/v1/me/transactions` | Ledger transactions, newest first. Query: `currency`, `limit`, `offset` |
| PUT | `/v1/me/settings` | Self-imposed settings: `currency`, `max_spend_per_day`, `max_spend_per_month`, `low_balance_threshold`, `auto_topup_*` |
| GET | `/v1/me/status` | Aggregated premium status: `has_active_subscription`, enriched `subscription`, `next_renewal_at`, `entitlements` |
| GET | `/v1/me/usage` | Usage breakdown for the token's subject |
| GET | `/v1/me/invoices` | List the subject's invoices |
| GET | `/v1/me/invoices/{id}` | One invoice |
| GET | `/v1/me/payments` | One-off payment history. Query: `type` (rail filter), `limit`, `offset` |
| GET | `/v1/me/entitlements/active` | The subject's currently-active entitlements |
| GET | `/v1/me/tier` | THE effective tier in one tier group (or#912): highest tier_rank among products whose entitlements intersect the subject's active windows; `tier: null` when none. Query: `group` (required), `at` (RFC3339, optional). Tier carries the immutable `entitlement` identifier + mutable `display_name` + `tier_rank` + product ref |
| GET | `/v1/me/products` | Products relevant to the subject |
| GET | `/v1/me/products/{product_id}/access` | Whether the subject currently has access to a product |
| GET | `/v1/me/notifications` | Notifications. Query: `limit`, `offset`, `seen` |
| GET | `/v1/me/notifications/unread-count` | `{ unread_count }` |
| POST | `/v1/me/notifications/{id}/read` | Mark one notification read |
| POST | `/v1/me/billing-portal` | Provider billing-portal session `{ url }` (mounted only when a Stripe rail is configured) |

### Subscriptions

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/me/subscriptions` | Subscription history. Query: `status` (`pending`,`active`,`past_due`,`cancelled`,`all`), `limit`, `offset` |
| GET | `/v1/me/subscriptions/{id}` | One subscription with enriched product/price/access (404 if not the caller's) |
| POST | `/v1/me/subscriptions/{id}/cancel` | Cancel. Body `{ "feedback": "..." }` (4-500 chars, required). Returns `202 { "status": "queued" }` on EVERY rail — the cancel is recorded locally and the remote cancel executes as a durable intent (CCBill included; the old portal-only 422 is retired) |
| POST | `/v1/me/subscriptions/{id}/resume` | Resume a cancelled subscription on a reversible rail before period end. `202 { "status": "queued" }`; 400 with a specific reason otherwise |
| POST | `/v1/me/subscriptions/{id}/change-tier` | Unified upgrade/downgrade. Body `{ "price_id": "..." }` (same tier group). See below |
| POST | `/v1/me/subscriptions/{id}/change-tier/preview` | Dry-run of the tier change (proration/effect preview), no mutation |
| PUT | `/v1/me/subscriptions/{id}/payment-method` | Reassign an NMI-backed subscription to another saved method. Body `{ "payment_method_id": "..." }` |

Solana on-chain lifecycle (mounted only when OpenRails has a Solana signer;
prepare → wallet signs → confirm):

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/me/subscriptions/{id}/solana-cancel-tx` | Prepare the on-chain cancel transaction for wallet signing |
| POST | `/v1/me/subscriptions/{id}/solana-cancel` | Confirm the signed on-chain cancel |
| POST | `/v1/me/subscriptions/{id}/solana-tier-change` | Prepare the on-chain tier-change transaction |
| POST | `/v1/me/subscriptions/{id}/solana-tier-change/confirm` | Confirm the signed tier change |

Tier-change response: `{ object: "tier_change", status: "succeeded"|"requires_action"|"blocked", action, price_id, url?, subscription_id?, next_action?, delayed_start?, message? }`.
Stripe/NMI upgrades succeed immediately with proration; downgrades succeed with
a `delayed_start` at period end; CCBill upgrades return `requires_action` with a
redirect `url`, downgrades are `blocked`; Solana tier changes go through the
on-chain prepare/confirm routes above.

### Payment methods

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/me/payment-methods` | List stored methods. Query: `limit`, `offset` |
| POST | `/v1/me/payment-methods` | Create + activate a vault record. Body: `payment_token` (Collect.js) + billing details |
| PUT | `/v1/me/payment-methods/{id}` | Replace the stored vault card (`payment_token` + optional billing fields) |
| DELETE | `/v1/me/payment-methods/{id}` | Soft-delete the method |

### Checkout (delegated)

`POST /v1/me/checkout`, `GET /v1/me/checkout/{id}`,
`POST /v1/me/checkout/{id}/confirm` — same semantics as `/v1/checkout`
(section 2) with the delegated token as the buyer.

### Customer treasury (`/v1/customers/{customer_id}/*`)

The customer-as-payer surface: a customer (any payer, possibly a shared/company
balance) acting on its OWN treasury, addressed by customer id. Handlers are
shared with `/v1/me/*`; the delegated principal must additionally hold the
listed `customer:*` grant for that customer (balances can be shared resources).

Scope (or#916): `{customer_id}` must name the caller's OWN payable subject —
its subject id or durable customer id. The merchant's own coordinates (slug or
merchant id) address the MERCHANT's treasury account and bind only for a
merchant-admin principal (`merchant:*`) on top of the `customer:*` grants.

| Method | Path | Permission |
|---|---|---|
| GET | `/v1/customers/{customer_id}/spend-delegations` | `customer:spend-delegations:read` |
| PUT | `/v1/customers/{customer_id}/spend-delegations` | `customer:spend-delegations:update` — replace the full payer-owned delegation policy |
| PUT | `/v1/customers/{customer_id}/spend-delegations:upsert` | `customer:spend-delegations:update` — upsert one delegation |
| GET | `/v1/customers/{customer_id}/balance` | `customer:balance:read` |
| GET | `/v1/customers/{customer_id}/transactions` | `customer:balance:read` |
| GET | `/v1/customers/{customer_id}/usage` | `customer:balance:read` |
| GET | `/v1/customers/{customer_id}/payments` | `customer:balance:read` |
| GET | `/v1/customers/{customer_id}/invoices` | `customer:balance:read` |
| GET | `/v1/customers/{customer_id}/invoices/{id}` | `customer:balance:read` |
| PUT | `/v1/customers/{customer_id}/settings` | `customer:billing:update` — billing mode (prepaid/arrears) + self-imposed caps |
| GET/POST | `/v1/customers/{customer_id}/payment-methods` | `customer:payment-methods:update` |
| PUT/DELETE | `/v1/customers/{customer_id}/payment-methods/{id}` | `customer:payment-methods:update` |
| POST | `/v1/customers/{customer_id}/billing-portal` | `customer:payment-methods:update` (Stripe rail only) |
| POST | `/v1/customers/{customer_id}/checkout` | `customer:checkout:create` — pre-pay / load credits |
| GET | `/v1/customers/{customer_id}/checkout/{id}` | `customer:checkout:create` |
| POST | `/v1/customers/{customer_id}/checkout/{id}/confirm` | `customer:checkout:create` |

`/status` is deliberately not mounted here — it reports consumer concepts a
payer does not own.

## 4. Merchant machine surface (`/v1/merchant/*`, `/v1/import/*`)

Server-to-server billing operations. Every route is gated on the listed
`merchant:*` permission regardless of credential type.

| Method | Path | Permission | Purpose |
|---|---|---|---|
| POST | `/v1/merchant/customers/entitlements:batch` | `merchant:customer-settings:read` | Batch entitlement lookup by external subject |
| GET | `/v1/merchant/customers/{customer_id}/entitlements` | `merchant:customer-settings:read` | Active entitlements for a customer. Query: `at` (RFC3339) for point-in-time |
| PUT | `/v1/merchant/customers/{customer_id}/spend-delegations` | `merchant:customer-settings:update` | Replace the customer's full spend-delegation policy |
| PUT | `/v1/merchant/customers/{customer_id}/spend-delegations:upsert` | `merchant:customer-settings:update` | Upsert one delegation |
| GET | `/v1/merchant/entitlements/{entitlement}/customers` | `merchant:customer-settings:read` | Customers currently holding an entitlement |
| GET | `/v1/merchant/users/{user_id}/product-access` | `merchant:customer-settings:read` | A user's product access |
| GET | `/v1/merchant/invokers/{invoker}/credits` | `merchant:customer-settings:read` | Invoker credit summary `{ currency, balance, held_balance }`. Query: `customer_id`, `currency` |
| POST | `/v1/merchant/admissions` | `merchant:admissions:create` | Pre-authorize spend / place holds; returns the durable admission id. Idempotent per `(customer_id, credit_type, source, source_id)` |
| POST | `/v1/merchant/admissions/{id}/capture` | `merchant:admissions:create` | Capture a hold: `{ amount }` (≤ hold) |
| POST | `/v1/merchant/admissions/{id}/release` | `merchant:admissions:create` | Release a hold without spending |
| POST | `/v1/merchant/wasted-spend` | `merchant:admissions:create` | Report wasted spend against admissions |
| POST | `/v1/merchant/usage/report` | `merchant:admissions:create` | Record usage events |
| POST | `/v1/merchant/usage/rollup` | `merchant:usage:read` | Usage rollup query |
| POST | `/v1/merchant/usage/resource-revenue` | `merchant:usage:read` | Resource-revenue query |
| GET | `/v1/merchant/settings` | `merchant:settings:read` | Merchant billing settings |
| PUT | `/v1/merchant/settings` | `merchant:settings:update` | Update merchant billing settings, incl. `billing_policies` + `billing_policy_bindings` ([billing-policies.md](../billing-policies.md)) |
| GET | `/v1/merchant/api-host` | `merchant:settings:read` | The merchant's canonical API host (#734 Host routing); `api_host` null when unset |
| PUT | `/v1/merchant/api-host` | `merchant:settings:update` | Assign the canonical API host: `{ api_host }` (bare lowercase hostname; `""` clears). Owner-only in the fixed role catalog; 409 when taken by another merchant |
| GET | `/v1/merchant/trust-level` | `merchant:customer-settings:read` | Customer trust level |
| GET | `/v1/merchant/credit-limit` | `merchant:customer-settings:read` | Read a customer's credit limit |
| PUT | `/v1/merchant/credit-limit` | `merchant:customer-settings:update` | Set a customer's credit limit |
| GET | `/v1/merchant/delinquency` | `merchant:customer-settings:read` | Arrears delinquency roster (grace + delinquent, oldest debt first) plus the effective policy. `?state=grace\|delinquent`, `?limit=`. See [arrears-delinquency.md](../arrears-delinquency.md) |
| GET | `/v1/merchant/customers/{customer_id}/delinquency` | `merchant:customer-settings:read` | One payer's delinquency state per currency; empty = never overdue |
| GET | `/v1/merchant/credits/balance` | `merchant:customer-settings:read` | Credit balance |
| POST | `/v1/merchant/credits/deposit` | `merchant:customer-settings:update` | Deposit/grant credits: `{ customer_id, invoker, currency, amount, source, source_id, expires_at?, description? }`. `source_id` (any non-empty string) is REQUIRED and is the caller's reproducible idempotency key: once-only per `(customer_id, source_id)` is a database fact; `source` is a label, NOT part of the key. Identical replay → same grant with `Replayed=true`; replay with a different `amount` → 409 `idempotency_key_reused` |
| GET | `/v1/merchant/credits/deposit` | `merchant:customer-settings:read` | What did this deposit key do (or#906): `?customer_id=&source_id=` → the committed grant (id, amount, created_at, `Replayed=true`); 404 `deposit_not_found` when the key never committed |
| POST | `/v1/import/billing` | `merchant:billing:import` | Bulk DeclaredBilling book import (subscriptions/payments/payment methods wholesale) — a distinct owner-level grant |

## 5. Merchant admin (human) routes

Same `/v1/merchant` prefix and permission gate; these are the console/support
surface. The merchant admin console SPA (when enabled and built) is served at
`GET /admin/`, and the selected AuthKit control-plane route groups (login,
tokens, membership) are mounted under `/auth/*` — see AuthKit's own reference
for those routes.

### Customers & support

| Method | Path | Permission | Purpose |
|---|---|---|---|
| GET | `/v1/merchant/customers` | `merchant:customer-settings:read` | Customer list/search |
| GET | `/v1/merchant/customers/{customer_id}` | `merchant:customer-settings:read` | Full billing profile: trust, balances, entitlements, history, redacted payment-method metadata |
| GET | `/v1/merchant/customers/{customer_id}/payment-methods` | `merchant:customer-settings:read` | Redacted saved-method metadata (admins can never create/update/delete customer methods) |
| GET | `/v1/merchant/customers/{customer_id}/payments` | `merchant:payments:read` | One customer's payment history |
| POST | `/v1/merchant/customers/{customer_id}/payments/off-channel` | `merchant:customer-settings:update` | Record an off-channel/manual purchase through the normal purchase path |
| POST | `/v1/merchant/customers/{customer_id}/entitlements` | `merchant:customer-settings:update` | Manually grant an entitlement (grant ledger) |
| DELETE | `/v1/merchant/customers/{customer_id}/entitlements/{id}` | `merchant:customer-settings:update` | Revoke a manual entitlement grant |
| POST | `/v1/merchant/customers/{customer_id}/product-access` | `merchant:customer-settings:update` | Manually grant product access |
| DELETE | `/v1/merchant/customers/{customer_id}/product-access/{id}` | `merchant:customer-settings:update` | Revoke a manual product-access grant |
| POST | `/v1/merchant/customers/{customer_id}/credits` | `merchant:credits:grant` | Grant credits (or#906): `{ currency, amount, source_id, invoker?, source?, expires_at?, description? }` — the human-admin deposit. `source_id` is the reproducible idempotency key (same semantics as the machine deposit above); `source` defaults to `admin`, `invoker` to the customer id. Owner-level permission (NOT held by the fixed support role); rate-limited as an admin grant operation |

### Payments & subscriptions

| Method | Path | Permission | Purpose |
|---|---|---|---|
| GET | `/v1/merchant/payments` | `merchant:payments:read` | List payments with filters |
| GET | `/v1/merchant/payments/{id}` | `merchant:payments:read` | One payment with refund history |
| POST | `/v1/merchant/payments/{id}/refunds` | `merchant:payments:refund` | Refund through the rail; `revoke_access` must be explicit to also revoke one-off access |
| GET | `/v1/merchant/subscriptions` | `merchant:subscriptions:read` | List subscriptions with filters |
| GET | `/v1/merchant/subscriptions/{id}` | `merchant:subscriptions:read` | One subscription |
| POST | `/v1/merchant/subscriptions/{id}/cancel` | `merchant:subscriptions:update` | Cancel; `revoke_access` must be explicit to revoke entitlements immediately |
| POST | `/v1/merchant/subscriptions/{id}/resume` | `merchant:subscriptions:update` | Resume where the rail supports it |
| PUT | `/v1/merchant/subscriptions/{id}/payment-method` | `merchant:subscriptions:update` | Reassign to another saved method of the same customer |
| POST | `/v1/merchant/subscriptions/{id}/reprice` | `merchant:subscriptions:update` | Schedule one subscription's price move at its next renewal on/after `effective_at` |

### Reprices & plan migrations

| Method | Path | Permission | Purpose |
|---|---|---|---|
| POST | `/v1/merchant/catalog/reprice-all-prior-versions` | `merchant:subscriptions:update` | Bulk-reprice all subscriptions on prior versions of a price key |
| GET | `/v1/merchant/catalog/reprice-all-prior-versions/preview` | `merchant:subscriptions:read` | Read-only affected-count dry run |
| GET | `/v1/merchant/reprices` | `merchant:subscriptions:read` | List scheduled reprices |
| GET | `/v1/merchant/reprices/batches` | `merchant:subscriptions:read` | Bulk reprice batches for a price key |
| GET | `/v1/merchant/reprices/{id}` | `merchant:subscriptions:read` | One reprice |
| POST | `/v1/merchant/reprices/{id}/cancel` | `merchant:subscriptions:update` | Cancel a pending reprice |
| POST | `/v1/merchant/plan-migrations` | `merchant:subscriptions:update` | Cross-product bulk plan retirement (plan A → plan B) |
| POST | `/v1/merchant/plan-migrations/preview` | `merchant:subscriptions:read` | Dry-run preview |
| GET | `/v1/merchant/plan-migrations/{id}` | `merchant:subscriptions:read` | One migration |
| POST | `/v1/merchant/plan-migrations/{id}/cancel` | `merchant:subscriptions:update` | Cancel a migration |

### Catalog (`/v1/merchant/catalog`)

Reads need `merchant:catalog:read`; writes need `merchant:catalog:update`. In
`merchant_source=manifest` deployments (mode 1, YAML-is-truth) every catalog and
payment-provider WRITE answers `405` with code `manifest_driven` — edit the
manifest and reboot instead. Reads stay live.

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/merchant/catalog/products` | Create a product: at least `{ key, display_name }`, optionally `entitlements_spec`, `credits_spec` |
| GET | `/v1/merchant/catalog/products` | List products |
| GET | `/v1/merchant/catalog/products/{id}` | One product |
| GET | `/v1/merchant/catalog/products/by-key/{key}` | Product by catalog key |
| PATCH | `/v1/merchant/catalog/products/{id}` | Update definition fields |
| POST | `/v1/merchant/catalog/products/{id}/activate` | Activate |
| POST | `/v1/merchant/catalog/products/{id}/deactivate` | Deactivate |
| POST | `/v1/merchant/catalog/prices` | Create a price with per-PSP links (`psp_links`: link existing provider ids or select declarative provider config; recurring Solana defaults to USDC, accepts `token: USD1`, or resolves an attached `plan_pda`) |
| GET | `/v1/merchant/catalog/prices` | List prices |
| GET | `/v1/merchant/catalog/prices/by-key/{key}` | Price by key |
| GET | `/v1/merchant/catalog/prices/by-key/{key}/history` | The key's version chain, most-recent-first |
| GET | `/v1/merchant/catalog/prices/{id}` | One price |
| PATCH | `/v1/merchant/catalog/prices/{id}` | Update links / `archived` flag |
| POST | `/v1/merchant/catalog/prices/{id}/activate` | Activate |
| POST | `/v1/merchant/catalog/prices/{id}/deactivate` | Deactivate |
| POST | `/v1/merchant/catalog/prices/{id}/key` | Relabel a price's key (version-bump repoint on collision) |
| GET | `/v1/merchant/catalog/drift` | List catalog↔provider drift (the pull reconciliation is alert-only, never mutating) |
| POST | `/v1/merchant/catalog/drift/refresh` | Refresh drift detection |
| POST | `/v1/merchant/catalog/publish` | Push OpenRails definitions to providers |
| POST | `/v1/merchant/catalog/ask` | Catalog copilot Q&A (read permission; never mutates) |
| POST | `/v1/merchant/catalog/copilot/confirm` | Log a copilot draft as confirmed (write permission; audit log only, exempt from the manifest guard) |

### Payment providers (`/v1/merchant/payment-providers`)

Reads: `merchant:payment-providers:read`; writes: `merchant:payment-providers:update`
(writes are mounted only when the deployment can persist secrets, and are
manifest-guarded like catalog writes).

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/merchant/payment-providers` | List configured providers |
| GET | `/v1/merchant/payment-providers/{provider}` | One provider's config (redacted) |
| PUT | `/v1/merchant/payment-providers/{provider}` | Create/update provider config + secrets |
| DELETE | `/v1/merchant/payment-providers/{provider}` | Remove provider config |
| POST | `/v1/merchant/payment-providers/routing/dry-run` | Explain which PSP a checkout would get, and why every other candidate was skipped. Read permission — creates nothing (or#288) |

### Metrics, dashboard, alerts, notifications

| Method | Path | Permission | Purpose |
|---|---|---|---|
| POST | `/v1/merchant/metrics/query` | `merchant:metrics:read` | Composable metrics query (Postgres-backed) |
| GET | `/v1/merchant/metrics/schema` | `merchant:metrics:read` | Metric registry / schema doc |
| POST | `/v1/merchant/metrics/ask` | `merchant:metrics:read` | Natural-language metrics Q&A (rate-limited, consent-gated) |
| GET | `/v1/merchant/dashboard` | `merchant:metrics:read` | Saved dashboard config |
| PUT | `/v1/merchant/dashboard` | `merchant:dashboard:update` | Replace dashboard config |
| POST | `/v1/merchant/dashboard/widgets/generate` | `merchant:dashboard:update` | NL widget generation |
| GET | `/v1/merchant/alerts/templates` | `merchant:metrics:read` | Alert rule templates |
| GET | `/v1/merchant/alerts/rules` | `merchant:metrics:read` | List alert rules |
| POST | `/v1/merchant/alerts/rules` | `merchant:settings:update` | Create rule |
| PATCH | `/v1/merchant/alerts/rules/{id}` | `merchant:settings:update` | Update rule |
| DELETE | `/v1/merchant/alerts/rules/{id}` | `merchant:settings:update` | Delete rule |
| POST | `/v1/merchant/alerts/rules/{id}/test` | `merchant:settings:update` | Test-fire a rule |
| GET | `/v1/merchant/webhooks` | `merchant:metrics:read` | List outbound alert webhooks (distinct from inbound provider webhooks) |
| POST | `/v1/merchant/webhooks` | `merchant:settings:update` | Create outbound webhook |
| DELETE | `/v1/merchant/webhooks/{id}` | `merchant:settings:update` | Delete outbound webhook |
| GET | `/v1/merchant/notifications` | `merchant:metrics:read` | Merchant notification feed |
| GET | `/v1/merchant/notifications/unread-count` | `merchant:metrics:read` | Unread count |
| POST | `/v1/merchant/notifications/{id}/read` | `merchant:settings:update` | Mark read |

### Operations: repair, findings, worker health

| Method | Path | Permission | Purpose |
|---|---|---|---|
| GET | `/v1/merchant/repair-alerts` | `merchant:repair-alerts:read` | Merchant repair alerts |
| GET | `/v1/merchant/worker-health` | `merchant:repair-alerts:read` | Background-worker health dashboard |
| GET | `/v1/merchant/findings` | `merchant:repair-alerts:read` | Operator findings queue |
| GET | `/v1/merchant/findings/{id}` | `merchant:repair-alerts:read` | One finding |
| POST | `/v1/merchant/findings/{id}/resolve` | `merchant:findings:resolve` | Execute a finding's recommendation (cancel/refund/revoke/grant) — one at a time, no bulk endpoint |

### API keys & team (owner-only, via AuthKit control plane)

| Method | Path | Permission | Purpose |
|---|---|---|---|
| POST | `/v1/merchant/api-keys` | `merchant:credentials:manage` | Mint a scoped API key (permissions can never exceed the caller's) |
| GET | `/v1/merchant/api-keys` | `merchant:credentials:manage` | List keys |
| DELETE | `/v1/merchant/api-keys/{id}` | `merchant:credentials:manage` | Revoke a key |
| GET | `/v1/merchant/team` | `merchant:members:read` | Team roster |
| GET | `/v1/merchant/team/invites` | `merchant:members:read` | Pending invites |
| POST | `/v1/merchant/team/invites` | `merchant:members:manage` | Invite a member (register/join links) |
| DELETE | `/v1/merchant/team/invites/{id}` | `merchant:members:manage` | Revoke an invite |
| PATCH | `/v1/merchant/team/{user_id}` | `merchant:members:manage` | Change a member's role |
| DELETE | `/v1/merchant/team/{user_id}` | `merchant:members:manage` | Remove a member |

On deployments without a control plane these routes stay mounted but answer 501.

### Platform operator (`/v1/platform`, standalone only)

Human operator sessions checked against the root permission group; no API keys,
no merchant context.

| Method | Path | Permission | Purpose |
|---|---|---|---|
| GET | `/v1/platform/merchants` | `root:merchants:read` | Cross-merchant directory |
| GET | `/v1/platform/merchants/{id}` | `root:merchants:read` | One merchant |
| DELETE | `/v1/platform/merchants/{id}` | `root:merchants:delete` | Soft-delete a merchant |
| POST | `/v1/platform/merchants/{id}/restore` | `root:merchants:restore` | Restore a soft-deleted merchant |

## 6. Webhooks (inbound, per rail)

No bearer auth — each request is verified by provider signature or source IP
AFTER merchant resolution (the signature, not the router, is the trust
boundary). Success returns `200 { "status": "accepted" }`.

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/webhooks/{rail}` | The standalone surface: NMI-backed rails / CCBill; the merchant is derived from the payload's account identity |
| POST | `/v1/webhooks/{rail}/{account_id}` | Same, with the receiving PSP account pinned in the path (direct Stripe; multi-account rails) |
| POST | `/billing/v1/merchants/{merchant}/webhooks/{rail}` | Embedded only: the host pins one merchant, so the `{merchant}` slug resolves it and THAT merchant's signing secret verifies the payload |
| POST | `/billing/v1/merchants/{merchant}/webhooks/{rail}/{account_id}` | Embedded only, per-account (e.g. multiple NMI accounts) |

`{rail}` is the gateway KIND — `nmi`, `ccbill`, `stripe`, `solana`,
`basistheory`. It is never a PSP key: `mobius` and `paykings` both post to
`/v1/webhooks/nmi` and are told apart by `{account_id}` or the payload's own
account identity.

Deployments using per-merchant hostnames (`api.<slug>.<domain>`) additionally
serve `/v1/webhooks/{rail}[/{account_id}]` with the merchant resolved from
the Host header.

Verification per rail:

- **NMI** (`/v1/webhooks/nmi`): JSON body; `Webhook-Signature`
  (`t=...,s=...`) — the one header NMI sends, and the only one read.
  Test mode (config) bypasses the check.
- **CCBill**: form-encoded; verified via CCBill's published source-IP ranges
  (unless test mode), plus `formName`/`flexId` validated against price metadata.
- **Stripe**: JSON body; `Stripe-Signature` with the configured endpoint secret.

Unknown providers return 400; verification failures return 401/403; an unknown
`{merchant}` slug returns 404 and never falls back to a default merchant.
