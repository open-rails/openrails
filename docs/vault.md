# HashiCorp Vault + OpenRails

OpenRails uses Vault for two **independent** capabilities (#661). Grant only what you need.

| Capability | What it's for | Vault paths |
|---|---|---|
| **KV secrets** | store/read per-merchant rail secrets (`secret_backend: vault`) | `secret/data/openrails/*`, `secret/metadata/openrails/*` |
| **Transit signing** | Solana `vault_transit` custody — Vault signs, the key never leaves | `transit/sign/<your-key>`, `transit/keys/<your-key>` |

They are decoupled: run **transit-only** (Vault signs Solana, secrets live in the DB or the boot
manifest), **KV-only**, or both. Mounts default to `secret` for KV-v2 and `transit` for Transit;
override them with `vault.kv_mount` and `vault.transit_mount` when needed.

## Where merchant secrets live

Credential custody is independent of merchant metadata and HTTP publication:

- **`secret_backend: snapshot` (default)** — the host supplies credentials at
  startup. Values stay in memory and ordinary Client operations cannot change
  them. Vault may still provide Transit signing.
- **`secret_backend: db`** — managed credentials are envelope-encrypted in
  PostgreSQL. `encryption.master_key` (`ENCRYPTION_MASTER_KEY`, base64 of 32 raw
  bytes) is required in both sandbox and live posture. A per-merchant DEK encrypts
  values; the configured master key wraps that DEK.
- **`secret_backend: vault`** — managed credentials use exact published KV-v2
  references. Set `vault.enabled` and the server-owned connection/auth settings.

`credential_read_only: true` narrows a managed backend to reads. Vault policy
can narrow it further. External merchant configuration routes are a separate
choice: embedded `Options.HTTP.MerchantConfig`, or standalone
`merchant_config_http`. Authorized local Client operations do not require those
HTTP routes. Remote Clients connect to the remote server and need no local Vault
or database configuration.

There is no backend auto-fallback. Changing backend configuration does not move
credentials; use the explicit custody transition described below. Snapshot
provider credentials can coexist with independently managed alert secrets using
`alert_secret_backend`.

## The custodial model — one process token

OpenRails authenticates to Vault **once, as itself**; there is no per-merchant Vault auth.
Merchant isolation is enforced in OpenRails code by the path addressing (every secret lives under
that merchant's UUID subtree). Operational consequences:

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
  # namespace: billing                  # optional Vault namespace
  # scope_prefix: openrails              # optional server-owned path prefix
  auth_method: kubernetes                # kubernetes | approle | token
  k8s_role: openrails                    # kubernetes: Vault role bound to the pod's ServiceAccount
  # role_id / secret_id                  # approle; a mounted secret FILE named VAULT_SECRET_ID
  #                                      #   is re-read on every re-auth (rotation-safe)
  # token                                # dev/e2e or a sidecar-managed token (env VAULT_TOKEN)
```

The minted token is held in memory only. Login never blocks startup: a background supervisor logs
in, renews up to Vault's max TTL and **re-authenticates** when renewal is no longer possible (#751),
retrying with capped full-jitter backoff forever. Until it succeeds, Transit/KV operations fail fast
with `vault.ErrUnavailable` (Solana signing answers 503) and readiness stays green. A Transit signer
whose PSP is already stored keeps its identity while Vault is down; on a first boot only that PSP
waits and is provisioned once Vault answers. Embedded hosts register the `openrails_vault` probe
from `Runtime.Probes()`.

## Minimal policies

Grant only what the deployment uses (these exact policies are exercised against real Vault ACLs
in the integration suite):

**Transit-only** (`secret_backend: db` or `snapshot`; Vault signs Solana only). Scope to exactly the
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
  credential publication is disabled; ordinary database metadata remains editable. **No KV read** → **boot
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

Logical credential slots have the shape
`psps/<rail>/<live|test>/<account_id>/<key>`. Accounts are operator-declared,
not selected by request-supplied Vault paths. Managed publication stores immutable
operation-named candidates and publishes their exact names and versions in
PostgreSQL. Secret values are write-only at the Client/API boundary and never go
into publication receipts.

Use `Client.PaymentProviders.Upsert` with a stable `operation_id` and the observed
`expected_revision`. Reuse that operation ID to recover a lost reply. Competing
updates fail with a revision conflict; they do not silently overwrite each other.
A failed SQL publication leaves the candidate inactive and preserves the old
active references. The API cannot change Vault addresses, mounts or prefixes.

**Reads** use the published exact reference. They neither accept a newer
unpublished version nor substitute another backend. Managed versioned reads
consult the backend, so cached values cannot hide revoked backend access. Direct
`vault kv put` does not publish a new active credential. Snapshot values are
reloaded from the host's supplied snapshot on restart.

**Error taxonomy** (money-path critical, `errors.Is`): `ErrSecretNotFound` is **terminal** — the
secret is genuinely absent; never retry, never treat as "verification disabled".
`ErrSecretBackendUnavailable` (Vault unreachable/sealed/denied, DB error) is **retryable** —
webhook routes return 503 so the provider redelivers; workers retry rather than cancel.

## Day-2 secret operations

- **Snapshot update:** change the host's credential source and restart with the
  new snapshot. Startup preserves existing merchant metadata and archived provider
  state; it does not make the YAML an automatic overwrite/prune operation.
- **Managed publication/rotation:** use `Client.PaymentProviders.Upsert` locally
  or remotely with explicit operation identity and revision. Webhook rotations
  retain required overlap; another rotation cannot discard an unretired prior key.
- **Custody transition:** the local operator supplies source and target runtimes
  to `TransitionProviderCredentials`. It copies and validates active/overlap
  credentials, then publishes target custody with revision checks. A backend flag
  flip alone refuses existing references. Source secrets are retained; do not
  delete historical material or old databases as part of the transition.
- **Managed to snapshot:** preload the target runtime using
  `Options.ProviderCredentials` and a stable `CredentialSnapshotID`. The label is
  an operator assertion; the host must supply the same snapshot on restart. The
  transition verifies the supplied credentials before publishing target custody.
- **Solana local keypair:** changing the signer changes the on-chain identity.
  Existing authorizations remain bound to the old key. Prefer Transit custody and
  handle signer changes as explicit operations, not an incidental secret refresh.

See [merchant metadata applications](merchant-configuration-applications.md) for
settings changes, replay and the local/remote CLI. Metadata changes and credential
publication are separate operations.

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
merchant's on-chain address — from `transit/keys/<key>`. Transit is independent of
the selected credential backend. A Transit key rotation mints a new keypair, so it carries the
same on-chain identity caveat as any signer change.

## Merchant purge cleanup

A committed merchant purge records its immutable Vault address/namespace and UUID
root in `maintenance_runs.coverage.secret_cleanup`. Database completion is stored
in `affected.database_purged`; the original authorization/inventory proof remains
immutable. The run stays `running` or `failed` until every secret version has been
deleted and a fresh root listing is empty. An external failure returns
`merchant secret cleanup pending` instead of reporting a complete purge.

The `openrails.merchant_secret_cleanup` worker retries committed, tombstoned
purges every five minutes and on startup. It re-lists the captured UUID root, so
credentials written after inventory but before tombstoning are included. A
changed Vault address, namespace, or mount is refused: restore the original
backend configuration to finish its outstanding cleanup. No secret values are
stored in the journal.

Runtime Vault writes and purge tombstoning take the same merchant row lock.
Writes that finish first are swept; writes after tombstoning are rejected.
Request-pinned database connections are reused. Direct operator writes to Vault
bypass OpenRails lifecycle locks: stop writing a purged UUID's subtree and remove
any per-merchant Vault credentials/policies before erasure. The process-level
Vault credential cannot prevent an independently authorized operator from
recreating a secret after cleanup.

This does not enable merchant purge on an HTTP or CLI surface. Its existing
inventory/confirmation/destructive-policy requirements and unwired-purge guard
remain in place.
