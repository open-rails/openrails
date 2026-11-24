-- Reporting over the MRR rollups: churn caused by a chargeback counts apart
-- in mrr_flows; each customer's run of MRR keeps its retention flows; each
-- product keeps its totals and the merchant its distinct customers with MRR;
-- every cancel decision is kept. Reports read these, never the movements or
-- the subscriptions.

-- Nothing commits a movement, a product flow or a status change while the
-- rollups are filled from them, and no settle in flight finishes under the
-- old settle_mrr; the locks follow the order writers take.
LOCK TABLE billing.subscriptions IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE billing.subscription_mrr, billing.subscription_status_transitions, billing.customer_mrr, billing.mrr_movements, billing.mrr_product_flows
    IN SHARE MODE;

ALTER TABLE billing.mrr_flows DROP CONSTRAINT mrr_flows_measure_check;
ALTER TABLE billing.mrr_flows ADD CONSTRAINT mrr_flows_measure_check
    CHECK (measure IN ('new', 'expansion', 'contraction', 'churn', 'reactivation', 'opening', 'correction', 'churn_voluntary', 'churn_chargeback'));
COMMENT ON TABLE billing.mrr_flows IS 'MRR flows per currency and 15-minute UTC bucket (billing.mrr_bucket): movements by kind (and churn_voluntary and churn_chargeback, parts of churn) with their sum, count and logo change, written only at commit. A read groups the buckets into the merchant''s local days; MRR at an instant is mrr_totals less the movements of the buckets from it on.';

CREATE TABLE billing.mrr_streak_flows (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    bucket_at timestamptz NOT NULL,
    streak_at timestamptz NOT NULL,
    measure text NOT NULL CHECK (measure IN ('expansion', 'contraction', 'churn', 'churn_voluntary', 'churn_chargeback', 'correction')),
    amount bigint NOT NULL,
    logos bigint NOT NULL,
    PRIMARY KEY (merchant_id, currency, bucket_at, streak_at, measure),
    CONSTRAINT mrr_streak_flows_order_check CHECK (streak_at <= bucket_at)
);
COMMENT ON TABLE billing.mrr_streak_flows IS 'Expansion, contraction, churn (and its voluntary and chargeback parts) and corrections per currency and 15-minute UTC bucket, with their sum and logo change, by the bucket in which each customer''s run of MRR began (its latest movement from none): the customers with MRR when a period began are the runs begun before it, so churn and revenue retention read this and mrr_totals, never the movements.';

CREATE TABLE billing.mrr_product_totals (
    merchant_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    product_id uuid NOT NULL,
    mrr bigint NOT NULL CHECK (mrr >= 0),
    subscribers bigint NOT NULL CHECK (subscribers >= 0),
    PRIMARY KEY (merchant_id, currency, product_id),
    CONSTRAINT mrr_product_totals_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products (merchant_id, id) ON DELETE RESTRICT
);
COMMENT ON TABLE billing.mrr_product_totals IS 'Each product''s MRR and the subscriptions adding it now, per currency: its mrr_product_flows summed, kept at commit. A product''s MRR at an instant is this less its flows of the buckets from it on.';

CREATE TABLE billing.mrr_customer_totals (
    merchant_id uuid PRIMARY KEY REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    customers bigint NOT NULL CHECK (customers >= 0)
);
COMMENT ON TABLE billing.mrr_customer_totals IS 'The merchant''s customers with MRR in any currency now, each once, kept at commit; at an instant it is this less mrr_customer_flows from it on.';

CREATE TABLE billing.mrr_customer_flows (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    bucket_at timestamptz NOT NULL,
    logos bigint NOT NULL,
    PRIMARY KEY (merchant_id, bucket_at)
);
COMMENT ON TABLE billing.mrr_customer_flows IS 'Per 15-minute UTC bucket, customers gaining MRR in a first currency less those losing it in their last: the change of mrr_customer_totals.';

-- A cohort's size is its customers, read by when each first had MRR.
CREATE INDEX customer_mrr_first_active_at_idx ON billing.customer_mrr (merchant_id, first_active_at);

