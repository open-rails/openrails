# SEC-18 merchant predicates

Owner: Codex authkit_enrollment_fix agent
Issue: /home/fidika/open-rails-tracker/openrails/SEC-18.md
Branch: fix/sec18-merchant-predicates
Base: 271a6f81e13b15ac80c0fb5df811ef17d78c7fad (origin/master)
Worktree: /home/fidika/cozy/.worktrees/openrails/sec18-merchant-predicates

Scope: explicit merchant UUIDs for reprice reads/cancellation and remaining customer-admin billing reads. The ENV and universal RLS startup fixes already exist; credit balances are already explicitly merchant-scoped. No GUC-only predicate or new authentication framework is introduced.
