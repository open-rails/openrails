# Compatibility

From v1.0.0 OpenRails follows semantic versioning. A v1.x release changes the
three contracts below only by adding to them; removing or changing anything in
them waits for v2. A security fix may change behavior when that behavior was
the vulnerability.

| Contract | Snapshot | CI fails when it is stale |
|---|---|---|
| Go API | [`api/go.txt`](../api/go.txt) | `TestGoAPISurface` (Checks) |
| HTTP API | [`api/openapi.json`](../api/openapi.json) | `TestGeneratedContractIsFresh` (Checks) |
| Database schema | [`api/schema.txt`](../api/schema.txt) | `TestSchemaSnapshot` (End-to-end) |

Each snapshot is generated from the code, never written by hand, so a change
to a contract cannot merge without its snapshot changing in the same pull
request. The snapshot's diff is what a reviewer reads: an added line is an
addition; a removed or changed line is a break, and is refused in v1.x.

```bash
# Rewrite all three. The schema is read from a real PostgreSQL 18: name a
# disposable server (the role needs CREATEDB; a scratch database is dropped).
OPENRAILS_E2E_DSN=postgres://postgres:postgres@127.0.0.1:5432/postgres \
  go run ./scripts/contracts -write

go run ./scripts/contracts   # check them, as CI does
```

Without a server the command still rewrites the Go and HTTP snapshots. It
leaves `api/schema.txt` alone while the migration files are the ones the
snapshot names, and fails when they are not.

## Go API

Every exported identifier of `openrails`, `billing`, `catalog`,
`adapters/http`, `adapters/gin`, `adapters/fiber`, `openrailstest` and
`web/admin`.
`api/go.txt` holds one line per constant, variable, function, type, struct
field (with its tag) and method. `internal/…`, `cmd/…`, `examples/…` and `ci/…`
are not covered. Some types are declared under `internal/` and named by an
alias (`openrails.Config`, `openrails.Deps`): the alias and its members are
covered, and the test fails when the public API reaches an internal type any
other way. The keys of a catalog application and of a merchant declaration
are struct tags, so the list covers the YAML and JSON documents too.

- **Additive:** a new package, identifier or `*Client` method; a struct field
  whose zero value keeps the old behavior (write keyed struct literals); a new
  value of a typed enum; a new error sentinel in `billing`.
- **Breaking:** removing or renaming a line of `api/go.txt`; changing a
  signature, a field's type or tag, or a constant's value; adding a method to
  an interface a host implements (`EmailSender`, `SMSSender`,
  `RequestAuthenticator`);
  moving to a new major version of a module whose types the API exposes (pgx
  v5, go-redis v9, the Vault client).

## HTTP API

Every route in the catalog (`internal/http/routes`): its method, path, auth
tier and permission, request and response schemas, success status and error
codes, and the error-code registry with each code's status and type.
`api/openapi.json` is generated from the catalog together with the TypeScript
wire types of `@openrails/billing-ui` and the console, and the
[route](api/routes.md) and [error-code](api/error-codes.md) tables.

- **Additive:** a new route, optional request member, response member, error
  code or enum value. Clients ignore response members and enum values they do
  not know.
- **Breaking:** removing or renaming a route, member, code or value; making a
  request member required; changing a type, a status or a meaning.

An error's `code` is contract; its `message` is not.

## Database schema

Every table, column (type, nullability, default), constraint, index, trigger
and function the migrations install. `api/schema.txt` is read back from the
catalogs of a PostgreSQL 18 after the real migrator ran, at the default schema
`billing`. The monthly partitions of `usage_events` and `admission_operations`
are left out, because their names follow the calendar; the partitioned tables
are in.

After v1.0.0 the baseline `0001_schema.up.sql` never changes: a schema change
is a new numbered migration, and `openrails.Migrate` of any v1.x upgrades a
database of any earlier v1 release in place.

- **Additive:** a new table; a new column that is nullable or has a default;
  a new index, function or trigger; a CHECK that accepts more values.
- **Breaking:** dropping or renaming a table or column; changing a column's
  type; a constraint existing rows may not satisfy.

`TestSchemaSnapshotNamesTheMigrations` (Checks) needs no database: it fails as
soon as a migration file changes without the snapshot.

## Not covered

`internal/…`, log lines, error `message` text and metric values. The
components, hooks and styles of `@openrails/billing-ui` and the admin console
follow their own releases (pin the exact version); their generated wire types
follow the HTTP contract.
