-- At-risk MRR: a contributing subscription is at risk while past due, waiting
-- for a payment method, unverified, renewing after its card expires, or while
-- its customer is delinquent in its currency. subscription_mrr holds each
-- subscription's reasons; at commit billing.settle_mrr opens, chains and
-- closes its case and the episodes of each reason, and keeps the rollups.

ALTER TABLE billing.subscription_mrr
    ADD COLUMN risk text[] NOT NULL DEFAULT '{}',
    ADD COLUMN recorded_risk text[] NOT NULL DEFAULT '{}',
    ADD COLUMN risk_changed_at timestamptz;
ALTER TABLE billing.subscription_mrr ALTER COLUMN risk DROP DEFAULT, ALTER COLUMN recorded_risk DROP DEFAULT;
ALTER TABLE billing.subscription_mrr
    ADD CONSTRAINT subscription_mrr_risk_check CHECK (risk <@ ARRAY['past_due', 'needs_payment_method', 'unverified', 'card_expiring', 'delinquent']::text[] AND (risk = '{}' OR mrr > 0)),
    ADD CONSTRAINT subscription_mrr_recorded_risk_check CHECK (recorded_risk <@ ARRAY['past_due', 'needs_payment_method', 'unverified', 'card_expiring', 'delinquent']::text[] AND (recorded_risk = '{}' OR recorded_mrr > 0)),
    ADD CONSTRAINT subscription_mrr_risk_changed_at_check CHECK ((risk = '{}' AND recorded_risk = '{}') OR risk_changed_at IS NOT NULL);
ALTER TABLE billing.subscription_mrr ALTER COLUMN dirty SET EXPRESSION AS (mrr <> recorded_mrr OR currency <> recorded_currency
    OR product_id <> recorded_product_id OR price_id <> recorded_price_id OR risk <> recorded_risk);
CREATE INDEX subscription_mrr_risk_idx ON billing.subscription_mrr (merchant_id, subscription_id)
    WHERE risk <> '{}';
COMMENT ON COLUMN billing.subscription_mrr.risk IS 'Why the subscription''s MRR is at risk now: past_due, needs_payment_method, unverified, card_expiring (the card it charges expires before its renewal) and delinquent (its customer, in its currency), in that order; empty when it is not at risk or adds no MRR. Canceled at its period end, it keeps the reasons it had until its MRR ends.';
COMMENT ON COLUMN billing.subscription_mrr.recorded_risk IS 'The reasons its open risk episodes record.';
COMMENT ON COLUMN billing.subscription_mrr.risk_changed_at IS 'The business instant of the latest change of its reasons.';

CREATE TABLE billing.mrr_risk_cases (
    merchant_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT uuidv7(),
    subscription_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    entry_reason text NOT NULL CHECK (entry_reason IN ('past_due', 'needs_payment_method', 'unverified', 'card_expiring', 'delinquent')),
    opened_at timestamptz NOT NULL,
    mrr_at_entry bigint NOT NULL CHECK (mrr_at_entry > 0),
    closed_at timestamptz,
    outcome text CHECK (outcome IN ('recovered', 'churned', 'superseded')),
    mrr_at_exit bigint CHECK (mrr_at_exit >= 0),
    voluntary boolean,
    backfilled boolean NOT NULL,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT mrr_risk_cases_subscription_id_fkey FOREIGN KEY (merchant_id, customer_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, customer_id, id) ON DELETE RESTRICT,
    CONSTRAINT mrr_risk_cases_closed_check CHECK ((closed_at IS NULL) = (outcome IS NULL) AND (closed_at IS NULL) = (mrr_at_exit IS NULL) AND (closed_at IS NULL OR closed_at >= opened_at)),
    CONSTRAINT mrr_risk_cases_voluntary_check CHECK ((voluntary IS NOT NULL) = COALESCE(outcome = 'churned', false))
);
COMMENT ON TABLE billing.mrr_risk_cases IS 'One stretch of a subscription''s MRR at risk, from its first reason to its last: opened by its entry_reason, chaining reasons (mrr_risk_episodes), and closed recovered (healthy again while adding MRR), churned (its MRR ended while the case was open; voluntary for a customer''s or merchant''s cancel) or superseded (a prune, an upgrade or a change of currency). A cancel at period end keeps the case open until the MRR ends; a resume continues it. The recovery rate is recovered MRR over recovered and churned. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.mrr_risk_cases.mrr_at_exit IS 'The MRR recovered, or the MRR at risk when it churned.';
COMMENT ON COLUMN billing.mrr_risk_cases.backfilled IS 'Opened from current state by a backfill: opened_at is when its delinquency or unverified state began where the book records it, else the backfill.';

