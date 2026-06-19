# Solana Signing & Vault Integration — Concrete Sketch

Companion to [`solana-subscriptions-plan.md`](./solana-subscriptions-plan.md) §8.
Sketches the **two `Signer` implementations** and the **live `VaultKV` adapter**.
Code blocks are illustrative (the `hashicorp/vault/api` dependency is not yet
added); they show interfaces, file layout, and the Solana/Ed25519-specific
details that are easy to get wrong.

Covers issues **#253** (merchant signer + tx builder) and **#251** (Vault KV adapter).

---

## 0. The shape of the decision

```
                     solana.Signer  (one interface, two impls)
                     ├── keypairSigner   "give me the key"  — works EVERYWHERE
                     │     loads solana/private_key from merchants.MerchantSecretStore
                     │     (DB+envelope self-hosted, or Vault KV managed), signs in-proc
                     └── transitSigner   "sign this"        — RECOMMENDED in prod
                           calls Vault transit/sign/<tenant-key>; key never leaves Vault
```

Both are selected per-deployment by config. The tx-builder and pull worker only
see `solana.Signer` — they never touch a private key.

---

## 1. The `Signer` interface (`internal/integrations/solana/signer.go`)

Remote signing needs **message-level** access (you can't hand Vault a private
key), so the interface is `PublicKey` + `SignMessage`, not `Sign(tx)`. The
tx-builder orchestrates: build → serialize message → `SignMessage` → attach.

```go
package solana

import (
    "context"

    solanago "github.com/gagliardetto/solana-go"
    "github.com/open-rails/openrails/pkg/tenant"
)

// Signer produces Solana signatures for a merchant key WITHOUT
// exposing the private key to callers. Resolved per tenant; there is no
// process-global signer.
type Signer interface {
    // PublicKey is the merchant address (also the fee payer / required
    // signer on every plan + pull transaction).
    PublicKey(ctx context.Context, tenantID tenant.ID) (solanago.PublicKey, error)

    // SignMessage signs the raw serialized Solana *message* bytes
    // (tx.Message.MarshalBinary()) with the tenant key and returns the 64-byte
    // Ed25519 signature.
    SignMessage(ctx context.Context, tenantID tenant.ID, message []byte) (solanago.Signature, error)
}
```

### Tx-builder orchestration (shared by both impls)

```go
// signAndSubmit builds the final wire tx: merchant is fee payer + sole signer.
func signAndSubmit(ctx context.Context, tid tenant.ID, s Signer, rpc *RPCClient,
    instrs []solanago.Instruction) (solanago.Signature, error) {

    payer, err := s.PublicKey(ctx, tid)
    if err != nil {
        return solanago.Signature{}, err
    }
    blockhash, err := rpc.RecentBlockhash(ctx)
    if err != nil {
        return solanago.Signature{}, err
    }
    tx, err := solanago.NewTransaction(instrs, blockhash, solanago.TransactionPayer(payer))
    if err != nil {
        return solanago.Signature{}, err
    }

    msg, err := tx.Message.MarshalBinary() // exact bytes Ed25519 signs
    if err != nil {
        return solanago.Signature{}, err
    }
    sig, err := s.SignMessage(ctx, tid, msg)
    if err != nil {
        return solanago.Signature{}, err
    }
    tx.Signatures = []solanago.Signature{sig} // single required signer = payer

    return rpc.SendTransaction(ctx, tx)
}
```

> **Ordering rule:** with one signer, `tx.Signatures[0]` must correspond to the
> fee payer (`payer`). If a plan/pull ever needs a co-signer, signatures must be
> ordered to match the message's required-signer account list.

---

## 2. `keypairSigner` — the everywhere path (`signer_keypair.go`)

Loads `solana/private_key` from the **existing** `merchants.MerchantSecretStore`
(whatever backend is wired — DB+envelope, or Vault KV), parses the Ed25519 key,
signs locally. Caches the parsed key per tenant with a short TTL so we don't hit
the store per signature.

