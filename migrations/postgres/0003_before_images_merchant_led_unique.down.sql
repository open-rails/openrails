-- Restore the 0001 index pair: the cross-merchant unique identity plus the
-- separate (merchant_id, destructive_run_id) lookup index it subsumed.
--
-- This can fail, and that is correct. Going back re-narrows the key, so it
-- rejects any row pair two merchants legitimately created while 0003 was
-- applied (same run id, same table, same row, different merchants). There is
-- nothing to reconcile automatically — the down migration is the statement that
-- the cross-merchant unique is back, and a deployment holding rows that need it
-- gone should not silently lose one.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DROP INDEX openrails.uq_destructive_run_before_images_identity;

CREATE UNIQUE INDEX uq_destructive_run_before_images_identity
    ON openrails.destructive_run_before_images USING btree (destructive_run_id, table_name, row_id);

CREATE INDEX idx_destructive_run_before_images_merchant_run
    ON openrails.destructive_run_before_images USING btree (merchant_id, destructive_run_id);
