# OpenRails Operations Manual

The deep day-2 reference: how OpenRails stays consistent with the payment
rails, what to run when it isn't, and the safety levers for doing any of this
against production credentials. [operator-guide.md](operator-guide.md) is the
orientation layer over this manual.

## The ownership model

Three facts decide every consistency mechanism: **OpenRails owns the
catalog** (products + prices; providers hold copies); **the provider owns
money state** (is a subscription alive, what was charged; OpenRails holds
copies); **OpenRails owns entitlements — but they are derived**,
deterministically, from catalog + money state + admin grants. So there are
exactly four ways the system diverges, each with its own mechanism:

| # | Divergence | Direction | Mechanism |
|---|---|---|---|
| 1 | Catalog wrong at the provider | push (OpenRails → provider) | `push-merchant-catalog` — verify-or-create at apply; alert-only scheduled drift watching (`catalog_reconciliation_interval`, default 1h, `0` disables); provider extras archived only under `--prune` |
| 2 | Money state wrong locally | pull (provider → OpenRails) | webhooks in real time; **Provider Refresh** as the always-on scheduled read; **`pull-provider`** as the manual batch truth-pull |
| 3 | Outbound action never executed | (intent, not sync) | **durable intent + replay** — see "Durability model"; the Convergence Engine's stuck-intent check is its detector |
| 4 | Entitlements inconsistent | derived | the **Convergence Engine** re-derives them once 1–3 are true |

## Mutation Flags

Operator commands share one mutation contract:

- no mutation flags: plan/report only
- `--insert`: create records or provider objects missing from the target
- `--overwrite`: update existing target records or mutable provider-owned fields
- `--prune`: disable, archive, delete, or tombstone target extras absent from the source

The flags compose; full reconciliation to the source of truth is
`--insert --overwrite --prune`.

### CLI inventory

Global flags on every command: `--config/-c` (default `config.yaml`),
`--provider-write-mode`, `--test-mode` (flag beats env beats yaml).

| Command | Purpose |
|---|---|
| `run-server [--no-workers]` / `run-worker` | serve the public API (+ workers unless disabled) / workers only |
| `migrate up` / `migrate pg` | apply all migrations / Postgres-only (River + OpenRails) |
| `push-auth-bootstrap [--file] [--dry-run] [--startup-only --name]` | push AuthKit root authority from a bootstrap manifest |
| `push-merchant-config [--file] [mutation flags]` | push merchant groups + PSP declarations + secrets from `merchants.yaml` |
| `push-merchant-catalog [--file] [mutation flags]` | terraform-style catalog apply (OpenRails rows + provider objects) |
| `dump-merchant-config --slug [--out] [--include-secrets]` / `dump-merchant-catalog --slug` | export a merchant's config / catalog manifest |
| `pull-provider` / `pull-provider report` | manual provider truth-pull / run report — see "Provider Pull" |
| `intents` / `intents-log` | read-only intent-ledger views — see "Inspecting the ledger" |

The `push-*` commands push declared file state outward; `pull-provider` moves
the opposite direction and never mutates a payment rail.

### Merchant secrets

PSP secrets in `push-merchant-config` are seed material, not the runtime
source of truth. The manifest key is `merchants.<slug>.psps.<psp-key>.<rail>`
(env overlay `BILLING_MERCHANTS_<MERCHANT>_PSPS_…`); the retired anchors
(`accounts`, `rail_merchant_accounts`, `provider_accounts`) fail loudly with
a rename error. The command imports each PSP's secrets under the canonical
scoped name `psps/<rail>/<environment>/<account_id>/<secret_key>` into the
backend the server reads (`secret_backend: db | vault`): Vault KV-v2 path
`<mount>/openrails/merchants/<merchant-slug>/<name>`, or
`openrails.merchant_secrets` envelope-encrypted under
`encryption.master_key` / `ENCRYPTION_MASTER_KEY`. Runtime checkout,
webhooks, tokenization, provider intents, and pulls all arm per-PSP from that
scoped name.

## Private Standalone First Run

On an empty private standalone install, run the file-backed push commands as
an init job or manual operation:

```bash
openrails push-auth-bootstrap --config /etc/openrails/config.yaml --file /run/openrails/bootstrap.yaml
openrails push-merchant-config --config /etc/openrails/config.yaml --file /run/openrails/merchants.yaml --insert
openrails push-merchant-catalog --config /etc/openrails/config.yaml --file /run/openrails/catalog.yaml --insert --overwrite
```

