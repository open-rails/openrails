-- Revert or#914 item 5: drop the dormant-merchant warning ledger. Going back,
-- hosts run dormancy entirely in their own tables (th#1774 shape).

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DROP TABLE openrails.merchant_dormancy_notices;
