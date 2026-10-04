-- #786 webhook-health recording. All statements run merchant-scoped (MerchantTx
-- or a pinned merchant connection); INSERTs pass merchant_id explicitly.

-- A source is a PSP or a custodian: exactly one of psp_id and custodian_id.

-- name: RecordWebhookAccepted :exec
-- Verified-accepted webhook: stamp the silence watermark.
INSERT INTO billing.webhook_health (merchant_id, psp_id, custodian_id, last_accepted_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.narg(psp_id)::uuid, sqlc.narg(custodian_id)::uuid, sqlc.arg(at)::timestamptz)
ON CONFLICT (merchant_id, psp_id, custodian_id) DO UPDATE SET
    last_accepted_at = EXCLUDED.last_accepted_at,
    updated_at = now();

-- name: RecordWebhookRejected :exec
-- Failed verification: bump the daily reject bucket. NEVER touches
-- last_accepted_at — rejects must not look like liveness. The snapshot row is
-- still upserted so a source that has only ever rejected has a created_at for
-- the silence age to measure from.
WITH health AS (
    INSERT INTO billing.webhook_health (merchant_id, psp_id, custodian_id)
    VALUES (sqlc.arg(merchant_id)::uuid, sqlc.narg(psp_id)::uuid, sqlc.narg(custodian_id)::uuid)
    ON CONFLICT (merchant_id, psp_id, custodian_id) DO UPDATE SET
        updated_at = now()
)
INSERT INTO billing.webhook_health_daily (merchant_id, psp_id, custodian_id, day_at, rejected)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.narg(psp_id)::uuid, sqlc.narg(custodian_id)::uuid, date_trunc('day', sqlc.arg(at)::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', 1)
ON CONFLICT (merchant_id, psp_id, custodian_id, day_at) DO UPDATE SET
    rejected = billing.webhook_health_daily.rejected + 1;

-- name: RecordWebhookDrift :execrows
-- Pull-derived corrections count as drift ONLY when the PSP's accepted
-- watermark predates its previous pull (last_pull_at still holds it during a
-- refresh) — the change arrived by pull when a webhook should have announced
-- it. A first-ever pull records nothing: an initial import is not drift.
-- Returns rows affected (0 = gate closed).
WITH gate AS (
    UPDATE billing.webhook_health
    SET updated_at = now()
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND psp_id = sqlc.arg(psp_id)::uuid
      AND last_pull_at IS NOT NULL
      AND (last_accepted_at IS NULL OR last_accepted_at < last_pull_at)
    RETURNING merchant_id, psp_id
)
INSERT INTO billing.webhook_health_daily (merchant_id, psp_id, day_at, drift)
SELECT merchant_id, psp_id, date_trunc('day', sqlc.arg(at)::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', sqlc.arg(n)::bigint
FROM gate
ON CONFLICT (merchant_id, psp_id, custodian_id, day_at) DO UPDATE SET
    drift = billing.webhook_health_daily.drift + EXCLUDED.drift;

-- name: StampWebhookPull :exec
-- Advance the PSP's pull watermark AFTER a refresh pass, so during the next
-- pass last_pull_at is the PREVIOUS pull the drift gate compares against.
INSERT INTO billing.webhook_health (merchant_id, psp_id, last_pull_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(at)::timestamptz)
ON CONFLICT (merchant_id, psp_id, custodian_id) DO UPDATE SET
    last_pull_at = EXCLUDED.last_pull_at,
    updated_at = now();

-- name: GetPSPRefreshWatermark :one
-- Exact account event coverage. A sibling account's refresh or a recent
-- health stamp while catching up historical windows cannot retire this job.
SELECT watermark_at
FROM billing.psp_refresh_watermarks
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND event_domain = 'events';

-- name: UpsertPSPRefreshWatermark :exec
INSERT INTO billing.psp_refresh_watermarks (merchant_id, psp_id, event_domain, watermark_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, 'events', sqlc.arg(watermark_at)::timestamptz)
ON CONFLICT (merchant_id, psp_id, event_domain)
DO UPDATE SET watermark_at = EXCLUDED.watermark_at, updated_at = now();
