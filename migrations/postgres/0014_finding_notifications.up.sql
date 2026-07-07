-- #787: pump requires_review reconciliation findings into the #736 operator
-- notification store. Findings are event-sourced (one Finding row persisted
-- per pull/converge pass), NOT a metric-threshold rule, so they get their own
-- dedupe linkage on the finding row itself rather than an alert_rules entry.
--
-- notified_at/notified_severity: set when an OPEN finding pushes an operator
-- notification (immediate fire, or inclusion in the low-severity digest).
-- notified_at IS NULL means "not yet notified this open episode" — cleared by
-- every resolution path (fixed/auto_fixed/ignored) so a finding that reopens
-- later notifies again. notified_severity records the severity that was
-- notified, so a later severity INCREASE while still open re-fires (genuine
-- escalation) but re-observing the same open finding at the same or a lower
-- severity does not.
ALTER TABLE openrails.reconciliation_findings
    ADD COLUMN notified_at timestamp with time zone,
    ADD COLUMN notified_severity text;

COMMENT ON COLUMN openrails.reconciliation_findings.notified_at IS '#787: last time this OPEN finding pushed an operator notification; NULL = not yet notified this open episode. Cleared to NULL on every resolution so a reopened finding notifies again.';
COMMENT ON COLUMN openrails.reconciliation_findings.notified_severity IS '#787: severity at last notification; a further increase while still open re-fires, re-observation at the same/lower severity does not.';

-- Armed-merchant scan for the low-severity findings digest (#787): distinct
-- merchants with at least one undigested low-severity requires_review finding.
-- Partial index keeps the scan scaled to backlog, never the findings table
-- on file (#719 only-run-what's-armed).
CREATE INDEX idx_reconciliation_findings_low_severity_pending_digest
    ON openrails.reconciliation_findings (merchant_id)
    WHERE status = 'requires_review' AND severity = 'low' AND notified_at IS NULL;

-- Per-merchant cadence watermark for the low-severity digest — the #736
-- digest-cadence PATTERN (stored watermark + duration gate) reused outside the
-- alert_rules/template registry, since findings are not a metric-threshold rule.
CREATE TABLE openrails.finding_digest_state (
    merchant_id uuid NOT NULL,
    last_digested_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON TABLE openrails.finding_digest_state IS '#787: one row per merchant recording when the low-severity reconciliation-findings digest last fired.';

ALTER TABLE ONLY openrails.finding_digest_state
    ADD CONSTRAINT finding_digest_state_pkey PRIMARY KEY (merchant_id);
ALTER TABLE ONLY openrails.finding_digest_state
    ADD CONSTRAINT finding_digest_state_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;

ALTER TABLE openrails.finding_digest_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.finding_digest_state FORCE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.finding_digest_state USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));
GRANT SELECT,INSERT,UPDATE,DELETE ON TABLE openrails.finding_digest_state TO openrails_app;