`push-auth-bootstrap` runs first because it creates the initial AuthKit root
operator; merchant config then creates OpenRails merchant groups, PSP rows,
and secrets. Normal server restarts never reconcile merchant config or
catalog files. If `/etc/openrails/bootstrap.yaml` is mounted, startup
bootstrap is first-run only and limited to AuthKit authority.

## Durability model

**Outbound — durability is OUR job.** Every mutation OpenRails wants to make
against a provider must survive failure of the attempt. The mechanism is the
**provider intent ledger** (`openrails.rail_intents`): every outbound
mutation is durably recorded with an idempotency key (re-enqueues dedupe), an
origin (`user`/`admin`/`system`), the PSP row it was produced against, and a
relevance window. Two scheduled workers drain it: the **executor** (every
minute, and on startup) claims due intents under a SKIP LOCKED lease, checks
relevance, gates on operating mode × origin, executes, and classifies the
outcome; the **verifier** (every 5 minutes) resolves `unknown_needs_verify`
intents via provider READS before any retry.

Statuses: `pending`, `in_flight`, `succeeded`, `failed_retryable`,
`failed_terminal`, `unknown_needs_verify`, `superseded`, `expired`. Failure
reasons — provider down, `readonly`/`limited` mode, unarmable credentials —
are not errors; they are reasons an intent stays pending, recorded on the
row. When the blocker lifts, the queue drains. Intents that outlive their
relevance window (a delete after the subscription resumed; a rebill past the
dunning window) are superseded or expire instead of firing stale. The mode ×
origin gate: nothing writes under `readonly`; `limited` blocks system-origin
intents (dunning charges, proactive deletes) while user/admin intents
execute; `full` executes everything; an unrecognized origin parks.

Every outbound provider mutation flows through the ledger: deferred NMI
deletes, NMI/Stripe/CCBill refunds, dunning `manual_rebill` charges, CCBill
cancels, catalog archive ops (`stripe_archive_product`/`stripe_archive_price`/
`solana_sunset_plan`), payment-method swaps, vault deletes, checkout NMI
sales, auto-top-up charges, and Solana recurring pulls. NMI deliberately has
NO catalog-archive write path (plan edits affect live subscribers).

Execution is **effectively-once**, never assumed exactly-once. Per class:
money-movers park ambiguous outcomes as `unknown_needs_verify` and are
resolved by *reading* the provider before any retry — a charge is never
blind-retried; deletes/cancels are verify-then-execute (already-deleted =
success); creates are content-addressed find-or-create. Stripe ops
additionally send `Idempotency-Key`. Every attempt/outcome is appended to
`openrails.rail_mutation_logs`.

**Inbound — durability is the PROVIDER's job.** NMI, CCBill and Stripe
deliver webhooks at-least-once and retry from their end; our handlers are
idempotent for exactly that reason. **There is deliberately no local inbound
queue** — it would share fate with the database it protects. The backstop for
an outage that exhausts the provider's webhook retries is Provider Refresh's
watermarked event backfill (and, for investigation, `pull-provider`).

### Inspecting the ledger

`openrails intents [--status=…] [--rail=…] [--type=…] [--merchant=…]
[--format table|json] [--limit N]` lists the queued outbound mutations
read-only. The default `--status=active` view is the live working set
(`pending`, `in_flight`, `failed_retryable`, `unknown`); `succeeded`,
`failed`, `superseded`, `expired`, and `all` are queryable explicitly. Each
row reports `executes_under` (derived from its origin) and the footer prints
the drain forecast: "N execute under mode=limited (or full), M require
mode=full; nothing executes under readonly." Under `limited`/`readonly` this
doubles as the dry-run view of a cutover.

`openrails intents-log [--rail=…] [--intent=…] [--provider-account=…]
[--phase=attempting|succeeded|failed|unknown|parked]` renders the append-only
mutation-attempt log — the executor's audit trail.

### Materialized backlog under mode=limited (#366)

