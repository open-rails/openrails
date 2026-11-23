# Compatibility contracts

From v1.0.0 OpenRails follows semantic versioning: a v1.x release changes three
contracts only by adding to them, and removing or changing anything waits for
v2. A security fix may change behaviour when that behaviour was the vulnerability.

| Contract | Snapshot | Covers |
|---|---|---|
| Go API | [`api/go.txt`](../api/go.txt) | every exported identifier of `openrails`, `billing`, `catalog`, `server`, `adapters/*`, `openrailstest` (with `nmimock`, `stripemock`) and `server/web/console`: one line per constant, variable, function, type, struct field (with its tag) and method. The server and AuthKit adapter modules release with the root under the same version. |
| HTTP API | [`api/openapi.json`](../api/openapi.json) | every route in the catalog (`internal/http/routes`): method, path, auth tier, permission, request and response schemas, status and error codes, and the error-code registry |
| Database schema | [`api/schema.txt`](../api/schema.txt) | every table, column, constraint, index, trigger and function the migrations install, read back from PostgreSQL 18 (monthly partitions left out) |

`contract.json` here is a fourth, finer guard: the source of every exported
declaration of the public packages, the internal types they reach, and the JSON
shapes and custom codecs of every wire type, so a change shows even when the
public signature does not.

Each snapshot is generated, never written by hand, so a contract change cannot
merge without its snapshot changing in the same pull request. An added line is
an addition; a removed or changed line is a break, refused in v1.x.

- **Go.** Additive: a new package, identifier or method; a struct field whose
  zero value keeps the old behaviour; a new value of a typed enum; a new error
  sentinel in `billing`. Breaking: removing or renaming a line; changing a
  signature, a field's type or tag, or a constant's value; adding a method to an
  interface a host implements; a new major version of a module whose types the
  API exposes (pgx v5, go-redis v9, the Vault client).
- **HTTP.** Additive: a route, an optional request member, a response member, an
  error code or an enum value (clients ignore unknown members and values).
  Breaking: removing or renaming any of them, making a request member required,
  changing a type, status or meaning. An error's `code` is contract; its
  `message` is not.
- **Schema.** After v1.0.0 the baseline migration never changes; a change is a
  new numbered migration, `New` of any v1.x upgrades any earlier v1 database in
  place, and an earlier v1.x boots on a database a later one migrated. Additive:
  a table, a nullable or defaulted column, an index, function or trigger, a
  CHECK accepting more. Breaking: dropping or renaming a table or column,
  changing a type, a constraint existing rows may fail. A break ships in two
  releases: the first stops using it, the second removes it in a migration whose
  name ends `_contract.up.sql`. Before v1 a release may break the schema in one
  step; its notes name the migration, and hosts sharing it upgrade together.

Not covered: `internal/…`, `cmd/…`, `examples/…`, log lines, error messages,
metric values, and billing-ui's and the console's components, hooks and styles
(pin the exact version; their generated wire types follow the HTTP contract).

```sh
# Rewrite the snapshots; the schema is read from a disposable PostgreSQL 18.
OPENRAILS_E2E_DSN=postgres://…/postgres go run ./scripts/contracts -write
go run ./scripts/contracts -dsn postgres://…/postgres   # check, as CI does
```

Without a server `-write` still rewrites the Go and HTTP snapshots and fails
when the migrations differ from the ones `api/schema.txt` names.