CREATE TABLE billing.subscription_cancellations (
    merchant_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT uuidv7(),
    subscription_id uuid NOT NULL,
    cancel_type text CHECK (cancel_type IN ('user', 'merchant', 'expired', 'chargeback', 'upgrade')),
    decided_at timestamptz NOT NULL,
    reversed_at timestamptz,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT subscription_cancellations_subscription_id_fkey FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, id) ON DELETE RESTRICT
);
CREATE INDEX subscription_cancellations_decided_at_idx ON billing.subscription_cancellations (merchant_id, decided_at);
CREATE UNIQUE INDEX subscription_cancellations_open_idx ON billing.subscription_cancellations (merchant_id, subscription_id) WHERE reversed_at IS NULL;
CREATE TRIGGER immutable_subscription_cancellations_delete BEFORE DELETE ON billing.subscription_cancellations
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
COMMENT ON TABLE billing.subscription_cancellations IS 'Each cancel decision: a subscription''s move into canceled, dated by its writer''s clock (updated_at), and when a move out of canceled (a resume) took it back. Kept by trg_subscriptions_track_cancellations; reports count cancellations here. Retention: permanent, never pruned.';

CREATE FUNCTION billing.subscriptions_track_cancellations() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF billing.billing_restore_active(NEW.merchant_id) THEN
        RETURN NULL;
    END IF;
    IF NEW.status = 'canceled' THEN
        INSERT INTO billing.subscription_cancellations (merchant_id, subscription_id, cancel_type, decided_at)
        VALUES (NEW.merchant_id, NEW.id, NEW.cancel_type, NEW.updated_at);
    ELSIF OLD.status = 'canceled' THEN
        UPDATE billing.subscription_cancellations SET reversed_at = NEW.updated_at
         WHERE merchant_id = NEW.merchant_id AND subscription_id = NEW.id AND reversed_at IS NULL;
    END IF;
    RETURN NULL;
END;
$$;
COMMENT ON FUNCTION billing.subscriptions_track_cancellations() IS 'Records a subscription''s move into canceled as a cancel decision dated by its updated_at, and a move out of canceled as that decision''s reversal.';
CREATE TRIGGER trg_subscriptions_track_cancellations AFTER UPDATE OF status ON billing.subscriptions
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status) EXECUTE FUNCTION billing.subscriptions_track_cancellations();