```go
type keypairSigner struct {
    secrets merchants.MerchantSecretStore
    cache   *ttlCache[tenant.ID, solanago.PrivateKey] // 60s TTL (decided), configurable; invalidate on Secret.Version bump
}

func (k *keypairSigner) load(ctx context.Context, tid tenant.ID) (solanago.PrivateKey, error) {
    if pk, ok := k.cache.Get(tid); ok {
        return pk, nil
    }
    sec, err := k.secrets.Get(ctx, tid, "solana/private_key")
    if err != nil {
        return nil, err // ErrSecretNotFound (terminal) vs backend error (retry) — see §6
    }
    pk, err := solanago.PrivateKeyFromBase58(sec.Value) // or raw bytes per storage choice
    if err != nil {
        return nil, fmt.Errorf("parse tenant solana key: %w", err)
    }
    k.cache.Put(tid, pk)
    return pk, nil
}

func (k *keypairSigner) PublicKey(ctx context.Context, tid tenant.ID) (solanago.PublicKey, error) {
    pk, err := k.load(ctx, tid)
    if err != nil { return solanago.PublicKey{}, err }
    return pk.PublicKey(), nil
}

func (k *keypairSigner) SignMessage(ctx context.Context, tid tenant.ID, msg []byte) (solanago.Signature, error) {
    pk, err := k.load(ctx, tid)
    if err != nil { return solanago.Signature{}, err }
    return pk.Sign(msg) // in-process Ed25519
}
```

The key briefly lives in container memory. Acceptable for self-hosted/dev;
in managed prod, prefer `transitSigner`.

---

## 3. `transitSigner` — Vault Transit, key never leaves Vault (`signer_transit.go`)

