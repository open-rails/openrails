# OpenRails focused test and release plan

Status: design only. The existing test corpus remains required until this plan
has earned an equivalent coverage receipt. This document does not authorize
deleting or weakening any current test.

## Why a new shape is needed

The current tree has 1,090 Go test files and about 178,935 test lines. 614 files
carry the `integration` build tag and are spread across 67 packages. The release
workflow manifest contains 59 retained journeys, while the required E2E job
runs the integration packages as a broad package graph. This gives strong
coverage, but it also makes ownership, isolation, and runtime hard to see. A
single serial `internal/integrationharness` package has held CI for the full
25-minute test timeout when one provider fixture failed.

The replacement should make each financial and authority invariant visible in a
small deterministic scenario. It must preserve the existing behavior while the
old suite is still the oracle.

## Test layers

### Static and pure unit checks (target: 2 minutes)

These tests do not open a database, start a provider, or use the network. They
cover parsers, canonical identifiers, money-unit conversion, currency and
interval validation, entitlement/resource keys, permission predicates, request
fingerprints, idempotency-key derivation, webhook event classification, and
configuration posture validation. Fuzz tests are bounded and deterministic.

This layer also runs `gofmt`, `go vet`, SQL/migration linters, generated-code
drift checks, neutral-auth dependency fences, secret/PAN scanners, and frontend
typecheck/lint/build checks.

### Database contract and invariant tests (target: 5 minutes)

Each process receives a unique database on one PostgreSQL service. A process
owns its schema/database name and cleanup; no test shares mutable rows with
another process. The layer runs migrations through the public library entry
points, verifies schema ownership and grants, and exercises production database
roles rather than a superuser for application operations.

Shard by package or scenario group, with `-race`, `-count=1`, and package-local
parallelism of one. Keep migration-lock contention tests in a dedicated serial
shard. The suite covers merchant/customer isolation, authority predicates,
catalog constraints, resource-grant ledger invariants, RLS or explicit
merchant predicates where retained, River job persistence, account deletion
callbacks, and archive/purge deadlines.

### Provider and durable-operation tests (target: 6 minutes)

One deterministic fake provider harness implements Stripe, NMI/Mobius, and
CCBill protocol boundaries. It records every request, can delay or drop a
response after committing provider state, and can deliver duplicate or
out-of-order notifications. Each test receives a fresh merchant/account and
fake journal. No test reaches a real PSP.

Run four independent shards. Every mutation asserts the request count and
idempotency key. Unknown outcomes remain unknown until an exact provider read
proves the result; retries must never create a second charge/refund/subscription.

### Embedded/remote parity tests (target: 3 minutes)

The same table-driven scenario is run through the embedded Client and the HTTP
Client. The service/runtime is constructed once per scenario, while the Client
surface and response/error contracts stay identical. The table compares status,
typed error, durable ledger state, entitlements, and provider request journal.

The minimum table includes catalog/product/price selectors, one-off checkout,
engine-owned recurring enrollment, cancellation, refund, webhook convergence,
River restart, and migration replay.

### Browser and frontend contracts (target: 8 minutes)

A single Chromium worker runs the public flows against a disposable demo host:
registration/login/recovery, channel owner/editor sharing, free and paid post
visibility, one-off purchase, membership purchase, cancellation, permanent
purchase access after cancellation, `/me` billing history, Stripe test-mode
tokenization, and NMI tokenized-card checkout. The browser uses deterministic
test credentials and fake or sandbox-approved provider endpoints; no card data
is committed to fixtures.

The route inventory and generated API clients are checked from the running
server, rather than maintained as a second hard-coded list.

Live Stripe/NMI/CCBill proofs remain a separate explicitly gated workflow. They
never become a required PR check and never share credentials with disposable
tests.

## Critical-invariant replacement matrix

| Invariant | Focused replacement scenario | Fixture/evidence |
| --- | --- | --- |
| Merchant/customer isolation | Two merchants and two customers attempt every read/write surface | Unique merchant IDs, application DB role, SQL row-count assertions |
| Neutral auth and authority | Embedded and HTTP callers use user, service, delegated, and API-key principals | Minimal verifier fake plus AuthKit adapter contract; denied and unavailable are distinct |
| Catalog identity | Product/price IDs and exact keys select the same record without UUID guessing | Collision keys, archive/reprice history, catalog-scoped selectors |
| Resource entitlements | Products grant opaque post/channel resources; grants are append-only snapshots | Exact resource checks, partial bundle ownership, archive retains prior access |
| One-off admission | Duplicate keys, concurrent callers, existing ownership, and member-only policy | Customer row lock, provider journal, one payable session |
| Recurring admission | Saved payer proof, membership entitlement, cancellation and renewal | Engine-owned schedule fixture and frozen accepted terms |
| Frozen terms | Archive/reprice after acceptance cannot change amount, currency, rail, or benefits | Accepted payload snapshot compared at webhook/replay |
| Uncertain payment safety | Provider commits then drops response; retry/restart resolves by exact receipt | Fake provider journal proves no duplicate mutation |
| Webhook ordering | Duplicate, delayed, thin, and out-of-order Stripe/NMI/CCBill events | Event ledger plus convergence worker; final state independent of delivery order |
| Refund/cancel/rebill | Full/partial refund, cancellation, failed retry, late success | Durable intent, exact provider receipt, access revoke only after proven refund |
| River durability | Enqueue, crash before/after claim, retry, snooze, and restart | One PostgreSQL River table and worker receipt; no in-memory substitute |
| Migrations/schema ownership | Fresh, concurrent, replayed, and drifted migrations | Per-process database, advisory lock, content hash and owner checks |
| Embedded/remote parity | Same table-driven operation through both transports | Normalized response/error/ledger/provider-journal diff |
| Frontend checkout | Browser registration, tokenization, pending/settled access, and `/me` | Playwright trace plus server receipt; no card PAN fixtures |
| Secrets/sandbox posture | Live key, test key, missing key, and loopback fake combinations | Config rejects unsafe combinations before provider traffic |
| Account deletion | Soft delete, self/operator recovery, 30-day hard delete, callbacks | AuthKit lifecycle callback recorder and retained billing rows |

