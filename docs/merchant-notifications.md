# Merchant notifications

Reconciliation findings that require review are the console's durable inbox
records. Low-severity findings stay in the
console. Medium, high and critical findings also use enabled outbound webhook
destinations; high and critical findings additionally use the merchant's alert
email address. Re-observation of the same open finding does not notify again
unless its severity increases. Resolving a finding clears that episode's
notification linkage. The episode claim commits before external delivery;
failed persistence leaves the finding eligible for retry. External
delivery follows the commit with bounded retries and is best-effort. A process
interruption after commit can lose external delivery; the console notification
remains durable as its finding, and repeated reconcile passes do not resend successful hooks.

Webhook destinations remain merchant-scoped, encrypted, write-only credentials.
The settings page supports adding, rotating and deleting destinations. Rotation
preserves the destination identity; incomplete credential/metadata rotation
refuses delivery. Generic JSON contains title, severity, summary, fired_at and an
optional dashboard_link. Discord and Slack destinations receive their native
message format. Delivery retains bounded retries and SSRF protection.

## Recipient storage

Customer billing notices live in `notifications`; the merchant bell reads the
authoritative `reconciliation_findings` work queue. The notifications table also
retains legacy merchant alert rows, but current findings do not create duplicate
notification rows. Customer queries and mutations explicitly require the
customer recipient kind. `read_at` is the customer inbox read marker; the API
derives `seen` from it. Only customer
rows enter the notification email sweep. One sender at a time holds a row's
email (the dispatch that created it, its job, or any replica's sweep), so
replicas send it once; a sender that dies is replaced after five minutes, and
one that dies between sending and recording it means one duplicate. Retention
applies to notification rows: by default read rows expire after 90 days and
all rows after 180 days. Findings have their own resolution-based retention.
Reading a notification never acknowledges a financial or lifecycle event in
`host_outbox`.

## Optional charge receipts

`charge_receipts` is **off by default**. Set the merchant setting to `true` to
create successful-payment notifications for ordinary renewals
(`premium_renewed`), one-off purchases (`one_off_purchase_completed`) and
automatic usage-order payments (`usage_charged`). The preference controls inbox
creation, so disabled receipts produce neither a notification email nor a
downstream copied notification. Existing notices remain retained.

Subscription activation (`premium_started`), plan-change/downgrade
confirmations, failures, required authentication, overdue debt, cancellation,
and required advance price-change notices remain enabled. Those describe a
contractual change or required action, even when they also mention a payment.
No upcoming-charge reminder producer is added by this option.

When enabled, renewal receipts are spaced
per subscription by `renewal_receipt_min_interval_hours` (default 24): a renewal
whose period starts less than that interval after the membership start or the
last receipted renewal creates no `premium_renewed` notification. Hourly and
daily members therefore get at most one receipt a day; weekly and longer
cadences get one per renewal. `0` sends a receipt for every renewal. Receipts
carry `subscription_id`, `period_starts_at` and `period_ends_at`.

See [customer notification integration](customer-notifications.md) for host
import, the cross-merchant portal, and shared general/actionable inbox models.

## Staff changes

A subscription change staff make at the customer's request sends the customer
a `subscription_changed` notice, "Changed by support at your request.", with
`subscription_id`, `to_price_id`, `to_product_name`, `quantity`,
`effective_at`, `new_amount` (the next renewal) and, when the change charged
now, `amount` and `transaction_id`.
