-- Chargebacks and refunds: each provider dispute and issuer fraud report as a
-- record, a refund's reason, a disputed card, and per PSP, card network,
-- currency and UTC day what the card networks' monitoring programs divide.

CREATE FUNCTION billing.card_network(p_brand text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT CASE lower(regexp_replace(coalesce(p_brand, ''), '[^A-Za-z]', '', 'g'))
    WHEN '' THEN 'unknown'
    WHEN 'unknown' THEN 'unknown'
    WHEN 'visa' THEN 'visa'
    WHEN 'mastercard' THEN 'mastercard'
    WHEN 'mc' THEN 'mastercard'
    WHEN 'amex' THEN 'amex'
    WHEN 'americanexpress' THEN 'amex'
    WHEN 'discover' THEN 'discover'
    WHEN 'disc' THEN 'discover'
    WHEN 'jcb' THEN 'jcb'
    WHEN 'diners' THEN 'diners'
    WHEN 'dinersclub' THEN 'diners'
    WHEN 'unionpay' THEN 'unionpay'
    WHEN 'chinaunionpay' THEN 'unionpay'
    ELSE 'other'
END
$$;
COMMENT ON FUNCTION billing.card_network(p_brand text) IS 'The card network a brand as a provider spells it belongs to: visa, mastercard, amex, discover, jcb, diners, unionpay, other, or unknown when none is named.';

CREATE TABLE billing.disputes (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    id uuid NOT NULL DEFAULT uuidv7(),
    psp_id uuid NOT NULL,
    rail text NOT NULL,
    provider_ref text NOT NULL CHECK (provider_ref <> ''),
    customer_id uuid,
    payment_id uuid,
    kind text NOT NULL CHECK (kind IN ('inquiry', 'chargeback', 'prevented', 'unknown')),
    network text NOT NULL CHECK (network IN ('visa', 'mastercard', 'amex', 'discover', 'jcb', 'diners', 'unionpay', 'other', 'unknown')),
    reason text NOT NULL CHECK (reason IN ('fraud', 'unrecognized', 'not_received', 'canceled_recurring', 'not_as_described', 'credit_not_processed', 'duplicate', 'processing_error', 'authorization', 'other', 'unknown')),
    reason_code text CHECK (reason_code <> ''),
    provider_reason text CHECK (provider_reason <> ''),
    amount bigint CHECK (amount > 0),
    currency text CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    status text NOT NULL CHECK (status IN ('open', 'won', 'lost', 'closed', 'unreported')),
    disputed_at timestamptz NOT NULL,
    chargeback_at timestamptz,
    evidence_due_at timestamptz,
    closed_at timestamptz,
    reversal_payment_id uuid,
    recovery_payment_id uuid,
    absorbed_refund_ref text CHECK (absorbed_refund_ref <> ''),
    observed_via text NOT NULL CHECK (observed_via IN ('notice', 'pull')),
    responded_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT disputes_psp_id_provider_ref_key UNIQUE (merchant_id, psp_id, provider_ref),
    CONSTRAINT disputes_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps (merchant_id, id, rail) ON DELETE RESTRICT,
    CONSTRAINT disputes_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT disputes_customer_id_payment_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_id) REFERENCES billing.payments (merchant_id, customer_id, id),
    CONSTRAINT disputes_customer_id_reversal_payment_id_fkey FOREIGN KEY (merchant_id, customer_id, reversal_payment_id) REFERENCES billing.payments (merchant_id, customer_id, id),
    CONSTRAINT disputes_customer_id_recovery_payment_id_fkey FOREIGN KEY (merchant_id, customer_id, recovery_payment_id) REFERENCES billing.payments (merchant_id, customer_id, id),
    CONSTRAINT disputes_payment_check CHECK ((payment_id IS NULL) = (customer_id IS NULL)),
    CONSTRAINT disputes_money_check CHECK ((payment_id IS NOT NULL) OR ((reversal_payment_id IS NULL) AND (recovery_payment_id IS NULL))),
    CONSTRAINT disputes_amount_currency_check CHECK ((amount IS NULL) = (currency IS NULL)),
    CONSTRAINT disputes_chargeback_check CHECK ((kind = 'chargeback') = (chargeback_at IS NOT NULL)),
    CONSTRAINT disputes_outcome_check CHECK ((status IN ('open', 'closed')) OR (kind = 'chargeback')),
    CONSTRAINT disputes_closed_check CHECK ((closed_at IS NOT NULL) = (status IN ('won', 'lost', 'closed')))
);
COMMENT ON TABLE billing.disputes IS 'One provider dispute case: an issuer''s inquiry, a chargeback, or a dispute resolved before it was filed. Its money is the payments mirror rows it names. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.disputes.provider_ref IS 'The case as its rail names it: a Stripe dispute id, an NMI chargeback id (else the charge''s transaction id) or a CCBill transaction id (else the subscription id); its reversal''s transaction id is chargeback:<provider_ref> on NMI and CCBill.';
COMMENT ON COLUMN billing.disputes.payment_id IS 'The disputed charge; NULL when the rail''s report matched no single charge (customer_id is NULL with it).';
COMMENT ON COLUMN billing.disputes.kind IS 'inquiry (the issuer asked; nothing was withdrawn), chargeback, prevented (resolved before it was filed: an RDR refund or a Compelling Evidence 3.0 block), or unknown (a status the rail had not reported before: nothing is booked). Only a chargeback counts in a network ratio.';
COMMENT ON COLUMN billing.disputes.network IS 'The card network, as billing.card_network names it.';
COMMENT ON COLUMN billing.disputes.reason IS 'The reason family the network code or the provider''s reason belongs to.';
COMMENT ON COLUMN billing.disputes.reason_code IS 'The network''s reason code (Visa 10.4, Mastercard 4853), verbatim.';
COMMENT ON COLUMN billing.disputes.provider_reason IS 'The provider''s reason, verbatim.';
COMMENT ON COLUMN billing.disputes.amount IS 'The disputed amount in native units of currency; both are NULL when the report named no currency it could be matched in.';
COMMENT ON COLUMN billing.disputes.status IS 'open (awaiting the decision), won, lost, closed (an inquiry or prevention that never became a chargeback), or unreported (the rail reports chargebacks but never their outcome).';
COMMENT ON COLUMN billing.disputes.chargeback_at IS 'When the case became a chargeback: when it opened, or when the issuer escalated its inquiry. It dates the chargeback in the network ratios.';
COMMENT ON COLUMN billing.disputes.reversal_payment_id IS 'The negative payments row that reversed the charge.';
COMMENT ON COLUMN billing.disputes.recovery_payment_id IS 'The positive payments row that returned the money of a won dispute.';
COMMENT ON COLUMN billing.disputes.absorbed_refund_ref IS 'The provider refund an RDR resolution took as its own (Stripe''s re_ id), so each refund is booked once.';
COMMENT ON COLUMN billing.disputes.observed_via IS 'How OpenRails first learned of the case: notice (a provider''s notice) or pull (a reconcile pull); a case a pull found is acted on only once the merchant is armed.';
COMMENT ON COLUMN billing.disputes.responded_at IS 'When the chargeback''s automatic response finished: the charged card blocked and the customer''s other provider-billed subscriptions on it canceled at period end. NULL while it waits.';

