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
for a staff or programmatic route's permission in `Routes.Scope`, and its
recent sign-in (helpers/auth `RecentSignInChecker`) for a person. AuthKit
provides one as `ak.Authenticator()`. The standalone server mounts its routes
with its own AuthKit's, exactly like an embedded host, and the in-process Go
client brings its own host authority.

| Route tier | What OpenRails asks | Who it admits |
| --- | --- | --- |
| public, provider callbacks | nothing (a callback checks its provider's signature) | anyone |
| checkout session | the session id; `Authenticate` only when a credential is presented, never to refuse | the session's own customer, a person in person, sees and pays with its saved cards |
| customer (`/v1/me`) | `Authenticate` | a user subject acting itself, the customer; an invoker acting for someone else, or an application, is refused |
| merchant (`/v1/admin`) | `Authenticate`, `Can` for the route's group permission (`Routes.Permissions`) in the merchant's scope, then the recent sign-in check when a person moves money, removes access or exports data | a person or an application holding that permission; an application has no sign-in to renew |
| access (`GET /v1/admin/access`) | `Authenticate`, then `Can` for each mounted group's permission | any person or application; it answers what they hold |
| application (`/v1/app`) | `Authenticate`, then `Can` for the route's permission in the merchant's scope | an application (its `Identity.SubjectKind`) holding that permission, never a person |

OpenRails binds the merchant a customer route serves before it asks
`Authenticate`; the host reads it with `openrails.RequestMerchant` rather than
resolving `OpenRails-Merchant` itself.

OpenRails answers every refusal itself, the same for every host, with the
status and challenge `auth.Refuse` gives: 401 with `WWW-Authenticate`
(`authentication_required`, `credential_expired`, `credential_revoked`, with
the provider's own challenge headers passed through unread), 403
(`permission_required`, `application_required`, `invoker_scoped_principal`,
`step_up_unavailable` for a person whose credential has no sign-in of its own),
503 when the host's auth cannot answer (`authentication_unavailable`,
`authorization_unavailable`, or a panic). A stale sign-in is RFC 9470's step-up:
401 `step_up_required` with `WWW-Authenticate: Bearer
error="insufficient_user_authentication", max_age="900"` and the provider's
challenge as the error's metadata. A Verified without `Can` holds nothing; an
identity without a subject or invoker is refused, so an Authenticator that
checks nothing admits no one; each handler checks the verdict again before it
runs. A mount whose groups need `Auth`, or a permission without `Scope`,
fails. `openrailstest.CheckAuth` checks an Authenticator against this contract
in the host's CI.

## Permissions

A permission is `persona:resource:action`, with AuthKit's actions `read` and
`manage`. The resource and action are fixed; the persona is where the
permission is held. Embedded, the host's staff and programs hold them in the
site's root group (`Scope: ak.Scope(ctx, iam.RootGroup())`), so they are
`root:…`, and the host passes them in `Routes.Permissions`: OpenRails names
none itself. On the standalone server they are held in each merchant's group,
so they are `merchant:…`, the server's constants below.

| `Routes.Permissions` | Embedded | Standalone server | Covers |
|---|---|---|---|
| `AdminRead` | `root:billing:read` | `server.MerchantBillingRead` | customer support's reads |
| `AdminUpdate` | `root:billing:manage` | `server.MerchantBillingManage` | refunds, cancellations, credits |
| `Catalog` | `root:catalog:manage` | `server.MerchantCatalogManage` | catalog edits |
| `MerchantConfig` | `root:config:manage` | `server.MerchantConfigManage` | PSPs, settings |
| `Metrics` | `root:metrics:read` | `server.MerchantMetricsRead` | metrics and their assistants |
| `Entitlements` | `root:entitlements:read` | `server.MerchantEntitlementsRead` | `POST /v1/app/entitlements/check` |
| `Offers` | `root:catalog:read` | `server.MerchantCatalogRead` | `GET /v1/app/catalog/products` (`ListOffers`) |
| `Usage` | `root:usage:manage` | `server.MerchantUsageManage` | admissions and usage events |
| `Costs` | `root:costs:manage` | `server.MerchantCostsManage` | provider operations |
| `Events` | `root:events:read` | `server.MerchantEventsRead` | host events |

The last five are the programmatic routes' (`RouteGroups.Programmatic`): each
`/v1/app` route mounts only with its permission, so a program gets only what
its task needs. `Mount` refuses a permission for a group that is off.

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
  saved card, and only one is asked for a recent sign-in.

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
  (`Server.MerchantScope`). Staff routes ask `Can(scope, permission)` there.
- **The merchant a request is for** comes from its API host, its
  `OpenRails-Merchant` selector, the scope its credential is bound to (an API
  key's group, a trusted issuer's group) and the configured merchant. They must
  agree, else `409 merchant_binding_mismatch`; with none it is
  `403 merchant_unresolved`. A person names the merchant: holding a role in
  one merchant never picks it.
- **Customer (`/v1/me`) and programmatic (`/v1/app`) credentials must be bound**
  to the request's merchant: a trusted issuer's token, or an API key of the
  merchant's group. A credential bound to no merchant is refused there.
- **A customer is its issuer's subject** (OIDC Core §5.7): the customer id is
  the token's `sub`, and `billing.customers` records the issuer that made it.
  Another issuer's credential with the same `sub` is refused
  (`409 merchant_binding_mismatch`). A customer with no recorded issuer is the
  host's own user, such as one made through the Go API; no trusted issuer
  reaches it.

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
