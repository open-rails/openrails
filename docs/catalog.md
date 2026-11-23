# Catalog

Products, their prices, meters and rate cards. The database is the catalog. It
changes two ways, usable together: **documents** applied by an operator, and
**edits** through the console, the catalog routes and `Client` methods. Starting
OpenRails never applies a catalog.

Vocabulary: a **price** is OpenRails': it vaults the card and runs every renewal.
A **provider price** mirrors one a provider owns (a legacy NMI plan, a Stripe
price, a CCBill FlexForm); OpenRails reads it and never writes it. Amounts are
native units ([conventions](api/conventions.md#money-ids-and-times)), authored in
documents as `amount: 20 USD`.

## A document

```yaml
schema_version: 1
prune: false
products:
  course-101:
    display_name: Course 101 — Intro to CSS
    entitlements: ["course:101"]
    prices:
      purchase: { amount: 4.99 USD, access_duration: forever }
      rent:     { amount: 1.99 USD, access_duration: 3 days }
  course-bundle:
    display_name: CSS courses 101 and 102
    entitlements: ["course:101", "course:102"]
    prices:
      purchase: { amount: 8.99 USD, access_duration: forever }
  channel-membership:
    display_name: Channel membership
    tier_group: membership
    tier_rank: 1
    entitlements: ["channel:membership"]
    prices:
      monthly: { amount: 10 USD, billing_interval: 30 days }
      yearly:  { amount: 99 USD, billing_interval: 365 days }
  members-live-qa:
    display_name: Live Q&A replay
    entitlements: ["video:live-qa"]
    required_entitlements: ["channel:membership"]
    prices:
      purchase: { amount: 2.99 USD }
```

Products, prices, meters and limits are maps keyed by a stable key you choose. A
document holds `schema_version`, optional `prune`, `products` (each with `prices`,
`provider_prices`, `rate_cards` and `limits`), `meters`, `limits`, and optional
`entitlement_replacements: [{from, to}]`, which move a key across every product
granting it before products apply (empty `to` removes it). See
[`config/catalog.example.yaml`](../config/catalog.example.yaml).

- **Entitlements** are plain strings your app gives meaning to. A customer holds
  the current keys of the products they hold ([entitlements](entitlements_timeline.md)).
- **`required_entitlements`**: keys a customer must already hold to buy. Without
  them ordering, and paying an order opened while they held them, is
  `403 entitlement_required` (`metadata.missing`). Checked at purchase only; staff
  give the product anyway with `POST /v1/admin/product-access`. `[]` clears it.
- **Tiers.** `tier_group` and `tier_rank` make an ordered plan family, which is
  what allows upgrades and downgrades. Two products with recurring prices granting
  one key must share a tier group (`409 catalog_benefit_overlap`). Repeated
  purchases of one timed pass stack.
- **`credit_grant`** makes a product top up a balance: [prepaid credits](prepaid-credits.md).

Price fields:

| Field | Meaning |
|---|---|
| `amount` | `9.99 USD` or `1 SOL`; sets amount and currency. Must convert exactly; stablecoins are refused (they are what a buyer pays a fiat price with) |
| `unit_amount`, `currency` | the numeric form (native units; a decimal string in JSON); never with `amount` |
| `billing_interval` | time between payments (`30 days` or `1 month`), null for one-time. See [billing intervals](billing-intervals.md) for `billing_interval_hours` and `billing_interval_months` |
| `access_duration` | how long each payment grants; default the billing interval, or `forever` for a one-off (`forever` is one-off only). `access_duration_hours` |
| `trial_unit_amount`, `trial_duration` | a first phase; refused by every rail's new-subscription purchase today |
| `quantity: {min, max}` | sold per seat; the amount is one seat's |
| `customer_amount: {min_amount, max_amount}` | the buyer picks the amount ([prepaid credits](prepaid-credits.md)) |
| `sell_via` | `card` (the merchant's `card_vault` and `charge_psps`) and/or `solana`; default every method the merchant has. Not a term: changing it keeps the version |
| `archived` | `true` retires, `false` reactivates |

Durations take `3 days`, `24 hours` or the `_hours` numbers; zero is invalid, null
clears, omission preserves. OpenRails charges and renews our recurring card
prices. Fixed intervals also support Solana, where apply publishes the on-chain
plan (USDC, owned by the first listed `solana.crankers`, paying the merchant's
treasury).

**Meters and rate cards.** `meters` declare `event_type`, `value_property`,
`aggregation`, `unit` and `group_by`; a product's `rate_cards` bind a meter to a
charge model (`per_unit` with `divide_by`, `round`, `maximum_amount` and `matrix`,
`tiered` graduated, `package`, `dynamic`) with an optional per-period `allowance`,
or charge a `flat` fee per period with no meter.
See [usage billing](usage-billing.md#pricing).

**Limits.** `limits` declare what a customer may use per window, apart from what
it pays:

```yaml
limits:
  five_hour:
    unit: usage_units                      # a lowercase name, or a currency code
    reset: {kind: session, window_seconds: 18000}
    overrun: admit_while_positive          # default require_estimate
    rate_cards:                            # how usage counts, in the limit's unit
    - meter: input-tokens
      price: {model: per_unit, per_unit: {divide_by: 1000, unit_amount: "1"}}
  weekly:
    unit: usage_units
    reset: {kind: fixed, window_seconds: 604800, anchor: subscription}
    rate_cards:
    - meter: input-tokens
      price: {model: per_unit, per_unit: {divide_by: 1000, unit_amount: "1"}}
products:
  pro: {display_name: Pro, limits: {five_hour: "1000", weekly: "5000"}}
```

`reset.kind` is `fixed` (windows of `window_seconds` tiled from the customer's
subscription or `first_use`), `billing_period` (the pricing period), `session`
(opens at the first use after the last one ended), `rolling` (the last
`window_seconds`), `rate` (refills continuously over `window_seconds`) or
`lifetime`. `overrun: require_estimate` admits only a request whose estimate fits;
`admit_while_positive` lets the request that crosses the limit finish. A limit's
rate cards are usage prices without a currency (never `flat`, volume-tiered or
`dynamic`) on declared `sum` or `count` meters, with their filter and matrix
dimensions in the meter's `group_by`; a currency limit has none and counts the
rated amount. Product `limits` give capacity: a customer's is the sum over its
live products. `balance` is reserved. A limit with windows keeps its unit, and a
meter a limit counts cannot be removed (`meter_in_use`). Limits are also declared
with `PUT /v1/catalog/limits/{key}` (`SetLimit`). How they hold and refuse:
[usage billing](usage-billing.md#limits).

### Provider prices

```yaml
products:
  premium:
    prices:
      premium-monthly: {amount: 23 USD, billing_interval: 30 days}
    provider_prices:
      premium-monthly-nmi: {psp: nmi-main, plan_id: premium_new}   # terms read from NMI
      premium-stripe: {psp: stripe, price_id: price_1Q…}            # terms read from Stripe
      premium-monthly-ccbill:                                      # no read API: terms declared
        psp: ccbill
        flex_id: 681cb38f-afb9-4665-931f-2b896072178a
        recurring_billing_option_id: "0000007498"
        amount: 23 USD
        billing_interval: 30 days
```

`psp` names the owning PSP; `plan_id`, `price_id` or `flex_id` +
`recurring_billing_option_id` the provider object. NMI takes `currency` (default
USD); CCBill declares its terms. `access_duration` omitted ends access at the
verified provider period end, keeping calendar months; `one_off` marks a price
that does not renew. NMI and Stripe terms come from what OpenRails observed
([PSP import](psp-import.md#observed-catalog)), or a read by id. An object the
provider never had, or a Stripe price with no fixed amount, is refused; one deleted
after it was observed still maps. A provider that does not answer keeps an applied
price's cached terms; observed new terms become a new version at the next apply. A
retired provider price is not for sale. NMI and Stripe provider prices sell nothing
new: they bill the subscriptions imported on them. A recurring price's card order
may also offer the product's CCBill provider price on the same terms
([CCBill fallback](rails/ccbill.md#fallback)).

## Applying

```bash
openrails catalog apply catalog.yaml --merchant your-merchant [--force-conflicts]
```

Or `Client.ApplyCatalog` (a host's own command, with `catalog.ReadFile` and
`openrails.WriteCatalogReceipt`) or `POST /v1/catalog/apply`. The CLI reads the
merchant's configuration from Vault or the manifest (`--merchant-manifest`).

- **Omission preserves.** Omitted products, prices and fields are untouched;
  `archived: true` retires. `prune: true` makes the document the whole catalog: an
  omitted key that only documents set is deleted if nothing ever referred to it,
  else archived; one an edit touched stays.
- **Replay by content.** A document that applied whole is recorded by its content
  hash (`application_id: sha256:…`, with the merchant's Solana PSPs folded in) and
  replays its receipt forever, even after later edits: an old document never rolls
  anything back.
- **Field ownership.** OpenRails records which manager, `apply` or `edit`, last set
  each field (`catalog_field_owners`, as Kubernetes server-side apply does). Each
  object a document names applies whole or is skipped when a field it names has a
  live value an edit set differently. Equal values share the field. Edits always
  apply. Owned fields: a product's `display_name`, `description`, `tier_group`,
  `tier_rank`, `archived`, `ownership`, `entitlements`, `required_entitlements`,
  `credit_grant`, `rate_cards`; a price key's terms, `quantity`,
  `customer_amount`, `archived`, `sell_via` and `provider`; a meter's fields.
- **The receipt** lists `changes` (objects, fields, new revisions), `conflicts`
  (each field's file and live value, who set it and when) and `warnings`. A
  document with conflicts or warnings is not recorded, so applying it again
  retries; `life.catalog.conflicts` stays open until one applies whole. Resolve a
  conflict by agreeing with the live value, dropping the field from the document,
  or forcing (`Force: true`, `?force=true`, `--force-conflicts`).
- **Warnings, not failures:** a price that cannot sell through any method it is
  restricted to, or a recurring price whose Solana plan cannot be published (no
  cranker, or `limited`/`readonly`, which only bind an existing plan); it applies
  and sells by card.

### Changing what you sell

| Change | Effect |
|---|---|
| a product field under an existing key | updated in place; its revision advances |
| a price's financial terms under an existing key | a new immutable version (`…:v2`) and the old one archived; subscribers keep their accepted price and access |
| a new price key | a separate price |
| `archived: true` on a price | stops new sales at it; subscribers keep billing and access |
| `archived: true` on a product | stops all its sales, keeping each price's own flag; buyers keep access |
| a product's `entitlements` | the list replaces the old one for new purchases; earlier buyers keep what they bought |
| the complete earlier terms with `archived: false` | restores that exact version and id |

Nothing in a document moves, cancels or reprices an existing subscription; that
is a [price migration](#price-migrations). A price cannot move to another product,
and a new rental price never extends existing rentals.

## Through the Client

```go
price, err := client.CreatePrice(ctx, billing.CreatePriceParams{
    ProductData: &billing.CreatePriceProduct{Key: "post-" + slug, DisplayName: title},
    Key:        "post-" + slug + "-usd",
    UnitAmount: 4_990_000, // USD micros
    Currency:   "USD",
})
products, err := client.ListProducts(ctx, billing.ProductListParams{
    Keys: []string{"premium", "course-101"}, Archived: new(false), ForSale: new(true),
})
```

Exactly one of `ProductID`, `ProductKey` and `ProductData` names the product;
`ProductData` creates it or reuses the one under its key, committed with the
price. The same key with other terms makes a new version. Updates are merge
patches (`catalog.Field`, `catalog.Null` clears); send the `Revision` you read as
`ExpectedRevision` to refuse a concurrent change (`409 revision_mismatch`).
`ListProducts` and `GetProduct` include each product's current `Prices`;
`ListPrices` with `ProductID` and `Archived` shows history.
`GET /v1/catalog/prices/{id}?verify=true` reads each linked PSP's copy and reports
drift. Reads need `merchant:catalog:read`, edits `merchant:catalog:manage`. No
public route reads the catalog: your server reads it and puts products in its own
pages with `billing.BuyerPrices`.

A price id is `premium:buy-usd:v1`: `Price.Version` is that financial version,
`Price.Revision` the concurrency counter of the price key. Use
`billing.ParsePriceID` or `billing.NewPriceID("premium", "buy-usd", 1)`; no UUID
alias is accepted.

A handle serves catalog and configuration reads from memory, reloading in the
background after `RefreshAfter` and serving the last good answer until `MaxStale`
while reloads fail; customer and money reads always go to the server.

| Read | Embedded | Remote |
|---|---|---|
| `ReadCatalog` (products, prices without `Verify`), `ReadMeters` (meters, limits) | 1 m, stale ≤ 10 m | 10 m, stale ≤ 1 h |
| `ReadPublicConfig` | live | 10 m, stale ≤ 1 h |
| `ReadMerchantConfiguration` | live | 30 s, stale ≤ 10 m |

A handle's own write, and any change the embedded engine commits, drops the cache
at once; another replica's shows within `RefreshAfter`. Override with
`Config.ReadCache` or `RemoteOptions.ReadCache` (a zero policy reads live).

## Archiving and deleting

Archiving a product (`UpdateProduct` with `archived: true`) stops its sales and
deletes nothing: buyers keep access and money, subscriptions keep renewing, and no
one is refunded. `DeleteProduct` deletes a product nothing ever referred to, with
its prices (`204`); creating its key again revives it. A product any order line,
payment, subscription, grant, access window, migration, scheduled change, provider
write, PSP link or rate card names answers `409 product_in_use`: archive it.

## Price migrations

A price migration moves existing subscribers to another price: those of one price
(`from_price_id`) or of every version of a key (`product_key`, `price_key`), to
`to_price_id`. Each subscription moves at its first renewal on or after
`effective_at` (default its next renewal), carried until then as its
`scheduled_change`; nothing is prorated or charged early.

- `POST /v1/catalog/price-migrations/preview` counts who would move;
  `POST /v1/catalog/price-migrations` creates it, `…/{id}/cancel` cancels it.
- A subscription whose price rises inside the merchant's
  `reprice_notice_window_days` is skipped unless `acknowledge_short_notice`.
- One its provider cannot move (CCBill, Solana, an unreachable NMI schedule) is
  `blocked` and follows `fallback_policy`: `keep_grandfathered` (default) or
  `cancel_at_period_end`, which only records the intent.
- `archive_source` (default true) stops the old price selling.
- Counts: `matched`, `skipped`, `scheduled`, `applied`, `canceled`, `blocked`.

Backups and editable exports: [catalog export](catalog-export.md).
