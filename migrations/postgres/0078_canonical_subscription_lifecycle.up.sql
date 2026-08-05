-- or#893 phase 2: ONE canonical local subscription lifecycle vocabulary.
--
-- The Go model has said it since #632: the status answers exactly one question,
-- "will we attempt to rebill this subscription?" — active/past_due mean yes,
-- unknown means we cannot tell yet, cancelled means never again, pending means
-- not yet started. Five states, and `cancel_type` carries WHY a cancelled row
-- is cancelled (user | merchant | expired | chargeback).
--
-- The enum still carried two states that predate that model and that no writer
-- produces:
--
--   'expired' — "the paid-through date passed". That is not a lifecycle state,
--     it is a clock reading, and as a TERMINAL state it directly contradicts the
--     NMI rebill doctrine: NMI rebills forever, a lapsed next_billing_date is
--     the normal state of every dunning customer, and gaps are forgiven. Only
--     PROVIDER ROSTER state may classify a subscription as dead. A row whose
--     date passed is `past_due` (we are still trying) or `unknown` (we cannot
--     tell) — never terminal. When the provider does confirm the subscription
--     is gone, the local row is `cancelled` with `cancel_type='expired'`, which
--     is where the word belongs and where the cancelled-row constraints
--     (cancelled_at, cancel_type, no retry schedule) actually bind.
--
--   'failed' — a payment outcome, not a subscription state. Payments have their
--     own `payment_status` enum; a subscription whose charge failed is
--     `past_due` or `cancelled`.
--
-- Remote provider vocabulary is untouched: reconcile.SubscriptionStatusExpired
-- still names what Stripe/CCBill/declared-import report, and the #665 decider
-- maps it onto TransitionCancel. This migration is about what LOCAL STORAGE may
-- hold.
--
-- Prelaunch hard cut: no dual-read lane, no alias. Any row still carrying a
-- retired value is backfilled to its canonical equivalent and counted out loud.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

-- ------------------------------------------------------------- backfill -----

-- 'expired'/'failed' both mean the same canonical thing: the row will never
-- rebill. cancel_type + cancelled_at + ended_at are what make that a legal
-- cancelled row (chk_cancelled_has_type / chk_cancelled_has_timestamp), and
-- chk_cancelled_no_retry_schedule forbids a live retry schedule on it.
-- ended_at >= cancelled_at is required by chk_ended_not_before_cancelled, so
-- cancelled_at is clamped to the existing ended_at when one is already recorded.
DO $$
DECLARE
    retired_subs bigint;
    retired_transitions bigint;
BEGIN
    WITH moved AS (
        UPDATE openrails.subscriptions SET
            status       = 'cancelled'::openrails.subscription_status,
            cancel_type  = COALESCE(cancel_type, 'expired'),
            cancelled_at = LEAST(
                COALESCE(cancelled_at, current_period_ends_at, ended_at, now()),
                COALESCE(ended_at, 'infinity'::timestamptz)),
            ended_at     = COALESCE(ended_at, current_period_ends_at, now()),
            next_retry_at = NULL,
            grace_ends_at = NULL,
            updated_at   = now()
        WHERE status::text IN ('expired', 'failed')
        RETURNING 1)
    SELECT count(*) INTO retired_subs FROM moved;

    WITH moved AS (
        UPDATE openrails.subscription_status_transitions SET
            from_status = CASE WHEN from_status::text IN ('expired', 'failed')
                               THEN 'cancelled'::openrails.subscription_status ELSE from_status END,
            to_status   = CASE WHEN to_status::text IN ('expired', 'failed')
                               THEN 'cancelled'::openrails.subscription_status ELSE to_status END
        WHERE from_status::text IN ('expired', 'failed')
           OR to_status::text IN ('expired', 'failed')
        RETURNING 1)
    SELECT count(*) INTO retired_transitions FROM moved;

    RAISE NOTICE 'or#893 phase 2: % subscription row(s) and % status-transition row(s) carried a retired lifecycle value (expired/failed) and were canonicalised to cancelled',
        retired_subs, retired_transitions;
END $$;

-- ------------------------------------------------------------ type swap -----

-- Swapping an enum type means every stored expression that mentions it —
-- partial-index predicates, CHECK constraints, view bodies — has to be reparsed
-- against the new type OID. Postgres will not do that itself, so they are
-- captured as SOURCE TEXT, dropped, and recreated verbatim afterwards. Captured
-- rather than restated: this migration owns the lifecycle vocabulary, and a
-- hand-copied predicate would silently rot the day one of them changes.
--
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

ALTER TYPE openrails.subscription_status RENAME TO subscription_status_retired;

CREATE TYPE openrails.subscription_status AS ENUM (
    'pending',
    'active',
    'past_due',
    'cancelled',
    'unknown'
);

COMMENT ON TYPE openrails.subscription_status IS
    'or#893: the canonical LOCAL subscription lifecycle. One question: will we attempt to rebill? pending = not started; active/past_due = yes; unknown = provider must tell us (#632); cancelled = never again, with cancel_type carrying why (user|merchant|expired|chargeback). Provider vocabulary is mapped onto this set at the boundary — a remote "expired" becomes cancelled/cancel_type=expired, never a local status.';

-- #733's audit trigger is declared UPDATE OF status, which pins the column.
DROP TRIGGER trg_subscriptions_status_transition ON openrails.subscriptions;

ALTER TABLE openrails.subscriptions ALTER COLUMN status DROP DEFAULT;
-- The type change is the point of this migration and no shape avoids the
-- rewrite: Postgres cannot remove an enum label, so the column has to move to a
-- new type. Every clause squawk warns about — the ACCESS EXCLUSIVE lock, the
-- table rewrite, breaking clients that read the column — is either unavoidable
-- or intended. Prelaunch, no deployment holds rows, and a client still writing
-- 'expired' MUST break: that is the invariant being installed.
ALTER TABLE openrails.subscriptions
    -- squawk-ignore changing-column-type
    ALTER COLUMN status TYPE openrails.subscription_status
    USING status::text::openrails.subscription_status;
ALTER TABLE openrails.subscriptions
    ALTER COLUMN status SET DEFAULT 'pending'::openrails.subscription_status;

CREATE TRIGGER trg_subscriptions_status_transition
    AFTER INSERT OR UPDATE OF status ON openrails.subscriptions
    FOR EACH ROW EXECUTE FUNCTION openrails.subscriptions_record_status_transition();

-- Same rationale as `subscriptions.status` above.
ALTER TABLE openrails.subscription_status_transitions
    -- squawk-ignore changing-column-type
    ALTER COLUMN from_status TYPE openrails.subscription_status
    USING from_status::text::openrails.subscription_status;
ALTER TABLE openrails.subscription_status_transitions
    -- squawk-ignore changing-column-type
    ALTER COLUMN to_status TYPE openrails.subscription_status
    USING to_status::text::openrails.subscription_status;

DROP TYPE openrails.subscription_status_retired;

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
