# Billing Consistency: Invariants, Taxonomy & the Convergence Engine

Status: design. The single source of truth for **what must always be true** across
OpenRails' local state and the external payment processors, **how each violation
is named**, and **how it is repaired** — including the rules that keep a bulk
import of messy legacy data from doing damage.

This taxonomy is designed from first principles. It is **not** anchored to the
older `PS-*` / `P-E-*` / `S-E-*` codes, which conflated unrelated things and had
no principled axis.

---

## 0. The two parts: the pull and the Convergence Engine

1. **The pull (`reconcile`).** Pull the *complete* current state from each provider
   and **overwrite** the local mirror. The provider is the source of truth for
   provider-owned facts (charges, refunds, remote liveness, vault). The pull does
   not reason about entitlements or credits — it just makes the local ledger a
   faithful mirror. A pull is authoritative only for the provider account it
   actually queried: `(merchant, provider_type, provider_account_id)`, where
   `provider_account_id` is a local FK to `provider_accounts.id`.
2. **The Convergence Engine.** Given a truthful ledger, drive every *grant effect*
   (entitlements, credits, product access) and every standing *external decision*
   to a consistent state with the source events — repairing internal and external
   divergence. It runs continuously while the server is up (inline + sweep) and as
   a one-shot pass after a pull.

```
reconcile check  → pull, log every row's old→new diff, ZERO writes, no convergence (dry run)
reconcile pull   → pull, overwrite mirror, log changes, THEN run the Convergence Engine
reconcile report → applied changes + standing findings + admin queue
```

---

## 1. The truth model: four diagnostic planes plus provider actions

Every invariant has one shape: **`A` should equal `B`, but doesn't.** You can't
repair it until you know *which side is right* — so each invariant is classified by
the authoritative `B` it is checked against, and that authority dictates what
repair action is allowed. The taxonomy is organized by diagnostic planes; pushing
changes to a provider is a remediation action, not a separate error class.

The four diagnostic planes split into one provider-observation plane and three
internal planes.

- **PULL — Provider-Pull** *(inbound)*: the provider is authoritative; pull its
  observed truth into our local row. This is the entire job of `reconcile pull`.
  The provider account must match the merchant/provider binding before a pull is
  allowed to insert or overwrite anything.

**Three planes are internal** — a local row checked against a local yardstick, no
processor involved:

- **DERIVE — Derivation**: a grant effect (entitlement / credit / access) vs. the source
  event (payment / subscription / admin action) that should produce it, via the
  grants layer (§2). Authority = the source ledger.
- **LIFE — Lifecycle**: a stateful record vs. where the **clock + its state machine**
  say it should be by now — the "overdue / hasn't advanced" plane (dunning unrun,
  pending stale, grace exhausted). Authority = time.
- **CON — Consistency**: local/provider-observed facts vs. the three residual
  integrity shapes that cannot always be expressed as DB constraints:
  duplicate facts (individually valid, invalid in combination), amount
  mismatches (A + B = C), and unresolved/nonsensical references. Authority = the
  consistency condition. (Internal amount checks, as opposed to Provider-Pull's
  provider-observed refresh.)

| Plane | `A` — the fact | `B` — authority | Repaired by | Channel |
|---|---|---|---|---|
| **PULL** Provider-Pull | local row | the provider | overwrite local (the pull) | local write |
| **DERIVE** Derivation | a grant effect | the source ledger (via grants) | replay / retract the grant effect | local write |
| **LIFE** Lifecycle | a record's state | the clock + state machine | converge the record forward | local write / provider action |
| **CON** Consistency | local/provider-observed facts | an internal consistency condition | fix the data / surface review | local write / operator |

The planes are also the engine's modules: **PULL is the pull; DERIVE, LIFE, CON are
the Convergence Engine's passes, run in that order** — sources must be truthful
before grant effects are derived, and lifecycle state must be current before final
consistency checks run.

**Provider actions** are the outbound solution channel. Any diagnostic plane may
enqueue a `provider_intent` or operator task when the repair must happen at Stripe,
NMI/Mobius, CCBill, Solana, or another provider. Examples: cancelling an extra
remote subscription, retrying a scheduled cancellation, or pushing a catalog plan
definition. Those actions are linked remediation for `life.*` or `consistency.*` findings,
not `PUSH-*` findings.

### The shape axis

Each invariant also has a **shape** that maps 1:1 to a repair verb:

| Shape | Meaning | Repair verb |
|---|---|---|
| `MISSING` | Under-representation — a justified thing doesn't exist | **MATERIALIZE** |
| `EXCESS` | Over-representation — a thing exists with no justification | **RETRACT** |
| `MISMATCH` | Exists, wrong attribute/value | **ADJUST** |

**These three are exhaustive** — a complete 2×2 over (should-exist × does-exist):

| `B` \ `A` | `A` present | `A` absent |
|---|---|---|
| **should exist** | match, or **MISMATCH** | **MISSING** |
| **should not exist** | **EXCESS** | (consistent) |

