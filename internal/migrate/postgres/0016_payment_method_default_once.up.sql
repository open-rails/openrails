-- parent: 15 sha256:04583b8c43bff8de8c458e1233f02311adf25d9e0e80f052ee3a4a9d9d3cb633
-- The default-method commit trigger picks the candidate once per customer.
-- In the UPDATE's WHERE the candidate function could run once per scanned
-- row; a 1,000-member import spent most of its time there (#1096).
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

CREATE OR REPLACE FUNCTION openrails.payment_methods_ensure_default() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    m uuid;
    c uuid;
    pick uuid;
BEGIN
    FOR m, c IN
        SELECT DISTINCT x.merchant_id, x.customer_id FROM (
            SELECT NEW.merchant_id AS merchant_id, NEW.customer_id AS customer_id WHERE TG_OP <> 'DELETE'
            UNION ALL
            SELECT OLD.merchant_id, OLD.customer_id WHERE TG_OP <> 'INSERT'
        ) x
    LOOP
        PERFORM pg_advisory_xact_lock(hashtextextended('openrails.default_payment_method:' || m::text || ':' || c::text, 0));
        IF NOT EXISTS (SELECT 1 FROM openrails.payment_methods WHERE merchant_id = m AND customer_id = c AND is_default) THEN
            pick := openrails.payment_method_default_candidate(m, c);
            IF pick IS NOT NULL THEN
                UPDATE openrails.payment_methods SET is_default = true WHERE merchant_id = m AND id = pick;
            END IF;
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$;
