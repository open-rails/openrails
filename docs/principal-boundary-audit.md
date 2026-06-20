# Principal Boundary Audit

This audit covers the split between delegated browser/admin JWTs and
server-to-server machine credentials.

## Service credential surface

`/v1/service/*` is always mounted (the OpenRails AuthKit control plane is
mandatory in standalone mode, #469). Every route is behind the API-key machine
credential resolver. That middleware accepts either:

- a generated OpenRails/AuthKit opaque API key; or
- a first-party OIDC service JWT from a registered issuer.

For service JWTs, the caller is authorized by its issuer's merchant registration:
OpenRails resolves the issuer to its merchant, treats the token's self-assigned
`permissions` claim as authoritative, and scopes every resource to that merchant (a
token can never reach another merchant's resources). Route-level permission gates
then require the relevant service permission:

- Entitlement reads: `openrails:entitlements:read`.
- Credit and account/balance reads: `openrails:credits:read`.
- Credit writes and hold/capture/release flows: `openrails:credits:write`, with
  spend operations also requiring `openrails:credits:spend`.
- Merchant issuer registration and token-bootstrap management routes:
  `openrails:admin`.

Credit/account/balance handlers that act on a `customer_id` call
`RequireServiceTokenCustomerScope` before touching service logic, so merchant-wide
API keys may act across the merchant and customer-scoped API keys are denied for
other customers.

## Delegated browser/admin surface

`/v1/me/*` and `/v1/admin/*` are mounted only with
`DelegatedSelfRequired`. That middleware resolves an OIDC delegated JWT, pins the
merchant from the verified issuer mapping, and binds the acting
`delegated_sub` as request user context. Generated API keys and
`token_use=service` JWTs fail this resolver and are rejected before any
delegated route permission gate runs.

Self routes require `openrails:self:*` permissions. Delegated admin routes require
`openrails:merchant:*` permissions. API-key permissions do not satisfy
delegated route gates.

## Bootstrap and admin surfaces

The declarative merchant manifest/bootstrap path is a deploy action, not a browser
or API-key route. Standalone merchant lifecycle admin routes remain behind
the configured user auth provider plus the live Principal permission gate; they are not
part of the API-key server-to-server surface.

**Admin authority is deployment authority (#312).** A caller is an OpenRails
admin iff they hold the LIVE `openrails:admin` permission in their owning AuthKit
org — evaluated at request time by the control plane — or present a
deployment-minted admin API key carrying `openrails:admin`. There is NO
separate "operator"/"admin"/"platform" AuthKit org acting as the admin
authority, no JWT role-claim gate, and no global-admin DB fallback. The bootstrap
org hosts its own admin role. Initial generated admin API keys are
minted only through explicit operator/admin token-minting commands, not through
declarative bootstrap YAML. The control plane is always present in standalone
mode (#469); `/v1/admin/*` (and `/v1/admin/merchants/*`) fail closed for embedded
hosts that wire no control plane, because there is then no live authority to
evaluate.

Canonical identity vocabulary lives in
`docs/authkit-merchant-oidc-glossary.md`. New docs and route examples should use
OpenRails merchant, registered issuer, delegated user, customer, invoker, and
API key exactly as defined there.

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
