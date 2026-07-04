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
| 1 | Catalog wrong at the provider | push (OpenRails → provider) | `push-merchant-catalog` — verify-or-create at apply time; alert-only drift watching; provider extras are logged, and archived only under the explicit `--prune` flag (#357) |
| 2 | Money state wrong locally | pull (provider → OpenRails) | webhooks in real time; **reconcile (#107)** as the batch truth-pull: advisory diffs, enforce converges local state |
| 3 | Outbound action never executed | (intent, not sync) | **durable intent + replay** — see "Durability model" below; reconcile is its *detector* |
| 4 | Entitlements inconsistent | derived | re-run the derivation once 1–3 are true; `internal/audit` checks the local derivation, reconcile's PS-9 converges it against trued-up inputs |

## Mutation Flags

Operator commands use the same mutation contract:

- no mutation flags: plan/report only
- `--insert`: create records or provider objects that are missing from the target
- `--overwrite`: update existing target records or mutable provider-owned fields
- `--prune`: disable, archive, delete, or tombstone target extras that are absent from the source

The flags compose. Full reconciliation to the source of truth is
`--insert --overwrite --prune`.

The primary commands are:

```bash
openrails push-auth-bootstrap --config /etc/openrails/config.yaml --file /run/openrails/bootstrap.yaml
openrails push-merchant-config --config /etc/openrails/config.yaml --file /run/openrails/merchants.yaml
openrails push-merchant-catalog --config /etc/openrails/config.yaml --file /run/openrails/catalog.yaml
openrails pull-provider --config /etc/openrails/config.yaml --merchant doujins --provider stripe
```

The `push-*` commands push declared file state into AuthKit root authority,
OpenRails-owned merchant state, the merchant secret backend, or provider catalog
surfaces. `pull-provider` moves the opposite direction: provider observed state
into OpenRails' local mirror, then local convergence. It never mutates external
payment rails.

Merchant provider secrets in `push-merchant-config` are seed material, not the
runtime source of truth. The command resolves each declared provider account
identity `(merchant, rail, environment, account_id)` and imports its
secret values into the same backend the server will read:

- `vault.enabled: true`: `secret/openrails/merchants/<merchant-slug>/rail_merchant_accounts/<rail>/<environment>/<account_id>/<secret_key>`
- Vault disabled: `openrails.merchant_secrets` with the same secret name,
  envelope-encrypted when `encryption.master_key` is configured

After import, runtime checkout, webhook, vault/tokenization, provider intent,
and provider-pull paths use the `provider_accounts` row and scoped secret name;
they do not read a broad merchant-wide `stripe/secret_key` or
`nmi/mobius/security_key` when a DB-backed merchant service is running.

## Private Standalone First Run

On an empty private standalone install, run the file-backed push commands as an
init job or manual operation:

```bash
openrails push-auth-bootstrap --config /etc/openrails/config.yaml --file /run/openrails/bootstrap.yaml
openrails push-merchant-config --config /etc/openrails/config.yaml --file /run/openrails/merchants.yaml --insert
openrails push-merchant-catalog --config /etc/openrails/config.yaml --file /run/openrails/catalog.yaml --insert --overwrite
```

`push-auth-bootstrap` runs first because it creates the initial AuthKit root
operator. Merchant config then creates OpenRails merchant groups and secrets.
Normal server restarts never reconcile merchant config or catalog files. If
`/etc/openrails/bootstrap.yaml` is mounted, startup bootstrap is first-run only
and limited to AuthKit authority.

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
bad credentials — are not errors, they are reasons an intent
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
`stripe_archive_price`, `solana_sunset_plan`; admin-origin):
`push-merchant-catalog --prune` (#357) enqueues+executes an intent per extra,
so a provider
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
`--provider`, `--type`, `--merchant`, `--format table|json`) lists the queued
outbound mutations read-only. Under
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

**Provider account guard / credential rotation (#518).** Provider-owned work is
bound to a merchant-scoped `payment_provider_accounts` row. The row stores the account's
own identity, never the credential or local config key: NMI uses the merchant
identity from the gateway's profile report (query.php `report_type=profile`),
Stripe uses the `acct_...` id from GET /v1/account, CCBill uses the configured
dash-joined `clientAccnum-clientSubacc` identity (#697), and Solana uses the
configured recipient/authority
identity where account-binding is useful. Local config keys such as
`stripe_live` are disposable selectors only.

Every provider intent is stamped at enqueue with the `provider_account_id` row
for the provider account the producer was configured against. The executor and
verifier compare that row against the current credentials and park/defer when no
current credential resolves to the same account — a queue built against one
account is never executed against another (NMI ids are small numerics that
collide across accounts: the wrong account's subscription would be deleted, the
wrong customer's card charged). Checkout session creation also resolves the
current non-archived provider account and stamps `checkout_sessions.provider_account_id`
before creating the provider checkout row.
Operational rules:

- Key rotation within the SAME account: no effect — the provider account identity
  is refetched under the new key and matches.
- Pointing a provider key at a DIFFERENT account for default work is an account
  rotation: OpenRails binds the new account as non-archived and leaves old
  provider-owned rows attributed to the old account. New default checkout/pull
  work uses a non-archived account; old accounts can remain archived for drain.
- Pending intents that were stamped with the OLD account do not follow that
  default rotation automatically. Restore credentials for the old account so they
  can drain, let stale intents expire/supersede, or — if deliberately adopting
  the new account for those old intents — rebind with
  `openrails intents rebind-account --provider=<name> --merchant=<slug> --yes`
  after verifying the provider-side operation is safe to run against the current
  account.
- A failed account-identity fetch (provider down) skips the guard for that pass
  (warn logged) rather than blocking the ledger.
- Legacy intents without `provider_account_id` execute ungated.

## Provider Pull (#107, #511)

Manual-only — **never scheduled**. It never writes to a provider:

- `openrails pull-provider` with no mutation flags: pull provider truth, diff,
  print what would change, and persist no local mirror changes.
- `openrails pull-provider --insert`: import provider-observed records missing
  locally.
- `openrails pull-provider --overwrite`: update existing local mirror rows from
  provider truth.
- `openrails pull-provider --prune`: delete eligible local subscriptions or
  payments attributed to the pulled provider account that are absent from the
  provider source.
- `openrails intents-log`: render append-only external provider mutation
  attempts/results created by the provider-intent executor. This is the
  opposite direction from `pull-provider`: it records remote provider writes,
  not local mirror corrections.
- `openrails reconcile report [--run=ID]`: render the latest (or given) run's
  summary, dunning forensics, and standing open findings.

`pull-provider` takes `--provider nmi|ccbill|stripe|solana` (repeatable;
default = every configured provider), `--provider-account` (optional explicit
provider account row id), `--since` / `--until` (RFC3339 or `YYYY-MM-DD`,
bounding the transaction window), `--merchant` (slug or id; required), and
`--format table|json`.

Every local write `fix` applies is logged (finding id, type, subject,
evidence) and persisted as the finding's resolution evidence, so a run's
changes are fully reconstructable from the log or the findings table.

Before a provider's data is treated as authoritative, `check` / `fix` resolves
the configured provider key to a provider account identity and verifies it
against the targeted `payment_provider_accounts` row. A changed
credential that points at a different Stripe/NMI/CCBill/Solana account
aborts the provider run before local mirror rows are inserted or overwritten.
The run summary and provider fetch params carry the local `provider_account_id`;
local mirror reads/writes are scoped to that row. Historical NULL
`provider_account_id` rows are ambiguous import state and are not used as proof
for destructive absence handling.

**Materialize** — part of `fix` (enforce mode; advisory never writes). PS-1
findings (the rail bills a subscription OpenRails does not know) are
auto-created locally **only when both halves resolve unambiguously**:
identity through the engine's existing matcher (a single vault/email match —
zero or multiple candidates never guess) and plan through catalog
provider_links (the billable price whose `rails[provider]` entry
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
operating mode are *informational* (that wait is by design; the executor drains
them when the blocker lifts); everything else
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

1. **provider** — the rail's own charge-attempt timeline, declines
   included (NMI transaction search, Stripe charges, CCBill exports);
2. **local** — the retry fields on the subscription row (`last_retry_at` /
   `retry_attempts` / `next_retry_at`), preserved verbatim by the legacy
   import;
3. **history** — Postgres history (#735): failed-payment rows plus, for
   migrated merchants, the imported legacy dunning history
   (`imported_dunning_history`: users_logs rebill attempts and
   mobius_schedulers scheduler events). This is the deep-history source:
   the provider query APIs will not serve years-old declines, the imported
   events will — it is what answers "did legacy dunning run and when did it
   die" end to end.

Aggregates report per-source and combined "last dunning action per ANY
source", never-attempted vs attempted-and-exhausted counts, and a
decline-reason histogram. An unavailable history source degrades to a
`history source: …` note in the report — never an error.

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
revoke entitlements + scheduled rail-side delete through the one shared
deferred-delete mechanism.

(Benchmark: Stripe's own default for monthly billing is 8 ML-timed retries
over 2 weeks — same span, more attempts; ours is sparser because each NMI
decline costs a per-transaction fee. Stripe-billed subscriptions use Stripe's
dunning; this section governs NMI-backed manual dunning.)

## Provider Refresh (#574), the unknown-cohort reconcile (#632/#665), and renewal grace (#368)

`pull-provider` is the manual full-batch/operator command. **Provider
Refresh** is the always-on provider-read system: a 4-hourly River worker
(`openrails.provider_refresh`, `RunOnStart=true`, so startup after a stale dump
or outage does not wait for the first 4-hour tick). It has three lanes:

| Lane | Purpose |
|---|---|
| Provider Event Refresh | bounded missed-event backfill for NMI, Stripe, and CCBill using durable per-merchant/provider/account/domain watermarks |
| Unknown-cohort Reconcile | resolves `unknown` subscriptions against provider truth: one windowed bulk pull per rail + targeted per-subscription probes for rows the bulk pull can't decide |
| CCBill DataLink Refresh | scheduled DataLink active-member bulk refresh, because CCBill has no cheap per-subscription liveness API |

Provider Event Refresh advances a watermark only after the provider/window
completes successfully. Provider errors, missing credentials, account mismatch,
pagination failure, or partial reads leave the watermark unchanged and the next
scheduled/startup pass retries the same bounded window. It writes only local
truth through the existing idempotent reconciliation writers, then runs scoped
convergence so entitlements/grants/derived state follow the refreshed provider
facts. It never mutates a provider.

Dunning owns `past_due` (we SAW the failure). The **silence cohort** — lapsed
subscriptions with NO webhook either way — is parked as `unknown` by the LIFE
convergence plane (#664) and resolved by the Unknown-cohort Reconcile lane. It
READS provider truth (one bulk snapshot per rail; per-subscription probes —
NMI: the period's sale transactions by order reference + the recurring record;
Stripe: `GET /v1/subscriptions/{id}` + latest invoice — only when the bulk pull
can't cover a row, e.g. NULL period end) and applies ONE verdict set:

| Provider evidence | Resolution |
|---|---|
| verified renewal charge | renewed — period advanced, the charge backfilled exactly once, never a second charge |
| declined / roster stalled, within dunning window | `past_due` — **dunning owns it from here** |
| declined / stalled, beyond the window | cancelled; the remote record may still exist, so the deferred NMI delete is queued (#679) |
| no charge, remote alive w/ future next billing | adopt the remote period end (clock misalignment); adoption alone grants no access |
| remote absent/terminal | cancel locally + revoke entitlements (no remote delete — it's already gone) |
| no conclusive evidence / unreachable | stays `unknown`; the next pass re-derives the cohort and retries (no read-queue; the intent ledger stays mutations-only) |

It never charges — charging stays inside dunning, whose derived staleness
window cancels months-stale subscriptions instead of surprise-charging.
Mode gating: Provider Refresh runs under `full` AND `limited` (provider reads
plus local convergence — consistent with #366 materialize); skipped under
`readonly`. Each pass logs one Provider Refresh heartbeat across lanes plus a
per-merchant unknown-reconcile summary (`renewed/adopted/past_due/cancelled/
still_unknown/probed/backfilled/rail_errors`).

While the probe resolves the silence, the user keeps access through the
**renewal grace window** (#368): activation and every renewal pre-append a
trailing `grace` entitlement window `[period_end, period_end + slack)` —
slack = half the billing cycle capped at 48h (daily: 12h), not a knob — for
NMI-backed + Stripe subscriptions. Grace is revoked the moment truth arrives
(renewal success, terminal failure, deliberate cancel — explicit cancels
delete the scheduled grace, access ends at period end as the user expects)
and lapses by its own end_at if silence outlasts the slack. See
docs/entitlements_timeline.md → "Grace".

**What reconcile (#107) remains for:** the full-surface operator tool (all PS
finding types, findings ledger, admin queue, forensics, manual enforce mode).
Provider Refresh reuses the same read-only fetchers and idempotent local writers
for routine catch-up, but does not replace manual investigation. A
`pull-provider --insert --overwrite` run before/after Provider Refresh converges
to the same state by idempotency. Note: NMI charge detection correlates by the
order reference OpenRails stamps at signup (the local subscription id).
Legacy-imported subscriptions whose NMI `orderid` predates OpenRails won't
match the per-subscription charge probe; the Provider Event Refresh lane is
what backfills those missed provider events from the durable watermark.

## Safety levers (recap — full details in README "Operating modes")

- `provider_write_mode = full | limited | readonly` — behavior dial. `limited`: nothing
  system-initiated touches a provider; everything user/admin-asked works.
  `readonly`: zero provider writes, wire-enforced on all three rails (NMI
  direct-post, Stripe transport, Solana transaction submission).
- `test_mode = true|false` — credential sandbox enforcement, orthogonal to
  provider_write_mode: sandbox routing + Stripe live-key refusal + the NMI boot probe (one
  auth on the non-issued test card; a decline proves production credentials
  and refuses the boot) + Solana devnet. Probe verdicts cache for 12h in
  `billing.probe_verdicts` (#348), keyed by sha256 of the key: a fresh `live`
  verdict refuses the boot from cache without re-probing (a crash loop costs
  one declined auth total), a fresh `simulated` verdict skips the probe, a
  rotated key or stale verdict re-probes, and cache failures degrade to
  probing. Dev-only.
**Cutover posture** (migration/reconciliation against production
credentials): use `PROVIDER_WRITE_MODE=limited` when OpenRails should keep serving reactive
customer/admin flows while system-origin provider writes stay parked, or
`PROVIDER_WRITE_MODE=readonly` for strict observation. Exit by moving to
`PROVIDER_WRITE_MODE=full`. All
paused work is delayed,
not lost; missed billing periods are never back-billed.
