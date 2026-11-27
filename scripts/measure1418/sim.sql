-- Simulation helpers: each event is one statement, so one transaction, and
-- the schema's own triggers keep MRR as every production writer does.
CREATE SCHEMA IF NOT EXISTS sim;

CREATE FUNCTION sim.subscribe(p_m uuid, p_c uuid, p_s uuid, p_price uuid, p_prod uuid, p_psp uuid, p_pm uuid,
    p_ps timestamptz, p_pe timestamptz, p_at timestamptz) RETURNS void
LANGUAGE sql AS $$
INSERT INTO billing.subscriptions (id, merchant_id, customer_id, price_id, product_id, status, rail, collection_policy, payment_method_id,
    current_period_starts_at, current_period_ends_at, started_at, psp_id, created_at, updated_at)
VALUES (p_s, p_m, p_c, p_price, p_prod, 'active', 'stripe', 'engine', p_pm, p_ps, p_pe, p_at, p_psp, p_at, p_at);
$$;

-- A new customer with a card and its plan, and an add-on when p_s2 is set.
CREATE FUNCTION sim.arrive(p_m uuid, p_c uuid, p_psp uuid, p_pm uuid, p_exp_year int,
    p_s1 uuid, p_price1 uuid, p_prod1 uuid, p_s2 uuid, p_price2 uuid, p_prod2 uuid,
    p_ps timestamptz, p_pe timestamptz, p_at timestamptz) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO billing.customers (id, merchant_id, created_at, last_seen_at) VALUES (p_c, p_m, p_at, p_at);
    INSERT INTO billing.payment_methods (id, merchant_id, customer_id, rail, psp_id, custodian, card_brand, card_last4,
        card_exp_month, card_exp_year, charge_via, status, created_at, updated_at)
    VALUES (p_pm, p_m, p_c, 'stripe', p_psp, 'psp', 'visa', '4242', 12, p_exp_year, 'pan_proxy', 'active', p_at, p_at);
    PERFORM sim.subscribe(p_m, p_c, p_s1, p_price1, p_prod1, p_psp, p_pm, p_ps, p_pe, p_at);
    IF p_s2 IS NOT NULL THEN
        PERFORM sim.subscribe(p_m, p_c, p_s2, p_price2, p_prod2, p_psp, p_pm, p_ps, p_pe, p_at);
    END IF;
END;
$$;

-- A returning customer's plan (and add-on).
CREATE FUNCTION sim.rejoin(p_m uuid, p_c uuid, p_psp uuid, p_pm uuid,
    p_s1 uuid, p_price1 uuid, p_prod1 uuid, p_s2 uuid, p_price2 uuid, p_prod2 uuid,
    p_ps timestamptz, p_pe timestamptz, p_at timestamptz) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM sim.subscribe(p_m, p_c, p_s1, p_price1, p_prod1, p_psp, p_pm, p_ps, p_pe, p_at);
    IF p_s2 IS NOT NULL THEN
        PERFORM sim.subscribe(p_m, p_c, p_s2, p_price2, p_prod2, p_psp, p_pm, p_ps, p_pe, p_at);
    END IF;
END;
$$;

-- A lifecycle write, as the single writer makes it: status, price, period,
-- cancellation, the revision advanced by one, dated p_at.
CREATE FUNCTION sim.set1(p_m uuid, p_s uuid, p_at timestamptz, p_status text, p_price uuid,
    p_ps timestamptz, p_pe timestamptz, p_cancel text, p_ended timestamptz) RETURNS void
LANGUAGE sql AS $$
UPDATE billing.subscriptions
   SET status = p_status, price_id = p_price, current_period_starts_at = p_ps, current_period_ends_at = p_pe,
       cancel_type = p_cancel, canceled_at = CASE WHEN p_status = 'canceled' THEN p_at END, ended_at = p_ended,
       lifecycle_rev = lifecycle_rev + 1, updated_at = p_at
 WHERE merchant_id = p_m AND id = p_s;
$$;

