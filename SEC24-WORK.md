# SEC-24 residual hardening

Owner: Codex authkit_enrollment_fix agent
Issue: /home/fidika/open-rails-tracker/openrails/SEC-24.md
Branch: fix/sec24-residuals
Base: 271a6f81e13b15ac80c0fb5df811ef17d78c7fad (origin/master)
Worktree: /home/fidika/cozy/.worktrees/openrails/sec24-residuals

Scope: required authentication for Solana enrollment, deletion of the unused CCBill FlexForm password field, and API-key Host merchant consistency.
Previously completed SEC-24 work: inbound weak callback verifier deletion, AEAD row binding, merchant-scoped checkout uniqueness, provider URL restrictions, secret-name traversal rejection, and accurate rejected-webhook responses. Those implementations remain intact.
