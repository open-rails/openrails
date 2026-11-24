-- Credit lots leave grants: each lot is a credit_grants row whose posted
-- units are counted by the ledger, with its revocations in
-- credit_grant_events. Ids are kept, so every ledger leg still names its lot.

DO $$
DECLARE n bigint;
BEGIN
    SELECT count(*) INTO n FROM billing.grants WHERE kind = 'credit' AND event NOT IN ('grant', 'revoke');
    IF n > 0 THEN
        RAISE EXCEPTION 'credit grants: % credit lot terminations are not revocations', n;
    END IF;
    SELECT count(*) INTO n FROM billing.grants
    WHERE kind = 'credit' AND event = 'grant'
      AND (payment_id IS NULL) <> ((spec_snapshot -> 'deposit' -> 'paid_amount') IS NULL);
    IF n > 0 THEN
        RAISE EXCEPTION 'credit grants: % credit lots disagree on their payment and paid amount', n;
    END IF;
    SELECT count(*) INTO n FROM billing.grants WHERE kind <> 'credit' AND (amount IS NOT NULL OR currency IS NOT NULL);
    IF n > 0 THEN
        RAISE EXCEPTION 'credit grants: % grants that are not credit lots carry an amount', n;
    END IF;
    SELECT count(*) INTO n FROM billing.ledger_transfers t
    WHERE t.grant_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM billing.grants g
        WHERE g.merchant_id = t.merchant_id AND g.id = t.grant_id AND g.kind = 'credit' AND g.event = 'grant'
          AND (t.customer_id IS NULL OR t.customer_id = g.customer_id) AND t.currency = g.currency);
    IF n > 0 THEN
        RAISE EXCEPTION 'credit grants: % ledger transfers name a lot that is not theirs', n;
    END IF;
    -- A lot drawn beyond what it credited (revoked or spent before its
    -- deposit posted) breaks units_debited <= units_credited; nothing repairs it.
    SELECT count(*) INTO n FROM billing.grants g
    JOIN billing.ledger_accounts a ON a.merchant_id = g.merchant_id AND a.customer_id = g.customer_id
        AND a.account_type = 'customer_balance' AND a.currency = g.currency
    CROSS JOIN LATERAL (SELECT COALESCE(sum(t.amount) FILTER (WHERE t.credit_account_id = a.id), 0) AS credited,
                               COALESCE(sum(t.amount) FILTER (WHERE t.debit_account_id = a.id), 0) AS debited
        FROM billing.ledger_transfers t WHERE t.merchant_id = g.merchant_id AND t.grant_id = g.id) l
    WHERE g.kind = 'credit' AND g.event = 'grant' AND l.debited > l.credited;
    IF n > 0 THEN
        RAISE EXCEPTION 'credit grants: % credit lots were drawn beyond what they credited (revoked or spent before their deposit posted); resolve them before this migration', n;
    END IF;
    SELECT count(*) INTO n FROM billing.grants
    WHERE kind = 'credit' AND event = 'grant'
      AND (octet_length(spec_snapshot -> 'deposit' ->> 'invoker') > 512 OR octet_length(spec_snapshot -> 'deposit' ->> 'source') > 512);
    IF n > 0 THEN
        RAISE EXCEPTION 'credit grants: % credit lots name an invoker or source longer than 512 bytes', n;
    END IF;
END;
$$;

