-- Customer support reads over the existing append-only grant and money ledgers.

-- name: CountCustomerCreditGrants :one
SELECT count(*) FROM openrails.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND g.customer_id = sqlc.arg(customer_id)::uuid
  AND g.currency = sqlc.arg(currency)::text
  AND g.kind = 'credit' AND g.event = 'grant';

-- name: ListCustomerCreditGrants :many
WITH page AS (
  SELECT g.* FROM openrails.grants g
  WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
    AND g.customer_id = sqlc.arg(customer_id)::uuid
    AND g.currency = sqlc.arg(currency)::text
    AND g.kind = 'credit' AND g.event = 'grant'
  ORDER BY g.created_at DESC, g.id DESC
  LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int
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
LEFT JOIN openrails.grants term ON term.merchant_id = g.merchant_id AND term.supersedes_id = g.id
  AND term.event IN ('revoke','expire','supersede')
LEFT JOIN LATERAL (
  SELECT sum(lt.amount) FILTER (WHERE lt.transfer_type='credit_spend') AS spent,
         sum(lt.amount) FILTER (WHERE lt.transfer_type='credit_revoke') AS revoked,
         sum(lt.amount) FILTER (WHERE lt.transfer_type='credit_expire') AS expired
  FROM openrails.ledger_transfers lt WHERE lt.merchant_id=g.merchant_id AND lt.grant_id=g.id
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
FROM openrails.grants g
LEFT JOIN openrails.grants term ON term.merchant_id=g.merchant_id AND term.supersedes_id=g.id
  AND term.event IN ('revoke','expire','supersede')
LEFT JOIN LATERAL (
  SELECT sum(lt.amount) FILTER (WHERE lt.transfer_type='credit_spend') AS spent,
         sum(lt.amount) FILTER (WHERE lt.transfer_type='credit_revoke') AS revoked,
         sum(lt.amount) FILTER (WHERE lt.transfer_type='credit_expire') AS expired
  FROM openrails.ledger_transfers lt WHERE lt.merchant_id=g.merchant_id AND lt.grant_id=g.id
) t ON true
WHERE g.merchant_id=sqlc.arg(merchant_id)::uuid AND g.customer_id=sqlc.arg(customer_id)::uuid
  AND g.id=sqlc.arg(grant_id)::uuid AND g.kind='credit' AND g.event='grant';
