# Events and notifications

OpenRails records each committed billing fact in an ordered, retained event log
per merchant. Hosts read the log and own delivery to customers: receipts, email,
preferences. OpenRails itself delivers three things: selected customer notices
to an AuthKit account inbox (optional), merchant alerts by email and alert
webhook, and the findings queue staff work in the console. It sends customers no
email and no SMS. Integration guides: [events](../events.md) and
[balance notices](../running-charges.md#balance-notices).

## Concepts

- **Fact**: one immutable row of the event log, id `hev_…` (source `host`) or `ntf_…` (source `notification`).
- **Host event**: a typed financial transition (`billing.HostEventType`): `payment.settled`, `order.*`, `delinquency.*`, `balance.low`, `balance.depleted`, `product.entitlements_changed`, `mandate.revoked`, `destructive.hold_opened`, `destructive.hold_cleared`.
- **Notification fact**: a customer-facing fact (`models.NotificationEventType`), e.g. `payment_method_failed`, `premium_ended`, `usage_charged`, `balance_low`.
- **Cursor**: an opaque position in one merchant's stream, bound to its epoch, merchant and type filter.
- **Issue key**: the exact inbox identity of a notice: `subscription:<id>:<kind>`, `order:<id>:<kind>` or `event:<id>`, with a revision.
- **Finding**: a row of `reconciliation_findings`; `requires_review` ones are the queue a person works.
- **Alert**: one notification episode of a finding, or a destructive hold, sent to the alert email and alert webhooks.

## How it works

**Capture.** Producers write `host_outbox` (`EnqueueHostLifecycleEvent`,
`EnqueueOrderHostEvent`, `EnqueueDestructiveHoldEvent`, unique on
`(merchant_id, dedupe_key)`) or `notification_events`
(`subscriptions.NotificationService`, or `CreateNotificationIfAbsent` with a
deterministic id) in the domain transaction. Insert triggers call
`billing.queue_event_log`, which refuses an event whose merchant differs from the
pinned `openrails.merchant_id` and stages it in `eventlog_pending`. The deferred
constraint trigger `eventlog_flush_at_commit` runs `billing.eventlog_flush` at
commit: it locks each touched `eventlog_streams` row, numbers the events into
`eventlog_events`, and `pg_notify`s `<schema>.eventlog`. Positions are assigned at
commit under the stream lock, so no reader sees a later fact before an earlier one
commits. `billing.event_log_data` writes money fields as decimal strings and
prefixes ids (`pay_`, `ord_`, `sub_`). Facts are immutable (`eventlog_immutable`,
`notification_event_immutable`); a different fact under an existing id is refused.
While a billing archive restore runs (`billing_restore_active`) nothing is captured.

**Reading.** `eventlog.List` reads the `event_log` and `event_log_streams` views in
one read snapshot through the helpers module's `eventlog.Log`. OpenRails keeps no
reader state. A cursor below the stream's floor is `410 event_cursor_expired`; one
from another merchant or filter is `400 invalid_cursor`. `CleanupExpiredDataWorker`
calls `eventlog.Prune` (bounded batches; `AdvanceEventLogFloor` records the floor,
so expiry is told apart from an empty page), `PruneNotificationJournal` (past both
90 days and `event_retention`) and `DeleteHostOutboxBefore`.

**Account inbox.** With `Deps.Notifications` set, `publishInbox` runs inside
`NotificationService.create`'s transaction. `inboxKey` selects payment issues
(`notificationinbox.PaymentIssueKinds`) not caused by a checkout or change click,
`premium_ended` with `access_ended`, and `premium_renewed` with
`downgrade_applied`; every other fact stays in the log only. `issueRevision`
upserts `notification_issue_revisions` under `notificationinbox.RequireOrdering`
(a per-merchant shared advisory lock, refused while
`notification_channel_state.restore_required`), then `Sink.Publish` joins the same
transaction. `notificationinbox.Withdraw` bumps and tombstones only keys that were
published: on settlement (`WithdrawOrderPaymentNotices`, `WithdrawOrder`), an
accepted card (`WithdrawMethodNotice`), confirmed payment or reinstatement
(`WithdrawSubscriptionPaymentNotices`) and restored access. The channel's
`Recipient` maps the exact issuer and customer UUID; `adapters/authkit.Notifications`
skips other merchants and unlisted issuers. `LocalClient.ExportNotificationAdoption`
takes `LockOrdering` for its snapshot; `RestoreNotificationOrdering` re-arms the
channel after an archive restore.

**Merchant alerts.** A finding's engine calls `alerting.Service.NotifyFinding` after
persisting it (reconcile and converge engines, configuration evaluation, webhook
parks, pay URL failures). Findings raised through `reconcile.PGStore.RaiseFinding`
(`alerting.RecordLedgerRepair`, `alerting.RecordCrankerGas`) are not passed to it
and reach only the console. Only `requires_review` findings notify. In one
transaction `ClaimReconciliationFindingNotification` stamps `notified_at` and
`notified_severity` (first time, or a strict escalation) and an
`openrails.operator_alert` job (`DeliveryArgs`) is queued; a low finding stays in
the console. A destructive hold queues its alert in the hold's own transaction
(`projectDestructiveHold`). `OperatorAlertWorker` snapshots destinations
(`Service.Channels`: alert email for high, critical and holds; enabled alert
webhooks for medium and above and holds) and queues one job per channel.
`Service.Deliver` re-reads the finding and the destination and sends nothing for an
ended episode or a removed or disabled destination. Webhook bodies come from
`shapeWebhookBody`: generic `Alert` JSON, Slack `{text}`, Discord `{content}`.
`ConfigRealertWorker`, a scan of the hourly billing pass, re-arms an undeclared
account's finding a day after its last alert (`ClaimConfigFindingRealerts`,
`QueueRealerts`).

