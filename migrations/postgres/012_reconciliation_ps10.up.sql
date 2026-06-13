-- #107/#358 tie-in: PS-10 stuck provider intents. A openrails.provider_intents
-- row (#358 ledger) that sits non-terminal beyond the reconcile engine's
-- hardcoded thresholds surfaces as a reconcile finding instead of rotting
-- silently. Extend the finding-type CHECK with the new taxonomy entry.

ALTER TABLE openrails.reconciliation_findings
    DROP CONSTRAINT chk_reconciliation_findings_type;

ALTER TABLE openrails.reconciliation_findings
    ADD CONSTRAINT chk_reconciliation_findings_type
    CHECK ((finding_type = ANY (ARRAY['PS-1'::text, 'PS-2'::text, 'PS-3'::text, 'PS-4'::text, 'PS-5'::text, 'PS-6'::text, 'PS-7'::text, 'PS-8'::text, 'PS-9'::text, 'PS-10'::text])));