CREATE INDEX disputes_disputed_at_id_idx ON billing.disputes (merchant_id, disputed_at DESC, id DESC);
CREATE INDEX disputes_customer_id_disputed_at_idx ON billing.disputes (merchant_id, customer_id, disputed_at DESC)
    WHERE customer_id IS NOT NULL;
CREATE INDEX disputes_payment_id_idx ON billing.disputes (merchant_id, payment_id) WHERE payment_id IS NOT NULL;
CREATE INDEX disputes_chargeback_at_idx ON billing.disputes (merchant_id, chargeback_at) WHERE chargeback_at IS NOT NULL;
CREATE INDEX disputes_closed_at_idx ON billing.disputes (merchant_id, closed_at) WHERE closed_at IS NOT NULL;
CREATE INDEX disputes_waiting_idx ON billing.disputes (merchant_id, id) WHERE kind = 'chargeback' AND responded_at IS NULL;

CREATE TABLE billing.fraud_reports (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    id uuid NOT NULL DEFAULT uuidv7(),
    psp_id uuid NOT NULL,
    rail text NOT NULL,
    provider_ref text NOT NULL CHECK (provider_ref <> ''),
    customer_id uuid NOT NULL,
    payment_id uuid NOT NULL,
    network text NOT NULL CHECK (network IN ('visa', 'mastercard', 'amex', 'discover', 'jcb', 'diners', 'unionpay', 'other', 'unknown')),
    fraud_type text NOT NULL CHECK (fraud_type IN ('card_never_received', 'fraudulent_card_application', 'made_with_counterfeit_card', 'made_with_lost_card', 'made_with_stolen_card', 'misc', 'unauthorized_use_of_card')),
    actionable boolean NOT NULL,
    amount bigint NOT NULL CHECK (amount > 0),
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    reported_at timestamptz NOT NULL,
    action text NOT NULL CHECK (action IN ('pending', 'recorded', 'refunded', 'nothing_to_refund', 'disputed')),
    refund_payment_id uuid,
    ended_at timestamptz,
    responded_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT fraud_reports_psp_id_provider_ref_key UNIQUE (merchant_id, psp_id, provider_ref),
    CONSTRAINT fraud_reports_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps (merchant_id, id, rail) ON DELETE RESTRICT,
    CONSTRAINT fraud_reports_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id),
    CONSTRAINT fraud_reports_customer_id_payment_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_id) REFERENCES billing.payments (merchant_id, customer_id, id),
    CONSTRAINT fraud_reports_customer_id_refund_payment_id_fkey FOREIGN KEY (merchant_id, customer_id, refund_payment_id) REFERENCES billing.payments (merchant_id, customer_id, id),
    CONSTRAINT fraud_reports_refund_check CHECK ((action = 'refunded') = (refund_payment_id IS NOT NULL))
);
COMMENT ON TABLE billing.fraud_reports IS 'An issuer''s report that a charge was fraud (Visa TC40, Mastercard SAFE) as its rail passes it on: a Stripe early fraud warning. Visa counts each in its monitoring ratio, refunded or not. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.fraud_reports.provider_ref IS 'The report as its rail names it: a Stripe early fraud warning id.';
COMMENT ON COLUMN billing.fraud_reports.fraud_type IS 'The kind of fraud the issuer reported, as Stripe names it.';
COMMENT ON COLUMN billing.fraud_reports.actionable IS 'Whether the charge could still be refunded to head off a dispute when the report arrived.';
COMMENT ON COLUMN billing.fraud_reports.amount IS 'The reported charge''s amount, in native units of currency.';
COMMENT ON COLUMN billing.fraud_reports.action IS 'What OpenRails decided: pending (an actionable report the merchant refunds, not yet answered), recorded (the report only: unactionable, or received while the merchant recorded reports only), refunded (the charge in full, refund_payment_id), nothing_to_refund (no refund remained possible) or disputed (an open dispute or an unbooked chargeback decides the charge).';
COMMENT ON COLUMN billing.fraud_reports.ended_at IS 'When the charge was refunded, what it paid for ended and its card blocked; NULL until then.';
COMMENT ON COLUMN billing.fraud_reports.responded_at IS 'When the response finished: with the end, the customer''s other provider-billed subscriptions on the card canceled at period end. NULL while it waits.';