There is no fourth cell, so under a sound truth model every disagreement is one of
these three. There is deliberately **no "conflict" shape**: a case the engine
*cannot evaluate* — because the authority is unreachable or the evidence is
ambiguous — is not a shape, it is the **INDETERMINATE** finding state (§8). That is
always an *evidence* problem (provider down, two identity matches, an in-flight
intent of unknown outcome), never a truth-model problem. Two valid authorities
genuinely fighting would mean the model assigned two masters to one fact — a design
bug that must never ship, not a runtime category.

### Finding type scheme

Use one self-describing qualified name as the error identity. Store that name
directly in `reconciliation_findings.finding_type`; do not maintain a second
opaque numbered code.

Format: `<plane>.<subject>.<shape-or-condition>`, e.g.
`pull.charge.missing`, `derive.grant.excess`,
`life.subscription.dunning_overdue`,
`consistency.amount_mismatch.provider_catalog`.
### Remediation class (orthogonal to plane & shape)

| Class | When | Channel |
|---|---|---|
| **AUTO** | Idempotent, reversible local write, unambiguous | Applied immediately |
| **ADMIN** | Known but consequential | Queued; on approval → local write or `provider_intent` |
| **OPERATOR** | No API to us, or too sensitive to ever auto-fire (refund issuance, dispute response) | Surfaced + runbook; human acts in dashboard |

---

## 2. The grants layer: source → grant → grant effect

Access is an effect of recorded source events. To make convergence **one uniform
mechanic** instead of a dozen bespoke pairwise checks, a canonical **grant** sits
between the source events and the fine-grained grant effects:

```
SOURCE EVENTS   (immutable facts: payment captured · subscription period billed · admin action)
      │   derive-1: event → grant
      ▼
GRANTS  (append-only events: "source S grants customer C product P for [start,end), spec snapshot";
          revoke / expire / supersede are NEW events, never edits)
      │   derive-2: grant → grant effects   (mechanical fan-out from the snapshot)
      ▼
GRANT EFFECTS (entitlement-feature windows · credit blocks · product access) — what the app reads
```

`openrails.product_access_grants` is already ~80% this shape (`source_type` ∈
{purchase, subscription, admin}, `source_id`, `payment_id`, `starts_at`/`ends_at`).
The change is to **promote it (or a generalized `grants` table) to be the layer that
entitlements *and* credits *and* access all derive from**, make it **append-only**
(a grant is never updated — revoke / expire / supersede are new events), and tag
credit deposits with the grant (replacing the free-string `money_transactions.source`).

The grant log is **single-entry** — an *issuance*, not a double-entry movement:
access is not conserved, so there is no counter-account. Money *is* conserved, so the
one cross-over is that a **credit grant also emits a double-entry ledger transfer**
(below). And a **credit grant is the credit lot** (amount + expiry + source), so the
legacy `money_blocks` lot table is subsumed.

> **Credit substrate.** The *credit* grant effect is the one that touches money, so
> it composes with the parallel double-entry ledger reorg: a credit grant's
> grant effect is a **ledger transfer** (deposit into the customer-balance account)
> tagged with the grant, and clawback is a **reversing transfer**, not a mutation
> (lapse → `expired_credits`; admin-revoke/refund → `revoked_credits`; see §11 (decision 4)).
> Entitlement and product-access grant effects are plain local rows. See the
> double-entry ledger work for the substrate; this doc only requires that a credit
> deposit carry its `grant_id`.

Derivation is then two pure, idempotent steps:

- **derive-1 (event → grant):** a completed payment / billed subscription period /
  admin action produces its grant(s), dated to the event, snapshotting the product's
  `entitlements_spec` + `credits_spec`.
- **derive-2 (grant → grant effects):** each grant fans out to its entitlement-feature
  windows, credit blocks, and product-access row — mechanically, from the snapshot.

Payoff: the whole **DERIVE plane collapses to two tiers** — "every event has its grant"
and "every grant has exactly its grant effects" — *uniform across entitlements,
credits, and access*. The "manual override is consistent" rule is just a **revoke
event** in the log (the access is gone, the reason is recorded). Replay (§4) is a
pure function of the grant.

---

## 3. Two doctrines that make legacy import safe

A legacy importer drops thousands of pre-broken records at once: dunning that hasn't
run in months, entitlements whose subscription was never imported, refunds recorded
out of order. Two rules keep the Convergence Engine from doing damage.

### 3.1 Replay vs. converge — never replay a side effect

- **Grant effects are replayable.** Recreating an entitlement / credit / access window
  has no external side effect, so the engine freely **replays history** (§4): it
  writes the window that *should have existed*, dated to the source event.
- **External actions are NOT replayable.** The engine must never re-attempt *missed
  historical* charges, rebills, or dunning cycles. If dunning hasn't run in three
  months, it does **not** try to charge the card three times. It computes **where the
  record should be now** (three months unpaid past grace ⇒ terminal) and converges to
  that single current state, revoking grant effects as of when grace *would* have ended.

> Rule: LIFE-plane repairs **converge to the correct current state**; they never
> replay the side-effecting steps that were skipped.

### 3.2 The confirmed-absence gate — MATERIALIZE freely, RETRACT carefully

`MISSING`/MATERIALIZE is additive and safe → **AUTO even in bulk import.**

`EXCESS`/RETRACT is destructive (revokes access, cancels) → it requires a **confirmed
absence**: a *completed authoritative pull/import* proving the justifying source
truly does not exist. "Source not present in the local DB" is **not** confirmed
absence during import — it usually just means *not imported yet*.

