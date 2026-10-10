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
deterministically, from catalog + money state + free product grants: a
customer holds the current keys of the products they hold. So there are
exactly four ways the system diverges, each with its own mechanism:

| # | Divergence | Direction | Mechanism |
|---|---|---|---|
| 1 | Catalog wrong at the provider | separate provider workflow | `apply-catalog` commits local catalog changes and read-verifies existing provider bindings; it never creates/updates provider objects or queues provider work. Changes requiring provider writes fail before local mutation. Scheduled drift watching is alert-only (`catalog_reconciliation_interval`, default 1h, `0` disables); catalog prune never deletes provider extras |
| 2 | Money state wrong locally | pull (provider → OpenRails) | webhooks in real time; **Provider Refresh** as the always-on scheduled read; **`pull-provider`** as the manual batch truth-pull |
| 3 | Outbound action never executed | (intent, not sync) | **durable intent + replay** — see "Durability model"; the Convergence Engine's stuck-intent check is its detector |
| 4 | Entitlements inconsistent | derived | the **Convergence Engine** re-derives them once 1–3 are true |

## One authoritative billing database

All active replicas for a merchant's billing book must share one PostgreSQL
database with one writable primary. Database admission locks, unique operation
keys and executor claims coordinate those replicas. Independently writable
copies of that database using the same payment-provider account are unsupported:
each copy can acquire its own locks and try to collect the same renewal.

Provider idempotency supplements database coordination within each provider's
documented limits; it does not make independent copies safe for active-active
billing. A provider lookup followed by a charge is not an atomic lock.

Two fences keep a copy from billing:

- **The book's identity.** `billing.book_identity` records the PostgreSQL
  cluster, database and schema the book was armed in. Anywhere else (a dump
  restored into another database or schema, a major-version upgrade that
  re-initializes the cluster) every provider write is readonly, startup logs
  an error, and `openrails book status` says so. Once every other copy is
  stopped, `openrails book arm --by NAME` arms this one. A promoted physical
  replica keeps the identity, so failover needs nothing; a physical clone (a
  disk snapshot, a base backup started as a second server) keeps it too, so
  rotate the PSP credentials before running one.
- **The merchant's write posture.** `billing.merchant_write_posture` can
  lower one merchant below `provider_write_mode`: the effective mode is the
  lower of the two. A billing export leaves its source readonly and an import
  lands readonly, so a moved merchant bills from neither copy until
  `openrails merchant arm --merchant NAME --by NAME` arms the one that stays.
  `openrails merchant hold --mode readonly|limited` holds one merchant.

Follow the [offline merchant transfer procedure](merchant-portability.md) when
moving a book: stop and drain source writers, disable automatic restarts, keep
destination writers stopped during restore, then activate only the destination.
Once destination billing starts, rollback requires another controlled transfer
of current state. Never restart the stale source snapshot as a rollback.

`provider_write_mode: readonly` blocks provider mutations, but still permits
verified local receipt recovery, additive provider reconciliation and webhook
ingestion. It does not make a running source
quiescent. Where supported, revoking separately scoped source credentials at the
provider offers stronger protection against accidentally restarting an old copy.

Established NMI and Stripe books also hold new collections and destructive work
when completed provider coverage is stale or known financial conflicts remain.
Startup reads catch up while this gate is closed. Configured `full` resumes
eligible work automatically after verified catch-up; explicit `readonly` remains
readonly. See [provider recovery](provider-recovery.md) for the account scope,
completion rules and limits. CCBill and Solana retain their existing controls.

## Running several instances

Several instances can serve one billing book: replicas of a host app that each
call `openrails.New` and `Start` (embedded), or several `openrails run-server`
and `run-worker` processes (standalone). They need nothing from each other but
the database: there is no instance count to configure, no primary instance and
no sticky session. `TestInstancesShareOneDatabase`, `TestServersShareOneDatabase`
and the `TestReplicas` suites run several instances on one database.

### What they share

- **PostgreSQL**, one writable primary (above): every record, River's queue and
  its leader election (in the River schema), request idempotency claims,
  webhook deduplication, provider intents, and the locks that admit a charge.
- **Redis**, optional at any scale and never assumed: an instance uses one only
  when `Config.Redis` (an address or a `redis://` or `rediss://` URL, with an
  ACL user and TLS as needed) or `Deps.Redis` names it. With it, rate-limit
  windows, admin lockouts and captcha challenges are counted there; without it
  they are counted in PostgreSQL, once for the whole fleet either way. A
  declared Redis that stops answering costs speed, not correctness: requests
  count in PostgreSQL meanwhile and readiness reports it degraded without
  failing. Only Redis carries the card-abuse captcha accelerator, the
  admission-denial statistics and the shared FX quote cache: without it each
  instance fetches and caches its own rates for five minutes.
- The standalone server records spent DPoP proofs in PostgreSQL. Its AuthKit
  still counts its own rate limits in Redis: without Redis, `auth.allow_memory`
  keeps them in the process, for one instance only.

### What must be identical

- **Configuration**: the schemas, `TestMode`, `ProviderWriteMode`,
  `Config.Merchant` with its PSPs and secrets, `Config.Catalog`,
  `Config.RateLimits`, `Config.Captcha`, `Config.TrustedProxies` and
  `Config.ReturnOrigins`. Every start applies the declaration: instances
  declaring different SCIM tokens replace each other's token at each start.
- **Routes**: every instance mounts the same `openrails.Routes` (prefix,
  permission bundles, admin console). A load balancer sends any request to any
  instance, so a route mounted on some instances is a 404 on the others.
- **Keys**: `encryption.master_key` (an instance with another key cannot read
  merchant credentials, and the first to create a merchant's data key locks
  the others out of it); the standalone signing key (`auth.active_key_id` and
  `auth.active_private_key_pem`, or `keys.json` in `auth.keys_path`) and the
  `totp.key` beside it, since a token one instance signs is refused by another
  (`auth.allow_ephemeral_signing_key` makes a key per process: one instance
  only); `resource_server.dpop_nonce_key`, since a nonce from one instance is
  refused by another; the captcha secret.
- **Version**, except during a rolling upgrade.

### Background work and leadership

Every instance that calls `Start` (or runs `run-server` without `--no-workers`,
or `run-worker`) works River jobs; River hands each job to one instance. A job
whose instance dies stops its liveness beat and is rescued about five minutes
later. Periodic work is scheduled by River's leader alone: the first instance
to start leads, an instance that closes resigns and another takes over at once,
and a crashed leader is replaced within about 20 seconds. Each schedule fires
on its period's clock boundaries and is unique per period, so the fleet runs
each sweep once a period whoever leads; a new leader repeats only the passes
that run on start (the due pass, rescue, provider refresh), which are
idempotent. A host fleet (`openrails.WithRiverClient`) follows the same rules,
and every replica's fleet in that River schema must carry OpenRails' jobs:
whichever replica leads schedules them.

Outside River, every instance runs the Solana Pay poller, which shares
references through leased claims, and verifies its own PSP credentials at
start.

### Caches

Catalog, merchant settings and lookups, PSPs, write posture, the kill switch
and entitlements are read from PostgreSQL per request: an edit through one
instance is read by every other on its next request. Per-instance caches:

| Cache | An edit elsewhere shows after |
|---|---|
| Merchant secrets (database backend) | at once for PSP credentials (read by version); up to 15 minutes for other secrets |
| Solana keypair signer | 60 seconds |
| FX quotes without Redis | 5 minutes |
| `GET /v1/config` in browsers and CDNs | 5 minutes (`Cache-Control`) |
| PSP posture verdicts | the instance's next start: restart every instance after fixing a PSP that started disarmed |

### What scales

- **HTTP**: any instance serves any request: checkout pages, webhooks, the
  admin console and its API.
- **Webhooks**: one effect per provider event however many instances receive
  it. A copy arriving while the first is applied waits up to 10 seconds, then
  answers 500 so the provider retries.
- **Idempotent requests**: one execution per key; a retry on another instance
  replays the result, or answers 409 while the first still runs.
- **Money**: one charge per renewal and retry slot, one payment per provider
  transaction, one grant, one ledger posting and one email per notification.
- **Workers**: each standalone instance works up to 20 billing jobs at once,
  so instances add throughput. Readiness needs the instance's own workers, so
  every `run-server` instance runs them (`--no-workers` is never ready); add
  `run-worker` instances for background work that serves no HTTP.
- PostgreSQL is the limit: size `max_connections` for every instance's pool.

### Rolling upgrades

