# Greenfield CI slice

The greenfield suite is the small contract gate for the OpenRails API. It is
intentionally independent of `internal/dbtest`, `internal/integrationharness`,
Redis, testcontainers, provider credentials, browser automation. Ordinary lifecycle setup uses public APIs. Narrow
subscription fixtures use direct SQL only for crash-state rewind and the
operator destructive-action switches.

The initial slice has eight focused contracts:

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

The `ci/greenfield/subscriptions` package adds engine- and provider-owned
Stripe/NMI lifecycle scenarios: confirmation, renewal, dunning, card replacement,
cancel/resume, repricing, refunds, and crash recovery. The
[coverage map](greenfield-coverage.md) records the exact cases and gaps.

Each test creates one random OpenRails schema in the PostgreSQL service, applies
the public `embed.ApplyMigrations` entry point, constructs an embedded runtime,
and drops the schema during cleanup. The suite never reads migration files. Ordinary billing fixtures use the
public client and HTTP routes; the subscription crash/operator fixtures are
the direct-SQL exceptions described above. A missing DSN is an error, so an empty test job
cannot report green.

Run it locally with:

```sh
OPENRAILS_GREENFIELD_DSN='postgres://postgres:postgres@127.0.0.1:5432/openrails_test?sslmode=disable' \
  bash scripts/greenfield.sh
```

The former broad integration, browser, and devnet harnesses have been removed,
including `native_engine_signup`. The contracts listed above are the current
coverage boundary. The new subscription scenarios earn their own lifecycle coverage; deleted
tests do not provide a current regression oracle. Real PSP and chain
qualification requires separate, explicitly scoped operator evidence.
