<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 519

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

# #511: unified-consistency-invariant-engine

**Completed:** no
**Status:** IN_PROGRESS 2026-06-18 (session): **all four planes now have working, integration-tested checks AND the engine RUNS in the server.** This session: finished the LIFE plane (added `dunning_overdue`; all six LIFE checks green), added DERIVE merchant-wide fan-out, **bootstrapped the CON plane** (`consistency.reference.source_reference`), and **wired Phase E's background sweep** — `ConvergeSweepWorker` (15-min periodic River job) iterates active merchants and runs `Converge(merchant)`, proven end-to-end by `TestConvergeSweepWorker_RemediatesDriftAcrossMerchant` (seed drift → run worker → both items remediated). **The Convergence Engine was extracted into its own package `internal/reconcile/converge`** (package `converge`; only coupling to the legacy package was the `Severity` string type, now local) so it builds + tests independently of the legacy PS-* engine — which the concurrent #518 provider-accounts work keeps churning. All converge tests + the sweep test green; legacy `internal/reconcile` (non-test) still builds. **Later 2026-06-18 (after #518 landed):** (1) **audit hard cut** — folded the still-valid `internal/audit` checks into the convergence engine (CON: `consistency.reference.source_reference` incl. admin-grant orphan, `consistency.duplicate.provider_charge`), dropped the billing-viability warnings, DELETED the `internal/audit` package + the `openrails audit` command; (2) **`pull-provider`** — renamed `reconcile`→`pull-provider` with dry-run default / `--overwrite` / `--prune` (account-bound, ledger-safe) / post-pull `Converge`, prune integration-tested; (3) **PS-* taxonomy fully retired** — `findings.go` emits four-plane slugs, migration 008 CHECK is slugs-only, PS-* rejected. REMAINING for #511: DERIVE grant tier (`derive.grant.*` event→grant); more CON subtypes (`amount_mismatch.*`); Phase E inline hooks (Converge after checkout/renewal/refund/dunning/admin); the pull engine's literal "pure overwrite" rewrite (cosmetic — it emits slugs + applies upserts today); Phase G (import gate + held triage). | (earlier 2026-06-17) **Phase A (findings-ledger foundation) BUILT + integration-tested.** `migrations/postgres/008_convergence_engine_findings.up.sql` widens the `reconciliation_findings.finding_type` CHECK to the unified four-plane taxonomy (`pull.*`, `derive.*`, `life.*`, `consistency.*`; legacy `PS-*` kept during the transition → dropped in Phase F), adds the `held` + `indeterminate` statuses, documents the `self` provider sentinel, and adds the `openrails.reconciliation_state` table (per-(merchant, source_domain) `fully_reconciled` + `last_full_pull_at`) backing the confirmed-absence gate (§3.2). sqlc: `UpsertReconciliationState` / `ListReconciliationState` / `IsSourceDomainReconciled`. Tests (`internal/reconcile/convergence_phase_a_integration_test.go`): qualified finding types + held/indeterminate admitted, unknown type rejected, legacy PS-* still admitted (existing engine test still green), gate defaults not-reconciled → flips on upsert → watermark preserved. Whole-package `go build` + reconcile integration suite green. **Phase C (Converge engine core) BUILT + integration-tested.** `internal/reconcile/converge.go`: `ConvergeEngine.Converge(scope)` runs the DERIVE→LIFE→CON passes (the `Pass` interface + `derivePass`/`lifePass`/`conPass` stubs in `converge_passes.go`), persists findings to the shared ledger (lazy run creation — a clean scope does zero writes), and remediates via the confirmed-absence gate (EXCESS held until its source domain is reconciled). Tests green: clean-scope no-op + the gate (MISSING repairs immediately, EXCESS held→repaired once the domain reconciles). DESIGN DECISION (Paul): build toward ONE unified engine (hard cut, no permanent legacy coexistence) — Converge is the new core; the legacy per-provider PS-* diff (`engine.go`) is deleted in Phase F, and the transitional PS-* CHECK union is removed then. **Phase D STARTED (DERIVE plane bootstrapped, integration-tested).** First + hardest-integration check live: `derive.grant_effect.missing` (`converge_passes.go` derivePass + `grants.MissingEffects` detection + `MaterializeGrant` repair), proving the engine→pass→#514-grants→AUTO-repair→idempotent path end-to-end (`TestConverge_DeriveGrantEffectMissing`). Finding identity is the **qualified slug only** (no short codes — Paul, decided 2026-06; `ConvergeFinding.Type`, regex-format CHECK in migration 008, doc §1 + §5 tables all slugs). Approach: #511 reframes+extends the existing `internal/reconcile` pull→findings-ledger→idempotent-applier→auto-vanish spine (not greenfield); ONE unified engine (hard cut — legacy PS-* diff deleted in Phase F). NEXT in Phase D: `derive.grant_effect.excess` (gated clawback via #514 `RevokeGrant`) + `.mismatch`, then `derive.grant.*`, then LIFE + CON. Original plan follows. — Replaces the two split consistency mechanisms — `internal/reconcile` (PS-1..PS-10, local-vs-processor) and `internal/audit` (P-E/S-E/SS, local-vs-local) — with ONE system: `reconcile` becomes a pure provider-state pull that overwrites the local mirror, plus a continuously-running **Convergence Engine** that drives every grant effect (entitlements / credits / product-access) and external decision into a consistent state with the source events, derived from the **#514 grant ledger**.

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

