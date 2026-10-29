# Testing OpenRails

The ordinary suite is the named critical public-interface set under `critical/`. It exercises real services and registered workers. External provider protocol fixtures have independent economic journals; internal authentication, billing stores and runners are not mocked.

Set `OPENRAILS_CRITICAL_DSN`, `OPENRAILS_CRITICAL_REDIS_URL`, and `OPENRAILS_CRITICAL_VAULT_ADDR`/`OPENRAILS_CRITICAL_VAULT_TOKEN` to owned disposable services. Never point these at an application or production database. Fixtures refuse missing required services rather than skipping or selecting a default database.

```sh
bash scripts/check.sh guards
bash scripts/check.sh build
bash scripts/check.sh browser
pnpm --dir sdk/billing-ui exec playwright install --with-deps chromium
bash scripts/critical-tests.sh
```

For local iteration, `bash scripts/critical-tests.sh --case OR-J14-S05` runs a named scenario; it cannot qualify hosted CI. Pure public-value tests may be run directly with an exact `go test -run` or the SDK's Node driver without starting unrelated services. These focused runs do not claim the whole selected set passed.

Isolated UI changes run `bash scripts/critical-tests.sh --suite browser`: the scenarios under `suites.browser` in `critical/executables.json`. The same strict result gate requires every selected case; it is not an arbitrary CI filter. Shared, unknown and backend changes run every scenario in `critical/coverage.json`. Browser assets are built once in either selection.

`critical/coverage.json` is the required logical scenario roster; `critical/executables.json` is the independently reviewed executable map. Every selected owner must actually run and pass once. Nested successes cannot hide a missing parent. Real service cases may expand into provider/actor/time subcases; counts are reported separately.

To avoid repeated standalone compilation, build `critical/server-fixture` once with `GOWORK=off go test -race -c -p 2 -o <owned-path> .` and set `OPENRAILS_CRITICAL_SERVER_BINARY` to that absolute path. Set `OPENRAILS_CRITICAL_PAYMENT_ASSETS` to the current `web/pay/dist` for hosted-page cases. Keep the binary/asset cohort matched to the tested source. The main CI job does this automatically.

`OPENRAILS_CRITICAL_REPORT_DIR` retains structured Go results and browser evidence. Fixture state remains separate per case, even when immutable binaries and bundles are reused. The required run fails on missing/skipped cases, package failure, duplicate owners or unmapped cases.

`task sqlc-check` retains SQL generation and real PostgreSQL preparation. `go run ./scripts/contracts -dsn <disposable PostgreSQL 18>` checks the reviewed contracts ([compatibility](../compatibility.md)). `scripts/check_business_time.sh` runs the retained physical-clock source guard directly, and `scripts/check-embedded-auth-boundary.sh` compiles an independent host without linking AuthKit's runtime. These are static/compile checks, separate from behavioral coverage; CI's "SQL bindings and query audit" job runs sqlc and the contracts.
