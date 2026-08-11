-- Remove the or#906 deposit once-only unique. Going back, deposit dedupe rests
-- on depositTx's check-then-insert under lockBalance alone again — the
-- non-unique idx_grants_source (0001) still serves the lookups.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DROP INDEX openrails.uq_grants_credit_deposit_once;
