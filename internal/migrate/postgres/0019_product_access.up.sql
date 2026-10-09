-- parent: 18 sha256:f63eb51f3b2556de4585c5617417da7d6bc5be414a597e0721993dfcd9b6f252
-- Repair: none-needed The widened grant checks admit every stored grant; the
-- new checks name only the new access kind and grant source, which no stored
-- row has. host_outbox and destructive before-images keep every stored row
-- under their widened checks.

-- An access grant gives a customer a product for a window. Its keys are the
-- product's at check time; the grant names no keys.
ALTER TABLE billing.grants DROP CONSTRAINT grants_kind_check;
ALTER TABLE billing.grants ADD CONSTRAINT grants_kind_check CHECK (kind IN ('entitlement', 'ownership', 'credit', 'access'));
ALTER TABLE billing.grants DROP CONSTRAINT grants_source_type_check;
ALTER TABLE billing.grants ADD CONSTRAINT grants_source_type_check CHECK (source_type IN ('purchase', 'subscription', 'admin', 'grace', 'grant'));
ALTER TABLE billing.grants ADD COLUMN actor text;
ALTER TABLE billing.grants ADD COLUMN grant_reason text;
ALTER TABLE billing.grants ADD CONSTRAINT grants_actor_check CHECK (actor IS NULL OR octet_length(actor) BETWEEN 1 AND 255);
ALTER TABLE billing.grants ADD CONSTRAINT grants_grant_reason_check CHECK (grant_reason IS NULL OR (grant_reason IN ('comp', 'staff', 'import', 'migration') AND kind = 'access' AND source_type = 'grant'));
ALTER TABLE billing.grants ADD CONSTRAINT grants_access_product_check CHECK (kind <> 'access' OR (product_id IS NOT NULL AND spec_snapshot IS NULL AND source_id IS NOT NULL));
ALTER TABLE billing.grants ADD CONSTRAINT grants_access_grant_reason_check CHECK (NOT (kind = 'access' AND source_type = 'grant' AND event = 'grant') OR (grant_reason IS NOT NULL AND actor IS NOT NULL));
COMMENT ON COLUMN billing.grants.kind IS 'access: a product for a window, whose keys are read from the product at check time; credit: a credit lot. entitlement and ownership are history from before product access.';
COMMENT ON COLUMN billing.grants.actor IS 'Who granted a free product: the operator or credential. Required on grant-sourced access grants.';
COMMENT ON COLUMN billing.grants.grant_reason IS 'Why a free product was granted: comp, staff, import or migration. Required on grant-sourced access grants; reason stays a free-text note.';
-- One grant per paid period, purchase or idempotency key. A revoked grace
-- allowance can be granted again from the same instant.
CREATE UNIQUE INDEX grants_access_once_key ON billing.grants (merchant_id, customer_id, product_id, source_type, source_id, starts_at)
    WHERE kind = 'access' AND event = 'grant' AND source_type <> 'grace';
CREATE UNIQUE INDEX grants_access_purchase_key ON billing.grants (merchant_id, payment_id, product_id)
    WHERE kind = 'access' AND event = 'grant' AND source_type = 'purchase';
CREATE UNIQUE INDEX grants_access_idempotency_key ON billing.grants (merchant_id, source_id)
    WHERE kind = 'access' AND event = 'grant' AND source_type = 'grant' AND grant_reason <> 'migration';
COMMENT ON INDEX billing.grants_access_idempotency_key IS 'A free product grant is once per idempotency key (its source_id): a replay returns the recorded grant.';

