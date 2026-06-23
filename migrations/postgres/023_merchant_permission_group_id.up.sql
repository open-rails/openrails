-- #567: drop the org↔merchant 1:1 coupling and repurpose the ownership column.
--
-- Under the authkit permission-group model (authkit #111) OpenRails has NO `org`
-- persona: a merchant IS a top-level `merchant` permission-group (child of
-- `root`), created directly with no parent org. The old `owner_org_id` column
-- held the backing AuthKit org id under the #527 1:1 coupling; it now holds the
-- merchant's OWN permission-group id, so it is renamed `permission_group_id`.
--
-- This supersedes #527: the deterministic one-org-backs-one-merchant constraint
-- (uq_merchants_owner_org_id, migration 016) is gone — the column is no longer an
-- FK into an org namespace, just the merchant's self group id. Stays NULLABLE
-- (embedded has no control plane) and bare `text` with NO FK (the #544 cross-mode
-- portability guard: core billing tables must not reference the auth schema).

-- 1) Drop the #527 1:1 UNIQUE index (org↔merchant coupling is removed).
DROP INDEX IF EXISTS openrails.uq_merchants_owner_org_id;

-- 2) Rename the column to its repurposed meaning (the merchant's own group id).
ALTER TABLE openrails.merchants RENAME COLUMN owner_org_id TO permission_group_id;

-- 3) Create the lookup index on the repurposed column. (The old baseline
--    idx_merchants_owner_org_id was dropped by migration 016 — it replaced it
--    with the now-dropped uq_ unique index — so there is nothing to rename;
--    create a fresh non-unique index for merchant-by-group-id resolution.)
CREATE INDEX IF NOT EXISTS idx_merchants_permission_group_id
    ON openrails.merchants USING btree (permission_group_id)
    WHERE (permission_group_id IS NOT NULL);

-- 4) Refresh the column comment.
COMMENT ON COLUMN openrails.merchants.permission_group_id IS 'The merchant''s OWN authkit permission-group id (#567 — a merchant is a top-level `merchant` group, child of `root`, with no parent org; supersedes #527''s owner_org_id 1:1 coupling). Bare `text`, NO FK into the auth schema (#544 portability guard). NULL in embedded (no control plane). Used to resolve a merchant from its authenticated group id.';
