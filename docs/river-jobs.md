# OpenRails background jobs and events audit

Source snapshot: `origin/master` at `88a70db5ac000a8d27802bbe806ab7ca0eec9cda`
(v0.279.0), inspected 2026-11-15. This is a source inventory, not a report of
jobs currently present in any deployed database.

The inventory contains **25 registered core River job kinds**, **6 core periodic
declarations**, **9 scans inside the hourly billing pass** (one is not a River
job), **22 provider-operation types**, **16 host event types**, and **21 customer
notification types**. It also covers incoming provider events, the standalone
server's AuthKit jobs, and background activity outside River.

These are capabilities present in the code. Whether they do work depends on the
host, enabled features, merchant configuration, credentials, and due database
rows. No production database, queued-job counts, or deployment configuration was
inspected. `git pull --ff-only` confirmed this source snapshot was current.

## How to read the inventory

- A **queue** is a concurrency lane, such as `billing`. A **job kind** identifies
  executable work, such as `openrails.provider_operation`.
- A **periodic declaration** inserts a job repeatedly. A **scan** can instead run
  as a function inside another job; it need not create its own River row.
- A **scheduled wake-up** is a job waiting until `scheduled_at`, or a snoozed job
  waiting for its next useful attempt. It is not necessarily a recurring timer.
- A **provider operation** is an authorized action in `billing.provider_intents`.
  Many different actions share the single `openrails.provider_operation` worker.
- A **business event** records something that happened. The retained event log
  does not automatically execute a River job or deliver a customer email.

Core registration is in [river_register.go](../internal/app/river_register.go);
the authoritative kind/timeout list is [timeouts.go](../internal/river/timeouts.go).

## Queues and ownership

| Queue | Default concurrent workers | What uses it |
| --- | ---: | --- |
| `billing` | 20 | All core work below, except PSP imports and standalone per-merchant provider refreshes. |
| `psp_import` | 2 | PSP import pulls, relinking, and application of approved imports. Contributed to host fleets too. |
| `provider_refresh` | 4 | Per-merchant provider refresh jobs when OpenRails owns River. In a host fleet those jobs use `billing`. |
| `default` | 10 | Configured when OpenRails owns River; none of the 25 core kinds defaults to this queue. |

These are defaults applied when a host has not already configured the queue;
they are per River client, not deployment-wide concurrency guarantees. Optional
components can contribute their own queues. See
[composition](../internal/app/river_composition.go) and
[defaults](../internal/app/build_runtime.go).

`Engine.Start()` starts OpenRails' fleet. A host can compose `Engine.RiverJobs()`
into its own fleet and pass that client to `Start(WithRiverClient(...))`; the host
then owns starting/stopping River. The standalone server contributes its AuthKit
jobs too. HTTP-only `Server.Serve()` binds producers but leaves workers to another
process. Jobs and domain changes that require atomicity share PostgreSQL
transactions. See [runtime](../internal/app/runtime.go),
[server](../server/server.go), and [database enqueueing](../internal/db/river.go).

## 1. Recurring core River jobs: all six declarations

Every core periodic declaration has `RunOnStart: true`. This means startup
catch-up, subject to lease, uniqueness and retained-history checks; it does not
mean an unconditional duplicate run on every restart. Except the Redis flush,
passes claim a shared database lease for 80% of their interval so fleets sharing
the billing database do not all insert the same pass. A lease read failure allows
the pass to run. See [periodics](../internal/river/periodic.go) and
[scheduling](../internal/app/river_periodic.go).

| Job kind | Schedule / gate | What it does and changes | Worker source |
| --- | --- | --- | --- |
| `openrails.billing_pass` | Top of every UTC hour (`0 * * * *`), plus startup catch-up. | Runs all nine due scans in section 2 in order. Each scan has its own timeout; an error or panic in one does not stop later scans. Records critical work left a day overdue in River output. Can cause charges, renewals, access changes and notifications through those scans. | [billing pass](../internal/river/jobs_billing_pass.go) |
| `openrails.merchant_health` | Every hour. | Evaluates recovery/payment health, held renewals, unverified backlog, stuck/abandoned provider operations, and timed access lapses. Selects merchants with recent activity, open findings or deadlines; updates findings/access effects and can alert operators. Skips readonly merchants. | [merchant health](../internal/river/jobs_merchant_health.go) |
| `openrails.cleanup_expired_data` | Every hour; additional jobs continue a large pass. | Permanently deletes expired operational data and drops old monthly partitions. Creates partitions ahead of time. Full deletion scope is listed below. | [cleanup](../internal/river/jobs_cleanup.go) |
| `openrails.stripe_webhook_reconcile` | Every 24 hours, plus PSP/configuration changes and endpoint-retirement deadlines. | The empty-argument pass queues one pass per armed Stripe PSP. A targeted pass creates/patches/rolls over its managed Stripe webhook endpoint, retires superseded endpoints after overlap, records configuration digest and opens/closes endpoint findings. **Writes to Stripe.** | [Stripe endpoints](../internal/river/jobs_stripe_webhooks.go), [boot/config trigger](../internal/app/stripe_webhook_follow.go) |
| `openrails.admission_denial_flush` | Every hour, only when Redis exists; each fleet runs its own flush. | Copies closed-hour Redis admission-denial counters to PostgreSQL `admission_denials_hourly`, then deletes flushed Redis keys. Allows five minutes after an hour closes before flushing it. No charge or access mutation. | [denial flush](../internal/river/jobs_admission_denials.go) |
| `openrails.provider_refresh` | Every **2 hours**. | Scheduler only: queues `openrails.provider_refresh_merchant` for active merchants with PSPs, deduplicated per merchant and spread over a 30-minute window. Skips merchants with no PSP. Does not itself pull provider data. | [scheduler](../internal/river/jobs_provider_refresh.go), [interval](../internal/providerrecovery/gate.go) |