-- The plan and, when p_s2 is set, its add-on, in one transaction.
CREATE FUNCTION sim.set2(p_m uuid, p_s1 uuid, p_price1 uuid, p_s2 uuid, p_price2 uuid, p_at timestamptz, p_status text,
    p_ps timestamptz, p_pe timestamptz, p_cancel text, p_ended timestamptz) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM sim.set1(p_m, p_s1, p_at, p_status, p_price1, p_ps, p_pe, p_cancel, p_ended);
    IF p_s2 IS NOT NULL THEN
        PERFORM sim.set1(p_m, p_s2, p_at, p_status, p_price2, p_ps, p_pe, p_cancel, p_ended);
    END IF;
END;
$$;

-- The card's expiry, then (when p_s is set) the renewal that reads it.
CREATE FUNCTION sim.card_renew(p_m uuid, p_pm uuid, p_month int, p_year int, p_s uuid, p_price uuid,
    p_ps timestamptz, p_pe timestamptz, p_at timestamptz) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    UPDATE billing.payment_methods SET card_exp_month = p_month, card_exp_year = p_year, updated_at = p_at
     WHERE merchant_id = p_m AND id = p_pm;
    IF p_s IS NOT NULL THEN
        PERFORM sim.set1(p_m, p_s, p_at, 'active', p_price, p_ps, p_pe, NULL, NULL);
    END IF;
END;
$$;

CREATE FUNCTION sim.delinquency(p_m uuid, p_c uuid, p_cur text, p_on boolean, p_amount bigint, p_at timestamptz) RETURNS void
LANGUAGE sql AS $$
INSERT INTO billing.customer_delinquency AS d (merchant_id, customer_id, currency, state, overdue_started_at, entered_at,
    overdue_amount, overdue_invoices, transition_seq, evaluated_at, created_at, updated_at)
VALUES (p_m, p_c, p_cur, CASE WHEN p_on THEN 'delinquent' ELSE 'current' END, CASE WHEN p_on THEN p_at END, p_at,
    CASE WHEN p_on THEN p_amount ELSE 0 END, CASE WHEN p_on THEN 1 ELSE 0 END, 1, p_at, p_at, p_at)
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE
   SET state = EXCLUDED.state, overdue_started_at = EXCLUDED.overdue_started_at, entered_at = EXCLUDED.entered_at,
       overdue_amount = EXCLUDED.overdue_amount, overdue_invoices = EXCLUDED.overdue_invoices,
       transition_seq = d.transition_seq + 1, evaluated_at = EXCLUDED.evaluated_at, updated_at = EXCLUDED.updated_at;
$$;

CREATE FUNCTION sim.schedule_change(p_m uuid, p_id uuid, p_s uuid, p_from uuid, p_to uuid, p_eff timestamptz, p_at timestamptz) RETURNS void
LANGUAGE sql AS $$
INSERT INTO billing.scheduled_changes (id, merchant_id, subscription_id, from_price_id, price_id, effective_at, source, status,
    acknowledged_short_notice, created_at)
VALUES (p_id, p_m, p_s, p_from, p_to, p_eff, 'change', 'scheduled', false, p_at);
$$;

-- The renewal that bills a scheduled change, and the change applied.
CREATE FUNCTION sim.apply_change(p_m uuid, p_id uuid, p_s uuid, p_price uuid, p_ps timestamptz, p_pe timestamptz, p_at timestamptz) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM sim.set1(p_m, p_s, p_at, 'active', p_price, p_ps, p_pe, NULL, NULL);
    UPDATE billing.scheduled_changes SET status = 'applied', applied_at = p_at WHERE merchant_id = p_m AND id = p_id;
END;
$$;

-- openrails.mrr_due for a customer's due rows, in one transaction.
CREATE FUNCTION sim.effect_due(p_m uuid, p_s1 uuid, p_s2 uuid, p_at timestamptz) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM billing.effect_due_mrr(p_m, p_s1, p_at);
    IF p_s2 IS NOT NULL THEN
        PERFORM billing.effect_due_mrr(p_m, p_s2, p_at);
    END IF;
END;
$$;
