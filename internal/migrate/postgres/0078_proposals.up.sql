-- A pull.* finding is a proposal: it carries the change approving it applies,
-- who raised it, a digest of the provider evidence it rests on and, once
-- approved, the approval run that applied it. Approval runs replace
-- converge-enforce runs on the maintenance ledger.

ALTER TABLE billing.reconciliation_findings
    ADD COLUMN change jsonb,
    ADD COLUMN source text,
    ADD COLUMN evidence_digest text,
    ADD COLUMN approval_run_id uuid;

-- Existing pull.* findings name their raiser: a PSP read (the pull) or the
-- by-id planes that raise without a PSP (verification).
UPDATE billing.reconciliation_findings
SET source = CASE WHEN psp_id IS NULL AND finding_type <> 'pull.ccbill.active_without_payment' THEN 'verification' ELSE 'pull' END
WHERE finding_type LIKE 'pull.%';

ALTER TABLE billing.reconciliation_findings
    DROP CONSTRAINT reconciliation_findings_resolution_check,
    ADD CONSTRAINT reconciliation_findings_resolution_check CHECK (resolution IS NULL OR resolution IN
        ('auto_vanished','enforced','admin_fixed','ignored','approved','dismissed','superseded')),
    ADD CONSTRAINT reconciliation_findings_source_check CHECK (
        (finding_type LIKE 'pull.%') = (source IS NOT NULL) AND (source IS NULL OR source IN ('pull','verification'))),
    ADD CONSTRAINT reconciliation_findings_change_check CHECK (change IS NULL OR (
        finding_type LIKE 'pull.%' AND jsonb_typeof(change) = 'object'
        AND change->>'kind' IN ('create_subscription','record_charge','record_refund','adopt_subscription',
            'cancel_subscription','update_card','revoke_access','refund_charge_after_cancel',
            'cancel_provider_schedule','collect_skipped_renewal'))),
    ADD CONSTRAINT reconciliation_findings_evidence_digest_check CHECK (
        (change IS NULL) = (evidence_digest IS NULL) AND (evidence_digest IS NULL OR evidence_digest ~ '^[0-9a-f]{64}$')),
    ADD CONSTRAINT reconciliation_findings_proposal_resolution_check CHECK (
        resolution IS NULL OR resolution NOT IN ('approved','dismissed','superseded') OR finding_type LIKE 'pull.%'),
    ADD CONSTRAINT reconciliation_findings_approval_run_check CHECK (
        (resolution IS NOT DISTINCT FROM 'approved') = (approval_run_id IS NOT NULL)),
    ADD CONSTRAINT reconciliation_findings_approval_run_id_fkey FOREIGN KEY (merchant_id, approval_run_id)
        REFERENCES billing.maintenance_runs(merchant_id, id) ON DELETE RESTRICT;

CREATE INDEX reconciliation_findings_approval_run_id_idx ON billing.reconciliation_findings (merchant_id, approval_run_id)
    WHERE approval_run_id IS NOT NULL;
-- The proposals list: pull.* findings by status, source and PSP.
CREATE INDEX reconciliation_findings_proposals_idx ON billing.reconciliation_findings (merchant_id, status, source, psp_id)
    WHERE finding_type LIKE 'pull.%';

COMMENT ON COLUMN billing.reconciliation_findings.change IS 'A pull.* proposal''s change: its kind, subject, from and to, effects, the basis it was computed against, when its evidence was observed, the provider evidence and what approving it executes. NULL on findings only dismissing resolves.';
COMMENT ON COLUMN billing.reconciliation_findings.source IS 'What raised a pull.* finding: a PSP read (pull) or a by-id read of an uncertain record (verification). Set exactly on pull.* findings.';
COMMENT ON COLUMN billing.reconciliation_findings.evidence_digest IS 'sha256 of the change''s kind and provider evidence. A dismissed proposal is raised again only when a read yields another digest.';
COMMENT ON COLUMN billing.reconciliation_findings.approval_run_id IS 'The approval run (maintenance_runs kind approval) that applied this proposal; undo-run reverses it. Set exactly when the resolution is approved.';

