-- parent: 43 sha256:4117a134f308efc5bd74c7645bee5b077b666e2a2c8871bce83b1d32bd4b9c3d
-- Repair: none-needed A new table; replicas start counting afresh.
-- Rate-limit windows, admin lockouts and captcha challenges that replicas
-- without Redis share; with Redis they live there.
CREATE TABLE billing.rate_windows (
    key text NOT NULL,
    hits bigint NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT rate_windows_pkey PRIMARY KEY (key),
    CONSTRAINT rate_windows_key_check CHECK (key <> '' AND length(key) <= 512),
    CONSTRAINT rate_windows_hits_check CHECK (hits > 0)
);
COMMENT ON TABLE billing.rate_windows IS 'Global by design: the rate-limit windows, admin lockouts and captcha challenges every replica counts when no Redis is configured, keyed as in Redis. Retention: rows are deleted once expired.';
CREATE INDEX rate_windows_expires_at_idx ON billing.rate_windows USING btree (expires_at);
