<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 513

---

# #512: double-entry-immutable-ledger-reorg

**Completed:** no
**Status:** PLANNED 2026-06-17: design from a TigerBeetle (github.com/tigerbeetle/tigerbeetle) data-model comparison; no code written yet. Re-shape the money ledger from a single-entry, mutate-in-place, per-customer-balance store into a TigerBeetle-style **double-entry, append-only ledger** (accounts + immutable transfers, balances derived, holds = one durable two-phase primitive), WITHOUT adopting TigerBeetle-the-database. Today there is NO counter-account anywhere (`grep` for debit/credit/counter-account in `internal/modules/money` returns nothing): a deposit mints a `money_block`, a charge decrements `money_blocks.remaining_amount`, and money is not conserved.

Adopt TigerBeetle's data-modeling **principles** inside Postgres so every money movement has two sides (conserved by construction), the ledger is immutable+auditable, holds stop being fragmented across four mechanisms, and arrears stops being a hand-maintained scalar. Running TigerBeetle itself is explicitly out of scope here and gated behind the Phase F scaling trigger.

## Why make these changes

Current model (verified in code 2026-06-17):

- **Single-entry, money not conserved.** `depositTx` inserts a `money_transactions` row + a `money_blocks` lot; `withdrawBalanceAndBlocks` decrements `money_blocks.remaining_amount` FIFO (`SetMoneyBlockRemaining`). There is no offsetting account (no platform-revenue / processor-clearing / arrears-liability / expired-credits account), so there is no invariant to assert and reconciliation against Stripe/NMI/Solana is a bespoke diff rather than "the books net out".
- **Mutable, not append-only.** `money_transactions` rows are UPDATEd in place (`status`, `authorized_amount`, `captured_amount`); `money_blocks` rows are mutated and DELETEd on expiry; a denormalized `balance_after` running snapshot is stored on every row. Mutation + denormalization is a drift surface and blocks any future replication/sharding of the ledger.
- **Holds are an admission concern, not a ledger concern (RESOLVED — keep in Redis).** Four distinct hold mechanisms coexist, and they are NOT redundant: (1) `AuthorizeAndHold`'s per-request money-capacity hold lives in **Redis** keyed by `merchant+request_id` — ephemeral *by design*, so the hot admission path never writes Postgres; (2) `money_windows` (`held_amount`/`settled_amount`, plus the `authorized_amount`/`captured_amount` columns written only by `windows.go` settle) — a prepaid BULK reservation handed to a host to meter locally, settled in batches (durable on purpose); (3) `budget_inflight_holds` — windowed spend-budget/rate caps, durable because the rolling window needs history (a different axis: rate/budget, not balance). `deriveBalance`'s `HeldBalance` (= `SumActiveMoneyHeld`) sums ONLY open `money_windows` — it deliberately does NOT see the Redis hold. This is sound: the Redis hold reserves nothing settled; the no-overspend guarantee lives at CAPTURE, not at admission (see Phase C / decision 8). TigerBeetle's durable two-phase transfer is the right model for real money movement (capture/refund/settle), NOT for the throwaway admission gate.
- **Arrears as a scalar.** `money_settings.outstanding_owed_amount` + `credit_limit_amount` model debt as a mutable counter + ceiling, exposure re-derived from open invoices — instead of a real account allowed to go negative.
- **Business attribution baked into ledger rows.** `money_transactions` carries `invoker_id`, `resource`, `description`, `metadata`, `invoice_id`. TigerBeetle keeps ledger rows pure and pushes all of that to opaque `user_data` references.
- **One coarse concurrency primitive.** Every spend/hold/deposit/capture takes a `FOR UPDATE` lock on the `customers` row (`lockBalance`). Correct, but a hard per-customer throughput ceiling — and exactly the row-lock TigerBeetle's deterministic batched applier exists to eliminate.

TigerBeetle's lesson, in five primitives we are mapping onto Postgres: **Account** (belongs to one ledger=currency; balance derived from debits/credits, never stored as an overwritten scalar; sign-constraint flags), **Transfer** (immutable, append-only, `id`=idempotency, moves amount debit→credit within ONE ledger), **two-phase transfer** (pending → post/void = authorize/capture/release), **linked transfers** (atomic all-or-nothing chains; FX = linked transfers through a liquidity account, since no single transfer crosses ledgers), and **`user_data`/`code`** (opaque join keys back to the control plane; the ledger knows nothing about subscriptions/products/entitlements).

