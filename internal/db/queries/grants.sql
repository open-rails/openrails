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
