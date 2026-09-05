# Merchant name authority

A merchant's billing UUID and AuthKit group UUID are stable identities. AuthKit owns the public name and its rename/forwarding/reclaim policy. A bound billing row's `slug` is a display projection; it must neither reserve a released name nor select an owner when that name changes hands.

AuthKit-free hosts own a separate local namespace. Live unbound merchant names remain unique. A local-only lookup selects unbound rows only; it cannot return a bound merchant based on its cached name. A configured AuthKit lookup resolves the group first and then selects the billing row by that exact group UUID, without falling back to the local namespace.

Name-bearing boundaries (CLI, manifest, catalog, HTTP) resolve the current name or valid forward once, through the configured authority. Imports, ledger operations and maintenance run against the resulting merchant UUID. An embedded host's explicit UUID binding remains the identity across name changes. Missing authority is an error for a bound-name operation; a stored projection is not a substitute.

Provisioning a newly reclaimed name creates a distinct billing identity for the new AuthKit group. The former group and merchant may remain active, with all their existing money and provider state. Re-provisioning the original group still returns its original billing UUID. An operator-supplied display name updates the selected UUID; it never performs a slug-based upsert over some other owner's row.

## Acceptance evidence

The change must prove rename, active forwarding, expiry and reclaim while the original merchant is still alive; both owners retain distinct billing identities and data. The same assertions apply to public resolution, provisioning, manifest/bootstrap, catalog and import/maintenance boundaries. Unbound host names must remain unique, ambiguous projections must never be picked arbitrarily, and all verification must use isolated databases.

The pre-launch cutover needs no old-name compatibility reader or second alias registry. AuthKit's own read-only directory API supplies the same canonical/alias semantics to tools that do not run its issuer, sessions or HTTP server.