### What cleanup deletes

The cleanup pass prunes the retained event log and producer journals; old inbox
notifications; completed webhook deduplication records; expired action
reservations; carts, idempotency keys and Solana Pay references; abandoned periodic
leases; payment attempts, rebill cycles and NMI history aggregates; subscription
transitions/billing-change history; eligible expired order attempts and unpaid
closed orders; failed usage windows and old meter-period quantities; admission
denial/webhook-health counters; resolved findings, reconciliation runs and released
parks; eligible finished instruction-only provider intents and mutation logs.

It also creates monthly `usage_events` and `admission_operations` partitions two
months ahead and drops fully expired partitions. Cleanup uses bounded deletes:
1,000 rows per statement, 200 merchants per job, and a default 50,000-row budget per
merchant per pass. Large backlogs drain through continuations and later passes.

Examples of retention: event log defaults to 90 days (configurable);
read/unread inbox rows 90/180 days; payment-attempt history 761 days; counters
397 days; resolved findings/runs 366 days; carts 30 days. Permanent financial
records and money-moving intent receipts are not a general cleanup target.
See [retention constants and instruction-only intent list](../internal/retention/retention.go),
[table rules](../internal/retention/tables.go), and [operations](operations.md).

## 2. Hourly billing-pass scans: all nine, in execution order

Eight scans below also have registered River workers, but **none has its own
periodic declaration**. Their ordinary timer is the enclosing
`openrails.billing_pass`; some also have immediate wake-ups. NMI history is the
one function-only scan, explicitly identified below.

| Scan / job kind | What selects work or wakes it | What it does / consequence | Source |
| --- | --- | --- | --- |
| `openrails.dunning` | Due subscription retry/renewal rows; also a merchant-scoped job after provider-refresh coverage advances. | Admits NMI recovery charges as `manual_rebill` operations and engine renewals as collection operations; handles exhausted waits for cards and asks for provider verification when confirmation is overdue. The scan queues operations; their own jobs contact the charging provider. Full mode permits system charging; limited mode can materialize held operations; readonly observes. | [dunning](../internal/river/jobs_dunning.go) |
| `openrails.balance_watch` | `balance_watches.next_check_at` is due. | Expires lapsed credit lots, posts running-charge daily checkpoints, evaluates low/depleted balance crossings, records host/customer notices and plans the next look. The per-balance job in section 3 uses the same money-service behavior. | [balance watch](../internal/river/jobs_balance_watch.go), [balance behavior](../internal/modules/money/balance_watch.go) |
| `openrails.delinquency` | `customer_delinquency.next_evaluation_at` is due. | Re-evaluates usage debt as a due date/grace deadline passes. Changes delinquency state and records events/notices; refuses new spend under policy. Makes no provider call and moves no money. Runs in limited/readonly mode too. Order mutations also evaluate the payer inline. | [delinquency](../internal/river/jobs_delinquency.go) |
| `openrails.order_expiry` | Order/attempt expiry deadlines and abandoned bank-authentication card saves. | Expires eligible orders and customer-waiting attempts, releases claims, and abandons stale payment-method setups. Leaves processing payments for a provider answer. | [expiry](../internal/river/jobs_order_expiry.go) |
| `openrails.usage_orders` | Included only with `UsageBillingEnabled`; due usage-account bill dates and order collection dates. Threshold crossings and completed provider observations can also enqueue it immediately. | Rates/bills due usage accounts into orders, then admits due order collections as `order_collection` operations. Collections wait for limited mode to end. Pages of 200 accounts/orders continue in another job. | [usage scan](../internal/river/jobs_usage_orders.go), [arguments/threshold wake](../internal/modules/money/account_usage_billing.go) |
| `openrails.rebill_watch` | Overdue provider-billed renewals; NMI schedule at least 24 hours past its expected cycle without a recorded attempt. | Reads provider evidence to recover a lost charge/webhook; only proven absence establishes a missed rebill. A cycle NMI skipped can be queued for collection. Other unconfirmed provider renewals become unverified while access is kept; elapsed time alone is not cancellation evidence. | [rebill watch](../internal/river/jobs_rebill_watch.go) |
| `openrails.nmi_history` **(scan only; no registered River kind/args)** | Active NMI PSPs whose history read is due. Checked hourly; eligible again 23 hours after the last read, giving roughly daily reads. | Reads NMI authorization/decline history into monthly aggregates for reporting. First read reaches back 25 months; subsequent reads overlap the month before the last read. Replaces covered aggregates atomically; reads providers without charging them. | [history scan](../internal/river/jobs_nmi_history.go) |
| `openrails.config_realerts` | An unresolved undeclared-account finding was last notified at least 24 hours ago. | Reloads merchant configuration first, resolves corrected findings, claims still-due findings and alerts the operator again. No alert service means no claimed/delivered re-alerts. | [configuration alerts](../internal/river/jobs_config_realerts.go) |
| `openrails.stored_card_refresh_due` | `stored_card_checks` marks a card due or its next renewal enters its vault's lookahead window. | Plans or queues one-card refresh jobs for armed account-updater vaults. Spreads requests over the following hour; rechecks an unanswered queued card after 24 hours. The scan itself stops opening new pages after ten minutes. | [card refresh planner](../internal/river/jobs_stored_card_refresh.go) |

