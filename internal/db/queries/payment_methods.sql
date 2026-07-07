-- openrails.payment_methods.

-- name: CreatePaymentMethod :execrows
INSERT INTO openrails.payment_methods (
    id, merchant_id, customer_id, rail, rail_customer_ref, rail_method_ref,
    initial_transaction_id, last_four, card_type, expiry_date,
    metadata, created_at, updated_at, rail_merchant_account_id, rebill_driver
) VALUES (
    $1, sqlc.arg(merchant_id)::uuid, $2, $3, sqlc.arg(rail_customer_ref), sqlc.arg(rail_method_ref),
    sqlc.arg(initial_transaction_id), sqlc.narg(last_four), sqlc.narg(card_type), sqlc.narg(expiry_date),
    sqlc.narg(metadata),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    sqlc.narg(rail_merchant_account_id),
    COALESCE(NULLIF(sqlc.arg(rebill_driver)::text, ''), 'provider')
);

-- name: GetPaymentMethodByID :one
SELECT * FROM openrails.payment_methods WHERE id = $1;

-- name: ListPaymentMethodsByIDs :many
SELECT * FROM openrails.payment_methods WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: DeletePaymentMethod :execrows
DELETE FROM openrails.payment_methods WHERE id = $1;

-- name: ListPaymentMethodsByCustomer :many
SELECT * FROM openrails.payment_methods pm
WHERE pm.customer_id = $1
ORDER BY pm.created_at DESC;

-- name: CountPaymentMethodsByCustomer :one
SELECT count(*) FROM openrails.payment_methods pm
WHERE pm.customer_id = $1;

-- name: ListPaymentMethodsByCustomerPaged :many
SELECT * FROM openrails.payment_methods pm
WHERE pm.customer_id = $1
ORDER BY pm.created_at DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0) OFFSET sqlc.arg(page_offset)::int;

-- name: GetPaymentMethodByRailMethodRef :one
SELECT * FROM openrails.payment_methods pm
WHERE pm.rail = $1 AND pm.rail_method_ref = $2
LIMIT 1;

-- name: UpdatePaymentMethod :execrows
UPDATE openrails.payment_methods SET
    customer_id = $2,
    rail = $3,
    rail_customer_ref = sqlc.arg(rail_customer_ref),
    rail_method_ref = sqlc.arg(rail_method_ref),
    initial_transaction_id = sqlc.arg(initial_transaction_id),
    last_four = sqlc.narg(last_four),
    card_type = sqlc.narg(card_type),
    expiry_date = sqlc.narg(expiry_date),
    metadata = sqlc.narg(metadata),
    updated_at = sqlc.arg(updated_at)
WHERE id = $1;

-- name: ListPaymentMethodsByRails :many
SELECT * FROM openrails.payment_methods pm
WHERE pm.rail = ANY(sqlc.arg(rails)::text[])
ORDER BY pm.created_at DESC;

-- name: ListPaymentMethodsByCustomerRails :many
SELECT * FROM openrails.payment_methods pm
WHERE pm.customer_id = $1
  AND pm.rail = ANY(sqlc.arg(rails)::text[])
ORDER BY pm.created_at DESC;

-- name: CountPaymentMethodForUser :one
SELECT count(*) FROM openrails.payment_methods pm
WHERE pm.id = $1 AND pm.customer_id = $2;

-- name: ListPaymentMethodsByRail :many
SELECT * FROM openrails.payment_methods pm
WHERE pm.rail = $1
ORDER BY pm.created_at DESC;

-- name: ListLatestChargeByPaymentMethodIDs :many
-- #589 derived health: the most recent charge (purchased_at + status) per payment
-- method, derived TRANSITIVELY via the subscription link — payments carry no direct
-- payment_method_id yet (option a; option b = a payments.payment_method_id column,
-- deferred). Source is the durable openrails.payments ledger, never provider_intents.
SELECT DISTINCT ON (s.payment_method_id)
    s.payment_method_id AS payment_method_id,
    p.purchased_at      AS purchased_at,
    p.status            AS status
