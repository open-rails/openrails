# Operator Guide — running OpenRails day 2

Orientation for the platform operator / SRE running an OpenRails deployment.
Every section links into the deep references; this page is the map, not the
territory. The primary deep manual is [operations.md](operations.md).

### Infrastructure requirements

| Service | Required | What it does | Losing it |
|---|---|---|---|
| **Postgres 18+** | yes | Source of truth: double-entry money ledger, grant ledger, subscriptions, entitlements, catalog, the provider-intent ledger, and River's job queue. Can share an instance with your host app — OpenRails owns one schema (default `billing`) and its River's (default `billing_river`). | Data loss. Provider-owned facts (charges, remote subscription liveness) can be re-imported with `pull-provider`, but the ledger, credits, entitlements, and catalog are OpenRails-owned and exist nowhere else. **Back this up.** |
| **Redis-compatible service** (Garnet recommended) | optional | Shared route/admin rate limits, captcha escalation of card abuse, and admission-denial counters flushed to Postgres every 5 minutes. The atomic spending-admission gate and failed-usage grace and cutoff windows are in PostgreSQL. | Without Redis, rate limits, admin lockouts and captcha challenges are kept in PostgreSQL instead, still shared by every replica; the card-abuse captcha accelerator is off. Denial statistics are best-effort. No money path needs Redis. A declared Redis that stops answering costs speed, not correctness: requests count in PostgreSQL, readiness reports it degraded without failing, and boot never waits for it. |
| **HashiCorp Vault** | optional | Primary merchant-secret backend in production (`secret_backend: vault`), and/or Transit signing for Solana custody — two independent capabilities, grantable separately. See [vault.md](vault.md). | With an effective `secret_backend: db`, secrets live envelope-encrypted in `billing.merchant_secrets` instead. `encryption.master_key` / env `ENCRYPTION_MASTER_KEY` (base64, 32 bytes) is what encrypts them; construction refuses managed DB storage without encryption in both sandbox and live. Snapshot credentials stay in process memory. |

OpenRails' own JWT signing keys come from `AUTHKIT_KEYS_PATH/keys.json`
(file-watched, hot-rotating) or the inline `AUTHKIT_ACTIVE_KEY_ID` /
`AUTHKIT_ACTIVE_PRIVATE_KEY_PEM` / `AUTHKIT_PUBLIC_KEYS` envs, the same names
the authkit binary reads.
The same directory holds `totp.key`, the key for authenticator-app secrets.
The root owner always needs a second factor, so boot refuses a control plane
with neither `totp.key` nor an email or SMS sender.

Postgres specifics worth knowing:

- There is no row-level security. Merchant isolation is the explicit
  `merchant_id` (or `psp_id`) predicate on every tenant query, backed by
  composite foreign keys, so it does not depend on the login's flags. Run
  migrations and the server as the same role: it owns what it creates. The
  few cross-merchant reads (webhook routing by PSP, the hosted portal's
  merchant list, fleet aggregates) are ordinary sqlc queries that return ids
  or aggregates only.
- Security defaults are independent of payment posture. Managed database secrets
  always require encryption. Local issuer/signing/sender exceptions are explicit
  Auth settings; see [runtime configuration](runtime-configuration.md). `ENV` is
  retired and refuses loading rather than silently selecting a weaker posture.
