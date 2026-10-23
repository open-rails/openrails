# Reading billing events

Use `Client.ListEvents` on an `Engine.Merchant(id)` or remote merchant handle, or
`GET /v1/app/events` with `merchant:events:read`. The same merchant authorization
applies on every request. OpenRails keeps no readers, offsets or acknowledgments.

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
`occurred_at`, `subject` and `data`. Existing IDs retain their `hev_` or `ntf_`
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

The older host-event and notification-consumer ACK routes are transitional.
New integrations should use this log. Provider execution, account cleanup and
other actual domain work remain separate from reading facts.
