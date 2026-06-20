-- name: InsertExternalProviderMutationLog :exec
INSERT INTO openrails.external_provider_mutation_logs (
    merchant_id,
    provider,
    provider_account_id,
    provider_intent_id,
    intent_type,
    idempotency_key,
    attempt,
    phase,
    reason,
    evidence
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
);

-- name: CountExternalProviderMutationLogs :one
SELECT count(*)
FROM openrails.external_provider_mutation_logs
WHERE (sqlc.narg(provider)::text IS NULL OR provider = sqlc.narg(provider)::text)
  AND (sqlc.narg(provider_intent_id)::uuid IS NULL OR provider_intent_id = sqlc.narg(provider_intent_id)::uuid)
  AND (sqlc.narg(provider_account_id)::uuid IS NULL OR provider_account_id = sqlc.narg(provider_account_id)::uuid)
  AND (sqlc.narg(phase)::text IS NULL OR phase = sqlc.narg(phase)::text);

-- name: ListExternalProviderMutationLogs :many
SELECT *
FROM openrails.external_provider_mutation_logs
WHERE (sqlc.narg(provider)::text IS NULL OR provider = sqlc.narg(provider)::text)
  AND (sqlc.narg(provider_intent_id)::uuid IS NULL OR provider_intent_id = sqlc.narg(provider_intent_id)::uuid)
  AND (sqlc.narg(provider_account_id)::uuid IS NULL OR provider_account_id = sqlc.narg(provider_account_id)::uuid)
  AND (sqlc.narg(phase)::text IS NULL OR phase = sqlc.narg(phase)::text)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);
