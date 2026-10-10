# OpenRails HTTP API

The API is served on one port under `/v1`. A standalone server serves it at the
root; an embedded host mounts the same routes beneath its prefix (usually
`/billing`), so `/v1/me/balance/transactions` there is `/billing/v1/me/balance/transactions`.

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
- **Several known records.** Every merchant list of records with ids takes
  `?ids=a,b,c`: 1 to 100 ids, answered in one page in the list's order,
  whatever their state. An unknown id, or another merchant's, is absent. `ids`
  takes no other parameter beside it; anything else, or more than 100, is
  `400 invalid_query`.
- **Batches.** A write or lookup of many items takes 1 to 100 (usage events and
  admissions: 1,000). A batch either applies all or none, refusing with the
  offending `items[i].field`, or answers one `{status, …, error}` result per
  item, decided on its own.
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
  `422 idempotency_key_reused`. There is no generic response cache; every
  request authenticates and authorizes again. One key names one purchase:
  generate it when the buyer decides to buy, store it with the order, and send
  the same key on every retry of that request (timeouts, network errors,
  restarts). A new key, or a new checkout session per click, is a new
  purchase: a subscription or permanent product is refused as a duplicate, but
  a timed pass is sold again (it starts when the previous one ends) and a
  credit pack grants its credits again.
