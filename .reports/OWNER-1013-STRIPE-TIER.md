Owner: /root/stripe_cut_finish (resumed from /root/consumer_helpers_current)
Purpose: #1013 consolidate Stripe tier-change integration workflows without losing payment invariants
Branch: refactor/1013-stripe-tier-workflows-20260922
Base: fetched origin/master d3377556f4bd2ed6cd0ae3cfa7f7139324983561
Worktree: /home/fidika/cozy/.worktrees/openrails/1013-stripe-tier-workflows-20260922
Initial scope: internal/modules/checkout/stripe_tier_change_integration_test.go plus ownership/review evidence. Root-authorized qualification extension: exact overlap-fixture cleanup and fresh merchant ownership in newSaleIntentFixture/direct callers. No production, HTTP or lifecycle_failopen changes. Root owns review/merge/tracker.
Qualification: real PostgreSQL via approved shared allocator creating task-unique test databases; existing guarded runner blocks non-loopback HTTP. Never drop allocator or foreign resources.

Focused race/shuffle qualification passed in 178.183s with zero failures/skips and zero external HTTP attempts. Complete checkout package is now running under the same guard. Full assertion map is .reports/stripe-tier-pr.md and will be included in the draft PR body.

2026-09-22 fixture qualification: initial full checkout race/shuffle seed1790060085784874899 found two pre-existing NMI fixture-order dependencies; immutable baseline predecessor/victim overlay reproduced one. Exact8line overlap cleanup passed unchanged pair (24.351s). Entire Stripe family passed49events/0fail/0skip in183.631s. Root authorized merchant-scoped sale-fixture isolation preserving every batch-count assertion and the intentional same-merchant cross-customer key race. Final full package run on the original seed follows compile; assertion map and evidence are in draft PR615.

Final local qualification: existing full checkout run completed with 434 passing test events, 0 failures, 0 skips, and 382.802s package elapsed under race, integration tags, count=1, and original shuffle seed 1790060085784874899. Guard unexpected=0/background_fx=0. Stripe source 1010 to 933 lines; whole PR test source net -69 lines including fixture corrections. Independent 21_660_000-micro amount oracle retained. The pending fixture diff was reviewed without weakening request, receipt, recovery or batch-count assertions; immutable facts remain until owned package database teardown. Exact-head CI and root independent review/merge remain required.
