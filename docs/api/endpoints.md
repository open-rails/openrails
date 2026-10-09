# OpenRails HTTP API

The API is served on one port under `/v1`. A standalone server serves it at the
root; an embedded host mounts the same routes beneath its prefix (usually
`/billing`), so `/v1/me/balance` there is `/billing/v1/me/balance`.

- **Every route**, with its tier, permission, request and response:
  [routes.md](routes.md).
- **Every shape and each route's error codes**:
  [`api/openapi.json`](../../api/openapi.json).
- **Every error code**: [error-codes.md](error-codes.md).

All three are generated from the route catalog. This page holds what they
cannot say: the conventions every route follows, and how the larger operations
behave. From v1.0.0 the API [changes only by addition](../compatibility.md).

## Conventions

- **Bodies.** JSON in, JSON out. A request body may hold only the fields its
  route declares: an unknown one is `400 unknown_field`, a malformed declared
  query parameter `400 invalid_query`, a body that is not JSON
  `415 unsupported_media_type`, and one over 1 MiB `413 request_body_too_large`.
- **Lists.** `{"data": [...], "next_cursor": "..."}`. Pass `next_cursor` back as
  `?cursor=`; it is `null` on the last page. `?limit=` is 1 to 500, 50 by
  default. There is no offset and no total.
- **Values.** Money is a decimal string of the currency's native units beside a
  `currency` ([money on the wire](../money-wire.md)). Times are RFC 3339 in UTC.
  A member with no value is `null`, never left out; an empty list is `[]`.
  Ids are prefixed text (`sub_…`, `pay_…`, `psp_…`); customer and merchant ids
  are UUIDs.
- **Statuses.** `200` a result, `201` a creation, `202` accepted and still
  unresolved, `204` done with no body. A `DELETE` answers `204`.
- **Errors.** `{"error": {"type", "code", "message", "param", "metadata"}}`
  ([errors](errors.md)). The `code` is stable; the `message` is not contract.
- **Idempotency.** Send `Idempotency-Key` only on the routes that read it
  (marked in [routes.md](routes.md)): checkout, refunds, tier changes, invoice
  collection. Each such operation keeps its own durable receipt: the same key
  with the same terms answers the original result, and with other terms
  `409 idempotency_key_reused`. There is no generic response cache; every
  request authenticates and authorizes again.
- **Discovery.** `GET /v1/capabilities` (public, cacheable) lists the route
  groups a deployment mounts and its features (`stripe_billing_portal`,
  `solana_one_time_payments`, `solana_subscription_management`,
  `provider_credential_writes`, `api_host`, `catalog_copilot`, `metrics_ask`,
  `dashboard_generation`, `team_invites`). A route the deployment cannot serve
  is not registered: it answers `404 route_not_found`.
- **Health.** `/health/live` and `/health/ready` on the standalone server; a
  failing dependency is logged, never answered.

## Who may call what