CREATE UNIQUE INDEX mrr_risk_cases_subscription_id_key ON billing.mrr_risk_cases (merchant_id, subscription_id)
    WHERE closed_at IS NULL;
CREATE INDEX mrr_risk_cases_closed_at_idx ON billing.mrr_risk_cases (merchant_id, currency, closed_at)
    WHERE closed_at IS NOT NULL;

CREATE TRIGGER immutable_mrr_risk_cases_delete BEFORE DELETE ON billing.mrr_risk_cases
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();

CREATE TABLE billing.mrr_risk_episodes (
    merchant_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT uuidv7(),
    case_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    reason text NOT NULL CHECK (reason IN ('past_due', 'needs_payment_method', 'unverified', 'card_expiring', 'delinquent')),
    entered_at timestamptz NOT NULL,
    mrr_at_entry bigint NOT NULL CHECK (mrr_at_entry > 0),
    overdue_amount bigint CHECK (overdue_amount >= 0),
    left_at timestamptz,
    mrr_at_exit bigint CHECK (mrr_at_exit >= 0),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT mrr_risk_episodes_case_id_fkey FOREIGN KEY (merchant_id, case_id) REFERENCES billing.mrr_risk_cases (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT mrr_risk_episodes_left_check CHECK ((left_at IS NULL) = (mrr_at_exit IS NULL) AND (left_at IS NULL OR left_at >= entered_at)),
    CONSTRAINT mrr_risk_episodes_overdue_check CHECK (overdue_amount IS NULL OR reason = 'delinquent')
);
COMMENT ON TABLE billing.mrr_risk_episodes IS 'One reason a subscription''s MRR was at risk, from when it entered to when it left, within its case. A delinquent one keeps the customer''s overdue amount in the currency when it entered. Retention: permanent, never pruned.';

CREATE UNIQUE INDEX mrr_risk_episodes_subscription_id_reason_key ON billing.mrr_risk_episodes (merchant_id, subscription_id, reason)
    WHERE left_at IS NULL;
CREATE INDEX mrr_risk_episodes_case_id_idx ON billing.mrr_risk_episodes (merchant_id, case_id);

CREATE TRIGGER immutable_mrr_risk_episodes_delete BEFORE DELETE ON billing.mrr_risk_episodes
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();

CREATE TABLE billing.mrr_risk_flows (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    bucket_at timestamptz NOT NULL,
    reason text NOT NULL CHECK (reason IN ('past_due', 'needs_payment_method', 'unverified', 'card_expiring', 'delinquent', 'any')),
    amount bigint NOT NULL,
    entered bigint NOT NULL CHECK (entered >= 0),
    left_count bigint NOT NULL CHECK (left_count >= 0),
    recovered_mrr bigint NOT NULL CHECK (recovered_mrr >= 0),
    recovered bigint NOT NULL CHECK (recovered >= 0),
    churned_mrr bigint NOT NULL CHECK (churned_mrr >= 0),
    churned bigint NOT NULL CHECK (churned >= 0),
    PRIMARY KEY (merchant_id, currency, bucket_at, reason)
);
COMMENT ON TABLE billing.mrr_risk_flows IS 'At-risk MRR flows per currency, 15-minute UTC bucket (billing.mrr_bucket) and reason: the change of at-risk MRR, the subscriptions entering and leaving it, and the cases closed recovered or churned, by their entry reason. The reason any counts each subscription once whatever its reasons, and carries no closed cases. At-risk MRR at an instant is mrr_risk_totals less the amounts of the buckets from it on.';

CREATE TABLE billing.mrr_risk_totals (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    reason text NOT NULL CHECK (reason IN ('past_due', 'needs_payment_method', 'unverified', 'card_expiring', 'delinquent', 'any')),
    mrr bigint NOT NULL CHECK (mrr >= 0),
    subscriptions bigint NOT NULL CHECK (subscriptions >= 0),
    PRIMARY KEY (merchant_id, currency, reason),
    CONSTRAINT mrr_risk_totals_count_check CHECK ((mrr > 0) = (subscriptions > 0))
);
COMMENT ON TABLE billing.mrr_risk_totals IS 'The merchant''s MRR at risk now per currency and reason, and how many subscriptions it is; any counts each subscription once.';

CREATE TABLE billing.mrr_risk_due (
    merchant_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT uuidv7(),
    customer_id uuid NOT NULL,
    changed_at timestamptz NOT NULL,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT mrr_risk_due_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE CASCADE
);
COMMENT ON TABLE billing.mrr_risk_due IS 'Changes outside a subscription row that may change the risk of its customer''s subscriptions: a card, a default card, a provider''s decline or delinquency. Each change appends its own row, so no writer waits on another''s; the MRR risk worker claims rows no other worker holds, deletes them and re-evaluates each customer''s subscriptions, locking them in id order. changed_at dates the change. Retention: state, deleted when claimed.';

-- Each change wakes the MRR risk worker at its commit; the payload is the
-- schema.
CREATE FUNCTION billing.notify_mrr_risk_due() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    PERFORM pg_notify('openrails_mrr_risk', TG_TABLE_SCHEMA);
    RETURN NULL;
END;
$$;

CREATE TRIGGER mrr_risk_due_notify AFTER INSERT ON billing.mrr_risk_due
    FOR EACH STATEMENT EXECUTE FUNCTION billing.notify_mrr_risk_due();

-- Why a subscription adding p_level is at risk, in the order the reasons are
-- listed; p_prev is its reasons until now. The caller holds the subscription
-- row.
CREATE FUNCTION billing.subscription_risk(s billing.subscriptions, p_level bigint, p_prev text[]) RETURNS text[]
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE
    v_currency text;
    v_fix boolean;
    v_card uuid;
    v_expires timestamptz;
BEGIN
    IF p_level = 0 OR s.deleted_at IS NOT NULL THEN
        RETURN '{}';
    END IF;
    -- Canceled at its period end, it adds MRR until then: its case stays open
    -- until the MRR ends, or a resume continues it.
    IF s.status = 'canceled' THEN
        RETURN p_prev;
    END IF;
    SELECT currency INTO v_currency FROM billing.prices WHERE merchant_id = s.merchant_id AND id = s.price_id;
    -- A provider retries on its own: its latest decline's action says
    -- whether the card must be replaced.
    IF s.status = 'past_due' AND s.collection_policy <> 'engine' THEN
        SELECT a.action = 'fix_payment_method' INTO v_fix FROM billing.payment_attempts a
         WHERE a.merchant_id = s.merchant_id AND a.subscription_id = s.id
         ORDER BY a.attempted_at DESC, a.id DESC LIMIT 1;
    END IF;
    -- The card OpenRails charges is good through the end of its month. An NMI
    -- schedule charges the card NMI holds for it: only one the subscription
    -- names is known to be that card.
    v_card := CASE s.collection_policy
        WHEN 'engine' THEN billing.subscription_payment_method_id(s.merchant_id, s.customer_id, s.payment_method_id, s.price_id, s.rail, s.collection_policy)
        WHEN 'nmi_schedule' THEN s.payment_method_id END;
    IF v_card IS NOT NULL AND s.current_period_ends_at IS NOT NULL THEN
        SELECT (make_date(pm.card_exp_year, pm.card_exp_month, 1) + interval '1 month') AT TIME ZONE 'UTC' INTO v_expires
          FROM billing.payment_methods pm
         WHERE pm.merchant_id = s.merchant_id AND pm.id = v_card;
    END IF;
    RETURN array_remove(ARRAY[
        CASE WHEN s.status = 'past_due' AND NOT COALESCE(v_fix, false) THEN 'past_due' END,
        CASE WHEN s.status = 'awaiting_method' OR COALESCE(v_fix, false) THEN 'needs_payment_method' END,
        CASE WHEN s.status = 'unverified' THEN 'unverified' END,
        CASE WHEN v_expires <= s.current_period_ends_at THEN 'card_expiring' END,
        CASE WHEN EXISTS (SELECT 1 FROM billing.customer_delinquency d WHERE d.merchant_id = s.merchant_id AND d.customer_id = s.customer_id
                            AND d.currency = v_currency AND d.state = 'delinquent') THEN 'delinquent' END
    ]::text[], NULL);
END;
$$;
COMMENT ON FUNCTION billing.subscription_risk(s billing.subscriptions, p_level bigint, p_prev text[]) IS 'The reasons a subscription adding MRR is at risk: its status (a provider''s past due needs a payment method when its latest decline''s action says so), the card it charges expiring by its period end (on an NMI schedule, only a card it names), and its customer''s delinquency in its currency. Canceled at its period end, it keeps p_prev.';

-- When a backfilled reason began, where the book records it: the customer's
-- delinquency or the subscription's unverified state; else p_at.
CREATE FUNCTION billing.mrr_risk_began(p_merchant uuid, p_customer uuid, p_subscription uuid, p_currency text, p_reason text, p_at timestamptz) RETURNS timestamptz
LANGUAGE sql STABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
SELECT least(p_at, COALESCE(CASE p_reason
    WHEN 'delinquent' THEN (SELECT d.entered_at FROM billing.customer_delinquency d
                             WHERE d.merchant_id = p_merchant AND d.customer_id = p_customer AND d.currency = p_currency AND d.state = 'delinquent')
    WHEN 'unverified' THEN (SELECT v.unverified_at FROM billing.subscription_verifications v
                             WHERE v.merchant_id = p_merchant AND v.subscription_id = p_subscription) END, p_at))
$$;
COMMENT ON FUNCTION billing.mrr_risk_began(p_merchant uuid, p_customer uuid, p_subscription uuid, p_currency text, p_reason text, p_at timestamptz) IS 'When a backfilled risk reason began: customer_delinquency.entered_at for delinquent, subscription_verifications.unverified_at for unverified, else p_at, never after p_at.';

-- One subscription's MRR at p_at, the change p_cause explains, its risk and
-- its scheduled churn. Its writer holds the subscription row.
CREATE OR REPLACE FUNCTION billing.refresh_subscription_mrr(s billing.subscriptions, p_at timestamptz, p_cause text) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
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
    SELECT * INTO v_prev FROM billing.subscription_mrr WHERE merchant_id = s.merchant_id AND subscription_id = s.id;
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
         WHERE merchant_id = s.merchant_id AND subscription_id = s.id;
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
COMMENT ON FUNCTION billing.refresh_subscription_mrr(s billing.subscriptions, p_at timestamptz, p_cause text) IS 'Recomputes one subscription''s subscription_mrr at p_at, its risk, and its scheduled churn, contraction or expansion. A changed level or risk makes the row dirty; billing.settle_mrr records it at commit.';

-- Records every dirty subscription_mrr row of the merchant: one movement per
-- customer and currency whose MRR changed, its lines and rollups, each
-- subscription's risk case and episodes and their rollups, then marks the
-- rows recorded. Only this transaction's rows are dirty. Every customer
-- row is locked before any rollup row, and each table's rows in key order,
-- so concurrent settlements never wait on each other in a cycle.
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
    -- The movements by kind; a voluntary churn also counts apart.
    INSERT INTO billing.mrr_flows AS f (merchant_id, currency, bucket_at, measure, amount, entries, logos)
    SELECT p_merchant, v.currency, billing.mrr_bucket(v.occurred_at), x.measure, sum(v.delta), count(*), sum(v.logo_delta)
      FROM billing.mrr_movements v
      CROSS JOIN LATERAL (SELECT v.kind UNION ALL SELECT 'churn_voluntary' WHERE v.voluntary) x(measure)
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
COMMENT ON FUNCTION billing.settle_mrr(p_merchant uuid) IS 'Records the merchant''s dirty subscription_mrr rows: per customer and currency one mrr_movements row classified by the customer''s MRR before and after, its lines, and the rollups; then each subscription''s risk case and episodes, the at-risk flows and totals.';

CREATE OR REPLACE FUNCTION billing.subscriptions_track_mrr() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.price_id IS NULL OR TG_OP = 'UPDATE' AND ROW(OLD.status, OLD.price_id, OLD.product_id, OLD.quantity, OLD.ended_at,
            OLD.current_period_starts_at, OLD.current_period_ends_at, OLD.deleted_at, OLD.cancel_type, OLD.payment_method_id)
        IS NOT DISTINCT FROM ROW(NEW.status, NEW.price_id, NEW.product_id, NEW.quantity, NEW.ended_at,
            NEW.current_period_starts_at, NEW.current_period_ends_at, NEW.deleted_at, NEW.cancel_type, NEW.payment_method_id) THEN
        RETURN NULL;
    END IF;
    IF billing.billing_restore_active(NEW.merchant_id) THEN
        RETURN NULL;
    END IF;
    PERFORM billing.refresh_subscription_mrr(NEW, NEW.updated_at, CASE
        WHEN current_setting('openrails.mrr_cause', true) IN ('import', 'undo') THEN current_setting('openrails.mrr_cause', true)
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

-- The card a subscription charges is part of its risk.
DROP TRIGGER trg_subscriptions_track_mrr ON billing.subscriptions;
CREATE TRIGGER trg_subscriptions_track_mrr AFTER INSERT OR UPDATE OF status, price_id, product_id, quantity, ended_at, current_period_starts_at, current_period_ends_at, deleted_at, cancel_type, payment_method_id ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_track_mrr();

-- A change outside the subscription row appends its customer for the MRR
-- risk worker instead of touching subscription_mrr: the worker locks each
-- subscription before it re-evaluates, in the order its writers take them.
-- Only a customer with a subscription adding MRR can be at risk.
CREATE FUNCTION billing.queue_mrr_risk(p_merchant uuid, p_customer uuid, p_at timestamptz) RETURNS void
LANGUAGE sql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
INSERT INTO billing.mrr_risk_due (merchant_id, customer_id, changed_at)
SELECT p_merchant, p_customer, p_at
 WHERE EXISTS (SELECT 1 FROM billing.subscription_mrr m WHERE m.merchant_id = p_merchant AND m.customer_id = p_customer AND m.mrr > 0)
$$;

CREATE FUNCTION billing.payment_methods_track_mrr_risk() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF ROW(OLD.card_exp_month, OLD.card_exp_year, OLD.status, OLD.replaced_by_id) IS DISTINCT FROM ROW(NEW.card_exp_month, NEW.card_exp_year, NEW.status, NEW.replaced_by_id)
       AND NOT billing.billing_restore_active(NEW.merchant_id) THEN
        PERFORM billing.queue_mrr_risk(NEW.merchant_id, NEW.customer_id, NEW.updated_at);
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER payment_methods_track_mrr_risk AFTER UPDATE OF card_exp_month, card_exp_year, status, replaced_by_id ON billing.payment_methods
    FOR EACH ROW EXECUTE FUNCTION billing.payment_methods_track_mrr_risk();

CREATE FUNCTION billing.money_settings_track_mrr_risk() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.default_payment_method_id IS DISTINCT FROM (CASE WHEN TG_OP = 'UPDATE' THEN OLD.default_payment_method_id END)
       AND NOT billing.billing_restore_active(NEW.merchant_id) THEN
        PERFORM billing.queue_mrr_risk(NEW.merchant_id, NEW.customer_id, NEW.updated_at);
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER money_settings_track_mrr_risk AFTER INSERT OR UPDATE OF default_payment_method_id ON billing.money_settings
    FOR EACH ROW EXECUTE FUNCTION billing.money_settings_track_mrr_risk();

CREATE FUNCTION billing.customer_delinquency_track_mrr_risk() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF (NEW.state = 'delinquent') IS DISTINCT FROM (TG_OP = 'UPDATE' AND OLD.state = 'delinquent')
       AND NOT billing.billing_restore_active(NEW.merchant_id) THEN
        PERFORM billing.queue_mrr_risk(NEW.merchant_id, NEW.customer_id, NEW.entered_at);
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER customer_delinquency_track_mrr_risk AFTER INSERT OR UPDATE OF state ON billing.customer_delinquency
    FOR EACH ROW EXECUTE FUNCTION billing.customer_delinquency_track_mrr_risk();

-- A provider's decline may say its subscription needs a payment method.
CREATE FUNCTION billing.payment_attempts_track_mrr_risk() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NOT billing.billing_restore_active(NEW.merchant_id) THEN
        PERFORM billing.queue_mrr_risk(NEW.merchant_id, NEW.customer_id, NEW.attempted_at);
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER payment_attempts_track_mrr_risk AFTER INSERT ON billing.payment_attempts
    FOR EACH ROW WHEN (NEW.subscription_id IS NOT NULL AND NEW.action IS NOT NULL AND NEW.owner IN ('provider', 'nmi_schedule'))
    EXECUTE FUNCTION billing.payment_attempts_track_mrr_risk();

-- The MRR risk worker: claims up to p_batch of the merchant's queued changes
-- that no other worker holds, then re-evaluates each customer's subscriptions,
-- locked in id order, dated at its latest claimed change. A change committed
-- after the claim stays queued for the next. Returns how many it claimed.
CREATE FUNCTION billing.effect_due_mrr_risk(p_merchant uuid, p_batch integer) RETURNS integer
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE
    v_due record;
    v_sub billing.subscriptions;
    v_risk text[];
    v_claimed integer := 0;
BEGIN
    FOR v_due IN
        WITH claimed AS (
            DELETE FROM billing.mrr_risk_due d
             USING (SELECT q.id FROM billing.mrr_risk_due q WHERE q.merchant_id = p_merchant ORDER BY q.id LIMIT p_batch FOR UPDATE SKIP LOCKED) c
             WHERE d.merchant_id = p_merchant AND d.id = c.id
            RETURNING d.customer_id, d.changed_at)
        SELECT customer_id, max(changed_at) AS changed_at, count(*)::integer AS changes FROM claimed GROUP BY customer_id ORDER BY customer_id
    LOOP
        v_claimed := v_claimed + v_due.changes;
        FOR v_sub IN
            SELECT s.* FROM billing.subscriptions s
             WHERE s.merchant_id = p_merchant AND s.customer_id = v_due.customer_id
               AND EXISTS (SELECT 1 FROM billing.subscription_mrr m WHERE m.merchant_id = s.merchant_id AND m.subscription_id = s.id)
             ORDER BY s.id
               FOR UPDATE
        LOOP
            SELECT billing.subscription_risk(v_sub, m.mrr, m.risk) INTO v_risk FROM billing.subscription_mrr m
             WHERE m.merchant_id = p_merchant AND m.subscription_id = v_sub.id;
            UPDATE billing.subscription_mrr SET risk = v_risk, risk_changed_at = greatest(v_due.changed_at, risk_changed_at)
             WHERE merchant_id = p_merchant AND subscription_id = v_sub.id AND risk <> v_risk;
        END LOOP;
    END LOOP;
    RETURN v_claimed;
END;
$$;
COMMENT ON FUNCTION billing.effect_due_mrr_risk(p_merchant uuid, p_batch integer) IS 'Claims up to p_batch of the merchant''s mrr_risk_due rows no other transaction holds (FOR UPDATE SKIP LOCKED), deletes them and re-evaluates each claimed customer''s subscriptions'' risk, locking them in id order. Returns the rows claimed.';

-- A subscription with no subscription_mrr row (one written before MRR, or a
-- restore from an archive without it) is adopted as an opening movement, and
-- its risk as a backfilled case.
CREATE OR REPLACE FUNCTION billing.seed_mrr(p_merchant uuid, p_at timestamptz) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE
    v_sub billing.subscriptions;
BEGIN
    PERFORM set_config('openrails.mrr_cause', 'backfill', true);
    FOR v_sub IN
        SELECT s.* FROM billing.subscriptions s
         WHERE s.merchant_id = p_merchant AND s.price_id IS NOT NULL
           AND NOT EXISTS (SELECT 1 FROM billing.subscription_mrr m WHERE m.merchant_id = s.merchant_id AND m.subscription_id = s.id)
         ORDER BY s.id
    LOOP
        PERFORM billing.refresh_subscription_mrr(v_sub, p_at, 'backfill');
        PERFORM billing.schedule_mrr_change(p_merchant, v_sub.id, p_at);
    END LOOP;
    PERFORM billing.settle_mrr(p_merchant);
    PERFORM set_config('openrails.mrr_cause', '', true);
END;
$$;

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
           WHERE COALESCE(t.mrr, 0) <> COALESCE(s.mrr, 0) OR COALESCE(t.subscriptions, 0) <> COALESCE(s.subscriptions, 0)) THEN
        RAISE EXCEPTION 'billing restore MRR integrity mismatch' USING ERRCODE='23514';
    END IF;
END;
$$;
REVOKE ALL ON FUNCTION billing.check_billing_restore_mrr(p_merchant uuid) FROM PUBLIC;

-- The risk of every subscription now, opened as backfilled cases dated when
-- the book says each reason began.
SELECT set_config('openrails.mrr_cause', 'backfill', true);
UPDATE billing.subscription_mrr m SET risk = r.risk, risk_changed_at = now()
  FROM (SELECT s.merchant_id, s.id, billing.subscription_risk(s, x.mrr, '{}') AS risk
          FROM billing.subscriptions s
          JOIN billing.subscription_mrr x ON x.merchant_id = s.merchant_id AND x.subscription_id = s.id
         WHERE x.mrr > 0) r
 WHERE m.merchant_id = r.merchant_id AND m.subscription_id = r.id AND r.risk <> '{}';
SELECT billing.settle_mrr(id) FROM billing.merchants ORDER BY id;
SELECT set_config('openrails.mrr_cause', '', true);

-- The restore guard names the risk tables.
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
                  'failed_usage_windows',
                  'fraud_reports',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'lease_in_flight',
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
                  'mrr_cohort_flows',
                  'mrr_flows',
                  'mrr_movement_lines',
                  'mrr_movements',
                  'mrr_product_flows',
                  'mrr_risk_cases',
                  'mrr_risk_due',
                  'mrr_risk_episodes',
                  'mrr_risk_flows',
                  'mrr_risk_totals',
                  'mrr_scheduled',
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
                  'running_charges',
                  'scheduled_changes',
                  'solana_pay_receipts',
                  'solana_pay_references',
                  'solana_subscriptions',
                  'stored_card_checks',
                  'subscription_billing_changes',
                  'subscription_mrr',
                  'subscription_status_transitions',
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
