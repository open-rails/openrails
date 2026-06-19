# Merchant Provisioning, Credentials, and Webhook Routing

OpenRails calls the billing/isolation namespace a **merchant**. AuthKit calls the
authority that controls it an **org**. Each merchant has its own dedicated
backing org (1:1); the merchant row carries `owner_org_id` pointing at it
(`UNIQUE` on non-null, nullable for embedded-without-AuthKit).

## Bootstrap model (#527)

The bootstrap manifest declares **merchants only** — there is no `auth`/users/
global-roles section. Each merchant carries an inline `issuer` (the host
application's JWKS or static public-key trust), which OpenRails registers as the
**owner** of the merchant's backing org. The host app's delegated tokens then
fully administer that one merchant — and no other — because federated authority
claims are stripped on verify and authority is the stored owner role:

```text
host-app issuer (JWKS) -> owner of the merchant's org -> OpenRails merchant
```

OpenRails owns authority; the host app owns identity; there are no local
OpenRails accounts in standalone mode. Startup provisioning is **first-run only**
(gated by the `openrails.bootstrap_state` marker; a reboot of a provisioned
deployment skips it, so a stale manifest can never brick a restart). Change
merchants afterward with `openrails push-merchant-config`:

- default: additive + **seed-once** (existing secrets are left untouched, so a
  secret rotated out of band via the admin API is never reverted to the seed);
- `--overwrite`: re-assert manifest secret values;
- `--prune`: delete secrets absent from the manifest;
- `--dry-run`: print the plan without mutating.

See `config/merchants.example.yaml` for the manifest shape.

Embedded hosts (cozy-art, doujins, tensorhub) instead provision via
`embed.Options` (one merchant per engine, secrets passed programmatically) and
authorize the `/v1/admin` surface with their own AuthKit/Authenticator — the
issuer-as-owner path is the standalone mechanism.

## Lifecycle

The lifecycle service is `internal/merchants.Service`.

| Operation | Behaviour |
|---|---|
| `Provision` | Creates or updates `openrails.merchants` and links `owner_org_id` when an AuthKit org owns the merchant. |
| `Suspend` / `Resume` | Flips `status` and `suspended_at`. Suspended merchants deny writes while reads and processor webhook reconciliation can still proceed. |
| `Export` | Writes a completed `merchant_exports` row with row counts and secret names, never secret values. |
| `Delete` | Requires confirmation and a completed export, purges merchant-owned rows and secrets, then tombstones the merchant directory row. |

## Secrets

Merchant credentials are addressed by `(merchant_id, name)`.

Canonical secret names include:

- `stripe/secret_key`
- `stripe/webhook_signing_secret`
- `stripe/webhook_signing_secret_thin`
- `nmi/<account>/production_key`
- `ccbill/account_config`

Secret stores:

- DB-backed store: `openrails.merchant_secrets`, envelope-encrypted by
  `openrails.merchant_deks`.
- In-memory store: tests and local development.
- Vault store: maps `(merchant_slug, name)` to
  `secret/openrails/merchants/<merchant-slug>/<name>`.

Credentials are loaded by merchant id at request time. They are never global
process configuration for a multi-merchant OpenRails instance.

## Webhooks

Processor webhooks resolve the merchant by explicit path slug.

Path form:

```text
POST /v1/merchants/:merchant/webhooks/:provider
POST /billing/v1/merchants/:merchant/webhooks/:provider
```

OpenRails always re-derives the merchant and loads that merchant's signing secret
before verification. The router is not the trust boundary; signature verification
with the resolved merchant's secret is.

## Admin Surface

Merchant lifecycle and credential routes are mounted under:

```text
/v1/admin/merchants
```

These routes are gated by live AuthKit org permissions. A user, API key, or
registered JWKS issuer controls a merchant only through its authority over the
merchant's `owner_org_id`.

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
        account_id: mobius-profile-id
        provider_key: mobius
        role: primary
        secrets:
          production_key: {env: DOUJINS_MOBIUS_PRODUCTION_KEY}
          tokenization_key: {env: DOUJINS_MOBIUS_TOKENIZATION_KEY}
```

Catalog manifests are explicitly catalog-scoped and may contain one or more
merchant catalog entries:

See `config/catalog.example.yaml` for a fuller example with tier
groups, products, prices, provider fan-out, and provider links.

```yaml
version: 1
catalogs:
  - merchant: doujins
    default_providers: [mobius]
```

Issuer/JWKS registration belongs in AuthKit remote applications through the
`auth.orgs[].issuers` bootstrap section. OpenRails stores only the merchant
ownership link, merchant profile/configuration, provider-account bindings, and
merchant secret values/references.
