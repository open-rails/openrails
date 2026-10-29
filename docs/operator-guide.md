# Operator Guide — running OpenRails day 2

Orientation for the platform operator / SRE running an OpenRails deployment.
Every section links into the deep references; this page is the map, not the
territory. The primary deep manual is [operations.md](operations.md).

### Infrastructure requirements

| Service | Required | What it does | Losing it |
|---|---|---|---|
| **Postgres 18+** | yes | Source of truth: double-entry money ledger, grant ledger, subscriptions, entitlements, catalog, the provider-intent ledger, and River's job queue. Can share an instance with your host app — OpenRails owns one schema (default `billing`) and its River's (default `billing_river`). | Data loss. Provider-owned facts (charges, remote subscription liveness) can be re-imported with a PSP import, but the ledger, credits, entitlements, and catalog are OpenRails-owned and exist nowhere else. **Back this up.** |
| **Redis-compatible service** (Garnet recommended) | optional for one instance, required for several | Rate limits, admin lockouts, captcha challenges and card-testing declines shared by every instance, and admission-denial counters flushed to Postgres hourly, once each hour closes. The atomic spending-admission gate and failed-usage grace and cutoff windows are in PostgreSQL. | Without Redis, rate limits, admin lockouts, captcha challenges and card-testing declines live in the process's memory: right for one instance, while several would each allow the full limit. They are never in PostgreSQL. Denial statistics are best-effort. No money path needs Redis. A declared Redis that stops answering costs sharing, not service: each instance keeps them in its own memory, logs an error, counts `openrails_abuse_state_fallbacks_total`, readiness reports Redis degraded without failing, and boot never waits for it. |
| **HashiCorp Vault** | optional | Name a KV mount (`vault.kv_mount`) and Vault holds every merchant's configuration, credentials included; name only a Transit mount and it signs for Solana. See [vault.md](vault.md). | Without a KV mount the merchant manifest is the configuration, read-only. With one, startup needs Vault; afterwards an outage is ridden out from each replica's cache. |

OpenRails' own JWT signing keys come from `keys.json` in `auth.keys.path`
(`AUTH_KEYS_PATH`; file-watched, hot-rotating) or the inline `AUTHKIT_ACTIVE_KEY_ID` /
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
  consulted. `limited` = humans can do everything (purchase, cancel, refund),
  the system initiates nothing; `readonly` = nothing writes to a provider at
  all, wire-enforced. Provider reads and verified local financial recovery
  continue in every mode.
- **PSP `environment`** — declared on every PSP in the merchant manifest
  (`live | test | sandbox`) and verified with the provider before the PSP is
  used ([PSP environments](psp-environments.md)). A merchant mixes live and
  test PSPs freely; a test or sandbox PSP moves no real money in any write
  mode.

| Operation | `full` | `limited` | `readonly` |
|---|---|---|---|
| Real money can move | yes | yes | no |
| User purchase / charge / refund / cancel | yes | yes | no (fails loudly) |
| Dunning charges + expiry cancellations | yes | dry-run, intents parked | no |
| Usage order collection, Solana pulls | yes | no | no |
| Catalog provider-object writes | yes | deferred | deferred |
| Provider reads + webhook ingestion | yes | yes | yes |

Reconciliation findings and bulk-observation age do not block normal operations.
Each operation retains its own ownership, provider, chargeability and receipt
checks. Explicit `readonly` and `limited` modes still apply. See
[provider observation](provider-recovery.md).

### Routine operation

Everything below runs by itself under River once the server (or a `run-worker`
process) is up. Provider-owned schedules bill at the provider and report the
result. OpenRails-owned NMI/Stripe agreements use the shared-database due
worker and accepted collection operations; each Solana period is pulled by the
operation its renewal scheduled.
Ownership determines which system may initiate the renewal.

