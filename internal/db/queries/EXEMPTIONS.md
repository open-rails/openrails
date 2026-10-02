# SQL gate exemptions

Two gates run in CI (`task sqlc-check`), on top of `sqlc vet`'s `db-prepare`
correctness check:

| gate | what it proves | allowlist |
|---|---|---|
| `internal/db/sqlaudit` | query scope, declared bounds and index availability | `AUDIT_ALLOWLIST.txt` |
| `scripts/sql-lint.sh` | no hand-written SQL outside `internal/db/gen` | `LINT_ALLOWLIST.txt` |

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

### Rules

Rule names are shared with host-four's equivalent gate so allowlists stay
portable. `unindexed-filter` also checks the explicit merchant predicate path.

- **`unbounded-many`** — a `:many` query over a merchant-scoped table with
  no `LIMIT` and no bounding predicate. Bounding means `col = $n` on an indexed
  column, or `col = ANY($n)` where col plus `merchant_id` covers a whole unique
  key (so the caller's list caps the rows). `merchant_id` alone never bounds:
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

## AUDIT_ALLOWLIST.txt

**PERMANENT — operator-declared catalog/config.** `products`, `prices`, `psps`,
`custodians`, `merchant_webhooks`. Row counts follow the merchant's own
configuration, not customer activity, so listing them whole does not scale with
records on file.

**PERMANENT — capped by a caller-supplied list.**
`SnapshotPaymentCards` is capped
by `transaction_ids[]` and index-backed by
`idx_payments_merchant_rail_transaction`; a `UNIQUE(merchant_id, rail,
transaction_id)` would make it provable.

**PERMANENT — optional admin filters.** `($n IS NULL OR col = $n)` on a paged
listing. The predicate is absent on most calls, so no index serves it
generically; the merchant index bounds the scan, the page `LIMIT` the result.

**DEBT (or#837).** Everything else. These are real:

- *Deployment-wide sweeps with no LIMIT* — `ListDueDunningSubscriptions` (the
  or#837 flagship: runs every 4h across the deployment), the converge scans, and
  the reconciliation/drift/intent scans.
- *Unbatched retention and expiry writes* — `DeleteCompletedWebhookEventsBefore`,
  `DeleteNotificationsBefore`, `DeleteSeenNotificationsBefore`,
  `ExpireCheckoutSessions`, `AutoResolveVanishedReconciliationFindings`. A large
  backlog makes each one a single long transaction.
- *Missing indexes* — `solana_subscriptions.merchant_id` (its merchant predicate is
  not index-backed; the only true `Seq Scan` in the codebase),
  `grants.payment_id`,
  `checkout_sessions.payment_id`, `checkout_sessions.subscription_id`,
  `reprice_batches.price_key`.
- *Unbounded fan-out* — `…ByPriceIDs`, `…ByPaymentMethodIDs`, `…ByCustomerIDs`.
  The caller's list is bounded but each element's row set is not.

## LINT_ALLOWLIST.txt

**PERMANENT** covers what sqlc cannot express: the DB layer itself (`MerchantTx`
GUCs, the schema-rewrite wrapper, advisory locks), SQL built
dynamically from operator definitions (metrics, fleet analytics, dump/restore
over a dynamic table list), and privileged access that runs before merchant
context exists (DEK bootstrap, merchant secret stores).

`internal/merchantarchive/archive.go` and `checks.go` are PERMANENT: the typed
archive profiles determine table/column projections, insert statements and
schema coverage checks at runtime. The checks also inspect PostgreSQL catalogs
for unclassified columns and required merchant coordinates. Identifiers come only from reviewed
profiles/classifications; merchant IDs and row values remain bound parameters.
The same transaction owns snapshot isolation, session settings, retained-row
inserts and restore-guard calls, so export/restore either validates the complete
billing book or refuses it atomically. sqlc cannot express those dynamic profiles
or transaction controls.

`internal/merchants/restore_identity.go` is PERMANENT privileged pre-context
provisioning: it creates the destination merchant directory identity before a
merchant context exists, using the directory pool rather than a merchant-scoped
billing connection. The caller authorizes the destination group/host authority;
the insert preserves the source UUID without rebinding an existing identity.
It follows the same pre-context boundary as the merchant credential stores.

Two more sit in the GUC group: `internal/db/merchant_scope.go` reads
`app.merchant_id` via `current_setting` (`AssertMerchantScope` checks the LIVE
session, which is the whole point — a context value would prove nothing), and
`internal/integrationharness/harness.go` sets it via `set_config`. Neither is a
query.

`internal/migrate/reset.go` is PERMANENT because the operator-only embedded
reset must keep its fixed schema DDL, migratekit-ledger delete and advisory lock
inside one `pgx` transaction. sqlc cannot express the DDL or lock, and splitting
the exact ledger delete from that transaction would remove the reset's rollback
guarantee. The target identity, allow-list and confirmation are validated before
the transaction starts.

`internal/river/progress.go` is PERMANENT for a different reason: it reads
**River's own** `river_job` table, which is not part of OpenRails' schema, is
created by River's migrator rather than `migrations/`, and lives in a schema
named at runtime (`config.RiverSchema`). sqlc has no type information for it and
could not express the schema-qualified name anyway. Only the schema is
interpolated, after an identifier check; the kind list is a bound parameter.
`internal/river/job_liveness.go` is PERMANENT for the same reason, and it
WRITES: while an OpenRails job runs it refreshes that job's
`river_job.attempted_at` — the one column River's rescuer reads to decide a
running job is stuck — because River has no heartbeat API (xs-007 row 31). One
UPDATE by primary key; only the schema is interpolated, after the same check.
`internal/river/job_rescue.go` is PERMANENT for the same reason: it returns
running OpenRails jobs whose liveness beat stopped, which River's rescuer
skips for timeout-free jobs.

`LockUsageEventsForMeterCorrection` is also PERMANENT, but remains in sqlc: it
holds a table-level transaction lock so an event insert cannot race a meter's
semantic correction. PostgreSQL does not permit `EXPLAIN LOCK TABLE`, so the
auditor cannot plan it; execution and the surrounding concurrency test are the
applicable proofs.

**DEBT** is ordinary queries not yet ported to `internal/db/queries/*.sql`.
Nothing about them requires raw SQL.

## Library schema initialization

`internal/migrate/migrator.go` applies the embedded migrations through migratekit
and creates the managed River schema. The configured schema is an identifier, so
this initialization SQL is outside sqlc's runtime query catalog. The standalone
AuthKit initializer delegates to AuthKit's migration API and contains no raw
SQL. Embedded billing never initializes a host-owned River fleet.
