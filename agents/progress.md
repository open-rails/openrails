<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 510

---

# #507: Remove legacy non-money gates from service admit

OpenRails still carries a legacy hot-path throughput layer from the earlier Tensorhub design:

- `tenant_throughput` lets the host send rate-limit policy in every admit request.
- `amounts` drives request/token/image/unit windows in OpenRails.
- tier policy `throughput` / `release_windows` and queue-unit reservations enforce RPM/TPM/IPM/RPD-style limits.
- `block_checks` runs an unrelated blocklist gate before the real prepaid/money admission logic.
- tier-policy `entitled_resources` / resource allowlist denies invokes for reasons that belong in Tensorhub's own
  endpoint authorization layer, not OpenRails' billing admission.
- `max_single_charge_amount` / per-job cost cap is an arbitrary service-admit denial that duplicates the real
  affordability check and should not be part of the generic billing gate.

That model is wrong for the current billing architecture. The payer/customer is the subject being governed, and
OpenRails' hot admit path should answer a narrower money question: can this customer afford this estimated request after
existing holds and delegated-spend reservations? Admit-time requests should not carry rate-limit policy, and OpenRails
should not enforce old request/token/image throughput limits or a separate max-in-flight-dollar cap for Tensorhub-style
admission. Prepaid balance + computed credit capacity + active holds already bound spend; Tensorhub can still use
in-flight-dollar policy as a scheduler fairness/reordering signal outside OpenRails' admit gate.

## Target Model
- Trust-tier policy is configured out of band by the merchant/operator and stored in OpenRails.
- Admit requests report only stable identity and billing facts: `customer_id`, `invoker`, `invoker_type`, `resource`,
  `currency`, `estimated_amount`, `request_id`, `source`, and expiry.
- OpenRails no longer accepts host-supplied throughput policy on the hot path.
- OpenRails no longer uses `amounts` to enforce request/token/image throughput windows.
- OpenRails no longer enforces tier-policy `throughput`, per-release throughput overrides, or queue-unit reservations.
- OpenRails no longer enforces `max_concurrent_held_amount` / Tensorhub `max_inflight_micros` as a hard admit gate.
  If Tensorhub keeps a max-in-flight-dollar policy, it is a scheduler fairness/reordering signal, not a second
  affordability check in OpenRails.
- Other money controls remain admission/accounting controls, not payer throughput limits: prepaid balance, Redis holds,
  optional arrears credit capacity, and wasted-spend abuse windows for refunded/costly failures.
- Name the money gate as `start_capacity` everywhere practical:
  `prepaid_available + remaining_arrears_credit - active_holds`. Admit only when the estimated charge fits that
  capacity.
- Arrears exposure should follow the redesigned invoice model. The final gate should derive owed exposure from pending
  unbilled invoice items plus open/past-due invoice receivables, not from a live account-level
  `outstanding_owed_amount` counter. Any use of that counter is transition-only and should disappear with #506.
- The DB capacity snapshot and Redis hold placement must remain race-safe. Redis protects admit-vs-admit races by
  atomically summing active holds, but tests should prove that balance/credit changes cannot slip between the DB
  snapshot and Redis hold placement and admit work beyond real capacity.
- The blocklist gate is also wrong here. Tensorhub/OpenRails prepaid job admission is not a payment-instrument fraud
  flow, and there is no current requirement to block arbitrary identifiers before an invoke. Remove the layer from
  service admit rather than preserving or relocating it in this path.
- Gate #6 is two concerns mixed together. Keep tier resolution only where service admit still needs the payer's resolved
  trust tier to load retained money policy such as wasted-spend windows. Remove the `entitled_resources` / resource
  allowlist denial from service admit. Endpoint/resource authorization belongs to the host application before billing
  admit.
- Current tier resolution also loads `max_single_charge_amount`, but Gate #7 removes that arbitrary per-job cost cap
  from the final service-admit policy surface.
- The per-job cost cap should be removed from service admit. If a request is too expensive, prepaid/arrears
  `start_capacity` already denies it; if the host wants endpoint-specific pricing or product policy, that belongs in
  Tensorhub before billing admit.
- Gate #8 should stay as a wasted-spend abuse backstop, not a normal throughput limit. It has two distinct policies:
  payer/customer wasted spend gets a grace budget and is charged through report-time accounting beyond that point;
  delegated-invoker wasted spend is an admit-time hard cutoff when the invoker is already over budget.