-- A merchant's reporting rollups rebuilt from what they derive from (its
-- movements, product flows, customer_mrr and status transitions) where it
-- has none: this migration's backfill, and a restore of an archive written
-- before them.
CREATE FUNCTION billing.rebuild_mrr_rollups(p_merchant uuid) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM billing.mrr_flows WHERE merchant_id = p_merchant AND measure = 'churn_chargeback') THEN
        INSERT INTO billing.mrr_flows (merchant_id, currency, bucket_at, measure, amount, entries, logos)
        SELECT merchant_id, currency, billing.mrr_bucket(occurred_at), 'churn_chargeback', sum(delta), count(*), sum(logo_delta)
          FROM billing.mrr_movements
         WHERE merchant_id = p_merchant AND kind = 'churn' AND cause = 'chargeback'
         GROUP BY merchant_id, currency, billing.mrr_bucket(occurred_at);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM billing.mrr_streak_flows WHERE merchant_id = p_merchant) THEN
        INSERT INTO billing.mrr_streak_flows (merchant_id, currency, bucket_at, streak_at, measure, amount, logos)
        SELECT v.merchant_id, v.currency, billing.mrr_bucket(v.occurred_at), billing.mrr_bucket(v.began_at), x.measure, sum(v.delta), sum(v.logo_delta)
          FROM (SELECT v.*, max(v.occurred_at) FILTER (WHERE v.logo_delta = 1)
                                OVER (PARTITION BY v.customer_id, v.currency ORDER BY v.occurred_at, v.id) AS began_at
                  FROM billing.mrr_movements v WHERE v.merchant_id = p_merchant) v
          CROSS JOIN LATERAL (SELECT v.kind UNION ALL SELECT 'churn_voluntary' WHERE v.voluntary
                             UNION ALL SELECT 'churn_chargeback' WHERE v.kind = 'churn' AND v.cause = 'chargeback') x(measure)
         WHERE v.kind IN ('expansion', 'contraction', 'churn', 'correction') AND v.began_at IS NOT NULL
         GROUP BY v.merchant_id, v.currency, billing.mrr_bucket(v.occurred_at), billing.mrr_bucket(v.began_at), x.measure;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM billing.mrr_product_totals WHERE merchant_id = p_merchant) THEN
        INSERT INTO billing.mrr_product_totals (merchant_id, currency, product_id, mrr, subscribers)
        SELECT merchant_id, currency, product_id, sum(amount), sum(subscribers)
          FROM billing.mrr_product_flows
         WHERE merchant_id = p_merchant
         GROUP BY merchant_id, currency, product_id
        HAVING sum(amount) <> 0 OR sum(subscribers) <> 0;
    END IF;
    -- A customer's currencies with MRR after each movement are the running
    -- sum of its logo changes.
    IF NOT EXISTS (SELECT 1 FROM billing.mrr_customer_totals WHERE merchant_id = p_merchant)
       AND NOT EXISTS (SELECT 1 FROM billing.mrr_customer_flows WHERE merchant_id = p_merchant) THEN
        INSERT INTO billing.mrr_customer_flows (merchant_id, bucket_at, logos)
        SELECT p_merchant, billing.mrr_bucket(v.occurred_at), sum((v.n > 0)::integer - (v.n - v.logo_delta > 0)::integer)
          FROM (SELECT v.occurred_at, v.logo_delta,
                       sum(v.logo_delta) OVER (PARTITION BY v.customer_id ORDER BY v.occurred_at, v.id) AS n
                  FROM billing.mrr_movements v WHERE v.merchant_id = p_merchant) v
         GROUP BY billing.mrr_bucket(v.occurred_at)
        HAVING sum((v.n > 0)::integer - (v.n - v.logo_delta > 0)::integer) <> 0;
        INSERT INTO billing.mrr_customer_totals (merchant_id, customers)
        SELECT p_merchant, count(DISTINCT customer_id) FROM billing.customer_mrr WHERE merchant_id = p_merchant AND mrr > 0
        HAVING count(DISTINCT customer_id) > 0;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM billing.subscription_cancellations WHERE merchant_id = p_merchant) THEN
        INSERT INTO billing.subscription_cancellations (merchant_id, subscription_id, cancel_type, decided_at, reversed_at)
        SELECT st.merchant_id, st.subscription_id, st.cancel_type, st.occurred_at,
               (SELECT min(r.occurred_at) FROM billing.subscription_status_transitions r
                 WHERE r.merchant_id = st.merchant_id AND r.subscription_id = st.subscription_id AND r.from_status = 'canceled'
                   AND r.to_status <> 'canceled' AND (r.occurred_at, r.id) > (st.occurred_at, st.id))
          FROM billing.subscription_status_transitions st
         WHERE st.merchant_id = p_merchant AND st.to_status = 'canceled' AND st.from_status IS NOT NULL AND st.from_status <> 'canceled';
    END IF;
END;
$$;
COMMENT ON FUNCTION billing.rebuild_mrr_rollups(p_merchant uuid) IS 'Rebuilds the merchant''s chargeback churn flows, streak flows, product totals, distinct customers and cancel decisions from what they derive from, each where the merchant has none.';
REVOKE ALL ON FUNCTION billing.rebuild_mrr_rollups(p_merchant uuid) FROM PUBLIC;
SELECT billing.rebuild_mrr_rollups(id) FROM billing.merchants ORDER BY id;

