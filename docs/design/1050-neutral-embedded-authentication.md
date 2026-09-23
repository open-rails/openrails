# Provider-neutral embedded authentication

Owner: `/root/astra_neutral_resume` (OpenRails extraction); foundation contract and
AuthKit implementation: `/root/astra_adapter_finish`; consumer hosts: release audit.
Worktree: `/home/fidika/cozy/.worktrees/openrails/1050-neutral-embedded-auth-20260923`
Branch: `refactor/1050-neutral-embedded-auth-20260923`
Base: `579653413a0b278e53003874904ed2069e4545a6`
Tracker: https://github.com/open-rails/tracker/blob/master/openrails/1050.md
Status: implementation in progress; no release or qualification claim.

The embedded billing library and its native adapters must have no AuthKit
production import dependency. The standalone OpenRails server may compose
AuthKit as its identity provider. Application hosts choose and construct their
own verifier; neither library imports the other to provide an adapter.

The shared helpers authentication protocol provides a per-request principal
with generic identity and an optional scoped permission checker. OpenRails
accepts AuthenticateRequest(context.Context, *http.Request) returning that
principal. Customer identity, payment interaction provenance and mapping billing
requirements to authority scopes remain explicit OpenRails host policy. Verified
credential restrictions remain captured in the principal; no permission snapshot
or mutable selector supplies authority.

Extraction includes shared billing credential types and permissions, billing-only
bootstrap declarations, host authentication configuration, shared middleware and
route handlers, and the embedded-to-standalone route construction dependency.
UserDirectory, UsernameResolver and merchant.NameAuthority remain generic seams.
Provider webhook signature verification remains billing infrastructure. No
financial identity or schema migration is required.

The separate PR640 worktree contains owned dirty work overlapping configuration,
embedded provisioning, control-plane facades, bootstrap, routes/server, models
and schema. This worktree neither copies nor edits that work. Integration must
preserve its authorized merchant-configuration and credential-publication changes.

Qualification requires dependency guards for root/embedded/native packages, an
independent non-AuthKit embedded host, actual standalone AuthKit composition,
existing merchant/customer/permission/DPoP and payment-interaction refusals, and
the coordinated published dependency graph before consumer adoption is complete.

## Host composition

`billingauth.NewIntegration` accepts the host's `Verifier`, optional `Customer`
mapping, and explicit `Authority` / `PlatformAuthority` mapping callbacks. The
verifier returns `helpers/auth.Principal`; its optional `PermissionChecker.Can`
method evaluates live authority for the host-resolved immutable scope. AuthKit
v0.126.0 implements this protocol directly. `SubjectCustomerID` is an explicit
opt-in for hosts whose native user UUID is also their billing customer UUID.
Other hosts supply their own issuer-aware mapping. No AuthKit role fallback or
credential permission list is part of the shared contract.

For a request already verified by trusted host middleware, an AuthKit host can
explicitly call `PrincipalFromVerifiedClaims` for that same unchanged request.
Ordinary `AuthenticateRequest` never reads ambient claims. Admission liveness is
explicit through `AuthenticateRequestLive`; ordinary native JWT bans remain lazy.

Billing config no longer has an Auth field. Standalone `hostauth/config.Config`
composes billing config with Auth config, which is passed explicitly through
control-plane construction. A standalone route bundle comes from the attached
control plane (`controlplane.HTTPRoutes`), while `embed.Runtime.HTTPRoutes` is
always billing-only. HTTP, Gin and Fiber accept either structural route source.
`embed.HTTPConfig.Standalone` and the public `embed/authkit` bridge are removed.

`scripts/check-embedded-auth-boundary.sh` checks actual package dependency
closures and compiles/runs an independent host in a temporary Go workspace.
The adapter check extends that fence to the Gin and Fiber modules. Neither
check introduces a replace directive into distributed go.mod files.

## Source qualification checkpoint

Published foundations: helpers v0.3.0 and AuthKit v0.126.0, with no replacement
module paths. Local race proofs pass for neutral request caching, nil/typed-nil
principal rejection, actual signed DPoP and replay denial, independent signed
customer/staff/machine requests against PostgreSQL, AuthKit live role/key
permissions, standalone root/subtree routing and the full Gin/Fiber integration
suites. The independent provider proves explicit customer mapping, token
issuer/audience integrity, live grant/revoke, preserved 403 admission denial,
and omission of host-owned team/API-key administration.

Initial PostgreSQL auto-container readiness failures and the independent test's
initial environment skip were not qualification. The actual successful runs
used a task-owned explicit PostgreSQL server and verbose output, including the
independent test's created database and unprivileged runtime role.

The ordinary suite caught and repaired extraction-specific regressions in
namespace-anchored wire permission matching and typed error test fixtures. The
reviewed contract snapshot was regenerated for the intentional public API cut.
Final exact-commit CI, integration with the separately owned runtime lane and
publication/consumer proofs remain release gates; this checkpoint is not a
production or release receipt.