FROM openrails.subscriptions s
JOIN openrails.payments p ON p.subscription_id = s.id
WHERE s.payment_method_id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY s.payment_method_id, p.purchased_at DESC;

-- name: CountPaymentMethodsSharingCustomerRef :one
-- #682 shared-vault guard: how many OTHER stored methods share this rail
-- customer-scope handle (e.g. an imported multi-card NMI vault). RLS scopes to
-- the merchant.
SELECT count(*) FROM openrails.payment_methods
WHERE rail = $1
  AND rail_customer_ref = sqlc.arg(rail_customer_ref)
  AND rail_customer_ref <> ''
  AND id <> sqlc.arg(exclude_id);

-- name: GetPaymentMethodByRailInstrument :one
-- #297: resolve the stored instrument for a charge that only knows the rail
-- handles (checkout intents carry vault/billing ids, not the local row id).
-- One-vault-per-card minting (#682) makes rail_customer_ref alone decisive for
-- NMI; the method-ref predicate narrows within shared (imported) vaults and is
-- skipped when either side has no billing id.
SELECT * FROM openrails.payment_methods pm
WHERE pm.merchant_id = sqlc.arg(merchant_id)
  AND pm.rail = sqlc.arg(rail)
  AND pm.rail_customer_ref = sqlc.arg(rail_customer_ref)
  AND (pm.rail_method_ref = sqlc.arg(rail_method_ref)
       OR sqlc.arg(rail_method_ref)::text = ''
       OR pm.rail_method_ref = '')
ORDER BY pm.created_at
LIMIT 1;

-- name: CaptureStoredCredentialRef :execrows
-- #297: persist the rail-scoped stored-credential replay reference captured by
-- a successful charge, WRITE-ONCE per agreement type — an existing non-empty
-- reference is never overwritten (the sequence anchors on its first capture).
UPDATE openrails.payment_methods SET
    stored_credential_recurring_ref = CASE
        WHEN sqlc.arg(agreement)::text = 'recurring' THEN sqlc.arg(ref)::text
        ELSE stored_credential_recurring_ref END,
    stored_credential_unscheduled_ref = CASE
        WHEN sqlc.arg(agreement)::text = 'unscheduled' THEN sqlc.arg(ref)::text
        ELSE stored_credential_unscheduled_ref END,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)
  AND id = sqlc.arg(id)
  AND ((sqlc.arg(agreement)::text = 'recurring' AND stored_credential_recurring_ref = '')
    OR (sqlc.arg(agreement)::text = 'unscheduled' AND stored_credential_unscheduled_ref = ''));

-- name: CaptureStoredCredentialRefByRailInstrument :execrows
-- #297: instrument-handle variant of CaptureStoredCredentialRef for charge
-- sites that never load the local row (checkout sale/subscription intents).
-- Same write-once semantics.
UPDATE openrails.payment_methods SET
    stored_credential_recurring_ref = CASE
        WHEN sqlc.arg(agreement)::text = 'recurring' THEN sqlc.arg(ref)::text
        ELSE stored_credential_recurring_ref END,
    stored_credential_unscheduled_ref = CASE
        WHEN sqlc.arg(agreement)::text = 'unscheduled' THEN sqlc.arg(ref)::text
        ELSE stored_credential_unscheduled_ref END,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)
  AND rail = sqlc.arg(rail)
  AND rail_customer_ref = sqlc.arg(rail_customer_ref)
  AND (rail_method_ref = sqlc.arg(rail_method_ref)
       OR sqlc.arg(rail_method_ref)::text = ''
       OR rail_method_ref = '')
  AND ((sqlc.arg(agreement)::text = 'recurring' AND stored_credential_recurring_ref = '')
    OR (sqlc.arg(agreement)::text = 'unscheduled' AND stored_credential_unscheduled_ref = ''));
