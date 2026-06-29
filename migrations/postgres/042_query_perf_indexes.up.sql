-- #629: index the hot ORDER BY ... LIMIT 1 access path the query-perf harness
-- confirmed was doing an avoidable Sort at scale (idx on the equality column
-- only, then a Sort to satisfy the ORDER BY). The composite index makes the
-- LIMIT 1 an index-ordered seek that stops on the first row — no Sort node.

-- GetActiveSubscriptionByCustomerAt: WHERE customer_id=? AND status='active'
-- ... ORDER BY created_at DESC LIMIT 1. Partial on the active state (the only one
-- the query reads), ordered by created_at DESC so the newest active sub is the
-- first index entry for the customer. Replaces the old idx_subscriptions_customer
-- scan + Sort for this path (that index stays for non-ordered customer lookups).
-- A customer's active-sub count grows with the merchant's product catalog, so the
-- fan-out (and thus the Sort it removes) is unbounded — worth the index.
CREATE INDEX idx_subscriptions_customer_active_created
    ON openrails.subscriptions (customer_id, created_at DESC)
    WHERE status = 'active'::openrails.subscription_status;

-- NOTE (documented, NOT indexed): GetLatestChargeBySubscriptionID also does an
-- ORDER BY purchased_at DESC LIMIT 1 over idx_payments_subscription_id + a Sort.
-- A (subscription_id, purchased_at DESC) index was prototyped but the planner
-- declined it: a subscription's charge count is bounded by its billing periods
-- (~one per cycle), so the top-N Sort of that small set is cheaper than the
-- index-ordered scan + filter. No clear win — left unindexed on purpose (#629).
