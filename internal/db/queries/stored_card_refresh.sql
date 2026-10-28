-- Stored cards a vault must be asked about (basis_theory and platform custody).
-- The ahead-of-renewal pass reads only billing.stored_card_checks, which
-- subscription and default-card changes fill; cards whose holder updates them
-- in place never appear.

-- CROSS-MERCHANT: the vault cards due for a look, oldest first. Capped.
-- name: ListDueStoredCardChecks :many
SELECT merchant_id, payment_method_id
FROM billing.stored_card_checks
WHERE due_at <= sqlc.arg(now)::timestamptz
ORDER BY due_at, merchant_id, payment_method_id
LIMIT sqlc.arg(row_limit)::int;

-- name: PlanStoredCardCheck :execrows
UPDATE billing.stored_card_checks SET due_at = sqlc.arg(due_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id) AND payment_method_id = sqlc.arg(payment_method_id);

-- name: DeleteStoredCardCheck :execrows
DELETE FROM billing.stored_card_checks
WHERE merchant_id = sqlc.arg(merchant_id) AND payment_method_id = sqlc.arg(payment_method_id);

-- The next renewal a card pays, as a subscription's own card or the default
-- it follows; none when no live subscription renews on it.
-- name: StoredCardNextRenewal :many
SELECT s.current_period_ends_at::timestamptz AS renews_at
FROM billing.payment_methods pm
JOIN billing.subscriptions s ON s.merchant_id = pm.merchant_id AND s.customer_id = pm.customer_id
WHERE pm.merchant_id = sqlc.arg(merchant_id)::uuid AND pm.id = sqlc.arg(payment_method_id)::uuid
  AND billing.subscription_payment_method_id(s.merchant_id, s.customer_id, s.payment_method_id, s.price_id, s.rail, s.collection_policy) = pm.id
  AND s.deleted_at IS NULL AND s.status IN ('active', 'past_due', 'awaiting_method')
  AND s.current_period_ends_at IS NOT NULL
ORDER BY s.current_period_ends_at
LIMIT 1;

-- A card is checked when its holder answered, refused it for good, or could
-- not take its answer.
-- name: StampAccountUpdaterChecked :execrows
UPDATE billing.payment_methods SET account_updater_checked_at = sqlc.arg(checked_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id) AND id = sqlc.arg(id);

-- Whether an open finding holds a vault's checks (its key was refused).
-- name: StoredCardVaultHeld :one
SELECT EXISTS (
    SELECT 1 FROM billing.reconciliation_findings
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND finding_type = sqlc.arg(finding_type)::text
      AND psp_id IS NULL AND subject_key = sqlc.arg(subject_key)::text
      AND status IN ('reconcile_required', 'requires_review')
)::bool;

-- The vault answered again: its refused-key finding is over.
-- name: ResolveStoredCardVaultHold :execrows
UPDATE billing.reconciliation_findings
SET status = 'auto_fixed', resolution = 'enforced', resolved_at = now(),
    notified_at = NULL, notified_severity = NULL, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND finding_type = sqlc.arg(finding_type)::text
  AND psp_id IS NULL AND subject_key = sqlc.arg(subject_key)::text
  AND status IN ('reconcile_required', 'requires_review');
