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

Embedded hosts provision billing merchants explicitly with
`embed.Runtime.UpsertMerchantConfig` and pass auth at HTTP mount time. The engine
is not bound to one merchant at construction; in-process SDK calls pin the
merchant with `openrails.WithMerchant(ctx, merchantID)`. The issuer-as-owner path
is the standalone mechanism.

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
rail_merchant_accounts/<rail>/<environment>/<account_id>/<secret_key>
```

Examples:

- `rail_merchant_accounts/stripe/live/acct_123/secret_key`
- `rail_merchant_accounts/stripe/live/acct_123/webhook_signing_secret`
- `rail_merchant_accounts/nmi/live/579145/security_key`
- `rail_merchant_accounts/ccbill/live/900000-0000/salt`
- `rail_merchant_accounts/ccbill/live/900000-0000/datalink_username`
- `rail_merchant_accounts/ccbill/live/900000-0000/datalink_password`

CCBill's composite identity is dash-joined (`clientAccnum-clientSubacc`, #697),
so no account_id embeds the `/` name delimiter.

The `rail_merchant_accounts/` name prefix is deliberate #683 vocabulary and does
NOT follow the manifest key rename: the config key is `accounts` (#698), while
secret names and the DB table keep `rail_merchant_accounts`.

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

See `config/merchants_config.example.yaml` for a fuller example with
merchant remote-application trust, profile data, rail merchant accounts, and
secret seeds. The `accounts` key (#698) declares rail merchant accounts; secret
values can also arrive via the `BILLING_MERCHANTS_*` env overlay (e.g.
`BILLING_MERCHANTS_DOUJINS_ACCOUNTS_MOBIUS_NMI_SECRETS_SECURITY_KEY`).

```yaml
version: 1

merchants:
  doujins:
    display_name: Doujins
    remote_application:
      issuer: https://doujins.example
      jwks_uri: https://doujins.example/.well-known/jwks.json
    profile:
      display_name: Doujins Billing
      logo_url: https://doujins.example/logo.png
      from_email: billing@doujins.example
      support_url: https://doujins.example/support
      signup_url: https://doujins.example/premium
    accounts:
      mobius:
        nmi:
          environment: live
          account_id: "579145"
          archived: false
          settings:
            tokenization_key: replace-with-nmi-tokenization-key
          secrets:
            security_key: replace-with-nmi-security-key
            webhook_signing_secret: replace-with-nmi-webhook-secret
```

Catalog manifests are explicitly catalog-scoped and may contain one or more
merchant catalog entries:

See `config/catalog.example.yaml` for a fuller example with products, prices,
provider fan-out, and provider links.

```yaml
version: 1
catalogs:
  - merchant: doujins
    products:
      - key: premium
        display_name: Premium
        prices:
          - currency: usd
            unit_amount: 23_000_000
            duration: 30d
            auto_renew: true
            providers: [mobius]
```

Issuer/JWKS registration belongs in AuthKit remote applications under the
merchant permission-group. OpenRails stores only the merchant permission-group
link, merchant profile/configuration, provider-account bindings, and merchant
secret values/references.
