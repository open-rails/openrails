-- Session/transaction settings (merchant GUC plumbing).

-- name: SetConfig :one
SELECT set_config(sqlc.arg(setting)::text, sqlc.arg(value)::text, sqlc.arg(is_local)::boolean)::text;

-- name: CurrentSetting :one
SELECT COALESCE(current_setting(sqlc.arg(setting)::text, true), '')::text;
