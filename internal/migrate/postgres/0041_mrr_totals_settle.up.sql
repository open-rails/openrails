-- settle_mrr proposed a product's or the merchant's fall in MRR or
-- customers as the row of an INSERT ... ON CONFLICT DO UPDATE, and a CHECK
-- judges the proposed row before the conflict makes it an update: any
-- settle in which a product's MRR or the distinct customers fell was
-- refused. Both totals are now made at zero and then moved, and every
-- merchant's are derived again from what they sum.
--
-- A cancel decision takes every kind a subscription records (a provider's
-- end, an abandoned checkout), and a finding of the MRR risk drain
-- (mrr.risk.evaluation_failed) is a finding type.

-- Nothing settles while the totals are derived again; the locks follow the
-- order writers take.
LOCK TABLE billing.subscriptions IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE billing.subscription_mrr, billing.subscription_status_transitions, billing.customer_mrr, billing.mrr_movements, billing.mrr_product_flows
    IN SHARE MODE;

ALTER TABLE billing.subscription_cancellations DROP CONSTRAINT subscription_cancellations_cancel_type_check,
    ADD CONSTRAINT subscription_cancellations_cancel_type_check
        CHECK (cancel_type IN ('user', 'merchant', 'expired', 'chargeback', 'upgrade', 'provider', 'abandoned'));
ALTER TABLE billing.reconciliation_findings DROP CONSTRAINT reconciliation_findings_type_check,
    ADD CONSTRAINT reconciliation_findings_type_check
        CHECK (finding_type ~ '^(pull|derive|life|consistency|notify|catalog|books|mrr)\.[a-z0-9_]+(\.[a-z0-9_]+)?$');

CREATE OR REPLACE FUNCTION billing.settle_mrr(p_merchant uuid) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE
    v_pair record;
    v_sum record;
    v_customer billing.customer_mrr;
    v_total bigint;
    v_subs integer;
    v_after bigint;
    v_logo integer;
    v_at timestamptz;
    v_cause text;
    v_kind text;
    v_first timestamptz;
    v_movement uuid;
    v_movements uuid[] := '{}';
    v_row record;
    v_case billing.mrr_risk_cases;
    v_outcome text;
    v_closed uuid[] := '{}';
    v_moved boolean;
    v_backfill boolean := COALESCE(current_setting('openrails.mrr_cause', true) = 'backfill', false);
    v_buckets timestamptz[];
    v_logos bigint[];
