-- Credit lots (billing.credit_grants) and their revocations. A lot's
-- remaining units are units_credited - units_debited, kept by the ledger.

-- name: InsertCreditGrant :one
INSERT INTO billing.credit_grants (
    merchant_id, customer_id, account_id, unit, units, funding_source, paid_amount, paid_currency,
    source, source_id, invoker, product_id, order_line_id, payment_id, description, starts_at, expires_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(account_id)::uuid, sqlc.arg(unit)::text,
    sqlc.arg(units)::bigint, sqlc.arg(funding_source)::text, sqlc.narg(paid_amount)::bigint, sqlc.narg(paid_currency)::text,
    sqlc.arg(source)::text, sqlc.narg(source_id)::text, sqlc.arg(invoker)::text, sqlc.narg(product_id)::uuid,
    sqlc.narg(order_line_id)::uuid, sqlc.narg(payment_id)::uuid, sqlc.narg(description)::text,
    sqlc.arg(starts_at)::timestamptz, sqlc.narg(expires_at)::timestamptz
)
RETURNING *;

-- name: GetCreditLot :one
SELECT * FROM billing.credit_grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: GetCreditGrantBySourceID :one
-- A funding's once-only key: the lot already funded at it.
SELECT * FROM billing.credit_grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND source_id = sqlc.arg(source_id)::text;

-- name: GetPurchasedCreditGrant :one
SELECT * FROM billing.credit_grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND payment_id = sqlc.arg(payment_id)::uuid
  AND paid_amount IS NOT NULL;

-- name: InsertCreditGrantEvent :one
-- Its trigger sets or clears the lot's revoked_at, and refuses a revoke of a
-- revoked lot or a reinstatement of a live one.
INSERT INTO billing.credit_grant_events (merchant_id, credit_grant_id, event, effective_at, reason, actor)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(credit_grant_id)::uuid, sqlc.arg(event)::text,
    sqlc.arg(effective_at)::timestamptz, sqlc.narg(reason)::text, sqlc.narg(actor)::text)
RETURNING *;

-- name: ListSpendableCreditLots :many
-- Live lots (started, not lapsed, not revoked) with units left, soonest
-- expiry first. available is what a pending refund does not reserve. A
-- running charge's posting through an instant draws the lots that had not
-- lapsed just before it (live_at).
SELECT id, units, expires_at, (units_credited - units_debited)::bigint AS remaining,
       GREATEST(units_credited - units_debited - units_reserved, 0)::bigint AS available
FROM billing.credit_grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND unit = sqlc.arg(currency)::text AND revoked_at IS NULL AND units_credited > units_debited
  AND starts_at <= sqlc.arg(as_of)::timestamptz
  AND (expires_at IS NULL OR expires_at > COALESCE(sqlc.narg(live_at)::timestamptz, sqlc.arg(as_of)::timestamptz))
ORDER BY expires_at ASC NULLS LAST, created_at ASC, id ASC;

-- name: ListLapsedCreditLots :many
-- Lots past their expiry with units left, to expire.
SELECT id, expires_at::timestamptz AS expires_at, (units_credited - units_debited)::bigint AS remaining
FROM billing.credit_grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND unit = sqlc.arg(currency)::text AND revoked_at IS NULL AND units_credited > units_debited
  AND expires_at IS NOT NULL AND expires_at <= sqlc.arg(as_of)::timestamptz
ORDER BY expires_at ASC, id ASC;

-- name: SyncCreditGrantReservation :exec
-- Written whenever a refund of the lot's payment is reserved, released or
-- settled (billing.credit_grant_reservation).
UPDATE billing.credit_grants g
SET units_reserved = billing.credit_grant_reservation(g.merchant_id, g.id)
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid AND g.payment_id = sqlc.arg(payment_id)::uuid
  AND g.paid_amount IS NOT NULL;

-- name: ListCreditReservationDrifts :many
-- Purchased lots whose reservation disagrees with their payment's pending
-- refunds. NULL customer_id = merchant-wide. Repair = SyncCreditGrantReservation.
SELECT * FROM billing.credit_grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR g.customer_id = sqlc.narg(customer_id)::uuid)
  AND g.payment_id IS NOT NULL AND g.paid_amount IS NOT NULL
  AND g.units_reserved <> billing.credit_grant_reservation(g.merchant_id, g.id)
ORDER BY g.created_at, g.id
LIMIT sqlc.arg(row_limit)::int;

