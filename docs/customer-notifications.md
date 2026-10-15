# Customer notification events

OpenRails produces durable billing facts. ContentKit, OpenRails SaaS and other
applications own their user inbox, read/archive state, recipient mapping,
rendering, email and other delivery channels. The source has no customer-facing
notification API or email scheduler. AuthKit's existing account events can be
consumed separately when an application wants to present them; OpenRails does
not turn account operations into billing notifications.

## Independent consumers

A merchant application uses these application-only routes with its normal
merchant-bound credential and `merchant:events:read` permission:

| Operation | Route | Response |
|---|---|---|
| Register | `POST /v1/app/notification-consumers/{name}` | 204 |
| Pending | `GET /v1/app/notification-consumers/{name}/events?limit=100` | `{"data": [...]}` |
| Acknowledge | `POST /v1/app/notification-consumers/{name}/acknowledge` | 204 |
| Retire | `DELETE /v1/app/notification-consumers/{name}` | 204 |

Writes require `Idempotency-Key`. An acknowledgment body is
`{"notification_ids": ["ntf_..."]}` with 1–100 IDs. Pending batches contain at
most 100 events. There is no cursor: process the batch, then ask for pending
facts again. An older transaction that commits later remains pending and is
not skipped by an ID or timestamp watermark.

A consumer name is 1–64 ASCII letters, digits, dots, underscores or hyphens, excluding `.` and `..`.
Up to 32 names may be registered per application and scope. Registration is
idempotent and starts from the retained source history. Its identity is the
verified application issuer and subject, merchant, and name—not the particular
API key. Credentials rotated for that same application retain the subscription;
a distinct application cannot consume or retire it by choosing the same name.
Native AuthKit group API keys share the group's stable application identity.

Each event contains `id`, `merchant_id`, `customer_id`, `event_type`, JSON object
`data`, `created_at`, and optional `action_requested` and `optional` hints.
Payloads preserve unknown future object fields and event kinds. The shared
inbox consumers accept objects up to 64 KiB; applications should keep their
notification payloads small and keep larger artifacts at their source. The
immutable stored fact is its identity, kind, payload and occurrence time; classification hints
come from the current source version. `action_requested` describes what the
event originally asked, not whether an invoice is still unpaid today.
Actions must re-read authoritative billing state.

Stopping renewal and losing membership are separate facts. A
`subscription_canceled` event records stopped renewal while purchased access
remains. Its `data.ended_at`, when present, is the retained access end. A
`premium_ended` event with `data.access_ended: true` records actual closed access
from the grant ledger, including expiration after an earlier cancellation.
Consumers should not present a cancellation click as membership ending.

`payment_method_failed`, `payment_method_update_required`,
`payment_authentication_required`, and `order_collection_stopped` include
`data.subscription_id` or `data.order_id` for
correlation and a `data.retry_status`: `scheduled` promises another attempt at
`data.next_attempt_at`; `payment_method_required` waits for a replacement payment
method; `stopped` schedules no further attempt; `unknown` makes no retry promise.
`data.recovery_action` is `update_payment_method`, `resubscribe`,
`authenticate_payment`, or `contact_support`. Consumers provide the corresponding
destination and use `failure_code` and `failure_reason` for appropriate copy.
These are facts as observed, not a substitute for current billing state. Phrase
retry guidance as the decision when the notice was created and link to current
billing state, since a later payment may already have resolved the problem.
A renewal with `downgrade_applied` records an applied asynchronous plan change,
with previous/new product IDs and names and its effective time.

Persist the destination inbox item idempotently by source installation,
merchant and event ID, then acknowledge the source. Deliberately skipped events
should also be acknowledged. If persistence succeeds but acknowledgment fails,
the same fact is returned again; a destination must preserve its existing read
and archive state. Multiple readers of one consumer may receive duplicates.
Acknowledging one consumer never consumes another consumer's delivery.

Retiring a consumer explicitly releases its retention claim and removes its
acknowledgment history. Retirement does not start a new grace period: if the
remaining consumers' acknowledgments (or capture time with no consumers) already
satisfy retention, the event may be pruned immediately. Re-registering the same
name replays whatever source facts remain retained. Stop using a principal's consumers before retiring that
application identity.

## Embedded hosts and cross-merchant portals

`Client.NotificationEvents(name)` returns a merchant consumer. The returned
handle offers `Register(ctx)`, `Pending(ctx, limit)`, `Acknowledge(ctx, refs)` and
`Retire(ctx)`. A Go acknowledgment uses `billing.NotificationEventRef`, pairing
`MerchantID` and `ID`; identical IDs at different merchants remain distinct.

`Engine.NotificationEvents(name)` and `Server.NotificationEvents(name)` provide
a trusted **global** host consumer across merchants. This capability is only
available in process. There is no global remote endpoint. An embedded SaaS
consumer and a merchant's remote ContentKit consumer receive the same facts
independently and maintain their own inboxes.

SaaS can wrap its own inbox routes with `Server.NetworkAccountHandler` and read
`Server.NetworkLinks(ctx, account)` on each request. These reuse the network's
native self-acting, unbound account and accepted-terms checks and its current
active links. An app must filter every read and mutation through those current
links, so revocation immediately hides the merchant's local inbox copies.

A message about an event links to the merchant profile URL for its purpose:
`MerchantProfile.NotificationURL(event_type)` answers `subscribe_url` for
`premium_ended` and `billing_url` for everything else, falling back to
`support_url` ([merchant configuration](merchant-configuration.md)).

## Facts, preferences and retention

Every real renewal and successful purchase produces its fact; the source does
not throttle or suppress emission for delivery preferences. Routine
`premium_renewed` (without a scheduled downgrade), `one_off_purchase_completed`
and `usage_charged` facts have `optional: true`. Consumers should default to
skipping these unless their application or user opts in, while still
acknowledging them. Subscription activation, contractual plan changes,
required advance repricing, failure and repair facts are not optional, nor
are `balance_low` and `balance_depleted` ([balance notices](balance-notices.md)).
OpenRails does not invent an upcoming-charge reminder schedule.

Price-change events describe the recorded schedule transition, not an applied
price. A later provider push may block that schedule. Applications choose how
to present that history and consult current subscription state before acting.

A source event remains while any active matching merchant or global consumer
has not acknowledged it. After every active consumer has acknowledged, a
30-day grace period follows the latest of those acknowledgments (and capture).
With no consumers, capture starts that grace period. Registration and pruning
serialize so a newly registered consumer cannot lose its pending facts to GC.
A newly registered consumer cannot recover already-pruned history.

Migration 73 copies all retained legacy customer notices, including previously
read ones, without transferring their old read or email state. Already-deleted
history cannot be reconstructed. The old customer inbox/read endpoints,
unread count, renewal receipt interval, and customer email workers/templates
are removed. Legacy merchant operator alerts and actual finding resolution
remain separate domain state; reading an app inbox does not resolve a finding.
Machine `host_outbox` events and their existing acknowledgments are unchanged.

Consumer registrations and delivery records are deployment state, excluded
from financial archive restoration. Export refuses active pending deliveries,
including global consumers, so the host must drain or explicitly retire them.
Customer source facts and their acknowledgments are erased by customer deletion;
embedded SCIM contact erasure also removes retained source facts. Applications
must keep their own erased-recipient fence to skip subsequent billing facts
without resurrecting a removed user inbox.
