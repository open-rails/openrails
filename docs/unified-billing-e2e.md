# Unified-billing e2e (#244)

Two complementary harnesses prove the unified credit money path
(estimate → authorize+hold → capture/release) on OpenRails' standalone public
OAT surface — the exact contract gen-orchestrator / Tensorhub call (#233/#222).

## 1. In-repo Go harness (CI) — `tests/unified_billing_e2e_test.go`

Build tag `integration`. Drives the lifecycle through the OAT-authed public
routes (`/v1/service/credits/*`) against a test Postgres, then asserts ledger
rows/statuses/balances directly. Scenarios:

- prepaid hold → **full** capture (balance reduced by full amount, held → 0)
- prepaid hold → **partial** capture (only the metered actual debited, remainder
  released, credits conserved)
- **insufficient balance** → 402 `insufficient_credits`, balance untouched, no hold row
- **failure release** → hold fully restored, nothing debited
- **idempotent replay** → same (source, source_id) returns the same hold, no double-reserve
- **owner scoping** → owner A's hold/capture never touches owner B

```
go test -tags integration -run TestUnifiedBilling ./tests/
```

> Needs working testcontainers (Postgres+ClickHouse+Redis). On hosts where the
> ClickHouse container handshake flakes, use the deployed-stack harness below,
> which is the stronger proof anyway (real deployed service + its own DB).

## 2. Deployed-stack harness — `scripts/unified_billing_e2e.sh`

POSIX sh + curl. Hits a *running, standalone* OpenRails over the public OAT
routes — fresh credit type per run, so balances are deterministic. Asserts:
create credit-type → deposit → GET balance (#247) → atomic authorize+hold (#235)
→ partial capture → balance reflects actual → over-balance authorize DENIED
(prepaid gate) → set arrears + cap (#242) → read settings back.

Run against `~/cozy/e2e` (openrails:2053 is not host-published, so run from a
container on the e2e network):

```sh
OAT=$(docker compose exec -T openrails /app/billing-server \
  --config /app/config/openrails.config.yaml mint-operator-oat \
  | grep -o '"oat_secret":"[^"]*"' | cut -d'"' -f4)

docker run --rm --network e2e_default \
  -v "$PWD/scripts/unified_billing_e2e.sh:/h.sh:ro" \
  -e OPENRAILS_OAT="$OAT" -e BASE_URL=http://openrails:2053 \
  --entrypoint sh alpine/curl:latest /h.sh
# => 12 passed, 0 failed
```

## Subsystem coverage (already in-repo)

Spend caps, prepaid auto-top-up, expiry, arrears accrual/collection, and
reconciliation/orphan-holds each have their own integration tests:
`internal/modules/credits/{spend_policy,money_in,arrears,reconcile,credits_lifecycle}_integration_test.go`.

## Cross-service remainder (other repos — see #249)

The live three-service driver — gen-orchestrator calling `/v1/service/credits/*`
on job submit/complete with Tensorhub-computed pricing (per_output,
per_output_second, per_million_tokens, tiered), plus Stripe-test-mode auto-top-up
driven from a real job — is, per #244's own scoping, owned by the embedding repos
(tensorhub / gen-orchestrator). It is the same wiring tracked by #249's
`[TENSORHUB]` / `[GEN-ORCH]` tasks.
