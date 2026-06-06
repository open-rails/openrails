# Tenant-aware core data model (issue #223)

Keystone refactor that makes OpenRails core tenant-aware so one shared app/DB
can serve many tenants, while **self-hosted single-tenant installs run the SAME
code paths** with one default tenant namespace. This is the prerequisite that
unblocks #221 (org-owned credits) and #222 (OAT public tenant routes).

This document describes the **first, additive increment** that has landed and,
crucially, the **remaining work** so the rest can be completed without
re-discovering the plan.

## What landed (this increment)

### 1. Migration `039_tenant_aware_core`

- Creates `billing.tenants` — the tenant / billing-namespace directory. It is a
  **global** (control-plane) table, not itself tenant-scoped.
- Seeds **one** default tenant: slug `default`, deterministic id
  `00000000-0000-0000-0000-000000000001`.
- Adds a **NULLABLE** `tenant_id UUID` column (with a column `DEFAULT` of the
  default tenant) to every tenant-owned table, and **backfills** existing rows
  to the default tenant.
- Adds new **tenant-scoped indexes** for the hot paths (entitlements, credit
  balances/transactions/blocks).
- It is **additive and backward-compatible**:
  - `tenant_id` is **not** `NOT NULL` yet.
  - No existing unique constraint / index is dropped or rewritten.
  - No cross-table FK to `billing.tenants` is added yet.

  > **Cross-repo coupling:** host applications may read `billing.entitlements`
  > via **direct SQL** from their own processes. Adding a nullable column and
  > extra indexes does not break `SELECT ... FROM billing.entitlements WHERE
  > user_id = ...` reads, so those direct readers keep working unchanged. Do
  > **not** make `tenant_id` `NOT NULL` or change the entitlements unique/overlap
  > constraints until those readers are coordinated.

The `.down.sql` is documentation-only (migratekit's `LoadFromFS` applies only
`*.up.sql`), matching this repo's convention.

### 2. Tenant-context primitive — `pkg/tenant`

- `tenant.ID` — typed tenant id (wraps `uuid.UUID`).
- `tenant.DefaultID` / `tenant.DefaultSlug` — the well-known default tenant
  matching the migration seed.
- `tenant.WithID(ctx, id)` / `tenant.FromContext(ctx)` — context plumbing.
- `tenant.FromContextOrDefault(ctx)` — returns the resolved tenant, or the
  **default** tenant when none was resolved. This is what lets single-tenant and
  multi-tenant share one query path.

### 3. Resolution plumbing — `middleware.ResolveTenant()`

Mounted in the public engine **before** authorization and before any
tenant-owned DB access. For now it resolves to the default tenant; the marked
extension point is where #222 layers host/path/JWT/OAT/delegated-token
resolution. Because downstream code reads the tenant via
`tenant.FromContextOrDefault`, only this resolver needs to change to turn on
multi-tenant routing.

### 4. Representative queries threaded (proof of the pattern)

The **credits** and **entitlements** read paths now scope by tenant and stamp
`tenant_id` on writes:

- `internal/db/repo/entitlement.go`: `IsEntitled`, `HasActiveIndefinite`,
  `GetLatestActive`, `GetLatestFiniteActive` (reads); `Insert` (stamp).
- `internal/modules/credits/credits_service.go`: `GetBalance`,
  `GetTransactions`, `GetTransactionBySource` (reads); `lockBalance`
  (read + stamp on the created balance row).
- Models carry `TenantID`: `Entitlement`, `UserCreditBalance`,
  `CreditTransaction`, `CreditBlock` (`nullzero` + DB default, so inserts that
  leave it zero fall back to the default tenant).

## Remaining work (NOT done — be explicit)

### Query discipline — still to scope

The tenant predicate has been applied to the representative subset above. Every
other query on a tenant-owned table still needs a tenant predicate. Audit list
(files containing user-keyed queries on tenant-owned tables that are **not yet**
tenant-scoped):

- `internal/db/repo/entitlement.go` — remaining methods beyond the four reads +
  `Insert` already done (e.g. list/timeline/update/revoke paths).