The billing pass flags dunning, balance watches, delinquency and enabled usage
billing as critical overdue work. Its other scans do not have `DueBy` reporting.
See [scan wiring](../internal/app/river_register.go).

## 3. Jobs driven by mutations, imports, webhooks or scheduled deadlines

These eleven kinds complete the set of 25 core workers. They have no separate
periodic declaration; two-hour refreshes, the hourly scans or domain mutations
create their work.

| Job kind | Trigger / scope | What it does / external effects | Source |
| --- | --- | --- | --- |
| `openrails.provider_refresh_merchant` | `provider_refresh` schedules one per merchant; standalone queue is `provider_refresh`, host fleet queue is `billing`. | Pulls bounded provider event/history windows, recovers missing observations/charges, reconciles unknown subscriptions, reads CCBill DataLink where armed, runs local convergence and advances PSP-specific watermarks. Default overlap 24 hours, safety lag 5 minutes, windows bounded to 8 days. Can queue later dunning/usage work and deferred provider operations; refresh itself does not invoke a provider writer. | [merchant refresh](../internal/river/jobs_provider_refresh.go) |
| `openrails.provider_operation` | Accepted `provider_intents` row, its next attempt/verification time, explicit wake or lease expiry. One operation identity per job. | Executes or verifies one authorized provider action from section 5 until terminal. Owns durable wake-up/retry, while the intent ledger owns authorization, evidence and accounting. Can charge, refund, cancel, change provider state or delete vault methods depending on type. | [worker](../internal/river/jobs_provider_intents.go), [wake contract](../internal/intents/wake/wake.go), [dispatch](../internal/intents/river_dispatch.go) |
| `openrails.subscription_converge` | Stripe/NMI subscription/payment webhooks, or fixing/releasing a parked subscription. Identity: merchant, PSP, rail, remote subscription ref. | Debounces a burst for 5 seconds, fetches current provider truth by ID, imports/mirrors/converges the subscription and its effects. Provider failure retries and preserves access. A wake arriving during an in-flight read creates a follow-up. | [subscription worker](../internal/river/jobs_subscription_converge.go), [enqueue/coalescing](../internal/wakejobs/wakejobs.go), [park release](../internal/modules/parking/parking.go) |
| `openrails.provider_object_observe` | Stripe product/price create/update/delete webhooks. Identity: merchant, PSP, object kind and ref. | Debounces for 5 seconds, reads the current catalog object, records observations/retirement and synchronizes unmapped-price findings. Read-only toward the provider; does not create a local sellable price automatically. | [object observer](../internal/river/jobs_provider_object_observe.go), [Stripe trigger](../internal/modules/webhooks/stripe_catalog.go) |
| `openrails.customer_converge` | Customer-affecting domain mutations commit a coalesced customer job in the same transaction. | Reconciles grant effects, references, duplicate charges, cleared findings and access-ended notices for that customer. Lowest River priority (4) lets charges/reads run first. Skips readonly merchants; concurrent mutations can chain another read. | [worker](../internal/river/jobs_customer_converge.go), [producer](../internal/convergejob/convergejob.go) |
| `openrails.balance_look` | Balance mutation crosses a watched line, or the last look schedules its next exact check. | Runs the due balance watch for one merchant/customer/currency; expires lots, checkpoints running charges, emits crossing notices and schedules its successor. An obsolete/not-yet-due look does nothing. | [worker](../internal/river/jobs_balance_watch.go), [identity/options](../internal/balancelook/look.go), [scheduling](../internal/modules/money/balance_watch.go) |
| `openrails.stored_card_refresh` | Card-refresh due scan, or an expired/reissued-card decline. Scope: one stored card/version. | Uses an `account_updater` operation to ask the card's vault for updates, applies replacement/closed-card lifecycle effects and wakes eligible renewals. A declined card without a replacement eventually requests customer action. **May make a billed vault request and mint a token.** | [card worker/handler](../internal/river/jobs_stored_card_refresh.go), [card identity/options](../internal/modules/paymentmethods/card_versions.go) |
| `openrails.psp_import` | Import creation, changing links, or approval/application. Steps: `pull`, `relink`, `apply`; queue `psp_import`. | Pull reads a PSP into staged observations; relink re-evaluates proposed matches; apply lands approved catalog/customer/subscription/payment observations locally. Resumes work after failures. No recurring timer and no instruction to re-charge imported payments. | [worker](../internal/river/jobs_psp_import.go), [admission/options](../internal/pspimport/importer.go), [step dispatch](../internal/pspimport/pull.go), [relink trigger](../internal/pspimport/link.go) |
| `openrails.charge_attributions` | A billing import declares charges without complete amounts/attribution; job commits with the import. | Reads due charge declarations from NMI in batches, records each payment against its declared subscription, or waits for CCBill pulls/index lag. Up to 20,000 reads selected per run; snoozes while pending declarations have future attempts. Reads/records existing charges, rather than collecting new money. | [worker](../internal/river/jobs_charge_attributions.go), [args](../internal/reconcile/attributions.go), [producer](../internal/billingimport/billingimport.go) |
| `openrails.destructive_alert` | Opening a destructive-action hold commits an incident fanout job; that job inserts one delivery job per configured alert channel. | Notifies operators about a held destructive-action incident. Each channel retries independently. Job stores channel references, not webhook URLs or credentials. | [worker/options](../internal/river/jobs_destructive_alert.go), [producer](../internal/app/destructive.go) |
| `openrails.merchant_secret_cleanup` | Merchant purge transaction captures its cleanup target and enqueues this job. | Deletes that purged merchant's Vault configuration and records success/failure on the purge run. **Deletes external secrets.** River retries failures; purge and cleanup status are separately inspectable. | [worker](../internal/river/jobs_merchant_secret_cleanup.go), [cleanup service](../internal/merchants/secret_cleanup.go), [producer](../internal/merchants/purge.go) |

