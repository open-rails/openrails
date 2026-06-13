-- =============================================================================
-- Prepaid credit windows (issue #335).
--
-- A window is a FIRST-CLASS bulk reservation: opening one moves funds from the
-- payer's available balance into held_balance (the same mechanics as a hold),
-- and the caller admits requests locally against it. Settlement decrements
-- held + balance per request (idempotent per request_id); close/expiry releases
-- the unsettled remainder. Holds settle exactly once (CaptureHold), so a window
-- is NOT a reused hold — its lifecycle state lives here.
-- =============================================================================

SET lock_timeout = '10s';
SET statement_timeout = '300s';

CREATE TABLE openrails.credit_windows (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    tenant_subject_id uuid CONSTRAINT credit_windows_tenant_subject_id_not_null NOT NULL,
    credit_type_id uuid NOT NULL,
    held_amount bigint NOT NULL,
    settled_amount bigint DEFAULT 0 NOT NULL,
    status text DEFAULT 'open'::text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT credit_windows_pkey PRIMARY KEY (id),
    CONSTRAINT credit_windows_status_chk CHECK ((status = ANY (ARRAY['open'::text, 'closed'::text, 'expired'::text]))),
    CONSTRAINT credit_windows_held_nonneg_chk CHECK ((held_amount >= 0)),
    CONSTRAINT credit_windows_settled_within_held_chk CHECK (((settled_amount >= 0) AND (settled_amount <= held_amount)))
);

ALTER TABLE ONLY openrails.credit_windows FORCE ROW LEVEL SECURITY;

ALTER TABLE ONLY openrails.credit_windows
    ADD CONSTRAINT credit_windows_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES openrails.tenant_subjects(id);

ALTER TABLE ONLY openrails.credit_windows
    ADD CONSTRAINT credit_windows_credit_type_id_fkey FOREIGN KEY (credit_type_id) REFERENCES openrails.credit_types(id);

COMMENT ON TABLE openrails.credit_windows IS 'Prepaid credit windows (issue #335): one bulk held reservation a host admits requests against locally; settled in cross-payer batches, remainder released at close/expiry.';

COMMENT ON COLUMN openrails.credit_windows.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';

COMMENT ON COLUMN openrails.credit_windows.tenant_subject_id IS 'OpenRails payable tenant subject id. Join openrails.tenant_subjects for tenant_id, issuer, and subject.';

COMMENT ON COLUMN openrails.credit_windows.held_amount IS 'Total reserved for this window (open + refills). Reflected in credit_balances.held_balance while status=open.';

COMMENT ON COLUMN openrails.credit_windows.settled_amount IS 'Sum of settled actuals. Server enforces settled_amount <= held_amount; the unsettled remainder releases at close/expiry.';

CREATE INDEX idx_credit_windows_subject_status ON openrails.credit_windows USING btree (tenant_subject_id, status);

CREATE INDEX idx_credit_windows_open_expires ON openrails.credit_windows USING btree (expires_at) WHERE (status = 'open'::text);

CREATE INDEX idx_credit_windows_tenant_id ON openrails.credit_windows USING btree (tenant_id);

ALTER TABLE openrails.credit_windows ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON openrails.credit_windows USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.credit_windows TO openrails_app;
