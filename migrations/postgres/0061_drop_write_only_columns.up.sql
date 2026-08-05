-- or#823: drop the columns that are written and never read.
--
-- Each one below was re-verified against current dev, not inherited from the
-- audit: a column qualifies only when no sqlc query, no generated reader, and no
-- raw SQL in the app consults it. A test-only reader does not save a column —
-- it only means a test asserts on a value nothing acts on, so the test moves to
-- whatever the app actually reads.
--
-- Three classes:
--   * counters and diagnostics superseded by a live substrate that IS read
--     (webhook_health's lifetime totals vs webhook_health_daily's buckets);
--   * provenance and version stamps written at insert and never consulted;
--   * columns with no writer at all, so every reader saw the default forever.
--
-- Prelaunch: dropped, not deprecated. There is no deployment holding rows and
-- no stable API contract over these names, and a column kept "just in case"
-- reads to the next person as a signal that something consumes it.
--
-- squawk fires ban-drop-column on every statement here. That is the operation
-- this migration exists to perform; the exemption is recorded in
-- internal/db/queries/EXEMPTIONS.md.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

-- --------------------------------------------------------------------------
-- The *_version optimistic-concurrency stamps
-- --------------------------------------------------------------------------

-- Four surviving columns of a five-column pattern (payer_spend_limits went with
-- its table in 0048). Every one is incremented on write and read by nothing;
-- spendgate/policy.go documented a policy cache "invalidated on policy_version
-- bump" that was never built, and that claim is corrected alongside this drop.
--
-- These are NOT groundwork for future #687. That issue specifies a `revision`
-- column with a CAS write path (`SET revision = revision + 1 WHERE id = $1 AND
-- revision = $expected`), a conflict error at every call site, and the counter
-- on API responses. None of that exists here: these bump unconditionally, are
-- never compared, and never leave the database. #687 would add its own column
-- to its own scope list (which includes checkout_sessions, provider_intents and
-- invoices, none of which carry a version column today) — it would not adopt
-- four unread bigints under three different names.

ALTER TABLE openrails.invoker_spend_limits
    -- squawk-ignore ban-drop-column
    DROP COLUMN policy_version;

ALTER TABLE openrails.merchant_configurations
    -- squawk-ignore ban-drop-column
    DROP COLUMN config_version;

ALTER TABLE openrails.tier_schedules
    -- squawk-ignore ban-drop-column
    DROP COLUMN schedule_version;

-- --------------------------------------------------------------------------
-- product_usage_limit_bindings: version + provenance
-- --------------------------------------------------------------------------