## 4. Scheduled wake-ups and retry behavior

| Cause | How future work is represented |
| --- | --- |
| Financial operation retry/verification | `provider_operation` runs at the ledger's `next_attempt_at` or relevant lease deadline. Waiting uses River snooze or a committed successor. Infrastructure errors/panics retain the operation with a one-minute snooze; ordinary handler backoff defaults to 2m, 4m, 8m, capped at 2h, with handler-specific policies where supplied. |
| Exact balance depletion / credit expiry / checkpoint | `balance_look` gets `ScheduledAt` from the next balance-watch instant. Its identities include the instant so a later scheduled look is distinct. |
| Provider refresh fanout | Scheduler assigns per-merchant `ScheduledAt` slots across 30 minutes. |
| Webhook event bursts | `subscription_converge` and `provider_object_observe` schedule a 5-second debounce; an in-flight predecessor can cause a snoozing follow-up. |
| Ahead-of-renewal stored-card check | Hourly card planner inserts refreshes with jitter across the following hour. |
| Stripe endpoint overlap retirement | The targeted endpoint job schedules a successor at retirement; configuration changes can bring pending work forward. |
| Import charge-attribution/index lag | `charge_attributions` snoozes until the next pending declaration's attempt time. |
| Renewal, retry, invoice due, delinquency grace, order expiry, access lapse | Usually a deadline on a domain row selected by the hourly billing/health pass, rather than a separate future River row for each deadline. |

### Attempts and runtime budgets

Unless explicitly overridden, unspecified attempt counts use the River client
default (25 in pinned River v0.49.0), which a host may configure. Unspecified
retry policies use River's retry policy. Snooze is waiting and does not consume
the normal failure-attempt budget.

| Kind(s) | Explicit attempt / deduplication behavior |
| --- | --- |
| `billing_pass`, `merchant_health`, `admission_denial_flush` | One attempt, unique per period; a later periodic pass takes remaining due work. |
| `provider_refresh` | One attempt, unique by two-hour period and outstanding state. Per-merchant child jobs deduplicate by merchant while outstanding and use client-default attempts. |
| Deployment-wide `dunning`, `balance_watch`, `delinquency`, `order_expiry`, `rebill_watch`, `config_realerts`, `stored_card_refresh_due` | One attempt, coalesced while outstanding. Merchant-scoped dunning wakes use client-default attempts instead. |
| `usage_orders` | One attempt, coalesced by arguments/outstanding state, including page continuations. |
| `provider_operation` | Explicit maximum 32,767 infrastructure attempts; business waiting uses snooze/successors. A River failure budget is not the business decision to abandon accepted money. |
| `stored_card_refresh` | Four attempts; final declined-card handling asks the customer unless a replacement settled it. |
| `balance_look` | Five attempts; hourly `balance_watch` is its backstop. |
| `stripe_webhook_reconcile` | Five attempts, coalesced by PSP arguments/outstanding state; daily pass is the backstop. |
| `destructive_alert` | 100 attempts, deduplicated by incident/channel arguments, including completed jobs. |
| `psp_import` | Pull/relink: 10,000 attempts for transient outages; apply: 10. Non-transient failure budget: five. Retry delay is attempt⁴ seconds, capped at one hour. Permanent errors or exhausted budget mark the import failed. |
| `cleanup_expired_data` | One attempt, unique by page arguments/hour; the next hour's pass takes what it left. |
| Other core kinds | See the argument/producer links in section 3; use client-default attempts unless the producer supplies options. Customer/object wake-ups coalesce outstanding work; completed jobs do not absorb later changes. |

All core jobs have a **10-minute timeout**, with these overrides:

- **1 hour:** `stored_card_refresh_due`, `cleanup_expired_data`,
  `provider_refresh_merchant`, `merchant_health`, `rebill_watch`, `charge_attributions`.
- **2 hours:** `psp_import`.
- **4 hours:** `billing_pass`, the sum of all nine scan budgets (including usage
  even when disabled). Inside it, six scans have 10 minutes each; rebill watch,
  NMI history and card-refresh planning have an hour each. Card planning also has
  its own shorter ten-minute page-opening bound.

