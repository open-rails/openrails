-- #1120: NMI's own authorization history, monthly per NMI PSP.

-- name: QueueNMIHistoryRead :exec
-- A declared, unarchived NMI PSP's history is due from due_at when none is
-- queued; one parked while it was not is due again. A queued read keeps its
-- time.
INSERT INTO billing.nmi_history_reads AS r (merchant_id, psp_id, due_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(due_at)::timestamptz)
ON CONFLICT (merchant_id, psp_id) DO UPDATE SET due_at = EXCLUDED.due_at
WHERE r.due_at = 'infinity';

-- CROSS-MERCHANT: a page of the NMI history reads due by due_by, by due time,
-- after the cursor: the hourly job's due work, read by its index. live is
-- false for a superseded PSP or a deleted merchant.
-- name: ListDueNMIHistoryReads :many
SELECT r.merchant_id, r.psp_id, r.read_at, r.due_at,
       (p.superseded_at IS NULL AND m.deleted_at IS NULL)::bool AS live
FROM billing.nmi_history_reads r
JOIN billing.psps p ON p.merchant_id = r.merchant_id AND p.id = r.psp_id
JOIN billing.merchants m ON m.id = r.merchant_id
WHERE r.due_at <= sqlc.arg(due_by)::timestamptz
  AND (r.due_at, r.merchant_id, r.psp_id) > (sqlc.arg(after_due_at)::timestamptz, sqlc.arg(after_merchant_id)::uuid, sqlc.arg(after_psp_id)::uuid)
ORDER BY r.due_at, r.merchant_id, r.psp_id
LIMIT sqlc.arg(row_limit)::int;

-- name: ParkNMIHistoryRead :exec
-- A PSP that is not live waits until its configuration declares it again.
UPDATE billing.nmi_history_reads SET due_at = 'infinity'
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid;

-- name: QueueRestoredNMIHistory :exec
-- A restored merchant's current NMI PSPs are due at once: the source's read
-- progress stays there, so each backfills its own.
INSERT INTO billing.nmi_history_reads (merchant_id, psp_id, due_at)
SELECT p.merchant_id, p.id, sqlc.arg(now)::timestamptz
FROM billing.psps p
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.rail = 'nmi' AND p.superseded_at IS NULL
ON CONFLICT (merchant_id, psp_id) DO NOTHING;

-- name: DeleteNMIHistoryMonthsFrom :exec
-- A read replaces every month from its first.
DELETE FROM billing.nmi_history_months
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
  AND month_at >= sqlc.arg(since)::timestamptz;

-- name: InsertNMIHistoryMonths :exec
INSERT INTO billing.nmi_history_months (merchant_id, psp_id, month_at, kind, reason, authorizations, outcome, decline_cause)
SELECT sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, c.month_at, c.kind, c.reason, c.authorizations, c.outcome, NULLIF(c.decline_cause, '')
FROM unnest(sqlc.arg(months)::timestamptz[], sqlc.arg(kinds)::text[], sqlc.arg(reasons)::text[],
    sqlc.arg(counts)::bigint[], sqlc.arg(outcomes)::text[], sqlc.arg(decline_causes)::text[]) AS c(month_at, kind, reason, authorizations, outcome, decline_cause);

-- name: RecordNMIHistoryRead :exec
-- A read that completed, and when the next is due.
INSERT INTO billing.nmi_history_reads (merchant_id, psp_id, read_at, due_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(read_at)::timestamptz, sqlc.arg(due_at)::timestamptz)
ON CONFLICT (merchant_id, psp_id) DO UPDATE SET read_at = EXCLUDED.read_at, due_at = EXCLUDED.due_at;

-- name: DeleteNMIHistoryMonthsBefore :execrows
-- #1120 retention: history months past the attempt retention.
DELETE FROM billing.nmi_history_months
WHERE (merchant_id, psp_id, month_at, kind, reason) IN (
    SELECT h.merchant_id, h.psp_id, h.month_at, h.kind, h.reason
    FROM billing.nmi_history_months h
    WHERE h.merchant_id = sqlc.arg(merchant_id)::uuid
      AND h.month_at < sqlc.arg(cutoff)::timestamptz
    LIMIT sqlc.arg(row_limit)::int
);
