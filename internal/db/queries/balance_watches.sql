-- billing.balance_watches: when each watched balance (every prepaid one, every
-- one with a running charge and every one holding a credit lot that lapses)
-- is next looked at, and what the last look found. Every instant is the
-- caller's clock.

-- name: UpsertBalanceWatch :exec
-- A running charge started or stopped: when to look next. A watch already due
-- stays due; the notices of a crossing and the usage rate stay.
INSERT INTO billing.balance_watches (merchant_id, customer_id, currency, next_check_at, depletes_at, usage_rate_per_hour, updated_at,
                                     low_line_amount, floor_amount, look_each_change)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text,
        sqlc.narg(next_check_at)::timestamptz, sqlc.narg(depletes_at)::timestamptz, 0, sqlc.arg(now)::timestamptz,
        sqlc.narg(low_line_amount)::bigint, sqlc.narg(floor_amount)::bigint, sqlc.arg(look_each_change)::boolean)
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE SET
    next_check_at = CASE WHEN billing.balance_watches.next_check_at <= EXCLUDED.updated_at
                         THEN billing.balance_watches.next_check_at ELSE EXCLUDED.next_check_at END,
    depletes_at = EXCLUDED.depletes_at, updated_at = EXCLUDED.updated_at, low_line_amount = EXCLUDED.low_line_amount,
    floor_amount = EXCLUDED.floor_amount, look_each_change = EXCLUDED.look_each_change;

-- name: InsertPrepaidBalanceWatch :exec
-- A new balance account is watched unless the customer is billed in arrears:
-- nothing is scheduled until a change marks it due.
INSERT INTO billing.balance_watches (merchant_id, customer_id, currency, next_check_at, usage_rate_per_hour, look_each_change)
SELECT sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text, NULL::timestamptz, 0, true
WHERE NOT EXISTS (
    SELECT 1 FROM billing.money_settings s
    WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.customer_id = sqlc.arg(customer_id)::uuid
      AND s.currency = sqlc.arg(currency)::text AND s.billing_mode = 'arrears' AND s.collection_revoked_at IS NULL)
ON CONFLICT (merchant_id, customer_id, currency) DO NOTHING;

-- name: WatchLapsingCredit :execrows
-- A credit lot that lapses was deposited: its balance is watched, an arrears
-- one too, and each change of it is looked at until a look schedules the
-- lapse. The caller queues that look.
INSERT INTO billing.balance_watches (merchant_id, customer_id, currency, next_check_at, usage_rate_per_hour, updated_at, look_each_change)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text, to_timestamp(0), 0, sqlc.arg(now)::timestamptz, true)
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE SET
    next_check_at = to_timestamp(0), look_each_change = true, updated_at = EXCLUDED.updated_at;

-- name: WatchCustomerBalance :execrows
-- The customer's billing mode, credit line or low line changed: its balance,
-- if it has one, is marked due for a look (the caller queues it), which drops
-- the watch of an arrears customer with no running charge. rearm_low clears
-- the low notice; funded re-arms both, as a funding does.
INSERT INTO billing.balance_watches (merchant_id, customer_id, currency, next_check_at, usage_rate_per_hour, updated_at, look_each_change)
SELECT a.merchant_id, a.customer_id::uuid, a.currency, to_timestamp(0), 0, sqlc.arg(now)::timestamptz, true
FROM billing.ledger_accounts a
WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.account_type = 'customer_balance'
  AND a.currency = sqlc.arg(currency)::text AND a.customer_id = sqlc.arg(customer_id)::uuid
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE SET
    next_check_at = to_timestamp(0),
    low_notified_at = CASE WHEN sqlc.arg(rearm_low)::boolean THEN NULL ELSE billing.balance_watches.low_notified_at END,
    funded_at = CASE WHEN sqlc.arg(funded)::boolean THEN COALESCE(billing.balance_watches.funded_at, EXCLUDED.updated_at)
                     ELSE billing.balance_watches.funded_at END,
    updated_at = EXCLUDED.updated_at;

-- name: DeleteBalanceWatch :exec
DELETE FROM billing.balance_watches
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text;

-- name: GetBalanceWatch :one
SELECT * FROM billing.balance_watches
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text;

-- name: RebuildBalanceWatches :exec
-- A restored merchant's balances are watched again: one with a running charge
-- or a credit lot not yet expired that lapses is due at once, a prepaid one
-- when a change marks it due.
INSERT INTO billing.balance_watches (merchant_id, customer_id, currency, next_check_at, usage_rate_per_hour, updated_at, look_each_change)
SELECT w.merchant_id, w.customer_id, w.currency,
       CASE WHEN bool_or(w.due) THEN sqlc.arg(now)::timestamptz END, 0, sqlc.arg(now)::timestamptz, true