-- name: ListCreditLotsMissingDeposit :many
-- derive.grant_effect.missing for lots: no unit of the lot ever posted.
-- NULL customer_id = merchant-wide. Repair = credits.Lots.Materialize.
SELECT * FROM billing.credit_grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND units_credited = 0 AND revoked_at IS NULL
ORDER BY created_at, id
LIMIT sqlc.arg(row_limit)::int;

-- name: ListRevokedCreditLotsWithUnits :many
-- derive.grant_effect.excess for lots: revoked with units still on the
-- balance. NULL customer_id = merchant-wide. Repair = the clawback.
SELECT * FROM billing.credit_grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND revoked_at IS NOT NULL AND units_credited > units_debited
ORDER BY created_at, id
LIMIT sqlc.arg(row_limit)::int;

-- name: ListCreditLotDrifts :many
-- Lots whose counters disagree with the legs on their balance account, or
-- whose reservation with their payment's pending refunds.
WITH logged AS (
    SELECT t.credit_grant_id AS id,
           COALESCE(sum(t.amount) FILTER (WHERE t.credit_account_id = g.account_id), 0)::bigint AS credited,
           COALESCE(sum(t.amount) FILTER (WHERE t.debit_account_id = g.account_id), 0)::bigint AS debited
    FROM billing.ledger_transfers t
    JOIN billing.credit_grants g ON g.merchant_id = t.merchant_id AND g.id = t.credit_grant_id
    WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid AND t.credit_grant_id IS NOT NULL
    GROUP BY t.credit_grant_id
)
SELECT g.id AS credit_grant_id, g.merchant_id, g.customer_id, g.unit,
       g.units_credited AS stored_credited, COALESCE(l.credited, 0)::bigint AS logged_credited,
       g.units_debited AS stored_debited, COALESCE(l.debited, 0)::bigint AS logged_debited,
       g.units_reserved AS stored_reserved, COALESCE(r.reserved, 0)::bigint AS expected_reserved
FROM billing.credit_grants g
LEFT JOIN logged l ON l.id = g.id
CROSS JOIN LATERAL (SELECT billing.credit_grant_reservation(g.merchant_id, g.id) AS reserved) r
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (g.units_credited <> COALESCE(l.credited, 0) OR g.units_debited <> COALESCE(l.debited, 0)
       OR g.units_reserved <> r.reserved)
ORDER BY g.unit, g.id;

-- Customer support reads: the lot, its latest revocation, and what its legs
-- on its balance account did. refunded is net of restores, revoked of
-- reinstatements.

