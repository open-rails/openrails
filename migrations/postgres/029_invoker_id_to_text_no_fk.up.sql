-- #491 REVERSAL (owner 2026-06-15): the INVOKER is OPAQUE TEXT, not an FK.
-- Migration 027 added `invoker_id uuid` + a cross-schema FK -> profiles.delegated_users(id)
-- on the three spend-attribution tables. That was wrong: the invoker ("under whose
-- authority an action happened") is a POLYMORPHIC principal (native-user | delegated-user
-- | service-token | issuer/JWKS) living in different authkit tables (separate schema) or
-- even a different app — OpenRails can't FK to it. The opaque invoker is already the
-- `actor` text column on each table; drop the redundant uuid FK column. authkit#81's
-- delegated_users is dropped in tandem (authkit migration 012). NEW numbered migration;
-- idempotent. (The customers org_id/issuer/subject natural-key columns from 027 STAY —
-- that resolution is correct and unaffected.)

SET lock_timeout = '10s';
SET statement_timeout = '300s';

ALTER TABLE openrails.money_transactions DROP CONSTRAINT IF EXISTS money_transactions_invoker_fk;
ALTER TABLE openrails.usage_events       DROP CONSTRAINT IF EXISTS usage_events_invoker_fk;
ALTER TABLE openrails.money_spend_limits DROP CONSTRAINT IF EXISTS money_spend_limits_invoker_fk;

DROP INDEX IF EXISTS openrails.idx_money_transactions_invoker;
DROP INDEX IF EXISTS openrails.idx_usage_events_invoker;
DROP INDEX IF EXISTS openrails.idx_money_spend_limits_invoker;

ALTER TABLE openrails.money_transactions DROP COLUMN IF EXISTS invoker_id;
ALTER TABLE openrails.usage_events       DROP COLUMN IF EXISTS invoker_id;
ALTER TABLE openrails.money_spend_limits DROP COLUMN IF EXISTS invoker_id;