> Poster case: *"a user has had an entitlement for months but no subscription."* The
> entitlement's grant has no live source -> `derive.grant.excess`.
> Pre-reconciliation it is **HELD** (the subscription may be un-imported). Only after
> the import + a full provider pull confirm no source exists does it become a real
> RETRACT — and even then mass retraction is **ADMIN**-gated, not silent.

The gate is **per source domain** (subscriptions / payments / admin grants): an
`EXCESS` repair does not fire as AUTO until that domain is marked **fully reconciled**
for the merchant. Until then it accumulates as a held finding for triage.

---

## 4. Repair is a historical replay (anchored to source time)

When the engine materializes a grant effect, it anchors the window to the **source
event's timestamps**, not wall-clock.

> A one-off membership bought **2025-06-30** granting **90 days** is reconstructed as
> `start=2025-06-30, end=2025-09-28` — even running in 2026, created
> **already-expired**. It grants no current access; it makes the ledger a faithful,
> complete history. Subscription windows are dated to the period they belong to.

Consistency is always judged against the `*_spec_snapshot` captured **on the
payment/subscription at purchase time** (now carried on the grant), never the live
`products` spec.

---

## 5. The invariant catalogue

This catalogue is the table/system pass. It targets the post-hard-cut model:
`money_transactions`, `money_blocks`, and `money_spend_limits` are retired by
migrations 004/005; credit lots are `grants`, balances are `ledger_transfers`,
and per-invoker spend caps are `invoker_spend_limits` / Redis admission state.

### Postgres-enforced invariants

These invariants are blocked by schema shape: unique indexes, CHECK constraints,
FKs, exclusion constraints, RLS/role privileges, or triggers. They should not
become normal Convergence Engine findings unless a legacy import or manual repair
path bypasses the schema.

Postgres-enforced invariants do **not** need runtime consistency findings.
Violations should fail at write/import time as constraint errors. If a legacy
repair path finds already-existing impossible rows, treat that as a schema/import
repair task, not as a durable finding taxonomy.

| Surface / tables | Invariant enforced by Postgres |
|---|---|
| Merchant isolation on merchant-owned tables | RLS requires rows to live inside the current `app.merchant_id` scope. Same-merchant composite FKs should be preferred whenever a child references another merchant-owned row. Current hardened example: `subscriptions(price_id, product_id, merchant_id)` -> `prices(id, product_id, merchant_id)`. |
| Identity maps: `customers`, `processor_customers`, `payment_methods`, `payment_blocklist` | Natural-key duplicates are blocked inside a merchant: one customer per AuthKit org or issuer/subject, one processor customer per customer/provider and provider customer id, one vault token per customer/provider, and one blocklist entry per `(kind,value)`. |
| Local catalog: `products`, `prices`, `entitlement_features`, `product_entitlement_features` | Product slugs are unique per merchant; price financial substance is unique per product; entitlement lookup keys are unique per merchant; product/feature joins are unique; local status/value enums are checked. |
| Subscriptions | Local product/price coherence is blocked by `subscriptions_price_product_merchant_fkey`; local provider subscription ids are unique; active/pending/past-due duplicates are blocked per `(merchant, customer, product)` and local active/pending tier group; cancelled/past-due/period fields obey CHECK constraints. |
| Payments and checkout sessions | Duplicate local provider transactions are blocked by `(merchant, processor, transaction_id)`; payments cannot be materially future-dated; checkout processor references/transactions are unique when present; payment/customer/subscription/price references are FK-backed. |
| Grant ledger: `grants` | Grant rows are append-only through role privileges; event/kind/source type are checked; credit grants carry positive amount+currency; windows are valid; FKs link customer/product/payment/supersedes; a root grant has at most one terminal event. |
| Grant effects: `entitlements` | Live entitlement windows cannot overlap; open-ended active duplicates are blocked; revoke fields move together; windows are valid; source type is constrained. |
| Double-entry ledger: `ledger_accounts`, `ledger_transfers` | Account natural keys are unique; transfers are append-only, positive, same-currency, between distinct accounts, and have valid phases; post/void resolvers require `pending_id`; each pending transfer has at most one resolver. |
| Usage/admission policy tables | Usage-event idempotency is unique by `(merchant, customer, currency, event_type, source, source_id)`; amount/window fields are nonnegative/valid; budget policy/window natural keys and enums are constrained. |
| Billing policy/configuration | Money settings are unique by payer/currency and constrain billing mode, alert percentage, owed amount, and credit limit; merchant secrets, DEKs, merchant configuration, and probe verdicts are single-row-per-scope by primary/unique key. |
| Reconciliation and operations ledgers | Reconciliation finding identity is unique by `(merchant, provider, finding_type, subject_key)`; resolved fields move together; catalog-drift open identity is unique; credential audit actions and export/status enums are checked; notification rows FK to customers. |

### Application-enforced invariants

These invariants cross time, source domains, derived state, provider state, or
polymorphic/non-FK references. They are the Convergence Engine/runtime surface.