CREATE INDEX fraud_reports_payment_id_idx ON billing.fraud_reports (merchant_id, payment_id);
CREATE INDEX fraud_reports_reported_at_idx ON billing.fraud_reports (merchant_id, reported_at);
CREATE INDEX fraud_reports_waiting_idx ON billing.fraud_reports (merchant_id, id)
    WHERE responded_at IS NULL AND action IN ('pending', 'refunded', 'nothing_to_refund');

-- A refund's reason. Refunds before it are unknown, but for OpenRails' own
-- refunds of a charge taken after cancellation or of a second payment.
ALTER TABLE billing.payments ADD COLUMN refund_reason text;
UPDATE billing.payments SET refund_reason = CASE WHEN metadata ->> 'charge_after_cancel' = 'true' THEN 'charged_after_cancel'
        WHEN metadata ->> 'order_late_payment' = 'true' THEN 'duplicate' ELSE 'unknown' END
    WHERE reversal_kind = 'refund';
ALTER TABLE billing.payments
    ADD CONSTRAINT payments_refund_reason_check CHECK ((refund_reason IS NULL) OR (refund_reason IN ('requested_by_customer', 'duplicate', 'fraudulent', 'fraud_warning', 'dispute_prevention', 'charged_after_cancel', 'other', 'unknown'))),
    ADD CONSTRAINT payments_refund_reason_kind_check CHECK ((refund_reason IS NOT NULL) = ((reversal_kind IS NOT NULL) AND (reversal_kind = 'refund')));
