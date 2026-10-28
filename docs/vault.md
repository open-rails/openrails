# HashiCorp Vault + OpenRails

`vault.kv_mount` selects Vault for merchant configuration. Platform card
credentials and Transit signing can use Vault while merchant configuration
stays in files.

| Capability | What it is for | Setting |
|---|---|---|
| **Merchant configuration** | every merchant's display name, settings, alert webhooks, PSPs and custodians, credentials included | `vault.kv_mount` (a KV v2 mount) |
| **Platform card credentials** | the operator's Basis Theory private key and merchant collection applications | `card_vault.kv_mount` (defaults to `vault.kv_mount`) |
| **Transit signing** | Solana `vault_transit` crankers: Vault signs, the key never leaves | `vault.transit_mount` (default `transit`) |

Without `vault.kv_mount` the merchant's configuration is its file (`Config.Merchants`,
or a standalone server's merchant manifest): read at startup and read-only.
Postgres never holds merchant configuration in either mode; it keeps the
identities history points at (merchants, PSP and custodian accounts) and what
OpenRails learns about them.

## Where configuration lives

With `vault.kv_mount` named, Vault is the only merchant configuration source:

- Staff edit configuration over HTTP (`PATCH /v1/merchant/configuration`, the
  `/v1/merchant/psps` and `/v1/merchant/alert-webhooks` routes) or in the console,
  starting from an empty configuration.
- Automation writes the documents straight into Vault at the paths below.
- A file that declares configuration beside Vault is refused at startup, naming
  what it declares (`display_name`, `psps`, `custodians`, a setting,
  `alert_webhooks`, `secrets`). The declaration names the merchant only: its
  slug, an embedded host's `Issuer` and `Catalog`, and on the standalone server
  `remote_application`.

There is no import or seeding step.

## Document paths

Each merchant's configuration is one JSON document per object, under the
merchant's id (`openrails merchants get <slug>` prints it; a rename never moves
it):

```
<kv_mount>/<scope_prefix>/merchants/<merchant-id>/merchant
<kv_mount>/<scope_prefix>/merchants/<merchant-id>/psps/<key>
<kv_mount>/<scope_prefix>/merchants/<merchant-id>/custodians/<key>
<kv_mount>/<scope_prefix>/credential_fingerprint_key
```

`scope_prefix` defaults to `openrails`. A key (the PSP or custodian name
purchase and the catalog use) matches `^[a-z0-9][a-z0-9_-]{0,62}$`.
`credential_fingerprint_key` is created by OpenRails on first start; it keys
the fingerprints that catch one gateway account declared twice. Keep it.
Platform card credentials live at
`<card_vault.kv_mount>/<scope_prefix>/platform_vault/<tenant_id>`.
If the platform mount is omitted, `vault.kv_mount` supplies it, preserving the
existing shared layout. Both use `vault.scope_prefix` (default `openrails`)
and the same Vault connection. An explicit platform mount is never replaced
by the merchant mount when a read fails. A deployment using only platform
credentials needs read access to that document, not merchant configuration
or its fingerprint key. See [payment-method custody](payment-method-custody.md).

```sh
id=$(openrails merchants get shop | jq -r .id)
vault kv put -mount=kv openrails/merchants/$id/psps/mobius @mobius.json
```

## Document shapes

Documents use the merchant manifest's spelling: flat, with only credentials
nested under `secrets`. Unknown fields are refused.

**`merchant`**

```json
{
  "display_name": "Shop",
  "arrears_grace_days": 7,
  "profile": { "support_url": "https://shop.example/support" },
  "alert_webhooks": [
    { "id": "0192f1c4-8d1e-7c3a-9a51-3b2f0c6d7e81", "name": "ops", "url": "https://hooks.example/alert", "format": "generic", "enabled": true }
  ],
  "secrets": { "scim_token": "…" }
}
```

Its settings are the fields [`GET /v1/merchant/configuration`](api/merchant-settings.md)
returns. An alert webhook's `id` is a UUID you choose; `format` is `generic`,
`discord` or `slack`.

**`psps/<key>`**

```json
{
  "rail": "nmi",
  "environment": "live",
  "account_id": "1234567",
  "archived": false,
  "custodian": "",
  "tokenization_key": "…",
  "secrets": { "security_key": "…", "webhook_signing_secret": "…" }
}
```

The rail decides the fields. `environment` is one of the rail's, its
real-money one when omitted ([PSP environments](psp-environments.md)).
`account_id` is the rail's own id ([merchant provisioning](merchant-provisioning.md)
lists each rail's). A Solana PSP has `treasury` in its place and an optional
`rpc_url` secret:

