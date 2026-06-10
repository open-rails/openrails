-- billing.linked_wallets.

-- name: GetLinkedWalletByTenantSubjectAndChain :one
SELECT * FROM billing.linked_wallets lw
WHERE lw.tenant_subject_id = $1 AND lw.chain = $2
LIMIT 1;

-- name: UpsertLinkedWallet :one
INSERT INTO billing.linked_wallets (
    id, tenant_id, tenant_subject_id, chain, address, verification_provider,
    verified_at, display_name, metadata, created_at, updated_at
) VALUES (
    $1,
    COALESCE(NULLIF(sqlc.arg(tenant_id)::uuid, '00000000-0000-0000-0000-000000000000'::uuid),
             '00000000-0000-0000-0000-000000000001'::uuid),
    $2, $3, $4, $5, $6, sqlc.narg(display_name), sqlc.arg(metadata),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
)
ON CONFLICT (tenant_id, tenant_subject_id, chain) DO UPDATE SET
    address = EXCLUDED.address,
    verification_provider = EXCLUDED.verification_provider,
    verified_at = EXCLUDED.verified_at,
    display_name = EXCLUDED.display_name,
    metadata = EXCLUDED.metadata,
    updated_at = now()
RETURNING *;

-- name: DeleteLinkedWalletByTenantSubjectAndChain :execrows
DELETE FROM billing.linked_wallets
WHERE tenant_subject_id = $1 AND chain = $2;