| Routes | Credential |
|---|---|
| Public: `/v1/products`, `/v1/prices`, `/v1/currencies`, `/v1/checkout-config`, `/v1/capabilities` | none |
| Checkout sessions: `/v1/checkout-sessions/{id}` | the session id (`ocs_…`) in the path |
| Customer: `/v1/me/*` | embedded: the host's own user credential. Standalone: a trusted issuer's access token with scope `openrails:self`, as `Authorization: DPoP <token>` with a fresh `DPoP` proof ([auth](../auth.md#trusted-issuers)) |
| Merchant: `/v1/merchant/*` | an API key (`openrails_st_…`), a user session, or a trusted issuer's access token with scope `openrails:merchant` |
| Control plane: `/v1/merchants`, the team and API-key routes | a signed-in user, or a trusted issuer's access token (standalone) |
| Platform: `/v1/platform/*` | an operator session holding the root permission (standalone) |
| Provider webhooks: `/v1/webhooks/{rail}/{account_id}` | the provider's signature |

Every merchant route is gated by one `merchant:` permission, whatever the
credential: an API key carries the permissions it was minted with, a service
JWT the `permissions` it asserts within its issuer's merchant, and a user
session the user's role in the merchant's group. A user session also needs a
recent sign-in for writes: otherwise `403 step_up_required`, with the step-up
methods in `metadata`.

A `/v1/me` route acts on the credential's own customer; no path names a user.
The merchant a request acts on comes from its credential, or from the
`OpenRails-Merchant` header ([choosing the merchant](../client-merchant-selection.md)).

Browser-facing routes (the public catalog, checkout, `/v1/me`) answer
`Access-Control-Allow-Origin: *` and never set credentials; every other route
sends no CORS headers.

## Checkout

A browser buys through a checkout session; a merchant's server can also charge
directly with a checkout attempt. See [checkout](commerce.md).

`GET /v1/checkout-config` is public and cacheable for a minute. It lists the
merchant's armed PSPs as `{psp_id, key, rail, custodian, display_name, flow,
checkout, config}`: `flow` is `tokenize`, `elements`, `redirect` or `wallet`,
`checkout` marks the PSPs that take new purchases, and `config` holds only
values that are public by nature (an NMI `tokenization_key`, a Stripe
`publishable_key`, a Basis Theory `public_api_key`). With a Solana PSP armed,
`solana` carries the network and accepted tokens. The merchant's
`GET /v1/merchant/checkout-config` adds, for a `price_id` or a `product_key` and `price_key`, the
`options` that can sell that price.

## Subscriptions

Cancel, resume and the payment-method switch answer the `Subscription`, in the
request. Cancel ends access at the period end; `revoke_access` on the merchant
route must be explicit to end it now. Resume undoes a scheduled cancel where
the rail can (not on CCBill or Solana).

A Solana subscription is changed and canceled in the customer's wallet: cancel
and change-tier answer `next_action: {type: "solana_sign_transactions",
transactions}`; the wallet signs and sends them, and the same request repeated
with `signature` mirrors the landed transaction. Nothing changes before the
chain confirms it, and a merchant cannot do it for the customer
(`403 customer_action_required`).

On an NMI-scheduled subscription, a cancel while the merchant's destructive
switch is off answers `409 provider_cancel_held`: OpenRails will not cancel
locally while NMI would keep charging. `account_deletion: true` cancels locally
and holds the NMI delete.

### Tier changes

`POST /v1/me/subscriptions/{id}/change-tier` (and its merchant twin) moves a
subscription to another price of its tier group. It answers a `TierChange`:
`status` (`succeeded`, `processing`, `requires_action`, `blocked`), `action`
(`upgrade`, `downgrade`), `effective` (`now`, `period_end`), `amount_due_now`,
`next_charge_amount`, `next_charge_date`, and a `next_action` or `operation_id`
when the customer must act. `…/change-tier/preview` answers the same numbers
and changes nothing.

- **`Idempotency-Key` is required** (`400 tier_change_idempotency_key_required`):
  it is the client's only handle on a lost response. The same key replays the
  stored result (`200`); while the provider outcome is unresolved it answers
  `202` with `status: "processing"`; a key that names a different change is
  `409 tier_change_idempotency_conflict`; another key while one is unresolved
  is `409 tier_change_in_flight`.
- **An upgrade** is effective now. The customer pays the new price less the
  unused share of the current period, for a fresh period of the new price's
  cycle. A declined charge changes nothing (`402` with the decline reason); a
  card challenge answers `requires_action` with `next_action.type:
  "payment_authentication"` and `operation_id` (authenticate at
  `/v1/me/payment-operations/{id}/authentication`, then repeat the request).
- **A downgrade** is effective at period end: nothing is charged or refunded,
  and the next renewal bills the new price. Another pending change is
  `409 tier_change_already_scheduled`.
- **Refusals.** `409 tier_change_renewal_due` while the period has ended or its
  renewal is unresolved; `422 tier_change_cycle_unknown` and
  `422 tier_change_period_unknown` when proration has nothing to measure;
  `409 tier_change_credit_exceeds_price` when the unused credit is larger than
  the target price.
- **NMI-scheduled subscriptions** keep their billing date: the schedule's
  amount changes in place, so only a price of the same billing cycle qualifies
  (`409 tier_change_cadence_unsupported`), and a schedule on a named NMI plan
  needs a linked plan on the target price (`409 tier_change_requires_linked_plan`).
- **CCBill** upgrades answer a `redirect_to_url`; downgrades are `blocked`.

## Payment methods

A payment method is `{id, customer_id, rail, psp_id, card, billing_details,
health, subscriptions, collection_currencies, created_at}`. `card` is the one
card shape of the API, `{brand, last4, exp_month, exp_year}`, each `null` when
the provider did not report it. `psp_id` is `null` for a card a third-party
custodian holds: each charge routes to the one live PSP of its rail that
reaches the custodian. There is no default card: a charge names its card, and
`collection_currencies` lists the currencies whose invoices the card collects
(set with `PUT /v1/me/collection-payment-method`).

- **Save** (`POST /v1/me/payment-methods`): `psp_id`, a `payment_token` from
  the PSP's card fields (or `card` for a PSP whose card entry is server), and
  optional `billing_details`. OpenRails reads brand, last four and expiry from
  the PSP. A save the PSP refuses is `502 payment_provider_rejected`.
- **Replace** (`PUT`): the updated method when confirmed, `202` with no body
  while the provider outcome is still being resolved (repeat the same request),
  `409 payment_method_update_retry_required` when a fresh token is needed.
- **Delete**: `204` when the provider and the local record are both gone, `202`
  while that converges. Stripe cards are managed in Stripe's billing portal
  (`POST /v1/me/billing-portal`).
- **Stripe cards** are saved through a setup: `POST /v1/me/payment-method-setups`,
  Stripe.js confirms it in the page, then `…/{id}/confirm`.

A merchant reads a customer's cards and may delete one; it can never create or
change one.

A customer pays an open invoice (`POST /v1/me/invoices/{id}/pay-now`) or
retries a past-due subscription (`POST /v1/me/subscriptions/{id}/retry-now`)
with a saved card and an `Idempotency-Key`: `200` complete, `202` unresolved, a
coded `402` refusal. See [customer payment recovery](../architecture/customer-payment-recovery.md).

## Customers, credit and usage

- A **customer** is created by its first use or declared with
  `PUT /v1/merchant/customers/{customer_id}`. Its balance, credit limit, trust
  level, spend delegations, credit grants and ledger all live beneath that
  path.
- A **credit grant** (`POST …/credit-grants`) is idempotent on `source_id`: an
  identical retry answers the same grant with `replayed: true`; other terms are
  `409 idempotency_key_reused`. Revoking takes the unspent remainder
  (`409 credit_grant_held` while holds need it).
- **Admissions** authorize spend before work starts and settle it after. See
  [request admission](../admission-operations.md).
- A **usage event** (`POST /v1/merchant/usage-events`) is idempotent on
  `(source, source_id)`; `occurred_at` may be up to 35 days old.
- **Provider operations** authorize and settle upstream compute cost:
  [provider obligations](../architecture/provider-obligation-contract.md).
- **Arrears**: [delinquency](../arrears-delinquency.md) and
  [billing policies](../billing-policies.md).

## Catalog

One shape per noun: a product (with its current `prices`), a price, a meter and
a rate override are the same object on every route that returns them and in the
Go client (`billing.Product`, `billing.Price`, `billing.Meter`,
`billing.RateOverride`). A price's cadence is `billing_interval_hours` (`null`:
one-time); `access_duration_hours` independently determines access (`null`: no
scheduled expiry). `psps` maps each PSP key to the price's state on
it; the public routes show the status only.

Reads need `merchant:catalog:read`. Writes need `merchant:catalog:update` and
`Routes.CatalogEdits` (standalone: `catalog_edits: true`): without it the write
routes are not mounted (the in-process Client is not gated). `Config.Catalog` is an optional startup batch
and does not restrict later edits. JSON/YAML batches are deduplicated permanently
by content hash, even after intervening edits.

A price's terms never change: the same key with other terms makes a new version
and archives the old one. Price keys are product-local and immutable; each has
automatic revisions starting at zero. `PATCH` archives or restores a price and
merges `psp_links`. `GET …/prices/{id}?verify=true` reads each linked PSP's
copy and reports drift; `GET /v1/merchant/catalog/drift` lists the open drift
findings, each with the `psp_id` that was compared.

## PSPs

A PSP is one merchant account on a rail (`mobius` and `paykings` are two PSPs on
`nmi`). `key` is the merchant's name for it, unique among its live PSPs; price
`psp_links` name PSPs by it. Creating one checks its credentials with the
provider before anything is stored; `PATCH` changes settings or rotates
credentials against the `expected_revision` it read. Credential writes need a
writable secret backend (`credential_source_read_only`,
`credential_store_read_only` otherwise); settings changes and archive do not.

Archive is not deletion: the row, its id, credentials and history remain, and
existing subscriptions, operations and inbound webhooks keep resolving to it
until they drain. New checkout selects only active PSPs. Archiving the last
active PSP on a rail is `409 psp_last_active` unless `allow_last` is `true`.

## Refunds

`POST /v1/merchant/payments/{id}/refunds` refunds through the rail: `{amount}`
or `{full: true}`, an optional `reason`, and an explicit `revoke_access`.
`Idempotency-Key` is required. `201` settled, `202` pending. A rail with no
automatic refund is `400 refund_unsupported`; one that cannot take it now is
`409 refund_rail_unavailable`.

## Host events

`GET /v1/merchant/host-events` is the feed a host drains: `payment.settled`,
`delinquency.grace`, `delinquency.entered`, `delinquency.cleared`, oldest
first, filtered by `type`. Acknowledge each
(`POST /v1/merchant/host-events/{id}/acknowledge`) after the host's own
processing commits, then fetch again. Acknowledgment is idempotent and
independent of notification read state. Acknowledged events are kept 30 days;
pending ones are never deleted.

`GET /v1/merchant/customers/{customer_id}/payment-settlement-status?price_id=`
answers whether the customer ever paid for that price on a rail, from the
durable payment records.

## Provider webhooks

`POST /v1/webhooks/{rail}/{account_id}` (beneath the mount prefix when
embedded). `{rail}` is the gateway kind (`nmi`, `ccbill`, `stripe`,
`basistheory`), never a PSP key: `mobius` and `paykings` both post to
`/v1/webhooks/nmi/{account_id}` and are told apart by their account. The
account resolves the merchant; the provider's signature or source address is
then verified with that account's own secret, and the payload's account must
agree. There is no bearer credential and no accountless route. Success is
`200`.

| Rail | Verification |
|---|---|
| NMI | `Webhook-Signature` (`t=…,s=…`), HMAC over the timestamp and body |
| Stripe | `Stripe-Signature` with the endpoint's signing secret |
| CCBill | CCBill's published source address ranges, plus the payload's account |

## Alert webhooks

An outbound alert webhook's URL is a write-only credential, path and query
included: reads show `destination_host` only. `PUT …/{id}/url` rotates it and
keeps the webhook's identity.

## Standalone only

The team and API-key routes (`/v1/merchant/team`, `/v1/merchant/api-keys`),
`/v1/merchants` and `/v1/platform/*` exist only with a control plane. API keys
are minted with a fixed role (`viewer`, `support`, `owner`); the secret is
shown once, and a caller can never mint beyond its own authority.