-- The access windows customers hold: the projection of access grants, one row
-- per grant. A customer's keys are the keys of the products they hold now.
CREATE TABLE billing.product_access (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    product_id uuid NOT NULL,
    grant_id uuid NOT NULL,
    source_type text NOT NULL,
    source_id text NOT NULL,
    payment_id uuid,
    starts_at timestamp with time zone NOT NULL,
    ends_at timestamp with time zone,
    revoked_at timestamp with time zone,
    revoke_reason text,
    deleted_at timestamp with time zone,
    destructive_run_id uuid,
    destructive_run_class text GENERATED ALWAYS AS (CASE WHEN destructive_run_id IS NOT NULL THEN 'destructive' END) STORED,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT product_access_pkey PRIMARY KEY (merchant_id, id),
    CONSTRAINT product_access_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT,
    CONSTRAINT product_access_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id),
    CONSTRAINT product_access_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products(merchant_id, id),
    CONSTRAINT product_access_grant_id_fkey FOREIGN KEY (merchant_id, customer_id, grant_id) REFERENCES billing.grants(merchant_id, customer_id, id),
    CONSTRAINT product_access_payment_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_id) REFERENCES billing.payments(merchant_id, customer_id, id),
    CONSTRAINT product_access_destructive_run_id_fkey FOREIGN KEY (merchant_id, destructive_run_id, destructive_run_class) REFERENCES billing.maintenance_runs(merchant_id, id, run_class) ON DELETE RESTRICT,
    CONSTRAINT product_access_source_type_check CHECK (source_type IN ('purchase', 'subscription', 'grace', 'grant')),
    CONSTRAINT product_access_source_id_check CHECK (octet_length(source_id) BETWEEN 1 AND 255),
    CONSTRAINT product_access_revoke_fields_together_check CHECK ((revoked_at IS NULL) = (revoke_reason IS NULL)),
    CONSTRAINT product_access_valid_time_window_check CHECK (ends_at IS NULL OR starts_at < ends_at)
);
COMMENT ON TABLE billing.product_access IS 'Product access windows projected from access grants: a purchase, a subscription period, a grace allowance or a free grant of one product for [starts_at, ends_at). A customer holds the keys the product grants while a window is live; windows may overlap and reads take their union. Rebuildable from the grant ledger.';
COMMENT ON COLUMN billing.product_access.source_id IS 'The source''s own id: the payment of a purchase, the subscription of a period or grace allowance, the idempotency key of a free grant.';
COMMENT ON COLUMN billing.product_access.deleted_at IS 'A window removed before it started (a canceled future period or a refunded future rental): it never granted access.';

CREATE UNIQUE INDEX product_access_grant_key ON billing.product_access (merchant_id, grant_id) WHERE deleted_at IS NULL;
CREATE INDEX product_access_customer_idx ON billing.product_access (merchant_id, customer_id, product_id) INCLUDE (starts_at, ends_at)
    WHERE revoked_at IS NULL AND deleted_at IS NULL;
CREATE INDEX product_access_product_idx ON billing.product_access (merchant_id, product_id, customer_id) INCLUDE (starts_at, ends_at)
    WHERE revoked_at IS NULL AND deleted_at IS NULL;
CREATE INDEX product_access_source_idx ON billing.product_access (merchant_id, source_type, source_id);
CREATE INDEX product_access_payment_idx ON billing.product_access (merchant_id, payment_id) WHERE payment_id IS NOT NULL;
CREATE INDEX product_access_customer_created_idx ON billing.product_access (merchant_id, customer_id, id);
CREATE INDEX product_access_destructive_run_id_idx ON billing.product_access (merchant_id, destructive_run_id) WHERE destructive_run_id IS NOT NULL;
CREATE INDEX product_access_closed_at_idx ON billing.product_access (merchant_id, LEAST(COALESCE(ends_at, 'infinity'::timestamptz), COALESCE(revoked_at, 'infinity'::timestamptz)))
    WHERE (ends_at IS NOT NULL OR revoked_at IS NOT NULL) AND deleted_at IS NULL;
COMMENT ON INDEX billing.product_access_customer_idx IS 'A customer''s live products: exact checks probe it per product granting a key; prefix reads walk it once.';
COMMENT ON INDEX billing.product_access_product_idx IS 'A product''s live holders in customer order: reverse lookups and affected-holder counts.';

