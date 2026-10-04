# OpenRails API Reference

The HTTP surface of the standalone OpenRails server. Everything is served on ONE
public port under the `/v1` prefix (plus unprefixed health probes and the `/auth`
control-plane mount). Embedded hosts mount a subset of the same route groups —
`GET /v1/capabilities` reports which groups a deployment actually serves.

Every route, with its tier, permission and types, is in [routes.md](routes.md)
and `api/openapi.json`, generated from the route catalog.

All requests and responses are JSON unless noted. A request body may hold only
the fields its route declares. Non-2xx responses use the error envelope of
[errors.md](errors.md):

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "invalid_param",
    "message": "Human readable description",
    "param": "optional_param_name"
  }
}
```

List endpoints use a Stripe-like list envelope:

```json
{ "object": "list", "data": [], "total": 0, "limit": 20, "offset": 0, "has_more": false }
```

Cursor lists answer `{"data": [...], "next_cursor": "..."}`; `next_cursor` is
null on the last page and is passed back as `?cursor=`.

## Authentication overview

| Caller class | Credential |
|---|---|
| Public (catalog, health, capabilities, solana pricing) | none |
| Self-service `/v1/me/*` | `Authorization: DPoP <delegated JWT>` plus per-request `DPoP` proof (native: Bearer plus matching TLS client certificate) — short-lived token minted by the merchant's registered issuer with `delegated_sub` (embedded mode: the host's user bearer adapted to the same principal) |
| Checkout sessions `/v1/checkout-sessions/{id}` | the session id (`ocs_…`) in the path |
| Merchant `/v1/merchant/*`, `/v1/import/*` | `Authorization: Bearer <API key (openrails_st_…) | service JWT | user access token>` — every route is gated on a `merchant:*` permission, not on credential type |
| Platform `/v1/platform/*` | human operator session checked against root-group grants (standalone only) |
| Webhooks | provider signature / source-IP verification, no bearer |

Merchant permissions: API keys carry the permissions they were minted with;
service JWTs (`token_use=service`, max 15-min lifetime, signed by a registered
issuer) carry a self-asserted `permissions` claim scoped to the issuer's
merchant; human sessions are checked against the user's merchant-group role.
The required permission is listed per route below. A human session also needs a
recent sign-in for every `merchant:` permission except reads, the dashboard
layout and host-event acknowledgement, whichever route (import, catalog,
merchant API) serves the operation: otherwise 403 `step_up_required` with the auth
provider's step-up methods in `metadata`. Delegated merchant requests
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
| GET | `/v1/capabilities` | none | Static capability document: `route_groups` (which route sets are mounted) + `features` (`stripe_billing_portal`, `solana_one_time_payments`, `solana_subscription_management`, `provider_credential_writes`). Features require both an exposed HTTP action and provider support; webhooks appear only in route groups. ETagged, `Cache-Control: public, max-age=300` |
| GET | `/v1/captcha/status` | none | Captcha challenge status for the browser tier |
| GET | `/v1/captcha/client.js` | none | Captcha client script |
| GET | `/v1/products` | optional | Products on sale, each with its current prices; a price's `psps` carry the status only. Query: `limit`, `cursor` |
| GET | `/v1/prices` | optional | Prices on sale. Query: `product_id`, `currency`, `auto_renew`, `limit`, `cursor` |
| GET | `/v1/currencies` | none | The currency scale registry: `{object:"currencies", currencies:[{code, decimals, minor_decimals}]}`. Every monetary string on the wire is in native units (`10^decimals` per major unit); providers settle in `10^minor_decimals`. System-fixed, merchant-independent; `billing.Currencies()` is the same table in Go. OpenRails stamps it into the checkout session document as `plan.unit_decimals` ([checkout](commerce.md#checkout-sessions)) |
| GET | `/v1/checkout-config` | none | Per-merchant checkout discovery: the merchant's **armed** PSPs as `{key, rail, display_name, flow, checkout, config}`, where `key` is a merchant checkout attempt's `payment.rail` value, `flow` is `tokenize`/`elements`/`redirect`/`wallet`, `checkout` marks PSPs that take new purchases and cards under the checkout routing, and `config` carries only public-by-nature values (NMI `tokenization_key` + `tokenization_url`; Stripe `publishable_key`; Basis Theory `public_api_key`). Merchant resolved from `Host`. ETagged, `Cache-Control: public, max-age=60`. Serves a fixed per-rail whitelist — no merchant secret can appear. When a Solana PSP is armed, `solana` carries `{network, chain, preferred_token, tokens[]}` |
| GET | `/v1/solana/tokens` | none | Supported Solana tokens with live pricing: `{ tokens: [{symbol, name, mint, decimals, price}] }`. Query: `price_id`, `wallet` |

There is no `/health` route — probes are `/health/live` and `/health/ready`.

## 2. Checkout sessions

A browser buys through a checkout session ([checkout](commerce.md)): the
signed-in customer mints one (section 3) or the merchant's server does (section
4), and the session id is then the only credential.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/v1/checkout-sessions/{id}` | the id | The session document: plan, amounts, `options`, saved methods, status |
| POST | `/v1/checkout-sessions/{id}/pay` | the id | Pay with one option: `{option_id, payment_token \| payment_method_id \| card \| token_symbol, billing fields}` → `{status, next_action, operation, failure, payment_id, subscription_id}` |
| GET | `/v1/checkout-attempts/{id}/solana-pay` | the attempt id | Solana Pay transaction request label (mounted when a Solana rail is configured) |
| POST | `/v1/checkout-attempts/{id}/solana-pay` | the attempt id | Solana Pay transaction-request callback: the wallet posts its account and receives the transaction to sign |

A buyer who already has an active subscription in the price's tier group gets
`{ "status": "blocked" }`; tier changes go through
`POST /v1/me/subscriptions/{id}/change-tier`.

## 3. Self-service (`/v1/me/*`)

All `/v1/me/*` routes require a delegated customer principal; every operation is
scoped to the token's subject — no `:user_id` appears in any path.

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/me/balance` | The caller's `Balance` in one currency: `balance_amount`, `held_amount`, `available_amount`, `owed_amount` (decimal strings, native units). Query: `currency` |
| GET | `/v1/me/transactions` | The caller's credit ledger, newest first (`ListPage<CreditTransaction>`). Query: `currency`, `limit`, `cursor` |
| PUT | `/v1/me/collection-payment-method` | Choose the saved method for automatic invoice collection in one currency. Body: `currency`, `payment_method_id`. The method must belong to the payer and support saved-method charges; otherwise `400` |
| GET | `/v1/me/usage` | The caller's usage (`Usage`), one row per `group_by` key (`event_type` default, `invoker`, `resource`, `function`, `tier`). Query: `currency`, `from`, `to` (default: the last month) |
| GET | `/v1/me/spend-limits` | The spend windows the AUTHENTICATED INVOKER is enforced against at admission, with live metering: `{ currency, invoker, windows: [{ scope, key, window_seconds, limit, currency, used, reserved, remaining, resets_at }] }`. Query: `currency` (required). Windows are estimate-based, so `used` already includes in-flight reservations and `reserved` names that part (what a release hands back); `resets_at` is the window's real staggered boundary. Self-scoped by construction — both the payer account and the invoker come from the credential, and naming another subject (`invoker`, `customer_id`, `scope_key`, `subject`) is refused `400 invalid_query`. The delegations a customer granted are the merchant's `GET /v1/merchant/customers/{customer_id}/spend-delegations` |
| GET | `/v1/me/invoices` | The subject's invoices, a cursor page. Query: `limit`, `cursor` |
| GET | `/v1/me/invoices/{id}` | One invoice, including payer-scoped recovery state |
| POST | `/v1/me/invoices/{id}/pay-now` | Verified customer payment on an NMI saved method. Requires Idempotency-Key and payment_method_id; 200 complete, 202 unresolved, coded 402 card refusal. |
| GET | `/v1/me/payments` | Payment and refund history, newest first, a cursor page; each row embeds the `price` and `product` it bought. Query: `limit`, `cursor`, `kind`, `rail`, `price_id`, `subscription_id`, `transaction_id` |
| GET | `/v1/me/entitlements` | The customer's active entitlement windows (`{data, next_cursor}`). Query: `at` |
| GET | `/v1/me/notifications` | Notifications (`billing.Notification`: typed `data`, money as decimal strings, ids typed). Query: `limit`, `offset`, `seen` |
| GET | `/v1/me/notifications/unread-count` | `{ unread_count }` |
| POST | `/v1/me/notifications/{id}/read` | Mark one notification read |
| POST | `/v1/me/billing-portal` | Provider billing-portal session `{ url }` (mounted only when a Stripe rail is configured) |

### Stripe engine payment setup and authentication

These self-service resources are scoped to the verified customer. Setup saves a
payment method; it does not create a subscription or grant paid access. Secret
responses are non-cacheable. Confirmation verifies the existing provider object.

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/me/payment-method-setups` | Accept recurring consent and create/replay saved-card setup for a selected PSP; requires Idempotency-Key |
| GET | `/v1/me/payment-method-setups/{id}` | Read the caller's setup action and client secret |
| POST | `/v1/me/payment-method-setups/{id}/confirm` | Verify successful setup and link the caller's reusable payment method |
| GET | `/v1/me/payment-operations/{id}/authentication` | Read the original accepted Stripe payment's authentication action |
| POST | `/v1/me/payment-operations/{id}/authentication/confirm` | Verify the original payment after authentication; never create a replacement charge |

### Subscriptions

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/me/subscriptions` | Subscription history as the shared `Subscription` shape (typed ids, `price.unit_amount` string, `scheduled_price`/`scheduled_product`, `card`, `cancel_portal_url`, `access`). Query: `status` (`pending`,`active`,`past_due`,`canceled`,`all`), `limit`, `offset` |
| GET | `/v1/me/subscriptions/{id}` | One subscription, same shape (404 if not the caller's); `{id}` is the listed `sub_…` id |
| POST | `/v1/me/subscriptions/{id}/cancel` | Cancel at period end. Body `{ "reason": "...", "signature": null }` (`reason` 4-500 chars). Answers the `Subscription`. A rail that needs the customer's own step (Solana: the wallet signs the on-chain cancel) answers the unchanged subscription with `next_action` (`solana_sign_transactions`); sign and send it, then repeat the request with `signature` |
| POST | `/v1/me/subscriptions/{id}/retry-now` | Verified customer retry of a past-due supported native NMI or engine NMI/Stripe agreement through its shared durable operation. Requires Idempotency-Key; optional payment_method_id must match its current method. 200 complete, 202 unresolved, coded 402 card refusal. |
| POST | `/v1/me/subscriptions/{id}/resume` | Undo a scheduled cancel on a reversible rail before period end. Answers the `Subscription`; 400 with a specific reason otherwise |
| POST | `/v1/me/subscriptions/{id}/change-tier` | Upgrade or downgrade within the tier group. Body `{ "price_id": "...", "signature": null }`. Answers a `TierChange`; see below |
| POST | `/v1/me/subscriptions/{id}/change-tier/preview` | Dry-run of the tier change (proration/effect preview), no mutation |
| PUT | `/v1/me/subscriptions/{id}/payment-method` | Charge renewals to another saved method. Body `{ "payment_method_id": "..." }`. Answers the `Subscription`. A method vaulted by a different provider account is `409 payment_method_psp_mismatch` |

A Solana subscription is changed and canceled in the customer's wallet: cancel
and change-tier answer `next_action: { type: "solana_sign_transactions",
transactions: [...] }`; the wallet signs and sends the transaction, and the
same request repeated with `signature` mirrors the landed transaction. Nothing
changes before the chain confirms it.

Tier-change response: `{ object: "tier_change", status: "succeeded"|"processing"|"requires_action"|"blocked", action, effective: "now"|"period_end", price_id, url?, subscription_id?, next_action?, delayed_start?, amount_due_now, next_charge_amount, next_charge_date?, message?, operation_id? }`.
**Engine-owned subscriptions** (Stripe and NMI, `collection_policy: "engine"`):
an upgrade is effective `now` — one engine charge of `amount_due_now` (new price −
unused credit, as previewed) on the subscription's saved card; on success a
successor subscription (the returned `subscription_id`) opens a fresh period of the
new cadence and the old one is canceled (`cancel_type: "upgrade"`), and renewals
bill the new price. A declined charge changes nothing (`402` with the decline
reason); an issuer challenge answers `requires_action` with
`next_action.type: "payment_authentication"` and `operation_id` (authenticate via
`/v1/me/payment-operations/{operation_id}/authentication`, then replay the same
`Idempotency-Key`). A downgrade is effective at `period_end`: nothing is charged or
refunded, access and price stay until the period ends, and that renewal bills the
new price for a period of its cadence; the same downgrade replays, another pending
change answers `409 tier_change_already_scheduled`. A tier change while the period
has ended or its renewal is unresolved answers `409 tier_change_renewal_due`.
Stripe/NMI upgrades succeed immediately with proration (the old plan's unused
share of its actual current period, at sub-second precision, credited against the
new price; cadences may differ — see `docs/merchant-guide.md`); an upgrade whose
target has no cycle answers `422 tier_change_cycle_unknown`, one without a valid
current period `422 tier_change_period_unknown`, and one whose credit exceeds the
target price `409 tier_change_credit_exceeds_price`. An NMI-billed (legacy)
subscription changes tier in place: its NMI schedule keeps its next billing
date E and only its amount changes (a named-plan schedule switches to the
target price's linked NMI plan; without one the change answers
`409 tier_change_requires_linked_plan` before any charge); an upgrade charges the new price's share
of the time to E less the old price's unused credit (preview equals charge),
a downgrade charges nothing and the renewal at E opens the new tier; a target
of another billing cycle answers `409 tier_change_cadence_unsupported`;
downgrades succeed with
a `delayed_start` at period end; CCBill upgrades return `requires_action` with a
redirect `url`, downgrades are `blocked`; Solana tier changes go through the
on-chain prepare/confirm routes above. **A tier change requires an
`Idempotency-Key`**: without one it answers `400
tier_change_idempotency_key_required` before any admission or provider call,
because the key is the client's only handle on a lost response. A key that
already names a different tier change (another customer, subscription or
target) answers `409 tier_change_idempotency_conflict` and never that
operation's result. A Stripe tier change and an NMI upgrade are durable
operations keyed by that header within the merchant, independent of rail:
concurrent requests cannot claim the same key for separate NMI and Stripe
operations. With one contract on both rails, the same key replays the stored
result (`200`); while the provider outcome is unresolved it
answers `202` with `status: "processing"` and `operation_id`; a request under
another key while one is unresolved answers `409 tier_change_in_flight` with
`metadata.operation_id`; a definitive provider refusal answers `400`/`402`
(`tier_change_refused`, or the card decline code) and an operator-attested
non-execution `409 tier_change_refused`. Checkout confirmation answers `409`
for an unresolved sale.

### Payment methods

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/me/payment-methods` | Stored cards, newest first, a cursor page. Query: `limit`, `cursor` |
| POST | `/v1/me/payment-methods` | Save a card with the PSP `psp_id`: a `payment_token` from the PSP's card fields, or `card` for a PSP whose card entry is server. Optional `billing_details`. OpenRails reads the card's brand, last four and expiry from the PSP. `201` with the method |
| PUT | `/v1/me/payment-methods/{id}` | Durably replace an NMI card with a new `payment_token` or `card`; `billing_details` present replace the saved ones. Returns the updated method when confirmed, `202` with no body while converging, `409 payment_method_update_retry_required` when a fresh token is required, or `502 payment_method_update_failed` for a terminal provider conflict. |
| DELETE | `/v1/me/payment-methods/{id}` | Delete an NMI method through the durable provider-aware workflow. Returns `204` when complete, `202` while provider convergence continues, and an error when refused/failed. Stripe cards are managed through Stripe Billing Portal. |

A payment method is `{id, customer_id, rail, psp_id, card, billing_details,
health, subscriptions, collection_currencies, created_at}`. `card` is the one
card shape of the API, `{brand, last4, exp_month, exp_year}`, each null when
the provider did not report it. `psp_id` is null for a card a third-party
custodian holds: each charge routes to the one live PSP of its rail that
reaches the custodian. There is no default card: a charge names its card, and
`collection_currencies` lists the currencies whose invoices the card collects
(the explicit choice set through `PUT /v1/me/collection-payment-method`; no
other saved method is inferred).

The admin payment-method card labels each currency separately. Refresh payment
methods invalidates both the customer profile and saved-method query caches.
Completed customer-initiated deletion removes the local method and clears its
collection choice through foreign keys; no replacement method is selected.

### Checkout (delegated)

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/me/checkout-sessions` | Mint a [checkout session](commerce.md#checkout-sessions): `{price_key \| price_id, success_url?}` → `201 {id, url, expires_at}` |

## 4. Merchant machine surface (`/v1/merchant/*`, `/v1/import/*`)

Server-to-server billing operations. Every route is gated on the listed
`merchant:*` permission regardless of credential type.

| Method | Path | Permission | Purpose |
|---|---|---|---|
| POST | `/v1/merchant/entitlements/lookup` | `merchant:customer-settings:read` | Active entitlements of up to 500 customers: body `{customer_ids, at}`, answer `{customers: {id: [EntitlementRecord]}}` |
| PUT | `/v1/merchant/customers/{customer_id}` | `merchant:customer-settings:update` | Create the customer or replace its declared fields (`Client.EnsureCustomer`): `{ email? }`; returns `Customer` `{ id, email, created_at, last_seen_at }` |
| GET | `/v1/merchant/customers/{customer_id}/spend-delegations` | `merchant:customer-settings:read` | The customer's spend delegations (`ListPage<SpendDelegation>`) |
| PUT | `/v1/merchant/customers/{customer_id}/spend-delegations` | `merchant:customer-settings:update` | Replace the customer's full spend-delegation policy: `{ delegations }` |
| PUT | `/v1/merchant/customers/{customer_id}/spend-delegations/{scope}/{scope_key}` | `merchant:customer-settings:update` | Set one delegation: `{ windows, provenance? }`; siblings untouched |
| DELETE | `/v1/merchant/customers/{customer_id}/spend-delegations/{scope}/{scope_key}` | `merchant:customer-settings:update` | Revoke exactly one delegation (or#911); siblings untouched; 404 when nothing exists at the key |
| GET | `/v1/merchant/entitlements/{entitlement}/customers` | `merchant:customer-settings:read` | One page of the customers holding an entitlement. Query: `at`, `cursor`, `limit` |
| GET | `/v1/merchant/customers/{customer_id}/product-access` | `merchant:customer-settings:read` | One page of the products a customer has access to. Query: `cursor`, `limit` |
| POST | `/v1/merchant/customers/{customer_id}/entitlements/check` | `merchant:customer-settings:read` | Exact grant-backed checks for at most 100 entitlement keys; optional `at` instant |
| POST | `/v1/merchant/customers/{customer_id}/product-access/check` | `merchant:customer-settings:read` | Check access for exactly one bounded product_ids or product_keys list without loading purchase history |
| POST | `/v1/merchant/checkout-sessions` | `merchant:checkout:create` | Mint a checkout session for the supplied customer and price; hand its id to that customer's browser |
| POST | `/v1/merchant/checkout-attempts` | `merchant:checkout:create` | Charge now for the supplied customer identity: exactly one price_id or price_key, optional entitlement and offer_kind assertions, required Idempotency-Key header. Creating the attempt accepts the price's terms |
| GET | `/v1/merchant/checkout-attempts/{id}` | `merchant:customer-settings:read` | Read a checkout attempt |
| POST | `/v1/merchant/checkout-attempts/{id}/confirm` | `merchant:checkout:create` | Confirm a Solana attempt with the wallet's signature: `{signature, wallet?}`; `202` while it is processing |
| GET | `/v1/merchant/checkout-config` | `merchant:customer-settings:read` | Armed PSPs, their public browser values and the Solana acceptance policy for the credential's merchant; with `price_id` or `price_key`, the `options` that can sell that price, locally ready, no provider request |
| GET | `/v1/merchant/customers/{customer_id}/tier` | `merchant:customer-settings:read` | The tier the customer holds in query `group`: `{group, tier}`, `tier` null when none |
| POST | `/v1/merchant/admissions` | `merchant:admissions:create` | Admit requests and place their holds: `{ items: [AdmitParams] }` → `{ items: [{ status, admission, error }] }`, one verdict per item. Each item's caller-chosen `request_id` identifies the admission: a retry with the same terms answers the same `Admission` (`replayed`), changed terms are `idempotency_key_reused`. An item with `estimated_amount > 0` places a hold and MUST carry `expires_at` (RFC3339): the deadline of the job the hold covers. There is no default lifetime — the hold lives until captured, released, extended, or that deadline |
| GET | `/v1/merchant/admissions/{request_id}` | `merchant:usage:read` | One `Admission` and its hold state (`open`, `captured`, `released`, `expired`); 404 `admission_not_found` |
| POST | `/v1/merchant/admissions/{request_id}/capture` | `merchant:admissions:create` | Capture a hold: `{ amount, usage? }` (`amount` required; `"0"` completes at no cost; `usage.event_type` also records a usage event). Idempotent on the request id; an identical retry answers `replayed: true`, a changed amount is refused 409 `idempotency_key_reused` |
| POST | `/v1/merchant/admissions/{request_id}/release` | `merchant:admissions:create` | Release a hold without spending; 409 `admission_captured` once captured |
| POST | `/v1/merchant/admissions/{request_id}/extend` | `merchant:admissions:create` | Re-declare a live hold's deadline: `{ expires_at }` (RFC3339). A hold lives exactly as long as its admit declared (`expires_at` is required with `estimated_amount`); a still-running job extends before that or loses it. 404 `hold_not_found` when nothing live exists — re-admit, a lapsed hold is never resurrected |
| POST | `/v1/merchant/wasted-spend` | `merchant:admissions:create` | Report wasted spend against admissions |
| POST | `/v1/merchant/usage-events` | `merchant:admissions:create` | Record one usage event (`UsageEventParams`): idempotent on `(source, source_id)`; 201 created, 200 replayed, a changed event is 409 `idempotency_key_reused`. A non-zero `amount` debits the customer's balance; zero records a metered-only event the rate cards price |
| GET | `/v1/merchant/customers/{customer_id}/usage` | `merchant:usage:read` | A customer's usage (`Usage`); same query as `/v1/me/usage` |
| POST | `/v1/merchant/provider-operations` | `merchant:admissions:create` | Open a durable provider-operation authorization (#1004): `{ operation_id, customer_id, record_owner, currency, amount, claim_reference, authorization_body, authorization_body_sha256 }` (`currency` is `USD`). Exact replay → `replayed=true`; a changed field → 409 `operation_authorization_conflict` with `param`; 402 `insufficient_credits` |
| GET | `/v1/merchant/provider-operations/{operation_id}` | `merchant:usage:read` | Read one authorization; 404 `operation_authorization_not_found`. Path-escape the id |
| POST | `/v1/merchant/provider-operations/{operation_id}/release` | `merchant:admissions:create` | Release after proven provider non-creation: `{ release_reference }`. 409 `operation_authorization_has_billing_evidence` once any evidence exists |
| POST | `/v1/merchant/provider-operations/{operation_id}/observations` | `merchant:admissions:create` | Append immutable provider billing evidence (lifecycle facts, raw body, typed records or refusal); unknown fields are refused. OpenRails qualifies, rates and settles; there is no caller-rated amount. Spend authority because an eligible observation settles the payer's reservation. Encoded body ≤ 768 KiB (`ProviderBillingObservationMaxBytes`, checked on the shared service path and mirrored by the remote Client, so over-cap is `400 invalid_param` in every deployment) |
| GET | `/v1/merchant/provider-operations/{operation_id}/qualification` | `merchant:usage:read` | Qualification state with its authorization; 404 `provider_billing_qualification_not_found` |
| GET | `/v1/merchant/settings` | `merchant:settings:read` | Merchant billing settings |
| GET | `/v1/merchant/configuration` | `merchant:settings:read` | Read the merchant configuration and its current revision |
| POST | `/v1/merchant/configuration/applications` | `merchant:settings:update` | Apply a configuration document with an application ID and expected revision; returns a durable receipt ([configuration applications](../merchant-configuration-applications.md)) |
| PUT | `/v1/merchant/settings` | `merchant:settings:update` | Replace the merchant settings document atomically ([merchant-settings.md](merchant-settings.md)), incl. `billing_policies` + `billing_policy_bindings` ([billing-policies.md](../billing-policies.md)) |
| GET | `/v1/merchant/api-host` | `merchant:settings:read` | The merchant's proven API host (#734 Host routing; `api_host` null when unset) and its open `claim`, if any |
| PUT | `/v1/merchant/api-host` | `merchant:settings:update` | Claim an API host: `{ api_host }` (bare lowercase domain). 202 with `claim.dns_record`: publish its `value` as a TXT record at its `name` (`_openrails-challenge.<host>`), then verify. A claim routes nothing. `""` releases the host and any claim at once. 400 `api_host_reserved` for the deployment's own hosts (public billing URL, console, issuer); 409 `api_host_taken` when another merchant holds it |
| POST | `/v1/merchant/api-host/verify` | `merchant:settings:update` | Prove the open claim through DNS (5s bound) and bind its host; 409 `api_host_unproven` until the TXT record carries the token, `api_host_taken` when another merchant proved it first |
| GET | `/v1/merchant/delinquency` | `merchant:customer-settings:read` | Arrears delinquency roster (`ListPage<Delinquency>`, grace + delinquent, oldest debt first). `?state=grace\|delinquent`, `?limit=`, `?cursor=`. See [arrears-delinquency.md](../arrears-delinquency.md) |
| GET | `/v1/merchant/customers/{customer_id}/billing-policy` | `merchant:customer-settings:read` | Read the explicit assignment; `policy_name: null` means inherit tier/default |
| PUT | `/v1/merchant/customers/{customer_id}/billing-policy` | `merchant:customer-settings:update` | Assign an existing policy or clear with `{ "policy_name": null }`; customer must exist; unknown fields/missing policy_name refuse. See [billing policies](../billing-policies.md#assigning-a-customer) |
| GET | `/v1/merchant/customers/{customer_id}/delinquency` | `merchant:customer-settings:read` | One payer's delinquency state per currency (`ListPage<Delinquency>`); empty = never overdue |
| GET | `/v1/merchant/customers/{customer_id}/balance` | `merchant:customer-settings:read` | The customer's `Balance` in one currency. Query: `currency` |
| GET | `/v1/merchant/customers/{customer_id}/credit-limit` | `merchant:customer-settings:read` | How much the customer may owe in arrears (`CreditLimit`). Query: `currency` |
| PUT | `/v1/merchant/customers/{customer_id}/credit-limit` | `merchant:credits:grant` | Set it: `{ currency, amount }` |
| GET | `/v1/merchant/customers/{customer_id}/trust-level` | `merchant:customer-settings:read` | The stored trust level admissions use when a request names none. Query: `currency` |
| PUT | `/v1/merchant/customers/{customer_id}/trust-level` | `merchant:customer-settings:update` | Set it: `{ currency, trust_level }`; empty clears |
| POST | `/v1/merchant/customers/{customer_id}/credit-grants` | `merchant:credits:grant` | Grant prepaid credit: `{ currency, amount, source, source_id, invoker?, expires_at?, description? }` → `CreditGrant` (201). `source_id` (any non-empty string) is the caller's reproducible idempotency key, once-only per `(customer_id, source_id)`: an identical retry answers the same grant with `replayed: true` (200); a different amount, currency or expiry is 409 `idempotency_key_reused`. Owner-level permission (NOT held by the fixed support role); rate-limited as an admin grant operation |
| GET | `/v1/merchant/customers/{customer_id}/credit-grants` | `merchant:customer-settings:read` | The customer's credit grants, newest first (`ListPage<CreditGrant>`, with spent, remaining, expired and revoked amounts). Query: `currency`, `source_id` (what did this key do), `limit`, `cursor` |
| GET | `/v1/merchant/customers/{customer_id}/credit-grants/{grant_id}` | `merchant:customer-settings:read` | One `CreditGrant`; 404 `credit_grant_not_found` |
| POST | `/v1/merchant/customers/{customer_id}/credit-grants/{grant_id}/revoke` | `merchant:credits:revoke` | Revoke the grant's unspent remainder: `{ reason }` → `CreditGrant`; revoking a revoked grant answers it with `replayed: true`; 409 `credit_grant_held` while holds need the credit |
| GET | `/v1/merchant/customers/{customer_id}/transactions` | `merchant:customer-settings:read` | The customer's credit ledger in one currency, newest first (`ListPage<CreditTransaction>`). Query: `currency`, `limit`, `cursor` |
| POST | `/v1/import/billing` | `merchant:billing:import` | Declared billing facts (`Client.ImportBilling`): customers, payment methods, subscriptions, transactions and admin comps (`admin_grants`), idempotent by source id — a distinct owner-level grant |
| GET | `/v1/merchant/billing-archive` | `merchant:billing:export` | Export supported merchant billing state as a bounded JSONL archive with a verified footer (`Client.ExportMerchantBilling`); source writers must be stopped for cutover. See [merchant portability](../merchant-portability.md) |
| POST | `/v1/merchant/billing-archive` | `merchant:billing:import` | Atomically restore into an explicitly provisioned empty destination with the same merchant UUID (`Client.ImportMerchantBilling`); identical artifact retries return the committed receipt. Secrets and identity authority are configured separately |

## 5. Merchant admin (human) routes

Same `/v1/merchant` prefix and permission gate; these are the console/support
surface. The merchant admin console SPA (when enabled and built) is served at
`GET {admin_console.path}/` (`/admin/` by default), and the selected AuthKit control-plane route groups (login,
tokens, membership) are mounted under `/auth/v1/*` — see AuthKit's own reference
for those routes.

### Customers & support

| Method | Path | Permission | Purpose |
|---|---|---|---|
| GET | `/v1/merchant/customers` | `merchant:customer-settings:read` | Customers, newest first (`ListPage<Customer>`). Query: `q` (id prefix or email substring), `limit`, `cursor` |
| GET | `/v1/merchant/customers/{customer_id}` | `merchant:customer-settings:read` | One `Customer`; 404 `customer_not_found` |
| GET | `/v1/merchant/customers/{customer_id}/billing-profile` | `merchant:customer-settings:read` | Billing at a glance: `customer`, `balances`, entitlements, subscriptions of every status (newest 100, with `status`), history, redacted payment-method metadata. Sections degrade independently: a failed collection-defaults read logs and returns the methods without `collection_default_currencies` instead of failing the profile |
| GET | `/v1/merchant/customers/{customer_id}/payment-methods` | `merchant:customer-settings:read` | The customer's saved cards, a cursor page (`limit`, `cursor`); admins can never create or update them |
| DELETE | `/v1/merchant/customers/{customer_id}/payment-methods/{id}` | `merchant:customer-settings:update` | Shared customer ownership and provider deletion; 204 completed or 202 pending reconciliation |
| POST | `/v1/merchant/customers/{customer_id}/payments/off-channel` | `merchant:customer-settings:update` | Record an off-channel/manual purchase through the normal purchase path: `201` with the `Payment`; the same `transaction_id` with the same terms answers it again (`200`), with other terms `409 idempotency_key_reused` |
| POST | `/v1/merchant/customers/{customer_id}/entitlements` | `merchant:customer-settings:update` | Grant an entitlement as the merchant's own grant: `hours` (at most 2562047) or `ends_at`; neither (no end) also needs `merchant:access:grant-permanent` |
| DELETE | `/v1/merchant/customers/{customer_id}/entitlements/{id}` | `merchant:customer-settings:update` | Revoke one entitlement window (the merchant's own grant is revoked in the grant ledger); `204` |
| POST | `/v1/merchant/customers/{customer_id}/product-access` | `merchant:customer-settings:update` | Manually grant product access; without `ends_at` (no end) also needs `merchant:access:grant-permanent` |
| DELETE | `/v1/merchant/customers/{customer_id}/product-access/{id}` | `merchant:customer-settings:update` | Revoke a manual product-access grant; `204` |
| GET | `/v1/merchant/customers/{customer_id}/invoice-profile` | `merchant:customer-settings:read` | Read invoicing terms, tax facts, contacts and memo; `404` when the customer has none |
| PUT | `/v1/merchant/customers/{customer_id}/invoice-profile` | `merchant:customer-settings:update` | Replace the profile used for future invoice snapshots (`201` created, `200` replaced); with `If-None-Match: *` only create it, answering an existing one unchanged |
| GET | `/v1/merchant/customers/{customer_id}/rate-overrides` | `merchant:customer-settings:read` | List the payer's negotiated meter rate cards |
| PUT | `/v1/merchant/customers/{customer_id}/rate-overrides/{meter_key}` | `merchant:customer-settings:update` | Set the payer's negotiated rate and optional included allowance |
| DELETE | `/v1/merchant/customers/{customer_id}/rate-overrides/{meter_key}` | `merchant:customer-settings:update` | Restore the merchant-default rate for future usage |

### Payments & subscriptions

| Method | Path | Permission | Purpose |
|---|---|---|---|
| GET | `/v1/merchant/payments` | `merchant:payments:read` | Payments (money that moved; declines are payment attempts), newest first, a cursor page. Filters `customer_id`, `subscription_id`, `price_id`, `rail`, `kind` (`charge`, `refund`, `chargeback`, `dispute_reversal`), `transaction_id`; `Client.ListPayments` |
| GET | `/v1/merchant/payments/{id}` | `merchant:payments:read` | One payment with its `refunds`; payments embed the `price` and `product` they bought; `Client.GetPayment` |
| POST | `/v1/merchant/payments/{id}/refunds` | `merchant:payments:refund` | Refund through the rail. `Idempotency-Key` required; body `{amount}` or `{full:true}`, optional `reason`, `revoke_access` (explicit; on an NMI-billed subscription it also cancels the membership and deletes its NMI schedule, and is refused `409 provider_cancel_held` while destructive actions are disarmed). 201 settled, 202 pending; `refund_rail_unavailable`/`refund_unsupported` refusals. `Client.RefundPayment` |
| GET | `/v1/merchant/payment-attempts` | `merchant:payments:read` | Every authorization a PSP answered (verifications, sales, rebills, retries), newest first, a cursor page. Filters `kind`, `owner`, `category`, `reason`, `response_code`, `card_entry`, `source`, `observed_via`, `avs_result`, `cvv_result`, `psp_id`, `customer_id`, `checkout_id`, `subscription_id`, `cycle_id`, `since`/`until` (RFC3339); a text filter takes a comma-separated list (`kind=verify,initial`). `Client.ListPaymentAttempts` |
| GET | `/v1/merchant/payment-attempts/{id}` | `merchant:payments:read` | One attempt (`att_…`). `Client.GetPaymentAttempt` |
| GET | `/v1/merchant/rebill-cycles` | `merchant:payments:read` | Rebill cycles (paid periods that came due), latest due first, a cursor page, with `first_outcome`, `outcome` (`collected`, `lost`, `open`), `recovered_by`. Filters `owner`, `first_outcome`, `miss_reason`, `outcome`, `psp_id`, `subscription_id`, `due_since`/`due_until`; a text filter takes a comma-separated list. `Client.ListRebillCycles` |
| GET | `/v1/merchant/rebill-cycles/{id}` | `merchant:payments:read` | One cycle (`cyc_…`) with its attempts, oldest first. `Client.GetRebillCycle` |
| GET | `/v1/merchant/purchase-reviews` | `merchant:payments:read` | Purchases a product archive recorded for review, a cursor page (`status=open\|refunded\|dismissed`, `product_archive_id`); `Client.ListPurchaseReviews` |
| POST | `/v1/merchant/purchase-reviews/{id}/resolve` | `merchant:payments:refund` | `{decision: refund\|dismiss, notes}`; refund returns the remaining amount and ends the purchase's access; `Client.ResolvePurchaseReview` |
| GET | `/v1/merchant/subscriptions` | `merchant:subscriptions:read` | List subscriptions with filters (`customer_id`, `status`, `rail`, `price_id`, ...); `Client.ListSubscriptions` |
| GET | `/v1/merchant/subscriptions/{id}` | `merchant:subscriptions:read` | One subscription |
| POST | `/v1/merchant/subscriptions/{id}/cancel` | `merchant:subscriptions:update` | Cancel; answers the `Subscription`. `revoke_access` must be explicit to revoke entitlements immediately. A Solana subscription is canceled only by the customer's wallet (`403 customer_action_required`). An NMI-billed cancel while the destructive switch is off answers `409 provider_cancel_held` and raises `life.provider_cancel.held`; `account_deletion: true` cancels locally and holds the NMI delete instead |
| POST | `/v1/merchant/subscriptions/{id}/resume` | `merchant:subscriptions:update` | Undo a scheduled cancel where the rail supports it; answers the `Subscription` |
| POST | `/v1/merchant/subscriptions/{id}/change-tier` | `merchant:subscriptions:update` | Apply a same-group tier change. Body `{ "price_id": "..." }` |
| POST | `/v1/merchant/subscriptions/{id}/change-tier/preview` | `merchant:subscriptions:update` | Preview the same tier change without mutation |
| PUT | `/v1/merchant/subscriptions/{id}/payment-method` | `merchant:subscriptions:update` | Reassign to another saved method of the same customer; answers the `Subscription` |

### Invoice administration

Full request and state-transition details are in
[invoice-administration.md](../invoice-administration.md).

| Method | Path | Permission | Purpose |
|---|---|---|---|
| GET | `/v1/merchant/invoices` | `merchant:invoices:read` | Invoices, a cursor page; filters `customer_id`, `currency`, `status`, `period_from`, `period_to` |
| GET | `/v1/merchant/invoices/{id}` | `merchant:invoices:read` | Read one invoice and its available actions |
| GET | `/v1/merchant/invoices/{id}/payments` | `merchant:invoices:read` | Collection and remittance history, a cursor page |
| POST | `/v1/merchant/invoices/{id}/payments` | `merchant:invoices:update` | Record an idempotent external remittance; does not charge a provider |
| POST | `/v1/merchant/invoices/{id}/retry-collection` | `merchant:invoices:collect` | Start or replay one durable collection operation with an explicit saved method and idempotency key (202 while unresolved) |
| POST | `/v1/merchant/invoices/{id}/uncollectible` | `merchant:invoices:update` | Stop collection while retaining the debt |
| POST | `/v1/merchant/invoices/{id}/void` | `merchant:invoices:update` | Void an eligible invoice and write off its remaining debt |

### Reprices & plan migrations

A reprice moves one subscription to another price at its first renewal on or
after `effective_at`. A reprice batch is one bulk move: every subscriber on a
price key's prior versions (`kind: reprice`), or a plan migration
(`kind: plan_change`). A batch reports `matched` and `skipped` from creation
and counts its reprices by status now.

| Method | Path | Permission | Purpose |
|---|---|---|---|
| POST | `/v1/merchant/reprice-batches` | `merchant:subscriptions:update` | Move every subscriber on a prior version of `price_key` to its current price |
| POST | `/v1/merchant/reprice-batches/preview` | `merchant:subscriptions:read` | Affected-count dry run for `price_key` |
| GET | `/v1/merchant/reprice-batches` | `merchant:subscriptions:read` | Batches and plan migrations, newest first (`?price_key=`) |
| GET | `/v1/merchant/reprice-batches/{id}` | `merchant:subscriptions:read` | One batch |
| POST | `/v1/merchant/reprice-batches/{id}/cancel` | `merchant:subscriptions:update` | Cancel the batch's still-scheduled reprices |
| POST | `/v1/merchant/plan-migrations` | `merchant:subscriptions:update` | Cross-product bulk plan retirement (plan A → plan B); creates a `plan_change` batch |
| POST | `/v1/merchant/plan-migrations/preview` | `merchant:subscriptions:read` | Dry-run preview |
| GET | `/v1/merchant/reprices` | `merchant:subscriptions:read` | Reprices, newest first (`?subscription_id=&reprice_batch_id=&status=`) |
| GET | `/v1/merchant/reprices/{id}` | `merchant:subscriptions:read` | One reprice |
| POST | `/v1/merchant/reprices/{id}/cancel` | `merchant:subscriptions:update` | Cancel a scheduled reprice; answers the reprice |
| POST | `/v1/merchant/subscriptions/{id}/provider-cutover/preview` | `merchant:subscriptions:read` | Validate per-user account cutover |
| POST | `/v1/merchant/subscriptions/{id}/provider-cutover` | `merchant:subscriptions:update` | Execute or resume the original durable cutover |
| GET | `/v1/merchant/subscriptions/{id}/provider-cutover` | `merchant:subscriptions:read` | Read cutover by idempotency_key |
| POST | `/v1/merchant/provider-refresh` | `merchant:subscriptions:update` | Run the merchant's provider refresh now; `202 {status: queued\|already_running, job_id}`. `Client.RefreshProviders` |

### Catalog (`/v1/merchant/catalog`)

One shape per noun: a product (with its current `prices`), a price, a meter
and a rate override are the same object on every route that returns them, in
the Go client (`billing.Product`, `billing.Price`, `billing.Meter`,
`billing.RateOverride`) and on the wire. Lists are `{data, next_cursor}`;
pass `cursor` for the next page. A price's cadence is `access_duration_hours`
(null: for good) and `auto_renew`. `psps` maps each PSP key to the price's
state on it.

Reads need `merchant:catalog:read`; writes need `merchant:catalog:update` and
`allow_catalog_updates: true` (catalog writes are not mounted otherwise; the
in-process Client is not gated). While `Config.Catalog` declares the catalog,
writes answer 405 `catalog_declared`.

A creator's own catalog uses the `/v1/catalog` prefix with the same product,
price and offer routes, under `merchant:catalog:read-own` /
`merchant:catalog:update-own`; the catalog is created on the creator's first
write. The owner is the Gate-verified subject, or the
`OpenRails-Catalog-Owner` header (base64url subject) an administrator selects
(`Client.ForCatalogOwner`). Creators cannot set entitlements, tier groups or
PSP links.

| Method | Path | Purpose |
|---|---|---|
| POST | `/v1/merchant/catalog/products` | Create a product |
| GET | `/v1/merchant/catalog/products` | List products, newest first. Query: `catalog_id`, `archived`, `tier_group` |
| GET | `/v1/merchant/catalog/products/{id}` | One product |
| PATCH | `/v1/merchant/catalog/products/{id}` | Merge patch: omitted fields stay, `null` clears `description`, `entitlements_spec`, `tier_group`; `archived` takes it off sale |
| GET | `/v1/merchant/catalog/products/by-key/{key}` | Product by key |
| PUT | `/v1/merchant/catalog/products/by-key/{key}` | Create the product unless it exists; an existing one is returned unchanged |
| POST | `/v1/merchant/catalog/prices` | Create a price on exactly one of `product_id`, `product_key`, `product_data`; `psps` lists the PSPs that sell it, `psp_links` their identifiers. The same key with other terms makes a new version and archives the old one |
| GET | `/v1/merchant/catalog/prices` | List prices, newest first. Query: `catalog_id`, `product_id`, `currency`, `auto_renew`, `archived` |
| GET | `/v1/merchant/catalog/prices/{id}` | One price; `?verify=true` reads each linked PSP's copy and reports drift |
| PATCH | `/v1/merchant/catalog/prices/{id}` | `key` moves the price onto a key (archiving the live price that held it), `archived` takes it off sale or back, `psp_links` merges links (a PSP set to `null` is unlinked) |
| GET | `/v1/merchant/catalog/prices/by-key/{key}` | The price a key currently names |
| GET | `/v1/merchant/catalog/prices/by-key/{key}/history` | When the key moved to which price, most recent first |
| POST | `/v1/merchant/catalog/offers/lookup` | Live offers per entitlement: `{entitlements (max 100), kind (permanent, finite, recurring), preferred_currency?, limit?, cursors?}` → `{entitlement: {data, next_cursor}}` |
| GET | `/v1/merchant/catalog/revision` | The catalog revision and whether writes are accepted |
| POST | `/v1/merchant/catalog/applications` | Apply a JSON or YAML catalog document |
| GET | `/v1/merchant/catalog/drift` | Open findings that a PSP's copy differs (alert-only). Query: `rail`, `kind`, `resource_type` |
| POST | `/v1/merchant/catalog/drift/refresh` | Read every linked PSP's catalog now and record its drift |
| GET | `/v1/merchant/catalog/meters` | List meters by key |
| GET | `/v1/merchant/catalog/meters/{key}` | One meter with its rate card |
| PUT | `/v1/merchant/catalog/meters/{key}` | Declare a meter; one with recorded usage keeps its definition (`meter_in_use`) |
| PUT | `/v1/merchant/catalog/meters/{key}/rate-card` | Set the rate card that prices the meter's usage |
| DELETE | `/v1/merchant/catalog/meters/{key}/rate-card` | Remove it once no customer has an override (`rate_card_has_overrides`) |
| GET | `/v1/merchant/catalog/meters/{key}/rate-overrides` | The customers whose negotiated price replaces the rate card |
| GET | `/v1/merchant/customers/{customer_id}/rate-overrides` | A customer's negotiated prices (`merchant:customer-settings:read`) |
| PUT | `/v1/merchant/customers/{customer_id}/rate-overrides/{meter_key}` | Set a customer's price for one meter (`merchant:customer-settings:update`) |
| DELETE | `/v1/merchant/customers/{customer_id}/rate-overrides/{meter_key}` | Remove it (`merchant:customer-settings:update`); 204 |
| POST | `/v1/merchant/catalog/product-archives` | Archive a product and refund or review its recent one-time purchases (also `merchant:payments:refund`; `Idempotency-Key`) |
| GET | `/v1/merchant/catalog/product-archives/{id}` | An archive operation and each purchase's outcome (also `merchant:payments:read`) |
| GET | `/v1/merchant/catalogs` | The merchant's catalogs, oldest first. Query: `owner_subject` selects one creator's |
| POST | `/v1/merchant/catalogs` | `{owner_subject}`: return the creator's catalog, creating it the first time |
| GET | `/v1/merchant/catalogs/{id}` | One catalog |
| POST | `/v1/merchant/catalog/ask` | Catalog copilot Q&A (mounted when configured; never mutates) |
| POST | `/v1/merchant/catalog/copilot/confirm` | Record that a copilot draft was applied |
| — | `/v1/catalog/products…`, `/v1/catalog/prices…`, `/v1/catalog/offers/lookup` | A creator's own catalog: the product, price and offer routes above |

### Payment providers (`/v1/merchant/payment-providers`)

Reads: `merchant:payment-providers:read`; writes: `merchant:payment-providers:update`
Host-owned credential mode omits all provider mutation routes. In API-owned
credential mode, create/update requires a writable secret backend; archive
routes remain available with a read-only secret backend. Reads and routing
dry runs remain mounted in both source modes.

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/merchant/payment-providers` | List configured providers |
| GET | `/v1/merchant/payment-providers/{provider}` | One provider's config (redacted) |
| PUT | `/v1/merchant/payment-providers/{provider}` | Create/update provider config + secrets. Live-probes the supplied or stored credentials before writing, including `{"account_id","enabled":false}` — it cannot archive an account whose provider is dark |
| POST | `/v1/merchant/payment-providers/{provider}/accounts/{psp_id}/archive` | Archive exactly this account by its immutable `id`. No provider call, credentials kept, idempotent; optional body `{"allow_last": true}` |
| DELETE | `/v1/merchant/payment-providers/{provider}` | Archive the rail's single active account (no provider call). More than one active: `409 provider_accounts_ambiguous` — use the per-account archive |
| POST | `/v1/merchant/payment-providers/routing/dry-run` | Explain which PSP a checkout would get, and why every other candidate was skipped. Read permission — creates nothing (or#288) |

Archive is not deletion (#655): the row, its `id`, credentials and history
remain, existing obligations and inbound webhooks keep resolving to it, and
new checkout selects only active accounts. Archiving the rail's last active
account answers `409 provider_account_last_active` (metadata: `psp_id`)
unless `allow_last` is `true`; new checkout on that rail is then refused until
another account is armed. Both archives are lifecycle writes: in API-owned credential mode they stay
mounted when the secret backend is read-only.

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

On deployments without a control plane these routes are not registered (404), like every other capability the deployment cannot serve: the LLM routes (`/dashboard/widgets/generate`, `/metrics/ask`, `/catalog/ask`, `/catalog/copilot/confirm`) exist only with `llm.api_key` and the matching consent, and `/api-host` only when the merchant directory is armed. `GET /v1/capabilities` and the console's `config.json` advertise what is mounted.

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
| POST | `/v1/webhooks/{provider}/{account_id}` | Standalone: the receiving PSP account is explicitly pinned in the path and its credentials verify the callback |
| POST | `/billing/v1/webhooks/{provider}/{account_id}` | Embedded under /billing: the configured account resolves its merchant; runtime bindings and account credentials verify the callback |

`{provider}` is the gateway KIND — `nmi`, `ccbill`, `stripe`, `solana`,
`basistheory`. It is never a PSP key: `mobius` and `paykings` both post to
`/v1/webhooks/nmi/{account_id}` and are distinguished by the explicit receiving
account. Callback account information must agree with that selected account.

Deployments using per-merchant hostnames (`api.<slug>.<domain>`) additionally
serve account-explicit webhook routes with the merchant resolved from the Host
header. Accountless routes are not mounted.

Verification per rail:

- **NMI** (`/v1/webhooks/nmi/{account_id}`): JSON body; `Webhook-Signature`
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
contains the original `pay_` payment id, its payer (`customer_id`), `price_id`, the
renewed `subscription_id` when any, amount, currency, merchant and settlement
time, preserving the fee-attribution coordinate.

`include_acknowledged=true` includes retained acknowledged rows; `payment_id`
selects one payment's event. Acknowledged rows are retained for 30 days by default;
pending rows survive retention. Hosts should filter by event type so one
consumer's pending work cannot starve another type.

`GET /v1/merchant/customers/{customer_id}/payment-settlement-status?price_id=...`
backs `Client.GetPaymentSettlementStatus` and requires `merchant:payments:read`. It reads
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
