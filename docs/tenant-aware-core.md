# Tenant-aware core data model (issue #223)

Keystone refactor that makes OpenRails core tenant-aware so one shared app/DB
can serve many tenants, while **self-hosted single-tenant installs run the SAME
code paths** with one default tenant namespace. This is the prerequisite that
unblocks #221 (tenant-subject-owned credits) and #222 (service-token public tenant routes).

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
- The planned hard-cut direction is:
  - tenant-owned rows must carry the tenant namespace until the table moves to
    the `tenant_subject_id` model.
  - old org/payer/account vocabulary should be removed instead of aliased.
  - host applications should use OpenRails APIs or current tenant-subject
    columns, not direct SQL against old user/org fields.

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
extension point is where #222 layers host/path/JWT/service-token/delegated-JWT
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

1. Tenant-scoped **UNIQUE** replacements (done in migration 039; the payable
   identity column moved from `user_id` to `tenant_subject_id` in #317 migration
   078, which recreated these uniques and dropped `user_id`). Affected uniques:
   - `processor_customers (tenant_id, tenant_subject_id, processor)` and `(tenant_id, processor, customer_id)`
   - credit balance and ledger uniqueness, keyed by
     `(tenant_subject_id, credit_type_id)` plus idempotency coordinates
   - `payment_methods (tenant_id, tenant_subject_id, vault_id)` / `(tenant_id, processor, vault_id)`
   - `subscriptions` lifecycle-owner uniques (now on `tenant_subject_id`)
   - `entitlements` overlap-exclusion + active-window uniques (on `tenant_subject_id`)
   - `payments (tenant_id, processor, transaction_id)`, checkout/rebill uniques
   - **provider identities** (`processor`, external customer/transaction ids)
     must become tenant-scoped per #223.
2. Enforce `tenant_id NOT NULL` once every writer stamps it and every reader
   scopes it (and after direct-SQL readers in host apps are coordinated).
3. Evaluate **Postgres RLS** as defense-in-depth: set tenant id per transaction
   and add tests proving cross-tenant access is denied even when a query forgets
   the tenant predicate.

### Resolution — deferred to #222

`resolveTenantID` currently returns the default tenant. #222 fills in
host/subdomain, `/t/:tenant/...` path, service-token-owning tenant, and delegated
browser JWT (`tenant` claim) resolution, including looking the slug up in
`billing.tenants`.

## Coordination notes / open questions

- **Tenant subject / invoker vocabulary:** durable OpenRails billing identity is
  `tenant_subject_id` plus optional `invoker_id`. `tenant_subject_id` identifies
  the payable OIDC subject under a tenant (`tenant + issuer + subject`);
  `invoker_id` identifies the principal causing the operation when it differs
  from the charged subject. Billing credit, usage, admission, and budget storage
  should use those physical column names; service API fields use
  `tenant_subject_id` and `invoker_id`.
- **Canonical AuthKit/OIDC vocabulary:** use
  `docs/authkit-tenant-oidc-glossary.md` for examples and public wording.
  Bootstrap authority is a deploy action; it should not be described as a
  billable tenant subject or customer tenant.
- **AuthKit resource-scoped service tokens:** standalone OpenRails service
  tokens are opaque server-to-server credentials. AuthKit stores permissions plus
  opaque resource scopes; OpenRails interprets `openrails.tenant=<tenant_uuid>`
  and optional `openrails.tenant_subject=<tenant_subject_uuid>`. Permissions say
  what the token may do; resources say where it may do it.
- **Host application direct SQL reads of `billing.entitlements`:** the additive
  column is safe today, but before `tenant_id` becomes `NOT NULL` or before any
  entitlements unique/overlap constraint is rescoped by tenant, those repos'
  direct queries must be updated to filter by `tenant_id` (or always run as the
  default tenant). **Open question:** are host apps each a single tenant
  (so they can hardcode the default/their tenant id), or will one deployment
  host multiple tenants reading the same table?