## Guiding principles / design decisions

1. **Principles in Postgres, not the database.** This issue does NOT introduce TigerBeetle as a running system. It re-shapes the Postgres schema + `internal/modules/money` to TB semantics. Actually running TB is a separate, later decision gated on Phase F.
2. **Conserve by construction.** Model an external/world side for every movement (a `world`/equity account or the processor-clearing account that nets against real processor float) so `Σ balances over all accounts in a (merchant, currency) ledger == 0` is an assertable invariant.
3. **Derive, don't store.** Account balance = `Σ credits_posted − Σ debits_posted`; held = pending side. Keep the cheap partial-index/derive approach already used post-#491; drop `balance_after`.
4. **Immutable ledger.** Capture/void/refund/expiry are NEW linked rows, never in-place updates or deletes.
5. **Holds stay ephemeral (Redis); the bill is durable (Postgres).** The admission hold is a throwaway Redis reservation keyed by the provider/tensorhub request-id; the hot path writes NO Postgres and reads balance from an in-memory cache. The durable double-entry ledger (Phases A/B) is written at/after capture, OFF the hot path (batched/async). Slight over-spend from a stale cache is acceptable and bounded (decision 8).
6. **Ledger purity.** Transfer rows carry accounts/amount/currency/type/pending_id/flags/source+source_id + opaque `user_data` refs only; business joins live in control-plane tables.
7. **Keep what already matches TB:** integer minor units (no floats), idempotency keys, entitlements `tstzrange` GIST-exclusion windows, append-only `usage_events`.
8. **Target admission model (RESOLVED 2026-06-17, Paul).** The whole money admission path is exactly this and nothing more: (a) request costs $X; (b) admit iff `cached_balance − Σ active Redis holds ≥ $X`; (c) on admit, record an $X hold in Redis keyed by the request-id; (d) on completion, deduct ACTUAL cost and release the hold; (e) on failure, release the hold and bill nothing, but record money-wasted in Redis for rate-limit/abuse. No Postgres on the hot path; balance is memory-cached; the durable spend is persisted to the ledger off the hot path. No-overspend is an ADMISSION-time check against the cached balance, not a capture-time Postgres gate — a slightly-negative balance from a stale cache is fine.

## Metadata

- Category: architecture
- Status: planned
- Passes: false

## Work breakdown

