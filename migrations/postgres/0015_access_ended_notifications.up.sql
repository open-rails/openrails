-- #789 access-ended notifications: delivery state on notification_queue +
-- the entitlement close-instant index the converge NOTIFY pass scans.
--
-- emailed_at — when the row's email was actually sent (NULL = undelivered;
-- the notification email sweep retries NULLs). Historical rows are assumed
-- delivered inline by the old fire-and-forget path.
ALTER TABLE openrails.notification_queue ADD COLUMN emailed_at timestamp with time zone;
UPDATE openrails.notification_queue SET emailed_at = created_at;

COMMENT ON COLUMN openrails.notification_queue.emailed_at IS '#789: when the notification email was sent; NULL = undelivered (the notification_email_sweep retries).';

CREATE INDEX idx_notification_queue_undelivered ON openrails.notification_queue (merchant_id, created_at) WHERE emailed_at IS NULL;

-- The NOTIFY plane's findings (notify.access_ended.missing) join the ledger.
ALTER TABLE openrails.reconciliation_findings DROP CONSTRAINT chk_reconciliation_findings_type;
ALTER TABLE openrails.reconciliation_findings
    ADD CONSTRAINT chk_reconciliation_findings_type CHECK ((finding_type ~ '^(pull|derive|life|consistency|notify)\.[a-z0-9_]+(\.[a-z0-9_]+)?$'::text));

-- Window CLOSE instant = LEAST(end_at, revoked_at) treating NULL as infinity;
-- must match ListRecentlyClosedLastEntitlementWindows exactly.
CREATE INDEX idx_entitlements_closed_at ON openrails.entitlements
    (merchant_id, (LEAST(COALESCE(end_at, 'infinity'::timestamp with time zone), COALESCE(revoked_at, 'infinity'::timestamp with time zone))))
    WHERE end_at IS NOT NULL OR revoked_at IS NOT NULL;
