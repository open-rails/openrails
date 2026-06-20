<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 552

---

# #549: fix-money-account-settings-units-and-self-editable-surface

**Completed:** yes
**Status:** COMPLETE 2026-06-20: customer money-account settings now use the real ledger currency contract, customer self-service is flat/narrow, USDC/SOL are no longer account currencies, saved OpenRails wallet linking and self-service USDC funding routes are removed, and Solana receipt metadata stays on `payments.metadata`.

Validation: `go test ./internal/http/routes/ginroutes ./internal/http/handlers ./internal/modules/money ./internal/modules/solana ./internal/modules/solana/recurring ./internal/river -run 'Test(SelfService|HostPrincipal|RunAutoTopups|ConfirmEnrollment|Solana|Bearer)'`; `go test -tags=integration ./tests -run TestSelfAccountSurface_HostPrincipalFullLoopAndScoping -count=1`; `go test -tags=integration ./internal/modules/money -run TestRunAutoTopups_ChargesAndDeposits -count=1 -timeout=60s`.

The `/v1/me/account/settings` contract currently mixes correct ledger-currency behavior with misleading field names and too much customer authority. OpenRails uses per-currency ledgers; `currency` means the actual accounting currency for the account, not a display preference or FX presentation layer. Amounts use that currency's registered internal precision. For USD, that means micro-USD (`decimals=6`), not cents. Fields like `auto_topup_amount_cents` are therefore wrong for this API and invite bugs. USDC and SOL also do not belong in the money-account currency registry: USDC is a USD-denominated stablecoin/payment rail, not a separate account currency; SOL is a crypto asset and not useful as a customer billing-account currency.

## Metadata

- Category: cleanup
- Status: complete
- Passes: true

## Decisions

- `currency` on money/account APIs is the actual ledger currency.
- Do not auto-display a USD balance as JPY/EUR/etc. through this API; display preferences belong in the host/app presentation layer.
- Every settings/balance amount is in the native decimal precision OpenRails registers for that currency.
- USD amounts are micro-USD everywhere in the OpenRails money system.
- Remove USDC and SOL from account/billing currencies. Stablecoins and crypto assets may remain processor/payment assets, but account ledgers should use real billing currencies such as USD.
- Customer self-settings should be narrow. Merchant/admin/service routes own billing policy and credit-risk knobs.
- Spend-cap breaches hard-stop by default. Near-limit alerts, if any, use platform constants instead of per-customer `hard_stop_on_breach` / `alert_threshold_pct` settings.
- `billing_mode` remains meaningful account policy/read-state (`prepaid` vs `arrears`), but it is platform-owned and must not be customer-editable.
- Self-service should stay flat under `/v1/me` because host apps already mount OpenRails under paths such as `/billing/v1`. Use `/v1/me/balance`, `/v1/me/transactions`, and `/v1/me/settings`; cut `/v1/me/account*` and `/v1/me/credits*` instead of keeping aliases.
- Customer transaction history should expose understandable movement types (`deposit`, `spend`, `expiry`, `arrears_accrual`, `arrears_payment`) even if internal ledger transfer names stay `owed_accrual` / `owed_payment`.
- Delete the unfinished USDC funding self-service feature; it is not part of the core customer billing surface.
- Do not model AuthKit/account-control wallet linking inside OpenRails. AuthKit may link Solana wallets as an authentication factor; OpenRails should only record wallet/token-account data when it is needed for a concrete payment/subscription flow.
- Delete OpenRails saved-wallet linking entirely: no customer wallet-link routes, no `linked_wallets` table, no linked-wallet repo/model. A customer may pay into their OpenRails account with any Solana wallet; the payment/subscription record is the audit trail.
- `payments.transaction_id` is the provider-specific external payment id. For Solana this is the chain transaction signature. Do not duplicate it in metadata.
- Do not add a one-off Solana payments table. `openrails.payments` is the durable receipt; Solana-specific receipt details live in `payments.metadata`. `openrails.solana_subscriptions` remains only for recurring on-chain subscription state.
- Keep `POST /v1/me/stripe/portal` while Stripe is supported; it is the provider handoff that returns a Stripe Billing Portal URL.
- Keep internal notification storage/email delivery. Downstream audit found Doujins still renders OpenRails billing notifications via `/v1/me/notifications*`, so customer notification inbox routes stay until that host migrates.

## Current Customer-Editable Fields To Review

- `billing_mode` (meaningful, but platform-owned; not customer-editable)
- `max_spend_per_day`
- `max_spend_per_month`
- `max_outstanding_owed_amount`
- `low_balance_threshold`
- `auto_topup_enabled`
- `auto_topup_amount_cents` (wrong; replace with native-precision `auto_topup_amount`)
- `auto_topup_payment_method_id`
- `default_credit_expiry_days` (wrong for customer self-service; platform/merchant decides credit expiry)
- `hard_stop_on_breach` (wrong for customer self-service; hard-stop should be default behavior)
- `alert_threshold_pct` (wrong for customer self-service; near-limit alert threshold should be a platform constant)

## Proposed Customer Self-Service Surface

Final `PUT /v1/me/settings` request body:

```json
{
  "currency": "USD",
  "low_balance_threshold": 10000000,
  "auto_topup_enabled": true,
  "auto_topup_amount": 25000000,
  "auto_topup_payment_method_id": "pm_uuid",
  "max_spend_per_day": 100000000,
  "max_spend_per_month": 1000000000
}
```

- Keep:
  - `currency`
  - `low_balance_threshold`
  - `auto_topup_enabled`
  - `auto_topup_amount`
  - `auto_topup_payment_method_id`
  - `max_spend_per_day`
  - `max_spend_per_month`
