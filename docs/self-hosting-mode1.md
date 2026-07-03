# Self-hosting MODE 1: manifest-is-truth (`merchant_source: manifest`)

MODE 1 (#723) is the default OpenRails deployment shape and the one doujins /
hentai0 run: the YAML you supply at boot **is** the source of truth, held in
memory for the life of the process. OpenRails writes nothing back — there is no
merchant-secret store, no seed-once dance, no store-vs-file divergence.

**Change anything = edit the file(s) + reboot.** This is rarely-changing
CONFIG, not data. The opposite posture — API-driven merchants with a Vault/DB
secret store — is MODE 2 (`merchant_source: api`, #724).

## The three files

| File | Owns | Loaded by |
|---|---|---|
| `config.yaml` | process/infrastructure config (env, DB, Redis, `provider_write_mode`, `test_mode`, `merchant_source`) | `config.Load` (standalone) / built programmatically (embedded hosts) |
| merchant manifest (`/etc/openrails/merchants.yaml`) | merchant identity, profile, invoice policy, **rail accounts + secrets** (`merchants.<slug>.accounts.<key>.<rail>`) | standalone server boot; embedded hosts pass the same shape to `UpsertMerchantConfig` |
| catalog manifest | products / prices / entitlements / meters | `push-merchant-catalog` CLI or `embedded.PushMerchantCatalog` (doujins `PushCatalog`) |

## The secrets directory

Secret VALUES do not belong in the YAML files. Render them (Vault Agent
template, k8s secret volume, CSI, …) into a directory mounted read-only in the
container — one file per secret, **filename = the env-var name**, content =
the value:

```
/vault/secrets/                      # override with VAULT_SECRETS_PATH
  BILLING_MERCHANTS_DOUJINS_ACCOUNTS_MOBIUS_NMI_SECRETS_SECURITY_KEY
  BILLING_MERCHANTS_DOUJINS_ACCOUNTS_CCBILL_CCBILL_SECRETS_DATALINK_PASSWORD
  ENCRYPTION_MASTER_KEY              # NOT needed in mode 1 (nothing persists)
```

OpenRails needs **no live Vault connection** at runtime (Vault Transit for
Solana signing is the one optional exception).

## Precedence

For every merchant-manifest value:

```
yaml  <  secret files  <  env
```

The manifest YAML is the base; mounted secret files overlay it; real
`BILLING_MERCHANTS_*` environment variables win over both.

## What happens at boot

1. The manifest parses strictly (unknown fields, renamed keys, and unroutable
   `BILLING_MERCHANTS_*` names refuse boot — never a silent drop).
2. Merchant rows, `merchant_configurations`, and `rail_merchant_accounts`
   converge into Postgres — these rows are **projections for foreign keys
   only**, steamrolled by the YAML every boot (Insert+Overwrite+Prune). The
   catalog converges the same way (deterministic natural-key ids keep the
   projection stable across rebuilds).
3. Secrets stay **in memory** (`Runtime.ManifestSecrets`), served through the
   same store interface every consumer already reads — checkout, webhooks,
   provider pulls (#699), arrears/rebill charging (#725/#730). Nothing is
   written to `openrails.merchant_secrets` or Vault KV.
4. A declared account whose secret is missing from files/env arms **nothing**
   for that rail: one loud WARN, the rail's fetcher is absent, charges on it
   fail closed. Boot proceeds.

## What is rejected in mode 1

- `PUT/DELETE /v1/merchant/payment-providers/:provider`, catalog
  create/update/activate/deactivate/publish (and their embedded `/billing/v1`
  twins) answer **405** with machine code `manifest_driven`: edit the YAML and
  reboot. Reads stay available.
- `dump-merchant-config` (there is no store to dump — you already hold the
  YAML).
- `ENCRYPTION_MASTER_KEY` posture is irrelevant: nothing is persisted, so the
  #667 encryption gate does not apply in this mode.

## Rotation walkthrough

1. Rotate the value in your (operator) Vault / secret source.
2. Re-render the mounted file (or env).
3. Reboot the pods. The next boot re-seeds memory; the next charge uses the
   new credential. A third boot with unchanged files is a no-op.

## Mode 2 in one line

`merchant_source: api` (#724): no manifests at boot (their presence refuses
boot — two truths), merchants/catalog mutate over the HTTP APIs, secrets live
in Vault KV (or the DEK-encrypted DB store), and a secret backend is REQUIRED
outside development.
