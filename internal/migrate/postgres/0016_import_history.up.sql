-- An import brings the merchant's history: each subscription it lands adds
-- MRR from its start to its end, at its own dates, classified along its
-- customer's timeline as live changes are. Nothing is booked at the import's
-- instant: opening goes, and the openings already written become history.

-- Rewritten below: no row event may wait for commit while a table changes.
DROP TRIGGER subscription_mrr_settle ON billing.subscription_mrr;
DROP TRIGGER immutable_mrr_movements ON billing.mrr_movements;
DROP TRIGGER immutable_mrr_movement_lines ON billing.mrr_movement_lines;

-- What an import wrote, which a later import of the customer writes again.
ALTER TABLE billing.mrr_movements ADD COLUMN imported boolean;
ALTER TABLE billing.mrr_movement_lines ADD COLUMN imported boolean;
UPDATE billing.mrr_movements SET imported = false;
UPDATE billing.mrr_movement_lines SET imported = false;
ALTER TABLE billing.mrr_movements ALTER COLUMN imported SET NOT NULL;
ALTER TABLE billing.mrr_movement_lines ALTER COLUMN imported SET NOT NULL;
COMMENT ON COLUMN billing.mrr_movements.imported IS 'Written by an import from its subscriptions'' history: the next import of the customer writes its imported movements again with its own.';
COMMENT ON COLUMN billing.mrr_movement_lines.imported IS 'A subscription''s change an import wrote: kept as written when a later import of the customer writes its history again.';

-- A movement is never rewritten. A scheduled one is deleted when a later
-- settlement writes the customer's ends again; an imported one when an import
-- writes the customer's history again (openrails.mrr_history_customer).
CREATE OR REPLACE FUNCTION billing.guard_mrr_movement() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_OP = 'DELETE' AND (OLD.scheduled
        OR OLD.imported AND OLD.customer_id::text = current_setting('openrails.mrr_history_customer', true)) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION '% is not permitted on immutable billing facts in %', TG_OP, TG_TABLE_NAME USING ERRCODE='23514';
END;
$$;
CREATE OR REPLACE FUNCTION billing.guard_mrr_movement_line() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_OP = 'DELETE' AND EXISTS (SELECT 1 FROM billing.mrr_movements v WHERE v.merchant_id = OLD.merchant_id AND v.id = OLD.movement_id AND v.scheduled) THEN
        RETURN OLD;
    END IF;
    IF TG_OP = 'DELETE' AND OLD.imported AND EXISTS (SELECT 1 FROM billing.subscriptions s WHERE s.merchant_id = OLD.merchant_id AND s.id = OLD.subscription_id
        AND s.customer_id::text = current_setting('openrails.mrr_history_customer', true)) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION '% is not permitted on immutable billing facts in %', TG_OP, TG_TABLE_NAME USING ERRCODE='23514';
END;
$$;

-- The session's working tables of an MRR history write, dropped at commit.
CREATE FUNCTION billing.mrr_history_tables() RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF to_regclass('pg_temp.mrr_history_input') IS NULL THEN
        CREATE TEMP TABLE mrr_history_input (
            merchant_id uuid NOT NULL, customer_id uuid NOT NULL, subscription_id uuid NOT NULL, cutoff timestamptz NOT NULL,
            currency text NOT NULL, product_id uuid NOT NULL, price_id uuid NOT NULL, mrr bigint NOT NULL) ON COMMIT DROP;
    END IF;
    IF to_regclass('pg_temp.mrr_history_lines') IS NULL THEN
        CREATE TEMP TABLE mrr_history_lines (
            at timestamptz NOT NULL, subscription_id uuid NOT NULL, currency text NOT NULL, product_id uuid NOT NULL, price_id uuid NOT NULL,
            mrr_before bigint NOT NULL, mrr_after bigint NOT NULL, cause text NOT NULL) ON COMMIT DROP;
    END IF;
END;
$$;
COMMENT ON FUNCTION billing.mrr_history_tables() IS 'Creates the session''s working tables of billing.write_mrr_history (pg_temp.mrr_history_input and mrr_history_lines), dropped at commit.';

