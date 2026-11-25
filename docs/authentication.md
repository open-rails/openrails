# Authentication

Who signs in to OpenRails, with what, in each deployment mode: embedded (your Go server runs the engine in process, [embedded.md](embedded.md)), standalone (the `openrails` server you host, [standalone.md](standalone.md)) and SaaS (the hosted platform, [client.md](client.md#credentials)).

## Three parties

| Party | Who | Routes |
|---|---|---|
| [Customers](#customers) | your app's users, who pay | `/v1/me` |
| [Your backend](#your-backend) | your app's server | `/v1/admin`, `/v1/catalog`, `/v1/merchant`, `/v1/metrics` and `/v1/app` |
| [Staff and operators](#staff-and-operators) | the people who run a merchant's billing, and the operators of the deployment | `/v1/admin`, `/v1/catalog`, `/v1/merchant` and `/v1/metrics`, from the `/admin` console (standalone, SaaS) or your own pages (embedded) |

Every request acts at the one merchant its credential is bound to, by who issued it; the request never names one, and a valid credential bound to none is `403 merchant_unresolved`. A staff or programmatic route then checks its permission pair: its `merchant:` name in the bound merchant's scope, or its `root:` name in the root scope ([route reference](api/routes.md)). Staff routes never ask for a recent sign-in: their authority is the permission.

The standards: OAuth 2.0 (RFC 6749) with PKCE (RFC 7636), resource indicators (RFC 8707), JWT access tokens (RFC 9068), JWT bearer assertions (RFC 7523), token exchange (RFC 8693), DPoP (RFC 9449) and SCIM 2.0 (RFC 7644). AuthKit verifies tokens, maps roles and checks DPoP proofs.

## Customers

Customers are your app's users. OpenRails gives them no account and no sign-in of its own: a customer is your user's UUID (`billing.CustomerID`), and they sign in only at your app.

| Mode | Credential at `/v1/me` | Issued by | Carries | Bound to the merchant by |
|---|---|---|---|---|
| Embedded | your app's own session credential, on your mount | your app's auth | who your `Routes.Auth` says the user is | its `BoundScope`, else the merchant whose declared `Issuer` signed it |
| Standalone | an `openrails:self` access token whose `aud` is the server's resource, its `public_url` | (a) your app's issuer, or (b) the server's token endpoint, for an assertion your backend signs | `sub` (the customer), the scope and verified contact claims; no permissions | the trusted issuer: its remote application's merchant |
| SaaS | the same token, for the platform's resource | (b) the platform's token endpoint, for an assertion signed with a key registered under **Integration → Customer tokens**, or (a) an issuer registered there | the same | the merchant the key or issuer is registered under |

### Embedded

One sign-in, one credential, no second token. The browser calls your own mount (`{Prefix}/v1/me/…`) with what your app already uses, in `Authorization` (the mount strips ambient cookies). `Routes.Auth` says who it is; only a `session`, `device_key` or `access_token` credential is the user in person, who may pay. With one merchant, declare your `Issuer`; with several, bind each session to its store ([merchant binding](embedded.md#auth)).

```text
browser                      your app (Routes.Auth)          engine, in process
  | GET {Prefix}/v1/me/…           |                                |
  | Authorization: <session>       |                                |
  |------------------------------->| user U, bound to merchant M    |
  |                                |------------------------------->| U's billing at M
  |<-------------------------------|<-------------------------------|
```

### Standalone and SaaS

The browser holds a second token, `openrails:self`, minted for OpenRails' resource alone: its `aud` is the server's resource identifier (RFC 8707, RFC 9068 §4). That separation is the point:

- it can't be replayed elsewhere: your app's API, which verifies its own audience, refuses it;
- your app's session token, whose `aud` is your app, is refused by OpenRails, so OpenRails never receives the credential that acts as the user inside your app;
- `openrails:self` grants no permissions: it reaches one customer's own billing, for minutes (15 by default for the tokens the server mints), and a DPoP-bound one (RFC 9449) works only with its browser's key.

A trusted issuer's token acts as the customer only with `openrails:self`: any other, a staff member's `openrails:merchant` one included, is `403 permission_required` at `/v1/me`. A trusted issuer's customer is pinned to that issuer: `billing.customers` records it, and another issuer's credential with the same `sub` is `403 customer_issuer_mismatch`. A customer your backend names in an assertion is the merchant's own. Verified contact claims in either kind of token fill the customer's contact ([customer contacts](standalone.md#customer-contacts)).

**(a) Your issuer mints it.** Your app's authorization server is one of the merchant's trusted issuers: on a standalone server, the manifest's `remote_application` or an issuer registered at run time; on SaaS, an issuer registered under **Integration → Customer tokens**. It registers OpenRails' resource with the scope `openrails:self` and a public client for the browser ([trusted issuers](standalone.md#trusted-issuers)). The browser trades the user's session token for the billing token at your issuer by token exchange (RFC 8693), bound to a DPoP key:

```text
browser                         your issuer                       OpenRails
  | POST /oauth2/token               |                                  |
  | grant_type=token-exchange        |                                  |
  | subject_token=<session token>    |                                  |
  | resource=<server URL>            |                                  |
  | scope=openrails:self, DPoP proof |                                  |
  |--------------------------------->|                                  |
  |<---------------------------------| at+jwt: aud=resource, sub=U      |
  | GET /v1/me/…  Authorization: DPoP <token>, DPoP proof               |
  |-------------------------------------------------------------------->| issuer → merchant M
  |<--------------------------------------------------------------------| U's billing at M
```

Your issuer can also mint that RFC 9068 token directly, without the exchange ([client: credentials](client.md#credentials)).

**(b) A customer token.** A backend with no authorization server registers a signing key as the merchant's application (standalone: the manifest's `remote_application`, or the console's **Access → Signing keys**; SaaS: **Integration → Customer tokens**). It signs an RFC 7523 assertion naming the customer, and OpenRails' token endpoint mints the token. `CreateCustomerToken` signs and redeems in one call; `CreateCustomerAssertion` only signs, for the browser to redeem with its own DPoP key. An assertion lives at most two minutes and redeems once.

```text
browser               your backend                   token endpoint           OpenRails API
  | your own route       |                                 |                        |
  |--------------------->| CreateCustomerToken             |                        |
  |                      | assertion: iss=your app, sub=U, |                        |
  |                      | aud=token endpoint, jti         |                        |
  |                      |-------------------------------->| jwt-bearer grant,      |
  |                      |                                 | scope=openrails:self   |
  |                      |<--------------------------------| at+jwt: aud=resource   |
  |<---------------------| access_token, expires_in        |                        |
  | GET /v1/me/…  Authorization: Bearer <token> ---------------------------------->| merchant M
```

The hosted platform also offers its own customer sign-in to merchants' sites (**Sign-in clients**, OpenID Connect); its console documents it.

## Your backend

| Mode | Credential | Issued by | Carries | Bound to the merchant by |
|---|---|---|---|---|
| Embedded | none: `Engine.Merchant(id)` is the in-process `*Client` | nobody | no permission check: authorize your own request first | the handle's merchant id |
| Standalone | an access token from your issuer (`client_credentials`, scope `openrails:merchant`), or a merchant API key | your issuer; the server's AuthKit for a key | the client's permissions at your issuer, within the application's role and the scope; a key, its role's | the issuer's merchant; the key's merchant |
| SaaS | a merchant API key | the console's **API keys** page | its role's permissions | the key's merchant, which `GetAdminAccess` reports as `ScopeID` |

Over HTTP, `NewRemote` takes one `RemoteCredential` per merchant and every call sends `OpenRails-Expected-Merchant`, so a credential bound to another merchant is `412 merchant_mismatch` before anything runs ([client: handles](client.md#handles)). A person's token is refused on `/v1/app` (`403 application_required`).

**Embedded.** Your server calls the engine in process and presents nothing. A backend in another process calls `/v1/app` (`RouteGroups.Programmatic`) with an application credential your `Routes.Auth` knows.

**Standalone: client credentials.** Your issuer registers a confidential client with the `client_credentials` grant, OpenRails' resource and the permissions the backend needs, by OpenRails' names ([permissions](client.md#credentials)). The token's `sub` is its `client_id`: a client acting for itself. Its permissions count within the application's role (a manifest `remote_application` holds the merchant's owner role) and the `openrails:merchant` ceiling. The token source goes in `RemoteCredential.Token`:

```text
your backend                      your issuer                        OpenRails
  | POST /oauth2/token                 |                                   |
  | grant_type=client_credentials      |                                   |
  | scope=openrails:merchant           |                                   |
  | resource=<server URL>              |                                   |
  |----------------------------------->|                                   |
  |<-----------------------------------| at+jwt: sub=client_id, permissions|
  | GET /v1/…  Authorization: Bearer <token>                               |
  | OpenRails-Expected-Merchant: M                                         |
  |----------------------------------------------------------------------->| issuer → M; permission
  |<-----------------------------------------------------------------------| OpenRails-Bound-Merchant: M
```

**API keys.** A merchant API key (`RemoteCredential.APIKey`, prefixed `openrails_st_`) holds one role at one merchant (`viewer`, `support`, `owner` or a custom one), never beyond its minter's, and its secret is shown once. A standalone server mints it in the console's **Access → API keys**, signed in with the server's own account; SaaS under **API keys**. A key alone can't mint customer tokens: that takes `RemoteCredential.Assertion`.

```text
your backend                                           OpenRails
  | GET /v1/…  Authorization: Bearer openrails_st_…        |
  | OpenRails-Expected-Merchant: M                         |
  |------------------------------------------------------->| the key's merchant and role
```

## Staff and operators

| Mode | Credential | Issued by | Carries | Bound to the merchant by |
|---|---|---|---|---|
| Embedded | your app's own session credential | your app's auth | the roles you build from the `billing.Permission` constants | its `BoundScope`, else the declared `Issuer`; `root:` permissions reach every merchant |
| Standalone, the server's own accounts | the server's own account, then a token exchanged for the chosen merchant | the server's AuthKit | the person's role in that merchant, or their root role | the merchant's id, named in the exchange |
| Standalone, in your own site | an `openrails:merchant` access token your app gives its signed-in staff | the merchant's trusted issuer | the person's permissions there, within the application's role | the issuer's merchant |
| Standalone, the console at a trusted issuer | an `openrails:merchant` access token, by authorization code with PKCE | one of root's trusted issuers | the person's permissions there, within the application's role | the issuer's group |
| SaaS | the platform's own account, then the same exchange | the platform | the person's role in that merchant | the merchant's id, named in the exchange |

### Embedded

No OpenRails login. Staff sign in to your app, and your roles hold OpenRails' permissions, built from the `billing.Permission` constants so a renamed one fails your build ([roles](embedded.md#auth)). They work in your own admin pages, which call the staff routes on your mount with their session, or your server calls the `*Client` after its own check. The `/admin` console is the standalone server's.

```text
staff browser                your app (Routes.Auth)                  engine
  | GET {Prefix}/v1/admin/…        |                                      |
  | Authorization: <session>       |                                      |
  |------------------------------->| who; holds merchant:customer:read    |
  |                                | at M, or root:customer:read?         |
  |                                |------------------------------------->| admit, or 403 permission_required
```

### Standalone, in your own site

Staff signed in to your app use the admin screens in your own pages: they render billing-ui's `BillingAdmin` and call the server's staff routes cross-origin, which answer any origin without credentials. There is no second sign-in, no redirect and no exchange at the server. Your app, the merchant's trusted issuer, gives the person an RFC 9068 access token (`typ: at+jwt`) for the server:

- `iss` your issuer, signed with a key it registered; `aud` the server's `public_url`; `sub` the person; `exp`;
- `scope` including `openrails:merchant`;
- `permissions`: the person's `merchant:` permissions by name, or `roles` the application's role map names.

The token is bound to its issuer's merchant, and each permission counts within the application's role there (the merchant's owner, from the manifest's `remote_application`); `root:` permissions never count. With AuthKit, your app registers the server as a resource with `openrails:merchant` and your browser client exchanges the person's session for its token (RFC 8693, resource indicator RFC 8707). Set-up: [standalone](standalone.md#admin-screens-in-your-own-site).

```text
staff browser (your page)            your app (the merchant's issuer)        OpenRails
  | signed in to your app                     |                                   |
  | token for <public_url>, openrails:merchant|                                   |
  |------------------------------------------>| the person's merchant: permissions|
  |<------------------------------------------| access token (aud=<public_url>)   |
  | OPTIONS, then GET /v1/admin/…  Authorization: Bearer <token> ---------------->| issuer → merchant M;
  |                                           |                                   | holds the route's permission?
```

### Standalone, the console at a trusted issuer

Root's trusted issuers (from the bootstrap manifest) that sign people in are buttons beside the server's own accounts on the console's sign-in page, each shown only while a probe says its sign-in works ([standalone](standalone.md#the-console)). A merchant's trusted issuer is never one.

The console is the OAuth 2.0 public client `openrails-console` at that issuer: the authorization code flow with PKCE (RFC 6749, RFC 7636) and OpenRails' resource as the resource indicator (RFC 8707). The access token it receives is minted for the server's resource by the issuer, carrying the person's permissions there, which count within the application's role. There is no token exchange: the console calls OpenRails with that token and renews it with the issuer's refresh token.

The issuer registers:

- OpenRails' resource, the server's `public_url`, with the scope `openrails:merchant` and a permission ceiling covering what staff do;
- the public client `openrails-console`, with the redirect URI `<origin>/admin/callback`, the post-logout redirect URI `<origin>/admin/login`, the authorization code grant (with refresh tokens) and that resource;
- roles granting staff OpenRails' permissions by name, which its tokens for the resource carry.

The console asks for `openid profile email openrails:merchant`. It checks the OpenID Connect ID token and uses it only to name the person and to end the issuer's session at sign-out; OpenRails accepts only the access token. People, roles and second factors are the issuer's to manage.

```text
staff browser (/admin)            root's trusted issuer                OpenRails
  | GET /.well-known/openid-configuration |                                 |
  | authorization_endpoint: client_id,    |                                 |
  |   redirect_uri=<origin>/admin/callback|                                 |
  |   code_challenge (S256),              |                                 |
  |   resource=<server URL>,              |                                 |
  |   scope=openid profile email openrails:merchant                         |
  |-------------------------------------->| the person signs in there       |
  |<--------------------------------------| /admin/callback?code=…          |
  | token_endpoint: code, code_verifier   |                                 |
  |-------------------------------------->|                                 |
  |<--------------------------------------| access token (aud=resource,     |
  |                                       | permissions), ID token, refresh |
  | GET /v1/admin/…  Authorization: Bearer <access token>                   |
  |------------------------------------------------------------------------>| issuer → its group
```

### Standalone, the server's own accounts

People sign in at `/admin` with the server's own AuthKit accounts, always offered. Every account needs a second factor, a passkey or an authenticator app: a password alone gets no session until one is enrolled, and every later sign-in asks for it. Accounts join by invitation only:

1. The first operator comes from `openrails push-auth-bootstrap`: a bootstrap manifest user with `root_role: root:owner`.
2. A person who manages a merchant's people (or root's) invites someone by email with a role, in the console's **Access → People**.
3. The invitee follows the emailed link to `/admin/invite` and creates an account, or accepts with the one they hold.

The console then exchanges the session's token for one bound to the merchant the person picks: RFC 8693 at the server's token endpoint, as its `openrails-console` client, naming the merchant's id as `group_id` and binding the result to a DPoP key. The exchange is refused (`invalid_target`) where the person holds no role in that merchant and no root role. The session keeps its access token in memory and a rotating HttpOnly refresh cookie (`__Host-authkit_rt` on HTTPS) that ends 30 days after sign-in; a revoked session answers `401 credential_revoked` at once.

```text
staff browser (/admin)                server's AuthKit                     OpenRails API
  | sign in, second factor --------------->|                                     |
  |<------------- session token, refresh cookie                                  |
  | pick merchant M                        |                                     |
  | POST /oauth2/token                     |                                     |
  |   grant_type=token-exchange            |                                     |
  |   client_id=openrails-console          |                                     |
  |   subject_token=<session token>        |                                     |
  |   resource=<server URL>                |                                     |
  |   scope=openrails:merchant             |                                     |
  |   group_id=M, DPoP proof               |                                     |
  |--------------------------------------->| a role in M, or a root role?        |
  |<---------------------------------------| token bound to M                    |
  | GET /v1/admin/…  Authorization: DPoP <token>, DPoP proof ------------------->| merchant M
```

The console offers both.

### SaaS

Staff sign in with the hosted platform's own accounts. An owner invites a person to a merchant with a role (`owner`, `support` or `viewer`) on the console's **Team** page: an account that verified that email joins at once, anyone else registers from the emailed link. The console binds each session to the chosen merchant by the same exchange as the standalone server's own accounts. Sign-in at a trusted issuer isn't offered on the platform.

## The standalone server's AuthKit

The server always runs its own AuthKit, configured from the server's own settings ([standalone](standalone.md#the-servers-authkit)). It:

- verifies every credential: trusted issuers' tokens against their registered keys, its own tokens, and API keys;
- mints the tokens bound to OpenRails: customer tokens from assertions (RFC 7523) at its token endpoint, and the console's exchanged tokens;
- holds each merchant's customer directory, which trusted issuers push to over SCIM at `/directory/scim/v2` ([customer contacts](standalone.md#customer-contacts));
- serves its JWKS at its issuer plus `/.well-known/jwks.json`;
- serves its staff accounts: sign-in, account security, invitations, and people, roles, API keys and signing keys under the console's **Access**.

The hosted platform runs the same server with its own account policy.