The fleet sets `RescueStuckJobsAfter` to at least ten minutes and its client job
timeout when a host has not set it. See [timeouts](../internal/river/timeouts.go),
[scan budgets](../internal/river/jobs_billing_pass.go),
[operation retry](../internal/river/jobs_provider_intents.go), and
[backoff](../internal/intents/backoff.go).

## 5. Provider operations carried by `openrails.provider_operation`

These are **operation types, not additional River job kinds**. The registry is
built for both synchronous request execution and background execution, so an
operation can finish inline or continue through River. System writes are gated
by process/merchant posture and applicable destructive-operation controls.
Credentials/rail availability can leave an operation parked for later recovery.

All 22 types below are registered by
[buildIntentRegistry](../internal/app/river_register.go), with `account_updater`
conditional on an account-updater-capable resolver. The descriptions identify the
action being authorized, rather than claiming every configured host executes it.

| Operation type | Trigger / action |
| --- | --- |
| `nmi_recurring_confirmation` | Customer recovery of an existing NMI recurring arrangement: confirm the stored card/mandate and apply the resulting subscription/payment evidence. [Handler](../internal/intents/nmi_recurring_confirmation.go). |
| `nmi_delete_subscription` | An accepted/deferred cancellation deletes the recurring NMI schedule. [Handler](../internal/intents/nmi_delete.go). |
| `nmi_payment_source_update` | A payment-method swap updates the source backing an NMI recurring schedule. [Handler](../internal/intents/nmi_payment_source_update.go). |
| `ccbill_cancel_subscription` | Cancels an accepted CCBill subscription at the provider. [Handler](../internal/intents/ccbill_cancel.go). |
| `nmi_refund` | Executes an accepted NMI refund/appropriate reversal and records evidence. [Handler](../internal/intents/refund.go). |
| `stripe_refund` | Executes an accepted Stripe refund and records evidence. [Handler](../internal/intents/refund.go). |
| `stripe_cancel_subscription` | Carries an accepted cancellation to Stripe. [Handler](../internal/intents/stripe_cancel.go). |
| `stripe_payment_method_domain` | Registers the payment page's host on a Stripe PSP for Apple Pay / Google Pay express checkout. [Handler](../internal/intents/stripe_payment_method_domain.go). |
| `manual_rebill` | An accepted recurring-recovery payment, commonly from dunning or a customer retry. Charges and reconciles the recorded renewal episode. [Handler](../internal/intents/manual_rebill.go). |
| `order_collection` | Collects an accepted order obligation, including usage debt, through the selected instrument; verifies uncertain submissions before deciding what to do next. [Handler](../internal/modules/money/usage_collection_intent.go). |
| `subscription_collection` | Collects an accepted engine-owned subscription renewal and records its payment/period outcome. [Handler](../internal/modules/money/subscription_collection_intent.go). |
| `nmi_sale` | Historical name for a one-time saved-card sale; supported PSP/custodian paths are broader than the name. Charges accepted purchase/order terms. [Handler](../internal/modules/payment/nmi_sale_intent.go), [terms](../internal/modules/payments/sale_terms.go). |
| `initial_membership` | Executes the accepted first membership enrollment/payment and records its accepted terms and result. [Handler](../internal/modules/payment/initial_membership_intent.go). |
| `nmi_upgrade` | Executes an accepted NMI membership upgrade and reconciles its charge/schedule effects. [Handler](../internal/modules/payment/nmi_upgrade_intent.go). |
| `stripe_tier_change` | Carries an accepted subscription plan/seat change to Stripe and records its resulting charge/state. [Handler](../internal/modules/payment/stripe_tier_change_intent.go). |
| `price_migration_push` | Pushes scheduled price/plan migration moves to Stripe or NMI subscription schedules. [Handler](../internal/intents/price_migration_push.go). |
| `nmi_vault_delete` | Durable customer/admin removal of an NMI-vaulted payment method. [Handler](../internal/intents/nmi_payment_method_delete.go). |
| `hyperswitch_method_delete` | Durable removal of a HyperSwitch-held payment method. [Handler](../internal/intents/hyperswitch_method_delete.go). |
| `platform_method_delete` | Durable removal of a platform-held payment method. [Handler](../internal/intents/platform_method_delete.go). |
| `nmi_card_vault` | Creates/updates the NMI vault representation needed for an accepted stored-card flow. [Handler](../internal/intents/nmi_card_vault.go). |
| `account_updater` | Billed vault account-updater request for one card; may return a replacement/token or closed-card result. Expires after the vault's 24-hour idempotency window. [Handler](../internal/river/jobs_stored_card_refresh.go). |
| `solana_pull` | Executes a due recurring Solana pull, records the signature before sending, verifies on-chain results and applies confirmed outcomes. Missing cranker/chain wiring parks or leaves the operation unresolved. [Handler](../internal/modules/solana/pull/pull.go). |

## 6. Business events emitted by OpenRails

The retained merchant event log combines host facts and customer-notification
facts. They append in the domain transaction, have stable IDs, and are consumed
through `ListEvents` or its HTTP surface using application-owned cursors. They are
not 37 extra River workers. Default retention is 90 days; reader progress does not
extend it. PostgreSQL notifications can wake listeners, but the log remains the
durable record. See [event-log contract](event-log.md),
[reader](../internal/eventlog/log.go), and [notification service](../internal/modules/subscriptions/notification_service.go).

