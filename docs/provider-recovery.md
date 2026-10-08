# Provider recovery

Provider observation is independent of permission to charge or cancel. In
`provider_write_mode: readonly`, refresh can read the provider and record
identified payments, refunds and missing provider-owned records. It does not
execute provider writes or policy-held local lifecycle changes. Read-only is
never automatically changed to full mode.

Observation and application use separate domains in `psp_refresh_watermarks`:

- `events` records provider-window observation, including older advisory runs.
- `applied_events` records complete account-bound windows whose required
  financial facts have been applied. Old observation cursors are never copied
  into this domain. Fetch failures, incomplete pagination, failed financial
  writes and unresolved receipt identity prevent advancement.

A deliberately withheld cancellation does not prevent financial observation.
It also does not become permission to cancel: full reconciliation proofs and
destructive policy remain separate.

Catch-up reads bounded time windows, repeats an overlap for delayed indexing,
and schedules its next batch immediately when more windows remain. Provider
transaction IDs are neither ordered cursors nor proof that nothing is missing.
NMI query windows describe provider modification time, not an exhaustive set of
transactions originally charged during the window. The overlap is not a claim
of provider finality or an exactly-once fence across independent databases.

When applied progress exists, recovery continues from that cursor. On a first
upgrade without it, the lower bound comes from relevant current subscriptions
and unresolved accepted operations, not every historical completed payment.
Relevant obligations older than the automatic lookback require an explicit
reviewed baseline; they are not silently certified by truncating the read.

Known engine renewals and invoice payments must recover through their accepted
operation and normal settlement path. An unmatched provider receipt is a
review finding, not permission to insert an unrelated subscription payment.

This work is still under qualification: the stale-book mutation gate, native
receipt correlation and strict NMI report completeness must be reviewed together
before the restore workflow is considered complete. Migration 0009 depends on
0008 from the billing-duration change and must be relinked onto that final parent
before merge. No live provider or deployed restore is qualified by local tests.
