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

## Customer event production

Customer billing events live in `notification_events`, independently of the
operator findings workflow and console alerts. OpenRails no longer owns a
customer inbox, read state, email templates or delivery schedule. See
[customer notification events](customer-notifications.md) for durable consumer
registration, independent acknowledgments, migration and app-owned delivery.
