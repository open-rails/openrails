-- #573 Simplify reconciliation_findings ledger shape.
--
-- Keep the durable stable-identity ledger, but remove older provider-diff/admin
-- queue columns that duplicated status or split evidence into rarely-used
-- buckets. Provider identity, when relevant, now lives in evidence.provider.

ALTER TABLE openrails.reconciliation_findings
    DROP CONSTRAINT IF EXISTS chk_reconciliation_findings_status;

ALTER TABLE openrails.reconciliation_findings
    DROP CONSTRAINT IF EXISTS chk_reconciliation_findings_resolution;

ALTER TABLE openrails.reconciliation_findings
    DROP CONSTRAINT IF EXISTS chk_reconciliation_findings_resolved_fields;

ALTER TABLE openrails.reconciliation_findings
    ADD COLUMN IF NOT EXISTS evidence jsonb;

UPDATE openrails.reconciliation_findings
SET status = CASE status
        WHEN 'admin_required' THEN 'requires_review'
        WHEN 'admin_pending' THEN 'requires_review'
        ELSE status
    END,
    resolved_at = CASE
        WHEN status IN ('auto_fixed', 'fixed', 'ignored', 'resolved', 'dismissed')
            THEN COALESCE(resolved_at, updated_at, last_seen_at, now())
        ELSE NULL
    END,
    resolution = CASE resolution
        WHEN 'admin_ack' THEN 'admin_fixed'
        WHEN 'dismissed' THEN 'ignored'
        ELSE resolution
    END,
    evidence = jsonb_strip_nulls(jsonb_build_object(
        'provider', NULLIF(provider, 'self'),
        'local', local_evidence,
        'remote', remote_evidence,
        'intent', intent_evidence,
        'resolution', resolution_evidence
    )),
    updated_at = now();

UPDATE openrails.reconciliation_findings
SET resolution = CASE status
        WHEN 'auto_fixed' THEN COALESCE(resolution, 'enforced')
        WHEN 'fixed' THEN COALESCE(resolution, 'auto_vanished')
        WHEN 'ignored' THEN COALESCE(resolution, 'ignored')
        ELSE NULL
    END,
    resolved_at = CASE
        WHEN status IN ('auto_fixed', 'fixed', 'ignored') THEN COALESCE(resolved_at, updated_at, now())
        ELSE NULL
    END,
    updated_at = now();

ALTER TABLE openrails.reconciliation_findings
    RENAME COLUMN notes TO operator_notes;

DROP INDEX IF EXISTS openrails.uq_reconciliation_findings_identity;
DROP INDEX IF EXISTS openrails.idx_reconciliation_findings_actionable;
DROP INDEX IF EXISTS openrails.idx_reconciliation_findings_admin_required;
DROP INDEX IF EXISTS openrails.idx_reconciliation_findings_open;
DROP INDEX IF EXISTS openrails.idx_reconciliation_findings_admin_queue;

ALTER TABLE openrails.reconciliation_findings
    DROP COLUMN IF EXISTS provider,
    DROP COLUMN IF EXISTS requires_admin,
    DROP COLUMN IF EXISTS local_evidence,
    DROP COLUMN IF EXISTS remote_evidence,
    DROP COLUMN IF EXISTS intent_evidence,
    DROP COLUMN IF EXISTS resolution_evidence,
    DROP COLUMN IF EXISTS first_seen_at,
    DROP COLUMN IF EXISTS occurrence_count;

CREATE UNIQUE INDEX uq_reconciliation_findings_identity
    ON openrails.reconciliation_findings (merchant_id, finding_type, subject_key);

CREATE INDEX idx_reconciliation_findings_actionable
    ON openrails.reconciliation_findings (finding_type)
    WHERE status IN ('reconcile_required'::text, 'requires_review'::text);

CREATE INDEX idx_reconciliation_findings_requires_review
    ON openrails.reconciliation_findings (last_seen_at DESC)
    WHERE status = 'requires_review'::text;

ALTER TABLE openrails.reconciliation_findings
    ADD CONSTRAINT chk_reconciliation_findings_status CHECK (status = ANY (ARRAY[
        'auto_fixed'::text,
        'reconcile_required'::text,
        'requires_review'::text,
        'fixed'::text,
        'ignored'::text
    ]));

ALTER TABLE openrails.reconciliation_findings
    ADD CONSTRAINT chk_reconciliation_findings_resolution CHECK (
        resolution IS NULL OR resolution = ANY (ARRAY[
            'auto_vanished'::text,
            'enforced'::text,
            'admin_fixed'::text,
            'ignored'::text
        ])
    );

ALTER TABLE openrails.reconciliation_findings
    ADD CONSTRAINT chk_reconciliation_findings_resolved_fields CHECK (
        (status IN ('auto_fixed', 'fixed', 'ignored') AND resolved_at IS NOT NULL AND resolution IS NOT NULL)
        OR (status IN ('reconcile_required', 'requires_review') AND resolved_at IS NULL AND resolution IS NULL)
    );

COMMENT ON TABLE openrails.reconciliation_findings IS 'Durable reconciliation findings ledger. Stable identity per (merchant, finding_type, subject_key); provider/account context lives in evidence for pull.* findings. Statuses: reconcile_required, requires_review, auto_fixed, fixed, ignored (#573).';
COMMENT ON COLUMN openrails.reconciliation_findings.evidence IS 'Machine-readable finding evidence. Optional nested keys: provider, local, remote, intent, resolution.';
COMMENT ON COLUMN openrails.reconciliation_findings.operator_notes IS 'Operator-entered notes attached when a finding is fixed or ignored manually.';
