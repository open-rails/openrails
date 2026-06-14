--
-- Make openrails.customers a PURE balance account (#491, slice 1): drop the
-- (issuer, subject) auth key. A customer is now (id, merchant_id, created_at,
-- last_seen_at) — id is the payable UUID (#364), merchant_id stays for
-- scoping/RLS. issuer/subject were largely redundant: the dominant self path
-- already set id = the subject UUID and issuer = a placeholder. The federated
-- (issuer, subject) -> customer mapping moves to a deterministic UUIDv5 derived
-- in TouchCustomer, so no lookup column is needed.
--
-- Existing rows are preserved verbatim — each keeps its UUID id as its own
-- payable balance. (The federated "one payer per issuer" collapse is a separate
-- semantic migration, not required to drop these columns.)
--
-- Idempotent: every step is guarded, so this is a no-op on a fresh DB whose 001
-- baseline already declares the columnless shape, and re-runnable on an old DB.
--
SET statement_timeout = '300s';
SET lock_timeout = '10s';

-- Drop the UNIQUE(merchant_id, issuer, subject) key (guarded).
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint con
          JOIN pg_class rel ON rel.oid = con.conrelid
          JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
         WHERE nsp.nspname = 'openrails' AND rel.relname = 'customers'
           AND con.conname = 'uq_customers_issuer_subject'
    ) THEN
        ALTER TABLE openrails.customers DROP CONSTRAINT uq_customers_issuer_subject;
    END IF;
END $$;

-- Drop the columns (guarded; column comments drop with the columns).
ALTER TABLE openrails.customers DROP COLUMN IF EXISTS issuer;
ALTER TABLE openrails.customers DROP COLUMN IF EXISTS subject;

COMMENT ON TABLE openrails.customers IS 'OpenRails payable identity — a PURE balance account keyed by its UUID id (#491). (id, merchant_id): id is the payable UUID (#364); merchant_id scopes it for RLS. NOT verified by OpenRails (the merchant asserts it). All money_* tables FK customers(id).';