-- The cutover to product access lists, per customer and key, every access
-- change it makes; an operator approves them before it runs (preflight).
CREATE TABLE billing.access_cutover_approvals (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    entitlement text COLLATE "C" NOT NULL,
    change text NOT NULL,
    approved_by text NOT NULL,
    approved_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT access_cutover_approvals_pkey PRIMARY KEY (merchant_id, id),
    CONSTRAINT access_cutover_approvals_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT,
    CONSTRAINT access_cutover_approvals_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id),
    CONSTRAINT access_cutover_approvals_change_check CHECK (change IN ('lost', 'gained')),
    CONSTRAINT access_cutover_approvals_approved_by_check CHECK (octet_length(approved_by) BETWEEN 1 AND 255)
);
CREATE UNIQUE INDEX access_cutover_approvals_key ON billing.access_cutover_approvals (merchant_id, customer_id, entitlement, change);
COMMENT ON TABLE billing.access_cutover_approvals IS 'Access changes an operator approved before the cutover from per-key entitlement windows to product access: a key a customer loses (their product dropped it after purchase) or gains (their product added it). The cutover refuses any change not listed. Retention: permanent, never pruned.';

-- A destructive pass records the access windows it bounds; per-key images
-- taken before product access stay as history.
ALTER TABLE billing.destructive_run_before_images DROP CONSTRAINT destructive_run_before_images_table_check;
ALTER TABLE billing.destructive_run_before_images ADD CONSTRAINT destructive_run_before_images_table_check
    CHECK (table_name IN ('subscriptions', 'entitlements', 'product_access'));

