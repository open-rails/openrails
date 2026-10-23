-- CROSS-MERCHANT: server-owned account authority cursor, never merchant data.
-- name: GetNetworkEventCursor :one
SELECT cursor FROM billing.network_event_cursors WHERE issuer=sqlc.arg(issuer)::text AND tenant_id=sqlc.arg(tenant_id)::uuid;

-- name: EnsureNetworkEventCursor :exec
INSERT INTO billing.network_event_cursors(issuer,tenant_id) VALUES(sqlc.arg(issuer)::text,sqlc.arg(tenant_id)::uuid) ON CONFLICT DO NOTHING;

-- name: LockNetworkEventCursor :one
SELECT cursor FROM billing.network_event_cursors WHERE issuer=sqlc.arg(issuer)::text AND tenant_id=sqlc.arg(tenant_id)::uuid FOR UPDATE;

-- name: SaveNetworkEventCursor :exec
UPDATE billing.network_event_cursors SET cursor=sqlc.arg(cursor)::text,updated_at=now() WHERE issuer=sqlc.arg(issuer)::text AND tenant_id=sqlc.arg(tenant_id)::uuid;
