# Subscription history

Merchant staff with subscription read permission can call
`GET /v1/admin/subscriptions/{id}/history?limit=50&cursor=…`, or use
`Client.GetSubscriptionHistory` in an embedded host. Customer credentials do
not grant access.

The response contains `events` (a paged `data` array and `next_cursor`) and
`coverage`. Each event has a stable source-qualified `id`, an `at` timestamp,
a `recorded_at` timestamp, and one typed detail:

- `subscription_transition`: the committed status or paid-period change,
  with its recorded decision and old/new values.
- `payment_attempt`: a provider answer and its existing classification,
  with references to the payment, order, renewal and operation where recorded.
- `provider_operation`: an execution phase. `unknown` is unresolved,
  `parked` is held, and `succeeded` means the operation succeeded; it does not
  by itself establish that money was collected. Structured `reason` codes
  describe newly observed holds (for example `write_posture` or
  `handler_unavailable`); free-form provider diagnostics are never returned.
- `subscription_billing_change`: `card_selection`, `default_card`,
  `card_version` or `retry_schedule`, with the recorded before/after values.
  A null selection follows the default. A default change is not proof that an
  external provider schedule has applied it. Card versions link the instrument
  observation without exposing card details.

New changes include `attribution` where known: `origin` and `actor` identify
who initiated the action, `executor` identifies the request/execution/verification
path, and `operation_id` links accepted asynchronous work. A later worker or
request replay retains the original initiator. Missing fields remain unknown.
Changes and their added history commit or roll back together. Repeated checks
of an unchanged hold record one observation; an execution attempt separates
hold episodes. This does not assert that a global operating mode changed.

An attempt's `action: "retry"` is its classification, not a scheduling promise.
Use the subscription's current dunning and recovery fields for its current
state. This endpoint never schedules or executes work.

Events are newest first by `at`, with deterministic source and ID tie breaks.
Pass the returned cursor unchanged for the same subscription. Payment event
time may precede the time OpenRails recorded it; refresh the first page to
discover late observations. Paging is not a frozen snapshot.

`coverage` states the sources and historical gaps. Richer recording begins
with migration 0108; older events are not backfilled. Direct writes without
admitted attribution retain unknown actors. Card/retry history and lifecycle
attribution are portable in merchant archives; operation logs remain excluded
from those archives under their existing policy. The 761-day retention policy is not a guarantee that
every event in that period exists. Imported creation time is not original
signup time, and deleted or unrecorded facts are not reconstructed.

Raw provider messages, payloads, evidence blobs, credentials and card data are
excluded. Only structured operation hold reasons are returned; the existing diagnostic
reason field remains excluded.
