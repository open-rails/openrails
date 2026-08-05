# SQL gate exemptions

Three gates run in CI (`task sqlc-check`), on top of `sqlc vet`'s `db-prepare`
correctness check:

| gate | what it proves | allowlist |
|---|---|---|
| `internal/db/sqlaudit` | every query is bounded and index-backed | `AUDIT_ALLOWLIST.txt` |
| `scripts/sql-lint.sh` | no hand-written SQL outside `internal/db/gen` | `LINT_ALLOWLIST.txt` |
| `scripts/migration-lint.sh` | new migrations are lock-safe (squawk) | `.squawk.toml` + inline `squawk-ignore` |

Every allowlist entry is classified **PERMANENT** (bounded by design) or
**DEBT** (a real bug, kept only so the gate could be switched on), carries a
mandatory rationale, and is rejected if duplicated. An entry that stops tripping
fails the build as stale — fixing a query deletes its line.

## How the query auditor works

`sqlc vet` PREPAREs each query, which proves it is valid SQL and nothing more.
The auditor connects to the same throwaway vet DB, EXPLAINs every query, walks
the plan in Go and applies four rules. Two facts make that meaningful on a
database built from `migrations/` with zero rows:

**`EXPLAIN (GENERIC_PLAN, FORMAT JSON)`** (PG16+) plans a parameterized
statement without values, so no parameters are fabricated. All 526 queries plan;
none are skipped. It must be sent over the **raw simple-query protocol**
(`conn.PgConn().Exec`): pgx's extended protocol binds the query's own `$n` as
parameters of the EXPLAIN ("expected N arguments, got 0"), and
`QueryExecModeSimpleProtocol` interpolates them client-side ("insufficient
arguments"). Because that is raw text, the auditor first proves via pg_query_go
that the statement is exactly one statement.

**Statistics are left alone — deliberately.** Inflating
`pg_class.reltuples/relpages` does *not* work and actively backfires:
`estimate_rel_size` takes the page count from the **physical file**, so an empty
table yields `density × 0 = 0` rows, and a non-zero `relpages` simultaneously
disables the "empty table ⇒ assume 10 pages" fallback. Measured here: every plan
collapsed to `cost=0.00` Seq Scans. Postgres's default 10-page estimate already
discriminates correctly — a query with a usable index plans as an Index Scan,
one without as Seq Scan + Filter. Forcing it with `enable_seqscan = off` was
measured across all 526 queries and changed *nothing*, so it is not used.
Nothing here judges cost.

The session runs as **`openrails_app` with `app.merchant_id` set**, so RLS
predicates appear in the plan exactly as production sees them — which is also
how the auditor verifies the RLS `merchant_id` predicate is index-backed.

`supabase/index_advisor` + `hypopg` are **opt-in advice, off in CI**
(`SQLAUDIT_INDEX_ADVISOR=1`). They cannot gate: without statistics index_advisor
recommends an index for an already-indexed query at a 1.5% "improvement" and for
a genuinely unindexed one at 29% — far too small a delta to threshold honestly.
Once plan shape has already proven a problem, it is good at naming the column
list. Enable locally with `apk add postgresql-hypopg` in the vet container plus
index_advisor's SQL. (It calls `DEALLOCATE` internally, which poisons pgx's
statement cache, so its connection uses `QueryExecModeExec`.)

### Rules

Rule names are shared with tensorhub's equivalent gate so allowlists stay
portable. `unindexed-filter` is openrails-only: tensorhub has no RLS.

