-- Rating at ingest: the rate cards an event type is priced by, each
-- customer's per-period meter quantities, and the period anchor.

-- Every meter of an event type that has a rate card, with the card.
-- name: ListUsageRatingCards :many
SELECT cm.key AS meter_key,
       COALESCE(NULLIF(cm.value_property, ''), cm.key)::text AS value_property,
       COALESCE(NULLIF(cm.aggregation, ''), 'sum')::text AS aggregation,
       cm.group_by,
       rc.filter,
       rc.allowance,
       rc.price
FROM billing.catalog_meters cm
JOIN billing.catalog_rate_cards rc
  ON rc.merchant_id = cm.merchant_id AND rc.meter_key = cm.key
WHERE cm.merchant_id = sqlc.arg(merchant_id)::uuid
  AND COALESCE(NULLIF(cm.event_type, ''), cm.key) = sqlc.arg(event_type)::text
ORDER BY cm.key;

-- Adds quantity to a meter's period quantity and answers the new total.
-- name: AddMeterPeriodQuantity :one
INSERT INTO billing.meter_period_quantities (
    merchant_id, customer_id, currency, meter_key, cell, period_starts_at, quantity, created_at, updated_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text, sqlc.arg(meter_key)::text,
    sqlc.arg(cell)::text, sqlc.arg(period_starts_at)::timestamptz, sqlc.arg(quantity)::bigint,
    sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz
)
ON CONFLICT (merchant_id, customer_id, currency, meter_key, cell, period_starts_at)
DO UPDATE SET quantity = billing.meter_period_quantities.quantity + EXCLUDED.quantity, updated_at = EXCLUDED.updated_at
RETURNING quantity;

-- name: GetMeterPeriodQuantity :one
SELECT COALESCE((
    SELECT quantity FROM billing.meter_period_quantities
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
      AND currency = sqlc.arg(currency)::text AND meter_key = sqlc.arg(meter_key)::text
      AND cell = sqlc.arg(cell)::text AND period_starts_at = sqlc.arg(period_starts_at)::timestamptz
), 0)::bigint AS quantity;

-- Periods past the ingest window go a bounded batch at a time, on the next
-- write; a row another writer holds waits for a later one.
-- name: PruneMeterPeriodQuantities :execrows
DELETE FROM billing.meter_period_quantities q
WHERE (q.merchant_id, q.customer_id, q.currency, q.meter_key, q.cell, q.period_starts_at) IN (
    SELECT e.merchant_id, e.customer_id, e.currency, e.meter_key, e.cell, e.period_starts_at
    FROM billing.meter_period_quantities e
    WHERE e.merchant_id = sqlc.arg(merchant_id)::uuid AND e.period_starts_at < sqlc.arg(before)::timestamptz
    LIMIT 100
    FOR UPDATE SKIP LOCKED);

-- A customer's first recorded activity in a currency: its first transfer or
-- the opening of its balance account. Anniversary periods are anchored on it.
-- name: GetCustomerPeriodAnchor :one
SELECT LEAST(
    (SELECT MIN(lt.created_at) FROM billing.ledger_transfers lt
     WHERE lt.merchant_id = sqlc.arg(merchant_id)::uuid AND lt.customer_id = sqlc.arg(customer_id)::uuid
       AND lt.currency = sqlc.arg(currency)::text),
    (SELECT a.created_at FROM billing.ledger_accounts a
     WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.customer_id = sqlc.arg(customer_id)::uuid
       AND a.currency = sqlc.arg(currency)::text AND a.account_type = 'customer_balance'))::timestamptz AS period_anchor;
