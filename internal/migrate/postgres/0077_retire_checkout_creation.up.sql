-- parent: 76 sha256:67d784d49e2ec82b305b55b9c903cd9de8430813b23c7aaa18d14d9aa78b9fc7
-- Repair: none-needed Prelaunch: historical checkout data is not reconstructed. The new order-only check is NOT VALID for existing rows.
ALTER TABLE billing.payment_methods ADD COLUMN capture jsonb;
ALTER TABLE billing.payment_methods ADD CONSTRAINT payment_methods_capture_check CHECK (
    capture IS NULL OR (custodian = 'hyperswitch' AND jsonb_typeof(capture) = 'object' AND octet_length(capture::text) <= 16384));
COMMENT ON COLUMN billing.payment_methods.capture IS 'Private accepted custodian card-save binding, encrypted SDK authorization until completion, then token hash and canonical method identity. Never payment authority.';
DROP INDEX billing.payment_methods_psp_instrument_key;
CREATE UNIQUE INDEX payment_methods_psp_instrument_key ON billing.payment_methods
    (merchant_id, psp_id, custodian_id, rail_customer_ref, rail_method_ref) NULLS NOT DISTINCT
    WHERE rail_customer_ref IS NOT NULL OR rail_method_ref IS NOT NULL;

-- A pending save may discover an instrument already stored for this customer.
CREATE OR REPLACE FUNCTION billing.guard_payment_method_status() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF OLD.status = 'requires_action' AND NEW.status IN ('requires_action', 'active', 'replaced', 'removed')
        AND (NEW.status <> 'replaced' OR OLD.capture IS NOT NULL) THEN
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

-- Checkout attempts now pay only orders. Existing prelaunch rows are ignored;
-- no historical reconstruction or destructive cleanup runs during migration.
ALTER TABLE billing.checkout_attempts DROP COLUMN price_id, DROP COLUMN mode;
ALTER TABLE billing.checkout_attempts ADD CONSTRAINT checkout_attempts_order_terms_check
    CHECK (order_id IS NOT NULL AND amount IS NOT NULL AND currency IS NOT NULL) NOT VALID;
COMMENT ON TABLE billing.checkout_attempts IS 'An order payment attempt: provider execution and any customer action are tied to the order. Retention: attempts that expired without reaching a provider are deleted 90 days after expires_at; every other attempt is permanent.';
COMMENT ON COLUMN billing.checkout_attempts.order_id IS 'The order this payment attempt pays.';
