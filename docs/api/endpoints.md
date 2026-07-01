# OpenRails API Reference

OpenRails exposes public catalog routes, delegated browser/self-service APIs,
delegated billing-admin APIs, rail webhooks, and an API-key server-to-server
surface on the same public port. All responses are JSON encoded. Unless
otherwise stated, errors follow the
Stripe-style envelope:

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

List endpoints use a Stripe-like envelope:

```json
{
  "object": "list",
  "data": [],
  "total": 0,
  "limit": 20,
  "offset": 0,
  "has_more": false,
  "url": "/v1/example"
}
```

`url` is included only on endpoints that use server-side pagination helpers; other list endpoints omit it.

## Authentication & Security

| Surface | How to authenticate |
|---------|---------------------|
| Public catalog (`/`, `/v1/products`, `/v1/prices`, `/v1/solana/tokens`, health probes) | No auth required |
| Self routes (`/v1/me/*`) | Standalone: `Authorization: Bearer <delegated JWT>` signed by a registered issuer with `delegated_sub`. Embedded: the host's configured AuthKit/user bearer is adapted to the same customer principal. |
| User/session routes (`/v1/checkout`) | Host JWT auth where still mounted by the embedding deployment |
| Merchant API (`/v1/merchant/*`, same public port) | `Authorization: Bearer <generated API key, first-party service JWT, delegated JWT, or user access token>`; each route requires a `merchant:*` permission |
| Webhooks (`/v1/webhooks/:provider`, `/v1/merchants/:merchant/webhooks/:provider`) | Provider-specific verification (see notes) |

Delegated JWTs and machine credentials are intentionally different credentials.
Delegated JWTs are browser/direct-user credentials verified through OIDC issuer,
JWKS, audience, expiry, and optional permission checks. OpenRails stores/touches
the payable customer reference `(merchant_id, subject)` needed for billing;
`issuer` is audit/last-seen source metadata, not identity. OpenRails does not
create OpenRails-native users for delegated subjects. Generated API keys and first-party service JWTs are backend
credentials and are rejected by delegated self routes.

Delegated JWT examples:

- Doujins/Hentai0 membership UI: the host frontend presents a short-lived token
  signed by its registered issuer with `aud: "openrails-app"` and
  `delegated_sub: "<canonical-user-id>"`. `/v1/me/*` acts only on that subject.
- Cozy Art billing-admin membership UI: an admin browser token is signed by the
  Cozy issuer with `delegated_sub: "<admin-subject>"`; AuthKit bounds requested
  permissions to that issuer's stored authority for `/v1/merchant/*`.
- Tensorhub merchant balance UI: Cozy Art can present a delegated JWT whose
  subject is the upstream merchant/company subject to read its own balance
  through browser/direct OpenRails routes. Backend
  reserve/capture/release remains machine-credential-only.

Verified AuthKit user-token `entitlements` claims are authoritative short-lived
snapshots for premium/product access decisions. `sid` remains session identity
for logout, reauth, and freshness flows; it is not profile data. Embedded
deployments that need grant/revoke changes reflected before token expiry can
configure a live freshness check, but standalone/remote mode accepts verified
JWT entitlement claims without that optional check.

## Health & Service Banner

### GET /
Returns a short JSON banner (`{"service":"billing","status":"ok","endpoints":[...]}`) so load balancers can
confirm the API is reachable.

### GET /health/live, /healthz
Unconditional liveness probes.

### GET /health/ready, /readyz
Runs readiness checks against Postgres, Redis, and the AuthKit verifier. Returns 200 when all checks pass,
or 503 with `{ status: "not_ready", ... }`. Add `?verbose=1` to include dependency details.

## Public Catalog Endpoints

### GET /v1/products
Lists products with embedded active prices. Query parameters mirror Stripe's `/v1/products`:
`active` (default `true`, only honoured for admins), `limit` (1-100, default 20), `offset` (>=0).
Response: `ListResponse<Product>`.

