-- Usage settles in batches: each customer's daily accrual, its daily rollups
-- and its per-minute spend. Nothing keeps a row per event.

-- name: LockUsageAccrual :one
SELECT * FROM billing.usage_accruals
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text AND day_at = sqlc.arg(day_at)::timestamptz
  AND period_starts_at = sqlc.arg(period_starts_at)::timestamptz
FOR UPDATE;

-- name: InsertUsageAccrual :one
INSERT INTO billing.usage_accruals (merchant_id, customer_id, currency, day_at, period_starts_at, period_ends_at, amount, posted_amount, postings, lines, created_at, updated_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text, sqlc.arg(day_at)::timestamptz,
    sqlc.arg(period_starts_at)::timestamptz, sqlc.arg(period_ends_at)::timestamptz, sqlc.arg(amount)::bigint, 0, 0, sqlc.arg(lines)::jsonb, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
RETURNING *;

-- name: AddToUsageAccrual :exec
UPDATE billing.usage_accruals
SET amount = sqlc.arg(amount)::bigint, lines = sqlc.arg(lines)::jsonb, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: ListUnpostedUsageAccruals :many
-- A customer's accruals in a currency the ledger has not taken in full, in
-- posting order. Accruals change only under the customer lock.
SELECT * FROM billing.usage_accruals
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text AND amount > posted_amount
ORDER BY day_at, period_starts_at, id;

-- name: MarkUsageAccrualPosted :exec
UPDATE billing.usage_accruals
SET posted_amount = amount, postings = postings + 1, lines = sqlc.arg(lines)::jsonb,
    posted_at = sqlc.arg(now)::timestamptz, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: ListDueUsageAccruals :many
-- CROSS-MERCHANT: customers whose accrual of a day before day_at is not posted
-- in full, in due order. An idle pass reads no row.
SELECT DISTINCT a.day_at, a.merchant_id, a.customer_id, a.currency
FROM billing.usage_accruals a
WHERE a.amount > a.posted_amount AND a.day_at < sqlc.arg(day_at)::timestamptz
  AND (sqlc.narg(after_day_at)::timestamptz IS NULL
       OR (a.day_at, a.merchant_id, a.customer_id, a.currency) > (sqlc.narg(after_day_at)::timestamptz, sqlc.narg(after_merchant_id)::uuid, sqlc.narg(after_customer_id)::uuid, sqlc.narg(after_currency)::text))
ORDER BY a.day_at, a.merchant_id, a.customer_id, a.currency
LIMIT sqlc.arg(row_limit)::int;

-- name: ListUsageAccrualsByIDs :many
SELECT * FROM billing.usage_accruals
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: GetUsageAccrual :one
SELECT * FROM billing.usage_accruals
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text AND day_at = sqlc.arg(day_at)::timestamptz
  AND period_starts_at = sqlc.arg(period_starts_at)::timestamptz;

-- name: GetFirstPostingSince :one
-- The first ledger leg posted at a source since an instant.
SELECT id FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text AND transfer_type IN ('credit_spend', 'owed_accrual')
  AND operation = sqlc.arg(operation)::text AND source = sqlc.arg(source)::text
  AND created_at >= sqlc.arg(since)::timestamptz
ORDER BY created_at, id
LIMIT 1;

-- name: UpsertUsageDaily :exec
-- Adds one sync's rollups; the caller sorts them by key, so concurrent
-- settlements lock rows in one order.
INSERT INTO billing.usage_daily AS d (merchant_id, customer_id, day_at, currency, event_type, meter_key, cell, invoker, outcome,
    requests, quantity, amount, forgiven_amount, created_at, updated_at)
SELECT sqlc.arg(merchant_id)::uuid, r.customer_id, r.day_at, r.currency, r.event_type, r.meter_key, r.cell, r.invoker, r.outcome,
    r.requests, r.quantity, r.amount, r.forgiven_amount, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz
FROM unnest(sqlc.arg(customer_ids)::uuid[], sqlc.arg(day_ats)::timestamptz[], sqlc.arg(currencies)::text[], sqlc.arg(event_types)::text[],
    sqlc.arg(meter_keys)::text[], sqlc.arg(cells)::text[], sqlc.arg(invokers)::text[], sqlc.arg(outcomes)::text[],
    sqlc.arg(requests)::bigint[], sqlc.arg(quantities)::bigint[], sqlc.arg(amounts)::bigint[], sqlc.arg(forgiven_amounts)::bigint[])
    AS r(customer_id, day_at, currency, event_type, meter_key, cell, invoker, outcome, requests, quantity, amount, forgiven_amount)
ON CONFLICT (merchant_id, customer_id, day_at, currency, event_type, meter_key, cell, invoker, outcome) DO UPDATE SET
    requests = d.requests + EXCLUDED.requests, quantity = d.quantity + EXCLUDED.quantity, amount = d.amount + EXCLUDED.amount,
    forgiven_amount = d.forgiven_amount + EXCLUDED.forgiven_amount, updated_at = EXCLUDED.updated_at;

-- name: UpsertUsageMinutes :exec
-- Adds one sync's per-minute spend, sorted by key as UpsertUsageDaily.
INSERT INTO billing.usage_minutes AS m (merchant_id, customer_id, currency, minute_at, amount)
SELECT sqlc.arg(merchant_id)::uuid, r.customer_id, r.currency, r.minute_at, r.amount
FROM unnest(sqlc.arg(customer_ids)::uuid[], sqlc.arg(currencies)::text[], sqlc.arg(minutes)::timestamptz[], sqlc.arg(amounts)::bigint[])
    AS r(customer_id, currency, minute_at, amount)
ON CONFLICT (merchant_id, customer_id, currency, minute_at) DO UPDATE SET amount = m.amount + EXCLUDED.amount;

-- name: SumUsageMinutesSince :one
-- accrual_rate_cap: what the customer's usage charged in the minutes from
-- since's, for the measured accrual rate.
SELECT COALESCE(SUM(amount), 0)::bigint AS total_amount
FROM billing.usage_minutes
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text AND minute_at >= date_trunc('minute', sqlc.arg(since)::timestamptz);

-- name: SumUsageMinutesBetween :one
-- A balance watch's usage rate: what the customer's usage charged in the
-- minutes from since's up to until's, so consecutive looks read each minute
-- once.
SELECT COALESCE(SUM(amount), 0)::bigint AS total_amount
FROM billing.usage_minutes
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND minute_at >= date_trunc('minute', sqlc.arg(since)::timestamptz) AND minute_at < date_trunc('minute', sqlc.arg(until)::timestamptz);

-- name: UsageDailyReport :many
-- One page of a customer's usage on the days overlapping [from, to) in one
-- currency, grouped by event_type or invoker: total amount descending, then
-- the key in byte order, after the group (after_amount, after_key). A group
-- counts its events once, on its rows with no meter.
SELECT g.key, g.event_count, g.total_amount
FROM (
    SELECT (CASE sqlc.arg(group_by)::text WHEN 'invoker' THEN d.invoker ELSE d.event_type END)::text AS key,
           COALESCE(SUM(d.requests) FILTER (WHERE d.meter_key = ''), 0)::bigint AS event_count,
           COALESCE(SUM(d.amount), 0)::bigint AS total_amount
    FROM billing.usage_daily d
    WHERE d.merchant_id = sqlc.arg(merchant_id)::uuid AND d.customer_id = sqlc.arg(customer_id)::uuid
      AND d.currency = sqlc.arg(currency)::text
      AND d.day_at >= sqlc.arg(from_day_at)::timestamptz AND d.day_at < sqlc.arg(to_day_at)::timestamptz
    GROUP BY 1
) g
WHERE sqlc.narg(after_amount)::bigint IS NULL
   OR g.total_amount < sqlc.narg(after_amount)::bigint
   OR (g.total_amount = sqlc.narg(after_amount)::bigint AND g.key COLLATE "C" > sqlc.narg(after_key)::text)
ORDER BY g.total_amount DESC, g.key COLLATE "C"
LIMIT sqlc.arg(row_limit)::int;

-- name: UsageDailyReportByMeter :many
-- One page of a customer's usage on the days overlapping [from, to) in one
-- currency, grouped by what each meter (and matrix cell) charged it: the key
-- is the meter key and the cell joined by U+001F. Ordered as UsageDailyReport.
SELECT g.key, g.event_count, g.total_amount, g.quantity
FROM (
    SELECT (d.meter_key || chr(31) || d.cell)::text AS key,
           COALESCE(SUM(d.requests), 0)::bigint AS event_count,
           COALESCE(SUM(d.amount), 0)::bigint AS total_amount,
           COALESCE(SUM(d.quantity), 0)::bigint AS quantity
    FROM billing.usage_daily d
    WHERE d.merchant_id = sqlc.arg(merchant_id)::uuid AND d.customer_id = sqlc.arg(customer_id)::uuid
      AND d.currency = sqlc.arg(currency)::text AND d.meter_key <> ''
      AND d.day_at >= sqlc.arg(from_day_at)::timestamptz AND d.day_at < sqlc.arg(to_day_at)::timestamptz
    GROUP BY 1
) g
WHERE sqlc.narg(after_amount)::bigint IS NULL
   OR g.total_amount < sqlc.narg(after_amount)::bigint
   OR (g.total_amount = sqlc.narg(after_amount)::bigint AND g.key COLLATE "C" > sqlc.narg(after_key)::text)
ORDER BY g.total_amount DESC, g.key COLLATE "C"
LIMIT sqlc.arg(row_limit)::int;

-- name: LockUsageEventTypes :exec
-- A meter correction's fence on its merchant's usage of event types: held
-- exclusively against ShareUsageEventTypes, which every settlement takes
-- before it adds rollups. No other merchant or event type waits.
SELECT pg_advisory_xact_lock(hashtextextended('openrails.usage_activity:' || sqlc.arg(merchant_id)::uuid::text || ':' || e.event_type, 0))
FROM (SELECT DISTINCT unnest(sqlc.arg(event_types)::text[]) AS event_type ORDER BY 1) e;

-- name: ShareUsageEventTypes :exec
-- A settlement's fence on the event types it adds rollups for, in the order
-- LockUsageEventTypes takes them.
SELECT pg_advisory_xact_lock_shared(hashtextextended('openrails.usage_activity:' || sqlc.arg(merchant_id)::uuid::text || ':' || e.event_type, 0))
FROM (SELECT DISTINCT unnest(sqlc.arg(event_types)::text[]) AS event_type ORDER BY 1) e;

-- name: UsageDailyExistsForTypes :one
SELECT EXISTS (
    SELECT 1
    FROM billing.usage_daily
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND event_type = ANY(sqlc.arg(event_types)::text[])
);

-- name: SumUnpostedUsageAccruals :one
SELECT COALESCE(SUM(amount - posted_amount), 0)::bigint AS unposted
FROM billing.usage_accruals
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text AND amount > posted_amount;

-- name: ForgiveUsageDaily :exec
-- Moves amount of one meter cell's charged usage on a day to forgiven, from
-- its rows in key order: what a posting could not draw of a merchant's unit.
WITH r AS (
    SELECT d.event_type, d.invoker, d.outcome,
           LEAST(d.amount, GREATEST(sqlc.arg(amount)::bigint - (SUM(d.amount) OVER (ORDER BY d.event_type, d.invoker, d.outcome) - d.amount), 0)) AS take
    FROM billing.usage_daily d
    WHERE d.merchant_id = sqlc.arg(merchant_id)::uuid AND d.customer_id = sqlc.arg(customer_id)::uuid AND d.day_at = sqlc.arg(day_at)::timestamptz
      AND d.currency = sqlc.arg(currency)::text AND d.meter_key = sqlc.arg(meter_key)::text AND d.cell = sqlc.arg(cell)::text AND d.amount > 0
)
UPDATE billing.usage_daily d
SET amount = d.amount - r.take, forgiven_amount = d.forgiven_amount + r.take, updated_at = sqlc.arg(now)::timestamptz
FROM r
WHERE d.merchant_id = sqlc.arg(merchant_id)::uuid AND d.customer_id = sqlc.arg(customer_id)::uuid AND d.day_at = sqlc.arg(day_at)::timestamptz
  AND d.currency = sqlc.arg(currency)::text AND d.meter_key = sqlc.arg(meter_key)::text AND d.cell = sqlc.arg(cell)::text
  AND d.event_type = r.event_type AND d.invoker = r.invoker AND d.outcome = r.outcome AND r.take > 0;
