-- parent: 44 sha256:30e54cfe73c050b71e3a4b4c0ca73afba7b48f572f77b41ba9224fcc6a0ffc0a
-- Repair: none-needed A new table; the first refresh, or the first quote, fills it.
-- The FX rates one refresh reads for the whole fleet, which every replica
-- quotes from.
CREATE TABLE billing.fx_rates (
    from_currency text NOT NULL,
    to_currency text NOT NULL,
    rate double precision NOT NULL,
    as_of timestamp with time zone NOT NULL,
    fetched_at timestamp with time zone NOT NULL,
    CONSTRAINT fx_rates_pkey PRIMARY KEY (from_currency, to_currency),
    CONSTRAINT fx_rates_currencies_check CHECK (from_currency ~ '^[A-Z0-9]{3,12}$' AND to_currency ~ '^[A-Z0-9]{3,12}$' AND from_currency <> to_currency),
    CONSTRAINT fx_rates_rate_check CHECK (rate > 0 AND rate < 'Infinity'::double precision)
);
COMMENT ON TABLE billing.fx_rates IS 'Global by design: the latest published rate of each currency pair, read once for the fleet and quoted by every replica. One row per pair, replaced in place.';
COMMENT ON COLUMN billing.fx_rates.as_of IS 'The source''s publication date for the rate.';
COMMENT ON COLUMN billing.fx_rates.fetched_at IS 'When the rate was read; a quote reads its base currency again once this is 3 hours old.';
