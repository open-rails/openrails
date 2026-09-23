# Greenfield CI slice

The greenfield suite is the small contract gate for the OpenRails API. It is
intentionally independent of `internal/dbtest`, `internal/integrationharness`,
Redis, testcontainers, provider credentials, browser automation, and direct
application-table SQL.

The first slice has three focused scenarios:

- fresh migration plus replay, product/price creation, and entitlement offer
  selection;
- two merchant scopes with isolated products, customers, and entitlements;
- repeat catalog provisioning through `Products.Ensure`.

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
backstop while this compact gate grows. It is not removed until focused
scenarios have earned equivalent receipts.
