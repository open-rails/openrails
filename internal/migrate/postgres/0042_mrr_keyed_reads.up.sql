-- The MRR writers read by key: a subscription's row, a customer's rows, the
-- dirty rows. A session caches their plans, and one made while
-- subscription_mrr was small scans it whole, or reads all of a merchant's
-- entries of an index, so each row of a bulk import or backfill read every
-- row written before it. These functions plan each read for the table as it
-- is, without sequential scans: each read stays a keyed index lookup, and a
-- write's work grows with what it changed. A later CREATE OR REPLACE of one
-- of them restates both settings.
ALTER FUNCTION billing.refresh_subscription_mrr(billing.subscriptions, timestamptz, text) SET enable_seqscan TO off;
ALTER FUNCTION billing.refresh_subscription_mrr(billing.subscriptions, timestamptz, text) SET plan_cache_mode TO force_custom_plan;
ALTER FUNCTION billing.schedule_mrr_change(uuid, uuid, timestamptz) SET enable_seqscan TO off;
ALTER FUNCTION billing.schedule_mrr_change(uuid, uuid, timestamptz) SET plan_cache_mode TO force_custom_plan;
ALTER FUNCTION billing.pending_mrr_lines(uuid, uuid, text) SET enable_seqscan TO off;
ALTER FUNCTION billing.pending_mrr_lines(uuid, uuid, text) SET plan_cache_mode TO force_custom_plan;
ALTER FUNCTION billing.settle_mrr(uuid) SET enable_seqscan TO off;
ALTER FUNCTION billing.settle_mrr(uuid) SET plan_cache_mode TO force_custom_plan;
ALTER FUNCTION billing.settle_at_commit() SET enable_seqscan TO off;
ALTER FUNCTION billing.settle_at_commit() SET plan_cache_mode TO force_custom_plan;