-- name: ListCustomerCreditGrants :many
WITH page AS (
  SELECT g.* FROM billing.credit_grants g
  WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
    AND g.customer_id = sqlc.arg(customer_id)::uuid
    AND (sqlc.narg(currency)::text IS NULL OR g.unit = sqlc.narg(currency)::text)
    AND (sqlc.narg(source_id)::text IS NULL OR g.source_id = sqlc.narg(source_id)::text)
    AND (sqlc.narg(after_at)::timestamptz IS NULL
     OR (g.created_at, g.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
  ORDER BY g.created_at DESC, g.id DESC
  LIMIT sqlc.arg(row_limit)::int
)
SELECT g.id, g.customer_id, g.unit, g.units, g.funding_source, g.paid_amount, g.paid_currency,
       g.source_id, g.description, g.starts_at, g.expires_at, g.revoked_at, g.created_at,
       g.units_credited AS credited, (g.units_credited - g.units_debited)::bigint AS remaining,
       revocation.reason AS revoke_reason,
       COALESCE(t.spent, 0)::bigint AS spent, COALESCE(t.expired, 0)::bigint AS expired,
       COALESCE(t.revoked, 0)::bigint AS revoked, COALESCE(t.refunded, 0)::bigint AS refunded
FROM page g
LEFT JOIN LATERAL (
  SELECT e.reason FROM billing.credit_grant_events e
  WHERE e.merchant_id = g.merchant_id AND e.credit_grant_id = g.id AND e.event = 'revoke' AND g.revoked_at IS NOT NULL
  ORDER BY e.created_at DESC, e.id DESC LIMIT 1
) revocation ON true
LEFT JOIN LATERAL (
  SELECT sum(lt.amount) FILTER (WHERE lt.transfer_type IN ('credit_spend', 'owed_repayment')) AS spent,
         sum(lt.amount) FILTER (WHERE lt.transfer_type = 'credit_expire') AS expired,
         sum(CASE WHEN lt.debit_account_id = g.account_id THEN lt.amount ELSE -lt.amount END)
             FILTER (WHERE lt.transfer_type IN ('credit_revoke', 'credit_reinstate')) AS revoked,
         sum(CASE WHEN lt.debit_account_id = g.account_id THEN lt.amount ELSE -lt.amount END)
             FILTER (WHERE lt.transfer_type IN ('credit_refund', 'credit_refund_restore')) AS refunded
  FROM billing.ledger_transfers lt
  WHERE lt.merchant_id = g.merchant_id AND lt.credit_grant_id = g.id
    AND g.account_id IN (lt.debit_account_id, lt.credit_account_id)
) t ON true
ORDER BY g.created_at DESC, g.id DESC;

-- name: ListCreditGrantsByIDs :many
-- The merchant's named credit grants, newest first.
SELECT g.id, g.customer_id, g.unit, g.units, g.funding_source, g.paid_amount, g.paid_currency,
       g.source_id, g.description, g.starts_at, g.expires_at, g.revoked_at, g.created_at,
       g.units_credited AS credited, (g.units_credited - g.units_debited)::bigint AS remaining,
       revocation.reason AS revoke_reason,
       COALESCE(t.spent, 0)::bigint AS spent, COALESCE(t.expired, 0)::bigint AS expired,
       COALESCE(t.revoked, 0)::bigint AS revoked, COALESCE(t.refunded, 0)::bigint AS refunded
FROM billing.credit_grants g
LEFT JOIN LATERAL (
  SELECT e.reason FROM billing.credit_grant_events e
  WHERE e.merchant_id = g.merchant_id AND e.credit_grant_id = g.id AND e.event = 'revoke' AND g.revoked_at IS NOT NULL
  ORDER BY e.created_at DESC, e.id DESC LIMIT 1
) revocation ON true
LEFT JOIN LATERAL (
  SELECT sum(lt.amount) FILTER (WHERE lt.transfer_type IN ('credit_spend', 'owed_repayment')) AS spent,
         sum(lt.amount) FILTER (WHERE lt.transfer_type = 'credit_expire') AS expired,
         sum(CASE WHEN lt.debit_account_id = g.account_id THEN lt.amount ELSE -lt.amount END)
             FILTER (WHERE lt.transfer_type IN ('credit_revoke', 'credit_reinstate')) AS revoked,
         sum(CASE WHEN lt.debit_account_id = g.account_id THEN lt.amount ELSE -lt.amount END)
             FILTER (WHERE lt.transfer_type IN ('credit_refund', 'credit_refund_restore')) AS refunded
  FROM billing.ledger_transfers lt
  WHERE lt.merchant_id = g.merchant_id AND lt.credit_grant_id = g.id
    AND g.account_id IN (lt.debit_account_id, lt.credit_account_id)
) t ON true
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid AND g.id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY g.created_at DESC, g.id DESC;

-- name: GetCreditGrant :one
SELECT g.id, g.customer_id, g.unit, g.units, g.funding_source, g.paid_amount, g.paid_currency,
       g.source_id, g.description, g.starts_at, g.expires_at, g.revoked_at, g.created_at,
       g.units_credited AS credited, (g.units_credited - g.units_debited)::bigint AS remaining,
       revocation.reason AS revoke_reason,
       COALESCE(t.spent, 0)::bigint AS spent, COALESCE(t.expired, 0)::bigint AS expired,
       COALESCE(t.revoked, 0)::bigint AS revoked, COALESCE(t.refunded, 0)::bigint AS refunded
FROM billing.credit_grants g
LEFT JOIN LATERAL (
  SELECT e.reason FROM billing.credit_grant_events e
  WHERE e.merchant_id = g.merchant_id AND e.credit_grant_id = g.id AND e.event = 'revoke' AND g.revoked_at IS NOT NULL
  ORDER BY e.created_at DESC, e.id DESC LIMIT 1
) revocation ON true
LEFT JOIN LATERAL (
  SELECT sum(lt.amount) FILTER (WHERE lt.transfer_type IN ('credit_spend', 'owed_repayment')) AS spent,
         sum(lt.amount) FILTER (WHERE lt.transfer_type = 'credit_expire') AS expired,
         sum(CASE WHEN lt.debit_account_id = g.account_id THEN lt.amount ELSE -lt.amount END)
             FILTER (WHERE lt.transfer_type IN ('credit_revoke', 'credit_reinstate')) AS revoked,
         sum(CASE WHEN lt.debit_account_id = g.account_id THEN lt.amount ELSE -lt.amount END)
             FILTER (WHERE lt.transfer_type IN ('credit_refund', 'credit_refund_restore')) AS refunded
  FROM billing.ledger_transfers lt
  WHERE lt.merchant_id = g.merchant_id AND lt.credit_grant_id = g.id
    AND g.account_id IN (lt.debit_account_id, lt.credit_account_id)
) t ON true
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid AND g.id = sqlc.arg(id)::uuid;
