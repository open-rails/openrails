# Events and notifications

OpenRails records each committed billing fact in an ordered, retained event log
per merchant, and delivers four things from it: customer messages by email
(receipts, payment problems, cancellations, price changes), selected customer
notices to an AuthKit account inbox (optional), merchant alerts by email and alert
webhook, and the findings queue staff work in the console. Hosts read the log for
anything else, and send a message themselves by turning its event type off. It
sends no SMS. Integration guides: [events](../client.md#events),
[customer messages](../guide.md#customer-messages) and
[balance notices](../client.md#usage-and-running-charges).

## Concepts

- **Fact**: one immutable row of the event log, id `hev_…` (source `host`) or `ntf_…` (source `notification`).
- **Host event**: a typed financial transition (`billing.HostEventType`): `payment.settled`, `order.*`, `delinquency.*`, `balance.low`, `balance.depleted`, `product.entitlements_changed`, `mandate.revoked`, `destructive.hold_opened`, `destructive.hold_cleared`.
- **Notification fact**: a customer-facing fact (`models.NotificationEventType`), e.g. `payment_method_failed`, `premium_ended`, `usage_charged`, `balance_low`.
- **Cursor**: an opaque position in one merchant's stream, bound to its epoch, merchant and type filter.
- **Customer message**: a row of `customer_messages`: one fact's message on one channel (`email`) and what became of it (`billing.MessageStatus`). It keeps no body.
- **Messages document**: a merchant's `messages` document (`billing.MessageSettings`): which event types send, and templates that replace the built-in ones.
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

**Customer messages.** The `AFTER INSERT` trigger
`notification_event_customer_message` runs `billing.queue_customer_message`: in the
fact's own transaction it inserts one `pending` `customer_messages` row per
channel (`ON CONFLICT (merchant_id, event_id, channel) DO NOTHING`) and
`pg_notify`s `openrails_customer_messages` with the schema; it inserts nothing while
`billing_restore_active`. `messages.Service.Run` (started by
`Runtime.StartWorkers` on worker processes) listens and calls `Drain` on every wake
and reconnect; `openrails.customer_messages_dispatch` (`DispatchArgs`, every
`DispatchInterval`, five minutes) drains what no listener saw. `Drain` claims
pending rows oldest first through `customer_messages_pending_idx`
(`ClaimPendingCustomerMessages`, `FOR UPDATE SKIP LOCKED`, 100 per transaction),
marks them `queued` and inserts each one's `openrails.customer_message` job
(`SendArgs`, unique by args, completed included; `SendAttempts`, ten) in the same
transaction. `Service.Send` settles a message in order, each stop where it ends:
`Enabled` (the document's `enabled`, else the default: `defaults`, refined by
`byDefault`, which turns `premium_ended` on for `confirmedEndings` and off a
`payment_method_failed` whose `recovery_action` is `resubscribe` and a payment
issue from `checkoutSources`) or `disabled`;
`CustomerMessageOnTestPSP` (the fact's `psp_id`, or any PSP of its payment, order
or subscription, with `psps.live` false) or `test_environment`; no `Deps.Email`, `failed`/`no_sender`;
the directory's contact (`identity.Directory.Contacts`) or `no_contact`; an address
`net/mail` parses within 320 bytes or `failed`/`invalid_recipient`. It renders
(`Deps.MessageRenderer` when it takes the message, else `Render`), then `claim`
takes `LockCustomerMessages` (a per-customer transaction advisory lock), counts
`CountCustomerMessagesSince` and either marks it `sending` with the recipient or,
past `RateCap` (ten an hour), `rate_limited` with
`alerting.RecordMessageRateLimit`'s `life.customer.messages_rate_limited` finding.
`config.EmailSender.Send` follows; `SettleCustomerMessage` marks `sent`, back to
`queued` for a retry, or `failed`: `refused` on a 5xx to `RCPT TO` (`refusalOf`
reads helpers/smtp's step text), `send_failed` or `unavailable` on the last
attempt. A 5xx to `AUTH` or `MAIL FROM` retries and raises
`alerting.RecordMessageSenderRefused`'s `life.customer.messages_sender_refused`,
which `alerting.ResolveMessageSenderRefused` closes on the next send or test the
sender accepts.
`settle` runs on `context.WithoutCancel` with its own timeout, so a timed-out last
attempt still ends. From is the profile's `from_email` and display name, else the
sender's own.

**Templates.** Built-in templates (`templates/en/<type>.subject|.txt|.html`, one
`layout`) are a `billing.MessageSettings` the merchant's document overlays per
event type and language. Subject and text are `text/template`, HTML
`html/template`, over `billing.MessageData` (version `MessageDataVersion`), with
`missingkey=error` and `functions` (comparisons, logic, `len`, `default`,
`upper`, `lower`); amounts are `moneyutil.FormatAmount`. `compile`
parses each part alone and `vet` refuses `range`, `define`/`block`, variables, a
`template` call but the layout's one body, and every other function; a part's
source is at most `maxSource` (64 KiB) and renders through `capped` (256 KiB).
`typecheck` walks every branch's field chains against `billing.MessageData` by
reflection, refusing arguments to a field and comparisons of unlike kinds. A document is checked by `Check` (every event type against
its `Sample` in every language it names) where it loads: `checkMessages` in the
app's merchant configuration sync and `merchantbootstrap.ScopeDeclaration`; a
refused one is `Rejected` under `merchantdocs.MessagesDoc` and the version served
before keeps serving. `Service.Preview` renders over a sample fact with the
merchant's own profile and an optional candidate template; `Service.Test` sends
that rendering to the merchant's `alert_email` only, `TestCap` (ten) an hour per
merchant, counted in the abuse state (`Service.Abuse`). Endings record their cause
for the confirmation: `CancelMembershipTx` (`CancelMembershipParams.EndedReason`:
`admin` for staff, `refund` for a refund), `FailMembership` (`expired` or
`non_recoverable` when a decline ends the subscription) and `ExpireAwaitingMethod`
(`expired`); the converge `notifyPass` records `access_ended`. `refund_completed`
is written by `PaymentService.queueRefundReceipt` where a refund settles (one
the provider reported only within `RefundReceiptWindow`, seven days of its
refund time), with `voided` for
a reversal `VoidTransactionID` names. `premium_started` carries the first period
and, when charged, the amount (`membershipCharge`).

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
parks, pay URL failures), and `alerting.RecordLedgerRepair`,
`alerting.RecordCrankerGas`, `alerting.RecordMessageRateLimit` and
`alerting.RecordMessageSenderRefused` notify the findings they raise. Only `requires_review` findings notify. In one
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
in the change's transaction; a look schedules the next; a failed look is taken by the
`openrails.balance_watch` scan (`ListDueBalanceWatches`). Balance facts are not
published to the inbox.

**Email.** One sender per deployment: `Deps.Email` (`config.EmailSender`: `Send`,
`CheckHealth`); nil sends none. `smtpmail.EmailSender` (`adapters/smtpmail`) wraps a
`helpers/smtp` sender a host opened; the standalone server builds one from
`email_smtp` (`hostdeps.NewEmailSender`; it does not connect until it sends) and
lends it to AuthKit for account email (`controlplane.AuthKitSender`). Neither
sets a `Message-ID`: helpers/smtp mints a random one, so a send repeated after a
crash is not deduplicated by mail clients.
It sends customer messages and merchant alerts (`alerting.MailSender`), each from
the merchant profile's `from_email`.

## Interfaces

- Go: `Client.ListEvents`, `Client.EventReader`, `Server.ListEvents`;
  `Client.ListAlertWebhooks`, `CreateAlertWebhook`, `UpdateAlertWebhook`,
  `DeleteAlertWebhook`; `Client.ListFindings`, `GetFinding`, `ResolveFinding`;
  `LocalClient.ExportNotificationAdoption`, `RestoreNotificationOrdering`;
  `Client.ListCustomerMessages`, `PreviewMessage`, `CreateTestMessage`;
  `Deps.Notifications` (`openrails.NotificationChannel`), `Deps.Email`,
  `Deps.MessageRenderer`; `billing.MerchantProfile.NotificationURL`.
- HTTP: `GET /v1/app/events` (`merchant:events:read`);
  `GET|POST /v1/merchant/alert-webhooks`, `PATCH|DELETE /v1/merchant/alert-webhooks/{id}`
  (`merchant:config:read` / `merchant:config:manage`); `/v1/merchant/findings`;
  `GET /v1/admin/customers/{customer_id}/messages` (`merchant:customer:read`),
  `POST /v1/merchant/messages/preview` (`merchant:config:read`),
  `POST /v1/merchant/messages/test` (`merchant:config:manage`).
- Configuration: `event_retention` (default `2160h`), the merchant's `alert_email`,
  `settings.balance_notices`, the merchant's `messages` document, `email_smtp`.
- Jobs: `openrails.operator_alert`, `openrails.balance_look`, `openrails.balance_watch`,
  `openrails.config_realerts`, `openrails.customer_messages_dispatch`,
  `openrails.customer_message`, `openrails.cleanup_expired_data`.

## Guarantees

### 1. Exactly-once provider calls
Does not apply: this system calls no payment provider. An email is sent at least
once: the message is marked `sending` before `Send` and `sent` after it, so a
crash between the two can send it once more (no deterministic `Message-ID`: the
sender mints its own).

### 2. Agreement with providers
Does not apply: facts are written by the systems that agree with providers; this
one only records and delivers them. A message reads the fact's PSP only to send
nothing about money a test PSP moved.

Checks:
- `CustomerMessageOnTestPSP` reads `psps.live` of the fact's payment, order and subscription.

### 3. Exact money
Amounts stay integers: `host_outbox.amount` is `bigint`, `billing.BalanceHostEvent`
fields marshal `,string`, and `billing.event_log_data` turns money fields into
decimal strings before a fact is stored. Every money host event carries its
currency; nothing here converts or sums across currencies.

Messages format each amount once, at its currency's registered scale; the log
keeps no amount.

Checks:
- `billing.event_log_data` lists `amount`, `available_amount`, `owed_amount` and the other money keys it stringifies.
- `messageData` formats every amount with `moneyutil.FormatAmount` and its fact's currency; `TestReceipts` pins `12.50 USD` and `500 JPY`.
- `host_outbox_currency_check` requires a currency on every money event type.
- `BalanceHostEvent` money fields all carry the `,string` tag.

### 4. Access control and tenant isolation
`GET /v1/app/events` checks `needEvents`; alert webhook routes check the
merchant-config pair. `eventlog.List` refuses a merchant other than the context's,
every per-merchant query names `merchant_id`, and a cursor binds its merchant.
`billing.queue_event_log` refuses an event for another merchant than the pinned
one. Inbox recipients are mapped by exact issuer and UUID. The message log is a
support read (`merchant:customer:read`); a preview is a configuration read and a
test a configuration change. A send job binds its merchant (`merchant.WithID`)
before it reads.

Checks:
- `eventlog.List` returns an error when `merchant.FromContext` differs from its merchant.
- Every query in `event_log.sql` and `notification_events.sql` except the deployment-wide prune and discovery names `merchant_id`.
- `adapters/authkit.Notifications` skips another merchant and an unlisted issuer.
- OR-J06-S05 refuses private and metadata alert webhook URLs.
- Every query in `customer_messages.sql` but `ClaimPendingCustomerMessages` (marked `CROSS-MERCHANT`) names `merchant_id`.
- OR-J39-S16: another merchant's credential lists none of the customer's messages.

### 5. Contracts match the code
Every route here is one catalog entry (`ops.go`, `merchant.go`, `messages.go` in
`internal/http/routes`) that generates its `Client` method, OpenAPI and TypeScript
types; the in-process and remote `Client` both send it through `Client.send`. The
public types (`billing.Event`, `EventPage`, `EventQuery`, `CustomerMessage`,
`MessageData`, `MessageSettings`) are in `api/go.txt`, regenerated with each
change. A fact carries a `version` for payload changes, and templates read
`MessageData` of `MessageDataVersion`.

Checks:
- `go run ./scripts/contracts` finds `client_gen.go`, `api/openapi.json`, `api/go.txt` and the route tables current.
- `TestClientProbe` sends each catalog route's `Client` method and checks its method, path and query.
- `Client.EventReader` maps `invalid_cursor` and `event_cursor_expired` the same way for both clients; OR-J39-S16 reads the message log in process and remotely alike.

### 6. Data lifecycle
`internal/retention/tables.go` classes `eventlog_events`, `notification_events` and
`host_outbox` as rows (event retention), `customer_messages` as rows (deleted 731
days after `created_at`, `retention.CustomerMessages`: dispute evidence), and
`eventlog_streams`, `notification_issue_revisions` and `notification_channel_state`
as state; each table comment states its class. None is partitioned. A customer's
erasure deletes their facts and messages, and a merchant purge all of the
merchant's; a merchant archive carries no message.

Checks:
- `CleanupExpiredDataWorker.sweepDeployment` prunes the log and journal in bounded batches.
- `PruneNotificationJournal`'s cutoff is the larger of `retention.EventLog` and `event_retention`.
- `merchantarchive` excludes the event log, journal and ordering state with reasons, and `customer_messages` (`excludedTables`: a restore sends nothing again).
- `CleanupExpiredDataWorker` deletes messages past 731 days in bounded batches (`DeleteCustomerMessagesBefore`).
- SCIM `deleteUser` runs `EraseCustomerMessages`; a merchant purge runs `PurgeMerchantCustomerMessages`.

### 7. Work scales with activity
Facts are pushed at commit; readers poll their own cursor. A message is queued by
its fact's insert and dispatched on its commit's notify; the backstop reads only
pending rows. Alerts are queued by the claim that opens an episode. Balance looks
are queued by the change that crosses a line or scheduled for the instant a look
names; the hourly scan reads only due watches. Prune finds expired merchants
through `eventlog_expired_merchants`.

Checks:
- `ListDueBalanceWatches` is keyed on `next_check_at`, in batches of 200.
- `ListConfigRealertMerchants` selects only merchants with an undeclared-account finding due.
- `DeliveryArgs.InsertOpts` deduplicates by args across states, completed included; so does `SendArgs.InsertOpts`.
- `ClaimPendingCustomerMessages` reads `customer_messages_pending_idx` (`WHERE status = 'pending'`) in batches of 100.
- `CountCustomerMessagesSince` is one customer's messages, on `customer_messages_customer_idx`.

### 8. Abuse protection
No customer-facing entry point: the log is an application route and AuthKit serves
the inbox. Outbound alert webhooks are SSRF-guarded: `httpx.Policy` validates each
URL on write, before each delivery, and at the dialer on every redirect hop. A
customer is sent at most ten messages an hour. A test message goes only to the
merchant's `alert_email`: its request has no address, and a merchant sends ten an
hour. `alert_email` and `from_email` are one mailbox each, stored as its address. Templates escape every
value in HTML, call a fixed set of functions and render in time and size linear
in their source.

Checks:
- `CreateWebhook` and `UpdateWebhook` answer `webhook_invalid` for a non-public host.
- `deliverOnce` re-validates the URL before posting.
- `claim` counts under `LockCustomerMessages`, so concurrent jobs cannot pass `RateCap` (OR-J39-S05).
- `CreateTestMessageParams` names no recipient; OR-J39-S18 refuses a `to` field (`unknown_field`).
- `TestHTMLEscapesWhatItIsGiven` renders script and `javascript:` values inert.
- `Service.Test` counts `customer_message_test:<merchant>` past `TestCap` (OR-J39-S18).
- `TestTemplatesCannotGrowWithoutBound` and `TestARenderStopsAtItsCap` refuse loops, recursion and amplifying functions and cap a render.

### 9. Destructive-action safeguards
This system tells operators about holds: the hold's alert and host event commit with
the hold, alert at critical, and go to every channel. Pruning is bounded per pass;
otherwise journal rows go only with a merchant purge or a customer's erasure. A
billing restore sends customers nothing again, and a fact on a test PSP sends
nothing.

Checks:
- `projectDestructiveHold` inserts the finding, the alert job and the host event in one transaction.
- `Service.Channels` selects email and webhooks for a hold regardless of severity.
- `billing.queue_customer_message` returns before inserting while `billing_restore_active`: OR-J39-S02 writes a fact inside a restore's transaction and outside one.

## Tests

- Event log: OR-J09-S01, OR-J09-S04, OR-J13-S01, OR-J14-S05, OR-J21-S04 and
  OR-J22-S04 commit or roll back facts with their domain transaction; OR-J31-S01
  restores into a new stream where source cursors answer `event_cursor_expired`.
- Customer messages (`critical/customer_messages_test.go`): OR-J39-S01 (one receipt
  across a replayed fact and a refused send), S02 (a restore sends nothing), S03 to
  S05 (`no_contact`, `test_environment`, `rate_limited` with its finding), S06
  (erasure), S07 and S08 (a merchant template; a refused document), S09, S13, S14
  and S15 (each cancellation cause sends one confirmation; a refund's access end
  none), S10 (price change), S11 (refund receipts), S12 (`Deps.MessageRenderer`),
  S16 (the log route), S17 (preview), S18 (test send and its cap), S19 (SMTP
  refusals through helpers/smtp), S20 (a last attempt cut off mid-send), S21 (a
  declined checkout), S22 (a refund found late), S23 (a void), S24 (the
  subscription receipt), S25 (a staff refund completed late). `internal/modules/messages` has unit tests of
  rendering, defaults, `Check`, template bounds and type checks, and `refusalOf`.
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
  (`eventlog.Prune`); `internal/modules/alerting` and `internal/eventlog` have no
  tests of their own. No test refuses a message with an SMTP 5xx (`refused`),
  fails the host's renderer to `render_failed`, drops the dispatcher's connection,
  or ends a provider-mirrored subscription (told as `access_ended`).
