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
   faithful mirror.
2. **The Convergence Engine.** Given a truthful ledger, drive every *projection*
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

## 1. The truth model: five planes

Every invariant has one shape: **`A` should equal `B`, but doesn't.** You can't
repair it until you know *which side is right* — so each invariant is classified by
the authoritative `B` it is checked against, and that authority dictates how the
mismatch is repaired. The taxonomy is organized by these planes so the codes mirror
how the engine actually repairs.

The five split into two groups.

**Two planes sit at the boundary with the processor** — the *same* two facts (a
local row vs. the processor), in *opposite* directions:

- **M — Mirror** *(inbound)*: the processor is authoritative; copy its truth into
  our local row. This is the entire job of the pull.
- **N — Intent** *(outbound)*: **we** are authoritative — we recorded a decision
  (cancel, delete, dedupe) the processor hasn't carried out, so we push the change
  *out* to the processor. The only plane that mutates the processor. (Not
  "external" — Mirror touches the processor too; what is distinctive here is the
  *outbound* direction.)

**Three planes are internal** — a local row checked against a local yardstick, no
processor involved:

- **D — Derivation**: a projection (entitlement / credit / access) vs. the source
  event (payment / subscription / admin action) that should produce it, via the
  grants layer (§2). Authority = the source ledger.
- **L — Lifecycle**: a stateful record vs. where the **clock + its state machine**
  say it should be by now — the "overdue / hasn't advanced" plane (dunning unrun,
  pending stale, grace exhausted). Authority = time.