COMMENT ON COLUMN billing.payments.refund_reason IS 'Why a refund was made: requested_by_customer, duplicate, fraudulent, fraud_warning (an issuer''s fraud report), dispute_prevention (resolved before a dispute was filed), charged_after_cancel, other or unknown (a provider dashboard refund that names none). Set exactly on refund rows.';

-- The card a charge was taken from, where the charge names it.
ALTER TABLE billing.payments ADD COLUMN payment_method_id uuid,
    ADD CONSTRAINT payments_customer_id_payment_method_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_method_id) REFERENCES billing.payment_methods (merchant_id, customer_id, id) ON DELETE SET NULL (payment_method_id);
-- Charges so far: the card their approved attempt charged, else their order's.
UPDATE billing.payments p SET payment_method_id = c.payment_method_id
    FROM (
        SELECT p.merchant_id, p.id, COALESCE(
            (SELECT a.payment_method_id FROM billing.payment_attempts a
             WHERE a.merchant_id = p.merchant_id AND a.payment_id = p.id AND a.payment_method_id IS NOT NULL
             ORDER BY a.attempted_at DESC LIMIT 1),
            o.payment_method_id) AS payment_method_id
        FROM billing.payments p
        LEFT JOIN billing.orders o ON o.merchant_id = p.merchant_id AND o.id = p.order_id
        WHERE p.channel = 'rail' AND p.refunded_payment_id IS NULL AND p.amount > 0
    ) c
    JOIN billing.payment_methods m ON m.merchant_id = c.merchant_id AND m.id = c.payment_method_id
    WHERE p.merchant_id = c.merchant_id AND p.id = c.id AND m.customer_id = p.customer_id;
CREATE INDEX payments_payment_method_id_idx ON billing.payments (merchant_id, payment_method_id) WHERE payment_method_id IS NOT NULL;
COMMENT ON COLUMN billing.payments.payment_method_id IS 'The saved card a charge was taken from, when its writer knows it; NULL on reversals, off-rail payments and charges no saved card names.';

-- A card that was charged back is never charged again.
ALTER TABLE billing.payment_methods DROP CONSTRAINT payment_methods_status_check,
    ADD CONSTRAINT payment_methods_status_check CHECK (status IN ('requires_action', 'active', 'closed', 'replaced', 'removed', 'disputed'));
ALTER TABLE billing.mandates DROP CONSTRAINT mandates_end_reason_check,
    ADD CONSTRAINT mandates_end_reason_check CHECK (
        end_reason IN ('replaced', 'brand_changed', 'closed', 'customer_revoked', 'subscription_ended', 'payment_method_removed', 'disputed')
        AND (status = 'revoked') = (end_reason = 'customer_revoked')
    );
COMMENT ON COLUMN billing.mandates.end_reason IS 'Why a revoked or ended mandate stopped: replaced, brand_changed, closed, customer_revoked, subscription_ended, payment_method_removed or disputed (a charge on the card was charged back).';
COMMENT ON COLUMN billing.payment_methods.status IS 'requires_action (being saved: the customer completes the bank''s authentication, setup_ref); active; closed (the bank closed the account); replaced (another method took its place, replaced_by_id); removed; disputed (a charge to it was charged back: it is never charged again). The last four are final.';