CREATE TABLE billing.credit_grants (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    id uuid NOT NULL DEFAULT uuidv7(),
    customer_id uuid NOT NULL,
    account_id uuid NOT NULL,
    unit text NOT NULL CHECK (unit ~ '^[A-Z0-9]{3,12}$'),
    units bigint NOT NULL CHECK (units > 0),
    funding_source text NOT NULL CHECK (funding_source IN ('purchase', 'recurring', 'grant', 'promotion', 'import')),
    paid_amount bigint CHECK (paid_amount > 0),
    paid_currency text CHECK (paid_currency ~ '^[A-Z0-9]{3,12}$'),
    source text NOT NULL CHECK (octet_length(source) BETWEEN 1 AND 512),
    source_id text CHECK (octet_length(source_id) BETWEEN 1 AND 512),
    invoker text NOT NULL CHECK (octet_length(invoker) BETWEEN 1 AND 512),
    product_id uuid,
    order_line_id uuid,
    payment_id uuid,
    description text CHECK (description <> ''),
    starts_at timestamptz NOT NULL,
    expires_at timestamptz,
    revoked_at timestamptz,
    units_credited bigint NOT NULL DEFAULT 0,
    units_debited bigint NOT NULL DEFAULT 0 CHECK (units_debited >= 0),
    units_reserved bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT credit_grants_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT credit_grants_account_id_fkey FOREIGN KEY (merchant_id, customer_id, unit, account_id) REFERENCES billing.ledger_accounts (merchant_id, customer_id, currency, id),
    CONSTRAINT credit_grants_payment_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_id) REFERENCES billing.payments (merchant_id, customer_id, id),
    CONSTRAINT credit_grants_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products (merchant_id, id),
    CONSTRAINT credit_grants_order_line_id_fkey FOREIGN KEY (merchant_id, order_line_id) REFERENCES billing.order_lines (merchant_id, id),
    CONSTRAINT credit_grants_paid_check CHECK ((paid_amount IS NULL) = (paid_currency IS NULL)),
    CONSTRAINT credit_grants_funding_check CHECK ((funding_source IN ('purchase', 'recurring')) = (paid_amount IS NOT NULL) OR funding_source = 'import'),
    CONSTRAINT credit_grants_units_posted_check CHECK (units_debited <= units_credited),
    CONSTRAINT credit_grants_units_reserved_check CHECK (units_reserved BETWEEN 0 AND units),
    CONSTRAINT credit_grants_valid_window_check CHECK (expires_at IS NULL OR starts_at < expires_at)
);
COMMENT ON TABLE billing.credit_grants IS 'Credit lots: units of a currency or merchant unit credited to a customer''s balance, spent soonest expiry first. Every movement of a lot''s units is a ledger transfer naming it; units_credited and units_debited count the legs on its balance account. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.credit_grants.account_id IS 'The customer''s balance account the lot''s units sit in; only legs on it move the lot''s counters.';
COMMENT ON COLUMN billing.credit_grants.unit IS 'What the lot counts: a registry currency, or a merchant-defined unit.';
COMMENT ON COLUMN billing.credit_grants.funding_source IS 'purchase and recurring were paid (a one-off order line, or a recurring price''s cycle); grant and promotion were given; import came from another system and may declare what was paid.';
COMMENT ON COLUMN billing.credit_grants.paid_amount IS 'What was paid for the lot, in paid_currency (a registry currency); NULL for a gift.';
COMMENT ON COLUMN billing.credit_grants.source IS 'The label the funding named, part of its once-only key.';
COMMENT ON COLUMN billing.credit_grants.source_id IS 'The funding''s own id: the caller''s key, or the payment that bought the lot. Unique per customer.';
COMMENT ON COLUMN billing.credit_grants.invoker IS 'Who funded the lot: the customer, or the staff or host that granted it.';
COMMENT ON COLUMN billing.credit_grants.revoked_at IS 'When the lot''s live revocation took effect; kept by its credit_grant_events.';
COMMENT ON COLUMN billing.credit_grants.units_credited IS 'Units the ledger credited to the lot on its balance account: deposits, bonuses, restores and reinstatements. Kept by ledger_transfers_apply_lot_counters.';
COMMENT ON COLUMN billing.credit_grants.units_debited IS 'Units the ledger debited from the lot on its balance account: spends, repayments, expiries, revocations and refunds. Remaining is units_credited - units_debited.';
COMMENT ON COLUMN billing.credit_grants.units_reserved IS 'Units pending refunds of the lot''s payment will withdraw: its share of the refunds'' cash, rounded up. Available is remaining - units_reserved.';

