-- parent: 1 sha256:a5dae506112557b6e58082e22a11789e87c15f7b89c7c04e5e2844fbe8c6d0d6
-- Hosted checkout sessions (#1124): one payment page addressed by session id.

CREATE TABLE billing.hosted_checkout_sessions (
    merchant_id uuid NOT NULL,
    id_hash bytea NOT NULL,
    customer_id uuid NOT NULL,
    price_id uuid NOT NULL,
    offer jsonb NOT NULL,
    success_url text DEFAULT '' NOT NULL,
    origin text DEFAULT '' NOT NULL,
    attempt integer DEFAULT 0 NOT NULL,
    engine_session_id uuid,
    expires_at timestamp with time zone NOT NULL,
    purge_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT hosted_checkout_sessions_pkey PRIMARY KEY (merchant_id, id_hash),
    CONSTRAINT hosted_checkout_sessions_merchant_fk FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE CASCADE,
    CONSTRAINT hosted_checkout_sessions_customer_fk FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT chk_hosted_checkout_sessions_id_hash CHECK (octet_length(id_hash) = 32),
    CONSTRAINT chk_hosted_checkout_sessions_attempt CHECK (attempt >= 0),
    CONSTRAINT chk_hosted_checkout_sessions_purge CHECK (purge_at >= expires_at)
);
COMMENT ON TABLE billing.hosted_checkout_sessions IS '#1124: one hosted checkout session per row. id_hash is SHA-256 of the ocs_ id, which is the bearer credential and is never stored. offer is the offer as minted (plan, amount due, payment options with their PSP bindings). attempt is the current payment attempt and engine_session_id the checkout session it created; attempt advances only after that session failed terminally. Paying stops at expires_at; the row stays readable until purge_at so a late provider return can still be reconciled, then openrails.cleanup_expired_data deletes it.';

CREATE INDEX idx_hosted_checkout_sessions_purge_at ON billing.hosted_checkout_sessions USING btree (purge_at);
CREATE INDEX idx_hosted_checkout_sessions_customer ON billing.hosted_checkout_sessions USING btree (merchant_id, customer_id);
