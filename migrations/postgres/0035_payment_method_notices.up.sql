-- or#870 bucket 2: the notification LADDER for a subscription parked on a
-- payment method the rail says cannot be charged (expired card, bad CVC, call
-- issuer, pick-up/lost card...).
--
-- Bucket 2 stops charging on purpose. That removes the only clock the customer
-- was ever on: no charge attempt means no failure event means no notification.
-- Before this table a bucket-2 park produced exactly ONE notice, at the moment
-- of the decline, and then silence — the precise failure mode the doctrine
-- names ("silence through a whole dunning cycle followed by a sudden
-- cancellation"). This is the durable ladder: rung 0 at park time, then the
-- later rungs, each idempotent, each a real customer-facing reminder that their
-- access is still on and one card update ends the problem.
--
-- It is a work QUEUE, not a scan. One row exists only while a subscription is
-- parked awaiting a card, and the due predicate is indexed, so the ladder's
-- cost scales with parked customers — never with subscriptions on file (Paul's
-- standing law: work scales with activity, not records).
--
-- Nothing in this table can destroy anything. It sends notices. The stored
-- payment method is untouched by every path that reads or writes it, and the
-- ladder never cancels: running out of rungs closes the row as `exhausted` and
-- leaves the subscription exactly where it was, entitlements intact.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

CREATE TABLE openrails.payment_method_notices (
    id uuid DEFAULT uuidv7() NOT NULL PRIMARY KEY,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    rail text NOT NULL,
    -- The rail's decline code VERBATIM (#733 doctrine): what made this bucket 2.
    failure_code text,
    -- When the ladder started. Every rung offset is anchored here, so a late
    -- worker run never compounds the delay into the following rungs.
    parked_at timestamp with time zone NOT NULL,
    -- Rungs already SENT. 1 immediately after the park (the decline itself
    -- notifies). Advanced in the SAME transaction as the notification insert,
    -- which is what makes a rung exactly-once under crash or concurrency.
    rungs_sent bigint DEFAULT 1 NOT NULL,
    -- The due predicate. NULL = no rung left to send.
    next_notice_at timestamp with time zone,
    resolved_at timestamp with time zone,
    -- recovered (they fixed it) | ended (the subscription is over) | exhausted
    -- (the ladder ran out; nothing else happens).
    resolution text,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT chk_payment_method_notices_rungs_sent CHECK ((rungs_sent >= 1)),
    CONSTRAINT chk_payment_method_notices_resolved CHECK (((resolved_at IS NULL) = (resolution IS NULL))),
    -- An open ladder is either due at some point or exhausted-and-closed; a row
    -- with neither a next rung nor a resolution is a stuck ladder by construction.
    CONSTRAINT chk_payment_method_notices_open_has_next CHECK (((resolved_at IS NOT NULL) OR (next_notice_at IS NOT NULL)))
);

COMMENT ON TABLE openrails.payment_method_notices IS 'or#870 bucket 2: one open row per subscription parked awaiting a payment-method fix, driving the notification ladder. Sends notices only — no path from this table cancels a subscription or touches a stored payment method.';

ALTER TABLE ONLY openrails.payment_method_notices FORCE ROW LEVEL SECURITY;

-- One ladder per subscription. Merchant-scoped (ID-11): the unique can never
-- collide across tenants, so it is not an existence oracle.
ALTER TABLE ONLY openrails.payment_method_notices
    ADD CONSTRAINT uq_payment_method_notices_subscription UNIQUE (merchant_id, subscription_id);

ALTER TABLE ONLY openrails.payment_method_notices
    ADD CONSTRAINT payment_method_notices_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.payment_method_notices
    ADD CONSTRAINT payment_method_notices_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id) ON DELETE CASCADE;

ALTER TABLE ONLY openrails.payment_method_notices
    ADD CONSTRAINT payment_method_notices_subscription_fk FOREIGN KEY (subscription_id) REFERENCES openrails.subscriptions(id) ON DELETE CASCADE;

-- The due-work index: open ladders only, ordered by when their next rung falls
-- due. This is what makes the sweep cost proportional to parked customers.
CREATE INDEX idx_payment_method_notices_due ON openrails.payment_method_notices USING btree (next_notice_at)
    WHERE ((resolved_at IS NULL) AND (next_notice_at IS NOT NULL));

CREATE INDEX idx_payment_method_notices_customer ON openrails.payment_method_notices USING btree (merchant_id, customer_id);

ALTER TABLE openrails.payment_method_notices ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.payment_method_notices USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.payment_method_notices TO openrails_app;

-- --------------------------------------------------------------------------
-- Cross-merchant fan-out (same contract as 0022/0023)
-- --------------------------------------------------------------------------

-- Merchants holding a ladder with a rung due. Ids ONLY: the rows themselves,
-- the subscription reads and the notification inserts all happen per-merchant
-- inside RunInMerchantScope, under the merchant's own policy. Asserts its
-- definer bypasses RLS so a mis-owned schema RAISES instead of silently
-- answering "no work to do" — the or#877 failure mode.
CREATE FUNCTION openrails.due_payment_method_notice_merchant_ids(p_now timestamptz, p_limit int)
    RETURNS TABLE (merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT DISTINCT n.merchant_id
      FROM openrails.payment_method_notices n
     WHERE n.resolved_at IS NULL
       AND n.next_notice_at IS NOT NULL
       AND n.next_notice_at <= p_now
     LIMIT p_limit;
END;
$$;

COMMENT ON FUNCTION openrails.due_payment_method_notice_merchant_ids(timestamptz, int) IS
    'Merchants with a due or#870 bucket-2 notice rung — the fan-out list for PaymentMethodNoticeWorker. Ids only; every rung is sent per-merchant under RunInMerchantScope.';

REVOKE ALL ON FUNCTION openrails.due_payment_method_notice_merchant_ids(timestamptz, int) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION openrails.due_payment_method_notice_merchant_ids(timestamptz, int) TO openrails_app;
