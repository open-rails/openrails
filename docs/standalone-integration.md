# Standalone Integration Guide

How to deploy OpenRails as its own self-hosted HTTP service and integrate your
application against it; `examples/standalone` is the runnable quickstart. Money
is an integer in the currency's native units (`GET /v1/currencies`; micros for
USD), a decimal string on the wire ([money-wire.md](money-wire.md)). Vocabulary:
a **rail** is a gateway kind (`nmi`, `ccbill`,
`stripe`, `solana`); a **PSP** is your concrete account on a rail (e.g. `mobius`
on nmi) — declared under `merchants.<slug>.psps.<key>.<rail>`.

```mermaid
flowchart LR
    B[Browser] -- session credential --> H[Your backend]
    H -. mints delegated token .-> B
    B -- delegated token, /v1/me/* --> OR[OpenRails :3053]
    H -- API key, /v1/merchant/* --> OR
    OR --> PG[(Postgres 18+)]
    OR --> RD[(Garnet/Redis)]
    R[Stripe / NMI / CCBill / Solana] -- webhooks --> OR
```

### Deployment

**Evaluation.** The compose stack runs everything zero-config:

```bash
task docker-up                                # Postgres + Garnet(Redis) + migrate + OpenRails
curl http://localhost:3053/health/ready       # readiness: dependencies, local workers, AuthKit verifier
```

Host ports (all bound to 127.0.0.1): OpenRails `:3053`, Postgres `:5434`,
Redis `:6380`. The `openrails-migrate` service applies migrations
(`openrails migrate up`) before the server starts. Everything — public catalog,
`/v1/me/*` self-service, `/v1/merchant/*`, and webhooks — shares the one port;
there is no separate private/service listener.

**Production needs:**

- **Postgres 18+.** OpenRails owns the `openrails` schema; it can share your
  app's database. Apply migrations with `openrails migrate up` before each new
  version boots (the server validates and refuses to start on missing migrations).
- **A Redis-compatible service** (we recommend Garnet) — optional, backs
  rate limiting. If omitted or unreachable, limits are in-memory per-process
  and readiness remains green.
- **HashiCorp Vault** — optional. Two independent uses: KV storage for merchant
  secrets (`secret_backend: vault`) and Transit signing for Solana custody. See
  [vault.md](vault.md).

**Configuration** is a `config.yaml` (see `config.example.yaml` for the full
surface: listener, db, redis, auth issuer, encryption, vault, rate limits,
trusted proxies, captcha, admin console) plus a koanf env overlay — an env var
maps onto the config tree by prefix, e.g. `DB_URL` → `db.url`,
`PROVIDER_WRITE_MODE` → `provider_write_mode`, `SECRET_BACKEND` →
`secret_backend`. For the two operating dials there are also CLI flags.
Precedence: **flag beats env beats yaml.**

Security defaults apply in sandbox and live deployments. Narrow local auth
exceptions must be explicit; provider posture does not relax storage, issuer,
signing or trusted-proxy requirements.

```bash
openrails run-server --config /etc/openrails/config.yaml \
  --provider-write-mode full --test-mode live
```

**Required configuration** (config validation refuses boot otherwise):

- `provider_write_mode: full | limited | readonly` — how much OpenRails may do
  against the rails (see [operations.md](operations.md) for the full matrix;
  `limited` parks system-initiated writes like dunning, `readonly` blocks all
  provider writes at the wire). Declare it explicitly; unset internal policy fails closed to `readonly`.
- `test_mode: sandbox | live` — the credential axis, orthogonal to the above.
  `sandbox` routes every rail to its test environment and refuses live
  credentials at boot (live Stripe keys rejected, NMI accounts probed), so no
  real money can move. `live` similarly refuses Stripe test keys in every
  environment rather than silently disabling the rail. Sandbox is allowed in
  every environment — credential validation, not the env string, keeps it
  honest.
- Non-default database credentials, and an `https` `auth.issuer`.
- **Credential custody**: explicitly select `secret_backend: snapshot`, `vault`,
  or `db`. Managed DB storage requires `ENCRYPTION_MASTER_KEY` (base64, 32-byte
  AES-256) even in sandbox. Snapshot credentials stay in process memory.
- Behind a load balancer, set `trusted_proxies` to its CIDR range or
  `X-Forwarded-For` is ignored and rate limiting keys on the LB's address.

### Credential custody and configuration publication

Merchant metadata lives in PostgreSQL. `secret_backend` selects snapshot, Vault
or encrypted DB credential custody. `merchant_config_http` independently selects
external configuration routes. Authorized local Client operations remain available
with HTTP off; credential mutation additionally requires a writable backend.

