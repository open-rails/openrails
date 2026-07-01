<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 660

---

# #658: grant-ledger temporal semantics — terminations carry no window; valid-time = effective everywhere

**Completed:** phase 1
**Status:** PHASE 1 SHIPPED v0.82.0 (2026-06-30) — the terminations-carry-no-window hardening + name-the-clock +
effective-instant plumbing landed (migration 053, grants ledger, ownership status read). Follows the v0.81.0
hotfix that stopped `terminate()` copying a grant's `ends_at` onto revoke/expire rows (past `ends_at` +
`starts_at=now()` violated `grants_valid_window` and broke bulk reconcile of migrated/expired subscriptions).
DECISION 2026-06-30 (Paul): the target end-state is that GRANTS carry no stored end AT ALL — a grant is an
effective `starts_at` + spec, and every access END is derived (subscription grants → the subscription's
`current_period_ends_at`; fixed one-off/trial → `starts_at` + a duration frozen into `spec_snapshot`) or is a
terminating event. That fully removes span-storage and the window-constraint class. It is Phase 2 below: a larger
money-path migration gated on one open decision (where a fixed one-off/trial end is frozen), deliberately NOT
bundled into the Phase 1 hardening.

## Metadata
- Category: data-integrity
- Status: phase-1-complete
- Passes: true

## Problem

`openrails.grants` is an append-only event ledger whose rows are two semantically different things sharing one
shape:

- **grant events** carry a real access window: `starts_at`/`ends_at` are *valid time* (effective/domain time,
  deliberately backdatable — a 2023 subscription grant materialized today keeps `starts_at=2023`). This is
  required for determinism: the fold must be a pure function of source facts, replayable at any wall-clock.
- **termination events** (`revoke`/`expire`/`supersede`) are point-in-time supersession markers. They have no
  window. `terminate()` sets `starts_at = l.now()` (transaction time, not effective time) and — pre-v0.81.0 —
  copied `ends_at` from the grant. No query reads a termination row's window; the revocation instant is read off
  `created_at` (`ListOwnershipGrantsWithStatus`: `term.created_at AS revoked_at`).

Three problems fall out:

1. **No enforced invariant.** Nothing stops a termination row from carrying a window. The v0.81.0 fix is a
   code-side convention only; the next caller/refactor that copies `ends_at` back reintroduces the
   `grants_valid_window` crash. One CHECK covers both event kinds — correct for grants, meaningless for
   terminations.
2. **The clock is unnamed, and the ambiguity is load-bearing.** `starts_at` means effective time on a grant and
   record time on a termination; `created_at` is read as the business fact `revoked_at`. Three answers to "which
   clock is this?" in one table. `RevokeSourcesForSubscriptionAsOf` already proves the team wants *effective*
   revocation time — it threads `asOf` into the entitlement effect — but does NOT thread it to the grant
   termination, which falls back to `l.now()`.
3. **Ownership revocation dates are not trustworthy.** Entitlement gating/history is already correct: the
   `entitlements` projection owns `end_at`/`revoked_at`, set via `asOf`. But **ownership** has no such projection —
   its status is reconstructed straight off the termination event's `created_at`, so a backdated/grace revocation
   records the wrong effective date for owned (one-off) products.

Preserve the elegant part: entitlement gating never reads grants (it reads the derived `entitlements` window).
Grants are the write-side source of truth — provenance, immutable audit, the convergence anchor for
`derive.grant_effect.missing`/`.excess`, replayable rebuild. This issue does not touch the gating path; it only
makes the ledger's own temporal semantics consistent.

## Target design — name the clock, enforce it

One rule, uniform across event types:

- **valid time** = effective/domain time, lives in `starts_at` (+ `ends_at` for grants). A grant's window is
  `[effective start, effective end]`. A termination's effective revocation instant is `starts_at`; it has NO
  `ends_at`.
- **transaction time** = `created_at` (when we recorded the row). Never used as a business fact.
- **only grant events carry `ends_at`** — DB-enforced.

## Tasks

**Phase 1 — SHIPPED v0.82.0 (terminations carry no window + name-the-clock + effective instant):**

- [x] Forward-only migration `053_grants_termination_no_window`: backfill `ends_at = NULL` on existing non-grant
      rows, then add CHECK `event = 'grant' OR ends_at IS NULL`. Encodes "only grants carry a window";
      `grants_valid_window` unchanged.
- [x] Document the clock convention on the `grants.Ledger` type: valid-time (effective) in `starts_at`/`ends_at`,
      grants own the window; terminations are window-less point events (effective instant on `starts_at`);
      `created_at` = transaction time, never a business fact.
- [x] Thread an effective as-of instant through the revoke path: `RevokeSourcesForSubscriptionAsOf` →
      `revokeGrantsForSubscriptionSources` → `RevokeBySourceAsOf` → `RevokeAsOf`/`terminate`, which stamps the
      termination's `starts_at` (zero-time falls back to `now()`, mirroring `Grant()`). `ends_at` stays NULL.
      Non-AsOf wrappers (`Revoke`/`RevokeBySource`) preserved for now-effective callers.
- [x] `ListOwnershipGrantsWithStatus` reads `term.starts_at AS revoked_at` (effective instant) instead of
      `term.created_at`. Backward-compatible for historical rows (`starts_at ≈ created_at`); only improves new
      backdated revocations.
- [x] Regression tests: `TestGrants_TerminationRejectsWindow` (CHECK rejects a windowed termination),
      `TestGrants_RevokeAsOfOwnershipRevokedAt` (backdated revoke → effective ownership `revoked_at`),
      `TestGrants_RevokeAlreadyExpired` still passes.

