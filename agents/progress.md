<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 599

---

# #598: Speculative — eliminate catalog-derived entitlement rows and derive access from product ownership

**Completed:** no

Decision 2026-06-28: this is a speculative simplification candidate, not an approved
hard cut yet. The current `openrails.entitlements` table may be duplicate state for
catalog-derived access if product ownership windows plus current product definitions can
answer the same reads. Evaluate replacing catalog-derived entitlement rows with derived
reads, while preserving manual/non-catalog grants and audit history.

## Hypothesis
Catalog-derived entitlements do not need their own source-of-truth rows.

Source-of-truth facts should be:
- product catalog: `product_slug -> entitlements/spend limits/linked products`.
- product ownership windows: `customer owns product_slug from starts_at to ends_at`,
  including grace windows.
- `grants`: append-only audit log for "customer got ownership/credit/manual entitlement
  because of payment/import/admin action."

Effective entitlements can then be derived at read time:

```text
active product ownership windows
+ current product definitions
+ manual/non-catalog entitlement grants
= active entitlements
```

## Why consider this
- Catalog changes become instant by construction: changing `premium -> pro` changes
  effective reads for active owners without rewriting per-customer entitlement rows.
- Grace/dunning becomes an ownership-window concern instead of special entitlement
  timeline mutation.
- Less duplicated state: no separate catalog-derived entitlement lifecycle to keep in
  sync with subscriptions, product ownership, catalog mutation, provider pull, and grace.
- Auditing remains in `grants`: the audit event is "user got product ownership", not
  "user got whatever entitlements the product happened to grant that day."

## What must remain supported
- Manual comp/support entitlements that are not represented by product ownership.
- Imported legacy one-off entitlements that cannot be mapped to product ownership.
- Reverse lookup: list customers with entitlement X.
- Batch active entitlement lookup for AuthKit/token enrichment.
- Historical audit: answer what caused access and when ownership/grace started/ended.
- Performance for hot reads without making every request scan huge ownership/catalog sets.

## Possible end state
Prefer the smallest end state that holds:

1. `product_ownership` is source of truth for product access windows.
2. Manual/non-catalog entitlements live in a small manual grant table or in `grants`
   with enough typed fields to query them.
3. Catalog-derived entitlements are computed from ownership + current product spec.
4. If performance requires materialization, keep `effective_entitlements` as a
   rebuildable projection/cache, not a source-of-truth table.
5. Delete or stop writing `openrails.entitlements` only after every read/write path has
   moved to source-of-truth ownership/manual grants or the projection boundary.

## Risks / reasons to reject
- Reverse entitlement lookup may need an indexed projection.
- Existing dunning/grace/refund code may rely on entitlement timeline rows more deeply
  than expected.
- Long-lived JWTs can still be stale after catalog changes unless token freshness is
  enforced elsewhere.
- Historical "what entitlement did this user have on date X?" may require catalog
  version history, not just current product definitions.
- Migration may be too risky if the current entitlement table is already serving as a
  useful compatibility projection.

## Tasks
- [ ] Inventory every writer to `openrails.entitlements` and classify it as catalog-derived, manual/imported, or projection maintenance.
- [ ] Inventory every reader of active entitlement names/records and classify whether it can derive from ownership + catalog or needs an indexed projection.
- [ ] Define product ownership window schema keyed by merchant + customer + product slug, including subscription, one-time, admin, import, grace, revoke, refund, and provider-pull sources.
- [ ] Define manual/non-catalog entitlement storage; do not mix these with catalog-derived entitlements.
- [ ] Prototype derived active entitlement query for one customer and batch customers.
- [ ] Prototype reverse lookup `entitlement -> customers`, using either derived joins or a rebuildable projection.
- [ ] Decide whether `openrails.entitlements` can be deleted or should be renamed/reframed as `effective_entitlements` projection.
- [ ] Add migration/backfill plan from current subscription/product_access/entitlement rows into product ownership windows plus manual grants.
- [ ] Add tests for catalog mutation changing effective entitlements immediately, grace preserving ownership-derived access, grace expiry removing derived access, manual grants coexisting with catalog-derived access, and reverse lookup.
- [ ] Only after tests/proof: delete or stop source-of-truth writes to `openrails.entitlements`.

---

# #597: Provider adoption/import — rebuild local billing mirror from provider truth when identity/catalog resolve

**Completed:** no

Decision 2026-06-28: `pull-provider` stays reconciliation against an existing
OpenRails mirror. A different command should handle adoption/import: create local
customers, payment methods, subscriptions, payments, and derived grants from provider
state only when ownership and catalog mapping are deterministic.

## Goal
Support this recovery/bootstrap path:

1. A host migrates users/subjects locally.
2. OpenRails has provider credentials and catalog mappings.
3. `adopt-provider` reads NMI/CCBill/Stripe/Solana provider state.
4. It resolves each provider object to a local merchant subject + local price/product.
5. It creates the missing OpenRails billing mirror rows.
6. `pull-provider` then verifies/repairs drift.

## Non-goal
No probabilistic imports. Email is report evidence, not authority. Provider customer ids
are rail-local ids, not OpenRails subjects. Provider plan ids are not OpenRails catalog
semantics unless metadata or a manifest maps them.

## Resolution order
1. Provider metadata recovery envelope:
   - merchant slug/id
   - OpenRails subject
   - product slug
   - price key/id
   - checkout/subscription/payment/payment-method ids when available
2. Adoption manifest:
   - provider customer/vault/subscription ids -> host subject ids
   - provider plan/price ids -> OpenRails price keys/ids
   - CCBill numeric price/subscription ids or flex/form ids -> OpenRails price keys
   - provider account bindings
3. Otherwise unresolved. Emit a report row; do not create local billing state.

## CLI shape

```bash
openrails adopt-provider --provider nmi --manifest adoption.yaml
openrails adopt-provider --provider nmi --manifest adoption.yaml --insert
```

Default is plan-only. `--insert` applies only deterministic rows.

## Materialized rows
- `customers` for resolved host subjects.
- payment-method/vault references tied to provider account + customer.
- subscriptions tied to resolved customer + price.
- payments tied to resolved customer/subscription/price.
- grants/entitlements/credits/product ownership/spend limits derived from the local
  product benefit bucket, not from provider catalog guesses.
- provider-account bindings on adopted rows.

## Tasks
- [ ] Define `adoption.yaml` schema for legacy mappings.
- [ ] Add plan-only `adopt-provider` command and report format.
- [ ] Reuse existing provider fetchers; do not duplicate rail clients.
- [ ] Implement deterministic resolver: metadata first, manifest second, unresolved third.
- [ ] Implement insert path for customers, vault refs, subscriptions, payments.
- [ ] Derive grants from local product benefits after subscription/payment creation.
- [ ] Add tests: metadata-only adoption, manifest-only adoption, unresolved rows, ambiguous rows, idempotent re-run.
- [ ] Document: adoption imports provider truth; `pull-provider` reconciles after adoption.

---

# #596: Stamp OpenRails recovery metadata on every provider-created object

**Completed:** no

Decision 2026-06-28: provider adoption is only safe if OpenRails-created provider
objects carry canonical OpenRails breadcrumbs. Add a single recovery envelope and
stamp it wherever each provider supports metadata or stable operator fields.

## Recovery envelope

Logical fields:

```text
openrails_version
merchant_slug
merchant_id
subject
product_slug
price_key
price_id
checkout_session_id
subscription_id
payment_id
payment_method_id
provider_account_id
```

Use the subset each provider supports, but keep one canonical internal shape.

## Provider behavior
- Stripe: product metadata and price `lookup_key`; keep `prod_...` / `price_...` as links.
- NMI: product `product_sku`, product `product_description`, recurring `plan_id`, `plan_name`,
  and order/customer/vault metadata fields that survive query/report reads.
- CCBill: form/flex/custom fields that survive DataLink exports; numeric admin price ids are
  provider links, not OpenRails identity.
- Solana: plan/payment memo/account metadata where feasible.

## Tasks
- [ ] Add canonical `RecoveryMetadata` type + serializer/parser.
- [ ] Stamp metadata during catalog/provider price creation.
- [ ] Stamp metadata during checkout/session creation.
- [ ] Stamp metadata during subscription creation/update.
- [ ] Stamp metadata during payment-method vault creation.
- [ ] Stamp metadata during payment creation/refund where providers support it.
- [ ] Extend provider fetchers to parse the envelope back into remote snapshots.
- [ ] Tests per rail: created provider payload includes metadata; fetched snapshot recovers it.

---

# #595: Deterministic, bidirectional catalog identity for products and prices

**Completed:** no

Decision 2026-06-28: OpenRails catalog identity should be recoverable from natural
descriptors. Products are mutable benefit buckets. Prices are immutable commercial
terms pointing at a product.

## Product identity and label
Product slug is the canonical immutable identifier used in APIs, manifests, provider
metadata, and DB relationships. Scope it by merchant. A separate product UUID or stored
product key is duplicate state if slug is immutable; use `(merchant_id, slug)` as the
DB key. The product's benefits/settings are mutable; ownership of the product remains
stable.

`display_name` is the mutable user-facing label. Provider mapping should follow the
same split where possible: NMI uses a unique product SKU for identity and product
description for the user-facing name.

## Price identity
Price natural key:

```text
price:<merchant_slug>:<product_slug>:<currency>:<unit_amount>:<billing_period>
```

Examples:

```text
price:doujins:premium:usd:25000000:30d
price:doujins:api-credits-100:usd:100000000:once
```

Price key should be deterministic from that natural key. Unit amount/currency/billing
period/product-link changes create a new price. Mutable price fields should be
operational only: status/archive, provider links, timestamps.

## Bidirectional mapping
- local merchant + product slug -> provider product via provider links.
- local price key -> provider price/plan via provider links.
- provider product/price -> local product slug and price key via stamped metadata.
- legacy provider product/price -> local product slug and price key via adoption manifest.

## Provider identifier mapping
- Stripe product: prefer caller-owned product `id` derived from product slug when allowed;
  otherwise store `product_slug` in metadata and keep Stripe's `prod_...` id as a provider link.
