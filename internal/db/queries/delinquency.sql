-- Arrears delinquency: the state projection, its due work, and the durable
-- host-lifecycle signal feed.

-- CROSS-MERCHANT: the payers whose delinquency time alone changes now (a usage
-- order fell due, a grace window ran out), in due order. Strictly before now:
-- an order is overdue only once its due_at has passed. An idle pass reads no
-- row.
-- name: ListDueDelinquency :many
SELECT merchant_id, customer_id, currency, next_evaluation_at::timestamptz AS next_evaluation_at
FROM billing.customer_delinquency
WHERE next_evaluation_at < sqlc.arg(now)::timestamptz
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (next_evaluation_at, merchant_id, customer_id, currency)
          > (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_merchant_id)::uuid, sqlc.narg(after_customer_id)::uuid, sqlc.narg(after_currency)::text))
ORDER BY next_evaluation_at, merchant_id, customer_id, currency
LIMIT sqlc.arg(row_limit)::int;

-- name: GetOverdueOrderAggregate :one
-- One payer's usage orders in a currency against now: how much is overdue and
-- since when, and when the next one falls due. Every evaluation and the
-- admission gate's live recompute read it, so the stored state and the gate
-- agree. grace_days and amount_floor are the bound billing policy's overrides,
-- -1 for none. policies is the merchant's settings, {policies: {name: body},
-- tiers: {tier: name}, default: name}; the payer's policy is its own
-- assignment while the settings still declare it, else its tier's
-- (money_settings.tier), else the default.
--
-- overdue_started_at and next_due_at are COALESCEd to now so the row is total:
-- they mean nothing when their count is 0, and every caller reads it first.
WITH pol AS (
    SELECT (bound.body ->> 'delinquency_grace_days')::int AS grace_days,
           (bound.body ->> 'delinquency_amount_floor')::bigint AS amount_floor
    FROM billing.customers c
    LEFT JOIN billing.money_settings ms
      ON ms.merchant_id = c.merchant_id AND ms.customer_id = c.id AND ms.currency = sqlc.arg(currency)
    CROSS JOIN LATERAL (
        SELECT sqlc.arg(policies)::jsonb -> 'policies' -> COALESCE(
            CASE WHEN sqlc.arg(policies)::jsonb -> 'policies' -> c.billing_policy IS NOT NULL THEN c.billing_policy END,
            sqlc.arg(policies)::jsonb -> 'tiers' ->> ms.tier,
            sqlc.arg(policies)::jsonb ->> 'default') AS body
    ) bound
    WHERE c.merchant_id = sqlc.arg(merchant_id) AND c.id = sqlc.arg(customer_id)
)
SELECT COALESCE(MIN(o.due_at) FILTER (WHERE o.due_at < sqlc.arg(now)::timestamptz), sqlc.arg(now)::timestamptz)::timestamptz AS overdue_started_at,
       COALESCE(SUM(o.total - o.amount_paid) FILTER (WHERE o.due_at < sqlc.arg(now)::timestamptz), 0)::bigint AS overdue_amount,
       (COUNT(*) FILTER (WHERE o.due_at < sqlc.arg(now)::timestamptz))::bigint AS overdue_orders,
       COALESCE(MIN(o.due_at) FILTER (WHERE o.due_at >= sqlc.arg(now)::timestamptz), sqlc.arg(now)::timestamptz)::timestamptz AS next_due_at,
       (COUNT(*) FILTER (WHERE o.due_at >= sqlc.arg(now)::timestamptz))::bigint AS pending_orders,
       COALESCE((SELECT grace_days FROM pol), -1)::int AS grace_days,
       COALESCE((SELECT amount_floor FROM pol), -1)::bigint AS amount_floor
FROM billing.orders o
WHERE o.merchant_id = sqlc.arg(merchant_id)
  AND o.customer_id = sqlc.arg(customer_id)
  AND o.currency = sqlc.arg(currency)
  AND o.origin = 'usage'
  AND o.status IN ('open', 'processing')
  AND o.amount_paid < o.total;

-- name: NotifyOverdueOrders :exec
-- Tells the payer once about each of its open usage orders in the currency
-- past its due date; the notice id is the order's, so a later evaluation adds
-- none. One with a collection in flight waits for its outcome.
INSERT INTO billing.notification_events (id, merchant_id, customer_id, event_type, data, created_at)
SELECT md5('invoice_overdue:' || o.id::text)::uuid, o.merchant_id, o.customer_id, 'order_overdue',
       jsonb_build_object('order_id', 'ord_' || o.id::text, 'amount_due', (o.total - o.amount_paid)::text, 'currency', o.currency, 'due_at', o.due_at),
       sqlc.arg(now)::timestamptz
FROM billing.orders o
WHERE o.merchant_id = sqlc.arg(merchant_id)::uuid AND o.customer_id = sqlc.arg(customer_id)::uuid
  AND o.currency = sqlc.arg(currency)::text
  AND o.origin = 'usage' AND o.status = 'open' AND o.amount_paid < o.total
  AND o.due_at < sqlc.arg(now)::timestamptz
ON CONFLICT (merchant_id, id) DO NOTHING;

