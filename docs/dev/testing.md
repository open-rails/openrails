# Testing

Two kinds of test gate a pull request.

**Package tests** (`go test ./...` in each module, the `Checks` job;
`bash scripts/check.sh go`) need no database: unit
tests, wire and contract guards, source guardrails. Among them are the freeze
gates: `TestGoAPISurface`, `TestGeneratedContractIsFresh`,
`TestSchemaSnapshotNamesTheMigrations` and the documentation checks
(`TestDocsNameWhatExists`, `TestDocsLinksResolve`). See
[compatibility](../compatibility.md).

**The end-to-end suite** (`ci/` and `server/ci`, the `End-to-end` job) is the only database and
provider test lane. It drives the public Client and the mounted HTTP routes
against a real PostgreSQL 18, with deterministic NMI and Stripe transports.
Every test migrates its own random schema, so tests never share state. It needs no browser and no real PSP credentials, and it fails when its
database is missing.

```bash
OPENRAILS_E2E_DSN='postgres://postgres:postgres@127.0.0.1:5432/openrails_test?sslmode=disable' \
OPENRAILS_E2E_REDIS_ADDR=127.0.0.1:6379 \
  bash scripts/e2e.sh
```

`scripts/e2e.sh` builds the packages under `./ci/...` of the root and server
modules once (tags
`e2e,integration`, `-race`) and runs each top-level test in its own process,
`OPENRAILS_E2E_JOBS` at a time, longest first. CI splits the suite across
`OPENRAILS_E2E_WORKERS` runners by the durations in `ci/e2e-durations.tsv`,
keyed by package directory; a test missing from it counts as the median. Redis is needed by the
tests of several instances sharing limits and the card-attack captcha tests.

- `ci/` covers migration replay, the schema snapshot (`TestSchemaSnapshot`),
  catalog and merchant isolation, checkout sessions and attempts, signed
  webhook replay, credits, usage and admissions, invoices, merchant
  configuration, retention and partitioning, and exact integer money.
- `ci/subscriptions` covers engine-owned and provider-owned subscriptions on
  NMI and Stripe (confirmation, renewals, declines and dunning, card
  replacement, cancel and resume, repricing, refunds, interrupted-operation
  recovery), imported CCBill memberships, and the same operations through the
  embedded and the HTTP client. `TestReplicas*` run two or three embedded
  replicas over one database to prove exactly-once rebilling; a crash cuts the
  replica's database link, so it records nothing afterwards.
- `server/ci` covers the standalone server and AuthKit: the control plane,
  registration, teams, merchant names and API keys, trusted issuers, the
  console, and AuthKit as an embedded host's `Auth`.
- Adversarial cases (IDOR, merchant isolation, webhook forgery, double-spend
  races, revocation, credential class) are indexed in
  [security tests](../security-tests.md).

Fakes check provider request and receipt contracts. They are not live PSP or
chain qualification, which is a separate operator activity
([rail matrix](../rails/certification-matrix.md)).

## Business time and money

Business time covers billing periods, entitlement validity, cancellation,
renewal, dunning retries, checkout expiry, and credit and hold expiry. It uses
the engine's `clockwork.Clock`: supply `Deps.Clock` before constructing a test
engine and advance the fake clock rather than sleeping. This seam is refused
with live provider credentials. Infrastructure time (cache TTLs, rate limits,
webhook signature tolerance, transport retry backoff) may use wall time when
elapsed wall time is the behavior under test.

`bash scripts/check_business_time.sh` scans domain paths for direct
`time.Now()`, SQL `NOW()`/`CURRENT_TIMESTAMP`, and `clockwork.NewRealClock()`
calls. Existing exceptions are classified in
`scripts/business-time-allowlist.txt`; new billing logic uses the engine clock
instead of adding an exception.

Currency amounts are integers at the registered native scale, with exact
conversion at provider boundaries. Every provider money boundary has a test
that pins a known amount to its exact wire value.

## Adding a scenario

Add a small public-client contract to `ci/`. Use a deterministic local
transport for provider behavior, keep each test on a fresh schema, and assert
the durable result and the negative safety case. A deliberate behavior change
sweeps `ci/` and `server/ci` for the codes, statuses, paths and fields it
changed: green in one package is not green.
