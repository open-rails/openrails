# SQL gate exemptions

Two gates run under `task sqlc-check`, on top of `sqlc vet`'s `db-prepare`
correctness check. In CI the query auditor runs in the End-to-end job and
`TestNoInlineSQL` in the unit suite:

| gate | what it proves | allowlist |
|---|---|---|
| `internal/db/sqlaudit` | query scope, declared bounds and index availability | `AUDIT_ALLOWLIST.txt` |
| `internal/db` `TestNoInlineSQL` | no SQL string literals outside `internal/db/queries` | `inlineSQLAllowed` in `inlinesql_test.go` |

Every allowlist entry is classified **PERMANENT** (bounded by design) or
**DEBT** (a real bug, kept only so the gate could be switched on), carries a
mandatory rationale, and is rejected if duplicated. An entry that stops tripping
fails the build as stale — fixing a query deletes its line.

## How the query auditor works

`sqlc vet` PREPAREs each query, which proves it is valid SQL and nothing more.
The auditor connects to the same throwaway vet DB, EXPLAINs every query, walks
the plan in Go and applies five rules. Two facts make that meaningful on a
database built from `migrations/` with zero rows:

**`EXPLAIN (GENERIC_PLAN, FORMAT JSON)`** (PG16+) plans a parameterized
statement without values, so no parameters are fabricated. Every generated query
is enumerated; unplannable statements require an explicit reviewed exception. It must be sent over the **raw simple-query protocol**
(`conn.PgConn().Exec`): pgx's extended protocol binds the query's own `$n` as
parameters of the EXPLAIN ("expected N arguments, got 0"), and
`QueryExecModeSimpleProtocol` interpolates them client-side ("insufficient
arguments"). Because that is raw text, the auditor first proves via pg_query_go
that the statement is exactly one statement.

**Index availability is distinct from cost on an empty table.** The audit keeps
real schema/statistics and sets `enable_seqscan=off` on its own connection.
PostgreSQL may otherwise choose an `EXISTS` sequential scan after a row-width
change even though a usable index exists. A forced sequential scan, or a full
index scan with only a residual filter, still fails. A partial index can serve
its predicate through membership; tautological `WHERE true` and a single
`IS NOT NULL` on a non-null column do not count. A real-database regression
proves an unrelated primary/partial index does not hide a missing predicate
index, and adding the useful index clears the finding.

This is an availability probe, not a production cost benchmark. The populated
`internal/db/querytest` performance suite retains normal planner settings and
checks actual execution time and buffer work.
An indexed equality is the structural rule's heuristic, not a hard row limit.
Creator HTTP lists use paginated queries. Internal catalog `GetAll` operations
still return the full selected collection; their catalog-ID predicate satisfies
the indexed-equality rule, so their earlier exemptions are no longer needed.
The session uses the normal test login with explicit merchant parameters and
session state for queries that call current_merchant_id(). Merchant predicates
must be index-backed; no RLS policy adds a missing predicate for the query.

`supabase/index_advisor` + `hypopg` are **opt-in advice, off in CI**
(`SQLAUDIT_INDEX_ADVISOR=1`). They cannot gate: without statistics index_advisor
recommends an index for an already-indexed query at a 1.5% "improvement" and for
a genuinely unindexed one at 29% — far too small a delta to threshold honestly.
Once plan shape has already proven a problem, it is good at naming the column
list. Enable locally with `apk add postgresql-hypopg` in the vet container plus
index_advisor's SQL. (It calls `DEALLOCATE` internally, which poisons pgx's
statement cache, so its connection uses `QueryExecModeExec`.)

### What it plans

Every sqlc query, and the SQL outside sqlc that runs against the same tables:

- **Stored function and trigger bodies.** `LoadFunctionQueries` reads every
  `billing` function from the catalog and extracts each statement that touches
  a billing table. NEW/OLD fields, variables, arguments and trigger context
  become typed parameters; a trigger function is planned once per table it is
  attached to. Named `function.<fn>.<n>` or `trigger.<fn>.<table>.<n>`.
  Dynamic `EXECUTE` text is not planned; nothing touching a billing table may
  fail to bind silently.
- **Metrics.** `metrics.AuditStatements` compiles every family bucketed by day
  with all its dimensions, and as one total (`metrics.<family>.<shape>`).
- **Archive.** `merchantarchive.AuditStatements` lists each table's export and
  count and every preflight and reference refusal (`archive.<kind>.<table>`).

`unbounded-many` applies to sqlc `:many` queries only; the plan rules apply to
everything.

### Rules

Rule names are shared with host-four's equivalent gate so allowlists stay
portable. `unindexed-filter` also checks the explicit merchant predicate path.

