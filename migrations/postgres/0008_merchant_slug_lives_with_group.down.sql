-- Revert or#914 slug-uniqueness scoping: restore the table-wide UNIQUE
-- constraint. Fails (23505) if a released slug has been re-claimed while a
-- soft-deleted row still holds it — resolve by renaming or purging the dead
-- row first; the collision is real data, not a migration bug.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DROP INDEX openrails.uq_merchants_slug;
ALTER TABLE openrails.merchants ADD CONSTRAINT uq_merchants_slug UNIQUE (slug);

COMMENT ON COLUMN openrails.merchants.slug IS 'Stable merchant slug used in merchant-scoped routes and resolution.';
COMMENT ON COLUMN openrails.merchants.permission_group_id IS 'The merchant''s own AuthKit permission-group id (#567): a merchant is a top-level `merchant` group, child of `root`. Bare `text`, NO FK into the auth schema (#544 portability guard). NULL in embedded (no control plane). Used to resolve a merchant from its authenticated group id.';
