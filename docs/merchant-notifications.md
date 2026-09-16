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

Configurable metric alert rules and scheduled digests are deferred from core.
There is no rule editor, template API, rule test-fire API or periodic digest job.
Customizable dashboards, provider webhook health/history, denial history, worker
health, durable findings and basic payment notifications remain available.

This is a fresh-schema hard cut: alert_rules, finding_digest_state, their two
cross-merchant lookup functions, and merchant_notifications.rule_id are absent.
There is no upgrade/backfill path or hidden replacement module. A future host
notification product can consume retained operational evidence if a concrete
consumer needs configurable rules or summaries.
