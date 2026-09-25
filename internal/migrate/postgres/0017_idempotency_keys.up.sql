-- parent: 16 sha256:a98b4a6b1fcbdb09db42024e7627e1d69ff071cc8058a2eea5eb06c6415cd8a3
-- #1099: request and webhook-delivery idempotency is durable and shared by
-- every replica. A claim is one INSERT … ON CONFLICT DO NOTHING; a processing
-- claim is held by a lease its owner renews. Provider calls are still made
-- once by rail_intents; these rows only decide who runs a request and what a
-- replay answers.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

CREATE TABLE openrails.idempotency_keys (
    merchant_id uuid NOT NULL,
    operation text NOT NULL,
    idempotency_key text NOT NULL,
    status text NOT NULL,
    claims bigint DEFAULT 1 NOT NULL,
    result jsonb,
    error text,
    lease_expires_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT idempotency_keys_pkey PRIMARY KEY (merchant_id, operation, idempotency_key),
    CONSTRAINT idempotency_keys_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE,
    CONSTRAINT chk_idempotency_keys_status CHECK (status IN ('processing', 'succeeded', 'failed')),
    CONSTRAINT chk_idempotency_keys_identity CHECK (operation <> '' AND idempotency_key <> ''),
    CONSTRAINT chk_idempotency_keys_result CHECK (status = 'succeeded' OR result IS NULL),
    CONSTRAINT chk_idempotency_keys_claims CHECK (claims > 0),
    CONSTRAINT chk_idempotency_keys_expiry CHECK (expires_at >= lease_expires_at)
);

COMMENT ON TABLE openrails.idempotency_keys IS '#1099: one claim per (merchant, operation, key). processing = owned until lease_expires_at, then reclaimable by exactly one caller; succeeded = replay result; failed = reclaimable. claims fences a superseded owner. Rows past expires_at are deleted by openrails.idempotency_gc.';

CREATE INDEX idx_idempotency_keys_expires_at ON openrails.idempotency_keys USING btree (expires_at);
