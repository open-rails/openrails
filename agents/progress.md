<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 645

---

# #644: product-kind-contracts-prevent-tier-group-misuse

**Completed:** no
**Status:** PROPOSED 2026-06-29: The catalog is letting different product shapes leak fields into each other. `config/catalog.example.yaml` puts `image-credit-topup` in `tier_group: ai-credit-topups`, but a variable credit top-up is a repeatable checkout/deposit product, not a mutually-exclusive SaaS membership tier. Same broader problem applies to VM rentals, durable movie ownership, API-credit purchases, and SaaS plans: they are different commercial shapes and should have inferred contracts from their fields, not a new required `product.type`.

## Metadata
- Category: billing
- Status: proposed
- Passes: false

## Problem

OpenRails currently has one broad `Product` struct with optional fields. That is flexible, but it lets invalid combinations look valid:

- **SaaS membership tier**: mutually exclusive within a `tier_group`, usually recurring, upgrade/downgrade comparable by `tier_rank`, may grant recurring credits/entitlements.
- **Credit/API top-up**: repeatable arbitrary checkout, deposits credits, may expire, should not upgrade/downgrade/cancel/replace anything.
- **Durable/limited ownership** such as a movie: buy once or rent for a fixed access window; product access/ownership semantics matter, not recurring tier hierarchy.
- **Rented infrastructure/resource usage** such as a VM: host owns the concrete resource lifecycle; OpenRails rates measured usage via meters/rate cards, not "user owns one VM product".

`tier_group` means "choose one active product from this subscription family" and drives tier-rank / upgrade semantics. A `credit_purchase` product does not behave that way: the customer may buy it repeatedly, the amount is arbitrary, and it deposits credits rather than plan membership.

The immediate symptom is the example top-up product using a fake tier group. The deeper issue is missing product-kind contracts.

There is a related price-model cleanup: the catalog currently carries `package` and `dynamic` price models, but the example and real use cases only exercise `flat`, `per_unit`, and `tiered`. `package` is redundant with `per_unit + divide_by + round: up` plus allowances/fixed products, and `dynamic` is speculative until a real provider-cost-plus-markup product exists.

## Design Direction

Separate two concepts:

- `price.model` is good and should stay. Stripe, Lago, and OpenMeter all use an explicit price/charge-model discriminator because the formula cannot be inferred safely from fields.
- Do not add a required `product.type` field. Product kind should be inferred from behavior fields, then illegal mixes should be rejected.
- The Go catalog structs must match the YAML shape 1:1. Do not keep a separate awkward Go-only representation that then has to be adapted into the YAML shape; parsing structs, validation, tests, and examples should speak the same structure.

Use inferred product kinds. The current fields are already enough to infer the shape:

- `credit_purchase` => top-up product.
- usage `rate_cards` without normal subscription prices => metered/rental product.
- `tier_group`/`tier_rank` with recurring prices => SaaS membership tier.
- product access / entitlement / finite-duration one-time prices => ownership/rental access product.

Then validate illegal mixes. Keep the YAML smaller and avoid a second field that can disagree with the actual configured behavior.

Price model direction:

- Keep `model: flat`, `model: per_unit`, and `model: tiered`.
- Move money currency/provider terms onto each price/offer, not the credit top-up product shape. A credit top-up product defines what balance is delivered; each price defines how much money, which currency, and which providers can sell it.
- Allow multiple credit-purchase prices/offers for the same top-up product when needed, e.g. USD and EUR offers, Stripe-only vs Solana-only offers, monthly promo rate vs normal rate.
- Remove duplicate credit-balance declarations. Today membership grants use `credits.ai-image-gen.unit = ai-image-credit`, while top-ups repeat `credit_type: ai-image-gen` and `unit: ai-image-credit`. The credit balance should be defined once and referenced by key. Product credit grants should be an array of keyed grant entries, not a map, so multiple grants are readable and ordered.
- Remove `model: package` and `model: dynamic` until a real merchant/product needs them.
- Keep both tier modes in the generic tiered price model:
  - `mode: graduated` for marginal tiers; standard in Stripe/OpenMeter/Lago and required for credit purchases.
  - `mode: volume` for intentional bulk repricing where the whole quantity is priced at the reached tier; standard in Stripe/OpenMeter/Lago, valid for some metered usage contracts.
- Restrict `credit_purchase` to `mode: graduated` because `volume` creates cliffs and makes spend-to-credits inversion ambiguous near thresholds.

## Current Shape Audit (2026-06-30)

Current code is not merely missing comments/docs; the implementation shape still encodes the wrong model:

- `pkg/catalog/manifest.go` has one broad `Product` with `TierGroup`, `Credits`, `Prices`, `RateCards`, and `CreditPurchase` all optional. There is no inferred-kind validation preventing a repeatable top-up from also being a tier.
- `Product.Credits` is currently `map[string]CreditGrant`, where the map key is the credit balance id and the grant repeats `unit`/`currency`. This does not match the desired YAML array shape and cannot preserve ordering. It also makes absence vs zero amount hard to express for variable top-ups.
- `Manifest` has no top-level `credit_balances`, so the `ai-image-gen` balance is defined implicitly in every membership grant and repeated again in the top-up block.
- `CreditPurchase` is currently a wrapper under the product with `credit_type`, `unit`, `currency`, `providers`, `input_min`, `input_max`, `round`, and a singular nested `price`. That is the exact shape we want to retire: money/provider/offer fields are product-level in Go/YAML, and multiple prices are impossible.
- `Product.Prices []Price` is the legacy fixed/subscription purchase shape (`unit_amount`, `duration`, `auto_renew`, `providers`, `provider_links`, `trial`, `metered`). It does not directly support top-up pricing fields such as `model`, `mode`, `tiers`, `input_min`, or `input_max`.
- `RateCard.Price` uses `pricing.RatePrice`, a separate charge-model shape with `model`, `currency`, `providers`, `amount`, `unit_amount`, `divide_by`, `mode`, `tiers`, `matrix`, etc. This is useful for metered/resource pricing, but it means top-up prices and normal product prices do not currently share one catalog offer shape.
- `pkg/pricing` and `pkg/catalog/ratecard.go` still expose and validate `package` and `dynamic`, and tests still cover them, even though no current example needs them.
- Persistence mirrors the wrapper: migration `046_catalog_rate_cards` creates one `catalog_credit_purchases` row per product, with `credit_type`, `unit`, `currency`, `providers`, limits, round mode, and raw `price` JSON. The primary key is `product_id`, so the DB cannot represent multiple top-up prices/offers for one product.
- Runtime quote/deposit reads that singular `catalog_credit_purchases` row by product. Supporting per-price currency/provider offers will require changing the query and input contract, not just changing YAML.
- `config/catalog.example.yaml` still has `image-credit-topup` with `tier_group: ai-credit-topups`, a `credit_purchase` wrapper, duplicated `unit`, product-level `currency`/`providers`, and a nested singular `price`.

Target YAML direction:

```yaml
credit_balances:
  - key: ai-image-gen
    unit: ai-image-credit
    expires_default: 30d

products:
  - key: grandmaster
    tier_group: cozy
    tier_rank: 4
    credits:
      - key: ai-image-gen
        amount: 15_000
        cadence: per_renewal
    prices:
      - currency: usd
        unit_amount: 119_000_000
        duration: 30d
        auto_renew: true
        providers: [stripe]

  - key: image-credit-topup
    credits:
      - key: ai-image-gen
    prices:
      - currency: usd
        providers: [stripe]
        input_min: 5_000_000
        input_max: 500_000_000
        round: down
        model: tiered
        mode: graduated
        tiers:
          - up_to: 2_000
            unit_amount: 10_000
          - up_to: 10_000
            unit_amount: 9_000
          - up_to: null
            unit_amount: 8_000
```

The top-up product says "this purchase deposits into `ai-image-gen`"; each price says "this is how this offer is bought." `amount` is required for fixed/recurring grants and omitted for variable top-ups. The Go struct should make that visible, likely with `Amount *int64` rather than `int64`, so validation can distinguish omitted from zero.

## Migration Plan

1. **Manifest structs first.** Add `Manifest.CreditBalances []CreditBalance`. Change product credits from `map[string]CreditGrant` to `[]CreditGrant` with a required `Key` field and pointer `Amount`. Keep Go field names/tags aligned exactly with the target YAML.
2. **Unify product offer parsing.** Extend or replace `Product.Prices []Price` so top-up offers can carry `model`, `mode`, `tiers`, `input_min`, `input_max`, and `round` directly on the price entry. Do not keep a Go-only `CreditPurchase.Price` adapter shape.
3. **Retire the `credit_purchase` wrapper.** Remove `Product.CreditPurchase` from the desired catalog shape. Infer "credit top-up" from a product with variable credit delivery (`credits: [{key: ...}]` with no fixed amount) and prices that contain charge-model fields.
4. **Keep rate-card pricing separate only where the YAML is separate.** `rate_cards[].price` can continue to use a rate-card price struct if that YAML remains nested under `rate_cards`. The rule is not "one global price struct for everything"; it is "each YAML shape has a matching Go shape, without an extra hidden adapter shape."
5. **Validation pass.** Validate credit balances globally, reject duplicate balance keys, reject product duplicate `credits[].key`, require every grant to reference an existing balance, require fixed membership grants to set `amount`, require variable top-ups to omit `tier_group`, reject top-up `mode: volume`, and reject ambiguous multiple top-up prices with the same currency/provider until offer keys are introduced.
6. **Delete speculative charge models.** Remove `package` and `dynamic` constants, validation cases, engine branches, tests, and docs while keeping `flat`, `per_unit`, `tiered`, `graduated`, and `volume`.
7. **DB sidecar migration.** Add catalog storage for canonical credit balances. Replace the one-row-per-product `catalog_credit_purchases` shape with a shape that can represent one top-up product and N prices, e.g. `catalog_credit_purchase_products(product_id, credit_balance_key, expires_hours)` plus `catalog_credit_purchase_prices(product_id, ordinal, currency, providers, input_min, input_max, round, price jsonb)`, or an equivalent normalized shape.
8. **Applier/service mapping.** Update `pkg/catalog/applier_service.go` and `pkg/service/catalog_sidecars.go` so the final YAML maps directly into service specs. Runtime-normalized structs are fine behind this boundary, but the loader structs should not preserve the old wrapper.
9. **Quote/runtime contract.** Update `QuoteCatalogCreditPurchase` to choose a top-up offer by product plus currency/provider or a future price key. For this pass, reject multiple indistinguishable prices rather than inventing a hidden selector.
10. **Examples and tests.** Update `config/catalog.example.yaml` to the canonical balance + credits array + prices-on-top-up shape. Add loader tests for the new good shape and each invalid mix. Add DB-backed integration tests proving multiple top-up prices persist, quote correctly, and deposit into the same canonical credit balance.

