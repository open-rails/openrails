-- #657 immutable account cutover admission and finalization.

-- name: GetNMIProviderCutoverSnapshot :one
SELECT s.customer_id, s.rail_subscription_id, s.payment_method_id,
 s.price_id,b.plan_id,pr.currency,pr.amount,pr.access_duration_hours,s.current_period_starts_at,s.current_period_ends_at
 FROM openrails.subscriptions s
 JOIN openrails.payment_methods old ON old.id=s.payment_method_id AND old.merchant_id=s.merchant_id AND old.customer_id=s.customer_id
 JOIN openrails.payment_methods pm ON pm.id=sqlc.arg(target_payment_method_id)::uuid AND pm.merchant_id=s.merchant_id AND pm.customer_id=s.customer_id
 JOIN openrails.psps source ON source.id=s.psp_id AND source.merchant_id=s.merchant_id
 JOIN openrails.psps target ON target.id=pm.psp_id AND target.merchant_id=s.merchant_id
 JOIN openrails.prices pr ON pr.id=s.price_id AND pr.merchant_id=s.merchant_id
 JOIN openrails.price_psp_bindings b ON b.price_id=s.price_id AND b.psp_id=target.id AND b.merchant_id=s.merchant_id
 WHERE s.id=sqlc.arg(subscription_id)::uuid AND s.merchant_id=sqlc.arg(merchant_id)::uuid AND s.deleted_at IS NULL
 AND s.psp_id=sqlc.arg(source_psp_id)::uuid AND pm.psp_id=sqlc.arg(target_psp_id)::uuid AND s.rail='nmi' AND pm.rail='nmi' AND old.psp_id=source.id
 AND source.rail='nmi' AND target.rail='nmi' AND source.environment=target.environment
 AND source.archived AND NOT target.archived AND source.custodian_id IS NULL AND target.custodian_id IS NULL
 AND s.status='active' AND s.scheduled_price_id IS NULL AND s.deletion_scheduled_at IS NULL
 AND pm.custodian='psp' AND COALESCE(pm.park_reason,'')='' AND pm.rebill_driver='provider'
 AND old.custodian='psp' AND old.rebill_driver='provider' AND pr.auto_renew AND NOT pr.archived AND lower(pr.currency)='usd';

-- name: LockProviderCutoverRequest :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));

-- name: LockProviderCutoverPaymentMethods :exec
SELECT id FROM openrails.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (id = sqlc.arg(target_payment_method_id)::uuid OR id = (
    SELECT payment_method_id FROM openrails.subscriptions
    WHERE id = sqlc.arg(subscription_id)::uuid AND merchant_id = sqlc.arg(merchant_id)::uuid
  ))
ORDER BY id FOR UPDATE;

-- name: HasOpenProviderCutoverSubscriptionIntent :one
SELECT EXISTS (
  SELECT 1 FROM openrails.rail_intents
  WHERE merchant_id = sqlc.arg(merchant_id)::uuid
    AND subscription_id = sqlc.arg(subscription_id)::uuid
    AND status NOT IN ('succeeded','failed_terminal','superseded','expired')
);

-- name: HasOpenProviderCutoverPaymentMethodIntent :one
SELECT EXISTS (
  SELECT 1 FROM openrails.rail_intents
  WHERE merchant_id = sqlc.arg(merchant_id)::uuid
    AND intent_type IN ('nmi_vault_delete','nmi_payment_method_update')
    AND payload->>'payment_method_id' = ANY(sqlc.arg(payment_method_ids)::text[])
    AND status NOT IN ('succeeded','failed_terminal','superseded','expired')
);

-- name: IsProviderCutoverRepointed :one
SELECT EXISTS (
  SELECT 1 FROM openrails.subscriptions
  WHERE id = sqlc.arg(subscription_id)::uuid AND merchant_id = sqlc.arg(merchant_id)::uuid
    AND psp_id = sqlc.arg(target_psp_id)::uuid
    AND rail_subscription_id = sqlc.arg(target_subscription_id)::text
    AND payment_method_id = sqlc.arg(target_payment_method_id)::uuid
    AND customer_id = sqlc.arg(customer_id)::uuid AND price_id = sqlc.arg(price_id)::uuid
    AND current_period_starts_at = sqlc.arg(period_start)::timestamptz
    AND current_period_ends_at = sqlc.arg(period_end)::timestamptz
    AND deleted_at IS NULL
);

-- name: RepointProviderCutoverSubscription :execrows
UPDATE openrails.subscriptions
SET psp_id = sqlc.arg(target_psp_id)::uuid,
    rail_subscription_id = sqlc.arg(target_subscription_id)::text,
    payment_method_id = sqlc.arg(target_payment_method_id)::uuid,
    updated_at = sqlc.arg(now)::timestamptz
WHERE id = sqlc.arg(subscription_id)::uuid AND merchant_id = sqlc.arg(merchant_id)::uuid;
