-- Usage is inserted into usage_intake, which stores nothing: its views write
-- the raw event and every rollup. A raw event keeps what was reported, its
-- identity and what postings, resends and erasure read; the provisional
-- amount and the limit windows feed limit_usage and usage_minutes only.
-- day_at is the start of the item's day in the merchant's accounting zone,
-- the day its history folds into.
CREATE TABLE IF NOT EXISTS {database}.usage_intake (
    merchant_id UUID,
    event_id UUID,
    customer_id UUID,
    currency LowCardinality(String),
    event_type LowCardinality(String),
    invoker String,
    invoker_type LowCardinality(String),
    outcome Enum8('succeeded' = 1, 'failed' = 2),
    source LowCardinality(String),
    source_id String,
    lease_id UUID,
    n UInt64,
    occurred_at DateTime64(6, 'UTC'),
    admitted_at DateTime64(6, 'UTC'),
    ingested_at DateTime64(6, 'UTC'),
    period_starts_at DateTime64(6, 'UTC'),
    day_at DateTime('UTC'),
    dimensions Map(LowCardinality(String), Int64),
    metadata String,
    amount Int64,
    stated Nullable(Int64),
    charged Int64,
    forgiven Int64,
    charges Nested(meter LowCardinality(String), cell String, print LowCardinality(String), quantity Int64, stated Int64),
    limits Nested(key String, window_starts_at DateTime64(6, 'UTC'), window_ends_at Nullable(DateTime64(6, 'UTC')), units Int64)
) ENGINE = Null;

DROP VIEW IF EXISTS {database}.limit_usage_mv;
DROP VIEW IF EXISTS {database}.usage_quantities_mv;
DROP VIEW IF EXISTS {database}.usage_minutes_mv;
DROP VIEW IF EXISTS {database}.usage_history_events_mv;
DROP VIEW IF EXISTS {database}.usage_history_charges_mv;

ALTER TABLE {database}.usage_events
    DROP COLUMN IF EXISTS amount,
    DROP COLUMN IF EXISTS `limits.key`,
    DROP COLUMN IF EXISTS `limits.window_starts_at`,
    DROP COLUMN IF EXISTS `limits.window_ends_at`,
    DROP COLUMN IF EXISTS `limits.units`;

-- What varies per event compresses with ZSTD: under LZ4 a reported UUID
-- source_id took 36 bytes and occurred_at 8 (scripts/load, storage world).
ALTER TABLE {database}.usage_events
    MODIFY COLUMN source_id String CODEC(ZSTD(1)),
    MODIFY COLUMN invoker String CODEC(ZSTD(1)),
    MODIFY COLUMN lease_id UUID CODEC(ZSTD(1)),
    MODIFY COLUMN n UInt64 CODEC(ZSTD(1)),
    MODIFY COLUMN occurred_at DateTime64(6, 'UTC') CODEC(ZSTD(1)),
    MODIFY COLUMN admitted_at DateTime64(6, 'UTC') CODEC(ZSTD(1)),
    MODIFY COLUMN ingested_at DateTime64(6, 'UTC') CODEC(ZSTD(1));

-- Raw usage is read until its period's last posting: a period of at most 31
-- days whose late usage (35 days) posts by the following month end, so 66
-- days after it occurred, plus two weeks for a posting held up. Disputes and
-- exports read postings, orders and history, which outlive it.
ALTER TABLE {database}.usage_events MODIFY TTL toDateTime(occurred_at) + INTERVAL 80 DAY;

CREATE MATERIALIZED VIEW IF NOT EXISTS {database}.usage_events_mv TO {database}.usage_events AS
SELECT merchant_id, event_id, customer_id, currency, event_type, invoker, invoker_type, outcome, source, source_id, lease_id, n,
       occurred_at, admitted_at, ingested_at, period_starts_at, dimensions, metadata, stated, charged, forgiven,
       charges.meter, charges.cell, charges.print, charges.quantity, charges.stated
FROM {database}.usage_intake;

CREATE MATERIALIZED VIEW IF NOT EXISTS {database}.limit_usage_mv TO {database}.limit_usage AS
SELECT merchant_id, customer_id, limits.key AS limit_key, limits.window_starts_at AS window_starts_at,
       limits.window_ends_at AS window_ends_at, limits.units AS units
FROM {database}.usage_intake
ARRAY JOIN limits;