| Surface / tables | Invariant application must enforce | Plane/check |
|---|---|---|
| Soft or polymorphic references | A declared `source_type/source_id`, ledger reference, or provider-intent reference must resolve to the intended row in the same merchant/scope. Prefer future DB constraints when the relationship can be expressed without blocking valid imports. | `consistency.reference.*` only for references that cannot be constrained |
| Provider mirror: `payments`, `subscriptions`, `payment_methods`, disputes/refunds | Provider-owned facts are reflected locally: successful charges, refunds, chargebacks/disputes, subscription status/period/next bill, and vault metadata. The pull upserts and overwrites provider-owned mirror fields only inside the same `(merchant, provider_type, provider_account_id)` binding, where `provider_account_id` points to `provider_accounts.id`. Active #511 intentionally ignores local-only provider mirror rows rather than emitting `pull.*.excess`; provider absence is too often ambiguous due to retention/reporting windows, incomplete attribution, partial pulls, or wrong provider/account lookup. The pull does not rewrite local intent or catalog definitions. | `pull.*` |
| Provider charges and local payments | Every successful provider charge has exactly one local payment row after pull. Same provider transaction duplicates are schema-blocked; distinct charge IDs for the same customer/product/period are only valid when explained by invoice/proration/operator intent. Local-only charge mirrors are not deleted until the payment domain is confirmed complete. | `pull.charge.*`, `consistency.duplicate.provider_charge` |
| Refunds / chargebacks | Every provider refund/chargeback is mirrored; every mirrored refund links to a real original payment; `sum(refunds) <= original charge` after the charge/refund domain is fully pulled. | `pull.refund.*`, `pull.dispute.*`, `consistency.amount_mismatch.refund_math` |
| Payment amount explanation | A provider-observed payment amount must be explained by the checkout session, invoice, discount, tax, proration, historical price snapshot, or explicit operator/off-channel record. It is not compared to the current live catalog price. | `consistency.amount_mismatch.payment_amount` |
| Subscription lifecycle | Active periods advance, failed periods enter dunning, grace exhaustion terminates, and pending sessions do not sit forever. Missing rebill/failure webhooks past the expected period boundary are lifecycle drift, not catalog drift. | `life.subscription.*`, `life.checkout_session.stale` |
| Remote recurring subscriptions | OpenRails expects one effective active billable remote subscription for a customer/merchant/product/tier group. Extra active remote subscriptions, including cross-provider duplicates, are duplicate facts. If no duplicate money has been collected yet, the finding is `consistency.duplicate.subscription` and the repair is a provider action to cancel/disable the extra remote subscription. If duplicate money was already collected, the primary integrity finding is `consistency.duplicate.provider_charge`, with the same provider-side cancellation linked as remediation while the extra remote subscription remains active. | `consistency.duplicate.subscription` + provider action |
| Scheduled provider decisions: `provider_intents`, `subscriptions.deletion_scheduled_at`, `scheduled_price_id` | Not-yet-due provider lag is consistent. Past-due local decisions must either be reflected at the provider, re-enqueued, or surfaced once automatic retry is abandoned. | provider action while retryable; `life.provider_intent.abandoned` once abandoned |
| Provider catalog objects | Local catalog is desired state. Provider product/plan/price objects must match the OpenRails catalog amount/cadence/provider metadata and OpenRails ownership markers. Drift is a catalog amount mismatch; repair is a provider catalog push or manual provider action. | `consistency.amount_mismatch.provider_catalog` + provider action |
| Grant ledger derivation | Each source event that should create access/credits has the right grant; each grant's source still exists and justifies it; grant windows/spec snapshots match the source event. | `derive.grant.*` |
| Grant effects: entitlements, product access, credit grants/ledger transfers | Every active grant has exactly its derived effects; every live effect traces to a live grant; windows/amounts/cadence match the grant spec snapshot. Product access and credit effects are derived, not authoritative. | `derive.grant_effect.*` |
| Credit-lot arithmetic | For each credit grant lot: `original_amount = spent + expired + revoked + refunded/frozen remainder + remaining`. Spend/expiry/revoke/refund are ledger transfers, never mutations. Revoked unspent credits freeze in `revoked_credits`; lapsed credits move to `expired_credits`; optional refund drains `revoked_credits` to `processor_clearing`. | `derive.grant_effect.excess` / `.mismatch` + ledger diagnostics |
| Double-entry ledger conservation | Per `(merchant,currency)`, `sum(account balances) = 0`; each account balance is derived from transfers, never stored. Held amount is derived from unresolved pending transfers. This is structurally guaranteed by the transfer model and stays a #512 smoke/diagnostic check, not a normal #511 finding. | #512 diagnostic/test |
| Ledger sign / credit limits | Customer balance may not go below the allowed arrears floor. `credit_limit_amount` / billing mode are admission and applier policy inputs, not a separate stored balance. | applier check + #512/#513 diagnostic |
| Invoices: `invoices`, `invoice_items`, `invoice_payments`, `usage_events` | Invoice amounts must match their source rows: `subtotal/total = sum(non-void items)`, `amount_paid = sum(settled invoice payments)`, `amount_due = total - amount_paid`, paid invoices have zero due, and closed-period billable usage is invoiced exactly once. | `consistency.duplicate.*` for duplicate coverage; `consistency.amount_mismatch.*` for amount mismatches |
| Admission windows and Redis spendgate | Admission compares `sum(captured + active holds)` against the effective policy. Redis is the hot-path source for in-flight holds; Postgres rows here are legacy/transitional unless still wired. | #513 admission checks |
| Provider/customer configuration | `provider_accounts` records a merchant-scoped provider account: merchant id, provider type, provider-returned `account_id`, display metadata, vault secret reference, and role (`primary`, `secondary`, `legacy`). Each merchant may bind multiple provider accounts per provider rail, but provider-owned rows must be tied to the exact `provider_account_id` FK that produced them, and only one enabled row per provider type is primary for new default checkout/catalog work. Provider credentials must resolve through the provider's account/profile/whoami endpoint to the `account_id` used by the targeted row. A mismatch is configuration drift and aborts the pull before any mirror write for that row; it is not a normal data inconsistency. Probe verdicts are credential-cache state, not merchant consistency state. | startup/runtime validation |
| Operational workflows | Merchant delete is gated by completed export; notification and credential-audit ledgers remain append/log hygiene unless a billing workflow consumes them. | app policy; outside #511 by default |