- Remove the manual payer/customer suspension gate from service admit. `suspended_at` is a vague manual policy override
  that does not belong in the hot billing admission path. A payer should be denied because money capacity, delegated-spend
  policy, wasted-spend cutoff, or in-flight exposure says no, not because an operator toggled a generic frozen state.
- Move arrears/payment-method eligibility out of the hot admit path. Admit should consume a precomputed credit capacity:
  "how much can this customer borrow right now?" That separate credit-policy path can consider trust tier, invoice
  receivables, functioning payment method, collection status, and risk. The hot path should then compare
  `prepaid_available + remaining_credit_capacity - active_holds` against the estimate, the same shape as prepaid.
- Tensorhub's current target is credit capacity `0`, so Tensorhub-style admit is effectively prepaid-only plus active
  holds.
- After this cleanup, the intended OpenRails service-admit policy gates are only:
  1. Payer/customer affordability and hold placement: compare `estimated_amount` against `start_capacity` after local FX
     conversion where needed, then place the idempotent request hold if allowed.
  2. Customer-funded invoker spend authorization and budget: when `invoker != customer`, OpenRails must find an explicit
     payer-configured delegated-spend grant for that invoker, role, invoker tier, or other configured scope, then reserve
     against its money-denominated spend windows. This applies equally to Tensorhub-native users, such as org members
     spending an org/customer balance, and to remote-application delegated users, such as Cozy-Art JWKS users spending
     Cozy-Art's customer balance. No matching delegated-spend grant means deny; a missing policy is not unlimited spend.
  3. Delegated-invoker wasted-spend cutoff: when `invoker != customer`, deny if the invoker is already past the
     platform-imposed wasted-spend budget for the relevant window. Direct customer credentials are not hard-denied here;
     customer wasted spend is handled by report-time accounting/charging beyond the grace budget.
  Merchant/service authentication, request idempotency, Redis hold atomicity, and FX conversion are required mechanics,
  but they are not additional Tensorhub user-governance gates.
- Gate #9 and Gate #10 are the old request/token/image throughput idea and should be removed: no per-payer/per-resource
  unit windows, no Tensorhub `amounts` enforcement, and no queue/batch unit reservations in OpenRails admission.
- The retained delegated-spend gate is the generalized "customer lets invoker spend his money" policy surface:
  money-denominated authorization + budget windows/reservations over estimated spend, not RPM/TPM/IPM/image counters. It
  must work for Cozy-Art-style org + remote-application/JWKS users spending the Cozy-Art customer balance, for Tensorhub
  native org members whose role allows them to spend an org/customer's money, and for Tensorhub-native delegated users.
  The payer/customer principal decides the spend policy: windows such as max spend per 5 hours, 7 days, month, or any
  overlapping configured period. Tensorhub owns credential validation and endpoint/resource authorization; OpenRails owns
  the billing authorization question "may this invoker spend this customer's money?" OpenRails service admit now treats
  explicit `budget_policies` rows (`scope=invoker`, `scope=role`, or `scope=invoker_tier`) as the customer-funded spend
  grant and fails closed when no matching delegated-spend policy exists. Tier-policy `budget_windows` are no longer an
  implicit admit grant; Tensorhub now syncs delegated tier budget windows into the explicit `invoker_tier` budget-policy
  scope. If group, org-membership, remote-application, or other customer-funded scopes are supported later, they must be
  explicit delegated-spend policy scopes rather than stale implicit subject/role pools. These reservations should follow
  the same idempotency and release/capture lifecycle as the request hold so denied, failed, retried, and settled work
  cannot leak allocated-spend capacity.
- Cross-repo audit finding: Tensorhub had multiple overlapping surfaces for this idea. The cleanup now maps delegated-user
  tier budget windows to explicit OpenRails `scope=invoker_tier` grants, maps `RoleBudgets` to `scope=role`, stops syncing
  tenant self-caps as admit grants, and deletes the Tensorhub-side `/platforms/me/generation-budget/check` preflight that
  passed caller-supplied windows into OpenRails `BudgetCheck`.
