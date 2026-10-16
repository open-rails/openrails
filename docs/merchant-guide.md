# Merchant Guide

You are the merchant: the business operator of an OpenRails deployment. This guide
covers defining what's for sale (the catalog), understanding entitlements, and running
day-to-day customer operations via the admin API and admin console.

Vocabulary: a **rail** is a gateway kind (`nmi`, `ccbill`, `stripe`, `solana`); a
**PSP** is *your account* on a rail (e.g. a `mobius` key on the nmi rail; `stripe`,
`ccbill`, `solana` are their own PSP names). A **price** is ours: OpenRails vaults
the card and runs the billing. A **provider price** is one a provider owns and runs
(a legacy NMI plan, a Stripe price, a CCBill FlexForm): OpenRails reads it and never
writes it. The API and database represent money
as **integers in the currency's native units** (micros for USD:
`20_000_000` = $20.00; scales are listed by `GET /v1/config`). Catalog prices
can be authored as `amount: 20 USD`; parsing produces those exact native units.

### The mental model

The database is the catalog. The authorized in-process client can always edit
individual records or apply a JSON/YAML batch. The catalog over HTTP is its own
route group: an embedded host turns on `RouteGroups.Catalog`; the standalone
server turns on `route_groups.catalog`. A
document skips what an edit set ([catalog ownership](catalog-ownership.md)).

Each batch is applied atomically once per merchant, identified by a canonical
content hash. Reapplying identical content returns the saved receipt even after
later API edits. Omission preserves records and fields; explicit `archived: true`
retires an entry, and `prune: true` removes omitted products and prices that
only documents set: deleted when nothing ever referred to them, archived otherwise.
Batches have no ordering guarantee: previously unseen content applies to current
state. Already-applied content never becomes an implicit rollback command.

The API is `POST /v1/catalog/apply`, and the client method (in process or
over HTTP) is `ApplyCatalog`. The operator CLI (`openrails catalog apply`) applies
the same document with local authority. No application ID or expected revision
is written by the caller. Applying a catalog is an operator action: starting
OpenRails never does it.

### Authoring the catalog

One application selects one authorized merchant outside the document and includes
`schema_version`, optional `prune`, `products`, supported `meters` and optional
`entitlement_replacements`: `[{from, to}]` pairs that move a key across every product
granting it before `products` apply (an empty `to` removes `from`), so holders lose
`from` and gain `to` at once.
Product-local `prices`, `provider_prices` and `rate_cards` are nested under each
product. Products, prices and meters are maps keyed by their key; the key is the
record's stable handle (a price's key names its version chain, and names one price
of the product, ours or a provider's). See `config/catalog.example.yaml`.

**A tiered subscription** — `tier_group` + `tier_rank` make products an ordered plan
family, which is what enables upgrade/downgrade between them:

```yaml
schema_version: 1
prune: false
products:
  novice:
    display_name: Novice
    tier_group: membership
    tier_rank: 1
    entitlements: ["tier:novice"]
    prices:
      novice-monthly:
        amount: 12 USD
        access_duration: 30 days
        billing_interval: 30 days
```

Two products with recurring prices that grant one entitlement must share a
`tier_group`; otherwise a customer could hold both and pay twice for it, and the
catalog change is refused with `409 catalog_benefit_overlap`. Repeated purchases
of one timed pass stack: each window starts when the previous one ends.

Catalog prices accept `amount: 9.99 USD` or `amount: 1 SOL`. The currency is
required and must be fiat or SOL: a stablecoin (USDC, USDT, PYUSD, USD1, USDG)
is refused, because it is what a buyer pays with for a price in the fiat
currency it tracks. Amounts convert exactly at that
currency's precision; values that require rounding are rejected.
Use `amount` by itself, or `unit_amount` with `currency`; combining these forms is
invalid. Supplying `amount` sets both the amount and currency. Omitting all money
fields in a partial update preserves both; other omitted fields also preserve
their values.

Duration fields also accept readable aliases: `access_duration: 3 days`,
`billing_interval: 30 days`, and `trial_duration: 24 hours`. Use one form per
field; zero is invalid, omission preserves existing terms, and null clears.