The tenant key is a **non-extractable Ed25519 key inside Vault Transit**. We ask
Vault to sign; we only ever receive a signature. The public key (= Solana
address) is exported once and cached (it's stable for the key version).

```go
// TransitClient is the minimal Vault Transit surface (kept an interface so this
// builds/tests without a live Vault; satisfied by the §5 hashicorp/vault/api adapter).
type TransitClient interface {
    // Sign returns the raw 64-byte Ed25519 signature for input over key `name`.
    Sign(ctx context.Context, name string, input []byte) ([]byte, error)
    // PublicKey returns the latest 32-byte Ed25519 public key for `name`.
    PublicKey(ctx context.Context, name string) ([]byte, error)
}

type transitSigner struct {
    transit TransitClient
    keyName func(tenant.ID) string            // e.g. "openrails-tenant-<id>"
    pubs    *ttlCache[tenant.ID, solanago.PublicKey]
}

func (t *transitSigner) PublicKey(ctx context.Context, tid tenant.ID) (solanago.PublicKey, error) {
    if pk, ok := t.pubs.Get(tid); ok {
        return pk, nil
    }
    raw, err := t.transit.PublicKey(ctx, t.keyName(tid)) // 32 bytes
    if err != nil { return solanago.PublicKey{}, err }
    pk := solanago.PublicKeyFromBytes(raw)               // base58(raw) == Solana address
    t.pubs.Put(tid, pk)
    return pk, nil
}

func (t *transitSigner) SignMessage(ctx context.Context, tid tenant.ID, msg []byte) (solanago.Signature, error) {
    raw, err := t.transit.Sign(ctx, t.keyName(tid), msg) // 64-byte sig
    if err != nil { return solanago.Signature{}, err }
    return solanago.SignatureFromBytes(raw)
}
```

### Vault-side Transit details (the gotchas)

- **Key type:** `vault write transit/keys/openrails-tenant-<id> type=ed25519 exportable=false`. `exportable=false` is the whole point — the key can never be read out.
- **Solana curve == Ed25519.** A Solana address *is* the base58 of the 32-byte Ed25519 public key, so the Transit key's public key is directly the merchant address. **No separate address storage needed** — derive it from Vault.
- **Sign request:** `POST transit/sign/<name>` with `input = base64(message)`, **`prehashed=false`** (default). For Ed25519 Vault signs the raw message (PureEdDSA, hashes internally) and **ignores `hash_algorithm`**. Do **not** prehash — Solana verifies over the raw message.
- **Response parsing:** signature comes back as `"vault:v1:<base64>"`. Strip the `vault:v<n>:` prefix, base64-decode → 64 bytes. (The `TransitClient.Sign` adapter does this so callers get raw bytes.)
- **Key versioning:** rotating the Transit key version changes the public key → new Solana address → forces plan re-publish + re-enroll (plan PDA derives from the merchant address). So pin a key version per tenant and treat rotation as the heavy operation it is (runbook in #258).

---

## 4. Self-hosted DB-encryption recap (no Vault)

When no Vault is configured, `keypairSigner` reads `solana/private_key` from
`dbSecretStore` wrapped by `encryptedSecretStore` — the private key is
envelope-encrypted at rest (master key wraps per-tenant DEK; `internal/crypto`,
issue #227). Same `Signer` interface, same callers. This is the default and
needs no new infra.

---

## 5. Live `VaultKV` adapter (`internal/integrations/vault/`)

Implements the **existing** `tenancy.VaultKV` interface (`secrets_vault.go`) over
KV-v2, plus app-level auth + token renewal. This is the only missing piece to
turn on Vault KV; `vaultSecretStore`'s addressing
(`secret/openrails/merchants/<id>/<name>`) is already done.

### 5a. `kv.go` — KV-v2 adapter

```go
package vault

import (
    "context"
    "fmt"
    "strings"

    vaultapi "github.com/hashicorp/vault/api"
    "github.com/open-rails/openrails/internal/tenancy"
)

// KVv2Adapter satisfies tenancy.VaultKV. The tenancy store passes FULL logical
// paths that already include the mount (e.g. "secret/openrails/merchants/<id>/<name>");
// KV-v2's HTTP API needs a "/data/" (read/write) or "/metadata/" (list/delete)
// segment inserted after the mount, which the typed KVv2 helper handles for us.
type KVv2Adapter struct {
    client *vaultapi.Client
    mount  string // "secret"
}

func NewKVv2Adapter(client *vaultapi.Client, mount string) *KVv2Adapter {
    return &KVv2Adapter{client: client, mount: strings.Trim(mount, "/")}
}

// subpath strips the mount prefix the tenancy store prepended.
func (a *KVv2Adapter) subpath(full string) string {
    return strings.TrimPrefix(strings.TrimPrefix(full, a.mount), "/")
}

func (a *KVv2Adapter) ReadSecret(ctx context.Context, path string) (map[string]string, error) {
    s, err := a.client.KVv2(a.mount).Get(ctx, a.subpath(path))
    if err != nil {
        if isVaultNotFound(err) {
            return nil, nil // -> tenancy maps missing "value" to ErrSecretNotFound
        }
        return nil, fmt.Errorf("vault kv read: %w", err) // transport error -> retry upstream
    }
    out := make(map[string]string, len(s.Data))
    for k, v := range s.Data {
        if sv, ok := v.(string); ok { out[k] = sv }
    }
    return out, nil
}

func (a *KVv2Adapter) WriteSecret(ctx context.Context, path string, data map[string]string) error {
    m := make(map[string]interface{}, len(data))
    for k, v := range data { m[k] = v }
    _, err := a.client.KVv2(a.mount).Put(ctx, a.subpath(path), m)
    return err
}

func (a *KVv2Adapter) DeleteSecret(ctx context.Context, path string) error {
    return a.client.KVv2(a.mount).DeleteMetadata(ctx, a.subpath(path)) // purge all versions
}

func (a *KVv2Adapter) ListSecrets(ctx context.Context, path string) ([]string, error) {
    // KVv2 list is via the metadata endpoint; raw logical List against
    // "<mount>/metadata/<subpath>" returns the child keys.
    sec, err := a.client.Logical().ListWithContext(ctx, a.mount+"/metadata/"+a.subpath(path))
    if err != nil || sec == nil { return nil, err }
    keys, _ := sec.Data["keys"].([]interface{})
    names := make([]string, 0, len(keys))
    for _, k := range keys { if s, ok := k.(string); ok { names = append(names, s) } }
    return names, nil
}
```

### 5b. `auth.go` — app authenticates ONCE as itself, token auto-renewed

```go
// Login authenticates the OpenRails process to Vault (NOT per-tenant; tenant
// isolation is enforced by the (tenant, name) addressing). AppRole for VMs,
// Kubernetes auth for pods. Returns a client whose token is kept fresh.
func Login(ctx context.Context, cfg Config) (*vaultapi.Client, error) {
    client, err := vaultapi.NewClient(&vaultapi.Config{Address: cfg.Address})
    if err != nil { return nil, err }

    var secret *vaultapi.Secret
    switch cfg.AuthMethod {
    case "approle":
        secret, err = client.Logical().WriteWithContext(ctx, "auth/approle/login",
            map[string]interface{}{"role_id": cfg.RoleID, "secret_id": cfg.SecretID})
    case "kubernetes":
        jwt, _ := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
        secret, err = client.Logical().WriteWithContext(ctx, "auth/kubernetes/login",
            map[string]interface{}{"role": cfg.K8sRole, "jwt": string(jwt)})
    default:
        return nil, fmt.Errorf("unsupported vault auth method %q", cfg.AuthMethod)
    }
    if err != nil { return nil, err }
    client.SetToken(secret.Auth.ClientToken)

    // Background renewal; on terminal failure, re-Login (caller supervises).
    go renew(ctx, client, secret)
    return client, nil
}

func renew(ctx context.Context, client *vaultapi.Client, secret *vaultapi.Secret) {
    w, err := client.NewLifetimeWatcher(&vaultapi.LifetimeWatcherInput{Secret: secret})
    if err != nil { return }
    go w.Start(); defer w.Stop()
    for {
        select {
        case <-ctx.Done():            return
        case err := <-w.DoneCh():     log.WithError(err).Warn("vault token renewal stopped; re-login needed"); return
        case <-w.RenewCh():           // renewed; loop
        }
    }
}
```

### 5c. Transit adapter (same package, same `*vaultapi.Client`)

```go
// TransitAdapter satisfies solana.TransitClient over Vault's transit engine.
type TransitAdapter struct{ client *vaultapi.Client; mount string } // mount: "transit"

func (t *TransitAdapter) Sign(ctx context.Context, name string, input []byte) ([]byte, error) {
    res, err := t.client.Logical().WriteWithContext(ctx, t.mount+"/sign/"+name,
        map[string]interface{}{"input": base64.StdEncoding.EncodeToString(input)})
    if err != nil { return nil, err }
    raw, _ := res.Data["signature"].(string)            // "vault:v1:<b64>"
    b64 := raw[strings.LastIndex(raw, ":")+1:]
    return base64.StdEncoding.DecodeString(b64)          // 64 bytes
}

func (t *TransitAdapter) PublicKey(ctx context.Context, name string) ([]byte, error) {
    res, err := t.client.Logical().ReadWithContext(ctx, t.mount+"/keys/"+name)
    if err != nil { return nil, err }
    keys, _ := res.Data["keys"].(map[string]interface{})
    latest := res.Data["latest_version"]                 // pick the pinned/latest version
    entry, _ := keys[fmt.Sprint(latest)].(map[string]interface{})
    b64, _ := entry["public_key"].(string)
    return base64.StdEncoding.DecodeString(b64)          // 32 bytes == Solana address
}
```

---

## 6. Wiring (`internal/http/server.go`) + failure semantics

```go
// Today (#225/#227): DB store, optionally envelope-encrypted.
secretStore, _ := tenancy.NewDBSecretStore(pool)
secretStore, _  = tenancy.NewEncryptedSecretStore(secretStore, enc)

// Managed (NEW): swap in Vault KV with the SAME addressing — no caller change.
if cfg.Vault != nil && cfg.Vault.Enabled {
    vc, _ := vault.Login(ctx, cfg.Vault)
    secretStore = tenancy.NewVaultSecretStore("secret", vault.NewKVv2Adapter(vc, "secret"))

    // Solana signing uses Vault Transit; the key never leaves Vault.
    solanaSigner = solana.NewTransitSigner(&vault.TransitAdapter{client: vc, mount: "transit"})
} else {
    solanaSigner = solana.NewKeypairSigner(secretStore)      // self-hosted: DB+envelope
}
```

**Fail-closed, distinguish modes** (critical for the pull worker, #256/#257):

| Condition | Classify as | Action |
|---|---|---|
| Vault unreachable / token expired / 5xx | **operational** | retry; **never** cancel a sub or skip verification |
| `ErrSecretNotFound` / Transit key missing | **terminal for that tenant** | mark tenant misconfigured; alert; don't pull |
| Signature OK, tx fails (insufficient USDC) | subscriber dunning | `FailMembership` (§7.5) |
| Signature OK, tx fails (insufficient SOL gas) | **operational** | retry; top-up fee wallet (#258) |

---

## 7. What ships where (maps to issues)

| Piece | File(s) | Issue |
|---|---|---|
| `Signer` interface + tx-builder | `internal/integrations/solana/signer.go` | #253 |
| `keypairSigner` (everywhere) | `internal/integrations/solana/signer_keypair.go` | #253 |
| `transitSigner` (prod) | `internal/integrations/solana/signer_transit.go` | #253 (Transit track) |
| `VaultKV` KV-v2 adapter | `internal/integrations/vault/kv.go` | #251 |
| Vault auth + renewal | `internal/integrations/vault/auth.go` | #251 |
| Transit adapter | `internal/integrations/vault/transit.go` | #251 / #253 |
| Store/signer selection wiring | `internal/http/server.go` (+ config) | #251 / #253 |

**Dependency to add:** `github.com/hashicorp/vault/api` (managed builds only;
keep it behind the `vault` package so self-hosted builds don't require Vault to
run — mirrors how `vaultSecretStore` already builds with a nil client).