The dunning worker's scan runs under `limited` and records its decisions
instead of skipping: window-expired `past_due` subscriptions (a freshly
migrated backlog's bulk) get the local no-charge cancel + downgrade
immediately, and in-window charges enqueue as PARKED system-origin
`manual_rebill` intents — bounded by `expires_at` = the dunning window, so
one can never fire stale after the mode lifts; the handler re-checks
relevance (still past_due, same period) at execution. Materialize never
claims the subscription (claiming writes `last_retry_at`, which is
dunning-forensics evidence imported from legacy) and never applies failure
policy. `readonly` is unchanged: pure dry-run observer.

### PSP binding and credential rotation

`openrails.psps` is an **operator-declared** catalog: one row per merchant
PSP account on a rail, with an opaque declared `account_id`. There is NO
runtime "whoami"/identity resolution — OpenRails never fetches or verifies
the account identity behind a credential; the declaration is trusted.

Every provider intent is stamped at enqueue with the `psp_id` it was produced
against, and the executor/verifier arm the rail client for **that** PSP row
from its scoped secrets at drain time. A PSP whose credentials cannot be
armed fails closed: its intents park (never execute against a different
account) until the credentials return or the intent expires/supersedes.
Rules:

- **Rotating a credential within the SAME provider account**: replace the
  secret under the same PSP row — intents arm with the new value
  transparently.
- **Moving to a DIFFERENT provider account**: never repoint an existing PSP
  row's credentials (OpenRails cannot detect the swap — the declared
  `account_id` would silently lie). Declare a NEW `psps` entry and archive
  the old one; `archived` is drain-only — no new checkout/pull work selects
  it, but it remains addressable for existing obligations and inbound events.
- **Pending intents stamped with the old PSP do not follow** a credential
  move: keep (or restore) the old PSP's credentials until its queue drains,
  or let stale intents expire/supersede via their relevance windows. There is
  no rebind command.

## Provider Pull (#107, #511)

Manual-only — **never scheduled**. It never writes to a provider.

```
openrails pull-provider --merchant=<slug> [--rail=nmi,stripe,…] [--provider-account=<uuid>]
                        [--since=… --until=…] [--manifest=…] [--format table|json]
                        [--log-dir=…] [--insert] [--overwrite] [--prune]
openrails pull-provider report --merchant=<slug> [--run=ID] [--format table|json]
```

A bare `pull-provider` pulls provider truth, diffs, logs what it WOULD
change, and persists nothing; the mutation flags follow the standard contract
(`--prune` deletes eligible local subscriptions/payments attributed to the
pulled PSP that are absent from the provider source). `--rail` is repeatable
(default: every configured rail); `--merchant` is required; `--manifest` arms
mode-1 credentials from a merchant manifest. After a mutating pull the engine
runs a one-shot `Converge(merchant)`.

A pull is authoritative only for the `(merchant, rail, psp)` it actually
queried; mirror reads/writes are scoped to that PSP row, and historical rows
with NULL PSP attribution are never used as proof for destructive absence
handling. NMI safety: cancelled subscriptions *vanish* from NMI's recurring
report rather than changing status, so a circuit breaker refuses
absence-based conclusions when the remote active set is implausibly small
versus local (protection against mass-cancellation from a bad fetch). Every
local write a mutating run applies is logged (finding id, type, subject,
evidence) and persisted as the finding's resolution evidence, so a run is
fully reconstructable.

**Materialize.** `pull.subscription.missing` findings (the rail bills a
subscription OpenRails does not know) are auto-created locally **only when
both halves resolve unambiguously**: identity through the engine's matcher (a
single vault/email match — zero or multiple candidates never guess) and plan
through the catalog's PSP links. A materialized subscription adopts the
remote status and period, snapshots the product's entitlement spec like a
normal signup, gets the latest successful charge backfilled as a payment, and
grants entitlements through the ordinary path. Anything unresolvable stays in
the admin queue with the blocker documented.

**Dunning forensics — three evidence sources**, each timeline entry tagged:
**provider** (the rail's own charge-attempt timeline, declines included —
NMI transaction search, Stripe charges, CCBill exports); **local** (the
retry fields on the subscription row — `last_retry_at` / `retry_attempts` /
`next_retry_at` — preserved verbatim by legacy import); **history**
(failed-payment rows plus, for migrated merchants,
`openrails.imported_dunning_history` — the deep-history source the provider
query APIs will not serve). Aggregates report per-source and combined
last-action, never-attempted vs attempted-and-exhausted counts, and a
decline-reason histogram; an unavailable history source degrades to a note,
never an error.

