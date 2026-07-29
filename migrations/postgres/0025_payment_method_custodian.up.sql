-- or#880 phase 1: custody becomes its own axis.
--
-- Two orthogonal facts were collapsed into one. WHO CHARGES the card is
-- rail + psp_id and was already modelled. WHO HOLDS the card was smuggled
-- into vault_provider, whose '' silently meant "held at the processor" — an
-- absence standing in for a fact.
--
--   1. vault_provider -> custodian. `vault` is reserved for HashiCorp in this
--      codebase (or#871); custodian names exactly what the column records.
--   2. '' -> 'psp'. "Stored at the processor itself" becomes a STATED value,
--      the same no-silent-default rule the currency work applied (or#830/#864).
--   3. A CHECK pins the vocabulary, so an unstated custodian is impossible
--      rather than merely unusual.
--
-- The matrix this makes statable (docs/payment-method-custody.md):
--
--   custodian    | processor (rail)   | example
--   psp          | stripe             | pm_ on a Stripe Customer
--   psp          | nmi (mobius)       | customer_vault_id
--   basis_theory | nmi (mobius)       | today's rail='vaulted_card'
--
-- "No stored instrument" — CCBill owns the subscription, Solana rides a
-- delegation — is represented by the ABSENCE OF A ROW, not by a custodian
-- value. A payment_methods row IS the record of a held instrument, so a
-- 'none' custodian would be self-contradictory. Verified 2026-07-29: no code
-- path writes a payment_methods row for either rail (the three writers are
-- the NMI vault, the Stripe card path and the Basis Theory sale; the legacy
-- importer mirrors provider-held vaults).
--
-- Behaviour-neutral by construction: the rail enum is untouched (retiring
-- rail='vaulted_card' is phase 2 / or#879), and the backfill only names the
-- fact each row already carried.

BEGIN;

ALTER TABLE openrails.payment_methods RENAME COLUMN vault_provider TO custodian;

UPDATE openrails.payment_methods SET custodian = 'psp' WHERE custodian = '';

ALTER TABLE openrails.payment_methods
    ALTER COLUMN custodian SET DEFAULT 'psp'::text,
    ADD CONSTRAINT payment_methods_custodian_check
        CHECK ((custodian = ANY (ARRAY['psp'::text, 'basis_theory'::text])));

COMMENT ON COLUMN openrails.payment_methods.custodian IS 'or#880 who HOLDS this instrument, orthogonal to who charges it (rail + psp_id): psp = stored at the processor itself (Stripe pm_, NMI customer vault) | basis_theory = neutral third-party vault (#795). Never empty — "no stored instrument" (CCBill, Solana) is the absence of a row, not a custodian value.';

COMMIT;
