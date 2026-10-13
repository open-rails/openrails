-- A pull.* finding about a provider record that is not this merchant's
-- obligation (another environment's schedule on a shared gateway) resolves as
-- foreign: settled for provider recovery, where a dismissed one still holds.
ALTER TABLE billing.reconciliation_findings
    DROP CONSTRAINT reconciliation_findings_resolution_check,
    ADD CONSTRAINT reconciliation_findings_resolution_check CHECK (resolution IS NULL OR resolution IN
        ('auto_vanished','enforced','admin_fixed','ignored','approved','dismissed','superseded','foreign')),
    DROP CONSTRAINT reconciliation_findings_proposal_resolution_check,
    ADD CONSTRAINT reconciliation_findings_proposal_resolution_check CHECK (
        resolution IS NULL OR resolution NOT IN ('approved','dismissed','superseded','foreign') OR finding_type LIKE 'pull.%');
