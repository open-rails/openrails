# Focused OpenRails CI

The focused suite runs two packages. Ordinary
unit/static/security checks run separately. The old broad integration runner,
browser harness, scheduled backstop and devnet workflows have been removed.

`ci/greenfield` tests migration replay, catalog/merchant isolation, product
provisioning, hosted checkout idempotency, signed webhook replay, exact integer
money, browser-safe JSON and recovery of the rescue worker itself after a crash.
`ci/greenfield/subscriptions` exercises:

- NMI and Stripe engine-owned confirmation and renewals;
- provider-owned schedules and a distinct NMI/OpenRails-dunning hybrid;
- soft/terminal declines, retry timing and card replacement;
- cancellation, resumption, account-deletion cancellation and repricing;
- refunds, authentication abandonment and interrupted-operation recovery;
- the same subscription operations through embedded and HTTP clients.

See the [feature coverage map](greenfield-coverage.md) for what these tests do
and do not establish. Test count is not a percentage of functionality covered.

Run the exact CI command against a disposable PostgreSQL database:

```sh
OPENRAILS_GREENFIELD_DSN='postgres://postgres:postgres@127.0.0.1:5432/openrails_test?sslmode=disable' \
  bash scripts/greenfield.sh
```

The runner selects both packages with race detection and no test-result cache,
running at most four lifecycle scenarios concurrently.
Each test owns a random schema; migrations use the libraries' public entry
points. Lifecycle setup and assertions use public clients/HTTP. The crash
fixture rewinds durable job/intent state to model interrupted execution, and
operator fixtures arm destructive switches through SQL; these narrow exceptions
are explicit. The suite uses no legacy harness, Redis, testcontainers, browser
or real PSP credentials. It fails when its database DSN is missing.

Fakes check provider request/receipt contracts; they are not live PSP or chain
qualification. Such qualification requires separate operator evidence.
