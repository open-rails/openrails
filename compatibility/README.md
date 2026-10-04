# Compatibility contracts

`contract.json` is the reviewed Go API and JSON wire snapshot.

The HTTP contract is generated from the route catalog (`internal/http/routes`)
and the error-code registry (`billing.ErrorCodes`): `api/openapi.json`, the
TypeScript wire types of `sdk/billing-ui` and `web/admin`, and
`docs/api/routes.md` and `docs/api/error-codes.md`.

`api/go.txt` lists the Go API of the public packages (`openrails`, `billing`,
`catalog`, `adapters/*`, `web/admin`), one exported constant, variable,
function, type, field or method per line, so a pull request shows its API
change as a diff. It also refuses an internal type the public API reaches
without a public name, and helper methods on the aliased `Config` family.

Regenerate all of them with `go run ./scripts/contracts -write` and review the
diff; `go run ./scripts/contracts` checks them, as `go test` does.

The former multi-deployment workflow matrix and integration-test runner were
removed with the legacy CI system. Database/provider behavior is now qualified
by the focused e2e contracts in `ci/`; live provider
qualification is performed explicitly outside the merge gate.
