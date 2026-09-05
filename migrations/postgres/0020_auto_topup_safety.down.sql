SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '60s';

DROP TABLE openrails.auto_topup_episodes;
ALTER TABLE openrails.money_settings DROP COLUMN auto_topup_failures;