Alert webhooks live in the merchant document (`AlertWebhooks`), not PostgreSQL;
their writes mount only where Vault holds merchant configuration
(`FeatureMerchantConfigManages`). The API returns `destination_host`, never the URL.
The console's bell counts `open_findings` with `finding_status: requires_review`
through the metrics API every 30 seconds.

**Balance notices.** `MoneyService.WatchBalanceInTx` looks at one balance under its
lock and its `balance_watches` row. A crossing writes the host event (dedupe key
by type, customer, currency and instant) and one customer fact (`notifyBalance`,
id derived from the crossing); low and depleted in one look are one
`balance_depleted`. `low_notified_at` and `depleted_at` hold a notice until a
funding (`funded_at`) re-arms it. A change that crosses a line queues an
`openrails.balance_look` job (`balancelook.Args`, unique per balance and instant)
in its own transaction; a look schedules the next; a failed look is taken by the
`openrails.balance_watch` scan (`ListDueBalanceWatches`). Balance facts are not
published to the inbox.

**Email.** One sender per deployment: `Deps.Email`, else `Config.SMTP`
(`internal/email.SMTP`, any SMTP server; `NewSMTP` validates without connecting,
`CheckHealth` authenticates without sending). OpenRails' own email is merchant
alerts (`alerting.MailSender`, from the merchant profile's `from_email`); the
standalone server lends the sender to AuthKit for account email. Hosts send
receipts from facts such as `one_off_purchase_completed` and `usage_charged`.

## Interfaces

- Go: `Client.ListEvents`, `Client.EventReader`, `Server.ListEvents`;
  `Client.ListAlertWebhooks`, `CreateAlertWebhook`, `UpdateAlertWebhook`,
  `DeleteAlertWebhook`; `Client.ListFindings`, `GetFinding`, `ResolveFinding`;
  `LocalClient.ExportNotificationAdoption`, `RestoreNotificationOrdering`;
  `Deps.Notifications` (`openrails.NotificationChannel`), `Deps.Email`,
  `Config.SMTP`; `billing.MerchantProfile.NotificationURL`.
- HTTP: `GET /v1/app/events` (`merchant:events:read`);
  `GET|POST /v1/merchant/alert-webhooks`, `PATCH|DELETE /v1/merchant/alert-webhooks/{id}`
  (`merchant:config:read` / `merchant:config:manage`); `/v1/merchant/findings`.
- Configuration: `event_retention` (default `2160h`), the merchant's `alert_email`,
  `settings.balance_notices`.
- Jobs: `openrails.operator_alert`, `openrails.balance_look`, `openrails.balance_watch`,
  `openrails.config_realerts`, `openrails.cleanup_expired_data`.

## Guarantees

### 1. Exactly-once provider calls
Does not apply: this system calls no payment provider.

### 2. Agreement with providers
Does not apply: facts are written by the systems that agree with providers; this one only records and delivers them.

### 3. Exact money
Amounts stay integers: `host_outbox.amount` is `bigint`, `billing.BalanceHostEvent`
fields marshal `,string`, and `billing.event_log_data` turns money fields into
decimal strings before a fact is stored. Every money host event carries its
currency; nothing here converts or sums across currencies.

Checks:
- `billing.event_log_data` lists `amount`, `available_amount`, `owed_amount` and the other money keys it stringifies.
- `host_outbox_currency_check` requires a currency on every money event type.
- `BalanceHostEvent` money fields all carry the `,string` tag.

### 4. Access control and tenant isolation
`GET /v1/app/events` checks `needEvents`; alert webhook routes check the
merchant-config pair. `eventlog.List` refuses a merchant other than the context's,
every per-merchant query names `merchant_id`, and a cursor binds its merchant.
`billing.queue_event_log` refuses an event for another merchant than the pinned
one. Inbox recipients are mapped by exact issuer and UUID.

Checks:
- `eventlog.List` returns an error when `merchant.FromContext` differs from its merchant.
- Every query in `event_log.sql` and `notification_events.sql` except the deployment-wide prune and discovery names `merchant_id`.
- `adapters/authkit.Notifications` skips another merchant and an unlisted issuer.
- OR-J06-S05 refuses private and metadata alert webhook URLs.

### 5. Stable contracts
`ListEvents` is one catalog route (`internal/http/routes/ops.go`); the in-process and
remote `Client` both call it through `Client.do`. `billing.Event`, `EventPage` and
`EventQuery` are in `api/go.txt`; a fact carries a `version` for payload changes.

Checks:
- `Client.EventReader` maps `invalid_cursor` and `event_cursor_expired` the same way for both clients.
- `eventlog_migrations` records the shared event-log schema versions.
- `go run ./scripts/contracts` covers `EventPage` and the alert webhook types.

### 6. Migrations and retention
All tables are in `0001_schema.up.sql`. `internal/retention/tables.go` classes
`eventlog_events`, `notification_events` and `host_outbox` as rows (event
retention), `eventlog_streams`, `notification_issue_revisions` and
`notification_channel_state` as state. None is partitioned.

Checks:
- `CleanupExpiredDataWorker.sweepDeployment` prunes the log and journal in bounded batches.
- `PruneNotificationJournal`'s cutoff is the larger of `retention.EventLog` and `event_retention`.
- `merchantarchive` excludes the event log, journal and ordering state with reasons.

### 7. Work scales with activity
Facts are pushed at commit; readers poll their own cursor. Alerts are queued by the
claim that opens an episode. Balance looks are queued by the change that crosses a
line or scheduled for the instant a look names; the hourly scan reads only due
watches. Prune finds expired merchants through `eventlog_expired_merchants`.

Checks:
- `ListDueBalanceWatches` is keyed on `next_check_at`, in batches of 200.
- `ListConfigRealertMerchants` selects only merchants with an undeclared-account finding due.
- `DeliveryArgs.InsertOpts` deduplicates by args across states, completed included.

### 8. Abuse protection
No customer-facing entry point: the log is an application route and AuthKit serves
the inbox. Outbound alert webhooks are SSRF-guarded: `httpx.Policy` validates each
URL on write, before each delivery, and at the dialer on every redirect hop.

Checks:
- `CreateWebhook` and `UpdateWebhook` answer `webhook_invalid` for a non-public host.
- `deliverOnce` re-validates the URL before posting.

### 9. Destructive-action safeguards
This system tells operators about holds: the hold's alert and host event commit with
the hold, alert at critical, and go to every channel. Pruning is bounded per pass;
otherwise journal rows go only with a merchant purge or a customer's erasure.

Checks:
- `projectDestructiveHold` inserts the finding, the alert job and the host event in one transaction.
- `Service.Channels` selects email and webhooks for a hold regardless of severity.

## Tests

- Event log: OR-J09-S01, OR-J09-S04, OR-J13-S01, OR-J14-S05, OR-J21-S04 and
  OR-J22-S04 commit or roll back facts with their domain transaction; OR-J31-S01
  restores into a new stream where source cursors answer `event_cursor_expired`.
- Inbox: OR-J31-S40 (adoption export ordering). `TestAccountNotificationChannel`
  and `TestOrderNotificationRecovery` (`critical/account_notifications_test.go`)
  publish and withdraw against AuthKit's real store; they run with the suite but
  map to no scenario id.
- Alerts: OR-J06-S62 (daily re-alert), OR-J06-S64 (failed email retried, sent once),
  OR-J06-S05 (webhook URL refusal). Findings: OR-J37-S09 (a cranker low-gas
  finding raised and resolved), OR-J34-S04 (a hold's finding cannot be resolved
  around the hold).
- Balance notices: OR-J25-S04, OR-J25-S37, OR-J25-S40, OR-J25-S41, OR-J25-S42.
- Gaps: no test delivers an alert webhook (generic, Slack or Discord body,
  `Idempotency-Key` reuse), an escalation, or an alert for a `RaiseFinding`
  finding (none is sent); no test expires a cursor by retention
  (`eventlog.Prune`); `internal/modules/alerting`, `internal/eventlog` and
  `internal/email` have no tests of their own.
