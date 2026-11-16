# CI and releases

## Branches

Feature branch → release candidate → `master` → tag.

- `master` is the release mainline. It takes pull requests only from the open release candidate (`rc/X.Y.Z`) and from `scripts/release` (`chore/release-vX.Y.Z`); the required `Source branch` check refuses any other head, documentation included.
- One release candidate is open at a time, named for the version it becomes (`rc/0.280.0`), cut from master and deleted once it merges. [OPEN.md](../OPEN.md) names it, or says none is open. Feature branches are cut from it and their pull requests target it.
- A release is a tag on master (`scripts/release`); not every master commit is one.

## Qualification

Ready PRs qualify the synthetic merge with the current target branch, in one of two tiers:

- **Light**, into `rc/**`: the selected static suites (guards, build and lint of every module, SQL bindings and contracts, browser build, chart, security, artifact snapshots). The `critical/` journeys do not run.
- **Full**, into `master`: every suite including `critical/`, with no selection, once for the whole candidate.

Explicit manual runs are full. Draft/closed PRs and ordinary branch/master/tag pushes do not run source CI. The final `CI Gate` requires every job its tier selected to pass, refuses a master run that skipped anything, and rechecks that the target branch has not moved.

For the light tier, `scripts/ci-scope.py` classifies changed paths automatically and reports each suite's selection and reasons. Unknown/shared source selects the broad source suite. Deleted/renamed inputs, dependency changes and artifact-build inputs select broader qualification. Only known isolated documentation, UI or chart inputs skip unrelated suites. `scripts/check-ci-policy.py` verifies this through actual Git diffs and the CLI.

The required behavioral suite is `critical/`: 148 selected logical scenarios, with exact executable paths in `critical/executables.json`. Source-only fixtures and helper tests are not scenario passes. Missing required owners, successful nested tests without their parent, duplicate mappings, skips, failures and empty runs fail qualification. Focused `--case` runs are local development only.

One critical job supplies actual PostgreSQL18, Redis7, Vault2.1.1, real AuthKit, the current embedded/standalone OpenRails and independent external provider fixtures. It builds the SDK, console and payment assets once (the light tier builds them in `Browser build`); the Go-hosted SDK cases own the browser execution. The standalone fixture binary is built once and reused with isolated per-scenario resources. Pure money/parser/cryptographic boundary vectors use literal independent oracles and do not need browser fixtures.

Static checks retain source clock/migration/injection guards, compilation and lint for the published modules/examples, independent-host dependency proof, SQLC generation/query preparation, the reviewed contracts (`scripts/contracts`, schema read from PostgreSQL), the proof that a pull request rewriting its base's migrations builds the same schema (`scripts/schemaproof`: a deliberate re-squash of the baseline; a schema change adds a numbered migration and rewrites nothing), browser artifact/contract checks, chart checks and security scans. The retired wildcard test launches, twelve runtime shards, test-name gates, old browser fixtures and test-only EXPLAIN auditor are removed. The separate SDK workflow is a static/manual artifact check; it does not repeat the Go-hosted browser cases.

Artifact snapshots run when their build inputs require them. Published releases build/package the tested source and verify the artifacts; they do not repeat the source suite. Deployment uses immutable artifacts and remains distinct from a passing source gate.

The new candidate must complete its entire selected manifest before entering ready CI. A partial implementation is not a passing reduced suite. Full-run timeout is a ceiling; calibrate it from the combined qualified run, not an estimate from test counts.

## Dependencies

No bot opens dependency pull requests. GitHub's advisory feed lists what is vulnerable; `scripts/deps` prints its open alerts by manifest and package, each with the version that fixes it, and exits 1 while a fix is published.

- `Required Security` refuses a pull request while a shipped dependency has a fixable vulnerability. Development-only dependencies are outside that scan; the feed covers them.
- Updates are one batch per release candidate: run `scripts/deps` when the candidate is cut and bump what it lists in one pull request into it.
- A Go bump moves every module in step (root, `server`, `examples`, `adapters/authkit`, `critical/server-fixture`); a JavaScript bump moves every lockfile that carries the package.
