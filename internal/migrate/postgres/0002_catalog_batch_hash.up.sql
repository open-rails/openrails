-- parent: 1 sha256:bc35a286355798adbb579dace4c7f058a438ab3e8db601bd57f83f22d47ccb0d
-- A batch is remembered by content independently of subsequent edits. Preserve
-- all historical receipts, including repeats written before hash replay was permanent.
CREATE INDEX catalog_applications_merchant_hash_idx
ON billing.catalog_applications (merchant_id, request_sha256, applied_revision DESC);
