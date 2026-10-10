# HashiCorp Vault + OpenRails

Name a KV mount and Vault holds merchant configuration; name only a Transit
mount and it only signs.

| Capability | What it is for | Setting |
|---|---|---|
| **Merchant configuration** | every merchant's display name, settings, alert webhooks, PSPs and custodians, credentials included | `vault.kv_mount` (a KV v2 mount) |
| **Transit signing** | Solana `vault_transit` signers: Vault signs, the key never leaves | `vault.transit_mount` (default `transit`) |

Without a KV mount the merchant's configuration is its file (`Config.Merchant`,
or a standalone server's merchant manifest): read at startup and read-only.
Postgres never holds merchant configuration in either mode; it keeps the
identities history points at (merchants, PSP and custodian accounts) and what
OpenRails learns about them.

## Where configuration lives

With a KV mount named, Vault is the only source:

- Staff edit configuration over HTTP (`PATCH /v1/admin/configuration`, the
  `/v1/admin/psps` and `/v1/admin/alert-webhooks` routes) or in the console,
  starting from an empty configuration.
- Automation writes the documents straight into Vault at the paths below.
- A file that declares configuration beside Vault is refused at startup, naming
  what it declares (`display_name`, `psps`, `custodians`, `settings`,
  `alert_webhooks`, `secrets`). The declaration names the merchant only: its
  slug, `api_host`, and on the standalone server `remote_application`.

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
checkout and the catalog use) matches `^[a-z0-9][a-z0-9_-]{0,62}$`.
`credential_fingerprint_key` is created by OpenRails on first start; it keys
the fingerprints that catch one gateway account declared twice. Keep it.

```sh
id=$(openrails merchants get shop | jq -r .id)
vault kv put -mount=kv openrails/merchants/$id/psps/mobius @mobius.json
```

## Document shapes

Documents use the merchant manifest's spelling. Unknown fields are refused.

**`merchant`**

```json
{
  "display_name": "Shop",
  "settings": { "arrears_grace_days": 7, "profile": { "support_url": "https://shop.example/support" } },
  "alert_webhooks": [
    { "id": "0192f1c4-8d1e-7c3a-9a51-3b2f0c6d7e81", "name": "ops", "url": "https://hooks.example/alert", "format": "generic", "enabled": true }
  ],
  "secrets": { "scim_token": "…" }
}
```

`settings` is the document [`GET /v1/admin/configuration`](api/merchant-settings.md)
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
  "settings": { "tokenization_key": "…" },
  "secrets": { "security_key": "…", "webhook_signing_secret": "…" }
}
```

`environment` is `live` or `test`; a deployment serves the documents of its own
posture (`test_mode`). `account_id` is the rail's own id ([merchant
provisioning](merchant-provisioning.md) lists each rail's). A Solana PSP's
`account_id` is its signer's public key, and its document carries
`"signer": {"mode": "vault_transit", "key": "<transit key>"}` or a
`private_key` secret. `custodian` names a custodian document whose cards this
PSP charges.

**`custodians/<key>`**

```json
{
  "kind": "basis_theory",
  "environment": "live",
  "account_id": "tenant-id",
  "archived": false,
  "settings": { "public_api_key": "…" },
  "secrets": { "api_key": "…" }
}
```

A document that fails validation is not served: the version before it keeps
serving, and the rejection is logged.

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
checkout, renewals and webhooks keep working for every merchant already loaded.
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
  # role_id / secret_id                  # approle; a mounted secret FILE named VAULT_SECRET_ID
  #                                      #   is re-read on every re-auth (rotation-safe)
  # token                                # dev/e2e or a sidecar-managed token (env VAULT_TOKEN)
```

OpenRails authenticates once, as itself; merchant isolation is the path, built
by one function in code. The token is held in memory only. A background
supervisor logs in, renews, and re-authenticates when renewal is no longer
possible. Embedded hosts register the `openrails_vault` probe from
`Client.Probes()`.

## Minimal policies

**Merchant configuration** (`kv_mount: kv`):

```hcl
path "kv/data/openrails/*"     { capabilities = ["create", "read", "update", "delete"] }
path "kv/metadata/openrails/*" { capabilities = ["read", "delete", "list"] }
```

A read-only token serves an existing configuration and refuses edits; the
first start writes `credential_fingerprint_key`.

**Transit signing**, scoped to the keys PSPs name as `signer.key`:

```hcl
path "transit/sign/my-mainnet-signer" { capabilities = ["update"] }
path "transit/keys/my-mainnet-signer" { capabilities = ["read"] }
```

## Solana Transit signing

The `vault_transit` signer keeps the merchant's Ed25519 key inside Vault
Transit ([rails/solana.md](rails/solana.md)). The operator creates the key;
OpenRails never creates or exports it:

```sh
vault write -f transit/keys/my-mainnet-signer type=ed25519 exportable=false
```

OpenRails signs at `transit/sign/<key>` and reads the public key, which is the
merchant's on-chain address and the PSP's `account_id`, from
`transit/keys/<key>`. A key whose public key changes is refused until an
operator approves the new identity (`openrails solana-signer approve`); the
approval updates the PSP document's `account_id`.

## Merchant purge

A committed merchant purge deletes every document under the merchant's id. The
cleanup is recorded in `maintenance_runs` and retried by the
`openrails.merchant_secret_cleanup` worker until the merchant's subtree is
empty. A changed Vault address or mount is refused: restore the original to
finish an outstanding cleanup.
