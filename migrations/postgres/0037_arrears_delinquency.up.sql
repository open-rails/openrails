-- or#878: arrears delinquency — OpenRails owns the state and the signal; the
-- operator owns the shutoff.
--
-- TWO AXES, deliberately kept apart (or#828/or#870 own the other one):
--   * the DECLINE BUCKET says why a charge failed ⇒ what to do about the CARD;
--   * DELINQUENCY says how long a debt has gone unpaid ⇒ what to do about
--     SERVICE. Amount- and time-based, independent of any single charge.
--
-- Nothing here revokes an entitlement or stops a customer's resources.
-- OpenRails refuses NEW spend at admission (the one lever it is authoritative
-- over) and emits a durable, acknowledged signal; the host decides what to shut
-- off, because only the host knows what it is running.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

-- --------------------------------------------------------------------------
-- The delinquency state, per (merchant, payer, currency)
-- --------------------------------------------------------------------------

-- A PROJECTION, not a second source of truth. Every field except the
-- transition watermarks is recomputable from the invoices themselves
-- (min(due_at) and sum(amount_due) over the payer's overdue open receivables)
-- against the merchant's declared grace/floor policy. What the invoices cannot
-- answer is "when did this state start" and "has this transition already been
-- announced" — that is the whole reason a row exists.
CREATE TABLE openrails.customer_delinquency (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL,
    state text DEFAULT 'current'::text NOT NULL,
    -- The oldest overdue due_at behind this state: the clock delinquency is
    -- measured on. NULL exactly when state = 'current'.
    overdue_since timestamp with time zone,
    -- When THIS state was entered. The watermark that makes a transition an
    -- edge rather than a level, so an event fires once.
    entered_at timestamp with time zone DEFAULT now() NOT NULL,
    overdue_amount bigint DEFAULT 0 NOT NULL,
    overdue_invoices bigint DEFAULT 0 NOT NULL,
    -- Monotonic per row, bumped only when state actually changes. It is the
    -- idempotency coordinate of the emitted signal: two concurrent evaluators
    -- of the same transition compute the same sequence and so the same
    -- dedupe_key, and the unique index below collapses them to one event.
    transition_seq bigint DEFAULT 0 NOT NULL,
    evaluated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT customer_delinquency_pkey PRIMARY KEY (merchant_id, customer_id, currency),
    CONSTRAINT customer_delinquency_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT,
    CONSTRAINT customer_delinquency_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id) ON DELETE CASCADE,
    CONSTRAINT customer_delinquency_state_chk CHECK ((state = ANY (ARRAY['current'::text, 'grace'::text, 'delinquent'::text]))),
    CONSTRAINT customer_delinquency_amount_chk CHECK (((overdue_amount >= 0) AND (overdue_invoices >= 0))),
    CONSTRAINT customer_delinquency_since_chk CHECK ((((state = 'current'::text) AND (overdue_since IS NULL)) OR ((state <> 'current'::text) AND (overdue_since IS NOT NULL)))),
    CONSTRAINT customer_delinquency_currency_shape CHECK ((currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^[a-z0-9][a-z0-9_-]*/[^/[:space:]]+$'))
);

-- The exit-scan's due-work index: currently-not-current payers only, which by
-- definition is a small set. Work scales with activity, never with the number
-- of customers on file.
CREATE INDEX ix_customer_delinquency_open
    ON openrails.customer_delinquency USING btree (merchant_id, customer_id, currency)
    WHERE (state <> 'current'::text);

COMMENT ON TABLE openrails.customer_delinquency IS
    'or#878 per-(merchant, payer, currency) arrears delinquency state: current -> grace -> delinquent, derived from overdue open receivables against the merchant''s declared grace window and amount floor. A projection of invoice truth; only the transition watermarks (entered_at, transition_seq) are not recomputable. Delinquency NEVER revokes an entitlement — it refuses new spend at admission and emits a host_lifecycle_events signal; the operator owns the shutoff.';

COMMENT ON COLUMN openrails.customer_delinquency.overdue_since IS
    'The oldest overdue due_at behind this state — the clock the grace window is measured on, not the moment we noticed.';

COMMENT ON COLUMN openrails.customer_delinquency.transition_seq IS
    'Bumped only when state changes; the idempotency coordinate of the emitted host_lifecycle_events row.';

ALTER TABLE openrails.customer_delinquency ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.customer_delinquency FORCE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.customer_delinquency USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.customer_delinquency TO openrails_app;

-- --------------------------------------------------------------------------
-- The signal: a durable, acknowledged host-lifecycle feed
-- --------------------------------------------------------------------------

-- Modelled on payment_settlement_events (0005/0010) because the guarantees are
-- the same and a missed message is expensive in both directions: a missed
-- cut-off signal is a revenue leak, a missed restore signal is an outage for
-- someone who already paid. So: durable rows, explicit ack, pruned after
-- delivery — never a fire-and-forget webhook.
--
-- The SHAPE is general (subject + type + payload) because the next such signal
-- will want identical guarantees. The CONTENT is not speculative: delinquency
-- transitions are the only kind emitted today.
CREATE TABLE openrails.host_lifecycle_events (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    event_type text NOT NULL,
    subject_type text NOT NULL,
    subject_id uuid NOT NULL,
    currency text,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    data jsonb DEFAULT '{}'::jsonb NOT NULL,
    delivered_at timestamp with time zone,
    -- Deterministic per transition, so re-emitting one is a no-op instead of a
    -- duplicate shutoff instruction.
    dedupe_key text NOT NULL,
    CONSTRAINT host_lifecycle_events_pkey PRIMARY KEY (id),
    CONSTRAINT host_lifecycle_events_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE,
    CONSTRAINT host_lifecycle_events_subject_chk CHECK ((subject_type = 'customer'::text)),
    CONSTRAINT host_lifecycle_events_currency_shape CHECK ((currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$' OR currency ~ '^[a-z0-9][a-z0-9_-]*/[^/[:space:]]+$'))
);

-- Merchant-scoped unique: the dedupe key is only ever computed within one
-- merchant's scope, so it cannot be a cross-merchant existence oracle.
CREATE UNIQUE INDEX uq_host_lifecycle_events_dedupe
    ON openrails.host_lifecycle_events USING btree (merchant_id, dedupe_key);

-- The feed's own read path: one merchant's undelivered events, oldest first.
CREATE INDEX ix_host_lifecycle_events_pending
    ON openrails.host_lifecycle_events USING btree (merchant_id, id)
    WHERE (delivered_at IS NULL);

-- The prune path (delivered rows, by age).
CREATE INDEX ix_host_lifecycle_events_delivered
    ON openrails.host_lifecycle_events USING btree (merchant_id, delivered_at)
    WHERE (delivered_at IS NOT NULL);

COMMENT ON TABLE openrails.host_lifecycle_events IS
    'or#878 durable host-consumption queue for lifecycle signals the embedding host must act on — today only arrears delinquency transitions (delinquency.grace / delinquency.entered / delinquency.cleared). Consumers ack after idempotent processing; delivered rows are pruned. OpenRails emits the signal and never performs the shutoff: it does not know what the host is running.';

COMMENT ON COLUMN openrails.host_lifecycle_events.dedupe_key IS
    'Deterministic per transition (delinquency:<customer>:<currency>:<transition_seq>) so a re-run collapses instead of instructing a second shutoff.';

ALTER TABLE openrails.host_lifecycle_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.host_lifecycle_events FORCE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.host_lifecycle_events USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.host_lifecycle_events TO openrails_app;

-- --------------------------------------------------------------------------
-- The fan-out work queue (0021/0022/0023 pattern)
-- --------------------------------------------------------------------------

-- Merchants with delinquency work: at least one overdue open receivable, or at
-- least one payer already parked non-current (the exit leg — someone has to
-- notice they paid). Ids only; every read and write of the rows themselves
-- happens per-merchant under RunInMerchantScope, under that merchant's policy.
--
-- Both legs are activity-shaped: the first rides ix_invoices_open_due, the
-- second ix_customer_delinquency_open. Neither enumerates customers.
CREATE FUNCTION openrails.delinquency_work_merchant_ids(p_now timestamptz, p_limit int)
    RETURNS TABLE (merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT q.mid
      FROM (
            SELECT DISTINCT i.merchant_id AS mid
              FROM openrails.invoices i
             WHERE i.status IN ('open', 'past_due')
               AND i.amount_due > 0
               AND i.due_at IS NOT NULL
               AND i.due_at < p_now
            UNION
            SELECT DISTINCT d.merchant_id AS mid
              FROM openrails.customer_delinquency d
             WHERE d.state <> 'current'
           ) q
     LIMIT p_limit;
END;
$$;

COMMENT ON FUNCTION openrails.delinquency_work_merchant_ids(timestamptz, int) IS
    'or#878: merchants with arrears delinquency work — an overdue open receivable (the enter leg) or a payer already parked non-current (the exit leg). The fan-out list for DelinquencyWorker. Ids only; states, transitions and signals are computed per-merchant under RunInMerchantScope.';

REVOKE ALL ON FUNCTION openrails.delinquency_work_merchant_ids(timestamptz, int) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION openrails.delinquency_work_merchant_ids(timestamptz, int) TO openrails_app;
