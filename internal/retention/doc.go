// Package retention holds how long OpenRails keeps each kind of row. The
// periods are constants: the same for every merchant and not configuration,
// except the event log's Config.EventRetention. Each table's comment in the
// baseline states its class (tables.go lists them):
//   - permanent: the ledger, grants, payments, owed usage, usage accruals,
//     receipts, operation authorizations, refusals, running charges,
//     maintenance runs and destructive before-images; also an order attempt
//     that reached a provider, and a cost observation while its operation is
//     open;
//   - partitioned: monthly partitions dropped whole (limit_holds and
//     limit_hold_parts after 61 days);
//   - rows: deleted by the hourly cleanup;
//   - state: one row per thing that exists.
//
// Row periods: usage_daily 24 months after its usage was billed (about 26
// after the month began); subscription_status_transitions,
// subscription_billing_changes, provider_mutation_logs, payment_attempts,
// rebill_cycles and nmi_history_months 25 months; instruction-only
// provider_intents 25 months after they last changed (the rest are
// permanent); resolved reconciliation_findings 12 months; order_attempts that
// never reached a provider, and unpaid closed orders with none, 90 days;
// parked_provider_subscriptions 90 days after release; carts 30 days idle;
// notification_events, host_outbox and the event log 90 days, never before
// EventRetention; webhook_events 90 days after completion; idempotency_keys and
// solana_pay_references at expiry (after the 7-day watch);
// user_destructive_reservations 24 hours; failed_usage_windows when the window
// ends; meter_period_quantities 100 days; limit_windows 61 days after
// window_ends_at (lifetime and rate limits stay); leases with their holds and
// limit_adjustments 61 days after the last sync; usage_minutes 25 hours;
// webhook_health_daily 13 months; periodic_leases after 30 days unscheduled.
//
// The cleanup job (openrails.cleanup_expired_data) creates partitions two
// months ahead, drops expired ones (giving up after 2 s rather than queue
// behind a long reader), then deletes 1,000 rows per statement and at most
// 50,000 per merchant per pass. subscription_status_transitions refuses other
// deletes (billing.guard_retention_delete, keyed on the
// openrails.retention_table setting).
//
// What integrators see: usage may be dated up to UsageIngestWindow back; a
// lease answers resends for 61 days after its last sync; a request hold lasts
// at most 30 days and a spend window AdmissionMaxWindow; after 61 days a
// request id reads as never admitted; usage reports reach back about 26
// months.
package retention
