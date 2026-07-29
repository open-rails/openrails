BEGIN;

ALTER TABLE openrails.payment_methods
    DROP CONSTRAINT IF EXISTS payment_methods_custodian_check;

ALTER TABLE openrails.payment_methods ALTER COLUMN custodian SET DEFAULT ''::text;

UPDATE openrails.payment_methods SET custodian = '' WHERE custodian = 'psp';

ALTER TABLE openrails.payment_methods RENAME COLUMN custodian TO vault_provider;

COMMENT ON COLUMN openrails.payment_methods.vault_provider IS '#795 neutral card vault holding this instrument (''basis_theory'' on vaulted_card rows; '''' elsewhere).';

COMMIT;
