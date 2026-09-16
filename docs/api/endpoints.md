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
| Self-service `/v1/me/*`, customer treasury `/v1/customers/*` | `Authorization: DPoP <delegated JWT>` plus per-request `DPoP` proof (native: Bearer plus matching TLS client certificate) — short-lived token minted by the merchant's registered issuer with `delegated_sub` (embedded mode: the host's user bearer adapted to the same principal) |
| Checkout `/v1/checkout` | any authenticated user bearer |
| Merchant `/v1/merchant/*`, `/v1/import/*` | `Authorization: Bearer <API key (openrails_st_…) | service JWT | user access token>` — every route is gated on a `merchant:*` permission, not on credential type |
| Platform `/v1/platform/*` | human operator session checked against root-group grants (standalone only) |
| Webhooks | provider signature / source-IP verification, no bearer |

Merchant permissions: API keys carry the permissions they were minted with;
service JWTs (`token_use=service`, max 15-min lifetime, signed by a registered
issuer) carry a self-asserted `permissions` claim scoped to the issuer's
merchant; human sessions are checked against the user's merchant-group role.
The required permission is listed per route below. Delegated merchant requests
use the same DPoP or native certificate profile as self-service requests.

### Idempotency-Key

Idempotency is an explicit operation contract, not a generic HTTP response
cache. Send `Idempotency-Key` only on routes that document it (for example,
checkout, invoice collection, and money operations). Those operations retain
their own durable receipts and conflict rules. Every HTTP attempt authenticates,
resolves its merchant, and authorizes against current authority. Credentials and
admin responses are never replayed by global middleware.

