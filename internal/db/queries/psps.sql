-- billing.psps: merchant-scoped PSP (payment-service-provider account) registry.

-- name: PublishPSP :one
-- A credential publication: the caller holds the row lock and supplies every
-- credential column. The key is set once, on insert.
INSERT INTO billing.psps (
    id, merchant_id, key, rail, environment, account_id, settings,
    credential_custody, credential_refs,
    credential_versions, retired_credentials, credentials_validated_at,
    webhook_endpoint_id, webhook_overlap_expires_at, revision
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(key)::text,
    lower(sqlc.arg(rail)::text), sqlc.arg(environment)::text, sqlc.arg(account_id)::text,
    sqlc.arg(settings)::jsonb, sqlc.narg(credential_custody)::text, sqlc.arg(credential_refs)::jsonb,
    sqlc.arg(credential_versions)::jsonb, sqlc.arg(retired_credentials)::text[],
    sqlc.narg(credentials_validated_at)::timestamptz, sqlc.narg(webhook_endpoint_id)::text,
    sqlc.narg(webhook_overlap_expires_at)::timestamptz, sqlc.arg(revision)::bigint
)
ON CONFLICT (rail, environment, account_id) DO UPDATE SET
    settings = EXCLUDED.settings,
    credential_custody = EXCLUDED.credential_custody,
    credential_refs = EXCLUDED.credential_refs,
    credential_versions = EXCLUDED.credential_versions,
    retired_credentials = EXCLUDED.retired_credentials,
    credentials_validated_at = EXCLUDED.credentials_validated_at,
    webhook_endpoint_id = EXCLUDED.webhook_endpoint_id,
    webhook_overlap_expires_at = EXCLUDED.webhook_overlap_expires_at,
    revision = EXCLUDED.revision,
    updated_at = now()
WHERE billing.psps.merchant_id = EXCLUDED.merchant_id
RETURNING *;

-- name: UpsertManifestPSP :one
-- A manifest declaration overwrites the declared fields and resets credential
-- publication state to the startup snapshot.
INSERT INTO billing.psps (
    id, merchant_id, key, rail, environment, account_id, archived, archived_at,
    custodian_id, settings, signer, credential_custody
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(key)::text,
    lower(sqlc.arg(rail)::text), sqlc.arg(environment)::text, sqlc.arg(account_id)::text,
    sqlc.arg(archived)::boolean, CASE WHEN sqlc.arg(archived)::boolean THEN now() END,
    sqlc.narg(custodian_id)::uuid, sqlc.arg(settings)::jsonb, sqlc.narg(signer)::jsonb, 'snapshot'
)
ON CONFLICT (rail, environment, account_id) DO UPDATE SET
    key = EXCLUDED.key,
    archived = EXCLUDED.archived,
    archived_at = CASE WHEN EXCLUDED.archived THEN COALESCE(billing.psps.archived_at, now()) END,
    -- Custody is declarative: a re-apply that no longer names a custodian
    -- un-arms the arrangement.
    custodian_id = EXCLUDED.custodian_id,
    settings = EXCLUDED.settings,
    signer = EXCLUDED.signer,
    credential_custody = 'snapshot',
    credential_refs = '{}',
    credential_versions = '{}',
    retired_credentials = '{}',
    credentials_validated_at = NULL,
    webhook_endpoint_id = NULL,
    webhook_overlap_expires_at = NULL,
    revision = billing.psps.revision + 1,
    updated_at = now()
WHERE billing.psps.merchant_id = EXCLUDED.merchant_id
RETURNING *;

-- name: GetPSP :one
SELECT * FROM billing.psps
WHERE psps.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1;

-- name: GetPSPByIdentity :one
SELECT * FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND account_id = sqlc.arg(account_id)::text
LIMIT 1;

-- name: GetPSPByRailIdentity :one
SELECT * FROM billing.psps
WHERE psps.merchant_id = sqlc.arg(merchant_id)::uuid AND rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND account_id = sqlc.arg(account_id)::text
LIMIT 1;

