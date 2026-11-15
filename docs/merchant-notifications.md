# Merchant notifications

Reconciliation findings that require review appear in the findings queue and
produce a deduplicated console notification. Low-severity findings stay in the
console. Medium, high and critical findings also use enabled outbound webhook
destinations; high and critical findings additionally use the merchant's alert
email address. Re-observation of the same open finding does not notify again
unless its severity increases. Resolving a finding clears that episode's
notification linkage.

The episode claim and its alert job commit in one transaction. That job gives
each destination its own delivery job, which retries a failed delivery up to 100
times and never sends a delivered alert again. A webhook retry carries the same
`Idempotency-Key`; email is at least once across a lost acknowledgment. An alert
whose finding is resolved before delivery is not sent, nor is one an escalation
superseded: the escalation sends its own.

Webhook destinations remain merchant-scoped, encrypted, write-only credentials.
The settings page supports adding, rotating and deleting destinations. Rotation
preserves the destination identity; incomplete credential/metadata rotation
refuses delivery. Generic JSON contains title, severity, summary, fired_at and an
optional dashboard_link. Discord and Slack destinations receive their native
message format. Delivery retains SSRF protection.

## Customer event production

Customer billing events live in `notification_events`, independently of the
operator findings workflow and console alerts. OpenRails no longer owns a
customer inbox, read state, email templates or delivery schedule. See
[customer notification events](customer-notifications.md) for durable consumer
registration, independent acknowledgments, migration and app-owned delivery.
