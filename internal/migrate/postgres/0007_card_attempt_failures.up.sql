-- parent: 6 sha256:c0bef9e0b3e2d935d328b09020333086fb91dcf4e310157fa8ecb86dc0c1e722
-- SEC-30: durable card-testing failure ledger shared by every replica. Failed
-- card attempts are counted per merchant and subject (customer, client address
-- or the whole merchant) in five-minute buckets; blocks are derived from the
-- rolling sums, so no Redis is required.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

CREATE TABLE openrails.card_attempt_failures (
    merchant_id uuid NOT NULL,
    subject text NOT NULL,
    bucket_at timestamp with time zone NOT NULL,
    failures bigint NOT NULL,
    CONSTRAINT card_attempt_failures_pkey PRIMARY KEY (merchant_id, subject, bucket_at),
    CONSTRAINT card_attempt_failures_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE,
    CONSTRAINT chk_card_attempt_failures_positive CHECK (failures > 0),
    CONSTRAINT chk_card_attempt_failures_subject CHECK (subject <> '' AND length(subject) <= 200)
);

COMMENT ON TABLE openrails.card_attempt_failures IS 'SEC-30 card-testing failure counts per merchant, subject and five-minute bucket.';

CREATE INDEX idx_card_attempt_failures_merchant_bucket ON openrails.card_attempt_failures USING btree (merchant_id, bucket_at);
