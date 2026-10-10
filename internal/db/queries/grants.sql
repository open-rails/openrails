-- The append-only grant ledger (billing.grants). derive-1 appends events here;
-- derive-2 folds them into projections (product_access windows, credit
-- deposits). entitlement and ownership events are history.

-- name: InsertGrant :one
INSERT INTO billing.grants (
    merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id,
    event, supersedes_id, spec_snapshot, starts_at, ends_at, amount, currency, reason, actor, grant_reason, quantity
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.narg(product_id)::uuid,
    sqlc.arg(kind)::text, sqlc.arg(source_type)::text, NULLIF(sqlc.arg(source_id)::text, ''), sqlc.narg(payment_id)::uuid,
    sqlc.arg(event)::text, sqlc.narg(supersedes_id)::uuid, sqlc.narg(spec_snapshot)::jsonb,
    sqlc.arg(starts_at)::timestamptz, sqlc.narg(ends_at)::timestamptz,
    sqlc.narg(amount)::bigint, sqlc.narg(currency)::text, sqlc.narg(reason)::text,
    sqlc.narg(actor)::text, sqlc.narg(grant_reason)::text, sqlc.narg(quantity)::int
)
RETURNING *;

-- name: InsertAccessGrantOnce :one
-- An access grant at its natural key: a replay returns no row and the caller
-- reads the recorded grant.
INSERT INTO billing.grants (
    merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id,
    event, starts_at, ends_at, reason, actor, grant_reason, quantity
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(product_id)::uuid,
    'access', sqlc.arg(source_type)::text, sqlc.arg(source_id)::text, sqlc.narg(payment_id)::uuid,
    'grant', sqlc.arg(starts_at)::timestamptz, sqlc.narg(ends_at)::timestamptz,
    sqlc.narg(reason)::text, sqlc.narg(actor)::text, sqlc.narg(grant_reason)::text, sqlc.narg(quantity)::int
)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetAccessGrantByIdempotencyKey :one
SELECT * FROM billing.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND kind = 'access' AND event = 'grant'
  AND source_type = 'grant' AND source_id = sqlc.arg(idempotency_key)::text AND grant_reason <> 'migration';

-- name: GetAccessGrantByPurchase :one
SELECT * FROM billing.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND kind = 'access' AND event = 'grant'
  AND source_type = 'purchase' AND payment_id = sqlc.arg(payment_id)::uuid AND product_id = sqlc.arg(product_id)::uuid;

-- name: ListLiveGrantsBySource :many
-- The live grants of one source: a subscription's periods and grace, a
-- purchase. Indexed by source; never the customer's whole history.
SELECT g.* FROM billing.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid AND g.customer_id = sqlc.arg(customer_id)::uuid
  AND g.kind = sqlc.arg(kind)::text AND g.event = 'grant'
  AND g.source_type = ANY(sqlc.arg(source_types)::text[]) AND g.source_id = sqlc.arg(source_id)::text
  AND NOT EXISTS (SELECT 1 FROM billing.grants t
      WHERE t.merchant_id = g.merchant_id AND t.supersedes_id = g.id AND t.event IN ('revoke', 'expire', 'supersede'))
ORDER BY g.id;

