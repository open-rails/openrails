<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 350

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

# #107: processor-reconcile (was: processor-sync)

**Completed:** no
**Status:** redesigned 2026-06-10 per Paul: all configured providers (stripe, nmi/mobius, ccbill, solana) where possible; advisory + enforce modes; remote system is source of truth and is NEVER mutated (admin action queue for cancels/refunds); doubles as empty-instance bootstrap/restore. Implementation not started.

Reconcile local OpenRails billing state against the payment processors as the source of truth, for ALL configured providers where the provider exposes the data: Stripe, NMI/Mobius, CCBill, Solana. Two modes: advisory (report only) and enforce (converge local state to the processor's declared state). The tool NEVER mutates the remote processor — remote actions (cancel, refund) are queued for an admin.

## Metadata

- Category: tooling/feature
- Status: planned (design refreshed 2026-06-10; supersedes the earlier billing-cli processor-sync design — that CLI-first shape is NOT binding)
- Passes: false

## Driving scenario (doujins legacy cutover)

Plan: (1) legacy-migrate doujins user billing data into OpenRails (#343), (2) connect to CCBill + NMI with production credentials, (3) pull processor batch data as the source of truth and reconcile. Suspected drift on the legacy machine:

1. Manual NMI dunning has not run in months.
2. Subscriptions failed to renew but users were never downgraded — they kept premium entitlements, the local subscription was never killed, and NMI keeps retrying the charge monthly.
3. Users with duplicate/overlapping subscriptions (same user signed up twice).
4. (worth checking, probably absent) users being charged without holding premium entitlement.

The same machinery doubles as disaster recovery: restore a (near-)empty OpenRails instance by materializing local records from processor data (bootstrap), then converging.

## Two reconciliation modes

1. **advisory** — fetch remote state, diff, persist + report findings. No writes to billing state.
2. **enforce** — assert the processor as source of truth and converge LOCAL state on the spot: adopt subscription status/periods, revoke entitlements of users without active subscriptions, grant entitlements to paying users missing them, backfill missing payment records. Findings that require REMOTE action (cancel a duplicate subscription, issue a refund) are NOT executed — they land in an admin action queue with a recommended action.

Hard constraint: read-only against every processor. No cancels, refunds, or vault edits, ever; those are deliberate admin actions taken by a human.

## Providers + data sources (fetch where possible)

- **Stripe**: full API — subscriptions, invoices/charges, refunds, disputes, customers. Richest source; supports every check incl. chargebacks.
- **NMI/Mobius**: Query API (query.php; client plumbing exists: NMIClient.sendQueryRequest/QueryURL) — report_type=recurring for subscription state, transaction search by date range, customer_vault for stored payment methods.
- **CCBill**: DataLink batch exports (DataLinkClient.FetchActiveMembers exists but covers ACTIVEMEMBERS only — add expired/cancellation/transaction/chargeback exports).
- **Solana**: on-chain subscriptions-program state IS the source of truth — read Plan + subscription accounts via internal/integrations/solana (respect the read-lag rules: *AtSlot/ReadUntilConsistent), payment history via signature scans. No refunds/chargebacks on-chain.

Each fetcher declares capabilities (subscriptions / transactions / refunds / chargebacks / vault) and the diff engine only runs the checks that provider can answer.

## Architecture

- `internal/reconcile` package, sibling of `internal/audit` (audit = internal DB consistency; reconcile = local vs processor).
- `ProcessorFetcher` interface per provider → normalized `RemoteSnapshot` (subscriptions, transactions, vault entries) + capability flags.
- Diff engine emits **findings persisted to DB** (`reconciliation_runs`, `reconciliation_findings`) with lifecycle `open → auto_fixed | admin_pending → resolved/dismissed`. The admin action queue is simply the findings with `requires_admin=true`. Persistence (not one-shot CLI output) is what makes the advisory report queryable and the admin queue workable.
- Identity + plan matching: `processor_subscription_id` first; for processor-only records (PS-1 / bootstrap) fall back to email/username/merchant-defined fields; catalog provider_links (#329) map processor plan ids → local prices.
- Surfaces: admin API (`POST /admin/v1/reconcile/runs` {mode, processors[], since/until, scope: user|subscription}, `GET .../runs/{id}`, findings list + ack/dismiss) plus a thin CLI wrapper over the same engine; optional scheduled advisory run via River with alerting on new findings.
- Tenant-scoped: a run executes under one tenant; processor credentials resolve from that tenant's processor config.

## Discrepancy taxonomy

- PS-1 processor subscription missing locally — CRITICAL — bootstrap-create when identity+plan resolvable, else admin investigate
- PS-2 local active/past_due, processor cancelled/expired — HIGH — enforce: cancel locally + revoke entitlement (legacy suspicion 2)
- PS-3 status mismatch — MEDIUM — enforce: adopt processor status + period timestamps
- PS-4 processor charge with no local payment — HIGH — enforce: backfill payment record + grant entitlement (legacy suspicion 4)
- PS-5 refund at processor not recorded locally — HIGH — enforce: record refund; entitlement-revocation recommendation → admin queue
- PS-6 chargeback at processor, subscription still active — CRITICAL — enforce: terminate + revoke
- PS-7 vault/payment-method mismatch — MEDIUM — enforce: adopt processor record
- PS-8 duplicate/overlapping active subscriptions for one subject — HIGH — advisory + admin queue (cancel+refund at the processor is a human decision; legacy suspicion 3)
- PS-9 entitlement ↔ subscription mismatch, either direction — HIGH — enforce: grant/revoke on the spot

## Out of scope

- Mutating any processor (by design, forever — admins do that by hand).
- The legacy doujins data import itself — issue #343; this tool then corrects whatever the import got wrong.
- Dunning: OpenRails' River dunning worker already exists (ListDueDunningSubscriptions); once legacy subscriptions are imported and converged, dunning resumes naturally. The legacy machine's dead dunning is evidence to find, not a thing to build here.

## Dunning forensics (advisory report section)

Beyond discrepancies, the advisory run must answer: did manual dunning ever run, when did it stop, and did attempts fail? For every subscription that is past_due locally or cancelled/expired at the processor, line up two timelines:

1. **Processor charge-attempt timeline** — NMI transaction search including DECLINED transactions per subscription (NMI's own monthly rebill attempts show up here even if our system did nothing); Stripe invoice payment attempts; CCBill rebill/expire transactions.
2. **Local dunning state** — last_retry_at / retry_attempts / next_retry_at on the subscription row (imported from legacy by #343).

Cross-referencing the two distinguishes 'dunning tried and failed' (local retry fields advancing, processor declines recorded) from 'dunning never ran' (processor shows months of declines, local retry fields frozen/null). Aggregate output: when the dunning worker last took any action, count of subscriptions with zero attempts vs attempted-but-exhausted, decline reasons histogram. This evidence is attached to the corresponding PS-2/PS-3 findings.

**Tasks:**
- DESIGN: refreshed 2026-06-10 (all providers, advisory+enforce, remote read-only, persisted findings + admin queue)
- [ ] internal/reconcile: ProcessorFetcher interface + normalized RemoteSnapshot with capability flags
- [ ] NMI fetcher: typed Query API calls (report_type=recurring, transaction search by date range, customer_vault)
- [ ] CCBill fetcher: extend DataLink beyond ACTIVEMEMBERS (expired/cancellation/transaction/chargeback exports)
- [ ] Stripe fetcher: subscriptions, charges/invoices, refunds, disputes
- [ ] Solana fetcher: on-chain subscription accounts + payment signature scan (respect *AtSlot/ReadUntilConsistent read-lag rules)
- [ ] Identity + plan matching: processor_subscription_id first, email/username/merchant-defined fallback, provider_links (#329) plan mapping
- [ ] Diff engine producing PS-1..PS-9 findings; migrations for reconciliation_runs + reconciliation_findings (lifecycle: open -> auto_fixed | admin_pending -> resolved/dismissed)
- [ ] Advisory mode: run + persisted, queryable report (counts by type/severity, per-user detail)
- [ ] Dunning forensics: per-subscription processor charge-attempt timeline (incl. declines) cross-referenced with local retry fields; aggregate report (when dunning last ran, never-attempted vs attempted-and-failed, decline reasons)
- [ ] Enforce mode: local-only convergence appliers (subscription status/periods, entitlement grant/revoke, payment backfill) — idempotent, audited, never touches the processor
- [ ] Admin action queue: requires_admin findings with recommended remote action (e.g. cancel duplicate + refund); ack/dismiss endpoints
- [ ] Bootstrap mode: materialize local records from a RemoteSnapshot into an empty instance (disaster recovery)
- [ ] Admin API endpoints (POST /admin/v1/reconcile/runs, GET runs/{id}, findings list/ack) + thin CLI wrapper
- [ ] Optional: River-scheduled advisory run + alerting on new findings
- [ ] Tests with mocked fetchers per provider; runbook doc for the doujins cutover reconcile

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

# #334: Eliminate bun: migrate all SQL to sqlc on pgx/v5, unify query patterns, remove unsafe Sprintf SQL

**Completed:** yes
**Status:**  || BURST REGRESSION 2026-06-10 (Claude, from tensorhub #447 kill-tests): Phase 0 twin-pinning (WithTenantConn acquires bun + pgx conns per request, 2f6e6c3) collapses under a 64-concurrent admit burst: goroutine dump showed hold-bun-wait-pgx + 112 queued for bun, parked 3-4min (client aborts don't propagate through haproxy, ctx never cancels) -> permanent wedge; ~30/150 admits succeed (= pool size), rest time out, tensorhub fails closed. 5ea47b4 bounds both acquires at 4s so the server RECOVERS, but throughput under burst is still ~15-22/150 — two sequential pool acquisitions per request starve cross-pool. e2e is pinned to e2cd5cf (pre-Phase-0) until admits hold burst. Suggested fix: single acquisition (pgx-only once call sites convert), or acquire the pgx twin lazily on first sqlc call site instead of per-request. || BURST REGRESSION FIXED 2026-06-10 (Claude): root cause was Phase-0 serving bun through stdlib.OpenDBFromPool on the SAME pgxpool as the sqlc side — each request then held a bun conn while waiting for a pgx conn from the same 30-slot pool (hold-and-wait deadlock). Fix (two halves): (1) SEPARATE pools — bun now has its own database/sql pool via stdlib.OpenDB (pgxpool.ParseConfig so pool_* DSN params stay legal); requests queue on the first pool BEFORE holding anything, so cross-pool starvation is impossible by construction; transitional cost is a 2x30 combined ceiling, removed with the bun side in Phase 2. (2) LAZY pgx twin — WithTenantConn no longer acquires the pgx conn eagerly; a lazyTenantPgxConn in ctx satisfies gen.DBTX and acquires+pins on the request's FIRST sqlc call site (4s bounded), so unconverted request paths cost zero extra connections. Regression tests added (burst_integration_test.go): 64 concurrent mixed bun+pgx requests against a pool_max_conns=5 pgx pool drain in ~68ms (was: permanent wedge); 16 bun-only requests against a 2-conn pgx pool all pass (twin never acquired). Both RLS suites still green (lazy pin sets/resets the GUC identically). e2e can be unpinned from e2cd5cf for re-verification. || 2026-06-10: Phase 1 modules/credits CONVERTED (14 source files incl. the #335 windows.go merged mid-flight): ~70 new queries (credit_ledger/credit_account_settings/usage_events/invoices + credit_windows), per-module genmap, all tx blocks on TenantTx/RunInTx(pgx); WithTenantConn now no-ops on pgx-tx-scoped DBs; dbtest.OpenAppDB gives integration tests the dual-handle DB converted paths need. Suites: unit green; credits integration green except TestChargeOutstanding_Threshold/MonthEndSweep/TestRunAutoTopups_ChargesAndDeposits — verified PRE-EXISTING on base 529f067 (stash-test), the #335-noted denomination fallout, separate fix. db RLS+burst integration green. || 2026-06-10: Phase 1 entitlements/budgets/productaccess/abuse/admission CONVERTED (timeline queries + exported repo timeline helpers; admission_support.sql for budgets/blocklist/tier policies; productaccess withTx -> TenantTx). Integration: entitlements/budgets/abuse/productaccess green; admission TestAdmit_EndpointGating+TestAdmit_BudgetDeny fail PRE-EXISTING on base (stash-verified, same denomination fallout). || 2026-06-10: Phase 1 webhooks/subscriptions/checkout/payments CONVERTED: all 15 tx blocks (ccbill 9, lifecycle 6) -> TenantTx + NewWithPgxTx; nmi chargeback match, cleanup superseded-stamp, processor_customers, stripe_card -> gen queries. CROSS-CUTTING FIX: repo.IsNotFound(err) (matches pgx+sql ErrNoRows) swept across 26 files — sql.ErrNoRows checks against converted repos were silently broken since the repo-layer conversion. Also notification_queue.data NULL-vs-omitted insert bug fixed (COALESCE '{}'). webhooks/subscriptions/checkout/payments integration suites green. || 2026-06-10: river jobs (hold/credit expiry sweeps on RunInTx+SKIP LOCKED, dunning incl. manual-rebill claim tx, cleanup, catalog drift, subscription manage) + handlers (entitlements raw SQL, admin operations lists, admin refund tx, GetMyCredits) CONVERTED. Sprintf SQL in tenancy/delete.go KILLED: per-table generated count/purge queries (tenant_lifecycle.sql) + compile-time dispatch. LATENT BUG fixed: GetMyCredits joined nonexistent credit_balances.user_id (endpoint 500ed); now joins tenant_subject_id. river/http/tenancy/pkg-service integration suites green. || 2026-06-10: audit CONVERTED (39 queries, 25 checks; interface now *gen.Queries). LATENT BUG: AG-1/3/4 referenced nonexistent admin_grants columns (entitlement/granted_at/expires_at/revoked_at) — any audit run with the admin_grant category failed at runtime; AG-1/AG-4 adapted to real schema (expiry = created_at + duration_days), AG-3 removed (no revocation column). pkg/service catalog_drift + GetCredits + admin grant insert converted (GetCredits had the same nonexistent-user_id-join bug as GetMyCredits). Dynamic list endpoints task was already satisfied by the repo-layer narg/CASE-WHEN pattern — marked done. ALL production bun query sites are now converted; remaining bun usage: models tags, db dual-pin plumbing, dbtest helper, 32 test fixture files (Phase 2). || 2026-06-10 HANDOFF (session end): Phase 2 task 'test fixtures/dbtest off bun' is CODE-COMPLETE — all 32 bun-importing test files converted (4 parallel agents; fixtures now use gen.New(dbi.Pool()) or parameterized pool.Exec raw SQL; dbtest gained OpenAppDB/EnsureTenantSubjectIDPgx/SharedPGXPool). vet -tags=integration clean repo-wide. VERIFIED green post-conversion: internal/db+repo+crypto, webhooks, river, credits-unit. NOT yet re-verified post-conversion: credits integration (agent edits done, suite was still running), pkg/service + abuse/admission/budgets/checkout/entitlements/productaccess/subscriptions integration (converting agent was stopped after finishing edits, before running suites). Only test files still importing bun: internal/db/rls_integration_test.go + tenant_rls_integration_test.go (deliberate bun-side RLS tests, deleted with the bun plumbing). REMAINING Phase 2: (1) run the unverified integration suites (expect pre-existing failures only: admission TestAdmit_EndpointGating/TestAdmit_BudgetDeny + credits TestChargeOutstanding_Threshold/MonthEndSweep/TestRunAutoTopups_ChargesAndDeposits — all stash-verified pre-existing denomination fallout, see #335); (2) strip bun tags from internal/db/models (24 files), delete ModelRegistry/RegisterModels; (3) remove dual-pin + bun pool from internal/db (db.go newBunSideDB/NewWithSQLDB/NewWithBun/NewWithTx/Q/RunInTenantTx/BeginTenantTx/tenant_guc.go, repo/tenant_subject_bun.go, dbtest bun EnsureTenantSubjectID, the 2 bun-side RLS test files; rework burst_integration_test.go for single-pool world; Q(ctx) callers: grep '\.Q(ctx)' — should be none left in prod), delete db.QualifiedTable Sprintf helper, fix internal/app validateDatabase (needs *sql.DB for migratekit — derive via stdlib.OpenDB(pool config) or pass DSN) + internal/migrate/migrator.go (same pattern); (4) drop uptrace/bun+pgdriver+pgdialect+bundebug from go.mod, go mod tidy; (5) full verification: go build/vet ./..., unit + integration suites, task sqlc-check, update README/docs, mark issue #334 tasks complete. CONVENTIONS: sqlc regen = export SQLC_DATABASE_URL=$(bash scripts/sqlc-vet-db.sh 2>/dev/null | tail -1) && go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate && ...vet. repo.IsNotFound(err) for not-found checks (matches both drivers). Branch sqlc-migration, pushed to origin. || 2026-06-10: BUN REMOVED. Models de-tagged (registry deleted; Processor type moved to processor.go). internal/db is single-pool pgx-only: Q/GetDB/RunInTenantTx/BeginTenantTx/SetTenantGUC/NewWithBun/NewWithSQLDB/NewWithTx/QualifiedTable deleted; WithTenantConn pins ONE lazy GUC conn; RLS posture checks on the pool; bun-side RLS tests replaced by posture test (enforcement lives in pgx twin); burst test reworked for single-pool. *sql.DB override dropped from embedded/bootstrap/app options (PGXPool is the embedded path). Migrator + validateDatabase open short-lived database/sql handles over pgx stdlib. tests/ e2e suite (21 files) converted off bun. go.mod: uptrace/* fully gone — authkit tagged v0.17.0 (bun-free) and bumped, so not even an indirect edge remains. README documents the sqlc workflow. Verified: build+vet (unit+integration tags) clean; sqlc generate+vet green, no gen diff; ALL unit suites green; ALL integration suites green except pre-existing (5x denomination #335, controlplane TestBootstrap_SeedsPermissionCatalog — fails on base 529f067, admin_* e2e family #333). e2e suite final gate in flight. || 2026-06-10 DONE: bun fully removed (zero uptrace entries in go.mod/go.sum; authkit bumped to v0.17.0 to clear the last indirect edge). Final verification: build+vet clean (unit+integration tags); sqlc generate+vet green, no gen diff; ALL unit suites green; ALL package integration suites green except documented pre-existing (#335 denomination x5, controlplane bootstrap — fails on base, admission x2). e2e suite: 21 failures, ALL verified pre-existing (16 admin_* = #333; 5 tier/lifecycle verified failing identically on origin/master, filed as #340). e2e fixes landed during verification: price/product status defaults in raw seed inserts (bun omitted zero-value columns), deterministic River-readiness wait in suite boot (bun-era double-init masked an init race), TestMain pins time.Local=UTC (pgx returns timestamptz in local zone). ISSUE COMPLETE.

Replace every bun query, model, and driver in openrails with sqlc-generated, type-safe code over pgx/v5, eliminating github.com/uptrace/bun entirely and unifying the repo's three coexisting query styles (bun builder, bun NewRaw, fmt.Sprintf-assembled SQL) into one: raw SQL files compiled by sqlc, vetted against a real database.

## Metadata

- Category: refactor
- Status: planned
- Prior art: authkit issue #64 (same migration, completed 2026-06; copy its conventions — sqlc v1.31.1 pinned via `go run`, generate+vet paired, queries/*.sql layout, type overrides, annotated raw-pgx escape hatches)

## Why

- Three query styles coexist: ~311 bun builder call sites (NewSelect/NewInsert/NewUpdate/NewDelete), ~50 bun NewRaw sites (mostly internal/audit), and fmt.Sprintf-interpolated SQL (internal/tenancy/delete.go builds `billing.%s` from a table-name list — identifier interpolation, the exact pattern sqlc exists to kill).
- Two Postgres drivers coexist: bun's pgdriver (main DB pool) and pgx/v5 (River, platform audit, crypto DEK store, tenancy secrets, controlplane). sqlc on pgx/v5 lets us unify on one driver and eventually one pool.
- bun models silently tolerate schema drift; sqlc generate+vet catches nonexistent columns/tables at CI time (caught a real latent bug in authkit).

## Survey (2026-06-10)

- 68 non-test .go files import bun; 31 internal test files + 4 pkg/service integration tests do too. NO non-test code under pkg/, cmd/, or go-client/ imports bun — the public API surface is clean, this is an internal-only migration.
- 30 bun models in internal/db/models, ALL hardcoded to the `billing.` schema (DB_SCHEMA configurability is effectively River/bootstrap-only) → sqlc compiles against the billing schema directly.
- 25 repo files in internal/db/repo; heavy module usage in credits, webhooks (ccbill.go alone has 11 RunInTx blocks), subscriptions, entitlements, budgets, productaccess; river jobs; audit checks.
- 22 RunInTx/BeginTx transaction sites.
- One cross-schema query: internal/db/repo/profile.go reads profiles.users (authkit schema, applied via migratekit WithSchema("profiles")).
- Migrations already on migratekit v1.0.0 (bootstrap creates `billing` schema; migrations/postgres has the DDL sqlc needs). ClickHouse (3 migrations, usage events) is OUT OF SCOPE — sqlc doesn't support ClickHouse.

## Architecture decisions

1. **Layout** (standard sqlc, mirroring authkit): hand-written SQL in `internal/db/queries/*.sql` (one file per domain), generated code in `internal/db/gen` (package `gen` — `internal/db` itself stays the hand-written pool/RLS wrapper). sqlc.yaml at repo root; schema input = migrations/bootstrap + migrations/postgres + a small `profiles`-schema shim DDL (declares just the profiles.users columns we query; authkit's migrations are unqualified and can't be compiled into a foreign schema directly).
2. **Engine**: postgresql, sql_package pgx/v5. Type overrides: uuid → github.com/google/uuid.UUID (models already use it everywhere; include nullable form), timestamptz/timestamp → time.Time, jsonb → []byte. emit_pointers_for_null_types. Expect the same nullability-inference fights as authkit (outer ::casts force non-null; CASE WHEN ... NULL forces nullable; sqlc.arg/sqlc.narg for param naming).
3. **Tooling**: sqlc v1.31.1 pinned via `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1` in Taskfile (no go.mod tool dependency). `task sqlc` = generate + vet (db-prepare rule against a real DB, SQLC_DATABASE_URL); `task sqlc-check` adds `git diff --exit-code -- internal/db/gen`. New CI job in ci.yaml: postgres:18 service, apply bootstrap + migrations + profiles shim, run task sqlc-check.
4. **RLS is the crux and must not regress.** Today DB.Q(ctx) returns a pinned bun.Conn carrying the `app.tenant_id` GUC (fail-closed RLS, migration 050, issue #227). Target: WithTenantConn pins a *pgxpool.Conn with the GUC set (reset on release so pooled conns never leak a tenant), Q(ctx) returns a gen.DBTX (satisfied by *pgxpool.Pool, *pgxpool.Conn, pgx.Tx), and a tx helper replaces RunInTx while preserving the SET LOCAL GUC behavior in tenant_guc.go. The existing RLS integration tests (rls_integration_test.go, tenant_rls_integration_test.go, repo/rls_realtable_integration_test.go) are the acceptance gate.
5. **Incremental conversion, dual-pin during transition.** bun and pgx pools coexist (they already do). During the transition WithTenantConn pins BOTH a bun.Conn and a pgx conn, each with the GUC set, so converted and unconverted call sites are both RLS-scoped within one request. Convert domain-by-domain with tests green after each; a single transaction never mixes drivers (convert whole tx blocks atomically). Drop the bun pin and pool last.
6. **Models become plain domain structs.** sqlc generates its own row types; the existing models double as JSON API types in handlers, so repos map gen rows ↔ models at the boundary (handler/JSON contracts don't churn). At the end, strip bun struct tags + delete ModelRegistry/RegisterModels. Models that turn out to be pure DB artifacts can be deleted in favor of gen types.
7. **Dynamic queries**: list endpoints driven by pkg/query options + filter structs (e.g. PaymentFilters: optional filters + runtime sort column/order) are sqlc's weak spot. Rule: sqlc.narg()-based static queries where the shape allows; truly runtime-assembled SQL (dynamic ORDER BY) keeps an ANNOTATED raw-pgx escape hatch (authkit AdminListUsers precedent — comment explains why it can't be sqlc). Escape hatches must use parameter binding for all values; identifiers only from compile-time allowlists.
8. **bundebug replacement**: a pgx QueryTracer logging via logrus behind the same verbosity toggle.

## Out of scope

- ClickHouse queries/migrations (sqlc has no ClickHouse support).
- River internals (riverpgxv5 already on pgx; untouched).
- Any pkg/ or go-client/ API changes (none needed — bun never leaked into the public surface).
- migrations content changes (DDL stays as-is; sqlc only reads it).

**Tasks:**
- [x] Phase 0 — sqlc.yaml at repo root: engine postgresql, sql_package pgx/v5, queries internal/db/queries, out internal/db/gen (package gen), schema = migrations/bootstrap + migrations/postgres + new profiles-schema shim DDL for the profiles.users cross-schema query; type overrides (uuid->google/uuid.UUID incl. nullable, timestamptz/timestamp->time.Time, jsonb->[]byte), emit_pointers_for_null_types; prove with 2-3 pilot queries that generate+vet pass
- [x] Phase 0 — Taskfile: `task sqlc` (generate + vet via `go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1`, SQLC_DATABASE_URL defaulting to the local dev DB) and `task sqlc-check` (adds git diff --exit-code -- internal/db/gen)
- [x] Phase 0 — CI: new sqlc job in .github/workflows/ci.yaml — postgres:18 service, apply bootstrap + openrails migrations + profiles shim, run task sqlc-check
- [x] Phase 0 — driver unification groundwork in internal/db: open a pgx/v5 pool alongside bun.DB (dual handles during transition), port pool tuning (30/10/1h/15m), replace bundebug with a pgx QueryTracer on logrus behind the same toggle
- [x] Phase 0 — RLS plumbing on pgx: WithTenantConn dual-pins bun.Conn + *pgxpool.Conn with app.tenant_id GUC (reset both on release); Q(ctx) gains a pgx-side accessor returning gen.DBTX; tx helper replacing RunInTx that preserves the SET LOCAL GUC behavior of tenant_guc.go; all existing RLS integration tests green on the pgx path (fail-closed verified)
- [x] Phase 1 — convert internal/db/repo core: tenant_subject, payment, payment_method, subscription, checkout_session, price, product, profile (+ their integration tests); build + tests green
- [x] Phase 1 — convert internal/db/repo rest: entitlement, entitlement_feature, entitlement_grace, entitlement_timeline, credit_type, admin_grant, notification_queue, linked_wallet, solana_subscription, usdc_funding_session, product_access_grant; build + tests green
- [ ] Phase 1 — convert internal/modules/credits (money_in, authorize, unified_spend, arrears, credits_service + tests); whole tx blocks move atomically to the pgx tx helper
- [ ] Phase 1 — convert internal/modules/webhooks (ccbill.go's 11 RunInTx blocks, nmi, stripe), subscriptions/lifecycle_service, checkout/purchase_service (+ tests)
- [ ] Phase 1 — convert internal/modules entitlements, budgets, productaccess, abuse, admission (+ tests)
- [ ] Phase 1 — convert internal/river jobs (dunning, credit_expiry, hold_expiry, catalog_reconciliation), internal/http/handlers (admin_payments tx, entitlements raw SQL), internal/app/build_runtime, services/health/postgres_checker, crypto/dek_store_db, internal/tenancy (+ tests)
- [ ] Phase 1 — convert internal/audit: all ~40 NewRaw consistency checks become sqlc queries (they are static SQL; vet now guards them against schema drift)
- [ ] Phase 1 — kill unsafe SQL: tenancy/delete.go fmt.Sprintf(`billing.%s`) count/delete loops -> static generated per-table queries (the table list is compile-time); then re-grep the whole repo for Sprintf/concat-into-SQL and fix any stragglers
- [ ] Phase 1 — dynamic list endpoints (pkg/query + filter structs, e.g. PaymentFilters): sqlc.narg() static queries where feasible; remaining runtime-assembled SQL gets annotated raw-pgx escape hatches (values always bound, identifiers from compile-time allowlists); document the escape-hatch rule
- [ ] Phase 2 — convert remaining test fixtures/helpers off bun (31 internal + 4 pkg/service test files) and switch internal/dbtest harness to hand out the pgx pool
- [ ] Phase 2 — strip bun tags from internal/db/models (keep plain structs where they serve JSON contracts; delete pure-DB ones in favor of gen types); delete ModelRegistry/RegisterModels; repos map gen rows <-> models at the boundary
- [ ] Phase 2 — remove dual-pin and the bun pool from internal/db (Q returns pgx DBTX only); drop uptrace/bun, pgdialect, pgdriver, bundebug from go.mod; go mod tidy; `grep -r uptrace/bun` returns nothing
- [ ] Phase 2 — full verification: go build ./... + go vet ./... clean, unit + integration (-tags=integration) suites green, task sqlc-check green in CI; update README/docs that mention bun

---

# #336: Multi-tenant writers don't stamp tenant_id: checkout_sessions / subscriptions / payments / entitlements rows land under the DEFAULT tenant on delegated self-checkout

**Completed:** no
**Status:** open (found 2026-06-10 during the doujins->hentai0 cross-origin payment E2E)

A real /v1/self/checkout charge (delegated token: iss=http://hentai0:4000, tenant=doujins, registered in billing.tenant_delegated_issuers) produced billing.checkout_sessions, billing.subscriptions, billing.payments, and billing.entitlements rows ALL with tenant_id=00000000-0000-0000-0000-000000000001 (default tenant), while billing.tenant_subjects correctly mapped the subject to the doujins tenant in the same request. ROOT CAUSE: those tables' tenant_id columns default to the hardcoded default-tenant uuid (the migration comments say: defaults to the 'default' tenant for single-tenant writers, stamped explicitly by multi-tenant writers) but the checkout-path writers never stamp it - models.CheckoutSession / Subscription / Payment / Entitlement carry NO TenantID field at all, so the INSERT always takes the column default. DelegatedSelfRequired DOES pin the resolved tenant on the request ctx (internal/http/middleware/ginmw/delegated.go:97) and TenantDBConn sets the app.tenant_id GUC, but the GUC only matters for RLS (no-op under the privileged dev role) - it does not change the INSERT default. Same bug class as the CreatePrice TenantID fix (pkg/service/service_definition_catalog.go:430). IMPACT: reads still work today because the self-surface scopes by tenant_subject_id, but tenant attribution/reporting is wrong, and under openrails_app+RLS (managed multi-tenant) these inserts would fail or rows would be invisible to the owning tenant. FIX SKETCH: add a TenantID field (bun tenant_id,type:uuid,nullzero) to CheckoutSession/Subscription/Payment/Entitlement (+ payment_methods/processor_customers/invoices - audit all tables with a tenant_id column whose model lacks the field) and stamp tenant.FromContextOrDefault(ctx) at the insert sites; or change the column defaults to current_setting('app.tenant_id')::uuid so the GUC pins it. Coordinate with #334 (bun->sqlc migration) - whichever lands second must carry the stamping.

**Tasks:**
- STEPS:
- [ ] Audit: list every billing.* table with tenant_id whose Go model lacks a TenantID field
- [ ] Decide: explicit stamping at insert sites vs current_setting('app.tenant_id') column default (GUC is already set by TenantDBConn/TenantTx)
- [ ] Implement for checkout_sessions, subscriptions, payments, entitlements (+ payment_methods, processor_customers, invoices if affected)
- [ ] Integration test: delegated self-checkout under a non-default tenant asserts tenant_id on all written rows == the token's tenant
- [ ] Verify against the doujins/hentai0 E2E stack (delegated issuer http://hentai0:4000, tenant doujins)

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

# #344: dunning-staleness-window + processor-subscription-deletion kill switch

**Completed:** no
**Status:** IMPLEMENTED 2026-06-10 (Claude, uncommitted): config flags + FailMembership.Terminal + dunning window (check precedes client-nil check so stale subs expire even unconfigured) + NMIClient.SubscriptionDeletesDisabled/ErrSubscriptionDeletesDisabled + all 5 call sites degrade per design. Verified: build/vet clean; unit suites green (config, nmi, river, subscriptions, checkout, app); FULL dunning integration file green incl. 2 new window tests (expired->cancelled+downgraded without charge, recent->stays past_due). FOLLOW-UPS DONE 2026-06-10 (Claude, uncommitted): boot rescan (Runtime.RescanPendingDeferredDeletes on worker start re-enqueues cancelled+marker subs at max(now, marker); skipped on external-River path) + lifecycle deferred delete (SubscriptionLifecycleService.SetDeferredDeleteScheduler, wired in build_runtime for the webhook path; terminal NMI-backed FailMembership persists DeletionScheduledAt in-tx and enqueues the delete job post-commit at now; gated off in limited mode per #345; dunning worker's own lifecycle instance deliberately left unwired — its window-expiry path deletes inline) + restored deletion_scheduled_at to UpdateSubscriptionAt (silently dropped in the #334 sqlc migration, so the marker never persisted on update); 4 new integration tests + full TestDunningWorker regression green (15/15).

Two safety mechanisms for the billing lifecycle, motivated by the doujins legacy cutover (#343) but permanent product behavior: (1) a dunning staleness window — dunning may only attempt charges within N days of the missed rebill; anything older is cancelled + downgraded WITHOUT charging; (2) a kill switch that blocks all outbound processor-side subscription deletions (NMI delete_subscription) so cutover/reconciliation can run without bulk-deleting remote subscriptions.

## Metadata

- Category: feature/safety
- Status: in_progress (designed + being implemented by Claude 2026-06-10)
- Passes: false

## Why

- A card that failed in February must never be surprise-charged by a catch-up dunning run in June. The 4-hourly dunning worker queries `past_due` with `next_retry_at <= now` — months-stale subscriptions (e.g. imported from the legacy machine) are all immediately "due". The dry_run_only boot flag mitigates at cutover, but the window is the structural fix: dunning operates within [period_end, period_end + window], default 15 days; past that the user is downgraded and the subscription cancelled instead.
- NMI recurring subscriptions do NOT auto-delete on failure: OpenRails must call delete_subscription, then downgrade entitlements. During cutover we do not want bulk remote deletions (risk: mass delete + downgrade on bad local state). Kill switch = `feature_flags.disable_processor_subscription_deletions` (env `FEATURE_FLAGS_DISABLE_PROCESSOR_SUBSCRIPTION_DELETIONS=true`); local lifecycle proceeds, remote subs stay alive, #107 reconciliation finds and disposes of the orphans.

## Design decisions (locked)

1. **Window config**: `FeatureFlags.DunningWindowDays` (koanf `dunning_window_days`), default `DefaultDunningWindowDays = 15`; <=0 means default. `Config.GetDunningWindow() time.Duration`.
2. **Window anchor**: `subscription.CurrentPeriodEndsAt` (= the missed expected bill time). DunningWorker.processSubscription checks BEFORE claiming/charging: `now > periodEnd + window` → no charge; terminal cancel.
3. **Terminal cancel path**: `FailMembershipParams.Terminal bool` — forces the existing max-retries cancellation branch (cancelled + cancel_type=expired + entitlement & grace revocation + premium-ended notification) without incrementing retry counters (no charge was attempted). Nil-safe when RetryAttempts is nil.
4. **Kill-switch enforcement at ONE choke point**: `NMIClient.SubscriptionDeletesDisabled` field (set once in build_runtime createNMIClients from the flag); `DeleteRecurringSubscription` returns sentinel `nmi.ErrSubscriptionDeletesDisabled`. No call site can bypass it; future call sites fail loud (error) rather than deleting. Call-site degradation:
   - deferred-delete worker (jobs_subscription_manage.go): skip, KEEP DeletionScheduledAt marker, return nil (no retry-spin).
   - user immediate cancel (user_service.go): proceed with local cancel, set DeletionScheduledAt=now as durable orphan marker.
   - admin cancel (admin_service.go): proceed local, loud log.
   - checkout tier-change supersede (checkout/service.go cancelNMISubscription): proceed, loud log — leaves a remote duplicate; #107 PS-8 catches it.
   - dunning window-expiry (new): attempt remote delete so NMI stops retrying the dead sub; on sentinel just log — reconcile disposes.
5. **Replay story**: there is NO boot rescan of pending DeletionScheduledAt today — after the flag is lifted, leftover remote subs are found by #107 reconciliation (PS-2/PS-3/PS-8) and the deferred markers; an optional boot rescan is a follow-up task, not a blocker.
6. Scope: NMI/Mobius only. CCBill cancellation is operator/CCBill-side; Stripe cancel paths are not part of this switch (note for #107: reconcile covers all providers anyway).

## Known pre-existing gap (documented, separate fix)

Webhook-driven dunning exhaustion (FailMembership reaching MaxDunningFailures from a webhook rebill-failure, not from the worker) cancels locally but NEVER deletes the remote NMI subscription — NMI keeps retrying monthly forever. The worker paths now handle remote delete on terminal cancel; the webhook path still doesn't (lifecycle service has no NMI clients). Tracked as a task here.

**Tasks:**
- [x] Config: FeatureFlags.DunningWindowDays (+GetDunningWindow, default 15d) and DisableProcessorSubscriptionDeletions (+IsProcessorSubscriptionDeletionDisabled); Config wrappers; startup logFeatureFlagsStatus warning
- [x] config.example.yaml: document dunning_window_days + disable_processor_subscription_deletions
- [x] FailMembershipParams.Terminal + FailMembership terminal branch (no retry increment, nil-safe RetryAttempts)
- [x] NMIClient.SubscriptionDeletesDisabled field + ErrSubscriptionDeletesDisabled sentinel in DeleteRecurringSubscription; wire flag in build_runtime createNMIClients
- [x] DunningWorker: window check before claim/charge -> terminal cancel + gated remote delete; outcome counters (succeeded/failed/window_expired) in run summary
- [x] Call-site sentinel handling: deferred-delete worker (keep marker), user immediate cancel (DeletionScheduledAt=now marker), admin cancel, checkout supersede
- [x] Tests: window-expiry decision, sentinel gate, terminal FailMembership; build/vet + unit suites green
- [x] Follow-up: boot rescan re-enqueues pending DeletionScheduledAt after flag lift (optional; #107 reconcile is the catch-all)
- [x] Follow-up (pre-existing gap): webhook-driven dunning exhaustion never deletes the remote NMI sub — needs NMI client access or a scheduled delete job from lifecycle

---

# #345: limited production mode: block all proactive payment-provider actions, keep reactive paths

**Completed:** no
**Status:** IMPLEMENTED 2026-06-10 (Claude, uncommitted): all tasks done. Verified: build+vet clean on touched packages; config unit tests green; integration green (limited-mode test: window-stale past_due sub left untouched — not charged, not cancelled; both #344 window tests still green).

A general "limited production mode" (`feature_flags.limited_mode` / `FEATURE_FLAGS_LIMITED_MODE=true`): OpenRails takes NO proactive action against any production payment provider, while everything user/admin-initiated (reactive) keeps working normally. Boot posture for migration cutovers, reconciliation runs, and incident response — one flag instead of remembering the right combination of narrower flags.

## Metadata

- Category: feature/safety
- Status: planned 2026-06-10 (design below), implementation by Claude same day
- Passes: false

## Semantics

PROACTIVE (system-initiated against a provider) — BLOCKED in limited mode:
- Dunning charges and window-expiry cancellations (#344): dunning behaves as dry_run_only regardless of dunning_mode (unless dunning_mode=off, which stays off) — due subscriptions are listed/logged, retry state preserved, nothing charged, nothing cancelled.
- Auto-top-up charges (AutoTopupWorker, #239).
- Arrears collection charges, hourly threshold + monthly sweep (ArrearsChargeWorker, #241/#301).
- Solana recurring pulls (SolanaCrankWorker, #256 — delegated transfers move real funds).

REACTIVE (someone asked) — UNAFFECTED:
- Checkout charges, card/vault saves, tier changes, resumes, admin refunds.
- User/admin cancellations INCLUDING their processor-side delete_subscription (a user who cancels gets really cancelled) — note this differs from the stricter #344 kill switch, which blocks even those.
- Webhook processing (chargebacks, rebill results, etc.).
- Read-only/alert workers (CCBill/catalog/Solana reconciles, gas + low-balance alerts) and local bookkeeping (credit/hold expiry per its own flag, invoice finalize, cleanup).

## Flag interplay (strictest wins)

- limited_mode + dunning_mode=on → effective dry_run_only. dunning_mode=off stays off (off is about failure semantics, not proactivity).
- disable_processor_subscription_deletions (#344) is STRICTER than limited_mode (blocks even user-asked deletes); both can be set — cutover boots set both.
- disable_entitlement_expiration is orthogonal (local access lifecycle).

Cutover boot posture (#343): FEATURE_FLAGS_LIMITED_MODE=true + FEATURE_FLAGS_DISABLE_PROCESSOR_SUBSCRIPTION_DELETIONS=true (limited_mode alone subsumes dunning_mode=dry_run_only).

## Implementation

- config: FeatureFlags.LimitedMode (koanf limited_mode) + IsLimitedMode() on flags and Config; loud startup warning in logFeatureFlagsStatus; config.example.yaml docs.
- DunningWorker.Work: when limited and dunning_mode != off, demote effective mode to dry_run_only (covers charges AND window-expiry cancels, which run after the dry-run early return).
- AutoTopupWorker / ArrearsChargeWorker: add Config field (wired in river_register), skip-with-warning when limited.
- SolanaCrankWorker: skip-with-warning when limited (already has Config).
- NMIDeleteSubscriptionWorker deliberately NOT gated by limited_mode (finalizes user-asked cancels); only the #344 kill switch gates it.
- Tests: config accessors; dunning integration test (limited mode: stale past_due sub stays past_due, untouched).

**Tasks:**
- [x] config: FeatureFlags.LimitedMode + IsLimitedMode + startup warning + config.example.yaml
- [x] DunningWorker: demote effective mode to dry_run_only when limited (covers charges + window-expiry cancels)
- [x] AutoTopupWorker/ArrearsChargeWorker: Config field + skip when limited; wire in river_register
- [x] SolanaCrankWorker: skip when limited
- [x] Tests: config accessors + dunning limited-mode integration test

---
# #346: unified MODE dial (test | production | limited | readonly) + catalog-write gating + dead-flag removal

**Completed:** no
**Status:** IMPLEMENTED 2026-06-11 (Claude, committed): mode dial (test|production|limited|readonly) + mode-aware accessors (feature_flags.limited_mode REMOVED, verify_processor_mappings dead-flag REMOVED); NMI readonly choke (sendDirectRequest -> ErrProviderReadOnly); catalog write gate (errRemoteWritesDisabled at mobius/stripe Attach create-points, dispatcher AutoCreate short-circuit -> pending_manual_link, 3 Update dispatch sites gated via catalogRemoteWritesDisabled); mode banner at startup; Validate rejects unknown modes + mode=test outside dev; README + config.example.yaml rewritten around the dial. Verified: build/vet clean repo-wide, unit green (config/pkg-service/nmi/river/subscriptions/app incl. new mode-matrix, NMI readonly, dispatcher-defers + mobius-attach-defers tests), mode-dependent integration tests green. Remaining: the Stripe-reactive-write follow-up task.

One top-level `mode` setting (yaml `mode:` / env `MODE=`) that picks an operating preset, replacing the unintuitive `feature_flags.limited_mode` boolean. Decided in conversation 2026-06-11.

## Metadata

- Category: feature/safety
- Status: in_progress
- Passes: false

## The four modes

- **test** (default — preserves today's safe default): every processor routed to sandbox; FULL behavior (charges, dunning, deletes all run — just no real money).
- **production**: live processors, full behavior.
- **limited**: live processors; reactive-only — everything #345 blocked (dunning charges + window cancels, auto-top-ups, arrears, Solana pulls) PLUS catalog remote writes (see below). User/admin-initiated operations work, including their processor-side deletes.
- **readonly**: live credentials, ZERO provider writes — even reactive ones. For reconciliation/forensics boots (#107): read processor state, serve local reads; a checkout/charge attempt fails loudly. Implies limited + the deletion kill switch.

## Semantics / precedence

- Strictest wins, no preset-vs-flag precedence puzzles: accessors OR the mode into the existing checks. `IsTestMode() = test_mode || mode==test` (explicit `TEST_MODE` always wins; when mode is set to production/limited/readonly and TEST_MODE is NOT explicitly set, the legacy test_mode=true default is suppressed). `IsLimitedMode() = mode in {limited, readonly}` (feature_flags.limited_mode REMOVED — shipped only yesterday in #345, nothing external depends on it). `IsProcessorSubscriptionDeletionDisabled() = flag || mode==readonly`. NEW `IsProviderReadOnly() = mode==readonly`, NEW `IsCatalogRemoteWriteDisabled() = mode in {limited, readonly}`.
- Fine-grained feature flags stay as overrides/dials on top: deletion kill switch, dunning_mode, dunning_window_days, disable_entitlement_expiration.

## Catalog-write gating (from the 2026-06-11 verify_processor_mappings discussion)

Catalog is the one domain where OPENRAILS is the source of truth, so sync direction reverses: apply may CREATE/UPDATE provider objects (NMI createPlan, Stripe find-or-create Product/Price, Update active-flag). The bootstrap manifest applies on EVERY boot (#327), so a cutover boot could write plans to live processors at startup. In limited/readonly mode: provider VERIFICATION (read) still runs; provider WRITES (AutoCreate, Attach's find-or-create-missing half, Update) are deferred — the price still applies locally and the provider slot goes to the existing pending_manual_link state with a mode-explains-why message; a later apply (every boot) converges once the mode allows writes. Mechanism: `autoCreateContext.RemoteWritesDisabled` consulted by adapters at their write points (return errPendingManualLink), dispatcher treats Attach's sentinel like AutoCreate's.

## Dead flag removal

`feature_flags.verify_processor_mappings` is declared but referenced NOWHERE — #329 superseded it (verification + find-or-create now unconditional in the provider adapters). Remove from config + README + example yaml. Read-only verification needs no flag (Paul: "because it's read only, it should always be on" — it already is).

## Enforcement choke points

- NMI: `NMIClient.ReadOnly` — `sendDirectRequest` (ALL NMI mutations flow through it; query.php reads don't) returns `ErrProviderReadOnly` when mode==readonly. Set in createNMIClients like the deletes switch.
- Catalog: dispatcher + adapter gate above (limited + readonly).
- Workers: existing #345 gates now keyed off the mode-aware IsLimitedMode().
- Solana: cranker covered by limited; user-side flows are user-signed (we read/verify/record).
- Stripe reactive writes (checkout-session creation etc.) have NO single choke point yet — readonly blocks NMI hard but Stripe reactive writes only via the catalog/worker gates; central Stripe gate is a follow-up task.

**Tasks:**
- [x] config: top-level Mode (test|production|limited|readonly, default test), validation, mode-aware accessors, remove feature_flags.limited_mode + verify_processor_mappings, TEST_MODE explicit-set detection, startup mode banner
- [x] NMI readonly choke point: NMIClient.ReadOnly + ErrProviderReadOnly in sendDirectRequest
- [x] Catalog write gate: autoCreateContext.RemoteWritesDisabled, adapter write-point checks (mobius Attach createPlan, stripe Attach lookup-key path + AutoCreate, Update dispatch), dispatcher converts Attach sentinel to pending_manual_link
- [x] README + config.example.yaml: rewrite around the mode dial; remove verify_processor_mappings row
- [x] Tests: mode accessor matrix; NMI readonly gate; resolveProviders limited-mode -> pending (no remote calls); migrate #345 tests off feature_flags.limited_mode
- [ ] Follow-up: central Stripe write choke point for readonly

---

# #347: SDK: fold entitlements read into openrails.Client (ListActiveEntitlements) — token-issuance enrichment for sibling hosts

**Status:** IN FLIGHT 2026-06-11 — implementation already in the working tree (client.go + remote.go + embed/client.go + pkg/service ListActiveEntitlementRecordsForTenantSubject + conformance test additions, ~172 insertions, uncommitted; a concurrent agent owns the code). This section files the issue so the work and its consumers are tracked.

#338 follow-up: add `ListActiveEntitlements(ctx, issuer, subject string, at time.Time) ([]EntitlementRecord, error)` to the unified `openrails.Client` — the payer addressed by its EXTERNAL (issuer, subject) identity, zero `at` = now, unknown subject = empty slice not error — implemented by BOTH transports (remote = the existing `/v1/service` by-external-subject entitlements route; embedded = handler-transcribed service call) and covered by the dual-transport conformance script. Purpose: sibling hosts (doujins, hentai0) enrich their access tokens with entitlement claims at mint time through the SDK instead of hand-rolled HTTP clients or direct SQL into the billing schema.

**Tasks:**
- [ ] Land the working-tree implementation (interface method + EntitlementRecord wire type + both transports + conformance coverage)
- [ ] Push/tag a version containing it so consumers can pin (doujins #390, hentai0 #168 hold local `replace` directives until then)

---
# #348: MODE=test credential guarantees: NMI test-mode probe + Stripe live-key hard-fail

**Completed:** no
**Status:** IMPLEMENTED 2026-06-11 (Claude): the failure protected against is "operator wires PRODUCTION credentials but believes test mode makes them safe". A self-declared account_type label was implemented and then REJECTED by Paul (rightly: the person making the mistake mislabels too) and reverted in favor of real detection.

## Per-provider guarantee under MODE=test

- **Stripe**: keys are self-identifying (sk_/rk_ test/live prefixes). A live key under sandbox-money mode is now a HARD Validate error (was: warn + disable). Test key under live mode stays warn+disable (already harmless).
- **NMI**: undetectable by configuration — sandbox accounts hit the SAME production gateway URL and keys carry no marker (confirmed against docs.nmi.com). Detection instead fingerprints the gateway's documented Test Mode simulation behavior (docs.nmi.com/reference/testing-methods: test card + amount>=1.00 -> simulated APPROVE, amount<1.00 -> simulated DECLINE, nothing reaches a processor): boot probe runs ONE auth $1.00 on the documented test card (4111.../10-29). SIMPLIFIED per Paul 2026-06-11: 4111 is a non-issued PAN no production processor can EVER approve, so approval alone is conclusive proof of simulation — the earlier second $0.50 sub-dollar-decline check was dropped (it encoded NMI's specific simulator rules and could false-positive a white-label NMI-compatible gateway whose test mode simulates differently). Approved = simulating -> safe (auth voided); DECLINED = live account -> REFUSE TO BOOT (harmless on live: one declined-attempt gateway fee, cents; no money moves — but beware supervisor crash-loops re-probing each restart: decline fees accumulate + card-testing fraud-flag risk; verdict cooldown cache offered, not yet built); response=3 (bad creds) or transport error = indeterminate -> loud warning, boot continues (keeps offline dev + CI alive). Implementation: internal/integrations/nmi/probe.go (ProbeTestMode), wired in createNMIClients.
- **Solana**: network is DERIVED from the mode (devnet iff sandbox money) with no override field — structurally guaranteed already.
- **CCBill**: sandbox uses a separate URL derived from the mode; per Paul, does not apply.

## Scope decision

HARD CUT 2026-06-11 (Paul): `test_mode` removed entirely — `mode` is the only dial. Unset mode = test in development; outside development an explicit mode is REQUIRED (Validate refuses to boot). The probe now runs whenever IsTestMode() (incl. dev default) for every configured NMI client; refusal message names the failure explicitly ("PRODUCTION NMI credentials detected while test mode is on ... refusing to start"). Also closed: explicit PROCESSORS_SOLANA_NETWORK=mainnet override is refused under test mode (the derived devnet can be overridden, so the guarantee needed the check). TEST_MODE env mapping deleted; .env.example/config.example.yaml/README updated; all test fixtures migrated to Mode. Unconfigured clients (empty key) still skipped; probe errors still warn-and-continue.

**Tasks:**
- [x] Stripe: live key + sandbox money -> hard Validate error; tests updated to new semantics
- [x] NMI: ProbeTestMode fingerprint probe + createNMIClients boot wiring (refuse on ProbeLive, warn on indeterminate); 5 probe unit tests
- [x] account_type declaration reverted (rejected design)
- [x] README: document the guarantees on the mode table

---

# #349: config.FlexiblePort is int16 — ports above 32767 overflow negative and run-server refuses to listen

**Status:** open (found 2026-06-11 by the doujins testcontainers harness booting a real openrails on a kernel-ephemeral port).

`config/config.go:25` declares `type FlexiblePort int16`. Any configured port above 32767 — the kernel's default ephemeral range STARTS at 32768 — wraps negative (44553 → -20983) and run-server dies with `listen tcp: address -20983: invalid port`. TCP ports go to 65535; the type should be a plain int with 1-65535 range validation. Doujins' integration harness works around it by allocating only sub-32768 ports (tests/openrails_harness.go freeLocalPortBelow32768).

**Tasks:**
- [ ] Widen FlexiblePort to int with 1-65535 range validation in UnmarshalText (keep the string/int flexible parse)
- [ ] Doujins follow-up: drop the freeLocalPortBelow32768 workaround once a release carries the fix

---