- **Discovery.** `GET /v1/config` (public, cacheable) is the deployment's
  configuration (see [below](#public-configuration)). Its `capabilities` list
  the route groups the mount serves and its features (`stripe_billing_portal`,
  `solana_one_time_payments`, `solana_subscription_management`,
  `merchant_config_edits`, `api_host`, `catalog_copilot`, `metrics_ask`,
  `dashboard_generation`). Its route groups are `admin`
  (customer support), `catalog`, `merchant_config` (the merchant's own
  configuration), `metrics` (business metrics) and `app` (the host backend's
  programmatic routes); `merchant_config_edits` says that configuration can
  change here (Vault holds it) rather than being read from a file. What a
  signed-in staff member holds of them is `GET /v1/admin/access`. A route the
  deployment cannot serve
  is not registered: it answers `404 route_not_found`.
- **Health.** `/health/live` and `/health/ready` on the standalone server; a
  failing dependency is logged, never answered.

## Who may call what

| Routes | Credential |
|---|---|
| Public: `/v1/config`, `/v1/catalog/products` | none |
| Checkout sessions: `/v1/checkout-sessions/{id}` | the session id (`ocs_…`) in the path |
| Customer: `/v1/me/*` | embedded: the host's own user credential. Standalone: a trusted issuer's access token with scope `openrails:self`, as `Authorization: DPoP <token>` with a fresh `DPoP` proof ([auth](../auth.md#trusted-issuers)) |
| Admin, catalog, merchant configuration and metrics: `/v1/admin/*` | embedded: the host's credential its permission admits. Standalone: a user session, or a trusted issuer's access token with scope `openrails:merchant`; on a hosted product, a merchant API key |
| Programmatic: `/v1/app/*` | the host backend's application credential, never a person's, holding the route's permission; each write sends an `Idempotency-Key`. SCIM names no permission and also takes the merchant's provisioning token |
| Provider webhooks: `/v1/webhooks/{rail}/{account_id}` | the provider's signature |

Every staff and programmatic route is gated by the host's permission for it
(`Routes.Permissions`; each route's is in [routes.md](routes.md)), whatever
the credential. On the standalone server the permissions are its merchant
persona's ([auth](../auth.md#permissions)): an API key carries its role's, a trusted issuer's token
the `permissions` it asserts within its ceiling, and a user session the user's
role in the merchant's group. A person also needs a recent sign-in on a
`sensitive` route: otherwise `401 step_up_required` (RFC 9470: `WWW-Authenticate:
Bearer error="insufficient_user_authentication", max_age="900"`), with the
step-up methods in `metadata`, or `403 step_up_unavailable` for a credential
with no sign-in of its own; an application never steps up. A `/v1/app` route refuses a person
(`403 application_required`), and answers a retried write's `Idempotency-Key`
with the first response; the key sent with another request is
`422 idempotency_key_reused`.

A `/v1/me` route acts on the credential's own customer; no path names a user.
The merchant a request acts on comes from its credential, or from the
`OpenRails-Merchant` header ([choosing the merchant](../client-merchant-selection.md)).

Browser-facing routes (the public catalog, checkout, `/v1/me`) answer
`Access-Control-Allow-Origin: *` and never set credentials; every other route
sends no CORS headers.

## Checkout

A browser buys through a checkout session: the customer mints it with their own
credential, or the merchant with its own. See [checkout](commerce.md).

## Public configuration

`GET /v1/config` is public, always mounted and cacheable for five minutes (with
an `ETag`). It holds what a browser needs before anything else:

- `capabilities`: the route groups the mount serves and its features.
- `currencies`: every currency's `decimals` (the native scale) and
  `minor_decimals`.
- `payment`: the merchant's browser payment setup, null when the request
  resolves no merchant. `psps` lists the armed PSPs as `{psp_id, key, rail,
  custodian, display_name, flow, checkout, config}`: `flow` is `tokenize`,
  `card`, `elements`, `redirect` or `wallet`, `checkout` marks the PSPs that
  take new purchases, and `config` holds only values that are public by nature
  (an NMI `tokenization_key`, a Stripe `publishable_key`, a Basis Theory
  `public_api_key`). A PSP whose credentials could not be checked just now has
  `status: "temporarily_unavailable"` and `retry_after` (seconds) instead of
  `config`, and then the document is not cached. With a Solana PSP armed,
  `solana` carries the network and accepted tokens.
- `rails`: every rail a PSP can be declared on, with the credential and
  setting keys it takes.
- `captcha`: the challenge to solve when a request answers 403
  `captcha_required` — `{provider, site_key, script_url, action,
  token_header}`: load `script_url`, solve with `site_key` and `action`, and
  resend the request with the token in `token_header`. Null when the
  deployment challenges nobody.

`GET /v1/admin/checkout-options` lists, for a `price_id` or a `product_key`
and `price_key`, the options that can sell that price.

## Subscriptions

Cancel, resume and the payment-method switch answer the `Subscription`, in the
request. Cancel ends access at the period end; `revoke_access` on the merchant
route must be explicit to end it now. Resume undoes a scheduled cancel where
the rail can (not on CCBill or Solana).

A Solana subscription is changed and canceled in the customer's wallet: cancel
and change answer `next_action: {type: "solana_sign_transactions",
transactions}`; the wallet signs and sends them, and the same request repeated
with `signature` mirrors the landed transaction. Nothing changes before the
chain confirms it, and a merchant cannot do it for the customer
(`403 customer_action_required`).

On an NMI- or CCBill-scheduled subscription, a cancel while the merchant's
destructive switch is off answers `409 provider_cancel_held`: OpenRails will
not cancel locally while the provider would keep charging.
`account_deletion: true` cancels locally and holds the provider stop.

Until the provider confirms its schedule stopped, a canceled subscription keeps
the customer's place in its product and tier group: buying it again answers
`409 resource_conflict` (resume it instead, or buy once the stop completes).
While a canceled subscription is still paid, buying its product or tier group
again charges nothing: it answers `409 subscription_resumable` when the
subscription can be resumed, else `409 subscription_paid_through` until its
paid period ends. A
charge the provider still takes after the cancel is refunded in full and
raises a `life.charge_after_cancel` finding. An OpenRails-billed subscription
whose renewal charge was sent and is not yet settled refuses a cancel with
`409 payment_in_progress`; cancel again once it settles.

### Subscription changes

`POST /v1/me/subscriptions/{id}/change` (and its admin twin) moves a
subscription to another price of its tier group, to other seats, or both:
`{price_id?, quantity?}`. It answers a `SubscriptionChange`: `status`
(`succeeded`, `processing`, `requires_action`, `blocked`), `effective` (`now`,
`period_end`), `price_id`, `quantity`, `amount_due_now`, `next_charge_amount`,
`next_charge_date`, and a `next_action` or `operation_id` when the customer
must act. `…/change/preview` answers the same numbers and changes nothing.

- **Seats** exist only on a price sold per seat: its catalog `quantity: {min,
  max}`. Its unit amount is one seat's, and `quantity` stays within the bounds.
  A quantity for any other price is `422 quantity_not_allowed`; seats are
  billed only by OpenRails on NMI or Stripe
  (`400 subscription_change_unsupported_on_rail` otherwise).
- **`Idempotency-Key` is required** (`400 subscription_change_idempotency_key_required`):
  it is the client's only handle on a lost response. The same key replays the
  stored result (`200`); while the provider outcome is unresolved it answers
  `202` with `status: "processing"`; a key that names a different change is
  `409 subscription_change_idempotency_conflict`; another key while one is
  unresolved is `409 subscription_change_in_flight`.
- **More seats** are effective now: the customer pays the unit price for the
  added seats over the rest of the period, and renewals bill every seat.
- **An upgrade** is effective now. The customer pays the new price × seats
  less the unused share of the current period, for a fresh period of the new
  price's cycle; a tier change keeps the seats unless the request names
  others. A declined charge changes nothing (`402` with the decline reason); a
  card challenge answers `requires_action` with `next_action.type:
  "payment_authentication"` and `operation_id` (authenticate at
  `/v1/me/payment-operations/{id}/authentication`, then repeat the request).
- **Fewer seats and a downgrade** are effective at period end: nothing is
  charged or refunded, and the next renewal bills them; the subscription shows
  them as its `scheduled_change`. A change to another tier while one is
  pending, or while a price migration is, is
  `409 subscription_change_already_scheduled`.
- **A change back** to the subscription's current price and seats cancels its
  pending change and charges nothing.
- **Staff** change a subscription at the customer's request through
  `POST /v1/admin/subscriptions/{id}/change`, which needs a recent sign-in and
  a `reason`. It behaves as the customer's own change: an upgrade or more
  seats is charged now, merchant-initiated under the card's recurring
  agreement (`409 stored_credential_required` when the card has none
  active), and fewer seats or a downgrade wait for the renewal. The reason and
  the staff member are kept with the change and its payment (the payment's
  `reason`), and the customer gets a `subscription_changed` notice. Staff
  cannot charge a subscription its provider bills
  (`403 customer_action_required`), and a staff change back also cancels a
  pending price migration for that subscription.
- **Refusals.** `409 subscription_change_renewal_due` while the period has
  ended or its renewal is unresolved; `422 subscription_change_cycle_unknown`
  and `422 subscription_change_period_unknown` when proration has nothing to
  measure; `409 subscription_change_credit_exceeds_price` when the unused
  credit is larger than the target price.
- **NMI-scheduled subscriptions** keep their billing date: the schedule's
  amount changes in place, so only a price of the same billing cycle qualifies
  (`409 subscription_change_cadence_unsupported`), and a schedule on a named
  NMI plan needs a linked plan on the target price
  (`409 subscription_change_requires_linked_plan`).
- **CCBill** upgrades answer a `redirect_to_url`; downgrades are `blocked`.

## Payment methods

A payment method is `{id, customer_id, rail, psp_id, card, billing_details,
health, subscriptions, default_currencies, created_at}`. `card` is the one
card shape of the API, `{brand, last4, exp_month, exp_year}`, each `null` when
the provider did not report it. `psp_id` is `null` for a card a third-party
custodian holds: each charge routes to the one live PSP of its rail that
reaches the custodian. `default_currencies` lists the currencies the card is
the customer's default for, and `subscriptions` the subscriptions it pays.

The default has two levels, as in Stripe. A customer's default card per
currency (`PUT /v1/me/default-payment-methods/{currency}`) collects their
invoices there and pays every card subscription in it without its own card.
A subscription's `payment_method_id` is its own card, `null` when it follows
the default; `card` shows the card that pays it. Changing the default moves
every subscription that follows it, and
`PUT /v1/me/subscriptions/{id}/payment-method` (staff:
`Client.SetSubscriptionPaymentMethod`) sets or, with `null`, clears one's own
card. A subscription bought with the default card follows it.

Each move carries the subscription's recurring agreement to the card; a card
with none on the subscription's account is verified first ($0, once per
request; a decline is `402 card_declined` and changes nothing). NMI schedules
follow through the durable schedule swap. A default that cannot pay a
following subscription is `409 payment_method_psp_mismatch`; following a
default not set is `400 default_payment_method_required`.

- **Save** (`POST /v1/me/payment-methods`): `psp_id`, a `payment_token` from
  the PSP's card fields (or `card` for a PSP whose card entry is server), and
  optional `billing_details`. OpenRails reads brand, last four and expiry from
  the PSP. A save the PSP refuses is `502 payment_provider_rejected`.
- **Replace** (`PUT`): the updated method when confirmed, `202` with no body
  while the provider outcome is still being resolved (repeat the same request),
  `409 payment_method_update_retry_required` when a fresh token is needed.
- **Delete**: `204` when the provider and the local record are both gone, `202`
  while that converges. Stripe cards are managed in Stripe's billing portal
  (`POST /v1/me/stripe/billing-portal-sessions`).
- **Stripe cards** are saved through a setup: `POST /v1/me/payment-method-setups`,
  Stripe.js confirms it in the page, then `…/{id}/confirm`.

A merchant reads a customer's cards and may delete one; it can never create or
change one.

A customer pays an open invoice (`POST /v1/me/invoices/{id}/pay-now`) or
retries a past-due subscription (`POST /v1/me/subscriptions/{id}/retry-now`)
with a saved card and an `Idempotency-Key`: `200` complete, `202` unresolved, a
coded `402` refusal. See [customer payment recovery](../architecture/customer-payment-recovery.md).

## Customers, credit and usage

- A **customer** is the host's user id, created by its first use (a purchase,
  a credit grant, a settings change). `GET /v1/admin/customers?ids=` reads up
  to 100, and `?search=` finds them by email, username or name. Its `contact`
  comes from the merchant's directory ([customer contacts](../customer-contacts.md)). Its balance, credit limit, trust level and ledger live
  beneath `/v1/admin/customers/{customer_id}`; its credit grants are
  `/v1/admin/credit-grants?customer_id=`.
- **Credit grants** (`POST /v1/admin/credit-grants`, up to 100 across
  customers, all or none) are idempotent on each customer's `source_id`: an
  identical retry answers the same grant with `replayed: true`; other terms
  refuse the batch with `422 idempotency_key_reused`. Revoking takes the
  unspent remainder (`409 credit_grant_held` while holds need it).
- **Admissions** authorize spend before work starts and settle it after. See
  [request admission](../admission-operations.md).
- **Usage events** (`POST /v1/app/usage-events`, up to 1,000 per call, one
  result per item) are idempotent on `(source, source_id)`; `occurred_at` may be
  up to 35 days old. An item with `outcome: failed` is work that cost and did
  not deliver: the customer's own failures are forgiven up to its policy's
  `bad_spend_windows` and charged past them (`amount` is what was charged,
  `forgiven_amount` what grace absorbed); a delegated invoker's
  (`invoker_type: delegated`) are never charged and count toward
  `delegated_invoker_wasted_spend_limits`, past which admission refuses it
  `failure_rate_limited`. Usage is reported through
  `POST /v1/admin/metrics/query` (`usage_units`, `usage_revenue`,
  `forgiven_usage` by `customer`, `invoker`, `outcome`, `sku` or `rate_card`).
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

Reads and writes are the catalog route group, `RouteGroups.Catalog` with
`Permissions.Catalog`: without it they are not mounted. A document skips what
an edit set ([catalog ownership](../catalog-ownership.md)). The in-process Client
is not gated. JSON/YAML batches are deduplicated permanently by content hash,
even after intervening edits.

A price's terms never change: the same key with other terms makes a new version
and archives the old one. Price keys are product-local and immutable; each has
automatic revisions starting at zero. `PATCH` archives or restores a price and
merges `psp_links`. `GET …/prices/{id}?verify=true` reads each linked PSP's
copy and reports drift; `GET /v1/admin/findings?type=catalog.*` lists the open
drift findings, each with the `psp_id` that was compared, and
`POST /v1/admin/psps/refresh` reads every PSP's catalog again.

## PSPs

A PSP is one merchant account on a rail (`mobius` and `paykings` are two PSPs on
`nmi`). `key` is the merchant's name for it, unique among its live PSPs; price
`psp_links` name PSPs by it. Creating one checks its credentials with the
provider before anything is stored; `PATCH` changes settings, rotates
credentials or archives against the `expected_revision` it read (`409
revision_mismatch` when the PSP moved since). These edits are mounted only
where Vault holds the configuration; with a file, PSPs are read-only.

Archive is not deletion: the row, its id, credentials and history remain, and
existing subscriptions, operations and inbound webhooks keep resolving to it
until they drain. New checkout selects only active PSPs. Archiving the last
active PSP on a rail is `409 psp_last_active` unless `allow_last` is `true`.

## Refunds

`POST /v1/admin/payments/{id}/refunds` refunds through the rail: `{amount}`
or `{full: true}`, an optional `reason`, and an explicit `revoke_access`.
`Idempotency-Key` is required. `201` settled, `202` pending. A rail with no
automatic refund is `400 refund_unsupported`; one that cannot take it now is
`409 refund_rail_unavailable`.

## Host events

`GET /v1/app/host-events` is the feed a host's backend drains: `payment.settled`,
`delinquency.grace`, `delinquency.entered`, `delinquency.cleared`, oldest
first, filtered by `type`. Acknowledge them, up to 100 at a time
(`POST /v1/app/host-events/acknowledge`), after the host's own
processing commits, then fetch again. Acknowledgment is idempotent and
independent of notification read state. Acknowledged events are kept 30 days;
pending ones are never deleted.

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
included: reads show `destination_host` only. `PATCH …/{id}` changes its
`url`, `name`, `format` or `enabled`; a new URL rotates the credential and keeps
the webhook's identity.

## Not routes

No route registers a merchant, lists a user's merchants, or manages a team,
API keys, federated grants or the merchant directory: those are the `server`
package's Go methods and the `openrails` CLI, on which a hosted product builds
its own routes. The operator's `/metrics` is on the standalone server's
private listener.
