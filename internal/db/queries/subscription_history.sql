-- name: ListSubscriptionHistory :many
-- Bounded source pages, merged by time/source/id. Every join and source
-- is merchant-scoped. No current subscription values supply historical facts.
WITH transitions AS (
    SELECT 1::int AS source, t.id, t.occurred_at AS at, t.occurred_at AS recorded_at,
           COALESCE(t.decision, '')::text AS kind, ''::text AS reason,
           COALESCE(t.from_status, '')::text AS from_status, t.to_status,
           COALESCE(t.cancel_type, '')::text AS cancel_type,
           t.from_current_period_ends_at AS from_period_end, t.to_current_period_ends_at AS to_period_end,
           ''::text AS category, ''::text AS action, ''::text AS observed_via, ''::text AS origin,
           t.operation_id, NULL::uuid AS order_id, NULL::uuid AS payment_id,
           NULL::uuid AS cycle_id, NULL::uuid AS payment_method_id, NULL::uuid AS psp_id,
           0::bigint AS amount, ''::text AS currency, 0::int AS attempt,
           COALESCE(t.origin, '')::text AS attribution_origin, COALESCE(t.actor, '')::text AS actor, COALESCE(t.executor, '')::text AS executor,
           NULL::uuid AS from_payment_method_id, NULL::uuid AS to_payment_method_id, NULL::timestamptz AS from_next_retry_at, NULL::timestamptz AS to_next_retry_at, NULL::timestamptz AS from_last_retry_at, NULL::timestamptz AS to_last_retry_at, NULL::int AS from_retry_attempts, NULL::int AS to_retry_attempts, NULL::int AS from_transient_retries, NULL::int AS to_transient_retries, NULL::timestamptz AS from_grace_ends_at, NULL::timestamptz AS to_grace_ends_at, NULL::uuid AS method_version_id
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
           a.psp_id, a.amount, COALESCE(a.currency, '')::text AS currency, 0::int AS attempt,
           ''::text AS attribution_origin, ''::text AS actor, ''::text AS executor,
           NULL::uuid AS from_payment_method_id, NULL::uuid AS to_payment_method_id, NULL::timestamptz AS from_next_retry_at, NULL::timestamptz AS to_next_retry_at, NULL::timestamptz AS from_last_retry_at, NULL::timestamptz AS to_last_retry_at, NULL::int AS from_retry_attempts, NULL::int AS to_retry_attempts, NULL::int AS from_transient_retries, NULL::int AS to_transient_retries, NULL::timestamptz AS from_grace_ends_at, NULL::timestamptz AS to_grace_ends_at, NULL::uuid AS method_version_id
    FROM billing.payment_attempts a
    WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.subscription_id = sqlc.arg(subscription_id)::uuid
      AND (sqlc.narg(after_at)::timestamptz IS NULL
           OR (a.attempted_at, 2::int, a.id) < (sqlc.narg(after_at)::timestamptz, sqlc.arg(after_source)::int, sqlc.arg(after_id)::uuid))
    ORDER BY a.attempted_at DESC, a.id DESC LIMIT sqlc.arg(row_limit)::int
), legacy_operations AS (
    SELECT 3::int AS source, l.id, l.created_at AS at, l.created_at AS recorded_at,
           COALESCE(l.intent_type, '')::text AS kind, COALESCE(l.reason_code, '')::text AS reason,
           ''::text AS from_status, ''::text AS to_status, ''::text AS cancel_type,
           NULL::timestamptz AS from_period_end, NULL::timestamptz AS to_period_end,
           l.phase AS category, ''::text AS action, ''::text AS observed_via, ''::text AS origin,
           COALESCE(l.operation_id,l.provider_intent_id) AS operation_id, NULL::uuid AS order_id, NULL::uuid AS payment_id,
           NULL::uuid AS cycle_id, NULL::uuid AS payment_method_id, l.psp_id,
           0::bigint AS amount, ''::text AS currency, l.attempt,
           COALESCE(l.origin, '')::text AS attribution_origin, COALESCE(l.actor, '')::text AS actor, COALESCE(l.executor, '')::text AS executor,
           NULL::uuid AS from_payment_method_id, NULL::uuid AS to_payment_method_id, NULL::timestamptz AS from_next_retry_at, NULL::timestamptz AS to_next_retry_at, NULL::timestamptz AS from_last_retry_at, NULL::timestamptz AS to_last_retry_at, NULL::int AS from_retry_attempts, NULL::int AS to_retry_attempts, NULL::int AS from_transient_retries, NULL::int AS to_transient_retries, NULL::timestamptz AS from_grace_ends_at, NULL::timestamptz AS to_grace_ends_at, NULL::uuid AS method_version_id
    FROM billing.provider_intents i
    JOIN billing.provider_mutation_logs l ON l.merchant_id = i.merchant_id AND l.provider_intent_id = i.id
    WHERE l.subscription_id IS NULL AND i.merchant_id = sqlc.arg(merchant_id)::uuid AND i.subscription_id = sqlc.arg(subscription_id)::uuid
      AND (sqlc.narg(after_at)::timestamptz IS NULL
           OR (l.created_at, 3::int, l.id) < (sqlc.narg(after_at)::timestamptz, sqlc.arg(after_source)::int, sqlc.arg(after_id)::uuid))
    ORDER BY l.created_at DESC, l.id DESC LIMIT sqlc.arg(row_limit)::int
), operations AS (
    SELECT 3::int AS source, l.id, l.created_at AS at, l.created_at AS recorded_at,
           COALESCE(l.intent_type, '')::text AS kind, COALESCE(l.reason_code, '')::text AS reason,
           ''::text AS from_status, ''::text AS to_status, ''::text AS cancel_type,
           NULL::timestamptz AS from_period_end, NULL::timestamptz AS to_period_end,
           l.phase AS category, ''::text AS action, ''::text AS observed_via, ''::text AS origin,
           COALESCE(l.operation_id,l.provider_intent_id) AS operation_id, NULL::uuid AS order_id, NULL::uuid AS payment_id,
           NULL::uuid AS cycle_id, NULL::uuid AS payment_method_id, l.psp_id,
           0::bigint AS amount, ''::text AS currency, l.attempt,
           COALESCE(l.origin, '')::text AS attribution_origin, COALESCE(l.actor, '')::text AS actor, COALESCE(l.executor, '')::text AS executor,
           NULL::uuid AS from_payment_method_id, NULL::uuid AS to_payment_method_id, NULL::timestamptz AS from_next_retry_at, NULL::timestamptz AS to_next_retry_at, NULL::timestamptz AS from_last_retry_at, NULL::timestamptz AS to_last_retry_at, NULL::int AS from_retry_attempts, NULL::int AS to_retry_attempts, NULL::int AS from_transient_retries, NULL::int AS to_transient_retries, NULL::timestamptz AS from_grace_ends_at, NULL::timestamptz AS to_grace_ends_at, NULL::uuid AS method_version_id
    FROM billing.provider_mutation_logs l
    WHERE l.merchant_id = sqlc.arg(merchant_id)::uuid AND l.subscription_id = sqlc.arg(subscription_id)::uuid
      AND (sqlc.narg(after_at)::timestamptz IS NULL
           OR (l.created_at, 3::int, l.id) < (sqlc.narg(after_at)::timestamptz, sqlc.arg(after_source)::int, sqlc.arg(after_id)::uuid))
    ORDER BY l.created_at DESC, l.id DESC LIMIT sqlc.arg(row_limit)::int
), changes AS (
    SELECT 4::int AS source,c.id,c.occurred_at AS at,c.occurred_at AS recorded_at,
           c.kind,COALESCE(c.decision,'')::text AS reason,
           ''::text AS from_status,''::text AS to_status,''::text AS cancel_type,
           NULL::timestamptz AS from_period_end,NULL::timestamptz AS to_period_end,
           ''::text AS category,''::text AS action,''::text AS observed_via,''::text AS origin,
           c.operation_id,NULL::uuid AS order_id,NULL::uuid AS payment_id,NULL::uuid AS cycle_id,
           NULL::uuid AS payment_method_id,NULL::uuid AS psp_id,0::bigint AS amount,''::text AS currency,0::int AS attempt,
           COALESCE(c.origin,'')::text AS attribution_origin,COALESCE(c.actor,'')::text AS actor,COALESCE(c.executor,'')::text AS executor,
           c.from_payment_method_id, c.to_payment_method_id, c.from_next_retry_at, c.to_next_retry_at, c.from_last_retry_at, c.to_last_retry_at, c.from_retry_attempts, c.to_retry_attempts, c.from_transient_retries, c.to_transient_retries, c.from_grace_ends_at, c.to_grace_ends_at, c.method_version_id
    FROM billing.subscription_billing_changes c
    WHERE c.merchant_id=sqlc.arg(merchant_id)::uuid AND c.subscription_id=sqlc.arg(subscription_id)::uuid
      AND (sqlc.narg(after_at)::timestamptz IS NULL
        OR (c.occurred_at,4::int,c.id) < (sqlc.narg(after_at)::timestamptz,sqlc.arg(after_source)::int,sqlc.arg(after_id)::uuid))
    ORDER BY c.occurred_at DESC,c.id DESC LIMIT sqlc.arg(row_limit)::int
)
SELECT * FROM transitions
UNION ALL SELECT * FROM attempts
UNION ALL SELECT * FROM operations
UNION ALL SELECT * FROM legacy_operations
UNION ALL SELECT * FROM changes
ORDER BY at DESC, source DESC, id DESC LIMIT sqlc.arg(row_limit)::int;
