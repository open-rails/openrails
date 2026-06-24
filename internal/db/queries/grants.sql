-- #514 append-only grant ledger (openrails.grants). derive-1 appends events here;
-- derive-2 folds them into projections (entitlement windows, #512 credit deposits;
-- ownership is read directly off this table).

-- name: InsertGrant :one
INSERT INTO openrails.grants (
    merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id,
    event, supersedes_id, spec_snapshot, starts_at, ends_at, amount, currency, reason
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.narg(product_id)::uuid,
    sqlc.arg(kind)::text, sqlc.arg(source_type)::text, sqlc.arg(source_id)::text, sqlc.narg(payment_id)::uuid,
    sqlc.arg(event)::text, sqlc.narg(supersedes_id)::uuid, sqlc.narg(spec_snapshot)::jsonb,
    sqlc.arg(starts_at)::timestamptz, sqlc.narg(ends_at)::timestamptz,
    sqlc.narg(amount)::bigint, sqlc.narg(currency)::text, sqlc.narg(reason)::text
)
RETURNING *;

-- name: GetGrant :one
SELECT * FROM openrails.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- GetCreditGrantBySourceID: idempotency lookup for a deposit-as-credit-grant by
-- its natural source_id key (the deposit's SourceID).
-- name: GetCreditGrantBySourceID :one
SELECT * FROM openrails.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND kind = 'credit' AND event = 'grant'
  AND source_id = sqlc.arg(source_id)::text
ORDER BY created_at ASC
LIMIT 1;

-- ListLiveGrantsByCustomer: grant-events not terminated by a later revoke/expire/
-- supersede event. The fold's input.
-- name: ListLiveGrantsByCustomer :many
SELECT g.* FROM openrails.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND g.customer_id = sqlc.arg(customer_id)::uuid
  AND g.event = 'grant'
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants t
      WHERE t.supersedes_id = g.id AND t.event IN ('revoke', 'expire', 'supersede')
  )
ORDER BY g.created_at;

-- ListGrantsByCustomer: every grant-event for the customer (live or terminated),
-- the full input to a customer-scoped re-derive.
-- name: ListGrantsByCustomer :many
SELECT * FROM openrails.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND event = 'grant'
ORDER BY created_at;

-- name: IsGrantTerminated :one
SELECT EXISTS (
    SELECT 1 FROM openrails.grants t
    WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid
      AND t.supersedes_id = sqlc.arg(grant_id)::uuid
      AND t.event IN ('revoke', 'expire', 'supersede')
) AS terminated;

-- GrantCreditDeposited: has derive-2 already emitted this credit grant's #512
-- deposit transfer? (idempotency for the credit projection)
-- name: GrantCreditDeposited :one
SELECT EXISTS (
    SELECT 1 FROM openrails.ledger_transfers
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND transfer_type = 'deposit' AND grant_id = sqlc.arg(grant_id)::uuid
) AS deposited;

-- GetCreditLotRemaining: a single credit lot's derived unspent remainder
-- (amount − spent − expired − already-revoked). Deducting `credit_revoke` makes
-- the revoke clawback idempotent: once clawed, remaining is 0 and re-running the
-- projection is a no-op.
-- name: GetCreditLotRemaining :one
SELECT (g.amount - COALESCE((
    SELECT SUM(t.amount) FROM openrails.ledger_transfers t
    WHERE t.merchant_id = g.merchant_id AND t.grant_id = g.id
      AND t.transfer_type IN ('credit_spend', 'credit_expire', 'credit_revoke')
), 0))::bigint AS remaining
FROM openrails.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid AND g.id = sqlc.arg(grant_id)::uuid
  AND g.kind = 'credit' AND g.event = 'grant';

-- SumCreditGrants: cumulative credits granted to a customer in one currency (the
-- "amount paid / trust signal" that graduates the tier). Replaces SumMoneyDeposits.
-- name: SumCreditGrants :one
SELECT COALESCE(SUM(amount), 0)::bigint
FROM openrails.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND kind = 'credit' AND event = 'grant';