## 1. Public routes

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/` | none | JSON service banner `{"service":"billing","status":"ok",...}` |
| GET | `/health/live` (alias `/healthz`) | none | Unconditional liveness probe |
| GET | `/health/ready` (alias `/readyz`) | none | Readiness: Postgres, configured Redis, merchant-secret backend, River producer/local consumer, auth verifier. 200 or 503 `not_ready`; `?verbose=1` adds per-dependency detail. `run-server --no-workers` remains not-ready |
| GET | `/v1/capabilities` | none | Static capability document: `route_groups` (which route sets are mounted) + `routes` (provider-specific toggles: `billing_portal`, `solana`, `solana_signing`, `webhooks`, `secret_write`). ETagged, `Cache-Control: public, max-age=300` |
| GET | `/v1/captcha/status` | none | Captcha challenge status for the browser tier |
| GET | `/v1/captcha/client.js` | none | Captcha client script |
| GET | `/v1/products` | optional | List products with embedded active prices. Query: `limit` (1-100, default 20), `offset` |
| GET | `/v1/prices` | optional | List prices. Query: `currency`, `product` (`prod_` ID or raw UUID), `type` (`recurring`/`one_time`), `limit`, `offset` |
| GET | `/v1/currencies` | none | The currency scale registry: `{object:"currencies", currencies:[{code, decimals, minor_decimals}]}`. Every monetary string on the wire is in native units (`10^decimals` per major unit); providers settle in `10^minor_decimals`. System-fixed, merchant-independent; `openrails.Currencies()` is the same table in Go |
| GET | `/v1/checkout-config` | none | Per-merchant checkout discovery: the merchant's **armed** PSPs as `{key, rail, display_name, flow, config}`, where `key` is checkout's `payment.rail` value, `flow` is `tokenize`/`redirect`/`wallet`, and `config` carries only public-by-nature values (NMI `tokenization_key` + `tokenization_url`; Basis Theory `public_api_key`). Merchant resolved from `Host`. ETagged, `Cache-Control: public, max-age=60`. Serves a fixed per-rail whitelist — no merchant secret can appear. When a Solana PSP is armed, `solana` carries `{network, chain, preferred_token, tokens[]}` (the same acceptance policy as `/v1/solana/config`) |
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
  - billing details for `ccbill`: canonical `name_on_card`, `zip`, and ISO-3166 alpha-2 `country`; `address1`, `city`, and `state` are optional, and the verified email comes from the authenticated identity rather than this payload. Legacy `first_name`/`last_name` aliases remain accepted
  - Stripe hosted Checkout collects its own email and billing address; those fields are not required in this request
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
| PUT | `/v1/me/collection-payment-method` | Choose the saved method for automatic invoice collection in one currency. Body: `currency`, `payment_method_id`. The method must belong to the payer and support saved-method charges; otherwise `400` |
| GET | `/v1/me/status` | Aggregated premium status: `has_active_subscription`, enriched `subscription`, `next_renewal_at`, `entitlements` |
| GET | `/v1/me/usage` | Usage breakdown for the token's subject |
| GET | `/v1/me/spend-limits` | The spend windows the AUTHENTICATED INVOKER is enforced against at admission, with live metering: `{ currency, invoker, windows: [{ scope, key, window_seconds, limit, currency, used, reserved, remaining, resets_at }] }`. Query: `currency` (required). Windows are estimate-based, so `used` already includes in-flight reservations and `reserved` names that part (what a release hands back); `resets_at` is the window's real staggered boundary. Self-scoped by construction — both the payer account and the invoker come from the credential, and naming another subject (`invoker`, `customer_id`, `scope_key`, `subject`) is refused `400 spend_scope_not_addressable`. The payer's admin view of every delegation it granted stays on `GET /v1/customers/{id}/spend-delegations` |
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
on-chain prepare/confirm routes above. An NMI upgrade whose provider outcome is
unresolved answers `409` (retry with the same `Idempotency-Key` to read the
durable result); a second upgrade of a subscription with an unresolved upgrade
also answers `409`. Checkout confirmation uses the same `409` for an unresolved
sale.

### Payment methods

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/me/payment-methods` | List stored methods with currency-qualified collection defaults. Query: `limit`, `offset` |
| POST | `/v1/me/payment-methods` | Store an NMI card. Body: `payment_token` (Collect.js) + billing details |
| PUT | `/v1/me/payment-methods/{id}` | Durably replace an NMI card. Requires tokenization's `payment_token`, `last_four`, `card_type`, and `expiry_date`; billing fields are optional. Returns the updated method when confirmed, `202` with no body while converging, `409 payment_method_update_retry_required` when a fresh token is required, or `502 payment_method_update_failed` for a terminal provider conflict. |
| DELETE | `/v1/me/payment-methods/{id}` | Delete an NMI method through the durable provider-aware workflow. Returns `204` when complete, `202` while provider convergence continues, and an error when refused/failed. Stripe cards are managed through Stripe Billing Portal. |

Saved-method lists and merchant customer profiles include
`collection_default_currencies` on each method that is the current invoice
collection choice for those billing currencies (for example, `["EUR", "USD"]`).
An absent or empty list indicates no collection-default badge for that method.
Only the explicit choice set through `PUT /v1/me/collection-payment-method`
counts; no other saved method is inferred. It does not change provider-managed
subscription defaults or select a checkout method.

The admin payment-method card labels each currency separately. Refresh payment
methods invalidates both the customer profile and saved-method query caches.
Completed customer-initiated deletion removes the local method and clears its
collection choice through foreign keys; no replacement method is selected.

### Checkout (delegated)