-- name: ScheduleDelinquency :exec
-- The payer is evaluated on the next pass: its policy could not be read when
-- one of its usage orders changed.
INSERT INTO billing.customer_delinquency (merchant_id, customer_id, currency, state, overdue_amount, overdue_orders,
    entered_at, transition_seq, evaluated_at, next_evaluation_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text, 'current', 0, 0,
    sqlc.arg(now)::timestamptz, 0, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE
SET next_evaluation_at = LEAST(billing.customer_delinquency.next_evaluation_at, EXCLUDED.next_evaluation_at);

-- name: MarkDelinquencyDue :exec
-- The merchant's delinquency policy may have changed: every payer it holds
-- in grace or delinquent is evaluated again on the next pass.
UPDATE billing.customer_delinquency
SET next_evaluation_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND state <> 'current'
  AND next_evaluation_at IS DISTINCT FROM sqlc.arg(now)::timestamptz;

-- name: RebuildDelinquencyDue :exec
-- A restored merchant's payers with an open usage order are evaluated again
-- at once; MarkDelinquencyDue covers the ones in grace or delinquent.
INSERT INTO billing.customer_delinquency (merchant_id, customer_id, currency, state, overdue_amount, overdue_orders,
    entered_at, transition_seq, evaluated_at, next_evaluation_at)
SELECT DISTINCT o.merchant_id, o.customer_id, o.currency, 'current', 0, 0,
       sqlc.arg(now)::timestamptz, 0, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz
FROM billing.orders o
WHERE o.merchant_id = sqlc.arg(merchant_id)::uuid AND o.origin = 'usage'
  AND o.status IN ('open', 'processing') AND o.amount_paid < o.total
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE SET next_evaluation_at = EXCLUDED.next_evaluation_at;

-- name: GetCustomerDelinquency :one
SELECT * FROM billing.customer_delinquency
WHERE merchant_id = sqlc.arg(merchant_id)
  AND customer_id = sqlc.arg(customer_id)
  AND currency = sqlc.arg(currency);

-- name: ListCustomersDelinquency :many
-- Every currency the named customers have been overdue in: at most one row
-- per customer and registered currency, which bounds row_limit.
SELECT * FROM billing.customer_delinquency
WHERE merchant_id = sqlc.arg(merchant_id)
  AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
ORDER BY customer_id, currency
LIMIT sqlc.arg(row_limit)::int;

-- name: UpsertCustomerDelinquency :one
-- One evaluation, atomic, returning BOTH the state that was there and the state
-- that is there now — the edge, which is what the signal is cut from.
--
-- transition_seq advances only on a real change, so it is a deterministic
-- coordinate for the emitted event: two evaluators racing the same transition
-- compute the same sequence, hence the same dedupe key, hence one event.
WITH previous AS (
    SELECT state, entered_at, transition_seq
    FROM billing.customer_delinquency
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND customer_id = sqlc.arg(customer_id)::uuid
      AND currency = sqlc.arg(currency)::text
), upserted AS (
    INSERT INTO billing.customer_delinquency AS d (
        merchant_id, customer_id, currency, state, overdue_started_at, overdue_amount,
        overdue_orders, entered_at, transition_seq, evaluated_at, next_evaluation_at, created_at, updated_at)
    VALUES (
        sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text, sqlc.arg(state)::text,
        sqlc.narg(overdue_started_at)::timestamptz, sqlc.arg(overdue_amount)::bigint,
        sqlc.arg(overdue_orders)::bigint, sqlc.arg(now)::timestamptz,
        CASE WHEN sqlc.arg(state)::text = 'current' THEN 0 ELSE 1 END,
        sqlc.arg(now)::timestamptz, sqlc.narg(next_evaluation_at)::timestamptz, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
    ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE
    SET state = EXCLUDED.state,
        overdue_started_at = EXCLUDED.overdue_started_at,
        overdue_amount = EXCLUDED.overdue_amount,
        overdue_orders = EXCLUDED.overdue_orders,
        entered_at = CASE WHEN d.state IS DISTINCT FROM EXCLUDED.state THEN EXCLUDED.entered_at ELSE d.entered_at END,
        transition_seq = d.transition_seq + (CASE WHEN d.state IS DISTINCT FROM EXCLUDED.state THEN 1 ELSE 0 END),
        evaluated_at = EXCLUDED.evaluated_at,
        next_evaluation_at = EXCLUDED.next_evaluation_at,
        updated_at = EXCLUDED.updated_at
    RETURNING d.state, d.overdue_started_at, d.overdue_amount, d.overdue_orders, d.entered_at, d.transition_seq
)
SELECT upserted.state,
       upserted.overdue_started_at,
       upserted.overdue_amount,
       upserted.overdue_orders,
       upserted.entered_at,
       upserted.transition_seq,
       COALESCE((SELECT previous.state FROM previous), '')::text AS previous_state
FROM upserted;

-- A row is NEVER deleted once it exists, and one is created `current` only to
-- hold a payer's next evaluation (transition_seq 0). Deleting a cleared row would
-- restart transition_seq, and a restarted sequence collides with the dedupe key
-- of the payer's earlier transition — which would silently swallow the second
-- shutoff signal for anyone who has been delinquent before.
