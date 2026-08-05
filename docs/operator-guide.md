# Operator Guide — running OpenRails day 2

Orientation for the platform operator / SRE running an OpenRails deployment.
Every section links into the deep references; this page is the map, not the
territory. The primary deep manual is [operations.md](operations.md).

### Infrastructure requirements

| Service | Required | What it does | Losing it |
|---|---|---|---|
| **Postgres 18+** | yes | Source of truth: double-entry money ledger, grant ledger, subscriptions, entitlements, catalog, the provider-intent ledger, and River's job queue. Can share an instance with your host app — OpenRails owns the `openrails` schema. | Data loss. Provider-owned facts (charges, remote subscription liveness) can be re-imported with `pull-provider`, but the ledger, credits, entitlements, and catalog are OpenRails-owned and exist nowhere else. **Back this up.** |
| **Redis-compatible service** (Garnet recommended) | optional | Rate-limit buckets (per-IP / per-user), the atomic usage-billing admission gate (spendgate), card-abuse tracking, and hourly admission-denial aggregates (flushed to Postgres every 5 minutes). | Rate limiting degrades to per-process in-memory counters (logged, automatic). Redis holds only transient counters — nothing durable. |
| **HashiCorp Vault** | optional | Primary merchant-secret backend in production (`secret_backend: vault`), and/or Transit signing for Solana custody — two independent capabilities, grantable separately. See [vault.md](vault.md). | With `secret_backend: db` (the default), secrets live envelope-encrypted in `openrails.merchant_secrets` instead. `encryption.master_key` / env `ENCRYPTION_MASTER_KEY` (base64, 32 bytes) is what encrypts them — without it the DB store is plaintext (loud warning; refused outside development for API-managed merchants). |

Postgres specifics worth knowing:

- RLS is enforced for the unprivileged `openrails_app` role (`NOLOGIN NOBYPASSRLS`,
  created by the baseline migration); every merchant-scoped table has a
  `merchant_isolation` policy keyed on the `app.merchant_id` GUC. Run MIGRATIONS
  as a superuser and the SERVER as `openrails_app` — in EVERY environment,
  local development included. The server refuses to boot on a role that
  bypasses RLS, because a privileged role does not just disable isolation, it
  hides bugs: an unscoped read of an RLS-forced table returns zero rows with no
  error, so the component logs success and does nothing. Also, the cross-merchant
  directory functions (webhook routing by provider account, the hosted portal's
  merchant list) are `SECURITY DEFINER` — they need an owner that can read
  across merchants, and they raise rather than return an empty result if it
  cannot.
- `ENV` is REQUIRED and has no default. It decides whether merchant secrets may
  be stored plaintext and whether the DB role must enforce RLS, so an
  undeclared environment refuses to boot instead of quietly meaning
  "development".
- Migrations: `openrails migrate up` applies AuthKit, River, and OpenRails
  migrations (`migrations/postgres/`, baseline `0001_schema.up.sql`, new ones
  start at `0002`). The server validates at boot and refuses to start behind.
- Local zero-config stack: `task docker-up` (Postgres 18 + Redis + OpenRails on
  `:3053`), `task docker-down` to tear down.

### The safety levers