- Stripe price: provider `price_...` is an opaque link; use deterministic `lookup_key` for
  the OpenRails price key and stamp metadata as backup.
- NMI product: map `product_sku = product slug`; map `product_description = display_name`.
- NMI recurring plan: map `plan_id = price key`; map `plan_name = display_name`.
- CCBill: no OpenRails-owned product slug object. Store `form_name`, `flex_id`, and any
  numeric Pricing Admin price/subscription ids as provider links; resolve legacy rows via
  adoption manifest.

## Tasks
- [ ] Add canonical product slug validation and price-key builder.
- [ ] Make products use immutable natural identity (`merchant_id + slug`) instead of a separate product UUID/key.
- [ ] Store mutable product `display_name` separately from immutable product `slug`.
- [ ] Store price natural keys or make them derivable from existing columns.
- [ ] Enforce price uniqueness by merchant + product + currency + unit amount + billing period.
- [ ] Keep product slug mutable fields separate from identity fields.
- [ ] Update provider link logic with Stripe/NMI direct identifiers and CCBill link-only identifiers.
- [ ] Add tests for stable product slug, changed product benefits preserving ownership, changed price terms creating a new price key.

---

# #594: Product benefit buckets — entitlements, credits, ownership grants, and spend limits

**Completed:** no

Decision 2026-06-28: a product is the mutable bucket of benefits a user gets by
owning/subscribing to that product. Price is only the commercial term. Product benefits
must model all OpenRails-owned access and billing effects directly.

## Target manifest shape

```yaml
products:
  - slug: premium
    display_name: Premium
    description: Optional text
    entitlements:
      - premium
    credits:
      - currency: usd
        amount: 25000000
        expires_after_days: null
    product_ownership:
      - product-b
      - product-c
    spend_limits:
      - window: 5h
        amount: 10000000
        currency: usd
      - window: 7d
        amount: 70000000
        currency: usd
    prices:
      - currency: usd
        unit_amount: 25000000
        billing_period: 30d
        providers:
          - stripe
          - nmi
```

Prices are nested under products. A price's identity includes the product slug plus
commercial terms, so a top-level `prices:` section would duplicate the product link
and invite drift. Products may have zero prices when they are benefit-only buckets
granted by another product; sellable products declare one or more nested prices.

Durations use one explicit OpenRails catalog grammar anywhere the manifest expresses
a time length: price `billing_period`, spend-limit `window`, and future duration
fields. Use Go-style strings with one OpenRails extension: `h` and `d`. Examples:
`24h`, `30d`, `365d`. Missing price `billing_period` means one-time; `once` is
accepted as an explicit spelling. Canonicalize equivalent durations before storing
or building keys, so `24h` and `1d` resolve to the same period. Go's standard
`time.ParseDuration` handles `h` but not `d`, so this needs a tiny catalog parser
for `h` and `d`; do not add a dependency.

Provider adapters translate the canonical duration into each provider's native shape.
Stripe recurring prices use `interval` (`day`, `week`, `month`, `year`) plus
`interval_count`; for OpenRails `30d`, prefer Stripe `interval=day` and
`interval_count=30` rather than calendar `month`. Price billing periods must be
`once` or whole-day durations; sub-day durations like `5h` are valid for spend-limit
windows, not provider recurring prices.

Amounts are currency-native integer units. For USD, use micro-dollars: $25.00 is
`25000000`, $10.00 is `10000000`, and $70.00 is `70000000`.
Prices use Stripe's `unit_amount` field name. Credits and spend limits use `amount`
because they are OpenRails ledger/budget amounts, not provider price objects.

Examples this must cover:
- `premium`, `premium-plus` entitlement grants.
- API-credit packs: pay $25 for $25 credits, or $100 for $110 credits.
- Product ownership chaining: buy product-A, also own product-B and product-C.
- Spend/rate limits: product-A grants 5h/$10 + 7d/$70; product-B grants 2x that.

## Semantics
- Product identity is stable; benefits are mutable.
- Durable product ownership windows are the source of truth for access. Subscriptions,
  one-time purchases, imports, and admin grants create/extend/revoke ownership windows
  for product slugs.
- Effective entitlements are derived from active product ownership plus the current
  product catalog definition. Changing `premium -> pro`, adding an entitlement,
  removing an entitlement, changing linked product ownership, or changing spend limits
  updates effective OpenRails benefits for every current owner without rewriting
  per-user catalog-derived entitlement rows.
- Grace is an ownership-window state/extension, not an entitlement special case. A
  subscription in grace still owns the product until `grace_ends_at`; when grace ends,
  product ownership ends and derived benefits disappear.
- Credit grants are payment ledger entries, not continuously recomputed access state.
  Each successful one-time purchase grants the product's configured credits once. Each
  successful recurring payment grants them once for that billing period. Editing catalog
  credit grants changes future payment grants; existing granted/spent credits remain
  auditable unless an explicit credit migration action is run.
- Grants are the audit log: "this user got ownership/credit/manual entitlement because
  of this payment/import/admin action." Source product/subscription/payment is preserved.
- The existing entitlement read surface should be backed by read-time derivation or a
  rebuildable projection. Keep entitlement rows only for manual/non-catalog grants or
  as a compatibility cache, not as the source of truth for catalog-derived benefits.
- Provider catalog never owns these semantics; providers only receive sellable products/prices.

## Provider boundary
- OpenRails owns entitlements, credits, linked product ownership, and spend limits.
- Stripe can mirror product display/description and some entitlement-like metadata/features,
  but it does not own OpenRails product ownership, credits, or spend limits.
- NMI and CCBill own sellable/payment-side objects only; they do not own OpenRails benefits.
- Provider sync is best-effort for provider-visible fields. Benefit changes must not depend
  on provider mutation succeeding.

## Storage approach
Lazy path first: extend the current product JSON specs rather than splitting tables
immediately. Split later only if querying/reporting demands it.

## Tasks
- [ ] Extend catalog manifest with flattened product benefit fields.
- [ ] Keep prices nested under products; allow benefit-only products with no direct prices.
- [ ] Normalize price `unit_amount` and benefit `amount` values as currency-native integer units; USD examples use micro-dollars.
- [ ] Replace catalog `interval`/`interval_count` with optional `billing_period` strings (`24h`, `30d`, `365d`; missing means one-time; `once` allowed) and canonicalize equivalent periods.
- [ ] Use the same duration parser/canonical form for spend-limit `window` values.
- [ ] Translate canonical billing periods into provider-native recurrence fields; Stripe gets `interval` + `interval_count`.
- [ ] Reject sub-day `billing_period` values for provider-backed prices while still allowing sub-day spend-limit windows.
- [ ] Map existing `entitlements` and `credits` into the new benefit model.
- [ ] Apply credit grants from successful payments/renewals, not from continuous ownership recomputation.
- [ ] Make product ownership windows the source of truth for subscription, one-time purchase, import, grace, and admin access.
- [ ] Derive effective entitlements from active ownership windows plus current product definitions.
- [ ] Treat `grants` as the audit ledger for ownership/credit/manual-entitlement events.
- [ ] Demote current entitlement rows to manual grants and/or rebuildable compatibility projection.
- [ ] Add product ownership grants to the product benefit application path.
- [ ] Add spend-limit/rate-limit grants to the product benefit application path.
- [ ] Add explicit credit expiry support in product credit grants.
- [ ] Update catalog apply/upsert so product benefit changes are visible to active holders immediately through derived reads or projection refresh.
- [ ] Keep provider sync limited to provider-visible product/price fields; OpenRails benefit changes stay local and authoritative.
- [ ] Tests: purchase applies all benefit kinds; product mutation updates active holders; provider adoption derives grants from benefits; provider sync failure does not block local benefit convergence.

---

# #591: platform identity & billing model — customer/merchant anchors (authkit groups); merged merchant_external_account; vault + lifecycle axes

**Completed:** no

STATUS 2026-06-28 (Claude): PLAN / north-star, REVISED to the converged model (owner-reviewed).
Umbrella for #588, #589, doujins #426. **Build incrementally** — today's model keeps working; the
platform layer lands as additive migrations (one new anchor table + a table merge + nullable /
defaulted columns), never a hot-table rewrite.

## Vision
OpenRails becomes a platform (Stripe Link / Shop Pay): a customer registers on OpenRails, saves a
card into OpenRails' OWN vault (self-hosted HyperSwitch, PCI), and OpenRails drives the
subscription lifecycle. This must COEXIST with today's model (provider tokenizes/vaults; provider
OR OpenRails drives the lifecycle), per-merchant AND per-customer — so it's data-driven, never a
global mode switch.

## Identity = authkit (do NOT rebuild it in billing)
authkit primitives (verified in ~/authkit): a **persona** is a permission-namespace type
(`PersonaDef`); a **permission group** is an instance (`CreatePermissionGroup`, contract.go:277);
**owner** is the group's apex role (contract.go:48); **principal kind** is the auth method and
`user` is ONE of five (principal.go:7-11): user, api_key, remote_application, delegated, service.
authz = `Can(subject, kind, persona, instance, perm)`.
- `customer` and `merchant` are **personas**; a specific one is a **permission group**; the
  registering `user` holds `owner` in each it creates — a user can own a customer AND a merchant group.
- authkit owns identity+authz; openrails owns BILLING keyed by the **group id** (opaque, NO FK;
  invoker-opaque + de-conflation patterns).

## THE RULE: bill by permission-group identity, never by user_id
openrails never keys billing by `user_id` — it keys by the **group** (the customer/merchant
permission-group id). This makes user-owns-both, ownership transfer, team membership, and
api_key/delegated/service actors all Just Work via authkit with zero re-keying. openrails is
principal-kind-agnostic: it runs `Can()` and uses the opaque invoker label for budgets/audit;
never branches on kind. No owner/roles/team tables in openrails.