- Remove from customer self-service:
  - `billing_mode`
  - `max_outstanding_owed_amount`
  - `default_credit_expiry_days`
  - `hard_stop_on_breach`
  - `alert_threshold_pct`
  - `credit_limit_amount`
  - `tier`
  - `tier_source`
  - `suspended_at`
  - `suspend_reason`
  - `verified_payment_method`

## Customer Route Cuts

- Delete:
  - `GET /v1/me/account`
  - `PUT /v1/me/account/settings`
  - `GET /v1/me/account/transactions`
  - `GET /v1/me/credits`
  - `GET /v1/me/credits/:currency`
  - `GET /v1/me/credits/:currency/transactions`
  - `GET /v1/me/usdc-funding-options`
  - `POST /v1/me/usdc-funding-sessions`
  - `GET /v1/me/wallets/solana`
  - `PUT /v1/me/wallets/solana`
  - `DELETE /v1/me/wallets/solana`
- Keep/replace with:
  - `GET /v1/me/balance?currency=USD`
  - `GET /v1/me/transactions?currency=USD`
  - `PUT /v1/me/settings`
  - `GET /v1/me/payments`
  - `GET /v1/me/usage?currency=USD&from=YYYY-MM-DD&to=YYYY-MM-DD`
  - `GET /v1/me/invoices`
  - `GET /v1/me/invoices/:id`
  - `POST /v1/me/stripe/portal`

**Tasks:**
- [x] Document that account-setting amount fields use the registered currency precision; for USD this is micro-USD.
- [x] Remove USDC and SOL from the OpenRails account/billing currency registry and tests/docs that present them as customer account currencies.
- [x] Keep stablecoin/crypto asset handling, where needed, in payment/processor-specific flows instead of the money-account currency enum.
- [x] Replace the customer wire field `auto_topup_amount_cents` with native-precision `auto_topup_amount`; do not expose cents-specific names in a multi-currency API.
- [x] Audit service/admin account-settings routes for the same `auto_topup_amount_cents` naming bug and either rename them too or document a short compatibility alias.
- [x] Keep `auto_topup_payment_method_id` as the saved payment method selector, but make it a real FK to `openrails.payment_methods(id)` with `ON DELETE SET NULL`.
- [x] In customer self-settings writes, verify `auto_topup_payment_method_id` belongs to the authenticated customer and current merchant before storing it.
- [x] In service/admin settings writes, verify `auto_topup_payment_method_id` belongs to the target customer and current merchant before storing it.
- [x] Replace customer routes with flat names: `GET /v1/me/balance?currency=USD`, `GET /v1/me/transactions?currency=USD`, and `PUT /v1/me/settings`.
- [x] Make `GET /v1/me/transactions` return customer-facing transaction type names; do not leak confusing `owed_*` labels unless explicitly kept as internal metadata.
- [x] Delete customer `/v1/me/account`, `/v1/me/account/settings`, and `/v1/me/account/transactions` route registrations after moving them to the flat routes.
- [x] Narrow `PUT /v1/me/settings` so customers cannot change merchant/admin-owned policy knobs.
- [x] Make the customer self-settings request body exactly: `currency`, `low_balance_threshold`, `auto_topup_enabled`, `auto_topup_amount`, `auto_topup_payment_method_id`, `max_spend_per_day`, `max_spend_per_month`.
- [x] Remove `default_credit_expiry_days` from customer self-settings; credit expiry is platform/merchant policy.
- [x] Remove `hard_stop_on_breach` and `alert_threshold_pct` from customer self-settings; hard-stop is the default and alert thresholds are platform constants.
- [x] Delete self-service `GET /v1/me/credits`, `GET /v1/me/credits/:currency`, and `GET /v1/me/credits/:currency/transactions`.
- [x] Delete now-unused self-service credits handlers/tests/docs after the route cut; keep service/admin account-credit APIs as needed.
- [x] Delete unfinished self-service USDC funding routes: `GET /v1/me/usdc-funding-options` and `POST /v1/me/usdc-funding-sessions`.
- [x] Delete saved Solana wallet self-service routes: `GET /v1/me/wallets/solana`, `PUT /v1/me/wallets/solana`, and `DELETE /v1/me/wallets/solana`; do not confuse billing payment state with AuthKit wallet-as-auth-factor identity.
- [x] Delete the saved-wallet storage surface: `openrails.linked_wallets`, `internal/db/models/linked_wallet.go`, `internal/db/repo/linked_wallet.go`, `internal/db/queries/linked_wallets.sql`, generated sqlc code, RLS/docs/tests that only exist for linked wallets.
- [x] Ensure Solana payment receipts persist where the money came from. The chain signature remains `payments.transaction_id`; do not duplicate it in metadata.
- [x] Harden Solana one-off receipt metadata. For transaction-request checkout, copy the bound `processor_state.payer` into `payments.metadata.solana_payer_wallet`; keep `solana_reference`, `checkout_session_id`, token symbol/mint/base-unit amount, recipient wallet, and quote timestamp/FX metadata when present.
- [x] Harden Solana transfer-request receipt metadata. Plain transfer-request receipts keep signature/reference/recipient/token amount but no payer wallet because the poller does not currently derive fee-payer/source wallet from chain transaction details; transaction-request checkout is the audited wallet-provenance path.
- [x] Harden Solana recurring payment metadata. Initial subscribe payment and renewal/crank payments include `solana_subscriber_wallet`, `solana_subscription_pda`, `solana_token_mint`, `solana_token_amount`, and `solana_recipient_wallet`; authority/plan PDA stay on `solana_subscriptions`.
- [x] Audit customer notification inbox routes (`GET /v1/me/notifications`, `GET /v1/me/notifications/unread-count`, `POST /v1/me/notifications/:id/read`): Doujins consumes them through `billingApiService`, so they remain mounted for now.
- [x] Delete now-unused USDC funding handlers/tests/docs after the route cut.
- [x] Keep service/admin account-settings routes capable of setting operator-owned policy fields where still needed.
- [x] Update docs and tests for micro-USD examples, especially any `$25.00 == 25_000_000` style values.
- [x] Audit downstream consumers for `auto_topup_amount_cents` and account-settings fields before removing any wire name.
- [x] Run focused account-settings tests.

