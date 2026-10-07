-- Configuration-managed catalogs advance by merchant-authored version. Keep
-- their ordering and checksum in the permanent, merchant-archived receipt.
ALTER TABLE billing.catalog_applications
    ADD COLUMN catalog_version bigint CHECK (catalog_version > 0);

CREATE UNIQUE INDEX catalog_applications_merchant_version_key
    ON billing.catalog_applications (merchant_id, catalog_version DESC)
    WHERE catalog_version IS NOT NULL;