Startup initializes missing identities and metadata and reloads snapshot values.
It preserves subsequent API edits and archived providers. Explicit metadata
applications carry a stable ID and revision precondition; managed credentials use
separate publication operations. See [metadata applications](merchant-configuration-applications.md).

Catalogs always use database state. `allow_catalog_updates` independently controls
ordinary catalog Client/API mutations and defaults to false in both credential
modes. Disabled mutations are absent from the route bundle; reads remain available.
Trusted operator application is still permitted and uses durable application IDs
so an unchanged artifact does not overwrite later edits. This does not change
provider permissions, sandbox/live posture, or `provider_write_mode`.

Snapshot walkthrough (file layout, YAML secret overlays via
`merchant_manifest_overlays`, rotation): [self-hosting-mode1.md](self-hosting-mode1.md).

### First run

Three file-backed manifests drive provisioning (example shapes:
`config/bootstrap.example.yaml`, `config/merchants_config.example.yaml`,
`config/catalog.example.yaml`). Every push command shares one **mutation-flag
contract**: with no flags it is **plan-only** (prints a terraform-style diff,
mutates nothing); `--insert` creates missing state, `--overwrite` updates
existing state, `--prune` removes target extras absent from the manifest. The
flags compose; full reconciliation is `--insert --overwrite --prune`.

On an empty install, in order:

```bash
# 1. AuthKit root authority: initial operator user(s) + trusted remote apps.
#    First-run only when applied at startup; explicit here.
openrails push-auth-bootstrap --config /etc/openrails/config.yaml --file /etc/openrails/bootstrap.yaml

# 2. Merchants: identity, profile, PSPs (rail accounts + secrets), and your
#    app's issuer registered as merchant OWNER (the manifest's
#    remote_application block).
openrails push-merchant-config --config /etc/openrails/config.yaml --file /etc/openrails/merchants.yaml --insert

# 3. Catalog: products, entitlements, prices and validated provider bindings.
#    Provider creation and mutation use their separate workflows.
openrails apply-catalog --merchant your-merchant --config /etc/openrails/config.yaml --file /etc/openrails/catalog.yaml
```

Startup loads a configured merchant snapshot into process memory and initializes
missing metadata. Existing API edits and archived accounts survive restarts.
AuthKit bootstrap is first-run only; catalog application is always explicit.
Use metadata applications for deliberate versioned configuration changes.

**Create an API key.** Backend credentials are merchant-scoped API keys
(`openrails_st_…`) minted through the merchant surface:

```
POST /v1/merchant/api-keys   {"name": "backend", "role": "owner"}
```

