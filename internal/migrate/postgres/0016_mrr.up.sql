-- MRR: what each subscription adds to monthly recurring revenue, the
-- movements its changes make at the customer level, scheduled churn and
-- contraction, and the rollups reports read. Triggers keep it for every
-- writer; nothing sweeps subscriptions.

-- Rollups are 15-minute UTC buckets: every UTC offset is a whole number of
-- quarter hours, so a read groups them into any zone's days and months with
-- AT TIME ZONE, and a change of the merchant's zone needs no rebuild.
CREATE FUNCTION billing.mrr_bucket(p_at timestamptz) RETURNS timestamptz
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT date_bin('15 minutes', p_at, '2000-01-01 00:00:00+00'::timestamptz)
$$;
COMMENT ON FUNCTION billing.mrr_bucket(p_at timestamptz) IS 'The start of the 15-minute UTC bucket of p_at: the key of the MRR rollups.';

-- MRR per seat: a calendar-month cadence, or a named one (monthly 720,
-- quarterly 2160, half-yearly 4320, yearly 8760 hours), divides by its
-- months; weekly (168 hours) counts 52 weeks over 12 months; any other window
-- scales by 730.485 hours, the average Gregorian month. Rounded once. A
-- provider price's cadence is the one its provider bills. A level beyond 10^15
-- native units (a billion dollars a month) is held there with a warning, so
-- no product overflows and sums stay in bigint below nine thousand such
-- levels.
CREATE FUNCTION billing.price_mrr(p billing.prices, p_seats integer) RETURNS bigint
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
DECLARE
    v_months integer := CASE WHEN p.provider_snapshot ? 'provider_billing_interval_months'
        THEN substring(p.provider_snapshot->>'provider_billing_interval_months' FROM '^[0-9]{1,4}$')::integer
        ELSE p.billing_interval_months END;
    v_hours integer := CASE WHEN p.provider_snapshot ? 'provider_billing_interval_hours'
        THEN substring(p.provider_snapshot->>'provider_billing_interval_hours' FROM '^[0-9]{1,9}$')::integer
        ELSE p.billing_interval_hours END;
    v_level numeric;
BEGIN
    IF COALESCE(v_months, 0) <= 0 THEN
        v_months := CASE v_hours WHEN 720 THEN 1 WHEN 2160 THEN 3 WHEN 4320 THEN 6 WHEN 8760 THEN 12 END;
    END IF;
    v_level := COALESCE(CASE
        WHEN v_months > 0 THEN round(p.amount::numeric / v_months)
        WHEN v_hours = 168 THEN round(p.amount::numeric * 52 / 12)
        WHEN v_hours > 0 THEN round(p.amount::numeric * 730.485 / v_hours)
    END, 0) * greatest(p_seats, 1);
    IF v_level > 1000000000000000 THEN
        RAISE WARNING 'MRR % of price % for % seats is held at 10^15', v_level, p.id, greatest(p_seats, 1);
        RETURN 1000000000000000;
    END IF;
    RETURN v_level;
END;
$$;
COMMENT ON FUNCTION billing.price_mrr(p billing.prices, p_seats integer) IS 'MRR of p_seats seats of a price: per seat its list amount divided by its calendar or named months (720, 2160, 4320, 8760 hours), times 52/12 weekly, or times 730.485 over other hours, rounded, then times the seats, held at 10^15 with a warning; 0 for a one-time price.';