-- name: ListPSPsForMerchant :many
-- Every PSP of the merchant, both environments and archived ones included.
SELECT * FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY rail, environment, created_at, id;

-- name: ListPSPs :many
-- One page of the merchant's PSPs in its environment, newest first.
SELECT * FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND environment = sqlc.arg(environment)::text
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(archived)::boolean IS NULL OR archived = sqlc.narg(archived)::boolean)
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: GetActivePSPForNewWork :one
-- The newest non-archived account on a rail+environment. Existing provider-bound
-- work must use its recorded psp_id instead of this selector.
SELECT * FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND archived = false
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: CountActivePSPsForNewWork :one
SELECT count(*)::bigint FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND archived = false;

-- name: CountPSPsForRailEnvironment :one
SELECT count(*)::bigint FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live');

-- CROSS-MERCHANT: PSP ownership by the global (rail, environment, account_id)
-- natural key, for webhook routing and the uniqueness preflight, both of which
-- run before any merchant context exists.
-- name: ResolvePSPOwnerByRailIdentity :one
SELECT id, merchant_id, rail, environment, account_id
FROM billing.psps
WHERE rail = lower(sqlc.arg(rail)::text)
  AND environment = COALESCE(sqlc.narg(environment)::text, 'live')
  AND account_id = sqlc.arg(account_id)::text;

-- CROSS-MERCHANT: an ordered page of merchants after the cursor, armed on at
-- least one of the named rails (live PSP, undeleted merchant). Ids only; each
-- merchant's PSP rows are read inside its own scope.
-- name: ListRailArmedMerchants :many
SELECT DISTINCT p.merchant_id
FROM billing.psps p
JOIN billing.merchants m ON m.id = p.merchant_id
WHERE p.rail = ANY(sqlc.arg(rails)::text[])
  AND p.archived = false
  AND m.deleted_at IS NULL
  AND (sqlc.narg(after_merchant_id)::uuid IS NULL OR p.merchant_id > sqlc.narg(after_merchant_id)::uuid)
ORDER BY p.merchant_id
LIMIT sqlc.arg(merchant_limit)::int;

-- One merchant's live PSPs on a rail, read inside that merchant's scope (the
-- second leg of the ListRailArmedMerchants fan-out).
-- name: ListLivePSPsForRail :many
SELECT id, merchant_id, rail, environment, account_id, key
FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = sqlc.arg(rail)::text
  AND archived = false
ORDER BY account_id;

-- or#880: the custody sibling moved to internal/db/queries/custodians.sql
-- (ResolveCustodianOwnerByIdentity). Custody identity is the CUSTODIAN's, not
-- a PSP's — and one custodian may back several PSPs, so "the" PSP was never a
-- well-defined answer.

-- name: GetPSPForCutoverWrite :one
SELECT * FROM billing.psps
WHERE id = sqlc.arg(id)::uuid AND merchant_id = sqlc.arg(merchant_id)::uuid
FOR SHARE;

-- Declaration supplies attribution only. A matching existing account retains
-- its original ID, key, archive state, custody and credentials.
-- name: DeclarePSPIdentity :one
INSERT INTO billing.psps (id, merchant_id, rail, environment, account_id, key)
VALUES (sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(rail)::text,
        sqlc.arg(environment)::text, sqlc.arg(account_id)::text, sqlc.arg(key)::text)
ON CONFLICT (rail, environment, account_id) DO UPDATE SET id=billing.psps.id
WHERE billing.psps.merchant_id=EXCLUDED.merchant_id
  AND billing.psps.key = EXCLUDED.key
RETURNING *;

-- name: SetPSPPendingSigner :execrows
-- #1101: record the unapproved public key a changed Transit signer reports.
UPDATE billing.psps
SET pending_signer_public_key = sqlc.arg(public_key)::text,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id = sqlc.arg(id)::uuid;