-- name: GetGrant :one
SELECT * FROM billing.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- GetCreditGrantBySourceID: idempotency lookup for a deposit-as-credit-grant by
-- its natural source_id key (the deposit's SourceID).
-- name: GetCreditGrantBySourceID :one
SELECT * FROM billing.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND kind = 'credit' AND event = 'grant'
  AND source_id = sqlc.arg(source_id)::text
ORDER BY created_at ASC
LIMIT 1;

-- ListLiveGrantsByCustomer: grant-events not terminated by a later revoke/expire/
-- supersede event. The fold's input.
-- name: ListLiveGrantsByCustomer :many
SELECT g.* FROM billing.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND g.customer_id = sqlc.arg(customer_id)::uuid
  AND g.event = 'grant'
  AND NOT EXISTS (
      SELECT 1 FROM billing.grants t
      WHERE t.supersedes_id = g.id AND t.event IN ('revoke', 'expire', 'supersede')
  )
ORDER BY g.created_at;

-- ListGrantsByCustomer: every grant-event for the customer (live or terminated),
-- the full input to a customer-scoped re-derive.
-- name: ListGrantsByCustomer :many
SELECT * FROM billing.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND event = 'grant'
ORDER BY created_at;

-- name: IsGrantTerminated :one
SELECT EXISTS (
    SELECT 1 FROM billing.grants t
    WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid
      AND t.supersedes_id = sqlc.arg(grant_id)::uuid
      AND t.event IN ('revoke', 'expire', 'supersede')
) AS terminated;

-- GrantCreditDeposited: whether derive-2 already posted this credit grant's
-- deposit transfer (the credit projection's idempotency).
-- name: GrantCreditDeposited :one
SELECT EXISTS (
    SELECT 1 FROM billing.ledger_transfers
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND transfer_type = 'deposit' AND grant_id = sqlc.arg(grant_id)::uuid
) AS deposited;

-- GetCreditLotRemaining: a credit lot's unspent remainder, net of every transfer
-- drawn from it. Deducting credit_revoke makes the revoke clawback idempotent.
-- name: GetCreditLotRemaining :one
SELECT (g.amount - COALESCE((
    SELECT SUM(CASE WHEN t.transfer_type = 'credit_refund_restore' THEN -t.amount ELSE t.amount END) FROM billing.ledger_transfers t
    WHERE t.merchant_id = g.merchant_id AND t.grant_id = g.id
      AND t.transfer_type IN ('credit_spend', 'owed_repayment', 'credit_expire', 'credit_revoke', 'credit_refund', 'credit_refund_restore')
), 0))::bigint AS remaining
FROM billing.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid AND g.id = sqlc.arg(grant_id)::uuid
  AND g.kind = 'credit' AND g.event = 'grant';

-- ListSpendableCreditLots: live (started, unexpired, non-terminated) credit lots
-- with their remainder, less the share a pending refund reserves. FIFO:
-- soonest expiry first.
-- name: ListSpendableCreditLots :many
SELECT g.id, g.amount, g.ends_at,
    (g.amount - COALESCE((
        SELECT SUM(CASE WHEN t.transfer_type = 'credit_refund_restore' THEN -t.amount ELSE t.amount END) FROM billing.ledger_transfers t
        WHERE t.merchant_id = g.merchant_id AND t.grant_id = g.id
          AND t.transfer_type IN ('credit_spend', 'owed_repayment', 'credit_expire', 'credit_refund', 'credit_refund_restore')
    ), 0) - COALESCE((
        SELECT ceil(g.amount::numeric * sum(-refund.amount) / payment.amount)
        FROM billing.payments refund JOIN billing.payments payment
          ON payment.merchant_id=g.merchant_id AND payment.id=g.payment_id AND payment.deleted_at IS NULL
        WHERE refund.merchant_id=g.merchant_id AND refund.customer_id=g.customer_id
          AND refund.currency=g.currency AND refund.refunded_payment_id=g.payment_id
          AND refund.status='pending' AND refund.amount<0 AND refund.deleted_at IS NULL
          AND g.spec_snapshot->'deposit'->'paid_amount' IS NOT NULL
        GROUP BY payment.amount
    ), 0))::bigint AS remaining
FROM billing.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND g.customer_id = sqlc.arg(customer_id)::uuid
  AND g.kind = 'credit' AND g.event = 'grant' AND g.currency = sqlc.arg(currency)::text
  AND g.starts_at <= sqlc.arg(as_of)::timestamptz
  AND (g.ends_at IS NULL OR g.ends_at > sqlc.arg(as_of)::timestamptz)
  AND NOT EXISTS (
      SELECT 1 FROM billing.grants tt WHERE tt.supersedes_id = g.id AND tt.event IN ('revoke', 'expire', 'supersede')
  )
ORDER BY g.ends_at ASC NULLS LAST, g.created_at ASC;

-- ListLapsedCreditLots: credit lots past their expiry with an unspent remainder
-- to claw to expired_credits.
-- name: ListLapsedCreditLots :many
SELECT g.id,
    (g.amount - COALESCE((
        SELECT SUM(CASE WHEN t.transfer_type = 'credit_refund_restore' THEN -t.amount ELSE t.amount END) FROM billing.ledger_transfers t
        WHERE t.merchant_id = g.merchant_id AND t.grant_id = g.id
          AND t.transfer_type IN ('credit_spend', 'owed_repayment', 'credit_expire', 'credit_refund', 'credit_refund_restore')
    ), 0))::bigint AS remaining
FROM billing.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND g.customer_id = sqlc.arg(customer_id)::uuid
  AND g.kind = 'credit' AND g.event = 'grant' AND g.currency = sqlc.arg(currency)::text
  AND g.ends_at IS NOT NULL AND g.ends_at <= sqlc.arg(as_of)::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM billing.grants tt WHERE tt.supersedes_id = g.id AND tt.event IN ('revoke', 'supersede')
  )
ORDER BY g.ends_at ASC;

-- ListCustomersWithLapsedCreditLots: distinct (merchant, customer, currency) that
-- have at least one past-expiry credit lot with an unspent remainder — the work
-- list for the credit-expiry job's per-customer ExpireLapsed sweep. Bounded batch.
-- name: ListCustomersWithLapsedCreditLots :many
SELECT DISTINCT g.merchant_id, g.customer_id, g.currency
FROM billing.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid AND g.kind = 'credit' AND g.event = 'grant'
  AND g.ends_at IS NOT NULL AND g.ends_at <= sqlc.arg(as_of)::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM billing.grants tt WHERE tt.merchant_id = sqlc.arg(merchant_id)::uuid AND tt.supersedes_id = g.id AND tt.event IN ('revoke', 'supersede')
  )
  AND (g.amount - COALESCE((
        SELECT SUM(CASE WHEN t.transfer_type = 'credit_refund_restore' THEN -t.amount ELSE t.amount END) FROM billing.ledger_transfers t
        WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid AND t.merchant_id = g.merchant_id AND t.grant_id = g.id
          AND t.transfer_type IN ('credit_spend', 'owed_repayment', 'credit_expire', 'credit_refund', 'credit_refund_restore')
    ), 0)) > 0