-- settle_mrr keeps the new flows at commit.
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
        INSERT INTO billing.mrr_customer_totals AS t (merchant_id, customers)
        SELECT p_merchant, sum(l)::bigint FROM unnest(v_logos) AS l
        ON CONFLICT (merchant_id) DO UPDATE SET customers = t.customers + EXCLUDED.customers;
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
    INSERT INTO billing.mrr_product_totals AS p (merchant_id, currency, product_id, mrr, subscribers)
    SELECT p_merchant, x.currency, x.product_id, sum(x.amount), sum(x.subscribers)
      FROM billing.subscription_mrr m
      CROSS JOIN LATERAL (VALUES (m.recorded_currency, m.recorded_product_id, -m.recorded_mrr, -(m.recorded_mrr > 0)::integer),
                                 (m.currency, m.product_id, m.mrr, (m.mrr > 0)::integer)) x(currency, product_id, amount, subscribers)
     WHERE m.merchant_id = p_merchant AND m.dirty
     GROUP BY x.currency, x.product_id
    HAVING sum(x.amount) <> 0 OR sum(x.subscribers) <> 0
     ORDER BY x.currency, x.product_id
    ON CONFLICT (merchant_id, currency, product_id) DO UPDATE
    SET mrr = p.mrr + EXCLUDED.mrr, subscribers = p.subscribers + EXCLUDED.subscribers;
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

