-- Repair: none-needed. A merchant purge queues its Vault cleanup as a River
-- job in its own transaction; no poll scans pending cleanups.
DROP INDEX billing.maintenance_runs_id_idx;
