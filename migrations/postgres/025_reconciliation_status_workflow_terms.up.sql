-- #570 Reconciliation finding lifecycle terms.
--
-- Rename the persisted finding states to the product workflow vocabulary:
--   open/admin_pending/held/resolved/dismissed
-- become:
--   reconcile_required/admin_required/reconcile_required/fixed/ignored.
--
-- `ignored` intentionally preserves the old dismissed de-dupe behavior: the
-- stable finding identity remains suppressed across reruns, while occurrence
-- counts continue to prove that reality still matched the ignored condition.

ALTER TABLE openrails.reconciliation_findings
    DROP CONSTRAINT IF EXISTS chk_reconciliation_findings_status;

ALTER TABLE openrails.reconciliation_findings
    DROP CONSTRAINT IF EXISTS chk_reconciliation_findings_resolution;

UPDATE openrails.reconciliation_findings
SET status = CASE status
        WHEN 'open' THEN 'reconcile_required'
        WHEN 'held' THEN 'reconcile_required'
        WHEN 'admin_pending' THEN 'admin_required'
        WHEN 'resolved' THEN 'fixed'
        WHEN 'dismissed' THEN 'ignored'
        WHEN 'indeterminate' THEN 'admin_required'
        ELSE status
    END,
    resolution = CASE resolution
        WHEN 'admin_ack' THEN 'admin_fixed'
        WHEN 'dismissed' THEN 'ignored'
        ELSE resolution
    END,
    requires_admin = CASE
        WHEN status IN ('admin_pending', 'indeterminate') THEN true
        WHEN status = 'dismissed' THEN false
        ELSE requires_admin
    END,
    updated_at = now()
WHERE status IN ('open', 'held', 'admin_pending', 'resolved', 'dismissed', 'indeterminate')
   OR resolution IN ('admin_ack', 'dismissed');

UPDATE openrails.reconciliation_findings
SET resolution = CASE status
        WHEN 'auto_fixed' THEN COALESCE(resolution, 'enforced')
        WHEN 'fixed' THEN COALESCE(resolution, 'auto_vanished')
        WHEN 'ignored' THEN COALESCE(resolution, 'ignored')
        ELSE resolution
    END,
    resolved_at = CASE
        WHEN status IN ('auto_fixed', 'fixed', 'ignored') THEN COALESCE(resolved_at, updated_at, now())
        ELSE NULL
    END,
    updated_at = now()
WHERE status IN ('auto_fixed', 'fixed', 'ignored')
   OR status IN ('reconcile_required', 'admin_required');

UPDATE openrails.reconciliation_findings
SET resolution = NULL,
    resolution_evidence = NULL,
    resolved_at = NULL,
    updated_at = now()
WHERE status IN ('reconcile_required', 'admin_required');

ALTER TABLE openrails.reconciliation_findings
    ADD CONSTRAINT chk_reconciliation_findings_status CHECK (status = ANY (ARRAY[
        'auto_fixed'::text,
        'reconcile_required'::text,
        'admin_required'::text,
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

DROP INDEX IF EXISTS openrails.idx_reconciliation_findings_open;
DROP INDEX IF EXISTS openrails.idx_reconciliation_findings_admin_queue;

CREATE INDEX idx_reconciliation_findings_actionable
    ON openrails.reconciliation_findings (provider, finding_type)
    WHERE status IN ('reconcile_required'::text, 'admin_required'::text);

CREATE INDEX idx_reconciliation_findings_admin_required
    ON openrails.reconciliation_findings (last_seen_at DESC)
    WHERE requires_admin AND status = 'admin_required'::text;

COMMENT ON TABLE openrails.reconciliation_findings IS 'Durable reconciliation findings ledger. Stable identity per (merchant, provider, finding_type, subject_key); finding_type is a qualified four-plane slug. Statuses: reconcile_required, admin_required, auto_fixed, fixed, ignored (#570).';
