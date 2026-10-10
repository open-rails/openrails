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
`CheckRecentSignIn` for a person's recent sign-in. AuthKit provides one as
`ak.Authenticator()`. The standalone server supplies its trusted issuers, API
keys and control-plane sessions through the same contract, and the in-process
Go client its own host authority.

| Route tier | What OpenRails asks | Who it admits |
| --- | --- | --- |
| public, provider callbacks | nothing (a callback checks its provider's signature) | anyone |
| checkout session | the session id; `Authenticate` only when a credential is presented, never to refuse | the session's own customer, a person in person, sees and pays with its saved cards |
| customer (`/v1/me`) | `Authenticate` | a user subject acting itself, the customer; an invoker acting for someone else, or an application, is refused |
| merchant (`/v1/admin`) | `Authenticate`, `Can` for the route's group permission (`Routes.Permissions`) in the merchant's scope, then `CheckRecentSignIn` when a person moves money, removes access or exports data | a person or an application holding that permission; an application has no sign-in to renew |
| access (`GET /v1/admin/access`) | `Authenticate`, then `Can` for each mounted group's permission | any person or application; it answers what they hold |
| application (`/v1/app`) | `Authenticate`, then `Can` for the route's permission in the merchant's scope | an application (its `Identity.SubjectKind`) holding that permission, never a person |

OpenRails binds the merchant a customer route serves before it asks
`Authenticate`; the host reads it with `openrails.RequestMerchant` rather than
resolving `OpenRails-Merchant` itself.

OpenRails answers every refusal itself, the same for every host, with the
status and challenge `auth.Refuse` gives: 401 with `WWW-Authenticate`
(`authentication_required`, `credential_expired`, `credential_revoked`,
`sender_proof_required`, and the provider's own challenge headers), 403
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
| `Usage` | `root:usage:manage` | `server.MerchantUsageManage` | admissions and usage events |
| `Costs` | `root:costs:manage` | `server.MerchantCostsManage` | provider operations |
| `Events` | `root:events:read` | `server.MerchantEventsRead` | host events |

The last four are the programmatic routes' (`RouteGroups.Programmatic`): each
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

The standalone server (package `server`) runs its own AuthKit with closed
registration. Its own sign-in (password, passwordless, registration) is opt-in
(`server.Config.LocalSignIn`, `local_sign_in`); without it people sign in at a
trusted issuer. OpenRails-SaaS opens hosted registration. A
merchant signing application maps to exactly one merchant permission group;
its token cannot select another merchant by adding a claim or changing a URL.
Identity/contact attributes do not confer authorization.

## Trusted issuers

Standalone OpenRails is an OAuth 2.0 resource server. It accepts RFC 9068
access tokens (`typ: at+jwt`) whose `aud` is `resource_server.identifier`, from
two kinds of trusted issuer:

- declared in `resource_server.trusted_issuers`: issuer, keys, the merchants it
  acts for, a permission ceiling, CORS origins and optional group roles;
- a merchant's registered signing application (the manifest's
  `remote_application`): it acts for that merchant only, within its stored
  authority, read on every request, so disabling it applies to the next one.

The token's `scope` selects the surface: `openrails:merchant` for the merchant
API and a user's own merchant list, `openrails:self` for a customer's own
billing (`/v1/me`). A token never names its merchant; the request does
(`OpenRails-Merchant` or the merchant's API host), and it must be one the
issuer is trusted for. Permissions are the token's `permissions`, the issuer's
group roles and the user's accepted federated grants, within the ceiling: the
server's merchant permissions ([Permissions](#permissions)). A
token whose `sub` equals its `client_id` is a client acting for itself.

A token bound to a key (`cnf.jkt`) is accepted only as `Authorization: DPoP`
with a fresh single-use proof for the method, the configured
`auth.request_origin` plus path, and the server nonce: the first proof without
one is answered `401 use_dpop_nonce` with a `DPoP-Nonce` header. Customer
tokens must be bound (DPoP, or a certificate-bound token over mTLS). Spent
proofs are recorded in PostgreSQL, shared by every replica with or without
Redis; a proof that cannot be recorded fails closed with 503.
Arbitrary Host/Forwarded headers never define a proof target.

Merchant routes answer CORS for the trusted issuers' declared origins only,
without credentials mode; browser self-service answers any origin. Origin is
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

Every HTTP attempt authenticates and authorizes again. Documented financial
operations own durable idempotency receipts; there is no generic response
replay cache. Inside one immutable request, repeated permission checks reuse
verified identity and sender proof. Permission checks are not memoized.

## Operational configuration

Configure `trusted_proxies` with actual proxy CIDRs or set
`auth.direct_peer_ip: true` for direct client connections. Cloudflare-specific
headers are trusted only from declared `cloudflare_proxies`; generic proxy
trust does not confer that authority. Embedded hosts set the same on
`Config.TrustedProxies` and `Config.CloudflareProxies`. Direct-peer and proxy
declarations are mutually exclusive.

Development signing keys persist under `auth.keys_path`; production supplies
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
