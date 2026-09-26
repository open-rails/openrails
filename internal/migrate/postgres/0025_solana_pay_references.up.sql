-- parent: 24 sha256:3db9238fbffeff5405330b666da95bdc5f05a39fae6c60443d491e5ca56b0db3
-- #1086: Solana Pay state is durable and shared by every replica. A checkout
-- attempt has one reference; it moves pending -> confirmed | expired by one
-- conditional UPDATE. Every transfer that lands on a reference is recorded
-- once, keyed by its on-chain signature, and a signature is credited at most
-- once anywhere.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

CREATE TABLE openrails.solana_pay_references (
    merchant_id uuid NOT NULL,
    reference text NOT NULL,
    checkout_session_id uuid NOT NULL,
    kind text NOT NULL,
    status text NOT NULL,
    settle_until timestamp with time zone NOT NULL,
    watch_until timestamp with time zone NOT NULL,
    next_poll_at timestamp with time zone NOT NULL,
    signature text,
    built_transaction text,
    built_valid_height bigint,
    created_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT solana_pay_references_pkey PRIMARY KEY (merchant_id, reference),
    CONSTRAINT solana_pay_references_session_key UNIQUE (merchant_id, checkout_session_id),
    CONSTRAINT solana_pay_references_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE,
    CONSTRAINT chk_solana_pay_references_kind CHECK (kind IN ('purchase', 'subscribe', 'lifecycle')),
    CONSTRAINT chk_solana_pay_references_status CHECK (status IN ('pending', 'confirmed', 'expired')),
    CONSTRAINT chk_solana_pay_references_signature CHECK ((status = 'confirmed') = (signature IS NOT NULL)),
    CONSTRAINT chk_solana_pay_references_window CHECK (watch_until >= settle_until),
    CONSTRAINT chk_solana_pay_references_built CHECK ((built_transaction IS NULL) = (built_valid_height IS NULL))
);

COMMENT ON TABLE openrails.solana_pay_references IS '#1086: one Solana Pay reference per checkout attempt. pending = awaiting a transfer landed by settle_until; confirmed = one signature credited (or mirrored); expired = nothing credited by settle_until. Purchase references stay watched until watch_until so a second or late transfer is recorded, then openrails.solana_pay_gc deletes the settled row. built_transaction is the one transaction-request tx offered while its blockhash can still land.';

CREATE INDEX idx_solana_pay_references_due ON openrails.solana_pay_references USING btree (next_poll_at);

CREATE INDEX idx_solana_pay_references_settled ON openrails.solana_pay_references USING btree (watch_until) WHERE status <> 'pending';

CREATE TABLE openrails.solana_pay_receipts (
    merchant_id uuid NOT NULL,
    reference text NOT NULL,
    signature text NOT NULL,
    checkout_session_id uuid NOT NULL,
    disposition text NOT NULL,
    review_reason text,
    token_mint text NOT NULL,
    expected_amount bigint NOT NULL,
    received_amount bigint NOT NULL,
    payer text,
    landed_at timestamp with time zone,
    payment_id uuid,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT solana_pay_receipts_pkey PRIMARY KEY (merchant_id, reference, signature),
    CONSTRAINT solana_pay_receipts_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE,
    CONSTRAINT chk_solana_pay_receipts_disposition CHECK (
        disposition = 'credited' AND payment_id IS NOT NULL AND (review_reason IS NULL OR review_reason = 'overpaid')
        OR disposition = 'review' AND payment_id IS NULL AND review_reason IN ('already_paid', 'late', 'underpaid', 'session_closed')
        OR disposition = 'ignored' AND payment_id IS NULL AND review_reason IS NULL
    ),
    CONSTRAINT chk_solana_pay_receipts_amounts CHECK (expected_amount >= 0 AND received_amount >= 0)
);

COMMENT ON TABLE openrails.solana_pay_receipts IS '#1086: every signature observed on a Solana Pay reference, recorded once. credited = the checkout was paid by it (overpaid flags the excess for refund); review = money that was not credited (already_paid, late, underpaid, session_closed) and needs a refund or operator decision; ignored = no transfer to the merchant (deleted with its reference). A signature is credited or reviewed at most once across every reference.';

CREATE UNIQUE INDEX uq_solana_pay_receipts_signature ON openrails.solana_pay_receipts USING btree (signature) WHERE disposition <> 'ignored';

CREATE INDEX idx_solana_pay_receipts_review ON openrails.solana_pay_receipts USING btree (merchant_id, created_at) WHERE review_reason IS NOT NULL;
