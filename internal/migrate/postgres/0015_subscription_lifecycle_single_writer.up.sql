-- parent: 13 sha256:18afceee7ee689bbc33dfdf23b9072976b40b57c54fabc1bb4cd5617cb6e060c
-- #1091 part C: a subscription's lifecycle fields (status, paid period,
-- cancellation) change only in a lifecycle decision, which advances
-- lifecycle_rev by exactly one against the revision it read. Any other write
-- that would change them is refused, so a stale full-row update can no longer
-- silently revert a decision made meanwhile.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

ALTER TABLE openrails.subscriptions ADD COLUMN lifecycle_rev bigint DEFAULT 0 NOT NULL;

CREATE FUNCTION openrails.subscriptions_lifecycle_single_writer() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    changed text[] := '{}';
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN changed := changed || format('status %s->%s', OLD.status, NEW.status); END IF;
    IF NEW.current_period_starts_at IS DISTINCT FROM OLD.current_period_starts_at THEN changed := changed || 'current_period_starts_at'::text; END IF;
    IF NEW.current_period_ends_at IS DISTINCT FROM OLD.current_period_ends_at THEN changed := changed || 'current_period_ends_at'::text; END IF;
    IF NEW.cancel_type IS DISTINCT FROM OLD.cancel_type THEN changed := changed || 'cancel_type'::text; END IF;
    IF NEW.cancelled_at IS DISTINCT FROM OLD.cancelled_at THEN changed := changed || 'cancelled_at'::text; END IF;
    IF NEW.ended_at IS DISTINCT FROM OLD.ended_at THEN changed := changed || 'ended_at'::text; END IF;
    IF NEW.lifecycle_rev IS DISTINCT FROM OLD.lifecycle_rev AND NEW.lifecycle_rev <> OLD.lifecycle_rev + 1 THEN
        RAISE EXCEPTION 'subscription % lifecycle_rev must advance by one (% -> %)', OLD.id, OLD.lifecycle_rev, NEW.lifecycle_rev
            USING ERRCODE = '55000', CONSTRAINT = 'subscriptions_lifecycle_single_writer';
    END IF;
    IF cardinality(changed) > 0 AND NEW.lifecycle_rev = OLD.lifecycle_rev THEN
        RAISE EXCEPTION 'subscription % lifecycle fields changed outside a lifecycle decision: %', OLD.id, array_to_string(changed, ', ')
            USING ERRCODE = '55000', CONSTRAINT = 'subscriptions_lifecycle_single_writer';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER subscriptions_lifecycle_single_writer BEFORE UPDATE ON openrails.subscriptions
    FOR EACH ROW EXECUTE FUNCTION openrails.subscriptions_lifecycle_single_writer();
