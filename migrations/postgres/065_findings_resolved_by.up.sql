-- #692 operator findings queue: attribution for manual resolutions.
-- resolved_by is the authenticated admin identity stamped on every
-- approve/ignore through the admin findings API; NULL for automatic
-- resolutions (auto_vanished / enforced).

ALTER TABLE openrails.reconciliation_findings
    ADD COLUMN IF NOT EXISTS resolved_by text;

COMMENT ON COLUMN openrails.reconciliation_findings.resolved_by IS
    'Authenticated admin identity stamped on manual resolution (approve/ignore via the findings queue, #692); NULL for automatic resolutions.';
