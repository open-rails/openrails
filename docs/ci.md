# CI and releases

Ready PRs into `master` or `release/**` qualify the synthetic merge with the current target branch. Explicit manual runs select the full suite. Draft/closed PRs and ordinary branch/master/tag pushes do not run source CI. The final `CI Gate` requires every selected job to pass and rechecks that the target branch has not moved.

`scripts/ci-scope.py` classifies changed paths automatically and reports each suite's selection and reasons. Unknown/shared source selects the broad source suite. Deleted/renamed inputs, dependency changes and artifact-build inputs select broader qualification. Only known isolated documentation, UI or chart inputs skip unrelated suites. `scripts/check-ci-policy.py` verifies this through actual Git diffs and the CLI.

The required behavioral suite is `critical/`: 148 selected logical scenarios, with exact executable paths in `critical/executables.json`. Source-only fixtures and helper tests are not scenario passes. Missing required owners, successful nested tests without their parent, duplicate mappings, skips, failures and empty runs fail qualification. Focused `--case` runs are local development only.

One critical job supplies actual PostgreSQL18, Redis7, Vault2.1.1, real AuthKit, the current embedded/standalone OpenRails and independent external provider fixtures. It builds the SDK, console and payment assets once; the Go-hosted SDK cases own the browser execution. The standalone fixture binary is built once and reused with isolated per-scenario resources. Pure money/parser/cryptographic boundary vectors use literal independent oracles and do not need browser fixtures.

Static checks retain source clock/migration/injection guards, compilation and lint for the published modules/examples, independent-host dependency proof, SQLC generation/query preparation, the reviewed contracts (`scripts/contracts`, schema read from PostgreSQL), browser artifact/contract checks, chart checks and security scans. The retired wildcard test launches, twelve runtime shards, test-name gates, old browser fixtures and test-only EXPLAIN auditor are removed. The separate SDK workflow is a static/manual artifact check; it does not repeat the Go-hosted browser cases.

Artifact snapshots run when their build inputs require them. Published releases build/package the tested source and verify the artifacts; they do not repeat the source suite. Deployment uses immutable artifacts and remains distinct from a passing source gate.

The new candidate must complete its entire selected manifest before entering ready CI. A partial implementation is not a passing reduced suite. Full-run timeout is a ceiling; calibrate it from the combined qualified run, not an estimate from test counts.
