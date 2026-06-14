--
-- Drop openrails.money_balances (#491, slice 3). It was a READ-CACHE roll-up of
-- balance + held_balance keyed (merchant, customer, currency). Both are now
-- DERIVED from the source of truth:
--   available = SUM(money_blocks.remaining_amount) over unexpired, unspent lots
--   held      = SUM(active-hold authorized_amount) + open-window unsettled
-- so the cache (and its drift/anomaly reconciliation) is gone. The per-customer
-- spend mutex moved from LockMoneyBalance to a FOR UPDATE on the customers row.
--
-- An index on money_blocks(merchant_id, customer_id, currency) WHERE
-- remaining_amount > 0 keeps the derived-available SUM cheap.
--
-- Idempotent: guarded DROP, IF NOT EXISTS index. Fresh DB (001 baseline no
-- longer creates the table) -> the DROP is a no-op.
--
SET statement_timeout = '300s';
SET lock_timeout = '10s';

DROP TABLE IF EXISTS openrails.money_balances;

-- Partial index for the derived-available aggregate (spendable lots only).
CREATE INDEX IF NOT EXISTS idx_money_blocks_spendable
    ON openrails.money_blocks (merchant_id, customer_id, currency)
    WHERE remaining_amount > 0;
