-- Validate the th-005 final-settlement shape in a separate migration
-- transaction. PostgreSQL enforces the NOT VALID check for new rows immediately;
-- this scan proves any pre-existing settled rows carry complete derived evidence.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE ONLY openrails.operation_authorizations
    VALIDATE CONSTRAINT operation_authorizations_settlement_shape;
