# Merchant Guide

You are the merchant: the business operator of an OpenRails deployment. This guide
covers defining what's for sale (the catalog), understanding entitlements, and running
day-to-day customer operations via the merchant API and admin console.

Vocabulary: a **rail** is a gateway kind (`nmi`, `ccbill`, `stripe`, `solana`); a
**PSP** is *your account* on a rail (e.g. a `mobius` key on the nmi rail; `stripe`,
`ccbill`, `solana` are their own PSP names). The API and database represent money
as **integers in the currency's native units** (micros for USD:
`20_000_000` = $20.00; scales are listed by `GET /v1/currencies`). Catalog prices
can be authored as `amount: 20 USD`; parsing produces those exact native units.

### The mental model

The database is the catalog. The authorized in-process client can always edit
individual records or apply a JSON/YAML batch. `Routes.CatalogEdits` (the
standalone server's `catalog_edits`) controls whether catalog-write HTTP routes
are exposed; it defaults false.

Each batch is applied atomically once per merchant, identified by a canonical
content hash. Reapplying identical content returns the saved receipt even after
later API edits. Omission preserves records and fields; explicit `archived: true`
retires an entry, and `prune: true` opts into archiving omitted products and prices.
Batches have no ordering guarantee: previously unseen content applies to current
state. Already-applied content never becomes an implicit rollback command.

The API is `POST /v1/merchant/catalog/applications`, and the in-process client
method is `ApplyCatalog`. The operator CLI (`openrails apply-catalog`) applies the
same document with local authority. No application ID or expected revision is
written by the caller.

### Authoring the catalog

One application selects one authorized merchant outside the document and includes
`schema_version`, optional `prune`, `products` and supported `meters`.
Product-local `prices` and `rate_cards` are nested under each product. See `config/catalog.example.yaml`.

**A tiered subscription** — `tier_group` + `tier_rank` make products an ordered plan
family, which is what enables upgrade/downgrade between them:

```yaml
schema_version: 1
prune: false
products:
  - key: novice
    display_name: Novice
    tier_group: membership
    tier_rank: 1
    entitlements: ["tier:novice"]
    prices:
      - key: novice-monthly
        amount: 12 USD
        access_duration_hours: 720
        billing_interval_hours: 720
```

Catalog prices accept `amount: 9.99 USD`, `amount: 1 SOL`, or `amount: 10 USDC`.
The currency is required and must be registered. Amounts convert exactly at that
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
| `access_duration_hours` | positive hour count, or null for indefinite access |
| `billing_interval_hours` | positive interval between recurring payments, or null for a one-time purchase; independent of access duration |
| `trial_unit_amount`, `trial_duration_hours` | first-phase terms; supply both together with a billing interval |
| `key` | required stable application handle for the price version chain |
| `archived` | explicit true retires, explicit false reactivates; omission preserves an existing value |
| `psps` | explicit PSP list; omitted = OpenRails-native only, no provider sync |
| `psp_links` | pre-supply provider ids, validated on apply (below) |

Trial terms are supported only on rails with native trial settlement; current
engine-managed checkout refuses trials before charging.

**A one-time purchase** — null `billing_interval_hours` with a finite duration gives timed
access; null `access_duration_hours` gives indefinite access:

```yaml
      - key: course-101
        display_name: Course 101 — Intro to CSS
        entitlements: ["course:101"]
        prices:
          - key: purchase
            amount: 4.99 USD
            access_duration_hours: null
            billing_interval_hours: null
```

Prepaid balance products use `credit_grant`; see the
[prepaid catalog example](../README.md#prepaid-api-balance-catalog). Administrative
funding uses `POST /v1/merchant/customers/{customer_id}/credit-grants`
(`Client.CreateCreditGrant`), whose grants carry their own expiry. The price
`amount` alias does not change numeric credit-grant amounts, customer-selected
bounds, or metered rate-card fields.

**Charge models** (for metered rate cards):

| Model | Cost | Use for |
|---|---|---|
| `flat` | fixed amount | base fees ("$5/mo includes…") |
| `per_unit` | `round(qty × unit_amount / divide_by)` | linear rates; `divide_by` keeps integer micros exact (e.g. micros/hour rated per second: `divide_by: 3_600`); `round: up\|down\|half_up` (default `half_up`) picks the mode, declared inside `per_unit` — the one place it is read; optional `maximum_amount` cap, `matrix` for per-SKU cells on a meter dimension |
| `tiered` | `graduated` (bands stack) or `volume` (final band prices everything) | volume discounts; prefer graduated — volume has price cliffs and doesn't invert |
| `package` | `ceil((qty − free_units)/package_size) × amount` | block pricing, round-up-to-next-unit |

**Metered usage** — declare `meters:` (event_type, value_property, aggregation,
group_by) and attach `rate_cards:` to a product: each card binds one meter to a charge
model, with optional `allowance` (included usage netted off first, poolable and
accruable from another meter) and `payment_term` (`in_advance`/`in_arrears`). Usage
products declare no billing cadence — the invoice period is the window: the daily
period finalize rates reported usage through the cards and invoices every customer with
ledger or metered activity, including a customer whose only activity is metered usage.
See the metering API documentation for complete rate-card contracts.

**psp_links** — supply provider-side ids or declarative provider config per PSP key.
Supplied links are validated against the provider (object exists + money terms match)
and never duplicated; a mismatch fails the apply loudly:

```yaml
            psp_links:
              stripe: {lookup_key: premium}         # find-or-create at a chosen key
              mobius: {plan_id: premium}            # NMI recurring plan; find-or-create
              ccbill: {form_name: premium, flex_id: abc-123}  # operator-owned, unvalidated
              solana: {token: USD1}                 # optional override; recurring defaults to USDC
              # solana: {plan_pda: "..."}           # alternatively attach and resolve the token on-chain
```

### Applying and verifying

```bash
openrails apply-catalog --merchant your-merchant --file catalog.yaml
```

For host-owned credentials, `--merchant-manifest PATH` loads the same credential
snapshot and overlays as the provider tools; otherwise the conventional merchant
manifest path is used. Managed DB/Vault deployments read their configured backend
and fail if it is unavailable. Recurring Solana references use public account and
chain reads; catalog application does not construct a signer or submit a plan.

The returned receipt's `application_id` is generated as `sha256:<content digest>`.
It records what committed and whether the call replayed, not whether later edits
have changed the catalog. Exports are inspection snapshots that can be edited and
submitted as another batch; submitting already-applied content remains a replay.

Provider support for individual catalog operations (batch applications reject unsupported external workflows before local mutation):

| Rail | On push |
|---|---|
| stripe | auto-creates Products, Prices, and entitlement Features (`lookup_key` = the entitlement string); financial terms are immutable, so amount changes re-mint a new price and archive the old |
| nmi PSPs | recurring `plan_id` found-or-created; otherwise link-only |
| ccbill | link-only: you supply `form_name` + `flex_id` from the CCBill admin |
| solana | recurring plans default to USDC and are found-or-created; `token: USD1` selects USD1; `plan_pda` attaches an existing plan and resolves its token on-chain |

A link-only price pushed without its ids is still created in OpenRails and recorded as
`pending_manual_link`, with a `pending_manual_actions` entry telling you what to PATCH
in later.

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
- Tier changes go through `POST /v1/me/subscriptions/{id}/change-tier` (target price
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
- **Upgrade proration** resets the period: the customer pays `new price − credit`
  now for a fresh period of the new price's cycle, where `credit = old price ×
  time left / current period length`, measured on the subscription's actual current
  period to the nanosecond and rounded once, up to a whole minor unit (never above
  what was paid). Cadences may differ (1h → 30d, 30d → 7d, 90d → 365d). Refusals
  are typed: `422 tier_change_cycle_unknown` (target has no positive cycle),
  `422 tier_change_period_unknown` (no valid current period) and `409
  tier_change_credit_exceeds_price` (the unused credit is larger than the target
  price, e.g. a monthly plan early in its period moving to a cheaper weekly one;
  credit is never forfeited — change at period end instead).

### Managing customers day-to-day

All merchant-admin operations live under `/v1/merchant/*` (same public port; each
route gated by a `merchant:*` permission). Auth is a merchant API key
(`Bearer openrails_st_...`), a user session, or a trusted issuer's access token. Full
reference: [api/routes.md](api/routes.md).

| Task | Route | Console page |
|---|---|---|
| Look up a customer (profile, balances, entitlements, history) | `GET /v1/merchant/customers/{customer_id}/billing-profile` | Customers → search |
| Grant / revoke an entitlement manually | `POST /v1/merchant/customers/{customer_id}/entitlements`, `DELETE /v1/merchant/customers/{customer_id}/entitlements/{id}` | Customers → profile |
| Grant / revoke product access manually | `POST /v1/merchant/customers/{customer_id}/product-access`, `DELETE /v1/merchant/customers/{customer_id}/product-access/{id}` | Customers → profile |
| Record an off-channel/manual purchase | `POST /v1/merchant/customers/{customer_id}/payments/off-channel` | Customers → profile |
| List / inspect payments | `GET /v1/merchant/payments[/{id}]` | Payments |
| Refund (with explicit `revoke_access` choice) | `POST /v1/merchant/payments/{id}/refunds` | Payments → detail (disabled on rails without API refunds) |
| List / inspect subscriptions | `GET /v1/merchant/subscriptions[/{id}]` | Subscriptions (incl. past_due dunning view) |
| Cancel / resume a subscription | `POST /v1/merchant/subscriptions/{id}/cancel` / `/resume` | Subscriptions |
| Change a subscription's payment method | `PUT /v1/merchant/subscriptions/{id}/payment-method` | Subscriptions (NMI) |
| Grant / revoke credit | `POST /v1/merchant/customers/{customer_id}/credit-grants`, `POST .../credit-grants/{id}/revoke` | Customers → profile |
| Ask what a grant key did | `GET /v1/merchant/customers/{customer_id}/credit-grants?source_id=` | — |
| Spend delegations (per-customer agent budgets) | `PUT /v1/merchant/customers/{customer_id}/spend-delegations[/{scope}/{scope_key}]`, `DELETE .../spend-delegations/{scope}/{scope_key}` | — |
| Credit limit / trust level | `PUT /v1/merchant/customers/{customer_id}/credit-limit`, `PUT /v1/merchant/customers/{customer_id}/trust-level` | Settings |
| Catalog over HTTP | `POST /v1/merchant/catalog/products`, `PATCH /v1/merchant/catalog/products/{id}`, and the same for prices (archive with `{"archived": true}`) | Catalog |
| Metrics | `POST /v1/merchant/metrics/query`, `GET /v1/merchant/metrics/schema` | Dashboard |
| Operational alerts / findings | `GET /v1/merchant/notifications`, `GET /v1/merchant/findings` | Ops |

A user session needs a recent sign-in for every write here (403
`step_up_required` otherwise); API keys and access tokens do not. A manual grant
with no end (no `hours` or `ends_at`) also needs
`merchant:access:grant-permanent`, owner-level by default; `hours` is at most
2562047.

Destructive semantics are deliberate: refunds and cancels require an explicit
`revoke_access` decision — refunding money and revoking access are separate choices.
For refunds, requested access revocation commits with successful local refund
completion. A queued or uncertain refund (HTTP 202) retains access until that
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
`revoke_on_any`, or `keep`. Set it in the merchant configuration (`ApplyMerchantConfiguration`).

### Archiving a product with purchase refunds

`Client.ArchiveProduct` (`POST /v1/merchant/catalog/product-archives`, catalog
update plus payment refund permission) archives a product — never deletes it —
and applies the host's policy to its one-time purchases at or after
`PurchaseWindowStartsAt` (or within `WindowSeconds` of first acceptance), chosen by
`PurchaseAction`:

- `none`: archive only.
- `refund`: refund each purchase in full and end the access it granted. Purchases
  that cannot be refunded automatically (refused, declined, off-rail) become reviews.
- `review`: record each purchase for merchant review; no money moves.

The idempotency key fixes the product, action, resolved window and reason;
replays report the current outcome of every purchase and never refund twice.
A response with `complete: false` stopped at its per-request provider budget;
replay it to continue. Subscription payments are excluded (subscriptions stay
grandfathered). A purchase under review is a finding
(`life.product_archived_purchase`; the archive names it as `finding_id`),
resolved with `Client.ResolveFinding`: `approve` refunds the remaining amount
and ends access, `ignore` keeps both.

Granting credits is money-in and carries its own permission,
`merchant:credits:grant` — owner-level by default (`merchant:*`), NOT part of the
fixed support role, unlike the entitlement/product-access grants (which ride
`merchant:customer-settings:update`). The grant body's `source_id` is the
caller's reproducible idempotency key: retrying it can never double-credit
(database-enforced), and a retry with a different `amount` is refused with 409.

### The admin console

A React SPA served at its path (`/admin/` by default), driving the same `/v1/merchant/*` API — off by
default. It mounts only when console assets are built into the binary **and** it is
switched on: `Routes.AdminConsole` embedded, `admin_console.enabled: true` (env
`ADMIN_CONSOLE_ENABLED`) standalone. Login is a real AuthKit
login (password standalone; OIDC when the embedded host configures it). Build and
mount details: `docs/admin-console.md`.

Pages: **Customers** (search → profile with grant/revoke and off-channel payment),
**Subscriptions** (status filters, cancel with typed confirmation, resume, payment-
method change), **Payments** (filters, detail, rail-aware refund), **Catalog**
(products/prices CRUD, archive/restore, durable catalog batch application, drift
view), **Ops** (findings queue, the merchant inbox, worker health), **Settings** (profile,
team, PSPs, API keys, credit limit, trust level), **Dashboard**.

- **API keys** (Settings): mint scoped Bearer keys with fixed roles — `viewer`
  (read-only; the role to mint for LLM agents), `support` (+ customer operations),
  `owner` (full control, never the default). Secret shown exactly once; revocation is
  immediate.
- **Team** (Settings): AuthKit-backed roster; invite by email, change role, remove.
  A merchant always keeps at least one human owner.
- **Dashboard**: a widget grid over the metrics API. Widget queries are written by a
  server-side LLM from natural language ("count of cancels per day, past 7 days") —
  requires `llm.api_key`; without it everything else still works and the add-widget
  button explains the fix.
- **Decline metrics**, all by owner (`engine`, `nmi_schedule`, `provider`) and PSP
  (`psp`):
  - `attempt_failure_rate`: the new-card decline rate is `kind` in (`verify`, `initial`)
    with `card_entry=new`. It can also be grouped by `reason`, `category`, `response_code`,
    `card_bin`, `card_brand` and AVS/CVV.
  - `checkout_failure_rate`: the same per buyer rather than per attempt.
  - `rebill_first_failure_rate` and `rebill_missed_rate`: rebills that failed on the first
    attempt, and rebills that never happened.
  - `dunning_recovered` by `recovery_attempt` or `days_to_recover`: the recovery curve.
- **NMI history**: `nmi_history_authorizations`, `nmi_history_approved`,
  `nmi_history_refused` and `nmi_history_refusal_rate` by month (`time`), `nmi_kind`
  (`verification`, `one_off_sale`, `scheduled_rebill`), PSP, `category` and `reason`. They
  are NMI's own transaction history, read daily and kept 25 months, including the months
  before OpenRails recorded attempts. `one_off_sale` cannot separate initial sales from
  retries of declined rebills.
- **Ask your metrics** (opt-in `llm.ask_enabled`): free-form Q&A where the model runs
  validated, merchant-scoped aggregate queries and shows every result as evidence tables —
  numbers come from the API, never model prose.
- **Catalog copilot** (opt-in `llm.catalog_copilot_enabled`): Q&A over products,
  prices, subscriber counts, and pending migrations. With
  `llm.catalog_drafting_enabled`, it can additionally *draft* a price change or new
  tier — but only into the wizard's review step; nothing mutates until a human clicks
  Confirm.
