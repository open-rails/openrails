-- Reverting drops the memory of every custody flip: which PSP vault an
-- instrument came from, and which run moved it. The instruments themselves are
-- untouched — they keep charging through the custodian — so a revert makes the
-- current state unexplainable rather than wrong. Export the table first.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DROP TABLE IF EXISTS openrails.custody_migrations;