### A. Double-entry account + transfer foundation
- [ ] New `openrails.ledger_accounts` (id, merchant_id, customer_id NULL, account_type, currency, flags, created_at). account_type ∈ {customer_balance, platform_revenue, processor_clearing, arrears_liability, expired_credits, fx_liquidity, world}; system accounts per (merchant, currency), customer accounts per (merchant, customer, currency). RLS + merchant_isolation like every other table.
- [ ] New `openrails.ledger_transfers` (immutable): id, merchant_id, debit_account_id, credit_account_id, amount, currency, transfer_type, pending_id NULL, flags (pending|post_pending|void_pending|linked), source, source_id, user_data refs (customer_id, invoker_id, resource, invoice_id), created_at. Append-only — NO update/delete grant intent.
- [ ] CHECK/app-invariant: `debit.currency = credit.currency = transfer.currency` (a transfer never crosses ledgers).
- [ ] Sign-constraint flags on accounts (TB `debits_must_not_exceed_credits` / `credits_must_not_exceed_debits`) enforced in the applier: customer_balance may go negative only up to the arrears credit line.
- [ ] Conservation invariant job: per (merchant, currency) assert `Σ account balances == 0`; surface as an Integrity finding (ties into #511 I-plane).
- [ ] Map the existing flows to transfer pairs: deposit = DR processor_clearing / CR customer_balance; spend = DR customer_balance / CR platform_revenue; refund = reverse; expiry = DR customer_balance / CR expired_credits.

### B. Immutable ledger — derive, don't mutate
- [ ] Stop UPDATE on `money_transactions`; capture = new post_pending transfer linked via `pending_id`; void/refund = new transfers. (touch: `internal/db/gen/money_ledger.sql.go` writers, `money_service.go`)
- [ ] Replace `SetMoneyBlockRemaining` mutation + `DELETE FROM money_blocks` expiry: lots become immutable (`original_amount` only); remaining is derived from transfers; expiry is a transfer to `expired_credits`, never a delete. (touch: `withdrawBalanceAndBlocks`, `jobs_credit_expiry.go`)
- [ ] Drop `money_transactions.balance_after`; extend `deriveBalance` to be the only balance source.
- [ ] Decide lot model: keep `money_blocks` as an immutable lot index for FIFO/expiry vs. fold the lot dimension into `ledger_transfers`. (OPEN design decision)

### C. Simplify holds to the ephemeral Redis admission model (decision 8)
Goal: collapse the admission path to decision 8's five steps. Holds NEVER touch Postgres; the durable ledger write happens at capture, off the hot path. This SIMPLIFIES the current system, which today is heavier than decision 8 on three counts: capture (`CaptureAuthorized`/`spendBalanceThenOwedTx`) is a synchronous Postgres tx under the per-customer `FOR UPDATE` lock; balance is Postgres-derived under that lock on every spend (not memory-cached); and two extra reservation layers exist beyond the per-request hold.
- [ ] **Balance cache.** Serve admission from an in-memory (per-instance) cache of the Postgres-derived balance; refresh on TTL and invalidate/bump on deposit/top-up so a funded payer can spend immediately. Per-instance staleness is fine; the resulting over-spend is bounded.
- [ ] **Hold.** Keep the per-request hold in Redis keyed by the provider/tensorhub request-id (shared Redis, so every instance's admission sees the same holds); admit iff `cached_balance − Σ active holds ≥ estimate`; record the hold on admit. (touch: `pkg/service/spend.go` `AuthorizeAndHold`, `internal/modules/money/authorize.go`)
- [ ] **Capture.** On completion, deduct ACTUAL cost from the cache + release the hold, and persist the real spend to the durable ledger (Phases A/B) OFF the hot path — NOT today's synchronous per-request locked Postgres tx. Decide durability of the off-path write: a durable spend outbox (at-least-once) vs. accepting crash-loss of in-flight captures as part of the bounded leak.
- [ ] **Failure.** Release the hold, bill nothing, record money-wasted in Redis for rate-limit/abuse (supersedes durable wasted-spend bookkeeping where it exists, e.g. `delegated_invoker_wasted_spend_windows`).
- [ ] **No-overspend at admission, not capture.** The credit/balance limit is checked at admission against `cached_balance − holds` (arrears: `≤ credit_limit`); capture/persist faithfully records actual spend even if a stale cache let the balance go slightly negative. The negative is bounded by `(cache-staleness × spend rate) + (estimate − actual)` and is accepted — never ledger corruption, never unbounded. This deliberately drops today's synchronous capture-time credit-limit gate in `spendBalanceThenOwedTx`.
- [ ] **Map before cutting the heavier layers.** `money_windows` (prepaid BULK reservation a host meters locally, settled in batches) and `budget_inflight_holds` + admission delegated-spend windows (windowed rate/budget caps) exceed decision 8. For each: either justify it as a genuinely-distinct feature or fold it in. Don't delete blind. (touch: `internal/modules/admission`, `internal/modules/money/windows.go`)

### D. Separate ledger fields from control-plane attribution
- [ ] Move `invoker_id`/`resource`/`description`/`metadata`/`invoice_id` off the hot ledger row into opaque `user_data` columns (or one `user_data` jsonb) + control-plane joins by id. Ledger stays narrow and reusable.

### E. FX rule (bake in before multi-currency settlement)
- [ ] Enforce "no cross-currency in one transfer" (Phase A CHECK already covers it).
- [ ] When FX is first needed: add a per-merchant `fx_liquidity` account + a linked-transfer helper (two transfers, one per currency, atomic via `flags.linked`). Document the rule now; implement on first multi-currency demand. (custom credits remain no-FX by design — #475)

### F. Per-customer FOR UPDATE lock = the scaling tell
- [ ] No behavior change now. Add observability: per (merchant, customer) lock-wait/contention metric on `lockBalance`.
- [ ] Decision gate: if a hot customer/invoker saturates the lock, THAT is the trigger to evaluate running TigerBeetle (or a deterministic batched applier) for the money ledger — TB = money source of truth, Postgres = control plane, synced by transfer id / user_data. Document the trigger + integration sketch.

### G. Tests
- [ ] Conservation: random transfer sequences leave `Σ ledger == 0`.
- [ ] Immutability: no UPDATE/DELETE on ledger rows; capture/void/refund/expiry are new rows.
- [ ] Two-phase durability: a crash between authorize and capture leaves a recoverable pending transfer; expiry voids it; no double-spend.
- [ ] Migration: backfill `money_transactions`/`money_blocks` → accounts+transfers; assert post-migration derived balances equal pre-migration derived balances for every (merchant, customer, currency).

## Non-goals

- NOT running TigerBeetle-the-database in this issue (it is a separate stateful system + a two-phase sync with Postgres); only its data-modeling principles, in Postgres. Adopting TB is gated on Phase F.
- Do NOT regress the already-TB-aligned wins: integer minor units, idempotency keys, entitlements `tstzrange` exclusion windows, append-only `usage_events`, derived available balance (post-#491).

## References

- Source comparison: TigerBeetle (github.com/tigerbeetle/tigerbeetle) Account/Transfer/two-phase/linked-transfer model.
- Current code: `internal/modules/money/money_service.go` (`depositTx`/`withdrawTx`/`withdrawBalanceAndBlocks`/`deriveBalance`/`lockBalance`), `internal/modules/money/authorize.go` (`AuthorizeAndHold` Redis hold), `internal/db/gen/money_ledger.sql.go`, `internal/db/gen/money_accounts.sql.go` (settings only — misnamed), schema `migrations/postgres/001_schema.up.sql` (money_transactions/money_blocks/money_settings/money_windows/budget_inflight_holds).
- Integrity-invariant overlap: #511 (the conservation check is an I-plane finding).

---

# #511: unified-consistency-invariant-engine

**Completed:** no
**Status:** PLANNED 2026-06-17: design complete in `docs/consistency-invariants.md`; no code written yet. Replaces the two split consistency mechanisms — `internal/reconcile` (PS-1..PS-10, local-vs-processor) and `internal/audit` (P-E/S-E/SS, local-vs-local) — with ONE system: `reconcile` becomes a pure provider-state pull that overwrites the local mirror, plus a continuously-running **Convergence Engine** that drives every projection (entitlements / credits / product-access) and external decision into a consistent state with the source events, via a canonical **grants** layer.

Build the unified billing-consistency system designed in `docs/consistency-invariants.md`: a provider-truth **pull** (`reconcile`) and a **continuously-running Convergence Engine**, organized by the five-plane taxonomy — **M** Mirror (inbound: processor → local, the pull) / **D** Derivation (source → grant → projection) / **L** Lifecycle (clock + state machine) / **N** Intent (outbound: our recorded decision → the processor must change) / **I** Integrity-Rule (internal financial/referential rules). Subsumes and retires `openrails audit` and the `PS-1..PS-10` reconcile taxonomy.

## Design decisions (resolved 2026-06-17)

1. **Provider state is authoritative but may be incomplete w.r.t. future-dated intent.** The pull overwrites the local mirror with observed provider state, but NEVER deletes a standing local intent (`deletion_scheduled_at`, `scheduled_price_id`, pending `provider_intents`). The engine is schedule-aware: a not-yet-due scheduled change (cancel scheduled for Jun 28, pull on Jun 17 still sees the sub live at NMI) is fully consistent — expected lag, not drift. Divergence is a fault (N-plane re-drive) only when a scheduled intent is PAST-DUE and the provider has not reflected it.
2. **No enforce command/crank.** Convergence runs continuously while the server is up (inline after every source mutation + a background sweep). `reconcile pull` is a CLI used when the server may be down, so it pulls and then runs a one-shot `Converge` pass itself. No `--enforce` flag; `pull` always converges.
3. **Credit clawback pulls back UNSPENT only.** A fully-refunded payment's unspent credit remainder is clawed back automatically — post-#512 a reversing `ledger_transfer` to `expired_credits` with "unspent" derived from the ledger (not a `money_blocks.remaining_amount` mutation); the already-spent portion is left untouched (informational only).
4. **Default pull = the head (current state).** `reconcile pull` pulls current provider state by default; an optional `--since` / date range backfills historical transactions for replaying old projections (legacy import, audit completeness).
5. **Grants layer.** A canonical `grants` row sits between source events and projections (generalizing `product_access_grants`); derivation is two pure steps — derive-1 (event → grant) and derive-2 (grant → projection) — so the D-plane is uniform across entitlements/credits/access and the manual-override rule lives on the grant's `revoked_at`+`revoke_reason`.
6. **No "conflict" shape.** Shapes are MISSING/EXCESS/MISMATCH only (an exhaustive 2×2). A case the engine can't evaluate (authority unreachable or evidence ambiguous) is the `indeterminate` finding state — an evidence problem, never a truth-model fault.

## Two safety doctrines for bulk legacy import

- **Replay vs converge.** Projections are replayed at their historical source timestamps (a 2025-06-30 90-day membership is recreated already-expired). Side-effecting external actions (charges, rebills, dunning) are NEVER replayed — the Convergence Engine converges a record to its correct CURRENT state (dunning unrun for 3 months ⇒ cancel now + revoke as-of grace end), never retro-charges.
- **Confirmed-absence gate.** MISSING/materialize is additive ⇒ AUTO even in bulk. EXCESS/retract is destructive and is HELD until the relevant source domain is confirmed fully reconciled for that merchant (an imported entitlement with no subscription is "not imported yet", not "orphaned"); then ADMIN-gated, never silent.

## Relationship to #512 (double-entry ledger)

#512 reshapes the money **substrate** (credits become double-entry `ledger_accounts` + immutable `ledger_transfers`); this issue reshapes the **control plane** (what a customer is *owed*, via grants). They meet at credit grants:

- **A credit projection is a `ledger_transfer`, not a `money_blocks` row.** For a credit-granting grant, derive-2 emits a credit-deposit transfer (`world`/`processor_clearing` → `customer_balance`) tagged with the grant via #512's `source='grant'` / `source_id=grant_id`. So #511 does NOT add `grant_id` to `money_blocks` — the grant linkage rides on `ledger_transfers`.
- **Clawback (D5) is a reversing transfer, not a mutation.** A refund's unspent remainder is returned by appending a `customer_balance → expired_credits` transfer; "unspent" is *derived* from the ledger — matching #512's append-only + derive-don't-store principles.
- **Entitlements and product-access projections are unaffected by #512** (not money); they stay local rows derived from grants.
- **D-plane credit checks run against `ledger_transfers`:** every credit grant has its deposit transfer (D4), every credit deposit traces to a live grant (D5), amounts/cadence match the grant spec (D6).
- **Sequencing:** land #512's credit substrate before/with #511's credit derivation so credits aren't migrated twice. If #512 slips, #511's credit derivation targets the current `money_transactions` shape behind the same derive-2 interface and swaps substrate when #512 lands.

## Metadata

- Category: feature
- Status: planned
- Passes: false

## Work breakdown

### A. Schema, grants layer & findings ledger
- [ ] **Grants layer:** generalize `product_access_grants` into the canonical `grants` table that entitlements + credits + access all derive from; repoint `entitlements`; tag credit deposits with the grant (under #512 via `ledger_transfers.source='grant'`/`source_id`; pre-#512 a `grant_id` on `money_transactions`), replacing the free-string `source` — NOT a separate `money_blocks` migration (see **Relationship to #512**); migrate existing rows.
- [ ] Extend `reconciliation_findings`: admit the M/D/L/N/I `finding_type` codes (replace the `PS-1..PS-10` CHECK with the new set / registry validation); allow a `self` value in `provider` for D/L/I findings.
- [ ] Add the `held` status (+ reason `held_pending_source_reconciliation`, the confirmed-absence gate) AND the `indeterminate` status (authority unreachable/ambiguous — verify then escalate; never a truth-model fault).
- [ ] Add per-merchant reconciliation-state (table or columns): per source-domain `last_full_pull_at` + `fully_reconciled` flag, backing the confirmed-absence gate (per-domain granularity: subscriptions / payments / grants).
- [ ] Confirm intent fields suffice for schedule-awareness (`subscriptions.deletion_scheduled_at`, `scheduled_price_id`, `provider_intents.next_attempt_at`/`expires_at`); add a scheduled-effective-at marker if any transition lacks one.

### B. The pull (`reconcile` rework)
- [ ] Rename `reconcile fix` → `reconcile pull`; keep `check` (dry-run change log, zero writes, no convergence) and `report`.
- [ ] Rewrite pull semantics from "diff + selectively apply findings" to "pull complete head state per provider, OVERWRITE the local mirror, log every row old→new".
- [ ] Mirror coverage: subscription observed status/period/next-bill, charges→payments, refunds, chargebacks/disputes, vault→payment_methods, plan amount/cadence.
- [ ] Intent preservation in the overwrite: never clobber `deletion_scheduled_at`/`scheduled_price_id`/pending intents; compute divergence against (mirror + scheduled future transitions); flag only PAST-DUE intents.
- [ ] `--since` / date-range flag for historical backfill; default pulls the head.
- [ ] After a successful pull: mark the pulled source domains reconciled (gate) and run a one-shot `Converge(merchant)` pass.

### C. The Convergence Engine (new core)
- [ ] `Converge(scope)` — idempotent (second run is a no-op), scope-narrowable (customer | subscription | merchant | global).
- [ ] Plane passes run in order D → L → N → I (M is the pull); each plane is a module.
- [ ] Derivation is two pure appliers — derive-1 (event → grant) and derive-2 (grant → projection) — the SOLE writers of grants and projections; refactor existing grant/entitlement/credit paths to route through them.
- [ ] Historical replay anchored to source-event timestamps, judged against the grant's `*_spec_snapshot` (never live product spec).
- [ ] Converge-not-replay for the L-plane; confirmed-absence gate on every EXCESS repair.

### D. Invariant checks + appliers (catalogue, `docs/consistency-invariants.md` §4)
- [ ] D grant tier: D1 grant.missing (event→grant) / D2 grant.excess (gated) / D3 grant.mismatch — derive-1, uniform across entitlements/credits/access.
- [ ] D projection tier: D4 projection.missing (grant→projection) / D5 projection.excess (gated; credit clawback = unspent only) / D6 projection.mismatch (incl. per-renewal credit cadence) — derive-2; credit + product-access projections are NEW beyond entitlements. The credit projection is a #512 `ledger_transfer`; entitlement + access projections are local rows.
- [ ] L subscription period-overdue / dunning-overdue / grace-exhausted / pending-stale (L1-L4), intent-stuck (L5), checkout-session-stale (L6).
- [ ] N (Intent) billing-leak (N1), undelivered-decision re-drive (N2), duplicate-subscription (N3), dispute-unresolved (N4) — via `provider_intents` / operator queue.
- [ ] I duplicate-charge (I1), refund-math (I2), price/product (I3), payment-amount (I4), unresolved-reference (I6).

### E. Continuous convergence wiring
- [ ] Inline hooks: `Converge(customer|subscription)` synchronously after checkout completion, renewal billing, refund/webhook ingestion, dunning transitions, admin grant/revoke.
- [ ] Background sweep (River worker): periodic `Converge(merchant)` scanning for due lifecycle transitions, overdue intents, and gate-released held findings.
- [ ] Server-running convergence and the CLI one-shot share the same `Converge` core.

### F. Retire the split mechanisms
- [ ] Fold `internal/audit` checks into the engine as D/I-plane checks; remove/alias the standalone `openrails audit` command.
- [ ] Replace `internal/reconcile` PS-* finding types + selective-apply with the pull + M/D/L/N/I Convergence Engine; update the store, report rendering, CLI help.
- [ ] Update `README.md` billing section (audit/reconcile description, PS-1..PS-10 mention) to the new model once shipped.

### G. Legacy import
- [ ] Import flow leaves the per-merchant gate "not reconciled" until import + a full historical pull complete; bulk sweep entry point for a freshly imported merchant.
- [ ] Triage report for `held` findings (entitlements without sources, dunning long-overdue, …) before any destructive repair is approved.

### H. Tests
- [ ] Per-plane / per-code unit tests; idempotency (double-run no-op); historical replay (already-expired windows); converge-not-replay (no retroactive charges).
- [ ] Schedule-aware intent test (Jun 15 cancel / Jun 28 delete / Jun 17 pull is consistent).
- [ ] Confirmed-absence gate (held pre-reconciliation, fires after); credit unspent-only clawback.
- [ ] Migration test: the findings-ledger CHECK admits the new codes and old runs still load.

## References
- Design doc: `docs/consistency-invariants.md`
- Supersedes the operator-facing taxonomy work-item in `agents/future.md`.

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

- Stripe, NMI/Mobius, CCBill, and Solana have different lifecycle semantics. Customers need predictable errors and routing decisions. OpenRails also needs a shared capability source for routing/fallback, catalog-as-code, checkout validation, and the provider certification matrix.

**Tasks:**
- CAPABILITY MODEL:
- [ ] Define ProcessorCapabilities with booleans/enums for recurring, one_time, vault/tokenization, hosted checkout, redirect checkout, direct sale, catalog push, recurring plan push, refund, dispute, cancel immediate, cancel deferred, remote subscription listing, remote dedup check, webhooks, drift enumeration, and manual actions.
- [ ] Add capability details for current processors: stripe, mobius/NMI-backed, ccbill, solana.
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
