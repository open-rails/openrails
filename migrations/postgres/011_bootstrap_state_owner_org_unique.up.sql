-- #527: bootstrap redesign — first-run marker + DB-enforced 1:1 merchant<->org.
--
-- bootstrap_state is a singleton marker: startup provisioning applies the
-- bootstrap manifest only on the FIRST boot (when this table is empty) and
-- records completion here. Reboots of a provisioned deployment skip the apply
-- entirely, so a stale/malformed manifest can never brick a restart. Manual
-- re-apply is `openrails push-merchant-config`.
CREATE TABLE openrails.bootstrap_state (
    singleton  boolean NOT NULL DEFAULT true,
    applied_at timestamp with time zone NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT bootstrap_state_pkey PRIMARY KEY (singleton),
    CONSTRAINT bootstrap_state_singleton CHECK (singleton)
);

-- One merchant <-> one backing org (#527). owner_org_id stays NULLABLE (embedded
-- without AuthKit, and merchant-less orgs, leave it NULL — Postgres permits many
-- NULLs under a partial unique index), but is now UNIQUE on non-null values so
-- the issuer->org->merchant resolution is a deterministic 1:1, not a convention.
DROP INDEX IF EXISTS openrails.idx_merchants_owner_org_id;
CREATE UNIQUE INDEX uq_merchants_owner_org_id
    ON openrails.merchants USING btree (owner_org_id)
    WHERE (owner_org_id IS NOT NULL);

COMMENT ON COLUMN openrails.merchants.owner_org_id IS 'OWNERSHIP FK to the AuthKit org that backs this merchant (#527). One merchant <-> one backing org (UNIQUE on non-null). NULL in embedded (no AuthKit). Injective, not bijective: most orgs have no merchant.';
