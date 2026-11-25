# Events

OpenRails records each committed billing fact in an ordered, retained event log per merchant: the integration surface a host reads for anything OpenRails does not do itself. Customer messages and the account inbox are sent from it ([customer messages](customer-messages.md)), and balance crossings write their facts to it. Integration guides: [events](../client.md#events) and [balance notices](../client.md#usage-and-running-charges).

## Concepts

- **Fact**: one immutable row of the event log, id `hev_…` (source `host`) or `ntf_…` (source `notification`).
- **Host event**: a typed financial transition (`billing.HostEventType`): `payment.settled`, `order.*`, `delinquency.*`, `balance.low`, `balance.depleted`, `product.entitlements_changed`, `mandate.revoked`, `destructive.hold_opened`, `destructive.hold_cleared`.
- **Notification fact**: a customer-facing fact (`models.NotificationEventType`), e.g. `payment_method_failed`, `premium_ended`, `usage_charged`, `balance_low`.
- **Cursor**: an opaque position in one merchant's stream, bound to its epoch, merchant and type filter.

## How it works

**Capture.** Producers write `host_outbox` (`EnqueueHostLifecycleEvent`, `EnqueueOrderHostEvent`, `EnqueueDestructiveHoldEvent`, unique on `(merchant_id, dedupe_key)`) or `notification_events` (`subscriptions.NotificationService`, or `CreateNotificationIfAbsent` with a deterministic id) in the domain transaction. Insert triggers (`capture_host_event_log`, `capture_notification_event_log`) call `billing.queue_event_log`, which refuses an event whose merchant differs from the pinned `openrails.merchant_id` and stages it in `eventlog_pending`. The deferred constraint trigger `eventlog_flush_at_commit` runs the books' commit-time sequence ([books](books.md#how-it-works)), whose last step, `billing.flush_event_log`, locks each touched `eventlog_streams` row, numbers the events into `eventlog_events`, and `pg_notify`s `<schema>.eventlog`. Positions are assigned at commit under the stream lock, so no reader sees a later fact before an earlier one commits. `billing.event_log_data` writes money fields as decimal strings and prefixes ids (`pay_`, `ord_`, `sub_`). Facts are immutable (`eventlog_immutable`, `notification_event_immutable`); a different fact under an existing id is refused. While a billing archive restore runs (`billing_restore_active`) nothing is captured.

**Reading.** `eventlog.List` reads the `event_log` and `event_log_streams` views in one read snapshot through the helpers module's `eventlog.Log`. OpenRails keeps no reader state. A cursor below the stream's floor is `410 event_cursor_expired`; one from another merchant or filter is `400 invalid_cursor`. `CleanupExpiredDataWorker` calls `eventlog.Prune` (bounded batches; `AdvanceEventLogFloor` records the floor, so expiry is told apart from an empty page), `PruneNotificationJournal` (past 90 days) and `DeleteHostOutboxBefore`.

**Balance notices.** `MoneyService.WatchBalanceInTx` looks at one balance under its lock and its `balance_watches` row. A crossing writes the host event (dedupe key by type, customer, currency and instant) and one customer fact (`notifyBalance`, id derived from the crossing); low and depleted in one look are one `balance_depleted`. `low_notified_at` and `depleted_at` hold a notice until a funding (`funded_at`) re-arms it. A change that crosses a line queues an `openrails.balance_look` job (`balancelook.Args`, unique per balance and instant) in the change's transaction; a look schedules the next; a failed look is taken by the `openrails.balance_watch` scan (`ListDueBalanceWatches`). Balance facts are not published to the inbox.

## Interfaces

- Go: `Client.ListEvents`, `Client.EventReader`, `Server.ListEvents`.
- HTTP: `GET /v1/app/events` (`merchant:events:read`).
- Configuration: `settings.balance_notices`; the event log keeps 90 days (`retention.EventLog`).
- Jobs: `openrails.balance_look`, `openrails.balance_watch`, `openrails.cleanup_expired_data`.

## Guarantees

### 1. Exactly-once provider calls
Does not apply: this system calls no payment provider.

### 2. Agreement with providers
Does not apply: facts are written by the systems that agree with providers; this one only records them.

### 3. Exact money
Amounts stay integers: `host_outbox.amount` is `bigint`, `billing.BalanceHostEvent` fields marshal `,string`, and `billing.event_log_data` turns money fields into decimal strings before a fact is stored. Every money host event carries its currency; nothing here converts or sums across currencies.

Checks:
- `billing.event_log_data` lists `amount`, `available_amount`, `owed_amount` and the other money keys it stringifies.
- `host_outbox_currency_check` requires a currency on every money event type.
- `BalanceHostEvent` money fields all carry the `,string` tag.

### 4. Access control and tenant isolation
`GET /v1/app/events` checks `needEvents`. `eventlog.List` refuses a merchant other than the context's, every per-merchant query names `merchant_id`, and a cursor binds its merchant. `billing.queue_event_log` refuses an event for another merchant than the pinned one.

Checks:
- `eventlog.List` returns an error when `merchant.FromContext` differs from its merchant.
- Every query in `event_log.sql` and `notification_events.sql` except the deployment-wide prune and discovery names `merchant_id`.

### 5. Contracts match the code
`GET /v1/app/events` is one catalog entry (`internal/http/routes/ops.go`) that generates its `Client` method, OpenAPI and TypeScript types; the in-process and remote `Client` both send it through `Client.send`. `billing.Event`, `EventPage` and `EventListParams` are in `api/go.txt`. A fact carries a `version` for payload changes.

Checks:
- `go run ./scripts/contracts` finds `client_gen.go`, `api/openapi.json`, `api/go.txt` and the route tables current.
- `TestClientProbe` sends each catalog route's `Client` method and checks its method, path and query.
- `Client.EventReader` maps `invalid_cursor` and `event_cursor_expired` the same way for both clients.

### 6. Data lifecycle
`internal/retention/tables.go` classes `eventlog_events`, `notification_events` and `host_outbox` as rows (event retention) and `eventlog_streams` and `balance_watches` as state; each table comment states its class. None is partitioned. A customer's erasure deletes their facts, and a merchant purge all of the merchant's.

Checks:
- `CleanupExpiredDataWorker.sweepDeployment` prunes the log and journal in bounded batches.
- `PruneNotificationJournal`'s cutoff is `retention.EventLog`, 90 days.
- `merchantarchive` excludes the event log and journal with reasons.

### 7. Work scales with activity
Facts are pushed at commit; readers poll their own cursor. Balance looks are queued by the change that crosses a line or scheduled for the instant a look names; the hourly scan reads only due watches. Prune finds expired merchants through `eventlog_expired_merchants`.

Checks:
- `ListDueBalanceWatches` is keyed on `next_check_at`, in batches of 200.

### 8. Abuse protection
No customer-facing entry point: the log is an application route.

### 9. Destructive-action safeguards
A destructive hold's host events commit with the hold ([findings and alerts](findings.md#9-destructive-action-safeguards)). Pruning is bounded per pass; otherwise facts go only with a merchant purge or a customer's erasure, and a billing restore captures nothing.

Checks:
- `capture_host_event_log` and `capture_notification_event_log` return before queueing while `billing_restore_active`.

## Tests

- OR-J09-S01, OR-J09-S04, OR-J13-S01, OR-J14-S05, OR-J21-S04 and OR-J22-S04 commit or roll back facts with their domain transaction; OR-J31-S01 restores into a new stream where source cursors answer `event_cursor_expired`.
- Balance notices: OR-J25-S04, OR-J25-S37, OR-J25-S40, OR-J25-S41, OR-J25-S42.
- Gaps: no test expires a cursor by retention (`eventlog.Prune`); `internal/eventlog` has no tests of its own.
