-- billing.mandates: stored-credential agreements and their network lineage.

-- name: InsertMandate :one
-- The brand is the card's as the agreement is made. The storing attempt is the
-- one the provider answered with the initial transaction id on the mandate's
-- gateway account, when it is retained.
INSERT INTO billing.mandates (
    merchant_id, customer_id, payment_method_id, psp_id, rail, kind, subscription_id, currency,
    status, card_brand, initial_transaction_id, network_transaction_id, transaction_link_id,
    storing_attempt_id, accepted_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(payment_method_id)::uuid,
    sqlc.arg(psp_id)::uuid, sqlc.arg(rail)::text, sqlc.arg(kind)::text, sqlc.narg(subscription_id)::uuid,
    sqlc.narg(currency)::text, 'active',
    (SELECT NULLIF(pm.card_brand, '') FROM billing.payment_methods pm
     WHERE pm.merchant_id = sqlc.arg(merchant_id)::uuid AND pm.id = sqlc.arg(payment_method_id)::uuid),
    sqlc.narg(initial_transaction_id)::text,
    sqlc.narg(network_transaction_id)::text, sqlc.narg(transaction_link_id)::text,
    (SELECT a.id FROM billing.payment_attempts a
     WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.psp_id = sqlc.arg(psp_id)::uuid
       AND a.transaction_id = sqlc.narg(initial_transaction_id)::text),
    sqlc.arg(accepted_at)::timestamptz
)
RETURNING *;

-- name: GetMandateForShare :one
-- A charge rechecks the mandate it froze under a shared lock, which conflicts
-- with ending or revoking it.
SELECT * FROM billing.mandates
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
FOR SHARE;

-- name: GetLiveRecurringMandateForShare :one
-- The subscription's agreement, active or waiting for consent.
SELECT * FROM billing.mandates
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_id = sqlc.arg(subscription_id)::uuid
  AND kind = 'recurring' AND status IN ('active', 'requires_reconsent')
FOR SHARE;

-- name: GetLiveUnscheduledMandateForShare :one
-- The customer's collection agreement in one currency.
SELECT * FROM billing.mandates
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text
  AND kind = 'unscheduled' AND status IN ('active', 'requires_reconsent')
FOR SHARE;

-- name: GetLiveCardOnFileMandate :one
SELECT * FROM billing.mandates
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND payment_method_id = sqlc.arg(payment_method_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND kind = 'card_on_file' AND status IN ('active', 'requires_reconsent');

-- name: GetCitableMandateForShare :one
-- The lineage a new agreement or a customer-present charge on the card cites:
-- the earliest active mandate of one of kinds with references on this
-- customer's card at this gateway account.
SELECT * FROM billing.mandates
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND payment_method_id = sqlc.arg(payment_method_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
  AND kind = ANY (sqlc.arg(kinds)::text[]) AND status = 'active' AND initial_transaction_id IS NOT NULL
ORDER BY created_at, id
LIMIT 1
FOR SHARE;

-- name: SetMandateLineage :execrows
-- A mandate made before its first storing transaction takes that
-- transaction's references, once.
UPDATE billing.mandates SET
    initial_transaction_id = sqlc.arg(initial_transaction_id)::text,
    network_transaction_id = sqlc.narg(network_transaction_id)::text,
    transaction_link_id = sqlc.narg(transaction_link_id)::text,
    storing_attempt_id = (SELECT a.id FROM billing.payment_attempts a
        WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.psp_id = mandates.psp_id
          AND a.transaction_id = sqlc.arg(initial_transaction_id)::text),
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND status = 'active' AND initial_transaction_id IS NULL;

-- name: EndMandate :execrows
UPDATE billing.mandates SET
    status = CASE WHEN sqlc.arg(reason)::text = 'customer_revoked' THEN 'revoked' ELSE 'ended' END,
    end_reason = sqlc.arg(reason)::text, ended_at = sqlc.arg(now)::timestamptz, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND status IN ('active', 'requires_reconsent');

-- name: RequireMandatesReconsent :execrows
-- A card reissued under another brand carries no network lineage: its active
-- agreements wait for the customer's fresh consent.
UPDATE billing.mandates SET status = 'requires_reconsent', updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND payment_method_id = sqlc.arg(payment_method_id)::uuid
  AND status = 'active';

-- name: EndPaymentMethodMandates :many
-- Every live agreement on one card ends together, returning what ended. A
-- caller without a business clock ends them at the database's time.
UPDATE billing.mandates SET
    status = CASE WHEN sqlc.arg(reason)::text = 'customer_revoked' THEN 'revoked' ELSE 'ended' END,
    end_reason = sqlc.arg(reason)::text, ended_at = COALESCE(sqlc.narg(now)::timestamptz, now()), updated_at = COALESCE(sqlc.narg(now)::timestamptz, now())
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND payment_method_id = sqlc.arg(payment_method_id)::uuid
  AND status IN ('active', 'requires_reconsent')
RETURNING *;

-- name: ListCustomerMandatesPage :many
-- One page of a customer's mandates, newest first, after a (created_at, id)
-- cursor.
SELECT * FROM billing.mandates m
WHERE m.merchant_id = sqlc.arg(merchant_id)::uuid AND m.customer_id = sqlc.arg(customer_id)::uuid
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (m.created_at, m.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY m.created_at DESC, m.id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: ListCustomerMandatesByIDs :many
-- A customer's named mandates, newest first.
SELECT * FROM billing.mandates
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND id = ANY (sqlc.arg(ids)::uuid[])
ORDER BY created_at DESC, id DESC;

-- name: RevokeCardOnFileMandates :many
-- The customer withdrew reuse of one card for one-click buys.
UPDATE billing.mandates SET status = 'revoked', end_reason = 'customer_revoked',
    ended_at = sqlc.arg(now)::timestamptz, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND payment_method_id = sqlc.arg(payment_method_id)::uuid
  AND kind = 'card_on_file' AND status IN ('active', 'requires_reconsent')
RETURNING *;

-- name: ListMandatesAwaitingConsent :many
-- A card's agreements waiting for the customer's fresh consent, oldest first.
SELECT * FROM billing.mandates
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND payment_method_id = sqlc.arg(payment_method_id)::uuid
  AND status = 'requires_reconsent'
ORDER BY created_at, id
FOR UPDATE;

-- name: ListLiveMandatesOfPaymentMethods :many
-- The live agreements on a customer's named cards, oldest first.
SELECT * FROM billing.mandates
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND payment_method_id = ANY (sqlc.arg(payment_method_ids)::uuid[])
  AND status IN ('active', 'requires_reconsent')
ORDER BY created_at, id;
