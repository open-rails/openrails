--
-- Tenant-wide DEFAULT tier policy (#477).
--
-- A tier POLICY (throughput windows + queue limits + entitled resources + money
-- budget windows) was per-(payer, tier) only — merchant_subject_id NOT NULL. But a
-- platform CAPACITY ladder (#477) is declared ONCE for the whole tenant, exactly
-- like the #476 tier SCHEDULE: one set of per-tier limits applies to every payer,
-- selected by the payer's auto-graduated tier. Forcing the host to materialize a
-- per-payer policy row for every payer defeats "declare once".
--
-- So allow merchant_subject_id NULL = the tenant-wide default policy for a tier
-- (the common case for the platform ladder); a non-NULL merchant_subject_id stays a
-- per-subject override that takes precedence. Mirrors tier_schedules (#476).
--

ALTER TABLE openrails.tier_policies
    DROP CONSTRAINT IF EXISTS tier_policies_merchant_subject_id_not_null;

ALTER TABLE openrails.tier_policies
    ALTER COLUMN merchant_subject_id DROP NOT NULL;

-- The original table has a (merchant_id, merchant_subject_id, tier) uniqueness the
-- UpsertTierPolicy ON CONFLICT targets. With a nullable merchant_subject_id, NULL
-- is not equal to NULL in a multi-column UNIQUE, so split into two partial unique
-- indexes: the tenant-wide default (NULL subject) and the per-subject override.
DROP INDEX IF EXISTS openrails.uq_tier_policies;

CREATE UNIQUE INDEX IF NOT EXISTS uq_tier_policies_merchant_default ON openrails.tier_policies (merchant_id, tier)
    WHERE (merchant_subject_id IS NULL);
CREATE UNIQUE INDEX IF NOT EXISTS uq_tier_policies_merchant_subject ON openrails.tier_policies (merchant_id, merchant_subject_id, tier)
    WHERE (merchant_subject_id IS NOT NULL);

COMMENT ON COLUMN openrails.tier_policies.merchant_subject_id IS 'NULL = tenant-wide default tier policy (#477 platform capacity ladder, declared once); non-NULL = per-subject override taking precedence for that subject.';
