# Compatibility contracts

`contract.json` is the reviewed API and wire contract snapshot. The required CI
checks verify that exported API, JSON wire, route, authority, and schema changes
are intentional.

The former multi-deployment workflow matrix and integration-test runner were
removed with the legacy CI system. Database/provider behavior is now qualified
by the focused greenfield contracts in `ci/greenfield`; live provider
qualification is performed explicitly outside the merge gate.
