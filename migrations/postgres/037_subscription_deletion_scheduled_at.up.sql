-- Deferred NMI delete window (issue 216). When an NMI-backed subscription is
-- cancelled with a genuine future undo window, we keep the processor-side
-- recurring subscription alive and record when the scheduled delete_subscription
-- should fire (period_end - 48h). While this is non-null and in the future, the
-- cancellation is still reversible (see subscriptions.CancelModeFor). The River
-- finalizer clears it back to NULL after it calls DeleteRecurringSubscription.
ALTER TABLE billing.subscriptions
    ADD COLUMN IF NOT EXISTS deletion_scheduled_at TIMESTAMPTZ NULL;
