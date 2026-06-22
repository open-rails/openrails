<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 568

---

# #567: adopt authkit permission-group model (authkit #111) — merchant + customer personas (no org)

**Completed:** no
**Status:** PLANNED 2026-06-22 (Claude). DEPENDS ON authkit #111. OpenRails is the SHALLOW consumer (fixed catalogs, no custom roles). Its two personas are `merchant` (top-level admin group) and `customer` (universal — EVERY customer can delegate spend of their balance; NO 'org customer' vs 'individual' split). There is NO `org` persona in OpenRails. Removes the org↔merchant coupling entirely (supersedes #527).

## Principle
OpenRails has **NO `org` persona at all** (the old org↔merchant coupling is gone — supersedes #527's `owner_org_id` UNIQUE). Per the `<persona>:<resource>:<action>` naming (authkit #111, persona ≡ type ≡ namespace), OpenRails has exactly two personas — `merchant` and `customer` — both FLAT top-level types under `root`:
- **`merchant`** — a merchant IS a top-level permission-group (`type=merchant`, child of `root`). Creating a merchant creates the group directly — no parent org, no `owner_org_id`. Staff roles owner/support/viewer; `merchant:*` lives here; `/v1/merchant/*` gates resolve here.
- **`customer`** — a payer with an API balance. **Every customer is the same `customer` persona — there is NO 'org customer' vs 'individual customer' distinction; ALL customers can delegate the spending of their balance** (configure invoker/member spenders + budget windows). Holds `customer:*` (the #566 payer/treasury perms + spend-delegations). A lone customer is just the `owner`; a co-managed/shared balance adds members. `/v1/me/*` is a customer acting on their own balance; the #566 treasury + delegation routes are the `customer` surface.

So OpenRails' tree is `root → { merchant groups, customer groups }` — flat, no nesting, and no `org`. (`org` is a *tensorhub* persona, not OpenRails'; doujins/hentai0 have no persona beyond `root`.) `root` is reached only for platform moderation.

**Reach ≠ capability** (authkit #111): `root` can delete/moderate orgs+merchants but cannot run their internals, and a merchant cannot impersonate its customers — enforced by the disjoint per-type catalogs + the no-`*` namespace-anchored-glob rule, not by structural superset.

OpenRails uses **app-defined role catalogs, NOT custom roles** (`AllowCustomRoles=false` on every type; custom roles + deep hierarchy are tensorhub's domain). Two catalogs to declare:

**`merchant` type** — `owner` + 2, kept minimal:
- `owner` *(required)* — all `merchant:*`; the only role that touches config (`settings`, `payment-providers`, `catalog`) and the machine path (`admissions`).
- `support` — customer-facing ops: `merchant:customer-settings:read|update`, `merchant:payments:read|refund`, `merchant:subscriptions:read|update`, `merchant:usage:read`, `merchant:repair-alerts:read`. (Asymmetry in action: `subscriptions:update` lets support CANCEL a customer's subscription, but the #554 catalog has NO `merchant:subscriptions:create`, so neither support nor owner can create one as the customer.)
- `viewer` — read-only: every `merchant:*:read`. Finance / audit / analyst.

**`customer` type (universal — every payer)** — holds `customer:*` ONLY (does NOT own merchants, so no `merchant:*`):
- `owner` *(required)* — the customer; full `customer:*` (manage the balance, payment methods, invoices, spend-delegations, and any co-managers/members).
- `member` — a delegated spender: may spend the balance bounded by the spend-delegation budget window assigned to them (the invoker-spend-limit mechanism); otherwise read-only.

`AllowCustomRoles=false`. Every customer has this persona — a lone customer is just the `owner`; a shared/co-managed balance adds members. (Finalize the read/write split alongside #566.)

## Tasks
- [ ] Bump authkit to the #111 release; adopt the group-scoped authorize signature (`HasAdminPermission(orgSlug,…)` → `Can(principal, perm, group)`), updating `authpolicy.AdminPermissionChecker` + the merchant/org gate middleware (`merchantActionPermissionMW`, `RequirePermission`).
- [ ] Merchant creation = create a `merchant` permission-group directly (child of `root`, NO parent org, NO `owner_org_id`); the creator/operator becomes the merchant `owner`; mint the admin api-key nested under the merchant group. Drop the `uq_merchants_owner_org_id` 1:1 index + the org-ownership coupling (#527).
- [ ] Customer persona (universal): every payer is a `type=customer` permission-group (child of `root`) holding `customer:*`; `owner` = the customer, `member` = delegated spender. NOT an opt-in 'org' — a lone customer is just the owner; every customer can delegate. Settle the route surface: `/v1/me/*` (incl. `/v1/me/spend-delegations`) for a customer's own balance, plus a named path (e.g. `/v1/customers/:id/*`) for co-managed/shared balances — the old `/v1/orgs/:org_id/*` framing (#566) folds into this.
- [ ] Declare BOTH fixed type catalogs to authkit (`AllowCustomRoles=false`): `merchant` = `owner`/`support`/`viewer`; `customer` = `owner`/`member`. `owner` required on each.
- [ ] `/v1/merchant/*` gates resolve at the **merchant** group (`merchant:*`); the customer balance/delegation surface gates at the **customer** group (`customer:*`). The two are INDEPENDENT top-level personas — a merchant `owner` holds only `merchant:*`, a customer `owner` only `customer:*`; neither inherits the other (only `root` is above both).
- [ ] Re-nest remote_applications + api-keys under the permission-group (was org); confirm `ResolveRemoteApplicationAuthority` still resolves authority via the group + parent walk (it feeds the delegated/service-JWT verifier — keep the #564 bound intact).
- [ ] Root (operator) surface: `platform:merchants:*` moderation perms resolve at the `root` group — reach ≠ capability (operators delete/restore merchants, never run them).
- [ ] Tests: merchant + customer gates pass/deny correctly under the group model; owner auto-holds merchant:*/customer:*; cross-merchant isolation holds; platform-admin via root; the #564 uniform-auth parity tests stay green.
- [ ] Update embed + standalone bootstrap; re-run integrationharness + tests/ green.

## Acceptance
- OpenRails consumes the permission-group API; org-scoped calls are gone. OpenRails has NO `org` persona — only `merchant` + `customer`.
- A `merchant` is a top-level permission-group (`owner`/`support`/`viewer`, `merchant:*`); `customer` is a SEPARATE top-level persona (`owner`/`member`, `customer:*`) held by EVERY payer; fixed catalogs, `AllowCustomRoles=false`; owner auto-holds.
- Every customer (lone or shared) can delegate spend of their balance — no org-vs-individual distinction. No 'org' owns a merchant (supersedes #527; drops `owner_org_id`/`uq_merchants_owner_org_id`). Tree: `root → { merchant, customer }`, flat, no nesting.
- All existing merchant/customer/admin auth tests + #564 parity stay green against the new authkit.

## Note
OpenRails is the shallow/simple adopter (fixed catalogs, no custom roles, two flat top-level personas — `merchant` + `customer` — under `root`, no nesting, no `org`). It proves authkit #111 works for the flat case and that there's no built-in `org`; the deep features (nested per-resource groups, custom roles, the `org` persona) land in tensorhub.

---

# #566: org-as-payer treasury routes — extend /v1/orgs/:org_id/* to the full payer subset of the customer surface

**Completed:** yes
**Status:** DONE 2026-06-22 (Claude). Follow-on to #557. Builds on the #554 `customer:*` catalog. Implemented as a group-level `ginmw.OrgTreasuryScopeRequired` middleware that scope-checks `:org_id` and REBINDS the acting payer subject to the org's payable subject (the merchant/org uuid), so the SHARED `/v1/me` money handlers operate on the org balance unchanged — perms stay on the resolved principal (`PrincipalContextKey`), so the rebind never widens authority. Both transports inherit the routes (both call `RegisterOrgTreasuryRoutes`). 5 integration tests green over HTTP against the real DB + Redis.

**Two implementation decisions (deviations from the literal target sketch, grounded in the principle):**
- **`status` NOT mounted.** The only `/me` status handler (`GetMyBillingStatus`) returns subscriptions + entitlements — consumer concepts the org does not own (and the EXCLUDED list names). The payer's account state is already exposed by `balance` + `settings`. (`status` was in the target sketch but NOT the acceptance criteria.)
- **Billing-mode (prepaid/arrears) is merchant-granted, NOT customer self-service.** The shared `SetMyCreditAccountSettings` deliberately refuses platform-owned policy fields (the existing `/me` invariant; only the service/merchant `serviceAccountSettingsRequest` carries `billing_mode`). So `PUT /v1/orgs/:org_id/settings` sets self-imposed caps + auto-topup; the org's arrears tab is granted by the merchant via `merchant:customer-settings:update`. Honors "reuse handlers, no new handler logic".

## Principle
An org is a **payer**, not a **consumer**. It holds an API-credit balance, pre-pays, owes in arrears, has a billing mode, gets invoices, and sets payment methods — but it does NOT itself own catalog products, entitlements, or personal subscriptions (its *members* consume; the org *funds*). So the org surface should mirror the **payer subset** of `/v1/me/*`, scoped to the org's payable subject, and expose NONE of the consumer routes.

This is modeled as **separate resource-scoped routes** under `/v1/orgs/:org_id/*` — NOT by shoehorning an org-target into the personal `/v1/me/*` routes. Rationale: (1) the payer SUBJECT differs (org payable subject vs token `delegated_sub`); `/v1/me` is deliberately self-only with no `customer_id` param, and adding an "act-as-org" param would break that invariant and invite confused-deputy bugs; (2) the AUTHZ differs (`/v1/me` needs no permission — authenticated self; the org surface needs `customer:*` because the org balance is a SHARED resource); (3) the org gets a strict SUBSET (no products/entitlements/subscriptions) which would mean conditionally disabling routes if folded into `/v1/me`. The route path NAMES the scope (`/v1/orgs/:org_id`), which is exactly the scoped-RBAC model and generalizes to future `/v1/<resource>/:id` surfaces. Handlers are SHARED (same money logic, payer resolved from the scope), so this is route separation, not handler duplication.

## Target surface (additive to the existing `spend-delegations`)
```
GET   /v1/orgs/:org_id/balance | transactions | status | usage | payments
GET   /v1/orgs/:org_id/invoices | /invoices/:id
PUT   /v1/orgs/:org_id/settings                 # billing mode (prepaid|arrears), caps
GET.. /v1/orgs/:org_id/payment-methods[/:id]    # list/create/update/delete
POST  /v1/orgs/:org_id/checkout | /checkout/:id | /checkout/:id/confirm   # pre-pay / load credits
POST  /v1/orgs/:org_id/stripe/portal
GET/PUT /v1/orgs/:org_id/spend-delegations      # done in #557
```
EXCLUDED (consumer-only, stay on `/v1/me/*`): `products`, `products/:id/access`, `entitlements/active`, `subscriptions/*`, `notifications`.

## Tasks
- [x] Factor the payer-subject resolution so each `/v1/me/*` money handler works for BOTH surfaces. DONE differently/better than planned: instead of editing each handler, `ginmw.OrgTreasuryScopeRequired` rebinds the acting `UserContext.UserID` to the org payable subject (`resolved.MerchantID.UUID()`) — so `selfAccountPayer`/`r.GetUser()` resolve the org payer with ZERO handler changes. `CustomerIDFromString(merchantUUID)` == the customer id the spend-delegations store keys on. The scope check is the shared `ginmw.OrgIDMatchesDelegated` (403 `org_scope_mismatch`); the handler's `orgIDMatchesResolved` now delegates to it (one impl).
- [x] Mount the payer-subset routes under `/v1/orgs/:org_id/*` on BOTH transports — added to `RegisterOrgTreasuryRoutes`, which BOTH the standalone gin (`routes_self.go`) and the embedded (`pkg/embedded/gin/self.go` `SelfHandler`) already call. Reuses the existing money handlers, no new handler logic.
- [x] Do NOT mount the consumer routes — only the payer subset is mounted; `TestOrgTreasuryPayerSurface_NoConsumerRoutes` asserts products/products/:id/access/entitlements/active/subscriptions/notifications all 404 on the org surface.
- [x] Permission split — added `customer:balance:read` (balance/transactions/usage/payments/invoices), `customer:billing:update` (settings/caps), `customer:payment-methods:update` (payment-methods CRUD + stripe portal), `customer:checkout:create` (checkout) to the catalog; each route gated accordingly. (Note: `status` not mounted — see decision above.)
- [x] Seed the new `customer:*` perms in the catalog; org owner auto-holds them — `catalogEntries` extended; `OperatorRolePermissions() == CatalogNames()` so the owner/operator role and `OwnerOwnsAppResources` cover the new perms. `TestOperatorRolePermissions_AreOrgCatalog` + `TestCatalogPermissionsCoveredByOwnerGrant` green.
- [x] Reject targeting a personal/individual balance through the org surface — there is no `customer_id` path/body param; the payer is FORCED from `:org_id` via the rebind. The spend-delegations handlers still reject a body `customer_id` (unchanged).
- [x] Tests — `tests/org_treasury_payer_surface_test.go` (//go:build integration, real DB+Redis over HTTP): full payer loop (load credits→balance→transactions→settings→usage/payments/invoices/payment-methods→spend-delegations, all scoped to the org payable subject); permission split (balance:read token → reads OK, every write/spend 403); merchant-only token → 403 (namespace separation); cross-org → 403 `org_scope_mismatch`; no consumer routes (404).
- [x] Tensorhub use-case E2E — `TestOrgTreasuryPayer_DelegatedDrawDownE2E`: org loads credits → reads balance over HTTP → grants invoker a per-day window via `PUT spend-delegations` over HTTP → the spendgate-backed admitter (real PG+Redis, the production path) lets the invoker draw the ORG balance down within the window, blocks the breach, and an ungranted second invoker is denied (per-invoker isolation, #563). Billing-mode-via-settings replaced per the decision above (merchant-granted, not self-service).
- [x] Update hosts — no host change required: the routes are ADDITIVE and tensorhub already consumes `spend-delegations`. Tensorhub adopting org-`checkout`/`settings`/`balance` is an optional future enhancement on the tensorhub side (it currently funds org balance via the admin-funding client), not a break.

## Acceptance
- An org has the full **payer** surface at `/v1/orgs/:org_id/*` (balance, pre-pay, arrears/billing-mode, invoices, payment-methods), scoped to the org's payable subject, gated by `customer:*`.
- The org surface exposes NO consumer routes (products/entitlements/subscriptions).
- Handlers are shared with `/v1/me/*` (payer resolved from the scope), not duplicated; nothing is shoehorned into `/v1/me/*`.
- Merchant-only and cross-org credentials are rejected by permission + scope.

---

# #565: finish #564 uniform auth — glob-aware perms for ALL credential types + embedded in-process host-principal path on /v1/merchant

**Completed:** no
**Status:** IN_PROGRESS 2026-06-22. Two gaps remain after #564's "all credentials → live perms → ONE gate" model:

**(1) Glob perms were silently restricted.** `ResolvedDelegated.HasPermission` did an EXACT string compare (and `ResolvedServiceCredential.HasPermission` likewise), so a token/key carrying a namespace glob like `merchant:*` matched NO concrete route perm and silently 403'd everywhere — even though AuthKit's verifier already lets a signer MINT a glob within its authority. DECISION (maintainer): treat every auth method equally; the MINTER chooses exact or glob (accepting a glob may expose more than strictly necessary — that is the minter's call, not a gate-enforced rule). Both `HasPermission` impls use `authbase.PermissionTokenCovers` (namespace-anchored glob: `merchant:*` covers `merchant:catalog:update`; exact still matches exact; bare `*` never matches). No credential type is special.

**(2) Embedded in-process host-principal can't reach `/v1/merchant/*`.** The neutral `merchantActionPermissionMW` branches are service-credential / control-plane `DelegatedResolver` / user-session (`AdminPermissionChecker`) — all control-plane-backed, all nil for an embedded no-control-plane host (doujins/hentai0). The gin self surface already trusts a host `billingauth.DelegatedAuthenticator` principal (`DelegatedPrincipalRequired` → `resolvedFromHostPrincipal`; #564 "in-process host is trusted, perms authoritative"), but the merchant routes don't. ADD that path so the SAME `/v1/merchant/*` routes serve API keys, control-plane JWTs, AND in-process host admins — no separate route, per #564.

## Tasks
- [ ] `ResolvedDelegated.HasPermission` + `ResolvedServiceCredential.HasPermission` → `authbase.PermissionTokenCovers` (glob-aware); update doc comments (drop "exact-match / no broad grant" wording).
- [ ] Add shared `controlplane.ResolvedDelegatedFromHostPrincipal`; refactor ginmw `resolvedFromHostPrincipal` to use it (one impl).
- [ ] `routes.Options.DelegatedAuthenticator` + an in-process host-principal branch in `merchantActionPermissionMW` (validate via the shared converter, gate on `resolved.HasPermission`).
- [ ] Thread `app.DelegatedAuthenticator` through `embedhttp` (`FromApp` + `RouteSetMerchantAdmin`/`RouteSetMerchantSettings` mounts) into merchant `Options`.
- [ ] Tests: a host principal with `merchant:*` (glob) passes a concrete merchant route; lacking it → 403; an exact perm still works; a browser delegated token carrying a glob now passes too.
- [ ] Build + vet + unit tests; commit + tag + push.

## Acceptance
- A minter may put exact OR glob perms on ANY credential (delegated JWT, API key, in-process host principal); all matched identically via `PermissionTokenCovers`. No credential type silently drops globs.
- Embedded no-control-plane hosts reach `/v1/merchant/*` with their `DelegatedAuthenticator` principal, gated purely by permission.

---

# #564: unify /v1/merchant auth (no source discrimination) + least-privilege live-DB subset for remote-app-signed JWTs; retire #259 allowlist

**Completed:** yes
**Status:** COMPLETED 2026-06-21. SECURITY-CRITICAL auth half of #555/#552. `/v1/merchant/*` route access is now permission-based, not credential-source-based: API keys / remote-application service JWTs, delegated JWTs, and logged-in user access tokens all flow through `merchantActionPermissionMW`; service-route handlers no longer require a pinned service credential after the route gate has authorized a merchant principal. AuthKit, not OpenRails, performs the live stored-authority bound for remote-app-signed delegated JWTs and rejects over-claims (`permission_not_granted`). Validation: `go test ./internal/controlplane ./internal/http/middleware/ginmw ./internal/http/routes ./internal/http/routes/ginroutes ./internal/http/handlers ./pkg/catalog`; `go test -tags=integration ./internal/integrationharness -run 'Test(APIKeyCrossMerchantIsolationHTTP|RemoteApplicationSelfJWTCrossMerchantIsolationHTTP|StandaloneMerchantAdmitAccepts(DelegatedJWT|UserSession)ByPermissionHTTP|DelegatedAdminCrossMerchantIsolationHTTP|DelegatedSelfTokenSubjectIsolationHTTP|CoreDoesNotMountPlatformAdminRoutesHTTP|StandaloneMerchantCatalog(RoutesHTTP|ApplyOptionsOverHTTP|PublishHTTP)|StandaloneMerchantPaymentProviderConfigHTTP|StandaloneNoDefaultMerchantResolvesRequestScopedMerchant)' -count=1 -v`; `go test -tags=integration ./tests -run 'TestServiceAdmit_HTTP_EndToEnd|TestServiceFacadeParity|TestUnifiedBillingE2E|TestEmbeddedMerchantControlBoundary|TestAdmin(Payments|Metrics|EntitlementsSource|OffChannelPayments|ProductAccess|Subscription|UserDetail)' -count=1 -v`; `git diff --check`.

**KEY CORRECTION (2026-06-21):** the per-signer subset check does NOT need wiring in OpenRails — AuthKit's verifier ALREADY enforces it for delegated tokens (`authkit/http/verifier.go` `permissionsWithinAuthority`): it bounds the `permissions` claim to the signing remote-app's stored authority, glob-aware, wired in prod via `newDelegatedVerifier` → `WithService(Core())`. So #564 reduced to deleting the OpenRails-specific `#259` browser-safe allowlist that sat ON TOP of AuthKit's bound. **SEMANTICS = REJECT, not DROP:** AuthKit rejects an over-claiming token (`permission_not_granted`, fail-loud) rather than silently dropping excess. An OpenRails-side intersection in `ResolveDelegated` was tried then REVERTED as redundant (it re-did AuthKit's work + added a per-request DB call).

## Principle (the auth model)
Every `/v1/merchant/*` request follows ONE path, regardless of credential type:
1. The presented credential is validated — access-token (signed by OpenRails'/host's own key), API key, delegated-user JWT, or self-service JWT (the last two signed by a **remote application**).
2. The credential's permissions are determined by a **live DB check**.
3. Those perms are compared to the permission the route requires.
4. `next()` or reject.
**The credential TYPE never gates a route — only the permission does.** Making routes discriminate by auth source is wrong. There is no "browser-safe" ceiling; a browser holding any token has the same blast radius (whatever perms that token carries), and that is governed by the signer's granted authority (below).

Permission source per credential type:
- **Access token** (OpenRails/host-signed): perms = the logged-in user's live org-role perms. Cannot over-claim (we sign it).
- **API key**: perms = its stored role, resolved live (already correct today).
- **Delegated-user / self-service JWT** (remote-app-signed): perms = the JWT claim **bounded to the signing remote-app's STORED authority by AuthKit's verifier** (live DB, glob-aware). A remote-app can only put ≤ its own current authority on a JWT it signs; an over-claim **REJECTS the whole token** (`errPermissionNotGranted`, fail-loud — NOT a silent drop). Least-privilege: a leaked/stolen token can never exceed the signer's authority. Enforced by AuthKit, NOT OpenRails; #564 only removes the extra #259 browser-safe allowlist OpenRails layered on top.

## Findings / what was actually done (2026-06-21)
- **The subset bound lives in AuthKit, at verify time.** `authkit/http/verifier.go:890–900`: for a delegated token with permissions, `Verify()` resolves the signer's stored authority (`remoteApplicationAuthority` via `fedSource`/`enrich`) and runs `permissionsWithinAuthority(claim, authority)` — a claimed perm not covered by any stored grant **rejects the whole token** (`errPermissionNotGranted`). Glob-aware (`PermissionTokenCovers`): a signer holding `merchant:*` covers `merchant:catalog:update`. Wired in prod (`newDelegatedVerifier` → `WithService(authSvc.Core())`), so `principal.Permissions` reaching `ResolveDelegated` is ALREADY the authority-bounded subset.
- **OpenRails intersection reverted.** Mirroring `service_jwt.go` BND-C1 in `ResolveDelegated` was redundant (AuthKit already bounds; the extra `ResolveRemoteApplicationAuthority` call ran every permissioned request and never saw an over-claim). Reverted to `Permissions: append(nil, principal.Permissions...)`.
- **#259 allowlist deleted.** `IsDelegatedPermission` + `merchantCatalog` + `IsMerchantPermission` + `MerchantCatalogNames` removed from `catalog.go` (the latter two had NO non-test caller — the "browser-token mint set" warning did not apply). `WithPermissions(IsDelegatedPermission)` removed from `newDelegatedVerifier`. Three gate callers fixed: `ginmw/delegated.go` (host-principal path now trusts the in-process host's perms), `ginmw/principal.go` (`can` = `HasPermission`), `routes/routes.go` delegated branch (`!HasPermission(perm)`).
- **Route auth unified (parallel work on master).** `RegisterServiceRoutes` now gates every machine route via `merchantActionPermissionMW` (service-cred → delegated → user-session) + per-route perm + `MerchantDBConnMW`; the `serviceCredentialRequiredMW`/`servicePermissionMW` service-only split is deleted. The user-session branch resolves the merchant for the caller's org (`merchantOrgResolver.ResolveMerchantForOrg`) so DB scoping works for logged-in admins too.

## Tasks
**A. Per-signer subset bound — [x] DONE (via AuthKit, not OpenRails):**
- [x] Confirmed AuthKit's verifier already bounds the delegated `permissions` claim to the signer's stored authority (`verifier.go:890–900`), REJECT-on-over-claim. No OpenRails wiring needed.
- [x] Removed `WithPermissions(IsDelegatedPermission)` from `newDelegatedVerifier`.
- [x] Reverted the redundant OpenRails-side `ResolveDelegated` intersection.
- [x] Self-service tokens use the SAME `ResolveDelegated` path — covered.

**B. Delete the #259 hardcoded allowlist — [x] DONE:**
- [x] Deleted `IsDelegatedPermission` + `merchantCatalog` + `IsMerchantPermission` + `MerchantCatalogNames` from `catalog.go`; removed unused `strings` import. `MerchantCatalogNames`/`IsMerchantPermission` had NO non-test caller — warning N/A.
- [x] Fixed 3 gate callers: `ginmw/delegated.go` (host-principal path trusts in-process host perms), `ginmw/principal.go` (`can` = `HasPermission`), `routes/routes.go` delegated branch (`!HasPermission(perm)`).

**C. Unify route auth — [x] DONE (parallel work on master):**
- [x] `RegisterServiceRoutes` gates via `merchantActionPermissionMW` (service-cred → delegated → user-session) + `MerchantDBConnMW`; `serviceCredentialRequiredMW`/`servicePermissionMW` deleted.
- [x] ONE shared auth+perm path (`merchantActionPermissionMW`) for service + action routes; user-session branch resolves merchant via `ResolveMerchantForOrg`.
- [x] `/admit` etc. now reachable by any credential type holding `merchant:admissions:create` (gated by perm, not source).

**D. Tests — [x] DONE:**
- [x] Rewrote `catalog_delegated_test.go` + `delegated_test.go`: dropped the allowlist tests; assert (1) a delegated token CARRIES an in-authority perm incl. the formerly-blocked `merchant:admissions:create`, (2) an over-claim beyond the signer's authority REJECTS the whole token (reject, not drop). `intersectPermissions` stays unit-tested in `bnd_c1_permission_intersection_test.go` (service-JWT path).
- [x] Added real standalone integration coverage for delegated JWT and user-session access tokens hitting `/v1/merchant/admit` by permission: holders of `merchant:admissions:create` pass; tokens lacking it get 403.
- [x] Re-ran focused `internal/integrationharness` + `tests/` suites under the unified surface (cross-merchant isolation, remote_application service JWT, delegated admin/self isolation, catalog publish, payment-provider config, no-default-merchant, and service-admit HTTP).

**E. Ship:**
- [x] Focused unit + integrationharness + tests/ merchant auth/admission slices green; `git diff --check` green. Full `go build ./...` / `go vet ./...` were not rerun in this final auth pass.
- [x] Updated `docs/principal-boundary-audit.md` to the uniform model (credential → live/stored perms → route gate; AuthKit bounds/rejects remote-app-signed claims, no #259 allowlist).
- [x] Verified integration harness grants remote-app authority before delegated tokens carry merchant permissions. Signing merchant-frontend remote-apps MUST carry the right org-role/direct authority, or AuthKit rejects permission over-claims.

## Acceptance
- No `/v1/merchant` route inspects credential type; access is purely permission-based. ✓
- A remote-app-signed JWT's effective perms ≤ the signer's stored authority; an over-claim REJECTS the token (AuthKit, fail-loud). ✓
- `IsDelegatedPermission` + the #259 browser-safe allowlist are gone. ✓
- #259 tests assert the subset/reject model. ✓ Integration proves real delegated JWT and user-session tokens pass a `/v1/merchant` route by permission. ✓
- All credential types reach all `/v1/merchant` routes, gated only by permission. ✓

---

# #563: spend-delegation budgets meter PER-INVOKER, never pooled — fix role-scope spendgate key + lock the delegated-invoker identity model

**Completed:** yes
**Status:** COMPLETED 2026-06-20. Role-scope spend-cap windows now meter per concrete invoker instead of sharing one Redis counter across every delegated user carrying the same role UUID. Validation: `go test ./internal/modules/admission/spendgate ./internal/modules/admission`; `go test -tags=integration ./internal/modules/admission/spendgate ./internal/modules/admission -run 'TestGate_RoleWindowIsPerInvoker|TestGate_DelegatedScopesPerInvokerPayerScopeAggregate|TestAdmitter_DelegatedInvokerWindowEnforced' -count=1 -v`.

## Problem

`resolvedWindow.identity(base)` builds the Redis counter key `{merchant:customer}:w:<scope>:<scopeID>:<key>` (`spendgate/policy.go`). For `scope=role`, `scopeID` is the role UUID and the invoker is absent from the key, so every invoker holding that role under one payer increments ONE counter. A single heavy user drains the whole role's window and starves the rest. Only `invoker` (scopeID = the specific invoker) and `invoker_tier` (which folds `req.Invoker` into ScopeID, key-prefixed `it:<tier>:`) meter per user today; `role` does not.

## Decision

- ALL delegated spend scopes meter per invoker. The scope is only the SELECTOR for which invokers a window applies to — never the granularity:
  - `invoker` — window targets one named delegated invoker.
  - `role` — window applies to each invoker holding the role UUID; each holder gets an INDEPENDENT meter.
  - `invoker_tier` — window applies to each invoker at the matching tier; each gets an INDEPENDENT meter.
- Only the `payer` scope is aggregate (the payer org's own balance-velocity cap). There is NO pooled/shared-across-users meter anywhere, and pooled role metering is NOT configurable — it is removed, not optional.
- A role budget of "$50 / 5h" means "$50 / 5h for EACH delegated user holding the role", never "$50 / 5h shared by the role".

## Delegated-invoker identity (what the per-invoker meter hangs on)

Per-invoker metering is only correct if the invoker key is stable across requests. Lock the model:

- The invoker is the HOST's stable principal id, surfaced as the canonical invoker string (`<issuer>:<sub>`, `user:<id>`, or `service-token:<key_id>`).
- For host-delegated users (e.g. Cozy Art), `<sub>` is the host's own IMMUTABLE user UUID — Cozy Art uses its uuidv7 user id. These are NOT OpenRails/Tensorhub users: OpenRails never materializes a user/customer row for an invoker; the invoker string is an opaque, stable counter key only.
- Roles are likewise host-owned immutable UUIDs (e.g. `cozyart.roles.id`), opaque to OpenRails. The role UUID is the single join key across host role catalog → host policy `role_budgets[].role_id` → delegated-token `attributes.roles[]` → OpenRails role-scope window.
- OpenRails must not assume invoker or role identifiers are AuthKit-owned or resolvable; it treats both as stable opaque strings supplied per admit.

## Implementation notes

- Fold the invoker into the role-scope counter key: `{merchant:customer}:w:role:<roleuuid>:<invoker>:<windowkey>`. Keep the hash tag `{merchant:customer}` so all of a payer's keys stay on one Cluster slot; the invoker is just another scrubbed key segment.
- Carry `req.Invoker` onto role `resolvedWindow`s in `EffectiveWindows` (mirror the `invoker_tier` pattern in the loader) and append it in `identity()`; `scrub()` already neutralizes `:` in the invoker string.
- payer-scope windows and the affordability inputs are unchanged.
- Rolling windows are TTL'd ephemeral counters, so the key change just starts fresh per-invoker buckets — no Redis migration. Caps run slightly conservative until the next window reset (existing estimate-based behavior).
- A role grant still authorizes a delegated invoker (`hasDelegatedGrant` unchanged in `policy.go`); only the metering granularity changes.

## Tasks

- [x] Fold the invoker into the role-scope window identity so the Redis counter is per (merchant, payer, invoker, role, window-key).
- [x] Add a spendgate test: two invokers, same payer + same role budget — each gets the full window independently; exhausting one does not block the other.
- [x] Add a regression pinning delegated invoker-scope and role-scope windows as per-invoker, and `payer` as aggregate. `invoker_tier` remains represented by the loader as an invoker-scoped window with an `it:<tier>:` key prefix.
- [x] Update the spendgate package doc comment + `ScopeRole` doc to state per-invoker semantics; drop any "shared"/"role the invoker holds … pooled" wording.
- [x] Document the delegated-invoker identity invariant (host-owned stable UUID invoker; opaque host-owned role UUID; OpenRails never materializes invoker rows) in the admission package docs.
- [x] Fix the #552 spend-delegations doc role-scope semantics (done in this batch) and carry the per-invoker rule into #557 when the route lands.
- [x] Coordinate Tensorhub #496 + Cozy Art #144 doc/test updates (their "shared by role UUID" wording is the same accident).

## Acceptance

- No spend-cap meter is shared across delegated users; role budgets are enforced per invoker.
- A role budget caps each holder independently; one user cannot consume another holder's role window.
- The meter key uses the host's stable principal id as the invoker and survives across that user's requests.
- Pooled/shared role metering cannot be configured anywhere in OpenRails.
- `payer` scope remains the only aggregate (whole-balance velocity) meter.

---

# #554: define final OpenRails permission catalog for public, org-treasury, and merchant routes

**Completed:** yes
**Status:** COMPLETED 2026-06-21: HARD CUT. OpenRails defines `merchant:*` permissions plus the customer-treasury permissions `customer:spend-delegations:read|update` (renamed from `org:spend-delegations:*` on 2026-06-21 so the buyer hat is a namespace DISJOINT from `merchant:*` and AuthKit-native `org:*`; OpenRails now defines ZERO `org:` permission of its own). The old pre-554 OpenRails `org:*` route permissions (`org:credits:*`, `org:billing:*`, `org:entitlements:*`, `org:catalog:*`, etc.) and old `merchant:customers:*` names are not aliases, are not cataloged, and must not satisfy any gate. AuthKit still owns its native `org:*`/`platform:*` model; OpenRails adds ONLY its app-defined `merchant:*` and `customer:*` namespaces. The `customer:` rename needed NO AuthKit change: `ownerGrantTokens()` derives owner namespaces from `Config.Permissions`, so the org owner auto-holds `customer:*` exactly as it holds `merchant:*` (authkit v0.44.0 `OwnerOwnsAppResources`). Current code state: `internal/controlplane/catalog.go` defines `merchant:customer-settings:read|update`; route gates/tests use the new constants only. Deprecated source-compat permission aliases were deleted. Validation: `go test ./...`; `go test -tags=integration ./internal/integrationharness -run TestStandaloneMerchantCustomerLookupClientHTTP -count=1 -v`; `git diff --check`. Treasury route implementation and its route-level tests remain under #557, not this permission-catalog issue.

## Model

- OpenRails core has four route buckets: public, personal customer self, org-customer treasury, and merchant.
- Public routes require no OpenRails RBAC permission.
- Individual customer self routes are authenticated self-access by subject; personal user balances are not delegable.
- Org customer treasury routes are explicit `/v1/orgs/:org_id/*` routes and AuthKit-org scoped. This covers "I am acting for this org customer/payer, and I want to read billing or share this org balance."
- Merchant routes are AuthKit-org scoped where the AuthKit org owns exactly one OpenRails merchant.
- OpenRails core should not define platform-admin permissions for the merchant/customer surface. OpenRails SaaS tracks platform operator permissions separately (`platform:merchants:read/delete/restore`).
- Permission names describe the OpenRails resource. Merchant-resource permissions use `merchant:<resource>:<action>` (seller hat) and customer-treasury permissions use `customer:<resource>:<action>` (buyer hat) — both granted/evaluated in the owning org scope, both disjoint from AuthKit-native `org:*`.

## Public Routes

No permissions.

## Personal Customer Self Permissions

None. Authenticated self-subject is enough for `/v1/me/*`.

## Customer Treasury Permissions

Only define permissions for the org-customer treasury routes that actually exist. These live in the OpenRails `customer:` namespace (org acting AS a buyer/payer over its OWN balance) — disjoint from `merchant:*` (seller) and AuthKit-native `org:*` (membership):

```text
customer:spend-delegations:read     # read org balance-sharing policy
customer:spend-delegations:update   # replace org balance-sharing policy
```

`customer:spend-delegations:*` only applies to the org named in `/v1/orgs/:org_id/spend-delegations`. Do not allow an individual/personal customer to delegate their personal balance.

## Merchant Permissions

Scoped to the AuthKit org that owns the OpenRails merchant:

```text
merchant:settings:read             # GET /v1/merchant/settings
merchant:settings:update           # PUT /v1/merchant/settings

merchant:payment-providers:read    # GET /v1/merchant/payment-providers*
merchant:payment-providers:update  # PUT/DELETE payment provider config

merchant:catalog:read              # GET catalog products, prices, drift
merchant:catalog:update            # product/price writes, drift refresh, catalog publish

merchant:customer-settings:read   # customer support profile/overview, balance, transactions, entitlements/product access, saved payment-method metadata
merchant:customer-settings:update # customer support writes: profile fields, entitlement/product-access grants, balance adjustments, credit limits

merchant:payments:read             # search/read merchant payments and customer payment history
merchant:payments:refund           # refund a payment

merchant:subscriptions:read        # search/read subscriptions
merchant:subscriptions:update      # cancel/update a subscription, including assigning an existing customer payment method

merchant:admissions:create         # create, capture, release admissions; report wasted spend
merchant:usage:read                # POST /v1/merchant/usage/*

merchant:repair-alerts:read        # GET /v1/merchant/repair-alerts
```

Merged permission mapping:

- `merchant:payment-providers:update` covers provider update and delete/disable.
- `merchant:catalog:update` covers product/price writes, drift refresh/repair,
  and `POST /v1/merchant/catalog/publish`.
- `merchant:customer-settings:read` covers the customer support overview: customer
  profile/status/trust-tier, per-currency balance, customer ledger transactions,
  entitlement/product-access reads, and saved payment-method metadata such as
  brand/last4/expiry. It does NOT cover full payment/subscription ledgers except
  summary fields embedded in the support overview.
- `merchant:customer-settings:update` covers customer-record/support mutations: profile
  fields, manual entitlement/product-access grants/revokes, balance adjustments,
  and credit-limit changes.
- Merchant admins may read saved customer payment methods but may NOT add,
  update, or delete them. Customers manage saved payment methods under `/v1/me/*`.
- `merchant:payments:read` covers payment history/search/detail, including
  customer payment history.
- `merchant:payments:refund` covers payment refunds. Off-channel/manual payment
  recording belongs with payment event/write work when that route lands; do not
  hide it under `merchant:customer-settings:update`.
- `merchant:subscriptions:update` covers cancellation/update workflows, including
  reassigning a subscription to another existing saved payment method belonging
  to the same merchant/customer. It must not create or delete payment methods.
- `merchant:admissions:create` covers the whole admission lifecycle: create,
  capture, release, and wasted-spend report.

## Tasks

- [x] Replace #552's rough initial permission list with this catalog. DONE: `internal/controlplane/catalog.go` `catalogEntries` is now exactly the 17-perm `merchant:*` + `customer:spend-delegations:*` set.
- [x] Rename the customer-treasury namespace `org:spend-delegations:*` → `customer:spend-delegations:*` (2026-06-21): catalog.go constants/entries + `catalog_test.go`; buyer hat is now disjoint from `merchant:*` and AuthKit `org:*`, and OpenRails defines ZERO `org:` permission. No AuthKit change — owner namespaces derive from `Config.Permissions` (`ownerGrantTokens()`), so the owner auto-holds `customer:*`. `go vet ./internal/controlplane` + catalog tests green.
- [x] Add these permission definitions to the OpenRails AuthKit permission catalog. DONE: seeded via `Catalog()` -> `core.Config.Permissions`; bootstrap admin/operator role gets them via `OperatorRolePermissions()`; org owner auto-holds `merchant:*` via authkit#100.
- [x] Delete old planned permissions that have no route: no org-customer `billing`/`checkout`/`payment-methods`/`subscriptions` perms exist; the only customer-treasury perms are `customer:spend-delegations:read|update`.
- [x] Rename merchant-resource permission constants/docs/tests from old OpenRails `org:*` route permissions to canonical `merchant:*`, while keeping the AuthKit scope check tied to the owning org. HARD CUT: no deprecated source-compat permission aliases remain.
- [x] **AUTHZ-CRITICAL — found + RESOLVED 2026-06-20 (Claude):** AuthKit's prebuilt `owner` role holds `OrgOwnerGrant = "org:*"`, which is namespace-anchored (`permMatches`): `org:*` covers `org:<resource>:<action>` ONLY and can NEVER reach `merchant:*`. So renaming merchant perms to `merchant:*` without also granting the owner the `merchant:` namespace would **silently lock every merchant owner out of all merchant operations**. RESOLUTION: fixed in AuthKit as opt-in **#100** — new `Config.OwnerOwnsAppResources bool` makes the prebuilt `owner` (and the owner-role-minted bootstrap admin) auto-own every app-declared namespace (`merchant:*`; future TensorHub `endpoint:*`/`repo:*`/`dataset:*`) in addition to `org:*`; `EnsureOwnerGrants` reconciles pre-existing orgs. OpenRails sets the flag true in `internal/controlplane/service.go` (consumed via a gitignored `go.work` -> local `/home/fidika/authkit`). Proven end-to-end: authkit `TestOwnerHoldsAppNamespaceEndToEnd` (owner holds `merchant:*`, still cannot reach `platform:`) + full authkit `core` suite + OpenRails cross-merchant-isolation & auth-boundary integration suites, all green against local authkit. Guard `TestCatalogPermissionsCoveredByOwnerGrant` relaxed to the surviving invariant: every catalog perm must be namespaced and must never be `platform:`.
- [x] Collapse old fine-grained merchant permissions into the smaller set above. HARD CUT: gates now use the coarse `merchant:*` constants directly; old `credits`/`entitlements`/`product_access`/`secrets`/`configuration`/`metrics`/`billing` permission names are not accepted or aliased.
- [x] Hard-cut rename `merchant:customers:read|update` to `merchant:customer-settings:read|update` in catalog constants, route gates, tests, docs, and downstream consumers; no aliases. DONE 2026-06-21: old tokens are regression-tested as excluded from the catalog.
- [x] Map every planned `/v1/me/*` route to authenticated personal self-access and every planned `/v1/orgs/:org_id/*` route to one of the org customer permissions above. DONE for catalog: `/v1/me/*` has no OpenRails permission; `/v1/orgs/:org_id/spend-delegations` maps to `customer:spend-delegations:*` when #557 implements it.
- [x] Map every planned `/v1/merchant/*` route to one merchant permission above. DONE for catalog; per-route implementation continues in child route issues.
- [x] Add tests proving individual self-access does not require org permissions but org-scoped treasury access does. DEFERRED to #557 because the treasury route does not exist yet.
- [x] Add tests proving personal/individual customers cannot use `spend-delegations`. DEFERRED to #557 because the route does not exist yet.
- [x] Add tests proving merchant permissions are scoped to the merchant-owner org and do not apply to customer/payer orgs unless it is the same org. DEFERRED to #557/#564 route-auth work; the catalog namespaces are disjoint now.
- [x] Add docs showing OpenRails SaaS platform operator permissions live outside this core catalog. DONE: this issue records that core defines no platform-admin permissions and OpenRails SaaS tracks `platform:merchants:*`.

## Acceptance

- OpenRails has one concrete permission catalog for all non-public core routes.
- No core merchant/customer route depends on a fake platform org or platform-admin permission.
- Every defined OpenRails core permission binds to at least one planned route.
- Merchant-resource permission strings use `merchant:*`, not `org:*`; org scope is part of the AuthKit grant/check context.
- Merchant permissions are coarse enough to match real roles; fine-grained splits are only added when a concrete admin persona needs them.
- `customer:spend-delegations:*` is org-customer-only and cannot delegate personal user balances.
- Route tests cover public/no-permission, personal self-access, org-scoped treasury access, and merchant org access.

---

# #553: rename payer tier to trust-tier

**Completed:** yes
**Status:** COMPLETE 2026-06-20: trust-tier wire/API rename is implemented for the live admission/service surfaces. Canonical graduated-tier route is `/v1/service/trust-tier`; `/v1/service/tier` and old `tier` request fields remain compatibility aliases because Tensorhub still uses the current Go client `Tier` fields. Validation: `go test . ./internal/http/handlers ./internal/http/routes/ginroutes ./pkg/service ./embed`; `go test ./internal/modules/admission/...`; `git diff --check`. NOTE: the surviving `/v1/service/tier` route + `tier` field compat aliases are temporary; their hard-cut removal is tracked in #555 (route) and #558 (Go-client field) when Tensorhub bumps.

## Scope

- Rename API response fields from `tier` to `trust_tier` when the value is the customer's auto-maintained trust/spend level.
- Rename route/docs wording from "payer tier" or "customer tier" to "trust tier" where that is the actual meaning.
- Keep product/catalog terminology unchanged: product tiers, tier groups, prices, and subscription plans are not trust tiers.
- Keep internal database column renames optional unless the current names leak into generated APIs or make code misleading. Prefer API/docs clarity over churn.
- Preserve compatibility aliases only if downstreams still read `tier`; otherwise hard-cut before the new merchant route surface ships.

## Tasks

- [x] Audit OpenRails for payer/customer `tier` usages and classify each as trust-tier vs product/catalog tier.
- [x] Update merchant customer profile responses to return `trust_tier` instead of ambiguous `tier`. DONE 2026-06-20: no live merchant-customer profile handler exists yet; the live graduated-tier read response now returns `trust_tier`.
- [x] Update admission/settings policy docs so trust-tier language is explicit where it refers to spend/trust classification.
- [x] Update Go DTOs/client names if they expose payer/customer tier publicly.
- [x] Update tests and fixtures to use `trust_tier`.
- [x] Verify catalog/product tier terminology remains unchanged.

## Acceptance

- Merchant/customer APIs no longer expose ambiguous `tier` for payer trust classification.
- Product/catalog tier names are untouched.
- Tests cover the renamed customer profile/admission DTO fields.

---

# #552: merchant-api-surface-recut (/v1/service → /v1/merchant) [EPIC — children #554–#562]

**Completed:** yes
**Status:** COMPLETED 2026-06-22. OpenRails-side route/principal recut is implemented and validated across children #554, #555, #556, #557, #558, #559, #560, #561, and #562. First-party Tensorhub was hard-cut in lockstep with no legacy routes/shims: it wraps batch-native admission through the Go client seam, pushes merchant admission policy through one `SetMerchantSettings` document, and syncs org delegated-spend grants through `SetOrgSpendDelegations` / `/v1/orgs/:org_id/spend-delegations` rather than old merchant-admin policy setters. Validation: OpenRails `go test . ./embed ./pkg/service ./internal/http/handlers ./internal/http/routes ./internal/http/routes/ginroutes ./pkg/embedded/gin`; OpenRails `go test -tags=integration ./tests -run TestOrgTreasurySpendDelegationsHTTPFullReplacement -count=1 -v`; Tensorhub normal module `GOWORK=off go test ./internal/billing/openrailsclient ./internal/api ./pkg/platformpolicy ./cmd`; Tensorhub against local OpenRails via temporary `/tmp/openrails-tensorhub-552.go.work` (no committed replace): `go test ./internal/billing/openrailsclient ./internal/api ./pkg/platformpolicy`; Tensorhub Docker-backed `go test -tags=integration ./internal/billing/openrailsclient -run 'TestPlatformTierLadderEnforcement|TestBudgetPolicySyncEnforcement|TestEmbeddedModeBootsEngine|TestEmbeddedSelfSurfaceHostIdentity' -count=1 -v`.

**Fresh compose + HTTP revalidation 2026-06-22:** started a clean isolated Docker Compose stack (`COMPOSE_PROJECT_NAME=openrails552`, alternate host ports) through Postgres, Redis, ClickHouse, migrations, and the real `openrails server`; `/health/live` returned `{"service":"billing","status":"ok"}` and unauthenticated `POST /v1/merchant/admissions` returned the expected 401 from the live route. Fixed two compose-start blockers found during that proof: ClickHouse bootstrap now waits for `analytics_user` before grants, and OpenRails rewrites AuthKit v0.45.0's API-key role migration to backfill legacy `service_tokens` before `role NOT NULL`. Post-fix validation passed: `go test ./internal/migrate`; `docker compose --profile all config --quiet`; clean compose `up -d --build --wait openrails`; `go test -tags=integration ./internal/integrationharness -run 'Test(APIKeyCrossMerchantIsolationHTTP|RemoteApplicationSelfJWTCrossMerchantIsolationHTTP|StandaloneMerchantAdmitAccepts(DelegatedJWT|UserSession)ByPermissionHTTP|DelegatedAdminCrossMerchantIsolationHTTP|DelegatedSelfTokenSubjectIsolationHTTP|CoreDoesNotMountPlatformAdminRoutesHTTP|StandaloneMerchantCatalog(RoutesHTTP|ApplyOptionsOverHTTP|PublishHTTP)|StandaloneMerchantPaymentProviderConfigHTTP|StandaloneNoDefaultMerchantResolvesRequestScopedMerchant)' -count=1 -v`; `go test -tags=integration ./tests -run 'Test(ServiceAdmit_HTTP_EndToEnd|OrgTreasurySpendDelegationsHTTPFullReplacement)' -count=1 -v`; Tensorhub `GOWORK=/tmp/openrails-tensorhub-552.go.work go test -tags=integration ./internal/billing/openrailsclient -run 'TestPlatformTierLadderEnforcement|TestBudgetPolicySyncEnforcement|TestEmbeddedModeBootsEngine|TestEmbeddedSelfSurfaceHostIdentity' -count=1 -v`.

HARD CUT — no backwards compatibility, no data migration, no aliases. `/v1/service/*` and `/v1/self/*` are deleted, not aliased. All consumers (Doujins, Tensorhub, Cozy Art) are first-party and bump in lockstep.

`/v1/service/*` describes the credential type, not the resource boundary. Merchant-owned operations move under `/v1/merchant/*` and accept any credential that resolves to the owning org with the needed permission: regular logged-in user, delegated JWT, self-service/browser JWT with org authority, or API key.

## Metadata

- Category: auth
- Status: completed
- Passes: true

## Decisions

- There is no OpenRails "platform org".
- Do not use AuthKit org membership or org roles to authorize OpenRails platform routes.
- Do not model platform admins as merchant admins on a special merchant.
- OpenRails owns the permission names and route gates; AuthKit owns the single RBAC model, role assignment, and effective-permission resolution.
- Platform permissions use `platform:<resource>:<action>` and are global to the OpenRails installation.
- Org-scoped OpenRails permissions use the AuthKit org RBAC plane but the permission string names the OpenRails resource, e.g. `merchant:payments:refund` for a merchant route scoped to the merchant-owner org.
- Merchant route authorization is tied to the AuthKit org that owns the OpenRails merchant.
- Route paths should describe the resource boundary, not the auth mechanism: use `/v1/merchant/*`, not `/v1/service/*`, for merchant-owned operations.
- Do not add `/admin` inside merchant routes. `/v1/merchant/*` already means "authorized actor operating inside the authenticated merchant/org"; admin-ness comes from the permission gate, not the path.
- API keys, delegated JWTs, self-service/browser JWTs with org authority, and regular logged-in users should all normalize into the same org/merchant principal check on merchant routes.
- Core merchant lookup APIs should be customer-forward: list a customer's entitlements, check one entitlement, list a customer's product access, check one product access, and read a customer's balance.
- Reverse lookups such as "which customers have entitlement X" are directory/filter APIs, not the common embedded host path.
- Drop `issuer` from merchant entitlement lookup request bodies once customer identity is `(merchant_id, subject)`; merchant/org comes from auth, subject comes from the request.
- Merchant HTTP routes should be classified by deployment need: standalone/remote needs HTTP APIs; embedded hosts should prefer the Go interface into OpenRails and direct DB access through OpenRails services.
- Embedded mode should mount browser-facing/customer/admin/webhook routes as needed, but should not require internal merchant lookup HTTP routes just to let the host call back into OpenRails.
- OpenRails routes should fall into four product buckets in the core product:
  public routes, personal customer self routes, org-customer treasury routes,
  and merchant routes. Platform/operator routes belong to OpenRails SaaS, not
  the core merchant/customer surface.
- `platform:*` must never imply any org-local OpenRails permission inside a merchant/org.
- Org-local OpenRails permissions must never imply any `platform:*` permission.
- Standalone mode uses OpenRails' bundled AuthKit control plane for RBAC.
- Embedded mode uses the host application's AuthKit RBAC/principal mapping; OpenRails still checks the same permission names.

## Child Issues

This epic is the design source of truth; the master checklist under "## Tasks" is decomposed into landable child issues. Each child carries its own detailed `- [ ]` task list and acceptance, and is a hard cut (no aliases).

- [x] #554 — final OpenRails permission catalog (public / personal-self / org-treasury / merchant; `merchant:*` naming). DONE 2026-06-21.
- [x] #555 — `/v1/service` → `/v1/merchant` rename + one merchant/org principal resolver; drop `issuer`; customer-forward vs directory split. DONE 2026-06-21.
- [x] #556 — embedded route-set presets and honest route-group split (replace `IncludeUser` / `IncludeAdmin` / `IncludeWebhooks` booleans; split checkout/customer/merchant admin/merchant settings/merchant API). DONE 2026-06-22.
- [x] #557 — customer self `/v1/me/*` + org-treasury `/v1/orgs/:org_id/*` (spend-delegations); delete `/v1/self/*`. Role-scope spend-delegation windows meter per-invoker per #563 (not a shared role pool). DONE 2026-06-22.
- [x] #558 — Tensorhub client recut into Admission/PolicySync/AdminFunding Go interfaces + settings/policy split; batch-native admission. DONE 2026-06-22; Tensorhub consumer hard-cut and integration validation completed.
- [x] #559 — merchant payment-provider config API (replace flat secret-name CRUD; atomic validate-then-store). DONE 2026-06-21: moved to `agents/completed.md`; integration coverage passes.
- [x] #560 — merchant catalog publish/drift over HTTP (HTTP form of `push-merchant-catalog`; plan-only default). DONE 2026-06-21: moved to `agents/completed.md`; integration coverage passes.
- [x] #561 — merchant customer-support + payments/subscriptions admin surface (resource-named, grant-ledger audited). DONE 2026-06-22.
- [x] #562 — delete dead platform-org wiring from core (empty-slug mount switch + `PermPlatformSuperadmin` alias).

**Build order (critical path):** #554 (permission catalog) and #555 (principal resolver + `/v1/merchant` base) land first. Everything that mounts under `/v1/merchant/*` — #557, #558, #559, #560, #561 — depends on #555's resolver and must not start before it lands. #556 (route-sets) and #562 (dead-wiring cleanup) are independent and can land any time.

## Permission Catalog

Use #554 as the core OpenRails permission catalog. This issue should not keep a
second rough list. OpenRails core currently plans only:

- org-customer treasury permissions for `/v1/orgs/:org_id/spend-delegations`.
- `merchant:*` permissions for actual `/v1/merchant/*` routes, scoped to the
  AuthKit org that owns the merchant.

OpenRails SaaS tracks platform/operator permissions separately.

## Current Smells To Remove

- #562 removed `controlPlane.PlatformOrgSlug()` as a mount switch for `/v1/platform/*`.
- #562 removed `PermPlatformSuperadmin` as a single fake route gate in core.
- #562 removed route comments that implied platform administration is tied to an AuthKit org.
- #562 removed test setup that granted platform route access by creating or referencing an org-like authority.
- Any route-specific merchant permission that still uses the misleading `org:*` prefix. Merchant-resource permissions should be `merchant:*` and scoped by the AuthKit org grant context.
- `/v1/service/*` as the primary path for merchant-owned APIs.
- `POST /v1/service/customers/by-external-subject/entitlements`: badly named, carries obsolete `issuer`, and mixes standalone remote identity lookup with the simpler merchant/customer model.
- Auth middleware split by credential type instead of normalizing credentials into one org/merchant principal.

## Proposed Merchant Lookup API

Core HTTP surface:

```text
GET  /v1/merchant/customers/:customer_id/entitlements
POST /v1/merchant/customers/entitlements:batch
GET  /v1/merchant/customers/:customer_id/entitlements/:name
GET  /v1/merchant/customers/:customer_id/products
GET  /v1/merchant/customers/:customer_id/products/:product_id/access
GET  /v1/merchant/customers/:customer_id/balance?currency=USD
```

Batch entitlements request after dropping issuer:

```json
{
  "subjects": ["user-uuid-1", "user-uuid-2"],
  "at": "2026-06-20T12:00:00Z"
}
```

Response remains keyed by subject, with unknown subjects returning `[]`.

Directory/filter API, if still needed:

```text
GET /v1/merchant/entitlements/:name/customers
```

Go library surface should stay as close as possible to HTTP:

```go
ListEntitlements(ctx, customerID)
HasEntitlement(ctx, customerID, name)
ListEntitlementsBatch(ctx, []customerID)
ListProductAccess(ctx, customerID)
HasProductAccess(ctx, customerID, productID)
GetBalance(ctx, customerID, currency)
ListCustomersWithEntitlement(ctx, name, page) // directory/filter only
```

## Route Mounting By Deployment Mode

Standalone/remote mode should mount merchant HTTP routes because the host calls OpenRails over the network:

```text
GET  /v1/merchant/customers/:customer_id/entitlements
POST /v1/merchant/customers/entitlements:batch
GET  /v1/merchant/customers/:customer_id/entitlements/:name
GET  /v1/merchant/customers/:customer_id/products
GET  /v1/merchant/customers/:customer_id/products/:product_id/access
GET  /v1/merchant/customers/:customer_id/balance?currency=USD
GET  /v1/merchant/entitlements/:name/customers
```

Embedded mode should prefer the Go interface for those same operations:

```go
ListEntitlements(...)
HasEntitlement(...)
ListEntitlementsBatch(...)
ListProductAccess(...)
HasProductAccess(...)
GetBalance(...)
ListCustomersWithEntitlement(...)
```

Embedded mode still mounts HTTP for browser/user flows, delegated merchant-admin UI flows, and webhooks. It should not need HTTP routes for host-internal lookups when the host can call OpenRails in-process.

## Embedded Route Group API

Host applications should not have to infer route groups from booleans like
`IncludeUser`, `IncludeAdmin`, and `IncludeWebhooks`. Expose boring named route
sets and presets instead:

```go
type RouteSet string

const (
	RouteSetCheckout         RouteSet = "checkout"          // buyer-facing products/prices/config + checkout/pay flows
	RouteSetCustomer         RouteSet = "customer"          // /me/* browser self-service
	RouteSetMerchantAdmin    RouteSet = "merchant_admin"    // human admin customer/support/payment/subscription operations
	RouteSetMerchantSettings RouteSet = "merchant_settings" // provider secrets, catalog pushes, merchant config/settings
	RouteSetMerchantAPI      RouteSet = "merchant_api"      // machine-to-machine API; embedded opt-in only
	RouteSetWebhooks         RouteSet = "webhooks"          // processor callbacks
)

var EmbeddedDefaultRouteSets = []RouteSet{
	RouteSetCheckout,
	RouteSetCustomer,
	RouteSetMerchantAdmin,
	RouteSetWebhooks,
}

var StandaloneDefaultRouteSets = []RouteSet{
	RouteSetCheckout,
	RouteSetCustomer,
	RouteSetMerchantAdmin,
	RouteSetMerchantSettings,
	RouteSetMerchantAPI,
	RouteSetWebhooks,
}
```

Deployment rule: standalone mounts every route set. Embedded mounts the normal
browser/admin/webhook route sets and skips `RouteSetMerchantSettings` plus
`RouteSetMerchantAPI` by default. Hosts may opt into `merchant_settings` when
they intentionally want HTTP-accessible provider/catalog/config management, and
may opt into `merchant_api` when they intentionally want HTTP loopback parity or
remote-compatible machine API access. Do not keep `public_catalog` as a separate
route set: product/price/config discovery is part of the buyer-facing checkout
surface.

## Customer Self-Service Route Recut

Customer routes are browser/delegated self-service for the authenticated
customer/payer. They do not need RBAC permissions for ordinary own-account
operations; the auth subject is the customer.

Core customer self routes:

```text
GET    /v1/me/balance?currency=USD
GET    /v1/me/transactions?currency=USD&limit=&offset=
GET    /v1/me/settings
PUT    /v1/me/settings
GET    /v1/me/payment-methods
POST   /v1/me/payment-methods
DELETE /v1/me/payment-methods/:id
POST   /v1/me/checkout
GET    /v1/me/subscriptions
GET    /v1/me/subscriptions/:id
POST   /v1/me/subscriptions/:id/cancel
GET    /v1/me/payments
GET    /v1/me/invoices
GET    /v1/me/invoices/:id
GET    /v1/me/usage?currency=USD&from=&to=
```

Org-customer delegated spend sharing:

```text
GET    /v1/orgs/:org_id/spend-delegations
PUT    /v1/orgs/:org_id/spend-delegations
```

`spend-delegations` is the self-service shape for "I am acting as this org
customer, and I want selected users/roles to spend this org's balance, subject
to budget windows." It is org-customer policy only: personal/individual customer
balances cannot be delegated. `:org_id` is the payer/customer org whose balance
is being shared. The caller must have AuthKit authority in that org and hold the
org permission that allows reading or changing spend delegation. It is not a
merchant-admin support override. The request should be a full replacement document
with boring scopes. This is a view over the same delegated-spend limit records
OpenRails admission enforces; do not create a second delegation system:

```json
{
  "currency": "USD",
  "delegations": [
    {
      "scope": "invoker_tier",
      "scope_id": "tier_1",
      "windows": [
        {"key": "5h", "window_seconds": 18000, "amount": 5000000}
      ]
    },
    {
      "scope": "role",
      "scope_id": "role-uuid",
      "windows": [
        {"key": "1w", "window_seconds": 604800, "amount": 35000000}
      ]
    }
  ]
}
```

Scope semantics — every delegated scope meters PER INVOKER (see #563); the scope
is only the SELECTOR for which invokers a window applies to, never a shared pool:

- `invoker`: a meter for one named delegated invoker.
- `invoker_tier`: a per-invoker meter applied to each delegated user at the
  selected tier.
- `role`: a per-invoker meter applied to each delegated user carrying the role
  UUID; each holder gets an INDEPENDENT window (NOT a pool shared by the role).
- `amount` is the currency-native integer amount. For USD, it is micro-dollars.
- Only the `payer` scope (the org's own balance velocity) is aggregate. No
  per-user custom override until a real UI/workflow needs it.

For embedded Tensorhub/Cozy Art, the host can keep using its Go/policy path:
Cozy Art authors role/tier budgets, Tensorhub checks AuthKit `billing:spend`
and passes payer/invoker/role/tier to OpenRails admission. These `/v1/me`
routes are only needed when OpenRails itself owns the customer-facing UI for
sharing a balance.

## Tensorhub Merchant API Recut

Tensorhub is the only real consumer of the broad `/v1/service/*` credit/admit
surface. Its production path does not need a generic service-account grab bag.
It needs three boring OpenRails client surfaces:

```go
type AdmissionClient interface {
	Admit(ctx, batchReq)
	Capture(ctx, requestID, capturedAmount, usage)
	Release(ctx, requestID)
	ReportWastedSpend(ctx, report)
	GetTier(ctx, customerID, currency)
}

type PolicySyncClient interface {
	SetTierSchedule(ctx, currency, schedule)
	SetTierSpendLimits(ctx, currency, tier, limits)
	SetDelegatedInvokerWastedSpendLimits(ctx, windows)
}

type AdminFundingClient interface {
	DepositCredits(ctx, request)
	SetCreditLimit(ctx, customerID, currency, limit)
	UsageRollup(ctx, customerID, currency, from, to, groupBy)
	ResourceRevenueDaily(ctx, resourceID, currency, from, to)
}
```

Tensorhub owns request pricing, endpoint/resource identity, endpoint access,
capacity/scheduler decisions, local platform-abuse event display, and the UI
meaning of its tiers/roles. OpenRails owns the money ledger, balances,
holds/captures/releases, arrears credit-limit enforcement, spend counters,
wasted-spend counters, tier graduation from paid spend, usage rollups from
captured ledger events, payments, subscriptions, and invoicing.

Remote/standalone Tensorhub should only need this merchant HTTP surface:

```text
POST /v1/merchant/admissions                    # always batch-shaped; one item is still an array
POST /v1/merchant/admissions/:request_id/capture
POST /v1/merchant/admissions/:request_id/release
POST /v1/merchant/wasted-spend
GET  /v1/merchant/settings
PUT  /v1/merchant/settings

POST /v1/merchant/customers/:customer_id/balance-adjustments
PUT  /v1/merchant/customers/:customer_id/credit-limit
POST /v1/merchant/customers/:customer_id/usage/rollup
POST /v1/merchant/usage/resource-revenue
```

Routes to cut from the Tensorhub-required surface unless another downstream
proves a real need:

```text
GET  /v1/service/budget
GET  /v1/service/merchant-configuration
GET  /v1/service/abuse-usage
GET  /v1/service/credit-limit
GET  /v1/service/credits/balance
POST /v1/service/credits/withdraw
GET  /v1/service/credits/transactions/lookup
PUT  /v1/service/credits/account-settings
GET  /v1/service/credits/account-settings
GET  /v1/service/credits/transactions
```

Reasoning:

- Tensorhub's hot path is `Admit` before work, `Capture` on success, `Release`
  on failure/cancel, and `ReportWastedSpend` for rejected or failed provider
  spend. Admit is always batch-shaped; a single request is just a one-item batch.
  Direct `withdraw` bypasses that lifecycle.
- Tensorhub reads local abuse/platform event tables for its abuse UI; it does
  not need OpenRails' `abuse-usage` endpoint.
- Tensorhub's previous admin proxy for OpenRails account settings and
  transaction history was already removed; customers/orgs should use the
  OpenRails customer/merchant surfaces directly.
- OpenRails invoicing remains internal to OpenRails. Tensorhub should not need
  routes to crank invoices, charge arrears, or inspect processor internals.
- Embedded Tensorhub should use the same Go interfaces directly and should not
  mount these merchant HTTP routes unless it wants remote parity.

## Merchant Admin Frontend Surface

There are two different admin surfaces and they should not blur together:

- Platform admin: cross-merchant OpenRails operations such as creating,
  disabling, exporting, or deleting merchants.
- Merchant admin: actions by an admin of exactly one merchant/org, usually from
  a host frontend or OpenRails standalone merchant dashboard.

Merchant admins should not manage `owner_org_id`, platform status, platform
exports, or hard deletes. Those stay under `platform:*`.

### Merchant Route Contracts

Keep merchant settings boring: display/support metadata, checkout/webhook URLs,
and merchant-owned billing policy. Payment-provider routing and credentials live
under `/v1/merchant/payment-providers/*`. Do not put platform lifecycle fields
here.

Settings and payment providers:

| Route | Purpose | Request | Response |
|---|---|---|---|
| `GET /v1/merchant/payment-providers` | List configured payment providers. | `?provider=&environment=&status=` | `{data:[payment_provider]}` |
| `GET /v1/merchant/payment-providers/:provider` | Read one payment provider's status/config. | optional `?environment=live` | `{payment_provider}` |
| `PUT /v1/merchant/payment-providers/:provider` | Create/update one provider config as a block. Supplied credentials are validated before storage. | `{environment,enabled,account_id?,public_config?,credentials?}` | `{payment_provider}` |
| `DELETE /v1/merchant/payment-providers/:provider` | Disable/remove one payment provider from future use. | optional `{environment,reason}` | `{payment_provider}` |

`payment_provider` responses expose metadata and configured-field status, never
secret plaintext:

```json
{
  "id": "uuid",
  "provider_type": "stripe",
  "environment": "live",
  "account_id": "acct_...",
  "role": "primary",
  "status": "enabled",
  "public_config": {
    "publishable_key": "pk_..."
  },
  "credentials": {
    "secret_key": {"configured": true, "updated_at": "..."},
    "webhook_signing_secret": {"configured": true, "updated_at": "..."}
  }
}
```

Current merchant credential/config fields by provider:

- Stripe: `secret_key`, `webhook_signing_secret`,
  `webhook_signing_secret_thin`, and optional public `publishable_key`.
- NMI/Mobius: `production_key`, `tokenization_key`, `tokenization_url`, and
  `webhook_signing_secret`. `tokenization_key`/URL are browser-facing config,
  but still belong on the provider account.
- CCBill: `account_config` for now, until it is split into typed fields.
- Solana: `private_key` exists but is OpenRails/platform-owned, not
  merchant-admin writable.

Credential updates are atomic: validate first, store only if validation passes.
There is no separate `/validate` route in the primary API. Direct
`/v1/merchant/secrets/*name` CRUD should be retired or compatibility-only.
The storage layer may still store values under provider-account-scoped secret
names like `provider_accounts/{provider}/{environment}/{account_id}/{key}`.

Catalog:

| Route | Purpose | Request | Response |
|---|---|---|---|
| `GET /v1/merchant/catalog/products` | List products for admin UI. | query filters/page | `{data:[product],page}` |
| `POST /v1/merchant/catalog/products` | Create product. | product fields | `{product}` |
| `GET /v1/merchant/catalog/products/:id` | Read product. | none | `{product}` |
| `PATCH /v1/merchant/catalog/products/:id` | Update product fields. | partial product fields | `{product}` |
| `POST /v1/merchant/catalog/products/:id/activate` | Make product sellable/visible. | optional `{reason}` | `{product}` |
| `POST /v1/merchant/catalog/products/:id/deactivate` | Stop new sales without deleting history. | optional `{reason}` | `{product}` |
| `GET /v1/merchant/catalog/prices` | List prices. | query filters/page | `{data:[price],page}` |
| `POST /v1/merchant/catalog/prices` | Create price. | price fields | `{price}` |
| `GET /v1/merchant/catalog/prices/:id` | Read price. | none | `{price}` |
| `PATCH /v1/merchant/catalog/prices/:id` | Update price fields. | partial price fields | `{price}` |
| `POST /v1/merchant/catalog/prices/:id/activate` | Make price available for new purchases. | optional `{reason}` | `{price}` |
| `POST /v1/merchant/catalog/prices/:id/deactivate` | Retire price for new purchases. | optional `{reason}` | `{price}` |
| `POST /v1/merchant/catalog/publish` | Plan/apply the catalog-as-code manifest, mirroring `openrails push-merchant-catalog`. | `{catalog:{version,default_providers?,tier_groups},insert?,overwrite?,prune?,plan_only?}` | `{plan,result?,extras?}` |
| `GET /v1/merchant/catalog/drift` | List provider/catalog drift findings, including provider-side orphans. | `?provider=&kind=&resource_type=&limit=&offset=` | `{data:[finding],page}` |
| `POST /v1/merchant/catalog/drift/refresh` | Run drift detection now. | optional `{provider}` | `{summary}` |

Product/price writes should enqueue provider sync automatically where a provider
mirror is needed. Do not require a merchant admin UI to call per-object
`reconcile` routes after saving. Keep drift reads as ops visibility; provider
orphans are just `GET /v1/merchant/catalog/drift?kind=orphan`, not a separate
route. If manual repair is needed later, add one boring bulk repair endpoint
rather than per-product/per-price buttons.

`POST /v1/merchant/catalog/publish` is the HTTP form of the existing
`openrails push-merchant-catalog` CLI. The route is merchant-scoped from auth, so
the body should contain one catalog entry, not the CLI file's multi-merchant
`catalogs[]` wrapper. The `catalog` object is the same desired-state shape used
inside `config/catalog.example.yaml`: `default_providers`, `tier_groups`,
products, prices, and `provider_links`. Default behavior should be plan-only;
mutation requires explicit `insert`, `overwrite`, or `prune`, matching the CLI.

Customer support:

| Route | Purpose | Request | Response |
|---|---|---|---|
| `GET /v1/merchant/customers` | Search/list merchant customers. | query filters/page | `{data:[customer],page}` |
| `GET /v1/merchant/customers/:customer_id` | Customer support profile/overview, including trust tier/status and summaries. | optional `?currency=USD` | `{customer,balance_summary,trust_tier,status,active_entitlements}` |
| `GET /v1/merchant/customers/:customer_id/balance` | Per-currency balance. | `?currency=USD` | `{currency,balance}` |
| `GET /v1/merchant/customers/:customer_id/transactions` | Customer ledger history. | `?currency=&limit=&offset=` | `{data:[transaction],page}` |
| `GET /v1/merchant/customers/:customer_id/entitlements` | List active/history entitlement grants. | query filters/page | `{data:[entitlement],page}` |
| `GET /v1/merchant/customers/:customer_id/entitlements/:name` | Check one entitlement. | optional `?at=` | `{active,entitlement?}` |
| `POST /v1/merchant/customers/:customer_id/entitlements` | Manual entitlement grant. | `{entitlement,starts_at?,ends_at?,reason}` | `{grant,entitlements}` |
| `DELETE /v1/merchant/customers/:customer_id/entitlements/:grant_id` | Revoke manual grant. | optional `{reason,refund?}` | `{grant,revoked:true}` |
| `GET /v1/merchant/customers/:customer_id/products` | List owned/product access. | query filters/page | `{data:[product_access],page}` |
| `GET /v1/merchant/customers/:customer_id/products/:product_id/access` | Check one product. | none | `{has_access,access?}` |
| `POST /v1/merchant/customers/:customer_id/product-access` | Manual product ownership grant. | `{product_id,starts_at?,ends_at?,reason}` | `{grant,product_access}` |
| `DELETE /v1/merchant/customers/:customer_id/product-access/:grant_id` | Revoke manual product grant. | optional `{reason}` | `{grant,revoked:true}` |
| `GET /v1/merchant/customers/:customer_id/payment-methods` | List saved payment-method metadata for support (brand/last4/expiry/status only); no detail route because there is no extra merchant-visible sensitive detail. | query filters/page | `{data:[payment_method],page}` |
| `POST /v1/merchant/customers/:customer_id/balance-adjustments` | Append-only support adjustment to the customer's prepaid balance; for comp credits, corrections, or migrations, not normal purchases. | `{currency,amount,reason,idempotency_key?}` | `{transaction}` |
| `PUT /v1/merchant/customers/:customer_id/credit-limit` | Set platform/merchant-approved arrears exposure for this customer. | `{currency,limit,reason}` | `{currency,limit}` |

Merchant-wide payments and subscriptions:

| Route | Purpose | Request | Response |
|---|---|---|---|
| `GET /v1/merchant/payments` | Search merchant payments. | query filters/page | `{data:[payment],page}` |
| `GET /v1/merchant/payments/:id` | Read payment detail. | none | `{payment}` |
| `GET /v1/merchant/customers/:customer_id/payments` | Customer payment history, same payment resource filtered to one customer. | query filters/page | `{data:[payment],page}` |
| `POST /v1/merchant/customers/:customer_id/payments/off-channel` | Record manual/off-channel payment for an existing price and run the normal purchase side effects. | `{price_id,transaction_id,amount?,currency?,purchased_at?,discount_code?,discount_reason?,discount_metadata?}` | `{payment_id,entitlements,delayed_start,eligibility}` |
| `POST /v1/merchant/payments/:id/refunds` | Refund payment through provider/ledger. | `{amount?,reason,revoke_access?}` | `{refund,payment}` |
| `GET /v1/merchant/subscriptions` | Search merchant subscriptions. | query filters/page | `{data:[subscription],page}` |
| `GET /v1/merchant/subscriptions/:id` | Read subscription detail. | none | `{subscription}` |
| `POST /v1/merchant/subscriptions/:id/cancel` | Stop future rebilling. | `{reason?,revoke_access?,effective_at?}` | `{subscription}` |
| `PUT /v1/merchant/subscriptions/:id/payment-method` | Reassign subscription to another existing saved payment method for the same merchant/customer. | `{payment_method_id}` | `{subscription}` |
| `GET /v1/merchant/customers/:customer_id/subscriptions` | Customer subscriptions, current and historical, same subscription resource filtered to one customer. | `?status=&limit=&offset=` | `{data:[subscription],page}` |

Usage/admission and policy:

| Route | Purpose | Request | Response |
|---|---|---|---|
| `POST /v1/merchant/admissions` | Batch authorize work and create holds. | `{items:[{payer,invoker?,resource?,currency,estimated_amount,idempotency_key?}]}` | `{items:[{request_id,admitted,reason?,hold?}]}` |
| `POST /v1/merchant/admissions/:request_id/capture` | Capture an admitted hold after work succeeds. | `{amount,usage?}` | `{transaction}` |
| `POST /v1/merchant/admissions/:request_id/release` | Release an admitted hold after failure/cancel. | optional `{reason}` | `{released:true}` |
| `POST /v1/merchant/wasted-spend` | Report provider spend that produced no billable result. | `{payer,invoker?,currency,amount,reason,resource?}` | `{recorded:true}` |
| `POST /v1/merchant/usage/rollup` | Usage/spend rollup for analytics/support. | `{customer_id?,currency,from,to,group_by}` | `{data:[row]}` |
| `POST /v1/merchant/usage/resource-revenue` | Revenue rollup by resource. | `{currency,from,to,resource?}` | `{data:[row]}` |
| `GET /v1/merchant/settings` | Read merchant-owned settings, including admission policy. | none | `{settings}` |
| `PUT /v1/merchant/settings` | Update merchant-owned settings as one document. | `{display?,checkout?,admission_policy?}` | `{settings}` |

Policy split:

- `settings.admission_policy.tier_schedule` is merchant-wide. Tensorhub uses it
  to declare the cumulative paid-spend ladder once; OpenRails auto-graduates each
  payer.
- `settings.admission_policy.tier_spend_limits` is merchant-wide default policy
  for a resolved payer tier: in-flight/held spend caps, single-charge caps, and
  payer wasted-spend windows.
- `settings.admission_policy.delegated_invoker_wasted_spend_limits` is
  merchant-wide. OpenRails can provide defaults, but multi-merchant deployments
  need a per-merchant override.
- Delegated spend authority is host/org policy, not a merchant-admin override.
  Tensorhub-style org delegation should stay in the host app and reach
  OpenRails through the embedded Go API or a future host-internal sync path only
  if standalone deployment needs it.
- If OpenRails owns the customer-facing UI for this, expose it as
  `/v1/orgs/:org_id/spend-delegations`, because the route must explicitly name
  the payer org/customer whose spend-sharing policy is changing. Do not allow
  individual/personal customer balances to be delegated.
- OpenRails should still trivially accommodate Tensorhub org-delegated spend:
  admission already has the right generic shape — payer/customer id, invoker,
  invoker type, tier, role UUIDs, estimated amount, request id, and resource.
  Tensorhub decides whether the invoker may spend the org's balance through
  AuthKit + Tensorhub policy; OpenRails meters the resulting windows and money.
- Merchant-wide invoker safety policy lives in settings, but payer authorization
  still lives on the payer/customer. Do not add a merchant-level "invoker may
  spend anyone's balance" grant.

Admission policy shape:

```json
{
  "admission_policy": {
    "currency": "USD",
    "tier_schedule": [
      {"tier": "free", "min_cumulative_paid_amount": 0},
      {"tier": "pro", "min_cumulative_paid_amount": 100000000}
    ],
    "tier_spend_limits": [
      {
        "tier": "free",
        "windows": [],
        "wasted_spend_windows": []
      }
    ],
    "delegated_invoker_wasted_spend_limits": []
  }
}
```

Ops reads:

| Route | Purpose | Request | Response |
|---|---|---|---|
| `GET /v1/merchant/repair-alerts` | Ledger/provider repair alerts. | query filters/page | `{data:[alert],page}` |

Routes to skip until a real caller needs them:

```text
POST /v1/merchant/customers/entitlements:batch
GET  /v1/merchant/entitlements/:name/customers
```

The first can come back as `POST /v1/merchant/entitlements:batch` if JWT
enrichment over HTTP needs it. The second is a directory/filter query, not a
core customer support workflow.

Rules:

- Customer rows should still be created naturally by checkout/usage/auth flows;
  admin routes may upsert by subject only when recording an off-channel payment
  or manual grant.
- Manual entitlements and product access should be recorded through the grant
  ledger with `source_type=admin`, explicit `starts_at`, optional `ends_at`,
  reason, and acting admin for audit.
- Off-channel payments are for payments tied to an existing catalog price. They
  use the same purchase registration path as checkout/webhooks so entitlements,
  product access, and idempotency stay consistent. Arbitrary free access uses
  the manual entitlement/product-access grant routes instead.
- Customer subscriptions should include current and historical subscriptions plus
  lifecycle dates the admin UI needs: started, current period start/end, cancel
  time, next retry/renewal where available, status, price/product, and processor
  references.
- Customer profile reads include the customer's auto-maintained trust/spend tier;
  do not keep a separate `GET /v1/merchant/customers/:customer_id/tier` route.
  Customer balance checks use `GET /v1/merchant/customers/:customer_id/balance`.
  `balance-adjustments` is append-only support/migration/correction credit,
  not a purchase path. `credit-limit` is explicit arrears exposure, not a
  billing-mode toggle. Tier is read-only and auto-maintained from spend/trust
  rules; do not add a merchant-admin tier override unless a real support
  workflow proves it is needed.
- Usage rollups are reporting/analytics, not core customer support CRUD, so keep
  them under `/v1/merchant/usage/*`.
- Refunds should not automatically revoke access. Revoking access is a separate
  admin decision unless the refund workflow explicitly asks for `revoke_access`.
- Cancel subscription means "do not rebill"; whether existing access is revoked
  immediately or ends at period end must be an explicit request flag.
- Deleting a payment method is support cleanup only; it must be scoped to the
  same merchant/customer and must not delete historical payment records.
- Payment-provider credentials are managed as payment-provider fields, not as a
  flat secret-name CRUD API. Admin UI can list configured status, update supplied
  fields, and delete/disable accounts; it never reads plaintext. Supplied
  credentials are validated immediately and are not stored if validation fails.

## Tasks

- [ ] Define/register only the OpenRails core permissions from #554 during bootstrap/config sync.
- [ ] Rely on AuthKit's single RBAC model to reject invalid permission/scope combinations: non-`platform:` permissions in platform roles and `platform:` permissions in org roles.
- [x] Delete core `/v1/platform/*` mounting instead of replacing it with another core platform gate; OpenRails SaaS owns platform operator routes when needed.
- [x] Delete core `/v1/admin/merchants*` / cross-merchant admin route wiring instead of keeping a fake superadmin surface.
- [x] Remove `PermPlatformSuperadmin`; do not replace it in core.
- [x] Ensure delegated/browser merchant admin tokens cannot reach any removed platform/cross-merchant surface.
- [x] Ensure any future SaaS platform principal cannot satisfy org-local merchant gates without explicit org-scoped authority for that merchant/org.
- [x] Rename merchant-local permission constants/routes/docs to the chosen org-local OpenRails permission names where needed. DONE under #554/#561.
- [x] Ensure standalone mode checks the bundled AuthKit control plane for org-local OpenRails merchant permissions. DONE under #564/#561.
- [x] Ensure embedded mode checks the same permission names from the host/AuthKit principal mapping. DONE under #556/#557/#561.
- [x] Move merchant-owned `/v1/service/*` routes to `/v1/merchant/*`; hard cut the old paths, no compatibility aliases. DONE under #555 / OpenRails v0.48.0; remaining auth unification is tracked in #564.
- [x] Make `/v1/merchant/*` routes accept every supported credential type that resolves to the owning org/merchant and required permission: logged-in user, delegated JWT, self-service/browser JWT with org authority, and API key. DONE under #564: `merchantActionPermissionMW` now gates the former service routes too; integration proves delegated JWT and user-session `/v1/merchant/admit` access by permission.
- [x] Replace route-level "service credential required" assumptions with a shared principal resolver for merchant routes. DONE under #564: `serviceCredentialRequiredMW` / `servicePermissionMW` removed; service handlers keep API-key customer-resource checks only when the caller is actually a service credential.
- [x] Remove nested `/admin` naming from merchant-owned routes; use resource paths such as `/v1/merchant/subscriptions/:id`, with permissions deciding who may call them. DONE under #561: the delegated billing-admin Gin bundle and tests moved to `/v1/merchant/*`.
- [x] Normalize personal customer self-service routes under `/v1/me/*` and add org-customer-owned `GET/PUT /v1/orgs/:org_id/spend-delegations` for org balance sharing; personal customer balances must not be delegable. DONE under #557.
- [x] Replace `POST /v1/service/customers/by-external-subject/entitlements` with `POST /v1/merchant/customers/entitlements:batch`; remove `issuer` from the request once customer identity is `(merchant_id, subject)`. DONE under #555 / OpenRails v0.48.0.
- [x] Split customer-forward lookups from directory/filter reverse lookups in both HTTP and Go APIs. DONE under #555 / OpenRails v0.48.0; no reverse lookup route is in the primary merchant surface.
- [x] Keep the Go library API and HTTP API names/shapes aligned so embedded and standalone hosts use the same concepts. DONE under #558; Tensorhub consumer hard-cut validated.
- [x] Separate merchant route registration by deployment mode: standalone mounts merchant HTTP lookup APIs; embedded exposes the same operations primarily through the Go interface and avoids unnecessary host-internal HTTP routes. DONE under #556/#558.
- [x] Replace/augment embedded `IncludeUser`, `IncludeAdmin`, `IncludeWebhooks` booleans with explicit named route sets or presets so hosts can see what should be mounted. DONE under #556.
- [x] Make embedded defaults exclude merchant host-internal HTTP lookup routes; provide an opt-in route set for HTTP parity. DONE under #556.
- [x] Update Doujins, Tensorhub, and Cozy Art in lockstep for the hard cut; expected common needs are account balance, check/list entitlements for JWT/auth, check/list product access, and Tensorhub `Admit`. DONE: Tensorhub and Cozy Art are on OpenRails v0.49.0; stale old SDK/route references are gone from Tensorhub's active Go code.
- [x] Recut Tensorhub's OpenRails dependency into explicit admission, policy-sync, and admin-funding/reporting Go interfaces; keep HTTP only for standalone/remote mode. DONE under #558; Tensorhub uses the Go client seam directly.
- [x] Make admission batch-native: one `POST /v1/merchant/admissions` endpoint and one Go method that both accept an item array; no separate single-admit route. DONE under #558.
- [x] Replace Tensorhub's broad `/v1/service/*` expectations with the smaller `/v1/merchant/*` route set above. DONE under #558; Tensorhub consumer adoption validated.
- [x] Remove or compatibility-gate Tensorhub-unused service routes: `budget`, merchant configuration read, `abuse-usage`, credit-limit read, direct credit withdraw, transaction lookup, account settings, and generic credit transactions. DONE as a hard cut in OpenRails under #558.
- [x] Keep OpenRails invoicing, payment processor state, and arrears charging internal to OpenRails; Tensorhub should only configure limits/policies and consume ledger/admission results. DONE on the OpenRails surface under #558.
- [x] Fold the existing delegated `/v1/admin/*` merchant-admin routes into the resource-named `/v1/merchant/*` surface; hard cut the old admin paths. DONE under #561.
- [x] Keep platform merchant provisioning routes separate from merchant self-admin routes: platform can create/delete/export/disable merchants; merchant admins can only manage their own merchant settings, payment providers, catalog, and customers. DONE under #561/#562.
- [x] Replace direct merchant secret-key CRUD with payment-provider configuration routes; store individual credentials as write-only fields under the provider account internally. DONE under #559; direct `/v1/merchant/secrets/*` and legacy delegated `/v1/admin/secrets/*` routes are hard-cut.
- [x] Cut generic `/v1/merchant/configuration`; use `/v1/merchant/settings` for merchant-owned settings and `/v1/merchant/payment-providers/*` for provider configuration. DONE 2026-06-21: removed stale `/merchant-configuration` mounts from the router `/v1/merchant` surface and delegated `/v1/admin` gin surface; route tests assert 404.
- [x] Implement `/v1/merchant/payment-providers` list/read/update/delete using provider names in the path and `{environment}` in query/body; keep `provider_accounts` as internal storage only. DONE under #559.
- [x] Make payment-provider update atomic: validate supplied credentials against the provider first, then store config/credentials only on success. DONE under #559; integration proves invalid NMI config is not persisted.
- [x] Return payment-provider credential status as redacted field metadata (`configured`, `updated_at`, `last_validated_at` if available), never plaintext. DONE under #559.
- [x] Support test/live provider environments explicitly; enforce one active config per `{merchant, provider, environment}` until multiple active accounts are actually needed. DONE under #559.
- [x] Expose catalog-as-code publish/apply over HTTP with the same plan/apply engine as `openrails push-merchant-catalog`; HTTP is single-merchant because the merchant comes from auth. DONE under #560.
- [x] Add `POST /v1/merchant/catalog/publish` that accepts the inner single-merchant catalog manifest shape from `config/catalog.example.yaml`, not the CLI `catalogs[]` wrapper. DONE under #560.
- [x] Keep catalog publish plan-only by default; require explicit `insert`, `overwrite`, or `prune` for mutation, matching `openrails push-merchant-catalog`. DONE under #560.
- [x] Remove product/price per-object reconcile routes from the planned primary surface; product/price writes and catalog publish should enqueue safe provider sync automatically. DONE under #560; primary surface has no per-object reconcile routes.
- [x] Fold catalog orphan listing into `GET /v1/merchant/catalog/drift?kind=orphan`; do not keep a separate `/orphans` route. DONE under #560; no `/orphans` aliases remain on the primary surface.
- [x] Add or normalize customer-management routes for merchant admins: customer settings/profile read, saved payment-method metadata read-only, manual entitlement grant/revoke, product-access grant/revoke, off-channel payment record, payment refund, subscription payment-method reassignment, and subscription cancel. DONE under #561.
- [x] Keep product access write routes named `/product-access`, not `/products`, because the written resource is the customer's access grant, not a catalog product. DONE under #561.
- [x] Make off-channel payment recording require `price_id` and `transaction_id`, then route through the existing normal purchase registration path for entitlements/product access/idempotency. DONE under #561.
- [x] Do not add merchant routes to create/update/delete saved payment methods; merchant admins may only read saved payment-method metadata and may reassign a subscription to an existing method belonging to the same customer/merchant. DONE under #561.
- [x] Ensure customer subscription list returns current and historical subscriptions with lifecycle dates, status, product/price, processor refs, renewal/retry data where available, and pagination/status filtering. DONE under #561.
- [x] Move usage rollup out of customer CRUD into `/v1/merchant/usage/rollup` with optional `customer_id`. DONE under #561/#558.
- [x] Keep `balance-adjustments` and `credit-limit` as explicit money admin writes, not balance reads; customer balance stays `GET /v1/merchant/customers/:customer_id/balance`. DONE under #561.
- [x] Make manual grants go through the grant ledger with acting-admin audit fields; do not write raw entitlement/product-access rows from HTTP handlers. DONE under #561.
- [x] Make refund and subscription-cancel workflows explicit about access revocation instead of silently coupling money reversal to entitlement revocation. DONE under #561.
- [x] Replace separate merchant policy routes with `GET/PUT /v1/merchant/settings`, containing merchant-owned admission policy as one document. DONE under #558.
- [x] Rename/update Go library policy methods to prefer one merchant settings call, e.g. `GetMerchantSettings` and `SetMerchantSettings`; do not add customer delegated-spend methods unless a real standalone host sync path requires them. DONE under #558.
- [x] Keep broad merchant-level "invoker can spend" grants out of the model; invoker/role spend authority must be attached to the payer/customer whose balance is being spent. DONE under #557/#558.
- [x] Preserve OpenRails admission support for Tensorhub org-delegated spend budgets with generic fields: payer/customer id, invoker, invoker type, tier, role UUIDs, estimated amount, request id, and resource. DONE under #558 and validated by Tensorhub integration.
- [x] Do not expose Tensorhub org delegated-spend budgets as OpenRails merchant-admin routes; org-customer-owned sharing belongs under `/v1/orgs/:org_id/spend-delegations`, while embedded Tensorhub/Cozy Art can keep their host-owned policy sync path. DONE under #557/#558 and validated by Tensorhub keeping `platform_budget_sync.go` out of merchant settings.
- [x] Keep `ResourceRevenueDaily` / `/v1/merchant/usage/resource-revenue` as reporting-only endpoint analytics, not admission settlement. DONE under #558; Tensorhub keeps this behind its reporting client seam.
- [x] Cut planned `GET /v1/merchant/manual-rebill-attempts`; rebill attempts are dunning/provider-intent events, so surface failures needing attention through `repair-alerts` and defer aggregate dashboard counts/history drill-downs to the future admin dashboard work. DONE 2026-06-21: removed the old delegated `/v1/admin/manual-rebill-attempts` mount; route test asserts 404.
- [x] Add integration coverage proving core no longer mounts platform/cross-merchant admin routes and merchant permissions cannot act as platform authority. DONE under #562/#564.
- [x] Add integration coverage for payment-provider credential validation failure proving invalid credentials are not persisted. DONE under #559: `TestStandaloneMerchantPaymentProviderConfigHTTP`.
- [x] Add integration coverage for `POST /v1/merchant/catalog/publish` plan-only and mutating modes against the same catalog engine as the CLI. DONE under #560: `TestStandaloneMerchantCatalogPublishHTTP`.
- [x] Add integration coverage for remote merchant admission batch shape, capture, release, and wasted-spend against live HTTP server + DB/Redis stack. DONE under #558.
- [x] Add integration coverage for customer manual grants, off-channel payment registration, refund with explicit revoke flag behavior, and subscription cancel with explicit access behavior. DONE under #561.
- [x] Add integration coverage for settings/policy split: Tensorhub-style merchant settings install tier schedule, tier spend limits, and delegated-invoker wasted-spend defaults without exposing customer delegated-spend overrides as merchant-admin routes. DONE under #558 and Tensorhub #495.
- [x] Update docs and comments to use "platform RBAC" and "org-local merchant RBAC", not "platform org".

## Acceptance

- No OpenRails route requires or references a "platform org".
- Core has no platform admin routes; OpenRails SaaS owns any future platform operator surface.
- Merchant admin routes are protected by OpenRails-defined AuthKit org-local permissions.
- Merchant-owned routes are exposed under `/v1/merchant/*`; `/v1/service/*` is deleted (hard cut, no alias).
- The same merchant permission gate works for regular users, delegated JWTs, self-service/browser JWTs with org authority, and API keys.
- Batch entitlement lookup no longer accepts or requires `issuer`; merchant/org is resolved from auth.
- Customer-forward lookup APIs are simple and mirrored between HTTP and Go.
- Standalone/remote merchant lookup routes are available over HTTP; embedded hosts can use the Go API without routing through HTTP.
- Tensorhub's hot path uses only admission/capture/release/wasted-spend/tier reads, with policy sync and admin funding/reporting kept separate.
- Tensorhub/Cozy Art org-delegated spend budgets can be enforced by OpenRails admission without adding merchant-admin delegated-spend override routes; if OpenRails owns the UI, it is org-customer self-service via `/v1/orgs/:org_id/spend-delegations`.
- Admission has one batch-shaped API; a one-item admission request uses the same route and response shape.
- Routes removed from the Tensorhub-required surface either have no downstream caller or are replaced by the OpenRails Go interface/customer/merchant surface.
- Merchant admin routes are resource-named under `/v1/merchant/*`, not hidden under credential-shaped `/v1/admin/*` or `/v1/service/*` paths.
- Merchant admins cannot create, delete, export, disable, or reassign merchants; those stay platform-only.
- Merchant customer-management actions are audited with the acting admin and scoped to the authenticated merchant/org.
- Merchant provider credentials are configured through payment-provider routes with redacted field status; direct secret-name routes are deleted (hard cut).
- Catalog publish/apply is available through both CLI and HTTP, using one shared engine and the same plan-only-by-default semantics.
- A platform role cannot contain org-local permissions, bare `*`, or negated permissions.
- A merchant role/token cannot grant `platform:*`.
- Integration tests prove the platform and merchant permission planes are disjoint.

---

# #555: service-to-merchant-route-rename-and-unified-principal

**Completed:** yes
**Status:** COMPLETED 2026-06-21. Child of #552. DONE + shipped in **OpenRails v0.48.0**: `/v1/service`→`/v1/merchant` hard cut + gin-duplicate deletion, `issuer` drop (`POST /customers/entitlements:batch`), customer-forward vs directory split, SDK (`remote.go`) move; consumer bump landed (Tensorhub @ v0.48.0). Final #555 follow-up completed 2026-06-21: public `openrails.Client` now mirrors customer-forward lookups with `ListEntitlements`, `HasEntitlement`, `ListActiveEntitlements` (batch), `ListProductAccess`, `HasProductAccess`, and existing balance APIs over the live `/v1/merchant` HTTP surface. Validation: `go test ./...`; `go test -tags=integration ./internal/integrationharness -run TestStandaloneMerchantCustomerLookupClientHTTP -count=1 -v`; `git diff --check`. The FULL uniform-auth model — all credential types on ALL `/v1/merchant` routes + live-DB-subset perms + retire the `#259` `IsDelegatedPermission` allowlist — is carved out to **#564**. The buyer-hat/seller-hat namespace-separation guard is deferred to **#557**, which builds the `/v1/orgs/:org_id/*` treasury surface it needs.

Delete `/v1/service/*` and mount merchant-owned operations under resource-named `/v1/merchant/*`. One shared principal resolver normalizes every credential type into an `(org, merchant, permissions)` principal so the same permission gate serves all callers. Customer identity is `(merchant_id, subject)`, so `issuer` is dropped. See parent #552 "Proposed Merchant Lookup API" and "Route Mounting By Deployment Mode"; permissions come from #554.

## Tasks

- [~] Add a shared merchant-principal resolver that maps logged-in user / delegated JWT / browser-org JWT / API key to `(org, merchant, permissions)`. SUPERSEDED: the first cut was a gin `MerchantPrincipalRequired` (commit f4a70655), but that was the WRONG abstraction (the live `/v1/merchant` is router-based, not gin) and is now DELETED (commit 9677a9dc). The canonical unified resolver is the router `merchantActionPermissionMW` (`internal/http/routes/routes.go`), which normalizes service-cred + delegated + user-session on the ACTION surface. The FULL uniform model (all credential types on ALL routes incl. the machine routes, + live-DB-subset perms) is tracked in **#564**.
- [~] DEFERRED to #557 (needs the treasury surface that #557 builds): keep the seller hat and buyer hat on disjoint namespaces. Since org↔merchant is 1:1 (#527), one org id is both a **merchant-owner** (`merchant:*`, merchant routes) and a **paying customer** over its own balance (`customer:*`, treasury routes). The route's required permission IS the context: gate merchant routes on `merchant:*`, treasury routes on `customer:*`, never a bare `org:*`. Disjoint namespaces (owner-owns-resources grants them as separate globs, neither covering the other) mean neither token satisfies the other's gate — no special mechanism, just the permission check. Regression test proving both directions 403 lives in #557.
- [x] Delete the surviving #553 compatibility alias as part of this cut: removed the `/v1/service/tier` route alias (only `/v1/merchant/trust-tier` remains). The matching `tier` Go-client field removal stays coordinated with #558/Tensorhub.
- [x] Remove the credential-type-split auth middleware; gate `/v1/merchant/*` on the resolved principal. DONE under #564: the router `merchantActionPermissionMW` now covers both action and former service routes (`/admit`, `/credits/*`, catalog, payment providers), accepts service credentials, delegated JWTs, and user sessions by permission, and the #259 `IsDelegatedPermission` browser-safe allowlist is gone. AuthKit bounds remote-app-signed permission claims to stored authority and rejects over-claims.
- [x] Move every merchant-owned `/v1/service/*` route to `/v1/merchant/*`; delete the old paths (hard cut, no alias). DONE: the canonical surface is the router-based `routes.RegisterServiceRoutes` mounted at `/v1/merchant` (standalone via `registerMerchantActionRoutesAt`, embedded via `RouteSetMerchantAPI`); the gin `ginroutes.RegisterServiceRoutes` + `routes_service.go` + `ServiceRoutePrefix` are deleted; SDK (`remote.go`) + all integration tests moved to `/v1/merchant`. Validated green: unit (72 pkgs), integrationharness, tests/ service+route-set suites, embed boundary.
- [x] Implement the customer-forward lookup API over HTTP: list/check entitlements, batch entitlements, list/check product access, get balance. DONE: entitlements batch route, customer entitlement list, product-access list/check route with `?product_id=`, and credit-account balance route are live under `/v1/merchant`.
- [x] Mirror those as Go library methods (`ListEntitlements`, `HasEntitlement`, `ListActiveEntitlements` batch, `ListProductAccess`, `HasProductAccess`, existing `Balance`/`GetCreditAccount`) with HTTP-aligned names/shapes. DONE 2026-06-21.
- [x] Replace `POST /v1/service/customers/by-external-subject/entitlements` with `POST /v1/merchant/customers/entitlements:batch`; drop `issuer` (merchant/org from auth, subjects from body). DONE under v0.48.0; the new SDK continues to call only `/v1/merchant/customers/entitlements:batch`.
- [x] Split customer-forward lookups from directory/filter reverse lookups (`GET /v1/merchant/entitlements/:name/customers`) in both HTTP and Go. DONE: forward methods are subject/customer-facing; reverse lookup remains `ListCustomersWithEntitlement`.
- [~] Update Doujins / Tensorhub / Cozy Art call sites to the new paths/principal; bump in lockstep. ASSESSED 2026-06-21: all three use the OpenRails Go SDK (`openrails.NewRemote`), NOT raw `/v1/service` paths — the `/v1/service`->`/v1/merchant` move is internal to `remote.go`, so no path edits in consumers. The only breaking SDK signature is `Client.ListActiveEntitlements` (dropped `issuer`). **Tensorhub** calls Admit/Capture/Release/GetTier/Deposit/... (all unchanged) and does NOT call `ListActiveEntitlements` -> ZERO code changes, a clean `go.mod` bump only. **Doujins/Cozy-Art** must drop the `issuer` arg IFF they call `ListActiveEntitlements`. RELEASED 2026-06-21: tagged + pushed **OpenRails v0.48.0** (on authkit v0.44.0). **Tensorhub** bumped to v0.48.0 and VALIDATED green (build + openrailsclient/api tests; its test mock servers moved `/v1/service`->`/v1/merchant`; no production code change) — staged in the tensorhub tree, not pushed (its remote diverged + carries unrelated WIP). Doujins/Cozy-Art `go.mod` bumps remain (mechanical) when those repos are next touched.
- [x] Tests: unknown subjects return `[]`; HTTP and Go return matching customer-forward shapes. DONE 2026-06-21: `TestStandaloneMerchantCustomerLookupClientHTTP` exercises live standalone HTTP + real Postgres/Redis for batch/single/check entitlements, product-access list/check, and balance. Credential-type parity is owned by #564.

## Acceptance

- `/v1/service/*` is gone; merchant-owned ops live under `/v1/merchant/*`.
- Uniform credential parity is owned by #564; #555's route rename/customer-lookup scope is complete.
- `issuer` is no longer accepted or required.
- Customer-forward lookups are mirrored between HTTP and Go.

---

# #556: embedded-route-set-presets

**Completed:** yes
**Status:** COMPLETED 2026-06-22: route-set presets now use the deployment names `checkout`, `customer`, `merchant_admin`, `merchant_settings`, `merchant_api`, and `webhooks`. `public_catalog` is gone. `merchant_admin` mounts merchant support/admin routes; `merchant_settings` separately mounts catalog/payment-provider management; `merchant_api` remains machine-to-machine. Embedded defaults mount `checkout`, `customer`, `merchant_admin`, and `webhooks`, and exclude both `merchant_settings` and `merchant_api`. Standalone mounts every route set. The browser `/me/*` route-table recut remains owned by #557; the existing embedded self-service handler is still `pkg/embedded/gin.SelfHandler`.

Replace the embedded `IncludeUser` / `IncludeAdmin` / `IncludeWebhooks` booleans with named `RouteSet` values + presets so hosts can see exactly what mounts. Embedded defaults exclude host-internal machine HTTP routes (the host calls the Go service directly); standalone includes them. See parent #552 "Embedded Route Group API".

Host contract: a normal embedded host mounts the zero-value/default handler and
gets checkout, customer self-service, merchant admin, and webhooks. It opts into
`merchant_settings` only when it wants provider/catalog/config management
available over HTTP, and into `merchant_api` only when it intentionally wants
machine-to-machine HTTP/API-key compatibility.

## Tasks

- [x] Define a `RouteSet` type and replace the old boolean handler options.
- [x] Replace the current canonical sets with: `checkout`, `customer`, `merchant_admin`, `merchant_settings`, `merchant_api`, and `webhooks`.
- [x] Collapse old `public_catalog` into `checkout`; `checkout` owns products/prices/config discovery plus checkout/pay flows.
- [x] Rename self-service route-set grouping to `customer`; the full `/me/*` route recut stays in #557.
- [x] Add `merchant_admin` for human admin customer/support/payment/subscription operations.
- [x] Add `merchant_settings` for HTTP-accessible provider secrets, catalog pushes, and merchant config/settings.
- [x] Keep `merchant_settings` out of embedded defaults so hosts like Doujins can manage provider/catalog/settings through CLI or internal calls instead of exposing them over HTTP.
- [x] Keep `merchant_api` machine-to-machine only; embedded default excludes it, standalone mounts it.
- [x] Do not fold `/me/*` into #556; #557 owns the neutral customer self-service route recut. Existing embedded self-service remains available via `pkg/embedded/gin.SelfHandler`.
- [x] Define `EmbeddedDefaultRouteSets` as `checkout`, `customer`, `merchant_admin`, and `webhooks`.
- [x] Define `StandaloneDefaultRouteSets` as embedded default plus `merchant_settings` and `merchant_api`.
- [x] Mount route groups by RouteSet; let hosts opt into `RouteSetMerchantSettings` for HTTP settings/catalog/provider management and `RouteSetMerchantAPI` for HTTP loopback parity.
- [x] Update public docs/examples to show the host-facing rule: zero-value embedded options for the normal host; append `RouteSetMerchantSettings` only for explicit HTTP settings/catalog/provider management; append `RouteSetMerchantAPI` only for explicit machine HTTP compatibility.
- [x] Migrate embedded hosts (Cozy Art, Tensorhub) off the boolean flags (hard cut); delete the booleans.
- [x] Tests: embedded default mounts checkout/customer/merchant-admin/webhooks; embedded default does not mount merchant settings or machine API; standalone mounts both; opt-in adds each route set.
- [x] Integration test through a real `httptest.NewServer` HTTP client/server proving embedded route-set defaults and opt-in behavior. PASS 2026-06-22: `go test -tags=integration ./tests -run TestHTTPHandlerOptions_RouteSetPresetsOverHTTPServer -count=1 -v`.

## Acceptance

- Hosts declare route sets, not booleans.
- Route-set names map to product surfaces, not implementation leftovers.
- Host apps can choose correctly from names alone: normal embedded default, explicit `merchant_settings` opt-in, and explicit `merchant_api` opt-in.
- Embedded default exposes real OpenRails checkout/merchant-admin/webhook routes directly, with no host proxy routes needed; `/me/*` customer self-service remains the #557 route-table recut and is currently available through `pkg/embedded/gin.SelfHandler`.
- Standalone mounts every route set; embedded default skips HTTP settings/config/catalog management and host-internal machine HTTP routes.
- `public_catalog` is gone as a standalone route set; buyer catalog discovery lives under `checkout`.
- Provider secrets, catalog pushes, and merchant config/settings live under `merchant_settings`, not `merchant_admin`.

---

# #557: customer-self-and-org-treasury-route-recut

**Completed:** yes
**Status:** COMPLETED 2026-06-22. Child of #552. `/v1/me/*` is the only personal customer self-service surface; `/v1/self/*` has no current source/docs references. `/v1/orgs/:org_id/spend-delegations` is live in standalone and embedded Gin self handler, backed by the existing payer-owned invoker spend-limit store, with the payer forced to the resolved org/merchant id. The route gates on `customer:spend-delegations:read|update` only; merchant permissions do not satisfy treasury routes and customer permissions do not satisfy merchant routes. Doujins was updated in lockstep from `/v1/self/*` to `/v1/me/*` and routes `/v1/orgs/*` to the embedded self/org handler. Validation: `go test ./internal/http/handlers ./internal/http/routes/ginroutes ./internal/http/routes ./pkg/embedded/gin`; `go test -tags=integration ./tests -run TestOrgTreasurySpendDelegationsHTTPFullReplacement -count=1 -v`; Doujins `go test ./internal/billing/openrailsembed`; Doujins `go build ./...`; Doujins frontend `pnpm build`; Doujins real docker-compose stack targeted Playwright proof: `E2E_API_BASE_URL=http://localhost:25252 pnpm -C frontend exec playwright test e2e/openrails/billing-live.spec.ts -g "billing route recut is live on the compose stack" --project=chromium` after starting the stack with `task e2e-test -- openrails-contract` and isolated compose ports; `git diff --check` in both repos.

Normalize personal customer self routes under `/v1/me/*` and delete `/v1/self/*`. Add the org-customer treasury bucket at `/v1/orgs/:org_id/*`, including `GET/PUT /v1/orgs/:org_id/spend-delegations` for sharing an org balance under budget windows, gated by `customer:spend-delegations:*` (#554). Personal/individual balances are never delegable. See parent #552 "Customer Self-Service Route Recut".

## Tasks

- [x] Move all personal customer self routes to `/v1/me/*`; delete `/v1/self/*` (hard cut, no alias). `/v1/me/*` needs no OpenRails permission beyond authenticated self-subject. Verified current Gin/server route registration uses `/v1/me/*`; remaining `/v1/self/*` references are stale docs/tracker notes only.
- [x] Add the `/v1/orgs/:org_id/*` treasury bucket, AuthKit-org scoped. DONE 2026-06-22: standalone mounts it under `/v1/orgs`; embedded `embgin.SelfHandler` mounts the same real route table under `/billing/v1/orgs`.
- [x] Keep the buyer hat and seller hat on separate namespaces. The same org id owns its merchant (`merchant:*`, seller) AND is a customer over its own balance (`customer:*`, buyer) — org↔merchant is 1:1 (#527). Gate the treasury route on `customer:spend-delegations:*` ONLY; never on `merchant:*` or a bare `org:*` wildcard. Because the namespaces are disjoint (and owner-owns-resources grants `merchant:*` and `customer:*` as separate globs, not one covering the other), a merchant-only token can't reach the treasury and a treasury-only token can't reach merchant routes — by permission, no special mechanism. DONE 2026-06-22: route tests prove both directions 403.
- [x] Implement `GET/PUT /v1/orgs/:org_id/spend-delegations` as a full-replacement document; gate read on `customer:spend-delegations:read`, write on `customer:spend-delegations:update`. DONE 2026-06-22: backed by the existing payer-owned `invoker_spend_limits` store, with payer forced to the org/merchant id.
- [x] Reject spend-delegation requests against personal/individual customer balances. DONE 2026-06-22: the route has no customer-id path, the payer is forced from the resolved org, and `customer_id` in the document or delegation rows is rejected.
- [x] Update host call sites (Doujins) off `/v1/self/*`; bump in lockstep. DONE 2026-06-22: Doujins embedded mount dispatch, payments clients, and E2E direct paths now use `/v1/me/*`; `/v1/orgs/*` is routed to the embedded self/org handler.
- [x] Tests: personal self-access needs no org permission; org-treasury access requires the org permission; personal balances cannot be delegated. DONE 2026-06-22: route tests cover auth/permission boundaries and bad personal `customer_id`; integration test exercises PUT/GET/replace over a real HTTP server + test DB.

## Acceptance

- `/v1/self/*` is gone; personal customer self-service is `/v1/me/*`.
- Org balance sharing is `/v1/orgs/:org_id/spend-delegations`, org-customer-only, not a merchant-admin override.

---

# #558: tensorhub-client-recut-and-policy-split

**Completed:** yes
**Status:** COMPLETED 2026-06-22. Child of #552. OpenRails exposes the hard-cut batch-native merchant admission/settings/usage surface and the public SDK is split into `AdmissionClient`, `PolicySyncClient`, `AdminFundingClient`, and `CustomerLookupClient`. Legacy SDK shims/routes are not kept: no registered `/v1/merchant/admit`, `/admit/batch`, `/credits/holds/*`, `/credits/usage/*`, `/budget`, `/abuse-usage`, credit-limit read, direct withdraw, transaction lookup/list, or account-settings routes remain in Go. Tensorhub is cut to `AdmitBatch`, `SetMerchantSettings`, and `SetOrgSpendDelegations` on released OpenRails `v0.50.0` with no local replace; source grep has no active old OpenRails route/method references. Validation: OpenRails packages and org-treasury integration listed under #552; Tensorhub `go test ./internal/billing/openrailsclient ./internal/api ./internal/orchestrator/...`; Tensorhub `go test ./...`; Tensorhub Docker-backed `go test -tags=integration ./internal/billing/openrailsclient -count=1 -v`.

Replace Tensorhub's broad `/v1/service/*` dependency with three narrow OpenRails Go interfaces (`AdmissionClient`, `PolicySyncClient`, `AdminFundingClient`) plus a minimal `/v1/merchant/*` HTTP surface for standalone/remote. Admission is batch-native. Merchant-wide admission policy lives in `/v1/merchant/settings`; payer authorization stays on the payer. See parent #552 "Tensorhub Merchant API Recut" and the admission/policy split under "Merchant Admin Frontend Surface".

## Tasks

- [x] Define `AdmissionClient` (batch admit / capture / release / wasted-spend / get-trust-tier), `PolicySyncClient` (single merchant settings document), `AdminFundingClient` (deposit, credit-limit, usage rollup, resource-revenue), and `CustomerLookupClient` Go interfaces.
- [x] Make admission batch-native: one `POST /v1/merchant/admissions` + one Go method taking an item array (a single request is a one-item batch); no separate single-admit route. Added `/v1/merchant/admissions/:request_id/capture`, `/v1/merchant/admissions/:request_id/release`, and `POST /v1/merchant/wasted-spend`.
- [x] Move merchant-wide policy (`tier_schedules`, `tier_spend_limits`, `delegated_invoker_wasted_spend_limits`) into `GET/PUT /v1/merchant/settings` as one document; added Go `GetMerchantSettings` / `SetMerchantSettings`.
- [x] Keep invoker/role spend authority attached to the payer; no merchant-level "invoker may spend anyone's balance" grant. Preserved generic admission fields (payer id, invoker, invoker type, trust tier, role UUIDs, estimated amount, request id, resource).
- [x] Delete Tensorhub-unused service routes (hard cut): `budget`, merchant-configuration read, `abuse-usage`, credit-limit read, direct credit `withdraw`, transaction lookup, account-settings read/write, generic credit transactions are no longer registered under `/v1/merchant/*`.
- [x] Remove the surviving #553 route/client compatibility alias from the OpenRails public SDK surface for this cut: no public `Client.Admit`, `CaptureHold`, `ReleaseHold`, `WithdrawCredits`, config setter shim, or old merchant policy route remains. NOTE: request-field `tier` aliases still exist on DTOs until downstream source stops constructing them; the registered route/API surface is hard-cut.
- [x] Keep OpenRails invoicing, processor state, and arrears charging internal; Tensorhub only configures limits/policy and consumes ledger/admission results.
- [x] Embedded Tensorhub uses the Go interfaces directly (no merchant HTTP mount); standalone uses the HTTP surface. DONE: Tensorhub pins OpenRails v0.50.0 with no local replace; Tensorhub source grep has no old service/self route or removed SDK references.
- [x] Tests: hot path admit/capture/release; one-item batch path via conformance; settings install policy without exposing customer delegated-spend as merchant-admin routes. See validation in Status.

## Acceptance

- Tensorhub hot path uses only admit/capture/release/wasted-spend/trust-tier read; policy sync and admin funding/reporting are separate interfaces.
- Admission has one batch-shaped API; a one-item request uses the same route/shape.
- Removed service routes have no remaining first-party caller.

# #561: merchant-customer-support-admin-surface

**Completed:** yes
**Status:** COMPLETED 2026-06-22. Child of #552. The resource-named `/v1/merchant/*` support/admin surface is mounted, permission-gated, and covered by route-permission plus Docker-backed integration tests. The old delegated billing-admin Gin bundle is removed rather than aliased; first-party admin integration tests now call `/v1/merchant/*` with JWT-shaped delegated test credentials. Final hard cut also removed the stale embedded/self `/v1/admin/*` mounts and tests. Validation: `go test ./...`; `go test -tags=integration ./tests -run 'TestAdmin|TestHTTPHandlerOptions_RouteSetPresetsOverHTTPServer' -count=1 -v`; `go test -tags=integration ./internal/integrationharness -run 'TestStandaloneMerchantAdmitAcceptsDelegatedJWTByPermissionHTTP|TestStandaloneMerchantAdmitAcceptsUserSessionByPermissionHTTP|TestDelegatedAdminCrossMerchantIsolationHTTP|TestDelegatedSelfTokenSubjectIsolationHTTP' -count=1 -v`; `rg -n "RegisterAdminRoutes|AdminRoutePrefix|/v1/admin|billing/v1/admin|/billing/v1/admin" internal pkg tests embed -g'*.go'` returns no Go references; `git diff --check`.

Resource-named `/v1/merchant/*` admin surface for one merchant's support staff: customer profile (incl. `trust_tier`), balance/transactions, saved payment-method metadata read-only, entitlement and product-access grant/revoke, off-channel payments, refunds, subscription cancel/payment-method reassignment, merchant-wide payments/subscriptions, usage rollups, balance-adjustments, credit-limit, and repair-alerts. Gated by `merchant:customer-settings:*` / `merchant:payments:*` / `merchant:subscriptions:*` / `merchant:usage:read` / `merchant:repair-alerts:read` (#554). Manual grants go through the grant ledger with acting-admin audit; refund/cancel make access-revocation explicit. Merchant admins cannot touch platform lifecycle or create/update/delete customer saved payment methods. See parent #552 "Merchant Admin Frontend Surface".

## Tasks

- [x] Mount resource-named customer support reads under `/v1/merchant/customers/:customer_id`: profile includes `trust_tier`, current balance sections, entitlements, product access, payments, and saved payment-method metadata.
- [x] Add read-only saved payment-method metadata list: `GET /v1/merchant/customers/:customer_id/payment-methods`; no merchant route creates/updates/deletes customer payment methods.
- [x] Mount manual entitlement grant/revoke + product-access grant/revoke under `/customers/:customer_id/{entitlements,product-access}` using the existing grant-ledger handlers. Keep write routes named `/product-access`, not `/products`.
- [x] Mount off-channel payment at `POST /v1/merchant/customers/:customer_id/payments/off-channel`; it requires `price_id` + `transaction_id` and routes through `CheckoutService.RegisterPurchase`.
- [x] Add explicit `revoke_access` handling to refund and subscription-cancel request bodies; do not silently couple money reversal/cancel to entitlement revocation. Refund revokes one-off entitlements/product access by payment only when requested; admin subscription cancel revokes subscription/grace entitlements only when requested.
- [x] Mount merchant-wide payments/subscriptions search/read and customer-filtered payment history; mount subscription cancel/resume/payment-method reassignment. Reassignment validates the new payment method against the subscription's own customer/merchant.
- [x] Decide whether `balance-adjustments`/`credit-limit` need new resource-named customer routes: no alias for now. Existing `/v1/merchant/credits/{deposit,withdraw}` + `/v1/merchant/credit-limit` remain until a real admin UI caller needs a friendlier route.
- [x] Decide whether usage rollup needs `POST /v1/merchant/usage/rollup`: no alias for now. Existing `/v1/merchant/credits/usage/rollup` remains until a real caller needs it moved.
- [x] Mount `GET /v1/merchant/repair-alerts`; keep `manual-rebill-attempts` cut.
- [x] Tests: route-permission unit coverage for support routes; live Docker-backed standalone coverage for delegated admin profile isolation and self-token denial on `/v1/merchant/customers/:customer_id`.
- [x] Final hard cut: remove the old delegated `/v1/admin/*` mount/tests after first-party tests and clients are moved to `/v1/merchant/*`.

## Acceptance

- Customer-support surface is resource-named under `/v1/merchant/*`; no credential-shaped `/v1/admin/*`.
- Manual grants are grant-ledger audited with the acting admin.
- Refund and cancel access behavior is explicit.
- Merchant admins cannot create/delete/export/disable/reassign merchants.

---

# #562: delete-dead-platform-org-wiring

**Completed:** yes
**Status:** COMPLETED 2026-06-20. Deleted the dead core platform-org/platform-superadmin wiring. Validation: `go test ./...`, `go build ./...`, `go vet ./...`, and `go test -tags=integration ./internal/integrationharness -run TestCoreDoesNotMountPlatformAdminRoutesHTTP -count=1 -v` pass.

The `platform:*` and org planes already exist and are already separate, and platform/operator permissions live in OpenRails SaaS (not core). This removed the vestigial core wiring: a mount switch keyed on an always-empty org slug, and a single coarse gate aliased to one platform permission.

## Tasks

- [x] Delete `internal/http/routes_platform.go` core route registration entirely, including the empty-slug `PlatformOrgSlug() == ""` mount switch.
- [x] Delete core cross-merchant admin route wiring that depends on `PermPlatformSuperadmin` (including `/v1/admin/merchants*` style routes).
- [x] Remove the `PermPlatformSuperadmin` coarse alias in `internal/controlplane/catalog.go` and all references.
- [x] Remove `PlatformOrgSlug()` / `HasPlatformSuperadmin` from the core control-plane authority.
- [x] Update route comments/docs: drop remaining "platform org" language; state the platform/operator surface lives in OpenRails SaaS.
- [x] Build/tests: `go build ./...` + `go vet ./...` clean; no core route is gated by a fake platform org.

## Acceptance

- No dead platform-org wiring remains in core; `/v1/platform/*` and core cross-merchant admin routes are gone.
- The `PermPlatformSuperadmin` coarse alias is gone.
- Docs/comments use "platform RBAC" / "org-local merchant RBAC", not "platform org".

---

# #330: nmi-immediate-subscription-checkout-stuck-pending

**Completed:** no
**Status:** IN_PROGRESS 2026-06-08: immediate NMI checkout activation patch is implemented; package tests and focused mock-provider regression pass; configured-account NMI recurring test has been added and compiles/skips locally because `NMI_SANDBOX_SECURITY_KEY` is not set. Remaining work: run real NMI credential test and repair/replay Paul2 after fixed code is deployed.

Fix the NMI subscription checkout path where an immediately approved recurring checkout is persisted as pending, leaving host apps such as Doujins stuck on the pending-subscription screen even though NMI accepted the transaction.

## Metadata

- Category: bug
- Status: in_progress
- Passes: false

## Live findings from Doujins/Paul2 on 2026-06-08

- OpenRails received the checkout request and `POST /v1/self/checkout` returned HTTP 200.
- NMI accepted the card vault and transaction: payment vault `314825442`, provider transaction `12162933364`, provider subscription `12162933429`.
- Local checkout session `019ea96b-221b-7274-a341-8cdc85cb72d6` was marked `succeeded` with processor `mobius` and amount `2300 usd`.
- Local subscription `019ea96b-223b-75b1-926b-35956d10eba5` stayed `pending`, with no current period start/end timestamps.
- Local payments only had a pending attempt row keyed as `nmi_sub_attempt:sub_cb204b034d2c3ec46be93b0470ff44df`; there was no completed payment row keyed by the real NMI transaction id.
- No billing entitlement rows were created for the user, so Doujins correctly kept showing the pending-subscription state.

## Root cause

`processNMISubscription` called `AddRecurringSubscription` successfully, but `completeNMISubscriptionRegistration` intentionally created a local pending subscription and returned a pending checkout response. The initial NMI response was not used to synchronously activate an immediate subscription, set the current billing period, create a completed payment, or grant entitlements. Webhooks should not be required for the initial happy path when NMI immediately approves the transaction; delayed/future starts can remain pending.

## Desired behavior

For immediate NMI subscription approvals, OpenRails should finish the local checkout atomically from the direct provider response: mark the subscription active, set current period timestamps, record the completed payment against the real provider transaction id, grant entitlements, and return a succeeded checkout response. Delayed-start subscriptions and genuinely asynchronous provider states should remain pending and rely on follow-up provider events/reconciliation.

**Tasks:**
- [x] Capture live-stack evidence for the Paul2 failure: NMI accepted the transaction, checkout succeeded, subscription stayed pending, payment stayed as a pending attempt, and no entitlement was granted.
- [x] Identify root cause in the NMI checkout finalization path.
- [x] Patch immediate NMI subscription finalization to activate the subscription, set period dates, create the completed payment, and grant entitlements without waiting for a webhook.
- [x] Preserve pending behavior for delayed/future-start NMI subscriptions.
- [x] Fix stale integration-test helpers that still query billing tables by `user_id` instead of `tenant_subject_id`.
- [x] Add/validate focused integration coverage proving an immediate NMI subscription checkout grants the premium entitlement synchronously.
- [x] Add an actual NMI test-account integration path so the live provider contract is exercised, guarded by NMI test credentials.
- [ ] Run the actual NMI configured-account recurring-subscription integration with `NMI_SANDBOX_SECURITY_KEY` set.
- [x] Run focused OpenRails checkout/module tests and NMI regression tests.
- [ ] After deployment/restart, repair or replay the affected live pending subscription row for Paul2 if it still exists.

---

# #328: robinhood-coinbase-usdc-funding-sessions

**Completed:** no
**Status:** PARTIAL 2026-06-08: Implemented Solana-only USDC funding session APIs, persistence, config, Coinbase hosted-session adapter with CDP JWT auth, Coinbase Hook0-signed Onramp webhook/status ingestion, Robinhood launch-template handoff, provider eligibility gates, self-service routes, idempotency, structured insufficient-USDC funding context on checkout errors, backend Solana USDC balance verification, focused tests, and DB-backed self-service API tests for create/get, merchant/user isolation, idempotency, and unsupported provider/network rejection. Retained for future provider integration work; current Doujins UX uses manual Robinhood/Coinbase links plus connected-wallet balance checks instead of OpenRails provider sessions. Remaining: real Robinhood partner adapter/status docs and access.

Plan and implement OpenRails-owned USDC funding sessions for host apps that need users to buy or transfer USDC into their own live Solana wallet before completing a Solana wallet checkout.

## Metadata

- Category: feature
- Status: partial
- Passes: false

## Goal

- OpenRails should expose a provider-backed funding-session API for Robinhood and Coinbase only. Host apps such as Doujins can ask OpenRails for a funding URL, send the user to the provider in a new tab or popup, then resume checkout after OpenRails and/or the host app verifies that the user's live Solana wallet has enough USDC.

## Product Behavior

- The user already has or creates a Robinhood/Coinbase account on the provider site.
- OpenRails does not custody funds and does not collect provider KYC; the provider handles account login, payment method, buy/transfer, KYC, and compliance.
- Provider redirect/return means the provider flow ended; it is not proof that the wallet is funded.
- Completion must be based on provider status/webhooks when available plus Solana wallet-balance verification.
- Only offer a provider when it can fund USDC on Solana. Coinbase/Base and all EVM chains are out of scope.

## Scope

- Implement Robinhood and Coinbase integration surfaces only for Solana USDC.
- Do not implement Ramp, Transak, MoonPay, PayPal, Venmo, Base, Ethereum, Polygon, Arbitrum, Optimism, or bridge paths for this issue.
- Keep provider abstraction narrow but extensible enough that more providers could be added later without changing the host-app contract.

**Tasks:**
- DESIGN:
- [x] Define the OpenRails funding-session contract for browser self-service callers: provider preference, wallet address, asset, network, requested amount, checkout_session_id, return_url, and idempotency key.
- [x] Define provider statuses and normalize them into OpenRails statuses such as created, opened, pending_provider, pending_settlement, funded, failed, expired, and cancelled.
- [x] Define Solana-only compatibility rules for USDC funding.
- [x] Decide whether funding amount comes from the checkout-session shortfall, an explicit requested amount, or both with server-side validation. Implemented explicit requested amount with optional checkout_session_id context.
- [x] Decide how provider ranking is configured per merchant: Robinhood preferred, Coinbase fallback. Implemented default provider order with Robinhood first and Coinbase second.
-
- DATA MODEL:
- [x] Add a funding/onramp session table with tenant_id, user_id, checkout_session_id, provider, wallet_address, asset, network, requested_amount, provider_session_id, provider_url, status, return_url, idempotency key, timestamps, and provider metadata.
- [x] Add indexes for tenant/user lookup, checkout_session_id lookup, provider_session_id lookup, and idempotency.
- [x] Store provider secrets/config in OpenRails config, never in host apps.
-
- API:
- [x] Add `POST /v1/self/usdc-funding-sessions` to create a Robinhood/Coinbase funding session for the authenticated browser user.
- [x] Add `GET /v1/self/usdc-funding-sessions/:id` to return normalized funding status and provider URL/status details safe for frontend polling.
- [x] Add `GET /v1/self/usdc-funding-options` to list eligible Robinhood/Coinbase options for wallet, network, asset, amount, and optional checkout_session_id.
- [x] Add provider webhook/status callback endpoints where Coinbase supports them. Implemented signed Coinbase Onramp webhook ingestion on the existing provider webhook route; Robinhood remains blocked on partner docs/access.
- [x] Enforce self-service auth, merchant boundaries, and idempotency on funding-session create/read routes.
-
- PROVIDERS:
- [x] Implement a Coinbase provider adapter that creates a hosted onramp URL/session with destination wallet, network, asset, amount, return URL, and partner/user reference, including short-lived CDP JWT bearer generation from Coinbase secret API keys.
- [x] Implement Coinbase status/webhook handling and map provider lifecycle into OpenRails funding-session status. Coinbase success maps to pending_settlement; only live Solana wallet-balance verification can mark funded.
- [ ] Implement a Robinhood provider adapter after partner docs/access are available, supporting external handoff into Robinhood Connect and funding into the user's live wallet.
- [ ] Implement Robinhood status/webhook handling if exposed by partner API; otherwise rely on return handling plus on-chain wallet-balance verification.
- [x] Add provider availability checks so unsupported network/asset combinations are hidden rather than offered.
-
- WALLET VERIFICATION:
- [x] Reuse existing Solana USDC balance-checking code where possible to verify the funded wallet before marking a session funded for Solana checkout.
- [x] Do not add Base/EVM balance verification for this issue; Solana is the only supported chain.
- [x] Ensure returning from a provider only triggers polling/checking; it must not mark the session funded by itself.
-
- CHECKOUT INTEGRATION:
- [x] Allow a funding session to reference the checkout session that produced an insufficient-USDC state.
- [x] Ensure insufficient-USDC API errors expose enough structured amount/network/wallet context for host apps to create a funding session. Added `error.metadata.usdc_funding` with Solana network, USDC asset, wallet, decimal amount/balance/shortfall, and base-unit values.
- [x] Keep final subscription/payment creation in the existing checkout confirmation path after the wallet is funded.
-
- VERIFY:
- [x] Add unit tests for provider eligibility and network compatibility gates.
- [x] Add API tests for create/get funding session, merchant isolation, idempotency, and unsupported-provider/network rejection.
- [x] Add provider adapter tests with mocked Coinbase responses.
- [x] Add wallet-balance verification tests proving redirect alone is insufficient through status semantics and frontend polling contract.
- [x] Document the host-app integration contract for Doujins in config.example.yaml and the tracker issue.

---

# #111: admin-rate-limiting

**Completed:** no

Add rate limiting to admin endpoints to limit blast radius of compromised JWT

## Metadata

- Category: security
- Status: not_started
- Passes: false

**Tasks:**
- STEPS:
- [ ] PROBLEM: If admin JWT is leaked, attacker could cancel/refund thousands of users before detection
- [ ] Implement per-admin-user rate limiting using Redis (keyed by admin user ID from JWT)
- [ ] Define rate limits for destructive operations:
-     - Cancellations: 5/minute, 10/hour, 50/day
-     - Refunds: 5/minute, 10/hour, 50/day
-     - Entitlement revocations: 5/minute, 10/hour, 50/day
- [ ] Define rate limits for bulk/expensive operations:
-     - Extend operations: 3/minute
-     - Off-channel payments: 10/minute
-     - Admin grants: 10/minute
- [ ] On rate limit exceeded: lock out admin for extended period (e.g., 1 hour) and alert
- [ ] Add alerting/notification when rate limits are approached (e.g., 80% threshold)
- [ ] Create admin rate limit middleware that wraps destructive endpoints
- [ ] Allow super-admin or manual override to unlock rate-limited admin if legitimate
- [ ] Log all rate limit events for security audit
- BENEFIT: Limits blast radius - attacker can only affect ~5-10 users before getting locked out

---

# #126: test-architecture-improvements

**Completed:** no

Tests should use real structs and functions from production code, not invent test-specific abstractions

## Metadata

- Category: testing
- Status: not_started
- Passes: false

## Details

- current_problems: ["SubscriptionOptions helper struct uses different field names than models.Subscription (PeriodStart vs CurrentPeriodStartsAt)","Tests can pass while using wrong field names because helpers translate between them","When a model field is renamed/removed, tests may not break because helper abstracts it away","Developers get confused about which struct to use and what fields are available"]
- example_bad: {"code":"suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{PeriodStart: now})","problem":"SubscriptionOptions.PeriodStart doesn't exist on models.Subscription"}
- example_good: {"code":"sub := &models.Subscription{CurrentPeriodStartsAt: now, ...}; suite.CreateTestSubscription(sub)","benefit":"Uses real model, compiler catches field name errors"}
- philosophy: {"principle":"Tests should verify actual production code behavior, not test-specific wrappers","goal":"When production code changes, tests should break if they're testing the affected behavior","anti_pattern":"Test helper structs that diverge from production models hide API changes and create maintenance burden"}
- recommendations: ["Use models.Subscription directly in tests instead of SubscriptionOptions","Use api.ListResponse[T] for parsing API responses instead of map[string]interface{}","Import and use the actual request/response structs from handlers","If a helper is needed, it should take the real model struct as input, not a parallel struct","Test assertions should use strongly-typed response parsing"]

**Tasks:**
- REFACTORING:
- [ ] Audit all test helper structs (SubscriptionOptions, PaymentMethodOptions, etc.)
- [ ] Replace helper structs with direct model usage where possible
- [ ] Update CreateTestSubscriptionWithOptions to accept *models.Subscription
- [ ] Use json.Unmarshal with actual API response types instead of map[string]interface{}
- [ ] Add linter rule or CI check to discourage map[string]interface{} in tests

---

# #158: ccbill-dunning-grace-entitlements

**Completed:** no

Adjust CCBill subscription entitlement logic to model paid term + retries (dunning) + end-of-term expiration. Use CCBill webhook fields like nextRetryDate/nextRenewalDate to drive subscription status and finite entitlement windows, and cap any grace extensions to avoid unlimited free access.

**Tasks:**
- SPEC:
- [ ] Verify CCBill webhook sequence in sandbox: cancel mid-term -> Expiration at end-of-term (and timing)
- [ ] Decide grace policy during retries: disabled vs enabled; cap strategy (extend-to-nextRetryDate vs fixed extra days)
- [ ] Decide policy for CCBill `Cancellation.source=failedRB`: treat as terminal end (no more retries) vs still paid-through end-of-term
-
- DATA MODEL:
- [x] Ensure we can store current_period_ends_at (from nextRenewalDate) for CCBill subscriptions
- [x] Store next_retry_at (from nextRetryDate) and optional grace_ends_at (policy derived)
- [x] Decide whether to reuse existing `subscriptions.next_retry_at` fields for CCBill (recommended) vs add CCBill-specific columns
-
- WEBHOOK -> STATE MACHINE:
- [x] NewSaleSuccess: set paid-term end; grant entitlements for [now, paid_term_end)
- [x] RenewalSuccess: extend paid-term end; extend entitlement windows
- [x] RenewalFailure: set past_due/dunning; persist next_retry_at from webhook; optionally extend entitlements within grace cap
- [x] Cancellation: mark cancelled but keep access until paid_term_end (do NOT revoke immediately)
- [x] Expiration: mark ended/expired and end/revoke entitlements
-
- CODE CHANGES (expected touch points):
- [x] `internal/services/webhook_ccbill.go`: parse and apply `nextRenewalDate` on `NewSaleSuccess` and `RenewalSuccess` (don’t rely solely on `price.billing_cycle_days`)
- [x] `internal/services/webhook_ccbill.go`: on `RenewalFailure`, parse `nextRetryDate` and update `subscriptions.status=past_due` + `subscriptions.next_retry_at` (avoid calling `SubscriptionLifecycleService.FailMembership`, which schedules NMI-style retries)
- [x] `internal/services/webhook_ccbill.go`: on `Cancellation`, call `CancelMembership` with `RevokeAccess=false` (today it passes true)
- [x] `internal/services/lifecycle_service.go`: update `CreateMembership` entitlement grant behavior for CCBill to create finite windows ending at `current_period_ends_at` (instead of indefinite `end_at=NULL`)
- [x] `internal/services/lifecycle_service.go`: update `RenewMembership` for CCBill to extend entitlements to match the new `current_period_ends_at` (today renewal doesn’t extend entitlements at all unless a downgrade changes entitlement set)
- [x] `internal/db/repo/entitlement.go`: make `IsEntitled` / “active entitlement” queries explicitly exclude `deleted_at` (don’t rely on implicit soft-delete behavior)
- [x] `config/config.go`: add a feature flag / config for CCBill grace behavior (enable/disable + max cap)
-
- ANALYTICS:
- [x] Ensure ClickHouse subscription_events reflect dunning/cancelled/expired transitions for CCBill
-
- TESTS:
- [x] Add integration test: NewSaleSuccess -> RenewalFailure(nextRetryDate) keeps access until paid_term_end (+ optional grace) -> RenewalSuccess extends window
- [x] Add integration test: Cancellation mid-term does NOT revoke immediately; entitlement ends at paid_term_end; verify Expiration handling if CCBill sends it
- [x] Add unit test for `EntitlementRepo.IsEntitled` to ensure deleted rows are excluded (regression guard)

---

# #164: cloudflared-managed-dev-tunnel

**Completed:** no

Make `Config.Cloudflared` an actual supported dev feature: OpenRails can (optionally) run and manage a Cloudflare Tunnel for local/dev, so a full local stack (e.g., multiple host apps + billing) can be accessed from an external device (phone) over HTTPS.

## Metadata

- Category: devx
- Status: planned
- Passes: false

## Goal

- With `cloudflared.tunnel_token` configured, a developer can start OpenRails and get a stable public hostname that routes to the local billing instance, usable from a phone browser/app.

## Non-goals

- Do not require Cloudflared in production.
- Do not make Cloudflared a hard dependency for boot.

## Notes

- This is for local/dev convenience (public ingress), not webhook signature bypass.
- Prefer explicit opt-in (config flag or separate command) so production never spawns subprocesses unexpectedly.

**Tasks:**
- DESIGN:
- [ ] Decide opt-in mechanism: (A) standalone CLI subcommand `openrails cloudflared up` that supervises both OpenRails + cloudflared, or (B) OpenRails spawns cloudflared when `cloudflared.tunnel_token` is set and `env=dev`
- [ ] Decide what constitutes “success”: tunnel established + /health/ready reachable via public hostname
-
- IMPLEMENTATION (if spawning subprocess):
- [ ] Add a small `pkg/cloudflared` supervisor that starts `cloudflared tunnel run` with the configured token and captures stdout/stderr into structured logs
- [ ] Ensure clean shutdown: propagate SIGINT/SIGTERM, kill child process group, avoid zombies
- [ ] Add a readiness check that probes the configured `cloudflared.public_hostname` (or local route) and reports status
-
- CONFIG / DOCS:
- [ ] Clarify config semantics in config.example.yaml (token is secret; hostname is non-secret; tunnel name optional)
- [ ] Document a phone-access workflow in docs (prereqs: cloudflared installed or image available; set APIURL/CORS origins appropriately)
-
- SECURITY / SAFETY:
- [ ] Ensure the service API (port 8060) is not accidentally exposed publicly by default; only expose the public API unless explicitly configured
- [ ] Ensure `api_key` and auth verification are still required for protected routes
-
- VERIFY:
- [ ] Add a lightweight unit test for supervisor command construction (no subprocess exec)
- [ ] Manual dev verification: start stack + tunnel and hit /health/ready from an external device

---

# #288: processor-routing-and-fallback-policy

**Completed:** no

Add deterministic processor selection and fallback policy so checkout can choose the best available rail for a merchant/product/tier/user context before creating a one-processor checkout session.

## Metadata

- Category: feature
- Status: planned
- Passes: false

## Motivation

- Hyperswitch wins on payment orchestration. OpenRails should not chase broad smart-routing, but customers do need basic processor redundancy and product/merchant-aware routing, especially high-risk merchants with Stripe plus NMI/CCBill/Solana.
- Keep checkout sessions one-processor-at-a-time; route before session creation.

## Non-goals

- No ML routing.
- No 200-connector orchestration platform.
- No automatic retry to a second processor after a successful authorization attempt unless the processor contract makes that safe.

**Tasks:**
- DESIGN:
- [ ] Define routing inputs: tenant_id, product_id, price_id, tier_group, amount, currency, billing cycle, user country/state when known, processor availability, processor capability metadata (#291), and explicit client preference.
- [ ] Define routing outputs: selected processor, fallback candidates, reason, and policy version.
- [ ] Decide precedence: explicit price/provider config > merchant policy > product/tier_group policy > global default.
- [ ] Decide failure classes that can trigger fallback before checkout finalization: processor unavailable, unsupported capability, credential missing, sandbox/live mismatch, hard validation failure. Do not fallback after a successful charge.
-
- DATA MODEL / CONFIG:
- [ ] Add routing policy representation in DB or catalog manifest: allowed processors, preferred order, disabled processors, and optional per-tier overrides.
- [ ] Extend catalog-as-code manifest only if needed; prefer using existing provider lists as the first version.
- [ ] Record selected processor and routing reason on checkout_sessions for auditability.
-
- IMPLEMENTATION:
- [ ] Add a ProcessorRouter service used before checkout session creation and legacy checkout flows.
- [ ] Integrate with processor health/capability metadata so unsupported processors are filtered with explicit errors.
- [ ] Add dry-run endpoint/CLI to explain routing for a price/user/merchant without creating a checkout.
- [ ] Ensure idempotency keys bind to the resolved processor/policy so retries do not unexpectedly switch rails.
-
- VERIFY:
- [ ] Unit-test policy precedence and fallback filtering.
- [ ] Integration-test Stripe primary -> NMI fallback when Stripe is disabled/unavailable before charge.
- [ ] Integration-test product constrained to NMI does not route to Stripe even if NMI is configured.

---

# #290: provider-certification-matrix

**Completed:** no

Publish and maintain a provider certification matrix for Stripe, NMI, CCBill, and Solana that records exactly which customer-visible flows are supported, sandbox/devnet tested, live/test-mode tested, and known-limited.

## Metadata

- Category: product
- Status: planned
- Passes: false

## Motivation

- OpenRails' strongest differentiator is deep support for real non-Stripe rails, especially NMI-compatible high-risk gateways. Customers need confidence that the specific flows they care about actually work.
- This should become both documentation and an executable certification harness where practical.

**Tasks:**
- MATRIX DESIGN:
- [ ] Define provider capabilities and certification statuses: not_supported, manual_only, unit_tested, integration_tested, sandbox_certified, live_test_mode_certified, devnet_certified, production_certified.
- [ ] Define flows to track: catalog/product push, price/recurring-plan push, one-time checkout, recurring checkout, vault/tokenization, rebill, cancellation, deferred cancellation, refund, dispute/chargeback, webhook handling, subscription sync/backfill, catalog drift detection.
- [ ] Include processor-specific notes: NMI product/prices are local while recurring prices push as NMI recurring plans; CCBill catalog actions may be manual; Solana recurring requires on-chain readback/devnet certification.
-
- DOCS:
- [ ] Add docs/providers.md or equivalent with the current matrix and exact tested commands.
- [ ] Add NMI-compatible gateway guidance: required security_key, Collect.js/tokenization key if needed, direct/query endpoints, test_mode behavior, and how white-label NMI accounts map to the same interface.
- [ ] Add troubleshooting for common provider failures: bad endpoint, key belongs to different gateway user, sandbox URL not supported, recurring plan query mismatch, webhook signature failure.
-
- EXECUTABLE CERTIFICATION:
- [ ] Add or formalize focused integration tests for NMI sale, vault, recurring plan create/readback, and query API.
- [ ] Add Stripe test-mode catalog command + subscription sync certification steps.
- [ ] Add Solana devnet read-back certification for plan accounts and recurring lifecycle.
- [ ] Add CCBill manual-action verification path so unsupported remote catalog operations surface as pending_manual_actions rather than errors.
-
- PROCESS:
- [ ] Make certification matrix updates part of provider-related PRs.
- [ ] Record last verified date, environment, and command for each provider flow without exposing secrets.

---

# #291: processor-capability-metadata

**Completed:** no

Expose processor capability metadata in code, APIs, catalog planning, checkout validation, and admin/provider status so OpenRails can explain what each rail supports instead of relying on scattered processor-specific conditionals.

## Metadata

- Category: architecture
- Status: planned
- Passes: false

## Motivation

- Stripe, NMI, CCBill, and Solana have different lifecycle semantics. Customers need predictable errors and routing decisions. OpenRails also needs a shared capability source for routing/fallback, catalog-as-code, checkout validation, and the provider certification matrix.

**Tasks:**
- CAPABILITY MODEL:
- [ ] Define ProcessorCapabilities with booleans/enums for recurring, one_time, vault/tokenization, hosted checkout, redirect checkout, direct sale, catalog push, recurring plan push, refund, dispute, cancel immediate, cancel deferred, remote subscription listing, remote dedup check, webhooks, drift enumeration, and manual actions.
- [ ] Add capability details for current processors: stripe, NMI-backed (`mobius`), ccbill, solana.
- [ ] Distinguish processor class capabilities (NMI-backed) from named provider overrides (Mobius).
-
- INTEGRATION POINTS:
- [ ] Use capabilities in checkout validation instead of hard-coded processor switches where practical.
- [ ] Use capabilities in catalog planning/reconciliation to decide provider actions and pending_manual_actions.
- [ ] Use capabilities in routing/fallback policy (#288) so unsupported rails are filtered before checkout.
- [ ] Surface capabilities through admin/provider status endpoints and docs.
-
- ERRORS / UX:
- [ ] Return structured unsupported-capability errors with processor, capability, and suggested alternative when possible.
- [ ] Ensure user-facing errors are clean while admin/debug surfaces retain processor detail.
-
- VERIFY:
- [ ] Unit-test capability metadata for each supported processor.
- [ ] Regression-test known special cases: CCBill manual catalog actions, NMI recurring plan push, Stripe remote dedup check, Solana one-off/recurring distinction.

---

# #320: Add Hyperswitch payment vault support for cloud and self-hosted deployments

**Completed:** no

Add Hyperswitch as an optional OpenRails payment provider and payment-method vault integration, covering both Hyperswitch Cloud and self-hosted Hyperswitch. This is payment vaulting/tokenization, not HashiCorp Vault merchant-secret storage. OpenRails should store only opaque Hyperswitch customer/payment-method identifiers plus non-sensitive metadata, while PAN/card collection stays in Hyperswitch-hosted/client-side tokenization flows or equivalent PCI-scoped Hyperswitch surfaces.

This issue must reconcile with future issue #297 (`deplatforming-resilient-card-vault`): Hyperswitch should not be positioned as the default adult/high-risk deplatforming-resilient vault until its contractual/export/compliance posture is explicitly certified. Hyperswitch Cloud may still be useful for lower-risk deployments or as an optional provider, and self-hosted Hyperswitch may be a break-glass/advanced deployment path if the operator accepts and satisfies the PCI requirements.

This should integrate with the provider capability and routing work in #288, #290, and #291: Hyperswitch can be selected as a processor/vault provider when the merchant/provider config says it is available, and OpenRails can route checkout/setup flows through it without treating it as a generic secret vault.

**Tasks:**
- [ ] Research the current Hyperswitch Cloud and self-hosted API surfaces needed for customers, payment methods, payment/setup intents, saved payment methods, refunds/voids, webhooks, connector routing, and vault/tokenization modes; record any version assumptions in docs.
- [ ] Reconcile the implementation plan with future issue #297: document whether Hyperswitch is an optional provider, a lower-risk deployment choice, or a PCI-heavy break-glass/self-hosted vault path; do not make it the default portable adult/high-risk vault without explicit certification.
- [ ] Define the OpenRails provider config shape for `hyperswitch`: cloud vs self-hosted mode, API base URL, optional vault/base URL split if Hyperswitch requires it, merchant/profile/account identifiers, API key secret reference, webhook secret reference, return/callback URLs, and test/live mode.
- [ ] Store Hyperswitch credentials in the existing merchant secret store path; do not store processor API keys in bootstrap YAML, database rows, logs, or generated frontend config.
- [ ] Extend provider capability metadata (#291) so Hyperswitch can advertise supported flows: payment-method vault/tokenization, one-time checkout, recurring/setup/mandate behavior if supported, refunds, webhooks, processor-side routing, remote payment-method listing, and manual certification status.
- [ ] Add a provider adapter/client abstraction that lets NMI customer-vault IDs and Hyperswitch payment-method IDs fit the same OpenRails payment-method model without hard-coding NMI semantics into checkout/subscription flows.
- [ ] Implement Hyperswitch customer/payment-method setup flow using an opaque token or hosted/client-side Hyperswitch collection result; persist only customer, provider, external customer ID, external payment method ID, brand/last4/expiry metadata, and status.
- [ ] Implement checkout/subscription charge paths that can use a saved Hyperswitch payment method and reconcile the resulting payment/subscription state back into OpenRails records.
- [ ] Implement webhook verification and event handling for successful payment, failed payment, refund/void, payment-method updates/deletes, and any recurring/mandate lifecycle events OpenRails relies on.
- [ ] Add self-hosted operational docs: required Hyperswitch services, base URL/TLS requirements, webhook reachability, secret injection, health checks, and local compose/dev smoke path if practical.
- [ ] Add cloud operational docs: required Hyperswitch Cloud credentials, webhook setup, provider certification checklist entry (#290), and merchant bootstrap/provider-link examples.
- [ ] Add tests with a mocked Hyperswitch server/client for tokenization/setup, saved-method charge, failure mapping, webhook signature verification, idempotency, merchant isolation, and no-sensitive-card-data persistence/logging.
- [ ] Validate with focused Go tests for provider/checkout/vault/webhook code, compile-only full package coverage, `task build`, and an optional live sandbox/self-hosted smoke test when credentials or local Hyperswitch are available.

# #333: admin-e2e-suite-needs-control-plane-wiring

**Completed:** no
**Status:** OPEN 2026-06-10 (Claude): diagnosed during the #332 failure review; not started. All other e2e failures from that review are fixed (see completed #332 + commits 9fe72d4 openrails, 68cb6a1 tensorhub, b72f61e8 cozy-art).

The admin e2e tests (tests/admin_payments_test.go, admin_metrics_test.go, admin_entitlements_source_test.go, admin_offchannel_payments_test.go) fail with 500 {'message':'authorization unavailable'} — the nil-admin-checker fail-closed path. DIAGNOSIS (2026-06-10): pre-existing #312 bit-rot, documented in the suite itself (testcontainer_suite.go Auth config comment: 'Admin-route integration tests therefore require the embedded control plane wired with the test admin granted openrails:admin'). The #312 hard-cut moved admin authority from JWT claims (tests still mint operatorAdminClaims() tokens via test_helpers.go) to the LIVE control-plane permission check (routes_admin.go only sets AdminPermissionChecker when s.controlPlane != nil; the suite never enables Auth.ControlPlane -> checker nil -> admin_neutral.go/ginmw admin gates 500 fail-closed).

WHAT THE FIX NEEDS:
1. suite.Config.Auth.ControlPlane = &config.ControlPlaneConfig{Enabled: true} (ginboot embcp.Attach then builds it; authkit profiles schema is already applied by migrate.RunPostgres).
2. The admin test users must EXIST in authkit profiles.users with ids equal to the JWT subs the suite mints, then be made members + granted the admin role (controlplane Bootstrap/AddMember/AssignRole or authkit core APIs — NOT raw SQL into roles). authkit core.CreateUser(email, username) generates its own id, so either (a) resolve the verifier's user mapping (issuer+sub -> user) and mint tokens for created users' real ids, or (b) add a test-only authkit user-provisioning hook.
3. operatorAdminClaims()/CreateAdminToken in tests/test_helpers.go are then vestigial — admin authority comes from the role grant, not claims.

NOTE: the admin-METRICS tests' 'Table test_analytics.daily_metrics does not exist' failures were a STALE TEST ENV artifact (migratekit records the ClickHouse ledger in postgres; reusing a postgres DB across runs against fresh CH containers skips re-apply). On a fresh env CH applies fine and those tests fail at the same admin-auth gate instead. Squashed CH baseline (cb9200b) is NOT at fault (daily_metrics present; migrations/clickhouse schema_test passes).

**Tasks:**
- [ ] Enable Auth.ControlPlane in testcontainer_suite config and verify embcp.Attach builds it in ginboot.
- [ ] Provision admin test users in authkit profiles.users matching the JWT subs (or mint tokens from created users' ids); AddMember + AssignRole openrails:admin via control plane / authkit core APIs.
- [ ] Re-run tests/admin_*.go on a fresh env; remove vestigial operatorAdminClaims if green.
- [ ] (env hygiene) consider failing loudly or re-validating when the migratekit CH ledger says applied but the CH database lacks the tables (stale-ledger detection).
