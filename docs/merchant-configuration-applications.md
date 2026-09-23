# Merchant metadata applications

Merchant metadata lives in the database independently of credential custody and
external HTTP publication. Startup creates missing identities and initializes
missing metadata; an unchanged startup declaration preserves later API edits and
archived provider accounts. Snapshot credential reload is separate from metadata
application and does not persist supplied credentials.

Use the same `Client.MerchantConfiguration` resource in embedded and remote
applications. `Retrieve` returns redacted settings and an opaque revision. `Apply`
requires a stable application ID and the revision observed before preparing the
update. It applies atomically. Retrying the identical document returns its original
receipt even if later updates changed the merchant. Reusing an ID for different
content or applying against a stale revision fails with a conflict.

Omitted fields preserve existing values. Explicit empty policy lists clear those
lists. Provider credentials and lifecycle use `Client.PaymentProviders` and their
own publication operations. Metadata applications cannot change issuer trust,
credential backends, HTTP exposure or Vault paths.

## CLI

Local execution needs the runtime database configuration and uses trusted operator
authority over an existing merchant. It publishes no HTTP routes and requires no
provider secrets or standalone authentication issuer. Names resolve through the
configured AuthKit group directory. Add `--unbound-merchants` for host-local
merchants that have no AuthKit group binding:

```sh
openrails get-merchant-config --config config.yaml --merchant shop
openrails apply-merchant-config --config config.yaml --merchant shop --file update.yaml
```

Prepare the document using the revision returned by `get-merchant-config`:

```yaml
application_id: support-links-2026-09
expected_revision: "COPY_THE_OBSERVED_REVISION"
display_name: Shop
settings:
  profile:
    support_url: https://shop.example/support
```

YAML and JSON use the same typed contract. Documents are limited to 1 MiB.
Unknown or duplicate fields, multiple documents, YAML aliases, anchors, merge
keys and tags are rejected. The CLI never refreshes the precondition automatically.

Remote execution constructs only a Client. The server must explicitly publish
merchant configuration routes, and the credential must authorize the selected
merchant and operation:

```sh
openrails get-merchant-config --server-url https://billing.example --token-file token.txt --merchant shop
openrails apply-merchant-config --server-url https://billing.example --token-file token.txt --merchant shop --file update.yaml
```

Remote mode does not read local infrastructure configuration and rejects explicit
`--config`, `--provider-write-mode`, `--test-mode` and `--unbound-merchants` flags. Merchant slugs select
scope; they do not grant authority.

The older `push-merchant-config` supports create-only initialization. Its
`--overwrite` and `--prune` mutations are retired. `dump-merchant-config` exports
redacted metadata in either custody mode; `--include-secrets` is removed.
