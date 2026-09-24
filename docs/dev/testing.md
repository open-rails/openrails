# Testing

OpenRails' required database and provider behavior is tested by the compact
greenfield suite. It uses the public embedded API, one disposable PostgreSQL
DSN, a random schema per test, and deterministic Stripe/NMI transports. It does
not use the former integration harness, Redis, testcontainers, browser
automation, or direct application-table SQL.

Run the focused suite locally:

```bash
OPENRAILS_GREENFIELD_DSN='postgres://postgres:postgres@127.0.0.1:5432/openrails_test?sslmode=disable' \
  bash scripts/greenfield.sh
```

The suite runs with `-race`, `-count=1`, one package process, and serial tests.
It covers migration replay, catalog and entitlement isolation, checkout
idempotency, Stripe webhook convergence, provider-owned NMI subscriptions with
dunning state, engine-owned NMI admission, and exact integer money/currency
boundaries. The retained legacy engine-subscription workflow remains outside
this compact gate only as historical confirmation evidence until its lifecycle
is represented by a focused contract.

The ordinary CI checks run pure unit/contract tests, source guardrails, builds,
frontend checks, and security scans. The greenfield workflow is the only
required database/provider integration job. Live PSP or blockchain qualification
must be invoked explicitly and is not a merge check.

## Business time and money

Business-time code uses the runtime clock; tests advance a fake clock rather
than sleeping. Currency amounts are integers at the registered native scale,
with exact conversion at provider boundaries. The focused money contract pins
rounding, overflow, unknown currencies, USD sub-cent rejection, and JPY's
zero-decimal scale.

## Adding a scenario

Add a small public-client contract to `ci/greenfield`. Use a deterministic local
transport for provider behavior, keep each test on a fresh schema, and assert
the durable result and the negative safety case. Do not add a new broad test
runner or reintroduce the deleted integration-package partition.