-- One customer's MRR history as its imports tell it. pg_temp.mrr_history_input
-- names the subscriptions it adds, each with the instant its import read
-- (cutoff) and what it added then (0 when it had ended): each adds from its
-- start, priced by the bills it paid (a change of price or seats dated at the
-- bill's period), to its end when that came by its cutoff; its last price is
-- what it added at its cutoff. Lines earlier imports wrote stay as written.
-- The customer's imported movements are written again from all of them in
-- time order, each classified by the customer's MRR then (the sum of its
-- movements dated by then, live ones included), as a settlement classifies;
-- then every end ahead of p_as_of is written again as a scheduled movement.
-- Live movements stand.
CREATE FUNCTION billing.write_mrr_history(p_merchant uuid, p_customer uuid, p_as_of timestamptz) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' SET enable_seqscan TO off SET plan_cache_mode TO force_custom_plan AS $$
DECLARE
    v_at timestamptz;
    v_currency text;
    v_end record;
    v_total bigint;
    v_before bigint;
    v_after bigint;
    v_logo integer;
    v_active integer;
    v_first timestamptz;
    v_cause text;
    v_kind text;
    v_movement uuid;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('openrails.mrr:' || p_merchant::text || ':' || p_customer::text, 0));
    DELETE FROM mrr_history_lines;

    -- The subscriptions this import adds: its segments, each from the bill
    -- that began it, on what it then billed.
    INSERT INTO mrr_history_lines (at, subscription_id, currency, product_id, price_id, mrr_before, mrr_after, cause)
    WITH span AS (
        SELECT i.subscription_id, i.cutoff, i.currency AS cut_currency, i.product_id AS cut_product, i.price_id AS cut_price, i.mrr AS cut_mrr,
               s.price_id AS sub_price, COALESCE(s.quantity, 1) AS sub_quantity, s.cancel_type,
               CASE WHEN p.trial_duration_hours IS NOT NULL THEN s.started_at + make_interval(hours => p.trial_duration_hours) ELSE s.started_at END AS starts,
               CASE WHEN i.mrr > 0 THEN NULL::timestamptz
                    WHEN s.status = 'canceled' AND COALESCE(s.ended_at, s.current_period_ends_at) <= i.cutoff THEN COALESCE(s.ended_at, s.current_period_ends_at) END AS ends
          FROM mrr_history_input i
          JOIN billing.subscriptions s ON s.merchant_id = p_merchant AND s.id = i.subscription_id
          JOIN billing.prices p ON p.merchant_id = p_merchant AND p.id = s.price_id
         WHERE i.merchant_id = p_merchant AND i.customer_id = p_customer
    ),
    paid AS (
        -- Added MRR at its cutoff, or ended by then after it began.
        SELECT sp.*, CASE WHEN sp.cut_mrr > 0 THEN least(sp.starts, sp.cutoff) ELSE sp.starts END AS since
          FROM span sp
         WHERE sp.cut_mrr > 0 OR sp.ends > sp.starts
    ),
    bills AS (
        SELECT pd.subscription_id, inv.period_starts_at AS at, l.price_id, COALESCE(l.quantity, 1) AS quantity,
               row_number() OVER (PARTITION BY pd.subscription_id ORDER BY inv.period_starts_at, inv.id) AS n
          FROM paid pd
          JOIN billing.invoices inv ON inv.merchant_id = p_merchant AND inv.subscription_id = pd.subscription_id AND inv.origin = 'subscription'
           AND inv.status = 'complete' AND inv.period_starts_at <= pd.cutoff AND (pd.ends IS NULL OR inv.period_starts_at < pd.ends)
         CROSS JOIN LATERAL (SELECT l.price_id, l.quantity FROM billing.invoice_lines l
                              WHERE l.merchant_id = p_merchant AND l.invoice_id = inv.id AND l.price_id IS NOT NULL
                              ORDER BY l.position LIMIT 1) l
    ),
    terms AS (
        SELECT b.*, lag(b.price_id) OVER w AS prev_price, lag(b.quantity) OVER w AS prev_quantity
          FROM bills b WINDOW w AS (PARTITION BY b.subscription_id ORDER BY b.n)
    ),
    segment AS (
        -- From its start on its first bill's terms (its own without one),
        -- then at each bill that changed them.
        SELECT pd.subscription_id, pd.since AS at, 0 AS ord, COALESCE(t.price_id, pd.sub_price) AS price_id, COALESCE(t.quantity, pd.sub_quantity) AS quantity
          FROM paid pd LEFT JOIN terms t ON t.subscription_id = pd.subscription_id AND t.n = 1
        UNION ALL
        SELECT t.subscription_id, greatest(t.at, pd.since), t.n, t.price_id, t.quantity
          FROM terms t JOIN paid pd ON pd.subscription_id = t.subscription_id
         WHERE t.n > 1 AND (t.price_id, t.quantity) IS DISTINCT FROM (t.prev_price, t.prev_quantity)
    ),
    valued AS (
        -- What each segment added; the last, of one that added MRR at its
        -- cutoff, is what it added then.
        SELECT g.subscription_id, g.at, g.ord,
               CASE WHEN x.last AND pd.cut_mrr > 0 THEN pd.cut_currency ELSE pr.currency END AS currency,
               CASE WHEN x.last AND pd.cut_mrr > 0 THEN pd.cut_product ELSE pr.product_id END AS product_id,
               CASE WHEN x.last AND pd.cut_mrr > 0 THEN pd.cut_price ELSE g.price_id END AS price_id,
               CASE WHEN x.last AND pd.cut_mrr > 0 THEN pd.cut_mrr ELSE billing.price_mrr(pr, g.quantity) END AS mrr,
               g.price_id AS billed_price, g.quantity
          FROM segment g
          JOIN paid pd ON pd.subscription_id = g.subscription_id
          JOIN billing.prices pr ON pr.merchant_id = p_merchant AND pr.id = g.price_id
         CROSS JOIN LATERAL (SELECT NOT EXISTS (SELECT 1 FROM segment g2 WHERE g2.subscription_id = g.subscription_id
                                                 AND (g2.at, g2.ord) > (g.at, g.ord)) AS last) x
    ),
    step AS (
        SELECT v.*, lag(v.currency) OVER w AS prev_currency, lag(v.product_id) OVER w AS prev_product, lag(v.price_id) OVER w AS prev_price,
               lag(v.billed_price) OVER w AS prev_billed, lag(v.mrr) OVER w AS prev_mrr, row_number() OVER w AS n
          FROM valued v WINDOW w AS (PARTITION BY v.subscription_id ORDER BY v.at, v.ord)
    ),
    lines AS (
        -- Entering, changing, or leaving one product and entering another.
        SELECT s.at, s.subscription_id, s.currency, s.product_id, s.price_id, 0::bigint AS mrr_before, s.mrr AS mrr_after, 'subscribed' AS cause
          FROM step s WHERE s.n = 1
        UNION ALL
        SELECT s.at, s.subscription_id, s.currency, s.product_id, s.price_id, s.prev_mrr, s.mrr,
               CASE WHEN s.billed_price IS DISTINCT FROM s.prev_billed THEN 'price_change' ELSE 'seat_change' END
          FROM step s WHERE s.n > 1 AND (s.currency, s.product_id) = (s.prev_currency, s.prev_product)
        UNION ALL
        SELECT s.at, s.subscription_id, s.prev_currency, s.prev_product, s.prev_price, s.prev_mrr, 0, 'price_change'
          FROM step s WHERE s.n > 1 AND (s.currency, s.product_id) IS DISTINCT FROM (s.prev_currency, s.prev_product)
        UNION ALL
        SELECT s.at, s.subscription_id, s.currency, s.product_id, s.price_id, 0, s.mrr, 'price_change'
          FROM step s WHERE s.n > 1 AND (s.currency, s.product_id) IS DISTINCT FROM (s.prev_currency, s.prev_product)
        UNION ALL
        -- Its end, from what its last segment added.
        SELECT pd.ends, s.subscription_id, s.currency, s.product_id, s.price_id, s.mrr, 0, billing.mrr_cancel_cause(pd.cancel_type)
          FROM step s JOIN paid pd ON pd.subscription_id = s.subscription_id
         WHERE pd.ends IS NOT NULL AND NOT EXISTS (SELECT 1 FROM step s2 WHERE s2.subscription_id = s.subscription_id AND s2.n > s.n)
    )
    SELECT l.at, l.subscription_id, l.currency, l.product_id, l.price_id, l.mrr_before, l.mrr_after, l.cause
      FROM lines l WHERE l.mrr_before <> l.mrr_after;

    -- What earlier imports wrote of the customer's other subscriptions.
    INSERT INTO mrr_history_lines (at, subscription_id, currency, product_id, price_id, mrr_before, mrr_after, cause)
    SELECT l.occurred_at, l.subscription_id, l.currency, l.product_id, l.price_id, l.mrr_before, l.mrr_after, l.cause
      FROM billing.subscriptions s
      JOIN billing.mrr_movement_lines l ON l.merchant_id = p_merchant AND l.subscription_id = s.id AND l.imported
     WHERE s.merchant_id = p_merchant AND s.customer_id = p_customer
       AND NOT EXISTS (SELECT 1 FROM mrr_history_input i WHERE i.merchant_id = p_merchant AND i.subscription_id = s.id);

    -- The customer's imported movements, and its ends ahead, go.
    PERFORM set_config('openrails.mrr_history_customer', p_customer::text, true);
    DELETE FROM billing.mrr_movement_lines l
     USING billing.mrr_movements v
     WHERE v.merchant_id = p_merchant AND v.customer_id = p_customer AND (v.imported AND NOT v.scheduled OR v.scheduled AND v.occurred_at > p_as_of)
       AND l.merchant_id = p_merchant AND l.movement_id = v.id;
    DELETE FROM billing.mrr_movement_lines l
     USING billing.subscriptions s
     WHERE s.merchant_id = p_merchant AND s.customer_id = p_customer
       AND l.merchant_id = p_merchant AND l.subscription_id = s.id AND l.movement_id IS NULL AND l.imported;
    DELETE FROM billing.mrr_movements v
     WHERE v.merchant_id = p_merchant AND v.customer_id = p_customer AND (v.imported AND NOT v.scheduled OR v.scheduled AND v.occurred_at > p_as_of);
    PERFORM set_config('openrails.mrr_history_customer', '', true);

    -- In time order: per instant, per currency whose MRR changed, a movement
    -- classified by the customer's MRR then, with its lines.
    FOR v_at IN SELECT DISTINCT h.at FROM mrr_history_lines h ORDER BY 1 LOOP
        SELECT count(*) FILTER (WHERE x.mrr > 0) INTO v_active
          FROM (SELECT sum(v.delta) AS mrr FROM billing.mrr_movements v
                 WHERE v.merchant_id = p_merchant AND v.customer_id = p_customer AND v.occurred_at <= v_at
                 GROUP BY v.currency) x;
        FOR v_currency IN SELECT DISTINCT h.currency FROM mrr_history_lines h WHERE h.at = v_at ORDER BY 1 LOOP
            SELECT COALESCE(sum(h.mrr_after - h.mrr_before), 0) INTO v_total FROM mrr_history_lines h WHERE h.at = v_at AND h.currency = v_currency;
            v_movement := NULL;
            IF v_total <> 0 THEN
                SELECT COALESCE(sum(v.delta), 0), min(v.cohort_at) INTO v_before, v_first FROM billing.mrr_movements v
                 WHERE v.merchant_id = p_merchant AND v.customer_id = p_customer AND v.currency = v_currency AND v.occurred_at <= v_at;
                v_after := v_before + v_total;
                v_logo := (v_after > 0)::integer - (v_before > 0)::integer;
                SELECT p.cause INTO v_cause
                  FROM (SELECT h.subscription_id, h.cause, sum(h.mrr_after - h.mrr_before) AS net
                          FROM mrr_history_lines h WHERE h.at = v_at AND h.currency = v_currency
                         GROUP BY h.subscription_id, h.cause) p
                 WHERE p.net <> 0 ORDER BY (p.cause = 'upgrade') DESC, abs(p.net) DESC, p.subscription_id LIMIT 1;
                v_kind := CASE
                    WHEN v_before = 0 THEN CASE WHEN v_first IS NULL THEN 'new' ELSE 'reactivation' END
                    WHEN v_after = 0 THEN 'churn'
                    WHEN v_total > 0 THEN 'expansion'
                    ELSE 'contraction'
                END;
                v_first := COALESCE(v_first, v_at);
                v_active := v_active + v_logo;
                INSERT INTO billing.mrr_movements (merchant_id, customer_id, currency, kind, delta, mrr_before, mrr_after, logo_delta,
                    customer_logo_delta, cohort_at, cause, voluntary, occurred_at, scheduled, imported)
                VALUES (p_merchant, p_customer, v_currency, v_kind, v_total, v_before, v_after, v_logo,
                    (v_active > 0)::integer - (v_active - v_logo > 0)::integer, v_first, v_cause,
                    CASE WHEN v_kind = 'churn' THEN v_cause IN ('canceled', 'merchant_canceled') END, v_at, false, true)
                RETURNING id INTO v_movement;
            END IF;
            INSERT INTO billing.mrr_movement_lines (merchant_id, movement_id, subscription_id, product_id, price_id, delta, mrr_before, mrr_after,
                subscriber_delta, cause, occurred_at, currency, imported)
            SELECT p_merchant, v_movement, h.subscription_id, h.product_id, h.price_id, h.mrr_after - h.mrr_before, h.mrr_before, h.mrr_after,
                   (h.mrr_after > 0)::integer - (h.mrr_before > 0)::integer, h.cause, v_at, v_currency, true
              FROM mrr_history_lines h WHERE h.at = v_at AND h.currency = v_currency;
        END LOOP;
    END LOOP;

    -- Each end ahead of the import, by when it comes: churn, or contraction
    -- when other MRR stays.
    SELECT count(*) FILTER (WHERE x.mrr > 0) INTO v_active
      FROM (SELECT sum(v.delta) AS mrr FROM billing.mrr_movements v
             WHERE v.merchant_id = p_merchant AND v.customer_id = p_customer AND v.occurred_at <= p_as_of
             GROUP BY v.currency) x;
    FOR v_end IN
        SELECT m.currency, m.ends_at AS at, sum(m.mrr)::bigint AS level,
               (array_agg(billing.mrr_cancel_cause(s.cancel_type) ORDER BY m.mrr DESC, m.subscription_id))[1] AS cause
          FROM billing.subscription_mrr m
          JOIN billing.subscriptions s ON s.merchant_id = m.merchant_id AND s.id = m.subscription_id
         WHERE m.merchant_id = p_merchant AND m.customer_id = p_customer AND m.mrr > 0 AND m.ends_at > p_as_of
         GROUP BY m.currency, m.ends_at
         ORDER BY m.ends_at, m.currency
    LOOP
        SELECT COALESCE(sum(v.delta), 0), min(v.cohort_at) INTO v_before, v_first FROM billing.mrr_movements v
         WHERE v.merchant_id = p_merchant AND v.customer_id = p_customer AND v.currency = v_end.currency AND v.occurred_at < v_end.at;
        v_after := v_before - v_end.level;
        v_logo := -(v_after = 0)::integer;
        v_kind := CASE WHEN v_after = 0 THEN 'churn' ELSE 'contraction' END;
        v_active := v_active + v_logo;
        INSERT INTO billing.mrr_movements (merchant_id, customer_id, currency, kind, delta, mrr_before, mrr_after, logo_delta,
            customer_logo_delta, cohort_at, cause, voluntary, occurred_at, scheduled, imported)
        VALUES (p_merchant, p_customer, v_end.currency, v_kind, -v_end.level, v_before, v_after, v_logo,
            (v_active > 0)::integer - (v_active - v_logo > 0)::integer, v_first, v_end.cause,
            CASE WHEN v_kind = 'churn' THEN v_end.cause IN ('canceled', 'merchant_canceled') END, v_end.at, true, false)
        RETURNING id INTO v_movement;
        INSERT INTO billing.mrr_movement_lines (merchant_id, movement_id, subscription_id, product_id, price_id, delta, mrr_before, mrr_after,
            subscriber_delta, cause, occurred_at, currency, imported)
        SELECT p_merchant, v_movement, m.subscription_id, m.product_id, m.price_id, -m.mrr, m.mrr, 0, -1,
               billing.mrr_cancel_cause(s.cancel_type), v_end.at, v_end.currency, false
          FROM billing.subscription_mrr m
          JOIN billing.subscriptions s ON s.merchant_id = m.merchant_id AND s.id = m.subscription_id
         WHERE m.merchant_id = p_merchant AND m.customer_id = p_customer AND m.currency = v_end.currency
           AND m.ends_at = v_end.at AND m.mrr > 0;
    END LOOP;
