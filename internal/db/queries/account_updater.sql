-- or#795 batch Account Updater: due-work discovery, the durable batch (job
-- ref), and the per-instrument watermark. Migration 0076.
--
-- The two CROSS-MERCHANT readers are migration 0076's SECURITY DEFINER work
-- queues (the or#837 shape). They return ids only; the instrument reads, the
-- provider calls and every write run per-merchant under RunInMerchantScope.

-- CROSS-MERCHANT: merchants whose ARMED custodian holds an instrument due for
-- a refresh ahead of its renewal. Capped and cursored.
-- name: ListAccountUpdaterWorkMerchants :many
SELECT merchant_id FROM openrails.account_updater_work_merchant_ids(
    sqlc.arg(custodian)::text,
    sqlc.arg(environment)::text,
    sqlc.arg(now)::timestamptz,
    sqlc.arg(default_lookahead_days)::int,
    sqlc.narg(after)::uuid,
    sqlc.arg(merchant_limit)::int);

-- CROSS-MERCHANT: merchants with a batch the custodian still owes results for.
-- name: ListAccountUpdaterOpenBatchMerchants :many
SELECT merchant_id FROM openrails.account_updater_open_batch_merchant_ids(
    sqlc.arg(merchant_limit)::int);

-- The batch membership for ONE merchant: custodian-held instruments backing a
-- subscription that renews inside the lookahead window and whose watermark is
-- stale. Parked instruments are deliberately included — a parked card is what
-- the updater exists to recover (or#872). Never-checked first, then stalest.
-- name: ListDueAccountUpdaterInstruments :many
SELECT pm.id, pm.rail_method_ref, pm.expiry_date
FROM openrails.payment_methods pm
WHERE pm.merchant_id = sqlc.arg(merchant_id)
  AND pm.custodian = sqlc.arg(custodian)
  AND pm.rail_method_ref <> ''
  AND (pm.account_updater_checked_at IS NULL
       OR pm.account_updater_checked_at < sqlc.arg(stale_before)::timestamptz)
  AND EXISTS (
        SELECT 1 FROM openrails.subscriptions s
         WHERE s.payment_method_id = pm.id
           AND s.merchant_id = pm.merchant_id
           AND s.deleted_at IS NULL
           AND s.status IN ('active', 'past_due')
           AND s.current_period_ends_at IS NOT NULL
           AND s.current_period_ends_at <= sqlc.arg(renewal_before)::timestamptz)
ORDER BY pm.account_updater_checked_at NULLS FIRST, pm.id
LIMIT sqlc.arg(row_limit);

-- Stamps the watermark for the instruments a batch actually carried. Written
-- in the SAME transaction that marks the batch submitted: a card is "checked"
-- exactly when the custodian took it, never before.
-- name: StampAccountUpdaterChecked :execrows
UPDATE openrails.payment_methods SET
    account_updater_checked_at = sqlc.arg(checked_at)::timestamptz,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)
  AND id = ANY(sqlc.arg(ids)::uuid[]);

-- The durable batch is written BEFORE the custodian is touched: the row IS the
-- job ref that makes a restart resume rather than resubmit. The partial unique
-- index (one open batch per custodian) is the duplicate-submit guard.
-- name: CreateAccountUpdaterBatch :one
INSERT INTO openrails.account_updater_batches (
    merchant_id, custodian_id, instruments
) VALUES (
    sqlc.arg(merchant_id), sqlc.arg(custodian_id), sqlc.arg(instruments)
)
RETURNING *;

-- name: GetAccountUpdaterBatch :one
SELECT * FROM openrails.account_updater_batches
WHERE merchant_id = sqlc.arg(merchant_id) AND id = sqlc.arg(id);

-- name: ListOpenAccountUpdaterBatches :many
-- Bounded twice over: the one-open-batch-per-custodian unique index means a
-- merchant has at most as many rows here as it has declared custodians, and
-- the caller still caps the pass.
SELECT * FROM openrails.account_updater_batches
WHERE merchant_id = sqlc.arg(merchant_id)
  AND status IN ('pending', 'submitted')
ORDER BY created_at
LIMIT sqlc.arg(row_limit);

-- Records the custodian's job id the moment the create call is confirmed, so a
-- crash immediately after it resumes on the SAME job.
-- name: SetAccountUpdaterBatchJobRef :execrows
UPDATE openrails.account_updater_batches SET
    job_ref = sqlc.arg(job_ref),
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)
  AND id = sqlc.arg(id)
  AND job_ref = '';

-- name: MarkAccountUpdaterBatchSubmitted :execrows
UPDATE openrails.account_updater_batches SET
    status = 'submitted',
    submitted_at = COALESCE(submitted_at, sqlc.arg(submitted_at)::timestamptz),
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)
  AND id = sqlc.arg(id)
  AND status = 'pending';

-- name: MarkAccountUpdaterBatchPolled :execrows
UPDATE openrails.account_updater_batches SET
    last_polled_at = sqlc.arg(polled_at)::timestamptz,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)
  AND id = sqlc.arg(id);

-- The results landed and were folded. Keyed on the JOB ref because both
-- ingestion paths (the poller and the account-updater.job.completed webhook)
-- close the same batch, and only the job id is common to both.
-- name: CompleteAccountUpdaterBatchByJobRef :execrows
UPDATE openrails.account_updater_batches SET
    status = 'completed',
    result_counts = sqlc.arg(result_counts),
    completed_at = sqlc.arg(completed_at)::timestamptz,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)
  AND job_ref = sqlc.arg(job_ref)
  AND job_ref <> ''
  AND status IN ('pending', 'submitted');

-- Abandoned, never parked: a batch we could not finish says nothing about the
-- customer's card, so the instruments simply become due again (no evidence,
-- no action).
-- name: FailAccountUpdaterBatch :execrows
UPDATE openrails.account_updater_batches SET
    status = 'failed',
    failure_reason = sqlc.arg(failure_reason),
    completed_at = sqlc.arg(completed_at)::timestamptz,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)
  AND id = sqlc.arg(id)
  AND status IN ('pending', 'submitted');
