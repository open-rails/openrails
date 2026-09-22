# Self-hosting MODE 1: manifest-is-truth (`merchant_config_source: manifest`)

MODE 1 is the default authority for merchant and provider configuration. The
host supplies the configuration and credentials at boot; provider credentials
stay in memory for the life of the runtime. Changing them means updating the
host configuration and restarting. OpenRails persists provider metadata for
references, but does not copy these credentials into its managed secret store.

Catalog authority defaults to the same mode. Set `catalog_source: api` when
products and prices should change dynamically while provider credentials remain
host-owned. The opposite credential posture — API-driven merchant configuration
with a Vault/DB secret store — is MODE 2 (`merchant_config_source: api`). Comparison table:
[standalone-integration.md](standalone-integration.md#two-merchant-source-modes).
Deployment shape does not imply mode: embedded and standalone can run either.

## The three files

| File | Owns | Loaded by |
|---|---|---|
| `config.yaml` | process/infrastructure config (env, DB, Redis, `provider_write_mode`, `test_mode`, `merchant_config_source`, `catalog_source`) | `config.Load` (standalone) / built programmatically (embedded hosts) |
| merchant manifest (`/etc/openrails/merchants.yaml`, or `run-server` / `run-worker --merchant-manifest <path>`) | merchant identity, profile, invoice policy, **PSPs** — rail accounts + secrets (`merchants.<slug>.psps.<key>.<rail>`) | standalone server and worker boot, every boot; embedded hosts pass the same shape to `Options.Merchant.Config` |
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
boot refuses otherwise. `merchant_config_source: api` (MODE 2) refuses a present
manifest outright: two truths.

Separate worker processes load the same manifest and overlays as API processes;
in-memory provider credentials are not shared through PostgreSQL. Both commands
accept `--merchant-manifest`. Each process must receive the same configured
manifest and secret files before it can use those providers.

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

- Payment-provider **mutation** routes are not mounted: provider PUT/DELETE
  and account archive are absent from both standalone and embedded surfaces.
  Update host configuration and restart. Provider reads and routing dry runs
  stay available. Requests to omitted routes receive the router's ordinary
  404 or 405; the advertised `secret_write` capability is false. Optional
  managed alert-webhook URLs remain independently editable when configured.
- Catalog APIs also return 405 by default. Set `catalog_source: api` to permit
  authorized dynamic product/price/metering edits while keeping provider
  credentials host-owned. `catalog_source: manifest` uses the catalog push
  command and refuses catalog API writes, including `POST /catalog/publish`.
- `openrails dump-merchant-config` errors: there is no store to dump — the
  YAML you already hold is the export.
- Provider credentials need no `ENCRYPTION_MASTER_KEY`. Optional managed
  alert-webhook URLs use `secret_backend` and require encryption when stored in
  the DB; HyperSwitch SDK capture authorization also requires DB encryption.
  Without that protection, those features refuse secret persistence while
  host-owned Stripe credentials and ordinary catalog operations remain usable.

## Rotation walkthrough

1. Rotate the value in your (operator) Vault / secret source.
2. Re-render the mounted file (or env var).
3. Reboot. The next boot re-seeds memory; the next charge uses the new
   credential. A boot with unchanged files is a no-op.

## Mode 2 in one line

`merchant_config_source: api`: no manifests at boot (a present manifest file refuses
boot: two truths), merchant/provider configuration mutates over the HTTP APIs,
and secrets live in the explicitly selected Vault KV or DB store. DB encryption
is required outside development. Catalog APIs are enabled by default; an
explicit `catalog_source: manifest` instead gives catalog authority to a separate
catalog manifest. Initial bootstrap is `openrails push-merchant-config --seed` — a
one-time, create-only import of a manifest file into those stores (the command
refuses without `--seed`; the stores are the truth afterward).
