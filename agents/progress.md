<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 623

---

# #622: catalog prices need explicit product-access windows

**Completed:** no
**Status:** PLANNED 2026-06-29 (Codex; revised w/ Claude, verified against code). Model prices as ways to create or renew product access windows; product entitlements are granted while that access is active. This commits to an engine change (link entitlement windows to the product-access window) on top of a manifest grammar change — not a manifest-only tweak — but reuses the existing materialization primitives instead of rebuilding them.

## Reason

Doujins has legacy Solana one-off premium prices: $23 for 30 days and $62 for 90 days. Today the catalog manifest says `entitlements: [premium]` at the product level, which loses the important detail: how long this specific price grants the entitlement.

The bad workaround is defining those one-off prices as separate premium-like products. That creates two identities for the same thing: a customer could buy product A as a 30-day one-off and product B as a recurring subscription, then either get billed twice for the same premium usage or produce unclear stacked entitlement windows. Even if stacking happens to work, the catalog has lost the fact that both purchases are the same product/use case.

The product should remain one product (`premium`). Prices should describe how that product is acquired and for how long: 7-day renewing access, 30-day renewing access, 365-day yearly renewing access, 30-day one-off access, 90-day one-off access, or a 7-day `trial` first phase that continues into 30-day auto-renewing access. The `trial` phase has its own `unit_amount` and may be free (`unit_amount: 0`) or paid — it is not restricted to free.

The root model should be product access, not direct entitlement duration. A customer owns/has access to `premium` for a window; while that product access is active, OpenRails grants the product's entitlements. When access expires, the derived entitlements expire too. The same model covers movie rentals: access to a movie for 3 days grants viewing rights for 3 days; durable ownership is access with no end.

