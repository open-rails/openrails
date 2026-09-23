# Self-hosting with host-owned credential snapshots

Set `secret_backend: snapshot` when the host supplies provider credentials at
startup. Values stay in process memory. Separate workers load their own snapshot;
PostgreSQL does not share those values between processes. Updating credentials
requires a new snapshot and provider/account validation.

Merchant metadata remains database-owned. Startup initializes missing identities
and metadata, preserves later API edits and archived accounts, and never prunes
undeclared providers. Use explicit metadata applications for deliberate updates:
[merchant configuration applications](merchant-configuration-applications.md).

Credential custody, authorization and external HTTP publication are independent.
`merchant_config_http` selects standalone configuration HTTP; embedded hosts select
`MerchantConfig` in their HTTP route configuration. Authorized local Client metadata
operations remain available when external configuration HTTP is off. Credential
writes require a writable managed backend.

## The three files

| File | Owns | Loaded by |
|---|---|---|
| `config.yaml` | process/infrastructure config (DB, Redis, `provider_write_mode`, `test_mode`, `secret_backend`, `merchant_config_http`, `allow_catalog_updates`) | `config.Load` (standalone) / built programmatically (embedded hosts) |
| merchant manifest (`/etc/openrails/merchants.yaml`, or `run-server` / `run-worker --merchant-manifest <path>`) | merchant identity, profile, invoice policy, **PSPs** — rail accounts + secrets (`merchants.<slug>.psps.<key>.<rail>`) | standalone server and worker boot, every boot; embedded hosts pass the same shape to `Options.Merchant.Config` |
| catalog manifest (`/etc/openrails/catalog.yaml`) | products / prices / entitlements / PSP links | `openrails apply-catalog --merchant NAME --file PATH` (or the trusted operator wrapper) |

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
boot refuses otherwise. Managed backends accept metadata initialization; provider
credential declarations require the separate Client publication operation.

Separate worker processes load the same manifest and overlays as API processes;
in-memory provider credentials are not shared through PostgreSQL. Both commands
accept `--merchant-manifest`. Each process must receive the same configured
manifest and secret files before it can use those providers.

1. The manifest and its overlays parse strictly. Unknown fields and retired
   key names (the old `accounts:` key — renamed to `psps:`) refuse boot — never
   a silent drop.
2. Missing merchant/provider identities and metadata initialize in PostgreSQL.
   Existing names, profile, routing, policy and archive decisions are preserved.
3. Secrets are seeded **into memory** (the runtime manifest secret plane) and
   served through the same store interface every consumer reads — checkout,
   webhook verification, provider pulls, rebill charging. Nothing is written
   to `openrails.merchant_secrets` or Vault KV.
4. A declared secret that resolves to an empty value (no YAML value, no
   mounted file, no env var) is a boot **error**. A PSP declared without a
   given secret seeds nothing for it — requests needing it fail closed at use
   time. Under `test_mode`, NMI accounts are probed before arming: production
   credentials refuse to arm.

## Capabilities

- Snapshot credentials are read-only through both Client transports. Metadata
  edits and provider archive decisions retain their ordinary authorization checks.
- External configuration routes require explicit publication; exposing a route
  does not make the credential backend writable.
- Catalog writes independently require `allow_catalog_updates: true`. Trusted
  operator catalog applications retain their own durable identity contract.
- `openrails dump-merchant-config` exports redacted metadata with snapshot or
  managed credentials. Plaintext credential export is not supported.
- Managed DB secrets require encryption in sandbox and live deployments.
  Host-owned snapshot values are not copied into a managed store.

## Rotation walkthrough

1. Rotate the value in your (operator) Vault / secret source.
2. Re-render the mounted file (or env var).
3. Restart with the new snapshot. Validate its provider/account/environment
   identity before use. Existing metadata and archive decisions are preserved.

## Managed credentials

Choose `secret_backend: vault` or encrypted `db` for managed credentials. Publish
validated candidates through the Client payment-provider operation. Actual backend
permissions determine write capability. External HTTP publication remains a
separate choice. Custody changes require an explicit transition preserving active
credentials and existing provider obligations.
