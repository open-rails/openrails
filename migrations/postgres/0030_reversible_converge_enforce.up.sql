-- or#859 tier 1, slice 2: converge-enforce becomes a reversible destructive run.
--
-- or#858 made `--prune` reversible, but its machinery is DELETE-shaped:
-- `deleted_at` + a run stamp, undone by `SET deleted_at = NULL`. The incident
-- that motivated or#859 is not a delete. An empty NMI roster drove the enforce
-- pass to CANCEL 40/40 subscriptions — an UPDATE that overwrote status,
-- ended_at, cancelled_at, the grace/retry schedule and the period bounds, and
-- queued deferred NMI vault deletes behind it. A tombstone cannot undo that:
-- the row was never removed, its prior VALUES were.
--
-- So this migration adds the one thing the tree had nowhere (grep for a
-- prior-state column returns nothing): a place to keep the row as it was.
--
--   destructive_run_before_images   one row per (run, table, row): the row
--                                   verbatim, before the run touched it.
--   rail_intents.destructive_run_id which run enqueued this provider write, so
--                                   the reverse can supersede the unfired ones
--                                   and MANIFEST the ones that already fired.
--
-- Why a generic side table and not `prior_status` / `prior_ended_at` / … on
-- subscriptions:
--
--   * it is one shape for six declared run kinds, not thirteen columns per
--     table re-added for each new one;
--   * per-table columns can hold exactly ONE prior state, so a second run over
--     the same row silently overwrites the first run's undo information —
--     UNIQUE (run, table, row) here holds one image per row PER RUN and the
--     FK pins each image to exactly one run, which is what "attributable"
--     means;
--   * storage is proportional to damage, not to records on file (Paul's
--     standing law): a merchant that never suffers a bad pass stores nothing,
--     where prior-state columns widen the hottest table in the book forever;
--   * the image is `to_jsonb(row)` taken server-side, so it cannot drift from
--     the table definition the way a hand-maintained Go struct would.
--
-- The jsonb is captured VERBATIM (complete forensic evidence), but restoration
-- reads an explicit typed column projection out of it — see
-- RestoreSubscriptionsFromBeforeImages. A blind whole-row rewrite would
-- resurrect columns this run never touched and re-assert identity/FK columns
-- that other planes legitimately advanced; the undo's job is to put back what
-- the run took, not to re-stamp the row wholesale.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

CREATE TABLE openrails.destructive_run_before_images (
    id uuid DEFAULT uuidv7() NOT NULL PRIMARY KEY,
    merchant_id uuid NOT NULL,
    destructive_run_id uuid NOT NULL,
    -- The table the row lives in. Deliberately a small closed set: a
    -- before-image is only useful where a restore path exists for it, and an
    -- unrestorable image is a promise the undo cannot keep.
    table_name text NOT NULL,
    row_id uuid NOT NULL,
    -- to_jsonb(row) as it stood immediately before the run's write.
    before jsonb NOT NULL,
    captured_at timestamp with time zone DEFAULT now() NOT NULL,
    -- Set by the reverse. An image with restored_at IS NULL after a reversal is
    -- one the undo deliberately did NOT replay (entitlements — see below).
    restored_at timestamp with time zone,
    CONSTRAINT chk_destructive_run_before_images_table CHECK ((table_name = ANY (ARRAY['subscriptions'::text, 'entitlements'::text])))
);

COMMENT ON TABLE openrails.destructive_run_before_images IS 'or#859 tier 1: the row as it stood immediately before a destructive run overwrote it. or#858''s soft-delete stamp reverses DELETEs; this reverses UPDATEs — which is the damage the empty-roster mass-cancellation actually did. One image per (run, table, row); FK-pinned to exactly one run.';
COMMENT ON COLUMN openrails.destructive_run_before_images.before IS 'to_jsonb(row) verbatim, captured server-side inside the run. Complete evidence; the restore reads an explicit typed column projection out of it rather than rewriting the whole row.';
COMMENT ON COLUMN openrails.destructive_run_before_images.restored_at IS 'When the reverse replayed this image. NULL after a completed reversal means the image was captured as evidence but deliberately never replayed: entitlement rows are RECOMPUTED from the append-only grant log by Converge, never restored (or#859 §3.3 / Class D). Restoring one directly could make it disagree with its grant, which recomputation cannot.';

