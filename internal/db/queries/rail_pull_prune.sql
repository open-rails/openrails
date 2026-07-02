-- #511 pull-provider --prune: account-bound EXCESS detection + safe deletion.
-- "Excess" = a local row attributed to the pulled rail_merchant_account_id whose
-- provider key is ABSENT from the freshly fetched provider snapshot. Only rows
-- stamped with the provider account are considered; legacy NULL-binding import
-- rows are never pruned (NULL <> arg).

-- name: ListExcessSubscriptionsForRailMerchantAccount :many
SELECT id FROM openrails.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail_merchant_account_id = sqlc.arg(rail_merchant_account_id)::uuid
  AND rail_subscription_id <> ''
  AND (
    COALESCE(cardinality(sqlc.arg(present_ids)::text[]), 0) = 0
    OR rail_subscription_id <> ALL(sqlc.arg(present_ids)::text[])
  );

-- name: ListExcessPaymentsForRailMerchantAccount :many
-- Windowed: only payments inside the pulled [since, until] window are eligible
-- (a snapshot only proves absence within the window it covered).
SELECT id FROM openrails.payments
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND rail_merchant_account_id = sqlc.arg(rail_merchant_account_id)::uuid
  AND (
    COALESCE(cardinality(sqlc.arg(present_txns)::text[]), 0) = 0
    OR transaction_id <> ALL(sqlc.arg(present_txns)::text[])
  )
  AND (sqlc.narg(since)::timestamptz IS NULL OR purchased_at >= sqlc.narg(since)::timestamptz)
  AND (sqlc.narg(until)::timestamptz IS NULL OR purchased_at <= sqlc.narg(until)::timestamptz);

-- name: SubscriptionHasGrant :one
-- A subscription that fed the #514 grant ledger is entangled: it must be
-- retracted through convergence (grant revoke), never row-deleted (that would
-- orphan the grant). Such excess subs are surfaced by prune, not deleted.
SELECT EXISTS(
  SELECT 1 FROM openrails.grants
  WHERE merchant_id = sqlc.arg(merchant_id)::uuid
    AND source_type = 'subscription'
    AND source_id = sqlc.arg(subscription_id)::uuid::text
) AS has_grant;

-- name: PaymentHasProtectedDependents :one
-- A payment is unsafe to hard-delete if it feeds the #514 grant ledger, backs a
-- refund, an admin grant, or a checkout session. Such rows are retracted through
-- convergence (grant revoke), never row-deleted.
SELECT
  EXISTS(SELECT 1 FROM openrails.grants WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND payment_id = sqlc.arg(payment_id)::uuid)
  OR EXISTS(SELECT 1 FROM openrails.payments r WHERE r.merchant_id = sqlc.arg(merchant_id)::uuid AND r.refunded_payment_id = sqlc.arg(payment_id)::uuid)
  OR EXISTS(SELECT 1 FROM openrails.checkout_sessions WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND payment_id = sqlc.arg(payment_id)::uuid)
  AS protected;

-- name: PruneDeleteCheckoutSessionsBySubscription :execrows
DELETE FROM openrails.checkout_sessions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_id = sqlc.arg(subscription_id)::uuid;

-- name: PruneDeleteEntitlementsBySubscription :execrows
DELETE FROM openrails.entitlements
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND source_type = 'subscription' AND source_id = sqlc.arg(subscription_id)::uuid;

-- name: PruneDeleteSubscriptionByID :execrows
DELETE FROM openrails.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: PruneDeletePaymentByID :execrows
DELETE FROM openrails.payments
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;
