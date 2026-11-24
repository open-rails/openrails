-- name: LockMerchantSettings :one
SELECT id FROM billing.merchants WHERE id = $1 FOR NO KEY UPDATE;

-- name: ReadMerchantSettingsLock :one
SELECT id FROM billing.merchants WHERE id = $1 FOR SHARE;