CREATE UNIQUE INDEX credit_grants_customer_id_source_id_key ON billing.credit_grants (merchant_id, customer_id, source_id)
    WHERE source_id IS NOT NULL;
COMMENT ON INDEX billing.credit_grants_customer_id_source_id_key IS 'A funding happens at most once at its key (merchant, customer, source_id), whatever its source label.';
CREATE UNIQUE INDEX credit_grants_payment_id_key ON billing.credit_grants (merchant_id, payment_id)
    WHERE payment_id IS NOT NULL;
CREATE INDEX credit_grants_customer_id_created_at_idx ON billing.credit_grants (merchant_id, customer_id, created_at, id);
CREATE INDEX credit_grants_created_at_idx ON billing.credit_grants (merchant_id, created_at);
CREATE INDEX credit_grants_product_id_idx ON billing.credit_grants (merchant_id, product_id)
    WHERE product_id IS NOT NULL;
CREATE INDEX credit_grants_live_idx ON billing.credit_grants (merchant_id, customer_id, unit, expires_at)
    WHERE revoked_at IS NULL AND units_credited > units_debited;
COMMENT ON INDEX billing.credit_grants_live_idx IS 'Lots with units left: spending, expiry and balance watches read only these.';
CREATE INDEX credit_grants_reserved_idx ON billing.credit_grants (merchant_id, customer_id, unit)
    WHERE units_reserved > 0;
CREATE INDEX credit_grants_unposted_idx ON billing.credit_grants (merchant_id, created_at, id)
    WHERE units_credited = 0;
CREATE INDEX credit_grants_revoked_idx ON billing.credit_grants (merchant_id, customer_id)
    WHERE revoked_at IS NOT NULL AND units_credited > units_debited;

-- A lot whose deposit never posted may have no balance account yet.
INSERT INTO billing.ledger_accounts (merchant_id, customer_id, account_type, currency, debits_must_not_exceed_credits, credits_must_not_exceed_debits)
SELECT DISTINCT g.merchant_id, g.customer_id, 'customer_balance', g.currency, true, false
FROM billing.grants g
WHERE g.kind = 'credit' AND g.event = 'grant'
  AND NOT EXISTS (SELECT 1 FROM billing.ledger_accounts a
      WHERE a.merchant_id = g.merchant_id AND a.customer_id = g.customer_id
        AND a.account_type = 'customer_balance' AND a.currency = g.currency);

INSERT INTO billing.credit_grants (merchant_id, id, customer_id, account_id, unit, units, funding_source, paid_amount, paid_currency,
    source, source_id, invoker, product_id, order_line_id, payment_id, description, starts_at, expires_at, revoked_at,
    units_credited, units_debited, created_at)
SELECT g.merchant_id, g.id, g.customer_id, a.id, g.currency, g.amount,
       CASE WHEN paid.amount IS NULL THEN 'grant'
            WHEN line.billing_interval_hours IS NOT NULL OR line.billing_interval_months IS NOT NULL THEN 'recurring'
            ELSE 'purchase' END,
       paid.amount, CASE WHEN paid.amount IS NOT NULL THEN g.currency END,
       COALESCE(NULLIF(g.spec_snapshot -> 'deposit' ->> 'source', ''), g.source_type),
       g.source_id,
       COALESCE(NULLIF(g.spec_snapshot -> 'deposit' ->> 'invoker', ''), g.customer_id::text),
       g.product_id, line.id, g.payment_id, NULLIF(g.reason, ''), g.starts_at, g.ends_at, revoke.starts_at,
       COALESCE(legs.credited, 0), COALESCE(legs.debited, 0), g.created_at
FROM billing.grants g
JOIN billing.ledger_accounts a ON a.merchant_id = g.merchant_id AND a.customer_id = g.customer_id
    AND a.account_type = 'customer_balance' AND a.currency = g.currency