END;
$$;
COMMENT ON FUNCTION billing.write_mrr_history(p_merchant uuid, p_customer uuid, p_as_of timestamptz) IS 'Writes one customer''s imported MRR history again: the subscriptions pg_temp.mrr_history_input names from their start, priced by their bills, to their end or what they added at their import, beside the lines earlier imports wrote; in time order, each movement classified by the customer''s MRR then, live movements included; then its ends ahead of p_as_of as scheduled movements.';

-- The import's entry: the subscriptions it landed, at its evidence instant.
-- Each adds what its subscription_mrr row holds then; one an earlier import
-- already wrote keeps its history. Their rows are then recorded: the commit
-- settles nothing of them.
CREATE FUNCTION billing.record_mrr_history(p_merchant uuid, p_subscriptions uuid[], p_as_of timestamptz) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' SET enable_seqscan TO off SET plan_cache_mode TO force_custom_plan AS $$
DECLARE
    v_customer uuid;
BEGIN
    PERFORM billing.mrr_history_tables();
    DELETE FROM mrr_history_input;
    INSERT INTO mrr_history_input (merchant_id, customer_id, subscription_id, cutoff, currency, product_id, price_id, mrr)
    SELECT m.merchant_id, m.customer_id, m.subscription_id, p_as_of, m.currency, m.product_id, m.price_id,
           CASE WHEN m.ends_at IS NULL OR m.ends_at > p_as_of THEN m.mrr ELSE 0 END
      FROM billing.subscription_mrr m
     WHERE m.merchant_id = p_merchant AND m.subscription_id = ANY(p_subscriptions)
       AND NOT EXISTS (SELECT 1 FROM billing.mrr_movement_lines l WHERE l.merchant_id = p_merchant AND l.subscription_id = m.subscription_id AND l.imported);
    FOR v_customer IN SELECT DISTINCT i.customer_id FROM mrr_history_input i ORDER BY 1 LOOP
        PERFORM billing.write_mrr_history(p_merchant, v_customer, p_as_of);
    END LOOP;
    UPDATE billing.subscription_mrr m
       SET recorded_currency = m.currency, recorded_product_id = m.product_id, recorded_price_id = m.price_id,
           recorded_mrr = m.mrr, recorded_ends_at = m.ends_at
      FROM mrr_history_input i
     WHERE m.merchant_id = p_merchant AND m.customer_id = i.customer_id AND m.subscription_id = i.subscription_id
       AND ROW(m.recorded_currency, m.recorded_product_id, m.recorded_price_id, m.recorded_mrr, m.recorded_ends_at)
           IS DISTINCT FROM ROW(m.currency, m.product_id, m.price_id, m.mrr, m.ends_at);
    DELETE FROM mrr_history_input;