CREATE TABLE billing.network_activity_daily (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    psp_id uuid NOT NULL,
    rail text NOT NULL,
    network text NOT NULL,
    currency text NOT NULL CHECK (currency = '' OR currency ~ '^[A-Z0-9]{3,12}$'),
    day_at timestamptz NOT NULL,
    sales bigint NOT NULL DEFAULT 0,
    sales_amount bigint NOT NULL DEFAULT 0,
    refunds bigint NOT NULL DEFAULT 0,
    refund_amount bigint NOT NULL DEFAULT 0,
    chargebacks bigint NOT NULL DEFAULT 0,
    chargeback_amount bigint NOT NULL DEFAULT 0,
    fraud_chargebacks bigint NOT NULL DEFAULT 0,
    fraud_chargeback_amount bigint NOT NULL DEFAULT 0,
    fraud_reports bigint NOT NULL DEFAULT 0,
    fraud_report_amount bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (merchant_id, psp_id, network, currency, day_at),
    CONSTRAINT network_activity_daily_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps (merchant_id, id, rail) ON DELETE RESTRICT
);
COMMENT ON TABLE billing.network_activity_daily IS 'Per card PSP, network, currency and UTC day: settled sales, refunds, chargebacks, fraud chargebacks (reason fraud or unrecognized) and fraud reports, each counted and summed. Triggers on payments, disputes and fraud_reports keep it for every writer, a restore included. currency is empty for a chargeback that named none.';

CREATE INDEX network_activity_daily_day_at_idx ON billing.network_activity_daily (merchant_id, day_at);

CREATE FUNCTION billing.add_network_activity(p_merchant uuid, p_psp uuid, p_rail text, p_network text, p_currency text, p_at timestamptz, p_measure text, p_count bigint, p_amount bigint) RETURNS void
LANGUAGE sql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
INSERT INTO billing.network_activity_daily AS d (merchant_id, psp_id, rail, network, currency, day_at,
    sales, sales_amount, refunds, refund_amount, chargebacks, chargeback_amount,
    fraud_chargebacks, fraud_chargeback_amount, fraud_reports, fraud_report_amount)
VALUES (p_merchant, p_psp, p_rail, p_network, p_currency, date_trunc('day', p_at, 'UTC'),
    CASE WHEN p_measure = 'sale' THEN p_count ELSE 0 END, CASE WHEN p_measure = 'sale' THEN p_amount ELSE 0 END,
    CASE WHEN p_measure = 'refund' THEN p_count ELSE 0 END, CASE WHEN p_measure = 'refund' THEN p_amount ELSE 0 END,
    CASE WHEN p_measure = 'chargeback' THEN p_count ELSE 0 END, CASE WHEN p_measure = 'chargeback' THEN p_amount ELSE 0 END,
    CASE WHEN p_measure = 'fraud_chargeback' THEN p_count ELSE 0 END, CASE WHEN p_measure = 'fraud_chargeback' THEN p_amount ELSE 0 END,
    CASE WHEN p_measure = 'fraud_report' THEN p_count ELSE 0 END, CASE WHEN p_measure = 'fraud_report' THEN p_amount ELSE 0 END)
ON CONFLICT (merchant_id, psp_id, network, currency, day_at) DO UPDATE SET
    sales = d.sales + EXCLUDED.sales, sales_amount = d.sales_amount + EXCLUDED.sales_amount,
    refunds = d.refunds + EXCLUDED.refunds, refund_amount = d.refund_amount + EXCLUDED.refund_amount,
    chargebacks = d.chargebacks + EXCLUDED.chargebacks, chargeback_amount = d.chargeback_amount + EXCLUDED.chargeback_amount,
    fraud_chargebacks = d.fraud_chargebacks + EXCLUDED.fraud_chargebacks, fraud_chargeback_amount = d.fraud_chargeback_amount + EXCLUDED.fraud_chargeback_amount,
    fraud_reports = d.fraud_reports + EXCLUDED.fraud_reports, fraud_report_amount = d.fraud_report_amount + EXCLUDED.fraud_report_amount
$$;
REVOKE ALL ON FUNCTION billing.add_network_activity(p_merchant uuid, p_psp uuid, p_rail text, p_network text, p_currency text, p_at timestamptz, p_measure text, p_count bigint, p_amount bigint) FROM PUBLIC;

-- A settled card sale and a succeeded refund count on their own day; a pruned
-- row stops counting and an undo counts it again.
CREATE FUNCTION billing.payment_network_activity() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE
    sign integer;
    p record;
    measure text;
