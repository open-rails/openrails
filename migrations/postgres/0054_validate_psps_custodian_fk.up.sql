-- or#880 phase 3, step two: validate the custody reference added NOT VALID by
-- 0053. Its own transaction, so the scan takes SHARE UPDATE EXCLUSIVE and does
-- not block writes to openrails.psps (0032/0043 precedent). Every row entering
-- the scan has custodian_id IS NULL, so it is a formality — but it is the
-- formality that lets the planner and future readers trust the constraint.
SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE openrails.psps VALIDATE CONSTRAINT psps_custodian_fk;
