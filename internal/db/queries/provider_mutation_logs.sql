-- name: InsertProviderMutationLog :exec
INSERT INTO billing.provider_mutation_logs (
    merchant_id,
    rail,
    psp_id,
    custodian_id,
    provider_intent_id,
    intent_type,
    idempotency_key,
    attempt,
    phase,
    reason,
    evidence
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
);

-- Operator read surface: this table is the durable mutation log.

-- name: ListProviderMutationLogs :many
SELECT * FROM billing.provider_mutation_logs
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(provider_intent_id)::uuid IS NULL OR provider_intent_id = sqlc.narg(provider_intent_id)::uuid)
  AND (sqlc.narg(psp_id)::uuid IS NULL OR psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(phase)::text IS NULL OR phase = sqlc.narg(phase)::text)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(limit_rows);

-- name: CountProviderMutationLogs :one
SELECT count(*) FROM billing.provider_mutation_logs
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(provider_intent_id)::uuid IS NULL OR provider_intent_id = sqlc.narg(provider_intent_id)::uuid)
  AND (sqlc.narg(psp_id)::uuid IS NULL OR psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(phase)::text IS NULL OR phase = sqlc.narg(phase)::text);
