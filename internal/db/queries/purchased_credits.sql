-- name: GetPurchasedCreditGrant :one
SELECT * FROM billing.grants
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND payment_id=sqlc.arg(payment_id)::uuid
  AND kind='credit' AND event='grant'
  AND spec_snapshot->'deposit'->'paid_amount' IS NOT NULL;

-- name: GetPurchasedCreditRefundState :one
SELECT
 COALESCE(sum(CASE transfer_type WHEN 'credit_refund_cash' THEN amount WHEN 'credit_refund_cash_restore' THEN -amount ELSE 0 END),0)::bigint AS cash,
 COALESCE(sum(CASE transfer_type WHEN 'credit_refund' THEN amount WHEN 'credit_refund_restore' THEN -amount ELSE 0 END),0)::bigint AS withdrawn,
 count(*)::bigint AS sequence
FROM billing.ledger_transfers
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND grant_id=sqlc.arg(grant_id)::uuid
 AND operation='purchased_credit_refund';

-- name: GetPurchasedCreditReversalState :one
SELECT
 COALESCE(sum(lt.amount) FILTER (WHERE lt.transfer_type='credit_refund_cash'),0)::bigint AS cash,
 COALESCE(sum(lt.amount) FILTER (WHERE lt.transfer_type='credit_refund'),0)::bigint AS withdrawn,
 COALESCE(sum(lt.amount) FILTER (WHERE lt.source='expired_credit'),0)::bigint AS expired,
 COALESCE(sum(lt.amount) FILTER (WHERE lt.source='revoked_credit'),0)::bigint AS revoked,
 COALESCE(sum(lt.amount) FILTER (WHERE lt.source='repaid_owed'),0)::bigint AS repaid,
 COALESCE(sum(lt.amount) FILTER (WHERE lt.transfer_type='credit_refund' OR lt.source IN ('consumed_credit','expired_credit','revoked_credit','repaid_owed')),0)::bigint AS face,
 COALESCE(sum(lt.amount) FILTER (WHERE lt.transfer_type='credit_refund_cash_restore'),0)::bigint AS recovery,
 count(*)::bigint AS legs
FROM billing.ledger_transfers lt
WHERE lt.merchant_id=sqlc.arg(merchant_id)::uuid AND lt.grant_id=sqlc.arg(grant_id)::uuid
 AND lt.operation='purchased_credit_refund' AND lt.source_id=sqlc.arg(reversal_id)::text;

-- name: GetPurchasedCreditRecoveryTotal :one
SELECT COALESCE(sum(amount),0)::bigint AS amount
FROM billing.ledger_transfers
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND grant_id=sqlc.arg(grant_id)::uuid
 AND operation='purchased_credit_refund' AND transfer_type='credit_refund_cash_restore'
 AND source='cash_restore:' || sqlc.arg(reversal_id)::text;

-- name: GetPurchasedCreditPendingRefunds :one
SELECT COALESCE(sum(-amount),0)::bigint AS amount
FROM billing.payments
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND refunded_payment_id=sqlc.arg(payment_id)::uuid
 AND status='pending' AND amount<0 AND deleted_at IS NULL;

-- name: GetPurchasedCreditRetiredBalance :one
SELECT
 COALESCE(sum(CASE WHEN cr.account_type='expired_credits' THEN lt.amount WHEN dr.account_type='expired_credits' THEN -lt.amount ELSE 0 END),0)::bigint AS expired,
 COALESCE(sum(CASE WHEN cr.account_type='revoked_credits' THEN lt.amount WHEN dr.account_type='revoked_credits' THEN -lt.amount ELSE 0 END),0)::bigint AS revoked
FROM billing.ledger_transfers lt
JOIN billing.ledger_accounts dr ON dr.merchant_id=lt.merchant_id AND dr.id=lt.debit_account_id
JOIN billing.ledger_accounts cr ON cr.merchant_id=lt.merchant_id AND cr.id=lt.credit_account_id
WHERE lt.merchant_id=sqlc.arg(merchant_id)::uuid AND lt.grant_id=sqlc.arg(grant_id)::uuid;

-- name: GetPurchasedCreditRepaidOwed :one
-- Owed the lot repaid and still stands repaid: repayments, less what reversals
-- revived, plus what recoveries repaid again.
SELECT COALESCE(sum(CASE
    WHEN transfer_type='owed_repayment' THEN amount
    WHEN operation='purchased_credit_refund' AND source='repaid_owed' THEN -amount
    WHEN operation='purchased_credit_refund' AND source LIKE 'repaid_owed_restore:%' THEN amount
    ELSE 0 END),0)::bigint AS amount
FROM billing.ledger_transfers
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND grant_id=sqlc.arg(grant_id)::uuid;
