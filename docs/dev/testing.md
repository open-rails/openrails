# Testing

## Doctrine

Prefer end-to-end integration tests over mocks: test the real codepaths
(real Postgres, real Redis, real HTTP surface, real provider sandboxes where
possible). Mocks make tests easy to write and miss real bugs. Unit tests are
fine for pure logic — e.g. every provider money boundary has a wire-pinning
unit test (known micros ⇒ exact wire amount) — but behavior is proven by
integration tests.

## Integration tests

All integration tests carry the build tag `integration`. Backing services
resolve in order:

1. `OPENRAILS_TEST_DB_URL` (or `OPENRAILS_TEST_DB_DSN`) — an admin DSN; the
   harness creates an isolated per-run database on that server.
2. Otherwise testcontainers spins up throwaway Postgres + Redis containers.

Redis: `OPENRAILS_TEST_REDIS_ADDR` (host:port), else a testcontainer.

### RLS is enforced by default

`dbtest.SharedPostgresDSN(t)` — the default handle — connects as **`openrails_app`**
(NOBYPASSRLS), the same role production connects as, and asserts the role is
neither `rolsuper` nor `rolbypassrls` before handing it out. A query that forgets
to open a merchant connection returns zero rows here exactly as it would in
production, instead of silently succeeding on a superuser connection.

Pick the handle by what the test is proving:

| helper | role | use for |
|---|---|---|
| `SharedPostgresDSN` / `SharedPGXPool` | app, no merchant | code that must pin its own merchant (HTTP routes, River workers) |
| `SharedMerchantPool` / `OpenMerchantDB` / `MerchantPinnedDSN` | app, merchant pinned | fixtures for one merchant, and module services called below the layer that pins |
| `SharedSuperuserDSN` / `SharedSuperuserPGXPool` | superuser | fixtures spanning merchants, assertions on another merchant's rows, migrations |

