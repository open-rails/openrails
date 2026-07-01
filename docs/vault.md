# HashiCorp Vault + OpenRails

OpenRails uses Vault for two **independent** things (#661). Grant only what you need.

| Capability | What it's for | Vault paths |
|---|---|---|
| **KV secrets** | store/read per-merchant provider secrets | `secret/data/openrails/*`, `secret/metadata/openrails/*` |
| **Transit signing** | Solana `vault_transit` custody — Vault signs, the key never leaves | `transit/sign/openrails-*`, `transit/keys/openrails-*` |

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

**Transit-only** (Vault-custody Solana signing; secrets managed elsewhere, `secret_backend=db`):

```hcl
path "transit/sign/openrails-*" { capabilities = ["update"] }   # sign
path "transit/keys/openrails-*" { capabilities = ["read"] }     # read the pubkey
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

At boot OpenRails probes `sys/capabilities-self` and adapts (advisory only — Vault's runtime 403 is the real boundary):

- transit capability present → Solana `vault_transit` signing works; absent → warned, and Solana falls back to a local
  key if one exists (else Solana is unavailable).
- `secret_backend=vault` + KV read-write → full secret ops; + read-only → writes/config-push disabled + warn; + **no
  KV** → **boot error** (declared-in-Vault but unreachable — never run empty).

Mounts are currently fixed: KV `secret`, Transit `transit`.
