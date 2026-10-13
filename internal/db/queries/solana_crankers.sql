-- billing.solana_crankers: deployment-global, no merchant scope.

-- name: PinSolanaCranker :one
-- Pins the named cranker to key unless it is pinned, and answers its pin.
INSERT INTO billing.solana_crankers (name, public_key)
VALUES (sqlc.arg(name)::text, sqlc.arg(public_key)::text)
ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
RETURNING public_key;