-- A deposit labelled a purchase (a charge's change among them) was paid at
-- face; credits_sold counted it so.
CROSS JOIN LATERAL (SELECT COALESCE((g.spec_snapshot -> 'deposit' ->> 'paid_amount')::bigint,
    CASE WHEN g.source_type = 'purchase' THEN g.amount END) AS amount) paid
LEFT JOIN LATERAL (SELECT l.id, l.billing_interval_hours, l.billing_interval_months
    FROM billing.payments p JOIN billing.order_lines l ON l.merchant_id = p.merchant_id AND l.order_id = p.order_id
    WHERE p.merchant_id = g.merchant_id AND p.id = g.payment_id AND l.product_id = g.product_id AND l.credit_grant IS NOT NULL
    ORDER BY l.position LIMIT 1) line ON true
LEFT JOIN billing.grants revoke ON revoke.merchant_id = g.merchant_id AND revoke.supersedes_id = g.id AND revoke.event = 'revoke'
LEFT JOIN LATERAL (SELECT sum(t.amount) FILTER (WHERE t.credit_account_id = a.id) AS credited,
                          sum(t.amount) FILTER (WHERE t.debit_account_id = a.id) AS debited
    FROM billing.ledger_transfers t WHERE t.merchant_id = g.merchant_id AND t.grant_id = g.id) legs ON true
WHERE g.kind = 'credit' AND g.event = 'grant';

CREATE TABLE billing.credit_grant_events (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    id uuid NOT NULL DEFAULT uuidv7(),
    credit_grant_id uuid NOT NULL,
    event text NOT NULL CHECK (event IN ('revoke', 'reinstate')),
    effective_at timestamptz NOT NULL,
    reason text CHECK (reason <> ''),
    actor text CHECK (octet_length(actor) BETWEEN 1 AND 255),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT credit_grant_events_credit_grant_id_fkey FOREIGN KEY (merchant_id, credit_grant_id) REFERENCES billing.credit_grants (merchant_id, id)
);
COMMENT ON TABLE billing.credit_grant_events IS 'Append-only revocations and reinstatements of credit lots; each sets or clears its lot''s revoked_at. The units they move are ledger transfers. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.credit_grant_events.effective_at IS 'When the event takes effect (valid time); created_at is when it was recorded.';
CREATE INDEX credit_grant_events_credit_grant_id_idx ON billing.credit_grant_events (merchant_id, credit_grant_id, created_at);

INSERT INTO billing.credit_grant_events (merchant_id, id, credit_grant_id, event, effective_at, reason, actor, created_at)
SELECT t.merchant_id, t.id, t.supersedes_id, 'revoke', t.starts_at, NULLIF(t.reason, ''), t.actor, t.created_at
FROM billing.grants t
WHERE t.kind = 'credit' AND t.event = 'revoke';

ALTER TABLE billing.ledger_transfers RENAME COLUMN grant_id TO credit_grant_id;
ALTER INDEX billing.ledger_transfers_grant_id_idx RENAME TO ledger_transfers_credit_grant_id_idx;
ALTER INDEX billing.ledger_transfers_grant_id_transfer_type_key RENAME TO ledger_transfers_credit_grant_id_transfer_type_key;
ALTER TABLE billing.ledger_transfers ADD CONSTRAINT ledger_transfers_credit_grant_id_fkey
    FOREIGN KEY (merchant_id, credit_grant_id) REFERENCES billing.credit_grants (merchant_id, id);
COMMENT ON COLUMN billing.ledger_transfers.credit_grant_id IS 'The credit lot the leg moves units of, or attributes money to. order_id and customer_id carry no foreign keys: the append-only ledger never blocks or cascades on business rows.';
-- What the pending refunds of a purchased lot's payment will withdraw of it:
-- their share of its paid amount, up to what no reversal returned, rounded up.
CREATE FUNCTION billing.credit_grant_reservation(p_merchant uuid, p_lot uuid) RETURNS bigint
LANGUAGE sql STABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
    SELECT COALESCE((
        SELECT LEAST(g.units, ceil(g.units::numeric * GREATEST(LEAST(pending.amount, g.paid_amount - returned.amount), 0) / g.paid_amount))::bigint
        FROM billing.credit_grants g
        CROSS JOIN LATERAL (SELECT sum(-r.amount)::numeric AS amount FROM billing.payments r
            WHERE r.merchant_id = g.merchant_id AND r.refunded_payment_id = g.payment_id AND r.currency = g.paid_currency
              AND r.status = 'pending' AND r.amount < 0 AND r.deleted_at IS NULL) pending
        CROSS JOIN LATERAL (SELECT COALESCE(sum(CASE t.transfer_type WHEN 'credit_refund_cash' THEN t.amount WHEN 'credit_refund_cash_restore' THEN -t.amount ELSE 0 END), 0)::numeric AS amount
            FROM billing.ledger_transfers t
            WHERE t.merchant_id = g.merchant_id AND t.credit_grant_id = g.id AND t.operation = 'purchased_credit_refund') returned
        WHERE g.merchant_id = p_merchant AND g.id = p_lot AND g.payment_id IS NOT NULL AND g.paid_amount IS NOT NULL
          AND pending.amount > 0), 0)
$$;
UPDATE billing.credit_grants SET units_reserved = billing.credit_grant_reservation(merchant_id, id)
WHERE payment_id IS NOT NULL AND paid_amount IS NOT NULL;

COMMENT ON INDEX billing.ledger_transfers_operation_once_key IS 'The structural once-only key for EVERY transfer type. ledger.ApplyIdempotent inserts ON CONFLICT DO NOTHING against this index, so a replay is refused by the database rather than by a check-then-insert in Go. credit_grant_id is part of the identity (one debit per operation per FIFO lot); NULLS NOT DISTINCT because the owed/payment legs carry no lot and system transfers carry no customer.';

-- The lots and their revocations moved; grants keeps access and history.
ALTER TABLE billing.grants DISABLE TRIGGER immutable_grants;
DELETE FROM billing.grants WHERE kind = 'credit';
ALTER TABLE billing.grants ENABLE TRIGGER immutable_grants;

DROP INDEX billing.grants_created_at_idx;
DROP INDEX billing.grants_customer_id_currency_starts_at_ends_at_idx;
DROP INDEX billing.grants_customer_id_source_id_key;
DROP INDEX billing.grants_ends_at_idx;
DROP INDEX billing.grants_purchased_credit_payment_key;
ALTER TABLE billing.grants DROP CONSTRAINT grants_credit_amount_check;
ALTER TABLE billing.grants DROP CONSTRAINT grants_kind_check;
ALTER TABLE billing.grants ADD CONSTRAINT grants_kind_check CHECK (kind IN ('entitlement', 'ownership', 'access'));
ALTER TABLE billing.grants DROP COLUMN amount;
ALTER TABLE billing.grants DROP COLUMN currency;
COMMENT ON TABLE billing.grants IS 'Append-only grant ledger. Immutable events (grant/revoke/expire/supersede); the live product access windows and ownership are DERIVED projections folded from this log. Credit lots are credit_grants. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.grants.kind IS 'access: a product for a window, whose keys are read from the product at check time. entitlement and ownership are history from before product access.';
COMMENT ON COLUMN billing.grants.spec_snapshot IS 'The keys a historical entitlement grant carried; NULL on access grants.';

-- Counters, revocations and their guard.
CREATE FUNCTION billing.guard_credit_grant_facts() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF (NEW.units_credited <> 0 OR NEW.units_debited <> 0 OR NEW.units_reserved <> 0 OR NEW.revoked_at IS NOT NULL)
           AND NOT billing.billing_restore_active(NEW.merchant_id) THEN
            RAISE EXCEPTION 'a credit grant starts with no posted, reserved or revoked units' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    -- Only a nested trigger moves the counters and revoked_at: the transfer
    -- and event inserts are their writers. units_reserved is state.
    IF TG_OP = 'DELETE'
       OR (to_jsonb(NEW) - ARRAY['units_credited', 'units_debited', 'units_reserved', 'revoked_at']) IS DISTINCT FROM
          (to_jsonb(OLD) - ARRAY['units_credited', 'units_debited', 'units_reserved', 'revoked_at'])
       OR (pg_trigger_depth() < 2 AND (NEW.units_credited, NEW.units_debited, NEW.revoked_at) IS DISTINCT FROM (OLD.units_credited, OLD.units_debited, OLD.revoked_at)) THEN
        RAISE EXCEPTION 'credit grant facts are immutable; its ledger transfers and events keep its counters' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER guard_credit_grant_facts BEFORE INSERT OR DELETE OR UPDATE ON billing.credit_grants
    FOR EACH ROW EXECUTE FUNCTION billing.guard_credit_grant_facts();
CREATE TRIGGER immutable_credit_grants_truncate BEFORE TRUNCATE ON billing.credit_grants
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();

CREATE FUNCTION billing.credit_grant_events_apply() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    UPDATE billing.credit_grants
    SET revoked_at = CASE WHEN NEW.event = 'revoke' THEN NEW.effective_at END
    WHERE merchant_id = NEW.merchant_id AND id = NEW.credit_grant_id
      AND (revoked_at IS NULL) = (NEW.event = 'revoke');
    IF NOT FOUND THEN
        RAISE EXCEPTION 'a credit grant is revoked only while live and reinstated only while revoked' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_credit_grant_events_apply AFTER INSERT ON billing.credit_grant_events
    FOR EACH ROW EXECUTE FUNCTION billing.credit_grant_events_apply();
CREATE TRIGGER immutable_credit_grant_events BEFORE DELETE OR UPDATE ON billing.credit_grant_events
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER immutable_credit_grant_events_truncate BEFORE TRUNCATE ON billing.credit_grant_events
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();

-- A leg on its lot's balance account moves the lot's counters; a leg that
-- names the lot on another account (a cash or revenue leg) does not.
CREATE FUNCTION billing.ledger_transfers_apply_lot_counters() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    UPDATE billing.credit_grants
    SET units_credited = units_credited + CASE WHEN account_id = NEW.credit_account_id THEN NEW.amount ELSE 0 END,
        units_debited = units_debited + CASE WHEN account_id = NEW.debit_account_id THEN NEW.amount ELSE 0 END
    WHERE merchant_id = NEW.merchant_id AND id = NEW.credit_grant_id
      AND account_id IN (NEW.debit_account_id, NEW.credit_account_id);
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_ledger_transfers_apply_lot_counters AFTER INSERT ON billing.ledger_transfers
    FOR EACH ROW WHEN (NEW.credit_grant_id IS NOT NULL) EXECUTE FUNCTION billing.ledger_transfers_apply_lot_counters();

-- A restore skips the triggers: its lots must agree with its legs and events.
CREATE FUNCTION billing.check_billing_restore_credit_grants(p_merchant uuid) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF p_merchant IS DISTINCT FROM billing.current_merchant_id() THEN
        RAISE EXCEPTION 'billing restore merchant mismatch' USING ERRCODE = '42501';
    END IF;
    IF EXISTS (SELECT 1 FROM billing.credit_grants g
        CROSS JOIN LATERAL (SELECT COALESCE(sum(t.amount) FILTER (WHERE t.credit_account_id = g.account_id), 0) AS credited,
                                   COALESCE(sum(t.amount) FILTER (WHERE t.debit_account_id = g.account_id), 0) AS debited
            FROM billing.ledger_transfers t WHERE t.merchant_id = g.merchant_id AND t.credit_grant_id = g.id) l
        LEFT JOIN LATERAL (SELECT e.event, e.effective_at FROM billing.credit_grant_events e
            WHERE e.merchant_id = g.merchant_id AND e.credit_grant_id = g.id
            ORDER BY e.created_at DESC, e.id DESC LIMIT 1) e ON true
        WHERE g.merchant_id = p_merchant
          AND (g.units_credited <> l.credited OR g.units_debited <> l.debited
               OR g.revoked_at IS DISTINCT FROM CASE WHEN e.event = 'revoke' THEN e.effective_at END)) THEN
        RAISE EXCEPTION 'billing restore credit grant integrity mismatch' USING ERRCODE = '23514';
    END IF;
END;
$$;
REVOKE ALL ON FUNCTION billing.check_billing_restore_credit_grants(p_merchant uuid) FROM PUBLIC;

CREATE OR REPLACE FUNCTION billing.require_finished_billing_restore() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.kind='billing_restore' THEN
        IF NOT EXISTS (SELECT 1 FROM billing.maintenance_runs WHERE id=NEW.id AND merchant_id=NEW.merchant_id AND status='completed') THEN
            RAISE EXCEPTION 'unfinished billing restore receipts cannot commit' USING ERRCODE='23514';
        END IF;
        PERFORM billing.check_billing_restore_ledger(NEW.merchant_id);
        PERFORM billing.check_billing_restore_credit_grants(NEW.merchant_id);
    END IF;
    RETURN NULL;
END;
$$;

-- The restore guard names the lots and their events.
CREATE OR REPLACE FUNCTION billing.guard_billing_restore_receipt() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $_$
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
                  'balance_watches',
                  'carts',
                  'catalog_applications',
                  'catalog_field_owners',
                  'catalog_limits',
                  'catalog_meters',
                  'catalog_product_limits',
                  'catalog_rate_cards',
                  'catalog_restore_receipts',
                  'credit_grant_events',
                  'credit_grants',
                  'custodians',
                  'customer_delinquency',
                  'customer_invoice_profiles',
                  'customer_limit_overrides',
                  'customer_low_balance_lines',
                  'customer_messages',
                  'customers',
                  'dashboard_configs',
                  'destructive_actions',
                  'destructive_holds',
                  'destructive_run_before_images',
                  'eventlog_events',
                  'failed_usage_windows',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'leases',
                  'ledger_accounts',
                  'ledger_transfers',
                  'limit_adjustments',
                  'limit_hold_parts',
                  'limit_holds',
                  'limit_windows',
                  'maintenance_runs',
                  'mandates',
                  'merchant_destructive_policy',
                  'meter_period_quantities',
                  'money_settings',
                  'nmi_bulk_checkpoints',
                  'nmi_history_months',
                  'nmi_history_reads',
                  'notification_events',
                  'order_attempts',
                  'order_lines',
                  'orders',
                  'owed_items',
                  'ownership_claims',
                  'payment_attempts',
                  'payment_method_versions',
                  'payment_methods',
                  'payments',
                  'price_key_movements',
                  'price_keys',
                  'price_migrations',
                  'price_psp_bindings',
                  'prices',
                  'product_access',
                  'product_entitlements',
                  'products',
                  'provider_bills',
                  'provider_charge_attributions',
                  'provider_cycle_receipts',
                  'provider_intents',
                  'provider_mutation_logs',
                  'provider_schedules',
                  'psp_customers',
                  'psp_refresh_watermarks',
                  'psps',
                  'rebill_cycles',
                  'reconciliation_findings',
                  'reconciliation_state',
                  'running_charges',
                  'scheduled_changes',
                  'solana_pay_receipts',
                  'solana_pay_references',
                  'solana_subscriptions',
                  'stored_card_checks',
                  'subscription_status_transitions',
                  'subscription_billing_changes',
                  'subscription_verifications',
                  'subscriptions',
                  'usage_accruals',
                  'usage_daily',
                  'usage_minutes',
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
    END IF;
    RETURN NEW;
END;
$_$;
