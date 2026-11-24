-- Credit books: a credit lot's money in the general ledger. A paid lot's price
-- is deferred revenue (deferred_revenue_credit) released at the lot's rate as
-- its units are spent, expire, are revoked or refunded; the lot counts the
-- legs on its customer's deferred account in its paid currency.

ALTER TABLE billing.credit_grants
    ADD COLUMN deferred_credited bigint NOT NULL DEFAULT 0,
    ADD COLUMN deferred_debited bigint NOT NULL DEFAULT 0,
    ADD CONSTRAINT credit_grants_deferred_check CHECK (deferred_debited >= 0 AND deferred_debited <= deferred_credited);
COMMENT ON COLUMN billing.credit_grants.deferred_credited IS 'Money of the lot''s price the general ledger deferred: legs crediting its customer''s deferred_revenue_credit in paid_currency (in unit for a lot given back at face, a usage reversal) that name the lot. Kept by ledger_transfers_apply_lot_deferred.';
COMMENT ON COLUMN billing.credit_grants.deferred_debited IS 'Money of the lot released from deferral: spent, broken, forfeited or refunded. deferred_credited - deferred_debited is what its remaining units are worth at its rate.';

-- A general-ledger leg on the lot's deferred account moves its money
-- counters; one that names the lot on another account does not.
CREATE FUNCTION billing.ledger_transfers_apply_lot_deferred() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN RETURN NEW; END IF;
    UPDATE billing.credit_grants g
    SET deferred_credited = g.deferred_credited + CASE WHEN a.id = NEW.credit_account_id THEN NEW.amount ELSE 0 END,
        deferred_debited = g.deferred_debited + CASE WHEN a.id = NEW.debit_account_id THEN NEW.amount ELSE 0 END
    FROM billing.ledger_accounts a
    WHERE g.merchant_id = NEW.merchant_id AND g.id = NEW.credit_grant_id
      AND a.merchant_id = NEW.merchant_id AND a.id IN (NEW.debit_account_id, NEW.credit_account_id)
      AND a.account_type = 'deferred_revenue_credit' AND a.customer_id = g.customer_id AND a.currency = COALESCE(g.paid_currency, g.unit);
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_ledger_transfers_apply_lot_deferred AFTER INSERT ON billing.ledger_transfers
    FOR EACH ROW WHEN (NEW.book = 'general' AND NEW.credit_grant_id IS NOT NULL) EXECUTE FUNCTION billing.ledger_transfers_apply_lot_deferred();

-- The money counters are the ledger's, like the unit counters.
CREATE OR REPLACE FUNCTION billing.guard_credit_grant_facts() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF (NEW.units_credited <> 0 OR NEW.units_debited <> 0 OR NEW.units_reserved <> 0 OR NEW.revoked_at IS NOT NULL
            OR NEW.deferred_credited <> 0 OR NEW.deferred_debited <> 0)
           AND NOT billing.billing_restore_active(NEW.merchant_id) THEN
            RAISE EXCEPTION 'a credit grant starts with no posted, reserved, deferred or revoked units' USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;
    -- Only a nested trigger moves the counters and revoked_at: the transfer
    -- and event inserts are their writers. units_reserved is state.
    IF TG_OP = 'DELETE'
       OR (to_jsonb(NEW) - ARRAY['units_credited', 'units_debited', 'units_reserved', 'revoked_at', 'deferred_credited', 'deferred_debited']) IS DISTINCT FROM
          (to_jsonb(OLD) - ARRAY['units_credited', 'units_debited', 'units_reserved', 'revoked_at', 'deferred_credited', 'deferred_debited'])
       OR (pg_trigger_depth() < 2 AND (NEW.units_credited, NEW.units_debited, NEW.revoked_at, NEW.deferred_credited, NEW.deferred_debited)
           IS DISTINCT FROM (OLD.units_credited, OLD.units_debited, OLD.revoked_at, OLD.deferred_credited, OLD.deferred_debited)) THEN
        RAISE EXCEPTION 'credit grant facts are immutable; its ledger transfers and events keep its counters' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;

-- A paid lot defers its price in the transaction that records it: its
-- credit_issue leg exists at commit. A restore brings lots and legs as they
-- were, a lot from before the credit books with none.
CREATE FUNCTION billing.credit_grants_require_books() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) OR NEW.paid_amount IS NULL THEN RETURN NULL; END IF;
    IF NOT EXISTS (SELECT 1 FROM billing.ledger_transfers
        WHERE merchant_id = NEW.merchant_id AND credit_grant_id = NEW.id AND book = 'general' AND operation = 'credit_issue') THEN
        RAISE EXCEPTION 'credit_books_unposted: paid credit lot % has no general-ledger issue', NEW.id USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER credit_grants_require_books AFTER INSERT ON billing.credit_grants DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION billing.credit_grants_require_books();

-- A restore runs its lots' check while it is still active.
CREATE OR REPLACE FUNCTION billing.finish_billing_restore(p_merchant uuid, p_digest text, p_rows bigint) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $_$
BEGIN
    IF NOT billing.billing_restore_active(p_merchant) OR p_digest IS NULL OR p_rows IS NULL OR p_digest !~ '^[0-9a-f]{64}$' OR p_rows<0 THEN
        RAISE EXCEPTION 'invalid billing restore finalization' USING ERRCODE='23514';
    END IF;
    SET CONSTRAINTS billing.ledger_rollups_settle IMMEDIATE;
    SET CONSTRAINTS billing.credit_grants_require_books IMMEDIATE;
    UPDATE billing.maintenance_runs SET status='completed',finished_at=now(),summary=jsonb_build_object('digest',p_digest,'rows',p_rows)
        WHERE merchant_id=p_merchant AND kind='billing_restore' AND id::text=current_setting('openrails.billing_restore_id',true);
