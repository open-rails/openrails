-- or#902 / ID-11 follow-up: a before-image must belong to the same merchant as
-- the destructive run it cites. The old FK referenced only the globally unique
-- run ID, so a cross-merchant citation was structurally valid even though no
-- application path intended to create one.
--
-- destructive_runs.id is already globally unique, so adding (merchant_id, id)
-- cannot reject an existing parent row. Validating the replacement FK may
-- reject an existing cross-merchant before-image; that is intentional evidence
-- of broken provenance and must stop the migration rather than be rewritten.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

CREATE UNIQUE INDEX destructive_runs_merchant_id_id_key
    ON openrails.destructive_runs (merchant_id, id);

ALTER TABLE ONLY openrails.destructive_runs
    ADD CONSTRAINT destructive_runs_merchant_id_id_key
    UNIQUE USING INDEX destructive_runs_merchant_id_id_key;

ALTER TABLE ONLY openrails.destructive_run_before_images
    DROP CONSTRAINT destructive_run_before_images_run_fk;

ALTER TABLE ONLY openrails.destructive_run_before_images
    ADD CONSTRAINT destructive_run_before_images_run_fk
    FOREIGN KEY (merchant_id, destructive_run_id)
    REFERENCES openrails.destructive_runs(merchant_id, id)
    ON DELETE RESTRICT
    NOT VALID;
