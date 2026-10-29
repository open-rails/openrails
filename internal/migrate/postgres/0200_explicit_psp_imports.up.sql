-- Repair: none-needed. A person or host creates every PSP import; OpenRails
-- starts none by itself, so imports lose their origin and the credentials
-- finding only the unattended onboarding pull raised goes.
DELETE FROM billing.reconciliation_findings WHERE finding_type = 'consistency.psp_import.credentials_rejected';
DROP INDEX billing.psp_imports_onboarding_key;
ALTER TABLE billing.psp_imports DROP CONSTRAINT psp_imports_origin_check, DROP COLUMN origin;
