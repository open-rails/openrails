# Contributor guide

Docs for people hacking on OpenRails itself. Audience-facing docs (integrators,
operators, merchants) live one level up in `docs/`.

- [testing.md](testing.md) — test doctrine, integration suite, business time / test clocks, e2e harnesses
- [local-webhooks.md](local-webhooks.md) — deterministic public webhook URLs for local dev (cloudflared)

## Task targets

Everything routine goes through [Task](https://taskfile.dev) (`Taskfile.yaml`):

| Target | What it does |
|---|---|
| `task build` | Build `bin/openrails` from `./cmd/openrails` |
| `task run` | Build + run the server |
| `task dev` | Hot-reload dev server (Air, `.air.toml`) |
| `task docker-up` / `task docker-down` | Start/stop the local compose stack (openrails + Postgres + Garnet) |
| `task docker-reset` | Recreate the stack from empty — deletes the Postgres volume, then re-migrates ([why you'd need this](#migrations)) |
| `task docker-logs` | Tail the openrails container |
| `task sqlc` / `task sqlc-check` | Regenerate + vet `internal/db/gen` (see below); `sqlc-check` is the CI staleness gate |
| `task test` | Business-time guardrail + unit tests (`-race`) + core integration tier |
| `task test-integration-core` / `task test-integration-all` | Integration tests against the compose stack (see testing.md) |
| `task admin-build` | Build the admin console SPA into `cmd/openrails/consoleassets/dist` (gitignored) |
| `task build-console-binary` | Binary with the console embedded (`-tags console_assets`) |
| `task fmt` / `task clean` | `go fmt` + `goimports` / remove build artifacts |

E2E helpers (`tunnel-webhooks`, `verify-webhook-tunnel`, `mint-jwt`,
`e2e-nmi-live`, `nmi-query`, `e2e-dump-local`, `docker-up-e2e-sandbox`) are
covered in [testing.md](testing.md) and [local-webhooks.md](local-webhooks.md).

## Database roles in local dev

Two roles, and the split is not cosmetic:

| Role | Used by | Why |
|---|---|---|
| `admin` (superuser) | `openrails migrate up`, the compose bootstrap SQL, `scripts/sqlc-vet-db.sh` | DDL, `GRANT`s, role creation |
| `app` (unprivileged, `NOBYPASSRLS`) | **the server, the workers, the CLI — everything else** | the development host login; production chooses its own name |

The server refuses to boot as a superuser or any `BYPASSRLS` role, in every
environment including development. That is deliberate and there is no dev
opt-out. Under a privileged role, a query against an RLS-forced table that
forgets its merchant scope still returns rows, so the bug looks fine locally
and runs inert in production — an unscoped read has its policy predicate
degenerate to `merchant_id = NULL`, returns **zero rows with no error**, and
the caller logs success. Running dev as `app` makes that fail on your
laptop instead. If a query of yours starts returning nothing after this, it is
missing a `MerchantTx`/`RunInMerchantConn` scope — fix the scope, do not reach
for the superuser.

The `openrails-app-login` one-shot Compose service creates the host's regular
LOGIN before migrations. The migration command receives its connection through
`--runtime-database-url`; AuthKit and OpenRails then grant their own runtime
access directly. The libraries create no database roles or memberships.

## sqlc workflow

SQL is hand-written in `internal/db/queries/*.sql` and compiled to
`internal/db/gen` by sqlc (version pinned in `Taskfile.yaml`; config in
`sqlc.yaml`). Never edit `internal/db/gen` by hand.

Both `generate` (database-backed analyzer) and `vet` (the `sqlc/db-prepare`
rule PREPAREs every query) need a live Postgres whose schema matches
`migrations/`, via `SQLC_DATABASE_URL`. `task sqlc` resolves it:

- If `SQLC_DATABASE_URL` is set, it is used as-is.
- Otherwise `scripts/sqlc-vet-db.sh` drops/creates a throwaway vet DB
  (`openrails_sqlc_vet`) on the local compose Postgres (default
  `127.0.0.1:5434`; override via `SQLC_ADMIN_DATABASE_URL`,
  `SQLC_POSTGRES_HOST`, `POSTGRES_HOST_PORT`, `SQLC_VET_DB`) and applies
  the authored baseline through migratekit with
  explicit canonical schema `openrails`. Runtime fixtures use the default
  `billing` schema and production query rewriting; SQLC vet prepares the source
  SQL directly. AuthKit tables and migrations are outside this query catalog.

So the usual loop: `task docker-up`, edit queries or migrations, `task sqlc`,
commit the regenerated `internal/db/gen`. CI runs `task sqlc-check` and fails
if generated code is stale.

## Migrations

`internal/migrate/postgres/0001_schema.up.sql` is a single squashed baseline
for fresh PostgreSQL 18 databases, including extensions and creator catalogs.
Schema-shape invariants are enforced by Go tests next to the migration.
All prerelease databases are disposable; no old-schema upgrade is supported.

Use a fresh database for this pre-v1 hard cut. Do not restamp an old ledger or
try to upgrade historical schemas; initialization verifies migration identity.

Recreating one:

| Database | How |
|---|---|
| Local compose stack | `task docker-reset` — `down -v` (deletes the `postgres_data` volume) then `docker-up`, which re-runs `openrails-migrate` against an empty server. Plain `task docker-down` keeps the volume and therefore keeps the stale ledger. |
| A dev/staging server you can't drop the volume of | `DROP DATABASE` + `CREATE DATABASE`, then `openrails migrate up`. |
| A hand-rolled test pool | Provision a new disposable database. The integration suite already creates a fresh per-run database. Never clear another library's shared ledger rows. |
| An EMBEDDED host's database (one schema inside the host's DB) | Stop the host, then run `task db-reset-embedded DSN='…'`. It is plan-only by default and prints the exact `host:port/database` allow-list entry and confirmation token. To apply, set that entry in `OPENRAILS_RESET_TARGETS` and rerun with `CONFIRM='…'`; the schema drop and exact OpenRails/Postgres/schema ledger delete commit together. Restart the host so it re-applies the chain. |

You will not have to notice this yourself: the engine REFUSES to start when the
ledger records migrations the build no longer carries (`OrphanedMigrationsError`,
or#901/upstream#1627), on both the standalone `migrate up` path and the embedded
runtime-init path. Before that fence existed the symptom was not a migration
error but a schema that silently lacked whatever the squash folded in — upstream#1627
was an embedded host answering 500 on every billed admission for hours while
both of migratekit's checks reported success.

## Repo layout

- `client.go`, `remote.go`, `errors.go`, … — root package `openrails`: the SDK surface, one concrete `*Client` (`NewRemote`, or `embed.Runtime.Client` over the in-process transport)
- `embed/` — the in-process runtime (`embed/authkit` auth bridges, `embed/controlplane` for hosts on OpenRails' own AuthKit)
- `cmd/openrails/` — the binary: server + CLI (catalog/merchant-config/bootstrap apply, reconcile)
- `pkg/` — importable packages (api, billingauth, catalog, merchant, adminconsole, query, …)
- `internal/` — everything else: `modules/` (domain), `db/` (queries/gen/models), `river/` (jobs), `integrations/` (nmi, stripeapi, solana, …), `http/`, `controlplane/`
- `migrations/` — bootstrap + postgres baseline and increments
- `tests/` — cross-cutting integration + live-sandbox e2e tests
- `scripts/` — Task-target implementations
- `web/admin/` — admin console SPA source
