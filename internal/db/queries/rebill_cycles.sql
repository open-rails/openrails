-- name: UpsertRebillCycle :one
-- #1111: the cycle for a subscription's period that came due at due_at.
INSERT INTO openrails.rebill_cycles (id, merchant_id, subscription_id, customer_id, psp_id, rail, owner, due_at, amount, currency)
VALUES (sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(subscription_id)::uuid, sqlc.arg(customer_id)::uuid,
    sqlc.arg(psp_id)::uuid, sqlc.arg(rail)::text, sqlc.arg(owner)::text, sqlc.arg(due_at)::timestamptz,
    sqlc.arg(amount)::bigint, sqlc.arg(currency)::text)
ON CONFLICT (merchant_id, subscription_id, due_at) DO UPDATE SET due_at = EXCLUDED.due_at
RETURNING id;
