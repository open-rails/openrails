-- or#870 bucket 2: the payment-method notification ladder.
--
-- All merchant-scoped and RLS-policied; the cross-merchant fan-out is the
-- SECURITY DEFINER work queue below, which returns merchant IDS ONLY.

-- OpenPaymentMethodNotice starts (or RESTARTS) a ladder for a subscription
-- parked awaiting a card fix. Restart on conflict is deliberate: a fresh
-- bucket-2 decline is a fresh problem, so the rungs re-anchor on the new park
-- instead of inheriting a ladder that may already be exhausted.
-- name: OpenPaymentMethodNotice :one
INSERT INTO openrails.payment_method_notices (
    merchant_id, customer_id, subscription_id, rail, failure_code,
    parked_at, rungs_sent, next_notice_at, created_at, updated_at
) VALUES (
    sqlc.arg(merchant_id), sqlc.arg(customer_id), sqlc.arg(subscription_id),
    sqlc.arg(rail), sqlc.narg(failure_code),
    sqlc.arg(parked_at), 1, sqlc.narg(next_notice_at), sqlc.arg(parked_at), sqlc.arg(parked_at)
)
ON CONFLICT (merchant_id, subscription_id) DO UPDATE
   SET rail           = EXCLUDED.rail,
       failure_code   = EXCLUDED.failure_code,
       parked_at      = EXCLUDED.parked_at,
       rungs_sent     = 1,
       next_notice_at = EXCLUDED.next_notice_at,
       resolved_at    = NULL,
       resolution     = NULL,
       updated_at     = EXCLUDED.updated_at
RETURNING *;

-- ClaimDuePaymentMethodNotices locks the due rungs for this pass. SKIP LOCKED
-- so two workers never fight over the same ladder; the rung advance commits
-- with the notification insert, which is what makes a rung exactly-once.
-- name: ClaimDuePaymentMethodNotices :many
SELECT n.*
  FROM openrails.payment_method_notices n
 WHERE n.merchant_id = sqlc.arg(merchant_id)
   AND n.resolved_at IS NULL
   AND n.next_notice_at IS NOT NULL
   AND n.next_notice_at <= sqlc.arg(now)::timestamptz
 ORDER BY n.next_notice_at
 LIMIT sqlc.arg(row_limit)
   FOR UPDATE OF n SKIP LOCKED;

-- name: AdvancePaymentMethodNoticeRung :execrows
UPDATE openrails.payment_method_notices
   SET rungs_sent     = rungs_sent + 1,
       next_notice_at = sqlc.narg(next_notice_at),
       resolved_at    = sqlc.narg(resolved_at),
       resolution     = sqlc.narg(resolution),
       updated_at     = sqlc.arg(now)
 WHERE merchant_id = sqlc.arg(merchant_id)
   AND id = sqlc.arg(id)
   AND resolved_at IS NULL;

-- ResolvePaymentMethodNotice closes a ladder WITHOUT sending a rung: the
-- customer fixed the card, or the subscription is over.
-- name: ResolvePaymentMethodNotice :execrows
UPDATE openrails.payment_method_notices
   SET next_notice_at = NULL,
       resolved_at    = sqlc.arg(now)::timestamptz,
       resolution     = sqlc.arg(resolution)::text,
       updated_at     = sqlc.arg(now)::timestamptz
 WHERE merchant_id = sqlc.arg(merchant_id)
   AND id = sqlc.arg(id)
   AND resolved_at IS NULL;

-- name: GetPaymentMethodNoticeBySubscription :one
SELECT * FROM openrails.payment_method_notices
 WHERE merchant_id = sqlc.arg(merchant_id)
   AND subscription_id = sqlc.arg(subscription_id);

-- name: ListDuePaymentMethodNoticeMerchants :many
SELECT merchant_id FROM openrails.due_payment_method_notice_merchant_ids(
    sqlc.arg(now)::timestamptz,
    sqlc.arg(merchant_limit)::int
);
