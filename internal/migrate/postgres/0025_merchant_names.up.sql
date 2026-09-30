-- parent: 24 sha256:3db9238fbffeff5405330b666da95bdc5f05a39fae6c60443d491e5ca56b0db3
-- #1106: OpenRails owns merchant names. merchants.slug is the name of every
-- live merchant, bound to an AuthKit group or not; AuthKit groups are addressed
-- by id only. A rename keeps the former name as an alias that forwards to the
-- merchant and cannot be claimed by another one until it expires. A merchant
-- leaving the directory (deleted or retired) releases its name and aliases.
-- Every time is the database's now().
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

ALTER TABLE openrails.merchants ADD COLUMN slug_changed_at timestamp with time zone;

COMMENT ON COLUMN openrails.merchants.slug IS 'The merchant''s public name (#1106): unique among live rows and never equal to another merchant''s unexpired former name (openrails.merchant_slug_aliases). OpenRails owns it; AuthKit groups carry no name.';
COMMENT ON COLUMN openrails.merchants.permission_group_id IS 'The merchant''s own AuthKit permission-group id (#567): a merchant IS a top-level `merchant` group, child of `root`. Bare `text`, NO FK into the auth schema (#544 portability guard). NULL for a host-owned merchant without a control plane.';
COMMENT ON COLUMN openrails.merchants.slug_changed_at IS 'When the merchant was last renamed (#1106); NULL if never. The rename interval counts from here.';

DROP INDEX openrails.uq_merchants_unbound_slug;
CREATE UNIQUE INDEX uq_merchants_live_slug ON openrails.merchants USING btree (slug) WHERE (deleted_at IS NULL);

CREATE TABLE openrails.merchant_slug_aliases (
    slug text NOT NULL,
    merchant_id uuid NOT NULL,
    expires_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT merchant_slug_aliases_pkey PRIMARY KEY (slug),
    CONSTRAINT merchant_slug_aliases_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE
);

COMMENT ON TABLE openrails.merchant_slug_aliases IS '#1106: former merchant names. An unexpired alias forwards to its merchant and blocks every other claim of the name; expires_at NULL keeps it forever. Written by a rename, removed when its merchant takes the name back, when it expires and is claimed, or when its merchant leaves the directory.';

CREATE INDEX idx_merchant_slug_aliases_merchant ON openrails.merchant_slug_aliases USING btree (merchant_id);

-- One name namespace across live names and aliases. Claims serialize per name
-- on a transaction lock, so a rename's alias and a concurrent claim of the same
-- name cannot both win.
CREATE FUNCTION openrails.guard_merchant_name() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path TO 'pg_catalog', 'openrails'
    AS $$
BEGIN
    IF NEW.deleted_at IS NOT NULL THEN
        IF TG_OP = 'UPDATE' AND OLD.deleted_at IS NULL THEN
            DELETE FROM openrails.merchant_slug_aliases WHERE merchant_id = NEW.id;
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.deleted_at IS NULL THEN
        IF OLD.slug = NEW.slug THEN
            RETURN NEW;
        END IF;
        PERFORM pg_advisory_xact_lock(1106, hashtext(least(OLD.slug, NEW.slug)));
        PERFORM pg_advisory_xact_lock(1106, hashtext(greatest(OLD.slug, NEW.slug)));
    ELSE
        PERFORM pg_advisory_xact_lock(1106, hashtext(NEW.slug));
    END IF;
    DELETE FROM openrails.merchant_slug_aliases
     WHERE slug = NEW.slug AND (merchant_id = NEW.id OR expires_at <= now());
    IF EXISTS (SELECT 1 FROM openrails.merchant_slug_aliases WHERE slug = NEW.slug) THEN
        RAISE EXCEPTION 'merchant name % is another merchant''s former name', NEW.slug
            USING ERRCODE = '23505', CONSTRAINT = 'merchant_slug_aliases_pkey';
    END IF;
    RETURN NEW;
END;
$$;

COMMENT ON FUNCTION openrails.guard_merchant_name() IS '#1106: a live merchant name is never another merchant''s unexpired former name; leaving the directory releases a merchant''s former names. Raises 23505 on merchant_slug_aliases_pkey.';

REVOKE ALL ON FUNCTION openrails.guard_merchant_name() FROM PUBLIC;

CREATE TRIGGER guard_merchant_name BEFORE INSERT OR UPDATE OF slug, deleted_at ON openrails.merchants FOR EACH ROW EXECUTE FUNCTION openrails.guard_merchant_name();
