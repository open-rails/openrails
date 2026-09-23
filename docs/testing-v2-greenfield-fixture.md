# Greenfield focused fixture

PR650's `compatibility/focused-test-map.tsv` remains the replacement matrix;
this lane supplies its first independent scenario. PR653 Phase 1 was discarded
and is not a fixture dependency. The legacy suite and selectors remain intact.

The fixture owns one disposable PostgreSQL database per test process. It accepts
`OPENRAILS_TEST_DATABASE_URL` as a cluster administrator DSN, creates a unique
process-scoped database, and drops it during cleanup. The provisioner exposes a
small Go API and performs only control-plane database operations; it does not
import `internal/dbtest`, `internal/integrationharness`, legacy migration
helpers, or execute application-schema SQL.

After provisioning, the fixture calls only public
`embed.ApplyMigrations`. It discovers the resulting schema through
`pg_catalog` and records the discovered object names as a receipt. Runtime
construction uses public `embed.New`; catalog, customer, and isolation
assertions use the production `openrails.Client` and the public HTTP route
bundle mounted in a standard-library mux. No assertion queries billing tables.

A deterministic fake provider `RoundTripper` is injected through the public
sandbox option. It records method, URL, headers, and body in a per-runtime
journal and is closed by default, so unexpected provider egress fails the
scenario. Later provider-recovery rows can extend this journal with explicit
commit/lost-response/replay scripts.

The first focused scenario provisions two merchants in one disposable database.
Merchant A creates a product through the Client; merchant B cannot retrieve it.
Each runtime also mounts `/v1/me/*` with a delegated authenticator that maps a
bearer token to an explicit merchant and customer UUID. A customer's read on its
own merchant succeeds, while presenting that token to the other merchant is
refused. The test carries a `greenfield` build tag and emits a JSON receipt; it
is invoked explicitly and cannot change the legacy integration command.
