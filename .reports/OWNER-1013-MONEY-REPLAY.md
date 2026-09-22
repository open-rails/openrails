Owner: /root/stripe_cut_finish
Purpose: #1013 first money replay/concurrency test consolidation slice; no production, invoice or Stripe adapter edits
Branch: refactor/1013-money-replay-workflows-20260922
Base: fetched origin/master 64eb7bd961f73e612a92720f519676df246b51a1
Worktree: /home/fidika/cozy/.worktrees/openrails/1013-money-replay-workflows-20260922
Scope: apply_idempotent, idempotency_key, unified_spend, deposit_once_only integration tests and money/ledger/replay integration assertions.
Qualification: complete affected money and ledger packages with real task-owned PostgreSQL, race/shuffle, GOMAXPROCS=2 and -p=1 through existing local nonloopback HTTP guard. Never clean foreign databases or worktrees.
Root owns independent review, merge and tracker. No merge or tag authorized for this lane.
