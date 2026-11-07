-- Repair: none-needed. Existing imported roots keep their source identity;
-- nullable import metadata is populated only by subsequent declarations.
ALTER TABLE billing.grants ADD COLUMN import_as_of timestamptz;
ALTER TABLE billing.grants ADD COLUMN import_root_id uuid;
ALTER TABLE billing.grants ADD CONSTRAINT grants_import_metadata_check CHECK (
    (import_as_of IS NULL AND import_root_id IS NULL) OR
    (import_as_of IS NOT NULL AND kind = 'access' AND event = 'grant'
     AND source_type = 'grant' AND grant_reason = 'import' AND actor = 'import')
);
ALTER TABLE billing.grants ADD CONSTRAINT grants_import_root_id_fkey
    FOREIGN KEY (merchant_id, customer_id, import_root_id) REFERENCES billing.grants (merchant_id, customer_id, id);
COMMENT ON COLUMN billing.grants.import_as_of IS 'Evidence horizon of an imported admin grant declaration. NULL on grants recorded before import revisions were supported.';
COMMENT ON COLUMN billing.grants.import_root_id IS 'Original imported admin grant for a replacement window. Replacements take effect no earlier than import_as_of; the root retains the permanent source key.';

DROP INDEX billing.grants_access_once_key;
CREATE UNIQUE INDEX grants_access_once_key ON billing.grants (merchant_id, customer_id, product_id, source_type, source_id, starts_at)
    WHERE kind = 'access' AND event = 'grant' AND source_type <> 'grace' AND import_root_id IS NULL;
DROP INDEX billing.grants_access_idempotency_key;
CREATE UNIQUE INDEX grants_access_idempotency_key ON billing.grants (merchant_id, source_id)
    WHERE kind = 'access' AND event = 'grant' AND source_type = 'grant' AND grant_reason <> 'migration' AND import_root_id IS NULL;
COMMENT ON INDEX billing.grants_access_idempotency_key IS 'A free product grant root permanently owns its source id. Imported replacements reference that root.';
CREATE UNIQUE INDEX grants_import_revision_key ON billing.grants (merchant_id, source_id, import_as_of)
    WHERE import_as_of IS NOT NULL;
