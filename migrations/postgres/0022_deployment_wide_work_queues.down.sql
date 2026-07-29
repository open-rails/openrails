-- Reverting re-inerts the provider-intent plane, the credit-lot expiry sweep,
-- the #816 re-driver and the fleet dashboard. Only do this alongside reverting
-- the callers.
DROP INDEX IF EXISTS openrails.idx_subscription_reprices_blocked_plan_change;
DROP FUNCTION IF EXISTS openrails.fleet_weekly_volume(uuid, timestamptz);
DROP FUNCTION IF EXISTS openrails.fleet_weekly_cancelled_subscriptions(uuid, timestamptz);
DROP FUNCTION IF EXISTS openrails.fleet_weekly_active_merchants(uuid, timestamptz);
DROP FUNCTION IF EXISTS openrails.fleet_mrr_by_currency(uuid);
DROP FUNCTION IF EXISTS openrails.fleet_rail_health(uuid, timestamptz);
DROP FUNCTION IF EXISTS openrails.fleet_revenue_by_currency(uuid, timestamptz);
DROP FUNCTION IF EXISTS openrails.fleet_merchant_funnel(uuid, timestamptz);
DROP FUNCTION IF EXISTS openrails.redrivable_plan_change_merchant_ids(int);
DROP FUNCTION IF EXISTS openrails.lapsed_credit_lot_merchant_ids(timestamptz, int);
DROP FUNCTION IF EXISTS openrails.due_verify_rail_intent_merchant_ids(timestamptz, int);
DROP FUNCTION IF EXISTS openrails.due_rail_intent_merchant_ids(timestamptz, int);
