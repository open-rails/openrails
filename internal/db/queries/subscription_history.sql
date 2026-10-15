-- name: ListSubscriptionHistory :many
-- Three bounded source pages, merged by time/source/id. Every join and source
-- is merchant-scoped. No current subscription values supply historical facts.
WITH transitions AS (
    SELECT 1::int AS source, t.id, t.occurred_at AS at, t.occurred_at AS recorded_at,
           COALESCE(t.decision, '')::text AS kind, ''::text AS reason,
           COALESCE(t.from_status, '')::text AS from_status, t.to_status,
           COALESCE(t.cancel_type, '')::text AS cancel_type,
           t.from_current_period_ends_at AS from_period_end, t.to_current_period_ends_at AS to_period_end,
           ''::text AS category, ''::text AS action, ''::text AS observed_via, ''::text AS origin,
           NULL::uuid AS operation_id, NULL::uuid AS order_id, NULL::uuid AS payment_id,
           NULL::uuid AS cycle_id, NULL::uuid AS payment_method_id, NULL::uuid AS psp_id,
           0::bigint AS amount, ''::text AS currency, 0::int AS attempt
    FROM billing.subscription_status_transitions t
    WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid AND t.subscription_id = sqlc.arg(subscription_id)::uuid
      AND (sqlc.narg(after_at)::timestamptz IS NULL
           OR (t.occurred_at, 1::int, t.id) < (sqlc.narg(after_at)::timestamptz, sqlc.arg(after_source)::int, sqlc.arg(after_id)::uuid))
    ORDER BY t.occurred_at DESC, t.id DESC LIMIT sqlc.arg(row_limit)::int
), attempts AS (
    SELECT 2::int AS source, a.id, a.attempted_at AS at, a.created_at AS recorded_at,
           a.kind, COALESCE(a.reason, '')::text AS reason,
           ''::text AS from_status, ''::text AS to_status, ''::text AS cancel_type,
           NULL::timestamptz AS from_period_end, NULL::timestamptz AS to_period_end,
           a.category, COALESCE(a.action, '')::text AS action, a.observed_via, a.source AS origin,
           a.provider_intent_id AS operation_id, a.order_id, a.payment_id, a.cycle_id, a.payment_method_id,
           a.psp_id, a.amount, COALESCE(a.currency, '')::text AS currency, 0::int AS attempt
    FROM billing.payment_attempts a
    WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.subscription_id = sqlc.arg(subscription_id)::uuid
      AND (sqlc.narg(after_at)::timestamptz IS NULL
           OR (a.attempted_at, 2::int, a.id) < (sqlc.narg(after_at)::timestamptz, sqlc.arg(after_source)::int, sqlc.arg(after_id)::uuid))
    ORDER BY a.attempted_at DESC, a.id DESC LIMIT sqlc.arg(row_limit)::int
), operations AS (
    SELECT 3::int AS source, l.id, l.created_at AS at, l.created_at AS recorded_at,
           COALESCE(l.intent_type, '')::text AS kind, ''::text AS reason,
           ''::text AS from_status, ''::text AS to_status, ''::text AS cancel_type,
           NULL::timestamptz AS from_period_end, NULL::timestamptz AS to_period_end,
           l.phase AS category, ''::text AS action, ''::text AS observed_via, ''::text AS origin,
           l.provider_intent_id AS operation_id, NULL::uuid AS order_id, NULL::uuid AS payment_id,
           NULL::uuid AS cycle_id, NULL::uuid AS payment_method_id, l.psp_id,
           0::bigint AS amount, ''::text AS currency, l.attempt
    FROM billing.provider_intents i
    JOIN billing.provider_mutation_logs l ON l.merchant_id = i.merchant_id AND l.provider_intent_id = i.id
    WHERE i.merchant_id = sqlc.arg(merchant_id)::uuid AND i.subscription_id = sqlc.arg(subscription_id)::uuid
      AND (sqlc.narg(after_at)::timestamptz IS NULL
           OR (l.created_at, 3::int, l.id) < (sqlc.narg(after_at)::timestamptz, sqlc.arg(after_source)::int, sqlc.arg(after_id)::uuid))
    ORDER BY l.created_at DESC, l.id DESC LIMIT sqlc.arg(row_limit)::int
)
SELECT * FROM transitions
UNION ALL SELECT * FROM attempts
UNION ALL SELECT * FROM operations
ORDER BY at DESC, source DESC, id DESC LIMIT sqlc.arg(row_limit)::int;