**Phase 2 — grants carry NO stored end (Paul's target end-state; larger money-path change, sequence deliberately):**

- [ ] RESOLVE FIRST (open decision): where a fixed one-off/trial end is frozen. Recommended: freeze the duration
      into `spec_snapshot` at grant time (drift-proof against later catalog edits), so `end = starts_at + duration`
      is derivable without coupling to mutable `prices.access_duration_hours`.
- [ ] Rewrite entitlement-window end derivation in `MaterializeGrant`: subscription-sourced → the subscription's
      `current_period_ends_at` (+ grace) instead of `grant.ends_at`; fixed one-off/trial → `starts_at` + frozen
      duration. Update the `derive.grant_effect.missing`/`.excess` set queries accordingly.
- [ ] Migration: stop populating `grant.ends_at`; once derivation no longer reads it, drop the column and
      `grants_valid_window` + `grants_termination_no_window` become moot (no event stores a span).
- [ ] Prove renewal needs no new grant/expire (entitlement window tracks the subscription period) and a
      fixed-window purchase is immune to catalog edits after the fact.

## Out of scope (named and rejected)

- **Splitting termination events into a separate table.** Rejected: fragments the single append-only fold source
      that is the grant ledger's whole value. Once the per-event-type CHECK enforces "only grants have windows,"
      the shared row shape is fine.
- **Giving ownership its own window-projection table with `revoked_at`.** Heavier than needed; reading the
      effective instant off the termination event closes the gap.
- **A generic bitemporal / temporal-table framework.** YAGNI — two named columns and one CHECK cover every current
      read.

Acceptance: a termination row can never carry `ends_at` (DB-enforced); entitlement gating and the derive/converge
invariants are unchanged; the grants module documents which column is which clock; and — if the scoped tasks are
taken — a backdated/grace subscription revoke records its effective instant as valid time and ownership
`revoked_at` reflects it, not the convergence wall-clock.

---

# #655: provider-account archive lifecycle, no routing role

**Completed:** yes
**Status:** COMPLETE 2026-06-30: replaced provider-account `routing`/primary/secondary/legacy semantics with
one lifecycle flag, `archived: true|false` (`false` by default). There is no longer a primary/secondary provider
account role: every non-archived account is available for new one-off purchases, new subscriptions, and other new
payment work. An archived payment-provider account is still operational for existing obligations, but is not
eligible for new checkout creation. Stripe's product/dashboard language uses "archive" for "no longer sell this
going forward while retaining historical records"; OpenRails should use the same operator-facing concept for
provider accounts.
REVIEW 2026-06-30 (Claude): direction endorsed — collapsing primary/secondary/legacy + enabled/disabled to one
drain-only flag is a real YAGNI win for a 1:1 org↔merchant world that usually runs ONE account per
(rail, environment); it lets us DELETE the primary-election machinery, not just rename it. Three things the plan
must fix (now folded into Open decisions + Tasks): (a) this is the 4th forward-only migration on this money-path
table in days (049 role→routing, 050 table rename, 051 global identity, now 052) and it REVERTS #649's
just-shipped `routing` — make #655 THE consolidation and freeze provider-account schema afterward; (b) `legacy`
ALREADY means "archived" (drain-only: old rows/rebills/refunds/webhooks), and there is a SEPARATE `status`
(enabled/disabled) column — #655 must resolve the fate of BOTH columns, not just `routing`; (c) dropping
`primary` removes the `uq_..._enabled_primary` ≤1 guarantee, so multi-account selection needs a deterministic
interim rule now, scoped to (rail, environment) — not wholly deferred to #288.

## Metadata
- Category: config
- Status: planned
- Passes: false

## Problem

`routing` is doing too much. `primary`/`secondary`/`legacy` mixes provider-account lifecycle with future processor
selection policy, and it is unclear which values still receive webhooks, rebills, dunning, catalog sync, checkout,
or subscription creation. The replacement is not another selection scheme: non-archived accounts are all eligible
for new work, and archived accounts are drain-only.

The actual lifecycle state needed here is simpler:

- **not archived**: usable for new one-off purchases, new subscriptions, saved-payment-method creation, and normal
  existing-subscription operations. If a merchant has multiple non-archived accounts on a rail, routing/fallback
  policy can choose among them later; the account lifecycle flag does not pick a "primary".
- **archived**: retained for existing users and historical/provider continuity. OpenRails still verifies webhooks,
  processes rebills, runs dunning, handles cancellation, reconciles provider state, lets users view/access saved
  payment methods, and allows safe existing-subscription operations. OpenRails must not route brand-new one-off
  purchases or brand-new subscriptions to this account.

Processor selection/fallback, if needed, belongs in #288 as an explicit routing policy over non-archived provider
accounts. It should not be embedded as per-account `primary`/`secondary` metadata.

The value of retaining archived accounts is migration safety. Existing subscriptions and saved payment methods can
be pinned to a provider account: provider subscription ids, customer vault ids, and stored-card tokens are not
generic OpenRails values. Stripe has formal data migration/export/import/copy tooling, but it is a migration
process with limits rather than simple object portability; Stripe also explicitly excludes Link credentials from
card exports. NMI docs expose Customer Vault, recurring, and transaction export/reporting surfaces, but do not
document a general export of vaulted payment credentials or subscriptions that OpenRails should depend on.
Therefore OpenRails should keep provider-account identity on subscriptions/payment methods and drive migration
gradually: create new work on non-archived accounts, keep old account-bound obligations running, and move users
only through explicit update/re-tokenization/import flows.

## Why an archived account keeps running — the drain model, and what's in/out of scope

Root cause: **payment credentials are not portable.** A subscriber on account A is bound to A by values only A
understands — the customer vault id / card token, the provider subscription id, the rebill schedule. You cannot
move that token to account B; the card was tokenized to A. So a merchant switching processors (relationship
ended, better rates, a high-risk account flagged, a banking change) cannot move its whole subscriber book at
once.

The model is a strangler/drain, and the ONLY migration path is gradual and per-user: **all NEW work goes to the
active account; the archived account keeps billing its EXISTING obligations until each subscriber's card
naturally lapses on A.** OpenRails never holds the card (it stays out of PCI scope — cards tokenize straight to
the provider), so it CANNOT move a subscriber to the active account on its own. A subscriber moves off A only
when their card lapses on A — it **expires, the rebill fails, or they cancel** — at which point they re-enter
their card, OpenRails captures it on the ACTIVE account, and bills forward from there. There is no proactive,
customer-silent mass move. (Stripe does offer a PCI account-to-account card-copy that would migrate cards
silently — but **by decision OpenRails does NOT use or build on it**: it's an offline Stripe-operated process,
often unavailable exactly when you're migrating off a terminated high-risk account. Migration is per-user card
re-entry, full stop.)

Why this is the ceiling: migration capability tracks WHO HOLDS THE CARD CREDENTIAL. When the provider owns the
vault + drives recurring (Stripe `pm_…`/subscriptions, NMI Customer-Vault id — gateway-scoped, CCBill recurring),
OpenRails is only a mirror, limited to lapse-then-recapture. Only when OpenRails holds the credential itself (its
own vault, or self-custody rails like Solana) could it re-point the processor proactively — not the case for
cards. The recapture always lands on "the active account on the same rail + environment", which is why Open
decision 2 (one deterministic active account) matters for migration, not just new checkout.

A's book drains over MONTHS–YEARS — you don't set the pace, card churn does (note Stripe's automatic card-updater
on A *slows* it by refreshing expiries; disable it on A to drain faster). When an archived account reaches ZERO
active subscriptions and no open obligations (no pending refunds, all dispute/chargeback windows closed) it is
fully drained and can be truly closed — `archived` has a TERMINAL state, it is not a roach-motel.

SCOPE BOUNDARY: #655 ships the lifecycle FLAG + PASSIVE DRAIN — the `archived` boolean, routing new work AND card
re-entry to the active account, keeping archived accounts live for existing obligations/inbound events, deleting
the primary-election machinery, and detecting the fully-drained terminal state. The only remaining migration
piece is a SMALL per-user hook (a SEPARATE follow-up, NOT an "engine"): when a subscriber re-enters their card at
a lapse point, capture it on the active account and run the cutover back-half — create the new rail subscription
anchored to the current period end, cancel the old one, re-point the OpenRails subscription's
`provider_account_id`. The data model already supports it (pinned per #641/#650). NO bulk import, NO silent mass
migration — ever.

## Open decisions (resolve before implementing)

1. **Fate of `status` (enabled/disabled) + the hard-off case.** The table has TWO lifecycle axes today: `routing`
   (primary/secondary/legacy) and `status` (enabled/disabled), and `legacy` already encodes exactly the
   "archived" (drain-only) meaning. RECOMMEND: collapse BOTH into the single `archived` boolean —
   `legacy`/`disabled` → `archived=true`; `primary`/`secondary` + `enabled` → `archived=false`. Do NOT keep a
   separate hard-off state: you never want to fully disable an account that still has bound obligations (its
   vault tokens / provider subscription ids live there); a compromised or provider-closed account is archived
   (drain-only) and its rebills fail naturally / lapse via dunning + credential revocation. Confirm this, or
   keep a distinct `disabled` and adopt a 3-state `active|archived|disabled` enum instead of a boolean.
2. **Selection when >1 non-archived account on the same (rail, environment).** Removing `primary` drops the
   unique index that guaranteed ≤1 eligible account. The common case stays unambiguous (one account per
   rail+environment; test vs live separated by `environment` + `test_mode`). For the >1 case, pick a
   DETERMINISTIC rule — recommend the NEWEST active account (the cutover target a migration points at) — and
   WARN, since >1 active on one (rail, environment) is normally a transient cutover window or a misconfig;
   never silently pick (same no-silent-default ethos as #651). A real multi-processor routing policy is #288.

## Tasks

- [x] Replace merchant config `routing` with `archived: true|false`, defaulting to `false`. Remove
      `primary`/`secondary`/`legacy` from examples, parser, dump, env overlays, docs, and tests.
- [x] Rename/replace DB lifecycle fields so provider accounts have a clear archived state. If keeping a physical
      `status` column internally is cheaper, expose only `archived` in config/API and document the DB mapping.
- [x] Update provider-account selection for new one-off purchases, new subscriptions, saved-payment-method
      creation, and catalog/new-sale targeting to require `archived=false`; do not select one "primary" account
      merely because it is first.
- [x] Preserve existing-obligation paths for archived accounts: webhook routing/verification, rebills,
      subscription renewal, dunning, cancellation, refunds/voids/chargebacks, reconciliation, and provider-origin
      backfills must still resolve and use archived accounts.
- [x] Ensure existing subscriptions/payment methods that already reference an archived provider account keep using
      that account unless an explicit user/admin migration changes them.
- [x] Audit all existing `provider_account_id` use on subscriptions, payments, payment methods, checkout sessions,
      vault records, webhook handlers, reconciliation, and dunning so archived accounts remain addressable wherever
      provider-bound objects already exist.
- [x] Add explicit checkout/subscription errors when the caller requests only archived provider accounts and no
      non-archived eligible account exists.
- [x] Update `dump-merchant-config` round-trip tests to emit `archived: true` only when true and omit it by default.
- [x] Add regression tests proving archived accounts reject new one-off/subscription creation but still accept
      inbound webhooks and existing-subscription rebill/dunning/cancel paths.
- [x] Update #288 wording, if needed, so processor routing/fallback chooses from non-archived accounts and never
      depends on old provider-account `routing` values.
- [x] DELETE the primary-election machinery: drop the `uq_payment_provider_accounts_enabled_primary` unique index
      and remove `PromoteProviderAccountToPrimary`, `DemoteOtherPrimaryProviderAccounts`, and the
      demote-on-insert logic — there is no "primary" anymore (this is a net deletion, not a rename).
- [x] Resolve the `status`/`disabled` column per Open decision 1 (recommended: fold into `archived`): retire or
      remap it in the same migration; do not leave two overlapping lifecycle columns on the table.
- [x] Implement the deterministic interim selection for >1 non-archived account on one (rail, environment) per
      Open decision 2; cross-reference #650 so archived accounts stay globally resolvable for inbound events.
- [x] Make #655 the consolidating change: do it as migration 052 (the last provider-account schema churn after
      049/050/051), and note in the issue that it supersedes #649's short-lived `routing` config surface.
- [x] Distinguish NEW payment-method creation (a new relationship → active account) from RE-TOKENIZATION / card
      update on an EXISTING archived-account obligation. The latter is allowed on the archived account to keep
      the drain alive (and is the migrate-on-touch trigger point) — "archived blocks saved-method creation" must
      NOT block an existing subscriber from fixing the card their archived subscription bills.
- [x] Detect + surface the fully-drained TERMINAL state: an archived account with zero active subscriptions and
      no open obligations (pending refunds, open disputes/chargeback windows) is safe to fully delete/close.
- [x] Follow-up issue (small, NOT an "engine"): the per-user cutover that runs when a subscriber re-enters their
      card at a lapse point (expiry / failed rebill / re-subscribe) — capture on the active account, create the
      new rail sub anchored to the current period end, cancel the old, re-point `provider_account_id`. NO bulk
      import, NO Stripe account-to-account card copy, NO proactive/silent mass migration — by decision. Keep #655
      to the flag + passive drain.

Acceptance: no public/operator config uses provider-account `routing`; `archived` is the only provider-account
lifecycle flag (the `status` enabled/disabled column is retired or folded in — no two overlapping lifecycle
columns); archived accounts remain live for existing obligations and inbound provider events; new purchases and
newly created subscriptions only choose non-archived accounts; the primary-election unique index +
Promote/Demote code are deleted and selecting among multiple non-archived accounts on a (rail, environment) is
deterministic; tests cover both sides of that boundary.

---

# #654: rename provider_accounts table to payment_provider_accounts

**Completed:** yes
**Status:** COMPLETE/SUPERSEDED into #652 (2026-06-30) — do NOT work this separately. The table rename
`provider_accounts` → `payment_provider_accounts` is the SAME DB surgery as #652's `provider_type` → `rail`
column rename (same table, same constraints/indexes/FKs/RLS, same sqlc/gen/repo churn). Splitting them into two
forward-only migrations + two code-churn passes over a money-path table is wasteful and riskier, so #652 now
does both in one migration. The naming rationale below still holds (it's the account REGISTRY, not a rail
catalog; never `payment_providers`); it just executes inside #652.
PLANNED 2026-06-30 (Paul): `payment_provider_accounts` is the clearer table name. The table is not a
catalog of rails/providers (`stripe`, `nmi`, `ccbill`, `solana`); it is the merchant-owned account registry for
payment providers on rails. Do not rename it to `payment_providers`.

## Metadata
- Category: data-integrity
- Status: superseded
- Passes: true

## Problem

`openrails.provider_accounts` is vague after the naming cleanup in #652. Each row is one merchant payment-provider
account on one rail, e.g. `doujins + nmi + 579145` or `doujins + stripe + acct_...`. The current name is workable,
but `payment_provider_accounts` is more accurate and avoids confusing this table with a future provider/rail
catalog. The rename should be done once, cleanly, and only if we are willing to pay the migration/query/FK churn.

The rename does not change identity: the durable logical identity remains `(rail, environment, account_id)`; the
row UUID remains an internal FK target; any config `key` remains mutable operator metadata.

## Tasks

- [ ] Rename table `openrails.provider_accounts` to `openrails.payment_provider_accounts` in a forward-only
      migration.
- [ ] Rename associated constraints, indexes, RLS policy comments, FK constraint names, and generated query names
      where practical. Prefer clear names over preserving historical `provider_accounts_*` leftovers.
- [ ] Update sqlc queries, generated models, repo code, reconciliation, webhook, catalog push, merchant config
      push/dump, and tests to use `payment_provider_accounts`.
- [ ] Keep public/operator naming aligned with #652: column currently named `provider_type` should become `rail`
      in the same pass if feasible; otherwise explicitly track it as remaining debt.
- [ ] Preserve all existing FKs and behavior: payments/subscriptions/payment_methods/checkout_sessions/intents/
      refresh watermarks continue to reference the same rows after migration.
- [ ] Add an integration migration/query test proving existing rows survive the rename and all FK-backed reads
      still work.
- [ ] Do not expose the internal UUID as the preferred public identity; keep APIs oriented around
      `(rail, environment, account_id)` or rail-qualified strings like `nmi:579145`.

Acceptance: the DB table and generated code use `payment_provider_accounts`; no operator-facing docs call it
`payment_providers`; row identity remains `(rail, environment, account_id)`; existing FK relationships still work;
and tests prove migration from `provider_accounts` preserves rows and references.

---

# #653: merchant billing-config mechanics — koanf map-shaped config, plain-value secrets, symmetric redacted dump

**Completed:** yes
**Status:** COMPLETE 2026-06-30: merchant billing config is now map-shaped and koanf-loadable, provider
secrets are plain strings with one canonical key per rail, provider account settings are separate from secrets,
and dump emits the same typed config shape with secrets redacted by default (`--include-secrets` is explicit).
PLANNED 2026-06-30 (split out of #649, which over-grew past its "rename" title). Treat the
merchant/provider-account manifest as ordinary koanf-loaded runtime config: map-shaped (not arrays), secrets as
plain resolved values with koanf owning source precedence, one canonical key per secret, non-secret config
under `settings`, and a dump that is the exact reverse of the typed config (redacted by default). Depends on
#652 for the naming vocabulary (rail / provider-account `key` / `account_id`) and relates to #650 (identity).

## Metadata
- Category: config
- Status: complete
- Passes: true

## Problem

The merchant manifest carries a bespoke secret-source DSL (`{value|env|file|vault}` per secret), array-indexed
identity (`merchants[0].provider_accounts[2]`), `Manifest*` struct names, and a dump shape that isn't symmetric
with input. That fights koanf (built to merge file/env/flags/Vault into one tree), makes env/file overrides
position-coupled, risks leaking secrets on dump, and accepts multiple spellings for one secret (NMI
`production_key`/`secret_key`/`security_key`).

## Tasks

- [x] One canonical key per provider secret; reject aliases — NMI `security_key` only (not `production_key`/
      `secret_key`); Stripe `secret_key`/`webhook_signing_secret`/`webhook_signing_secret_thin`; CCBill
      `salt`/`datalink_username`/`datalink_password`; Solana `private_key`. (NMI research: `security_key` is
      NMI's own wire/API term; test accounts also use a security key, so lifecycle names like
      `production_key` are wrong.)
- [x] Replace `ManifestSecretSource` with `Secrets map[string]string`; koanf/provider loading owns source
      precedence BEFORE manifest validation. No `{value|env|file|vault}` descriptors in YAML.
- [x] Arrays → maps: `merchants.<slug>` + `provider_accounts.<key>`; drop redundant `slug`/`name` fields (the
      map key is the handle). Map-shaped typed blocks `provider_accounts.<key>.<rail>` (e.g. `…mobius.nmi`).
      (Names `key`/`rail` per #652.)
- [x] Rename koanf-loaded structs `Manifest*` → `*Config` (`BillingConfig`, `MerchantConfig`, …); keep
      `Manifest` only for a standalone CLI envelope if one remains. (See "ProviderAccountConfig collision" below.)
- [x] `settings` map beside `secrets` for non-secret account config (NMI `tokenization_url` +
      `tokenization_key`; CCBill `allowed_cidrs`), with CCBill `account_id` carrying
      `client_acc_num/client_sub_acc` and CCBill `salt`/DataLink credentials as typed secrets. CCBill
      FlexForm/RBO stay catalog provider-links (per-price), never account settings.
- [x] Dedicated single-underscore `BILLING_` env mapper (schema-aware), exported as a reusable helper for
      embedded hosts (doujins/hentai0/cozy) to attach to their own koanf env callback. (See ambiguity below.)
- [x] Mounted Vault/secret files as structured YAML/JSON overlays with the same `billing.*` shape, merged after
      the public config (which may leave secret leaves blank). Choose an explicit koanf provider
      (`k8smount`/Vault) or a documented+tested filename→key mapping — don't assume a koanf convention.
- [x] Symmetric dump: `dump-merchant-config` emits the SAME typed map-shaped `*Config` structs as input (no
      arrays, no `provider_type`/`name`, no `{env:…}` placeholders); secrets redacted by default; plaintext only
      via an explicit `--include-secrets` flag, with regression coverage that a normal dump can't leak secrets.
- [x] Round-trip tests: load → apply → dump → load, asserting stable non-secret shape + account identities
      (sorted map keys — see below).
- [x] Route merchant-config loading through the same koanf pipeline as runtime config; no `os.Getenv`/secret-file
      reads inside the manifest parser.

## Validation

- `task sqlc`
- `go test ./...`
- `go test -tags integration ./internal/bootstrap ./embed ./internal/db/querytest ./internal/http -run 'TestMerchantConfig|TestReconcileMerchantManifest|TestUpsertMerchantConfig|TestPaymentProviderAccountIdentityIsGlobal|TestMerchantWebhookRouteHTTPResolvesMerchantBeforeVerifyingStripe' -count=1`

## Open design questions (resolve before building)

- **`BILLING_` env mapper ambiguity.** Single underscore + arbitrary map keys is ambiguous:
  `BILLING_MERCHANTS_MY_MERCHANT_PROVIDER_ACCOUNTS_MOBIUS_2_NMI_SECURITY_KEY` — is the slug `my` or
  `my_merchant`? the account `mobius` or `mobius_2`? The mapper must anchor on FIXED schema tokens (`MERCHANTS`,
  `PROVIDER_ACCOUNTS`, the rail blocks `NMI`/`STRIPE`/`CCBILL`/`SOLANA`, known field names) and treat spans
  between them as keys — and the key charset MUST be constrained + documented (e.g. lowercase, no underscores,
  never a reserved token). This is the fiddliest part; decide + test it explicitly.
- **`ProviderAccountConfig` name collision.** `config.ProviderAccountConfig` ALREADY exists (the in-process
  railSet entry, #641). Decide: does the koanf-loaded merchant-config provider account UNIFY with it (one
  struct, one source of truth — preferred), or stay separate? If separate, the names MUST differ — two
  same-named structs for the same concept is exactly the confusion this issue removes.
- **Dump symmetry vs map order.** Go map iteration is unordered — dump must sort merchant/account keys for
  stable, diffable, round-trippable output.
- **DO NOT regress multi-merchant secret isolation — this is the load-bearing decision.** Today the STANDALONE
  multi-merchant runtime loads each merchant's secrets PER-REQUEST from an encrypted store (per-merchant DEK,
  #227; `LoadStripeCredentials` is explicitly "loaded by merchant id at request time, NOT injected
  process-wide"). The `{env|file|vault}` manifest descriptors are SEED-time instructions — push-merchant-config
  resolves them and stores the value encrypted — not the runtime source. So this koanf model must be scoped to:
  (a) SEED-time value resolution for push-merchant-config (koanf reads env/file/Vault → store encrypted), and
  (b) the runtime secret source for EMBEDDED single-merchant hosts (one merchant — config/env is fine and
  simpler). It must NOT become "load every merchant's plaintext secret into the process config tree at boot"
  for the standalone SaaS — those stay in the per-merchant encrypted store, fetched per-request. Decide and
  document which path koanf feeds before touching the loader.

Acceptance: in the manifest/seed surface and the embedded single-merchant runtime, secret values are plain
strings resolved by koanf providers (no `{env|file|value|vault}` descriptor objects) — and the standalone
multi-merchant runtime still loads secrets per-merchant from the encrypted store, never the process config tree;
exactly one canonical key per provider secret (aliases fail validation); merchant/provider-account identity is
map-keyed (no numeric array indexes); non-secret account config lives under `settings`; the `BILLING_` mapper is
a documented, exported, tested helper that unambiguously parses map keys; dump uses the same typed structs as
input, redacted by default with opt-in plaintext; and load⇄dump round-trips stably.

---

# #652: normalize payment naming — rail vs provider account

**Completed:** yes
**Status:** COMPLETE 2026-06-30 (uncommitted): implemented the hard DB/query/code naming cut for
`provider_accounts` -> `payment_provider_accounts` and `provider_type` -> `rail` in one migration (#654 folded
here), regenerated sqlc, updated merchant config/examples/tests, and tightened credential helper naming around
rail/account identity. `#650` then builds on this table/column shape.
PLANNED 2026-06-30 (Paul): OpenRails should use one naming model consistently. A `rail` is the
integration/backend/protocol OpenRails speaks (`stripe`, `nmi`, `ccbill`, `solana`, future
`authorize_net`). A merchant payment provider is a configured provider account on that rail: usually the same
name as the rail for single-account providers, but distinct for multi-processor rails such as NMI
(`mobius`, `paykings`). Stop using `provider_type`, `provider`, `payment provider`, and `rail` interchangeably.
HARD CUT (no legacy, no aliases): `provider_type`/ambiguous `provider` become `rail` EVERYWHERE — DB column,
structs, DTOs, YAML, route params, comments — with no compatibility column/view/field alias and no
canonicalizing shim (same discipline as #649's `role`→`routing`). This issue OWNS the canonical struct/field
names; it should land FIRST in the cluster (#650 identity and #653 config-mechanics build on this vocabulary).

## Metadata
- Category: config
- Status: complete
- Passes: true

## Problem

The current codebase still mixes three concepts:

- **Rail**: the backend family/protocol (`nmi`, `stripe`, `ccbill`, `solana`). This is what selects code paths,
  webhook handlers, provider clients, catalog push adapters, and credential validation rules.
- **Provider account / payment-provider account**: one merchant-owned account on a rail. Examples:
  `mobius` and `paykings` are two arbitrary local keys for NMI provider accounts; a Stripe account may simply be
  keyed `stripe`.
- **External account identity**: the durable provider-native ID (`acct_...`, NMI gateway/merchant id, CCBill
  client/subaccount, Solana signing-authority public key). This is `account_id` and is not a display/config name.
  For Solana specifically, `account_id` is the rebill/cranking authority public key, because existing rebill
  accounts are tied to that signer; recipient/treasury wallets are settings, not provider-account identity. When
  a single string identity is needed, qualify it with the rail, e.g. `nmi:579145` or
  `solana:<signing-authority-pubkey>`, rather than inventing a random public UUID. Include `environment` in
  uniqueness where live/test accounts can share identifiers.

The old `provider_type` name blurs rail and provider account. It is especially wrong for NMI because `nmi` is
not the merchant's payment provider in the operational sense; `mobius` or `paykings` is the configured payment
provider account on the NMI rail. The same pattern will likely matter for future rails like Authorize.net.

## Tasks

- [ ] Write the naming glossary into config/API docs and examples: `rail`, provider-account local key,
      `account_id`, `routing`, `environment`.
- [ ] Standardize on `key` for the operator-chosen local handle when it must be represented as a field. In the
      YAML map shape, the map key is the `key`; it is encouraged to be stable for env/secret overlays and diffs,
      but it is not provider identity.
- [ ] Rename every field/param/tag that means backend family from `provider_type` / ambiguous `provider` to
      `rail` — public AND internal, no exceptions, no alias kept for either spelling.
- [ ] Keep provider-account local names as map keys (`mobius`, `paykings`, `stripe`) rather than duplicate
      `name:` fields. Use optional `display_name` only if a prettier UI label is needed.
- [ ] Audit structs, comments, CLI flags, JSON/YAML tags, route params, and examples for ambiguous naming:
      `PaymentProviderConfig`, `ProviderAccountConfig`, embedded `PaymentProviders`, webhook `Rail()`,
      provider-account DB/query comments, merchant-config dump/push paths.
- [ ] DECIDE THE CANONICAL STRUCT/FIELD NAMES HERE — this issue is the naming authority; #653 only applies them.
      Resolve the `ProviderAccountConfig` collision: `config.ProviderAccountConfig` already exists (the in-process
      railSet entry, #641). Either UNIFY it with the koanf merchant-config provider-account struct (one type, one
      source of truth — preferred) or give them distinct names. Two same-named structs for the same concept is
      exactly the confusion this issue removes.
- [ ] HARD-RENAME the provider-accounts table+column in ONE migration (folds in #654): table
      `openrails.provider_accounts` → `openrails.payment_provider_accounts` AND column `provider_type` → `rail`,
      together with their constraints/indexes/FK names/RLS comments + sqlc query names + generated structs +
      fixtures + the `ProviderAccountSecretName(rail, …)` scheme + query comments. ONE forward-only migration and
      ONE code-churn pass over the table — not two (renaming the same table's column and the table itself in
      separate passes doubles the FK/query churn on a money table). No legacy column/table, no compatibility view
      — same hard cut as #649's `role`→`routing`. The durable logical identity is `(rail, environment,
      account_id)`, never the local `key` and never a random external UUID. (An integration test must prove
      existing rows + all FK-backed reads survive the rename — see #654.)
- [ ] Make the planned map-shaped config use rail as the typed block key:
      `provider_accounts.<local_key>.<rail>`, e.g. `provider_accounts.mobius.nmi`.
- [ ] Update env mapper planning/tests so rail and provider-account keys are parsed separately:
      `BILLING_MERCHANTS_DOUJINS_PROVIDER_ACCOUNTS_MOBIUS_NMI_SECURITY_KEY`. (The `<key>.<rail>` shape helps the
      #653 env-mapper: the rail block — `nmi`/`stripe`/`ccbill`/`solana` — is a FIXED anchor token between the
      arbitrary local key and the field name, which is what makes the single-underscore key parseable.)
- [ ] Add tests or static assertions for config parse/dump examples proving two NMI provider accounts can exist
      under different local keys on the same `nmi` rail without treating `mobius`/`paykings` as rail names.
- [ ] Define Solana provider-account identity explicitly: `account_id` is the signing authority public key used
      for rebill/cranking, and startup validation must prove the configured signer resolves to that public key.
      Recipient/treasury/funding/RPC values are account settings, not identity. NOTE: this is a hard change from
      the CURRENT convention — fixtures + `merchants.example.yaml` use the RECIPIENT WALLET as the Solana
      `account_id` — so migrate existing Solana `account_id`s to the signer pubkey and update all examples/fixtures
      (coordinate with #650, which owns provider-account identity/uniqueness).

Acceptance: config, docs, AND the DB use `rail` for backend family — grepping provider-account code/queries/DB
for `provider_type` or `provider`-meaning-rail finds nothing (no alias survives anywhere, public or internal);
provider-account `key`s name merchant payment providers; examples show `mobius`/`paykings` as arbitrary stable
local `key` values on the `nmi` rail, not rails; `account_id` is the provider-native identity (Solana = signer
pubkey, NOT recipient wallet), with rail-qualified strings like `nmi:579145` preferred over random UUIDs; no field
carries both a rail discriminator and a typed rail block; exactly one struct per concept (no duplicate
`ProviderAccountConfig`); and future multi-account rails follow the same shape without a new naming layer.

## Implementation Progress (2026-06-30)

- [x] Added `050_payment_provider_accounts_rail.up.sql`: table rename, `rail` column rename, constraints/indexes/FK
      names, and comments in one forward-only migration.
- [x] Replaced provider-account sqlc query/gen files with `payment_provider_accounts` / `Rail`.
- [x] Updated runtime/reconcile/bootstrap/embedded tests and examples to use `rail` and the renamed table.
- [x] Kept the config collection name `provider_accounts` because it names configured accounts, not the DB table.
- [x] Validation: `task sqlc`; `go test ./...`; focused integration below under #650.

---

# #650: globally unique provider-account identity, route merchant from provider account

**Completed:** yes
**Status:** COMPLETE 2026-06-30 (uncommitted): added global `(rail, environment, account_id)` uniqueness with
preflight duplicate detection, updated upserts to reject cross-merchant ownership conflicts, added global account
lookup, and made standalone canonical webhooks derive merchant/account routing from provider account identity:
NMI by `event_body.merchant.id`, CCBill by `clientAccnum/clientSubacc`, and direct Stripe by
`/webhooks/stripe/:account_id`. Merchant-scoped routes remain transition aliases and still reject mismatched
accounts by failing local account lookup. Added real Postgres integration coverage for duplicate account
ownership and account-derived webhook routing. PLANNED 2026-06-30 (Paul): a payment-provider account must belong to exactly one merchant. If an
inbound webhook or provider callback carries a provider-native account id, OpenRails should resolve the provider
account first and derive the merchant from that row, instead of requiring both merchant and provider-account in
the route. Every provider interaction must make the payment-provider account explicit from the route, request
payload, provider payload, or deterministic routing-policy output before calling/recording against a provider.
Webhook routes should be one canonical endpoint per provider/rail when the provider event carries account
identity. Stripe direct-account webhooks are the exception: they need an account-id route because normal Stripe
events do not carry `acct_...` in the body.
This also prevents ambiguous cross-merchant ownership of the same Stripe/NMI/CCBill account.

## Metadata
- Category: data-integrity
- Status: complete
- Passes: true

## Problem

Provider accounts are real external accounts, not reusable OpenRails config labels, and sharing them across
merchants is not allowed. A Stripe `acct_...`, NMI
gateway/merchant id, CCBill `clientAccnum/clientSubacc`, or Solana signing-authority public key should be owned
by exactly one merchant in an OpenRails deployment. The stable logical identity is the natural provider identity
qualified by rail and environment, e.g. `(rail=nmi, environment=live, account_id=579145)` or external string form
`nmi:579145`. For Solana, `account_id` is the signer authority public key that owns/controls rebill cranking;
changing it is a provider-account migration because old rebill accounts remain tied to the old authority. The
local config `key` is mutable metadata and must not be the identity. Today
`openrails.provider_accounts` only enforces uniqueness inside one
merchant: `(merchant_id, provider_type, environment, account_id)`. That allows the same provider account id to be
registered under two merchants, which makes webhook routing, provenance stamping, catalog pushes, refunds, and
reconciliation ambiguous.

If provider account identity is unique globally, a webhook that includes provider account identity does not also
need a merchant path segment. The account row already points to the only merchant that may own it. Provider type
alone (`stripe`, `nmi`, `ccbill`) is not enough; provider account identity is enough. The canonical webhook shape
should therefore be provider-only for NMI and CCBill (`/webhooks/nmi`, `/webhooks/ccbill`) because their payloads
carry account identity. Direct Stripe webhooks should stay account-scoped (`/webhooks/stripe/:account_id`) because
normal Stripe direct-account events do not carry the account id. Merchant-scoped paths are redundant once the
handler can derive account→merchant; keep them only as transition aliases, if needed, and have them validate that
any route merchant/account matches the resolved provider account.

The provider account is the primary routing fact. Merchant is derivable from it; the reverse is not true when a
merchant has multiple accounts on one rail. So boundaries must resolve provider-account identity first whenever
the request/event contains it, and only derive/check merchant after that.

## Tasks

- [x] Add a forward-only Postgres migration replacing `uq_payment_provider_accounts_identity` with a global
      uniqueness constraint/index on `(rail, environment, account_id)`.
- [x] Add a preflight duplicate check before the constraint is created, with a clear error listing duplicated
      `(rail, environment, account_id)` values and their merchant ids.
- [x] Update `UpsertProviderAccount` conflict handling to use `(rail, environment, account_id)` and reject
      cross-merchant upserts instead of silently moving or merging ownership.
- [x] Treat the provider-account natural key as `(rail, environment, account_id)` in APIs and
      provenance. Do not expose a random UUID as the preferred external identifier; if a compact string is needed,
      use a rail-qualified account id such as `nmi:579145` (plus environment where ambiguous).
- [x] For Solana, validate that the configured signer public key equals `account_id`. Store recipient/treasury
      wallets separately as settings; do not use them for provider-account identity or uniqueness.
- [x] Add a lookup query for provider account by provider-native identity without merchant id, returning the row
      and owning `merchant_id`.
- [x] Audit all provider-touching HTTP/worker/CLI boundaries and classify how provider-account identity is made
      explicit: route parameter, request payload, provider webhook payload/header, or routing-policy output.
      Fix any boundary that only has merchant + rail/provider type.
- [x] Make the canonical NMI and CCBill webhook routes provider-only (`/webhooks/nmi`, `/webhooks/ccbill`), with
      no merchant slug and no provider-account id in the path.
- [x] Keep canonical direct Stripe webhooks account-scoped (`/webhooks/stripe/:account_id`) because ordinary
      Stripe direct-account events do not include `acct_...` in the event body. Stripe Connect/org webhooks may
      use provider-only routing when account/context is present in the payload.
- [x] Use account-derived webhook routing before dispatch: CCBill by `clientAccnum/clientSubacc`, NMI by
      merchant/gateway id, direct Stripe by route `:account_id`, and Stripe Connect/org by payload account/context.
- [x] For merchant-scoped account routes, verify the provider account owner matches the path merchant; reject
      mismatches instead of treating the path merchant as authoritative.
- [x] Keep merchant/account-id webhook URL forms only as transition aliases or for a provider event class that
      cannot be safely resolved by payload identity. Do not identify Stripe accounts by trying configured webhook
      secrets until one verifies; direct Stripe account identity must come from the route.
- [x] Checkout and saved-payment-method creation must resolve one concrete provider account before calling a
      provider. `payment.rail`/`provider` names the rail; it is not enough when a merchant has multiple accounts
      on that rail. If routing policy chooses an account, record that chosen account; do not call NMI/Stripe/etc.
      through a primary-account fallback and leave `provider_account_id` blank.
- [x] Make saved-payment-method creation validate provider/rail at the handler boundary, and require either a
      concrete provider account or enough routing context to select one. Do not accept an optional `provider`
      field only to let vault creation fail later.
- [x] Add integration tests proving duplicate provider accounts across two merchants fail, same provider account
      under the same merchant still upserts, and account-derived webhook routing resolves the merchant correctly.
- [x] Make fixtures/examples use DISTINCT `account_id`s per merchant. Global uniqueness will reject the same
      `(rail, environment, account_id)` under two merchants, and current test fixtures/example manifests REUSE
      placeholder ids (e.g. the integration suite's NMI `579145`, `merchants.example.yaml`'s ids); seeding two
      merchants with a shared id would now fail. Sweep them before the constraint lands.
- [x] This issue REPLACES the #641 webhook routing: the merchant-scoped per-account path
      (`/merchants/:merchant/webhooks/:provider/:account_id`) becomes a transition alias, and canonical routing
      becomes provider-only for NMI/CCBill + account-scoped for direct Stripe (per the tasks above). Migrate the
      #641 handler + `LoadNMIWebhookSigningSecretForAccount`/`LoadStripeCredentialsForAccount` paths onto
      account→merchant resolution rather than leaving two parallel routing models.
- [x] SEQUENCE after #652 (+ its folded #654 rename): this issue's migration and the account-lookup query name
      `rail`/`payment_provider_accounts` — build on the
      renamed names, don't write the old ones and re-migrate.

Acceptance: the same `(rail, environment, account_id)` cannot exist under two merchants; provider-account
lookups can derive merchant ownership directly from provider-native account identity; canonical NMI/CCBill
webhooks are one endpoint per provider/rail and derive provider account→merchant from payload; canonical direct
Stripe webhooks use `/webhooks/stripe/:account_id`; merchant-scoped webhook URL forms, if retained, are transition
aliases that reject mismatches; checkout/vault creation records the provider account it actually uses; and
provenance stamping uses the resolved provider-account row, never a separate merchant guess.

## Implementation Progress (2026-06-30)

- [x] Added `051_payment_provider_account_global_identity.up.sql` with duplicate preflight and global unique
      `(rail, environment, account_id)`.
- [x] Updated generated `UpsertProviderAccount` and the hand-written merchant admin upsert to conflict on the
      global natural key and no-op/error on cross-merchant collisions.
- [x] Added `GetProviderAccountByRailIdentity` and `Service.ResolvePaymentProviderAccountByIdentity`.
- [x] Added canonical `/webhooks/:provider/:account_id` route for direct Stripe account webhooks.
- [x] Added no-merchant global webhook resolution for NMI, CCBill, and Stripe, pinning both merchant id and
      internal provider-account id into request context before dispatch.
- [x] Added real Postgres integration tests:
      `go test -tags integration ./internal/db/querytest -run 'TestPaymentProviderAccountIdentityIsGlobal|TestQueryContractsHighValueBillingDomains' -count=1`
      and
      `go test -tags integration ./internal/http -run 'TestMerchantWebhookRouteHTTPResolvesMerchantBeforeVerifyingStripe' -count=1`.
- [x] Full non-integration validation: `go test ./...`.

---

# #649: hard-rename provider-account `mode` to `routing` everywhere

**Completed:** yes
**Status:** COMPLETE 2026-06-30: the provider-account routing concept is named `routing` end-to-end (manifest
field + DB column + structs/DTOs/dump/validation), no `mode`/`role` alias; and `provider_account_id` is never
invented from routing — it records only an explicitly observed/pinned external account.
NARROWED 2026-06-30: this issue had accreted a whole manifest/secret/config-loading redesign well past its
"rename" title. That redesign is split into #653 (koanf map-shaped billing config + plain-value secrets +
symmetric redacted dump). See also #652 (rail vs provider-account naming), #651 (stop fabricating provider
data — broader audit), #650 (global provider-account identity + account→merchant routing).

## Metadata
- Category: config
- Status: complete
- Passes: true

## Problem

Two coupled hard cuts:

1. **`mode`/`role` → `routing`.** `provider_accounts[].mode` (manifest) and the DB `role` column were vague —
   `mode` collides with signer mode, `provider_write_mode`, and pricing-tier mode. Rename the routing concept
   (primary/secondary/legacy, with disabled via `status`) to `routing` everywhere — structs, YAML, DB column,
   DTOs, dump, validation errors, tests, docs — with NO compatibility alias.
2. **Provenance is not routing.** `provider_account_id` on payments/subscriptions/payment_methods must be the
   external account that ACTUALLY produced the row, not the configured `primary` defaulted in. Stamping an
   internal config choice as historical fact is wrong for an audit/money system: if the producer can't be
   resolved, leave it unset (or fail at the boundary), never guess.

Everything else that previously lived here — one-canonical-key-per-secret, `ManifestSecretSource` → plain
values, arrays → maps, `Manifest*` → `*Config`, koanf/Vault overlays, the `BILLING_` env mapper, `settings`,
and the symmetric redacted dump — moved to **#653**.

## Tasks

- [x] Rename DB `provider_accounts.role` → `routing` (migration, sqlc, generated structs, fixtures, API DTOs).
- [x] Rename manifest field `mode` → `routing` (structs, YAML tags, examples, dump, validation errors, tests).
- [x] Rename helpers/locals/comments (`manifestProviderAccountMode`, `providerAccountModeFromRoleStatus`, …).
- [x] Update `config/merchants.example.yaml` + dump round-trip to emit `routing`.
- [x] Hard-cut: `provider_accounts[].mode` fails unknown-field validation (no alias).
- [x] Remove primary-routing fallback stamping; `resolveProviderAccountIDForStamp` returns ONLY the
      context-pinned account. `GetPrimaryProviderAccount` is for default selection, not provenance.
- [x] Keep unrelated `mode` concepts unchanged (Solana `signer.mode`, `provider_write_mode`, tiered-pricing
      `mode`, AuthKit remote-app mode, invoice/collection policy mode).
- [x] `go test ./...` + focused integration (manifest push/dump, stamping, query contracts, provisioning).

Acceptance: provider-account config/DB/docs use `routing` (no `mode`/`role` routing uses); `mode` under
`provider_accounts[]` fails as unknown; DB persistence is `provider_accounts.routing` + `status`;
`provider_account_id` is never derived from `routing=primary` and is stamped only from an observed account.

---

# #647: catalog as round-trippable YAML — push ⇄ dump, 1:1 Go↔YAML catalog structs

**Completed:** yes
**Status:** DONE 2026-06-30: `dump-merchant-catalog` / `dump-catalog` now exists and exports
OpenRails-owned catalog DB state back into the push-catalog YAML envelope (`version` + `catalogs[]`) using
the existing typed `catalog.Manifest` structs. It reads products/prices/meters/rate_cards/credit_balances/
usage_limits/product includes/credit-purchase prices and marshals the existing push shape. Added a real
testcontainer push→dump→reparse→push→dump integration test for a canonical catalog fixture; the round-trip
dump is stable. `config/catalog.example.yaml` is now covered as the faithful provider-backed round-trip
fixture by applying it under readonly provider mode, dumping every merchant, reparsing, reapplying, and
asserting the second dump is stable.
PROPOSED 2026-06-30 (Paul): like the merchant config (#646), the CATALOG (products / prices /
meters / rate cards / credit balances) should round-trip — `push-catalog` applies a YAML, and a new
`dump-catalog` exports a merchant's live catalog back into the SAME shape, with the Go structs 1:1 with the
YAML so marshal/unmarshal are symmetric. Paul: "this design may not be a good idea after later inspection,
but I like the idea anyway." Builds directly on #645 (typed catalog structs / 1:1 Go↔YAML), which is the
prerequisite — once the catalog structs are clean typed sub-blocks, a dump is just reading the sidecars back
into them.

## Metadata
- Category: devex
- Status: done
- Passes: true

## Problem

`push-catalog` applies `config/catalog.example.yaml` (products → prices/meters/rate cards/credit balances)
but there is no `dump-catalog`: you cannot serialize a merchant's live catalog back to the manifest shape, so
the file and the DB drift, and there is no export-to-review/version/clone for catalogs the way #646 gives for
merchant config. #645 is making the catalog structs typed + 1:1 with YAML, which is exactly what a faithful
dump needs.

## Tasks

- [x] (depends on #645) Confirm the catalog Go structs are 1:1 with the YAML (typed price sub-blocks, credit
      balances, rate cards) so marshal(parse(x)) == x for a canonical catalog.
- [x] Add `dump-catalog --slug <m>` (CLI + a service read path): read the catalog sidecars
      (products / prices / meters / rate_cards / credit_balances / credit_purchase_prices) and rebuild the
      `catalog.Manifest`, marshal to YAML. Mirror #646's `dump-merchant-config`.
- [x] Round-trip integration test (testcontainers): push the example catalog → dump → re-parse → assert the
      manifest is structurally equal (modulo ordering / defaults).
- [x] Make `config/catalog.example.yaml` a faithful round-trip fixture.

Acceptance: `dump-catalog` exports a merchant's catalog into the push-catalog YAML shape; push→dump→push is
idempotent; the catalog Go structs are the single 1:1 source for both directions. Depends on #645.

---

# #648: Solana signer — injected keypair OR HashiCorp Vault transit signing (key never leaves the vault)

**Completed:** yes
**Status:** DONE 2026-06-30: manifest Solana provider accounts can now declare `signer:
{mode: vault_transit, key: ...}` or use `secrets.private_key` / `signer: {mode: keypair}`. Push validates
Vault transit public key == `account_id` and stores signer mode in provider-account evidence; dump emits the
transit reference, never key material. Runtime recurring Solana services now use one provider-account-aware
signer: declared Vault transit signs via Vault; keypair mode reads the provider-account scoped private_key
with legacy `solana/private_key` fallback. Added an integration test proving provider-account evidence selects
the named Vault transit key for public-key lookup and signing, plus the `vaultint` live test now exercises the
OpenRails Solana signer wrapper over Vault Transit. Targeted unit/integration tests pass.
PROPOSED 2026-06-30 (Paul): a Solana provider account's signer keypair should be suppliable two
ways: (a) a keypair injected directly (e.g. an `env:`/`file:` secret, as today), OR (b) HashiCorp Vault
transit SIGNING — the private key NEVER leaves the vault; OpenRails asks Vault's API to produce signatures.
"that should definitely be planned and done too."

## Metadata
- Category: billing
- Status: done
- Passes: true

## Problem

Today a Solana account declares its `private_key` as a manifest secret (`{file: …}` / `{env: …}`), so the
raw Ed25519 key is materialized into OpenRails to sign on-chain transactions. For higher-assurance
deployments the key should stay in HashiCorp Vault: the Vault transit engine holds a non-extractable Ed25519
key and signs on request, so OpenRails never holds the private key. There is ALREADY groundwork —
`internal/integrations/vault/transit.go` creates a per-merchant Ed25519 transit key as non-extractable
(exportable=false) and can sign via Vault. The gap: the merchant manifest can't EXPRESS "this Solana
account's signer is a vault-transit key (sign-via-vault)" vs "an injected keypair", and the Solana signer
path doesn't branch on it.

## Target design

- **Manifest:** let a Solana provider account's signer declare its mode — either an injected keypair
  (`secrets.private_key: {file|env|value}`, today's path) or a vault-transit signer
  (e.g. `secrets.private_key: {vault_transit: <key-name>}`, or a `signer: {mode: vault_transit, key: …}`
  block). The account_id (recipient wallet, #592) stays operator-declared either way.
- **Signer path:** when the account is vault-transit, the Solana rail signs by calling Vault transit sign
  (reuse `internal/integrations/vault/transit.go`) instead of loading a local keypair. Validate at boot that
  the transit key exists / its public key matches the declared wallet.
- **Dump (#646):** emit the vault-transit reference (key name), never key material — consistent with the
  secrets-as-refs rule.

## Tasks

- [x] Extend the manifest provider-account secret/signer shape to express vault-transit signing for Solana
      (decide `vault_transit` secret source vs a `signer` block; validate exactly-one signer mode).
- [x] Branch the Solana signer to sign via Vault transit when declared; keep the injected-keypair path.
- [x] Boot-time validation: the transit key exists and its Ed25519 public key matches the declared wallet.
- [x] dump-merchant-config emits the transit reference (no key material).
- [x] Integration test: a vault-transit Solana account signs a transaction via Vault (no local private key),
      and the injected-keypair path still works.

Acceptance: a Solana account can be configured for EITHER an injected keypair OR Vault transit signing (key
never leaves the vault); both produce valid signatures; the manifest + dump express the choice; secrets/key
material are never dumped. Reuses `internal/integrations/vault/transit.go`.

---

# #646: merchant config as round-trippable YAML — push ⇄ dump the complete merchant_configurations, realistic test+live example

**Completed:** yes
**Status:** DONE 2026-06-30 (commit 8125c6f2, pushed; branch issue-646). IMPLEMENTED: the manifest applies the
COMPLETE merchant_configurations payload — added `invoice` (collection_threshold/monthly_floor/
billing_period_boundary) + `delegated_invoker_wasted_spend_windows` to `ManifestMerchant`, wired into the same
merchantconfig store the profile uses (omit = leave-as-is). Provider accounts gained a human `name`/alias label
(persisted as display_name; account_id stays the routing id). New `dump-merchant-config --slug` CLI +
`DumpMerchantConfig` serializes a live merchant back into the manifest shape — secrets as `env:` references
(never values), so push⇄dump is round-trippable; issuer lives in AuthKit (#480/#481), out of the dump.
`merchants.example.yaml` rewritten: realistic, self-describing header, every field with its default, TEST+LIVE
provider-account pairs (NMI mobius + Stripe) with test_mode selecting the active set. Tests: example-parse asserts
the new fields; testcontainers push→dump round-trip (invoice + windows + named test/live accounts + secrets-as-refs).
FOLLOW-UPS SPLIT OUT (Paul 2026-06-30): catalog push⇄dump 1:1 YAML↔Go → #647; Solana signer keypair-or-vault-signing
→ #648. NOTE: the runtime test/live FILTER-not-reject behavior (config.ValidateRailSet) was left to the runtime
config layer — the manifest already declares + seeds both environments; test_mode selection is config.yaml's job.
PROPOSED 2026-06-30 (Paul, refined): a merchant's ENTIRE configuration should be one
round-trippable YAML — `push-merchant-config <file>` applies it, a new `dump-merchant-config` exports the
merchant's current state back out in the SAME shape, and the Go structs map 1:1 to the YAML (one shape drives
both directions). The example must be realistic and must show a TEST environment account right next to a LIVE
one for the same provider (NMI, Stripe), so a deployment runs the test accounts under `test_mode=true` and the
live accounts under `test_mode=false` from one declaration.

## Metadata
- Category: devex
- Status: done
- Passes: true

## Problem

1. **Incomplete push.** `ManifestMerchant` (`config/merchants.example.yaml`, applied by `push-merchant-config`
   → `ReconcileMerchantManifestData`) carries `slug`, `display_name`, `issuer`
   (uri / jwks_uri / public_keys / allowed_origins / slug), `profile`, and `provider_accounts`
   (provider_type / environment / account_id / mode / secrets / vault_secret_ref). It does NOT apply the
   `merchant_configurations` JSONB payload (`models.MerchantConfiguration`), whose other fields are set ONLY
   via the admission API (`Service.SetMerchantConfiguration`): **invoice/collection**
   (`collection_threshold`, `monthly_floor`, `billing_period_boundary`) and **delegated-invoker wasted-spend
   windows** (`[]BudgetWindowPolicy{key, window_seconds, limit, currency}`). So #643's merchant collection
   policy + the abuse cutoffs are invisible in the file and unreachable from config-as-code.
2. **No dump direction.** There is NO `dump-merchant-config` (the `merchant_exports` table is GDPR data export
   #225, not config). You can push a YAML but cannot serialize a live merchant's config back to YAML, so the
   file and reality drift and there is no "export to review/version/clone a merchant".
3. **Example isn't realistic and shows one environment.** Today's example is plausible but doesn't model the
   real test+live split. The runtime is all-test OR all-live (`test_mode` is the switch; `config.ValidateRailSet`
   rejects an account whose `environment` contradicts it; the manifest already tolerates declaring test
   accounts — only `mode=primary + environment=test` is barred outside dev, `merchant_manifest.go:497`). The
   example should declare BOTH a test and a live account per provider so one file serves both deployments.

## Target design

ONE Go struct family (`ManifestMerchant` + new blocks) ⇄ ONE YAML shape, used by BOTH `push-merchant-config`
(apply) and a new `dump-merchant-config` (export). The struct is the single source of truth for the shape, so
push and dump are symmetric and a dump→push round-trip is idempotent.

- **Complete the payload.** Add to `ManifestMerchant` (and wire `ReconcileMerchantManifestData` →
  `SetMerchantConfiguration`):
  - `invoice:` → `{collection_threshold, monthly_floor, billing_period_boundary}` (boundary ∈
    calendar_month | anniversary | fixed_interval). Defaults: threshold 50_000_000 ($50), monthly_floor
    1_000_000 ($1), boundary fixed_interval (`money.DefaultInvoice*` / `InvoiceBoundary*`).
  - `delegated_invoker_wasted_spend_windows:` → `[{key, window, limit, currency}]`. Default =
    `DefaultInvokerWastedWindows` (burst 15m/$5, sustained 5h/$20).
  - Omit = leave-as-is (never clobber an API-set value to default); mirror `SetMerchantConfiguration`'s nil rule.
- **Dump command.** `dump-merchant-config --slug <m>` reads the merchant row + issuer/remote-app + profile +
  provider_accounts + `merchant_configurations` and serializes to the manifest YAML. SECRETS are emitted as
  REFERENCES/placeholders (`env:`/`file:`/`vault:` names, never decrypted values) — the dump is structurally
  complete, not a secret leak. A dumped file re-applied with `push-merchant-config` reproduces the same state.
- **Realistic test+live example.** For NMI and Stripe, declare a `environment: live` account AND an
  `environment: test` account side by side (test ones as secondary/legacy or dev-primary, honoring the
  primary+test-outside-dev guard). Document that `test_mode` selects the active environment at boot. CONFIRM
  the activation path: the runtime must FILTER provider accounts by `ExpectedProviderEnvironment(test_mode)`
  and activate only the matching set — if `ValidateRailSet` still hard-rejects the non-matching environment
  when both are declared, change it to filter-not-reject (and note that selection decision here).
- **Name each provider account (Paul 2026-06-30).** A manifest `provider_accounts[]` entry has no human label
  today (`provider_type` + `account_id` + secrets only), yet the runtime config keys accounts by a NAME
  ("mobius", "paykings") — visible only as a prefix buried in the secret env-var names. Add an explicit label
  field so a `nmi` account reads `{provider_type: nmi, name: mobius, account_id: "100001", …}`. The label is
  the readable account NAME (config map key), NOT a second routing id — `account_id` stays the routing identity
  (#641). (Paul suggested `provider: mobius`; `name`/`alias` reads clearer next to `provider_type` — pick the
  key, keep the meaning.) The label also makes dump output legible.

## Tasks

- [ ] Make the manifest shape 1:1 with the YAML (one struct family); add `invoice` + delegated-invoker-windows
      blocks (parse + validate; reuse `money.NormalizeInvoiceBoundary` + the admission range checks).
- [ ] Add a human `name`/`alias` label to manifest `provider_accounts[]` (the config-layer account name), keep
      `account_id` as the routing id; thread it through reconcile + dump.
- [ ] Wire `ReconcileMerchantManifestData` → `SetMerchantConfiguration` for the new blocks (omit = leave-as-is;
      honor insert/overwrite reconcile tiers).
- [ ] Add `dump-merchant-config` (CLI + service read path) → serialize full merchant state to the manifest YAML
      shape; secrets as refs/placeholders, never values. Round-trip test: dump → push → dump is stable.
- [ ] Rewrite `config/merchants.example.yaml`: realistic, a self-describing header enumerating every
      merchant-config domain + pointers to sibling manifests, EVERY merchant-level field shown with its default,
      and a TEST+LIVE account pair for NMI and Stripe demonstrating `test_mode` selection.
- [ ] Decide + implement the test/live activation: filter provider accounts by `ExpectedProviderEnvironment`
      (not reject) so one manifest with both environments boots under either `test_mode`.
- [ ] Update `ParseMerchantConfigManifest` validation + `TestExampleMerchantConfigManifestParses` /
      `TestParseMerchantConfigManifest`; add a dump↔push round-trip integration test.

## Out of scope (separate manifests / runtime — cross-reference, do NOT fold in)

- **Catalog** (products / prices / meters / rate cards) → `config/catalog.example.yaml` (its own apply
  pipeline; "what you sell", not "how the merchant is configured").
- **Auth bootstrap** (permission groups, host-app issuer-as-owner) → `config/bootstrap.example.yaml`.
- **Process/runtime config** (DB, rails wiring, server, the `test_mode` flag itself) → top-level `config.example.yaml`.
- **Per-CUSTOMER settings** — `money_settings` (billing_mode, spend limits, auto-topup, credit expiry) and
  `customer_minimum_spend` (#643) are per-customer, set at runtime, never in a merchant-level file.
- **Tier/role/budget policy templates** (`tier_schedules`, invoker/payer spend limits) — a separate
  merchant-policy domain; future candidate, not this issue.

Acceptance: one struct family ⇄ one YAML shape; `push-merchant-config` applies the COMPLETE merchant config
(identity/issuer/profile/provider_accounts + invoice + delegated-invoker windows); `dump-merchant-config`
exports it back (secrets as refs) such that dump→push→dump is idempotent; `merchants.example.yaml` is realistic,
self-describing, shows every field with defaults, and declares a test+live account pair per provider with
`test_mode` selecting the active environment.

---

# #644: product-kind contracts — compose typed capabilities, validate illegal mixes (no product.type)

**Completed:** yes
**Status:** DONE 2026-06-30. Enforced capability compatibility on the current catalog model: variable credit top-ups and usage/rate-card products cannot live in tier groups, membership tier groups require recurring prices, fixed one-time/ownership products remain untiered, and no `product.type` was added. `config/catalog.example.yaml` no longer puts the image-credit top-up or prepaid API balances in fake tier groups.

## Metadata
- Category: billing
- Status: done
- Passes: true

## Problem

One broad `Product` struct with all-optional capability fields (`tier_group`/`tier_rank`, `prices`, `rate_cards`, `credit_purchase`, `credits`, `entitlements`) lets invalid combinations look valid. Live symptom: `config/catalog.example.yaml` puts `image-credit-topup` under `tier_group: ai-credit-topups`, but a variable credit top-up is a repeatable checkout, not a mutually-exclusive membership tier.

## Design decision: TYPE prices, COMPOSE products

The catalog has two axes, and they get OPPOSITE treatment — this is the crux:

- **Prices are disjoint variants.** A price is exactly one of flat / per_unit / tiered; `unit_amount` + `package_size` + `tiers` together is always a bug. → keep an explicit `price.model` discriminator, and (in #645) give each model its OWN typed sub-block so mixing is impossible by construction. Stripe/Lago/OpenMeter all type their price/charge models (OpenMeter's `price.go` is a discriminated union).
- **Products are compositions, NOT variants.** A product is a BUNDLE of orthogonal capabilities (pricing × grants × membership-semantics), not a single kind. Legitimate real combinations: a membership tier that ALSO grants credits (cozy), and a plan with a flat base price AND metered `rate_cards` overage (the standard OpenMeter/Lago plan — the example's `spaces` product already does flat + usage rate cards). A single `product.type` (or product sum type) would FORBID those. → **NO `product.type`.** Stripe/Lago/OpenMeter don't type products either; behavior lives in the (typed) prices/features.

So the safety the catalog needs is not a product type — it is **explicit capability-compatibility rules**: testable, self-documenting, and unable to disagree with the actual fields (a derived "kind" can drift from them). Prefer direct rules over "infer a kind, then reject."

## Capability-compatibility rules

- **credit top-up** (variable credit delivery) ⇒ NO `tier_group`/`tier_rank`, NO subscription/recurring prices, NO `rate_cards`. Standalone repeatable checkout.
- **usage / `rate_cards`** ⇒ NO `tier_group`/`tier_rank` (a metered resource is not a tier). MAY carry a flat base-fee price — do NOT reject `flat base + metered overage`; that is a valid plan.
- **membership** (`tier_group`/`tier_rank`) ⇒ has recurring prices; mutually exclusive within its group.
- **credits / entitlements** ⇒ orthogonal grants; allowed on memberships AND one-time/ownership products.

## Tasks (non-breaking; current schema)

- [x] Remove `tier_group: ai-credit-topups` from `image-credit-topup` in `config/catalog.example.yaml`; audit for other leaks (esp. usage/rental products placed inside tier groups).
- [x] Add loader validation for the capability-compatibility rules above, with credit top-up + `tier_group` as an explicit rejection.
- [x] Explicitly ALLOW `flat price + rate_cards` (flat base + metered overage); add a test so future validation can't regress it.
- [x] Do NOT add a `product.type` field or a product sum type. Rules are derived from actual fields.
- [x] Keep `price.model` explicit; it is the price-FORMULA discriminator, not the product kind.
- [x] Loader tests: good products and rejected illegal mixes covered in `pkg/catalog` plus the example parse gate.

The price-model TYPING (sub-blocks), 1:1 Go↔YAML structs, and the credit-model reshape are #645 (breaking).

---

# #645: typed price-model sub-blocks + 1:1 Go↔YAML catalog structs + credit-model reshape (BREAKING — coordinated bump)

**Completed:** yes
**Status:** DONE 2026-06-30. Breaking manifest + JSONB schema change shipped in OpenRails `v0.78.0`: typed `RatePrice` sub-blocks, `credit_balances`, credit grants as arrays, top-ups as variable credit grants plus price offers, `catalog_credit_purchase_prices`, and runtime quote/deposit selection from the new offer table. Consumer bumps completed for doujins, hentai0, and cozy-art.

## Metadata
- Category: billing
- Status: done
- Passes: true

## Goal

Make every catalog AUTHORING struct 1:1 with its YAML so (un)marshaling is a natural fit with no parallel/adapter Go shape, and type the price models so cross-model field leakage is impossible by construction (the #644 decision).

## 1. Typed price models — discriminator + typed sub-block

Today `pkg/pricing.RatePrice` is one wide struct: `Model` plus every model's fields optional (`unit_amount`, `divide_by`, `round`, `mode`, `tiers`, `package_size`, `multiplier`, `maximum_amount`, `matrix`). Nothing structurally stops `unit_amount` + `tiers` on one price; only validation catches it.

Adopt the SAME pattern #630 used for `ProviderAccountConfig` (a discriminator + exactly-one typed sub-block):

```yaml
price:
  model: per_unit
  per_unit: { unit_amount: 8_930, divide_by: 3_600, round: up, maximum_amount: 6_000_000, matrix: { ... } }
# or
  model: tiered
  tiered: { mode: graduated, tiers: [ { up_to: 2_000, unit_amount: 10_000 }, { up_to: null, unit_amount: 8_000 } ] }
# or
  model: flat
  flat: { amount: 5_000_000 }
```

```go
type RatePrice struct {
    Model   string        `json:"model" yaml:"model"`
    Flat    *FlatPrice    `json:"flat,omitempty" yaml:"flat,omitempty"`
    PerUnit *PerUnitPrice `json:"per_unit,omitempty" yaml:"per_unit,omitempty"`
    Tiered  *TieredPrice  `json:"tiered,omitempty" yaml:"tiered,omitempty"`
}
```

- Each variant struct holds ONLY its fields → no cross-model leakage.
- Standard JSON/YAML marshaling (nested pointers; NO custom Unmarshaler, unlike an interface union), and it round-trips cleanly through the `catalog_rate_cards.price` JSONB column.
- Validation collapses to "exactly one sub-block, and it matches `Model`."
- Keep `ChargeModel` as the flat NORMALIZED EVALUATION struct; `RatePrice.ToChargeModel()` flattens at rate-time. The 1:1 rule is about AUTHORING/wire structs; the eval form is a legitimately separate normalized type behind the apply boundary.

## 2. 1:1 Go↔YAML for all authoring structs

- products, prices, credits, `credit_balances`, rate cards, meters each map directly to their YAML with no second Go shape that must be hand-adapted. (money's old duplicated pricing engine is already gone after the #638 `pkg/pricing` extraction; audit for any remaining adapter-only shapes.)
- Precise rule: each YAML shape has a matching Go shape with no hidden adapter; normalized RUNTIME structs (e.g. `ChargeModel`, service specs that carry pre-marshaled JSON) are allowed ONLY behind the validation/apply boundary.

## 3. Credit-model reshape (the breaking part of original #644)

Current shape (to retire): `Product.Credits` is `map[string]CreditGrant` (repeats unit/currency, unordered); there is no top-level `credit_balances`; `CreditPurchase` is a per-product wrapper with `credit_type`/`unit`/`currency`/`providers`/`input_min`/`input_max`/`round`/singular `price`; migration `046` stores one `catalog_credit_purchases` row per product (PK `product_id`, so multiple offers are impossible); `QuoteCatalogCreditPurchase` reads that single row.

Target:
- `credit_balances: [{key, unit, expires_default}]` — canonical, declared once per catalog; memberships and top-ups reference it by `key` (no repeated `unit`).
- product `credits: [{key, amount, cadence}]` — ARRAY (ordered, multiple grants), not a map; `Amount *int64` so omitted (variable top-up delivery) is distinct from zero. Reject duplicate `credits[].key`; every grant must reference a declared balance; fixed/recurring grants require `amount`.
- Retire the `credit_purchase` wrapper. A top-up = a product with `credits: [{key}]` (no fixed amount) + one or more `prices` offers, each carrying `currency`, `providers`, `input_min`/`input_max`, `round`, and a typed price sub-block (§1). Multiple offers (USD/EUR, Stripe-only/Solana-only, promo). Credit-top-up offers restricted to `tiered{mode: graduated}` or `per_unit` (invertible — #640).

## 4. Price-model set — keep flat/per_unit/tiered; package vs dynamic

**Correction to original #644's rationale:** "`package` is redundant with `per_unit` + `divide_by` + `round: up`" is FALSE. Verified in `pkg/pricing/chargemodel.go`: `per_unit` rounds MONEY after multiply (`round(qty × unit ÷ divisor)`), while `package` rounds UNITS up to whole blocks (`ceil(qty ÷ size) × amount`). E.g. 150 units, 100-block @ 5_000: package = 10_000, per_unit{round:up} = 7_500 — different. And you cannot make `per_unit` round units instead: that breaks DO per-second pro-rating (the reason #638 made `round` = money-rounding). So:
- **KEEP `package`** — it is the only block/round-up-to-whole-units model; standard in Stripe/Lago/OpenMeter. (Recommended; fix the "redundant" wording wherever it appears.)
- **`dynamic`** (cost-plus markup) is the fair YAGNI cut — remove until a real passthrough/reseller product needs it. Easy to re-add (standard in Lago/OpenMeter).
- Keep `tiered` `graduated` + `volume` in the engine.
- (If strict minimalism is preferred, both could go — but on YAGNI grounds, NOT "redundant," and noting block pricing is dropped.)

## 5. Persistence + runtime + coordinated bump

- DB: add `catalog_credit_balances`; replace the one-row-per-product `catalog_credit_purchases` (PK `product_id`) with a top-up-product + N-prices shape, e.g. `catalog_credit_purchase_prices(product_id, ordinal, currency, providers, input_min, input_max, round, price jsonb)`. Price JSONB stores the typed sub-block shape (§1).
- Applier/service (`pkg/catalog/applier_service.go`, `pkg/service/catalog_sidecars.go`): map final YAML → specs directly; runtime-normalized structs allowed behind the boundary.
- `QuoteCatalogCreditPurchase`: select an offer by (currency, provider) [reject ambiguous same-(currency,provider) duplicates for now; add an offer key later].
- **Coordinated release:** completed. OpenRails tagged `v0.78.0`; doujins and hentai0 bumped from `v0.77.0`; cozy-art bumped from `v0.72.1` and adjusted for current embedded/config/catalog service APIs.
- **Validation:** OpenRails focused unit + integration suite passed before tag (`pkg/pricing`, `pkg/catalog`, `pkg/embedded`, `pkg/service`, `internal/modules/money`, catalog sidecar/rating/credit-purchase integration tests, and `internal/integrationharness TestCatalogPublishRateCardsHTTP`). Re-verified after completion from clean committed HEAD with DB-backed integration tests including `TestExampleCatalogPublishesOverHTTP`. Consumer validation passed: doujins `go test ./internal/billing/openrailsembed ./config`; hentai0 `go test ./internal/openrails ./internal/app`; cozy-art `go test ./cmd ./internal/app ./internal/api ./internal/billing`.

## Tasks

- [x] Reshape `pricing.RatePrice` to discriminator + typed sub-blocks (flat/per_unit/tiered/package); validation = exactly-one-block-matching-`model`; keep `ChargeModel` as the flat eval form; update catalog aliases + the example.
- [x] Audit + collapse remaining catalog authoring shape drift in OpenRails; runtime specs remain normalized behind the apply boundary.
- [x] `Manifest.CreditBalances []CreditBalance`; product `Credits []CreditGrant` (array, required `Key`, `Amount *int64`); retire `Product.CreditPurchase`.
- [x] Top-up = `credits:[{key}]` + `prices:[offers]`; multiple offers; graduated/per_unit only; reject ambiguous same-(currency,provider) offers.
- [x] KEEP `package`; REMOVE `dynamic` (constants/engine/validation/tests/docs).
- [x] DB: `catalog_credit_balances` + multi-price top-up storage; price JSONB = typed sub-block.
- [x] Applier/service mapping + `QuoteCatalogCreditPurchase` offer selection (currency/provider).
- [x] Migrate `config/catalog.example.yaml`; loader + DB-integration tests for the new shapes (persist/quote/deposit into one canonical balance; illegal mixes rejected).
- [ ] Coordinated release: tag openrails + bump doujins/hentai0/cozy (per the #630 playbook).

Validation:
- `go test ./pkg/pricing ./pkg/catalog ./pkg/embedded ./pkg/service ./internal/modules/money`
- `go test -tags=integration ./pkg/service ./internal/modules/money -run 'TestSyncCatalogSidecars_PersistsRateCardsAndCreditPurchases|TestFinalizeInvoice_RatesCatalogRateCardsWithMatrixCapAndAllowance|TestCatalogCreditPurchase_QuotesBonusCreditsAndDepositsLedgerBalance|TestCatalogCreditPurchase_QuotesByCreditsEntryWithinSpendBounds' -count=1`
- `go test -tags=integration ./internal/integrationharness -run 'TestCatalogPublishRateCardsHTTP' -count=1`

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

**Completed:** yes
**Status:** DONE 2026-06-30 (commit 9beec4d5). MAJOR DISCOVERY during implementation: this was NOT
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
DROPPED as over-engineering (Paul 2026-06-30 — not lingering, deliberately out of scope): (a) per-CUSTOMER
override of the merchant collection cadence/threshold — an enterprise feature with no current use case; the
merchant-level config covers every product we run. (b) an admin/HTTP surface for `SetCustomerMinimumSpend` —
the service method exists + is tested; wire an endpoint when a real caller appears, not before. (c) a global
invoice-rounding policy — there is no sub-cent-suppression requirement. None block this issue; revive only on
a concrete need. Merchant collection policy is NOT in `merchants.example.yaml` (that manifest carries identity/
profile/provider_accounts only) — it's runtime config in `merchant_configurations` via the admission API, which
is the right home; `minimum_spend` is per-customer so it never belongs in a merchant file.
PROPOSED 2026-06-29: the billing-relationship half of the #642 metering review (split out).

## Metadata
- Category: feature
- Status: done
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
      (threshold always + monthly when scheduled = hybrid).
- [x] Account-level `minimum_spend` (commit $X/period, period-end true-up at close) — BUILT: migration 047
      `customer_minimum_spend` + `SetCustomerMinimumSpend` + `FinalizeInvoice` `WithMinimumSpendTrueUp`.
- [x] Invoice-close driver — ALREADY EXISTED (`FinalizeThresholdInvoices` + `FinalizeDueInvoicesForBoundary`
      + `InvoiceWorker`); ONE invoice per customer per currency across all rate cards is how `FinalizeInvoice`
      already rolls up. Minimum-spend true-up now hooks the periodic close.
- DROPPED (over-engineering, no current need): per-customer collection-cadence override; admin/HTTP setter for
      `minimum_spend`; global invoice-rounding policy. See Status.

Acceptance (delivered): collection cadence is a merchant policy (periodic/threshold/hybrid via merchant config
+ the InvoiceWorker); threshold billing works (impossible under a per-product `billing_cadence`); `minimum_spend`
trues-up at the periodic close; one invoice per customer account covers all their rate cards. Per-account override
descoped as over-engineering. Depends on #642 (pricing-only rate cards + calendar-month
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
- Environment (test|live) disambiguation on the in-process config too — added `Environment` to `config.ProviderAccountConfig` (mirrors the manifest's per-account `environment`) + `EffectiveEnvironment` + `ExpectedProviderEnvironment`. `ValidateRailSet` now enforces the all-test-or-all-live rule: an account whose declared environment contradicts `test_mode` is rejected (empty → derived from test_mode, back-compat). Tested (`config` TestRailEnvironmentMustMatchTestMode).
- Stripe managed-webhook per-account URL auto-registration (Paul 2026-06-30) — `PublicStripeWebhookURL` appends the account_id (per-account endpoint) when set; `ReconcileManagedStripeWebhook` passes it; the River reconcile job (`jobs_stripe_webhooks.go`) now iterates EVERY enabled Stripe account (primary/secondary/legacy), not just the primary, registering one managed endpoint each (matched by `openrails_managed` metadata, so the primary's existing endpoint is updated in place, not duplicated). Config-rail/embedded path (no account_id) keeps the shared URL. Tested (`internal/modules/catalog` TestPublicStripeWebhookURL + TestReconcileManagedStripeWebhook* + the example manifest's primary+legacy Stripe via `internal/bootstrap`).

DELIBERATELY DROPPED as over-engineering (Paul 2026-06-30 — "delete any tasks you feel are over-engineering"). The per-account PATH + write-time secondary sync already deliver the capability; these add cost without a concrete need:
- Payload-disambiguation single-shared-endpoint mode (CCBill `clientAccnum`, Stripe Connect `event.account`) — the per-account PATH covers multi-account routing; this is a convenience alternative with no current consumer. YAGNI.
- CCBill account-scoped path — CCBill auth is IP-allowlist + account-number match (no per-account HMAC secret), and the postback already carries the account number, so there is nothing account-scoped to add.
- Persist per-account catalog links for secondaries — a broad links-BY-ACCOUNT model change whose only added value is drift DETECTION on failover accounts; the idempotent write-time sync already keeps secondaries current.
- Per-(merchant,rail,environment) primary-UNIQUENESS specifically — already enforced by the manifest/DB upsert (one primary per merchant,type,environment) + `validateRails` (≤1 primary per rail). The environment FIELD + test_mode-consistency guard it implied is now DONE on the in-process config (see LANDED above), so this is no longer "non-applicable" — the uniqueness sub-task itself was just already covered.
- ~~Strictly-require account_id in-process~~ DONE (Paul 2026-06-30 disliked the fallback): removed the name-fallback — `EffectiveAccountID()` returns the real `account_id` or "". `ValidateRailSet` requires `account_id` when a rail has >1 account (the made-up map name can never be the routing key); a single account still works via the rail-primary path, and `createNMIClients` always requires it for NMI. Tested (`config` TestPrimaryRailByType + TestCatalogTargetSelectors). BREAKING for embedded hosts with multiple accounts on a rail or any NMI account (e.g. doujins must declare its NMI gateway-id) — single-Stripe hosts (cozy-art) are unaffected.

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
- [ ] Define routing inputs: tenant_id, product_id, price_id, tier_group, amount, currency, billing cycle, user country/state when known, non-archived provider-account eligibility, processor availability, processor capability metadata (#291), and explicit client preference.
- [ ] Define routing outputs: selected processor, fallback candidates, reason, and policy version.
- [ ] Decide precedence: explicit price/provider config > merchant policy > product/tier_group policy > global default.
- [ ] Decide failure classes that can trigger fallback before checkout finalization: processor unavailable, unsupported capability, credential missing, sandbox/live mismatch, hard validation failure. Do not fallback after a successful charge.
-
- DATA MODEL / CONFIG:
- [ ] Add routing policy representation in DB or catalog manifest: allowed processors, preferred order, archived-account exclusion, and optional per-tier overrides.
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

---

# #659: embedded HTTP surface should be canonical, method-limited, and provider-aware

**Completed:** no
**Status:** PLANNED 2026-06-30 — route-surface cleanup from the Doujins/Hentai0 embedded mount review. OpenRails
must not expose provider-specific endpoints for rails the merchant has not configured, and embedded hosts should
mount OpenRails at its canonical billing namespace instead of hiding it behind a generic app API path.

## Metadata

- Category: http-api
- Status: planned
- Passes: false

## Problem

The embedded OpenRails surface is too broad and too confusing:

- Doujins/Hentai0 mount OpenRails through catch-all Gin `Any`, which registers `CONNECT` and `TRACE` even though
  billing only needs normal API methods.
- OpenRails internally logs `/billing/v1/...`, while embedded hosts commonly expose `/api/openrails/...`; that
  makes startup route logs look wrong and blurs ownership with the host app API.
- Self-service registers provider-specific routes unconditionally, including Stripe portal and Solana wallet
  signing routes. A merchant with no Stripe account should not expose `/stripe/portal`; a merchant with no Solana
  rail should not expose Solana subscription mutation routes.
- AuthKit, OpenRails, and host app routes should have distinct default namespaces:
  - OpenRails billing: `/billing/v1/...`
  - AuthKit auth: `/auth/v1/...`
  - host app API: `/api/v1/...`

This is not about returning nicer "unsupported provider" errors. If a provider is not configured, the route should
not be mounted for that runtime surface.

## Target design

- Keep OpenRails' canonical embedded prefix as `/billing/v1`.
- Make OpenRails examples and embedded helpers steer hosts to mount at `/billing`, not `/api/openrails`.
- Replace Gin `Any` helper mounts with an explicit method allowlist: `GET`, `POST`, `PUT`, `PATCH`, `DELETE`,
  `OPTIONS`, `HEAD`. Do not register `CONNECT` or `TRACE`.
- Derive a provider route-mount plan from configured provider accounts and capabilities:
  - Stripe portal route is mounted only when the active runtime has a configured Stripe account with portal support.
  - Solana wallet-signing routes are mounted only when Solana is configured.
  - Webhook route groups are mounted only when at least one configured provider supports webhooks; individual
    provider webhook dispatch still validates merchant/provider identity.
- Prefer generic public route names where the API is provider-independent:
  - replace `/me/stripe/portal` and `/customers/:customer_id/stripe/portal` with generic billing-portal routes
    if the product still needs a portal handoff.
  - keep provider-specific wallet-signing flows under a provider namespace only when the provider protocol is
    inherently provider-specific.

## Tasks

- [ ] Audit current public and embedded route registration for provider-specific endpoints:
      Stripe portal, Solana wallet-signing subscription routes, provider webhook routes, captcha/user checkout,
      customer self-service, merchant admin, catalog, payment-provider admin, and merchant service API.
- [ ] Add a minimal route-mount planner that consumes the selected `RouteSet`s plus configured provider rails /
      capability metadata. Do not add a second router abstraction; build on the existing `RouteSet` and
      `CapabilitiesHandler` machinery.
- [ ] Split self-service registration so provider-specific subroutes are registered conditionally instead of being
      hardcoded inside the always-on customer route group.
- [ ] Hard-cut provider-specific public paths that should be generic. At minimum, plan the Stripe portal handoff as
      a generic billing-portal endpoint, mounted only when a configured provider supports it.
- [ ] Update `pkg/embedded/gin.RegisterAPI` to register only the explicit billing HTTP method allowlist, not
      `group.Any`. Provide the same guidance/helper for hosts that use the lower-level `http.Handler` escape hatch.
- [ ] Update examples/docs so embedded hosts use `/billing` for OpenRails, AuthKit examples use `/auth`, and app
      APIs keep `/api/v1`.
- [ ] Add regression tests proving:
      - no `CONNECT`/`TRACE` routes are registered by the OpenRails embedded Gin helper;
      - a runtime with no Stripe does not mount Stripe portal/billing-portal routes;
      - a runtime with no Solana does not mount Solana wallet-signing routes;
      - configured Stripe/Solana runtimes still mount their intended routes;
      - route capabilities report the actual mounted groups/routes, not every route OpenRails knows how to serve.
- [ ] Add/adjust Doujins and Hentai0 follow-up issues after OpenRails lands: mount OpenRails at `/billing`, AuthKit
      at `/auth`, host app routes at `/api/v1`, and stop direct `Any` mounting of OpenRails handlers.

## Out of scope

- A full processor-routing engine. Provider selection/fallback stays in the provider-account routing/capability
  work; this issue only controls whether HTTP routes exist.
- Backward-compatible aliases for `/api/openrails` or `/stripe/portal`. This is a hard-cut route cleanup.
