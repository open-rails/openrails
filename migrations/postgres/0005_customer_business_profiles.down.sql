-- Remove the or#908 business-profile record. Going back, business posture has
-- no first-class representation again (hosts hand-roll their own tables).

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DROP TABLE openrails.customer_business_profiles;
