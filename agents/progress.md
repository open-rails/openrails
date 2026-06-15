<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 493

---

# #492: Operator-configurable FLAT per-actor wasted-spend windows (close the #488 per-invoker gap)

Consumer: tensorhub #486. GENERIC — every platform has a flat per-invoker abuse backstop. The #488 per-PAYER
bad_spend budget was operator-settable via SetTierPolicy, but the per-ACTOR flat budget had NO client setter:
admit hardcoded `service.DefaultActorWastedWindows()` ($5/15m, $20/5h), so a host's declared per-invoker budget
(tensorhub `per_invoker_bad_spend` $1/15m, $3/5h) was silently ignored.

## Scope
- Migration 028 `openrails.actor_wasted_windows`: tenant-scoped (customer_id NULLABLE; NULL = tenant-wide
  default, non-NULL = per-customer override), windows jsonb + version + timestamps, merchant_id RLS — mirrors
  tier_schedules' "= $2 OR IS NULL" effective resolution.
- sqlc: UpsertActorWastedWindowsDefault/Customer + GetEffectiveActorWastedWindows (admission_support.sql).
- Service: `SetActorWastedWindows(ctx, payer, []abuse.WastedWindow)` + `actorWastedWindows(ctx, payer)` resolver
  (stored else DefaultActorWastedWindows). Wired into ALL 3 hardcoded sites — Admit (WithWastedSpend guard),
  ReportWastedSpend (guard.Record), AbuseUsage (guard.Usage). DefaultActorWastedWindows kept as the FALLBACK.
- HTTP `ServiceSetActorWastedWindows` mounted PUT /v1/service/actor-wasted-windows (creditsWrite), same router.
- SDK `SetActorWastedWindows(ctx, tenantSubjectID string, []BudgetWindowInput) error` — client.go interface +
  remote.go (PUT) + embed/client.go (localClient -> svc). Reuses BudgetWindowInput.

## Pairs with
tensorhub #486 (PushTierLadder now pushes the YAML per_invoker_bad_spend via the new setter); openrails #488
(per-payer bad_spend), #487 (tier policy template), #476 (tier-schedule end-to-end template).

## Outcome
DONE. Release v0.31.0 (additive/back-compat over v0.30.0), tagged + pushed. Migration 028, sqlc queries,
service `SetActorWastedWindows` + `actorWastedWindows` resolver wired into ALL 3 hardcoded sites (Admit guard,
ReportWastedSpend, AbuseUsage), HTTP `ServiceSetActorWastedWindows` (PUT /v1/service/actor-wasted-windows),
SDK `SetActorWastedWindows(ctx, tenantSubjectID string, []BudgetWindowInput) error` (client/remote/embed).
DefaultActorWastedWindows() kept as the FALLBACK. Tests GREEN: 3 service integration tests
(actor_wasted_windows_integration_test.go — configured $1 window DENIES, unset falls back to $5 default,
per-customer override precedence) + extended conformance (embed vs remote identical; AbuseUsage observes the
CONFIGURED $1 limit + over-budget). go build ./... + unit suite green.