Same semantics as `/v1/checkout` (section 2), with the delegated token as the
buyer.

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/me/checkout` | Create a checkout session |
| GET | `/v1/me/checkout/{id}` | Retrieve the caller's checkout session |
| POST | `/v1/me/checkout/{id}/confirm` | Confirm the caller's Solana checkout session |

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
| DELETE | `/v1/customers/{customer_id}/spend-delegations/{scope}/{scope_key}` | `customer:spend-delegations:update` — revoke exactly one delegation (or#911); siblings untouched; 404 when nothing exists at the key |
| GET | `/v1/customers/{customer_id}/balance` | `customer:balance:read` |
| GET | `/v1/customers/{customer_id}/transactions` | `customer:balance:read` |
| GET | `/v1/customers/{customer_id}/usage` | `customer:balance:read` |
| GET | `/v1/customers/{customer_id}/payments` | `customer:balance:read` |
| GET | `/v1/customers/{customer_id}/invoices` | `customer:balance:read` |
| GET | `/v1/customers/{customer_id}/invoices/{id}` | `customer:balance:read` |
| PUT | `/v1/customers/{customer_id}/collection-payment-method` | `customer:billing:update` — invoice collection method per currency |
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
| PUT | `/v1/merchant/customers/{customer_id}` | `merchant:customer-settings:update` | Materialize or touch the customer record (`Client.EnsureCustomer`); returns `{id, created_at, last_seen_at}` |
| GET | `/v1/merchant/customers/{customer_id}/entitlements` | `merchant:customer-settings:read` | Active entitlements for a customer. Query: `at` (RFC3339) for point-in-time |
| PUT | `/v1/merchant/customers/{customer_id}/spend-delegations` | `merchant:customer-settings:update` | Replace the customer's full spend-delegation policy |
| PUT | `/v1/merchant/customers/{customer_id}/spend-delegations:upsert` | `merchant:customer-settings:update` | Upsert one delegation |
| DELETE | `/v1/merchant/customers/{customer_id}/spend-delegations/{scope}/{scope_key}` | `merchant:customer-settings:update` | Revoke exactly one delegation (or#911); siblings untouched; 404 when nothing exists at the key |
| GET | `/v1/merchant/entitlements/{entitlement}/customers` | `merchant:customer-settings:read` | Customers currently holding an entitlement |
| GET | `/v1/merchant/users/{user_id}/product-access` | `merchant:customer-settings:read` | A user's product access |
| GET | `/v1/merchant/invokers/{invoker}/credits` | `merchant:customer-settings:read` | Invoker credit summary `{ currency, balance, held_balance }`. Query: `customer_id`, `currency` |
| POST | `/v1/merchant/checkout-sessions` | `merchant:checkout:create` | Create a checkout for the supplied customer identity; required Idempotency-Key header |
| GET | `/v1/merchant/checkout-sessions/{id}` | `merchant:customer-settings:read` | Read a checkout owned by query customer_id |
| POST | `/v1/merchant/checkout-sessions/{id}/confirm` | `merchant:checkout:create` | Confirm the checkout for the supplied customer_id |
| GET | `/v1/merchant/checkout-options` | `merchant:customer-settings:read` | Locally ready providers for query price_id; no provider request |
| GET | `/v1/merchant/checkout-config` | `merchant:customer-settings:read` | Armed PSPs, their public browser values and the Solana acceptance policy for the credential's merchant |
| GET | `/v1/merchant/customers/{customer_id}/effective-tier` | `merchant:customer-settings:read` | Active tier for query group; null when none |
| POST | `/v1/merchant/admissions` | `merchant:admissions:create` | Pre-authorize spend / place holds; returns the durable admission id. Idempotent per `(customer_id, credit_type, source, source_id)`. An item with `estimated_amount > 0` places a hold and MUST carry `expires_at` (RFC3339): the deadline of the job the hold covers. There is no default lifetime — the hold lives until captured, released, extended, or that deadline |
| POST | `/v1/merchant/admissions/{id}/capture` | `merchant:admissions:create` | Capture a hold: `{ amount }`. Idempotent on the path `{id}` unconditionally (or#907); an identical retry answers `Replayed: true`, a changed amount is refused 409 `idempotency_key_reused` |
| POST | `/v1/merchant/admissions/{id}/release` | `merchant:admissions:create` | Release a hold without spending |
| POST | `/v1/merchant/admissions/{id}/extend` | `merchant:admissions:create` | Re-declare a live hold's deadline: `{ expires_at }` (RFC3339). A hold lives exactly as long as its admit declared (`expires_at` is required with `estimated_amount`); a still-running job extends before that or loses it. 404 `hold_not_found` when nothing live exists — re-admit, a lapsed hold is never resurrected |
| POST | `/v1/merchant/wasted-spend` | `merchant:admissions:create` | Report wasted spend against admissions |
| POST | `/v1/merchant/usage/report` | `merchant:admissions:create` | Record usage events |
| POST | `/v1/merchant/provider-operations` | `merchant:admissions:create` | Open a durable provider-operation authorization (#1004): `{ operation_id, payer, record_owner, authorized_usd_micros, claim_reference, authorization_body, authorization_body_sha256 }`. Exact replay → `replayed=true`; a changed field → 409 `operation_authorization_conflict` with `param`; 402 `insufficient_credits` |
| GET | `/v1/merchant/provider-operations/{operation_id}` | `merchant:usage:read` | Read one authorization; 404 `operation_authorization_not_found`. Path-escape the id |
| POST | `/v1/merchant/provider-operations/{operation_id}/release` | `merchant:admissions:create` | Release after proven provider non-creation: `{ release_reference }`. 409 `operation_authorization_has_billing_evidence` once any evidence exists |
| POST | `/v1/merchant/provider-operations/{operation_id}/observations` | `merchant:admissions:create` | Append immutable provider billing evidence (lifecycle facts, raw body, typed records or refusal); unknown fields are refused. OpenRails qualifies, rates and settles; there is no caller-rated amount. Spend authority because an eligible observation settles the payer's reservation. Encoded body ≤ 768 KiB (`ProviderBillingObservationMaxBytes`, checked on the shared service path and mirrored by the remote Client, so over-cap is `400 invalid_param` in every deployment) |
| GET | `/v1/merchant/provider-operations/{operation_id}/qualification` | `merchant:usage:read` | Qualification state with its authorization; 404 `provider_billing_qualification_not_found` |
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
| POST | `/v1/import/billing` | `merchant:billing:import` | Declared billing facts (`Client.ImportBilling`): customers, payment methods, subscriptions, transactions and admin comps (`admin_grants`), idempotent by source id — a distinct owner-level grant |

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
| GET | `/v1/merchant/customers/{customer_id}` | `merchant:customer-settings:read` | Full billing profile: trust, balances, entitlements, subscriptions of every status (newest 100, with `status`), history, redacted payment-method metadata. Sections degrade independently: a failed collection-defaults read logs and returns the methods without `collection_default_currencies` instead of failing the profile |
| GET | `/v1/merchant/customers/{customer_id}/payment-methods` | `merchant:customer-settings:read` | Redacted saved-method metadata (admins can never create/update/delete customer methods) |
| DELETE | `/v1/merchant/customers/{customer_id}/payment-methods/{id}` | `merchant:customer-settings:update` | Shared customer ownership and provider deletion; 204 completed or 202 pending reconciliation |
| GET | `/v1/merchant/customers/{customer_id}/payments` | `merchant:payments:read` | One customer's payment history |
| GET | `/v1/merchant/customers/{customer_id}/credits` | `merchant:customer-settings:read` | Credit-grant lots, including remaining and expired amounts |
| DELETE | `/v1/merchant/customers/{customer_id}/credits/{grant_id}` | `merchant:credits:revoke` | Revoke the unspent remainder of one credit grant |
| GET | `/v1/merchant/customers/{customer_id}/credit-transactions` | `merchant:customer-settings:read` | Paginated credit ledger. Query: `currency`, `limit`, `offset` |
| POST | `/v1/merchant/customers/{customer_id}/payments/off-channel` | `merchant:customer-settings:update` | Record an off-channel/manual purchase through the normal purchase path |
| POST | `/v1/merchant/customers/{customer_id}/entitlements` | `merchant:customer-settings:update` | Manually grant an entitlement (grant ledger) |
| DELETE | `/v1/merchant/customers/{customer_id}/entitlements/{id}` | `merchant:customer-settings:update` | Revoke a manual entitlement grant |
| POST | `/v1/merchant/customers/{customer_id}/product-access` | `merchant:customer-settings:update` | Manually grant product access |
| DELETE | `/v1/merchant/customers/{customer_id}/product-access/{id}` | `merchant:customer-settings:update` | Revoke a manual product-access grant |
| POST | `/v1/merchant/customers/{customer_id}/credits` | `merchant:credits:grant` | Grant credits (or#906): `{ currency, amount, source_id, invoker?, source?, expires_at?, description? }` — the human-admin deposit. `source_id` is the reproducible idempotency key (same semantics as the machine deposit above); `source` defaults to `admin`, `invoker` to the customer id. Owner-level permission (NOT held by the fixed support role); rate-limited as an admin grant operation |
| GET | `/v1/merchant/customers/{customer_id}/invoice-profile` | `merchant:customer-settings:read` | Read invoicing terms, tax facts, contacts and memo |
| PUT | `/v1/merchant/customers/{customer_id}/invoice-profile` | `merchant:customer-settings:update` | Replace the profile used for future invoice snapshots |
| GET | `/v1/merchant/customers/{customer_id}/rate-overrides` | `merchant:customer-settings:read` | List the payer's negotiated meter rate cards |
| PUT | `/v1/merchant/customers/{customer_id}/rate-overrides/{meter_key}` | `merchant:customer-settings:update` | Set the payer's negotiated rate and optional included allowance |
| DELETE | `/v1/merchant/customers/{customer_id}/rate-overrides/{meter_key}` | `merchant:customer-settings:update` | Restore the merchant-default rate for future usage |

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
| POST | `/v1/merchant/subscriptions/{id}/change-tier` | `merchant:subscriptions:update` | Apply a same-group tier change. Body `{ "price_id": "..." }` |
| POST | `/v1/merchant/subscriptions/{id}/change-tier/preview` | `merchant:subscriptions:update` | Preview the same tier change without mutation |
| PUT | `/v1/merchant/subscriptions/{id}/payment-method` | `merchant:subscriptions:update` | Reassign to another saved method of the same customer |
| POST | `/v1/merchant/subscriptions/{id}/reprice` | `merchant:subscriptions:update` | Schedule one subscription's price move at its next renewal on/after `effective_at` |

### Invoice administration

Full request and state-transition details are in
[invoice-administration.md](../invoice-administration.md).

| Method | Path | Permission | Purpose |
|---|---|---|---|
| GET | `/v1/merchant/invoices` | `merchant:invoices:read` | List invoices with customer, currency, status and period filters |
| GET | `/v1/merchant/invoices/{id}` | `merchant:invoices:read` | Read one invoice and its available actions |
| GET | `/v1/merchant/invoices/{id}/payments` | `merchant:invoices:read` | List collection and remittance history |
| POST | `/v1/merchant/invoices/{id}/payments` | `merchant:invoices:update` | Record an idempotent external remittance; does not charge a provider |
| POST | `/v1/merchant/invoices/{id}/retry-collection` | `merchant:invoices:collect` | Start or replay one durable collection operation with an explicit saved method and idempotency key (202 while unresolved) |
| POST | `/v1/merchant/invoices/{id}/uncollectible` | `merchant:invoices:update` | Stop collection while retaining the debt |
| POST | `/v1/merchant/invoices/{id}/void` | `merchant:invoices:update` | Void an eligible invoice and write off its remaining debt |

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
| POST | `/v1/merchant/catalog/products` | Create a product: at least `{ key, display_name }`, optionally `entitlements_spec` |
| GET | `/v1/merchant/catalog/products` | Paginated products; `tier_group` and `archived` (`false` live only, `true` archived only, absent both) filter before count/pagination |
| GET | `/v1/merchant/catalog/products/{id}` | One product |
| GET | `/v1/merchant/catalog/products/by-key/{key}` | Product by catalog key |
| PATCH | `/v1/merchant/catalog/products/{id}` | Update definition fields |
| POST | `/v1/merchant/catalog/products/{id}/activate` | Activate |
| POST | `/v1/merchant/catalog/products/{id}/deactivate` | Deactivate |
| POST | `/v1/merchant/catalog/prices` | Create a price with per-PSP links (`psp_links`: link existing provider ids or select declarative provider config; recurring Solana defaults to USDC, accepts `token: USD1`, or resolves an attached `plan_pda`) |
| GET | `/v1/merchant/catalog/prices` | Paginated prices; `product_id`, `currency`, `type`, `archived` (`false` live only, `true` archived only, absent both) filters |
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
| GET | `/v1/merchant/catalog/meters` | List usage-meter definitions |
| GET | `/v1/merchant/catalog/meters/{key}` | Read one usage meter |
| GET | `/v1/merchant/catalog/meters/{key}/overrides` | List negotiated customer overrides for a meter |
| PUT | `/v1/merchant/catalog/meters/{key}` | Create or replace a usage-meter definition |
| PUT | `/v1/merchant/catalog/meters/{key}/rate-card` | Set the merchant-default rate card |
| DELETE | `/v1/merchant/catalog/meters/{key}/rate-card` | Remove the merchant-default rate card |

Catalog product and price lists return `{items, total, limit, offset}`. `limit`
and `offset` are the effective query values: nonpositive limits default to 100,
limits above 1000 clamp to 1000, and negative offsets become zero. Advance by the
returned `offset + limit`; a product-scoped price list follows the same paging
contract. Ties in creation time are ordered by ID. Catalog screens page these
results; selectors and manifest pruning explicitly traverse every page.

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

### Metrics, dashboard, webhooks, notifications

| Method | Path | Permission | Purpose |
|---|---|---|---|
| POST | `/v1/merchant/metrics/query` | `merchant:metrics:read` | Composable metrics query (Postgres-backed) |
| GET | `/v1/merchant/metrics/schema` | `merchant:metrics:read` | Metric registry / schema doc |
| POST | `/v1/merchant/metrics/ask` | `merchant:metrics:read` | Natural-language metrics Q&A (rate-limited, consent-gated) |
| GET | `/v1/merchant/dashboard` | `merchant:metrics:read` | Saved dashboard config |
| PUT | `/v1/merchant/dashboard` | `merchant:dashboard:update` | Replace dashboard config |
| POST | `/v1/merchant/dashboard/widgets/generate` | `merchant:dashboard:update` | NL widget generation |
| GET | `/v1/merchant/webhooks` | `merchant:metrics:read` | List outbound alert webhook metadata; destination host only, never URL credentials |
| POST | `/v1/merchant/webhooks` | `merchant:settings:update` | Create outbound webhook; URL is write-only and stored encrypted |
| PUT | `/v1/merchant/webhooks/{id}/url` | `merchant:settings:update` | Replace the destination credential while preserving the webhook identity |
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

On deployments without a control plane these routes are not registered (404), like every other capability the deployment cannot serve: the LLM routes (`/dashboard/widgets/generate`, `/metrics/ask`, `/catalog/ask`, `/catalog/copilot/confirm`) exist only with `llm.api_key` and the matching consent, and `/api-host` only when the merchant directory is armed. `GET /v1/capabilities` and `/admin/config.json` advertise what is mounted.

### Platform operator (`/v1/platform`, standalone only)

Human operator sessions checked against the root permission group; no API keys,
no merchant context.

| Method | Path | Permission | Purpose |
|---|---|---|---|
| GET | `/v1/platform/merchants` | `root:merchants:read` | Cross-merchant directory |
| GET | `/v1/platform/merchants/{id}` | `root:merchants:read` | One merchant |
| DELETE | `/v1/platform/merchants/{id}` | `root:merchants:delete` | Soft-delete a merchant |
| POST | `/v1/platform/merchants/{id}/restore` | `root:merchants:restore` | Restore a soft-deleted merchant |
| GET | `/v1/platform/worker-health` | `root:worker-health:read` | Cross-merchant worker health, including last-error text |
| DELETE | `/v1/platform/admin-rate-limit-lockouts/{user_id}` | `root:admin-rate-limits:unlock` | Clear one human administrator's distributed lockout |

## 6. Webhooks (inbound, per rail)

No bearer auth — each request is verified by provider signature or source IP
AFTER merchant resolution (the signature, not the router, is the trust
boundary). Success returns `200 { "status": "accepted" }`.

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/webhooks/{provider}` | The standalone surface: NMI-backed rails / CCBill; the merchant is derived from the payload's account identity |
| POST | `/v1/webhooks/{provider}/{account_id}` | Same, with the receiving PSP account pinned in the path (direct Stripe; multi-account rails) |
| POST | `/billing/v1/merchants/{merchant}/webhooks/{provider}` | Embedded only: the host pins one merchant, so the `{merchant}` slug resolves it and THAT merchant's signing secret verifies the payload |
| POST | `/billing/v1/merchants/{merchant}/webhooks/{provider}/{account_id}` | Embedded only, per-account (e.g. multiple NMI accounts) |