The materialization primitives mostly exist; the new work is manifest grammar plus one small engine link (verified in code 2026-06-29):
- `product.EntitlementsSpec map[string]*int` is entitlement name → **duration days** (nil/0 = indefinite).
- One-off checkout already honors it: `grantProductEntitlements` (`internal/modules/checkout/purchase_service.go:582-599`) sets a finite entitlement `end_at` = start + duration, overriding the wallet/`billing_cycle_days` fallback.
- Re-purchase already **stacks**: a new window starts at the existing access end (`coverage.EndDate`, same file `:584`). Deterministic merge is already the behavior.
- One-off purchase already records a product-access grant (#250, `grantProductAccess` `:524`) — but ALWAYS durable; it never sets `ends_at`, even though the column + `GrantProductAccess` params support a finite window.
- Entitlements and product-access are **independent** grant paths today; entitlements are NOT derived from active product-access (no link in `internal/modules/{entitlements,grants}` or the converge passes). Linking them is the deliberate engine change this issue commits to.
- A two-phase price (first period at its own price/length, then recurring terms) already exists as `Price.Intro` (#602: `intro: {amount, interval}`). The `trial` phase below IS this field, renamed (`intro`→`trial`, `amount`→`unit_amount`) and extended to the new grammar — not a new parallel field.

So the work splits into reuse vs new. **Reused as-is:** entitlement `end_at`, `product_access_grants.ends_at`, coverage-stacking, and `Price.Intro`. **New:** (G1) per-price access grammar in the manifest — `product.Entitlements []string` is product-level and drops durations (`entitlementsSpec` maps every name to `nil`, `pkg/catalog/plan.go:384`), so one product can't host two prices granting the same entitlement for different windows ($23/30d vs $62/90d); (G2) finite product-access — `grantProductAccess` (`:524`) must pass `ends_at` instead of always-durable; (G3) derive the entitlement window from the granted product-access window. This is an engine change, but a small one — it wires existing primitives together. `ParseDurationSpec` (`pkg/catalog/duration.go`) already parses `30d`/`72h` and rejects other units (h/d only — no month/year unit; a year is `365d`).

This should be explicit catalog data, not inferred from payment provider, rail, price amount, or product name.

## Scope

- Design price-level access effects. Product metadata describes what is being sold; each price declares how buying that price creates or renews access to that product.
- Keep equivalent purchase paths under the same product identity instead of creating duplicate products for one-off vs recurring access.
- Add an explicit price access shape, reusing the existing duration grammar (`ParseDurationSpec`: `7d`/`30d`/`90d`/`365d`/`72h`, h/d only, reject other units):
  - `duration` is the access-window length and the SINGLE source for it — there is no separate `product_access` field (that would restate the same number). A finite value (`3d`/`30d`/`90d`/`365d`) = rental/one-off window; `duration: indefinite` = durable/perpetual ownership (no end). `duration` is OPTIONAL and defaults to `indefinite` when omitted. `duration` drives BOTH `product_access_grants.ends_at` (nil for `indefinite`) and the derived entitlement `end_at`. `indefinite` reuses the codebase's existing vocabulary (entitlement service `Indefinite`, `EntitlementsSpec` nil=indefinite) and the grammar's existing non-numeric keyword precedent (`once`); it is accepted bare or quoted.
  - `auto_renew: true|false` says whether the price charges again and extends after `duration`. OPTIONAL, defaults to `false`. Do NOT infer auto-renew from `duration` vs `interval`. A finite `duration` is required to renew: `auto_renew: true` with `duration: indefinite` OR omitted `duration` is a manifest validation ERROR (nothing to renew).
  - `interval` is the existing recurring field — keep it ONLY as a deprecated compat alias mapping `interval: 30d` → `duration: 30d, auto_renew: true`; new manifests use `duration`+`auto_renew`.
  - the optional first phase is the EXISTING `Price.Intro` (#602: `intro: {amount, interval}`), renamed/extended to `trial: {unit_amount, duration, auto_renew}` (`intro`→`trial`, `amount`→`unit_amount` for consistency with the price's own `unit_amount`) — a first period at its own price/length, then the price's recurring terms. Do NOT add a second parallel intro field.
- Materialize through existing primitives only — no new tables/columns: entitlement window via entitlement `end_at`, product-access window via `product_access_grants.ends_at` (pass it into `grantProductAccess`).
- Derive the entitlement window from the granted product-access window (the G3 link) instead of every price duplicating the product's entitlement list. When access expires, the derived entitlement expires with it.
- Overlapping equivalent purchase: default is extend/stack the access window — already the behavior via coverage-stacking, so keep it. Blocking checkout for an active equivalent is OUT OF SCOPE for v1 (one product + stacking already prevents two unrelated identities).
- Preserve subscription behavior: recurring prices renew/refresh the access window each billing period.
- Avoid fake products for beta access, rentals, or legacy premium duration variants.

## Acceptance

- A catalog can declare multiple prices for the same product with different access shapes: `premium` for `30d` one-off (`duration: 30d`, `auto_renew: false`), `90d` one-off, `30d` auto-renewing, `365d` auto-renewing, and a free or paid `7d` `trial` phase into `30d` auto-renewing access.
- No parallel grammar: `trial` reuses/renames `Price.Intro` (#602: `intro`→`trial`, `amount`→`unit_amount`) and `interval` is a deprecated compat alias — there is exactly one way to express "first phase then recurring," and one way to express a recurring window.
- A catalog does not need a second premium product to model Solana one-off premium access.
- Overlapping equivalent purchase extends/stacks the existing access window (no second unrelated identity); v1 does not block checkout.
- A catalog can declare a movie rental price (`duration: 3d`, `auto_renew: false`) that grants product access for 3 days without granting permanent ownership; a price with `duration: indefinite` (or omitted) grants durable ownership; `auto_renew` omitted defaults to false.
- Apply fails with a validation error when `auto_renew: true` is paired with `duration: indefinite` or an omitted `duration` (nothing to renew).
- Checkout/purchase tests prove finite entitlement `end_at` and product access `ends_at` are set from price effects, and that the entitlement window matches the granted product-access window (G3).
- Subscription tests prove recurring plans still renew/grant through the existing subscription window behavior.
- Doujins legacy Solana premium prices can be represented without relying on implicit billing-cycle fallback semantics.

---

# #621: catalog prices own providers explicitly

**Completed:** yes
**Status:** COMPLETED 2026-06-29 (Codex). Removed `default_providers` and product-level `providers` from catalog manifests. Provider sync/routing is declared only on the price itself; omitted/empty price providers means OpenRails-native/no external provider sync. OpenRails examples/tests and Doujins bootstrap were updated. Validation: `go test ./pkg/catalog ./pkg/embedded ./cmd/openrails`; Doujins `go test ./config ./internal/billing/openrailsembed`.

## Reason

`default_providers` and product `providers` are only YAML inheritance:

`price.providers -> product.providers -> catalog.default_providers`

That saves a few repeated strings but hides the important decision: which external rails this exact price should sync/link/create against. It is especially confusing for mixed catalogs where the current price, historical prices, native/metered prices, Solana prices, and CCBill-only legacy prices all differ.

Follow the Stripe-simple model: a price carries the provider-specific publication/linking intent. The product and catalog do not.

## Scope

- Delete `Manifest.DefaultProviders`.
- Delete `Product.Providers`.
- Keep `Price.Providers` as the only provider list.
- Change provider resolution to read `price.providers` only.
- Decide and document the nil/empty meaning for `Price.Providers`; preferred: omitted or `[]` means OpenRails-native/no external provider sync.
- Update `config/catalog.example.yaml`, embedded catalog push tests, and Doujins bootstrap manifests to put `providers` directly on each externally synced price.
- Remove docs/comments that describe provider inheritance.

## Acceptance

- Catalog parser rejects `default_providers` and product-level `providers` as unknown fields.
- Every external-provider price in examples/tests declares `providers` on that price.
- Native/metered/internal-only prices omit `providers` or set `providers: []` consistently.
- Focused tests pass: `go test ./pkg/catalog ./pkg/embedded ./cmd/openrails`.
- Doujins embedded bootstrap tests pass after updating its manifest shape.

---

# #611: exhaustive v1 catalog bootstrap examples and real HTTP coverage

**Completed:** no
**Status:** PLANNED 2026-06-29: catalog v1 needs one canonical bootstrap example and real HTTP publish/apply coverage for the full intended product surface, not just simple subscription tiers.

Added 2026-06-29 (Codex). `config/catalog.example.yaml` and the catalog publish integration tests should prove the v1 manifest can model the main OpenRails billing/product shapes without old `tier_groups` compatibility. Doujins can remain a simple single-tier premium entitlement consumer, but the OpenRails example must also cover more complex product and billing systems.

## Goal

Make the catalog example and integration coverage exhaustive for the planned v1 catalog surface:

- Single-tier premium entitlement product for Doujins-style apps.
- SaaS multi-tier entitlement ladder with free-trial/intro pricing before recurring renewal.
- AI image generation credit packs and recurring AI credit grants using custom credit units.
- Prepaid API credits for fal.ai-style API spend using a separate custom credit unit.
- Content marketplace products, such as one-off movie/video ownership.
- Bundle products using `includes`, such as a Prime-style package that owns several videos.
- Claude Code-style rate-limit plans with 5x and 20x usage-limit windows.
- DigitalOcean/Runpod-style infrastructure billing: VM rental and cloud-storage metered billing.

## Implementation Tasks

- [ ] Expand `config/catalog.example.yaml` into the canonical v1 catalog bootstrap example using only flat `products:` and no legacy catalog structures.
- [ ] Keep the example readable but exhaustive: use product slugs/tier groups that map clearly to the above use cases, with comments only where needed to explain units.
- [ ] Add explicit `interval: once` support for one-time purchases so content marketplace products are not represented as fake monthly prices.
- [ ] Ensure recurring free-trial/intro pricing is shown with `intro: {amount: 0, period_days: ...}` and then normal recurring renewal.
- [ ] Make `credits` examples cover custom non-money units, including `ai-image-credit` and `api-credit`.
- [ ] Make `usage_limits` examples cover request-count windows and 5x/20x plan variants.
- [ ] Make `meters`/metered prices examples cover counter billing for VM-hours and gauge/time billing for cloud storage.
- [ ] Persist catalog-owned usage-limit registry rows, meter registry rows, metered price sidecars, and product include relationships during catalog publish/apply; do not fake these as parse-only fields.
- [ ] Do not materialize customer-scoped usage bindings during catalog publish; those belong at grant/subscription/purchase time when a customer exists.

## Integration Tests

- [ ] Add a real HTTP integration manifest fixture that uses the same full product surface as the example, with per-test unique slugs.
- [ ] Publish it through `POST /v1/merchant/catalog/publish` against the real standalone OpenRails server.
- [ ] Assert the plan-only call returns changes but writes nothing.
- [ ] Assert the apply call creates every product and price through the live HTTP surface.
- [ ] Query the real DB after apply and assert:
      products have expected `tier_group`, `tier_rank`, entitlements, and credit specs;
      one-time prices have no recurring cycle;
      intro/trial prices store initial amount/period;
      metered prices store catalog meter sidecars;
      usage-limit registry rows exist;
      product include relationships exist.
- [ ] Assert a second plan after apply is idempotent for products/prices and sidecars.
- [ ] Keep existing smaller publish/apply tests if they still prove permission and mutation-mode behavior.

## Acceptance

- `config/catalog.example.yaml` is a useful bootstrap reference for all listed catalog use cases.
- No public example or active integration fixture uses `tier_groups`.
- Real HTTP integration coverage exercises the full manifest through OpenRails, not mocks.
- Focused unit tests cover parser edge cases only; end-to-end catalog confidence comes from `go test -tags=integration ./internal/integrationharness -run ...`.

---

# #607: provider-intents-as-boring-outbound-queue

**Completed:** no
**Status:** NOT_STARTED 2026-06-29: simplify the provider-intent system so `openrails.provider_intents` is a normal outbound message queue/current-work table, while provider mutation history lives in ClickHouse events. This issue intentionally cuts the "ledger/tombstone forever" model back to boring queue semantics.

Make provider intents easier to reason about: pending/retryable/in-flight/dead work belongs in Postgres; historical attempts/results belong in ClickHouse; successful work should leave the queue.

REVIEW 2026-06-29 (Claude) — STOP before building: the core move (DELETE successful rows) breaks
effectively-once and can DOUBLE-CHARGE. Grounded in the schema + code:

- `provider_intents` is the "durable, effectively-once outbox for ALL outbound provider mutations"
  (#358). Dedup is the UNIQUE index `uq_provider_intents_merchant_idempotency_key` on
  `(merchant_id, idempotency_key)`. The column comment is explicit: "Re-enqueues conflict here ...
  anything else untouched — effectively-once per logical intent." The SUCCEEDED row IS the dedupe
  tombstone — a re-enqueue with the same key hits the conflict and becomes a no-op instead of
  re-running the provider mutation.
- So "delete successful rows" DIRECTLY removes effectively-once: after deletion a re-enqueue with the
  same idempotency_key finds no conflict → inserts a fresh `pending` intent → the executor runs the
  provider call AGAIN → double charge / duplicate refund / duplicate subscription. Retries and webhook
  replays are exactly when this fires.
- The non-goal "do not add a dedupe table unless a replay bug proves it is needed" is BACKWARDS: the
  existing row IS the dedupe record. Delete it and forbid a replacement and you have removed
  idempotency from a PAYMENTS queue — the replay bug is then structurally guaranteed.

BETTER DESIGN — separate the WORKING SET from the DEDUPE TOMBSTONE (both already exist):
1. Boring-queue reads come FREE, no deletes. `idx_provider_intents_due` is ALREADY a partial index over
   active states (pending/in_flight/failed_retryable/unknown_needs_verify), so `openrails intents`
   ("what work exists now?") already excludes succeeded rows. Point the CLI at the active set —
   succeeded work leaves the WORKING SET without leaving the TABLE.
2. Bound growth by trimming WEIGHT, not IDENTITY. After logging the rich attempt to ClickHouse, prune
   the heavy columns (`payload`, `result_evidence`) on succeeded/superseded/expired rows but RETAIN the
   slim tombstone (`merchant_id`, `idempotency_key`, terminal status, `executed_at`, result pointer).
   Or move it to a slim `provider_intent_dedupe` archive that enqueue ALSO checks. Delete the bytes,
   never the identity.
3. The dedupe check MUST stay in Postgres. The issue itself says execution must not depend on ClickHouse
   — so ClickHouse holds HISTORY but can NEVER be the idempotency source. "delete row + rely on
   ClickHouse" violates the issue's own non-goal.

Net: keep the boring-queue ergonomics (the goal is right); drop only the "DELETE succeeded rows"
mechanism (the means is unsafe). The table stays the effectively-once outbox; the CLI gets its clean
"current work" view from the partial index. Desired model / tasks / non-goals below revised accordingly.

## Metadata

- Category: cleanup
- Status: not_started (REVISED 2026-06-29 — redesign before build)
- Passes: false

## Desired model

- `provider_intents` is the durable outbound queue, not an audit ledger.
- Live queue states are the only normal residents: `pending`, `in_flight`, `failed_retryable`, `unknown_needs_verify`.
- Operator-attention states may remain temporarily: `failed_terminal` as dead-letter, maybe parked/retryable states while blocked.
- Successful, superseded, and expired work is logged to ClickHouse and leaves the WORKING SET once the transition is committed — but its slim dedupe tombstone (merchant_id, idempotency_key, terminal status) STAYS in Postgres. The active partial index, not row deletion, is what makes it leave the queue view.
- `openrails intents` answers "what work exists now?" (reads the active partial index, not the whole table)
- `openrails intents-log` answers "what happened historically?"
- ClickHouse remains optional/degraded analytics/audit storage; provider execution must not depend on ClickHouse being reachable.

## Non-goals

- Do not DELETE the dedupe record. The succeeded `provider_intents` row (UNIQUE on merchant_id+idempotency_key) IS the effectively-once tombstone; keep it (slimmed) or move it to a durable Postgres archive enqueue also checks. A slim dedupe table is REQUIRED here, not forbidden — only a SECOND redundant one is.
- Do not make ClickHouse a source of truth for billing/provider decisions OR for idempotency — the dedupe check must read Postgres only.
- Do not preserve compatibility for the old "provider_intents as forever ledger" mental model.

**Tasks:**
- [ ] Rename comments/docs/help text that call `provider_intents` a ledger where that implies permanent history; use queue/outbox/dead-letter wording.
- [ ] Point `openrails intents` (the "current work" view) at the ACTIVE partial index (`idx_provider_intents_due` states), so succeeded rows leave the working set with NO row deletion.
- [ ] On the success transition, log the rich attempt to ClickHouse, then PRUNE the heavy columns (`payload`, `result_evidence`) on the succeeded row while RETAINING the slim dedupe tombstone (merchant_id, idempotency_key, terminal status, executed_at, result pointer). Do NOT delete the row — effectively-once depends on it.
- [ ] Same prune-not-delete for `superseded`/`expired`: trim weight, keep the identity row so re-enqueue stays a no-op.
- [ ] Treat `failed_terminal` as the dead-letter state: keep it visible in `openrails intents`, then add an explicit operator action or bounded prune path later if needed.
- [ ] Keep `EnqueueAndExecute`'s effectively-once contract intact: a re-enqueue on an existing (now slimmed) succeeded tombstone returns the prior result and does NOT re-execute the provider call.
- [ ] Update CLI output/tests so `openrails intents --status=succeeded` is no longer expected to show historical successes; use `openrails intents-log --phase=succeeded`.
- [ ] Remove remaining writes/reads of the Postgres `external_provider_mutation_logs` path once ClickHouse provider mutation events cover the CLI/history use case.
- [ ] Remove the Postgres `external_provider_mutation_logs` table from the greenfield schema and merchant-delete/export bookkeeping after the app no longer uses it.
- [ ] Keep focused tests proving: success leaves the active working set but a re-enqueue with the same idempotency_key still no-ops (NO second provider call); retryable/dead rows stay; logs are emitted/spooled; and a webhook-replay / retry after success does NOT double-execute.

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
