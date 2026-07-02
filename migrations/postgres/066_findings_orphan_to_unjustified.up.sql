-- #690 three-error-category taxonomy (Orphaned / Freeloader / Double-Billed):
-- "orphaned" is reserved for exactly ONE operator-facing meaning — a PAYING
-- member without access (the MISSING side: derive.grant.missing et al).
-- The freeloader-side finding `derive.entitlement.orphan` (a live window whose
-- justification chain is proven broken — access WITHOUT payment) collided with
-- that word; rename it to `derive.entitlement.unjustified` so stable finding
-- identities keep upserting in place under the new name.
-- (chk_reconciliation_findings_type is pattern-based —
-- ^(pull|derive|life|consistency)\.[a-z0-9_]+(\.[a-z0-9_]+)?$ — the new name
-- passes; no constraint change. Same pattern as 060.)
UPDATE openrails.reconciliation_findings
   SET finding_type = 'derive.entitlement.unjustified'
 WHERE finding_type = 'derive.entitlement.orphan';
