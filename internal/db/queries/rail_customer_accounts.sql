-- openrails.rail_customer_accounts: customer <-> rail customer mapping.

-- name: UpsertRailCustomerAccount :exec
-- or#893: the mapping is PER-PSP. Two Stripe accounts on one merchant hold two
-- independent rows for the same person; before this, whichever account's
-- webhook landed last overwrote the other.
INSERT INTO openrails.rail_customer_accounts (
    id, merchant_id, customer_id, rail, psp_id, account_id, created_at, updated_at
) VALUES ($1, sqlc.arg(merchant_id)::uuid, $2, $3, sqlc.arg(psp_id)::uuid, $4, $5, $6)
ON CONFLICT (merchant_id, customer_id, rail, psp_id) DO UPDATE SET
    account_id = EXCLUDED.account_id,
    updated_at = EXCLUDED.updated_at;

-- name: GetRailCustomerAccountIDForPSP :one
SELECT account_id FROM openrails.rail_customer_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND rail = sqlc.arg(rail)::text
  AND psp_id = sqlc.arg(psp_id)::uuid;

-- name: GetRailCustomerAccountIDForMerchant :one
-- Rail-scoped read for callers that legitimately hold no PSP (the customer
-- portal, invoice collection). Deterministic by recency: with two PSPs on a
-- rail this returns the most recently written mapping, never an arbitrary one.
SELECT account_id FROM openrails.rail_customer_accounts
WHERE merchant_id = $1 AND customer_id = $2 AND rail = $3
ORDER BY updated_at DESC, id DESC
LIMIT 1;

-- name: GetRailCustomerAccountSubjectForPSP :one
SELECT customer_id::text FROM openrails.rail_customer_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND account_id = sqlc.arg(account_id)::text
  AND rail = sqlc.arg(rail)::text;
