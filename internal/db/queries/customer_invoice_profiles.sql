-- billing.customer_invoice_profiles: per-payer enterprise invoicing profile
-- (#798) — net-N terms, collection method and the document fields snapshotted
-- onto invoices at finalize.

-- name: UpsertCustomerInvoiceProfile :exec
INSERT INTO billing.customer_invoice_profiles (
    merchant_id, customer_id, net_terms_days, collection_method,
    po_number, tax, billing_contacts, memo, created_at, updated_at
) VALUES (
    $1, $2, sqlc.arg(net_terms_days), sqlc.arg(collection_method),
    sqlc.narg(po_number), COALESCE(sqlc.arg(tax), '{}'::jsonb),
    COALESCE(sqlc.arg(billing_contacts), '[]'::jsonb), sqlc.narg(memo),
    sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz
)
ON CONFLICT (merchant_id, customer_id) DO UPDATE SET
    net_terms_days = EXCLUDED.net_terms_days,
    collection_method = EXCLUDED.collection_method,
    po_number = EXCLUDED.po_number,
    tax = EXCLUDED.tax,
    billing_contacts = EXCLUDED.billing_contacts,
    memo = EXCLUDED.memo,
    updated_at = EXCLUDED.updated_at;

-- name: GetInvoiceProfile :one
SELECT * FROM billing.customer_invoice_profiles
WHERE merchant_id = $1 AND customer_id = $2
LIMIT 1;

-- name: ListInvoiceProfiles :many
SELECT * FROM billing.customer_invoice_profiles
WHERE merchant_id = sqlc.arg(merchant_id) AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[]);

-- name: DeleteCustomerInvoiceProfile :exec
DELETE FROM billing.customer_invoice_profiles
WHERE merchant_id = $1 AND customer_id = $2;
