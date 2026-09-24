# Merchant notifications

Reconciliation findings that require review appear in the findings queue and
produce a deduplicated console notification. Low-severity findings stay in the
console. Medium, high and critical findings also use enabled outbound webhook
destinations; high and critical findings additionally use the merchant's alert
email address. Re-observation of the same open finding does not notify again
unless its severity increases. Resolving a finding clears that episode's
notification linkage. The episode claim and console notification commit in one
transaction; failed persistence leaves the finding eligible for retry. External
delivery follows the commit with bounded retries and is best-effort. A process
interruption after commit can lose external delivery; the console notification
remains durable, and repeated reconcile passes do not resend successful hooks.

Webhook destinations remain merchant-scoped, encrypted, write-only credentials.
The settings page supports adding, rotating and deleting destinations. Rotation
preserves the destination identity; incomplete credential/metadata rotation
refuses delivery. Generic JSON contains title, severity, summary, fired_at and an
optional dashboard_link. Discord and Slack destinations receive their native
message format. Delivery retains bounded retries and SSRF protection.

## Recipient storage

Customer billing notices and the merchant console bell share `notifications`.
Each row declares `recipient_kind`: customer rows have a merchant-scoped
`customer_id`; merchant rows have no customer and carry the operator message.
Queries and mutations select their recipient kind explicitly. `read_at` is the
single inbox read marker; the customer API derives `seen` from it. Only customer
rows enter the notification email sweep. Retention applies to both recipient
kinds: by default read rows expire after 90 days and all rows after 180 days.
Reading a notification never acknowledges a financial or lifecycle event in
`host_outbox`.

## Renewal receipts

The first purchase always sends its confirmation, and every failure,
cancellation and downgrade notice is always sent. Renewal receipts are spaced
per subscription by `renewal_receipt_min_interval_hours` (default 24): a renewal
whose period starts less than that interval after the membership start or the
last receipted renewal creates no `premium_renewed` notification. Hourly and
daily members therefore get at most one receipt a day; weekly and longer
cadences get one per renewal. `0` sends a receipt for every renewal. Receipts
carry `subscription_id`, `period_start` and `period_end`.
