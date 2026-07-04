-- #733 merchant analytics API — data-layer gaps.
--
-- 1. payments: attempt_kind (initial|renewal, stamped at write time),
--    failure_code (raw rail code, verbatim) + failure_reason (normalized
--    category, deterministic per-rail map in code), and reversal_kind
--    (refund|chargeback|dispute_reversal) so chargeback mirror rows are
--    distinguishable from refunds (both were bare negative rows before).
-- 2. subscription_status_transitions: append-only audit of every status
--    change, written by a row trigger — the only point that atomically sees
--    OLD.status and NEW.status for all ~25 writer sites (and skips full-row
--    no-op updates). Powers recovery_rate / dunning funnels exactly.
-- 3. admission_denials_hourly: merchant x payer x denial_reason x hour
--    counters, flushed periodically from the Redis admission hot path
--    (never per-request PG writes).
-- 4. Indexes for the metrics query shapes.

-- --- 1. payments columns ----------------------------------------------------

ALTER TABLE openrails.payments
    ADD COLUMN attempt_kind text,
    ADD COLUMN failure_code text,
    ADD COLUMN failure_reason text,
    ADD COLUMN reversal_kind text;

ALTER TABLE openrails.payments
    ADD CONSTRAINT chk_payments_attempt_kind CHECK (attempt_kind IS NULL OR attempt_kind IN ('initial', 'renewal')),
    ADD CONSTRAINT chk_payments_reversal_kind CHECK (reversal_kind IS NULL OR reversal_kind IN ('refund', 'chargeback', 'dispute_reversal'));

COMMENT ON COLUMN openrails.payments.attempt_kind IS '#733 initial|renewal, stamped at write time by the checkout vs rebill paths; NULL = unknown (imported/pre-instrumentation rows).';
COMMENT ON COLUMN openrails.payments.failure_code IS '#733 raw rail decline code, recorded verbatim (no fabrication).';
COMMENT ON COLUMN openrails.payments.failure_reason IS '#733 normalized decline category, derived deterministically from failure_code per rail.';
COMMENT ON COLUMN openrails.payments.reversal_kind IS '#733 discriminates mirror rows: refund | chargeback | dispute_reversal (dispute won). NULL on sale rows.';

-- Backfill mirror-row kinds from the synthetic transaction_id conventions the
-- webhook handlers already use (chargeback:* for NMI/CCBill, dp_* Stripe
-- disputes, dispute_won:* recoveries); everything else linked via
-- refunded_payment_id is a refund.
UPDATE openrails.payments SET reversal_kind = 'chargeback'
WHERE refunded_payment_id IS NOT NULL AND reversal_kind IS NULL
  AND (transaction_id LIKE 'chargeback:%' OR transaction_id LIKE 'dp\_%');
UPDATE openrails.payments SET reversal_kind = 'dispute_reversal'
WHERE refunded_payment_id IS NOT NULL AND reversal_kind IS NULL
  AND transaction_id LIKE 'dispute\_won:%';
UPDATE openrails.payments SET reversal_kind = 'refund'
WHERE refunded_payment_id IS NOT NULL AND reversal_kind IS NULL;

-- --- 2. subscription_status_transitions ---------------------------------------

CREATE TABLE openrails.subscription_status_transitions (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    subscription_id uuid NOT NULL,
    from_status openrails.subscription_status,
    to_status openrails.subscription_status NOT NULL,
    cancel_type text,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_sst_real_transition CHECK (from_status IS DISTINCT FROM to_status)
);

ALTER TABLE ONLY openrails.subscription_status_transitions FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.subscription_status_transitions IS '#733 append-only subscription status audit, written by trg_subscriptions_status_transition in the SAME tx as the status change. from_status NULL = row creation. Not retroactive: history begins at go-live.';
COMMENT ON COLUMN openrails.subscription_status_transitions.cancel_type IS 'cancel_type on the subscription at transition time (meaningful for to_status=cancelled).';

ALTER TABLE ONLY openrails.subscription_status_transitions
    ADD CONSTRAINT subscription_status_transitions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.subscription_status_transitions
    ADD CONSTRAINT sst_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.subscription_status_transitions
    ADD CONSTRAINT sst_subscription_fk FOREIGN KEY (subscription_id) REFERENCES openrails.subscriptions(id) ON DELETE CASCADE;

