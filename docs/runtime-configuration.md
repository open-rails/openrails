# Runtime security and public URLs

Security policy is independent of `test_mode: sandbox | live`. Both postures
require encrypted managed database credentials, verified provider webhook
signatures, explicit provider write permission and declared rate-limit policy.
`env` / `ENV`, `api_url` / `API_URL`, and
`new_subscription_collection_policy` / `NEW_SUBSCRIPTION_COLLECTION_POLICY` are
retired inputs and refuse configuration loading.

## Authentication

An embedded host supplying authentication does not need a standalone issuer or
signing key. When constructing the OpenRails authentication control plane, supply
`auth.issuer` explicitly. No billing URL is an issuer fallback. HTTPS is required.
Declare `auth.direct_peer_ip` for direct connections or trusted proxy ranges for
a reverse proxy; request headers never declare their own trusted origin.

These independent exceptions default to false:

| Setting | Explicit permission |
| --- | --- |
| `auth.allow_loopback_http` | HTTP issuer and request origin only on localhost or a literal loopback address. |
| `auth.allow_memory` | AuthKit ephemeral state and rate limiting in one process without Redis. |
| `auth.allow_private_network_jwks` | AuthKit private-network JWKS retrieval for local federation. |
| `auth.allow_missing_senders` | Construct authentication without email/message delivery. |
| `auth.allow_ephemeral_signing_key` | Generate a disposable signing key when configured key material is absent. |

`auth.mint_disabled` explicitly selects verification-only operation. A key-loading
failure never silently selects it. Inline signing keys retain their restart-only
rotation warning. Runtime constructors supply rate-limit and captcha defaults
when omitted. `rate_limits_disabled` explicitly declares that the host supplies
rate limiting instead.

HyperSwitch has its own `hyperswitch.allow_loopback_http` exception. It permits
literal loopback HTTP fixtures only with sandbox provider credentials. This
permission never allows plaintext stored secrets.

## URL ownership

- `public_billing_base_url` names the public billing mount, excluding `/v1`.
  For example, `https://shop.example/billing` produces callbacks under
  `https://shop.example/billing/v1/...`. Configure it only for features needing
  external callbacks or billing links.
- `auth.request_origin` names the public request origin for DPoP checks, for
  example `https://shop.example`. It cannot include `/billing`. The original
  escaped request path supplies that prefix, preserving encoded slashes and
  repeated slashes. Hosts that rewrite paths can supply the existing
  `WithDPoPRequestURL` resolver with the original external target.
- `auth.issuer` independently defines token trust.
- `dashboard_base_url` independently names the administrative console destination.
- A remote Client's server URL selects its transport destination; it does not
  construct local HTTP routes or determine issuer trust.

## New agreements

New supported recurring purchases use engine-owned saved NMI or Stripe payment
methods and explicit agreement confirmation. Unsupported rails and free/trial
terms fail admission; they do not fall back to provider-owned enrollment.
`engine_admission_hold` still pauses new payment admission independently.

Existing/imported subscriptions retain their stored collector and provider
references. Startup configuration does not transfer collection ownership or
retire provider schedules. Accepted sessions replay their persisted terms.
Stripe one-off checkout uses accepted inline product and price data. Native
catalog requirements follow the operation and rail, independently of stored
subscription ownership. Explicit historical provider catalog links remain usable.
