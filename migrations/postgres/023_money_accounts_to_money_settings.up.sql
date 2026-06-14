--
-- Rename openrails.money_accounts -> money_settings (#491). The table holds NO
-- money: it is the per-(merchant, customer, currency) spend POLICY / settings row
-- (billing mode, caps, auto-topup, alerts, suspension, tier). Every money_* table
-- FKs to customers(id), never to this row; the "account" name collided with the
-- customer (the actual account/balance).
--
-- The TABLE rename is a plain `ALTER TABLE IF EXISTS ... RENAME TO` (not a DO
-- block) on purpose: sqlc's schema parser reads migrations/ but skips PL/pgSQL DO
-- blocks, so the table rename must be a top-level statement for sqlc to see the
-- final name. IF EXISTS makes it a no-op if the table is already money_settings.
-- The constraint/index renames stay in guarded DO blocks (sqlc ignores them; only
-- the live DB runner needs them) so the whole migration is idempotent.
--
--   * openrails.money_accounts table              -> money_settings
--   * constraints/indexes carrying "money_accounts" -> "money_settings"
--   * column names are UNCHANGED.
--
SET statement_timeout = '300s';
SET lock_timeout = '10s';

-- Table rename (top-level so sqlc's parser tracks it; IF EXISTS = idempotent).
ALTER TABLE IF EXISTS openrails.money_accounts RENAME TO money_settings;

-- Rename constraints + indexes whose names carry "money_accounts" -> "money_settings".
DO $$
DECLARE
    r record;
BEGIN
    -- constraints (PK/FK/CHECK/UNIQUE/NOT NULL)
    FOR r IN
        SELECT con.conname, rel.relname AS table_name
          FROM pg_constraint con
          JOIN pg_class rel ON rel.oid = con.conrelid
          JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
         WHERE nsp.nspname = 'openrails' AND con.conname LIKE '%money_accounts%'
    LOOP
        EXECUTE format('ALTER TABLE openrails.%I RENAME CONSTRAINT %I TO %I',
                       r.table_name, r.conname,
                       replace(r.conname, 'money_accounts', 'money_settings'));
    END LOOP;

    -- indexes not backing a constraint (e.g. uq_money_accounts_payer)
    FOR r IN
        SELECT c.relname AS idxname
          FROM pg_class c
          JOIN pg_namespace nsp ON nsp.oid = c.relnamespace
         WHERE nsp.nspname = 'openrails' AND c.relkind = 'i'
           AND c.relname LIKE '%money_accounts%'
           AND NOT EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = c.oid)
    LOOP
        EXECUTE format('ALTER INDEX openrails.%I RENAME TO %I',
                       r.idxname, replace(r.idxname, 'money_accounts', 'money_settings'));
    END LOOP;
END $$;
