-- sqlc schema shim for the authkit `profiles` schema. NOT a migration —
-- authkit owns and migrates profiles.* (applied via migratekit with
-- WithSchema("profiles"); see internal/migrate/migrator.go). This file exists
-- only so sqlc can type-check the few cross-schema queries openrails makes
-- against profiles.users, and so the throwaway sqlc-vet database
-- (scripts/sqlc-vet-db.sh) can be built without applying authkit migrations.
--
-- Declare ONLY the columns openrails queries; keep types in sync with
-- authkit's schema. email/username are citext in the real schema (declared
-- text here — case-insensitivity is a runtime property of the real database
-- and does not affect query compilation or the generated Go types).

CREATE SCHEMA IF NOT EXISTS profiles;

CREATE TABLE profiles.users (
    id uuid PRIMARY KEY,
    email text,
    username text,
    email_verified boolean NOT NULL DEFAULT false,
    deleted_at timestamp with time zone
);

-- #491: cross-schema FK target for openrails invoker_id columns (authkit#81).
-- authkit owns/migrates the real table; declared here only so sqlc can resolve
-- the FK added in migration 027.
CREATE TABLE profiles.delegated_users (
    id uuid PRIMARY KEY
);
