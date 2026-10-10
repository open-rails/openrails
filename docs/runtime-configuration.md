# Runtime security and public URLs

Security policy is independent of `test_mode: sandbox | live`. Both postures
require encrypted managed database credentials, verified provider webhook
signatures and an explicit provider write mode.
`env` / `ENV`, `api_url` / `API_URL`, and
`new_subscription_collection_policy` / `NEW_SUBSCRIPTION_COLLECTION_POLICY` are
retired inputs and refuse configuration loading.

## Authentication

Embedded billing receives `openrails.Config` and is given the host's `Auth`
where its routes are mounted; it loads no AuthKit configuration. The standalone
server's `config.yaml` composes billing settings with the server's own AuthKit
settings (`auth`, `server.AuthConfig`); its loader handles YAML, environment
variables and mounted secret files. Remote consumers only construct a Client.

An embedded host supplying authentication does not need a standalone issuer or
signing key. The standalone server needs `auth.issuer` explicitly. No billing URL is an issuer fallback. HTTPS is required.
Declare `auth.direct_peer_ip` for direct connections or trusted proxy ranges for
a reverse proxy; request headers never declare their own trusted origin.

These independent exceptions default to false:

| Setting | Explicit permission |
| --- | --- |
| `auth.allow_loopback_http` | HTTP issuer and request origin only on localhost or a literal loopback address. |
| `auth.allow_private_network_jwks` | AuthKit private-network JWKS retrieval for local federation. |
| `auth.allow_missing_senders` | Construct authentication without email/message delivery. |
| `auth.allow_ephemeral_signing_key` | Generate a signing key and a TOTP key when configured key material is absent, written to `auth.keys_path` when it is set (which must then be writable). |

The root owner always needs a second factor, so the control plane refuses to
start unless one can be enrolled: a 16, 24 or 32-byte key at
`auth.keys_path/totp.key` (beside `keys.json`), or an email or SMS sender.

`auth.mint_disabled` explicitly selects verification-only operation. A key-loading
failure never silently selects it. Inline signing keys retain their restart-only
rotation warning. `rate_limits` names the limited buckets (`checkout`, `payment`,
`subscribe`, `webhook`; any other key refuses boot) and defaults to the built-in
limits; `rate_limits_disabled` declares that the host supplies rate limiting
instead ([rate limiting](rate-limiting.md)).

HyperSwitch has its own `hyperswitch.allow_loopback_http` exception. It permits
literal loopback HTTP fixtures only with sandbox provider credentials. This
permission never allows plaintext stored secrets.

## URL ownership

- `public_billing_base_url` names the public billing mount, excluding `/v1`.
  For example, `https://shop.example/billing` produces callbacks under
  `https://shop.example/billing/v1/...`. Configure it only for features needing
  external callbacks or billing links.
- `auth.request_origin` names the public request origin for DPoP checks, for
  example `https://shop.example`. It cannot include `/billing`: a proof names
  the origin plus the request's path as the server receives it, so a host
  mounts billing without rewriting its path.
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

## Database schema

`database.schema` / `DATABASE_SCHEMA` (default `billing`; `Config.Database.Schema`
embedded) names the Postgres schema that holds every OpenRails table, function
and type. It must be a plain identifier
(letters, digits, underscore). All OpenRails SQL is authored in `billing` and
runs there verbatim. Any other schema is reached by one token-aware rewrite,
applied to migrations and to every statement at runtime: it moves schema
qualifiers, the name after `SCHEMA`, `SET search_path` values and
`'billing.<table>'::regclass` / `to_regclass('billing.<table>')` literals. Every other string
literal and comment is data and is never rewritten. River and AuthKit keep their
own schemas: OpenRails' River is in `database.river_schema` /
`DATABASE_RIVER_SCHEMA` (`Config.Database.RiverSchema`), default the schema plus
`_river`.

The server (`New`) creates or upgrades these schemas at boot, before anything
else touches the database. Its role owns every object and is the role
OpenRails runs as; there are no grants and no row-level security. When two
apps share one schema, both connect as one role, or `SET ROLE` to a shared one
on every connection. The whole schema is listed in
[`api/schema.txt`](../api/schema.txt); see [compatibility](compatibility.md#database-schema).
