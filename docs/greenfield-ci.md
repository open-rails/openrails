# Greenfield CI slice

The greenfield suite is the small contract gate for the OpenRails API. It is
intentionally independent of `internal/dbtest`, `internal/integrationharness`,
Redis, testcontainers, provider credentials, browser automation, and direct
application-table SQL.

The first slice has eight focused contracts:

- fresh migration plus replay, product/price creation, and entitlement offer
  selection;
- two merchant scopes with isolated products, customers, and entitlements;
- repeat catalog provisioning through `Products.Ensure`.
- checkout admission and replay through a deterministic Stripe transport,
  including changed-fingerprint rejection and entitlement access checks.
- signed Stripe completion, duplicate delivery, and stale expiration converge
  to one successful purchase and entitlement.
- provider-owned NMI subscription import preserves the provider schedule,
  dunning state, retry history, and replay behavior;
- engine-owned NMI admission remains local to OpenRails, creates no NMI
  recurring schedule, and waits for customer confirmation before charging.
- exact integer money parsing and currency-aware rail conversion, including
  half-away-from-zero rounding, overflow rejection, USD sub-cent refusal, and
  zero-decimal JPY scaling.

Each test creates one random OpenRails schema in the PostgreSQL service, applies
the public `embed.ApplyMigrations` entry point, constructs an embedded runtime,
and drops the schema during cleanup. The suite never reads migration files or
writes billing rows itself. A missing DSN is an error, so an empty test job
cannot report green.

Run it locally with:

```sh
OPENRAILS_GREENFIELD_DSN='postgres://postgres:postgres@127.0.0.1:5432/openrails_test?sslmode=disable' \
  bash scripts/greenfield.sh
```

The old broad integration workflow remains in `ci-full.yaml` as a scheduled
backstop while this compact gate grows. The legacy `native_engine_signup`
workflow remains the full engine-owned confirmation, renewal, dunning, and
entitlement oracle. The old suite is not removed until focused scenarios have
earned equivalent receipts.
