-- name: InsertRailMutationLog :exec
INSERT INTO openrails.rail_mutation_logs (
    merchant_id,
    rail,
    psp_id,
    custodian_id,
    rail_intent_id,
    intent_type,
    idempotency_key,
    attempt,
    phase,
    reason,
    evidence
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
);

-- Operator read surface (#735: replaced the ClickHouse mirror; this table is
-- the durable mutation log).

-- name: ListRailMutationLogs :many
SELECT * FROM openrails.rail_mutation_logs
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(rail_intent_id)::uuid IS NULL OR rail_intent_id = sqlc.narg(rail_intent_id)::uuid)
  AND (sqlc.narg(psp_id)::uuid IS NULL OR psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(phase)::text IS NULL OR phase = sqlc.narg(phase)::text)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(limit_rows);

-- name: CountRailMutationLogs :one
SELECT count(*) FROM openrails.rail_mutation_logs
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(rail_intent_id)::uuid IS NULL OR rail_intent_id = sqlc.narg(rail_intent_id)::uuid)
  AND (sqlc.narg(psp_id)::uuid IS NULL OR psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(phase)::text IS NULL OR phase = sqlc.narg(phase)::text);
