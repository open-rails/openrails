# Authentication and request authority

OpenRails keeps billing identity merchant-scoped. A customer belongs to one
merchant; a future SaaS consumer account may explicitly link separate merchant
customer records. Shared wallets and cross-merchant account linking belong to
OpenRails-SaaS, not the engine.

## Deployment boundaries

| Surface | Embedded application | Standalone or SaaS server |
| --- | --- | --- |
| Go business client | In-process application authority | API key or registered service JWT |
| Browser self-service | Host `DelegatedAuthenticator` maps the normal user credential to the bound merchant | Sender-bound delegated token from the merchant issuer |
| User billing routes | Host `Authenticator` | Local AuthKit user credential |
| Merchant operations | Host `Gate` | Verified credential plus current merchant permission |
| Platform operations | Owned by the host | Local human operator plus current root permission |

Embedded applications supply `billingauth.NewIntegration` with a provider-neutral
request verifier and explicit customer/permission mappings. AuthKit hosts pass
their existing verifier directly; live admission is an explicit host policy.
Remote JWKS verification cannot independently observe a remote user's ban.
The normal local host-user adapter remains distinct from the wire delegated
profile: a local user has `sub`, while a delegated caller has `delegated_sub`.

The standalone control plane is mandatory and uses closed registration.
OpenRails-SaaS explicitly enables hosted registration when attaching it. A
merchant signing application maps to exactly one merchant permission group;
its token cannot select another merchant by adding a claim or changing a URL.
Identity/contact attributes do not confer authorization.

## Browser and native delegation

The browser first authenticates to its merchant application. It then calls
that issuer's AuthKit `POST /delegated/token`, using its local access credential
and a non-extractable WebCrypto key. The host authorizer selects the grant.
OpenRails accepts the resulting `Authorization: DPoP <token>` only with a fresh
ES256 proof covering that token, HTTP method and external URL. Replay, wrong
key/target/token, missing proof and a Bearer downgrade are refused. Native
clients instead use a certificate-bound delegated Bearer token and its actual
TLS client certificate. Unbound wire delegation is unsupported.

See [frontend integration](frontend-integration.md#authentication) for the
mint/request contract. Receiver proof targets use the configured `auth.request_origin`;
`AttachOptions.DPoPRequestURL` supplies trusted external URL mapping for hosts
that rewrite paths. Arbitrary Host/Forwarded headers never define that target.
Redis-backed AuthKit proof claims are shared across receiver replicas and must
retain accepted claims for the full proof window. Storage errors fail closed
with 503; a rejected proof receives the DPoP authentication challenge.

Delegated CORS permits credential-free browser requests and preflight with
Authorization/DPoP headers. Origin is transport metadata, not identity or the
signing application's authority.

## Cookies and local account admission

Billing HTTP mounts ignore ambient cookies by default. A cookie-based host
must explicitly wrap its billing mount in
`billingauth.CookieAuthentication("https://merchant.example")`. Every unsafe
cookie request must carry that exact Origin, including bodyless POSTs. Missing,
opaque, cross-origin and sibling origins are refused. Explicit Authorization
never falls back to an attached cookie. AuthKit's own `/auth` transport keeps
its separate session/refresh/CSRF cookie protocol and must not be wrapped by
the billing cookie adapter.

Privileged local users pass AuthKit request verification and live account
admission. Enrollment-only tokens remain restricted after enrollment completes;
clients obtain a new normal token. Ban/deletion blocks an unexpired token even
when the user's role remains. Session revocation is separate: an already
issued access JWT can remain usable until its configured expiry. Account
liveness does not imply a per-session access-token deny-list.

Delegated verification consults the registered signing application's enabled
state/grant and active merchant binding. Issuer disablement, grant removal or
merchant retirement affects subsequent requests. OpenRails cannot see the
issuer's end-user ban/session revocation independently: the issuer stops
mint/refresh, and remaining delegated-token lifetime bounds that exposure.

Every HTTP attempt authenticates and authorizes again. Documented financial
operations own durable idempotency receipts; there is no generic response
replay cache. Inside one immutable request, repeated permission checks reuse
verified identity and sender proof. Permission checks are not memoized.

## Operational configuration

Configure `trusted_proxies` with actual proxy CIDRs or set
`auth.direct_peer_ip: true` for direct client connections. Cloudflare-specific
headers are trusted only from declared `cloudflare_proxies`; generic proxy
trust does not confer that authority. Embedded `AttachOptions` forward the
same settings. Direct-peer and proxy declarations are mutually exclusive.

Development signing keys persist under `auth.keys_path`; production supplies
its managed keys. After in-process application registration, call
`ControlPlane.ReloadRemoteApplications` for immediate discovery. Out-of-process
registration/key rotation converges through the bounded registry refresh.
Unverified token issuers never trigger arbitrary database or JWKS discovery.

Cookie origins use canonical browser spelling: lowercase host, no wildcard,
userinfo, path, query, fragment, or explicit default port. HTTPS is required;
HTTP is allowed for explicit localhost/loopback development origins.

## Customer portal membership

Billing customer records and spend-delegation policies are merchant-scoped and
do not create AuthKit customer groups as a side effect. Hosts such as
OpenRails-SaaS explicitly call `EnsureCustomerPermissionGroup` when a user
creates a portal account. `CustomerType`, `CustomerGroup`, and
`CustomerGroupSlug` remain available for that membership and discovery flow.

Customer groups do not issue API keys or register signing applications: those
credentials have no supported customer-treasury authentication path. Their
seven generated credential routes are absent. Merchant API keys and registered
merchant applications remain supported. Existing generated customer member,
role, invite and descriptor routes require an explicitly created group.
