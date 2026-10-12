-- A batch is remembered by content independently of subsequent edits. Preserve
-- all historical receipts, including repeats written before hash replay was permanent.
CREATE INDEX catalog_applications_merchant_hash_idx
ON billing.catalog_applications (merchant_id, request_sha256, applied_revision DESC);
