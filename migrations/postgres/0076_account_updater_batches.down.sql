SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DROP FUNCTION IF EXISTS openrails.account_updater_open_batch_merchant_ids(int);
DROP FUNCTION IF EXISTS openrails.account_updater_work_merchant_ids(text, text, timestamptz, int, uuid, int);

DROP TABLE IF EXISTS openrails.account_updater_batches;

DROP INDEX IF EXISTS openrails.ix_subscriptions_renewal_by_payment_method;
DROP INDEX IF EXISTS openrails.ix_payment_methods_account_updater_due;

ALTER TABLE openrails.payment_methods
    -- squawk-ignore ban-drop-column
    DROP COLUMN IF EXISTS account_updater_checked_at;
