-- #846: RLS appends `merchant_id = current_setting('app.merchant_id')` to EVERY
-- query on a policy-bearing table. Where the only merchant_id-leading index is
-- PARTIAL, that predicate is unindexed for rows outside the partial predicate
-- and the table seq-scans in production — and because SOME index path usually
-- exists, the miss rarely surfaces as a Seq Scan. Five of the 60 policy-bearing
-- tables had that shape; payment_settlement_events had no merchant_id index at
-- all. TestMerchantIsolationPolicyIsIndexBacked makes the class a build failure.

-- Only index was idx_solana_subscriptions_due ... WHERE status = 'active'.
CREATE INDEX idx_solana_subscriptions_merchant_id
    ON openrails.solana_subscriptions USING btree (merchant_id);

-- Only index was idx_..._active ... WHERE revoked_at IS NULL. The grant columns
-- ride along: they are the table's actual lookup keys
-- (ProductUsageLimitBindingExistsForGrant, RevokeProductUsageLimitBindingsByGrant).
CREATE INDEX idx_product_usage_limit_bindings_grant
    ON openrails.product_usage_limit_bindings USING btree (merchant_id, grant_id, usage_limit_key);

-- Complementary partial pair (customer_id IS NULL / IS NOT NULL): the union
-- covers every row, but the planner cannot use two partial indexes as one for an
-- unqualified RLS predicate.
CREATE INDEX idx_payer_spend_limits_merchant_id
    ON openrails.payer_spend_limits USING btree (merchant_id);

CREATE INDEX idx_tier_schedules_merchant_id
    ON openrails.tier_schedules USING btree (merchant_id);

-- Got RLS in 0010 but no merchant_id index at all; (merchant_id, id) also orders
-- the pending-delivery drain.
CREATE INDEX idx_payment_settlement_events_merchant_id
    ON openrails.payment_settlement_events USING btree (merchant_id, id);

-- Lookup keys with no index at all. Partial where the column is mostly NULL —
-- safe, because each of these tables already has an unconditional merchant_id
-- index carrying the RLS predicate.
CREATE INDEX idx_grants_payment_id
    ON openrails.grants USING btree (merchant_id, payment_id) WHERE (payment_id IS NOT NULL);

CREATE INDEX idx_checkout_sessions_payment_id
    ON openrails.checkout_sessions USING btree (merchant_id, payment_id) WHERE (payment_id IS NOT NULL);

CREATE INDEX idx_checkout_sessions_subscription_id
    ON openrails.checkout_sessions USING btree (merchant_id, subscription_id) WHERE (subscription_id IS NOT NULL);

CREATE INDEX idx_reprice_batches_price_key
    ON openrails.reprice_batches USING btree (merchant_id, price_key, created_at DESC) WHERE (price_key IS NOT NULL);
