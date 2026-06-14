<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 491

---

# #487: Generic spend-graduated tier policy + $-denominated admit limits (in-flight held-$ cap + per-charge cap)

Consumer: tensorhub #486 (platform rate-limiting/tier redesign). MUST stay GENERIC — OpenRails sees only $, tiers,
holds, and host-reported events; no host-domain concepts (GPU/tokens/images) leak in. The $-denomination is what
keeps it reusable by other platforms.

Today OpenRails has SetTierSchedule (cumulative-spend thresholds → auto-graduation) + SetTierPolicy carrying
unit-based throughput windows (rpm/tpm/ipm) + queue limits. The tensorhub consumer no longer needs the throughput
dimensions; it needs two $-denominated per-tier limits. KEEP the throughput capability (other hosts may use it);
ADD/expose these generic primitives.

## Scope
- KEEP: SetTierSchedule + auto-graduation by cumulative CAPTURED spend (generic, unchanged); KEEP `ResolvedTier` on
  AdmitResponse; KEEP captured-ledger $ budget windows (#475).
- ADD two GENERIC $-denominated per-tier limits to the tier policy:
  - `max_concurrent_held_micros` — cap on the sum of a payer's ACTIVE (un-settled) hold $. Enforce at admit
    (over-limit signal) AND surface the resolved value on AdmitResponse, so a host that enforces true occupancy
    itself (tensorhub's scheduler — only it sees live GPU occupancy) can use the value + the per-job estimate.
  - `max_single_charge_micros` — per-charge ceiling; reject an Admit whose estimate exceeds it. Generic runaway guard.
- Throughput windows + queue limits: keep supported; tensorhub simply stops pushing them.

## Notes
- "held $" = running + queued (admitted-not-settled). OpenRails' own enforcement is a committed-$ admission gate; a
  host wanting work-conserving QUEUEING reads the cap value + per-job estimate and queues in its scheduler instead
  (tensorhub does this) rather than getting a hard deny.
- Values are tenant-wide defaults (empty subject), auto-applied at the payer's resolved tier.

## Pairs with
tensorhub #486; openrails #488 (failure windows), #489 (arrears), #490 (deposit fraud).

---

# #488: $-valued failure/wasted-spend budget windows (per-payer tier + per-invoker flat) + report-failed-charge API

Consumer: tensorhub #486. GENERIC — every platform has failed/refunded work that costs it money (a hold released
without capture, or a host-reported out-of-band cost). Rate-limit that "wasted spend" so a low-trust account can't
abuse expensive failures for free (the prepaid balance can't bound this — failures are refunded).

Today there are event-COUNT AbuseWindows (LimitEvents) + abuse_rate_limited / moderation_rate_limited /
failure_rate_limited codes + /abuse-usage. Generalize to $-VALUED windows so a pricey failure weighs more than a
cheap one.

## Scope
- A host API to REPORT a failed/wasted charge: `ReportWastedSpend(payer, actor, micros, reason)` (or a "wasted" flag
  on the settle/release path) — the host says "this attempt failed and cost $X." (tensorhub reports content-filter
  rejects + inference failures; cheap malformed rejects ≈ $0, not reported.)
- Track per-PAYER and per-ACTOR (invoker) wasted-spend in $ windows, multi-window (e.g. 15 min + 5 h).
- Budgets: per-PAYER window graduated by the tier policy (#487) — a `bad_spend` $ budget per tier; per-ACTOR window
  a flat config default (invokers aren't trusted — an account mints unlimited).
- Enforce at admit: deny when EITHER the payer or the actor is over its wasted-$ budget (reuse
  abuse_rate_limited / failure_rate_limited). Surface wasted $ (not just counts) on /abuse-usage.

## Pairs with
tensorhub #486 (reports the failures + defines the $ amounts); openrails #487 (per-tier bad_spend budget lives in the
tier policy).

---

# #489: Per-tenant arrears credit-line (admin-set credit limit; negative-balance-up-to-limit)

Consumer: tensorhub #486 (admin-only arrears for trusted tenants). GENERIC — OpenRails already supports an arrears
billing mode; make the credit exposure an explicit, admin-set per-tenant limit.

## Scope
- Per-tenant `credit_limit_micros` (admin/operator-set, NOT self-serve): under billing_mode=arrears, allow the
  balance to go NEGATIVE up to the credit limit; AdmitHold denies (insufficient_credit) when a new hold would
  exceed it.
- Periodic invoicing of accrued arrears (or expose the arrears balance for the host to invoice) — confirm what the
  existing arrears mode already does vs new.
- Default OFF: prepaid, credit_limit = 0 unless an operator sets it.
- Capacity (#487 held-$ cap) for an arrears tenant scales from the credit limit (the trust signal) rather than a
  positive balance.

## Pairs with
tensorhub #486 (per-tenant `arrears_allowed` flag + admin surface); openrails #487.

---

# #490: Deposit/chargeback fraud controls — 3-D Secure + processor fraud screening + unseasoned-funds spend velocity

Consumer: tensorhub #486 (the "fraud" the tier ramp pretended to address actually lives at the PAYMENT layer).
GENERIC — any prepaid platform taking card deposits faces chargeback fraud (deposit on a stolen/disputed card,
consume irreversible value, then charge back). The prepaid balance does NOT bound this (the deposit reverses AFTER
delivery); the existential risk is the chargeback RATIO terminating the merchant account.

## Scope (payment-layer, distinct from the capacity system)
- 3-D Secure (SCA) on deposits — shifts liability for fraudulent disputes to the card issuer; largely neutralizes
  the stolen-card vector. Wire on the deposit/checkout path per processor (NMI / Stripe / CCBill / etc).
- Processor fraud screening at deposit (Radar-equivalent / NMI fraud tools): risky BIN / AVS / CVC / velocity → block.
- Unseasoned-funds spend velocity (optional): limit how fast NEWLY-deposited funds drain for a new/low-trust account,
  slowing extraction before a chargeback lands. Distinct from the capacity held-$ cap.
- Optional KYC / friction above a deposit threshold.

## Notes
- Priority: 3DS + screening are the high-leverage controls; velocity/KYC secondary. Arguably more important than the
  capacity ladder ever was — this is the real fraud surface.

## Pairs with
tensorhub #486 (out-of-scope-there → here); openrails #489 (arrears amplifies deposit-fraud exposure — extra reason
arrears is trust-gated).

---

# #486: Rename the payable identity `merchant_subject` → `customer` (HARD CUT — table + columns + SDK + wire)

Design (Paul, 2026-06-14): `merchant_subject` is abstract; the payable identity is a **customer** (NMI/Stripe's
own word). It stays MERCHANT-SCOPED (implicit, enforced by `merchant_id` + FORCE RLS) — the same human/card at
two merchants is two `customer` rows (NMI vault + Stripe Customer are both per-merchant/account; confirmed for
all rails). Hierarchy reads: **merchant → customer → processor_customer** (the customer's per-processor object;
`processor_customers` KEEPS its name). HARD CUT, no compat (pre-launch).

## Scope (OpenRails — one breaking change; tag bundled with #484 as v0.28.0)
- **Migration 019** (idempotent/transactional, lock+statement timeouts): `openrails.merchant_subjects` →
  `openrails.customers`; the `merchant_subject_id` FK column → `customer_id` on EVERY referencing table
  (`payment_methods`, `subscriptions`, `payments`, `checkout_sessions`, `usdc_funding_sessions`,
  `entitlement_grants`, `processor_customers`, …); rename its constraints/indexes
  (`uq_merchant_subjects_*`→`uq_customers_*`, `*_merchant_subject_fk`→`*_customer_fk`). Keep `UNIQUE(merchant_id,
  issuer, subject)` + FORCE RLS (scoping stays implicit). `processor_customers` table name unchanged.
- **sqlc**: regen; `MerchantSubjectID`→`CustomerID`; `GetMerchantSubject…`→`GetCustomer…`.
- **Go SDK** (public `openrails` pkg): type `PayerTenantID` → `CustomerID` (drops the doubly-stale Payer+Tenant);
  `payerString`/`payer*` helpers → `customer*`; request/response fields `PayerTenantID`/`MerchantSubjectID` →
  `CustomerID`.
- **Wire JSON (HARD CUT)**: `tenant_subject_id` → `customer_id` on every `/v1/service/*` + `/v1/self/*`
  body/query. No alias.
- **identity pkg**: `identity.MerchantSubjectID` → `identity.CustomerID`.
- Tests + `internal/integrationharness` updated (`RegisterRemoteApplication`/etc. that reference the subject).

## Non-goals / notes
- `processor_customers` stays (now reads customer→processor_customer). `merchant`/`owner_tenant_id` unchanged.
- Cross-merchant "global customer" (Shop Pay) is explicitly OUT (NMI vault + Stripe Customer are per-merchant);
  the only cross-merchant link is the `(issuer, subject)` tuple in the host's auth, not an OpenRails entity.

## Consumers (separate per-app issues — bump to v0.28.0, rename refs)
doujins #409, hentai0 #171, tensorhub #486, cozy-art #148.

**Tasks:**
- [x] Migration 019: `merchant_subjects`→`customers`, `merchant_subject_id`→`customer_id` (all FK tables) +
      constraint/index renames; verify it converges to a clean fresh-install baseline + is idempotent.
      (001 baseline + 015–017 also re-declared `customer`, per the repo's squash-baseline convention; 018
      left historical. processor_customers' OWN `customer_id` text col disambiguated to `processor_customer_id`
      to make room for the FK — added as step 2a in 019.)
- [x] sqlc regen; Go fields/methods `MerchantSubjectID`/`GetMerchantSubject…` → `CustomerID`/`GetCustomer…`.
- [x] SDK type `PayerTenantID`→`CustomerID` + `customer*` helpers; request/response fields renamed.
- [x] Wire JSON `tenant_subject_id`→`customer_id` (hard cut, all routes); `/v1/service/tenant-subjects/*` path → `/customers/*`.
- [x] `identity.MerchantSubjectID`→`identity.CustomerID`; internal refs + comments.
- [x] Tests + harness updated; `go build`/`vet`/`test ./...` + `-tags=integration ./embed/...` green.

---

# #485: Integration tests must run the REAL stack — no stubbed AuthKit; a no-auth host harness for embedded + the real standalone server for remote

Design (Paul, 2026-06-14): the embed conformance test today builds ONE in-process engine and serves its
`/v1/service/*` routes on httptest with a `stubResolver` standing in for AuthKit (`conformance_integration_test.go:52-67`).
That's wrong for an *integration* test: the standalone server's production auth (control plane → AuthKit `core`
service-token resolution, #481 role-based merchant authz, #74 remote_application, #76 JWKS principal) is never
exercised. Move both sides to REAL setups.

## A. Embedded integration — a real no-auth HOST harness (≈ doujins, minus auth)
A minimal embedding-host test fixture that uses OpenRails exactly as `doujins/internal/billing/openrailsembed`
does: build the engine via `embed.New` (host-owns-auth), supply a TRUSTING authenticator (accepts every HTTP
request, injects the bound merchant + the request's subject, verifies NOTHING — fine for tests), and mount the
EMBEDDED HTTP surface (`rt.Handler(...)`). Drive real HTTP against it. This is "doujins with a no-op authenticator,"
so it tests the real embedded host integration, not in-test `embed.New` + direct facade calls.

## B. Standalone integration — boot the REAL server + REAL AuthKit (as in production)
Start the actual standalone server (`cmd/billing`, the real router + control plane) against a test Postgres with
BOTH OpenRails migrations AND AuthKit `profiles.*` migrations (authkit/migrations/postgres 001–006) applied.
Provision via the REAL control-plane bootstrap (`cmd/billing/bootstrap_apply.go`) + mint a REAL service token /
operator JWT (`mint_operator_jwt.go`) through real AuthKit `core` — NO stub. The remote client authenticates with
that real token, resolved by the real `ServiceTokenRequired` → control-plane `ResolveServiceToken` → AuthKit. This
validates the production auth path (service tokens, #481 owner_tenant role authz; later #76 JWKS principal via #484).

## C. Parity preserved across the REAL surfaces
Run the existing conformance operation script against BOTH real surfaces (embedded-host HTTP from A vs standalone
HTTP from B) and assert observable parity — same value as today, but real-vs-real with NO stub. DELETE `stubResolver`.

## Basis that already exists
- `tests/unified_billing_e2e_test.go` (real standalone e2e harness), `cmd/billing` (+ `bootstrap_apply.go`,
  `mint_operator_jwt.go`), and the control-plane integration tests (`internal/controlplane/bootstrap_integration_test.go`,
  `service_token_test.go`) that already wire real AuthKit `core`. `doujins/internal/billing/openrailsembed` is the
  shape reference for the A harness. ("No wire mocks of sibling services" — use real AuthKit `core` in-process, the
  way standalone OpenRails consumes it; NOT the dev-server HTTP API, which OpenRails never calls.)

## Note
Heavier/slower than the one-engine stub, but it's a true integration test of the production standalone server (the
explicit requirement). Keep any fast adapter-level unit coverage where it exists; this is the integration layer.

**Tasks:**
- [x] A: no-auth embedding-host harness (mirrors openrailsembed; trusting authenticator; mounts the embedded HTTP surface).
      `internal/integrationharness.Harness.StartEmbeddedHost` — `embed.New` (host-owns-auth) over the shared migrated
      Postgres + Redis, serving the real embedded `/v1/service/*` on httptest behind the REAL `ServiceTokenRequired`
      middleware wired to a `trustingResolver` (accepts every token, pins the bound merchant, verifies nothing).
- [x] B: standalone harness — boot the real standalone server (`ginboot.NewServer` → `internal/http`, the cmd/billing
      run-server graph) + real AuthKit `core`; OpenRails + AuthKit `profiles.*` migrations applied by
      `migrate.RunPostgres` (migratekit over `authkit/migrations/postgres`, via `dbtest`); provision via the REAL
      control-plane bootstrap (`RunBootstrap` links `owner_tenant_id` + mints a real admin service token through
      AuthKit core); the client auths with that real token resolved by `ServiceTokenRequired` → `ResolveServiceToken`
      → AuthKit core (#481). No stub. `Harness.StartStandalone`.
- [x] C: re-pointed the conformance parity script to A (embedded-real) vs B (standalone-real); `stubResolver` DELETED.
      `embed/conformance_integration_test.go` now drives `openrails.NewRemote` against BOTH harness surfaces and asserts
      `require.Equal`. The un-stubbed standalone path surfaced NO real parity bug.
- [x] CI: harness uses Docker/testcontainers (shared Postgres via `dbtest`, Redis testcontainer); both migration sets
      are applied by the shared migrator. Package doc on `internal/integrationharness` documents the two-server API.

---

# #484: Accept JWKS-principal programmatic auth in the standalone control plane (stored role/perms) — blocked on authkit #76

Design (Paul, 2026-06-14): authkit #76 adds a second programmatic-access credential type — a **JWKS principal**
(a remote_application presenting a SELF-SIGNED token whose subject is itself) granted STORED permissions/role,
parallel to shared-secret service tokens. Standalone OpenRails authenticates programmatic callers today via
service tokens (resolved through authkit) + #481 role-based merchant authz on `owner_tenant_id`.

## What changes (after authkit #76 lands)
- The control-plane auth path ALSO accepts a JWKS-principal self-token (verify via authkit → the principal's
  STORED perms/role) as a programmatic credential, alongside service tokens. A JWKS principal holding a role on
  a merchant's `owner_tenant_id` can administer that merchant — the #481 model already keys authz on roles, so
  this is mostly wiring the new caller type into the existing role-based authorization.

## Blocked on
- authkit #76 (the verifier auth-method + assignable-authority surface).

**Tasks:**
- [x] After authkit #76: accept JWKS-principal self-tokens on the standalone control-plane auth path, alongside
      service tokens; resolve the caller's stored perms/role and run the existing #481 role-based merchant authz.
      (authkit bumped v0.27.0->v0.28.0. New `ControlPlane.ResolveRemoteApplication` (internal/controlplane/
      remote_application.go) verifies a `remote-application-access+jwt` self-token via the existing delegated
      Verifier, detects `Claims.IsRemoteApplication()`, and resolves the merchant by the principal's STORED
      tenant role -> owner_tenant_id, returning the SAME `*ResolvedServiceToken` shape so #481 authz runs
      unchanged. Wired as a second accepted credential in `ginmw.resolveServiceCredential` (new
      `RemoteApplicationResolver` iface): service-token path unchanged; JWT-shaped bearers try the remote-app
      path, falling through to service-JWT when not a remote_application self-token.)
- [x] Tests: a JWKS-principal caller with a role on a merchant's owner_tenant can administer that merchant;
      self-claimed perms not honored. (embed/remote_application_integration_test.go via StartStandalone +
      harness.RegisterRemoteApplication: authorized principal (operator role on owner_tenant) deposits+reads;
      principal without a role is denied 403; service-token auth still works. Authority is STORED only — the
      verifier ignores token self-claims.)

---

# #483: Deposit currency-validation parity — remote accepts an unknown currency that local rejects

Found 2026-06-14 while greening the embed conformance integration test (was un-runnable since the #336 regression,
so this never surfaced). A `DepositCredits` with an unknown currency (`"conformance_missing_currency"`) is
REJECTED by the embedded/local client path but ACCEPTED by the remote HTTP path — both ultimately call the same
`pkg/service Service.DepositCredits`, so the divergence is in how the HTTP deposit handler binds/defaults/forwards
the `currency` field vs the local adapter (a #407/#476 money-decouple artifact; "currency optional" per #476).
UNRELATED to the tenant→merchant rename (#480) — deposit/window/transaction/balance/usage parity is otherwise
green local-vs-remote.

Currently carved out in `embed/conformance_integration_test.go` (the unknown-currency probe is exercised on both
paths but not asserted) so the suite is green; remove the carve-out when fixed.

**Tasks:**
- [x] Trace why the remote deposit handler does not reject an unknown currency (binding/default/normalizeCurrency)
      while the local adapter does; decide the canonical behavior (reject unknown per "money validates the unit").
      ROOT CAUSE: `serviceDepositRequest` had no `Currency` field, so the wire `currency` was dropped → service saw
      "" → defaulted to USD → accepted. Local adapter forwarded `req.Currency` → service rejected. Canonical: reject
      unknown (service `depositTx`→`validateUnit`→`ValidateCurrency` already does); empty still defaults.
- [x] Make embedded + remote deposit currency validation identical; re-assert the conformance probe (remove the
      #483 carve-out). Added `Currency` to the handler request + forward it; both transports now map unknown-currency
      to a 400 (ErrInvalid). Carve-out removed; ErrDepositBadType re-asserted.

---

# #482: Rename `admin_grants` → `entitlement_grants` (disambiguate from authorization "grants")

Design (Paul, 2026-06-13): `openrails.admin_grants` records admin-initiated ENTITLEMENT freebies — comps,
contest winners, partnerships, manual payments (grants a `price_id`/product → a `tenant_subject`; see the table
comment line ~700). The bare word "grants" reads as an AUTHORIZATION grant and already misled an agent into
treating it as an operator→merchant admin mechanism (it is not). Rename to make the billing intent obvious; it
sits naturally beside `entitlements`/`entitlement_features`.

Name: **`entitlement_grants`** (covers all four reasons; unambiguously billing). Alt: `comp_grants` (too narrow
— it's not only comps). Avoid keeping bare "grants".

Mechanical, no logic change. Fold into the #480 tenant→merchant rename pass (`tenant_id`→`merchant_id`,
`tenant_subject_id`→`merchant_subject_id` happen there anyway).

**Tasks:**
- [x] Migration: `ALTER TABLE openrails.admin_grants RENAME TO entitlement_grants` (+ rename its
      constraints/indexes/grants); update the table/column comments. (migration 018, dynamic + guarded;
      001 baseline updated; constraint/index names re-prefixed admin_grants->entitlement_grants.)
- [x] sqlc-gen type + queries + all Go refs renamed (`AdminGrant*` → `EntitlementGrant*`).
      (queries/entitlement_grants.sql; gen regenerated, no drift.)
- [x] Confirm no external consumer references the old table name (it's internal billing state).

---

# #480: Rename tenant→MERCHANT (a dumb billing bucket); embedded = ZERO auth (host owns all of it); register a merchant from config (supersedes #478, realizes #473)

Design (Paul, 2026-06-13): the OpenRails "tenant" is a BILLING LABEL, not an identity — it answers "whose books
does this row go on?", never "who are you / what may you do." Auth is 100% AuthKit's job. So:
- **Rename `tenant` → `merchant`** across OpenRails (specific + billing-flavored; matches NMI's own word; each
  merchant maps 1:1 to a Stripe connected `account`/NMI merchant account — see the existing `stripe_account_id`).
  A merchant carries ONLY billing/money-rail state and NO auth columns. The merchant OWNS all processor config:
  `stripe_account_id` + Stripe webhook secrets, NMI security keys + webhook callbacks (`webhook_host`/`webhook_path`),
  CCBill accounts, Solana wallets/config — i.e. `merchant_secrets`/`merchant_deks` (was `tenant_secrets`/`tenant_deks`),
  `payment_methods`, `processor_customers`, `linked_wallets`, `solana_subscriptions` all key on `merchant_id`.
  The ONLY things that LEAVE the merchant are the AUTH bits currently conflated onto `tenants`: `authkit_tenant_id`
  (→ #481) and the JWKS/issuer registry `tenant_delegated_issuers` (→ AuthKit, #74). Rails stay; identity goes.
- **Embedded mode runs NO AuthKit and stores ZERO auth state.** The host authenticates everyone (its own AuthKit
  / own logic) and hands OpenRails a trusted `(merchant, subject)` via the host-plugged authenticator
  (`DelegatedAuthenticator`/host authenticator). OpenRails verifies nothing → the JWKS/issuer registry
  (`tenant_delegated_issuers`) is an AUTH concern and is DROPPED from OpenRails (lives host-side in embedded;
  AuthKit-side in standalone — #74/#481).
- The merchant row must NOT be "seeded" as a random uuid; it's REGISTERED from config at boot, idempotently.

## What changes
1. **Rename pass** (mechanical, no logic change): `openrails.tenants`→`merchants`, `tenant_subjects`→
   `merchant_subjects`, `tenant.ID`/`tenant.Require`→`merchant.*`, GUC `app.tenant_id`→`app.merchant_id`, sqlc
   types + ~all refs. (Do this first; #481 builds on it.)
2. **`merchant_subjects` = thin payable-identity reference** `(merchant_id, issuer, subject)` — a LABEL to hang
   charges on, NOT verified by OpenRails. The host/AuthKit already proved the subject.
3. **`db.RegisterMerchant(ctx, qx, {slug, name, processor refs})`** — billing-only, config-driven, idempotent
   (ON CONFLICT). Replaces ALL THREE seeds: `db.EnsureTenantBySlug` (#478), doujins `Seed()`, control-plane
   org-minting. `embed.New` calls it after migrations; resolves the bound `merchant.ID` from the row.
4. **Drop `openrails.tenant_delegated_issuers`** (JWKS = auth, not billing). Embedded: host authenticator does
   verification. Standalone: AuthKit `remote_application` holds JWKS (#74/#481).
5. **Canonical merchant uuid = `openrails.merchants.id`** (#473), self-owned uuidv7 — never an AuthKit uuid.
6. **#478 superseded.**

## Non-goals
- Standalone AuthKit wiring + `authkit_tenant_id` teardown → #481. AuthKit org/remote_application split → #74.

**Tasks:**
- [x] Rename pass tenant→merchant (tables, GUC `app.tenant_id`→`app.merchant_id`, `merchant` pkg, sqlc, refs).
      Migration 018 (idempotent, dynamic): `tenants`→`merchants`, `tenant_subjects`→`merchant_subjects`,
      tenant_{deks,secrets,exports,credential_audit}→merchant_*, all tenant_id/tenant_subject_id columns,
      RLS tenant_isolation→merchant_isolation, constraint/index names; 001 baseline kept in sync; verified an
      existing DB migrates to the exact fresh-baseline shape (cols/indexes/constraints identical).
- [x] Drop `tenant_delegated_issuers` table + its code; embedded verifies nothing (host-plugged authenticator).
      Standalone JWKS = AuthKit remote_application (loadRemoteApplications + verifier.WithService); the OpenRails
      issuer registry (issuer_registry/issuer_admin + routes + manifest reconciliation) is removed.
- [x] `db.RegisterMerchant({slug, name, processor refs})` idempotent (ON CONFLICT); called in `embed.New` after
      migrations; resolves bound `merchant.ID` from the row. `EnsureTenantBySlug` + seed two-step deleted.
- [x] `embed.Options` carries the billing-only bound slug; RegisterMerchant is billing-only (no issuer/JWKS).
- [x] Kept `ResolveMerchantSlug`'s unprovisioned error for standalone; integration test (embed) registers a
      FRESH slug + is idempotent on a second `New` (passes against a real migrated DB).
- [x] Fixed the embed conformance test (merchant resource/owner-tenant fields; RegisterServiceRoutes arity).
- [ ] Docs: embedding guide note (deferred — no code-blocking doc; behavior covered by tests/comments).

---

# #481: Standalone — AuthKit owns identity/authz; the TENANT (org) is the ownership hub; merchant gets an explicit owner-tenant link + role-based admin; kill the `authkit_tenant_id` identity-equation + org-per-merchant minting

Design (Paul, 2026-06-13). Standalone OpenRails embeds AuthKit for ALL identity/authorization, and the **tenant
(org) is the ownership hub**. The provisioning model:
1. user registers (AuthKit native `user`).
2. user creates a **tenant (org)** — user is auto-`owner` (the one predefined minimum role; standalone may
   define additional roles + permissions).
3. user creates a **remote_application** controlling that tenant (external JWKS peer; AuthKit #74).
4. user creates a **merchant** linked to that tenant (a billing bucket; #480).

A user's role on the tenant is what authorizes them to administer that tenant's remote_applications and
merchants. ONE tenant → MANY merchants/remote_applications. There is NO bespoke OpenRails grant table
(`admin_grants` is unrelated — it records comp/free product grants, not authz).

## What changes (the actual fix)
1. **Kill the identity-equation.** Today `authkit_tenant_id` fuses billing with auth: service-token auth
   resolves the merchant 1:1 as "the org that owns your token" (`controlplane/service_token.go:243
   tenantForAuthKitTenant`, `WHERE authkit_tenant_id = $1`), and provisioning AUTO-MINTS an org per merchant
   (`tenancy/lifecycle.go:151 EnsureAuthKitTenant` → `UPDATE ... SET authkit_tenant_id`). Both go.
2. **Explicit owner-tenant link instead.** A merchant carries a nullable **`owner_tenant_id`** (the org that
   owns it) — user-created in step 4, NULL in embedded (no AuthKit). This is the ONE permitted billing→auth
   reference, and it is OWNERSHIP (who administers), not IDENTITY (the merchant is never "equal to" the org;
   one org owns many merchants). It is never used to resolve a merchant from a token.
3. **Authorization is role-based on the owning tenant.** Admin (human managing merchant/catalog/config): a role
   on the merchant's `owner_tenant_id` (owner minimum; custom roles/permissions allowed). Runtime (a
   remote_application's delegated token billing a merchant): the remote_application controls the tenant that
   owns the merchant. Verification (JWKS) is AuthKit's `remote_application` (#74); OpenRails holds no issuer
   registry (dropped in #480).
4. **Provisioning = explicit user flow**, not auto-minting. Remove `EnsureAuthKitTenant`/
   `recordAuthKitTenantBySlug` org-per-merchant creation; merchant via #480 `RegisterMerchant`,
   remote_application via AuthKit #74, both linked to the user's tenant.

## Depends on / relates
- #480 (merchant rename + JWKS removed). AuthKit #74 (tenant/org + roles, remote_application owns issuer/JWKS).
- App-specific escape hatch is AuthKit #75 (token `attributes` + consumer-stored mapping; OpenRails adds nothing).

## Non-goals
- Embedded path (no AuthKit, `owner_tenant_id` NULL) → #480.

**Tasks:**
- [x] Added nullable `merchants.owner_tenant_id` (ownership FK to the AuthKit org); migration 018 drops
      `authkit_tenant_id`/`authkit_tenant_slug` + their index (001 baseline in sync).
- [x] Replaced `tenantForAuthKitTenant` (`WHERE authkit_tenant_id = $1`) with role-based authz:
      `merchantForOwnerTenant` (`WHERE owner_tenant_id = $1`) + `AuthorizeMerchant` (request NAMES the merchant;
      check caller's owning tenant owns it). Caller identity + JWKS come from AuthKit (remote_application). The
      delegated/service-JWT paths resolve merchant via `merchantForIssuer` (iss→remote_application→its tenant
      memberships→owner_tenant_id→merchant).
- [x] Removed org-per-merchant minting: `EnsureAuthKitTenant` + `TenantProvisioner` deleted from tenancy;
      `recordAuthKitTenantBySlug`→`recordOwnerTenantBySlug` (records owner_tenant_id only). Provisioning takes an
      EXPLICIT `OwnerTenantID` (never auto-minted; embedded leaves it NULL).
- [x] Audited/cleaned the Go refs (`controlplane/service_token.go`, `service_jwt.go`, `delegated.go`,
      `issuer_registry.go`, `bootstrap.go`, `tenancy/lifecycle.go`, `ginmw/service_token.go`,
      `http/routes_tenant_admin.go`, `bootstrap/tenant_manifest.go`).
- [x] Tests: standalone authz via owner-tenant role (`TestMerchantForOwnerTenant` incl. a tenant owning TWO
      merchants + AuthorizeMerchant), `TestProvision_Idempotent` (merchant with no auto-minted org + unowned
      embedded path), `TestBootstrap_Idempotent` (owner_tenant_id recorded). All pass vs a real migrated DB.

---

# #478: embed.New auto-ensures the bound tenant; clear unprovisioned-tenant error (not panic)

The no-default-tenant change (#336) left embedded boot broken: a host binds the engine to a tenant
SLUG via `embed.New(... Options{Tenant: "x"} ...)`, but nothing created that tenant row, so tenant
resolution PANICKED at handler build (`embedhttp: resolve configured tenant ... no rows in result
set`; same in `pkg/embedded/gin/self.go`).

Fix (MODE-AWARE):
- EMBEDDED (`embed.New`): after `migrate.RunPostgres`, idempotently ensure the bound tenant exists
  (`db.EnsureTenantBySlug` → `INSERT ... ON CONFLICT (slug) DO NOTHING`, schema-rewrite-aware via the
  engine's `DB.Qx`). The host owns the embed + DB, so auto-create is safe + idempotent.
- STANDALONE/REMOTE: do NOT auto-create. `db.ResolveTenantSlug` now maps a no-rows result to a CLEAR
  error — "configured tenant slug %q is not provisioned — create the tenant and register its JWKS via
  the control plane". The two former `panic(...)` sites (embedhttp.NewHTTPHandler, gin self handler)
  return a fail-closed 500 handler carrying that message instead of panicking. (standalone server.go
  already fails boot on the error.)

**Tasks:**
- [x] `db.EnsureTenantBySlug` (sqlc `EnsureTenantBySlug` :exec) + call in `embed.New` after migrations.
- [x] `db.ResolveTenantSlug` returns the actionable unprovisioned-tenant error on `pgx.ErrNoRows`.
- [x] Replace the embedhttp + gin self-handler panics with fail-closed error handlers.
- [x] Integration test: embed.New on a FRESH slug boots (no panic), row exists, handler builds, second
      New is an idempotent no-op (embed/tenant_ensure_integration_test.go).

---

# #476: Persisted tier SCHEDULE + AUTO-graduation by cumulative spend — host declares the ladder once; OpenRails owns + maintains each tenant-subject's tier (no host cranking)

Today graduation is host-cranked: `GraduateTier(payer, creditType, ladder)` (#298) requires the
HOST to pass the ladder AND call it on every spend event (or poll). That's wrong ergonomically: the
host shouldn't have to iterate payers or re-graduate. The ladder is a SCHEDULE that changes rarely;
graduation should be OpenRails' job, driven off the spend it already records.

## What changes
1. **Store the schedule once.** New unified-client API `SetTierSchedule(tenantSubjectID-or-tenant, schedule)`
   where `schedule = ordered [{tier, min_cumulative_paid_micros}]`, persisted per tenant (a
   `tier_schedules` table; RLS + owner=platform, like the budget-policy owner split #473). Host calls
   it once on boot / config-change. (HTTP + embedded transports, mirroring `SetTierPolicy`.)
2. **OpenRails auto-maintains each payer's tier.** Derive `tier = highest schedule rung whose
   min_cumulative_paid_micros <= payer.cumulative_paid_micros`, monotonic (never silently regresses).
   Recompute + persist to `credit_account_settings.tier` server-side on the deposit/settlement path
   (where `cumulative_paid` already changes) — EVENTFUL, O(1) reads. (Lazy derive-at-`GetTier`/admit
   is the equivalent fallback if a schedule exists but the materialized tier is stale.) No background
   crank required by the host; an internal worker may sweep for schedule-change re-grades.
3. **`GetTier` returns the auto-maintained tier**; `Admit` resolves limits by it. The host (tensorhub)
   only READS the tier (for its scheduler cap) — never writes/cranks it.

## Migrate off host-cranked GraduateTier
Keep `GraduateTier` working but make the stored-schedule path authoritative: if a `tier_schedule`
exists for the tenant, OpenRails owns graduation and host `GraduateTier` calls become a no-op/deprecated.
Manual admin tier override (explicit tier on account settings) still wins over the schedule (#298).

## Tasks
- [x] `tier_schedules` table (tenant-scoped, owner=platform) + migration + RLS. (migration 016; tenant-wide
      default = tenant_subject_id NULL, optional per-subject override; rungs jsonb [{tier,min_cumulative_paid_micros}].
      Also added money_accounts.tier_source ('auto'|'admin') to distinguish auto-graduation from an admin override.)
- [x] `SetTierSchedule` on the unified client (HTTP route + embedded adapter), mirroring `SetTierPolicy`.
      (client.go interface + TierScheduleRung; remote.go PUT /v1/service/tier-schedules; embed/client.go local
      adapter; pkg/service.SetTierSchedule; handler ServiceSetTierSchedule + ginroutes route; money.SetTierSchedule/
      GetTierSchedule store. Empty tenant_subject_id = tenant-wide default.)
- [x] Auto-graduation: recompute+persist `money_accounts.tier` from `cumulative_paid` vs the stored schedule
      on the DEPOSIT path (eventful — money_service.depositTx after the deposit posts, in-band in the same tx);
      monotonic (rungThreshold guard never regresses); admin-override wins (AutoGraduateMoneyAccountTier guards
      tier_source<>'admin' at the DB level + an in-code check). DefaultCurrency only.
- [x] `GetTier`/`Admit` read the auto-maintained tier (unchanged — admitter falls back to money.GetTier which
      reads money_accounts.tier; auto-graduation now keeps it current). Host-cranked `GraduateTier` short-circuits
      to a no-op returning the auto-maintained tier when a schedule exists (kept compiling for schedule-less hosts);
      added SetTierOverride for the explicit admin override.
- [x] VERIFY: integration tests (internal/modules/money/tier_schedule_integration_test.go) — set a schedule once →
      deposits crossing thresholds auto-raise the tier with NO host call; GraduateTier no-ops; admin override sticks
      across further deposits; multi-tenant isolation (tenant A's schedule does not graduate tenant B's payer).
      Existing TestGraduateTier (schedule-less legacy path) still passes.

## Related
#298 (host-cranked `GraduateTier` this supersedes), #473 (budget-policy owner=platform store pattern to
mirror), #472 (per-tier throughput/queue limits resolved by the tier). CONSUMER: tensorhub #477
(declares the schedule + per-tier capacity limits once, reads the auto-maintained tier for its
scheduler cap).

---

# #477: Finish money/credit decouple at the SDK boundary — CreditType → Currency

**Status:** in progress.

The public client request/response types still carry `CreditType` — vestigial/USD-only after #474's
internal decouple (the internal money service already speaks `currency`). Rename the consumer-facing
field `CreditType` → `Currency` (Go field) with wire tag `credit_type` → `currency`, make it OPTIONAL
(empty → "USD" via the money currency normalizer), and drop the "CreditType required" validations so
the consumer surface matches the decoupled money ledger. `Currency` is a free string so #475's
qualified `tenant/name` custom-credit unit codes ride the same field (ledger ResolveUnit/validateUnit
handles validation — do not hard-restrict here). BREAKING SDK rename: host apps need a follow-up
`CreditType:`→`Currency:` edit. References #474 (done, internal decouple) + #475 (custom credits).

---

# #475: Tenant-defined custom credits (api-credits, gold-coins) — consume-only, no FX

**Status:** DONE (wip/475-custom-credits). Was referenced as "#473" before the id collision; it's #475.

**Done (minimal):**
- [x] Registry: `openrails.custom_credit_types(id, tenant_id, name, decimals, active, created_at, updated_at)`
  in migrations/postgres/001_schema.up.sql — RLS tenant_isolation, tenant_id NOT NULL (#336), UNIQUE
  (tenant_id, name), decimals CHECK [0,18]. sqlc queries (define/list/get/set-active) in
  internal/db/queries/custom_credit_types.sql; gen regenerated.
- [x] Qualified unit codes + resolution: internal/modules/money/custom_credit.go — `IsQualifiedUnit`,
  `ResolveUnit(ctx, code) (decimals, builtin, err)` (unqualified → built-in registry; `slug/name` →
  ctx-tenant-owned + active custom type), `normalizeUnit` (preserves qualified codes, no uppercasing),
  `FormatAmount(minor, decimals)`, registry CRUD methods, and `RequireBillingCurrency` (NEW here — see
  the F-merge note below).
- [x] Ledger consumption: deposit/withdraw/hold/balance + the grant path (validateCreditGrantSpec → now a
  method using validateUnit) accept resolved qualified custom units (money_service.go, subscription_credits.go).
- [x] Invariant: billing paths reject qualified units via RequireBillingCurrency — AccrueOwed (owed/arrears)
  + getAccountSettings (account-settings/auto-topup). FinalizeInvoice has no currency param (USD-only) so
  it's already currency-safe.
- [x] Presentation: FormatAmount pure helper (100 @ 2dp → "1.00").
- [x] Tests: custom_credit_test.go (unit) green in default suite; custom_credit_integration_test.go
  (define gold dp=2, deposit 500 + spend 150 → 350, owed rejected, format) PASSES under -tags=integration
  against a real migrated DB.

**Merge note (#474 overlap, resolved):** `RequireBillingCurrency` now lives once in currency.go (master's #474
home); it rejects qualified custom units (wrapping ErrBillingUnitRequired), else ValidateCurrency.

Custom credits are tenant-defined consumable units (api-credits, in-game gold) with NO fixed dollar
exchange ratio (buy 100 for $10 OR 1000 for $80). They are NEVER billed in (see the #474 invariant) —
acquired by purchase (paid in a real currency, any ratio) or product grant, then SPENT on usage.
Tensorhub's "API credits" are NOT custom credits — they are USD money (#474). Reuse the money LEDGER
primitives as-is; do NOT give custom credits a `money_accounts`/invoice/owed row.

Design notes:
- Unit namespace: qualified `tenant-slug/name` (e.g. `tensorhub/api-credit`) = custom credit;
  unqualified (`usd`) = built-in currency (#474). One `currency`/`unit` column hosts both.
- DEFINITION model (like Sui custom coins): a custom credit type is just `{name, decimals}` — the
  tenant picks a name + a decimal precision. `decimals` separates PRESENTATION (what the user/admin
  sees) from internal STORAGE: amounts are ALWAYS stored as integer minor units, and the UI divides
  by 10^decimals for display. Examples: tickets → decimals=0; gold coins → decimals=2. This is exactly
  how the built-in currencies already work — micro-USD is "USD with decimals=6" — so the SAME storage
  model + currency registry shape covers both; built-ins are {USD/USDC/EUR=6, JPY=4, SOL=9}, custom
  types carry their tenant-chosen decimals in the per-tenant registry.
- Tasks: (1) a per-tenant custom-credit-type registry storing `{tenant, name, decimals, active}`
  (define/list/activate); (2) allow the money ledger's `currency` column to hold qualified codes +
  resolve their decimals from the registry (tenant-owned); (3) product grants may target a
  custom-credit unit; (4) the admission/spend path consumes custom credits; (5) presentation: format
  stored integer minor units → display via the unit's decimals (built-in or custom) at the API/admin
  boundary; (6) ENFORCE the invariant — billing/invoice/owed/charge paths reject non-currency
  (qualified) units.
- Keep it minimal; build only when a tenant actually needs a non-dollar unit.

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

