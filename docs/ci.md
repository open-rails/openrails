# Validation and release jobs

Ready pull requests are validated against GitHub's synthetic merge commit and the current target branch. Scope checks both parents before selecting suites. The single `CI Gate` requires every selected job to succeed and rechecks that the base did not move while tests ran. Failed/canceled/missing jobs cannot become a green gate through skips. Updating a stale PR requires another validation run.

There is no duplicate full suite on master push or release tags. Manual `Validate` runs every source suite and the artifact snapshots. Local `scripts/check.sh` and `scripts/e2e.sh` keep their full commands and tests.

| Changes | Selected work |
|---|---|
| Markdown prose used by contract tests | Existing cheap guards and the complete documentation-contract package |
| Explicit visual components/assets in billing-ui, payment page or admin console | All browser SDK/payment/example/admin checks and security |
| Explicit own-auth, standalone or embedded example files | Core Go/build/lint, examples, browser consumers and security |
| Helm chart | Chart and security |
| Dependencies, schemas, generated API/SDK clients, shared Go, unknown paths | All source suites |
| CI/build tooling, image/package inputs, deletion/rename or uncertain comparison | All source suites plus artifact snapshots |

LICENSE-only edits retain the cheap guards. Documentation contracts are already included when the full core suite runs, so they are not duplicated.

The source suites retain the full Go test commands across root/server/adapter/examples, the full twelve-way runtime E2E suite, SQL generation and `TestQueryAudit`, all SDK/browser consumers, and security checks. Selection never chooses individual tests or reduces the shard roster. The allowlist is intentionally small; mixed changes take the union, unknown paths expand coverage, and malformed comparisons fail.

`SDK` is reused by the central gate. Release/package and container workflows retain their artifact build, migration/readiness and security smoke checks, reused as nonpublishing snapshots when relevant to a PR. Version tags retain publication; explicit npm/image manual workflows remain available. Those artifact workflows do not rerun the source unit/E2E suites.

The branch protection requirement is `CI Gate`. Changing protection is a separate reviewed repository operation after this workflow exists; this source change does not mutate GitHub settings.