BEGIN
    FOREACH sign IN ARRAY CASE TG_OP WHEN 'INSERT' THEN ARRAY[1] WHEN 'DELETE' THEN ARRAY[-1] ELSE ARRAY[-1, 1] END LOOP
        IF sign < 0 THEN p := OLD; ELSE p := NEW; END IF;
        measure := CASE
            WHEN p.channel <> 'rail' OR p.rail NOT IN ('nmi', 'stripe', 'ccbill') OR p.deleted_at IS NOT NULL THEN NULL
            WHEN p.reversal_kind IS NULL AND p.money_movement = 'rail' AND p.amount > 0 AND p.status IN ('succeeded', 'refunded') THEN 'sale'
            WHEN p.reversal_kind = 'refund' AND p.status = 'succeeded' THEN 'refund'
        END;
        IF measure IS NOT NULL THEN
            PERFORM billing.add_network_activity(p.merchant_id, p.psp_id, p.rail, billing.card_network(p.card_brand), p.currency, p.purchased_at, measure, sign, sign * abs(p.amount));
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$;

CREATE FUNCTION billing.dispute_network_activity() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE
    sign integer;
    d record;
BEGIN
    FOREACH sign IN ARRAY CASE TG_OP WHEN 'INSERT' THEN ARRAY[1] WHEN 'DELETE' THEN ARRAY[-1] ELSE ARRAY[-1, 1] END LOOP
        IF sign < 0 THEN d := OLD; ELSE d := NEW; END IF;
        IF d.kind = 'chargeback' THEN
            PERFORM billing.add_network_activity(d.merchant_id, d.psp_id, d.rail, d.network, coalesce(d.currency, ''), d.chargeback_at, 'chargeback', sign, sign * coalesce(d.amount, 0));
            IF d.reason IN ('fraud', 'unrecognized') THEN
                PERFORM billing.add_network_activity(d.merchant_id, d.psp_id, d.rail, d.network, coalesce(d.currency, ''), d.chargeback_at, 'fraud_chargeback', sign, sign * coalesce(d.amount, 0));
            END IF;
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$;

CREATE FUNCTION billing.fraud_report_network_activity() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE
    sign integer;
    f record;
BEGIN
    FOREACH sign IN ARRAY CASE TG_OP WHEN 'INSERT' THEN ARRAY[1] WHEN 'DELETE' THEN ARRAY[-1] ELSE ARRAY[-1, 1] END LOOP
        IF sign < 0 THEN f := OLD; ELSE f := NEW; END IF;
        PERFORM billing.add_network_activity(f.merchant_id, f.psp_id, f.rail, f.network, f.currency, f.reported_at, 'fraud_report', sign, sign * f.amount);
    END LOOP;
    RETURN NULL;
END;
$$;

-- Card sales and refunds so far, before their trigger counts new ones.
INSERT INTO billing.network_activity_daily (merchant_id, psp_id, rail, network, currency, day_at, sales, sales_amount, refunds, refund_amount)
SELECT merchant_id, psp_id, rail, billing.card_network(card_brand), currency, date_trunc('day', purchased_at, 'UTC'),
    count(*) FILTER (WHERE reversal_kind IS NULL), coalesce(sum(amount) FILTER (WHERE reversal_kind IS NULL), 0),
    count(*) FILTER (WHERE reversal_kind = 'refund'), coalesce(sum(-amount) FILTER (WHERE reversal_kind = 'refund'), 0)
FROM billing.payments
WHERE channel = 'rail' AND rail IN ('nmi', 'stripe', 'ccbill') AND deleted_at IS NULL
  AND ((reversal_kind IS NULL AND money_movement = 'rail' AND amount > 0 AND status IN ('succeeded', 'refunded'))
       OR (reversal_kind = 'refund' AND status = 'succeeded'))
GROUP BY 1, 2, 3, 4, 5, 6;

CREATE TRIGGER payments_network_activity AFTER INSERT OR DELETE OR UPDATE OF status, amount, currency, deleted_at, card_brand, purchased_at, psp_id, money_movement ON billing.payments
    FOR EACH ROW EXECUTE FUNCTION billing.payment_network_activity();
CREATE TRIGGER disputes_network_activity AFTER INSERT OR DELETE OR UPDATE OF kind, chargeback_at, network, reason, amount, currency ON billing.disputes
    FOR EACH ROW EXECUTE FUNCTION billing.dispute_network_activity();
CREATE TRIGGER fraud_reports_network_activity AFTER INSERT OR DELETE OR UPDATE OF network, amount, currency, reported_at ON billing.fraud_reports
    FOR EACH ROW EXECUTE FUNCTION billing.fraud_report_network_activity();