### Host facts: all 16 current types

Catalog/payload definitions: [billing/host_events.go](../billing/host_events.go).

| Event type | Meaning / intended consumer action |
| --- | --- |
| `destructive.hold_opened` | A destructive-action hold opened; exposes the incident to the host/operator. Also causes the separate alert fanout job. |
| `destructive.hold_cleared` | The destructive-action hold was cleared. |
| `payment.settled` | A rail payment succeeded; names its customer, amount, currency and order/subscription where applicable. |
| `delinquency.grace` | An indebted customer entered a grace state. |
| `delinquency.entered` | Debt passed policy grace/floor and the customer became delinquent. Host owns any service shutoff. |
| `delinquency.cleared` | Delinquency cleared after the underlying debt/state changed. |
| `product.entitlements_changed` | Product entitlement keys were added/removed, affecting its holders. |
| `order.completed` | An order completed; the host can fulfill it. |
| `order.requires_action` | Order payment requires customer action. |
| `order.payment_failed` | An order payment failed. |
| `order.canceled` | An order was canceled. |
| `order.expired` | An eligible order passed its expiry deadline. |
| `order.voided` | A renewal order became unowed when its subscription ended. |
| `mandate.revoked` | Customer revoked a charge agreement; further charging under that agreement stops. |
| `balance.low` | A watched balance crossed below its effective low amount/runway line. |
| `balance.depleted` | A watched balance's capacity reached zero. The host should stop the service/resource the running charges pay for. |

### Customer-notification facts: all 21 current types

Catalog: [notification_event.go](../internal/db/models/notification_event.go).
Payload/action classification: [billing/notifications.go](../billing/notifications.go).
Applications own delivery, presentation and read state; successful-charge receipts
can be optional under the public classification helpers.

| Event type | What happened / what the customer is told |
| --- | --- |
| `premium_started` | Membership/access started. |
| `premium_renewed` | Membership renewed; includes its new period and an applied scheduled downgrade where applicable. |
| `premium_ended` | Access ended, with reason such as expiry, refund, chargeback or observed access closure. |
| `subscription_canceled` | Recurrence stopped; paid access can remain until its end. |
| `payment_method_failed` | A charge failed; includes reason/retry/recovery context when known. |
| `payment_method_auto_updated` | The provider/vault automatically updated the saved method. |
| `payment_method_update_required` | Customer must supply/update a payment method. |
| `payment_authentication_required` | Bank requires the customer to authenticate a renewal payment/order. |
| `one_off_purchase_completed` | Receipt for a completed one-off purchase. |
| `order_collection_stopped` | Automatic usage-order collection stopped because recovery was exhausted or the mandate/instrument is non-recoverable; debt remains. |
| `account_delinquent` | Usage debt outlived the merchant's grace policy. |
| `account_delinquency_cleared` | The account's delinquency cleared. |
| `agreement_revoked` | Customer revoked usage collection in a currency; any remaining debt/order is still owed. |
| `usage_charged` | Receipt for a usage-order payment under the customer's agreement. |
| `balance_low` | Balance/runway crossed its low line. |
| `balance_depleted` | Available capacity reached zero. |
| `order_due` | A usage order has an amount due, with due date/collection method. |
| `order_overdue` | A usage order passed its due date. |
| `subscription_reprice_scheduled` | Advance notice of a scheduled recurring amount change, not proof it already applied. |
| `subscription_plan_change_scheduled` | Advance notice of a scheduled plan migration. |
| `subscription_changed` | Receipt for a staff-assisted subscription change, including effective plan/seats and charge where applicable. |

No timer is implied by “scheduled” in a notification name: the fact announces
accepted future terms; renewal/migration execution follows the applicable domain
deadline/operation path. Historical `customer.network_linked` and
`customer.network_unlinked` facts can remain readable but are no longer produced
by core. See [event-log ownership notes](event-log.md).

## 7. Incoming provider events and their relationship to River

Webhook dispatch is provider-specific domain processing. There is no registered
generic `openrails.webhook` job in this snapshot. Some handlers enqueue a fetch or
wake an existing financial operation; others apply verified observations directly.
Signature/deduplication processing does not create an additional kind per event.

### Stripe: all 29 enabled/handled event types

Canonical list and dispatch: [stripe.go](../internal/modules/webhooks/stripe.go).

| Event types | Processing |
| --- | --- |
| `payment_intent.succeeded`, `payment_intent.payment_failed`, `payment_intent.requires_action` | Wake the corresponding engine financial operation to recover its provider result. |
| `invoice.paid`, `invoice.payment_failed` | Mark subscription dirty and queue `subscription_converge`. |
| `customer.subscription.updated`, `customer.subscription.deleted` | Queue subscription convergence and reconcile stored payment state. |
| `invoice_payment.paid` | Apply invoice-payment evidence. |
| `checkout.session.completed`, `checkout.session.async_payment_succeeded` | Complete/register the checkout purchase from the successful session. |
| `checkout.session.async_payment_failed`, `checkout.session.expired` | Record checkout failure/expiry and release applicable waiting state. |
| `refund.created`, `refund.updated`, `charge.refunded` | Reconcile refund/reversal evidence and its local effects. |
| `charge.succeeded` | Reconcile charge/payment evidence. |
| `charge.dispute.created`, `charge.dispute.closed` | Reconcile dispute outcome and applicable payment/access effects. |
| `payment_method.attached`, `payment_method.detached`, `payment_method.automatically_updated`, `payment_method.updated`, `customer.updated` | Reconcile saved-method, default-method and card lifecycle state; may wake affected recovery paths. |
| `product.created`, `product.updated`, `product.deleted`, `price.created`, `price.updated`, `price.deleted` | Queue `provider_object_observe`; the worker reads current truth by ID. |