`{provider}` is the gateway KIND — `nmi`, `ccbill`, `stripe`, `solana`,
`basistheory`. It is never a PSP key: `mobius` and `paykings` both post to
`/v1/webhooks/nmi` and are told apart by `{account_id}` or the payload's own
account identity.

Deployments using per-merchant hostnames (`api.<slug>.<domain>`) additionally
serve `/v1/webhooks/{provider}[/{account_id}]` with the merchant resolved from
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

Outbound alert webhook URLs are write-only credentials, including path/query
components. The read projection contains `destination_host`, never the URL.
DB-backed writes require `ENCRYPTION_MASTER_KEY` even in development; Vault
uses the configured merchant secret backend. Rotation uses `PUT .../{id}/url`
with `{ "url": "..." }` and retains webhook identity. A metadata/secret version
mismatch refuses delivery until the same URL update is retried successfully.
Manifest deployments retain read-only provider credentials while this managed
webhook namespace uses the configured encrypted DB/Vault backend.

## Host event consumption

| Method | Path | Permission | Purpose |
|---|---|---|---|
| GET | `/v1/merchant/host-events` | `merchant:host-events:read` | Bounded typed pending host events |
| POST | `/v1/merchant/host-events/{id}/acknowledge` | `merchant:host-events:acknowledge` | Idempotent acknowledgment after host processing |
| GET | `/v1/merchant/customers/{customer_id}/payment-settlement-status` | `merchant:payments:read` | Historical positive rail payment for `price_id` |