END;
$_$;

-- A reinstated lot can be revoked again: credit_revoke is no longer once per
-- lot.
DROP INDEX billing.ledger_transfers_credit_grant_id_transfer_type_key;
CREATE UNIQUE INDEX ledger_transfers_credit_grant_id_transfer_type_key ON billing.ledger_transfers (merchant_id, credit_grant_id, transfer_type)
    WHERE credit_grant_id IS NOT NULL AND transfer_type IN ('deposit', 'deposit_bonus', 'credit_purchase_revenue', 'credit_expire');

-- What reversals returned of a lot's price: the units book's cash legs before
-- the credit books, the general ledger's cash legs since.
CREATE OR REPLACE FUNCTION billing.credit_grant_reservation(p_merchant uuid, p_lot uuid) RETURNS bigint
LANGUAGE sql STABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
    SELECT COALESCE((
        SELECT LEAST(g.units, ceil(g.units::numeric * GREATEST(LEAST(pending.amount, g.paid_amount - returned.amount), 0) / g.paid_amount))::bigint
        FROM billing.credit_grants g
        CROSS JOIN LATERAL (SELECT sum(-r.amount)::numeric AS amount FROM billing.payments r
            WHERE r.merchant_id = g.merchant_id AND r.refunded_payment_id = g.payment_id AND r.currency = g.paid_currency
              AND r.status = 'pending' AND r.amount < 0 AND r.deleted_at IS NULL) pending
        CROSS JOIN LATERAL (SELECT COALESCE(sum(CASE
                WHEN t.book = 'units' AND t.transfer_type = 'credit_refund_cash' THEN t.amount
                WHEN t.book = 'units' AND t.transfer_type = 'credit_refund_cash_restore' THEN -t.amount
                WHEN t.book = 'general' AND c.account_type IN ('cash_clearing', 'external_receipts') THEN t.amount
                WHEN t.book = 'general' AND d.account_type IN ('cash_clearing', 'external_receipts') THEN -t.amount
                ELSE 0 END), 0)::numeric AS amount
            FROM billing.ledger_transfers t
            JOIN billing.ledger_accounts d ON d.merchant_id = t.merchant_id AND d.id = t.debit_account_id
            JOIN billing.ledger_accounts c ON c.merchant_id = t.merchant_id AND c.id = t.credit_account_id
            WHERE t.merchant_id = g.merchant_id AND t.credit_grant_id = g.id AND t.operation = 'purchased_credit_refund') returned
        WHERE g.merchant_id = p_merchant AND g.id = p_lot AND g.payment_id IS NOT NULL AND g.paid_amount IS NOT NULL
          AND pending.amount > 0), 0)
$$;

-- A restore skips the triggers: its lots must agree with their legs, money
-- included, and events.
CREATE OR REPLACE FUNCTION billing.check_billing_restore_credit_grants(p_merchant uuid) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF p_merchant IS DISTINCT FROM billing.current_merchant_id() THEN
        RAISE EXCEPTION 'billing restore merchant mismatch' USING ERRCODE = '42501';
    END IF;
    IF EXISTS (SELECT 1 FROM billing.credit_grants g
        CROSS JOIN LATERAL (SELECT COALESCE(sum(t.amount) FILTER (WHERE t.credit_account_id = g.account_id), 0) AS credited,
                                   COALESCE(sum(t.amount) FILTER (WHERE t.debit_account_id = g.account_id), 0) AS debited
            FROM billing.ledger_transfers t WHERE t.merchant_id = g.merchant_id AND t.credit_grant_id = g.id) l
        CROSS JOIN LATERAL (SELECT COALESCE(sum(t.amount) FILTER (WHERE t.credit_account_id = a.id), 0) AS credited,
                                   COALESCE(sum(t.amount) FILTER (WHERE t.debit_account_id = a.id), 0) AS debited
            FROM billing.ledger_transfers t
            JOIN billing.ledger_accounts a ON a.merchant_id = t.merchant_id AND a.id IN (t.debit_account_id, t.credit_account_id)
                AND a.account_type = 'deferred_revenue_credit' AND a.customer_id = g.customer_id AND a.currency = COALESCE(g.paid_currency, g.unit)
            WHERE t.merchant_id = g.merchant_id AND t.credit_grant_id = g.id AND t.book = 'general') m
        LEFT JOIN LATERAL (SELECT e.event, e.effective_at FROM billing.credit_grant_events e
            WHERE e.merchant_id = g.merchant_id AND e.credit_grant_id = g.id
            ORDER BY e.created_at DESC, e.id DESC LIMIT 1) e ON true
        WHERE g.merchant_id = p_merchant
          AND (g.units_credited <> l.credited OR g.units_debited <> l.debited
               OR g.deferred_credited <> m.credited OR g.deferred_debited <> m.debited
               OR g.revoked_at IS DISTINCT FROM CASE WHEN e.event = 'revoke' THEN e.effective_at END)) THEN
        RAISE EXCEPTION 'billing restore credit grant integrity mismatch' USING ERRCODE = '23514';
    END IF;
END;
$$;