### GET /v1/prices
Lists price objects. Query parameters: `active`, `currency`, `product` (product ID), `type`
(`recurring` or `one_time`), plus `limit`/`offset`. Response: `ListResponse<Price>`.

### GET /v1/prices?product=prod_xxx
Same endpoint; explicitly documenting that product filters accept either the `prod_` prefixed ID or a
raw UUID.

### GET /v1/solana/tokens
Returns the currently supported Solana tokens and live pricing:

```json
{
  "tokens": [
    { "symbol": "USDC", "name": "USD Coin", "mint": "...", "decimals": 6, "price": 1.0 }
  ]
}
```

### POST /v1/webhooks/{provider}
Receives rail webhooks. `provider` is `ccbill`, `stripe`, or a configured NMI-backed rail such as `mobius`.
- `ccbill`: form-encoded payload, verified via source IP ranges (unless test mode).
- NMI-backed rails (for example `mobius`): JSON body with `Webhook-Signature` (`t=...,s=...`, preferred) or alternate
  `X-Signature`/`X-NMI-Signature`/`X-Mobius-Signature`.
- `stripe`: JSON body with `Stripe-Signature` header (if configured).
Returns 200 with `{ status: "accepted" }` on success, 401/403 for auth failures, 400 for unknown provider.

### POST /v1/merchants/{merchant}/webhooks/{provider}
Merchant-scoped webhook path for private/embedded multi-merchant installs. OpenRails resolves `{merchant}` first, then verifies with that merchant's provider signing secret. Unknown merchants return 404 and never fall back to a default merchant.

## Checkout Sessions (Authenticated)

### POST /v1/checkout
Creates a checkout session for **new** subscriptions and one-off purchases.

> **Note:** Tier upgrades/downgrades are **not** supported via this endpoint. If the user already has
> an active subscription in the same tier group, the response will be `{ "status": "blocked" }` with a
> message directing the client to use `POST /v1/me/subscriptions/change-tier` instead.

- Auth: bearer token
- Optional header: `X-Idempotency-Key` to dedupe create requests
- Body:
  - `price_id` (required)
  - `mode` (optional) – `one_off` or `subscription`; if omitted, resolved from the price
  - `payment` (required):
    - `rail` (required) – `mobius`, `ccbill`, `solana`, or `stripe`
    - `payment_method_id` or `payment_token` for `mobius`/`stripe`
    - `token_symbol` for `solana`
    - `flow` for `solana` – `transfer_request` (default) or `transaction_request`
    - `wallet` required for `transaction_request`
    - Billing details for `ccbill`/`stripe`: `email`, `first_name`, `last_name`, `address1`, `city`, `state`, `zip`, `country`
  - `metadata` (optional string map)
- Response: `CheckoutSessionResponse` with `payment` details, `next_action` (redirect/solana), and optional
  `payment_id` / `subscription_id` once completed.

### GET /v1/checkout/{id}
Retrieves a checkout session by ID. Returns `CheckoutSessionResponse`. Responds with 403 if the session
belongs to another user.

### POST /v1/checkout/{id}/confirm
Confirms a Solana checkout session.
- Body: `{ payment: { rail: "solana", signature: "...", wallet?: "..." } }`
- Response: `CheckoutSessionResponse`
- Errors: 400 validation, 403 forbidden, 404 not found, 409 conflict, 410 expired

## Self-Service API (`/v1/me`)

Every endpoint in this section requires an authenticated customer principal for
the current user: a delegated JWT in standalone mode, or the embedded host's
authenticated user bearer mapped to a customer principal.

### GET /v1/me/balance
Query param: `currency` (`USD`, `EUR`, `JPY`). Returns the caller's durable per-currency balance:
`{ currency, balance_amount }`. Amounts use the currency's native internal precision; USD is micro-USD.

### GET /v1/me/transactions
Query params: `currency`, `limit`, `offset`. Lists the caller's ledger transactions newest first.

