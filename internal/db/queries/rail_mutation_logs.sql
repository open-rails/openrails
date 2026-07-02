-- name: InsertRailMutationLog :exec
INSERT INTO openrails.rail_mutation_logs (
    merchant_id,
    provider,
    rail_merchant_account_id,
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