| Worker | Cadence | Job |
|---|---|---|
| Provider operation | its own job, at each attempt | runs one outbound provider mutation from the intent ledger (Solana pulls included, scheduled at each due period); resolves an `unknown_needs_verify` outcome by *reading* the provider before any retry |
| Due / dunning | 1 min | admits due engine renewals and retries `past_due` per the derived schedule; parks whole missed periods for verification instead of charging ([operations.md → Dunning](operations.md#dunning)) |
| Customer convergence | with each mutation | re-derives the changed customer's state in a job committed with the mutation ([operations.md](operations.md#the-convergence-engine)) |
| Merchant health | hourly | health findings and timed access lapses, for merchants with activity ([operations.md](operations.md#the-convergence-engine)) |
| Provider Refresh | 2 h (+ on start) | watermarked missed-event backfill, unknown-cohort reconcile — reads only, never mutates a provider |
| Retention cleanup | 1 h | deletes rows past their retention ([operations.md → Data retention](operations.md#data-retention)) |
| Usage orders (opt-in) | 5 min | bills due usage accounts and queues due collections as operations ([usage billing](usage-billing.md)) |
| Arrears delinquency | 1 min | evaluates customers whose usage order fell due or whose grace ran out |

The full list: [operations.md → Background worker schedule](operations.md#background-worker-schedule).

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
handler with the engine's `Ready(ctx)` and register its `Probes()` as optional
dependencies with their supervisor; a host fleet passed to `Start` with
`WithRiverClient` is watched by the `openrails_job_progress` probe because its
process state is outside OpenRails.

The standalone server serves `GET /metrics` on its private listener
(`private_port`; `server.Config.PrivateAddr`, or mount `PrivateHandler`
yourself), never on the public port: one gauge per dependency,
`openrails_dependency_up{dependency,class}` (class `required` or `optional`;
alert on optional ones at 0 as degraded), and per job kind, read from River's
tables, `openrails_job_overrunning{kind}`, `openrails_job_retryable{kind}`,
`openrails_job_discarded{kind}` and
`openrails_job_last_completed_timestamp_seconds{kind}`. Beyond that there is
no runtime telemetry endpoint.
`/v1/metrics`, `/query`, and `/schema` are authenticated merchant
business analytics, not process/runtime metrics.

**Healthy looks like**: `openrails intents` shows a near-empty active set (the
sweep flags `pending` older than 24h and `in_flight`/`unknown` older than 2h as
findings), the findings queue (`GET /v1/merchant/findings`) holds nothing open, and
`openrails workers` shows no overrunning or discarded jobs.

### Merchants and administrators

The merchant directory and administrator lockouts are the operator's, with no
HTTP route: the `openrails` CLI over the server's Go methods.

```bash
openrails merchants list [--status active|deleted|all] [--query acme] [--json]
openrails merchants get acme                    # an id, or a live merchant's name
openrails merchants create acme --display-name Acme [--owner-user-id <uuid>]
openrails merchants rename acme acme-shop       # the former name forwards (naming)
openrails merchants delete <merchant-id>        # soft: off the lists, credentials resolve nothing, records kept
openrails merchants restore <merchant-id>
openrails admin-lockouts unlock <user-id>       # the lockout in Redis (without it, it lives in the serving process)
```

### When things drift

The operator's toolbox — all read-only or plan-only by default.

| Command | What it does |
|---|---|
| `openrails prune --merchant=<slug> [--psp=<id\|key>] [--expect-rows=N]` | soft-delete a PSP's local subscriptions and payments its provider no longer lists: a plan without `--expect-rows`, reversible with `openrails undo-run`; the provider is never written ([operations.md → Prune](operations.md#prune)) |
| `openrails proposals list --merchant=<slug>` | the changes provider reads propose, for a person to approve or dismiss ([operations.md → Proposals](operations.md#proposals)) |
| `openrails nmi decline-report --merchant=<slug> --since=<date> [--until] [--psp] [--format table\|json]` | read-only decline baseline from an NMI account's history: approval and refusal rates of card verifications, one-off sales and NMI-scheduled rebills by month, and each refusal's reason and category from OpenRails' classifier. Writes nothing. History cannot say who sent a one-off sale, so rebill retries are not separated. The history job stores the same numbers daily for Payments → Health. |
| `openrails intents [--status=…] [--rail=…] [--type=…] [--merchant=…]` | list the provider-intent ledger: queued outbound mutations, each row's `executes_under` mode, and the drain forecast |
| `openrails intents-log [--rail=…] [--intent=…] [--phase=…]` | append-only log of actual provider mutation attempts/results (the executor's audit trail) |
| `openrails intents resolve --merchant=… --intent=… [--step=…] (--receipt=<provider id> \| --not-executed) --actor=… --reason=…` | close an `unknown_needs_verify` operation from an exact provider receipt (read back and matched) or provider-supported non-execution (an empty NMI search is never enough); `--not-executed` also releases a `pending` invoice collection that never crossed its submission fence; never resends ([provider uncertainty](provider-uncertainty.md)) |
| `openrails catalog apply PATH --merchant NAME [--force-conflicts]` | apply one catalog document and print what changed; applying it again changes nothing. A refused document, or an object whose field an edit set differently (skipped and listed), exits non-zero; `--force-conflicts` overwrites it. Prune is explicit in the document ([catalog ownership](catalog-ownership.md)). Starting the server never applies a catalog |
| `openrails push-merchant-config` / `push-auth-bootstrap` | provision merchant/provider configuration or AuthKit authority under their own command contracts |

Routine catch-up is Provider Refresh's job. To re-read a PSP now, create a
manual PSP import (`POST /v1/merchant/psp-imports`); its differences become
proposals ([PSP import](psp-import.md)).

**The findings queue doctrine**: findings that require judgment (remote
mutations, ambiguous identity matches) land in the admin queue and **never
auto-fire** — a human acknowledges them, and raising the write mode does
nothing to this queue by design. Findings have stable identity across runs and
auto-resolve when the divergence vanishes. Taxonomy:
[operations.md — The Convergence Engine](operations.md#the-convergence-engine).

### Cutover / migration boots

Set the mode **before first start**. With `full`, eligible collection runs
under its ordinary operation-specific checks; bulk reconciliation is separate. Stop the source writers before activating a restored
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
4. `GET /v1/merchant/findings` and `openrails proposals list --merchant=<slug>` —
   review the human-judgment findings queue. Resolve financial conflicts from evidence; dismissing a
   finding does not satisfy the recovery gate.
5. Raise to `PROVIDER_WRITE_MODE=full`: eligible work resumes after its current
   policy and freshness checks pass. Missed whole periods are never back-billed.

Full sequence and rationale: [operations.md →
Cutover](operations.md#cutover-booting-against-production-credentials).

### Secrets & credential rotation

- **Where they live**: a PSP's credentials are part of the merchant's
  configuration: the merchant manifest, or with `vault.kv_mount` the PSP's
  Vault document (paths, shapes and minimal policies: [vault.md](vault.md)).
  Postgres holds no credential.
- **Rotation within the same PSP**: with Vault, `Client.UpdatePSP`
  (`PATCH /v1/merchant/psps/{id}`) with the expected revision; the new credential
  is checked with the provider before it is written, and every replica reloads
  at once. With a manifest, change the file and restart.
- **Declaring a PSP key with a different account** registers a new PSP.
  Every provider intent is stamped with the PSP row it was enqueued against,
  so a queue built against one account never executes against another: the
  old account's intents wait without credentials. Restore its declaration so
  the queue drains, or let stale intents expire or be superseded. There is no
  check that a credential belongs to the declared account. Rules:
  [operations.md → PSP binding](operations.md#psp-binding-and-credential-rotation).

### Processor routing

Purchase is one processor at a time, chosen **before** the session exists. A request that
names a PSP (`payment.psp`, its key) gets that PSP; a request that names none is routed.

- **Default** (no policy declared): stripe → nmi → ccbill → solana, first one that can
  serve the price.
- **Policy**: the `psp_routing` merchant setting, declared under `settings:`
  in the merchant's YAML or set through `PATCH /v1/merchant/configuration`. Ordered
  rules, first match wins; each rule's `prefer` list is both the ranking and the
  whitelist, so a rule can pin a product to one rail. Conditions: `currency`, `product`,
  `price`, `mode` — all optional, all AND-ed; a rule with no conditions is the
  catch-all and must be last.

```yaml
psp_routing:
  - match: { currency: EUR, mode: subscription }
    prefer: [ccbill, mobius]
  - prefer: [mobius, ccbill, solana]
```

- **Fallback** is availability-only and evaluated pre-charge: `not_armed`,
  `credentials_missing`, `link_missing`, `mode_unsupported`, `service_unavailable`,
  `ambiguous_selector`, `unknown_selector`, `resolve_failed`. A **decline is not one of
  them** — routing never retries a charge on a second processor.
- **Why did this customer get CCBill?** `order_attempts.routing_reason` holds the
  decision: policy, matched rule, winner, ranked fallbacks, and every skipped candidate
  with its class. Written once at creation, never rewritten.
- **Preview without charging**: `POST /v1/merchant/psps/routing-preview`
  with `{"price_id": "..."}` returns the same decision a real session
  would make, including the exact `routing_reason` it would store.

Country conditions are rejected in routing configuration and previews because an order
does not have a country when routing chooses its provider. Before deploying this
validation, check existing merchant settings and explicitly replace any country rules.
Billing address country is still accepted for payments; it does not control routing.

### Observability

- **Metrics / analytics API**: `GET /v1/metrics/schema` (self-describing
  measure/dimension registry) + `POST /v1/metrics/query` — aggregate-only,
  scoped to the API key's merchant, designed to be driven by an LLM agent.
  [metrics-for-llms.md](metrics-for-llms.md).
- **Logs**: structured logrus to stdout; level via `logger.level` in
  config.yaml. Every local write a pull applies is logged with finding id +
  evidence; `openrails intents-log` is the durable audit trail of provider
  mutations. Provider Refresh logs a per-pass heartbeat and per-merchant
  reconcile summary.
- **Job health**: read from River's own tables, never stored by OpenRails.
  `openrails workers` prints each job kind's jobs running past their timeout,
  waiting to retry and discarded, a periodic kind's last completion and the
  newest error (`--json` for the full text). The `openrails_job_progress`
  probe fails while the River schema has no leader, a job runs past its
  timeout, or a periodic pass the fleet queues has not completed within two
  periods and its timeout. Stalls are an operator signal, never a merchant
  notification.
- **Notifications**: reconciliation findings raise deduplicated console
  notifications and, by severity, outbound webhooks / the alert email
  ([merchant-notifications.md](merchant-notifications.md)).
