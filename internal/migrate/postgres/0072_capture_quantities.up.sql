-- parent: 71 sha256:0c1ef001a156f451cd9cf2c95eecaf0d652cbf812ec1d801140aba3d57e3e755
-- Repair: none-needed Both columns are new and null on every stored row, which the check accepts.
-- An admission may hold an estimate of its usage, priced by the meters' rate
-- cards; the cards that priced it are pinned with it, and its capture is
-- priced by them.
ALTER TABLE billing.admission_operations ADD COLUMN estimate_usage jsonb;
ALTER TABLE billing.admission_operations ADD COLUMN rate_cards jsonb;
ALTER TABLE billing.admission_operations ADD CONSTRAINT admission_operations_estimate_usage_check CHECK (
    (estimate_usage IS NULL) = (rate_cards IS NULL)
    AND (estimate_usage IS NULL OR (jsonb_typeof(estimate_usage) = 'object' AND octet_length(estimate_usage::text) <= 65536))
    AND (rate_cards IS NULL OR (jsonb_typeof(rate_cards) = 'array' AND octet_length(rate_cards::text) <= 262144)));
COMMENT ON COLUMN billing.admission_operations.estimate_usage IS 'The usage the request expects ({event_type, dimensions, metadata}); estimated_amount is its price when admitted. Null for a host-stated estimate.';
COMMENT ON COLUMN billing.admission_operations.rate_cards IS 'The rate cards that priced estimate_usage, pinned: the capture is priced by them, whatever the catalog says later.';
