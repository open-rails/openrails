# Compatibility contracts

`contract.json` is the reviewed API and wire contract snapshot. The required CI
checks verify that exported API, JSON wire, route, authority, and schema changes
are intentional.

`api/go.txt` lists the Go API of the public packages (`openrails`, `billing`,
`catalog`, `adapters/*`, `web/admin`), one exported constant, variable,
function, type, field or method per line, so a pull request shows its API
change as a diff. It also refuses an internal type the public API reaches
without a public name, and helper methods on the aliased `Config` family.

Regenerate both with `go run ./scripts/contracts -write` and review the diff;
`go run ./scripts/contracts` checks them, as `go test` does.

The former multi-deployment workflow matrix and integration-test runner were
removed with the legacy CI system. Database/provider behavior is now qualified
by the focused e2e contracts in `ci/`; live provider
qualification is performed explicitly outside the merge gate.
