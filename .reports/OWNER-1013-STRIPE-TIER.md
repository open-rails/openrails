Owner: /root/consumer_helpers_current
Purpose: #1013 consolidate Stripe tier-change integration workflows without losing payment invariants
Branch: refactor/1013-stripe-tier-workflows-20260922
Base: fetched origin/master d3377556f4bd2ed6cd0ae3cfa7f7139324983561
Worktree: /home/fidika/cozy/.worktrees/openrails/1013-stripe-tier-workflows-20260922
Scope: internal/modules/checkout/stripe_tier_change_integration_test.go only, plus ownership/review evidence. No production, HTTP or lifecycle_failopen changes. Root owns review/merge/tracker.
Qualification: real PostgreSQL via approved shared allocator creating task-unique test databases; existing guarded runner blocks non-loopback HTTP. Never drop allocator or foreign resources.

Focused race/shuffle qualification passed in 178.183s with zero failures/skips and zero external HTTP attempts. Complete checkout package is now running under the same guard. Full assertion map is .reports/stripe-tier-pr.md and will be included in the draft PR body.
