-- Batch account updater: due-work discovery, the durable batch (job ref) and
-- the per-instrument watermark. The two cross-merchant readers return ids only;
-- instrument reads, provider calls and writes run per merchant.

-- CROSS-MERCHANT: merchants whose armed custodian holds an instrument backing a
-- subscription that renews inside the custodian's lookahead and was not
-- refreshed since. Starts at the custodian registry, so a merchant without the
-- add-on costs one index probe. Capped and cursored.
-- name: ListAccountUpdaterWorkMerchants :many
SELECT c.merchant_id
FROM billing.custodians c
CROSS JOIN LATERAL (
    -- The custodian's own declared lookahead, else the caller's default.
    SELECT make_interval(days => COALESCE(
        CASE WHEN c.settings ->> 'account_updater_lookahead_days' ~ '^[0-9]+$'
             THEN (c.settings ->> 'account_updater_lookahead_days')::int END,
        sqlc.arg(default_lookahead_days)::int)) AS lookahead
) w
WHERE c.kind = lower(sqlc.arg(custodian)::text)
  AND c.environment = sqlc.arg(environment)::text
  AND NOT c.archived
  AND COALESCE(c.settings ->> 'account_updater', 'false') IN ('true', 't', '1')
  AND (sqlc.narg(after)::uuid IS NULL OR c.merchant_id > sqlc.narg(after)::uuid)
  -- One open batch per custodian: a waiting merchant has results to ingest, not new work.
  AND NOT EXISTS (
        SELECT 1 FROM billing.account_updater_batches b
         WHERE b.merchant_id = c.merchant_id AND b.custodian_id = c.id
           AND b.status IN ('pending', 'submitted'))
  AND EXISTS (
        SELECT 1 FROM billing.payment_methods pm
         WHERE pm.merchant_id = c.merchant_id
           AND pm.custodian <> 'psp' AND pm.custodian = c.kind AND pm.custodian_id = c.id
           AND pm.rail_method_ref IS NOT NULL
           AND (pm.account_updater_checked_at IS NULL
                OR pm.account_updater_checked_at < sqlc.arg(now)::timestamptz - w.lookahead)
           AND EXISTS (
                 SELECT 1 FROM billing.subscriptions s
                  WHERE s.merchant_id = pm.merchant_id AND s.payment_method_id = pm.id
                    AND s.deleted_at IS NULL
                    AND s.status IN ('active', 'past_due')
                    AND s.current_period_ends_at IS NOT NULL
                    AND s.current_period_ends_at <= sqlc.arg(now)::timestamptz + w.lookahead))
ORDER BY c.merchant_id
LIMIT sqlc.arg(merchant_limit)::int;

-- CROSS-MERCHANT: merchants with a batch the custodian still owes results for,
-- oldest open batch first so the longest-waiting merchant is served at the cap.
-- name: ListAccountUpdaterOpenBatchMerchants :many
SELECT b.merchant_id
FROM billing.account_updater_batches b
WHERE b.status IN ('pending', 'submitted')
GROUP BY b.merchant_id
ORDER BY MIN(b.created_at)
LIMIT sqlc.arg(merchant_limit)::int;

-- The batch membership for ONE merchant: custodian-held instruments backing a
-- subscription that renews inside the lookahead window and whose watermark is
-- stale. Parked instruments are deliberately included — a parked card is what
-- the updater exists to recover (or#872). Never-checked first, then stalest.
-- name: ListDueAccountUpdaterInstruments :many
SELECT pm.id, pm.rail_method_ref, pm.card_exp_month, pm.card_exp_year
FROM billing.payment_methods pm
WHERE pm.merchant_id = sqlc.arg(merchant_id)
  AND pm.custodian <> 'psp'
  AND pm.custodian_id = sqlc.arg(custodian_id)::uuid
  AND pm.custodian = sqlc.arg(custodian)
  AND pm.rail_method_ref IS NOT NULL
  AND (pm.account_updater_checked_at IS NULL
       OR pm.account_updater_checked_at < sqlc.arg(stale_before)::timestamptz)
  AND EXISTS (
        SELECT 1 FROM billing.subscriptions s
         WHERE s.payment_method_id = pm.id
           AND s.merchant_id = pm.merchant_id
           AND s.deleted_at IS NULL
           AND s.status IN ('active', 'past_due', 'awaiting_method')
           AND s.current_period_ends_at IS NOT NULL
           AND s.current_period_ends_at <= sqlc.arg(renewal_before)::timestamptz)
ORDER BY pm.account_updater_checked_at NULLS FIRST, pm.id
LIMIT sqlc.arg(row_limit);

-- Stamps the watermark for the instruments a batch actually carried. Written
-- in the SAME transaction that marks the batch submitted: a card is "checked"
-- exactly when the custodian took it, never before.
-- name: StampAccountUpdaterChecked :execrows
UPDATE billing.payment_methods SET
    account_updater_checked_at = sqlc.arg(checked_at)::timestamptz,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)
  AND id = ANY(sqlc.arg(ids)::uuid[]);

