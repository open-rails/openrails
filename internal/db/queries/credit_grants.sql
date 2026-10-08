-- Customer support reads over the existing append-only grant and money ledgers.

-- name: ListCustomerCreditGrants :many
WITH page AS (
  SELECT g.* FROM billing.grants g
  WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
    AND g.customer_id = sqlc.arg(customer_id)::uuid
    AND (sqlc.narg(currency)::text IS NULL OR g.currency = sqlc.narg(currency)::text)
    AND (sqlc.narg(source_id)::text IS NULL OR g.source_id = sqlc.narg(source_id)::text)
    AND g.kind = 'credit' AND g.event = 'grant'
    AND (sqlc.narg(after_at)::timestamptz IS NULL
     OR (g.created_at, g.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
  ORDER BY g.created_at DESC, g.id DESC
  LIMIT sqlc.arg(row_limit)::int
)
SELECT g.id, g.customer_id, COALESCE(g.currency, '')::text AS currency,
       COALESCE(g.amount, 0)::bigint AS amount,
       g.source_type, g.source_id, g.reason, g.starts_at, g.ends_at, g.created_at,
       COALESCE(term.event, '')::text AS termination,
       term.starts_at AS terminated_at, term.reason AS termination_reason,
       COALESCE(t.spent, 0)::bigint AS spent_amount,
       COALESCE(t.revoked, 0)::bigint AS revoked_amount,
       COALESCE(t.expired, 0)::bigint AS expired_amount,
       (g.amount - COALESCE(t.spent,0) - COALESCE(t.revoked,0) - COALESCE(t.expired,0))::bigint AS remaining_amount
FROM page g
LEFT JOIN billing.grants term ON term.merchant_id = g.merchant_id AND term.supersedes_id = g.id
  AND term.event IN ('revoke','expire','supersede')
LEFT JOIN LATERAL (
  SELECT sum(lt.amount) FILTER (WHERE lt.transfer_type='credit_spend') AS spent,
         sum(CASE WHEN lt.transfer_type='credit_refund_restore' THEN -lt.amount ELSE lt.amount END) FILTER (WHERE lt.transfer_type IN ('credit_revoke','credit_refund','credit_refund_restore')) AS revoked,
         sum(lt.amount) FILTER (WHERE lt.transfer_type='credit_expire') AS expired
  FROM billing.ledger_transfers lt WHERE lt.merchant_id=g.merchant_id AND lt.grant_id=g.id
) t ON true
ORDER BY g.created_at DESC, g.id DESC;

-- name: GetCustomerCreditGrant :one
SELECT g.id, g.customer_id, COALESCE(g.currency, '')::text AS currency,
       COALESCE(g.amount, 0)::bigint AS amount,
       g.source_type, g.source_id, g.reason, g.starts_at, g.ends_at, g.created_at,
       COALESCE(term.event, '')::text AS termination,
       term.starts_at AS terminated_at, term.reason AS termination_reason,
       COALESCE(t.spent, 0)::bigint AS spent_amount,
       COALESCE(t.revoked, 0)::bigint AS revoked_amount,
       COALESCE(t.expired, 0)::bigint AS expired_amount,
       (g.amount - COALESCE(t.spent,0) - COALESCE(t.revoked,0) - COALESCE(t.expired,0))::bigint AS remaining_amount
FROM billing.grants g
LEFT JOIN billing.grants term ON term.merchant_id=g.merchant_id AND term.supersedes_id=g.id
  AND term.event IN ('revoke','expire','supersede')
LEFT JOIN LATERAL (
  SELECT sum(lt.amount) FILTER (WHERE lt.transfer_type='credit_spend') AS spent,
         sum(CASE WHEN lt.transfer_type='credit_refund_restore' THEN -lt.amount ELSE lt.amount END) FILTER (WHERE lt.transfer_type IN ('credit_revoke','credit_refund','credit_refund_restore')) AS revoked,
         sum(lt.amount) FILTER (WHERE lt.transfer_type='credit_expire') AS expired
  FROM billing.ledger_transfers lt WHERE lt.merchant_id=g.merchant_id AND lt.grant_id=g.id
) t ON true
WHERE g.merchant_id=sqlc.arg(merchant_id)::uuid AND g.customer_id=sqlc.arg(customer_id)::uuid
  AND g.id=sqlc.arg(grant_id)::uuid AND g.kind='credit' AND g.event='grant';
