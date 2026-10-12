# Authentication and request authority

OpenRails keeps billing identity merchant-scoped. A customer belongs to one
merchant; a future SaaS consumer account may explicitly link separate merchant
customer records. Shared wallets and cross-merchant account linking belong to
OpenRails-SaaS, not the engine.

## Who is calling: one contract

OpenRails authenticates nobody itself, and the host writes no middleware for
it. Every gated route asks an `openrails.Authenticator`, given at `Mount` as
`Routes.Auth`, who the request is (`Authenticate`, once per request), and
builds its gate from the answer: its `Identity`, its `Can(scope, permission)`
for a staff or programmatic route's permission. Admin operations require no
recent sign-in; sensitive customer-network operations retain that check
(authkit/auth `RecentSignInChecker`). The host names no
permission and no scope: the names are OpenRails' own ([below](#permissions)),
and the scopes come from the Authenticator's `RootScope` (authkit/auth `Root`).
AuthKit provides one as `ak.Authenticator()`; a host without AuthKit writes
its own ([below](#your-own-auth)). The standalone server mounts its routes
with its own AuthKit's, exactly like an embedded host, and the in-process Go
client brings its own host authority: its subject is the host application, and
`openrails.WithInvoker` names the signed-in person a call acts for as its
invoker ([destructive actions](destructive-actions.md)).

| Route tier | What OpenRails asks | Who it admits |
| --- | --- | --- |
| public, provider callbacks | nothing (a callback checks its provider's signature) | anyone |
| customer (`/v1/me`) | `Authenticate` | a user subject acting itself, the customer; an invoker acting for someone else, or an application, is refused |
| merchant (`/v1/admin`, `/v1/catalog`, `/v1/merchant`, `/v1/metrics`) | `Authenticate`, `Can` for the route's permission (its `merchant:` name in the merchant's scope, else its `root:` name in the root scope) | a person or an application holding that permission, without a recent-sign-in check |
| access (`GET /v1/admin/access`) | `Authenticate`, then `Can` for each mounted group's permission | any person or application; it answers what they hold |
| application (`/v1/app`) | `Authenticate`, then `Can` for the route's permission, as a merchant route does | an application (its `Identity.SubjectKind`) holding that permission, never a person |

Every gated route acts on the merchant the credential is bound to
([merchant binding](merchant-binding.md)), read once right after
`Authenticate`: the merchant whose scope its `BoundScope` is (the
group id being the merchant's id, in the `RootScope`'s authority), else the
merchant that declares the identity's issuer (`MerchantDeclaration.Issuer`),
else, for OpenRails' own credentials (a pay URL's secret, a provisioning
token, the in-process handle), the merchant of their row. Nothing in the
request names it. A credential bound to none, or to a scope that is not a live
merchant's, is `403 merchant_unresolved`.

OpenRails answers every refusal itself, the same for every host, with the
status and challenge `auth.Refuse` gives: 401 with `WWW-Authenticate`
(`authentication_required`, `credential_expired`, `credential_revoked`, with
the provider's own challenge headers passed through unread), 403
(`permission_required`, `application_required`, `invoker_scoped_principal`,
`step_up_unavailable` on sensitive customer-network routes for a credential
without a sign-in of its own),
503 when the host's auth cannot answer (`authentication_unavailable`,
`authorization_unavailable`, or a panic). On sensitive customer-network
routes a stale sign-in is RFC 9470's step-up:
401 `step_up_required` with `WWW-Authenticate: Bearer
error="insufficient_user_authentication", max_age="900"` and the provider's
challenge as the error's metadata. A Verified without `Can` holds nothing; an
identity without a subject or invoker is refused, so an Authenticator that
checks nothing admits no one; each handler checks the verdict again before it
runs. A route group never mounts without `Auth`, behind an `Auth` that is not
a authkit/auth `Root`, or with a route neither of whose permissions the `Auth`
knows (authkit/auth `PermissionCatalog`); a name it does not know is never
asked. `openrailstest.CheckAuth` checks an Authenticator against this contract
in the host's CI, with the route groups' `root:` permissions.

## Permissions

A permission is `persona:resource:action`, with AuthKit's actions `read` and
`manage`. The names are OpenRails' own: the `billing.Permission` constants,
listed by `billing.Permissions()`. A host declares them in its auth and grants
them in its roles from the constants, so a renamed one fails its build.

Every permission has two names, the persona being where it is held. A route
acting on merchant M admits a caller holding either:

- the `merchant:` name in M's own scope: `{Authority: RootScope's, ID: M's
  id}`, with AuthKit the group whose id is the merchant's;
- the `root:` name in the Authenticator's `RootScope`, with AuthKit the root
  group. It reaches every merchant.

Grants are ordinary per-user roles, embedded and standalone alike: a person
holding a role in one merchant's group acts at that merchant, one holding a
root role at all of them.

| Routes | `merchant:` | `root:` | Covers |
|---|---|---|---|
| admin, reads | `billing.MerchantCustomerRead` | `billing.RootCustomerRead` | customer support's reads |
| admin, changes | `billing.MerchantCustomerManage` | `billing.RootCustomerManage` | refunds, cancellations, credits |
| catalog | `billing.MerchantCatalogManage` | `billing.RootCatalogManage` | the catalog and its edits |
| merchant config | `billing.MerchantConfigManage` | `billing.RootConfigManage` | PSPs and their refresh, settings |
| metrics | `billing.MerchantMetricsRead` | `billing.RootMetricsRead` | metrics and their assistants |
| `/v1/app` | `billing.MerchantEntitlementsRead` | `billing.RootEntitlementsRead` | `POST /v1/app/entitlements/check` |
| Catalog reads | `billing.MerchantCatalogRead` | `billing.RootCatalogRead` | Product, price, meter and price-migration reads |
| `/v1/app` | `billing.MerchantUsageManage` | `billing.RootUsageManage` | admissions, usage events and running charges |
| `/v1/app` | `billing.MerchantEventsRead` | `billing.RootEventsRead` | host events |
| `/v1/app` | `billing.MerchantOrdersManage` | `billing.RootOrdersManage` | merchant orders: `POST /v1/app/orders` (`CreateOrder`), `/preview`, `/{id}/cancel` |
| `/v1/app` | `billing.MerchantCustomersRead` | `billing.RootCustomersRead` | `GET /v1/app/customers`: a customer by openrails.dev account, where the [customer network](customer-network.md) runs |

A route group that is on mounts all its routes; what a caller may use is its
roles. Each `/v1/app` route has its own permission, so a program's role grants
only what its task needs. The names are `merchant:customer:read`,
`root:customer:read` and so on: each route's pair is in
[api/routes.md](api/routes.md).

With AuthKit, declare each name on its persona and build roles from them:

```go
rbac := authkit.NewRoles()
merchant := rbac.Persona(billing.MerchantGroupPersona)
for _, p := range billing.Permissions() {
	if p.Persona() == billing.MerchantGroupPersona {
		merchant.Permission(p.Resource(), p.Action()) // held in a merchant's group
	} else {
		rbac.Root.Permission(p.Resource(), p.Action()) // held on root: every merchant
	}
}
```

A host that grants only root roles declares the `root:` names alone; `Mount`
needs one known name per route.

### Roles

Permissions are held through roles. On the standalone server a merchant's team
members and API keys hold a declared role (`server.MerchantRoles`: `viewer`,
`support`, `owner`) or one the merchant defines: a custom role,
`merchant:custom-<name>`, holding any of the `merchant:` permissions above.
There are no preset program roles; a merchant composes what each program
needs. A storefront's key holding `billing.MerchantCatalogRead` and
`billing.MerchantEntitlementsRead` reads the catalog and checks entitlements;
it cannot edit the catalog or customers' billing. The server declares the `root:` names on root too;
its root owner holds them all.

- Custom roles are AuthKit's (the merchant persona's `CustomRoles`), in the
  merchant's group. Defining, changing and deleting one takes
  `server.MerchantRolesManage`, reading what roles grant
  `server.MerchantRolesRead`; the owner holds both. A role grants only
  permissions its editor holds.
- Checks read a role live: an edit applies to every holder on the next
  request. Deleting one takes it from every teammate and revokes the API keys
  and invitations carrying it.
- The server's AuthKit API serves them for the merchant's group, whose id is
  the merchant's: `GET` and `POST /groups/{group_id}/roles`, `GET`, `PATCH`
  and `DELETE /groups/{group_id}/roles/{role}`. Its team
  (`/groups/{group_id}/members`) and API key (`/groups/{group_id}/api-keys`)
  routes take a custom role like a declared one. In Go: `ak.ListGroupRoles`,
  `ak.CreateGroupRole`, `ak.UpdateGroupRole`, `ak.DeleteGroupRole`; a hosted
  product resolves a role named on its own wire with
  `server.MerchantRole("custom-storefront")`.
- The admin console manages them in Settings, Roles: shown with
  `server.MerchantRolesRead`, editable with `server.MerchantRolesManage`.

Embedded, the host's AuthKit and roles are its own and OpenRails changes
nothing. A host lets its staff define roles by opting in the persona they hold
their permissions in: `authkit.NewRoles(authkit.CustomRoles)` for the root
group, `rbac.Persona(billing.MerchantGroupPersona, authkit.CustomRoles)` for a
merchant's own. The console's Roles page, where its `auth_base_url` serves
AuthKit's group routes, then shows the root group's roles and their holders as
all-merchants staff, and the merchant's group's, each to those holding its
persona's `roles:read` there. A host that grants only root roles has no group
for its merchant, and the page shows the root group alone.

## Your own auth

A host without AuthKit implements the same contract over its own users and
roles: [examples/own-auth](../examples/own-auth) is one, with its tests.
`authkit/auth` defines the contract and uses only the standard library. It is
versioned with AuthKit but imports no AuthKit runtime, so a host implements it
without running AuthKit:

- `Authenticate(r)` reads the request's credential, live: `ErrUnauthenticated`
  for none or an unknown one (joined with `ErrRevoked` once signed out),
  `ErrUnavailable` when your store cannot answer. OpenRails passes it no
  cookies: a browser sends the credential in `Authorization`.
- `Identity()` names your issuer, the user's UUID as the subject (it is the
  customer's id), the user as its own invoker, and the credential.
- `RootScope` is a `Scope{Authority, ID}` naming your root group, where staff
  of every merchant hold the `root:` names. A merchant's scope is
  `{Authority: the same, ID: the merchant's id}`: your own name for that
  merchant's staff group. OpenRails builds it from the merchant's id and keeps
  no list of scopes.
- `BoundScope` is the merchant the credential acts at, from your own record
  (the merchant a session chose), never a request field; zero for none.
- `Can(ctx, scope, permission)` answers exactly that `billing.Permission` in
  exactly that scope, read live: a merchant role's `merchant:` names in its
  merchant's scope, a root role's `root:` names in the root scope. A
  credential bound to one merchant holds nothing at another; a person keeps
  their root roles.
- Customer-network operations marked `sensitive` ask the recent-sign-in
  check (authkit/auth `RecentSignInChecker`): nil within your step-up window,
  else an `auth.Challenge` with `ErrStepUpRequired` and the window as
  `MaxAge`. Admin operations never ask it.
- `KnownPermission` knows `billing.Permissions()`, so `Mount` checks every
  route's names.

A host serving one merchant declares its issuer on that merchant
(`MerchantDeclaration.Issuer`) and returns a zero `BoundScope`.
`openrailstest.CheckAuth` checks the result in CI. A host's directory is
`Deps.UserInfo` (`Get` and `Search` over its users).

## Identities

An identity has three parts:

- the **subject**: the native account acted as, a user or an application.
  Its money and authority are used: a customer route's customer, a staff
  permission's holder. A user is the same subject on every credential.
- the **invoker**: the party actually acting, the subject itself or someone
  acting on its behalf, possibly another issuer's user. Spend limits, spend
  delegations, staff rate limits and the destructive-operation ceiling key on
  it: its id, as `issuer|id` when another issuer vouches for it.
- the **credential**: how it was proven (session, device key, API key, signed
  token or access token). Only a user acting in person pays with their own
  saved card. A recent-sign-in check is limited to the customer-network
  operations marked `sensitive`.

Provider writes record all three: the subject, the invoker and the
credential (`kind:id`).

The standalone server (package `server`) runs its own AuthKit, configured by
its `auth:` section in AuthKit's own keys, with closed registration unless it
says otherwise. Its own sign-in (password, passwordless, registration) is
opt-in (`server.Config.LocalSignIn`, `local_sign_in`); without it people sign in
at a trusted issuer. Identity and contact attributes confer no authorization.

## The standalone server

The server's AuthKit decides who every request is. OpenRails maps merchants to
AuthKit groups and nothing more:

- **A merchant's scope is its AuthKit group**, whose id is the merchant's id
  (`Server.MerchantScope`). Staff routes ask `Can` for the `merchant:` name
  there, else for the `root:` name in the root group.
- **The merchant a request is for** is the AuthKit group its credential is
  bound to: an API key's group, a trusted issuer's group, a token exchanged for
  a group. A person's own sign-in is bound to none and reaches no merchant:
  the admin console exchanges it for a token bound to the merchant chosen
  (RFC 8693 with `group_id`, through the `openrails-console` client the server
  declares), and holding a role in one merchant never picks it.
- **Customer (`/v1/me`) and programmatic (`/v1/app`) credentials are bound** to
  their merchant: a trusted issuer's token, a customer token minted for a
  merchant's backend, or an API key of the merchant's group.
- **A customer is its issuer's subject** (OIDC Core §5.7): the customer id is
  the token's `sub`, and `billing.customers` records the issuer that made it.
  Another issuer's credential with the same `sub` is refused
  (`403 customer_issuer_mismatch`). A customer with no recorded issuer is the
  merchant's own: one made through the Go API or the admin API, one its own
  backend names in a customer token (RFC 7523), a user of the issuer it
  declares. No foreign trusted issuer reaches it, so let a foreign issuer's
  customer make itself with its first request.

## Trusted issuers

With `auth.resource.id` set, the server's AuthKit is an OAuth 2.0 resource
server: it accepts RFC 9068 access tokens (`typ: at+jwt`) minted for that
resource by its trusted issuers. A trusted issuer is an AuthKit remote
application in a merchant's group: the merchant manifest's
`remote_application` (holding the merchant's owner role), or one registered at
run time through AuthKit. One issuer acts for one merchant. Its tokens'
permissions count within its application's role and the token's scopes:
`openrails:merchant` grants the merchant permissions, `openrails:self` none (a
customer's own billing). A token whose `sub` equals its `client_id` is a client
acting for itself.

A trusted application without an authorization server mints its customer a
token at the server's own token endpoint with an RFC 7523 assertion. How
AuthKit verifies tokens, maps an issuer's roles and handles sender-constrained
tokens is AuthKit's: see its
[resource server](https://github.com/open-rails/authkit/blob/master/docs/resource-server.md)
and [DPoP](https://github.com/open-rails/authkit/blob/master/docs/resource-server.md#dpop)
documentation. `auth.sign_in.dpop` sets how the server's AuthKit issues tokens
to its own users; it never changes how tokens are validated.

Every API route answers CORS for any origin, without credentials mode; the
header lists add the ones the mount's Authenticator advertises. Origin is
transport metadata, not identity.

OpenRails cannot see an issuer's end-user ban or session revocation: the
issuer stops minting, and the access token's lifetime bounds the exposure.

## Cookies and local account admission

Billing HTTP mounts strip ambient cookies: a browser call carries its
credential in a header. AuthKit's own `/auth` transport keeps its separate
session/refresh/CSRF cookie protocol.

The control plane enables AuthKit's browser refresh transport: access tokens
stay in memory, while the rotating refresh credential is an HttpOnly,
SameSite=Lax cookie (`__Host-authkit_rt` on HTTPS; `authkit_rt` on explicitly
allowed plain-HTTP development deployments). Auth responses contain no refresh
token for JavaScript storage. A page reload restores the session with
`POST /auth/v1/token` and the cookie; body refresh tokens, duplicate cookies and
cross-origin consumption are refused. Logout clears the cookie and revokes its
server-side session. Go AuthKit calls retain their own explicit token results.

Privileged local users pass AuthKit request verification and live account
admission. Enrollment-only tokens remain restricted after enrollment completes;
clients obtain a new normal token. Every permission check acts as the token's
own sign-in (`verify.ActorFromClaims`): once that session is revoked (logout,
revoke-all, password change, ban, deletion) the check answers 401
`credential_revoked`, whatever time the token has left. Routes that only
authenticate, with no permission check, accept the token until it expires.

Every HTTP attempt authenticates and authorizes again: the route gate asks
the Authenticator once per request. Documented financial operations own durable
idempotency receipts; there is no generic response replay cache. Permission
checks are not memoized.

## Operational configuration

Configure `trusted_proxies` with actual proxy CIDRs or set
`auth.http.direct_peer_ip: true` for direct client connections. Cloudflare-specific
headers are trusted only from declared `cloudflare_proxies`; generic proxy
trust does not confer that authority. Embedded hosts set the same on
`Config.TrustedProxies` and `Config.CloudflareProxies`. Direct-peer and proxy
declarations are mutually exclusive.

Development signing keys persist under `auth.keys.path`; production supplies
its managed keys. AuthKit reads a signing application's registration on every
verification, so registration, key changes and disablement apply to the next
request, wherever they were made. Unverified token issuers never trigger
arbitrary database or JWKS discovery.

Cookie origins use canonical browser spelling: lowercase host, no wildcard,
userinfo, path, query, fragment, or explicit default port. HTTPS is required;
HTTP is allowed for explicit localhost/loopback development origins.

## Customer portal membership

Billing customer records are merchant-scoped and
do not create AuthKit customer groups as a side effect. Hosts such as
OpenRails-SaaS explicitly call `EnsureCustomerPermissionGroup` when a user
creates a portal account. The group's id is the customer's user id:
`CustomerGroup(customerID)` addresses it and `CustomerType` is its persona.

Customer groups do not issue API keys or register signing applications: those
credentials have no supported customer authentication path. Their
seven generated credential routes are absent. Merchant API keys and registered
merchant applications remain supported. Existing generated customer member,
role, invite and descriptor routes require an explicitly created group.