CREATE MATERIALIZED VIEW IF NOT EXISTS {database}.usage_quantities_mv TO {database}.usage_quantities AS
SELECT merchant_id, customer_id, currency, period_starts_at, charges.meter AS meter, charges.cell AS cell,
       charges.print AS print, charges.quantity AS quantity, charges.stated AS stated, toUInt64(1) AS requests
FROM {database}.usage_intake
ARRAY JOIN charges;

CREATE MATERIALIZED VIEW IF NOT EXISTS {database}.usage_minutes_mv TO {database}.usage_minutes AS
SELECT merchant_id, customer_id, currency, toStartOfMinute(toDateTime(occurred_at, 'UTC')) AS minute, amount
FROM {database}.usage_intake;

-- Usage history per 15-minute UTC bucket for 35 days, which every zone's
-- days are made of; then one row per day of the accounting zone the usage
-- was taken in under, at its first bucket, until 25 months. Rows are as
-- 0001's; day_at leads the key so a day's buckets fold together.
DROP TABLE IF EXISTS {database}.usage_history;

CREATE TABLE IF NOT EXISTS {database}.usage_history (
    merchant_id UUID,
    customer_id UUID,
    day_at DateTime('UTC'),
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
ORDER BY (merchant_id, customer_id, day_at, currency, event_type, meter, cell, print, period_starts_at, invoker, outcome, bucket_at)
TTL bucket_at + INTERVAL 25 MONTH DELETE,
    bucket_at + INTERVAL 35 DAY GROUP BY merchant_id, customer_id, day_at, currency, event_type, meter, cell, print, period_starts_at, invoker, outcome
        SET bucket_at = min(bucket_at), requests = sum(requests), quantity = sum(quantity), stated = sum(stated), charged = sum(charged), forgiven = sum(forgiven)
SETTINGS index_granularity = 1024;

CREATE MATERIALIZED VIEW IF NOT EXISTS {database}.usage_history_events_mv TO {database}.usage_history AS
SELECT merchant_id, customer_id, day_at, toStartOfFifteenMinutes(toDateTime(occurred_at, 'UTC')) AS bucket_at, currency, event_type, '' AS meter, '' AS cell,
       '' AS print, period_starts_at, invoker, outcome, toInt64(1) AS requests, toInt64(0) AS quantity,
       ifNull(stated, 0) AS stated, charged, forgiven
FROM {database}.usage_intake;

CREATE MATERIALIZED VIEW IF NOT EXISTS {database}.usage_history_charges_mv TO {database}.usage_history AS
SELECT merchant_id, customer_id, day_at, toStartOfFifteenMinutes(toDateTime(occurred_at, 'UTC')) AS bucket_at, currency, event_type, charges.meter AS meter,
       charges.cell AS cell, charges.print AS print, period_starts_at, invoker, outcome, toInt64(1) AS requests,
       charges.quantity AS quantity, charges.stated AS stated, toInt64(0) AS charged, toInt64(0) AS forgiven
FROM {database}.usage_intake
ARRAY JOIN charges;

-- The history 0001 kept is rebuilt from the raw usage it still holds, its
-- days in UTC.
INSERT INTO {database}.usage_history (merchant_id, customer_id, day_at, bucket_at, currency, event_type, meter, cell, print, period_starts_at,
    invoker, outcome, requests, quantity, stated, charged, forgiven)
SELECT merchant_id, customer_id, toStartOfDay(toDateTime(occurred_at, 'UTC')), toStartOfFifteenMinutes(toDateTime(occurred_at, 'UTC')), currency, event_type,
       '', '', '', period_starts_at, invoker, outcome, 1, 0, ifNull(stated, 0), charged, forgiven
FROM {database}.usage_events FINAL;

INSERT INTO {database}.usage_history (merchant_id, customer_id, day_at, bucket_at, currency, event_type, meter, cell, print, period_starts_at,
    invoker, outcome, requests, quantity, stated, charged, forgiven)
SELECT merchant_id, customer_id, toStartOfDay(toDateTime(occurred_at, 'UTC')), toStartOfFifteenMinutes(toDateTime(occurred_at, 'UTC')), currency, event_type,
       charges.meter, charges.cell, charges.print, period_starts_at, invoker, outcome, 1, charges.quantity, charges.stated, 0, 0
FROM {database}.usage_events FINAL
ARRAY JOIN charges;