CREATE FUNCTION billing.mrr_cancel_cause(p_cancel_type text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT CASE p_cancel_type
    WHEN 'user' THEN 'canceled'
    WHEN 'merchant' THEN 'merchant_canceled'
    WHEN 'chargeback' THEN 'chargeback'
    WHEN 'upgrade' THEN 'upgrade'
    ELSE 'expired'
END
$$;
COMMENT ON FUNCTION billing.mrr_cancel_cause(p_cancel_type text) IS 'The MRR cause of a cancellation by its cancel_type: expired covers dunning, a non-recoverable decline, a provider''s end and an abandoned first payment.';

CREATE TABLE billing.subscription_mrr (
    merchant_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    product_id uuid NOT NULL,
    price_id uuid NOT NULL,
    mrr bigint NOT NULL CHECK (mrr >= 0),
    contributes boolean NOT NULL,
    ends_at timestamptz,
    next_change_at timestamptz,
    changed_at timestamptz NOT NULL,
    cause text NOT NULL CHECK (cause IN ('subscribed', 'resumed', 'renewed', 'price_change', 'seat_change', 'canceled', 'merchant_canceled', 'expired', 'chargeback', 'upgrade', 'import', 'prune', 'undo', 'backfill')),
    recorded_currency text NOT NULL CHECK (recorded_currency ~ '^[A-Z0-9]{3,12}$'),
    recorded_product_id uuid NOT NULL,
    recorded_price_id uuid NOT NULL,
    recorded_mrr bigint NOT NULL CHECK (recorded_mrr >= 0),
    dirty boolean NOT NULL GENERATED ALWAYS AS (mrr <> recorded_mrr OR currency <> recorded_currency OR product_id <> recorded_product_id OR price_id <> recorded_price_id) STORED,
    PRIMARY KEY (merchant_id, subscription_id),
    CONSTRAINT subscription_mrr_subscription_id_fkey FOREIGN KEY (merchant_id, customer_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, customer_id, id) ON DELETE RESTRICT,
    CONSTRAINT subscription_mrr_contributes_check CHECK (contributes OR (mrr = 0 AND ends_at IS NULL))
);
COMMENT ON TABLE billing.subscription_mrr IS 'What each subscription adds to MRR now, kept by trg_subscriptions_track_mrr for every writer: its price''s MRR for its seats (billing.price_mrr), while it is live or canceled with its end ahead; a pending subscription, a trial and an ended one add 0. The recorded_ columns are what the customer''s MRR last counted; a row that differs (dirty) is settled into a movement at commit.';
COMMENT ON COLUMN billing.subscription_mrr.contributes IS 'The subscription counts toward MRR: live, or canceled with its end ahead, and past any trial.';
COMMENT ON COLUMN billing.subscription_mrr.ends_at IS 'When a cancellation at period end takes the subscription out of MRR; NULL when none is scheduled.';
COMMENT ON COLUMN billing.subscription_mrr.next_change_at IS 'When time alone next changes the row; openrails.mrr_due takes it then by this column''s index.';
COMMENT ON COLUMN billing.subscription_mrr.changed_at IS 'The business instant of the change not yet recorded, or of the last one recorded: the writer''s updated_at, or the end a due change took effect at.';
COMMENT ON COLUMN billing.subscription_mrr.cause IS 'Why the row last changed; the movement it settles into takes the cause of its largest change.';

CREATE INDEX subscription_mrr_customer_id_idx ON billing.subscription_mrr (merchant_id, customer_id);
CREATE INDEX subscription_mrr_dirty_idx ON billing.subscription_mrr (merchant_id, customer_id)
    WHERE dirty;
CREATE INDEX subscription_mrr_next_change_at_idx ON billing.subscription_mrr (next_change_at, merchant_id, subscription_id)
    WHERE next_change_at IS NOT NULL;

CREATE TABLE billing.customer_mrr (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    mrr bigint NOT NULL CHECK (mrr >= 0),
    subscriptions integer NOT NULL CHECK (subscriptions >= 0),
    first_active_at timestamptz,
    PRIMARY KEY (merchant_id, customer_id, currency),
    CONSTRAINT customer_mrr_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT customer_mrr_count_check CHECK ((mrr > 0) = (subscriptions > 0)),
    CONSTRAINT customer_mrr_active_check CHECK (mrr = 0 OR first_active_at IS NOT NULL)
);
COMMENT ON TABLE billing.customer_mrr IS 'Each customer''s MRR in each currency, as its movements left it: the sum of its subscriptions'' recorded MRR, how many add to it, and when it first had any (its cohort).';
COMMENT ON COLUMN billing.customer_mrr.first_active_at IS 'When the customer first had MRR in the currency: its cohort.';

CREATE TABLE billing.mrr_movements (
    merchant_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT uuidv7(),
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    kind text NOT NULL CHECK (kind IN ('new', 'expansion', 'contraction', 'churn', 'reactivation', 'opening', 'correction')),
    delta bigint NOT NULL CHECK (delta <> 0),
    mrr_before bigint NOT NULL CHECK (mrr_before >= 0),
    mrr_after bigint NOT NULL CHECK (mrr_after >= 0),
    logo_delta integer NOT NULL CHECK (abs(logo_delta) <= 1),
    cohort_at timestamptz NOT NULL,
    cause text NOT NULL CHECK (cause IN ('subscribed', 'resumed', 'renewed', 'price_change', 'seat_change', 'canceled', 'merchant_canceled', 'expired', 'chargeback', 'upgrade', 'import', 'prune', 'undo', 'backfill')),
    voluntary boolean,
    backfilled boolean NOT NULL,
    occurred_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT mrr_movements_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT mrr_movements_amount_check CHECK (mrr_after = mrr_before + delta),
    CONSTRAINT mrr_movements_voluntary_check CHECK ((voluntary IS NOT NULL) = (kind = 'churn'))
);
COMMENT ON TABLE billing.mrr_movements IS 'One change of a customer''s MRR in one currency, written once at the commit of the transaction that made it: new (first MRR), reactivation (MRR again after none), expansion, contraction, churn (to none; voluntary for a customer''s or merchant''s cancel), opening (an import or backfill adopting subscriptions) or correction (a prune or an undo). Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.mrr_movements.logo_delta IS '1 when the customer gains MRR from none, -1 when it loses all, 0 otherwise.';
COMMENT ON COLUMN billing.mrr_movements.occurred_at IS 'The business instant of the change: its writer''s, or the end a scheduled churn took effect at.';
COMMENT ON COLUMN billing.mrr_movements.cohort_at IS 'The customer''s first_active_at in the currency: its cohort.';
COMMENT ON COLUMN billing.mrr_movements.backfilled IS 'Written by a backfill from current state, not when the change happened.';

CREATE INDEX mrr_movements_customer_id_idx ON billing.mrr_movements (merchant_id, customer_id, currency, occurred_at);
CREATE INDEX mrr_movements_occurred_at_idx ON billing.mrr_movements (merchant_id, currency, occurred_at);

CREATE TRIGGER immutable_mrr_movements BEFORE UPDATE OR DELETE ON billing.mrr_movements
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER immutable_mrr_movements_truncate BEFORE TRUNCATE ON billing.mrr_movements
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();

CREATE TABLE billing.mrr_movement_lines (
    merchant_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT uuidv7(),
    movement_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    product_id uuid NOT NULL,
    price_id uuid NOT NULL,
    delta bigint NOT NULL CHECK (delta <> 0),
    mrr_before bigint NOT NULL CHECK (mrr_before >= 0),
    mrr_after bigint NOT NULL CHECK (mrr_after >= 0),
    subscriber_delta integer NOT NULL CHECK (abs(subscriber_delta) <= 1),
    cause text NOT NULL CHECK (cause IN ('subscribed', 'resumed', 'renewed', 'price_change', 'seat_change', 'canceled', 'merchant_canceled', 'expired', 'chargeback', 'upgrade', 'import', 'prune', 'undo', 'backfill')),
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT mrr_movement_lines_movement_id_fkey FOREIGN KEY (merchant_id, movement_id) REFERENCES billing.mrr_movements (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT mrr_movement_lines_subscription_id_fkey FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT mrr_movement_lines_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT mrr_movement_lines_price_id_fkey FOREIGN KEY (merchant_id, price_id) REFERENCES billing.prices (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT mrr_movement_lines_amount_check CHECK (mrr_after = mrr_before + delta)
);
COMMENT ON TABLE billing.mrr_movement_lines IS 'Each subscription''s part of a movement: its product and price, its change of MRR and whether it began or stopped adding to it. Retention: permanent, never pruned.';

CREATE INDEX mrr_movement_lines_movement_id_idx ON billing.mrr_movement_lines (merchant_id, movement_id);
CREATE INDEX mrr_movement_lines_subscription_id_idx ON billing.mrr_movement_lines (merchant_id, subscription_id);

CREATE TRIGGER immutable_mrr_movement_lines BEFORE UPDATE OR DELETE ON billing.mrr_movement_lines
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER immutable_mrr_movement_lines_truncate BEFORE TRUNCATE ON billing.mrr_movement_lines
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();

CREATE TABLE billing.mrr_scheduled (
    merchant_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT uuidv7(),
    subscription_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    kind text NOT NULL CHECK (kind IN ('churn', 'contraction', 'expansion')),
    mrr_delta bigint NOT NULL CHECK (mrr_delta <> 0),
    effective_at timestamptz NOT NULL,
    cause text NOT NULL CHECK (cause IN ('subscribed', 'resumed', 'renewed', 'price_change', 'seat_change', 'canceled', 'merchant_canceled', 'expired', 'chargeback', 'upgrade', 'import', 'prune', 'undo', 'backfill')),
    scheduled_change_id uuid,
    status text NOT NULL CHECK (status IN ('scheduled', 'reversed', 'effected', 'superseded')),
    scheduled_at timestamptz NOT NULL,
    resolved_at timestamptz,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT mrr_scheduled_subscription_id_fkey FOREIGN KEY (merchant_id, customer_id, subscription_id) REFERENCES billing.subscriptions (merchant_id, customer_id, id) ON DELETE RESTRICT,
    CONSTRAINT mrr_scheduled_scheduled_change_id_fkey FOREIGN KEY (merchant_id, scheduled_change_id) REFERENCES billing.scheduled_changes (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT mrr_scheduled_change_check CHECK ((kind = 'churn') = (scheduled_change_id IS NULL)),
    CONSTRAINT mrr_scheduled_sign_check CHECK ((kind = 'expansion') = (mrr_delta > 0)),
    CONSTRAINT mrr_scheduled_resolved_check CHECK ((status = 'scheduled') = (resolved_at IS NULL))
);
COMMENT ON TABLE billing.mrr_scheduled IS 'MRR a subscription will lose or gain at a date: churn from a cancellation at period end, contraction or expansion from a scheduled change at its renewal. scheduled until it is effected, reversed (a resume, a canceled change) or superseded (a later cancel or change). At most one is scheduled per subscription. A report counts what was added in a period by scheduled_at and what was reversed, effected or superseded by resolved_at. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.mrr_scheduled.mrr_delta IS 'The change of the subscription''s MRR it schedules: negative for churn and contraction.';
COMMENT ON COLUMN billing.mrr_scheduled.effective_at IS 'When it takes effect: the end a cancellation runs to, or the renewal on or after the scheduled change''s date.';

CREATE UNIQUE INDEX mrr_scheduled_subscription_id_key ON billing.mrr_scheduled (merchant_id, subscription_id)
    WHERE status = 'scheduled';
CREATE INDEX mrr_scheduled_effective_at_idx ON billing.mrr_scheduled (merchant_id, effective_at)
    WHERE status = 'scheduled';
CREATE INDEX mrr_scheduled_scheduled_at_idx ON billing.mrr_scheduled (merchant_id, currency, scheduled_at);
CREATE INDEX mrr_scheduled_resolved_at_idx ON billing.mrr_scheduled (merchant_id, currency, resolved_at)
    WHERE resolved_at IS NOT NULL;
CREATE INDEX mrr_scheduled_scheduled_change_id_idx ON billing.mrr_scheduled (merchant_id, scheduled_change_id)
    WHERE scheduled_change_id IS NOT NULL;

CREATE TRIGGER immutable_mrr_scheduled_delete BEFORE DELETE ON billing.mrr_scheduled
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();

CREATE TABLE billing.mrr_flows (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    bucket_at timestamptz NOT NULL,
    measure text NOT NULL CHECK (measure IN ('new', 'expansion', 'contraction', 'churn', 'reactivation', 'opening', 'correction', 'churn_voluntary')),
    amount bigint NOT NULL,
    entries bigint NOT NULL CHECK (entries >= 0),
    logos bigint NOT NULL,
    PRIMARY KEY (merchant_id, currency, bucket_at, measure)
);
COMMENT ON TABLE billing.mrr_flows IS 'MRR flows per currency and 15-minute UTC bucket (billing.mrr_bucket): movements by kind (and churn_voluntary, a part of churn) with their sum, count and logo change, written only at commit. A read groups the buckets into the merchant''s local days with AT TIME ZONE; MRR at an instant is mrr_totals less the movements of the buckets from it on.';

CREATE TABLE billing.mrr_totals (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    mrr bigint NOT NULL CHECK (mrr >= 0),
    customers bigint NOT NULL CHECK (customers >= 0),
    subscribers bigint NOT NULL CHECK (subscribers >= 0),
    PRIMARY KEY (merchant_id, currency)
);
COMMENT ON TABLE billing.mrr_totals IS 'The merchant''s MRR now per currency, the customers with any and the subscriptions adding to it: the sums of customer_mrr.';

CREATE TABLE billing.mrr_product_flows (
    merchant_id uuid NOT NULL,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    bucket_at timestamptz NOT NULL,
    product_id uuid NOT NULL,
    amount bigint NOT NULL,
    subscribers bigint NOT NULL,
    PRIMARY KEY (merchant_id, currency, bucket_at, product_id),
    CONSTRAINT mrr_product_flows_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products (merchant_id, id) ON DELETE RESTRICT
);
COMMENT ON TABLE billing.mrr_product_flows IS 'The change of every product''s MRR and subscribers per currency and 15-minute UTC bucket; a product''s MRR now is the sum of its buckets.';

CREATE TABLE billing.mrr_cohort_flows (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    currency text NOT NULL CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    cohort_at timestamptz NOT NULL,
    bucket_at timestamptz NOT NULL,
    amount bigint NOT NULL,
    logos bigint NOT NULL,
    PRIMARY KEY (merchant_id, currency, cohort_at, bucket_at)
);
COMMENT ON TABLE billing.mrr_cohort_flows IS 'Movements per currency of the customers who first had MRR in the 15-minute UTC bucket cohort_at, per 15-minute UTC bucket: a read groups both into the merchant''s local months, and a cohort''s MRR at a month''s end is the sum of its buckets through it.';

CREATE FUNCTION billing.resolve_mrr_scheduled(p_entry billing.mrr_scheduled, p_status text, p_at timestamptz) RETURNS void
LANGUAGE sql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
UPDATE billing.mrr_scheduled SET status = p_status, resolved_at = p_at
 WHERE merchant_id = p_entry.merchant_id AND id = p_entry.id AND status = 'scheduled'
$$;

-- A scheduled change of a contributing subscription not already ending is
-- its scheduled contraction or expansion.
CREATE FUNCTION billing.schedule_mrr_change(p_merchant uuid, p_subscription uuid, p_at timestamptz) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE
    v_change billing.scheduled_changes;
    v_state billing.subscription_mrr;
    v_sub billing.subscriptions;
    v_target billing.prices;
    v_delta bigint;
    v_kind text;
BEGIN
    SELECT * INTO v_change FROM billing.scheduled_changes
     WHERE merchant_id = p_merchant AND subscription_id = p_subscription AND status = 'scheduled';
    IF NOT FOUND THEN
        RETURN;
    END IF;
    SELECT * INTO v_state FROM billing.subscription_mrr WHERE merchant_id = p_merchant AND subscription_id = p_subscription;
    IF NOT FOUND OR v_state.mrr = 0 OR v_state.ends_at IS NOT NULL
       OR EXISTS (SELECT 1 FROM billing.mrr_scheduled e WHERE e.merchant_id = p_merchant AND e.subscription_id = p_subscription AND e.status = 'scheduled') THEN
        RETURN;
    END IF;
    SELECT * INTO v_sub FROM billing.subscriptions WHERE merchant_id = p_merchant AND id = p_subscription;
    SELECT * INTO v_target FROM billing.prices WHERE merchant_id = p_merchant AND id = v_change.price_id;
    IF v_target.currency IS DISTINCT FROM v_state.currency THEN
        RETURN;
    END IF;
    v_delta := billing.price_mrr(v_target, COALESCE(v_change.quantity, v_sub.quantity)) - v_state.mrr;
    IF v_delta = 0 THEN
        RETURN;
    END IF;
    v_kind := CASE WHEN v_delta < 0 THEN 'contraction' ELSE 'expansion' END;
    INSERT INTO billing.mrr_scheduled (merchant_id, subscription_id, customer_id, currency, kind, mrr_delta, effective_at, cause,
        scheduled_change_id, status, scheduled_at)
    VALUES (p_merchant, p_subscription, v_sub.customer_id, v_state.currency, v_kind, v_delta,
        -- The renewal on or after the change's date.
        CASE WHEN v_sub.current_period_ends_at >= v_change.effective_at THEN v_sub.current_period_ends_at ELSE v_change.effective_at END,
        CASE WHEN v_change.price_id = v_change.from_price_id THEN 'seat_change' ELSE 'price_change' END,
        v_change.id, 'scheduled', p_at);
END;
$$;

-- One subscription's MRR at p_at, the change p_cause explains, and its
-- scheduled churn. Its writer holds the subscription row.
CREATE FUNCTION billing.refresh_subscription_mrr(s billing.subscriptions, p_at timestamptz, p_cause text) RETURNS void
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
    IF v_prev.subscription_id IS NULL THEN
        INSERT INTO billing.subscription_mrr (merchant_id, subscription_id, customer_id, currency, product_id, price_id, mrr, contributes,
            ends_at, next_change_at, changed_at, cause, recorded_currency, recorded_product_id, recorded_price_id, recorded_mrr)
        VALUES (s.merchant_id, s.id, s.customer_id, v_price.currency, s.product_id, s.price_id, v_level, v_live,
            v_ends, v_ends, v_at, p_cause, v_price.currency, s.product_id, s.price_id, 0);
    ELSIF ROW(v_prev.currency, v_prev.product_id, v_prev.price_id, v_prev.mrr, v_prev.contributes, v_prev.ends_at)
          IS DISTINCT FROM ROW(v_price.currency, s.product_id, s.price_id, v_level, v_live, v_ends) THEN
        UPDATE billing.subscription_mrr
           SET currency = v_price.currency, product_id = s.product_id, price_id = s.price_id, mrr = v_level, contributes = v_live,
               ends_at = v_ends, next_change_at = v_ends,
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
COMMENT ON FUNCTION billing.refresh_subscription_mrr(s billing.subscriptions, p_at timestamptz, p_cause text) IS 'Recomputes one subscription''s subscription_mrr at p_at and its scheduled churn, contraction or expansion. A changed level makes the row dirty; billing.settle_mrr records it at commit.';

-- The lines a customer's changes in one currency make: each changed
-- subscription's MRR before (as recorded) and after, on the side of that
-- currency.
CREATE FUNCTION billing.pending_mrr_lines(p_merchant uuid, p_customer uuid, p_currency text)
RETURNS TABLE(subscription_id uuid, product_id uuid, price_id uuid, mrr_before bigint, mrr_after bigint, cause text, changed_at timestamptz)
LANGUAGE sql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
SELECT m.subscription_id,
       CASE WHEN m.currency = p_currency THEN m.product_id ELSE m.recorded_product_id END,
       CASE WHEN m.currency = p_currency THEN m.price_id ELSE m.recorded_price_id END,
       CASE WHEN m.recorded_currency = p_currency THEN m.recorded_mrr ELSE 0 END,
       CASE WHEN m.currency = p_currency THEN m.mrr ELSE 0 END,
       m.cause, m.changed_at
  FROM billing.subscription_mrr m
 WHERE m.merchant_id = p_merchant AND m.customer_id = p_customer AND m.dirty
   AND p_currency IN (m.currency, m.recorded_currency)
$$;

-- Records every dirty subscription_mrr row of the merchant: one movement per
-- customer and currency whose MRR changed, its lines and rollups, then marks
-- the rows recorded. Only this transaction's rows are dirty. Every customer
-- row is locked before any rollup row, and each table's rows in key order,
-- so concurrent settlements never wait on each other in a cycle.
CREATE FUNCTION billing.settle_mrr(p_merchant uuid) RETURNS void
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
    UPDATE billing.subscription_mrr
       SET recorded_currency = currency, recorded_product_id = product_id, recorded_price_id = price_id, recorded_mrr = mrr
     WHERE merchant_id = p_merchant AND dirty;
END;
$$;
COMMENT ON FUNCTION billing.settle_mrr(p_merchant uuid) IS 'Records the merchant''s dirty subscription_mrr rows: per customer and currency one mrr_movements row classified by the customer''s MRR before and after, its lines, and the rollups.';

-- Each queued row event probes its row as it is now: the first one settles
-- the merchant, and every later one finds its row recorded.
CREATE FUNCTION billing.settle_subscription_mrr() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM billing.subscription_mrr
                WHERE merchant_id = NEW.merchant_id AND subscription_id = NEW.subscription_id AND dirty) THEN
        PERFORM billing.settle_mrr(NEW.merchant_id);
    END IF;
    RETURN NULL;
END;
$$;

-- Deferred to commit, so the changes of one transaction (a cancel and the
-- subscription replacing it) make one movement per customer and currency.
CREATE CONSTRAINT TRIGGER subscription_mrr_settle AFTER INSERT OR UPDATE ON billing.subscription_mrr DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION billing.settle_subscription_mrr();

CREATE FUNCTION billing.subscriptions_track_mrr() RETURNS trigger
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

CREATE TRIGGER trg_subscriptions_track_mrr AFTER INSERT OR UPDATE OF status, price_id, product_id, quantity, ended_at, current_period_starts_at, current_period_ends_at, deleted_at, cancel_type ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_track_mrr();

CREATE FUNCTION billing.scheduled_changes_track_mrr() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE
    v_open billing.mrr_scheduled;
BEGIN
    IF NEW.status <> 'scheduled' AND (TG_OP = 'INSERT' OR OLD.status <> 'scheduled') OR billing.billing_restore_active(NEW.merchant_id) THEN
        RETURN NULL;
    END IF;
    IF NEW.status = 'scheduled' THEN
        PERFORM billing.schedule_mrr_change(NEW.merchant_id, NEW.subscription_id, NEW.created_at);
    ELSE
        SELECT * INTO v_open FROM billing.mrr_scheduled
         WHERE merchant_id = NEW.merchant_id AND scheduled_change_id = NEW.id AND status = 'scheduled' FOR UPDATE;
        IF FOUND THEN
            PERFORM billing.resolve_mrr_scheduled(v_open, CASE WHEN NEW.status = 'applied' THEN 'effected' ELSE 'reversed' END,
                COALESCE(NEW.applied_at, NEW.canceled_at, statement_timestamp()));
        END IF;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER scheduled_changes_track_mrr AFTER INSERT OR UPDATE OF status ON billing.scheduled_changes
    FOR EACH ROW EXECUTE FUNCTION billing.scheduled_changes_track_mrr();

-- openrails.mrr_due: a subscription whose end has come leaves MRR at that
-- end. The subscription is locked first, as its writers lock it.
CREATE FUNCTION billing.effect_due_mrr(p_merchant uuid, p_subscription uuid, p_at timestamptz) RETURNS boolean
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE
    v_sub billing.subscriptions;
    v_due timestamptz;
BEGIN
    SELECT * INTO v_sub FROM billing.subscriptions WHERE merchant_id = p_merchant AND id = p_subscription FOR UPDATE;
    SELECT next_change_at INTO v_due FROM billing.subscription_mrr
     WHERE merchant_id = p_merchant AND subscription_id = p_subscription AND next_change_at <= p_at;
    IF NOT FOUND THEN
        RETURN false;
    END IF;
    PERFORM billing.refresh_subscription_mrr(v_sub, v_due, billing.mrr_cancel_cause(v_sub.cancel_type));
    RETURN true;
END;
$$;
COMMENT ON FUNCTION billing.effect_due_mrr(p_merchant uuid, p_subscription uuid, p_at timestamptz) IS 'Takes one due subscription_mrr row (next_change_at <= p_at): its scheduled churn takes effect, dated at its end. False when nothing is due.';

-- A subscription with no subscription_mrr row (one written before MRR, or a
-- restore from an archive without it) is adopted as an opening movement.
CREATE FUNCTION billing.seed_mrr(p_merchant uuid, p_at timestamptz) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE
    v_sub billing.subscriptions;
BEGIN
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
END;
$$;

SELECT billing.seed_mrr(id, now()) FROM billing.merchants ORDER BY id;

CREATE FUNCTION billing.check_billing_restore_mrr(p_merchant uuid) RETURNS void
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
              OR COALESCE(t.subscribers, 0) <> COALESCE(c.subscribers, 0)) THEN
        RAISE EXCEPTION 'billing restore MRR integrity mismatch' USING ERRCODE='23514';
    END IF;
END;
$$;
REVOKE ALL ON FUNCTION billing.check_billing_restore_mrr(p_merchant uuid) FROM PUBLIC;

-- A restore skips the MRR triggers; its rows must agree before it commits.
CREATE OR REPLACE FUNCTION billing.require_finished_billing_restore() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.kind='billing_restore' THEN
        IF NOT EXISTS (SELECT 1 FROM billing.maintenance_runs WHERE id=NEW.id AND merchant_id=NEW.merchant_id AND status='completed') THEN
            RAISE EXCEPTION 'unfinished billing restore receipts cannot commit' USING ERRCODE='23514';
        END IF;
        PERFORM billing.check_billing_restore_ledger(NEW.merchant_id);
        PERFORM billing.check_billing_restore_credit_grants(NEW.merchant_id);
        PERFORM billing.check_billing_restore_mrr(NEW.merchant_id);
    END IF;
    RETURN NULL;
END;
$$;

-- The restore guard names the MRR tables.
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

-- Catalog authoring takes the merchant row FOR NO KEY UPDATE: it changes no
-- key, so rows that only reference the merchant (the MRR rollups) never make
-- it refuse.
CREATE OR REPLACE FUNCTION billing.catalog_authored_write() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE mid uuid;
BEGIN
    IF TG_OP='UPDATE' AND NEW IS NOT DISTINCT FROM OLD THEN RETURN NEW; END IF;
    IF TG_OP='DELETE' THEN mid := OLD.merchant_id; ELSE mid := NEW.merchant_id; END IF;
    IF current_setting('openrails.catalog_batch_merchant_id',true) IS DISTINCT FROM mid::text THEN
        -- A legacy raw writer may already hold a child-row lock. Do not wait
        -- behind a merchant-first transaction while holding that child: refuse
        -- with a retryable serialization conflict instead of deadlocking.
        BEGIN
            PERFORM id FROM billing.merchants WHERE id=mid FOR NO KEY UPDATE NOWAIT;
        EXCEPTION WHEN lock_not_available THEN
            RAISE EXCEPTION 'concurrent catalog authoring; retry the transaction' USING ERRCODE='40001';
        END;
        UPDATE billing.merchants SET catalog_revision=catalog_revision+1 WHERE id=mid;
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
END $$;

CREATE OR REPLACE FUNCTION billing.catalog_authored_entitlements() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
DECLARE mid uuid;
BEGIN
    FOR mid IN SELECT DISTINCT merchant_id FROM changed LOOP
        IF current_setting('openrails.catalog_batch_merchant_id',true) IS DISTINCT FROM mid::text THEN
            BEGIN
                PERFORM id FROM billing.merchants WHERE id=mid FOR NO KEY UPDATE NOWAIT;
            EXCEPTION WHEN lock_not_available THEN
                RAISE EXCEPTION 'concurrent catalog authoring; retry the transaction' USING ERRCODE='40001';
            END;
            UPDATE billing.merchants SET catalog_revision=catalog_revision+1 WHERE id=mid;
        END IF;
    END LOOP;
    RETURN NULL;
END $$;

-- Seats are bounded as a per-seat price bounds them (catalog.MaxQuantity).
-- Last: each holds its table's exclusive lock to commit, so reads wait from
-- here, not through the backfill.
ALTER TABLE billing.subscriptions ADD CONSTRAINT subscriptions_quantity_max_check CHECK (quantity <= 10000);
ALTER TABLE billing.scheduled_changes ADD CONSTRAINT scheduled_changes_quantity_max_check CHECK (quantity <= 10000);
