-- parent: 72 sha256:de38170fa42bc1f56d0f59653635d57077ab1c71b267dcc348d554e6eaafb854
-- Repair: inferred Pending HyperSwitch captures move from attempts to payment methods with the same row identity and encrypted binding; no provider operation is dispatched.
-- Candidate 75 follows this branch's 72. Restack and relink after integration 73/74 before merge.
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
    IF OLD.status = 'requires_action' AND NEW.status IN ('requires_action', 'active', 'replaced', 'removed') THEN
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

INSERT INTO billing.payment_methods
    (id, merchant_id, customer_id, rail, custodian, custodian_id, charge_via, status, setup_ref, capture, created_at, updated_at)
SELECT id, merchant_id, customer_id, 'nmi', 'hyperswitch',
    (rail_state#>>'{capture,custodian_id}')::uuid, 'pan_proxy', 'requires_action',
    rail_state#>>'{capture,vendor_session_id}', rail_state->'capture', created_at, updated_at
FROM billing.checkout_attempts
WHERE mode='payment_method' AND rail='nmi' AND status='requires_action' AND deleted_at IS NULL;

-- The migrated pending resource now owns the authorization. The historical
-- attempt retains only nonsecret evidence, so there is one live secret owner.
UPDATE billing.checkout_attempts SET status='canceled',rail_state=rail_state #- '{capture,secret_ciphertext}'
WHERE mode='payment_method' AND rail='nmi' AND status IN ('created','requires_action') AND deleted_at IS NULL;

UPDATE billing.checkout_attempts SET status='canceled',rail_state=COALESCE(rail_state,'{}'::jsonb)||'{"retired_setup":true}'::jsonb
WHERE mode='payment_method' AND NOT COALESCE(rail_state ? 'capture',false) AND COALESCE(rail_state->>'kind','')<>'stripe_engine_setup';

-- Freeze identity before dropping the old producer's selector columns. Every
-- retained pending operation continues from its accepted terms and PSP.
UPDATE billing.checkout_attempts SET rail_state=COALESCE(rail_state,'{}'::jsonb)
    || jsonb_build_object('accepted_offer',jsonb_build_object('price_id',price_id,'subscription',mode='subscription'))
WHERE price_id IS NOT NULL;
ALTER TABLE billing.checkout_attempts DROP COLUMN price_id, DROP COLUMN mode;
COMMENT ON TABLE billing.checkout_attempts IS 'An order payment attempt, or retained provider recovery evidence accepted before order-based checkout. New creation requires an order.';

ALTER TABLE billing.checkout_attempts ADD CONSTRAINT checkout_attempts_retained_terms_check CHECK (COALESCE(
    (order_id IS NOT NULL AND amount IS NOT NULL AND currency IS NOT NULL AND NOT (COALESCE(rail_state,'{}'::jsonb) ? 'accepted_offer'))
    OR (order_id IS NULL AND rail_state ? 'accepted_offer' AND amount IS NOT NULL AND currency IS NOT NULL)
    OR (order_id IS NULL AND amount IS NULL AND currency IS NULL AND payment_id IS NULL AND subscription_id IS NULL
        AND (rail_state ? 'capture' OR rail_state->>'kind'='stripe_engine_setup' OR rail_state->>'retired_setup'='true')), false));
CREATE INDEX checkout_attempts_accepted_offer_idx ON billing.checkout_attempts
    (merchant_id, ((rail_state#>>'{accepted_offer,price_id}')::uuid)) WHERE rail_state ? 'accepted_offer';
COMMENT ON COLUMN billing.checkout_attempts.order_id IS 'The order this attempt pays; null only on recovery evidence accepted before order-based checkout.';