END;
$$;
COMMENT ON FUNCTION billing.record_mrr_history(p_merchant uuid, p_subscriptions uuid[], p_as_of timestamptz) IS 'Records the MRR history of the subscriptions an import landed, as of its evidence instant p_as_of: each customer''s history written again (billing.write_mrr_history), then the subscriptions'' rows recorded so the commit settles nothing of them. A subscription an earlier import wrote keeps its history.';

-- Settlements classify live changes alone: an import writes its history.
CREATE OR REPLACE FUNCTION billing.settle_customer_mrr(p_merchant uuid, p_customer uuid) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' SET enable_seqscan TO off SET plan_cache_mode TO force_custom_plan AS $$
DECLARE
    v_at timestamptz;
    v_currency text;
    v_end record;
    v_total bigint;
    v_before bigint;
    v_after bigint;
    v_logo integer;
    v_active integer;
    v_first timestamptz;
    v_cause text;
    v_kind text;
    v_movement uuid;
BEGIN
    SELECT max(m.changed_at) INTO v_at FROM billing.subscription_mrr m
     WHERE m.merchant_id = p_merchant AND m.customer_id = p_customer AND m.dirty
       AND ROW(m.currency, m.product_id, m.price_id, m.mrr, m.ends_at)
           IS DISTINCT FROM ROW(m.recorded_currency, m.recorded_product_id, m.recorded_price_id, m.recorded_mrr, m.recorded_ends_at);
    IF v_at IS NULL THEN
        RETURN;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('openrails.mrr:' || p_merchant::text || ':' || p_customer::text, 0));
    SELECT greatest(v_at, max(v.occurred_at)) INTO v_at FROM billing.mrr_movements v
     WHERE v.merchant_id = p_merchant AND v.customer_id = p_customer AND NOT v.scheduled;
    DELETE FROM billing.mrr_movement_lines l
     USING billing.mrr_movements v
     WHERE v.merchant_id = p_merchant AND v.customer_id = p_customer AND v.scheduled AND v.occurred_at > v_at
       AND l.merchant_id = p_merchant AND l.movement_id = v.id;
    DELETE FROM billing.mrr_movements v
     WHERE v.merchant_id = p_merchant AND v.customer_id = p_customer AND v.scheduled AND v.occurred_at > v_at;
    -- The customer's MRR at an instant is the sum of its movements dated by
    -- then, whatever order they were written in: its currencies with MRR at
    -- the change.
    SELECT count(*) FILTER (WHERE x.mrr > 0) INTO v_active
      FROM (SELECT sum(v.delta) AS mrr FROM billing.mrr_movements v
             WHERE v.merchant_id = p_merchant AND v.customer_id = p_customer AND v.occurred_at <= v_at
             GROUP BY v.currency) x;
    FOR v_currency IN
        SELECT DISTINCT c.currency FROM billing.subscription_mrr m
         CROSS JOIN LATERAL (VALUES (m.currency), (m.recorded_currency)) c(currency)
         WHERE m.merchant_id = p_merchant AND m.customer_id = p_customer AND m.dirty
         ORDER BY 1
    LOOP
        SELECT COALESCE(sum(l.mrr_after - l.mrr_before), 0) INTO v_total
          FROM billing.pending_mrr_lines(p_merchant, p_customer, v_currency, v_at) l;
        v_movement := NULL;
        IF v_total <> 0 THEN
            SELECT COALESCE(sum(v.delta), 0), min(v.cohort_at) INTO v_before, v_first FROM billing.mrr_movements v
             WHERE v.merchant_id = p_merchant AND v.customer_id = p_customer AND v.currency = v_currency AND v.occurred_at <= v_at;
            v_after := v_before + v_total;
            v_logo := (v_after > 0)::integer - (v_before > 0)::integer;
            -- An upgrade explains the whole movement; otherwise its largest
            -- change does.
            SELECT p.cause INTO v_cause
              FROM (SELECT l.subscription_id, l.cause, sum(l.mrr_after - l.mrr_before) AS net
                      FROM billing.pending_mrr_lines(p_merchant, p_customer, v_currency, v_at) l
                     GROUP BY l.subscription_id, l.cause) p
             WHERE p.net <> 0 ORDER BY (p.cause = 'upgrade') DESC, abs(p.net) DESC, p.subscription_id LIMIT 1;
            v_kind := CASE
                WHEN v_cause IN ('prune', 'undo') THEN 'correction'
                WHEN v_before = 0 THEN CASE WHEN v_first IS NULL THEN 'new' ELSE 'reactivation' END
                WHEN v_after = 0 THEN 'churn'
                WHEN v_total > 0 THEN 'expansion'
                ELSE 'contraction'
            END;
            v_first := COALESCE(v_first, v_at);
            v_active := v_active + v_logo;
            INSERT INTO billing.mrr_movements (merchant_id, customer_id, currency, kind, delta, mrr_before, mrr_after, logo_delta,
                customer_logo_delta, cohort_at, cause, voluntary, occurred_at, scheduled, imported)
            VALUES (p_merchant, p_customer, v_currency, v_kind, v_total, v_before, v_after, v_logo,
                (v_active > 0)::integer - (v_active - v_logo > 0)::integer, v_first, v_cause,
                CASE WHEN v_kind = 'churn' THEN v_cause IN ('canceled', 'merchant_canceled') END, v_at, false, false)
            RETURNING id INTO v_movement;
        END IF;
        -- A move between products at an equal MRR is lines alone.
        INSERT INTO billing.mrr_movement_lines (merchant_id, movement_id, subscription_id, product_id, price_id, delta, mrr_before, mrr_after,
            subscriber_delta, cause, occurred_at, currency, imported)
        SELECT p_merchant, v_movement, l.subscription_id, l.product_id, l.price_id, l.mrr_after - l.mrr_before, l.mrr_before, l.mrr_after,
               (l.mrr_after > 0)::integer - (l.mrr_before > 0)::integer, l.cause, v_at, v_currency, false
          FROM billing.pending_mrr_lines(p_merchant, p_customer, v_currency, v_at) l;
    END LOOP;
    -- Each end still ahead, by when it comes: churn, or contraction when
    -- other MRR stays.
    FOR v_end IN
        SELECT m.currency, m.ends_at AS at, sum(m.mrr)::bigint AS level,
               (array_agg(billing.mrr_cancel_cause(s.cancel_type) ORDER BY m.mrr DESC, m.subscription_id))[1] AS cause
          FROM billing.subscription_mrr m
          JOIN billing.subscriptions s ON s.merchant_id = m.merchant_id AND s.id = m.subscription_id
         WHERE m.merchant_id = p_merchant AND m.customer_id = p_customer AND m.mrr > 0 AND m.ends_at > v_at
         GROUP BY m.currency, m.ends_at
         ORDER BY m.ends_at, m.currency
    LOOP
        SELECT COALESCE(sum(v.delta), 0), min(v.cohort_at) INTO v_before, v_first FROM billing.mrr_movements v
         WHERE v.merchant_id = p_merchant AND v.customer_id = p_customer AND v.currency = v_end.currency AND v.occurred_at < v_end.at;
        v_after := v_before - v_end.level;
        v_logo := -(v_after = 0)::integer;
        v_kind := CASE WHEN v_after = 0 THEN 'churn' ELSE 'contraction' END;
        v_active := v_active + v_logo;
        INSERT INTO billing.mrr_movements (merchant_id, customer_id, currency, kind, delta, mrr_before, mrr_after, logo_delta,
            customer_logo_delta, cohort_at, cause, voluntary, occurred_at, scheduled, imported)
        VALUES (p_merchant, p_customer, v_end.currency, v_kind, -v_end.level, v_before, v_after, v_logo,
            (v_active > 0)::integer - (v_active - v_logo > 0)::integer, v_first, v_end.cause,
            CASE WHEN v_kind = 'churn' THEN v_end.cause IN ('canceled', 'merchant_canceled') END, v_end.at, true, false)
        RETURNING id INTO v_movement;
        INSERT INTO billing.mrr_movement_lines (merchant_id, movement_id, subscription_id, product_id, price_id, delta, mrr_before, mrr_after,
            subscriber_delta, cause, occurred_at, currency, imported)
        SELECT p_merchant, v_movement, m.subscription_id, m.product_id, m.price_id, -m.mrr, m.mrr, 0, -1,
               billing.mrr_cancel_cause(s.cancel_type), v_end.at, v_end.currency, false
          FROM billing.subscription_mrr m
          JOIN billing.subscriptions s ON s.merchant_id = m.merchant_id AND s.id = m.subscription_id
         WHERE m.merchant_id = p_merchant AND m.customer_id = p_customer AND m.currency = v_end.currency
           AND m.ends_at = v_end.at AND m.mrr > 0;
    END LOOP;
