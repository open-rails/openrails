--
-- Fixed per-user-anchored budget windows (#337).
--
-- One row per (tenant, tenant_subject, actor, window_key) holding the window
-- anchor. Replaces the rolling-lookback semantics: budgets now reset at
-- knowable boundaries derived from each user's OWN first charged request, so
-- boundaries are naturally staggered across users (no global reset moment).
--
--   cadence = 'session': the window opens at the first charged request when no
--     window is active and closes exactly window_seconds later; the next window
--     opens on the next charged request (window_start is rewritten on reopen).
--   cadence = 'fixed': boundaries tick at anchor + k*window_seconds forever
--     (same wall-clock reset each period); window_start is derived from anchor
--     at read time and never rewritten.
--
-- The row is SELECT ... FOR UPDATE-locked inside Reserve so concurrent
-- reserves at a boundary serialize on it (the rolling engine had no such
-- serialization point).
--

CREATE TABLE openrails.budget_window_state (
    id uuid DEFAULT uuidv7() NOT NULL,
    tenant_id uuid NOT NULL,
    tenant_subject_id uuid CONSTRAINT budget_window_state_tenant_subject_id_not_null NOT NULL,
    actor text CONSTRAINT budget_window_state_actor_not_null NOT NULL,
    window_key text CONSTRAINT budget_window_state_window_key_not_null NOT NULL,
    cadence text NOT NULL,
    window_seconds bigint NOT NULL,
    anchor timestamp with time zone NOT NULL,
    window_start timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT budget_window_state_pkey PRIMARY KEY (id),
    CONSTRAINT budget_window_state_cadence_check CHECK ((cadence = ANY (ARRAY['session'::text, 'fixed'::text]))),
    CONSTRAINT budget_window_state_window_seconds_check CHECK ((window_seconds > 0)),
    CONSTRAINT budget_window_state_uniq UNIQUE (tenant_id, tenant_subject_id, actor, window_key)
);

ALTER TABLE ONLY openrails.budget_window_state
    ADD CONSTRAINT budget_window_state_tenant_subject_fk FOREIGN KEY (tenant_subject_id) REFERENCES openrails.tenant_subjects(id);

COMMENT ON TABLE openrails.budget_window_state IS 'Per-(tenant, tenant subject, actor, window_key) fixed-window anchor (#337). session: window_start rewritten on reopen; fixed: window_start derived from anchor. Locked FOR UPDATE in Reserve as the boundary-rollover serialization point.';
COMMENT ON COLUMN openrails.budget_window_state.anchor IS 'First-ever window open for this (subject, actor, window_key); fixed-cadence boundaries are anchor + k*window_seconds.';
COMMENT ON COLUMN openrails.budget_window_state.window_start IS 'Start of the most recently OPENED window. Authoritative for session cadence; for fixed cadence the current start is derived from anchor at read time.';

ALTER TABLE openrails.budget_window_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.budget_window_state FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON openrails.budget_window_state USING ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) WITH CHECK ((tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.budget_window_state TO openrails_app;

-- Aggregation support: Reserve/Check sum reservations since window_start for
-- one (tenant, subject, actor); the rolling engine relied on the same shape.
CREATE INDEX IF NOT EXISTS budget_reservations_window_agg_idx
    ON openrails.budget_reservations (tenant_id, tenant_subject_id, actor, created_at)
    WHERE status IN ('active', 'captured');