## Acceptance

- No customer-facing OpenRails docs imply USD cents for money-account settings.
- USDC and SOL are not accepted or documented as customer money-account currencies.
- The customer settings API does not expose `billing_mode`, arrears/owed caps, credit-line, tier, suspension, or verification knobs.
- The customer settings API does not expose credit-expiry or enforcement-policy knobs.
- `auto_topup_payment_method_id` cannot point at another customer's saved payment method and is cleared safely when that payment method is deleted.
- The customer self-service `/v1/me/account*` and `/v1/me/credits*` route families are gone; balance/history/settings use `/v1/me/balance`, `/v1/me/transactions`, and `/v1/me/settings`.
- The unfinished self-service USDC funding route family is gone.
- The saved Solana wallet route/table/repo/model family is gone; Solana payment receipts record wallet/source metadata in `payments.metadata` while `payments.transaction_id` remains the provider id / chain signature, and subscriptions carry their own `subscriber_wallet`.
- Customer notification inbox routes remain because Doujins is a confirmed downstream consumer; internal notification/email workflows remain.
- USD examples use micro-USD.
- `currency` is documented as real ledger currency, not preferred display currency.

---

# #548: simplify-self-account-balance-contract

**Completed:** yes
**Status:** COMPLETE 2026-06-20: customer `/v1/me/account` now returns only the durable per-currency balance contract; focused self-account tests pass.

`GET /v1/me/account?currency=usd` currently returns `balance_amount`, `held_amount`, `available_amount`, `outstanding_owed_amount`, `billing_mode`, and settings. That suggests the endpoint is doing a live in-flight hold lookup, but it is not: request/admission holds live in Redis spendgate, while this endpoint reads the durable money ledger. Since customer balance display should be per-currency and simple, do not add a Redis lookup here. Drop `held_amount` and `available_amount` from the customer account response and return the durable balance for the requested currency.

## Metadata

- Category: cleanup
- Status: complete
- Passes: true

## Desired Behavior

- `GET /v1/me/account?currency=usd` returns one customer account for one currency.
- The response exposes the durable per-currency balance as the balance.
- The response does not expose `held_amount` or `available_amount`.
- Live in-flight spendgate holds remain internal admit-path state, not part of the customer account balance contract.
- Billing mode remains the account policy value (`prepaid` or `arrears`) unless the implementation shows it is unused by consumers.

**Tasks:**
- [x] Update `internal/http/handlers/self_account.go` response shape to remove `held_amount` and `available_amount` from `GET /v1/me/account`.
- [x] Keep balance lookup per-currency via the required `currency` query parameter.
- [x] Do not add a Redis spendgate held lookup to this route.
- [x] Update docs/tests that assert the old customer account response shape.
- [x] Audit downstream OpenRails self-account consumers for `held_amount` / `available_amount` usage and update them to use `balance_amount`.
- [x] Run the smallest focused test set covering `/v1/me/account`.

## Acceptance

- `/v1/me/account` no longer returns `held_amount` or `available_amount`.
- Customer-facing docs describe per-currency balance, not live held/available capacity.
- Existing service/admit internals may still track Redis held amounts where needed, but that state is not leaked through the customer account endpoint.

---

# #547: remove-dev-only-nmi-tokenization-debug-surface

**Completed:** yes
**Status:** COMPLETE 2026-06-20: OpenRails' dev-only NMI tokenization pages are removed; docs/scripts now generate a per-run NMI Collect.js `payment_token` in Playwright, ensure the stable `openrails_e2e_nmi_daily_999` sandbox recurring plan exists, and pass only that opaque token into OpenRails production APIs. `task e2e-nmi-live` passes against the Docker Compose stack with sandbox `NMI_SANDBOX_SECURITY_KEY`, `NMI_TOKENIZATION_KEY`, and `NMI_WEBHOOK_SIGNING_SECRET`: Collect.js tokenization, saved-card vaulting, one-off checkout, subscription checkout, NMI Query API verification, signed webhook ingestion/idempotent replay, activation, and cancellation.

OpenRails currently serves a dev-only browser harness at `/debug/nmi/tokenization` plus a local fake Collect.js at `/debug/nmi/collect-stub.js`. That was useful while wiring NMI, but it is the wrong long-term test surface: production hosts run the card form, load processor tokenization JS in the browser, receive an opaque `payment_token`, and call OpenRails production endpoints such as `POST /v1/me/payment-methods` or checkout. Dev/test should exercise that path, not OpenRails-owned HTML that only exists in dev mode.

## Metadata

- Category: cleanup
- Status: completed
- Passes: true

## Touched References

- Deleted OpenRails routes/handlers/tests: `internal/http/routes_debug.go`, `internal/http/debug_nmi_tokenization.go`, `internal/http/debug_nmi_tokenization_test.go`.
- Removed OpenRails security/path special cases from `internal/http/middleware/neutral_paths.go`, `internal/http/middleware/ginmw/security.go`, and `internal/http/middleware/ginmw/security_test.go`.
- Updated OpenRails docs/scripts: deleted `docs/nmi-tokenization-harness.md`; updated `docs/e2e-mobius-sandbox.md`, `scripts/mobius_sandbox_e2e.sh`, and `scripts/mobius_live_lifecycle_e2e.sh`.
- Doujins/Hentai0 exact `/debug/nmi/*` references were not found; both downstream apps already have browser-side payment/tokenization surfaces.

## Desired Behavior

