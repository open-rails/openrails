-- Private typed producer journal for idempotent financial transitions.
-- Public readers use event_log; this table has no delivery state.

-- name: EnqueueHostLifecycleEvent :execrows
-- Idempotent on the transition's dedupe key: re-announcing a transition is a
-- no-op, never a second instruction to the host.
INSERT INTO billing.host_outbox
    (merchant_id, event_type, subject_type, subject_id, currency, occurred_at, data, dedupe_key)
VALUES (
    sqlc.arg(merchant_id), sqlc.arg(event_type)::text, sqlc.arg(subject_type)::text,
    sqlc.arg(subject_id), sqlc.arg(currency)::text, sqlc.arg(occurred_at)::timestamptz,
    sqlc.arg(data)::jsonb, sqlc.arg(dedupe_key)::text)
ON CONFLICT (merchant_id, dedupe_key) DO NOTHING;

-- name: EnqueueProductEntitlementsChanged :exec
-- One key edit of a product, keyed by the revision it produced: announcing it
-- again collapses.
INSERT INTO billing.host_outbox (merchant_id, event_type, subject_type, subject_id, occurred_at, data, dedupe_key)
SELECT p.merchant_id, 'product.entitlements_changed', 'product', p.id, sqlc.arg(occurred_at)::timestamptz, sqlc.arg(data)::jsonb,
       'product.entitlements_changed:' || p.id::text || ':' || p.revision::text
FROM billing.products p
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.id = sqlc.arg(product_id)::uuid
ON CONFLICT (merchant_id, dedupe_key) DO NOTHING;
