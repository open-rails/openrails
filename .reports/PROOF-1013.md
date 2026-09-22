# #1013 package partition proof

Base ca38c10d4e1a10c89cde6a0a71b2677e6706f291. Owner /root/astra_retry_compare. Draft PR595.

Checks retains default build/vet, integration vet, source/security guards and the console gates. Its race test command now selects the package complement of E2E, plus any entire package where Go's integration/browser file selection omits default production or test files. E2E retains the full existing integration package selector and gains -race. SQLAudit's nonvacuous gate and the unchanged63-row/55-path/28-root workflow manifest remain in place.

Focused proof passed: shell syntax, existing integration-wrapper ownership/teardown tests, real Go metadata fixtures for mixed/default-only-test/default-only-production/integration-only packages, metadata error/empty-selection controls, and the nonvacuous Go test gate self-tests. List-only selection starts neither tests nor Docker.

Actual Go metadata on this base:141 default packages;84 Checks and62 E2E packages;146-package union; no missing default files; no current fallback exceptions. Source Test-root census:1581 default,512 Checks,2356 E2E;1069 repeat selections removed,0 remaining. Counts are selected top-level Test roots, not wall time or subtest counts. No physical Go tests were deleted.

The whole CI workflow must qualify this split. Runtime improvement is unmeasured: integration-only tests now also run under the race detector, potentially increasing E2E runtime or exposing real races. Those failures must be investigated rather than removing race/coverage. Existing job/test budgets are unchanged.

## First whole-CI result and concurrency correction

Exact413e968ea CI35676656309: ChecksPASS10m55s, RequiredSecurityPASS1m59s, E2EFAIL27m20s. Every one of the63 manifest rows passed; the only test failure was the race detector in TestConverge_OverlappingRuns_SingleClawback. Concurrent Converge calls both wrote SubscriptionLifecycleService.clock through SetClock. The unchanged whole-run failure gate correctly rejected this non-manifest failure.

Verified baseline: ca38's tree is identical to corrected590head6d1d3e6bb0de3c89342e896bff56a65a87ebcfd6, whose CI35673883738 passed all3 jobs (Checks13m15s/E2E19m24s). Older447 failures were superseded and are not this baseline. Cross-run timings are not a controlled performance benchmark; no overall speedup is claimed.

Root approved the minimal production correction after inspecting the lifecycle struct (no locks/once/atomic or owned mutable collections): LIFE takes a local lifecycle value copy, binds it to its existing detection instant, and captures it in its two repairs. Converge no longer mutates the shared clock. No constructor/public API, global lock, test serialization or assertion change was introduced.

Owned-PG negative proof: an immutable overlay of both unchanged413 production files reproduces the data race and fails1.330s. Fixed entire convergence package under-race passes7.711s, including overlapping single-clawback, deterministic cancelled_at/ended_at, grant and plane-interleaving assertions. Existing TestApplyLocalCancellationPersistsTerminalState under-race passes2.160s. Root approved the exact7-insertion/5-deletion production diff. Logs are retained under .reports/converge-race. A new whole CI run is required on the corrected head with the partition, race flag, SQLAudit and all63 manifest gates unchanged.