-- The durable batch is written BEFORE the custodian is touched: the row IS the
-- job ref that makes a restart resume rather than resubmit. The partial unique
-- index (one open batch per custodian) is the duplicate-submit guard.
-- name: CreateAccountUpdaterBatch :one
INSERT INTO billing.account_updater_batches (
    merchant_id, custodian_id, instruments
) VALUES (
    sqlc.arg(merchant_id), sqlc.arg(custodian_id), sqlc.arg(instruments)
)
RETURNING *;

-- name: GetAccountUpdaterBatch :one
SELECT * FROM billing.account_updater_batches
WHERE merchant_id = sqlc.arg(merchant_id) AND id = sqlc.arg(id);

-- name: ListOpenAccountUpdaterBatches :many
-- Bounded twice over: the one-open-batch-per-custodian unique index means a
-- merchant has at most as many rows here as it has declared custodians, and
-- the caller still caps the pass.
SELECT * FROM billing.account_updater_batches
WHERE merchant_id = sqlc.arg(merchant_id)
  AND status IN ('pending', 'submitted')
ORDER BY created_at
LIMIT sqlc.arg(row_limit);

-- Records the custodian's job id the moment the create call is confirmed, so a
-- crash immediately after it resumes on the SAME job.
-- name: SetAccountUpdaterBatchJobRef :execrows
UPDATE billing.account_updater_batches SET
    job_ref = sqlc.arg(job_ref)::text,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)
  AND id = sqlc.arg(id)
  AND job_ref IS NULL;

-- name: MarkAccountUpdaterBatchSubmitted :execrows
UPDATE billing.account_updater_batches SET
    status = 'submitted',
    submitted_at = COALESCE(submitted_at, sqlc.arg(submitted_at)::timestamptz),
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)
  AND id = sqlc.arg(id)
  AND status = 'pending';

-- name: MarkAccountUpdaterBatchPolled :execrows
UPDATE billing.account_updater_batches SET
    last_polled_at = sqlc.arg(polled_at)::timestamptz,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)
  AND id = sqlc.arg(id);

-- The results landed and were folded. Keyed on the JOB ref because both
-- ingestion paths (the poller and the account-updater.job.completed webhook)
-- close the same batch, and only the job id is common to both.
-- name: CompleteAccountUpdaterBatchByJobRef :execrows
UPDATE billing.account_updater_batches SET
    status = 'completed',
    result_counts = sqlc.arg(result_counts),
    completed_at = sqlc.arg(completed_at)::timestamptz,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)
  AND custodian_id = sqlc.arg(custodian_id)::uuid
  AND job_ref = sqlc.arg(job_ref)::text
  AND status IN ('pending', 'submitted');

-- Abandoned, never parked: a batch we could not finish says nothing about the
-- customer's card, so the instruments simply become due again (no evidence,
-- no action).
-- name: FailAccountUpdaterBatch :execrows
UPDATE billing.account_updater_batches SET
    status = 'failed',
    failure_reason = NULLIF(sqlc.arg(failure_reason)::text, ''),
    completed_at = sqlc.arg(completed_at)::timestamptz,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)
  AND id = sqlc.arg(id)
  AND status IN ('pending', 'submitted');
