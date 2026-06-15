<!-- openrails issue tracker — PLANNED/future issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


---

# #479: OpenMeter-parity ideas — first-class configurable METERS + priority-based grant burn-down

**Completed:** no — future direction (not blocking). Surfaced comparing OpenRails to OpenMeter
(openmeterio/openmeter). OpenRails is already a superset (metering + entitlements + its own
billing/payments/dunning + multi-tenant control plane + admission/rate-limit/fairness-policy +
spend-graduated tiers), but two OpenMeter concepts are worth adopting:

1. **First-class configurable METERS.** Today usage is recorded as `usage_events` (#289) + dimensions
   and aggregated implicitly for billing; throughput units (#472) and budget windows (#475/#476) are
   semi-separate. OpenMeter models a **Meter** as a named, configurable aggregation —
   `agg(event.field) over window` (sum/count/unique/min/max), e.g. `sum(tokens)`, `count(requests)`,
   `unique(model)`. Make meters first-class and have limits/entitlements/tiers *reference a meter*
   instead of hardcoded unit strings (`token`/`image`/`request`). Unifies throughput + budget windows
   + usage analytics under one substrate; adding a metered dimension becomes config-only.

2. **Priority-based grant burn-down.** OpenMeter's prepaid credits consume grants in a defined
   PRIORITY order (which grant/credit to spend first) with reset/rollover. OpenRails' credit/grant
   model could formalize the same — relevant to #475 custom credits + entitlement grants (e.g. spend
   promotional/expiring grants before purchased balance).

NOT in scope for the rate-limit/tier system (which is done). Build when a real need appears. Reference:
OpenMeter (metering + entitlements; explicitly has NO request scheduler/fair-queuing — that stays the
orchestrator's job, confirming OpenRails' boundary: gate/meter/bill, not dispatch-reorder).

---

# #228: admin-analytics-and-billing-dashboard

**Completed:** no

Build a merchant/admin web dashboard for billing analytics, provider health, and safe support operations.

## Metadata

- Category: product
- Status: future
- Passes: false

## Goals

- Primary: analytics visibility (sales, churn, failures, revenue).
- Secondary: safe merchant/admin operations (support workflows) with audit trails.
- Make OpenRails' operational state visible without SQL or ad hoc CLIs: subscriptions, checkout sessions, payments, entitlements, credits, catalog/provider status, webhook failures, rebills, refunds/disputes, and processor-specific lifecycle actions.
- Give merchants confidence in non-Stripe rails by showing processor configuration, test/live mode, last successful checks, catalog provider status, and known limitations.

## Non-goals (initially)

- Customer-facing self-serve dashboard.
- Replacing core billing APIs.
- A marketing analytics site; this is an operational merchant console.

## Notes

- Keep the surface area minimal: read-heavy analytics and support visibility first, then a small set of high-confidence mutations.
- Every mutation must be gated + audited (who/what/when/why).
- Treat this as the productized surface for the backend roadmap items around processor capabilities, routing/fallback, provider certification, catalog-as-code status, dunning, and credits.

**Tasks:**
- FOUNDATION:
- [ ] Choose hosting path (serve via billing service, or separate static app + API calls)
- [ ] Define admin authz model (role/entitlement) and enforce on all dashboard APIs
- [ ] Add structured audit log table/events for admin actions (actor, action, target, before/after, request_id)
- 
- ANALYTICS (READ):
- [ ] Define core metrics: gross sales, net sales, refunds, chargebacks, MRR, active subs, churn, failed attempts
- [ ] Implement time-series queries (today, 7d, 30d) with timezone handling
- [ ] Add breakdowns: processor (NMI/CCBill/Solana), product/tier, country (if available)
- [ ] Add payments funnel: attempts -> succeeded -> refunded/chargeback -> net
- [ ] Add dunning visibility: past_due count, retries, recovered vs churned
- 
- ADMIN OPS (MUTATIONS):
- [ ] User lookup + detail view (subscriptions, checkout sessions, payments, entitlements, credits, payment methods, processor IDs)
- [ ] Add billing timeline per user/subscription that merges payments, subscription events, entitlement changes, credits, webhooks, and admin actions
- [ ] Support actions: cancel subscription, pause/resume, extend access, comp/refund, grant/revoke entitlement, grant/revoke credits, retry rebill, replay webhook, reconcile catalog item (only where safe)
- [ ] Add external processor links where available (Stripe objects, NMI transaction/plan identifiers, CCBill portal/admin references, Solana signatures/accounts)
- [ ] Guardrails: confirmation steps, rate limiting, and require a reason for every mutation
- [ ] Safe idempotency for all admin operations (avoid double-cancel/refund)
- 
- PROVIDER + CATALOG OPS:
- [ ] Provider health page: configured processors, sandbox/test/live mode, credential presence, last successful API call, supported capabilities, and known limitations
- [ ] Catalog/provider page: catalog-as-code apply status, provider object ids, NMI recurring plan ids, Stripe Product/Price ids, CCBill pending_manual_actions, Solana plan accounts, and open catalog drift events
- [ ] Routing policy page when #288 exists: show preferred processors, fallback order, disabled rails, and dry-run/explain selected processor for a price/user/tenant
- [ ] Certification matrix page or linked view when #290 exists: show last verified date/environment/command for provider flows without exposing secrets
- 
- API + DATA LAYER:
- [ ] Add dedicated admin endpoints (e.g. /v1/admin/analytics/*, /v1/admin/users/*)
- [ ] Add indexes/materialized views if needed for dashboard performance
- [ ] Add caching strategy for expensive aggregates (optional)
- 
- UI:
- [ ] Minimal dashboard pages: Overview, Payments, Subscriptions, Checkout Sessions, Dunning, Credits, User Lookup, Providers, Catalog
- [ ] Tables: filter/sort/paginate; export CSV for key views
- [ ] Detail drilldowns from charts -> filtered tables
- [ ] Compact support workflow: search -> user/subscription/payment detail -> safe action -> audit result
- 
- VERIFICATION:
- [ ] Unit tests for analytics queries (edge dates/timezones) and authz checks
- [ ] Integration tests for a small set of admin mutations + audit logging
- [ ] Manual checklist for production access controls and least privilege

---

# #134: stripe-meter-connector (optional; Stripe-hosted metered invoices)

**Completed:** no

OPTIONAL connector for the narrow case where a customer specifically wants STRIPE to host their metered billing + invoice, instead of OpenRails' native usage metering (#289). Native metering (#289) is the DEFAULT: it works across all rails (NMI/CCBill/Solana) and feeds the credit ledger. This issue only adds a thin one-way sync so a Stripe-native customer's usage_events are mirrored to Stripe's Billing Meter API and Stripe generates the invoice.

This is niche, NOT the main path. DROPPED from the original plan (do not build): the internal Stripe meter / meter_events mirror tables, the metered_subscriptions table, and any native invoice generation.

Mechanism: POST usage_events to Stripe /v1/billing/meter_events; let Stripe aggregate + invoice; handle invoice.paid / invoice.payment_failed webhooks for status only.
Refs: https://docs.stripe.com/api/billing/meter , https://docs.stripe.com/api/billing/meter-event

**Tasks:**
- [ ] Forward usage_events (#289) to Stripe via POST /v1/billing/meter_events (event_name + stripe_customer_id + value + idempotency identifier).
- [ ] Map OpenRails event_type -> Stripe meter event_name; document the one-time Stripe Meter + metered Price setup.
- [ ] River retry/queue for failed meter-event syncs.
- [ ] Handle invoice.paid / invoice.payment_failed webhooks (status only; Stripe owns the invoice in this mode).
- [ ] Tests: usage_event -> meter_event sync + idempotency.
- DROPPED (do not build): internal meter/meter_events mirror tables, metered_subscriptions table, native invoice generation.

---

# #230: auto-topup hardening (core shipped #239; caps + disable-on-failure + extras)

**Completed:** no

Auto-topup so a customer's prepaid credit never hits $0 and gets shut off: when balance drops below a floor, charge their card a fixed amount and deposit it.

## CORE ALREADY SHIPPED (#239) — the user's exact ask
On CreditAccountSettings: low_balance_threshold_cents (the floor, e.g. $100), auto_topup_amount_cents (the charge, e.g. $50), auto_topup_enabled, auto_topup_payment_method_id, last_topup_at (cooldown). RunAutoTopups (internal/modules/credits/money_in.go) scans below-threshold accounts and charges the card OFF-SESSION via Charger + deposits credits; AutoTopupWorker runs it every 15min (River); LowBalanceAlertWorker (#240) alerts. Configured via the admin settings endpoint (service_credits.go). Tests: TestRunAutoTopups_ChargesAndDeposits/_Declined.
=> 'keep $100, charge $50 when it dips below' = set threshold=10000, amount=5000, enabled=true, a payment method. Both values are user-configurable. NOTHING TO BUILD for the basic feature.
NOTE: implemented as a fixed-amount charge + cooldown on CreditAccountSettings, NOT the separate auto_topups/auto_topup_events tables or price_id-package model this issue originally sketched (superseded).

## REMAINING (optional deltas only)
- Hard safety caps (max topups per day/week/month) beyond the single cooldown.
- Disable-after-N-consecutive-failures + notify the user.
- Optional 'top up TO a target' mode vs the current fixed-amount charge.
- Lago wallet cherry-picks: GRANTED_CREDITS (paid + bonus in one topup, 'buy $50 get $5 free' — needs Deposit to carry granted vs paid), and INTERVAL (scheduled) top-ups distinct from the threshold trigger, with a pending-transaction guard.

**Tasks:**
- [x] Threshold trigger + fixed-amount off-session charge + deposit + cooldown (SHIPPED #239: RunAutoTopups, AutoTopupWorker).
- [x] Settings surface (threshold, amount, payment method, enabled) + admin endpoint (SHIPPED: CreditAccountSettings + service_credits.go).
- [x] Low-balance alerts (SHIPPED #240: LowBalanceAlertWorker).
- [x] Integration tests for charge+deposit and decline (SHIPPED: money_in_integration_test.go).
- REMAINING (optional deltas):
- [ ] Hard safety caps (max topups per day/week/month) beyond the single cooldown.
- [ ] Disable-after-N-consecutive-failures + notify the user.
- [ ] OPTIONAL 'top up TO target' mode (vs the current fixed-amount charge).
- [ ] GRANTED_CREDITS: paid + granted in one topup ('buy $50 get $5 free') — needs Deposit to carry granted vs paid.
- [ ] INTERVAL top-ups: scheduled (e.g. monthly) trigger distinct from the low-balance threshold, with a pending-transaction guard.

---

# #202: admin-credit-blocks (manual grant + revoke by block)

**Completed:** no

Add a minimal admin surface for managing user credits as immutable "blocks":

- Admin can **create** a credit block (grant credits, optional expiry)
- Admin can **delete** a credit block (revoke the remaining credits from that specific grant)

This intentionally avoids a full taxonomy (refund/admin_adjust/etc.). The goal is a small, safe API that maps to support workflows.

## Metadata

- Category: admin
- Status: future
- Passes: false

## Goals

- Minimal mutation surface: create block, delete block.
- Revocation is scoped to a specific block/grant, not "take N credits from the user".
- Strong consistency under concurrency (holds, captures, withdrawals, and block revocation).
- Clear auditability: who/what/when/why for every admin mutation.

## Non-goals (initially)

- Editing blocks (no partial edits or amount changes).
- Automatically interpreting payment refunds as credit revocations.
- Allowing negative balances (clawbacks) unless explicitly added later.

## Design sketch

### Data model

Today, revocation-by-block is hard because only expiring deposits create `billing.credit_expiry_batches`, and withdrawals consume FIFO by `expires_at`.

Introduce an explicit "block" record so we can revoke remaining credits precisely:

- New table: `billing.credit_blocks`
  - {id, user_id, credit_type_id, original_amount, remaining_amount, expires_at NULL, source, source_id UUID NULL, created_by, revoke_reason, revoked_at, created_at, updated_at}
  - Unique/indexes: (user_id, credit_type_id, expires_at), (user_id, credit_type_id, revoked_at IS NULL)

Consumption rule:
- On withdrawal/capture, decrement `credit_blocks.remaining_amount` in deterministic order:
  - Prefer earliest `expires_at` first, then oldest created.
  - Treat NULL expires as "never", ordered last.

Balances rule:
- Keep `billing.user_credit_balances` as the fast denormalized read.
- All mutations that affect blocks also update `balance` in the same DB transaction.

### APIs

Private admin/service routes (X-API-KEY):
- `POST /v1/admin/credit-blocks` create a block
- `DELETE /v1/admin/credit-blocks/:id` revoke remaining credits from that block
- (Optional but likely needed) `GET /v1/admin/credit-blocks?user_id=&type=` list blocks for a user

### Semantics for delete/revoke

- Default: revoke only the block's `remaining_amount` at time of deletion.
- If the block has been partially spent, deletion still succeeds and just removes what remains.
- Guardrail: do not allow revocation that would make `balance < held_balance`.
  - Return 409 with guidance to release/expire holds first.

## Integration points

- `internal/services/credits_service.go`:
  - Add a block-backed accounting path for deposits/withdrawals/captures.
  - Keep existing ledger writes to `billing.credit_transactions` for observability.
- `internal/river/jobs_credit_expiry.go`:
  - If blocks have expiries, expiry worker should zero `remaining_amount` and reduce balance (analogous to existing batches).
  - Or keep expiry as a direct block operation without `credit_expiry_batches`.

## Exit Criteria

- Admin can grant credits with optional expiry as blocks.
- Admin can delete a block and the system removes only the remaining credits from that grant.
- All operations are race-safe with holds/capture/withdrawal.
- Tests cover concurrency and edge cases (partial spend + revoke, holds present, expiry).

**Tasks:**
- DATA MODEL:
- [ ] Add migration for billing.credit_blocks table + indexes
- [ ] Decide how to map existing credit_expiry_batches (keep, migrate, or deprecate)
- 
- SERVICE METHODS:
- [ ] Add CreateCreditBlock(user_id, type, amount, expires_at?, reason, source?)
- [ ] Add RevokeCreditBlock(block_id, reason) that removes remaining_amount and marks revoked_at
- [ ] Update withdrawal/capture logic to consume from blocks deterministically (expiry first)
- [ ] Enforce guardrail: prevent balance dropping below held_balance on revoke/expiry
- 
- HTTP API:
- [ ] Add private admin endpoints under /v1/admin (create/delete/list blocks)
- [ ] Return stable IDs for blocks and expose remaining/expiry info
- 
- WORKERS:
- [ ] Implement expiry worker for credit_blocks (or adapt existing credit expiry worker)
- 
- AUDITABILITY:
- [ ] Plumb through actor + reason fields from admin requests
- [ ] Add structured audit log event per mutation (optional table if not already present)
- 
- TESTS:
- [ ] Integration tests: create block -> spend -> revoke -> verify balances and remaining
- [ ] Integration tests: revoke blocked by holds (balance < held_balance)
- [ ] Concurrency test: withdrawal and revoke racing (no double spend, no negative remaining)

---

# #260: solana-swap-to-usdc-via-jupiter

**Completed:** no

Add a frontend UI + backend support that lets a user swap any token they hold (SOL, PYUSD, USDG, etc.) into USDC to top up their USDC balance for Solana payments, using the Jupiter aggregator. This is the SWAP-TO-USDC funding flow; it is adjacent to the existing Solana one-off + recurring (USDC) payment work (see internal/modules/solana/pay.go, support.go, and the supported-tokens API in internal/http/handlers/solana_supported_tokens.go) but is its own feature.

## User problem + flow

A user wants to pay or subscribe in USDC (the preferred stablecoin; config.PreferredStablecoin) but holds value in another token — e.g. $100 of SOL. Today the supported-tokens API (GET supported tokens) returns each token's balance + a fiat->token quote and a `preferred`/`recurring_eligible` flag, but there is no way to CONVERT a non-USDC holding into USDC. The flow we are adding: user opens a "Top up USDC" panel -> sees their held balances (from the existing wallet-balance fetch) -> picks a source token + an amount (or a target USDC output) -> we fetch a Jupiter quote -> show estimated USDC output, price impact, slippage, and minimum received -> user confirms -> we return the Jupiter swap transaction -> THE USER signs and submits it from their own wallet -> their on-chain USDC balance is topped up. The supported-tokens API re-fetches and the new USDC balance is reflected.

## Jupiter integration

Jupiter is Solana's standard swap aggregator. Two calls drive the flow: (1) the Quote API (GET /quote with inputMint, outputMint=USDC mint EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v, amount, slippageBps, and routing filters) which returns the best route, out amount, price impact, and per-leg market info; and (2) the Swap API (POST /swap with the quote response + the user's wallet pubkey) which returns a base64 serialized (versioned) transaction. CRITICAL — consistent with the rest of this project (see pay.go: the user signs and submits Solana Pay transfers; there is no fee-payer delegation): the USER signs and submits the Jupiter swap transaction and THE USER pays the gas/priority fee. We do NOT set ourselves as feePayer and do NOT co-sign. The backend is a thin authenticated proxy/helper around Jupiter (to inject routing/dex preferences, slippage guards, and our USDC mint) plus optional quote caching; it never holds keys or custody. We should reuse the existing token config (config/solana_tokens.go) for the USDC mint + decimals and respect mainnet vs devnet (DefaultDevnetTokens) — note Jupiter routing liquidity is mainnet-centric, so devnet may be a stubbed/quote-only path.

## Orderbook-preference design (prefer orderbook venues over AMMs)

The product owner prefers orderbook-based execution (RFQ / limit- or market-order venues) over Uniswap-style constant-product AMM liquidity pools where possible. Jupiter can route across BOTH, so we express the preference via Jupiter's routing controls: use the `dexes` inclusion allowlist to bias toward true on-chain orderbook DEXs on Solana — notably Phoenix and OpenBook (OpenBook V2) — and consider Jupiter's RFQ / order-flow (Metis routing, Jupiter Z / RFQ) for market-maker fills. Other relevant knobs: `restrictIntermediateTokens=true` (route only through liquid intermediates, reduces exotic AMM hops), `onlyDirectRoutes` (single-hop, often lands on a single orderbook venue), and `excludeDexes` to drop specific AMM pools. The preference must be SOFT/configurable, not a hard requirement: if no orderbook route exists or an orderbook route is materially worse (price impact / output) than the best available route, we fall back to the aggregator's best route rather than failing the swap — surfacing which venue(s) were used. Expose this as backend config (an allowlisted-dexes list + an orderbook-preference toggle) so it can be tuned without code changes.

## Slippage / price-impact handling and limits

Slippage is expressed in basis points (slippageBps) and must be bounded server-side (e.g. a sane default like 50 bps and a hard max so the client cannot request an abusive value). The Quote response carries `priceImpactPct` and `otherAmountThreshold`/minimum-out; we surface price impact and the minimum-received USDC, and reject or warn when price impact exceeds a configured ceiling. The swap transaction must carry a concrete minimum-output so a worsening market causes the on-chain swap to revert rather than fill at a bad rate. Quotes are short-lived — stamp a `quoted_at`/`expires_at` (the existing flow uses ~15m for Pay quotes, but swap quotes should be much shorter, e.g. 30-60s) and refuse to build a swap tx from a stale quote.

## Failure handling + min-output

Handle: insufficient source balance (cross-check against the wallet balance the supported-tokens flow already fetches); no route found; price impact over ceiling; stale/expired quote; partial fills (some venues/RFQ can partially fill — decide whether to allow partial top-up or require full fill and reflect that in the min-output); and on-chain failure/revert (return a clear error and let the user retry with a fresh quote). The user pays gas, so a failed/reverted swap costs them only the network fee, never custody risk.

## Ties into existing supported-tokens API + quote flow

Reuse: the wallet-balance fetch and per-token balance/quote shape in solana_supported_tokens.go; TokenPriceProvider + CalculateTokenQuote (support.go) for a sanity USD reference on the swap (compare Jupiter's implied price against our Pyth price to flag suspicious quotes); config.PreferredStablecoin / IsStablecoin and the USDC mint from config/solana_tokens.go. The new swap endpoints live alongside the Solana module (internal/modules/solana) with their own handler(s), mirroring how GetSupportedTokens and GeneratePayment are structured.

## UX

A "Top up USDC" panel: lists the user's held token balances (excluding USDC itself), lets them pick a source token and enter either an input amount or a desired USDC output, then shows a live quote preview — estimated USDC received, price impact, slippage (bps, editable within bounds), minimum received, and the venue(s)/route used (highlighting when an orderbook venue is used). A single Confirm button triggers the wallet sign+submit. Show pending/confirmed/failed states and refresh balances on success.

## Security / edge cases

Slippage abuse (clamp bps server-side, reject excessive values); stale quotes (short TTL, server-side freshness check before building the tx, and bind the quote to the requesting user + wallet); partial fills (explicit min-out semantics); MEV / sandwiching (tight slippage, prefer orderbook/RFQ which is less MEV-exposed than AMM pools, and rely on the user's wallet for priority fees); quote tampering (the swap tx is built by Jupiter from the exact quote we proxied — re-validate inputMint/outputMint/amount/min-out before returning it); and never logging or storing wallet private keys (we never see them — user signs locally).

## Open questions

- Should we also support topping up OTHER stablecoins (USD1/PYUSD/USDG) or USDC only for v1? (USDC only recommended for v1.)
- Allow target-output mode (specify desired USDC out, solve for input) or input-only for v1?
- Do we run our own Jupiter API host / self-hosted Metis, or use the public Lite/Pro API (rate limits, key management)?
- Partial fill policy: allow partial top-up or require full fill?
- Devnet story: stub/quote-only vs a devnet swap path (Jupiter liquidity is mainnet-centric).
- Exact orderbook-preference fallback threshold (how much worse an AMM-inclusive route may be before we prefer it over an orderbook-only route).

**Tasks:**
- BACKEND:
- [ ] Add a Jupiter client in internal/integrations (or internal/modules/solana) wrapping the Quote API (GET /quote) and Swap API (POST /swap), with configurable base URL + optional API key
- [ ] Add backend config: USDC output mint (reuse config/solana_tokens.go), default slippageBps, max slippageBps, max price-impact ceiling, quote TTL, orderbook-preference toggle, and an allowlisted-dexes list (Phoenix, OpenBook/OpenBook V2)
- [ ] Implement routing/dex-preference logic: bias Jupiter toward orderbook venues via `dexes` allowlist + restrictIntermediateTokens/onlyDirectRoutes, consider RFQ/Metis order-flow, with soft fallback to best route when no/worse orderbook route exists
- [ ] Add POST quote endpoint (authenticated): inputs source token symbol/mint, amount, slippageBps; returns USDC out, price impact, min received, route/venues, quoted_at/expires_at
- [ ] Add POST swap endpoint (authenticated): takes a fresh quote + user wallet pubkey, returns the base64 serialized swap transaction for the USER to sign and submit (no fee-payer delegation, no co-sign)
- [ ] Implement slippage guards (clamp bps server-side), price-impact ceiling rejection, min-output enforcement, and stale-quote rejection (bind quote to user+wallet, short TTL)
- [ ] Cross-check requested input amount against the wallet's held balance; sanity-check Jupiter's implied price against TokenPriceProvider/CalculateTokenQuote (Pyth) and flag divergence
- FRONTEND:
- [ ] Build the "Top up USDC" panel that lists held token balances (excluding USDC) sourced from the supported-tokens API
- [ ] Add source-token selector + amount input (input-amount mode for v1), wired to the quote endpoint
- [ ] Render quote preview: estimated USDC received, price impact, editable slippage (within server bounds), minimum received, and route/venue(s) used (highlight orderbook venues)
- [ ] Add a Confirm button that requests the swap tx, has the user's wallet sign + submit it, and shows pending/confirmed/failed states
- [ ] Refresh held + USDC balances on success and surface clear errors (no route, price-impact too high, expired quote, insufficient balance, on-chain revert)
- INTEGRATION:
- [ ] Wire the panel to the existing supported-tokens API (balances + preferred/recurring_eligible) and reuse the USDC mint/decimals from config/solana_tokens.go
- [ ] After a successful swap, reflect the topped-up USDC balance in the supported-tokens / balance view used by the Solana payment flow
- [ ] Respect mainnet vs devnet token config (DefaultDevnetTokens); decide + implement the devnet behavior (stub/quote-only vs swap path)
- VERIFICATION:
- [ ] Unit tests for slippage clamping, price-impact ceiling, min-output, stale-quote rejection, and orderbook-preference routing/fallback selection
- [ ] Unit tests for the Jupiter client request building (dexes allowlist, restrictIntermediateTokens, onlyDirectRoutes) and response parsing (priceImpactPct, otherAmountThreshold)
- [ ] Handler tests for the quote + swap endpoints incl. auth, balance cross-check, and error paths (no route, expired quote, insufficient balance)
- [ ] Integration test that hits the real Jupiter quote API (devnet/mainnet) for a SOL->USDC quote and asserts a sane route + USDC out + min received
- [ ] Frontend test/dogfood of the Top-up-USDC panel: balances render, quote preview updates, confirm triggers sign+submit, balances refresh on success

---

# #276: solana-recurring-analytics

**Completed:** no

DEFERRED (analytics, work later). Analytics surfaces for recurring Solana (the remaining #258 analytics tasks): active recurring subs, MRR (USDC/USD1 at $1 peg), failed pulls, recovered-vs-churned, cranker gas spend. Admin endpoints + dashboard wiring, consistent with the existing Stripe/NMI metrics. Depends: #258.

**Tasks:**
- [ ] active recurring Solana subs + MRR per tenant (sum plan amounts at $1 peg)
- [ ] failed-pull / dunning metrics; recovered vs churned
- [ ] cranker gas spend per tenant (surface alongside the gas-alert #258)
- [ ] admin endpoints / dashboard wiring matching the card-processor metrics shape

---

# #292: openrails-helm-charts-k8s-deploy

**Completed:** no

Make OpenRails trivial to deploy on Kubernetes via OFFICIAL Helm charts, targeting BOTH self-hosted k8s (bare/on-prem clusters) AND managed k8s (DigitalOcean DOKS first, then GKE/EKS/AKS). Today deployment is docker-compose + a hand-rolled image; there is no first-class k8s story. A clean Helm chart is the substrate for both self-hosters and our own managed SaaS (see [[openrails-managed-saas]] / #293).

## Scope
Package the whole stack as a configurable chart: the OpenRails app (HTTP API + the embedded River workers for crank/dunning/reconcile), DB (in-cluster Postgres OR point at an external/managed DB), Redis/Garnet, ClickHouse (analytics, optional/toggleable), the migrations job, secrets (k8s Secrets / Vault / sealed-secrets), ingress + TLS, autoscaling, and config via values.yaml. Self-hosted vs managed differ mainly in whether DB/Redis/CH are in-cluster or external managed services — express both via values.

## Why DigitalOcean specifically
DOKS is cheap + popular with the indie/self-host audience; ship a DOKS quickstart (DO managed Postgres + Redis, DO load balancer + cert) and evaluate a DO Marketplace 1-click app. Keep the chart cloud-agnostic so GKE/EKS/AKS are just different values.

**Tasks:**
- [ ] Chart skeleton (deployment for the API, a separate deployment/args for the River workers, service, configmap, secret, HPA, PDB, serviceaccount); helm lint + a CI render test.
- [ ] values.yaml profiles: self-hosted (in-cluster Postgres/Redis/ClickHouse subcharts) vs managed (externalDatabase/externalRedis/externalClickHouse pointing at DO/GCP/etc managed services). ClickHouse/analytics fully optional (toggle).
- [ ] Migrations as a pre-install/pre-upgrade Job (helm hook) running the OpenRails migrator; gate the app start on it.
- [ ] Secrets: support k8s Secrets, Vault (the existing vault KV/transit adapter #251), and sealed-secrets/external-secrets; document the per-tenant DEK/secret-store wiring.
- [ ] Ingress + TLS via cert-manager (Let's Encrypt) + an nginx/traefik example; configurable host, CORS allowlist, and the public base URL the Solana Pay endpoints need.
- [ ] Resource requests/limits + HPA (CPU/RAM, and a worker-queue-depth custom metric later); readiness/liveness probes on the health endpoint.
- [ ] DigitalOcean DOKS quickstart: DO managed Postgres + managed Redis + DO LB; a values-do.yaml + a runbook; evaluate a DO Marketplace 1-click listing.
- [ ] Publish the chart to an OCI registry (ghcr) + a versioned helm repo; pin the OpenRails image tag; document upgrade/rollback.
- [ ] Docs: 'deploy OpenRails on k8s in 10 minutes' for self-hosted and for DOKS; production hardening notes (DB backups, Redis persistence for the Solana Pay pending set, secret rotation).

---

# #293: openrails-managed-saas

**Completed:** no

Stand up a HOSTED OpenRails SaaS where a user signs up and spins up their own OpenRails in a few clicks — no infra to run. THIS IS THE MAIN BUSINESS MODEL. Self-hosting stays free/OSS; the managed service is the commercial offering: we run, scale, monitor, and upgrade OpenRails for customers who just want billing-as-a-service.

## What it is
A control plane + dashboard on top of OpenRails' existing multi-tenant foundation: signup -> provision a tenant (or an isolated instance) -> the customer gets API keys + a dashboard to configure products/prices, connect their processors (Stripe/NMI/CCBill/Solana), and watch revenue. We lean on the multi-tenant work already built — RLS isolation + per-tenant secret store / DEK (#227/#259), federated/delegated tokens, the catalog-as-code apply, the de-embed/standalone deploy (#249). The Helm charts (#292 / [[openrails-helm-charts-k8s-deploy]]) are the deploy substrate for the managed pods (and keep self-host parity).

## Isolation model (decide early)
Two tiers: (a) SHARED multi-tenant cluster with RLS + per-tenant DEK (cheap, for small customers / free tier), and (b) DEDICATED per-tenant instance/namespace (for larger customers needing hard isolation / their own DB). Same image + chart, different provisioning.

## Meta-billing (the fun part)
We bill our SaaS customers... using OpenRails itself (dogfood): usage metering (API calls / active subscriptions / GMV %), pricing tiers, free tier, Stripe for the SaaS subscription. OpenRails-billing-OpenRails.

**Tasks:**
- [ ] Signup + onboarding: create an account -> provision a tenant (RLS row + per-tenant secret store/DEK) -> issue API keys + a delegated dashboard token. Self-serve, < 5 min to first test charge.
- [ ] Control-plane dashboard (web): manage products/prices (catalog-as-code #162 under the hood), connect processors (Stripe/NMI/CCBill/Solana keys into the per-tenant secret store), view subscriptions/revenue/dunning, manage webhooks.
- [ ] Isolation tiers: (a) shared cluster + RLS + per-tenant DEK for free/small; (b) dedicated instance/namespace via the Helm chart (#292) for enterprise. Provisioning automation for both.
- [ ] Meta-billing: dogfood OpenRails to bill SaaS customers — usage metering (active subs / API volume / optional GMV %), pricing tiers + free tier, Stripe for the SaaS plan, dunning on our own customers.
- [ ] Usage metering + quotas + rate limiting per tenant; surface usage in the dashboard; enforce plan limits.
- [ ] Scaling + ops: per-tenant/queue autoscaling, monitoring/alerting (cranker gas, failed pulls, webhook lag), backups, on-call runbooks, status page.
- [ ] Migration paths: self-host -> managed import, and managed -> self-host export (no lock-in; OSS core is the trust anchor).
- [ ] Security/compliance posture for handling processor keys + payment data at scale (secret rotation, audit log, SOC2 trajectory, PCI scope minimization since processors hold the PANs).
- [ ] Pricing + positioning: free tier (self-host parity), usage-based paid tiers, the 'Stripe-like billing infra you actually own, hosted for you' pitch.

---

# #297: deplatforming-resilient-card-vault

**Completed:** no

Make OpenRails' CARD billing survive a processor/vault de-platforming — the existential threat for the adult/high-risk merchants OpenRails serves (host apps). The card-side analog of the Solana rail (which is already un-de-platformable: no vault, no PCI, no acquirer).

## Strategy (decided 2026-06, after researching the vault market)
Keep cards in a NEUTRAL, EXPORTABLE third-party vault; OpenRails stays the orchestration/billing brain and NEVER sees the PAN. Architecture:
  Browser --(vault SDK/iframe, PAN bypasses OpenRails)--> VAULT (holds card + network token)
  OpenRails --("charge token X, $Y, off-session")--> VAULT --> high-risk acquirer (CCBill / NMI-high-risk / Epoch / Verotel ...)
PCI: because the PAN never touches OpenRails' servers, OpenRails/the merchant stays at SAQ A (a checklist), NOT PCI Level 1 (audits). The vault vendor carries PCI L1 + the breach liability. If OpenRails ever RECEIVES the raw PAN (even unstored) it jumps to SAQ D; if it STORES PANs itself it's PCI Level 1 — both to be avoided.

## Vendor decision: Spreedly (with caveats)
Researched Spreedly vs VGS vs Hyperswitch Cloud on (adult/de-platform trigger, export-on-exit, self-host):
- SPREEDLY = best fit: neutral infra (not judging content), most high-risk-tolerant, and the STRONGEST contractual export — documented self-service SFTP/JSON/PGP export to a PCI-DSS-compliant destination + a defined 30-DAY post-termination window. Cloud-only (no self-host), but the export right offsets lock-in. Caveat: broad 'unacceptable level of risk' suspension clause -> confirm adult stance + the export SLA contractually.
- VGS = content-neutral too, BUT its standard T&C grants NO export right ('may, but not obligated to, delete') -> export must be negotiated in. Weaker for this use case.
- HYPERSWITCH CLOUD = WORST for adult: ToS lets Juspay terminate IMMEDIATELY if the business is 'restricted or cautioned by card networks/acquirers' (adult IS network-restricted) -> a trapdoor for adult; and its export is a TEAM-ASSISTED process dependent on Juspay's cooperation (no clear for-cause survival). Hyperswitch's only real value here is that it's OSS + vault-agnostic (bring-your-own-vault: connect VGS/TokenEx) -> use it (if at all) as a router over an external vault, not as the vault. But OpenRails IS already the orchestration, so Hyperswitch's router is redundant.

## Why Spreedly + OpenRails (not Hyperswitch + Chargebee)
OpenRails already owns the billing brain (subscriptions, dunning, entitlements, proration) + the charge across processors. So the only missing piece for card portability is the vault. Spreedly slots in as 'the vault OpenRails commands'; no second billing vendor (Chargebee) or redundant router (Hyperswitch) needed.

## Phasing (decided 2026-06)
- NOW: processor-direct — Mobius/NMI (cards) + CCBill + Solana, OpenRails as orchestration. Simple, low-PCI (SAQ A), works today. INTERIM RISK to accept knowingly: card tokens are vaulted AT the processor and are NON-PORTABLE, so if a card processor de-platforms us TODAY the saved cards are STRANDED (we'd have to re-collect from every user). Only Solana is de-platform-resilient in this phase; the card side is not.
- LATER: slot Spreedly in as the neutral card vault -> cards become portable -> full card-side de-platform resilience, still at SAQ A, OpenRails unchanged as the brain. This issue is that 'later' work.
- TRIGGER to prioritize this issue: if any of Mobius/NMI/CCBill becomes shaky or signals de-platform risk, pull this forward (the interim card-lock-in is the exposure). Absent that, it can wait behind the usage-billing engine work.
## Precision (so the model is exact)
Spreedly sits IN FRONT OF the gateways — it holds the card + presents it to whichever gateway OpenRails picks; it is NOT itself a money-mover and does NOT replace the gateway/acquirer relationships. The win is gateway FLEXIBILITY: because the card lives in Spreedly (not in Mobius), we can swap/add gateways and route the SAME saved card to them without re-collecting from users.

## Export destinations if a vault de-platforms us (decided 2026-06)
Any export target must be PCI-DSS-compliant and provide a PCI AoC (the migration is vault-to-vault; we never touch PANs). Two tiers:
- STAY SAQ A: migrate to another managed neutral vault — TokenEx / VGS / Basis Theory (all neutral, portability-oriented; the new vault holds the PCI scope). Pre-provision one (TokenEx or VGS) as the standing failover and test the migration.
- PCI LEVEL 1 break-glass: self-hosted Hyperswitch vault (or Basis Theory on-prem) — only if we make it PCI-L1 with an AoC; this takes on the heavy regime, so it's the fallback-to-the-fallback, used only if we want to OWN the vault.

**Tasks:**
- [ ] Integrate a vault (Spreedly) via its CLIENT SDK/iframe so the PAN goes browser->vault and NEVER reaches OpenRails -> keep OpenRails at SAQ A. OpenRails stores only the vault token reference (mirror the existing token-reference posture).
- [ ] OpenRails charge path: 'charge vault token X, amount Y, off-session/MIT' -> vault de-tokenizes + routes to the configured high-risk acquirer. A new processor type ('vaulted-card via Spreedly') behind the planned narrow-processor-interface.
- [ ] Use NETWORK TOKENS (Visa VTS / MC MDES via Spreedly) for portability + higher auth rates + surviving card re-issuance (recurring doesn't break).
- [ ] CONTRACT: negotiate an export SLA that explicitly SURVIVES for-cause termination (not just voluntary exit); confirm Spreedly's adult-content stance in writing.
- [ ] FAILOVER PREP + DESTINATIONS: pre-provision a SECOND managed neutral vault account NOW (idle standby) as the export destination, and TEST the vault-to-vault migration once. EXPORT TARGETS (all must provide a PCI AoC to receive the migration):
      - PREFERRED (stay SAQ A): another managed neutral vault — TokenEx, VGS, or Basis Theory. The new vault carries PCI; OpenRails just re-points token refs (token-remap). The portable ecosystem is Spreedly <-> VGS <-> TokenEx <-> Basis Theory.
      - BREAK-GLASS (accept PCI Level 1): a SELF-HOSTED vault (self-hosted Hyperswitch locker, or Basis Theory on-prem) — only works if we make it PCI-DSS Level 1 compliant WITH an AoC (Spreedly only exports to a PCI-compliant destination), which flips us into PCI L1. Continuity option of last resort, not the default.
- [ ] TOKEN-REMAP design: a vault swap issues new token IDs -> OpenRails' vault/processor layer must consume an old->new token mapping and re-point references. Design the layer so 'swap vaults' = a remap step, not a re-architecture.
- [ ] Maintain standing relationships with multiple ADULT/HIGH-RISK acquirers (CCBill, NMI-via-high-risk-bank, Epoch, Verotel/Vendo) so the vault can route A->B->C if one acquirer drops you.
- [ ] Document the two-layer resilience posture for ops + sales: card payers ride a portable/exportable vault (move on 30 days' notice); crypto payers ride Solana (no vault, no PCI, no acquirer, nothing to de-platform).
- [ ] NOT to do (record the rationale): do NOT receive raw PANs in OpenRails (->SAQ D), do NOT self-host a PAN vault (->PCI Level 1 + breach liability), do NOT depend on Hyperswitch Cloud's vault for adult (termination trapdoor + cooperation-dependent export).
- [ ] TRIGGER/sequencing: prioritize this work if any card processor (Mobius/NMI/CCBill) signals de-platform risk (the interim phase leaves card tokens processor-locked = stranded-on-ban). Otherwise sequence behind the usage-billing engine (#289).

---

# #305: fast-eventual-consistency-admission

**Completed:** no

OPTIONAL future optimization: a FAST (eventual-consistency) admission mode for extreme per-payer throughput. TODAY the money axis is STRONG consistency: admission.Admitter calls credits.AuthorizeAndHold, a Postgres tx with SELECT ... FOR UPDATE on the payer balance row, so requests for one payer serialize on the lock and read the exact committed balance (zero overspend, but throughput bounded by lock-serialized txns + a DB round-trip per request). The THROUGHPUT axis is already Redis-fast in both modes.

FAST mode (this issue): per-request money decision against a Redis/Garnet HEADROOM counter (= available balance + remaining credit line), O(1), no lock, no DB round-trip; the durable Postgres debit is WRITE-BEHIND (async) and the Redis counter is RECONCILED from the authoritative PG balance periodically. Sub-ms at scale. BOUNDED OVERSPEND: atomic Redis decrement (no concurrent oversell) + cap = reconcile-lag x spend-rate, hard-capped by the credit limit. Per-host CONFIGURABLE strict|fast. Phase-2 host-side LEASE: the host reserves N units from OpenRails and spends locally, removing the per-request network hop; overspend bounded by lease size.

WHEN TO BUILD: only when a single payer needs hundreds+ req/s where the per-payer FOR UPDATE serialization or the admission round-trip becomes the bottleneck. Until then strict is plenty. Source: internal/modules/admission + the #298 latency design.

**Tasks:**
- [ ] CONFIGURABLE consistency per host/tenant (optionally per endpoint/credit_type): STRICT (sync AuthorizeAndHold, exact, zero overspend, higher latency) vs FAST (Redis headroom, eventual, bounded overspend, sub-ms). Default STRICT; opt into FAST for high-QPS.
- [ ] FAST PATH: admission = one Redis/Garnet op (throughput windows + cached money-headroom counter, atomic decrement); NO Postgres lock/round-trip per request. Keep strict FOR-UPDATE AuthorizeAndHold for low-QPS/exact callers.
- [ ] WRITE-BEHIND + RECONCILE: durable PG debit + usage_event async via host RecordUsage (#289); periodically resync the Redis headroom from the authoritative PG balance.
- [ ] BOUNDED OVERSPEND: atomic Redis decrement (no concurrent oversell) + credit_limit cap + reconcile-lag bound (throughput caps spend-rate); make the tolerance tier-tunable.
- [ ] OPTIONAL phase 2: host-side lease (reserve N units, spend locally, reconcile) to remove the per-request network hop; overspend bounded by lease size.

---

# #306: billing-admission-future-refinements

**Completed:** no

Deferred refinements to the usage-billing/admission system (core shipped + live-validated). None blocking; each is a small, well-scoped follow-up.

- TENANT->OWNER limit level (#304): admission enforces owner->actor budgets/throughput today; add the second level so a TENANT can cap each OWNER (tensorhub caps cozy), via the SAME admitter logic at a tenant-scoped key + tenant-level tier policy (owner sentinel). The budgets/limiter engines already support arbitrary scope keys.
- BUDGET CAPTURE tie-to-ledger (#304): on ledger CaptureHold, also budgets.Capture the matching reservation so reserved->used converts immediately. NOT required for correctness (the rolling window self-heals: an un-captured reservation ages out of the window), so it's an accuracy nicety.
- /v1/self delegated budget introspection (#304): a browser-token variant of GET /v1/service/budget. Redundant while hosts proxy the service endpoint; add if browsers should read budgets directly.
- usage_events MONTH PARTITIONING (#289): partition billing.usage_events by month for scale. Wrinkle: the idempotency unique index (tenant,owner,event_type,source,source_id) must include the partition key, weakening cross-month dedup (acceptable since source_ids are request/time-scoped). Premature until event volume demands it.
- INVOICE admin list + CSV export (#303): an admin (cross-owner) invoice list + CSV download. Self endpoints (GET /v1/self/invoices[/:id]) cover the customer need; admin/CSV on demand. (PDF rendering intentionally NOT planned.)
- tensorhub->OpenRails platform_policies ownership migration (#304): cross-repo move of tensorhub's tier policies onto OpenRails' tier_policies; do when consolidating.

**Tasks:**
- [ ] Tier policy carries money caps (credit_limit/monthly) + graduation applies them to account settings (auto credit-limit by tier).
- [ ] Arrears authorize gate: combine prepaid balance + remaining credit line as headroom (currently line-only; conservative).
- [ ] TENANT->OWNER admission level (tensorhub caps cozy) via tenant-scoped tier policy + admitter check.
- [ ] Tie budgets.Capture/Release to ledger CaptureHold/ReleaseHold (accuracy; window self-heals without it).
- [ ] /v1/self delegated budget introspection variant.
- [ ] usage_events month partitioning (include partition key in the idempotency index).
- [ ] Invoice admin (cross-owner) list + CSV export.
- [ ] Migrate tensorhub platform_policies -> OpenRails tier_policies (cross-repo).

---

# #129: direct-card-processing

**Completed:** no

Accept credit card details directly on our server instead of proxying through Mobius/NMI's hosted tokenization

## Metadata

- Category: architecture
- Priority: low
- Status: not_started
- Passes: false

## Details

- context: {"current_flow":["1. Frontend collects card info in Mobius-hosted iframe/form","2. Card data sent directly to Mobius/NMI (never touches our server)","3. Mobius returns a payment token or customer_vault_id","4. Our server uses token to charge via NMI API"],"proposed_flow":["1. Frontend collects card info in our own form","2. Card data POSTed to our server","3. Our server sends card data directly to NMI API","4. Store customer_vault_id for future charges"],"why_consider":["Full control over checkout UX (no iframe styling limitations)","Faster checkout (no redirect to third-party)","Can implement custom validation and error handling","Reduce dependency on Mobius-specific integration"]}
- nmi_api_parameters: {"direct_card_fields":["ccnumber - Credit card number","ccexp - Expiration date (MMYY format)","cvv - Card security code"],"with_customer_vault":["customer_vault=add_customer - Store card in vault","Returns customer_vault_id for future charges"]}
- pci_compliance_requirements: {"current_level":"SAQ A or SAQ A-EP (card data never touches our servers)","required_level":"SAQ D or full PCI DSS assessment","key_requirements":["Encrypt card data in transit (TLS 1.2+) - already done","Never store full card numbers (only last 4 + token)","Never log card data","Quarterly vulnerability scans by ASV (Approved Scanning Vendor)","Annual penetration testing","Secure coding practices audit","Network segmentation for cardholder data environment","Access controls and audit logging","Incident response plan"],"cost_estimate":{"asv_quarterly_scans":"$100-500/quarter","annual_pentest":"$5,000-20,000/year","pci_assessment":"$15,000-50,000/year for SAQ D","ongoing_compliance":"Significant engineering time for documentation and controls"}}
- recommendation: {"verdict":"Probably not worth it for current scale","reasoning":["PCI SAQ D compliance is expensive and time-consuming","Hosted tokenization (current approach) is industry standard","Mobius iframe can be styled reasonably well","Security risk/liability not worth UX improvement"],"reconsider_when":["Processing >$1M/month and UX is measurably hurting conversion","Mobius hosted form has significant technical limitations","We hire dedicated security/compliance staff"]}
- risks: ["PCI compliance burden and ongoing costs","Security liability if breached (card data exposure)","Mobius may not support direct integration (contractual requirement for hosted form)","More attack surface on our servers"]

**Tasks:**
- IMPLEMENTATION:
- [ ] Determine if Mobius Pay allows direct API integration (vs requiring their hosted form)
- [ ] Review NMI direct card submission API parameters (ccnumber, ccexp, cvv)
- [ ] Implement server-side card validation (Luhn check, expiry, CVV format)
- [ ] Add endpoint: POST /v1/checkout/card with {card_number, exp_month, exp_year, cvv, ...}
- [ ] Ensure card data is never logged (scrub from request logs)
- [ ] Ensure card data is never stored (only vault token after successful charge)
- [ ] Pass PCI compliance assessment
- [ ] Update frontend to use direct form instead of Mobius iframe

---

# #127: durable-workflow-execution

**Completed:** no

Evaluate and potentially adopt embedded durable workflow execution for critical payment flows

## Metadata

- Category: architecture
- Status: on_hold
- Passes: false

## Details

- status_update: {"date":"2025-12-14","decision":"On hold - focus on simpler solutions first","reasoning":["Ad-hoc durable workflow system was built and then reverted","Durable workflows don't compensate for faulty code - unit tests do","Idempotency keys on NMI calls solve the double-charge problem more simply","If we need durable workflows later, use a proper library like go-workflows"],"prerequisite":"Implement wrap-nmi-calls-with-idempotency first, then reassess if durable workflows are still needed"}
- candidate_library: {"go_workflows":{"repo":"github.com/cschleiden/go-workflows","maturity":"Production-ready, supports PostgreSQL","when_to_adopt":["Multi-step flows that genuinely need crash recovery","Workflows with long-running waits (e.g., approval flows)","Complex saga patterns with compensating transactions"]}}
- candidate_workflows: ["Checkout flow (charge → record → grant entitlements)","Subscription upgrade (charge proration → cancel old → create new)","Dunning/rebill (charge → renew or → escalate failure)","Refund flow (refund at processor → revoke entitlements → update records)"]
- when_to_reconsider: ["After idempotency keys are implemented and we still see issues","When we add complex multi-step flows (e.g., multi-party payouts)","If reconciliation reports show frequent partial-completion issues"]

**Tasks:**
- ON HOLD - Reassess after implementing idempotency keys
- Previous ad-hoc implementation was reverted on 2025-12-14
- If needed later:
- [ ] Evaluate go-workflows with a simple PoC (non-critical path first)
- [ ] Create PoC: Wrap a simple workflow (e.g., refund) with go-workflows
- [ ] Benchmark: Measure overhead of event persistence
- [ ] Decide: Adopt go-workflows based on PoC results

---
# #351: optional managed cloudflared: expose a local OpenRails instance externally

**Completed:** no

Optionally RUN cloudflared (not just document it): a local/dev OpenRails instance gets a stable public hostname so external systems can reach it — primarily processor webhooks (NMI/Mobius, Stripe, CCBill) hitting a laptop or CI box, and host apps integrating against a dev instance. Context: the doc-only `cloudflared` config block (tunnel_token/tunnel_name/public_hostname) was removed in the #350 knob diet because OpenRails never ran it; if we ever DO run it, the config earns its way back. Filed per Paul 2026-06-11: "we presumably want cloudflared to run, so that a local instance can be made available externally by another system. Kind of optional."

Sketch: a `cloudflared` sidecar in the dev compose stack (image exists upstream; needs tunnel token) OR an opt-in supervisor goroutine that execs a bundled/system cloudflared with the configured token and logs the public hostname at boot. Compose-sidecar is likely the right shape — keeps the binary out of OpenRails and the knob count at zero (token lives in .env for compose only). docs/cloudflared-webhooks.md already documents the manual flow.

**Tasks:**
- [ ] Decide shape: compose sidecar (preferred) vs supervised child process
- [ ] Wire dev compose service + .env token passthrough; print the public webhook URL on boot
- [ ] Update docs/cloudflared-webhooks.md from manual steps to the supported flow

---

# #493: fold the product catalog into the platform-policy / platform-config umbrella

**Completed:** no

One declarative, CLI-refreshable config per platform. Today the OpenRails product
catalog (`internal/modules/catalog`: ProductService/PriceService, models.Product/
models.Price — what's sold + prices) is managed separately from the host's
platform policy, and tensorhub's `platform-config apply` only pushes the #486
capacity ladder. Goal: fold the catalog into the same umbrella so one config doc
(per platform) drives both, pushed idempotently (full-upsert + diff) the same way
the tier ladder is.

Constraint (Paul, 2026-06-14): doujins, hentai0, cozy-art are NOT API platforms —
they need NO api-spend platform policy (tiers / wasted-spend / in-flight caps /
allowed-endpoints / arrears). They need ONLY the product catalog (products +
prices for subscriptions/checkout). tensorhub IS the API platform and needs both
halves. So the unified per-platform doc has two parts, the governance half OPTIONAL:
  - `catalog:` (products + prices) — every platform.
  - `api_governance:` (capacity ladder #486 + tier/budget policy + allowed
    endpoints + arrears) — API platforms only (tensorhub).

This supersedes the earlier vague "per-stack config CLI for sibling stacks" idea:
non-API stacks just run the same `platform-config apply` with a catalog-only doc.

Design questions to resolve before building:
- Does a generic SDK/client setter exist for hosts to push products + prices
  (SetCatalog / UpsertProducts)? The catalog module is internal — confirm; if
  absent, add one mirroring SetTierSchedule/SetTierPolicy (idempotent full-upsert).
  Keep it generic — products/prices only, no host meaning.
- Unified config schema (one YAML: `catalog:` + optional `api_governance:`).
- Push targets: catalog -> OpenRails catalog; governance -> OpenRails admission +
  tensorhub.platform_policies. Extend `platform-config apply` to push both.

**Tasks:**
- [ ] Audit the catalog module for an existing host-facing setter; design one if missing
- [ ] Design the unified per-platform config schema (catalog + optional api_governance)
- [ ] Extend tensorhub `platform-config apply` to push the catalog section
- [ ] Author catalog-only configs for doujins/hentai0/cozy-art; full config for tensorhub

---
