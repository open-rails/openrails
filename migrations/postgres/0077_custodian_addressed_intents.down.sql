-- Structural reverse of 0077. Custodian-addressed rows are DELETED rather than
-- forced back into a psp_id they never had: re-imposing NOT NULL means the
-- class that migration exists for is unrepresentable again, and a fabricated
-- gateway account is worse than no row.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DELETE FROM openrails.rail_mutation_logs WHERE psp_id IS NULL;
DELETE FROM openrails.rail_intents WHERE psp_id IS NULL;

ALTER TABLE openrails.rail_mutation_logs DROP CONSTRAINT rail_mutation_logs_addressed;
ALTER TABLE openrails.rail_mutation_logs ALTER COLUMN psp_id SET NOT NULL;
DROP INDEX openrails.idx_rail_mutation_logs_custodian;
ALTER TABLE openrails.rail_mutation_logs DROP CONSTRAINT rail_mutation_logs_custodian_fk;
ALTER TABLE openrails.rail_mutation_logs DROP COLUMN custodian_id;

ALTER TABLE openrails.rail_intents DROP CONSTRAINT rail_intents_addressed;
ALTER TABLE openrails.rail_intents ALTER COLUMN psp_id SET NOT NULL;
DROP INDEX openrails.idx_rail_intents_custodian;
ALTER TABLE openrails.rail_intents DROP CONSTRAINT rail_intents_custodian_fk;
ALTER TABLE openrails.rail_intents DROP COLUMN custodian_id;

COMMENT ON COLUMN openrails.rail_intents.psp_id IS
    'PSP the outbound intent was enqueued against. Required (or#893).';
COMMENT ON COLUMN openrails.rail_mutation_logs.psp_id IS
    'PSP the logged mutation was addressed to. Required (or#893).';