## The Convergence Engine

The continuous internal-repair half of the system (the pull feeds it). One
idempotent **`Converge(scope)`** — scope-narrowable from a single customer to
a whole merchant — runs from three places so the system never *holds* an
inconsistency: **inline** after every source mutation (checkout completes,
renewal bills, refund webhook lands, dunning transitions, admin grants);
**after every mutating `pull-provider` run**; and **on the sweep** — a
15-minute River job (RunOnStart) over every active merchant, catching drift
no inline mutation touched.

A clean scope is **zero writes** — the second run of anything is a no-op. The
engine is the single writer of grants and grant effects, so each invariant
has exactly one implementation. Doctrine, in brief: repairs **converge to the
correct current state, never replay side effects** (three months of missed
dunning becomes one terminal transition, not three charge attempts); grant
effects (entitlements, credits, access) ARE replayable and are reconstructed
anchored to source-event time; destructive repairs are gated — see below.

### The finding taxonomy — four planes

Every finding gets one self-describing qualified type,
`<plane>.<subject>.<shape-or-condition>`, stored in
`openrails.reconciliation_findings.finding_type`:

| Plane | The fact checked | Authority | Repaired by |
|---|---|---|---|
| `pull.*` | local mirror row (charge, subscription, refund, dispute, vault) | the provider | the pull overwrites local |
| `derive.*` | a grant effect (entitlement / credit / access) vs its source event | the source ledger, via the grants layer | replay / retract the grant effect |
| `life.*` | a record's state vs where the clock + state machine say it should be | time | converge the record forward |
| `consistency.*` | duplicates, amount mismatches, unresolved references | the internal consistency condition | fix the data / surface review |

Each finding also has a **shape** (`missing` → materialize, `excess` →
retract, `mismatch` → adjust) and a **remediation class**: AUTO (idempotent
local write, applied immediately), ADMIN (queued for human approval),
OPERATOR (surfaced with a runbook; never auto-fires). Representative types:
`pull.charge.missing`, `pull.subscription.duplicate`, `derive.grant.missing`,
`life.subscription.period_overdue`, `life.provider_intent.stuck`,
`consistency.duplicate.provider_charge`.

Finding states: `reconcile_required` (open, engine may still converge it —
including *held* destructive repairs), `requires_review` (the ADMIN/OPERATOR
queue), `auto_fixed`, `fixed` (operator-acked or the drift vanished),
`ignored` (silenced identity; re-runs refresh but never reopen). Findings
have stable identity across runs and auto-resolve when the divergence
disappears. Two safety doctrines matter operationally:

- **Confirmed-absence gate**: `excess`/retract repairs (revoking access,
  cancelling) do not auto-fire until the relevant source domain
  (subscriptions / payments / grants) is marked *fully reconciled* for the
  merchant — flipped automatically after a completed mutating pull whose
  fetcher proved exhaustive coverage across every declared PSP
  (`openrails.reconciliation_state`, a ratchet). During an import, "not in
  the local DB" is not absence — it usually means *not imported yet*. Held
  repairs stay `reconcile_required` with the unproven domain in evidence.
- **Stuck intents** (`life.provider_intent.stuck`): the sweep flags rail
  intents sitting non-terminal too long — `pending`/`failed_retryable` older
  than 24h, `in_flight`/`unknown_needs_verify` older than 2h; thresholds are
  hardcoded. Mode-parked intents are informational (that wait is by design);
  everything else is `requires_review` — provider failures, bad credentials,
  or a dead worker. The engine never touches intent rows (the
  executor/verifier own them); findings auto-resolve on recovery.

## Dunning (#359)

No knobs. The schedule is a hardcoded function of the price's billing cycle —
the retry span always stays well inside one cycle. Retries are OFFSETS from
the initial failure (progressive, front-loading where transient declines
clear):

| Cycle | Retry offsets | Failures to terminal | Derived staleness window |
|---|---|---|---|
| < 4 days | none | 1 (first failure is terminal) | 0 |
| 4–27 days ("weekly") | +1d, +2d | 3 | 3 days |
| ≥ 28 days ("monthly+", capped) | +2d, +5d, +9d, +13d | 5 | 14 days |