-- A restore's reporting rollups agree with what they derive from.
CREATE OR REPLACE FUNCTION billing.check_billing_restore_mrr(p_merchant uuid) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF p_merchant IS DISTINCT FROM billing.current_merchant_id() THEN
        RAISE EXCEPTION 'billing restore merchant mismatch' USING ERRCODE='42501';
    END IF;
    IF EXISTS (SELECT 1 FROM billing.subscription_mrr WHERE merchant_id = p_merchant AND dirty)
       OR EXISTS (SELECT 1 FROM billing.subscriptions s WHERE s.merchant_id = p_merchant AND s.price_id IS NOT NULL
           AND NOT EXISTS (SELECT 1 FROM billing.subscription_mrr m WHERE m.merchant_id = s.merchant_id AND m.subscription_id = s.id))
       OR EXISTS (SELECT 1 FROM (SELECT customer_id, currency, mrr, subscriptions FROM billing.customer_mrr WHERE merchant_id = p_merchant) c
           FULL JOIN (SELECT customer_id, recorded_currency AS currency, sum(recorded_mrr) AS mrr, count(*) FILTER (WHERE recorded_mrr > 0) AS subscriptions
                        FROM billing.subscription_mrr WHERE merchant_id = p_merchant GROUP BY customer_id, recorded_currency) s
             ON s.customer_id = c.customer_id AND s.currency = c.currency
           WHERE COALESCE(c.mrr, 0) <> COALESCE(s.mrr, 0) OR COALESCE(c.subscriptions, 0) <> COALESCE(s.subscriptions, 0))
       OR EXISTS (SELECT 1 FROM (SELECT customer_id, currency, mrr FROM billing.customer_mrr WHERE merchant_id = p_merchant) c
           FULL JOIN (SELECT customer_id, currency, sum(delta) AS mrr FROM billing.mrr_movements WHERE merchant_id = p_merchant GROUP BY customer_id, currency) v
             ON v.customer_id = c.customer_id AND v.currency = c.currency
           WHERE COALESCE(c.mrr, 0) <> COALESCE(v.mrr, 0))
       OR EXISTS (SELECT 1 FROM (SELECT currency, mrr, customers, subscribers FROM billing.mrr_totals WHERE merchant_id = p_merchant) t
           FULL JOIN (SELECT currency, sum(mrr) AS mrr, count(*) FILTER (WHERE mrr > 0) AS customers, sum(subscriptions) AS subscribers
                        FROM billing.customer_mrr WHERE merchant_id = p_merchant GROUP BY currency) c
             ON c.currency = t.currency
           WHERE COALESCE(t.mrr, 0) <> COALESCE(c.mrr, 0) OR COALESCE(t.customers, 0) <> COALESCE(c.customers, 0)
              OR COALESCE(t.subscribers, 0) <> COALESCE(c.subscribers, 0))
       -- Each recorded reason is an open episode of the subscription's open
       -- case, and the at-risk totals are their sums.
       OR EXISTS (SELECT 1 FROM (SELECT m.subscription_id, r.reason FROM billing.subscription_mrr m, unnest(m.recorded_risk) AS r(reason) WHERE m.merchant_id = p_merchant) m
           FULL JOIN (SELECT subscription_id, reason FROM billing.mrr_risk_episodes WHERE merchant_id = p_merchant AND left_at IS NULL) e
             ON e.subscription_id = m.subscription_id AND e.reason = m.reason
           WHERE m.subscription_id IS NULL OR e.subscription_id IS NULL)
       OR EXISTS (SELECT 1 FROM (SELECT subscription_id FROM billing.subscription_mrr WHERE merchant_id = p_merchant AND recorded_risk <> '{}') m
           FULL JOIN (SELECT subscription_id FROM billing.mrr_risk_cases WHERE merchant_id = p_merchant AND closed_at IS NULL) c
             ON c.subscription_id = m.subscription_id
           WHERE m.subscription_id IS NULL OR c.subscription_id IS NULL)
       OR EXISTS (SELECT 1 FROM (SELECT currency, reason, mrr, subscriptions FROM billing.mrr_risk_totals WHERE merchant_id = p_merchant) t
           FULL JOIN (SELECT m.recorded_currency AS currency, r.reason, sum(m.recorded_mrr) AS mrr, count(*) AS subscriptions
                        FROM billing.subscription_mrr m, unnest(m.recorded_risk || CASE WHEN m.recorded_risk <> '{}' THEN '{any}'::text[] ELSE '{}' END) AS r(reason)
                       WHERE m.merchant_id = p_merchant GROUP BY m.recorded_currency, r.reason) s
             ON s.currency = t.currency AND s.reason = t.reason
           WHERE COALESCE(t.mrr, 0) <> COALESCE(s.mrr, 0) OR COALESCE(t.subscriptions, 0) <> COALESCE(s.subscriptions, 0))
       -- Each product's totals are its subscriptions' recorded MRR and its
       -- flows summed.
       OR EXISTS (SELECT 1 FROM (SELECT currency, product_id, mrr, subscribers FROM billing.mrr_product_totals WHERE merchant_id = p_merchant) t
           FULL JOIN (SELECT recorded_currency AS currency, recorded_product_id AS product_id, sum(recorded_mrr) AS mrr, count(*) FILTER (WHERE recorded_mrr > 0) AS subscribers
                        FROM billing.subscription_mrr WHERE merchant_id = p_merchant AND recorded_mrr > 0 GROUP BY recorded_currency, recorded_product_id) s
             ON s.currency = t.currency AND s.product_id = t.product_id
           WHERE COALESCE(t.mrr, 0) <> COALESCE(s.mrr, 0) OR COALESCE(t.subscribers, 0) <> COALESCE(s.subscribers, 0))
       OR EXISTS (SELECT 1 FROM (SELECT currency, product_id, mrr, subscribers FROM billing.mrr_product_totals WHERE merchant_id = p_merchant) t
           FULL JOIN (SELECT currency, product_id, sum(amount) AS mrr, sum(subscribers) AS subscribers
                        FROM billing.mrr_product_flows WHERE merchant_id = p_merchant GROUP BY currency, product_id) f
             ON f.currency = t.currency AND f.product_id = t.product_id
           WHERE COALESCE(t.mrr, 0) <> COALESCE(f.mrr, 0) OR COALESCE(t.subscribers, 0) <> COALESCE(f.subscribers, 0))
       -- The distinct customers are those with MRR in any currency, and their
       -- flows summed.
       OR COALESCE((SELECT customers FROM billing.mrr_customer_totals WHERE merchant_id = p_merchant), 0)
          <> (SELECT count(DISTINCT customer_id) FROM billing.customer_mrr WHERE merchant_id = p_merchant AND mrr > 0)
       OR COALESCE((SELECT customers FROM billing.mrr_customer_totals WHERE merchant_id = p_merchant), 0)
          <> COALESCE((SELECT sum(logos) FROM billing.mrr_customer_flows WHERE merchant_id = p_merchant), 0)
       -- The streak flows split the flows of their kinds by run, and the
       -- chargeback churn is the chargebacks' churn movements.
       OR EXISTS (SELECT 1 FROM (SELECT currency, measure, sum(amount) AS amount, sum(logos) AS logos FROM billing.mrr_streak_flows
                                  WHERE merchant_id = p_merchant GROUP BY currency, measure) s
           FULL JOIN (SELECT currency, measure, sum(amount) AS amount, sum(logos) AS logos FROM billing.mrr_flows
                       WHERE merchant_id = p_merchant AND measure IN ('expansion', 'contraction', 'churn', 'churn_voluntary', 'churn_chargeback', 'correction')
                       GROUP BY currency, measure) f
             ON f.currency = s.currency AND f.measure = s.measure
           WHERE COALESCE(s.amount, 0) <> COALESCE(f.amount, 0) OR COALESCE(s.logos, 0) <> COALESCE(f.logos, 0))
       OR EXISTS (SELECT 1 FROM (SELECT currency, sum(amount) AS amount FROM billing.mrr_flows
                                  WHERE merchant_id = p_merchant AND measure = 'churn_chargeback' GROUP BY currency) f
           FULL JOIN (SELECT currency, sum(delta) AS amount FROM billing.mrr_movements
                       WHERE merchant_id = p_merchant AND kind = 'churn' AND cause = 'chargeback' GROUP BY currency) v
             ON v.currency = f.currency
           WHERE COALESCE(f.amount, 0) <> COALESCE(v.amount, 0))
       -- A cancel decision still standing is a canceled subscription's.
       OR EXISTS (SELECT 1 FROM billing.subscription_cancellations c
                    JOIN billing.subscriptions s ON s.merchant_id = c.merchant_id AND s.id = c.subscription_id
                   WHERE c.merchant_id = p_merchant AND c.reversed_at IS NULL AND s.status <> 'canceled') THEN
        RAISE EXCEPTION 'billing restore MRR integrity mismatch' USING ERRCODE='23514';
    END IF;
