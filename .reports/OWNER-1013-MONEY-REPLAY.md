Owner: /root/stripe_cut_finish
Purpose: #1013 first money replay/concurrency test consolidation slice; no production, invoice or Stripe adapter edits
Branch: refactor/1013-money-replay-workflows-20260922
Base: fetched origin/master 64eb7bd961f73e612a92720f519676df246b51a1
Worktree: /home/fidika/cozy/.worktrees/openrails/1013-money-replay-workflows-20260922
Scope: apply_idempotent, idempotency_key, unified_spend, deposit_once_only integration tests and money/ledger/replay integration assertions.
Qualification: complete affected money and ledger packages with real task-owned PostgreSQL, race/shuffle, GOMAXPROCS=2 and -p=1 through existing local nonloopback HTTP guard. Never clean foreign databases or worktrees.
Root owns independent review, merge and tracker. No merge or tag authorized for this lane.

Root-approved qualification extension: exact starting-balance delta for the shared clearing account in the retained raw duplicate-coordinate proof, after immutable base reproduction. Update the LED-15 surviving-test pointer in docs/invariants.md. No financial fact cleanup. Consolidation -205 test lines; fixture correction +2; net -203.

Final guarded race/shuffle: money311passes/0fail/3live-opt-in-skips134.914s; ledger13passes/0fail/0skip7.835s; seed1790067505117462480; HTTP unexpected0/background_fx0. Exact old-to-new map and baseline/fixed fixture receipts: .reports/MONEY-REPLAY-ASSERTIONS.md.