-- name: EntitlementExistsForGrant :one
SELECT EXISTS (
    SELECT 1 FROM openrails.entitlements
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND grant_id = sqlc.arg(grant_id)::uuid
      AND entitlement = sqlc.arg(entitlement)::text
      AND deleted_at IS NULL
) AS exists;

-- ListSpendableCreditLots: live (started, unexpired, non-terminated) credit-lot
-- grants with derived remaining = lot amount − Σ(credit_spend + credit_expire
-- transfers tagged to the lot). FIFO order: soonest expiry first.
-- name: ListSpendableCreditLots :many
SELECT g.id, g.amount, g.ends_at,
    (g.amount - COALESCE((
        SELECT SUM(t.amount) FROM openrails.ledger_transfers t
        WHERE t.merchant_id = g.merchant_id AND t.grant_id = g.id
          AND t.transfer_type IN ('credit_spend', 'credit_expire')
    ), 0))::bigint AS remaining
FROM openrails.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND g.customer_id = sqlc.arg(customer_id)::uuid
  AND g.kind = 'credit' AND g.event = 'grant' AND g.currency = sqlc.arg(currency)::text
  AND g.starts_at <= sqlc.arg(as_of)::timestamptz
  AND (g.ends_at IS NULL OR g.ends_at > sqlc.arg(as_of)::timestamptz)
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants tt WHERE tt.supersedes_id = g.id AND tt.event IN ('revoke', 'expire', 'supersede')
  )
ORDER BY g.ends_at ASC NULLS LAST, g.created_at ASC;

-- ListLapsedCreditLots: credit lots past their expiry with an unspent remainder
-- to claw to expired_credits.
-- name: ListLapsedCreditLots :many
SELECT g.id,
    (g.amount - COALESCE((
        SELECT SUM(t.amount) FROM openrails.ledger_transfers t
        WHERE t.merchant_id = g.merchant_id AND t.grant_id = g.id
          AND t.transfer_type IN ('credit_spend', 'credit_expire')
    ), 0))::bigint AS remaining
FROM openrails.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND g.customer_id = sqlc.arg(customer_id)::uuid
  AND g.kind = 'credit' AND g.event = 'grant' AND g.currency = sqlc.arg(currency)::text
  AND g.ends_at IS NOT NULL AND g.ends_at <= sqlc.arg(as_of)::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants tt WHERE tt.supersedes_id = g.id AND tt.event IN ('revoke', 'supersede')
  )
ORDER BY g.ends_at ASC;

-- ListCustomersWithLapsedCreditLots: distinct (merchant, customer, currency) that
-- have at least one past-expiry credit lot with an unspent remainder — the work
-- list for the credit-expiry job's per-customer ExpireLapsed sweep. Bounded batch.
-- name: ListCustomersWithLapsedCreditLots :many
SELECT DISTINCT g.merchant_id, g.customer_id, g.currency
FROM openrails.grants g
WHERE g.kind = 'credit' AND g.event = 'grant'
  AND g.ends_at IS NOT NULL AND g.ends_at <= sqlc.arg(as_of)::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants tt WHERE tt.supersedes_id = g.id AND tt.event IN ('revoke', 'supersede')
  )
  AND (g.amount - COALESCE((
        SELECT SUM(t.amount) FROM openrails.ledger_transfers t
        WHERE t.merchant_id = g.merchant_id AND t.grant_id = g.id
          AND t.transfer_type IN ('credit_spend', 'credit_expire')
    ), 0)) > 0
LIMIT sqlc.arg(batch_size)::int;

-- name: RevokeEntitlementsByGrant :execrows
UPDATE openrails.entitlements
SET revoked_at = sqlc.arg(revoked_at)::timestamptz,
    revoke_reason = sqlc.arg(revoke_reason)::text,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND grant_id = sqlc.arg(grant_id)::uuid
  AND revoked_at IS NULL AND deleted_at IS NULL;

-- GrantHasLiveEntitlement: does this grant still have a non-revoked entitlement
-- window? Used by the Convergence Engine to detect `derive.grant_effect.excess`
-- (a terminated grant whose entitlement effect was never retracted).
-- name: GrantHasLiveEntitlement :one
SELECT EXISTS (
    SELECT 1 FROM openrails.entitlements
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND grant_id = sqlc.arg(grant_id)::uuid
      AND revoked_at IS NULL AND deleted_at IS NULL
) AS exists;

