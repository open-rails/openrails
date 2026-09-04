# SEC-24 residual hardening

Owner: Codex authkit_enrollment_fix agent
Issue: /home/fidika/open-rails-tracker/openrails/SEC-24.md
Branch: fix/sec24-residuals
Base: 271a6f81e13b15ac80c0fb5df811ef17d78c7fad (origin/master)
Worktree: /home/fidika/cozy/.worktrees/openrails/sec24-residuals

Scope: required authentication for Solana enrollment, deletion of the unused CCBill FlexForm password field, and API-key Host merchant consistency.
Previously completed SEC-24 work: inbound weak callback verifier deletion, AEAD row binding, merchant-scoped checkout uniqueness, provider URL restrictions, secret-name traversal rejection, and accurate rejected-webhook responses. Those implementations remain intact.

## Delivered

- `RegisterUserRoutes` attaches its existing required authenticator to Solana recurring enrollment. The handler and wallet-proof implementation are unchanged.
- Removed `GenerateFlexFormURLParams.Password` and its query serialization. The only production caller, `CheckoutService.processCCBillSubscription`, never populated it; all other indexed callers are tests. DataLink account credentials and the outbound FlexForm signature remain intact.
- `ResolveAPIKey` rejects a Host-pinned merchant different from its authenticated group's merchant. A dedicated error maps to the existing HTTP 403 `host_merchant_mismatch`; deployments without a Host pin retain their existing behavior.

## Proof

Before the fix, regression tests showed Solana enrollment bypassing the authenticator, FlexForm emitting a password query field, and real API keys succeeding with HTTP 200 against the other merchant's Host.

After the fix:
- Race suites passed for internal/http/routes, internal/controlplane, internal/integrations/ccbill, and internal/modules/checkout.
- Real HTTP + PostgreSQL tests passed for API-key matching/mismatching/no-Host cases and existing JWT/user-session/late-host-registration behavior.
- `go build ./...`, targeted `go vet`, and `git diff --check` passed.

Integration tests used isolated database `or_sec24` on the audit-owned PostgreSQL port 15434 and Redis port 16380, with fixture merchants and no live provider operations.
PR: https://github.com/open-rails/openrails/pull/330
Root agent owns merge and SEC-24 tracker closure.
