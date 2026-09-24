# Merchant Guide

You are the merchant: the business operator of an OpenRails deployment. This guide
covers defining what's for sale (the catalog), understanding entitlements, and running
day-to-day customer operations via the merchant API and admin console.

Vocabulary: a **rail** is a gateway kind (`nmi`, `ccbill`, `stripe`, `solana`); a
**PSP** is *your account* on a rail (e.g. a `mobius` key on the nmi rail; `stripe`,
`ccbill`, `solana` are their own PSP names). Money amounts are **integers in the
currency's native units** (micros for USD: `20_000_000` = $20.00; the scale per
currency is `GET /v1/currencies`). YAML underscore separators are
just readability — there are no dollar-string amounts in the catalog manifest.

### The mental model

The database is the catalog. Authorized clients can edit individual records when
`allow_catalog_updates: true`, or submit a JSON/YAML batch of changes. The flag
defaults false; a trusted local operator can still apply bootstrap documents.

Each batch has an application ID and an expected merchant catalog revision.
Reusing an applied ID and identical contents returns the original receipt without
repeating changes, even after later API edits. A new ID intentionally applies the
document again against its pinned revision. Omitted records survive unless
`prune: true`; explicit `archived: true` retires a known record independently.

Products have stable keys. Prices have immutable financial terms; changing terms
under a stable price key selects a new financial version. Existing subscriptions
retain their historical price. Provider reconciliation detects drift separately;
it does not make a startup file continuously authoritative.

