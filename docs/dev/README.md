# Contributing

For people working on OpenRails itself. [Testing](testing.md),
[compatibility](../compatibility.md), [local webhooks](local-webhooks.md), the
[injected-code scan](injection-scan.md).

## Tasks

Routine work goes through [Task](https://taskfile.dev) (`Taskfile.yaml`):

| Target | Does |
|---|---|
| `task build`, `task run`, `task dev` | build `bin/openrails` (embedding `server/web/console/dist` if built), run it, hot-reload it (Air) |
| `task docker-up`, `docker-down`, `docker-reset`, `docker-logs` | the local stack (server, PostgreSQL, Garnet); `docker-reset` deletes the database volume |
| `task sqlc`, `task sqlc-check` | regenerate `internal/db/gen`; check it is current and prepare every query |
| `task test`, `task ci-local` | `scripts/check.sh all` against owned disposable services |
| `task admin-build`, `payment-page-build`, `build-console-binary` | the console and payment page builds; the binary with the console embedded |
| `task lint`, `task fmt`, `task clean` | |

`tunnel-webhooks`, `verify-webhook-tunnel`, `mint-jwt`, `nmi-query` and
`docker-up-e2e-sandbox` help with provider work ([local webhooks](local-webhooks.md));
none is a merge gate.

## Modules

Four Go modules, each checked alone (`GOWORK=off`; `scripts/check.sh build` builds
all):

- `.`: the embedded library. It imports only AuthKit's standard-library contracts;
  `scripts/check-embedded-auth-boundary.sh` rejects AuthKit's runtime.
- `server/`: package `server`, the binary `server/cmd/openrails` and the
  AuthKit-backed packages under `server/internal/`. It builds against the root's
  source (`replace … => ../`).
- `adapters/authkit` and `examples/`.

In the root: the package `openrails` (`embedded.go`, `remote.go`, …), `billing/`
(the API vocabulary), `catalog/`, `adapters/{http,gin,fiber}/`, `openrailstest/`
(with `nmimock`, `stripemock`), and `internal/` for everything else (`internal/engine`, `internal/modules`,
`internal/db`, `internal/river`, `internal/integrations`, the route catalog in
`internal/http/routes`).
`api/` holds the contract snapshots, `critical/` the scenario suite,
`internal/migrate/postgres/` the migrations. Only those public packages,
`server/web/console` and `server` are importable; another changes
`compatibility/contract.json`.

## Database

One role runs the server, workers and CLI, and owns every object; there are no
grants and no row-level security, so every tenant query carries its own
`merchant_id` (or `psp_id`) predicate.

SQL is hand-written in `internal/db/queries/*.sql` and compiled by sqlc into
`internal/db/gen` (never edit it). `generate` and `vet` need a PostgreSQL whose
schema matches the migrations: `SQLC_DATABASE_URL`, or `task sqlc` creates a
throwaway `openrails_sqlc_vet` database on the local stack
(`SQLC_ADMIN_DATABASE_URL`, `POSTGRES_HOST_PORT`, `SQLC_VET_DB` override it).

`internal/migrate/postgres/0001_schema.up.sql` is the whole schema, written by
hand: tables in foreign-key order, each followed by its comments, indexes,
triggers and the trigger functions only it uses. After v1.0.0 it does not
change: a schema change is a new numbered migration (`0002_…`) with its
`api/schema.txt` diff.

Before v1 a merged migration is folded into the baseline: apply its effect where
the objects it touches are defined, delete the numbered file and take it out of
`sqlc.yaml`. CI's "SQL bindings and query audit" job proves the fold: when a
pull request into a release candidate changes or removes a migration of its base, `go run
./scripts/schemaproof` applies the base's chain and the tree's to empty
databases and fails unless they dump identically (`pg_dump` 18), in `billing`
and in another schema.

A database the current chain does not match is refused at boot, one built by
the numbered migrations the baseline replaced included: recreate it
(`task docker-reset`; `DROP DATABASE` elsewhere; for an embedded host, drop the
schema and delete its rows from `public.migrations` where `app = 'openrails'`).

## Contracts

The Go API, HTTP API and schema have generated snapshots under `api/`
([compatibility](../compatibility.md)). A change regenerates them in the same pull
request, against a disposable PostgreSQL 18:

```sh
OPENRAILS_E2E_DSN=postgres://…/postgres go run ./scripts/contracts -write
```

That rewrites `api/go.txt`, `api/openapi.json`, `api/schema.txt`,
`compatibility/contract.json`, the TypeScript wire types and `docs/api/routes.md`
and `error-codes.md`. Without `-write` it checks them. Never hand-merge them.

## Branches

Cut a feature branch from the open release candidate named in [OPEN.md](../../OPEN.md) and open its
pull request against that `rc/<name>` branch, as a draft until its last commit. Master takes only the
release candidate and the release PR below. [docs/ci.md](../ci.md) has the rules and what each runs.

## Releases

```sh
scripts/release prepare vX.Y.Z [rc/<name>]  # opens chore/release-vX.Y.Z from master, or from the open RC, pinning server and adapter to the root
scripts/release publish vX.Y.Z ["note"]     # once that PR's checks pass
```

`publish` fast-forwards master to the green PR and pushes the signed `vX.Y.Z`,
`server/vX.Y.Z` and `adapters/authkit/vX.Y.Z` tags atomically. The tag runs
GoReleaser (binaries, checksums, SBOMs, provenance, then `@openrails/billing-ui` on
npm) and the image build (`vX.Y.Z`, `X.Y`, `latest` on Docker Hub and GHCR,
amd64 and arm64). Dry run: `goreleaser release --snapshot --clean --skip=publish,sign`.
