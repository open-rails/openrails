# Customer notification events

OpenRails records billing facts. Applications own their inboxes, recipient
mapping, read/archive state, rendering, email and delivery preferences.
OpenRails has no customer-facing inbox or per-event delivery jobs.

## Reading facts

Use [`Client.ListEvents` or `GET /v1/app/events`](event-log.md), then save the
returned cursor with local inbox changes atomically, or after idempotent
processing. A reader never registers with or acknowledges the source. The
retained envelope supplies `id`, `merchant_id`, `source`, `type`, `version`,
`occurred_at`, `subject` and `data`. Notification data snapshots the account ID
and its issuer; match both before mapping it to an application account. Preserve
unknown object fields and event kinds. An erased-recipient fence must prevent
late facts from recreating an erased inbox.

A trusted embedded portal can use `Server.ListEvents(ctx, merchantID, query)`
for each authorized merchant, including archived merchants. Every merchant has
its own cursor; there is no global cursor or remote cross-merchant endpoint.
The ordinary Client/HTTP interface retains the active-merchant policy.

The optional standalone network portal uses `Server.NetworkAccountHandler`
and checks `Server.NetworkLinks` on every inbox request. Notification URLs come
from `MerchantProfile.NotificationURL(event_type)`: `subscribe_url` for
`premium_ended`, `billing_url` for other events, falling back to `support_url`.

## Billing notice policy

Stopping renewal and losing membership are separate facts. A
`subscription_canceled` event records stopped renewal while purchased access
remains. Its `data.ended_at`, when present, is the retained access end. A
`premium_ended` event with `data.access_ended: true` records actual closed access
from the grant ledger, including expiration after an earlier cancellation.
Applications should not present a cancellation click as membership ending.

`payment_method_failed`, `payment_method_update_required`,
`payment_authentication_required` and `order_collection_stopped` include
subscription/order identity and the recorded retry decision:

- `scheduled` promises another attempt at `data.next_attempt_at`.
- `payment_method_required` waits for a replacement method.
- `stopped` schedules no further attempt.
- `unknown` makes no retry promise.

`data.recovery_action` is `update_payment_method`, `resubscribe`,
`authenticate_payment` or `contact_support`. Applications supply the destination.
These describe the decision when the fact was emitted; actions must re-read
current billing state because a later payment may already have resolved it.

Every real renewal and purchase records its fact. Ordinary `premium_renewed`
without an applied downgrade, `one_off_purchase_completed` and `usage_charged`
can be omitted from user notifications unless an application opts in. Applied
downgrades, access loss and payment failures deserve an actionable notice.
OpenRails does not create an upcoming-charge reminder schedule.

Price-change events describe the scheduled transition, not an already-applied
price. A provider may later block the schedule; consult the current subscription
before acting.

## Retention and migration

The public log expires facts after configured retention (90 days by default),
independently of readers. An expired cursor is an explicit 410 requiring
resynchronization. Its private typed producer journals retain dedupe evidence
from ingestion for at least 90 days and never less than event retention. Their
cleanup has no consumer/ACK dependencies.

Migration 73 preserved retained legacy customer notices without copying old
read/email state. The current migration removes source consumer registrations,
ACK records and their routes after replacing them with the retained log.
Merchant operator alerts and finding resolution remain independent domain
state. Billing archives contain financial evidence, not deployment cursors or
inboxes; restoring historical producer rows emits no new public activity.
