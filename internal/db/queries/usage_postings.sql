-- What the ledger took of usage, which lives in the event store: who has
-- usage not yet posted and what it costs at provisional prices, each posting,
-- and the rate cards usage was priced under.

-- name: UpsertUsagePending :exec
-- Intake queues customers' usage in a currency for posting, in the
-- transaction that commits it, in customer order: the oldest instant and the
-- earliest due date win.
INSERT INTO billing.usage_pending AS u (merchant_id, customer_id, currency, since_at, due_at, updated_at)
SELECT sqlc.arg(merchant_id)::uuid, m.customer_id, sqlc.arg(currency)::text, m.since_at, m.due_at, sqlc.arg(now)::timestamptz
FROM unnest(sqlc.arg(customer_ids)::uuid[], sqlc.arg(since_ats)::timestamptz[], sqlc.arg(due_ats)::timestamptz[]) AS m(customer_id, since_at, due_at)
ORDER BY m.customer_id
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE SET
    since_at = LEAST(u.since_at, EXCLUDED.since_at), due_at = LEAST(u.due_at, EXCLUDED.due_at), updated_at = EXCLUDED.updated_at;

-- name: ShareUsagePending :many
-- An intake's hold on the queue rows of its customers in a currency, taken
-- before it inserts its events and kept until it commits: a posting, which
-- locks the row FOR UPDATE, waits for every intake that may have inserted
-- usage, and later intakes insert theirs after it commits. Intakes never
-- wait on each other here.
SELECT customer_id, since_at, due_at FROM billing.usage_pending
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND currency = sqlc.arg(currency)::text
  AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
ORDER BY customer_id
FOR KEY SHARE;

-- name: AddUsagePendingAmounts :exec
-- Intake adds what one lease's usage costs at its provisional prices, per
-- customer, in customer order. Only that lease's syncs write its rows.
INSERT INTO billing.usage_pending_amounts AS p (merchant_id, customer_id, currency, lease_id, amount, updated_at)
SELECT sqlc.arg(merchant_id)::uuid, m.customer_id, sqlc.arg(currency)::text, sqlc.arg(lease_id)::uuid, m.amount, sqlc.arg(now)::timestamptz
FROM unnest(sqlc.arg(customer_ids)::uuid[], sqlc.arg(amounts)::bigint[]) AS m(customer_id, amount)
WHERE m.amount > 0
ORDER BY m.customer_id
ON CONFLICT (merchant_id, customer_id, currency, lease_id) DO UPDATE SET
    amount = p.amount + EXCLUDED.amount, updated_at = EXCLUDED.updated_at;

-- name: DeleteUsagePendingAmounts :exec
-- A posting that took all the event store held clears what was pending.
DELETE FROM billing.usage_pending_amounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text;

-- name: LockUsagePending :one
SELECT * FROM billing.usage_pending
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text
FOR UPDATE;

-- name: GetUsagePending :one
SELECT * FROM billing.usage_pending
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text;

-- name: SetUsagePending :exec
UPDATE billing.usage_pending
SET since_at = sqlc.arg(since_at)::timestamptz, due_at = sqlc.arg(due_at)::timestamptz, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text;

-- name: DeleteUsagePending :exec
DELETE FROM billing.usage_pending
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text;

-- name: ListDueUsagePending :many
-- CROSS-MERCHANT: the daily posting's due work, in due order. An idle pass
-- reads no row.
SELECT u.due_at, u.merchant_id, u.customer_id, u.currency
FROM billing.usage_pending u
WHERE u.due_at <= sqlc.arg(due_at)::timestamptz
  AND (sqlc.narg(after_due_at)::timestamptz IS NULL
       OR (u.due_at, u.merchant_id, u.customer_id, u.currency) > (sqlc.narg(after_due_at)::timestamptz, sqlc.narg(after_merchant_id)::uuid, sqlc.narg(after_customer_id)::uuid, sqlc.narg(after_currency)::text))
ORDER BY u.due_at, u.merchant_id, u.customer_id, u.currency
LIMIT sqlc.arg(row_limit)::int;

-- name: CountUsagePendingByMerchant :many
-- CROSS-MERCHANT: whose usage only the event store can post.
SELECT merchant_id, count(*)::bigint AS customers
FROM billing.usage_pending
GROUP BY merchant_id
ORDER BY merchant_id;

-- name: DiscardUsagePending :many
-- An operator's discard of one merchant's pending usage; its amounts go with
-- it.
DELETE FROM billing.usage_pending
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
RETURNING customer_id, currency, since_at;

