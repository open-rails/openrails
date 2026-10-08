-- name: GetInvoiceRecoveryPayment :many
SELECT * FROM billing.invoice_payments
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND psp_id=sqlc.arg(psp_id)::uuid
 AND rail_payment_id=sqlc.arg(transaction_id)::text
LIMIT 2;

-- A transaction already allocated to a subscription or purchase cannot also
-- settle an invoice, including soft-deleted financial rows.
-- name: InvoiceRecoveryHasOtherPayment :one
SELECT EXISTS(SELECT 1 FROM billing.payments WHERE merchant_id=sqlc.arg(merchant_id)::uuid
 AND psp_id=sqlc.arg(psp_id)::uuid AND transaction_id=sqlc.arg(transaction_id)::text)::boolean;

-- name: InvoiceRecoveryVaultCustomers :many
SELECT DISTINCT customer_id FROM billing.payment_methods
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND psp_id=sqlc.arg(psp_id)::uuid
 AND rail='nmi' AND rail_customer_ref=sqlc.arg(vault)::text
LIMIT 2;

-- The invoice's retained paid amount must be explained by its allocations,
-- including native rounding (provider amount can exceed the allocated debt).
-- name: InvoiceRecoveryAllocationTotal :one
SELECT COALESCE(sum(l.amount),0)::bigint AS amount,
 COALESCE(bool_and(l.id IS NOT NULL AND l.invoice_id IS NOT DISTINCT FROM p.invoice_id AND l.customer_id IS NOT DISTINCT FROM p.customer_id
   AND l.currency=p.currency AND l.currency=sqlc.arg(currency)::text
   AND l.transfer_type='owed_payment' AND l.operation IN ('invoice_payment','manual_invoice_payment')
   AND l.amount>0 AND p.amount>=l.amount),true)::boolean AS consistent
FROM billing.invoice_payments p LEFT JOIN billing.ledger_transfers l
 ON l.merchant_id=p.merchant_id AND l.id=p.ledger_transfer_id
WHERE p.merchant_id=sqlc.arg(merchant_id)::uuid AND p.invoice_id=sqlc.arg(invoice_id)::uuid AND p.status='settled';

-- A full observed payment can settle a previously uncollectible invoice just
-- as customer pay-now does. This intermediate state is inside the locked payment
-- transaction and is never committed without the full settlement.
-- name: ReopenObservedInvoiceForSettlement :exec
UPDATE billing.invoices SET status='open'
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(invoice_id)::uuid
 AND status='uncollectible' AND collection_intent_id IS NULL;

-- name: GetInvoiceRecoveryLedgerTransfer :one
SELECT * FROM billing.ledger_transfers
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(transfer_id)::uuid;
