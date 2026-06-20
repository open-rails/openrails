# Merchant-Aware Core Data Model

OpenRails is merchant-aware: every billing row belongs to an explicit merchant,
and there is no default merchant fallback. The merchant id is the isolation key
for Postgres RLS, background jobs, API-key resource scopes, ClickHouse
analytics, and webhook credential lookup.

## Vocabulary

- **Merchant**: OpenRails billing/isolation namespace.
- **Org**: AuthKit ownership and control authority.
- **Customer**: OpenRails payable subject under a merchant.
- **Remote application**: AuthKit registered issuer/JWKS credential that may be
  granted authority through org roles and permissions.

## Schema Shape

The consolidated schema uses:

- `openrails.merchants`
- `merchant_id` on merchant-owned tables
- `app.merchant_id` for RLS pinning
- `openrails.merchant_*` control-plane tables for secrets, exports, and DEKs

Writers must stamp `merchant_id` explicitly or run on a merchant-pinned DB
connection. Missing merchant context is a bug and should fail closed.

## Context Primitive

The Go primitive is `pkg/merchant`:

- `merchant.ID`
- `merchant.WithID(ctx, id)`
- `merchant.Require(ctx)`

There is no `FromContextOrDefault` path. Embedded hosts bind a merchant at
construction time or provide a principal mapper that returns an explicit
merchant id. Standalone HTTP requests resolve the merchant from API-key
resources, registered issuer/JWKS mapping, route parameters, or control-plane
admin authorization.

## RLS

RLS policies compare row `merchant_id` against:

```sql
current_setting('app.merchant_id', true)
```

OpenRails helpers set that GUC through:

- `DB.WithMerchantConn`
- `DB.RunInMerchantConn`
- `DB.MerchantTx`

Tests under `internal/db/*rls*_integration_test.go` prove fail-closed behavior,
GUC reset on connection release, and transaction scoping.

## Auth Chain

Merchant control is not direct user ownership. The chain is:

```text
AuthKit credential -> AuthKit org permission -> OpenRails merchant owner_org_id
```

API keys and remote applications are credentials controlled by the org.
They are not merchant owners.

## Migration Note

Older documentation and historical issue text used `tenant` for the OpenRails
merchant namespace. Current code, APIs, schema, and docs should use `merchant`
unless referring to an intentional legacy JWT claim rejection, the persisted
`tenant_subject`/`tenant_id` schema names, or a documented storage-path remnant.
