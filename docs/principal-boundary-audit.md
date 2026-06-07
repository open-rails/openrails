# Principal Boundary Audit

This audit covers the split between delegated browser/admin JWTs and
server-to-server machine credentials.

## Service credential surface

`/v1/service/*` is mounted only when the OpenRails AuthKit control plane exists.
Every route is behind `ServiceTokenRequired`. That middleware accepts either:

- a generated OpenRails/AuthKit opaque service token; or
- a first-party OIDC service JWT from a registered tenant issuer.

For service JWTs, the caller's `permissions` claim is only a request. OpenRails
resolves `(tenant, issuer, subject)` to a server-side
`billing.service_jwt_grants` row and intersects requested permissions/resources
with that grant. Route-level permission gates then require the relevant
effective service permission:

- Entitlement reads: `openrails:entitlements:read`.
- Credit and account/balance reads: `openrails:credits:read`.
- Credit writes and hold/capture/release flows: `openrails:credits:write`, with
  spend operations also requiring `openrails:credits:spend`.
- Tenant issuer registration and token-bootstrap management routes:
  `openrails:admin`.

Credit/account/balance handlers that act on a `tenant_subject_id` call
`requireServiceTenantSubjectScope` before touching service logic, so
tenant-wide tokens may act across the tenant and tenant-subject-scoped tokens
are denied for other tenant subjects.

## Delegated browser/admin surface

`/v1/self/*` and `/v1/tenant-admin/*` are mounted only with
`DelegatedSelfRequired`. That middleware resolves an OIDC delegated JWT, pins the
tenant from the verified issuer/claim mapping, and binds the acting
`delegated_sub` as request user context. Generated service tokens and
`token_use=service` JWTs fail this resolver and are rejected before any
delegated route permission gate runs.

Self routes require `openrails:self:*` permissions. Tenant-admin routes require
`openrails:tenant:*` permissions. Service-token permissions do not satisfy
delegated route gates.

## Bootstrap and admin surfaces

The declarative tenant manifest/bootstrap path is a deploy action, not a browser
or service-token route. Standalone tenant lifecycle admin routes remain behind
the configured user auth provider plus the admin policy middleware; they are not
part of the service-token server-to-server surface.

**Admin authority is deployment authority (#312).** A caller is an OpenRails
admin iff they hold the LIVE `openrails:admin` permission in their OWN AuthKit
tenant — evaluated at request time by the control plane — or present a
deployment-minted admin service token carrying `openrails:admin`. There is NO
separate "operator"/"admin"/"platform" AuthKit *tenant* acting as the admin
authority, no JWT role-claim gate, and no global-admin DB fallback. The default
tenant hosts its own admin role; bootstrap (`openrails billing bootstrap-tenants`
/ the manifest path) seeds that role and an initial admin service token under the
default tenant's own org. `/v1/admin/*` (and `/v1/admin/tenants/*`) fail closed
when no control plane is wired (verifier-only mode), because there is then no
live authority to evaluate.

Canonical identity vocabulary lives in
`docs/authkit-tenant-oidc-glossary.md`. New docs and route examples should use
OpenRails tenant, tenant issuer, delegated user, tenant subject, invoker, and
service token exactly as defined there.

## Validation

The following tests cover the boundary:

- `TestServiceEntitlementRouteRequiresEntitlementReadPermission`
- `TestServiceCreditBalanceRouteRejectsWrongTenantSubjectScope`
- `TestCreditServiceRoutesRejectDelegatedJWTs`
- `TestSelfService_RejectsServiceTokenCredential`
- `TestDelegatedSelfRequired_DeniesServiceTokenCredential`
- `TestServiceTokenRequired_DeniesNonServiceTokenCredential`
- `TestServiceTokenRequired_SucceedsForServiceJWT`
- `TestFederatedDelegatedTokens`