END;
$$;
COMMENT ON FUNCTION billing.settle_customer_mrr(p_merchant uuid, p_customer uuid) IS 'Records one customer''s dirty subscription_mrr rows: per currency one mrr_movements row at the latest change, classified by the customer''s MRR there, with its lines (lines alone when products moved at an equal MRR); then deletes its scheduled movements after the change and writes one per currency and end still ahead.';

CREATE OR REPLACE FUNCTION billing.subscriptions_track_mrr() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.price_id IS NULL OR TG_OP = 'UPDATE' AND ROW(OLD.status, OLD.price_id, OLD.product_id, OLD.quantity, OLD.ended_at,
            OLD.current_period_starts_at, OLD.current_period_ends_at, OLD.deleted_at, OLD.cancel_type)
        IS NOT DISTINCT FROM ROW(NEW.status, NEW.price_id, NEW.product_id, NEW.quantity, NEW.ended_at,
            NEW.current_period_starts_at, NEW.current_period_ends_at, NEW.deleted_at, NEW.cancel_type) THEN
        RETURN NULL;
    END IF;
    IF billing.billing_restore_active(NEW.merchant_id) THEN
        RETURN NULL;
    END IF;
    PERFORM billing.refresh_subscription_mrr(NEW, NEW.updated_at, CASE
        WHEN current_setting('openrails.mrr_cause', true) = 'undo' THEN 'undo'
        WHEN TG_OP = 'UPDATE' AND OLD.deleted_at IS DISTINCT FROM NEW.deleted_at THEN CASE WHEN NEW.deleted_at IS NULL THEN 'undo' ELSE 'prune' END
        WHEN NEW.status = 'canceled' THEN billing.mrr_cancel_cause(NEW.cancel_type)
        WHEN TG_OP = 'INSERT' THEN 'subscribed'
        WHEN OLD.price_id IS DISTINCT FROM NEW.price_id THEN 'price_change'
        WHEN OLD.quantity IS DISTINCT FROM NEW.quantity THEN 'seat_change'
        WHEN OLD.status = 'canceled' THEN 'resumed'
        WHEN OLD.status = 'pending' THEN 'subscribed'
        ELSE 'renewed'
    END);
    RETURN NULL;