LIMIT sqlc.arg(batch_size)::int;

-- derive.grant.missing: succeeded, positive, non-subscription payments that
-- produced no grant (every purchase grants its product or a credit lot). A
-- refunded purchase keeps its grant event, so it is not flagged. Surface-only:
-- re-granting re-runs derive-1. NULL customer_id = merchant-wide.
-- name: ListUngrantedGrantablePayments :many
SELECT p.id, p.amount, p.currency
FROM billing.payments p
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR p.customer_id = sqlc.narg(customer_id)::uuid)
  AND p.deleted_at IS NULL
  AND p.status = 'succeeded'
  AND p.amount > 0
  AND p.subscription_id IS NULL
  -- An order's lines name what they produced; an invoice's payment grants nothing.
  AND p.order_id IS NULL AND p.invoice_id IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM billing.grants g
      WHERE g.merchant_id = p.merchant_id AND g.event = 'grant'
        AND (g.payment_id = p.id OR g.source_id = p.id::text)
  )
ORDER BY p.id;

-- derive.grant.excess: live grants whose payment was refunded. Surface-only: a
-- goodwill refund may keep access, so an operator decides. NULL customer_id =
-- merchant-wide.
-- name: ListLiveGrantsWithRefundedPayment :many
SELECT g.id, g.kind, g.payment_id
FROM billing.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR g.customer_id = sqlc.narg(customer_id)::uuid)
  AND g.event = 'grant'
  AND g.payment_id IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM billing.grants tt
      WHERE tt.supersedes_id = g.id AND tt.event IN ('revoke', 'expire', 'supersede')
  )
  AND EXISTS (
      SELECT 1 FROM billing.payments p
      WHERE p.id = g.payment_id AND p.merchant_id = g.merchant_id AND p.deleted_at IS NULL AND p.status = 'refunded'
  )
ORDER BY g.id;

-- derive.grant_effect.missing: live access/credit grants without their effect:
-- access has no window (revoked windows count), credit has no deposit transfer.
-- NULL customer_id = merchant-wide. Repair = MaterializeGrant.
-- name: ListLiveGrantsMissingEffects :many
SELECT g.* FROM billing.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR g.customer_id = sqlc.narg(customer_id)::uuid)
  AND g.event = 'grant'
  AND NOT EXISTS (
      SELECT 1 FROM billing.grants t
      WHERE t.supersedes_id = g.id AND t.event IN ('revoke', 'expire', 'supersede')
  )
  AND (
    (g.kind = 'access' AND NOT EXISTS (
        SELECT 1 FROM billing.product_access pa
        WHERE pa.merchant_id = g.merchant_id AND pa.grant_id = g.id AND pa.deleted_at IS NULL))
    OR
    (g.kind = 'credit' AND NOT EXISTS (
        SELECT 1 FROM billing.ledger_transfers lt
        WHERE lt.merchant_id = g.merchant_id AND lt.transfer_type = 'deposit' AND lt.grant_id = g.id))
  )
ORDER BY g.created_at;