The replacement manifest should assign every retained workflow row and every
current integration test to one matrix row. A test may be retained as a
regression when it proves a unique failure mode; otherwise it must point to the
focused scenario that replaces it.

The initial workflow-cell inventory is checked in at
`compatibility/focused-test-map.tsv`. Its rows intentionally say `planned`:
they are coverage obligations, not evidence that a replacement already exists.
The implementation must add focused scenario IDs and receipt paths to those
rows before changing a status to `covered`.

## Deterministic fixture contract

Create a small internal `testkit` with these explicit capabilities:

* `PostgresProcess` provisions one disposable database per process and exposes
  privileged migration and unprivileged runtime pools separately.
* `MerchantFixture` creates isolated merchant, customer, catalog, and provider
  records with stable readable handles and random internal IDs.
* `ProviderJournal` implements Stripe/NMI/CCBill request/response scripts,
  post-commit loss, exact read receipts, duplicate events, and event reordering.
* `RiverFixture` starts one worker fleet over the same database and records
  enqueue/claim/snooze/complete receipts.
* `PrincipalFixture` produces neutral user/service/delegated/API-key identities
  with explicit authority scopes; it never bypasses the verifier contract.
* `TopologyFixture` constructs embedded and HTTP Clients from the same scenario
  data and normalizes their results for comparison.

Fixtures must not use process globals, wall-clock sleeps, arbitrary retry
counts, shared Redis flushes, or provider network calls. Every test gets a
context deadline tied to its scenario budget and reports progress before any
long provider wait.

## CI shape and budgets

The required PR workflow should run independent jobs in parallel:

1. Static/unit and generated-contract checks: 2 minutes.
2. Database invariants: four shards, five-minute job timeout.
3. Provider/durable operations: four shards, six-minute job timeout.
4. Embedded/remote parity: two shards, five-minute job timeout.
5. Browser/frontend contracts: one worker, eight-minute job timeout.

The wall-clock target is under ten minutes on a warm GitHub Actions cache and
under fifteen minutes on a cold cache. Every shard uploads a JSON receipt and
fails the job if its process exits nonzero or emits an incomplete manifest.
Container ownership is explicit: one PostgreSQL service per job, one Redis
container per process only where the invariant needs Redis, and no shared
mutable provider fixture between shards.

## Evidence gate before removing the old suite

The old suite remains required while the focused suite is introduced. Hard-cut
approval requires all of the following:

1. A checked-in mapping from every retained workflow row and every old
   integration package/test to a focused scenario, or an explicit “unique
   regression retained” decision.
2. Three consecutive exact-head CI runs where old and focused suites both pass,
   including race, security, SQL/migration, browser, and embedded/remote parity
   checks.
3. Mutation tests or injected-failure fixtures demonstrate that each critical
   matrix row fails when its safety condition is removed.
4. JSON receipts show the same merchant isolation, entitlement, payment
   uncertainty, webhook convergence, and deletion-callback outcomes in both
   suites. Any difference blocks deletion and receives an issue.
5. A release candidate run proves the focused suite on a fresh database and a
   restored database. No local-only timing or a green subset is sufficient.

Only after this evidence is accepted may old tests be removed in a separate
hard-cut change. This issue itself does not delete tests or alter required
coverage.

## Implementation sequence

1. **Freeze the inventory.** Validate the TSV map against
   `compatibility/workflows.tsv`, enumerate every integration package/test, and
   mark unique regressions that must remain. Add a checker that rejects an
   unknown current workflow cell or a focused row without an owner.
2. **Build fixtures without changing production code.** Land the process-owned
   PostgreSQL/River/provider/principal/topology helpers and prove their cleanup
   and connection budgets with small contract tests.
3. **Port one vertical slice.** Implement merchant isolation, neutral authority,
   catalog/resource access, one-off checkout, and embedded/HTTP parity. Run it
   beside the old workflows and record normalized receipts.
4. **Port financial uncertainty.** Add recurring admission, frozen terms,
   provider journals, duplicate/out-of-order webhooks, refunds, cancellation,
   rebill, and River restart scenarios. Add mutation fixtures that deliberately
   remove each safety fence and require failure.
5. **Port schema and browser contracts.** Add migration ownership/drift,
   account-deletion callbacks, channel/editor flows, tokenized checkout, and
   `/me` billing lifecycle. Keep live PSP checks in their existing gated job.
6. **Run the dual-suite gate.** Require three exact-head runs with the old and
   focused suites, compare receipts and failure injection results, then obtain
   explicit review for any hard-cut deletion in a separate change.
