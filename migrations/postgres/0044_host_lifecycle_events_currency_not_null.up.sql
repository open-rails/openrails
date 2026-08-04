-- CUR-1: host_lifecycle_events.currency must be NOT NULL.
--
-- or#878's 0037 created the column nullable. CUR-1 (pinned by
-- TestCUR1_CurrencyColumnsAreNotNull) allows exactly ONE nullable currency
-- column in the schema — grants.currency, which CUR-2 makes conditionally
-- required — so this one was an unintended second.
--
-- Nothing writes NULL. The only producer is delinquency.Service's transition
-- emit (EnqueueHostLifecycleEvent), which always passes the transition's
-- currency; every event is per-(merchant, payer, currency) by construction, and
-- the currency is already part of its dedupe key, so a NULL-currency row could
-- never have been a well-formed event. The query's sqlc.narg is what left the
-- door open, not any caller — tightened to sqlc.arg alongside this migration.
--
-- A bare SET NOT NULL rather than the NOT VALID / VALIDATE two-step: per
-- EXEMPTIONS.md, splitting only pays off across two transactions (two files),
-- and this table does not need it — it is a delivery feed created two
-- migrations ago and pruned after delivery, so the validating scan is over a
-- small, short-lived table.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

-- squawk's suggested alternative (leave it nullable, add a CHECK) cannot
-- satisfy the invariant: CUR-1 reads information_schema.columns.is_nullable, so
-- only the column attribute counts. The scan is inherent to SET NOT NULL and is
-- not the constraint two-step this rule's sibling covers.
ALTER TABLE openrails.host_lifecycle_events
    -- squawk-ignore adding-not-nullable-field
    ALTER COLUMN currency SET NOT NULL;

COMMENT ON COLUMN openrails.host_lifecycle_events.currency IS
    'The transition''s currency. NOT NULL (CUR-1): every lifecycle event is per-(merchant, payer, currency) and the currency is part of its dedupe key, so an event without one is not a well-formed event.';