### PUT /v1/me/settings
Body accepts customer-owned self-imposed settings only: `currency`, `max_spend_per_day`,
`max_spend_per_month`, `low_balance_threshold`, `auto_topup_enabled`, `auto_topup_amount`,
and `auto_topup_payment_method_id`.

### GET /v1/me/status
Aggregated premium status: whether the user currently has an active membership, the enriched
subscription object, the next renewal timestamp, and all entitlement records.
Response includes `has_active_subscription`, `subscription`, `next_renewal_at`, and `entitlements`.

### GET /v1/me/subscriptions
History of the caller's subscriptions. Query params: `status` (`pending`, `active`, `past_due`, `cancelled`, or `all`),
`limit`, `offset`. Response: `ListResponse<UserSubscription>` (with `product`, `price`, `access`).

### GET /v1/me/subscriptions/{id}
Retrieves a single subscription by ID with enriched product, price, and access data.
Returns `UserSubscriptionResponse`. Returns 404 if subscription is not found or doesn't belong to the user.

### PUT /v1/me/subscriptions/{id}/payment-method
Request body `{ "payment_method_id": "pm_uuid" }`. Updates the card vault entry
used for an NMI-backed subscription (CCBill/Solana subscriptions cannot be reassigned). Returns:
`{ success, message, subscription_id, payment_method_id }`.

### POST /v1/me/subscriptions/{id}/cancel
Body `{ "feedback": "optional text" }`. Cancels the specified subscription and returns
`202 { "status": "queued" }`. For CCBill subscriptions, returns
`422 { error, support_url, code }` because cancellation must be performed via the CCBill portal.

### POST /v1/me/subscriptions/{id}/resume
Queues a resume for a cancelled Stripe subscription. Returns `202 { "status": "queued" }`.
Returns 400 if the subscription is not cancelled or not a Stripe subscription.

### POST /v1/me/subscriptions/{id}/change-tier
Unified tier change endpoint for upgrades and downgrades across all rails.

**Request:**
- Body: `{ "price_id": "price_..." }`
- Optional header: `X-Idempotency-Key` for retry safety

**Response:** `TierChangeResponse`
```json
{
  "object": "tier_change",
  "status": "succeeded|requires_action|blocked",
  "mode": "tier_change",
  "action": "upgrade|downgrade",
  "price_id": "price_...",
  "url": "https://...",
  "payment": { "rail": "stripe|mobius|ccbill" },
  "subscription_id": "sub_...",
  "next_action": { "type": "redirect_to_url", "redirect_to_url": { "url": "..." } },
  "message": "...",
  "delayed_start": "2024-02-15T00:00:00Z"
}
```

**Rail behavior:**
| Rail | Upgrade | Downgrade |
|-----------|---------|-----------|
| Stripe | `succeeded` (immediate with proration) | `succeeded` + `delayed_start` (scheduled for period end) |
| NMI-backed rails | `succeeded` (immediate proration charge) | `succeeded` + `delayed_start` (scheduled) |
| CCBill | `requires_action` + top-level `url` redirect to FlexForm | `blocked` + message |
| Solana | HTTP 400 (not supported) | HTTP 400 (not supported) |

**Notes:**
- Target price must be in the same tier group as current subscription
- For hosted rail actions, clients should redirect to top-level `url`
- For scheduled downgrades, the change takes effect at `delayed_start`

### GET /v1/me/payments
Lists one-off payments. Query params: `type` (rail filter), `limit`, `offset`.
Response: list of `PaymentRecord` entries (raw payment model with optional `price` and `subscription`).

### GET /v1/me/payment-methods
Query params: `limit`, `offset`, `include_inactive`. Response: list of stored payment methods.

### POST /v1/me/payment-methods
Body includes `payment_token` (Collect.js token) plus billing details (`first_name`, `last_name`, `address1`, `city`,
`state`, `zip`, `country`, optional `email`, `phone`, `company`, `address2`, `provider`). Creates and activates an NMI
vault record. Response: payment method object.

