-- billing.psps: merchant-scoped PSP identities. A PSP's configuration
-- (settings, credentials, archived) is its document in a file or Vault; a row
-- holds the identity history points at and what OpenRails discovers.

-- name: RegisterPSP :one
-- Records a PSP document's identity under its natural-key id, current under
-- the document's key, with its declared environment: every non-live
-- environment is one money class, so a declaration moving between them
-- corrects the row in place, except Solana whose network is immutable.
-- An identity another merchant owns answers no row;
-- a key another current PSP of the merchant holds violates psps_key_key.
INSERT INTO billing.psps (id, merchant_id, key, rail, environment, account_id)
VALUES (sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(key)::text,
        lower(sqlc.arg(rail)::text), sqlc.arg(environment)::text, sqlc.arg(account_id)::text)
ON CONFLICT (rail, live, account_id) DO UPDATE SET
    key = EXCLUDED.key,
    environment = EXCLUDED.environment,
    superseded_at = NULL,
    updated_at = now()
WHERE billing.psps.merchant_id = EXCLUDED.merchant_id
  AND (billing.psps.rail <> 'solana' OR billing.psps.environment = EXCLUDED.environment)
RETURNING *;

-- name: PSPHeldBilling :one
-- What needs the PSP's own account: cards in its vault, subscriptions it
-- bills itself, and OpenRails-run subscriptions renewing on it.
SELECT
    (SELECT count(*) FROM billing.payment_methods m
     WHERE m.merchant_id = sqlc.arg(merchant_id)::uuid AND m.psp_id = sqlc.arg(psp_id)::uuid
       AND m.custodian = 'psp' AND m.status IN ('active', 'requires_action'))::bigint AS vaulted_methods,
    (SELECT count(*) FROM billing.subscriptions s
     WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.psp_id = sqlc.arg(psp_id)::uuid
       AND s.collection_policy <> 'engine' AND s.status <> 'canceled' AND s.deleted_at IS NULL)::bigint AS provider_subscriptions,
    (SELECT count(*) FROM billing.subscriptions s
     WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.psp_id = sqlc.arg(psp_id)::uuid
       AND s.collection_policy = 'engine' AND s.status <> 'canceled' AND s.deleted_at IS NULL)::bigint AS engine_subscriptions;

-- name: SupersedePSP :exec
-- The PSP's key now names another account: it drains and releases its
-- gateway account's fingerprint.
UPDATE billing.psps
SET superseded_at = COALESCE(superseded_at, now()), credential_fingerprint = NULL, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: GetPSP :one
SELECT * FROM billing.psps
WHERE psps.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1;

-- name: GetPSPByIdentity :one
SELECT * FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail = lower(sqlc.arg(rail)::text)
  AND live = sqlc.arg(live)::bool
  AND account_id = sqlc.arg(account_id)::text
LIMIT 1;

-- name: ListPSPsForMerchant :many
-- Every PSP identity of the merchant, both environments and superseded ones
-- included, oldest first.
SELECT * FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY created_at, id;

-- name: ListPSPsByIDs :many
SELECT * FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY created_at DESC, id DESC;

-- CROSS-MERCHANT: PSP ownership by the global (rail, money class, account_id)
-- natural key, for webhook routing and the uniqueness preflight, both of which
-- run before any merchant context exists.
-- name: ResolvePSPOwnerByRailIdentity :one
SELECT id, merchant_id, rail, environment, account_id
FROM billing.psps
WHERE rail = lower(sqlc.arg(rail)::text)
  AND live = sqlc.arg(live)::bool
  AND account_id = sqlc.arg(account_id)::text;

-- CROSS-MERCHANT: an ordered page of merchants after the cursor holding a
-- current PSP on one of the named rails (undeleted merchant). Ids only; each
-- merchant's PSPs are read inside its own scope, where its configuration says
-- which are archived.
-- name: ListRailArmedMerchants :many
SELECT DISTINCT p.merchant_id
FROM billing.psps p
JOIN billing.merchants m ON m.id = p.merchant_id
WHERE p.rail = ANY(sqlc.arg(rails)::text[])
  AND p.superseded_at IS NULL
  AND m.deleted_at IS NULL
  AND (sqlc.narg(after_merchant_id)::uuid IS NULL OR p.merchant_id > sqlc.narg(after_merchant_id)::uuid)
ORDER BY p.merchant_id
LIMIT sqlc.arg(merchant_limit)::int;

-- name: GetPSPForCutoverWrite :one
SELECT * FROM billing.psps
WHERE id = sqlc.arg(id)::uuid AND merchant_id = sqlc.arg(merchant_id)::uuid
FOR SHARE;

-- Declaration supplies attribution only. A matching existing account keeps
-- its original id, key and discovered state.
-- name: DeclarePSPIdentity :one
INSERT INTO billing.psps (id, merchant_id, rail, environment, account_id, key)
VALUES (sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(rail)::text,
        sqlc.arg(environment)::text, sqlc.arg(account_id)::text, sqlc.arg(key)::text)
ON CONFLICT (rail, live, account_id) DO UPDATE SET id=billing.psps.id
WHERE billing.psps.merchant_id=EXCLUDED.merchant_id
  AND billing.psps.key = EXCLUDED.key
RETURNING *;

-- name: MerchantHasPSPs :one
SELECT EXISTS (SELECT 1 FROM billing.psps WHERE merchant_id = sqlc.arg(merchant_id)::uuid);

-- name: GetPSPIDByRailAccount :one
SELECT id FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND rail = lower(sqlc.arg(rail)::text)
  AND account_id = sqlc.arg(account_id)::text
LIMIT 1;

-- name: GetPSPEnvironmentForRail :one
SELECT environment FROM billing.psps
WHERE id = sqlc.arg(id)::uuid AND merchant_id = sqlc.arg(merchant_id)::uuid AND rail = sqlc.arg(rail)::text;

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

-- name: SetPSPCredentialsValidated :exec
-- When the provider last accepted the PSP's credentials.
UPDATE billing.psps SET credentials_validated_at = sqlc.narg(validated_at)::timestamptz, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: SetPSPWebhookEndpoint :exec
-- The provider webhook endpoint OpenRails manages for the PSP.
UPDATE billing.psps SET webhook_endpoint_id = sqlc.narg(endpoint_id)::text, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: SetPSPCredentialFingerprint :exec
-- Records the keyed fingerprint of the credential naming the PSP's gateway
-- account. The live fingerprint index refuses it when another PSP on the rail
-- already holds it.
UPDATE billing.psps SET credential_fingerprint = sqlc.arg(fingerprint)::text, credential_duplicate_at = NULL
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: ClearPSPCredentialFingerprint :exec
-- An archived PSP releases its gateway account.
UPDATE billing.psps SET credential_fingerprint = NULL, credential_duplicate_at = NULL
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND (credential_fingerprint IS NOT NULL OR credential_duplicate_at IS NOT NULL);

-- name: MarkPSPCredentialDuplicate :exec
-- The PSP declares a gateway account another live PSP already declares.
UPDATE billing.psps SET credential_fingerprint = sqlc.narg(fingerprint)::text,
    credential_duplicate_at = COALESCE(credential_duplicate_at, sqlc.arg(found_at)::timestamptz)
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: ClearPSPCredentialDuplicate :exec
UPDATE billing.psps SET credential_duplicate_at = NULL
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND credential_duplicate_at IS NOT NULL;

