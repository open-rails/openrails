--
-- Persisted tier SCHEDULE + auto-graduation by cumulative spend (#476).
--
-- Today graduation is HOST-CRANKED: GraduateTier(payer, ladder) (#298) makes the
-- host pass the ladder AND call it on every spend event. That's wrong
-- ergonomically — the ladder is a SCHEDULE that changes rarely; graduation should
-- be OpenRails' job, driven off the spend it already records.
--
-- A tier_schedule is the ordered ladder declared ONCE per tenant (or per
-- tenant-subject as an override): rungs = [{tier, min_cumulative_paid_micros}],
-- ascending by min_cumulative_paid_micros. OpenRails then auto-maintains each
-- payer's money_accounts.tier from cumulative_paid vs this schedule on the
-- deposit/settlement path (eventful), monotonic (never regresses); an explicit
-- admin tier override still wins (money_accounts.tier_source='admin').
--
-- OWNER mirrors the #473 budget-policy owner split: owner='platform' rows are set
-- by US (the platform) and the subject can neither see nor loosen them. (Only
-- owner='platform' is written today; the column keeps the door open for a
-- subject-declared schedule without a migration.)
--
-- SCOPE: merchant_subject_id NULL = the tenant-wide default schedule (the common
-- case — one ladder for the whole tenant). A non-NULL merchant_subject_id is a
-- per-subject override that takes precedence for that subject.
--
-- rungs is a JSONB array of {tier, min_cumulative_paid_micros}.
--

CREATE TABLE IF NOT EXISTS openrails.tier_schedules (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL DEFAULT COALESCE((NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid, '00000000-0000-0000-0000-000000000001'::uuid),
    merchant_subject_id uuid,
    owner text NOT NULL DEFAULT 'platform',
    rungs jsonb DEFAULT '[]'::jsonb NOT NULL,
    schedule_version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tier_schedules_pkey PRIMARY KEY (id),
    CONSTRAINT tier_schedules_owner_check CHECK ((owner = ANY (ARRAY['platform'::text, 'subject'::text])))
);

-- One schedule per (tenant, scope, owner): a per-subject override and the
-- tenant-wide default (merchant_subject_id IS NULL) coexist. Partial unique
-- indexes because NULL is not equal to NULL in a multi-column UNIQUE.
CREATE UNIQUE INDEX uq_tier_schedules_merchant_default ON openrails.tier_schedules (merchant_id, owner)
    WHERE (merchant_subject_id IS NULL);
CREATE UNIQUE INDEX uq_tier_schedules_merchant_subject ON openrails.tier_schedules (merchant_id, merchant_subject_id, owner)
    WHERE (merchant_subject_id IS NOT NULL);

ALTER TABLE ONLY openrails.tier_schedules
    ADD CONSTRAINT tier_schedules_merchant_subject_fk FOREIGN KEY (merchant_subject_id) REFERENCES openrails.merchant_subjects(id);

COMMENT ON TABLE openrails.tier_schedules IS 'Persisted tier ladder (#476): rungs[{tier,min_cumulative_paid_micros}] declared once per tenant (or per tenant-subject override). OpenRails auto-maintains money_accounts.tier from cumulative_paid vs this schedule on the deposit path; admin tier override (tier_source=admin) still wins.';
COMMENT ON COLUMN openrails.tier_schedules.owner IS 'platform (set by us; subject cannot edit/see) | subject. Mirrors the #473 budget-policy owner split.';
COMMENT ON COLUMN openrails.tier_schedules.merchant_subject_id IS 'NULL = tenant-wide default schedule; non-NULL = per-subject override taking precedence for that subject.';
COMMENT ON COLUMN openrails.tier_schedules.rungs IS 'Ordered JSONB array of {tier, min_cumulative_paid_micros}; a payer''s tier = highest rung whose min_cumulative_paid_micros <= cumulative_paid.';

ALTER TABLE openrails.tier_schedules ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.tier_schedules FORCE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.tier_schedules USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.tier_schedules TO openrails_app;

--
-- tier_source distinguishes an AUTO-graduated tier (schedule-driven) from an
-- explicit ADMIN override on money_accounts.tier. Auto-graduation only writes
-- when tier_source <> 'admin', so an admin override (#298) always wins.
--
ALTER TABLE openrails.money_accounts
    ADD COLUMN IF NOT EXISTS tier_source text NOT NULL DEFAULT 'auto';

ALTER TABLE openrails.money_accounts
    DROP CONSTRAINT IF EXISTS money_accounts_tier_source_chk;
ALTER TABLE openrails.money_accounts
    ADD CONSTRAINT money_accounts_tier_source_chk CHECK ((tier_source = ANY (ARRAY['auto'::text, 'admin'::text])));

COMMENT ON COLUMN openrails.money_accounts.tier_source IS 'auto = tier maintained by the tier_schedule auto-graduation (#476); admin = explicit override that auto-graduation must not overwrite.';
