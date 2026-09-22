-- These catalog-only queries qualify the actual database and River namespace.
-- They must run on the supplied physical transaction/pool, without schema rewriting.

-- name: AcquireDatabaseIdentityWitness :one
SELECT pg_catalog.pg_backend_pid() AS backend_pid,
       pg_catalog.pg_try_advisory_xact_lock(sqlc.arg(first_class)::integer, sqlc.arg(first_object)::integer) AS first_locked,
       pg_catalog.pg_try_advisory_xact_lock(sqlc.arg(second_class)::integer, sqlc.arg(second_object)::integer) AS second_locked;

-- name: ObserveDatabaseIdentityWitness :one
SELECT pg_catalog.count(*) = 2 AS same_database
FROM pg_catalog.pg_locks
WHERE locktype = 'advisory'
  AND pid = sqlc.arg(backend_pid)::integer
  AND granted AND mode = 'ExclusiveLock' AND objsubid = 2
  AND database = (SELECT oid FROM pg_catalog.pg_database WHERE datname = pg_catalog.current_database())
  AND ((classid = sqlc.arg(first_class)::bigint::pg_catalog.oid AND objid = sqlc.arg(first_object)::bigint::pg_catalog.oid)
    OR (classid = sqlc.arg(second_class)::bigint::pg_catalog.oid AND objid = sqlc.arg(second_object)::bigint::pg_catalog.oid));

-- name: RiverQueueTableExists :one
SELECT pg_catalog.to_regclass(sqlc.arg(relation_name)::text) IS NOT NULL AS queue_table_exists;
