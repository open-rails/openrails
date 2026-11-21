# CI and releases

## Branches

Feature branch → release candidate → `master` → tag.

- Feature pull requests target the open release candidate (`rc/X.Y.Z`). See [OPEN.md](../OPEN.md).
- `master` accepts the release candidate or a release-script branch
  (`chore/release-vX.Y.Z)) from the same repository. This rule also applies to
  documentation. [Source branch check](../scripts/ci-source-branch.sh)
- `scripts/release` tags the version on `master`. [Release script](../scripts/release)

## Qualification

The [Validate workflow](../.github/workflows/ci.yaml) qualifies the synthetic
merge with the current target branch when a pull request is ready.

- **Light** applies to pull requests into the release candidate. Changed
  paths select source guards, build and lint, SQL and contract checks, browser
  build, chart, security, and artifact snapshots. Shared or unknown inputs
  select the broad source suite. Known isolated docs, UI or chart changes
  select only affected checks. [Suite selection](../scripts/ci-scope.py)
- **Full** runs every suite, including critical journeys, for pull requests
  into `master` and manual workflow runs.

Draft or closed pull requests and ordinary branch or tag pushes skip source
CI. The `CI Gate` requires every suite selected for the tier to pass and
rechecks the base commit. Published releases build and verify the release
artifacts from qualified source. Deployment has its own checks.

The [critical coverage manifest](../critical/coverage.json) lists the required
scenarios. Its [executable map](../critical/executables.json) names the Go tests
that must each run and pass once. The results checker requires a package pass
and rejects skips, failures, missing paths and duplicate mappings.
[Results checker](../scripts/critical-results.py)

Run `bash scripts/critical-tests.sh` with `OPENRAILS_CRITICAL_DSN` set to an
owned disposable PostgreSQL database. Add `--case OR-JNN-SNN` to run one
scenario locally. The full job runs OpenRails embedded and standalone with
AuthKit, PostgreSQL, Redis, Vault and independent provider fixtures. It
builds the standalone fixture, CLI and browser assets once. Go-hosted SDK
cases drive the browser. [Test runner](../scripts/critical-tests.sh) ·
[CI fixture setup](../.github/workflows/ci.yaml#L283) ·
[Service image pins](../scripts/ci-services.sh)

The SQL job checks SQLC, generated API and schema contracts, and schema
equivalence when an existing migration is rewritten. [Contract snapshots](compatibility.md) ·
[Schema proof](../scripts/schemaproof/main.go)

## Dependencies

Run `scripts/deps` when a release candidate is cut. It lists open GitHub
advisories by manifest and package and exits 1 when a fix is available.
Submit the updates as one pull request into the release candidate.
`Required Security` blocks fixable vulnerabilities in shipped dependencies.
The advisory feed also covers development dependencies. [Dependency report](../scripts/deps)

A Go version bump moves the root, server, examples, AuthKit adapter and
critical server fixture in step. Update every lockfile that carries a bumped
JavaScript dependency.