### NMI: all 14 handled event types

Dispatch: [nmi.go](../internal/modules/webhooks/nmi.go);
card updater: [nmi_acu.go](../internal/modules/webhooks/nmi_acu.go).

| Event types | Processing |
| --- | --- |
| `recurring.subscription.add`, `recurring.subscription.update`, `recurring.subscription.delete` | Mark subscription dirty and queue fetch/convergence. |
| `transaction.sale.success`, `transaction.sale.failure`, `transaction.sale.unknown` | Route transaction evidence/wakes through the transaction handler, including subscription convergence or operation recovery as applicable. |
| `transaction.refund.success`, `transaction.refund.failure` | Apply successful refund evidence or record refused/failed refund evidence. |
| `transaction.void.success`, `transaction.void.failure` | Apply successful void/reversal or record its failure. |
| `acu.summary.automaticallyupdated`, `acu.summary.contactcustomer`, `acu.summary.closedaccount` | Fold card-updater results into payment-method/subscription recovery and customer-action state. |
| `chargeback.batch.complete` | Read/process chargeback batch evidence and corresponding local financial/access effects. |

### CCBill: all 14 handled event types

Dispatch and handlers: [ccbill.go](../internal/modules/webhooks/ccbill.go).

| Event types | Processing |
| --- | --- |
| `NewSaleSuccess`, `NewSaleFailure` | Record successful initial sale/membership or a refused initial sale. |
| `RenewalSuccess`, `RenewalFailure` | Apply observed renewal or failed renewal to payment/subscription state. |
| `UpgradeSuccess`, `UpgradeFailure` | Apply or record the outcome of an upgrade. |
| `Cancellation`, `Expiration` | Apply canceled recurrence or ended access according to provider evidence. |
| `BillingDateChange` | Update the observed billing schedule. |
| `CustomerDataUpdate` | Update observed provider customer details. |
| `UserReactivation` | Apply reactivated membership state. |
| `Refund`, `Void`, `Chargeback` | Record the reversal/dispute and applicable payment/access effects. |

### Basis Theory: seven explicit event cases

Dispatch: [basistheory.go](../internal/modules/webhooks/basistheory.go).

| Event types | Processing |
| --- | --- |
| `token.deleted`, `token.expired` | Park the missing instrument; does not automatically terminal-cancel its subscriptions. |
| `token.updated` | Read current token/card state and reconcile the instrument. |
| `token.property.expired` | Explicit no-op for expected post-purchase CVC-retention expiry. |
| `token-intent.converted` | Reconcile completion of a token-intent conversion. |
| `network-token.updated`, `network-token.deleted` | Fold network-token state/deletion into stored instrument state. |

Other Basis Theory types, including declared SDK constants such as
`token.created` and `network-token.created`, are not explicit action cases in this
handler. Provider event names are not River job kinds.

## 8. AuthKit jobs contributed by the standalone server

The standalone control plane adds `authkit.Client.RiverJobs()` to the engine.
Core embedded OpenRails does not automatically supply an AuthKit control plane.
Pinned versions: AuthKit **v1.43.0**, Helpers **v1.18.0**, River **v0.49.0**.
See [attachment](../server/internal/operator/controlplane.go),
[contribution](../server/internal/controlplane/routes.go), and [module pins](../go.mod).

