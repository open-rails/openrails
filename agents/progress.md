<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 536

---

# #535: Entitlement reverse-query — billing-enrichment FILTER provider for AuthKit's user directory

**Completed:** no

## Metadata

- Category: feature
- Status: not_started
- Passes: false

ACTIVE — the OpenRails half of the cross-repo admin-directory work (AuthKit #91 + doujins #414), pulled in 2026-06-19 because doujins's admin dashboard needs it NOW to delete its raw cross-schema premium-filter SQL. Build alongside AuthKit #91.

**Division of responsibility:** the USER DIRECTORY (list/search/sort/detail by email/username/role/org) belongs to **AuthKit** (it owns identity). OpenRails is NOT a user directory; it owns BILLING state keyed by subject and acts as the ENRICHMENT + FILTER PROVIDER behind AuthKit's directory. The composition happens in the host (or OpenRails' own standalone billing console, #228) via AuthKit's pluggable provider seam — NOT raw cross-schema SQL.

The ENRICH direction already ships in doujins: AuthKit's `AdminListUsers` calls a `BatchEntitlementsProvider`; doujins wires OpenRails in (`ListActiveEntitlements(issuer, userIDs, now)` — one batched in-process call per page), so user-list rows already carry entitlements. This #535 issue adds the still-missing REVERSE direction so AuthKit can FILTER the directory BY entitlement.

**FORWARD ("of THESE users, who's premium?") — ALREADY SHIPPED, no work.** `POST /v1/service/customers/by-external-subject/entitlements` (`ServiceGetExternalSubjectEntitlements`, perm `entitlements:read`, #354). Embedded equivalent: `EntitlementService.ListActiveRecordsByExternalSubjects(issuer, subjects, at)`.

**REVERSE ("find ALL users who have premium") — THE GAP (this issue). DONE + DB-VALIDATED 2026-06-19.**
- [x] `EntitlementService.ListCustomersWithEntitlement(ctx, entitlement, at, afterID uuid.UUID, limit int) ([]uuid.UUID, error)` — customer_ids holding an ACTIVE window (`start_at <= at AND (end_at IS NULL OR end_at > at) AND revoked_at IS NULL AND deleted_at IS NULL`, matching the existing forward queries exactly), ordered by customer_id, keyset-paginated (afterID exclusive). limit clamps to [1, 10000]. sqlc query `ListCustomersWithEntitlement` (+ hand-written gen mirroring sqlc — no live DB for codegen here) + repo method.
- [x] Identity detail: returns customer_id; in UUID-only mode (#364, doujins) customer_id == host user_id. (Federated issuer+subject return-shape left for when a federated host needs it — the embedded/doujins UUID-only case is what #414 consumes now.)
- [x] Standalone parity: `GET /v1/service/entitlements/:entitlement/customers?cursor=&limit=&at=` (perm `entitlements:read`), handler `ServiceGetCustomersWithEntitlement` → `{customers[], next_cursor, has_more}` (keyset). Embedded hosts call the Go method directly; standalone hits the same logic over HTTP.
- [x] Supporting index: migration `017_entitlements_reverse_lookup_index.up.sql` — partial btree `(merchant_id, entitlement, customer_id) WHERE revoked_at IS NULL AND deleted_at IS NULL`.
- [x] DB-integration-test: `tests/entitlement_reverse_lookup_test.go` (real Postgres) — returns only active/non-revoked/non-deleted matches ordered by customer_id, excludes expired/revoked/deleted/other-entitlement, keyset pagination walks the set, empty entitlement rejected. GREEN.
- [ ] Ship: tag + bump so AuthKit's provider-filter (#91) + doujins (#414) can consume. Pending user OK to commit/tag — batched with #91/#414.

**STATUS: code-complete + DB-validated (2026-06-19).** `go build ./...` clean. Only the release remains.

**Architectural principle (capture in docs):** an embedded host consumes OpenRails through its Go SERVICE API / the `BatchEntitlementsProvider` seam, NOT raw SQL against `openrails.*` — raw SQL re-implements the entitlement-active semantics (drift), bypasses RLS, and couples the host to the physical schema (the tenant_id→merchant_id rename that broke the test helpers would have broken a raw-SQL host identically). **doujins violates this TODAY** for `?filter=premium` (`internal/api/admin/users/handler.go` runs a `profiles.users ⋈ openrails.entitlements/customers` join); this issue + AuthKit #91 let doujins delete it.

The OpenRails-hosted admin-console "sophisticated user search" is a SEPARATE concern (#228, future); the user DIRECTORY itself is AuthKit's, never OpenRails'.

---

# #534: Unify bearer-auth trust models behind one Principal + RequirePermission; harden shared-handler reuse

**Status:** planned (not started)

The public API *looks* like ~7 distinct trust tiers, but that count conflates
authentication (how you prove identity) with authorization (what you may do). The
real shape is much smaller; the apparent sprawl is the error-prone part, and it is
what makes the heavy handler-reuse across tiers risky.

## Problem

Reality is **2 non-bearer mechanisms + 1 bearer channel carrying 4 credential profiles**:

- **Non-bearer (irreducible):** public (no credential); webhooks (HMAC signature — a
  payment processor cannot present a bearer).
- **Bearer channel** (`Authorization: Bearer …`), 4 credential profiles:
  - AuthKit user session/bearer — backs `/me` and `/platform` (+`platform:superadmin`).
  - OpenRails API key (currently named service token internally) — `/service`
    (merchant = API-key owner). This is a shared-secret API key, not a signed JWT.
  - Remote-application self JWT — `/service` machine/app credential signed by a
    registered issuer with no `delegated_sub`; internally `ResolveRemoteApplication`
    returns the same `ResolvedServiceToken` shape as API keys today, but the credential
    type is distinct.
  - Delegated JWT — `/self` (`openrails:self:*`) and `/admin` (`openrails:merchant:*`,
    the #528 delegated admin surface; stale `/merchant-admin` wording should be removed).
    This is user-delegated and has `delegated_sub`.
  Standalone is MULTI-MERCHANT: the server leaves `configuredMerchant` zero and resolves the
  merchant PER-CREDENTIAL (API-key owner / delegated issuer). `configuredMerchant` is
  the EMBEDDED single-merchant binding only. (#528 retired the per-user `openrails:admin`
  surface — there is no longer a user-bearer `/admin` operator tier.)

So the "7 tiers" are mostly **authorization granularity** (which is good) layered on
**4 bearer profiles**, where each format grew its own middleware family + per-tier
permission-gate helper, applied inconsistently per route group:
`requiredMW` (+ live admin checker); `ServiceTokenRequired` + `RequireServiceTokenPermission`
(legacy/internal names for the API-key gate);
`DelegatedSelfRequired`/`DelegatedPrincipalRequired` + `RequireDelegatedPermission`.

**The risk (ties to #227 / #528):** the same handlers are reused across tiers — the
admin handlers back operator-admin *and* federated merchant-admin (#528); the user
handlers back session *and* delegated self. Every handler trusts `r.GetUser()` + the
pinned merchant, so cross-merchant isolation rests entirely on **four different
middlewares each pinning the merchant correctly**. One bug in any of them is
cross-merchant exposure. RLS (`MerchantDBConnMW`, #227) is a backstop, not a boundary
test.

## Target Model

One authentication boundary resolves **any bearer** into a common, intentionally small principal:

    Principal{ MerchantID, MerchantSource, CredentialType, Subject, Can(ctx, perm) }

(Vocabulary: in OpenRails the data-scoping entity is the **merchant**
(`pkg/merchant`, `merchant.ID`); the AuthKit/tensorhub equivalent term is **org**. A
merchant is *owned by* an AuthKit org (`OwnerOrgID`) — a separate axis. "tenant"
survives only in legacy comments / generic prose below, never as a type.)

Every route then becomes exactly one of: `public` | `webhook(verifySignature)` |
`RequirePermission("openrails:…")`. The real distinctions (machine vs human vs
delegated; which merchant; which scope) become **data on the Principal**, not separate
middleware stacks. The multi-format resolver already exists in embryo —
`resolveServiceCredential` (`internal/http/routes/routes.go`) already multiplexes
API-key/service-token / remote-app-JWT / service-JWT, and the merchant-catalog gate
already does "API-key OR user." Extend that into the unified bearer boundary.

Do not force webhooks into this model. Processor HMAC/webhook authentication is a
separate non-bearer boundary and should stay outside `Principal`.

### Invariant that makes this safe-or-not

Merchant-binding — the `merchant.ID` that scopes all data access, today pinned per
request via `merchant.WithID` — differs per credential and MUST be preserved verbatim:
- user bearer → merchant = ResolveMerchant (URL/config-pinned merchant)
- API key → merchant = key's owning merchant
- remote-application self JWT → merchant = the registered remote application's owner org
  mapped to its OpenRails merchant
- delegated JWT → merchant = token issuer's merchant

The unified `Principal` carries `MerchantID` + `MerchantSource` (its provenance) +
`CredentialType` (`api_key`, `remote_application`, `delegated_user`, `user_session`).
The resolver sets it per format. Centralizing this is the entire point — **one
place to get merchant-pinning right instead of four.** Webhooks + public stay
separate by design.

### Second invariant: where permissions come from also differs

The gate becomes uniform (`RequirePermission`), and the permission STRINGS are already
one catalog today (the colon-form `controlplane.Perm*` constants). But a principal's
permission SET/evaluator is obtained differently per credential, and the resolver must
preserve that when it populates `Principal.Permissions` or `Principal.Can(ctx, perm)`:
- API key (service token internally) → permissions come from server-side API-key
  resolution; the bearer itself is a shared secret, not a self-verifying JWT.
- Remote-application self JWT → signature/issuer/audience prove the application,
  but authority comes from stored AuthKit/OpenRails remote_application grants/roles,
  not from self-claimed permissions in the JWT.
- Delegated JWT → permissions are carried in the signed token / delegated grant model.
- operator-admin / platform-superadmin → permissions are evaluated LIVE against the
  caller's own org roles at request time (#312 live authority), NOT from a claim.

Do NOT flatten operator-admin/platform into a static token claim — that would silently
drop the live-authority guarantee. Prefer `Principal.Can(ctx, perm)` or an explicit
permission-source field over a bare static `[]string` unless the resolver has already
performed the live authority lookup.

## Phases (smallest / safest first)

### Phase 1 — alias cleanup (cheap, no behavior change)
- Delete the **dead singular** `POST /v1/service/credits/hold/:id/{capture,release}`
  routes (`internal/http/routes/ginroutes/routes.go`). The SDK (`remote.go`) and all
  consumers use only the plural `holds/:id/…`; nothing first-party hits the singular.
- Document the **intentional** `drift/refresh` ≡ `drift/reconcile-all` alias (both →
  `AdminRefreshCatalogDrift`, `registerCatalogActionRoutes` in
  `internal/http/routes/routes.go`) so it does not read as an accident.
- (optional) tiny test asserting any retained alias maps to the same handler + same
  permission gate.

### Phase 2 — cross-merchant isolation test suite (cheap, high value, no prod-code change)
HTTP-boundary tests that pin the reuse invariant BEFORE the refactor. Phase 3 must not
start until these are green:
- delegated admin token for merchant A hitting
  `/v1/admin/users/{B-user}/…` → fail closed (404/forbidden or empty isolated profile,
  depending on the handler contract).
- API key for merchant A → cannot read/mutate merchant B's payer on `/v1/service/*`.
- remote-application self JWT for merchant A → cannot read/mutate merchant B on
  `/v1/service/*` or other machine surfaces.
- self token → can only act on its own `delegated_sub` (no `:user_id` reachable).
- operator-admin in merchant A → cannot touch merchant B.
These become the regression net for Phase 3 and any future refactor.

### Phase 3 — bearer unification (the simplification; reviewed effort)
- Introduce `Principal` + a single `Authenticate(bearer) → Principal` resolver
  (extending `resolveServiceCredential` to the user + delegated formats).
- Replace the per-tier permission helpers with one `RequirePermission(perm)` gate
  calling `Principal.Can(ctx, perm)`.
- Migrate route groups one at a time behind the new gate; keep RLS as backstop.
- Eng-review before starting — security-critical.

## Tasks

### Phase 1 — alias cleanup (cheap, no behavior change)
- [x] Delete singular `POST /v1/service/credits/hold/:id/{capture,release}` (`ginroutes/routes.go`); keep plural `holds/:id`
- [x] Make the `drift/refresh` ≡ `drift/reconcile-all` shared-handler intent explicit in a comment
- [x] `go build ./...` + service-route tests green

### Phase 2 — cross-merchant isolation suite (no prod-code change)
`internal/integrationharness/cross_merchant_isolation_test.go` (//go:build integration).
RLS-real: the server runs under `StartStandaloneRLS` (connects as `openrails_app`,
NOBYPASSRLS), so real per-merchant RLS enforces — not just the in-app predicate.
Fixtures are seeded via the super pool.
- [x] Harness: two merchants A/B + a payer (`ProvisionOwnedMerchant` + `MintServiceToken`)
- [x] API key / service token (A) → `/v1/service/*` cannot read merchant B's payer balance (green, real server+DB)
- [x] remote-application self JWT (A) → `/v1/service/*` cannot read/mutate merchant B;
      prove stored remote_application authority, not self-claimed JWT permissions, is what
      grants access. Green in real standalone+RLS HTTP suite; includes deny case for
      a self-token that claims read/write permissions without stored authority.
- [x] delegated admin token (A) → `/v1/admin/users/{B-user}/…` isolated — building block
      found: `authcore.MintDelegatedAccessToken(signer, {Issuer, Audiences:["openrails"],
      DelegatedSubject, Permissions:["openrails:merchant:billing:read"]})` signed by a
      `RegisterRemoteApplication` issuer (the verifier trusts remote_application issuers, #481);
      harness helper added. ASSERTION SHAPE: `GetAdminUserBillingProfile` returns
      200 with an EMPTY profile (not 404) for an unknown user, so the test is "A sees an empty
      profile / B sees a populated one" — seeded through real service HTTP deposit under B.
- [x] self token → only its own `delegated_sub`; no `:user_id` reachable — same delegated-mint helper
- [x] delegated `/v1/admin` (the #528 delegated admin surface) cross-merchant — same
      delegated-mint dependency as `/self`. CORRECTION: standalone is MULTI-MERCHANT by design.
      `ginboot.NewServer` leaves `configuredMerchant` ZERO, so the server resolves the merchant
      PER-CREDENTIAL (API-key owner / delegated token issuer) — proven by THIS suite serving
      merchants A and B through one standalone server. `configuredMerchant` is the EMBEDDED
      single-merchant binding only (`embed.Options.Merchant`); it never pins standalone. (#528
      retired the per-user operator `/admin`; `/admin` IS the delegated multi-merchant surface now.)
- [x] API-key case green; the cornerstone multi-merchant surface is pinned
- [x] Phase 2 suite green with real standalone HTTP + `openrails_app` RLS:
      `go test -tags integration ./internal/integrationharness -run 'TestServiceTokenCrossMerchantIsolationHTTP|TestRemoteApplicationSelfJWTCrossMerchantIsolationHTTP|TestDelegatedAdminCrossMerchantIsolationHTTP|TestDelegatedSelfTokenSubjectIsolationHTTP' -count=1`

### Phase 3 — bearer unification (ENG-REVIEW GATED; do not start without review)
- [ ] Design `Principal{MerchantID, MerchantSource, CredentialType, Subject, Can(ctx, perm)}` + `Authenticate(bearer)` resolver (extend `resolveServiceCredential`)
- [ ] Preserve both invariants: per-credential merchant-binding + live-vs-claim permission source
- [ ] One `RequirePermission(perm)` gate calling `Principal.Can(ctx, perm)`
- [ ] Rename public/operator terminology from service-token to API-key throughout
      docs, CLI/help, route comments, and response fields where feasible. Keep
      existing internal code names only as temporary compatibility/migration labels
      until the auth boundary refactor can safely rename them.
- [ ] Migrate `/service`
- [ ] Migrate `/self`
- [ ] Migrate `/admin` + `/admin/merchants`
- [ ] Migrate `/me`
- [ ] Migrate `/platform`
- [ ] Delete superseded middleware families (admin-checker path, `ServiceTokenRequired`+gate, `DelegatedSelfRequired`/`DelegatedPrincipalRequired`+gate)
- [ ] Do not start this refactor until the full Phase 2 isolation suite is green.
- [ ] Phase 2 suite green before + after; per-tier auth tests still pass

## Validation
- Phase 2 cross-merchant suite green BEFORE and AFTER Phase 3.
  - Current Phase 2 gate: `go test -tags integration ./internal/integrationharness -run 'TestServiceTokenCrossMerchantIsolationHTTP|TestRemoteApplicationSelfJWTCrossMerchantIsolationHTTP|TestDelegatedAdminCrossMerchantIsolationHTTP|TestDelegatedSelfTokenSubjectIsolationHTTP' -count=1`
  - Existing remote-application harness gate: `go test -tags integration ./embed -run 'TestStandaloneRemoteApplicationAuth|TestMerchantControlBoundary' -count=1`
- Existing per-tier auth tests still pass (no permission-semantics change).
- No new public surface; per-credential merchant-binding semantics identical.

## Related
- #528 (delegated-admin convergence — same admin surface this builds on)
- #227 (RLS merchant scoping — the backstop these tests + the unified resolver protect)

---

# #533: Log external provider mutations executed by convergence/provider intents

**Completed:** yes

The convergence engine and provider-intent executor need an append-only record of every external payment-provider mutation they actually execute. We do not need an audit log for ordinary internal DB changes here; Postgres state and reconcile findings already cover that. The important evidence is: "OpenRails called Stripe/NMI/CCBill/Solana and changed something outside our database."

This should be symmetric with `pull-provider`:

- `pull-provider` logs/plans local mirror changes it would make and records what it did make when mutation flags are used.
- Provider-intent/convergence execution should log remote mutations it is about to attempt and the remote mutation result once attempted.

## Scope

Log only external side effects, for example:

- provider subscription cancel/delete/update
- provider refund/void/capture
- provider catalog product/price/plan create/update/archive
- provider payment retry or rebill attempt
- provider customer/payment-method/vault mutation
- Solana on-chain transaction submission when OpenRails submits the transaction

Do not log routine internal state repairs, grant derivation, lifecycle status flips, local mirror upserts, or other Postgres-only convergence changes in this issue.

## Target Model

- Add a structured append-only external mutation log, likely table-backed so it is queryable by merchant/provider/account/intent/run.
- Link each event to the source of the side effect when available:
  - `provider_intent_id`
  - `convergence_run_id` or finding id if applicable
  - merchant id
  - provider account id
  - provider type/environment/account id
  - idempotency key / request id
  - remote object id(s)
- Store action metadata in a scrubbed form: action type, target type/id, non-secret parameters, status, error class/message, remote response identifiers. Never store provider API keys, webhook secrets, card data, private keys, raw Authorization headers, or full unsanitized request/response bodies.
- Record at least two phases:
  - `planned` / `attempting`: OpenRails decided to call the provider.
  - `succeeded` or `failed`: provider call result.
- Make retry behavior explicit: multiple attempts against the same provider intent should be separate attempts linked to the same intent/idempotency key, not overwritten history.
- Use the same merchant/provider-account scoping from #530/#529 so an external mutation log can never ambiguously mix provider accounts.

## CLI / Operator Surface

- `pull-provider` remains the read-side mirror command and should continue to show would-change/did-change local mirror behavior.
- Add or extend an operator-facing command/report for external mutation history, probably under `openrails intents` or `openrails provider-actions`, for example:

```bash
openrails intents log --merchant doujins --provider stripe
openrails intents log --merchant doujins --intent <uuid>
```

- The output should be safe for terminals/log collectors by default: compact summary in table mode, full scrubbed structured fields in `--format json`.

## Tasks

- [ ] Inventory every code path that mutates a remote provider from provider intents, convergence repairs, catalog push, refund/cancel/rebill flows, and Solana submission.
- [ ] Design and add the append-only external mutation log schema with merchant/provider-account/idempotency linkage.
- [ ] Add a small writer API used at the provider boundary, not scattered ad hoc across business logic.
- [ ] Wrap provider-intent execution so every external attempt writes `attempting` then `succeeded`/`failed`.
- [ ] Ensure retries append attempts instead of mutating away prior history.
- [ ] Ensure logs are scrubbed and add tests proving secrets/card/private-key material is not persisted.
- [ ] Add CLI/report surface for querying external provider mutation history.
- [ ] Update `pull-provider` reporting/docs to describe the symmetry: pull logs local mirror changes; provider-intent/convergence logs remote provider changes.
- [ ] Add integration tests with real local HTTP provider fakes/dev servers where available, proving a remote mutation creates the expected external mutation log entries.

---

# #528: Converge billing admin on the delegated model — retire per-user /v1/admin, rename /merchant-admin → /admin

**Completed:** no

**Actual-code audit (2026-06-19):** OpenRails side is complete. The delegated
billing-admin surface is `/v1/admin/*`; composite user-detail includes
subscriptions, entitlements, payments, payment methods, product access,
multi-currency credit balances, and owed money; payment methods have an admin
delete action; old dedicated admin reads/actions and the old live-admin catalog
checker path are gone. **Still open only because host repos must update their
OpenRails billing paths from `/v1/merchant-admin/*` to `/v1/admin/*`.**

**Remaining-scope decision (2026-06-19):** admins MUST be able to inspect a user's
multi-currency credit balances, owed money, and payment methods, and must be able
to delete payment methods when needed. Do this in the composite admin user-detail
plus explicit payment-method delete action; do not keep old processor-specific
read endpoints. Unused old live-admin catalog/checker paths can be cleaned up.

The per-user `/v1/admin` model is outdated. The correct model (same as #527's bootstrap): a merchant has an org + issuer in OpenRails' AuthKit; the issuer (host app) mints JWTs that grant ITS users permissions in OpenRails — a user reading/modifying their own billing (`openrails:self:*`), or an admin-user acting on another user (`openrails:merchant:*`). That is the **delegated** model, served today by `/v1/merchant-admin/*` (+ `/v1/self/*`). The per-user `openrails:admin` surface (`/v1/admin/*`, `HasAdminPermission(org,userID)` live check) assumes local users that the standalone model doesn't have.

**Investigation (2026-06-19):**
- No consumer calls OpenRails' billing `/v1/admin/*`: embedded hosts (cozy-art/doujins/tensorhub) use `/v1/merchant-admin/*` + `/v1/self/*`; their own `/v1/admin/*` paths are app-admin (runpod/fleet/galleries), unrelated. No host imports `embgin.RegisterAdminRoutes`. In standalone it's unreachable (no local users). So the per-user surface is effectively dead.
- It is NOT a pure rename: the two surfaces each have unique endpoints.
  - `/v1/admin` only: `GET /payments`+`/payments/:id`, `/intents`, `POST /solana/recurring/plans`, `/users/:id/product-access` (get/grant/revoke), entitlement-features (RegisterEntitlementFeatureRoutes), `/reconcile/*`.
  - `/v1/merchant-admin` only: `/merchant-configuration` (get/put), `/secrets/*`.
  - Shared (handlers reused): subscriptions, refund, off-channel, users/* profile+entitlements+nmi+ccbill, metrics, repair-alerts, manual-rebill-attempts.

**Plan:**
- Phase 1 (OpenRails, non-breaking): port the `/v1/admin`-only endpoints onto the delegated surface (RegisterMerchantAdminRoutes) with `openrails:merchant:*` perms; delete the per-user RegisterAdminRoutes + `registerAdminRoutesOn/At` + server.go mount + embedhttp mount + `embgin.RegisterAdminRoutes`. (Leave `HasAdminPermission`/`IsLiveAdmin` for now — still used by admin-gated catalog reads; migrate those to the delegated principal as a follow-up.)
- Phase 2 (breaking, cross-repo): rename `MerchantAdminRoutePrefix` `/merchant-admin` → `/admin` and `RegisterMerchantAdminRoutes` → `RegisterAdminRoutes`; update host frontends `/v1/merchant-admin/*` → `/v1/admin/*` (and `/billing/...`) in cozy-art/doujins/tensorhub.

**Sequencing decision (OPEN):** Phase 2 breaks host frontends the moment the path changes. Either (a) hard cut + update all three host repos in lockstep, or (b) mount the delegated surface at BOTH `/admin` and `/merchant-admin` for a deprecation window, migrate hosts, then drop `/merchant-admin`.

**DECIDED:** hard cut (no alias, no old-route support); hosts updated later. Also redesign routes + request/response shapes while consolidating — minimal RESTful surface, only fields actually used, no theoretical/unused routes or fields.

## Historical proposed minimal RESTful surface (superseded by the resolution below)

Single delegated admin surface at `/v1/admin/*` (`openrails:merchant:*` perms). Consolidations vs today in **bold**:

```
Subscriptions
  GET    /admin/subscriptions
  GET    /admin/subscriptions/{id}
  DELETE /admin/subscriptions/{id}                 # cancel (was POST /:id/cancel)

Payments
  GET    /admin/payments[?user_id=]                # **folds /users/{id}/payments into a filter**
  GET    /admin/payments/{id}
  POST   /admin/payments/{id}/refunds              # was POST /:id/refund
  POST   /admin/payments                           # record off-channel (body: user_id + fields)

Users (billing)
  GET    /admin/users/{id}                         # billing profile
  GET    /admin/users/{id}/payment-methods         # **folds /nmi + /ccbill + their /metrics into one shape**
  GET    /admin/users/{id}/entitlements
  POST   /admin/users/{id}/entitlements
  DELETE /admin/users/{id}/entitlements/{id}

Entitlement features / product features
  GET/POST        /admin/features
  GET/POST/DELETE /admin/products/{id}/features

Metrics
  GET /admin/metrics                               # **one response folding summary/revenue/subscriptions/processors/churn**

Configuration & secrets
  GET/PUT          /admin/configuration
  GET              /admin/secrets
  PUT/DELETE       /admin/secrets/{name}
  POST             /admin/secrets/{name}/validate

Operational
  GET /admin/repair-alerts
  GET /admin/manual-rebill-attempts
  GET /admin/provider-intents                      # was /intents

Reconciliation (#107)
  GET/POST /admin/reconcile/runs
  GET      /admin/reconcile/runs/{id}
  GET      /admin/reconcile/findings
  PATCH    /admin/reconcile/findings/{id}          # {status: acknowledged|dismissed}; was POST /ack + /dismiss

Recurring plans (#254)
  POST /admin/recurring-plans                      # was /solana/recurring/plans
```

Shapes: trim each request/response to fields a consumer actually reads (ground in host frontend usage during impl); drop pagination/filter knobs nothing uses, collapse single-use nested objects.

**Usage data (host repos, 2026-06-19):** of the merchant-admin surface, hosts only call `merchant-admin/subscriptions`, `merchant-admin/users/{id}`, `merchant-admin/metrics/summary`. (`/v1/self/*` is heavily used for self-service; `/v1/service/*` for s2s — both out of #528 scope. The `/v1/admin/*` paths in host repos are the hosts' OWN app admin — runpod/galleries/creators/etc. — not OpenRails billing.) Admin UIs are incomplete, so "uncalled" ≠ "droppable" for obvious console operations.

**Resolution (FINAL — user decisions 2026-06-19):**
- DROP (routes + dead handlers): `/provider-intents`, `/reconcile/*` (#107), `/recurring-plans` (#254), entitlement-features + `active_entitlements` (#245).
- Metrics: fold the 5 into one `GET /admin/metrics`.
- **Three separate concepts, kept distinct** (NOT merged): entitlements, product-access/ownership (#250), credit balance. Each is its own section in user reads + its own write actions.
- **No dedicated read endpoints for entitlements/product-access/credits.** Their info is EMBEDDED in user reads (user-detail + user list/search). Writes stay as dedicated actions.

Resulting user surface:
```
GET    /admin/users[?entitlement={slug}&...]   # list/search; each row embeds entitlements/product-access/credit summary (NEW — admin user search)
GET    /admin/users/{id}                        # billing profile: subscriptions + payment-methods + entitlements + product-access + credit-balance sections
POST   /admin/users/{id}/entitlements           # grant
DELETE /admin/users/{id}/entitlements/{slug}    # revoke
POST   /admin/users/{id}/product-access         # grant ownership (#250, separate concept)
DELETE /admin/users/{id}/product-access/{id}    # revoke ownership
# credit-balance read embedded; credit write = existing PermMerchantCreditsWrite op (keep if present)
# DROPPED reads: GET /users/{id}/entitlements, /product-access, /nmi, /ccbill, /nmi/metrics, /ccbill/metrics, active_entitlements
```
- `payment-methods` folds /nmi + /ccbill (+ metrics) into one embedded section / `GET /admin/users/{id}/payment-methods`.
- Self surface (`/v1/self/*`) likewise embeds the caller's active entitlements in their self-detail response.
- Admin user search (`GET /admin/users?entitlement=`) is NEW capability (no current handler) — build minimal (list + filter), or flag as follow-up if the list query is non-trivial.

**Implementation plan — build-safe increments (DECIDED, in progress). Detailed checklists below.**

### Increment 1 (structural) — DONE (commit 53fbbe3c)
- [x] ginroutes/routes.go: `MerchantAdminRoutePrefix`→`AdminRoutePrefix` ("/admin"); `RegisterMerchantAdminRoutes`→`RegisterAdminRoutes`; ported `GET /admin/payments` + `/payments/:id` (`read`).
- [x] Unmount per-user surface: `server.go` (drop `registerAdminRoutesOn` call), `embedhttp.go` (drop `RegisterAdminRoutes` mount), `pkg/embedded/gin/gin.go` (delete `embgin.RegisterAdminRoutes`).
- [x] Rewire callers: routes_self.go, pkg/embedded/gin/self.go. Update tests: ginroutes/self_service_test.go, routes_self_test.go, pkg/embedded/gin/self_test.go, embedded_mux_test.go.
- [x] Validated via HTTP route harness (`go test ./internal/http/... ./pkg/embedded/gin/...`).

### Increment 2 (drops) — DONE (commit a89ca887): dropped features' routes + dead per-user code removed; self handlers + product-access kept; build+vet+route tests green.
- [x] Delete dead per-user `RegisterAdminRoutes` (internal/http/routes/routes.go) + `registerAdminRoutesAt`/`registerAdminRoutesOn` (internal/http/routes_admin.go).
- [x] **Before deleting each handler, grep its callers** — DONE (2026-06-19): kept lower-level `/self`, `/service`, worker, repo, and catalog-action code; removed only admin-only reads/actions.
- [x] `/provider-intents`: drop route + `httphandlers.GetAdminProviderIntents` (verify not used elsewhere). DONE (2026-06-19): no handler/function/route remains; provider-intent DB/worker code stays live.
- [x] `/reconcile/*` (#107): drop routes + `AdminReconcile{Run,ListRuns,GetRun,ListFindings,AckFinding,DismissFinding}`. DONE (2026-06-19): old admin-run handlers are gone; convergence/reconcile services, workers, migrations, and catalog item reconcile actions remain live and intentionally kept.
- [x] `/recurring-plans` (#254): drop route + `AdminPublishSolanaPlan` + `publishSolanaPlanRequest` (internal/http/handlers/solana_recurring.go). DONE (2026-06-19): old admin plan-publish handler is gone; on-chain execution/webhook paths remain live.
- [x] entitlement-features + `active_entitlements` (#245): delete `RegisterEntitlementFeatureRoutes` (internal/http/routes/entitlement_features.go) + handlers `Create/ListEntitlementFeatures`, `ServiceGetActiveEntitlements`, `List/Attach/DetachProductFeature`. DONE (2026-06-19): old route/handlers are gone; repo/sqlc code remains for internal product-access/feature data.
- [x] Fix orphaned imports; remove/upd tests referencing dropped handlers. Build + vet + route tests green. DONE (2026-06-19): focused route/handler packages are green.

### Increment 3 (RESTful route shapes) — DONE (commit d0336be0)
- [x] ginroutes/routes.go: subscription cancel `POST /subscriptions/:id/cancel` → `DELETE /subscriptions/:id` (handler `AdminCancelSubscription` unchanged).
- [x] refund `POST /payments/:id/refund` → `POST /payments/:id/refunds` (in route; DB-validated by `TestAdminRefundPayment*` against the delegated surface).
- [x] Update ginroutes/self_service_test.go path assertions. Build + tests green.

### Increment 4 (shapes — handler logic; the big one) — NOT a mechanical pass; needs design decisions + DB/ClickHouse integration tests. Findings from increment-2/3 investigation:
> - **product-access is currently DEAD code**: `ginroutes.RegisterProductAccessRoutes` (which mounts /me + /service + /admin product-access) is defined but **mounted nowhere**. So "porting" it = reviving a dormant feature; also decide the /me + /service surfaces, not just /admin. Handlers exist (`GetMyProducts`, `GetAdminUserProductAccess`, `Grant/RevokeAdminProductAccess`, `ServiceGetUserProductAccess`).
> - **metrics fold is a design decision, not a fold**: the 5 handlers (admin_metrics.go) have DIFFERENT default ranges (churn 180d, others 30d), `granularity` params (revenue/subscriptions), and per-section multi-currency disambiguation (`?currency`, else error on multi). A single `GET /admin/metrics` must decide: one `?period`+`?currency` applied uniformly (churn computes its own window internally), sections as `{summary,revenue,subscriptions,processors,churn}`. Confirm the unified shape before building. ClickHouse-backed → needs the analytics stack to integration-test.
> - composite user-detail + payment-methods + self-entitlements compose multiple services → DB-integration-test against dbtest/compose.
- [x] New perm `PermMerchantProductAccessWrite = "openrails:merchant:product-access:write"` in internal/controlplane/catalog.go (const + merchantCatalog map + MerchantCatalogNames — placed beside its sibling `PermMerchantEntitlementsWrite`; merchant perms deliberately live in merchantCatalog, NOT catalogEntries).
- [x] Port product-access WRITES to ginroutes/routes.go: `POST /admin/users/:user_id/product-access` (`GrantAdminProductAccess`) + `DELETE /admin/users/:user_id/product-access/:id` (`RevokeAdminProductAccess`), gated by the new perm. DB-validated by `tests/admin_product_access_test.go`: grant→201 (customer_id=userID, status active) → appears in composite product_access section → DELETE→200; + 403 gate without the perm. Handlers resolve the target by `path.UserID` (no federated-v7 bug — consistent with reads).
- [x] Composite `GET /admin/users/:user_id` (`GetAdminUserBillingProfile`, `internal/http/handlers/admin_users.go`): embeds `entitlements[]` + `payments[]` + `payment_methods[]` + `product_access[]` + `subscription` (single active). DB-validated by `TestAdminUserDetailComposite_Delegated`. The two remaining sections (`subscriptions[]`, `credit_balance[]`) are the two sub-items below.
- [x] **`subscription` (single active) → `subscriptions[]` (ALL active).** DONE (2026-06-19): `GetAdminUserBillingProfile` now returns `subscriptions []models.Subscription` via `SubscriptionService.GetActiveSubscriptionsByUserID`; the legacy singular `subscription` field is gone. `TestAdminUserDetailComposite_Delegated` asserts `subscriptions` is present and `subscription` is absent.
- [x] **`credit_balance[]` + owed-money section is REQUIRED.** DONE (2026-06-19): `MoneyService.ListBalancesForCustomer` lists every currency where the user has balance/settings/owed exposure, and the composite embeds `credit_balance[]` with `{currency,balance,held_balance,outstanding_owed_amount,...}`. `TestAdminUserDetailComposite_Delegated` asserts a funded currency plus an owed-only currency.
- [x] DROP dedicated reads (routes + handlers if now-unused): `GET /users/:id/entitlements`, `/product-access` (admin), `/nmi`, `/nmi/metrics`, `/ccbill`, `/ccbill/metrics`. DONE (2026-06-19): unmounted the old admin reads from `RegisterAdminRoutes`, removed the dead NMI/CCBill/admin-entitlements/admin-product-access read handlers, and kept write actions plus composite user-detail.
- [x] `payment_methods[]` admin visibility is REQUIRED, plus a delete action. DONE (2026-06-19): composite user-detail returns payment methods through the existing payment-method API DTO; `DELETE /admin/users/:user_id/payment-methods/:id` is gated by `openrails:merchant:payments:write` and refuses active/pending/past_due subscription-linked methods. `TestAdminUserPaymentMethodDelete_Delegated` validates delete + the composite no longer listing the method.
- [x] Fold metrics → one `GET /admin/metrics` returning `{summary, revenue, subscriptions, processors, churn}`; 5 sub-routes removed. `GetAdminMetrics` composes the 5 analytics calls + generic `resolveMetricSection[T]` (single-currency → bare object; multi-currency needs `?currency` else 400 — this disambiguation is specific to metrics' SCALAR summaries; credit_balance above does NOT need it). DB-validated (ClickHouse) by `TestAdminMetricsFolded` (all 5 sections) + `TestAdminMetrics_RequiresMetricsRead` (403 gate).
- [x] Self surface: caller's active entitlements ALREADY embedded — `GET /v1/self/status` (`GetMyBillingStatus`, `billing_status.go`) returns `BillingStatusResponse.Entitlements`; plus the dedicated `GET /v1/self/entitlements/active` (`SelfGetActiveEntitlements`). No further work unless we also want them in another self-detail shape.
- [x] Shape trimming: DROPPED from #528 (2026-06-19). This is speculative DTO churn without a failing consumer; #528 now limits shape work to the required composite fixes (`subscriptions[]`, `credit_balance[]`, owed money, `payment_methods[]`, delete action) and deleted dead dedicated reads.
- [x] DB-integration-test the composite (`subscriptions[]` + `credit_balance[]`/owed money + payment-method visibility) and admin payment-method delete against the dbtest/compose harness (extend `tests/admin_user_detail_integration_test.go`). DONE (2026-06-19): `go test ./tests -tags integration -run 'TestAdminUserDetailComposite_Delegated|TestAdminUserPaymentMethodDelete_Delegated|TestAdminSurface_RejectsWithoutBillingRead' -count=1` green.

### Increment 5 (admin user search) — DROPPED from #528 (2026-06-19, user decision)
Cut from this issue. #528 is about CONVERGING the admin surface on the delegated model, not building a new search capability — bolting a net-new `GET /admin/users?entitlement=` search on here over-scoped it.

Two DIFFERENT needs were conflated under "user search," neither belongs in #528, and BOTH are deferred (build when needed) — they live in **future.md** now (the old #108 was deleted):
1. **OpenRails' own admin-dashboard user search** — a human operator browsing/filtering users in an OpenRails-hosted billing console. Embedded hosts (doujins/cozy-art/etc.) DON'T need this: they own their user tables and have their own admin UIs. Only useful once a STANDALONE OpenRails admin GUI exists (it doesn't yet). → folded into **#228** (future.md), the standalone admin-console plan; punt.
2. **A host-facing "which of my users have entitlement X" query** — useful for BOTH embedded and standalone hosts; now ACTIVE (doujins needs it). → **#535** (progress.md) as the billing-enrichment FILTER provider behind AuthKit's directory, not an OpenRails admin-console search.

Net: #528 ships the delegated admin surface (composite user-DETAIL by id) WITHOUT a user LIST/search. See **#535** (progress.md, active) for the reverse-query provider, and **#228** (future.md) for the standalone admin console.

### Cross-cutting cleanup
- [x] **CRITICAL — migrate the `tests/` admin INTEGRATION suite to the delegated model. DONE (2026-06-19) — hard cut, per-user model fully removed.** All admin integration files now authenticate as a delegated merchant principal via `newHostSeamAdminRouter(t, suite, subject, []perms)` (host-seam `DelegatedPrincipalRequired`), hit the new `/v1/admin` paths, and assert the redesigned shapes. `go test ./tests/ -tags integration -run 'TestAdmin|TestRemoved'` is GREEN (57s, real Postgres+ClickHouse+Redis).
  - Migrated files: `admin_user_detail_integration_test.go` (composite read + 403 gate), `admin_offchannel_payments_test.go` (payments:write), `admin_subscription_test.go` (billing:read profile + `GET /subscriptions` list + `extend`→404 regression + public health), `admin_entitlements_source_test.go` (entitlements:write grant + append-after-end + `/grants`→404 + **403 without entitlements:write**), `admin_payments_test.go` (billing:read list/get/filter + payments:write refund via `/refunds` + **403 without payments:write** + reaches-any-user-in-tenant), `admin_metrics_test.go` (single folded `GET /metrics` asserting all 5 sections + **403 without metrics:read**).
  - Per-user model CUT from `tests/test_helpers.go`: deleted `setupTestSuiteWithAdminAuth`, `CreateAdminIdentity`, `adminOrgClaims`, `testAdminOrgSlug` (+ unused `controlplane`/`dbtest`/`embcp` imports); `setupAdminTestSuite` deleted from `admin_subscription_test.go`.
  - **Bugs the migration EXPOSED + FIXED:**
    1. **Grant-target customer-resolution bug (real handler bug, fixed in `internal/http/handlers/entitlements.go`):** `GrantAdminEntitlement`'s delegated branch minted a federated `(issuer,subject)` v7 customer via `UpsertCustomerByIssuerSubject`, but the admin READ path (`GetAdminUserBillingProfile`→`identity.CustomerIDFromString(userID)`), off-channel payments (`RegisterPurchase`→`customerIDFromUser`), and seeds all key on the raw `userID` (#364 UUID-only). So an admin-granted entitlement landed on a DIFFERENT customer than the user's reads/payments — the admin couldn't see what they just granted, and append-after-latest-end couldn't see prior windows. Collapsed `tenantSubjectForEntitlementGrantTarget` to always resolve by `userID` (the #364 subject), identical to reads + commerce writers. This is exactly the "wrong assumption" #528 set out to remove.
    2. **Test-helper bit-rot vs the tenant→merchant rename:** `tests/seed_data.go` (products + prices) and `tests/db_helpers.go` (`entitlementCols`) still inserted/scanned a `tenant_id` column that no longer exists (schema uses `merchant_id`). Fixed all three.
    3. **RLS bit-rot:** `InsertEntitlement` (and `TestAdminRevokeAccess`) called RLS-aware `Runtime.DB` with a bare context. Made `InsertEntitlement` default the canonical test merchant when none is pinned (#336 single-merchant suite); pinned the merchant in `TestAdminRevokeAccess`.
- [x] Migrate admin-gated catalog reads off the per-user model (self-contained, DB-testable). DONE (2026-06-19): public catalog `GET /products` and `GET /prices` no longer expose inactive rows through `IsLiveAdmin`; inactive/draft catalog management belongs to `/merchant/catalog/*`. Deleted `runtime.AdminChecker`, the `server.go` runtime wiring, and dead `policy.IsLiveAdmin` + tests. Kept `AdminPermissionChecker`/`AdminPermissionRequired*` because they are still real gates for merchant action routes and `/v1/admin/merchants`, not leftover billing-admin user-detail paths.
- [x] Docs + comment sweep (deferred from Increment 1): DONE (2026-06-19). Current docs/comments now describe `/v1/admin/*` / `/billing/v1/admin/*` and `RegisterAdminRoutes`; only historical tracker notes/test names still use `merchant-admin` as old-state or conceptual wording.
- [ ] (Later, separate repos) update host frontends (cozy-art/doujins/tensorhub): `/v1/merchant-admin/*`→`/v1/admin/*` paths + new shapes; cozy-art may switch any per-user `/v1/admin` billing calls to the delegated surface.

**Verification each increment:** build + vet + route/unit tests, AND DB-integration tests via the `internal/dbtest` harness (testcontainers, or `OPENRAILS_TEST_DB_URL` against `docker-compose.yaml`); host-level e2e via `~/cozy/e2e` and the host repos' full-stack compose. (Earlier "not DB-testable" note was WRONG — corrected.)

---

# #525: Use API-key terminology in bootstrap authority seeding

**Completed:** yes

OpenRails bootstrap should use `api_keys` for long-lived machine credentials instead of exposing AuthKit's internal/service-token terminology.

AuthKit issue #86 owns the primitive and manifest alias. OpenRails owns the bootstrap YAML/docs/examples that seed OpenRails authority through AuthKit.

Target bootstrap shape:

```yaml
auth:
  orgs:
    - slug: local-stack
      api_keys:
        - name: local-stack-operator
          permissions:
            - openrails:admin
            - openrails:catalog:write
          resources:
            - kind: openrails.merchant
              id: local-stack
          output:
            file: ./.secrets/openrails/local-stack-operator.key
```

Prefix direction:
- AuthKit #86 now exposes the public `APIKeyPrefix` name while preserving the existing `<prefix>_st_<key_id>_<secret>` resolver mechanics.
- OpenRails still uses the existing OpenRails brand prefix/wire format for generated keys until the deeper AuthKit key-format migration is done.
- Prefix is not the merchant/org/user and has no authorization meaning.
- Do not put tenant identifiers or secrets in the prefix.

**Tasks:**
- [x] Update bootstrap parsing/examples from `service_tokens` to `api_keys`.
- [x] Add an OpenRails compatibility shim that accepts `auth.orgs[].api_keys` and converts it to the pinned AuthKit provisioning field, while rejecting mixed `api_keys` + `service_tokens`.
- [x] Update `config/merchants*.yaml`, docs, CLI text, and tests to say API key.
- [x] Keep any accepted legacy `service_tokens` alias undocumented and temporary.
- [x] Ensure generated outputs use `.key` or similarly clear filenames, not `.token`.
- [x] Tests: example bootstrap manifest with `api_keys` parses and produces the same AuthKit provisioning request.

---

# #524: embedded merchant configuration startup parity

**Completed:** yes

DONE 2026-06-19: embedded startup now has parity with standalone merchant-config seeding. `embed.Options.MerchantConfiguration` accepts the public SDK merchant configuration shape, registers the bound merchant first, then seeds `merchant_configurations` through `Runtime.Client().SetMerchantConfiguration`. Public `openrails.MerchantConfigurationInput` now includes `Profile`, and both remote and embedded client adapters write profile plus delegated wasted-spend windows.

Embedded hosts should be able to seed the same merchant-scoped configuration that standalone/SaaS OpenRails seeds through `openrails push-merchant-config`.

Original gap: `merchant_configurations.config.profile` is the correct home for sender/display branding, and the service HTTP route already supported it, but `openrails.Client.SetMerchantConfiguration` only exposed delegated wasted-spend windows and `embed.New` only registered the merchant row.

Lazy plan:
- [x] Add profile to the public `openrails.MerchantConfigurationInput`.
- [x] Wire profile through remote and embedded `SetMerchantConfiguration`.
- [x] Add one `embed.Options.MerchantConfiguration` field so embedded startup can seed all current merchant-configuration fields after the merchant slug is registered.
- [x] Test embedded startup profile seeding and SDK conformance for profile writes.

---

# #513: admission-and-holds-redis-simplification

**Completed:** yes
**Status:** DONE 2026-06-18. The whole admission path now runs on the Redis `spendgate` + the #512/#514 ledger; the Postgres-with-locks machinery is gone. Shipped: (A) budget windows moved off `budget_window_state`/`budget_inflight_holds` onto the Redis limiter and those tables dropped (migration 007); `internal/modules/holds` deleted, `internal/modules/budgets` gutted to `types.go`. (B) cap taxonomy collapsed to TWO symmetrical spend limits via #517 (`PayerSpendLimits` platform/tier + `InvokerSpendLimits` payer/scope); the dead `spend_policy.go` enforcement + `money_spend_limits` removed (B1, migration 005). (C) balance cache removed after #512 Phase H — admit does one O(1) `GetAdmissionCapacity` read; policy config stays cached (15-min TTL, tier-only; invoker grants read LIVE). (D) one atomic admit/capture/release Lua gate wired into `admitter.go` + `pkg/service`. (E) capture writes the durable #512 ledger off the hot path, no outbox. (F) money_windows credit-window API + table deleted (migration 012). Windows are fixed-only (#517 removed cadence). DROPPED as unneeded (do NOT build later): the `held`-gauge reconciliation sweep and the optional "max concurrent in-flight $" cap (both noted inline). The dead throughput/concurrency surface was removed (fleet fairness = consumer scheduler's job). `go build ./...` + `go vet -tags integration ./...` clean. Remaining tail is outside this repo: tensorhub SDK lockstep (#517 already did the rename + Admit rewire; verify build/tests against local openrails).
**Update 2026-06-18:** balance cache REMOVED after #512 Phase H made balance an O(1) account-counter read. Admit now does a direct `GetAdmissionCapacity` point lookup (balance + held + billing mode + credit limit) and then the same Redis `spendgate` EVAL. Policy config cache stays. There is no `admission.BalanceCache`, no runtime field, and no `CachedBalance` spendgate input anymore.

The north star is decision 8 of #512 (the five-step model: balance-check → Redis hold → capture actual → release; failures record wasted-spend in Redis). This issue generalizes that to the *whole* admission system, including budget/spend caps and rate windows.

## Why (the current hot path is heavy and muddled)

Traced in `internal/modules/admission/admitter.go` `Admit` — one DELEGATED admit does:
1. `money.GetTier` → Postgres read.
2. `policies.GetTierPolicy` → Postgres read (cacheable).
3. FX convert estimate → policy currency (`internal/integrations/fx`).
4. wasted-spend gate → **Redis** (`WastedSpendGuard` on `ratelimit.Limiter`) — already efficient ✅.
5. `buildBudgetScopes` → Postgres reads of `budget_policies`.
6. `budgets.ReserveScopes` → `budgets.Reserve` → **Postgres tx, `budget_window_state` locked `FOR UPDATE`, INSERT `budget_inflight_holds`**.
7. `money.AuthorizeAndHold` → **Postgres tx, `customers` row locked `FOR UPDATE`, derive balance by SUM(`money_blocks`)+`money_windows`** → then the Redis hold.

So one admit = ~3 Postgres reads + **two Postgres write-txns each taking a `FOR UPDATE` row lock**; capture (`CaptureAuthorized`/`spendBalanceThenOwedTx` + `budgets.CaptureByCoords`) adds more locked txns. The two row locks (customer row + window-state row) serialize a payer's concurrent requests — the throughput ceiling.

Two structural problems:
- **Policy and accounting are muddled.** Policy = read-mostly config (what the caps/tiers are: `money_settings`, `money_spend_limits`, `budget_policies`, `tier_policies`, `tier_schedules`). Accounting = hot counters (how much used this window: `budget_window_state`, `budget_inflight_holds`, balance derivation, the Redis hold, the Redis wasted-spend). Accounting belongs in Redis; policy belongs in a versioned in-memory cache. Today budget accounting is the one axis still in Postgres-with-locks while throughput + wasted-spend already ride Redis.
- **The cap taxonomy has ~4 overlapping shapes** for "max $X over window W for scope S": `money_settings.max_spend_per_day/month`, `money_spend_limits` (per invoker), `budget_policies` (hierarchical scopes), `tier_policies` (per-tier windows). There also appear to be TWO generations of spend-cap code: the newer `budget_policies`/`tier_policies` path in `admitter.go` and an older `money_settings`/`money_spend_limits` path in `spend_policy.go` (`CheckSpendAllowed`/`getSpendLimit`).

## Guiding principles

1. **Policy is cached config; accounting is Redis counters.** The hot path does one O(1) Postgres capacity/config read on misses, but no Postgres locks or write-side budget reservations.
2. **One windowed-cap primitive.** Reuse the `ratelimit.Limiter` fixed-window Redis counter that throughput + wasted-spend already use; every cap (throughput / budget-$ / wasted-$) becomes the same counter.
3. **Atomicity without locks.** Admit and capture are each a single Redis `EVAL` (Lua), atomic by Redis's single thread — no `FOR UPDATE`, no races.
4. **Bounded over-admission is acceptable** (decision 8, #512): Redis/cache loss relaxes caps temporarily; the worst case is bounded over-spend, never ledger corruption. The durable truth is the #512 ledger, written off the hot path.
5. **Simplify aggressively, but map before cutting** — confirm each retired mechanism is dead/covered before deleting.

## Metadata

- Category: architecture
- Status: done
- Passes: true

## Work breakdown

### A. Unify windowed caps onto the existing Redis limiter
- [ ] Move money-budget windows off `budget_window_state` + `budget_inflight_holds` (Postgres, `FOR UPDATE` per request) onto `ratelimit.Limiter`. Reserved (holds) + used (captured) become two counter contributions per (scope, window) key, TTL = window. (touch: `internal/modules/budgets/*`, `internal/modules/admission/admitter.go`)
- [ ] Delete `budgets.Reserve/ReserveScopes/Capture/Release/CaptureByCoords` locked-tx machinery once windows are Redis-backed.
- [ ] Drop tables `budget_window_state` and `budget_inflight_holds` (migration); window history that reporting needs comes from the durable spend events (#512 ledger), not these.

### B. Collapse the cap taxonomy to one shape
What the "spend-policy cap" IS (for the record): a TIME-WINDOWED ceiling on cumulative ACTUAL spend — per-customer max spend per UTC calendar DAY + per MONTH (`money_settings.max_spend_per_day/month`), the same per-invoker (`money_spend_limits`), plus an arrears OUTSTANDING-owed ceiling (deny codes `daily_cap`/`monthly_cap`/`invoker_*_cap`/`outstanding_cap`). It is NOT a concurrent-in-flight-hold cap — it bounds spend velocity, orthogonal to the balance gate ("even with $10k, this API key can't burn >$100/day").
VERIFIED DEAD 2026-06-17: the `spend_policy.go` enforcement (`AuthorizeSpend` → `CheckSpendAllowed` → `evaluateSpend`) has ZERO live callers — no HTTP route, nothing. `admitter.Admit` never reads it; day/month/outstanding caps are now expressed via `budget_policies`/`tier_policies` windowed scopes. `money_spend_limits` is still WRITTEN (`SetSpendLimit` API wired) but enforced nowhere on the hot path.
- [ ] One policy shape: a list of `{scope, window, limit}`, scope ∈ {payer, invoker, role, tier}; effective policy = merge of matching scopes, DENY if any window over. Tiers just *name* a policy. Day/month become fixed-calendar windows; outstanding-owed stays a balance-side ceiling.
#### B1. DECIDED 2026-06-17 (Paul): delete the dead spend-limit feature in full. It is a MIX of dead + live-shared, so surgical, not a wholesale file delete:
- [x] Dead-only logic — DELETED 2026-06-17: `pkg/service/spend.go` `AuthorizeSpend`/`AuthorizeSpendRequest`/`AuthorizeSpendResult` + `SetSpendLimit` (service wrapper); `internal/modules/money/spend_policy.go` `CheckSpendAllowed`, `evaluateSpend`, `capInput`, `CapResult`, `dayWindow`, `monthWindow`, `SetSpendLimit`, `getSpendLimit`, `spentInWindow`, `activeHoldsTotal`, `ActiveHeldForCurrency`, and the cap deny-codes `DenyInvokerDailyCap`/`DenyInvokerMonthlyCap`/`DenyDailyCap`/`DenyMonthlyCap`/`DenyOutstandingCap`.
- [x] PRESERVED the live-shared bits 2026-06-17: kept `BillingModePrepaid`/`BillingModeArrears` consts + `SpendDecision` in `spend_policy.go` (no relocation needed — the file still hosts the live account-settings code). `SpendDecision` trimmed to `{Allowed, HardStop, DenyCode}` — the only fields `authorize.go`/`admitter.go` set or read; dropped `Caps`/`AlertCode`/`RetryAfterSeconds`/`NextAllowedAt`.
- [x] `pkg/service/spend.go` `DenyInsufficientBalance` REMOVED 2026-06-17 (had only test callers; live deny is `money.DenyInsufficientBalance`). Updated `authorize_and_hold_integration_test.go` to reference `money.DenyInsufficientBalance`.
- [x] DONE 2026-06-17 (unblocked once the #512/#514 agent finished): added forward migration `005_drop_money_spend_limits.up.sql` (`DROP TABLE IF EXISTS openrails.money_spend_limits`, mirroring their `004_drop_single_entry` pattern; `001` baseline + the `merchant_aware_schema_test.go` table list left intact, same convention they used for `money_blocks`/`money_transactions`).
- [x] DONE 2026-06-17 — removed the `money_spend_limits` queries from `money_accounts.sql` (`Get/Upsert`) + `merchant_lifecycle.sql` (`Count/Purge`) and ran `sqlc generate` (vet DB built from migrations via `scripts/sqlc-vet-db.sh` on the compose Postgres). Dropped the gen methods/structs (`Get/Upsert MoneySpendLimit`, `Count/Purge MerchantRowsMoneySpendLimits`, `gen.OpenrailsMoneySpendLimit`) + the already-orphaned `SumSpentInMoneyWindow`. NOTE: the same regen SYNCED the #512/#514 agent's stale staged gen (`grants.sql.go`/`ledger.sql.go`/`money_ledger.sql.go`/`models.go`) to their current `.sql` — whole-repo `go build` + integration-tag `go vet` green across grants/ledger/money/admission/merchants/service, so the sync is correct (it's what CI's `sqlc generate + diff` enforces).
- [x] Removed the `money_spend_limits` cascade entries from `internal/merchants/delete.go` (the list + count + purge switch arms) 2026-06-17. The `merchant_lifecycle.sql` query removal + regen is folded into the STAGED sqlc pass above.
- [x] Deleted the dead tests 2026-06-17: `internal/modules/money/spend_policy_eval_test.go`, `spend_policy_integration_test.go`; pruned the two dead `AuthorizeSpend` funcs in `pkg/service/spend_integration_test.go` (kept `TestGetCreditAccount_Snapshot`). `money_in_integration_test.go`/`reconcile_integration_test.go` had NO dead refs (the original note was off). Re-homed the shared `strptr` helper (was defined in the deleted `spend_policy_integration_test.go`, used by 6 money tests) to money `main_test.go`.
- [x] No HTTP route exposes `SetSpendLimit`/`AuthorizeSpend` (re-verified — only test callers).
- [x] After removal: `go build ./...` + integration-tag `go vet` clean 2026-06-17. The per-invoker spend cap it provided is covered by `budget_policies` scope=invoker windows (Phase A/B unified policy).
- [~] DROPPED 2026-06-18 (do NOT build): a "max concurrent in-flight $" cap is explicitly NOT openrails' job — fleet fairness/concurrency belongs to the consumer's scheduler (see protection 2; tensorhub WFQ), and the openrails throughput/concurrency surface was already deleted. Concurrency stays implicitly bounded by available balance. Recorded here so it isn't re-built as a "gauge window."

### C. Cache policy config; read balance directly
- [~] Balance cache REMOVED 2026-06-18 per Paul after #512 Phase H: balance/held are O(1) account-counter reads, and admission uses one `GetAdmissionCapacity` point lookup instead of a staleness-tolerant cache.
- [x] Policy cache: load `tier_policies`/`budget_policies`/tier ladders into memory, invalidate on the existing `policy_version`/`schedule_version`/`config_version` bumps. (These version columns already exist — they were built for this.)
- [ ] Tier resolution from cache, not `money.GetTier` Postgres read; tier graduation stays a background recompute (on deposit), it just selects the policy.

### D. One atomic admit, one atomic capture (Lua)
BUILT + INTEGRATION-TESTED 2026-06-17 (8/8 green against real Redis via testcontainers): `internal/modules/admission/spendgate/` (`policy.go` + `gate.go` + `gate_integration_test.go`) — the `{scope,cadence,duration,limit}` model + `EffectiveWindows` merge + 3 `redis.NewScript` scripts (admit/capture/release) + `Gate.Admit/Capture/Release/HeldAmount`. Decisions baked in (Paul, 2026-06-17):
- **Direct O(1) account balance, not Redis `:bal`.** `Admit` takes the caller's ledger snapshot as `AccountBalance` ARGV; the affordability gate is `accountBalance − held − cost ≥ −creditLimit`. Only the shared `held` gauge + per-request `hold:<reqID>` + window counters live in Redis. NO `:bal` key, NO balance-refresh job/cache.
- **Anchored, estimate-based windows.** Cadence = session|fixed (per-user-anchored, #337); the Lua stores the per-(payer,scope,window) anchor in Redis and buckets relative to it (`floor((now−anchor)/dur)` for fixed; session = one key, TTL=dur set at open, never refreshed). Windows count the ESTIMATE; **capture does NOT true the window up to actual** (only the caller-side balance/#512 ledger trues up); **release frees the estimate from the windows**. Tested: prepaid + arrears-credit floor, fixed-window reset across a bucket boundary (fake clock), idempotent replay, multi-scope deny-on-any, capture-keeps-window vs release-frees-window.
- **Wire contract:** clean break + lockstep (Paul) — the eventual admit/capture/release HTTP+SDK shapes may change freely; tensorhub + embedders move in lockstep.
`held`-gauge reconciliation sweep: DROPPED 2026-06-18 (do NOT build) — the Redis `held` gauge can drift on a crash/EVAL-loss, but per decision 8 (#512) bounded over-admission is acceptable and the durable #512 ledger is the truth, so a periodic reconciliation sweep is unnecessary complexity. Cadence also no longer exists (#517 made windows fixed-only), so the loader carries no cadence. DONE since the original draft: Postgres→`spendgate.Policy` loader (tier_policies + budget_policies → scoped windows), direct O(1) admission capacity read, and the admitter/`Service.{Admit,CaptureHold,ReleaseHold}` rewire that deletes `internal/modules/holds` + the `budgets` Postgres machinery + `money.AuthorizeAndHold`'s capacity read.

**WINDOW-ANCHORING GAP — found AND RESOLVED 2026-06-17.** The original draft `spendgate` bucketed windows by GLOBALLY-ALIGNED time, but the live budgets engine (`internal/modules/budgets/budgets_service.go`, #337) uses PER-USER-ANCHORED windows (each opens at the payer's first charge; staggered resets; 5h session / 7d fixed — SHIPPED behavior tensorhub/cozy-art rely on). FIXED in the rebuilt `gate.go`: the Lua stores the per-(payer,scope,window) anchor in Redis and buckets relative to it (`floor((now−anchor)/dur)` for fixed; session = one TTL-bounded key opened at first reserve). The integration test exercises the fixed reset across a bucket boundary. Cadence (session|fixed) maps straight from `BudgetWindowPolicy.Cadence`; the loader (TODO) carries it through. Per Paul, windows are estimate-based (no per-bucket capture true-up), which is what made the faithful port tractable. Until the wire lands, the budgets Postgres path stays the live engine.

SUBSUMES (hard-cut: delete on switch-over, do NOT coexist): `spendgate` replaces THREE current mechanisms that today split one decision — (1) the live Redis hold store `internal/modules/holds` (`Place`/`Release`/`ActiveAmount`, used by `pkg/service/service.go` + `admission.go`); (2) the Postgres money-capacity calc in `money.AuthorizeAndHold`; (3) the `budgets` Postgres window path (Phase A). All three collapse into the one Lua gate. After the cut, `internal/modules/holds` is deleted and `AuthorizeAndHold`'s capacity read is gone — there must NOT be two Redis-hold implementations (`holds.Store` + `spendgate`) running side by side.
- [x] Admit = one `EVAL`: check `account_balance − Σ holds ≥ cost` AND every window `used + reserved + cost ≤ limit`; if all pass, place the request-id hold + bump reserved. One Redis round trip after the O(1) DB capacity/config reads.
- [ ] Capture = one `EVAL`: move reserved→used by ACTUAL cost, drop the hold, and enqueue the durable spend (off hot path). 
- [ ] Fail/release = one `EVAL`: drop hold + reserved, bump the wasted-spend counter for abuse.
- [x] Holds in SHARED Redis keyed by the provider/tensorhub request-id (cross-instance agreement); no balance cache remains.

### E. Durable-spend seam (to #512, off the hot path)
RESOLVED 2026-06-17 (Paul): crash-loss of in-flight captures is acceptable — NO durable outbox. Keep it simple.
- [x] Capture releases the Redis hold and calls the #512 durable ledger writer (`CaptureAuthorized`). No balance cache or background drainer remains; no at-least-once outbox/WAL was added.

### F. money_windows (prepaid bulk reservation) — DELETE (verified zero consumers)
VERIFIED 2026-06-17: `money_windows` is API-exposed (#335: `POST /v1/service/credits/windows`, `/:id/refill`, `/:id/close`, settle) but NO consumer uses it — grep of tensorhub, doujins, cozy-art (incl. cozy-art's embedded in-process calls) found zero `OpenWindow`/`credits/windows` usage. Under the hard cut it's a dormant public API → remove it entirely, don't re-express it.
- [x] DONE 2026-06-18: deleted the credit-window API end to end — routes (`credits.POST /windows|/settle|/:id/refill|/:id/close`), handlers (`service_credit_window.go`), service (`pkg/service/windows.go`), money methods (`internal/modules/money/windows.go`), SDK interface + DTOs (`client.go`/`remote.go` `OpenWindow`/`SettleWindowItems`/`RefillWindow`/`CloseWindow` + `CreditWindow`/`OpenWindowRequest`/`WindowSettle*`), and the embed transcription (`embed/client.go` window block). Also removed the now-dead `HoldExpiryWorker` (durable money-hold sweep — holds are Redis-only per #505): `internal/river/jobs_hold_expiry.go` + its registration/periodic job + `hold_expiry_worker_test.go`. The kept admit-batch bound was renamed `maxWindowBatchItems`→`maxAdmitBatchItems` and re-homed (it had lived in the deleted credit-window handler).
- [x] DONE 2026-06-18: dropped `money_windows` via migration `012_drop_money_windows.up.sql` (frozen-`001`-baseline + new-drop-migration pattern, same as `004`/`005`/`007`; `001` and `merchant_aware_schema_test.go`'s table list keep it — sqlc reads all migrations so the final gen state has no `MoneyWindow` model). Removed its sqlc queries + `SumActiveMoneyHeld`; `deriveBalance`/`GetCreditAccount` now report `HeldBalance: 0` (held = spendgate Redis only).
- [x] DONE 2026-06-18: `go build ./...` + `go vet -tags integration ./...` clean; no orphaned `MoneyWindow`/`money_windows`/credit-window refs (conformance + reconcile + e2e tests updated). NOTE: the e2e `ledgerRows`/`rawBalanceRow` helpers still target the #512-dropped `money_transactions`/`money_blocks` — flagged in `db_helpers.go` as a #512 follow-up, NOT #513.

### G. Tests
- [ ] Admit/capture/release atomicity under concurrency (Redis EVAL, no lost updates, no double-spend).
- [ ] Cap-merge correctness across payer/invoker/role/tier scopes; deny when any window over.
- [~] Balance-cache staleness bound + deposit invalidation — N/A after 2026-06-18 cache deletion; funded payer is visible on the next direct O(1) ledger-account read.
- [ ] Redis-loss behaviour: caps relax, over-admission stays bounded, ledger stays correct.
- [ ] Migration: `budget_window_state`/`budget_inflight_holds` removed; in-flight reservations drained or expired cleanly.

## What stays (don't lose) — and a VERIFIED inventory of the three protections (2026-06-17)

The three protections spend limits exist for, mapped to their LIVE system (all SEPARATE from the dead `spend_policy.go`):
1. **Abuse (request velocity / low-trust)** — LIVE: edge rate-limit middleware mounted globally (`internal/http/server.go:473` `ginmw.RateLimitWithChallengeStore`, per-IP/route + captcha + attack-mode), card-abuse guard (`abuse/card_abuse.go`), velocity guard (`abuse/velocity.go`), wasted-spend guard (`abuse/wasted_spend.go`, in `admitter.Admit`), and the low-default tier for new accounts (#300).
2. **Fairness (rich payer can't crowd out others)** — RESOLVED 2026-06-17, NOT openrails' job: fleet fairness is owned by the CONSUMER'S SCHEDULER, not openrails. Verified in tensorhub (`~/cozy/tensorhub/internal/orchestrator/grpc/fairness.go`): a work-conserving weighted-fair-queue dispatches the shared GPU fleet — L1 per-payer / L2 per-user nesting, a per-payer in-flight **$** soft cap that scales by platform tier, weighted by the OpenRails hold ESTIMATE (#486), never idles a slot, never rejects. OpenRails owns the HARD money decision (affordability/hold) + $-budget windows + abuse; tensorhub owns scheduling/reordering and the live in-flight tally. This is the right split — openrails can't fairly schedule a fleet it doesn't see. CONSEQUENCE: openrails' throughput/concurrency surface (`ThroughputPolicy` written-not-read, `AcquireConcurrency` zero callers, the `ratelimit/concurrency.go` primitive) is dead-on-PURPOSE — tensorhub explicitly stopped sending throughput units (`invoke_admission.go:66`). Do NOT wire it; REMOVE it (same treatment as `spend_policy.go`), and document the contract: openrails = money truth + $-budget + abuse; the consumer's scheduler owns fleet fairness. The edge per-IP limiter stays as the DoS backstop for schedulerless consumers (doujins/cozy-art/direct API).
3. **Delegated spend-control (cap what another invoker spends + how often)** — LIVE: `budget_policies` scope=invoker/role/invoker_tier windowed `$` caps, built in `buildBudgetScopes` and reserved in `admitter.Admit`. This is strictly MORE general than the dead per-invoker day/month caps. (Subject-wide windowed cap = the payer's own velocity limit, distinct from balance: exists as config `ScopeSubject`/tier BudgetWindows but `buildBudgetScopes` skips subject — verify whether it's enforced anywhere on admit, or fold it into the one shape.)

Keep through the simplification:
- Hierarchical delegated-spend SEMANTICS (an invoker may spend the payer's money up to a cap) — keep; only the accounting + policy-shape change.
- FX conversion for cross-currency policy comparison (`internal/integrations/fx`) — pure function on the estimate.
- Tier graduation from cumulative paid (`tier_schedules`) — background; selects the policy.
- Wasted-spend abuse gate — already Redis; it becomes one consumer of the unified primitive.
- [x] REMOVE the dead fairness/throughput surface from openrails (NOT wire it — see protection 2): DONE 2026-06-17. Deleted `ratelimit/concurrency.go` (`AcquireConcurrency`/`ReleaseConcurrency`/`ConcurrencyCount`, zero callers) + `ratelimit/units.go` (#472 weighted queue pools `AcquireUnits`/`AcquireQueue`/`ReleaseQueueByRequest`, zero callers) + both integration tests; renamed `models.ThroughputPolicy` → `models.TierMoneyPolicy` (the misnomer carried no throughput axis). `AdmitRequest` had no throughput-unit field to drop. Fleet fairness is the consumer scheduler's job (tensorhub WFQ). `valuewindow.go` (wasted-spend `AddWindowValue`/`WindowValue`/`ClaimOnce`) + `limiter.go` `Check` (edge rate-limit) are LIVE and kept.

## Non-goals

- Not changing WHAT the caps mean or the delegated-spend model — only where state lives and how it's checked.
- Not building the durable ledger here (that's #512); this issue only enqueues to it.

## HARD CUT — no legacy, no backward compatibility (decided 2026-06-17, Paul)

Move to the new system completely; do NOT leave old code or compat shims around.
- NO feature flag toggling old-vs-new admission; NO dual-write / shadow mode; NO parallel "Postgres-budget AND Redis-budget" running side by side. One path, the new one.
- DELETE, don't deprecate: `budget_window_state`, `budget_inflight_holds`, the `budgets.Reserve/ReserveScopes/Capture/Release` locked-tx machinery, `spend_policy.go` enforcement + `money_spend_limits`, and the dead throughput/concurrency surface (`ThroughputPolicy`, `ratelimit/concurrency.go`) all go.
- NO on-the-wire backward-compat window on the admit/capture contract: if the API shape changes, change it outright. Consumers (tensorhub, doujins, cozy-art) move in LOCKSTEP via a coordinated openrails version bump — same model as the #403/#404 hard cut that already deleted the legacy broker.
- Exit gate: after the cut, `grep` finds no reference to the deleted tables/types/functions anywhere (no orphaned readers, no commented-out fallbacks); `go build ./... && go vet ./...` clean.

## Lockstep cutover (consumers) — verified 2026-06-17

Hard cut = every consumer moves to the new contract in ONE coordinated openrails version bump; no compat window, and APIs with zero consumers are deleted outright. Verified coupling + surface each consumer touches:

- **tensorhub** — standalone HTTP/SDK client (openrails SDK pinned, currently v0.36.0). Calls the USAGE surface: `AdmitHold` (admit), capture-on-job-result, `ReportWastedSpend`, `/credits/deposit`, `credit-types`, `/billing/v1/self/*`. **Most affected** — the admit/capture/wasted contract changes under #513. Lockstep: bump SDK, update the admit/capture/release/wasted call sites in `internal/orchestrator/http/invoke_admission.go` + `internal/billing/openrailsclient`.
- **doujins** — EMBEDS openrails in-process (`go.mod` `open-rails/openrails v0.39.0`; mounts routes in `internal/server/server.go` + `internal/di/builder.go`). Uses the SUBSCRIPTION surface, not admit/holds/windows/spend-limits (grep clean). Lockstep: bump the module dep + recompile — a compile break is the signal if it touches any removed symbol; subscription API is unchanged by #512/#513.
- **cozy-art** — EMBEDS openrails in-process; frontend hits `/billing/v1/me/subscriptions|payments|payment-methods`, `/checkout`, `/products`, `/stripe/portal`. Subscription-only; does NOT touch admit/holds/credits. Lockstep: bump the module dep + recompile; frontend endpoints unchanged.
- **Zero-consumer APIs being deleted — break nobody:** the credit-window API (#335 / `money_windows`) and the spend-limit API (`SetSpendLimit`/`money_spend_limits`) have NO consumer in any of the three repos → delete with no consumer migration.
- **#512 (ledger) is internal** — the consumer-facing deposit/charge/balance/subscription wire shapes (amounts already minor units) should NOT change; verify the API responses are byte-stable so #512 needs no consumer change beyond the embedded-dep bump.

## Efficiency (before → after)

- Admit: ~3 PG reads + 2 PG txns w/ `FOR UPDATE`  →  1 Redis `EVAL` + in-memory policy/balance.
- Capture: 1–2 locked PG txns  →  1 Redis `EVAL` + async durable enqueue.
- Per-payer concurrency: serialized by 2 row locks  →  lock-free.
- Cap surface: 4 policy shapes + 2 PG accounting tables  →  1 policy shape + 1 Redis counter primitive.

## References

- Current code: `internal/modules/admission/admitter.go`, `internal/modules/budgets/{budgets_service,scopes}.go`, `internal/modules/abuse/wasted_spend.go` (the Redis pattern to copy), `internal/modules/money/{authorize,unified_spend,windows,spend_policy}.go`, `pkg/service/{admission,spend,windows}.go`.
- Tables: `budget_window_state`, `budget_inflight_holds`, `budget_policies`, `tier_policies`, `tier_schedules`, `money_spend_limits`, `money_settings`, `money_windows`.
- Relationship: realizes decision 8 of #512 across the whole admission path; #512 Phase C defers here for the admission/holds detail.

---

# #330: nmi-immediate-subscription-checkout-stuck-pending

**Completed:** no
**Status:** IN_PROGRESS 2026-06-08: immediate NMI checkout activation patch is implemented; package tests and focused mock-provider regression pass; configured-account Mobius recurring test has been added and compiles/skips locally because `MOBIUS_PRODUCTION_KEY` is not set. Remaining work: run real Mobius credential test and repair/replay Paul2 after fixed code is deployed.

Fix the NMI/Mobius subscription checkout path where an immediately approved recurring checkout is persisted as pending, leaving host apps such as Doujins stuck on the pending-subscription screen even though NMI accepted the transaction.

## Metadata

- Category: bug
- Status: in_progress
- Passes: false

## Live findings from Doujins/Paul2 on 2026-06-08

- OpenRails received the checkout request and `POST /v1/self/checkout` returned HTTP 200.
- NMI/Mobius accepted the card vault and transaction: payment vault `314825442`, provider transaction `12162933364`, provider subscription `12162933429`.
- Local checkout session `019ea96b-221b-7274-a341-8cdc85cb72d6` was marked `succeeded` with processor `mobius` and amount `2300 usd`.
- Local subscription `019ea96b-223b-75b1-926b-35956d10eba5` stayed `pending`, with no current period start/end timestamps.
- Local payments only had a pending attempt row keyed as `nmi_sub_attempt:sub_cb204b034d2c3ec46be93b0470ff44df`; there was no completed payment row keyed by the real NMI transaction id.
- No billing entitlement rows were created for the user, so Doujins correctly kept showing the pending-subscription state.

## Root cause

`processNMISubscription` called `AddRecurringSubscription` successfully, but `completeNMISubscriptionRegistration` intentionally created a local pending subscription and returned a pending checkout response. The initial NMI response was not used to synchronously activate an immediate subscription, set the current billing period, create a completed payment, or grant entitlements. Webhooks should not be required for the initial happy path when NMI immediately approves the transaction; delayed/future starts can remain pending.

## Desired behavior

For immediate NMI/Mobius subscription approvals, OpenRails should finish the local checkout atomically from the direct provider response: mark the subscription active, set current period timestamps, record the completed payment against the real provider transaction id, grant entitlements, and return a succeeded checkout response. Delayed-start subscriptions and genuinely asynchronous provider states should remain pending and rely on follow-up provider events/reconciliation.

**Tasks:**
- [x] Capture live-stack evidence for the Paul2 failure: NMI accepted the transaction, checkout succeeded, subscription stayed pending, payment stayed as a pending attempt, and no entitlement was granted.
- [x] Identify root cause in the NMI checkout finalization path.
- [x] Patch immediate NMI subscription finalization to activate the subscription, set period dates, create the completed payment, and grant entitlements without waiting for a webhook.
- [x] Preserve pending behavior for delayed/future-start NMI subscriptions.
- [x] Fix stale integration-test helpers that still query billing tables by `user_id` instead of `tenant_subject_id`.
- [x] Add/validate focused integration coverage proving an immediate NMI subscription checkout grants the premium entitlement synchronously.
- [x] Add an actual NMI test-account integration path so the live provider contract is exercised, guarded by Mobius/NMI test credentials.
- [ ] Run the actual Mobius/NMI configured-account recurring-subscription integration with `MOBIUS_PRODUCTION_KEY` set.
- [x] Run focused OpenRails checkout/module tests and NMI regression tests.
- [ ] After deployment/restart, repair or replay the affected live pending subscription row for Paul2 if it still exists.

---

# #328: robinhood-coinbase-usdc-funding-sessions

**Completed:** no
**Status:** PARTIAL 2026-06-08: Implemented Solana-only USDC funding session APIs, persistence, config, Coinbase hosted-session adapter with CDP JWT auth, Coinbase Hook0-signed Onramp webhook/status ingestion, Robinhood launch-template handoff, provider eligibility gates, self-service routes, idempotency, structured insufficient-USDC funding context on checkout errors, backend Solana USDC balance verification, focused tests, and DB-backed self-service API tests for create/get, tenant/user isolation, idempotency, and unsupported provider/network rejection. Retained for future provider integration work; current Doujins UX uses manual Robinhood/Coinbase links plus connected-wallet balance checks instead of OpenRails provider sessions. Remaining: real Robinhood partner adapter/status docs and access.

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
- [x] Decide how provider ranking is configured per tenant: Robinhood preferred, Coinbase fallback. Implemented default provider order with Robinhood first and Coinbase second.
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
- [x] Enforce self-service auth, tenant boundaries, and idempotency on funding-session create/read routes.
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
- [x] Add API tests for create/get funding session, tenant isolation, idempotency, and unsupported-provider/network rejection.
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

# #118: admin-dashboard-metrics-overhaul

**Completed:** no

Overhaul admin dashboard metrics to provide useful business intelligence

## Metadata

- Category: feature
- Status: not_started
- Passes: false

## Details

- api_design: {"dashboard_summary":{"endpoint":"GET /v1/admin/metrics/summary","returns":"Key KPIs for dashboard cards (MRR, active subs, churn rate, etc.)"},"revenue_over_time":{"endpoint":"GET /v1/admin/metrics/revenue?start=DATE&end=DATE&granularity=day|week|month","returns":"Time series of revenue data"},"subscriptions_over_time":{"endpoint":"GET /v1/admin/metrics/subscriptions?start=DATE&end=DATE&granularity=day|week|month","returns":"Time series of subscription counts"},"churn_analysis":{"endpoint":"GET /v1/admin/metrics/churn?start=DATE&end=DATE","returns":"Churn breakdown by reason, cohort retention"},"processor_breakdown":{"endpoint":"GET /v1/admin/metrics/processors","returns":"Revenue and counts by processor"}}
- implementation_notes: ["MRR calculation: sum(price.amount / (price.billing_cycle_days / 30)) for active subs","Need to handle different billing cycles (monthly, quarterly, annual)","Solana one-time payments are not subscriptions - track separately","Consider caching expensive aggregations (refresh every 5 min)","Use PostgreSQL window functions for period-over-period comparisons","May want ClickHouse for historical analytics at scale"]
- problems: ["Limited metrics - only subscription counts, no revenue or business KPIs","Confusing naming - 'without_auto_renew' actually means 'cancelled but still in period'","No revenue metrics - MRR, ARR, total revenue, average revenue per user","No churn metrics - churn rate, retention rate, LTV","No Solana support - processor metrics exclude Solana one-time payments","No period comparisons - can't compare this week/month vs previous","No cohort analysis - can't track retention by signup month","No conversion funnel - signups to first payment to recurring","Tests are minimal - just verify response structure, not actual data accuracy"]
- proposed_metrics: {"revenue_metrics":["MRR (Monthly Recurring Revenue) - sum of active subscription monthly values","ARR (Annual Recurring Revenue) - MRR * 12","Total revenue this period - sum of all payments in date range","Revenue by processor - breakdown by CCBill, NMI, Solana","ARPU (Average Revenue Per User) - total revenue / active users"],"subscription_metrics":["Total active subscriptions","New subscriptions this period","Cancelled subscriptions this period (with breakdown by reason)","Net subscriber change (new - cancelled)","Subscriptions by status (active, past_due, cancelled)","Subscriptions by product/tier"],"churn_metrics":["Monthly churn rate - cancelled / active at start of month","Voluntary churn - user/merchant initiated","Involuntary churn - payment failures","Retention rate by cohort - % of month-N signups still active"],"payment_metrics":["Successful payments this period","Failed payments this period","Payment success rate","Refunds issued","Chargebacks received","Average payment amount"],"one_time_purchases":["Solana payments count and value","One-time purchase revenue (non-subscription)"]}

**Tasks:**
- STEPS:
- [ ] Define exact metrics needed for admin dashboard UI
- [ ] Design API response formats for each endpoint
- [ ] Implement MRR/ARR calculation logic
- [ ] Implement churn rate calculation
- [ ] Add revenue aggregation queries
- [ ] Add one-time purchase tracking (Solana)
- [ ] Add period comparison support (vs last period)
- [ ] Add caching layer for expensive queries
- [ ] Update existing metrics endpoints or create new ones
- [ ] Write comprehensive tests with seeded data
- [ ] Document all metrics definitions

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

Add deterministic processor selection and fallback policy so checkout can choose the best available rail for a tenant/product/tier/user context before creating a one-processor checkout session.

## Metadata

- Category: feature
- Status: planned
- Passes: false

## Motivation

- Hyperswitch wins on payment orchestration. OpenRails should not chase broad smart-routing, but customers do need basic processor redundancy and product/tenant-aware routing, especially high-risk merchants with Stripe plus NMI/Mobius/CCBill/Solana.
- Keep checkout sessions one-processor-at-a-time; route before session creation.

## Non-goals

- No ML routing.
- No 200-connector orchestration platform.
- No automatic retry to a second processor after a successful authorization attempt unless the processor contract makes that safe.

**Tasks:**
- DESIGN:
- [ ] Define routing inputs: tenant_id, product_id, price_id, tier_group, amount, currency, billing cycle, user country/state when known, processor availability, processor capability metadata (#291), and explicit client preference.
- [ ] Define routing outputs: selected processor, fallback candidates, reason, and policy version.
- [ ] Decide precedence: explicit price/provider config > tenant policy > product/tier_group policy > global default.
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
- [ ] Add dry-run endpoint/CLI to explain routing for a price/user/tenant without creating a checkout.
- [ ] Ensure idempotency keys bind to the resolved processor/policy so retries do not unexpectedly switch rails.
-
- VERIFY:
- [ ] Unit-test policy precedence and fallback filtering.
- [ ] Integration-test Stripe primary -> Mobius fallback when Stripe is disabled/unavailable before charge.
- [ ] Integration-test product constrained to Mobius does not route to Stripe even if Stripe is configured.

---

# #290: provider-certification-matrix

**Completed:** no

Publish and maintain a provider certification matrix for Stripe, NMI/Mobius, CCBill, and Solana that records exactly which customer-visible flows are supported, sandbox/devnet tested, live/test-mode tested, and known-limited.

## Metadata

- Category: product
- Status: planned
- Passes: false

## Motivation

- OpenRails' strongest differentiator is deep support for real non-Stripe rails, especially NMI-compatible high-risk gateways like Mobius. Customers need confidence that the specific flows they care about actually work.
- This should become both documentation and an executable certification harness where practical.

**Tasks:**
- MATRIX DESIGN:
- [ ] Define provider capabilities and certification statuses: not_supported, manual_only, unit_tested, integration_tested, sandbox_certified, live_test_mode_certified, devnet_certified, production_certified.
- [ ] Define flows to track: catalog/product push, price/recurring-plan push, one-time checkout, recurring checkout, vault/tokenization, rebill, cancellation, deferred cancellation, refund, dispute/chargeback, webhook handling, subscription sync/backfill, catalog drift detection.
- [ ] Include processor-specific notes: NMI product/prices are local while recurring prices push as NMI recurring plans; CCBill catalog actions may be manual; Solana recurring requires on-chain readback/devnet certification.
-
- DOCS:
- [ ] Add docs/providers.md or equivalent with the current matrix and exact tested commands.
- [ ] Add NMI-compatible gateway guidance: required security_key, Collect.js/tokenization key if needed, direct/query endpoints, test_mode behavior, and how Mobius/NMI white-label accounts map to the same interface.
- [ ] Add troubleshooting for common provider failures: bad endpoint, key belongs to different gateway user, sandbox URL not supported, recurring plan query mismatch, webhook signature failure.
-
- EXECUTABLE CERTIFICATION:
- [ ] Add or formalize focused integration tests for NMI/Mobius sale, vault, recurring plan create/readback, and query API.
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

- Stripe, NMI/Mobius, CCBill, and Solana have different lifecycle semantics. Customers need predictable errors and routing decisions. OpenRails also needs a shared capability source for routing/fallback, catalog-as-code, checkout validation, and the provider certification matrix.

**Tasks:**
- CAPABILITY MODEL:
- [ ] Define ProcessorCapabilities with booleans/enums for recurring, one_time, vault/tokenization, hosted checkout, redirect checkout, direct sale, catalog push, recurring plan push, refund, dispute, cancel immediate, cancel deferred, remote subscription listing, remote dedup check, webhooks, drift enumeration, and manual actions.
- [ ] Add capability details for current processors: stripe, mobius/NMI-backed, ccbill, solana.
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

This should integrate with the provider capability and routing work in #288, #290, and #291: Hyperswitch can be selected as a processor/vault provider when the tenant/provider config says it is available, and OpenRails can route checkout/setup flows through it without treating it as a generic secret vault.

**Tasks:**
- [ ] Research the current Hyperswitch Cloud and self-hosted API surfaces needed for customers, payment methods, payment/setup intents, saved payment methods, refunds/voids, webhooks, connector routing, and vault/tokenization modes; record any version assumptions in docs.
- [ ] Reconcile the implementation plan with future issue #297: document whether Hyperswitch is an optional provider, a lower-risk deployment choice, or a PCI-heavy break-glass/self-hosted vault path; do not make it the default portable adult/high-risk vault without explicit certification.
- [ ] Define the OpenRails provider config shape for `hyperswitch`: cloud vs self-hosted mode, API base URL, optional vault/base URL split if Hyperswitch requires it, merchant/profile/account identifiers, API key secret reference, webhook secret reference, return/callback URLs, and test/live mode.
- [ ] Store Hyperswitch credentials in the existing tenant secret store path; do not store processor API keys in bootstrap YAML, database rows, logs, or generated frontend config.
- [ ] Extend provider capability metadata (#291) so Hyperswitch can advertise supported flows: payment-method vault/tokenization, one-time checkout, recurring/setup/mandate behavior if supported, refunds, webhooks, processor-side routing, remote payment-method listing, and manual certification status.
- [ ] Add a provider adapter/client abstraction that lets NMI customer-vault IDs and Hyperswitch payment-method IDs fit the same OpenRails payment-method model without hard-coding NMI semantics into checkout/subscription flows.
- [ ] Implement Hyperswitch customer/payment-method setup flow using an opaque token or hosted/client-side Hyperswitch collection result; persist only tenant subject, provider, external customer ID, external payment method ID, brand/last4/expiry metadata, and status.
- [ ] Implement checkout/subscription charge paths that can use a saved Hyperswitch payment method and reconcile the resulting payment/subscription state back into OpenRails records.
- [ ] Implement webhook verification and event handling for successful payment, failed payment, refund/void, payment-method updates/deletes, and any recurring/mandate lifecycle events OpenRails relies on.
- [ ] Add self-hosted operational docs: required Hyperswitch services, base URL/TLS requirements, webhook reachability, secret injection, health checks, and local compose/dev smoke path if practical.
- [ ] Add cloud operational docs: required Hyperswitch Cloud credentials, webhook setup, provider certification checklist entry (#290), and tenant bootstrap/provider-link examples.
- [ ] Add tests with a mocked Hyperswitch server/client for tokenization/setup, saved-method charge, failure mapping, webhook signature verification, idempotency, tenant isolation, and no-sensitive-card-data persistence/logging.
- [ ] Validate with focused Go tests for provider/checkout/vault/webhook code, compile-only full package coverage, `task build`, and an optional live sandbox/self-hosted smoke test when credentials or local Hyperswitch are available.

---

# #331: Tune card-abuse rate-limit windows (#371): 15-min 3->captcha/6->block, 24h 10->block per user+ip, keep global 100->attack-mode

**Completed:** no

The #371 CardAbuseGuard (internal/modules/abuse/card_abuse.go), middleware subject-key plumbing, checkout hook, and integration tests already landed on master. This refines the policy to the agreed windows. Card-testing is defined by volume over time, so pair a short BURST window with a rolling DAILY cap, both keyed per user AND per IP. Final thresholds (user-specified): a 15-MINUTE rolling window per (user+ip) where 3 failed card charges -> future attempts require captcha, and 6 failed charges -> cut off (blocked) for the remainder of that 15-min window; PLUS a 24-HOUR rolling cap of 10 failed charges per (user+ip) -> cut off for the day. Keep the existing site-wide GLOBAL cap of 100 failed charges / 24h -> attack mode (captcha for every request from everyone).

**Tasks:**
- [ ] DefaultCardAbuseConfig: short window 15 min, CaptchaAfter=3, BlockAfter=6 (block lasts the remainder of the window)
- [ ] Add a second per-subject rolling 24h window with a cap of 10 failed charges -> block; the Limiter already returns multiple windows, so configure the daily window + add the second-window check in RecordChargeFailure
- [ ] Keep both subject keys (user: and ip:) so a logged-in attacker rotating cards and a bot rotating accounts behind one IP are both caught
- [ ] Keep the global 24h/100 attack-mode (captcha for everyone) unchanged
- [ ] Extend the abuse integration tests (real Redis) to cover the 15-min 3/6 thresholds and the 24h/10 daily cap; go build ./... + go vet ./... clean

---

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

---


# #340: e2e: 5 tier/lifecycle tests fail on master (pre-existing, surfaced by #334 verification)

**Completed:** no
**Status:** open (filed 2026-06-10 during #334 verification)

TestTierGroupDetection, TestEntitlementChangesOnTierChange, TestScheduledDowngrade, TestRenewMembershipDuplicateTransactionIsNoOp, TestLifecycleServiceUsesMockClock fail identically on origin/master (bun era) and on the sqlc-migration branch — verified via worktree runs on 2026-06-10 during #334 final verification, so they are NOT migration regressions. Observed modes: duplicate key on uq_subscriptions_tenant_subject_tier_group_active across subtests (CleanupSubscriptionsForUser resolves non-UUID test user ids to uuid.Nil via identity.TenantSubjectIDFromString, so per-user cleanup deletes nothing), and mock-clock/lifecycle assertion failures. Distinct from the admin_* family (#333). Suspect shared-suite state + the broken per-user cleanup; fix the cleanup to resolve the tenant subject through billing.tenant_subjects (issuer 'openrails:legacy-user') instead of pure-parsing.
