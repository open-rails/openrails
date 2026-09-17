# Self-hosting MODE 1: manifest-is-truth (`merchant_source: manifest`)

MODE 1 is the default OpenRails deployment shape: the YAML you supply at boot
**is** the source of truth, held in memory for the life of the process.
OpenRails writes nothing back — there is no merchant-secret store, no seed-once
dance, no store-vs-file divergence.

**Change anything = edit the file(s) + reboot.** This is rarely-changing
CONFIG, not data. The opposite posture — API-driven merchants with a Vault/DB
secret store — is MODE 2 (`merchant_source: api`). Comparison table:
[standalone-integration.md](standalone-integration.md#two-merchant-source-modes).
Deployment shape does not imply mode: embedded and standalone can run either.

## The three files

| File | Owns | Loaded by |
|---|---|---|
| `config.yaml` | process/infrastructure config (env, DB, Redis, `provider_write_mode`, `test_mode`, `merchant_source`) | `config.Load` (standalone) / built programmatically (embedded hosts) |
| merchant manifest (`/etc/openrails/merchants.yaml`, or `run-server --merchant-manifest <path>`) | merchant identity, profile, invoice policy, **PSPs** — rail accounts + secrets (`merchants.<slug>.psps.<key>.<rail>`) | standalone server boot, every boot; embedded hosts pass the same shape to `UpsertMerchantConfig` |
| catalog manifest (`/etc/openrails/catalog.yaml`) | products / prices / entitlements / PSP links | `openrails push-merchant-catalog` (or the embedded push API) |

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
        nmi:
          secrets: { security_key: ..., webhook_signing_secret: ... }
      ccbill:
        ccbill:
          secrets: { datalink_username: ..., datalink_password: ..., salt: ... }
```

OpenRails needs **no live Vault connection** at runtime (Vault Transit for
Solana signing is the one optional exception — KV is never consulted).

## Precedence

The manifest is the base; overlays merge over it in the listed order, later
wins. The merged tree is strict-parsed: an unknown field, or secrets for a PSP
the manifest does not declare, refuses boot rather than being dropped.

## What happens at boot

The conventional file is optional: absent, the server boots control-plane-only
(bind merchants later). An explicit `--merchant-manifest` path must exist —
boot refuses otherwise. `merchant_source: api` (MODE 2) refuses a present
manifest outright: two truths.

1. The manifest and its overlays parse strictly. Unknown fields and retired
   key names (the old `accounts:` key — renamed to `psps:`) refuse boot — never
   a silent drop.
2. Merchant rows, configuration, and `openrails.psps` converge into Postgres —
   **projections for foreign keys only**, steamrolled by the YAML every boot
   (insert+overwrite+prune).
3. Secrets are seeded **into memory** (the runtime manifest secret plane) and
   served through the same store interface every consumer reads — checkout,
   webhook verification, provider pulls, rebill charging. Nothing is written
   to `openrails.merchant_secrets` or Vault KV.
4. A declared secret that resolves to an empty value (no YAML value, no
   mounted file, no env var) is a boot **error**. A PSP declared without a
   given secret seeds nothing for it — requests needing it fail closed at use
   time. Under `test_mode`, NMI accounts are probed before arming: production
   credentials refuse to arm.

## What is rejected in mode 1

- Every catalog and payment-provider **mutation** API — product/price
  create/update/activate/deactivate, `POST /catalog/publish`,
  `PUT/DELETE /v1/merchant/payment-providers/:provider`, and their embedded
  `/billing/v1` twins — answers **405** with machine code `manifest_driven`:
  edit the YAML and reboot. Reads stay available, and the routes stay mounted
  so callers get the pointed error, never a bare 404.
- `openrails dump-merchant-config` errors: there is no store to dump — the
  YAML you already hold is the export.
- `ENCRYPTION_MASTER_KEY` / `secret_backend` posture is irrelevant: no
  persistent secret store is ever constructed.

## Rotation walkthrough

1. Rotate the value in your (operator) Vault / secret source.
2. Re-render the mounted file (or env var).
3. Reboot. The next boot re-seeds memory; the next charge uses the new
   credential. A boot with unchanged files is a no-op.

## Mode 2 in one line

`merchant_source: api`: no manifests at boot (a present manifest file refuses
boot: two truths), merchants/catalog mutate over the HTTP APIs, secrets live in Vault
KV or the DEK-encrypted DB store, and a secret backend is REQUIRED outside
development. Initial bootstrap is `openrails push-merchant-config --seed` — a
one-time, create-only import of a manifest file into those stores (the command
refuses without `--seed`; the stores are the truth afterward).
