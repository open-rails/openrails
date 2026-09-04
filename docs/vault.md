# HashiCorp Vault + OpenRails

OpenRails uses Vault for two **independent** capabilities (#661). Grant only what you need.

| Capability | What it's for | Vault paths |
|---|---|---|
| **KV secrets** | store/read per-merchant rail secrets (`secret_backend: vault`) | `secret/data/openrails/*`, `secret/metadata/openrails/*` |
| **Transit signing** | Solana `vault_transit` custody — Vault signs, the key never leaves | `transit/sign/<your-key>`, `transit/keys/<your-key>` |

They are decoupled: run **transit-only** (Vault signs Solana, secrets live in the DB or the boot
manifest), **KV-only**, or both. Mounts are fixed: KV-v2 at `secret`, Transit at `transit`.

## Where merchant secrets live

Where secrets live follows the two-mode doctrine (`merchant_source`, see
[operator-guide.md](operator-guide.md)):

- **`merchant_source: manifest` (MODE 1, default)** — the boot YAML is the truth; secrets are held
  in memory, **no persistent secret store is constructed**, `secret_backend` is not consulted.
  Vault, if enabled, serves Transit signing only.
- **`merchant_source: api` (MODE 2)** — a persistent backend selected by `secret_backend`
  (env `SECRET_BACKEND`), which is **required**:

```yaml
secret_backend: db      # DEK-encrypted Postgres store — or `vault`
vault:
  enabled: true
```

`secret_backend` is **declared intent** — never auto-detected, never auto-fallback (the data lives
in exactly one place; a store that lacks it would run silently empty). `merchant_source: api`
refuses to boot without it; `vault.enabled` is a Vault *connection* (Transit signing counts) and
never stands in for the declaration. `secret_backend: vault` requires `vault.enabled`;
`secret_backend: db` outside development requires `ENCRYPTION_MASTER_KEY` (#667/#723).

### The DB fallback: envelope encryption

With `secret_backend: db`, secrets persist in `openrails.merchant_secrets`, envelope-encrypted:
`encryption.master_key` (env `ENCRYPTION_MASTER_KEY`, base64 of 32 raw bytes, AES-256) wraps a
per-merchant DEK in `openrails.merchant_deks`; the DEK encrypts the values. Without the master
key the store would persist plaintext — **non-development boots refuse this** (#667); development
proceeds with a loud warning. In production, source the master key from a KMS — the wrapped DEKs
stay in the DB, the key that unwraps them never does.

## The custodial model — one process token

OpenRails authenticates to Vault **once, as itself**; there is no per-merchant Vault auth.
Merchant isolation is enforced in OpenRails code by the path addressing (every secret lives under
that merchant's slug subtree). Operational consequences:

- The Vault policy scopes what the **process** may do, not what any merchant may do. Protect the
  app credential accordingly: it can read every merchant's secrets the policy grants.
- Merchants never receive Vault credentials. Their surface is the delegated admin API
  (`/v1/merchant/payment-providers`): secret fields are accepted on write, validated, and
  **redacted on read** — plaintext is never returned.
- All secret paths derive from one builder in code (test-guarded); ad-hoc path construction
  cannot escape a merchant's namespace.

### Authenticating

Pick one `auth_method`. In-cluster, prefer `kubernetes` (no stored secret — the pod's
ServiceAccount is the credential).

```yaml
vault:
  enabled: true
  address: https://vault.internal:8200   # env VAULT_ADDR
  auth_method: kubernetes                # kubernetes | approle | token
  k8s_role: openrails                    # kubernetes: Vault role bound to the pod's ServiceAccount
  # role_id / secret_id                  # approle; a mounted secret FILE named VAULT_SECRET_ID
  #                                      #   is re-read on every re-auth (rotation-safe)
  # token                                # dev/e2e or a sidecar-managed token (env VAULT_TOKEN)
```

The minted token is held in memory only. A background supervisor renews it up to Vault's max TTL
and **re-authenticates** when renewal is no longer possible (#751); auth health feeds readiness
alongside a live reachability re-check of the KV mount.

## Minimal policies

Grant only what the deployment uses (these exact policies are exercised against real Vault ACLs
in the integration suite):

**Transit-only** (`secret_backend: db` or MODE 1; Vault signs Solana only). Scope to exactly the
key(s) declared as `signer.key`:

```hcl
path "transit/sign/my-mainnet-signer" { capabilities = ["update"] }   # sign
path "transit/keys/my-mainnet-signer" { capabilities = ["read"] }     # read the pubkey
# no secret/* access at all
```

**KV secret store** (`secret_backend: vault`):

```hcl
path "secret/data/openrails/*"     { capabilities = ["create","read","update","delete"] }
path "secret/metadata/openrails/*" { capabilities = ["read","delete","list"] }
```

**Combined**: both blocks. Bind the policy to the auth role; the credential is identical
regardless of policy — the narrowing lives entirely in the policy.

### Capability-aware boot

At boot OpenRails probes `sys/capabilities-self` for the KV paths and adapts (advisory only —
Vault's runtime 403 remains the real boundary):

- `secret_backend: vault` + KV read-write → full secret ops. Read-only → boots, but
  merchant-secret writes / config-push are disabled with a warning. **No KV read** → **boot
  error** (declared-in-Vault but unreachable — never run empty; never fall back to the DB).
- Transit is NOT path-probed (key names are yours, so there is no path to guess). A Vault
  connection enables the Solana signing surface; the grant is verified against the real key when
  the PSP is provisioned (pubkey read via `transit/keys/<key>`), and a later policy change
  surfaces as a runtime 403.

## Secret paths and canonical names

Code addresses secrets by `(merchant_id, name)`; the Vault store resolves that to

```
secret/openrails/merchants/<merchant-uuid>/<name>     # value under KV-v2 field "value"
```

Immutable merchant UUIDs own the subtree. Renames change no secret paths, and a new owner of an old slug cannot address the original merchant's secrets. Existing pre-launch slug-based keys must be replaced at the UUID paths; no fallback reads old slug paths.

There is exactly ONE canonical name shape, for every rail (#884 retired the flat
`<rail>/<purpose>` spellings — they were write-only and never read):

| Secret | `<name>` |
|---|---|
| PSP-scoped credential (all rails) | `psps/<rail>/<live\|test>/<account_id>/<key>` |

The `psps/` prefix is the durable per-PSP shape — one merchant can run multiple accounts on a
rail without collisions. `<account_id>` is the operator-declared PSP account id (NMI gateway id,
CCBill `accnum-subacc`, Solana signer address); `<key>` is a rail-registry credential slot
(`security_key`, `salt`, `datalink_username`, `datalink_password`, `private_key`, …). Examples:

```sh
vault kv put secret/openrails/merchants/<merchant-uuid>/psps/nmi/live/<gateway-id>/security_key value="$NMI_SECURITY_KEY"
vault kv put secret/openrails/merchants/<merchant-uuid>/psps/ccbill/live/<accnum-subacc>/salt value="$CCBILL_SALT"
vault kv put secret/openrails/merchants/<merchant-uuid>/psps/stripe/live/<acct-id>/secret_key value="$STRIPE_SECRET_KEY"
```

`psps/solana/<env>/<address>/private_key` (local-keypair signer) is operator-only — it is never
merchant-writable through the dashboard API. Prefer Transit so no such secret exists at all.

**Caching**: all backends are fronted by an in-process 15-minute TTL cache. A write through an
OpenRails node refreshes that node immediately; an out-of-band `vault kv put` converges on every
node within one TTL — no restart needed. Roll the nodes for an instant cluster-wide cutover.

**Error taxonomy** (money-path critical, `errors.Is`): `ErrSecretNotFound` is **terminal** — the
secret is genuinely absent; never retry, never treat as "verification disabled".
`ErrSecretBackendUnavailable` (Vault unreachable/sealed/denied, DB error) is **retryable** —
webhook routes return 503 so the provider redelivers; workers retry rather than cancel.

## Day-2 secret ops

- **Adding a merchant's rail secrets** — MODE 1: edit the boot manifest (or its env/secret-file
  overlay) and reboot. `push-merchant-config` converges DB projection rows only; it does **not**
  persist secrets (the server reads them from its own boot manifest), and `merchant_source: api`
  refuses the command outright. MODE 2: `PUT /v1/merchant/payment-providers/<provider>`, or
  pre-provision with `vault kv put` at the canonical path — OpenRails discovers it lazily.
- **Rotation** — rotate via the admin API (validated, idempotent on value, version-bumped) or
  `vault kv put` directly. KV-v2 keeps prior versions; OpenRails always reads the latest.
- **Solana local keypair** — rotating `psps/solana/.../private_key` changes the merchant's
  on-chain signer identity: existing on-chain subscription authorizations are bound to the old
  key, forcing a plan re-publish and re-enroll. Use Transit instead.
- **DB → Vault migration** — stand up Vault + policy; export existing secrets **through
  OpenRails** (`List` then `Get` per name — the DB values are envelope-encrypted, never `SELECT`
  the column raw); `vault kv put` each at its canonical path; flip `secret_backend: vault` and
  restart; verify per merchant (the admin API's Stripe test action confirms live resolution);
  only then purge the `merchant_secrets`/`merchant_deks` rows (keep a backup).

## Solana Transit signing

The `vault_transit` signer mode (declared per PSP in the manifest — see
[rails/solana.md](rails/solana.md)) keeps the merchant's Ed25519 key inside Vault Transit. The
operator creates the key and names it in the manifest; OpenRails never creates or exports it:

```sh
vault write -f transit/keys/my-mainnet-signer type=ed25519 exportable=false
# manifest: signer: { mode: vault_transit, key: my-mainnet-signer }
```

OpenRails sends the serialized transaction message to `transit/sign/<key>` (raw Ed25519,
`prehashed=false` — exactly what Solana verifies) and reads the public key — which IS the
merchant's on-chain address — from `transit/keys/<key>`. Transit works with either secret backend
and in both merchant-source modes. A Transit key rotation mints a new keypair, so it carries the
same on-chain identity caveat as any signer change.
