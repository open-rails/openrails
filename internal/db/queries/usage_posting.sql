-- What the ledger took of usage and running charges: who has either that no
-- posting has taken (the queue), the running totals intake settles and
-- postings price, each posting's usage receipt, and the rate cards usage was
-- priced under.

-- name: UpsertUsagePending :exec
-- Intake queues customers' usage in a currency for posting, in the
-- transaction that settles it, in customer order, and a running charge's
-- start its customer: the earliest due date wins.
INSERT INTO billing.usage_pending AS u (merchant_id, customer_id, currency, due_at, updated_at)
SELECT sqlc.arg(merchant_id)::uuid, m.customer_id, sqlc.arg(currency)::text, m.due_at, sqlc.arg(now)::timestamptz
FROM unnest(sqlc.arg(customer_ids)::uuid[], sqlc.arg(due_ats)::timestamptz[]) AS m(customer_id, due_at)
ORDER BY m.customer_id
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE SET
    due_at = LEAST(u.due_at, EXCLUDED.due_at), updated_at = EXCLUDED.updated_at;

-- name: ShareUsagePending :many
-- An intake's hold on the queue rows of its customers in a currency, taken
-- before it writes its usage and kept until it commits: a posting, which
-- locks the row FOR UPDATE, waits for every intake that may have settled
-- usage, and later intakes settle theirs after it commits. Intakes never
-- wait on each other here.
SELECT customer_id, due_at FROM billing.usage_pending
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND currency = sqlc.arg(currency)::text
  AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
ORDER BY customer_id
FOR KEY SHARE;

-- name: LockUsagePending :one
SELECT * FROM billing.usage_pending
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text
FOR UPDATE;

-- name: GetUsagePending :one
SELECT * FROM billing.usage_pending
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text;

-- name: SetUsagePendingDue :exec
UPDATE billing.usage_pending
SET due_at = sqlc.arg(due_at)::timestamptz, updated_at = sqlc.arg(now)::timestamptz
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

-- name: AddUsageTotals :exec
-- Intake adds what one lease settled, per customer, currency, pricing period,
-- meter cell, card print and lapse segment, in key order. Only that lease's
-- syncs write its rows; the caller sums each key once.
INSERT INTO billing.usage_totals AS t (merchant_id, customer_id, currency, period_starts_at, period_ends_at, meter_key, cell, print, lapse_at, lease_id,
    quantity, stated, requests, amount, updated_at)
SELECT sqlc.arg(merchant_id)::uuid, m.customer_id, m.currency, m.period_starts_at, m.period_ends_at, m.meter_key, m.cell, m.print, m.lapse_at,
       sqlc.arg(lease_id)::uuid, m.quantity, m.stated, m.requests, m.amount, sqlc.arg(now)::timestamptz
FROM unnest(sqlc.arg(customer_ids)::uuid[], sqlc.arg(currencies)::text[], sqlc.arg(period_starts_ats)::timestamptz[], sqlc.arg(period_ends_ats)::timestamptz[],
    sqlc.arg(meter_keys)::text[], sqlc.arg(cells)::text[], sqlc.arg(prints)::text[], sqlc.arg(lapse_ats)::timestamptz[], sqlc.arg(quantities)::bigint[],
    sqlc.arg(stateds)::bigint[], sqlc.arg(requests)::bigint[], sqlc.arg(amounts)::bigint[])
    AS m(customer_id, currency, period_starts_at, period_ends_at, meter_key, cell, print, lapse_at, quantity, stated, requests, amount)
ORDER BY m.customer_id, m.currency, m.period_starts_at, m.meter_key, m.cell, m.print, m.lapse_at
ON CONFLICT (merchant_id, customer_id, currency, period_starts_at, meter_key, cell, print, lapse_at, lease_id) DO UPDATE SET
    quantity = t.quantity + EXCLUDED.quantity, stated = t.stated + EXCLUDED.stated, requests = t.requests + EXCLUDED.requests,
    amount = t.amount + EXCLUDED.amount, updated_at = EXCLUDED.updated_at;

-- name: ListUsagePositions :many
-- Where customers stand in each meter cell of their pricing periods from one
-- start on, every lease's settled usage and what posted: the tier position
-- intake prices the next items at.
SELECT customer_id, currency, period_starts_at, meter_key, cell, sum(quantity)::bigint AS quantity
FROM billing.usage_totals
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
  AND period_starts_at >= sqlc.arg(period_starts_at)::timestamptz
