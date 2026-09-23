-- name: GetMerchantConfigurationApplication :one
SELECT request_sha256,result FROM openrails.merchant_configuration_applications
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND application_id=sqlc.arg(application_id)::text;

-- name: InsertMerchantConfigurationApplication :exec
INSERT INTO openrails.merchant_configuration_applications(merchant_id,application_id,request_sha256,result)
VALUES(sqlc.arg(merchant_id)::uuid,sqlc.arg(application_id)::text,sqlc.arg(request_sha256)::bytea,sqlc.arg(result)::jsonb);

-- name: GetMerchantConfigurationDirectory :one
SELECT COALESCE(display_name,'')::text AS display_name,COALESCE(api_host,'')::text AS api_host
FROM openrails.merchants WHERE id=sqlc.arg(merchant_id)::uuid AND status='active' AND deleted_at IS NULL;

-- name: ApplyMerchantConfigurationDirectory :exec
UPDATE openrails.merchants SET
 display_name=CASE WHEN sqlc.arg(set_display_name)::boolean THEN sqlc.arg(display_name)::text ELSE display_name END,
 api_host=CASE WHEN sqlc.arg(set_api_host)::boolean THEN NULLIF(sqlc.arg(api_host)::text,'') ELSE api_host END,
 updated_at=current_timestamp
WHERE id=sqlc.arg(merchant_id)::uuid AND status='active' AND deleted_at IS NULL;
