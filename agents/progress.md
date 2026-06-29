<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 607

---

# #606: EPIC + sequencing — "Catalog & Money v1 launch" (#594–#604), with a plan review

**Completed:** yes — REQUESTED v1 EPIC SLICE LANDED 2026-06-29. #590/#591/#594/#596/#597/#599/#600/#601/#602/#603/#604 are complete in the worktree for the launch path.

STATUS 2026-06-29 (Codex worktree `openrails-606`): coordinated the parallel streams and landed the remaining catalog/money v1 pieces:
- #603 micros cutover across money helpers, migrations, provider/webhook/reconcile/checkout boundaries, plus catalog example magnitudes.
- #594 flat products + benefit manifest fields (`entitlements`, `credits`, `usage_limits`, `includes`), usage-limit registry validation, durable product usage-limit binding sidecars, and product credit planning.
- #599 meter registry + `metered` price validation/storage sidecars, aggregate rating math, and `AccrueMeteredAggregate` into the existing owed/invoice path.
- #596 recovery metadata keys + stable benefit fingerprint stamped on Stripe-created catalog objects.
- #597 provider adoption path validated through existing `pull-provider` materialization tests: resolvable provider subscriptions map by provider plan/price links and materialize locally; unresolved/ambiguous rows stay review-only.
- #591 additive WHO-axis anchor tables + provider account owner bridge.
- #590/#600/#601/#602/#604 integrated from parallel workers.
- Validation: `go test ./internal/shared/moneyutil ./pkg/catalog ./pkg/service ./internal/modules/webhooks ./internal/modules/checkout ./internal/modules/money ./internal/reconcile ./internal/river ./internal/app ./internal/http/handlers ./migrations/postgres` and `git diff --check`.

Added 2026-06-28 (Claude review of the plan). #594–#604 are not independent — they
are one program redefining the catalog/product/price/money model, and they have a
strict dependency order that wasn't written down. They also overlapped in places.
This epic records the review + the build order so they don't get built out of sequence.

