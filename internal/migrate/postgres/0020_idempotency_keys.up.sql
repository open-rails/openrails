-- parent: 19 sha256:ccbd9723de6431d71dba2d6937cb8e6ac87141ded6a5ac6d62deacbb6622333d
-- #1099: request and webhook-delivery idempotency is durable and shared by
-- every replica. A claim is one INSERT … ON CONFLICT DO NOTHING; a processing
-- claim is held by a lease its owner renews. Every time is the database's
-- now(). token fences a superseded owner. Provider calls are still made once
-- by rail_intents; these rows decide who runs a request and what a replay
-- answers.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

CREATE TABLE openrails.idempotency_keys (
    merchant_id uuid NOT NULL,
    operation text NOT NULL,
    idempotency_key text NOT NULL,
    status text NOT NULL,
    token uuid NOT NULL,
    claims bigint DEFAULT 1 NOT NULL,
    result jsonb,
    error text,
    lease_expires_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT idempotency_keys_pkey PRIMARY KEY (merchant_id, operation, idempotency_key),
    CONSTRAINT idempotency_keys_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE,
    CONSTRAINT chk_idempotency_keys_status CHECK (status IN ('processing', 'succeeded', 'failed')),
    CONSTRAINT chk_idempotency_keys_identity CHECK (operation <> '' AND idempotency_key <> ''),
    CONSTRAINT chk_idempotency_keys_result CHECK (status = 'succeeded' OR result IS NULL),
    CONSTRAINT chk_idempotency_keys_claims CHECK (claims > 0),
    CONSTRAINT chk_idempotency_keys_expiry CHECK (expires_at >= lease_expires_at)
);

COMMENT ON TABLE openrails.idempotency_keys IS '#1099: one claim per (merchant, operation, key). processing = owned until lease_expires_at, then reclaimable by exactly one caller; succeeded = replay result; failed = reclaimable. token fences a superseded owner; claims counts claims. Rows past expires_at are deleted by openrails.idempotency_gc.';

CREATE INDEX idx_idempotency_keys_expires_at ON openrails.idempotency_keys USING btree (expires_at);
