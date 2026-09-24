# Testing

OpenRails' required database and provider behavior is tested by the compact
greenfield suite. It uses the public embedded API, one disposable PostgreSQL
DSN, a random schema per test, and deterministic Stripe/NMI transports. It does
not use the former integration harness, Redis, testcontainers, browser
automation. Ordinary lifecycle setup uses the public client and HTTP routes;
the subscription suite uses narrow SQL fixtures to simulate crash recovery and
arm operator-controlled destructive-action policy.

Run the focused suite locally:

```bash
OPENRAILS_GREENFIELD_DSN='postgres://postgres:postgres@127.0.0.1:5432/openrails_test?sslmode=disable' \
  bash scripts/greenfield.sh
```

The suite runs with `-race`, `-count=1`, one package process, and serial tests.
It covers migration replay, catalog and entitlement isolation, checkout
idempotency, Stripe webhook convergence, provider-owned NMI subscriptions with
dunning state, engine-owned NMI admission, and exact integer money/currency
boundaries. The legacy engine-subscription workflow has been removed. The focused
`ci/greenfield/subscriptions` scenarios now cover engine- and provider-owned
Stripe/NMI lifecycles, including confirmation, renewal, dunning, cancellation,
refunds, and crash recovery. See the [coverage map](../greenfield-coverage.md)
for the precise scope and remaining gaps.

The ordinary CI checks run pure unit/contract tests, source guardrails, builds,
frontend checks, and security scans. The greenfield workflow is the only
required database/provider integration job. Live PSP or blockchain qualification
must be invoked explicitly and is not a merge check.

## Business time and money

Business time covers billing periods, entitlement validity, cancellation,
renewal, dunning retries, checkout expiry, and credit/hold expiry. It uses the
runtime `clockwork.Clock`; supply `embed.Options.Clock` before constructing a
test runtime and advance the fake clock rather than sleeping. This seam is
refused with live provider credentials. Infrastructure time (cache TTLs, rate
limits, webhook signature tolerance, and transport retry backoff) may use wall
time when elapsed wall time is the behavior under test.

`bash scripts/check_business_time.sh` scans domain paths for direct `time.Now()`,
SQL `NOW()`/`CURRENT_TIMESTAMP`, and `clockwork.NewRealClock()` calls. Existing
exceptions are classified in `scripts/business-time-allowlist.txt`; new billing
logic should use the runtime clock instead of adding an exception.

Currency amounts are integers at the registered native scale,
with exact conversion at provider boundaries. The focused money contract pins
rounding, overflow, unknown currencies, USD sub-cent rejection, and JPY's
zero-decimal scale.

## Adding a scenario

Add a small public-client contract to `ci/greenfield`. Use a deterministic local
transport for provider behavior, keep each test on a fresh schema, and assert
the durable result and the negative safety case. Do not add a new broad test
runner or reintroduce the deleted integration-package partition.
