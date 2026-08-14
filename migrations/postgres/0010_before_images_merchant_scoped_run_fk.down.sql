-- Restore the globally addressed run FK. This weakens the tenant-provenance
-- invariant but does not reject rows created while the composite FK was active.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE ONLY openrails.destructive_run_before_images
    DROP CONSTRAINT destructive_run_before_images_run_fk;

ALTER TABLE ONLY openrails.destructive_run_before_images
    ADD CONSTRAINT destructive_run_before_images_run_fk
    FOREIGN KEY (destructive_run_id)
    REFERENCES openrails.destructive_runs(id)
    ON DELETE RESTRICT
    NOT VALID;

ALTER TABLE ONLY openrails.destructive_run_before_images
    VALIDATE CONSTRAINT destructive_run_before_images_run_fk;

ALTER TABLE ONLY openrails.destructive_runs
    DROP CONSTRAINT destructive_runs_merchant_id_id_key;
