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

## The secrets directory

Secret VALUES do not belong in the YAML. Render them (Vault Agent template,
k8s secret volume, CSI, …) into a directory mounted read-only in the container
— one file per secret, **filename = the env-var name**, content = the value:

```text
/vault/secrets/                      # override with VAULT_SECRETS_PATH
  BILLING_MERCHANTS_MYAPP_PSPS_MOBIUS_NMI_SECRETS_SECURITY_KEY
  BILLING_MERCHANTS_MYAPP_PSPS_CCBILL_CCBILL_SECRETS_DATALINK_PASSWORD
```

OpenRails needs **no live Vault connection** at runtime (Vault Transit for
Solana signing is the one optional exception — KV is never consulted).

## Precedence

For every merchant-manifest value: `yaml < secret files < env`. The manifest
YAML is the base; mounted secret files overlay it; real `BILLING_MERCHANTS_*`
environment variables win over both.

## What happens at boot

The conventional file is optional: absent, the server boots control-plane-only
(bind merchants later). An explicit `--merchant-manifest` path must exist —
boot refuses otherwise. `merchant_source: api` (MODE 2) refuses a present
manifest outright: two truths.

1. The manifest parses strictly. Unknown fields, retired key names (the old
   `accounts:` key and `_ACCOUNTS_` / `_RAIL_MERCHANT_ACCOUNTS_` env anchors —
   renamed to `psps:` / `_PSPS_`), and any `BILLING_MERCHANTS_*` name that
   routes to no manifest field all refuse boot — never a silent drop.
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

`merchant_source: api`: no manifests at boot (their presence — file,
`BILLING_MERCHANTS_*` env, or mounted secret files — refuses boot: two
truths), merchants/catalog mutate over the HTTP APIs, secrets live in Vault
KV or the DEK-encrypted DB store, and a secret backend is REQUIRED outside
development. Initial bootstrap is `openrails push-merchant-config --seed` — a
one-time, create-only import of a manifest file into those stores (the command
refuses without `--seed`; the stores are the truth afterward).
