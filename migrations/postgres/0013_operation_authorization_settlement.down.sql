ALTER TABLE openrails.operation_authorizations
    DROP CONSTRAINT IF EXISTS operation_authorizations_settlement_shape,
    DROP COLUMN IF EXISTS settlement_body_digest,
    DROP COLUMN IF EXISTS settlement_body_bytes,
    DROP COLUMN IF EXISTS settlement_rated_usd_micros;
