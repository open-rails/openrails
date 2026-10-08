# Provider recovery

Provider observation is independent of permission to charge or cancel. In
`provider_write_mode: readonly`, startup and periodic refresh read the provider
and record qualified payments, refunds and missing provider-owned records.
They do not execute provider writes or policy-held local cancellations.
Explicit readonly mode is never automatically changed to full mode.

An established NMI or Stripe account whose completed financial coverage is older than the
four-hour refresh interval (plus the five-minute provider window delay) holds
renewal/invoice dispatch and destructive work. A new account without older
billing facts can serve its initial checkout. A fresh row never hides an older
book. Known unresolved financial findings hold writes even when the last
completion marker is recent; ignoring an alert does not settle its receipt.
Verified receipt recovery continues while the gate is closed. Configured full
mode resumes automatically after catch-up completes without those conflicts.
Starting another replica does not reset healthy shared-database coverage.
CCBill and Solana retain their existing webhook and destructive-policy behavior;
this delivery does not give them an automatic stale-backup recovery guarantee.
Completion durably wakes recovery-held operations and requests the merchant's
normal renewal and invoice scans, including invoices refused before admission.
Issuer retry dates and monthly collection cadence remain unchanged. A verifier
that commits its hold just after completion rechecks readiness after commit, so
its live lease cannot strand the operation on the former recovery delay.

Observation and application use separate domains in `psp_refresh_watermarks`:

- `events` records provider-window observation, including old advisory runs.
- `applied_events` records successfully applied account-bound windows, so a
  partial catch-up can resume without repeating its entire history.
- `completed_events` records the captured target only after the whole financial
  catch-up completes. Old observation cursors are never promoted into this
  domain. Fetch failures, incomplete pagination, failed financial writes and
  unresolved receipt identity prevent completion.

A deliberately withheld cancellation does not prevent financial observation.
It also does not become permission to cancel: full reconciliation proofs and
destructive policy remain separate. A confirmed Stripe reversal records its
original charge first; the accepted operation retains its pending cancellation
until policy permits that separate lifecycle step.

Catch-up reads bounded time windows, repeats an overlap for delayed indexing,
and schedules the next batch immediately when more windows remain. Provider
transaction IDs are neither ordered cursors nor proof that nothing is missing.
NMI query windows describe provider modification time, not an exhaustive set of
transactions originally charged during the window. A financial finding closes
only when its actual transaction is observed and now matches local facts.

When applied progress exists, recovery continues from that cursor. On first
upgrade without it, the lower bound comes from relevant current subscriptions,
unpaid invoice periods and unresolved accepted operations, not every historical
completed payment. Long billing periods are read in bounded batches from their
relevant obligation floor. Unavailable provider history remains an error; the
read is not truncated and an operator flag cannot manufacture its proof.

Native renewals recover through their accepted operation and normal settlement
path. A restored copy may admit local recovery work and verify a positive
stable-obligation receipt without pretending it previously submitted a charge.
A known invoice whose accepted operation was lost can recover a verified NMI
receipt carrying its exact invoice description, account, payer, currency and
full remaining amount. Partial, reversed, ambiguous or contradictory receipts
remain held. Existing accepted operations retain their canonical authority;
recovery never creates a fake dispatched operation or a manual payment.

The gate detects known stale timestamps; it is not a universal detector of an
arbitrary database clone. Provider visibility can lag, and an account created
after a backup is absent from that backup. Window overlap and freshness alone
are not an exactly-once fence across independent databases. Immediate native
obligation lookups complement bulk catch-up; an uncertain submitted NMI charge
still requires positive evidence rather than an automatic resend. Entire missed
subscription billing periods remain outside automatic catch-up.

Migration 0010 extends existing watermark domains without changing the merchant
archive format. Local PostgreSQL and provider-simulator tests exercise restore
and recovery; they do not qualify live provider visibility or a deployed restore.
