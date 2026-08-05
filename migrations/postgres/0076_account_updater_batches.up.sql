-- or#795: the batch Account Updater needs a durable job ref and an armed,
-- activity-shaped due-work queue.
--
-- Everything DOWNSTREAM of an account-updater job completing already shipped
-- (result CSV parse, UPD_* rotation through RotateCustodianMethodRef,
-- WRN_CLOSED_ACCOUNT park). What was missing is the half that ASKS: something
-- that decides which instruments are worth refreshing, submits them, and
-- survives a restart while the network takes days to answer.
--
-- Three pieces:
--
--   * payment_methods.account_updater_checked_at — the per-instrument
--     watermark. "Not refreshed since the last cycle" is a column, not a scan.
--   * openrails.account_updater_batches — the durable job ref. A batch is
--     written BEFORE the provider is touched, so a crash between submit and
--     ingest resumes by POLLING the recorded job, never by resubmitting. A
--     partial unique index allows exactly ONE open batch per custodian, which
--     is the duplicate-submit guard the intents log alone cannot express.
--   * two SECURITY DEFINER work queues (the or#837 shape): merchants with due
--     instruments, and merchants with an open batch. Ids only; every read and
--     write then runs per-merchant under RunInMerchantScope.
--
-- The due queue starts at the CUSTODIANS table and only descends into
-- instruments for a merchant that declares an ARMED basis_theory custodian
-- (`settings.account_updater`, the contracted add-on). A merchant with no
-- custodian costs one index probe of a table that holds one row per
-- arrangement — not a walk of its payment methods.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

-- --------------------------------------------------------------------------
-- 1. The per-instrument watermark
-- --------------------------------------------------------------------------

ALTER TABLE openrails.payment_methods
    ADD COLUMN account_updater_checked_at timestamp with time zone;

COMMENT ON COLUMN openrails.payment_methods.account_updater_checked_at IS
    'or#795: when this instrument was last SUBMITTED to a batch account-updater cycle (not when it last changed). NULL = never. The staleness half of the due-work predicate: an instrument refreshed inside the lookahead window is not re-submitted, so one renewal cycle costs at most one network lookup per card.';

-- The due-work leg: per merchant, the custodian-held instruments whose
-- watermark is stale. Parked rows are deliberately INCLUDED — a parked card is
-- exactly what the updater exists to recover (or#872).
-- NULLS FIRST matches the runner's urgency order (never-checked cards first,
-- then the stalest), so the ordered scan is the index walk itself.
CREATE INDEX ix_payment_methods_account_updater_due
    ON openrails.payment_methods USING btree (merchant_id, custodian, account_updater_checked_at NULLS FIRST)
    WHERE (custodian <> 'psp' AND rail_method_ref <> '');

-- The renewal leg: "does this instrument back a subscription renewing inside
-- the window". Ordered by the renewal instant so the probe stops at the first
-- qualifying row.
CREATE INDEX ix_subscriptions_renewal_by_payment_method
    ON openrails.subscriptions USING btree (payment_method_id, current_period_ends_at)
    WHERE (deleted_at IS NULL AND payment_method_id IS NOT NULL
           AND status IN ('active'::openrails.subscription_status, 'past_due'::openrails.subscription_status));

-- --------------------------------------------------------------------------
-- 2. The durable batch (the job ref that makes a restart resumable)
-- --------------------------------------------------------------------------

CREATE TABLE openrails.account_updater_batches (
    id uuid DEFAULT uuidv7() NOT NULL PRIMARY KEY,
    merchant_id uuid NOT NULL,
    custodian_id uuid NOT NULL,
    -- The custodian's own job id. '' until the create call is confirmed; the
    -- row exists before that, which is what makes the create idempotent
    -- (same batch id => same intent => same BT-IDEMPOTENCY-KEY).
    job_ref text DEFAULT ''::text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    -- The batch membership, verbatim as uploaded: [{"payment_method_id":…,
    -- "token":…,"expiration_month":…,"expiration_year":…}]. Bounded by the
    -- runner's per-batch cap.
    instruments jsonb DEFAULT '[]'::jsonb NOT NULL,
    -- Per-instrument result vocabulary, VERBATIM as the custodian returned it:
    -- {"UPD_PAN": 3, "WRN_CLOSED_ACCOUNT": 1, …}. Unrecognized codes are
    -- counted here too — recorded honestly, folded by nothing (#651).
    result_counts jsonb DEFAULT '{}'::jsonb NOT NULL,
    failure_reason text DEFAULT ''::text NOT NULL,
    submitted_at timestamp with time zone,
    last_polled_at timestamp with time zone,
    completed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT account_updater_batches_status_check
        CHECK ((status = ANY (ARRAY['pending'::text, 'submitted'::text, 'completed'::text, 'failed'::text]))),
    -- A submitted batch without a job ref would be a batch nobody can poll.
    CONSTRAINT account_updater_batches_submitted_has_job
        CHECK ((status <> 'submitted'::text) OR (btrim(job_ref) <> ''::text))
);

ALTER TABLE ONLY openrails.account_updater_batches FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.account_updater_batches IS
    'or#795: one batch account-updater cycle for one custodian. Written BEFORE the provider is touched and kept until the results are folded, so a worker restart between submit and ingest RESUMES POLLING the recorded job instead of resubmitting a paid batch. The membership is recorded verbatim; the result vocabulary is counted verbatim.';
COMMENT ON COLUMN openrails.account_updater_batches.job_ref IS
    'The custodian-native job id (Basis Theory account-updater job). '''' until the create call is confirmed.';
COMMENT ON COLUMN openrails.account_updater_batches.status IS
    'pending = assembled, not yet confirmed at the custodian | submitted = the custodian owns it, poll for results | completed = results folded | failed = abandoned (the instruments become due again; nothing is parked on our own malfunction).';

ALTER TABLE ONLY openrails.account_updater_batches
    ADD CONSTRAINT account_updater_batches_merchant_fk FOREIGN KEY (merchant_id)
    REFERENCES openrails.merchants(id) ON DELETE CASCADE;

-- Composite, like psps_custodian_fk: a batch can only reference ITS OWN
-- merchant's custodian, enforced by the database rather than the validator.
ALTER TABLE ONLY openrails.account_updater_batches
    ADD CONSTRAINT account_updater_batches_custodian_fk FOREIGN KEY (custodian_id, merchant_id)
    REFERENCES openrails.custodians(id, merchant_id) ON DELETE CASCADE;

-- THE duplicate-submit guard: at most one open batch per custodian. Without
-- it, a pass that crashed after submitting but before stamping the instruments
-- would find the same cards due and pay for a second batch.
--
-- Merchant-led (GAP-10 / SEC-24): a custodian belongs to exactly one merchant,
-- so (merchant_id, custodian_id) is the same constraint — and it is the one a
-- merchant can see under RLS, so no merchant can squat another's slot with an
-- invisible conflicting row.
CREATE UNIQUE INDEX uq_account_updater_batches_open
    ON openrails.account_updater_batches USING btree (merchant_id, custodian_id)
    WHERE (status = ANY (ARRAY['pending'::text, 'submitted'::text]));

-- One batch per job ref, and the index the completion path looks the batch up
-- by (both ingestion paths close a batch by the JOB id — it is the only handle
-- the webhook and the poller share).
CREATE UNIQUE INDEX uq_account_updater_batches_job
    ON openrails.account_updater_batches USING btree (merchant_id, job_ref)
    WHERE (job_ref <> ''::text);

-- Non-partial and merchant-led: this is what the RLS predicate itself rides.
CREATE INDEX ix_account_updater_batches_merchant_status
    ON openrails.account_updater_batches USING btree (merchant_id, status, created_at);

ALTER TABLE openrails.account_updater_batches ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.account_updater_batches
    USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid))
    WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.account_updater_batches TO openrails_app;

-- --------------------------------------------------------------------------
-- 3. The work queues (or#837 shape: ids only, armed, capped, cursored)
-- --------------------------------------------------------------------------

CREATE FUNCTION openrails.account_updater_work_merchant_ids(
        p_custodian text,
        p_environment text,
        p_now timestamptz,
        p_default_lookahead_days int,
        p_after uuid,
        p_limit int)
    RETURNS TABLE (merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT c.merchant_id
      FROM openrails.custodians c
      -- The window is the CUSTODIAN's own declared lookahead (or the caller's
      -- default), so this queue and the per-merchant pass select the same
      -- instruments from the same number. A value that is not a plain integer
      -- cannot reach here through the settings validator; if one ever does, the
      -- default stands rather than the whole fan-out raising.
      CROSS JOIN LATERAL (
            SELECT make_interval(days => COALESCE(
                       CASE WHEN c.settings ->> 'account_updater_lookahead_days' ~ '^[0-9]+$'
                            THEN (c.settings ->> 'account_updater_lookahead_days')::int END,
                       p_default_lookahead_days)) AS lookahead
           ) w
     WHERE c.kind = lower(p_custodian)
       AND c.environment = p_environment
       AND NOT c.archived
       -- The contracted add-on. Unarmed custodians are not work: calling the
       -- account-updater API without it is a 403, not a refresh.
       AND COALESCE(c.settings ->> 'account_updater', 'false') IN ('true', 't', '1')
       AND (p_after IS NULL OR c.merchant_id > p_after)
       -- One open batch at a time per custodian: a merchant already waiting on
       -- the network has no NEW work, only results to ingest.
       AND NOT EXISTS (
             SELECT 1 FROM openrails.account_updater_batches b
              WHERE b.custodian_id = c.id
                AND b.status IN ('pending', 'submitted'))
       AND EXISTS (
             SELECT 1
               FROM openrails.payment_methods pm
              WHERE pm.merchant_id = c.merchant_id
                AND pm.custodian = c.kind
                AND pm.rail_method_ref <> ''
                AND (pm.account_updater_checked_at IS NULL
                     OR pm.account_updater_checked_at < p_now - w.lookahead)
                AND EXISTS (
                      SELECT 1
                        FROM openrails.subscriptions s
                       WHERE s.payment_method_id = pm.id
                         AND s.deleted_at IS NULL
                         AND s.status IN ('active', 'past_due')
                         AND s.current_period_ends_at IS NOT NULL
                         AND s.current_period_ends_at <= p_now + w.lookahead))
     ORDER BY c.merchant_id
     LIMIT p_limit;
END;
$$;

COMMENT ON FUNCTION openrails.account_updater_work_merchant_ids(text, text, timestamptz, int, uuid, int) IS
    'or#795: merchants whose ARMED custodian holds an instrument that backs a subscription renewing inside the custodian''s lookahead window and has not been refreshed since the last cycle — the fan-out list for AccountUpdaterWorker. Starts at the custodian registry, so a merchant that never signed up for the add-on costs one index probe and never reaches its payment methods. Ids only, after a cursor, capped.';

REVOKE ALL ON FUNCTION openrails.account_updater_work_merchant_ids(text, text, timestamptz, int, uuid, int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION openrails.account_updater_work_merchant_ids(text, text, timestamptz, int, uuid, int) TO openrails_app;

CREATE FUNCTION openrails.account_updater_open_batch_merchant_ids(p_limit int)
    RETURNS TABLE (merchant_id uuid)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path TO 'openrails', 'pg_catalog'
    AS $$
BEGIN
    PERFORM openrails.assert_cross_merchant_reader();
    RETURN QUERY
    SELECT b.merchant_id
      FROM openrails.account_updater_batches b
     WHERE b.status IN ('pending', 'submitted')
     GROUP BY b.merchant_id
     -- Oldest open batch first: at cap, the merchant who has waited longest is
     -- served, never an arbitrary head (the or#837 fairness fix).
     ORDER BY MIN(b.created_at)
     LIMIT p_limit;
END;
$$;

COMMENT ON FUNCTION openrails.account_updater_open_batch_merchant_ids(int) IS
    'or#795: merchants with an account-updater batch the custodian still owes results for — the ingest fan-out. Ids only; the poll, the download and the fold all run per-merchant under RunInMerchantScope.';

REVOKE ALL ON FUNCTION openrails.account_updater_open_batch_merchant_ids(int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION openrails.account_updater_open_batch_merchant_ids(int) TO openrails_app;
