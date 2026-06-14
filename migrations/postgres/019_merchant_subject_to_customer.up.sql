--
-- Rename the payable identity merchant_subject -> CUSTOMER (#486). The OpenRails
-- payable identity is a customer (NMI/Stripe's own word), still MERCHANT-SCOPED
-- (implicit, via merchant_id; the UNIQUE(merchant_id, issuer, subject) stays). This
-- migrates EXISTING databases to the customer naming the consolidated 001 baseline
-- now declares. It is fully idempotent (every step is guarded), so it is a no-op on
-- a fresh DB whose 001 already created the customer_* objects.
--
--   * openrails.merchant_subjects table     -> customers
--   * merchant_subject_id FK column         -> customer_id (every referencing table)
--   * constraints/indexes carrying          -> customer
--     "merchant_subject" (uq_merchant_subjects_* -> uq_customers_*,
--     *_merchant_subject_fk -> *_customer_fk)
--   * processor_customers TABLE NAME is unchanged (now reads customer->processor_customer)
--   * merchant / merchant_id / owner_tenant_id are NOT touched.
--
SET statement_timeout = '300s';
SET lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- 1. Table rename: merchant_subjects -> customers.
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF to_regclass('openrails.merchant_subjects') IS NOT NULL
       AND to_regclass('openrails.customers') IS NULL THEN
        ALTER TABLE openrails.merchant_subjects RENAME TO customers;
    END IF;
END $$;

-- ---------------------------------------------------------------------------
-- 2a. processor_customers carries its OWN customer_id (the processor's external
--     customer id, e.g. Stripe cus_*). Disambiguate it to processor_customer_id
--     first, so our FK can take the customer_id name in 2b (customer ->
--     processor_customer hierarchy). Guarded: only when the old layout exists.
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
         WHERE table_schema = 'openrails' AND table_name = 'processor_customers'
           AND column_name = 'merchant_subject_id'
    ) AND EXISTS (
        SELECT 1 FROM information_schema.columns
         WHERE table_schema = 'openrails' AND table_name = 'processor_customers'
           AND column_name = 'customer_id'
    ) THEN
        ALTER TABLE openrails.processor_customers RENAME COLUMN customer_id TO processor_customer_id;
        -- move its NOT NULL constraint name out of the way too, so step 3 can give
        -- the customer_id name to the (renamed) merchant_subject_id NOT NULL.
        IF EXISTS (
            SELECT 1 FROM pg_constraint con
              JOIN pg_class r ON r.oid = con.conrelid
              JOIN pg_namespace ns ON ns.oid = r.relnamespace
             WHERE ns.nspname = 'openrails' AND r.relname = 'processor_customers'
               AND con.conname = 'processor_customers_customer_id_not_null'
        ) THEN
            ALTER TABLE openrails.processor_customers
                RENAME CONSTRAINT processor_customers_customer_id_not_null
                TO processor_customers_processor_customer_id_not_null;
        END IF;
    END IF;
END $$;

-- ---------------------------------------------------------------------------
-- 2b. Column rename: merchant_subject_id -> customer_id across every openrails
--    table that still carries the old column name.
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    r record;
BEGIN
    FOR r IN
        SELECT c.table_name
          FROM information_schema.columns c
         WHERE c.table_schema = 'openrails'
           AND c.column_name = 'merchant_subject_id'
    LOOP
        EXECUTE format('ALTER TABLE openrails.%I RENAME COLUMN merchant_subject_id TO customer_id',
                       r.table_name);
    END LOOP;
END $$;

-- ---------------------------------------------------------------------------
-- 3. Rename constraints + indexes whose names carry "merchant_subject".
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    r record;
BEGIN
    -- constraints (PK/FK/CHECK/UNIQUE/EXCLUDE)
    FOR r IN
        SELECT con.conname, rel.relname AS table_name
          FROM pg_constraint con
          JOIN pg_class rel ON rel.oid = con.conrelid
          JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
         WHERE nsp.nspname = 'openrails' AND con.conname LIKE '%merchant_subject%'
    LOOP
        EXECUTE format('ALTER TABLE openrails.%I RENAME CONSTRAINT %I TO %I',
                       r.table_name, r.conname,
                       replace(r.conname, 'merchant_subject', 'customer'));
    END LOOP;

    -- indexes not backing a constraint (partial/expression/plain)
    FOR r IN
        SELECT c.relname AS idxname
          FROM pg_class c
          JOIN pg_namespace nsp ON nsp.oid = c.relnamespace
         WHERE nsp.nspname = 'openrails' AND c.relkind = 'i'
           AND c.relname LIKE '%merchant_subject%'
           AND NOT EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = c.oid)
    LOOP
        EXECUTE format('ALTER INDEX openrails.%I RENAME TO %I',
                       r.idxname, replace(r.idxname, 'merchant_subject', 'customer'));
    END LOOP;
END $$;
