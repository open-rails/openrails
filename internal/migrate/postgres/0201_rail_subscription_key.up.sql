-- Repair: none-needed. The key derives from existing columns. A PSP holding two
-- live rows whose CCBill ids differ only in leading zeros fails the unique
-- index: they are one provider subscription, and a person merges them.
-- A CCBill subscription id is a number: a legacy bigint column drops the
-- leading 0 of a 19-digit id. rail_subscription_id keeps the provider's
-- spelling for outbound calls; every match by provider id compares this key.
CREATE FUNCTION billing.rail_subscription_key(rail text, ref text) RETURNS text
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$ SELECT CASE WHEN rail = 'ccbill' THEN ltrim(ref, '0') ELSE ref END $$;
COMMENT ON FUNCTION billing.rail_subscription_key(text, text) IS 'The identity of a provider subscription id on its rail: a CCBill id without its leading zeros, any other rail''s id as spelled.';

ALTER TABLE billing.subscriptions
    ADD COLUMN rail_subscription_key text GENERATED ALWAYS AS (billing.rail_subscription_key(rail, rail_subscription_id)) STORED;
COMMENT ON COLUMN billing.subscriptions.rail_subscription_key IS 'rail_subscription_id as an identity (billing.rail_subscription_key): every lookup by provider id matches it. A provider contact matching it spelled otherwise rewrites rail_subscription_id to the provider''s spelling.';

DROP INDEX billing.subscriptions_rail_rail_subscription_id_idx;
DROP INDEX billing.subscriptions_psp_id_rail_subscription_id_key;
CREATE UNIQUE INDEX subscriptions_psp_id_rail_subscription_key_key ON billing.subscriptions (merchant_id, psp_id, rail_subscription_key)
    WHERE rail_subscription_key IS NOT NULL AND deleted_at IS NULL;

-- Respelling the same provider id is no change of the subscription: its
-- row_version stays, as when only facts_authority or due_recheck_at moves.
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
    unchanged.rail_subscription_key := OLD.rail_subscription_key;
    IF billing.rail_subscription_key(NEW.rail, NEW.rail_subscription_id) IS NOT DISTINCT FROM OLD.rail_subscription_key THEN
        unchanged.rail_subscription_id := OLD.rail_subscription_id;
    END IF;
    IF unchanged IS NOT DISTINCT FROM OLD THEN
        RETURN NEW;
    END IF;
    NEW.row_version := OLD.row_version + 1;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION billing.close_landed_park() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.rail_subscription_id IS NULL OR NEW.deleted_at IS NOT NULL THEN
        RETURN NULL;
    END IF;
    WITH closed AS (
        UPDATE billing.parked_provider_subscriptions p
        SET released_at = now(), released_by = 'landed'
        WHERE p.merchant_id = NEW.merchant_id AND p.psp_id = NEW.psp_id
          AND billing.rail_subscription_key(p.rail, p.subscription_ref) = NEW.rail_subscription_key
          AND p.released_at IS NULL
        RETURNING p.subscription_ref
    )
    UPDATE billing.reconciliation_findings f
    SET status = 'fixed', resolution = 'auto_vanished', resolved_at = now(), notified_at = NULL, notified_severity = NULL, updated_at = now()
    FROM closed
    WHERE f.merchant_id = NEW.merchant_id AND f.psp_id = NEW.psp_id AND f.finding_type = 'life.subscription.unattributed'
      AND f.subject_key = closed.subscription_ref AND f.status IN ('reconcile_required', 'requires_review');
    RETURN NULL;
END;
$$;