- `internal/db/repo/entitlement_timeline.go`
- `internal/db/repo/admin_grant.go`
- `internal/db/repo/checkout_session.go`
- `internal/db/repo/notification_queue.go`
- `internal/db/repo/payment.go`
- `internal/db/repo/payment_method.go`
- `internal/db/repo/subscription.go`
- `internal/db/repo/product.go`, `price.go`, `credit_type.go`,
  `processor_customer.go` (catalog/global-ish but still tenant-owned per #223)
- `internal/modules/credits/credits_service.go` — **mutation** paths still
  unscoped: `Deposit`/`depositTx`, `Withdraw`/`withdrawTx`, `Hold`,
  `CaptureHold`, `ReleaseHold`, `withdrawBalanceAndBlocks`, and the
  `credit_transactions` / `credit_blocks` inserts (these must also stamp
  `tenant_id`). See coordination note with #221 below.
- `internal/modules/entitlements/entitlement_service.go`
- `internal/modules/payments/processor_customer_service.go`
- `internal/modules/subscriptions/cleanup.go`
- Background jobs / River workers (`internal/river/...`), the audit checks
  (`internal/audit/...`), and metrics — must carry tenant context on every
  job/worker operation, audit row, and metric (issue #223 "TENANT RESOLUTION &
  CONTEXT").

### Schema hardening — deferred follow-up migrations

1. Add tenant-scoped **UNIQUE** replacements and retire the old global uniques
   in lockstep with all writers being tenant-aware. Affected uniques today:
   - `processor_customers (user_id, processor)` and `(processor, customer_id)`
   - credit balance and ledger uniqueness, now keyed by
     `(tenant_id, tenant_subject_id, credit_type_id)` plus idempotency coordinates
   - `payment_methods (user_id, vault_id)` / `(processor, vault_id)`
   - `subscriptions` lifecycle-owner uniques
   - `entitlements` overlap-exclusion + `uniq_entitlements_active`
   - `payments (processor, transaction_id)`, checkout/rebill uniques
   - **provider identities** (`processor`, external customer/transaction ids)
     must become tenant-scoped per #223.
2. Enforce `tenant_id NOT NULL` once every writer stamps it and every reader
   scopes it (and after direct-SQL readers in host apps are coordinated).
3. Evaluate **Postgres RLS** as defense-in-depth: set tenant id per transaction
   and add tests proving cross-tenant access is denied even when a query forgets
   the tenant predicate.

### Resolution — deferred to #222

`resolveTenantID` currently returns the default tenant. #222 fills in
host/subdomain, `/t/:tenant/...` path, OAT-owning-tenant, and delegated browser
token (`tenant` claim) resolution, including looking the slug up in
`billing.tenants`.

## Coordination notes / open questions

- **Payer / invoker vocabulary:** durable OpenRails billing identity is
  `tenant_id` + `tenant_subject_id` + `invoker_id`. `tenant_id` is the
  deployment/application namespace; `tenant_subject_id` is the AuthKit org or
  personal-org whose balance is charged; `invoker_id` is the user/service/OAT
  principal causing the operation. Billing credit, usage, admission, and budget
  storage use those physical column names; service API fields use
  `tenant_subject_id` and `invoker_id`.
- **AuthKit resource-scoped OATs:** standalone OpenRails OATs are opaque AuthKit
  Organization Access Tokens. AuthKit stores permissions plus opaque resource
  scopes; OpenRails interprets `openrails.tenant=<tenant_uuid>` and optional
  `openrails.tenant_subject=<authkit_org_uuid>`. Permissions say what the token may
  do; resources say where it may do it.
- **Host application direct SQL reads of `billing.entitlements`:** the additive
  column is safe today, but before `tenant_id` becomes `NOT NULL` or before any
  entitlements unique/overlap constraint is rescoped by tenant, those repos'
  direct queries must be updated to filter by `tenant_id` (or always run as the
  default tenant). **Open question:** are host apps each a single tenant
  (so they can hardcode the default/their tenant id), or will one deployment
  host multiple tenants reading the same table?
