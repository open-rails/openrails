# #1013 package partition proof

Base ca38c10d4e1a10c89cde6a0a71b2677e6706f291. Owner /root/astra_retry_compare. Draft PR595.

Checks retains default build/vet, integration vet, source/security guards and the console gates. Its race test command now selects the package complement of E2E, plus any entire package where Go's integration/browser file selection omits default production or test files. E2E retains the full existing integration package selector and gains -race. SQLAudit's nonvacuous gate and the unchanged63-row/55-path/28-root workflow manifest remain in place.

Focused proof passed: shell syntax, existing integration-wrapper ownership/teardown tests, real Go metadata fixtures for mixed/default-only-test/default-only-production/integration-only packages, metadata error/empty-selection controls, and the nonvacuous Go test gate self-tests. List-only selection starts neither tests nor Docker.

Actual Go metadata on this base:141 default packages;84 Checks and62 E2E packages;146-package union; no missing default files; no current fallback exceptions. Source Test-root census:1581 default,512 Checks,2356 E2E;1069 repeat selections removed,0 remaining. Counts are selected top-level Test roots, not wall time or subtest counts. No physical Go tests were deleted.

The whole CI workflow must qualify this split. Runtime improvement is unmeasured: integration-only tests now also run under the race detector, potentially increasing E2E runtime or exposing real races. Those failures must be investigated rather than removing race/coverage. Existing job/test budgets are unchanged.
