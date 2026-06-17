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
| 1 | Catalog wrong at the provider | push (OpenRails → provider) | `bootstrap apply` — verify-or-create at apply time, re-applied idempotently on every boot; alert-only drift watching; provider extras are logged, and archived only under the explicit `--prune` flag (#357) |
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
the intent's finalize. Phase B moved admin refunds onto the ledger
(`nmi_refund`/`stripe_refund`, admin-origin): the local reservation flow is
unchanged, but the provider-side money movement is a durable intent executed
synchronously from the request — anything not finished inline (mode park,
ambiguity) is drained by the scheduled executor/verifier, whose finalize
completes (or releases) the reservation. Phase C retired the
`manual_rebill_attempts` claim table (migration 011): dunning charges are
`intent_type=manual_rebill`, system-origin (they WAIT under `limited`/
`readonly`), one intent per (subscription, period end, attempt ordinal), with
the dunning window as the relevance window; the verifier resolves ambiguous
charges by querying NMI for the period's order reference and repairs the
subscription lifecycle on late-confirmed success. Phase D routed catalog
archive ops through the ledger (`stripe_archive_product`,
`stripe_archive_price`, `solana_sunset_plan`; admin-origin): `bootstrap apply
--prune` (#357) enqueues+executes an intent per extra, so a provider
being down no longer aborts the sweep — items park and drain durably, and an
intent expires if its object joins the local catalog before execution. NMI
deliberately has NO archive write path (plan edits affect live subscribers);
Solana extras are detected from our own stored plan handles (the chain has no
plan enumeration) and sunset flips only the plan status. With D, **all four
phases are shipped — every outbound provider mutation flows through the
ledger.** Catalog `pending_manual_link` states remain converged by
re-apply-on-boot (idempotent by construction — no intent needed).

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

**Inspecting the ledger.** `openrails intents` (CLI; `--status pending|all|...`,
`--provider`, `--type`, `--merchant`, `--format table|json`) and
`GET /v1/admin/intents` list the queued outbound mutations read-only. Under
`mode=limited`/`readonly` this doubles as the dry-run view of a cutover:
pending rows are exactly what the executor drains when the mode lifts, and
each row's `executes_under` (derived from its origin via the GateExecution
matrix) says whether `limited` suffices or `full` is required.

**Materialized backlog under mode=limited (#366).** The dunning worker's scan
runs under `limited` and records its decisions instead of skipping: window-expired
past_due subs (a freshly migrated backlog's bulk) get the local no-charge
cancel + downgrade immediately, and in-window charges enqueue as PARKED
system-origin manual_rebill intents — bounded by `expires_at` = the dunning
window, so one can never fire stale after the mode lifts; the handler also
re-checks relevance (still past_due, same period) at execution. Materialize
never claims the subscription (claiming writes `last_retry_at`, which is the
dunning-forensics evidence imported from legacy) and never applies failure
policy. `readonly` is unchanged: pure dry-run observer.

**Account guard / credential rotation (#365).** Every intent is stamped at
enqueue with a fingerprint of the provider ACCOUNT the producer was configured
against — always the account's own identity, never the credential: NMI uses
the merchant identity from the gateway's profile report (query.php
`report_type=profile` -> "nmi:<company> <email>"), Stripe the `acct_...` id
from GET /v1/account; both fetched lazily and cached per key. The executor AND
verifier compare it against the current credentials and park/defer on
mismatch — a queue built against one account is never executed against
another (NMI ids are small numerics that collide across accounts: the wrong
account's subscription would be deleted, the wrong customer's card charged).
Operational rules:

- Key rotation within the SAME account (NMI or Stripe): no effect — the
  fingerprint is refetched under the new key and matches.
- Pointing a provider key at a DIFFERENT account: pending intents park with
  "provider account changed since enqueue". Drain or expire them first, or —
  if adopting the new account deliberately — re-stamp with
  `openrails intents refingerprint --provider=<name> --merchant=<slug> --yes`.
- A failed fingerprint fetch (provider down) skips the guard for that pass
  (warn logged) rather than blocking the ledger.
- Intents enqueued before #365 carry no stamp and execute ungated.

## Reconcile (#107)

Manual-only — **never scheduled**. Two modes, neither ever writes to a
provider:

- `openrails reconcile check` (advisory): pull provider truth, diff, persist
  findings, change nothing.
- `openrails reconcile fix` (enforce): same pull + converge **local** state to
  provider truth (status/periods adopted, payments backfilled,
  subscription-sourced entitlements granted/revoked — admin-grant comps are
  untouchable). Anything requiring a *remote* action lands in the admin queue
  for a human.
- `openrails reconcile report [--run=ID]`: render the latest (or given) run's
  summary, dunning forensics, and standing open findings.

`check` and `fix` take `--provider nmi|ccbill|stripe|solana` (repeatable;
default = every configured provider), `--since` / `--until` (RFC3339 or
`YYYY-MM-DD`, bounding the transaction window), `--merchant` (slug or id;
default merchant otherwise) and `--format table|json`. The same engine sits
behind the admin API: `POST /v1/admin/reconcile/runs` `{mode, providers,
since, until}` runs synchronously; `GET .../runs`,
`GET .../runs/:id`, `GET .../findings?status=&provider=&type=&admin_queue=`,
and `POST .../findings/:id/{ack,dismiss}` work the queue.

Every local write `fix` applies is logged (finding id, type, subject,
evidence) and persisted as the finding's resolution evidence, so a run's
changes are fully reconstructable from the log or the findings table.

**Materialize** — part of `fix` (enforce mode; advisory never writes). PS-1
findings (the processor bills a subscription OpenRails does not know) are
auto-created locally **only when both halves resolve unambiguously**:
identity through the engine's existing matcher (a single vault/email match —
zero or multiple candidates never guess) and plan through catalog
provider_links (the billable price whose `processors[provider]` entry
carries the remote plan id). A materialized subscription adopts the remote
status and period timestamps, snapshots the product's entitlement spec like a
normal signup, gets the snapshot's latest successful charge backfilled as a
payment, and grants entitlements through the ordinary subscription-sourced
path; the finding resolves as `enforced` with the materialization evidence.
Anything unresolvable stays admin_pending, with the blocker documented on
the finding.

Findings have stable identity across runs (re-runs update, vanished findings
auto-resolve), carry intent evidence ("our recorded delete never executed —
the executor replays it") so the admin queue holds only genuine unknowns, and
include the dunning-forensics report.

**Stuck intents (PS-10).** Every run — regardless of `--provider` filters,
reading only the local ledger — flags provider intents sitting non-terminal
too long: `pending`/`failed_retryable` older than 24h, and
`in_flight`/`unknown_needs_verify` older than 2h (a healthy verifier resolves
unknowns in minutes). Thresholds are hardcoded — no knobs. Intents parked by
operating mode or the kill switch are *informational* (that wait is by
design; the executor drains them when the blocker lifts); everything else
stuck is admin-queued — it means provider failures, bad credentials, or a
dead worker. Findings auto-resolve when the intent completes or recovers;
`fix` never touches intent rows (the executor/verifier own them). NMI safety note: cancelled
subscriptions *vanish* from NMI's recurring report rather than changing
status, so the engine refuses to act when the remote active set is
implausibly small versus local (circuit breaker against mass-cancellation
from a bad fetch).

**Dunning forensics — three evidence sources.** For every examined
subscription (locally past_due, or cancelled-as-expired) the report
cross-references, with each timeline entry tagged by source:

1. **provider** — the processor's own charge-attempt timeline, declines
   included (NMI transaction search, Stripe charges, CCBill exports);
2. **local** — the retry fields on the subscription row (`last_retry_at` /
   `retry_attempts` / `next_retry_at`), preserved verbatim by the legacy
   import;
3. **history** — OpenRails' own ClickHouse analytics events
   (`payment_events` / `subscription_events`), which for migrated merchants
   include the imported legacy history (users_logs rebill attempts,
   mobius_schedulers scheduler events, payment_settings gateway state). This
   is the deep-history source: the provider query APIs will not serve
   years-old declines, the migrated events will — it is what answers "did
   legacy dunning run and when did it die" end to end.

Aggregates report per-source and combined "last dunning action per ANY
source", never-attempted vs attempted-and-exhausted counts, and a
decline-reason histogram. ClickHouse unconfigured or unreachable degrades to
a `history source: …` note in the report — never an error.

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

## Subscription liveness sync (#367) + renewal grace (#368)

Dunning owns `past_due` (we SAW the failure). The **silence cohort** —
`active` subscriptions whose period lapsed with NO webhook either way — now
has an automated owner too: the subscription liveness sync, a 4-hourly
state-scan worker (`RunOnStart=true`, so a reboot after an outage sweeps the
backlog immediately). Per silent subscription it READS provider truth (NMI:
the period's sale transactions by order reference + the recurring record;
Stripe: `GET /v1/subscriptions/{id}` + latest invoice) and converges through
the normal lifecycle services:

| Probe says | Convergence |
|---|---|
| charged | `RenewMembership` repair — payment + entitlements backfilled exactly once, never a second charge |
| declined | `FailMembership` + #359 schedule — **dunning owns it from here** |
| no attempt, remote alive w/ future next billing | adopt the remote period end (clock misalignment); adoption alone grants no access |
| remote absent/terminal | cancel locally + revoke entitlements (no remote delete — it's already gone) |
| unreachable | nothing changes; the next pass re-derives the cohort and retries (no read-queue; the intent ledger stays mutations-only) |

It never charges — charging stays inside dunning, whose derived staleness
window cancels months-stale subscriptions instead of surprise-charging.
Mode gating: runs under `full` AND `limited` (probes are reads, convergence
is local writes — consistent with #366 materialize); skipped under
`readonly`. Each pass logs an alert-style summary
(`cohort/repaired/failed/adopted/cancelled/unreachable`).

While the probe resolves the silence, the user keeps access through the
**renewal grace window** (#368): activation and every renewal pre-append a
trailing `grace` entitlement window `[period_end, period_end + slack)` —
slack = half the billing cycle capped at 48h (daily: 12h), not a knob — for
NMI-backed + Stripe subscriptions. Grace is revoked the moment truth arrives
(renewal success, terminal failure, deliberate cancel — explicit cancels
delete the scheduled grace, access ends at period end as the user expects)
and lapses by its own end_at if silence outlasts the slack. See
docs/entitlements_timeline.md → "Grace".

**What reconcile (#107) remains for:** everything else — the full-surface
manual batch tool (all PS finding types, findings ledger, admin queue,
forensics, enforce mode). The liveness sync is the always-on, narrow,
per-subscription slice for exactly one failure mode (inbound silence); a
reconcile fix run before/after a liveness pass converges to the same state
by idempotency. Note: NMI charge detection correlates by the order reference
OpenRails stamps at signup (the local subscription id). Legacy-imported
subscriptions whose NMI `orderid` predates OpenRails won't match the charge
probe — their charged periods converge as period-adoption (no access
granted) until a webhook or reconcile backfill lands the payment.

## Safety levers (recap — full details in README "Operating modes")

- `mode = full | limited | readonly` — behavior dial. `limited`: nothing
  system-initiated touches a provider; everything user/admin-asked works.
  `readonly`: zero provider writes, wire-enforced on all three rails (NMI
  direct-post, Stripe transport, Solana transaction submission).
- `test_env = true|false` — credential sandbox enforcement, orthogonal to
  mode: sandbox routing + Stripe live-key refusal + the NMI boot probe (one
  auth on the non-issued test card; a decline proves production credentials
  and refuses the boot) + Solana devnet. Probe verdicts cache for 12h in
  `billing.probe_verdicts` (#348), keyed by sha256 of the key: a fresh `live`
  verdict refuses the boot from cache without re-probing (a crash loop costs
  one declined auth total), a fresh `simulated` verdict skips the probe, a
  rotated key or stale verdict re-probes, and cache failures degrade to
  probing. Dev-only.
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
