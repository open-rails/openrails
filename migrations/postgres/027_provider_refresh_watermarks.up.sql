-- Provider Refresh durable event watermarks (#574).

CREATE TABLE openrails.provider_refresh_watermarks (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    merchant_id uuid NOT NULL,
    provider text NOT NULL,
    provider_account_id uuid NULL,
    provider_account_key uuid GENERATED ALWAYS AS (COALESCE(provider_account_id, '00000000-0000-0000-0000-000000000000'::uuid)) STORED,
    event_domain text NOT NULL,
    watermark_at timestamptz NOT NULL,
    last_attempted_at timestamptz NULL,
    last_succeeded_at timestamptz NULL,
    last_error text NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT provider_refresh_watermarks_pkey PRIMARY KEY (id),
    CONSTRAINT provider_refresh_watermarks_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE,
    CONSTRAINT provider_refresh_watermarks_provider_account_fk FOREIGN KEY (provider_account_id) REFERENCES openrails.provider_accounts(id) ON DELETE SET NULL,
    CONSTRAINT provider_refresh_watermarks_provider_check CHECK (provider = ANY (ARRAY['nmi'::text, 'ccbill'::text, 'stripe'::text, 'solana'::text])),
    CONSTRAINT provider_refresh_watermarks_event_domain_check CHECK (event_domain = ANY (ARRAY['events'::text])),
    CONSTRAINT provider_refresh_watermarks_account_nonzero CHECK (provider_account_id IS NULL OR provider_account_id <> '00000000-0000-0000-0000-000000000000'::uuid),
    CONSTRAINT provider_refresh_watermarks_identity_key UNIQUE (merchant_id, provider, provider_account_key, event_domain)
);

CREATE INDEX idx_provider_refresh_watermarks_provider ON openrails.provider_refresh_watermarks USING btree (provider, event_domain, watermark_at);
CREATE INDEX idx_provider_refresh_watermarks_account ON openrails.provider_refresh_watermarks USING btree (provider_account_id) WHERE provider_account_id IS NOT NULL;

ALTER TABLE ONLY openrails.provider_refresh_watermarks FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.provider_refresh_watermarks ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.provider_refresh_watermarks USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.provider_refresh_watermarks TO openrails_app;

COMMENT ON TABLE openrails.provider_refresh_watermarks IS 'Durable Provider Refresh watermarks. A failed or partial provider read records last_error but never advances watermark_at.';
COMMENT ON COLUMN openrails.provider_refresh_watermarks.provider_account_id IS 'Current provider account row when resolvable; NULL is the compatibility/global lane for providers without a bound account identity.';
COMMENT ON COLUMN openrails.provider_refresh_watermarks.event_domain IS 'Refresh domain. events currently covers provider transaction/subscription event windows.';
COMMENT ON COLUMN openrails.provider_refresh_watermarks.watermark_at IS 'Exclusive lower bound for the next successful bounded provider event refresh window.';
