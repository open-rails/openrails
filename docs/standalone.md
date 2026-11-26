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
| PostgreSQL 18+ | yes | everything: the ledger, grants, subscriptions, access, catalog, provider intents and River's queue. OpenRails owns one schema (`billing`), its River schema (`billing_river`) and the server's AuthKit schema (`billing_auth`); the instance may be shared. | data loss. Provider-owned facts can be re-read with a PSP import; the ledger, credits, access and catalog exist nowhere else. |
| ClickHouse 26.8 LTS | for usage | the event store usage needs: usage reporting, the gate, meters, limits and usage history ([event store](#event-store)) | a ClickHouse without the store's identity is refused at boot: restore its backup |
| Redis 6.2+, Valkey or Garnet | for more than one instance | rate limits, admin lockouts, captcha challenges, card-testing declines, AuthKit's rate limits and spent DPoP proofs | each instance keeps them in memory: right for one instance only. No money path needs Redis, and nothing in it needs a backup. |
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
| Binary | the release assets of each `vX.Y.Z` tag: Linux and macOS on amd64 and arm64, Windows on amd64, with checksums, SBOMs and provenance |
| Image | `docker.io/openrails/openrails:vX.Y.Z` (also `X.Y`, `latest`, and on `ghcr.io/open-rails/openrails`); its entrypoint is the `openrails` CLI |
| Helm | `oci://ghcr.io/open-rails/charts/openrails`, chart `X.Y.Z` for image `vX.Y.Z` ([Kubernetes and Helm](#kubernetes-and-helm)) |
| Source | `task build` builds `bin/openrails`, embedding the console if `task admin-build` ran and the payment page if `task payment-page-build` ran |

`openrails version` prints the version, commit and build date.

The server listens on loopback (`127.0.0.1`) unless `host` names another address; a container sets `0.0.0.0`, and the image, the chart and Compose do. From a checkout, `task dev` runs it with every dependency ([local development](#local-development)).

## Local development

`task dev` starts the Compose dependencies (PostgreSQL, Redis as Valkey, ClickHouse and Vault, each on a loopback port), then the server under Air on `127.0.0.1:3053` and the console under Vite at `http://localhost:3054/admin/`. Once the server is ready it seeds a local operator and a merchant named `sandbox`, and prints the sign-in; `task dev:login` prints it again.

| Sign-in | |
|---|---|
| email | `operator@localhost.test` |
| password | `openrails` |
| role | `root:owner`; the first sign-in enrolls an authenticator app |

Vault is a development server in memory, root token `openrails-dev-root`, with KV v2 at `kv` (the server's `vault.kv_mount`) and Transit at `transit`. It forgets everything when its container restarts; the next `task dev` mounts both again and re-creates nothing else. `docker compose down -v` discards every local service's data.

Root holds that operator, so the server logs no setup link ([the first operator](#first-run)). That operator's password is published, so the server refuses to start listening beyond loopback while the account exists, wherever the database came from. The Compose `openrails` service (`docker compose --profile openrails up -d`, followed by `docker compose logs -f openrails`) listens on `0.0.0.0` in its container and so refuses a database `task dev` seeded: start it on a fresh one (`docker compose --profile openrails down -v` removes everything, volumes included).

## First run

The example's server files are a working local configuration. `config.yaml` is
the process: listeners, database, Redis and the server's AuthKit.

```yaml guide:standalone-config
# The merchant's configuration, its PSPs and their keys, is merchant.yaml: read
# at boot and read-only. To change it in the console instead, name a Vault KV
# mount (vault.kv_mount) and keep only the merchant's name in merchant.yaml.

port: 3053
public_url: http://localhost:3053 # where browsers and PSPs reach this server

database:
  url: postgres://openrails:openrails@postgres:5432/openrails?sslmode=disable
redis: # optional for one server; replicas share rate limits and spent proofs through it
  url: redis://redis:6379

# The server's own AuthKit takes its issuer and resource from public_url. On
# a loopback URL it generates development keys; in production its keys are
# keys.json and totp.key in /vault/auth (auth.keys.path).

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
      nmi-main: # your name for this account; any key you like
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

The server migrates the database under an advisory lock, applies a mounted `/etc/openrails/bootstrap.yaml` once, creates the merchants the manifest names, arms each PSP whose credentials its provider confirms, and starts the workers. It never applies a catalog: that is an operator step.

```sh
openrails catalog apply catalog.yaml --config config.yaml --merchant onlydemo
```

**The first operator.** No credential is shipped or configured. While no account holds a root role, the server logs a single-use setup link on start, at warning level ("no administrator yet"):

```text
https://billing.example.com/admin/setup#token=…
```

Open it, create the owner account (email, username and password) and enroll the second factor its first sign-in asks for. The link is valid 24 hours and only its hash is stored; a restart while it lives logs only its expiry, a start after it expires logs a new one, and once anyone holds a root role it creates nobody. The process that runs the workers logs it (`run-server`, or `run-worker` beside `run-server --no-workers`). From the server's shell, `openrails setup-link` issues a fresh one that replaces a live link and creates an owner even while operators exist: the break-glass. Both need the console, whose `/admin/setup` page opens them.

An init job can seed operators instead, then no link is logged: a bootstrap manifest user holding a root role, with a password they reset through the console (which needs `email_smtp`), as a deployment with `admin_console.disabled` must:

```yaml
users:
  - email: ops@example.com
    username: ops
    email_verified: true
    password: {reset_required: true}
    root_role: root:owner
```

A private deployment's first run, as an init job: the bootstrap manifest seeds that operator and root's trusted applications.

```sh
openrails push-auth-bootstrap --file /etc/openrails/bootstrap.yaml
openrails push-merchant-config --file /etc/openrails/merchants.yaml --insert
openrails catalog apply /etc/openrails/catalog.yaml --merchant your-merchant
```

In production set an https `public_url`, mount a real signing key and `totp.key` ([secrets](#secrets)) and set `trusted_proxies` behind a proxy. Boot refuses an unknown key.

## Configuration sources

Each key of `config.yaml` but a policy section ([below](#configuration-reference)) can come from three places; the later wins:

1. `config.yaml` (`--config`/`-c`, else `OPENRAILS_CONFIG`, else `config.yaml`
   in the working directory or `config/`).
2. Mounted secret files: a file named like the key's environment variable under
   `/vault/secrets` (`VAULT_SECRETS_PATH` names another directory). Vault Agent,
   a Kubernetes Secret or CSI renders them.
3. The environment: the key's path upper-cased and joined by `_`
   (`database.url` is `DATABASE_URL`, `email_smtp.password` is
   `EMAIL_SMTP_PASSWORD`). A list is comma-separated or a JSON array; a section
   may be a JSON object. A `.env` in the working directory is loaded too, and
   the server logs that it did.

Exceptions to the mechanical name: `VAULT_ADDR` is `vault.address` and `VAULT_CACERT` is `vault.ca_cert`; `AUTH_KEYS_PATH` is `auth.keys.path`. A key is spelled exactly as here. With `database.url` unset, libpq's own variables (`PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`, `PGPASSWORD` or `PGPASSFILE`, `PGSSLMODE`, `PGSSLROOTCERT`) name the database.

The file is two Go types: `server.Config`, whose `Engine` is `openrails.Config` with its keys at the top level, and the keys only the binary reads (the connections it opens, `logger`). A key neither reads refuses boot. Embedded, decode the same keys into `openrails.Config` (its `yaml` tags).

The environment holds secrets and per-environment facts (URLs, hosts, ports, credentials); `config.yaml` holds policy. `rate_limits` is set in `config.yaml` alone: a variable or secret file naming one refuses boot.

Three inputs configure a deployment, each on its own lifecycle:

| Input | Owns | Read |
|---|---|---|
| `config.yaml` and its sources | the process: database, Redis, Vault, the server's AuthKit | at boot |
| the merchant manifest, or Vault | each merchant: name, settings, alert webhooks, PSPs and their secrets | at every boot (file), or live (Vault) |
| catalog documents | products, prices, entitlements, limits, provider links | only when an operator applies one ([catalog](client.md#catalog)) |

The process configuration never decides whether money is real: each PSP's
declared environment does ([merchant configuration](client.md#merchant-configuration-and-psps)).

## Configuration reference

[`config.example.yaml`](../config.example.yaml) is an annotated file;
[`.env.example`](../.env.example) lists every environment variable with its
default and [`merchant.example.yaml`](../merchant.example.yaml) every key of a
merchant's document, both generated from the configuration types (`go run
./internal/examplegen -write` in `server/`; `scripts/check.sh guards` refuses
them stale). Durations
are Go durations (`90s`, `24h`). Every key below is read at boot. The defaults
run a server on this machine against the repository's Compose dependencies
(`docker compose up -d`) with nothing configured but `public_url`; none relaxes a protection, and a deployment that leaves one unset fails to connect or refuses to boot. The top-level keys: `host`, `port`, `metrics_port`, `logger`, `public_url`, `checkout_origin`, `trusted_proxies`, `database`, `clickhouse`, `redis`, `auth`, `admin_console`, `email_smtp`, `vault`, `card_vault`, `solana`, `hyperswitch`, `rate_limits` and `captcha`.

**Listener and lifecycle**

| Key | Default | Meaning |
|---|---|---|
| `host` | `127.0.0.1` | the address both listeners bind (`HOST`), an IP literal. The loopback default reaches nothing off this machine; a container sets `0.0.0.0` (the image, the chart and Compose do) and its platform decides exposure. Off loopback the server refuses to start while the development operator `operator@localhost.test` exists ([local development](#local-development)) |
| `port` | `3053` | the public listener: API, console, payment page, webhooks |
| `metrics_port` | unset | the metrics listener: the operator's Prometheus `GET /metrics` (dependency up/down, abuse-state fallbacks, per-job-kind health), on its own port so it is never public; unset serves none, and the chart sets 9090. Not the merchant dashboard's business metrics, which are the `/v1/metrics` API on `port` |
| `logger.level` | `info` | `debug`, `info`, `warn` or `error`; logs go to stderr, text on a terminal and JSON otherwise |

**Routes**

Every route group is served: public, customer (`/v1/me`), payment and webhook routes, customer support (`/v1/admin`), the catalog (`/v1/catalog`), the merchant's configuration (`/v1/merchant`), business metrics (`/v1/metrics`) and your backend's `/v1/app` calls. Each route's permission gates it.

**URLs and origins**

| Key | Default | Meaning |
|---|---|---|
| `public_url` | none: boot refuses without it | where browsers and PSPs reach the server, without `/v1` (`http://localhost:3053` locally). Provider callbacks and billing links are built on it; Stripe webhook registration needs it public https |
| `checkout_origin` | none | needed only to serve OpenRails' own hosted pay page: its separate origin ([the payment page](#the-payment-page)). Leave it unset with the embedded checkout or your own card form; pay URLs then answer `pay_url_unavailable` |
| `trusted_proxies` | none | CIDRs of your own proxies, whose `X-Forwarded-For` is walked; Cloudflare's published ranges are trusted too ([proxies](#tls-proxies-cors-and-webhooks)). It also decides the sender address a CCBill webhook is checked against, so list only the proxy itself |

A return URL (a pay URL's `success_url` and `cancel_url`, a hosted payment's `return_url`, the Stripe billing portal's return) may name only the billing origin, the origin of the merchant's issuer, or one of its `checkout.embed_origins`, so one merchant's checkout never returns to another's site.

**Database**

| Key | Default | Meaning |
|---|---|---|
| `database.url` | `postgres://app:app_password@localhost:5434/openrails_db?sslmode=disable` when neither it nor a libpq variable is set | the connection string (`DATABASE_URL`). Unset, libpq's `PG*` variables name the database, `sslmode` `verify-full` off this machine unless `PGSSLMODE` says otherwise. The default is Compose's Postgres: a deploy that forgets its database fails to connect |
| `database.session_url` | none | the same database and role without the pooler (`DATABASE_SESSION_URL`), when `database.url` is PgBouncer in transaction mode: the engine's pool then prepares no statement, and its listeners, session locks, migrations, AuthKit's pool and one-off commands connect through `session_url` |
| `database.schema` | `billing` | every OpenRails table, function and type; a plain identifier. River's jobs are in the schema plus `_river` |

Boot migrates, replicas taking turns on a lock; `openrails migrate up` runs the same ahead of a deploy. A separate pool of 4 connections renews leases (idempotency claims, the billing pass, merchant pulls): count it in `max_connections`.

SQL is authored in `billing`; another schema is reached by one token-aware
rewrite of schema qualifiers, never other strings. The server's AuthKit keeps its
tables in `<database.schema>_auth` (`billing_auth`) and its jobs in `<database.schema>_river`.

**Event store**

| Key | Default | Meaning |
|---|---|---|
| `clickhouse.url` | `clickhouse://localhost:9000/openrails` | `clickhouse://user:password@host:9000/database` over the native protocol; the path names the event store's database (`openrails` by default), created or upgraded at boot. A secret (`CLICKHOUSE_URL`). Set, the server waits for it at boot like the database; set empty, usage is off. The default is Compose's ClickHouse: when it does not answer at boot, the server logs one line and runs with usage off |

**Redis**

| Key | Meaning |
|---|---|
| `redis.url` | `redis://user:password@host:6379/db`, or `rediss://` over TLS; a secret (`REDIS_URL`). While a declared Redis does not answer, a DPoP proof or a JWT-bearer or client assertion is refused (503): another replica could not see it spent |
| `redis.ca_cert` | the PEM bundle that verifies a `rediss://` server; unset, the system roots |

A Redis section that sets nothing declares none. One with invalid settings is
turned off and reported (`openrails_configuration`); one that stops answering
costs sharing, not service ([scaling](#scaling-and-redis)).

**The server's AuthKit**

OpenRails derives its AuthKit's configuration ([the server's AuthKit](#the-servers-authkit)); a deployment sets only where its keys live. Any other `auth` key or `AUTH_*` variable refuses boot as an unknown key.

| Key | Default | Meaning |
|---|---|---|
| `auth.keys.path` | unset: AuthKit reads `/vault/auth`, or for a loopback `public_url` generates development keys beneath `XDG_STATE_HOME` (the image sets it to `/vault/auth`) | the directory holding `keys.json` and `totp.key` (`AUTH_KEYS_PATH`), neither group- nor world-writable; file-watched |

A merchant renames at most every 72 hours, and a former name forwards and stays reserved for 90 days.

**The admin console**

| Key | Meaning |
|---|---|
| `admin_console.disabled` | omit the console at `/admin` (an API-only server) |

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
| `vault.ca_cert` | the PEM file of the CA that signed Vault's certificate (`VAULT_CACERT`); set, it is the only trust |
| `vault.kv_mount` | a KV v2 mount: Vault holds merchant configuration ([Vault](#vault)) |
| `vault.scope_prefix` | the path under the mount: `openrails` by default |
| `vault.transit_mount` | the Transit mount for Solana crankers: `transit` by default |
| `vault.namespace` | a Vault Enterprise namespace |
| `vault.auth_method` | `kubernetes`, `approle` or `token` |
| `vault.k8s_role` | the kubernetes auth role |
| `vault.role_id`, `vault.secret_id` | AppRole credentials |
| `vault.secrets_path` | a directory whose `VAULT_SECRET_ID` file is re-read at every re-authentication |
| `vault.token` | a token (development, or one a sidecar manages; `VAULT_TOKEN`) |
| `vault.auth_mount` | where the auth method is mounted, without `auth/`; default its name |
| `card_vault.tenant_id` | the operator's platform card vault: its Basis Theory tenant |
| `card_vault.kv_mount` | where its credential document lives (`platform_vault/<tenant_id>`); default `vault.kv_mount`. Setting only this keeps merchant configuration in files |
| `solana.crankers` | the deployment's Solana crankers, in order: `{mode: vault_transit, key}` (the key never leaves Transit) or `{mode: local_keypair, private_key}`. The first publishes new plans; each pulls the plans it owns; none turns Solana rebilling off. `SOLANA_CRANKERS` takes the list as JSON |
| `hyperswitch.api_base_url`, `hyperswitch.sdk_url` | a deployment's HyperSwitch custodian endpoints; http only on a loopback IP literal, a fixture refused beside a live PSP |

**Abuse limits**

| Key | Default | Meaning |
|---|---|---|
| `rate_limits.purchase.requests_per_minute` | `10` | buying: checkout and paying an invoice ([abuse limits](#abuse-limits)) |
| `rate_limits.payment.requests_per_minute` | `40` | a customer's payment-method routes |
| `rate_limits.subscribe.requests_per_minute` | `20` | a customer's subscription changes |
| `rate_limits.webhook.requests_per_minute` | `1200` | provider callbacks |
| `captcha.provider`, `captcha.site_key`, `captcha.secret_key` | `turnstile` | both keys set turn captcha on; `turnstile`, `recaptcha-v3` or `hcaptcha` |
A bucket set to 0 is unlimited: a deployment whose gateway limits sets every bucket to 0. Captcha is on only with its keys. The customers' own cancellations and card removals, and staff destructive actions, have fixed limits ([abuse limits](#abuse-limits), [destructive holds](#destructive-holds)). The example file sets the shipped limits explicitly:

```yaml
rate_limits:
  purchase: {requests_per_minute: 10}
  payment: {requests_per_minute: 40}
  subscribe: {requests_per_minute: 20}
  webhook: {requests_per_minute: 1200}
```

## The merchant manifest

The manifest is `version: 1` and `merchants.<name>`, each entry a merchant
document ([its fields and every rail's PSP](client.md#merchant-configuration-and-psps))
plus, on the server, `remote_application`: the app's issuer (`issuer` with
`jwks_uri`, inline `jwks` or `public_keys`), a trusted issuer in the merchant's
group holding its owner role ([trusted issuers](#trusted-issuers)).

`run-server` and `run-worker` read it at every boot: `--merchant-manifest PATH`
(an explicit path must exist), else `/etc/openrails/merchants.yaml` when present;
none boots control-plane-only. At boot:

1. The manifest parses strictly: unknown fields, settings and PSP declarations the configuration API would refuse, and secrets for an undeclared PSP refuse boot.
2. Missing merchants are created and each PSP's and custodian's identity is
   recorded; PostgreSQL keeps only those identities, never configuration.
3. A document the merchant still cannot serve is refused alone and reported (`openrails_configuration`); the merchant serves the rest.
4. Each PSP's environment is verified with its provider in the background
   before it arms; the server never waits on a provider to start.

Without Vault the file is the configuration, held in memory with its
credentials: the configuration routes read it (never credentials) and the edit
routes are not mounted. Change it by editing the file and restarting.
`openrails get-merchant-config` and `apply-merchant-config` read and apply
metadata by revision where Vault holds it; `push-merchant-config --insert`
creates the merchants a manifest names; `dump-merchant-config --slug` prints a
merchant's configuration in the manifest's shape, without credentials.

**Secrets.** The manifest is one file. Keep PSP secrets out of a committed copy: put them in Vault (`vault.kv_mount`), or keep the whole manifest, secrets included, in a secret store and mount the file from it (the chart's `secrets.merchantManifest`). To rotate a credential in the file, update the secret and restart.

## Secrets

Nothing in `config.yaml` needs to be secret: every secret can come from a
mounted file or the environment, named like its key's variable.

| Secret | Where it goes |
|---|---|
| database URL | `DATABASE_URL`, and `DATABASE_SESSION_URL` behind a transaction pooler (with `sslmode=verify-full` across a network) |
| Redis | `REDIS_URL`, `REDIS_CA_CERT` |
| ClickHouse | `CLICKHOUSE_URL` |
| SMTP | `EMAIL_SMTP_PASSWORD` |
| Vault | `VAULT_TOKEN`, or `VAULT_SECRET_ID` re-read from `vault.secrets_path` |
| Solana local keypairs | `SOLANA_CRANKERS` |
| AuthKit's signing key and TOTP key | `keys.json` and `totp.key` under `auth.keys.path` |
| PSP and custodian credentials | Vault, or the merchant manifest mounted from a secret |

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

`vault.kv_mount` makes Vault the only merchant configuration source. Staff then edit configuration over HTTP (`PATCH /v1/merchant/configuration`, the `/v1/merchant/psps` and `/v1/merchant/alert-webhooks` routes) or in the console, starting from an empty configuration, and automation may write the documents directly. Beside Vault a manifest names the merchant only (its slug and `remote_application`): configuration it declares is refused and reported, naming what it declares, and Vault's serves. There is no import or seeding step.

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
vault kv put -mount=kv openrails/merchants/$id/psps/nmi-main @nmi-main.json
```

**Revisions.** A document's KV version is its revision. Edits through OpenRails write with check-and-set, and `expected_revision` refuses an object changed since (`409 revision_mismatch`). An edit through OpenRails reaches every replica at once (PostgreSQL `NOTIFY`); one made in Vault directly is reloaded in the background by the merchant's first use 30 seconds or more after a replica last loaded it, and served from then on. A document that fails validation is not served: the version before it keeps serving, the `openrails_configuration` probe names it and a `consistency.merchant_config.not_served` finding stays open. Credentials are written, never read back over the API.

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
With an event store attached it also queues `openrails.merchant_usage_purge`,
which deletes the merchant's usage there and retries until the store takes it.

## Auth and trust

The server runs its own AuthKit. It decides who every request is: the server's own accounts and API keys, and the access tokens trusted issuers mint for the server's resource. OpenRails then binds the credential to one merchant and checks the route's permission; nothing in a request selects the merchant, and a valid credential bound to none is `403 merchant_unresolved` ([credentials](client.md#credentials)).

Who signs in, and how, in each mode, with the flows step by step: [authentication](authentication.md).

### The server's AuthKit

OpenRails builds AuthKit's configuration (`controlplane.AuthKit`) from settings it already has; `auth.keys.path` is the one a deployment sets:

| AuthKit | From |
|---|---|
| issuer, resource identifier (RFC 8707) and its public URL | `public_url`: https, or http on a loopback host |
| tables and jobs | `<database.schema>_auth`, and the engine's River schema `<database.schema>_river` |
| client IP | `trusted_proxies` and Cloudflare's published ranges |
| JSON API | `/auth/v1` beneath the issuer's origin, or `/v1` beneath an issuer with a path |
| emailed and logged links | the console's `/admin/invite`, `/admin/reset`, `/admin/verify`, `/admin/passwordless` and `/admin/setup` |
| keys on a private network | allowed for a loopback issuer and for the applications the operator declares (the merchant manifest's `remote_application`, the bootstrap manifest); an application registered at run time names only https key URLs on public hosts |

The server's own accounts are staff. The first is the owner the setup link creates ([first run](#first-run)); the rest join by invitation (registration `invite_only`). They need a second factor (a passkey or an authenticator app, whose secrets `totp.key` encrypts) and stay signed in for 30 days; passwords follow AuthKit's policy (8 to 128 characters, common ones refused). Every sign-in binds its session to the browser's DPoP key (RFC 9449): its access tokens go with a proof of that key and are refused as bearer tokens. Readiness fails while staff cannot enroll a second factor: with no `totp.key` the log names the file to mount. The server fixes the merchant roles, the API-key prefix, the scopes `openrails:merchant` and `openrails:self` and the console's client `openrails-console`; a token's `aud` is its issuer, and lifetimes and rate limits are AuthKit's.

A hosted product built on `server` sets its own account policy (open registration with `MerchantCreation`, agreements, usernames, group OAuth clients, SMS countries, AuthKit's opt-in route groups) in code, with `server.Config.AuthKit`: it adjusts the derived configuration before the fixed parts apply.

### The account directory

The server's AuthKit is single-tenant: `openrails migrate up`, or the first boot, creates its one operator account directory on a fresh database and adopts the one an existing database holds; a database with several refuses to start. Nothing names it in configuration.

### Trusted issuers

The server accepts RFC 9068 access tokens (`typ: at+jwt`) minted for its resource, `public_url`, by its trusted issuers: AuthKit remote applications in a merchant's group, from the manifest's `remote_application` (holding the merchant's owner role) or registered at run time, and root's, from the bootstrap manifest. One issuer acts for one merchant. A token's permissions count within its application's role and its scopes:

| Scope | Grants |
|---|---|
| `openrails:merchant` | merchant permissions, within the application's role: your backend |
| `openrails:self` | none: a customer's own billing at `/v1/me`, which refuses any other token of a trusted issuer (`403 permission_required`) |

When your app runs AuthKit, it is the authorization server: it registers the server as a resource, a public client for the browser's token exchange and a confidential client for the backend's client credentials; the same exchange gives your staff their tokens for the [admin screens in your own site](#admin-screens-in-your-own-site). In the example's app:

```go guide:standalone-issuer
// OpenRails is a resource server this AuthKit mints tokens for.
AuthorizationServer: authkit.AuthorizationServerConfig{
	Resources: []authkit.ResourceServerConfig{{
		ID: s.OpenRailsURL, // the tokens' aud
		// A customer's own billing; the merchant API.
		Scopes: []string{"openrails:self", "openrails:merchant"},
		// The most a token may carry.
		Permissions: backendPermissions,
	}},
	Clients: []authkit.OAuthClientConfig{{
		ID:         webClient, // public: the browser proves a DPoP key instead of a secret
		Origins:    []string{s.PublicURL},
		Resources:  []string{s.OpenRailsURL},
		GrantTypes: []authkit.OAuthGrantType{authkit.GrantTokenExchange},
	}, {
		ID:           backendClient, // confidential: this server
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
| create invoices | `merchant:invoices:manage` |
| read the event log | `merchant:events:read` |
| read or act on customers | `merchant:customer:read`, `merchant:customer:manage` |

Root operators hold the `root:` names, plus `billing.RootMerchantsRead`,
`billing.RootMerchantsDelete`, `billing.RootMerchantsRestore` and
`billing.RootWorkerHealthRead` for the server's merchant directory and worker
health. A person's token is
refused on `/v1/app` (`403 application_required`).

### The console

`/admin` is the staff console. Staff sign in with the server's own accounts, AuthKit's sign-in form, and with nothing else. A merchant's own staff can instead use the admin screens in its own site, signed in there ([below](#admin-screens-in-your-own-site)).

The console exchanges the session's token for one bound to the chosen merchant (RFC 8693 through the server's `openrails-console` client), refused where the person holds no role there and no root role. The session keeps the access token in memory and a rotating HttpOnly refresh cookie (`__Host-authkit_rt` on HTTPS) that ends 30 days after sign-in; a revoked session answers `401 credential_revoked` at once.

Operators invite operators on **Team** (`/admin/team`): root's people, an invitation by email with a root role. Where `email_smtp` is unset the invitation is not sent and Team shows its link to share. The invitee opens it at `/admin/invite`, registers and proves the address with an emailed code (so someone new needs `email_smtp`), or accepts it into the account they hold. The console shows `/admin/setup`, `/admin/invite` and Team only where the server's AuthKit serves its people and groups (`GET /auth/v1/capabilities` `route_groups` names `groups`).

The flow is in [authentication](authentication.md#standalone-the-servers-own-accounts). `admin_console.disabled` omits the console.

### Admin screens in your own site

A merchant's staff can work in your own admin pages, signed in to your app, with no second sign-in and no redirect. Your page renders billing-ui's `BillingAdmin` ([staff administration](../web/billing-ui/README.md#staff-administration)) and it calls the server's staff routes (`/v1/admin`, `/v1/catalog`, `/v1/merchant`, `/v1/metrics`) cross-origin, which answer any origin without credentials ([CORS](#tls-proxies-cors-and-webhooks)).

1. Declare your app the merchant's trusted issuer: `remote_application` in the merchant manifest, or one registered at run time. Its role there, the merchant's owner, is the most its tokens may do.
2. Your app gives the signed-in staff member an access token for the server: an RFC 9068 access token (`typ: at+jwt`) from your issuer, its `aud` the server's `public_url`, its `scope` including `openrails:merchant`, and the person's `merchant:` permissions in `permissions` (or `roles` the application's role map names). With AuthKit, register the server as a resource with `openrails:merchant` and your browser client exchanges the person's session for it (RFC 8693, resource indicator RFC 8707), as for customer tokens.
3. Pass `BillingAdmin` the server's API base (`<public_url>/v1`) as `baseUrl`, the merchant as `merchant`, and an `auth` whose `authFetch` sends that token.

There is no exchange at the server: the token is bound to its issuer's merchant already. Each permission counts within the application's role; a `root:` permission never counts, another merchant's records are not found, and a request naming another merchant in `OpenRails-Expected-Merchant` is `412 merchant_mismatch`. The flow is in [authentication](authentication.md#standalone-in-your-own-site).

### Customer contacts

The server mounts none of OpenRails' own SCIM routes (`/v1/app/scim/v2`, [client](client.md#customers)); AuthKit's directory at `/directory/scim/v2` is mounted instead. A customer of the server's own accounts is read from them; a trusted issuer's customer from the merchant's directory in the server's AuthKit, which the issuer pushes to over SCIM at `/directory/scim/v2` with a directory key (an API key of the merchant's group bound to its application, `provisions_for` on `POST /auth/v1/groups/{merchant_id}/api-keys`) or its own client-credentials token, and from the verified contact claims of its tokens (an AuthKit issuer adds `email` to a token asked for with the `email` scope). Each customer is read from its own issuer's directory, newest write winning. A customer your backend names in an assertion (`CreateCustomerToken`) is the merchant's own, and its contact is what that application pushed or asserted for it. OpenRails emails a customer only at an address the issuer proved.

## The payment page

An invoice's pay URL is a page on an origin of its own, `/pay/<invoice>#<secret>`,
where the customer pays that one invoice. The server serves it at
`checkout_origin` (the engine's `Config.CheckoutOrigin`), answering by `Host`: point
that origin's DNS and TLS at the same server. The checkout origin has no path
and must differ from `public_url`, the server's issuer and the
frontend's origin: the page runs the PSPs' scripts and never shares an origin
with the API or the console. Boot refuses otherwise. It is needed only for this page: with the embedded checkout or your own card form leave it unset, and an invoice asking for a pay URL is `400 pay_url_unavailable`.

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
the checkout origin. Give one public origin to `public_url`, which is also the server's issuer and resource, and another to `checkout_origin`.

- **Client IP.** The client is the socket peer unless `trusted_proxies` lists the proxy's CIDRs; then `X-Forwarded-For` is walked right to left past trusted hops. Without it rate limits, abuse tracking, request logs and webhook source addresses all see the proxy. `trusted_proxies` also decides which sender address a CCBill webhook is checked against: any peer it lists can name any address in `X-Forwarded-For`, CCBill's included. List only the proxy itself, never a pod or node network other workloads share. AuthKit takes the same proxies. Cloudflare's published ranges are built in and trusted like `trusted_proxies`, and their peers alone are believed for `CF-Connecting-IP` (AuthKit's client IP) and `CF-IPCountry` (the checkout's country); Cloudflare overwrites both, so this holds also where it does not front the origin.
- **Body size.** Requests are capped at 1 MiB (`413 request_body_too_large`),
  but a billing archive import (`POST /v1/merchant/billing-archive`) streams up
  to 1 GiB: raise the proxy's limit if you import over HTTP.
- **CORS** is fixed: the API, the staff routes included, answers any origin without credentials
  (`Access-Control-Allow-Origin: *`, `WWW-Authenticate` exposed, 12 h max-age);
  the console, webhooks and the server's AuthKit routes send no CORS headers.
  OpenRails sets no cookies on the API.
- **Webhooks.** Each PSP posts to `/v1/webhooks/{rail}/{account_id}`: the rail
  (`nmi`, `stripe`, `ccbill`, `basistheory`), never a PSP key, and the PSP's
  account id. The account resolves the merchant and its own secret verifies the
  request. NMI and Stripe sign; CCBill signs nothing and is accepted only from
  its published ranges, and, when its PSP declares a `webhook_token`, only at
  `/v1/webhooks/ccbill/{account_id}/{token}` ([CCBill](client.md#ccbill)).
  Request logs show that path's token as `{token}`. Your proxy, ingress and CDN access logs record the full path: mask the token segment there too, since only OpenRails' own request log masks it. Without a token, or with `require_webhook_token: false`, the source-address check is all that stands, and anything that can set `X-Forwarded-For` through a trusted hop can forge it (Cloudflare's published ranges are trusted by default, so a request sent through Cloudflare may name a CCBill address): keep the transition short. Stripe endpoints are registered automatically once
  `public_url` is public https and provider writes are allowed.
  Per-rail setup: [PSPs](client.md#merchant-configuration-and-psps).

One API host serves every merchant. `billing.ReservedMerchantSlugs` lists names a
hosted product should not let merchants claim.

### Network egress

Besides the hosts you configure (database, Redis, Vault, SMTP, trusted issuers' JWKS):

| Destination | For |
|---|---|
| `latest.currency-api.pages.dev`, then `cdn.jsdelivr.net` | FX rates, every 2 hours from an instance's first cross-currency quote; a failure fails only that quote |
| `hermes.pyth.network` | Solana token prices |
| `api.stripe.com`; `secure.nmi.com`, `secure.networkmerchants.com`, `sandbox.nmi.com`; `api.ccbill.com`, `sandbox-api.ccbill.com`, `datalink.ccbill.com` | PSPs |
| `api.basistheory.com`, `cdn.basistheory.com` | Basis Theory custodians |
| `api.mainnet-beta.solana.com`, `api.devnet.solana.com`, a PSP's `rpc_url` | Solana RPC |
| `challenges.cloudflare.com`, `www.google.com`, `hcaptcha.com` | captcha, when set |

## Event store

Usage needs an event store on ClickHouse; money stays in PostgreSQL. Every usage item is a raw event there (kept 90 days: what was reported, its identity and its priced meter cells), with the rollups limits, balances and history read (limit windows, pricing periods, minutes, and history by UTC hour, kept with no TTL and grouped into the merchant's `accounting.timezone` days as it is read). PostgreSQL keeps what the ledger took (`usage_postings`), what usage it has not taken yet costs at its provisional prices (`usage_pending`, `usage_pending_amounts`: the balance deducts it at once) and the rate cards usage was priced under (`usage_card_prints`). Usage posts once a day, after the day ends in the merchant's `accounting.timezone` (UTC by default), in the billing pass (`openrails.usage_posting`), before a usage invoice and before a credit lot's expiry, revocation or refund. The exact price of a period is read from ClickHouse when it posts: back it up nightly ([backup and restore](#backup-and-restore)).

Without an event store (`clickhouse.url` empty, or the default one unreachable at boot) the server runs subscriptions, purchases, checkout, entitlements, credit packs and balances, running charges, usage invoices, customer messages and the books; the usage routes stay mounted and answer 409 `event_store_required`, catalog documents and edits declaring meters, limits, a product's `limits` or `rate_cards` are refused, and so are the settings only usage reads (spend windows, failed-usage grace and cutoffs). Meters and limits already stored stay dormant: they are named in one log line at boot and in a `consistency.dormant_usage` finding, and the next catalog document applied removes them. A database with usage the ledger has not taken starts without ClickHouse: only the store can post that usage, so each merchant with some gets a `consistency.pending_usage` finding, and the customers it belongs to read their balances, pending usage included, but their usage cannot post (`event_store_required`), so their lots cannot expire, be revoked or refunded, until the store is attached; every other customer bills as before. Replicas starting together take turns migrating ClickHouse under a PostgreSQL advisory lock.

The first boot with ClickHouse records the store's identity in PostgreSQL. A later boot against a ClickHouse with another identity, an empty or different one, is refused: restore the event store's backup instead. Where none exists, `openrails usage attach --actor NAME --reason TEXT` records the configured ClickHouse as the store; with usage pending it also needs `--discard-pending`, which deletes it and records each merchant's discard as a `usage_discard` maintenance run. While ClickHouse is out of reach, syncs answer 503 `service_unavailable` (gates spend their offline share), and pending usage cannot post; balances read and move as before.

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
- **Identical everywhere:** schemas, merchant configuration (the same manifest, or Vault mount),
  `rate_limits`, `captcha`, `trusted_proxies`
  (a route some instances lack is a 404 on them), the signing key and `totp.key`
  (generated development keys are one instance only), the captcha
  secret, and the version outside a rolling upgrade.
- **Background work:** every instance with workers takes River jobs, each job on
  one instance. River's leader alone schedules periodic work; a crashed leader
  is replaced within about 20 seconds. Each instance works up to 20 billing
  jobs at once, plus the `psp_import`, `provider_refresh` and `nmi_history` queues. Outside
  River every instance runs the Solana Pay poller (leased claims) and checks its
  PSP credentials at start, on change and before a write once its verdict is an
  hour old.
- **Caches:** catalog, merchants, merchant controls and access are
  read per request. Merchant configuration is cached per instance (see
  [Vault](#vault)). An FX rate published more than 48 hours ago is never quoted.
- **Connections:** size PostgreSQL `max_connections` for every instance's two pools (the engine's and its AuthKit's, of the same size), its lease pool of 4, and one connection per instance for each listener: River's, unverified subscriptions, Solana Pay, customer messages, dispute responses, MRR risk changes and, with Vault configuration, configuration edits. Behind PgBouncer in transaction mode set `database.session_url`: the engine's pool and lease pool go through the pooler, and the listeners, session locks, migrations and AuthKit's pool, which hold sessions, connect directly. Without it, a pooler in front of the database runs in session mode. The billing pass and merchant pulls hold lease rows, not sessions.

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
- **Merchant controls.** `billing.merchant_controls` holds one merchant's writes ([merchant controls](#merchant-controls)). A billing export leaves its source readonly and an import lands readonly and disarmed until `openrails merchant writes full --merchant NAME --by NAME --reason TEXT`.

Move a merchant only by a billing archive ([backup and restore](#backup-and-restore)),
and never restart a stale source as a rollback.

## Upgrades and migrations

`run-server` and `run-worker` migrate OpenRails', AuthKit's and River's schemas
under an advisory lock before serving: one instance migrates, the rest wait, and
a failed migration fails the start. `openrails migrate up` runs the same step on
its own, as a deploy step; `openrails migrate status [--json]` compares the
applied ledger with the binary's chain and exits non-zero on any mismatch. With ClickHouse, the same step creates or upgrades the event store's database, under the same lock.

The schema is one baseline migration and numbered migrations after it, each an
addition, so the previous build runs on a schema the next one migrated and
rolling upgrades are safe. Released migrations never change: a database that
applied a migration whose file differs from the binary's by even a byte is
refused, and nothing is applied. Before v1 a release
may break the schema in one step: its release notes say so, the previous build
does not run on the migrated schema, there is no rollback past it, and every
host sharing the schema upgrades together. Migrations are never reversed.

From v1.0.0 the Go API, the HTTP API and the schema change only by addition
([compatibility](../compatibility/README.md)).

## Health and monitoring

| Signal | Meaning |
|---|---|
| `GET /` | a summary, always 200: `service`, `version`, `status` (`ready` or `not_ready`, as `/health/ready` would answer) and `links` to `/v1`, `/v1/config`, the console (`/admin/`, null when disabled) and the health routes. Public, and says no more than `/health/ready` |
| `GET /health/live` | the process serves HTTP; only after PostgreSQL answers and migrations finish, so allow a long startup probe |
| `GET /health/ready` | PostgreSQL, the merchants service, the River producer, a local River consumer, and a second factor staff can enroll (`totp.key`). Redis, Vault, PSP posture and refused configuration are degraded, never failing. Public and detail-free: a failure is logged and answers 503 |
| `GET /metrics` on `metrics_port` | the operator's Prometheus telemetry, on its own listener so it is never public (unset serves none; the chart enables it on 9090 with its own ClusterIP Service): `openrails_dependency_up{dependency,class}`, `openrails_abuse_state_fallbacks_total` and, per job kind, `openrails_job_overrunning`, `openrails_job_retryable`, `openrails_job_discarded` and `openrails_job_last_completed_timestamp_seconds`. Not the merchant dashboard's business metrics, which are the `/v1/metrics` API on `port`. `Server.MetricsHandler` mounts it in your own binary |
| probes | `openrails_psp_posture`, `openrails_configuration` (what is not served and why), `openrails_solana_crankers` and `openrails_job_progress` |
| `openrails workers [--json]` | each job kind as River holds it: overrunning, retrying and discarded jobs, last completion and the newest error |
| logs | to stderr at `logger.level`: JSON, or text on a terminal. Secrets, card data and SQL arguments are never logged |

`openrails_job_progress` fails with no River leader, an overrunning job, or a billing pass that has not finished within a day or left critical work (usage postings, renewals and retries, balance watches, delinquency, invoice expiry, usage billing) due since before the start of yesterday (UTC). Fleets sharing a database each answer for the passes they queue (the fleet holding the pass's lease in `periodic_leases` queues it), and every fleet fails when none has queued one within a day.

On SIGTERM the server fails readiness at once, keeps serving for 5 seconds,
then stops within 20 seconds; give it a grace period of at least 25 seconds
(Kubernetes' default is 30). `run-server --no-workers` serves but never becomes ready;
`run-worker` has no listener.

Healthy is a near-empty `openrails intents`, no open findings and no overrunning
or discarded jobs. [River jobs](river-jobs.md) lists every job kind, what queues
it and how to inspect one; the [operations inventory](operations-inventory.md)
lists each provider call and local change behind them.

## Merchant controls

Each merchant has three operator controls, one row of `billing.merchant_controls`; a merchant without one writes `full`, rebills released and destructive actions disarmed. One merchant's controls never hold another's. Every command names one merchant (`--merchant NAME`) or every merchant in the directory (`--all-merchants`), and requires `--by NAME` and `--reason TEXT`; each change is a `controls` entry of the merchant's configuration history (`GET /v1/merchant/changes`), and a control already at its value records nothing.

| Command | Control |
|---|---|
| `openrails merchant controls [--merchant NAME]` | show every merchant's controls, or one's, with the last change |
| `openrails merchant writes full\|limited\|readonly` | how much OpenRails writes to the merchant's providers |
| `openrails merchant rebills hold\|release` | whether the charges OpenRails initiates wait |
| `openrails merchant destructive arm\|disarm` | whether destructive convergence runs |

**Writes.** In a copy of the billing book every merchant is readonly whatever its writes say ([one billing database](#one-billing-database)). A billing export leaves its source readonly and an import lands readonly and disarmed.

| Operation | `full` | `limited` | `readonly` |
|---|---|---|---|
| customer purchase, card save, tier change, resume, refund | yes | yes | refused |
| customer or staff cancel, and its provider delete | yes | yes | intent parks |
| dunning charges and window-expiry cancellations | yes | intents park | no |
| usage-invoice collection, Solana pulls | yes | no | no |
| `catalog apply` publishing a recurring Solana plan | yes | binds an existing plan, else refuses the price | as `limited` |
| `catalog apply` local changes, provider reads, webhooks | yes | yes | yes |

`limited` stops what the system initiates; `readonly` stops the wire, enforced in each rail's transport. Verified receipt recovery runs under every setting. Every subscription has one collector, fixed when it is created or imported: `engine` (OpenRails charges a saved card or pulls on chain, and retries declines), `nmi_schedule` (NMI charges, OpenRails retries) or `provider` (Stripe or CCBill charge and retry). Configuration never moves a subscription to another collector.

Each PSP's environment decides whether money is real, and the provider must agree before it arms; no setting redirects a provider, and a non-live PSP relaxes no other protection.

**Rebills.** Held, the charges OpenRails initiates for the merchant wait: engine renewals (with any scheduled plan change they apply), dunning retries, NMI recurring recovery, scheduled usage collection and Solana renewal pulls. The due pass does not select them and an accepted one parks unsent; access is kept as for any renewal that has not been attempted. Released, they run at the next billing pass. What the customer starts never waits: a purchase and its first charge, an immediate plan or seat change, paying a renewal or usage invoice now. Refunds, card refreshes and deletes follow writes, not rebills. A hold longer than a whole period leaves that period to the operator, as any missed period is.

**Importing a merchant with production credentials.** Hold it before its subscriptions arrive: imported `past_due` subscriptions are due at once.

1. Provision the merchant, then set its writes `limited` (the site works; system writes park) or `readonly` before importing.
2. Let provider reads catch up. Dunning materializes in-window charges as parked `manual_rebill` intents and parks window-expired cases `unverified`; it cancels nothing.
3. `openrails intents --merchant <name>` prints the drain forecast ("N execute under limited, M require full"). Resolve what should not fire.
4. Review `GET /v1/merchant/findings` and `openrails proposals list`.
5. `openrails merchant writes full`. Missed whole periods are never back-billed.

**Destructive actions.** Disarmed (the default for every new merchant), the merchant's provider pulls run advisory: findings are stored, nothing changes, and the first completed pull stamps `first_pull_completed_at`. No cancellation, revocation, provider delete or pull overwrite runs except a customer's own cancel; detection, findings, additive repairs and notices run either way. A held retraction stays `reconcile_required` with `held_by: kill_switch`; a provider-confirmed cancel it holds parks the subscription `unverified` with access kept, until a targeted verification or an operator refresh applies it once armed. A rollback of a destructive run disarms its merchant. Review the first pull, then arm:

```sh
openrails merchant controls --merchant shop
openrails merchant destructive arm --merchant shop --by you --reason "reviewed first-pull findings"
# stop one merchant, or every merchant, now; parked intents resume when it is armed again
openrails merchant destructive disarm --all-merchants --by you --reason "incident: mass cancellation observed"
```

A held cancel logs `convergence cancel held; the subscription keeps its access`. Every destructive act reads the control first, so a change reaches every instance at its next check. Until the merchant is armed a staff cancel of an NMI- or CCBill-owned membership is `409 provider_cancel_held`: OpenRails will not cancel locally while the provider would keep charging. A customer's own cancel is never held: the provider stops billing and the subscription ends at period end, refused only by the [customer budget](#abuse-limits).

**Cancellation caps.** A provider read, or a billing import's cancels of subscriptions it did not create, may cancel at most `min(25, max(3, 5% of the PSP's live linked book))` subscriptions per PSP, or the whole book when it has at most 5. Over that it cancels none, halts the merchant's pass and raises `pull.cancellation.capped`; a held import cancel keeps the subscription `unverified` with access. The usual causes are a misdeclared `account_id`, a credential rotated onto a sibling sub-account, or a provider returning a short page.

## Destructive holds

Staff refunds, cancellations, payment-method removal, access and credit revocation, write-offs and substantive catalog or configuration changes share one admission policy. It counts each staff member's and application's logical actions (affected targets in a batch, not requests), keyed on the verified issuer and invoker: rotating a credential creates no new allowance. A billing import is a merchant-level operation: its cancels, the terminal declines it lands and the provider stops it owes a canceled subscription are `cancel_subscription` actions recorded in its caller's name and admitted together before any lands, against the merchant's and the deployment's budgets rather than the caller's; a book larger than what they have left is refused with 429 and `Retry-After`, opening no incident. Replays and proven no-ops spend nothing; creating a catalog object spends nothing; a file applied with `openrails catalog apply` spends a separate automation budget of its merchant. An in-process call that names no person is the host's own work: it is recorded, and spends no allowance.

The limits are fixed: 5 an hour and 15 a day per actor; per merchant at least 15 and 25, per deployment at least 50 and 100, both rising by one per 400 (hour) and 100 (day) customers active in the last 30 days. A legitimate bulk job that trips one asks staff to clear the hold.

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
| `purchase` | 10 | `POST /v1/me/invoices`, an invoice's pay and confirm, `POST /v1/invoices/{id}/solana-pay` |
| `subscribe` | 20 | the `/v1/me/subscriptions` writes |
| `payment` | 40 | `/v1/me/payment-methods` |
| `webhook` | 1200 | `/v1/webhooks/{rail}/{account_id}` |

Counting is per minute, per bucket and per client IP; a request over the limit is refused with `429 rate_limit_exceeded`, `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset` and `Retry-After`. Other routes are not limited here: a per-address ceiling belongs to your proxy. A pay URL is also limited per invoice (120 reads and 10 pays a minute). A declared `Content-Length` over 64 KiB is refused 413 before counting for purchase, subscriptions and payment methods; webhook bodies have per-rail caps.

- **Captcha.** With captcha on, a subject reaching three times a purchase, payment or subscribe limit in a window is challenged for 15 minutes: `403 captcha_required` with `X-Captcha-Required: true` until it sends a valid `X-Captcha-Token`. Staff, console and server-to-server routes never see a captcha.
- **Card testing.** A decline counts against the customer and the client address (IPv6: the /64). Before any provider call, a card save or payment is refused (`429 card_attempts_blocked`) when its customer or its address has 6 declines in 15 minutes (blocked for the window) or 10 in 24 hours (for the day): a customer with no declines of its own is refused from a blocked address. 100 declines in 24 hours from at least 25 customers and 25 addresses is attack mode for the merchant: with captcha on, every request to its card routes must solve one, and a single decline in the last 15 minutes, by the customer or from its address, refuses the attempt, so a customer without a recent decline is still refused from an address that has one.
- **Customers' own cancellations and card removals** share one merchant-wide budget, enforced even with every `rate_limits` bucket at 0: per rolling day the larger of 10 customers or 1 % of customers who paid in the last 30 days, per rolling hour the larger of 5 or one per 400 paying customers. Staff and host cancellations count under the [destructive holds](#destructive-holds) instead. A refusal is 429 with the wait, changes nothing, and records a finding that alerts staff, naming the wave's cancellations, card removals and start.
- **Admin lockouts.** A person making more than 100 staff grants in a minute, counted by item (credits, access, limit adjustments, customer updates), or more than 10 off-channel changes (recorded payments, subscription changes) is locked out of them for an hour; `openrails admin-lockouts unlock <user-id>` ends one (with Redis; without it a lockout lives in the serving process).

## Provider intents

Every outbound provider mutation is first a row in `billing.provider_intents`: an idempotency key, an origin (`user`, `admin`, `provider` for a provider-reported cancel, `system`), the PSP it targets and a relevance window. Its own job (`openrails.provider_operation`) claims it under a lease, rechecks relevance, gates on the merchant's write posture and the origin, executes and classifies the outcome; each attempt's phases (attempting before the provider call, then its result or a hold) are written ahead to `billing.operation_log`, the operations history. A blocked intent (provider down, write posture, credentials that do not arm) stays pending with its reason and drains when the blocker lifts; one past its relevance window is superseded or expires instead of firing stale. An ambiguous money movement is resolved by reading the provider before any retry; deletes verify, then execute.

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
receipt or provider-confirmed non-execution. For an NMI checkout sale, an empty
order search also allows release with an actor and reason attesting release
without proof of nonexecution. Such sales release automatically after 24 hours
from submission when the order search is empty. Both raise a high finding for
later reconciliation; the terminal intent cannot accept a later receipt. It never
resends. Webhooks are at-least-once and handlers idempotent;
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

Refresh reads run under every write posture. Overwrites and lifecycle repairs need the
merchant's destructive actions armed and its writes above `readonly`. Unverified
subscriptions are also checked without a refresh: the hourly billing pass
retries flagged ones against their own PSP in bounded batches.

| Provider evidence for an `unverified` subscription | Resolution |
|---|---|
| a verified renewal charge | active again, period advanced |
| live with a later billing date, no charge | its period end is taken; no access is granted |
| a decline inside the dunning window | `past_due` |
| canceled, expired or absent | canceled: NMI and engine schedules keep access to the paid date, Stripe and CCBill end it at once |
| inconclusive or unreachable | stays `unverified`, access kept |

NMI merchant-defined field 20 and Stripe `openrails_book` mark this book's
objects; an unknown object without the mark is a low finding
(`evidence.quiet_foreign`).

## Findings, proposals and undo

Writers commit a change and its effects together, and a customer's `openrails.customer_converge` job re-derives them; deadlines with no event are indexed due work in the hourly passes. Findings are `<plane>.<subject>[.<condition>]` in `billing.reconciliation_findings`: `pull.*` (a mirror row against the provider, repaired by an approved proposal), `derive.*` (a grant effect against its source), `life.*` (a record against its clock), `consistency.*` (duplicates, mismatches, unbooked events), `notify.*` (a notice not sent) and `catalog.*` (a parked subscription's unmapped price). A finding resolves itself when the divergence goes, and raises console notifications and alerts. Retractions from a provider read wait until the merchant's domain is proven fully reconciled (`billing.reconciliation_state`). Unlandable provider subscriptions park (`GET /v1/merchant/parked-subscriptions`) until their price is mapped or customer linked.

```sh
openrails proposals list    --merchant <name> [--psp KEY] [--kind K] [--source pull|verification]
openrails proposals show    --merchant <name> <fnd_id>
openrails proposals approve --merchant <name> <fnd_id>… | --psp KEY --kind K --expect N
openrails proposals dismiss --merchant <name> <fnd_id>… --notes TEXT
openrails proposals foreign --merchant <name> <fnd_id>… --notes TEXT
openrails proposals runs    --merchant <name>
```

Each proposal applies in its own transaction; a filtered set needs `--expect` equal to its size. One whose basis moved, or that ends access on evidence older than 7 days, is superseded. Disarmed destructive actions hold changes that end access or write to a provider (`destructive_actions_held`); readonly writes hold those that move money or write to a provider (`provider_writes_held`); each held proposal is reported in the results. Over HTTP: `/v1/merchant/proposals` (approving and dismissing need a person: `403 person_required`).

`openrails prune --merchant <name> [--psp] [--rail] [--since --until] [--expect-rows N]` retires the local subscriptions and payments a PSP no longer lists: without `--expect-rows` it is a plan; applying needs N equal to the plan, one PSP at a time, and an incomplete or empty provider read retires nothing. `prune` and approval runs reverse with one command:

```sh
openrails prune list --merchant <name>
openrails undo-run --merchant <name> --run <id>                          # plan
openrails undo-run --merchant <name> --run <id> --apply --expect-rows N
```

An undo restores rows in one transaction (a row changed since keeps its newer state), supersedes the run's unfired intents, lets convergence rebuild access, and reports what it could not undo: provider writes already sent. It never touches the ledger, grants or logs. Undoing an approval run also disarms the merchant's destructive actions until it is armed again ([merchant controls](#merchant-controls)).

## Retention

Periods are constants, the same for every merchant. Each table's comment states its class: **permanent** (the ledger, grants, payments, owed usage, usage postings, receipts, operation authorizations, refusals, running charges, maintenance runs, the before-images of destructive runs, MRR movements and scheduled MRR changes), **rows** (deleted by the hourly cleanup) or **state** (one row per thing that exists).

| Table | Deleted |
|---|---|
| `subscription_status_transitions`, `subscription_billing_changes`, `operation_log`, `payment_attempts`, `rebill_cycles`, `nmi_history_months` | 25 months |
| `provider_intents` | instruction-only intents 25 months after they last changed; the rest are permanent |
| `reconciliation_findings` | resolved ones, 12 months after resolved and last seen |
| `invoice_attempts`; `invoices`, `invoice_lines` | attempts that never reached a provider 90 days after `expires_at`; unpaid closed invoices with no attempt, 397 days |
| `parked_provider_subscriptions` | 90 days after release |
| `carts` | 30 days after `last_changed_at` |
| `notification_events`, `host_outbox`, the event log | 90 days |
| `webhook_events` | 90 days after completion |
| `customer_messages` | two years (731 days) after created |
| `idempotency_keys`, `solana_pay_references` | at expiry, after the 7-day watch |
| `user_destructive_reservations` | 24 hours |
| `limit_windows` | 61 days after `window_ends_at`; a lifetime or rate limit's row stays |
| `leases` (with their in-flight reports), `limit_adjustments` | 61 days after the last sync, or after the adjustment |
| `webhook_health_daily` | 13 months |
| `periodic_leases` | a kind unscheduled for 30 days |

The event store's tables expire by their own TTLs: raw usage events 90 days after they occurred, a pricing period's quantities 100 days after it starts, minutes after 2 days, and a limit window's usage 61 days after it ends (a lifetime limit's stays). Usage history, by UTC hour, has no TTL; a merchant purge deletes its rows.

The hourly cleanup deletes 1,000 rows per statement and at most 50,000 per merchant per pass. `subscription_status_transitions` and `subscription_billing_changes` refuse other deletes (`billing.guard_retention_delete`). What integrators see: usage may be dated up to 35 days back; a lease answers resends for 61 days after its last sync; usage reports reach back about 26 months.

## Backup and restore

Back up PostgreSQL and the merchant configuration (Vault with its `credential_fingerprint_key`, or the manifest), plus `keys.json` and `totp.key` in your secret manager. PostgreSQL holds no credential, so a database alone cannot charge anyone.

Turn on point-in-time recovery (`wal_level = replica`, `archive_mode = on`, an off-host `archive_command`, periodic base backups). PITR is whole-cluster: right for disaster, wrong for one damaged merchant. A restore never undoes what happened at a provider; it creates divergence that convergence repairs. Money is reversed by compensating entries, never deletion; the append-only tables (the ledger, grants, status transitions, the operations and configuration histories, `webhook_events`, findings) are restored whole or not at all.

1. Stop and drain the source writers and disable their restart.
2. Restore PostgreSQL; make the configuration available.
3. Start it. A restore into another cluster, database or schema stays readonly
   until `openrails book arm`, once the source is gone for good. Start
   `readonly` to inspect first.
4. Review unresolved findings: ignoring one does not settle it.

Drill restores into a scratch instance: migrations current, PSPs arm, a refresh
produces sane findings.

**ClickHouse.** The event store holds the usage whose exact price a posting reads (the daily posting, or a late item's later one), so back it up nightly to S3-compatible storage: `BACKUP DATABASE openrails TO S3('https://<bucket>/clickhouse/<date>', '<key>', '<secret>')`, incremental (`SETTINGS base_backup = S3(...)`) from a weekly full one, kept 8 days like PostgreSQL's window. A backup carries the store's identity: restore it (`RESTORE DATABASE openrails FROM S3(...)`) before starting OpenRails, and the boot finds the store its database recorded. Usage that arrived after the backup is lost with it; what posted is in PostgreSQL. With no backup, `openrails usage attach` records an empty ClickHouse ([event store](#event-store)).

**One merchant's archive.** `openrails billing export` writes a complete,
integrity-protected snapshot of one merchant's billing to a private file, with
every writer stopped (`--source-stopped` attests it), and leaves the source
readonly. `openrails billing prepare-target` provisions the exact merchant
identity at the destination; `openrails billing import --target-stopped` restores
into it and lands readonly and disarmed until `openrails merchant writes full`. Over HTTP the same
archive is `GET` and `POST /v1/merchant/billing-archive`. With an event store the
archive carries the merchant's usage from it, each table as stored: raw events
within their 90 days and every rollup. Restoring one
with usage needs an event store holding none of the merchant's; without a store
an export refuses a merchant with pending usage. The event log, active
destructive incidents and pending actions are not in an archive.

**Not backups:** the purge inventory (counts and document names only; after a
purge, PITR is the only way back), a provider read (providers hold no access,
grants or ledger) and `undo-run` (one operation).

**Deleting a merchant.** `openrails merchants delete` is soft: off the lists,
credentials resolve nothing, records kept; `merchants restore` brings it back.
`openrails merchant purge --merchant NAME --by NAME` records an inventory;
typing back its id, row count and confirmation phrase (`--inventory`,
`--expect-rows`, `--confirm`) with its destructive actions armed deletes the merchant's rows
permanently and queues its Vault cleanup. No deletion revokes cards at the PSP.

## Commands

The image is the CLI. Every command takes `--config`/`-c` (default
`config.yaml`). One-off commands open only what they
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
| `openrails setup-link` | print a single-use link, valid 24 hours, that creates an owner account, replacing a live one; works while operators exist (break-glass) |
| `openrails push-auth-bootstrap [--file] [--dry-run] [--startup-only]` | create AuthKit root authority from a bootstrap manifest (`/etc/openrails/bootstrap.yaml`); `--startup-only` applies it at most once, as `run-server` does with a mounted one |
| `openrails push-merchant-config [--file] --insert` | create the merchants a manifest names that are missing; bare, it only reports |
| `openrails dump-merchant-config --slug NAME [--out]` | print a merchant's configuration as manifest YAML, without credentials or alert webhooks |
| `openrails get-merchant-config --merchant NAME` | read a merchant's configuration through the client (remote: `--server-url`, `--token-file`) |
| `openrails apply-merchant-config --merchant NAME --file PATH` | apply configuration metadata by revision (Vault only) |
| `openrails catalog apply FILE --merchant NAME [--force-conflicts]` | apply a catalog document; startup never does |
| `openrails billing export`, `openrails billing import`, `openrails billing prepare-target` | move one merchant's billing ([backup and restore](#backup-and-restore)) |
| `openrails merchants list`, `merchants get`, `merchants create`, `merchants rename`, `merchants delete`, `merchants restore` | the merchant directory; delete is soft, a rename forwards the former name for 90 days |
| `openrails merchant controls`, `merchant writes`, `merchant rebills`, `merchant destructive` | a merchant's controls ([merchant controls](#merchant-controls)) |
| `openrails merchant purge` | record a purge inventory, or with it typed back delete the merchant permanently |
| `openrails book status`, `openrails book arm --by NAME` | the book identity |
| `openrails usage attach --actor NAME --reason TEXT [--discard-pending]` | record the configured ClickHouse as the event store in place of a lost one; `--discard-pending` discards usage not yet posted, recorded per merchant |
| `openrails provider-refresh --merchant NAME` | queue a provider refresh and print its job |
| `openrails intents`, `openrails intents resolve`, `openrails intents-log` | the provider intent ledger |
| `openrails proposals list`, `proposals show`, `proposals approve`, `proposals dismiss`, `proposals foreign`, `proposals runs` | review what provider reads propose |
| `openrails prune`, `openrails prune list`, `openrails undo-run` | retire what a PSP no longer lists; reverse a run |
| `openrails destructive-holds list`, `openrails destructive-holds clear` | destructive-action incidents |
| `openrails admin-lockouts unlock <user-id>` | end an administrator's lockout |
| `openrails workers [--json]` | job health from River's tables |
| `openrails ledger-audit [--merchant NAME] [--format json]` | check ledger conservation, account counters, credit lot counters, rollups and sign rules, and warn of a general-ledger total opposite its normal side; non-zero on a finding, not a warning. Nothing schedules it |
| `openrails nmi decline-report --merchant NAME --since DATE [--until] [--psp]` | read-only NMI approval and refusal rates by month and reason |
| `openrails solana-pay resolve --merchant NAME --signature SIG --resolution TEXT` | close a reviewed Solana Pay receipt |

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
  public_url: https://billing.example.com
  redis: {url: redis://redis:6379}
  email_smtp: {host: smtp.example.com, port: 587, username: openrails, from: "Billing <billing@example.com>"}
  trusted_proxies: [10.244.250.0/28]   # only the ingress controller's addresses (a range kept for it), never the shared pod CIDR
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
    DATABASE_URL: {name: openrails-db-app, key: uri}
  files: [openrails-secrets]         # EMAIL_SMTP_PASSWORD, REDIS_URL, …
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
| `config` | `config.yaml`, rendered as written; no secrets. Policy (`rate_limits`) goes here, never in `env`. `host` (rendered `0.0.0.0`), `port` and `metrics_port` are the chart's and refused here |
| `files` | more files in `/etc/openrails`: `merchants.yaml` (read at every boot; without secrets), `bootstrap.yaml` (first run), `catalog.yaml` (for `openrails catalog apply`) |
| `secrets` | `env` (one variable per Secret key), `files` (Secrets whose keys are variable names, mounted at `/vault/secrets`), `authKeys` (`keys.json` and `totp.key` at `/vault/auth`, reloaded on change), `merchantManifest` (`{name, key}`: a Secret key mounted as `/etc/openrails/merchants.yaml`, the manifest with its PSP secrets, in place of `files.merchants.yaml`) |
| `env`, `envFrom` | further environment |
| `ports` | `http`, default 3053 |
| `metrics` | the metrics listener, the operator's Prometheus `GET /metrics` (`enabled`, default true; `port` 9090, rendered as `metrics_port`), served by its own ClusterIP Service `<fullname>-metrics` whatever `service.type` is so it is never public, and an optional Prometheus Operator `serviceMonitor`. Not the `/v1/metrics` business metrics API |
| `service` | `type`, `port` (80), `annotations` |
| `ingress` | `enabled`, `className`, `annotations`, `hosts` (every path to the server), `tls` |
| `resources` | requests 100m and 256Mi, limits one CPU and 768Mi |
| `podSecurityContext`, `securityContext` | UID and GID 10001, non-root, read-only root filesystem, no capabilities |
| `startupProbe`, `livenessProbe`, `readinessProbe` | `/health/live` (startup allows 10 minutes for migrations) and `/health/ready` |
| `terminationGracePeriodSeconds` | 30: the server drains 5 seconds, then stops within 20 |
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

**Secrets.** `secrets.env` takes CloudNativePG's app Secret directly (`DATABASE_URL: {name: openrails-db-app, key: uri}`). With External Secrets Operator, name each target key after its variable; with the Vault Agent Injector render one file per variable into `/vault/secrets` with `podAnnotations` and leave `secrets.files` empty. For Vault-held merchant configuration declare `config.vault` with `auth_method: kubernetes`: the chart mounts the pod's ServiceAccount token; bind the role in `k8s_role` to the ServiceAccount the chart creates (`<release>-openrails`, the release name alone when it contains `openrails`, or `serviceAccount.name`).

```sh
kubectl -n openrails create secret generic openrails-auth-keys --from-file=keys.json --from-file=totp.key
kubectl -n openrails create secret generic openrails-secrets --from-literal=EMAIL_SMTP_PASSWORD="$SMTP_PASSWORD"
```

**PostgreSQL** needs version 18 and one role owning its database: a
CloudNativePG cluster (`imageName: ghcr.io/cloudnative-pg/postgresql:18`,
`bootstrap.initdb: {database: openrails, owner: openrails}`) writes the
`openrails-db-app` Secret whose `uri` key is `DATABASE_URL`. **Redis** or Garnet has no
default; its `REDIS_URL` (with credentials) and CA go in `secrets.files` (`REDIS_URL`, `REDIS_CA_CERT`).

**Probes and drain.** On SIGTERM `/health/ready` answers 503 at once while the
pod keeps serving for 5 seconds, so endpoints drop it before it stops
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
| boot refuses a key | its log names the key |
| `/health/ready` 503 | PostgreSQL, the merchants service, local workers or a missing `totp.key`: the log names it |
| a PSP never arms | the `openrails_psp_posture` probe: the provider's verdict for its environment, or credentials of another account (`consistency.duplicate_gateway_account`) |
| configuration not served | the `openrails_configuration` probe and `consistency.merchant_config.not_served` |
| 401 on a trusted issuer's token | its `iss` (a trusted issuer of that merchant), its `aud` (`public_url`), its signing keys' JWKS |
| `403 merchant_unresolved` | the credential is bound to no live merchant: fix the credential, nothing in the request changes it |
| 403 with a permission code | the role behind the key or the issuer's application lacks the route's permission |
| every client shares one rate limit | `trusted_proxies` is unset behind a proxy |
| webhooks refused | the URL's rail and account id, the PSP's signing secret, CCBill's source address and webhook token |
| `400 pay_url_unavailable` | `checkout_origin` is unset |
| renewals not charging | `openrails merchant controls` (writes, rebills), `openrails book status`, `openrails intents` |
| staff cancels refused with `provider_cancel_held` | the merchant's destructive arming (`openrails merchant controls`) |
| jobs stuck | `openrails workers`, `openrails_job_progress`, the River schema of every fleet |
| missed provider events | `openrails provider-refresh --merchant NAME` |
