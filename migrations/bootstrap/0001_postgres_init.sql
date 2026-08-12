-- PostgreSQL Bootstrap
-- Simple initialization: create the required OpenRails schema (default openrails) and install extensions.
-- Open Rails Billing is designed to run standalone; do not create schemas for other apps here.

-- Create schemas
CREATE SCHEMA IF NOT EXISTS openrails;

-- Install required extensions in public schema.
-- Billing generates ids with the Postgres 18 built-in uuidv7() (no extension
-- needed). pgcrypto is kept for any incidental hashing/random needs.
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;

-- migratekit's tracker table. migratekit creates and upgrades this itself
-- (EnsurePublicMigrationsTable), so the copy here exists only for the paths that
-- apply this file WITHOUT migratekit in the loop — notably sqlc's db-prepare,
-- and 0001_schema.up.sql's `GRANT SELECT ON TABLE public.migrations`.
--
-- It must therefore match migratekit's CURRENT shape. It previously did not: it
-- predated the `schema` column and the wider UNIQUE (app, database, schema, name)
-- that distinguishes two schemas of one database, so every fresh volume booted
-- into `column "schema" does not exist`. Keep this in step with
-- migratekit/internal/coremigrate; CREATE IF NOT EXISTS means migratekit's own
-- ensure is then a no-op rather than a conflict.
CREATE TABLE IF NOT EXISTS public.migrations (
    id BIGSERIAL PRIMARY KEY,
    app TEXT NOT NULL,
    database TEXT NOT NULL,
    name TEXT NOT NULL,
    schema TEXT NOT NULL DEFAULT '',
    migrated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(app, database, schema, name)
);
