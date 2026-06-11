-- #107 phase 2: processor reconciliation persistence.
--
-- reconciliation_runs records each manual reconcile execution (advisory or
-- enforce) with its provider set, fetch window, lifecycle, and a summary jsonb
-- (per-provider counts + dunning forensics).
--
-- reconciliation_findings is the durable drift ledger with STABLE IDENTITY
-- (design decision 1, 2026-06-11): one row per (tenant, provider,
-- finding_type, subject_key) forever. Re-runs UPDATE the open finding
-- (last_seen_*, occurrence_count); findings absent from a completed run
-- covering their provider auto-resolve as auto_vanished. The admin action
-- queue is simply the findings with requires_admin = true.
--
-- Tenancy mirrors the neighboring billing tables: tenant_id stamped NOT NULL
-- with the single-tenant default, RLS tenant_isolation policy on the
-- app.tenant_id GUC, FORCE ROW LEVEL SECURITY.

CREATE TABLE billing.reconciliation_runs (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    mode text NOT NULL,
    providers text[] NOT NULL,
    window_since timestamp with time zone,
    window_until timestamp with time zone,
    started_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    finished_at timestamp with time zone,
    status text DEFAULT 'running'::text NOT NULL,
    summary jsonb,
    error text,
    CONSTRAINT reconciliation_runs_pkey PRIMARY KEY (id),
    CONSTRAINT chk_reconciliation_runs_mode CHECK ((mode = ANY (ARRAY['advisory'::text, 'enforce'::text]))),
    CONSTRAINT chk_reconciliation_runs_status CHECK ((status = ANY (ARRAY['running'::text, 'completed'::text, 'failed'::text])))
);

ALTER TABLE ONLY billing.reconciliation_runs FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE billing.reconciliation_runs IS 'One row per manual reconcile run (#107): advisory diffs or enforce convergence against the payment processors. Summary jsonb carries per-provider counts and the dunning-forensics report.';
COMMENT ON COLUMN billing.reconciliation_runs.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';

CREATE TABLE billing.reconciliation_findings (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid DEFAULT '00000000-0000-0000-0000-000000000001'::uuid NOT NULL,
    provider text NOT NULL,
    finding_type text NOT NULL,
    subject_key text NOT NULL,
    severity text NOT NULL,
    status text DEFAULT 'open'::text NOT NULL,
    requires_admin boolean DEFAULT false NOT NULL,
    recommended_action text,
    local_evidence jsonb,
    remote_evidence jsonb,
    intent_evidence jsonb,
    resolution_evidence jsonb,
    first_seen_run uuid NOT NULL,
    last_seen_run uuid NOT NULL,
    first_seen_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    last_seen_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    occurrence_count integer DEFAULT 1 NOT NULL,
    resolved_at timestamp with time zone,
    resolution text,
    notes text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT reconciliation_findings_pkey PRIMARY KEY (id),
    CONSTRAINT chk_reconciliation_findings_type CHECK ((finding_type = ANY (ARRAY['PS-1'::text, 'PS-2'::text, 'PS-3'::text, 'PS-4'::text, 'PS-5'::text, 'PS-6'::text, 'PS-7'::text, 'PS-8'::text, 'PS-9'::text]))),
    CONSTRAINT chk_reconciliation_findings_severity CHECK ((severity = ANY (ARRAY['critical'::text, 'high'::text, 'medium'::text, 'low'::text]))),
    CONSTRAINT chk_reconciliation_findings_status CHECK ((status = ANY (ARRAY['open'::text, 'auto_fixed'::text, 'admin_pending'::text, 'resolved'::text, 'dismissed'::text]))),
    CONSTRAINT chk_reconciliation_findings_resolution CHECK ((resolution IS NULL OR resolution = ANY (ARRAY['auto_vanished'::text, 'enforced'::text, 'admin_ack'::text, 'dismissed'::text]))),
    CONSTRAINT chk_reconciliation_findings_resolved_fields CHECK (((resolved_at IS NULL) = (resolution IS NULL)))
);

ALTER TABLE ONLY billing.reconciliation_findings FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE billing.reconciliation_findings IS 'Durable local-vs-processor drift ledger (#107 PS-1..PS-9). Stable identity per (tenant, provider, finding_type, subject_key): re-runs update, disappearance auto-resolves as auto_vanished. requires_admin = true rows are the admin action queue.';
COMMENT ON COLUMN billing.reconciliation_findings.subject_key IS 'Stable identity of the drifted subject within (provider, finding_type): processor subscription id, transaction id, local subscription/payment-method uuid, or tenant_subject uuid depending on the check.';
COMMENT ON COLUMN billing.reconciliation_findings.intent_evidence IS 'Class-3 local-intent annotation (design decision 5): e.g. DeletionScheduledAt marker present => "recorded delete never executed; boot rescan replays it" — keeps the admin queue to genuine unknowns.';
COMMENT ON COLUMN billing.reconciliation_findings.tenant_id IS 'Tenant / billing-namespace this row belongs to (issue #223). NOT NULL; defaults to the ''default'' tenant for single-tenant writers, stamped explicitly by multi-tenant writers.';

CREATE UNIQUE INDEX uq_reconciliation_findings_identity ON billing.reconciliation_findings USING btree (tenant_id, provider, finding_type, subject_key);

CREATE INDEX idx_reconciliation_findings_tenant_id ON billing.reconciliation_findings USING btree (tenant_id);

CREATE INDEX idx_reconciliation_findings_open ON billing.reconciliation_findings USING btree (provider, finding_type) WHERE (status = ANY (ARRAY['open'::text, 'admin_pending'::text]));

CREATE INDEX idx_reconciliation_findings_admin_queue ON billing.reconciliation_findings USING btree (last_seen_at DESC) WHERE (requires_admin AND (status = 'admin_pending'::text));

CREATE INDEX idx_reconciliation_runs_tenant_id ON billing.reconciliation_runs USING btree (tenant_id);

CREATE INDEX idx_reconciliation_runs_started_at ON billing.reconciliation_runs USING btree (started_at DESC);

ALTER TABLE billing.reconciliation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.reconciliation_findings ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON billing.reconciliation_runs USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));

CREATE POLICY tenant_isolation ON billing.reconciliation_findings USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.reconciliation_runs TO openrails_app;

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE billing.reconciliation_findings TO openrails_app;
