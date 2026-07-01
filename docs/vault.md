# HashiCorp Vault + OpenRails

OpenRails uses Vault for two **independent** things (#661). Grant only what you need.

| Capability | What it's for | Vault paths |
|---|---|---|
| **KV secrets** | store/read per-merchant provider secrets | `secret/data/openrails/*`, `secret/metadata/openrails/*` |
| **Transit signing** | Solana `vault_transit` custody — Vault signs, the key never leaves | `transit/sign/<your-key>`, `transit/keys/<your-key>` |

They are decoupled: you can run **transit-only** (Vault signs Solana, secrets live in the DB), **KV-only**, or both.

## Where secrets live: `secret_backend` (declared intent)

```yaml
secret_backend: db      # DEK-encrypted Postgres store (default) — or `vault`
vault:
  enabled: true
```

`secret_backend` (env `SECRET_BACKEND`) declares WHERE merchant secrets physically live. It is **not** auto-detected
and **never** auto-falls-back — the data lives in exactly one place, so falling back to a store that lacks it would
run silently empty. Empty derives from `vault.enabled` for backward-compat. `secret_backend=vault` requires
`vault.enabled`.

Transit signing is orthogonal to this: it works with either backend.

## Authenticating OpenRails to Vault

Pick one `auth_method`. In-cluster, prefer `kubernetes` (no stored secret — the pod's ServiceAccount is the credential).

```yaml
vault:
  enabled: true
  address: https://vault.internal:8200
  auth_method: kubernetes     # kubernetes | approle | token
  k8s_role: openrails         # kubernetes: bind this Vault role to the pod's ServiceAccount
  # role_id / secret_id       # approle
  # token                     # dev/e2e or a Vault-Agent-managed token file
```

OpenRails logs in at boot and holds the minted token in memory (renewed while it can); nothing is persisted.

## Minimal policies — grant only what the deployment uses

**Transit-only** (Vault-custody Solana signing; secrets managed elsewhere, `secret_backend=db`). Key names are
yours — scope the policy to exactly the key(s) declared as `signer.key` in the merchant manifest:

```hcl
path "transit/sign/my-mainnet-signer" { capabilities = ["update"] }   # sign
path "transit/keys/my-mainnet-signer" { capabilities = ["read"] }     # read the pubkey
# no secret/* access at all
```

**KV secret store** (`secret_backend=vault`):

```hcl
path "secret/data/openrails/*"     { capabilities = ["create","read","update","delete"] }
path "secret/metadata/openrails/*" { capabilities = ["read","delete","list"] }
```

**Combined**: both blocks.

Bind the policy to the auth role (`vault write auth/kubernetes/role/openrails … policies=openrails-transit`), and hand
OpenRails the ordinary credential for that role. The credential is identical regardless of policy — the narrowing lives
entirely in the policy.

## Capability-aware behavior

At boot OpenRails probes `sys/capabilities-self` for the KV paths and adapts (advisory only — Vault's runtime 403 is
the real boundary):

- `secret_backend=vault` + KV read-write → full secret ops; + read-only → writes/config-push disabled + warn; + **no
  KV** → **boot error** (declared-in-Vault but unreachable — never run empty).
- Transit is NOT path-probed (key names are yours, so there is no path to guess). A Vault connection enables the
  Solana signing surface; the actual grant is verified against the real key when the provider account is provisioned
  (the pubkey is read via `transit/keys/<your-key>`), and a policy change afterwards surfaces as a runtime 403.

Mounts are currently fixed: KV `secret`, Transit `transit`.