-- Converge-enforce runs become approval runs. kind is a fact column, so its
-- guard is lifted for this rename only.
ALTER TABLE billing.maintenance_runs DROP CONSTRAINT maintenance_runs_kind_check;
ALTER TABLE billing.maintenance_runs DISABLE TRIGGER immutable_maintenance_run_facts;
UPDATE billing.maintenance_runs SET kind = 'approval' WHERE kind = 'converge_enforce';
ALTER TABLE billing.maintenance_runs ENABLE TRIGGER immutable_maintenance_run_facts;

ALTER TABLE billing.maintenance_runs
    ADD CONSTRAINT maintenance_runs_kind_check CHECK ((
        (kind = 'reconciliation' AND mode IN ('advisory','enforce')
         AND status IN ('running','completed','failed') AND psp_id IS NULL
         AND NOT dry_run AND coverage IS NULL AND expected_rows IS NULL AND affected IS NULL
         AND reversed_at IS NULL AND reversed_by IS NULL AND inventory_manifest IS NULL AND inventory_total_rows IS NULL)
        OR (kind IN ('prune','approval','merchant_purge') AND btrim(actor) <> ''
         AND mode IS NULL AND cardinality(rails) = 0 AND window_starts_at IS NULL AND window_ends_at IS NULL
         AND summary IS NULL AND error IS NULL AND inventory_manifest IS NULL AND inventory_total_rows IS NULL)
        OR (kind = 'billing_restore' AND actor = 'merchantarchive' AND status IN ('running','completed')
         AND mode IS NULL AND cardinality(rails)=0 AND psp_id IS NULL AND NOT dry_run
         AND window_starts_at IS NULL AND window_ends_at IS NULL AND coverage IS NULL AND expected_rows IS NULL
         AND affected IS NULL AND reversed_at IS NULL AND reversed_by IS NULL AND note IS NULL AND error IS NULL
         AND inventory_manifest IS NULL AND inventory_total_rows IS NULL
         AND ((status='running' AND finished_at IS NULL AND summary IS NULL)
              OR (status='completed' AND finished_at IS NOT NULL AND jsonb_typeof(summary)='object'
                  AND summary ?& ARRAY['digest','rows'] AND jsonb_typeof(summary->'digest')='string'
                  AND jsonb_typeof(summary->'rows')='number' AND summary->>'digest' ~ '^[0-9a-f]{64}$' AND (summary->>'rows')::bigint >= 0)))
        OR (kind = 'purge_inventory' AND status = 'completed' AND finished_at IS NOT NULL
         AND mode IS NULL AND cardinality(rails) = 0 AND psp_id IS NULL
         AND window_starts_at IS NULL AND window_ends_at IS NULL AND NOT dry_run
         AND coverage IS NULL AND expected_rows IS NULL AND affected IS NULL
         AND reversed_at IS NULL AND reversed_by IS NULL AND summary IS NULL AND error IS NULL
         AND inventory_manifest IS NOT NULL AND jsonb_typeof(inventory_manifest) = 'object'
         AND jsonb_typeof(inventory_manifest->'total_rows') = 'number'
         AND inventory_total_rows IS NOT NULL AND inventory_total_rows >= 0
         AND inventory_total_rows = (inventory_manifest->>'total_rows')::bigint)
    ) IS TRUE);

-- A run seals each before-image with the subscription's revision right after
-- its own write, once; nothing else in an image changes but its restore stamp.
CREATE FUNCTION billing.guard_before_image() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_OP = 'DELETE'
       OR (OLD.after_lifecycle_rev IS NOT NULL AND NEW.after_lifecycle_rev IS DISTINCT FROM OLD.after_lifecycle_rev)
       OR (to_jsonb(NEW) - ARRAY['restored_at','destructive_run_class','after_lifecycle_rev'])
          IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['restored_at','destructive_run_class','after_lifecycle_rev']) THEN
        RAISE EXCEPTION 'immutable billing facts in % cannot be rewritten', TG_TABLE_NAME USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
DROP TRIGGER immutable_destructive_before_images ON billing.destructive_run_before_images;
CREATE TRIGGER immutable_destructive_before_images BEFORE UPDATE OR DELETE ON billing.destructive_run_before_images
    FOR EACH ROW EXECUTE FUNCTION billing.guard_before_image();