- Remove the payer-level max-in-flight-dollar gate from OpenRails admit. Affordability is already covered by
  `start_capacity`; Tensorhub may keep in-flight-dollar fairness in its scheduler to queue or reorder work, but
  OpenRails should not deny solely because a customer is over a fairness cap.
- Capture/usage reporting can still record operational metadata, but it must not reintroduce an admission-time
  rate-limit policy surface.

## Tasks
- [x] Inventory the legacy throughput admission surface:
      `AdmitRequest.TenantThroughput`, `AdmitInput.TenantThroughput`, `Amounts`, `admission.ResolvedPolicy.Throughput`,
      `ReleaseThroughput`, `QueueLimits`, `ThroughputForRelease`, Redis limiter calls in `Admitter.Admit`, SDK structs,
      embed/remote clients, HTTP handlers, tests, docs, and generated examples.
- [x] Inventory the blocklist admit surface:
      `block_checks`, `AdmitBlockCheck`, `admission.BlockCheck`, `abuse.BlocklistService` dependency construction in
      `pkg/service.Admit`, `BlockedBy="blocked"`, service admit/batch wire shape, SDK structs, docs, and tests.
- [x] Remove `tenant_throughput` from service admit and admit-batch request/response/client contracts.
- [x] Remove the gate #3 implementation from `pkg/service.Admit`: the pre-admitter
      `TenantThroughput` Redis check must disappear rather than be renamed or moved.
- [x] Remove admit-time `amounts` consumption from OpenRails policy enforcement. If a field remains for telemetry, rename
      and route it only to durable usage metadata/capture, not to admission gates.
- [x] Delete per-(merchant, payer, resource) unit-throughput checks from `Admitter.Admit`.
- [x] Delete queue-unit reservation acquire/release from admission settlement paths.
- [x] Remove tier-policy `throughput`, `release_windows`, and `queue_limits` from the enforceable policy model, schema JSON
      parsing/writing, SDK types, docs, and tests unless another non-Tensorhub host has a live use case.
- [x] Remove `block_checks` / blocklist evaluation from the service admit contract. Do not keep a blocklist dependency in
      this path; any future fraud control must be introduced separately with an explicit current requirement.
- [x] Remove `entitled_resources` / `BlockedBy="resource"` evaluation from service admit. Preserve `resource` only as a
      stable reporting/provenance key unless another current billing requirement justifies it.
- [x] Keep tier resolution only for retained money policy that truly belongs in OpenRails admit, such as payer
      wasted-spend windows. It must not imply endpoint authorization or max-in-flight-dollar admission denial.
- [x] Remove `max_single_charge_amount` / per-job cost cap from service admit, tier policy SDK/wire structs, docs, and
      tests unless another current non-Tensorhub billing requirement justifies it.
- [x] Keep the wasted-spend abuse gate, but make the contract explicit: payer/customer wasted spend has a grace budget
      and is charged through report-time accounting beyond that point; delegated invokers are hard-cut from admit when
      they are already over their wasted-spend budget.
- [x] Keep and verify delegated-spend budget windows as money-denominated reservations over `estimated_amount`. They must
      not depend on Tensorhub `amounts`, request/token/image units, or endpoint resources.
- [x] Make delegated-spend fail closed: if `invoker != customer`, OpenRails must require at least one matching
      payer-configured delegated-spend grant/scope before placing a money hold. A missing matching policy denies rather
      than allowing unlimited delegated spend.
- [x] Define the generalized customer-funded invoker spend model for "customer lets invoker spend his money": direct
      invoker, delegated user, verified org role, invoker tier, or another explicit configured scope. Keep payer/customer
      trust tier separate from invoker/delegated-user spend tier.
- [x] Ensure the delegated-spend model is source-agnostic: Tensorhub-native org users and remote-application delegated
      users are both just invokers spending a payer/customer principal's money under that principal's configured
      overlapping money windows.
- [x] Decide the canonical OpenRails storage surface for delegated-spend policy. Prefer explicit budget-scope policies
      (`scope=invoker`, `scope=role`, and optionally a named invoker-tier/group scope if needed) over hiding
      customer-funded invoker-spend windows inside payer trust-tier `TierPolicy.BudgetWindows`.
- [x] Expose the delegated-spend scope key in the public SDK/HTTP/embedded client as `scope_key`, with `role_id` kept only
      as a role-compatible alias. Tensorhub can now write explicit `scope=invoker` grants without overloading role ids.