-- name: ApprovePSPPendingSigner :execrows
-- #1101: the operator approval is the only writer that clears a pending
-- signer. The stored identity drains; the approved one is provisioned next.
UPDATE billing.psps
SET pending_signer_public_key = NULL,
    archived = true,
    archived_at = COALESCE(archived_at, now()),
    revision = revision + 1,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id = sqlc.arg(id)::uuid
  AND pending_signer_public_key = sqlc.arg(public_key)::text;

-- name: MerchantHasPSPs :one
SELECT EXISTS (SELECT 1 FROM billing.psps WHERE merchant_id = sqlc.arg(merchant_id)::uuid);

-- name: ArchivedPSPKeyExists :one
SELECT EXISTS (
    SELECT 1 FROM billing.psps
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND lower(key) = lower(sqlc.arg(key)::text)
      AND environment = sqlc.arg(environment)::text AND archived = true
);

-- name: GetActivePSPByKey :one
SELECT * FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND lower(key) = lower(sqlc.arg(key)::text)
  AND environment = sqlc.arg(environment)::text AND NOT archived;

-- name: ListActivePSPsForRailEnvironment :many
SELECT * FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND rail = lower(sqlc.arg(rail)::text)
  AND environment = sqlc.arg(environment)::text AND archived = false
ORDER BY created_at DESC, id DESC;

-- name: ListActivePSPsForEnvironment :many
SELECT * FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND environment = sqlc.arg(environment)::text AND archived = false
ORDER BY rail ASC, created_at DESC, id DESC;

-- Archived included: the drain-pull leg (#699).
-- name: GetNewestPSPForRail :one
SELECT * FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND rail = lower(sqlc.arg(rail)::text)
  AND environment = sqlc.arg(environment)::text
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: GetPSPIDByRailAccount :one
SELECT id FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND rail = lower(sqlc.arg(rail)::text)
  AND account_id = sqlc.arg(account_id)::text
LIMIT 1;

-- name: GetPSPEnvironmentForRail :one
SELECT environment FROM billing.psps
WHERE id = sqlc.arg(id)::uuid AND merchant_id = sqlc.arg(merchant_id)::uuid AND rail = sqlc.arg(rail)::text;

-- One lock order so concurrent archives serialize instead of deadlocking.
-- name: LockPSPsForRailEnvironment :many
SELECT * FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND rail = sqlc.arg(rail)::text
  AND environment = sqlc.arg(environment)::text
ORDER BY created_at, id
FOR UPDATE;

-- name: ArchivePSP :one
UPDATE billing.psps
SET archived = true,
    archived_at = now(),
    revision = revision + 1,
    updated_at = now()
WHERE id = sqlc.arg(id)::uuid AND merchant_id = sqlc.arg(merchant_id)::uuid
RETURNING *;

-- name: CountPSPOpenObligations :many
SELECT psp.id AS psp_id,
       ((SELECT count(*) FROM billing.subscriptions sub
          WHERE sub.merchant_id = psp.merchant_id AND sub.psp_id = psp.id
            AND sub.status IN ('active', 'pending', 'past_due') AND sub.deleted_at IS NULL)
      + (SELECT count(*) FROM billing.payments payment
          WHERE payment.merchant_id = psp.merchant_id AND payment.psp_id = psp.id
            AND payment.status = 'pending' AND payment.deleted_at IS NULL)
      + (SELECT count(*) FROM billing.provider_intents intent
          WHERE intent.merchant_id = psp.merchant_id AND intent.psp_id = psp.id
            AND intent.status IN ('pending', 'in_flight', 'failed_retryable', 'unknown_needs_verify'))
       )::bigint AS open_obligations
FROM billing.psps psp
WHERE psp.merchant_id = sqlc.arg(merchant_id)::uuid AND psp.id = ANY(sqlc.arg(psp_ids)::uuid[]);