`GET /v1/merchant/host-events` and `POST /v1/merchant/host-events/{id}/acknowledge`
are shared by standalone HTTP and the embedded Go `Client`. They require
`merchant:host-events:read` and `merchant:host-events:acknowledge`, respectively.

The list is merchant-scoped and bounded (`limit` defaults to 100, maximum 1000).
`type` selects `payment.settled`, `delinquency.grace`, `delinquency.entered`, or
`delinquency.cleared`. Pending events are returned oldest first. Acknowledge only
after idempotent host processing commits, then fetch again; a UUID high-water
mark can miss transactions that commit late. Acknowledgment is idempotent and
independent of customer and merchant notification read state. A payment event
contains the original payment UUID, its payer (`customer_id`), `price_id`, the
renewed `subscription_id` when any, amount, currency, merchant and settlement
time, preserving the fee-attribution coordinate.

`include_acknowledged=true` includes retained acknowledged rows; `payment_id`
selects one payment's event. Acknowledged rows are retained for 30 days by default;
pending rows survive retention. Hosts should filter by event type so one
consumer's pending work cannot starve another type.

`GET /v1/merchant/customers/{customer_id}/payment-settlement-status?price_id=...`
backs `Client.HasSettledPayment` and requires `merchant:payments:read`. It reads
the durable payment records: a positive completed or refunded original rail
payment establishes the historical fact for that merchant, customer and price.
Acknowledging or pruning its host event does not erase that fact.

## Catalog drift findings

`GET /v1/merchant/catalog/drift` lists open standing findings; each carries the
immutable `psp_id` of the provider account that was compared. A refresh reads
the active Stripe and NMI accounts completely and each stored Solana plan
individually. Only a successful read of that account (or that Solana price)
resolves an absent finding; unarmed, failed or other accounts keep theirs, an
older snapshot never overrides newer evidence, and an ignored finding stays
ignored. Price/product reconcile closes findings only for accounts it verified
in sync.
