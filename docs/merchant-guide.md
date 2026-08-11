# Merchant Guide

You are the merchant: the business operator of an OpenRails deployment. This guide
covers defining what's for sale (the catalog), understanding entitlements, and running
day-to-day customer operations via the merchant API and admin console.

Vocabulary: a **rail** is a gateway kind (`nmi`, `ccbill`, `stripe`, `solana`); a
**PSP** is *your account* on a rail (e.g. a `mobius` key on the nmi rail; `stripe`,
`ccbill`, `solana` are their own PSP names). All money amounts are **integer micros**
(millionths of a currency unit): `20_000_000` = $20.00. YAML underscore separators are
just readability — there are no dollar-string amounts in the catalog manifest.

### The mental model

The catalog is **declarative**. You author products and prices in a YAML manifest (or
your embedded host defines them via the API), then *push* it. OpenRails computes a
terraform-style plan and converges its own DB **and** the provider side onto your
declared state:

- **Push** creates provider objects: Stripe Products/Prices/Features are auto-created;
  NMI recurring plans are found-or-created by `plan_id`; CCBill form links are stored
  as operator-owned identifiers; Solana recurring plans are found-or-created in USDC
  by default, or attached by `plan_pda`.
- **Pull** (the scheduled reconciliation job) is **alert-only**: it detects drift and
  orphans and records events for you to review. It never mutates providers.
- Identity is content-addressed: a product's identity is its `key`; a price's identity
  is its financial substance (currency, amount, duration, auto-renew, trial terms).
  Re-pushing, or even wiping the DB and re-pushing, re-attaches to the same provider
  objects — never duplicates.

Products grant **entitlements** — plain strings (e.g. `premium`, `tier:novice`). Your
application reads a user's entitlement timeline for access decisions, *not*
subscription rows. Subscriptions produce entitlement windows; so do one-off purchases,
admin grants, and grace. See [Entitlements](#entitlements).

### Authoring the catalog

The manifest is `catalogs:` → one entry per merchant → `products:` (plus optional
`meters:`, `credit_balances:`, `usage_limits:`). Full worked examples:
`config/catalog.example.yaml`.

**A tiered subscription** — `tier_group` + `tier_rank` make products an ordered plan
family, which is what enables upgrade/downgrade between them:

```yaml
catalogs:
  - merchant: your-merchant-slug
    products:
      - key: novice
        display_name: Novice
        tier_group: membership
        tier_rank: 1
        entitlements: [tier:novice]
        prices:
          - currency: usd
            unit_amount: 12_000_000     # $12.00
            duration: 30d               # access window per charge
            auto_renew: true            # recurring; requires a finite duration
            psps: [stripe]              # which of your PSPs sells this price
```

Price fields worth knowing:

| Field | Meaning |
|---|---|
| `unit_amount` | integer micros |
| `duration` | access window: `Nd`/`Nh`, or `indefinite` (default — perpetual ownership) |
| `auto_renew` | charge again and extend at each period end; rejected with `indefinite` |
| `trial` | optional first phase: `{unit_amount: 0, duration: 7d}` = free 7-day trial, then the recurring terms; requires `auto_renew` |
| `key` | optional durable handle; defaults to `<product-key>-<interval>` — two prices at the same interval must each set an explicit key |
| `archived` | bool; omitted/false = active. This is the whole lifecycle — there is no draft/active enum |
| `psps` | explicit PSP list; omitted = OpenRails-native only, no provider sync |
| `psp_links` | pre-supply provider ids, validated on apply (below) |

**A one-time purchase** — omit `auto_renew`; use a finite `duration` for timed access
or `indefinite` for ownership:

```yaml
      - key: prepaid-api-credits
        display_name: Prepaid API Credits
        credits:
          - {key: inference-api, amount: 10_000, cadence: once}
        prices:
          - currency: usd
            unit_amount: 20_000_000
            duration: indefinite
            psps: [stripe]
```

Credits reference a top-level balance declaration
(`credit_balances: [{key: inference-api, unit: api-credit, expires_default: 365d}]`).
`cadence` is `once` (initial activation) or `per_renewal` (granted on each confirmed
renewal — webhook-replay safe).

**Credits expire only if you say so.** A grant takes its lifetime from its own
`expires`, else the balance's `expires_default`. Declare neither and the balance
never expires — OpenRails will not put a clock on customer money you did not ask
for.

**A variable credit top-up** — buy *any* amount within bounds; graduated tiers give
volume discounts and stay monotonic, so the quote inverts cleanly (enter $ → credits,
or credits → $):

```yaml
      - key: image-credit-topup
        display_name: AI Image Credits
        credits:
          - key: ai-image-gen
        prices:
          - currency: usd
            psps: [stripe]
            input_min: 5_000_000        # $5 minimum spend
            input_max: 500_000_000
            model: tiered
            tiered:
              mode: graduated
              tiers:
                - {up_to: 2_000,  unit_amount: 10_000}   # first 2,000 credits at $0.01
                - {up_to: 10_000, unit_amount: 9_000}
                - {up_to: null,   unit_amount: 8_000}    # last tier unbounded
```

**Charge models** (shared by credit purchases and metered rate cards):

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
products declare no billing cadence — the invoice period is the window. See the
`digital-ocean` example in `config/catalog.example.yaml` for the full pattern.

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

### Pushing and verifying

Catalog push is one of the three provisioning surfaces (`push-auth-bootstrap`,
`push-merchant-config`, `push-merchant-catalog` — see `docs/merchant-provisioning.md`).
A bare command is **plan-only**; mutation classes are explicit flags that compose:

```bash
openrails push-merchant-catalog -f catalog.yaml                      # plan only, prints the diff
openrails push-merchant-catalog -f catalog.yaml --insert             # create missing products/prices/provider objects
openrails push-merchant-catalog -f catalog.yaml --insert --overwrite # + update existing OpenRails-owned rows
openrails push-merchant-catalog -f catalog.yaml --insert --overwrite --prune  # full convergence; prune archives extras
```

`--prune` only archives OpenRails-owned objects absent from the manifest — foreign
provider objects are never touched. Declared prices are a SET: an active price whose
financial identity is not declared gets archived under `--prune`.

Round-trip check: `openrails dump-merchant-catalog --slug <merchant>` emits the live
catalog as push-compatible YAML.

Provider side per rail:

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
| Spend delegations (per-customer agent budgets) | `PUT /v1/merchant/customers/{id}/spend-delegations[:upsert]` | — |
| Credit limit / trust level | `PUT /v1/merchant/credit-limit`, `GET /v1/merchant/trust-level` | Settings |
| Catalog CRUD over HTTP | `POST/PATCH /v1/merchant/catalog/products`, `/prices` | Catalog |
| Metrics | `GET /v1/merchant/metrics`, `/v1/merchant/metrics/query` + `/schema` | Dashboard |
| Repair alerts / drift findings | `GET /v1/merchant/repair-alerts` | Ops |

Destructive semantics are deliberate: refunds and cancels require an explicit
`revoke_access` decision — refunding money and revoking access are separate choices.

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
(products/prices CRUD, activate/deactivate, manifest publish with plan preview, drift
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
