-- Restore payer_spend_limits (0001 shape + the 0012 RLS index) and drop the
-- billing-policy registry. Policy bodies are NOT translated back: the retired
-- table cannot express a policy kind, which is the whole reason it was replaced.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

CREATE TABLE IF NOT EXISTS openrails.payer_spend_limits (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid,
    tier text NOT NULL,
    policy jsonb DEFAULT '{}'::jsonb NOT NULL,
    policy_version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY openrails.payer_spend_limits FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.payer_spend_limits IS 'Per-tier payer spend limit (#477/#517): the platform caps the payer''s spend, keyed by trust-tier. customer_id NULL is the merchant-wide default; non-NULL is a per-customer override.';

COMMENT ON COLUMN openrails.payer_spend_limits.customer_id IS 'NULL = merchant-wide default tier limit (#477); non-NULL = per-customer override taking precedence for that customer.';

COMMENT ON COLUMN openrails.payer_spend_limits.policy IS 'JSONB tier money policy: budget_windows and bad_spend_windows. Money values use the request currency internal precision.';

ALTER TABLE ONLY openrails.payer_spend_limits
    ADD CONSTRAINT payer_spend_limits_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.payer_spend_limits
    ADD CONSTRAINT payer_spend_limits_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id);

ALTER TABLE ONLY openrails.payer_spend_limits
    ADD CONSTRAINT payer_spend_limits_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE UNIQUE INDEX uq_payer_spend_limits_customer ON openrails.payer_spend_limits USING btree (merchant_id, customer_id, tier) WHERE (customer_id IS NOT NULL);

CREATE UNIQUE INDEX uq_payer_spend_limits_merchant_default ON openrails.payer_spend_limits USING btree (merchant_id, tier) WHERE (customer_id IS NULL);

CREATE INDEX idx_payer_spend_limits_merchant_id ON openrails.payer_spend_limits USING btree (merchant_id);

ALTER TABLE openrails.payer_spend_limits ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.payer_spend_limits USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.payer_spend_limits TO openrails_app;

DROP TABLE IF EXISTS openrails.billing_policy_bindings;
DROP TABLE IF EXISTS openrails.billing_policies;
