# Vault per-tenant secrets — ops runbook (#251)

OpenRails resolves per-tenant processor secrets (Stripe keys, webhook signing
secrets, Solana keys) through `tenancy.TenantSecretStore`, addressed by
`(tenant_id, name)`. Three backends ship behind one interface:

| Backend | Selected when | At rest |
|---|---|---|
| `dbSecretStore` + `encryptedSecretStore` | **default / self-hosted** (no Vault config) | `billing.tenant_secrets`, envelope-encrypted (master key wraps per-tenant DEK in `billing.tenant_deks`) |
| `vaultSecretStore` (KV-v2) | `vault.enabled: true` | Vault KV-v2 at `secret/openrails/tenants/<tenant-id>/<name>` |
| `memSecretStore` | tests | in-process |

All three are fronted by an in-process **TTL cache** (`cachedSecretStore`,
default 45s, `vault.secret_cache_ttl_seconds`) so a worker scanning many rows for
one tenant resolves the secret once per window. A rotation *through this node*
invalidates the entry immediately; a rotation on another node converges within one
TTL. Set the TTL negative to disable.

> **Addressing is identical across backends.** Switching to Vault changes neither
> the secret *names* (`stripe/secret_key`, `stripe/webhook_signing_secret`, …) nor
> any caller. Only `server.go` wiring changes.

## Self-hosted is unchanged

With no `vault` block (or `vault.enabled: false`), OpenRails builds
`dbSecretStore` wrapped in `encryptedSecretStore` exactly as before #251 — the
cache decorator is semantically transparent (it only elides reads within the TTL).
No migration, no Vault, no behaviour change.

## Error taxonomy — why it matters on the money path

Callers MUST distinguish two failure modes (`errors.Is`):

- `tenancy.ErrSecretNotFound` — **terminal**: the secret is genuinely absent
  (never configured / tenant deprovisioned). Never retry; **never** treat as
  "verification disabled".
- `tenancy.ErrSecretBackendUnavailable` — **retryable**: Vault unreachable /
  sealed / permission-denied, a DB error, or a not-yet-wired Vault client
  (`ErrVaultNotConfigured` wraps this). Retry; do **not** cancel a subscription,
  suspend a tenant, or skip a webhook signature check.

Wired examples: the tenant Stripe webhook route returns **503 (retryable,
Stripe redelivers)** on `ErrSecretBackendUnavailable` vs 500/401 otherwise; the
Solana submitter retries rather than cancelling a sub on a backend blip.

## Enabling Vault (managed)

```yaml
vault:
  enabled: true
  address: https://vault.internal:8200
  auth_method: approle        # or "kubernetes" / "token"
  role_id: <approle-role-id>
  secret_id: <approle-secret-id>
  kv_mount: secret            # KV-v2 mount (default "secret")
  transit_mount: transit      # only if use_transit_for_solana
  use_transit_for_solana: true
  secret_cache_ttl_seconds: 45
```

The app authenticates **once** as itself (AppRole / K8s service-account JWT) and
is the trusted broker; tenant isolation is enforced in code by the `(tenant_id,
name)` path, not by per-tenant Vault auth. The token is renewed in the background
and re-acquired on expiry.

A suggested per-tenant policy (future BYO-key self-service) scopes an operator to
only its subtree:

```hcl
path "secret/data/openrails/tenants/<tenant-id>/*"     { capabilities = ["read"] }
path "secret/metadata/openrails/tenants/<tenant-id>/*" { capabilities = ["list"] }
```

## Migration: move DB-stored secrets into Vault

Greenfield installs skip this. For an existing self-hosted install moving to
managed Vault:

1. **Stand up Vault**, enable KV-v2 at `secret/`, configure app auth (AppRole/K8s).
2. **Export** existing secrets from `billing.tenant_secrets`. They are
   envelope-encrypted, so decrypt through OpenRails rather than reading the column
   raw — enumerate with `Service` / `TenantSecretStore.List(tenant)` then `Get`
   each name (returns plaintext via the encryptor). Do NOT `SELECT value` directly.
3. **Write** each into Vault at the SAME address:
   `vault kv put secret/openrails/tenants/<tenant-id>/<name> value=<plaintext>`
   (the store keys the value under `"value"`). Start with the `default` tenant.
4. **Flip** `vault.enabled: true` and restart. The store now resolves from Vault;
   names and callers are unchanged.
5. **Verify** per tenant: `PutCredential`'s `test` action (Stripe balance check)
   confirms the live key resolves from Vault.
6. **Decommission** the DB rows only after verification (keep a backup): the
   envelope-encrypted `billing.tenant_secrets` / `billing.tenant_deks` rows can be
   purged once every tenant resolves from Vault.

## Rotation (KV-v2 versions)

- Rotate via the admin credential API (`RotateCredential` → `Put`) or directly:
  `vault kv put secret/openrails/tenants/<id>/<name> value=<new>`. KV-v2 keeps the
  prior version; OpenRails always reads the latest.
- A rotation through an OpenRails node refreshes that node's cache immediately;
  other nodes pick it up within one cache TTL (default 45s). For an instant
  cluster-wide cutover, set `secret_cache_ttl_seconds` low or roll the nodes.
- **Solana signing key:** rotating `solana/private_key` changes the tenant's
  on-chain merchant/signer identity. It **forces a plan re-publish and re-enroll**
  — existing on-chain subscription authorizations are bound to the old key and do
  not transfer. Prefer Vault **Transit** (`use_transit_for_solana: true`) so the
  key never leaves Vault and rotation is a Transit key-version bump.