RELEASE MECHANICS (important for the next agent): v0.31.0 was cut from an ISOLATED git worktree based on the
v0.30.0 tag, NOT from master. Reason: at cut time master's working tree held the entire uncommitted #491
refactor (migration 027 + customers issuer/subject natural key + InvokerID columns + the identity rename) as
ACTIVE concurrent WIP by another agent — committing the regenerated gen/models.go on master would have swept
that WIP, and a tag there would either ship 028 without 027 or carry half-finished #491. The clean worktree
gave a coherent v0.31.0 = v0.30.0 + #492 with zero #491 drift. The #492 source changes also still live
uncommitted in the main ~/openrails working tree (alongside the #491 WIP) — once #491 lands on master, those
identical changes will already be present / can be fast-forwarded; the released tag is the source of truth.

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
tensorhub #486; openrails #488 (failure windows), #489 (arrears). (#490 deposit-fraud was deleted — not needed.)

## Outcome
LANDED. Two generic $-denominated per-tier limits added to the tier policy JSONB (no new column —
consistent with the existing budget_windows/queue_limits which also live in `policy`): `max_single_charge_micros`
(per-charge ceiling; deny `single_charge_cap_exceeded` at admit before any hold) and `max_concurrent_held_micros`
(cap on the SUM of the payer's ACTIVE un-settled hold $; deny `concurrent_held_cap_exceeded`, AND surfaced on every
AdmitResponse as `max_concurrent_held_micros` + `held_micros` so an occupancy-aware host queues itself). Reused the
existing `SumActiveMoneyHoldAuthorizations` query via new `MoneyService.ActiveHeldMicros`. KEPT throughput windows /
queue limits / budgets / SetTierSchedule + auto-graduation unchanged. SDK/wire: AdmitResponse + TierPolicyInput
extended across client.go/remote.go/embed/client.go + pkg/service; HTTP handler unchanged (serializes the struct).
Migration 020 is a JSONB-contract COMMENT (sqlc generate/vet clean). Tests: 3 new integration tests; full admission
integration suite green; build OK.

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

## Outcome
LANDED. NOTE: the spec's "existing event-COUNT AbuseWindows / LimitEvents / abuse_rate_limited / /abuse-usage" did
NOT literally exist in this repo — what existed was the Redis fixed-window limiter + count-based card-failure velocity
guards. Built the $-valued wasted-spend windows ON THAT proven infra (no new store): new ratelimit value-window
primitives (AddWindowValue/WindowValue — DECOUPLED record vs read, vs throughput's Check which folds them) + a new
`abuse.WastedSpendGuard` tracking per-PAYER + per-ACTOR wasted $ in multi-window Redis. Host API
`ReportWastedSpend(payer, actor, micros, reason)` accrues into both subjects. Per-PAYER budget = tier policy JSONB
`bad_spend_windows` (graduated by tier); per-ACTOR = flat config default `DefaultActorWastedWindows` ($5/15m + $20/5h).
Admit denies `abuse_rate_limited` (payer over) / `failure_rate_limited` (actor over), BlockedBy="abuse" → 429.
New `/abuse-usage` read (wasted $, not counts). SDK/wire: ReportWastedSpend + AbuseUsage added to the Client interface
(client/remote/embed) + pkg/service + 2 HTTP handlers + routes. Migration 021 = JSONB-contract COMMENT. Tests: 2 new
integration tests (payer-over → abuse_rate_limited; actor-over → failure_rate_limited + per-actor isolation); admission
+ abuse + ratelimit integration suites green; build + sqlc vet OK.
GOTCHA fixed: a helper file named `*_windows.go` is silently treated by Go as a Windows-GOOS build constraint
(excluded from the build); renamed to `valuewindow.go`.

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

## Outcome
LANDED. EXISTING vs NEW: the existing arrears mode ALREADY had a credit line — `max_outstanding_owed_micros` (the
existing tests literally name it "credit limit") — but it is (a) SELF-SERVE (set via UpsertAccountSettings) and (b)
enforced on the SETTLEMENT path (SpendCredits/AccrueOwed); AuthorizeAndHold SKIPPED the balance gate for arrears
entirely. #489's NEW bits: an explicit OPERATOR-ONLY `credit_limit_micros` column (migration 022) + admit-time
enforcement in AuthorizeAndHold (deny `insufficient_credit` when a new hold would push the balance below
-credit_limit). New `MoneyService.SetCreditLimit/GetCreditLimit` (operator-only, dedicated SQL — NOT on the self-serve
AccountSettingsInput) + pkg/service + SDK (client/remote/embed) + admin-gated PUT/GET /v1/service/credit-limit routes.
DESIGN CHOICE / spec deviation: the spec's literal "credit_limit=0 ⇒ prepaid behavior" would REGRESS the existing #302
arrears-spill-past-balance (a hold may exceed the balance and spill to owed at capture). So credit_limit=0 = OFF
(existing arrears behavior UNCHANGED; bounded by the outstanding ceiling), and credit_limit>0 = the new admit-time
ceiling. Prepaid is unaffected either way. Tests: 3 new integration tests (credit-line allow up to limit +
insufficient_credit deny; 0=existing-arrears-behavior; prepaid-unaffected); money + admission + pkg/service integration
suites green (incl. the pre-existing #302 spill test); build + sqlc generate/vet OK.

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

# #491: Split CUSTOMER (payer / money account) from ACTOR (invoker / delegated-user) — delegated-users hold no money

**Completed:** no

Today `customer` (the renamed merchant_subject, #486) is keyed `UNIQUE(merchant_id, issuer, subject)` —
one row PER DELEGATED-USER — AND owns the money: `money_accounts` (balance, tier, spend-caps),
`processor_customers` (Stripe cus_*/payment methods), credit-line (#489), tier_schedules (#476). That
conflates two different things:

- The PAYER: the account that actually holds money / a payment method / a balance — we have a real
  financial relationship with it. FEDERATED: this is the TENANT (e.g. cozy-art). EMBEDDED: it's the
  host's end-user (doujins end-users buy their own credits via the host's processor).
- The ACTOR (invoker): the individual performing an action. FEDERATED: a DELEGATED-USER — ephemeral, NO
  payment method, NO balance, NO money relationship with us; we know it only via (issuer, subject).

The RUNTIME layer already distinguishes these (#487 caps the PAYER's active held-$; #488 has per-payer
TIER budgets + per-ACTOR "invokers aren't trusted" FLAT budgets), but the IDENTITY/data model does NOT:
`internal/modules/admission` types the payer as `identity.CustomerID`, and `actor` exists only as an
ephemeral Redis budget-scope STRING (budgets scope=actor), not a first-class identity. This issue
promotes the split to the identity model.

TERMINOLOGY (decided): **customer = the PAYER** (money account, merchant-scoped, NEVER delegated).
**actor** (a.k.a. invoker) = the doer (can be a delegated-user, a native user, or the tenant itself).

STRUCTURE (corrected, per cozy-art->tensorhub->openrails walkthrough): the MERCHANT is the OPERATOR that
authenticates to OpenRails — its issuer/tenant/service-token in OpenRails's mounted authkit IS the merchant
(e.g. tensorhub). OpenRails hosts MANY merchants. The merchant is the ONLY party OpenRails authenticates; it
ASSERTS, for its OWN namespace only, who the CUSTOMER (payer) and ACTOR (invoker) are — opaque strings
OpenRails records but does NOT independently authenticate. OpenRails is FEDERATION-AGNOSTIC: it does not
care whether the merchant's end-user is native or delegated; the merchant runs its OWN end-user auth (e.g.
tensorhub's internal cozy-art federation, invisible to OpenRails) and just reports (customer, actor).
cozy-art is a CUSTOMER STRING, not an OpenRails-authkit entity. NO tenant<->merchant schema link: the
operator's authkit tenant/issuer CORRESPONDS to its merchant (same real entity), and issuer -> merchant is
the issuer registry. owner_tenant_id stays ownership/admin-only. Within a merchant:

  customer = the PAYER = a BALANCE / money account — a PURE billing entity: (id, merchant_id). ALL the
    money state already FKs to customers(id): money_balances (balance + held), money_blocks (credit lots),
    money_transactions (ledger), money_accounts (policy/config + tier), processor_customers (Stripe cus_*),
    payment_methods. NO issuer, NO subject, NO tenant. It is an ACCOUNT, not a person; a balance has no
    issuer.
  actor (invoker) = a MERCHANT-SUPPLIED, OPAQUE label for the end-user that triggered a charge — NOT
    authenticated by OpenRails (exactly money_spend_limits.actor today: "a caller-supplied opaque principal
    string"). DRAWS ON a customer's balance via a customer_id FK; used ONLY for spend-attribution +
    per-actor abuse caps. NO money, NO tier. End-users NEVER present a token to OpenRails.

  merchant(1) -> customers(many);   customer(1) -> actors(many);   actor -> customer (FK)

This finishes the de-conflation: MONEY on the customer, ATTRIBUTION/abuse on the (unbilled) invoker. The
AUTHENTICATED token subject (delegated_sub, or a merchant self-token) is ALWAYS a CUSTOMER (has a balance)
or the merchant itself — NEVER an unbilled end-user. End-users have no balance and never present a token to
OpenRails; the merchant reports them as the actor label on charges. TERMINOLOGY collision to avoid: authkit
comments delegated_sub as "the actor", but in BILLING terms that authenticated subject is a CUSTOMER —
#491's actor is the unbilled end-user label, so call it the INVOKER in code.

A customer is a payer/balance scoped to a merchant, ASSERTED by the merchant. STANDALONE-individual
(doujins): each end-user is BOTH payer (customer) and invoker (actor) — actor -> customer 1:1; the end-user's
merchant-minted token is sent straight to OpenRails, which verifies it (subject = customer/actor). B2B2C
(cozy-art via tensorhub): the MERCHANT = tensorhub; cozy-art = a CUSTOMER STRING tensorhub reports (NOT an
OpenRails-authkit entity); cozy-art's users = the ACTORS (many:1); tensorhub runs the cozy-art federation
itself, OpenRails never sees it. EMBEDDED: host vouches; customer/actor ids passed directly.

EMBEDDED degenerate case (doujins): the end-user genuinely holds money, so end-user = customer(payer) =
actor (1:1); MANY customers per merchant (one per end-user), one actor each. Unifying rule: actor->customer
is many:1 (federated) or 1:1 (embedded). "One customer per merchant" is FEDERATED-ONLY — embedded keeps
per-end-user customers because the end-user holds money.

================================================================================
OWNER CONFIRMATION + UNIFIED RULE (2026-06-14) — supersedes the "CONFIRM this nuance" note above.
================================================================================
The split is REAL and reduces to ONE rule. Two primitives, never conflated:

  INVOKER (formerly "actor") = the END-USER that made the call. ALWAYS the token SUBJECT (delegated_sub) or
    a native user. Referenced by a STABLE UUID, never a username (e.g. cozy-art reports PaulFidika by his
    cozy-art UUID, so a rename on cozy-art doesn't reparent his history). The invoker is ALWAYS tracked
    (billing visibility for the payer + per-invoker abuse/rate caps the payer sets), and NEVER holds money.

  CUSTOMER (payer) = who the spend is billed to. Resolved from the ISSUER:
    - issuer is ORG-BOUND (cozy-art on tensorhub: cozy-art is an authkit ORG *and* an issuer) ->
        payer = the ORG's single customer. MANY invokers : 1 customer. cozy-art:PaulFidika is NEVER a
        tensorhub user and NEVER a tensorhub-OpenRails customer — only an invoker under cozy-art's balance.
    - issuer is ORG-LESS, or a native user (doujins/hentai0 users; tensorhub native user Sam) ->
        payer = the end-user THEMSELVES. invoker -> customer 1:1 (Sam pays for Sam; each doujins user pays
        for itself). authkit#80 makes org_id NULLABLE precisely so org-binding can BE this switch.

  So: invoker = always the subject; payer = the org (org-bound issuer) ELSE the subject/native-user.
  Embedded (doujins, hentai0, cozy-art-standalone) and standalone differ only in transport, not in this rule.

DEPLOYMENT SHAPES (owner, 2026-06-14):
  - doujins/hentai0/cozy-art use OpenRails EMBEDDED, identically: merchant = the app (sole); customers =
    each of the app's users (native authkit users -> per-user balances); no orgs; issuers unused embedded.
  - doujins/hentai0 STANDALONE OpenRails: authkit there has NO users/orgs, only ISSUERS [doujins, hentai0]
    tied to the one merchant; each token subject -> its own customer (org-less issuer branch above).
  - tensorhub uses OpenRails EMBEDDED: merchant = tensorhub (sole); customers = tensorhub's ORGS + native
    users; cozy-art is an authkit org+issuer; cozy-art's delegated users are INVOKERS, not customers.
    tensorhub resolves payer=org / invoker=subject via authkit IN-PROCESS, then charges its embedded
    OpenRails. (The earlier "OpenRails is federation-agnostic, cozy-art is an opaque string it never sees"
    framing was the STANDALONE-remote model; under tensorhub-EMBEDDED, cozy-art IS a real authkit entity
    and the payer corresponds to that org.)

  ISSUERS only appear in two places (owner, 2026-06-14): (1) STANDALONE OpenRails, where the embedding app
  (doujins/hentai0) signs JWTs for its users and the issuer ties to the merchant (no org); (2) FEDERATED
  delegators inside an embedded host (cozy-art inside tensorhub), where the issuer is org-bound. In PLAIN
  embedded mode there is no JWT-to-OpenRails step at all (see below). "Org" = a grouping of an app's
  users/sub-customers, NEVER the app itself. In tensorhub an org (cozy-art) -> one customer (payer) and may
  own an org-bound issuer.

  EMBEDDED SECURITY MODEL (owner, 2026-06-14) — load-bearing: EMBEDDED OpenRails has NO security model of
  its own; it DEFERS ENTIRELY to the host app. The host is in charge of everything security-related — it
  fully controls OpenRails, calls in freely, and may define ONE OR MORE merchants as it sees fit (so
  "embedding-app == one merchant" is the common case but NOT a rule). For a host-exposed OpenRails route
  (e.g. /me), the flow is: host's authkit MIDDLEWARE validates the JWT -> resolves the user's identity ->
  attaches it to the request CONTEXT -> calls into OpenRails, which simply READS (merchant, customer/payer,
  invoker) from that context. OpenRails embedded never validates a token or authenticates a principal
  itself. (Standalone OpenRails is the ONLY mode where OpenRails authenticates incoming JWTs against the
  issuer registry; that is what issuers/merchant-auth are for.)

DELEGATED-USER (INVOKER) IDENTITY LIVES IN AUTHKIT (decided 2026-06-14; REVERSES the earlier "lives in
OpenRails" note). Restore authkit profiles.delegated_users (un-drops authkit#78; see authkit#81) as the
SHARED federated-end-user identity primitive — it's consumed by TWO domains: the APP (tensorhub ALREADY
references a delegated_user_id in 5 NON-billing tables: user_file_objects/media ownership,
media_output_events, resource_visibility_audit, platform_abuse_events, platform_policy_denials) AND BILLING
(openrails invoker attribution + per-invoker spend/abuse caps). A shared identity belongs in the identity
service so both FK to it and neither depends on the other; were it in OpenRails, tensorhub's media-OWNERSHIP
tables would FK into the BILLING service (app->billing coupling, wrong). My earlier "FK can't cross schemas
so it must live in OpenRails" was WRONG: Postgres allows cross-SCHEMA FKs (only cross-DATABASE is
impossible), and authkit `profiles` + openrails `billing` share ONE database in every deployment (the
embedded host DB; or standalone-openrails's bundled authkit DB).
  - openrails attribution rows (money_spend_limits / usage_events / money_transactions / budget_*) FK their
    invoker -> authkit profiles.delegated_users(id), CROSS-SCHEMA, same DB.
  - id is uuidv7 (pg18 native — the fleet's UNIVERSAL pk; owner: uuidv7 everywhere, NO uuidv5). uuidv7 is
    random/time-ordered and CANNOT be content-derived, so idempotency comes from a UNIQUE NATURAL KEY, not a
    derived id: delegated_users = `id uuid DEFAULT uuidv7()` + `UNIQUE(remote_application_id, subject)`, and
    authkit TouchDelegatedUser(issuer, subject) = `INSERT ... ON CONFLICT (remote_application_id, subject)
    DO UPDATE last_seen_at RETURNING id` — the id is minted ONCE and RETURNED; openrails stamps the returned
    value. This REPLACES the two clashing derivations (tensorhub `du_`+sha256(issuer\x00sub); openrails
    FederatedCustomerID uuidv5) — both go away.
  - SAME pivot for the payer CUSTOMER id: DROP FederatedCustomerID(uuidv5). customers.id = uuidv7() with a
    per-branch natural key — org-bound: UNIQUE(merchant_id, org_id); org-less/native: UNIQUE(merchant_id,
    issuer, subject) — resolved via ON CONFLICT ... RETURNING id. This REVERSES #491-slice-1's "customers is
    UUID-only, derived, no lookup column": the natural-key columns return (more explicit/queryable for
    billing) behind a uuidv7 surrogate pk. Audit money_in.go depositSourceID + subscription_credits grantID
    (also uuidv5 idempotency keys) -> same uuidv7-pk + UNIQUE(natural key) pattern.
  - subject = the STABLE uuid the merchant supplies (cozy-art reports PaulFidika by uuid, never username).
  - NATIVE / org-less branch: invoker == payer 1:1, so the customer_id already IS the attribution; a
    delegated_users row is needed for the FEDERATED (org-bound) case where invoker != payer. (Standalone
    OpenRails has no app tables, so org-less subjects likely need no delegated_users row — confirm in impl.)

WHAT THE ACTOR TRACKS (and ONLY this):
1. spend ATTRIBUTION — how much of the payer's money this actor spent, when, on what (payer visibility).
2. abuse caps — per-actor concurrent-capacity cap + per-actor wasted-money budget (#488 per-invoker flat),
   so one bad delegated-user can't burn the payer's whole rate-limit / wasted-money budget or make the
   payer look bad. Rate-limit the actor FIRST (protects the payer).
NO trust-tier — tiers are cumulative-spend/age based (#476); a delegated-user spends none of its own money
and is ephemeral, so a tier is meaningless for it. Tiers stay PAYER-level.

WHAT STAYS ON CUSTOMER (payer): money_accounts, processor_customers (payment methods), tier +
tier_schedules (#476), credit-line (#489), money-denominated spend-caps, #487 max_concurrent_held /
max_single_charge tier caps, #488 per-PAYER bad_spend_windows.

**Tasks:**
- [x] Make `customer` a PURE balance account: DROP (issuer, subject) from customers; customer =
      (id, merchant_id) + money. Done (slice 1, 53f8fb37): migration 024 drops the columns/constraint
      idempotently; the three upserts fold into one EnsureCustomer(id, merchant_id); federated (issuer,
      subject) -> customer maps to a deterministic UUIDv5 (identity.FederatedCustomerID) instead of a
      lookup column. NOTE: existing-row collapse (federated -> one payer per issuer) is NOT migrated —
      each existing UUID id stays its own balance; a separate data-collapse migration if/when needed.
- [x] Track the `actor` (invoker) as a MERCHANT-SUPPLIED opaque label scoped under a customer. Partial
      (slice 2, c290de39): the BUDGET SCOPE actor->invoker is renamed (Go ScopeInvoker; canonical stored
      value 'invoker' via migration 025 + NormalizeScope accepting 'actor' transiently), scoped
      (merchant_id, customer_id, invoker_string). NOTE: the opaque KEY column (`actor`) on
      budget_reservations/budget_window_state and the money_transactions/usage_events/money_spend_limits
      `actor` columns are intentionally NOT renamed (opaque keys; a column rename is high-risk + out of
      the minimal-safe slice). Per-invoker abuse counters onto a first-class actor identity row remains.
- [x] Federated resolution (org-bound issuer): the subject becomes the invoker, NOT a customer.
      Outcome (slice 5): TouchCustomer (controlplane/customer.go) switches on
      Core().ResolveRemoteApplicationOrg(issuer); org-bound => UpsertCustomerByOrg(merchant,org). uuidv5
      FederatedCustomerID DELETED from pkg/identity.
- [x] Resolution switch keys on authkit#80 nullable org_id; uuidv7 pk + ON CONFLICT RETURNING id.
      Outcome (slice 5): migration 027 re-adds customers (org_id, issuer, subject) nullable + two partial
      UNIQUE indexes; UpsertCustomerByOrg / UpsertCustomerByIssuerSubject (RETURNING id). The external-subject
      entitlement lookup switched to LookupCustomerIDsByIssuerSubjects (real lookup, no derived id). grep
      "NewSHA1|FederatedCustomerID|uuidv5" empty in non-test source (doc comments only). Also converted the
      two other uuidv5 idempotency keys (money_in depositSourceID + subscription/purchase grantID) to store
      the NATURAL-KEY STRING directly in source_id (uuidv7 pk + existing UNIQUE(merchant,customer,currency,
      source,source_id)); DepositParams.SourceID is now *string.
- [x] Invoker identity = authkit profiles.delegated_users; invoker_id FK + wire json:"actor"->"invoker".
      Outcome (slice 5): invoker_id uuid added NULLABLE + ADDITIVE (actor text RETAINED) to the THREE true-
      ATTRIBUTION tables (money_transactions / usage_events / money_spend_limits), FK ->
      profiles.delegated_users(id) cross-schema (guarded). DEVIATION (documented in 027): budget_reservations
      / budget_window_state `actor` is a POLYMORPHIC scope_key (subject|role:<uuid>|<invoker> per
      budgets/scopes.go), NOT a pure invoker — renaming to an invoker FK would break role/subject budgets, so
      it stays opaque text. Populated on the DELEGATED path (ResolveDelegated -> TouchInvoker ->
      TouchDelegatedUser RETURNING id -> ResolvedDelegated.InvokerID, threaded through DepositParams /
      RecordUsageParams / Capture). The EMBEDDED/SERVICE path has NO issuer (host asserts identity per the
      EMBEDDED SECURITY MODEL) so it cannot call TouchDelegatedUser -> invoker_id NULL there, opaque actor
      stays the key. Wire: request DTOs gained json:"invoker" (admit/admit-batch, deposit/withdraw/hold,
      authorize, report-wasted-spend, budget-check, open-window, window-settle-item); pre-#491 json:"actor"
      still accepted transiently (resolveInvokerField prefers "invoker").
- [x] Migrate existing per-(issuer,subject) money state for ORG-BOUND issuers only.
      Outcome (slice 5): VERIFIED no real federated data to fold — slice-1 (024) already DROPPED the
      (issuer,subject) columns, so no org-bound customer carries a natural key to collapse; 027 re-adds them
      EMPTY. The fold is STRUCTURAL-ONLY (no-op with a RAISE NOTICE in 027); a real reparent is left for
      if/when federated balances exist.
- [x] Re-key/confirm money/processor/tier/credit-line on customer(balance) only.
      Outcome (slice 5): confirmed — all money_* / processor_customers / tier / credit-line FK customers(id)
      only; the new invoker_id FK lands ONLY on attribution tables. No money state moved onto the invoker.
- [x] RENAMED money_accounts -> money_settings (shipped v0.29.2). Done: it holds NO money and nothing FKs to it
      (all money_* tables FK customers(id)) — it is a per-(customer,currency) settings/policy sibling
      (billing mode, caps, auto-topup, alerts, suspension, tier), NOT an account. "account" collides with
      customer-as-the-account under this issue. Name TBD with owner.
- [ ] Embedded path (CONFIRMED correct as-is): native user (subject = end-user) -> a per-user customer
      balance, invoker -> customer 1:1. This is the org-less/native branch; no change beyond the invoker_id
      rename (invoker = the user; payer = the same user).
- [ ] Move #488 per-invoker flat wasted-budget + a per-invoker concurrent-capacity cap onto the new
      invokers identity (persist attribution keyed by invoker_id; abuse counters may stay in Redis but
      keyed by the real invoker id, not a free-text label).
- [x] SDK + wire: distinguish payer (customer) from actor on Admit/charge. Verified (slice 4): the
      Admit/charge/spend wire DTOs already carry customer_id (payable uuid) + actor (the invoker opaque
      label) as REQUEST fields (serviceAdmitRequest, service_credits, service_credit_window). No code
      change — the split already exists at the wire. NOTE: wire field stays json:"actor" (renaming it to
      "invoker" is a wire-breaking change for existing consumers + a stored-column value; deferred, like
      the Slice 2 column-rename). Per-actor spend attribution surfacing to the payer is unchanged.
- [x] NO customer-claim-in-token needed. Verified (slice 4): no invoker/actor JWT claim exists anywhere;
      the invoker (actor) arrives ONLY as a merchant-supplied opaque request field on Admit/charge/spend
      (= money_spend_limits.actor). The authenticated subject is always a customer or the merchant
      self-token. Confirmed in serviceAdmitRequest + the service_credits/window handlers; no change needed.
- [x] DROP money_balances (read-cache roll-up). Done (slice 3, 5e302967): available = SUM(unexpired
      money_blocks.remaining_amount), held = SUM(active-hold authorized_amount) + open-window unsettled
      (windows reserve held without a hold row). Spend mutex moved to FOR UPDATE on the customers row
      (LockCustomerForSpend). Drift/anomaly queries + GetMoneyBalance/Set*/Insert* deleted. Migration 026
      DROP TABLE money_balances + partial index money_blocks(merchant,customer,currency) WHERE
      remaining_amount>0. Money integration + conformance + controlplane integration gates GREEN.
- [x] Background COMPACTION job. Done (slice 3, 5e302967): the credit-block EXPIRY River job now DELETEs
      fully-spent (remaining_amount=0) and expired money_blocks (DeleteCompactableMoneyBlocks), bounded
      batch, touching money_blocks ONLY — never money_transactions or payments.
- [ ] AUDIT-SAFE: the purchase receipt lives in `payments` (the card charge, "bought $10 June 11") + the
      `money_transactions` transaction_type='deposit' row — NOT in money_blocks. Compaction must NEVER
      delete money_transactions deposit rows or payments; only the derived spendable lots. (money_blocks
      .source_transaction_id -> money_transactions may dangle/null on block delete; the receipt survives.)
- [ ] Resolution (RESOLVED): the MERCHANT = the authenticated OPERATOR (its issuer/tenant/service-token in
      OpenRails-authkit; issuer -> merchant via the issuer registry). CUSTOMER + ACTOR are ASSERTED by that
      merchant for its own namespace — opaque, never re-authenticated by OpenRails. Two interaction patterns,
      same anchor: (1) DIRECT — end-user's merchant-minted token sent to OpenRails; OpenRails verifies,
      subject = customer/actor (doujins). (2) PROXY — merchant server authenticates as itself and REPORTS
      (customer, actor) (tensorhub). NO tenant->merchant FK; owner_tenant_id ownership-only.
- [ ] Tests: federated many-actors-one-payer (spend attributed per actor, money debited from the ONE
      payer, per-actor abuse cap trips before the payer's); embedded actor==customer 1:1; a delegated-user
      has no tier and no money_account.

**Related**
#486 (merchant_subject->customer rename this builds on), #487/#488 (payer-vs-actor runtime caps this
formalizes at the identity layer), #476 (tier-schedule stays payer-level), #489 (credit-line stays
payer-level), #75 (escape-hatch per-delegated-user attributes drive per-actor limits). DEPENDS: authkit#77.

---

