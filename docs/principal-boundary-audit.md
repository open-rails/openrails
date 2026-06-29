# Principal Boundary Audit

This audit covers the merchant-route auth boundary after the #552/#564 hard cut.
Merchant-owned HTTP operations live under `/v1/merchant/*`; `/v1/service/*` is
not a compatibility surface.

## Merchant Route Model

Every `/v1/merchant/*` request is authorized by the same route gate:

1. Validate the presented credential.
2. Resolve the OpenRails merchant mapped to the credential's merchant
   permission-group.
3. Resolve the credential's live or stored permission set.
4. Compare that set to the route's required `merchant:*` permission.
5. Pin the merchant context before merchant-owned DB access.

Credential type does not decide access. The required permission decides access.

Supported credential classes:

- API keys and remote-application service JWTs resolve through the control plane
  as service credentials. Stored AuthKit authority and resource scopes are live;
  customer-scoped API keys keep their `openrails.customer` resource restriction.
- Delegated/self-service JWTs are signed by a registered remote application.
  AuthKit verifies issuer/audience/signature and bounds requested permissions to
  the signing remote application's stored authority. Over-claims reject the token
  (`permission_not_granted`); OpenRails does not apply a browser-safe allowlist.
- Logged-in user access tokens authenticate through the configured Authenticator.
  OpenRails checks the user's live permission in the merchant permission-group
  and then resolves that merchant for DB scoping.

## Handler Boundary

Route middleware is the merchant permission boundary. Former service handlers no
longer require a service credential after the route gate has authorized a merchant
principal.

Service credentials still get stricter customer-resource checks inside handlers:
if a service credential carries an `openrails.customer` resource, it may only act
for that customer. Delegated and user-session merchant principals are already
scoped by merchant permission and merchant context, so they are not forced through
service-credential resource checks.

## Platform Boundary

Core does not expose platform/cross-merchant admin routes. Future platform
operator surfaces belong to OpenRails SaaS and must use `platform:*` authority.
Merchant routes use OpenRails-defined `merchant:*` permissions in the merchant
permission-group. A merchant role/token cannot grant `platform:*`, and a
platform role cannot satisfy a merchant route without explicit merchant-group
authority.

## Validation

Focused coverage for this boundary:

- `TestStandaloneMerchantAdmitAcceptsDelegatedJWTByPermissionHTTP`
- `TestStandaloneMerchantAdmitAcceptsUserSessionByPermissionHTTP`
- `TestRemoteApplicationSelfJWTCrossMerchantIsolationHTTP`
- `TestAPIKeyCrossMerchantIsolationHTTP`
- `TestDelegatedAdminCrossMerchantIsolationHTTP`
- `TestDelegatedSelfTokenSubjectIsolationHTTP`
- `TestCoreDoesNotMountPlatformAdminRoutesHTTP`
- `TestServiceAdmit_HTTP_EndToEnd`