-- derive.grant_effect.excess: terminated grants whose effect is still live (an
-- unrevoked window, or a credit remainder > 0). NULL customer_id =
-- merchant-wide. Repair = MaterializeGrant, idempotent.
-- name: ListUnretractedTerminations :many
SELECT g.* FROM billing.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR g.customer_id = sqlc.narg(customer_id)::uuid)
  AND g.event = 'grant'
  AND EXISTS (
      SELECT 1 FROM billing.grants t
      WHERE t.merchant_id = g.merchant_id AND t.supersedes_id = g.id
        AND t.event IN ('revoke', 'expire', 'supersede')
  )
  AND (
    (g.kind = 'access' AND EXISTS (
        SELECT 1 FROM billing.product_access pa
        WHERE pa.merchant_id = g.merchant_id AND pa.grant_id = g.id
          AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL))
    OR
    (g.kind = 'credit' AND (
        g.amount - COALESCE((
            SELECT SUM(CASE WHEN t.transfer_type = 'credit_refund_restore' THEN -t.amount ELSE t.amount END) FROM billing.ledger_transfers t
            WHERE t.merchant_id = g.merchant_id AND t.grant_id = g.id
              AND t.transfer_type IN ('credit_spend', 'owed_repayment', 'credit_expire', 'credit_revoke', 'credit_refund', 'credit_refund_restore')
        ), 0)) > 0)
  )
ORDER BY g.created_at;

-- derive.subscription.missing: subscriptions in an access-granting state with no
-- subscription-sourced grant; derive-1 materializes the grant and its window
-- (computed Go-side). A chargeback cancel grants no runway. scan_since skips
-- windows that ended long ago. NULL customer_id = merchant-wide.
-- name: ListUngrantedSubscriptions :many
SELECT s.id, s.customer_id, s.product_id, s.status,
       s.current_period_starts_at,
       -- A provider-billed member in the provider's dunning keeps access
       -- through its grace window, as a mirrored decline does.
       GREATEST(s.current_period_ends_at, CASE WHEN s.status = 'past_due' THEN s.grace_ends_at END) AS current_period_ends_at,
       s.started_at, s.ended_at, s.access_duration_hours_snapshot
FROM billing.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR s.customer_id = sqlc.narg(customer_id)::uuid)
  AND s.deleted_at IS NULL
  -- Engine card access is authored only by its atomic accepted-payment writer.
  AND NOT (s.collection_policy='engine' AND s.rail IN ('nmi','stripe'))
  AND (s.status IN ('active', 'canceled', 'unverified', 'awaiting_method') OR (s.status = 'past_due' AND s.collection_policy <> 'engine'))
  AND NOT (s.status = 'canceled' AND s.cancel_type = 'chargeback')
  AND (s.access_duration_hours_snapshot IS NULL OR
       COALESCE(s.current_period_starts_at, s.started_at) + s.access_duration_hours_snapshot * interval '1 hour' >= sqlc.arg(scan_since)::timestamptz)
  AND NOT EXISTS (
      SELECT 1 FROM billing.grants g
      WHERE g.merchant_id = s.merchant_id AND g.event = 'grant'
        AND g.source_type = 'subscription' AND g.source_id = s.id::text
  )
ORDER BY COALESCE(s.current_period_starts_at, s.started_at);

-- derive.wallet.missing: succeeded Solana wallet payments with a stored access
-- window (metadata.expiration_rfc3339) and no grant. The stored expiry makes the
-- window [purchased_at, expiration) unambiguous, so unlike derive.grant.missing
-- this auto-repairs.
-- name: ListUngrantedWalletPayments :many
SELECT p.id, p.customer_id, p.purchased_at,
       (p.metadata->>'expiration_rfc3339')::timestamptz AS expires_at, pr.product_id
FROM billing.payments p
JOIN billing.prices pr ON pr.id = p.price_id AND pr.merchant_id = p.merchant_id
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR p.customer_id = sqlc.narg(customer_id)::uuid)
  AND p.deleted_at IS NULL
  AND p.rail = 'solana'
  AND p.status = 'succeeded'
  AND p.amount > 0
  AND p.subscription_id IS NULL
  AND p.metadata->>'expiration_rfc3339' IS NOT NULL
  AND p.metadata->>'expiration_rfc3339' ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}'
  AND (p.metadata->>'expiration_rfc3339')::timestamptz > p.purchased_at
  AND (p.metadata->>'expiration_rfc3339')::timestamptz >= sqlc.arg(scan_since)::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM billing.grants g
      WHERE g.merchant_id = p.merchant_id AND g.event = 'grant'
        AND ((g.source_type = 'purchase' AND g.source_id = p.id::text) OR g.payment_id = p.id)
  )
ORDER BY p.purchased_at;

