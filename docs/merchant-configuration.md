# Merchant configuration

A merchant's configuration is its display name, settings, alert webhooks, PSPs
and custodians, credentials included. It lives in one of two places:

- **A file** (`Config.Merchant`, or a standalone server's merchant manifest):
  read at startup. Reads work; the edit routes are not mounted, so change the
  file and restart.
- **Vault**, when a KV mount is named ([vault.md](vault.md)): staff edit it over
  HTTP or in the console, and automation may write the documents directly.

`GetMerchantConfiguration` returns the display name, the API host, the settings
and a `revision`; credentials are never returned. `UpdateMerchantConfiguration`
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
```

YAML and JSON use the same typed contract. Documents are limited to 1 MiB.
Unknown or duplicate fields, multiple documents, YAML aliases, anchors, merge
keys and tags are rejected.

Remote execution constructs only a Client. The credential must authorize the
selected merchant and operation:

```sh
openrails get-merchant-config --server-url https://billing.example --token-file token.txt --merchant shop
openrails apply-merchant-config --server-url https://billing.example --token-file token.txt --merchant shop --file update.yaml
```

Remote mode does not read local infrastructure configuration and rejects
explicit `--config`, `--provider-write-mode` and `--test-mode` flags. Merchant
slugs select scope; they do not grant authority.

`push-merchant-config --seed` provisions the merchants a manifest names (create
only). `dump-merchant-config` prints a Vault-held merchant's configuration in
the manifest's shape, without credentials or alert webhooks.
