-- Usage, the first event family: one row per settled usage item, and the
-- rollups its materialized views keep, keyed the way readers ask. Every key
-- leads with merchant_id. Times are UTC microseconds.

-- Raw usage. event_id is the intake identity (merchant, lease, n) or the
-- request id, and with the customer the whole sorting key: a resend finds an
-- earlier attempt's copy by key at any occurred_at, and FINAL and merges
-- collapse any copy in the same month. amount is the provisional price limits
-- and the accrual-rate meter count; charges are the priced meter cells, frozen
-- at intake with the card print and pricing period; limits are the windows
-- that count the item, located at admitted_at.
CREATE TABLE IF NOT EXISTS {database}.usage_events (
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
    dimensions Map(LowCardinality(String), Int64),
    metadata String CODEC(ZSTD(3)),
    amount Int64,
    stated Nullable(Int64),
    charged Int64,
    forgiven Int64,
    charges Nested(meter LowCardinality(String), cell String, print LowCardinality(String), quantity Int64, stated Int64),
    limits Nested(key String, window_starts_at DateTime64(6, 'UTC'), window_ends_at Nullable(DateTime64(6, 'UTC')), units Int64)
) ENGINE = ReplacingMergeTree
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (merchant_id, customer_id, event_id)
TTL toDateTime(occurred_at) + INTERVAL 100 DAY;

-- Units per limit window (fixed, session, billing period, lifetime, a rolling
-- or rate limit's bucket, spend and failed-usage windows). A window without an
-- end is kept.
CREATE TABLE IF NOT EXISTS {database}.limit_usage (
    merchant_id UUID,
    customer_id UUID,
    limit_key String,
    window_starts_at DateTime64(6, 'UTC'),
    window_ends_at Nullable(DateTime64(6, 'UTC')),
    units Int64
) ENGINE = SummingMergeTree(units)
ORDER BY (merchant_id, customer_id, limit_key, window_starts_at)
TTL toDateTime(ifNull(window_ends_at, window_starts_at)) + INTERVAL 61 DAY DELETE WHERE window_ends_at IS NOT NULL
SETTINGS index_granularity = 1024;

CREATE MATERIALIZED VIEW IF NOT EXISTS {database}.limit_usage_mv TO {database}.limit_usage AS
SELECT merchant_id, customer_id, limits.key AS limit_key, limits.window_starts_at AS window_starts_at,
       limits.window_ends_at AS window_ends_at, limits.units AS units
FROM {database}.usage_events
ARRAY JOIN limits;

-- Quantities per pricing period and meter cell, by card print: what a
-- period's price is taken from.
CREATE TABLE IF NOT EXISTS {database}.usage_quantities (
    merchant_id UUID,
    customer_id UUID,
    currency LowCardinality(String),
    period_starts_at DateTime64(6, 'UTC'),
    meter LowCardinality(String),
    cell String,
    print LowCardinality(String),
    quantity Int64,
    stated Int64,
    requests UInt64
) ENGINE = SummingMergeTree((quantity, stated, requests))
ORDER BY (merchant_id, customer_id, currency, period_starts_at, meter, cell, print)
TTL toDateTime(period_starts_at) + INTERVAL 100 DAY
SETTINGS index_granularity = 1024;

CREATE MATERIALIZED VIEW IF NOT EXISTS {database}.usage_quantities_mv TO {database}.usage_quantities AS
SELECT merchant_id, customer_id, currency, period_starts_at, charges.meter AS meter, charges.cell AS cell,
       charges.print AS print, charges.quantity AS quantity, charges.stated AS stated, toUInt64(1) AS requests
FROM {database}.usage_events
ARRAY JOIN charges;

-- Provisional amounts per minute: the accrual-rate cap and balance watch.
CREATE TABLE IF NOT EXISTS {database}.usage_minutes (
    merchant_id UUID,
    customer_id UUID,
    currency LowCardinality(String),
    minute DateTime('UTC'),
    amount Int64
) ENGINE = SummingMergeTree(amount)
ORDER BY (merchant_id, customer_id, currency, minute)
TTL minute + INTERVAL 2 DAY
SETTINGS index_granularity = 1024;

CREATE MATERIALIZED VIEW IF NOT EXISTS {database}.usage_minutes_mv TO {database}.usage_minutes AS
SELECT merchant_id, customer_id, currency, toStartOfMinute(toDateTime(occurred_at, 'UTC')) AS minute, amount
FROM {database}.usage_events;

-- Usage history per 15-minute UTC bucket, which every zone's days and
-- months are made of: each event once with meter '', and each of its charges
-- under its meter, cell and card print in its pricing period, which price
-- the bucket's share of the period's total at read. Sums are signed: an
-- erasure folds an invoker's rows into '' by inserting their negation.
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
ORDER BY (merchant_id, customer_id, bucket_at, currency, event_type, meter, cell, print, period_starts_at, invoker, outcome)
TTL bucket_at + INTERVAL 25 MONTH
SETTINGS index_granularity = 1024;

CREATE MATERIALIZED VIEW IF NOT EXISTS {database}.usage_history_events_mv TO {database}.usage_history AS
SELECT merchant_id, customer_id, toStartOfFifteenMinutes(toDateTime(occurred_at, 'UTC')) AS bucket_at, currency, event_type, '' AS meter, '' AS cell,
       '' AS print, period_starts_at, invoker, outcome, toInt64(1) AS requests, toInt64(0) AS quantity,
       ifNull(stated, 0) AS stated, charged, forgiven
FROM {database}.usage_events;

CREATE MATERIALIZED VIEW IF NOT EXISTS {database}.usage_history_charges_mv TO {database}.usage_history AS
SELECT merchant_id, customer_id, toStartOfFifteenMinutes(toDateTime(occurred_at, 'UTC')) AS bucket_at, currency, event_type, charges.meter AS meter,
       charges.cell AS cell, charges.print AS print, period_starts_at, invoker, outcome, toInt64(1) AS requests,
       charges.quantity AS quantity, charges.stated AS stated, toInt64(0) AS charged, toInt64(0) AS forgiven
FROM {database}.usage_events
ARRAY JOIN charges;
