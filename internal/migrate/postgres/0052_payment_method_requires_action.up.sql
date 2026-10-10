-- parent: 51 sha256:06d3c1ae2a943af8e998eba4ee7868bda5d343e8a9b45410c3d56a80fa59699d
-- Repair: none-needed Every existing method is active, closed, replaced or removed; none awaits an action.
-- A card being saved can wait for the customer: the bank asks for 3-D
-- Secure before the card is stored. Such a method is requires_action, names
-- the provider's setup awaiting it, and becomes active or removed when the
-- customer confirms; it is never charged.
ALTER TABLE billing.payment_methods ADD COLUMN setup_ref text;
ALTER TABLE billing.payment_methods DROP CONSTRAINT payment_methods_status_check;
ALTER TABLE billing.payment_methods
    ADD CONSTRAINT payment_methods_status_check CHECK (status IN ('requires_action', 'active', 'closed', 'replaced', 'removed')),
    ADD CONSTRAINT payment_methods_setup_ref_check CHECK ((status = 'requires_action') = (setup_ref IS NOT NULL) AND setup_ref <> '');
COMMENT ON COLUMN billing.payment_methods.status IS 'requires_action (being saved: the customer completes the bank''s authentication, setup_ref); active; closed (the bank closed the account); replaced (another method took its place, replaced_by_id); removed. The last three are final.';
COMMENT ON COLUMN billing.payment_methods.setup_ref IS 'The provider''s setup awaiting the customer (a Stripe SetupIntent) while status is requires_action.';
CREATE INDEX payment_methods_requires_action_idx ON billing.payment_methods USING btree (merchant_id, created_at) WHERE status = 'requires_action';

CREATE OR REPLACE FUNCTION billing.guard_payment_method_status() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF OLD.status = 'requires_action' AND NEW.status IN ('requires_action', 'active', 'removed') THEN
        RETURN NEW;
    END IF;
    IF OLD.status <> 'active' AND (NEW.status, NEW.replaced_by_id) IS DISTINCT FROM (OLD.status, OLD.replaced_by_id) THEN
        RAISE EXCEPTION 'payment method % is %, which is final', OLD.id, OLD.status USING ERRCODE = '23514';
    END IF;
    IF NEW.status = 'requires_action' AND OLD.status <> 'requires_action' THEN
        RAISE EXCEPTION 'payment method % was already saved', OLD.id USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