END;
$$;

-- Openings become history. A subscription an import opened runs from its
-- start to the opening, at what the opening counted; one an import touched
-- without opening it (landed ended) runs from its start to its end. The
-- import movements go with their lines; each customer's history is written
-- in their place (live movements after an opening keep how they were
-- classified then).
SELECT billing.mrr_history_tables();
INSERT INTO mrr_history_input (merchant_id, customer_id, subscription_id, cutoff, currency, product_id, price_id, mrr)
SELECT DISTINCT ON (l.merchant_id, l.subscription_id) l.merchant_id, v.customer_id, l.subscription_id, max(l.occurred_at) OVER w,
       l.currency, l.product_id, l.price_id, sum(l.delta) OVER w
  FROM billing.mrr_movement_lines l
  JOIN billing.mrr_movements v ON v.merchant_id = l.merchant_id AND v.id = l.movement_id
 WHERE l.cause = 'import'
WINDOW w AS (PARTITION BY l.merchant_id, l.subscription_id)
 ORDER BY l.merchant_id, l.subscription_id, l.occurred_at DESC, l.id DESC;
INSERT INTO mrr_history_input (merchant_id, customer_id, subscription_id, cutoff, currency, product_id, price_id, mrr)
SELECT m.merchant_id, m.customer_id, m.subscription_id, now(), m.currency, m.product_id, m.price_id, 0
  FROM billing.subscription_mrr m
  JOIN billing.subscriptions s ON s.merchant_id = m.merchant_id AND s.id = m.subscription_id
 WHERE (m.cause = 'import' OR s.cancel_feedback LIKE 'imported: declared %')
   AND NOT EXISTS (SELECT 1 FROM mrr_history_input i WHERE i.merchant_id = m.merchant_id AND i.subscription_id = m.subscription_id);
