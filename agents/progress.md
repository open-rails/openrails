<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 474

---

# #473: hierarchical money budget scopes — platform→payer caps (us) + payer→role/invoker caps (the tenant), composed in ONE admit verdict over ONE balance

The money axis (payer-protection, tensorhub #475) has a HIERARCHY of limits set by TWO different
authorities. This issue makes the OpenRails budgets engine express all of them, evaluate them
together in one admit, and keep them editable only by their rightful owner.

## Terminology (OpenRails vs tensorhub vocab)
- **tenant** = the host platform (= tensorhub). Top RLS isolation.
- **subject** (tenant_subject / payer) = the billing principal that holds the credit balance — a
  tensorhub TENANT like cozy-art, or a native user (#437). "payer" below = subject.
- **actor** = the invoker that made the request (a delegated-user like PaulFidika; for a native-user
  payer, actor == subject).
- **role** = a subject-defined grouping with a granted spend budget, assigned to the subject's users.

## The hierarchy (who caps whom)
1. **Platform → payer** (set by US / the tenant=tensorhub): cap each payer's TOTAL spend. Protects
   us + the payer from unpayable debt (arrears exposure; tensorhub #475 scope B / problem #3). Keyed
   on `(subject)`, OWNER = platform.
2. **Payer → role** (set by the SUBJECT, e.g. cozy-art): a shared budget pool for all users holding a
   role. Keyed on `(subject, role)`, OWNER = subject. NEW scope.
3. **Payer → invoker** (set by the SUBJECT): per-delegated-user cap by plan. Keyed on `(subject, actor)`,
   OWNER = subject. ALREADY EXISTS.
(A subject may also self-cap its own total: `(subject)`, OWNER = subject — distinct from the
platform's `(subject)` cap.)

## Design — generalized scope + owner, composed
Model a budget policy as **{scope, owner, windows[]}**:
- `scope` ∈ { `subject` | `actor` | `role` } (extensible). The window's effective key is the
  subject uuid, plus the actor uuid or role uuid for those scopes.
- `owner` ∈ { `platform` | `subject` } — determines WHO may write it and that the subject can
  neither see nor loosen a platform-owned window.
- `windows[]` = the fixed-interval windows (5h/1w/30d, #401) with LimitMicros + cadence.

**Admit evaluation (composition):** for a request `(subject, actor, roles[])` gather every matching
budget window — platform `(subject)`, subject `(subject)`, each `(subject, role)` the actor holds,
`(subject, actor)` — RESERVE against ALL of them, and DENY if ANY would breach (whichever-first,
return the blocking window + retry hint). On settle, capture/release ALL reserved windows.

**Key invariants:**
- Budgets are GATES, not wallets. A `(subject, role)` cap of $100/wk does NOT give the role its own
  $100 — it means users with that role may collectively spend ≤ $100/wk OF THE SUBJECT'S BALANCE.
  Sub-budgets MAY over-subscribe (Σ caps > balance); the payer BALANCE is the hard floor.
- ONE balance debit per request (the subject's), regardless of how many windows gate it. Windows
  track reserved/used independently; they do not each debit.
- The subject CANNOT edit or loosen a platform-owned window (enforced by owner + a separate write
  path / authz; not exposed in the subject's policy doc).
- Identity: all window keys are immutable UUIDs (subject, actor, role), never slug/username (#475/#476
  convention).

## What exists / what's new
- EXISTS: `(subject, actor)` budget windows + reserve/capture/release lifecycle (#304/#337); fixed
  cadence (#401). #306 already lists the "tenant→owner limit level" (= platform→payer, level 1) and a
  tensorhub→OpenRails policy-ownership migration as future bullets — this issue SUBSUMES and details
  those, and adds the role scope.
- NEW: the `role` scope key `(subject, role)`; the `owner` (platform|subject) discriminator + a
  platform-writable policy store separate from the subject's editable policy; multi-window admit
  composition across scopes in one verdict; role(s) carried on the admit request.

## Tasks
- [x] Generalize the budget policy to `{scope, owner, windows[]}`; add the `role` scope and the
      `(subject, role)` key; migration + RLS. — DONE: `budgets.ScopeReservation`/`ReserveScopes` (internal/modules/budgets/scopes.go) key each scope into the existing opaque `actor` column (`@subject`, `@role:<uuid>`, actor string — no data migration); migration 015_budget_policies.up.sql adds the `budget_policies` table (scope/owner/scope_key/windows JSONB, RLS tenant_isolation, FK to tenant_subjects).
- [x] `owner` discriminator + a PLATFORM-owned policy store (writable only by platform admins, NOT
      via the subject's policy doc); subject-owned policies stay where they are. — DONE: `admission.BudgetPolicyStore` + `pkg/service` `SetPlatformBudgetPolicy`/`SetSubjectBudgetPolicy` (owner forced per path) + `SubjectBudgetPolicies` (LoadByOwner=subject hides platform rows).
- [x] Admit composition: evaluate platform `(subject)` + subject `(subject)` + every `(subject, role)`
      + `(subject, actor)` in one verdict; reserve all, deny on first breach, return blocking window. — DONE: `Admitter.buildBudgetScopes` + `budgets.ReserveScopes` (all-or-nothing, one tx, key-ordered lock); `AdmitDecision.BudgetScopes` carries per-scope state incl. the breached window.
- [x] Settlement: capture/release ALL reserved windows for a request (idempotent on request_id);
      single balance debit unchanged. — DONE: `budgets.CaptureByCoords`/`ReleaseByCoords` (settle every scope by (payer,source,source_id)); wired into `pkg/service` CaptureHold/ReleaseHold (credits.ReleaseHold now returns the trx); the ONE ledger debit is unchanged.
- [x] Admit request carries the actor's role(s) (from the host; tensorhub reads them from the
      delegated JWT/permission set). — DONE: `AdmitRequest.Roles []uuid.UUID` + `service.AdmitInput.Roles`.
- [x] VERIFY: platform cap denies even when role+invoker are under; role pool shared across two users
      of the same role; invoker cap independent; over-subscription (Σ caps > balance) still floored by
      balance; subject cannot raise the platform cap; whichever-first blocking-window reporting. — DONE: internal/modules/admission/admission_scopes_integration_test.go (4 tests, all PASS): platform-cap-denies, role-pool-shared, invoker-independent, over-subscription-composes. Subject-cannot-raise-platform is enforced by owner-forced write paths (SetSubjectBudgetPolicy can never write owner=platform).
- [x] Expose budget-policy management on the UNIFIED `openrails.Client` so hosts (tensorhub) that
      talk only through the unified client can push role/subject/platform budget policies (previously
      only `SetTierPolicy` was exposed). — DONE: `Client.SetSubjectBudgetPolicy`/`SetPlatformBudgetPolicy`/`SubjectBudgetPolicies`
      + SDK DTOs (`SubjectBudgetPolicyInput`/`PlatformBudgetPolicyInput`/`SubjectBudgetPolicy`/`BudgetScopeWindow`,
      `role_id`->facade `ScopeKey`) in client.go, mirroring `SetTierPolicy` across all 3 transports:
      remote HTTP (`PUT/GET /v1/service/budget-policies/{subject,platform}` in remote.go), embedded
      adapter (embed/client.go transcribing the handlers), and HTTP handlers/routes
      (`ServiceSetSubjectBudgetPolicy`/`ServiceSetPlatformBudgetPolicy`/`ServiceGetSubjectBudgetPolicies`;
      subject=credits:write/read, platform=openrails:admin gated). Dual-transport round-trip added to the
      embed conformance test (self + role + platform write, subject-only read-back; platform cap stays
      invisible) — PASS. v0.24.0.

## Open decisions
- **Multiple roles per user:** enforce ALL of the user's role-budget windows (conservative,
  whichever-first) vs a single governing role. RECOMMEND enforce-all; revisit if surprising.
- **Level-1 timing:** the platform→payer cap is mooted today by prepaid-only (balance is the cap);
  build the scope/owner machinery now, wire the actual platform cap when arrears ships (#475 scope B).
- **Policy store shape:** extend `tier_policies` JSONB with scope/owner, or a dedicated
  `budget_policies` table keyed (subject, scope, key, owner). Lean dedicated table for the
  platform-vs-subject write-authz split.

## Related
tensorhub #475 (payer-protection — the consumer; scope A = roles/invoker, scope B = platform cap),
#476 (capacity, separate axis), #437 (billing principal = subject), #306 (supersedes its tenant→owner
+ policy-ownership bullets), #304/#337/#401 (budgets engine + fixed windows).

---

# #472: full rate-limit dimension support (OpenAI-shaped) — per-(payer × endpoint-release) throughput limit VALUES + batch-queue (weighted, release-on-completion) reservation

OpenAI publishes rate limits across several dimensions (e.g. gpt-5.5: TPM, images/min, videos/min,
requests/min, + a batch queue limit; gpt-realtime-2: RPM + TPM). OpenRails is the general throughput
engine (#298) and should express ALL of these. UNIT NOTE: OpenAI keys its tables per *model*; OUR
rate-limit unit is the **endpoint-release**, NOT the model — an endpoint has many releases, and a
release contains multiple models + functions inside it. So never call this "per-model"; it's
per-(payer × endpoint-release). Most dimensions already work; two pieces don't.

## Already supported (confirm — no build)
- Arbitrary-unit fixed windows: `ratelimit.Limit{Unit, Window, Max}` + `Policy.Windows`, atomic Lua,
  whichever-window-first deny (`internal/modules/ratelimit/limiter.go`). So RPM/RPD, TPM/TPD,
  images/min (IPM), videos/min (VPM), gpu-second/min are ALL expressible today — `video` is just
  another unit string; "per minute" = `Window: time.Minute`.
- Per-(tenant, subject, actor, resource) counter namespacing — `resource` = the endpoint-release
  (`internal/modules/admission/admitter.go:148`).
- Concurrency cap (count-based in-flight): `AcquireConcurrency/ReleaseConcurrency/ConcurrencyCount`
  (`internal/modules/ratelimit/concurrency.go`).
- Per-window post-check state for `x-ratelimit-*` headers (`ratelimit.WindowInfo`).

## Gaps to build

### G1 — throughput rate-limits scoped per (PAYER × ENDPOINT-RELEASE), with per-release limit VALUES
Owner steer (Paul): the rate-limit unit is **(payer × endpoint-release)** — NOT per invoker, per
function, or per model. (An endpoint has many releases; a release holds models+functions; the
RELEASE is the unit.) Concretely:
- **KEY**: throughput counters key on **(payer, endpoint-release)** only. Today the admitter bucket
  is `tenant:subject:actor:resource` (`internal/modules/admission/admitter.go:148`) — it includes
  `actor` (the invoker), which over-subdivides. Drop `actor` from the THROUGHPUT key so a payer's
  limit AGGREGATES across all its invokers; `resource` = the endpoint-release (the host's `releaseID`),
  NOT model or function. **WHY payer, not invoker:** per-invoker throughput is BYPASSABLE — a payer
  can register an unlimited number of invokers/accounts to multiply its limit, so a capacity limit
  MUST be payer-scoped to be abuse-resistant. MONEY can stay per-invoker (#475) because money is real
  and cannot be duplicated infinitely (the per-payer money cap is the true backstop). This change is
  the throughput axis only.
- **VALUES**: limits are per-release, not one tier-global list. `ThroughputPolicy.Windows` is
  currently a single global list (`internal/db/models/tier_policy.go`); add per-release window values
  (a `map[release][]ThroughputWindow`, or a default list + per-release overrides).
- **SOURCE**: since throughput is now payer×release (not the invoker's tier), the limits come from a
  payer/release-level policy, not the invoker's `tier_policy`. OPEN (decide in build): keep a default
  + per-release override, or move throughput limits onto a dedicated (payer, release) policy table.
  `EntitledResources` gating unchanged.
- **IDENTITY CONVENTION** — the throughput KEY and all persisted records use the immutable UUID
  (payer = tenant_subject uuid; release = endpoint-release uuid / `releaseID`), NEVER a tenant-slug
  or endpoint NAME. Today the admit resource is `tenant/endpointName` (`admitter.go:148`) — confirm
  the host passes the release uuid, not a mutable name; a slug/name key lets a rename reset counters
  or a recycled name inherit another payer's limits. Public API shapes may use slugs, resolved to
  uuid before the admit. (Builds on the fleet uuid-canonical work; matches tensorhub #475/#476.)

### G2 — batch queue limit = weighted, release-on-completion reservation over arbitrary units
OpenAI's batch queue limit: "tokens from pending batch jobs count against your queue limit; once a
job completes, its tokens no longer count." This is NOT a time-window (the fixed-window limiter is
increment-only and time-resets) — it's a RESERVATION POOL: hold N units while a job is pending,
RELEASE N on completion; deny when the in-flight sum would exceed the cap. It is the concurrency
primitive GENERALIZED from count (weight 1) to weighted (N units), with the same acquire/release
lifecycle as a money hold (Reserve→Capture/Release) but denominated in throughput units.

**Concrete tensorhub instance (owner steer, Paul):** tensorhub serves flex/batch (async, long-lived)
requests, so the analogue is "limit how many FLEX/BATCH requests may be IN QUEUE against a single
endpoint-release at once" — i.e. a cap on in-flight flex/batch requests per **(payer, endpoint-release)**,
scoped to the flex/batch availability tier only (fast/standard requests are not queue-capped this
way). That instance is COUNT-based (weight 1) — it's exactly the existing concurrency primitive, just
keyed per (payer, endpoint-release) + availability-tier-filtered + acquire-on-enqueue/release-on-
complete. The weighted (token-denominated) pool above is the GENERAL engine capability for OpenAI
token-queue parity; tensorhub itself only needs the count form. Same payer-not-invoker rationale from
G1 applies: key the queue cap on the payer, never the invoker.

## Tasks
- [x] G1 — throughput key = (payer, endpoint-release): drop `actor`/invoker (and any function/model
      sub-key) from the throughput bucket so limits AGGREGATE per payer per release; add per-release
      limit VALUES (`map[release][]ThroughputWindow` or default + per-release overrides); decide
      config home (default + per-release override vs a dedicated (payer,release) policy table);
      `SetTierPolicy`/policy load/RLS updated. Test: two releases, different RPM, one payer; counters
      independent per release and SHARED across the payer's invokers. — DONE: admitter.go base key now `tenant:payer:resource` (actor dropped); `ThroughputPolicy.ReleaseWindows map[release][]ThroughputWindow` (default + per-release override; CONFIG HOME = the existing tier_policies JSONB, no new table needed since it rides the same policy doc); `ResolvedPolicy.ThroughputForRelease`; `SetTierPolicy` accepts `ReleaseWindows`. Tests PASS: TestThroughput_PayerAggregatesAcrossInvokers, TestThroughput_PerReleaseWindows.
- [x] G2a — weighted reservation primitive `AcquireUnits(key, amount, max, ttl)` / `ReleaseUnits(key, amount)`
      in `internal/modules/ratelimit` (generalize concurrency to a unit weight; TTL auto-releases a
      crashed caller's hold; reuse the existing Lua acquire/decrement with an amount arg). — DONE: internal/modules/ratelimit/units.go (rlu:* keyspace, INCRBY/DECRBY-by-amount Lua, PEXPIRE crash-safety, floor-at-0, `UnitsCount`). Tests PASS: TestAcquireUnits_WeightedPool, TestReleaseUnits_FloorsAtZero.
- [x] G2b — `QueueLimit{Unit, Max}` axis on the policy (a pool cap, NO time window), keyed per
      **(payer, endpoint-release)** (same scoping/rationale as G1 — never per invoker); admission
      ACQUIRES the units on admit (enqueue) and returns `BlockedBy:"queue"` + the blocking unit +
      retry hint on breach. tensorhub's instance is the COUNT form (`Unit:"request"`, weight 1)
      filtered to the flex/batch availability tier; the weighted form (G2a) covers token pools. — DONE: `QueueLimitPolicy{Unit,Max}` on ThroughputPolicy + `ratelimit.AcquireQueue` (all-or-nothing over pools keyed `tenant:payer:release:unit`); admitter acquires on admit, denies `BlockedBy:"queue"`+BlockedUnit. Tests PASS: TestQueue_AdmitDeniesOnPoolOverflow (count form), TestAcquireQueue_WeightedTokens (token form).
- [x] G2c — wire release into the existing Capture/Release settlement so a completed/failed job frees
      its queued units (idempotent on request_id; same lifecycle as the credit hold). — DONE: `ratelimit.ReleaseQueueByRequest(source,sourceID)` (request-scoped record stores poolKey->amount so release needs no base; deletes after release = idempotent); wired into `pkg/service` CaptureHold AND ReleaseHold (a completed OR failed job frees its queue). Tests PASS: TestAcquireQueue_HoldAndReleaseByRequest (hold/free/idempotent/crash-TTL).
- [x] Confirm-no-op — document that TPM/RPM/IPM/VPM/RPD/TPD + concurrency are already supported via
      arbitrary-unit windows; add `video` to the documented unit examples. — DONE: confirmed `ratelimit.Limit{Unit,Window,Max}` + whichever-first deny already express RPM/RPD/TPM/TPD/IPM/VPM/gpu-second (just unit strings + Window); `video` added to the unit examples in ratelimit/limiter.go `Limit.Unit` doc.
- [x] VERIFY — per-release differing windows; payer-aggregation (two invokers of one payer share the
      (payer,endpoint-release) counter, invoker not in the key); queue-limit hold-and-release (pending
      sum capped, completion frees, crash TTL-releases); whichever-first across window + queue + money
      axes; x-ratelimit/queue header state. — DONE: per-release + payer-aggregation + queue tests above all PASS; whichever-first ordering is throughput -> queue -> budget -> money (each deny rolls back later-stage holds; verified queue+money deny rollback in admitter). x-ratelimit window state unchanged (existing `WindowInfo`).

## Related
#298 (throughput engine + admission spine), #304/#337 (budget windows), #306 (tenant→owner level —
separate), #305 (fast-path — separate); tensorhub #475 (consumes ONLY the $ budget axis today;
per-release windows + queue limits are for when a host wants OpenAI-style per-unit limits), tensorhub
#476 (per-payer scheduler fairness — these throughput caps are its secondary hard ceiling). Reference:
OpenAI rate-limit tables (gpt-5.5: TPM/IPM/VPM/RPM + batch queue; gpt-realtime-2: RPM/TPM) — but our
unit is the endpoint-release, not the model.

---

# #471: Rename migration app key + default schema `billing` -> `openrails`

**Completed:** yes (openrails v0.22.0 tagged; hosts renamed+bumped, uncommitted pending Paul's review)
**Priority:** low (branding/clarity; no functional benefit). Deferred — file now, do later.

Make OpenRails identify itself as `openrails` everywhere a name is exposed, instead of the generic `billing`, for branding + naming consistency. Two coupled renames:

1. **migratekit app/tracking key** `billing` -> `openrails` (the `public.migrations.app` value). Hard-coded in ~5 spots: `internal/migrate/migrator.go` (NewPostgres key, ClickHouse `App`, self-validation), `internal/app/build_runtime.go` `validateDatabase` (`MigrationSource{App: "billing"}` + the ClickHouse `App`). This is the value the embedded engine's boot validation greps for, which is why doujins/hentai0 were forced to match it — so this rename MUST land in openrails first, then the host repos follow.
2. **Default Postgres schema** `billing` -> `openrails` (`config/config.go` `DefaultSchema`). NOT just a constant flip: the migration SQL is hard-qualified `billing.*` (529 refs in `001_schema.up.sql` alone, more across the set + runtime queries). #165 made the schema nominally configurable via `WithSchema`/search_path, but the qualified DDL means the knob doesn't actually relocate tables. Proper fix = schema-template the SQL (the way authkit #69 templated `profiles.`) with default `openrails`, OR a blanket `billing.` -> `openrails.` rewrite if configurability is abandoned. Decide which; schema name and app key are independent (authkit app manages `profiles` schema) so they need not be equal, but the user wants both to read `openrails`.

**Upgrade path for existing databases** (the footgun — must ship with the rename):
- Postgres: `ALTER SCHEMA billing RENAME TO openrails;` (moves all contained tables incl. `billing.river_*` in one shot) + `UPDATE public.migrations SET app='openrails' WHERE app='billing';`. Mis-sequencing bricks boot (engine re-applies already-applied DDL, migratekit doesn't tolerate "already exists"; or validateDatabase fatals on `openrails` pending while rows say `billing`).
- ClickHouse: rename the tracker `app` rows; confirm whether the CH side uses a `billing`-named database/prefix that also needs moving.

**Host follow-up (separate, after the openrails tag):** bump the pin in doujins + hentai0; flip their migratekit source key `billing`->`openrails`; update the `billing` schema name in their river-billing provisioning + the `010_openrails_embed_control_plane_cleanup` migration's `billing.tenants` reference + the embed config schema + any `billing.*` references in host code; run the shared-dev-DB `ALTER SCHEMA` + tracker `UPDATE` once.

**Decision (2026-06-12):** HARD CUT, no data migration (greenfield per migrations-squash). Schema made
genuinely configurable via an execution-time rewrite (NOT search_path — embedded shares the host pool):
SQL is authored against default `openrails.`, and when `db.schema` differs the qualifier is rewritten
`openrails.`->`<schema>.` just before execution. Runtime: rewriting `gen.DBTX` (DB.Qx), wrapped `pgx.Tx`
(RunInTx/TenantTx + Pool.Begin), and a `db.Pool` wrapper for the control-plane pool (consumers retyped
`*pgxpool.Pool`->`*db.Pool`, authcore/River keep `.Raw()`). Migrations: whole-word rewrite in
`rewriteMigrationsSchema` (relocates qualifiers AND bare `CREATE/ON/IN SCHEMA openrails`, leaves
`openrails_app` role + prose). App key is a fixed const `config.MigratekitApp="openrails"` (PG + CH).
Also rebranded the `billing.`-prefixed River job kinds + gin context keys + Stripe lookup_key prefix.
DELIBERATELY KEPT as `billing` (out of schema/app-key scope): cobra cmd `Use`/`cmd/billing` dir, health
`{"service":"billing"}`, River queue name `QueueBilling`, pg_dump `Schema: billing` header comments.

**Tasks:**
- [x] openrails: rename app key `billing`->`openrails` (PG NewPostgres + ValidatePostgres + CH App, via `config.MigratekitApp`)
- [x] openrails: default schema -> `openrails`, made truly configurable via execution-time rewrite (runtime queries + migration DDL)
- [x] ~~ship the `ALTER SCHEMA` upgrade migration~~ — N/A, hard cut / greenfield (no data migration)
- [x] openrails: build/vet/test green (added schema-rewrite unit tests in internal/db + internal/migrate); committed + pushed + **tagged v0.22.0** (Paul authorized the commit/push/tag for this).
- [x] hosts pin-bumped to v0.22.0 + renamed (all build+vet+gofmt clean; changes left UNCOMMITTED for Paul to review/commit):
  - doujins: app key->`openrails` (NewPostgres + MigrationSource + WithSchema + identity test), bun `"billing".`->`"openrails".` table exprs, legacy_migrate targetColumnSpec schema, 76 SQL qualifiers, bootstrap CREATE SCHEMA, runRiver schema, restore/schema lists. KEPT doujins' OWN `koanf:"billing"` config section + `billing-app` audience + prose.
  - hentai0: targeted SQL-table rename only (integration test + repo TableExpr); KEPT Go `billing` pkg selectors, `billing.public_url`/`billing.hmovie-moe.us` config+host, `doujins:billing.inspect` permission.
  - tensorhub: bootstrap schema only (all `billing.*` are config keys under its `billing:` section; embeds engine on default schema).
  - cozy-art (branch `rebuild`): targeted SQL-table rename, app key (`applyMigrations`), `create schema`, bootstrap, backup/restore schema+tracker lists, AND the pool `search_path` `cozyart,billing,...`->`cozyart,openrails,...`. KEPT Go `billing` pkg selectors + `koanf:"billing"`/config-map/API field.
  - NOTE: shared dev DBs still hold a `billing` schema — hard cut means re-bootstrap into `openrails` (drop old `billing` schema) on next deploy / dev-DB reset.

---

# #470: Fix stale TestDunningWorkerLimitedModeTakesNoAction (pre-#366 semantics) 

**Completed:** no

`tests/dunning_worker_test.go` `TestDunningWorkerLimitedModeTakesNoAction` still asserts the pre-#366 limited-mode contract (subscription stays `past_due`, `next_retry_at` non-nil — i.e. "takes no action"). Commit a67cc3f5 (#366 limited-mode materialize) changed the behavior deliberately: limited mode now materializes decisions — local window-expiry cancellations APPLY (subscription cancelled, entitlements revoked, remote processor sub left alive for reconciliation), and charge intents enqueue PARKED. The test was never updated and fails on every integration run (found during #469 verification; NOT a #469 regression — confirmed via a67cc3f5 history).

**Tasks:**
- [ ] Rewrite the test to assert #366 semantics: window-expired sub → cancelled locally + entitlements revoked + NO provider writes + parked charge intents; rename accordingly (e.g. TestDunningWorkerLimitedModeMaterializesWithoutProviderWrites)
- [ ] Keep/add a companion assertion that a NON-expired past_due sub takes no action in limited mode (the surviving half of the old contract)

---

# #469: HARD CUT: control plane mandatory in standalone — remove verifier-only mode entirely

**Completed:** no

Standalone OpenRails always runs its own AuthKit control plane. The "verifier-only" /
"pure JWT verifier" deployment mode (control plane omitted/disabled) is REMOVED, not
deprecated: delete the code, config knobs, conditional branches, and docs that exist
only to support it. No legacy deployments exist; no compatibility shims.

Rationale: OpenRails' product model is a Stripe-like multi-tenant billing server —
users register accounts (native AuthKit users), create tenants (merchants), and
tenants' customers exist as tenant-subjects. The control plane IS that model; the
self-service browser surface (/v1/self/* delegated tokens), runtime tenant/issuer
management, and the admin API all already live behind it. Verifier-only survives only
as a topology fork that every auth-adjacent feature must branch on (e.g. the
"nil resolver => self-service surface not mounted" fork in ginmw/delegated.go), doubling
the test matrix and muddying the docs. Private/self-hosted posture is expressed by the
existing registration axes (public_user_registration / public_tenant_registration,
both default false) — NOT by amputating the control plane.

The minimal "billing sidecar with no auth state" trust model remains available where it
belongs: pkg/embedded (host-authenticated, in-process, one trust domain). Standalone is
the multi-tenant server and always owns its AuthKit instance + profiles schema in its
own database.

Scope (survey for completeness; this list is the known surface, not the boundary):
- Standalone boot always runs controlplane.Attach; remove auth.control_plane.enabled /
  ControlPlaneEnabled() and every `cp == nil` / nil-resolver / "verifier-only" branch
  (route mounting, ginmw/delegated.go DelegatedResolver nil contract, admin gating).
  Control-plane construction failure is fatal at boot, not a silent downgrade.
- Remove the static-config trust root that exists only for verifier-only standalone
  (auth.issuers as the sole issuer allowlist + internal/auth/verifier.go wiring), in
  favor of the control plane's live issuer registry. Where first-party service-JWT
  verification still needs config-declared issuers, keep that as an explicit
  control-plane input, not a parallel auth mode.
- config.example.yaml / README / DEVSERVER docs: delete the "pure JWT verifier"
  narrative and the optional-control-plane decision tree; the standalone auth story is
  one model (control plane, registration axes closed by default).
- pkg/embedded is unaffected in its core (authkit-free, host-authenticated). Evaluate
  pkg/embedded/authkit + pkg/embedded/controlplane opt-in helpers: keep what embedded
  hosts and the standalone binary genuinely use, delete what existed only to make the
  control plane optional in standalone.
- Tests: collapse the dual-topology matrix; delete verifier-only fixtures/harness paths
  (tests/openrails_harness.go in consumers may pin public_tenant_registration etc. —
  those knobs stay; the enabled/disabled fork goes).
- Migrations/bootstrap: control-plane bootstrap (default tenant org, bootstrap admin
  service token, manifest apply) becomes the unconditional standalone boot path.

NOTE: next_id above was stale (365) while issues up to #468 already exist across
progress/future/completed; this issue took #469 and reset next_id to 470.

**Tasks:**
- [x] Inventory every control-plane-optional branch (`ControlPlaneEnabled`, nil cp/resolver checks, "verifier-only" mentions) across internal/, pkg/, cmd/, config/, docs
- [x] Make controlplane.Attach unconditional in standalone boot; fail-fast on error
- [x] Remove auth.control_plane.enabled knob + ControlPlaneEnabled()/OperatorTenantEnabled() shims
- [x] Remove/fold internal/auth/verifier.go static-issuer mode into control-plane issuer config — auth.issuers KEPT as the first-party user/admin JWT input (doujins AUTH_ISSUERS keeps working); parallel-mode framing deleted; delegated/service creds stay on the control plane's live issuer registry
- [x] Delete nil-resolver fork in ginmw/delegated.go (+ equivalent forks elsewhere); self-service surface always mounted
- [x] Prune pkg/embedded/authkit + pkg/embedded/controlplane to what's actually used post-cut — both kept (used by standalone wiring + embedded opt-in); only the enabled-check/nil-tolerant paths inside them were cut
- [x] Docs hard cut: README auth-model section, config.example.yaml, principal-boundary-audit.md (no DEVSERVER.md exists)
- [x] Collapse test matrix; go build/vet/test green — build/vet/unit suites green. Integration suite (`-tags=integration ./tests/`): verbose run completed all #469-relevant tests green; ONE failure, `TestDunningWorkerLimitedModeTakesNoAction`, confirmed PRE-EXISTING (encodes pre-#366 "takes no action" semantics; behavior changed in a67cc3f5 — filed as #470; clean HEAD can't even build the integration tests, fixed in this tree). Suite also exceeded a 45m wall under heavy concurrent machine load (3 sibling agent runs + dev compose); re-run on a quiet machine before tagging.

---

# #330: nmi-immediate-subscription-checkout-stuck-pending

**Completed:** no
**Status:** IN_PROGRESS 2026-06-08: immediate NMI checkout activation patch is implemented; package tests and focused mock-provider regression pass; configured-account Mobius recurring test has been added and compiles/skips locally because `PROCESSORS_MOBIUS_SECURITY_KEY` is not set. Remaining work: run real Mobius credential test and repair/replay Paul2 after fixed code is deployed.

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
- [ ] Run the actual Mobius/NMI configured-account recurring-subscription integration with `PROCESSORS_MOBIUS_SECURITY_KEY` set.
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


# #108: admin-user-search

**Completed:** no

Sophisticated user search for admin dashboard

## Metadata

- Category: feature
- Status: not_started
- Passes: false

**Tasks:**
- STEPS:
- [ ] Design search API:
- [ ]   - GET /v1/admin/users?q=...&filters... - search users with billing data
- [ ] Search fields:
- [ ]   - email (partial match from subscriptions/payments)
- [ ]   - user_id (exact match)
- [ ]   - processor_subscription_id (exact match for support lookups)
- [ ]   - transaction_id (exact match for payment support)
- [ ] Filters:
- [ ]   - has_subscription=true/false
- [ ]   - subscription_status=active/cancelled/past_due/etc
- [ ]   - processor=nmi/ccbill/solana
- [ ]   - has_entitlement=premium/etc
- [ ]   - created_after, created_before (date range)
- [ ] Sorting:
- [ ]   - sort_by=newest/oldest/last_payment/subscription_start
- [ ] Pagination:
- [ ]   - page, page_size, cursor-based pagination for large results
- [ ] Performance:
- [ ]   - Add database indexes for search fields
- [ ]   - Consider full-text search for email
- [ ]   - Rate limit search queries
- NOTE: Billing service searches its own data. Full user profile search (username, etc) lives in main host-app API.

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

# #163: remove-dead-config-and-debug-prints

**Completed:** no

Remove unused/dead config surface area and stray debug prints.

## Metadata

- Category: cleanup
- Status: planned
- Passes: false

## Motivation

- Keep OpenRails configuration minimal and accurate.
- Avoid maintaining config keys/struct fields that have no behavioral effect.
- Remove accidental stdout logging from production code paths.

## Targets

- Deprecated + unused config: `Config.Webhooks` / `WebhookConfig` / `WebhookRetryConfig` and `Config.GetWebhookRetryConfig()` (config/config.go:536, config/config.go:709)
- Likely-dead CCBill config field: `subscription_type_id` in processor config (config/config.go:245, config/config.go:372)
- Unused rate-limit knob: `RateLimit.Burst` (config/config.go:526)
- Stray debug prints: `fmt.Println(...)` in NMI client request path (internal/integrations/nmi/nmi.go:1085)

**Tasks:**
- CODE REMOVALS:
- [ ] Remove `Config.Webhooks` field and `WebhookConfig` / `WebhookRetryConfig` types from config/config.go
- [ ] Remove `GetWebhookRetryConfig()` from config/config.go and delete any call sites if discovered
- [ ] Remove `subscription_type_id` from `ProcessorConfig` (ccbill) and from `CCBillConfig`
- [ ] Remove `RateLimit.Burst` from config/config.go and from any default rate limit definitions
- [ ] Remove `fmt.Println` debug output in internal/integrations/nmi/nmi.go (direct request path)
- 
- DOCS / EXAMPLES:
- [ ] Remove corresponding keys from config.example.yaml and .env.example
- [ ] Ensure README.md does not mention removed keys; update if needed
- 
- COMPAT / SAFETY:
- [ ] Confirm unknown config keys do not crash koanf load/validate (so removal doesn’t break old config files)
- 
- VERIFY:
- [ ] `go test ./...` passes

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

# #219: Require prepared or parameterized SQL; migrate from Bun to sqlc

**Completed:** no

Adopt a hard rule that OpenRails database access must use prepared statements or parameterized statements, never direct SQL built from interpolated values. Run a direct benchmark comparison between current Bun-written queries and equivalent sqlc + pgx queries for performance, allocations, generated SQL quality, query plans, and ergonomics. Plan direction: switch all OpenRails queries from Bun to sqlc, using the benchmark to guide rollout order, catch regressions, and validate the expected performance/control gains.

**Tasks:**
- [ ] Define the SQL safety rule precisely: placeholders/parameter binding are required for all runtime values; string concatenation/interpolation is allowed only for vetted static SQL fragments such as fixed identifiers or generated migrations
- [ ] Audit all Bun query builder, NewRaw, Exec, Query, QueryRow, and fmt.Sprintf SQL construction sites for interpolated runtime values
- [ ] Add focused tests or static checks that fail on unsafe raw SQL patterns where practical
- [ ] Add a benchmark suite comparing representative Bun queries against equivalent sqlc + pgx queries for latency, allocations, generated SQL quality, query plans, and operational debuggability
- [ ] Benchmark active entitlement reads: IsEntitled, ListActiveEntitlements, and ListActiveRecords against billing.entitlements with realistic active, expired, revoked, deleted, finite, and indefinite rows
- [ ] Benchmark credit balance and credit transaction lifecycle flows: GetBalance, Deposit, Hold, CaptureHold, ReleaseHold, Withdraw, and FIFO credit_blocks depletion with 1/10/100 spendable blocks
- [ ] Benchmark credit transaction history and idempotency lookups: paginated transactions by user+credit_type and source/source_id lookup for metering/request idempotency
- [ ] Benchmark checkout session lifecycle queries: price/product lookup, checkout session insert, GetByID, status/state update, and latest open session by user+price+processor
- [ ] Benchmark subscription lifecycle guard queries: active-or-pending by user+product and active-or-pending by user+tier_group with product relation loading
- [ ] Benchmark webhook resolution queries: subscription lookup by processor_subscription_id, subscription metadata lookup, price lookup by processor JSONB fields, payment lookup by processor+transaction_id, and payment insert-if-not-exists
- [ ] Benchmark processor customer mapping queries: upsert customer id, lookup customer id by user+processor, and reverse lookup user id by processor+customer_id
- [ ] Benchmark public catalog reads and account/admin pagination as second-wave cases: active prices with product relation, filtered price listing, user subscriptions, user payments, and subscriber/payment admin listings
- [ ] Seed benchmark datasets at realistic sizes and skews: large entitlements, payments, subscriptions, credit_transactions, credit_blocks, and user_credit_balances tables with both hot users and normal users
- [ ] Capture EXPLAIN ANALYZE, query text, DB round trips, Go allocations, p50/p95 latency, and transaction contention behavior for each Bun and sqlc implementation
- [ ] Use the benchmark to choose rollout order and identify hot repositories/queries that should move first
- [ ] Introduce sqlc configuration, query directories, generated package layout, and CI checks (`sqlc generate`, `sqlc vet`, and any schema verification we adopt)
- [ ] Design the sqlc repository layer: generated query package ownership, transaction handling, pagination helpers, nullable/type overrides, and scan/mapping conventions
- [ ] Migrate one representative repository from Bun to sqlc as a spike and compare readability, safety, performance, and test ergonomics
- [ ] Migrate all remaining Bun-backed repositories and runtime queries to sqlc + pgx
- [ ] Remove Bun from runtime dependencies once no production query path depends on it
- [ ] Document the approved SQL patterns for new code and code review

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
- [ ] Add Stripe test-mode catalog apply + subscription sync certification steps.
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
- [ ] Use capabilities in catalog apply/plan to decide provider actions and pending_manual_actions.
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

Add Hyperswitch as an optional OpenRails payment provider and payment-method vault integration, covering both Hyperswitch Cloud and self-hosted Hyperswitch. This is payment vaulting/tokenization, not HashiCorp Vault tenant-secret storage. OpenRails should store only opaque Hyperswitch customer/payment-method identifiers plus non-sensitive metadata, while PAN/card collection stays in Hyperswitch-hosted/client-side tokenization flows or equivalent PCI-scoped Hyperswitch surfaces.

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


# #336: Multi-tenant writers don't stamp tenant_id: checkout_sessions / subscriptions / payments / entitlements rows land under the DEFAULT tenant on delegated self-checkout

**Completed:** no
**Status:** IN PROGRESS 2026-06-13 — full audit done (scope is wider than the original 4 tables; see ## Audit), design chosen (pinned-tenant embedded client + GUC-derived column default; see ## Plan), implementing now.

A real /v1/self/checkout charge (delegated token: iss=http://hentai0:4000, tenant=doujins, registered in billing.tenant_delegated_issuers) produced billing.checkout_sessions, billing.subscriptions, billing.payments, and billing.entitlements rows ALL with tenant_id=00000000-0000-0000-0000-000000000001 (default tenant), while billing.tenant_subjects correctly mapped the subject to the doujins tenant in the same request. ROOT CAUSE: those tables' tenant_id columns default to the hardcoded default-tenant uuid (the migration comments say: defaults to the 'default' tenant for single-tenant writers, stamped explicitly by multi-tenant writers) but the checkout-path writers never stamp it - models.CheckoutSession / Subscription / Payment / Entitlement carry NO TenantID field at all, so the INSERT always takes the column default. DelegatedSelfRequired DOES pin the resolved tenant on the request ctx (internal/http/middleware/ginmw/delegated.go:97) and TenantDBConn sets the app.tenant_id GUC, but the GUC only matters for RLS (no-op under the privileged dev role) - it does not change the INSERT default. Same bug class as the CreatePrice TenantID fix (pkg/service/service_definition_catalog.go:430). IMPACT: reads still work today because the self-surface scopes by tenant_subject_id, but tenant attribution/reporting is wrong, and under openrails_app+RLS (managed multi-tenant) these inserts would fail or rows would be invisible to the owning tenant. FIX SKETCH: add a TenantID field (bun tenant_id,type:uuid,nullzero) to CheckoutSession/Subscription/Payment/Entitlement (+ payment_methods/processor_customers/invoices - audit all tables with a tenant_id column whose model lacks the field) and stamp tenant.FromContextOrDefault(ctx) at the insert sites; or change the column defaults to current_setting('app.tenant_id')::uuid so the GUC pins it. Coordinate with #334 (bun->sqlc migration) - whichever lands second must carry the stamping.

**Tasks:**
- STEPS:
- [ ] Audit: list every billing.* table with tenant_id whose Go model lacks a TenantID field
- [ ] Decide: explicit stamping at insert sites vs current_setting('app.tenant_id') column default (GUC is already set by TenantDBConn/TenantTx)
- [ ] Implement for checkout_sessions, subscriptions, payments, entitlements (+ payment_methods, processor_customers, invoices if affected)
- [ ] Integration test: delegated self-checkout under a non-default tenant asserts tenant_id on all written rows == the token's tenant
- [ ] Verify against the doujins/hentai0 E2E stack (delegated issuer http://hentai0:4000, tenant doujins)

## Audit (2026-06-13)

Swept every `openrails.*` table with a `tenant_id` column for INSERTs that omit it (→ silently take the static default-tenant default `…0001`). Wider than the original 4-table report. **No tenant_id-deriving trigger exists** (only `subscriptions_set_tier_group`). RLS `tenant_isolation` (`WITH CHECK (tenant_id = current_setting('app.tenant_id'))`) is present, so under the `openrails_app` role these inserts are REJECTED for a non-default tenant; under the dev BYPASSRLS role they silently land under the default tenant.

BUGGY inserts (omit tenant_id):
- `checkout_sessions` — CreateCheckoutSession
- `subscriptions` — CreateSubscription
- `payments` — CreatePayment, CreatePaymentIfNotExists, **and** reconciliation ReconcileBackfillPayment / ReconcileRecordRefund (INSERT omits, yet `ON CONFLICT (tenant_id, processor, transaction_id)` references it — extra-broken)
- `payment_methods` — CreatePaymentMethod
- `processor_customers` — UpsertProcessorCustomer (INSERT omits; ON CONFLICT references tenant_id)
- `admin_grants` — CreateAdminGrant
- `notification_queue` — CreateNotification
- `catalog_drift_events` — InsertCatalogDriftEvent
- `entitlements` — via reconciliation ReconcileGrantSubscriptionEntitlement (note: the primary CreateEntitlement DOES stamp it — that is the reference pattern)
- `credit_types` — CreateCreditType (likely intentionally system-wide / not tenant-scoped — confirm; leave alone if so)

CORRECT (stamp tenant_id with a COALESCE(arg, default) fallback): entitlements CreateEntitlement, prices, products, product_access_grants, product_entitlement_features, entitlement_features, invoices, linked_wallets, payment_blocklist, solana_subscriptions, tier_policies, usage_events, usdc_funding_sessions, budget_reservations, credit_account_settings, credit_blocks, credit_spend_limits, credit_transactions.

**doujins side audited separately: CLEAN.** `migrate legacy` does direct DB inserts but explicitly stamps `opts.BillingTenantID` (resolved once from `openrails.tenants` slug='doujins') on all 9 billing-table inserts + ClickHouse analytics, and runs BYPASSRLS. Doujins' own tables have no tenant_id. No doujins changes needed; it deliberately bypasses the client and stamps columns directly, which is correct for an importer.

## Plan (2026-06-13) — pinned-tenant embedded client + GUC-derived default

Mechanism mirrors the just-landed configurable schema (#471, `DB.Schema` → search_path): a one-off construction-time tenant selection that the engine then respects everywhere, so embedded hosts (doujins/hentai0 — both the single `doujins` tenant) never pass tenant per-call.

1. **GUC-derived column default.** New migration: for every affected table, `ALTER COLUMN tenant_id SET DEFAULT COALESCE(NULLIF(current_setting('app.tenant_id', true), '')::uuid, '00000000-0000-0000-0000-000000000001'::uuid)`. When the connection's `app.tenant_id` GUC is set (the existing WithTenantConn mechanism), omitted-tenant_id INSERTs stamp the correct tenant; when unset (single-tenant standalone), they fall back to the default tenant exactly as today. Fixes ALL the buggy inserts above at once, no per-query/repo churn, and makes the `payments` query comment ("column default + RLS own it") finally true. Idempotent + transactional.
2. **`embed.Options.Tenant`** (slug; resolved to a tenant ID at New()). The embed Runtime wraps the Client's per-call contexts AND the worker contexts with `tenant.WithID(ctx, pinned)`, so WithTenantConn sets `app.tenant_id` from it on every embedded operation. Standalone HTTP is unchanged — it keeps resolving the tenant per-request from the delegated token via the existing middleware.
3. **Ensure the GUC is set on every write connection.** Audit the embedded Client call path and the River worker write paths (dunning/lifecycle INSERT payments/subscriptions) to confirm they run on a WithTenantConn connection carrying the ctx tenant; wrap any that don't (standalone multi-tenant workers must set the GUC from the job's tenant — UPDATEs keep their existing tenant_id, but INSERTs need it).
4. Leave the explicit-stamping queries as-is (they already pass the real tenant); the GUC default only governs the omitted-column case.

REJECTED alternative: per-insert explicit stamping (add tenant_id arg to ~12 queries + repo methods + sqlc regen). More invasive and easy to miss a site; the GUC default is centralized and matches the schema-config precedent the owner asked for.

## Findings refinement (2026-06-13, during implementation) — the conn IS pinned on the HTTP path

Traced the actual write paths. The `app.tenant_id` GUC is NOT the missing piece on the HTTP surface:
- `RegisterUserRoutes` (internal/http/routes/routes.go:84) wraps the ENTIRE self group — `/checkout` POST (CreateCheckoutSession), `/me/payment-methods`, `/me/subscriptions/*` — in `TenantDBConnMW(rt.DB)`, which pins a tenant-scoped connection and sets `app.tenant_id` from the request's resolved tenant. This applies to BOTH standalone (gin) AND embedded (embedhttp.go calls the same RegisterUserRoutes). So the delegated self-checkout that filed #336 already ran with the GUC set — the ONLY defect was the static column default ignoring it. **Migration 014 (GUC-derived default) is therefore the complete fix for the HTTP self-checkout repro**, standalone and embedded. This matches the issue's own line: "TenantDBConn sets the app.tenant_id GUC, but ... it does not change the INSERT default."
- So `embed.Options.Tenant`/pinned-client is NOT needed for the checkout/self/admin paths (they resolve tenant per-request from the delegated token and pin the conn). It would only matter for direct `Client()` SDK writes that bypass HTTP — and those are credits/admit/authorize, which already stamp tenant_id explicitly. De-scoped unless a future direct-write consumer appears.

**Remaining real gap: River worker INSERTs.** Workers (dunning/lifecycle in internal/river) do NOT call RunInTenantConn/WithTenantConn at all — no GUC is set on their connections. Workers that insert into explicit-tenant_id tables (credit_expiry, solana_crank) pass TenantID in the struct, so they're fine; but the dunning/lifecycle path that INSERTs `payments`/`subscriptions` (which omit tenant_id) would, post-migration, still take the default-tenant fallback (GUC unset) — wrong under multi-tenant AND wrong for doujins (whose tenant is NOT the default tenant). Fix: wrap each worker's per-subscription unit of work in `db.RunInTenantConn(tenant.WithID(ctx, sub.TenantID), …)` (the documented background analogue of the request middleware) so its INSERTs inherit the GUC. UPDATEs are unaffected (they keep the row's existing tenant_id).

## Revised tasks (2026-06-13)
- [x] Audit all tenant_id tables (openrails engine + doujins legacy-migrate) — see ## Audit
- [x] Trace write paths: HTTP self/checkout/admin pin the tenant conn via TenantDBConnMW (standalone + embedded); workers do not — see ## Findings refinement
- [x] Migration 014: GUC-derived `tenant_id` DEFAULT on the 9 affected tenant-scoped tables — fixes the HTTP path
- [ ] Worker fix: wrap dunning/lifecycle per-subscription work in RunInTenantConn(sub.TenantID) so payment/subscription INSERTs carry the GUC
- [ ] Integration test under the `openrails_app` role: delegated self-checkout under a non-default tenant → assert checkout_sessions/subscriptions/payments/payment_methods/entitlements rows all carry that tenant_id (NOT the default); plus a dunning-worker insert case
- [ ] go build/vet + unit + integration green
- [ ] (de-scoped) embed.Options.Tenant pinned-client — only if a direct non-HTTP Client() write to a tenant table appears; checkout path is covered by the conn middleware + migration. doujins needs NO changes either way.

## SCOPE EXPANSION (2026-06-13, owner directive) — remove the "default tenant" entirely; tenant_id strictly required

Owner: "there is no such thing as a default tenant — that concept doesn't even make sense. The tenant column should be required (non-nullable). A missing tenant must be an ERROR, not a silent default." This supersedes the fallback half of migration 014.

Target end state:
- **DB:** every tenant_id column stays NOT NULL and its DEFAULT becomes `(NULLIF(current_setting('app.tenant_id', true), ''))::uuid` with NO `…0001` fallback. GUC set → stamps; GUC unset → NULL → NOT NULL violation → loud error. (Revise migration 014: drop the COALESCE-to-default-tenant.)
- **Go (pkg/tenant):** delete `DefaultID`, `DefaultSlug`, `IsDefault`, and `FromContextOrDefault`. Replace the latter with a require-or-error accessor; a missing tenant is an error at the call site, not a silent default.
- Delete the seeded "default" tenant row (002_seed) and scrub the `…0001` literal from migrations/queries/Go.
- Single-tenant / embedded hosts no longer ride a "default" tenant — they run as their OWN configured tenant (doujins resolves slug `doujins`); the GUC is always set from the resolved/configured tenant. This is where the earlier "pin the tenant at embedded-client construction, like DB.Schema" directive lands: a required per-instance tenant, not a default.

This is a LARGE, COUPLED refactor and CANNOT land incrementally-green: the instant the default tenant is gone, every write path / seed / test that doesn't set the GUC errors at once, so the whole change (migration + ~114 `FromContextOrDefault` call sites converted to require-or-error + seed + RLS + worker GUC) must land together and be verified in one pass against the integration suite (confirmed runnable here, testcontainers).

OPEN DESIGN QUESTION for the owner: genuinely platform-global tables (`platform_audit`, `platform_break_glass`, possibly `credit_types`) currently carry tenant_id defaulting to `…0001`. With no default tenant, either (a) they drop tenant_id (they're not tenant-scoped), or (b) platform ops run under an explicit platform context. Needs a decision before the literal can be fully removed.

WORKER-FIX FINDING (2026-06-13): the dunning worker loop holds `models.Subscription`, which has NO TenantID field (the gen type used deeper, `genSub`, does). So pinning the worker's tenant requires exposing tenant_id on `ListDueDunningSubscriptions` (query + model) or resolving it via tenant_subject_id — i.e., a small query/model change + sqlc regen, not a one-liner. Folded into the refactor above.

## Status of work (2026-06-13)
- DONE + committed (master): migration 014 (GUC-derived default WITH interim `…0001` fallback) — fixed the HTTP self-checkout repro.
- IN PROGRESS on branch `wip/336-remove-default-tenant` (NOT on master; does not yet compile): the full removal.

### Refactor executed (branch) — ~110 mechanical call sites converted (5 parallel agents)
`pkg/tenant`: removed `DefaultID`/`DefaultSlug`/`IsDefault`/`FromContextOrDefault`; added `Require(ctx) (ID, error)` + `ErrNoTenant`. Migration 014 rewritten: GUC-derived `tenant_id` default with NO fallback on all 40 tenant_id-owner tables (GUC unset → NULL → NOT NULL error; validated). Seed default-tenant row removed (002_seed). dbtest gained `TestTenantID`/`EnsureTestTenant`/`WithTestTenant` (no default tenant in tests).
- credits: 44 sites; db+db/repo: 34; checkout/analytics/vault/abuse/admission/budgets: 20; http/pkg-service/reconcile: 9; controlplane/cmd/billingauth/tenancy/embed: DefaultID/Slug removed. All propagate the missing-tenant error in each function's existing style; zero `…0001` reintroduced.

### THE DEFICIENCIES THE DEFAULT TENANT WAS HIDING (the point of removal)
Removing the default tenant surfaced that the deployment-level tenant RESOLVERS silently defaulted — i.e. multi-tenant resolution was never actually required:
1. **`internal/http/middleware/http_base.go` `ResolveTenantHTTP`** and **`ginmw/tenant.go` `resolveTenantID`** — the BASE request middleware pinned `tenant.DefaultID` whenever it couldn't resolve a tenant. So every request lacking downstream (delegated/token) resolution silently ran as the default tenant. (doujins is saved only because its delegated-auth middleware overrides with the real tenant downstream.)
2. **`internal/modules/solana/recurring/wiring.go` `SeedDefaultTenantSolanaSecret`** — boot-time secret seed hardcoded the default tenant (no request ctx).
3. **`cmd/billing/main.go` standalone startup bootstrap** — passed no tenant slug, relied on the default fallback (now errors).
4. **`cmd/billing/reconcile.go`** — `--tenant` empty → default; now required.
5. control-plane bootstrap/service-token + `--org` mints — empty value silently meant the default tenant; now required.

### DESIGN DECISION NEEDED (gates completion) — the "configured tenant", i.e. the owner's earlier "pin the tenant at construction, like DB.Schema"
The base resolvers (#1) and boot seed (#2) need a tenant when there's no per-request resolution. Two correct behaviors, owner to confirm the split:
- **Base HTTP resolver:** when it cannot resolve a tenant, do NOT default — leave it UNRESOLVED so downstream `tenant.Require` errors (a request with no tenant is a 4xx). Embedded/single-tenant hosts that want a per-instance tenant supply it via a CONFIGURED tenant (the embedded host already knows its slug, e.g. doujins='doujins') threaded into the resolver — this is `embed.Options.Tenant` / a standalone `config` tenant. NOT a default.
- **Boot seed / CLIs:** take the configured/explicit tenant; error if absent.

### CONFIRMED DESIGN (owner, 2026-06-13): tenant-bound at construction, no default, identical embedded/remote
The OpenRails engine/client is bound to a SINGLE tenant at construction (e.g. doujins/hentai0 construct theirs with tenant='doujins'). Same interface for embedded and remote — remote↔embedded is "mostly a config change". Multiple tenants = multiple clients (one per tenant). No default tenant in either mode; a missing tenant must hard-fail (insert with no tenant fails 100% — that exposes the error, by design). So the construction-time tenant is the resolver's source; per-request multi-tenant resolution (#222) would layer on top later.

PUBLIC API = SLUG ONLY (owner, 2026-06-13): the tenant is always a tenant-SLUG (string) in the library API, config, and HTTP request/response. The tenant UUID is an INTERNAL detail — resolve slug→uuid once at the boundary (gen.GetTenantBySlug) and uuid→slug on the way out. Never expose a tenant uuid publicly. So `config.Config.Tenant` and `embed.Options.Tenant` are SLUGS; internal middleware/seed take the resolved tenant.ID.

### DONE on branch (commit 65edd8c7) — resolver/seed mechanism
- `ginmw.ResolveTenant(configured)` + `middleware.ResolveTenantHTTP(configured)`: pin the configured construction tenant; if zero, pin NOTHING → downstream `tenant.Require` errors (no silent default).
- `recurring.SeedConfiguredTenantSolanaSecret(tenantID)`: seeds the global-config Solana key under the configured tenant; no-op if zero.
(Branch does NOT compile yet — callers still use old signatures; this is the next step.)

### Remaining (branch wip/336-remove-default-tenant), in order
- [ ] Add the construction tenant source: `embedded.Options.Tenant tenant.ID` (+ `embed.Options.Tenant`) for embedded; a `config` tenant slug for standalone, resolved slug→id at bootstrap via `GetTenantBySlug` (profiles.sql) / tenancy lifecycle resolver; resolve once and FAIL boot if the configured slug doesn't exist.
- [ ] Wire the resolved id into the 4 callers: server.go:458 `ResolveTenant(id)`, embedhttp.go:136 + gin/self.go:80 `ResolveTenantHTTP(id)`, server.go:274 `SeedConfiguredTenantSolanaSecret(id, …)`. Plus cmd/billing bootstrap/reconcile already require an explicit tenant.
- [ ] Remote client: bind its tenant at construction too (parity with embedded), so doujins/hentai0 set tenant='doujins' once.
- [ ] `go build ./...` green on the branch.
- [ ] Worker fix: expose tenant_id on `ListDueDunningSubscriptions` (models.Subscription has no TenantID → add to query/model, sqlc regen) + RunInTenantConn(sub.TenantID) in the dunning loop.
- [ ] Test harness (tests/testcontainer_suite.go): set `suite.Config` construction tenant = dbtest.TestTenantID (already seeded via dbtest.EnsureTestTenant) so the standalone server the suite builds pins it — this makes MOST of the ~43 test files pass without per-test changes (they relied on the default; now they ride the construction tenant). Only tests asserting a specific tenant or using >1 tenant need edits.
- [ ] RLS integration test: a non-default tenant's self-checkout stamps tenant_id on checkout_sessions/subscriptions/payments/payment_methods/entitlements; assert under the openrails_app role.
- [ ] Full suite green; then merge branch to master (migration 014's no-fallback form replaces the interim committed version).

---

# #335: Batch admission: prepaid credit windows (bulk hold + batched settle) so callers stop paying an HTTP hop per request

**Status:** open (planned 2026-06-10; Paul approved the design direction from tensorhub #443: 'adding a batch endpoint to openrails is a great idea') || DONE 2026-06-10 overnight (merge of ded7124): windows + cross-payer settle + batch admit + go-client, integration tests green (4 new PASS; 5 failures verified PRE-EXISTING on base 2f6e6c3 — units-conversion-looking, in arrears/topup/admit-gating integration tests; likely fallout of the usd_micro denomination change e2cd5cf — needs a separate fix). Tensorhub-side consumption = tensorhub #443. || SDK FOLD 2026-06-11 (#338 follow-up, tensorhub #468 parity): OpenWindow/SettleWindowItems/RefillWindow/CloseWindow/AdmitBatch are now openrails.Client interface methods with typed requests/responses, implemented by BOTH transports (remote = the existing HTTP routes; embedded = handler-transcribed direct service calls) and covered by the dual-transport conformance script — embedded hosts (tensorhub) get real windows instead of degrading to per-request Admit.

Tensorhub's hot path pays ~30-40ms per request for the synchronous /v1/service/admit hop (authorize+hold per request). Replace per-request holds with a PREPAID WINDOW the caller admits against locally: one bulk hold worth ~N requests, batched settlement of actuals, async refill. External hops drop to ~1/N; over-spend is bounded by the window size; an abandoned window's remainder releases at hold expiry (existing TTL machinery). SCOPE CLARIFICATION (Paul): windows are necessarily PER-PAYER (a hold reserves one payer's funds) — that's where the zero-hop local-admission win lives for payers with traffic. The SETTLE batch is CROSS-PAYER: one flush carries all users' actuals ([{window_id, request_id, amount}...] mixed freely). For COLD payers (no open window) add a cross-payer BATCH-ADMIT endpoint: the caller CONFLATES admits (flush immediately when no flush is in flight; arrivals during an in-flight flush form the next batch — zero added wait at idle, self-tuning batch size under load, hops/sec ~= 1/RTT) into POST /v1/service/admit/batch with mixed payers, processed in one transaction — collapses N concurrent hops into 1 (load win); sustained traffic graduates a payer to a window (latency win). Settlement reuses the existing per-request capture-dedup (idempotent on request_id) so re-sent batches never double-charge. INVARIANT (Paul's broke-user question): NO OPTIMISTIC APPROVAL anywhere. A window is a REAL hold — funds leave the payer's available balance when it opens, so local admission spends already-reserved money; a $0 payer cannot open a window and his batch-admit item returns insufficient_credits on the FIRST request (batching collapses transport, never defers the check). Server enforces sum(settles) <= window held. Residual risk = estimate-vs-actual within a request, identical to today's per-request holds (capture clamped to held). LATENCY TARGETS: hot payer <1ms (local decrement); cold payer ~= one hop (~35ms; conflation adds ~0 at idle, <=1 in-flight RTT under burst); refill/settle fully off the request path. Window-dry mid-burst: fail closed INTO batch-admit (one-hop real verdict), not outright reject. Settlement flush cadence is OFF the request path (nobody waits on it) — 250ms-1s accumulation is fine there.

**Tasks:**
- {'k': '1', 'desc': "POST /v1/service/credits/windows {tenant_subject_id, actor, credit_type, amount, ttl} -> {window_id, expires_at}: one bulk hold (existing hold machinery, source='window')", 'done': True}
- {'k': '2', 'desc': "POST /v1/service/credits/settle {items: [{window_id, request_id, amount, usage{...}}]} — CROSS-PAYER batched captures (items span many windows/payers), idempotent per request_id (capture dedup), per-item partial-failure reporting; each window's remainder stays held", 'done': True}
- {'k': '3', 'desc': 'POST /v1/service/credits/windows/:id/refill {amount} (extend hold + ttl) and /close (release remainder). Window expiry = hold expiry (auto-release, already exists)', 'done': True}
- {'k': '3b', 'desc': 'POST /v1/service/admit/batch {items: [admit-requests, mixed payers]} — one-transaction batch admission for cold payers (no window yet); per-item verdicts; same semantics as /v1/service/admit per item', 'done': True}
- {'k': '4', 'desc': "Decide the non-credit admit axes for window mode: throughput/budget/blocklist checks are per-request in /v1/service/admit — either the window grants 'N requests within T' alongside funds, or the caller keeps a local limiter while in window mode. Document the contract", 'done': True}
- {'k': '5', 'desc': 'go-client: OpenWindow/SettleWindow/RefillWindow/CloseWindow + a WindowedAuthorizer that admits locally and flushes settlement batches (size-or-deadline)', 'done': True}
- {'k': '6', 'desc': 'Tests: concurrent local admits vs window balance; idempotent re-settle; expiry releases remainder; burst benchmark proving ~1/N external calls', 'done': True}

---

# #340: e2e: 5 tier/lifecycle tests fail on master (pre-existing, surfaced by #334 verification)

**Completed:** no
**Status:** open (filed 2026-06-10 during #334 verification)

TestTierGroupDetection, TestEntitlementChangesOnTierChange, TestScheduledDowngrade, TestRenewMembershipDuplicateTransactionIsNoOp, TestLifecycleServiceUsesMockClock fail identically on origin/master (bun era) and on the sqlc-migration branch — verified via worktree runs on 2026-06-10 during #334 final verification, so they are NOT migration regressions. Observed modes: duplicate key on uq_subscriptions_tenant_subject_tier_group_active across subtests (CleanupSubscriptionsForUser resolves non-UUID test user ids to uuid.Nil via identity.TenantSubjectIDFromString, so per-user cleanup deletes nothing), and mock-clock/lifecycle assertion failures. Distinct from the admin_* family (#333). Suspect shared-suite state + the broken per-user cleanup; fix the cleanup to resolve the tenant subject through billing.tenant_subjects (issuer 'openrails:legacy-user') instead of pure-parsing.

---

# #343: doujins-legacy-billing-import

**Completed:** no
**Status:** planned 2026-06-10; prerequisite/companion to #107 (reconcile corrects whatever the import gets wrong)

One-time migration of legacy doujins user billing state (subscriptions, entitlements, payment-method references) from the legacy machine into OpenRails, ahead of the processor reconciliation (#107). The import is best-effort by design: after it lands, run #107 advisory then enforce against NMI + CCBill (the source of truth) to correct the drift the legacy system accumulated (manual dunning dead for months, failed renewals never downgraded, duplicate subscriptions).

## Metadata

- Category: migration/tooling
- Status: planned 2026-06-10
- Passes: false

## Notes

- Requires production NMI + CCBill credentials wired to the doujins tenant in OpenRails (legacy machine has the real ones; the current dev key is the Mobius SANDBOX account).
- Legacy export source/format TBD (legacy doujins DB); rows land in billing.* under the doujins tenant — make sure tenant_id is stamped correctly (see #336).
- #107 bootstrap mode is the fallback for anything the legacy export can't provide.

## Dunning forensics input

Preserve legacy dunning state verbatim on import — last_retry_at, retry_attempts, next_retry_at (and any legacy dunning/rebill log tables) — do NOT zero these out, they are the evidence #107's dunning-forensics report needs to distinguish 'dunning tried and failed' from 'dunning never ran'. Also produce a quick legacy-side report at export time: per past_due/stale subscription, were rebill attempts ever recorded (count, timestamps, outcomes), and when did the dunning job last touch anything.

## Safe-boot flags for the cutover

Boot OpenRails with production credentials in passive mode: `FEATURE_FLAGS_DUNNING_MODE=dry_run_only` (NOT `off` — off changes rebill-failure semantics to immediate cancellation; dry_run_only runs the workflow, logs due subscriptions, attempts zero charges, preserves retry state) and optionally `FEATURE_FLAGS_DISABLE_ENTITLEMENT_EXPIRATION=true` while reconciling. CRITICAL: the default dunning mode is ON and the periodic job fires every 4h — importing months of stale past_due subscriptions with next_retry_at in the past and booting with defaults would mass-charge every one of them. Set dry_run_only BEFORE first boot with imported data. User-initiated flows (vault saves, requested charges, checkout) are unaffected by these flags.

## Migration-coverage audit vs ~/doujins `migrate legacy` (2026-06-10, verified against doujins-legacy PHP + doujins Go code)

COVERED by the existing migration: user identity via legacy_user_identities + AuthKit (emails w/ fallback resolution, password hashes); Mobius subscription_id + CCBill ccbill_sub_id -> processor_subscription_id; customers_vaults.customer_vault_id -> billing.payment_methods.vault_id + billing.processor_customers; manual_exp/manual_expiry -> admin_grants + entitlements; chargeback/void -> cancel_type; billings_logs/mobius_logs/cancellations_logs -> ClickHouse events; role_user premium WITHOUT a billing source is explicitly detected and recorded as blocked ("hardcoded premium access was not inferred", subscription_entitlements.go:823) — suspicion-2 drift is surfaced at migration time, not silently dropped.

GAP 1 — rebill/dunning attempt history NOT migrated: users_logs (action='rebill-attempt', Rebill-Success/Rebill-Failed, source='RebillingSystem') and vault_logs (full rebill responses) have ZERO references in internal/legacy_migrate. This is THE evidence for 'did manual dunning run / fail'. Fix: migrate both to ClickHouse subscription/payment events like the other log tables, or at minimum archive the legacy dump before decommission; the export-time forensics report must read these tables.

GAP 2 — in-flight dunning arrives dead: legacy status 'void' (the dunning target: RebillingSystem processes void subs with customers_vaults.retry_status='ON') maps to cancelled/cancel_type=merchant (subscriptions.go:63,711); retry state (retries/retry_date/retry_status) survives only as JSON in payment_methods.metadata, never as past_due + last_retry_at/retry_attempts/next_retry_at on billing.subscriptions — so OpenRails dunning (past_due-only) will never resume them. Probably the RIGHT default (auto-resuming charges on months-stale cards = mass-charge hazard), but make it an explicit decision; #107 will surface these as PS-2/PS-3 against NMI's live recurring state and the admin queue disposes of them.

GAP 3 — payment history is initial-transaction-only: one billing.payments row per subscription from subscriptions.transaction_id, and NONE for void/chargeback subs (legacySubscriptionPayment returns nil, subscriptions.go:1261); years of rebill charges exist only in users_logs/vault_logs (not migrated) and at the processors. Acceptable iff #107 PS-4 backfill (NMI transaction query + CCBill DataLink/transaction exports) lands; otherwise local revenue history is permanently incomplete. Also ccbill_rebill (next rebill date) is metadata-only — fine, CCBill dunns on its own side.

Also boot the cutover with `FEATURE_FLAGS_DISABLE_PROCESSOR_SUBSCRIPTION_DELETIONS=true` (#344 kill switch) so no remote NMI subscriptions are deleted while local state is being converged; the dunning window (#344, default 15d) prevents stale-backlog charges even once dunning_mode=on.

**Tasks:**
- [ ] Inventory legacy doujins billing schema + produce export from the legacy machine
- [ ] Map legacy users -> tenant_subjects (authkit identities) under the doujins tenant
- [ ] Import subscriptions / payments / entitlements / payment-method references (correct tenant_id stamping, see #336)
- [ ] Preserve legacy dunning state on import (last_retry_at / retry_attempts / next_retry_at + legacy dunning logs) — evidence for #107 forensics, do not reset
- [ ] Legacy-side dunning report at export time: per stale subscription, rebill attempts recorded (count/timestamps/outcomes); when the dunning job last ran at all
- [ ] GAP 1: migrate users_logs + vault_logs (rebill-attempt evidence) to ClickHouse events — or archive legacy dump pre-decommission; forensics report reads them — filed as doujins #387 (incl. dump-coverage findings: the loaded 2026-06-09 dump lacks users_logs/vault_logs; billing_methods/card_infos in no dump)
- [ ] GAP 2: decide disposition of legacy void+retry_status=ON subs (stay cancelled vs revive as past_due with retry fields) — explicit decision, default stay-cancelled + admin queue via #107 — doujins #387
- [ ] GAP 3: confirm #107 PS-4 payment backfill covers rebill history, or extend migration to synthesize payments from users_logs/vault_logs — doujins #387
- [ ] Wire production NMI + CCBill credentials to the doujins tenant
- [ ] Boot with FEATURE_FLAGS_DUNNING_MODE=dry_run_only (+ optionally DISABLE_ENTITLEMENT_EXPIRATION=true) BEFORE first start with imported data; flip to on only after enforce-mode convergence
- [ ] Run #107 advisory against NMI + CCBill; review the drift report
- [ ] Run #107 enforce; work the admin action queue (duplicate subscriptions -> manual cancel+refund)

---

# #347: SDK: fold entitlements read into openrails.Client (ListActiveEntitlements) — token-issuance enrichment for sibling hosts

**Status:** DONE 2026-06-11 — landed as 8d9d747 and tagged v0.16.0 (committed by the consumer-side session with the owner's go-ahead to unblock dropping the consumers' replace directives; build/vet/unit green, and the method was end-to-end-proven beforehand by doujins' real-OpenRails integration harness). doujins (a0a6ea02) and hentai0 (8ede7df1) pin v0.16.0 with NO replace directives.

#338 follow-up: add `ListActiveEntitlements(ctx, issuer, subject string, at time.Time) ([]EntitlementRecord, error)` to the unified `openrails.Client` — the payer addressed by its EXTERNAL (issuer, subject) identity, zero `at` = now, unknown subject = empty slice not error — implemented by BOTH transports (remote = the existing `/v1/service` by-external-subject entitlements route; embedded = handler-transcribed service call) and covered by the dual-transport conformance script. Purpose: sibling hosts (doujins, hentai0) enrich their access tokens with entitlement claims at mint time through the SDK instead of hand-rolled HTTP clients or direct SQL into the billing schema.

**Tasks:**
- [x] Land the working-tree implementation (interface method + EntitlementRecord wire type + both transports + conformance coverage) — 8d9d747
- [x] Push/tag a version containing it so consumers can pin — v0.16.0; doujins #390 + hentai0 #168 pinned and their replaces dropped

---
# #349: config.FlexiblePort is int16 — ports above 32767 overflow negative and run-server refuses to listen

**Status:** FIXED 2026-06-11 (Claude, committed): FlexiblePort is now a plain int; UnmarshalText parses 32-bit and enforces 1-65535; Validate range-checks integer-typed yaml values that bypass UnmarshalText (0 = unset/default). Unit tests cover 44553/65535/trim/empty plus 65536/0/-1/non-numeric and the Validate path (44553 ok, 70000 and -20983 refused). Doujins can drop freeLocalPortBelow32768 once it consumes a build with this fix.

`config/config.go:25` declares `type FlexiblePort int16`. Any configured port above 32767 — the kernel's default ephemeral range STARTS at 32768 — wraps negative (44553 → -20983) and run-server dies with `listen tcp: address -20983: invalid port`. TCP ports go to 65535; the type should be a plain int with 1-65535 range validation. Doujins' integration harness works around it by allocating only sub-32768 ports (tests/openrails_harness.go freeLocalPortBelow32768).

**Tasks:**
- [x] Widen FlexiblePort to int with 1-65535 range validation in UnmarshalText (keep the string/int flexible parse)
- [x] Doujins follow-up: drop the freeLocalPortBelow32768 workaround once a release carries the fix — done in doujins a0a6ea02 (harness builds the openrails binary from the sibling checkout, which carries the fix; v0.16.0 tag includes it)

---

# #354: batch-first service reads — ListActiveEntitlementsBatch + batch convention for list-shaped consumers

**Status:** PLANNED 2026-06-11 (owner decision, Paul: "make everything batch by default... where it makes sense. Then it's just a difference of the consumer supplying a [] array of user-ids, rather than a single one"). Self-scoped surfaces (/v1/self/* — the token IS the user) stay single; machine surfaces (/v1/service/*) and admin/dashboard reads are batch-shaped wherever the consumer naturally holds a LIST. || OPENRAILS + AUTHKIT SIDES DONE 2026-06-11: branch `batch-354` (pushed, unmerged — master in use by a concurrent session) + authkit v0.21.0. Consumer integrations blocked on the branch merging + a release tag.

## Why

The driving case: admin dashboards listing N users fan out N HTTP calls today. doujins/hentai0 admin user lists enrich per row (authkit `AdminListUsers` calls `Service.ListEntitlements` per user → the host's EntitlementsProvider → one `/v1/service` round-trip EACH). A 100-user admin page = 100 sequential HTTP calls where the engine could answer with ONE query (`subject = ANY($1)`). The same economics apply to any list-shaped read (payer balance dashboards, subscription-status columns). Precedent already in the SDK: `AdmitBatch` (#335) and `SettleWindowItems` are batch; reads should follow.

## Design — entitlements batch (the flagship)

- Route: `POST /v1/service/tenant-subjects/by-external-subject/entitlements/batch`, body `{"issuer": "...", "subjects": ["...", ...], "at": <RFC3339, optional>}` (POST because hundreds of ids don't fit query strings; mirrors /v1/service/admit/batch).
- Response: object keyed by subject — `{"<subject>": [EntitlementRecord, ...], ...}` with an entry for EVERY requested subject; unknown subject = `[]`, matching the single route's empty-not-error semantics. Read batches are ONE SQL query (`ts.subject = ANY($1)` under the issuer's tenant), so no per-item error isolation is needed (unlike write batches): the call errors atomically or answers completely.
- Cap: max 500 subjects per call; over-cap = 400 with an explicit message (no silent truncation). Dedupe repeated subjects server-side.
- SDK: `Client.ListActiveEntitlementsBatch(ctx, issuer string, subjects []string, at time.Time) (map[string][]EntitlementRecord, error)` on BOTH transports + dual-transport conformance coverage. `ListActiveEntitlements` (single) STAYS — it is the token-mint hot path and the degenerate case; internally the remote single route is kept (stable, cacheable GET) and the embedded single call shares the same service function as batch.
- Convention going forward: NEW list-consumable service reads are designed batch-first with the single call as sugar.

## Survey — other batch candidates (build when a consumer materializes, not speculatively)

- `Balance`/`GetCreditAccount` batch by payer tenant-subject ids — payer-balance dashboards (tensorhub-shaped hosts listing payers). Same one-query economics.
- `BudgetStatus` batch by actor — actor-budget dashboards.
- Subscriptions-by-external-subject batch — admin user lists with a subscription-status column (doujins/hentai0 admin pages); today only per-user tenant-admin reads exist.
- Tenant-admin BROWSER surface batch reads (/v1/tenant-admin/*) — NOT needed now: doujins/hentai0 admin list pages render from their own backends (authkit), which is where the batch belongs; revisit only if a browser-direct list view appears.

## Integration (consumers)

- [x] openrails: batch route + handler (one ANY() query, cap 500, dedupe) + SDK method on both transports + conformance test — IMPLEMENTED 2026-06-11 on branch `batch-354` (pushed; NOT merged — master is in use by a concurrent session; merge + tag when free). Conformance run also surfaced that the #347 conformance section was committed unrun (scriptEnv never got issuer/subject, no entitlement seeding — failed on both transports); fixed on the branch, suite green
- [x] COLLAPSE (owner decision 2026-06-11, same day: "I don't want separate 'batch' and 'single' functions... it's always batch"): NO single variant anywhere — `Client.ListActiveEntitlements(ctx, issuer, subjects []string, at) (map[subject][]EntitlementRecord, error)` is THE method and `POST .../by-external-subject/entitlements` is THE route (GET single deleted; orphaned single-path code removed). BREAKING + deploy-coupled: v0.16.0 consumers' GET 404s against a server running this (enrichment degrades to no claims, not an outage) — bump doujins/hentai0 providers with the merge/tag
- [x] authkit: BatchEntitlementsProvider seam + AdminListUsers one-call enrichment (per-user fallback, batch-failure degrades to none) — authkit v0.21.0 (a708e2b), tagged + pushed
- [x] doujins (#390 follow-up): DONE via #356 (cc83bafe) — mint = batch of one, BatchEntitlementsProvider implemented, pins bumped
- [x] hentai0 (#168 follow-up): DONE via #356 (52d9af11)
- [x] cozy-art: DONE via #356 (220a6702, branch rebuild) — embedded host: provider uses Billing.Client() + openrails.SelfIssuer (no service JWTs); dead user_id SQL deleted. Original finding: adopt the openrails client AT ALL — AUDIT FINDING 2026-06-11: cozy-art still runs its own direct-SQL entitlements provider (internal/billing/entitlements_provider.go) querying `billing.entitlements.user_id`, a column REMOVED in the tenant_subject hard cut → its mint-time entitlements are silently broken at runtime. Port the doujins #390 pattern wholesale: openrails.Client remote provider (single now, batch with the authkit seam), bump openrails v0.14.0 → v0.16.0+ and authkit v0.19.0 → v0.20.0 (names-only provider contract), delete the SQL provider
- [x] tensorhub: bumped to v0.17.1 via #356 (77cf648); Balance/GetCreditAccount/BudgetStatus batch variants remain build-when-a-consumer-materializes

---
# #356: release v0.17.0 (batch-only entitlements) + adapt all four consumers

**Status:** DONE 2026-06-11 (owner request: "push as v0.17.0... adapt tensorhub + cozy-art + doujins + hentai0"). Executes the consumer half of #354. v0.17.1 followed same-day: exports `openrails.SelfIssuer` (the issuer embedded hosts address their own UUID users by — cozy-art needed it). Deploy note: doujins/hentai0 consumer bumps must deploy WITH the v0.17 server (their old GET 404s against it; degrades to no claims, not an outage).

**Tasks:**
- [x] Merge batch-354 -> master (resolve drift; clean), verify build/vet/unit/conformance, push, tag v0.17.0 — note: -tags=integration vet fails in internal/river (dunningOutcome vs bool), PRE-EXISTING on master, not from this branch
- [x] doujins cc83bafe: mint = batch of one; BatchEntitlementsProvider implemented; openrails v0.17.0 + authkit v0.21.0; auth integration suite green vs real openrails. BONUS: the test harness now builds the openrails server from the PINNED module version in a throwaway module (embedded migrations) — sibling-checkout dependency + SDK/server version skew eliminated
- [x] hentai0 52d9af11: same adaptation + bumps; build/vet/tests green
- [x] cozy-art 220a6702 (branch rebuild): cozy-art is an EMBEDDED host — provider rewritten over Billing.Client().ListActiveEntitlements with openrails.SelfIssuer (no service JWTs needed); dead user_id SQL deleted; openrails v0.14.0 -> v0.17.1 (test_mode hard-cut mapped onto the mode dial), authkit v0.19.0 -> v0.21.0; build/vet/tests green. Deploy smoke check advised: one known-entitled user keeps premium
- [x] tensorhub 77cf648: openrails -> v0.17.1, ZERO code changes (narrow local interfaces; entitlements not consumed); build/vet/tests green

# #360: solana-token-pricing-degrade-not-die

**Completed:** code done 2026-06-11 (unit-tested; live doujins redeploy pending)
**Status:** IMPLEMENTED 2026-06-11: boot never fails over Solana token pricing; USD1/USDG Pyth feeds added (verified); stablecoin registry + mainnet pricing policy landed; doujins crash-loop fix is a rebuild of the openrails image.

## Metadata

- Category: incident / bug
- Status: implemented
- Passes: true (unit suites; live verification on doujins pending redeploy)

## Incident

doujins compose stack: all openrails replicas crash-looped at boot with
`bootstrap application: initialise runtime: solana token USD1 missing pyth price feed`
(another replica said USDG — Go map iteration order is randomized, so each replica
died on whichever unpriceable token it iterated first).

## Root cause

1. doujins configures NO Solana tokens — only `PROCESSORS_SOLANA_HELIUS_API_KEY`,
   which creates the `processors.solana` entry with an EMPTY token map.
2. `config.Validate` → `validateSolanaProcessor` loops over `proc.Tokens`; with an
   empty map the feed check is vacuous → Validate passes. **This is the
   Validate-ordering answer**: token DEFAULTING happens later, in
   `configureSolanaProcessor` (buildRuntimeWithOverrides), AFTER Validate — so the
   sibling check in config.go never sees the defaulted token set.
3. At runtime `configureSolanaProcessor` filled the empty map with
   `TokensForNetwork("mainnet")` (doujins is mainnet: its `test_mode: true` /
   `TEST_MODE=true` knobs map to nothing — the #355 axis is `test_env`), which
   includes USD1 + USDG (added pre-#352 as deliberately FEEDLESS $1-peg
   stablecoins, see feedlessStablecoins in the old solana_tokens.go).
4. #352 (0b503815) hardcoded `DefaultPythPriceFeeds` to SOL/USDC/PYUSD only and
   made any token outside that map a FATAL boot error — contradicting the
   feedless-stablecoin design in the same file. Result: every default mainnet
   boot was a guaranteed crash-loop.
5. Irony: doujins has no `recipient_wallet`, so Solana payments were already
   disabled-with-warning — the container died pricing tokens for a feature that
   was not even enabled.

## Fix (three-policy, all-cases-boot)

1. **Mainnet-only pricing.** Under test_env (network=devnet) there is NO feed
   requirement: `configureSolanaProcessor` skips classification, and
   `createPythPriceProvider` wraps the Pyth client in `devnetParityPriceProvider`
   (any pricing failure → $1.00 parity; devnet never needs Hermes).
2. **Stablecoins ≈ peg unless depegged.** New hardcoded registry
   `config.knownStablecoins` (mint → peg; mint is the trust anchor so a token
   merely NAMED "USDC" can't buy parity): USDC/USDT/PYUSD/USD1/USDG → usd,
   EURC → eur. Mainnet policy via `config.ClassifySolanaTokenPricing`:
   feed present → feed used (depeg failsafe); USD-pegged registry mint w/o feed
   → kept at $1.00 parity with LOUD warning; quote path is now mint-aware
   (`config.IsUSDPeggedToken` in CalculateTokenQuote).
3. **Cross-currency pegs need pricing.** Non-USD-pegged stablecoin (EURC) w/o
   feed → that TOKEN disabled with loud warning; unknown token w/o feed →
   disabled too. Malformed entries (empty symbol/mint, bad decimals) → dropped
   with warning. `validateSolanaProcessor` mirrors all of this WARN-ONLY.
   In ALL cases the container boots (recipient_wallet pattern).

`IsFeedlessStablecoin` is now DERIVED (registry ∧ no feed), so adding a feed
auto-upgrades a coin to depeg-protected pricing.

## USD1/USDG feed verdicts (verified 2026-06-11)

Both have real, live Pyth mainnet feeds — added to `DefaultPythPriceFeeds`
(this alone fixes doujins):

- USD1 (Crypto.USD1/USD): `0a2425d43486780990d8b63543029e20556be51fd756cca584212f4d539611d4`
- USDG (Crypto.USDG/USD): `daa58c6a3ce7d4b9c46c32a6e646012c17c4a2b24c08dd8c5e476118b855a7da`

Source: Pyth's own Hermes registry (the API production consumes):
`https://hermes.pyth.network/v2/price_feeds?query=USD1` (resp. `USDG`); live
prices confirmed via `/v2/updates/price/latest` (USD1 ≈ $0.9988, USDG ≈ $1.0000,
publish age 6s). EURC registry mint verified via Jupiter token API:
`HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr` ($121M mcap, SPL Token program).

## Boot-path fatal-vs-degrade audit (principle: feature-disabling config degrades; dangerous config stays fatal)

| Check | Today | Verdict |
|---|---|---|
| Invalid/typo'd `mode` (Validate) | fatal | KEEP — typo must not silently run full behavior (#346) |
| Invalid port | fatal | KEEP — nothing to degrade to |
| `test_env` outside development | fatal | KEEP — sandbox creds in prod posture |
| `mode` unset outside dev | fatal | KEEP — explicit posture required |
| Default DB/ClickHouse creds outside dev | fatal | KEEP — dangerous |
| Auth issuers/audience/CORS missing outside dev | fatal | KEEP — degrading = unauthenticated billing API |
| Stripe live key under test_env | fatal | KEEP — real-money credential in sandbox (#347) |
| Stripe test key under live | warn+disable | OK (already degrades) |
| Stripe key missing | warn | OK (already degrades) |
| NMI `security_key` missing (non-dev Validate) | fatal | SHOULD DEGRADE (disable that processor + loud warn) — inconsistent with Stripe sibling; follow-up, not changed here |
| createNMIClients: empty security key (runtime, ANY env) | fatal | SHOULD DEGRADE — latent #352-style crash-loop: a half-configured NMI processor kills the container even in dev (Validate skips dev, runtime doesn't). Follow-up |
| NMI probe = PRODUCTION creds under test_env | fatal | KEEP — real charges possible (#348) |
| NMI probe inconclusive | warn | OK |
| CCBill acc_num/sub_acc missing (non-dev) | fatal | SHOULD DEGRADE (feature-disabling) — follow-up |
| CCBill DataLink half-pair | fatal | KEEP — half-configured pair is always a mistake (captcha precedent) |
| CCBill config missing entirely | info+nil | OK (already degrades) |
| Captcha half-pair / unknown provider | fatal | KEEP — ambiguous intent |
| billing_hot_path unknown fail_policy | fatal | KEEP — fail-open/closed is a money/safety dial (#248) |
| Unknown processor type / missing type | fatal | KEEP — typo'd config |
| DB unreachable / URL undetermined / migrations unapplied | fatal | KEEP — core dependency. Wart: validateDatabase uses `log.Fatal` (os.Exit) instead of returning the error |
| RLS posture violation with RequireRLS | fatal | KEEP — tenant isolation (#227) |
| River producer pool failure | fatal | KEEP — DB again |
| Redis ping failure / unconfigured | warn+permissive | OK (already degrades) |
| ClickHouse init/migrations failure | warn, analytics off | OK |
| EmailService init failure | warn, email off | OK |
| Solana recipient_wallet missing | warn, payments off | OK (the pattern this fix mirrors) |
| Solana token pricing (was: missing feed) | WAS fatal | FIXED → degrade per policy above |

## Doujins-side recommendation (NOT applied — their repo untouched)

- USD1/USDG come from OpenRails' own `DefaultSupportedTokens()` (mints
  `USD1ttGY1N17NEEHLmELoaybftRBUSErhqYiQzvEmuB`,
  `2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH`) — doujins configures no
  tokens. **No doujins config change is required**: rebuild the openrails image
  (compose builds from `../openrails`) and the stack boots.
- Stale knob: `e2e/openrails.yaml` `test_mode: true` (top-level) and compose
  `TEST_MODE=true` map to NOTHING since #355 — openrails there runs with live
  credential posture + mainnet Solana. If sandbox semantics are intended (their
  Mobius account IS sandbox), set `test_env: true` / `TEST_ENV=true` instead;
  note that also derives devnet Solana (#349 — the axes are coupled by design).
- `PROCESSORS_SOLANA_RECIPIENT_WALLET` is unset, so Solana payments are disabled
  regardless; either configure the wallet or drop the Helius key if unused.

**Tasks:**
- [x] Root-cause the crash-loop incl. why Validate never fired (defaults injected post-Validate)
- [x] Add verified USD1/USDG Pyth feed ids to DefaultPythPriceFeeds (Hermes-verified, live)
- [x] Stablecoin registry (mint → peg) + ClassifySolanaTokenPricing policy
- [x] configureSolanaProcessor: degrade-not-die (devnet exempt; parity; per-token disable)
- [x] validateSolanaProcessor: warn-only mirror; createPythPriceProvider: devnet parity wrapper
- [x] CalculateTokenQuote mint-aware USD-peg check
- [x] Policy-matrix unit tests (config + app + solana module + pyth suites green)
- [ ] Rebuild/redeploy doujins openrails image and confirm boot + supported-tokens endpoint
- [ ] Follow-up: degrade NMI/CCBill missing-credential fatals (audit table) — separate issue if accepted

---

---

# #361: delegated-tokens-slug-only-tenant-identity

**Completed:** no
**Status:** IN_PROGRESS 2026-06-11 (UPDATED AGAIN same day — authkit v0.23.0 PUBLISHED):
the profile is now ISSUER-ONLY, a hard cut beyond v0.22.0's slug-only cut. Delegated
tokens carry NO tenant claims at all (neither `tenant_id` nor the `tenant` slug): the
VALIDATED `iss` IS the tenant identity, resolved purely from the receiver's issuer
registry. `DelegatedAccessParams.Tenant` and `DelegatedPrincipal.Tenant` are removed;
the verifier rejects `tenant` (`delegated_access_has_tenant`) same as `tenant_id`;
`IssuerOptions.TrustedResourceAccount` is replaced by `IssuerOptions.TenantSlug`
(issuer-registry data, never compared against claims — the mismatch check is gone).
Tenant slug renames are fully TRANSPARENT to in-flight tokens. openrails ADAPTED
2026-06-11 with a DIRECT pin to v0.23.0 (no replace directive — see openrails task
notes below). Host cleanups still pending.

Host apps (doujins, hentai0) must never need to know their OpenRails tenant uuid. A
tenant knows its chosen name (slug) the way a user knows their username; the uuid is
OpenRails-internal database identity. The desired mint shape for a host talking to
OpenRails is exactly:

```
iss:    https://doujins.com   (or the hentai0 issuer — distinct issuers, ONE tenant)
aud:    openrails
tenant: doujins               (the slug; same value from BOTH issuers)
signed by the host's tenant key (registered JWKS)
```

No `tenant_id` claim. If the tenant is renamed doujins -> doujins2 inside OpenRails,
outstanding tokens carrying `tenant: doujins` stop validating (slug-claim cross-check)
and hosts simply mint new ones with `tenant: doujins2` — short TTLs (5m) make this a
non-event, and it is the CORRECT behavior: the slug is the host-facing identity.

## Metadata

- Category: design-debt / cross-repo coordination
- Status: planned
- Passes: false

## Current state (verified 2026-06-11)

- OpenRails ALREADY resolves delegated tokens the right way: `ResolveDelegated`
  (internal/controlplane/delegated.go) pins the tenant from the VALIDATED `iss` via the
  issuer registry (`billing.tenant_delegated_issuers` -> tenant; issuer is globally
  unique so a signing key can only ever reach its own tenant), then cross-checks the
  `tenant` SLUG claim if present. The `tenant_id` claim is NEVER read on this path.
- The ONLY thing forcing hosts to know the uuid is authkit's v0.19 delegated-access
  token profile: `MintDelegatedAccessToken` errors `tenant_id required`
  (authkit/http/delegation.go) and the verifier rejects `missing_tenant_id`
  (authkit/http/verifier.go ~660). Authkit's resource-account binding check already
  accepts a match on EITHER the slug or the uuid. (STALE as of the v0.22.0 hard cut:
  the binding check now matches the slug ONLY, and a token carrying `tenant_id` is
  rejected outright.)
- Consequence: doujins/hentai0 carry `BILLING_TENANT_ID` env plumbing
  (DOUJINS_BILLING_TENANT_ID / HENTAI0_BILLING_TENANT_ID -> openrailsmint minter), a
  random uuidv7 that goes stale on every dev-stack reset, with "SELECT id FROM
  billing.tenants WHERE slug='doujins'" lookup instructions sprinkled in comments.
- Security note: an issuer-asserted `tenant_id` is only trustworthy when bound to the
  issuer registration server-side, which OpenRails already does via the registry — so
  the claim is at best a cache of the binding, never a substitute for it.
- One receiver DOES key on the claim: tensorhub
  (internal/api/platform_delegated_principal.go) makes the token's `tenant_id` the
  principal's EffectiveTenant and persists user files under `{tenant_id}/...` paths,
  and its middleware independently 401s when the claim is empty. Tensorhub keeps its
  guarantee with zero changes once authkit relaxes; moving tensorhub to issuer-pinned
  resolution is tensorhub's debt, out of scope here.

**Tasks:**
- [x] authkit: make `tenant_id` OPTIONAL on the delegated-access profile — drop the
      mint-time `tenant_id required` error in `MintDelegatedAccessToken` and the
      verify-time `missing_tenant_id` rejection; keep the claim pass-through when
      present and keep `validateDelegatedIssuerResourceAccount` slug-or-uuid binding
      unchanged. Document that receivers which key on `tenant_id` (tensorhub) must
      enforce presence themselves. DONE 2026-06-11 in the authkit working tree:
      http/delegation.go (mint), http/verifier.go (verify), http/claims.go docs, new
      http/delegation_slug_only_test.go (slug-only mint+verify; claim absent not empty;
      slug-trusted issuer accepts slug-only; uuid-trusted issuer fails closed without /
      with-wrong uuid). Full `go test ./...` green.
      SUPERSEDED 2026-06-11 (v0.22.0 HARD CUT): `tenant_id` is no longer optional — it
      is FORBIDDEN. Mint cannot write the claim (`DelegatedAccessParams.TenantID`
      removed) and the verifier rejects any delegated token carrying it
      (`delegated_access_has_tenant_id`). There is no pass-through and no legacy
      acceptance; receivers that keyed on the claim (tensorhub) must move to
      issuer-pinned resolution before adopting v0.22.0.
- [x] authkit: version bump + release. DONE 2026-06-11: v0.23.0 PUBLISHED — and it
      EXTENDS the hard cut to the full ISSUER-ONLY profile: delegated tokens carry NO
      tenant claims at all (neither `tenant_id` nor the `tenant` slug). The VALIDATED
      `iss` IS the tenant identity; receivers resolve the tenant purely from their
      issuer registry. `DelegatedAccessParams.Tenant` and `DelegatedPrincipal.Tenant`
      are removed, the verifier rejects a `tenant` claim (`delegated_access_has_tenant`)
      same as `tenant_id`, and `IssuerOptions.TrustedResourceAccount` is replaced by
      `IssuerOptions.TenantSlug` (issuer-registry data; never compared against claims —
      the slug mismatch check no longer exists). Consequence: tenant slug renames are
      fully TRANSPARENT to in-flight delegated tokens.
- [x] openrails: bump the pinned authkit; add/adjust a controlplane test proving a
      delegated token WITHOUT `tenant_id` resolves end-to-end (issuer-pinned tenant,
      slug cross-check, TouchTenantSubject) — codifying that openrails never needs
      the claim. DONE 2026-06-11: TEMPORARY go.mod replace -> /home/fidika/authkit
      until v0.22.0 is tagged; `ResolveDelegated` needed NO logic change (already
      issuer-pinned + slug cross-check, never read the claim). New integration test
      `TestDelegatedTokenWithoutTenantIDResolvesEndToEnd` (issuer_federation_
      integration_test.go) pins the wire shape (no tenant_id claim) + full resolution
      incl. tenant_subjects upsert + idempotency; new unit test
      `TestDelegatedVerify_RejectsTenantIDClaim` (delegated_test.go) pins the HARD CUT
      (a hand-signed token carrying tenant_id is REJECTED, not just optional-and-
      ignored). mintDelegated/mintFed helpers no longer set TenantID. Full unit suite
      + controlplane integration suite (testcontainers) green.
      RE-ADAPTED 2026-06-11 (authkit v0.23.0 issuer-only): openrails now pins
      `github.com/open-rails/authkit v0.23.0` DIRECTLY (no replace directive).
      `ResolveDelegated` no longer reads `principal.Tenant` or cross-checks any slug
      claim (the claim cannot exist); `ResolvedDelegated.Tenant`/`TenantSlug` come
      purely from the issuer-registry resolution (tenantForIssuer).
      `reloadDelegatedIssuers` drops the per-issuer slug option entirely (openrails
      never reads the service-JWT principal's Tenant — ResolveServiceJWT resolves
      live from billing.tenants via tenantForIssuer). The end-to-end test now
      asserts the minted token carries NO `tenant` claim either; new unit twin
      `TestDelegatedVerify_RejectsTenantSlugClaim` pins `delegated_access_has_tenant`.
- [x] openrails: add a slug-rename semantics test: after renaming a tenant's slug, a
      token carrying the OLD slug claim is rejected (tenant-claim mismatch) and a
      token with the NEW slug validates — documenting rename behavior instead of
      discovering it in prod. DONE 2026-06-11: `TestSlugRenameSemantics`
      (issuer_federation_integration_test.go): old-slug token rejected even BEFORE
      registry reload (ResolveDelegated DB cross-check) and after; new-slug token
      validates after `reloadDelegatedIssuers` (the operational step that refreshes
      the verifier's TrustedResourceAccount slug binding); tenant uuid unchanged.
      SUPERSEDED 2026-06-11 (v0.23.0): that rejection behavior is GONE — tokens carry
      no slug claim, so the test is REWRITTEN as rename-TRANSPARENCY: the SAME
      in-flight token resolves to the SAME tenant uuid before the rename, immediately
      after it (no reload needed), and after `reloadDelegatedIssuers`; only the
      resolved slug changes. Renames are a non-event for hosts.
- [x] doujins: drop the tenant-uuid mint requirement (internal/billing/openrailsmint/
      minter.go) and ALL `BILLING_TENANT_ID` plumbing: docker-compose env for both the
      doujins and hentai0 services, DOUJINS_BILLING_TENANT_ID / HENTAI0_BILLING_TENANT_ID
      in .env, scripts/e2e/lib.sh E2E_BILLING_TENANT_ID wiring, and the "look it up
      after openrails migrates" comments. Token mints carry slug only.
      DONE — VERIFIED 2026-06-12: the cleanup landed in doujins commit f4faf69c (on
      origin/master; rode along with "require verified registration bool" — touched
      minter.go, minter_test.go, docker-compose.yaml, .env.example, scripts/e2e/lib.sh,
      cmd/e2e-mint-self, config, di). Per the v0.23.0 issuer-only hard cut the mint is
      now ISSUER-ONLY (no tenant_id AND no tenant slug claim — minter_test.go pins both
      absent). Grep-zero across the repo for BILLING_TENANT_ID / DOUJINS_BILLING_TENANT_ID
      / HENTAI0_BILLING_TENANT_ID / E2E_BILLING_TENANT_ID. go build/vet clean;
      ./internal/billing/... tests green. Only slug CONFIG remains
      (BILLING_TENANT_SLUG, never a token claim) — intentional host-side config.
- [x] hentai0: same cleanup in its own repo (its embedded minter + config).
      DONE — VERIFIED 2026-06-12: already committed AND pushed as hentai0 4cca9a53
      ("wip snapshot: issuer-only delegated tokens (authkit v0.23)") — HEAD ==
      origin/master. local_minter.go mints issuer-only; local_minter_test.go pins
      both tenant and tenant_id claims absent. Grep-zero for all *_BILLING_TENANT_ID
      vars. go build/vet clean; ./internal/clients/billing tests green. Config keeps
      BILLING_TENANT_SLUG (display/config only, never a claim).
- [x] e2e: doujins tests/openrails_harness.go + e2e flows pass with no tenant uuid
      configured anywhere (fresh stack, bootstrap manifest, register, delegated
      self-token, checkout).
      VERIFIED 2026-06-12 (harness portion): tests/openrails_harness.go carries no
      tenant uuid anywhere (bootstrap manifest is slug+issuer only);
      `go test -tags integration -run TestAuthIntegration ./tests/` PASSES (53s,
      testcontainers + real openrails binary: bootstrap, register, tokens, premium
      entitlement, admin) with zero tenant-uuid config. The LIVE docker-compose e2e
      (fresh stack -> checkout) is deferred to post-rebuild — the local stack is
      being wiped/rebuilt; hentai0's TestLive* auth_billing suite likewise needs the
      rebuilt stack.
- [x] RESOLVED 2026-06-11 (caveat eliminated, not deferred): tensorhub moved to
      issuer-pinned resolution the same day (tensorhub agents/progress.md #474) — its
      EffectiveTenant now resolves from the validated issuer via authkit's
      profiles.tenant_issuers registry (issuer -> slug -> tenants.id, rename-proof via
      tenant_renames), identical uuid to the old claim so no storage migration; and
      cozy-art dropped PLATFORM_TENANT_ID entirely. NO host anywhere needs a receiver
      uuid anymore; nothing to caveat in the release notes.

## Identity principles (clarified 2026-06-11)

Two different identifiers, two different rules:

- TENANT identity crossing the host->openrails boundary is the SLUG (mutable,
  host-chosen, like a username). The tenant's openrails uuid never leaves openrails.
- DELEGATED USER identity crossing the boundary (`delegated_sub`) is the HOST's
  canonical user uuid — stable precisely so host-side username renames (cozy ->
  cozy2) never fracture the openrails payment account. The host owns and trivially
  knows its users' uuids. VERIFIED already implemented: doujins MintSessionToken
  passes the authenticated `userCtx.User.ID` (never request input), hentai0
  MintSelfTokenWithIdentity likewise passes the canonical subject; openrails
  TouchTenantSubject upserts the payable subject keyed on (tenant_id, issuer,
  subject) verbatim. No changes needed.

## OPEN QUESTION (Paul): per-issuer payable-subject keying vs #259 shared namespace

`billing.tenant_subjects` is keyed (tenant_id, ISSUER, subject) — the integration test
explicitly asserts the same canonical subject from the doujins vs hentai0 issuers
creates TWO different payable subjects — and entitlement reads are issuer-scoped
(`ListActiveEntitlementRecordsByExternalSubjects(issuer, subjects)`). But the #259
comment in controlplane/delegated.go says delegated_sub must be the canonical shared
user id "so a token from EITHER issuer resolves to the SAME OpenRails billing
account". As implemented, a premium subscription bought through a doujins-issued
token will NOT surface to an entitlement query made with a hentai0-issued token for
the same canonical user. Decide: (a) intended — the two sites bill separately despite
the shared user store (then fix the #259 comment), or (b) shared-namespace tenants
need (tenant_id, subject) resolution — a schema/lookup change with a merge migration,
guarded per-tenant so single-issuer tenants keep fail-closed per-issuer isolation.

## Rollout order

authkit release (profile relaxation, backward compatible — tokens WITH the claim stay
valid) -> openrails pin bump + tests (no behavior change) -> doujins/hentai0 cleanup
(env vars deleted; mints go slug-only). No migration, no data movement; outstanding
tokens are unaffected either way.

# #365: intent-account-fingerprint-guard: park pending intents when provider credentials point at a different account

## Metadata

**Completed:** yes
**Status:** IMPLEMENTED 2026-06-12 (same day as planned). Migration 013 +
sqlc; FingerprintSource (intents/fingerprint.go) — NMI resolver REVISED
2026-06-12 per Paul (key-hash rejected: it breaks key rotation): the
fingerprint is now the MERCHANT IDENTITY from query.php report_type=profile
("nmi:<company> <email>"; verified live against the Mobius sandbox), lazily
fetched + cached per security key exactly like the Stripe acct-id resolver,
so same-account key rotation refetches and matches (no false park); Store stamps at enqueue (best-effort,
NULL on unresolvable); Runner parks on execute mismatch AND defers verify on
mismatch; wired at every construction site (intentRunner, both intent workers,
dunning fallback runner, both deferred-delete schedulers, marker sweep);
`billing intents refingerprint --provider --tenant --yes` escape hatch;
fingerprint surfaced in `billing intents` table/json + GET /v1/admin/intents;
unit tests (runner guard execute/verify, NMI determinism, stripe fetch/cache/
rotation, provider-map resolution) green; docs (operations.md account-guard
runbook, README intents section). All checklist items below done.

## Problem

Intents on the provider intent ledger (#358) are bound to credentials only by
(tenant_id, provider-key string). `h.Clients[intent.Provider]` resolves whatever
credentials are CURRENTLY configured under that key. Swap the credentials behind
"mobius" or "stripe" to a DIFFERENT provider account while intents are pending and
the executor will run them against the new account.

Severity is provider-shaped:
- Stripe: accidentally safe — object ids (ch_/sub_/re_) are globally unique, wrong
  account = "no such object" = terminal failure. Noisy but never wrong-object.
- NMI: genuinely dangerous — subscription ids and customer_vault_ids are small
  NUMERICS. On a different NMI account the same id can exist and belong to a
  different merchant's customer: nmi_delete_subscription deletes an unrelated
  subscription; manual_rebill charges a STRANGER'S CARD. Worse, verify-then-execute
  inverts: "absent = success" answered by the wrong account falsely resolves the
  intent and clears the local DeletionScheduledAt marker.
- The verify pass runs with NO mode gate (read-only), so wrong-account
  verification is reachable even under mode=readonly.

## Design

Stamp an account fingerprint on every intent at enqueue; compare at execute AND
verify; PARK (never fail) on mismatch. Park-not-fail matches ledger philosophy:
swap back -> queue drains; intentional swap -> parked rows surface in
`openrails intents` for explicit refingerprint/cleanup.

Fingerprint per provider (FingerprintSource resolved from live clients/config):
- NMI-backed providers: "nmi:" + sha256(SecurityKey)[:16]. Local, always
  available. Security-key rotation (same account) WILL park pending intents —
  acceptable: rotation is rare, the escape hatch is one command, and false-park
  is the safe failure direction. (NMI has no whoami API to do better.)
- stripe: "stripe:" + account id from GET /v1/account (raw HTTP through the
  stripeapi choke point; lazy on first use, cached per secret key — key rotation
  within the SAME account refetches and matches, NO false park). Fetch failure ->
  source returns unknown -> check skipped this pass (warn log); a wrong-account
  swap still gets caught on a later pass / fails at execution like today.
- solana: EXEMPT, documented — addresses are globally-unique pubkeys; a PDA from
  the wrong cluster/keypair simply does not exist, execution cannot hit a wrong
  object.
- NULL fingerprint on the intent (pre-#365 rows) = grandfathered: executes
  without the check (otherwise every existing deployment parks its whole queue
  on upgrade).

## Tasks

- [x] Migration 013_intent_account_fingerprint: ALTER TABLE
      billing.provider_intents ADD COLUMN account_fingerprint text (nullable;
      comment documents NULL = pre-guard, grandfathered).
- [x] provider_intents.sql: EnqueueProviderIntent inserts account_fingerprint and
      refreshes it on the pending/superseded/expired conflict branches (latest
      enqueue wins, same as payload); new RefingerprintProviderIntents update
      (live statuses only: pending/failed_retryable/unknown_needs_verify) for the
      escape hatch. Regenerate sqlc (task sqlc).
- [x] intents package: FingerprintSource interface
      (Fingerprint(ctx, provider) (string, bool)); Store.Fingerprints field —
      Enqueue stamps best-effort (resolver miss/error logs + stamps NULL, never
      fails the enqueue); Runner.Fingerprints field — executeOne parks on
      mismatch after GateExecution, verify pass MarkUnknown-with-reason on
      mismatch (verify must be guarded too, see Problem).
- [x] fingerprint sources: nmi.NMIClient.AccountFingerprint() (sha256 of
      SecurityKey); stripe account fetch via stripeapi GET /v1/account with
      per-key cache (internal/integrations/stripeapi or a small
      internal/intents/fingerprint.go composite over Runtime deps).
- [x] wiring: Runtime.AccountFingerprints() built once from NMIClients + stripe
      config; attach at every Store/Runner construction site that enqueues or
      executes: app/river_register.go intentRunner, app/deferred_delete_scheduler.go
      (both NewStore sites), river/jobs_provider_intents.go (both workers — add
      the source to worker deps), river/jobs_dunning.go. jobs_subscription_manage
      only supersedes; no stamp needed.
- [x] escape hatch CLI: `openrails intents refingerprint --provider=... --tenant=...
      --yes` — re-stamps live intents to the CURRENT fingerprint after the
      operator confirms the account is intentionally the same/new; prints count.
      (Covers legit NMI security-key rotation.)
- [x] surfaces: account_fingerprint (short form) in `openrails intents` table +
      GET /v1/admin/intents JSON; park reason "provider account changed since
      enqueue (intent <fp> vs current <fp>)" is visible in LAST FAILURE already.
- [x] tests: store stamps at enqueue; revive-conflict refreshes stamp; runner
      parks on execute mismatch; verifier defers on mismatch; NULL grandfathered
      executes; stripe cache refetch-on-key-change; refingerprint updates only
      live statuses.
- [x] docs: operations.md credential-rotation runbook paragraph (drain-or-
      refingerprint before pointing a provider key at a DIFFERENT account; NMI
      key rotation parks pending intents -> run refingerprint); README intents
      section one-liner.

# #366: materialize the migration backlog under mode=limited: dunning decisions become parked intents + local convergence, not a log line

## Metadata

**Completed:** yes (2026-06-12; the DeferDelete-under-park verification tail rides on the existing #344 deferred-delete coverage — closed)
**Status:** IMPLEMENTED 2026-06-12. The "CRITICAL prerequisite" turned out to
be ALREADY satisfied structurally: the dunning worker stamps ExpiresAt =
dunning-window end on every manual_rebill intent (ClaimDue never claims past
expires_at; ExpireOverdue sweeps), and ManualRebillHandler.CheckRelevance
re-checks still-past_due + same-period at execution — so a parked charge
cannot fire stale; no handler change was needed. Worker change shipped:
limited-mode demotion to dry_run_only replaced with materialize (window-expiry
local cancel+downgrade applies via the unchanged lifecycle path incl.
DeferDelete; in-window charges Enqueue-only as parked system-origin intents —
deliberately NO claim, because claimDunningAttempt writes last_retry_at which
is the dunning-forensics evidence imported from legacy, and NO
payment-method failure policy). readonly unchanged (pure observer).
Integration tests green vs real PG: materialize records pending/system/
window-bounded intent WITH the #365 fingerprint, zero NMI calls, last_retry_at
untouched, idempotent re-pass; window-expiry still cancels locally. Docs:
README safe-boot section (visible-not-implied backlog + cutover sequence),
operations.md materialized-backlog paragraph.

## Original status

**Status:** PLANNED 2026-06-12 (Paul: "if we migrate over a user whose account
failed to be rebilled, we'd want to queue up some dunning actions for it, or at
least queue up that it should be deleted from NMI, and we'd also want to
downgrade the user's entitlements")

## Problem

A legacy migration imports HISTORICAL state (stale past_due subs, failed
rebills, dead-at-processor subscriptions) — state that implies WORK. Today none
of that work surfaces until MODE=full:

- jobs_dunning.go:126 demotes dunning to dry_run_only under limited mode: the
  worker queries due subscriptions, logs a count, does NOTHING. No manual_rebill
  intents, no window-expiry cancel+downgrade, no deferred NMI deletes.
- Consequence 1 (visibility): `openrails intents` shows an EMPTY ledger after a
  migration — the "drain forecast" dry-run is vacuous exactly when the operator
  most needs it (pre-cutover). The backlog is implicit in subscription rows.
- Consequence 2 (correctness): users whose subs died months ago on the legacy
  system keep premium entitlements locally until full mode (or a reconcile fix
  vs the processor) finally converges them.
- The decision logic (staleness window: charge within window, cancel+downgrade
  past it) and the execution are fused in one full-mode pass; there is no way
  to get decisions early and execution later.

The migration itself must NOT enqueue intents (rejected): it would duplicate
the staleness/schedule derivation at import time, rot as state changes, and
violate the state-scan principle (workers derive work from state; the ledger
is an execution outbox, not a second source of truth).

## Design

Split decision from execution using machinery that already exists: the
origin x mode gate already parks system-origin intents under limited. So let
the dunning worker RUN its scan under limited — every provider action it
decides on becomes an enqueued intent that the gate parks (visible, durable,
drains on MODE=full), and the purely-local lifecycle consequences apply
immediately (limited mode only restricts PROVIDER writes; reconcile fix
already does local convergence under readonly by design).

Under mode=limited, dunning_mode=on ("materialize" behavior — replaces the
dry_run_only demotion):
- window-expired due subs (the bulk of a migration backlog): apply the local
  cancel + entitlement downgrade NOW (same code path as full mode; respects
  FEATURE_FLAGS_DISABLE_ENTITLEMENT_EXPIRATION for operators who want a frozen
  system) and schedule the deferred NMI delete intent — system-origin, parks,
  shows in the drain forecast.
- within-window due subs: enqueue the manual_rebill intent through the normal
  EnqueueAndExecute path — the gate parks it (system-origin under limited);
  the charge fires on MODE=full via the scheduled executor.
- readonly mode: UNCHANGED (stays dry_run_only/skip). Readonly is the
  strictly-observing forensics boot; materializing lifecycle changes there
  would taint observation. The operator path is readonly (observe) -> limited
  (materialize + converge) -> full (execute).

CRITICAL correctness prerequisite: a parked manual_rebill intent may sit for
weeks before the mode lifts. The staleness window MUST be re-derived at
EXECUTION time (handler relevance/execute), not only at scan time — verify
manual_rebill.CheckRelevance covers (a) sub still past_due with this period,
(b) still INSIDE the staleness window at execution; add the window check if
missing, with outcome superseded ("window expired while parked") + the worker
or a follow-up scan applies cancel+downgrade.

Interplay with #365: parked backlog intents carry the account fingerprint of
the creds that enqueued them; a cutover that also swaps to a different
provider account parks them with the changed-account reason instead of firing
— exactly the double-safety wanted for migration cutovers.

Idempotency: enqueue is effectively-once per logical intent (tenant,
idempotency_key); re-scans refresh pending rows rather than duplicating. A
later full-mode scan that re-decides the same charge converges on the same
key.

## Tasks

- [x] manual_rebill handler: NO CHANGE NEEDED — ExpiresAt(=window end) bounds
      claiming and CheckRelevance re-checks past_due+period at execution
      (verified 2026-06-12).
- [x] jobs_dunning.go: replace the limited-mode dry_run_only demotion with
      materialize behavior — run the scan; window-expiry path applies local
      cancel+downgrade (existing code; respect
      FEATURE_FLAGS_DISABLE_ENTITLEMENT_EXPIRATION) + DeferDelete enqueue;
      charge path enqueues via intentRunner (gate parks). readonly: unchanged
      skip. Log line states the materialize totals (parked charges, local
      cancellations, deferred deletes).
- [ ] verify DeferredDeleteScheduler + FailMembership paths run cleanly when
      the resulting intents only park (no regressions from execution never
      happening inline).
- [x] tests: limited-mode dunning run on a seeded backlog — asserts N parked
      manual_rebill + M parked nmi_delete intents, local cancellations +
      downgrades applied, NO provider calls (fake NMI client asserts zero
      writes); entitlement-expiration flag freezes the local part; flip to
      full -> executor drains the parked set.
- [x] docs: README safe-boot section + operations.md cutover posture — the
      operator story becomes: migrate -> boot limited -> first dunning cycle
      materializes the backlog -> `openrails intents` shows the real drain
      forecast -> review/refingerprint -> MODE=full drains it. Update the
      "All paused work is delayed, not lost" paragraph to mention the parked
      backlog is now VISIBLE.

# #367: subscription liveness sync — scheduled provider-truth convergence for SILENT lapsed subscriptions (NMI + Stripe)

## Metadata

**Completed:** yes (2026-06-12; shipped in v0.20.0 — 9 outcome-class integration tests green)
**Status:** IMPLEMENTED 2026-06-12 — SubscriptionLivenessWorker (4h, RunOnStart, skip-readonly) + ListSilentLapsedSubscriptions cohort + NMI probes (ProbeSalesByOrderID/GetRecurringLiveness, findSuccessfulSale extracted) + Stripe prober; all four converge actions through lifecycle services; 9 integration tests incl. months-stale-never-charges; awaiting review

## Problem

A subscription whose period lapses with NO provider signal (no rebill-success,
no rebill-failure webhook) currently converges only when a human runs
`reconcile`. Locally the entitlement window expires (fail-closed, good) but
the row sits as a zombie (status=active, period in the past): dunning never
sees it (it scans status=past_due only), so a user whose card WAS charged at
NMI but whose webhook was lost has paid and lost access until a manual
reconcile fix backfills (PS-4). Detection exists (audit SS-1) but is also
manual. The migration cutover makes this acute: imported subs can be months
past their period end with unknown remote liveness.

## Design (Paul, 2026-06-12)

Period end + slack passes with no signal -> reach out to the provider for
THAT subscription's state; converge local state; route the consequence into
the existing pipelines. Must work regardless of how stale the gap is.

Mapped onto what exists — this is a state-scan worker, NOT a new queue:

- COHORT (the "no signal" detector): NMI-backed (and stripe) subs with
  status='active' AND current_period_ends_at < now - slack (slack ~1 day)
  AND no recorded payment for the period. past_due rows are EXCLUDED — that
  cohort is dunning's, already owned end to end. Re-derived every cycle, so
  "queue + dedupe the check when the provider is unreachable" is inherent:
  an unreachable provider just leaves state unchanged and the next cycle
  retries — no durable read-queue needed (the intent ledger stays
  mutations-only by design).
- PROBE (read-only, two lookups reusing dunning-verify + reconcile fetcher
  queries): (a) the period's transactions by order reference/subscription id
  — charged? declined?; (b) the remote recurring record — alive?
  next_billing_at?
- CONVERGE, by probe outcome:
  - charged       -> RenewMembership repair (same path as dunning's
                     verified-existing-sale repair; entitlements + payment
                     backfill, never a second charge).
  - declined      -> FailMembership + #359 retry schedule -> dunning OWNS it
                     from here (its manual_rebill intents, staleness window,
                     window-expiry cancel+downgrade all apply unchanged).
  - never attempted, remote alive with future next_billing_at (the
    date-misalignment case) -> adopt the remote period timestamps
    (PS-3-equivalent local write); entitlement window extends through the
    normal renewal-shaped path only when a charge actually exists — period
    adoption alone never grants access.
  - remote absent/terminal -> PS-2-equivalent: cancel locally + revoke
    subscription-sourced entitlements (no remote delete needed — it is
    already gone; if a deletion IS needed it goes through the nmi_delete
    intent, whose verify-then-execute finalize IS the "atomically linked"
    two-system sync Paul described).
- STALENESS: months-old gaps safe by construction — charging decisions stay
  inside dunning (whose derived staleness window cancels instead of
  charging); the liveness sync itself never charges, it only reads and
  converges.
- MODE GATING: probes are reads; convergence is local writes + parked
  intents. Run under full AND limited (consistent with #366 materialize);
  SKIP under readonly (pure observer, consistent everywhere).
- PROVIDERS: NMI (query.php per-sub: the whole point); stripe (GET
  /v1/subscriptions/{id} + latest invoice — lower urgency since Stripe runs
  its own dunning and retries webhooks for days, but repairs long outages);
  CCBill EXCLUDED (no per-record API — the existing 6h DataLink bulk worker
  is its version of this); Solana EXCLUDED (the cranker is already
  pull-based; there were never webhooks).
- CADENCE: every 4-6h, RunOnStart=true (a reboot after an outage is exactly
  when the silence cohort is largest). Alert-style summary log per pass
  (cohort size, repaired/failed/adopted/cancelled/unreachable counts).

## Interplay with #368 (renewal grace)

The liveness sync is #368's RESOLVER: its charged-repair revokes grace and
grants the real paid window; its confirmed-dead outcomes revoke grace with
the cancellation; its period-adoption outcome (provider clock misalignment)
re-anchors the next expectation. Grace guarantees the user keeps access
during the uncertainty the probe is resolving.

## Relationship to reconcile (#107)

NOT a replacement and does not violate reconcile's manual-only decision:
reconcile stays the full-surface manual batch tool (all PS types, findings
ledger, admin queue, forensics). The liveness sync is the always-on, narrow,
per-subscription slice for exactly one failure mode (inbound silence), acting
through the normal lifecycle services so every action is evidence-logged.
Overlap is convergent by idempotency: a reconcile fix run before/after a
liveness pass lands on the same state.

## Tasks

- [x] cohort query (ListSilentLapsedSubscriptions: processor-filtered,
      active, period_end < now - slack, no payment for period) + sqlc.
- [x] NMI prober: reuse/extract dunning's findSuccessfulSale (charged?) +
      QueryRecurringSubscriptions (remote liveness, next_billing_at);
      classify charged/declined/no-attempt-alive/absent.
- [x] converge actions via lifecycle services (RenewMembership repair,
      FailMembership+schedule, period adoption, cancel+revoke); all
      idempotent; respect DISABLE_ENTITLEMENT_EXPIRATION for the revoke path.
- [x] stripe prober (GET subscription + latest invoice through stripeapi)
      with the same classification.
- [x] River worker + periodic registration (4-6h, RunOnStart=true), mode
      gate (skip readonly), pass-summary log.
- [x] tests: integration vs real PG with fake NMI server per outcome class
      (charged-repair grants access + backfills payment exactly once;
      declined routes into dunning state; misalignment adopts period without
      granting access; absent cancels+revokes; unreachable leaves state
      untouched and the next pass retries); months-stale fixture proves no
      charge is ever issued by this worker.
- [x] docs: operations.md (the silence cohort now has an automated owner;
      what reconcile remains for), README one-liner near the dunning section.

# #368: renewal grace windows — silence at period end extends access briefly instead of cutting it; #367 resolves the uncertainty

## Metadata

**Completed:** yes (2026-06-12; shipped in v0.20.0 at the revised 48h cap)
**Status:** IMPLEMENTED 2026-06-12 (cap revised 72h->48h same day per Paul) — GraceSlack (half-cycle capped 48h, daily
12h) next to DunningWindow; lifecycle pre-appends trailing grace on
create/renew/reactivate (NMI-backed + Stripe; no-gap by tail-append; no
resurrection grace for stale periods); deliberate period-end cancel deletes
scheduled grace; D-3 + DB exclusion half-open semantics verified; docs
generalized; integration tests green; awaiting review. (Original plan
2026-06-12, Paul's product decision: "I'd rather be generous and give our
users more access than they're entitled to, than end it early and have them
complaining. This is the principle behind manual-dunning as well." Also:
provider schedules are not minute-aligned with ours — NMI may think July 1
where we think June 30.)

## Problem

Subscription entitlement windows end at exactly current_period_ends_at.
Renewal appends the next window; SILENCE (no webhook either way) means access
cuts off at the period-end second even when (a) NMI actually charged and we
missed the webhook, (b) NMI simply bills on its own day boundary, or (c) the
webhook is merely late. Fail-closed protects revenue but punishes paying
users; Paul wants fail-open-for-a-bounded-window.

## Design — do NOT change window semantics; generalize the existing grace primitive

The entitlement timeline already has the right tool: `grace` source windows
(used today for CCBill retry windows — docs/entitlements_timeline.md). Paid
windows stay truthful (end_at = period end; audit S-E-4 keeps working;
end_at stays immutable); generosity is a SEPARATE, bounded, revocable window:

- On subscription creation AND every renewal (NMI-backed + stripe; CCBill
  keeps its own retry-driven grace; solana excluded — pull-based, no
  webhooks to miss), append alongside the paid window a trailing grace
  window: [period_end, period_end + graceSlack], source_type='grace',
  source_id = subscription id. Pre-appended (not granted lazily) so there is
  NEVER an access gap regardless of worker cadence.
- graceSlack: bounded "little extra time" — min(48h, half the billing
  cycle) (daily cycles get 12h, monthly+ get 48h; REVISED 2026-06-12 from
  72h per Paul: this grace is for provider rounding/late webhooks, dunning
  has its own grace). Named constant + derived helper next to DunningWindow;
  NOT a config knob.
- Grace ends the moment truth arrives (same revoke-on-resolution pattern the
  CCBill handler already implements):
  - renewal success (webhook, dunning success, #367 charged-repair) ->
    revoke active grace + delete future grace for the sub; the new paid
    window (+ its own new trailing grace) takes over.
  - confirmed dead (terminal decline, remote absent/cancelled, window-expiry
    cancel+downgrade) -> revoke grace immediately with the rest.
  - USER-initiATED cancel (deliberate, knows the contract) -> delete the
    scheduled/future grace at cancel time; access ends at period end as the
    user expects. No generosity for explicit cancellation.
  - still-silence past the slack -> grace lapses by its own end_at;
    fail-closed eventually. #367 keeps probing and can still repair later
    (charged-repair re-grants the paid window).
- DISABLE_ENTITLEMENT_EXPIRATION interplay: none needed — grace windows
  expire by timestamp, the flag governs revocation sweeps, and revocation
  paths above already run only on affirmative evidence.

This also resolves the missed-success-webhook asymmetry: the user keeps
access through grace while #367's probe finds the NMI charge and repairs the
lifecycle — the paid window lands before grace runs out in any same-week
resolution.

## Tasks

- [x] graceSlack helper (cadence-derived, capped) + unit tests.
- [x] lifecycle: append trailing grace window on activate/renew for
      NMI-backed + stripe subs; revoke/delete grace on renewal success,
      terminal failure, and user cancel (mirror the CCBill revoke pattern);
      idempotent (deterministic source ids per (sub, period)).
- [x] migration interplay: imported ACTIVE subs get the same trailing grace
      from their imported period end ONLY when period_end + slack > now
      (months-stale imports must not get resurrection grace) — note for the
      doujins migration audit follow-up.
- [x] audit: confirm S-E/D-3 checks tolerate one scheduled grace window per
      sub (overlap exclusion constraint: grace starts exactly at paid end —
      verify half-open interval semantics).
- [x] tests: no-gap property (paid end == grace start); revocation on each
      resolution class; user-cancel deletes future grace; daily-cycle slack
      cap; integration with #367 charged-repair (grace revoked, paid window
      granted, no double access).
- [x] docs: entitlements_timeline.md grace section generalized; README
      lifecycle paragraph (silence now = bounded grace, then fail-closed).