Price fields worth knowing:

| Field | Meaning |
|---|---|
| `amount` | decimal plus explicit currency, such as `9.99 USD`; sets amount and currency together |
| `unit_amount`, `currency` | numeric alternative: integer native units at the registered scale plus a currency code; JSON applications spell `unit_amount` as a decimal string |
| `access_duration` | how long access lasts, such as `30 days`. Omitted: a recurring price's billing interval, a one-off's `forever`. `forever` (never expires) is for one-off prices only; `access_duration_hours` is the numeric form |
| `billing_interval` | time between recurring payments, such as `30 days`, or null for a one-time purchase; independent of access duration; `billing_interval_hours` is the numeric form |
| `trial_unit_amount`, `trial_duration` | first-phase terms; supply both together with a billing interval; `trial_duration_hours` is the numeric form |
| `archived` | explicit true retires, explicit false reactivates; omission preserves an existing value |
| `sell_via` | the methods the price sells through: `card` (the merchant's `card_vault` and `charge_psps`) and `solana`; null or omitted on a new price: every method the merchant has. Not a term: changing it keeps the version |

Trial terms are supported only on rails with native trial settlement; current
engine-managed purchase refuses trials before charging.

**A one-time purchase** — null `billing_interval`; access is forever unless
`access_duration` sets a rental:

```yaml
products:
  course-101:
    display_name: Course 101 — Intro to CSS
    entitlements: ["course:101"]
    prices:
      purchase:
        amount: 4.99 USD
        access_duration: forever
        billing_interval: null
```

**A members-only product** — `required_entitlements` lists keys a customer must
already hold to buy it:

```yaml
products:
  members-live-qa:
    display_name: Live Q&A replay
    entitlements: ["video:live-qa"]
    required_entitlements: ["channel:membership"]
    prices:
      purchase:
        amount: 2.99 USD
        access_duration: forever
```

A customer lacking any of them is refused `403 entitlement_required`
(`metadata.missing` names the keys) when ordering, and again when paying an order
or purchase opened while they held them; a preview's line carries the same
`refusal`. Merchant orders follow the rule too: to give the product anyway, grant
it (`POST /v1/admin/product-access`). The check is at purchase only: what a
customer bought stays theirs if they later lose the keys. `[]` clears the list.

Prepaid balance products use `credit_grant`; see the
[prepaid catalog example](../README.md#prepaid-api-balance-catalog). Administrative
funding uses `POST /v1/admin/credit-grants`
(`Client.CreateCreditGrants`), whose grants carry their own expiry. The price
`amount` alias does not change numeric credit-grant amounts, customer-selected
bounds, or metered rate-card fields.

**Charge models** (for metered rate cards):

| Model | Cost | Use for |
|---|---|---|
| `flat` | fixed amount | base fees ("$5/mo includes…") |
| `per_unit` | `round(qty × unit_amount / divide_by)` | linear rates; `divide_by` keeps integer micros exact (e.g. micros/hour rated per second: `divide_by: 3_600`); `round: up\|down\|half_up` (default `half_up`) picks the mode, declared inside `per_unit` — the one place it is read; optional `maximum_amount` cap, `matrix` for per-SKU cells on a meter dimension |
| `tiered` | `graduated` (bands stack) or `volume` (final band prices everything) | volume discounts; a metered card takes `graduated` only |
| `package` | `ceil((qty − free_units)/package_size) × amount` | block pricing, round-up-to-next-unit |

**Metered usage** — declare `meters:` (event_type, value_property, aggregation,
group_by) and attach `rate_cards:` to a product: each card binds one meter to a charge
model, with an optional `allowance` (`included` usage netted off first, per period).
Usage products declare no billing cadence — the billing period is the window. Each
usage event is priced through the cards when it is recorded and draws the prepaid
balance or is owed at once; the daily pass bills each customer's owed usage as a
usage order. A metered card states no `payment_term`. See
[usage pricing](usage-pricing.md).

### Our prices and provider prices

A price under `prices` is ours, without synchronizing a provider catalog. It sells
by card through the merchant's `card_vault` and `charge_psps` (OpenRails charges the
card and runs every renewal) and on Solana: a one-time price by direct transfer, a
recurring one on an on-chain plan the apply publishes from its terms (USDC, owned by
the deployment's first listed `solana.crankers`, paying the merchant's treasury).
`sell_via` restricts a price to some of those methods.

A price under `provider_prices` is a provider's: it mirrors what the provider still
runs (legacy NMI-run subscriptions, imported) or sells (CCBill FlexForms). OpenRails
reads its terms and caches them on the price; it never creates or edits it.

```yaml
products:
  premium:
    prices:
      premium-monthly: {amount: 23 USD, billing_interval: 30 days}
    provider_prices:
      premium-monthly-mobius: {psp: mobius, plan_id: premium_new}   # NMI plan; terms read from NMI
      premium-stripe: {psp: stripe, price_id: price_1Q…}             # Stripe price; terms read from Stripe
      premium-monthly-ccbill:                                       # CCBill has no read API: terms declared
        psp: ccbill
        flex_id: 681cb38f-afb9-4665-931f-2b896072178a
        recurring_billing_option_id: "0000007498"
        amount: 23 USD
        billing_interval: 30 days
```

| Field | Meaning |
|---|---|
| `psp` | the merchant's PSP key that owns the price |
| `plan_id` / `price_id` / `flex_id` + `recurring_billing_option_id` | the provider's object: an NMI plan, a Stripe price, a CCBill FlexForm |
| `amount`, `billing_interval`, `trial_unit_amount` + `trial_duration` | CCBill only: its declared terms |
| `currency` | NMI only: the plan's currency, USD when omitted |
| `access_duration` | omitted: access ends at the verified provider service-period end, preserving calendar months and years; explicit hours override that boundary; `forever` only on a `one_off` price |
| `one_off` | a price that does not renew: its billing interval is the access it sells |
| `archived` | as for prices |

An NMI plan or Stripe price that does not exist is refused: a provider price names an
existing object. A provider that does not answer keeps the cached terms of a provider
price already applied (with a warning), and leaves a new one out. When the provider's
terms change, the next apply takes them as a new version. Legacy NMI and Stripe
provider prices sell nothing new; they bill the subscriptions imported on them
([batch import](batch-import.md)), whose renewals OpenRails records from provider
events and reads, and whose declines its dunning retries against the subscription's
own NMI schedule.

### Applying and verifying

```bash
openrails catalog apply catalog.yaml --merchant your-merchant
```

Without Vault, `--merchant-manifest PATH` loads the merchant's configuration and
overlays as the provider tools do; otherwise the conventional merchant manifest
path is used. With Vault it reads Vault and fails if Vault is unavailable.

A price that cannot sell through any method it is restricted to (`sell_via: [solana]`
on a recurring price with no listed cranker, say) is refused by name, and a recurring
price whose Solana plan cannot be published (no cranker, or `provider_write_mode`
limited or readonly, which only bind a plan already on chain) applies without one and
sells by card. Both appear in the receipt's `warnings`; nothing is fatal. A document
with warnings or conflicts is not recorded as applied, so applying it again retries.

The returned receipt's `application_id` is generated as `sha256:<content digest>`,
with the merchant's Solana PSPs folded in: a changed treasury makes the same
document a new application.
It records what committed and whether the call replayed, not whether later edits
have changed the catalog. Exports are inspection snapshots that can be edited and
submitted as another batch; submitting already-applied content remains a replay.

Card checkout offers the merchant's charge PSPs. An order for one recurring price of
ours also offers the product's live CCBill provider price on the same terms, sold on
its FlexForm; keeping it back as a fallback (CCBill until Mobius declines twice) is a
checkout option, billing-ui's `fallbacks` prop, not merchant configuration: see
[CCBill](rails/ccbill.md#fallback).

### Entitlements

Each entitlement is a **timeline per user** — the host app asks "does user X have
entitlement Y at time T?" against it. Full semantics: `docs/entitlements_timeline.md`.

- Windows are appended or revoked, never edited: a window's end is immutable, and a
  renewal appends a new window.
- Every window derives from a grant and carries its source: `purchase`,
  `subscription`, `admin`, or `grace`.
- Each payment grants its accepted access duration, independently of the next
  billing date. Finite windows expire by the clock; null means no scheduled expiry.
  Normal cancellation stops future billing and preserves purchased windows.
  Refunds or explicit revocations can remove access.
- Tier changes go through `POST /v1/me/subscriptions/{id}/change` (target price
  must share the tier group). Stripe and NMI upgrade immediately with proration;
  downgrades are scheduled for period end (`delayed_start`, `effective:
  "period_end"`), with no refund. On engine-owned subscriptions the upgrade is one
  engine charge on the saved card (with issuer authentication when required) that
  replaces the subscription with a successor on the new price; nothing changes if it
  is declined. On NMI-billed (legacy) subscriptions the member's NMI schedule is
  changed in place and keeps its billing date: an upgrade charges the prorated
  difference now, a downgrade takes effect at the next NMI renewal; only prices of
  the same billing cycle qualify. CCBill upgrades redirect
  to a FlexForm; a Solana tier change is signed in the customer's wallet.
- Staff changes through `POST /v1/admin/subscriptions/{id}/change` schedule
  engine-billed price or seat changes at the next renewal, including upgrades
  and additional seats. Preview reports zero due now; current access and seats
  remain until renewal settles. Choosing the current terms clears a pending
  engine change without charging. Staff cannot start or resume an immediate
  saved-card charge; provider-billed upgrades require the customer's own flow.
- **Upgrade proration** resets the period: the customer pays `new price − credit`
  now for a fresh period of the new price's cycle, where `credit = old price ×
  time left / current period length`, measured on the subscription's actual current
  period to the nanosecond and rounded once, up to a whole minor unit (never above
  what was paid). Cadences may differ (1h → 30d, 30d → 7d, 90d → 365d). Refusals
  are typed: `422 subscription_change_cycle_unknown` (target has no positive cycle),
  `422 subscription_change_period_unknown` (no valid current period) and `409
  subscription_change_credit_exceeds_price` (the unused credit is larger than the target
  price, e.g. a monthly plan early in its period moving to a cheaper weekly one;
  credit is never forfeited — change at period end instead).

### Managing customers day-to-day

Customer support lives under `/v1/admin/*`, metrics under `/v1/metrics/*`; catalog
management lives under `/v1/catalog/*`; merchant configuration, including
findings and PSP refresh, lives under `/v1/merchant/*` (same public port; each
route behind its group's permission, held in the merchant's group:
`billing.MerchantCustomerRead` for reads, `billing.MerchantCustomerManage` for
actions on customers, `billing.MerchantCatalogManage` for the catalog,
`billing.MerchantConfigManage` for the merchant's configuration and findings,
`billing.MerchantMetricsRead` for business metrics); your backend's own calls
live under `/v1/app/*`, taking its application credential holding each
route's permission ([auth](auth.md#permissions)). Auth is a trusted issuer's access token (client credentials
for a backend), a user session, or, on a hosted product, a merchant API key. Full
reference: [api/routes.md](api/routes.md).

| Task | Route | Console page |
|---|---|---|
| Look up a customer (settings, balances, arrears, default cards) | `GET /v1/admin/customers/{customer_id}` | Customers → search |
| Find overdue usage orders | `GET /v1/admin/orders?origin=usage&overdue=true` (each says whether it is `delinquent`) | Orders → Overdue only |
| Follow failed renewals | `GET /v1/admin/subscriptions?dunning=true` (each with its `dunning`) | Subscriptions → Dunning |
| Grant a product free / revoke a window | `POST /v1/admin/product-access` (a batch), `POST /v1/admin/product-access/{id}/revoke` | Customers → profile |
| Record money received outside OpenRails for an order | `POST /v1/admin/payments` | Orders → detail |
| List / inspect payments | `GET /v1/admin/payments[/{id}]` | Payments |
| Refund (with explicit `revoke_access` choice) | `POST /v1/admin/payments/{id}/refunds` | Payments → detail (disabled on rails without API refunds) |
| List / inspect subscriptions | `GET /v1/admin/subscriptions[/{id}]`; `sort` is `created_at` (newest first, the default), `current_period_ends_at` (soonest first) or `canceled_at` (latest first) | Subscriptions (incl. past_due dunning view, Renewing soon, Recently canceled) |
| Cancel / resume a subscription | `POST /v1/admin/subscriptions/{id}/cancel` / `/resume` | Subscriptions |
| Change a subscription's payment method | `PUT /v1/admin/subscriptions/{id}/payment-method` | Subscriptions (NMI) |
| Grant / revoke credit | `POST /v1/admin/credit-grants`, `POST /v1/admin/credit-grants/{id}/revoke` | Customers → profile |
| Ask what a grant key did | `GET /v1/admin/credit-grants?customer_id=&source_id=` | — |
| Customer settings: credit limits, trust levels, billing policy, invoice profile | `GET` / `PATCH /v1/admin/customers/{customer_id}` | Customers → profile |
| Catalog over HTTP | `POST /v1/catalog/products`, `PATCH /v1/catalog/products/{id}`, and the same for prices (archive with `{"archived": true}`) | Catalog |
| Metrics | `POST /v1/metrics/query`, `GET /v1/metrics/schema` | Dashboard |
| Findings: problems that need a merchant-config operator, catalog drift and operational alerts included | `GET /v1/merchant/findings` (`?type=`), counted by `open_findings` in `POST /v1/metrics/query` | Ops, header bell |

Staff sessions and API keys pass on their permissions without a recent
sign-in. A manual grant's `hours` is at most 2562047; with neither `hours` nor `ends_at` it has no
end.

Destructive semantics are deliberate: refunds and cancels require an explicit
`revoke_access` decision — refunding money and revoking access are separate choices.
For refunds, requested access revocation commits with successful local refund
completion. Only the refunded payment's access and its renewal grace are removed;
newer paid periods stay available, and renewal remains scheduled. Cancellation is
a separate action. If a seat or plan change combined access from several payments,
the refund completes and a `consistency.refund_access` finding requests review;
access stays intact rather than removing another payment's entitlement. A queued or uncertain refund (HTTP 202) retains access until that
completion. The amount, reason, and revocation choice are immutable for an
`Idempotency-Key`; reusing that key resumes the same operation. A different key
creates a new operation, including an equal-sized partial refund within the
remaining balance or a retry after a definitive refusal.

For NMI and CCBill, a lost response without an exact captured operation receipt
requires operator verification. The system retains the reservation and never
infers success from an equal refund amount or subscription counters. Check the
provider before resolving the operation; do not use a new key to retry an
uncertain refund. A captured success retries local finalization automatically
without sending a second provider refund.

Hosts refund through `Client.RefundPayment` (same route, embedded or remote):
`Amount` or `Full`, a mandatory `IdempotencyKey`, `Reason` and `RevokeAccess`.
A rail that cannot refund now returns `ErrRefundRailUnavailable` (nothing is
reserved); CCBill and off-rail payments return `ErrRefundUnsupported`. Provider
write gates (read-only mode, NMI test-mode qualification) park the operation:
the refund is returned `pending` and settles when the gate allows it.

A refund made in the provider's own dashboard carries no `revoke_access`
choice; the merchant setting `provider_refund_access` decides it on every rail:
`revoke_on_full` (default: access ends once the charge is fully refunded),
`revoke_on_any`, or `keep`. The same rule applies when an import or background
sync discovers the refund. These policies also preserve renewal and unrelated
paid access. Set it in the merchant configuration (`UpdateMerchantConfiguration`).

### Archiving a product

`PATCH /v1/catalog/products/{id}` with `archived: true`
(`Client.UpdateProduct`) archives a product: it stops being for sale and is
never deleted. Its buyers keep their access and their money, and their payments,
orders and subscriptions keep naming it. Subscriptions on its prices stay
grandfathered and keep renewing. Archiving refunds nobody; a refund is its own
decision (`Client.RefundPayment`).

### Deleting a product

`DELETE /v1/catalog/products/{id}` (`Client.DeleteProduct`) deletes a
product nothing ever referred to, with its prices, and answers 204. Reads and
catalog listings leave it out. Creating its key again revives it with the new
definition, since a product's id is its key's. A product an order line,
payment, subscription, grant, product access, purchase, price migration,
scheduled change, provider write, PSP link or meter rate card refers to is
never deleted: the request answers `409 product_in_use`, and you archive it
instead. A purchase therefore always names a product that exists. OpenRails
deletes only its own product, never the host's content behind it.

Granting credits is money-in, an admin change (`billing.MerchantCustomerManage`). Each grant item's `source_id`
is the caller's reproducible idempotency key: retrying it can never double-credit
(database-enforced), and a retry with a different `amount` is refused with 409.

### The admin console

The standalone server defaults to serving its staff console at `/admin`, with
**Billing**, **Access** and **Account**. BillingAdmin supplies the billing pages;
AuthKit components supply group administration and account security. Staff use
configured app issuers or the separately enabled local sign-in. Release binaries
include the assets; a source build without them answers 503 with build instructions.
Set `admin_console.disabled: true` for an API-only server. Its `/admin` subtree
then returns 404; billing APIs, route groups and local sign-in remain independent.

Embedded hosts compose `@openrails/billing-ui/admin` at their chosen UI path;
the embedded Go engine serves no console. Build and mounting details are in
`docs/billing-admin.md`.

Pages: **Customers** (search → profile with grant/revoke),
**Subscriptions** (status filters, cancel with typed confirmation, resume, payment-
method change), **Payments** (filters, detail, rail-aware refund), **Catalog**
(products/prices CRUD, archive/restore, durable catalog batch application, drift
view), **Ops** (findings queue, the merchant inbox), **Settings** (profile,
PSPs, credit limit, trust level), **Dashboard**. Hosts may extend BillingAdmin with their own pages and panels. Each billing area
shows only to staff holding its
route group's permission (`GET /v1/admin/access`).
- **Dashboard**: a widget grid over the metrics API. Saved and default dashboards remain available. Operators can move, resize, rename and remove existing widgets, choose their chart type and change whether they follow the dashboard's date range. Built-in AI metrics questions, widget creation and refinement are not available.

- **Decline metrics**, all by owner (`engine`, `nmi_schedule`, `provider`) and PSP
  (`psp`):
  - `attempt_failure_rate`: the new-card decline rate is `kind` in (`verify`, `initial`)
    with `card_entry=new`. It can also be grouped by `reason`, `category`, `response_code`,
    `card_bin`, `card_brand` and AVS/CVV.
  - `purchase_failure_rate`: the same per buyer rather than per attempt.
  - `rebill_first_failure_rate` and `rebill_missed_rate`: rebills that failed on the first
    attempt, and rebills that never happened.
  - `dunning_recovered` by `recovery_attempt` or `days_to_recover`: the recovery curve.
- **NMI history**: `nmi_history_authorizations`, `nmi_history_approved`,
  `nmi_history_refused` and `nmi_history_refusal_rate` by month (`time`), `nmi_kind`
  (`verification`, `one_off_sale`, `scheduled_rebill`), PSP, `category` and `reason`. They
  are NMI's own transaction history, read daily and kept 25 months, including the months
  before OpenRails recorded attempts. `one_off_sale` cannot separate initial sales from
  retries of declined rebills.
- **Catalog copilot** (opt-in `llm.catalog_copilot_enabled`): Q&A over products,
  prices, subscriber counts, and pending migrations. With
  `llm.catalog_drafting_enabled`, it can additionally *draft* a price change or new
  tier — but only into the wizard's review step; nothing mutates until a human clicks
  Confirm.

Provider prices keep their native calendar cadence: a month is not thirty days,
and a year is twelve calendar months. Existing accepted hourly access snapshots
keep those exact hours. Reapplying a successfully applied catalog file replays
its stored receipt without provider I/O; an explicit `CreateProviderPrice`
operator call can read current external terms and append a new immutable version.
Earlier versions retain their provider account, object, cadence and access choice.

Stripe hosted one-time Checkout uses inline, nonreusable price/product objects
for the accepted payment. OpenRails does not synchronize a maintained Stripe
catalog or create Stripe-managed subscriptions; new recurring card billing stays
in OpenRails. CCBill remains the provider-run sales exception.
