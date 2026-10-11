# Customer notification integration

OpenRails owns billing notices and their delivery state. A host can compose
them with content, community, or account notices while choosing its own UI.
The customer inbox and the operator findings queue are distinct authorization
boundaries. An operator finding's private evidence never belongs in a customer
notification feed.

## A host application's inbox

`Client.ListCustomerNotifications(ctx, customerID, params)` and
`GET /v1/app/customers/{customer_id}/notifications` read one merchant customer's
retained notices. The application's credential needs `merchant:customers:read`.
The credential selects the merchant; the host authenticates its user and maps
that user explicitly to the merchant's customer. Neither matching email
addresses nor equal UUIDs across merchants establish that mapping.

The response is the usual `{"data": [...], "next_cursor": null|string}` page.
`limit` bounds the page; `cursor` continues it; optional `seen` filters read
state. Notices are newest first, ordered by creation time and ID. Each notice
includes its `merchant_id`, `customer_id`, stable `id`, extensible `event_type`,
typed `data`, `created_at`, `seen`, and `read_at`. Money in `data` remains a
decimal string. Unknown event types must remain usable: retain the payload and
let the application supply a generic presentation.

For a copied inbox, such as ContentKit, deduplicate by the configured source
installation, merchant ID and notification ID. Keep the destination's read
state separate. Importing must not mark the source read, send another email,
or acknowledge the host event outbox. Restart completed scans from the first
page: a timestamp watermark is not a guaranteed delivery cursor, and a
transaction may commit an older creation timestamp after a previous scan.

This API is a retained inbox, not a permanent event archive. Read notices
normally expire after 90 days from creation and all notices after 180 days;
merchant policy can change those periods. Recovery can also remove superseded
notices. A copy retains imported history unless the host explicitly reconciles
deletions. A consumer offline past retention cannot recover every historical
notice from this API. Use the inbox live when immediate read/supersession
consistency matters.

## Cross-merchant customer portal

`GET /v1/network/notifications` reads notices through the signed-in account's
active customer links. Each item names `link_id`, `merchant_id`, the public
merchant details, and its `notification`. Pagination includes the merchant in
its key, so matching notification IDs at different merchants stay distinct.
The account's own OpenRails sign-in and network terms are required; a merchant
application's token cannot list the account's other merchants.

`POST /v1/network/links/{link_id}/notifications/read` accepts the existing
`{"notification_ids": [...]}` or `{"all": true}` request. It shares the source
customer's read state with `/v1/me/notifications/read`. The link must still be
active and belong to the account on every request. An unlinked merchant
disappears from the portal without deleting its billing records.

## Operator workflows

Findings remain the durable operator work queue: stable subject identity,
evidence, severity escalation, reopening, recommended actions, resolution,
and operator audit. Reading a customer notice is different from resolving a
finding. General notice history and actionable work can share an inbox model
without creating duplicate copies of authoritative findings or weakening
audience-specific access checks. Financial/lifecycle host events retain their
own processing acknowledgments.