If an application-enforced row can be moved into a DB constraint without blocking
valid imports, prefer the constraint. The Convergence Engine should focus on
facts that cross tables, cross time, or cross the provider boundary.

Application-enforced rows that belong to #511 should map to a stable qualified
finding type (`pull.*`, `derive.*`, `life.*`, or `consistency.*`). Rows marked as #512/#513
diagnostics, startup/runtime validation, or operational policy are adjacent checks:
they may log, fail the operation, or feed an operator queue, but they are not
normal `reconciliation_findings` rows.

### Plane PULL — Provider-Pull (provider authoritative; resolved by the pull, surfaced by `check`)

Provider-Pull findings are grouped by provider-owned resource. The active #511
implementation intentionally uses only `missing` and `mismatch` shapes. A local
provider-owned row not found in a provider list/report is not reliable enough to
call an inconsistency: provider retention/reporting windows may be incomplete, the
pull may be partial, or the row may be attributed to the wrong provider/account.
Future provider-absence/tombstone work (#517) can revisit `pull.*.excess`, but the current
reconcile command should not emit it.

Provider-Pull is account-bound. Before `check` or `pull` treats provider output
as truth, OpenRails resolves the live credentials through the provider's
account/profile/whoami endpoint and compares the returned provider account id to
the targeted `provider_accounts.account_id`. Stripe uses the account id
from `/v1/account` (for example `acct_...`); NMI/Mobius uses the gateway
merchant/account identity returned by the profile endpoint; CCBill should use its
account/subaccount identity once implemented. A merchant may have multiple
bindings for the same provider type, such as Stripe-A for legacy rebills and
Stripe-B as primary for new checkout. A pull writes only inside the targeted
provider account row. If current credentials resolve to a different account than
the targeted row, the pull aborts as a configuration error. It must not insert `missing`
rows, overwrite `mismatch` fields, or mark a source domain fully reconciled from
the wrong account. Legacy mirror rows without a binding are ambiguous import state
and should be handled conservatively.

| Resource | Finding types | Repair |
|---|---|---|
| charge | `pull.charge.missing` / `.mismatch` | Insert missing provider charge mirrors; overwrite provider-owned charge status/amount/currency/timestamps when provider differs |
| subscription | `pull.subscription.missing` / `.mismatch` | Insert missing remote subscriptions when identity/plan resolve; overwrite provider-owned status/period/next-bill fields |
| refund | `pull.refund.missing` / `.mismatch` | Insert missing provider refunds; overwrite provider-owned refund amount/status/timestamps |
| dispute | `pull.dispute.missing` / `.mismatch` | Insert missing provider disputes/chargebacks; overwrite provider-owned dispute status/amount/deadline fields |
| vault/payment method | `pull.vault.missing` / `.mismatch` | Insert missing vault/payment-method mirrors; overwrite provider-owned metadata such as last4/expiry/token status |

> Provider-Pull drifts aren't "findings" in steady state — the pull overwrites them. They
> matter as the dry-run change log and the first wave of an import diff. The pull
> **never** overwrites local *intent* (`deletion_scheduled_at`, `scheduled_price_id`,
> pending `provider_intents`) or local catalog/product/price definitions. A divergence
> there is either expected provider lag, a provider action to enqueue/retry,
> `life.provider_intent.abandoned` once the action is abandoned, or a
> `consistency.amount_mismatch.provider_catalog`
> finding for catalog drift. A not-yet-due scheduled change (cancel set for Jun 28,
> pull on Jun 17 still sees the sub live) is fully consistent — expected lag.

### Plane DERIVE — Derivation (source → grant → grant effect; §2)

**Grant tier** (event ↔ grant):

| Finding type | Invariant — must hold | Shape | Class |
|---|---|---|---|
| `derive.grant.missing` | A completed payment / billed sub period / admin action has its grant | MISSING | AUTO |
| `derive.grant.excess` | A grant has a current, non-refunded, non-revoked source | EXCESS | ADMIN *(gated §3.2)* |
| `derive.grant.mismatch` | A grant's window + spec snapshot match its source (period, spec) | MISMATCH | AUTO |

**Grant-effect tier** (grant ↔ effect):

| Finding type | Invariant — must hold | Shape | Class |
|---|---|---|---|
| `derive.grant_effect.missing` | Every active grant has all its grant effects (entitlement windows, credit blocks, access) | MISSING | AUTO |
| `derive.grant_effect.excess` | Every live grant effect has a live grant | EXCESS | ADMIN *(gated; credit clawback = unspent only -> `revoked_credits`, reversible; refund is separate OPERATOR; see §11 (decision 4))* |
| `derive.grant_effect.mismatch` | A grant effect's window/amount/cadence matches the grant's spec (incl. per-renewal credit cadence) | MISMATCH | AUTO |

> "Grant effect" = an entitlement-feature window, a credit block, or a product-access
> row; the finding names which kind. Credits and product-access derivation are
> **net-new** — nothing checks them today (credits the largest gap). Revoke/refund
> clawback (`derive.grant_effect.excess`, credits) retracts the **unspent** remainder only via a reversing
> transfer to `revoked_credits` (reversible; money frozen, not refunded — refund is a
> separate OPERATOR step, optionally bundled into `RevokeGrant(grant,{refund})`; §11 (decision 4));
> spent credits are left as-is. An expired admin grant (`created_at + duration_days`)
> is a `derive.grant.excess` grant whose source no longer justifies it -> its grant effects become `derive.grant_effect.excess`.

> **Implementation status (2026-06-18).** `derive.grant_effect.missing` and
> `derive.grant_effect.excess` are implemented; `derive.grant.excess` is implemented
> for the refunded-payment case (a live grant whose backing payment was refunded →
> ADMIN surface-only). The two **`*.mismatch`** rows above are **NOT runtime checks
> and intentionally so** — they are *guaranteed by construction* (the §9 category),
> not pending work: `MaterializeGrant` is the sole writer of grant effects and writes
> them to match the grant, grants are immutable snapshots, and revocation now flows
> through the ledger too. So an effect cannot drift from its grant through normal
> operation — the only remaining mutation path is the admin `ExtendActiveBySubscription`
> (legitimately lengthens a window), against which a strict effect==grant check would
> merely *false-positive*. (`derive.grant.mismatch` — "grant matches its source" — is
> likewise not a drift check: a grant is a historical snapshot that is *supposed* to
> diverge from the evolving source.) These become real runtime checks only if a future
> architecture introduces an independent effect-mutation path. `derive.grant.missing`
> IS implemented (spec-aware): the positive "this payment should grant X" signal is the
> product's own grant spec via `payment → price → product`, so a completed one-off
> payment for a product whose `entitlements_spec`/`credits_spec` promises grants but that
> produced none is flagged (`ListUngrantedGrantablePaymentsByCustomer` → ADMIN,
> surface-only). Empty-spec products / pure fees are never flagged (no promised grant),
> and a refunded purchase keeps its `grant` event so it is not flagged (existence, not
> liveness). An earlier naive absence check ("any payment with no grant") was rejected for
> false-positiving on empty-spec/backfilled payments — the product spec is the signal.

### Plane LIFE — Lifecycle (clock/state-machine authoritative; converge forward, §3.1)

| Finding type | Invariant — must hold | Shape | Class |
|---|---|---|---|
| `life.subscription.period_overdue` | An `active` sub past `current_period_ends_at` is renewing or in dunning | MISMATCH | AUTO |
| `life.subscription.dunning_overdue` | A `past_due` sub has a live, on-time retry schedule | MISSING | AUTO |
| `life.subscription.grace_exhausted` | A `past_due` sub past grace/max-retries is terminal (converge: cancel now, revoke as-of grace end) | EXCESS | AUTO -> provider cancel action |
| `life.subscription.pending_stale` | A `pending` sub does not sit unconfirmed past threshold | EXCESS | AUTO |
| `life.provider_intent.abandoned` | A desired provider action remains unapplied, but no automatic retry will happen (max attempts exhausted, expired, no retry scheduled, retry policy disabled, provider unavailable, or manual action required) | MISMATCH | OPERATOR / ADMIN |
| `life.checkout_session.stale` | An expired `checkout_session` is cleaned up | EXCESS | AUTO |

### Plane CON — Consistency (internal accounting / referential)

CON is intentionally small. Every finding should fit one of three residual
consistency classes: duplicate, amount/explainability mismatch, or reference resolution.
Specific surfaces such as refund math, payment amount explanation, invoice math,
usage coverage, and credit-lot arithmetic are encoded in the qualified finding
type, such as `consistency.reference.source_reference` or
`consistency.amount_mismatch.invoice_math`. Apparent "invalid state"
cases should usually be moved into a DB constraint (`CHECK`, FK, unique/exclusion
index), the LIFE plane (clock/state-machine overdue), DERIVE (source -> grant ->
effect), or PULL (provider-observed truth). Do not use CON as a miscellaneous
local-audit bucket.

| Finding type family | Invariant — must hold | Shape | Class |
|---|---|---|---|
| `consistency.duplicate.<subject>` | Individually valid facts are invalid in combination because more than one exists where only one is allowed | EXCESS | ADMIN / OPERATOR |
| `consistency.amount_mismatch.<subject>` | Numbers or explainability equations do not add up: `A + B = C`, total <= source, observed amount is unexplained, expected coverage differs from actual coverage | MISMATCH | ADMIN / OPERATOR |
| `consistency.reference.<subject>` | A polymorphic, soft, or not-yet-FK-hardened reference resolves to no row, the wrong row, or a row in the wrong merchant/scope | EXCESS / MISMATCH | ADMIN |

`consistency.duplicate.provider_charge` includes duplicate provider-observed charges for the same customer/product/
period when no invoice, proration, or explicit operator action explains the overlap.
This is the primary finding once money has actually been collected twice. If the
duplicate charge came from overlapping active remote subscriptions, the repair may
also enqueue/link a provider action to cancel the extra provider subscription and
stop future billing. `consistency.duplicate.*` also covers duplicate invoice/usage coverage.
Duplicate local rows for the same provider transaction are structurally blocked by
`uq_payments_merchant_processor_transaction`; if a legacy/import path still
materializes one, it is a local mirror repair, not a normal recurring finding.
Initial subtypes: `provider_charge`, `subscription`, `invoice_usage`,
`invoice_payment`.

`consistency.amount_mismatch.*` is the generic arithmetic/explainability class. Examples: provider-observed
refund totals exceed the original charge after pull; a provider-observed payment
amount is not explained by checkout/invoice/discount/proration/operator snapshot;
invoice totals do not equal item/payment rows; closed-period billable usage does not
match invoice items; a credit lot's original amount does not equal spent plus
expired plus revoked plus refunded/frozen plus remaining.
Initial subtypes: `refund_math`, `payment_amount`, `invoice_math`, `usage_coverage`,
`credit_lot`, `provider_catalog`.

Do **not** create normal `consistency.amount_mismatch.*` findings for `ledger_conservation` or
`pending_resolver`. Ledger conservation is a #512 diagnostic/smoke check because
double-entry inserts make it structural. Pending transfer resolution is guarded by
`uq_ledger_transfers_pending_resolution`; an old unresolved pending transfer is a
LIFE/staleness problem, not an amount-mismatch finding.

`consistency.reference.*` is the fallback for references that cannot yet be made impossible through
Postgres: unresolved polymorphic references, soft references whose declared
type/id pair does not resolve, or opaque ledger/provider-intent references that
cannot be FK-backed. If a reference can become an FK/composite FK without blocking
valid imports, prefer the schema constraint over a runtime finding.
Initial subtypes: `source_reference`, `ledger_reference`, `provider_intent_reference`.

Do **not** create normal `consistency.reference.*` findings for FK-backed catalog/payment/checkout/
subscription scope relationships. Those belong in Postgres constraints or
one-off schema/import repair, not the runtime finding taxonomy.

Subscription price/product coherence is enforced by schema, not by a CON-plane
finding: `subscriptions(price_id, product_id, merchant_id)` references
`prices(id, product_id, merchant_id)`. Provider plan drift is
`consistency.amount_mismatch.provider_catalog`, because OpenRails catalog definitions
are desired state and the provider catalog must be pushed back into shape.

### Adjacent: billing viability (risk, not inconsistency)

"Active sub with a failed/expired payment method" and "sub processor ≠ PM processor"
are **predictive risk** states, not state inconsistencies — they predict a *future*
dunning failure. Surfaced as **OPERATOR** notifications (prompt the customer to update
the card), kept out of the core taxonomy to preserve "no irrelevant consistencies."

---

## 6. Manual admin overrides are consistent

A recorded human action must never read as a violation. With the grant log this
collapses to one rule:

- **Admin revoked access** -> a **revoke event** is appended to the grant log (the
  grant itself is never edited). So `derive.grant_effect.missing` asks *"was the
  grant effect **ever** granted?"* — a grant followed by a recorded revoke is not
  `MISSING`; only *no grant ever* is. Every revoke event carries a reason.
- **Admin granted with no payment** → an `admin`-sourced grant is itself the source,
  so `derive.grant.excess` ("grant needs a payment/subscription source") never applies to it.

Principle: **every grant effect traces to a grant; every grant traces to a recorded
source or an admin action; every removal carries a reason.** A finding fires only on
an *unexplained* gap.

---

## 7. The Convergence Engine (continuous architecture)

The Convergence Engine is one idempotent **`Converge(scope)`** over DERIVE, LIFE,
and CON (Provider-Pull feeds it). It is invoked from three places so the system never *holds*
an inconsistency:

1. **Inline**, after every source mutation (checkout completes, renewal bills, refund
   webhook lands, dunning transitions, admin grants).
2. **After every `reconcile pull`** (the pull just rewrote the sources).
3. **On a sweep** (scheduled / boot rescan, and the bulk pass after a legacy import).

There is no separate "enforce" command or crank: while the server is up the engine
runs continuously (1 + 3); `reconcile pull` runs a one-shot `Converge` only because
the CLI may run when the server is down.

Requirements: **idempotent** (second run is a no-op), **scope-narrowable**
(`Converge(customer | subscription | merchant | global)` — cheap inline, exhaustive on
sweep), and the **single writer** of grants and grant effects (derive-1 / derive-2), so
each invariant has exactly one implementation.

---

## 8. Findings ledger & finding states

All findings share `openrails.reconciliation_findings` (extended: `finding_type`
admits qualified finding types; `provider` admits a `self` sentinel for derive/life/consistency
findings; severity lowercase). States:

- `open` → `auto_fixed` | `admin_pending` | `resolved` | `dismissed`; disappearance
  on a later run auto-resolves (`auto_vanished`); `requires_admin` rows are the admin
  queue.
- `held` — an `EXCESS` repair blocked by the confirmed-absence gate (§3.2); carries
  `held_pending_source_reconciliation` so triage sees *why* a destructive repair has
  not fired.
- `indeterminate` — the engine could not evaluate the invariant because the authority
  was **unreachable** (provider down / pull failed) or the evidence was **ambiguous**
  (two identity matches; an in-flight intent of unknown outcome). Resolution: gather
  more evidence (re-pull, verifier); only a genuinely-ambiguous residue escalates to
  an operator to pick. This is always evidence-limited, never model-limited.

---

## 9. Already guaranteed by DB constraints (NOT engine invariants)

Excluded to avoid redundancy — the schema makes these impossible:

- one active open-ended entitlement per `(merchant, customer, entitlement)` and no
  overlapping live entitlement windows (GIST);
- entitlement revoke fields together + valid time windows;
- cancelled subscriptions without timestamp/type, cancelled subscriptions with a
  live retry schedule, `past_due` subscriptions without period end, and invalid
  subscription periods;
- subscription price/product mismatch via
  `subscriptions_price_product_merchant_fkey`;
- more than one local active/pending/past_due subscription per
  `(merchant, customer, product)` or local active/pending tier-group;
- duplicate local provider transactions via
  `uq_payments_merchant_processor_transaction`;
- double-entry transfer basics: amount > 0, debit != credit, valid phase,
  resolver iff `pending_id`, one resolver per pending transfer, same-currency
  debit/credit accounts, append-only transfer/account roles;
- append-only grant basics: valid event/kind/source type, credit grants carry
  positive amount+currency, grant windows are valid, and each grant has at most
  one terminal event;
- natural-key duplicates for customers, processor customers, payment methods,
  linked wallets, blocklist entries, catalog product slugs, price financial
  substance, invoice periods, invoice item sources, usage-event idempotency, and
  open reconciliation/catalog-drift finding identities.

---

## 10. Build implications

- `reconcile fix` → **`reconcile pull`**: semantics become "overwrite mirror + run the
  Convergence Engine"; `check` becomes a pure dry-run change log.
- **Grants layer:** generalize `product_access_grants` into the canonical `grants`
  table; repoint `entitlements` and tag credit deposits with their `grant_id`
  (a ledger transfer under the double-entry ledger reorg); migrate
  existing rows; implement derive-1 / derive-2 as the sole writers.
- Replace the old `PS-*`/`P-E-*` codes with qualified finding types under `pull.*`, `derive.*`, `life.*`, and `consistency.*`; extend the ledger
  CHECK + add the `self` provider, the `held` status, and the `indeterminate` status.
- **`Converge(scope)`** as the single idempotent engine / sole grant-effect writer.
- **Import mode:** per-source-domain "fully reconciled" flags gating every `EXCESS`
  repair (§3.2); bulk-sweep entry point; converge-not-replay for LIFE.

## 11. Resolved decisions

1. **Provider authoritative but incomplete w.r.t. future-dated intent.** The pull
   overwrites observed provider state but never deletes a standing intent
   (`deletion_scheduled_at`, `scheduled_price_id`, pending `provider_intents`); the
   engine is schedule-aware: not-yet-due provider lag is expected; retryable
   lag remains a provider action; abandoned past-due work becomes `life.provider_intent.abandoned`.
2. **`reconcile pull` always converges** (no `--enforce` flag); continuous in-server,
   one-shot via CLI.
3. **Confirmed-absence gate is per source domain** (subscriptions / payments / grants).
4. **Credit clawback retracts unspent only**, as a reversing `ledger_transfer`
   (never a balance mutation — there is no stored balance to rewrite; balance is
   derived from the transfer log, so "freeze the lot" = append a clawback event).
   The destination account distinguishes the cause: time-lapse →
   `expired_credits`; **admin-revoke / refund-driven retraction → `revoked_credits`**
   (a distinct platform holding account — the money is *frozen*, not returned, and
   the clawback is **reversible** by a counter-transfer; the original deposit +
   the clawback both stay in the immutable log). A revoked credit lot already
   drops out of `ListSpendableCreditLots`, so this clawback exists only to make
   the *derived balance* agree with the *spendable* set — exactly mirroring how
   expiry already emits a transfer. **Refunds are never automatic** (decision: Paul
   2026-06-17): returning money to the card/wallet is a separate OPERATOR action
   (`revoked_credits → processor_clearing` + a provider refund), optionally bundled
   into one `RevokeGrant(grant, {refund})` call for convenience — the `refund` flag
   IS the operator authorization, defaults to the clawed-back unspent amount, and
   (unlike clawback-only) is **not** reversible. Spent credits are left as-is.
5. **Default pull = the head** (current state); optional `--since` / date range
   backfills history for replaying old grant effects.