CREATE INDEX idx_sst_merchant_occurred ON openrails.subscription_status_transitions USING btree (merchant_id, occurred_at);

CREATE INDEX idx_sst_subscription ON openrails.subscription_status_transitions USING btree (subscription_id, occurred_at);

ALTER TABLE openrails.subscription_status_transitions ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.subscription_status_transitions USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT ON TABLE openrails.subscription_status_transitions TO openrails_app;

-- Trigger: the single point that sees OLD+NEW status atomically for every
-- writer (lifecycle service, dunning jobs, webhooks, admin, reconcile), in the
-- same transaction, skipping full-row updates that do not change status.
CREATE FUNCTION openrails.subscriptions_record_status_transition() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO openrails.subscription_status_transitions
            (merchant_id, subscription_id, from_status, to_status, cancel_type, occurred_at)
        VALUES (NEW.merchant_id, NEW.id, NULL, NEW.status, NEW.cancel_type, now());
    ELSIF OLD.status IS DISTINCT FROM NEW.status THEN
        INSERT INTO openrails.subscription_status_transitions
            (merchant_id, subscription_id, from_status, to_status, cancel_type, occurred_at)
        VALUES (NEW.merchant_id, NEW.id, OLD.status, NEW.status, NEW.cancel_type, now());
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_subscriptions_status_transition
    AFTER INSERT OR UPDATE OF status ON openrails.subscriptions
    FOR EACH ROW EXECUTE FUNCTION openrails.subscriptions_record_status_transition();

-- --- 3. admission_denials_hourly ------------------------------------------------

CREATE TABLE openrails.admission_denials_hourly (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    denial_reason text NOT NULL,
    hour_at timestamp with time zone NOT NULL,
    denials bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chk_adh_hour_aligned CHECK (hour_at = date_trunc('hour', hour_at)),
    CONSTRAINT chk_adh_denials_positive CHECK (denials > 0)
);

ALTER TABLE ONLY openrails.admission_denials_hourly FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.admission_denials_hourly IS '#733 hourly admission-denial aggregates (merchant x payer x reason), flushed periodically from Redis counters — the hot path never writes PG per-request.';

ALTER TABLE ONLY openrails.admission_denials_hourly
    ADD CONSTRAINT admission_denials_hourly_pkey PRIMARY KEY (merchant_id, customer_id, denial_reason, hour_at);

ALTER TABLE ONLY openrails.admission_denials_hourly
    ADD CONSTRAINT adh_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE INDEX idx_adh_merchant_hour ON openrails.admission_denials_hourly USING btree (merchant_id, hour_at);

ALTER TABLE openrails.admission_denials_hourly ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.admission_denials_hourly USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,UPDATE ON TABLE openrails.admission_denials_hourly TO openrails_app;

-- --- 4. metrics query-shape indexes ------------------------------------------------

-- payments flow: (merchant, purchased_at) range scans; status/kind predicates
-- ride as filters on the fetched rows.
CREATE INDEX idx_payments_merchant_purchased ON openrails.payments USING btree (merchant_id, purchased_at);

-- subscriptions flow + interval reconstruction.
CREATE INDEX idx_subscriptions_merchant_started ON openrails.subscriptions USING btree (merchant_id, started_at);
CREATE INDEX idx_subscriptions_merchant_ended ON openrails.subscriptions USING btree (merchant_id, ended_at) WHERE (ended_at IS NOT NULL);
CREATE INDEX idx_subscriptions_merchant_cancelled ON openrails.subscriptions USING btree (merchant_id, cancelled_at) WHERE (cancelled_at IS NOT NULL);

-- credit lots: purchased-lot flow + expiry lookups.
CREATE INDEX idx_grants_merchant_credit_created ON openrails.grants USING btree (merchant_id, created_at) WHERE (kind = 'credit');
CREATE INDEX idx_grants_credit_expiry ON openrails.grants USING btree (merchant_id, ends_at) WHERE (kind = 'credit' AND event = 'grant' AND ends_at IS NOT NULL);

-- ledger balance reconstruction + usage flow.
CREATE INDEX idx_ledger_transfers_merchant_created ON openrails.ledger_transfers USING btree (merchant_id, created_at);
CREATE INDEX idx_usage_events_merchant_occurred ON openrails.usage_events USING btree (merchant_id, occurred_at);