`New` and `run-server` migrate before serving, under an advisory lock: the first
instance migrates, the others wait for it, and none fails or applies a
migration twice. Migrations are additive, so instances of the previous build
keep running on the migrated schema and a rollback to it is safe; the newer
migration stays. `openrails migrate up` can run first as a deploy step
instead. During the rollout a browser may load the admin console from a new
instance and its assets from an old one; a reload fixes it once the rollout
ends.

## Mutation Flags

Provider pull and merchant-configuration commands use mutation flags. Catalog
application instead carries optional `prune` in its document; it has no
insert/overwrite flags. Each canonical catalog content hash is remembered
permanently, so replaying an old document never rolls back later edits. For
commands that use mutation flags:

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
| `run-server [--no-workers]` / `run-worker` | serve the public API (+ workers unless disabled; `--no-workers` remains live but not ready) / workers only |
| `migrate up` / `migrate pg` | apply OpenRails' and River's migrations ahead of a rollout; the server applies them, and AuthKit's, at boot |
| `migrate status [--json]` | compare embedded OpenRails migrations with the applied ledger; non-zero unless names and hashes match exactly |
| `access-cutover preflight [--approve NAME] [--json]` | list every customer and key the cutover to product access changes; `--approve` records them so the cutover migration applies exactly that list — see "Cutover to product access" |
| `push-auth-bootstrap [--file] [--dry-run] [--startup-only --name]` | push AuthKit root authority from a bootstrap manifest |
| `push-merchant-config [--file] --insert` | initialize missing merchant identities and snapshot metadata; existing metadata is preserved |
| `get-merchant-config` / `apply-merchant-config --merchant NAME --file PATH` | read or apply metadata using stable application ID and revision; local or `--server-url` remote Client |
| `apply-catalog --merchant NAME --file PATH [--force-conflicts]` | apply a catalog document: each product, price and meter applies whole unless an edit set a field it names differently, which is skipped, listed, and exits non-zero (`--force-conflicts` overwrites it). A document that applied whole replays by content hash; omission is preserved unless `prune: true` ([catalog ownership](catalog-ownership.md)) |
| `catalog export --merchant UUID --out PATH` / `catalog import --merchant UUID --in PATH` | [full catalog YAML snapshot](catalog-export.md), preserving archived rows, original IDs and retained history; restore requires an empty catalog |
| `dump-merchant-config --slug [--out]` / `dump-merchant-catalog --slug` | export a merchant's config / [active catalog YAML](catalog-export.md); full billing history uses `billing export` |
| `pull-provider` / `pull-provider report` | manual provider truth-pull / run report — see "Provider Pull" |
| `prune list` / `converge list` | inspect the destructive runs a `--prune` / an enforcing pull opened |
| `undo-run --run <id>` | plan or apply the reversal of one destructive run, whatever kind — see "Reversing a destructive run" |
| `intents` / `intents-log` | read-only intent-ledger views — see "Inspecting the ledger" |
| `book status` / `book arm --by NAME` | report whether this database is the billing book's armed copy / arm it once every other copy is stopped |
| `merchant arm` / `merchant hold --mode readonly\|limited` (`--merchant NAME --by NAME`) | restore or hold one merchant's provider writes; export and import leave it readonly |

The `push-*` commands push declared file state outward; `pull-provider` moves
the opposite direction and never mutates a payment rail.

### Merchant secrets

PSP secrets in `push-merchant-config` are seed material, not the runtime
source of truth. The manifest key is `merchants.<slug>.psps.<psp-key>`
(secret overlays: `merchant_manifest_overlays`); the retired anchors
(`accounts`, `rail_merchant_accounts`, `provider_accounts`) fail loudly with
a rename error. The command imports each PSP's secrets under the canonical
scoped name `psps/<rail>/<environment>/<account_id>/<secret_key>` into the
backend the server reads (`secret_backend: db | vault`): Vault KV-v2 path
`<mount>/openrails/merchants/<merchant-slug>/<name>`, or
`billing.merchant_secrets` envelope-encrypted under
`encryption.master_key` / `ENCRYPTION_MASTER_KEY`. Runtime checkout,
webhooks, tokenization, provider intents, and pulls all arm per-PSP from that
scoped name.

## Private Standalone First Run

On an empty private standalone install, run the file-backed push commands as
an init job or manual operation:

```bash
openrails push-auth-bootstrap --config /etc/openrails/config.yaml --file /run/openrails/bootstrap.yaml
openrails push-merchant-config --config /etc/openrails/config.yaml --file /run/openrails/merchants.yaml --insert
openrails apply-catalog --merchant your-merchant --config /etc/openrails/config.yaml --file /run/openrails/catalog.yaml
```

`push-auth-bootstrap` runs first because it creates the initial AuthKit root
operator; merchant config then creates OpenRails merchant groups, PSP rows,
and secrets. Normal server restarts never reconcile merchant config or
catalog files. If `/etc/openrails/bootstrap.yaml` is mounted, startup
bootstrap is first-run only and limited to AuthKit authority.

## Durability model

**Outbound — durability is OUR job.** Every mutation OpenRails wants to make
against a provider must survive failure of the attempt. The mechanism is the
**provider intent ledger** (`billing.provider_intents`): every outbound
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
deletes, NMI/Stripe refunds, dunning `manual_rebill` charges, CCBill
cancels, payment-method swaps, vault deletes, checkout NMI sales and Solana
recurring pulls.

Execution is **effectively-once**, never assumed exactly-once. Per class:
money-movers park ambiguous outcomes as `unknown_needs_verify` and are
resolved by *reading* the provider before any retry — a charge is never
blind-retried; deletes/cancels are verify-then-execute (already-deleted =
success); creates are content-addressed find-or-create. Stripe ops
additionally send `Idempotency-Key`. Every attempt/outcome is appended to
`billing.provider_mutation_logs`.

**Several replicas.** Hosts may run any number of processes against one
authoritative database and River schema. Rebilling coordination uses the
database: admission locks the customer and subscription
rows; unique indexes allow one unresolved charge operation per subscription
and one engine operation per (period, attempt) slot; the write-once submission
fence admits one sender per charge; completion re-reads the operation under the
same locks. A pass that finds its membership already settled by another replica
does nothing. Lease checks and provider-call deadlines require synchronized
instance clocks; keep replicas NTP-synchronized. A known failure before dispatch
retains proof that permits another attempt. A crashed sender cannot supply that
proof merely because a provider lookup is empty. `ci/`'s
`TestReplicas*` fleets exercise shared-database concurrency and recovery.
Database claims are not a provider-enforced fence against an arbitrarily paused
sender or an independently writable database copy.

**Renewal deduplication.** New engine renewal operations have a deterministic
identity for the merchant, PSP, subscription, original period boundary and
attempt. A network retry keeps that identity. A new payment attempt follows only
a conclusively closed earlier attempt; a changed card or amount is not an excuse
to generate a different identity for the same attempt. Already accepted
operations retain their original IDs.

Stripe PaymentIntents retain the renewal obligation, attempt and accepted-term
binding in metadata. Before submission, OpenRails lists the customer's provider
payments and checks that metadata, then reads any matching payment by ID. An
existing matching payment is recovered; conflicting terms or another executable
or paid attempt require reconciliation. This uses customer listing rather than
Stripe's [eventually consistent Search API](https://docs.stripe.com/api/payment_intents/search).
Native NMI saved-card renewals use the persistent obligation
order reference and check for a qualified existing payment even before the
first local attempt. The `order_description` identifies the exact operation
(`OpenRails renewal <operation UUID>`), so a previous attempt's decline cannot
release a newer uncertain payment. Decline recovery also checks the frozen
instrument, amount and currency. These lookups can find charges after a
provider's duplicate window has elapsed, but a lookup and a subsequent charge
are not atomic.

