<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 517

---

# #514: grant-ledger

**Completed:** yes
**Status:** COMPLETE 2026-06-17: the live money path now runs on the grant ledger. `money_service.depositTx` records a deposit as a credit grant (`kind=credit`) + `MaterializeGrant` (→ #512 ledger deposit); the grant IS the FIFO credit lot (no `money_blocks`). Credit spend = `grants.CreditSpend` FIFO across lots (one #512 `credit_spend` transfer per lot, tagged `grant_id` for lot attribution + the caller's `source/source_id` for idempotency); expiry = `CreditExpiryWorker` → `grants.ExpireLapsed` (claws lapsed remainders to `expired_credits`); subscription/purchase credits + tier graduation (`SumCreditGrants`) all route through grants. Added a `grant_id` column to `ledger_transfers` (migration 002) so lot attribution and caller idempotency don't collide. `migrations/postgres/004_drop_single_entry.up.sql` drops `money_blocks` + `money_transactions`. Validated live: `internal/modules/grants` (7/7) + `internal/modules/money` (67/67) + `internal/modules/money/ledger` + `internal/river` (credit expiry) + `pkg/service` + `internal/modules/admission` all green; whole-repo `go build` + `go vet` clean. Refund clawback on revoke is DONE (revoke→`revoked_credits`, reversible; optional bundled refund via `RevokeGrant(grant,{refund})`; `revoke_clawback_integration_test.go`). No remaining implementable work; `supersede`/`adjust` appliers stay deferred until a proration/plan-change flow consumes them. [#513 cache-bump-on-deposit DROPPED: the #513 balance cache was removed on 2026-06-18 after #512 Phase H; fresh deposits are visible on the next direct ledger-account counter read.] | (foundation, earlier 2026-06-17) **grant-ledger foundation BUILT + integration-tested, 5/5 green.** Added `migrations/postgres/003_grants.up.sql` (append-only `openrails.grants` via explicit `REVOKE UPDATE/DELETE`, RLS, + extends `entitlements.source_type` with `grant`), sqlc queries `internal/db/queries/grants.sql` → `gen`, and `internal/modules/grants`: derive-1 (`Grant`/`Revoke`/`Expire`, the sole append-only writer) + derive-2 (`Materialize` → entitlement windows, ownership roster, and the **#514→#512 seam**: a credit grant emits a conserved ledger deposit). Integration tests on live Postgres — entitlement projection + historical replay (already-expired), revoke→retract + single-termination, credit→#512 deposit + conservation + idempotency, ownership, append-only — all pass. — The access-domain sibling of #512's money ledger — an **append-only grant ledger**. A grant is an immutable event ("source S grants customer C product P for [start,end), spec snapshot X"); revoke / expire / supersede / adjust are NEW events, never edits. The live **entitlement windows, product ownership, and credit lots are DERIVED grant effects** folded from the grant log, never the source of truth. Generalizes today's `product_access_grants` (already ~80% the shape) into the one layer entitlements + credits + ownership all derive from. Foundation of #511 (its DERIVE-plane derives from this) and feeds #512 (credit grants emit money transfers; the grant IS the credit lot).

Build the grant ledger: the access analogue of double-entry money. Money (#512) became append-only transfers with derived balances; access becomes append-only grants with derived grant effects — same discipline (immutable, derive-don't-store, replay-exact), so "who has access and why" is one auditable log and reconciliation / import / repair reduce to "re-derive the grant effects and diff."

## Why make these changes

Current model (verified 2026-06-17):
- **Access is authored by scattered code paths.** Checkout, renewal, dunning, admin-grant, and reconcile each write `entitlements` / `product_access_grants` / `money_blocks` directly, with heterogeneous source links (`entitlements.source_type`+`source_id` typed; `money_transactions.source` a free string; `product_access_grants` its own shape). There is no single record of "who got what, when, why" and no uniform way to re-derive or reconcile it.
- **Revocation is a mutation.** `revoked_at`/`revoke_reason` are UPDATEd in place — a drift surface that loses event history, exactly the mutation #512 removes from money.
- **The credit lot is a separate mutable table.** `money_blocks` (amount + expiry + remaining, FIFO) is precisely a grant of credits — but lives apart and is mutated/deleted on spend/expiry.

## Guiding principles (mirror #512)

1. **Append-only / immutable.** A grant row is never updated. Revoke / expire / supersede / adjust = NEW events referencing the original (`supersedes_id`).
2. **Derive, don't store.** Live entitlement windows, ownership rows, and credit balances/lots are grant effects folded from the grant log — rebuildable, never the source of truth.
3. **Single-entry issuance, NOT double-entry.** Access is not conserved (no counter-account): a grant asserts a right came into existence. Money IS conserved, so the ONE cross-over is that a **credit grant also emits a double-entry transfer** into #512's ledger.
4. **Spec snapshot on the grant.** Captures the product's `entitlements_spec` + `credits_spec` at issuance, so derive-2 (grant → grant effect) is a pure function and replay is exact + historical (a 2025-06-30 90-day grant reconstructs already-expired).
5. **Control-plane object; #512 ledger purity preserved.** The grant knows products/customers/windows; #512's transfers stay pure and reference the grant by id (`source='grant'`/`source_id`), per #512 decision 6.
6. **The grant IS the credit lot.** A credit grant carries amount + expiry + source — exactly a FIFO lot. Consumption + expiry operate over grants; `money_blocks` is subsumed (resolves #512's open lot-model decision).

## Metadata

- Category: architecture
- Status: planned
- Passes: false

## Work breakdown

### A. Grant schema & migration
- [x] New `openrails.grants`: id, merchant_id, customer_id, product_id, kind ∈ {entitlement, ownership, credit}, source_type ∈ {purchase|subscription|admin|grace}, source_id, payment_id NULL, event ∈ {grant|revoke|expire|supersede|adjust}, supersedes_id NULL, spec_snapshot jsonb, starts_at, ends_at NULL, amount NULL + currency NULL (credit lots), reason NULL, created_at. Append-only (GRANT SELECT,INSERT + REVOKE UPDATE/DELETE), CHECKs (event↔supersedes, credit↔amount/currency, valid window), unique single-termination index, RLS.
- [x] Kind modeling: one table + `kind` discriminator.
- [~] Migration parity: N/A under the pre-launch HARD CUT. `product_access_grants`, `money_blocks`, and `money_transactions` are dropped rather than translated; grant effects are produced by the new grant ledger path.

### B. derive-1 (event → grant) — sole writer of grants
- [x] `Grant` applier appends a grant event, dated to the event, snapshotting the spec. (Wiring it into the real callers — checkout/renewal/dunning/admin — is the cutover.)
- [x] Revoke / Expire appended as new events (never edits); single-termination enforced by unique index. (supersede/adjust events are schema-ready; appliers land with the tier-change/cutover work.)

- [x] `Materialize` folds the grant log into live entitlement windows (existing GIST-exclusion table) + the ownership roster (read off grants); rebuildable, re-derivation is a no-op (idempotency tested). Terminated grants retract their grant effects.
- [x] Credit grants emit a #512 deposit transfer (DR processor_clearing / CR customer_balance) tagged `source='grant'`/`source_id` — the seam is live + tested.
- [x] **Credit spend + expiry on the new model** (`CreditSpend` FIFO-by-expiry across lots with derived per-lot remaining → one ledger spend transfer per lot; `ExpireLapsed` claws each lapsed lot's unspent remainder to `expired_credits`). Tested (`TestGrants_CreditSpendFIFO`, `TestGrants_CreditExpire`): FIFO order, atomic over-spend rejection, idempotent expiry, conservation.
- [x] **Clawback on revoke — BUILT + integration-tested 2026-06-17.** `MaterializeGrant` on a terminated credit grant appends a reversing transfer for the unspent remainder `DR customer_balance / CR revoked_credits` (`clawbackRevokedCredit`); the new `revoked_credits` account type (migration 002 + `ledger.RevokedCredits`) keeps admin-revoke distinct from lapse. Idempotent via `GetCreditLotRemaining` (nets out prior `credit_revoke`). The revoke event already drops the lot from `ListSpendableCreditLots`, so the clawback only reconciles the *derived balance*; money is **frozen** in `revoked_credits` (not refunded) and reversible. `grants.Ledger.RevokeGrant(grantID, reason, refund) (clawed int64, …)` = revoke event + materialize + optional refund leg `DR revoked_credits / CR processor_clearing` (the `refund` flag is the operator authorization; the actual external provider refund is the caller's, using the returned clawed amount). Tests (`internal/modules/grants/revoke_clawback_integration_test.go`): `TestGrants_RevokeClawback` (spent-30/claw-70, balance→0, revoked_credits=70, conservation, idempotent), `…Reversible` (counter-transfer restores), `…WithRefund` (revoked_credits drained → processor_clearing nets the deposit). Whole-repo `go build`+`go vet` clean. (Convergence-engine policy home = #511 `derive.grant_effect.excess`; see `docs/consistency-invariants.md` §11 (decision 4).)
- [~] On a credit-grant deposit, bump the #513 admission balance cache — DROPPED/OBSOLETE. As of 2026-06-18 the cache is gone; admission reads the O(1) ledger account counters directly, so a fresh deposit is visible on the next admit read.

### D. Tests
- [x] Append-only: app role denied UPDATE/DELETE on grants; revoke/expire are new rows (`TestGrants_AppendOnly`, `TestGrants_Revoke`).
- [x] Replay/idempotency: re-deriving a grant is a no-op (`TestGrants_EntitlementProjection`, `TestGrants_CreditDepositSeam`).
- [x] Historical replay: a 2025 grant rebuilds as an already-expired window (`TestGrants_EntitlementProjection`).
- [x] Grant-as-lot FIFO consumption + lapsed expiry (`TestGrants_CreditSpendFIFO`, `TestGrants_CreditExpire`).
- [~] Migration parity: N/A — pre-launch HARD CUT, no data migration (decided 2026-06-17, Paul). The flip just drops the old tables.

## Relationship
- **#511 (Convergence Engine)** derives its DERIVE plane from this ledger; derive-1/derive-2 live here and are the sole writers of grants + grant effects; #511 invokes + verifies them.
- **#512 (money ledger)** receives the deposit transfer a credit grant emits; the grant is the credit lot (resolves #512's open lot-model decision).
- **#513 (admission)** balance cache was removed 2026-06-18 after Phase H; fresh credit deposits are visible on the next direct O(1) ledger account read — no explicit deposit-bump from this layer.

---

# #513: admission-and-holds-redis-simplification

**Completed:** no
**Status:** IN_PROGRESS 2026-06-18: OpenRails side of #513 is wired to Redis spendgate with direct O(1) ledger admission capacity and cached policy config. Balance cache was deleted after #512 Phase H. Remaining tracker tail is outside this repo: tensorhub test compile cleanup/version bump called out below.
**Update 2026-06-18:** balance cache REMOVED after #512 Phase H made balance an O(1) account-counter read. Admit now does a direct `GetAdmissionCapacity` point lookup (balance + held + billing mode + credit limit) and then the same Redis `spendgate` EVAL. Policy config cache stays. There is no `admission.BalanceCache`, no runtime field, and no `CachedBalance` spendgate input anymore.

The north star is decision 8 of #512 (the five-step model: balance-check → Redis hold → capture actual → release; failures record wasted-spend in Redis). This issue generalizes that to the *whole* admission system, including budget/spend caps and rate windows.

## Why (the current hot path is heavy and muddled)

Traced in `internal/modules/admission/admitter.go` `Admit` — one DELEGATED admit does:
1. `money.GetTier` → Postgres read.
2. `policies.GetTierPolicy` → Postgres read (cacheable).
3. FX convert estimate → policy currency (`internal/integrations/fx`).
4. wasted-spend gate → **Redis** (`WastedSpendGuard` on `ratelimit.Limiter`) — already efficient ✅.
5. `buildBudgetScopes` → Postgres reads of `budget_policies`.
6. `budgets.ReserveScopes` → `budgets.Reserve` → **Postgres tx, `budget_window_state` locked `FOR UPDATE`, INSERT `budget_inflight_holds`**.
7. `money.AuthorizeAndHold` → **Postgres tx, `customers` row locked `FOR UPDATE`, derive balance by SUM(`money_blocks`)+`money_windows`** → then the Redis hold.

So one admit = ~3 Postgres reads + **two Postgres write-txns each taking a `FOR UPDATE` row lock**; capture (`CaptureAuthorized`/`spendBalanceThenOwedTx` + `budgets.CaptureByCoords`) adds more locked txns. The two row locks (customer row + window-state row) serialize a payer's concurrent requests — the throughput ceiling.

Two structural problems:
- **Policy and accounting are muddled.** Policy = read-mostly config (what the caps/tiers are: `money_settings`, `money_spend_limits`, `budget_policies`, `tier_policies`, `tier_schedules`). Accounting = hot counters (how much used this window: `budget_window_state`, `budget_inflight_holds`, balance derivation, the Redis hold, the Redis wasted-spend). Accounting belongs in Redis; policy belongs in a versioned in-memory cache. Today budget accounting is the one axis still in Postgres-with-locks while throughput + wasted-spend already ride Redis.
- **The cap taxonomy has ~4 overlapping shapes** for "max $X over window W for scope S": `money_settings.max_spend_per_day/month`, `money_spend_limits` (per invoker), `budget_policies` (hierarchical scopes), `tier_policies` (per-tier windows). There also appear to be TWO generations of spend-cap code: the newer `budget_policies`/`tier_policies` path in `admitter.go` and an older `money_settings`/`money_spend_limits` path in `spend_policy.go` (`CheckSpendAllowed`/`getSpendLimit`).

## Guiding principles

1. **Policy is cached config; accounting is Redis counters.** The hot path does one O(1) Postgres capacity/config read on misses, but no Postgres locks or write-side budget reservations.
2. **One windowed-cap primitive.** Reuse the `ratelimit.Limiter` fixed-window Redis counter that throughput + wasted-spend already use; every cap (throughput / budget-$ / wasted-$) becomes the same counter.
3. **Atomicity without locks.** Admit and capture are each a single Redis `EVAL` (Lua), atomic by Redis's single thread — no `FOR UPDATE`, no races.
4. **Bounded over-admission is acceptable** (decision 8, #512): Redis/cache loss relaxes caps temporarily; the worst case is bounded over-spend, never ledger corruption. The durable truth is the #512 ledger, written off the hot path.
5. **Simplify aggressively, but map before cutting** — confirm each retired mechanism is dead/covered before deleting.

## Metadata

- Category: architecture
- Status: planned
- Passes: false

## Work breakdown

### A. Unify windowed caps onto the existing Redis limiter
- [ ] Move money-budget windows off `budget_window_state` + `budget_inflight_holds` (Postgres, `FOR UPDATE` per request) onto `ratelimit.Limiter`. Reserved (holds) + used (captured) become two counter contributions per (scope, window) key, TTL = window. (touch: `internal/modules/budgets/*`, `internal/modules/admission/admitter.go`)
- [ ] Delete `budgets.Reserve/ReserveScopes/Capture/Release/CaptureByCoords` locked-tx machinery once windows are Redis-backed.
- [ ] Drop tables `budget_window_state` and `budget_inflight_holds` (migration); window history that reporting needs comes from the durable spend events (#512 ledger), not these.

### B. Collapse the cap taxonomy to one shape
What the "spend-policy cap" IS (for the record): a TIME-WINDOWED ceiling on cumulative ACTUAL spend — per-customer max spend per UTC calendar DAY + per MONTH (`money_settings.max_spend_per_day/month`), the same per-invoker (`money_spend_limits`), plus an arrears OUTSTANDING-owed ceiling (deny codes `daily_cap`/`monthly_cap`/`invoker_*_cap`/`outstanding_cap`). It is NOT a concurrent-in-flight-hold cap — it bounds spend velocity, orthogonal to the balance gate ("even with $10k, this API key can't burn >$100/day").
VERIFIED DEAD 2026-06-17: the `spend_policy.go` enforcement (`AuthorizeSpend` → `CheckSpendAllowed` → `evaluateSpend`) has ZERO live callers — no HTTP route, nothing. `admitter.Admit` never reads it; day/month/outstanding caps are now expressed via `budget_policies`/`tier_policies` windowed scopes. `money_spend_limits` is still WRITTEN (`SetSpendLimit` API wired) but enforced nowhere on the hot path.
- [ ] One policy shape: a list of `{scope, window, limit}`, scope ∈ {payer, invoker, role, tier}; effective policy = merge of matching scopes, DENY if any window over. Tiers just *name* a policy. Day/month become fixed-calendar windows; outstanding-owed stays a balance-side ceiling.
#### B1. DECIDED 2026-06-17 (Paul): delete the dead spend-limit feature in full. It is a MIX of dead + live-shared, so surgical, not a wholesale file delete:
- [x] Dead-only logic — DELETED 2026-06-17: `pkg/service/spend.go` `AuthorizeSpend`/`AuthorizeSpendRequest`/`AuthorizeSpendResult` + `SetSpendLimit` (service wrapper); `internal/modules/money/spend_policy.go` `CheckSpendAllowed`, `evaluateSpend`, `capInput`, `CapResult`, `dayWindow`, `monthWindow`, `SetSpendLimit`, `getSpendLimit`, `spentInWindow`, `activeHoldsTotal`, `ActiveHeldForCurrency`, and the cap deny-codes `DenyInvokerDailyCap`/`DenyInvokerMonthlyCap`/`DenyDailyCap`/`DenyMonthlyCap`/`DenyOutstandingCap`.
- [x] PRESERVED the live-shared bits 2026-06-17: kept `BillingModePrepaid`/`BillingModeArrears` consts + `SpendDecision` in `spend_policy.go` (no relocation needed — the file still hosts the live account-settings code). `SpendDecision` trimmed to `{Allowed, HardStop, DenyCode}` — the only fields `authorize.go`/`admitter.go` set or read; dropped `Caps`/`AlertCode`/`RetryAfterSeconds`/`NextAllowedAt`.
- [x] `pkg/service/spend.go` `DenyInsufficientBalance` REMOVED 2026-06-17 (had only test callers; live deny is `money.DenyInsufficientBalance`). Updated `authorize_and_hold_integration_test.go` to reference `money.DenyInsufficientBalance`.
- [x] DONE 2026-06-17 (unblocked once the #512/#514 agent finished): added forward migration `005_drop_money_spend_limits.up.sql` (`DROP TABLE IF EXISTS openrails.money_spend_limits`, mirroring their `004_drop_single_entry` pattern; `001` baseline + the `merchant_aware_schema_test.go` table list left intact, same convention they used for `money_blocks`/`money_transactions`).
- [x] DONE 2026-06-17 — removed the `money_spend_limits` queries from `money_accounts.sql` (`Get/Upsert`) + `merchant_lifecycle.sql` (`Count/Purge`) and ran `sqlc generate` (vet DB built from migrations via `scripts/sqlc-vet-db.sh` on the compose Postgres). Dropped the gen methods/structs (`Get/Upsert MoneySpendLimit`, `Count/Purge MerchantRowsMoneySpendLimits`, `gen.OpenrailsMoneySpendLimit`) + the already-orphaned `SumSpentInMoneyWindow`. NOTE: the same regen SYNCED the #512/#514 agent's stale staged gen (`grants.sql.go`/`ledger.sql.go`/`money_ledger.sql.go`/`models.go`) to their current `.sql` — whole-repo `go build` + integration-tag `go vet` green across grants/ledger/money/admission/merchants/service, so the sync is correct (it's what CI's `sqlc generate + diff` enforces).
- [x] Removed the `money_spend_limits` cascade entries from `internal/merchants/delete.go` (the list + count + purge switch arms) 2026-06-17. The `merchant_lifecycle.sql` query removal + regen is folded into the STAGED sqlc pass above.
- [x] Deleted the dead tests 2026-06-17: `internal/modules/money/spend_policy_eval_test.go`, `spend_policy_integration_test.go`; pruned the two dead `AuthorizeSpend` funcs in `pkg/service/spend_integration_test.go` (kept `TestGetCreditAccount_Snapshot`). `money_in_integration_test.go`/`reconcile_integration_test.go` had NO dead refs (the original note was off). Re-homed the shared `strptr` helper (was defined in the deleted `spend_policy_integration_test.go`, used by 6 money tests) to money `main_test.go`.
- [x] No HTTP route exposes `SetSpendLimit`/`AuthorizeSpend` (re-verified — only test callers).
- [x] After removal: `go build ./...` + integration-tag `go vet` clean 2026-06-17. The per-invoker spend cap it provided is covered by `budget_policies` scope=invoker windows (Phase A/B unified policy).
- [ ] (optional) A "max concurrent in-flight $" cap, if ever wanted, drops in as a gauge-type window (current held, not a time-sum) on the same primitive; today concurrency is only implicitly bounded by available balance.

### C. Cache policy config; read balance directly
- [~] Balance cache REMOVED 2026-06-18 per Paul after #512 Phase H: balance/held are O(1) account-counter reads, and admission uses one `GetAdmissionCapacity` point lookup instead of a staleness-tolerant cache.
- [x] Policy cache: load `tier_policies`/`budget_policies`/tier ladders into memory, invalidate on the existing `policy_version`/`schedule_version`/`config_version` bumps. (These version columns already exist — they were built for this.)
- [ ] Tier resolution from cache, not `money.GetTier` Postgres read; tier graduation stays a background recompute (on deposit), it just selects the policy.

### D. One atomic admit, one atomic capture (Lua)
BUILT + INTEGRATION-TESTED 2026-06-17 (8/8 green against real Redis via testcontainers): `internal/modules/admission/spendgate/` (`policy.go` + `gate.go` + `gate_integration_test.go`) — the `{scope,cadence,duration,limit}` model + `EffectiveWindows` merge + 3 `redis.NewScript` scripts (admit/capture/release) + `Gate.Admit/Capture/Release/HeldAmount`. Decisions baked in (Paul, 2026-06-17):
- **Direct O(1) account balance, not Redis `:bal`.** `Admit` takes the caller's ledger snapshot as `AccountBalance` ARGV; the affordability gate is `accountBalance − held − cost ≥ −creditLimit`. Only the shared `held` gauge + per-request `hold:<reqID>` + window counters live in Redis. NO `:bal` key, NO balance-refresh job/cache.
- **Anchored, estimate-based windows.** Cadence = session|fixed (per-user-anchored, #337); the Lua stores the per-(payer,scope,window) anchor in Redis and buckets relative to it (`floor((now−anchor)/dur)` for fixed; session = one key, TTL=dur set at open, never refreshed). Windows count the ESTIMATE; **capture does NOT true the window up to actual** (only the caller-side balance/#512 ledger trues up); **release frees the estimate from the windows**. Tested: prepaid + arrears-credit floor, fixed-window reset across a bucket boundary (fake clock), idempotent replay, multi-scope deny-on-any, capture-keeps-window vs release-frees-window.
- **Wire contract:** clean break + lockstep (Paul) — the eventual admit/capture/release HTTP+SDK shapes may change freely; tensorhub + embedders move in lockstep.
Still TODO for the wire: `held`-gauge reconciliation sweep. DONE since the original draft: Postgres→`spendgate.Policy` loader (tier_policies + budget_policies → scoped windows, carrying cadence), direct O(1) admission capacity read, and the admitter/`Service.{Admit,CaptureHold,ReleaseHold}` rewire that deletes `internal/modules/holds` + the `budgets` Postgres machinery + `money.AuthorizeAndHold`'s capacity read.

**WINDOW-ANCHORING GAP — found AND RESOLVED 2026-06-17.** The original draft `spendgate` bucketed windows by GLOBALLY-ALIGNED time, but the live budgets engine (`internal/modules/budgets/budgets_service.go`, #337) uses PER-USER-ANCHORED windows (each opens at the payer's first charge; staggered resets; 5h session / 7d fixed — SHIPPED behavior tensorhub/cozy-art rely on). FIXED in the rebuilt `gate.go`: the Lua stores the per-(payer,scope,window) anchor in Redis and buckets relative to it (`floor((now−anchor)/dur)` for fixed; session = one TTL-bounded key opened at first reserve). The integration test exercises the fixed reset across a bucket boundary. Cadence (session|fixed) maps straight from `BudgetWindowPolicy.Cadence`; the loader (TODO) carries it through. Per Paul, windows are estimate-based (no per-bucket capture true-up), which is what made the faithful port tractable. Until the wire lands, the budgets Postgres path stays the live engine.

SUBSUMES (hard-cut: delete on switch-over, do NOT coexist): `spendgate` replaces THREE current mechanisms that today split one decision — (1) the live Redis hold store `internal/modules/holds` (`Place`/`Release`/`ActiveAmount`, used by `pkg/service/service.go` + `admission.go`); (2) the Postgres money-capacity calc in `money.AuthorizeAndHold`; (3) the `budgets` Postgres window path (Phase A). All three collapse into the one Lua gate. After the cut, `internal/modules/holds` is deleted and `AuthorizeAndHold`'s capacity read is gone — there must NOT be two Redis-hold implementations (`holds.Store` + `spendgate`) running side by side.
- [x] Admit = one `EVAL`: check `account_balance − Σ holds ≥ cost` AND every window `used + reserved + cost ≤ limit`; if all pass, place the request-id hold + bump reserved. One Redis round trip after the O(1) DB capacity/config reads.
- [ ] Capture = one `EVAL`: move reserved→used by ACTUAL cost, drop the hold, and enqueue the durable spend (off hot path). 
- [ ] Fail/release = one `EVAL`: drop hold + reserved, bump the wasted-spend counter for abuse.
- [x] Holds in SHARED Redis keyed by the provider/tensorhub request-id (cross-instance agreement); no balance cache remains.

### E. Durable-spend seam (to #512, off the hot path)
RESOLVED 2026-06-17 (Paul): crash-loss of in-flight captures is acceptable — NO durable outbox. Keep it simple.
- [x] Capture releases the Redis hold and calls the #512 durable ledger writer (`CaptureAuthorized`). No balance cache or background drainer remains; no at-least-once outbox/WAL was added.

### F. money_windows (prepaid bulk reservation) — DELETE (verified zero consumers)
VERIFIED 2026-06-17: `money_windows` is API-exposed (#335: `POST /v1/service/credits/windows`, `/:id/refill`, `/:id/close`, settle) but NO consumer uses it — grep of tensorhub, doujins, cozy-art (incl. cozy-art's embedded in-process calls) found zero `OpenWindow`/`credits/windows` usage. Under the hard cut it's a dormant public API → remove it entirely, don't re-express it.
- [ ] Delete the credit-window API: routes (`credits.POST /windows`, `/:id/refill`, `/:id/close`, settle), handlers (`internal/http/handlers/service_credit_window.go`), service (`pkg/service/windows.go`), money methods (`OpenWindow`/`RefillWindow`/`CloseWindow`/`SettleWindowItems` in `internal/modules/money/windows.go`).
- [ ] Drop the `money_windows` table (forward migration) + its generated queries/`HeldBalance`-from-windows logic in `deriveBalance`/`SumActiveMoneyHeld` (windows were a `HeldBalance` source; once gone, held = spendgate Redis only).
- [ ] `go build ./... && go vet ./...` clean; no orphaned `MoneyWindow` refs.

### G. Tests
- [ ] Admit/capture/release atomicity under concurrency (Redis EVAL, no lost updates, no double-spend).
- [ ] Cap-merge correctness across payer/invoker/role/tier scopes; deny when any window over.
- [~] Balance-cache staleness bound + deposit invalidation — N/A after 2026-06-18 cache deletion; funded payer is visible on the next direct O(1) ledger-account read.
- [ ] Redis-loss behaviour: caps relax, over-admission stays bounded, ledger stays correct.
- [ ] Migration: `budget_window_state`/`budget_inflight_holds` removed; in-flight reservations drained or expired cleanly.

## What stays (don't lose) — and a VERIFIED inventory of the three protections (2026-06-17)

The three protections spend limits exist for, mapped to their LIVE system (all SEPARATE from the dead `spend_policy.go`):
1. **Abuse (request velocity / low-trust)** — LIVE: edge rate-limit middleware mounted globally (`internal/http/server.go:473` `ginmw.RateLimitWithChallengeStore`, per-IP/route + captcha + attack-mode), card-abuse guard (`abuse/card_abuse.go`), velocity guard (`abuse/velocity.go`), wasted-spend guard (`abuse/wasted_spend.go`, in `admitter.Admit`), and the low-default tier for new accounts (#300).
2. **Fairness (rich payer can't crowd out others)** — RESOLVED 2026-06-17, NOT openrails' job: fleet fairness is owned by the CONSUMER'S SCHEDULER, not openrails. Verified in tensorhub (`~/cozy/tensorhub/internal/orchestrator/grpc/fairness.go`): a work-conserving weighted-fair-queue dispatches the shared GPU fleet — L1 per-payer / L2 per-user nesting, a per-payer in-flight **$** soft cap that scales by platform tier, weighted by the OpenRails hold ESTIMATE (#486), never idles a slot, never rejects. OpenRails owns the HARD money decision (affordability/hold) + $-budget windows + abuse; tensorhub owns scheduling/reordering and the live in-flight tally. This is the right split — openrails can't fairly schedule a fleet it doesn't see. CONSEQUENCE: openrails' throughput/concurrency surface (`ThroughputPolicy` written-not-read, `AcquireConcurrency` zero callers, the `ratelimit/concurrency.go` primitive) is dead-on-PURPOSE — tensorhub explicitly stopped sending throughput units (`invoke_admission.go:66`). Do NOT wire it; REMOVE it (same treatment as `spend_policy.go`), and document the contract: openrails = money truth + $-budget + abuse; the consumer's scheduler owns fleet fairness. The edge per-IP limiter stays as the DoS backstop for schedulerless consumers (doujins/cozy-art/direct API).
3. **Delegated spend-control (cap what another invoker spends + how often)** — LIVE: `budget_policies` scope=invoker/role/invoker_tier windowed `$` caps, built in `buildBudgetScopes` and reserved in `admitter.Admit`. This is strictly MORE general than the dead per-invoker day/month caps. (Subject-wide windowed cap = the payer's own velocity limit, distinct from balance: exists as config `ScopeSubject`/tier BudgetWindows but `buildBudgetScopes` skips subject — verify whether it's enforced anywhere on admit, or fold it into the one shape.)

Keep through the simplification:
- Hierarchical delegated-spend SEMANTICS (an invoker may spend the payer's money up to a cap) — keep; only the accounting + policy-shape change.
- FX conversion for cross-currency policy comparison (`internal/integrations/fx`) — pure function on the estimate.
- Tier graduation from cumulative paid (`tier_schedules`) — background; selects the policy.
- Wasted-spend abuse gate — already Redis; it becomes one consumer of the unified primitive.
- [x] REMOVE the dead fairness/throughput surface from openrails (NOT wire it — see protection 2): DONE 2026-06-17. Deleted `ratelimit/concurrency.go` (`AcquireConcurrency`/`ReleaseConcurrency`/`ConcurrencyCount`, zero callers) + `ratelimit/units.go` (#472 weighted queue pools `AcquireUnits`/`AcquireQueue`/`ReleaseQueueByRequest`, zero callers) + both integration tests; renamed `models.ThroughputPolicy` → `models.TierMoneyPolicy` (the misnomer carried no throughput axis). `AdmitRequest` had no throughput-unit field to drop. Fleet fairness is the consumer scheduler's job (tensorhub WFQ). `valuewindow.go` (wasted-spend `AddWindowValue`/`WindowValue`/`ClaimOnce`) + `limiter.go` `Check` (edge rate-limit) are LIVE and kept.

## Non-goals

- Not changing WHAT the caps mean or the delegated-spend model — only where state lives and how it's checked.
- Not building the durable ledger here (that's #512); this issue only enqueues to it.

## HARD CUT — no legacy, no backward compatibility (decided 2026-06-17, Paul)

Move to the new system completely; do NOT leave old code or compat shims around.
- NO feature flag toggling old-vs-new admission; NO dual-write / shadow mode; NO parallel "Postgres-budget AND Redis-budget" running side by side. One path, the new one.
- DELETE, don't deprecate: `budget_window_state`, `budget_inflight_holds`, the `budgets.Reserve/ReserveScopes/Capture/Release` locked-tx machinery, `spend_policy.go` enforcement + `money_spend_limits`, and the dead throughput/concurrency surface (`ThroughputPolicy`, `ratelimit/concurrency.go`) all go.
- NO on-the-wire backward-compat window on the admit/capture contract: if the API shape changes, change it outright. Consumers (tensorhub, doujins, cozy-art) move in LOCKSTEP via a coordinated openrails version bump — same model as the #403/#404 hard cut that already deleted the legacy broker.
- Exit gate: after the cut, `grep` finds no reference to the deleted tables/types/functions anywhere (no orphaned readers, no commented-out fallbacks); `go build ./... && go vet ./...` clean.

## Lockstep cutover (consumers) — verified 2026-06-17

Hard cut = every consumer moves to the new contract in ONE coordinated openrails version bump; no compat window, and APIs with zero consumers are deleted outright. Verified coupling + surface each consumer touches:

- **tensorhub** — standalone HTTP/SDK client (openrails SDK pinned, currently v0.36.0). Calls the USAGE surface: `AdmitHold` (admit), capture-on-job-result, `ReportWastedSpend`, `/credits/deposit`, `credit-types`, `/billing/v1/self/*`. **Most affected** — the admit/capture/wasted contract changes under #513. Lockstep: bump SDK, update the admit/capture/release/wasted call sites in `internal/orchestrator/http/invoke_admission.go` + `internal/billing/openrailsclient`.
- **doujins** — EMBEDS openrails in-process (`go.mod` `open-rails/openrails v0.39.0`; mounts routes in `internal/server/server.go` + `internal/di/builder.go`). Uses the SUBSCRIPTION surface, not admit/holds/windows/spend-limits (grep clean). Lockstep: bump the module dep + recompile — a compile break is the signal if it touches any removed symbol; subscription API is unchanged by #512/#513.
- **cozy-art** — EMBEDS openrails in-process; frontend hits `/billing/v1/me/subscriptions|payments|payment-methods`, `/checkout`, `/products`, `/stripe/portal`. Subscription-only; does NOT touch admit/holds/credits. Lockstep: bump the module dep + recompile; frontend endpoints unchanged.
- **Zero-consumer APIs being deleted — break nobody:** the credit-window API (#335 / `money_windows`) and the spend-limit API (`SetSpendLimit`/`money_spend_limits`) have NO consumer in any of the three repos → delete with no consumer migration.
- **#512 (ledger) is internal** — the consumer-facing deposit/charge/balance/subscription wire shapes (amounts already minor units) should NOT change; verify the API responses are byte-stable so #512 needs no consumer change beyond the embedded-dep bump.

## Efficiency (before → after)

- Admit: ~3 PG reads + 2 PG txns w/ `FOR UPDATE`  →  1 Redis `EVAL` + in-memory policy/balance.
- Capture: 1–2 locked PG txns  →  1 Redis `EVAL` + async durable enqueue.
- Per-payer concurrency: serialized by 2 row locks  →  lock-free.
- Cap surface: 4 policy shapes + 2 PG accounting tables  →  1 policy shape + 1 Redis counter primitive.

## References

- Current code: `internal/modules/admission/admitter.go`, `internal/modules/budgets/{budgets_service,scopes}.go`, `internal/modules/abuse/wasted_spend.go` (the Redis pattern to copy), `internal/modules/money/{authorize,unified_spend,windows,spend_policy}.go`, `pkg/service/{admission,spend,windows}.go`.
- Tables: `budget_window_state`, `budget_inflight_holds`, `budget_policies`, `tier_policies`, `tier_schedules`, `money_spend_limits`, `money_settings`, `money_windows`.
- Relationship: realizes decision 8 of #512 across the whole admission path; #512 Phase C defers here for the admission/holds detail.

---

# #512: double-entry-immutable-ledger-reorg

**Completed:** yes
**Status:** COMPLETE 2026-06-18: HARD CUT done, including Phase H. The live money path is fully on the double-entry ledger; account balance/held reads are O(1) `ledger_accounts` counters maintained from immutable `ledger_transfers`; admission capacity is a direct point lookup; the #513 balance cache is gone.
**Update 2026-06-18:** Phase H is BUILT + tested. `ledger_accounts` now carries TB-style `{credits,debits}_{posted,pending}` counters maintained by the `ledger_transfers` insert trigger; `LedgerAccountBalance`/`Held` and `checkDebitFloor` are O(1) counter reads; `GetAdmissionCapacity` returns balance + held + billing mode + credit limit in one query; `admission.BalanceCache` and all runtime wiring/tests were deleted per Paul. Validation: `task sqlc`; `go test ./...`; `go vet ./...`; `go build ./...`; integration `money/ledger TestLedger_*`, `admission TestAdmitter_*`, and service-admit HTTP E2E all green.

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
5. **Holds stay ephemeral (Redis); the bill is durable (Postgres).** The admission hold is a throwaway Redis reservation keyed by the provider/tensorhub request-id; the hot path writes NO Postgres and reads capacity from O(1) ledger account counters. The durable double-entry ledger (Phases A/B) is written at/after capture. Slight over-spend from concurrency is acceptable and bounded (decision 8).
6. **Ledger purity.** Transfer rows carry accounts/amount/currency/type/pending_id/flags/source+source_id + opaque `user_data` refs only; business joins live in control-plane tables.
7. **Keep what already matches TB:** integer minor units (no floats), idempotency keys, entitlements `tstzrange` GIST-exclusion windows, append-only `usage_events`.
8. **Target admission model (UPDATED 2026-06-18, Paul).** The whole money admission path is exactly this and nothing more: (a) request costs $X; (b) admit iff `account_balance − Σ active Redis holds ≥ $X`; (c) on admit, record an $X hold in Redis keyed by the request-id; (d) on completion, deduct ACTUAL cost and release the hold; (e) on failure, release the hold and bill nothing, but record money-wasted in Redis for rate-limit/abuse. The balance is an O(1) Postgres account-counter read, not an in-memory cache. No-overspend is an ADMISSION-time check, not a capture-time Postgres gate; bounded over-admission under concurrency is accepted.

## Metadata

- Category: architecture
- Status: planned
- Passes: false

## Work breakdown

### A. Double-entry account + transfer foundation
- [x] New `openrails.ledger_accounts` (id, merchant_id, customer_id NULL, account_type, currency, flags, created_at). account_type ∈ {customer_balance, platform_revenue, processor_clearing, arrears_liability, expired_credits, fx_liquidity, world}; system accounts per (merchant, currency), customer accounts per (merchant, customer, currency). RLS + merchant_isolation like every other table.
- [x] New `openrails.ledger_transfers` (immutable): id, merchant_id, debit_account_id, credit_account_id, amount, currency, transfer_type, `phase` (posted|pending|post_pending|void_pending), pending_id NULL, source, source_id, user_data refs (customer_id, invoker_id, resource, invoice_id), created_at. Append-only enforced by `REVOKE UPDATE/DELETE`. (`linked` FX flag deferred to Phase E.)
- [x] CHECK/app-invariant: `debit.currency = credit.currency = transfer.currency` (a transfer never crosses ledgers) — enforced by the `ledger_transfers_currency_guard` BEFORE-INSERT trigger.
- [x] Sign-constraint flags on accounts (TB `debits_must_not_exceed_credits` / `credits_must_not_exceed_debits`) enforced in the applier: customer_balance may go negative only up to the arrears credit line (`AllowDebitNegativeUpTo` floor).
- [x] Conservation invariant: per (merchant, currency) assert `Σ account balances == 0` — `Ledger.Conservation()` / `LedgerLedgerNet` query + integration test. This is a ledger diagnostic/smoke check; the double-entry transfer model makes it structural, not a scheduled convergence finding.
- [x] Map the existing flows to transfer pairs: deposit = DR processor_clearing / CR customer_balance; spend = DR customer_balance / CR platform_revenue; expiry = DR customer_balance / CR expired_credits; two-phase authorize/capture/release. (Implemented as `Ledger` flow constructors; wiring them into `money_service` is Phase B.)

### B. Immutable ledger — derive, don't mutate
- [x] Stop UPDATE on `money_transactions` — the table is GONE (migration 004); capture/void/refund are new transfers. `money_service`/`unified_spend`/`arrears`/`invoice`/`windows` all emit `ledger_transfers`.
- [x] Replaced `SetMoneyBlockRemaining` + `DELETE FROM money_blocks`: lots are immutable #514 credit grants; remaining is derived; expiry/clawback are transfers (`ExpireLapsed`→`expired_credits`, revoke→`revoked_credits`), never a delete.
- [x] Dropped `money_transactions.balance_after`; `deriveBalance` (ledger customer_balance net) is the only balance source.
- [x] Lot model — RESOLVED by #514: the lot = the credit grant; `money_blocks` subsumed; deposit transfer references the lot via `grant_id`.

### C. Simplify holds to the ephemeral Redis admission model (decision 8) → MOVED TO #513
The full admission + holds redesign (Redis accounting, direct O(1) balance capacity, cached policy config, atomic Lua admit/capture, cap-taxonomy collapse, durable-spend seam) lives in **#513**. This ledger issue only consumes its output: the durable spend that #513's capture writes here as immutable double-entry transfers (Phases A/B).
- [x] Durable-spend writer is live (`CaptureAuthorized`→`spendBalanceThenOwedTx` writes immutable transfers); the balance-cache refresh source #513 reads is `deriveBalance`/`LedgerAccountBalance`. The Redis hot-path/cache itself is #513.

### D. Separate ledger fields from control-plane attribution
- [~] CANCELLED (2026-06-17, recommend won't-do): moving `invoker_id`/`resource`/`invoice_id`/`grant_id` into an opaque `user_data` jsonb is YAGNI — the typed columns are indexed and actively queried (invoice rollup, lot attribution, history). Collapsing to jsonb LOSES queryability for only theoretical "ledger purity." Reversible if strict TB purity is ever wanted.

### E. Cross-currency settlement (the ONLY real FX gap — gated on a product decision)
Reality check (verified 2026-06-17): multi-currency native balances ALREADY work and are NOT a gap. The registry (`internal/modules/money/currency.go`) defines USD/USDC/EUR (6dp), JPY (4dp), SOL (9dp); every money table keys on `currency`; balance/owed/caps/settings are per (merchant, customer, currency). Each currency is its own sealed ledger — already the TB ledger-per-currency model, "one currency per entry" enforced by construction. FX conversion also already exists for READ-ONLY budget-policy comparison (`internal/integrations/fx` `ConvertAmount`, ceil-rounded, `NoOpProvider`=1.0 default), so a request in currency A is evaluable against a cap denominated in currency B.
The only thing missing is cross-currency SETTLEMENT as money movement — spending a currency-A balance against a currency-B charge, or converting balance between currencies. Today that's impossible (a EUR charge needs EUR balance/credit). This is a feature, not a bug; it brings rate sourcing, spreads, rounding residual, liquidity accounting, and FX gain/loss.
- [~] CLOSED as NOT-NEEDED under the current product model ("each currency is its own wallet" — every balance/cap/credit keys on `currency`; custom credits are no-FX by design #475). Per this phase's own gate, NOTHING here is required. The settlement design (when/how, minimal mechanic, risk boundary) is extracted to **future.md #515** to pick up if/when a real use case appears (most likely crypto-deposit→USD-credit).
- [~] Future #515: model conversion as two linked transfers through a per-merchant `fx_liquidity` account (DR payer A-balance / CR fx_liquidity-A; DR fx_liquidity-B / CR settle-B), atomic via `flags.linked`; the liquidity account accrues spread / FX P&L.
- [~] Future #515: record the quote (rate, AsOf, provider) on the conversion transfers for audit; needs a REAL `fx.Provider` (the NoOp returns 1.0).
- [~] Future #515: decide explicit user-initiated convert (simpler, auditable — RECOMMENDED) vs implicit cross-currency spend at spot (magical, riskier: rate at spend time, partial draws across currencies).
- [~] Already true by construction: no cross-currency in one transfer. Custom credits remain no-FX by design (#475).

### F. Per-customer FOR UPDATE lock = the scaling tell
- [~] Lock-wait/contention metric DEFERRED to when operational-metrics infra exists (the repo has app-level analytics but no Prometheus/operational metrics sink today; adding one for this alone is out of scope). No behavior change is needed now regardless.
- [x] Trigger DOCUMENTED: the per-customer `lockBalance` `FOR UPDATE` is the scaling tell. UPDATE 2026-06-17 (Paul): the FIRST response is now Phase H (denormalized account counters → O(1) balance reads, keeps Postgres) — chosen proactively rather than waiting for contention, because the balance SUM grows unbounded on both read and write. TigerBeetle (TB = money source of truth, Postgres = control plane, synced by transfer id) remains the FURTHER escalation if a single hot account's write rate still contends after H (H removes the read-cost growth but not per-account write serialization).

### G. Tests
- [x] Conservation: transfer sequences leave `Σ ledger == 0` (`TestLedger_ConservationAndFlows`).
- [x] Immutability: app role (NOBYPASSRLS) denied UPDATE/DELETE on ledger rows; capture/void/expiry are new rows (`TestLedger_AppendOnly`).
- [x] Two-phase durability: a pending persists until resolved; capture posts the actual, release frees the hold, a pending resolves at most once (`TestLedger_TwoPhase`). Plus sign-constraint + currency-guard tests.
- [~] Migration backfill — N/A: pre-launch HARD CUT (decided 2026-06-17, Paul). Migration 004 DROPs `money_transactions`/`money_blocks` outright; no data to translate.

### H. Denormalized account balances — O(1) reads (DESIGNED 2026-06-17, Paul; the chosen fix, not "wait for TB")
PROBLEM (confirmed 2026-06-17): `LedgerAccountBalance`/`LedgerAccountHeld` are `SUM(...) FILTER (...)` over EVERY `ledger_transfers` row touching the account — O(transfers-per-account), growing forever. Worse, it's on the WRITE path too: `Apply`'s `checkDebitFloor` calls `Balance()` (another full SUM) on every posted debit to a floor-enforced account. The admit hot path pays it on read; capture/spend pays it on write.

FIX — maintain running counters on the `ledger_accounts` row, updated atomically with each transfer insert (this is exactly what TigerBeetle does: accounts carry `debits_posted`/`credits_posted`/`debits_pending`/`credits_pending`; balances are O(1) reads, never re-summed). The append-only `ledger_transfers` stays the immutable source of truth; the counters are a maintained PROJECTION, verified by the conservation re-SUM.
- [x] Add `credits_posted`/`debits_posted`/`credits_pending`/`debits_pending` (bigint NOT NULL default 0) to `ledger_accounts` in the pre-launch `002_ledger` baseline. No separate backfill needed under the current hard-cut migration model.
- [x] The `ledger_transfers` insert trigger updates the counters of the affected accounts in the SAME tx as the insert, per phase: **posted** → debit.debits_posted += amt, credit.credits_posted += amt; **pending** → debit.debits_pending += amt (+ credit.credits_pending += amt); **post_pending** (capture, may true-up to actual) → release the full pending reservation and post the actual; **void_pending** (release) → debit.debits_pending −= pending amt (+ credit.credits_pending −= pending amt).
- [x] `Balance` = `credits_posted − debits_posted` (O(1) row read); `Held` = `debits_pending` (O(1)); `checkDebitFloor` reads the row instead of re-summing.
- [x] **Available balance + available credit in ONE query** (Paul's ask): for the admit affordability gate, arrears = the `customer_balance` account is allowed to go NEGATIVE down to `−credit_limit` (already how `Spend` works: `AllowDebitNegativeUpTo = credit_line`), so **the negative balance IS the used credit — there is NO separate owed quantity to subtract** for affordability. One point-lookup JOIN returns everything:
  ```sql
  SELECT a.credits_posted - a.debits_posted AS balance,
         a.debits_pending                   AS held,
         s.billing_mode,
         COALESCE(s.credit_limit_amount, 0)  AS credit_limit
  FROM openrails.ledger_accounts a
  LEFT JOIN <money settings> s
    ON s.merchant_id = a.merchant_id AND s.customer_id = a.customer_id AND s.currency = a.currency
  WHERE a.merchant_id = $1 AND a.customer_id = $2 AND a.currency = $3
    AND a.account_type = 'customer_balance';
  ```
  Then the caller computes: `floor = billing_mode='arrears' ? -credit_limit : 0`; `available_credit = arrears ? credit_limit + min(0, balance) : 0`; `total_spendable = balance + (arrears ? credit_limit : 0)`. The admit gate already takes exactly `(balance, creditLimit→floor)`. (NOTE: the `arrears_liability` account / `OutstandingOwedAmount` is the INVOICING/collection view — keep it OUT of the spend-affordability path so owed isn't double-counted.)
- [x] Conservation stays the HARD-GATE invariant test: `Σ(transfers for account) == stored counters` (drift = wrong money). `TestLedger_*` now asserts the counters equal the re-SUM after transfer sequences.
- [~] Documented caveat, not remaining implementation work: the counter `UPDATE` still takes a row lock on the account row, so per-account WRITE serialization (Phase F) remains — that's inherent to a consistent running balance; TB/sharding is the escalation if a single hot account's write rate contends. H removes the READ-cost growth, not the per-account write-serialization.
- [x] WHEN THIS LANDS: delete the #513 in-memory balance cache (`admission.BalanceCache` + its wiring). DONE 2026-06-18: cache impl/test/runtime field/admitter wiring removed; spendgate input renamed from `CachedBalance` to `AccountBalance`.

## Non-goals

- NOT running TigerBeetle-the-database in this issue (it is a separate stateful system + a two-phase sync with Postgres); only its data-modeling principles, in Postgres. Adopting TB is gated on Phase F.
- Do NOT regress the already-TB-aligned wins: integer minor units, idempotency keys, entitlements `tstzrange` exclusion windows, append-only `usage_events`, derived available balance (post-#491).

## HARD CUT — no legacy, no backward compatibility (decided 2026-06-17, Paul)

The ledger flips to double-entry/immutable completely; the single-entry representation does NOT survive alongside it.
- No backfill migration is required under the pre-launch HARD CUT. `migrations/postgres/004_drop_single_entry.up.sql` drops `money_transactions` + `money_blocks`; the live path writes only the double-entry ledger and grant ledger. Specifically `money_transactions.balance_after` is gone (balances are derived), `money_blocks` mutation/expiry is replaced, and there is NO dual-read (old single-entry + new double-entry) and NO compatibility view kept around.
- NO feature flag toggling single-entry vs double-entry; NO dual-write; NO "fall back to balance_after if the derived balance disagrees." One model, the new one.
- Wire-contract changes (if any) are outright — consumers move in LOCKSTEP via a coordinated version bump, no compat window.
- Exit gate: after the cut, `grep` finds no `balance_after`/single-entry-mutation reader; conservation invariant (`Σ ledger == 0`) holds; `go build ./... && go vet ./...` clean.

## References

- Source comparison: TigerBeetle (github.com/tigerbeetle/tigerbeetle) Account/Transfer/two-phase/linked-transfer model.
- Current code: `internal/modules/money/money_service.go` (`depositTx`/`withdrawTx`/`withdrawBalanceAndBlocks`/`deriveBalance`/`lockBalance`), `internal/modules/money/authorize.go` (`AuthorizeAndHold` Redis hold), `internal/db/gen/money_ledger.sql.go`, `internal/db/gen/money_accounts.sql.go` (settings only — misnamed), schema `migrations/postgres/001_schema.up.sql` (money_transactions/money_blocks/money_settings/money_windows/budget_inflight_holds).
- Consistency-invariant overlap + DEPENDENCY: #511 (unified consistency engine). The conservation check (`Σ ledger == 0`) stays a #512 ledger diagnostic/test, not a #511 finding. #511's money-side consistency checks (`consistency.duplicate.provider_charge`, `consistency.amount_mismatch.*` for refund/payment/invoice/credit mismatches) must be written against provider-observed payments/refunds plus #512/#514 source events, not the deleted single-entry `money_transactions`.

---

# #518: provider-accounts

**Completed:** no
**Status:** PLANNED 2026-06-18: split out from #511 provider-pull design. OpenRails currently has per-intent provider account identity stamps under the legacy/misleading column name `account_fingerprint` (#365) and a Stripe-specific `merchants.stripe_account_id` metadata field, but it does not have a merchant-scoped `provider_accounts` table that says "this merchant's Stripe/Mobius/CCBill account is this provider account." #511's provider-pull must not run against ambient credentials alone; it needs durable provider account rows first. This issue should also delete the old fingerprint terminology from the codebase: the durable identity is the provider-returned `account_id`, resolved from the provider's account/profile/whoami endpoint, not a hash of credentials.

Bind each merchant to one or more provider accounts per provider type/rail, while keeping every provider-owned row tied to the exact account that produced it. A private standalone merchant like `doujins` may start with Stripe account A, later add Stripe account B as the new primary account, and keep account A around for legacy rebills, refunds, webhooks, or historical charge lookup. Swapping accounts is an explicit rotation/adoption workflow that preserves historical account identity; it is never an accidental effect of changing config credentials.

## Design decisions

1. **Provider account identity is durable state, not config.** Config supplies credentials for a provider key; OpenRails resolves those credentials through the provider's account/profile/whoami endpoint and records the provider account they represent.
2. **Multiple accounts per provider type are allowed, one primary by default.** Provider type is the rail (`stripe`, `nmi`/`mobius`, `ccbill`, `solana`, ...). A merchant may bind several accounts of the same type, but only one account per provider type should be `primary` for new checkout/catalog work unless a future product explicitly supports account selection. Legacy/live accounts remain bound for rebills, refunds, pulls, webhooks, and evidence.
3. **Provider accounts are merchant-scoped.** `provider_accounts` is the provider-account record and the merchant binding in one table. It stores merchant id, provider type, provider-returned account id, vault credential reference, role/status, and operational evidence. We do not need a separate global provider-account registry unless we later intentionally support shared provider accounts across merchants.
4. **Account id identifies the provider account, not the secret.** `provider_accounts.account_id` is the canonical provider-account identity returned by a provider account/profile/"whoami" endpoint. Stripe uses account id (`acct_...`); NMI/Mobius uses the gateway merchant/account identity from its profile endpoint; CCBill must use its account/subaccount identity; Solana can use network + wallet/program authority where account-binding is useful. Do not use a credential hash as canonical identity when the provider can tell us the account id.
5. **Provider mirror rows should reference the merchant-scoped account row.** New provider-owned mirror rows should store `provider_account_id` as a local FK to `provider_accounts.id` rather than duplicating raw provider account ids. Historical rows retain the old account row when a merchant rotates from Stripe-A to Stripe-B, so Stripe-B pulls cannot overwrite or "not find" Stripe-A rows.
6. **Credential mismatch aborts provider-pull for the targeted account row.** A pull targets a specific `provider_accounts.id`. If the credentials for that row resolve to a different provider account id than `provider_accounts.account_id`, `reconcile pull` aborts before inserting, overwriting, or marking that account's source domain reconciled. A mismatch against the primary account is not relevant when intentionally pulling a legacy account.
7. **Account rotation is explicit.** Moving `doujins` from Stripe-A to Stripe-B creates a new merchant-scoped provider account row, promotes B to primary, and leaves A in a legacy/live state if it still has rebills/refunds/webhooks to service. It does not rewrite old rows and does not make old provider objects disappear.
8. **Delete provider fingerprint terminology.** The existing #365 `account_fingerprint` column/interface/CLI names are legacy implementation detail. Replace them with provider account identity terms (`provider_account_id`, `account_id`, `ProviderAccountResolver`, etc.) and drop the old column once intents can point at provider account rows.

## Binding roles

- **`primary`**: the default account for new provider work of that type. If a user chooses Stripe checkout and no account is explicitly selected, OpenRails routes to the enabled primary Stripe binding. Catalog push also targets primary unless told otherwise.
- **`secondary`**: an enabled non-default account. It is not used automatically just because primary fails unless a merchant/operator config explicitly allows fallback/routing policy. Useful for manual migration, controlled traffic split, alternate geography/MID, or a temporary backup account.
- **`legacy`**: an enabled account retained for old provider objects. OpenRails may pull it, receive webhooks for it, refund historical charges from it, cancel old subscriptions on it, or let existing remote rebills continue if the operator chooses that migration style. It is not used for new checkout/catalog creation by default.
- **`disabled` status**: no new provider work and no routine pull. Historical rows remain attributed to the binding. Operator repair may still require a targeted credential restore or manual provider action.

## Work breakdown

### A. Schema
- [ ] Add `openrails.provider_accounts`: `id`, `merchant_id`, `provider_type`, `provider_key`, `account_id`, `display_name`, `vault_secret_ref`, `role` (`primary`, `secondary`, `legacy`), `status` (`enabled`, `disabled`), `evidence jsonb`, `first_seen_at`, `last_verified_at`, `replaced_at`, timestamps. `id` is the local FK target for provider-owned mirror rows; `account_id` is the provider-returned identity (`acct_...`, Mobius account id/profile identity, CCBill account/subaccount, Solana public key/authority). Store secret references only, never provider secrets.
- [ ] Add FK/RLS/indexes and constraints:
  - unique `(merchant_id, provider_type, account_id)` so the same account is not bound twice to the same merchant;
  - at most one enabled `primary` row per `(merchant_id, provider_type)`;
  - normalize/lowercase provider type/key and validate the provider key resolves to the row's provider type.
- [ ] Decide whether to backfill or deprecate `merchants.stripe_account_id`; it should not remain a parallel source of truth for Stripe account identity.
- [ ] Add `provider_account_id` (UUID FK to `provider_accounts.id`) to provider-owned mirror tables where it matters first: `payments`, `subscriptions`, `payment_methods`, `processor_customers`, and future refund/dispute mirror tables. Keep it nullable for legacy/import rows. Existing provider-specific remote object ids remain on those tables (`payments.transaction_id`, `subscriptions.processor_subscription_id`, `processor_customers.processor_customer_id`, payment-method token/id fields); do not introduce a generic `provider_object_id` column unless a new generic mirror table needs one.
- [ ] Migrate `provider_intents`: add nullable `provider_account_id` (UUID FK to `provider_accounts.id`), stamp it at enqueue, verify it at execution/verification, backfill where possible from legacy `account_fingerprint` + provider key, then drop `provider_intents.account_fingerprint`.

### B. Provider account identity resolvers
- [ ] Replace the #365 `FingerprintSource` naming with a provider account resolver API that returns `(provider_type, account_id)`.
- [ ] Reuse/adapt the existing account-identity resolver logic for Stripe and NMI/Mobius, but expose the concept as provider `account_id` for #518 and create/verify `provider_accounts`.
- [ ] Add CCBill account/subaccount identity resolution through the provider's account/profile/whoami-style endpoint before CCBill provider-pull is considered safe.
- [ ] Define Solana's binding identity only where the provider account concept is load-bearing (`network + receiving wallet/program authority`); do not over-model it if globally unique addresses already protect the operation.

### C. Binding service
- [ ] Add a small service/store for `VerifyOrBind(ctx, merchant, providerKey)`:
  - resolves current credentials to provider `account_id`;
  - creates or verifies the merchant-scoped provider account row and vault secret reference only from trusted bootstrap/admin context;
  - verifies normal runtime operations against the targeted row, or the primary row when no account is specified;
  - returns a typed mismatch error with current vs bound provider account id redacted enough for logs.
- [ ] Add explicit rotation/adoption path: add the new account binding, promote it to primary, demote the old primary to `legacy` or `secondary`, record evidence, and choose what happens to live pending intents.
- [ ] Make key rotation within the same provider account a no-op because the resolved provider account id is unchanged.

### D. Integrations
- [ ] Gate `reconcile check` / `reconcile pull` on binding verification before provider data is treated as authoritative.
- [ ] Run provider-pull per `provider_account_id`: by default pull the primary account for each provider type, with flags/options to include legacy/secondary accounts or target a specific provider account row.
- [ ] Stamp new provider mirror rows with `provider_account_id`; compare/upsert only inside the same provider account row.
- [ ] Include binding id/provider account id in reconciliation run summaries and finding evidence.
- [ ] Teach checkout/vault/subscription paths to verify the active binding before creating provider-owned rows when practical.
- [ ] Replace existing `provider_intents.account_fingerprint` guard with `provider_intents.provider_account_id`: queued intents should execute only when current credentials resolve to the same account id as the provider account row they were enqueued against.

### E. Bootstrap / operations
- [ ] Extend bootstrap auth/merchant config to declare expected provider account bindings by provider type/key and provider account id.
- [ ] Add CLI/admin commands to inspect bindings, verify current credentials, and rotate/adopt a new account explicitly.
- [ ] Replace `openrails intents refingerprint` with binding-aware operations (`verify`, `rotate/adopt`, and any needed pending-intent rebind command); remove "fingerprint" from user-facing command names/help.
- [ ] Document recovery: restoring old credentials, rotating keys within the same account, deliberately adopting a new account, and handling pending intents from the old account.

### F. Tests
- [ ] Integration test: first trusted bind succeeds, later same-account key rotation verifies cleanly.
- [ ] Integration test: credentials resolving to a different Stripe/NMI account abort provider-pull before writes.
- [ ] Integration test: Stripe-A + Stripe-B can both be bound to the same merchant/provider type, with exactly one primary; new checkout uses B while legacy Stripe-A rebills/refunds remain attributed to A.
- [ ] Integration test: old rows linked to Stripe-A are not overwritten or treated as absent after explicit rotation to Stripe-B.
- [ ] Integration test: duplicate account binding for the same merchant/type/account_id is rejected, and two enabled primaries for the same merchant/type are rejected.
- [ ] Integration test: shared active account across merchants is rejected unless an explicit future shared-account mode is added.
- [ ] Migration/code cleanup test: no remaining runtime code paths depend on `provider_intents.account_fingerprint`; old fingerprint terminology is removed from commands/docs except migration notes.

## Relationship

- **#511 depends on this for safe Provider-Pull.** #511 can keep building the convergence engine, but provider-pull must not mark source domains reconciled or overwrite provider mirrors until this binding exists.
- **#365's safety property remains necessary, but its schema/name should not.** Merchant bindings protect the merchant/account boundary; per-intent binding ids protect already-queued outbound work from executing against changed credentials. Replace the `account_fingerprint` column and "refingerprint" command with binding-aware provider account id semantics.
- **#517 provider-absence tombstones depend on this even more strongly.** Absence checks are meaningless unless they prove absence in the same provider account that originally produced the row.

---

# #511: unified-consistency-invariant-engine

**Completed:** no
**Status:** IN_PROGRESS 2026-06-17: **Phase A (findings-ledger foundation) BUILT + integration-tested.** `migrations/postgres/008_convergence_engine_findings.up.sql` widens the `reconciliation_findings.finding_type` CHECK to the unified four-plane taxonomy (`pull.*`, `derive.*`, `life.*`, `consistency.*`; legacy `PS-*` kept during the transition → dropped in Phase F), adds the `held` + `indeterminate` statuses, documents the `self` provider sentinel, and adds the `openrails.reconciliation_state` table (per-(merchant, source_domain) `fully_reconciled` + `last_full_pull_at`) backing the confirmed-absence gate (§3.2). sqlc: `UpsertReconciliationState` / `ListReconciliationState` / `IsSourceDomainReconciled`. Tests (`internal/reconcile/convergence_phase_a_integration_test.go`): qualified finding types + held/indeterminate admitted, unknown type rejected, legacy PS-* still admitted (existing engine test still green), gate defaults not-reconciled → flips on upsert → watermark preserved. Whole-package `go build` + reconcile integration suite green. **Phase C (Converge engine core) BUILT + integration-tested.** `internal/reconcile/converge.go`: `ConvergeEngine.Converge(scope)` runs the DERIVE→LIFE→CON passes (the `Pass` interface + `derivePass`/`lifePass`/`conPass` stubs in `converge_passes.go`), persists findings to the shared ledger (lazy run creation — a clean scope does zero writes), and remediates via the confirmed-absence gate (EXCESS held until its source domain is reconciled). Tests green: clean-scope no-op + the gate (MISSING repairs immediately, EXCESS held→repaired once the domain reconciles). DESIGN DECISION (Paul): build toward ONE unified engine (hard cut, no permanent legacy coexistence) — Converge is the new core; the legacy per-provider PS-* diff (`engine.go`) is deleted in Phase F, and the transitional PS-* CHECK union is removed then. **Phase D STARTED (DERIVE plane bootstrapped, integration-tested).** First + hardest-integration check live: `derive.grant_effect.missing` (`converge_passes.go` derivePass + `grants.MissingEffects` detection + `MaterializeGrant` repair), proving the engine→pass→#514-grants→AUTO-repair→idempotent path end-to-end (`TestConverge_DeriveGrantEffectMissing`). Finding identity is the **qualified slug only** (no short codes — Paul, decided 2026-06; `ConvergeFinding.Type`, regex-format CHECK in migration 008, doc §1 + §5 tables all slugs). Approach: #511 reframes+extends the existing `internal/reconcile` pull→findings-ledger→idempotent-applier→auto-vanish spine (not greenfield); ONE unified engine (hard cut — legacy PS-* diff deleted in Phase F). NEXT in Phase D: `derive.grant_effect.excess` (gated clawback via #514 `RevokeGrant`) + `.mismatch`, then `derive.grant.*`, then LIFE + CON. Original plan follows. — Replaces the two split consistency mechanisms — `internal/reconcile` (PS-1..PS-10, local-vs-processor) and `internal/audit` (P-E/S-E/SS, local-vs-local) — with ONE system: `reconcile` becomes a pure provider-state pull that overwrites the local mirror, plus a continuously-running **Convergence Engine** that drives every grant effect (entitlements / credits / product-access) and external decision into a consistent state with the source events, derived from the **#514 grant ledger**.

Build the unified billing-consistency system designed in `docs/consistency-invariants.md`: a provider-truth **pull** (`reconcile`) and a **continuously-running Convergence Engine**, organized by four diagnostic planes — **PULL** Provider-Pull (inbound: provider actual -> OpenRails observed state) / **DERIVE** Derivation (source -> grant -> grant effect) / **LIFE** Lifecycle (clock + state machine) / **CON** Consistency (internal accounting/referential consistency). Outbound provider work is a remediation channel (`provider_intents` / operator action), not a finding namespace. Subsumes and retires `openrails audit` and the old `PS-*`/audit finding taxonomies.

## Design decisions (resolved 2026-06-17)

1. **Provider state is authoritative for observed processor facts, not catalog definitions.** The pull overwrites the local mirror for observed charges/refunds/disputes/subscription/vault state, but NEVER overwrites local catalog/product/price definitions and NEVER deletes a standing local intent (`deletion_scheduled_at`, `scheduled_price_id`, pending `provider_intents`). The engine is schedule-aware: a not-yet-due scheduled change (cancel scheduled for Jun 28, pull on Jun 17 still sees the sub live at NMI) is fully consistent — expected lag, not drift. Retryable provider lag remains provider action state; abandoned past-due work becomes `life.provider_intent.abandoned`. Catalog/plan drift is `consistency.amount_mismatch.provider_catalog`: OpenRails catalog is authoritative and the processor must be pushed back into shape, or surfaced as manual action where the provider cannot be pushed.
2. **No enforce command/crank.** Convergence runs continuously while the server is up (inline after every source mutation + a background sweep). `reconcile pull` is a CLI used when the server may be down, so it pulls and then runs a one-shot `Converge` pass itself. No `--enforce` flag; `pull` always converges.
3. **Credit clawback pulls back UNSPENT only.** On revoke/refund-retraction the unspent credit remainder is clawed back automatically — a reversing `ledger_transfer` `DR customer_balance / CR revoked_credits` (a system account distinct from `expired_credits`, which is for time-lapse), "unspent" derived from the ledger; reversible by a counter-transfer; the already-spent portion is left untouched (informational only). The credit clawback is automatic, but the **money refund** to the card/wallet is NOT — that's a separate OPERATOR step (`revoked_credits → processor_clearing` + provider refund), optionally bundled into one `RevokeGrant(grant,{refund})` call (Paul, 2026-06-17). See `docs/consistency-invariants.md` §11 (decision 4).
4. **Default pull = the head (current state).** `reconcile pull` pulls current provider state by default; an optional `--since` / date range backfills historical transactions for replaying old grant effects (legacy import, audit completeness).
5. **Grant ledger (#514).** An append-only `grants` log sits between source events and grant effects (generalizing `product_access_grants`); derive-1 (event → grant) and derive-2 (grant → grant effect) live in #514 and are the sole writers of grants + grant effects. The DERIVE plane is uniform across entitlements/credits/access; grants are immutable (revoke / expire / supersede = NEW events), so the manual-override rule is just a revoke event in the log.
6. **No "conflict" shape.** Shapes are MISSING/EXCESS/MISMATCH only (an exhaustive 2×2). A case the engine can't evaluate (authority unreachable or evidence ambiguous) is the `indeterminate` finding state — an evidence problem, never a truth-model fault.
7. **No Provider-Pull `excess` in the active #511 implementation.** A provider list/report failing to return a local transaction is not reliable enough to treat as a current inconsistency: providers may have retention/reporting limits, the pull may be incomplete, or the local row may be attributed to the wrong provider/account. The #511 reconcile/pull path should materialize missing provider facts and overwrite mismatched provider-owned mirror fields, but it should not emit or repair `pull.*.excess`. Possible future provider-absence/tombstone work is tracked as future issue #517.
8. **Provider-Pull is account-bound.** A merchant may have multiple provider accounts per provider rail (`stripe`, `mobius`/NMI, `ccbill`, etc.), but provider-owned rows and pulls are scoped to one `provider_account_id` row at a time. `reconcile pull` must resolve the current provider credentials to a provider account id before diffing. The pull is authoritative only for `(merchant, provider_type, provider_account_id)`, where `provider_account_id` is the local FK to `provider_accounts.id`. If the Stripe-A account row is being pulled but the configured credentials resolve to Stripe-B, the pull aborts for that account row as a configuration error; it must not materialize, overwrite, or compare rows across accounts. This replaces the old #365 `provider_intents.account_fingerprint` guard with provider account id semantics.

## Two safety doctrines for bulk legacy import

- **Replay vs converge.** Grant effects are replayed at their historical source timestamps (a 2025-06-30 90-day membership is recreated already-expired). Side-effecting external actions (charges, rebills, dunning) are NEVER replayed — the Convergence Engine converges a record to its correct CURRENT state (dunning unrun for 3 months ⇒ cancel now + revoke as-of grace end), never retro-charges.
- **Confirmed-absence gate.** MISSING/materialize is additive ⇒ AUTO even in bulk. EXCESS/retract is destructive and is HELD until the relevant source domain is confirmed fully reconciled for that merchant (an imported entitlement with no subscription is "not imported yet", not "orphaned"); then ADMIN-gated, never silent.

## Relationship to the ledgers (#514 grant, #512 money)

The **Convergence Engine** reconciles two append-only ledgers it does not own. **#514** is the grant ledger — the access source of truth + derive-1/derive-2; this issue's DERIVE plane derives grant effects from it and verifies them. **#512** is the money substrate. They meet at credit grants:

- **A credit grant effect is a `ledger_transfer`, not a `money_blocks` row.** For a credit-granting grant, derive-2 emits a credit-deposit transfer (`world`/`processor_clearing` → `customer_balance`) tagged with the grant via #512's `source='grant'` / `source_id=grant_id`. So #511 does NOT add `grant_id` to `money_blocks` — the grant linkage rides on `ledger_transfers`.
- **Clawback (`derive.grant_effect.excess`) is a reversing transfer, not a mutation.** A revoke/refund's unspent remainder is returned by appending a `customer_balance → revoked_credits` transfer (lapse uses `expired_credits`); "unspent" is *derived* from the ledger — matching #512's append-only + derive-don't-store principles. Clawback is automatic + reversible; the money refund is a separate OPERATOR step (never automatic). See `docs/consistency-invariants.md` §11 (decision 4).
- **Entitlement and product-access grant effects are unaffected by #512** (not money); they stay local rows derived from grants.
- **DERIVE-plane credit checks run against `ledger_transfers`:** every credit grant has its deposit transfer (`derive.grant_effect.missing`), every credit deposit traces to a live grant (`derive.grant_effect.excess`), amounts/cadence match the grant spec (`derive.grant_effect.mismatch`).
- **Sequencing:** land #512's credit substrate before/with #511's credit derivation so credits aren't migrated twice. If #512 slips, #511's credit derivation targets the current `money_transactions` shape behind the same derive-2 interface and swaps substrate when #512 lands.

## Metadata

- Category: feature
- Status: planned
- Passes: false

## Work breakdown

### A. Schema, grants layer & findings ledger
- [x] **Depends on #514 (grant ledger):** DONE — #514 shipped; derive-1/derive-2 live in `internal/modules/grants` (the sole writers). The DERIVE plane will consume + verify them.
- [x] Extend `reconciliation_findings`: admit qualified `pull.*` / `derive.*` / `life.*` / `consistency.*` finding types (migration 008; legacy `PS-*` unioned during transition, dropped in Phase F); `provider` is free text so the `self` sentinel needs no schema change (documented via COMMENT).
- [x] Add the `held` status (confirmed-absence gate, §3.2) AND the `indeterminate` status (authority unreachable/ambiguous) — migration 008 status CHECK. (Reason carried in finding metadata.)
- [x] Add per-merchant reconciliation-state: `openrails.reconciliation_state` (per-(merchant, source_domain) `fully_reconciled` + `last_full_pull_at`), RLS-scoped; sqlc `UpsertReconciliationState`/`ListReconciliationState`/`IsSourceDomainReconciled`. Tested.
- [ ] Confirm intent fields suffice for schedule-awareness (`subscriptions.deletion_scheduled_at`, `scheduled_price_id`, `provider_intents.next_attempt_at`/`expires_at`); add a scheduled-effective-at marker if any transition lacks one. (Deferred to Phase B pull rework, where schedule-awareness is exercised.)

### B. The pull (`reconcile` rework)
- [ ] Rename `reconcile fix` → `reconcile pull`; keep `check` (dry-run change log, zero writes, no convergence) and `report`.
- [ ] Rewrite pull semantics from "diff + selectively apply findings" to "pull complete head state per provider, OVERWRITE the local mirror, log every row old→new".
- [ ] Provider-Pull coverage: charges→payments, subscription observed status/period/next-bill, refunds, chargebacks/disputes, vault→payment_methods, and remote catalog/plan observations as evidence for `consistency.amount_mismatch.provider_catalog` only (never as local catalog overwrites). For provider-owned mirror resources in #511, implement only meaningful `missing` and `mismatch` shapes: missing provider fact → materialize/upsert local mirror; differing provider fact → adjust/overwrite provider-owned mirror fields. Do **not** emit `pull.*.excess` in the active reconcile command.
- [ ] Account-bind every pull using #518: resolve the live provider account id using the provider account/profile/whoami endpoint (Stripe account id from `/v1/account`, NMI/Mobius merchant identity from profile report, CCBill account/subaccount identity once implemented); verify the targeted `provider_account_id` row before any provider list is treated as authoritative.
- [ ] Abort on merchant/provider account mismatch (#518): if the targeted provider account row says `account_id_A` and current credentials resolve to `account_id_B`, fail the pull with an operator-facing configuration error. Do not emit `pull.*`, mark a domain reconciled, insert missing rows, or overwrite mismatched rows from the wrong account.
- [ ] Stamp provider-owned mirror rows/run evidence with #518 `provider_account_id` for new writes where possible. Comparisons and overwrites must require the same provider account row; legacy null-binding rows are ambiguous import state and must not be destructively reconciled.
- [ ] Provider-action preservation in the overwrite: never clobber `deletion_scheduled_at`/`scheduled_price_id`/pending intents; compute divergence against (provider-observed state + scheduled future transitions); treat not-yet-due lag as consistent, retryable lag as provider action state, and abandoned past-due work as `life.provider_intent.abandoned`.
- [ ] `--since` / date-range flag for historical backfill; default pulls the head.
- [ ] After a successful pull: mark the pulled source domains reconciled (gate) and run a one-shot `Converge(merchant)` pass.

### C. The Convergence Engine (new core)
- [x] `Converge(scope)` — idempotent (clean scope = no run, no writes; verified `TestConverge_CleanScopeIsNoOp`), scope-narrowable (`Scope{Merchant, Customer?, Subscription?}`). `internal/reconcile/converge.go` (`ConvergeEngine.Converge`). Runs DERIVE→LIFE→CON, lazily creates a run only when there are findings, persists to the shared ledger.
- [x] Plane passes run in order DERIVE -> LIFE -> CON; each plane is a module (`Pass` interface; `derivePass`/`lifePass`/`conPass` in `converge_passes.go`, stubs to be filled in Phase D). The finding model (`ConvergeFinding` with Shape/Class/SourceDomain/Repair) carries provider-action remediation via Class + an optional repair closure.
- [ ] derive-1/derive-2 (#514 grants) invoked + verified by the DERIVE pass. (Phase D — pass skeleton wired, calls land with the checks.)
- [ ] Historical replay anchored to source-event timestamps, judged against the grant's `*_spec_snapshot`. (Phase D.)
- [x] Confirmed-absence gate on every EXCESS repair — DONE (`ConvergeEngine.remediate` holds EXCESS as `held` until `IsSourceDomainReconciled`; verified `TestConverge_ConfirmedAbsenceGate`). (Converge-not-replay for LIFE lands with the LIFE pass in Phase D.)

### D. Invariant checks + appliers (catalogue, `docs/consistency-invariants.md` §5)
- [ ] Use `docs/consistency-invariants.md` §5's Postgres-enforced vs application-enforced catalogue as the implementation checklist; first prefer schema hardening for same-merchant references and impossible local states. DB-enforced invariants do not need runtime findings; only the remaining #511 cross-table/cross-time/provider invariants become `reconciliation_findings` rows.
- [ ] DERIVE grant tier: `derive.grant.missing` (event->grant) / `derive.grant.excess` (gated) / `derive.grant.mismatch` — against the #514 grant log; uniform across entitlements/credits/access.
- [~] DERIVE grant-effect tier (customer-scoped): **`derive.grant_effect.missing` + `derive.grant_effect.excess` DONE.** `missing`: `grants.MissingEffects` (live grants whose entitlement-window/credit-deposit effect is absent) → AUTO repair runs derive-2 (`MaterializeGrant`); tested `TestConverge_DeriveGrantEffectMissing`. `excess`: `grants.UnretractedTerminations` (TERMINATED grants whose effect is still live — `GrantHasLiveEntitlement` / `GetCreditLotRemaining>0`) → AUTO repair `MaterializeGrant` retracts (entitlement revoke / credit clawback); NOT gated (grant+revoke both present = recorded decision, not absence); tested BOTH paths under the app role — `TestConverge_DeriveGrantEffectExcess_TerminatedNotRetracted` (entitlement → window revoked) + `TestConverge_DeriveGrantEffectExcess_CreditClawback` (credit → unspent frozen in `revoked_credits`). Both idempotent. (An earlier transient `revoked_credits` RLS failure was a mid-`openrails_db`-rebuild artifact — re-verified clean under the app role; no bug.) REMAINING: `.mismatch` (window/amount/cadence vs grant spec); the true-orphan `.excess` variant (live effect, NO grant → gated/ADMIN) with merchant-wide enumeration.
- [~] LIFE plane (via `lifePass`): **`life.checkout_session.stale` + `life.subscription.grace_exhausted` DONE.** `checkout_session.stale`: `ListStaleCheckoutSessions` (expired + non-terminal) → AUTO `ExpireCheckoutSessionByID`; tested `TestConverge_LifeCheckoutSessionStale`. `grace_exhausted` (the marquee converge-not-replay check): `ListGraceExhaustedSubscriptions` (past_due + `grace_ends_at < now`) → AUTO repair reuses the local appliers `ReconcileCancelSubscriptionLocal` (cancel NOW) + `ReconcileRevokeSubscriptionEntitlements` with `now=grace_ends_at` (revoke AS-OF grace end — no replayed dunning charges); tested `TestConverge_LifeSubscriptionGraceExhausted` (verifies status=cancelled, entitlement revoked_at == grace_ends_at, idempotent). Both EXCESS-but-time-driven → AUTO, NOT confirmed-absence gated. REMAINING: `life.subscription.period_overdue` / `.dunning_overdue` / `.pending_stale`; `life.provider_intent.abandoned` (OPERATOR/ADMIN); and the linked provider-cancel action for grace_exhausted (needs the provider-action emission wiring, Phase E).
- [ ] Provider actions are remediation, not findings: cancel still-active extras, retry scheduled provider decisions, push catalog plans, or surface pending manual action through `provider_intents`/operator queue.
- [ ] CON is limited to three generic consistency classes, encoded directly as qualified finding types: duplicates (`consistency.duplicate.provider_charge` for already-collected duplicate money, `consistency.duplicate.remote_subscription` for extra active billable remote subscriptions before duplicate money is collected, `consistency.duplicate.invoice_usage`, `consistency.duplicate.invoice_payment`), amount/explainability mismatch (`consistency.amount_mismatch.refund_math`, `consistency.amount_mismatch.payment_amount`, `consistency.amount_mismatch.invoice_math`, `consistency.amount_mismatch.usage_coverage`, `consistency.amount_mismatch.credit_lot`, `consistency.amount_mismatch.provider_catalog`), and reference resolution (`consistency.reference.source_reference`, `consistency.reference.ledger_reference`, `consistency.reference.provider_intent_reference`). Do not create normal findings for #512 ledger conservation, pending resolver uniqueness, FK-backed catalog/payment/checkout/subscription scope references, or subscription price/product coherence; those are schema/diagnostic/import-repair concerns.

### E. Continuous convergence wiring
- [ ] Inline hooks: `Converge(customer|subscription)` synchronously after checkout completion, renewal billing, refund/webhook ingestion, dunning transitions, admin grant/revoke.
- [ ] Background sweep (River worker): periodic `Converge(merchant)` scanning for due lifecycle transitions, overdue intents, and gate-released held findings.
- [ ] Server-running convergence and the CLI one-shot share the same `Converge` core.

### F. Retire the split mechanisms
- [ ] Fold `internal/audit` checks into the engine as DERIVE/CON-plane checks; remove/alias the standalone `openrails audit` command.
- [ ] Replace `internal/reconcile` old `PS-*` finding types + selective-apply with Provider-Pull + the DERIVE/LIFE/CON Convergence Engine; update the store, report rendering, CLI help.
- [ ] Update `README.md` billing section (audit/reconcile description, PS-1..PS-10 mention) to the new model once shipped.

### G. Legacy import
- [ ] Import flow leaves the per-merchant gate "not reconciled" until import + a full historical pull complete; bulk sweep entry point for a freshly imported merchant.
- [ ] Triage report for `held` findings (entitlements without sources, dunning long-overdue, …) before any destructive repair is approved.

### H. Tests
- [ ] Per-plane / per-finding-type tests; idempotency (double-run no-op); historical replay (already-expired windows); converge-not-replay (no retroactive charges).
- [ ] Schedule-aware intent test (Jun 15 cancel / Jun 28 delete / Jun 17 pull is consistent).
- [ ] Confirmed-absence gate (held pre-reconciliation, fires after); credit unspent-only clawback.
- [ ] Provider-Pull tests: missing provider facts materialize/upsert local mirrors; mismatched provider-owned fields overwrite locally; local-only rows are ignored rather than producing `pull.*.excess`.
- [ ] Provider-Pull account-binding tests: first/explicit binding allows the pull; later credentials resolving to another account abort before writes; two merchants cannot silently share/swap provider accounts unless an explicit future shared-account mode is configured; mismatched-binding mirror rows are not overwritten.
- [ ] Migration test: the findings-ledger CHECK admits qualified finding types and old runs still load.

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
