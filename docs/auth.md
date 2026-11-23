# Authentication and authority

Billing identity is merchant-scoped. Each integration establishes who is
calling and which merchant the credential belongs to.

- [Embedded](embedded.md) uses the `Authenticator` passed as `Routes.Auth`.
  Use AuthKit's `ak.Authenticator()` or [implement the adapter](#your-own-auth)
  for your application's identity system.
- [Self-hosted standalone](standalone.md) uses the server's AuthKit and its
  configured trusted issuers.
- [OpenRails SaaS](saas.md) provides platform credentials for your merchant and
  customer billing flows.

Gated routes resolve identity once per request and check the route's authority.

| Route tier | OpenRails asks | Admits |
|---|---|---|
| public, provider callbacks | nothing (a callback checks its provider's signature) | anyone |
| customer `/v1/me` | `Authenticate` | a user acting itself: the customer |
| merchant `/v1/admin`, `/v1/catalog`, `/v1/merchant`, `/v1/metrics` | `Authenticate`, then `Can` for the route's permission | a person or application holding it, with no recent-sign-in check |
| access `GET /v1/admin/access` | `Authenticate`, then `Can` per mounted group | anyone; it answers what they hold |
| application `/v1/app` | `Authenticate`, then `Can` | an application (`Identity.SubjectKind`) holding the permission, never a person |

The in-process Go client brings its own authority: its subject is the host
application, and `openrails.WithInvoker` names the person a call acts for
([destructive actions](destructive-actions.md)).

OpenRails answers every refusal itself, as `auth.Refuse` gives it: 401 with
`WWW-Authenticate` (`authentication_required`, `credential_expired`,
`credential_revoked`; your challenge headers pass through), 403
(`permission_required`, `application_required`, `invoker_scoped_principal`,
`step_up_unavailable`), 503 when your auth cannot answer
(`authentication_unavailable`, `authorization_unavailable`). A stale sign-in on a
sensitive [host-mapped customer route](#host-customer-apis) is RFC 9470 step-up:
401 `step_up_required` with `WWW-Authenticate: Bearer
error="insufficient_user_authentication", max_age="900"`. An identity without a
subject or invoker is refused, so an Authenticator that checks nothing admits no
one. A route group never mounts without `Auth`, behind an `Auth` that is not an
`auth.Root`, or with a route neither of whose permissions the `Auth` knows
(`auth.PermissionCatalog`). `openrailstest.CheckAuth(t, routes, authtest.Cases{…})`
checks an Authenticator against this contract in CI.

## Merchant binding

A request acts on the merchant its credential is bound to. Nothing in the request
selects it (not the Host, a header, the path, the query or the body), and no
default fills in. Right after `Authenticate`, OpenRails takes the first that
applies:

| Binding | The merchant is | Credentials |
|---|---|---|
| Scope | the one whose scope is the credential's `BoundScope` (`{Authority: RootScope's, ID: the merchant's id}`); a scope of no live merchant is refused, never passed to the next rule | an API key of the merchant's group, a merchant application's or trusted issuer's token, a customer token minted for it, a console token exchanged for it, a host session bound to it |
| Issuer | the one whose `MerchantDeclaration.Issuer` is the credential's issuer (one merchant per issuer per engine) | a single-merchant host's own sign-ins |
| Object | the merchant of the row an OpenRails secret opens | a pay URL's secret, a webhook signature, a provisioning token, the in-process handle |

`403 merchant_unresolved` means a valid credential bound to no live merchant: a
person's own sign-in where no merchant declares its issuer, a scope that is no
merchant's (or a deleted one's), an unknown in-process handle
(`billing.ErrMerchantUnresolved`). Fix the credential; no parameter changes it.
Merchant-bound answers carry `OpenRails-Bound-Merchant`.

Getting a bound credential:

- **A backend** in process uses the handle (`bill.Merchant(id)`,
  `bill.MerchantBySlug`). Over HTTP, `openrails.NewRemoteMerchant` takes the
  merchant's API key or application token; `openrails.NewRemote` takes one
  credential per merchant (`RemoteOptions`) and hands out `Merchant(id)`. Handles
  send `OpenRails-Expected-Merchant` once they know their merchant (a mismatch is
  `412 merchant_mismatch` before the operation) and check the bound header
  (`openrails.BoundMerchantError`). `NewRemote` with a known id guards the first
  write too.
- **A browser** on `/v1/me`: embedded with one merchant, the host's own sign-in
  (bound by issuer); with several, the session bound to the store (AuthKit's
  `MintAccessToken` with `Group`, or your own `BoundScope`). Standalone or hosted,
  the merchant's backend mints `Client.CustomerToken` (an RFC 7523 assertion
  redeemed at the server's token endpoint, 15 minutes) or `Client.CustomerAssertion`
  for the browser to redeem with its own DPoP key; billing-ui's `tokenSource`
  refreshes it.
- **The admin console** exchanges the person's token for one bound to the chosen
  merchant (RFC 8693 with `group_id`, through the server's `openrails-console`
  client), refused where they hold no role.

The binding picks the merchant; permissions pick the routes.

## Permissions

A permission is `persona:resource:action` (`read`, `manage`): the
`billing.Permission` constants, listed by `billing.Permissions()`. Build roles
from the constants so a renamed one fails your build. Each permission has two
names: a route acting on merchant M admits the `merchant:` name held in M's scope
(with AuthKit, the group whose id is M's) or the `root:` name held in the
`RootScope` (every merchant). Each route's pair is in [routes](api/routes.md).

| Routes | `merchant:` / `root:` | Covers |
|---|---|---|
| admin reads | `MerchantCustomerRead` / `RootCustomerRead` | customer support's reads |
| admin changes | `MerchantCustomerManage` / `RootCustomerManage` | refunds, cancellations, credits, limits |
| catalog reads | `MerchantCatalogRead` / `RootCatalogRead` | products, prices, meters, limits, price migrations |
| catalog edits | `MerchantCatalogManage` / `RootCatalogManage` | edits, documents, migrations |
| merchant config | `MerchantConfigRead`, `MerchantConfigManage` / `RootConfig…` | PSPs, imports, findings, settings, billing export |
| metrics | `MerchantMetricsRead` / `RootMetricsRead` | metrics and dashboards |
| `/v1/app` | `MerchantEntitlementsRead` / `RootEntitlementsRead` | `POST /v1/app/entitlements/check` |
| `/v1/app` | `MerchantUsageRead`, `MerchantUsageManage` / `RootUsage…` | admissions, lease syncs, customer limits, running charges |
| `/v1/app` | `MerchantEventsRead` / `RootEventsRead` | the event log |
| `/v1/app` | `MerchantOrdersManage` / `RootOrdersManage` | merchant orders |
| none | `MerchantCustomersRead` / `RootCustomersRead` | declared; no route checks it |
| standalone server | — / `RootMerchantsRead`, `RootMerchantsDelete`, `RootMerchantsRestore`, `RootWorkerHealthRead` | the server's merchant directory and worker health, root only |

`root:admin-rate-limits:unlock` (`billing.RootAdminRateLimitsUnlock`) has no
`merchant:` name: with the config manage permission it clears a
[destructive hold](destructive-actions.md#review-and-clear), from a person other
than the one held. A group that is on mounts all its routes; roles decide what a
caller may use.

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

A host granting only root roles declares the `root:` names alone.

**Roles on the standalone server.** Team members and API keys hold a declared
role (`server.MerchantRoles`: `viewer`, `support`, `owner`) or a custom one,
`merchant:custom-<name>`, holding any `merchant:` permissions (a storefront key
with `MerchantCatalogRead` and `MerchantEntitlementsRead` reads the catalog and
checks entitlements, nothing more). Custom roles are AuthKit's, in the merchant's
group: managing them takes `server.MerchantRolesManage`, reading
`server.MerchantRolesRead`, and an editor grants only what they hold. Checks read
roles live; deleting one revokes the keys and invitations carrying it. The
server's AuthKit serves `/groups/{group_id}/roles`, `/members` and `/api-keys`;
the console manages them in Settings, Roles. Embedded, roles are the host's; a
host opts its staff into defining roles with `authkit.NewRoles(authkit.CustomRoles)`
or `rbac.Persona(billing.MerchantGroupPersona, authkit.CustomRoles)`.

## Your own auth

`authkit/auth` is standard-library only and versioned with AuthKit; implementing
it runs no AuthKit. [`examples/own-auth`](../examples/own-auth) is a complete one.

- `Authenticate(r)` reads the credential live: `ErrUnauthenticated` for none or
  unknown (with `ErrRevoked` once signed out), `ErrUnavailable` when your store is
  down. OpenRails passes no cookies; browsers send `Authorization`.
- `Identity()` names your issuer, the user's UUID as subject (the customer id),
  the user as its own invoker, and the credential.
- `RootScope` is `Scope{Authority, ID}` for your all-merchants staff group; a
  merchant's scope is `{the same Authority, the merchant's id}`.
- `BoundScope` is the merchant the credential acts at, from your own records; zero
  for none (and for a single merchant that declares your issuer).
- `Can(ctx, scope, permission)` answers exactly that permission in exactly that
  scope, live. A credential bound to one merchant holds nothing at another.
- `CheckRecentSignIn` (`auth.RecentSignInChecker`) is asked only by sensitive
  host-mapped customer operations: nil inside your window, else an
  `auth.Challenge` with `ErrStepUpRequired` and `MaxAge`.
- `KnownPermission` knows `billing.Permissions()`.

Your directory is `Deps.UserInfo` (`Get` and `Search`).

## Identities

- **Subject:** the native account acted as, a user (`SubjectUser`) or application
  (`SubjectApplication`). Its money and authority are used. A customer is a
  canonical UUID, the same on every credential.
- **Invoker:** the party actually acting, the subject or someone on its behalf,
  possibly another issuer's user. Spend limits, delegations, staff rate limits
  and the destructive ceiling key on it (`issuer|id` when another issuer vouches).
  An invoker acting for someone else, or an application, calls no `/v1/me` route.
- **Credential:** how it was proven (`CredentialSession`, `CredentialDeviceKey`,
  `CredentialAPIKey`, `CredentialSignedToken`, `CredentialAccessToken`). Only a user
  acting in person pays with their own saved card.

Provider writes record all three.

## The standalone server

The server runs its own AuthKit (its `auth:` section, [configuration](configuration.md#the-servers-authkit)),
with closed registration and local sign-in off unless `local_sign_in` says
otherwise; without it people sign in at a trusted issuer.

- **A merchant's scope is its AuthKit group**, whose id is the merchant's
  (`Server.MerchantScope`). A person's own sign-in is bound to no merchant; holding
  a role in one never picks it.
- **A customer is its issuer's subject** (OIDC Core §5.7): `billing.customers`
  records the issuer, and another issuer's credential with the same `sub` is
  `403 customer_issuer_mismatch`. A customer with no recorded issuer is the
  merchant's own (made through the APIs, a customer token, or the declared issuer).
  For a tenant-bound merchant the credential's tenant is checked too.

### Standalone account directory

`auth.default_tenant_id` (`AUTH_DEFAULT_TENANT_ID`) is a UUID generated once for
the server's own operator account directory and kept in configuration; it is never
inferred. `openrails migrate up` migrates OpenRails and AuthKit and passes it as
AuthKit's adoption target for existing accounts, keeping their ids: review that
mapping before migrating a populated database.

### Trusted issuers

With `auth.resource.id` set, the server accepts RFC 9068 access tokens
(`typ: at+jwt`) minted for that resource by its trusted issuers: AuthKit remote
applications in a merchant's group, from the manifest's `remote_application`
(holding the merchant's owner role) or registered at run time. One issuer acts for
one merchant. A token's permissions count within its application's role and its
scopes: `openrails:merchant` grants merchant permissions, `openrails:self` none (a
customer's own billing). A token whose `sub` equals its `client_id` is a client
acting for itself. An application without an authorization server mints customer
tokens at the server's token endpoint with an RFC 7523 assertion. Verification,
role mapping and DPoP are AuthKit's
([resource server](https://github.com/open-rails/authkit/blob/master/docs/resource-server.md)).
OpenRails cannot see an issuer's bans or revocations: the token's lifetime bounds
the exposure.

### Sessions and proxies

Billing mounts strip ambient cookies. The server's own console session keeps its
access token in memory and a rotating HttpOnly refresh cookie (`__Host-authkit_rt`
on HTTPS, `authkit_rt` on allowed plain-HTTP development); `POST /auth/v1/token`
restores it on reload, and logout revokes it. Every permission check acts as the
token's own sign-in, so a revoked session answers 401 `credential_revoked` at
once. Every attempt authenticates and authorizes again; nothing is memoized.

Set `trusted_proxies` to the real proxy CIDRs or `auth.http.direct_peer_ip: true`
(mutually exclusive); Cloudflare headers are trusted only from
`cloudflare_proxies`. Embedded hosts set `Config.TrustedProxies` and
`Config.CloudflareProxies`.

## Host customer APIs

A host that keeps its own account relationships (a hosted product linking its
accounts to merchants' customers) reuses OpenRails' customer checks while
OpenRails stores no relationship. Neither adds an HTTP route.

- `Engine.ResolveCustomer(ctx, authority, verified)` returns a `CustomerProof` for
  the existing customer the original request proves: its merchant, customer and
  stored `Issuer` (nil is the merchant's native namespace). It never
  authenticates again, admits a user acting in person (not a personal access
  token; a scoped token needs `openrails:self`), binds the merchant exactly as
  `/v1/me` does and changes nothing. The Verified must implement
  `auth.CredentialChecker`. A missing customer is `billing.ErrNotFound`.
- `Engine.CustomerRoutes(selection)` returns customer operations under its
  `Prefix` (default `/v1/me`; whole-segment parameters allowed). `Auth` verifies
  once; `Resolve` maps the request to one existing `CustomerBinding`, returning
  `billing.ErrNotFound` to hide a relationship. Operations: config, payments,
  entitlements, orders (list, detail, payment attempt, pay, confirm),
  subscriptions (list, detail, cancel, resume, payment method), payment methods
  (list, add, confirm, delete) and agreements (list, revoke). Every mutation asks
  for a recent sign-in; order creation is not mapped.
- Standalone, `server.Deps.CustomerAdmissionPolicy` may refuse an ordinary
  `/v1/me` request after identity, merchant and issuer checks and before the
  customer is admitted. Return an AuthKit refusal for the usual HTTP error; any
  other error fails closed. It does not run on staff, AuthKit, host-mapped or pay
  URL routes.

Customer records create no AuthKit groups. A hosted product calls
`Server.EnsureCustomerPermissionGroup` when a user creates a portal account; the
group's id is the customer's id, and it issues no API keys or signing
applications.
