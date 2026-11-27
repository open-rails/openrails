// Package retention holds how long OpenRails keeps each kind of row. The
// periods are constants: the same for every merchant and not configuration.
// Each table's comment in the
// baseline states its class (tables.go lists them):
//   - permanent: the ledger, grants, payments, owed usage, usage receipts
//     and card prints, receipts, operation authorizations, refusals, running charges,
//     maintenance runs and destructive before-images; also an invoice attempt
//     that reached a provider, and a cost observation while its operation is
//     open;
//   - rows: deleted by the hourly cleanup;
//   - state: one row per thing that exists.
//
// Row periods: subscription_status_transitions,
// subscription_billing_changes, operation_log, payment_attempts,
// rebill_cycles and nmi_history_months 25 months; instruction-only
// provider_intents 25 months after they last changed (the rest are
// permanent); resolved reconciliation_findings 12 months; invoice_attempts that
// never reached a provider 90 days, and unpaid closed invoices with none 397
// days;
// parked_provider_subscriptions 90 days after release; carts 30 days idle;
// notification_events, host_outbox and the event log 90 days; webhook_events
// 90 days after completion; idempotency_keys and
// solana_pay_references at expiry (after the 7-day watch);
// user_destructive_reservations 24 hours; limit_windows a day after
// window_ends_at, 61 days once reached_at is set (lifetime and rate limits
// stay; usage settled after a window ended counts toward none); leases with
// their in-flight reports, and limit_adjustments, 61 days after the last
// sync; usage_totals a day after their period's late usage window closes;
// webhook_health_daily 13 months; provider_listed_rebills 90 days;
// periodic_leases after 30 days unscheduled.
//
// The event store's ClickHouse tables expire by their TTLs (EventStoreTTLs):
// usage_events 90 days after occurred_at (the owner's choice), limit_usage a
// day after its window ends (a window without an end stays). usage_history,
// by UTC quarter hour, has no TTL and names no invoker (a customer's erasure
// clears the raw events'); a merchant purge deletes its rows. What posted is
// in PostgreSQL, permanently, and no posting reads the store's rollups.
//
// The cleanup job (openrails.cleanup_expired_data) deletes 1,000 rows per
// statement and at most 50,000 per merchant per pass. subscription_status_transitions refuses other
// deletes (billing.guard_retention_delete, keyed on the
// openrails.retention_table setting).
//
// What integrators see: usage may be dated up to UsageIngestWindow back; a
// lease answers resends for 61 days after its last sync; a spend window lasts
// at most MaxSpendWindow; usage reports and series read the whole history by
// event type and meter, and by invoker the raw events' 90 days.
package retention
