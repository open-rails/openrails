# Vault per-tenant secrets — ops runbook (#251)

OpenRails resolves per-tenant processor secrets (Stripe keys, webhook signing
secrets, Solana keys) through `tenancy.TenantSecretStore`, addressed by
`(tenant_id, name)` in code. Vault stores use the stable tenant slug in the
physical path so manual operator writes are deterministic and readable. Three
backends ship behind one interface:

| Backend | Selected when | At rest |
|---|---|---|
| `dbSecretStore` + `encryptedSecretStore` | **default / self-hosted** (no Vault config) | `billing.tenant_secrets`, envelope-encrypted (master key wraps per-tenant DEK in `billing.tenant_deks`) |
| `vaultSecretStore` (KV-v2) | `vault.enabled: true` | Vault KV-v2 at `secret/openrails/tenants/<tenant-slug>/<name>` |
| `memSecretStore` | tests | in-process |

All three are fronted by an in-process **TTL cache** (`cachedSecretStore`,
default 15 minutes, `vault.secret_cache_ttl_seconds`) so a worker or webhook
handler resolves a tenant's secret once per window instead of per row/request. A
rotation *through this node* invalidates the entry immediately; an out-of-band
admin Vault write or rotation on another node converges within one TTL. Set the
TTL negative to disable. Vault WebSocket/event notifications are not required for
the baseline design.

> **Caller addressing is identical across backends.** Switching to Vault changes
> neither the code-level `(tenant_id, name)` calls nor the secret *names*
> (`stripe/secret_key`, `stripe/webhook_signing_secret`, …). Only the physical
> Vault path uses `<tenant-slug>`.

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
  secret_cache_ttl_seconds: 900
```

The app authenticates **once** as itself (AppRole / K8s service-account JWT) and
is the trusted broker; tenant isolation is enforced in code by resolving the
tenant id to its stable slug and using the `(tenant_slug, name)` Vault path, not
by per-tenant Vault auth. The token is renewed in the background and re-acquired
on expiry.

A suggested per-tenant policy (future BYO-key self-service) scopes an operator to
only its subtree:

```hcl
path "secret/data/openrails/tenants/<tenant-slug>/*"     { capabilities = ["read"] }
path "secret/metadata/openrails/tenants/<tenant-slug>/*" { capabilities = ["list"] }
```

## Canonical secret names and paths

OpenRails owns the secret names. Vault-backed installs store each value under the
KV-v2 field `value`:

| Secret | Vault path example | Tenant dashboard writable |
|---|---|---|
| Stripe API key | `secret/openrails/tenants/cozy-art/stripe/secret_key` | yes |
| Stripe webhook signing secret | `secret/openrails/tenants/cozy-art/stripe/webhook_signing_secret` | yes |
| Stripe thin-event signing secret | `secret/openrails/tenants/cozy-art/stripe/webhook_signing_secret_thin` | yes |
| Mobius/NMI production key | `secret/openrails/tenants/doujins/nmi/mobius/production_key` | yes |
| CCBill account config JSON | `secret/openrails/tenants/doujins/ccbill/account_config` | yes |
| Solana signing keypair | `secret/openrails/tenants/default/solana/private_key` | no |

`solana/private_key` remains an OpenRails internal/platform-admin secret for
signing compatibility. It is in the canonical registry so platform tools and
startup seed paths can still address it, but delegated tenant-admin APIs do not
expose it as a writable dashboard secret.

## Manual placement examples

An operator can preprovision provider credentials before a tenant ever opens a
dashboard:

```sh
vault kv put secret/openrails/tenants/doujins/nmi/mobius/production_key value="$MOBIUS_PRODUCTION_KEY"
vault kv put secret/openrails/tenants/doujins/ccbill/account_config value='{"client_acc_num":"...","client_sub_acc":"...","salt":"..."}'
vault kv put secret/openrails/tenants/tensorhub/stripe/secret_key value="$STRIPE_SECRET_KEY"
vault kv put secret/openrails/tenants/tensorhub/stripe/webhook_signing_secret value="$STRIPE_WEBHOOK_SECRET"
```

OpenRails discovers those values lazily by tenant slug on first use or status
read, then keeps the value in memory until the cache TTL expires. No app restart
is required for out-of-band Vault writes; convergence is bounded by
`vault.secret_cache_ttl_seconds`.

## Tenant-admin write-only API examples

Dashboard callers use tenant-signed delegated access tokens with the exact
tenant-secret permissions from the control-plane catalog. These endpoints never
return plaintext secret values.

List configured status:

```sh
curl -H "Authorization: Bearer $DELEGATED_ADMIN_JWT" \
  "$OPENRAILS_URL/v1/tenant-admin/secrets"
```

Validate before saving:

```sh
curl -X PUT "$OPENRAILS_URL/v1/tenant-admin/secrets/stripe/secret_key" \
  -H "Authorization: Bearer $DELEGATED_ADMIN_JWT" \
  -H "Content-Type: application/json" \
  -d '{"value":"sk_live_...","validate_only":true}'
```

Save and validate:

```sh
curl -X PUT "$OPENRAILS_URL/v1/tenant-admin/secrets/stripe/secret_key" \
  -H "Authorization: Bearer $DELEGATED_ADMIN_JWT" \
  -H "Content-Type: application/json" \
  -d '{"value":"sk_live_...","save_and_validate":true}'
```

Delete a configured secret:

```sh
curl -X DELETE "$OPENRAILS_URL/v1/tenant-admin/secrets/stripe/webhook_signing_secret" \
  -H "Authorization: Bearer $DELEGATED_ADMIN_JWT"
```

## Migration: move DB-stored secrets into Vault

Greenfield installs skip this. For an existing self-hosted install moving to
managed Vault:

1. **Stand up Vault**, enable KV-v2 at `secret/`, configure app auth (AppRole/K8s).
2. **Export** existing secrets from `billing.tenant_secrets`. They are
   envelope-encrypted, so decrypt through OpenRails rather than reading the column
   raw — enumerate with `Service` / `TenantSecretStore.List(tenant)` then `Get`
   each name (returns plaintext via the encryptor). Do NOT `SELECT value` directly.
3. **Write** each into Vault at the canonical slug address:
   `vault kv put secret/openrails/tenants/<tenant-slug>/<name> value=<plaintext>`
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
  `vault kv put secret/openrails/tenants/<tenant-slug>/<name> value=<new>`. KV-v2 keeps the
  prior version; OpenRails always reads the latest.
- A rotation through an OpenRails node refreshes that node's cache immediately;
  other nodes pick it up within one cache TTL (default 15m). For an instant
  cluster-wide cutover, set `secret_cache_ttl_seconds` low or roll the nodes.
- **Solana signing key:** rotating `solana/private_key` changes the tenant's
  on-chain merchant/signer identity. It **forces a plan re-publish and re-enroll**
  — existing on-chain subscription authorizations are bound to the old key and do
  not transfer. Prefer Vault **Transit** (`use_transit_for_solana: true`) so the
  key never leaves Vault and rotation is a Transit key-version bump.
