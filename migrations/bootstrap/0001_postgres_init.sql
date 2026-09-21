-- PostgreSQL Bootstrap
-- Install shared extensions. The OpenRails initializer creates its configured
-- billing schema (default billing); SQLC explicitly chooses canonical openrails.
-- Open Rails Billing is designed to run standalone; do not create schemas for other apps here.

-- Install required extensions in public schema.
-- Billing generates ids with the Postgres 18 built-in uuidv7() (no extension
-- needed). pgcrypto is kept for any incidental hashing/random needs.
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;

-- Tracker tables are initialized by migratekit before application migrations.