Products grant **entitlements** — plain strings (e.g. `premium`, `tier:novice`). Your
application reads a user's entitlement timeline for access decisions, *not*
subscription rows. Subscriptions produce entitlement windows; so do one-off purchases,
admin grants, and grace. See [Entitlements](#entitlements).

### Applying through the Go Client

Use `client.Catalog.Apply(ctx, params)` for embedded and remote Clients. Decode
YAML with `openrails.ParseCatalogApplicationYAML`; both encodings share the same
validation and authorization as individual writes. The HTTP operation is
`POST /v1/merchant/catalog/applications`; read the required base revision through
`client.Catalog.Revision(ctx)` or `GET /v1/merchant/catalog/revision`.

The server commits local products, prices, related definitions, history and the
application receipt atomically. Unsupported provider changes fail before mutation;
supported external work is durable and reported separately. Retry an uncertain
response with exactly the same ID and contents. A changed revision is a conflict,
not permission to silently refresh the precondition and overwrite intervening edits.

The default target is the merchant-owned catalog. An explicit catalog ID must be
inside the authenticated merchant and caller's authority; pruning never implicitly
includes other creator catalogs. Meter/rate-card dependency checks still apply;
prune does not delete historical billing definitions or customer rate overrides.

For bootstrap, `embed/operator.Operator.ApplyCatalog` uses the same private engine
with trusted local authority. Runtime has no catalog business methods.

### Authoring the catalog

One application selects one authorized merchant outside the document and includes
`schema_version`, `application_id`, `expected_revision`, optional `catalog_id`,
`prune`, `products` and supported `meters`. See `config/catalog.example.yaml`.

**A tiered subscription** — `tier_group` + `tier_rank` make products an ordered plan
family, which is what enables upgrade/downgrade between them:

```yaml
schema_version: 1
application_id: membership-launch-1
expected_revision: 0
prune: false
products:
  - key: novice
    display_name: Novice
    tier_group: membership
    tier_rank: 1
    entitlements_spec: {tier:novice: null}
    prices:
      - key: novice-monthly
        currency: USD
        unit_amount: 12000000
        access_duration_hours: 720
        auto_renew: true
```

Price fields worth knowing:

| Field | Meaning |
|---|---|
| `unit_amount` | integer native units at the currency's registered scale (micros for USD); a JSON application (`POST /merchant/catalog/applications`) spells it as a decimal string |
| `access_duration_hours` | positive hour count, or null for indefinite access |
| `auto_renew` | charge again and extend at each period end; requires a finite access duration |
| `trial_unit_amount`, `trial_duration_hours` | first-phase terms; supply both together and enable renewal |
| `key` | required stable application handle for the price version chain |
| `archived` | explicit true retires, explicit false reactivates; omission preserves an existing value |
| `psps` | explicit PSP list; omitted = OpenRails-native only, no provider sync |
| `psp_links` | pre-supply provider ids, validated on apply (below) |

**A one-time purchase** — false `auto_renew` with a finite duration gives timed
access; null `access_duration_hours` gives indefinite access:

```yaml
      - key: course-101
        display_name: Course 101
        entitlements_spec: {course:101: null}
        prices:
          - key: course-101-usd
            currency: USD
            unit_amount: 20000000
            access_duration_hours: null
            auto_renew: false
```

Prepaid balances are not catalog products: fund them with
`POST /v1/merchant/credits/deposit` (`Client.DepositCredits`), whose grants carry
their own expiry.

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
period finalize rates reported usage through the cards and invoices every payer with
ledger or metered activity, including a payer whose only activity is metered usage.
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

Application identity, expected revision and prune belong in the document, not
CLI mutation flags. Keep the same artifact across restarts; review current state
before authoring a new application ID. The returned receipt proves what committed,
not that no one has edited the catalog since.

For host-local merchants without AuthKit bindings, add `--unbound-merchants`.
Name resolution does not fall back between AuthKit and host-local namespaces.
Catalog exports are inspection snapshots; author explicit application identity and
revision before applying changes from an export.

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

- Windows are appended at the tail (`PushNewEntitlement`) or revoked immediately
  (`RevokeExistingEntitlement`); `end_at` is immutable — renewals append new windows.
- Every window carries a source: `subscription`, `one_off`, `admin`, or `grace`.
- **Grace** is bounded, revocable generosity: paid windows stay truthful, and access
  beyond payment is a separate grace window that lapses by its own `end_at` if truth
  never arrives (fail-closed). Renewal grace is pre-appended so a late webhook never
  gates a paying user; a deliberate cancel deletes scheduled grace — access ends at
  the period end the user expects.
- Tier changes go through `POST /v1/me/subscriptions/{id}/change-tier` (target price
  must share the tier group). Stripe and NMI upgrade immediately with proration;
  downgrades are scheduled for period end (`delayed_start`). CCBill upgrades redirect
  to a FlexForm; Solana does not support tier changes.
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
(`Bearer openrails_st_...`), a first-party service JWT, or a user session. Full
reference: `docs/api/endpoints.md`.

| Task | Route | Console page |
|---|---|---|
| Look up a customer (profile, balances, entitlements, history) | `GET /v1/merchant/customers/{id}` | Customers → search |
| Grant / revoke an entitlement manually | `POST` / `DELETE /v1/merchant/customers/{id}/entitlements[/{grant_id}]` | Customers → profile |
| Grant / revoke product access manually | `POST` / `DELETE /v1/merchant/customers/{id}/product-access[/{grant_id}]` | Customers → profile |
| Record an off-channel/manual purchase | `POST /v1/merchant/customers/{id}/payments/off-channel` | Customers → profile |
| List / inspect payments | `GET /v1/merchant/payments[/{id}]` | Payments |
| Refund (with explicit `revoke_access` choice) | `POST /v1/merchant/payments/{id}/refunds` | Payments → detail (disabled on rails without API refunds) |
| List / inspect subscriptions | `GET /v1/merchant/subscriptions[/{id}]` | Subscriptions (incl. past_due dunning view) |
| Cancel / resume a subscription | `POST /v1/merchant/subscriptions/{id}/cancel` / `/resume` | Subscriptions |
| Change a subscription's payment method | `PUT /v1/merchant/subscriptions/{id}/payment-method` | Subscriptions (NMI) |
| Deposit credits (machine/rails) | `POST /v1/merchant/credits/deposit` | — |
| Grant credits to a customer (human admin) | `POST /v1/merchant/customers/{id}/credits` | Customers → profile |
| Ask what a deposit key did | `GET /v1/merchant/credits/deposit?customer_id=&source_id=` | — |
| Spend delegations (per-customer agent budgets) | `PUT /v1/merchant/customers/{id}/spend-delegations[:upsert]`, `DELETE .../spend-delegations/{scope}/{scope_key}` | — |
| Credit limit / trust level | `PUT /v1/merchant/credit-limit`, `GET /v1/merchant/trust-level` | Settings |
| Catalog CRUD over HTTP | `POST/PATCH /v1/merchant/catalog/products`, `/prices` | Catalog |
| Metrics | `POST /v1/merchant/metrics/query`, `GET /v1/merchant/metrics/schema` | Dashboard |
| Repair alerts / drift findings | `GET /v1/merchant/repair-alerts` | Ops |

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

### Archiving a product with purchase refunds

`Client.ArchiveProduct` (`POST /v1/merchant/catalog/product-archives`, catalog
update plus payment refund permission) archives a product — never deletes it —
and applies the host's policy to its one-time purchases at or after
`PurchasedSince` (or within `Window` of first acceptance):

- `none`: archive only.
- `refund`: refund each purchase in full and end the access it granted. Purchases
  that cannot be refunded automatically (refused, declined, off-rail) become reviews.
- `review`: record each purchase for merchant review; no money moves.

The idempotency key fixes the product, action, resolved window and reason;
replays report the current outcome of every purchase and never refund twice.
A response with `complete: false` stopped at its per-request provider budget;
replay it to continue. Subscription payments are excluded (subscriptions stay
grandfathered). Reviews are listed with `Client.ListPurchaseReviews` and
resolved with `Client.ResolvePurchaseReview`: `refund` returns the remaining
amount and ends access, `dismiss` keeps both. They also appear in the findings
queue as `life.product_archived_purchase`.

Granting credits is money-in and carries its own permission,
`merchant:credits:grant` — owner-level by default (`merchant:*`), NOT part of the
fixed support role, unlike the entitlement/product-access grants (which ride
`merchant:customer-settings:update`). The grant body's `source_id` is the
caller's reproducible idempotency key: retrying it can never double-credit
(database-enforced), and a retry with a different `amount` is refused with 409.

### The admin console

A React SPA served at `/admin/`, driving the same `/v1/merchant/*` API — off by
default. It mounts only when console assets are built into the binary **and**
`admin_console.enabled: true` (env `ADMIN_CONSOLE_ENABLED`). Login is a real AuthKit
login (password standalone; OIDC when the embedded host configures it). Build and
mount details: `docs/admin-console.md`.

Pages: **Customers** (search → profile with grant/revoke and off-channel payment),
**Subscriptions** (status filters, cancel with typed confirmation, resume, payment-
method change), **Payments** (filters, detail, rail-aware refund), **Catalog**
(products/prices CRUD, activate/deactivate, durable catalog batch application, drift
view), **Ops** (findings queue, repair alerts, worker health), **Settings** (profile,
team, payment providers, API keys, credit limit, trust level), **Dashboard**.

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
- **Ask your metrics** (opt-in `llm.ask_enabled`): free-form Q&A where the model runs
  validated, RLS-pinned aggregate queries and shows every result as evidence tables —
  numbers come from the API, never model prose.
- **Catalog copilot** (opt-in `llm.catalog_copilot_enabled`): Q&A over products,
  prices, subscriber counts, and pending migrations. With
  `llm.catalog_drafting_enabled`, it can additionally *draft* a price change or new
  tier — but only into the wizard's review step; nothing mutates until a human clicks
  Confirm.
