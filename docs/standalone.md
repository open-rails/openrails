# Self-hosting OpenRails

The standalone server is OpenRails as its own service: one binary,
`openrails run-server`, serving the HTTP API at `/v1`, the staff console at
`/admin`, the payment page on its checkout origin, provider webhooks and the
background workers. Your application calls it over HTTP, from Go with the same
`*openrails.Client` the embedded engine hands out ([the client](client.md)), or
from any stack ([without the Go client](client.md#without-the-go-client)). You
operate it, its PostgreSQL and its secrets. To run billing inside your own Go
process instead, see [the embedded guide](embedded.md).

## Requirements

| Service | Needed | Holds | Losing it |
|---|---|---|---|
| PostgreSQL 18+ | yes | everything: the ledger, grants, subscriptions, access, catalog, provider intents and River's queue. OpenRails owns one schema (`billing`), its River schema (`billing_river`) and the server's AuthKit schema (`profiles`); the instance may be shared. | data loss. Provider-owned facts can be re-read with a PSP import; the ledger, credits, access and catalog exist nowhere else. |
| ClickHouse 26.8 LTS | for usage | the event store usage needs: usage reporting, the gate, meters, limits and usage history ([event store](#event-store)) | a ClickHouse without the store's identity is refused at boot: restore its backup |
| Redis 6.2+ or Garnet | for more than one instance | rate limits, admin lockouts, captcha challenges, card-testing declines, AuthKit's rate limits and spent DPoP proofs | each instance keeps them in memory: right for one instance only. No money path needs Redis, and nothing in it needs a backup. |
| Vault | optional | with `vault.kv_mount`, every merchant's configuration, credentials included; a Transit mount signs for Solana crankers | without a KV mount the merchant manifest is the configuration, read-only |
| SMTP | optional | billing and operator mail, the server's own sign-in codes | no email; operator alerts still reach webhooks |

One PostgreSQL role runs migrations, the server, the workers and the CLI, and
owns every object it creates: there are no grants and no row-level security.
Tenant isolation is the `merchant_id` (or `psp_id`) predicate every query
carries. Two applications sharing a schema connect as that role or `SET ROLE`
to it.

## Install

| Form | Where |
|---|---|
| Binary | the release assets of each `vX.Y.Z` tag: Linux, macOS and Windows, amd64 and arm64, with checksums, SBOMs and provenance |
| Image | `docker.io/openrails/openrails:vX.Y.Z` (also `X.Y`, `latest`, and on `ghcr.io/open-rails/openrails`); its entrypoint is the `openrails` CLI |
| Helm | `oci://ghcr.io/open-rails/charts/openrails`, chart `X.Y.Z` for image `vX.Y.Z` ([Kubernetes and Helm](#kubernetes-and-helm)) |
| Source | `task build` builds `bin/openrails`, embedding the console if `task admin-build` ran and the payment page if `task payment-page-build` ran |

`openrails version` prints the version, commit and build date. For a local stack
from a checkout, `task docker-up` starts PostgreSQL, Garnet and the server on
`:3053` (`task docker-down`, `task docker-reset` deletes the database volume).

## First run

The example's server files are a working local configuration. `config.yaml` is
the process: listeners, database, Redis, the server's AuthKit and route groups.

```yaml guide:standalone-config
provider_write_mode: full # full | limited | readonly: how much it may do at the PSPs
# The merchant's configuration, its PSPs and their keys, is merchant.yaml: read
# at boot and read-only. To change it in the console instead, name a Vault KV
# mount (vault.kv_mount) and keep only the merchant's name in merchant.yaml.

host: 0.0.0.0
port: 3053
public_billing_base_url: http://localhost:3053 # where browsers and PSPs reach this server

db:
  url: postgres://openrails:openrails@postgres:5432/openrails?sslmode=disable
redis: # optional for one server; replicas share rate limits and spent proofs through it
  addr: redis:6379

# The server's own AuthKit, by AuthKit's keys: its signing key, and the
# resource server it is. Its sign-in stays off: people sign in at the app,
# which merchant.yaml declares the merchant's trusted issuer.
auth:
  # Stable example platform/operator directory, independent of the app's own.
  default_tenant_id: 51336692-ef57-4cdb-a189-a8d388465c11
  token:
    issuer: http://localhost:3053
    allow_private_network_jwks: true # local only: the app's keys at http://localhost:8080
  keys:
    path: /vault/auth
    allow_ephemeral_dev_keys: true   # local only: generates its key under keys.path
  http:
    direct_peer_ip: true             # clients connect directly; behind a proxy, set trusted_proxies
  registration:
    allow_missing_senders: true      # it sends no sign-in mail of its own
  # OpenRails is an OAuth resource server: it accepts the tokens its
  # merchants' trusted issuers mint for it.
  resource:
    id: http://localhost:3053        # the tokens' aud, and where DPoP proofs point

# Route groups beyond the public, customer (/v1/me) and webhook routes.
route_groups:
  admin: true        # customer support; the app's backend reads customers with merchant:customer:read
  catalog: true      # product and price reads (merchant:catalog:read)
  programmatic: true # /v1/app: the backend's entitlement checks (merchant:entitlements:read)

# Receipts and payment notices, sent from the merchant's from_email.
email_smtp:
  host: mailpit
  port: 1025
  from: "OnlyDemo Billing <billing@onlydemo.example>"
```

The merchant manifest declares each merchant: its name, the issuer whose tokens
act for it, its settings and its PSPs ([the merchant manifest](#the-merchant-manifest)).

```yaml guide:standalone-merchant
version: 1
merchants:
  onlydemo: # your merchant's name: lowercase letters, digits and hyphens
    display_name: OnlyDemo
    remote_application: # the app's AuthKit: its tokens act for this merchant only
      issuer: http://localhost:8080
      jwks_uri: http://localhost:8080/.well-known/jwks.json
    profile:
      from_email: billing@onlydemo.example # receipts come from this address
    psps:
      mobius: # your name for this account; any key you like
        rail: nmi
        environment: sandbox # live (real money; the default), test (an NMI account in test mode) or sandbox (a sandbox.nmi.com account)
        gateway_id: "000000" # the NMI dashboard's Gateway ID
        tokenization_key: your-public-tokenization-key # the browser's card fields use it
        secrets:
          security_key: your-private-security-key
          webhook_signing_secret: your-webhook-signing-key
```

Start it, then check readiness and the public API:

```sh
openrails run-server --config config.yaml --merchant-manifest merchant.yaml
curl --fail-with-body http://localhost:3053/health/ready
curl --fail-with-body http://localhost:3053/v1/config
```

The server migrates the database under an advisory lock, creates the merchants
the manifest names, arms each PSP whose credentials its provider confirms, and
starts the workers. It never applies a catalog: that is an operator step.

```sh
openrails catalog apply catalog.yaml --config config.yaml --merchant onlydemo
```

A private deployment's first run, as an init job (with people signing in at a
trusted issuer, `push-auth-bootstrap` has nothing to do):

```sh
openrails push-auth-bootstrap --file /etc/openrails/bootstrap.yaml
openrails push-merchant-config --file /etc/openrails/merchants.yaml --insert
openrails catalog apply /etc/openrails/catalog.yaml --merchant your-merchant
```

In production drop every `allow_*` key, use https for the issuer and resource,
mount a real signing key ([secrets](#secrets)) and set `trusted_proxies` behind a
proxy. Boot refuses an unset `provider_write_mode`, an unknown key and a retired
one (naming its replacement).

## Configuration sources

Each key of `config.yaml` can come from four places; the later wins:

1. `config.yaml` (`--config`/`-c`, else `OPENRAILS_CONFIG`, else `config.yaml`
   in the working directory or `config/`).
2. Mounted secret files: a file named like the key's environment variable under
   `/vault/secrets` (`VAULT_SECRETS_PATH` names another directory). Vault Agent,
   a Kubernetes Secret or CSI renders them.
3. The environment: the key's path upper-cased and joined by `_` (`db.url` is
   `DB_URL`, `email_smtp.password` is `EMAIL_SMTP_PASSWORD`). A list is
   comma-separated or a JSON array; a section may be a JSON object. A `.env` in
   the working directory is loaded too, and the server logs that it did.
4. Flags: `--provider-write-mode` on every command, `--merchant-manifest` on
   `run-server` and `run-worker`.

Exceptions to the mechanical name: `VAULT_ADDR` is `vault.address`;
`AUTHKIT_ACTIVE_KEY_ID`, `AUTHKIT_ACTIVE_PRIVATE_KEY_PEM` and
`AUTHKIT_PUBLIC_KEYS` are `signing_key.*`; `AUTH_<PATH>` is `auth.<path>` by
AuthKit's keys (`AUTH_TOKEN_ISSUER`); `NAMING_FORMER_NAMES_MODE` and
`NAMING_FORMER_NAMES_DURATION` are `naming.former_names.*`. Inside a section a
key matches its field name with or without underscores (`db.sql_trace`).

Three inputs configure a deployment, each on its own lifecycle:

| Input | Owns | Read |
|---|---|---|
| `config.yaml` and its sources | the process: database, Redis, write mode, Vault, route groups, the server's AuthKit | at boot |
| the merchant manifest, or Vault | each merchant: name, settings, alert webhooks, PSPs and their secrets | at every boot (file), or live (Vault) |
| catalog documents | products, prices, entitlements, limits, provider links | only when an operator applies one ([catalog](client.md#catalog)) |

The process configuration never decides whether money is real: each PSP's
declared environment does ([merchant configuration](client.md#merchant-configuration-and-psps)).

## Configuration reference

[`config.example.yaml`](../config.example.yaml) is an annotated file. Durations
are Go durations (`90s`, `24h`). Every key below is read at boot. The top-level
keys: `host`, `port`, `private_port`, `drain_delay`, `shutdown_timeout`,
`logger`, `provider_write_mode`, `engine_admission_hold`,
`usage_billing_enabled`, `event_retention`, `webhook_secret_overlap`,
`merchant_manifest_overlays`, `route_groups`, `public_billing_base_url`,
`checkout_origin`, `return_origins`, `trusted_proxies`,
`cloudflare_proxies`, `db`, `database`, `clickhouse`, `redis`,
`auth`, `signing_key`, `local_sign_in`, `naming`, `admin_console`,
`email_smtp`, `vault`, `card_vault`, `solana`, `hyperswitch`, `rate_limits`,
`rate_limits_disabled`, `captcha`, `user_action_rate_limit`,
`destructive_action_limits`, `llm` and `provider_sandbox`.

**Listener and lifecycle**

| Key | Default | Meaning |
|---|---|---|
| `host`, `port` | `0.0.0.0`, `3053` | the public listener: API, console, payment page, webhooks |
| `private_port` | unset | the private listener for `/metrics`; unset serves none |
| `drain_delay` | `5s` | on SIGTERM readiness fails at once and serving continues this long |
| `shutdown_timeout` | `20s` | then in-flight requests and workers finish within this |
| `logger.level` | `info` | `debug`, `info`, `warn` or `error` |
| `logger.format` | `json` | `json` or `text`; logs go to stderr |

**Billing**

| Key | Default | Meaning |
|---|---|---|
| `provider_write_mode` | required | `full`, `limited` or `readonly` ([write safety](#write-safety-and-the-kill-switch)) |
| `engine_admission_hold` | `false` | pause admission and first submission of new engine payments; submitted ones are still verified |
| `usage_billing_enabled` | `false` | turn on automatic usage orders on credit ([usage billing](client.md)) |
| `event_retention` | `2160h` | how long the event log keeps facts, whatever readers have read |
| `webhook_secret_overlap` | `24h` | how long a rotated-out webhook signing secret keeps verifying, at most `168h` |
| `merchant_manifest_overlays` | none | secret overlays merged over the merchant manifest ([the merchant manifest](#the-merchant-manifest)) |
| `route_groups.admin` | off | customer support: `/v1/admin` |
| `route_groups.catalog` | off | catalog reads and edits: `/v1/catalog` |
| `route_groups.merchant_config` | off | PSPs, settings, findings, imports and archives: `/v1/merchant` |
| `route_groups.metrics` | off | business metrics: `/v1/metrics` |
| `route_groups.programmatic` | off | your backend's `/v1/app` calls |

Public, customer (`/v1/me`), payment and webhook routes are always on. A route
the deployment does not serve answers `404 route_not_found`.

**URLs and origins**

| Key | Meaning |
|---|---|
| `public_billing_base_url` | where browsers and PSPs reach the server, without `/v1`. Provider callbacks and billing links are built on it; Stripe webhook registration needs it public https |
| `checkout_origin` | the payment page's own origin ([the payment page](#the-payment-page)); unset issues no pay URLs |
| `return_origins` | exact origins a purchase's success, cancel and billing-portal return URLs may name; empty allows `public_billing_base_url`'s origin only |
| `trusted_proxies` | CIDRs whose `X-Forwarded-For` is walked; empty trusts none ([proxies](#tls-proxies-cors-and-webhooks)) |
| `cloudflare_proxies` | Cloudflare's ranges where Cloudflare fronts the origin: trusted like `trusted_proxies`, and the only peers whose `CF-Connecting-IP` AuthKit takes |

**Database**

| Key | Default | Meaning |
|---|---|---|
| `db.url` | — | the connection string (`DB_URL`); overrides the parts below |
| `db.host`, `db.port`, `db.database`, `db.username`, `db.password` | — | the parts, when `db.url` is unset; all four of host, port, database and username or boot refuses |
| `db.sslmode` | `verify-full`; `require` for a loopback host or Unix socket | libpq's `sslmode` for the parts |
| `db.sslrootcert` | the system roots | PEM file of the CAs that verify the database's certificate, for the parts |
| `db.sql_trace` | `false` | log every query at debug level |
| `db.lease_conns` | `4` | the separate pool for idempotency lease renewals; count it in `max_connections` |
| `database.schema` | `billing` | every OpenRails table, function and type; a plain identifier |
| `database.river_schema` | the schema plus `_river` | OpenRails' River jobs |
| `database.skip_migrations` | `false` | only check the migration ledger at boot; run `openrails migrate up` yourself |

SQL is authored in `billing`; another schema is reached by one token-aware
rewrite of schema qualifiers, never other strings. The server's AuthKit keeps its
own schema (`auth.database.schema`, default `profiles`).

**Event store**

| Key | Meaning |
|---|---|
| `clickhouse.addr` | `host:port` of ClickHouse's native protocol. Set, the server waits for it at boot like the database; unset, usage is off |
| `clickhouse.database` | the event store's database, created or upgraded at boot; default `openrails` |
| `clickhouse.username`, `clickhouse.password` | the user (default `default`) and its password |

**Redis**

| Key | Meaning |
|---|---|
| `redis.addr` | `host:port`. While a declared Redis does not answer, a DPoP proof or a JWT-bearer or client assertion is refused (503): another replica could not see it spent |
| `redis.url` | instead of `addr`: `redis://` or `rediss://` with user, password and database |
| `redis.username`, `redis.password`, `redis.db` | ACL user, password, database number |
| `redis.tls`, `redis.ca_cert` | TLS against the system roots, or this PEM bundle (which implies TLS) |

A Redis section that sets nothing declares none. One with invalid settings is
turned off and reported (`openrails_configuration`); one that stops answering
costs sharing, not service ([scaling](#scaling-and-redis)).

**The server's AuthKit**

`auth` is AuthKit's own configuration in AuthKit's keys; an unknown key refuses
boot. The ones a deployment sets:

| Key | Meaning |
|---|---|
| `auth.default_tenant_id` | the server's operator account directory: a UUID generated once for the deployment and never changed ([the account directory](#the-account-directory)) |
| `auth.token.issuer` | required; the server's own issuer, https (http only on a loopback host). No billing URL stands in for it |
| `auth.token.refresh_token_duration` | how long a session lasts from its sign-in: 30 days when unset; negative lasts until revoked |
| `auth.token.allow_private_network_jwks` | fetch a trusted issuer's keys from a private address (local federation only) |
| `auth.keys.path` | the directory holding `keys.json` and `totp.key` (`AUTH_KEYS_PATH`), neither group- nor world-writable; file-watched |
| `auth.keys.allow_ephemeral_dev_keys` | generate keys under `auth.keys.path`; one instance, local only |
| `auth.keys.verify_only` | verify tokens, mint none |
| `auth.resource.id` | the resource trusted issuers mint tokens for: their `aud` (RFC 8707). Turns trusted-issuer tokens on |
| `auth.resource.public_url` | the public origin DPoP proofs point at |
| `auth.registration.native_user_mode` | local enrollment: `invite_only` by default, `closed` turns it off; `open` is refused unless a hosted product supplies `server.Config.MerchantCreation` |
| `auth.registration.allow_missing_senders` | run without an email or SMS sender |
| `auth.sign_in.dpop` | `optional` or `required` for the server's own users |
| `auth.http.direct_peer_ip` | optional: AuthKit's client IP follows `trusted_proxies` and `cloudflare_proxies`, the socket peer when both are empty; `true` beside either refuses boot |
| `auth.http.rate_limits` | AuthKit's own buckets |
| `auth.database.schema` | AuthKit's tables, default `profiles` |
| `signing_key.active_key_id`, `signing_key.active_private_key_pem`, `signing_key.public_keys` | an inline signing key from `AUTHKIT_ACTIVE_KEY_ID`, `AUTHKIT_ACTIVE_PRIVATE_KEY_PEM` and `AUTHKIT_PUBLIC_KEYS` (a JSON object of further PEM keys by kid); fixed until restart, where `auth.keys.path` rotates live |
| `local_sign_in` | serve sign-in to the server's own accounts; off, people sign in at a trusted issuer |
| `naming.enabled`, `naming.rename_interval` | merchant renames on (default) and how often one merchant may rename (`72h`; `0s` at once) |
| `naming.former_names.mode`, `naming.former_names.duration` | how long a former name forwards: `finite` for the duration (`2160h`), `forever`, or `immediate` |

The server fixes the rest: merchant roles, the API-key prefix, the scopes
`openrails:merchant` and `openrails:self`, AuthKit's River schema and mount. The
root owner needs a second factor, so boot refuses without a 16, 24 or 32-byte
`totp.key` or an email or SMS sender.

**The admin console**

| Key | Meaning |
|---|---|
| `admin_console.disabled` | omit the console at `/admin` (an API-only server) |
| `admin_console.issuers` | sign-in choices, empty by default |
| `admin_console.issuers.<merchant>.url` | a trusted issuer staff sign in at, keyed by a merchant id or name, or `root` for operators |
| `admin_console.issuers.<merchant>.client_id` | the console's public PKCE client there, with redirect URI `<origin>/admin/callback` |
| `admin_console.issuers.<merchant>.name`, `.scope` | the button's label; the scope asked for (add `offline_access` where the issuer needs it for refresh tokens) |

**Mail**

| Key | Meaning |
|---|---|
| `email_smtp.host`, `email_smtp.port` | any SMTP server; port 465 is implicit TLS, others STARTTLS. An empty host sends no mail |
| `email_smtp.username`, `email_smtp.password` | credentials (`EMAIL_SMTP_PASSWORD` from a secret) |
| `email_smtp.from` | the deployment's own mailbox (`Billing <billing@example.com>`); billing mail uses the merchant's `profile.from_email` when set |

**Vault, card vault and Solana**

| Key | Meaning |
|---|---|
| `vault.address` | the server (`VAULT_ADDR`); declaring any `vault` key connects |
| `vault.kv_mount` | a KV v2 mount: Vault holds merchant configuration ([Vault](#vault)) |
| `vault.scope_prefix` | the path under the mount, default `openrails` |
| `vault.transit_mount` | the Transit mount for Solana crankers, default `transit` |
| `vault.namespace` | a Vault Enterprise namespace |
| `vault.auth_method` | `kubernetes`, `approle` or `token` |
| `vault.k8s_role` | the kubernetes auth role |
| `vault.role_id`, `vault.secret_id` | AppRole credentials |
| `vault.secrets_path` | a directory whose `VAULT_SECRET_ID` file is re-read at every re-authentication |
| `vault.token` | a token (development, or one a sidecar manages; `VAULT_TOKEN`) |
| `vault.auth_mount` | where the auth method is mounted, without `auth/`; default its name |
| `card_vault.kind`, `card_vault.tenant_id` | the operator's platform card vault: `basis_theory` and its tenant |
| `card_vault.kv_mount` | where its credential document lives (`platform_vault/<tenant_id>`); default `vault.kv_mount`. Setting only this keeps merchant configuration in files |
| `solana.crankers` | the deployment's Solana crankers, in order: `{mode: vault_transit, key}` (the key never leaves Transit) or `{mode: local_keypair, private_key}`. The first publishes new plans; each pulls the plans it owns; none turns Solana rebilling off. `SOLANA_CRANKERS` takes the list as JSON |
| `hyperswitch.api_base_url`, `hyperswitch.sdk_url` | a deployment's HyperSwitch custodian endpoints |
| `hyperswitch.allow_loopback_http` | admit a loopback http fixture; refused beside a live PSP |

**Abuse limits and staff limits**

| Key | Default | Meaning |
|---|---|---|
| `rate_limits.<bucket>.requests_per_minute` | see [abuse limits](#abuse-limits) | per-minute limits of the `purchase`, `subscribe`, `payment`, `webhook` and `catalog-ask` buckets; a bucket left out is unlimited, an unknown key is reported and ignored, 0 or less means 60 |
| `rate_limits_disabled` | `false` | make the middleware a passthrough where your gateway limits instead |
| `captcha.provider`, `captcha.site_key`, `captcha.secret_key` | `turnstile` | both keys set turn captcha on; `turnstile`, `recaptcha-v3` or `hcaptcha` |
| `user_action_rate_limit.minimum_customers_per_hour`, `.paid_customers_per_hourly_customer` | 5, 400 | the merchant-wide hourly budget for customer cancellations and card removals |
| `user_action_rate_limit.minimum_customers_per_day`, `.paid_customers_per_daily_customer` | 10, 100 | the daily one |
| `destructive_action_limits.actor_per_hour`, `.actor_per_day` | 5, 15 | staff destructive actions per actor ([destructive holds](#destructive-holds)) |
| `destructive_action_limits.merchant_per_hour`, `.merchant_per_day` | 15, 25 | per merchant, floors that rise with active customers |
| `destructive_action_limits.deployment_per_hour`, `.deployment_per_day` | 50, 100 | per deployment, likewise |
| `destructive_action_limits.erasure_per_hour`, `.erasure_per_day` | 100, 1000 | contact erasures over OpenRails' own SCIM routes per merchant, floors that rise with its directory; past them an erasure waits ([client](client.md#customers)) |

The example file sets the shipped buckets explicitly:

```yaml
rate_limits:
  subscribe: {requests_per_minute: 20}
  purchase: {requests_per_minute: 10}
  webhook: {requests_per_minute: 1200}
  payment: {requests_per_minute: 40}
user_action_rate_limit:
  minimum_customers_per_hour: 5
  paid_customers_per_hourly_customer: 400
  minimum_customers_per_day: 10
  paid_customers_per_daily_customer: 100
```

**LLM**

| Key | Meaning |
|---|---|
| `llm.provider`, `llm.model` | `anthropic` (default) or `openai`, and a model; each provider has a default |
| `llm.api_key` | a secret (`LLM_API_KEY`); without it the catalog copilot is not mounted |
| `llm.base_url` | an endpoint override; with `openai`, any compatible backend, including `/v1` |
| `llm.catalog_copilot_enabled` | consent to send aggregate catalog and subscriber counts to the provider for `POST /v1/console/catalog/ask` |
| `llm.catalog_drafting_enabled` | separate consent for the copilot's draft tools |

Metrics and dashboards use no LLM.

**Provider sandbox (tests only)**

| Key | Meaning |
|---|---|
| `provider_sandbox.nmi_gateway_url`, `.stripe_api_url`, `.solana_rpc_url`, `.ccbill_datalink_url` | loopback fakes the rails talk to instead of the providers. The host must be a loopback IP literal and every PSP non-live; anything else refuses boot |
| `provider_sandbox.ccbill_webhook_sources` | extra CCBill webhook source CIDRs, honored only for a webhook naming a sandbox CCBill account; `/0` is refused |

`openrails sandbox nmi-gateway` serves such an NMI fake.

## The merchant manifest

The manifest is `version: 1` and `merchants.<name>`, each entry a merchant
document ([its fields and every rail's PSP](client.md#merchant-configuration-and-psps))
plus, on the server, `remote_application`: the app's issuer (`issuer` with
`jwks_uri`, inline `jwks` or `public_keys`), a trusted issuer in the merchant's
group holding its owner role ([trusted issuers](#trusted-issuers)).

`run-server` and `run-worker` read it at every boot: `--merchant-manifest PATH`
(an explicit path must exist), else `/etc/openrails/merchants.yaml` when present;
none boots control-plane-only. At boot:

1. The manifest and its overlays merge (later wins) and parse strictly: unknown
   fields, retired keys (`accounts`, `rail_merchant_accounts`,
   `provider_accounts`, which name their rename), several documents, aliases,
   anchors, merge keys and tags refuse boot, and so do secrets for an undeclared
   PSP.
2. Missing merchants are created and each PSP's and custodian's identity is
   recorded; PostgreSQL keeps only those identities, never configuration.
3. Each document validates as the configuration API would, and a declared
   secret that resolves empty is an error.
4. Each PSP's environment is verified with its provider in the background
   before it arms; the server never waits on a provider to start.

Without Vault the file is the configuration, held in memory with its
credentials: the configuration routes read it (never credentials) and the edit
routes are not mounted. Change it by editing the file and restarting.
`openrails get-merchant-config` and `apply-merchant-config` read and apply
metadata by revision where Vault holds it; `push-merchant-config --insert`
creates the merchants a manifest names; `dump-merchant-config --slug` prints a
merchant's configuration in the manifest's shape, without credentials.

**Secret overlays.** Keep secret values out of the committed manifest. Render
them as YAML in the manifest's shape, holding only `secrets` blocks, and list the
files in `merchant_manifest_overlays` (`MERCHANT_MANIFEST_OVERLAYS`,
comma-separated). An overlay that changes anything but secret values is refused:
the manifest stays the authority for identities, account ids and settings.

```yaml
# /vault/secrets/merchants.yaml
merchants:
  onlydemo:
    psps:
      mobius:
        secrets: {security_key: replace-me, webhook_signing_secret: replace-me}
```

To rotate a credential, update the secret source, re-render and restart.

## Secrets

Nothing in `config.yaml` needs to be secret: every secret can come from a
mounted file or the environment, named like its key's variable.

| Secret | Where it goes |
|---|---|
| database URL | `DB_URL` (with `sslmode=verify-full` across a network) |
| Redis | `REDIS_URL` or `REDIS_PASSWORD`, `REDIS_CA_CERT` |
| ClickHouse | `CLICKHOUSE_PASSWORD` |
| SMTP | `EMAIL_SMTP_PASSWORD` |
| Vault | `VAULT_TOKEN`, or `VAULT_SECRET_ID` re-read from `vault.secrets_path` |
| LLM | `LLM_API_KEY` |
| Solana local keypairs | `SOLANA_CRANKERS` |
| AuthKit's signing key and TOTP key | `keys.json` and `totp.key` under `auth.keys.path`, or the `AUTHKIT_*` variables |
| PSP and custodian credentials | Vault, or a merchant manifest overlay |

`keys.json` is `{active_key_id, active_private_key_pem, public_keys}`; the file
is watched, so rotate by adding the new key as active, moving the old one into
`public_keys` and replacing the file. Never rotate `totp.key`: enrolled
authenticators depend on it.

```sh
openssl genrsa -traditional -out signing.pem 2048
jq -n --arg pem "$(cat signing.pem)" \
  '{active_key_id: "2026-11", active_private_key_pem: $pem, public_keys: {}}' > keys.json
openssl rand -base64 32 > totp.key
```

## Vault

`vault.kv_mount` makes Vault the only merchant configuration source. Staff then
edit configuration over HTTP (`PATCH /v1/merchant/configuration`, the
`/v1/merchant/psps` and `/v1/merchant/alert-webhooks` routes) or in the console,
starting from an empty configuration, and automation may write the documents
directly. A manifest that declares configuration beside Vault is refused at
startup, naming what it declares; it names the merchant only (its slug and
`remote_application`). There is no import or seeding step.

| Capability | Setting |
|---|---|
| merchant configuration, credentials included | `vault.kv_mount` (KV v2) |
| the operator's platform card vault credentials | `card_vault.kv_mount`, default `vault.kv_mount` |
| Solana `vault_transit` crankers: Vault signs, the key never leaves | `vault.transit_mount`, default `transit` |

**Document paths**, one JSON document per object under the merchant's id
(`openrails merchants get <name>` prints it; a rename never moves it):

```
<kv_mount>/<scope_prefix>/merchants/<merchant-id>/merchant
<kv_mount>/<scope_prefix>/merchants/<merchant-id>/psps/<key>
<kv_mount>/<scope_prefix>/merchants/<merchant-id>/custodians/<key>
<kv_mount>/<scope_prefix>/merchants/<merchant-id>/messages
<kv_mount>/<scope_prefix>/credential_fingerprint_key
<card_vault.kv_mount>/<scope_prefix>/platform_vault/<tenant_id>
```

Documents use the manifest's spelling, flat, with credentials under `secrets`;
unknown fields are refused. A key matches `^[a-z0-9][a-z0-9_-]{0,62}$`.
`credential_fingerprint_key` is created on first start and keys the fingerprints
that catch one gateway account declared twice: back it up with Vault.

```sh
id=$(openrails merchants get shop | jq -r .id)
vault kv put -mount=kv openrails/merchants/$id/psps/mobius @mobius.json
```

**Revisions.** A document's KV version is its revision. Edits through OpenRails
write with check-and-set, and `expected_revision` refuses an object changed
since (`409 revision_mismatch`). An edit through OpenRails reaches every replica
at once (PostgreSQL `NOTIFY`); one made in Vault directly within 30 seconds of the
merchant's next use. A document that fails validation is not served: the
version before it keeps serving, the `openrails_configuration` probe names it and
a `consistency.merchant_config.not_served` finding stays open. Credentials are
written, never read back over the API.

**Availability.** The server logs in to Vault in the background; startup waits
up to 5 seconds for Vault to answer, then serves either way. Until it answers,
merchant configuration is unavailable: reads and edits are refused (PSP, card and
webhook routes answer `503 service_unavailable`) and Solana Transit signing fails.
`/health/ready` stays ready; `openrails_dependency_up{dependency="vault_login"}`
and `{dependency="configuration"}` are 0, and the log names
`subject=vault.merchant_configuration`. Login retries with capped backoff, and
configuration serves once Vault answers, without a restart. A malformed `vault`
section (its login settings, an unreadable `vault.ca_cert`) is not retried: it is
refused as `subject=vault.login` and Vault stays unused until it is fixed.

Once loaded, an outage is ridden out from each replica's cache: purchase,
renewals and webhooks keep working for every merchant already loaded; a merchant
first used during the outage fails until Vault returns.

**Authenticating.** OpenRails authenticates once, as itself; merchant isolation
is the path. A supervisor logs in, renews and re-authenticates when renewal is
no longer possible; the token lives in memory only. In a cluster prefer
`kubernetes`: the pod's ServiceAccount is the credential.

```yaml
vault:
  address: https://vault.internal:8200
  kv_mount: kv
  auth_method: kubernetes
  k8s_role: openrails
```

**Minimal policies.**

```hcl
# merchant configuration (kv_mount: kv)
path "kv/data/openrails/*"     { capabilities = ["create", "read", "update", "delete"] }
path "kv/metadata/openrails/*" { capabilities = ["read", "delete", "list"] }
# Transit signing, scoped to the crankers' keys
path "transit/sign/openrails-solana-cranker" { capabilities = ["update"] }
path "transit/keys/openrails-solana-cranker" { capabilities = ["read"] }
```

A read-only token serves an existing configuration and refuses edits. A
deployment using only platform credentials needs read access to that one
document.

**Solana Transit keys.** The operator creates the key; OpenRails never creates
or exports it, signs at `transit/sign/<key>` and reads the cranker's address
from `transit/keys/<key>`. A cranker is pinned to the public key it first had,
so rotate by creating a new key and listing it first, never with
`transit/keys/<key>/rotate`.

```sh
vault write -f transit/keys/openrails-solana-cranker type=ed25519 exportable=false
```

**Purge.** `openrails merchant purge` queues the `openrails.merchant_secret_cleanup`
job, which deletes every document under the merchant's id and retries until the
subtree is empty; a changed Vault address or mount is refused until restored.

## Auth and trust

The server runs its own AuthKit (the `auth` section, `server.Config.Auth`). It
decides who every request is: the server's own accounts and API keys, and the
access tokens trusted issuers mint for `auth.resource.id`. OpenRails then binds
the credential to one merchant and checks the route's permission; nothing in a
request selects the merchant, and a valid credential bound to none is
`403 merchant_unresolved` ([credentials](client.md#credentials)).

### The account directory

`auth.default_tenant_id` (`AUTH_DEFAULT_TENANT_ID`) is the server's own operator
account directory: generate a UUID once for the deployment, keep it in
configuration and never change it. It is never inferred. `openrails migrate up`
migrates OpenRails and AuthKit and passes it as AuthKit's adoption target for
existing accounts, keeping their ids: review that mapping before migrating a
populated database.

### Trusted issuers

With `auth.resource.id` set, the server accepts RFC 9068 access tokens
(`typ: at+jwt`) minted for that resource by its trusted issuers: AuthKit remote
applications in a merchant's group, from the manifest's `remote_application`
(holding the merchant's owner role) or registered at run time. One issuer acts
for one merchant. A token's permissions count within its application's role and
its scopes:

| Scope | Grants |
|---|---|
| `openrails:merchant` | merchant permissions, within the application's role: your backend |
| `openrails:self` | none: a customer's own billing at `/v1/me` |

When your app runs AuthKit, it is the authorization server: it registers the
server as a resource, a public client for the browser's token exchange and a
confidential client for the backend's client credentials. In the example's app:

```go guide:standalone-issuer
// OpenRails is a resource server this AuthKit mints tokens for.
AuthorizationServer: authkit.AuthorizationServerConfig{
	Resources: []authkit.ResourceServerConfig{{
		ID: s.OpenRailsURL, // the tokens' aud
		// A customer's own billing; the merchant API.
		Scopes: []string{"openrails:self", "openrails:merchant"},
		// The most a token may carry.
		Permissions: backendPermissions,
		// The user's email in every token: a receipt reaches a brand-new buyer.
		ContactClaims: true,
	}},
	Clients: []authkit.OAuthClientConfig{{
		ID:         webClient, // public: the browser proves a DPoP key instead of a secret
		Origins:    []string{s.PublicURL},
		Resources:  []string{s.OpenRailsURL},
		GrantTypes: []authkit.OAuthGrantType{authkit.GrantTokenExchange},
	}, {
		ID:           backendClient, // confidential: this server and the SCIM pushes
		SecretSHA256: hex.EncodeToString(secret[:]),
		Resources:    []string{s.OpenRailsURL},
		Permissions:  backendPermissions,
		GrantTypes:   []authkit.OAuthGrantType{authkit.GrantClientCredentials},
	}},
},
```

A token whose `sub` equals its `client_id` is a client acting for itself. An
application without an authorization server mints customer tokens at the
server's token endpoint with an RFC 7523 assertion signed by its registered key
([customer tokens](client.md#credentials)). Verification, role mapping and DPoP
are AuthKit's. OpenRails cannot see an issuer's bans or revocations, so the
token's lifetime bounds the exposure: constrain each client's permissions at the
issuer. A customer is its issuer's subject: `billing.customers` records the
issuer, and another issuer's credential with the same `sub` is
`403 customer_issuer_mismatch`.

### API keys and roles

A merchant's scope is its AuthKit group, whose id is the merchant's
(`Server.MerchantScope`). Team members and API keys hold a declared role
(`server.MerchantRoles`: `viewer`, `support`, `owner`) or a custom one holding
any `merchant:` permissions; managing custom roles takes
`server.MerchantRolesManage` and reading them `server.MerchantRolesRead`, and
an editor grants only what they hold. Checks read roles live: deleting a role
revokes the keys and invitations carrying it. A key's secret is shown once; its
prefix `openrails_st_<key_id>` identifies it. Grant a backend only what it needs:

| Purpose | Permissions |
|---|---|
| read or change the catalog | `merchant:catalog:read`, `merchant:catalog:manage` |
| read or change configuration and archives | `merchant:config:read`, `merchant:config:manage` |
| admit and report usage, limits and running charges (a gate needs both) | `merchant:usage:read`, `merchant:usage:manage` |
| check entitlements | `merchant:entitlements:read` |
| create orders | `merchant:orders:manage` |
| read the event log | `merchant:events:read` |
| read or act on customers | `merchant:customer:read`, `merchant:customer:manage` |

Root operators hold the `root:` names, plus `billing.RootMerchantsRead`,
`billing.RootMerchantsDelete`, `billing.RootMerchantsRestore` and
`billing.RootWorkerHealthRead` for the server's merchant directory and worker
health. A person's token is
refused on `/v1/app` (`403 application_required`).

### The console

`/admin` is the staff console. Staff sign in at an app's issuer
(`admin_console.issuers`, one choice per merchant or `root`) or, with
`local_sign_in`, with the server's own accounts. The console exchanges the
person's token for one bound to the chosen merchant (RFC 8693 through the
server's `openrails-console` client), refused where they hold no role. Its
session keeps the access token in memory and a rotating HttpOnly refresh cookie (`__Host-authkit_rt` on HTTPS) that ends 30 days after sign-in (`auth.token.refresh_token_duration`); a revoked session answers `401 credential_revoked` at once. `admin_console.disabled` omits it.

### Customer contacts

The server mounts no SCIM routes. A customer of the server's own accounts is
read from them; a trusted issuer's customer from the merchant's directory in the
server's AuthKit, which the issuer's AuthKit pushes to over SCIM at
`/directory/scim/v2` with its client credentials, and from the verified contact
claims of its tokens. Each customer is read from its own issuer's directory,
newest write winning. OpenRails emails a customer only at an address the issuer
proved.

## The payment page

An order's pay URL is a page on an origin of its own, `/pay/<order>#<secret>`,
where the customer pays that one order. The server serves it at
`checkout_origin` (the engine's `Config.CheckoutOrigin`), answering by `Host`: point
that origin's DNS and TLS at the same server. The checkout origin has no path
and must differ from `public_billing_base_url`, the server's issuer and the
frontend's origin: the page runs the PSPs' scripts and never shares an origin
with the API or the console. Boot refuses otherwise. Without it an order asking
for a pay URL is `400 pay_url_unavailable`.

The page is built from `web/pay` and embedded in the release binary and image.
A host building its own binary on `server` builds it with
`scripts/build-payment-page.sh <dir>` and passes it as the engine's
`Deps.CheckoutAssets`; one that mounts the server's routes on its own
router sends the checkout origin's requests to `Server.CheckoutPage`. The
checkout origin also serves Stripe's Apple Pay domain-association file at
`/.well-known/apple-developer-merchantid-domain-association`, and declaring a
Stripe PSP registers the origin as a payment-method domain on its account.

## TLS, proxies, CORS and webhooks

TLS ends at your proxy or ingress. Every path goes to the server: the API, the
console at `/admin`, `/v1/webhooks/{rail}/{account_id}` for provider webhooks and
the checkout origin. Give one public origin to `public_billing_base_url`,
`auth.token.issuer` and `auth.resource.id`, and another to `checkout_origin`.

- **Client IP.** The client is the socket peer unless `trusted_proxies` lists the proxy's CIDRs; then `X-Forwarded-For` is walked right to left past trusted hops. Without it rate limits, abuse tracking, request logs and webhook source addresses all see the proxy. AuthKit takes the same proxies; `auth.http` declares none of its own. Behind Cloudflare, list its ranges in `cloudflare_proxies` and lock the origin to Cloudflare's ingress.
- **Body size.** Requests are capped at 1 MiB (`413 request_body_too_large`),
  but a billing archive import (`POST /v1/merchant/billing-archive`) streams up
  to 1 GiB: raise the proxy's limit if you import over HTTP.
- **CORS** is fixed: the API answers any origin without credentials
  (`Access-Control-Allow-Origin: *`, `WWW-Authenticate` exposed, 12 h max-age);
  the console, webhooks and the server's AuthKit routes send no CORS headers.
  OpenRails sets no cookies on the API.
- **Webhooks.** Each PSP posts to `/v1/webhooks/{rail}/{account_id}`: the rail
  (`nmi`, `stripe`, `ccbill`, `basistheory`), never a PSP key, and the PSP's
  account id. The account resolves the merchant and its own secret verifies the
  request. NMI and Stripe sign; CCBill signs nothing and is accepted only from
  its published ranges (plus `provider_sandbox.ccbill_webhook_sources` for
  sandbox accounts). Stripe endpoints are registered automatically once
  `public_billing_base_url` is public https and provider writes are allowed.
  Per-rail setup: [PSPs](client.md#merchant-configuration-and-psps).

One API host serves every merchant. `billing.ReservedMerchantSlugs` lists names a
hosted product should not let merchants claim.

### Network egress

Besides the hosts you configure (database, Redis, Vault, SMTP, trusted issuers'
JWKS, `llm.base_url`):

| Destination | For |
|---|---|
| `latest.currency-api.pages.dev`, then `cdn.jsdelivr.net` | FX rates, every 2 hours from an instance's first cross-currency quote; a failure fails only that quote |
| `hermes.pyth.network` | Solana token prices |
| `api.stripe.com`; `secure.nmi.com`, `secure.networkmerchants.com`, `sandbox.nmi.com`; `api.ccbill.com`, `sandbox-api.ccbill.com`, `datalink.ccbill.com` | PSPs |
| `api.basistheory.com`, `cdn.basistheory.com` | Basis Theory custodians |
| `api.mainnet-beta.solana.com`, `api.devnet.solana.com`, a PSP's `rpc_url` | Solana RPC |
| `challenges.cloudflare.com`, `www.google.com`, `hcaptcha.com` | captcha, when set |
| `api.anthropic.com`, `api.openai.com` | the catalog copilot, with `llm.api_key` and no `llm.base_url` |

## Event store

Usage needs an event store on ClickHouse; money stays in PostgreSQL. Every usage item is a raw event there (kept 100 days), with the rollups limits, balances and history read (limit windows, pricing periods, minutes, and days kept 25 months). PostgreSQL keeps what the ledger took (`usage_postings`), who has usage it has not taken yet (`usage_unposted`) and the rate cards usage was priced under (`usage_card_prints`). Usage posts at each month's end in the merchant's `accounting.timezone` (UTC by default), in the billing pass (`openrails.usage_close`), and before any other movement of a customer's balance, so up to a month of usage is only in ClickHouse: back it up nightly ([backup and restore](#backup-and-restore)).

Without a `clickhouse` section the server runs subscriptions, purchases, checkout, entitlements, credit packs and balances, running charges, usage orders, customer messages and the books; the usage routes stay mounted and answer 409 `event_store_required`, catalog documents and edits declaring meters, limits, a product's `limits` or `rate_cards` are refused, and so are the settings only usage reads (spend windows, failed-usage grace and cutoffs, the accrual-rate cap). Meters and limits already stored stay dormant: they are named in one log line at boot and in a `consistency.dormant_usage` finding, and the next catalog document applied removes them. A database with usage the ledger has not taken does not start without ClickHouse (`event_store_required`, naming the merchants): only the store can post it.

The first boot with ClickHouse records the store's identity in PostgreSQL. A later boot against a ClickHouse with another identity, an empty or different one, is refused: restore the event store's backup instead. Where none exists, `openrails usage attach --actor NAME --reason TEXT` records the configured ClickHouse as the store; with usage unposted it also needs `--discard-unposted`, which deletes it and records each merchant's discard as a `usage_discard` maintenance run. While ClickHouse is out of reach, syncs and admissions answer 503 `service_unavailable` (gates spend their offline share), and the balances of customers with unposted usage cannot be read or moved; others are unaffected.

## Scaling and Redis

Any number of instances can serve one book: `openrails run-server` replicas,
plus `run-worker` processes if you want workers apart. There is no instance
count, primary or sticky session.

- **Shared through PostgreSQL:** every record, River's queue and leader
  election, idempotency claims, webhook deduplication and charge admission.
- **Shared through Redis:** the abuse state above. A declared Redis that stops
  answering costs sharing, not service: each instance falls back to memory,
  logs, counts `openrails_abuse_state_fallbacks_total` and reports Redis
  degraded; what it counted meanwhile still counts when Redis returns. Use a
  trusted Redis 6.2+ with synchronized clocks across failover nodes.
- **Identical everywhere:** schemas, `provider_write_mode`, merchant
  configuration (the same manifest and overlays, or Vault mount),
  `rate_limits`, `captcha`, `trusted_proxies`, `return_origins`, `route_groups`
  (a route some instances lack is a 404 on them), the signing key and `totp.key`
  (`auth.keys.allow_ephemeral_dev_keys` is one instance only), the captcha
  secret, and the version outside a rolling upgrade.
- **Background work:** every instance with workers takes River jobs, each job on
  one instance. River's leader alone schedules periodic work; a crashed leader
  is replaced within about 20 seconds. Each instance works up to 20 billing
  jobs at once, plus the `psp_import` and `provider_refresh` queues. Outside
  River every instance runs the Solana Pay poller (leased claims) and checks its
  PSP credentials at start, on change and before a write once its verdict is an
  hour old.
- **Caches:** catalog, merchants, write posture, the kill switch and access are
  read per request. Merchant configuration is cached per instance (see
  [Vault](#vault)). An FX rate published more than 48 hours ago is never quoted.
- **Connections:** size PostgreSQL `max_connections` for every instance's pool,
  its `db.lease_conns` lease pool, and one connection per instance for each listener: customer messages, dispute responses and, with Vault configuration, configuration edits. `LISTEN` needs a session, so a pooler in front of the database must run in session mode, not transaction mode.

A webhook takes effect once per provider event: a concurrent copy waits up to
10 seconds, then answers 500 so the provider retries. An idempotent request runs
once per key; a retry elsewhere replays or answers 409.

### One billing database

All instances serving a book share one PostgreSQL database with one writable
primary. Independently writable copies using the same PSP accounts are
unsupported: each would take its own locks and collect the same renewal. Keep
instances NTP-synchronized. Two fences stop a copy from billing:

- **Book identity.** `billing.book_identity` records the cluster, database and
  schema the book was armed in. Anywhere else (a dump restored elsewhere, a
  re-initialized cluster) every provider write is readonly and
  `openrails book status` says so; `openrails book arm --by NAME` arms it once
  every other copy is stopped. A promoted physical replica or a physical clone
  keeps the identity: rotate the PSP credentials before running a clone.
- **Merchant write posture.** `billing.merchant_write_posture` lowers one
  merchant below `provider_write_mode`. A billing export leaves its source
  readonly and an import lands readonly until
  `openrails merchant arm --merchant NAME --by NAME`;
  `openrails merchant hold --mode readonly|limited` holds one merchant.

Move a merchant only by a billing archive ([backup and restore](#backup-and-restore)),
and never restart a stale source as a rollback.

## Upgrades and migrations

`run-server` and `run-worker` migrate OpenRails', AuthKit's and River's schemas
under an advisory lock before serving: one instance migrates, the rest wait, and
a failed migration fails the start. `openrails migrate up` runs the same step on
its own, as a deploy step; `openrails migrate status [--json]` compares the
applied ledger with the binary's chain and exits non-zero on any mismatch. With
`database.skip_migrations`, startup only checks and is refused with
`ErrMigrationsRequired` until the step has run for the build.
With `clickhouse`, the same step creates or upgrades the event store's database, and startup checks it too.

The schema is one baseline migration and numbered migrations after it, each an
addition, so the previous build runs on a schema the next one migrated and
rolling upgrades are safe. A database that recorded a migration with other
content is compared with a fresh build of the migrations it records, whatever
monthly partitions exist: equal, it is re-stamped and the change audited;
different, the migration stops and reports the difference. Before v1 a release
may break the schema in one step: its release notes say so, the previous build
does not run on the migrated schema, there is no rollback past it, and every
host sharing the schema upgrades together. Migrations are never reversed.

From v1.0.0 the Go API, the HTTP API and the schema change only by addition
([compatibility](../compatibility/README.md)).

Upgrading from a release that scheduled provider refreshes: stop the old workers
sharing the River schema before starting the new fleet. The new fleet cancels
outstanding `openrails.provider_refresh` and `openrails.provider_refresh_merchant`
jobs in that schema, keeping their history; request any refresh you still need
with the command ([provider refresh](#provider-refresh)).

## Health and monitoring

| Signal | Meaning |
|---|---|
| `GET /health/live` | the process serves HTTP; only after PostgreSQL answers and migrations finish, so allow a long startup probe |
| `GET /health/ready` | PostgreSQL, the merchants service, the River producer and a local River consumer. Redis, Vault, PSP posture and refused configuration are degraded, never failing. Public and detail-free: a failure is logged and answers 503 |
| `GET /metrics` on `private_port` | `openrails_dependency_up{dependency,class}`, `openrails_abuse_state_fallbacks_total` and, per job kind, `openrails_job_overrunning`, `openrails_job_retryable`, `openrails_job_discarded` and `openrails_job_last_completed_timestamp_seconds`. Never route it publicly; `Server.PrivateHandler` mounts it in your own binary |
| probes | `openrails_psp_posture`, `openrails_configuration` (what is not served and why), `openrails_solana_crankers` and `openrails_job_progress` |
| `openrails workers [--json]` | each job kind as River holds it: overrunning, retrying and discarded jobs, last completion and the newest error |
| logs | structured (`logger.format`), to stderr, at `logger.level`. Secrets, card data and SQL arguments are never logged |

`openrails_job_progress` fails with no River leader, an overrunning job, or a
billing pass that has not finished within a day or left critical work (usage
postings, renewals and retries, balance watches, delinquency, order expiry,
usage billing) due since before the start of yesterday (UTC).

On SIGTERM the server fails readiness at once, keeps serving for `drain_delay`,
then stops within `shutdown_timeout`; keep their sum under your orchestrator's
grace period. `run-server --no-workers` serves but never becomes ready;
`run-worker` has no listener.

Healthy is a near-empty `openrails intents`, no open findings and no overrunning
or discarded jobs. [River jobs](river-jobs.md) lists every job kind, what queues
it and how to inspect one; the [operations inventory](operations-inventory.md)
lists each provider call and local change behind them.

## Write safety and the kill switch

Declare `provider_write_mode` (`PROVIDER_WRITE_MODE`, `--provider-write-mode`)
explicitly. The lower of it, the merchant's write posture and the book identity
applies.

| Operation | `full` | `limited` | `readonly` |
|---|---|---|---|
| customer purchase, card save, tier change, resume, refund | yes | yes | refused |
| customer or staff cancel, and its provider delete | yes | yes | intent parks |
| dunning charges and window-expiry cancellations | yes | intents park | no |
| usage-order collection, Solana pulls | yes | no | no |
| `catalog apply` publishing a recurring Solana plan | yes | binds an existing plan, else refuses the price | as `limited` |
| `catalog apply` local changes, provider reads, webhooks | yes | yes | yes |

`limited` stops what the system initiates; `readonly` stops the wire, enforced
in each rail's transport. Verified receipt recovery runs in every mode. Every
subscription has one collector, fixed when it is created or imported:
`engine` (OpenRails charges a saved card or pulls on chain, and retries
declines), `nmi_schedule` (NMI charges, OpenRails retries) or `provider` (Stripe
or CCBill charge and retry). Configuration never moves a subscription to another
collector.

Each PSP's environment decides whether money is real, and the provider must
agree before it arms; test seams (`provider_sandbox`) are refused beside a live
PSP, and a non-live PSP relaxes no other protection.

**Booting against production credentials.** Set the mode before the first
start: imported `past_due` subscriptions are due at once.

1. Boot `limited` (the site works; system writes park) or `readonly`.
2. Let provider reads catch up. Dunning materializes in-window charges as parked
   `manual_rebill` intents and parks window-expired cases `unverified`; it
   cancels nothing.
3. `openrails intents --merchant <name>` prints the drain forecast ("N execute
   under limited, M require full"). Resolve what should not fire.
4. Review `GET /v1/merchant/findings` and `openrails proposals list`.
5. Set `full`. Missed whole periods are never back-billed.

**The kill switch.** `billing.destructive_action_switch` is one row read before
every destructive act, so one `UPDATE` reaches every instance at its next check.
It ships off: a fresh deployment makes no cancellation, revocation, provider
delete or pull overwrite until it is enabled. Detection, findings, additive
repairs and notices run either way. A held retraction stays
`reconcile_required` with `held_by: kill_switch`; a provider-confirmed cancel it
holds parks the subscription `unverified` with access kept, until a targeted
verification or an operator refresh applies it after the switch is enabled.

```sql
-- stop everything now; parked intents resume when it is enabled again
UPDATE billing.destructive_action_switch SET enabled = false,
       updated_by = 'you', reason = 'incident: mass cancellation observed';
SELECT enabled, updated_by, reason, updated_at FROM billing.destructive_action_switch;
```

Workers log `retractions held — instance kill switch is OFF`.

**First enforcement.** A merchant with no `billing.merchant_destructive_policy`
row, or `enforce_armed_at` null, pulls in advisory mode: findings are stored,
nothing changes, and `first_pull_completed_at` marks the survey ready. Review it,
then arm both halves:

```sql
SELECT finding_type, status, count(*) FROM billing.reconciliation_findings
 WHERE merchant_id = :merchant GROUP BY 1, 2 ORDER BY 3 DESC;
INSERT INTO billing.merchant_destructive_policy
       (merchant_id, destructive_actions_enabled, enforce_armed_at, updated_by, reason)
VALUES (:merchant, true, now(), 'you', 'reviewed first-pull findings')
ON CONFLICT (merchant_id) DO UPDATE
   SET enforce_armed_at = now(), destructive_actions_enabled = true;
UPDATE billing.destructive_action_switch SET enabled = true, updated_by = 'you';
```

Until then a cancel of an NMI- or CCBill-owned membership is
`409 provider_cancel_held`: OpenRails will not cancel locally while the provider
would keep charging.

**Cancellation caps.** A provider read, or a billing import's cancels of subscriptions it did not create, may cancel at most `min(25, max(3, 5% of the PSP's live linked book))` subscriptions per PSP, or the whole book when it has at most 5. Over that it cancels none, halts the merchant's pass and raises `pull.cancellation.capped`; a held import cancel keeps the subscription `unverified` with access. The usual causes are a misdeclared `account_id`, a credential rotated onto a sibling sub-account, or a provider returning a short page.

## Destructive holds

Staff refunds, cancellations, payment-method removal, access and credit revocation, write-offs and substantive catalog or configuration changes share one admission policy. It counts each staff member's and application's logical actions (affected targets in a batch, not requests), keyed on the verified issuer and invoker: rotating a credential creates no new allowance. A billing import is a merchant-level operation: its cancels, the terminal declines it lands and the provider stops it owes a canceled subscription are `cancel_subscription` actions recorded in its caller's name and admitted together before any lands, against the merchant's and the deployment's budgets rather than the caller's; a book larger than what they have left is refused with 429 and `Retry-After`, opening no incident. Replays and proven no-ops spend nothing; creating a catalog object spends nothing; a file applied with `openrails catalog apply` spends the merchant's own budget. An in-process call that names no person is the host's own work: it is recorded, and spends no allowance.

The defaults are `destructive_action_limits`: 5 an hour and 15 a day per actor;
per merchant at least 15 and 25, per deployment at least 50 and 100, both rising
by one per 400 (hour) and 100 (day) customers active in the last 30 days. Startup
refuses a negative limit, an hourly limit above its daily one, an actor limit
above the merchant's and a merchant limit above the deployment's.

A denied admission answers 429 and opens an incident that persists until
explicit review: waiting, restarting, rotating a key or resolving a finding does
not release it. Reads and the completion of effects already started continue;
new provider requests are held. A pending or uncertain action stays reserved
after clearance, so clearing cannot unleash an old queue beside a new allowance.

Review and clear:

- `GET /v1/merchant/destructive-holds` lists the merchant's incident summaries;
  `POST /v1/merchant/destructive-holds/{id}/clear` with `{"reason": "…"}` clears
  one. It needs merchant configuration authority plus `root:admin-rate-limits:unlock`
  (`billing.RootAdminRateLimitsUnlock`) from a person acting for themselves.
  Application keys, personal API keys, delegated identities and the triggering
  actor are refused, so a different person clears a hold.
- `openrails destructive-holds list [--limit N --after ID]` and
  `openrails destructive-holds clear <merchant> <hold-id> --operator-issuer ISSUER --operator SUBJECT --reason TEXT`.

Clearance covers one episode and records the operator and reason; a further
breach opens a new incident. Hosts receive `destructive.hold_opened` and
`destructive.hold_cleared` in the event log, and operator email and webhook
channels receive an alert, at least once. Incidents and the action audit belong
to the deployment: keep them in full database backups; a merchant archive
neither resets nor carries them.

## Abuse limits

Counters, captcha challenges, card-testing declines and admin lockouts live in
Redis when configured, otherwise in each process's memory; never in PostgreSQL.

| Bucket | Default per minute | Routes |
|---|---|---|
| `purchase` | 10 | `POST /v1/me/orders`, an order's pay and confirm, `POST /v1/orders/{id}/solana-pay` |
| `subscribe` | 20 | the `/v1/me/subscriptions` writes |
| `payment` | 40 | `/v1/me/payment-methods` |
| `webhook` | 1200 | `/v1/webhooks/{rail}/{account_id}` |
| `catalog-ask` | 10 | `POST /v1/console/catalog/ask` |

Counting is per minute, per bucket and per subject (the client IP, and the user
when authenticated); the strictest trip refuses with `429 rate_limit_exceeded`,
`X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset` and
`Retry-After`. Other routes are not limited here: a per-address ceiling belongs
to your proxy. A pay URL is also limited per order (120 reads and 10 pays a
minute). A declared `Content-Length` over a bucket's ceiling is refused 413
before counting (64 KiB for purchase, subscriptions and payment methods; per-rail
caps for webhooks).

- **Captcha.** A subject reaching three times a purchase, payment or subscribe
  limit in a window is challenged for 15 minutes: `403 captcha_required` with
  `X-Captcha-Required: true` until it sends a valid `X-Captcha-Token`. Staff,
  console and server-to-server routes never see a captcha.
- **Card testing.** Card saves and payments check declines before any provider
  call: 6 declines in 15 minutes per customer or address (IPv6: the /64) block
  for the window, 10 in 24 hours for the day (`429 card_attempts_blocked`). 100
  declines in 24 hours from at least 25 customers and 25 addresses is attack
  mode for the merchant: every subject must solve a captcha, and any with a
  recent decline is blocked. A customer without a recent decline is never
  blocked.
- **Customer cancellations and card removals** share one merchant-wide budget,
  enforced even with `rate_limits_disabled`: per rolling day the larger of 10
  customers or 1 % of customers who paid in the last 30 days, per rolling hour
  the larger of 5 or one per 400 paying customers (`user_action_rate_limit`).
  A refusal is 429, records a finding and changes nothing.
- **Admin lockouts.** Repeated refused staff operations lock a person out;
  `openrails admin-lockouts unlock <user-id>` ends one (with Redis; without it a
  lockout lives in the serving process).

## Provider intents

Every outbound provider mutation is first a row in `billing.provider_intents`: an idempotency key, an origin (`user`, `admin`, `system`), the PSP it targets and a relevance window. Its own job (`openrails.provider_operation`) claims it under a lease, rechecks relevance, gates on mode and origin, executes and classifies the outcome; each attempt's phases (attempting before the provider call, then its result or a hold) are written ahead to `billing.operation_log`, the operations history. A blocked intent (provider down, write mode, credentials that do not arm) stays pending with its reason and drains when the blocker lifts; one past its relevance window is superseded or expires instead of firing stale. An ambiguous money movement is resolved by reading the provider before any retry; deletes verify, then execute.

Intents follow the PSP id, not its key: they never execute against another
account. To move to another account, declare a new PSP and archive the old one
(`{archived: true}`, no provider call); the old one's subscriptions keep renewing
there until they lapse. Rotate credentials within one PSP with
`PATCH /v1/merchant/psps/{id}` (the console's **Rotate**), which probes the new
credential first, or by editing the manifest and restarting.

```sh
openrails intents --merchant <name> [--status active|pending|…|all] [--rail nmi] [--type manual_rebill] [--format json]
openrails intents-log --merchant <name> [--rail] [--intent] [--psp] [--phase attempting|succeeded|failed|unknown|parked]
openrails intents resolve --merchant <name> --intent <id> [--step S] (--receipt <provider id> | --not-executed) --reason TEXT
```

`intents` shows each row's `executes_under` mode and the drain forecast.
`resolve` closes an `unknown_needs_verify` operation from an exact provider
receipt or provider-confirmed non-execution (an empty NMI search is never
enough); it never resends. Webhooks are at-least-once and handlers idempotent;
there is no local inbound queue, so a long outage is backfilled with a provider
refresh.

## Provider refresh

Request a refresh for one merchant after missed webhooks, an outage or when
investigating drift. It is an operator command; nothing schedules it.

```sh
openrails --config /etc/openrails/config.yaml provider-refresh --merchant my-store
```

It answers `job_id`, `merchant_id`, `river_schema` and `queue`; `already_queued`
is true when a refresh for that merchant is outstanding. Track the job with
`openrails workers` or in `river_job` ([River jobs](river-jobs.md#inspect-a-running-deployment)).
The job reads each PSP in bounded windows from its watermark
(`billing.psp_refresh_watermarks`) to five minutes ago (NMI's by last change,
the last reading through now), NMI and Stripe with a
24-hour overlap for late indexing, and retries incomplete reads up to five
attempts; a later request resumes from the saved watermarks. A watermark
advances only after a complete read. Before each Stripe window a separate read
records refunds and disputes that match existing payments, up to eight days
back, even when the full read fails.

CCBill's DataLink answers a live account once every 60 minutes. A live CCBill
PSP's refresh is its own job, `openrails.paced_provider_refresh`, which the
merchant's queues and which makes one DataLink request per hour: a day of transactions, advancing the watermark day by day,
then the roster once the days are read to within two hours. A backlog catches
up a day an hour; 90 days take about four days. A PSP import of a live account
reads its history a day an hour the same way, its `history_through` showing how
far it got. A request waiting for its hour, or one CCBill refuses for coming
within it, waits and resumes; it never fails the job. The hour is kept per
CCBill account in the abuse state (Redis), so every instance shares it; a
sandbox account is not paced.

Refresh reads run in every write mode. Overwrites and lifecycle repairs need an
armed merchant, the kill switch on and a posture above `readonly`. Unverified
subscriptions are also checked without a refresh: the hourly billing pass
retries flagged ones against their own PSP in bounded batches.

| Provider evidence for an `unverified` subscription | Resolution |
|---|---|
| live, paid through a later period | active again, period advanced |
| canceled at the provider, still paid | canceled at period end, access to the paid date |
| remote absent or terminal | canceled locally, access revoked |
| inconclusive or unreachable | stays `unverified`, access kept |

NMI merchant-defined field 20 and Stripe `openrails_book` mark this book's
objects; an unknown object without the mark is a low finding
(`evidence.quiet_foreign`).

## Findings, proposals and undo

Writers commit a change and its effects together, and a customer's
`openrails.customer_converge` job re-derives them; deadlines with no event are
indexed due work in the hourly passes. Findings are
`<plane>.<subject>.<condition>` in `billing.reconciliation_findings`: `pull.*`
(a mirror row against the provider, repaired by an approved proposal),
`derive.*` (a grant effect against its source), `life.*` (a record against its
clock) and `consistency.*` (duplicates, mismatches, unbooked events). A finding
resolves itself when the divergence goes, and raises console notifications and
alerts. Retractions from a provider read wait until the merchant's domain is
proven fully reconciled (`billing.reconciliation_state`). Unlandable provider
subscriptions park (`GET /v1/merchant/parked-subscriptions`) until their price
is mapped or customer linked.

```sh
openrails proposals list    --merchant <name> [--psp KEY] [--kind K] [--source pull|verification]
openrails proposals show    --merchant <name> <fnd_id>
openrails proposals approve --merchant <name> <fnd_id>… | --psp KEY --kind K --expect N
openrails proposals dismiss --merchant <name> <fnd_id>… --notes TEXT
openrails proposals foreign --merchant <name> <fnd_id>… --notes TEXT
openrails proposals runs    --merchant <name>
```

Each proposal applies in its own transaction; a filtered set needs `--expect`
equal to its size. One whose basis moved, or that ends access on evidence older
than 7 days, is superseded. The kill switch holds changes that end access or
write to a provider (`409 destructive_actions_held`); a readonly posture holds
those that move money. Over HTTP: `/v1/merchant/proposals` (approving and
dismissing need a person: `403 person_required`).

`openrails prune --merchant <name> [--psp] [--rail] [--since --until] [--expect-rows N]`
retires the local subscriptions and payments a PSP no longer lists: without
`--expect-rows` it is a plan; applying needs N equal to the plan, one PSP at a
time, and an empty roster or an implausibly small NMI set is refused. `prune` and
approval runs reverse with one command:

```sh
openrails prune list --merchant <name>
openrails undo-run --merchant <name> --run <id>                          # plan
openrails undo-run --merchant <name> --run <id> --apply --expect-rows N
```

An undo restores rows in one transaction (a row changed since keeps its newer
state), supersedes the run's unfired intents, lets convergence rebuild access,
and reports what it could not undo: provider writes already sent. It never
touches the ledger, grants or logs.

## Retention

Periods are constants, the same for every merchant, except the event log's `event_retention`. Each table's comment states its class: **permanent** (the ledger, grants, payments, owed usage, usage postings, receipts, operation authorizations, refusals, running charges, maintenance runs, the before-images of destructive runs, MRR movements and scheduled MRR changes), **partitioned** (monthly partitions dropped whole), **rows** (deleted by the hourly cleanup) or **state** (one row per thing that exists).

| Table | Deleted |
|---|---|
| `limit_holds` (by `created_at`), `limit_hold_parts` (by `hold_created_at`) | partition dropped after 61 days |
| `subscription_status_transitions`, `subscription_billing_changes`, `operation_log`, `payment_attempts`, `rebill_cycles`, `nmi_history_months` | 25 months |
| `provider_intents` | instruction-only intents 25 months after they last changed; the rest are permanent |
| `reconciliation_findings` | resolved ones, 12 months after resolved and last seen |
| `order_attempts`; `orders`, `order_lines` | attempts that never reached a provider 90 days after `expires_at`; unpaid closed orders with no attempt, 90 days |
| `parked_provider_subscriptions` | 90 days after release |
| `carts` | 30 days after `last_changed_at` |
| `notification_events`, `host_outbox`, the event log | 90 days, never before `event_retention` |
| `webhook_events` | 90 days after completion |
| `customer_messages` | two years (731 days) after created |
| `idempotency_keys`, `solana_pay_references` | at expiry, after the 7-day watch |
| `user_destructive_reservations` | 24 hours |
| `limit_windows` | 61 days after `window_ends_at`; a lifetime or rate limit's row stays |
| `leases` (with their holds), `limit_adjustments` | 61 days after the last sync, or after the adjustment |
| `webhook_health_daily` | 13 months |
| `periodic_leases` | a kind unscheduled for 30 days |

The event store's tables expire by their own TTLs: raw usage events 100 days after they occurred, a pricing period's quantities 100 days after it starts, minutes after 2 days, usage history after 25 months, and a limit window's usage 61 days after it ends (a lifetime limit's stays).

The hourly cleanup creates partitions two months ahead and drops expired ones
(giving up after 2 seconds rather than queue behind a long reader), then deletes
1,000 rows per statement and at most 50,000 per merchant per pass.
`subscription_status_transitions` refuses other deletes
(`billing.guard_retention_delete`). What integrators see: usage may be dated up
to 35 days back; a lease answers resends for 61 days after its last sync; a
request hold lasts at most 30 days; after 61 days a request id reads as never
admitted; usage reports reach back about 26 months.

## Backup and restore

Back up PostgreSQL and the merchant configuration (Vault with its
`credential_fingerprint_key`, or the manifest and its overlays), plus
`keys.json` and `totp.key` in your secret manager. PostgreSQL holds no
credential, so a database alone cannot charge anyone.

Turn on point-in-time recovery (`wal_level = replica`, `archive_mode = on`, an off-host `archive_command`, periodic base backups). PITR is whole-cluster: right for disaster, wrong for one damaged merchant. A restore never undoes what happened at a provider; it creates divergence that convergence repairs. Money is reversed by compensating entries, never deletion; the append-only tables (the ledger, grants, status transitions, the operations and configuration histories, `webhook_events`, findings) are restored whole or not at all.

1. Stop and drain the source writers and disable their restart.
2. Restore PostgreSQL; make the configuration available.
3. Start it. A restore into another cluster, database or schema stays readonly
   until `openrails book arm`, once the source is gone for good. Start
   `readonly` to inspect first.
4. Review unresolved findings: ignoring one does not settle it.

Drill restores into a scratch instance: migrations current, PSPs arm, a refresh
produces sane findings.

**ClickHouse.** The event store holds every customer's usage the month-end close has not posted yet, so back it up nightly to S3-compatible storage: `BACKUP DATABASE openrails TO S3('https://<bucket>/clickhouse/<date>', '<key>', '<secret>')`, incremental (`SETTINGS base_backup = S3(...)`) from a weekly full one, kept 8 days like PostgreSQL's window. A backup carries the store's identity: restore it (`RESTORE DATABASE openrails FROM S3(...)`) before starting OpenRails, and the boot finds the store its database recorded. Usage that arrived after the backup is lost with it; what posted is in PostgreSQL. With no backup, `openrails usage attach` records an empty ClickHouse ([event store](#event-store)).

**One merchant's archive.** `openrails billing export` writes a complete,
integrity-protected snapshot of one merchant's billing to a private file, with
every writer stopped (`--source-stopped` attests it), and leaves the source
readonly. `openrails billing prepare-target` provisions the exact merchant
identity at the destination; `openrails billing import --target-stopped` restores
into it and lands readonly until `openrails merchant arm`. Over HTTP the same
archive is `GET` and `POST /v1/merchant/billing-archive`. With an event store the
archive carries the merchant's usage from it: raw events within their 100 days,
and the history and limit usage older events no longer rebuild. Restoring one
with usage needs an event store holding none of the merchant's; without a store
an export refuses a merchant with unposted usage. The event log, active
destructive incidents and pending actions are not in an archive.

**The catalog alone.** `openrails catalog export --merchant UUID --out FILE`
writes a sealed snapshot of every catalog table with original ids and history;
`openrails catalog import --merchant UUID --in FILE` restores it into an empty
catalog for the same merchant. It is a backup, separate from the editable
documents `catalog apply` takes; `openrails dump-merchant-catalog --slug` prints
the active catalog as one.

**Not backups:** the purge inventory (counts and document names only; after a
purge, PITR is the only way back), a provider read (providers hold no access,
grants or ledger) and `undo-run` (one operation).

**Deleting a merchant.** `openrails merchants delete` is soft: off the lists,
credentials resolve nothing, records kept; `merchants restore` brings it back.
`openrails merchant purge --merchant NAME --by NAME` records an inventory;
typing back its id, row count and confirmation phrase (`--inventory`,
`--expect-rows`, `--confirm`) with the kill switch on deletes the merchant's rows
permanently and queues its Vault cleanup. No deletion revokes cards at the PSP.

## Commands

The image is the CLI. Every command takes `--config`/`-c` (default
`config.yaml`) and `--provider-write-mode`. One-off commands open only what they
need, start no server or workers, and keep standard output parseable. A
merchant is named by its name or `id:<uuid>`; commands that act over HTTP take
`--server-url` (or `--url`) with `--token-file`, re-read on every call, and act on
the merchant the token is bound to.

| Command | Does |
|---|---|
| `openrails run-server [--no-workers] [--merchant-manifest PATH]` | serve the API and work jobs; `--no-workers` serves but is never ready |
| `openrails run-worker [--merchant-manifest PATH]` | work jobs only |
| `openrails migrate up`, `openrails migrate pg` | apply OpenRails', AuthKit's and River's migrations |
| `openrails migrate status [--json]` | compare the applied ledger with the binary's; non-zero on any mismatch |
| `openrails version` | the version, commit and build date |
| `openrails push-auth-bootstrap [--file] [--dry-run] [--startup-only]` | create AuthKit root authority from a bootstrap manifest (`/etc/openrails/bootstrap.yaml`); first run only |
| `openrails push-merchant-config [--file] --insert` | create the merchants a manifest names that are missing; bare, it only reports. `--overwrite` and `--prune` are refused |
| `openrails dump-merchant-config --slug NAME [--out]` | print a merchant's configuration as manifest YAML, without credentials or alert webhooks |
| `openrails get-merchant-config --merchant NAME` | read a merchant's configuration through the client (remote: `--server-url`, `--token-file`) |
| `openrails apply-merchant-config --merchant NAME --file PATH` | apply configuration metadata by revision (Vault only) |
| `openrails dump-merchant-catalog --slug NAME` | print the active catalog as a document |
| `openrails catalog apply FILE --merchant NAME [--force-conflicts]` | apply a catalog document; startup never does |
| `openrails catalog export --merchant UUID --out FILE [--overwrite]`, `openrails catalog import --merchant UUID --in FILE` | a sealed catalog snapshot with ids and history |
| `openrails billing export`, `openrails billing import`, `openrails billing prepare-target` | move one merchant's billing ([backup and restore](#backup-and-restore)) |
| `openrails merchants list`, `merchants get`, `merchants create`, `merchants rename`, `merchants delete`, `merchants restore` | the merchant directory; delete is soft, a rename forwards the former name under `naming` |
| `openrails merchant arm`, `openrails merchant hold` | a merchant's write posture |
| `openrails merchant purge` | record a purge inventory, or with it typed back delete the merchant permanently |
| `openrails book status`, `openrails book arm --by NAME` | the book identity |
| `openrails usage attach --actor NAME --reason TEXT [--discard-unposted]` | record the configured ClickHouse as the event store in place of a lost one; `--discard-unposted` discards usage not yet posted, recorded per merchant |
| `openrails provider-refresh --merchant NAME` | queue a provider refresh and print its job |
| `openrails intents`, `openrails intents resolve`, `openrails intents-log` | the provider intent ledger |
| `openrails proposals list`, `proposals show`, `proposals approve`, `proposals dismiss`, `proposals foreign`, `proposals runs` | review what provider reads propose |
| `openrails prune`, `openrails prune list`, `openrails undo-run` | retire what a PSP no longer lists; reverse a run |
| `openrails destructive-holds list`, `openrails destructive-holds clear` | destructive-action incidents |
| `openrails admin-lockouts unlock <user-id>` | end an administrator's lockout |
| `openrails workers [--json]` | job health from River's tables |
| `openrails ledger-audit [--merchant NAME] [--format json]` | check ledger conservation, account counters and credit lot counters; non-zero on a finding. Nothing schedules it |
| `openrails nmi decline-report --merchant NAME --since DATE [--until] [--psp]` | read-only NMI approval and refusal rates by month and reason |
| `openrails solana-pay resolve --merchant NAME --signature SIG --resolution TEXT` | close a reviewed Solana Pay receipt |
| `openrails sandbox nmi-gateway [--listen] [--plan] [--max-lifetime]` | serve an NMI fake for `provider_sandbox.nmi_gateway_url` in disposable test stacks |

## Kubernetes and Helm

The chart runs the server (`openrails run-server`) as one Deployment.
PostgreSQL and Redis are yours: the chart takes their addresses in `config` and
their credentials from existing Secrets. It creates no Secret and inlines none.

```sh
helm install openrails oci://ghcr.io/open-rails/charts/openrails --version X.Y.Z \
  --namespace openrails --create-namespace -f values.yaml
```

A minimal `values.yaml`:

```yaml
config:
  provider_write_mode: full
  public_billing_base_url: https://billing.example.com
  auth:
    default_tenant_id: 51336692-ef57-4cdb-a189-a8d388465c11  # generate once
    token: {issuer: https://billing.example.com}
    resource: {id: https://billing.example.com}
  redis: {addr: redis:6379}
  email_smtp: {host: smtp.example.com, port: 587, username: openrails, from: "Billing <billing@example.com>"}
  trusted_proxies: [10.244.0.0/16]   # the pod CIDR the ingress controller runs in
files:
  merchants.yaml:
    version: 1
    merchants:
      shop:
        display_name: Shop
        remote_application:
          issuer: https://id.example.com
          jwks_uri: https://id.example.com/.well-known/jwks.json
secrets:
  env:
    DB_URL: {name: openrails-db-app, key: uri}
  files: [openrails-secrets]         # EMAIL_SMTP_PASSWORD, REDIS_PASSWORD, …
  authKeys: openrails-auth-keys      # keys.json and totp.key
ingress:
  enabled: true
  className: traefik
  hosts: [billing.example.com]
  tls: [{secretName: billing-tls, hosts: [billing.example.com]}]
```

| Value | Meaning |
|---|---|
| `replicaCount` | 1. More need Redis (`config.redis`) and `secrets.authKeys`: an ephemeral signing key is per pod |
| `image` | `repository`, `tag` (default the chart's `vX.Y.Z`; required to render a checkout, whose appVersion is `latest`), `digest` (pins over the tag), `pullPolicy` |
| `imagePullSecrets` | for a private registry |
| `args` | the command, `[run-server]`; the chart appends `--config=/etc/openrails/config.yaml` |
| `config` | `config.yaml`, rendered as written; no secrets. `host`, `port` and `private_port` are the chart's and refused here |
| `files` | more files in `/etc/openrails`: `merchants.yaml` (read at every boot), `bootstrap.yaml` (first run), `catalog.yaml` (for `openrails catalog apply`) |
| `secrets` | `env` (one variable per Secret key), `files` (Secrets whose keys are variable names, mounted at `/vault/secrets`), `authKeys` (`keys.json` and `totp.key` at `/vault/auth`, reloaded on change), `merchantOverlays` (`{name, key}` overlays added to `merchant_manifest_overlays`) |
| `env`, `envFrom` | further environment |
| `ports` | `http`, default 3053 |
| `metrics` | the private `/metrics` listener (`enabled`, `port` 9090), served by its own ClusterIP Service `<fullname>-metrics` whatever `service.type` is, and an optional Prometheus Operator `serviceMonitor` |
| `service` | `type`, `port` (80), `annotations` |
| `ingress` | `enabled`, `className`, `annotations`, `hosts` (every path to the server), `tls` |
| `resources` | requests 100m and 256Mi, limits one CPU and 768Mi |
| `podSecurityContext`, `securityContext` | UID and GID 10001, non-root, read-only root filesystem, no capabilities |
| `startupProbe`, `livenessProbe`, `readinessProbe` | `/health/live` (startup allows 10 minutes for migrations) and `/health/ready` |
| `terminationGracePeriodSeconds` | 30; raise it with `drain_delay` and `shutdown_timeout` |
| `strategy` | the Deployment's rollout strategy |
| `podDisruptionBudget` | off; `maxUnavailable: 1` when enabled |
| `serviceAccount` | created, no token mounted unless `config.vault.auth_method` is `kubernetes` |
| `podAnnotations`, `podLabels`, `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints` | scheduling; spread constraints are rendered with `tpl` |
| `extraVolumes`, `extraVolumeMounts` | further mounts |
| `nameOverride`, `fullnameOverride` | resource names |

A change to `config` or `files` rolls the pods.

**Verifying the image.** Each release image carries a signed build-provenance attestation of its digest in both registries. Check it before pinning that digest in `image.digest`:

```sh
gh attestation verify oci://docker.io/openrails/openrails:vX.Y.Z --repo open-rails/openrails
```

**Secrets.** `secrets.env` takes CloudNativePG's app Secret directly
(`DB_URL: {name: openrails-db-app, key: uri}`). With External Secrets Operator,
name each target key after its variable; with the Vault Agent Injector render one
file per variable into `/vault/secrets` with `podAnnotations` and leave
`secrets.files` empty. For Vault-held merchant configuration declare
`config.vault` with `auth_method: kubernetes`: the chart mounts the pod's
ServiceAccount token; bind the role in `k8s_role` to the ServiceAccount the chart
creates (`<release>-openrails`, or `serviceAccount.name`).

```sh
kubectl -n openrails create secret generic openrails-auth-keys --from-file=keys.json --from-file=totp.key
kubectl -n openrails create secret generic openrails-secrets --from-literal=EMAIL_SMTP_PASSWORD="$SMTP_PASSWORD"
```

**PostgreSQL** needs version 18 and one role owning its database: a
CloudNativePG cluster (`imageName: ghcr.io/cloudnative-pg/postgresql:18`,
`bootstrap.initdb: {database: openrails, owner: openrails}`) writes the
`openrails-db-app` Secret whose `uri` key is `DB_URL`. **Redis** or Garnet has no
default; its credentials and CA go in `secrets.files` (`REDIS_PASSWORD`,
`REDIS_CA_CERT`).

**Probes and drain.** On SIGTERM `/health/ready` answers 503 at once while the
pod keeps serving for `drain_delay`, so endpoints drop it before it stops
accepting; no `preStop` hook is needed. Every pod runs `run-server`, so a replica
adds both API and workers; the chart runs neither `--no-workers` nor
`run-worker`.

**Upgrades** are `helm upgrade` with a new chart version: each new pod migrates
under the advisory lock while old pods serve, and a pod whose migration fails
never becomes ready, stopping the rollout. `helm rollback` returns the previous
image; migrations are never reversed, so a pre-v1 release that breaks the schema
has no rollback past it.

**Backups.** With CloudNativePG archive WAL and take base backups to object
storage for point-in-time recovery. For one merchant's archive, scale the
Deployment to zero first, then run `openrails billing export --source-stopped`
against the database. Operator commands run in a pod:

```sh
kubectl -n openrails exec deploy/openrails -- \
  openrails --config /etc/openrails/config.yaml intents --merchant shop
```

## Troubleshooting

| Symptom | Look at |
|---|---|
| boot refuses a key | its log names the key and, for a retired one, its replacement |
| `/health/ready` 503 | PostgreSQL, the merchants service or local workers: the log names it |
| a PSP never arms | the `openrails_psp_posture` probe: the provider's verdict for its environment, or credentials of another account (`consistency.duplicate_gateway_account`) |
| configuration not served | the `openrails_configuration` probe and `consistency.merchant_config.not_served` |
| 401 on a trusted issuer's token | its `iss` (a trusted issuer of that merchant), its `aud` (`auth.resource.id`), its signing keys' JWKS |
| `403 merchant_unresolved` | the credential is bound to no live merchant: fix the credential, nothing in the request changes it |
| 403 with a permission code | the role behind the key or the issuer's application lacks the route's permission |
| 404 on a route | its route group is off (`route_groups`) |
| every client shares one rate limit | `trusted_proxies` is unset behind a proxy |
| webhooks refused | the URL's rail and account id, the PSP's signing secret, CCBill's source address |
| `400 pay_url_unavailable` | `checkout_origin` is unset |
| renewals not charging | `provider_write_mode`, the merchant's posture, `openrails book status`, `openrails intents` |
| cancels refused with `provider_cancel_held` | the kill switch and the merchant's first enforcement |
| jobs stuck | `openrails workers`, `openrails_job_progress`, the River schema of every fleet |
| missed provider events | `openrails provider-refresh --merchant NAME` |