- OpenRails never serves tokenization HTML or a Collect.js stub, even in dev mode.
- Browser tokenization lives outside OpenRails, because OpenRails should never serve card-entry HTML or receive raw PAN/CVV.
- OpenRails only receives opaque per-run `payment_token` values and continues to validate/store them through the real payment-method and checkout endpoints.
- NMI sandbox/live E2E drives browser-side Collect.js tokenization, then verifies OpenRails state through production HTTP routes.

**Tasks:**
- [x] Inventory all OpenRails references to `/debug/nmi/*`, tokenization harness docs, and NMI scripts before deleting anything.
- [x] Audit Doujins and Hentai0 for tokenization/payment E2E paths and confirm they do not depend on OpenRails-only dev pages; add tracker tasks there if any dependency is found. No `/debug/nmi/*` references found; both apps already own Collect.js/payment-origin flows.
- [x] Identify browser-side tokenization surfaces outside OpenRails. Doujins/Hentai0 have `/external-payment?flow=save-card` and embedded payment UI; the OpenRails E2E uses an ephemeral Playwright page that loads NMI Collect.js directly.
- [x] Update `scripts/mobius_live_lifecycle_e2e.sh` to use browser-side NMI Collect.js tokenization instead of `GET /debug/nmi/tokenization`. Implemented as Playwright-generated per-run `payment_token`, then production `POST /v1/me/payment-methods`.
- [x] Update `scripts/mobius_sandbox_e2e.sh` and NMI docs to point at the production-path host flow, not an OpenRails debug page.
- [x] Delete the OpenRails debug NMI route/handler/stub code and its direct tests.
- [x] Remove `/debug/nmi/tokenization` middleware special cases and update the security tests accordingly.
- [x] Verify `rg "debug/nmi|collect-stub|nmi-tokenization-harness|TOKENIZATION_BASE_URL"` has no live-code/docs hits outside historical completed tracker entries.
- [x] Run focused OpenRails tests: `go test ./internal/http ./internal/http/middleware/ginmw`.
- [x] Run the replacement NMI E2E against a live HTTP server. Passed with `task e2e-nmi-live` against the Docker Compose stack on 2026-06-20.

## Acceptance

- `/debug/nmi/tokenization` and `/debug/nmi/collect-stub.js` do not exist in OpenRails route registration.
- No security middleware or neutral-path logic special-cases the deleted debug page.
- OpenRails docs/scripts describe browser-side tokenization and OpenRails-owned payment-method/checkout APIs as separate responsibilities.
- Doujins/Hentai0 E2E coverage, where present, exercises the same tokenization code path production users hit.
- No raw PAN/CVV is ever posted to OpenRails.

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


# #541: Embedded merchants should require a backing org (delegation hook)

**Completed:** no

**Status:** PLAN — raised 2026-06-20 in the permission-model discussion. Not yet designed; no code changes made.

**Requires code change:** yes.

## Problem
Today an embedded merchant does NOT require an org. `embed.New` provisions the bound merchant as a "dumb billing bucket" via `boot.ProvisionMerchant` and explicitly omits the issuer/owner-org step — "Embedded OpenRails runs NO AuthKit here and stores ZERO auth: the issuer/owner-org step is omitted" (embed/embed.go:130-150). `openrails.merchants.owner_org_id` is NULLABLE and is NULL in embedded (migrations/postgres/001_schema.up.sql:100; 016_bootstrap_state_owner_org_unique.up.sql:15-24). So a merchant can exist with no backing org, and therefore no place to hang delegated merchant-admin authority.

## Why it should change
The org is the unit that lets merchant-admin authority be delegated to multiple users (e.g. grant an admin the power to cancel a customer's subscription). A merchant with no backing org can only ever be administered by the single statically-bound host principal, with nowhere to attach per-user grants. Every merchant should have exactly one backing org — the same 1:1 invariant standalone already enforces (UNIQUE index on non-null `owner_org_id`, #527 / migration 016).

## Central open question (decide before implementing)
Embedded OpenRails runs NO AuthKit, so the backing org physically lives in the HOST's AuthKit (e.g. doujins'), not OpenRails'. NOTE (verified 2026-06-20): `owner_org_id` is a plain `text` column with NO foreign-key constraint, so it can carry a host-org id in embedded just as it carries the OpenRails-AuthKit org id in standalone — this makes it the natural cross-mode bridge column (#544) and strongly favors option (a). Options to choose between:
- (a) Require the embed host to declare a backing-org identifier per merchant, stored as an opaque host-org id in `owner_org_id` (no FK), enforced at `embed.New` / `ProvisionMerchant`. Keeps the core schema identical across modes.
- (b) Treat the invariant as host-upheld and require the merchant↔org binding only to be expressible through the `DelegatedAuthenticator` principal seam.
- (c) Other.

## Affected code (for the implementer)
- embed/embed.go (the `ProvisionMerchant` call; the omitted owner-org step)
- pkg/merchant + `boot.ProvisionMerchant` / merchant manifest
- `openrails.merchants.owner_org_id` semantics (nullable today)

