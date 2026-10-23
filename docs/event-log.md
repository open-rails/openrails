# Reading billing events

Use `Client.ListEvents` on an `Engine.Merchant(id)` or remote merchant handle, or
`GET /v1/app/events` with `merchant:events:read`. The same merchant authorization
applies on every request. OpenRails keeps no readers, offsets or acknowledgments.

`Client.EventReader()` implements `helpers/eventlog.Reader` over that same
client. Pass it to `eventlog.Consumer` to keep polling/effect code identical when
switching from `Engine.Merchant` to `NewRemoteMerchant`. Checkpoint keys include
source, merchant, subscription and normalized filters. The consumer's commit
function must apply effects and save the cursor atomically (including empty
pages), comparing the prior checkpoint or serializing writers. HTTP and embedded
cursor refusals both match `eventlog.ErrInvalidCursor` or
`eventlog.ErrExpiredCursor`; the underlying `billing.StatusError` is preserved.

The privileged platform can call `Server.ListEvents(ctx, merchantID, query)` to
read retained facts for a soft-deleted or archived merchant. This uses the same
scoped cursor checks and rechecks directory existence each time. The caller
authorizes/audits this control-plane access, as with `Server.GetMerchant`. It
does not make a deleted merchant accessible through ordinary Client/HTTP calls.

```go
page, err := merchant.ListEvents(ctx, billing.EventQuery{
    After: savedCursor,
    Limit: 100,
    Types: []string{"payment_method_failed", "premium_ended"}, // optional
})
```

Process `page.Events` idempotently by `event.ID`, then persist
`page.NextCursor` in your application's transaction. Follow `page.HasMore` to
read another page. An empty cursor starts at the oldest retained fact. An empty
page still carries a cursor; poll from it later. The HTTP parameters are `after`,
`limit` (1–500, default 100) and repeated `type`. Filters are fixed for a cursor;
changing them starts a separate reader position.

The event envelope contains `id`, `merchant_id`, `source`, `type`, `version`,
`occurred_at`, `recorded_at`, `subject` and `data`. `occurred_at` is domain
time; `recorded_at` is when the fact entered the ordered log. Existing IDs retain their `hev_` or `ntf_`
prefix. Money values in data are decimal strings, and billing resource IDs keep
their usual prefixes. Notification events snapshot `customer_id` and `account_id`
(the host/AuthKit UUID), plus `account_issuer` (empty for native users), so
routing does not need a customer row that might later be deleted. Match the issuer
as well as the UUID before routing to a local account. A null issuer means the
historical authority could not be recovered; it must not be assumed native. The receiving application must still enforce its account-erasure
policy and never recreate an erased inbox.

Events append in the domain transaction. A per-merchant position is held until
commit, so a reader never skips an earlier uncommitted fact. A transaction
writing several merchants locks those streams in UUID order at deferred flush.
Its private pending row is deleted before commit; it is not a delivery queue.

Retention defaults to 90 days (`event_retention: 2160h`); progress does not extend
it. HTTP 410 `event_cursor_expired` means the cursor predates retention, or its
history was reset/replaced. Rebuild the required state before starting a new
cursor. HTTP 400 `invalid_cursor` means malformed input or a merchant/filter
mismatch. Cursors are not credentials. They are portable positions only within
the same deployment history; merchant archives exclude the log and its cursors.

The old host-event and notification-consumer ACK routes are removed. The
private host and notification tables remain producer deduplication journals,
retained from ingestion for at least 90 days and never less than event_retention.
They contain no reader offsets or acknowledgments. Provider execution, account
cleanup and other actual domain work remain separate from reading facts.

### Migrating from source acknowledgments

Stop all writers sharing the billing schema and take a verified database backup
before applying migrations 0110–0112. They preserve retained facts and remove the
old reader bookkeeping. Deploy all readers with application-owned cursors before
resuming writes. An older binary that still uses the removed tables cannot run
on the migrated schema; restoring it also requires restoring the matching backup.

### Standalone customer-network account events

When the optional customer network is enabled, the server polls its configured
AuthKit account tenant once a minute, at most 100 committed facts per pass.
`billing.network_event_cursors` belongs to the server. Link changes and that
cursor commit in one billing transaction, so retries cannot reorder projection
changes. A replayed revocation predating a newly proved link cannot revoke
that newer relationship. Account proof times use the same wall-clock domain
as AuthKit, independently of a simulated billing clock. This is not a source registration or an acknowledgment. Other embedded
apps do not run this job. The normal credential, consent and deletion checks
still run on requests; a payment-page proof's displayed network link follows the
bounded background projection.

A lost cursor stops this reader with an explicit error. **Do not delete the
cursor alone:** retained history might no longer contain a revoked independent
payment-page proof. Restore a matching AuthKit history and server cursor when
possible. If the history cannot be recovered, take the network offline, revoke
all its active customer links in a controlled repair transaction (emit the
usual `customer.network_unlinked` facts), and only then clear this authority's
cursor. Resume reading from the oldest retained fact and require new proofs to
link independent payment-page accounts again. Customer records, subscriptions
and payments remain intact. Apply the same procedure before replacing the
configured account issuer or tenant; those identities cannot inherit an old
network projection merely because UUIDs happen to match.

### Shared implementation adoption

Helpers supplies the reader, consumer, paging/cursor/retention algorithm and
PostgreSQL deferred queue. Contract migration0113 copies the existing facts and
stream state without changing IDs, positions, epochs, boundaries or timestamps.
The old read/prune table names become compatibility views; source-journal capture,
merchant pinning and restore suppression remain product-owned and unchanged.
Merchant deletion still cascades through the shared streams to their facts.
Same-transaction pending facts are discarded when that merchant is deleted.

The canonical Helpers SQL is copied verbatim in the migration and checked by a
contract test. Exact retained fact replay is an idempotent no-op; reusing a retained
ID with conflicting data aborts the transaction. Neither case overwrites history.

Stop older writers and retain a database backup before applying0113. It changes
physical storage and requires a coordinated fleet rollout. This is separate from
merging source or adopting the read-only shared client interface.
