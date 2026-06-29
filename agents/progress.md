<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 628

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

# #582: rename `processor` → `rail` across notes + codebase (terminology consolidation; breaking DB + API allowed)

**Completed:** no

Decided 2026-06-24 (Paul + Claude). On-brand consolidation: **"rail" becomes THE term for a payment channel/lane.** `processor` and `rail` are synonyms — the `models.Processor` enum values (`mobius`/`ccbill`/`stripe`/`solana`/…) literally ARE the rails. `provider` is a **different** concept (a credentialed *account on* a rail) and **stays**. Breaking changes to DB + API are authorized now (pre-production; **no migrations** — edit the schema in place and `task sqlc`). Behavior does not change; this is cosmetic/brand only.

## Decision / scope boundary (read first)
- **RENAME:** everything meaning the payment lane/type currently called `processor` → `rail`.
- **KEEP `provider`** (account-level concept): `provider_accounts`, `provider_account_id` (71-use FK), `provider_intents`, `provider_refresh_watermarks`, `external_provider_mutation_logs`. A rail is the lane; a provider account is an account on the lane (the schema already says "multiple provider accounts per provider rail").
- **KEEP wire/string VALUES:** `"mobius"`, `"ccbill"`, `"stripe"`, `"solana"`, `"paypal"`, `"admin"`, `"manual"` are rail *names*, not the word "processor" — they don't change.
- **No blanket-sed:** rename ONLY the payment-rail sense of "processor". Audit for any unrelated "processor" (generic data/event/webhook processors) and leave those.

## Target naming

**Go**
- `internal/db/models/processor.go` → `rail.go`; `type Processor string` → `type Rail string`; `ProcessorMobius/CCBill/Solana/Stripe/Paypal/Admin/Manual` → `RailMobius/...` (values unchanged).
- `internal/db/models/processor_customer.go` → `rail_customer.go`; `ProcessorCustomer` → `RailCustomer`.
- `internal/modules/payments/processors/` package → `internal/modules/payments/rails/`; `IsNMIBackedProcessor`→`IsNMIBackedRail`, `NMIBackedProcessors`→`NMIBackedRails`, `SameProcessor`→`SameRail`.
- reconcile `ProcessorFetcher` → `RailFetcher`; webhooks `WebhookHandler.Processor()` → `.Rail()`; intents handler `processor` refs; all fields/params/locals named `processor` → `rail`.
- `pkg/` exported API (`pkg/service`, `pkg/embedded`, `pkg/api`): exported `Processor` → `Rail` (**BREAKING** for embedded consumers).

**DB (breaking, no migration — edit `migrations/postgres/*.sql` in place)**
- column `processor` → `rail` everywhere (payments, subscriptions, etc.); the named enum type (if a `processor` enum type exists) → `rail`; table `processor_customers` → `rail_customers`.
- `internal/db/queries/*.sql`: update column refs; `task sqlc` to regenerate `internal/db/gen` + vet against the real schema.

**JSON / HTTP / CLI**
- request/response fields named `processor` → `rail` (**BREAKING** API change — audit `pkg/api`, handlers, `docs/api/endpoints.md`).
- CLI: `pull-provider --provider=nmi,ccbill,stripe,solana` currently takes **rail** names → this is the provider/rail overload below; resolve there.