Two orthogonal settings, both settable as yaml / env / CLI flag (flag beats env
beats yaml). Full detail and semantics: [operations.md → "Operating
modes"](operations.md#operating-modes-the-safety-levers).

- **`provider_write_mode`** (`PROVIDER_WRITE_MODE`, `--provider-write-mode`) —
  the behavior dial: `full | limited | readonly`. Required outside development
  (boot refuses without it); unset fail-closes to `readonly` wherever it is
  consulted. `limited` = humans can do everything (checkout, cancel, refund),
  the system initiates nothing; `readonly` = nothing writes to a provider at
  all, wire-enforced.
- **`test_mode`** (`TEST_MODE`, `--test-mode`) — the credential axis:
  `sandbox | live`. Sandbox routes every rail to its test environment and
  refuses live credentials at boot (live Stripe keys rejected, NMI accounts
  probed with a test card), so no real money can move regardless of write mode.

| Operation | `full` | `limited` | `readonly` |
|---|---|---|---|
| Real money can move | yes | yes | no |
| User checkout / charge / refund / cancel | yes | yes | no (fails loudly) |
| Dunning charges + expiry cancellations | yes | dry-run, intents parked | no |
| Auto-top-ups, arrears collection, Solana pulls | yes | no | no |
| Catalog provider-object writes | yes | deferred | deferred |
| Provider reads + webhook ingestion | yes | yes | yes |

### Routine operation

Everything below runs by itself under River once the server (or a `run-worker`
process) is up. The rails themselves do the recurring billing (NMI/CCBill/Stripe
bill provider-side and webhook the result; Solana is pulled by our crank);
OpenRails' workers converge state around that:

| Worker | Cadence | Job |
|---|---|---|
| Provider-intent executor | 1 min (+ on start) | drains due outbound provider mutations from the intent ledger |
| Provider-intent verifier | 5 min | resolves `unknown_needs_verify` outcomes by *reading* the provider before any retry |
| Convergence Engine sweep | 15 min (+ on start) | per-merchant internal-drift repair: stalled dunning, lapsed periods, unmaterialized grants ([operations.md](operations.md#the-convergence-engine)) |
| Provider Refresh | 4 h (+ on start) | watermarked missed-event backfill, unknown-cohort reconcile, CCBill DataLink refresh — reads only, never mutates a provider |
| Dunning | 4 h | retries `past_due` per the derived no-knobs schedule; cancels past the staleness window instead of charging ([operations.md → Dunning](operations.md#dunning-359)) |
| Credit expiry | 1 h | expires credit lots |
| Solana crank | 1 h | executes due on-chain subscription pulls |
| Cleanup / invoices / alerts / digests | 1 h – daily | expired-data cleanup, invoice collection + finalization, metric alert eval, findings digest |
| Worker health check | 5 min | seeds `openrails.worker_health`, raises repair alerts when a kind stops completing |

**Health endpoint**: `GET /health/live` (liveness) and `GET /health/ready`
(readiness; `?verbose=1` adds per-dependency detail — DB, Redis, auth). K8s
aliases `/healthz` / `/readyz`. Embedded hosts wire the same checks into their
own handler via `Embedded.Ready`.

**Healthy looks like**: `openrails intents` shows a near-empty active set (the
sweep flags `pending` older than 24h and `in_flight`/`unknown` older than 2h as
findings), `pull-provider report` shows no open findings, and worker-health
alerts are quiet.

### When things drift

The operator's toolbox — all read-only or plan-only by default. Mutation flags
follow one contract everywhere: no flags = plan/report; `--insert` creates
missing, `--overwrite` updates existing, `--prune` removes extras
([operations.md → Mutation Flags](operations.md#mutation-flags)).

| Command | What it does |
|---|---|
| `openrails pull-provider --merchant=<slug>` | pull provider-observed truth, diff against the local mirror, write nothing. Add `--insert/--overwrite/--prune` to converge local state; the remote rails are **never** mutated. Filters: `--rail`, `--provider-account`, `--since/--until`, `--format table\|json`. |
| `openrails pull-provider report --merchant=<slug> [--run=ID]` | render a run's summary, standing open findings, and the dunning-forensics report |
| `openrails intents [--status=…] [--rail=…] [--type=…] [--merchant=…]` | list the provider-intent ledger: queued outbound mutations, each row's `executes_under` mode, and the drain forecast |
| `openrails intents-log [--rail=…] [--intent=…] [--phase=…]` | append-only log of actual provider mutation attempts/results (the executor's audit trail) |
| `openrails push-merchant-catalog` / `push-merchant-config` / `push-auth-bootstrap` | push declared file state outward (catalog to providers, merchants/secrets, auth authority); same mutation-flag contract |

`pull-provider` is manual-only by design — never scheduled. Routine catch-up is
Provider Refresh's job; `pull-provider` is the full-surface investigation tool
with the findings ledger and forensics
([operations.md → Provider Pull](operations.md#provider-pull-107-511)).

**The findings queue doctrine**: findings that require judgment (remote
mutations, ambiguous identity matches) land in the admin queue and **never
auto-fire** — a human acknowledges them, and raising the write mode does
nothing to this queue by design. Findings have stable identity across runs and
auto-resolve when the divergence vanishes. Taxonomy:
[operations.md — The Convergence Engine](operations.md#the-convergence-engine).

### Cutover / migration boots

Set the mode **before first start** — freshly imported stale `past_due`
subscriptions are immediately "due", and a `full` boot would start charging
them within hours:

1. Boot with `PROVIDER_WRITE_MODE=limited` (site fully usable; system-origin
   writes park) — or `readonly` for a strictly-observing boot.
2. The first dunning cycle materializes the backlog as parked intents:
   window-expired subs get the local no-charge cancel, in-window charges park.
3. `openrails intents --merchant=<slug>` shows the real drain forecast
   ("N execute under limited, M require full"). Resolve anything you do not
   want to fire.
4. `openrails pull-provider report --merchant=<slug>` — clear the human-judgment
   findings queue (it never drains automatically).
5. Raise to `PROVIDER_WRITE_MODE=full`: the executor drains exactly what you
   saw. Missed periods are never back-billed.

Full sequence and rationale: [operations.md →
Cutover](operations.md#cutover-booting-against-production-credentials).

### Secrets & credential rotation

- **Backends**: `secret_backend: db` (default; envelope-encrypted in Postgres
  under `ENCRYPTION_MASTER_KEY`) or `secret_backend: vault` (KV-v2). Declared,
  never auto-detected, never silently falls back. Vault setup + minimal
  policies: [vault.md](vault.md); per-merchant secret ops, canonical names,
  and the DB→Vault migration runbook:
  [vault.md](vault.md).
- **Naming**: addressed as `(merchant_id, name)` in code; Vault path
  `secret/openrails/merchants/<merchant-slug>/<name>`. A 15-minute TTL cache
  fronts all backends — out-of-band Vault writes converge within one TTL, no
  restart needed.
- **Rotation within the same provider account** is a non-event: the account
  identity re-resolves under the new key and matches. Rotate through
  `PUT /v1/merchant/payment-providers/{rail}` or the console's **Rotate**
  action — the new credential is live-probed first (a failed probe writes
  nothing and leaves the old credential serving), and the committed rotation
  records a version watermark on the shared PSP row that no node may serve a
  cached credential below. Cutover is deployment-wide at the next credential
  read; the 15-minute TTL is only the backstop for writes made out of band.
- **Pointing credentials at a different account** trips the account guard:
  every provider intent is stamped with the provider-account row it was
  enqueued against, and the executor parks intents whose account no longer
  resolves — a queue built against one account never executes against another.
  Options: restore the old account's credentials so the queue drains, or let
  stale intents expire/supersede. Rules:
  [operations.md → Durability model](operations.md#durability-model).

### Processor routing

Checkout is one processor at a time, chosen **before** the session exists. A request that
names a PSP gets that PSP; a request that omits `payment.rail` is routed.

- **Default** (no policy declared): stripe → nmi → ccbill → solana, first one that can
  serve the price.
- **Policy**: `checkout_routing` in the merchant manifest (mode 1) or
  `checkout_routing` on `PUT /v1/merchant/settings` (mode 2). Ordered
  rules, first match wins; each rule's `prefer` list is both the ranking and the
  whitelist, so a rule can pin a product to one rail. Conditions: `currency`, `product`,
  `price`, `mode`, `country` — all optional, all AND-ed; a rule with no conditions is the
  catch-all and must be last.

```yaml
checkout_routing:
  - match: { currency: eur, mode: subscription }
    prefer: [ccbill, mobius]
  - prefer: [mobius, ccbill, solana]
```

- **Fallback** is availability-only and evaluated pre-charge: `not_armed`,
  `credentials_missing`, `link_missing`, `mode_unsupported`, `service_unavailable`,
  `ambiguous_selector`, `unknown_selector`, `resolve_failed`. A **decline is not one of
  them** — routing never retries a charge on a second processor.
- **Why did this customer get CCBill?** `checkout_sessions.routing_reason` holds the
  decision: policy, matched rule, winner, ranked fallbacks, and every skipped candidate
  with its class. Written once at creation, never rewritten.
- **Preview without charging**: `POST /v1/merchant/payment-providers/routing/dry-run`
  with `{"price_id": "...", "country": "US"}` returns the same decision a real session
  would make, including the exact `routing_reason` it would store.

### Observability

- **Metrics / analytics API**: `GET /v1/merchant/metrics/schema` (self-describing
  measure/dimension registry) + `POST /v1/merchant/metrics/query` — aggregate-only,
  RLS-scoped to the API key's merchant, designed to be driven by an LLM agent.
  [metrics-for-llms.md](metrics-for-llms.md).
- **Logs**: structured logrus to stdout; level via `logger.level` in
  config.yaml. Every `pull-provider` local write is logged with finding id +
  evidence; `openrails intents-log` is the durable audit trail of provider
  mutations. Provider Refresh logs a per-pass heartbeat and per-merchant
  reconcile summary.
- **Worker health**: `openrails.worker_health` rows per job kind; the 5-minute
  checker raises durable repair alerts when a periodic kind stops completing.
- **Alerting**: metric-threshold alert evaluation every 15 minutes and a daily
  low-severity findings digest, delivered through the notification queue.