GROUP BY customer_id, currency, period_starts_at, meter_key, cell;

-- name: ListUsageTotals :many
-- A customer's running totals in a currency: what a posting prices.
SELECT * FROM billing.usage_totals
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text
ORDER BY period_starts_at, meter_key, cell, print, lapse_at, lease_id;

-- name: FoldUsageTotals :exec
-- A posting folds the unposted rows it took (all, or those of the lapse
-- segments and periods before a cut) into each period's posted row. The
-- caller holds the customer's queue row, so no intake writes meanwhile.
WITH taken AS (
    DELETE FROM billing.usage_totals
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text
      AND lease_id <> '00000000-0000-0000-0000-000000000000'::uuid
      AND (sqlc.narg(cut)::timestamptz IS NULL OR lapse_at < sqlc.narg(cut)::timestamptz AND period_starts_at < sqlc.narg(cut)::timestamptz)
    RETURNING merchant_id, customer_id, currency, period_starts_at, period_ends_at, meter_key, cell, print, quantity, stated, requests
)
INSERT INTO billing.usage_totals AS t (merchant_id, customer_id, currency, period_starts_at, period_ends_at, meter_key, cell, print, lapse_at, lease_id,
    quantity, stated, requests, amount, updated_at)
SELECT merchant_id, customer_id, currency, period_starts_at, max(period_ends_at), meter_key, cell, print, period_starts_at,
       '00000000-0000-0000-0000-000000000000'::uuid, sum(quantity)::bigint, sum(stated)::bigint, sum(requests)::bigint, 0, sqlc.arg(now)::timestamptz
FROM taken
GROUP BY merchant_id, customer_id, currency, period_starts_at, meter_key, cell, print
ORDER BY period_starts_at, meter_key, cell, print
ON CONFLICT (merchant_id, customer_id, currency, period_starts_at, meter_key, cell, print, lapse_at, lease_id) DO UPDATE SET
    quantity = t.quantity + EXCLUDED.quantity, stated = t.stated + EXCLUDED.stated, requests = t.requests + EXCLUDED.requests,
    updated_at = EXCLUDED.updated_at;

-- name: InsertUsageReceipt :one
INSERT INTO billing.usage_receipts (merchant_id, customer_id, currency, cut_at, reason, amount, lines, charges, forgiven, created_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text, sqlc.arg(cut_at)::timestamptz, sqlc.arg(reason)::text,
    sqlc.arg(amount)::bigint, sqlc.arg(lines)::jsonb, sqlc.arg(charges)::jsonb, sqlc.arg(forgiven)::bigint, sqlc.arg(now)::timestamptz)
RETURNING *;

-- name: ListForgivenUsageReceipts :many
-- The merchant's receipts with a line that forgave usage or took forgiveness
-- back, cut after one instant and through another.
SELECT * FROM billing.usage_receipts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND jsonb_path_exists(lines, '$[*] ? (@.forgiven != 0)')
  AND cut_at > sqlc.arg(cut_after)::timestamptz AND cut_at <= sqlc.arg(cut_through)::timestamptz
ORDER BY cut_at, id;

-- name: SumPeriodForgiveness :many
-- What a customer's receipts in a currency forgave, net, of each of some
-- pricing periods: every receipt that took usage of a period was cut at or
-- after its start.
SELECT (l.line ->> 'period_starts_at')::timestamptz AS period_starts_at, sum((l.line ->> 'forgiven')::bigint)::bigint AS forgiven
FROM billing.usage_receipts r
CROSS JOIN LATERAL jsonb_array_elements(r.lines) AS l(line)
WHERE r.merchant_id = sqlc.arg(merchant_id)::uuid AND r.customer_id = sqlc.arg(customer_id)::uuid AND r.currency = sqlc.arg(currency)::text
  AND r.cut_at >= sqlc.arg(cut_from)::timestamptz
  AND (l.line ->> 'period_starts_at')::timestamptz = ANY(sqlc.arg(periods)::timestamptz[])
GROUP BY 1;

-- name: ListUsageReceiptsByIDs :many
SELECT * FROM billing.usage_receipts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND id = ANY(sqlc.arg(ids)::uuid[]);

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
-- One merchant's customers whose usage no posting has taken.
SELECT customer_id, currency FROM billing.usage_pending
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY customer_id, currency;
