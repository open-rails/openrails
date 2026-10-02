# Contributor guide

Docs for people hacking on OpenRails itself. Audience-facing docs (integrators,
operators, merchants) live one level up in `docs/`.

- [testing.md](testing.md) — greenfield contracts, ordinary checks, business time / test clocks
- [local-webhooks.md](local-webhooks.md) — deterministic public webhook URLs for local dev (cloudflared)

## Task targets

Everything routine goes through [Task](https://taskfile.dev) (`Taskfile.yaml`):

| Target | What it does |
|---|---|
| `task build` | Build `bin/openrails` from `./cmd/openrails` (embeds `web/admin/dist` if built) |
| `task run` | Build + run the server |
| `task dev` | Hot-reload dev server (Air, `.air.toml`) |
| `task docker-up` / `task docker-down` | Start/stop the local compose stack (openrails + Postgres + Garnet) |
| `task docker-reset` | Recreate the stack from empty — deletes the Postgres volume, then re-migrates ([why you'd need this](#migrations)) |
| `task docker-logs` | Tail the openrails container |
| `task sqlc` / `task sqlc-check` | Regenerate + vet `internal/db/gen` (see below); `sqlc-check` includes the local staleness gate |
| `task test` | Source guardrails + unit tests (`-race`) + focused greenfield contracts (requires `OPENRAILS_GREENFIELD_DSN`) |
| `task ci-local` | Run the same compact checks and greenfield contracts as CI |
| `task admin-build` | Build the admin console SPA into `web/admin/dist` (gitignored) |
| `task build-console-binary` | `admin-build` + `build`: the binary with the console embedded |
| `task fmt` / `task clean` | `go fmt` + `goimports` / remove build artifacts |

Local provider-development helpers (`tunnel-webhooks`, `verify-webhook-tunnel`,
`mint-jwt`, `nmi-query`, `e2e-dump-local`, `docker-up-e2e-sandbox`) remain
available for explicitly scoped operator work. See
[local-webhooks.md](local-webhooks.md); these helpers are not merge gates.

## Database roles in local dev

Two roles, and the split is not cosmetic:

| Role | Used by | Why |
|---|---|---|
| `admin` (superuser) | `openrails migrate up`, the compose bootstrap SQL, `scripts/sqlc-vet-db.sh` | DDL, `GRANT`s, role creation |
| `app` (unprivileged) | **the server, the workers, the CLI — everything else** | the development host login; production chooses its own name |

There is no row-level security, so the login does not isolate merchants:
every tenant query must carry its own `merchant_id` (or `psp_id`) predicate,
backed by composite foreign keys. A query that forgets it reads across
merchants under either role.

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
`internal/migrate/postgres/`, via `SQLC_DATABASE_URL`. `task sqlc` resolves it:

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
commit the regenerated `internal/db/gen`. Run `task sqlc-check` locally to
check generated code, query plans, and SQL discipline; the compact CI workflow
does not currently run that task.

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
| A hand-rolled test pool | Provision a new disposable database. The greenfield suite creates a fresh schema per test. Never clear another library's shared ledger rows. |
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
- `embed/` — the in-process runtime (`pkg/billingauth` neutral authentication, `embed/controlplane` for explicit standalone AuthKit composition)
- `cmd/openrails/` — the binary: server + CLI (catalog/merchant-config/bootstrap apply, reconcile)
- `pkg/` — importable packages (api, billingauth, catalog, merchant, adminconsole, query, …)
- `internal/` — everything else: `modules/` (domain), `db/` (queries/gen/models), `river/` (jobs), `integrations/` (nmi, stripeapi, solana, …), `http/`, `controlplane/`
- `internal/migrate/postgres/` — the authored PostgreSQL migration baseline
- `ci/` — focused public-client contracts with disposable PostgreSQL schemas and deterministic provider transports
- `scripts/` — Task-target implementations
- `web/admin/` — admin console SPA source; `embed.go` embeds its `dist/` build

## Releases

```sh
gh workflow run cut-release.yaml -f bump=minor   # or bump=patch
```

This tags master's head with the next version; the tag runs `release.yaml`
(GoReleaser: binaries, checksums, SBOMs, generated notes, the
`openrails-billing-ui-X.Y.Z.tgz` asset, build provenance) and
`docker-publish.yaml` (`vX.Y.Z`, `X.Y`, `latest` on Docker Hub and GHCR).
Edit the generated notes on the release page if needed. Dry run:
`goreleaser release --snapshot --clean --skip=publish,sign`.

`cut-release.yaml` pushes the tag with the `RELEASE_TOKEN` secret (a
fine-grained PAT with Contents and Workflows read/write on this repo): tags
pushed with `GITHUB_TOKEN` start no workflows. Pushing a `v*` tag by hand
releases the same way.
