-- openrails.custody_migrations + the payment_methods writes that move an
-- instrument between custodians (or#297 Phase C).

-- name: LockPaymentMethodForCustodyRemap :one
-- The flip is atomic per instrument: take the row lock first so a concurrent
-- charge site reading the same instrument cannot straddle the custody change.
SELECT * FROM openrails.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id = sqlc.arg(id)::uuid
FOR UPDATE;

-- name: CountInFlightChargeIntentsForPaymentMethod :one
-- or#297 Phase C refusal predicate: NO charge may straddle the flip. An intent
-- that is in_flight is being executed right now; one parked as
-- unknown_needs_verify was SENT and its outcome is not yet known. Either way
-- the money-mover is mid-attempt against the PSP vault, and moving custody
-- underneath it would leave the verifier resolving an attempt whose instrument
-- no longer describes how the charge was made. Both states clear on their own
-- (the executor finishes, the verifier resolves), so this is a "come back
-- later", not a failure.
SELECT count(*)::bigint FROM openrails.rail_intents ri
JOIN openrails.subscriptions s ON s.id = ri.subscription_id
WHERE ri.merchant_id = sqlc.arg(merchant_id)::uuid
  AND s.payment_method_id = sqlc.arg(payment_method_id)::uuid
  AND s.deleted_at IS NULL
  AND ri.status = ANY (ARRAY['in_flight'::text, 'unknown_needs_verify'::text]);

-- name: RemapPaymentMethodCustody :execrows
-- The custody flip. What moves: who holds the card (custodian), the handle that
-- addresses it there (rail_method_ref), the custodian's fingerprint, the charge
-- transport, and the PSP that settles it — a deplatformed merchant's whole
-- point is that this may be a DIFFERENT gateway account.
--
-- What deliberately does NOT move:
--   * id — subscriptions reference the instrument, so they never notice;
--   * rail — the proxy changes how the card reaches the gateway, never which
--     gateway kind charges it (or#879);
--   * rail_customer_ref — the old PSP vault handle stays on the row. It is
--     dead as an address the moment custody changes, and it is the only
--     forensic link to charges that settled before the flip;
--   * stored_credential_*_ref — the network's credential-on-file sequence
--     anchors are gateway-scoped, not custody-scoped. Clearing them would
--     restart every stored-credential sequence for no reason.
--
-- Guarded on the CURRENT custody so a concurrent second flip cannot apply
-- twice: the WHERE clause is the compare-and-swap.
UPDATE openrails.payment_methods SET
    custodian = sqlc.arg(to_custodian)::text,
    rail_method_ref = sqlc.arg(to_rail_method_ref)::text,
    fingerprint = COALESCE(NULLIF(sqlc.arg(fingerprint)::text, ''), fingerprint),
    charge_via = COALESCE(NULLIF(sqlc.arg(charge_via)::text, ''), 'pan_proxy'),
    network_token_id = sqlc.arg(network_token_id)::text,
    network_token_status = sqlc.arg(network_token_status)::text,
    network_token_par = sqlc.arg(network_token_par)::text,
    psp_id = COALESCE(sqlc.narg(to_psp_id)::uuid, psp_id),
    rebill_driver = 'openrails',
    last_four = COALESCE(NULLIF(sqlc.arg(last_four)::text, ''), last_four),
    card_type = COALESCE(NULLIF(sqlc.arg(card_type)::text, ''), card_type),
    expiry_date = COALESCE(NULLIF(sqlc.arg(expiry_date)::text, ''), expiry_date),
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id = sqlc.arg(id)::uuid
  AND custodian = sqlc.arg(from_custodian)::text;

-- name: RecordCustodyMigration :one
INSERT INTO openrails.custody_migrations (
    merchant_id, batch_id, payment_method_id, rail,
    from_custodian, from_custodian_id, from_rail_customer_ref, from_rail_method_ref, from_psp_id,
    to_custodian, to_custodian_id, to_rail_method_ref, to_psp_id,
    exported_at, outcome, reason
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(batch_id)::uuid, sqlc.arg(payment_method_id)::uuid, sqlc.arg(rail)::text,
    sqlc.arg(from_custodian)::text, sqlc.narg(from_custodian_id)::uuid,
    sqlc.arg(from_rail_customer_ref)::text, sqlc.arg(from_rail_method_ref)::text, sqlc.narg(from_psp_id)::uuid,
    sqlc.arg(to_custodian)::text, sqlc.arg(to_custodian_id)::uuid, sqlc.arg(to_rail_method_ref)::text,
    sqlc.narg(to_psp_id)::uuid,
    sqlc.narg(exported_at)::timestamptz, sqlc.arg(outcome)::text, sqlc.arg(reason)::text
)
RETURNING *;

-- name: GetCustodyMigrationForTarget :one
-- Idempotency read: has THIS instrument already reached THIS custodian token?
SELECT * FROM openrails.custody_migrations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND payment_method_id = sqlc.arg(payment_method_id)::uuid
  AND to_rail_method_ref = sqlc.arg(to_rail_method_ref)::text;

-- name: ListCustodyMigrationsForBatch :many
-- The operator's after-the-fact report for one run.
SELECT * FROM openrails.custody_migrations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND batch_id = sqlc.arg(batch_id)::uuid
ORDER BY created_at, id;

-- name: GetPaymentMethodForCustodianToken :one
-- Conflict guard: a custodian token addresses exactly one instrument. If the
-- export maps two source vault entries onto one token (or onto a token another
-- instrument already holds), the second is refused rather than silently
-- pointing two instruments at one card.
SELECT * FROM openrails.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND custodian = sqlc.arg(custodian)::text
  AND rail_method_ref = sqlc.arg(rail_method_ref)::text
LIMIT 1;
