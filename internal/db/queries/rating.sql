-- Rating at intake: the rate cards an event type is priced by, and the
-- period anchor.

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
