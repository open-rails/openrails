-- name: InsertSnapshotPSP :exec
INSERT INTO openrails.psps(id,merchant_id,rail,environment,account_id,key,archived,evidence,custodian_id)
VALUES(sqlc.arg(id)::uuid,sqlc.arg(merchant_id)::uuid,sqlc.arg(rail)::text,sqlc.arg(environment)::text,sqlc.arg(account_id)::text,sqlc.arg(key)::text,sqlc.arg(archived)::boolean,sqlc.arg(evidence)::jsonb,sqlc.narg(custodian_id)::uuid)
ON CONFLICT(rail,environment,account_id) DO NOTHING;

-- name: InsertSnapshotCustodian :exec
INSERT INTO openrails.custodians(merchant_id,key,kind,environment,account_id,settings,archived)
VALUES(sqlc.arg(merchant_id)::uuid,sqlc.arg(key)::text,sqlc.arg(kind)::text,sqlc.arg(environment)::text,sqlc.arg(account_id)::text,sqlc.arg(settings)::jsonb,sqlc.arg(archived)::boolean)
ON CONFLICT(kind,environment,account_id) DO NOTHING;