Never reach for the superuser helper to make a failure go away — under the default
handle, a failure is usually the harness reporting a production bug (see or#867/#868).

Ways to run:

```bash
task test                    # guardrail + unit tests (-race) + core integration tier
task test-integration-core   # ./tests ./embed ./internal/river ./pkg/service
task test-integration-all    # every integration-tagged package, serially
bash scripts/test_integration.sh ./tests -run TestFoo   # targeted
```

`scripts/test_integration.sh` starts the compose `postgres` + `garnet`
services (host ports `POSTGRES_HOST_PORT`=5434, `GARNET_HOST_PORT`=6380),
exports the matching `OPENRAILS_TEST_DB_DSN` / `OPENRAILS_TEST_REDIS_ADDR`,
expands `./...` to only the integration-tagged packages, and runs
`go test -p 1 -parallel 1 -tags=integration` (timeout
`OPENRAILS_INTEGRATION_TIMEOUT`, default 25m). The suite is self-cleaning
(per-run DBs are dropped; a reaper removes orphans).

Query-layer checks: `task test-query-contracts` and `task test-query-perf`
run `internal/db/querytest` against a migrated Postgres
(`QUERY_TEST_DATABASE_URL` overrides `OPENRAILS_TEST_DB_URL`).

### Known fragility

Running a SINGLE integration package in isolation can hit a pre-existing
`*_merchant_fk` fixture-seeding failure (the merchant isn't seeded for that
subset). Not a regression — the full suite seeds it correctly.

## Business time and test clocks

OpenRails has two kinds of time:

- **Business time** — billing state: subscription periods, entitlement
  validity, cancellation timestamps, renewal windows, dunning retries,
  checkout session expiry, credit/hold expiry.
- **Infrastructure time** — process mechanics: cache TTLs, rate limits,
  webhook signature tolerance, retry backoff, metrics.

Business-time code must use the runtime `clockwork.Clock` (production boots
with `clockwork.NewRealClock()` at the composition boundary). Infrastructure
code may use wall-clock time when wall-clock behavior is the thing itself.

Tests inject a fake clock before runtime construction and advance it instead
of sleeping:

```go
clock := clockwork.NewFakeClockAt(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC))
suite := setupTestSuite(t, WithSuiteClock(clock))
// ... create data at fake time ...
clock.Advance(30 * 24 * time.Hour)
```

Prefer `WithSuiteClock`; `SetMockClock` is a compatibility helper for older
tests that patch the shared runtime after construction.

Rail-side test clocks (e.g. Stripe Test Clocks) are separate from app time:
the rail clock produces realistic external events, the OpenRails fake clock
controls how the app interprets them. Advance both deliberately.

**Guardrail:** `bash scripts/check_business_time.sh` (first step of
`task test`) scans business/domain paths (`internal/modules`, `internal/river`,
`internal/db/repo`, `internal/http/handlers`, `pkg/service`) for direct
`time.Now()`, SQL `NOW()`/`CURRENT_TIMESTAMP`, and
`clockwork.NewRealClock()`. Existing allowed usages are classified in
`scripts/business-time-allowlist.txt` (`file|fragment|classification|reason`
lines). New business-time logic should inject the runtime clock, not add
allowlist entries.

## E2E harnesses

### Unified billing (credit money path)

Proves estimate → authorize+hold → capture/release on the standalone
API-key-authenticated `/v1/merchant/*` surface — the server-to-server
contract host orchestrators use. Two editions:

- **In-repo Go harness** — `tests/unified_billing_e2e_test.go`
  (`go test -tags integration -run TestUnifiedBilling ./tests/`). Covers full
  and partial capture, insufficient-balance 402, failure release, idempotent
  replay, and owner scoping, asserting ledger rows directly.
- **Deployed-stack harness** — `scripts/unified_billing_e2e.sh` (POSIX sh +
  curl). Hits a running standalone instance; needs `OPENRAILS_API_KEY` and
  `BASE_URL`. Fresh credit type per run keeps balances deterministic. Runs
  fine from an `alpine/curl` container on the stack's network.

### NMI live sandbox lifecycle

`task e2e-nmi-live` — a Go integration test
(`tests/nmi_live_lifecycle_e2e_test.go`) against a **real NMI sandbox
account**: registers a live NMI provider, ensures a sandbox recurring plan,
vaults a sandbox test card server-side (the Customer Vault equivalent of
browser Collect.js tokenization — OpenRails never accepts a raw PAN), runs
one-off + subscription checkouts, verifies remote state via NMI's Query API,
verifies signed webhook ingestion + idempotent replay, and cancels. Requires
`NMI_SANDBOX_SECURITY_KEY` (loaded from `.env`; the test skips without it).
Sandbox test cards move no real money; charge amounts are randomized per run
to dodge NMI duplicate-transaction checks.

Supporting targets: `task docker-up-e2e-sandbox` (stack + AuthKit issuer),
`task mint-jwt` (needs `AUTHKIT_DEV_MINT_SECRET`; prints `E2E_RUN_ID` /
`E2E_USER_ID` / a JWT), `task e2e-dump-local` (dump local rows for the current
run), `task nmi-query TXN_ID=… | SUB_ID=…` (NMI Query API, needs
`NMI_QUERY_SECURITY_KEY`). For real inbound webhooks, see
[local-webhooks.md](local-webhooks.md).

### Solana recurring (devnet)

On-chain mechanics tests carry the `devnet` build tag and run against Solana
devnet with a funded payer:

```bash
SOLANA_DEVNET_PAYER_KEY=<funded> SOLANA_DEVNET_SUBSCRIBER_KEY=<funded> HELIUS_API_KEY=<key> \
  go test -tags devnet -v -timeout 580s ./internal/integrations/solana/...
```

Proven there: full plan/subscribe/transfer/cancel lifecycle; partial pulls
capped at the plan amount per period; cancel vs revoke semantics
(`cancel_subscription` does NOT stop pulls — the subscriber's SPL `Revoke` is
the real stop, surfacing as token OwnerMismatch on the next crank); and
submit-and-confirm before a pull counts as success. CI runs the real-USDC
service-layer test daily and the multi-hour rebill test on manual dispatch
(`.github/workflows/solana-devnet-integration.yml`).

The full-stack flow (checkout `payment.rail: "solana"` →
`next_action: solana_sign_transactions` → wallet signs → confirm → first
crank → membership) needs a locally built image in a host-app compose stack,
devnet **USDC** (the service layer enforces the USDC/USD1 mint allowlist — a
self-minted token won't resolve), and a wallet that can sign (browser wallet,
or sign the returned base64 txns with a test keypair for an API-level run).
A fully automated browser run is blocked on a mock-wallet adapter; the wallet
approval click is manual by nature.
