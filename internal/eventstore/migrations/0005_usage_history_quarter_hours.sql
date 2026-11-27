-- The per-minute spend rollup goes, and with it its only feed, intake's
-- provisional amount: the low-balance burn rate reads usage history.
DROP VIEW IF EXISTS {database}.usage_minutes_mv;
DROP TABLE IF EXISTS {database}.usage_minutes;
ALTER TABLE {database}.usage_intake DROP COLUMN IF EXISTS amount;

-- A finished limit window's counts go a day after it ends: nothing reads a
-- window once it is over, and usage settled after its end counts toward it
-- no more; the day covers clock skew and syncs in flight. A window without
-- an end (a lifetime or rate limit) stays.
ALTER TABLE {database}.limit_usage MODIFY TTL toDateTime(ifNull(window_ends_at, window_starts_at)) + INTERVAL 1 DAY DELETE WHERE window_ends_at IS NOT NULL;

-- Each meter charge keeps the price intake took it at (never below zero) and
-- the discount a volume tier's fall gave there, so amounts less discounts
-- sum to the period's price. Raw usage keeps them for reads by invoker;
-- events stored before carry zeros.
ALTER TABLE {database}.usage_intake
    ADD COLUMN IF NOT EXISTS `charges.amount` Array(Int64) AFTER `charges.stated`,
    ADD COLUMN IF NOT EXISTS `charges.discount` Array(Int64) AFTER `charges.amount`;
ALTER TABLE {database}.usage_events
    ADD COLUMN IF NOT EXISTS `charges.amount` Array(Int64) CODEC(ZSTD(1)) AFTER `charges.stated`,
    ADD COLUMN IF NOT EXISTS `charges.discount` Array(Int64) CODEC(ZSTD(1)) AFTER `charges.amount`;

DROP VIEW IF EXISTS {database}.usage_events_mv;
CREATE MATERIALIZED VIEW IF NOT EXISTS {database}.usage_events_mv TO {database}.usage_events AS
SELECT merchant_id, event_id, customer_id, currency, event_type, invoker, invoker_type, outcome, source, source_id, lease_id, n,
       occurred_at, admitted_at, ingested_at, period_starts_at, dimensions, metadata, stated, charged, forgiven,
       charges.meter, charges.cell, charges.print, charges.quantity, charges.stated, charges.amount, charges.discount
FROM {database}.usage_intake;

-- Usage history is one row per UTC quarter hour, merchant, customer,
-- currency, event type and meter cell, kept indefinitely: every zone's offset
-- is a whole number of quarter hours, so its hours, days, weeks and months
-- are exact when read. A row with no meter counts a bucket's events
-- (requests; failed_requests, failed_amount and forgiven of the failed
-- ones); a meter cell's row counts what they took of it (requests, quantity,
-- stated, amount at intake's price and discount). The invoker is not kept:
-- reads by invoker read raw usage (90 days). A customer's rows sort by UTC
-- day, then by what they count, then by bucket. A merchant purge deletes its
-- rows.
DROP VIEW IF EXISTS {database}.usage_history_events_mv;
DROP VIEW IF EXISTS {database}.usage_history_charges_mv;
DROP TABLE IF EXISTS {database}.usage_history;

CREATE TABLE IF NOT EXISTS {database}.usage_history (
    merchant_id UUID,
    customer_id UUID,
    bucket_at DateTime('UTC'),
    currency LowCardinality(String),
    event_type LowCardinality(String),
    meter LowCardinality(String),
    cell String,
    requests Int64,
    quantity Int64,
    stated Int64,
    amount Int64,
    discount Int64,
    failed_requests Int64,
    failed_amount Int64,
    forgiven Int64
) ENGINE = SummingMergeTree((requests, quantity, stated, amount, discount, failed_requests, failed_amount, forgiven))
PARTITION BY toYYYYMM(bucket_at)
ORDER BY (merchant_id, customer_id, toDate(bucket_at), currency, event_type, meter, cell, bucket_at)
SETTINGS index_granularity = 1024;

CREATE MATERIALIZED VIEW IF NOT EXISTS {database}.usage_history_events_mv TO {database}.usage_history AS
SELECT merchant_id, customer_id, toStartOfFifteenMinutes(toDateTime(occurred_at, 'UTC')) AS bucket_at, currency, event_type, '' AS meter, '' AS cell,
       toInt64(1) AS requests, toInt64(0) AS quantity, ifNull(stated, 0) AS stated, toInt64(0) AS amount, toInt64(0) AS discount,
       toInt64(outcome = 'failed') AS failed_requests, charged AS failed_amount, forgiven
FROM {database}.usage_intake;

CREATE MATERIALIZED VIEW IF NOT EXISTS {database}.usage_history_charges_mv TO {database}.usage_history AS
SELECT merchant_id, customer_id, toStartOfFifteenMinutes(toDateTime(occurred_at, 'UTC')) AS bucket_at, currency, event_type, charges.meter AS meter,
       charges.cell AS cell, toInt64(1) AS requests, charges.quantity AS quantity, charges.stated AS stated, charges.amount AS amount,
       charges.discount AS discount, toInt64(0) AS failed_requests, toInt64(0) AS failed_amount, toInt64(0) AS forgiven
FROM {database}.usage_intake
ARRAY JOIN charges;

-- The history before is rebuilt from the raw usage the store still holds
-- (90 days); older buckets are not carried over, and usage stored before
-- this migration has no amounts (intake did not keep its prices).
INSERT INTO {database}.usage_history (merchant_id, customer_id, bucket_at, currency, event_type, meter, cell, requests, quantity, stated,
    amount, discount, failed_requests, failed_amount, forgiven)
SELECT merchant_id, customer_id, toStartOfFifteenMinutes(toDateTime(occurred_at, 'UTC')), currency, event_type, '', '', 1, 0, ifNull(stated, 0),
       0, 0, toInt64(outcome = 'failed'), charged, forgiven
FROM {database}.usage_events FINAL;

INSERT INTO {database}.usage_history (merchant_id, customer_id, bucket_at, currency, event_type, meter, cell, requests, quantity, stated,
    amount, discount, failed_requests, failed_amount, forgiven)
SELECT merchant_id, customer_id, toStartOfFifteenMinutes(toDateTime(occurred_at, 'UTC')), currency, event_type, charges.meter, charges.cell,
       1, charges.quantity, charges.stated, charges.amount, charges.discount, 0, 0, 0
FROM {database}.usage_events FINAL
ARRAY JOIN charges;
