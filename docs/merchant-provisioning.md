# Merchant Provisioning, Credentials, and Webhook Routing

OpenRails calls the billing/isolation namespace a **merchant**. AuthKit calls the
authority that controls it an **org**. A merchant row carries `owner_org_id`,
which is the AuthKit org whose roles, service tokens, and registered issuers can
administer that merchant.

Bootstrap-created deployments should still keep this chain explicit:

```text
credential -> AuthKit org -> OpenRails merchant
```

For standalone bootstrap, create or reuse the AuthKit org first, then create the
OpenRails merchant owned by that org. If a deployment is fully bootstrapped and
not user-managed, the org can be a deployment-owned bootstrap org rather than a
human-owned account.

## Lifecycle

The lifecycle service is `internal/merchants.Service`.

| Operation | Behaviour |
|---|---|
| `Provision` | Creates or updates `openrails.merchants`, records routing/tier/region, and links `owner_org_id` when an AuthKit org owns the merchant. |
| `Suspend` / `Resume` | Flips `status` and `suspended_at`. Suspended merchants deny writes while reads and processor webhook reconciliation can still proceed. |
| `TierChange` | Updates OpenRails' own `billing_tier` metadata for the merchant. |
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

Processor webhooks can resolve the merchant by explicit path slug or registered
host.

Path form:

```text
POST /v1/m/:merchant/webhooks/:provider
```

Host form:

```text
POST /v1/webhooks/:provider
Host: hooks.example.com
```

OpenRails always re-derives the merchant and loads that merchant's signing secret
before verification. The router is not the trust boundary; signature verification
with the resolved merchant's secret is.

## Admin Surface

Merchant lifecycle and credential routes are mounted under:

```text
/v1/admin/merchants
```

These routes are gated by live AuthKit org permissions. A user, service token, or
registered JWKS issuer controls a merchant only through its authority over the
merchant's `owner_org_id`.

## Merchant Config Manifest

The merchant config manifest used by `openrails push-merchant-config` keeps
AuthKit-owned authority in `auth:` and OpenRails-owned merchant definitions in
`merchants:`. Catalog state is pushed separately with
`openrails push-merchant-catalog`.

```yaml
version: 1

auth:
  orgs:
    - slug: doujins
      issuers:
        - slug: doujins-app
          issuer: https://doujins.example
          jwks_uri: https://doujins.example/.well-known/jwks.json

merchants:
  - slug: doujins
    name: Doujins
    profile:
      display_name: Doujins Billing
      logo_url: https://doujins.example/logo.png
      from_email: billing@doujins.example
      support_url: https://doujins.example/support
      support_email: support@doujins.example
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

```yaml
version: 1
catalogs:
  - merchant: doujins
    default_providers: [nmi]
```

Issuer/JWKS registration belongs in AuthKit remote applications through the
`auth.orgs[].issuers` bootstrap section. OpenRails stores only the merchant
ownership link, merchant profile/configuration, provider-account bindings, and
merchant secret values/references.
