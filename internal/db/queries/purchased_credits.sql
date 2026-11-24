-- What reversals of purchased credit lots took: their shares, and their legs.

-- name: GetPurchasedCreditRefundState :one
-- cash is what reversals returned of the lot's price, net of recoveries, as
-- their shares record it; withdrawn, the units they took from the balance.
SELECT
 (SELECT COALESCE(sum(CASE WHEN s.origin_id = s.reversal_id THEN s.cash ELSE -s.cash END),0) FROM billing.credit_reversal_shares s
   WHERE s.merchant_id=sqlc.arg(merchant_id)::uuid AND s.credit_grant_id=sqlc.arg(credit_grant_id)::uuid)::bigint AS cash,
 (SELECT COALESCE(sum(CASE WHEN lt.transfer_type='credit_refund' THEN lt.amount WHEN lt.transfer_type='credit_refund_restore' THEN -lt.amount ELSE 0 END),0)
   FROM billing.ledger_transfers lt
   WHERE lt.merchant_id=sqlc.arg(merchant_id)::uuid AND lt.credit_grant_id=sqlc.arg(credit_grant_id)::uuid
     AND lt.book='units' AND lt.operation='purchased_credit_refund')::bigint AS withdrawn;

-- name: GetPurchasedCreditReversalState :one
-- Where the units one reversal took of the lot were. legs counts both
-- books', so a reversal posts once.
SELECT
 COALESCE(sum(lt.amount) FILTER (WHERE lt.book='units' AND lt.transfer_type='credit_refund'),0)::bigint AS withdrawn,
 COALESCE(sum(lt.amount) FILTER (WHERE lt.book='units' AND lt.source='expired_credit'),0)::bigint AS expired,
 COALESCE(sum(lt.amount) FILTER (WHERE lt.book='units' AND lt.source='revoked_credit'),0)::bigint AS revoked,
 COALESCE(sum(lt.amount) FILTER (WHERE lt.book='units' AND lt.source='repaid_owed'),0)::bigint AS repaid,
 count(*)::bigint AS legs
FROM billing.ledger_transfers lt
WHERE lt.merchant_id=sqlc.arg(merchant_id)::uuid AND lt.credit_grant_id=sqlc.arg(credit_grant_id)::uuid
 AND lt.operation='purchased_credit_refund' AND lt.source_id=sqlc.arg(reversal_id)::text;

-- name: GetCreditReversalShare :one
-- What one reversal took of the lot, as its share records it.
SELECT COALESCE(sum(cash),0)::bigint AS cash, COALESCE(sum(units),0)::bigint AS units
FROM billing.credit_reversal_shares
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND credit_grant_id=sqlc.arg(credit_grant_id)::uuid
 AND reversal_id=sqlc.arg(reversal_id)::uuid AND origin_id=sqlc.arg(reversal_id)::uuid;

-- name: GetCreditRecoveredShare :one
-- What recoveries restored of one reversal's share.
SELECT COALESCE(sum(cash),0)::bigint AS cash, COALESCE(sum(units),0)::bigint AS units
FROM billing.credit_reversal_shares
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND credit_grant_id=sqlc.arg(credit_grant_id)::uuid
 AND origin_id=sqlc.arg(reversal_id)::uuid AND reversal_id<>sqlc.arg(reversal_id)::uuid;

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
-- Decides a reversal's share of the lot, or of origin's, once; a second
-- decision fails.
INSERT INTO billing.credit_reversal_shares (merchant_id, reversal_id, credit_grant_id, origin_id, cash, units)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(reversal_id)::uuid, sqlc.arg(credit_grant_id)::uuid, sqlc.arg(origin_id)::uuid, sqlc.arg(cash)::bigint, sqlc.arg(units)::bigint);

-- name: SeedCreditReversalShares :exec
-- Records the shares of reversals an archive from before them restored.
SELECT billing.seed_credit_reversal_shares(sqlc.arg(merchant_id)::uuid);