## Tasks

- [ ] Remove `tier_group: ai-credit-topups` from `image-credit-topup` in `config/catalog.example.yaml`.
- [ ] Add loader validation rejecting products that define both `credit_purchase` and `tier_group`.
- [ ] Add a focused loader test proving credit-purchase products are standalone repeatable top-up products.
- [ ] Audit `config/catalog.example.yaml` for other field leaks between product shapes, especially metered/rental products inside subscription tier groups.
- [ ] Keep `price.model` explicit; document that it is the price formula discriminator, not the product kind.
- [ ] Redesign top-up products so the delivered credit balance is declared by `credits: [{key: ...}]`, while `currency`, `providers`, input bounds, round mode, and price formula live on one or more `prices` entries.
- [ ] Remove the desired YAML `credit_purchase` wrapper; if a transition shim is kept temporarily, mark it legacy and keep it out of `config/catalog.example.yaml`.
- [ ] Add validation/tests for multiple credit-purchase prices on one top-up product and remove product-level top-up `currency`/`providers`.
- [ ] Make Go catalog structs mirror the final YAML shapes 1:1 for all catalog items: products, prices, credits, credit balances, rate cards, and usage meters.
- [ ] Remove adapter-only struct shapes where they exist only to compensate for YAML/Go mismatch; keep separate normalized runtime structs only behind validation/apply boundaries.
- [ ] Introduce one canonical credit-balance definition per merchant/catalog, e.g. `credit_balances: [{key: ai-image-gen, unit: ai-image-credit, expires_default: 30d}]`, then have memberships and top-ups reference `ai-image-gen` instead of repeating `unit`.
- [ ] Change product credit grants from a map to an array shape: `credits: [{key: ai-image-gen, amount: 15_000, cadence: per_renewal}]`.
- [ ] Support multiple credit grants per product with the array shape; reject duplicate `credits[].key` entries on the same product.
- [ ] Update membership `credits` and `credit_purchase` examples to reference the same credit balance key; reject mismatched duplicate unit declarations.
- [ ] Add `CreditGrant.Amount *int64` or equivalent so validation can distinguish fixed grants from variable top-up delivery.
- [ ] Add `Manifest.CreditBalances []CreditBalance` plus service/applier persistence for canonical balance metadata.
- [ ] Define the small set of inferred product kinds in comments/docs near catalog validation: membership tier, credit top-up, access/ownership product, metered/rental product.
- [ ] Add validation for any other obviously invalid combinations found in the audit; do not add a broad `product.type` migration.
- [ ] Remove unused `package` and `dynamic` price models from catalog validation, pricing constants/engine, tests, and docs/progress wording.
- [ ] Keep `tiered` support for both `graduated` and `volume` in the generic pricing engine.
- [ ] Keep/ensure validation rejects `credit_purchase` with `mode: volume`; credit top-ups must use graduated tiering for stable bidirectional quotes.
- [ ] Keep membership products that grant recurring credits in tier groups; only variable top-up products are forbidden.
- [ ] Replace the one-row-per-product `catalog_credit_purchases` storage with a multi-price top-up storage shape, or prove the existing schema can represent multiple prices before claiming support.
- [ ] Update credit-purchase quote/deposit integration tests to use the final YAML-shaped catalog, not hand-inserted legacy sidecar rows.

---

# #642: metering catalog corrections — usage products aren't subscriptions; pricing-only rate cards (collection/invoicing → #643)

