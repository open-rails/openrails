-- #1120: NMI's own authorization history, monthly per NMI PSP.

-- name: ListNMIHistoryDuePSPs :many
-- The merchant's live NMI PSPs never read, or last read before due_before,
-- with that read.
SELECT p.id, r.read_at
FROM openrails.psps p
LEFT JOIN openrails.nmi_history_reads r ON r.merchant_id = p.merchant_id AND r.psp_id = p.id
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid
  AND p.rail = 'nmi' AND p.archived = false
  AND (r.read_at IS NULL OR r.read_at < sqlc.arg(due_before)::timestamptz)
ORDER BY p.id
LIMIT sqlc.arg(row_limit)::int;

-- name: DeleteNMIHistoryMonthsFrom :exec
-- A read replaces every month from its first.
DELETE FROM openrails.nmi_history_months
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
  AND month >= sqlc.arg(since)::timestamptz;

-- name: InsertNMIHistoryMonths :exec
INSERT INTO openrails.nmi_history_months (merchant_id, psp_id, month, kind, category, reason, authorizations)
SELECT sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, c.month, c.kind, c.category, c.reason, c.authorizations
FROM unnest(sqlc.arg(months)::timestamptz[], sqlc.arg(kinds)::text[], sqlc.arg(categories)::text[],
    sqlc.arg(reasons)::text[], sqlc.arg(counts)::bigint[]) AS c(month, kind, category, reason, authorizations);

-- name: RecordNMIHistoryRead :exec
INSERT INTO openrails.nmi_history_reads (merchant_id, psp_id, read_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(read_at)::timestamptz)
ON CONFLICT (merchant_id, psp_id) DO UPDATE SET read_at = EXCLUDED.read_at;

-- name: DeleteNMIHistoryMonthsBefore :execrows
-- #1120 retention: history months past the attempt retention.
DELETE FROM openrails.nmi_history_months
WHERE (merchant_id, psp_id, month, kind, category, reason) IN (
    SELECT h.merchant_id, h.psp_id, h.month, h.kind, h.category, h.reason
    FROM openrails.nmi_history_months h
    WHERE h.merchant_id = sqlc.arg(merchant_id)::uuid
      AND h.month < sqlc.arg(cutoff)::timestamptz
    LIMIT sqlc.arg(row_limit)::int
);