DELETE FROM billing.mrr_movement_lines l
 WHERE l.cause = 'import' OR EXISTS (SELECT 1 FROM billing.mrr_movements v WHERE v.merchant_id = l.merchant_id AND v.id = l.movement_id AND v.cause = 'import');
DELETE FROM billing.mrr_movements WHERE cause = 'import';
UPDATE billing.subscription_mrr SET cause = 'subscribed' WHERE cause = 'import';
SELECT billing.write_mrr_history(c.merchant_id, c.customer_id, now())
  FROM (SELECT DISTINCT i.merchant_id, i.customer_id FROM mrr_history_input i ORDER BY 1, 2) c;
DELETE FROM mrr_history_input;

-- No import names a cause, and opening is no kind.
ALTER TABLE billing.mrr_movements DROP CONSTRAINT mrr_movements_kind_check,
    ADD CONSTRAINT mrr_movements_kind_check CHECK (kind IN ('new', 'expansion', 'contraction', 'churn', 'reactivation', 'correction')),
    DROP CONSTRAINT mrr_movements_cause_check,
    ADD CONSTRAINT mrr_movements_cause_check CHECK (cause IN ('subscribed', 'resumed', 'renewed', 'price_change', 'seat_change', 'canceled', 'merchant_canceled', 'expired', 'chargeback', 'upgrade', 'prune', 'undo'));