-- The chargebacks booked so far: their reason is unknown; a won one has its
-- recovery, and Stripe's others may still be open. History is answered
-- already: no response runs for it.
INSERT INTO billing.disputes (merchant_id, psp_id, rail, provider_ref, customer_id, payment_id, kind, network, reason, amount, currency, status,
    disputed_at, chargeback_at, closed_at, reversal_payment_id, recovery_payment_id, observed_via, responded_at)
SELECT c.merchant_id, c.psp_id, c.rail,
    CASE WHEN c.rail IN ('nmi', 'ccbill') THEN regexp_replace(c.transaction_id, '^chargeback:', '') ELSE c.transaction_id END, c.customer_id, c.refunded_payment_id, 'chargeback', billing.card_network(o.card_brand), 'unknown', -c.amount, c.currency,
    CASE WHEN w.id IS NOT NULL THEN 'won' WHEN c.rail = 'stripe' THEN 'open' ELSE 'unreported' END,
    c.purchased_at, c.purchased_at, w.purchased_at, c.id, w.id, 'notice', now()
FROM billing.payments c
JOIN billing.payments o ON o.merchant_id = c.merchant_id AND o.id = c.refunded_payment_id
LEFT JOIN billing.payments w ON w.merchant_id = c.merchant_id AND w.reversal_kind = 'dispute_reversal' AND w.metadata ->> 'reverses_payment_id' = c.id::text
    AND w.deleted_at IS NULL
WHERE c.reversal_kind = 'chargeback' AND c.psp_id IS NOT NULL AND c.amount < 0 AND c.deleted_at IS NULL;

-- A dispute response's card block is a command of its own kind.
ALTER TABLE billing.destructive_actions DROP CONSTRAINT destructive_actions_kind_check,
    ADD CONSTRAINT destructive_actions_kind_check CHECK (kind IN ('refund', 'cancel_subscription', 'delete_payment_method', 'block_payment_method',
        'revoke_credit', 'revoke_access', 'waive_order', 'mark_uncollectible', 'catalog_change', 'configuration_change'));

-- What a dispute response waits for changing wakes the waiting responses:
-- the destructive switch, a merchant's policy or write posture, the book
-- armed, a hold cleared. The payload is the schema.
CREATE FUNCTION billing.wake_dispute_responses() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    PERFORM pg_notify('openrails_dispute_responses', TG_TABLE_SCHEMA);
    RETURN NULL;
END;
$$;
CREATE TRIGGER destructive_action_switch_wake_dispute_responses AFTER UPDATE OF enabled ON billing.destructive_action_switch
    FOR EACH STATEMENT EXECUTE FUNCTION billing.wake_dispute_responses();
CREATE TRIGGER merchant_destructive_policy_wake_dispute_responses AFTER INSERT OR UPDATE OF destructive_actions_enabled, enforce_armed_at ON billing.merchant_destructive_policy
    FOR EACH STATEMENT EXECUTE FUNCTION billing.wake_dispute_responses();
CREATE TRIGGER merchant_write_posture_wake_dispute_responses AFTER INSERT OR UPDATE OF mode ON billing.merchant_write_posture
    FOR EACH STATEMENT EXECUTE FUNCTION billing.wake_dispute_responses();
CREATE TRIGGER book_identity_wake_dispute_responses AFTER INSERT OR UPDATE ON billing.book_identity
    FOR EACH STATEMENT EXECUTE FUNCTION billing.wake_dispute_responses();
CREATE TRIGGER destructive_holds_wake_dispute_responses AFTER UPDATE OF cleared_at ON billing.destructive_holds
    FOR EACH STATEMENT EXECUTE FUNCTION billing.wake_dispute_responses();

-- The restore guard names the new tables.
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
                  'custodians',
                  'customer_delinquency',
                  'customer_entitlement_cache',
                  'customer_entitlement_cache_stamps',
                  'customer_invoice_profiles',
                  'customer_limit_overrides',
                  'customer_low_balance_lines',
                  'customer_messages',
                  'customers',
                  'dashboard_configs',
                  'destructive_actions',
                  'destructive_holds',
                  'destructive_run_before_images',
                  'disputes',
                  'eventlog_events',
                  'failed_usage_windows',
                  'fraud_reports',
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
