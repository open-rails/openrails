# SEC-18 merchant predicates

Owner: Codex authkit_enrollment_fix agent
Issue: /home/fidika/open-rails-tracker/openrails/SEC-18.md
Branch: fix/sec18-merchant-predicates
Base: 271a6f81e13b15ac80c0fb5df811ef17d78c7fad (origin/master)
Worktree: /home/fidika/cozy/.worktrees/openrails/sec18-merchant-predicates

Scope: explicit merchant UUIDs for reprice reads/cancellation and remaining customer-admin billing reads. The ENV and universal RLS startup fixes already exist; credit balances are already explicitly merchant-scoped. No GUC-only predicate or new authentication framework is introduced.

## Delivered

- Reprice and batch ID reads, reprice cancellation, and their admin list queries take an explicit merchant UUID.
- Customer subscription, entitlement, payment, and payment-method reads are scoped explicitly. Payment and payment-method pagination counts use the same predicate as their item queries.
- Repository methods derive the UUID through merchant.Require(ctx), preserving context-scoped worker calls without adding a dependency on the connection GUC.
- Active entitlement records reuse the existing merchant-scoped query; the redundant unscoped query and generated implementation were deleted.
- No schema migration, RLS policy, ENV behavior, credit-balance implementation, or lifecycle mutation was changed.

## Validation

- Failing-before regression reproduced foreign reprice reads/cancellation, foreign customer billing reads, and successful unscoped calls with no merchant context.
- The repaired regression uses a deliberately RLS-bypassing pool, asserts its merchant GUC is absent, and proves both denied foreign access and successful own-merchant operations.
- Integration race tests passed for query contracts, reprice flows, plan migration, the re-driver, facade RLS, and real customer/subscription/payment HTTP isolation under the enforcing role.
- Unit race suites passed for subscriptions, payments, paymentmethods, and pkg/service.
- Full build, sqlc generation/vet, query audit, SQL lint, migration lint, and diff checks passed.

Tests use isolated fixtures through the audit PostgreSQL on port 15434 and Redis on port 16380. The separate or_sec18 database supplies the migrated schema for sqlc checks. No provider or secret operations were performed.

PR: https://github.com/open-rails/openrails/pull/331
Root owns merge and tracker closure. Subscription query/repository overlap with #933 is limited to the existing active-customer list; this patch does not touch its row-locking changes.
