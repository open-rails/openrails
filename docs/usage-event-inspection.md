# Inspecting individual usage events

Use `GetUsageEvent(id)` / `GET /v1/app/usage-events/{id}` to read a recorded
usage event. The stable `uev_…` ID comes from `RecordUsage`. A missing event,
an event owned by another merchant, or an event past retention returns
`resource_not_found`. The read does not record, reprice, or charge usage.

`ListUsageEvents` / `GET /v1/app/usage-events` returns individual records with
their source identity, dimensions, reported outcome, charged/forgiven amounts,
metadata, and any balance transaction ID. It complements aggregate usage
reports and ledger lists. The returned facts use the same `UsageEvent` shape
as recording; `replayed` is false for an inspection.

Filters are `customer_id`, `currency`, `event_type`, `source`, `source_id`, and
`outcome` (`succeeded` or `failed`). `source_id` requires `source`. Source
identity can recur for different customers, currencies or event types, so use
those filters too when reconciling one caller's event.

`from` is inclusive and `to` exclusive, both occurrence timestamps. A query
covers at most 31 days; the default covers the latest 31 days. Request earlier
history in successive windows within retained usage history (see
[retention](operations.md)). An ID lookup covers all retained partitions and
needs no time parameter. Rows outside retention are absent even before the
partition cleanup job runs.

Results are ordered by occurrence time and then ID, both descending. `limit`
is 1–500 (default 50); pass `next_cursor` as `cursor` with the same filters.
The cursor keeps its original time window so paging does not shift to a newer
window. New backdated events can appear between requests; this is a live list,
not an exported snapshot.

These routes require an application credential with `merchant:usage:read` or
`root:usage:read`. They grant no ability to record usage or charge a customer.
