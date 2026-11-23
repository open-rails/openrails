# Merchant configuration

A merchant's configuration is its display name, settings, alert webhooks, PSPs
and custodians, credentials included. It lives in one of two places:

- **A file**: `Config.Merchants` embedded, or the standalone merchant manifest.
  Read at startup; the read routes work, the edit routes are not mounted. Change
  the file and restart ([configuration](configuration.md#merchant-configuration-in-a-file)).
- **Vault**, when `vault.kv_mount` is set: staff edit it over HTTP or in the
  console, and automation may write the documents directly ([Vault](vault.md)).

PostgreSQL never holds it.

## PSPs

A PSP is declared under `psps.<key>` (`merchants.<slug>.psps.<key>` in a
standalone manifest) with its `rail`, `environment` (omitted: the rail's
real-money one), its account fields, settings and `secrets`:

| Rail | Environments | Account fields | Settings | Secrets |
|---|---|---|---|---|
| `nmi` | `live`, `test` (account in test mode), `sandbox` (sandbox.nmi.com) | `gateway_id` (the dashboard's Gateway ID) | `tokenization_key`, `tokenization_url`, `card_entry`, `custodian` | `security_key`, `webhook_signing_secret` |
| `stripe` | `live`, `test` | `account_id` (`acct_…`) | `publishable_key` | `secret_key`, `webhook_signing_secret`, `webhook_signing_secret_thin` |
| `ccbill` | `live`, `sandbox` | `client_accnum`, `client_subacc` | — | `salt`, `datalink_username`, `datalink_password` |
| `solana` | `mainnet`, `devnet`, `testnet` | `treasury` | — | `rpc_url` (optional) |

`webhook_signing_secret_previous` (with `webhook_overlap_expires_at`) keeps an
old webhook secret valid during rotation. `GET /v1/config`'s `rails` lists the
same fields. The account fields join with `-` into the account id webhooks post
to (`/v1/webhooks/{rail}/{account_id}`). Per-rail details: [rails](rails/README.md).
The retired keys `accounts`, `rail_merchant_accounts` and `provider_accounts`
refuse to load with a rename error.

With Vault, `Client.CreatePSP` checks credentials with the provider before
storing anything, and `UpdatePSP` changes settings, rotates or archives against
`expected_revision` ([operations](operations.md#psps-and-credentials)). Archiving
the last active PSP on a rail is `409 psp_last_active` unless `allow_last`.

### Other manifest fields

- `remote_application` (standalone): the app's issuer (`issuer` with `jwks_uri`,
  inline `jwks` or `public_keys`), a trusted issuer in the merchant's group holding
  its owner role ([trusted issuers](auth.md#trusted-issuers)).
- `custodians.<key>`: a card custodian a PSP references with `custodian: <key>`
  (`kind: basis_theory`, `environment` `live` or `test`, `account_id`,
  `public_api_key`, `network_tokens`, `secrets`); several PSPs may share one
  ([payment-method custody](payment-method-custody.md)).
- `secrets.scim_token`: a provisioning token (32+ characters) an embedded engine's
  SCIM routes accept; OpenRails keeps its SHA-256 ([customer contacts](customer-contacts.md)).
- Settings sit flat on the merchant (below); omitted fields keep stored values and
  a declared list replaces the stored one.

Internally a credential is named
`psps/<rail>/<environment>/<account>/<secret_key>` (`psps/nmi/live/100001/security_key`);
unknown secret keys are refused. See [`config/merchants_config.example.yaml`](../config/merchants_config.example.yaml).

## Reading and editing

`GetMerchantConfiguration` (`GET /v1/merchant/configuration`) returns the display
name, settings, a `revision` and `platform_vault.available` (whether
`settings.card_vault` may be `platform`); never credentials.
`UpdateMerchantConfiguration` (`PATCH`, Vault only) merges the fields it names:
omitted fields keep their values, an explicit empty list clears one, and
`expected_revision` refuses an edit made since (`409 revision_mismatch`). The
whole document is validated first; a refusal changes nothing, and every replica
reloads on commit. Alert webhooks use `CreateAlertWebhook`, `UpdateAlertWebhook`
and `DeleteAlertWebhook`; a webhook URL is a write-only credential (reads show
`destination_host`), and a new URL keeps the webhook's identity.

YAML and JSON share one typed contract, at most 1 MiB; unknown or duplicate
fields, several documents, aliases, anchors, merge keys and tags are refused.

```sh
openrails get-merchant-config --config config.yaml --merchant shop
openrails apply-merchant-config --config config.yaml --merchant shop --file update.yaml
# remote: acts on the merchant its token is bound to; no --config or --merchant
openrails apply-merchant-config --server-url https://billing.example --token-file token.txt --file update.yaml
```

```yaml
expected_revision: 4
display_name: Shop
settings:
  profile:
    support_url: https://shop.example/support
    subscribe_url: https://shop.example/subscribe
    billing_url: https://shop.example/account
```

`push-merchant-config --insert` creates the merchants a manifest names;
`dump-merchant-config` prints a Vault-held configuration in the manifest's shape,
without credentials or alert webhooks.

## Settings

`billing.MerchantSettings`, flat on the merchant in a file:

- `profile`: `support_url`, `subscribe_url` (where new customers and those whose
  access ended subscribe), `billing_url` (where customers manage subscriptions and
  [agreements](agreements.md)), `from_email`, `logo_url`, `terms_url`,
  `privacy_url`, `statement_descriptor`. Notices pick a URL by purpose
  (`MerchantProfile.NotificationURL`), falling back to `support_url`.
- `alert_email`: where high and critical findings go ([events](events.md#merchant-alerts)).
- `card_vault` (a PSP key or `platform`) and `charge_psps` (card PSPs taking new
  charges, in order): [payment-method custody](payment-method-custody.md).
- `psp_routing`: [processor routing](operations.md#processor-routing).
- `dunning_policy`: [dunning](dunning.md).
- `usage_billing` (default billing day, per-currency credit limits and minimum
  invoice amounts), billing policies with their default and tier bindings, and
  delegated wasted-spend limits, and arrears (`arrears_grace_days`,
  `arrears_delinquency_floor`, `collection_threshold`, `monthly_floor`,
  `billing_period_boundary`): [usage billing](usage-billing.md) and
  [spend limits](spend-limits.md). The billing day seeds new accounts only.
- `reprice_notice_window_days`: notice before a price migration takes effect.
- `balance_notices`: default low-balance lines ([running charges](running-charges.md#balance-notices)).
- `checkout.embed_origins`: sites that may frame the merchant's pay URLs
  ([payment page](payment-page.md)).
- `provider_refund_access`: what a provider-side refund does to access.
- `usage_gate_safe_spend` (default `false`): gates may spend each hold's money
  size again while OpenRails is unreachable ([the gate](usage-billing.md#the-gate)).

Policy references must resolve in the resulting document, and removing a policy
a customer is bound to is refused.

## Customer settings

`billing.CustomerSettings` is what the merchant sets for one customer:
`credit_limits` and `trust_levels` by currency, `billing_policy`,
`invoice_profile`, `low_balance_lines` and `usage_billing` overrides. `GetCustomer` /
`ListCustomers` answer them as `settings`; `UpdateCustomer`
(`PATCH /v1/admin/customers/{customer_id}`, `billing.MerchantCustomerManage`)
merges by currency (amount `"0"` or level `""` clears one; `null` clears a policy
or profile) and returns the saved `settings`. It never creates a customer
(`404 customer_not_found`); an undeclared policy is `404 billing_policy_not_found`.
A minimum invoice above the resulting credit limit is refused.

```json
{"items": [{"customer_id": "…", "credit_limits": [{"currency": "USD", "amount": "50000000"}], "billing_policy": "cloud_monthly"}]}
```