## Tables (7; converged with owner 2026-06-28)
1. **customer** — thin anchor; PK = customer permission-group-id. The entry ownership hangs off.
2. **merchant** — thin anchor; PK = merchant permission-group-id.
3. **merchant_external_account** — a (merchant x rail) account: `merchant_id, provider_type
   (nmi|stripe|ccbill|solana), account_id, creds_ref, owner (merchant|platform), role, env`.
   MERGES the two we'd split — a merchant's own gateway AND "charge this merchant's customers
   against us" (owner=platform). "mobius" is a ROW here (provider_type=nmi), NOT a rail value.
4. **payment_methods** — `customer_id` (buyer, global); `stored_in -> merchant_external_account`
   (the vault: a merchant gateway, or the platform vault); rail handle = ONE or TWO keys
   (`rail_method_ref` always; `rail_customer_ref` for rails that need the customer handle to
   charge, e.g. NMI vault+billing). Replaces vault_id/billing_id; adds
   network_transaction_id/mandate_ref (off-session/MIT); kind/fingerprint deferred.
5. **payments** — `customer_id + merchant_id + charged_via -> merchant_external_account`.
6. **subscriptions** — `customer_id + merchant_id + payment_method_id + lifecycle_owner`.
7. **merchant_customer_config** — thin, sparse per-(merchant, customer) CONFIG sidecar:
   `merchant_id, customer_id, default_payment_method_id, standing (e.g. blocked),
   merchant_external_customer_ref (the merchant's own CRM id for this buyer)`. A row exists only
   when there's something to store.

`merchant_customer_config` is NOT a routing "edge": per-relationship STATE (balance, entitlements,
budgets) stays in the existing purpose-built tables keyed `(customer_id, merchant_id)` —
`ledger_accounts`, `entitlements`, `invoker_spend_limits` (they already carry both ids). The config
table only holds the loose per-relationship fields that otherwise have no home.

## Multi-tenant RLS (tenant separation — still required)
Many merchants share ONE database, so row-level security stays the tenant boundary (today's
`FORCE ROW LEVEL SECURITY` + `merchant_isolation` policy pattern). Two RLS axes:
- **merchant-scoped tables** (`merchant_external_account`, `payments`, `subscriptions`,
  `entitlements`, `ledger_accounts`, `invoker_spend_limits`, `merchant_customer_config`) →
  policy `merchant_id = current_setting('app.merchant_id')`. A merchant tenant NEVER sees another
  merchant's rows. Unchanged from today.
- **customer-global tables** (`customer`, and platform-vaulted `payment_methods`) span merchants,
  so they are NOT merchant-scoped — they use a **customer** context
  (`customer_id = current_setting('app.customer_id')`, the portal). A merchant session must NOT be
  able to SELECT a customer's cross-merchant card (privacy); a merchant charge reaches it only via a
  mediated charge op, not a direct read.
- `payment_methods` spans both: merchant-vaulted rows (owner=merchant, `merchant_id` set) → merchant
  RLS; platform-vaulted rows (owner=platform, customer-global) → customer RLS. Policy: visible if
  `merchant_id = app.merchant_id` OR `customer_id = app.customer_id`.
Invariant: a merchant tenant sees neither another merchant's rows (isolation) nor a customer's
cross-merchant card (privacy).