**Completed:** yes
**Status:** DONE 2026-06-30 (commit 58d08981). Implemented the catalog-layer cleanup. KEY DECISION made
during implementation: the "calendar-month `rating_period`" is NOT a new per-card field. The cap/allowance
window already IS the invoice period (`sweepCatalogRateCardUsage` runs over the invoice's `[from,to)`), and
the merchant invoice boundary already supports `calendar_month` (`InvoiceBillingBoundary`,
`CurrentInvoicePeriodStart`). A per-card rating period that disagrees with the close cadence cannot even be
honored (to cap monthly you MUST close monthly — the sweep only sees one window). So `billing_cadence` was
DROPPED outright (the field + the `catalog_rate_cards.billing_cadence_hours` column) rather than renamed; the
window = invoice period, documented in the example. LANDED: `minimum_amount` removed from `RatePrice`+`MatrixCell`
(`applyCommitments` cap-only; `maximum_amount` kept — real per-SKU cap); dead `included_per_cycle` removed (rater
reads matrix `cell.Included`); usage products (rate_cards present) carry no `tier_group` (loader → synthetic
singleton group, plan persists NULL); example faithful to DO. Tests green: pkg/pricing + pkg/catalog units, the
example-parse gate, and the testcontainers rate-card metered-rating integration test.
PROPOSED 2026-06-29: design review of the #638 realistic-resource-metering catalog
(`config/catalog.example.yaml`, `pkg/catalog/ratecard.go`, `internal/modules/money/metered_rating.go`)
surfaced structural + realism issues. The rating MATH itself is implemented and correct (matrix
per-SKU pricing, `divide_by`, `min`/`max` commitment range, flat + accrued pooled allowances,
tiers — verified used: Matrix 21 / DivideBy 9 / Min/Max 14 / Allowance 14 / Pool 7 consumption
sites; `input_min/max` enforced at quote). The problems are catalog fields that conflate PRICING
with the BILLING RELATIONSHIP, a metered product forced into the subscription tier hierarchy, one
dead field, and a fabricated charge floor in the example. SPLIT: #642 is the CATALOG-LAYER cleanup
(rate cards become pricing-only); the WHEN-to-bill / minimums / invoicing subsystem is #643. NO code
touched (the #638 metering work is a concurrent agent's — this is the design plan to hand over).

## Metadata
- Category: refactor
- Status: done
- Passes: true

## Problem

#638 models a metered product (a DigitalOcean-style Droplet) as a Product inside a `tier_group`,
with a `billing_cadence` on the product, plus rate cards carrying matrix pricing + allowances.
Defects:

1. **`tier_group` does not belong on a usage-metered product.** `tier_group` enforces "one active
   subscription per (user, tier_group)" — a MEMBERSHIP-EXCLUSIVITY concept (free vs pro vs
   enterprise: pick one; `uq_subscriptions_user_tier_group_active`). A metered resource is the
   opposite: a customer runs N droplets concurrently, and a droplet is NOT a "subscription to a
   tier" — it is a resource that emits usage events. Forcing usage products into the subscription
   `TierGroups → Products` hierarchy is a structural mismatch; the uniqueness constraint actively
   fights multi-resource usage.

2. **`billing_cadence` on the product conflates PRICING with COLLECTION.** Two independent axes are
   collapsed into one field:
   - the **rating/cap window** (the period over which `maximum_amount` caps and allowances accrue)
     — a PRICING fact that belongs on the rate card; and
   - the **collection cadence** (WHEN the accrued balance is invoiced — monthly / weekly / at $X
     owed / whichever-first) — a property of the BILLING RELATIONSHIP, not the catalog.
   They need not align (rate caps monthly but collect at a $50 threshold). And **threshold billing
   ("invoice at $X owed") cannot be a product field at all** — it is a function of the customer's
   running balance across ALL their resources, so it is structurally impossible to express on a
   single product. That alone proves collection cadence is a policy, not catalog. A customer with 5
   droplets + block storage + egress gets ONE invoice, not 5 on 5 per-product cadences.

3. **`included_per_cycle` is dead/decorative.** The accrued-allowance rater (`metered_rating.go`
   ~L335/343/349) reads the SOURCE rate card's matrix `cell.Included` directly; the example's
   `included_per_cycle: size_slug.included` string is never parsed or consumed (0 consumption sites;
   only a struct field). Wire it or remove it.

4. **Per-rate-card `minimum_amount` is the wrong concept — remove it.** Two different things wear the
   word "minimum" and only one is real:
   - a **per-line/per-resource floor** ("this droplet charges at least $0.01") — what's on the card
     today. No real cloud floors a VM's charge (AWS/GCP/Azure/DO bill actual usage); the only relative
     is a minimum billing *increment* ("round up to 1h"), a DURATION concept that modern per-second
     billing retired. As a card field it degrades to "don't emit sub-cent lines" = rounding hygiene,
     which is a global invoice policy, not pricing. On the example it is doubly wrong: DO has NO
     droplet minimum, and the `$0.01 / 60s` comments mis-describe the code (`applyCommitments` clamps a
     per-PERIOD total floor, NOT per-60s).
   - a **minimum commitment / minimum spend** ("customer commits to $500/mo; if usage is less, true-up
     to $500") — real and valuable, but a CONTRACT property of the billing relationship, not a catalog
     field. Open-source (Lago) + commercial (Orb, Metronome, Kill Bill) all model minimums as
     account/subscription-level commitments with true-up — none floor individual priced lines. The
     industry puts minimums where the CONTRACT lives, not where the PRICE lives.
   Conclusion: **drop per-rate-card `minimum_amount` entirely** (cap `maximum_amount` STAYS — a real,
   intrinsic per-SKU pricing fact: DO genuinely caps a droplet at its monthly price). If minimums are
   wanted, add account-level `minimum_spend` (commitment + period-end true-up) to the billing policy.

## Target design

- **Rate card (catalog, pricing only):** keep `rating_period` (the cap/allowance window) + per-SKU
  rate + `maximum_amount` cap + allowances on the card. The cap STAYS (a real, intrinsic per-SKU
  pricing fact — DO caps a droplet at its monthly price). `minimum_amount` is REMOVED (a floor is not
  pricing). The card knows WHAT is metered and HOW MUCH; nothing about WHEN money is collected, minimum
  commitments, or true-ups. `rating_period` is **calendar-month-anchored** (resets on the 1st), NOT a
  rolling 30-day window — follow DO's convention, which is what businesses / accounting / invoicing
  actually expect (books close on calendar months). So `maximum_amount` means a **flat price per
  calendar month regardless of month length** (28 vs 31 days): the hourly rate is `monthly ÷ 672`
  (28×24h), so a 24/7 server hits the cap exactly at the end of February and *early* in a 31-day month
  with the remaining hours free — same monthly bill either way. (The #638 example is self-inconsistent
  here: `divide_by` encodes the 672h/28-day breakeven while `billing_cadence: 30d` says 30d-rolling; the
  calendar-month `rating_period` fixes it.)
- **Everything about WHEN money is collected → #643** (the billing-relationship layer): the
  merchant/account collection policy (monthly / weekly / $X-threshold / hybrid), account-level
  `minimum_spend` with true-up, the per-customer invoice-close job, and the optional global
  invoice-rounding policy. None of it belongs on the catalog. This issue (#642) leaves the rate card
  pricing-only; #643 builds the policy + invoicing subsystem on top.

## Tasks

- [ ] Remove `tier_group` from usage-metered products (they are not tier-exclusive subscriptions);
      model a usage product outside the subscription `TierGroups` hierarchy, or make `tier_group`
      optional + ignored for usage products.
- [ ] Split `billing_cadence`: keep a `rating_period` on the rate card, **calendar-month-anchored**
      (cap + allowances reset on the 1st), NOT a rolling 30-day duration — so `maximum_amount` is a
      flat price per calendar month regardless of 28/31-day length (DO convention; hourly rate =
      monthly÷672). Drop the collection-cadence meaning from the product.
- [ ] Wire or remove `included_per_cycle` (the accrued-allowance rater hardcodes the source card's
      `cell.Included` today).
- [ ] Remove `minimum_amount` from the rate-card model ENTIRELY (a per-line floor is not pricing);
      keep `maximum_amount` (the cap is a real per-SKU pricing fact). Delete the droplet card's
      `minimum_amount` + its `$0.01/60s` comments.
- [ ] Make `config/catalog.example.yaml` faithful: the DO example uses only mechanisms DO actually
      applies (hourly rate, calendar-month cap, pooled egress allowance) — no fabricated minimum, no
      30d-rolling window.
- [ ] (→ #643) collection policy + account `minimum_spend` + invoice-close job + optional invoice
      rounding — the billing-relationship layer, tracked separately.

Acceptance: a usage-metered product carries no `tier_group`, no `billing_cadence`, no `minimum_amount`;
its rate card carries only a **calendar-month-anchored `rating_period`** + per-SKU rate + `maximum_amount`
cap + allowances (flat monthly cap regardless of 28/31-day length, DO convention; not a rolling 30 days);
`included_per_cycle` is either functional or gone; the droplet example matches real DO. The #638 pricing
MATH is reused unchanged. (Collection cadence, `minimum_spend`, threshold billing, and the invoice-close
job → #643.)

---

# #643: collection & invoicing policy — merchant/account billing cadence, minimum_spend, invoice-close

**Completed:** core done; one optional follow-up open (per-account override)
**Status:** DONE-CORE 2026-06-30 (commit 9beec4d5). MAJOR DISCOVERY during implementation: this was NOT
greenfield — ~80% already existed. `FinalizeThresholdInvoices` (threshold mode), `FinalizeDueInvoicesForBoundary`
(periodic, calendar-month-anchored via `InvoiceBillingBoundary`), the `InvoiceWorker` River driver (runs
threshold-finalize always + monthly-finalize when scheduled + collect → effectively hybrid), one-invoice-
per-customer rollup (`FinalizeInvoice` rolls ALL the customer's usage into one invoice per currency), and the
merchant collection config (`InvoiceCollectionThreshold` / `InvoiceBillingBoundary` / `InvoiceMonthlyFloor`)
were all already built. The genuinely-missing, genuinely-valuable piece was the account-level MINIMUM_SPEND
COMMITMENT with true-up — built here:
- New `openrails.customer_minimum_spend` (merchant/customer/currency commitment) + sqlc get/upsert/delete
  (migration 047); `MoneyService.SetCustomerMinimumSpend` / `GetCustomerMinimumSpend`.
- `FinalizeInvoice` opt-in `WithMinimumSpendTrueUp`: on a full-period close, if rated usage < commitment,
  post the shortfall as a real owed ledger accrual + an invoiced `minimum_spend_trueup` line (amount_due
  reaches the minimum; arrears ledger consistent). Idempotent. Threshold mid-period closes omit it.
- Wired into the periodic boundary close (`FinalizeDueInvoicesForBoundary`) — the monthly path the
  InvoiceWorker drives. testcontainers integration tests: true-up to minimum, real owed ledger, idempotency,
  no-op without the option / when usage already meets the minimum.
REMAINING (optional, low value for current products — enterprise feature): per-CUSTOMER override of the
merchant collection cadence/threshold (today `InvoiceSettings` is merchant-wide; the override would thread a
per-customer threshold/boundary into `InvoiceSettings` + `ListInvoiceThresholdCandidates`), and an admin/HTTP
surface for `SetCustomerMinimumSpend` (the service method exists + is tested; no caller wired yet). Left
unbuilt per YAGNI — the merchant-level config covers the common case.
PROPOSED 2026-06-29: the billing-relationship half of the #642 metering review (split out).

## Metadata
- Category: feature
- Status: core_done
- Passes: true

## Problem

Metered usage (#638/#642) accrues an owed balance per CUSTOMER ACCOUNT across all their resources.
Nothing today decides WHEN that balance is invoiced, supports minimum-spend commitments, or closes the
balance into one invoice. These are properties of the BILLING RELATIONSHIP, not the catalog:
- **Collection cadence** — monthly / weekly / at $X owed / whichever-comes-first. Threshold billing
  ("invoice at $X owed") cannot be a product field — it is a function of the customer's whole running
  balance across all resources.
- **Minimum spend** — "commit $X/period, true-up at close if usage is less." A contract commitment
  (how Lago/Orb/Metronome/Kill Bill model minimums) — never a floor on a priced line.
- **One invoice per customer** — a customer with 5 droplets + block storage + egress gets ONE invoice,
  not 5 on 5 per-product cadences.

## Target design

- **Merchant collection policy (default):** `{mode: periodic(monthly|weekly|…) | threshold($X) |
  hybrid(whichever-first), value, currency}`, alongside the existing merchant `money_settings`.
- **Customer-account override:** a customer overrides the merchant default (enterprise NET-30 monthly
  vs self-serve $X-threshold), same precedence as the tier ladder (merchant default → account
  override). The invoiced entity is the CUSTOMER ACCOUNT (one accrued balance across all resources).
- **Account `minimum_spend`:** commit $X/period; at invoice close, if rated usage < minimum, true-up to
  the minimum. A contract property, NOT catalog.
- **Invoice-close job:** per customer account, fire when `rating_period elapsed OR accrued ≥ threshold`,
  rate the open usage for the window (reuse #638 rating math + #642's calendar-month `rating_period`),
  apply any `minimum_spend` true-up, and emit ONE invoice covering all the customer's rate cards.
- **Global invoice-rounding policy (optional):** sub-cent line suppression ("don't bill below $X") is a
  merchant invoice policy, NOT a per-rate-card `minimum_amount`.

## Tasks

- [x] Merchant-level collection policy — ALREADY EXISTED: `InvoiceCollectionThreshold` (threshold) +
      `InvoiceBillingBoundary` (periodic: calendar_month|anniversary|fixed_interval) in merchant config,
      read by `InvoiceSettings`. The mode is expressed by which finalize the `InvoiceWorker` runs
      (threshold always + monthly when scheduled = hybrid). [~] per-customer OVERRIDE still open (see Status).
- [x] Account-level `minimum_spend` (commit $X/period, period-end true-up at close) — BUILT: migration 047
      `customer_minimum_spend` + `SetCustomerMinimumSpend` + `FinalizeInvoice` `WithMinimumSpendTrueUp`.
- [x] Invoice-close driver — ALREADY EXISTED (`FinalizeThresholdInvoices` + `FinalizeDueInvoicesForBoundary`
      + `InvoiceWorker`); ONE invoice per customer per currency across all rate cards is how `FinalizeInvoice`
      already rolls up. Minimum-spend true-up now hooks the periodic close.
- [ ] (Optional) Global merchant invoice-rounding policy ("don't bill below $X") — not built (low value).

Acceptance: collection cadence is a merchant policy with per-account override; threshold billing works
(impossible under a per-product `billing_cadence`); `minimum_spend` trues-up at close; one invoice per
customer account covers all their rate cards. Depends on #642 (pricing-only rate cards + calendar-month
`rating_period`); #638 pricing MATH reused unchanged.

---

# #641: provider-account-routing-roles + per-account-webhooks + catalog-push-targeting

**Completed:** yes
**Status:** COMPLETE 2026-06-30: all five task groups (taxonomy, multi-account indexing, per-account webhooks [NMI+Stripe], provider_account_id stamping, catalog primary+secondary) landed + tested; remaining refinements consciously dropped as over-engineering (see DELIBERATELY DROPPED). Builds on #630 (rail = gateway, provider account = a credentialed instance on a rail). Makes N provider accounts per rail per merchant a first-class, routable thing: each account carries a routing role (primary/secondary/legacy), each gets its own inbound webhook endpoint keyed by account_id, and catalog pushes target only primary+secondary. Supersedes the deferred "Option R" multi-NMI item — that error exists because runtime client maps are keyed by rail, not account.

LANDED (uncommitted, master working tree; touched packages build green, unit + targeted integration tests green against the compose stack):
- Routing taxonomy primary|secondary|legacy + `account_id`/`EffectiveAccountID` (`config/config.go`).
- Multi-NMI-account boot — clients keyed by account_id, single-NMI error gone, primary aliased under rail key transitionally (`internal/app/build_runtime.go`, `tests/testcontainer_suite.go`).
- `provider_account_id` stamping — columns already existed (no migration); plumbed through models + create queries (sqlc regen) + repo + a primary-resolving chokepoint, WITH a context-pin override (`repo.WithProviderAccountID`) so per-account webhooks stamp the routed account. Integration test green (primary default + pin override).
- Per-account inbound webhooks — `:account_id` route + account-scoped secret resolution for **NMI and Stripe** (`Load{NMIWebhookSigningSecret,StripeCredentials}ForAccount`) + dispatcher per-account NMI client; unknown account rejected (404, no fallback); records stamped with the routed account. Integration tests green; existing webhook HTTP suite still green.
- Catalog push targets primary+secondary, skips legacy — `syncSecondaryCatalogAccounts` does an idempotent best-effort sync to each secondary; adapters account-aware via `autoCreateContext.TargetAccountID` (Stripe secret override + NMI client selection). Tested.
- `config/merchants.example.yaml` updated (two NMI accounts on one rail + a legacy Stripe account, account_id = gateway-id); strengthened `merchant_manifest_test`; fixed a pre-existing #630-staleness unit test.

DELIBERATELY DROPPED as over-engineering (Paul 2026-06-30 — "delete any tasks you feel are over-engineering"). The per-account PATH + write-time secondary sync already deliver the capability; these add cost without a concrete need:
- Payload-disambiguation single-shared-endpoint mode (CCBill `clientAccnum`, Stripe Connect `event.account`) — the per-account PATH covers multi-account routing; this is a convenience alternative with no current consumer. YAGNI.
- CCBill account-scoped path — CCBill auth is IP-allowlist + account-number match (no per-account HMAC secret), and the postback already carries the account number, so there is nothing account-scoped to add.
- Persist per-account catalog links for secondaries — a broad links-BY-ACCOUNT model change whose only added value is drift DETECTION on failover accounts; the idempotent write-time sync already keeps secondaries current.
- Config-layer per-(merchant,rail,environment) primary-uniqueness — NON-APPLICABLE: the in-process `ProviderAccountConfig` has no environment axis (environment is global via test_mode); the manifest/DB upsert already enforces one primary per (merchant,type,environment), and `validateRails` enforces ≤1 primary per rail.
- Strictly-require account_id in the in-process set — the name-fallback is intentional for tests/embedded callers; the manifest (production) path already requires it.
- Stripe managed-webhook auto-registration of the per-account URL — the primary auto-registers and works on the existing route; a secondary Stripe account's webhook is one manual dashboard setting (point it at `…/webhooks/{slug}/stripe/{account_id}`). Auto-registering N endpoints across N Stripe accounts is automation polish for a rare case.

Note: a separate, concurrent #638/#639/#642 catalog rework is editing `pkg/catalog`/`pkg/pricing` in the same tree (transient full-build breaks there are NOT this work). Pre-existing failing integration tests on master (NOT caused by #641, verified on clean master): a cluster of `TestDunning*`/`TestFailMembership*`/`TestEntitlementsDunningStateMachine_NMI*`, `TestGetProductsEndpoint`, and `TestProviderAccountScopedLocalState…` (single-package seeding fragility).

## Metadata

- Category: billing
- Status: complete
- Passes: true

## Problem

A merchant can legitimately hold multiple credentialed accounts on the SAME rail:

- **NMI** — multiple gateway accounts (different ISOs/MIDs: e.g. mobius + paykings) for redundancy or per-product routing. This is common.
- **Stripe** — an old/legacy `acct_…` kept for in-flight subscriptions plus a new `acct_…` for fresh business.
- Multi-merchant hosts run one process across many merchants, each with their own accounts.

Today the model is single-account-per-rail:

- Runtime client maps are keyed by rail (`rt.NMIClients[string(models.RailNMI)]`), so only ONE NMI client can exist; `build_runtime` hard-errors on a second NMI account ("only one NMI account is supported per deployment").
- Catalog adapters resolve THE primary account per rail (`GetStripeRail()`, `nmiClient()`); there is no notion of pushing to a fallback account, nor of skipping a retired one.
- Inbound webhooks are routed merchant-by-slug then secret-by-rail. With N accounts on a rail (each its own signing secret), an inbound event cannot be matched to the right account — the NMI payload carries no gateway-id, so the rail alone is ambiguous.
- Money records (`subscriptions`, `payments`, `payment_methods`) are not stamped with which account processed them, so reconciliation/refunds/rebills can't target the originating account.

## Decisions (settled with Paul 2026-06-29)

1. **Routing roles: `primary` / `secondary` / `legacy`.** Per provider account, per rail.
   - `primary` — receives new default work AND catalog push.
   - `secondary` — redundancy/fallback; receives catalog push (kept in sync so it can take over); not selected for new work unless the primary is unavailable (the actual fallback *policy* is #288's job — this issue only makes secondary a deterministic, pushable target).
   - `legacy` — retained for old subscriptions, rebills, refunds, and INBOUND webhooks; receives NO new work and NO catalog push.
   - **Retirement uses the existing `status` axis, not a fourth role:** `status=disabled` ("archived") = fully retired, no inbound expected. `role` and `status` are the two columns `openrails.provider_accounts` already has.
2. **Resolve the inbound account by id — prefer the payload, fall back to the path.** One webhook configured per external account. Resolution order:
   - **Preferred: an account identifier carried IN the webhook payload** — disambiguate on it, and a single shared endpoint per rail suffices. Known cases: **CCBill** postback carries `clientAccnum`/`clientSubacc`; **Stripe Connect** events carry `event.account` (`acct_…`).
   - **Fallback: the `account_id` as a path parameter** — required for any rail/event whose payload does NOT carry the identifier (assume **NMI** here; verify each rail). The path then names which external account the event is for.
   This is the only correct mechanism when one merchant has N accounts on a rail and when one process hosts many merchants. Either way OpenRails resolves the SPECIFIC account, loads THAT account's signing secret, and stamps the resulting records.
3. **Index provider accounts by their ID (gateway-id / `account_id`), never by an arbitrary config name.** The YAML map key ("mobius", "paykings") is a human label only; the routing identity is the operator-declared `account_id` (NMI gateway-id, Stripe `acct_…`, CCBill `client_acc_num[/sub]`, Solana recipient wallet — per #592, operator-declared, no runtime whoami). Self-discovering rails (Stripe) may fill it from `GET /v1/account`; the rest must declare it.
4. **Catalog push/verify/drift target primary + secondary only.** Legacy and disabled accounts are never pushed to — old pricing on a retired account is intentional and must not be mutated.

## Current state (verified)

- `config.ProviderAccountConfig` has `Routing string` (`default|manual|legacy`) + `PrimaryRailByType()` which returns the single default and ERRORS on multiple defaults. `GetStripeRail/GetCCBillRail/GetSolanaRail` return that single primary.
- `openrails.provider_accounts` already has `role` + `status` columns, `PromoteProviderAccountToPrimary` / `DemoteOtherPrimaryProviderAccounts` queries, and manifest reconcile (`internal/bootstrap/merchant_manifest.go`).
- `NMIWebhookHandler.SecretFor func(rail string)` and `WebhookMessage.{Rail,SigningSecret}` already allow per-message secret selection ("NMI deployments can run multiple gateway aliases, each with its own secret") — but the HTTP layer never sets a per-account secret; `merchants/webhook_routing.go ResolveBySlug` resolves only the merchant.
- `rt.NMIClients` is keyed by rail (single client/rail); `build_runtime` errors on a 2nd NMI account.
- `NMIRailConfig{SecurityKey,TokenizationKey,WebhookSecret}` has NO gateway-id field — can't be indexed by ID yet.
- `openrails.rail_customers` already has a `provider_account_id` column; `subscriptions`/`payments`/`payment_methods` domain models do NOT.

## Tasks

- TAXONOMY:
- [x] Replace the config `Routing` taxonomy `default|manual|legacy` with `primary|secondary|legacy` (hardcut). Done in `config/config.go`: consts `RailRoutingPrimary|Secondary|Legacy`, `manual` deleted; `EffectiveRouting` defaults to primary; `validateRails`/`PrimaryRailByType` updated. Already matches the manifest layer's `configRailRolePrimary|Secondary|Legacy` (`internal/bootstrap/`).
- [~] manifest `mode`/`status=disabled` already map to role+status in `internal/bootstrap/merchant_manifest.go` (unknown mode → secondary+disabled), and `validateRails` enforces ≤1 `primary` per rail. STILL TODO: per-(merchant,rail,**environment**) primary uniqueness at the config layer (DB demote already exists).
- CONFIG / INDEXING:
- [x] Added `AccountID` + `EffectiveAccountID(name)` to `ProviderAccountConfig`; runtime keys off the account_id. NOTE: `EffectiveAccountID` falls back to the map name when account_id is unset (keeps existing tests/embedded callers working); the manifest path already REQUIRES `account_id`. STILL TODO: make account_id strictly required for nmi/ccbill/solana in the in-process set.
- [x] `createNMIClients` builds one client per account keyed by `account_id`; single-NMI error removed. TRANSITIONAL: the primary is also aliased under the rail key `"nmi"` so the ~30 `NMIClients[rail]` consumers keep resolving the primary until records carry `provider_account_id`. Stripe/CCBill/Solana already tolerate multiple configs via `GetXRail()` primary-selection, so NMI was the only hard blocker.
- WEBHOOKS:
- [x] Per-account endpoint `POST /merchants/:merchant/webhooks/:provider/:account_id` added (additive; the existing `:provider`-only route is unchanged → no regression). Handler reads `:account_id`, loads THAT account's signing secret, and rejects an unknown account (404, no fallback). `WebhookMessage.ProviderAccountID` carries the routed account; the dispatcher selects `NMIClients[account_id]` (falls back to the rail/primary alias). NMI wired + integration-tested (`internal/merchants` per-account secret test + existing webhook suite green).
- [x] Account-scoped SECRET loading wired for **NMI** (`LoadNMIWebhookSigningSecretForAccount`) and **Stripe** (`LoadStripeCredentialsForAccount`) via `providerAccountSecretScopeByAccountID`; both reject an unknown account (tested: `internal/merchants` TestLoadNMIWebhookSigningSecretForAccount + TestLoadStripeCredentialsForAccount). STILL TODO: CCBill account-scoped path (CCBill auth is IP-allowlist + account-number match, not an HMAC secret) and payload-disambiguation single-shared-endpoint mode (CCBill `clientAccnum`, Stripe Connect `event.account`).
- [ ] Stripe managed-webhook registration (`ReconcileManagedStripeWebhook`) should register the per-account URL.
- RUNTIME / STAMPING:
- [x] DONE — the `provider_account_id` columns ALREADY existed on `subscriptions`/`payments`/`payment_methods` (no migration). Wired through domain models + create queries (sqlc regen) + repo read/write mappings. Repo `Create` chokepoint best-effort resolves the merchant's PRIMARY account for the rail and stamps it when the caller didn't set one (`resolvePrimaryProviderAccountID` + new `GetPrimaryProviderAccount` query). Behavioral test green (`internal/reconcile` TestRepoCreateStampsPrimaryProviderAccount). Per-account OVERRIDE done: `repo.WithProviderAccountID(ctx, id)` pins the routed account so a per-account webhook's records are stamped with THAT account (the NMI + Stripe webhook handlers resolve account_id→id via `ResolveProviderAccountID` and pin it); `resolveProviderAccountIDForStamp` prefers the pinned account, else the primary. Tested (pin overrides primary) in TestRepoCreateStampsPrimaryProviderAccount.
- [x] Per-merchant resolvers added: `GetPrimaryProviderAccount` (rail→primary id, for stamping) and `providerAccountSecretScopeByAccountID` (account_id→secret scope, for inbound webhooks).
- CATALOG:
- [x] Catalog push targets primary + secondary, skips legacy. `resolveProviders` (pkg/service/catalog_providers.go) resolves the primary slot as before (stores the link), then `syncSecondaryCatalogAccounts` does an idempotent best-effort find-or-create against each SECONDARY account on the rail. Adapters are account-aware via `autoCreateContext.TargetAccountID`: `stripeAdapter.stripeServiceFor` (overrides the Stripe secret to the target account) and `nmiAdapter.nmiClientFor` (selects `NMIClients[account_id]`). Config selectors `SecondaryRailKeysByType` + `FindByAccountID`. Tested: `config` TestCatalogTargetSelectors + `pkg/service` TestMobiusAdapter_AutoCreateTargetsSecondaryAccount.
- [~] REFINEMENT: secondary links are NOT persisted (checkout uses the primary; find-or-create re-discovers a secondary object by content key on takeover), so drift/verify still only cover the primary. Persisting per-account links (drift coverage for secondaries) is the broader links-BY-ACCOUNT model change — deferred. CCBill secondary push is a no-op (AutoCreate is manual/errPendingManualLink); Solana secondary (different recipient wallet) is out of scope here.
- CHECKOUT:
- [~] New payments/subscriptions are stamped with the primary `provider_account_id` via the repo chokepoint (covers the deterministic-primary requirement). Explicit checkout-time selection + secondary-fallback policy stays #288.
- TESTS:
- [x] Two NMI accounts on one merchant — each resolves its OWN webhook signing secret; unknown account → not found/rejected (`internal/merchants` TestLoadNMIWebhookSigningSecretForAccount, integration-green).
- [x] Repo create stamps the PRIMARY provider account (over a secondary) — `internal/reconcile` TestRepoCreateStampsPrimaryProviderAccount, integration-green.
- [x] Stripe per-account webhook secret — `internal/merchants` TestLoadStripeCredentialsForAccount.
- [x] Pinned account overrides primary stamping — `internal/reconcile` TestRepoCreateStampsPrimaryProviderAccount (second assertion).
- [ ] Payload-disambiguation single-endpoint mode (CCBill `clientAccnum` / Stripe Connect `event.account`) — edge-case refinement; the per-account PATH already covers multi-account routing.
- [x] Catalog push primary+secondary / legacy-skip — `config` TestCatalogTargetSelectors (routing) + `pkg/service` TestMobiusAdapter_AutoCreateTargetsSecondaryAccount (adapter targets the secondary's client).

## Acceptance Criteria

- A merchant can declare ≥2 NMI accounts (and ≥2 Stripe accounts) with roles, and OpenRails boots (no single-account error).
- Every inbound event resolves to exactly one provider account — by payload identifier where available, else by path parameter — and is verified with that account's secret (rejected under any other's). No event falls back to a default account.
- New subscriptions/payments/payment-methods carry `provider_account_id`; an inbound event resolves the originating account by id.
- Catalog push/verify touches only primary+secondary; legacy/disabled accounts are never mutated.
- Provider accounts are indexed by operator-declared `account_id` everywhere; no arbitrary config name is load-bearing.

## Relationships

- Builds on #630 (rail/gateway + provider-account model) and #592 (account_id is operator-declared; no runtime whoami).
- Uses the #518 `provider_accounts` table (role/status/promote/demote already present).
- Foundation under #288 (processor routing & fallback policy): #641 provides account identity, routing role, and stamping; #288 adds the smart selection/fallback policy on top.

---

# #639: variable-quantity-credit-products

**Completed:** yes
**Status:** DONE 2026-06-29: Cozy Creator-style arbitrary AI image credit top-ups now parse from catalog, persist to Postgres sidecars, quote spend->credits or credits->spend through the shared charge model, and deposit the quoted credits idempotently into the real ledger.

Add a first-class catalog primitive for variable-quantity credit purchases.

## Metadata

- Category: billing
- Status: done
- Passes: true

## Problem

The current catalog shape can express fixed products and fixed credit grants:

- product `image-credit-pack`
- price `$20`
- product-level grant `2,000 ai-image-credit`

It cannot express the actual desired Cozy Creator flow:

- Customer enters a dollar amount, e.g. `$20` or `$21`, and receives credits at `100 credits = $1`.
- Or customer enters a credit quantity, e.g. `2,000` or `2,100 credits`, and OpenRails computes the charge.
- The product is one top-up product, not one catalog product per pack size.
- The purchased credits should deposit into a named balance key such as `ai-image-credit`.
- Top-up credits can have their own expiry policy, distinct from monthly membership grants.

## Proposed Model

Add a variable credit purchase shape under a product, separate from ordinary fixed `prices`:

```yaml
products:
  - key: ai-image-credit-topup
    display_name: AI Image Credits
    credit_purchase:
      unit: ai-image-credit
      credit_type_key: ai-image-gen
      base_rate:
        credits: 100
        unit_amount: 1_000_000
      input_mode: spend_amount
      min_unit_amount: 1_000_000
      max_unit_amount: 500_000_000
      expires: 30d
      providers: [stripe]
```

`input_mode` is presentation metadata. Under the hood the same purchase quote should support both:

- spend amount -> credited quantity
- credit quantity -> price amount

## Tasks

- [x] Add manifest structs/validation for `credit_purchase`.
- [x] Require `unit`/credit type key, base rate, currency, min/max bounds, expiry, and provider eligibility.
- [x] Add quote logic for spend-amount input and credit-quantity input using the same rate definition.
- [x] Add checkout-completion support that deposits the quoted credit quantity after a completed payment/checkout source ID.
- [x] Make credit deposits idempotent by checkout/payment source ID.
- [x] Keep fixed product-level `credits` behavior unchanged for memberships and fixed packs.
- [x] Add quote response fields so frontends can render arbitrary top-up controls without hardcoded pack math.
- [x] Add DB-backed tests for arbitrary spend, expiry, bonus breakdown, ledger deposit, and idempotent replay.

## Design Review & Refinement (2026-06-29)

Sound and genuinely needed (Cozy Creator "enter any $ amount → credits"), with three fixes — all of which fold this onto the shared pricing grammar proposed in #638 instead of a new parallel one:

1. **Reuse the #638 `per_unit` charge model — don't invent a separate rate field.** `base_rate: {credits: 100, unit_amount: 1_000_000}` IS a per-unit price: `unit_amount` per credit (10_000 micros/credit) with a `divide_by: 100` block (or its inverse). A variable top-up = a `per_unit`-priced product whose deliverable is a credit DEPOSIT (pay-in-advance), versus #638's metered-usage rating (pay-in-arrears) — same math, different delivery. Express it with #638's `{model: per_unit, unit_amount, divide_by, round}` so OpenRails has ONE pricing vocabulary, not three (`metered{rate,per_units,per}`, `credit_purchase{base_rate}`, and #640's `volume_tiers`).
2. **Specify the rounding policy (currently missing).** $20.005, or 2,001 credits at 100/$1, don't divide evenly. Pick and document a rule — recommend floor-credits / ceil-price (never grant more value than paid) — using the same `round` knob as #638. Without it the quote is ambiguous and the spend→credits and credits→price directions can disagree by a credit/micro.
3. **Example is missing `currency`** (the task list mentions it; the YAML block omits it). Add it.

Keep the separate `credit_purchase` block (advance purchase is a distinct flow from arrears rating), but make its rate the #638 per-unit primitive. The min/max here bound the INPUT (spend or quantity) — name that explicitly (e.g. `input_min`/`input_max`) so it isn't confused with #638's output-side `minimum_amount`/`maximum_amount` (charge floor/cap).

## Implementation Progress (2026-06-29)

Manifest + quote engine DONE (shared with #638), unit-tested + green. `credit_purchase` parses/validates (`pkg/catalog/ratecard.go`, `ratecard_test.go`), reuses the #638 per_unit/graduated charge model; `QuoteUnitsForSpend` does spend→credits and `ChargeModel.Rate` does credits→spend. DB Tier 2 DONE: migration `046_catalog_rate_cards.up.sql` adds `catalog_credit_purchases`; `pkg/catalog/applier_service.go` + `pkg/service/catalog_sidecars.go` persist it; `internal/modules/money/credit_purchase.go` quotes and deposits the resulting custom-credit balance idempotently on checkout/payment source ID. Integration proof: `TestCatalogCreditPurchase_QuotesBonusCreditsAndDepositsLedgerBalance`.

---

# #640: credit-purchase-volume-bonuses

**Completed:** yes
**Status:** DONE 2026-06-29: Bonus-credit / discount presentation is derived from the same graduated tiered credit price used by #639, with quote output carrying paid/base/bonus/total/effective-rate fields.

Add volume bonus/discount rules to variable credit purchases.

## Metadata

- Category: billing
- Status: done
- Passes: true

## Problem

Cozy Creator wants one top-up model that supports both merchant presentations:

- If the customer enters dollars, show bonus credits: "$20 gets 2,200 credits".
- If the customer enters credit quantity, show a discount: "2,200 credits costs $20".

Those are the same pricing rule expressed from opposite directions. OpenRails should store one canonical set of volume tiers and quote either display mode from it.

## Proposed Model

Attach volume rules to `credit_purchase`:

```yaml
credit_purchase:
  unit: ai-image-credit
  base_rate: {credits: 100, unit_amount: 1_000_000}
  volume_tiers:
    - min_unit_amount: 20_000_000
      bonus_percent: 10
    - min_unit_amount: 50_000_000
      bonus_credits: 2_000
```

The quote engine canonicalizes the result as:

- paid amount
- base credits
- bonus credits
- total credits
- effective price per credit

The frontend can phrase that as either bonus credits or discounted price.

## Tasks

- [x] Use graduated tiered charge prices for variable credit purchases instead of a parallel bonus DSL.
- [x] Support bonus/discount presentation by deriving base credits, bonus credits, total credits, paid amount, and effective rate from the quote.
- [x] Define precedence through graduated tier math, avoiding volume cliffs for credit purchases.
- [x] Keep thresholds canonical in the shared price model, not a separate spend-vs-quantity rule.
- [x] Return quote breakdown fields for paid amount, base credits, bonus credits, total credits, and effective rate.
- [x] Preserve paid-vs-bonus explanation in the quote and deposit description; durable credit lots remain one FIFO grant in the current ledger schema.
- [x] Leave refund/reversal to existing grant/ledger reversal work; no separate bonus ledger subsystem was added.
- [x] Add tests for tiered bonus quote and idempotent deposit.

## Design Review & Refinement (2026-06-29)

The "one rule, two presentations (bonus credits vs price discount)" insight is correct — keep it. But the `volume_tiers: {bonus_percent | bonus_credits}` parameterization is the weak part and should be **replaced with standard volume/graduated tiers on the per-credit price** — the same `tiered {mode: volume|graduated, tiers:[{up_to, unit_amount, flat_amount}]}` charge model #638 already needs:

1. **Bonuses ARE tiered pricing.** "$20 → 2,200 credits" is just a better $/credit rate above a threshold. Store ONE volume/graduated tier table on the credit unit price and derive BOTH "bonus credits" and "% discount" as display projections of it. This unifies #638/#639/#640 on one primitive rather than adding a fourth bespoke DSL.
2. **The bespoke bonus model has an inversion bug — this is the main reason to switch.** Task "thresholds on spend or quantity?" is the symptom. With fixed `bonus_credits` tiers keyed on spend, the spend→credits function is DISCONTINUOUS at each threshold, so the inverse direction (customer enters a credit quantity → price) is non-monotonic / ill-defined near a threshold: two nearby quantities can map to the same or inverted prices, and exact-threshold quotes become arbitrary. A monotonic volume/graduated unit-price table makes the inverse well-defined by construction — which the spec REQUIRES, since both entry directions must agree.
3. **Choose volume vs graduated deliberately.** "highest eligible threshold wins" = **volume** mode (whole quantity repriced at the landed tier), which creates cliffs (buy 1 more credit and the TOTAL price can drop). If that UX is undesirable for top-ups, use **graduated** (marginal) tiers. State the choice explicitly; #638's appendix covers both.
4. **Scope — likely fold into #639.** Once #638 ships the tiered charge model and #639 ships `credit_purchase`, this issue collapses to "let `credit_purchase` use tiered mode" — a thin feature, not a standalone bonus subsystem. Consider merging #640 into #639 to avoid tracking a parallel volume engine. Keep the genuinely useful parts: the canonical quote breakdown (paid / base / bonus / total / effective rate) and storing paid-vs-bonus split in ledger metadata for refunds.

## Implementation Progress (2026-06-29)

Folded into #638/#639 as recommended — the volume discount IS the shared `tiered` charge model on the credit price (no separate bonus engine). `validateCreditPurchase` enforces graduated-only for credit purchases, so the spend↔credits quote stays monotonic and the original `bonus_credits`-tier inversion bug is structurally prevented (tested in `ratecard_test.go: TestCreditPurchase_RejectsVolumeMode`). Runtime quote breakdown DONE in `internal/modules/money/credit_purchase.go`; DB-backed proof in `TestCatalogCreditPurchase_QuotesBonusCreditsAndDepositsLedgerBalance`.

---

# #638: realistic-resource-metering-catalog

**Completed:** yes
**Status:** DONE 2026-06-29: The realistic resource-metering/rate-card model is implemented additively: manifest structs/validation, DB sidecars, applier persistence, runtime invoice rating, dimensional matrix pricing, commitments/caps, flat and accrued allowances, and integration tests are in place.

Redesign OpenRails catalog metering so it can model rented datacenter resources such as droplets, GPUs, block storage, object storage, and bandwidth without pretending that each provisioned resource is a product access grant or entitlement.

## Metadata

- Category: billing
- Status: done
- Passes: true

## Problem

The current catalog metered-price shape is too small:

- `products[].prices[].metered` is only `{meter, rate, per_units, per}`.
- `RateMeteredUsageFromEvents` aggregates usage by `event_type == meter_key` and one dimension named after that same meter key.
- The model has no first-class rate-card dimensions, filter pricing, included allowances, overage pools, minimum charge, capped monthly charge, rating formula, or catalog version.
- A DigitalOcean-like product is not "user owns droplet-vcpu forever/for 30d". The host owns resource inventory and lifecycle. OpenRails should rate measured resource usage.

This is enough for simple API calls or host-prepriced usage, but it is not enough for real infrastructure billing.

## Research Notes

DigitalOcean-like billing facts:

- CPU/GPU Droplets are billed per second with a minimum charge of 60 seconds or $0.01, whichever is higher.
- Billing starts when the Droplet is created and ends when it is destroyed; powered-off Droplets still bill because reserved resources remain allocated.
- GPU Droplets follow the same per-second/minimum model.
- Droplet bandwidth is not a naive per-resource `bandwidth-gb` SKU. Each plan contributes included outbound transfer allowance, the allowance accrues per second, it is capped at 2,419,200 seconds/28 days per monthly billing cycle, and it is pooled at the team level. Overage is $0.01/GiB.
- Volumes block storage is provisioned-capacity billing with hourly/monthly caps, e.g. 100 GiB at about $0.015/hour or $10/month, not a durable product-access grant.
- Spaces-style object storage combines a base subscription, included storage/egress allowances, pooled bucket usage, hourly proration, and overage rates.

Other billing/catalog systems:

- Lago models plans as pricing/billing/access/invoicing packages assigned to subscriptions. Usage is driven by billable metrics; charges can be filtered by metric dimensions, and metrics can be recurring or reset per period.
- OpenMeter separates events, meters, features, rate cards, prices, entitlements, plans, plan versions, and subscriptions. Meters support aggregation types such as sum, count, unique count, latest, min, and max, plus `groupBy` dimensions. Pricing supports per-unit, tiered, package, overage, flat-fee, and dynamic cost-based models.
- Kill Bill catalogs separate products, plans, phases, recurring/fixed prices, and usage sections. The common pattern is still catalog/rate-card + usage records + invoice rating, not one product row per concrete VM.

Useful references:

- DigitalOcean Droplet pricing: https://docs.digitalocean.com/products/droplets/details/pricing/
- DigitalOcean bandwidth billing: https://docs.digitalocean.com/platform/billing/bandwidth/
- DigitalOcean Volumes pricing: https://www.digitalocean.com/pricing/volumes
- DigitalOcean Spaces pricing: https://docs.digitalocean.com/products/spaces/details/pricing/
- Lago plan overview: https://getlago.com/docs/guide/plans/overview
- Lago charges with filters: https://getlago.com/docs/guide/plans/charges/charges-with-filters
- Lago recurring vs metered metrics: https://getlago.com/docs/guide/billable-metrics/recurring-vs-metered
- OpenMeter metering overview: https://openmeter.io/docs/metering/overview
- OpenMeter meter creation: https://openmeter.io/docs/metering/guides/creating-meters
- OpenMeter product catalog overview: https://openmeter.io/docs/product-catalog/overview
- OpenMeter pricing models: https://openmeter.io/docs/product-catalog/pricing-models
- Kill Bill catalog examples: https://docs.killbill.io/latest/catalog-examples

## Proposed Model

Keep the host/resource boundary simple:

- Host app owns concrete resources: droplet IDs, volume IDs, power state, resize history, region, image, network interfaces, lifecycle timestamps, and authorization.
- OpenRails owns finance-grade billing: catalog rate cards, meter definitions, usage event ingestion, rating, invoice lines, credits, receivables, and provider collection.

Replace the current one-rate-per-meter shape with:

- `meters`: define event type, value extraction, aggregation, unit, optional group-by dimensions, and late-data/idempotency rules.
- `features`: billable/limitable things like `droplet-runtime`, `block-storage`, `public-egress`, `snapshot-storage`.
- `rate_cards`: attach one feature/meter to pricing rules inside a product/plan. Rate cards can carry filters such as `{size_slug: s-1vcpu-1gb, region: nyc3}` and a default fallback.
- `prices`: support flat recurring fees, per-unit usage, tiered/graduated/volume pricing, package pricing, dynamic pass-through/markup, minimum charges, maximum charges/monthly caps, and included allowances.
- `allowances`: model included usage that can be per subscription, per active resource, or accrued by resource-runtime and pooled by customer/team.
- `catalog_versions`: preserve old pricing for existing subscriptions while allowing new catalog pushes to change future rates.

Example shape to aim for:

```yaml
meters:
  - key: droplet-runtime
    event_type: droplet.lifecycle
    aggregation: sum
    value: $.seconds
    unit: second
    group_by:
      size_slug: $.size_slug
      region: $.region
      resource_id: $.droplet_id

  - key: public-egress
    event_type: network.egress
    aggregation: sum
    value: $.bytes
    unit: byte
    group_by:
      region: $.region

plans:
  - key: droplets
    rate_cards:
      - feature: droplet-runtime
        meter: droplet-runtime
        prices:
          - filters: {size_slug: s-1vcpu-1gb}
            unit_amount: 6_000_000
            per: 30d
            prorate: second
            minimum_duration: 60s
            minimum_amount: 10_000
            max_amount: 6_000_000
      - feature: public-egress
        meter: public-egress
        allowance:
          source: droplet-runtime
          value: $.included_transfer_bytes
          accrual: per_second
          pool: customer
        overage:
          unit_amount: 10_000
          per_units: 1_073_741_824
```

The exact YAML can change during implementation; the important part is the model: metric dimensions and rate cards, not fake ownership rows.

## Implementation Plan

**Tasks:**
- [x] Remove or clearly mark the DigitalOcean example as executable only once the real rate-card model lands. Updated: the example now states loader + DB apply + invoice sweeps rate it.
- [x] Add catalog structs for meters with `event_type`, `aggregation`, `value_property`, `unit`, and `group_by`.
- [x] Add rate-card structs decoupled from product ownership/access grants.
- [x] Support filtered pricing by meter dimensions, with validation that filters refer to declared `group_by` keys.
- [x] Support minimum amount and max amount per billing period so Droplet-style per-second billing can enforce $0.01 floors and monthly caps.
- [x] Support included allowances and overage rating, including pooled customer allowances in the rated period.
- [x] Support accrued allowances derived from resource-runtime usage so Droplet bandwidth pools accrue per resource and cap at the monthly-cycle limit.
- [x] Persist rate-card sidecars as immutable-ish catalog rows keyed by product/ordinal for current catalog apply. Full catalog version pinning remains a separate subscription-versioning problem.
- [x] Update usage-event rating so invoice sweeps select a rate card by meter plus dimensions, not only by `event_type == meter_key`.
- [x] Keep concrete resource lifecycle outside OpenRails; tests use host resource IDs only as usage metadata/source IDs.
- [x] Add integration tests for matrix SKU billing, monthly caps, accrued pooled bandwidth allowance, overage, sidecar apply, and idempotent invoices.
- [x] Add loader validation tests that reject ambiguous rate cards and invalid filters/allowance sources.
- [x] Update `config/catalog.example.yaml` to show a realistic DigitalOcean-like catalog that the model can execute.

## Acceptance Criteria

- A host can provision 3 droplets of the same SKU and 2 of another SKU, report lifecycle/usage events, and receive correct invoice lines without any product-access grants.
- A powered-off resource continues billing until a destroy event stops its runtime interval.
- A resized resource bills old and new SKUs for their respective intervals.
- A 30-second droplet bills the documented minimum; a full-month droplet caps at the monthly SKU amount.
- Customer/team bandwidth overage is billed only after pooled accrued allowance is consumed.
- Re-finalizing the same invoice window stays idempotent.
- Catalog examples only show behavior that the code and tests actually support.

## Research Appendix — verified 2026-06-29 (four-source deep dive)

Primary-source verification of the Research Notes above (DigitalOcean docs; Stripe/Lago/OpenMeter/Orb/Metronome docs + source). Where this disagrees with the notes above, the figure here is the corrected one.

### DigitalOcean — corrected billing mechanics

- **Compute is per-second, capped at a monthly DOLLAR amount.** The advertised monthly price IS the cap; the per-second rate = `monthly ÷ 672`, where **672 = 24h × 28d** is a FIXED constant (not the calendar month's hours). A resource up all month hits the dollar cap before month-end (real months are 720–744h > 672), so the advertised monthly price holds. Verified against published tiers: $4/mo→$0.00595/h, $6→$0.00893, $12→$0.01786, $18→$0.02679 (each = monthly ÷ 672, matching the rates already in the example file).
- **Minimum charge:** 60 seconds, or $0.01, whichever is higher.
- **Powered-off Droplets still bill** (resources stay reserved); only DESTROY stops the clock. Billing runs create→destroy.
- **Volumes:** **$0.10 / GiB / month** (100 GiB = $10/mo), prorated hourly, billed attached or not. (The earlier "~$0.015/hour" note was the per-GiB hourly slice = $0.10 ÷ 672; the rate to model is $0.10/GiB-mo.)
- **Bandwidth:** ingress free, egress only. Each Droplet plan bundles 500 GiB–11 TiB/mo; the allowance **accrues per second, caps at 2,419,200 s (= 28 d) per cycle, and is POOLED at the team level**; overage **$0.01/GiB**, no rollover.
- **Spaces:** **$5/mo** base incl. 250 GiB storage + 1,024 GiB egress (pooled across buckets); storage overage **$0.02/GiB-mo**, egress overage **$0.01/GiB**.
- **Flat-ish monthly (billed hourly, shown monthly):** Managed DBs from $15/mo; Load Balancers $12/mo (regional HTTP) / $15 (network, global) per node; Snapshots $0.06/GiB-mo; reserved IPv4 unattached $5/mo (free attached; IPv6 free).

### The structural lesson: every system separates THREE layers

The current `metered: {meter, rate, per_units, per}` fuses three independent concerns into one record. Stripe, Lago, OpenMeter and Orb all keep them apart:

**1. Meter (event → quantity) — aggregation lives HERE, not on the price.**
- Aggregation sets: Stripe sum/count/last · Lago count/sum/max/unique_count/latest/**weighted_sum**/(custom) · OpenMeter sum/count/avg/min/max/unique_count/latest · Metronome count/sum/max/latest.
- Value extraction: a property/JSONPath (`event_payload_key` / `field_name` / `valueProperty`).
- Dimensions: `group_by` (OpenMeter) / metric `filters` (Lago) — the substrate for dimensional pricing.
- **Gauge:** only Lago integrates time natively (`weighted_sum_agg` + `weighted_interval` → GiB-seconds prorated by time-in-effect). OpenMeter has NO engine-side integration: emitters HEARTBEAT (periodically emit unit-seconds) and SUM. → design fork below.

**2. Price (quantity → money) — a discriminated union; names shared across systems:**
- `flat` — fixed recurring/one-time fee (OpenMeter FlatPrice; Lago plan amount). e.g. Spaces $5/mo, LB $12/mo.
- `per_unit` — `unit_amount` × qty, with an optional **divisor**: Stripe `transform_quantity {divide_by, round: up|down}` — divide, round, then multiply. **This is the principled `per_units`.**
- `tiered {mode: volume|graduated, tiers:[{up_to, unit_amount, flat_amount}]}` — graduated = slice & sum; volume = whole qty at the landed tier (Stripe tiers_mode; OpenMeter TieredPrice.Mode; Lago graduated/volume; Orb tiered/bulk). A tier may carry BOTH a per-unit and a flat amount.
- `package {package_size, package_amount, free_units}`, round-up — block pricing (Lago/Orb/OpenMeter). = per_unit + divisor(round: up).
- `dynamic {multiplier}` — event already carries the cost; apply markup/passthrough (Lago/OpenMeter).
- **Commitments `{minimum_amount, maximum_amount}`** (OpenMeter Commitments; Lago min_amount true-up + plan minimum_commitment). `maximum_amount` IS DO's **monthly cap**; a per-charge `minimum` ≈ DO's $0.01 minimum charge.
- **Free allowance / included units** before overage (OpenMeter usage-discount; Lago free_units). DO's bundled transfer/storage.

**3. Dimensional rate cards (price keyed on meter dimensions).** Orb `matrix`/dimensional pricing keys a unit price on a tuple of event properties (`region × instance_type`) with a default cell. → ONE `droplet-runtime` meter + a matrix price over `size_slug` replaces the per-SKU-meter explosion. (The current loader even FORBIDS sharing a meter across prices — "meter is used by multiple metered prices" — which is what forces today's per-SKU meters.)

### Why `per_units: 1` is meaningless — and the fix

Current rating: `cost = aggregate × rate / denom`, where `denom = per_units` (counter) or `per_units × per_seconds` (gauge). A gauge therefore has TWO divisors (`per_units` AND `per`) multiplying into one denominator, so `per_units: 1` is vestigial — the real normalizer is `per`. The fix is Stripe's single `transform_quantity`: one `divide_by` + explicit `round`. Then:
- counter "$/hour from seconds" → `divide_by: 3600`.
- gauge "$/GiB-month from GiB-seconds" → `divide_by: 2_592_000` (30 d) — ONE divisor, no `per_units`.
- **The counter/gauge `kind` dissolves**: it only ever encoded (a) the aggregation choice (sum-of-counts vs sum-of-unit-seconds — now `aggregation` on the meter) plus (b) a time divisor (now `divide_by`).

### Keep micros

Stripe uses cents (+ `unit_amount_decimal`, ≤12 dp, for sub-cent). Lago/OpenMeter use decimal strings. OpenRails micros (1e6/unit) already represent DO's sub-cent rates exactly ($0.00595/h = 5_950 micros) — keep it; strictly better than cents, on par with decimal strings for this domain.

### Design fork to decide: gauge integration

- **A — native time-weighting** (today's gauge): host emits level samples/unit-seconds; OpenRails integrates over the period (Lago `weighted_sum`).
- **B — host pre-integrates** (OpenMeter heartbeat): host emits unit-seconds via the EXISTING `dimensions[meter_key]` convention; OpenRails just SUMs. `RateMeteredUsageFromEvents` already SUMs `dimensions->>meter_key`, so B is nearly free and deletes the gauge special-case.
- **Recommend B**: gauge becomes "sum of unit-seconds + `divide_by` time" — same result as today's sweep, less code.

### Must-haves vs nice-to-haves for THIS use case

DO's real catalog needs only a SUBSET of the union: `flat`, `per_unit` + `divide_by`/`round`, `commitments {min,max}` (the cap + min-charge), included **allowance** + **pooled overage**, and **dimensional/matrix** SKU pricing. `tiered`/`package`/`dynamic` are for other domains (analytics, AI passthrough) — design the union so they slot in, but they aren't required to model DigitalOcean.

### Full-fidelity DigitalOcean catalog in the proposed model (target shape)

```yaml
meters:
  - key: droplet-runtime          # host heartbeats uptime
    event_type: droplet.usage
    value: $.seconds
    aggregation: sum
    group_by: { size_slug: $.size_slug, region: $.region, resource_id: $.droplet_id }
  - key: volume-storage           # host heartbeats GiB held × interval
    event_type: volume.usage
    value: $.gib_seconds
    aggregation: sum
    group_by: { resource_id: $.volume_id }
  - key: public-egress
    event_type: network.egress
    value: $.bytes
    aggregation: sum
    group_by: { resource_id: $.droplet_id }

products:
  - key: droplet
    display_name: Droplet
    rate_cards:
      - meter: droplet-runtime
        price:
          model: per_unit
          divide_by: 3_600         # seconds -> hours
          round: up
          minimum_amount: 10_000   # $0.01 / 60s minimum charge
          matrix:                  # one meter, priced per SKU
            dimension: size_slug
            cells:
              s-1vcpu-512mb: { unit_amount: 5_950,  maximum_amount: 4_000_000 }   # $0.00595/h, cap $4/mo
              s-1vcpu-1gb:   { unit_amount: 8_930,  maximum_amount: 6_000_000 }   # cap $6/mo
              s-1vcpu-2gb:   { unit_amount: 17_860, maximum_amount: 12_000_000 }  # cap $12/mo
      - meter: public-egress
        allowance:                 # bundled transfer, accrued by runtime, pooled per team
          accrue_from: droplet-runtime
          per_second_bytes: $.included_transfer_bytes_per_second
          cap_seconds: 2_419_200   # 28-day accrual cap
          pool: customer
        price:
          model: per_unit
          unit_amount: 10_000      # $0.01 / GiB overage
          divide_by: 1_073_741_824 # bytes -> GiB
          round: up

  - key: volume
    display_name: Block Storage Volume
    rate_cards:
      - meter: volume-storage
        price:
          model: per_unit
          unit_amount: 100_000     # $0.10 / GiB-month
          divide_by: 2_592_000     # GiB-seconds -> GiB-months (30d)
          round: down

  - key: spaces
    display_name: Spaces Object Storage
    rate_cards:
      - price: { model: flat, amount: 5_000_000, per: 30d }          # $5/mo base
      - meter: spaces-storage
        allowance: { included_gib_months: 250 }
        price: { model: per_unit, unit_amount: 20_000, divide_by: 2_592_000, round: down }  # $0.02/GiB-mo
      - meter: spaces-egress
        allowance: { included_gib: 1_024 }
        price: { model: per_unit, unit_amount: 10_000, divide_by: 1_073_741_824, round: up } # $0.01/GiB

  - key: load-balancer
    display_name: Regional Load Balancer
    rate_cards:
      - price: { model: flat, amount: 12_000_000, per: 30d }         # $12/mo per node
```

YAML keys are illustrative; the load-bearing parts are: aggregation on the meter, a `divide_by`+`round` per-unit divisor that subsumes `per_units`/`per`, `minimum_amount`/`maximum_amount` for DO's floor+cap, `allowance`+`pool` for bundled/pooled transfer, and `matrix` for per-SKU pricing off one meter.

### Done in the working tree now

Within the CURRENT loader (validated by `pkg/embedded/catalog_push_test.go::TestExampleCatalogManifestParses`), cleaned `config/catalog.example.yaml`'s `digital-ocean` section: removed the four vestigial `per_units: 1` lines (the loader defaults PerUnits=1, so this is behavior-preserving), annotated each rate with its real June-2026 DO figure, and added a header comment stating what the flat-rate model cannot yet express, pointing here. Full-fidelity rewrite waits on the model above.

### Sources

DigitalOcean: docs.digitalocean.com {droplets,volumes,spaces,snapshots}/details/pricing, platform/billing/bandwidth, the per-second-billing blog. Stripe: docs.stripe.com/api {billing/meter, prices/object}, subscriptions/pricing-models/tiered-pricing, transform-quantities. Lago: docs.getlago.com/guide {billable-metrics/aggregation-types, plans/charges/charge-models/*, plans/commitment, plans/charges/prorated-vs-full}. OpenMeter: openmeter.io/docs + source openmeter/{meter/meter.go, productcatalog/price.go, productcatalog/ratecard.go}. Orb: docs.withorb.com {product-catalog/price-configuration, api-reference/price/create-price}. Metronome: docs.metronome.com core-concepts.

## Implementation Progress (2026-06-29)

Tier 1 (manifest model + pricing engine) DONE; Tier 2 (DB apply + runtime rating + credit-purchase deposit) DONE and green.

**Done (additive — legacy `metered{rate,per_units,per}` still validates, no existing catalog/test broke):**
- `pkg/catalog/pricing.go` — shared charge-model engine: `ChargeModel.Rate` (flat / per_unit with `divide_by`+`round` / tiered volume|graduated / package / dynamic + `minimum_amount`/`maximum_amount` commitments) and `QuoteUnitsForSpend` (monotonic inverse for credit quotes). Integer-micros, rate-once via big.Int. `pricing_test.go` covers pro-rate, rounding, cap/floor, volume vs graduated, package, dynamic, bidirectional quote.
- `pkg/catalog/ratecard.go` — YAML model: `Meter` aggregation/value_property/group_by (additive beside legacy `kind`); `RateCard`/`RatePrice`/`RateTier`/`Matrix`/`MatrixCell`/`Allowance`/`CreditPurchase`; `ToChargeModel`/`ChargeModelForCell`; validation (`validateRateCardModel`): one-meter-per-card, matrix dimension ∈ group_by, usage card requires `billing_cadence`, allowance.accrue_from exists, credit_purchase graduated-only. `ratecard_test.go` covers all.
- `load.go` — `validateMeters` accepts aggregation meters; `validate()` calls `validateRateCardModel`.
- `config/catalog.example.yaml` — digital-ocean + cozy in the new model, PARSES + VALIDATES; `pkg/embedded` example test rewritten to assert the design end-to-end (matrix month-cap → $6/mo; $20 → 2000 credits) + legacy surfaces. Stale `load_test`/integration assertions fixed.
- Audit (gap-analysis vs OpenMeter/Lago) applied: dropped `weighted_sum` (gauge = sum of heartbeated unit-seconds + divide_by); added `billing_cadence`; `value`→`value_property`; `package_amount`→`amount`.

**Completed Tier 2 (2026-06-29):**
- DB schema: `migrations/postgres/046_catalog_rate_cards.up.sql` extends `catalog_meters` with rate-card meter metadata and adds `catalog_rate_cards` + `catalog_credit_purchases`.
- Applier: `pkg/catalog/applier_service.go` maps manifest `meters`, `rate_cards`, and `credit_purchase` blocks into `pkg/service/catalog_sidecars.go`; legacy `catalog_price_metered` still persists for existing catalogs.
- Runtime rating: `internal/modules/money/metered_rating.go` sweeps new rate cards before invoice finalization, aggregates usage by meter metadata/group_by dimensions, selects matrix cells, applies divisor rounding, commitments/monthly caps, flat allowances, and accrued per-resource allowances capped by the allowance window.
- Credit purchase: `internal/modules/money/credit_purchase.go` quotes spend or credit quantity, returns paid/base/bonus/total/effective fields, and deposits the quoted custom-credit balance idempotently by checkout/payment source ID.
- Integration tests: `pkg/service::TestSyncCatalogSidecars_PersistsRateCardsAndCreditPurchases`, `internal/modules/money::TestFinalizeInvoice_RatesCatalogRateCardsWithMatrixCapAndAllowance`, and `internal/modules/money::TestCatalogCreditPurchase_QuotesBonusCreditsAndDepositsLedgerBalance`.
- Legacy hard-cut: intentionally not removed in this patch; legacy `metered{rate,per_units,per}` remains additive compatibility for existing catalogs/tests.

**Verification 2026-06-29:**
- `go test ./pkg/catalog ./pkg/service ./internal/modules/money ./migrations/postgres -count=1`
- `python3` duplicate-prefix check over `migrations/postgres/[0-9]*_*.up.sql` → `ok 21 postgres up migrations`
- `go test -tags=integration ./pkg/service ./internal/modules/money -run 'TestSyncCatalogSidecars_PersistsRateCardsAndCreditPurchases|TestFinalizeInvoice_RatesCatalogRateCardsWithMatrixCapAndAllowance|TestCatalogCreditPurchase_QuotesBonusCreditsAndDepositsLedgerBalance' -count=1`

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
