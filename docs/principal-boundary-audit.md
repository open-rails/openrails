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

- Entitlement reads: `org:entitlements:read`.
- Credit and account/balance reads: `org:credits:read`.
- Credit writes and hold/capture/release flows: `org:credits:update`, with
  spend operations also requiring `org:credits:spend`.
- Merchant issuer registration and token-bootstrap management routes use
  merchant-local `org:` authority.

Credit/account/balance handlers that act on a `customer_id` call
`RequireServiceCredentialCustomerScope` before touching service logic, so merchant-wide
API keys may act across the merchant and customer-scoped API keys are denied for
other customers.

## Delegated browser/admin surface

`/v1/me/*` and `/v1/admin/*` are mounted only with
`DelegatedSelfRequired`. That middleware resolves an OIDC delegated JWT, pins the
merchant from the verified issuer mapping, and binds the acting
`delegated_sub` as request user context. Generated API keys and
`token_use=service` JWTs fail this resolver and are rejected before any
delegated route permission gate runs.

Self routes require a delegated/customer principal and always target the
authenticated subject. Delegated admin routes require browser-safe `org:*`
permissions. API-key credentials do not satisfy delegated route gates.

## Bootstrap and admin surfaces

The declarative merchant manifest/bootstrap path is a deploy action, not a browser
or API-key route. Standalone merchant lifecycle admin routes remain behind
the configured user auth provider plus the live Principal permission gate; they are not
part of the API-key server-to-server surface.

**Admin authority is deployment authority (#537).** Merchant-local authority is
live AuthKit `org:` permission state in the owning merchant org. Cross-merchant
directory authority is live AuthKit `platform:` state, not a special platform
org or JWT role claim. The bootstrap org hosts its own operator role. Initial
generated admin API keys are
minted only through explicit operator/admin token-minting commands, not through
declarative bootstrap YAML. The control plane is always present in standalone
mode (#469); `/v1/admin/*` fails closed for embedded hosts that wire no control
plane, because there is then no live authority to evaluate. Core does not expose
platform/cross-merchant admin routes.

Canonical identity vocabulary lives in
`docs/authkit-merchant-oidc-glossary.md`. New docs and route examples should use
OpenRails merchant, registered issuer, delegated user, customer, invoker, and
API key exactly as defined there.

## Validation

The following tests cover the boundary:

- `TestServiceEntitlementRouteRequiresEntitlementReadPermission`
- `TestServiceCreditBalanceRouteRejectsWrongMerchantSubjectScope`
- `TestCreditServiceRoutesRejectDelegatedJWTs`
- `TestSelfService_RejectsServiceCredential`
- `TestDelegatedSelfRequired_DeniesServiceCredential`
- `TestServiceCredentialRequired_DeniesNonServiceCredential`
- `TestServiceCredentialRequired_SucceedsForServiceJWT`
- `TestFederatedDelegatedTokens`
