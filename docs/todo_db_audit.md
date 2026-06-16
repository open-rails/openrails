# DB Audit — Missing FK Constraints

Findings from a schema-vs-DDL audit of `migrations/postgres/001_schema.up.sql`
and `migrations/postgres/015_budget_policies.up.sql`. Many tables visually
"float" in `docs/schema.dbml` because the schema declares `tenant_id NOT NULL`
but no FK to `openrails.tenants`. Some absences are deliberate; others look
like oversights worth a follow-up migration.

Decisions captured here are advisory — flip a checkbox when applied (link the
migration). Discussion is welcome inline.

## Category 1 — Intentional, do not add

These FKs are deliberately absent. Adding them would violate a design
invariant. Documented here so a future reader doesn't propose them again.

- `platform_audit.target_tenant_id` → `tenants.id`
  - **Skip.** Audit must survive tenant delete; any `ON DELETE` behavior
    breaks the guarantee (CASCADE deletes the audit, RESTRICT blocks the
    delete, SET NULL loses the link). Schema comment: "CROSS-TENANT
    control-plane state: NOT purged by tenant delete."
- `platform_break_glass.target_tenant_id` → `tenants.id`
  - **Skip.** Same reasoning — historical elevation records must outlive
    the tenant.
- `tenant_credential_audit.tenant_id` → `tenants.id`
  - **Skip.** Append-only audit, same reasoning.
- `entitlements.source_id` → `(subscriptions | payments | admin_grants | …)`
  - **Skip.** Polymorphic by `source_type ∈ {subscription, one_off, admin,
    grace}`. Can't be a single FK by definition. Visual reference left out
    of the DBML on purpose.

## Category 2 — Likely oversights, propose adding

These rely solely on RLS for isolation today. A misbehaving writer (wrong
tenant UUID, stale parent reference) succeeds silently. All would also clean
up the DBML diagram and give Postgres a real referential-integrity guard.

Suggested batch into one migration, e.g. `016_add_missing_fks.up.sql`.

- [ ] `products.tenant_id` → `tenants.id` `ON DELETE RESTRICT`
  - Don't let a tenant delete leave orphan catalog.
- [ ] `prices.tenant_id` → `tenants.id` `ON DELETE RESTRICT`
- [ ] `entitlement_features.tenant_id` → `tenants.id` `ON DELETE RESTRICT`
- [ ] `product_entitlement_features.tenant_id` → `tenants.id` `ON DELETE CASCADE`
  - Join table; safe to cascade.
- [ ] `catalog_drift_events.tenant_id` → `tenants.id` `ON DELETE CASCADE`
  - Operational record; not audit. Safe to clean up on tenant delete.
- [ ] `reconciliation_runs.tenant_id` → `tenants.id` `ON DELETE CASCADE`
- [ ] `reconciliation_findings.tenant_id` → `tenants.id` `ON DELETE CASCADE`
- [ ] `reconciliation_findings.first_seen_run` → `reconciliation_runs.id`
      `ON DELETE RESTRICT`
  - Referential integrity within the reconciliation system itself.
- [ ] `reconciliation_findings.last_seen_run` → `reconciliation_runs.id`
      `ON DELETE RESTRICT`
- [ ] `provider_intents.tenant_id` → `tenants.id` `ON DELETE CASCADE`
- [ ] `solana_subscriptions.tenant_id` → `tenants.id` `ON DELETE CASCADE`
- [ ] `usage_events.money_transaction_id` → `money_transactions.id`
      `ON DELETE SET NULL`
  - Already nullable; SET NULL preserves the usage event but cuts the
    dangling pointer.

## Category 3 — Defensible but debatable

Decide-then-implement. Each has a real trade-off; the current "no FK" choice
is plausible.

- [ ] `tenant_deks.tenant_id` → `tenants.id` `ON DELETE RESTRICT`
  - **Tension:** The DEK is required to decrypt during the export-before-delete
    flow, so it must outlive the export. A `RESTRICT` FK enforces ordering
    explicitly (delete fails until the DEK is purged separately). Today the
    ordering is convention only.
