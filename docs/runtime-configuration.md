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
server's `config.yaml` composes billing settings with its own AuthKit's: the
`auth` section is AuthKit's configuration (`server.Config.Auth`, an
`authkit.Config`) in AuthKit's own keys, and `AUTH_<PATH>` sets the key the
path names (`AUTH_TOKEN_ISSUER` is `auth.token.issuer`, `AUTH_SIGN_IN_DPOP`
`auth.sign_in.dpop`). An `auth` key AuthKit does not know refuses boot. The
loader handles YAML, environment variables and mounted secret files. Remote
consumers only construct a Client.

An embedded host supplying authentication does not need a standalone issuer or
signing key. The standalone server needs `auth.token.issuer` explicitly; no
billing URL is an issuer fallback. Declare `auth.http.direct_peer_ip` for
direct connections or trusted proxy ranges for a reverse proxy (with neither,
the server's AuthKit takes the top-level `trusted_proxies`); request headers
never declare their own trusted origin.

| Setting | Meaning |
| --- | --- |
| `auth.token.issuer` | The server's AuthKit issuer: JWKS beneath it, its routes beneath its path (else `/auth`). |
| `auth.keys.path` (`AUTH_KEYS_PATH`) | The signing keys' `keys.json` and `totp.key`. `AUTHKIT_ACTIVE_KEY_ID`, `AUTHKIT_ACTIVE_PRIVATE_KEY_PEM` and `AUTHKIT_PUBLIC_KEYS` give an inline key instead, frozen until a restart. |
| `auth.keys.allow_ephemeral_dev_keys` | Generate a signing key and a TOTP key when none is configured, under `auth.keys.path`. |
| `auth.keys.verify_only` | Verify tokens, mint none. A key-loading failure never selects it. |
| `auth.registration.native_user_mode` | `open`, `invite_only` or `closed`; unset is closed. Registration needs `local_sign_in`. |
| `auth.registration.allow_missing_senders` | Construct authentication without email or message delivery. |
| `auth.token.allow_private_network_jwks` | Fetch a trusted issuer's keys from a private address, for local federation. |
| `auth.resource.id`, `auth.resource.public_url` | The resource (RFC 8707) trusted issuers mint tokens for, and where clients reach it; default the id's origin. |
| `auth.sign_in.dpop` | `optional` or `required`: how the server's AuthKit issues tokens to its own users. It never changes how tokens are validated ([AuthKit](https://github.com/open-rails/authkit/blob/master/docs/resource-server.md#dpop)). |
| `auth.http.rate_limits` | AuthKit's own rate-limit buckets. |

The server sets the rest of the product's own: the merchant roles, the
`openrails` API key prefix and issued audience, the resource's scopes
(`openrails:merchant`, `openrails:self`), AuthKit's River schema and its HTTP
mount. The server's former keys (`auth.issuer`, `auth.keys_path`,
`auth.request_origin`, `resource_server` and the others) refuse boot and name
their replacements. `naming` is the rename policy for merchant names;
usernames follow `auth.username`.

The root owner always needs a second factor, so the server refuses to start
unless one can be enrolled: a 16, 24 or 32-byte `totp.key` beside `keys.json`
under `auth.keys.path`, or an email or SMS sender.

`rate_limits` names the limited buckets (`checkout`, `payment`, `subscribe`,
`webhook`; any other key refuses boot) and defaults to the built-in limits;
`rate_limits_disabled` declares that the host supplies rate limiting instead
([rate limiting](rate-limiting.md)).

HyperSwitch has its own `hyperswitch.allow_loopback_http` exception. It permits
literal loopback HTTP fixtures only with sandbox provider credentials. This
permission never allows plaintext stored secrets.

## URL ownership

- `public_billing_base_url` names the public billing mount, excluding `/v1`.
  For example, `https://shop.example/billing` produces callbacks under
  `https://shop.example/billing/v1/...`. Configure it only for features needing
  external callbacks or billing links.
- `auth.resource.public_url` names the server's public origin for
  sender-constrained tokens' proofs, for example `https://shop.example`;
  a merchant's API host is admitted too.
- `auth.token.issuer` independently defines the server's own token trust.
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