The following five dependency kinds are separate from the 25 core kinds. Their
registration is in pinned
[AuthKit river.go](https://github.com/open-rails/authkit/blob/v1.43.0/internal/engine/river.go).

| Job kind | Schedule / queue | What it does |
| --- | --- | --- |
| `authkit_cleanup_expired_auth_state` | `CleanupInterval`, default hourly, startup catch-up; `authkit_maintenance_<schema>` (hashed fallback for names River cannot accept), one worker by default. | Deletes expired codes/ceremonies/counters, dead refresh sessions/history, expired name claims, terminal lifecycle receipts/keys/invitations, old session events and retained identity-event history. Five-minute job timeout. [Cleanup](https://github.com/open-rails/authkit/blob/v1.43.0/internal/engine/host_cleanup.go). |
| `authkit_account_delete_due` | Daily with startup catch-up, plus explicit purge requests. Dispatcher on maintenance queue; issuer-specific batches on `authkit_delete_due-<hash>`, one worker by default. | Scans due account-deletion lifecycle records and resumes account cleanup/purge for each issuer. Deadlines live in domain rows, not a future River job per account. Five-minute timeout. [Sweep](https://github.com/open-rails/authkit/blob/v1.43.0/internal/engine/account_delete_due.go). |
| `authkit_credential_sweep` | Mutation-driven; `authkit_credentials-<hash>` queue, one worker by default. | Retires/reconciles credentials this application issued when an authority change in another application affects them. [Worker](https://github.com/open-rails/authkit/blob/v1.43.0/internal/engine/credential_sweeps.go). |
| `authkit_backchannel_logout` | OAuth consent revocation, when the client has a back-channel logout URI; maintenance queue. | Sends an OIDC back-channel logout token to the affected OAuth client's endpoint. Eight attempts, 30-second worker timeout, ten-second HTTP request timeout. [Producer/worker](https://github.com/open-rails/authkit/blob/v1.43.0/internal/engine/oauth_consents.go). |
| `authkit_provisioning` | Only with configured SCIM provisioning targets: one periodic per target, default every five minutes, startup catch-up; `authkit_provisioning-<hash>` queue, concurrency defaults to target count. | Sends pending current user states to configured SCIM destinations, tracks accepted rows/backoff and periodically reconciles remote drift (default daily) inside this same job. Four-minute run budget. [Worker](https://github.com/open-rails/authkit/blob/v1.43.0/internal/engine/provisioning.go). |

AuthKit also defines `authkit_notification_delivery` in its optional remote-inbox
`NotificationDelivery` component. **OpenRails does not attach that component in
this snapshot**, so it is not included as an active standalone contribution.
Likewise, customer billing facts do not imply a background billing-email worker.

For audit completeness, AuthKit's separate retained identity event catalog has
24 types: `user.registered`, `user.email_changed`, `user.phone_changed`,
`user.username_changed`, `user.banned`, `user.unbanned`, `user.deleted`,
`user.restored`, `user.purged`, `user.sessions_revoked`, `user.agreement_accepted`,
`role.granted`, `role.changed`, `role.revoked`, `group.created`, `group.deleted`,
`group.purged`, `group.role_created`, `group.role_updated`, `group.role_deleted`,
`oauth_client.created`, `oauth_client.updated`, `oauth_client.deleted`, and
`oauth_consent.revoked`. Each records the named identity/authority mutation;
consumers own their cursors. These are not core billing events or extra River
workers. See pinned [catalog](https://github.com/open-rails/authkit/blob/v1.43.0/iam/event.go).

## 9. Background activity outside River

These matter when auditing all scheduled behavior, but do not appear as River job
kinds or independently retryable River rows.

| Activity | Trigger / timing | What it does / source |
| --- | --- | --- |
| Unverified-subscription verifier | PostgreSQL notifications and refresh requests; default coalescing 300ms, four concurrent readers. | Reads current provider state for unverified subscriptions, with bulk reads for bursts and local convergence. Rows remain unverified if reads fail; later notifications/passes rediscover them. [Verifier](../internal/reconcile/verify.go). |
| Solana Pay poller | Started by runtime workers when wired; notification wake-ups and next due reference, minimum 500ms wait. | Watches pending Solana Pay references and applies observed chain payments. Sleeps without a timer when there are no due references to watch. [Poller](../internal/modules/solana/poller.go), [startup](../internal/app/runtime.go). |
| FX rate refresh | Lazy start on first cross-currency quote; every two hours thereafter. | Refreshes held currency tables; failed fetches leave held data subject to rate freshness policy. [Rates](../internal/integrations/fx/rates.go). |
| In-flight operation / idempotency claim renewal | While a request/job holds a claim; heartbeat at one quarter of its lease. | Keeps ownership alive while the actual action runs. This is an in-flight heartbeat, not a recurring job that scans for new financial work. [Operation heartbeat](../internal/intents/runner.go), [idempotency heartbeat](../internal/modules/idempotency/store.go). |
| AuthKit sender-health checks | When the attached AuthKit client starts; default every five minutes. | Checks configured email/SMS senders and updates channel health. Does not enqueue customer billing emails. [Pinned sender lifecycle](https://github.com/open-rails/authkit/blob/v1.43.0/internal/engine/host_senders.go). |

River itself also runs leader election, scheduled-job promotion, retry/rescue and
finished-job cleanup. Those are River's fleet services, not extra OpenRails
business job kinds. Their effective configuration belongs to the fleet owner.

## Audit notes and completeness checks

- **Actual schedule beats an unused constant:** `money.UsageOrdersInterval` still
  says five minutes, but it is not in `Periodics`; current registration runs usage
  inside the hourly billing pass. Threshold-triggered wakes remain immediate.
- **Kind-looking names are not always jobs:** `openrails.nmi_history` labels a
  billing scan and has no `JobArgs.Kind()`, registered worker or kind timeout.
- **Separate observation from execution:** provider refresh reads and records
  provider truth; any admitted charge/cancel is executed through its own
  authorized provider-operation path. Stripe endpoint reconciliation itself does
  write provider configuration.
- **Business facts are not delivery tasks:** the 37 billing event names do not
  imply 37 workers, a core account-event poller, or automatic email delivery.
- **Conditional is not deployed:** Redis, usage billing, PSP/vault credentials,
  Solana wiring, SCIM targets and control-plane attachment determine which
  capabilities actually run. Live queue state would be a separate audit.

Coverage was checked against every core `Kind()` declaration, all 25
`addWorker` registrations, the timeout map, all six `Periodics` entries, all nine
scan registrations, the complete intent-handler registry, both billing event
catalogs and provider dispatch switches. Dependency jobs were read from the exact
pinned AuthKit module source. Documentation-only change; no jobs were run, no
provider actions performed, and no database/configuration changes made.
