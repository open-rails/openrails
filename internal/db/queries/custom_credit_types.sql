-- openrails.custom_credit_types (#475): per-tenant consume-only credit units.
-- Rows are written by the catalog sidecar push (#706, auto-defined from
-- catalog_credit_balances.unit); this read backs ResolveUnit.

-- name: GetCustomCreditType :one
SELECT * FROM openrails.custom_credit_types
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND name = sqlc.arg(name)::text;
