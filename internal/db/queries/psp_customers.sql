-- billing.psp_customers: a customer's customer object at one PSP.

-- name: UpsertPSPCustomer :exec
-- or#893: the mapping is PER-PSP. Two Stripe accounts on one merchant hold two
-- independent rows for the same person.
INSERT INTO billing.psp_customers (
    merchant_id, customer_id, psp_id, remote_customer_ref, created_at, updated_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(psp_id)::uuid,
    sqlc.arg(remote_customer_ref)::text, sqlc.arg(at)::timestamptz, sqlc.arg(at)::timestamptz
)
ON CONFLICT (merchant_id, customer_id, psp_id) DO UPDATE SET
    remote_customer_ref = EXCLUDED.remote_customer_ref,
    updated_at = EXCLUDED.updated_at;

-- name: GetPSPCustomerRef :one
SELECT remote_customer_ref FROM billing.psp_customers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid;

-- name: GetPSPCustomerRefForRail :one
-- Rail-scoped read for callers that legitimately hold no PSP (the customer
-- portal, invoice collection). Deterministic by recency: with two PSPs on a
-- rail this returns the most recently written mapping, never an arbitrary one.
SELECT c.remote_customer_ref FROM billing.psp_customers c
JOIN billing.psps p ON p.merchant_id = c.merchant_id AND p.id = c.psp_id
WHERE c.merchant_id = sqlc.arg(merchant_id)::uuid
  AND c.customer_id = sqlc.arg(customer_id)::uuid
  AND p.rail = sqlc.arg(rail)::text
ORDER BY c.updated_at DESC, c.id DESC
LIMIT 1;

-- name: GetPSPCustomerByRef :one
SELECT customer_id::text FROM billing.psp_customers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND remote_customer_ref = sqlc.arg(remote_customer_ref)::text;
