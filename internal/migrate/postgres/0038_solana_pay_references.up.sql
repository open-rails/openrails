-- parent: 37 sha256:25a67680f5319bd76563defa50096cea4d8c9f492b4a624ad38cb4c822936b7b
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
    seen_until text,
    scan_stack text[] DEFAULT '{}'::text[] NOT NULL,
    scan_below text,
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

COMMENT ON TABLE openrails.solana_pay_references IS '#1086: one Solana Pay reference per checkout attempt. pending = awaiting a transfer landed by settle_until; confirmed = one signature credited (or mirrored); expired = nothing credited by settle_until. Purchase references stay watched until watch_until so a second or late transfer is recorded, then openrails.solana_pay_gc deletes the settled row. seen_until is the newest signature whose older history is fully processed; scan_stack holds the before-cursors of an unfinished walk down the history and scan_below the cursor whose older signatures were just processed, so no signature is ever skipped however many land on the reference; a reference is never collected mid-walk. built_transaction is the one transaction-request tx offered while its blockhash can still land.';

CREATE INDEX idx_solana_pay_references_due ON openrails.solana_pay_references USING btree (next_poll_at);

CREATE INDEX idx_solana_pay_references_settled ON openrails.solana_pay_references USING btree (watch_until) WHERE status <> 'pending';

CREATE TABLE openrails.solana_pay_receipts (
    merchant_id uuid NOT NULL,
    reference text NOT NULL,
    signature text NOT NULL,
    checkout_session_id uuid NOT NULL,
    disposition text NOT NULL,
    review_reason text,
    recipient text NOT NULL,
    token_mint text NOT NULL,
    expected_amount bigint NOT NULL,
    received_amount bigint NOT NULL,
    payer text,
    landed_at timestamp with time zone,
    payment_id uuid,
    resolved_at timestamp with time zone,
    resolution text,
    created_at timestamp with time zone NOT NULL,
    CONSTRAINT solana_pay_receipts_pkey PRIMARY KEY (merchant_id, reference, signature),
    CONSTRAINT solana_pay_receipts_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE,
    CONSTRAINT chk_solana_pay_receipts_disposition CHECK (
        disposition = 'credited' AND payment_id IS NOT NULL AND (review_reason IS NULL OR review_reason = 'overpaid')
        OR disposition = 'review' AND payment_id IS NULL AND review_reason IN ('already_paid', 'late', 'underpaid', 'session_closed', 'wrong_asset', 'unreadable', 'settle_failed')
        OR disposition = 'duplicate' AND payment_id IS NULL AND review_reason = 'claimed_elsewhere'
        OR disposition = 'ignored' AND payment_id IS NULL AND review_reason IS NULL
    ),
    CONSTRAINT chk_solana_pay_receipts_resolution CHECK ((resolved_at IS NULL) = (resolution IS NULL) AND (resolved_at IS NULL OR review_reason IS NOT NULL)),
    CONSTRAINT chk_solana_pay_receipts_amounts CHECK (expected_amount >= 0 AND received_amount >= 0)
);

COMMENT ON TABLE openrails.solana_pay_receipts IS '#1086: every signature observed on a Solana Pay reference, recorded once. credited = the checkout was paid by it (overpaid flags the excess for refund); review = money that was not credited (already_paid, late, underpaid, session_closed, wrong_asset, unreadable, settle_failed) and needs a refund or operator decision, closed by resolved_at; duplicate = the transfer already settled another reference; ignored = no value to the merchant (deleted with its reference). A transfer to one recipient in one mint is credited or reviewed at most once across every reference. Unresolved reviews refuse the billing archive.';

CREATE UNIQUE INDEX uq_solana_pay_receipts_transfer ON openrails.solana_pay_receipts USING btree (signature, recipient, token_mint) WHERE disposition IN ('credited', 'review');

CREATE INDEX idx_solana_pay_receipts_review ON openrails.solana_pay_receipts USING btree (merchant_id, created_at) WHERE review_reason IS NOT NULL AND resolved_at IS NULL;
