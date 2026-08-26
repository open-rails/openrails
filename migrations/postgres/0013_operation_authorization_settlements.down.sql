DROP TABLE IF EXISTS openrails.operation_authorization_settlements;

ALTER TABLE openrails.operation_authorizations
    DROP CONSTRAINT IF EXISTS operation_authorizations_release_has_no_capture,
    DROP CONSTRAINT IF EXISTS operation_authorizations_captured_nonnegative,
    DROP COLUMN IF EXISTS captured_usd_micros;