- [x] Reconcile Tensorhub policy sync with OpenRails enforcement: `RoleBudgets` participate as `scope=role`,
      delegated-user tier `BudgetWindows` participate as `scope=invoker_tier`, and tenant self-caps are not synced as
      OpenRails service-admit grants.
- [x] Revisit Tensorhub `/platforms/me/generation-budget/check`: it should be a read/preflight view over the same stored
      OpenRails delegated-spend policy used by admit, not a second path where Tensorhub passes ad hoc budget windows for
      OpenRails to evaluate. Tensorhub deleted the ad hoc route and removed `BudgetCheck` from its OpenRails admin
      interface.
- [x] Verify delegated-spend reservations use request-level idempotency and are released or settled alongside the money
      hold lifecycle, including admit denial rollback, post-admit failure release, retry, and capture.
- [x] Clean stale budget comments/fields that describe subject/role pools as accidental behavior. Role or invoker-tier
      support is allowed only if it is intentionally modeled as delegated-spend policy.
- [x] Remove `max_concurrent_held_amount` / Tensorhub `max_inflight_micros` as an OpenRails hard admit gate, including
      deny codes, response fields that exist only for hard-denial scheduling, tier policy SDK/wire fields, docs, and
      tests unless another current non-Tensorhub billing requirement justifies them.
- [x] If Tensorhub keeps max-in-flight-dollar fairness, keep it in Tensorhub's scheduler/configuration surface and make it
      explicitly soft/work-conserving: it reorders or queues requests, but affordability is still decided by OpenRails
      `start_capacity`.
- [x] Keep and verify FX conversion for retained money policies: if the admit estimate is in EUR and a delegated-spend or
      wasted-spend policy is in USD (or any other configured policy currency), convert locally before evaluating it. All
      values remain integer micros in their respective currencies.
- [x] Remove manual payer/customer suspension from service admit and delete or quarantine the `suspended_at` /
      `suspend_reason` account-freeze surface if no other current product requirement uses it.
- [x] Move arrears/payment-method eligibility out of service admit. The hot path should consume already-computed
      `remaining_credit_capacity` and should not perform payment-method setup checks itself.
- [x] For Tensorhub configuration, set credit capacity to `0` until a real arrears product is deliberately enabled.
      Tensorhub defaults `billing.platform_arrears_cap_micros` to `0`, no longer syncs that config through delegated-spend
      budget policy, and only changes OpenRails credit limit through the explicit admin arrears endpoint.
- [x] Keep and verify non-throughput admit/accounting gates: prepaid/credit capacity, Redis hold placement, and
      wasted-spend gates.
- [x] Rename/refactor the solvency gate around the explicit `start_capacity` model:
      `prepaid_available + remaining_arrears_credit - active_holds`, with denial when `estimated_amount` exceeds it.
