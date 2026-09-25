-- parent: 20 sha256:785da24bea0242c3ee94b8121f2db7852ef16c239844782a5a4ab596111e66ad
-- #1102: the lifecycle audit names each decision and records paid-period
-- moves, not only status changes. UpdateSubscriptionDecided sets the
-- transaction-local billing.decision the trigger records.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

ALTER TABLE openrails.subscription_status_transitions
    ADD COLUMN decision text,
    ADD COLUMN from_paid_through timestamp with time zone,
    ADD COLUMN to_paid_through timestamp with time zone,
    DROP CONSTRAINT chk_sst_real_transition,
    ADD CONSTRAINT chk_sst_real_transition
        CHECK (from_status IS DISTINCT FROM to_status OR from_paid_through IS DISTINCT FROM to_paid_through) NOT VALID;

CREATE OR REPLACE FUNCTION openrails.subscriptions_record_status_transition() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    decision text := nullif(current_setting('billing.decision', true), '');
BEGIN
    IF openrails.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    IF TG_OP = 'INSERT' THEN
        INSERT INTO openrails.subscription_status_transitions
            (merchant_id, subscription_id, from_status, to_status, cancel_type, occurred_at, decision, to_paid_through)
        VALUES (NEW.merchant_id, NEW.id, NULL, NEW.status, NEW.cancel_type, now(), 'created', NEW.current_period_ends_at);
    ELSIF OLD.status IS DISTINCT FROM NEW.status OR OLD.current_period_ends_at IS DISTINCT FROM NEW.current_period_ends_at THEN
        INSERT INTO openrails.subscription_status_transitions
            (merchant_id, subscription_id, from_status, to_status, cancel_type, occurred_at, decision, from_paid_through, to_paid_through)
        VALUES (NEW.merchant_id, NEW.id, OLD.status, NEW.status, NEW.cancel_type, now(), decision, OLD.current_period_ends_at, NEW.current_period_ends_at);
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER trg_subscriptions_status_transition ON openrails.subscriptions;
CREATE TRIGGER trg_subscriptions_status_transition AFTER INSERT OR UPDATE OF status, current_period_ends_at ON openrails.subscriptions
    FOR EACH ROW EXECUTE FUNCTION openrails.subscriptions_record_status_transition();
