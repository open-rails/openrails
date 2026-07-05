-- #736 metric threshold alerting.
-- Merchant-scoped statements (all except ListArmedAlertMerchants) run inside
-- RunInMerchantConn so the app.merchant_id GUC + RLS policy scope them to the
-- request/eval merchant; INSERTs still pass merchant_id for the RLS WITH CHECK.

-- ============================================================================
-- alert_rules
-- ============================================================================

-- name: CreateAlertRule :one
INSERT INTO openrails.alert_rules (merchant_id, name, template, params, severity, channels, enabled)
VALUES (sqlc.arg(merchant_id)::uuid, $1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetAlertRule :one
SELECT * FROM openrails.alert_rules WHERE id = $1;

-- name: ListAlertRules :many
SELECT * FROM openrails.alert_rules ORDER BY created_at DESC, id;

-- name: ListEnabledAlertRules :many
-- Evaluator's per-merchant rule set (RLS-scoped; partial index backs it).
SELECT * FROM openrails.alert_rules WHERE enabled ORDER BY created_at, id;

-- name: UpdateAlertRule :one
-- Full read-modify-write of the mutable fields (handler patches then persists).
UPDATE openrails.alert_rules
SET name = $2, params = $3, severity = $4, channels = $5, enabled = $6, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteAlertRule :execrows
DELETE FROM openrails.alert_rules WHERE id = $1;

-- name: MarkAlertRuleFired :exec
-- Edge transition: open a new active alert. Records the crossing value/detail.
UPDATE openrails.alert_rules
SET fired_at = sqlc.arg(fired_at)::timestamptz,
    cleared_at = NULL,
    last_evaluated_at = sqlc.arg(evaluated_at)::timestamptz,
    last_value = sqlc.narg(value),
    last_detail = sqlc.narg(detail),
    updated_at = now()
WHERE id = $1;

-- name: MarkAlertRuleCleared :exec
-- Edge transition: the metric recrossed back under threshold.
UPDATE openrails.alert_rules
SET fired_at = NULL,
    cleared_at = sqlc.arg(cleared_at)::timestamptz,
    last_evaluated_at = sqlc.arg(evaluated_at)::timestamptz,
    last_value = sqlc.narg(value),
    updated_at = now()
WHERE id = $1;

-- name: TouchAlertRuleEvaluated :exec
-- No state change (still-firing, still-quiet, or digest emitted): only advance
-- the evaluation watermark + observed value.
UPDATE openrails.alert_rules
SET last_evaluated_at = sqlc.arg(evaluated_at)::timestamptz,
    last_value = sqlc.narg(value),
    updated_at = now()
WHERE id = $1;

-- name: ListArmedAlertMerchants :many
-- CROSS-MERCHANT (base pool / GenGlobal): the evaluator scheduler's armed-merchant
-- selection. Same base-pool posture as the #358 intent executor sweeps.
SELECT DISTINCT merchant_id FROM openrails.alert_rules WHERE enabled;

-- ============================================================================
-- merchant_webhooks
-- ============================================================================

-- name: CreateMerchantWebhook :one
INSERT INTO openrails.merchant_webhooks (merchant_id, name, url, format, enabled)
VALUES (sqlc.arg(merchant_id)::uuid, $1, $2, $3, $4)
RETURNING *;

-- name: GetMerchantWebhook :one
SELECT * FROM openrails.merchant_webhooks WHERE id = $1;

-- name: ListMerchantWebhooks :many
SELECT * FROM openrails.merchant_webhooks ORDER BY created_at DESC, id;

-- name: DeleteMerchantWebhook :execrows
DELETE FROM openrails.merchant_webhooks WHERE id = $1;

-- ============================================================================
-- merchant_notifications  (in_app bell)
-- ============================================================================

-- name: CreateMerchantNotification :one
INSERT INTO openrails.merchant_notifications (merchant_id, severity, title, body, link, rule_id, data)
VALUES (sqlc.arg(merchant_id)::uuid, $1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListMerchantNotifications :many
SELECT * FROM openrails.merchant_notifications
WHERE (NOT sqlc.arg(unread_only)::boolean OR read_at IS NULL)
ORDER BY created_at DESC, id
LIMIT sqlc.arg(row_limit)::int;

-- name: MarkMerchantNotificationRead :execrows
UPDATE openrails.merchant_notifications
SET read_at = COALESCE(read_at, now())
WHERE id = $1;

-- name: CountUnreadMerchantNotifications :one
SELECT count(*) FROM openrails.merchant_notifications WHERE read_at IS NULL;

-- ============================================================================
-- payment_methods digest (#733 payment_methods_expiring is Deferred; the
-- monthly digest template runs this dedicated RLS-scoped count instead).
-- expiry_date is 'MM/YY'; a card lapses at the END of its expiry month.
-- ============================================================================

-- name: CountPaymentMethodsExpiringWithin :one
SELECT count(*) FROM openrails.payment_methods
WHERE expiry_date ~ '^[0-9]{2}/[0-9]{2}$'
  AND (date_trunc('month', to_date(expiry_date, 'MM/YY')) + interval '1 month' - interval '1 day')
        BETWEEN sqlc.arg(now)::timestamptz
            AND (sqlc.arg(now)::timestamptz + make_interval(days => sqlc.arg(days_ahead)::int));