- **`unbounded-many`** — a `:many` query over a merchant-scoped table with
  no `LIMIT` and no bounding predicate. Bounding means `col = $n` on an indexed
  column, or `col = ANY($n)` where col plus `merchant_id` covers a whole unique
  key (so the caller's list caps the rows). `merchant_id` alone never bounds:
  one merchant's entire table still grows with records on file.
- **`unscoped-write`** — `UPDATE`/`DELETE` pinning neither `merchant_id` nor a
  key, and not fed by a `LIMIT`ed claim CTE.
- **`seq-scan`** — planner-proven: no usable index exists.
- **`unplannable`** — the parser or EXPLAIN could not analyse the query. Fails
  like any other finding; nothing is ever silently skipped.
- **`unindexed-filter`** — the query looks something up by `col = $n`, the scan
  is narrowed by nothing but `merchant_id`, and no index on that table covers
  `col`. This is what a missing index looks like *under RLS*, where the
  merchant_id index always hands the planner some index path.

## AUDIT_ALLOWLIST.txt

**PERMANENT — operator-declared catalog/config.** `products`, `prices`, `psps`,
`custodians`, `alert_rules`, `merchant_webhooks`. Row counts follow the merchant's own
configuration, not customer activity, so listing them whole does not scale with
records on file.

**PERMANENT — capped by a caller-supplied list.**
`LookupCustomerIDsBySubjects` is capped by `subjects[]`;
`uq_customers_merchant_subject` is a *partial* unique index and the auditor
deliberately refuses to credit partial indexes. `SnapshotPaymentCards` is capped
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
- *Missing indexes* — `solana_subscriptions.merchant_id` (its RLS predicate is
  not index-backed; the only true `Seq Scan` in the codebase),
  `product_usage_limit_bindings` (no index at all), `grants.payment_id`,
  `checkout_sessions.payment_id`, `checkout_sessions.subscription_id`,
  `reprice_batches.price_key`.
- *Unbounded fan-out* — `…ByPriceIDs`, `…ByPaymentMethodIDs`, `…ByCustomerIDs`.
  The caller's list is bounded but each element's row set is not.

## LINT_ALLOWLIST.txt

**PERMANENT** covers what sqlc cannot express: the DB layer itself (`MerchantTx`
GUCs, RLS probes, the schema-rewrite wrapper, advisory locks), SQL built
dynamically from operator definitions (metrics, fleet analytics, dump/restore
over a dynamic table list), and privileged access that runs before merchant
context exists (DEK bootstrap, merchant secret stores).

Two more sit in the GUC group: `internal/db/merchant_scope.go` reads
`app.merchant_id` via `current_setting` (`AssertMerchantScope` checks the LIVE
session, which is the whole point — a context value would prove nothing), and
`internal/integrationharness/harness.go` sets it via `set_config`. Neither is a
query.

`internal/river/progress.go` is PERMANENT for a different reason: it reads
**River's own** `river_job` table, which is not part of OpenRails' schema, is
created by River's migrator rather than `migrations/`, and lives in a schema
named at runtime (`config.RiverSchema`). sqlc has no type information for it and
could not express the schema-qualified name anyway. Only the schema is
interpolated, after an identifier check; the kind list is a bound parameter.

**DEBT** is ordinary queries not yet ported to `internal/db/queries/*.sql`.
Nothing about them requires raw SQL.

## .squawk.toml

`assume_in_transaction` is set because migratekit's `applyOne()` does
`BeginTx` / `Exec(whole file)` / `Commit`, and `scripts/sqlc-vet-db.sh` mirrors
that with `psql -1`. Those are the only two apply paths — there is no
non-transactional one — which is why migrations use `SET LOCAL` for their
timeouts rather than `SET`.

`0001` is excluded by path (the squashed baseline creates the schema from
nothing, so lock-safety rules are vacuous) and `0002`-`0009` predate the gate.
**Nothing else is excluded by path.** `0010`-`0033` were brought clean under
or#887; everything still exempt is an inline `-- squawk-ignore <rule>` written
at the statement with its reason on the lines above it, so the rest of the file
stays linted and the reason sits where the next person edits.

### The two rules this migrator cannot satisfy

`require-concurrent-index-creation` and `require-concurrent-index-deletion` are
in `excluded_rules`. They are not judgement calls — they are unsatisfiable here,
verified both ways:

```
BEGIN; CREATE INDEX CONCURRENTLY …;
  ERROR:  CREATE INDEX CONCURRENTLY cannot run inside a transaction block
BEGIN; DROP INDEX CONCURRENTLY …;
  ERROR:  DROP INDEX CONCURRENTLY cannot run inside a transaction block
```

and squawk, run with `assume_in_transaction`, fires
`ban-concurrent-index-creation-in-transaction` on the very edit the rule asks
for. Between them the pair accounted for 52 of the gate's 96 original findings.
Excluding a rule that cannot apply is honest; excluding a *file* from a rule
that does apply is not, which is the distinction `excluded_paths` above holds
to.

The alternative is a non-transactional migration mode — per-file
`-- migratekit:no-transaction`, applied statement-by-statement outside a
transaction, with every such file responsible for its own idempotency (an
interrupted `CREATE INDEX CONCURRENTLY` leaves an INVALID index behind and must
be re-runnable). That is a change to migratekit, not to this repo, plus a
matching change to `sqlc-vet-db.sh`, plus a review of what "half-applied
migration" means for the ledger. Roughly two days, and it buys nothing until
the schema is large enough that a non-concurrent index build actually blocks
production writes. Deliberately not started.

### The inline exemptions

Forty-five statements, sixty-four rule instances, all classified
**PERMANENT**. All but `0048`, `0061`, `0063` and `0077` are **history** —
rewriting them changes no live database and the honest record is what actually
ran. The rules stay armed for new migrations.

| where | rule(s) | why |
|---|---|---|
| `0014` ledger_transfers CHECK | `constraint-missing-not-valid` | `NOT VALID` buys nothing inside a single-transaction migrator — the ADD's `ACCESS EXCLUSIVE` lock is held to COMMIT either way. The two-step only pays off across two transactions, i.e. two migration files. |
| `0018` ×4 destructive_run FKs | `adding-foreign-key-constraint`, `constraint-missing-not-valid` | The referencing columns were added `NULL` a few statements earlier and the referenced table is created by the same file, so the validating scan is over an all-NULL column. squawk suppresses both rules when the *referencing* table is new, but cannot see this case. |
| `0025` vault_provider→custodian | `renaming-column` | Deliberate greenfield hard cut (or#880): no alias, no compatibility view. A caller still naming `vault_provider` must fail loudly. |
| `0025` custodian CHECK | `constraint-missing-not-valid` | The `UPDATE` two lines above normalises the rows the CHECK then validates, in the same transaction. |
| `0026` money_movement CHECK | `constraint-missing-not-valid` | Same as `0014`. |
| `0027` merchant_exports→merchant_purge_inventories | `renaming-table`, `renaming-column` | Same hard-cut rationale as `0025`; breaking a client that still names `merchant_exports` is the point of or#858. |
| `0031` vault_fingerprint→fingerprint | `renaming-column` | Same hard-cut rationale as `0025` (or#871): `vault` is reserved for HashiCorp Vault, and a caller still naming `vault_fingerprint` must fail loudly rather than read a stale alias. |
| `0031` payments token_type CHECK | `constraint-missing-not-valid` | The two `UPDATE`s immediately above rewrite every row the re-added CHECK then validates, in the same transaction — the constraint is dropped and restored only to move `provider_vault`/`pan_via_vault` to their new names. |
| `0044` host_lifecycle_events.currency | `adding-not-nullable-field` | The rule's own suggested fix — stay nullable, add a `CHECK` — provably cannot satisfy the invariant it exists for: CUR-1 reads `information_schema.columns.is_nullable`, so only the column attribute counts. The scan is inherent to `SET NOT NULL`, not the constraint two-step the sibling rule covers, so no file split reduces it. The table is a delivery feed created in `0037` and pruned after delivery, so the scan is over a small, short-lived table. |
| `0060` rail_refresh_watermarks PSP cut | `ban-drop-column`, `adding-not-nullable-field`, `disallowed-unique-constraint`, `adding-foreign-key-constraint`, `constraint-missing-not-valid` | or#893 deletes the table's NULL/global lane. `psp_key` is a generated column that existed only to key that lane; the `DELETE … WHERE psp_id IS NULL` two statements earlier removes every row the `SET NOT NULL` and the re-keyed UNIQUE then validate. The table is a resumable cursor — one row per (merchant, rail, PSP, domain) — so every scan here is over a handful of rows, and `NOT VALID` buys nothing inside a single-transaction migrator (see `0014`). |
| `0061` ×12 write-only column drops | `ban-drop-column` (×21) | **Deliberate hard cut, not history.** or#823 drops the columns that are written and read by nothing — the rule's concern is breaking a client that reads them, and the verified fact that qualified each column is that no such client exists (no sqlc query, no generated reader, no raw SQL). Prelaunch: no deployment holds rows, and a column kept "just in case" reads to the next person as evidence something consumes it. The lock is `ACCESS EXCLUSIVE` either way and `DROP COLUMN` is metadata-only in Postgres, so there is no cheaper shape to split this into. |
| `0085` catalog_meters.kind drop | `ban-drop-column` | or#893 phase 5 makes the column dead **in its own change set**: the manifest loses `Meter.Kind`, the applier stops writing it, and the rating join loses the `missing_default` CASE that was its last reader. The rule's concern is breaking a client that reads the column; after this commit there is none, in this repo or in any consumer (the manifest field it mirrored no longer exists to send). Prelaunch, `DROP COLUMN` is metadata-only, and the CHECK dropped alongside it constrained nothing else. |
| `0063` ×6 psp_id `SET NOT NULL` (subscriptions, payment_methods, checkout_sessions, rail_intents, rail_mutation_logs, rail_customer_accounts) | `adding-not-nullable-field` | **Deliberate hard cut, not history.** or#893 makes PSP provenance total. The rule's suggested fix — stay nullable, add a `CHECK` — cannot express a column attribute, and `information_schema.is_nullable` is what CUR-1 and the sqlc model shape both read; a nullable column with a CHECK still generates `*uuid.UUID` and still lets a caller pass nil. The `UPDATE`/`DELETE` block above each statement attributed or removed every row the scan validates, in the same transaction. |
| `0063` payments + invoice_payments `psp_required_on_rail` CHECKs | `constraint-missing-not-valid` | These are the two tables where a CHECK is the RIGHT shape (both also record off-rail money), so only the lock argument applies: `NOT VALID` buys nothing inside a single-transaction migrator — see `0014`. The rows it validates were just rewritten by the same transaction. |
| `0063` rail_mutation_logs + rail_customer_accounts psp FKs | `adding-foreign-key-constraint`, `constraint-missing-not-valid` | Both re-point at `openrails.psps` after the column closed: the mutation-log FK moves `ON DELETE SET NULL` → `CASCADE` (SET NULL is unrepresentable against NOT NULL — the same choice `0060` made for the watermark cursor), and rail_customer_accounts' is a brand-new column whose rows were attributed or deleted three statements earlier. Same single-transaction argument as `0014`. |
| `0063` ×2 rail-transaction unique rebuilds | `disallowed-unique-constraint` | The rule wants a `UNIQUE` constraint rather than a unique index; every equivalent invariant in this schema is a PARTIAL unique index (`WHERE deleted_at IS NULL`, `WHERE rail_subscription_id <> ''`), which a table constraint cannot express at all. Dropping the partial predicate to satisfy the rule would change the invariant. |
| `0077` ×2 rail_intents/rail_mutation_logs custodian FKs | `adding-foreign-key-constraint`, `constraint-missing-not-valid` | Composite `(custodian_id, merchant_id)` FKs, the same shape `psps_custodian_fk` carries, so the reference cannot cross a merchant boundary. The column is added in the statement above each, so every existing row is NULL and the validating scan is over an all-NULL column — the case `0018` records squawk cannot see. `NOT VALID` buys nothing inside a single-transaction migrator (`0014`). |
| `0077` ×2 psp_id `DROP NOT NULL` | `ban-drop-not-null` | **Deliberate, and not a weakening.** or#795's batch account updater is addressed to a CUSTODIAN — one custodian backs many PSPs, so no single `psp_id` names the write. The `rail_intents_addressed` / `rail_mutation_logs_addressed` CHECKs added two lines below each restate `0063`'s actual invariant (*an outbound write names the account it will execute against*) over both kinds of account, so nothing becomes unattributable. The rule's concern is a client that assumes non-NULL; the readers were changed in the same PR and the CHECK is what they now rely on. |
| `0077` ×2 addressed CHECKs | `constraint-missing-not-valid` | Every existing row has `psp_id` NOT NULL — it was the column attribute until two statements earlier — so the scan validates rows already known to satisfy the constraint. Same single-transaction argument as `0014`. |
| `0048` DROP payer_spend_limits | `ban-drop-table` | **Deliberate hard cut, not history.** or#897 replaces the table with `billing_policies` + `billing_policy_bindings`; the rule's concern (breaking existing clients) is the intended effect, and prelaunch there is no deployment holding rows worth keeping. Leaving the table alongside its replacement would be the two-substrate disease or#878 removed from exposure. |

A new migration that genuinely needs one of these must add the constraint
`NOT VALID` and `VALIDATE CONSTRAINT` it in a *later* file — one transaction
each. That is the only shape that actually reduces lock time here.