-- #511 DERIVE merchant-wide fan-out: customers in a merchant that hold any grant,
-- so Converge(merchant) can run the per-customer DERIVE checks across all of them.
-- name: ListCustomerIDsWithGrants :many
SELECT DISTINCT customer_id FROM openrails.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY customer_id;

-- #511 DERIVE `derive.grant.missing` (grant tier): a customer's completed,
-- positive, one-off (non-subscription) payments for a product that PROMISES
-- grants — a non-empty `entitlements_spec` or `credits_spec` — yet produced NO
-- grant at all. "Paid for a grantable product, got nothing." Spec-aware (the
-- positive signal is the product's own grant spec via payment→price→product), so
-- empty-spec products / pure fees are never flagged; subscription payments are
-- excluded (their entitlement grant is subscription-sourced, not payment-linked);
-- refund rows are negative-amount (amount > 0 excludes them); a refunded purchase
-- still has its `grant` event (existence, not liveness), so it is not flagged.
-- Surface-only ADMIN: auto-granting re-runs derive-1 (owned by the purchase path).
-- customer_id is nullable (#575): the inline customer-scoped path passes it (fast
-- indexed seek); the merchant-wide convergence sweep passes NULL (one anti-join
-- over the whole merchant instead of one query per grant-holder).
-- name: ListUngrantedGrantablePayments :many
SELECT p.id, p.amount, p.currency
FROM openrails.payments p
JOIN openrails.prices pr ON pr.id = p.price_id AND pr.merchant_id = p.merchant_id
JOIN openrails.products pd ON pd.id = pr.product_id AND pd.merchant_id = p.merchant_id
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR p.customer_id = sqlc.narg(customer_id)::uuid)
  AND p.status = 'completed'
  AND p.amount > 0
  AND p.subscription_id IS NULL
  AND (
        (pd.entitlements_spec IS NOT NULL AND pd.entitlements_spec <> '{}'::jsonb)
     OR (pd.credits_spec IS NOT NULL AND pd.credits_spec <> '{}'::jsonb)
      )
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants g
      WHERE g.merchant_id = p.merchant_id AND g.event = 'grant'
        AND (g.payment_id = p.id OR g.source_id = p.id::text)
  )
ORDER BY p.id;

-- #511 DERIVE `derive.grant.excess`: a customer's LIVE grants whose backing
-- payment was refunded — the source no longer justifies the grant (money came
-- back, access is still live). Surface-only ADMIN: a refund that intentionally
-- keeps access (goodwill) is legitimate, so an operator decides; the source
-- (refund) is a PRESENT recorded fact, so this is NOT confirmed-absence-gated.
-- customer_id is nullable (#575): NULL = merchant-wide sweep.
-- name: ListLiveGrantsWithRefundedPayment :many
SELECT g.id, g.kind, g.payment_id
FROM openrails.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR g.customer_id = sqlc.narg(customer_id)::uuid)
  AND g.event = 'grant'
  AND g.payment_id IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants tt
      WHERE tt.supersedes_id = g.id AND tt.event IN ('revoke', 'expire', 'supersede')
  )
  AND EXISTS (
      SELECT 1 FROM openrails.payments p
      WHERE p.id = g.payment_id AND p.merchant_id = g.merchant_id AND p.status = 'refunded'
  )
ORDER BY g.id;