An unknown cycle (one-time price in a `past_due` state that shouldn't exist)
falls back to the monthly schedule defensively, logged. **Hard declines**
(stolen/lost card, do-not-honor, expired card, "stop recurring" codes) are
terminal immediately regardless of schedule — retrying cannot succeed and
risks card-network flags; soft declines (insufficient funds, comms errors,
merchant-config errors) follow the schedule. The staleness window ("never
charge a months-old failure") derives from the same schedule — last offset +
24h slack — so it cannot be misconfigured; anything older is cancelled +
downgraded WITHOUT a charge. Terminal failure = cancel + revoke entitlements
+ rail-side delete via the intent ledger's deferred-delete mechanism.

(Stripe-billed subscriptions use Stripe's own dunning; this section governs
NMI-backed manual dunning. Ours is sparser than Stripe's 8-retry default
because each NMI decline costs a per-transaction fee.)

## Provider Refresh (#574) and the unknown cohort (#632/#664/#665)

`pull-provider` is the manual operator command. **Provider Refresh** is the
always-on provider-read system: a 4-hourly scheduler (RunOnStart, so startup
after a stale dump or outage does not wait for the tick) fans out one
per-merchant refresh job — staggered, unique per merchant, on a bounded
queue, skipping merchants with no declared PSPs (#719). Three lanes:

| Lane | Purpose |
|---|---|
| Provider Event Refresh | bounded missed-event backfill for NMI, Stripe, CCBill using durable per-merchant/rail/account/domain watermarks (`openrails.rail_refresh_watermarks`) |
| Unknown-cohort Reconcile | resolves `unknown` subscriptions against provider truth: one windowed bulk pull per rail + targeted per-subscription probes for rows the bulk pull can't decide |
| CCBill DataLink Refresh | scheduled active-member bulk refresh (CCBill has no cheap per-subscription liveness API) |

A watermark advances only after its provider/window completes successfully;
errors, missing credentials, or partial reads leave it unchanged and the next
pass retries the same bounded window. Refresh writes only local truth through
the same idempotent reconciliation writers the pull uses, then runs scoped
convergence. It never mutates a provider and **never charges** — charging
stays inside dunning. Dunning owns `past_due` (we SAW the failure); the
**silence cohort** — lapsed subscriptions with NO webhook either way — is
parked as `unknown` by the LIFE plane and resolved by the Unknown-cohort lane
against ONE verdict set:

| Provider evidence | Resolution |
|---|---|
| verified renewal charge | renewed — period advanced, the charge backfilled exactly once, never a second charge |
| declined / roster stalled, within the dunning window | `past_due` — dunning owns it from here |
| declined / stalled, beyond the window | cancelled; the remote record may still exist, so the deferred rail-side delete is queued |
| no charge, remote alive with future next-billing | adopt the remote period end (clock misalignment) |
| remote absent/terminal | cancel locally + revoke entitlements (no remote delete — it's already gone) |
| no conclusive evidence / rail unreachable | stays `unknown`; the next pass re-derives the cohort and retries |

Mode gating: Provider Refresh runs under `full` AND `limited`; skipped under
`readonly`. Each pass logs a heartbeat plus a per-merchant summary
(`renewed/adopted/past_due/cancelled/still_unknown/probed/backfilled/rail_errors`).

**Access during silence — standing access (#691).** There is no timed grace
window (the #368 trailing-grace mechanism was deleted). An auto-renew
subscription's entitlement window is **standing (open-ended) from creation**
and closes only on proven events: terminal dunning failure,
provider-confirmed death, or an explicit cancel (access then ends at period
end, as the user expects). A subscription parked `unknown` keeps access —
entitlements are never lost to our own uncertainty.

Note: NMI charge detection correlates by the order reference OpenRails stamps
at signup; legacy-imported subscriptions whose NMI `orderid` predates
OpenRails won't match the per-subscription probe — the Event Refresh lane's
watermarked backfill catches their provider events.

## Background worker schedule

Everything runs by itself under River once `run-server` (or `run-worker`) is
up. "start" = RunOnStart.

| Worker | Cadence |
|---|---|
| Provider-intent executor | 1 min + start |
| Provider-intent verifier · admission-denial flush · worker health check (health check + start) | 5 min |
| Notification email sweep | 10 min |
| Convergence sweep (+ start) · auto-top-up · alert evaluation · findings digest | 15 min |
| Credit-ledger reconcile (alert-only) | 30 min |
| Plan-migration re-driver (+ start) · cleanup · credit expiry · Solana crank · Stripe webhook reconcile · invoice collection | 1 h |
| Dunning · Provider Refresh scheduler (+ start; fans out per-merchant jobs) | 4 h |
| Solana gas alert · Solana ledger reconcile | 6 h |
| Catalog reconciliation pull (alert-only) | `catalog_reconciliation_interval` (default 1h; `0` disables) |
| Invoice period finalize / monthly-floor sweep | daily / 30 d |

The health checker seeds `openrails.worker_health` and raises durable repair
alerts when a periodic kind stops completing.

### Health endpoints

`GET /health/live` (liveness) and `GET /health/ready` (readiness;
`?verbose=1` adds per-dependency detail), with K8s aliases `/healthz` /
`/readyz`. There is no `/health`. Embedded hosts wire the same checks into
their own handler.

## Operating modes (the safety levers)

Two orthogonal settings:

- **`provider_write_mode`** (yaml) / `PROVIDER_WRITE_MODE` (env) /
  `--provider-write-mode` (CLI flag; flag beats env beats yaml) — the pure
  **behavior** dial: how much OpenRails may do against the payment rails. One
  of `full | limited | readonly`. Required outside development — the boot
  refuses without an explicit value, checked on the RAW setting. Unset
  **fail-closes to `readonly`** everywhere the mode is consulted (including
  development): forgetting the knob can never mean full behavior. The old
  `mode` / `MODE` / `--mode` alias is removed (#710) — a set key fails loudly.
- **`test_mode`** (yaml) / `TEST_MODE` (env) / `--test-mode` (CLI flag) — the
  **credential** axis: `sandbox | live`, no other values. `config.Load`
  defaults to sandbox in development-like boots and live otherwise; embedded
  hosts build `Config` programmatically and must set it explicitly or
  construction refuses to boot (#745).

What each provider write mode permits (`test_mode` applies orthogonally: with
sandbox the same matrix holds against sandbox rails, so no real money can move
in any mode):

| Operation | `full` | `limited` | `readonly` |
|---|---|---|---|
| User checkout / charge | yes | yes | no — fails loudly (`ErrProviderReadOnly`) |
| Card/vault save, tier change, resume, refund | yes | yes | no |
| User/admin cancel → rail-side delete | yes | yes | no — intent parks for replay |
| Dunning charges + window-expiry cancellations | yes | no — runs dry, intents park | no |
| Auto-top-ups, arrears collection, Solana pulls | yes | no | no |
| Catalog provider-object writes (`push-merchant-catalog`) | yes | deferred | deferred |
| Provider reads (query APIs, catalog verification) | yes | yes | yes |
| Webhook ingestion + local serving | yes | yes | yes |

`limited` draws the line at *who initiates* (the system initiates nothing;
humans get everything), `readonly` at *the wire* (nothing writes to a
provider, not even a customer clicking buy — enforced at the transport on
every rail: NMI direct-post gate, Stripe transport gate, CCBill DataLink
read-only flag, Solana submission gate). Typical uses: `limited` = migration
cutover with the site fully usable; `readonly` = reconciliation/forensics
boots that must only observe.

### `test_mode` — sandbox credentials

`test_mode = sandbox` is sandbox money with whatever behavior
`provider_write_mode` selects: every rail routes to its test environment, and
credential guarantees attach — a live Stripe key (`sk_live_`/`rk_live_`)
refuses to boot; each NMI account is probed when armed with one auth on the
canonical non-issued test PAN (only a simulator approves it — a decline
proves a live account and refuses the arm); CCBill uses the sandbox API host;
Solana derives devnet. NMI probe verdicts cache for 12h in
`openrails.probe_verdicts`, keyed by sha256 of the key: a fresh `live`
verdict refuses from cache without re-probing (a crash loop costs one
declined auth total), a fresh `simulated` verdict skips the probe, a rotated
key or stale verdict re-probes, and cache failures degrade to probing.
Sandbox is allowed in every environment (#762) — what keeps it honest is
rail-credential validation (the live-key refusal, plus each PSP's declared
`environment` cross-checked against `test_mode`), not the environment string.

### Cutover: booting against production credentials

Set the mode **before first start** — imported stale `past_due`
subscriptions are immediately "due" and full-behavior modes would start
charging them within hours: `PROVIDER_WRITE_MODE=limited` (site fully
usable, system-origin writes parked), or `readonly` for a strictly-observing
boot.

Before raising the mode to `full`, check the two places deferred work
accumulates:

1. **The provider intent ledger** — fires automatically when the mode is
   raised. `openrails intents --merchant=<slug>` shows pending rows + the
   drain forecast ("N execute under limited, M require full"). If the
   forecast shows something you do NOT want to fire, resolve it first.
2. **The admin findings queue** — never fires automatically.
   `openrails pull-provider report --merchant=<slug>` shows findings
   requiring a human; raising the mode does nothing to this queue by design.

The sequence: boot `limited` → the first dunning cycle materializes the
backlog → `openrails intents` shows the real drain forecast → review (and
fix PSP declarations if credentials moved — see "PSP binding and credential
rotation") → `PROVIDER_WRITE_MODE=full` drains exactly what you saw. Paused
work is delayed, not lost; the workers are state-scan loops, so the first
enabled run processes whatever is outstanding. Missed billing periods are
never back-billed: dunning past the staleness window cancels instead of
charging, and a Solana subscription that skipped whole periods gets exactly
one pull anchored at the pull moment.

## The destructive-action kill switch (#836) and first-enforce gate (#835)

`provider_write_mode` is a boot setting: changing it needs a deploy. The kill
switch is the runtime brake — a single DB row, read at the top of every
destructive plane (converge sweep, provider refresh, intent executor), so one
`UPDATE` halts every node at its next gate check.

**It ships OFF.** A fresh deployment converges nothing destructive — no local
cancellation, no entitlement revocation, no provider delete — until an operator
arms it. That is deliberate: the first pass against an imported legacy book is
exactly when a bad roster does the most damage.

### Stop everything, now

```sql
UPDATE openrails.destructive_action_switch SET enabled = false,
       updated_by = 'you', reason = 'incident: mass cancellation observed';
```

No restart, no deploy, no scaling workers to zero. In-flight destructive intents
**park** (they are not failed), so flipping it back resumes them where they
stopped.

### Confirm it stopped

```sql
-- 1. the switch itself
SELECT enabled, updated_by, reason, updated_at FROM openrails.destructive_action_switch;

-- 2. nothing has been cancelled since the flip
SELECT count(*) FROM openrails.subscriptions
 WHERE cancelled_at > (SELECT updated_at FROM openrails.destructive_action_switch);

-- 3. no entitlement has been revoked since the flip
SELECT count(*) FROM openrails.entitlements
 WHERE revoked_at > (SELECT updated_at FROM openrails.destructive_action_switch);

-- 4. destructive provider intents are parked, not executing
SELECT status, count(*) FROM openrails.rail_intents
 WHERE intent_type = 'nmi_delete_subscription' GROUP BY status;
```

Worker logs name the gate explicitly: `destructive actions gated — instance kill
switch is OFF`.

### Arming a merchant (the #835 first-enforce gate)

A merchant with no `openrails.merchant_destructive_policy` row — or one with
`enforce_armed_at IS NULL` — pulls in **advisory** mode: findings are persisted,
nothing is mutated, no source domain is proven, and `first_pull_completed_at` is
stamped so you know the survey is ready.

```sql
-- what did the first pull find?
SELECT finding_type, status, count(*) FROM openrails.reconciliation_findings
 WHERE merchant_id = :merchant GROUP BY 1, 2 ORDER BY 3 DESC;

-- happy with it? arm the merchant for enforcing pulls
INSERT INTO openrails.merchant_destructive_policy
       (merchant_id, destructive_actions_enabled, enforce_armed_at, updated_by, reason)
VALUES (:merchant, true, now(), 'you', 'reviewed first-pull findings')
ON CONFLICT (merchant_id) DO UPDATE
   SET enforce_armed_at = now(), destructive_actions_enabled = true;

-- and the instance switch (once, per deployment)
UPDATE openrails.destructive_action_switch SET enabled = true, updated_by = 'you';
```

Both halves must be on: the instance switch gates the fleet, the merchant row
gates one merchant. Disabling either stops that merchant.

### Cancellation caps (#837)

Independently of the switch, one pass may cancel at most
`min(25, max(3, 5% of the merchant's live linked book))` subscriptions. Over
that, **none** are applied, the merchant's pass halts, and a
`pull.cancellation.capped` finding lands in the review queue. It is all-or-
nothing on purpose: a pass that wants to cancel 850 customers is not a pass that
should cancel the first 25 of them.

```sql
SELECT subject_key, recommended_action, updated_at
  FROM openrails.reconciliation_findings
 WHERE finding_type = 'pull.cancellation.capped' AND status = 'requires_review';
```

Investigate the roster before clearing it. The usual causes are a misdeclared
`psps.account_id`, a credential rotated onto a sibling sub-account, or a
provider incident returning a short page — never 850 customers all leaving.

## Per-merchant API hosts (#734) + browser CORS (#765)

Public multi-merchant deployments (one engine serving several merchants) give
each merchant its own canonical API hostname — used for Host→merchant
resolution and Host-routed webhooks. Browser CORS is a **separate, fixed,
engine-wide policy**, not a per-merchant setting.

- **Configuring a merchant's host**: `merchants.Service.SetHostConfig(ctx,
  merchantID, apiHost)` sets `openrails.merchants.api_host` (globally unique
  among live merchants) — a plain row UPDATE, resolved LIVE on the next
  request; no boot-time host map, so a merchant configured on one node
  resolves immediately on every node sharing the database. Leave `api_host`
  unset for a merchant that should never resolve from any Host.
- **Local-dev hostnames**: `api_host` compares against the request Host with
  the port stripped (`merchants.NormalizeAPIHost`), so
  `api_host = "api.acme.localhost"` resolves on any listen port — point
  `/etc/hosts` at `127.0.0.1` per name.
- **Reserved names**: `pkg/merchant.ReservedHostedSlugs` is the advisory list
  a hosted product should refuse to let a merchant self-provision as a slug
  (a slug commonly becomes `api.<slug>.<domain>`); the engine doesn't enforce
  it — the host does.
- **Webhook surfaces**: three shapes, all verifying with the resolved
  merchant/account's own signing secret. Canonical provider-only:
  `/v1/webhooks/:provider` (NMI/CCBill — payloads carry account identity) and
  `/v1/webhooks/:provider/:account_id` (Stripe / multi-account rails).
  Merchant-scoped alias:
  `/v1/merchants/:merchant/webhooks/:provider[/:account_id]`. Host-routed
  (`RegisterHostWebhookRoutes`, mounted when a host resolver is attached):
  `/webhooks/:provider[/:account_id]`, merchant resolved from the Host header.
- **Consistency with token issuers**: a JWT minted for merchant A's issuer is
  rejected when presented against merchant B's Host, even though the token
  verifies — Host-merchant must equal issuer-merchant on every
  merchant-scoped route. The check only fires when a Host actually resolved a
  merchant.

### Browser CORS doctrine (#765)

CORS protects requests authorized by an ambient credential (a cookie the
browser attaches automatically). OpenRails never issues cookies: every
browser-tier request carries an explicit bearer JWT placed by the page's own
JS, which a different origin's script cannot read; an unauthenticated
cross-origin call just 401s; a stolen token is replayed from `curl`, where
CORS doesn't exist. So a per-merchant origin allowlist protected nothing —
cut in #765. The engine answers a **static, non-configurable** policy, by
route tier:

- **Checkout + self-service + customer-treasury** (buyer-facing
  catalog/checkout, `/v1/me/*`, `/v1/customers/*`, and their embedded
  equivalents) answer every preflight and response with
  `Access-Control-Allow-Origin: *`, the methods/headers those routes need,
  and a 12h `Access-Control-Max-Age` — from ANY origin, zero configuration;
  `Access-Control-Allow-Credentials` is NEVER set. A merchant frontend calls
  OpenRails directly with no origin-registration step.
- **Every other surface** (admin console, platform directory,
  merchant/service API, inbound webhooks, control-plane auth) emits NO CORS
  headers at all — the correct, free posture for bearer-JWT curl/service
  callers.
- This is engine code (`internal/http/middleware.PermissiveCORSHTTP`, gated
  by a `BrowserTierRoutes` registry populated as the browser-tier routes
  mount), not a database column or config key, and it has no dependency on
  `api_host`/Host resolution. The legacy global `cors_origins` config key
  stays retired: OpenRails' CORS posture isn't configurable at all.
