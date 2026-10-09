# Contributor guide

Docs for people hacking on OpenRails itself. Audience-facing docs (integrators,
operators, merchants) live one level up in `docs/`.

- [testing.md](testing.md) — package tests, the end-to-end suite, business time / test clocks
- [compatibility](../compatibility.md) — the three frozen contracts and their snapshots
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
| `task test` | Source guardrails + unit tests (`-race`) + focused e2e contracts (requires `OPENRAILS_E2E_DSN`) |
| `task ci-local` | Run the same compact checks and e2e contracts as CI |
| `task admin-build` | Build the admin console SPA into `web/admin/dist` (gitignored) |
| `task build-console-binary` | `admin-build` + `build`: the binary with the console embedded |
| `task fmt` / `task clean` | `go fmt` + `goimports` / remove build artifacts |

Local provider-development helpers (`tunnel-webhooks`, `verify-webhook-tunnel`,
`mint-jwt`, `nmi-query`, `e2e-dump-local`, `docker-up-e2e-sandbox`) remain
available for explicitly scoped operator work. See
[local-webhooks.md](local-webhooks.md); these helpers are not merge gates.

## Database role in local dev

One role, `app` (the Compose Postgres user), runs `openrails migrate up`, the
server, the workers and the CLI. It owns every object it creates; OpenRails
creates no roles and issues no grants.

There is no row-level security, so the login does not isolate merchants:
every tenant query must carry its own `merchant_id` (or `psp_id`) predicate,
backed by composite foreign keys. A query that forgets it reads across
merchants.

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
  the authored baseline through migratekit in `billing`, the schema all SQL is
  authored in, so vet prepares exactly what a default deployment runs. AuthKit
  tables and migrations are outside this query catalog.

So the usual loop: `task docker-up`, edit queries or migrations, `task sqlc`,
commit the regenerated `internal/db/gen`. `task sqlc-check` checks generated
code, query plans and SQL discipline locally; CI's `End-to-end` job regenerates
the bindings and runs the query audit (`TestQueryAudit`).

## Frozen contracts

The Go API, the HTTP API and the database schema are frozen from v1.0.0
([compatibility](../compatibility.md)). Each has a generated snapshot under
`api/`; a change to one regenerates its snapshot in the same pull request:

```sh
OPENRAILS_E2E_DSN=postgres://postgres:postgres@127.0.0.1:5432/postgres \
  go run ./scripts/contracts -write
```

That rewrites `api/go.txt`, `api/openapi.json` with the generated TypeScript
wire types and the tables under `docs/api`, and `api/schema.txt` (read from the
PostgreSQL 18 the DSN names). Never hand-merge a generated file: regenerate it.

## Migrations

`internal/migrate/postgres/0001_schema.up.sql` is the v1 baseline and does not
change. A schema change is a new numbered migration (`0002_…`) that upgrades
any v1 database in place, with its `api/schema.txt` diff in the same pull
request.

A database built before v1.0.0 is not upgraded: `migrate up` refuses it
(migratekit strict integrity: the applied `0001` differs and the schema is not
a fresh build of it). Wipe it:

| Database | How |
|---|---|
| Local compose stack | `task docker-reset` — `down -v` (deletes the `postgres_data` volume) then `docker-up`, which re-runs `openrails-migrate` against an empty server. Plain `task docker-down` keeps the volume and therefore keeps the stale ledger. |
| A dev/staging server you can't drop the volume of | `DROP DATABASE` + `CREATE DATABASE`, then `openrails migrate up`. |
| A hand-rolled test pool | Provision a new disposable database. The e2e suite creates a fresh schema per test. Never clear another library's shared ledger rows. |
| An EMBEDDED host's database (one schema inside the host's DB) | Stop the host, then `DROP SCHEMA billing CASCADE; DELETE FROM public.migrations WHERE app = 'openrails' AND schema = 'billing';` (use the configured schema). Restart the host so it re-applies the chain. |

## Repo layout

- `client.go`, `remote.go`, … — root package `openrails`: the SDK surface, one concrete `*Client` (`NewRemote`, or `New` over the in-process transport); `Config`/`Deps` named from `internal/config`
- `billing/` — the API vocabulary: request/response types, IDs, errors and codes, permission names
- `catalog/` — the catalog document (`catalog.Application`) and its charge-model types
- `adapters/{http,gin,fiber}/` — router adapters for `client.Routes`
- `internal/engine/` — the in-process engine behind `openrails.New` (lifecycle, routes, River, the opt-in control plane)
- `cmd/openrails/` — the binary: server + CLI (catalog/merchant-config/bootstrap apply, reconcile)
- `internal/` — everything else: `modules/` (domain), `db/` (queries/gen/models), `river/` (jobs), `integrations/` (nmi, stripeapi, solana, …), `http/` (the route catalog in `http/routes`), `controlplane/`
- `internal/migrate/postgres/` — the authored PostgreSQL migration baseline
- `api/` — the frozen-contract snapshots: `go.txt`, `openapi.json`, `schema.txt`
- `ci/` — the end-to-end suite: public-client contracts with disposable PostgreSQL schemas and deterministic provider transports
- `scripts/` — Task-target implementations
- `web/admin/` — admin console SPA source; `embed.go` embeds its `dist/` build

The root, `billing`, `catalog`, the adapters and `web/admin` are the only
importable non-`main` packages; `internal/contractaudit` `TestPublicPackages`
fails on any other.

## Releases

```sh
gh workflow run cut-release.yaml -f bump=minor   # or bump=patch, bump=major
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
