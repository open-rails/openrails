# The auth model: one credential or two?

The rule across both deployment modes is: **one credential per trust domain.**

- **Embedded:** your app and OpenRails are the same process — one trust domain. The frontend
  uses its normal session credential for everything, including the mounted billing routes.
  Your code verifies it and hands OpenRails the resulting identity through a Go interface
  (`billingauth.Authenticator` / `DelegatedAuthenticator`). **One token.** OpenRails never
  parses your credential at all.
- **Standalone:** OpenRails is a separate system across a network boundary, and it always
  runs its own AuthKit control plane — the in-process authority that issues and verifies
  these credentials, holds the runtime merchant/issuer registry, and gates admin routes.
  (There is no control-plane-less "verifier-only" standalone; private/self-hosted
  registration is the only standalone mode in this repo. Public hosted registration
  belongs in the private OpenRails SaaS layer.)
  Identity claims that cross that boundary must be independently verifiable, so each caller
  class gets a credential scoped to exactly what it may do:
  - your **backend** uses an **API key** (`openrails_st_...`) or a first-party OIDC
    service JWT — server-to-server, never sent to browsers;
  - your **frontend** uses a short-lived **delegated access token** that *your own backend*
    mints and signs — browser-direct, self-service-scoped.

## Why the browser holds two tokens in standalone mode

In standalone mode the browser does hold two tokens: its normal session token for your
API, and a delegated token for OpenRails. **This is deliberate, not incidental.** The
alternative — OpenRails accepting your webserver's session JWTs directly — was considered
and rejected for four reasons:

1. **Your session tokens would leave your trust domain.** Every billing call would ship a
   full-power webapp credential to another system. If that system (or its logs) is ever
   compromised, the attacker holds tokens that unlock *your* API. A delegated token is
   worthless anywhere except the OpenRails self-service surface — a fully compromised
   OpenRails yields nothing replayable against you.
2. **Every session leak would become a billing leak.** Session tokens pass through many
   hands (browser extensions, analytics, your own microservices). Today none of those
   exposures touch billing; with pass-through acceptance, all of them would.
3. **Audience discipline.** A JWT recipient must reject tokens not addressed to it
   (RFC 7519 `aud`). Accepting foreign-audience tokens is the classic confused-deputy
   anti-pattern, and OpenRails fails closed on it: a token carrying a normal `sub` is
   rejected on sight.
4. **Least privilege.** Delegated tokens carry only the OpenRails audience, the acting
   delegated subject, an optional narrow OpenRails permission set, and a short TTL.
   Your session token can do everything your app allows; it should never be spendable
   as a billing credential.

The cost is small, because **your backend mints the delegated token itself** — with the
same signing key it already uses for its own auth, if you like. "Getting a token for
OpenRails" is one authenticated fetch to *your own* API, not a separate login or a
round-trip to a foreign identity provider. Wrap it in a token-exchange endpoint plus a
small frontend helper that fetches and auto-refreshes, and client code sees one system
(this is the same shape as Stripe's ephemeral keys or Plaid's link tokens). The minting
flow is in the README's standalone integration guide.

## Design parity between the modes

The two modes have exact design parity: both translate *your* credential into a billing
principal at the trust boundary. Embedded does the translation through an in-process
interface (`billingauth.Authenticator` / `DelegatedAuthenticator`); standalone does the
same translation as a signed wire artifact (the delegated token, verified against your
registered JWKS). Same seam, two serializations.

| Surface | Embedded credential | Standalone credential |
|---|---|---|
| Backend / server-to-server | In-process call — no credential | Service token (`/v1/merchant/*`) |
| Browser self-service | Your session credential, via `DelegatedAuthenticator` | Delegated token, minted by your backend (`/v1/me/*`) |
| User billing routes | Your session credential, via `Authenticator` | AuthKit user JWT (AuthKit-backed deployments) |
| Merchant/admin routes | Live `merchant:*` permissions, checked per request | Same (requires the control plane) |

## Identity semantics

- OpenRails treats the subject id (`UserContext.UserID` embedded, `delegated_sub`
  standalone) as an opaque principal — it is your user id, and OpenRails keys billing
  state to it verbatim. Identity attributes (email, username) are optional,
  non-authoritative metadata for things like checkout prefill.
- Admin authority is **live `merchant:*` permission in the caller's merchant group**,
  evaluated per request against the control plane. OpenRails never interprets your role
  names, and there is no role-string fallback.
- Browser origin policy for delegated calls is configured on the AuthKit
  `remote_application` issuer record, not in OpenRails runtime config.

## Client IP and development keys

AuthKit requires an explicit client-IP posture outside development. Configure
OpenRails' top-level `trusted_proxies` with the actual proxy CIDRs, or set
`auth.direct_peer_ip: true` (`AUTH_DIRECT_PEER_IP=true`) when client connections
arrive directly. Embedded `AttachOptions.TrustedProxies` and `DirectPeerIP`
forward the same choices; combining direct-peer and proxy declarations is an
error. Generic proxy trust does not trust client-supplied `CF-Connecting-IP`.

Development signing keys must persist to a writable `auth.keys_path`
(`AUTHKIT_KEYS_PATH`) directory. Tests use their own temporary directories;
production mounts its managed signing-key directory.

After in-process remote-application registration, call
`ControlPlane.ReloadRemoteApplications` for immediate verification. Registrations
made by another process converge through the bounded issuer-registry refresh.
An arbitrary unverified token issuer does not trigger its own database lookup.
