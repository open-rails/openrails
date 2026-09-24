-- parent: 8 sha256:e8d357f031f34d999f645f71caa033bf68a4ef3964339204ea9476e585ae4fc7
-- Every customer with a usable stored payment method has exactly one default
-- (#1084). Usable = the row exists and is not parked (park_reason = '');
-- deletion removes the row. At most one default is the partial unique index;
-- at least one is kept by a deferred trigger that, at commit, promotes the
-- most recently used card (latest completed payment of a subscription it
-- funds), else a non-expired card, else the most recently saved.
ALTER TABLE openrails.payment_methods ADD COLUMN is_default boolean DEFAULT false NOT NULL;
ALTER TABLE openrails.payment_methods ADD CONSTRAINT payment_methods_default_usable CHECK (NOT is_default OR park_reason = '');
CREATE UNIQUE INDEX uq_payment_methods_customer_default ON openrails.payment_methods USING btree (merchant_id, customer_id) WHERE is_default;

COMMENT ON COLUMN openrails.payment_methods.is_default IS 'The customer''s default payment method: exactly one per (merchant, customer) with a usable method (#1084).';

CREATE FUNCTION openrails.payment_method_default_candidate(p_merchant uuid, p_customer uuid) RETURNS uuid
    LANGUAGE sql STABLE
    AS $$
    SELECT pm.id FROM openrails.payment_methods pm
    LEFT JOIN LATERAL (
        SELECT max(p.purchased_at) AS used_at
        FROM openrails.subscriptions s
        JOIN openrails.payments p ON p.merchant_id = s.merchant_id AND p.subscription_id = s.id
            AND p.deleted_at IS NULL AND p.status = 'completed'
        WHERE s.merchant_id = pm.merchant_id AND s.payment_method_id = pm.id
    ) used ON true
    WHERE pm.merchant_id = p_merchant AND pm.customer_id = p_customer AND pm.park_reason = ''
    ORDER BY used.used_at DESC NULLS LAST,
        (CASE WHEN regexp_replace(coalesce(pm.expiry_date, ''), '\D', '', 'g') !~ '^(0[1-9]|1[0-2])[0-9]{2}$' THEN 0
              WHEN make_date(2000 + substr(regexp_replace(pm.expiry_date, '\D', '', 'g'), 3, 2)::int,
                             substr(regexp_replace(pm.expiry_date, '\D', '', 'g'), 1, 2)::int, 1) + interval '1 month' <= now() THEN 1
              ELSE 0 END),
        pm.created_at DESC, pm.id DESC
    LIMIT 1
$$;

-- Backfill before the triggers exist.
UPDATE openrails.payment_methods pm SET is_default = true
FROM (SELECT DISTINCT merchant_id, customer_id FROM openrails.payment_methods WHERE park_reason = '') c
WHERE pm.merchant_id = c.merchant_id AND pm.customer_id = c.customer_id
  AND pm.id = openrails.payment_method_default_candidate(c.merchant_id, c.customer_id);

-- A parked method is never default.
CREATE FUNCTION openrails.payment_methods_default_usable() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.park_reason <> '' THEN
        NEW.is_default := false;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_payment_methods_default_usable BEFORE INSERT OR UPDATE OF park_reason, is_default ON openrails.payment_methods
    FOR EACH ROW EXECUTE FUNCTION openrails.payment_methods_default_usable();

-- At commit, a customer whose methods changed keeps exactly one default. The
-- customer lock serializes concurrent commits, so two first saves end with one.
CREATE FUNCTION openrails.payment_methods_ensure_default() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    m uuid;
    c uuid;
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
            UPDATE openrails.payment_methods SET is_default = true
            WHERE merchant_id = m AND id = openrails.payment_method_default_candidate(m, c);
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_payment_methods_ensure_default
    AFTER INSERT OR DELETE OR UPDATE OF is_default, park_reason, customer_id ON openrails.payment_methods
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION openrails.payment_methods_ensure_default();

-- Changing only which card is default never touches a card in a provider cutover.
CREATE OR REPLACE FUNCTION openrails.guard_provider_cutover_card() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP = 'UPDATE' AND (to_jsonb(NEW) - 'is_default' - 'updated_at') = (to_jsonb(OLD) - 'is_default' - 'updated_at') THEN
   RETURN NEW;
 END IF;
 IF EXISTS(SELECT 1 FROM openrails.rail_intents i
   WHERE i.merchant_id=OLD.merchant_id AND i.intent_type='nmi_provider_cutover'
   AND i.status NOT IN ('succeeded','failed_terminal','superseded','expired')
   AND (i.payload->'request'->>'target_payment_method_id'=OLD.id::text
        OR i.payload->>'source_payment_method_id'=OLD.id::text)) THEN
   RAISE EXCEPTION 'payment method has an unresolved provider cutover' USING ERRCODE='55000';
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;
