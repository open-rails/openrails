# #1013 money replay consolidation assertion map

Owner: /root/stripe_cut_finish.
Base: fetched origin/master 64eb7bd961f73e612a92720f519676df246b51a1.
Draft PR: https://github.com/open-rails/openrails/pull/619.
Scope: five test files; no production, invoice or Stripe adapter changes.

The five files total 1,078 lines before and 875 after: **203 fewer test-source
lines**, including the added ledger assertion and the two-line fixture correction.
Consolidation alone removes 205 lines; qualification adds two fixture-accounting
lines. The one-line LED-15 documentation pointer update has zero net lines. This is the actual current diff,
separate from the earlier 180–220-line estimate. Ten old top-level test roots
become one new workflow plus existing stronger survivors; distinct operation
entrypoints and direct database/concurrency proofs remain exercised.

## Old test to surviving assertions

All filenames below are under `internal/modules/money/`.

| Previous test | Survivor and preserved assertions |
| --- | --- |
| `unified_spend_integration_test.go`: `TestSpendCredits_Idempotent` | `apply_idempotent_integration_test.go`: `TestOr892_AReplayDoesNotRecomputeTheSpendAgainstTheDebitedBalance`. First spend succeeds, subsequent same-key calls succeed without another debit. Uses the stronger 500-funded/300-spent/200-remaining values instead of the former 1,000/300/700, so replay would expose recomputation against insufficient remaining funds. |
| `idempotency_key_integration_test.go`: `TestOr891_KeyedSpendReplayMovesNoMoney` | Same depleted-balance survivor now makes three total identical calls (first plus two replays), matching the old repetition count. First `Replayed=false`; both replays `true`, with the original transaction ID. Exact final balance remains 200. |
| `idempotency_key_integration_test.go`: `TestOr891_ConcurrentKeyedSpendsDebitOnce` | `apply_idempotent_integration_test.go`: `TestOr892_ConcurrentIdenticalSpendsApplyExactlyOnce`. Six goroutines, now released through a common start barrier; all return without error; exactly one response reports applied. Uses the existing stronger survivor's 10,000-funded/2,500-spent/7,500-final values rather than the deleted test's 1,500-spent/8,500-final values. Both proved one exact debit, and the applied-response count is retained. |
| `apply_idempotent_integration_test.go`: `TestOr892_ApplyIdempotentReportsReplayInsteadOfErroring` | `ledger/replay_integration_test.go`: `TestLedger_ReplayPreservesCountersAndDepletedBalance`, in both deposit and depleting-spend branches. The forced overlapping calls produce exactly one applied result and one replay, no errors, and the same transfer ID; a later replay retains that ID and reports not applied. The exact one-row assertion is carried over and scopes merchant, customer, operation, source and source ID. Balance, counter-drift and ledger-net-zero assertions remain. The 4,000-unit duplicate fixture is removed; existing branches use 1,000 units and final balances 1,000/0. |
| `apply_idempotent_integration_test.go`: `TestOr892_WritesReportAppliedVersusReplayed` | `TestMoneyWritesPreserveAcceptedAmountAndReplay` retains first=false/replay=true for Spend and Capture. Deposit flags move to `TestDepositReplayPreservesFinancialTermsAndReceipt`. The new workflow also checks first/replay flags for Withdraw and RecordUsage. Independent balance checkpoints verify only accepted writes move money. |
| `idempotency_key_integration_test.go`: `TestOr891_SpendReusedKeyChangedAmountIsRefused` | New direct workflow calls Spend for 1,000, then 4,000 under the same key; requires `ErrIdempotencyKeyReused`, `IdempotencyConflict`, `Committed=1,000`, `Retried=4,000`, `Field="amount"`. Balance is 9,000 immediately after rejection and again after the successful 1,000 replay. |
| `idempotency_key_integration_test.go`: `TestOr891_CaptureReusedKeyChangedAmountIsRefused` | Same workflow calls Capture for 400, then 4,000 under the same key; requires typed refusal, then successful nonnil 400 replay with `Replayed=true`. Balance is 8,600 before and after replay because this payer already spent 1,000. The old isolated 9,600 result is preserved as the same 400 delta within a cumulative sequence. |
| `idempotency_key_integration_test.go`: `TestOr891_WithdrawReusedKeyChangedAmountIsRefused` | Same workflow calls Withdraw for 1,000, then 2,500 under the same payout UUID; requires typed refusal. Balance is 7,600 immediately after rejection and after the now-explicit successful 1,000 replay; the unchanged delta is 1,000. |
| `idempotency_key_integration_test.go`: `TestOr891_RecordUsageReusedKeyChangedAmountIsRefused` | Same workflow calls RecordUsage for event `invoke`, amount 700, then 7,000 under the same usage-operation coordinate; requires typed refusal. Balance is 6,900 immediately after rejection and after a successful replay reporting `Replayed=true`. The changed amount remains affordable before the failed retry, so insufficient funds cannot substitute for the required conflict. |
| `deposit_once_only_integration_test.go`: `TestOr906_MoneyDepositRefusesAChangedAmount` | `TestDepositReplayPreservesFinancialTermsAndReceipt` now explicitly checks first=false/replay=true. It retains amount-conflict refusal and original receipt ID through full receipt equality, including GetDepositBySourceID. Existing amount/currency/expiry/permanent conflicts, normalized currency, clock advance, immutable diagnostic labels, USD balance and zero EUR balance are unchanged. The old 5,000/10,000 amount-only case is subsumed by the existing 1,000,000/1,000,001 precision-sensitive case. |