BEGIN
    FOR v_pair IN
        SELECT DISTINCT m.customer_id, c.currency
          FROM billing.subscription_mrr m
          CROSS JOIN LATERAL (VALUES (m.currency), (m.recorded_currency)) c(currency)
         WHERE m.merchant_id = p_merchant AND m.dirty
         ORDER BY m.customer_id, c.currency
    LOOP
        -- The latest change dates the customer's movement.
        SELECT COALESCE(sum(l.mrr_after - l.mrr_before), 0), COALESCE(sum((l.mrr_after > 0)::integer - (l.mrr_before > 0)::integer), 0), max(l.changed_at)
          INTO v_total, v_subs, v_at
          FROM billing.pending_mrr_lines(p_merchant, v_pair.customer_id, v_pair.currency) l;
        IF v_total = 0 AND v_subs = 0 THEN
            CONTINUE;
        END IF;
        INSERT INTO billing.customer_mrr (merchant_id, customer_id, currency, mrr, subscriptions)
        VALUES (p_merchant, v_pair.customer_id, v_pair.currency, 0, 0)
        ON CONFLICT (merchant_id, customer_id, currency) DO NOTHING;
        SELECT * INTO v_customer FROM billing.customer_mrr
         WHERE merchant_id = p_merchant AND customer_id = v_pair.customer_id AND currency = v_pair.currency FOR UPDATE;
        v_after := v_customer.mrr + v_total;
        v_logo := (v_after > 0)::integer - (v_customer.mrr > 0)::integer;
        v_first := v_customer.first_active_at;
        IF v_total <> 0 THEN
            -- An upgrade explains the whole movement; otherwise its largest
            -- change does.
            SELECT l.cause INTO v_cause FROM billing.pending_mrr_lines(p_merchant, v_pair.customer_id, v_pair.currency) l
             WHERE l.mrr_after <> l.mrr_before ORDER BY (l.cause = 'upgrade') DESC, abs(l.mrr_after - l.mrr_before) DESC, l.subscription_id LIMIT 1;
            v_kind := CASE
                WHEN v_cause IN ('prune', 'undo') THEN 'correction'
                WHEN v_cause IN ('import', 'backfill') THEN CASE WHEN v_total > 0 THEN 'opening' ELSE 'correction' END
                WHEN v_customer.mrr = 0 THEN CASE WHEN v_customer.first_active_at IS NULL THEN 'new' ELSE 'reactivation' END
                WHEN v_after = 0 THEN 'churn'
                WHEN v_total > 0 THEN 'expansion'
                ELSE 'contraction'
            END;
            IF v_first IS NULL THEN
                v_first := v_at;
                -- An adopted subscription's cohort is when it started.
                IF v_kind = 'opening' THEN
                    SELECT least(v_at, min(s.started_at)) INTO v_first
                      FROM billing.pending_mrr_lines(p_merchant, v_pair.customer_id, v_pair.currency) l
                      JOIN billing.subscriptions s ON s.merchant_id = p_merchant AND s.id = l.subscription_id
                     WHERE l.mrr_after > 0;
                END IF;
            END IF;
            INSERT INTO billing.mrr_movements (merchant_id, customer_id, currency, kind, delta, mrr_before, mrr_after, logo_delta,
                cohort_at, cause, voluntary, backfilled, occurred_at)
            VALUES (p_merchant, v_pair.customer_id, v_pair.currency, v_kind, v_total, v_customer.mrr, v_after, v_logo,
                v_first, v_cause, CASE WHEN v_kind = 'churn' THEN v_cause IN ('canceled', 'merchant_canceled') END,
                v_cause = 'backfill', v_at)
            RETURNING id INTO v_movement;
            v_movements := array_append(v_movements, v_movement);
            INSERT INTO billing.mrr_movement_lines (merchant_id, movement_id, subscription_id, product_id, price_id, delta, mrr_before, mrr_after,
                subscriber_delta, cause)
            SELECT p_merchant, v_movement, l.subscription_id, l.product_id, l.price_id, l.mrr_after - l.mrr_before, l.mrr_before, l.mrr_after,
                   (l.mrr_after > 0)::integer - (l.mrr_before > 0)::integer, l.cause
              FROM billing.pending_mrr_lines(p_merchant, v_pair.customer_id, v_pair.currency) l
             WHERE l.mrr_after <> l.mrr_before;
        END IF;
        UPDATE billing.customer_mrr
           SET mrr = v_after, subscriptions = subscriptions + v_subs, first_active_at = v_first
         WHERE merchant_id = p_merchant AND customer_id = v_pair.customer_id AND currency = v_pair.currency;
    END LOOP;
    -- The movements by kind; a voluntary churn and a chargeback's churn also
    -- count apart.
    INSERT INTO billing.mrr_flows AS f (merchant_id, currency, bucket_at, measure, amount, entries, logos)
    SELECT p_merchant, v.currency, billing.mrr_bucket(v.occurred_at), x.measure, sum(v.delta), count(*), sum(v.logo_delta)
      FROM billing.mrr_movements v
      CROSS JOIN LATERAL (SELECT v.kind UNION ALL SELECT 'churn_voluntary' WHERE v.voluntary
                         UNION ALL SELECT 'churn_chargeback' WHERE v.kind = 'churn' AND v.cause = 'chargeback') x(measure)
     WHERE v.merchant_id = p_merchant AND v.id = ANY(v_movements)
     GROUP BY v.currency, billing.mrr_bucket(v.occurred_at), x.measure
     ORDER BY v.currency, billing.mrr_bucket(v.occurred_at), x.measure
    ON CONFLICT (merchant_id, currency, bucket_at, measure) DO UPDATE
    SET amount = f.amount + EXCLUDED.amount, entries = f.entries + EXCLUDED.entries, logos = f.logos + EXCLUDED.logos;
    INSERT INTO billing.mrr_cohort_flows AS c (merchant_id, currency, cohort_at, bucket_at, amount, logos)
    SELECT p_merchant, v.currency, billing.mrr_bucket(v.cohort_at), billing.mrr_bucket(v.occurred_at), sum(v.delta), sum(v.logo_delta)
      FROM billing.mrr_movements v
     WHERE v.merchant_id = p_merchant AND v.id = ANY(v_movements)
     GROUP BY v.currency, billing.mrr_bucket(v.cohort_at), billing.mrr_bucket(v.occurred_at)
     ORDER BY v.currency, billing.mrr_bucket(v.cohort_at), billing.mrr_bucket(v.occurred_at)
    ON CONFLICT (merchant_id, currency, cohort_at, bucket_at) DO UPDATE
    SET amount = c.amount + EXCLUDED.amount, logos = c.logos + EXCLUDED.logos;
    -- The expansion, contraction, churn and corrections of each customer's
    -- run of MRR, by when the run began (its latest movement from none): a
    -- period's opening base is the runs begun before it.
    INSERT INTO billing.mrr_streak_flows AS s (merchant_id, currency, bucket_at, streak_at, measure, amount, logos)
    SELECT p_merchant, v.currency, billing.mrr_bucket(v.occurred_at), billing.mrr_bucket(r.began_at), x.measure, sum(v.delta), sum(v.logo_delta)
      FROM billing.mrr_movements v
      CROSS JOIN LATERAL (SELECT max(p.occurred_at) AS began_at FROM billing.mrr_movements p
                           WHERE p.merchant_id = v.merchant_id AND p.customer_id = v.customer_id AND p.currency = v.currency
                             AND p.logo_delta = 1 AND (p.occurred_at, p.id) <= (v.occurred_at, v.id)) r
      CROSS JOIN LATERAL (SELECT v.kind UNION ALL SELECT 'churn_voluntary' WHERE v.voluntary
                         UNION ALL SELECT 'churn_chargeback' WHERE v.kind = 'churn' AND v.cause = 'chargeback') x(measure)
     WHERE v.merchant_id = p_merchant AND v.id = ANY(v_movements) AND v.kind IN ('expansion', 'contraction', 'churn', 'correction')
       AND r.began_at IS NOT NULL
     GROUP BY v.currency, billing.mrr_bucket(v.occurred_at), billing.mrr_bucket(r.began_at), x.measure
     ORDER BY v.currency, billing.mrr_bucket(v.occurred_at), billing.mrr_bucket(r.began_at), x.measure
    ON CONFLICT (merchant_id, currency, bucket_at, streak_at, measure) DO UPDATE
    SET amount = s.amount + EXCLUDED.amount, logos = s.logos + EXCLUDED.logos;
    -- A customer gaining MRR in a first currency, or losing it in its last,
    -- moves the merchant's distinct customers: its currencies with MRR before
    -- this settlement's movements are those now less their logo changes, and
    -- each movement counts as the running sum crosses zero. Every flow row is
    -- written in bucket order before the one totals row.
    SELECT array_agg(c.bucket_at ORDER BY c.bucket_at), array_agg(c.logos ORDER BY c.bucket_at)
      INTO v_buckets, v_logos
      FROM (SELECT billing.mrr_bucket(x.occurred_at) AS bucket_at, sum((x.n > 0)::integer - (x.n - x.logo_delta > 0)::integer)::bigint AS logos
              FROM (SELECT v.occurred_at, v.logo_delta,
                           a.active - sum(v.logo_delta) OVER (PARTITION BY v.customer_id)
                                    + sum(v.logo_delta) OVER (PARTITION BY v.customer_id ORDER BY v.occurred_at, v.id) AS n
                      FROM billing.mrr_movements v
                      CROSS JOIN LATERAL (SELECT count(*) FILTER (WHERE cm.mrr > 0) AS active FROM billing.customer_mrr cm
                                           WHERE cm.merchant_id = p_merchant AND cm.customer_id = v.customer_id) a
                     WHERE v.merchant_id = p_merchant AND v.id = ANY(v_movements)) x
             GROUP BY billing.mrr_bucket(x.occurred_at)
            HAVING sum((x.n > 0)::integer - (x.n - x.logo_delta > 0)::integer) <> 0) c;
    IF v_buckets IS NOT NULL THEN
        INSERT INTO billing.mrr_customer_flows AS f (merchant_id, bucket_at, logos)
        SELECT p_merchant, b.bucket_at, b.logos FROM unnest(v_buckets, v_logos) AS b(bucket_at, logos) ORDER BY b.bucket_at
        ON CONFLICT (merchant_id, bucket_at) DO UPDATE SET logos = f.logos + EXCLUDED.logos;
        -- A CHECK judges the row an INSERT proposes before ON CONFLICT
        -- turns it into an update, so a fall is never proposed: the row is
        -- made at zero, then moved.
        INSERT INTO billing.mrr_customer_totals (merchant_id, customers) VALUES (p_merchant, 0)
        ON CONFLICT (merchant_id) DO NOTHING;
        UPDATE billing.mrr_customer_totals SET customers = customers + (SELECT sum(l) FROM unnest(v_logos) AS l)
         WHERE merchant_id = p_merchant;
    END IF;
    -- Each changed subscription moves its products' MRR and subscribers, in
    -- the bucket of its customer's movement in each currency: the latest
    -- change of the customer's rows in that currency.
    INSERT INTO billing.mrr_product_flows AS p (merchant_id, currency, bucket_at, product_id, amount, subscribers)
    SELECT p_merchant, x.currency, billing.mrr_bucket(x.at), x.product_id, sum(x.amount), sum(x.subscribers)
      FROM (SELECT x.currency, x.product_id, x.amount, x.subscribers, max(m.changed_at) OVER (PARTITION BY m.customer_id, x.currency) AS at
              FROM billing.subscription_mrr m
              CROSS JOIN LATERAL (VALUES (m.recorded_currency, m.recorded_product_id, -m.recorded_mrr, -(m.recorded_mrr > 0)::integer),
                                         (m.currency, m.product_id, m.mrr, (m.mrr > 0)::integer)) x(currency, product_id, amount, subscribers)
             WHERE m.merchant_id = p_merchant AND m.dirty) x
     GROUP BY x.currency, billing.mrr_bucket(x.at), x.product_id
    HAVING sum(x.amount) <> 0 OR sum(x.subscribers) <> 0
     ORDER BY x.currency, billing.mrr_bucket(x.at), x.product_id
    ON CONFLICT (merchant_id, currency, bucket_at, product_id) DO UPDATE
    SET amount = p.amount + EXCLUDED.amount, subscribers = p.subscribers + EXCLUDED.subscribers;
    -- Each product's totals, one at a time in key order, made at zero and
    -- then moved: a CHECK would refuse a fall proposed as an INSERT.
    FOR v_sum IN
        SELECT x.currency, x.product_id, sum(x.amount)::bigint AS mrr, sum(x.subscribers)::bigint AS subscribers
          FROM billing.subscription_mrr m
          CROSS JOIN LATERAL (VALUES (m.recorded_currency, m.recorded_product_id, -m.recorded_mrr, -(m.recorded_mrr > 0)::integer),
                                     (m.currency, m.product_id, m.mrr, (m.mrr > 0)::integer)) x(currency, product_id, amount, subscribers)
         WHERE m.merchant_id = p_merchant AND m.dirty
         GROUP BY x.currency, x.product_id
        HAVING sum(x.amount) <> 0 OR sum(x.subscribers) <> 0
         ORDER BY x.currency, x.product_id
    LOOP
        INSERT INTO billing.mrr_product_totals (merchant_id, currency, product_id, mrr, subscribers)
        VALUES (p_merchant, v_sum.currency, v_sum.product_id, 0, 0)
        ON CONFLICT (merchant_id, currency, product_id) DO NOTHING;
        UPDATE billing.mrr_product_totals SET mrr = mrr + v_sum.mrr, subscribers = subscribers + v_sum.subscribers
         WHERE merchant_id = p_merchant AND currency = v_sum.currency AND product_id = v_sum.product_id;
    END LOOP;
    -- The merchant's totals, one currency at a time.
    FOR v_sum IN
        SELECT x.currency, sum(x.amount)::bigint AS mrr, sum(x.subscribers)::bigint AS subscribers,
               COALESCE((SELECT sum(v.logo_delta) FROM billing.mrr_movements v
                          WHERE v.merchant_id = p_merchant AND v.id = ANY(v_movements) AND v.currency = x.currency), 0)::bigint AS customers
          FROM billing.subscription_mrr m
          CROSS JOIN LATERAL (VALUES (m.recorded_currency, -m.recorded_mrr, -(m.recorded_mrr > 0)::integer),
                                     (m.currency, m.mrr, (m.mrr > 0)::integer)) x(currency, amount, subscribers)
         WHERE m.merchant_id = p_merchant AND m.dirty
         GROUP BY x.currency
         ORDER BY x.currency
    LOOP
        CONTINUE WHEN v_sum.mrr = 0 AND v_sum.subscribers = 0 AND v_sum.customers = 0;
        INSERT INTO billing.mrr_totals (merchant_id, currency, mrr, customers, subscribers)
        VALUES (p_merchant, v_sum.currency, 0, 0, 0)
        ON CONFLICT (merchant_id, currency) DO NOTHING;
        UPDATE billing.mrr_totals
           SET mrr = mrr + v_sum.mrr, customers = customers + v_sum.customers, subscribers = subscribers + v_sum.subscribers
         WHERE merchant_id = p_merchant AND currency = v_sum.currency;
    END LOOP;
    -- At-risk MRR: a subscription whose reasons or currency changed opens,
    -- chains or closes its case and episodes. A change of currency supersedes
    -- the open case; one opens in the new currency.
    FOR v_row IN
        SELECT m.*, s.status, s.cancel_type, s.deleted_at
          FROM billing.subscription_mrr m
          JOIN billing.subscriptions s ON s.merchant_id = m.merchant_id AND s.id = m.subscription_id
         WHERE m.merchant_id = p_merchant AND m.dirty
           AND (m.risk <> m.recorded_risk OR m.recorded_risk <> '{}' AND m.currency <> m.recorded_currency)
         ORDER BY m.subscription_id
    LOOP
        v_moved := v_row.recorded_risk <> '{}' AND v_row.currency <> v_row.recorded_currency;
        -- Dated at the change of its reasons, else of its currency.
        v_at := CASE WHEN v_row.risk <> v_row.recorded_risk THEN v_row.risk_changed_at ELSE v_row.changed_at END;
        v_case := NULL;
        UPDATE billing.mrr_risk_episodes SET left_at = greatest(v_at, entered_at), mrr_at_exit = v_row.recorded_mrr
         WHERE merchant_id = p_merchant AND subscription_id = v_row.subscription_id AND left_at IS NULL
           AND (v_moved OR reason <> ALL(v_row.risk));
        IF v_row.recorded_risk <> '{}' THEN
            SELECT * INTO v_case FROM billing.mrr_risk_cases
             WHERE merchant_id = p_merchant AND subscription_id = v_row.subscription_id AND closed_at IS NULL;
            IF v_moved OR v_row.risk = '{}' THEN
                -- Healthy again while adding MRR is recovered; MRR that ended
                -- churned; a prune, an upgrade or a new currency supersedes.
                v_outcome := CASE
                    WHEN v_moved OR v_row.deleted_at IS NOT NULL OR v_row.cancel_type = 'upgrade' THEN 'superseded'
                    WHEN v_row.mrr > 0 THEN 'recovered'
                    ELSE 'churned' END;
                UPDATE billing.mrr_risk_cases
                   SET closed_at = greatest(v_at, opened_at), outcome = v_outcome,
                       mrr_at_exit = CASE WHEN v_outcome = 'recovered' THEN v_row.mrr ELSE v_row.recorded_mrr END,
                       voluntary = CASE WHEN v_outcome = 'churned' THEN COALESCE(v_row.cancel_type IN ('user', 'merchant'), false) END
                 WHERE merchant_id = p_merchant AND id = v_case.id;
                v_closed := array_append(v_closed, v_case.id);
                v_case := NULL;
            END IF;
        END IF;
        IF v_row.risk <> '{}' THEN
            -- A backfilled reason enters when the book says it began.
            IF v_case.id IS NULL THEN
                INSERT INTO billing.mrr_risk_cases (merchant_id, subscription_id, customer_id, currency, entry_reason, opened_at, mrr_at_entry, backfilled)
                SELECT p_merchant, v_row.subscription_id, v_row.customer_id, v_row.currency, e.reason, e.at, v_row.mrr, v_backfill
                  FROM (SELECT r.reason, r.n, CASE WHEN v_backfill THEN billing.mrr_risk_began(p_merchant, v_row.customer_id, v_row.subscription_id,
                               v_row.currency, r.reason, v_at) ELSE v_at END AS at
                          FROM unnest(v_row.risk) WITH ORDINALITY AS r(reason, n)) e
                 ORDER BY e.at, e.n LIMIT 1
                RETURNING * INTO v_case;
            END IF;
            INSERT INTO billing.mrr_risk_episodes (merchant_id, case_id, subscription_id, currency, reason, entered_at, mrr_at_entry, overdue_amount)
            SELECT p_merchant, v_case.id, v_row.subscription_id, v_row.currency, r.reason,
                   CASE WHEN v_backfill THEN billing.mrr_risk_began(p_merchant, v_row.customer_id, v_row.subscription_id,
                        v_row.currency, r.reason, v_at) ELSE v_at END, v_row.mrr,
                   CASE WHEN r.reason = 'delinquent' THEN (SELECT d.overdue_amount FROM billing.customer_delinquency d
                       WHERE d.merchant_id = p_merchant AND d.customer_id = v_row.customer_id AND d.currency = v_row.currency) END
              FROM unnest(v_row.risk) AS r(reason)
             WHERE v_moved OR r.reason <> ALL(v_row.recorded_risk);
        END IF;
    END LOOP;
    -- The at-risk amounts and counts per reason, any counting a subscription
    -- once, and the closed cases by their entry reason; a change of MRR while
    -- at risk moves the amounts too.
    INSERT INTO billing.mrr_risk_flows AS f (merchant_id, currency, bucket_at, reason, amount, entered, left_count, recovered_mrr, recovered, churned_mrr, churned)
    SELECT p_merchant, x.currency, billing.mrr_bucket(x.at), x.reason, sum(x.amount), sum(x.entered), sum(x.left_count),
           sum(x.recovered_mrr), sum(x.recovered), sum(x.churned_mrr), sum(x.churned)
      FROM (SELECT o.currency, o.reason, o.amount, o.entered, o.left_count, 0 AS recovered_mrr, 0 AS recovered, 0 AS churned_mrr, 0 AS churned,
                   CASE WHEN m.risk <> m.recorded_risk THEN m.risk_changed_at ELSE m.changed_at END AS at
              FROM billing.subscription_mrr m
              CROSS JOIN LATERAL (
                  SELECT m.recorded_currency, r.reason, -m.recorded_mrr, 0,
                         (m.currency <> m.recorded_currency OR r.reason <> ALL(m.risk || CASE WHEN m.risk <> '{}' THEN '{any}'::text[] ELSE '{}' END))::integer
                    FROM unnest(m.recorded_risk || CASE WHEN m.recorded_risk <> '{}' THEN '{any}'::text[] ELSE '{}' END) AS r(reason)
                  UNION ALL
                  SELECT m.currency, r.reason, m.mrr,
                         (m.currency <> m.recorded_currency OR r.reason <> ALL(m.recorded_risk || CASE WHEN m.recorded_risk <> '{}' THEN '{any}'::text[] ELSE '{}' END))::integer, 0
                    FROM unnest(m.risk || CASE WHEN m.risk <> '{}' THEN '{any}'::text[] ELSE '{}' END) AS r(reason)
              ) o(currency, reason, amount, entered, left_count)
             WHERE m.merchant_id = p_merchant AND m.dirty AND (m.risk <> '{}' OR m.recorded_risk <> '{}')
            UNION ALL
            SELECT c.currency, c.entry_reason, 0, 0, 0,
                   CASE WHEN c.outcome = 'recovered' THEN c.mrr_at_exit ELSE 0 END, (c.outcome = 'recovered')::integer,
                   CASE WHEN c.outcome = 'churned' THEN c.mrr_at_exit ELSE 0 END, (c.outcome = 'churned')::integer, c.closed_at
              FROM billing.mrr_risk_cases c
             WHERE c.merchant_id = p_merchant AND c.id = ANY(v_closed) AND c.outcome <> 'superseded') x
     GROUP BY x.currency, billing.mrr_bucket(x.at), x.reason
    HAVING sum(x.amount) <> 0 OR sum(x.entered) <> 0 OR sum(x.left_count) <> 0 OR sum(x.recovered) <> 0 OR sum(x.churned) <> 0
     ORDER BY x.currency, billing.mrr_bucket(x.at), x.reason
    ON CONFLICT (merchant_id, currency, bucket_at, reason) DO UPDATE
    SET amount = f.amount + EXCLUDED.amount, entered = f.entered + EXCLUDED.entered, left_count = f.left_count + EXCLUDED.left_count,
        recovered_mrr = f.recovered_mrr + EXCLUDED.recovered_mrr, recovered = f.recovered + EXCLUDED.recovered,
        churned_mrr = f.churned_mrr + EXCLUDED.churned_mrr, churned = f.churned + EXCLUDED.churned;
    -- The at-risk totals, in key order.
    FOR v_sum IN
        SELECT o.currency, o.reason, sum(o.amount)::bigint AS mrr, sum(o.subscriptions)::bigint AS subscriptions
          FROM billing.subscription_mrr m
          CROSS JOIN LATERAL (
              SELECT m.recorded_currency, r.reason, -m.recorded_mrr, -1 FROM unnest(m.recorded_risk || CASE WHEN m.recorded_risk <> '{}' THEN '{any}'::text[] ELSE '{}' END) AS r(reason)
              UNION ALL
              SELECT m.currency, r.reason, m.mrr, 1 FROM unnest(m.risk || CASE WHEN m.risk <> '{}' THEN '{any}'::text[] ELSE '{}' END) AS r(reason)
          ) o(currency, reason, amount, subscriptions)
         WHERE m.merchant_id = p_merchant AND m.dirty AND (m.risk <> '{}' OR m.recorded_risk <> '{}')
         GROUP BY o.currency, o.reason
        HAVING sum(o.amount) <> 0 OR sum(o.subscriptions) <> 0
         ORDER BY o.currency, o.reason
    LOOP
        INSERT INTO billing.mrr_risk_totals (merchant_id, currency, reason, mrr, subscriptions)
        VALUES (p_merchant, v_sum.currency, v_sum.reason, 0, 0)
        ON CONFLICT (merchant_id, currency, reason) DO NOTHING;
        UPDATE billing.mrr_risk_totals
           SET mrr = mrr + v_sum.mrr, subscriptions = subscriptions + v_sum.subscriptions
         WHERE merchant_id = p_merchant AND currency = v_sum.currency AND reason = v_sum.reason;
    END LOOP;
    UPDATE billing.subscription_mrr
       SET recorded_currency = currency, recorded_product_id = product_id, recorded_price_id = price_id, recorded_mrr = mrr, recorded_risk = risk
     WHERE merchant_id = p_merchant AND dirty;
END;
$$;
COMMENT ON FUNCTION billing.settle_mrr(p_merchant uuid) IS 'Records the merchant''s dirty subscription_mrr rows: per customer and currency one mrr_movements row classified by the customer''s MRR before and after, its lines, and the rollups (with each run''s retention flows, product totals and distinct customers); then each subscription''s risk case and episodes, the at-risk flows and totals.';

DELETE FROM billing.mrr_customer_flows;
DELETE FROM billing.mrr_customer_totals;
DELETE FROM billing.mrr_product_totals;
SELECT billing.rebuild_mrr_rollups(id) FROM billing.merchants ORDER BY id;