### PUT /v1/me/payment-methods/{id}
Body accepts a new `payment_token` and optional billing fields (all pointers). Replaces the stored vault card for the
referenced method. Returns updated payment method.

### DELETE /v1/me/payment-methods/{id}
Soft-deletes the stored method. Response `{ success, message }`.

### PUT /v1/me/payment-methods/{id}/activate
Re-verifies and marks the method active. Response `{ success, message }`.

### GET /v1/me/balance
Query param: `currency` (`USD`, `EUR`, `JPY`). Returns the caller's durable per-currency balance:
`{ currency, balance_amount }`. Amounts use the currency's native internal precision; USD is micro-USD.

### GET /v1/me/transactions
Query params: `currency`, `limit`, `offset`. Lists the caller's ledger transactions newest first.

### PUT /v1/me/settings
Body accepts customer-owned self-imposed settings only: `currency`, `max_spend_per_day`,
`max_spend_per_month`, `low_balance_threshold`, `auto_topup_enabled`, `auto_topup_amount`,
and `auto_topup_payment_method_id`.

### GET /v1/me/notifications
Query params: `limit` (1-100), `offset`, `seen` (`true`/`false`). Response list of
notifications `{ id, event_type, data, seen, created_at }`.

### GET /v1/me/notifications/unread-count
Returns `{ unread_count: <int> }`.

### POST /v1/me/notifications/{id}/read
Marks the notification as read. Response `{ message: "notification marked as read" }`.

### POST /v1/me/billing-portal
Creates a billing portal session when the merchant has a provider that supports portal handoff.
Response `{ "url": "https://..." }`.

## Merchant API (`/v1/merchant/*`)