## rail = type, not brand
`rail` / `provider_type` is the integration (nmi/stripe/ccbill/solana). A specific account
("mobius" = doujins' NMI account) is a `merchant_external_account` ROW, not a rail value — drop
`mobius` from the rail enum; the rail type is derivable from the account a method/charge binds to.
(Detail in #588.)

## Two orthogonal axes (data-driven, coexist — NOT one mode enum)
1. **Vault ownership** = `payment_methods.stored_in -> merchant_external_account` — the EXACT account
   the token lives in. The rail token is account-specific; it CANNOT be charged via any other account.
   - `owner=platform` (the single shared HyperSwitch vault): card is **customer-global** — any
     merchant may charge it (with consent), Link/Shop-Pay style. No binding table.
     `payment_methods.merchant_id` = NULL.
   - `owner=merchant` (a merchant's own gateway): card is **locked to that one account** — NOT
     transferable across merchants, NOR across provider accounts (even same type / same merchant; a
     Stripe/mobius token only works in the account that vaulted it). `merchant_id` = that account's
     merchant. (Implication: switching a merchant's gateway account = re-collect/re-vault; tokens
     don't migrate.) The denormalized `merchant_id` (NULL for platform) is what drives merchant RLS.
2. **Lifecycle ownership** = `subscriptions.lifecycle_owner (openrails|provider) DEFAULT
   'openrails'`. openrails -> our dunning/scheduler/liveness run (charge the vault); provider ->
   the rail drives it, we mirror webhooks and our jobs `WHERE lifecycle_owner='openrails'` skip it.

Today's doujins NMI/mobius = vault owner **merchant** (doujins' own NMI account "mobius") +
lifecycle owner **openrails** (NMI doesn't auto-rebill, so we drive dunning). The platform vision
= owner=platform + lifecycle=openrails. That two-combo reality is why the axes stay orthogonal.

## Evolvability invariants (what makes it additive)
1. bill by **group id** (opaque), not user_id; no authz/owner/roles in openrails.
2. per-(customer,merchant) facts carry `(customer_id, merchant_id)` (they already do) — no edge table.
3. `payment_methods`: surrogate PK, customer-scoped, identity NOT hardwired to (merchant,vault);
   `stored_in` decoupled so flipping to the platform vault is a re-point, not a rewrite (#588).
4. `subscriptions.lifecycle_owner` + `merchant_external_account.owner` default to today's behavior
   (one-column, additive).
5. charge by (instrument, merchant_external_account), never (merchant, vault_id).

## RLS hardening (refines the RLS section — privacy holes to avoid)
- the `merchant_id = app.merchant_id OR customer_id = app.customer_id` policy is the IDEA, not the
  impl: a merchant session must NEVER read a customer's platform-vaulted cards (they span other
  merchants). So: merchant tenant sessions see ONLY merchant-vaulted rows (owner=merchant, their
  merchant_id); platform-vaulted rows are invisible to ANY merchant session.
- charging a platform-vaulted card for a merchant runs in the PRIVILEGED billing-engine context
  (SECURITY DEFINER / engine role above tenant RLS) that resolves the card server-side — the
  merchant never gets row read access (no setting `app.customer_id` inside a merchant session).
- `app.merchant_id` / `app.customer_id` are set ONLY from the authenticated authkit principal,
  never from request input (else a merchant forges a customer context). RLS ⊆ trusted session vars.
- the `customer` anchor + customer-global payment_methods are customer-context only; a merchant
  learns a `customer_id` solely via its OWN merchant-scoped rows, never by reading the customer table.

## Cross-merchant reuse needs consent (not just a shared vault)
A platform-vaulted card saved at merchant A is NOT automatically chargeable by merchant B. Reuse
requires per-(customer, merchant) **consent** (Link/Shop-Pay style) — also the card-network
stored-credential/MIT requirement (ties to `network_transaction_id`/`mandate_ref`). Model it as a
per-(customer, merchant) consent record (a `merchant_customer_config` flag, or a dedicated row).

## Open questions
1. **Platform vault: shared vs per-merchant — RESOLVED (owner, 2026-06-28).** The platform vault is a
   SINGLE shared account (owner=platform); a card stored there is **customer-global, usable by any
   merchant** (Link/Shop-Pay). A provider-vaulted card (owner=merchant) is **bound to its exact
   `stored_in` account** — not transferable across merchants OR across provider accounts (even same
   type / same merchant), because the rail token is account-specific. `payment_methods.stored_in`
   captures both; no per-merchant platform vault rows. "Merchant bills via platform" = the merchant is
   the beneficiary (`payments.merchant_id`) on a charge against the shared platform vault + consent —
   not a separate vault row.
2. `lifecycle_owner=provider` needs per-rail webhook ingestion to mirror state — real infra, not just
   the column.
3. dropping `rail` from payment_methods depends on universal account binding → doujins #426 first.

## "Additive" honesty
The EARLY phase is genuinely additive (rail=type, rail_method_ref, defaulted lifecycle_owner/owner —
no behavior change). The FULL pivot (payment_methods re-scoped customer-global, customer-context RLS,
portal, consent) is a real migration on a heavily-FK'd table + the RLS model — staged, not a
column-add. Don't undersell it.

## Phasing
- now: #588 instrument restructure + rail=type; bake invariants into doujins #426.
- later (each additive): `customer` anchor + nullable customer-group key; merge provider tables ->
  `merchant_external_account` + `owner`; self-hosted HyperSwitch as an owner=platform account;
  `subscriptions.lifecycle_owner`; read-only consumer portal (cross-merchant read by customer group).

## References
- authkit: principal.go:7-11 (kinds), contract.go:277 (CreatePermissionGroup) / :48 (owner),
  README RBAC (personas/groups/Can).
- openrails: payment_methods, rail_customers, provider_accounts (-> merchant_external_account),
  subscriptions, ledger_accounts, entitlements; jobs_dunning, jobs_subscription_liveness.
- Children: #588 (instrument model), #589 (listing API + health), doujins #426 (account binding).

---

# #590: Auto-register + reconcile the Stripe webhook endpoint (OpenRails owns the endpoint + signing secret)

**Completed:** partial — CORE DONE + LIVE-VALIDATED 2026-06-26. Built the
webhook-endpoint client + reconcile in internal/modules/catalog/stripe_webhooks.go
(`CreateWebhookEndpoint` returns the `whsec_`; `ListWebhookEndpoints`,
`UpdateWebhookEndpoint`, `DeleteWebhookEndpoint`; `ReconcileWebhookEndpoint` =
find-or-create by `openrails_managed` marker, in-place patch of url/events/disabled
with the secret surviving, delete+recreate on api_version drift OR lost secret,
ignores unmanaged endpoints). `enabled_events` single source = `HandledStripeEventTypes`
in internal/modules/webhooks/stripe.go (reconcile takes events as a param to avoid
the webhooks→catalog import cycle). Endpoint pinned to `stripeapi.APIVersion`.

DECISION: register a SNAPSHOT endpoint for the first cut (the mature path the
handler is built around); thin-event Destinations are a follow-up.

VALIDATED AGAINST REAL STRIPE (test account, restricted `rk_test_` key,
`-tags=stripelive`): TestLiveWebhookEndpointReconcile — real create returns a
secret + endpoint pinned to our version + our events; idempotent unchanged; URL
drift patches IN PLACE (same endpoint id, no recreate); events drift patches in
place. TestLiveWebhookDeliveryThroughTunnel — stood up a real cloudflared quick
tunnel, registered a managed endpoint at it, created a real product, and the
`product.created` webhook was ACTUALLY DELIVERED through the tunnel and its
signature VERIFIED with the auto-captured secret via `sigverify.VerifyStripe` (the
same verifier production uses). Both self-clean.

REMAINING (the integration wiring — deliberately deferred; needs deployment-mode-
aware design + dual-mode testing I can't validate solo): persisting the captured
`whsec_` to the RIGHT place is mode-dependent — multi-merchant verification reads
the per-merchant secret store (`MerchantSecretStore.Put(merchantID,
merchants.SecretStripeWebhookSigning, …)`), but standalone/config verification
reads `stripeProc.WebhookSecret` from config Rails. Get this wrong and webhook
verification silently breaks, so it's not safe to blind-wire overnight. Also
remaining: the public-URL config source, the create-at-credential-setup hook, and
the periodic reconcile River job (URL-drift self-heal). The reusable core +
ReconcileWebhookEndpoint is ready for these to call.

Proposed 2026-06-26. Follow-up to #587 (version pin) and #586 (catalog push).
Today the operator manually configures the Stripe webhook endpoint in the
dashboard per merchant: create endpoint → paste the OpenRails URL → pick event
types → set API version → reveal the signing secret → copy `whsec_` into the
merchant's stripe rail config (`WebhookSecret` / `WebhookSecretThin`, read by
`prepareStripeMultiSecret` in internal/http/handlers/webhook.go). Five fiddly,
error-prone steps; the classic silent failures are a wrong/under-selected event
list and a mistyped/stale secret. Make OpenRails do it programmatically.

GOAL: OpenRails registers + keeps-correct its own Stripe webhook endpoint, so the
operator only supplies the Stripe secret key. Closes the #587 OPS ACTION (the
endpoint's `api_version` gets pinned to `stripeapi.APIVersion` from code, no
dashboard step).

VERSION DECISION (owner, 2026-06-26, see #587): the endpoint's `api_version` is the
SAME single hardcoded `stripeapi.APIVersion` const used for outbound — not
per-merchant, not a config field. One value, both directions, bumped only by a
deliberate code change + breaking-change audit.

DESIGN DECISION (confirmed with owner 2026-06-26): OpenRails OWNS the webhook
secret lifecycle — capture the `whsec_` returned on create, store it encrypted in
the merchant's stripe rail secret(s), use it for verification, and re-capture on
any recreate. (The alternative — auto-register URL only, operator still hand-copies
the secret — keeps the two most painful/breakage-prone steps, so rejected.)

Key facts that shape it:
- The signing secret is returned ONLY on the create response (`POST
  /v1/webhook_endpoints`); it can't be fetched later. So create MUST capture +
  persist it, or verification breaks.
- Endpoint identity for find-or-create = a stable `metadata[openrails_managed]=true`
  marker, NOT the URL — the URL is the field that drifts, so it can't be the key.
- COST ASYMMETRY: `url`, `enabled_events`, `disabled` are updatable in place
  (`POST /v1/webhook_endpoints/{id}`) → patch, secret SURVIVES, cheap. `api_version`
  is NOT updatable → a version bump forces delete + recreate → secret ROTATES →
  must re-capture + re-store. So a redeploy/URL change self-heals cheaply; only a
  deliberate version bump is the expensive reconcile.
- Two endpoint flavors exist (snapshot vs thin events, separate secrets:
  `WebhookSecret` / `WebhookSecretThin`). DECIDE: register a thin-event endpoint
  (thin + our pinned hydration in `hydrateThinStripeEvent` = version-robust
  inbound — attractive), a snapshot endpoint, or both. Lean thin-first.

TWO triggers:
1. CREATE at Stripe credential setup (the merchant-adds-secret-key moment, where
   the balance/`/v1/account` check already runs) — first-time registration.
2. PERIODIC RECONCILE as a background River job (sibling to
   jobs_catalog_reconciliation), sweeping merchants with Stripe configured — this
   is what catches later config drift (URL changed on redeploy, events list grew,
   endpoint auto-disabled by Stripe). NOT inline at process boot: multi-merchant
   boot must not fan out network writes across every merchant's Stripe account.
   (Embedded single-merchant hosts like doujins MAY reconcile at startup — one
   merchant, one write.)

Reconcile (desired = our config+code, actual = the registered endpoint):
- missing → create (capture+store secret).
- `url` mismatch → update in place.
- `enabled_events` mismatch → update in place (desired = exactly the types
  `handleEvent` switches on; keep this list in one place so it can't drift).
- `disabled` → re-enable.
- `api_version` mismatch (we bumped the pin) → delete + recreate → re-capture +
  re-store secret. Comment loudly so a future bump can't silently break verify.
- secret not on hand (e.g. DB restore lost it; can't re-fetch) → delete + recreate.

Scope:
- [x] Stripe webhook-endpoints client: Create / List / Update / Delete (through the
      stripeapi choke-point — writes blocked in readonly, version header attached).
- [x] `enabled_events` single source of truth (`HandledStripeEventTypes`, kept next to the `handleEvent` switch).
- [ ] Public webhook URL from config; skip cleanly + log when absent (embedded/local/no public URL). [DEFERRED — wiring]
- [ ] Persist the returned `whsec_` to the mode-correct store (per-merchant secret store vs config Rails); wire to what `prepareStripeMultiSecret` reads. [DEFERRED — mode-aware]
- [ ] Create-at-credential-setup hook (idempotent find-or-create by `openrails_managed` marker). [DEFERRED — wiring]
- [ ] Periodic reconcile River job (drift-fix per the rules above), best-effort, never blocks boot. [DEFERRED — wiring]
- [x] Decided: SNAPSHOT endpoint for first cut (thin Event Destinations = follow-up).
- [x] Tests: mock unit tests (idempotency, url/events in-place patch, version recreate, lost-secret recreate, ignore-unmanaged) + LIVE create/reconcile/delete + LIVE delivery-through-tunnel with signature verify.

---

# #584: Migration baseline 001 self-creates the `openrails` schema

**Completed:** no

Proposed 2026-06-25 (doujins embedded-migration review).
The squashed `migrations/postgres/001_schema.up.sql` baseline is fully
schema-qualified (`openrails.*`) but never runs `CREATE SCHEMA openrails`, so the
migration FS is NOT self-contained: any consumer applying it via migratekit must
pre-create the schema first. openrails' own standalone migrator already does
`CREATE SCHEMA IF NOT EXISTS` (internal/migrate/migrator.go), but the embedded
FS-driven path (doujins) bypasses that, forcing doujins to hand-maintain a
`CREATE SCHEMA IF NOT EXISTS openrails` pre-step. Make the FS own its schema.

- [x] Prepend `CREATE SCHEMA IF NOT EXISTS openrails;` to 001 (before the first
      `openrails.`-qualified object), idempotent so already-migrated DBs skip it.
- [x] Confirm migration tests still pass (schema pre-create becomes redundant, not conflicting).
- [x] Tag + release (v0.65.1); doujins drops `openrails` from its host-side ensureBaseSchemas list.

---

# #330: nmi-immediate-subscription-checkout-stuck-pending

**Completed:** no
**Status:** IN_PROGRESS 2026-06-08: immediate NMI checkout activation patch is implemented; package tests and focused mock-provider regression pass; configured-account NMI recurring test has been added and compiles/skips locally because `NMI_SANDBOX_SECURITY_KEY` is not set. Remaining work: run real NMI credential test and repair/replay Paul2 after fixed code is deployed.

Fix the NMI subscription checkout path where an immediately approved recurring checkout is persisted as pending, leaving host apps such as Doujins stuck on the pending-subscription screen even though NMI accepted the transaction.

## Metadata

- Category: bug
- Status: in_progress
- Passes: false

## Live findings from Doujins/Paul2 on 2026-06-08

- OpenRails received the checkout request and `POST /v1/self/checkout` returned HTTP 200.
- NMI accepted the card vault and transaction: payment vault `314825442`, provider transaction `12162933364`, provider subscription `12162933429`.
- Local checkout session `019ea96b-221b-7274-a341-8cdc85cb72d6` was marked `succeeded` with processor `mobius` and amount `2300 usd`.
- Local subscription `019ea96b-223b-75b1-926b-35956d10eba5` stayed `pending`, with no current period start/end timestamps.
- Local payments only had a pending attempt row keyed as `nmi_sub_attempt:sub_cb204b034d2c3ec46be93b0470ff44df`; there was no completed payment row keyed by the real NMI transaction id.
- No billing entitlement rows were created for the user, so Doujins correctly kept showing the pending-subscription state.

## Root cause

`processNMISubscription` called `AddRecurringSubscription` successfully, but `completeNMISubscriptionRegistration` intentionally created a local pending subscription and returned a pending checkout response. The initial NMI response was not used to synchronously activate an immediate subscription, set the current billing period, create a completed payment, or grant entitlements. Webhooks should not be required for the initial happy path when NMI immediately approves the transaction; delayed/future starts can remain pending.

## Desired behavior

For immediate NMI subscription approvals, OpenRails should finish the local checkout atomically from the direct provider response: mark the subscription active, set current period timestamps, record the completed payment against the real provider transaction id, grant entitlements, and return a succeeded checkout response. Delayed-start subscriptions and genuinely asynchronous provider states should remain pending and rely on follow-up provider events/reconciliation.

**Tasks:**
- [x] Capture live-stack evidence for the Paul2 failure: NMI accepted the transaction, checkout succeeded, subscription stayed pending, payment stayed as a pending attempt, and no entitlement was granted.
- [x] Identify root cause in the NMI checkout finalization path.
- [x] Patch immediate NMI subscription finalization to activate the subscription, set period dates, create the completed payment, and grant entitlements without waiting for a webhook.
- [x] Preserve pending behavior for delayed/future-start NMI subscriptions.
- [x] Fix stale integration-test helpers that still query billing tables by `user_id` instead of `tenant_subject_id`.
- [x] Add/validate focused integration coverage proving an immediate NMI subscription checkout grants the premium entitlement synchronously.
- [x] Add an actual NMI test-account integration path so the live provider contract is exercised, guarded by NMI test credentials.
- [ ] Run the actual NMI configured-account recurring-subscription integration with `NMI_SANDBOX_SECURITY_KEY` set.
- [x] Run focused OpenRails checkout/module tests and NMI regression tests.
- [ ] After deployment/restart, repair or replay the affected live pending subscription row for Paul2 if it still exists.

---

# #328: robinhood-coinbase-usdc-funding-sessions

**Completed:** no
**Status:** PARTIAL 2026-06-08: Implemented Solana-only USDC funding session APIs, persistence, config, Coinbase hosted-session adapter with CDP JWT auth, Coinbase Hook0-signed Onramp webhook/status ingestion, Robinhood launch-template handoff, provider eligibility gates, self-service routes, idempotency, structured insufficient-USDC funding context on checkout errors, backend Solana USDC balance verification, focused tests, and DB-backed self-service API tests for create/get, merchant/user isolation, idempotency, and unsupported provider/network rejection. Retained for future provider integration work; current Doujins UX uses manual Robinhood/Coinbase links plus connected-wallet balance checks instead of OpenRails provider sessions. Remaining: real Robinhood partner adapter/status docs and access.

Plan and implement OpenRails-owned USDC funding sessions for host apps that need users to buy or transfer USDC into their own live Solana wallet before completing a Solana wallet checkout.

## Metadata

- Category: feature
- Status: partial
- Passes: false

## Goal

- OpenRails should expose a provider-backed funding-session API for Robinhood and Coinbase only. Host apps such as Doujins can ask OpenRails for a funding URL, send the user to the provider in a new tab or popup, then resume checkout after OpenRails and/or the host app verifies that the user's live Solana wallet has enough USDC.

## Product Behavior

- The user already has or creates a Robinhood/Coinbase account on the provider site.
- OpenRails does not custody funds and does not collect provider KYC; the provider handles account login, payment method, buy/transfer, KYC, and compliance.
- Provider redirect/return means the provider flow ended; it is not proof that the wallet is funded.
- Completion must be based on provider status/webhooks when available plus Solana wallet-balance verification.
- Only offer a provider when it can fund USDC on Solana. Coinbase/Base and all EVM chains are out of scope.

## Scope

- Implement Robinhood and Coinbase integration surfaces only for Solana USDC.
- Do not implement Ramp, Transak, MoonPay, PayPal, Venmo, Base, Ethereum, Polygon, Arbitrum, Optimism, or bridge paths for this issue.
- Keep provider abstraction narrow but extensible enough that more providers could be added later without changing the host-app contract.

**Tasks:**
- DESIGN:
- [x] Define the OpenRails funding-session contract for browser self-service callers: provider preference, wallet address, asset, network, requested amount, checkout_session_id, return_url, and idempotency key.
- [x] Define provider statuses and normalize them into OpenRails statuses such as created, opened, pending_provider, pending_settlement, funded, failed, expired, and cancelled.
- [x] Define Solana-only compatibility rules for USDC funding.
- [x] Decide whether funding amount comes from the checkout-session shortfall, an explicit requested amount, or both with server-side validation. Implemented explicit requested amount with optional checkout_session_id context.
- [x] Decide how provider ranking is configured per merchant: Robinhood preferred, Coinbase fallback. Implemented default provider order with Robinhood first and Coinbase second.
-
- DATA MODEL:
- [x] Add a funding/onramp session table with tenant_id, user_id, checkout_session_id, provider, wallet_address, asset, network, requested_amount, provider_session_id, provider_url, status, return_url, idempotency key, timestamps, and provider metadata.
- [x] Add indexes for tenant/user lookup, checkout_session_id lookup, provider_session_id lookup, and idempotency.
- [x] Store provider secrets/config in OpenRails config, never in host apps.
-
- API:
- [x] Add `POST /v1/self/usdc-funding-sessions` to create a Robinhood/Coinbase funding session for the authenticated browser user.
- [x] Add `GET /v1/self/usdc-funding-sessions/:id` to return normalized funding status and provider URL/status details safe for frontend polling.
- [x] Add `GET /v1/self/usdc-funding-options` to list eligible Robinhood/Coinbase options for wallet, network, asset, amount, and optional checkout_session_id.
- [x] Add provider webhook/status callback endpoints where Coinbase supports them. Implemented signed Coinbase Onramp webhook ingestion on the existing provider webhook route; Robinhood remains blocked on partner docs/access.
- [x] Enforce self-service auth, merchant boundaries, and idempotency on funding-session create/read routes.
-
- PROVIDERS:
- [x] Implement a Coinbase provider adapter that creates a hosted onramp URL/session with destination wallet, network, asset, amount, return URL, and partner/user reference, including short-lived CDP JWT bearer generation from Coinbase secret API keys.
- [x] Implement Coinbase status/webhook handling and map provider lifecycle into OpenRails funding-session status. Coinbase success maps to pending_settlement; only live Solana wallet-balance verification can mark funded.
- [ ] Implement a Robinhood provider adapter after partner docs/access are available, supporting external handoff into Robinhood Connect and funding into the user's live wallet.
- [ ] Implement Robinhood status/webhook handling if exposed by partner API; otherwise rely on return handling plus on-chain wallet-balance verification.
- [x] Add provider availability checks so unsupported network/asset combinations are hidden rather than offered.
-
- WALLET VERIFICATION:
- [x] Reuse existing Solana USDC balance-checking code where possible to verify the funded wallet before marking a session funded for Solana checkout.
- [x] Do not add Base/EVM balance verification for this issue; Solana is the only supported chain.
- [x] Ensure returning from a provider only triggers polling/checking; it must not mark the session funded by itself.
-
- CHECKOUT INTEGRATION:
- [x] Allow a funding session to reference the checkout session that produced an insufficient-USDC state.
- [x] Ensure insufficient-USDC API errors expose enough structured amount/network/wallet context for host apps to create a funding session. Added `error.metadata.usdc_funding` with Solana network, USDC asset, wallet, decimal amount/balance/shortfall, and base-unit values.
- [x] Keep final subscription/payment creation in the existing checkout confirmation path after the wallet is funded.
-
- VERIFY:
- [x] Add unit tests for provider eligibility and network compatibility gates.
- [x] Add API tests for create/get funding session, merchant isolation, idempotency, and unsupported-provider/network rejection.
- [x] Add provider adapter tests with mocked Coinbase responses.
- [x] Add wallet-balance verification tests proving redirect alone is insufficient through status semantics and frontend polling contract.
- [x] Document the host-app integration contract for Doujins in config.example.yaml and the tracker issue.

---

# #111: admin-rate-limiting

**Completed:** no

Add rate limiting to admin endpoints to limit blast radius of compromised JWT

## Metadata

- Category: security
- Status: not_started
- Passes: false

**Tasks:**
- STEPS:
- [ ] PROBLEM: If admin JWT is leaked, attacker could cancel/refund thousands of users before detection
- [ ] Implement per-admin-user rate limiting using Redis (keyed by admin user ID from JWT)
- [ ] Define rate limits for destructive operations:
-     - Cancellations: 5/minute, 10/hour, 50/day
-     - Refunds: 5/minute, 10/hour, 50/day
-     - Entitlement revocations: 5/minute, 10/hour, 50/day
- [ ] Define rate limits for bulk/expensive operations:
-     - Extend operations: 3/minute
-     - Off-channel payments: 10/minute
-     - Admin grants: 10/minute
- [ ] On rate limit exceeded: lock out admin for extended period (e.g., 1 hour) and alert
- [ ] Add alerting/notification when rate limits are approached (e.g., 80% threshold)
- [ ] Create admin rate limit middleware that wraps destructive endpoints
- [ ] Allow super-admin or manual override to unlock rate-limited admin if legitimate
- [ ] Log all rate limit events for security audit
- BENEFIT: Limits blast radius - attacker can only affect ~5-10 users before getting locked out

---

# #126: test-architecture-improvements

**Completed:** no

Tests should use real structs and functions from production code, not invent test-specific abstractions

## Metadata

- Category: testing
- Status: not_started
- Passes: false

## Details

- current_problems: ["SubscriptionOptions helper struct uses different field names than models.Subscription (PeriodStart vs CurrentPeriodStartsAt)","Tests can pass while using wrong field names because helpers translate between them","When a model field is renamed/removed, tests may not break because helper abstracts it away","Developers get confused about which struct to use and what fields are available"]
- example_bad: {"code":"suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{PeriodStart: now})","problem":"SubscriptionOptions.PeriodStart doesn't exist on models.Subscription"}
- example_good: {"code":"sub := &models.Subscription{CurrentPeriodStartsAt: now, ...}; suite.CreateTestSubscription(sub)","benefit":"Uses real model, compiler catches field name errors"}
- philosophy: {"principle":"Tests should verify actual production code behavior, not test-specific wrappers","goal":"When production code changes, tests should break if they're testing the affected behavior","anti_pattern":"Test helper structs that diverge from production models hide API changes and create maintenance burden"}
- recommendations: ["Use models.Subscription directly in tests instead of SubscriptionOptions","Use api.ListResponse[T] for parsing API responses instead of map[string]interface{}","Import and use the actual request/response structs from handlers","If a helper is needed, it should take the real model struct as input, not a parallel struct","Test assertions should use strongly-typed response parsing"]

**Tasks:**
- REFACTORING:
- [ ] Audit all test helper structs (SubscriptionOptions, PaymentMethodOptions, etc.)
- [ ] Replace helper structs with direct model usage where possible
- [ ] Update CreateTestSubscriptionWithOptions to accept *models.Subscription
- [ ] Use json.Unmarshal with actual API response types instead of map[string]interface{}
- [ ] Add linter rule or CI check to discourage map[string]interface{} in tests

---

# #158: ccbill-dunning-grace-entitlements

**Completed:** no

Adjust CCBill subscription entitlement logic to model paid term + retries (dunning) + end-of-term expiration. Use CCBill webhook fields like nextRetryDate/nextRenewalDate to drive subscription status and finite entitlement windows, and cap any grace extensions to avoid unlimited free access.

**Tasks:**
- SPEC:
- [ ] Verify CCBill webhook sequence in sandbox: cancel mid-term -> Expiration at end-of-term (and timing)
- [ ] Decide grace policy during retries: disabled vs enabled; cap strategy (extend-to-nextRetryDate vs fixed extra days)
- [ ] Decide policy for CCBill `Cancellation.source=failedRB`: treat as terminal end (no more retries) vs still paid-through end-of-term
-
- DATA MODEL:
- [x] Ensure we can store current_period_ends_at (from nextRenewalDate) for CCBill subscriptions
- [x] Store next_retry_at (from nextRetryDate) and optional grace_ends_at (policy derived)
- [x] Decide whether to reuse existing `subscriptions.next_retry_at` fields for CCBill (recommended) vs add CCBill-specific columns
-
- WEBHOOK -> STATE MACHINE:
- [x] NewSaleSuccess: set paid-term end; grant entitlements for [now, paid_term_end)
- [x] RenewalSuccess: extend paid-term end; extend entitlement windows
- [x] RenewalFailure: set past_due/dunning; persist next_retry_at from webhook; optionally extend entitlements within grace cap
- [x] Cancellation: mark cancelled but keep access until paid_term_end (do NOT revoke immediately)
- [x] Expiration: mark ended/expired and end/revoke entitlements
-
- CODE CHANGES (expected touch points):
- [x] `internal/services/webhook_ccbill.go`: parse and apply `nextRenewalDate` on `NewSaleSuccess` and `RenewalSuccess` (don’t rely solely on `price.billing_cycle_days`)
- [x] `internal/services/webhook_ccbill.go`: on `RenewalFailure`, parse `nextRetryDate` and update `subscriptions.status=past_due` + `subscriptions.next_retry_at` (avoid calling `SubscriptionLifecycleService.FailMembership`, which schedules NMI-style retries)
- [x] `internal/services/webhook_ccbill.go`: on `Cancellation`, call `CancelMembership` with `RevokeAccess=false` (today it passes true)
- [x] `internal/services/lifecycle_service.go`: update `CreateMembership` entitlement grant behavior for CCBill to create finite windows ending at `current_period_ends_at` (instead of indefinite `end_at=NULL`)
- [x] `internal/services/lifecycle_service.go`: update `RenewMembership` for CCBill to extend entitlements to match the new `current_period_ends_at` (today renewal doesn’t extend entitlements at all unless a downgrade changes entitlement set)
- [x] `internal/db/repo/entitlement.go`: make `IsEntitled` / “active entitlement” queries explicitly exclude `deleted_at` (don’t rely on implicit soft-delete behavior)
- [x] `config/config.go`: add a feature flag / config for CCBill grace behavior (enable/disable + max cap)
-
- ANALYTICS:
- [x] Ensure ClickHouse subscription_events reflect dunning/cancelled/expired transitions for CCBill
-
- TESTS:
- [x] Add integration test: NewSaleSuccess -> RenewalFailure(nextRetryDate) keeps access until paid_term_end (+ optional grace) -> RenewalSuccess extends window
- [x] Add integration test: Cancellation mid-term does NOT revoke immediately; entitlement ends at paid_term_end; verify Expiration handling if CCBill sends it
- [x] Add unit test for `EntitlementRepo.IsEntitled` to ensure deleted rows are excluded (regression guard)

---

# #164: cloudflared-managed-dev-tunnel

**Completed:** no

Make `Config.Cloudflared` an actual supported dev feature: OpenRails can (optionally) run and manage a Cloudflare Tunnel for local/dev, so a full local stack (e.g., multiple host apps + billing) can be accessed from an external device (phone) over HTTPS.

## Metadata

- Category: devx
- Status: planned
- Passes: false

## Goal

- With `cloudflared.tunnel_token` configured, a developer can start OpenRails and get a stable public hostname that routes to the local billing instance, usable from a phone browser/app.

## Non-goals

- Do not require Cloudflared in production.
- Do not make Cloudflared a hard dependency for boot.

## Notes

- This is for local/dev convenience (public ingress), not webhook signature bypass.
- Prefer explicit opt-in (config flag or separate command) so production never spawns subprocesses unexpectedly.

**Tasks:**
- DESIGN:
- [ ] Decide opt-in mechanism: (A) standalone CLI subcommand `openrails cloudflared up` that supervises both OpenRails + cloudflared, or (B) OpenRails spawns cloudflared when `cloudflared.tunnel_token` is set and `env=dev`
- [ ] Decide what constitutes “success”: tunnel established + /health/ready reachable via public hostname
-
- IMPLEMENTATION (if spawning subprocess):
- [ ] Add a small `pkg/cloudflared` supervisor that starts `cloudflared tunnel run` with the configured token and captures stdout/stderr into structured logs
- [ ] Ensure clean shutdown: propagate SIGINT/SIGTERM, kill child process group, avoid zombies
- [ ] Add a readiness check that probes the configured `cloudflared.public_hostname` (or local route) and reports status
-
- CONFIG / DOCS:
- [ ] Clarify config semantics in config.example.yaml (token is secret; hostname is non-secret; tunnel name optional)
- [ ] Document a phone-access workflow in docs (prereqs: cloudflared installed or image available; set APIURL/CORS origins appropriately)
-
- SECURITY / SAFETY:
- [ ] Ensure the service API (port 8060) is not accidentally exposed publicly by default; only expose the public API unless explicitly configured
- [ ] Ensure `api_key` and auth verification are still required for protected routes
-
- VERIFY:
- [ ] Add a lightweight unit test for supervisor command construction (no subprocess exec)
- [ ] Manual dev verification: start stack + tunnel and hit /health/ready from an external device

---

# #288: processor-routing-and-fallback-policy

**Completed:** no

Add deterministic processor selection and fallback policy so checkout can choose the best available rail for a merchant/product/tier/user context before creating a one-processor checkout session.

## Metadata

- Category: feature
- Status: planned
- Passes: false

## Motivation

- Hyperswitch wins on payment orchestration. OpenRails should not chase broad smart-routing, but customers do need basic processor redundancy and product/merchant-aware routing, especially high-risk merchants with Stripe plus NMI/CCBill/Solana.
- Keep checkout sessions one-processor-at-a-time; route before session creation.

## Non-goals

- No ML routing.
- No 200-connector orchestration platform.
- No automatic retry to a second processor after a successful authorization attempt unless the processor contract makes that safe.

**Tasks:**
- DESIGN:
- [ ] Define routing inputs: tenant_id, product_id, price_id, tier_group, amount, currency, billing cycle, user country/state when known, processor availability, processor capability metadata (#291), and explicit client preference.
- [ ] Define routing outputs: selected processor, fallback candidates, reason, and policy version.
- [ ] Decide precedence: explicit price/provider config > merchant policy > product/tier_group policy > global default.
- [ ] Decide failure classes that can trigger fallback before checkout finalization: processor unavailable, unsupported capability, credential missing, sandbox/live mismatch, hard validation failure. Do not fallback after a successful charge.
-
- DATA MODEL / CONFIG:
- [ ] Add routing policy representation in DB or catalog manifest: allowed processors, preferred order, disabled processors, and optional per-tier overrides.
- [ ] Extend catalog-as-code manifest only if needed; prefer using existing provider lists as the first version.
- [ ] Record selected processor and routing reason on checkout_sessions for auditability.
-
- IMPLEMENTATION:
- [ ] Add a ProcessorRouter service used before checkout session creation and legacy checkout flows.
- [ ] Integrate with processor health/capability metadata so unsupported processors are filtered with explicit errors.
- [ ] Add dry-run endpoint/CLI to explain routing for a price/user/merchant without creating a checkout.
- [ ] Ensure idempotency keys bind to the resolved processor/policy so retries do not unexpectedly switch rails.
-
- VERIFY:
- [ ] Unit-test policy precedence and fallback filtering.
- [ ] Integration-test Stripe primary -> NMI fallback when Stripe is disabled/unavailable before charge.
- [ ] Integration-test product constrained to NMI does not route to Stripe even if NMI is configured.

---

# #290: provider-certification-matrix

**Completed:** no

Publish and maintain a provider certification matrix for Stripe, NMI, CCBill, and Solana that records exactly which customer-visible flows are supported, sandbox/devnet tested, live/test-mode tested, and known-limited.

## Metadata

- Category: product
- Status: planned
- Passes: false

## Motivation

- OpenRails' strongest differentiator is deep support for real non-Stripe rails, especially NMI-compatible high-risk gateways. Customers need confidence that the specific flows they care about actually work.
- This should become both documentation and an executable certification harness where practical.

**Tasks:**
- MATRIX DESIGN:
- [ ] Define provider capabilities and certification statuses: not_supported, manual_only, unit_tested, integration_tested, sandbox_certified, live_test_mode_certified, devnet_certified, production_certified.
- [ ] Define flows to track: catalog/product push, price/recurring-plan push, one-time checkout, recurring checkout, vault/tokenization, rebill, cancellation, deferred cancellation, refund, dispute/chargeback, webhook handling, subscription sync/backfill, catalog drift detection.
- [ ] Include processor-specific notes: NMI product/prices are local while recurring prices push as NMI recurring plans; CCBill catalog actions may be manual; Solana recurring requires on-chain readback/devnet certification.
-
- DOCS:
- [ ] Add docs/providers.md or equivalent with the current matrix and exact tested commands.
- [ ] Add NMI-compatible gateway guidance: required security_key, Collect.js/tokenization key if needed, direct/query endpoints, test_mode behavior, and how white-label NMI accounts map to the same interface.
- [ ] Add troubleshooting for common provider failures: bad endpoint, key belongs to different gateway user, sandbox URL not supported, recurring plan query mismatch, webhook signature failure.
-
- EXECUTABLE CERTIFICATION:
- [ ] Add or formalize focused integration tests for NMI sale, vault, recurring plan create/readback, and query API.
- [ ] Add Stripe test-mode catalog command + subscription sync certification steps.
- [ ] Add Solana devnet read-back certification for plan accounts and recurring lifecycle.
- [ ] Add CCBill manual-action verification path so unsupported remote catalog operations surface as pending_manual_actions rather than errors.
-
- PROCESS:
- [ ] Make certification matrix updates part of provider-related PRs.
- [ ] Record last verified date, environment, and command for each provider flow without exposing secrets.

---

# #291: processor-capability-metadata

**Completed:** no

Expose processor capability metadata in code, APIs, catalog planning, checkout validation, and admin/provider status so OpenRails can explain what each rail supports instead of relying on scattered processor-specific conditionals.

## Metadata

- Category: architecture
- Status: planned
- Passes: false

## Motivation

- Stripe, NMI, CCBill, and Solana have different lifecycle semantics. Customers need predictable errors and routing decisions. OpenRails also needs a shared capability source for routing/fallback, catalog-as-code, checkout validation, and the provider certification matrix.

**Tasks:**
- CAPABILITY MODEL:
- [ ] Define ProcessorCapabilities with booleans/enums for recurring, one_time, vault/tokenization, hosted checkout, redirect checkout, direct sale, catalog push, recurring plan push, refund, dispute, cancel immediate, cancel deferred, remote subscription listing, remote dedup check, webhooks, drift enumeration, and manual actions.
- [ ] Add capability details for current processors: stripe, NMI-backed (`mobius`), ccbill, solana.
- [ ] Distinguish processor class capabilities (NMI-backed) from named provider overrides (Mobius).
-
- INTEGRATION POINTS:
- [ ] Use capabilities in checkout validation instead of hard-coded processor switches where practical.
- [ ] Use capabilities in catalog planning/reconciliation to decide provider actions and pending_manual_actions.
- [ ] Use capabilities in routing/fallback policy (#288) so unsupported rails are filtered before checkout.
- [ ] Surface capabilities through admin/provider status endpoints and docs.
-
- ERRORS / UX:
- [ ] Return structured unsupported-capability errors with processor, capability, and suggested alternative when possible.
- [ ] Ensure user-facing errors are clean while admin/debug surfaces retain processor detail.
-
- VERIFY:
- [ ] Unit-test capability metadata for each supported processor.
- [ ] Regression-test known special cases: CCBill manual catalog actions, NMI recurring plan push, Stripe remote dedup check, Solana one-off/recurring distinction.

---

# #320: Add Hyperswitch payment vault support for cloud and self-hosted deployments

**Completed:** no

Add Hyperswitch as an optional OpenRails payment provider and payment-method vault integration, covering both Hyperswitch Cloud and self-hosted Hyperswitch. This is payment vaulting/tokenization, not HashiCorp Vault merchant-secret storage. OpenRails should store only opaque Hyperswitch customer/payment-method identifiers plus non-sensitive metadata, while PAN/card collection stays in Hyperswitch-hosted/client-side tokenization flows or equivalent PCI-scoped Hyperswitch surfaces.

This issue must reconcile with future issue #297 (`deplatforming-resilient-card-vault`): Hyperswitch should not be positioned as the default adult/high-risk deplatforming-resilient vault until its contractual/export/compliance posture is explicitly certified. Hyperswitch Cloud may still be useful for lower-risk deployments or as an optional provider, and self-hosted Hyperswitch may be a break-glass/advanced deployment path if the operator accepts and satisfies the PCI requirements.

This should integrate with the provider capability and routing work in #288, #290, and #291: Hyperswitch can be selected as a processor/vault provider when the merchant/provider config says it is available, and OpenRails can route checkout/setup flows through it without treating it as a generic secret vault.

**Tasks:**
- [ ] Research the current Hyperswitch Cloud and self-hosted API surfaces needed for customers, payment methods, payment/setup intents, saved payment methods, refunds/voids, webhooks, connector routing, and vault/tokenization modes; record any version assumptions in docs.
- [ ] Reconcile the implementation plan with future issue #297: document whether Hyperswitch is an optional provider, a lower-risk deployment choice, or a PCI-heavy break-glass/self-hosted vault path; do not make it the default portable adult/high-risk vault without explicit certification.
- [ ] Define the OpenRails provider config shape for `hyperswitch`: cloud vs self-hosted mode, API base URL, optional vault/base URL split if Hyperswitch requires it, merchant/profile/account identifiers, API key secret reference, webhook secret reference, return/callback URLs, and test/live mode.
- [ ] Store Hyperswitch credentials in the existing merchant secret store path; do not store processor API keys in bootstrap YAML, database rows, logs, or generated frontend config.
- [ ] Extend provider capability metadata (#291) so Hyperswitch can advertise supported flows: payment-method vault/tokenization, one-time checkout, recurring/setup/mandate behavior if supported, refunds, webhooks, processor-side routing, remote payment-method listing, and manual certification status.
- [ ] Add a provider adapter/client abstraction that lets NMI customer-vault IDs and Hyperswitch payment-method IDs fit the same OpenRails payment-method model without hard-coding NMI semantics into checkout/subscription flows.
- [ ] Implement Hyperswitch customer/payment-method setup flow using an opaque token or hosted/client-side Hyperswitch collection result; persist only customer, provider, external customer ID, external payment method ID, brand/last4/expiry metadata, and status.
- [ ] Implement checkout/subscription charge paths that can use a saved Hyperswitch payment method and reconcile the resulting payment/subscription state back into OpenRails records.
- [ ] Implement webhook verification and event handling for successful payment, failed payment, refund/void, payment-method updates/deletes, and any recurring/mandate lifecycle events OpenRails relies on.
- [ ] Add self-hosted operational docs: required Hyperswitch services, base URL/TLS requirements, webhook reachability, secret injection, health checks, and local compose/dev smoke path if practical.
- [ ] Add cloud operational docs: required Hyperswitch Cloud credentials, webhook setup, provider certification checklist entry (#290), and merchant bootstrap/provider-link examples.
- [ ] Add tests with a mocked Hyperswitch server/client for tokenization/setup, saved-method charge, failure mapping, webhook signature verification, idempotency, merchant isolation, and no-sensitive-card-data persistence/logging.
- [ ] Validate with focused Go tests for provider/checkout/vault/webhook code, compile-only full package coverage, `task build`, and an optional live sandbox/self-hosted smoke test when credentials or local Hyperswitch are available.

# #333: admin-e2e-suite-needs-control-plane-wiring

**Completed:** no
**Status:** OPEN 2026-06-10 (Claude): diagnosed during the #332 failure review; not started. All other e2e failures from that review are fixed (see completed #332 + commits 9fe72d4 openrails, 68cb6a1 tensorhub, b72f61e8 cozy-art).

The admin e2e tests (tests/admin_payments_test.go, admin_metrics_test.go, admin_entitlements_source_test.go, admin_offchannel_payments_test.go) fail with 500 {'message':'authorization unavailable'} — the nil-admin-checker fail-closed path. DIAGNOSIS (2026-06-10): pre-existing #312 bit-rot, documented in the suite itself (testcontainer_suite.go Auth config comment: 'Admin-route integration tests therefore require the embedded control plane wired with the test admin granted openrails:admin'). The #312 hard-cut moved admin authority from JWT claims (tests still mint operatorAdminClaims() tokens via test_helpers.go) to the LIVE control-plane permission check (routes_admin.go only sets AdminPermissionChecker when s.controlPlane != nil; the suite never enables Auth.ControlPlane -> checker nil -> admin_neutral.go/ginmw admin gates 500 fail-closed).

WHAT THE FIX NEEDS:
1. suite.Config.Auth.ControlPlane = &config.ControlPlaneConfig{Enabled: true} (ginboot embcp.Attach then builds it; authkit profiles schema is already applied by migrate.RunPostgres).
2. The admin test users must EXIST in authkit profiles.users with ids equal to the JWT subs the suite mints, then be made members + granted the admin role (controlplane Bootstrap/AddMember/AssignRole or authkit core APIs — NOT raw SQL into roles). authkit core.CreateUser(email, username) generates its own id, so either (a) resolve the verifier's user mapping (issuer+sub -> user) and mint tokens for created users' real ids, or (b) add a test-only authkit user-provisioning hook.
3. operatorAdminClaims()/CreateAdminToken in tests/test_helpers.go are then vestigial — admin authority comes from the role grant, not claims.

NOTE: the admin-METRICS tests' 'Table test_analytics.daily_metrics does not exist' failures were a STALE TEST ENV artifact (migratekit records the ClickHouse ledger in postgres; reusing a postgres DB across runs against fresh CH containers skips re-apply). On a fresh env CH applies fine and those tests fail at the same admin-auth gate instead. Squashed CH baseline (cb9200b) is NOT at fault (daily_metrics present; migrations/clickhouse schema_test passes).

**Tasks:**
- [ ] Enable Auth.ControlPlane in testcontainer_suite config and verify embcp.Attach builds it in ginboot.
- [ ] Provision admin test users in authkit profiles.users matching the JWT subs (or mint tokens from created users' ids); AddMember + AssignRole openrails:admin via control plane / authkit core APIs.
- [ ] Re-run tests/admin_*.go on a fresh env; remove vestigial operatorAdminClaims if green.
- [ ] (env hygiene) consider failing loudly or re-validating when the migratekit CH ledger says applied but the CH database lacks the tables (stale-ledger detection).

# #582: rename `processor` → `rail` across notes + codebase (terminology consolidation; breaking DB + API allowed)

**Completed:** no

Decided 2026-06-24 (Paul + Claude). On-brand consolidation: **"rail" becomes THE term for a payment channel/lane.** `processor` and `rail` are synonyms — the `models.Processor` enum values (`mobius`/`ccbill`/`stripe`/`solana`/…) literally ARE the rails. `provider` is a **different** concept (a credentialed *account on* a rail) and **stays**. Breaking changes to DB + API are authorized now (pre-production; **no migrations** — edit the schema in place and `task sqlc`). Behavior does not change; this is cosmetic/brand only.

## Decision / scope boundary (read first)
- **RENAME:** everything meaning the payment lane/type currently called `processor` → `rail`.
- **KEEP `provider`** (account-level concept): `provider_accounts`, `provider_account_id` (71-use FK), `provider_intents`, `provider_refresh_watermarks`, `external_provider_mutation_logs`. A rail is the lane; a provider account is an account on the lane (the schema already says "multiple provider accounts per provider rail").
- **KEEP wire/string VALUES:** `"mobius"`, `"ccbill"`, `"stripe"`, `"solana"`, `"paypal"`, `"admin"`, `"manual"` are rail *names*, not the word "processor" — they don't change.
- **No blanket-sed:** rename ONLY the payment-rail sense of "processor". Audit for any unrelated "processor" (generic data/event/webhook processors) and leave those.

## Target naming

**Go**
- `internal/db/models/processor.go` → `rail.go`; `type Processor string` → `type Rail string`; `ProcessorMobius/CCBill/Solana/Stripe/Paypal/Admin/Manual` → `RailMobius/...` (values unchanged).
- `internal/db/models/processor_customer.go` → `rail_customer.go`; `ProcessorCustomer` → `RailCustomer`.
- `internal/modules/payments/processors/` package → `internal/modules/payments/rails/`; `IsNMIBackedProcessor`→`IsNMIBackedRail`, `NMIBackedProcessors`→`NMIBackedRails`, `SameProcessor`→`SameRail`.
- reconcile `ProcessorFetcher` → `RailFetcher`; webhooks `WebhookHandler.Processor()` → `.Rail()`; intents handler `processor` refs; all fields/params/locals named `processor` → `rail`.
- `pkg/` exported API (`pkg/service`, `pkg/embedded`, `pkg/api`): exported `Processor` → `Rail` (**BREAKING** for embedded consumers).

**DB (breaking, no migration — edit `migrations/postgres/*.sql` in place)**
- column `processor` → `rail` everywhere (payments, subscriptions, etc.); the named enum type (if a `processor` enum type exists) → `rail`; table `processor_customers` → `rail_customers`.
- `internal/db/queries/*.sql`: update column refs; `task sqlc` to regenerate `internal/db/gen` + vet against the real schema.

**JSON / HTTP / CLI**
- request/response fields named `processor` → `rail` (**BREAKING** API change — audit `pkg/api`, handlers, `docs/api/endpoints.md`).
- CLI: `pull-provider --provider=nmi,ccbill,stripe,solana` currently takes **rail** names → this is the provider/rail overload below; resolve there.

**Docs/notes**
- README.md, docs/*.md, agents/*.md: "processor" (payment sense) → "rail".
- Add a billing glossary: **rail** = the lane/type (former `Processor`); **provider account** = a credentialed account on a rail; **integration** = the client code under `internal/integrations/`.

## Provider-overload cleanup (decide as part of this)
Today "provider" is overloaded: it means the ACCOUNT (`provider_accounts`) AND sometimes the TYPE (the `provider` column in `provider_intents`/`provider_refresh_watermarks` holds `"stripe"`/`"nmi"`; CLI `--provider=nmi,…`). Renaming processor→rail makes this glaring. Options:
- **(A) Minimal:** rename only `processor`→`rail`; leave every `provider` as-is (accept that some `provider` columns/flags hold a rail value). Lowest churn.
- **(B) Disambiguate (recommended):** also rename the *type-holding* `provider` columns/flags → `rail` (e.g. `provider_intents.provider`→`.rail`, `--provider`→`--rail`), while KEEPING `provider_accounts`/`provider_account_id` as the true account concept. Cleanest end state.
- Either way **KEEP the convergence vocabulary "provider-observed truth"** — the `pull.*` plane is about a provider *account's* observed facts, so "provider" is correct there. Pick A or B before starting.

## Findings (audit DONE 2026-06-24)
**"processor" is overloaded — three senses:**
1. **OUR rail** (`models.Processor`, columns `processor`/`processor_subscription_id`/`processor_customer_id`/`processor_transaction_id`/`processor_fields`/`processor_state`/`processors`, enum `processor_type`, table `processor_customers`, ~283 Go files) → **rename**.
2. **NMI's *acquirer* "processor"** in `internal/integrations/nmi/` + `internal/modules/webhooks/` — `json:"processor_id"`, `json:"processor_response_text"`, the `Processor` acquirer object (`body.Processor.ID`), and decline strings `transaction_was_declined_by_processor` / `transaction_error_returned_by_processor`. These mirror **NMI's actual webhook payload** — renaming breaks parsing. **PRESERVE** (NMI's external wire format; we don't own it, so pre-launch status is irrelevant here).
3. **Value strings** in CHECK constraints: ledger `processor_clearing`, blocklist `processor_customer` (singular, quoted). **PRESERVE**.

**Coupling:** DB column `processor` → sqlc-gen field `.Processor` → referenced everywhere ⇒ this is ONE coordinated transform (Go + SQL schema + queries + `task sqlc` regen), not a Go-only first pass.

**Method:** sentinel-protect the value strings (#3); EXCLUDE the NMI/webhook wire dirs from the *lowercase* sweep (preserves their acquirer wire tags #2 — their CamelCase fields still rename to `Rail*` but the lowercase `json:` tags survive, so wire stays correct); CamelCase `Processor`→`Rail` + lowercase `processor`→`rail` everywhere else; `git mv` package `processors`→`rails`; `task sqlc` regen; `go build`/`vet`/test (compose stack is up: openrails-postgres `:5434`, garnet, clickhouse).

**Pre-launch:** breaking DB/JSON/API changes are fine (Paul, 2026-06-24) — except NMI's external wire (#2). **LOC:** take trivial simplifications opportunistically, but keep the diff reviewable as a rename.

## Tasks
- [x] Audit: enumerate every payment-sense "processor" (Go idents, DB schema, `*.sql`, JSON, CLI, docs); separate from unrelated "processor" uses. **DONE 2026-06-24** — see Findings above.
- [x] Go rename: types/consts/files (`Processor*`→`Rail*`), `processors` pkg→`rails`, fetcher/handler/field renames. **DONE** — 811+56 `*.go` swept; `git mv` package + files; `go build`/`go vet` green; gofmt clean.
- [x] DB rename (breaking, no migration): `processor` col→`rail`, enum `processor_type`→`rail_type`, `processor_customers`→`rail_customers`; queries + `processor_customers.sql`→`rail_customers.sql`; `task sqlc` regen + vet. **DONE** — gen flipped (0 residual `Processor`).
- [x] JSON/HTTP/CLI + **ClickHouse** field rename (breaking). **DONE** — `json:"processor"`→`json:"rail"` (21 of-ours fields), env `PROCESSORS_*`→`RAILS_*`, example config/env, `migrations/clickhouse/*.sql` (caught by `TestAdminMetricsFolded`), `scripts/e2e_dump_local.sh`, `pkg/embedded/README.md`. No `--processor` CLI flag (only `--provider`, kept).
- [x] Provider-overload decision. **DECIDED A** — renamed only `processor`→`rail`; kept ALL `provider` (incl. type-holding `provider_intents.provider` col + `--provider` CLI, which still hold a rail value) + `provider_accounts`/`provider_account_id` + "provider-observed truth". B (rename those too) is an optional follow-up.
- [x] Docs/notes sweep + glossary. **DONE** — README + `docs/*.md` + `docs/api/*.md`; new `docs/glossary-rails.md`. Historical `agents/*.md` left as point-in-time records.
- [x] Grep gate. **DONE** — every residual is an intentional preservation (NMI acquirer wire / `processor_clearing` / `processor_customer`) or out-of-scope (historical notes, other-repo paths, skill-doc examples).

## Validation
- [x] `go build ./...` + `go vet ./...` clean. **PASS** (+ `gofmt -l` clean).
- [x] `task sqlc` generate + vet (PREPAREs every query vs schema). **PASS**. (`task sqlc-check` fails ONLY because gen differs from git HEAD — i.e. the whole uncommitted rename; passes once committed.)
- [~] DB-backed tests. Unit: **71/72 packages green** (the 1 fail, `internal/migrate TestRewriteMigrationsSchema`, is PRE-EXISTING — missing `025/026` migration files, untracked in git). Integration: `./tests` (incl. analytics/CH after the fix), `./pkg/service`, `./embed` **green**; targeted NMI-webhook (wire preserved) + checkout (API/DB) **green**. `./internal/river` 13 liveness/dunning fails are **PROVEN PRE-EXISTING** — identical `products_merchant_fk` on stashed HEAD (a test-harness merchant-seeding/ordering gap; my diff to those files is a pure rename, `internal/dbtest` untouched).
- [x] `rg -i '\bprocessor'` over go/sql/ch returns only intentional/unrelated hits. **PASS**.

## STATUS 2026-06-24 (Claude)
Rename **complete** across Go (`models.Rail`, `payments/rails` pkg, all idents), Postgres schema + queries + sqlc gen, ClickHouse schema, JSON/HTTP fields, env contract (`RAILS_*`), example config, scripts, and docs (+ new `docs/glossary-rails.md`). Three concepts kept distinct: **rail** (renamed from processor), **provider account** (`provider_*`, unchanged), **NMI acquirer "processor"** (wire fields preserved). Behavior unchanged — pure rename. **Uncommitted** in the working tree; not yet committed/tagged, and consumer bump (Cross-repo) not yet done. The one regression introduced (ClickHouse schema lag) was caught + fixed; all other test failures predate this change.

## Cross-repo
- The breaking embedded-API + JSON `processor` rename affects any consumer of openrails' Go API or HTTP that references `processor` (openrails-saas; doujins/hentai0 only if they read a `processor` field). Enumerate consumers, coordinate a version bump, adopt after this lands.

## Notes
- Pure rename, no behavior change — land as a reviewable rename (a Go-commit + a DB/sqlc-commit pair reads cleanest).
- Out of scope (separate, already-flagged decisions): `mobius` as the NMI rail id (rename to `nmi`?); keeping "provider-observed truth" in the convergence taxonomy (yes, keep).
