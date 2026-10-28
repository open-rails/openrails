-- Ahead-of-renewal refresh of cards held by a vault that must be asked for
-- updates (basis_theory and platform custody). Holders that update their cards
-- in place are never enumerated. Indexed on payment_methods_account_updater_due_idx.

-- CROSS-MERCHANT: merchants with a vault holding a card that pays a renewal
-- inside the longest lookahead and was not checked inside the shortest. Whether
-- the vault is armed, and its own lookahead, are in the merchant's
-- configuration, which the caller reads per merchant. Capped and cursored.
-- name: ListStoredCardRefreshMerchants :many
SELECT DISTINCT c.merchant_id
FROM billing.custodians c
WHERE c.kind IN ('basis_theory', 'platform')
  AND (sqlc.narg(after)::uuid IS NULL OR c.merchant_id > sqlc.narg(after)::uuid)
  AND EXISTS (
        SELECT 1 FROM billing.payment_methods pm
         WHERE pm.merchant_id = c.merchant_id AND pm.custodian_id = c.id
           AND pm.custodian IN ('basis_theory', 'platform') AND pm.status = 'active' AND pm.rail_method_ref IS NOT NULL
           AND (pm.account_updater_checked_at IS NULL
                OR pm.account_updater_checked_at < sqlc.arg(now)::timestamptz - make_interval(days => sqlc.arg(min_lookahead_days)::int))
           AND regexp_replace(lower(pm.card_brand), '[^a-z]', '', 'g') IN ('visa', 'mastercard', 'mc')
           AND (EXISTS (
                   SELECT 1 FROM billing.subscriptions s
                    WHERE s.merchant_id = pm.merchant_id AND s.payment_method_id = pm.id
                      AND s.deleted_at IS NULL AND s.status IN ('active', 'past_due', 'awaiting_method')
                      AND s.current_period_ends_at <= sqlc.arg(now)::timestamptz + make_interval(days => sqlc.arg(max_lookahead_days)::int))
                OR EXISTS (
                   SELECT 1 FROM billing.money_settings ms
                     JOIN billing.subscriptions s ON s.merchant_id = ms.merchant_id AND s.customer_id = ms.customer_id
                    WHERE ms.merchant_id = pm.merchant_id AND ms.customer_id = pm.customer_id AND ms.default_payment_method_id = pm.id
                      AND s.payment_method_id IS NULL
                      AND billing.subscription_payment_method_id(s.merchant_id, s.customer_id, s.payment_method_id, s.price_id, s.rail, s.collection_policy) = pm.id
                      AND s.deleted_at IS NULL AND s.status IN ('active', 'past_due', 'awaiting_method')
                      AND s.current_period_ends_at <= sqlc.arg(now)::timestamptz + make_interval(days => sqlc.arg(max_lookahead_days)::int))))
ORDER BY c.merchant_id
LIMIT sqlc.arg(merchant_limit)::int;

-- One vault's due cards: Visa and Mastercard cards paying a renewal before
-- renewal_before, as a subscription's own card or the default it follows, not
-- checked since stale_before. Parked cards are included:
-- a reissue is what recovers them. Never-checked first, then stalest.
-- name: ListDueStoredCards :many
SELECT pm.id, pm.rail_method_ref, pm.account_updater_checked_at
FROM billing.payment_methods pm
WHERE pm.merchant_id = sqlc.arg(merchant_id)
  AND pm.custodian_id = sqlc.arg(custodian_id)::uuid
  AND pm.custodian IN ('basis_theory', 'platform') AND pm.status = 'active' AND pm.rail_method_ref IS NOT NULL
  AND (pm.account_updater_checked_at IS NULL OR pm.account_updater_checked_at < sqlc.arg(stale_before)::timestamptz)
  AND regexp_replace(lower(pm.card_brand), '[^a-z]', '', 'g') IN ('visa', 'mastercard', 'mc')
  AND (EXISTS (
          SELECT 1 FROM billing.subscriptions s
           WHERE s.merchant_id = pm.merchant_id AND s.payment_method_id = pm.id
             AND s.deleted_at IS NULL AND s.status IN ('active', 'past_due', 'awaiting_method')
             AND s.current_period_ends_at <= sqlc.arg(renewal_before)::timestamptz)
       OR EXISTS (
          SELECT 1 FROM billing.money_settings ms
            JOIN billing.subscriptions s ON s.merchant_id = ms.merchant_id AND s.customer_id = ms.customer_id
           WHERE ms.merchant_id = pm.merchant_id AND ms.customer_id = pm.customer_id AND ms.default_payment_method_id = pm.id
             AND s.payment_method_id IS NULL
             AND billing.subscription_payment_method_id(s.merchant_id, s.customer_id, s.payment_method_id, s.price_id, s.rail, s.collection_policy) = pm.id
             AND s.deleted_at IS NULL AND s.status IN ('active', 'past_due', 'awaiting_method')
             AND s.current_period_ends_at <= sqlc.arg(renewal_before)::timestamptz))
ORDER BY pm.account_updater_checked_at NULLS FIRST, pm.id
LIMIT sqlc.arg(row_limit)::int;

-- A card is checked when its holder answered, or refused for good.
-- name: StampAccountUpdaterChecked :execrows
UPDATE billing.payment_methods SET account_updater_checked_at = sqlc.arg(checked_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id) AND id = sqlc.arg(id);