Merchant endpoints live on the SAME public port as everything else (issue #222)
- there is no private port, mTLS listener, or separate legacy API-key layer.
Callers present one of:

```
Authorization: Bearer <openrails_st_...>
Authorization: Bearer <service-jwt-signed-by-registered-issuer>
Authorization: Bearer <delegated-jwt-or-user-access-token>
```

Generated API keys are resolved through the OpenRails-owned AuthKit control
plane: their owning permission group maps to an OpenRails merchant, and granted
`merchant:*` permissions gate what they may do. API-key resource scopes are gone;
a merchant key acts within the merchant group it was minted under.

First-party service JWTs are signed by a registered issuer and must carry
standard JWT/OIDC claims plus `token_use=service`, `jti`, a maximum 15-minute
lifetime, accepted `aud`, and self-assigned `permissions`. Registering the issuer
to a merchant is the authorization: OpenRails resolves the issuer to its merchant,
treats the token's `permissions` claim as authoritative, and scopes every
resource to that merchant (a token can never reach another merchant's resources).

The canonical merchant permission vocabulary is OpenRails-owned `merchant:*`.
Every route passes through the same OpenRails principal gate regardless of
credential type: API key, service JWT, delegated JWT, or live AuthKit user
session.

Credential examples:

- Doujins/Hentai0/Cozy backend entitlement read: first-party service JWT subject
  such as `service:doujins-runtime`, permission `merchant:customer-settings:read`,
  resource `openrails.merchant=<merchant_uuid>`, route
  `GET /v1/merchant/customers/{customer_id}/entitlements`.
- Tensorhub reserve/capture/release: permission `merchant:admissions:create`;
  resources include `openrails.merchant=<merchant_uuid>` and, for payer-scoped
  generated tokens or service-JWT grants, `openrails.customer=<customer_uuid>`.

| Route | Required permission |
|-------|---------------------|
| `GET /v1/merchant/customers/{customer_id}/entitlements`, `GET /v1/merchant/users/{user_id}/product-access`, `GET /v1/merchant/invokers/{invoker}/credits`, `GET /v1/merchant/trust-tier`, `GET /v1/merchant/credit-limit`, `GET /v1/merchant/credits/balance` | `merchant:customer-settings:read` |
| `POST /v1/merchant/customers/entitlements:batch` | `merchant:customer-settings:read` |
| `PUT /v1/merchant/credit-limit`, `POST /v1/merchant/credits/deposit` | `merchant:customer-settings:update` |
| `POST /v1/merchant/admissions`, `POST /v1/merchant/admissions/{id}/capture`, `POST /v1/merchant/admissions/{id}/release`, `POST /v1/merchant/wasted-spend` | `merchant:admissions:create` |
| `GET /v1/merchant/settings` | `merchant:settings:read` |
| `PUT /v1/merchant/settings` | `merchant:settings:update` |
| `POST /v1/merchant/usage/rollup`, `POST /v1/merchant/usage/resource-revenue` | `merchant:usage:read` |

Embedded hosts skip HTTP entirely and call the in-process `pkg/service` facade
after authorizing the action themselves.

Terminology for this surface is defined in
`docs/authkit-merchant-oidc-glossary.md`: OpenRails merchant = billing/integration
boundary, delegated user = external OIDC `issuer` + `subject`, customer =
payable identity, generated API key/service JWT = server-to-server
credentials.

### GET /v1/merchant/customers/{customer_id}/entitlements
Returns active entitlements for the payable customer at the current time.
Optional query param `at` (RFC3339) queries entitlements at a specific time.
Response: array of entitlement records with `customer_id`. Merchant
entitlement reads query `billing.entitlements.customer_id` directly; they
do not translate the customer through legacy `user_id`.

### GET /v1/merchant/invokers/{invoker}/credits
Returns credit balance summary for an invoker. Optional query params:
`customer_id` and `type` (defaults to `api_credits`, which must exist in
`billing.credit_types`).
Response: `{ type, balance, held_balance }`.

### POST /v1/merchant/credits/deposit
Deposit/grant credits. Body: `{ customer_id, invoker_id, credit_type, amount, source, source_id?, expires_at?, description? }` where `expires_at` is epoch seconds.
Returns a `CreditTransaction`. If `source_id` is provided, deposits are idempotent per `(customer_id, credit_type, source, source_id)`.

### POST /v1/merchant/admissions
Pre-authorize spend and place holds. The returned admission id is the durable
identifier you later capture or release.

Idempotency:
- Hold creation is idempotent per `(customer_id, credit_type, source, source_id)`; retries return the existing hold transaction.

### POST /v1/merchant/admissions/{id}/capture
Capture a hold: `{ amount }` (amount <= hold). Updates the same `CreditTransaction` row to `status='captured'`, setting `captured_amount` and `amount` (negative).

### POST /v1/merchant/admissions/{id}/release
Release a hold without spending credits. Response `{ ok: true }`.

### Catalog (definition surface)

#### Checkout prerequisites

OpenRails does not seed products/prices/credit types in production. For checkout to work, the host must define:

- `billing.products`: at least one active product.
- `billing.prices`: at least one active price for that product.
- Rail mappings on the price (`billing.prices.rails`) for any rail you intend to use:
  - Stripe: `rails.stripe.price_id` (and optionally `rails.stripe.product_id`).
  - NMI-backed rails: `rails.<provider>.plan_id` (for example `rails.mobius.plan_id`).
  - CCBill: `rails.ccbill.form_name` + `rails.ccbill.flex_id`.
- Any credit types referenced by `products.credits_spec` must exist in `billing.credit_types`.

### POST /v1/merchant/catalog/products
Create a product. Body includes at least `{ key, display_name }`, and may include `entitlements_spec` and `credits_spec`.

#### `credits_spec` v2

`credits_spec` is a JSON object keyed by credit type name (`billing.credit_types.name`). Example:

```json
{
  "api_credits": { "amount": 100000, "expiry_hours": 720, "cadence": "per_renewal" },
  "signup_bonus": { "amount": 5000, "expiry_hours": 2160, "cadence": "once" }
}
```

- `amount` is in the credit type's base integer units (not USD cents).
- `expiry_hours` is optional; when present, each grant expires after N hours.
- `cadence` is `once` (default) or `per_renewal`.

Renewal semantics:
- `cadence=once` is granted on initial subscription activation.
- `cadence=per_renewal` is granted on confirmed renewal/rebill success (Stripe invoice paid; NMI rebill success; CCBill RenewalSuccess).

Idempotency / webhook replay safety:
- Recurring grants are idempotent per `(subscription_id, credit_type_id, period_end)` by using a deterministic deposit `source_id` derived from those fields (no dedicated idempotency table).
- Duplicate webhooks for the same period do not double-grant.

Host policy defaults (current behavior):
- Upgrades/downgrades do not trigger an immediate extra credit grant; recurring credits are granted on the next confirmed renewal.
- Refunds do not claw back previously granted credits (no automatic negative adjustments).

### PATCH /v1/merchant/catalog/products/{id}
Update product definition fields (display_name, description, entitlements_spec, credits_spec, tier_group/tier_rank, is_active).

### POST /v1/merchant/catalog/prices
Create a price. Supports per-rail mapping mode: `{ rails: { stripe: { link: {...} } | { create: {...} }, ... } }`.

Rail mapping modes:
- `link`: host provides existing rail identifiers and OpenRails stores them in `billing.prices.rails`.
- `create`: OpenRails attempts to create remote objects and stores the returned IDs.

Auto-create support:
- Stripe: supported (`create`), using Stripe API.
- NMI-backed rails: link-only (provide `plan_id`).
- CCBill: link-only (provide `form_name` + `flex_id`).

### PATCH /v1/merchant/catalog/prices/{id}
Update price display name, rails mapping, or active status.

### Provider registration & content-addressed dedup

OpenRails is the source of truth for catalog, product, entitlement, usage,
invoice, and billing semantics. Provider catalog objects are payment/sync
adapters: Stripe can mirror product/price objects, NMI-backed rails are
link-only for recurring plans, and Solana carries payment identifiers rather
than rich billing metadata. Do not rely on a provider round-trip to recover the
full OpenRails catalog.

When the catalog is synced, OpenRails find-or-creates the matching provider
objects. Matching is **content-addressed** — it keys off the catalog product key and price terms, not
the OpenRails row UUIDs — so re-syncing or wiping the DB and re-syncing always
re-attaches to the existing provider objects rather than duplicating them.

- **Price identity** is `(product_key, currency, unit_amount, access_duration_hours, auto_renew, trial_unit_amount, trial_duration_hours)`.
- **Stripe content keys** are derived from the product key and price terms.
- These keys do not depend on row UUIDs, so a DB-wipe-and-resync that regenerates
  UUIDs reattaches to the same Stripe objects. Re-sync is idempotent: re-attach,
  never duplicate. Reconciliation reverse-matches Stripe objects to OpenRails
  rows the same way (by content key, falling back from `openrails_price_key`
  metadata to the `openrails.` lookup_key prefix).

**Provider coverage:**

| Provider | Registration |
|----------|--------------|
| Stripe | **Auto-creates** products and prices on sync (and stamps the content keys above). |
| NMI-backed rails | **Link-only** — the operator must supply the existing NMI `plan_id` in the catalog. Not auto-created. |
| CCBill | **Link-only** — the operator must supply the CCBill `form_name` + `flex_id` (FlexForm) in the catalog. Not auto-created. |

A link-only provider with no operator-supplied ids is recorded as
`pending_manual_link` (the price is still created in OpenRails) and the response
carries a `pending_manual_actions` entry telling the operator which link ids to
PATCH in.

**Amount / billing-cycle changes (re-mint):** Stripe and NMI prices are
immutable on financial terms — `unit_amount`, `currency`, and the billing cycle
cannot be edited in place. When such a field changes on a key-stable price,
reconcile **re-mints**: it creates a new Stripe price (which sets
`transfer_lookup_key`, atomically moving the content `lookup_key` off the old
price onto the new one) and archives the old price. Mutable-only drift (active
flag) is propagated with a plain update. Re-mint requires `recreate=true` on the
reconcile call; otherwise the price reports `drifted_no_recreate` / a missing
remote reports `missing_no_recreate`.

**Test vs. live:** registration runs in whichever environment `test_mode`
selects. Stripe test and live are entirely separate namespaces (separate objects
and lookup_keys), so a price registered in the test environment is never matched
against a live object and vice versa.

## Merchant Support API (`/v1/merchant`, `merchant:*` permissions)

The old `/v1/admin/*` billing surface is removed. Staff/support calls use the
resource-named merchant surface below. Merchant admins can read saved payment
method metadata, but cannot create/update/delete customer payment methods.

### GET /v1/merchant/customers/{customer_id}
Returns the customer billing profile: customer id, trust tier/status, balances,
entitlements, product access, payment history, subscription history, and saved
payment-method metadata. Requires `merchant:customer-settings:read`.

### GET /v1/merchant/customers/{customer_id}/payment-methods
Lists redacted saved payment-method metadata. Requires
`merchant:customer-settings:read`.

### GET /v1/merchant/customers/{customer_id}/payments
Lists payment history for one customer. Requires `merchant:payments:read`.

### POST /v1/merchant/customers/{customer_id}/payments/off-channel
Records an off-channel/manual purchase and grants product entitlements through
the normal checkout purchase path. Requires `merchant:customer-settings:update`.

### POST /v1/merchant/customers/{customer_id}/entitlements
Manually grants an entitlement through the grant ledger. Requires
`merchant:customer-settings:update`.

### DELETE /v1/merchant/customers/{customer_id}/entitlements/{grant_id}
Revokes a manual entitlement grant. Requires
`merchant:customer-settings:update`.

### POST /v1/merchant/customers/{customer_id}/product-access
Manually grants product access through the grant ledger. Requires
`merchant:customer-settings:update`.

### DELETE /v1/merchant/customers/{customer_id}/product-access/{grant_id}
Revokes a manual product-access grant. Requires
`merchant:customer-settings:update`.

### GET /v1/merchant/payments
Lists merchant payments with filters. Requires `merchant:payments:read`.

### GET /v1/merchant/payments/{id}
Returns one payment with refund history. Requires `merchant:payments:read`.

### POST /v1/merchant/payments/{id}/refunds
Issues or records a refund through the underlying rail. `revoke_access`
must be explicit when the refund should also revoke one-off access granted by
that payment. Requires `merchant:payments:refund`.

### GET /v1/merchant/subscriptions
Lists merchant subscriptions with filters. Requires
`merchant:subscriptions:read`.

### GET /v1/merchant/subscriptions/{id}
Returns one subscription. Requires `merchant:subscriptions:read`.

### POST /v1/merchant/subscriptions/{id}/cancel
Cancels a subscription. `revoke_access` must be explicit when cancellation
should also revoke subscription/grace entitlements immediately. Requires
`merchant:subscriptions:update`.

### POST /v1/merchant/subscriptions/{id}/resume
Resumes a canceling subscription where the rail supports it. Requires
`merchant:subscriptions:update`.

### PUT /v1/merchant/subscriptions/{id}/payment-method
Reassigns a subscription to another saved payment method owned by the same
customer and merchant. Requires `merchant:subscriptions:update`.

### GET /v1/merchant/metrics
Returns folded support metrics. Requires `merchant:usage:read`.

### GET /v1/merchant/repair-alerts
Returns merchant repair alerts. Requires `merchant:repair-alerts:read`.

## Webhook Notes

- **CCBill**: Must originate from the published IP ranges; the handler also validates `formName`/`flexId`
  against the price metadata.
- **NMI-backed rails**: Supply `Webhook-Signature` (`t=...,s=...`, preferred) or alternate
  `X-Signature`/`X-NMI-Signature`/`X-Mobius-Signature`. When test mode is enabled via config the signature
  check is bypassed.
- **Stripe**: Uses the `Stripe-Signature` header with the configured webhook secret.
