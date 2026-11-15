# Configuration

Three inputs configure a deployment:

| Input | Owns | Read |
|---|---|---|
| `config.yaml` (standalone) or `openrails.Config` (embedded) | the process: database, Redis, `provider_write_mode`, Vault, route groups, the server's AuthKit | at boot |
| merchant configuration: the manifest (`--merchant-manifest`, default `/etc/openrails/merchants.yaml`) or `Config.Merchants`, or Vault | each merchant: name, settings, alert webhooks, PSPs and their secrets | at every boot (file), or live (Vault) |
| catalog documents | products, prices, entitlements, provider links | only when an operator applies one ([catalog](catalog.md)) |

[`config.example.yaml`](../config.example.yaml) lists every server key. Each key
also reads from the environment, its path upper-cased and joined by `_`
(`db.url` is `DB_URL`), or from a file of that name under `/vault/secrets`
(`VAULT_SECRETS_PATH`). Flags beat the environment, which beats files and YAML.
Libraries never read the environment: an embedded host builds `openrails.Config`
itself. Unknown keys and retired names (`env`, `api_url`,
`new_subscription_collection_policy`, the server's old `auth.issuer`,
`auth.keys_path`, `auth.request_origin`, `resource_server`) refuse boot and name
their replacement.

Security does not depend on PSP environments: live and non-live deployments
alike need encrypted managed database credentials, verified webhook signatures
and an explicit `provider_write_mode`.

## The server's AuthKit

Embedded billing is given the host's `Auth` where its routes mount and loads no
AuthKit configuration. The standalone server's `auth` section is AuthKit's own
configuration (`server.Config.Auth`) in AuthKit's keys; `AUTH_<PATH>` sets one
(`AUTH_TOKEN_ISSUER` is `auth.token.issuer`).

| Key | Meaning |
|---|---|
| `auth.default_tenant_id` | the server's own account directory; generate once and keep it ([auth](auth.md#standalone-account-directory)) |
| `auth.token.issuer` | required; the server's issuer. No billing URL stands in for it |
| `auth.keys.path` (`AUTH_KEYS_PATH`) | `keys.json` and `totp.key`; `AUTHKIT_ACTIVE_KEY_ID` / `AUTHKIT_ACTIVE_PRIVATE_KEY_PEM` / `AUTHKIT_PUBLIC_KEYS` give an inline key, fixed until restart |
| `auth.keys.allow_ephemeral_dev_keys` | generate keys under `auth.keys.path`; one instance, local only |
| `auth.keys.verify_only` | verify tokens, mint none |
| `auth.registration.native_user_mode` | local sign-in defaults to `invite_only`; `closed` disables enrollment; standalone refuses `open` unless a hosted product supplies `server.Config.MerchantCreation` |
| `auth.registration.allow_missing_senders` | run without email or SMS delivery |
| `auth.token.allow_private_network_jwks` | fetch a trusted issuer's keys from a private address (local federation) |
| `auth.resource.id`, `auth.resource.public_url` | the resource trusted issuers mint tokens for (the `aud`, RFC 8707) and where clients reach it |
| `auth.sign_in.dpop` | `optional` or `required` for the server's own users; validation is unaffected |
| `auth.http.direct_peer_ip` | clients connect directly; behind a proxy set `trusted_proxies` (or `cloudflare_proxies`) instead. Request headers never declare their own origin |
| `auth.http.rate_limits` | AuthKit's own buckets |

The server fixes the rest: merchant roles, the `openrails` API-key prefix, the
scopes `openrails:merchant` and `openrails:self`, AuthKit's River schema and
mount. `naming` is the rename policy for merchant names. The root owner needs a
second factor, so boot refuses without a 16, 24 or 32-byte `totp.key` or an email
or SMS sender.

In production drop every `allow_*`, use https for the issuer and resource, mount
a real signing key, and take database credentials from secrets. Redis takes
`redis.url` (`redis://`, `rediss://`), `redis.username`, `redis.tls` and
`redis.ca_cert`. `hyperswitch.allow_loopback_http` admits a loopback HTTP
fixture, refused beside a live PSP.

## URLs

- `public_billing_base_url`: the public billing mount without `/v1`
  (`https://shop.example/billing` puts callbacks under `…/billing/v1/…`). Needed
  only for external callbacks and billing links.
- `auth.resource.public_url`: the public origin DPoP proofs point at.
- `auth.token.issuer`: the server's own token trust, independent of both.
- `dashboard_base_url`: where links to the console go.
- A remote Client's server URL is only its transport destination.

## Rate limits

`rate_limits` overrides the built-in buckets (`purchase`, `payment`,
`subscribe`, `webhook`, `catalog-ask`; other keys are reported and ignored);
`rate_limits_disabled` says the host limits instead ([limits](limits.md)).

## Database schema

`database.schema` (`DATABASE_SCHEMA`, `Config.Database.Schema`, default
`billing`) holds every table, function and type; it must be a plain identifier.
SQL is authored in `billing` and runs there verbatim; another schema is reached by
one token-aware rewrite of schema qualifiers, `SCHEMA` names, `search_path` values
and `'billing.<table>'::regclass` literals, never other strings. River uses
`database.river_schema` (default the schema plus `_river`); AuthKit keeps its own.
`New` creates or upgrades them at boot. Its role owns every object: no grants, no
row-level security. The schema is listed in [`api/schema.txt`](../api/schema.txt).

## Merchant configuration in a file

Without a Vault KV mount, the merchant manifest (standalone) or `Config.Merchants`
(embedded) is the configuration: read at startup and held in memory, credentials
included. PostgreSQL keeps only the merchant and PSP identities history points at.
The configuration routes read it (never credentials); the edit routes are not
mounted. Change it by editing the file and restarting. To edit over HTTP, use
[Vault](vault.md). Field reference: [merchant configuration](merchant-configuration.md).

Keep secret values out of the committed file. Render them (Vault Agent, a
Kubernetes Secret, CSI) as YAML in the manifest's shape and list the files in
`merchant_manifest_overlays` (`MERCHANT_MANIFEST_OVERLAYS`, comma-separated):

```yaml
# /vault/secrets/merchants.yaml
merchants:
  myapp:
    psps:
      nmi-main:
        secrets: { security_key: ..., webhook_signing_secret: ... }
      ccbill:
        secrets: { datalink_username: ..., datalink_password: ..., salt: ... }
```

Overlays merge over the manifest in order, later wins. No live Vault connection
is needed (except Transit for Solana signing). At boot:

1. The merged tree parses strictly: unknown fields, retired keys and secrets for
   an undeclared PSP refuse boot.
2. Missing merchants are created and each PSP's and custodian's identity recorded.
3. Each document validates as the configuration API would.
4. A declared secret that resolves empty is an error, and each PSP's
   `environment` is verified with its provider before arming.

An absent default manifest boots control-plane-only; an explicit
`--merchant-manifest` path must exist. `run-worker` loads the same manifest and
overlays. To rotate a credential, update the secret source, re-render, restart.