**FINAL 2026-06-20 (per Paul — "org == merchant by name"):** The model is **merchant slug == backing-org slug, 1:1**, in whatever AuthKit owns identity. Standalone ALREADY does exactly this — `provisionMerchantOrg` creates an AuthKit org with `Slug: merchant.Slug` and links via `owner_org_id`. Embedded runs no AuthKit, so the backing org is the HOST's same-slug org (e.g. doujins's `doujins` org) — linked by the shared slug, nothing for OpenRails to create or store (`owner_org_id` stays NULL in embedded). So the earlier `BackingOrgID` mechanism was unnecessary and was REVERTED. No enforcement needed (the link is the name). Builds + vets; verified live (`./internal/bootstrap` integration suite + the #544 round-trip both pass on docker Postgres).

## Tasks
- [x] Model = slug==slug, 1:1 (org slug == merchant slug). Standalone creates the org with the merchant (`provisionMerchantOrg`, `Slug: mt.Slug`); embedded relies on the host's same-slug org.
- [x] Reverted the `BackingOrgID`/`ManifestMerchant.OwnerOrgID` detour — unnecessary under slug==slug; embedded provisioning is the plain ownerless `RegisterMerchant` (owner_org_id NULL; host owns the org).
- [x] embed.go doc comment updated to the slug==slug model.
- [x] Verified live: `./internal/bootstrap` passes; round-trip showed an embedded-origin merchant (owner NULL) get its org rebuilt by `auth recreate`.
- [x] Docs: invariant recorded in docs/embedded-standalone-mode-switch.md.
- [x] Reuses the existing `owner_org_id text` column — no embedded-only column or FK (#544).

Related: #542 (catalog ownership), #543 (stop seeding the operator role), #544 (cross-mode data move — `owner_org_id` is the bridge).

---


# #542: Permission catalog is owned by OpenRails, never by the host app

**Completed:** no

**Status:** DONE 2026-06-20. Audited: no doc/comment wrongly attributes the catalog to the host (the "catalog" mentions in docs are the product/price catalog, not the permission catalog). Behavior already correct; invariant recorded in docs/embedded-standalone-mode-switch.md.

**Requires code change:** no (doc/comment audit only; behavior is already correct).

**Completed:** yes.

## The invariant
The OpenRails permission catalog lives in OpenRails (internal/controlplane/catalog.go: `catalogEntries` + `merchantCatalog`). A host app (e.g. doujins) does NOT have a table holding OpenRails' catalog. In the federated delegated-token model (#259) the host merely SIGNS a token asserting permission strings; OpenRails validates each asserted permission against its OWN catalog and rejects anything outside it (delegated.go:135-140 `WithPermissions` → `IsDelegatedPermission`; ginmw/delegated.go:201; ginmw/principal.go:198). The host needs no copy of the catalog.

## Why this matters
This is the principle that motivates #543: because OpenRails owns the catalog and validates against it, OpenRails should NOT be pushing that catalog into an AuthKit org as a seeded role. Catalog = OpenRails. Identity / role-assignment = wherever the AuthKit lives (the host in embedded, OpenRails in standalone).

## Action
- Audit docs/comments for any claim that the host stores or holds OpenRails' catalog and correct them. No behavioral code change.

## Tasks
- [x] Grep docs/ + code comments for wrong "host holds the catalog" claims — none found (catalog mentions = product/price catalog).
- [x] (n/a — nothing to correct.)
- [x] Record the invariant — docs/embedded-standalone-mode-switch.md ("permission catalog is owned by OpenRails, never the host").
- [x] Sanity-check no code path expects the host to hold the catalog — confirmed: delegated verify validates against the local catalog (`IsDelegatedPermission`).

Related: #543 (stop seeding the operator role), #541 (embedded backing org).

---


# #543: Stop OpenRails seeding the `openrails-operator` role into an AuthKit org

**Completed:** no

**Status:** PLAN — raised 2026-06-20. Not yet designed; no code changes made.

**Requires code change:** yes.

## Problem
Standalone bootstrap pushes OpenRails' entire permission catalog into the merchant's AuthKit org as a seeded role: `DefineRole(OperatorRole)` + `SetRolePermissions(OperatorRole, OperatorRolePermissions())` + `AssignRole` (internal/controlplane/bootstrap.go:108-127); the role is also registered in service.go:156. This is the AuthKit-holds-the-authority model and conflicts with #542 (OpenRails owns the catalog). (Confirmed standalone-only — the embedded path runs no AuthKit and never seeds this.)

## Desired end state
OpenRails does not define/seed an `openrails-operator` role inside AuthKit orgs. OpenRails owns the catalog and validates against it; authority is conferred by tokens/keys carrying catalog permissions, not by an OpenRails-managed AuthKit role.

## Open question (decide before implementing)
If the operator role goes away, how do standalone admins get authority? Note bootstrap already mints the initial admin API key scoped DIRECTLY to `OperatorRolePermissions()` (bootstrap.go:148-151) and `mint-merchant-api-key` scopes keys directly too (cmd/openrails/mint_merchant_api_key.go:67) — i.e. direct-scoped keys may already make the seeded role redundant. Confirm, then remove the role seeding while keeping `OperatorRolePermissions()` as the catalog-permission-set helper used to scope keys directly.

## Affected code (for the implementer)
- internal/controlplane/bootstrap.go (remove steps 2 & 3: `DefineRole`/`SetRolePermissions`/`AssignRole`; keep the direct-scoped key mint)
- internal/controlplane/service.go:156 (role registration)
- internal/controlplane/catalog.go (`OperatorRole` const; `OperatorRolePermissions` — likely keep the perms helper, drop the role identity if unused)
- internal/integrationharness/harness.go (test setup uses `DefineRole`/`SetRolePermissions` for `OperatorRole`)

**Status update 2026-06-20:** DONE in production code, builds + vets clean on authkit v0.42.0. Per Paul's constraint "OpenRails must not define/invent any new roles in orgs", OpenRails now seeds NO role: the bootstrap admin is assigned AuthKit's built-in `owner` role (`org:*`, seeded by CreateOrg, covers the whole OpenRails `org:` catalog); the admin API key stays direct permission-scoped. Verified `AssignRole(…, "owner")` does not clobber the seeded `org:*` (authkit `materializeDefaultRole` early-returns for the reserved owner role).

## Tasks
- [x] Confirm direct-scoped API keys cover server-to-server admin authority without the seeded role; human admin authority = org `owner` (`org:*`).
- [x] Remove the role-seeding steps from bootstrap.go (`DefineRole`+`SetRolePermissions`+`AssignRole(OperatorRole)`); admin now `AssignRole(OwnerRole)`; direct-scoped key mint kept.
- [x] Remove the `OperatorRole` `DefaultRole` registration in service.go (`DefaultRoles: nil`).
- [x] Fate of `OperatorRole`/`OperatorRolePermissions()`: added `OwnerRole = "owner"` (built-in); kept `OperatorRolePermissions()` as the catalog perm-SET for direct key scoping; `OperatorRole` const retained for test fixtures only, docs reframed.
- [x] Update bootstrap + catalog.go package/header doc comments (no longer claim OpenRails defines roles).
- [x] Updated internal/integrationharness/harness.go to stop seeding `OperatorRole`; the test principal authenticates via its direct permission-scoped API key, and remote-app tests assign the built-in `owner` role (embed/{remote_application,merchant_control_boundary}_integration_test.go).
- [x] Tests VERIFIED LIVE (docker Postgres): full `./internal/controlplane` integration suite + `./embed` pass with the owner-grant model. Updated bootstrap_integration_test.go to assert the `owner` role's `org:*` effectively covers the catalog.

Related: #542 (catalog ownership), #541 (embedded backing org).

---


# #544: Embedded ↔ standalone mode must be a trivial data move — identical core schema

**Completed:** no

**Status:** PLAN / INVARIANT — raised 2026-06-20. Architectural requirement. The schema already mostly conforms; needs the tables enumerated, tooling built, and a regression guard.

**Requires code change:** yes (tooling + tests + CI guard; the core schema is already conformant).

## Requirement
Any application must be able to switch OpenRails between **embedded** and **standalone (remote)** modes trivially, WITHOUT altering data or changing the application:
1. Export embedded OpenRails data → load into a remote DB → now standalone.
2. Dump standalone data → load into embedded → now embedded.

The ONLY difference between modes is that standalone has a few EXTRA tables because it runs its own auth (AuthKit). Therefore:
- standalone → embedded: the auth tables are thrown away.
- embedded → standalone: the auth data must be RECREATED (an org per merchant, roles/grants per #543's replacement model, `owner_org_id` backfill).

This is the reason the OpenRails core data model MUST stay identical across modes.

## Current state (verified 2026-06-20)
- GOOD: the `openrails` core schema is self-contained. `openrails.merchants.owner_org_id` is a plain `text` column with NO foreign-key constraint to any auth/org table (001_schema.up.sql) — so core DDL never references auth tables and is identical in both modes.
- `owner_org_id` is the natural BRIDGE column: opaque text, NULL in embedded today, the AuthKit org id in standalone. It can carry a backing-org id in BOTH modes with no schema change (ties into #541).
- AuthKit tables live in a SEPARATE Postgres schema (`profiles`), distinct from the `openrails` core schema. CAVEAT (verified 2026-06-20): embedded currently runs the `profiles` migrations too — `embed.New` → `migrate.RunPostgres` applies AuthKit + River + OpenRails in one pass — so today embedded HAS the auth tables but leaves them EMPTY. Making "standalone has extra tables" literally true (your model) requires splitting the migrator (task below).
- The migration ledger ALREADY lives in `public.migrations` (NOT the `openrails` schema). River `river_*` tables are the ONLY non-portable resident of the `openrails` schema today — and they are being moved to `public` in #545, after which `pg_dump --schema=openrails` IS exactly the portable export (no `--exclude-table` needed).

## (1) Data-model & migration changes needed
The core model barely changes — it's already self-contained. The real work is making the table partition explicit and the embedded/standalone DDL difference real:
- **Three table classes, made explicit:** (a) PORTABLE core billing = the whole `openrails` schema (once River leaves it, #545); (b) STANDALONE-ONLY auth = the `profiles` (AuthKit) schema; (c) RUNTIME/infra in `public` = `river_*` (→ `public` per #545) + the migration ledger (`public.migrations`, already there) — never exported; the target rebuilds them.
- **Split the migrator** so EMBEDDED runs only core (+River) and SKIPS the `profiles`/AuthKit migrations — then "standalone has extra tables" is literally true. (Alternative: keep empty `profiles` tables in embedded — simpler, but muddies the model and ships dead tables. Recommend the split.)
- **Keep `owner_org_id` the SOLE cross-mode bridge** (opaque `text`, no FK; see #541). Audit every other core table for any coupling to `profiles`.
- No new columns/FKs that would diverge the embedded vs standalone core DDL.

## (2) CLI commands (cobra; siblings of `migrate` / `mint-merchant-api-key` / `pull-provider`)
- `openrails data export --out <file> [--merchant <slug>]` — dump the PORTABLE core billing data. Once River is in `public` (#545) this is a clean `pg_dump --schema=openrails` (the ledger is already in `public`, so no `--exclude-table` is needed). Mode-agnostic: identical output whether the source is embedded or standalone. A logical per-merchant JSON export is a later option for cross-PG-version portability and selective single-merchant moves.
- `openrails data import --in <file>` — load a core export into a target DB that has already had `openrails migrate up` run (so the schema exists).
- `openrails auth recreate` (or fold into `bootstrap apply`) — the embedded→standalone-ONLY step: for each imported merchant, create the backing org, backfill `owner_org_id`, seed authority per #543 (direct-scoped admin API key, not a role), and register the issuer(s) (`remote_applications`) needed to verify tokens. Idempotent.
- standalone→embedded needs NO special command: `data export` then `data import` into the embedded DB; with the migrator split the `profiles` schema is never created, so auth data is dropped by construction (the host owns auth in embedded).

## Tasks
- [x] Enumerate the three table classes — recorded in docs/embedded-standalone-mode-switch.md (PORTABLE core / STANDALONE-only `profiles` / RUNTIME `river_*`+ledger in `public`).
- [x] Split `migrate.RunPostgres` so embedded skips the `profiles`/AuthKit step — added `RunPostgresEmbedded` (vs `RunPostgres`); `embed.New` now calls it. Builds.
- [x] Confirm NO core `openrails` table has a NOT NULL/FK dependency on `profiles` — verified (zero `REFERENCES profiles/authkit` across migrations/postgres/*.sql; `owner_org_id` is bare `text`, no FK).
- [x] Implement `openrails data export` (cmd/openrails/data.go; pg_dump `--schema=<openrails>` whole-schema dump — `river_*`/ledger now in `public` so no per-table exclusions needed; `--data-only`/`--format` flags). Builds + `data --help` works. Per-merchant `--merchant` deferred to the planned logical-JSON export.
- [x] Implement `openrails data import` (same file; pg_restore custom / psql plain; `--data-only`/`--clean`). NOTE: unverified end-to-end pending the round-trip test below (no Postgres in this env).
- [x] Implement `openrails auth recreate` (cmd/openrails/auth.go; lists merchants, runs idempotent control-plane Bootstrap per slug → creates backing org + rewrites `owner_org_id` to the new standalone org + optional `--mint-keys`). Builds + `auth --help` works. (Issuer re-registration left to the existing merchant-config manifest path; noted.)
- [x] Document both procedures (embedded→standalone, standalone→embedded) — docs/embedded-standalone-mode-switch.md.
- [x] Round-trip VERIFIED LIVE (docker Postgres, 2026-06-20): seed merchant → `data export --data-only` → fresh DB `migrate pg` → `data import --data-only` (merchant survived, owner=NULL) → `auth recreate` (created backing org, set `owner_org_id`, `profiles.orgs` row present). Live testing caught a circular-FK (`grants`) issue → fixed with `--disable-triggers` on data-only restore (cmd/openrails/data.go).
- [x] CI guard: `migrations/postgres/portability_guard_test.go` (`TestPortabilityInvariant`) FAILS if a migration adds a FK into `profiles`/`authkit` or creates a `river_*` table in an OpenRails migration. Passes on current migrations.

Related: #541 (embedded backing org — `owner_org_id` bridge; prerequisite for a clean embedded→standalone), #543 (the authority model that `auth recreate` seeds), #545/#546 (River → `public`, out of the billing schema, which makes the export a clean whole-schema dump).

---


# #545: River job-queue tables always live in the `public` schema

**Completed:** no

**Status:** PLAN — raised 2026-06-20. DECIDED: River `river_*` tables always in `public` in every mode (River's own documented default — riverqueue.com/docs/alternate-schema). Not yet implemented.

**Requires code change:** yes.

## Decision
River's `river_*` tables must ALWAYS live in the `public` Postgres schema, in every mode. This is River's documented default, sits alongside `public.migrations` (the migratekit ledger) and the `pgcrypto` extension that already live in `public`, and keeps the `openrails` (billing) schema free of any non-portable runtime state. Net effect: the `openrails` schema becomes 100% portable billing data, so #544's `data export` collapses to a clean `pg_dump --schema=openrails` with no `--exclude-table`.

## Current state (verified 2026-06-20)
- Standalone: OpenRails runs River in the OpenRails billing schema (`db.schema`, default `billing`/`openrails`) — `runRiverMigrations(ctx, cfg, schema)` (migrator.go:70) + `standaloneRiverSchema()` returns the billing schema; `riverCfg.Schema = schema`, set only when schema != `public` (migrator.go:233-234). `pkg/embedded/river.go` documents "standalone River schema == OpenRails schema, NOT separately configurable".
- Embedded-own (no host client injected): same — builds its own River in the billing schema.
- The migratekit ledger is ALREADY in `public.migrations` (migrator.go:144), so River is the ONLY remaining non-portable resident of the billing schema.

**Status update 2026-06-20:** code-complete + builds clean on authkit v0.42.0. Added `config.RiverSchema = "public"` (single source of truth); `standaloneRiverSchema()` and the River migration call both use it. Remaining: cutover doc + tests + riverui check.

## Tasks
- [x] Point standalone River migrations at `public` (migrator.go now passes `config.RiverSchema`).
- [x] Point the standalone runtime River client at `public` (`standaloneRiverSchema` → `config.RiverSchema`; used by both `buildRiverProducer` clients).
- [x] Point the embedded-own River path (no host client injected, #546) at `public` too (same `standaloneRiverSchema`).
- [x] Update `pkg/embedded/river.go` docs to "River always uses `public`".
- [x] One-time cutover: N/A — greenfield hard cut, no legacy/production data (per Paul). River is in `public` from day one; nothing to drain/drop.
- [ ] Check riverui / any ops tooling that assumes the River schema.
- [x] VERIFIED LIVE (docker Postgres): `migrate pg` logged "River migrations (schema \"public\")" and the DB showed 6 `river_*` tables in `public`, **0** in the `openrails` schema. `./embed` integration suite (embedded-own River) passes.

Related: #546 (embedded shares host River or runs its own — always `public`), #544 (export collapses to `pg_dump --schema=openrails` once River leaves the billing schema).

---


# #546: Embedded OpenRails shares the host's River instance, or runs its own — always in `public`

**Completed:** no

**Status:** PLAN — raised 2026-06-20. The injection capability (`SetRiverClient`) exists; the share-or-fallback behavior + doujins wiring are not done.

**Requires code change:** yes (OpenRails + doujins).

## Decision
In EMBEDDED mode, OpenRails uses the HOST application's River instance when the host runs River, and runs its OWN River only if the host does not. Either way River tables live in `public` (#545), so host and OpenRails share a single `public.river_*` set. In STANDALONE mode OpenRails always runs its own River (in `public`).

## Current state (verified 2026-06-20)
- The injection capability exists: `pkg/embedded.SetRiverClient` / `HasExternalRiverClient` — when a host client is injected, OpenRails "NEVER constructs a River client" (pkg/embedded/river.go:13-19).
- BUT doujins does NOT inject: `internal/billing/openrailsembed/openrailsembed.go` passes only a `PGXPool` + `RunWorkers: true`, so embedded OpenRails builds its OWN River. Combined with doujins keeping its River in the `doujins` schema (`doujins.river_job`), doujins today runs TWO River table sets (`doujins.river_job` + `openrails.river_job`) — the `restore.go` "doujins.ln vs openrails.ln" artifact.

## Tasks
- [x] OpenRails embedded: "use injected host River client if present (`HasExternalRiverClient`), else run own in `public`" — capability already present (`SetExternalRiverClient`); fallback now lands in `public` (#545); `SetRiverClient` doc updated to the public rule.
- [x] Ensure the embedded-own River path lands in `public` (via `standaloneRiverSchema` → `config.RiverSchema`).
- [x] doujins: moved its River `doujins`→`public` (workers/client.go, migrate.go, legacy_migrate/coordinator.go, command/*.go SQL refs) and now SHARES one client with embedded OpenRails (openrailsembed `RunWorkers:false` + `RegisterRiverWorkers`/`RegisterRiverClient`; DI carries the registry; River start deferred to after `SetupRoutes`). Builds/vet/gofmt clean, unit tests pass. Done 2026-06-20 (against published openrails v0.45.0, which already has `SetRiverClient` — decoupled from the authkit-v0.42 chain). NOT committed.
- [x] Embedded OpenRails River path VERIFIED LIVE: `./embed` integration suite passes on docker Postgres (boots embedded OpenRails via `embed.New` → `RunPostgresEmbedded`, exercises the embedded-own River in `public`). Cutover: N/A — greenfield hard cut, no legacy data (per Paul).
- [x] `./embed` integration tests pass (embedded-own River in `public`). The host-injected-client path is the doujins side (agent-done); doujins is greenfield too, so no cutover.

Related: #545 (River always in `public`), #544 (clean export once River leaves the billing schema).

---


# #550: `auth recreate` registers the host issuer (one-command federated embedded→standalone)

**Completed:** no

**Status:** PLAN — 2026-06-20. Decided (Paul): one step is simpler. Builds on #544's `auth recreate`.

**Requires code change:** yes (OpenRails).

## Problem
`auth recreate` (cmd/openrails/auth.go) creates each merchant's backing org + owner + optional admin key, but does NOT register the host's issuer / remote-application. For a FEDERATED standalone (e.g. tensorhub as issuer-owner), that linkage lives in AuthKit, not in the billing export, so it can't be reconstructed from the dump — today it's a separate manifest step. Fold it in so federated embedded→standalone is ONE command.

**Status update 2026-06-20:** DONE + VERIFIED LIVE. `auth recreate --issuers <merchant-config.yaml>` reuses the tested merchant-config reconcile (`ReconcileMerchantManifestData` → `provisionMerchantOrg`); merchants without a manifest issuer fall back to `cp.Bootstrap` (org+owner+key). Builds + vets.

## Tasks
- [x] Added `--issuers <manifest.yaml>` to `auth recreate` (cmd/openrails/auth.go).
- [x] Per manifest merchant: backing org + issuer registered as org `owner` remote-application — via `ReconcileMerchantManifestData`/`provisionMerchantOrg` (no duplicated logic).
- [x] Idempotent (Insert+Overwrite); merchants not in the manifest get `cp.Bootstrap` (org+owner+key).
- [x] VERIFIED LIVE (docker Postgres): `auth recreate --issuers` on a fresh standalone DB → merchant `monkey` (owner_org_id set) + backing org `monkey` (same id, slug==slug) + remote-app `monkey-app` for `https://monkey.example` assigned **role=owner**.
- [x] Doc: docs/embedded-standalone-mode-switch.md updated to the `--issuers` one-command federated flow.

Related: #544 (data move), #259/#527 (federated issuer-as-owner), #551 (slug==slug).

---


# #551: Enforce merchant.slug == backing-org.slug (hard invariant)

**Completed:** no

**Status:** PLAN — 2026-06-20. Decided (Paul): a merchant's slug MUST equal its backing-org slug; any divergence invites confusion and must be rejected. `owner_org_id` stays the authority link, but the linked org's slug MUST match.

**Requires code change:** yes (OpenRails).

## Problem
slug==slug holds by construction in standalone today (`provisionMerchantOrg` creates the org with the merchant slug), but nothing REJECTS a divergence, and embedded relies on an unenforced host convention. Make it a hard boundary where it is code-enforceable.

## Tasks
- [x] Standalone hard guard: `provisionMerchantOrg` now asserts `res.Org.Slug == merchant.slug` and REJECTs a mismatch; `merchants.Service.Provision` validates the slug. (Manifest can't point at a divergent org — `ManifestMerchant` has no separate org field; the backing org is derived from the merchant slug.)
- [x] Align merchant-slug validation with AuthKit's org-slug ruleset: added `merchant.NormalizeSlug` + `merchant.ValidateSlug` (mirrors `authkit/core validateOrgSlug` regex `^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`), enforced at `db.RegisterMerchant` (embedded) + `merchants.Service.Provision` (standalone). Unit-tested (`TestValidateSlug`, passes).
- [x] Embedded: single-slug shape kept (BackingOrgID already reverted); `db.RegisterMerchant` now validates the slug so embedded merchants stay standalone-compatible; no cross-AuthKit check (keeps #541/#543 decoupling).
- [x] Tests: `TestValidateSlug` (pkg/merchant) passes; `./internal/controlplane` + `./internal/bootstrap` integration suites pass LIVE against docker Postgres (exercise provisioning with the new slug validation + the `provisionMerchantOrg` `org.slug == merchant.slug` assert — valid path unaffected). The federated `auth recreate` run (#547) further confirmed `owner_org_id` links to the same-slug org.

Related: #541 (`owner_org_id` bridge), #543 (no OpenRails-seeded roles), #544 (mode switch), #550.
