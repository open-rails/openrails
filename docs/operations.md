# OpenRails Operations Manual

How OpenRails stays consistent with the payment providers, what to run when it
isn't, and the safety levers for doing any of this against production
credentials. Decisions recorded 2026-06-11 (issues #107, #344–#348, #355,
#358, #359).

## The ownership model

Three facts decide every consistency mechanism in the system:

1. **OpenRails owns the catalog** (products + prices). Providers hold copies.
2. **The provider owns money state** (is a subscription alive, what was
   charged). OpenRails holds copies.
3. **OpenRails owns entitlements — but they are derived**, deterministically,
   from catalog + money state + admin grants.

So there are exactly four ways the system diverges, each with its own
mechanism:

| # | Divergence | Direction | Mechanism |
|---|---|---|---|
| 1 | Catalog wrong at the provider | push (OpenRails → provider) | `bootstrap apply` — verify-or-create at apply time, re-applied idempotently on every boot; alert-only drift watching; provider extras are logged, and archived only under the explicit `--exhaustive` flag (#357) |
| 2 | Money state wrong locally | pull (provider → OpenRails) | webhooks in real time; **reconcile (#107)** as the batch truth-pull: advisory diffs, enforce converges local state |
| 3 | Outbound action never executed | (intent, not sync) | **durable intent + replay** — see "Durability model" below; reconcile is its *detector* |
| 4 | Entitlements inconsistent | derived | re-run the derivation once 1–3 are true; `internal/audit` checks the local derivation, reconcile's PS-9 converges it against trued-up inputs |

## Durability model

**Outbound — durability is OUR job.** Every mutation OpenRails wants to make
against a provider must survive failure of the attempt. The mechanism is the
**provider intent ledger** (`billing.provider_intents`, #358 phase A —
shipped): every outbound mutation is durably recorded with an idempotency key
(one row per logical intent; re-enqueues dedupe), origin (user/admin/system),
and a relevance window. Two scheduled workers drain it: the **executor**
(every minute, and on startup) claims due intents under a SKIP LOCKED lease,
checks the type's relevance, gates on operating mode × origin (user/admin
intents execute under `limited`; system intents need `full`; nothing writes
under `readonly`), executes, and classifies the outcome; the **verifier**
(every 5 minutes) resolves `unknown_needs_verify` intents via provider READS
before any retry. Failure reasons — provider down, `readonly`/`limited` mode,
kill switch, bad credentials — are not errors, they are reasons an intent
stays *pending*, recorded on the row. When the blocker lifts, the queue
drains. Intents that outlive their relevance window (a delete after the
subscription resumed; a rebill past the dunning window) are superseded or
expire instead of firing stale.

Phase A migrated the deferred NMI `delete_subscription` onto the ledger
(verify-then-execute: query the subscription first, absent = success) and
retired the boot rescan + the dedicated delete River job — a startup sweep
converts any surviving `DeletionScheduledAt` markers into ledger intents
(idempotent), and `DeletionScheduledAt` remains as the read model, cleared by
the intent's finalize. Still on their specialized mechanisms until phases
B–D: refunds/Stripe ops (B), the `manual_rebill_attempts` claim table (C,
folds in as `intent_type=manual_rebill`), and catalog archive ops (D);
catalog `pending_manual_link` states remain converged by re-apply-on-boot.

Execution is **effectively-once**, never assumed exactly-once (impossible over
a network). Per class: money-movers (rebills, refunds) park ambiguous outcomes
as `unknown_needs_verify` and are resolved by *reading* the provider before
any retry — a charge is never blind-retried; deletes/cancels are
verify-then-execute (already-deleted = success); creates are content-addressed
find-or-create (idempotent by construction). Stripe ops additionally send
`Idempotency-Key`.

**Inbound — durability is the PROVIDER's job.** NMI, CCBill and Stripe all
deliver webhooks at-least-once and retry failed deliveries from their end;
our handlers are idempotent for exactly that reason. **There is deliberately
no local inbound queue**: it would share fate with the database it protects —
if we cannot update the subscription row, we cannot enqueue either. The
backstop for an outage long enough to exhaust the provider's webhook retries
is reconcile's pull: the next run reads provider state directly and the missed
event materializes as a finding. Inbound therefore has two layers — provider
retries + reconcile — and we build neither.

## Reconcile (#107)

Manual-only — **never scheduled**. Two modes, neither ever writes to a
provider:

- `billing reconcile check` (advisory): pull provider truth, diff, persist
  findings, change nothing.
- `billing reconcile fix` (enforce): same pull + converge **local** state to
  provider truth (status/periods adopted, payments backfilled,
  subscription-sourced entitlements granted/revoked — admin-grant comps are
  untouchable). Anything requiring a *remote* action lands in the admin queue
  for a human.

Findings have stable identity across runs (re-runs update, vanished findings
auto-resolve), carry intent evidence ("our recorded delete never executed —
the executor replays it") so the admin queue holds only genuine unknowns, and
include the dunning-forensics report (remote decline timeline vs local retry
state). NMI safety note: cancelled subscriptions *vanish* from NMI's recurring
report rather than changing status, so the engine refuses to act when the
remote active set is implausibly small versus local (circuit breaker against
mass-cancellation from a bad fetch).

## Dunning (#359)

No knobs. The schedule derives from the price's billing cycle — total retry
span always well inside one cycle:

| Cadence | Retries | Spacing | ~Window |
|---|---|---|---|
| 1 day | none | — | 0 (first failure is terminal) |
| ~weekly | 2 | 1 day | ~2–3 days |
| monthly+ | 5 | 3 days | ~15 days (capped — never more generous) |

The staleness window ("never charge a months-old failure") is derived from
the same schedule, so it cannot be misconfigured. Terminal failure = cancel +
revoke entitlements + scheduled processor-side delete through the one shared
deferred-delete mechanism. `feature_flags.dunning_mode`
(on / dry_run_only / off) still dials *whether* dunning runs; `dry_run_only`
is the pause that preserves retry state.

(Benchmark: Stripe's own default for monthly billing is 8 ML-timed retries
over 2 weeks — same span, more attempts; ours is sparser because each NMI
decline costs a per-transaction fee. Stripe-billed subscriptions use Stripe's
dunning; this section governs NMI-backed manual dunning.)

## Safety levers (recap — full details in README "Operating modes")

- `mode = full | limited | readonly` — behavior dial. `limited`: nothing
  system-initiated touches a provider; everything user/admin-asked works.
  `readonly`: zero provider writes, wire-enforced on all three rails (NMI
  direct-post, Stripe transport, Solana transaction submission).
- `test_env = true|false` — credential sandbox enforcement, orthogonal to
  mode: sandbox routing + Stripe live-key refusal + the NMI boot probe (one
  auth on the non-issued test card; a decline proves production credentials
  and refuses the boot) + Solana devnet. Dev-only.
- `feature_flags.disable_processor_subscription_deletions` — kill switch on
  NMI deletes, stricter than `limited` (blocks even user-asked deletes);
  blocked deletes PARK as pending intents on the ledger (reason recorded) and
  the intent executor drains them once the switch lifts.

**Cutover posture** (migration/reconciliation against production
credentials): `MODE=limited` +
`FEATURE_FLAGS_DISABLE_PROCESSOR_SUBSCRIPTION_DELETIONS=true` (+ optionally
`FEATURE_FLAGS_DISABLE_ENTITLEMENT_EXPIRATION=true`). Exit in order: lift the
deletion switch (the intent executor drains the parked deletes within a
minute — no restart needed), then `MODE=full`. All paused work is delayed,
not lost; missed billing periods are never back-billed.