-- #511/#575 DERIVE `derive.grant_effect.missing` as a single set query: live
-- (un-terminated) entitlement/credit grants whose derived effect is NOT fully
-- materialized. Mirrors the Go isMaterialized exactly — entitlement: some spec
-- feature has no entitlement row (deleted_at IS NULL, revoked rows still count as
-- materialized); credit: no #512 deposit transfer. ownership has no effect (never
-- flagged). customer_id nullable: NULL = merchant-wide sweep. Repair =
-- MaterializeGrant (idempotent), so re-running converges to empty.
-- name: ListLiveGrantsMissingEffects :many
SELECT g.* FROM openrails.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR g.customer_id = sqlc.narg(customer_id)::uuid)
  AND g.event = 'grant'
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants t
      WHERE t.supersedes_id = g.id AND t.event IN ('revoke', 'expire', 'supersede')
  )
  AND (
    (g.kind = 'entitlement' AND EXISTS (
        SELECT 1 FROM jsonb_array_elements_text(
                 COALESCE(g.spec_snapshot->'entitlements', '[]'::jsonb)) AS feat
        WHERE NOT EXISTS (
            SELECT 1 FROM openrails.entitlements e
            WHERE e.merchant_id = g.merchant_id AND e.grant_id = g.id
              AND e.entitlement = feat AND e.deleted_at IS NULL)))
    OR
    (g.kind = 'credit' AND NOT EXISTS (
        SELECT 1 FROM openrails.ledger_transfers lt
        WHERE lt.merchant_id = g.merchant_id AND lt.transfer_type = 'deposit' AND lt.grant_id = g.id))
  )
ORDER BY g.created_at;

-- #511/#575 DERIVE `derive.grant_effect.excess` as a single set query: TERMINATED
-- grants whose derived effect is still live (a revoke/expire recorded but its
-- retraction never propagated). Mirrors Go IsGrantTerminated + effectStillLive —
-- entitlement: a non-revoked, non-deleted entitlement row; credit: lot remainder
-- (amount − spend/expire/revoke transfers) > 0. customer_id nullable: NULL =
-- merchant-wide sweep. Repair = MaterializeGrant (retracts) — idempotent.
-- name: ListUnretractedTerminations :many
SELECT g.* FROM openrails.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR g.customer_id = sqlc.narg(customer_id)::uuid)
  AND g.event = 'grant'
  AND EXISTS (
      SELECT 1 FROM openrails.grants t
      WHERE t.merchant_id = g.merchant_id AND t.supersedes_id = g.id
        AND t.event IN ('revoke', 'expire', 'supersede')
  )
  AND (
    (g.kind = 'entitlement' AND EXISTS (
        SELECT 1 FROM openrails.entitlements e
        WHERE e.merchant_id = g.merchant_id AND e.grant_id = g.id
          AND e.revoked_at IS NULL AND e.deleted_at IS NULL))
    OR
    (g.kind = 'credit' AND (
        g.amount - COALESCE((
            SELECT SUM(t.amount) FROM openrails.ledger_transfers t
            WHERE t.merchant_id = g.merchant_id AND t.grant_id = g.id
              AND t.transfer_type IN ('credit_spend', 'credit_expire', 'credit_revoke')
        ), 0)) > 0)
  )
ORDER BY g.created_at;

-- #511 ownership-on-grants: live (un-terminated) ownership grant ids backing a
-- payment, so a refund/chargeback can revoke product access for that payment.
-- name: ListLiveOwnershipGrantIDsByPayment :many
SELECT g.id FROM openrails.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND g.payment_id = sqlc.arg(payment_id)::uuid
  AND g.kind = 'ownership'
  AND g.event = 'grant'
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants t
      WHERE t.supersedes_id = g.id AND t.event IN ('revoke', 'expire', 'supersede')
  );

-- #511 ownership-on-grants: every ownership grant-event for a customer with its
-- derived status (the termination event, if any) — so the legacy
-- ProductAccessGrant.{status,revoked_at,revoke_reason} shape can be reconstructed
-- from the append-only ledger in one query.
-- name: ListOwnershipGrantsWithStatus :many
SELECT g.id, g.merchant_id, g.customer_id, g.product_id, g.source_type, g.source_id,
       g.payment_id, g.starts_at, g.ends_at, g.created_at,
       term.created_at AS revoked_at, term.reason AS revoke_reason
FROM openrails.grants g
LEFT JOIN openrails.grants term
  ON term.supersedes_id = g.id AND term.event IN ('revoke', 'expire', 'supersede')
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND g.customer_id = sqlc.arg(customer_id)::uuid
  AND g.kind = 'ownership'
  AND g.event = 'grant'
ORDER BY g.created_at;
