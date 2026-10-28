-- Repair: none-needed. Job health is read from River's own tables and long
-- passes continue in new jobs, so worker_state (health rows and the sweep
-- cursor) goes, with the stall findings only it raised.
DELETE FROM billing.reconciliation_findings WHERE finding_type = 'life.worker.stalled';
DROP TABLE billing.worker_state;
