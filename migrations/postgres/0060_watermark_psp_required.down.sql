SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DROP INDEX openrails.idx_rail_refresh_watermarks_psp;

ALTER TABLE openrails.rail_refresh_watermarks
    DROP CONSTRAINT rail_refresh_watermarks_psp_fk;

ALTER TABLE openrails.rail_refresh_watermarks
    ADD CONSTRAINT rail_refresh_watermarks_psp_fk
    FOREIGN KEY (psp_id) REFERENCES openrails.psps(id) ON DELETE SET NULL;

ALTER TABLE openrails.rail_refresh_watermarks
    DROP CONSTRAINT rail_refresh_watermarks_identity_key;

ALTER TABLE openrails.rail_refresh_watermarks
    ALTER COLUMN psp_id DROP NOT NULL;

ALTER TABLE openrails.rail_refresh_watermarks
    ADD CONSTRAINT rail_refresh_watermarks_psp_nonzero
    CHECK ((psp_id IS NULL) OR (psp_id <> '00000000-0000-0000-0000-000000000000'::uuid));

ALTER TABLE openrails.rail_refresh_watermarks
    ADD COLUMN psp_key uuid
    GENERATED ALWAYS AS (COALESCE(psp_id, '00000000-0000-0000-0000-000000000000'::uuid)) STORED;

ALTER TABLE openrails.rail_refresh_watermarks
    ADD CONSTRAINT rail_refresh_watermarks_identity_key
    UNIQUE (merchant_id, rail, psp_key, event_domain);

CREATE INDEX idx_rail_refresh_watermarks_account
    ON openrails.rail_refresh_watermarks USING btree (psp_id) WHERE (psp_id IS NOT NULL);

COMMENT ON COLUMN openrails.rail_refresh_watermarks.psp_id IS
    'Current PSP row when resolvable; NULL is the compatibility/global lane for providers without a bound account identity.';
