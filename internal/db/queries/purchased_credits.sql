-- What reversals of purchased credit lots posted, from the lots' ledger legs.

-- name: GetPurchasedCreditRefundState :one
-- cash is what reversals returned of the lot's price, net of recoveries: the
-- units book's cash legs before the credit books, the general ledger's since.
SELECT
 COALESCE(sum(CASE
     WHEN lt.book='units' AND lt.transfer_type='credit_refund_cash' THEN lt.amount
     WHEN lt.book='units' AND lt.transfer_type='credit_refund_cash_restore' THEN -lt.amount
     WHEN lt.book='general' AND cr.account_type IN ('cash_clearing','external_receipts') THEN lt.amount
     WHEN lt.book='general' AND dr.account_type IN ('cash_clearing','external_receipts') THEN -lt.amount
     ELSE 0 END),0)::bigint AS cash,
 COALESCE(sum(CASE WHEN lt.book<>'units' THEN 0 WHEN lt.transfer_type='credit_refund' THEN lt.amount WHEN lt.transfer_type='credit_refund_restore' THEN -lt.amount ELSE 0 END),0)::bigint AS withdrawn
FROM billing.ledger_transfers lt
JOIN billing.ledger_accounts dr ON dr.merchant_id=lt.merchant_id AND dr.id=lt.debit_account_id
JOIN billing.ledger_accounts cr ON cr.merchant_id=lt.merchant_id AND cr.id=lt.credit_account_id
WHERE lt.merchant_id=sqlc.arg(merchant_id)::uuid AND lt.credit_grant_id=sqlc.arg(credit_grant_id)::uuid
 AND lt.operation='purchased_credit_refund';

-- name: GetPurchasedCreditReversalState :one
-- What one reversal took of the lot: its cash from either book, its units by
-- where they were. legs counts both books', so a reversal posts once.
SELECT
 (COALESCE(sum(lt.amount) FILTER (WHERE lt.book='units' AND lt.transfer_type='credit_refund_cash'),0)
  + COALESCE(sum(lt.amount) FILTER (WHERE lt.book='general' AND cr.account_type IN ('cash_clearing','external_receipts')),0))::bigint AS cash,
 COALESCE(sum(lt.amount) FILTER (WHERE lt.book='units' AND lt.transfer_type='credit_refund'),0)::bigint AS withdrawn,
 COALESCE(sum(lt.amount) FILTER (WHERE lt.book='units' AND lt.source='expired_credit'),0)::bigint AS expired,
 COALESCE(sum(lt.amount) FILTER (WHERE lt.book='units' AND lt.source='revoked_credit'),0)::bigint AS revoked,
 COALESCE(sum(lt.amount) FILTER (WHERE lt.book='units' AND lt.source='repaid_owed'),0)::bigint AS repaid,
 COALESCE(sum(lt.amount) FILTER (WHERE lt.book='units' AND (lt.transfer_type='credit_refund' OR lt.source IN ('consumed_credit','expired_credit','revoked_credit','repaid_owed'))),0)::bigint AS face,
 count(*)::bigint AS legs
FROM billing.ledger_transfers lt
JOIN billing.ledger_accounts cr ON cr.merchant_id=lt.merchant_id AND cr.id=lt.credit_account_id
WHERE lt.merchant_id=sqlc.arg(merchant_id)::uuid AND lt.credit_grant_id=sqlc.arg(credit_grant_id)::uuid
 AND lt.operation='purchased_credit_refund' AND lt.source_id=sqlc.arg(reversal_id)::text;

-- name: GetPurchasedCreditRecoveryTotal :one
-- What recoveries restored of one reversal's cash, in either book.
SELECT COALESCE(sum(lt.amount),0)::bigint AS amount
FROM billing.ledger_transfers lt
JOIN billing.ledger_accounts dr ON dr.merchant_id=lt.merchant_id AND dr.id=lt.debit_account_id
WHERE lt.merchant_id=sqlc.arg(merchant_id)::uuid AND lt.credit_grant_id=sqlc.arg(credit_grant_id)::uuid
 AND lt.operation='purchased_credit_refund'
 AND ((lt.book='units' AND lt.transfer_type='credit_refund_cash_restore' AND lt.source='cash_restore:' || sqlc.arg(reversal_id)::text)
   OR (lt.book='general' AND dr.account_type IN ('cash_clearing','external_receipts') AND lt.source LIKE '%\_restore:' || sqlc.arg(reversal_id)::text));

-- name: GetPurchasedCreditPendingRefunds :one
SELECT COALESCE(sum(-amount),0)::bigint AS amount
FROM billing.payments
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND refunded_payment_id=sqlc.arg(payment_id)::uuid
 AND status='pending' AND amount<0 AND deleted_at IS NULL;

-- name: GetPurchasedCreditRetiredBalance :one
SELECT
 COALESCE(sum(CASE WHEN cr.account_type='units_expired' THEN lt.amount WHEN dr.account_type='units_expired' THEN -lt.amount ELSE 0 END),0)::bigint AS expired,
 COALESCE(sum(CASE WHEN cr.account_type='units_revoked' THEN lt.amount WHEN dr.account_type='units_revoked' THEN -lt.amount ELSE 0 END),0)::bigint AS revoked
FROM billing.ledger_transfers lt
JOIN billing.ledger_accounts dr ON dr.merchant_id=lt.merchant_id AND dr.id=lt.debit_account_id
JOIN billing.ledger_accounts cr ON cr.merchant_id=lt.merchant_id AND cr.id=lt.credit_account_id
WHERE lt.merchant_id=sqlc.arg(merchant_id)::uuid AND lt.credit_grant_id=sqlc.arg(credit_grant_id)::uuid;

-- name: GetPurchasedCreditRepaidOwed :one
-- Owed the lot repaid and still stands repaid: repayments, less what reversals
-- revived, plus what recoveries repaid again.
SELECT COALESCE(sum(CASE
    WHEN transfer_type='owed_repayment' THEN amount
    WHEN operation='purchased_credit_refund' AND source='repaid_owed' THEN -amount
    WHEN operation='purchased_credit_refund' AND source LIKE 'repaid_owed_restore:%' THEN amount
    ELSE 0 END),0)::bigint AS amount
FROM billing.ledger_transfers
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND credit_grant_id=sqlc.arg(credit_grant_id)::uuid AND book='units';

-- name: CreditReversalDecided :one
-- Whether a reversal's, or a recovery's, share of the lot is decided.
SELECT EXISTS (
    SELECT 1 FROM billing.credit_reversal_shares
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND reversal_id = sqlc.arg(reversal_id)::uuid AND credit_grant_id = sqlc.arg(credit_grant_id)::uuid
)::boolean AS decided;

-- name: RecordCreditReversalShare :exec
-- Decides a reversal's share of the lot once; a second decision fails.
INSERT INTO billing.credit_reversal_shares (merchant_id, reversal_id, credit_grant_id, cash, units)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(reversal_id)::uuid, sqlc.arg(credit_grant_id)::uuid, sqlc.arg(cash)::bigint, sqlc.arg(units)::bigint);