Roles are fixed: `viewer` (read-only — right for LLM agents), `support`,
`owner`. Requires `merchant:credentials:manage` (owner-only), so authenticate
the mint with a delegated JWT signed by the issuer you registered in step 2
(issuer-as-owner: your app's tokens administer exactly that one merchant), an
operator session from the bootstrap user, or the admin console
(`admin_console.enabled`). The secret is returned **exactly once** in the mint
response and is never retrievable again. `GET /v1/merchant/api-keys` lists,
`DELETE /v1/merchant/api-keys/{id}` revokes. Details:
[merchant-provisioning.md](merchant-provisioning.md).

### Backend integration

**Go SDK** — the root module's `openrails.Client`, identical interface to
embedded mode:

```go
client, err := openrails.NewRemote("https://openrails.example",
    openrails.WithAPIKey(os.Getenv("OPENRAILS_API_KEY")), // or WithTokenProvider for minted JWTs
    openrails.WithMerchantID(merchantID),                 // immutable merchant binding
    openrails.WithCurrency("USD"),
    openrails.WithTimeout(2*time.Second), // per-call deadline; default 2s
)
if err != nil { log.Fatal(err) }         // static config: bad URL, no credential
if err := client.Verify(ctx); err != nil { // authenticated boot probe
    log.Fatal(err)                         // unreachable, bad key — fail fast
}

verdicts, err := client.AdmitBatch(ctx, []openrails.AdmitRequest{{
    CustomerID:      openrails.CustomerID(customerID), // the host's subject UUID
    Invoker:         userID,
    EstimatedAmount: 50_000,    // native units (USD: micros)
    ExpiresAt:       &deadline, // required with a hold: the job's deadline
    RequestID:       requestID, // idempotency key
}})
receipt, err := client.Capture(ctx, requestID, 43_000, &openrails.CaptureUsage{EventType: "chat.completion"})
// or client.Release(ctx, requestID) if the work failed
```

Options: `WithAPIKey`, `WithTokenProvider` (per-call minted bearer),
`WithMerchantID` ([client-merchant-binding.md](client-merchant-binding.md)),
`WithCurrency`, `WithTimeout`, `WithHTTPClient`. The constructor validates
static configuration without I/O; `Verify` is the live check.

**Errors** are canonical sentinels (`errors.Is` works identically against a
remote or embedded engine): `ErrUnauthorized`, `ErrInvalid`, `ErrDenied`,
`ErrNotFound`, `ErrConflict`, `ErrInsufficientCredits` (402),
`ErrPaymentRefused` (402 `card_declined` / `payment_method_stale`),
`ErrInternal`, and `ErrUnreachable` — which wraps transport failures,
timeouts, and 5xx. Every server error is a `*StatusError` carrying the HTTP
status and wire code/message ([api/errors.md](api/errors.md)). Identifiers are typed (`openrails.CustomerID`,
`ProductID`, `PriceID`, `SubscriptionID`, `PaymentID`, `PaymentMethodID`,
`CheckoutSessionID`): a zero id, or a blank, whitespace or dot key, is refused
by the Client before any request with the same `400 invalid_param`
`StatusError` the server returns for a malformed identifier, so embedded and
remote callers observe one error.

**Fail-open vs fail-closed for admission:** key your policy off
`ErrUnreachable`. A clean deny (`allowed=false`, or `ErrInsufficientCredits`)
is a real verdict — always honor it. `ErrUnreachable` means OpenRails could
not answer: fail-open (serve the request, reconcile later) keeps your product
up when billing is down, fail-closed protects against unmetered spend — choose
per endpoint cost. Keep `WithTimeout` short so a slow OpenRails cannot stall
your hot path.

**Any other stack** calls the same HTTP surface with the API key:

```bash
# Pre-authorize + hold atomically before doing expensive work
curl -X POST https://openrails.example/v1/merchant/admissions \
  -H "Authorization: Bearer openrails_st_..." \
  -d '{"items":[{"customer_id":"...","invoker":"user-123","estimated_amount":"50000",
       "expires_at":"2026-09-16T12:00:00Z","request_id":"req-789"}]}'

# Settle at real cost…
curl -X POST https://openrails.example/v1/merchant/admissions/req-789/capture \
  -H "Authorization: Bearer openrails_st_..." \
  -d '{"amount":"43000","event_type":"chat.completion"}'

# …or release the hold when the work failed
curl -X POST https://openrails.example/v1/merchant/admissions/req-789/release \
  -H "Authorization: Bearer openrails_st_..."
```

The `/v1/merchant/*` surface (admissions, credits, entitlements, usage,
settings, customers, payments, subscriptions) is permission-gated per route —
see [api/endpoints.md](api/endpoints.md) for the full reference and the
permission table. Keys are bound to their merchant and can never act on
another merchant's data.

### Frontend integration

Your users' browsers call OpenRails' self-service surface (`/v1/me/*`:
status, subscriptions, payment methods, checkout, invoices) **directly**, using
a short-lived delegated token your backend mints with its registered issuer
key — your session tokens never leave your trust domain. The token contract,
exchange-endpoint pattern, and checkout flows are in
[frontend-integration.md](frontend-integration.md); the rationale for the
two-token model is in [auth.md](auth.md). CORS requires zero configuration
(see below).

### Webhooks

Point each rail's webhook directly at OpenRails — not through your app:

```
POST https://openrails.example/v1/webhooks/{rail}[/{account_id}]
```

OpenRails resolves the merchant from the payload's (or the path's) PSP account
identity, verifies the rail's signature
with that merchant's own signing secret, and updates
subscriptions/entitlements; your app just reads the results. For local rail
sandboxes see [dev/local-webhooks.md](dev/local-webhooks.md).

**Per-merchant API hosts (#734).** A multi-merchant deployment can give each
merchant a canonical hostname (`PUT /v1/merchant/api-host`, or
`cp.SetMerchantAPIHost` on an attached control plane; resolved live on the next
request, no restart). Host resolution then routes `/v1/webhooks/{rail}`
without the path slug, and enforces Host-merchant == issuer-merchant on every
merchant-scoped route: a token minted for merchant A is rejected on merchant
B's host even though it verifies.

**CORS (#765)** is a fixed, engine-wide policy — not configurable, no origin
registration: browser-facing tiers (checkout, `/v1/me/*`, `/v1/customers/*`)
answer `Access-Control-Allow-Origin: *` (never with credentials — OpenRails
issues no cookies; every browser call is an explicit bearer token), and every
other surface (merchant API, webhooks, admin) emits no CORS headers at all.

### Upgrades and ops

On every upgrade run `openrails migrate up` before the new version serves
traffic — the server refuses to boot on a missing migration. For everything
operational — the `provider_write_mode` matrix, cutover onto production
credentials (boot `limited`, inspect `openrails intents`, then raise to
`full`), the durable provider-intent ledger, `pull-provider` reconciliation,
and dunning — see [operations.md](operations.md).
