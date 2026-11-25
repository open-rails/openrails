-- The MRR writers read by key: a subscription's row, a customer's rows, the
-- dirty rows. A session caches their plans, and one made while
-- subscription_mrr was small scans it whole, or reads every entry of the
-- merchant from an index, so each row of a bulk import or backfill read every
-- row written before it. These functions plan each read for the table as it
-- is, without sequential scans, and a subscription's row is looked up by its
-- customer too, so whichever index a plan takes is keyed: a write's work
-- grows with what it changed. A later CREATE OR REPLACE of one of them
-- restates its settings.
CREATE OR REPLACE FUNCTION billing.refresh_subscription_mrr(s billing.subscriptions, p_at timestamptz, p_cause text) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' SET enable_seqscan TO off SET plan_cache_mode TO force_custom_plan AS $$
DECLARE
    v_price billing.prices;
    v_prev billing.subscription_mrr;
    v_open billing.mrr_scheduled;
    v_change billing.scheduled_changes;
    v_has_open boolean;
    v_level bigint := 0;
    v_live boolean := false;
    v_ends timestamptz;
    v_at timestamptz := p_at;
    v_risk text[];
BEGIN
    SELECT * INTO v_price FROM billing.prices WHERE merchant_id = s.merchant_id AND id = s.price_id;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    SELECT * INTO v_prev FROM billing.subscription_mrr WHERE merchant_id = s.merchant_id AND customer_id = s.customer_id AND subscription_id = s.id;
    IF s.deleted_at IS NULL THEN
        IF s.status = 'canceled' THEN
            -- A cancellation at period end contributes until it ends (an older
            -- one recorded no end); a subscription replaced by its upgrade
            -- leaves at once.
            v_ends := COALESCE(s.ended_at, s.current_period_ends_at);
            v_live := s.cancel_type IS DISTINCT FROM 'upgrade' AND COALESCE(v_ends > p_at, false);
        ELSE
            v_live := s.status IN ('active', 'past_due', 'awaiting_method', 'unverified');
        END IF;
        -- A trial contributes from its conversion: the first renewal of a
        -- price with a trial phase.
        IF v_live AND v_price.trial_duration_hours IS NOT NULL
           AND (s.current_period_starts_at IS NULL OR s.current_period_starts_at <= s.started_at) THEN
            v_live := false;
        END IF;
    END IF;
    IF v_live THEN
        v_level := billing.price_mrr(v_price, s.quantity);
    ELSE
        v_ends := NULL;
        -- A scheduled end that passed took effect at that end.
        IF v_prev.ends_at IS NOT NULL AND v_prev.ends_at <= p_at THEN
            v_at := v_prev.ends_at;
        END IF;
    END IF;
    v_risk := billing.subscription_risk(s, v_level, COALESCE(v_prev.risk, '{}'));
    IF v_prev.subscription_id IS NULL THEN
        INSERT INTO billing.subscription_mrr (merchant_id, subscription_id, customer_id, currency, product_id, price_id, mrr, contributes,
            ends_at, next_change_at, changed_at, cause, recorded_currency, recorded_product_id, recorded_price_id, recorded_mrr,
            risk, recorded_risk, risk_changed_at)
        VALUES (s.merchant_id, s.id, s.customer_id, v_price.currency, s.product_id, s.price_id, v_level, v_live,
            v_ends, v_ends, v_at, p_cause, v_price.currency, s.product_id, s.price_id, 0,
            v_risk, '{}', CASE WHEN v_risk <> '{}' THEN v_at END);
    ELSIF ROW(v_prev.currency, v_prev.product_id, v_prev.price_id, v_prev.mrr, v_prev.contributes, v_prev.ends_at, v_prev.risk)
          IS DISTINCT FROM ROW(v_price.currency, s.product_id, s.price_id, v_level, v_live, v_ends, v_risk) THEN
        UPDATE billing.subscription_mrr
           SET currency = v_price.currency, product_id = s.product_id, price_id = s.price_id, mrr = v_level, contributes = v_live,
               ends_at = v_ends, next_change_at = v_ends, risk = v_risk,
               risk_changed_at = CASE WHEN v_prev.risk <> v_risk THEN greatest(v_at, v_prev.risk_changed_at) ELSE v_prev.risk_changed_at END,
               -- A change of what MRR counts is dated and explained by this one.
               changed_at = CASE WHEN ROW(v_prev.currency, v_prev.product_id, v_prev.price_id, v_prev.mrr) IS DISTINCT FROM ROW(v_price.currency, s.product_id, s.price_id, v_level)
                   THEN v_at ELSE v_prev.changed_at END,
               cause = CASE WHEN ROW(v_prev.currency, v_prev.product_id, v_prev.price_id, v_prev.mrr) IS DISTINCT FROM ROW(v_price.currency, s.product_id, s.price_id, v_level)
                   THEN p_cause ELSE v_prev.cause END
         WHERE merchant_id = s.merchant_id AND customer_id = s.customer_id AND subscription_id = s.id;
    END IF;

    SELECT * INTO v_open FROM billing.mrr_scheduled
     WHERE merchant_id = s.merchant_id AND subscription_id = s.id AND status = 'scheduled' FOR UPDATE;
    v_has_open := FOUND;
    IF v_ends IS NOT NULL AND v_level > 0 THEN
        -- Canceled with its end ahead: scheduled churn of what it adds now.
        -- An entry is never edited: any other one is superseded by a new one.
        IF NOT (v_has_open AND v_open.kind = 'churn'
                AND (v_open.currency, v_open.mrr_delta, v_open.effective_at) = (v_price.currency, -v_level, v_ends)) THEN
            IF v_has_open THEN
                PERFORM billing.resolve_mrr_scheduled(v_open, 'superseded', p_at);
            END IF;
            INSERT INTO billing.mrr_scheduled (merchant_id, subscription_id, customer_id, currency, kind, mrr_delta, effective_at, cause, status, scheduled_at)
            VALUES (s.merchant_id, s.id, s.customer_id, v_price.currency, 'churn', -v_level, v_ends, billing.mrr_cancel_cause(s.cancel_type), 'scheduled', p_at);
        END IF;
    ELSIF v_has_open AND v_open.kind = 'churn' THEN
        -- It resumed, or it ended: at its end or earlier as churn, or by a
        -- correction or an upgrade, which no churn records.
        PERFORM billing.resolve_mrr_scheduled(v_open, CASE
            WHEN v_level > 0 THEN 'reversed'
            WHEN p_cause IN ('canceled', 'merchant_canceled', 'expired', 'chargeback') THEN 'effected'
            ELSE 'superseded' END, v_at);
        IF v_level > 0 THEN
            PERFORM billing.schedule_mrr_change(s.merchant_id, s.id, p_at);
        END IF;
    ELSIF v_has_open AND v_level IS DISTINCT FROM v_prev.mrr THEN
        -- A scheduled contraction or expansion is met when the subscription
        -- bills its change, and overtaken by any other change.
        SELECT * INTO v_change FROM billing.scheduled_changes WHERE merchant_id = s.merchant_id AND id = v_open.scheduled_change_id;
        IF v_level > 0 AND s.price_id = v_change.price_id AND (v_change.quantity IS NULL OR v_change.quantity = s.quantity) THEN
            PERFORM billing.resolve_mrr_scheduled(v_open, 'effected', p_at);
        ELSE
            PERFORM billing.resolve_mrr_scheduled(v_open, 'superseded', p_at);
            PERFORM billing.schedule_mrr_change(s.merchant_id, s.id, p_at);
        END IF;
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION billing.settle_at_commit() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' SET enable_seqscan TO off SET plan_cache_mode TO force_custom_plan AS $$
BEGIN
    IF TG_TABLE_NAME = 'ledger_rollup_pending' THEN
        PERFORM 1 FROM billing.ledger_rollup_pending WHERE merchant_id = NEW.merchant_id AND transfer_id = NEW.transfer_id;
    ELSIF TG_TABLE_NAME = 'subscription_mrr' THEN
        PERFORM 1 FROM billing.subscription_mrr WHERE merchant_id = NEW.merchant_id AND customer_id = NEW.customer_id AND subscription_id = NEW.subscription_id AND dirty;
    ELSE
        PERFORM 1 FROM billing.eventlog_pending WHERE ordinal = NEW.ordinal;
    END IF;
    IF FOUND THEN
        PERFORM billing.settle_commit(TG_TABLE_SCHEMA);
    END IF;
    RETURN NULL;
END;
$$;

ALTER FUNCTION billing.schedule_mrr_change(uuid, uuid, timestamptz) SET enable_seqscan TO off;
ALTER FUNCTION billing.schedule_mrr_change(uuid, uuid, timestamptz) SET plan_cache_mode TO force_custom_plan;
ALTER FUNCTION billing.pending_mrr_lines(uuid, uuid, text) SET enable_seqscan TO off;
ALTER FUNCTION billing.pending_mrr_lines(uuid, uuid, text) SET plan_cache_mode TO force_custom_plan;
ALTER FUNCTION billing.settle_mrr(uuid) SET enable_seqscan TO off;
ALTER FUNCTION billing.settle_mrr(uuid) SET plan_cache_mode TO force_custom_plan;
