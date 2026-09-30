-- parent: 26 sha256:4d30f5570dc11bb861f5e951dc5371ff2a190097402de28275b12669df53f870
-- #1107: a merchant's api_host takes effect only after the merchant proves it
-- controls the domain. A claim holds the host and a random token until a TXT
-- record at _openrails-challenge.<host> carries the token; then the host binds
-- (merchants.api_host) and the claim closes. A claim routes nothing.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

CREATE TABLE openrails.merchant_api_host_claims (
    merchant_id uuid NOT NULL,
    api_host text NOT NULL,
    token text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT merchant_api_host_claims_pkey PRIMARY KEY (merchant_id),
    CONSTRAINT merchant_api_host_claims_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE
);

COMMENT ON TABLE openrails.merchant_api_host_claims IS '#1107: a merchant''s unproven api_host claim, one per merchant. The token must appear in a TXT record at _openrails-challenge.<api_host> before the host binds to merchants.api_host. Routes nothing.';