- [x] Align arrears capacity with the invoice-receivables redesign (#506): derive exposure from pending invoice items and
      open/past-due invoice balances; remove or clearly quarantine any remaining `outstanding_owed_amount` dependency as
      transition-only.
- [x] Add regression coverage for the DB-snapshot/Redis-hold race boundary: concurrent admits, captures/releases,
      deposits/withdrawals/collections, and credit-line changes must not allow active holds to exceed current start
      capacity.
- [x] Keep the Tensorhub prepaid admit gate narrow: no daily/monthly spend caps, blocklists, token/request rates,
      arbitrary per-job caps, max-in-flight-dollar hard denials, or other user-governance policy in this path. The normal
      payer controls are prepaid reserve, computed credit capacity, active holds, and costly-failure backstops.
- [x] Update Tensorhub integration guidance: Tensorhub must stop sending `tenant_throughput` and `amounts` for admission.
      Any `max_inflight_micros` style policy belongs in Tensorhub scheduler fairness, not OpenRails service admit.
- [x] Remove or update stale comments that describe OpenRails as enforcing Tensorhub RPM/RPD/TPM/IPM on admit.
- [x] Slim the admit response contract after the removals: do not return resolved scheduling policy such as
      `max_concurrent_held_amount` or per-job charge caps once they no longer drive OpenRails denial. Keep only the
      admit decision, deny reason, hold/capacity facts, policy currency/amount, and retained delegated-spend or
      wasted-spend window status that a caller can legitimately display or act on.
- [x] Coordinate the Tensorhub sibling cleanup: Tensorhub must remove stale #486 assumptions about OpenRails-owned
      `max_inflight_micros`, per-job caps, request/token/image `amounts`, host-supplied `tenant_throughput`, and ad hoc
      generation-budget checks. Tensorhub should retain endpoint authorization and scheduler fairness locally, while
      OpenRails owns affordability, delegated-spend authorization/budget, and delegated wasted-spend cutoff.
      Status 2026-06-16: request-shape removals, windowed-admission removal, max-in-flight/per-job cap response cleanup,
      BudgetCheck route deletion, local scheduler fairness docs, local OpenRails replace, and embedded integration tests
      are done. Tier budget windows now sync as explicit `invoker_tier` grants and role budgets sync as `role` grants.
      Tensorhub's remaining follow-through is route-level proof that native org members and remote-application delegated
      users resolve the expected payer/invoker/tier facts before OpenRails admit.

## Acceptance
- `POST /v1/service/admit` and `/v1/service/admit/batch` have no host-supplied throughput-policy field.
- OpenRails admit decisions cannot be affected by request/token/image `amounts`.
- No Redis throughput or queue-unit reservation is created during admit; Redis is used for money holds and any remaining
  explicitly money-denominated windows only.
- OpenRails service admit has no hard max-in-flight-dollar denial; in-flight-dollar fairness, if retained, is a
  Tensorhub scheduler concern.
- Cross-currency admits compare policy-currency micros after local FX conversion for retained delegated-spend and
  wasted-spend policies; no retained money policy silently assumes USD-only request amounts.
- Payer/customer wasted spend is charged beyond its grace budget through report-time accounting, not by an admit denial.
- Delegated-invoker wasted spend remains an admit-time hard cutoff when the invoker is already over budget.
- Tensorhub-style admit has no manual customer suspension gate and no prepaid payment-setup gate.
- Arrears/payment-method eligibility is not checked inline by service admit; admit consumes a computed credit-capacity
  value. Tensorhub sets that capacity to `0` for now.
- Delegated-spend budget windows are retained only as money-denominated estimated-spend reservations.
- Delegated-spend authorization fails closed: `invoker != customer` cannot spend customer money unless a matching
  payer-configured delegated-spend grant/scope exists.
- Delegated-spend policy supports explicit customer-funded invoker scopes such as direct invoker, delegated user,
  verified org role, org membership, remote-application user, or invoker tier without confusing those invoker scopes with
  the payer/customer trust tier.
- Tensorhub and OpenRails have one delegated-spend contract: Tensorhub authors/syncs policy, OpenRails stores/enforces
  reservations and actuals, and preflight reads the same policy admit will enforce.
- Delegated-spend reservations are idempotent and cannot leak capacity across denied, failed, retried, or
  captured requests.
- The OpenRails admit response no longer exposes removed scheduling/capacity policy fields whose only purpose was
  Tensorhub's old hard or soft max-in-flight-dollar design.
- Tensorhub-style admit does not accept or evaluate blocklist identifiers.
- Tensorhub-style admit does not deny on resource allowlists; endpoint/resource authorization is host-owned and happens
  before billing admit.
- Tensorhub-style admit does not deny on an arbitrary per-job cost cap; affordability is handled by `start_capacity`.
- Tier resolution still happens before money policy evaluation and is not treated as resource authorization.
- The solvency gate is documented and implemented as `start_capacity`; prepaid and arrears paths share the same final
  admit predicate.
- Arrears exposure is invoice-derived after #506, with no live account-level owed counter required for correctness.
- Tests cover concurrent DB balance/credit changes around Redis hold placement and prove no stale capacity admit.
- Payer trust-tier policy is represented as stored OpenRails configuration, not request payload.
- Tensorhub-style admission remains protected by money-denominated controls and the hold lifecycle.
- Docs clearly state that host applications configure payer/customer policy ahead of time and report billable estimates at
  admit time.

## Validation
- `task sqlc`
- `go test ./internal/modules/admission ./internal/modules/ratelimit ./pkg/service ./internal/http/handlers`
- `go test ./...`
- `task build`

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
