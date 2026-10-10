# Self-hosting with the configuration in a file

Without a Vault KV mount, a merchant's configuration is its file: the
standalone server's merchant manifest, or `Config.Merchant` for an embedded
host. It is read at startup and held in memory, credentials included; Postgres
keeps only the merchant and PSP identities history points at. Changing the
configuration is changing the file and restarting. The configuration routes
read it (never credentials); the edit routes are not mounted. To edit over HTTP
instead, put the configuration in Vault ([vault.md](vault.md)).

## The three files

| File | Owns | Loaded by |
|---|---|---|
| `config.yaml` | process/infrastructure config (DB, Redis, `provider_write_mode`, `test_mode`, `vault`) | the standalone server / `openrails.Config`, built programmatically (embedded hosts) |
| merchant manifest (`/etc/openrails/merchants.yaml`, or `run-server` / `run-worker --merchant-manifest <path>`) | merchant identity, display name, settings, alert webhooks, **PSPs**: accounts on rails and their secrets (`merchants.<slug>.psps.<key>`, each with its `rail:`) | standalone server and worker boot, every boot; embedded hosts pass the same shape as `Config.Merchant` |
| catalog document (`/etc/openrails/catalog.yaml`) | products / prices / entitlements / PSP links | `openrails apply-catalog --merchant NAME --file PATH` (standalone) / `Config.Catalog`, applied by `openrails.New` (embedded hosts) |

Manifest anatomy and field semantics:
[merchant-provisioning.md](merchant-provisioning.md).

## Secret overlays

Secret VALUES do not belong in the committed YAML. Render them (Vault Agent
template, k8s Secret volume, CSI, …) as YAML files in the manifest's own shape
and list them in `merchant_manifest_overlays` (env
`MERCHANT_MANIFEST_OVERLAYS`, comma-separated):

```yaml
# /vault/secrets/merchants.yaml
merchants:
  myapp:
    psps:
      mobius:
        secrets: { security_key: ..., webhook_signing_secret: ... }
      ccbill:
        secrets: { datalink_username: ..., datalink_password: ..., salt: ... }
```

OpenRails needs **no live Vault connection** in this mode (Vault Transit for
Solana signing is the one optional exception).

## Precedence

The manifest is the base; overlays merge over it in the listed order, later
wins. The merged tree is strict-parsed: an unknown field, or secrets for a PSP
the manifest does not declare, refuses boot rather than being dropped.

## What happens at boot

The conventional file is optional: absent, the server boots control-plane-only
(bind merchants later). An explicit `--merchant-manifest` path must exist —
boot refuses otherwise.

Separate worker processes load the same manifest and overlays as API
processes; nothing in the configuration is shared through PostgreSQL. Both
commands accept `--merchant-manifest`.

1. The manifest and its overlays parse strictly. Unknown fields and retired
   key names refuse boot, never a silent drop.
2. Missing merchant identities are created in PostgreSQL, and each PSP's and
   custodian's identity is recorded.
3. Each document validates as the configuration API validates it; one that
   does not refuses boot.
4. A declared secret that resolves to an empty value (no YAML value, no
   mounted file, no env var) is a boot **error**. Under `test_mode`, NMI
   accounts are probed before arming: production credentials refuse to arm.

## Capabilities

- Configuration edits over HTTP or through a Client are refused: the edit
  routes are not mounted.
- Catalog edits over HTTP and catalog documents share the catalog: a document
  skips a product, price or meter whose field an edit set differently, and
  reports it (see [catalog ownership](catalog-ownership.md)).
- `openrails dump-merchant-config` reads a Vault-held configuration; with the
  manifest, the manifest is the dump.

## Rotation walkthrough

1. Rotate the value in your (operator) secret source.
2. Re-render the mounted file (or env var).
3. Restart. The PSP's credential is checked against its declared account
   before use.
