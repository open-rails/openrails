-- #836: a DB-backed kill switch for destructive convergence, plus the #835
-- per-merchant first-enforce gate.
--
-- Before this, an operator watching a mass cancellation at 3am had no way to
-- stop it short of killing the process: `provider_write_mode: readonly` is
-- config/env only (no SIGHUP, no config watch), River QueuePause is unwired,
-- and the #679 volume breaker only holds provider deletes — never the LOCAL
-- cancel and entitlement revoke that actually takes a customer's access away.
--
-- Two tables because they are two different things, and the schema doctrine is
-- explicit that RLS-exempt tables must not carry tenant data:
--
--   destructive_action_switch   — the INSTANCE emergency stop. One row, no
--     merchant_id, RLS-exempt, readable from the no-GUC background connections
--     the intent runner and the sweep scheduler use. A kill switch scoped by
--     the very connection it is meant to police is not a kill switch. DEFAULT
--     SAFE: it ships disabled, so a fresh deployment converges nothing
--     destructive until an operator arms it.
--
--   merchant_destructive_policy — per-merchant tenant data, RLS-protected as
--     usual. `destructive_actions_enabled` is the per-merchant stop;
--     `enforce_armed_at` is the #835 first-enforce gate: until an operator sets
--     it, that merchant's provider pull runs ADVISORY — it persists findings
--     and mutates nothing. A legacy NMI book imported into a new deployment is
--     therefore surveyed on first boot, not cancelled within seconds of it.

CREATE TABLE openrails.destructive_action_switch (
    id uuid DEFAULT uuidv7() NOT NULL,
    singleton boolean DEFAULT true NOT NULL,
    enabled boolean DEFAULT false NOT NULL,
    updated_by text,
    reason text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_destructive_action_switch_singleton CHECK ((singleton = true))
);

COMMENT ON TABLE openrails.destructive_action_switch IS 'RLS-exempt by design: instance-level operator kill switch for destructive convergence (#836), not tenant data. One row. Read from the no-GUC background connections the intent runner and sweep scheduler use, so it cannot be defeated by the connection scope it polices. Default disabled: a fresh deployment cancels nothing until an operator arms it.';

ALTER TABLE ONLY openrails.destructive_action_switch
    ADD CONSTRAINT destructive_action_switch_pkey PRIMARY KEY (id);

CREATE UNIQUE INDEX uq_destructive_action_switch_singleton
    ON openrails.destructive_action_switch USING btree (singleton);

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.destructive_action_switch TO openrails_app;

INSERT INTO openrails.destructive_action_switch (enabled, reason)
VALUES (false, 'default safe (#836): arm deliberately once the first pull''s findings have been reviewed');

CREATE TABLE openrails.merchant_destructive_policy (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    destructive_actions_enabled boolean DEFAULT true NOT NULL,
    enforce_armed_at timestamp with time zone,
    first_pull_completed_at timestamp with time zone,
    updated_by text,
    reason text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY openrails.merchant_destructive_policy FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.merchant_destructive_policy IS '#836/#835 per-merchant destructive-action policy: destructive_actions_enabled is the per-merchant emergency stop (the instance switch in destructive_action_switch gates it globally); enforce_armed_at is the first-enforce gate — NULL means the merchant''s provider pull runs advisory (findings only, zero mutations) until an operator reviews the first pull and arms it.';

COMMENT ON COLUMN openrails.merchant_destructive_policy.enforce_armed_at IS '#835: NULL = advisory-only pulls for this merchant. Absence of a row is the same as NULL, so a newly onboarded merchant is surveyed before it is enforced.';

ALTER TABLE ONLY openrails.merchant_destructive_policy
    ADD CONSTRAINT merchant_destructive_policy_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.merchant_destructive_policy
    ADD CONSTRAINT merchant_destructive_policy_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE UNIQUE INDEX uq_merchant_destructive_policy_merchant
    ON openrails.merchant_destructive_policy USING btree (merchant_id);

ALTER TABLE openrails.merchant_destructive_policy ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.merchant_destructive_policy USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.merchant_destructive_policy TO openrails_app;
