-- Raw usage is kept 90 days, the owner's choice: a period's last posting
-- reads its events up to 66 days after they occurred (a period of at most
-- 31 days, its 35-day late window, the next month end), with room to spare.
ALTER TABLE {database}.usage_events MODIFY TTL toDateTime(occurred_at) + INTERVAL 90 DAY;

-- Usage history is one row per UTC hour, kept indefinitely: the only
-- history. Readers group the hours into the days, weeks and months of the
-- merchant's accounting zone when they read, so a zone change regroups all
-- of it; a zone whose offset is not a whole number of hours has its days cut
-- at the hour. Postings never read it. An erasure folds an invoker's rows
-- into '' by inserting their negation; a merchant purge deletes its rows.
DROP VIEW IF EXISTS {database}.usage_history_events_mv;
DROP VIEW IF EXISTS {database}.usage_history_charges_mv;
DROP TABLE IF EXISTS {database}.usage_history;

ALTER TABLE {database}.usage_intake DROP COLUMN IF EXISTS day_at;

CREATE TABLE IF NOT EXISTS {database}.usage_history (
    merchant_id UUID,
    customer_id UUID,
    bucket_at DateTime('UTC'),
    currency LowCardinality(String),
    event_type LowCardinality(String),
    meter LowCardinality(String),
    cell String,
    print LowCardinality(String),
    period_starts_at DateTime64(6, 'UTC'),
    invoker String,
    outcome Enum8('succeeded' = 1, 'failed' = 2),
    requests Int64,
    quantity Int64,
    stated Int64,
    charged Int64,
    forgiven Int64
) ENGINE = SummingMergeTree((requests, quantity, stated, charged, forgiven))
PARTITION BY toYYYYMM(bucket_at)
ORDER BY (merchant_id, customer_id, bucket_at, currency, event_type, meter, cell, print, period_starts_at, invoker, outcome)
SETTINGS index_granularity = 1024;

CREATE MATERIALIZED VIEW IF NOT EXISTS {database}.usage_history_events_mv TO {database}.usage_history AS
SELECT merchant_id, customer_id, toStartOfHour(toDateTime(occurred_at, 'UTC')) AS bucket_at, currency, event_type, '' AS meter, '' AS cell,
       '' AS print, period_starts_at, invoker, outcome, toInt64(1) AS requests, toInt64(0) AS quantity, ifNull(stated, 0) AS stated,
       charged, forgiven
FROM {database}.usage_intake;

CREATE MATERIALIZED VIEW IF NOT EXISTS {database}.usage_history_charges_mv TO {database}.usage_history AS
SELECT merchant_id, customer_id, toStartOfHour(toDateTime(occurred_at, 'UTC')) AS bucket_at, currency, event_type, charges.meter AS meter,
       charges.cell AS cell, charges.print AS print, period_starts_at, invoker, outcome, toInt64(1) AS requests,
       charges.quantity AS quantity, charges.stated AS stated, toInt64(0) AS charged, toInt64(0) AS forgiven
FROM {database}.usage_intake
ARRAY JOIN charges;

-- The history before is rebuilt from the raw usage the store still holds.
INSERT INTO {database}.usage_history (merchant_id, customer_id, bucket_at, currency, event_type, meter, cell, print, period_starts_at,
    invoker, outcome, requests, quantity, stated, charged, forgiven)
SELECT merchant_id, customer_id, toStartOfHour(toDateTime(occurred_at, 'UTC')), currency, event_type, '', '', '', period_starts_at,
       invoker, outcome, 1, 0, ifNull(stated, 0), charged, forgiven
FROM {database}.usage_events FINAL;

INSERT INTO {database}.usage_history (merchant_id, customer_id, bucket_at, currency, event_type, meter, cell, print, period_starts_at,
    invoker, outcome, requests, quantity, stated, charged, forgiven)
SELECT merchant_id, customer_id, toStartOfHour(toDateTime(occurred_at, 'UTC')), currency, event_type, charges.meter, charges.cell,
       charges.print, period_starts_at, invoker, outcome, 1, charges.quantity, charges.stated, 0, 0
FROM {database}.usage_events FINAL
ARRAY JOIN charges;
