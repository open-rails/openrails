--
-- Hierarchical money budget policies (#473).
--
-- Generalizes per-tier money budgets into {scope, owner, windows[]} rows so the
-- payer-protection HIERARCHY (tensorhub #475) can be expressed and composed in
-- ONE admit verdict over the ONE payer balance:
--
--   scope='subject', owner='platform'  -> platform->payer cap (set by US; the
--       payer can neither see nor loosen it). Keyed on (subject).
--   scope='subject', owner='subject'   -> the subject's self-cap on its total.
--   scope='role',    owner='subject'   -> a shared pool for all users holding a
--       role. Keyed on (subject, role_uuid).
--   scope='actor',   owner='subject'   -> per-invoker cap. Keyed on
--       (subject, actor). (Pre-#473 this lived in tier_policies.budget_windows;
--       that path is preserved — this table is ADDITIVE.)
--
-- The OWNER discriminator is the write-authz split: platform-owned rows are
-- writable only via the platform path (SetPlatformBudgetPolicy), never via the
-- subject's tier policy doc; subject-owned rows are writable by the subject. The
-- admit path reads ALL rows for a (subject) regardless of owner and reserves
-- against every matching scope.
--
-- IDENTITY: scope_key holds the immutable role uuid (scope='role') as text, or
-- the actor string (scope='actor'); it is empty for scope='subject' (the subject
-- is already tenant_subject_id). Never a slug/name (#475/#476 convention).
--
-- windows is a JSONB array of {key, window_seconds, limit_micros, cadence},
-- the same BudgetWindowPolicy shape tier_policies.budget_windows uses.
--

CREATE TABLE IF NOT EXISTS openrails.budget_policies (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid NOT NULL DEFAULT COALESCE((NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid, '00000000-0000-0000-0000-000000000001'::uuid),
    tenant_subject_id uuid CONSTRAINT budget_policies_tenant_subject_id_not_null NOT NULL,
    scope text NOT NULL,
    owner text NOT NULL,
    scope_key text NOT NULL DEFAULT '',
    windows jsonb DEFAULT '[]'::jsonb NOT NULL,
    policy_version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT budget_policies_pkey PRIMARY KEY (id),
    CONSTRAINT budget_policies_scope_check CHECK ((scope = ANY (ARRAY['subject'::text, 'actor'::text, 'role'::text]))),
    CONSTRAINT budget_policies_owner_check CHECK ((owner = ANY (ARRAY['platform'::text, 'subject'::text]))),
    CONSTRAINT budget_policies_uniq UNIQUE (tenant_id, tenant_subject_id, scope, owner, scope_key)
);

ALTER TABLE ONLY openrails.budget_policies
    ADD CONSTRAINT budget_policies_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES openrails.tenant_subjects(id);

COMMENT ON TABLE openrails.budget_policies IS 'Hierarchical money-budget policies (#473): {scope, owner, windows[]} composed in one admit verdict over the one payer balance. owner=platform rows are writable only via the platform path (the subject cannot see/loosen them); owner=subject rows are the subject''s own caps.';
COMMENT ON COLUMN openrails.budget_policies.owner IS 'platform (set by us; subject cannot edit/see) | subject (the subject''s own cap). The write-authz split.';
COMMENT ON COLUMN openrails.budget_policies.scope_key IS 'Immutable scope discriminator: role uuid (scope=role) or actor string (scope=actor); empty for scope=subject. Never a slug/name.';

ALTER TABLE openrails.budget_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.budget_policies FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON openrails.budget_policies USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.budget_policies TO openrails_app;