## Findings from the review
- **Micros (#603) is the FOUNDATION, not a late refinement.** #594 AND #599 already
  write their manifests in micros (`10_000_000`, `200_000`/`1_000_000`), but the shipped
  base money is cents. Building #594/#599 before #603 means designing on a unit the base
  doesn't yet use. Lock #603 first (or jointly with #594).
- **#595 (key identity) is the other foundation.** #594's manifest, #596's recovery
  envelope (`product_key`/`price_key`), and #597's adoption resolution all key on the
  immutable product `key` + price natural key #595 defines. It must land before them.
- **#604 was duplicative.** Its manifest reshape == #594 (flat `products:`) + #595
  (`key`); narrowed #604 to just tier_rank (relative/0-negative/explicit). Done.
- **#601/#602 are PARTIAL, not stale.** Catalog expression shipped (v0.71.2/v0.71.3);
  the RBO reconciliation (#601) and per-rail intro/trial CHARGE orchestration (#602)
  remain — correctly still `Completed: no` with status notes.
- **#591 (older "platform identity & billing model") needs reconciliation.** It predates
  and overlaps #594/#595 (identity/anchors) and is partly superseded by #592 (SHIPPED —
  provider-account de-conflation). Re-read #591 against #592/#594/#595; close, split, or
  rescope it rather than build it as written. (ACTION: owner to triage #591.)
- **#598 is DECIDED (keep flat entitlements ledger); its impl lives in #594.** Good — no
  separate work; #594 owns materialize-at-grant-time.

## Recommended build order
1. **Foundations (do first, ideally together):** #603 (money → micros) + #595 (product
   `key` + price natural key). #604's tier_rank rides with #595.
2. **Core model:** #594 (benefit buckets: entitlements/credits/usage_limits/ownership +
   materialize-at-grant per #598). The manifest reshape lands here.
3. **On top of the model:** #599 (metered prices — needs micros + the usage/meter pattern)
   and #596 (recovery metadata — needs product_key/price_key + benefit_fingerprint).
4. **Adoption + ops:** #597 (pull-provider adoption — needs #595 identity + #596 envelope +
   #594 benefits to derive grants) and #600 (invoice cadence — mostly independent config).
5. **#591 (platform identity — the WHO axis):** complementary to #594/#595 (the WHAT /
   catalog axis), NOT duplicative — corrected after re-reading it. North-star umbrella;
   shipped slices (#588/#589/#426 + #592 group-id keying) are done; next concrete slice =
   the customer/merchant anchor tables. Sequence independently of the catalog cluster.

## Assignment (parallel streams) — #606 itself is NOT assigned; it just drives this
- **Stream A — #603 (micros).** One owner, starts NOW, fully PARALLEL: the money UNIT
  (amounts + provider-edge rounding), independent of catalog structure.
- **Stream B — catalog-model rewrite: #595 + #594 + #604 as ONE workstream, one owner.**
  Do NOT split across people — they rewrite the SAME manifest parser + converge + identity,
  so splitting = constant merge conflicts. Inside B: #595 keys → #594 benefit model +
  manifest shape (tier_rank from #604 rides along).
- **Stream C — #591 (platform WHO axis), independent owner when prioritized:** the
  customer/merchant anchor tables. Doesn't block A/B (different axis).
- **After A + B: #596 + #599 in parallel** (recovery metadata; metered — need micros + keys).
- **After those: #597 + #600 in parallel** (adoption; invoice cadence).
- **More hands inside #594** (314 lines): once the #595/#594 manifest shape is pinned, split
  #594 into children — entitlements-materialize / credits / usage_limits / product-ownership.
  Don't split before the shape is fixed.

## Tasks
- [ ] Owner: confirm the build order + the micros/keys foundation-first sequencing.
- [x] Triage #591 (Claude): KEPT as the platform "who"-axis north-star; shipped slices marked;
      it is complementary to #594/#595 (who vs what), not superseded. Next slice = anchor tables.
- [ ] As each issue starts, pin its manifest/identity assumptions to #594/#595/#603 (one
      source of truth for the shape + the money unit + the keys).

---

# #605: Residual "org" naming cleanup — bootstrap/merchant manifests + example YAMLs (follow-up to #567)

**Completed:** yes — LANDED 2026-06-29.

STATUS 2026-06-29 (Codex): internal OpenRails money amounts now use micros. Added
`internal/shared/moneyutil` micros parse/format/conversion helpers, provider-edge
micros↔cents conversion with exactness checks for single provider charges, webhook and
reconciliation comparisons in a common micros unit, and migration `032_money_micros.up.sql`
to multiply existing money columns by 10_000. Catalog examples now use micros with `_`
separators; plan labels render micros instead of cents.

Context 2026-06-28: #567 migrated OpenRails to AuthKit's permission-group model — dropped the
`org` persona, renamed `merchants.owner_org_id`→`permission_group_id`, re-pathed `/v1/orgs/*`→
`/v1/customers/*`. But that rename stopped at the DB column + Go identifiers; the **provisioning
manifests, example YAMLs, comments, and CLI help still speak "org"**, which is now wrong
vocabulary (the "org" here IS the merchant's own permission-group, 1:1). Pure naming/docs debt —
NO behavior or schema change.

## Stale spots (verified 2026-06-28)
- `internal/bootstrap/bootstrap_manifest.go`: field `authority.bootstrap_org_slug` (Go
  `BootstrapOrgSlug`, `BootstrapOptions.BootstrapOrgSlug`, the Validate error string) + comment
  "names the merchant/backing org".
- `internal/bootstrap/merchant_manifest.go`: comments "made the `owner` of the merchant's backing
  org", "creates missing merchant/org/issuer/...", "backing AuthKit org", "creates/records no
  backing org".
- `config/bootstrap.example.yaml`: `bootstrap_org_slug:` + "merchant/backing org referenced below".
- `config/merchants.example.yaml`: comment "registered as the OWNER of the merchant's backing org".
- `cmd/openrails` `push-merchant-config` help: "(org + issuer-as-owner + secrets + profile)".
- Sweep `internal/controlplane` + `internal/bootstrap` for residual `org`/`Org` that means the OLD
  OpenRails org (NOT tensorhub's `org` persona, which is legitimately tensorhub's and stays).

## Approach
Rename to merchant / permission-group vocabulary. The field already equals the merchant slug, so:
- `bootstrap_org_slug` → `bootstrap_merchant_slug`. DECIDE: hard-cut + version bump (consistent
  with #567's no-alias style) vs. accept the old key as a deprecated alias for one release (it's
  operator-facing deploy config, so an alias is friendlier). Recommend a one-release alias.
- "backing org" / "merchant/org" comments → "the merchant's permission-group".
- help string → "(merchant group + issuer-as-owner + secrets + profile)".
- Leave tensorhub's `org` persona references intact (org is tensorhub's, not OpenRails' — #567).

## Non-goals
- No behavior/DB change — naming, comments, help, examples only.
- Catalog-side staleness is OUT of scope and already tracked: `config/catalog.example.yaml` uses
  cents (#603 cents→micros), the flat tier_groups→products shape (#604 manifest restructure), and
  lacks meters/metered prices (#599). Those issues own updating the catalog example.

## Tasks
- [ ] Rename `bootstrap_org_slug`→`bootstrap_merchant_slug` (struct field + `BootstrapOptions` +
  Validate message) in `bootstrap_manifest.go` and `config/bootstrap.example.yaml`; decide
  alias-vs-hard-cut (bump if hard cut).
- [ ] Sweep "backing org" / "merchant/org" comments in `bootstrap_manifest.go` +
  `merchant_manifest.go` → "merchant permission-group".
- [ ] Update `push-merchant-config` Short/Long help: drop "org" → "merchant group".
- [ ] Update `config/merchants.example.yaml` comments (issuer = owner of the merchant's group).
- [ ] grep `internal/controlplane` + `internal/bootstrap` for residual OpenRails-`org` identifiers/
  strings and rename; keep tensorhub's `org` persona references untouched.
- [ ] Build + vet (incl. `-tags=integration`) green; confirm zero functional/DB diff.

---

# #604: tier_rank — relative, renumber-safe, 0/negative, explicit (manifest shape owned by #594/#595)

**Completed:** yes

Proposed 2026-06-28 (owner). REVISED 2026-06-28 (Claude review): the manifest
RESHAPE this issue first proposed (a flat `products:` list keyed by an immutable
`key`, dropping the `tier_groups` / `catalogs:name` nesting) is ALREADY owned by
**#594** (its benefit-bucket manifest shows exactly that flat `products:` shape) and
**#595** (product `key` identity replacing `slug`). To avoid two competing manifest
specs, this issue is NARROWED to the one thing #594/#595 don't cover — tier_rank
semantics — and defers the shape to them. See the #606 sequencing epic.

## tier_rank: relative, renumber-safe, 0/negative allowed
- `tier_rank` lives ONLY on `products` (NOT denormalized onto subscriptions/payments,
  confirmed), and every runtime use is a RELATIVE `<`/`>`/`>=` (upgrade = higher,
  downgrade = lower). So renumbering is safe with zero data migration — insert a lower
  tier by bumping the others up, OR prepend a NEGATIVE rank without touching them.
- Make `tier_rank` an EXPLICIT manifest field (`*int`, so "omitted" ≠ "0"; the DB
  default is 0) and require it when a `tier_group` has >1 product, so two products
  don't silently collide at the default 0.
- Drop the loader's `tier_rank > 0` rule (pkg/catalog/load.go:136) — allow 0/negative.

## Flagged for #594/#595 (not owned here)
- Top-level keying: `merchants: [{slug, products}]` (explicit) vs today's
  `catalogs: [{name = merchant slug}]` (implicit) — pick one in #594/#595.
- Where a tier-group display label lives once `tier_group` is a product attribute.

## Tasks
- [x] tier_rank: `*int`, allow 0/negative, explicit-when-grouped; drop the >0 rule.
- [x] Tests: renumber + prepend-negative preserve upgrade/downgrade direction.

---

# #603: Money in micro-USD (micros) instead of cents — sub-cent precision for metered/usage pricing

**Completed:** yes — v1 CORE LANDED 2026-06-29.

STATUS 2026-06-29 (Codex): added top-level `meters` validation, `metered` price blocks
(`meter`, `rate`, `per_units`, gauge-only `per`), sidecar storage in
`034_catalog_benefits_metering.up.sql`, pure aggregate rating math that rounds once, and
`MoneyService.AccrueMeteredAggregate` into the existing `AccrueOwed` pending-invoice path.
Metered prices are OpenRails-native/DB-only and must not inherit external providers.

Proposed 2026-06-28 (owner). OpenRails currently stores ALL money in CENTS
(integer minor units; `moneyutil.CentsToMajorUnits = cents/100`, `prices.amount`,
`payments.amount`, the ledger — existing doujins data is `1900`/`2300` = $19/$23).
Cents cannot express SUB-CENT prices, which #599 (metered/usage rating: usage ×
rate) and fine-grained proration/discount math need — e.g. $0.0001 per unit. Move
the internal money unit to **micro-USD ("micros"; 1 USD = 1_000_000)**, the
idiomatic choice (Stripe/Google use micros). $23.00 = `23_000_000`; the catalog
YAML writes amounts with `_` digit separators for readability.

This also UNIFIES the unit. #599 (metered rating) already specifies its `rate` /
`per_units` in **micros** (e.g. `{rate: 200_000, per_units: 1_000_000}` = $0.20 /
1M requests), so leaving flat `prices.amount`/`payments` in cents would split the
system across two money units — converging on micros everywhere is the consistent
end state.

## Scope (the unit is pervasive — every money column + helper)
- Columns: `prices.amount` + `prices.initial_amount` (#602); `payments.amount` /
  `list_amount`; subscription/credit/grant amounts; `money_ledger` / ledger
  transfers; spend-limit/budget amounts; ClickHouse `payment_events`.
- `internal/shared/moneyutil`: `ParseDecimalToCents`/`FormatCentsDecimal`/
  `CentsToMajorUnits` → micro equivalents (÷1_000_000, 6 dp); add a typed
  `Micros int64` maybe.
- Catalog manifest + converge + service validation already amount-agnostic; just
  the magnitudes change.

## Provider boundary (the hard part)
NMI/CCBill/Stripe charge in **minor units (cents)**, not micros. So:
- Outbound charge: convert micros → cents with a DEFINED rounding (half-up?) at
  the provider edge; a price not representable in whole cents (e.g. $0.0001) is
  only billable via AGGREGATED metered billing (sum usage until ≥ 1¢), never a
  single sub-cent charge — document this constraint.
- Inbound (webhooks/reconciliation/migration): provider cents → micros (× 10_000);
  amount matching in reconciliation must compare in a common unit.

## Migration (value-preserving hard cut, like #588)
- One migration multiplies every existing money column by 10_000 (cents → micros);
  no value changes, only the unit. Mirror across Postgres + ClickHouse.
- doujins catalog: `2300` → `23_000_000`, `1900` → `19_000_000`, intro
  `1995`→`19_950_000` / recurring `1495`→`14_950_000`, etc.

## Tasks
- [ ] Decide precision (micros = 1e-6) + whether to introduce a `Micros` type.
- [ ] Migration: ×10_000 all money columns (Postgres + ClickHouse), value-preserving.
- [ ] Rework `moneyutil` (parse/format/convert) + every caller.
- [ ] Provider edge: micros↔cents conversion with explicit rounding; block/aggregate
      sub-cent single charges; reconciliation matches in a common unit.
- [ ] Update catalog YAML (doujins) to micros with `_` separators.
- [ ] Tests: round-trip, provider rounding, reconciliation, sub-cent metered (#599).

---

# #602: Introductory & trial pricing — express "$X (or $0 trial) first period then $Y recurring"

**Completed:** yes

Proposed 2026-06-28 (owner). OpenRails prices currently model a SINGLE flat
recurring amount (`amount` + `billing_cycle_days`). Real provider offers commonly
have a DIFFERENT first/initial period than the recurring price. Two shapes to
support, which are the same primitive (an initial period at its own price/length):
- **Intro / step-down**: a different first-period PRICE — e.g. doujins legacy
  CCBill plan `0000000931`: **$19.95 for the first 30 days, then $14.95 every 30
  days thereafter**.
- **Free trial**: a first period at **$0** for a (usually shorter) length — e.g.
  **$0 for 7 days, then $15 every 30 days**. Stripe models this natively
  (`trial_period_days` / a trial phase then the recurring price), so the catalog
  should express it portably.

Today we can only approximate either as a flat recurring amount, losing the
initial-period semantics; migrated subscribers and new checkouts can't faithfully
express or reconcile them.

## Goal
Model a recurring price whose FIRST billing period differs from subsequent ones:
- initial amount + initial period length
- recurring amount + recurring cycle
- so a charge path bills the intro amount once, then the recurring amount on each
  rebill, and reconciliation can match a provider subscription on either rate.

## Shape (sketch, not final)
Extend the price with an optional intro/trial block:
```
amount: 1495            # recurring (the steady-state rate)
billing_cycle_days: 30
intro:
  amount: 1995          # first-period price ($0 for a free trial)
  period_days: 30       # first-period length (CCBill "initial period"; e.g. 7 for a trial)
  periods: 1            # how many initial periods at the intro rate (usually 1)
```
A flat price omits `intro` (today's behavior unchanged). A free trial is the same
block with `amount: 0` (e.g. `amount: 0, period_days: 7`). Validation must allow a
$0 initial amount (distinct from "no intro").

## Provider mapping
- **CCBill**: native — a Recurring Billing Option has distinct initial price/period
  vs recurring price/period (see #601). Map intro -> initial, recurring -> recurring;
  $0 initial = a trial.
- **Stripe**: native — `trial_period_days` for $0 trials, and a first invoice /
  schedule phase for a non-zero step-down. Map `intro` onto whichever Stripe
  primitive fits ($0 -> trial, non-zero -> first-phase price).
- **NMI/mobius**: flat-only in practice (doujins NMI plans `premium`/`premium_new`
  are flat $19/$23); express intro via an add-on/one-time first charge or leave
  unsupported per rail.
- **Solana**: evaluate.

## Status 2026-06-28 (Claude): EXPRESSION shipped (v0.71.3) — a price can carry an
## intro/trial first period (initial_amount/initial_period_days), end-to-end:
## migration 031, model GetIntro(), manifest `intro:` block, converge, service
## validation ($0 trial allowed). Integration-tested (intro/trial/flat round-trip).
## doujins legacy $19.95->$14.95 (CCBill RBO 0000000931) now modeled. REMAINING:
## per-rail charge orchestration (bill intro-once-then-recurring / skip the trial
## window) + reconciliation keying — not needed for the legacy CCBill case (CCBill
## bills via the RBO; openrails records), needed for openrails-native intro billing.
##
## Status 2026-06-29 (Codex): CCBill webhook charge paths now use the RBO when
## present, validate NewSale/Upgrade against initial_amount once (including $0
## trials) and RenewalSuccess against recurring amount; provider cents are
## converted at the CCBill edge to internal micros. Stripe checkout maps $0 intro
## to subscription_data[trial_end]; non-zero paid Stripe intro is rejected loudly
## until a Stripe phase/schedule implementation exists. Reconciliation maps
## CCBill provider plan ids through recurring_billing_option_id.

## Tasks
- [x] Decide storage: extend `prices` (initial_amount + initial_period_days cols, migration 031).
- [x] Manifest + catalog converge support for the intro block.
- [x] Charge paths bill intro-once-then-recurring per rail (start CCBill + Stripe).
- [x] Express `amount: 0` initial as a free trial (model + validation + round-trip test);
      the charge-side "skip the first charge for the trial window" rides on the charge task above.
- [x] Reconciliation matches a provider sub on intro/trial OR recurring rate.
- [x] Migrate doujins legacy `$19.95 -> $14.95` (CCBill RBO 0000000931) onto it.

---

# #601: CCBill catalog identity — model the Recurring Billing Option (price/product/plan id) SEPARATELY from the FlexForm

**Completed:** yes

Proposed 2026-06-28 (owner). OpenRails' CCBill catalog link models ONLY
`flex_id` + `form_name` (and `Attach` requires both). But in CCBill those are the
**FlexForm** — the hosted purchase-flow page the buyer is sent to — which is
DISTINCT from the **price/product/plan identifier**. CCBill officially calls the
latter a **"Recurring Billing Option"** (RBO): a zero-padded numeric id (e.g.
`#0000042836`) that defines the actual pricing (initial price/period + recurring
price/period). One FlexForm can present a given RBO; legacy subscribers carry an
RBO with NO FlexForm at all.

Consequence today: doujins cannot record its CCBill pricing ids in the catalog —
`$23`=`0000007498`, legacy `$19`=`0000001412`, legacy `$19.95->$14.95`=
`0000000931` — because the only CCBill keys are FlexForm fields. The migrated
billing rows have no catalog-level CCBill price identity to reconcile against.

## Goal
Treat the two as separate, independently-settable CCBill identifiers on a price:
- **Recurring Billing Option id** (the price/product/plan identity) — the canonical
  CCBill price key; required for reconciliation/recovery (#595/#596) and for
  legacy/archived tiers that have no FlexForm.
- **FlexForm** (`flex_id` + `form_name`) — the purchase-flow page; needed only for
  NEW self-service checkout, optional otherwise.

## Plan
- Add a `recurring_billing_option_id` (CCBill RBO) rail key alongside the existing
  `flex_id`/`form_name`.
- Relax `Attach` (pkg/service/catalog_provider_ccbill.go) + the price-model CCBill
  validation (internal/db/models/product_catalog.go): a CCBill link is valid with
  an RBO id alone, a FlexForm alone, or both; a FlexForm still requires BOTH
  flex_id+form_name together.
- Surface RBO in GetCCBill* accessors; webhooks/reconciliation key on the RBO id.
- doujins: hardcode `recurring_billing_option_id` 0000007498/0000001412/0000000931
  on the $23/$19/$15 prices (currently blocked on this).

## Status 2026-06-28 (Claude): catalog support SHIPPED (v0.71.2) — the RBO id is a
## first-class CCBill rail key, settable independently of the FlexForm; doujins
## hardcoded its three RBO ids. Reconciliation/webhook keying on the RBO remains.
##
## Status 2026-06-29 (Codex): webhooks prefer CCBill subscriptionTypeId/RBO over
## flexId for price lookup and validate RBO mismatches; price lookup accepts either
## flex_id or recurring_billing_option_id. Reconciliation plan indexing includes
## recurring_billing_option_id and CCBill active-member snapshots carry the RBO as
## PlanID when DataLink exposes it.

## Tasks
- [x] Add `RailKeyCCBillRecurringBillingOption` + accessor.
- [x] Relax CCBill Attach + product_catalog validation (RBO and/or FlexForm).
- [x] Reconciliation/webhook mapping keys on the RBO id.
- [x] doujins bootstrap: hardcode the three RBO ids (+ keep the $23 FlexForm).

---

# #600: Per-merchant invoice cadence + collection thresholds (#301 follow-up)

**Completed:** yes

Decision 2026-06-28: arrears collection thresholds and the invoice/sweep cadence are
hardcoded constants — `ArrearsHourlyThresholdAmount = $50` and `ArrearsMonthlyFloorAmount =
$1` in `internal/river/jobs_credit_money_in.go`, with a fixed ~30d arrears sweep interval
and a calendar-month invoice boundary. #301 closed noting "calendar arrears = future
refinement" and the code carries `TODO(#301): make these configurable per-merchant; decide
calendar-month vs fixed-interval boundary.` This issue does that. Config + plumbing only — no
new collection path; the engine (#301/#302/#303) is built.

## What
- Move the collection threshold (charge when owed ≥ X mid-cycle) and the monthly floor (skip
  dust < Y at period close) into per-merchant config, defaulting to today's $50 / $1.
- Make the billing-period boundary per-merchant: calendar-month-UTC vs fixed-interval vs
  signup anniversary. Today the #301 sweep uses fixed-30d and the #303 invoice uses
  calendar-month; unify both under one per-merchant setting so they always agree.
- Defaults preserve current behavior.

## Tasks
- [x] Per-merchant config: collection_threshold, monthly_floor, billing_period_boundary
  (calendar_month | fixed_interval | anniversary); defaults $50 / $1 / fixed_interval.
- [x] Thread config into `InvoiceWorker` (FinalizeThresholdInvoices / FinalizeDueInvoices /
  ChargeOutstanding) instead of the Arrears* constants; delete the constants.
- [x] Align the #301 sweep boundary and the #303 invoice boundary to the same per-merchant
  setting (no sweep/invoice period mismatch).
- [x] Tests: custom threshold/floor honored; calendar-month vs fixed-interval boundary;
  defaults reproduce current behavior; sweep + invoice periods agree.

---

# #599: Catalog metered prices — OpenRails-native usage rating (usage × rate → money)

**Completed:** yes — VALIDATED 2026-06-29.

STATUS 2026-06-29 (Codex): existing `pull-provider` adoption/materialization path is the
v1 implementation: provider snapshots reuse the shared fetchers, resolve plan/price through
catalog provider links (`plan_id`, `price_id`, `recurring_billing_option_id`), require
deterministic identity, create local subscription/payment materialization actions in enforce
mode, and leave unresolved/ambiguous rows review-only. Revalidated with `internal/reconcile`
tests.

Decision 2026-06-28: today OpenRails is amount-agnostic — the host prices every usage event
and OpenRails only banks it (`RecordUsage` takes a host-supplied `amount`, #289). That already
supports cloud-style billing IF the app does the rating. This issue adds an OPTIONAL,
catalog-defined rate so OpenRails itself turns metered usage into money — the "$0.10/GB·month",
"$X/sec" cloud model — reusing the built accrual/invoice/charge engine (#301/#302/#303). Rating
is opt-in per price; host-priced events (#289) keep working unchanged.

## What a metered price is
Two pieces, mirroring OpenMeter (Meter) / Lago (billable metric): a first-class **meter** and a
**price** that references it. A `meter` is a top-level registry entry (same PATTERN as #594's
`usage_limits` registry, but a DISTINCT concept — see below) naming a usage stream + its `kind`
(gauge|counter). A metered `price` references a `meter` and adds the `rate` (+ `per`); `kind` is on
the meter so a stream's identity + kind are defined ONCE, referenced by one or more prices.

Meter ≠ usage_limit. A `usage_limit` (#594) is a quota GIVEN AHEAD — a granted allowance enforced
at admission (#298), prepaid-shaped, "you may use up to X." A `meter` is something BILLED LATER —
measured then rated into a charge, postpaid-shaped, "you used X, you owe $Y." They may observe the
same event stream but are independent objects with independent lifecycles; neither references the
other. (A "free tier then pay" is tiered pricing on the meter — deferred — not a usage_limit.)

```yaml
# top-level meter registry (like `usage_limits`) — names the stream + its kind, ONCE
meters:
  - key: vcpu          # gauge: a level HELD over time → time-integrated
    kind: gauge
  - key: storage_mb    # gauge: MB stored (1 GB = 1_000)
    kind: gauge
  - key: api_calls     # counter: discrete events → summed
    kind: counter

products:
  - key: compute-vm
    prices:
      - currency: usd
        metered: { meter: vcpu, rate: 50_000, per: 1h }           # $0.05/vCPU-hour
  - key: object-storage
    prices:
      - currency: usd
        metered: { meter: storage_mb, rate: 0_000_100, per: 30d }  # $0.10/GB-month
  - key: api
    prices:
      - currency: usd
        metered: { meter: api_calls, rate: 2_000 }                 # $0.002/call (counter, no per)
```

- `meter` (top-level registry, same PATTERN as `usage_limits` but a DISTINCT billing concept) = a
  named usage stream the host reports against (the #289 usage_event `event_type`) + its `kind`.
  Defined ONCE for BILLING and referenced by one or more metered prices. It is NOT a usage_limit
  (those are granted quotas, #594/#298); a meter and a limit may watch the same event stream but
  neither references the other. The running tally is per `(customer, meter)` (or
  `(customer, meter, resource)` when instance-tagged — see Host hooks).
- `kind` (on the METER, not the price — it's intrinsic to the stream) = `gauge` (a level HELD
  over time — VMs, storage; time-integrated, persists across periods) or `counter` (discrete
  EVENTS — API calls, egress; summed, resets each period). Prometheus's gauge-vs-counter; replaces
  aggregation+recurring. Rare peak/value-at-close (`max`/`latest`) deferred.
- `rate` (on the price) = micros per `per_units` meter-units (per `per` time, for gauges); ISO
  money currency ONLY (custom counters can't be arrears — #474), consistent with the ledger (#594).
- `per_units` (on the price, default 1) = the rate's UNIT denominator, so fine-grained prices stay
  clean integer micros: `{ rate: 200_000, per_units: 1_000_000 }` = $0.20 per 1M requests (an
  effective 0.2 micros/request that integer-micros can't hold directly). Quote the way clouds do.
- `per` (on the price) = the rate's TIME denominator for gauge meters — "$rate per unit per
  `per`" (VM `1h`, storage `30d`); reuses the #594 duration grammar. Omitted for counter meters
  (priced per event, not per time). It is the only time field on a price: when the invoice CLOSES
  is the customer billing cycle (#600), and how often OpenRails accrues is arbitrary/internal (the
  integral can be evaluated at any instant) — neither belongs on the price.

## Precision (micros + `per_units`)
Amounts/ledger stay micro-USD (1e-6). For typical cloud rates micros has ample headroom: 1¢/hour =
10_000 micros; $0.10/GB-month = 100 micros/MB; $0.023/GB-month = 23 micros/MB; even RAM at
~$0.007/GB-hr = 7 micros/MB. VMs/RAM/storage/bandwidth all fit comfortably. The ONLY sub-micro case
is rates per a TINY unit — per request/token/byte (e.g. $0.20/1M requests = 0.2 micros/request).
Handle it WITHOUT a finer money scale: `per_units` (default 1) quotes the rate per a unit-count
(200_000 micros per 1_000_000 requests), keeping the rate an integer ≥1 micro; and rating NEVER
rounds per event — it aggregates the whole period, computes `aggregate × rate ÷ (per_units ×
per_seconds)` in a wide int64 intermediate, and rounds to micros ONCE, so the effective fractional
per-unit rate is exact at the accrual.

## How it bills
A rating job (River) runs periodically — the frequency is INTERNAL and arbitrary, because the
integral can be evaluated at any instant ("owed so far" = integral up to now). Each run computes
the meter's aggregate since the last accrual (by the meter's `kind`): for a `counter`, the summed events; for a `gauge`,
the time-integral of the level (unit·seconds). It multiplies by `rate` ONCE (rounding once —
NEVER per event; sub-unit per-event rounding drifts), dividing by `per_units` (and, for a gauge,
by `per`_seconds): cost = aggregate × rate ÷ (`per_units` × `per`_seconds), in a wide int64
intermediate, rounded to micros ONCE. Then `AccrueOwed` (#302) the result. The
existing `InvoiceWorker` (#303) closes the invoice on the customer's billing cycle (#600) and
`ChargeOutstanding` (#301) collects. Metered prices are pure rating on top of the built engine —
no new ledger, no new collection path.

Granularity: OpenRails has no time unit — it's the app's emit cadence. Rate per-second or
aggregate a session; never write a ledger row per microsecond. Gauges ($/GB·month): the app
emits an ABSOLUTE sample (current GB + timestamp) whenever the level CHANGES — not deltas, not
per-tick polling; the rating job TIME-INTEGRATES the level over the period (`weighted_sum`) to a
time-weighted quantity, then × rate. `recurring` carries the last level into the next period so
untouched storage keeps billing. This is the Lago weighted_sum model; CloudKitty computes the
same integral by polling the gauge each collect-period and summing — push vs poll, same math.

## Gauge contract (push-on-change — OpenRails never polls)
Any resource HELD OVER TIME is a gauge — a STEP FUNCTION that changes only at discrete moments
and is constant between them: storage GB, running VMs / vCPUs, reserved IPs, provisioned seats.
A VM is the canonical case — emit `running=1` at start and `running=0` at stop, and OpenRails
bills everything in between by integrating the level over the elapsed time: TWO events, not one
per second. Per-second (or finer) granularity falls out of the breakpoint TIMESTAMPS, not the
event frequency. So OpenRails does NOT sample/poll a gauge — the host pushes an ABSOLUTE level
sample ONLY WHEN the level changes: `RecordUsage(measure, value=<current level>, amount=0)` — a
metered-only event (#289 already supports `amount=0` = recorded, not debited; the level rides a
`Dimensions` entry, so NO new ingestion primitive). The rating job reconstructs "usage at any
time" by treating the level as piecewise-constant between samples and time-integrating
(`weighted_sum`). Consequences:
- IDLE = ZERO traffic. A customer who stores 30GB and never touches it emits ONE event ever; the
  level simply persists (a gauge holds until the next change event) so it keeps billing every
  period. No 10-minute polling, no callback OpenRails invokes, no background samples with zero
  value. Cost is proportional to CHURN, not to time.
- OpenRails NEVER measures or pulls — it has no access to merchant data; the app always pushes.
  (CloudKitty polls only because Ceilometer is a generic telemetry collector; a purpose-built
  system uses push-on-change, like Lago weighted_sum.)
- Mid-life changes are just MORE breakpoints: a VM resize (2→4 vCPU) or storage growth emits a
  new level at that instant and each segment integrates at its own level. A resource still held
  at a period boundary just carries forward (a gauge holds until changed) — bill [start,
  period_end) now, continue next period until the stop/delete arrives.
- OPTIONAL drift backstop: one low-frequency reconciliation sample (e.g. a daily "current truth"
  emit) self-corrects a missed delete/stop — a crashed VM whose stop event never fired would
  otherwise bill forever. A daily "list what's currently held" emit doubles as stop-detection.
  ~1 event/day/customer is negligible and is the ONLY periodic emit; skip it if change events
  are reliable.

This is the unifying cloud model: ALL resource provisioning is a gauge — compute, storage, IPs,
seats, provisioned throughput — billed by emitting a level on each state change (start / resize /
stop) and integrating between. So the entire host contract is: (1) declare the `metered` price
ONCE; (2) emit the new level on change. OpenRails does integrate → rate → accrue → invoice →
charge.

## Host hooks (client surface)
Two methods cover everything; both are thin wrappers over #289 usage events.
- `ReportLevel(customer, meter, level)` — GAUGE meters (held resources). Sets the current ABSOLUTE
  integer level; call ONLY when it changes. OpenRails timestamps it and time-integrates between
  reports. VM lifecycle = `ReportLevel(cust, "vcpu", 4)` at boot, `ReportLevel(cust, "vcpu", 0)`
  at stop; storage = `ReportLevel(cust, "storage_mb", 1100)` whenever bytes change. (Optional
  `ChangeID` for idempotency, `OccurredAt` to override now.)
- `RecordUsage(customer, meter, quantity, sourceID)` — COUNTER meters (discrete events). Adds a
  count — one call per occurrence or a batched quantity; `sourceID` for idempotency. (This is
  today's #289 RecordUsage; the gauge form is the SAME event with `amount=0` + the level in a
  dimension.)
- Both take an optional `resource` (instance) label — the existing #289 usage_event `Resource`
  column. Tag every report for VM-1 with `resource="vm-1"` and #303 groups the invoice PER
  INSTANCE (VM-1 → its RAM/disk/bandwidth lines; VM-2 → its own). Untagged = one aggregate line
  per meter. Rate/group per `(customer, meter, resource)` when tagged (#311 ServiceUsageRollup
  already groups by resource).

Units: pick the FINEST integer unit you bill (MB for storage, vCPU for compute, 0/1 for a whole
VM); declare it as a `meter` and set the price `rate` in micros per that unit per `per`.
$0.10/GB·month → meter `storage_mb` (kind gauge, 1 GB = 1_000) + price rate 100 micros, `per: 30d`.
No fractional units — choose a fine enough integer one.

## Scope
- v1 meter `kind`: `counter` (summed events — the PRIMARY case: API calls, bandwidth/egress,
  images; what OpenMeter/Lago center on) AND `gauge` (time-integrated level — storage, RAM,
  running VMs; Lago's weighted_sum, which OpenMeter lacks). BOTH core. Peak/value-at-close
  (`max`/`latest`) deferred.
- Opt-in: a price with no `metered` block stays host-priced (#289); both coexist.
- Meter ≠ usage_limit: a `usage_limit` (#594) is a quota given AHEAD (admission #298); a `meter`
  is BILLED LATER (rated → invoiced). Separate objects, separate lifecycles — they may watch the
  same event stream but neither references the other.

## Non-goals (v1)
- Tiered / graduated / volume pricing ("first 100GB free, next 1TB at $X"). Big feature; defer
  to its own issue. v1 = one flat rate per measure.
- Per-event rating on the admission hot path — rating runs in the cadence job off the request
  path (admission stays the Redis headroom op, #298).
- Provider-hosted metered invoicing (Stripe Billing Meter) — that's the separate optional #134.

## Prior art (researched 2026-06-28)
Every open-source cloud-billing stack converges on the SAME four stages: meter → aggregate →
rate → invoice/collect.
- OpenStack (the AWS-like IaaS reference): Ceilometer/Gnocchi (meter + time-series store) →
  CloudKitty (rate: `hashmap` flat/rate mappings or `pyscripts`, run every `collect_period`,
  default hourly) → downstream billing. CloudKitty is PURELY the rater.
- Lago: usage events → billable metric (7 aggregations incl. `weighted_sum` + the `recurring`
  flag) → charge model (standard/graduated/volume/package) → invoice. Best modern reference;
  `weighted_sum` IS the storage answer (absolute samples, time-integrated).
- OpenMeter: high-volume meter (Kafka/ClickHouse, CloudEvents idempotency; sum/count/avg/
  min/max/latest) feeding Stripe/Lago — a meter, not a biller.

Ingestion design (both, researched 2026-06-28): the host fires DUMB events — payload +
idempotency key + customer + a meter/metric CODE + timestamp — and a SEPARATELY-DEFINED named
meter decides which value field to read and how to aggregate. Lago event {transaction_id,
external_subscription_id, code, timestamp, properties} + billable metric {code, aggregation,
field_name, recurring}; OpenMeter CloudEvent + Meter {slug, eventType, valueProperty JSONPath,
aggregation, groupBy}. Lessons: (1) the meter is FIRST-CLASS and reused — define `kind`/aggregation
ONCE and reference from many prices (→ promote OpenRails `measure` to a top-level meter registry,
same pattern as `usage_limits`, `kind` on the meter not the price; ADOPTED 2026-06-28). OpenRails
DEVIATES on one point: the billing `meter` is kept SEPARATE from the granted `usage_limit` quota
(#594) — billed-later vs given-ahead are distinct lifecycles, even when they watch one stream. (2)
events bucket into periods by TIMESTAMP not arrival (#289 `OccurredAt` already does this). (3)
prefer ONE event per occurrence over a pre-summed count (preserves granularity for later pricing).
(4) flow metering (SUM/COUNT) is the PRIMARY case in both → `counter` stays core; OpenMeter has NO
time-weighted gauge, so OpenRails' gauge follows Lago's `weighted_sum`.

Broader OSS survey (researched 2026-06-28) — more production systems doing exactly this:
- Apache CloudStack Usage Server (OSS IaaS, AWS-like): reads the event log → summary usage records.
  HELD resources (running VM, allocated VM, IP, volume, LB, VPN) are billed in HOURS (a gauge
  integrated over time); network bytes sent/received are ACCUMULATED bytes (a counter). Real-world
  confirmation of the gauge(hours)/counter(bytes) split and of per-hour rating ("VM-hours",
  "GB-hours"). Runs ≥1×/day.
- Kill Bill (mature OSS billing): two usage types — CONSUMABLE (sum of units = our `counter`) and
  CAPACITY (billed on the MAX / high-water-mark over the period). CAPACITY = peak pricing = our
  deferred `max`, and is LESS precise than time-weighted; Lago/CloudStack/OpenRails bill the
  time-integral, not the peak. Tiers: ALL_TIER (graduated) vs TOP_TIER (volume) — confirms tiered
  pricing is a separate, ubiquitous charge-model layer → our v1 non-goal.
- CGRateS (Go, telecom real-time charging, 50k+ req/s): rating engine + account balances +
  session/event charging WITH RESERVATION — the same shape as our admission holds (#298) + ledger.
  A production Go reference that rating-engine + reservation + balances is the right decomposition.
- Cyclops (OSS cloud RCB, ICCLab): microservice pipeline UDR (meter, pulls from OpenStack/
  CloudStack) → CDR (rate) → Billing (invoice) — the meter→rate→invoice pipeline again.
- Meteroid (Rust OSS): real-time usage without pre-aggregation; versioned plans + grandfathering
  (we get this via materialize-at-grant, #594/#598).
Takeaway: the gauge(time-integrated, billed in resource-hours)/counter(summed) split is UNIVERSAL;
time-weighted (Lago/CloudStack/OpenRails) is the precise model while max/high-water-mark (Kill Bill
CAPACITY, OpenMeter MAX) is the deferred peak-pricing variant; tiers are everywhere a separate
charge-model layer (deferred); a Go rating-engine + reservation + ledger (CGRateS) is proven.
OpenRails already has meter (#289 usage_events) + aggregate (rollups) + invoice/collect
(#301/#302/#303). #599 is exactly the missing RATING module — the CloudKitty/Lago piece. The
tiered/graduated/volume "charge model" is a distinct concern in all of them → v1 non-goal.

## Tasks
- [ ] Add a top-level `meters` registry (key + `kind` gauge|counter) for BILLING — same pattern as
  `usage_limits` but DISTINCT from it; validate URL-safe unique keys. Referenced by metered prices
  only (`kind` on the meter, not the price). Do NOT couple it to #594 usage_limits — a meter
  (billed later) and a usage_limit (granted ahead) are separate concepts that may merely watch the
  same event stream.
- [ ] Extend the #594 price model with an optional `metered` block referencing a `meter` + `rate`
  (micros) + `per` (gauge only) + `per_units` (default 1, the rate's unit denominator for sub-micro
  prices). Validate ISO money currency only, positive rate, `per` required for gauge / absent for
  counter, parseable `per`, `per_units` ≥ 1.
- [ ] Rating job (internal cadence, arbitrary), by the meter's `kind`: `counter` → sum events;
  `gauge` → time-integral of the level (unit·seconds); cost = aggregate × rate ÷ (`per_units` ×
  `per`_seconds) in a wide int64 intermediate, rounded to micros ONCE (never per event);
  `AccrueOwed`; idempotent per (customer, meter, accrual).
- [ ] Gauge = a level per `(customer, meter[, resource])` that persists until the next change event
  (no per-period reset, no `recurring` flag); time-integrate the piecewise-constant level. Document:
  app emits absolute gauge samples on change, not deltas.
- [ ] Gauge ingestion = push-on-change (NO OpenRails polling/callback): reuse #289 `RecordUsage`
  with `amount=0` + the level in a `Dimensions` entry; the rating job time-integrates the
  piecewise-constant level. Idle accounts emit nothing. Document the OPTIONAL daily
  reconciliation-sample backstop for missed-delete drift.
- [ ] Coexist with host-priced #289: no `metered` block → unchanged; with one → OpenRails
  computes the amount.
- [ ] Surface rated amounts on #303 invoices as metered line items (meter, qty, rate, amount).
- [ ] Group invoice line items per `resource`/instance (existing usage_event `Resource` column +
  #311 rollup-by-resource): tagged usage bills as "VM-1 → RAM/disk/bandwidth" sub-groups; untagged
  aggregates per meter. Rate per (customer, meter, resource) when tagged.
- [ ] Tests: counter (sum) accrues; storage gauge (weighted_sum time-integral, persists across
  periods); rate-the-aggregate rounds once (no per-event drift); custom-currency rate rejected;
  a meter (billing) and a usage_limit (#594 cap) over the same event stream operate independently.

---

# #598: DECIDED — keep `openrails.entitlements` as the flat access ledger; materialize product benefits at grant time (rejected: derive-from-ownership)

**Completed:** yes — DECISION 2026-06-28 (owner-reviewed). Do NOT eliminate the
entitlements table and do NOT switch to read-time derivation from product ownership.
Keep `entitlements` as the source-of-truth flat access ledger; products MATERIALIZE
their benefits into it (and the credit/admission ledgers) at grant time (#594). No
schema change is required by this issue. The implementation work — materializing a
product's benefits on purchase — lives in #594.

## Why keep it (rejecting the rewrite)
The current `openrails.entitlements` table is already a windowed, source-tracked,
revocable, audit-linked access ledger: `entitlement` key + `start_at`/`end_at`/`period`
(a generated `tstzrange`) + `source_type` (subscription|one_off|admin|grace|grant) +
`grant_id` + `revoked_at`. It is NOT duplicate state today (there is no
`product_ownership` table to duplicate); it *is* the access state. Its two decisive
advantages — both flowing from being a flat, denormalized, point-in-time ledger:

- **Manual grants are trivial.** A comp/support/legacy entitlement is one INSERT
  (`source_type='admin'|'grant'`), needing no product or catalog mapping. Entitlements
  that map to no product *must* live in a flat table like this anyway.
- **Reads are trivial and fast.** "What does this customer have now?" is one indexed
  query (`WHERE merchant_id+customer_id AND period @> now() AND revoked_at IS NULL`),
  no joins, no derivation. Reverse lookup ("who has entitlement X?") is just as cheap.
  It also unifies catalog-derived AND manual access behind one read surface.

A pure derive-from-ownership model trades these away: reads become ownership→product-spec
→feature-key joins, reverse lookup scans all ownership × product specs, and you end up
re-introducing a projection cache — which *is* this table. Its one real weakness is the
catalog-churn case (changing a product's entitlement set needs a backfill of active
rows), and at current scale that is rare (doujins: one product, one entitlement, for
years). YAGNI; the flat ledger wins.

## Model: materialize at grant time (NOT derive at read time)
- No top-level entitlement registry in the catalog. Product entitlement grants are direct
  URL-safe keys.
- On a successful purchase/subscription/renewal/admin grant, write the product's CURRENT
  benefits into the existing ledgers: one `entitlements` row per granted entitlement
  (windowed, with `source_type` + `grant_id`); credits into the money/credit ledger;
  usage limits as admission-policy bindings (#594). Included products recurse.
- The entitlement rows ARE the snapshot of what was granted, so grandfathering is
  automatic and free: a later catalog change does not touch existing windows. Catalog
  changes apply to FUTURE grants by default; an explicit backfill action can push
  *additive* changes onto active windows.
- `grants` remains the audit log of why each materialization happened.

The only future trigger to revisit product-level ownership is #594 reaching genuinely
rich products (credits + included products + usage limits) PLUS real catalog churn —
not entitlements alone.

Catalog shape stays lazy:

```yaml
usage_limits:
  - key: starter-spend
    measure: billable_spend
    windows:
      - window: 5h
        amount: 10_000_000

products:
  - key: premium-monthly
    display_name: Premium Monthly
    entitlements:
      - premium
    usage_limits:
      - starter-spend
```

Product-level `entitlements: ["premium"]` means "materialize an entitlement row whose
key is `premium`." There is no separate feature definition to create or validate against.
Entitlement keys are URL-safe lookup keys in the entitlement namespace. They may share
the same literal value as product keys because product and entitlement identity are
separate namespaces.

## What the rewrite would have bought (and why it isn't worth it now)
- Instant catalog changes for active owners with no backfill — the one real win, but
  rare at current scale; an explicit additive-backfill action covers it when needed.
- Durable feature-key metadata/validation — rejected for v1; direct keys are enough until
  OpenRails itself needs entitlement labels/descriptions.
- Grace as an ownership-window concern — the flat ledger already models grace as a
  `source_type='grace'` window, so no rewrite is needed for it.

## What must remain supported
- Manual comp/support entitlements that are not represented by product ownership.
- Imported legacy one-off entitlements that cannot be mapped to product ownership.
- Reverse lookup: list customers with entitlement X.
- Batch active entitlement lookup for AuthKit/token enrichment.
- Historical audit: answer what caused access and when ownership/grace started/ended.
- Performance for hot reads without making every request scan huge ownership/catalog sets.
- Direct entitlement keys in product manifests and ledger rows; no separate feature CRUD
  unless OpenRails later needs labels/descriptions.

## End state (decided)
1. `openrails.entitlements` STAYS the source-of-truth flat access ledger — catalog-derived
   and manual rows unified behind one read surface. No `product_ownership` source-of-truth table.
2. No top-level catalog `entitlements` registry and no required entitlement metadata
   layer; products carry direct entitlement keys.
3. Products materialize their benefits into `entitlements` (+ the credit/admission ledgers)
   at grant time (#594); `grants` records why each materialization happened.
4. No table is deleted, and none is demoted to a projection/cache.

## Risks / reasons to reject
- Reverse entitlement lookup may need an indexed projection.
- Existing dunning/grace/refund code may rely on entitlement timeline rows more deeply
  than expected.
- Long-lived JWTs can still be stale after catalog changes unless token freshness is
  enforced elsewhere.
- Historical "what entitlement did this user have on date X?" may require catalog
  version history, not just current product definitions.
- Direct entitlement keys can hide typos; accept this for v1 unless merchants need strict
  validation.
- Migration may be too risky if the current entitlement table is already serving as a
  useful compatibility projection.

## Tasks
- None. This is a decision record: keep the flat access ledger and skip the feature
  registry. The materialize-at-grant implementation lives in #594.

---

# #597: `pull-provider` adoption/import — rebuild local billing mirror from provider truth when identity/catalog resolve

**Completed:** yes — VALIDATED 2026-06-29.

STATUS 2026-06-29 (Codex): existing `pull-provider` adoption/materialization path is the
v1 implementation: provider snapshots reuse the shared fetchers, resolve plan/price through
catalog provider links (`plan_id`, `price_id`, `recurring_billing_option_id`), require
deterministic identity, create local subscription/payment materialization actions in enforce
mode, and leave unresolved/ambiguous rows review-only. Revalidated with `internal/reconcile`
tests.

Decision 2026-06-28: `pull-provider` owns provider reconciliation and provider
adoption. Against an existing mirror it repairs drift. Against a blank or partially
incomplete database it should create local customers, payment methods, subscriptions,
payments, and derived grants from provider state only when ownership and catalog
mapping are deterministic.

## Goal
Support this recovery/bootstrap path:

1. A host migrates users/subjects locally.
2. OpenRails has provider credentials and catalog mappings.
3. `pull-provider` reads NMI/CCBill/Stripe/Solana provider state.
4. It resolves each provider object to a local merchant subject + local price/product.
5. It creates the missing OpenRails billing mirror rows.
6. It verifies/repairs drift for rows that already exist.

## Non-goal
No probabilistic imports. Email is report evidence, not authority. Provider customer ids
are rail-local ids, not OpenRails subjects. Provider plan ids are not OpenRails catalog
semantics unless metadata or a manifest maps them.

## Resolution order
1. Provider metadata recovery envelope:
   - merchant slug/id
   - canonical host subject / AuthKit user id
   - OpenRails customer / permission-group id when known
   - product key
   - price key/id
   - checkout/subscription/payment/payment-method ids when available
2. Adoption manifest:
   - provider customer/vault/subscription ids -> host subject ids or OpenRails
     customer/permission-group ids
   - provider plan/price ids -> OpenRails price keys/ids
   - CCBill numeric price/subscription ids or flex/form ids -> OpenRails price keys
   - provider account bindings
3. Otherwise unresolved. Emit a report row; do not create local billing state.

Customer association is never inferred from email/name/card details. Adoption resolves a
subscription/payment by reading the stamped subject/customer envelope from the subscription
first, then provider customer/vault, then checkout/payment/order records, then the manifest.
If none of those provide a deterministic local subject/customer mapping, the row stays
unresolved.

## Provider-data recoverability
Recoverable from provider records, when deterministic ids or recovery metadata were
stamped before the loss:
- provider customers, vault/payment-method references, subscriptions, payments, refunds,
  provider account links, and status/timestamps exposed by the rail.
- product/price shell: product key/display fields, price key, amount, currency,
  billing period, and provider ids/links.
- local customer association, only from stamped canonical subject/customer fields or
  an adoption manifest.
- Stripe-only mirrors, if OpenRails deliberately used the Stripe primitives: entitlement
  features attached to products, billing credit grants/balances, and meter/price
  structure.
- NMI/CCBill equivalents only where the rail exposes durable ids/report fields or
  custom/merchant-defined fields OpenRails stamped.

Not recoverable from provider records alone:
- `includes` product ownership.
- OpenRails usage-limit/admission policy and Redis counter state.
- custom ledgers/counters and non-provider balances.
- permission-group delegation.
- full `grants` provenance, manual/admin grants, and local benefit-change history.
- customer association when neither stamped metadata nor a manifest maps the provider
  object to the local subject/customer.

`pull-provider` should recover every deterministic field it can, but its report must
show each field source: provider-native, recovery metadata, adoption manifest, local
catalog, or unresolved. Provider-native benefit objects can seed missing local rows, but
OpenRails catalog/adoption manifests remain the source for OpenRails-only benefits.

## CLI shape

```bash
openrails pull-provider --provider nmi --manifest adoption.yaml
openrails pull-provider --provider nmi --manifest adoption.yaml --insert
```

Default is plan-only. `--insert` applies only deterministic rows.

## Materialized rows
- `customers` for resolved host subjects.
- payment-method/vault references tied to provider account + customer.
- subscriptions tied to resolved customer + price.
- payments tied to resolved customer/subscription/price.
- entitlements/credits/usage-limit bindings MATERIALIZED into the existing ledgers
  (entitlements table, credit/money ledger, admission) from the LOCAL catalog
  product/benefit definitions — never from provider catalog guesses — with `grants`
  recording each event (#594/#598).
- provider-account bindings on adopted rows.

## Tasks
- [ ] Define `adoption.yaml` schema for legacy mappings.
- [ ] Extend `pull-provider` plan output to report adoption candidates, source of truth per field,
  unresolved rows, and intended inserts.
- [ ] Reuse existing provider fetchers; do not duplicate rail clients.
- [ ] Implement deterministic resolver: metadata first, manifest second, unresolved third.
- [ ] Implement insert path for customers, vault refs, subscriptions, payments.
- [ ] Derive grants from local product benefits after subscription/payment creation.
- [ ] Add tests: metadata-only adoption, manifest-only adoption, unresolved rows, ambiguous rows, idempotent re-run.
- [ ] Document the per-provider recoverability matrix: what can be recovered from provider-native
  data, what requires stamped metadata, what requires a manifest/local catalog, and what is
  unrecoverable.
- [ ] Document: `pull-provider` reconciles existing rows and imports missing rows where deterministic.

---

# #596: Stamp OpenRails recovery metadata on every provider-created object

**Completed:** yes — CATALOG RECOVERY METADATA LANDED 2026-06-29.

STATUS 2026-06-29 (Codex): added recovery metadata constants for provider-created catalog
objects and a deterministic product benefit fingerprint over OpenRails-owned benefits.
Stripe catalog creation now stamps recovery version, stable product key, stable price key,
benefit fingerprint, and informational row UUIDs on created Products/Prices. NMI/CCBill keep
using deterministic provider identifiers where metadata documents are not available.

Decision 2026-06-28: provider adoption is only safe if OpenRails-created provider
objects carry canonical OpenRails breadcrumbs. Add a single recovery envelope and
stamp it wherever each provider supports metadata or stable operator fields.

## Recovery envelope

Logical fields:

```text
openrails_version
merchant_slug
merchant_id
subject_issuer
subject
authkit_user_id
customer_id
permission_group_id
product_key
price_key
price_id
catalog_version
catalog_fingerprint
benefit_fingerprint
checkout_session_id
subscription_id
payment_id
payment_method_id
provider_account_id
```

Use the subset each provider supports, but keep one canonical internal shape.

## Provider behavior
- Stripe: deterministic product `id` when safe for the connected account, product metadata
  as backup, and deterministic price `lookup_key`; keep `prod_...` / `price_...` as links.
  Stamp the envelope onto customer, product, price, checkout/session, subscription,
  payment intent/invoice/refund objects where Stripe supports metadata. Metadata stores
  compact keys and fingerprints, not the full OpenRails benefit spec.
- NMI: product `product_sku`, product `product_description`, recurring `plan_id`, `plan_name`,
  and order/customer/vault merchant-defined fields that survive query/report reads. Treat
  NMI as a fixed breadcrumb surface, not an arbitrary metadata document store.
- CCBill: form/flex/custom fields that survive DataLink exports; numeric admin price ids are
  provider links, not OpenRails identity.
- Solana: plan/payment memo/account metadata where feasible.

## Tasks
- [ ] Add canonical `RecoveryMetadata` type + serializer/parser.
- [ ] Stamp metadata during catalog/provider price creation.
- [ ] During provider catalog push, derive deterministic remote identifiers from local keys:
  Stripe product id from namespaced product key, Stripe price `lookup_key` from price key,
  NMI `product_sku` from product key, and NMI `plan_id` from price key.
- [ ] Find existing provider catalog objects by deterministic identifiers before creating;
  after creation, persist provider-generated links (`prod_...`, `price_...`, CCBill ids)
  alongside the deterministic key mapping.
- [ ] Stamp metadata during checkout/session creation.
- [ ] Stamp metadata during subscription creation/update.
- [ ] Stamp metadata during payment-method vault creation.
- [ ] Stamp metadata during payment creation/refund where providers support it.
- [ ] Extend provider fetchers to parse the envelope back into remote snapshots.
- [ ] Ensure subject/customer breadcrumbs are stamped redundantly enough that a provider
  subscription/payment can be reattached to a migrated local user after a blank-DB restore.
- [ ] Tests per rail: created provider payload includes metadata; fetched snapshot recovers it;
  adoption links subscription/payment to a migrated user from metadata and refuses email-only
  matching.

---

# #595: Deterministic, bidirectional catalog identity for products and prices

**Completed:** yes

Decision 2026-06-28: OpenRails catalog identity should be recoverable from natural
descriptors. Products are mutable benefit buckets. Prices are immutable commercial
terms pointing at a product.

## Product identity and label
Product `key` is the canonical immutable identifier used in APIs, manifests, provider
metadata, and DB relationships. Scope it by merchant. A separate product UUID or
opaque derived product key is duplicate state if the user-chosen key is immutable; use
`(merchant_id, key)` as the DB key. The product's benefits/settings are mutable;
ownership of the product remains stable.

Use `key` for all user-chosen catalog identifiers. Keys must be URL-safe, lowercase,
and stable. Product keys and entitlement keys live in separate namespaces: product
`premium` and entitlement key `premium` may both exist and do not collide.

`display_name` is the mutable user-facing label. Provider mapping should follow the
same split where possible: NMI uses a unique product SKU for identity and product
description for the user-facing name.

## Price identity
Price natural key:

```text
price:<merchant_slug>:<product_key>:<currency>:<unit_amount>:<billing_period>
```

Examples:

```text
price:doujins:premium:usd:25_000_000:30d
price:doujins:api-credits-100:usd:100_000_000:once
```

Price key should be deterministic from that natural key. Price `currency` is required;
do not default it to USD. Unit amount/currency/billing period/product-link changes create
a new price. Mutable price fields should be operational only: status/archive, provider
links, timestamps.

## Bidirectional mapping
- local merchant + product key -> provider product via provider links.
- local price key -> provider price/plan via provider links.
- provider product/price -> local product key and price key via stamped metadata.
- legacy provider product/price -> local product key and price key via adoption manifest.

## Provider identifier mapping
- Stripe product: prefer caller-owned product `id` derived from the namespaced product key
  when safe for the connected account; otherwise store `product_key` in metadata and keep
  Stripe's `prod_...` id as a provider link.
- Stripe price: provider `price_...` is an opaque link; use deterministic `lookup_key`
  derived from the OpenRails price key and stamp metadata as backup.
- NMI product: map `product_sku = product key`; map `product_description = display_name`.
- NMI recurring plan: map `plan_id = deterministic price key`; map `plan_name = display_name`.
  Do not use product key as the canonical plan id except as a compatibility alias for
  single-price legacy products; a plan is price/period terms, not the mutable product bucket.
- CCBill: no OpenRails-owned product key object. Store `form_name`, `flex_id`, and any
  numeric Pricing Admin price/subscription ids as provider links; resolve legacy rows via
  adoption manifest.

## Tasks
- [ ] Add canonical URL-safe product key / entitlement key validation and price-key builder.
- [ ] Make products use immutable natural identity (`merchant_id + key`) instead of a separate product UUID/derived key.
- [ ] Store mutable product `display_name` separately from immutable product `key`.
- [ ] Store price natural keys or make them derivable from existing columns.
- [ ] Enforce price uniqueness by merchant + product key + currency + unit amount + billing period.
- [ ] Keep product identity fields separate from mutable product fields.
- [ ] Update provider link logic with Stripe/NMI deterministic identifiers and CCBill link-only identifiers.
- [ ] Add provider lookup tests: Stripe product by deterministic id, Stripe price by
  `lookup_key`, NMI product by `product_sku`, and NMI recurring plan by deterministic
  `plan_id`.
- [ ] Add tests for stable product key, changed product benefits preserving ownership, changed price terms creating a new price key.

---

# #594: Product benefit buckets — entitlements, credits, ownership grants, and spend limits

**Completed:** yes — v1 MANIFEST/STORAGE SLICE LANDED 2026-06-29.

STATUS 2026-06-29 (Codex): flat top-level `products:` now normalize into tier groups using
`key` as the slug fallback; products accept benefit fields (`entitlements`, `credits`,
`usage_limits`, `includes`) and benefit-only products with no prices. Added usage-limit
registry validation, credit `currency` alias + `expires` duration validation, shared `h`/`d`
duration parser, sidecar tables for catalog usage-limit definitions and materialized
product-derived bindings, and guarded provider boundary tests so OpenRails benefits remain
local authority.

Decision 2026-06-28: a product is the mutable bucket of benefits a user gets by
owning/subscribing to that product. Price is only the commercial term. Product benefits
must model all OpenRails-owned access and billing effects directly.

## Target manifest shape

```yaml
usage_limits:
  - key: starter-spend
    measure: billable_spend
    windows:
      - window: 5h
        amount: 10_000_000
      - window: 7d
        amount: 70_000_000

products:
  - key: premium
    display_name: Premium
    description: Optional text
    entitlements:
      - premium
    usage_limits:
      - starter-spend
    credits:
      - ledger: credits          # named ledger; real currency → money
        currency: usd
        amount: 25_000_000
      - ledger: ai-image-credits  # currency omitted → custom integer counter
        amount: 200
    includes:
      - product-b
      - product-c
    prices:
      - currency: usd
        unit_amount: 25_000_000
        billing_period: 30d
        providers:
          - stripe
          - nmi

  - key: claude-api-credits-50
    display_name: $55 Claude API credits
    description: Pay $50, receive $55 in Claude API credits
    credits:
      - ledger: claude-api-credits
        currency: usd
        amount: 55_000_000
    prices:
      - currency: usd
        unit_amount: 50_000_000
        providers:
          - stripe
          - nmi

  - key: claude-api-credits-100
    display_name: $120 Claude API credits
    description: Pay $100, receive $120 in Claude API credits
    credits:
      - ledger: claude-api-credits
        currency: usd
        amount: 120_000_000
    prices:
      - currency: usd
        unit_amount: 100_000_000
        providers:
          - stripe
          - nmi
```

Prices are nested under products. A price's identity includes the product key plus
commercial terms, so a top-level `prices:` section would duplicate the product link
and invite drift. Products may have zero prices when they are benefit-only buckets
granted by another product; sellable products declare one or more nested prices.

Durations use one explicit OpenRails catalog grammar anywhere the manifest expresses
a time length: price `billing_period`, usage-limit `window`, credit `expires`, and
future duration fields. Use Go-style strings with one OpenRails extension: `h` and
`d`. Examples: `24h`, `30d`, `365d`. Missing price `billing_period` means one-time;
`once` is accepted as an explicit spelling there. Missing credit `expires` means the
default `365d`. Canonicalize equivalent durations before storing or building keys, so
`24h` and `1d` resolve to the same period. Go's standard `time.ParseDuration` handles
`h` but not `d`, so this needs a tiny catalog parser for `h` and `d`; do not add a
dependency.

Provider adapters translate the canonical duration into each provider's native shape.
Stripe recurring prices use `interval` (`day`, `week`, `month`, `year`) plus
`interval_count`; for OpenRails `30d`, prefer Stripe `interval=day` and
`interval_count=30` rather than calendar `month`. Price billing periods must be
`once` or whole-day durations; sub-day durations like `5h` are valid for usage-limit
windows, not provider recurring prices.

Money amounts are currency-native integer units. For USD, use micro-dollars: $25.00
is `25_000_000`, $10.00 is `10_000_000`, and $70.00 is `70_000_000`. Prices use Stripe's
`unit_amount` field name because they describe the unit price a provider charges.
Credit grants and usage limits use `amount` because they describe ledger deposits or
budget counters, not provider price objects. Credit grants target a
named ledger: `(merchant, customer, ledger)` identifies it, and the ledger's `currency`
determines what the integer `amount` means — a real ISO currency (`usd`, `yen`) is money
in micro-units, while `custom` (the default when `currency` is omitted) is an app-defined
integer counter. So `280_000_000` is $280.00 in a `usd` ledger and 280,000,000 units in a
`custom` ledger; the ledger, not the bare number, carries the meaning.

Examples this must cover:
- `premium`, `premium-plus` entitlement grants.
- Money credit packs: pay $25 for $25 prepaid USD credits, $50 for $55 Claude API credits,
  or $100 for $120 Claude API credits.
- Credit packs: pay $10 for 100 `ai_image_credit` units.
- Separate ledgers: a `usd` `balance-owed` ledger (driven negative by arrears) and a
  separate `usd` `credits` ledger, held by the same customer without auto-netting.
- Included products: buy product-A, also own product-B and product-C.
- Usage/rate limits: product-A grants usage limit `starter-spend`, which caps
  `billable_spend` at 5h/$10 + 7d/$70, represented as integer measure units. Products
  can grant entitlements, usage limits, credits, and included products independently.

## Semantics
- Product key is stable; benefits are mutable.
- The `entitlements` ledger is the source of truth for access (see #598). A purchase,
  subscription, renewal, import, or admin grant MATERIALIZES the product's CURRENT
  benefits at grant time: one windowed `entitlements` row per granted entitlement
  (carrying `source_type` + `grant_id`), credits into the credit/money ledger, and usage
  limits as admission-policy bindings. Reads use the existing flat ledger directly — no
  read-time derivation from ownership, and no `product_ownership` source-of-truth table.
- The materialized rows ARE the snapshot of what was granted, so grandfathering is
  automatic: a later catalog change does not touch existing windows. Catalog changes
  apply to FUTURE grants by default; an explicit additive-backfill action can push newly
  added benefits onto active windows when a merchant wants the change to be retroactive.
- Grace is just another materialized window, not a special case: a subscription in grace
  holds `source_type='grace'` entitlement rows whose `end_at` is `grace_ends_at`; when
  grace ends those rows simply expire (no special-case derivation to unwind).
- Credit grants are payment/ledger entries, not continuously recomputed access state.
  Each successful one-time purchase grants the product's configured credits once. Each
  successful recurring payment grants them once for that billing period. Editing catalog
  credit grants changes future payment grants; existing granted/spent credits remain
  auditable unless an explicit credit migration action is run.
- Balances live in named ledgers keyed `(merchant, customer, ledger)`, where `ledger` is an
  arbitrary merchant-chosen URL-safe key. A customer×merchant may hold AS MANY ledgers as the
  merchant wants, including more than one in the same currency — e.g. a `balance-owed` ledger
  and a separate `credits` ledger, both `usd`. Each ledger carries a `currency`:
  - a real ISO currency (`usd`, `yen`, …) → MONEY: micro-unit scale, may go negative (arrears,
    capped by the configured outstanding-owed limit), recorded via double-entry (money's
    source of truth).
  - `custom` (the default when `currency` is omitted) → an app-defined integer counter
    (`ai_image_credit`, game gold): floors at 0, recorded as credit lots, is not money, and
    never offsets owed money.
  A ledger's currency is fixed on first use; a later grant referencing the same ledger with a
  different currency is a validation error.
- Currency vocabulary: built-ins are the ISO 4217 codes, compiled in (NOT a merchant-managed
  table) — every ISO currency is money, stored in micros uniformly; the ISO minor-unit exponent
  (USD→2, JPY→0) is used ONLY for display and provider conversion (e.g. Stripe cents). `custom`
  is a single sentinel for a non-money integer counter; the LEDGER NAME (`ai-image-credits` vs
  `gold`) is what distinguishes counters, so merchants do NOT define custom currencies — that is
  the unit registry rejected in #598 (a label is the app's job; a fractional `scale` is a YAGNI
  per-ledger add only if some merchant ever truly needs sub-unit amounts).
- Currency validation: `currency` ∈ {any ISO 4217 code} ∪ {`custom`}; omitted → `custom`; any
  other value (e.g. `usdd`) is a validation error. A price/payment may only target an ISO (money)
  ledger — charging into a `custom` ledger is an error, which also catches a forgotten
  `currency: usd`.
- ISO money ledgers are signed balances: positive means prepaid/credit, negative means owed
  arrears, bounded by the configured outstanding-owed limit. `custom` ledgers are not signed
  debt ledgers; they floor at zero.
- Cross-ledger application is allowed only for the same merchant, same customer, and same ISO
  currency. It is explicit or configured, not surprise global netting: e.g. customer owes $10
  on ledger-A and has $20 on ledger-B, so apply $10 from B to A, leaving A=$0 and B=$10.
  Otherwise ledgers stay isolated. No cross-currency, no custom-counter application, no FX.
- Money credit grant materialization deposits into the named ISO-currency ledger; within that
  ledger the signed balance absorbs any negative (owed) balance before going positive (prepaid).
  Custom grants deposit into the named `custom` ledger as integer units, spendable by the
  merchant's own usage logic but never offsetting owed money.
- Usage limits are a separate admission/rate-limit policy category. They are not
  entitlements and not credits. A product can grant usage limit keys alongside
  entitlement keys and credit grants. `measure` is a direct URL-safe event-stream key
  whose values are summed as integers for admission control; `billable_spend` does not
  touch the money ledger. Future measures can be arbitrary integer counters, but their
  admission/capture semantics live in the merchant application, not a catalog registry.
- Grants are the audit log: "this user got ownership/credit/manual entitlement because
  of this payment/import/admin action." Source product/subscription/payment is preserved.
- The existing entitlement read surface stays backed by the `entitlements` ledger itself:
  catalog-derived and manual rows live in the same table and are read uniformly. It is the
  source of truth, not a projection or compatibility cache (#598).
- Provider catalog never owns these semantics; providers only receive sellable products/prices.

## Usage limit system
- Specified in catalog as top-level `usage_limits` definitions referenced by product keys.
  A definition has `key`, direct `measure`, and one or more windows (`window`, `amount`).
  `measure` is just the URL-safe event-stream key the merchant app reports against.
- Validate catalog usage limits on apply: URL-safe `key` and `measure`, unique limit keys,
  positive window durations, positive integer amounts, and no duplicate window key/duration
  within one limit. Do not add a measure registry or unit system.
- Store catalog definitions lazily in the product/catalog JSON first. Split tables only when
  querying/reporting needs it.
- At grant time, materialize product-granted usage limits into a durable binding row, not
  a hot-path counter row. Shape: merchant, customer, usage_limit_key, measure, windows
  JSON, product_key/source_type/grant_id, starts_at, ends_at, revoked_at, policy_version.
  This row is the recoverable "customer is entitled to this limit" fact and is written
  only on purchase/renewal/admin grant/backfill/revoke.
- Delegated application traffic is a separate usage-policy source, not product ownership.
  A platform application such as Cozy Art registered under Tensorhub can define delegated
  tiers/profiles that point at usage-limit keys or inline windows. A delegated user is an
  opaque invoker key namespaced by application, e.g.
  `app:<application_id>:sub:<stable_host_subject>`, so native Tensorhub users, Cozy Art
  users, and another tenant's users cannot collide.
- For delegated users, avoid per-user OpenRails rows unless Tensorhub deliberately wants
  server-side assignment. The remote app JWT should identify the application, delegated
  subject, and tier/profile key; Tensorhub/OpenRails resolves that tier/profile against
  platform-owned application config. Membership changes take effect on next token issuance
  or server-side profile update, bounded by token TTL.
- If Tensorhub needs an application-wide aggregate cap, model the application as the payer
  customer and apply payer-scope windows. If it needs per-delegated-user caps, apply
  invoker-scope windows keyed by the namespaced delegated user. Both compose in the same
  admission policy and both use Redis counters.
- Customer ledger delegation is a third layer: a customer permission group may authorize
  other principals to spend a specific `(merchant, customer, ledger)` balance on its behalf,
  including stored-balance spend or arrears spend. This chooses the payer/ledger authority;
  it does not by itself define the usage limit. After authorization resolves the payer
  ledger, usage-limit policy still composes from product-derived customer limits,
  delegated-application limits, and customer-owned invoker/role caps.
- Keep the axes separate:
  native Tensorhub user purchase -> customer/product bindings;
  Cozy Art delegated user -> application tier/profile + invoker-scoped counters;
  customer "let these users spend my balance" -> permission-group ledger delegation plus
  optional payer-owned invoker/role caps.
- Do not cram product-derived usage limits into existing `payer_spend_limits` or
  `invoker_spend_limits`; those are trust-tier/delegated-invoker admin policies. The
  admission loader should compose product-derived bindings with those existing policies.
- Enforcement uses Redis/Garnet fixed-window counters, reusing the current spendgate/ratelimit
  primitives. Request-time counter state lives only in Redis; Postgres is never updated
  per API request.
- Admission input must name `measure`, integer `amount`, customer, request id, and optional
  invoker/roles. The loader selects active bindings for that customer+measure. The gate
  atomically denies if any applicable window would exceed its amount; on allow it reserves
  the estimate in every applicable window.
- For delegated application admission, the input also carries application id, delegated
  subject/invoker, and tier/profile key. The host may report the invoker key directly only
  after Tensorhub has validated the remote application JWT and normalized it under the
  application namespace.
- For customer-authorized ledger spend, admission first resolves the customer permission
  group and ledger the invoker is allowed to spend from, then runs the same Redis gate with
  that customer/ledger as payer authority and the acting principal as invoker.
- Capture keeps the reserved estimate in the windows and releases only the in-flight hold.
  Release subtracts the estimate from the windows. This matches the current estimate-based
  spendgate model and avoids true-up complexity. If exact usage is required, the caller should
  admit with the exact amount.
- Refresh is implicit: Redis keys expire at the fixed-window boundary plus slack. No cron job
  resets counters. If Redis is flushed, counters restart at zero but durable bindings reload
  from Postgres. Bindings refresh only when a new grant/renewal materializes a new window.
- Catalog edits affect future grants by default. Additive backfill inserts new active
  usage-limit binding rows for existing windows; removals do not delete existing bindings
  unless an explicit revoke/migration action does it.
- Introspection reads active bindings plus Redis window status and returns, per customer and
  optional measure/invoker: usage_limit_key, measure, window, limit, used, remaining,
  reset_at, and source product/grant. This is dashboard/reporting state, not source of truth.

## Provider boundary
- OpenRails owns entitlements, usage limits, money credits, non-money credit balances, and
  included product ownership.
- Stripe can mirror product display/description, but it does not own OpenRails
  entitlements, included products, credits, or usage limits.
- NMI and CCBill own sellable/payment-side objects only; they do not own OpenRails benefits.
- Provider sync is best-effort for provider-visible fields. Benefit changes must not depend
  on provider mutation succeeding.

## Storage approach
Lazy path first: extend the current product JSON specs rather than splitting tables
immediately. Split later only if querying/reporting demands it.

## Tasks
- [ ] Extend catalog manifest with flattened product benefit fields plus a separate `usage_limits` registry.
- [ ] Keep prices nested under products; allow benefit-only products with no direct prices.
- [ ] Normalize price `unit_amount` and money-credit/usage-limit `amount` values as integer units; USD examples use micro-dollars with YAML underscore grouping.
- [ ] Require price `currency`; do not default sellable prices to USD. Credit grants may omit `currency`, which defaults to `custom`.
- [ ] Replace catalog `interval`/`interval_count` with optional `billing_period` strings (`24h`, `30d`, `365d`; missing means one-time; `once` allowed) and canonicalize equivalent periods.
- [ ] Use the same duration parser/canonical form for usage-limit `window` and credit `expires` values.
- [ ] Replace credit `expires_after_days` with optional `expires`; omitted means default `365d`.
- [ ] Translate canonical billing periods into provider-native recurrence fields; Stripe gets `interval` + `interval_count`.
- [ ] Reject sub-day `billing_period` values for provider-backed prices while still allowing sub-day usage-limit windows.
- [ ] Map existing `entitlements` and money credits into the new benefit model.
- [ ] Apply credit grants from successful payments/renewals, not from continuous ownership recomputation.
- [ ] Model balances as named ledgers `(merchant, customer, ledger)` with a per-ledger `currency`; allow as many ledgers per customer as the merchant wants, including multiple of the same currency. Fix a ledger's currency on first use; reject later references that change it.
- [ ] Route by currency: real ISO currency → double-entry money posting (micro-units, may go negative up to the outstanding-owed limit); `custom` (default) → integer credit-lot posting (floors at 0).
- [ ] Currency vocabulary + validation: compiled-in ISO 4217 table (code → display exponent) as the built-in money currencies; single `custom` sentinel for integer counters (no merchant-defined currencies). Validate `currency` ∈ {ISO} ∪ {`custom`}, omitted → `custom`, else error; reject prices/payments targeting a `custom` ledger.
- [ ] Allow ISO money ledgers to carry signed balances: positive prepaid/credit, negative owed arrears bounded by the outstanding-owed limit; keep `custom` ledgers non-negative.
- [ ] Provide explicit/configured same-currency ledger application/transfer for merchants who want prepaid on one ledger to pay down owed balance on another ledger; no cross-currency/custom-counter netting.
- [ ] Tests: ISO ledger can go negative within owed limit; custom ledger cannot go below zero; same-currency ledger application moves value between two ledgers for the same merchant/customer; cross-currency and custom-counter application are rejected.
- [ ] Materialize a product's current benefits into the existing ledgers at grant time (subscription, one-time purchase, renewal, import, grace, admin): windowed `entitlements` rows + credit deposits + admission-policy bindings.
- [ ] Default catalog changes to apply on the NEXT grant/renewal only; existing windows keep their materialized rows. Provide an explicit additive-backfill action for retroactive adds.
- [ ] Keep reads on the flat `entitlements` ledger (catalog-derived + manual unified); NO read-time derivation and NO `product_ownership` source-of-truth table (#598).
- [ ] Treat `grants` as the audit ledger for the materialization events (entitlement/credit/manual).
- [ ] Add `includes` product ownership grants to the product benefit application path.
- [ ] Apply product-granted usage limit keys separately from entitlements and credits.
- [ ] Treat `usage_limits[].measure` as a direct URL-safe event-stream key; windows only carry `window` + integer `amount`.
- [ ] Add durable product-derived usage-limit binding storage with source/grant/window metadata; do not reuse tier/delegated admin policy tables as the source of truth.
- [ ] Keep usage-limit request counters exclusively in Redis/Garnet; no Postgres write on admit/capture/release.
- [ ] Compose active product-derived usage-limit bindings into the existing Redis admission gate policy.
- [ ] Add usage-limit admission input fields for direct `measure` + integer `amount`; keep money affordability separate from non-money usage counters.
- [ ] Add delegated application usage policy support: application config maps tier/profile keys to usage-limit keys/windows.
- [ ] Normalize delegated invoker identity as application-namespaced opaque keys; reject un-namespaced delegated subjects.
- [ ] Compose delegated application policies with product-derived bindings and existing admin/delegated policies in the Redis admission gate.
- [ ] Add cross-tenant/app isolation tests: same host subject under two applications gets distinct counters; native user ids do not collide with delegated ids.
- [ ] Add customer permission-group ledger spend delegation support: resolve which principals may spend each `(merchant, customer, ledger)` balance, including stored-balance and arrears spend.
- [ ] Compose customer-owned ledger delegation with usage-limit admission: authorization selects payer/ledger; Redis policy enforces payer/invoker windows.
- [ ] Add delegation tests: authorized principal can spend the delegated ledger, unauthorized principal cannot, and two ledgers owned by the same customer do not share spend authority unless explicitly granted.
- [ ] Add usage-limit introspection: active bindings plus Redis used/remaining/reset state.
- [ ] Add explicit credit expiry support in product credit grants.
- [ ] Catalog apply/upsert updates the product definition for FUTURE grants; it does NOT rewrite active holders' materialized rows unless the explicit additive-backfill action is run.
- [ ] Keep provider sync limited to provider-visible product/price fields; OpenRails benefit changes stay local and authoritative.
- [ ] Tests: purchase materializes all benefit kinds; a catalog change applies to the next grant (not active windows) by default; additive-backfill pushes an added benefit onto active windows; provider adoption materializes benefits from the local catalog; provider sync failure does not block local benefit convergence.

---

# #591: platform identity & billing model — customer/merchant anchors (authkit groups); merged merchant_external_account; vault + lifecycle axes

**Completed:** yes — FIRST ADDITIVE WHO-AXIS SLICE LANDED 2026-06-29.

STATUS 2026-06-28 (Claude): PLAN / north-star, REVISED to the converged model (owner-reviewed).
Umbrella for #588, #589, doujins #426. **Build incrementally** — today's model keeps working; the
platform layer lands as additive migrations (one new anchor table + a table merge + nullable /
defaulted columns), never a hot-table rewrite.

STATUS 2026-06-29 (Codex): FIRST ADDITIVE SCHEMA SLICE LANDED.
- Added `customer_anchors` + `merchant_anchors`, keyed by opaque AuthKit permission-group id
  (`text` PK, no AuthKit/profile FK), leaving existing `customers` / `merchants` hot tables intact.
- Safe `merchant_external_account` merge floor: `provider_accounts.owner DEFAULT 'merchant'`
  with `merchant|platform` check + table/comment wording. Full table rename deferred because it would
  churn query/generated/FK surfaces for no runtime gain in this slice.
- Focused schema test guards the anchors, owner column, no AuthKit FK, and no payment/subscription
  hot-table rewrite.

TRIAGE 2026-06-28 (Claude, plan review — see #606): KEPT, not closed/merged.
- **This is the WHO axis** (customer/merchant identity, vault ownership, lifecycle) — DISTINCT
  from and COMPLEMENTARY to the catalog cluster #594/#595 (the WHAT axis: product/price keys).
  They share authkit groups as the identity root but don't overlap; build them on separate streams.
- **Already SHIPPED slices of this umbrella:** #588 (two-slot instrument), #589 (payment-methods
  health), doujins #426 (provider-account binding), and #592 (provider-account de-conflation +
  the bill-by-group-id / invoker-opaque / no-FK keying patterns this issue mandates).
- **Next concrete buildable slice:** the thin `customer` + `merchant` ANCHOR tables (PK =
  permission-group-id) + the `merchant_external_account` merge — additive, independent of #594/#595.
- Own-vault (HyperSwitch) + per-customer/per-merchant lifecycle axes are the later platform slices.

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

**Completed:** yes — CORE DONE + LIVE-VALIDATED 2026-06-26; WIRING DONE 2026-06-29. Built the
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

WIRING DONE 2026-06-29: public URL derives from `api_url` and skips local/non-HTTPS
bases; merchant-scoped endpoints use `/v1/merchants/{slug}/webhooks/stripe`, while
legacy config-rail mode uses `/v1/webhooks/stripe`. Captured `whsec_` persists to
the provider-account scoped merchant secret path, with config-rail fallback updating
`stripeProc.WebhookSecret`. The merchant manifest/provider-account reconcile path
now runs idempotent create/reconcile, and a best-effort hourly River worker handles
later drift without blocking boot.

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
- [x] Public webhook URL from config; skip cleanly + log when absent (embedded/local/no public URL). Wired from `api_url`; local/non-HTTPS bases skip.
- [x] Persist the returned `whsec_` to the mode-correct store (per-merchant secret store vs config Rails); wire to what `prepareStripeMultiSecret` reads.
- [x] Create-at-credential-setup hook (idempotent find-or-create by `openrails_managed` marker). Wired via merchant manifest/provider-account reconcile.
- [x] Periodic reconcile River job (drift-fix per the rules above), best-effort, never blocks boot.
- [x] Decided: SNAPSHOT endpoint for first cut (thin Event Destinations = follow-up).
- [x] Tests: mock unit tests (idempotency, url/events in-place patch, version recreate, lost-secret recreate, ignore-unmanaged) + LIVE create/reconcile/delete + LIVE delivery-through-tunnel with signature verify.

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