-- name: InsertUsagePosting :one
INSERT INTO billing.usage_postings (merchant_id, customer_id, currency, period_starts_at, period_ends_at, cut_at, reason, amount, lines,
    ingested_through, forgiven, forgiven_lines, created_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text, sqlc.arg(period_starts_at)::timestamptz,
    sqlc.arg(period_ends_at)::timestamptz, sqlc.arg(cut_at)::timestamptz, sqlc.arg(reason)::text, sqlc.arg(amount)::bigint,
    sqlc.arg(lines)::jsonb, sqlc.arg(ingested_through)::timestamptz, sqlc.arg(forgiven)::bigint, sqlc.narg(forgiven_lines)::jsonb,
    sqlc.arg(now)::timestamptz)
RETURNING *;

-- name: ListForgivenUsagePostings :many
-- The merchant's postings that forgave usage or took forgiveness back, cut
-- after one instant and through another.
SELECT * FROM billing.usage_postings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND forgiven <> 0
  AND cut_at > sqlc.arg(cut_after)::timestamptz AND cut_at <= sqlc.arg(cut_through)::timestamptz
ORDER BY cut_at, id;

-- name: ListUsagePostings :many
-- A customer's postings in a currency of the periods from one start on.
SELECT * FROM billing.usage_postings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text AND period_starts_at >= sqlc.arg(period_starts_at)::timestamptz
ORDER BY period_starts_at, created_at, id;

-- name: ListUsagePostingsByIDs :many
SELECT * FROM billing.usage_postings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: GetUsagePostingTransferAfter :one
-- The first ledger leg of the first posting of a customer's usage cut after
-- an instant: what took the usage settled then.
SELECT lt.id
FROM billing.usage_postings p
JOIN billing.ledger_transfers lt
  ON lt.merchant_id = p.merchant_id AND lt.customer_id = p.customer_id AND lt.currency = p.currency
 AND lt.operation = 'usage_posting' AND lt.source = 'usage_posting:' || p.id::text
 AND lt.transfer_type IN ('credit_spend', 'owed_accrual')
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.customer_id = sqlc.arg(customer_id)::uuid
  AND p.currency = sqlc.arg(currency)::text AND p.cut_at > sqlc.arg(at)::timestamptz AND p.amount > 0
  AND p.period_starts_at <= sqlc.arg(at)::timestamptz AND p.period_ends_at > sqlc.arg(at)::timestamptz
ORDER BY p.created_at, p.id, lt.created_at, lt.id
LIMIT 1;

-- name: InsertUsageCardPrint :exec
INSERT INTO billing.usage_card_prints (merchant_id, print, card, created_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(print)::text, sqlc.arg(card)::jsonb, sqlc.arg(now)::timestamptz)
ON CONFLICT (merchant_id, print) DO NOTHING;

-- name: ListUsageCardPrints :many
SELECT print, card, created_at FROM billing.usage_card_prints
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND print = ANY(sqlc.arg(prints)::text[]);

-- name: LockUsageEventTypes :exec
-- A meter correction's fence on its merchant's usage of event types: held
-- exclusively against ShareUsageEventTypes, which every intake takes before
-- it reads its meters. No other merchant or event type waits.
SELECT pg_advisory_xact_lock(hashtextextended('openrails.usage_activity:' || sqlc.arg(merchant_id)::uuid::text || ':' || e.event_type, 0))
FROM (SELECT DISTINCT unnest(sqlc.arg(event_types)::text[]) AS event_type ORDER BY 1) e;

-- name: ShareUsageEventTypes :exec
-- An intake's fence on the event types it takes in, in the order
-- LockUsageEventTypes takes them.
SELECT pg_advisory_xact_lock_shared(hashtextextended('openrails.usage_activity:' || sqlc.arg(merchant_id)::uuid::text || ':' || e.event_type, 0))
FROM (SELECT DISTINCT unnest(sqlc.arg(event_types)::text[]) AS event_type ORDER BY 1) e;

-- name: ListMerchantUsagePending :many
-- One merchant's customers whose usage is not posted.
SELECT customer_id, currency FROM billing.usage_pending
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY customer_id, currency;

-- name: RecordUsageDiscard :one
-- An operator's discard of a merchant's pending usage, with what was
-- discarded.
INSERT INTO billing.maintenance_runs (merchant_id, kind, actor, status, started_at, finished_at, affected, note)
VALUES (sqlc.arg(merchant_id)::uuid, 'usage_discard', sqlc.arg(actor)::text, 'completed', sqlc.arg(now)::timestamptz,
    sqlc.arg(now)::timestamptz, sqlc.arg(affected)::jsonb, sqlc.arg(note)::text)
RETURNING id;