- **`unbounded-many`** — a `:many` query over a merchant-scoped table with
  no `LIMIT` and no bounding predicate. Bounding means `col = $n` on an indexed
  column, or `col = ANY($n)` where col plus `merchant_id` covers a whole unique
  key (so the caller's list caps the rows), or a `GROUP BY` whose every key the
  same WHERE pins with an AND-ed `= $n`, literal or `= ANY($n)` (one row per
  caller-supplied value). `merchant_id` alone never bounds:
  one merchant's entire table still grows with records on file.
- **`unscoped-write`** — `UPDATE`/`DELETE` pinning neither `merchant_id` nor a
  key, and not fed by a `LIMIT`ed claim CTE.
- **`seq-scan`** — the availability probe still needs a full heap/index scan
  without an index condition or a restricting partial-index predicate.
- **`unplannable`** — the parser or EXPLAIN could not analyse the query. Fails
  like any other finding; nothing is ever silently skipped.
- **`unindexed-filter`** — the query looks something up by `col = $n`, the scan
  is narrowed by nothing but `merchant_id`, and no index on that table covers
  `col`. A merchant_id index alone must not hide a missing lookup index.
- **`unpruned-partition`** — a scan of a partitioned table (`usage_events`,
  `admission_operations`) carries no predicate on its partition key, so every
  partition is read. Partition scans are folded into their table before the
  rules run, so a table is reported once however many partitions it has.

## AUDIT_ALLOWLIST.txt

**PERMANENT — operator-declared catalog/config.** `products`, `prices`, `psps`,
`custodians`, `merchant_webhooks`, `catalog_meters`, default `catalog_rate_cards`,
`merchant_secrets`. Row counts follow the merchant's own configuration, not
customer activity, so listing them whole does not scale with records on file.

A residual filter on a scan whose index condition and filter together pin
every primary-key column (`merchant_id` and `id` on tenant tables) is a
compare-and-swap guard on one row, never a lookup, and is not flagged.

**PERMANENT — capped by a caller-supplied list.**
`SnapshotPaymentCards` is capped
by `transaction_ids[]` and index-backed by
`payments_rail_transaction_id_idx`; a `UNIQUE(merchant_id, rail,
transaction_id)` would make it provable.

**PERMANENT — optional admin filters.** `($n IS NULL OR col = $n)` on a paged
listing. The predicate is absent on most calls, so no index serves it
generically; the merchant index bounds the scan, the page `LIMIT` the result.

**PERMANENT — merchant archive over partitioned tables.** An archive is the
merchant's whole retained history, so its export, count and in-flight refusal
on `usage_events` and `admission_operations` read every partition by
definition. It is an operator action, never a request or a routine job.

**PERMANENT — existence over every retained month.** "Has this meter ever been
used" (`UsageEventsExistForTypes`, the `*UsageMeter*WithCatalog` reads) and
"has this merchant any usage" (`HasUsageActivity`) cannot name a time range:
the answer is about all retained history. Each is one index probe per
partition (an `EXISTS`, or the newest row through a backward scan), and none
runs on a request's money path.

`LockUsageEventsForMeterCorrection` is PERMANENT `unplannable`: it holds a
table-level transaction lock so an event insert cannot race a meter's semantic
correction. PostgreSQL does not permit `EXPLAIN LOCK TABLE`; execution and the
surrounding concurrency test are the applicable proofs.

**PERMANENT — aggregates over a closed vocabulary or time bucket.** The fleet
dashboard reads (`Fleet*`) and `CountOpenCatalogDriftByKind` group by currency,
rail, drift kind or week: the result has one row per bucket, never one per
record. The fleet reads are on-demand operator analytics, never a merchant list.

**PERMANENT — catalog-bounded diff.** `ListOpenCatalogDriftEvents` returns every
open `catalog.*` finding so the drift pass can resolve what it no longer
observes; the set is bounded by the merchant's catalog and PSPs.

**DEBT (or#837).** Everything else in the DEBT section. These are real:

- *Deployment-wide or merchant-wide scans with no LIMIT* — the Solana converge
  scans, the reconciliation findings scans, `ListStuckProviderIntents`,
  `ListInvoicePayers` and `ListChargeableOpenInvoices`.
- *Unbounded fan-out* — `…ByPriceIDs`, `…ByPaymentMethodIDs`, `…ByCustomerIDs`,
  `ListPaymentMethodsByRails` and `ListRecordedSubscriptionCharges`. The
  caller's list is bounded but each element's row set is not.

## Inline SQL (`TestNoInlineSQL`)

A SQL string literal in non-test Go outside `internal/db/gen` fails the unit
suite unless its file (or directory) is in `inlineSQLAllowed`, which states the
reason per entry. What stays inline is what sqlc cannot express:

- **Transaction/session control** — `SET TRANSACTION`, `SET LOCAL`.
- **Runtime identifiers** — River's own `river_job` table in a schema named at
  runtime (`internal/river`), and `internal/merchantarchive`'s per-table
  export/insert/check statements, built from reviewed archive profiles (row
  values stay bound parameters).
- **Catalog introspection** — `pg_catalog` / `information_schema` reads.
- **Generated SQL** — metrics compiled from definitions (`internal/modules/metrics`).
- **Schema bootstrap** — `internal/migrate/migrator.go` (see below).

Static statements in those files (advisory locks, GUC `set_config`, restore
function calls) are sqlc queries like any other. An allowance whose file no
longer holds inline SQL fails as stale.

## Library schema initialization

`internal/migrate/migrator.go` applies the embedded migrations through migratekit
and creates the managed River schema. The configured schema is an identifier, so
this initialization SQL is outside sqlc's runtime query catalog. The standalone
AuthKit initializer delegates to AuthKit's migration API and contains no raw
SQL. Embedded billing never initializes a host-owned River fleet.
