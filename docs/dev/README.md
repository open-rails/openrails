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
  `migrations/bootstrap/`, the `profiles` shim (`internal/db/schema/profiles_shim.sql`,
  standing in for AuthKit's schema), then `migrations/postgres/*.up.sql` in order.

So the usual loop: `task docker-up`, edit queries or migrations, `task sqlc`,
commit the regenerated `internal/db/gen`. CI runs `task sqlc-check` and fails
if generated code is stale.

## Migrations

`migrations/postgres/0001_schema.up.sql` is a single squashed baseline
(greenfield — no numbered history before it); new migrations continue from
`0002_*`. `migrations/bootstrap/` holds instance-level init that runs before
the app migrations. Schema-shape invariants are enforced by Go tests that live
next to the migrations.

## Repo layout

- `client.go`, `remote.go`, `errors.go` — root package `openrails`: the SDK surface, one `Client` interface with remote (HTTP) and embedded constructors
- `embed/` — embedded-mode integration (host app embeds OpenRails in-process)
- `cmd/openrails/` — the binary: server + CLI (catalog/merchant-config/bootstrap apply, reconcile)
- `pkg/` — importable packages (api, catalog, service, embedded, merchant, …)
- `internal/` — everything else: `modules/` (domain), `db/` (queries/gen/models), `river/` (jobs), `integrations/` (nmi, stripeapi, solana, …), `http/`, `controlplane/`
- `migrations/` — bootstrap + postgres baseline and increments
- `tests/` — cross-cutting integration + live-sandbox e2e tests
- `scripts/` — Task-target implementations
- `web/admin/` — admin console SPA source
