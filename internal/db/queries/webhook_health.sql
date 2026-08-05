-- #786 webhook-health recording. All statements run merchant-scoped (MerchantTx
-- or a pinned merchant connection); INSERTs pass merchant_id for RLS WITH CHECK.

-- name: RecordWebhookAccepted :exec
-- Verified-accepted webhook: stamp the silence watermark. The lifetime tallies
-- this used to bump were dropped in or#823 — a monotonic total answers no
-- windowed question, which is what webhook_health_daily is for.
INSERT INTO openrails.webhook_health (merchant_id, rail, last_accepted_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(rail)::text, sqlc.arg(at)::timestamptz)
ON CONFLICT (merchant_id, rail) DO UPDATE SET
    last_accepted_at = EXCLUDED.last_accepted_at,
    updated_at = now();

-- name: RecordWebhookRejected :exec
-- Failed signature verification: bump the daily reject bucket. NEVER touches
-- last_accepted_at — rejects must not look like liveness. The snapshot row is
-- still upserted so a rail that has ONLY ever rejected still has a created_at
-- for the silence age to measure from.
WITH health AS (
    INSERT INTO openrails.webhook_health (merchant_id, rail)
    VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(rail)::text)
    ON CONFLICT (merchant_id, rail) DO UPDATE SET
        updated_at = now()
)
INSERT INTO openrails.webhook_health_daily (merchant_id, rail, day_at, rejected)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(rail)::text, date_trunc('day', sqlc.arg(at)::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', 1)
ON CONFLICT (merchant_id, rail, day_at) DO UPDATE SET
    rejected = openrails.webhook_health_daily.rejected + 1;

-- name: RecordWebhookDrift :execrows
-- Pull-derived corrections count as drift ONLY when the accepted watermark
-- predates the previous pull (last_pull_at still holds it during a refresh) —
-- i.e. the change arrived by pull when a webhook should have announced it.
-- First-ever pull (no last_pull_at / no row) records nothing: an initial
-- import is not drift. Returns rows affected (0 = gate closed).
WITH gate AS (
    UPDATE openrails.webhook_health
    SET updated_at = now()
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND rail = sqlc.arg(rail)::text
      AND last_pull_at IS NOT NULL
      AND (last_accepted_at IS NULL OR last_accepted_at < last_pull_at)
    RETURNING merchant_id, rail
)
INSERT INTO openrails.webhook_health_daily (merchant_id, rail, day_at, drift)
SELECT merchant_id, rail, date_trunc('day', sqlc.arg(at)::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', sqlc.arg(n)::bigint
FROM gate
ON CONFLICT (merchant_id, rail, day_at) DO UPDATE SET
    drift = openrails.webhook_health_daily.drift + EXCLUDED.drift;

-- name: StampWebhookPull :exec
-- Advance the pull watermark AFTER a provider-refresh pass, so during the next
-- pass last_pull_at is the PREVIOUS pull the drift gate compares against.
INSERT INTO openrails.webhook_health (merchant_id, rail, last_pull_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(rail)::text, sqlc.arg(at)::timestamptz)
ON CONFLICT (merchant_id, rail) DO UPDATE SET
    last_pull_at = EXCLUDED.last_pull_at,
    updated_at = now();

-- name: ListWebhookExpectedRails :many
-- Expectation gate for the webhook_silence template: rails that are ARMED
-- (declared in psps; archived rows count — drain accounts
-- still receive provider events, #655) AND carry subscriptions projected to
-- keep billing (billable_subscriptions doctrine). RLS-scoped.
SELECT s.rail, count(*) AS billable
FROM openrails.subscriptions s
JOIN openrails.prices pr ON pr.id = s.price_id
WHERE pr.auto_renew
  AND s.deleted_at IS NULL
  AND s.status IN ('pending','active','past_due','unknown')
  AND s.cancelled_at IS NULL
  AND s.deletion_scheduled_at IS NULL
  AND EXISTS (
      SELECT 1 FROM openrails.psps rma
      WHERE rma.merchant_id = s.merchant_id AND rma.rail = s.rail
  )
GROUP BY s.rail;
