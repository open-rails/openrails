-- billing.payment_method_versions: the history of the card behind a method.

-- name: InsertPaymentMethodVersion :execrows
-- One row per method, source and event; a replay inserts nothing.
INSERT INTO billing.payment_method_versions (
    merchant_id, customer_id, payment_method_id, source, kind, event_ref, psp_id, custodian_id,
    rail_customer_ref, rail_method_ref, card_brand, card_last4, card_exp_month, card_exp_year, fingerprint,
    network_token_id, network_token_status, network_token_par, effective_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(payment_method_id)::uuid,
    sqlc.arg(source)::text, sqlc.arg(kind)::text, sqlc.arg(event_ref)::text,
    sqlc.narg(psp_id)::uuid, sqlc.narg(custodian_id)::uuid,
    sqlc.narg(rail_customer_ref)::text, sqlc.narg(rail_method_ref)::text,
    sqlc.narg(card_brand)::text, sqlc.narg(card_last4)::text,
    sqlc.narg(card_exp_month)::smallint, sqlc.narg(card_exp_year)::smallint, sqlc.narg(fingerprint)::text,
    sqlc.narg(network_token_id)::text, sqlc.narg(network_token_status)::text, sqlc.narg(network_token_par)::text,
    sqlc.arg(effective_at)::timestamptz
)
ON CONFLICT (merchant_id, payment_method_id, source, event_ref) DO NOTHING;

-- name: PaymentMethodVersionRecorded :one
-- Whether a source's event already wrote a version of the method.
SELECT EXISTS (
    SELECT 1 FROM billing.payment_method_versions
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND payment_method_id = sqlc.arg(payment_method_id)::uuid
      AND source = sqlc.arg(source)::text AND event_ref = sqlc.arg(event_ref)::text
);

-- name: ListPaymentMethodVersions :many
-- A method's history, oldest first.
SELECT * FROM billing.payment_method_versions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND payment_method_id = sqlc.arg(payment_method_id)::uuid
ORDER BY effective_at, id;

-- name: SetPaymentMethodCard :execrows
-- The one write of a method's card, holder handles and standing: the caller
-- locked the row and records the version in the same transaction.
UPDATE billing.payment_methods SET
    rail_customer_ref = sqlc.narg(rail_customer_ref)::text,
    rail_method_ref = sqlc.narg(rail_method_ref)::text,
    card_brand = sqlc.narg(card_brand)::text,
    card_last4 = sqlc.narg(card_last4)::text,
    card_exp_month = sqlc.narg(card_exp_month)::smallint,
    card_exp_year = sqlc.narg(card_exp_year)::smallint,
    fingerprint = sqlc.narg(fingerprint)::text,
    network_token_id = sqlc.narg(network_token_id)::text,
    network_token_status = sqlc.narg(network_token_status)::text,
    network_token_par = sqlc.narg(network_token_par)::text,
    status = sqlc.arg(status)::text,
    contact_cardholder_at = sqlc.narg(contact_cardholder_at)::timestamptz,
    park_reason = sqlc.narg(park_reason)::text,
    parked_at = sqlc.narg(parked_at)::timestamptz,
    updated_at = sqlc.arg(updated_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: ListPaymentMethodsByCustodianRef :many
-- The methods a custodian token names, live or not.
SELECT id FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND custodian <> 'psp'
  AND custodian_id = sqlc.arg(custodian_id)::uuid AND custodian = sqlc.arg(custodian)::text
  AND rail_method_ref = sqlc.arg(rail_method_ref)::text
ORDER BY created_at, id;

-- name: ListPaymentMethodsByNetworkToken :many
-- The methods a custodian's network token stands in for.
SELECT id FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND custodian <> 'psp'
  AND custodian_id = sqlc.arg(custodian_id)::uuid AND custodian = sqlc.arg(custodian)::text
  AND network_token_id = sqlc.arg(network_token_id)::text
ORDER BY created_at, id;

-- name: RemoveStripePaymentMethodByRef :one
-- A Stripe detach is irreversible provider truth: the exact PSP-held method
-- is removed, its row kept as evidence.
UPDATE billing.payment_methods SET status = 'removed', updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND rail = 'stripe'
  AND psp_id = sqlc.arg(psp_id)::uuid AND rail_method_ref = sqlc.arg(rail_method_ref)::text
  AND status = 'active'
RETURNING id;