[Stripe may discard idempotency keys after 24 hours](https://docs.stripe.com/api/idempotent_requests).
Automatic resubmission of an
uncertain Stripe renewal therefore stops before that deadline, with clock and
provider-call margins measured from its original submission fence. Reads of the
existing payment continue. An empty or unavailable lookup after that deadline
leaves an unresolved-submission finding; it does not authorize another charge.
Provider receipts and operator reconciliation must resolve that uncertainty.
NMI duplicate-check behavior depends on the account/processor; an order reference
is a lookup key, not a provider-enforced uniqueness constraint. Once an NMI
submission may have reached the gateway, OpenRails only reads its outcome and
never automatically submits it again. Empty Query API results have no documented
visibility deadline. An unknown outcome keeps its durable claim and raises
`life.submission.unresolved`; a later matching receipt completes it. If the
request actually never arrived but its sender crashed after the submission
fence, operator/provider reconciliation is necessary. NMI cannot provide both
unconditional automatic recovery and a guarantee against duplicate charges in
that ambiguous window.

**Outages within a billing period.** After a five-day outage of a monthly book,
startup's normal due and rescue jobs recover queued work and overdue renewals.
The payment covers the original due period and preserves its next billing date.
Fresh collection is refused once the entire next renewal period has elapsed;
an accepted but never-submitted operation also expires at its frozen period end.
This produces a reviewable finding instead of shifting the schedule or charging
a backlog. Already submitted payments continue receipt recovery even after that
boundary. `TestNMIFiveDayRestartRecovery` exercises real process termination and
restart with two replicas; provider fixtures establish the local contract, not
live processor qualification.

Invoice collection and finalization also scan at startup. Monthly small-balance
collection retains its last completed thirty-day period in the merchant book,
including export/restore; pruning completed River jobs cannot reset its cadence.
The marker advances only after the entire eligible scan succeeds. Partial errors
leave the scan retryable, while already accepted payment operations retain their
own claims and receipts. Monthly scans serialize through a PostgreSQL session
lock without holding a transaction across provider requests. Worker pools need
at least two connections (coordination and billing); a five-second coordination
acquisition failure leaves the scan retryable. A collection scan held by
`limited` or `readonly` remains queued,
so it can resume after writes are enabled instead of consuming its monthly slot.

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

`openrails intents-log [--rail=…] [--intent=…] [--psp=…]
[--phase=attempting|succeeded|failed|unknown|parked]` renders the append-only
mutation-attempt log — the executor's audit trail.

### Materialized backlog under mode=limited

The dunning worker's scan runs under `limited` and records its decisions
instead of skipping: window-expired `past_due` subscriptions (a freshly
migrated backlog's bulk) are parked as unknown for provider verification,
and in-window charges enqueue as PARKED system-origin
`manual_rebill` intents — bounded by `expires_at` = the dunning window, so
one can never fire stale after the mode lifts; the handler re-checks
relevance (still past_due, same period) at execution. Materialize never
claims the subscription (claiming writes `last_retry_at`, which is
dunning-forensics evidence imported from legacy) and never applies failure
policy or local terminal cancellation. `readonly` holds new provider writes and policy-held local cancellations;
provider reads and verified local financial recovery continue. NMI/Stripe
freshness checks also hold destructive decisions until the account catches up.

### PSP binding and credential rotation

`billing.psps` is an **operator-declared** catalog: one row per merchant
PSP account on a rail, with an opaque declared `account_id`. There is NO
runtime "whoami"/identity resolution — OpenRails never fetches or verifies
the account identity behind a credential; the declaration is trusted.

One local check stands: two live PSPs on a rail holding the same account
credential (NMI `security_key`, Stripe `secret_key`) are one gateway account
declared twice. The later one stays disarmed — its credentials read as absent,
so its intents park and checkout never offers it — and a
`consistency.duplicate_gateway_account` finding names it. Declared PSPs are
compared at startup; published ones store `psps.credential_fingerprint`, an
HMAC of the credential under a key derived from `encryption.master_key`
(nothing is stored, and published PSPs are not compared, without one).
Archiving the duplicate, or publishing its own credential, clears it.

Every provider intent is stamped at enqueue with the `psp_id` it was produced
against, and the executor/verifier arm the rail client for **that** PSP row
from its scoped secrets at drain time. A PSP whose credentials cannot be
armed fails closed: its intents park (never execute against a different
account) until the credentials return or the intent expires/supersedes.
Rules:

- **Rotating a credential within the SAME PSP**: replace the
  secret under the same PSP row — intents arm with the new value
  transparently. `PATCH /v1/admin/psps/{id}` (and the console's
  **Rotate** action) is atomic in the way that matters:
  - the **new** credential is live-probed against the provider *before*
    anything is written (NMI and CCBill today). A probe failure fails the whole
    request — no secret is stored, no watermark moves, and the **old credential
    keeps serving**, unchanged, everywhere.
  - a committed rotation is **deployment-wide at the next read**, not
    per-node. Each node fronts the secret backend with an in-process TTL cache,
    so the rotation records the credential's new secret version on the shared
    PSP row (`psps.credential_versions`, surfaced as
    `credentials.<key>.rotation_version`). Every credential resolution already
    re-reads that row, and no node may answer from a cache entry below the
    recorded version — so a retired credential cannot be presented after the
    rotation commits, on any node, without waiting out a TTL. Restarts and
    manual cache flushes are not part of the procedure.
  - omit a credential from the request to leave it (and its watermark) alone;
    re-submitting an identical value is a no-op, not a rotation.
- **Moving to a DIFFERENT PSP**: never repoint an existing PSP
  row's credentials (OpenRails cannot detect the swap — the declared
  `account_id` would silently lie). Arm a NEW PSP and archive
  the old one; `archived` is drain-only — no new checkout/pull work selects
  it, but it remains addressable for existing obligations and inbound events.
  `POST /v1/admin/psps/{id}/archive` makes no provider call, so it
  works when the old provider is terminated or unreachable.
- **Pending intents stamped with the old PSP do not follow** a credential
  move: keep (or restore) the old PSP's credentials until its queue drains,
  or let stale intents expire/supersede via their relevance windows. There is
  no rebind command.
- **Identity is the PSP id, not its key.** Provider references (price links,
  vault references, webhook dedup) belong to the PSP's id, so renaming or
  archiving a PSP moves nothing ([provider object identity](architecture/provider-object-identity.md)).
- **Subscribers on an archived PSP stay there.** OpenRails never moves or
  cancels a working provider-owned subscription. An archived PSP takes no new
  purchases; its live subscriptions keep renewing on it and stay monitored. A
  subscriber leaves it only when their card lapses and they buy again with a
  new card, which lands on an active PSP.

### Custodians

`billing.custodians` is the same kind of catalog, one axis over: a row is
one merchant-owned account with a third-party card CUSTODIAN (Basis Theory
today). A PSP references it by `psps.custodian_id`, so one custodian can back
several gateways — its tenant id and its private application key exist once,
not once per PSP.

Custodial credentials are scoped by the custodian's own identity,
`custodians/<kind>/<environment>/<account_id>/<key>`, exactly as PSP
credentials are scoped by theirs, and are read through the same rotation
version floor — recorded on `custodians.credential_versions` rather
than on a PSP row. Rotation and archival follow the PSP rules
above verbatim: rotate in place under the same row; to move to a different
custodian account, declare a NEW one and archive the old one for drain — an
instrument the old custodian holds is never re-vaulted or destroyed.

Inbound custodian webhooks route by the global `(kind, environment,
account_id)` key. They resolve the
CUSTODIAN, not a PSP: the event is about the stored instrument, and asking
which of several referencing PSPs it belongs to has no answer.

## Provider Pull

Manual-only — **never scheduled**. It never writes to a provider.

```
openrails pull-provider --merchant=<slug> [--rail=nmi,stripe,…] [--psp=<uuid>]
                        [--since=… --until=…] [--manifest=…] [--format table|json]
                        [--log-dir=…] [--insert] [--overwrite] [--prune [--expect-rows=N]]
openrails pull-provider report --merchant=<slug> [--run=ID] [--format table|json]
openrails prune list    --merchant=<slug> [--limit=N] [--format table|json]
openrails converge list --merchant=<slug> [--limit=N] [--format table|json]
openrails undo-run      --merchant=<slug> --run=<id> [--apply --expect-rows=N] [--format table|json]
```

A bare `pull-provider` pulls provider truth, diffs, logs what it WOULD
change, and persists nothing; the mutation flags follow the standard contract
(`--prune` retires eligible local subscriptions/payments attributed to the
pulled PSP that are absent from the provider source). `--rail` is repeatable
(default: every configured rail); `--merchant` is required; `--manifest` arms
mode-1 credentials from a merchant manifest. After a mutating pull the engine
runs a one-shot `Converge(merchant)`.

### `--prune` is reversible, and refuses uncertainty

A prune acts on ABSENCE — "the provider did not list this row" — the weakest
evidence there is. So it is built to be undone and hard to fire by accident:

- **Nothing is deleted.** Eligible rows are soft-deleted (`deleted_at`) and
  stamped with a destructive-run id. They vanish from every live read; the data
  stays.
- **`--prune` alone is a plan.** It reports what it would retire and writes
  nothing. Applying needs `--expect-rows N`, and N must equal the number the
  plan reported — an operator who miscounts is stopped, not obeyed.
- **An empty provider roster refuses outright.** A successful-but-empty pull is
  indistinguishable from a misdeclared `account_id`, a credential rotated onto a
  sibling sub-account, or a provider incident. It never means "prune everything".
- **Grant-entangled rows are skipped**, as before: retraction goes through
  convergence, never removal.

### An enforcing pull is reversible too

`--prune` retires rows; an **enforcing** pull (mutation flags set) overwrites
them — the measured incident is a bad NMI roster cancelling 40/40 subscriptions,
which changed `status`, `ended_at`, `canceled_at`, the grace/retry schedule and
the period bounds, and queued deferred NMI vault deletes behind them. Tombstones
cannot undo that, so an enforcing pass records what it is about to overwrite:

- it opens a `maintenance_runs` record — merchant-scoped, PSP-bound when the pass
  is account-bound, carrying the coverage proof that authorised it;
- it captures a **before-image** of each subscription (and of the entitlement
  windows the transition closes) *before* writing. If the capture fails, the
  transition is not applied;
- it stamps the provider intents it queues with the run id.

A pass with no way to record its undo refuses to run rather than doing
irreversible damage.

### Reversing a destructive run

One verb reverses any of them. A prune destroys rows and reverses by clearing
tombstones; an enforcing pass destroys row VALUES and reverses from the captured
before-images — but `undo-run` reads the kind from the ledger and dispatches, so
nobody can reverse the wrong way round and be told it worked.

```
openrails prune list --merchant=<slug>            # or `converge list`: find the run id
openrails undo-run   --merchant=<slug> --run=<id>                        # PLAN — changes nothing
openrails undo-run   --merchant=<slug> --run=<id> --apply --expect-rows=N
openrails pull-provider --merchant=<slug>          # advisory: review findings, then re-arm
```

**Dry run is the default.** With no `--apply` the command prints what it would
restore, the provider writes it would supersede, the ones that already fired and
cannot be undone, plus the coverage proof that every live provider row is
PSP-attributed (a non-zero count refuses the undo outright).
Applying additionally requires `--expect-rows` to match that plan: an undo is
itself a mass mutation of the live book, at the worst possible moment to be
wrong.

**Scope is a property of the run, not a flag.** The ledger row carries the
merchant and — when the pass was account-bound — the PSP, and every restore
predicate is keyed on the run id inside a merchant-scoped connection. Reversing
one PSP's bad run cannot reach a sibling PSP's book, and cannot reach another
merchant at all. There is no widening knob.

An apply runs five steps in a fixed order:

1. **Quiesce** — clears the merchant's first-enforce arming and trips its
   destructive stop, so nothing re-cancels what is about to be restored.
2. **Supersede the unfired intents FIRST**, before any row is restored. This is
   the only step racing a live actor: the intent runner may claim a queued NMI
   vault delete at any moment, and `superseded` is an ordinary forward status, so
   neutralising a queued provider write is a lifecycle transition, not a rewrite
   of the intent log. Only `pending`/`failed_retryable` count as unfired.
3. **Restore** the rows, in one transaction with step 2 — a reversal that
   superseded the intents but failed to restore the rows would leave the operator
   worse off than before. A subscription a renewal, cancel or payment moved after
   the run keeps that newer state: rewriting it could make a paid period due
   again. The plan counts those rows as `subscriptions_changed`.
4. **Invalidate and re-derive.** The product-access windows the run closed are
   soft-deleted, never replayed, and `Converge` rebuilds them from the
   append-only grant log the rollback never touched. The proven source-domain
   flags are reset to unproven, because the post-rollback book is definitionally
   incomplete and a stale `true` would license a mass retraction against it.
5. **Report** — rows restored, intents superseded, and what could NOT be undone.

What it never does: restore an entitlement directly (a restored effect can
silently disagree with its grant; a re-derived one cannot), touch the ledger,
grants, status transitions, or the intent/webhook/findings logs, resurrect a row
some other run deliberately removed, or count a provider write that already fired
as undone. Those are reported as **irreversible divergence**, with the
provider-side consequence spelled out per intent type ("the NMI vault entry is
gone; the customer must re-enter a card"); an `in_flight` or
`unknown_needs_verify` intent is reported as **ambiguous** and left to its
executor's lease. Resubscribe and card re-entry are operator and customer work,
never an automatic provider re-write.

Some kinds are refused by name rather than half-reversed: a `merchant_delete`
run hard-DELETEs append-only rows nothing local restores (recovery there is a
cluster PITR or a snapshot taken beforehand), and a kind that exists in the
ledger but has not been made reversible yet says so instead of marking itself
reversed.

The final pull is not optional. A rollback restores local state to before the
run while the provider has moved on; `rollback → pull → converge` is the
complete operation, and the pull runs **advisory** until an operator reviews the
findings and re-arms enforcement by hand.

A pull is authoritative only for the `(merchant, rail, psp)` it actually
queried; mirror reads/writes are scoped to that PSP row, and historical rows
with NULL PSP attribution are never used as proof for destructive absence
handling. NMI safety: canceled subscriptions *vanish* from NMI's recurring
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
(retained failed-payment receipts). Legacy retry-detail artifacts stay with
the importing host; core does not store arbitrary imported dunning reports. Aggregates report per-source and combined
last-action, never-attempted vs attempted-and-exhausted counts, and a
decline-reason histogram; an unavailable history source degrades to a note,
never an error.

## The Convergence Engine

The continuous internal-repair half of the system (the pull feeds it). One
idempotent **`Converge(scope)`** — scope-narrowable from a single customer to
a whole merchant — runs from three places so the system never *holds* an
inconsistency: **inline** after every source mutation (checkout completes,
renewal bills, refund webhook lands, dunning transitions, free product grants);
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
`billing.reconciliation_findings.finding_type`:

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
`life.subscription.dunning_overdue`, `life.provider_intent.stuck`,
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
  (`billing.reconciliation_state`, a ratchet). During an import, "not in
  the local DB" is not absence — it usually means *not imported yet*. Held
  repairs stay `reconcile_required` with the unproven domain in evidence.
- **Stuck intents** (`life.provider_intent.stuck`): the sweep flags provider
  intents sitting non-terminal too long — `pending`/`failed_retryable` older
  than 24h, `in_flight`/`unknown_needs_verify` older than 2h; thresholds are
  hardcoded. Mode-parked intents are informational (that wait is by design);
  everything else is `requires_review` — provider failures, bad credentials,
  or a dead worker. The engine never touches intent rows (the
  executor/verifier own them); findings auto-resolve on recovery.
- **Refused provider operations** (`life.provider_operation.refused`): an open
  provider operation whose cost will not qualify automatically holds the
  customer's capacity until an operator closes it
  (`POST /v1/admin/provider-operations/{operation_id}/close`); the finding
  clears with the close.
- **Silent provider operations** (`life.provider_operation.silent`): an open,
  unrefused hold with no increment or observation for 7 days. A hold never
  expires, so its host must observe, release or refuse it; the finding clears
  when it does.
- **Operational problems** outside the sweep are findings too:
  `consistency.ledger.unbooked` (a provider event OpenRails could not book; the
  merchant repairs the ledger and resolves the finding) and
  `life.worker.stalled` (a periodic job kind stopped completing; it resolves
  itself when the kind progresses). The findings queue is the one merchant
  inbox, and its counts are metrics measures (`open_findings`,
  `orphaned_members`, `freeloaders`, `duplicate_coverage`,
  `verification_pressure`).

## Dunning

The built-in schedule is a function of the price's billing cycle — the retry
span always stays well inside one cycle. A merchant may replace it with its
`dunning_policy` setting; a case runs under the policy in force at its first
decline. Retries are OFFSETS from the initial failure (progressive,
front-loading where transient declines clear). The built-in schedule:

| Cycle | Retry offsets | Failures to terminal | Derived staleness window |
|---|---|---|---|
| < 4 days | none | 1 (first failure is terminal) | min(24h, cycle/2) |
| 4–27 days ("weekly") | +1d, +2d | 3 | 3 days |
| ≥ 28 days ("monthly+", capped) | +2d, +5d, +9d, +13d | 5 | 14 days |

The window always ends inside one cycle (1h → 30m, 1d → 12h). An unknown
cycle (≤ 0, e.g. a one-time price behind a membership) is never given a
schedule: collection refuses to charge, retry or end it and raises
`life.cadence.unknown` for the operator. What a decline does comes from one
table, the same for every owner:

- **Retry on the schedule:** issuer soft and generic declines (insufficient
  funds, over limit, do-not-honor, call issuer, retry later) and gateway,
  processor or configuration errors.
- **Wait for a new card** (`awaiting_method`): bad card data (expired, wrong
  number or security code) and codes the networks forbid retrying on the same
  card (transaction not allowed, lost or picked-up card). A new card resumes
  dunning at the next due pass.
- **Terminal at once:** stolen or fraudulent card, and "stop recurring" codes.

The staleness window ("never
charge a months-old failure") derives from the case's schedule — last offset +
min(24h, cycle/2) slack — so it cannot be misconfigured; anything older is
skipped WITHOUT a charge and parked `unverified` (access intact) for provider
verification. Terminal failure = cancel + revoke entitlements + rail-side
delete via the intent ledger's deferred-delete mechanism.

This schedule governs every subscription OpenRails collects itself: engine
memberships on Stripe and NMI, Solana pulls, and every NMI schedule
(`nmi_schedule`). NMI
never retries a declined scheduled charge, so on `nmi_schedule` the provider's
decline is the schedule's first failure (first retry at the first offset, never
at once). Retries count from when the decline happened, not when OpenRails saw
it: a decline found late by a provider read keeps the same retry days, and a
retry already past runs at the next due pass. A past_due `nmi_schedule` row left with no retry scheduled resumes at
its next step after the last attempt, clamped to grace. Below 96h cycles no
retry is scheduled: NMI's next scheduled charge is the retry. Provider-owned Stripe subscriptions use Stripe's own dunning. Ours is sparser
than Stripe's 8-retry default because each NMI decline costs a per-transaction
fee.

**The due pass** runs every minute and on start: it admits engine renewals at
their paid-period boundary and runs retries whose `next_retry_at` has passed.
A subscription it cannot process raises a standing `life.due_pass.refused`
finding (resolved automatically once it processes) and never fails the pass.

**Reading a case.** `GET /v1/admin/subscriptions?dunning=true` lists the open
cases, `past_due` and `awaiting_method`, each with its `dunning`: `attempts`
(declined charges so far), `retries_left` and `final_retry_at` (from the policy
the case opened under), `next_retry_at`, `waiting_for_new_card` (canceled at
`final_retry_at` unless a card arrives) and `last_failure_reason`. A
provider-owned subscription carries no `retries_left` or `final_retry_at`: the
provider runs its own retries, and `next_retry_at` is its next attempt when
OpenRails knows it (CCBill reports it; Stripe's is not stored). A case leaves the list when it is paid or its retries give up.

**Missed rebills.** Every 15 minutes the rebill watch looks for renewals with
no attempt past their deadline (engine: 1h; `nmi_schedule`: 24h, after reading
NMI's Query API and schedule) and raises one `life.rebill.missed` finding per
cycle. When NMI holds nothing for the cycle and its schedule has moved to the
next period (`provider_skipped`), OpenRails charges the period itself, then duns
a decline on the same schedule. "Nothing" means no transaction of any kind or
outcome since the period's midpoint under the schedule, the subscription's order
reference or the card's vault. A sale NMI voided or refunded in full
(`provider_reversed`), or any other transaction no attempt records
(`provider_unrecorded`), is never charged again: the finding lists the
transactions for review. A stalled or deleted NMI schedule only raises the
finding: NMI may still bill it.

**Payment health alerts.** Each LIFE sweep reads the decline measures per PSP
account and owner and keeps these findings open while their condition holds
(one notification per episode):

| Finding | Fires when |
|---|---|
| `life.payments.new_card_decline_spike` | the last 24h's new-card decline rate is 10 points above the 28 days before, with 50 attempts on each side |
| `life.payments.rebill_failure_spike` | the last 24h's rebill first-attempt failure rate is 10 points above that baseline, or above 25%, over 50 cycles |
| `life.payments.system_errors` | system errors are over 2% of the last hour's attempts (at least 20); critical at once on NMI 410/411 (our account refused) |
| `life.decline.unmapped` | a decline code of the last 30 days that no table maps, per rail and code |
| `life.webhooks.silent` | a PSP account whose provider charges came by webhook (at least 10 in the 28 days before) sent none in the last 24h while pulls found at least 3; per PSP account |

Payment attempts, rebill cycles and NMI history months are kept 25 months;
the cleanup worker deletes older ones.

**Engine outcomes.** A renewal allowance of min(24h, max(5m, period/10))
follows each paid engine period (1h → 6m, 1d → 2h24m, 7d → 16h48m, 30d and
longer → 24h), so unpaid access never exceeds a tenth of the period and a
member keeps access until the renewal decides; a qualified renewal
supersedes it, and a decline or cancellation revokes it. A card-fixable
decline, or a stored method that is gone, parked or no longer qualified, moves
the membership to `awaiting_method`: the customer is asked for a new card,
access follows the dunning access policy, and the wait ends (canceled, access
ended) when the cycle's dunning window does, unless the destructive switch is
off. A new card retries at the next due pass. A terminal outcome while the
destructive switch is off leaves the membership `past_due`.

**Held renewals.** A renewal with no outcome past its allowance is held:
collection is stopped (fleet halted, `engine_admission_hold`, breaker,
readonly, kill switch). A refusal only the member can fix is not held (see
above). By default the member keeps access until the renewal is attempted
(`dunning_policy.access_while_renewal_held`: `keep`, or `suspend` to end access
at the allowance). While any are held, `life.renewal.held` reports their count
and the oldest held age; when collection resumes they are charged normally, a
decline enters dunning, and the finding resolves. Issuer authentication a customer never completes is closed after one
hour for a first payment and after the renewal period's allowance for a
renewal. A
renewal with an unknown NMI submission is never automatically sent again:
provider records may lag even when its charge succeeded. Stripe alone can replay
its original idempotency key after a five-minute observation delay, at most
twice and only inside the key retention window. Unresolved outcomes raise
`life.submission.unresolved` (docs/provider-uncertainty.md).

## Provider Refresh and the unknown cohort

`pull-provider` is the manual operator command. **Provider Refresh** is the
always-on provider-read system: a 4-hourly scheduler (RunOnStart, so startup
after a stale dump or outage does not wait for the tick) fans out one
per-merchant refresh job — staggered, unique per merchant, on a bounded
queue, skipping merchants with no declared PSPs. Three lanes:

| Lane | Purpose |
|---|---|
| Provider Event Refresh | bounded missed-event backfill for NMI, Stripe, CCBill using durable per-merchant/rail/account/domain watermarks (`billing.psp_refresh_watermarks`) |
| Unknown-cohort Reconcile | resolves `unverified` subscriptions against provider truth: one windowed bulk pull per rail + targeted per-subscription probes for rows the bulk pull can't decide |
| CCBill DataLink Refresh | scheduled active-member bulk refresh (CCBill has no cheap per-subscription liveness API) |

A watermark advances only after its provider/window completes successfully;
errors, missing credentials, or partial reads leave it unchanged and the next
pass retries the same bounded window. Refresh writes only local truth through
the same idempotent reconciliation writers the pull uses, then runs scoped
convergence. It never mutates a provider and **never charges** — charging
stays inside dunning. Dunning owns `past_due` (we SAW the failure); the
**silence cohort** — lapsed subscriptions with NO webhook either way — is
parked as `unverified` by the LIFE plane and resolved by the Unknown-cohort lane
against ONE verdict set:

| Provider evidence | Resolution |
|---|---|
| verified renewal charge | renewed — period advanced, the charge backfilled exactly once, never a second charge |
| declined / roster stalled, within the dunning window | `past_due` — dunning owns it from here |
| declined / stalled, beyond the window | canceled; the remote record may still exist, so the deferred rail-side delete is queued |
| no charge, remote alive with future next-billing | adopt the remote period end (clock misalignment); never for a `past_due` row |
| remote absent/terminal | cancel locally + revoke entitlements (no remote delete — it's already gone) |
| no conclusive evidence / rail unreachable | stays `unverified`; the next pass re-derives the cohort and retries |

Mode gating: Provider Refresh runs under `full` AND `limited`; skipped under
`readonly`. Each pass logs a heartbeat plus a per-merchant summary
(`renewed/adopted/past_due/canceled/still_unknown/probed/backfilled/rail_errors`).

**Access during silence — standing access.** There is no timed grace
window. An auto-renew
subscription's product-access window is **standing (open-ended) from creation**
and closes only on proven events: terminal dunning failure,
provider-confirmed death, or an explicit cancel (access then ends at period
end, as the user expects). A subscription parked `unverified` keeps access —
entitlements are never lost to our own uncertainty.

Note: NMI charge detection correlates by the order reference OpenRails stamps
at signup; legacy-imported subscriptions whose NMI `orderid` predates
OpenRails won't match the per-subscription probe — the Event Refresh lane's
watermarked backfill catches their provider events.

## Solana Pay settlement

Solana Pay state is in PostgreSQL. A checkout attempt has one reference in
`billing.solana_pay_references` (`pending` → `confirmed` | `expired`). Every
replica's poller claims due references with `SKIP LOCKED` and walks each
reference's whole finalized signature history, oldest first, resuming from a
stored cursor, so no number of transactions naming a reference can hide a
payment. Each new signature goes to one settlement transaction under the
reference's row lock and is recorded once in `billing.solana_pay_receipts`;
a transfer to one recipient in one mint is credited or reviewed at most once
across all references.

- The amount is the recipient's balance change in the quoted mint, including
  Token-2022 mints, split transfers, transfers made inside another program and
  any token account the recipient owns. Only finalized transactions credit; a
  page confirm waits up to 60 s, then answers `processing` and the poller
  credits it.
- A Token-2022 transfer fee is priced in at quote time: the request asks for
  the gross that delivers the quoted amount, and a built transaction asserts
  the fee on-chain. A fee raised after the quote leaves the merchant short and
  is recorded as `underpaid`. Mints with a transfer hook are refused for
  transaction requests.
- The first transfer of at least the quoted amount that lands by quote expiry
  + 30 min is credited; an excess is credited and flagged `overpaid`.
- Anything else is recorded with `disposition = 'review'` and a
  critical `consistency.ledger.unbooked` finding
  (`solana_pay_<reason>`):
  `already_paid`, `late`, `underpaid`, `session_closed`, `wrong_asset`,
  `unreadable`, `settle_failed`. Nothing is refunded automatically, and
  underpayments are not added together. A transfer that already settled another
  checkout is recorded as `duplicate`.
- Unresolved reviews and pending references refuse the billing archive. After
  refunding, close a review with `openrails solana-pay resolve --merchant …
  --signature … --resolution …`.
- A paid or expired reference stays watched for 7 days so later transfers are
  recorded; the GC job then deletes it. Credited, review and duplicate
  receipts are kept.

## Data retention

Retention periods are constants, the same for every merchant, and not
configuration (`internal/retention`). Every table has one class; its table
comment states it, and a table added without one fails the build.

| Class | What it means |
|---|---|
| Permanent | Never pruned: the ledger, grants, payments, invoices and their items and payments, receipts (catalog and configuration applications, credential publications, product archive operations, credited and review Solana Pay receipts), operation authorizations and their cost qualifications, refusals and resolutions, metered rating watermarks, destructive runs and their before-images. |
| Partitioned | Monthly partitions, created ahead and dropped whole by the calendar. No row is read to prune them. |
| Rows | Rows past a period are deleted by the hourly cleanup job, oldest first. |
| State | Configuration and entities (merchants, PSPs, catalog with its price keys and field owners, customers, subscriptions, payment methods, cursors): one row per thing that exists. |

Partitioned tables:

| Table | Key | Dropped |
|---|---|---|
| `usage_events` | `occurred_at` | 24 months after the month's usage was invoiced. A month is invoiced by the end of the next one, so a month goes 26 months after it began. |
| `admission_operations` | `admitted_at` | Once older than 61 days: the longest spend window (31 days) plus 30. |

Row retention:

| Table | Deleted |
|---|---|
| `subscription_status_transitions` | 25 months (761 days) after `occurred_at` |
| `provider_intents` | finished intents that only instructed a provider (subscription cancel, payment-method and source update, card vault, network token, account-updater batch), 25 months after they last changed |
| `provider_mutation_logs` | 25 months after `created_at` |
| `cost_observations` | 90 days after their operation was settled or released |
| `reconciliation_findings` | resolved findings, 12 months (366 days) after they were resolved and last seen |
| `maintenance_runs` | reconciliation runs no finding names, 12 months after they started; every other kind is permanent |
| `checkout_attempts` | attempts that expired without reaching a provider, 90 days after `expires_at` |
| `checkout_sessions` | at `purge_at`, 24 hours after the session expired |
| `payment_attempts`, `rebill_cycles`, `nmi_history_months` | 25 months |
| `notifications` (customers') | 90 days once read, 180 days if never read |
| `webhook_events` | 90 days after completion |
| `host_outbox` | 30 days after delivery; an undelivered event is never deleted |
| `idempotency_keys`, `card_attempt_failures`, `solana_pay_references` | at expiry, past the longest card-abuse window, and after the 7-day watch window |
| `failed_usage_windows` | once the window has ended, on the merchant's next failed usage |

What is kept on purpose:

- **A provider intent that is the record of something is permanent.** Collections,
  sales, enrollments, rebills, tier changes, refunds and Solana pulls (succeeded
  or refused) are what grants, payments and invoices are checked against, and a
  card erasure is the proof the card is gone. Only the instruction-only types
  above age out. The list lives once in the baseline's
  `provider_intents_updated_at_idx` and once in the retention code; a test
  keeps them equal.
- **A checkout attempt that reached a provider is permanent**, expired or not:
  one with a payment, a subscription or a provider transaction, or that a
  provider intent or a Solana Pay reference or receipt names.
- **A cost observation outlives its operation.** While the operation is open its
  observations stay however old, because settlement is authored from them.
- **A resolved finding that is still being seen stays**, so an ignored drift that
  persists does not come back as new.

How it runs:

- The cleanup job (hourly) creates the partitions rows can be written into plus
  two months ahead, drops the months past retention, then deletes rows for the
  merchants that have any due, 1,000 rows per statement and at most 50,000 rows
  per merchant per pass. A backlog drains over the following passes. Partitions
  are also ensured at migration and by the first write a process makes in a new
  month, so writes never wait for the job.
- A partition belongs to the role that creates it, the one OpenRails runs as.
  Writers that cross into a new month together all create its partitions;
  the first wins and the rest find it there.
- Creating a partition attaches a table built beside the parent, so reads and
  writes carry on. Dropping one locks the table briefly; it gives up after two
  seconds rather than queue writers behind a long reader (a logical backup, a
  merchant archive export) and the next pass tries again.
- `subscription_status_transitions`, reconciliation runs and `cost_observations`
  refuse every other `DELETE`: `billing.guard_retention_delete` lets one through
  only when the transaction names the table in the `openrails.retention_table`
  setting and the row is older than the period the trigger declares. `usage_events` and
  `admission_operations` rows leave only with their partition.

What an integrator sees:

- A usage event's `occurred_at` may be up to 35 days in the past and not in the
  future; its `source` and `source_id` are honoured as its idempotency key for
  those 35 days. A priced event's ledger debit is idempotent for good.
- An admission's hold deadline, as admitted or as extended, is at most 30 days
  past the admission, and a spend window is at most 31 days. After 61 days a
  request id reads as never admitted.
- Usage reports reach back about 26 months, subscription transition metrics 25.
- A merchant archive restore does not bring back `usage_events` or
  `admission_operations` rows already past their retention.

## Cutover to product access

Before product access, each customer held per-key entitlement windows. The
cutover migration converts them to windows of the products that grant the keys,
then drops them. From then on, a customer holds a product's current keys. If
the products changed their keys after a sale, the customer gains or loses them
at the cutover, and the migration refuses until an operator approves every such
change:

1. Stop the old version: the cutover migration drops a column it reads. The new
   version migrates as it boots, so do not start it yet.
2. Run `openrails access-cutover preflight` with the new binary. It applies the
   migrations before the cutover and dry-runs the conversion for every
   merchant. It lists each customer and key the conversion **lost** or
   **gained**, the windows it cannot carry (`unmapped`, e.g. a manual key grant no product matches), and
   purchases whose keys had different durations (`mixed_duration`: they keep
   the longest).
3. Fix what should not change (give a key a product, adjust a product), and
   rerun the preflight.
4. Run `openrails access-cutover preflight --approve <your name>` to record
   the list, and only then start the new version; its boot runs the cutover
   migration. The migration refuses any change that is not on the approved
   list, and the version's boot fails until it is approved.

A host that embeds OpenRails runs the same steps from its own binary, built
with the new version, before starting it: `openrails.AccessCutoverPreflight`
takes the host's pool and `Config.Database` and returns the report the command
prints (`billing.AccessCutoverReport`). An empty `approvedBy` is the dry run of
step 2; a name approves the list as in step 4, and `openrails.New` then boots.

A billing archive exported before the cutover restores through the same
conversion. It is refused (`unsupported_state`, table `entitlements`) if the
conversion would change anyone's access; cut over the source first.

### Heavy buyers

A customer holding at least 500 products gets a cached key set
(`customer_entitlement_cache`). It is rebuilt when a check finds it stale, and
it answers only while the merchant's key generation and the customer's access
version match the ones it was built from. A stale cache is never used.

## Background worker schedule

Everything runs by itself under River once `run-server` (or `run-worker`) is
up. "start" = RunOnStart: the replica that takes River's leadership runs the
job then. Only the leader schedules, and each cadence fires on the clock's
boundaries (an hourly job at :00, a 15-minute one at :00, :15, …), so a change
of leader neither delays nor repeats a period's run.

| Worker | Cadence |
|---|---|
| Provider-intent executor | 1 min + start |
| Provider-intent verifier · admission-denial flush · worker health check (health check + start) | 5 min |
| Notification email sweep | 10 min |
| Convergence sweep (+ start) · arrears delinquency evaluation · Solana Pay reference GC | 15 min |
| Credit-ledger reconcile (alert-only) | 30 min |
| Price-migration re-driver (+ start) · cleanup · credit expiry · Solana crank · Stripe webhook reconcile · invoice collection | 1 h |
| Dunning · Provider Refresh scheduler (+ start; fans out per-merchant jobs) | 4 h |
| Solana gas alert · Solana ledger reconcile | 6 h |
| Catalog reconciliation pull (alert-only) | `catalog_reconciliation_interval` (default 1h; `0` disables) |
| Invoice period finalize / monthly-floor sweep | daily / 30 d |

The health checker seeds `billing.worker_state` and raises a critical
`life.worker.stalled` finding in every merchant when a periodic kind stops
completing; the finding resolves itself when the kind progresses again. Its per-kind rows are written
monotonically: job completions of one kind reach the row in any order, so a
late write can only add what is newer (timestamps never move back, the error
text is the newest failure's, a success resets the failure streak only when no
newer failure is recorded, and a failure counts only when no newer success is
recorded — the streak can over-count after reordering, never under-count). The
fair-sweep cursor is a ring position, saved by compare-and-swap on an opaque
per-save version (a counter, not a timestamp, so tokens never collide), so a
pass finishing after a newer one keeps the newer position.

No job runs under a clock. River's one-minute `JobTimeout` default is
overridden to "never" on every OpenRails worker; a running job is canceled
only when it reports no progress past the same staleness rule the health
checker uses for its kind (3× the declared cadence, floored at 30 min), and
the job row records what it last reported. A job that dies with its process
is reclaimed by River's rescuer, which measures silence from the job's last
liveness beat rather than from its start.

### Health endpoints

`GET /health/live` (liveness) and `GET /health/ready` (readiness). Both are
public and carry no dependency detail: a failing check is logged and answers
503 `service_unavailable`. There is no `/health`, `/healthz` or `/readyz`. Embedded hosts wire the same checks into
their own handler. Readiness requires Postgres, the merchants service, the River producer and a
locally managed River worker consumer; Redis, Vault and PSP posture are
reported as degraded and never fail it; `run-server --no-workers` is therefore live but not ready. An
embedded host's own River fleet (`WithRiverClient`) is outside that
local-process check and is observed through the `openrails_job_progress` probe
(`Client.Probes`).

On SIGTERM or SIGINT the server drains: readiness answers 503 at once, while
the listener keeps serving for `drain_delay` (default 5s), so load balancers
and Kubernetes endpoints drop the instance before it stops accepting. Then it
finishes in-flight requests and stops its workers within `shutdown_timeout`
(default 20s). Keep their sum under the orchestrator's grace period
(Kubernetes' `terminationGracePeriodSeconds`, 30s by default).

`GET /metrics` exports `openrails_dependency_up{dependency,class}` for every
dependency readiness reports, optional ones included. The authenticated
`/v1/admin/metrics` query and schema routes expose merchant business
analytics, not Go/process telemetry.

## Network egress

Besides what its configuration names (Postgres, Redis, Vault, the SMTP host,
trusted issuers' JWKS URIs, `llm.base_url`), the server calls:

| Destination | What for |
|---|---|
| `latest.currency-api.pages.dev`, then `cdn.jsdelivr.net` | FX rates for cross-currency quotes. Fetched when a quote needs one and cached 5 minutes; with Redis, refreshed in the background every 2 hours. Boot never waits for them: a failed refresh is retried with backoff, and a rate it cannot fetch fails only the quote that needed it. |
| `hermes.pyth.network` | Solana token prices, when a Solana rail quotes a token. |
| `api.stripe.com` | Stripe PSPs. |
| `secure.nmi.com`, `sandbox.nmi.com` | NMI PSPs, by `test_mode`. |
| `api.ccbill.com`, `sandbox-api.ccbill.com`, `datalink.ccbill.com` | CCBill PSPs. |
| `api.basistheory.com`, `cdn.basistheory.com` | Basis Theory custodians. |
| `api.mainnet-beta.solana.com`, `api.devnet.solana.com`, `*.helius-rpc.com` | Solana PSPs' RPC (`provider_sandbox.solana_rpc_url` replaces it). |
| `challenges.cloudflare.com`, `www.google.com`, `hcaptcha.com` | Captcha verification, when `captcha` is set. |
| `api.anthropic.com`, `api.openai.com` | LLM features, when `llm.api_key` is set and no `llm.base_url`. |

An egress policy that allows only these keeps every feature; one that blocks
the FX hosts leaves cross-currency quotes failing and nothing else.

## Operating modes (the safety levers)

Security defaults are independent of provider sandbox/live posture. Declare both
`provider_write_mode` (`full`, `limited`, `readonly`) and `test_mode` (`sandbox`,
`live`) explicitly. CLI flags override environment variables, which override YAML.
An unset write policy fails closed to `readonly` wherever it is consulted;
constructor validation requires an explicit policy. Sandbox validates test
credentials and never relaxes issuer, signing, storage or proxy protections.

Narrow local exceptions are configured explicitly under `auth` (for example
`allow_loopback_http`, `allow_memory`, `allow_missing_senders`, `direct_peer_ip`).
Managed DB credentials always require encryption. `public_billing_base_url` is
only the public callback/link mount base; issuer, `auth.request_origin`, remote
Client server URL and `dashboard_base_url` are independent.

Every subscription has one collector, fixed when it is created or imported
(`subscriptions.collection_policy`):

| Policy | Who charges | Who retries a decline |
|---|---|---|
| `engine` | OpenRails, on a saved card (NMI, Stripe) or by on-chain pull (Solana) | OpenRails ([dunning](#dunning)) |
| `nmi_schedule` | NMI, on its own schedule | OpenRails |
| `provider` | the provider (Stripe, CCBill) | the provider |

New subscriptions are `engine`: they need no provider catalog link, and a rail
or term OpenRails cannot collect (CCBill, a trial first phase) is refused
before payment. Imported subscriptions keep the collector their rail implies;
it is never inferred from a saved card, and sharing or replacing a card does
not change it. Configuration never transfers a subscription or starts a second
collector.

`engine_admission_hold: true` pauses the admission and first submission of new
engine payments (initial and renewal). Operations that may already have been
submitted are still verified, and webhooks are still handled. It changes no
stored ownership; `provider_write_mode` remains a separate gate.

A merchant's write posture and the book's identity can lower the mode for one
merchant or the whole book (see above); the lower mode applies.

What each provider write mode permits (`test_mode` applies orthogonally: with
sandbox the same matrix holds against sandbox rails, so no real money can move
in any mode):

| Operation | `full` | `limited` | `readonly` |
|---|---|---|---|
| User checkout / charge | yes | yes | no — refused |
| Card/vault save, tier change, resume, refund | yes | yes | no |
| User/admin cancel → rail-side delete | yes | yes | no — intent parks for replay |
| Dunning charges + window-expiry cancellations | yes | no — runs dry, intents park | no |
| Invoice collection, Solana pulls | yes | no | no |
| `apply-catalog` local changes + existing binding verification | yes — no provider writes | yes — no provider writes | yes — no provider writes |
| Provider reads (query APIs, catalog verification) | yes | yes | yes |
| Webhook ingestion + local serving | yes | yes | yes |

`limited` draws the line at *who initiates* (the system initiates nothing;
humans get everything), `readonly` at *the wire* (nothing writes to a
provider, not even a customer clicking buy — enforced at the transport on
every rail: NMI direct-post gate, Stripe transport gate, CCBill DataLink
read-only flag, Solana submission gate). Typical uses: `limited` = migration
cutover with the site fully usable; `readonly` = reconciliation/forensics
boots that may recover local records without initiating provider writes.
Provider receipt recovery runs in every mode. The NMI/Stripe freshness gate is
an additional prerequisite for existing-obligation collection and destructive
work, including under `full`; see [provider recovery](provider-recovery.md).

### `test_mode` — sandbox credentials

`test_mode = sandbox` is sandbox money with whatever behavior
`provider_write_mode` selects: every rail routes to its test environment, and
credential guarantees attach — a live Stripe key (`sk_live_`/`rk_live_`)
refuses to boot; each NMI account is probed when armed with one auth on the
canonical non-issued test PAN (only a simulator approves it — a decline
proves a live account and refuses the arm); CCBill uses the sandbox API host;
Solana derives devnet. Each NMI arm requires a fresh probe (manifest
reconciliation and the provider API alike); unavailable, indeterminate or live
responses refuse the arm, and an update that omits credentials re-probes the
stored key — a secret-backend failure cannot bypass it. Nothing caches a
verdict. Production mode does not run the sandbox probe.
Sandbox is allowed in every environment — what keeps it honest is
rail-credential validation (the live-key refusal and the NMI live-gateway
probe, which ask the credential itself), not the environment string.

The converse Stripe mismatch is also fatal in every environment: explicit
`test_mode = live` with an `sk_test_` or `rk_test_` key refuses boot instead of
silently disabling the configured rail.

### Cutover: booting against production credentials

Set the mode **before first start**. Imported `past_due` subscriptions are
immediately due for evaluation. With `full`, established NMI/Stripe accounts
first satisfy the automatic [provider recovery](provider-recovery.md) gate,
then eligible collection resumes without another operator action. That gate
does not establish ownership across independent copies; follow the controlled
transfer procedure above.

For a cutover that needs a manual review before collection, use
`PROVIDER_WRITE_MODE=limited` (customer writes permitted, system-origin writes
parked), or `readonly` (provider writes blocked, verified local recovery
continues). Neither explicit mode is automatically raised to `full`.

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
rotation") → `PROVIDER_WRITE_MODE=full` resumes eligible work after its current
policy and freshness checks pass. Paused
work is delayed, not lost; the workers are state-scan loops, so the first
enabled run processes whatever is outstanding. Missed billing periods are
never back-billed: dunning past the staleness window parks for verification instead of
charging, and a Solana subscription that skipped whole periods gets exactly
one pull anchored at the pull moment.

## The destructive-action kill switch and first-enforce gate

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
UPDATE billing.destructive_action_switch SET enabled = false,
       updated_by = 'you', reason = 'incident: mass cancellation observed';
```

No restart, no deploy, no scaling workers to zero. In-flight destructive intents
**park** (they are not failed), so flipping it back resumes them where they
stopped.

### Confirm it stopped

```sql
-- 1. the switch itself
SELECT enabled, updated_by, reason, updated_at FROM billing.destructive_action_switch;

-- 2. nothing has been canceled since the flip
SELECT count(*) FROM billing.subscriptions
 WHERE canceled_at > (SELECT updated_at FROM billing.destructive_action_switch);

-- 3. no product access has been revoked since the flip
SELECT count(*) FROM billing.product_access
 WHERE revoked_at > (SELECT updated_at FROM billing.destructive_action_switch);

-- 4. destructive provider intents are parked, not executing
SELECT status, count(*) FROM billing.provider_intents
 WHERE intent_type = 'nmi_delete_subscription' GROUP BY status;
```

Worker logs name the gate explicitly: `destructive actions gated — instance kill
switch is OFF`.

### Arming a merchant (the first-enforce gate)

A merchant with no `billing.merchant_destructive_policy` row — or one with
`enforce_armed_at IS NULL` — pulls in **advisory** mode: findings are persisted,
nothing is mutated, no source domain is proven, and `first_pull_completed_at` is
stamped so you know the survey is ready.

```sql
-- what did the first pull find?
SELECT finding_type, status, count(*) FROM billing.reconciliation_findings
 WHERE merchant_id = :merchant GROUP BY 1, 2 ORDER BY 3 DESC;

-- happy with it? arm the merchant for enforcing pulls
INSERT INTO billing.merchant_destructive_policy
       (merchant_id, destructive_actions_enabled, enforce_armed_at, updated_by, reason)
VALUES (:merchant, true, now(), 'you', 'reviewed first-pull findings')
ON CONFLICT (merchant_id) DO UPDATE
   SET enforce_armed_at = now(), destructive_actions_enabled = true;

-- and the instance switch (once, per deployment)
UPDATE billing.destructive_action_switch SET enabled = true, updated_by = 'you';
```

Both halves must be on: the instance switch gates the fleet, the merchant row
gates one merchant. Disabling either stops that merchant.

### Cancellation caps

Independently of the switch, one pass may cancel at most
`min(25, max(3, 5% of the merchant's live linked book))` subscriptions, or the
whole book when it holds at most 5 live subscriptions (a tiny book whose
schedules the provider ended must converge; no larger book is ever canceled
entirely by one pass). Over
that, **none** are applied, the merchant's pass halts, and a
`pull.cancellation.capped` finding lands in the review queue. It is all-or-
nothing on purpose: a pass that wants to cancel 850 customers is not a pass that
should cancel the first 25 of them.

```sql
SELECT subject_key, recommended_action, updated_at
  FROM billing.reconciliation_findings
 WHERE finding_type = 'pull.cancellation.capped' AND status = 'requires_review';
```

Investigate the roster before clearing it. The usual causes are a misdeclared
`psps.account_id`, a credential rotated onto a sibling sub-account, or a
provider incident returning a short page — never 850 customers all leaving.

## Per-merchant API hosts + browser CORS

Every merchant a standalone server serves needs its own canonical API
hostname, a single merchant's too: the public routes (catalog, checkout)
resolve their merchant only from the Host, and a Host no merchant answers to
is `404 merchant_not_found`. A DPoP proof names `auth.request_origin`, so
customers' browsers calling `/v1/me` must reach the server at that origin; a
proof for a merchant's API host on another origin is refused. Browser CORS is a **separate, fixed,
engine-wide policy**, not a per-merchant setting.

- **Configuring a merchant's host**: the operator binds it (the merchant
  manifest's `api_host`, or the server's `SetMerchantAPIHost`); a hosted
  product lets the owner claim and prove one (`ClaimMerchantAPIHost`, then
  `VerifyMerchantAPIHost`). It is
  `billing.merchants.api_host` (globally unique among live merchants),
  resolved LIVE on the next
  request; no boot-time host map, so a merchant configured on one node
  resolves immediately on every node sharing the database. Leave `api_host`
  unset for a merchant that should never resolve from any Host.
- **Local-dev hostnames**: `api_host` compares against the request Host with
  the port stripped, so
  `api_host = "api.acme.localhost"` resolves on any listen port — point
  `/etc/hosts` at `127.0.0.1` per name.
- **Reserved names**: `billing.ReservedMerchantSlugs` is the advisory list
  a hosted product should refuse to let a merchant self-provision as a slug
  (a slug commonly becomes `api.<slug>.<domain>`); the engine doesn't enforce
  it — the host does.
- **Webhook surface**: `<prefix>/v1/webhooks/{rail}/{account_id}` in embedded and
  standalone deployments. The provider account resolves its merchant in the
  configured environment; an explicit runtime merchant binding is enforced.
  Provider signature/source verification and matching payload identity remain
  required. Host headers do not choose callback authority.
- **Consistency with token issuers**: a JWT minted for merchant A's issuer is
  rejected when presented against merchant B's Host, even though the token
  verifies — Host-merchant must equal issuer-merchant on every
  merchant-scoped route. The check only fires when a Host actually resolved a
  merchant.

### Browser CORS doctrine

CORS protects requests authorized by an ambient credential (a cookie the
browser attaches automatically). OpenRails never issues cookies: every
browser-tier request carries an explicit bearer JWT placed by the page's own
JS, which a different origin's script cannot read; an unauthenticated
cross-origin call just 401s; a stolen token is replayed from `curl`, where
CORS doesn't exist. So a per-merchant origin allowlist would protect nothing.
The engine answers a **static, non-configurable** policy, by
route tier:

- **Checkout + self-service** (buyer-facing
  catalog/checkout, `/v1/me/*`, and their embedded
  equivalents) answer every preflight and response with
  `Access-Control-Allow-Origin: *`, the methods/headers those routes need,
  `WWW-Authenticate` and `DPoP-Nonce` exposed, and a 12h `Access-Control-Max-Age` — from ANY origin, zero configuration;
  `Access-Control-Allow-Credentials` is NEVER set. A merchant frontend calls
  OpenRails directly with no origin-registration step.
- **Every other surface** (admin console, platform directory,
  merchant/service API, inbound webhooks, control-plane auth) emits NO CORS
  headers at all — the correct, free posture for bearer-JWT curl/service
  callers.
- This is engine code, not a database column or config key, and it does not
  depend on `api_host` or Host resolution: OpenRails' CORS posture is not
  configurable.

## Payment-method update notices

A recoverable stored-card failure sends one `payment_method_update_required` customer notice and parks collection until the method is fixed; any follow-up is the host's. Retry/dunning, provider verification, stored-card account updates and paid-period access continue unchanged.
