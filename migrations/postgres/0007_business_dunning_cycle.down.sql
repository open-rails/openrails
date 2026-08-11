-- Remove the or#910 dunning-cycle state. Going back, hosts run their own
-- dunning ladders again (tensorhub's business/runcycle.go shape).

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DROP FUNCTION openrails.business_cycle_work_merchant_ids(integer);

ALTER TABLE openrails.customer_business_profiles
    DROP COLUMN suspension_recommended_at,
    DROP COLUMN suspension_reason;
