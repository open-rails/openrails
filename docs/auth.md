# Authentication and request authority

OpenRails keeps billing identity merchant-scoped. A customer belongs to one
merchant; a future SaaS consumer account may explicitly link separate merchant
customer records. Shared wallets and cross-merchant account linking belong to
OpenRails-SaaS, not the engine.

## Deployment boundaries

| Surface | Embedded application | Standalone or SaaS server |
| --- | --- | --- |
| Go business client | In-process application authority | API key, or a trusted issuer's client-credentials access token |
| Browser self-service | `Deps.Authenticate` maps the normal user credential to its paying customer | A trusted issuer's DPoP-bound access token with scope `openrails:self` |
| User billing routes | `Deps.Authenticate` | Local AuthKit user credential, or a trusted issuer's access token |
| Merchant operations | `Deps.Authorize`, live per operation | Verified credential plus current merchant permission |
| Platform operations | Owned by the host | Local human operator plus current root permission |

Embedded applications supply `Deps.Authenticate`, `Deps.Authorize` and
`Deps.RecentSignIn` over their own request verifier, with explicit customer and
permission mappings; live admission is an explicit host policy.
Remote JWKS verification cannot independently observe a remote user's ban.

The standalone control plane is mandatory and uses closed registration. Its
own sign-in (password, passwordless, registration) is opt-in
(`ControlPlaneConfig.LocalSignIn`, standalone `local_sign_in`); without it
people sign in at a trusted issuer.
OpenRails-SaaS explicitly enables hosted registration when attaching it. A
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
group roles and the user's accepted federated grants, within the ceiling. A
token whose `sub` equals its `client_id` is a client acting for itself.

A token bound to a key (`cnf.jkt`) is accepted only as `Authorization: DPoP`
with a fresh single-use proof for the method, the configured
`auth.request_origin` plus path, and the server nonce: the first proof without
one is answered `401 use_dpop_nonce` with a `DPoP-Nonce` header. Customer
tokens must be bound (DPoP, or a certificate-bound token over mTLS). Proof
claims are shared across replicas through Redis and fail closed with 503.
Arbitrary Host/Forwarded headers never define a proof target.

Merchant routes answer CORS for the trusted issuers' declared origins only,
without credentials mode; browser self-service answers any origin. Origin is
transport metadata, not identity.

OpenRails cannot see an issuer's end-user ban or session revocation: the
issuer stops minting, and the access token's lifetime bounds the exposure.

## Cookies and local account admission

Billing HTTP mounts ignore ambient cookies by default. A cookie-based host
sets `Routes.CookieOrigin` to `"https://merchant.example"` when it mounts. Every unsafe
cookie request must carry that exact Origin, including bodyless POSTs. Missing,
opaque, cross-origin and sibling origins are refused. Explicit Authorization
never falls back to an attached cookie. AuthKit's own `/auth` transport keeps
its separate session/refresh/CSRF cookie protocol and must not be wrapped by
the billing cookie adapter.

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

Billing customer records and spend-delegation policies are merchant-scoped and
do not create AuthKit customer groups as a side effect. Hosts such as
OpenRails-SaaS explicitly call `EnsureCustomerPermissionGroup` when a user
creates a portal account. The group's id is the customer's user id:
`CustomerGroup(customerID)` addresses it and `CustomerType` is its persona.

Customer groups do not issue API keys or register signing applications: those
credentials have no supported customer authentication path. Their
seven generated credential routes are absent. Merchant API keys and registered
merchant applications remain supported. Existing generated customer member,
role, invite and descriptor routes require an explicitly created group.
