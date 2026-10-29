# Provider observation and recovery

Provider observation reports differences between the local billing book and
provider records. An unresolved finding or an old bulk-refresh timestamp does
not block purchases, renewals, refunds, cancellations, or card removal.
Operators review findings separately; ordinary operations retain their own
ownership, payment-method, duplicate-charge, and provider checks.

Provider refresh runs every two hours. Each PSP is read in bounded time windows
from its watermark to five minutes ago; NMI and Stripe start 24 hours before
theirs, for late provider indexing. It records qualified payments, refunds and
provider-owned records. A failed read leaves the watermark and is read again
next pass. Unknown identities,
duplicate subscriptions and conflicting receipts remain visible findings;
acknowledging an alert does not resolve its underlying evidence.

Observation and application use separate `psp_refresh_watermarks` domains:

- `events` records observed provider windows, including advisory runs.
- `applied_events` records successfully applied account-bound windows.
- `completed_events` records the target of a complete financial observation.
  Failed reads, incomplete pagination and unresolved financial identities can
  leave it unchanged. It is reporting progress, never write authorization.

A newly configured or restored instance follows its explicit provider write
mode. `readonly` permits observation but no provider mutation; `limited`
permits customer-requested operations but no autonomous collection. Neither
setting changes automatically. A copied billing book still requires its
separate ownership/arming procedure before provider writes.

Every operation must prove its own authority. An uncertain submitted charge is
verified against its exact order, account, payer, currency and amount before
any retry. A positive receipt can be recovered without inventing a local
submission. Missing or contradictory evidence for that operation remains an
error. Removing the bulk reconciliation gate does not authorize duplicate
charges, unknown provider targets, or the deletion of a card in active use.

A subscription cancellation records nonrenewal immediately and preserves paid
access. A provider-owned recurring schedule stops through its existing deferred
operation; NMI's stop remains scheduled near the end of the paid period. Card
removal is different: the caller waits for provider confirmation and local
removal. It creates no background execution or automatic retry. An incomplete
response is an error, and the person can explicitly retry; an ambiguous earlier
attempt is verified before another provider mutation.

Provider object provenance remains part of reporting. NMI merchant-defined
field 20 and Stripe `openrails_book` distinguish this book's objects from foreign
ones where available. Unmatched foreign/test objects can be informational;
matching identities and conflicting receipts still require review. These labels
affect reporting severity, not permission for unrelated operations.

Bulk catch-up resumes from its applied cursor. Without one it uses relevant
subscriptions, unpaid usage periods and unresolved accepted operations rather
than every historical payment. Provider timestamps are not ordered cursors, and
bulk observation does not guarantee exactly-once behavior across independent
billing databases. Local PostgreSQL and provider-simulator tests do not qualify
live provider visibility or a production restore.
