-- or#914: merchant identity = authkit permission group — one slug, one team
-- system. The merchant's NAME is its permission group's instance slug
-- (ak#263/ak#264): claim arbitration, rename + tombstone forwarding, and
-- release-on-delete all live in the group namespace. The directory row keeps a
-- mirror of the slug for fast lookup and RLS-anchored billing state, but it no
-- longer PINS the name after the merchant is deleted: slug uniqueness applies
-- to LIVE rows only, so a name authkit released (DeleteGroup ReleaseSlug) or
-- tombstoned is arbitrated by authkit, not by a dead directory row. NOTE: a
-- soft-deleted row's restore can now collide with a live row that has since
-- claimed the slug — the restore fails loudly on this index, which is correct
-- (the name was released and re-claimed).

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE openrails.merchants DROP CONSTRAINT uq_merchants_slug;
CREATE UNIQUE INDEX uq_merchants_slug ON openrails.merchants (slug)
    WHERE deleted_at IS NULL;

COMMENT ON COLUMN openrails.merchants.slug IS 'Mirror of the merchant permission-group''s CURRENT instance slug (or#914): the group namespace is the naming authority — claim arbitration, renames (ak#264 tombstone forwarding) and release-on-delete happen there; this column is kept in sync for fast lookup (lazily re-synced after a rename) and is unique among LIVE rows only.';
COMMENT ON COLUMN openrails.merchants.permission_group_id IS 'The merchant''s own AuthKit permission-group id (#567/or#914): a merchant IS a top-level `merchant` group, child of `root`; the group is also the naming authority for the slug. Bare `text`, NO FK into the auth schema (#544 portability guard). NULL in embedded (no control plane). Used to resolve a merchant from its authenticated group id and from renamed slugs.';
