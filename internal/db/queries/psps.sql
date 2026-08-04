-- openrails.psps: merchant-scoped PSP (payment-service-provider account) registry.

-- name: UpsertPSP :one
INSERT INTO openrails.psps (
    id, merchant_id, rail, environment, account_id, key,
    archived, evidence, last_verified_at
) VALUES (
    -- #662: the id column keeps its uuidv7() default. The production write paths
    -- (merchant payment-provider config + manifest bootstrap) supply a
    -- deterministic uuidv5 derived from the (rail, environment, account_id)
    -- natural key via merchants.PSPNaturalKey, so a provider
    -- account has one stable id across environments; any other caller (fixtures,
    -- ad-hoc inserts) omits it (passes the zero uuid) and gets the uuidv7 default.
    -- Mirrors the COALESCE(narg, default) idiom used for `environment` below.
    COALESCE(NULLIF(sqlc.arg(id)::uuid, '00000000-0000-0000-0000-000000000000'::uuid), uuidv7()),
    sqlc.arg(merchant_id)::uuid,
    lower(sqlc.arg(rail)::text),
    COALESCE(sqlc.narg(environment)::text, 'live'),
    sqlc.arg(account_id)::text,
    sqlc.narg(key),
    COALESCE(sqlc.narg(archived)::boolean, false),
    sqlc.narg(evidence),
    sqlc.narg(last_verified_at)::timestamptz
)
ON CONFLICT (rail, environment, account_id) DO UPDATE SET
    key = COALESCE(EXCLUDED.key, openrails.psps.key),
    archived = EXCLUDED.archived,
    replaced_at = CASE
        WHEN EXCLUDED.archived THEN COALESCE(openrails.psps.replaced_at, now())
        ELSE NULL
    END,
    evidence = COALESCE(EXCLUDED.evidence, openrails.psps.evidence),
    last_verified_at = COALESCE(EXCLUDED.last_verified_at, openrails.psps.last_verified_at),
    updated_at = now()
WHERE openrails.psps.merchant_id = EXCLUDED.merchant_id
RETURNING *;

-- name: GetPSP :one
SELECT * FROM openrails.psps
WHERE id = $1;

-- name: GetPSPByIdentity :one
SELECT * FROM openrails.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND account_id = sqlc.arg(account_id)::text
LIMIT 1;

-- name: GetPSPByRailIdentity :one
SELECT * FROM openrails.psps
WHERE rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND account_id = sqlc.arg(account_id)::text
LIMIT 1;

-- name: ListPSPsForMerchant :many
SELECT * FROM openrails.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(rail)::text IS NULL OR rail = lower(sqlc.narg(rail)::text))
ORDER BY rail, environment, archived, created_at, id;

-- name: GetActivePSPForNewWork :one
-- The newest non-archived account on a rail+environment. Existing provider-bound
-- work must use its recorded psp_id instead of this selector.
SELECT * FROM openrails.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND archived = false
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: CountActivePSPsForNewWork :one
SELECT count(*)::bigint FROM openrails.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND archived = false;

-- name: CountPSPsForRailEnvironment :one
SELECT count(*)::bigint FROM openrails.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live');

-- #824: cross-merchant PSP ownership by the GLOBAL (rail, environment,
-- account_id) natural key, for webhook routing and the uniqueness preflight —
-- both of which run BEFORE any merchant context exists. GetPSPByRailIdentity
-- above carries no merchant predicate, so under the RLS-enforcing app role it
-- can only ever return no rows; the SECURITY DEFINER directory function
-- (migration 0016) is the sanctioned way to make that read, and it RAISES
-- rather than returning empty if its definer cannot bypass RLS.
-- name: ResolvePSPOwnerByRailIdentity :one
SELECT id, merchant_id, rail, environment, account_id
FROM openrails.psp_owner_by_identity(
    lower(sqlc.arg(rail)::text),
    COALESCE(sqlc.narg(environment)::text, 'live'),
    sqlc.arg(account_id)::text
);

-- CROSS-MERCHANT: merchants armed on one of the named rails, through migration
-- 0023's SECURITY DEFINER work queue (or#877 B6). The Stripe webhook reconciler
-- used to JOIN merchants to psps on the base pool; psps FORCEs RLS, so the join
-- yielded nothing and the managed endpoint was never registered or
-- version-bumped. Ids only — each merchant's PSP rows are read inside its own
-- scope.
-- name: ListRailArmedMerchants :many
SELECT merchant_id FROM openrails.psp_rail_merchant_ids(
    sqlc.arg(rails)::text[],
    sqlc.arg(merchant_limit)::int);

-- One merchant's live PSPs on a rail, read inside that merchant's scope (the
-- second leg of the ListRailArmedMerchants fan-out).
-- name: ListLivePSPsForRail :many
SELECT id, merchant_id, rail, environment, account_id, key
FROM openrails.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = sqlc.arg(rail)::text
  AND archived = false
ORDER BY account_id;
