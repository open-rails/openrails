SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DROP FUNCTION IF EXISTS openrails.delinquency_work_merchant_ids(timestamptz, int);

DROP TABLE IF EXISTS openrails.host_lifecycle_events;

DROP TABLE IF EXISTS openrails.customer_delinquency;
