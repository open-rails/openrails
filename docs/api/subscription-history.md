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
  by itself establish that money was collected.

An attempt's `action: "retry"` is its classification, not a scheduling promise.
Use the subscription's current dunning and recovery fields for its current
state. This endpoint never schedules or executes work.

Events are newest first by `at`, with deterministic source and ID tie breaks.
Pass the returned cursor unchanged for the same subscription. Payment event
time may precede the time OpenRails recorded it; refresh the first page to
discover late observations. Paging is not a frozen snapshot.

`coverage` states the sources and historical gaps. The current reader cannot
establish actor attribution, every card assignment or retry-date change, or
every pre-dispatch hold. The 761-day retention policy is not a guarantee that
every event in that period exists. Imported creation time is not original
signup time, and deleted or unrecorded facts are not reconstructed.

Raw provider messages, payloads, evidence blobs, credentials and card data are
excluded. Operation reasons are omitted because the existing reason field
contains free-form diagnostic text.
