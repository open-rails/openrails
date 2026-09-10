# Testing

## Doctrine

Prefer end-to-end integration tests over mocks: test the real codepaths
(real Postgres, real Redis, real HTTP surface, real provider sandboxes where
possible). Mocks make tests easy to write and miss real bugs. Unit tests are
fine for pure logic — e.g. every provider money boundary has a wire-pinning
unit test (known micros ⇒ exact wire amount) — but behavior is proven by
integration tests.

## Integration tests

The ordinary Docker-backed integration suite carries the build tag
`integration`; live/on-chain exceptions use the `devnet` and
`stripe_integration` tags documented below. Backing services resolve in order:

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

PR CI narrows ordinary integration coverage to the packages touched by the
diff and always includes `internal/integrationharness`. Narrow diffs use one
serial shard; changes to shared surfaces run the complete tagged package set
in two balanced shards, each with a six-minute test timeout and its own service
stack. `.github/workflows/ci-full.yaml` is the unsharded backstop: every Monday
at 05:00 UTC and on manual dispatch it runs the complete tagged suite serially
against one stack.

Query-layer checks: `task test-query-contracts` and `task test-query-perf`
run `internal/db/querytest` against a migrated Postgres
(`QUERY_TEST_DATABASE_URL` overrides `OPENRAILS_TEST_DB_URL`).

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

Pass `WithSuiteClock` to `setupTestSuite` and advance the returned clock instead
of sleeping. A fresh suite receives the clock before runtime construction; the
ordinary shared suite swaps its runtime-wide `SettableClock` delegate for the
test and restores it with `t.Cleanup`:

```go
clock := clockwork.NewFakeClockAt(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC))
suite := setupTestSuite(t, WithSuiteClock(clock))
// ... create data at fake time ...
clock.Advance(30 * 24 * time.Hour)
```

Prefer `WithSuiteClock`; `SetMockClock` is the compatibility helper for tests
that must swap the shared runtime clock after setup.

**Guardrail:** `bash scripts/check_business_time.sh` (first step of
`task test`) scans business/domain paths (`internal/modules`, `internal/river`,
`internal/http/handlers`, `internal/reconcile`, `internal/intents`,
`pkg/service`) for direct
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
  curl). Hits a running standalone instance; needs `OPENRAILS_API_KEY`.
  `BASE_URL` defaults to `http://127.0.0.1:3053`. Fresh credit type per run
  keeps balances deterministic. Runs fine from an `alpine/curl` container on
  the stack's network.

### NMI live sandbox lifecycle

`task e2e-nmi-live` runs two Go integration tests against a **real NMI sandbox
account**. `TestNMILiveLifecycleE2E` registers a live NMI provider, ensures a
sandbox recurring plan, vaults a sandbox test card server-side (the Customer
Vault equivalent of browser Collect.js tokenization — OpenRails never accepts
a raw PAN), runs one-off + subscription checkouts, verifies remote state via
NMI's Query API, and cancels. `TestLiveSandboxStoredCredentialCITThenMIT`
executes an initial customer-initiated stored-card transaction and a subsequent
merchant-initiated transaction tied to the initial NMI transaction ID. Requires
`NMI_SANDBOX_SECURITY_KEY` (loaded from `.env`; the tests skip without it).
Sandbox test cards move no real money; charge amounts are randomized per run
to dodge NMI duplicate-transaction checks.

The separate `.github/workflows/live-gated-integration.yaml` runs weekly and
on demand. Its enforcing lanes run five NMI sandbox proofs, two live invoice
collection proofs (Stripe and NMI), and the Stripe Model-B upgrade proof under
`stripe_integration`. Each lane fails when a credential required by its named
proofs is absent; these provider-sandbox jobs are not required merge checks.

Supporting targets: `task docker-up-e2e-sandbox` (stack + AuthKit issuer),
`task mint-jwt` (needs `AUTHKIT_DEV_MINT_SECRET`; prints `E2E_RUN_ID` /
`E2E_USER_ID` / a JWT), `task e2e-dump-local` (dump local rows for the current
run), and either `task nmi-query TXN_ID=…` or `task nmi-query SUB_ID=…` (NMI
Query API, needs `NMI_QUERY_SECURITY_KEY`). For real inbound webhooks, see
[local-webhooks.md](local-webhooks.md).

### Solana recurring (devnet)

On-chain mechanics tests carry the `devnet` build tag and run against Solana
devnet with a funded payer:

```bash
SOLANA_DEVNET_PAYER_KEY=<funded> \
SOLANA_DEVNET_SUBSCRIBER_KEY=<usdc-funded> \
HELIUS_API_KEY=<key> \
  go test -tags devnet -run 'TestDevnetLifecycle/FastPlan' -v -timeout 480s \
  ./internal/modules/solana/recurring/...
```

The devnet suite proves atomic subscribe plus the first pull, the same-period
cap, cancellation blocking future-period pulls, sustained rebilling,
insufficient-funds and revoked-delegate classifications, independent multiple
subscriptions, and tier changes. `.github/workflows/solana-devnet-integration.yaml`
runs the bounded FastPlan proof daily. Manual dispatch can additionally run the
multi-hour rebill, the four extended lifecycle/multi-subscription/tier-change/
failure proofs, and the separately keyed integration-harness money-movement
proof.

The full-stack flow (checkout `payment.rail: "solana"` →
`next_action: solana_sign_transactions` → wallet signs → confirm → first
crank → membership) needs a locally built image in a host-app compose stack,
devnet **USDC** (the service layer enforces the USDC/USD1 mint allowlist — a
self-minted token won't resolve), and a wallet that can sign (browser wallet,
or sign the returned base64 txns with a test keypair for an API-level run).
A fully automated browser run is blocked on a mock-wallet adapter; the wallet
approval click is manual by nature.
