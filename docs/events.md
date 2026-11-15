# Events and notifications

OpenRails records billing facts in a retained, ordered event log. Applications
read it and own everything after: inboxes, rendering, email, preferences and
delivery. An optional account-inbox channel publishes selected notices for you.
Merchant staff get findings as console notifications and alerts.

## Reading the event log

`Client.ListEvents` (in process or remote), or `GET /v1/app/events` with
`merchant:events:read`. OpenRails keeps no readers, offsets or acknowledgments.

```go
page, err := merchant.ListEvents(ctx, billing.EventQuery{
    After: savedCursor,
    Limit: 100,
    Types: []string{"payment_method_failed", "premium_ended"}, // optional
})
```

Process `page.Events` idempotently by `event.ID`, then save `page.NextCursor` in
the same transaction as your effects; follow `page.HasMore`. An empty cursor
starts at the oldest retained fact, and an empty page still returns a cursor to
poll from. Over HTTP: `after`, `limit` (1–500, default 100) and repeated `type`.
Filters are fixed for a cursor. `Client.EventReader()` implements the helpers
module's `eventlog.Reader`; give it to `eventlog.Consumer` to keep the same
polling code in process and remote. Its commit must apply effects and save the
cursor atomically, empty pages included.

The envelope: `id` (`hev_` or `ntf_`), `merchant_id`, `source`, `type`,
`version`, `occurred_at` (domain time), `recorded_at` (when it entered the log),
`subject` and `data`. Money in `data` is decimal strings. Notification events
snapshot `customer_id`, `account_id` and `account_issuer` (empty for native users;
null when unknown, which is not native): match the issuer as well as the UUID
before routing to an account, and never recreate an erased inbox.

Facts append in the domain transaction, and a reader never skips an earlier
uncommitted one. Retention is `event_retention` (default 90 days, `2160h`);
reading does not extend it. `410 event_cursor_expired` means the cursor predates
retention or its history was replaced: rebuild state, then start a new cursor.
`400 invalid_cursor` means malformed, or another merchant's or filter's cursor.
Cursors are positions within one deployment's history, not credentials; merchant
archives exclude the log. `Server.ListEvents(ctx, merchantID, query)` reads a
soft-deleted or archived merchant's facts for a privileged platform caller.

## Billing notice policy

- `subscription_canceled` means renewal stopped while paid access remains
  (`data.ended_at`, when present, is the access end). `premium_ended` with
  `data.access_ended: true` means access actually closed. Do not present a cancel
  click as membership ending.
- `payment_method_failed`, `payment_method_update_required`,
  `payment_authentication_required` and `order_collection_stopped` carry the
  subscription or order and the retry decision: `scheduled` (another attempt at
  `data.next_attempt_at`), `payment_method_required`, `stopped` or `unknown`.
  `data.recovery_action` is `update_payment_method`, `resubscribe`,
  `authenticate_payment` or `contact_support`; you supply the destination. Re-read
  current state before acting: a later payment may have resolved it.
- Routine `premium_renewed`, `one_off_purchase_completed` and `usage_charged`
  need no notice unless you want one; applied downgrades, access loss and payment
  failures deserve one. There is no upcoming-charge reminder.
- A price-change event describes a scheduled transition that may still be
  blocked; check the subscription.

`MerchantProfile.NotificationURL(event_type)` gives `subscribe_url` for
`premium_ended`, `billing_url` for others, falling back to `support_url`.

## Account inbox channel

`Deps.Notifications` publishes selected notices to an AuthKit account inbox in the
billing transaction (a remote publisher queues its delivery job there and retries
after commit). Without it only inbox publication stops; the facts remain.

```go
publisher, err := accounts.Notifications("openrails")
if err != nil { return err }
deps.Notifications = authkitadapter.Notifications(publisher, merchantID, accountIssuer)
```

The AuthKit producer binds source `openrails`, the merchant id as source scope and
tenant-wide visibility. The single-merchant adapter needs a nonzero merchant and
skips other merchants; equal UUIDs from different issuers are different
recipients. Notifications have deterministic ids and increasing revisions; the
producer only publishes and withdraws exact ids.

Published by default: unexpected payment failures, required payment-method updates
or authentication, stopped collection, access loss and applied downgrades (not
renewal receipts, or failures caused by a checkout or change click). Withdrawn
when resolved: a successful payment, accepted card or restored access withdraws
the issues it resolves; a confirmed cancellation or expiry withdraws its payment
requests; an order request goes when the order completes, is canceled, expires or
is voided, or is paid in full by a provider, remittance or credit.

**Adopting an existing inbox.** Stop billing writers and the old importer and back
up. Apply the AuthKit inbox and OpenRails migrations, configure the producer, then
call `LocalClient.ExportNotificationAdoption` (from `Engine.LocalMerchant`) while
writers are stopped and pass its `Adoptions` to AuthKit's `AdoptNotifications`,
which keeps receipt ids and read state and withdraws resolved issues. Start the new
producer and readers together; never run old readers after withdrawals begin. The
`Ordering` part is not in the financial archive: after an archive restore, call
`RestoreNotificationOrdering` with the matching snapshot before enabling delivery.

## Merchant alerts

A finding needing review lands in the findings queue with a deduplicated console
notification, committed with the finding. Medium and higher also go to the
merchant's outbound webhook destinations; high and critical also to its alert
email. The same open finding notifies again only if its severity rises; resolving
it ends the episode. External delivery follows the commit with bounded retries
and is best-effort.

Webhook destinations are merchant-scoped, encrypted and write-only; the settings
page adds, rotates (keeping the identity) and deletes them. Generic JSON carries
`title`, `severity`, `summary`, `fired_at` and an optional `dashboard_link`;
Discord and Slack get their native format. Delivery has SSRF protection.
Notifications are kept 90 days once read, 180 if never read.