- **I — Integrity-Rule**: a record vs. an internal arithmetic/referential **rule**
  not enforced by a DB constraint (refund totals ≤ original, no duplicate charges,
  price ∈ product). Authority = the rule. (Accounting's *trial balance / tie-out*,
  as opposed to Mirror's *bank reconciliation*.)

| Plane | `A` — the fact | `B` — authority | Repaired by | Channel |
|---|---|---|---|---|
| **M** Mirror | local row | the processor | overwrite local (the pull) | local write |
| **D** Derivation | a projection | the source ledger (via grants) | replay / retract the projection | local write |
| **L** Lifecycle | a record's state | the clock + state machine | converge the record forward | local write (may emit **N**) |
| **N** Intent | the processor's state | our recorded decision | push the change to the processor | `provider_intents` / operator |
| **I** Integrity-Rule | a record | an internal rule | fix the data | local write / operator |

The planes are also the engine's modules: **M is the pull; D, L, N, I are the
Convergence Engine's passes, run in that order** — sources must be truthful before
projections are derived, and projections settled before external corrections.

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

### Code scheme

- **Compact stable ID:** `<plane-letter><n>` — `M1`, `D2`, `L3`, `N1`, `I4`.
  Greppable, stable; numbers are never recompacted when a check retires.
- **Canonical slug:** `<plane>.<subject>.<shape>` — e.g.
  `derivation.grant.missing`, `lifecycle.subscription.dunning_overdue`,
  `intent.subscription.billing_leak`. Used everywhere a human reads output.

### Remediation class (orthogonal to plane & shape)

| Class | When | Channel |
|---|---|---|
| **AUTO** | Idempotent, reversible local write, unambiguous | Applied immediately |
| **ADMIN** | Known but consequential | Queued; on approval → local write or `provider_intent` |
| **OPERATOR** | No API to us, or too sensitive to ever auto-fire (refund issuance, dispute response) | Surfaced + runbook; human acts in dashboard |

---

## 2. The grants layer: source → grant → projection

Access is a projection of recorded source events. To make convergence **one uniform
mechanic** instead of a dozen bespoke pairwise checks, a canonical **grant** sits
between the source events and the fine-grained projections:

```
SOURCE EVENTS   (immutable facts: payment captured · subscription period billed · admin action)
      │   derive-1: event → grant
      ▼
GRANTS  ("customer C is entitled to product P for [start,end), justified by source S,
          status active|revoked(+reason), spec snapshot")
      │   derive-2: grant → projections   (mechanical fan-out from the snapshot)
      ▼
PROJECTIONS   (entitlement-feature windows · credit blocks · product access) — what the app reads
```

`openrails.product_access_grants` is already ~80% this shape (`source_type` ∈
{purchase, subscription, admin}, `source_id`, `payment_id`, `status` active|revoked,
`revoked_at`/`revoke_reason`, `starts_at`/`ends_at`). The change is to **promote it
(or a generalized `grants` table) to be the layer that entitlements *and* credits
*and* access all derive from**, and to tag credit deposits with the grant
(replacing the free-string `money_transactions.source`).

> **Credit substrate.** The *credit* projection is the one that touches money, so
> it composes with the parallel double-entry ledger reorg: a credit grant's
> projection is a **ledger transfer** (deposit into the customer-balance account)
> tagged with the grant, and clawback is a **reversing transfer**, not a mutation.
> Entitlement and product-access projections are plain local rows. See the
> double-entry ledger work for the substrate; this doc only requires that a credit
> deposit carry its `grant_id`.

Derivation is then two pure, idempotent steps:

- **derive-1 (event → grant):** a completed payment / billed subscription period /
  admin action produces its grant(s), dated to the event, snapshotting the product's
  `entitlements_spec` + `credits_spec`.
- **derive-2 (grant → projections):** each grant fans out to its entitlement-feature
  windows, credit blocks, and product-access row — mechanically, from the snapshot.

Payoff: the whole **D-plane collapses to two tiers** — "every event has its grant"
and "every grant has exactly its projections" — *uniform across entitlements,
credits, and access*. The "manual override is consistent" rule lives in exactly one
place (the grant's `revoked_at` + `revoke_reason`). Replay (§4) is a pure function
of the grant.

---

## 3. Two doctrines that make legacy import safe

A legacy importer drops thousands of pre-broken records at once: dunning that hasn't
run in months, entitlements whose subscription was never imported, refunds recorded
out of order. Two rules keep the Convergence Engine from doing damage.

### 3.1 Replay vs. converge — never replay a side effect

- **Projections are replayable.** Recreating an entitlement / credit / access window
  has no external side effect, so the engine freely **replays history** (§4): it
  writes the window that *should have existed*, dated to the source event.
- **External actions are NOT replayable.** The engine must never re-attempt *missed
  historical* charges, rebills, or dunning cycles. If dunning hasn't run in three
  months, it does **not** try to charge the card three times. It computes **where the
  record should be now** (three months unpaid past grace ⇒ terminal) and converges to
  that single current state, revoking projections as of when grace *would* have ended.

> Rule: `L`-plane repairs **converge to the correct current state**; they never
> replay the side-effecting steps that were skipped.

### 3.2 The confirmed-absence gate — MATERIALIZE freely, RETRACT carefully

`MISSING`/MATERIALIZE is additive and safe → **AUTO even in bulk import.**

`EXCESS`/RETRACT is destructive (revokes access, cancels) → it requires a **confirmed
absence**: a *completed authoritative pull/import* proving the justifying source
truly does not exist. "Source not present in the local DB" is **not** confirmed
absence during import — it usually just means *not imported yet*.

> Poster case: *"a user has had an entitlement for months but no subscription."* The
> entitlement's grant has no live source → `D2 derivation.grant.excess`.
> Pre-reconciliation it is **HELD** (the subscription may be un-imported). Only after
> the import + a full provider pull confirm no source exists does it become a real
> RETRACT — and even then mass retraction is **ADMIN**-gated, not silent.

The gate is **per source domain** (subscriptions / payments / admin grants): an
`EXCESS` repair does not fire as AUTO until that domain is marked **fully reconciled**
for the merchant. Until then it accumulates as a held finding for triage.

---

## 4. Repair is a historical replay (anchored to source time)

When the engine materializes a projection, it anchors the window to the **source
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

### Plane M — Mirror (processor authoritative; resolved by the pull, surfaced by `check`)

| ID | Slug | Invariant — must hold | Shape | Class |
|---|---|---|---|---|
| `M1` | mirror.subscription.status | Local sub observed status/period/next-bill = processor | MISMATCH | AUTO (pull) |
| `M2` | mirror.charge.missing | Every successful processor charge has a local payment | MISSING | AUTO (pull) |
| `M3` | mirror.refund.missing | Every processor refund is recorded locally | MISSING | AUTO (pull) |
| `M4` | mirror.dispute.missing | Every processor chargeback/dispute is recorded | MISSING | AUTO (pull) → may emit `N4` |
| `M5` | mirror.vault.mismatch | Payment-method metadata (last4/expiry/token) = processor vault | MISMATCH | AUTO (pull) |
| `M6` | mirror.plan.mismatch | Local price amount/cadence = plan the processor bills | MISMATCH | ADMIN |

> M-plane drifts aren't "findings" in steady state — the pull overwrites them. They
> matter as the dry-run change log and the first wave of an import diff. The pull
> **never** overwrites local *intent* (`deletion_scheduled_at`, `scheduled_price_id`,
> pending `provider_intents`). A divergence there becomes an `N` finding **only when
> the scheduled change is past-due**; a not-yet-due scheduled change (cancel set for
> Jun 28, pull on Jun 17 still sees the sub live) is fully consistent — expected lag.

### Plane D — Derivation (source → grant → projection; §2)

**Grant tier** (event ↔ grant):

| ID | Slug | Invariant — must hold | Shape | Class |
|---|---|---|---|---|
| `D1` | derivation.grant.missing | A completed payment / billed sub period / admin action has its grant | MISSING | AUTO |
| `D2` | derivation.grant.excess | A grant has a current, non-refunded, non-revoked source | EXCESS | ADMIN *(gated §3.2)* |
| `D3` | derivation.grant.mismatch | A grant's window + spec snapshot match its source (period, spec) | MISMATCH | AUTO |

**Projection tier** (grant ↔ projection):

| ID | Slug | Invariant — must hold | Shape | Class |
|---|---|---|---|---|
| `D4` | derivation.projection.missing | Every active grant has all its projection rows (entitlement windows, credit blocks, access) | MISSING | AUTO |
| `D5` | derivation.projection.excess | Every live projection has a live grant | EXCESS | ADMIN *(gated; credit clawback = unspent only)* |
| `D6` | derivation.projection.mismatch | A projection's window/amount/cadence matches the grant's spec (incl. per-renewal credit cadence) | MISMATCH | AUTO |

> "Projection" = an entitlement-feature window, a credit block, or a product-access
> row; the finding names which kind. Credits and product-access derivation are
> **net-new** — nothing checks them today (credits the largest gap). Refund clawback
> (`D5`, credits) retracts the **unspent** remainder only; spent credits are left
> as-is. An expired admin grant (`created_at + duration_days`) is a `D2` grant whose
> source no longer justifies it → its projections become `D5`.

### Plane L — Lifecycle (clock/state-machine authoritative; converge forward, §3.1)

| ID | Slug | Invariant — must hold | Shape | Class |
|---|---|---|---|---|
| `L1` | lifecycle.subscription.period_overdue | An `active` sub past `current_period_ends_at` is renewing or in dunning | MISMATCH | AUTO |
| `L2` | lifecycle.subscription.dunning_overdue | A `past_due` sub has a live, on-time retry schedule | MISSING | AUTO |
| `L3` | lifecycle.subscription.grace_exhausted | A `past_due` sub past grace/max-retries is terminal (converge: cancel now, revoke as-of grace end) | EXCESS | AUTO → emits `N1` |
| `L4` | lifecycle.subscription.pending_stale | A `pending` sub does not sit unconfirmed past threshold | EXCESS | AUTO |
| `L5` | lifecycle.intent.stuck | A `provider_intent` is not non-terminal past its threshold | MISMATCH | OPERATOR (diagnostic) |
| `L6` | lifecycle.checkout_session.stale | An expired `checkout_session` is cleaned up | EXCESS | AUTO |

### Plane N — Intent (local decision authoritative; the processor must change)

| ID | Slug | Invariant — must hold | Shape | Class |
|---|---|---|---|---|
| `N1` | intent.subscription.billing_leak | A sub terminal/cancelled locally is **not** still billed by the processor | EXCESS | ADMIN (AUTO when dunning-driven) |
| `N2` | intent.subscription.undelivered | A standing local decision (delete/tier-change) is reflected at the processor, or re-driven | MISSING | AUTO (re-enqueue) |
| `N3` | intent.subscription.duplicate | A customer has no overlapping active remote subs for the same thing | EXCESS | ADMIN (cancel) + OPERATOR (refund) |
| `N4` | intent.dispute.unresolved | A chargeback has a recorded response/decision | MISSING | OPERATOR |

### Plane I — Integrity-Rule (internal rules; financial / referential)

| ID | Slug | Invariant — must hold | Shape | Class |
|---|---|---|---|---|
| `I1` | integrity.charge.duplicate | No two successful charges for the same product + period | EXCESS | OPERATOR (refund) |
| `I2` | integrity.refund.math | Σ refunds ≤ original; every refund links a real original | MISMATCH | OPERATOR |
| `I3` | integrity.price.product | A sub's `price` belongs to its `product` | MISMATCH | OPERATOR |
| `I4` | integrity.payment.amount | A payment amount reconciles with its price net of recorded discount | MISMATCH | ADMIN |
| `I6` | integrity.reference.unresolved | A reference resolves to a row of its declared type | EXCESS | ADMIN |

> `I5 integrity.invoice.*` (arrears: invoice total = Σ items, paid invoices fully
> covered, no unbilled closed-period usage) is a **distinct surface**
> (`invoices`/`usage_events`/`money_windows`), tracked separately.

### Adjacent: billing viability (risk, not inconsistency)

"Active sub with a failed/expired payment method" and "sub processor ≠ PM processor"
are **predictive risk** states, not state inconsistencies — they predict a *future*
dunning failure. Surfaced as **OPERATOR** notifications (prompt the customer to update
the card), kept out of the core taxonomy to preserve "no irrelevant consistencies."

---

## 6. Manual admin overrides are consistent

A recorded human action must never read as a violation. With the grants layer this
collapses to one rule on the grant row:

- **Admin revoked access** → the grant persists with `revoked_at` + `revoke_reason`.
  So `D4` (grant → projection) asks *"was the projection **ever** created?"* — a
  recorded-revoked grant is not `MISSING`; only *no grant ever* is. The revoke fields
  always travel together (a revocation always carries a reason).
- **Admin granted with no payment** → an `admin`-sourced grant is itself the source,
  so `D2` ("grant needs a payment/subscription source") never applies to it.

Principle: **every projection traces to a grant; every grant traces to a recorded
source or an admin action; every removal carries a reason.** A finding fires only on
an *unexplained* gap.

---

## 7. The Convergence Engine (continuous architecture)

The four enforcement planes are one idempotent **`Converge(scope)`** (the M-plane is
the pull that feeds it). It is invoked from three places so the system never *holds*
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
sweep), and the **single writer** of grants and projections (derive-1 / derive-2), so
each invariant has exactly one implementation.

---

## 8. Findings ledger & finding states

All findings share `openrails.reconciliation_findings` (extended: `finding_type`
admits the new IDs; `provider` admits a `self` sentinel for D/L/I findings; severity
lowercase). States:

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

Excluded to avoid redundancy — the schema makes these impossible: one active
open-ended entitlement per `(merchant, customer, entitlement)` + no overlapping live
windows (GIST); revoke fields together + valid window; cancelled sub has timestamp/type
+ no live retry schedule; `past_due` has period end; valid period; one
active/pending/past_due sub per `(merchant, customer, product)` and per
`(customer, tier_group)`; payment not future-dated; unique
`(merchant, processor, transaction_id)`; credit-deposit idempotency; sub
`payment_method_id` FK.

---

## 10. Build implications

- `reconcile fix` → **`reconcile pull`**: semantics become "overwrite mirror + run the
  Convergence Engine"; `check` becomes a pure dry-run change log.
- **Grants layer:** generalize `product_access_grants` into the canonical `grants`
  table; repoint `entitlements` and tag credit deposits with their `grant_id`
  (a ledger transfer under the double-entry ledger reorg); migrate
  existing rows; implement derive-1 / derive-2 as the sole writers.
- Replace the `PS-*`/`P-E-*` codes with the `M/D/L/N/I` scheme; extend the ledger
  CHECK + add the `self` provider, the `held` status, and the `indeterminate` status.
- **`Converge(scope)`** as the single idempotent engine / sole projection writer.
- **Import mode:** per-source-domain "fully reconciled" flags gating every `EXCESS`
  repair (§3.2); bulk-sweep entry point; converge-not-replay for L.

## 11. Resolved decisions

1. **Provider authoritative but incomplete w.r.t. future-dated intent.** The pull
   overwrites observed provider state but never deletes a standing intent
   (`deletion_scheduled_at`, `scheduled_price_id`, pending `provider_intents`); the
   engine is schedule-aware and faults (`N`) only on a **past-due** intent.
2. **`reconcile pull` always converges** (no `--enforce` flag); continuous in-server,
   one-shot via CLI.
3. **Confirmed-absence gate is per source domain** (subscriptions / payments / grants).
4. **Credit clawback retracts unspent only**; spent credits are left as-is.
5. **Default pull = the head** (current state); optional `--since` / date range
   backfills history for replaying old projections.