```json
{ "rail": "solana", "environment": "devnet", "treasury": "9hSR…BWBu", "secrets": { "rpc_url": "https://devnet.helius-rpc.com/?api-key=…" } }
```

`custodian` names a custodian document whose cards this PSP charges.

**`custodians/<key>`**

```json
{
  "kind": "basis_theory",
  "environment": "live",
  "account_id": "tenant-id",
  "archived": false,
  "public_api_key": "…",
  "secrets": { "api_key": "…" }
}
```

A document that fails validation is not served: the version before it keeps
serving, the rejection is logged, the `openrails_configuration` probe names
it and a `consistency.merchant_config.not_served` finding stays open until it
serves.

## Revisions and edits

A document's KV version is its revision (`revision` on the configuration and on
each PSP). Edits through OpenRails write with check-and-set: naming
`expected_revision` refuses an object changed since with `409
revision_mismatch`. Rotating one PSP's credentials rewrites that PSP's document
alone.

Each replica caches a merchant's documents, loaded on first use. An edit
through OpenRails reaches every replica at once (Postgres `NOTIFY`). An edit
made in Vault directly is seen within 30 seconds of the merchant's next use, or
on restart. Credentials are written, never read back over the API.

## Availability

Startup waits for Vault (bounded) when it holds configuration and fails when it
does not answer. Afterwards a Vault outage is ridden out from the cache:
purchase, renewals and webhooks keep working for every merchant already loaded.
A merchant first used during the outage fails until Vault returns.

## Authenticating

Pick one `auth_method`. In-cluster, prefer `kubernetes` (no stored secret: the
pod's ServiceAccount is the credential).

```yaml
vault:                                   # declaring it connects; embedded: Config.Vault or Deps.Vault
  address: https://vault.internal:8200   # env VAULT_ADDR
  kv_mount: kv                           # merchant configuration; omit for Transit only
  # scope_prefix: openrails
  # transit_mount: transit
  # namespace: billing
  auth_method: kubernetes                # kubernetes | approle | token
  k8s_role: openrails
  # auth_mount: openrails-prod/k8s       # where the method is mounted (vault auth enable -path,
  #                                      #   without auth/); default its name (env VAULT_AUTH_MOUNT)
  # role_id / secret_id                  # approle; a mounted secret FILE named VAULT_SECRET_ID
  #                                      #   is re-read on every re-auth (rotation-safe)
  # token                                # dev/e2e or a sidecar-managed token (env VAULT_TOKEN)
```

OpenRails authenticates once, as itself; merchant isolation is the path, built
by one function in code. The token is held in memory only. A background
supervisor logs in, renews, and re-authenticates when renewal is no longer
possible. Embedded hosts register the `openrails_vault` probe from
the engine's `Probes()`.

## Minimal policies

**Merchant configuration** (`kv_mount: kv`):

```hcl
path "kv/data/openrails/*"     { capabilities = ["create", "read", "update", "delete"] }
path "kv/metadata/openrails/*" { capabilities = ["read", "delete", "list"] }
```

A read-only token serves an existing configuration and refuses edits; the
first start writes `credential_fingerprint_key`.

**Transit signing**, scoped to the crankers' keys (`solana.crankers[].key`):

```hcl
path "transit/sign/openrails-solana-cranker" { capabilities = ["update"] }
path "transit/keys/openrails-solana-cranker" { capabilities = ["read"] }
```

## Solana Transit signing

A `vault_transit` cranker keeps its Ed25519 key inside Vault Transit
([rails/solana.md](rails/solana.md)). The operator creates the key; OpenRails
never creates or exports it:

```sh
vault write -f transit/keys/openrails-solana-cranker type=ed25519 exportable=false
```

OpenRails signs at `transit/sign/<key>` and reads the public key, the
cranker's on-chain address, from `transit/keys/<key>`. A cranker is pinned to
the public key it first had: after `transit/keys/<key>/rotate` it is refused,
so rotate by creating a new key and listing it first instead.

## Merchant purge

A merchant purge (`openrails merchant purge`) queues the
`openrails.merchant_secret_cleanup` job in its own transaction. The job deletes
every document under the merchant's id, records the cleanup on the purge's
`maintenance_runs` row and is retried by River until the merchant's subtree is
empty. A changed Vault address or mount is refused: restore the original to
finish an outstanding cleanup.