ALTER TABLE billing.mrr_movement_lines DROP CONSTRAINT mrr_movement_lines_cause_check,
    ADD CONSTRAINT mrr_movement_lines_cause_check CHECK (cause IN ('subscribed', 'resumed', 'renewed', 'price_change', 'seat_change', 'canceled', 'merchant_canceled', 'expired', 'chargeback', 'upgrade', 'prune', 'undo'));
ALTER TABLE billing.subscription_mrr DROP CONSTRAINT subscription_mrr_cause_check,
    ADD CONSTRAINT subscription_mrr_cause_check CHECK (cause IN ('subscribed', 'resumed', 'renewed', 'price_change', 'seat_change', 'canceled', 'merchant_canceled', 'expired', 'chargeback', 'upgrade', 'prune', 'undo'));

CREATE TRIGGER immutable_mrr_movements BEFORE UPDATE OR DELETE ON billing.mrr_movements
    FOR EACH ROW EXECUTE FUNCTION billing.guard_mrr_movement();
CREATE TRIGGER immutable_mrr_movement_lines BEFORE UPDATE OR DELETE ON billing.mrr_movement_lines
    FOR EACH ROW EXECUTE FUNCTION billing.guard_mrr_movement_line();
CREATE CONSTRAINT TRIGGER subscription_mrr_settle AFTER INSERT OR UPDATE ON billing.subscription_mrr DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION billing.settle_at_commit();

COMMENT ON TABLE billing.mrr_movements IS 'One change of a customer''s MRR in one currency: new (first MRR), reactivation (MRR again after none), expansion, contraction, churn (to none; voluntary for a customer''s or merchant''s cancel) or correction (a prune or an undo). Written at the commit of the transaction that made it, or by an import at the instants of its subscriptions'' history (imported); a scheduled one (a cancellation at period end) is dated at the end and counts once that instant passes. Retention: permanent, never pruned.';