- Migrations: the server and worker apply AuthKit, River, and OpenRails
  migrations at boot, before anything else touches the database; replicas
  booting together take turns, and a failed migration fails the boot.
  `openrails migrate up` applies OpenRails' and River's ahead of a rollout, and
  `openrails migrate status` reports the ledger against the embedded chain.
  An older release boots against a database a newer one migrated.
  Every v1.x release upgrades a database of any earlier v1 release in place
  ([compatibility](compatibility.md#database-schema)); a pre-v1 database is not
  upgraded ([migrating to v1](migrating-to-v1.md)).
- Two apps sharing one billing schema connect as one role, or `SET ROLE` to a
  shared one on every connection: that role owns every object.
- Local zero-config stack: `task docker-up` (Postgres 18 + Redis + OpenRails on
  `:3053`), `task docker-down` to tear down, `task docker-reset` to recreate the
  database from empty.

### The safety levers

Two orthogonal settings, both settable as yaml / env / CLI flag (flag beats env
beats yaml). Full detail and semantics: [operations.md → "Operating
modes"](operations.md#operating-modes-the-safety-levers).

- **`provider_write_mode`** (`PROVIDER_WRITE_MODE`, `--provider-write-mode`) —
  the behavior dial: `full | limited | readonly`. Required explicitly
  (boot refuses without it); unset fail-closes to `readonly` wherever it is
  consulted. `limited` = humans can do everything (checkout, cancel, refund),
  the system initiates nothing; `readonly` = nothing writes to a provider at
  all, wire-enforced. Provider reads and verified local financial recovery
  continue in every mode.
- **`test_mode`** (`TEST_MODE`, `--test-mode`) — the credential axis:
  `sandbox | live`; required explicitly with no implicit posture default. Sandbox routes every rail to
  its test environment and refuses live credentials at boot (live Stripe keys
  rejected, NMI accounts probed with a test card). Live posture likewise
  refuses Stripe test keys in every environment instead of silently disabling
  the rail, so no real money can move under sandbox regardless of write mode.

| Operation | `full` | `limited` | `readonly` |
|---|---|---|---|
| Real money can move | yes | yes | no |
| User checkout / charge / refund / cancel | yes | yes | no (fails loudly) |
| Dunning charges + expiry cancellations | yes | dry-run, intents parked | no |
| Invoice collection, Solana pulls | yes | no | no |
| Catalog provider-object writes | yes | deferred | deferred |
| Provider reads + webhook ingestion | yes | yes | yes |

For established NMI and Stripe accounts, stale completed provider coverage or
unresolved financial conflicts also hold new collection and destructive work.
Configured `full` resumes eligible work automatically after verified catch-up;
explicit `readonly` stays readonly. This automatic gate does not cover CCBill
or Solana and does not establish ownership across separate database copies.
See [provider recovery](provider-recovery.md).

### Routine operation

Everything below runs by itself under River once the server (or a `run-worker`
process) is up. Provider-owned schedules bill at the provider and report the
result. OpenRails-owned NMI/Stripe agreements use the shared-database due
worker and accepted collection operations; Solana is pulled by its crank.
Ownership determines which system may initiate the renewal.

| Worker | Cadence | Job |
|---|---|---|
| Provider-intent executor | 1 min (+ on start) | drains due outbound provider mutations from the intent ledger |
| Provider-intent verifier | 5 min | resolves `unknown_needs_verify` outcomes by *reading* the provider before any retry |
| Convergence Engine sweep | 15 min (+ on start) | per-merchant internal-drift repair: stalled dunning, lapsed periods, unmaterialized grants ([operations.md](operations.md#the-convergence-engine)) |
| Provider Refresh | 4 h (+ on start) | watermarked missed-event backfill, unknown-cohort reconcile, CCBill DataLink refresh — reads only, never mutates a provider |
| Due / dunning | 1 min | admits due engine renewals and retries `past_due` per the derived schedule; parks whole missed periods for verification instead of charging ([operations.md → Dunning](operations.md#dunning)) |
| Credit expiry | 1 h | expires credit lots |
| Solana crank | 1 h | executes due on-chain subscription pulls |
| Cleanup / invoices | 1 h – daily | expired-data cleanup, invoice collection + period finalization |
| Worker health check | 5 min | seeds `billing.worker_state`, raises a critical notification in the merchant inbox when a kind stops completing |

**Health endpoint**: `GET /health/live` (liveness) and `GET /health/ready`
(readiness; the failing dependency is logged, never answered). Readiness requires only
Postgres, the merchants service, the River producer, a locally managed River
consumer and auth; Redis, Vault and PSP posture are reported as `degraded`
from cached background state and never fail it. A standalone
`run-server --no-workers` process remains live but not ready because it has no
local job consumer. It still binds its request-side River producers before HTTP
starts; a separate `run-worker` process contributes both billing and AuthKit
lifecycle workers using the same database, issuer and manifest configuration.
Embedded hosts wire the dependency checks into their own
handler with `client.Ready(ctx)` and register `client.Probes()` as optional
dependencies with their supervisor; a host fleet passed to `Start` with
`WithRiverClient` is watched by the `openrails_job_progress` probe because its
process state is outside OpenRails.

The standalone server serves `GET /metrics` on its private listener
(`private_port`; `server.Config.PrivateAddr`, or mount `PrivateHandler`
yourself), never on the public port: one gauge per dependency,
`openrails_dependency_up{dependency,class}` (class `required` or `optional`;
alert on optional ones at 0 as degraded), and per job kind
`openrails_worker_consecutive_failures{kind}` and
`openrails_worker_last_success_timestamp_seconds{kind}`. Beyond that there is
no runtime telemetry endpoint.
`/v1/admin/metrics`, `/query`, and `/schema` are authenticated merchant
business analytics, not process/runtime metrics.

**Healthy looks like**: `openrails intents` shows a near-empty active set (the
sweep flags `pending` older than 24h and `in_flight`/`unknown` older than 2h as
findings), `pull-provider report` shows no open findings, and worker-health
alerts are quiet.

### Merchants and administrators

The merchant directory and administrator lockouts are the operator's, with no
HTTP route: the `openrails` CLI over the server's Go methods.

```bash
openrails merchants list [--status active|deleted|all] [--query acme] [--json]
openrails merchants get acme                    # an id, or a live merchant's name
openrails merchants create acme --display-name Acme [--owner-user-id <uuid>]
openrails merchants rename acme acme-shop       # the former name forwards (auth.naming)
openrails merchants delete <merchant-id>        # soft: off the lists, credentials resolve nothing, records kept
openrails merchants restore <merchant-id>
openrails admin-lockouts unlock <user-id>       # every replica's lockout, in Redis or PostgreSQL
```

### When things drift

The operator's toolbox — all read-only or plan-only by default. Mutation flags
follow one contract everywhere: no flags = plan/report; `--insert` creates
missing, `--overwrite` updates existing, `--prune` removes extras
([operations.md → Mutation Flags](operations.md#mutation-flags)).

| Command | What it does |
|---|---|
| `openrails pull-provider --merchant=<slug>` | pull provider-observed truth, diff against the local mirror, write nothing. Add `--insert/--overwrite/--prune` to converge local state; the remote rails are **never** mutated. Filters: `--rail`, `--psp`, `--since/--until`, `--format table\|json`. |
| `openrails pull-provider report --merchant=<slug> [--run=ID]` | render a run's summary, standing open findings, and the dunning-forensics report |
| `openrails nmi decline-report --merchant=<slug> --since=<date> [--until] [--psp] [--format table\|json]` | read-only decline baseline from an NMI account's history: approval and refusal rates of card verifications, one-off sales and NMI-scheduled rebills by month, and each refusal's reason and category from OpenRails' classifier. Writes nothing. History cannot say who sent a one-off sale, so rebill retries are not separated. The history job stores the same numbers daily for Payments → Health. |
| `openrails intents [--status=…] [--rail=…] [--type=…] [--merchant=…]` | list the provider-intent ledger: queued outbound mutations, each row's `executes_under` mode, and the drain forecast |
| `openrails intents-log [--rail=…] [--intent=…] [--phase=…]` | append-only log of actual provider mutation attempts/results (the executor's audit trail) |
| `openrails intents resolve --merchant=… --intent=… [--step=…] (--receipt=<provider id> \| --not-executed) --actor=… --reason=…` | close an `unknown_needs_verify` operation from an exact provider receipt (read back and matched) or provider-supported non-execution (an empty NMI search is never enough); `--not-executed` also releases a `pending` invoice collection that never crossed its submission fence; never resends ([provider uncertainty](provider-uncertainty.md)) |
| `openrails apply-catalog --merchant NAME --file PATH [--force-conflicts]` | apply one catalog document, safe on every boot: an object whose field an edit set differently is skipped and listed, and the command exits non-zero; `--force-conflicts` overwrites it. Prune is explicit in the document ([catalog ownership](catalog-ownership.md)) |
| `openrails push-merchant-config` / `push-auth-bootstrap` | provision merchant/provider configuration or AuthKit authority under their own command contracts |

`pull-provider` is manual-only by design — never scheduled. Routine catch-up is
Provider Refresh's job; `pull-provider` is the full-surface investigation tool
with the findings ledger and forensics
([operations.md → Provider Pull](operations.md#provider-pull)).

**The findings queue doctrine**: findings that require judgment (remote
mutations, ambiguous identity matches) land in the admin queue and **never
auto-fire** — a human acknowledges them, and raising the write mode does
nothing to this queue by design. Findings have stable identity across runs and
auto-resolve when the divergence vanishes. Taxonomy:
[operations.md — The Convergence Engine](operations.md#the-convergence-engine).

### Cutover / migration boots

Set the mode **before first start**. With `full`, an established NMI/Stripe
book catches up through the automatic provider recovery gate before eligible
collection resumes. Stop the source writers before activating a restored
database; see [backup and recovery](backup-and-recovery.md#restore-procedure).
For a cutover requiring manual review before system-origin collection:

1. Boot with `PROVIDER_WRITE_MODE=limited` (site fully usable; system-origin
   writes park) — or `readonly` to block all provider writes while permitting
   verified local recovery.
2. Let provider reads catch up. In `limited`, eligible dunning work materializes
   as parked intents and whole missed periods park for verification. Dunning
   does not cancel locally in this mode. In `readonly`, it only observes due work;
   provider receipt recovery runs separately.
3. `openrails intents --merchant=<slug>` shows the real drain forecast
   ("N execute under limited, M require full"). Resolve anything you do not
   want to fire.
4. `openrails pull-provider report --merchant=<slug>` — review the human-judgment
   findings queue. Resolve financial conflicts from evidence; dismissing a
   finding does not satisfy the recovery gate.
5. Raise to `PROVIDER_WRITE_MODE=full`: eligible work resumes after its current
   policy and freshness checks pass. Missed whole periods are never back-billed.

Full sequence and rationale: [operations.md →
Cutover](operations.md#cutover-booting-against-production-credentials).

### Secrets & credential rotation

- **Backends**: `secret_backend: db` (envelope-encrypted in Postgres under
  `ENCRYPTION_MASTER_KEY`) or `secret_backend: vault` (KV-v2). Managed DB storage requires encryption even in sandbox. Snapshot custody keeps
  host-owned values in memory. Declared, never auto-detected, never inferred from
  a Vault connection, never silently falls back. Vault setup + minimal
  policies: [vault.md](vault.md); per-merchant secret ops, canonical names,
  and the DB→Vault migration runbook:
  [vault.md](vault.md).
- **Naming**: addressed as `(merchant_id, name)` in code; Vault path
  `secret/openrails/merchants/<merchant-uuid>/<name>`. Published references select exact validated versions. Direct backend edits do
  not publish a new active credential. Managed publication does not require restarting the runtime.
- **Rotation within the same PSP** uses `Client.UpdatePSP`
  (`PATCH /v1/admin/psps/{id}`) with a stable operation ID and the
  expected revision. A candidate is
  staged and account/environment validated before its exact version is published.
  Retry the same operation to recover a lost response. A failed publication leaves
  the previous published credentials active; an unpublished candidate is not read
  merely because it is newer in Vault.
- **Pointing credentials at a different account** trips the account guard:
  every provider intent is stamped with the PSP row it was
  enqueued against, and the executor parks intents whose account no longer
  resolves — a queue built against one account never executes against another.
  Options: restore the old account's credentials so the queue drains, or let
  stale intents expire/supersede. Rules:
  [operations.md → Durability model](operations.md#durability-model).

### Processor routing

Checkout is one processor at a time, chosen **before** the session exists. A request that
names a PSP (`payment.psp`, its key) gets that PSP; a request that names none is routed.

- **Default** (no policy declared): stripe → nmi → ccbill → solana, first one that can
  serve the price.
- **Policy**: the `checkout_routing` merchant setting, declared under `settings:`
  in the merchant's YAML or applied through a configuration application. Ordered
  rules, first match wins; each rule's `prefer` list is both the ranking and the
  whitelist, so a rule can pin a product to one rail. Conditions: `currency`, `product`,
  `price`, `mode`, `country` — all optional, all AND-ed; a rule with no conditions is the
  catch-all and must be last.

```yaml
checkout_routing:
  - match: { currency: EUR, mode: subscription }
    prefer: [ccbill, mobius]
  - prefer: [mobius, ccbill, solana]
```

- **Fallback** is availability-only and evaluated pre-charge: `not_armed`,
  `credentials_missing`, `link_missing`, `mode_unsupported`, `service_unavailable`,
  `ambiguous_selector`, `unknown_selector`, `resolve_failed`. A **decline is not one of
  them** — routing never retries a charge on a second processor.
- **Why did this customer get CCBill?** `checkout_attempts.routing_reason` holds the
  decision: policy, matched rule, winner, ranked fallbacks, and every skipped candidate
  with its class. Written once at creation, never rewritten.
- **Preview without charging**: `POST /v1/admin/psps/routing-preview`
  with `{"price_id": "...", "country": "US"}` returns the same decision a real session
  would make, including the exact `routing_reason` it would store.

### Observability

- **Metrics / analytics API**: `GET /v1/admin/metrics/schema` (self-describing
  measure/dimension registry) + `POST /v1/admin/metrics/query` — aggregate-only,
  scoped to the API key's merchant, designed to be driven by an LLM agent.
  [metrics-for-llms.md](metrics-for-llms.md).
- **Logs**: structured logrus to stdout; level via `logger.level` in
  config.yaml. Every `pull-provider` local write is logged with finding id +
  evidence; `openrails intents-log` is the durable audit trail of provider
  mutations. Provider Refresh logs a per-pass heartbeat and per-merchant
  reconcile summary.
- **Worker health**: `billing.worker_state` rows per job kind; the 5-minute
  checker raises a critical merchant notification when a periodic kind stops
  completing. `openrails workers` prints each kind's last success, failure
  streak and last error (`--json` for the full text).
- **Notifications**: reconciliation findings raise deduplicated console
  notifications and, by severity, outbound webhooks / the alert email
  ([merchant-notifications.md](merchant-notifications.md)).
