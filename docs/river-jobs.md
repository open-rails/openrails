# OpenRails River jobs

OpenRails registers 27 core River job kinds. Seven have recurring schedules.
A kind identifies the worker that handles a job, and
a queue controls which workers can take it. A shared River fleet can also
contain AuthKit or host application jobs. [Worker registrations](../internal/app/river_register.go) ·
[Kind and timeout map](../internal/river/timeouts.go) · [Shared fleets](embedded.md)

The [operations inventory](operations-inventory.md) lists the provider calls and local changes behind these jobs.

## Recurring jobs

River attempts startup catch-up for all seven declarations. Shared database
leases coordinate them. [Periodic declarations](../internal/river/periodic.go) ·
[Startup and leases](../internal/app/river_periodic.go)

| Kind | Queue | When it runs | What it does |
| --- | --- | --- | --- |
| `openrails.billing_pass` | `billing` | At the top of each UTC hour | Runs the [hourly billing scans](#hourly-billing-scans) and records any critical work still overdue. [Worker](../internal/river/jobs_billing_pass.go) |
| `openrails.cleanup_expired_data` | `billing` | Every hour, with continuation jobs for large passes | Deletes expired operational data. [Worker](../internal/river/jobs_cleanup.go) |
| `openrails.customer_messages_dispatch` | `billing` | Every 5 minutes | Queues the send job of each pending customer message a dispatcher's database notification missed. [Worker](../internal/river/jobs_customer_messages.go) · [Dispatch](../internal/modules/messages/dispatch.go) |
| `openrails.merchant_health` | `billing` | Every hour | Evaluates payment and recovery health, held renewals, the unverified backlog, stuck or abandoned provider operations, and timed access lapses, for merchants with activity. [Worker](../internal/river/jobs_merchant_health.go) |
| `openrails.mrr_risk` | `billing` | Every 30 seconds | Drains the MRR risk changes in `mrr_risk_due` that no drain woken by their database notification took. [Worker](../internal/river/jobs_mrr_risk.go) · [Drain](../internal/modules/subscriptions/mrr_risk.go) |
| `openrails.nmi_history` | `nmi_history` | At the top of each UTC hour | Reads the NMI history due in `nmi_history_reads` (indexed on `due_at`) into monthly reporting aggregates: 25 months the first time, then from the month before the last read. A completed read is due again a day later; a failed one stays due for the next hour. Archived or superseded PSPs wait until their configuration declares them again. Its own queue keeps a slow provider report off billing workers; a job that starts while another reads, on any fleet, exits. [Worker](../internal/river/jobs_nmi_history.go) |
| `openrails.stripe_webhook_reconcile` | `billing` | Every 24 hours. Per-PSP jobs also run on configuration changes, at boot when configuration differs, and at endpoint retirement. | Queues checks for active Stripe PSPs and registers, updates, or retires their managed webhook endpoints. [Worker](../internal/river/jobs_stripe_webhooks.go) · [Configuration trigger](../internal/app/stripe_webhook_follow.go) |

## Jobs queued as work arrives

| Kind | Queue | What queues it | What it does |
| --- | --- | --- | --- |
| `openrails.balance_look` | `billing` | A balance change crosses a watched threshold, or the previous look schedules the next check | Checks one customer balance for expiring credits, running-charge checkpoints, and low or depleted balance notices. [Worker](../internal/river/jobs_balance_watch.go) · [Scheduling](../internal/modules/money/balance_watch.go) |
| `openrails.charge_attributions` | `billing` | A billing import declares charges that need provider evidence | Reads or waits for provider charge facts, then records each charge against its declared subscription. [Worker](../internal/river/jobs_charge_attributions.go) · [Import trigger](../internal/billingimport/billingimport.go) |
| `openrails.contact_erasures` | `billing` | A SCIM erasure that waits on the merchant's erasure budget or hold commits a job in the same transaction; clearing the erasure hold brings it forward | Runs the merchant's waiting contact erasures, oldest first, within its erasure budget while the hold is clear, and looks again every 10 minutes while any wait. [Worker](../internal/river/jobs_contact_erasures.go) · [Drain](../internal/scim/erasure.go) |
| `openrails.customer_converge` | `billing` | A customer-affecting mutation commits a job in the same transaction | Reconciles grant effects, references, duplicate charges, findings, and access-ended notices for one customer. [Worker](../internal/river/jobs_customer_converge.go) · [Enqueue](../internal/convergejob/convergejob.go) |
| `openrails.customer_message` | `billing` | The dispatcher, woken by the commit of a billing fact, or its periodic pass | Sends one customer message, or records why not; a failed send is retried with backoff, ten attempts in all. [Worker](../internal/river/jobs_customer_messages.go) · [Send](../internal/modules/messages/send.go) |
| `openrails.dispute_monitor` | `billing` | A chargeback, a fraud report, or a sale on a PSP with an open threshold finding commits a job in the same transaction, one per PSP while queued | Evaluates the PSP's dispute monitoring programs, this month and any ended month with an open finding, and raises, escalates or resolves `life.disputes.threshold`, alerting; alerts on `life.disputes.customer_review`. [Worker](../internal/river/jobs_dispute_monitor.go) · [Enqueue](../internal/disputejob/disputejob.go) |
| `openrails.dispute_response` | `billing` | A chargeback or a pending fraud report commits a job in the same transaction, one per case; a change of what a waiting response waits for queues it again (`openrails_dispute_responses`, `disputes.Service.Run`) | Refunds a fraud-reported charge and ends what it paid for, blocks the charged card, and cancels the customer's other provider-billed subscriptions on it at period end, each step an autonomous destructive command; a held step raises `life.disputes.response_waiting` and ends. [Worker](../internal/river/jobs_dispute_monitor.go) · [Enqueue](../internal/disputejob/disputejob.go) |
| `openrails.merchant_secret_cleanup` | `billing` | A merchant purge commits with Vault cleanup pending | Deletes the purged merchant's Vault configuration and records the result on the purge run. [Worker](../internal/river/jobs_merchant_secret_cleanup.go) · [Cleanup](../internal/merchants/secret_cleanup.go) |
| `openrails.merchant_usage_purge` | `billing` | A merchant purge commits with an event store attached | Deletes the purged merchant's usage from the event store, retried until the store takes it; what posted stays. [Worker](../internal/river/jobs_merchant_usage_purge.go) · [Purge](../internal/merchants/purge.go) |
| `openrails.operator_alert` | `billing` | A destructive-action hold or a finding notification queues the initial job, which queues one delivery job per applicable channel | Delivers email or webhook alerts to operators, with independent retries per channel. [Worker](../internal/river/jobs_operator_alert.go) · [Delivery](../internal/modules/alerting/delivery.go) |
| `openrails.paced_provider_refresh` | `provider_refresh` | A merchant's refresh, for each live CCBill PSP | Reads that PSP one DataLink request an hour, a day of transactions each, then its roster; it snoozes between requests. [Provider refresh](standalone.md#provider-refresh) · [Worker](../internal/river/jobs_provider_refresh.go) |
| `openrails.price_migration` | `billing` | A price migration whose subscribers outnumber its first page commits the next page's job with each page | Reaches the next page of the migration's subscribers in one transaction: schedules or blocks each move, queues its provider push, and, after the last page or a cancel, records what it skipped and ends the migration's preparation. [Worker](../internal/river/jobs_price_migration.go) · [Pages](../internal/modules/subscriptions/price_migration.go) |
| `openrails.provider_object_observe` | `billing` | Stripe product or price webhooks | Reads the current catalog object from Stripe and records its observed state or retirement. [Worker](../internal/river/jobs_provider_object_observe.go) · [Webhook trigger](../internal/modules/webhooks/stripe_catalog.go) |
| `openrails.provider_operation` | `billing` | An accepted provider intent commits and schedules a wake for its next attempt, or the hourly scan restores an overdue wake | Executes or verifies one authorized provider operation until it reaches a terminal state. [Worker](../internal/river/jobs_provider_intents.go) · [Wake](../internal/intents/wake/wake.go) |
| `openrails.provider_refresh_requested` | `provider_refresh` | An operator runs `openrails provider-refresh --merchant <merchant>` | Pulls provider events and history for one merchant and resumes from its watermarks. [Operator command](standalone.md#provider-refresh) · [Upgrade](standalone.md#upgrades-and-migrations) · [Worker](../internal/river/jobs_provider_refresh.go) |
| `openrails.psp_import` | `psp_import` | An operator starts an import, changes matching links, or applies an approved import | Pulls PSP data into staging, re-evaluates proposed matches, or applies approved observations locally. [Worker](../internal/river/jobs_psp_import.go) · [Import steps](../internal/pspimport/importer.go) · [Relink](../internal/pspimport/link.go) |
| `openrails.stored_card_refresh` | `billing` | The hourly scan finds a card due for an updater check, or a charge declines because the card expired or was reissued | Asks the vault's account updater for one card's latest details and applies replacement or closure effects. [Worker](../internal/river/jobs_stored_card_refresh.go) · [Decline trigger](../internal/modules/subscriptions/lifecycle_service.go) |
| `openrails.subscription_converge` | `billing` | Stripe or NMI subscription-state webhooks, or release of a parked subscription | Coalesces wakes, fetches current provider state, and updates the local subscription and its effects. [Worker](../internal/river/jobs_subscription_converge.go) · [Coalescing](../internal/wakejobs/wakejobs.go) · [Park release](../internal/modules/parking/parking.go) |
| `openrails.usage_erasure` | `billing` | A customer's directory user is deleted (SCIM), in that transaction | Under the customer's lock, clears the invoker and metadata of the customer's usage events in the event store (usage history names no invoker); quantities and what posted stay, and a retry after a partial run converges. [Worker](../internal/river/jobs_usage_erasure.go) · [Erasure](../internal/eventstore/erase.go) |
| `openrails.usage_invoices` | `billing` | Usage crosses a billing threshold or an hourly usage scan needs another page | Bills due usage accounts into invoices and queues their due collections as provider operations. [Worker](../internal/river/jobs_usage_invoices.go) · [Threshold trigger](../internal/modules/money/account_usage_billing.go) |

## Hourly billing scans

Each `openrails.billing_pass` job runs these scans in order within its own River row. Usage billing also has a registered kind for threshold wakes and page continuations. The other scan names label work inside the pass. A pass that starts while another runs, on any fleet, exits: the running pass holds a lease in `periodic_leases`, renewed while it runs and lapsing within two minutes of its holder dying.
[Scan registration](../internal/app/river_register.go) ·
[Pass execution](../internal/river/jobs_billing_pass.go)

| Scan | What it looks for |
| --- | --- |
| `openrails.usage_posting` | The daily posting: once the accounting-zone day of a customer's oldest pending usage ends, posts all of it to the ledger, one usage receipt per pricing period, priced from the period's settled total in PostgreSQL (`usage_totals`) less what its earlier receipts took, never from the event store; a period whose volume price fell is credited in the same posting. The read is indexed on when each customer is due (`usage_pending.due_at`). [Source](../internal/river/jobs_usage_posting.go) |
| `openrails.dunning` | Due subscription renewals and collection retries; then provider-billed renewals unpaid past their rail's deadline (24 hours for an NMI schedule, 48 for Stripe and CCBill), each moved to `unverified` with access kept, which the `Verifier` reads at once. [Source](../internal/river/jobs_dunning.go) |
| `openrails.subscription_verification_retry` | Flagged `unverified` subscriptions not read for an hour: a missed notification, a failed read, or a decision still held or inconclusive. [Source](../internal/reconcile/verify.go) |
| `openrails.balance_watch` | Due balance checks and failed or missed balance looks. [Source](../internal/river/jobs_balance_watch.go) |
| `openrails.delinquency` | Usage arrears due for evaluation after a payment or grace deadline. [Source](../internal/river/jobs_delinquency.go) |
| `openrails.invoice_expiry` | Expired invoices and attempts, including abandoned card saves. [Source](../internal/river/jobs_invoice_expiry.go) |
| `openrails.revenue_recognition` | Revenue schedules due: a month boundary reached in the merchant's accounting timezone, a schedule's end or an early end. The read is indexed on the due instant. [Source](../internal/river/jobs_revenue_recognition.go) |
| `openrails.usage_invoices` | Due usage bills and collections. [Source](../internal/river/jobs_usage_invoices.go) |
| `openrails.config_realerts` | Unresolved undeclared-account findings due for another operator alert. [Source](../internal/river/jobs_config_realerts.go) |
| `openrails.stored_card_refresh_due` | Stored cards due for account updater checks before renewal. [Source](../internal/river/jobs_stored_card_refresh.go) |
| `openrails.operation_wakes` | Live provider operations more than an hour overdue that need another wake. [Source](../internal/river/jobs_operation_wakes.go) |
| `openrails.mrr_due` | Subscriptions canceled at period end whose end has come: each leaves MRR, dated at its end. The read is indexed on the change instant. [Source](../internal/river/jobs_mrr_due.go) |
| `openrails.stripe_fees` | Stripe charges, refunds and disputes whose processor fees the general ledger has yet to read (`stripe_fee_reads`, indexed on `due_at`): each object's balance transactions are read and each fee posts once. A failed read waits and is read again; after eight failures it waits for its `books.fee.unread` finding to be fixed. [Source](../internal/modules/money/fees/stripe.go) |

## Inspect a running deployment

Use the deployment's configuration to see per-kind counts of overrunning,
retryable, and discarded jobs, the newest error, and the last completion for
periodic kinds. The JSON form includes full error text.
[CLI](../server/cmd/openrails/operator_cli.go) · [Health query](../internal/river/health.go)

```sh
openrails --config /path/to/config.yaml workers
openrails --config /path/to/config.yaml workers --json
```

For individual rows, query `river_job` in River's schema: the billing schema plus `_river` (`Config.Database.RiverSchema` in Go sets another), so the default table is `billing_river.river_job`.
Replace `billing_river` with your configured schema if it differs. The query
shows recent retained rows across all components in the selected schema.
[Schema configuration](../billing/config.go) · [River table usage](../internal/river/health.go)

```sql
SELECT id, kind, queue, state, scheduled_at, attempt
FROM billing_river.river_job
ORDER BY id DESC
LIMIT 100;
```

For a provider refresh, use the `river_schema` and `job_id` from the command's
response. This example checks job 12345 and shows its recorded errors.

```sql
SELECT id, state, attempt, max_attempts, errors
FROM billing_river.river_job
WHERE id = 12345;
```
