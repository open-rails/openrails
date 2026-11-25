-- A dispute finding names the card PSP it is about: an unknown charge or a
-- program's ratio is one account's.
ALTER TABLE billing.reconciliation_findings DROP CONSTRAINT reconciliation_findings_scope_check;
ALTER TABLE billing.reconciliation_findings ADD CONSTRAINT reconciliation_findings_scope_check CHECK (
    (finding_type IN ('catalog.price.unmapped', 'life.subscription.unattributed') AND rail IS NOT NULL AND psp_id IS NOT NULL)
    OR (finding_type NOT LIKE 'catalog.%' AND finding_type <> 'life.subscription.unattributed' AND (
        (rail IS NULL AND psp_id IS NULL)
        OR (finding_type LIKE 'pull.%' AND rail IS NOT NULL AND psp_id IS NOT NULL)
        OR (finding_type LIKE 'life.disputes.%' AND rail IS NOT NULL AND psp_id IS NOT NULL)))
);
