CREATE TABLE openrails.external_provider_mutation_logs (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    provider text NOT NULL,
    provider_account_id uuid,
    provider_intent_id uuid,
    intent_type text,
    idempotency_key text,
    attempt integer NOT NULL DEFAULT 0,
    phase text NOT NULL,
    reason text,
    evidence jsonb,
    created_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT external_provider_mutation_logs_pkey PRIMARY KEY (id),
    CONSTRAINT external_provider_mutation_logs_phase_check CHECK (phase = ANY (ARRAY['attempting'::text, 'succeeded'::text, 'failed'::text, 'unknown'::text, 'parked'::text])),
    CONSTRAINT external_provider_mutation_logs_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE,
    CONSTRAINT external_provider_mutation_logs_provider_account_fk FOREIGN KEY (provider_account_id) REFERENCES openrails.provider_accounts(id) ON DELETE SET NULL,
    CONSTRAINT external_provider_mutation_logs_provider_intent_fk FOREIGN KEY (provider_intent_id) REFERENCES openrails.provider_intents(id) ON DELETE SET NULL
);

ALTER TABLE ONLY openrails.external_provider_mutation_logs FORCE ROW LEVEL SECURITY;

CREATE INDEX idx_external_provider_mutation_logs_merchant_created ON openrails.external_provider_mutation_logs USING btree (merchant_id, created_at DESC);
CREATE INDEX idx_external_provider_mutation_logs_provider_intent ON openrails.external_provider_mutation_logs USING btree (provider_intent_id) WHERE (provider_intent_id IS NOT NULL);
CREATE INDEX idx_external_provider_mutation_logs_provider_account ON openrails.external_provider_mutation_logs USING btree (provider_account_id) WHERE (provider_account_id IS NOT NULL);
CREATE INDEX idx_external_provider_mutation_logs_provider_phase ON openrails.external_provider_mutation_logs USING btree (provider, phase, created_at DESC);

ALTER TABLE openrails.external_provider_mutation_logs ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.external_provider_mutation_logs USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE ON TABLE openrails.external_provider_mutation_logs TO openrails_app;

COMMENT ON TABLE openrails.external_provider_mutation_logs IS 'Append-only operator history for external provider mutations executed from provider intents/convergence (#533).';
COMMENT ON COLUMN openrails.external_provider_mutation_logs.phase IS 'Provider mutation lifecycle phase: attempting before the remote call, then succeeded/failed/unknown/parked after the handler classifies the result.';
COMMENT ON COLUMN openrails.external_provider_mutation_logs.evidence IS 'Scrubbed structured metadata only. Never store API keys, authorization headers, card data, private keys, or unsanitized provider bodies.';
