-- Structural reverse of 0078: the retired lifecycle values become
-- representable again. Rows are NOT moved back — the backfill was lossy on
-- purpose (a cancelled row carries cancel_type/cancelled_at that 'expired'
-- never did), and re-deriving "this one used to say expired" would be a guess.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

-- The capture/drop pass runs inside a DO block on purpose: it reads the system
-- catalogs, and sqlc's static schema parser (which reads every migration to
-- build its catalog) does not model pg_class/pg_constraint. A DO block is
-- opaque to it and transparent to Postgres, which is exactly the split needed —
-- the type statements below stay top-level so sqlc DOES see the new enum.
DO $$
DECLARE
    r record;
BEGIN
    CREATE TEMP TABLE or893_saved_views ON COMMIT DROP AS
    SELECT c.relname::text AS name,
           pg_get_viewdef(c.oid, true) AS def,
           obj_description(c.oid, 'pg_class') AS comment
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'openrails'
      AND c.relname IN ('freeloader_episodes', 'orphaned_episodes');

    -- Every index and CHECK constraint that touches the enum: either it depends
    -- on the type outright (a predicate naming a label) or it constrains a
    -- column of that type (`from_status IS DISTINCT FROM to_status` names no
    -- label but is compiled against the type all the same). Both legs are
    -- needed; each alone misses real objects.
    CREATE TEMP TABLE or893_saved_indexes ON COMMIT DROP AS
    SELECT c.relname::text AS name, pg_get_indexdef(c.oid) AS def
    FROM pg_class c
    JOIN pg_index ix ON ix.indexrelid = c.oid
    WHERE c.relkind = 'i'
      AND NOT EXISTS (SELECT 1 FROM pg_constraint pc WHERE pc.conindid = c.oid)
      AND (c.oid IN (SELECT d.objid FROM pg_depend d
                     WHERE d.classid = 'pg_class'::regclass AND d.objsubid = 0
                       AND d.refclassid = 'pg_type'::regclass
                       AND d.refobjid = 'openrails.subscription_status'::regtype)
           OR EXISTS (SELECT 1 FROM pg_attribute a
                      WHERE a.attrelid = ix.indrelid
                        AND a.attnum = ANY (ix.indkey::smallint[])
                        AND a.atttypid = 'openrails.subscription_status'::regtype));

    CREATE TEMP TABLE or893_saved_checks ON COMMIT DROP AS
    SELECT co.conrelid::regclass::text AS tbl, co.conname::text AS name,
           pg_get_constraintdef(co.oid) AS def
    FROM pg_constraint co
    WHERE co.contype = 'c'
      AND EXISTS (SELECT 1 FROM unnest(co.conkey) AS k(attnum)
                  JOIN pg_attribute a ON a.attrelid = co.conrelid AND a.attnum = k.attnum
                  WHERE a.atttypid = 'openrails.subscription_status'::regtype);

    DROP VIEW openrails.orphaned_episodes;
    DROP VIEW openrails.freeloader_episodes;

    FOR r IN SELECT tbl, name FROM or893_saved_checks LOOP
        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', r.tbl, r.name);
    END LOOP;
    FOR r IN SELECT name FROM or893_saved_indexes LOOP
        EXECUTE format('DROP INDEX openrails.%I', r.name);
    END LOOP;
END $$;

ALTER TYPE openrails.subscription_status RENAME TO subscription_status_canonical;

CREATE TYPE openrails.subscription_status AS ENUM (
    'pending',
    'active',
    'expired',
    'cancelled',
    'failed',
    'past_due',
    'unknown'
);

DROP TRIGGER trg_subscriptions_status_transition ON openrails.subscriptions;

ALTER TABLE openrails.subscriptions ALTER COLUMN status DROP DEFAULT;
ALTER TABLE openrails.subscriptions
    ALTER COLUMN status TYPE openrails.subscription_status
    USING status::text::openrails.subscription_status;
ALTER TABLE openrails.subscriptions
    ALTER COLUMN status SET DEFAULT 'pending'::openrails.subscription_status;

CREATE TRIGGER trg_subscriptions_status_transition
    AFTER INSERT OR UPDATE OF status ON openrails.subscriptions
    FOR EACH ROW EXECUTE FUNCTION openrails.subscriptions_record_status_transition();

ALTER TABLE openrails.subscription_status_transitions
    ALTER COLUMN from_status TYPE openrails.subscription_status
    USING from_status::text::openrails.subscription_status;
ALTER TABLE openrails.subscription_status_transitions
    ALTER COLUMN to_status TYPE openrails.subscription_status
    USING to_status::text::openrails.subscription_status;

DROP TYPE openrails.subscription_status_canonical;

DO $$
DECLARE
    r record;
    v record;
BEGIN
    FOR r IN SELECT def FROM or893_saved_indexes LOOP
        EXECUTE r.def;
    END LOOP;
    FOR r IN SELECT tbl, name, def FROM or893_saved_checks LOOP
        EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I %s', r.tbl, r.name, r.def);
    END LOOP;
    FOR v IN SELECT name, def, comment FROM or893_saved_views ORDER BY name LOOP
        EXECUTE format('CREATE VIEW openrails.%I WITH (security_invoker=%L) AS %s',
                       v.name, 'true', v.def);
        IF v.comment IS NOT NULL THEN
            EXECUTE format('COMMENT ON VIEW openrails.%I IS %L', v.name, v.comment);
        END IF;
        EXECUTE format('GRANT SELECT ON TABLE openrails.%I TO openrails_app', v.name);
    END LOOP;
END $$;
