# Provider-neutral embedded authentication

Owner: `/root/astra_tracker_finish` (OpenRails extraction); foundation contract and
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
