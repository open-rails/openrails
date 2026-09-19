-- PostgreSQL Bootstrap
-- Simple initialization: create the required OpenRails schema (default openrails) and install extensions.
-- Open Rails Billing is designed to run standalone; do not create schemas for other apps here.

-- Create schemas
CREATE SCHEMA IF NOT EXISTS openrails;

-- Install required extensions in public schema.
-- Billing generates ids with the Postgres 18 built-in uuidv7() (no extension
-- needed). pgcrypto is kept for any incidental hashing/random needs.
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;

-- Tracker tables are initialized by migratekit before application migrations.
