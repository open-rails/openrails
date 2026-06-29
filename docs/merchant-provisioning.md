# Merchant Provisioning, Credentials, and Webhook Routing

OpenRails calls the billing/isolation namespace a **merchant**. AuthKit authority
for that merchant is a top-level **merchant permission-group**. The merchant row
carries `permission_group_id` pointing at that group (nullable for
embedded-without-AuthKit).

## Provisioning model (#527/#531)

OpenRails splits provisioning into three file-backed surfaces:

- `push-auth-bootstrap` applies AuthKit's standalone authority bootstrap
  manifest: initial users, trusted remote applications, and root roles.
- `push-merchant-config` declares merchants, profile/config, provider accounts,
  provider secrets, and optional host-app issuer ownership.
- `push-merchant-catalog` declares products, prices, product tier metadata, and provider
  catalog links.

The merchant config manifest carries an inline `issuer` (the host application's
JWKS or static public-key trust), which OpenRails registers under the merchant's
permission-group. The host app's delegated tokens then fully
administer that one merchant — and no other — because federated authority claims
are stripped on verify and authority is the stored owner role:

```text
host-app issuer (JWKS) -> merchant permission-group role -> OpenRails merchant
```

OpenRails owns merchant authority; AuthKit owns root identity authority. Startup
bootstrap, if `/etc/openrails/bootstrap.yaml` exists, is **first-run only** and
limited to AuthKit authority (gated by AuthKit's bootstrap marker). A normal
server restart never reapplies merchant config or catalog state. Change
merchants with `openrails push-merchant-config`:

- no mutation flags: plan-only;
- `--insert`: create missing merchant/config/secret state;
- `--overwrite`: re-assert manifest secret values;
- `--prune`: delete secrets absent from the manifest;

See `config/merchants.example.yaml` for the manifest shape.

Embedded hosts (cozy-art, doujins, tensorhub) instead provision via
`embed.Options` (one merchant per engine, secrets passed programmatically) and
authorize the `/v1/merchant` surface with their own AuthKit/Authenticator — the
issuer-as-owner path is the standalone mechanism.

## Lifecycle

The lifecycle service is `internal/merchants.Service`.

| Operation | Behaviour |
|---|---|
| `Provision` | Creates or updates `openrails.merchants` and records `permission_group_id` for control-plane merchants. |
| `Suspend` / `Resume` | Flips `status` and `suspended_at`. Suspended merchants deny writes while reads and rail webhook reconciliation can still proceed. |
| `Export` | Writes a completed `merchant_exports` row with row counts and secret names, never secret values. |
| `Delete` | Requires confirmation and a completed export, purges merchant-owned rows and secrets, then tombstones the merchant directory row. |

## Secrets

Merchant credentials are addressed by `(merchant_id, name)`. Provider credentials
use provider-account-scoped names:

```text
provider_accounts/<provider_type>/<environment>/<provider_account_id>/<secret_key>
```

Examples:

- `provider_accounts/stripe/live/acct_123/secret_key`
- `provider_accounts/stripe/live/acct_123/webhook_signing_secret`
- `provider_accounts/nmi/live/mobius-profile-id/security_key` (manifest key; legacy storage may still normalize this to `production_key`)
- `provider_accounts/ccbill/live/900000-0000/account_config`

Secret stores:

- DB-backed store: `openrails.merchant_secrets`, envelope-encrypted by
  `openrails.merchant_deks`.
- In-memory store: tests and local development.
- Vault store: maps `(merchant_slug, name)` to
  `secret/openrails/merchants/<merchant-slug>/<name>`.

Credentials are loaded by merchant id at request time. They are never global
process configuration for a multi-merchant OpenRails instance. The
`push-merchant-config` manifest is seed material: it reads values from env/file
references and imports them into the configured runtime backend. With
`vault.enabled: true`, provider secrets are written to Vault paths; otherwise
they are written to `openrails.merchant_secrets` and envelope-encrypted when an
`encryption.master_key` is configured.

## Webhooks

Rail webhooks resolve the merchant by explicit path slug.

Path form:

```text
POST /v1/merchants/:merchant/webhooks/:provider
POST /billing/v1/merchants/:merchant/webhooks/:provider
```

OpenRails always re-derives the merchant and loads that merchant's signing secret
before verification. The router is not the trust boundary; signature verification
with the resolved merchant's secret is.

## Admin Surface

OpenRails core no longer exposes cross-merchant lifecycle or credential routes.
Merchant-owned admin APIs are scoped to the authenticated merchant; any
future platform operator surface belongs in OpenRails SaaS.

## Merchant Config Manifest

The merchant config manifest used by `openrails push-merchant-config` declares
OpenRails-owned merchants. Catalog state is pushed separately with
`openrails push-merchant-catalog`.

See `config/merchants.example.yaml` for a fuller example with
merchant issuer trust, profile data, provider accounts, and provider secret
sources.

```yaml
version: 1

merchants:
  - slug: doujins
    display_name: Doujins
    issuer:
      uri: https://doujins.example
      jwks_uri: https://doujins.example/.well-known/jwks.json
    profile:
      display_name: Doujins Billing
      logo_url: https://doujins.example/logo.png
      from_email: billing@doujins.example
      support_url: https://doujins.example/support
    provider_accounts:
      - provider_type: nmi
        environment: live
        account_id: mobius-profile-id
        mode: primary
        secrets:
          security_key: {env: DOUJINS_NMI_SECURITY_KEY}
          tokenization_key: {env: DOUJINS_NMI_TOKENIZATION_KEY}
          webhook_signing_secret: {env: DOUJINS_NMI_WEBHOOK_SECRET}
```

Catalog manifests are explicitly catalog-scoped and may contain one or more
merchant catalog entries:

See `config/catalog.example.yaml` for a fuller example with products, prices,
provider fan-out, and provider links.

```yaml
version: 1
catalogs:
  - merchant: doujins
    default_providers: [mobius]
```

Issuer/JWKS registration belongs in AuthKit remote applications under the
merchant permission-group. OpenRails stores only the merchant permission-group
link, merchant profile/configuration, provider-account bindings, and merchant
secret values/references.