- [ ] `tenant_secrets.tenant_id` → `tenants.id` `ON DELETE RESTRICT`
  - Same reasoning as `tenant_deks`.
- [ ] `tenant_exports.tenant_id` → `tenants.id` `ON DELETE …`
  - **Tension:** The export manifest *documents* the tenant deletion. Cascading
    deletes the evidence; restricting blocks the delete the export was meant
    to enable. Probably treat like audit and **skip** — move to Category 1.
- [ ] Direct `tenant_id` FK on every tenant-scoped table that currently goes
      through `tenant_subjects` (~30 tables: `payments`, `subscriptions`,
      `entitlements`, `usage_events`, `invoices`, all the `money_*`, all the
      `budget_*`, etc.)
  - **Tension:** There is already a transitive cleanup chain
    `tenant_subject_id → tenant_subjects.tenant_id → tenants.id`. A direct
    FK on `tenant_id` is technically redundant for cascade behavior, but it
    would catch a write-time bug where the row's `tenant_id` doesn't match
    its `tenant_subject.tenant_id` (currently caught only by RLS, which
    relies on the writer to set the GUC correctly). Nice-to-have, not
    essential — comes with index overhead on ~30 tables.

## Dead / write-only columns

Methodology: extracted 521 (table, column) pairs from
`migrations/postgres/{001,015}*.sql`, then for each name counted whole-word
matches across `internal/db/queries/*.sql`, non-generated Go, and embedded
YAML. Generated code (`internal/db/gen/`), `_test.go`, and migration files
were excluded from the search corpus so they could not provide false
positives.

Caveat: this catches the strong signals (column has no references at all,
or only one reference) but does not catch every dead column shape — e.g.
`SELECT *` queries hide unused columns behind a wildcard; jsonb keys
inside `metadata` / `payload` columns are opaque to a column-name grep;
and a column that's read into a Go struct field which itself is never
consumed downstream still counts as "live" here. Deeper passes welcome.

### Fully dead — no read, no write anywhere

- [ ] `tenant_deks.key_version` (`integer NOT NULL DEFAULT 1`)
  - Looks like an unfinished DEK-rotation hook. Zero references in the
    codebase. Drop unless rotation support is actively planned.
- [ ] `tenants.stripe_account_id` (`text`)
  - Probable leftover from a Stripe Connect / multi-account design that
    never landed. Zero references. Drop unless platform-Connect support
    is actively planned.

### Write-only — INSERTed but never SELECTed

These get a value on creation but nothing in the codebase ever reads them.
Either remove them or add a use site (admin UI / audit query).

- [ ] `tenant_exports.completed_at` (`timestamptz`)
  - Set by `internal/tenancy/delete.go` on row insert; no query reads it.
    If you want to expose "when did the export finish?" through an admin
    route, keep it and add the SELECT; otherwise drop.
- [ ] `tenants.provisioned_at` (`timestamptz`)
  - Stamped by `internal/tenancy/lifecycle.go`; no read site. Same
    decision: surface it through an admin / status query or drop.

### Read but possibly never consumed downstream

Hydrated from sqlc, but the Go field it lands in has very few callers — may
be dead at the API surface even though the data layer still pulls it.

- [ ] `reconciliation_findings.first_seen_at`
  - Read into `internal/reconcile/store.go:FirstSeenAt` and forwarded once.
    Worth tracing whether any admin handler actually uses the field. If
    not, drop both column and field.

## How to verify after applying

```sql
-- Every tenant-scoped non-audit table should have a FK to tenants.
SELECT n.nspname || '.' || c.relname AS table_name
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'openrails'
   AND c.relkind = 'r'
   AND EXISTS (
     SELECT 1 FROM pg_attribute a
      WHERE a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped
   )
   AND NOT EXISTS (
     SELECT 1 FROM pg_constraint con
      WHERE con.conrelid = c.oid
        AND con.contype = 'f'
        AND EXISTS (
          SELECT 1 FROM unnest(con.conkey) AS k
            JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k
           WHERE a.attname = 'tenant_id'
        )
   )
 ORDER BY table_name;
```

Output should be a small explicit set (Category 1 / Category 3 skips), not a
surprise list.
