-- or#902 / ID-11 (was GAP-10, SEC-24): make the before-images identity unique
-- merchant-led.
--
-- 0001 shipped it as (destructive_run_id, table_name, row_id) — a UNIQUE that
-- spans merchants, on a table that is RLS-FORCED. Under RLS the conflicting row
-- is invisible to the inserting session, so a collision surfaces as a unique
-- violation naming a row the victim cannot select: indistinguishable from a bug
-- from the inside, and an existence oracle for whoever provoked it.
--
-- Reachability today is narrow, and worth stating honestly: destructive_runs.id
-- is a GLOBAL primary key, so two merchants cannot open the same run, and the
-- only way to land on the same key is a before-image pointing at another
-- merchant's run — which destructive_run_before_images_run_fk permits (it
-- references destructive_runs(id), not (merchant_id, id)) but no code path does.
-- That is why the exemption looked defensible. It is still the wrong shape: a
-- tenancy invariant on THIS table should not rest on another table's key
-- choice. Leading with merchant_id states the rule the table actually means —
-- one image per row per run, WITHIN a merchant — and makes it true regardless
-- of how run ids are minted.
--
-- Strictly weaker than what it replaces: the new key is the old key plus a
-- leading column, so every row set the 0001 index accepted the new one accepts.
-- No deployment can hold data that fails this.
--
-- idx_destructive_run_before_images_merchant_run goes with it, subsumed rather
-- than merely redundant: (merchant_id, destructive_run_id) is the new key's
-- leading prefix, and every read the reverse performs — restore, invalidate,
-- mark-restored, count — filters exactly those two columns. Keeping both would
-- cost a second index write on the capture, which is one INSERT per
-- subscription inside a mass enforce pass.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DROP INDEX openrails.idx_destructive_run_before_images_merchant_run;
DROP INDEX openrails.uq_destructive_run_before_images_identity;

CREATE UNIQUE INDEX uq_destructive_run_before_images_identity
    ON openrails.destructive_run_before_images USING btree (merchant_id, destructive_run_id, table_name, row_id);

COMMENT ON INDEX openrails.uq_destructive_run_before_images_identity IS 'ID-11: merchant-led. One image per (run, table, row) WITHIN a merchant — the second capture inside a run is the run''s own later write, not the state it inherited, and must never displace the first (the capture is ON CONFLICT DO NOTHING for that reason). Also serves the by-run reads of the reverse, so no separate (merchant_id, destructive_run_id) index is kept.';