-- An access window is a distinct fact even when it overlaps another paid
-- window or has no expiry. A replay must not reinstate a revoked grant.
-- name: AccessGrantWindowExists :one
SELECT EXISTS (
    SELECT 1 FROM billing.grants g
    WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
      AND g.customer_id = sqlc.arg(customer_id)::uuid
      AND g.product_id = sqlc.arg(product_id)::uuid
      AND g.kind = 'access' AND g.event = 'grant'
      AND g.source_type = sqlc.arg(source_type)::text
      AND g.source_id = sqlc.arg(source_id)::text
      AND g.starts_at = sqlc.arg(starts_at)::timestamptz
      AND g.ends_at IS NOT DISTINCT FROM sqlc.narg(ends_at)::timestamptz
) AS exists;

-- CROSS-MERCHANT: merchants holding a past-expiry credit lot that was not
-- revoked or superseded. Ids only; the per-customer work list and the ledger
-- transfers run per merchant.
-- name: ListLapsedCreditLotMerchants :many
SELECT DISTINCT g.merchant_id
FROM billing.grants g
WHERE g.kind = 'credit' AND g.event = 'grant'
  AND g.ends_at IS NOT NULL AND g.ends_at <= sqlc.arg(as_of)::timestamptz
  AND NOT EXISTS (
        SELECT 1 FROM billing.grants tt
         WHERE tt.merchant_id = g.merchant_id AND tt.supersedes_id = g.id
           AND tt.event IN ('revoke', 'supersede'))
LIMIT sqlc.arg(merchant_limit)::int;

-- name: ListOriginalPurchaseGrants :many
-- Original immutable events, including later-revoked sources. Accepted purchase
-- replay validates the original windows without reopening revoked projections.
SELECT * FROM billing.grants
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND source_type='purchase'
  AND source_id=sqlc.arg(payment_id)::uuid::text AND event='grant'
ORDER BY id LIMIT sqlc.arg(row_limit)::int;

-- name: ListInitialMembershipGrants :many
-- Original source events before the accepted initial period ends; later renewal
-- events and later revocations do not rewrite this initial history.
SELECT * FROM billing.grants
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND source_type='subscription'
  AND source_id=sqlc.arg(subscription_id)::uuid::text AND event='grant'
  AND starts_at < sqlc.arg(before)::timestamptz
ORDER BY id LIMIT sqlc.arg(row_limit)::int;

-- name: HasInitialMembershipGrant :one
-- Refused or still-pending initial membership cannot own a grant at any instant.
SELECT EXISTS(SELECT 1 FROM billing.grants
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND source_type='subscription'
  AND source_id=sqlc.arg(subscription_id)::uuid::text AND event='grant')::boolean;

-- name: ListRenewalGrantsForArchive :many
SELECT * FROM billing.grants
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND source_type='subscription'
  AND source_id=sqlc.arg(subscription_id)::uuid::text AND event='grant'
  AND starts_at=sqlc.arg(period_start)::timestamptz
ORDER BY id LIMIT sqlc.arg(row_limit)::int;

-- name: CountUnpaidEngineRenewalGrants :one
-- Initial and pre-engine history precedes the first accepted engine period.
-- Every later source grant needs its own successful accepted period, including
-- grants following a declined attempt whose later retry bought the same window.
SELECT count(*) FROM billing.grants g
WHERE g.merchant_id=sqlc.arg(merchant_id)::uuid AND g.source_type='subscription'
  AND g.event='grant'
  AND EXISTS (SELECT 1 FROM billing.provider_intents i
    WHERE i.merchant_id=g.merchant_id AND i.intent_type='subscription_collection'
      AND i.subscription_id::text=g.source_id
      AND g.starts_at >= (i.payload->'renewal'->>'period_start')::timestamptz)
  AND NOT EXISTS (SELECT 1 FROM billing.provider_intents i
    WHERE i.merchant_id=g.merchant_id AND i.intent_type='subscription_collection'
      AND i.subscription_id::text=g.source_id AND i.status='succeeded'
      AND g.starts_at=(i.payload->'renewal'->>'period_start')::timestamptz
      AND g.ends_at IS NOT DISTINCT FROM CASE
          WHEN i.payload->'renewal' ? 'access_duration_hours' THEN
            (i.payload->'renewal'->>'period_start')::timestamptz +
            (i.payload->'renewal'->>'access_duration_hours')::int * interval '1 hour'
          ELSE (i.payload->'renewal'->>'period_end')::timestamptz END);


-- name: ListAccessGrantsAt :many
-- The access grant of one source period, read back after a concurrent replay.
SELECT * FROM billing.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND product_id = sqlc.arg(product_id)::uuid AND kind = 'access' AND event = 'grant'
  AND source_type = sqlc.arg(source_type)::text AND source_id = sqlc.arg(source_id)::text
  AND starts_at = sqlc.arg(starts_at)::timestamptz
ORDER BY id;
