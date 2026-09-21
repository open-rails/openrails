-- name: GetMerchantWrappedDEK :one
SELECT wrapped_dek FROM openrails.merchant_deks
WHERE merchant_id = sqlc.arg(merchant_id)::uuid;

-- name: PutMerchantWrappedDEK :one
INSERT INTO openrails.merchant_deks (merchant_id, wrapped_dek)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(wrapped_dek))
ON CONFLICT (merchant_id) DO UPDATE
SET merchant_id = openrails.merchant_deks.merchant_id
RETURNING wrapped_dek;
