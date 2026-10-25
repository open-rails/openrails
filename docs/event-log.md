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

### Hosted account events

The hosted application owns platform-account relationships, lifecycle readers
and their cursors. Core no longer polls AuthKit for account-link changes or
produces `customer.network_linked` / `customer.network_unlinked` facts. Existing
committed facts remain readable through the retained event log for their normal
retention; their raw type and data are preserved.

Moving the account reader requires an explicit offline transfer of both link
proof and the exact source cursor/filter. New SaaS account facts use a distinct
source. Do not splice cursor positions, acknowledge source events, or reset
progress when changing ownership. See [the extraction plan](customer-network-removal-plan.md).

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