END;
$$;
REVOKE ALL ON FUNCTION billing.check_billing_restore_mrr(p_merchant uuid) FROM PUBLIC;

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
                  'credit_grant_events',
                  'credit_grants',
                  'custodians',
                  'customer_delinquency',
                  'customer_invoice_profiles',
                  'customer_limit_overrides',
                  'customer_low_balance_lines',
                  'customer_messages',
                  'customer_mrr',
                  'customers',
                  'dashboard_configs',
                  'destructive_actions',
                  'destructive_holds',
                  'destructive_run_before_images',
                  'disputes',
                  'eventlog_events',
                  'fraud_reports',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'lease_in_flight',
                  'leases',
                  'ledger_accounts',
                  'ledger_daily',
                  'ledger_rollup_pending',
                  'ledger_transfers',
                  'ledger_type_totals',
                  'limit_adjustments',
                  'limit_hold_parts',
                  'limit_holds',
                  'limit_windows',
                  'maintenance_runs',
                  'mandates',
                  'merchant_destructive_policy',
                  'money_settings',
                  'mrr_cohort_flows',
                  'mrr_customer_flows',
                  'mrr_customer_totals',
                  'mrr_flows',
                  'mrr_movement_lines',
                  'mrr_movements',
                  'mrr_product_flows',
                  'mrr_product_totals',
                  'mrr_risk_cases',
                  'mrr_risk_due',
                  'mrr_risk_episodes',
                  'mrr_risk_flows',
                  'mrr_risk_totals',
                  'mrr_scheduled',
                  'mrr_streak_flows',
                  'mrr_totals',
                  'nmi_bulk_checkpoints',
                  'nmi_history_months',
                  'nmi_history_reads',
                  'notification_events',
                  'operation_log',
                  'order_attempts',
                  'order_lines',
                  'orders',
                  'owed_items',
                  'ownership_claims',
                  'payment_attempts',
                  'payment_method_versions',
                  'payment_methods',
                  'payments',
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
                  'provider_schedules',
                  'psp_customers',
                  'psp_refresh_watermarks',
                  'psps',
                  'rebill_cycles',
                  'reconciliation_findings',
                  'reconciliation_state',
                  'revenue_schedules',
                  'running_charges',
                  'scheduled_changes',
                  'solana_pay_receipts',
                  'solana_pay_references',
                  'solana_subscriptions',
                  'stored_card_checks',
                  'subscription_billing_changes',
                  'subscription_cancellations',
                  'subscription_mrr',
                  'subscription_status_transitions',
                  'subscription_verifications',
                  'subscriptions',
                  'usage_card_prints',
                  'usage_postings',
                  'usage_unposted',
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