ALTER TABLE ONLY openrails.destructive_run_before_images
    ADD CONSTRAINT destructive_run_before_images_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY openrails.destructive_run_before_images
    ADD CONSTRAINT destructive_run_before_images_run_fk FOREIGN KEY (destructive_run_id) REFERENCES openrails.destructive_runs(id) ON DELETE RESTRICT;

-- Exactly one image per row per run: the second capture inside a run is the
-- run's own later write, not the state it inherited, and must never displace
-- the first (the capture is ON CONFLICT DO NOTHING for that reason).
--
-- Led by merchant_id (GAP-10 / SEC-24): under RLS a conflicting row belonging to
-- another merchant is INVISIBLE, so a unique index that spans merchants lets one
-- merchant's row block another's insert with an error naming nothing. It also
-- serves the by-run lookups the reverse does, so no second index is needed.
CREATE UNIQUE INDEX uq_destructive_run_before_images_identity
    ON openrails.destructive_run_before_images USING btree (merchant_id, destructive_run_id, table_name, row_id);

ALTER TABLE openrails.destructive_run_before_images ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.destructive_run_before_images FORCE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.destructive_run_before_images USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

-- The undo information is itself append-only: no DELETE, and UPDATE only on
-- restored_at. An operator who can edit the before-image can rewrite what the
-- undo will put back, which is strictly worse than having no undo at all.
GRANT SELECT,INSERT ON TABLE openrails.destructive_run_before_images TO openrails_app;
GRANT UPDATE (restored_at) ON TABLE openrails.destructive_run_before_images TO openrails_app;

-- --- intent attribution -------------------------------------------------------
--
-- Stamping WHICH run queued an outbound provider write is what lets the reverse
-- neutralise the ones that have not fired. `superseded` is an existing FORWARD
-- status in the rail_intents lifecycle, so this is a normal transition, not a
-- rollback of an append-only log — and an intent that already fired is
-- irreversible divergence that the reverse must REPORT, never silently count as
-- undone. The column is attribution only; nothing here weakens the ledger.
ALTER TABLE openrails.rail_intents
    ADD COLUMN destructive_run_id uuid;

COMMENT ON COLUMN openrails.rail_intents.destructive_run_id IS 'or#859: the destructive run whose pass enqueued this intent. The reverse of that run supersedes the ones still pending/failed_retryable and reports the rest — succeeded ones as irreversible provider-side divergence, in_flight/unknown_needs_verify ones as ambiguous. Attribution only: never cleared, never used to delete a row.';

-- The referencing column was added NULL one statement ago, so the validating
-- scan is over an all-NULL column; NOT VALID would only defer a no-op. (Same
-- reasoning as 0018's four run FKs — the rule stays armed for a new FK on a
-- column that already carries data.)
ALTER TABLE ONLY openrails.rail_intents
    -- squawk-ignore adding-foreign-key-constraint, constraint-missing-not-valid
    ADD CONSTRAINT rail_intents_destructive_run_fk FOREIGN KEY (destructive_run_id) REFERENCES openrails.destructive_runs(id) ON DELETE RESTRICT;

-- Only a vanishing fraction of intents ever carry a run stamp — partial, so the
-- cost is proportional to destructive passes, not to intent volume.
CREATE INDEX idx_rail_intents_destructive_run ON openrails.rail_intents USING btree (destructive_run_id) WHERE (destructive_run_id IS NOT NULL);

-- --- run kinds -----------------------------------------------------------------
--
-- destructive_runs.kind is deliberately unconstrained text (0018), so
-- 'converge_enforce' needs no DDL. It does need to be sayable in the run
-- comment so the next reader knows the ledger has a second user.
COMMENT ON TABLE openrails.destructive_runs IS 'or#858/or#859 tier 1: every destructive operation is an attributable, scoped, stamped unit of damage with a single-command undo. kind=prune stamps rows it soft-deleted (destructive_run_id on the row); kind=converge_enforce captures before-images of the rows it OVERWROTE plus the provider intents it queued. Both reverse with `openrails prune rollback --run <id>`, which dispatches on kind. declared_import / plan_migration / catalog_push / merchant_delete are declared and not yet converted.';