-- The table's ONLY reader (admission/policy.go) selects `usage_limit_key,
-- windows`; everything else is EXISTS-by-grant. So the grant-time provenance
-- stamps answer no question anyone asks. grant_id stays — it IS the key the
-- revoke path and the existence check use.

ALTER TABLE openrails.product_usage_limit_bindings
    -- squawk-ignore ban-drop-column
    DROP COLUMN policy_version,
    -- squawk-ignore ban-drop-column
    DROP COLUMN source_type,
    -- squawk-ignore ban-drop-column
    DROP COLUMN source_id,
    -- squawk-ignore ban-drop-column
    DROP COLUMN product_key;

-- --------------------------------------------------------------------------
-- webhook_health: lifetime counters superseded by webhook_health_daily
-- --------------------------------------------------------------------------

-- #786 shipped both a lifetime tally and a daily bucket table. The metrics
-- registry reads the buckets (`whd.rejected`, `whd.drift`) and, from the
-- snapshot table, only `last_accepted_at`/`created_at` for the silence age. A
-- monotonic lifetime total cannot answer a windowed question anyway, which is
-- why the daily table was added.
--
-- last_accepted_at and last_pull_at STAY: the first is the silence watermark
-- the alert template ages, the second is the drift gate's comparison point.

ALTER TABLE openrails.webhook_health
    -- squawk-ignore ban-drop-column
    DROP COLUMN accepted_count,
    -- squawk-ignore ban-drop-column
    DROP COLUMN rejected_count,
    -- squawk-ignore ban-drop-column
    DROP COLUMN drift_count,
    -- squawk-ignore ban-drop-column
    DROP COLUMN last_rejected_at,
    -- squawk-ignore ban-drop-column
    DROP COLUMN last_drift_at;

-- The daily bucket nobody measures. `rejected` and `drift` back the
-- webhook_rejects / webhook_drift_events measures; there is no accepted-volume
-- measure, and the silence watermark already answers "are webhooks arriving".
ALTER TABLE openrails.webhook_health_daily
    -- squawk-ignore ban-drop-column
    DROP COLUMN accepted;

-- --------------------------------------------------------------------------
-- rail_refresh_watermarks: the operator diagnostics with no operator surface
-- --------------------------------------------------------------------------

-- The table comment advertised last_error as a failure signal. Nothing surfaces
-- it: the only app reader (ProviderRefreshWorker.loadWatermark) selects
-- watermark_at alone, and a failed pass already logs and raises through River's
-- own retry/error record — which IS an operator surface. Keeping a second,
-- invisible copy of the same fact just made the schema claim a capability the
-- deployment does not have.

ALTER TABLE openrails.rail_refresh_watermarks
    -- squawk-ignore ban-drop-column
    DROP COLUMN last_attempted_at,
    -- squawk-ignore ban-drop-column
    DROP COLUMN last_succeeded_at,
    -- squawk-ignore ban-drop-column
    DROP COLUMN last_error;

COMMENT ON TABLE openrails.rail_refresh_watermarks IS
    'Durable Provider Refresh watermarks: the exclusive lower bound for the next bounded event window, per (merchant, rail, PSP, domain). A failed or partial provider read simply never advances watermark_at — the failure itself is recorded by the job, not here.';

-- --------------------------------------------------------------------------
-- Columns with no writer at all
-- --------------------------------------------------------------------------

-- usage_events.invoker_type: both INSERT statements omit it, so every row has
-- carried NULL since the table existed. The invoker's TYPE is a property of the
-- admit request (identity.InvokerType), decided before the event is recorded and
-- not part of what a usage event means; invoker_id is the column that identifies
-- who consumed the units.
ALTER TABLE openrails.usage_events
    -- squawk-ignore ban-drop-column
    DROP COLUMN invoker_type;

-- merchant_deks.key_version: dbDEKStore writes (merchant_id, wrapped_dek) and
-- reads wrapped_dek. Nothing has ever set or consulted a version, so it has been
-- 1 on every row. DEK rotation, when it is built, needs a rotation state machine
-- and a re-wrap path, not a counter that predates the design.
ALTER TABLE openrails.merchant_deks
    -- squawk-ignore ban-drop-column
    DROP COLUMN key_version;

-- money_settings.last_alert_at: the low-balance alerter was deleted with the
-- rest of #240, and its stamp went with it — stampMoneyInTimestamp, the only
-- caller of the write, now has no callers of its own. The column is still
-- projected onto the money-account API response, where it has always been null.
-- last_topup_at STAYS: it is the durable auto-top-up episode anchor.
ALTER TABLE openrails.money_settings
    -- squawk-ignore ban-drop-column
    DROP COLUMN last_alert_at;

-- alert_rules.last_detail: written on the fired edge beside last_value, which IS
-- read. Nothing reads the detail — not the API model, not the notification
-- payload, not the digest. The detail that matters travels in the notification
-- the crossing emits.
ALTER TABLE openrails.alert_rules
    -- squawk-ignore ban-drop-column
    DROP COLUMN last_detail;

-- reconciliation_state.last_full_pull_at: the gate this table exists for
-- (IsSourceDomainReconciled) reads fully_reconciled, a ratchet. The timestamp
-- beside it was never consulted — freshness of a pull lives in
-- rail_refresh_watermarks, which is the cursor the next pull actually resumes
-- from.
ALTER TABLE openrails.reconciliation_state
    -- squawk-ignore ban-drop-column
    DROP COLUMN last_full_pull_at;
