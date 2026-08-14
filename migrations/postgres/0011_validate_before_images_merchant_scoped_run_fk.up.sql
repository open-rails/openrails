-- Validate or#902's merchant-scoped before-image run FK in a separate
-- migration transaction. PostgreSQL enforces the NOT VALID constraint for new
-- rows immediately; this scan proves all pre-existing evidence has matching
-- merchant provenance without retaining the stronger DDL lock from 0010.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE ONLY openrails.destructive_run_before_images
    VALIDATE CONSTRAINT destructive_run_before_images_run_fk;