FROM (
    SELECT rc.merchant_id, rc.customer_id, rc.currency, true AS due
    FROM billing.running_charges rc
    WHERE rc.merchant_id = sqlc.arg(merchant_id)::uuid AND rc.stopped_at IS NULL
    UNION ALL
    SELECT g.merchant_id, g.customer_id, g.currency::text, true
    FROM billing.grants g
    WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid AND g.kind = 'credit' AND g.event = 'grant'
      AND g.ends_at IS NOT NULL AND g.currency IS NOT NULL
      AND NOT EXISTS (SELECT 1 FROM billing.grants t WHERE t.merchant_id = g.merchant_id AND t.supersedes_id = g.id AND t.event IN ('revoke', 'expire', 'supersede'))
      AND NOT EXISTS (SELECT 1 FROM billing.ledger_transfers lt WHERE lt.merchant_id = g.merchant_id AND lt.grant_id = g.id AND lt.transfer_type = 'credit_expire')
    UNION ALL
    SELECT a.merchant_id, a.customer_id::uuid, a.currency, false
    FROM billing.ledger_accounts a
    LEFT JOIN billing.money_settings s
      ON s.merchant_id = a.merchant_id AND s.customer_id = a.customer_id AND s.currency = a.currency
    WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.account_type = 'customer_balance' AND a.customer_id IS NOT NULL
      AND (s.billing_mode IS DISTINCT FROM 'arrears' OR s.collection_revoked_at IS NOT NULL)
) w
GROUP BY w.merchant_id, w.customer_id, w.currency
ON CONFLICT (merchant_id, customer_id, currency) DO NOTHING;

-- name: MarkBalanceWatchDue :execrows
-- A change queued a look at the balance: the watch is due until a look resets
-- it, so the billing pass looks at it if the look fails. A row already due is
-- not written again. Skipping a due row is sound only because every marker
-- holds the customer's LockCustomerForSpend, which the look takes before it
-- reads the balance and resets the row.
UPDATE billing.balance_watches
SET next_check_at = to_timestamp(0), updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text AND next_check_at IS DISTINCT FROM to_timestamp(0);

-- name: StampBalanceWatchFunded :exec
-- A funding since the last notice, which re-arms it.
UPDATE billing.balance_watches SET funded_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text AND funded_at IS NULL;

-- name: ListDueBalanceWatches :many
-- The watches due, across merchants, in key order: an idle pass reads no row.
SELECT merchant_id, customer_id, currency, next_check_at::timestamptz AS next_check_at FROM billing.balance_watches
WHERE next_check_at <= sqlc.arg(now)::timestamptz
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (next_check_at, merchant_id, customer_id, currency)
          > (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_merchant_id)::uuid, sqlc.narg(after_customer_id)::uuid, sqlc.narg(after_currency)::text))
ORDER BY next_check_at, merchant_id, customer_id, currency
LIMIT sqlc.arg(row_limit)::int;

-- name: LockBalanceWatch :one
-- Taken after the customer lock: a mark that races the look waits for it.
SELECT * FROM billing.balance_watches
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
FOR UPDATE;

-- name: SetBalanceWatch :exec
-- What a look found: when to look next, the notices of the crossings it is
-- in, whether a funding is still to re-arm one, the usage rate, and the lines
-- a change is judged against.
UPDATE billing.balance_watches
SET next_check_at = sqlc.narg(next_check_at)::timestamptz, depletes_at = sqlc.narg(depletes_at)::timestamptz,
    low_line_amount = sqlc.narg(low_line_amount)::bigint, floor_amount = sqlc.narg(floor_amount)::bigint,
    look_each_change = sqlc.arg(look_each_change)::boolean,
    low_notified_at = sqlc.narg(low_notified_at)::timestamptz, depleted_at = sqlc.narg(depleted_at)::timestamptz,
    funded_at = sqlc.narg(funded_at)::timestamptz, usage_rate_per_hour = sqlc.arg(usage_rate_per_hour)::bigint,
    rate_sampled_at = sqlc.narg(rate_sampled_at)::timestamptz, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text;

-- name: BalanceWatchDueBefore :one
-- CROSS-MERCHANT: whether a watch has been due since before the instant; one
-- marked due counts from when it was marked.
SELECT EXISTS (
    SELECT 1 FROM billing.balance_watches
    WHERE next_check_at <= sqlc.arg(before)::timestamptz
      AND (next_check_at > to_timestamp(0) OR updated_at <= sqlc.arg(before)::timestamptz)
)::boolean AS due;