**Docs/notes**
- README.md, docs/*.md, agents/*.md: "processor" (payment sense) → "rail".
- Add a billing glossary: **rail** = the lane/type (former `Processor`); **provider account** = a credentialed account on a rail; **integration** = the client code under `internal/integrations/`.

## Provider-overload cleanup (decide as part of this)
Today "provider" is overloaded: it means the ACCOUNT (`provider_accounts`) AND sometimes the TYPE (the `provider` column in `provider_intents`/`provider_refresh_watermarks` holds `"stripe"`/`"nmi"`; CLI `--provider=nmi,…`). Renaming processor→rail makes this glaring. Options:
- **(A) Minimal:** rename only `processor`→`rail`; leave every `provider` as-is (accept that some `provider` columns/flags hold a rail value). Lowest churn.
- **(B) Disambiguate (recommended):** also rename the *type-holding* `provider` columns/flags → `rail` (e.g. `provider_intents.provider`→`.rail`, `--provider`→`--rail`), while KEEPING `provider_accounts`/`provider_account_id` as the true account concept. Cleanest end state.
- Either way **KEEP the convergence vocabulary "provider-observed truth"** — the `pull.*` plane is about a provider *account's* observed facts, so "provider" is correct there. Pick A or B before starting.

## Findings (audit DONE 2026-06-24)
**"processor" is overloaded — three senses:**
1. **OUR rail** (`models.Processor`, columns `processor`/`processor_subscription_id`/`processor_customer_id`/`processor_transaction_id`/`processor_fields`/`processor_state`/`processors`, enum `processor_type`, table `processor_customers`, ~283 Go files) → **rename**.
2. **NMI's *acquirer* "processor"** in `internal/integrations/nmi/` + `internal/modules/webhooks/` — `json:"processor_id"`, `json:"processor_response_text"`, the `Processor` acquirer object (`body.Processor.ID`), and decline strings `transaction_was_declined_by_processor` / `transaction_error_returned_by_processor`. These mirror **NMI's actual webhook payload** — renaming breaks parsing. **PRESERVE** (NMI's external wire format; we don't own it, so pre-launch status is irrelevant here).
3. **Value strings** in CHECK constraints: ledger `processor_clearing`, blocklist `processor_customer` (singular, quoted). **PRESERVE**.

**Coupling:** DB column `processor` → sqlc-gen field `.Processor` → referenced everywhere ⇒ this is ONE coordinated transform (Go + SQL schema + queries + `task sqlc` regen), not a Go-only first pass.

**Method:** sentinel-protect the value strings (#3); EXCLUDE the NMI/webhook wire dirs from the *lowercase* sweep (preserves their acquirer wire tags #2 — their CamelCase fields still rename to `Rail*` but the lowercase `json:` tags survive, so wire stays correct); CamelCase `Processor`→`Rail` + lowercase `processor`→`rail` everywhere else; `git mv` package `processors`→`rails`; `task sqlc` regen; `go build`/`vet`/test (compose stack is up: openrails-postgres `:5434`, garnet, clickhouse).

**Pre-launch:** breaking DB/JSON/API changes are fine (Paul, 2026-06-24) — except NMI's external wire (#2). **LOC:** take trivial simplifications opportunistically, but keep the diff reviewable as a rename.

## Tasks
- [x] Audit: enumerate every payment-sense "processor" (Go idents, DB schema, `*.sql`, JSON, CLI, docs); separate from unrelated "processor" uses. **DONE 2026-06-24** — see Findings above.
- [x] Go rename: types/consts/files (`Processor*`→`Rail*`), `processors` pkg→`rails`, fetcher/handler/field renames. **DONE** — 811+56 `*.go` swept; `git mv` package + files; `go build`/`go vet` green; gofmt clean.
- [x] DB rename (breaking, no migration): `processor` col→`rail`, enum `processor_type`→`rail_type`, `processor_customers`→`rail_customers`; queries + `processor_customers.sql`→`rail_customers.sql`; `task sqlc` regen + vet. **DONE** — gen flipped (0 residual `Processor`).
- [x] JSON/HTTP/CLI + **ClickHouse** field rename (breaking). **DONE** — `json:"processor"`→`json:"rail"` (21 of-ours fields), env `PROCESSORS_*`→`RAILS_*`, example config/env, `migrations/clickhouse/*.sql` (caught by `TestAdminMetricsFolded`), `scripts/e2e_dump_local.sh`, `pkg/embedded/README.md`. No `--processor` CLI flag (only `--provider`, kept).
- [x] Provider-overload decision. **DECIDED A** — renamed only `processor`→`rail`; kept ALL `provider` (incl. type-holding `provider_intents.provider` col + `--provider` CLI, which still hold a rail value) + `provider_accounts`/`provider_account_id` + "provider-observed truth". B (rename those too) is an optional follow-up.
- [x] Docs/notes sweep + glossary. **DONE** — README + `docs/*.md` + `docs/api/*.md`; new `docs/glossary-rails.md`. Historical `agents/*.md` left as point-in-time records.
- [x] Grep gate. **DONE** — every residual is an intentional preservation (NMI acquirer wire / `processor_clearing` / `processor_customer`) or out-of-scope (historical notes, other-repo paths, skill-doc examples).

## Validation
- [x] `go build ./...` + `go vet ./...` clean. **PASS** (+ `gofmt -l` clean).
- [x] `task sqlc` generate + vet (PREPAREs every query vs schema). **PASS**. (`task sqlc-check` fails ONLY because gen differs from git HEAD — i.e. the whole uncommitted rename; passes once committed.)
- [~] DB-backed tests. Unit: **71/72 packages green** (the 1 fail, `internal/migrate TestRewriteMigrationsSchema`, is PRE-EXISTING — missing `025/026` migration files, untracked in git). Integration: `./tests` (incl. analytics/CH after the fix), `./pkg/service`, `./embed` **green**; targeted NMI-webhook (wire preserved) + checkout (API/DB) **green**. `./internal/river` 13 liveness/dunning fails are **PROVEN PRE-EXISTING** — identical `products_merchant_fk` on stashed HEAD (a test-harness merchant-seeding/ordering gap; my diff to those files is a pure rename, `internal/dbtest` untouched).
- [x] `rg -i '\bprocessor'` over go/sql/ch returns only intentional/unrelated hits. **PASS**.

## STATUS 2026-06-24 (Claude)
Rename **complete** across Go (`models.Rail`, `payments/rails` pkg, all idents), Postgres schema + queries + sqlc gen, ClickHouse schema, JSON/HTTP fields, env contract (`RAILS_*`), example config, scripts, and docs (+ new `docs/glossary-rails.md`). Three concepts kept distinct: **rail** (renamed from processor), **provider account** (`provider_*`, unchanged), **NMI acquirer "processor"** (wire fields preserved). Behavior unchanged — pure rename. **Uncommitted** in the working tree; not yet committed/tagged, and consumer bump (Cross-repo) not yet done. The one regression introduced (ClickHouse schema lag) was caught + fixed; all other test failures predate this change.

## Cross-repo
- The breaking embedded-API + JSON `processor` rename affects any consumer of openrails' Go API or HTTP that references `processor` (openrails-saas; doujins/hentai0 only if they read a `processor` field). Enumerate consumers, coordinate a version bump, adopt after this lands.

## Notes
- Pure rename, no behavior change — land as a reviewable rename (a Go-commit + a DB/sqlc-commit pair reads cleanest).
- Out of scope (separate, already-flagged decisions): `mobius` as the NMI rail id (rename to `nmi`?); keeping "provider-observed truth" in the convergence taxonomy (yes, keep).

---

# #624: split-merchant-settings-routeset-delete-credentialmode

**Completed:** yes
**Status:** COMPLETED 2026-06-29. HARD CUT landed: `RouteSetMerchantSettings`
and `CredentialMode` are gone; catalog and payment-provider routes are separate
route sets.

Collapse the redundant credential-mutation gating. Today exposing the
provider-credential mutation routes requires BOTH including
`RouteSetMerchantSettings` in `RouteSets` AND setting `CredentialMode = mutable`
— and `embedhttp.validateAuthBoundary` PANICS if they disagree
(`internal/http/embedhttp/embedhttp.go:206`). Two coupled knobs + a panic-on-mismatch
for one decision. Replace with plain route-set membership.

## Metadata

- Category: refactor
- Status: completed
- Passes: true

## Result

- Added `RouteSetCatalog` and `RouteSetPaymentProviders`.
- Embedded defaults now include catalog and exclude payment-provider config.
- Standalone defaults include both catalog and payment-provider config.
- Deleted `CredentialMode`, `CredentialModeFixed`, `CredentialModeMutable`, and
  all `HTTPHandlerOptions.CredentialMode` plumbing.
- Deleted the `RegisterMerchantSettingsRoutes` wrapper; callers register catalog
  and payment providers explicitly.
- Validation: `go test -count=1 ./...`; `go test -tags=integration -count=1 ./embed -run 'Test(Conformance_EmbeddedAndStandaloneAreObservablyIdentical|UpsertMerchantConfig_SeedsProviderAccounts|StandaloneMerchantControlBoundaries|StandaloneRemoteApplicationAuth)$'`; `go test -tags=integration -count=1 ./internal/integrationharness -run 'Test(EmbeddedMountHandlerEndToEnd|StandaloneMerchantCatalogRoutesHTTP|StandaloneMerchantCatalogApplyOptionsOverHTTP|StandaloneMerchantPaymentProviderConfigHTTP|StandaloneNoDefaultMerchantResolvesRequestScopedMerchant|APIKeyCrossMerchantIsolationHTTP|StandaloneMerchantAdmitAcceptsDelegatedJWTByPermissionHTTP|StandaloneMerchantAdmitAcceptsUserSessionByPermissionHTTP|CoreDoesNotMountPlatformAdminRoutesHTTP)$'`; `go test -tags=integration -count=1 ./tests -run 'Test(HTTPHandlerOptions_RouteSetPresetsOverHTTPServer|HTTPHandlerOptions_MerchantRoutesAcceptHostPrincipalPermissions|EmbeddedHandlers_Surface)$'`.

## Current state

`RouteSetMerchantSettings` (`internal/http/embedhttp/route_set.go:16`) mounts exactly
two subgroups via `RegisterMerchantSettingsRoutes` (`internal/http/routes/routes.go:235`):
- `/catalog/*` — products, prices, drift, `publish` (`registerCatalogActionRoutes`,
  routes.go:484). Gated per-route by `merchant:catalog:read/update`.
- `/payment-providers/*` — list/get/PUT/DELETE provider config + secrets
  (`registerPaymentProviderActionRoutes`, routes.go:512). Gated per-route by
  `merchant:payment_providers:read/update`.

(The `route_set.go:15` comment says "provider secrets, catalog pushes, and merchant
config" — stale; there is no separate merchant-config group.)

`CredentialMode` (`fixed`/`mutable`, route_set.go:6/22) is read in exactly ONE place:
`validateAuthBoundary` requires `mutable` when `RouteSetMerchantSettings` is mounted
(embedhttp.go:206). It is a redundant SECOND coarse gate on top of the per-route
permission checks — its only behavior is to error when it disagrees with route-set
membership.

## Decision

- **Catalog is always available.** No reason to gate catalog push behind a coarse
  switch; the `merchant:catalog:*` permission is the real protection. ("Always on" ≠
  unprotected — the permission gate stays.)
- **Provider config + secrets is the only opt-in surface.** Its coarse on/off becomes
  simply "is the route set mounted?" — one knob, the host's model.
- **Storage backend is a SEPARATE, untouched axis.** Vault-if-`VaultConfig.Enabled`
  else DB+envelope (`internal/secretstore`, `internal/merchantsecrets/store.go:35`) is
  already auto-derived from config presence. Exposing the routes is independent of where
  secrets are stored — do NOT couple them.

## Plan

1. Split `RouteSetMerchantSettings` into:
   - `RouteSetCatalog` (`/catalog/*`) — add to `EmbeddedDefaultRouteSets` AND
     `StandaloneDefaultRouteSets` (always-on).
   - `RouteSetPaymentProviders` (`/payment-providers/*`) — opt-in; in
     `StandaloneDefaultRouteSets`, NOT in `EmbeddedDefaultRouteSets`.
   - Split `RegisterMerchantSettingsRoutes` into two registrars (or have the assembler
     mount the two existing `register*ActionRoutes` under the two new sets).
2. Delete `CredentialMode` end-to-end: the enum + consts (route_set.go), the
   `Assembler.CredentialMode` field + `validateAuthBoundary`'s mutable check
   (embedhttp.go), `HTTPHandlerOptions.CredentialMode` (pkg/embedded), the `embed`/`embedded`
   re-exports, and `internal/http/server.go:395` standalone default. Keep the
   user/admin-needs-Authenticator check in `validateAuthBoundary`.
3. Leave `VaultConfig` / secret-store selection completely untouched.

## Acceptance

- Catalog routes mount with no coarse gate (permission-gated only), in both embedded and
  standalone defaults.
- Provider-config routes mount iff `RouteSetPaymentProviders` is selected; no
  `CredentialMode` anywhere in the tree (grep clean).
- No panic path for knob-mismatch (the panic is gone with the cross-check).
- Existing HTTP integration tests green; embedded hosts that previously set
  `mutable` + merchant-settings now just select `RouteSetPaymentProviders`.

---

# #625: unify-merchant-auth-into-single-host-gate

**Completed:** yes
**Status:** COMPLETED 2026-06-29. Merchant-route registration now takes one public
`billingauth.Gate`; `routes.Options` no longer exposes service/delegated/admin
resolver fields, embedded constructor auth fields are gone, and `internal/app` /
`internal/bootstrap` no longer store or accept authenticators. Buyer/user route
authentication remains `billingauth.Authenticator`; that is intentionally separate
from merchant/admin authorization.

OpenRails defines ONE interface for protecting routes; the host supplies the
implementation. "OpenRails asks the host: gate this route — it needs permission P
on merchant M; tell me who the caller is or reject." AuthKit ships a plug-and-play
`Gate`; any other host writes a thin shim over its own auth. Mirrors AuthKit's own
model (one `Verifier.VerifyRequest` → one `Principal{Kind}`), which OpenRails today
reimplements as a hand-rolled dispatch.

## Metadata

- Category: refactor
- Status: completed
- Passes: true

## Landed 2026-06-29

- Added public `billingauth.Gate`, `billingauth.Principal`, and
  `billingauth.GateError`.
- Changed merchant/admin/catalog/payment-provider/API route registration to use
  one `Gate` instead of exposing `ServiceCredentialResolver`,
  `DelegatedResolver`, and `AdminPermissionChecker` on `routes.Options`.
- Kept the existing standalone/control-plane behavior behind an internal
  `httproutes.NewGate(GateOptions{...})` adapter.
- Moved embedded host auth out of `embedded.Options` / `embed.Options` and into
  `embgin.MountOptions`.
- Removed authenticator fields from `internal/app.App`, `app.BootstrapOptions`,
  and `bootstrap.Options`; standalone HTTP auth seams now live on
  `ginboot.Options` and are passed directly into `server.New`.
- Direct gin route helpers no longer fall back to app-stored auth; they use
  explicit `RouteOptions.AuthProvider`, `RouteOptions.Gate`, or
  `RouteOptions.DelegatedAuthenticator`.
- Validation: `go test -count=1 ./...`; `go test -tags=integration -count=1 ./embed -run 'Test(Conformance_EmbeddedAndStandaloneAreObservablyIdentical|UpsertMerchantConfig_SeedsProviderAccounts|StandaloneMerchantControlBoundaries|StandaloneRemoteApplicationAuth)$'`; `go test -tags=integration -count=1 ./internal/integrationharness -run 'Test(EmbeddedMountHandlerEndToEnd|StandaloneMerchantCatalogRoutesHTTP|StandaloneMerchantCatalogApplyOptionsOverHTTP|StandaloneMerchantPaymentProviderConfigHTTP|StandaloneNoDefaultMerchantResolvesRequestScopedMerchant|APIKeyCrossMerchantIsolationHTTP|StandaloneMerchantAdmitAcceptsDelegatedJWTByPermissionHTTP|StandaloneMerchantAdmitAcceptsUserSessionByPermissionHTTP|CoreDoesNotMountPlatformAdminRoutesHTTP)$'`; `go test -tags=integration -count=1 ./tests -run 'Test(HTTPHandlerOptions_RouteSetPresetsOverHTTPServer|HTTPHandlerOptions_MerchantRoutesAcceptHostPrincipalPermissions|EmbeddedHandlers_Surface)$'`.

## What was intentionally not deleted

- `pkg/billingauth.Authenticator`, `AuthenticatorFunc`, and `Required`/`Optional`
  still protect buyer/user routes and provide best-effort identity for rate
  limiting. They are not the merchant/admin authorization seam.
- `pkg/billingauth.DelegatedAuthenticator` and `DelegatedPrincipal` still protect
  the self-service delegated browser surface.
- A first-class external AuthKit `Gate` adapter can be added when an embedder
  needs it; the internal standalone adapter is enough for this refactor.

## Original hard-cut sketch, narrowed during implementation

- `pkg/billingauth.Authenticator` + `AuthenticatorFunc` + `Required`/`Optional`.
- `pkg/billingauth.DelegatedAuthenticator` + `DelegatedAuthenticatorFunc` +
  `DelegatedPrincipal` (folded into the new `Principal`).
- The credential-source ladder in `internal/http/routes/routes.go`:
  `merchantActionPermissionMW` (routes.go:244) + `resolveServiceCredential`
  (routes.go:408) + `authenticateUser` — the try-api-key→remote-app→service-jwt→
  delegated→host→user dispatch.
- The resolver interfaces on `routes.Options` + the assembler:
  `ServiceCredentialResolver`, `DelegatedResolver`, `AdminPermissionChecker`,
  `merchantUserResolver`, `merchantGroupResolver`, `serviceJWTResolver`,
  `remoteApplicationResolver`. These were OpenRails wrapping the AuthKit control
  plane behind its own abstraction; the `Gate` subsumes them.

## What replaced the merchant/admin seam

```go
// OpenRails defines this; it imports NO auth library.
type Gate interface {
    // Verify the request's credential and check the caller holds `permission`
    // on `merchant`. Returns the resolved caller or an error → 401 / 403.
    Authorize(ctx context.Context, r *http.Request, permission string, merchant MerchantRef) (Principal, error)
}

type Principal struct {
    Kind    PrincipalKind // user | api_key | remote_application | delegated | service | host_asserted
    Subject string
    Email, Username string
    EmailVerified   bool
}
```

- Routes declare the requirement, AuthKit-style:
  `gate("merchant:subscription:cancel", ownerOf("id"))`.
- `Gate` is the neutral seam (this dissolves the earlier "keep or drop the seam?"
  question — the seam IS the `Gate`, AuthKit is merely one implementation).
- AuthKit adapter: a `Gate` impl in an OPT-IN package (e.g. `pkg/embedded/authkit`),
  backed by AuthKit's verifier + permission check, so the core still imports no
  AuthKit. Standalone wires this adapter in place of the control-plane resolvers.

## Where the Gate is supplied — the MOUNT call, NOT the constructor (decided)

Auth exists only to protect the HTTP boundary. The in-process `Client()`/`service`
path is trusted — the host already authenticated before calling in-process — so it
needs NO Gate. (True even today: `embedded.Options.Authenticator` is read only by the
HTTP assembler; `service.New`/`Client()` never touch it.)

- DELETE `Authenticator` + `DelegatedAuthenticator` from `embedded.Options` /
  `embed.Options`. The constructor carries no auth (feeds #626's thin constructor).
- The `Gate` is a parameter of the mount call: `NewHTTPHandler(HTTPHandlerOptions{Gate})`
  / `RegisterAPI(group, runtime, WithGate(...))`.
- In-process callers pass nothing — they're trusted.
- OPEN: rate-limiting keys best-effort by user (identify, don't reject), so the Gate
  likely needs an Optional/identify mode alongside `Authorize` (mirrors AuthKit
  `Required`/`Optional`). Decide: Gate exposes both, or rate-limit keys by IP only.

## Stays OpenRails-side (cannot move to the host/AuthKit) — bookends the gate

1. **Resource→merchant resolution** (`ownerOf("id")`): only OpenRails knows which
   merchant owns a subscription/resource (its DB). Runs BEFORE the gate, produces
   the `merchant MerchantRef` passed in.
2. **RLS GUC pinning**: set AFTER `Authorize` succeeds, from the resolved merchant.

## Open decision (flag, not blocker)

- **Token-derived vs resource-derived merchant.** Current code pins merchant from
  the verified token and deliberately never trusts the URL (anti-IDOR). The natural
  `Gate` example is resource-derived (find the resource's owner, check permission on
  it). Both are safe; pick one explicitly — it defines what `ownerOf(...)` means and
  what `Gate` may assume.

## Acceptance

- Merchant/admin/catalog/payment-provider/API route registration accepts one
  `Gate` interface; resolver/checker internals are hidden behind the internal
  standalone adapter.
- Embedded auth is supplied at route-mount time, not engine construction time.
- `internal/app` and `internal/bootstrap` carry no authenticators.
- Integration-tag package compile stays green; standalone behavior is preserved by
  `httproutes.NewGate(GateOptions{...})`.

## Related

- Supersedes the earlier `DelegatedAuthenticator` rename idea.
- Interlocks with #626 (in-process has no token, so merchant is host-supplied per
  call — see `WithMerchant`).

---

# #626: embed-multi-merchant-always-and-thin-constructor

**Completed:** yes
**Status:** COMPLETED 2026-06-29. HARD CUT landed: `embed.New` builds the engine
only; merchant provisioning, migrations, settings seed, and auth are caller-owned.

`embed.New` shrinks to "build the multi-merchant engine." The engine is ALWAYS
multi-merchant; the merchant acted on is supplied per call where the operation is
merchant-scoped. All construction side-effects (migrate, provision, seed settings)
become explicit caller actions.

## Metadata

- Category: refactor
- Status: completed
- Passes: true

## Result

- Deleted `embed.Options.Merchant`, `embed.Options.MerchantSettings`, and
  `embed.Options.RunMigrations`.
- Deleted `Runtime.tenantID` and all hidden per-call merchant injection in the
  embedded client.
- Added `openrails.WithMerchant(ctx, merchantID)` for explicit merchant-scoped
  in-process SDK calls.
- Kept `Runtime.UpsertMerchantConfig` as the explicit provisioning path.
- Deleted constructor-provisioning integration coverage and kept explicit
  `UpsertMerchantConfig` coverage.
- Validation: `go test -count=1 ./...`; `go test -tags=integration -count=1 ./embed -run 'Test(Conformance_EmbeddedAndStandaloneAreObservablyIdentical|UpsertMerchantConfig_SeedsProviderAccounts|StandaloneMerchantControlBoundaries|StandaloneRemoteApplicationAuth)$'`; `go test -tags=integration -count=1 ./internal/integrationharness -run 'Test(EmbeddedMountHandlerEndToEnd|StandaloneMerchantCatalogRoutesHTTP|StandaloneMerchantCatalogApplyOptionsOverHTTP|StandaloneMerchantPaymentProviderConfigHTTP|StandaloneNoDefaultMerchantResolvesRequestScopedMerchant|APIKeyCrossMerchantIsolationHTTP|StandaloneMerchantAdmitAcceptsDelegatedJWTByPermissionHTTP|StandaloneMerchantAdmitAcceptsUserSessionByPermissionHTTP|CoreDoesNotMountPlatformAdminRoutesHTTP)$'`; `go test -tags=integration -count=1 ./tests -run 'Test(HTTPHandlerOptions_RouteSetPresetsOverHTTPServer|HTTPHandlerOptions_MerchantRoutesAcceptHostPrincipalPermissions|EmbeddedHandlers_Surface)$'`.

## Context

This does NOT reverse the org↔merchant 1:1 data model (#527). It removes only the
construction-time BINDING shortcut. A 1:1 host stays 1:1 — it just names its merchant
per call instead of at `embed.New`. In-process has no HTTP request/token to derive a
merchant from, so per-call supply is the honest model anyway.

## What gets deleted (hard cut)

- `embed.Options.Merchant` + the bound-merchant provisioning in `embed.New`
  (embed.go:170-185) + `Runtime.tenantID` (embed.go:130) + the per-call tenantID
  injection in `Runtime.Client()` (embed.go:255) + `a.Runtime.ConfiguredMerchant`
  set from the binding.
- `embed.Options.MerchantSettings` + the startup seeding (embed.go:193-202,
  `hasMerchantSettings`/`startupMerchantSettings`/`embeddedManifestMerchant`).
- `embed.Options.RunMigrations` + the in-constructor `migrate.RunPostgresEmbedded`
  call (embed.go:142-156). No production host sets it (grep); the explicit path
  `cmd/openrails migrate` / `internal/migrate` stays.

## What replaces it

- **Multi-merchant always.** Export `openrails.WithMerchant(ctx, ref)` (thin wrapper
  over the existing `merchant.WithID`/`FromContext`). Merchant-scoped `Client`
  methods read the merchant from ctx and fail closed if absent.
  ```go
  client.CancelSubscription(openrails.WithMerchant(ctx, mref), subID)
  ```
- **Migrations**: caller runs `cmd/openrails migrate` / `internal/migrate` explicitly.
- **Merchant provisioning**: explicit — host calls `boot.ProvisionMerchant` (already
  exists), not the constructor.
- **Settings seeding**: host calls `client.SetMerchantSettings(...)` after construction.
- **`RunWorkers` STAYS**: it's a genuine Runtime lifecycle goroutine (stopped by
  `Close`), not a one-shot construction side-effect.
- **Auth fields ALSO leave the constructor** (owned by #625): `Authenticator` +
  `DelegatedAuthenticator` move to the route-mount call, not `embed.New`. After
  #624+#625+#626 the engine constructor surface is just: `Config`, `PGXPool`,
  `Redis`, `Cache`, `PaymentProviders` (+ `RunWorkers` on `embed.Options`).

## Open decision (flag, not blocker)

- **Context-injected merchant (`WithMerchant`, recommended — matches the internal
  `merchant.WithID` pattern, clean signatures) vs explicit `merchantRef` first arg
  on every merchant-scoped method (un-missable, noisier).**

## Acceptance

- No `Merchant`/`MerchantSettings`/`RunMigrations` on `embed.Options`; no `tenantID`
  binding/injection (grep clean).
- A single engine serves multiple merchants in-process; a merchant-scoped call with
  no merchant in context fails closed.
- doujins/cozy-art updated to supply merchant per call + provision/migrate/seed
  explicitly; build green.
- HTTP path unchanged (already per-request multi-merchant).

## Related

- Interlocks with #625 (in-process merchant is host-supplied; no token to derive from).
- Same theme as #624: side-effects and special-cases out of the constructor.

---

# #627: embed-sdk-simplification-epic

**Completed:** yes
**Status:** COMPLETED 2026-06-29. #624, #625, and #626 landed. The embedded SDK
constructor is infrastructure-only; route groups are explicit; merchant/admin
authorization is a mount-time `Gate`; in-process merchant scope is explicit.

One theme, captured as one epic over the member issues: **`embed.New` shrinks to
"build the engine"; everything else becomes an explicit caller action; auth becomes
one host-plugged interface supplied at route-mount time.** This supersedes the
earlier `DelegatedAuthenticator`-rename and "fix CredentialMode naming" sketches.

## Metadata

- Category: refactor
- Status: completed
- Passes: true

## Landed 2026-06-29

- `embed.New` / `embedded.New` constructor surface is now infrastructure-only:
  `Config`, `PGXPool`, `Redis`, `Cache`, `PaymentProviders`, plus `RunWorkers` on
  `embed.Options`.
- Auth for embedded HTTP is supplied at mount time through
  `embgin.MountOptions{Authenticator, Gate, DelegatedAuthenticator}`.
- Standalone HTTP auth overrides are supplied through `ginboot.Options`; the core
  app/bootstrap graph no longer carries authenticators.
- Merchant-scoped in-process calls use `openrails.WithMerchant`.
- Catalog routes and payment-provider config routes are separate route sets.
- Validation: `go test -count=1 ./...`; `go test -tags=integration ./embed ./internal/integrationharness ./tests -run '^$'`.

## Member issues

- **#624** — split `RouteSetMerchantSettings` → `RouteSetCatalog` (always-on) +
  `RouteSetPaymentProviders` (opt-in); delete `CredentialMode`.
- **#625** — replace exposed merchant/admin resolver/checker plumbing with ONE
  host-supplied `Gate`, supplied at the mount call; keep buyer/user
  `Authenticator` and self-service `DelegatedAuthenticator` as separate route
  auth seams.
- **#626** — multi-merchant always: delete the `Merchant` binding + `tenantID`
  injection (per-call `WithMerchant`); delete constructor side-effects
  `RunMigrations` + `MerchantSettings` (migrate/provision/seed become explicit).

## Cross-cutting decisions (RESOLVED)

- **Auth lives at the route-mount call, not the constructor.** In-process `Client()`
  is trusted (host authenticated upstream) and takes no Gate. `embedded.Options`/
  `embed.Options` lose `Authenticator` + `DelegatedAuthenticator`.
- **The `Gate` IS the neutral seam.** The earlier "keep or drop the neutral auth
  seam?" question dissolves — AuthKit is just one `Gate` implementation; a host on
  another auth library writes its own. Core imports no auth library.
- **Removing the merchant binding does NOT reverse org↔merchant 1:1 (#527).** Only
  the construction-time shortcut goes; a 1:1 host names its merchant per call.

## Deferred calls

- **#625** token-derived vs resource-derived merchant for future resource-owner
  gates. Current code preserves token-derived/control-plane merchant pinning and
  does not trust URL merchant ids.
- **#625** optional identity mode for rate-limit keying. Current code keeps the
  existing `billingauth.Optional` buyer-auth path.
- **#626** context-injected merchant (`WithMerchant`, recommended) vs explicit
  `merchantRef` first arg on every merchant-scoped method.

## End-state constructor surface (after all three land)

- `embedded.Options`: `Config`, `PGXPool`, `Redis`, `Cache`, `PaymentProviders`.
- `embed.Options`: the above + `RunWorkers`.
- Auth (`Gate`) + route-set selection: supplied at the mount call.
- Migrate / provision merchant / seed settings: explicit caller actions.

## Suggested order

#624 (smallest, self-contained) → #625 (Gate; biggest auth surface) → #626
(constructor + multi-merchant; depends on #625 having pulled auth out of the
constructor). Each lands as its own breaking PR; hosts bumped per PR.

## Note

#623 (your AuthKit-style route-group mounting) is adjacent but separate; it still
references `CredentialModeMutable`, which #624 deletes — reconcile #623 when #624
lands.

---

# #628: query-contract-and-performance-harness

**Completed:** yes
**Status:** COMPLETED 2026-06-29. Added the shared OpenRails query-test command
surface, migrated-Postgres sqlc query-contract coverage across the first
high-value billing domains, a 100k-row entitlement lookup plan/perf check, JSON
report output, and a raw-SQL inventory/pruning classification.

## Goal

`task sqlc-check` already proves sqlc queries PREPARE against the migrated schema.
This issue adds the next layer: execute important queries against real seeded data,
assert the results/mutations, and run heavyweight plan/performance tests against
large tables so missing indexes and bad query shapes are caught before production.

The command surface must match AuthKit exactly:

- `task test-query-contracts`
- `task test-query-perf`

## Metadata

- Category: test-infra
- Status: completed
- Passes: true
- Paired AuthKit issue: AuthKit #195

## Progress 2026-06-29

- Added `task test-query-contracts`, matching AuthKit's command name/env
  contract and reusing the existing `dbtest.SharedPostgresDSN` migrated scratch
  Postgres harness.
- Added `task test-query-perf`, with `QUERY_TEST_DATABASE_URL`,
  `QUERY_TEST_KEEP_DB`, `QUERY_PERF_SCALE`, and optional `QUERY_PERF_REPORT`
  support.
- Added `internal/db/querytest` integration tests covering the first
  high-value sqlc domains: provider accounts, catalog products/prices/features,
  customers/rail customers/payment methods, subscriptions, payments,
  entitlements, money account capacity, invoices, usage events, provider intents,
  and reconciliation support queries.
- Added a 100k-row default perf seed using `CopyFrom`, `ANALYZE`, and
  `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)` for
  `EntitlementExistsActive`; it fails if the hot lookup falls back to a
  sequential scan.
- Added `internal/db/querytest/raw_sql_inventory.md`; no obvious duplicated
  static raw query was safe to delete in this pass. Surviving raw SQL is
  classified as DDL/session/lock/test harness, dynamic analytics/policy,
  ClickHouse, control-plane global SQL, or package-local persistence.
- Validation passed: `task test-query-contracts`, `task test-query-perf`,
  `QUERY_PERF_REPORT=/tmp/openrails-query-perf-report.json task test-query-perf`,
  `task sqlc-check`, `go test -tags=integration -count=1 ./internal/crypto ./internal/db ./internal/modules/money ./pkg/service`,
  `task test-integration-all`, and `git diff --check`. The 100k-row perf report
  for `EntitlementExistsActive` used `Index Scan` and completed in 3.646 ms in
  this run.

## Design

### A. Shared command contract

- Add `task test-query-contracts`: starts/uses a real Postgres, runs migrations,
  seeds small deterministic fixtures, and runs query-contract Go tests.
- Add `task test-query-perf`: starts/uses a real Postgres, runs migrations, bulk
  seeds large deterministic fixtures, runs `ANALYZE`, then executes hot queries
  through `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)` with explicit budgets.
- Use identical env names in both repos:
  - `QUERY_TEST_DATABASE_URL` — optional existing DB override.
  - `QUERY_TEST_KEEP_DB` — keep the scratch DB for debugging.
  - `QUERY_PERF_SCALE` — default row scale for perf seeds.
  - `QUERY_PERF_REPORT` — optional JSON report path.
- Keep `task sqlc-check` as the cheap universal schema/query drift gate; do not
  fold perf tests into it.

### B. Query-contract tests

- Add a small harness under `internal/db/querytest` (or nearest existing test
  helper package) for:
  - scratch DB creation
  - migration application
  - deterministic fixture helpers
  - merchant/RLS context pinning
  - sqlc query wrapper access
- Add contract tests by query domain, not one giant generated-method runner.
- Cover the important sqlc groups first:
  - merchants/provider_accounts
  - catalog products/prices/entitlement features
  - customers/rail_customers/payment_methods
  - entitlements/product access/grants
  - money_accounts/ledger/invoices/usage_events
  - subscriptions/payments/provider intents/reconciliation
- Each contract test should prove:
  - query executes
  - returned rows match seeded data
  - mutations change only intended rows
  - missing-row / duplicate / constraint edge cases return the expected behavior

### C. Query-performance tests

- Seed large datasets with `pgx.CopyFrom` / `COPY`, never row-by-row loops.
- Start with representative scales, then allow override:
  - default `QUERY_PERF_SCALE=100000`
  - manual/nightly target `QUERY_PERF_SCALE=1000000`
- Build reusable `Explain` helpers that parse JSON plans and fail on:
  - sequential scan over large tenant/merchant-owned tables unless allowlisted
  - unexpected sort/hash spill or temp blocks
  - excessive shared read blocks
  - bad row-estimate skew for hot queries
  - execution time over query-specific budget
- Store query budgets in a small checked-in manifest, e.g.
  `internal/db/querytest/perf_budgets.yaml`.
- Keep wall-clock thresholds loose; prefer plan shape and buffer budgets because CI
  machines vary.

### D. Raw SQL inventory and pruning

- Add an inventory step for handwritten SQL outside `internal/db/queries`.
- Classify each raw query:
  - convert to sqlc
  - keep raw because it is dynamic SQL / DDL / advisory lock / session setup
  - delete because unused or duplicated
- Require every kept raw SQL path to have either:
  - a query-contract test, or
  - an explicit allowlist reason in the inventory.
- Prefer moving static raw queries into sqlc as domains are covered.

### E. CI policy

- PR/default CI:
  - `task sqlc-check`
  - `task test-query-contracts`
- Nightly/manual CI:
  - `task test-query-perf`
- Add docs explaining that `PREPARE` validates schema compatibility, while
  query-contract/perf tests validate behavior and scaling.

## Acceptance

- `task test-query-contracts` exists and runs against a migrated scratch Postgres.
- `task test-query-perf` exists with the same name/env contract as AuthKit.
- At least the first high-value OpenRails query domains have semantic contract
  coverage: merchant/provider config, catalog, entitlements, money, subscriptions.
- Perf harness can seed at least 100k rows and emit JSON plan/budget reports.
- Raw SQL inventory exists; obvious duplicated/static raw queries are converted or
  deleted.
- `task sqlc-check`, `task test-query-contracts`, and focused normal Go tests pass.

## Non-goals

- Do not blindly auto-execute every generated sqlc method with fake arguments.
  Query args and seed state must be meaningful.
- Do not make million-row perf tests part of every local `task test` run.
- Do not add an ORM or a new query abstraction.

## Notes

- Pair implementation with AuthKit #195 so helpers, command names, env vars, and
  report shape stay identical.
