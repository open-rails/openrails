-- Repair: none-needed. The converge sweep and the rebill watch's engine leg
-- are gone: a lapsed deadline is due work that changes the row and raises no
-- finding, nothing makes a pending membership, and an engine renewal's miss
-- is read, not recorded. Their open findings close.
UPDATE billing.reconciliation_findings
   SET status = 'fixed', resolution = 'auto_vanished', resolved_at = now(),
       notified_at = NULL, notified_severity = NULL, updated_at = now()
 WHERE status IN ('reconcile_required', 'requires_review')
   AND (finding_type IN ('life.subscription.renewal_overdue', 'life.subscription.grace_exhausted',
                         'life.subscription.paid_pending', 'life.subscription.pending_stale',
                         'life.subscription.dunning_overdue', 'life.subscription.dunning_without_decline')
        OR (finding_type = 'life.rebill.missed' AND evidence->>'owner' = 'engine'));
