<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 476

---

# #475: Tenant-defined custom credits (api-credits, gold-coins) — consume-only, no FX

**Status:** planned (not started). Was referenced as "#473" before the id collision; it's #475.

Custom credits are tenant-defined consumable units (api-credits, in-game gold) with NO fixed dollar
exchange ratio (buy 100 for $10 OR 1000 for $80). They are NEVER billed in (see the #474 invariant) —
acquired by purchase (paid in a real currency, any ratio) or product grant, then SPENT on usage.
Tensorhub's "API credits" are NOT custom credits — they are USD money (#474). Reuse the money LEDGER
primitives as-is; do NOT give custom credits a `money_accounts`/invoice/owed row.

Design notes:
- Unit namespace: qualified `tenant-slug/name` (e.g. `tensorhub/api-credit`) = custom credit;
  unqualified (`usd`) = built-in currency (#474). One `currency`/`unit` column hosts both.
- Tasks: (1) a per-tenant custom-credit-type registry (define/list/activate); (2) allow the money
  ledger's `currency` column to hold qualified codes + validate (tenant-owned); (3) product grants
  may target a custom-credit unit; (4) the admission/spend path consumes custom credits;
  (5) ENFORCE the invariant — billing/invoice/owed/charge paths reject non-currency (qualified) units.
- Keep it minimal; build only when a tenant actually needs a non-dollar unit.

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