### B. The pull (`reconcile` rework) — CLI DESIGN LOCKED + SHIPPED (Paul 2026-06-18)
- [x] Rename the `reconcile` command → **`pull-provider`** (chosen over `pull-transactions`/`pull-data`). `report` kept as a subcommand; `check`/`fix` removed in favor of the flag model. DONE: `cmd/openrails/reconcile.go` `newPullProviderCmd` + `runPullProvider`, registered in `main.go`; `go run ./cmd/openrails pull-provider --help` shows the new surface; whole-repo `go build ./...` clean. (Preserved #518's `--provider-account` flag + `BuildFetchersWithOptions` binding path.)
- [x] **Dry-run is the DEFAULT (non-destructive).** Bare `pull-provider` runs the engine in advisory mode (discovers divergences, logs, writes nothing); `--overwrite` maps to enforce (applies the mirror upserts). DONE + wired.
- [x] **`--prune` (destructive, opt-in).** DONE + integration-tested (`internal/reconcile/prune.go` `PruneProviderAccountExcess` + `TestPruneProviderAccountExcess`): account-bound (only rows stamped with the pulled `provider_account_id`; legacy NULL-binding rows never touched), absent-from-snapshot detection, honors dry-run unless `--overwrite`. SAFE BY CONSTRUCTION — an excess subscription that fed the #514 grant ledger is SKIPPED (deleting its row would orphan an append-only grant; it is retracted via convergence instead), and an excess payment with protected dependents (grant/refund/entitlement-grant/checkout) is SKIPPED; bare excess rows are deleted in a per-record merchant-scoped transaction (subs: checkout_sessions + entitlements + the row; payments SET NULL via FK; solana cascades). Test covers dry-run (no writes), apply (bare deleted, entangled preserved), idempotency.
- [x] **After a successful `--overwrite` pull: run one-shot `Converge(merchant)`.** DONE: `runPullProvider` invokes `converge.NewConvergeEngine(rt.DB).Converge(Scope{Merchant})` and logs the tally; dry-run never converges.
- [~] Rewrite pull semantics from "diff + selectively apply findings" to "pull complete head state per provider, UPSERT the local mirror (missing→insert, mismatch→overwrite provider-owned fields), log every row old→new". PARTIAL: the CLI surface + dry-run/overwrite/prune + post-pull Converge are SHIPPED, but the missing/mismatch UPSERT path still routes through the existing `internal/reconcile` engine (`Engine.Run` advisory/enforce, PS-* findings). The internal PS-*→`pull.*` finding rename + the pure-overwrite mirror rewrite stay coupled to the provider-pull engine #518 is actively expanding (multi-provider config) → deferred to the full Phase F once #518 settles.
- [ ] Provider-Pull coverage: charges→payments, subscription observed status/period/next-bill, refunds, chargebacks/disputes, vault→payment_methods, and remote catalog/plan observations as evidence for `consistency.amount_mismatch.provider_catalog` only (never as local catalog overwrites). For provider-owned mirror resources in #511, implement only meaningful `missing` and `mismatch` shapes: missing provider fact → materialize/upsert local mirror; differing provider fact → adjust/overwrite provider-owned mirror fields. Do **not** emit `pull.*.excess` in the active reconcile command.
- [ ] Account-bind every pull using #518: resolve the live provider account id using the provider account/profile/whoami endpoint (Stripe account id from `/v1/account`, NMI/Mobius merchant identity from profile report, CCBill account/subaccount identity once implemented); verify the targeted `provider_account_id` row before any provider list is treated as authoritative.
- [ ] Abort on merchant/provider account mismatch (#518): if the targeted provider account row says `account_id_A` and current credentials resolve to `account_id_B`, fail the pull with an operator-facing configuration error. Do not emit `pull.*`, mark a domain reconciled, insert missing rows, or overwrite mismatched rows from the wrong account.
- [ ] Stamp provider-owned mirror rows/run evidence with #518 `provider_account_id` for new writes where possible. Comparisons and overwrites must require the same provider account row; legacy null-binding rows are ambiguous import state and must not be destructively reconciled.
- [ ] Provider-action preservation in the overwrite: never clobber `deletion_scheduled_at`/`scheduled_price_id`/pending intents; compute divergence against (provider-observed state + scheduled future transitions); treat not-yet-due lag as consistent, retryable lag as provider action state, and abandoned past-due work as `life.provider_intent.abandoned`.
- [ ] `--since` / date-range flag for historical backfill; default pulls the head.
- [x] After a successful pull: mark the pulled source domains reconciled (gate) and run a one-shot `Converge(merchant)` pass. DONE in `runPullProvider`: a clean full HEAD pull across ALL providers (no `--provider` filter, no `--since` window — the only case that proves provider-side absence) `UpsertReconciliationState(subscriptions, payments, fully_reconciled=true, last_full_pull_at=now)`, releasing the confirmed-absence gate (§3.2), then runs `converge.Converge(Scope{Merchant})`. A provider-filtered or windowed pull does NOT flip the gate. (Provides the gate's enabling half; consumers are the future gated destructive-EXCESS checks.)

### C. The Convergence Engine (new core)
- [x] `Converge(scope)` — idempotent (clean scope = no run, no writes; verified `TestConverge_CleanScopeIsNoOp`), scope-narrowable (`Scope{Merchant, Customer?, Subscription?}`). `internal/reconcile/converge.go` (`ConvergeEngine.Converge`). Runs DERIVE→LIFE→CON, lazily creates a run only when there are findings, persists to the shared ledger.
- [x] Plane passes run in order DERIVE -> LIFE -> CON; each plane is a module (`Pass` interface; `derivePass`/`lifePass`/`conPass` in `converge_passes.go`, stubs to be filled in Phase D). The finding model (`ConvergeFinding` with Shape/Class/SourceDomain/Repair) carries provider-action remediation via Class + an optional repair closure.
- [ ] derive-1/derive-2 (#514 grants) invoked + verified by the DERIVE pass. (Phase D — pass skeleton wired, calls land with the checks.)
- [ ] Historical replay anchored to source-event timestamps, judged against the grant's `*_spec_snapshot`. (Phase D.)
- [x] Confirmed-absence gate on every EXCESS repair — DONE (`ConvergeEngine.remediate` holds EXCESS as `held` until `IsSourceDomainReconciled`; verified `TestConverge_ConfirmedAbsenceGate`). (Converge-not-replay for LIFE lands with the LIFE pass in Phase D.)

### D. Invariant checks + appliers (catalogue, `docs/consistency-invariants.md` §5)
- [ ] Use `docs/consistency-invariants.md` §5's Postgres-enforced vs application-enforced catalogue as the implementation checklist; first prefer schema hardening for same-merchant references and impossible local states. DB-enforced invariants do not need runtime findings; only the remaining #511 cross-table/cross-time/provider invariants become `reconciliation_findings` rows.
- [ ] DERIVE grant tier: `derive.grant.missing` (event->grant) / `derive.grant.excess` (gated) / `derive.grant.mismatch` — against the #514 grant log; uniform across entitlements/credits/access.
- [~] DERIVE grant-effect tier (customer-scoped): **`derive.grant_effect.missing` + `derive.grant_effect.excess` DONE.** `missing`: `grants.MissingEffects` (live grants whose entitlement-window/credit-deposit effect is absent) → AUTO repair runs derive-2 (`MaterializeGrant`); tested `TestConverge_DeriveGrantEffectMissing`. `excess`: `grants.UnretractedTerminations` (TERMINATED grants whose effect is still live — `GrantHasLiveEntitlement` / `GetCreditLotRemaining>0`) → AUTO repair `MaterializeGrant` retracts (entitlement revoke / credit clawback); NOT gated (grant+revoke both present = recorded decision, not absence); tested BOTH paths under the app role — `TestConverge_DeriveGrantEffectExcess_TerminatedNotRetracted` (entitlement → window revoked) + `TestConverge_DeriveGrantEffectExcess_CreditClawback` (credit → unspent frozen in `revoked_credits`). Both idempotent. (An earlier transient `revoked_credits` RLS failure was a mid-`openrails_db`-rebuild artifact — re-verified clean under the app role; no bug.) **Merchant-wide fan-out DONE:** `derivePass` now runs `runForCustomer` per-customer for a customer scope, and enumerates `ListCustomerIDsWithGrants` for a merchant scope (so the Phase E sweep exercises DERIVE across every customer). REMAINING: `.mismatch` (window/amount/cadence vs grant spec); the true-orphan `.excess` variant (live effect, NO grant → gated/ADMIN).
- [x] LIFE plane (via `lifePass`): **all six checks DONE + integration-tested.** (1) `life.checkout_session.stale`: `ListStaleCheckoutSessions` (expired + non-terminal) → AUTO `ExpireCheckoutSessionByID` (`TestConverge_LifeCheckoutSessionStale`). (2) `life.subscription.grace_exhausted` (marquee converge-not-replay): `ListGraceExhaustedSubscriptions` (past_due + `grace_ends_at < now`) → AUTO `ReconcileCancelSubscriptionLocal` (cancel NOW) + `ReconcileRevokeSubscriptionEntitlements` with `now=grace_ends_at` (revoke AS-OF grace end — no replayed dunning charges) (`TestConverge_LifeSubscriptionGraceExhausted`: status=cancelled, entitlement revoked_at == grace_ends_at, idempotent). (3) `life.subscription.period_overdue`: active sub past period end → AUTO `MarkSubscriptionPastDueFromOverdue` (past_due + grace dated to period_end+48h) (`TestConverge_LifeSubscriptionPeriodOverdue`). (4) `life.subscription.pending_stale`: `pending` unconfirmed > 72h → AUTO cancel (expired) (`TestConverge_LifeSubscriptionPendingStale`). (5) `life.subscription.dunning_overdue`: past_due still in grace but `next_retry_at IS NULL` (stalled schedule) → AUTO `SetSubscriptionNextRetry(now)` to resume dunning — converge-not-replay (schedules the NEXT retry, never re-runs missed attempts) (`TestConverge_LifeSubscriptionDunningOverdue`; also verified composing on top of period_overdue across passes to a true fixpoint). (6) `life.provider_intent.abandoned`: terminal/expired provider intent → ADMIN, surface-only, no auto-repair (`TestConverge_LifeProviderIntentAbandoned`). All time-driven EXCESS/MISMATCH/MISSING → AUTO, NOT confirmed-absence gated; all idempotent. REMAINING (deferred): the linked provider-cancel ACTION for grace_exhausted (needs the Phase E provider-action emission wiring).
- [ ] Provider actions are remediation, not findings: cancel still-active extras, retry scheduled provider decisions, push catalog plans, or surface pending manual action through `provider_intents`/operator queue.
- [~] CON is limited to three generic consistency classes, encoded directly as qualified finding types: duplicates (`consistency.duplicate.provider_charge` for already-collected duplicate money, `consistency.duplicate.remote_subscription` for extra active billable remote subscriptions before duplicate money is collected, `consistency.duplicate.invoice_usage`, `consistency.duplicate.invoice_payment`), amount/explainability mismatch (`consistency.amount_mismatch.refund_math`, `consistency.amount_mismatch.payment_amount`, `consistency.amount_mismatch.invoice_math`, `consistency.amount_mismatch.usage_coverage`, `consistency.amount_mismatch.credit_lot`, `consistency.amount_mismatch.provider_catalog`), and reference resolution (`consistency.reference.source_reference`, `consistency.reference.ledger_reference`, `consistency.reference.provider_intent_reference`). Do not create normal findings for #512 ledger conservation, pending resolver uniqueness, FK-backed catalog/payment/checkout/subscription scope references, or subscription price/product coherence; those are schema/diagnostic/import-repair concerns. **CON plane BOOTSTRAPPED + integration-tested:** `conPass.Run` implements `consistency.reference.source_reference` (an entitlement whose polymorphic `source_type/source_id` resolves to no row — reuses the existing `AuditEntitlementOrphanSubscriptionSource`/`AuditEntitlementOrphanPaymentSource` queries, RLS-scoped so it also catches wrong-merchant refs); MISMATCH → ADMIN, surface-only (no auto-repair — an operator decides), entitlement left untouched (`TestConverge_ConReferenceSourceReference`: admin_pending + requires_admin, idempotent upsert). The duplicate.* and amount_mismatch.* subtypes layer onto this same harness (provider_charge/refund_math need the Phase B provider pull for provider-observed data; credit_lot/invoice_math are pure-local arithmetic and can land next). REMAINING: those subtypes.

### E. Continuous convergence wiring
- [ ] Inline hooks: `Converge(customer|subscription)` synchronously after checkout completion, renewal billing, refund/webhook ingestion, dunning transitions, admin grant/revoke.
- [x] Background sweep (River worker): **DONE + integration-tested end-to-end.** `internal/river/jobs_converge_sweep.go` `ConvergeSweepWorker` reads the active-merchant directory on a privileged no-GUC connection (`ListActiveMerchantIDs`; merchants is control-plane), then for each runs `reconcile.Converge(Scope{Merchant})` inside its own `RunInMerchantConn` (RLS-scoped); one merchant's failure is logged + skipped, never aborts the sweep. Registered in `internal/app/river_register.go` (worker + a 15-min `RunOnStart` periodic job, queue=billing). To make the merchant-scoped sweep exercise DERIVE across all customers, `derivePass` now fans out: customer-scope checks that one customer, merchant-scope enumerates `ListCustomerIDsWithGrants` and runs the same `runForCustomer` checks for each (LIFE queries already accept a nil customer = whole merchant). `TestConvergeSweepWorker_RemediatesDriftAcrossMerchant` seeds a stale checkout session + a grace-exhausted sub, runs `worker.Work` exactly as River would (no inline hook), and asserts BOTH converged (session expired, sub cancelled, entitlement revoked AS-OF grace end, 2 findings auto_fixed) + idempotent on a second sweep. This is the keystone: the engine now RUNS in the server and autonomously remediates drift no request path touched.
- [ ] Server-running convergence and the CLI one-shot share the same `Converge` core.

### F. Retire the split mechanisms
- [x] Fold `internal/audit` checks into the engine + delete the package + the `openrails audit` command (HARD CUT, Paul 2026-06-18). DONE: mapped every audit check to its convergence home — S-E-1/AG-1 (active sub / admin grant missing entitlement) → `derive.grant_effect.missing`; S-E-3/P-E-3/AG-4 (cancelled-sub / refunded-payment / expired-grant with live effect) → `derive.grant_effect.excess`; SS-1 → `life.subscription.period_overdue`; SS-3 → `life.subscription.dunning_overdue`; T-1 → `life.subscription.pending_stale`; T-2 → `life.subscription.grace_exhausted`; S-E-2/P-E-2/FK-5 (orphan subscription/one_off entitlement source) + AG-2 (orphan admin-grant source) → `consistency.reference.source_reference`; D-2 (duplicate charges same period) → `consistency.duplicate.provider_charge` (`conPass` reuses the `AuditEntitlementOrphan*` / `AuditOrphanAdminEntitlements` / `AuditDuplicateChargesSamePeriod` queries, RLS-scoped; `TestConverge_ConReferenceSourceReference` + `TestConverge_ConDuplicateProviderCharge`). PM-1/PM-2 (failed/expired payment method on active sub) + T-4 (distant-future start, "not enforced") are billing-viability/risk warnings, NOT consistency findings (doc §billing-viability) → dropped. Deleted `internal/audit/` + the `audit` cobra command + `runAudit` in `cmd/openrails/main.go`. Whole-repo `go build ./...` clean. TAIL: the now-unused `Audit*` queries in `audit_checks.sql` are dead (only the four above are referenced) — a cosmetic regen-cleanup deferred to avoid an unnecessary regen mid-#518-churn.
- [~] Replace `internal/reconcile` old `PS-*` finding types + selective-apply with Provider-Pull + the DERIVE/LIFE/CON Convergence Engine; update the store, report rendering, CLI help. **TAXONOMY UNIFICATION DONE (Paul 2026-06-18, #518 landed):** the `PS-1..PS-10` finding-type CODES are fully retired — `internal/reconcile/findings.go` now emits qualified four-plane slugs directly: PS-1→`pull.subscription.missing`, PS-2→`pull.subscription.dead`, PS-3→`pull.subscription.mismatch`, PS-4→`pull.charge.missing`, PS-5→`pull.refund.missing`, PS-6→`pull.dispute.chargeback`, PS-7→`pull.payment_method.mismatch`, PS-8→`consistency.duplicate.remote_subscription` (CON — pull detects it, classified consistency), PS-9→`derive.grant_effect.mismatch` (DERIVE), PS-10→`life.provider_intent.stuck` (LIFE). Migration 008's `chk_reconciliation_findings_type` CHECK dropped the transitional `PS-*` UNION — slugs only now; the Phase A test asserts PS-* is REJECTED. Whole-repo `go build`, full `internal/reconcile` + `converge` integration suites green. REMAINING (deferred, lower-value): the engine's internal "selective-apply" is still its own diff+apply machinery (works, emits slugs) rather than a literal "pure overwrite"; and the PS-8/9/10 checks live in the pull engine rather than physically moved into Converge's CON/DERIVE/LIFE passes (they emit the right plane's slug, so the ledger taxonomy is unified — the physical relocation is cosmetic). The pull engine's fetch+diff+apply is the PULL-plane implementation and is NOT deleted (it's the provider mirror sync #518 enhanced); only the PS-* code namespace was retired.
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

### I. Entitlements + ownership on the grant ledger (DECISION + PLAN, Paul 2026-06-18)
**Why this exists:** the DERIVE plane can only converge what is *grant-derived*. Today **only credits** flow through the #514 grant ledger (`money_service` → `grants.Grant(kind=credit)` → `MaterializeGrant`). Subscription/manual/grace **entitlements** are still created DIRECTLY (`entitlements.PushNewEntitlement`, the legacy `entitlement_grants` provenance table) and **ownership** directly (`product_access_grants` table). So DERIVE is blind to entitlements + ownership, and `derive.grant.*` can't exist. Decision (Paul): unify ALL THREE grant kinds onto the ledger; **retire the legacy tables — the grant ledger is the sole source of truth.** (Reminder for future agents: openrails grant KINDS = `entitlement` / `credit` / `ownership`. `source_type` = provenance only (`purchase`/`subscription`/`admin`/`grace`); `admin` = "operator-granted by hand", it is NOT auth — the old `admin_grants` table was renamed to `entitlement_grants` exactly to stop that confusion, and all leftover `admin_grants` names were scrubbed 2026-06-18. AuthKit roles/permissions are a separate system; openrails never deals in roles.)

**Key technical finding (why it's an atomic, system-wide change, not a small increment):** for DERIVE to link a grant to its effect, `MaterializeGrant` stamps the projected entitlement `source_type='grant'`, `source_id=grant.id` (that's how `EntitlementExistsForGrant` finds it). So once entitlement *creation* routes through grants, every entitlement's `source_type` flips from `subscription`/`admin`/`grace` → `grant`. That atomically breaks every reader keyed on the old source_type — `ReconcileRevokeSubscriptionEntitlements` (`WHERE source_type='subscription'`, used by `life.subscription.grace_exhausted` + pull `subscription.dead`), the CON orphan-source checks, `ListDistinctEntitlementNamesBySource`, analytics — all must rewire to resolve via the grant. Ownership has NO projection ("the grant row IS the record"), so it has no such ripple — hence sequence ownership first.

**Simplest/most-extensible approach (Paul's criterion):** ONE creation chokepoint per kind, rewired internally to the ledger; all callers route through it untouched. Confirmed `entitlements.PushNewEntitlement` is the sole entitlement chokepoint (16 callers) and `productaccess.Service.GrantProductAccess` the ownership one. No import cycle (`grants` imports only `money/ledger`; neither imports `entitlements`/`productaccess`), so those modules can call `grants`.

**Sequenced plan (incremental, each verified green before the next):**
1. **Ownership first** (cleanest, no projection ripple): back `productaccess.Service` with `grants(kind=ownership)` — `GrantProductAccess`→`grants.Grant`, `Has/ListAccessible`→live-ownership-grant queries, `Revoke*`→`RevokeGrant`. Add grant-layer ownership query helpers. Then drop `product_access_grants`.
2. **Entitlements**: rewire `PushNewEntitlement`'s terminal insert to `grants.Grant(kind=entitlement)+MaterializeGrant` (keep its timeline-window computation), AND atomically rewire the source-keyed readers above to resolve via the grant. Then drop `entitlement_grants`.
3. **CON check adapts**: `consistency.reference.source_reference` becomes "entitlement `source_type='grant'` whose grant row is missing" (replacing the per-source-type orphan variants).
4. Unblocks `derive.grant.*` (event→grant) — now meaningful since every entitlement/ownership IS a grant.

**Status:** IN_PROGRESS — ownership flow (step 1) scoped + approach locked 2026-06-18.
**Ownership step-1 execution recipe (simplest, lowest blast radius — verified feasible):** reimplement `internal/db/repo/product_access_grant.go`'s 8 methods against the grant ledger via the `grants` module (no import cycle — `grants` imports only `money/ledger`), Go-side filtering, **no new SQL queries**: Insert→`grants.New(gen,merchant).Grant(GrantInput{Kind:Ownership,Product,Source,SourceID,Payment,EndsAt})`; GetBySource→`ListGrantsByCustomer` filter kind=ownership+product+source_id (most recent); HasActiveAccess→`LiveGrants(customer)` any kind=ownership+product; ListActiveByUser→`LiveGrants` filter kind=ownership; ListByUser→`ListGrantsByCustomer` kind=ownership + per-row `IsGrantTerminated` for status; GetByID→`GetGrant`; RevokeByID→`grants.Revoke(grantID,reason)`; RevokeByPayment→`ListGrantsByCustomer` filter kind=ownership+payment+live → `Revoke` each. Map `gen.OpenrailsGrant`→`models.ProductAccessGrant` (status='revoked' iff `IsGrantTerminated`; revoked_at/reason from the termination event; updated_at→created_at). The Service + all callers stay UNTOUCHED (repo-layer swap). Then drop `product_access_grants` table + its ~8 sqlc queries + the `OpenrailsProductAccessGrant` gen type; regen. **Verify against the 4 existing integration tests** (`TestGrantProductAccess_Idempotent`, `TestHasProductAccess_And_List`, `TestRevokeProductAccessByPayment_OnRefund`, `TestRevokeProductAccess_ByID`) — green = correct. This is the precise resume point. Access/money-critical (paid-content access), so executed as a fully-tested unit.

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
