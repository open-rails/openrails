-- Repair: none-needed. Existing imports were all created by a person, so they
-- take origin 'manual'; the first configuration load after this migration
-- creates each live PSP's onboarding import.
ALTER TABLE billing.psp_imports ADD COLUMN origin text NOT NULL DEFAULT 'manual';
ALTER TABLE billing.psp_imports ALTER COLUMN origin DROP DEFAULT;
ALTER TABLE billing.psp_imports ADD COLUMN error_code text;
ALTER TABLE billing.psp_imports
    ADD CONSTRAINT psp_imports_origin_check CHECK (origin IN ('onboarding', 'manual')),
    ADD CONSTRAINT psp_imports_error_code_check CHECK (error_code IS NULL OR status = 'failed' AND error_code IN ('psp_credentials_rejected', 'psp_not_readable'));
CREATE UNIQUE INDEX psp_imports_onboarding_key ON billing.psp_imports (merchant_id, psp_id) WHERE origin = 'onboarding' AND status <> 'failed';
COMMENT ON COLUMN billing.psp_imports.origin IS 'onboarding: the one pull OpenRails starts by itself when a PSP is first loaded (re-armed in place when it failed and the PSP is loaded again); manual: created by a person.';
COMMENT ON COLUMN billing.psp_imports.error_code IS 'Why a failed import stopped, when the cause is a configuration one: psp_credentials_rejected (the provider refused the credentials) or psp_not_readable (the PSP, its rail''s import or a credential is not declared).';