-- A key edit tells the host which product changed and how many hold it.
ALTER TABLE billing.host_outbox ALTER COLUMN currency DROP NOT NULL;
ALTER TABLE billing.host_outbox DROP CONSTRAINT host_outbox_currency_check;
ALTER TABLE billing.host_outbox ADD CONSTRAINT host_outbox_currency_check CHECK ((currency IS NULL) = (event_type = 'product.entitlements_changed') AND (currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$'));
ALTER TABLE billing.host_outbox DROP CONSTRAINT host_outbox_payload_check;
ALTER TABLE billing.host_outbox ADD CONSTRAINT host_outbox_payload_check CHECK (
    (event_type = 'payment.settled' AND subject_type = 'payment' AND payment_id IS NOT NULL
     AND subject_id = payment_id AND amount IS NOT NULL AND amount > 0 AND data = '{}'::jsonb)
    OR (event_type IN ('delinquency.grace', 'delinquency.entered', 'delinquency.cleared')
        AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type = 'product.entitlements_changed' AND subject_type = 'product' AND payment_id IS NULL AND amount IS NULL)
);
COMMENT ON COLUMN billing.host_outbox.currency IS 'The transition''s currency; NULL only for a product''s key change, which has none.';
COMMENT ON TABLE billing.host_outbox IS 'Typed durable host events: successful rail payment settlements, delinquency lifecycle transitions and product key changes. Acknowledge after idempotent processing; acknowledgments are separate from notification read state. Retention: delivered events are deleted 30 days after delivered_at; an undelivered event is never deleted.';

-- Converts one merchant's per-key entitlement windows (source: billing.entitlements,
-- or an older archive's rows) into access grants and product_access windows,
-- and supersedes the per-key and ownership grants they replace.
--   purchase      the payment's product; one window from its earliest start to
--                 its latest end, indefinite when any key was (the longest)
--   subscription  the subscription's product, or a product it paid for whose
--   / grace       keys match the period's keys better (tier changes)
--   admin         the product whose live keys are exactly the grant's keys;
--                 none: the keys are not carried (preflight lists them)
--   ownership     a product grant with no key windows (a product without keys,
--                 an operator's product grant) converts as it stands
-- Soft-deleted windows never granted access and are not carried.
-- Dynamic statements name tables unqualified: the function's search_path is
-- the schema it was created in.
CREATE FUNCTION billing.convert_entitlement_windows(p_merchant uuid, p_source regclass, p_at timestamptz)
RETURNS bigint LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $fn$
DECLARE converted bigint;
BEGIN
    DROP TABLE IF EXISTS pg_temp.access_conversion;
    EXECUTE format($q$
    CREATE TEMP TABLE access_conversion ON COMMIT DROP AS
    WITH win AS (
        SELECT e.customer_id, e.entitlement::text COLLATE "C" AS entitlement, e.starts_at, e.ends_at,
               e.revoked_at, e.revoke_reason, e.destructive_run_id::text AS destructive_run_id,
               g.source_type, COALESCE(g.source_id, g.id::text) AS source_id,
               CASE WHEN g.source_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN g.source_id::uuid END AS source_uuid
        FROM %1$s e
        JOIN grants g ON g.merchant_id = e.merchant_id AND g.customer_id = e.customer_id AND g.id = e.grant_id
        WHERE e.merchant_id = %2$L AND e.deleted_at IS NULL
    ), purchases AS (
        SELECT w.customer_id, pr.product_id, 'purchase'::text AS source_type, w.source_id, pay.id AS payment_id,
               min(w.starts_at) AS starts_at,
               CASE WHEN bool_or(w.ends_at IS NULL) THEN NULL ELSE max(w.ends_at) END AS ends_at,
               bool_and(w.revoked_at IS NOT NULL) AS revoked, max(w.revoked_at) AS revoked_at, max(w.revoke_reason) AS revoke_reason,
               max(w.destructive_run_id) AS destructive_run_id
        FROM win w
        JOIN payments pay ON pay.merchant_id = %2$L AND pay.id = w.source_uuid AND pay.customer_id = w.customer_id
        JOIN prices pr ON pr.merchant_id = %2$L AND pr.id = pay.price_id
        WHERE w.source_type = 'purchase'
        GROUP BY w.customer_id, pr.product_id, w.source_id, pay.id
    ), periods AS (
        SELECT w.customer_id, w.source_type, w.source_id, w.source_uuid, w.starts_at,
               CASE WHEN bool_or(w.ends_at IS NULL) THEN NULL ELSE max(w.ends_at) END AS ends_at,
               bool_and(w.revoked_at IS NOT NULL) AS revoked, max(w.revoked_at) AS revoked_at, max(w.revoke_reason) AS revoke_reason,
               max(w.destructive_run_id) AS destructive_run_id, array_agg(DISTINCT w.entitlement) AS keys
        FROM win w WHERE w.source_type IN ('subscription', 'grace')
        GROUP BY w.customer_id, w.source_type, w.source_id, w.source_uuid, w.starts_at
    ), subscription_windows AS (
        SELECT p.customer_id, best.product_id, p.source_type, p.source_id, NULL::uuid AS payment_id, p.starts_at, p.ends_at,
               p.revoked, p.revoked_at, p.revoke_reason, p.destructive_run_id
        FROM periods p
        JOIN subscriptions s ON s.merchant_id = %2$L AND s.id = p.source_uuid
        CROSS JOIN LATERAL (
            SELECT c.product_id FROM (
                SELECT s.product_id
                UNION SELECT pr.product_id FROM payments pay
                JOIN prices pr ON pr.merchant_id = pay.merchant_id AND pr.id = pay.price_id
                WHERE pay.merchant_id = %2$L AND pay.subscription_id = s.id
            ) c
            ORDER BY (SELECT count(*) FROM product_entitlements pe
                      WHERE pe.merchant_id = %2$L AND pe.product_id = c.product_id AND pe.removed_at IS NULL
                        AND pe.entitlement = ANY (p.keys)) DESC,
                     (c.product_id = s.product_id) DESC, c.product_id
            LIMIT 1
        ) best
    ), admin_groups AS (
        SELECT w.customer_id, w.source_id, w.starts_at,
               CASE WHEN bool_or(w.ends_at IS NULL) THEN NULL ELSE max(w.ends_at) END AS ends_at,
               bool_and(w.revoked_at IS NOT NULL) AS revoked, max(w.revoked_at) AS revoked_at, max(w.revoke_reason) AS revoke_reason,
               max(w.destructive_run_id) AS destructive_run_id, array_agg(DISTINCT w.entitlement ORDER BY w.entitlement) AS keys
        FROM win w WHERE w.source_type = 'admin'
        GROUP BY w.customer_id, w.source_id, w.starts_at
    ), admin_windows AS (
        SELECT a.customer_id, match.product_id, 'grant'::text AS source_type, a.source_id, NULL::uuid AS payment_id, a.starts_at, a.ends_at,
               a.revoked, a.revoked_at, a.revoke_reason, a.destructive_run_id
        FROM admin_groups a
        CROSS JOIN LATERAL (
            SELECT candidate.product_id
            FROM product_entitlements candidate
            JOIN products p ON p.merchant_id = candidate.merchant_id AND p.id = candidate.product_id
            WHERE candidate.merchant_id = %2$L AND candidate.entitlement = a.keys[1] AND candidate.removed_at IS NULL
              AND (SELECT array_agg(pe.entitlement ORDER BY pe.entitlement) FROM product_entitlements pe
                   WHERE pe.merchant_id = candidate.merchant_id AND pe.product_id = candidate.product_id AND pe.removed_at IS NULL) = a.keys
            ORDER BY p.archived, p.key
            LIMIT 1
        ) match
    ), ownership_windows AS (
        SELECT g.customer_id, g.product_id, CASE g.source_type WHEN 'admin' THEN 'grant' ELSE g.source_type END AS source_type,
               COALESCE(g.source_id, g.id::text) AS source_id, g.payment_id, g.starts_at, g.ends_at,
               t.id IS NOT NULL AS revoked, t.starts_at AS revoked_at, COALESCE(t.reason, t.event) AS revoke_reason, NULL::text AS destructive_run_id
        FROM grants g
        LEFT JOIN grants t ON t.merchant_id = g.merchant_id AND t.supersedes_id = g.id AND t.event IN ('revoke', 'expire', 'supersede')
        WHERE g.merchant_id = %2$L AND g.kind = 'ownership' AND g.event = 'grant' AND g.product_id IS NOT NULL
          AND NOT EXISTS (SELECT 1 FROM purchases p WHERE p.payment_id = g.payment_id AND p.product_id = g.product_id)
    )
    SELECT uuidv7() AS grant_id, uuidv7() AS access_id, c.customer_id, c.product_id, c.source_type, c.source_id, c.payment_id,
           c.starts_at, c.ends_at, c.revoked AND c.revoked_at IS NOT NULL AS revoked, c.revoked_at,
           COALESCE(c.revoke_reason, 'revoked') AS revoke_reason, c.destructive_run_id::uuid AS destructive_run_id
    FROM (
        SELECT * FROM purchases UNION ALL SELECT * FROM subscription_windows
        UNION ALL SELECT * FROM admin_windows UNION ALL SELECT * FROM ownership_windows
    ) c
    $q$, p_source, p_merchant);

    INSERT INTO billing.grants (id, merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id, event, starts_at, ends_at, reason, actor, grant_reason)
    SELECT c.grant_id, p_merchant, c.customer_id, c.product_id, 'access', c.source_type, c.source_id, c.payment_id, 'grant', c.starts_at, c.ends_at,
           'product access migration', CASE WHEN c.source_type = 'grant' THEN 'migration' END, CASE WHEN c.source_type = 'grant' THEN 'migration' END
    FROM access_conversion c;
    INSERT INTO billing.grants (merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id, event, supersedes_id, starts_at, reason)
    SELECT p_merchant, c.customer_id, c.product_id, 'access', c.source_type, c.source_id, c.payment_id, 'revoke', c.grant_id, c.revoked_at, c.revoke_reason
    FROM access_conversion c WHERE c.revoked;
    INSERT INTO billing.product_access (id, merchant_id, customer_id, product_id, grant_id, source_type, source_id, payment_id,
                                        starts_at, ends_at, revoked_at, revoke_reason, destructive_run_id)
    SELECT c.access_id, p_merchant, c.customer_id, c.product_id, c.grant_id, c.source_type, c.source_id, c.payment_id, c.starts_at, c.ends_at,
           CASE WHEN c.revoked THEN c.revoked_at END, CASE WHEN c.revoked THEN c.revoke_reason END, c.destructive_run_id
    FROM access_conversion c;
    GET DIAGNOSTICS converted = ROW_COUNT;

    INSERT INTO billing.grants (merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id, event, supersedes_id, spec_snapshot, starts_at, reason)
    SELECT g.merchant_id, g.customer_id, g.product_id, g.kind, g.source_type, g.source_id, g.payment_id, 'supersede', g.id, g.spec_snapshot, p_at, 'product access migration'
    FROM billing.grants g
    WHERE g.merchant_id = p_merchant AND g.kind IN ('entitlement', 'ownership') AND g.event = 'grant'
      AND NOT EXISTS (SELECT 1 FROM billing.grants t WHERE t.merchant_id = g.merchant_id AND t.supersedes_id = g.id
                        AND t.event IN ('revoke', 'expire', 'supersede'));
    RETURN converted;
END $fn$;

-- Every key whose access the conversion changes from p_at on, per customer:
-- lost (an old window granted it, no product window does) or gained.
CREATE FUNCTION billing.product_access_changes(p_merchant uuid, p_source regclass, p_at timestamptz)
RETURNS TABLE (customer_id uuid, entitlement text, change text) LANGUAGE plpgsql STABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $fn$
BEGIN
    RETURN QUERY EXECUTE format($q$
    WITH old AS (
        SELECT e.customer_id, e.entitlement::text COLLATE "C" AS entitlement,
               range_agg(tstzrange(GREATEST(e.starts_at, %3$L::timestamptz), e.ends_at)) AS r
        FROM %1$s e
        WHERE e.merchant_id = %2$L AND e.revoked_at IS NULL AND e.deleted_at IS NULL
          AND (e.ends_at IS NULL OR e.ends_at > %3$L::timestamptz)
        GROUP BY 1, 2
    ), new AS (
        SELECT pa.customer_id, pe.entitlement::text COLLATE "C" AS entitlement,
               range_agg(tstzrange(GREATEST(pa.starts_at, pe.added_at, %3$L::timestamptz), LEAST(pa.ends_at, pe.removed_at))) AS r
        FROM product_access pa
        JOIN product_entitlements pe ON pe.merchant_id = pa.merchant_id AND pe.product_id = pa.product_id
        WHERE pa.merchant_id = %2$L AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
          AND (pa.ends_at IS NULL OR pa.ends_at > %3$L::timestamptz)
          AND (pe.removed_at IS NULL OR pe.removed_at > %3$L::timestamptz)
          AND GREATEST(pa.starts_at, pe.added_at, %3$L::timestamptz) < COALESCE(LEAST(pa.ends_at, pe.removed_at), 'infinity'::timestamptz)
        GROUP BY 1, 2
    )
    SELECT COALESCE(o.customer_id, n.customer_id), COALESCE(o.entitlement, n.entitlement)::text, d.change
    FROM old o FULL JOIN new n ON n.customer_id = o.customer_id AND n.entitlement = o.entitlement
    CROSS JOIN LATERAL (VALUES
        ('lost', NOT isempty(COALESCE(o.r, '{}'::tstzmultirange) - COALESCE(n.r, '{}'::tstzmultirange))),
        ('gained', NOT isempty(COALESCE(n.r, '{}'::tstzmultirange) - COALESCE(o.r, '{}'::tstzmultirange)))
    ) d(change, changed)
    WHERE d.changed
    ORDER BY 1, 2, 3
    $q$, p_source, p_merchant, p_at);
END $fn$;

-- What the preflight explains besides key changes: live per-key windows the
-- conversion could not carry, and live purchases whose keys had different
-- durations (they keep the longest).
CREATE FUNCTION billing.product_access_conversion_notes(p_merchant uuid, p_source regclass, p_at timestamptz)
RETURNS TABLE (customer_id uuid, note text, source_type text, source_id text, entitlements text[]) LANGUAGE plpgsql STABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $fn$
BEGIN
    RETURN QUERY EXECUTE format($q$
    WITH live AS (
        SELECT e.customer_id, e.entitlement::text AS entitlement, e.ends_at, g.source_type, COALESCE(g.source_id, g.id::text) AS source_id
        FROM %1$s e
        JOIN grants g ON g.merchant_id = e.merchant_id AND g.customer_id = e.customer_id AND g.id = e.grant_id
        WHERE e.merchant_id = %2$L AND e.revoked_at IS NULL AND e.deleted_at IS NULL
          AND (e.ends_at IS NULL OR e.ends_at > %3$L::timestamptz)
    )
    SELECT l.customer_id, 'unmapped'::text, l.source_type, l.source_id, array_agg(DISTINCT l.entitlement ORDER BY l.entitlement)
    FROM live l
    WHERE NOT EXISTS (SELECT 1 FROM product_access pa
        WHERE pa.merchant_id = %2$L AND pa.customer_id = l.customer_id AND pa.source_id = l.source_id
          AND pa.source_type = CASE l.source_type WHEN 'admin' THEN 'grant' ELSE l.source_type END)
    GROUP BY 1, 2, 3, 4
    UNION ALL
    SELECT l.customer_id, 'mixed_duration'::text, l.source_type, l.source_id, array_agg(DISTINCT l.entitlement ORDER BY l.entitlement)
    FROM live l WHERE l.source_type = 'purchase'
    GROUP BY 1, 2, 3, 4
    HAVING count(DISTINCT COALESCE(l.ends_at, 'infinity'::timestamptz)) > 1
    ORDER BY 1, 2, 3, 4
    $q$, p_source, p_merchant, p_at);
END $fn$;

-- Product access is part of the merchant book and occupies a restore destination.
CREATE OR REPLACE FUNCTION billing.guard_billing_restore_receipt() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE item record; occupied boolean;
BEGIN
    IF TG_OP='DELETE' THEN
        IF OLD.kind='billing_restore' THEN
            RAISE EXCEPTION 'billing restore receipts are immutable' USING ERRCODE='23514';
        END IF;
        RETURN OLD;
    END IF;
    IF NEW.kind<>'billing_restore' AND (TG_OP='INSERT' OR OLD.kind<>'billing_restore') THEN RETURN NEW; END IF;
    IF NEW.merchant_id IS DISTINCT FROM billing.current_merchant_id() THEN
        RAISE EXCEPTION 'billing restore merchant mismatch' USING ERRCODE='42501';
    END IF;
    -- The merchant row is the serialization point used by begin_billing_restore
    -- and by FK-backed first writes. No database-owner exemption or GUC-only
    -- permission can create a receipt for an occupied destination.
    PERFORM 1 FROM billing.merchants WHERE id=NEW.merchant_id AND status='active' AND deleted_at IS NULL FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'billing restore merchant missing or inactive' USING ERRCODE='P0002'; END IF;
    IF TG_OP='INSERT' THEN
        IF NEW.status<>'running' OR NEW.finished_at IS NOT NULL OR NEW.summary IS NOT NULL THEN
            RAISE EXCEPTION 'billing restore receipts must begin running and unfinished' USING ERRCODE='23514';
        END IF;
        FOR item IN SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
            JOIN pg_attribute a ON a.attrelid=c.oid AND a.attname='merchant_id' AND NOT a.attisdropped
            WHERE n.nspname=TG_TABLE_SCHEMA AND c.relkind IN ('r','p')
              -- Every merchant-scoped table but the directory's own identity rows
              -- (former names, host claims); a test keeps this list complete.
              AND c.relname = ANY(ARRAY[
                  'access_cutover_approvals',
                  'account_updater_batches',
                  'admission_denials_hourly',
                  'admission_operations',
                  'billing_policies',
                  'billing_policy_bindings',
                  'card_attempt_failures',
                  'catalog_applications',
                  'catalog_meters',
                  'catalog_rate_cards',
                  'catalog_restore_receipts',
                  'checkout_attempts',
                  'checkout_sessions',
                  'cost_observations',
                  'cost_qualifications',
                  'credential_publications',
                  'custodians',
                  'custody_migrations',
                  'customer_delinquency',
                  'customer_invoice_profiles',
                  'customers',
                  'dashboard_configs',
                  'destructive_run_before_images',
                  'entitlements',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'invoice_collection_cadence',
                  'invoice_items',
                  'invoice_payments',
                  'invoices',
                  'invoker_spend_limits',
                  'ledger_accounts',
                  'ledger_transfers',
                  'maintenance_runs',
                  'merchant_configuration_applications',
                  'merchant_configurations',
                  'merchant_deks',
                  'merchant_destructive_policy',
                  'merchant_secrets',
                  'merchant_webhooks',
                  'metered_rating_watermarks',
                  'money_settings',
                  'nmi_bulk_checkpoints',
                  'nmi_history_months',
                  'nmi_history_reads',
                  'notifications',
                  'operation_authorization_extensions',
                  'operation_authorizations',
                  'payment_attempts',
                  'payment_method_updates',
                  'payment_methods',
                  'payments',
                  'price_key_movements',
                  'price_psp_bindings',
                  'prices',
                  'product_archive_operations',
                  'product_access',
                  'product_entitlements',
                  'products',
                  'provider_intents',
                  'provider_mutation_logs',
                  'psp_customers',
                  'psp_refresh_watermarks',
                  'psps',
                  'rebill_cycles',
                  'reconciliation_findings',
                  'reconciliation_state',
                  'reprice_batches',
                  'solana_pay_receipts',
                  'solana_pay_references',
                  'solana_subscriptions',
                  'subscription_reprices',
                  'subscription_status_transitions',
                  'subscription_verifications',
                  'subscriptions',
                  'usage_events',
                  'webhook_events',
                  'webhook_health',
                  'webhook_health_daily']::text[])
        LOOP
            EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.%I WHERE merchant_id=$1)',TG_TABLE_SCHEMA,item.relname)
                INTO occupied USING NEW.merchant_id;
            IF occupied THEN RAISE EXCEPTION 'billing restore destination is not empty: %',item.relname USING ERRCODE='55000'; END IF;
        END LOOP;
    ELSE
        IF OLD.kind<>'billing_restore' OR NEW.kind<>'billing_restore'
           OR OLD.status<>'running' OR NEW.status<>'completed'
           OR NEW.finished_at IS NULL
           OR (to_jsonb(NEW)-ARRAY['status','finished_at','summary','run_class']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['status','finished_at','summary','run_class'])
           OR NOT EXISTS (SELECT 1 FROM billing.maintenance_runs r WHERE r.id=OLD.id AND r.merchant_id=OLD.merchant_id
               AND r.xmin=pg_current_xact_id_if_assigned()::xid)
           OR OLD.id::text IS DISTINCT FROM current_setting('openrails.billing_restore_id',true)
           OR NEW.summary->>'digest' IS NULL OR NEW.summary->>'digest' !~ '^[0-9a-f]{64}$'
           OR jsonb_typeof(NEW.summary->'rows') IS DISTINCT FROM 'number'
           OR (NEW.summary->>'rows')::numeric < 0
           OR (NEW.summary->>'rows')::numeric <> trunc((NEW.summary->>'rows')::numeric) THEN
            RAISE EXCEPTION 'invalid billing restore receipts transition' USING ERRCODE='23514';
        END IF;
        PERFORM billing.check_billing_restore_ledger(NEW.merchant_id);
    END IF;
    RETURN NEW;
END;
$$;
