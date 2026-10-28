-- Repair: none-needed. OpenRails owns its catalog; nothing compares provider
-- catalogs any more, so catalog drift findings and their columns go. Provider
-- price findings (catalog.price.unmapped) stay.
DELETE FROM billing.reconciliation_findings WHERE finding_type IN ('catalog.orphan_in_stripe', 'catalog.missing_in_stripe',
    'catalog.orphan_in_nmi', 'catalog.missing_in_nmi', 'catalog.missing_in_solana', 'catalog.field_drift');
DROP INDEX billing.reconciliation_findings_open_catalog_idx;
ALTER TABLE billing.reconciliation_findings
    DROP CONSTRAINT reconciliation_findings_scope_check,
    DROP COLUMN openrails_resource_type,
    DROP COLUMN openrails_resource_id,
    DROP COLUMN external_resource_id,
    DROP COLUMN field,
    DROP COLUMN openrails_value,
    DROP COLUMN external_value,
    ADD CONSTRAINT reconciliation_findings_scope_check CHECK (
        (finding_type IN ('catalog.price.unmapped', 'life.subscription.unattributed') AND rail IS NOT NULL AND psp_id IS NOT NULL)
        OR (finding_type NOT LIKE 'catalog.%' AND finding_type <> 'life.subscription.unattributed'
            AND ((rail IS NULL AND psp_id IS NULL) OR (finding_type LIKE 'pull.%' AND rail IS NOT NULL AND psp_id IS NOT NULL))));
COMMENT ON TABLE billing.reconciliation_findings IS 'Durable reconciliation findings ledger. Stable identity per (merchant, finding_type, psp_id, subject_key): pull.*, catalog.price.unmapped and life.subscription.unattributed findings name the PSP whose read raised them. Statuses: reconcile_required, requires_review, auto_fixed, fixed, ignored. Retention: resolved findings are deleted 12 months (366 days) after they were resolved and last seen.';
COMMENT ON COLUMN billing.reconciliation_findings.psp_id IS 'pull.*, catalog.price.unmapped and life.subscription.unattributed findings: the PSP whose read raised the finding. Part of the identity; absence can be proven only by a complete read of this PSP.';