## Unchanged proof boundaries

The following eight function bodies were compared byte-for-byte with the base
and remain unchanged:

- `TestOr892_TheLedgerRefusesABlankCoordinate`.
- `TestOr906_TheDatabaseRefusesADuplicateDepositGrant`.
- `TestOr891_SpendCreditsRefusesAnEmptyKey`.
- `TestOr891_WithdrawRefusesAnEmptyKey`.
- `TestOr891_OwedLegAccruesOneInvoiceItemPerKey`.
- `TestOr891_KeylessSpendCannotReachTheOwedLeg`.
- `TestLedger_RollbackAndRejectedInsertPreserveCounters`.
- `TestLedger_ConcurrentTransfersWithAccountReferences`.

No production method changed. The raw duplicate-coordinate fixture now accounts
for prior clearing transfers as described below; all other fixtures are unchanged.
Raw generated database writes still
prove uniqueness without Go-side guards; blank coordinates/keys still fail;
the owed-leg test still installs a real credit line and asserts one invoice item
after three calls; ledger forced interleavings, rollback and account-reference
locks remain. The ordinary prepaid/arrears/credit-limit cases in unified_spend
are unchanged apart from deletion of its redundant replay test.

## Qualification-discovered fixture correction

The first complete race/shuffle run used seed `1790067505117462480` and reported
323 passing test events, three opt-in live-provider skips, and one failure:
`TestOr892_TheDatabaseRefusesADuplicateCoordinate` expected the shared merchant's
clearing account to contain only this test's debit. Its body was unchanged from
the base. Earlier tests legitimately populate that same system account.

An immutable overlay restores the exact base apply_idempotent file and appends
only an ordered wrapper calling the existing permanent-deposit predecessor and
raw duplicate test. It reproduces the failure under race: expected -5,000,
actual -6,000 (1,000 prior funding plus the test's one 5,000 debit). Evidence:
`.reports/baseline-shared-clearing.jsonl`, package elapsed 3.971s, guard
unexpected=0/background_fx=0.

Root approved the narrow correction: read clearing balance before either raw
INSERT, then assert final clearing equals the starting balance minus exactly
5,000. The raw duplicate `pgx.ErrNoRows`, one row/5,000 total, and fresh customer
credit assertions remain intact. No immutable fact is deleted and no exact
assertion becomes an inequality. This is separate from the consolidation savings.
The LED-15 invariant table now points to the surviving money-write workflow.

## Qualification

Integration-tag compilation of both packages passed before final balance checks.
The first complete run exercised every changed workflow successfully; its only
failure was the baseline clearing assumption above. The fixed ordered pair and
final complete packages passed on the same shuffle seed:

`GOMAXPROCS=2 GOWORK=off go test -json -race -count=1 -shuffle=1790067505117462480 -p=1 -tags=integration ./internal/modules/money ./internal/modules/money/ledger`

The existing guarded runner blocks nonloopback HTTP and uses the approved local
allocator only to create task-owned databases. The complete suite contains three
explicitly opt-in live-provider tests: live NMI invoice, live Stripe invoice, and
NMI sandbox collection. Their opt-in flags/credentials are intentionally absent;
report them as skips, not as local-provider qualification.

Final complete qualification passed on shuffle seed `1790067505117462480`:

- Money: 311 passing test events, zero failures, three opt-in live-provider skips; 134.914s package elapsed.
- Ledger: 13 passing test events, zero failures/skips; 7.835s package elapsed.
- Combined: 324 passing test events, zero failures, three explicit skips.
- HTTP guard: unexpected=0, background_fx=0; runner exit zero.
- Corrected ordered predecessor/raw-proof pair: three passes, zero failures/skips, 3.812s.

Final-source hashes are retained in `.reports/qualification-source-final.json`;
full receipts are `.reports/money-ledger-race-shuffle-final.jsonl` and
`.reports/fixed-shared-clearing.jsonl`. Each changed test file still matches the
source used by the final complete run. `git diff --check` passes. Exact-head CI
and root independent review remain merge gates.

No provider write, merge or release is part of this task.
