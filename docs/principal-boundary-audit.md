# Principal Boundary Audit

This audit covers the issue 315 split between delegated browser/admin JWTs and
opaque server-to-server service tokens.

## Service-token surface

`/v1/service/*` is mounted only when the OpenRails AuthKit control plane exists.
Every route is behind `ServiceTokenRequired`, which rejects non-service-token
bearer credentials before any handler runs. Route-level permission gates then
require the relevant service permission:

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
`delegated_sub` as request user context. Service tokens fail this resolver and
are rejected before any delegated route permission gate runs.

Self routes require `openrails:self:*` permissions. Tenant-admin routes require
`openrails:tenant:*` permissions. Service-token permissions do not satisfy
delegated route gates.

## Bootstrap and admin surfaces

The declarative tenant manifest/bootstrap path is a deploy action, not a browser
or service-token route. Standalone tenant lifecycle admin routes remain behind
the configured user auth provider plus operator/admin policy middleware; they
are not part of the service-token server-to-server surface.

## Validation

The following tests cover the boundary:

- `TestServiceEntitlementRouteRequiresEntitlementReadPermission`
- `TestServiceCreditBalanceRouteRejectsWrongTenantSubjectScope`
- `TestCreditServiceRoutesRejectDelegatedJWTs`
- `TestSelfService_RejectsServiceTokenCredential`
- `TestDelegatedSelfRequired_DeniesServiceTokenCredential`
- `TestServiceTokenRequired_DeniesNonServiceTokenCredential`
- `TestFederatedDelegatedTokens`
