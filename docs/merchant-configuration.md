# Merchant configuration

A merchant's configuration is its display name, settings, alert webhooks, PSPs
and custodians, credentials included. It lives in one of two places:

- **A file** (`Config.Merchants`, or a standalone server's merchant manifest):
  read at startup. Reads work; the edit routes are not mounted, so change the
  file and restart.
- **Vault**, when a KV mount is named ([vault.md](vault.md)): staff edit it over
  HTTP or in the console, and automation may write the documents directly.

`GetMerchantConfiguration` returns the display name, the settings, a
`revision` and `platform_vault.available` (whether `settings.card_vault`
may be `platform`); credentials are never returned. `UpdateMerchantConfiguration`
merges the fields it names: omitted fields keep their values and an explicit
empty list clears one. Name the `expected_revision` you read and an edit made
since is refused with `409 revision_mismatch`; without one, the edit merges
onto the current configuration. PSPs and credentials use the PSP methods
(`Client.CreatePSP`, `UpdatePSP`), alert webhooks `CreateAlertWebhook`,
`UpdateAlertWebhook` and `DeleteAlertWebhook`.

## CLI

Local execution reads the deployment's configuration (the database, and Vault
or the manifest at its conventional path) with trusted operator authority over
an existing merchant. `--merchant` takes a current or former merchant name:

```sh
openrails get-merchant-config --config config.yaml --merchant shop
openrails apply-merchant-config --config config.yaml --merchant shop --file update.yaml
```

An update names the revision `get-merchant-config` returned:

```yaml
expected_revision: 4
display_name: Shop
settings:
  profile:
    support_url: https://shop.example/support
    subscribe_url: https://shop.example/subscribe
    billing_url: https://shop.example/account
```

`card_vault` is where new cards are saved: a merchant PSP key or `platform`.
`charge_psps` lists the card PSP keys taking new charges in order; omitted, a
named PSP vault charges through that account alone, and an unspecified vault
uses each chosen PSP's vault
([payment-method custody](payment-method-custody.md)).

`subscribe_url` is where new customers, and those whose access ended, buy a
subscription. `billing_url` is where customers manage and cancel their
subscriptions and [charge agreements](agreements.md). Messages about
notification events pick one by purpose (`MerchantProfile.NotificationURL`):
access ended links to `subscribe_url`; failed or due payments, payment method
updates, receipts and confirmations link to `billing_url`; either falls back to
`support_url`. `balance_notices` sets the customers' default
low-balance lines ([balance notices](balance-notices.md)).
`checkout.embed_origins` lists the sites that may frame its embedded pay URLs
([payment page](payment-page.md)).

YAML and JSON use the same typed contract. Documents are limited to 1 MiB.
Unknown or duplicate fields, multiple documents, YAML aliases, anchors, merge
keys and tags are rejected.

Remote execution constructs only a Client. It acts on the merchant its
credential is bound to, which must authorize the operation:

```sh
openrails get-merchant-config --server-url https://billing.example --token-file token.txt
openrails apply-merchant-config --server-url https://billing.example --token-file token.txt --file update.yaml
```

Remote mode does not read local infrastructure configuration and rejects
explicit `--config`, `--provider-write-mode`, `--test-mode` and `--merchant`
flags. In a local run `--merchant` names the merchant; a slug selects scope
and grants no authority.

`push-merchant-config --seed` provisions the merchants a manifest names (create
only). `dump-merchant-config` prints a Vault-held merchant's configuration in
the manifest's shape, without credentials or alert webhooks.
