# Raw SQL Inventory

Status: reviewed 2026-06-29 for #628.

Scope: handwritten SQL outside `internal/db/queries` and generated
`internal/db/gen`. Migrations, sqlc query files, and generated files are out of
scope. Test fixture SQL is allowed when it creates or mutates fixture state.

## Keep Raw

### Database lifecycle, migrations, and test harness

- `internal/migrate/*`: migration driver, migration lock/tracking, schema
  bootstrap. Keep raw because it is DDL/control-plane migration plumbing.
- `internal/dbtest/*`: creates/drops scratch databases, enables test roles,
  seeds the canonical test merchant. Keep raw because it is test infrastructure.
- `internal/db/querytest/*`: `TRUNCATE`, `ANALYZE`, and
  `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)` for the query performance harness.
  Keep raw because these are harness operations, not application queries.

### Session state, RLS, advisory locks, and wrappers

- `internal/db/db_pgx.go`, `internal/db/rls.go`,
  `internal/controlplane/customer.go`: `set_config` / RLS session setup. Keep raw
  because this is session state, not a domain query.
- `internal/db/schema_rewrite.go`: wrapper methods rewrite already-authored SQL
  before delegating to pgx/sqlc. Keep raw wrapper calls.
- `internal/bootstrap/merchant_manifest.go`,
  `internal/http/handlers/admin_payments.go`: advisory locks. Keep raw because
  Postgres lock functions are coordination primitives.

### Dynamic or cross-store queries

- `internal/modules/analytics/*`: dynamic analytics filters and ClickHouse
  queries. Keep raw; sqlc does not cover ClickHouse here, and the Postgres
  analytics filters are dynamic by design.
- `internal/modules/admission/policy.go`, `internal/modules/money/*`: dynamic
  policy/meter rating lookups assembled from configured measures/meters. Keep raw
  until those policy surfaces stabilize enough to move static pieces.

### Control-plane/global tables

- `internal/controlplane/*`: bootstrap, API-key, and customer control-plane
  queries. Keep raw for now because these are global/control-plane paths outside
  the merchant sqlc surface. Convert later if this package grows more static SQL.
- `internal/merchants/*`: merchant lifecycle, secrets, webhook routing, delete,
  and provider config queries. Keep raw for now because they operate on global
  merchant/control-plane state and include lock/delete lifecycle steps.

### Catalog sidecars and drift helpers

- `pkg/service/catalog_sidecars.go`: catalog sidecar upserts/deletes and product
  lookups. Keep raw for now because this is tightly coupled to manifest apply and
  bulk replace semantics; it is now covered indirectly by the query-contract
  catalog path and should be the next sqlc conversion candidate if it grows.
- `pkg/service/service_definition_catalog_admin.go`: advisory drift close; keep
  raw because it is best-effort ops cleanup.

### Reconciliation, River, and repository edge paths

- `internal/river/jobs_provider_refresh.go`,
  `internal/river/jobs_stripe_webhooks.go`: worker-local provider refresh/webhook
  reads and writes. Keep raw for now; provider-refresh code is still operational
  integration plumbing.
- `internal/db/repo/product_access_grant.go`,
  `internal/db/repo/solana_subscription.go`: repository-specific dynamic reads.
  Keep raw until the owning packages are folded into sqlc.
- `internal/crypto/dek_store_db.go`: tiny DEK store queries. Keep raw because the
  crypto package should not depend on the OpenRails sqlc domain package.

## Converted Or Deleted

No obvious duplicated static raw query was safe to delete in this pass. The broad
runtime raw SQL that remains is either DDL/session/lock code, dynamic analytics or
policy SQL, ClickHouse SQL, control-plane global SQL, or package-local persistence
with a clear ownership boundary.

## Covered By #628 Query Tests

- sqlc prepare/schema validity: `task sqlc-check`.
- contract execution: `task test-query-contracts`.
- hot entitlement lookup plan shape at 100k rows: `task test-query-perf`.

