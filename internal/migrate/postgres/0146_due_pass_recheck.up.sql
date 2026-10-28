-- Repair: none-needed A new nullable column.
-- A row the due pass leaves unchanged (held, refused, unarmed) is not due
-- again before due_recheck_at. Setting it is no change of the subscription:
-- row_version stays, as it does when only facts_authority moves.
ALTER TABLE billing.subscriptions ADD COLUMN due_recheck_at timestamptz;
COMMENT ON COLUMN billing.subscriptions.due_recheck_at IS 'The due pass left the row unchanged; it is not due again before this instant.';

CREATE OR REPLACE FUNCTION billing.subscriptions_row_version() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    unchanged billing.subscriptions := NEW;
BEGIN
    unchanged.due_recheck_at := OLD.due_recheck_at;
    unchanged.facts_authority := OLD.facts_authority;
    unchanged.row_version := OLD.row_version;
    unchanged.destructive_run_class := OLD.destructive_run_class;
    IF unchanged IS NOT DISTINCT FROM OLD THEN
        RETURN NEW;
    END IF;
    NEW.row_version := OLD.row_version + 1;
    RETURN NEW;
END;
$$;
